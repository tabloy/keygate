package handler

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
	"github.com/tabloy/keygate/pkg/apperr"
	"github.com/tabloy/keygate/pkg/response"
)

// SetupHandler manages the first-run setup wizard.
type SetupHandler struct {
	Store *store.Store
}

func NewSetupHandler(s *store.Store) *SetupHandler {
	return &SetupHandler{Store: s}
}

// setupNeeded returns true if no owner exists and setup_complete is not "true".
func (h *SetupHandler) setupNeeded(c *gin.Context) (bool, string) {
	complete, _ := h.Store.GetSetting(c, "setup_complete")
	if complete == "true" {
		return false, "complete"
	}

	ownerCount, err := h.Store.CountOwners(c)
	if err != nil {
		// If we can't count owners, assume setup is needed
		return true, "initialize"
	}
	if ownerCount > 0 {
		return false, "complete"
	}

	return true, "initialize"
}

// Status returns whether setup is needed and which step the wizard is on.
// GET /api/v1/setup/status
func (h *SetupHandler) Status(c *gin.Context) {
	needed, step := h.setupNeeded(c)
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
	needed, _ := h.setupNeeded(c)
	if !needed {
		response.Err(c, http.StatusConflict, "SETUP_COMPLETE", "setup already complete")
		return
	}

	// Acquire advisory lock — check the boolean result, not just SQL error
	var locked bool
	if err := h.Store.DB.NewRaw("SELECT pg_try_advisory_lock(8675309)").Scan(c, &locked); err != nil {
		response.Internal(c)
		return
	}
	if !locked {
		response.Err(c, http.StatusConflict, "SETUP_IN_PROGRESS", "another setup is in progress")
		return
	}
	defer h.Store.DB.NewRaw("SELECT pg_advisory_unlock(8675309)").Exec(c)

	// Re-check after acquiring lock
	needed, _ = h.setupNeeded(c)
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

	// All mutations in a single transaction — if any step fails, everything rolls back
	ctx := c.Request.Context()
	tx, err := h.Store.DB.BeginTx(ctx, nil)
	if err != nil {
		response.Internal(c)
		return
	}
	defer tx.Rollback()

	// Create owner user
	userID := store.NewID()
	if _, err := tx.NewRaw(
		"INSERT INTO users (id, email, name, role, created_at, updated_at) VALUES (?, ?, ?, 'owner', now(), now()) ON CONFLICT (email) DO UPDATE SET role = 'owner', name = EXCLUDED.name, updated_at = now()",
		userID, req.AdminEmail, req.AdminName,
	).Exec(ctx); err != nil {
		response.Internal(c)
		return
	}
	// Get actual user ID (may differ if email existed)
	var actualUserID string
	if err := tx.NewRaw("SELECT id FROM users WHERE email = ?", req.AdminEmail).Scan(ctx, &actualUserID); err != nil {
		response.Internal(c)
		return
	}

	// Set site_name
	if _, err := tx.NewRaw(
		"INSERT INTO settings (key, value) VALUES ('site_name', ?) ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value",
		req.SiteName,
	).Exec(ctx); err != nil {
		response.Internal(c)
		return
	}

	// Create product
	productID := store.NewID()
	if _, err := tx.NewRaw(
		"INSERT INTO products (id, name, slug, type, created_at) VALUES (?, ?, ?, ?, now())",
		productID, req.ProductName, req.ProductSlug, req.ProductType,
	).Exec(ctx); err != nil {
		response.Internal(c)
		return
	}

	// Create default plan
	planID := store.NewID()
	if _, err := tx.NewRaw(
		"INSERT INTO plans (id, product_id, name, slug, license_type, max_activations, grace_days, active, created_at) VALUES (?, ?, 'Pro', 'pro', 'subscription', 5, 7, true, now())",
		planID, productID,
	).Exec(ctx); err != nil {
		response.Internal(c)
		return
	}

	// Generate API key
	rawKey := store.GenerateRawAPIKey()
	apiKeyID := store.NewID()
	keyHash := store.HashAPIKey(rawKey)
	if _, err := tx.NewRaw(
		"INSERT INTO api_keys (id, product_id, name, key_hash, prefix, scopes, created_at) VALUES (?, ?, 'Default', ?, ?, '{}', now())",
		apiKeyID, productID, keyHash, rawKey[:12],
	).Exec(ctx); err != nil {
		response.Internal(c)
		return
	}

	// Mark setup complete
	if _, err := tx.NewRaw(
		"INSERT INTO settings (key, value) VALUES ('setup_complete', 'true') ON CONFLICT (key) DO UPDATE SET value = 'true'",
	).Exec(ctx); err != nil {
		response.Internal(c)
		return
	}

	if err := tx.Commit(); err != nil {
		response.Internal(c)
		return
	}

	// Build response objects
	user := &model.User{ID: actualUserID, Email: req.AdminEmail, Name: req.AdminName, Role: model.RoleOwner}
	product := &model.Product{ID: productID, Name: req.ProductName, Slug: req.ProductSlug, Type: req.ProductType}
	plan := &model.Plan{ID: planID, ProductID: productID, Name: "Pro", Slug: "pro", LicenseType: "subscription"}
	apiKey := &model.APIKey{ID: apiKeyID, ProductID: productID, Name: "Default", Prefix: rawKey[:12]}

	// 8. Return created resources
	response.Created(c, gin.H{
		"user":    user,
		"product": product,
		"plan":    plan,
		"api_key": gin.H{
			"id":         apiKey.ID,
			"product_id": apiKey.ProductID,
			"name":       apiKey.Name,
			"prefix":     apiKey.Prefix,
			"scopes":     apiKey.Scopes,
			"key":        rawKey,
		},
	})
}
