package handler

import (
	"net/http"
	"slices"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/tabloy/keygate/internal/store"
	"github.com/tabloy/keygate/pkg/apperr"
	"github.com/tabloy/keygate/pkg/response"
)

// SetupHandler manages the first-run setup wizard.
type SetupHandler struct {
	Store *store.Store
	// AdminEmails is ADMIN_EMAILS. When set, only those addresses can
	// become owner through setup, so a stranger who reaches a fresh
	// install first cannot claim it.
	AdminEmails []string
}

func NewSetupHandler(s *store.Store, adminEmails []string) *SetupHandler {
	return &SetupHandler{Store: s, AdminEmails: adminEmails}
}

// Status returns whether setup is needed and which step the wizard is on.
// GET /api/v1/setup/status
func (h *SetupHandler) Status(c *gin.Context) {
	needed, err := h.Store.SetupNeeded(c.Request.Context(), h.Store.DB)
	if err != nil {
		response.Internal(c, err)
		return
	}
	step := "complete"
	if needed {
		step = "initialize"
	}
	response.OK(c, gin.H{
		"needed": needed,
		"step":   step,
	})
}

// Initialize completes setup in one call. This is the ONLY endpoint that allows
// creating the first owner without authentication.
// POST /api/v1/setup/initialize
func (h *SetupHandler) Initialize(c *gin.Context) {
	// 1. Verify setup not already complete
	needed, err := h.Store.SetupNeeded(c.Request.Context(), h.Store.DB)
	if err != nil {
		response.Internal(c, err)
		return
	}
	if !needed {
		response.Err(c, http.StatusConflict, "SETUP_COMPLETE", "setup already complete")
		return
	}

	var req struct {
		AdminEmail  string `json:"admin_email" binding:"required,email"`
		AdminName   string `json:"admin_name" binding:"required"`
		SiteName    string `json:"site_name" binding:"required"`
		ProductName string `json:"product_name" binding:"required"`
		ProductSlug string `json:"product_slug" binding:"required"`
		ProductType string `json:"product_type" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "invalid request: "+err.Error())
		return
	}

	req.AdminEmail = strings.TrimSpace(strings.ToLower(req.AdminEmail))
	req.AdminName = strings.TrimSpace(req.AdminName)
	req.SiteName = strings.TrimSpace(req.SiteName)
	req.ProductName = strings.TrimSpace(req.ProductName)
	req.ProductSlug = strings.TrimSpace(strings.ToLower(req.ProductSlug))
	req.ProductType = strings.TrimSpace(strings.ToLower(req.ProductType))

	if req.ProductType != "saas" && req.ProductType != "desktop" && req.ProductType != "hybrid" {
		response.BadRequest(c, "product_type must be one of: saas, desktop, hybrid")
		return
	}
	if err := apperr.ValidateName("product_name", req.ProductName); err != nil {
		response.BadRequest(c, err.Message)
		return
	}
	if err := apperr.ValidateSlug(req.ProductSlug); err != nil {
		response.BadRequest(c, err.Message)
		return
	}
	// After every field check, so sending an invalid field cannot tell a
	// listed address from an unlisted one. Both sides are lower-cased, so
	// compare exactly: strings.EqualFold would let a Unicode fold such as
	// "ſ" for "s" pass for a listed address, and create an owner no one
	// can sign in as.
	if len(h.AdminEmails) > 0 && !slices.Contains(h.AdminEmails, req.AdminEmail) {
		response.Err(c, http.StatusForbidden, "SETUP_EMAIL_NOT_ALLOWED", "this server only accepts an owner email listed in ADMIN_EMAILS")
		return
	}

	// All mutations in a single transaction — if any step fails, everything rolls back
	ctx := c.Request.Context()
	tx, err := h.Store.DB.BeginTx(ctx, nil)
	if err != nil {
		response.Internal(c, err)
		return
	}
	defer tx.Rollback()

	// Serialize setups and check again under the lock. A transaction lock
	// holds one connection for the whole setup; a session lock taken on
	// the pool could land on another connection and let two setups through.
	var locked bool
	if err := tx.NewRaw("SELECT pg_try_advisory_xact_lock(8675309)").Scan(ctx, &locked); err != nil {
		response.Internal(c, err)
		return
	}
	if !locked {
		response.Err(c, http.StatusConflict, "SETUP_IN_PROGRESS", "another setup is in progress")
		return
	}
	if needed, err := h.Store.SetupNeeded(ctx, tx); err != nil {
		response.Internal(c, err)
		return
	} else if !needed {
		response.Err(c, http.StatusConflict, "SETUP_COMPLETE", "setup already complete")
		return
	}

	// Create owner user
	userID := store.NewID()
	if _, err := tx.NewRaw(
		"INSERT INTO users (id, email, name, role, created_at, updated_at) VALUES (?, ?, ?, 'owner', now(), now()) ON CONFLICT (email) DO UPDATE SET role = 'owner', name = EXCLUDED.name, updated_at = now()",
		userID, req.AdminEmail, req.AdminName,
	).Exec(ctx); err != nil {
		response.Internal(c, err)
		return
	}
	// Get actual user ID (may differ if email existed)
	var actualUserID string
	if err := tx.NewRaw("SELECT id FROM users WHERE email = ?", req.AdminEmail).Scan(ctx, &actualUserID); err != nil {
		response.Internal(c, err)
		return
	}

	// Set site_name
	if _, err := tx.NewRaw(
		"INSERT INTO settings (key, value) VALUES ('site_name', ?) ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value",
		req.SiteName,
	).Exec(ctx); err != nil {
		response.Internal(c, err)
		return
	}

	// Create product
	productID := store.NewID()
	if _, err := tx.NewRaw(
		"INSERT INTO products (id, name, slug, type, created_at) VALUES (?, ?, ?, ?, now())",
		productID, req.ProductName, req.ProductSlug, req.ProductType,
	).Exec(ctx); err != nil {
		response.Internal(c, err)
		return
	}

	// Create default plan — pick activation/seat defaults that match
	// the product type's capability surface. The product_type drives
	// what fields are even valid: a SaaS product whose plan carries
	// max_activations=5 would silently violate the capability gate
	// at runtime (every /license/activate call would 404).
	planID := store.NewID()
	var maxActivations, maxSeats int
	switch req.ProductType {
	case "saas":
		maxActivations, maxSeats = 0, 10
	case "desktop":
		maxActivations, maxSeats = 3, 0
	case "hybrid":
		maxActivations, maxSeats = 3, 10
	}
	if _, err := tx.NewRaw(
		"INSERT INTO plans (id, product_id, name, slug, license_type, max_activations, max_seats, grace_days, active, checkout_id, created_at) VALUES (?, ?, 'Pro', 'pro', 'subscription', ?, ?, 7, true, ?, now())",
		planID, productID, maxActivations, maxSeats, store.ShortID(),
	).Exec(ctx); err != nil {
		response.Internal(c, err)
		return
	}

	// We deliberately don't auto-create an API key during setup.
	// Under RequireScope, a fresh key with scopes={} can't reach any
	// admin route — shipping one in the response would mislead users
	// into thinking they have a usable credential. They mint one
	// later from /admin/api-keys with the scope that matches the
	// intended use (admin / licenses:write / releases:write).

	// Mark setup complete
	if _, err := tx.NewRaw(
		"INSERT INTO settings (key, value) VALUES ('setup_complete', 'true') ON CONFLICT (key) DO UPDATE SET value = 'true'",
	).Exec(ctx); err != nil {
		response.Internal(c, err)
		return
	}

	if err := tx.Commit(); err != nil {
		response.Internal(c, err)
		return
	}

	// Answer with the rows as stored. Objects built here by hand carried
	// zero values for every column the inserts left to the database
	// (require_signing, active, created_at, the plan's limits and
	// checkout id), so the response contradicted what was saved.
	user, err := h.Store.FindUserByID(c, actualUserID)
	if err != nil {
		response.Internal(c, err)
		return
	}
	product, err := h.Store.FindProductByID(c, productID)
	if err != nil {
		response.Internal(c, err)
		return
	}
	plan, err := h.Store.FindPlanByID(c, planID)
	if err != nil {
		response.Internal(c, err)
		return
	}

	response.Created(c, gin.H{
		"user":    user,
		"product": product,
		"plan":    plan,
	})
}
