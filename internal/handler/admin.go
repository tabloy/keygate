package handler

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stripe/stripe-go/v82/subscription"

	"github.com/tabloy/keygate/internal/license"
	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/service"
	"github.com/tabloy/keygate/internal/store"
	"github.com/tabloy/keygate/pkg/apperr"
	"github.com/tabloy/keygate/pkg/response"
)

// PayPalCanceler can cancel PayPal subscriptions (implemented by PayPalHandler).
type PayPalCanceler interface {
	CancelSubscriptionByID(ctx context.Context, subscriptionID, reason string) error
}

type AdminHandler struct {
	Store        *store.Store
	Webhook      *service.WebhookService
	PayPalCancel PayPalCanceler // optional: set if PayPal is configured
}

func NewAdminHandler(s *store.Store, wh *service.WebhookService) *AdminHandler {
	return &AdminHandler{Store: s, Webhook: wh}
}

// ─── Stats ───

func (h *AdminHandler) Stats(c *gin.Context) {
	stats, err := h.Store.GetStats(c)
	if err != nil {
		response.Internal(c)
		return
	}
	response.OK(c, stats)
}

// ─── Products ───

func (h *AdminHandler) ListProducts(c *gin.Context) {
	products, err := h.Store.ListProducts(c, c.Query("search"))
	if err != nil {
		response.Internal(c)
		return
	}
	response.OK(c, gin.H{"products": products})
}

func (h *AdminHandler) GetProduct(c *gin.Context) {
	p, err := h.Store.FindProductByID(c, c.Param("id"))
	if err != nil {
		response.NotFound(c, "product not found")
		return
	}
	response.OK(c, p)
}

func (h *AdminHandler) CreateProduct(c *gin.Context) {
	var req struct {
		Name string `json:"name" binding:"required"`
		Slug string `json:"slug" binding:"required"`
		Type string `json:"type" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "name, slug, and type are required")
		return
	}
	if req.Type != "desktop" && req.Type != "saas" && req.Type != "hybrid" {
		response.BadRequest(c, "type must be desktop, saas, or hybrid")
		return
	}
	if err := apperr.ValidateName("name", req.Name); err != nil {
		response.BadRequest(c, err.Message)
		return
	}
	if err := apperr.ValidateSlug(req.Slug); err != nil {
		response.BadRequest(c, err.Message)
		return
	}

	p := &model.Product{Name: req.Name, Slug: req.Slug, Type: req.Type}
	if err := h.Store.CreateProduct(c, p); err != nil {
		response.Err(c, http.StatusConflict, "DUPLICATE", "product slug already exists")
		return
	}

	h.Store.Audit(c, &model.AuditLog{
		Entity: "product", EntityID: p.ID, Action: "created",
		ActorType: "admin", ActorID: adminID(c),
		Changes: map[string]any{"name": req.Name, "slug": req.Slug, "type": req.Type},
	})

	response.Created(c, p)
}

func (h *AdminHandler) UpdateProduct(c *gin.Context) {
	p, err := h.Store.FindProductByID(c, c.Param("id"))
	if err != nil {
		response.NotFound(c, "product not found")
		return
	}

	var req struct {
		Name string `json:"name"`
		Slug string `json:"slug"`
		Type string `json:"type"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "invalid request body")
		return
	}
	if req.Name != "" {
		if err := apperr.ValidateName("name", req.Name); err != nil {
			response.BadRequest(c, err.Message)
			return
		}
		p.Name = req.Name
	}
	if req.Slug != "" {
		if err := apperr.ValidateSlug(req.Slug); err != nil {
			response.BadRequest(c, err.Message)
			return
		}
		p.Slug = req.Slug
	}
	if req.Type != "" {
		if req.Type != "desktop" && req.Type != "saas" && req.Type != "hybrid" {
			response.BadRequest(c, "type must be desktop, saas, or hybrid")
			return
		}
		p.Type = req.Type
	}

	if err := h.Store.UpdateProduct(c, p); err != nil {
		response.Internal(c)
		return
	}
	response.OK(c, p)
}

func (h *AdminHandler) DeleteProduct(c *gin.Context) {
	id := c.Param("id")
	count, _ := h.Store.ProductLicenseCount(c, id)
	if count > 0 {
		response.Err(c, http.StatusConflict, "HAS_LICENSES", "cannot delete product with existing licenses")
		return
	}
	if err := h.Store.DeleteProduct(c, id); err != nil {
		response.Internal(c)
		return
	}
	h.Store.Audit(c, &model.AuditLog{
		Entity: "product", EntityID: id, Action: "deleted",
		ActorType: "admin", ActorID: adminID(c),
	})
	response.NoContent(c)
}

// ─── Plans ───

func (h *AdminHandler) ListPlans(c *gin.Context) {
	plans, err := h.Store.ListPlans(c, c.Query("product_id"), c.Query("search"))
	if err != nil {
		response.Internal(c)
		return
	}
	response.OK(c, gin.H{"plans": plans})
}

func (h *AdminHandler) GetPlan(c *gin.Context) {
	p, err := h.Store.FindPlanByID(c, c.Param("id"))
	if err != nil {
		response.NotFound(c, "plan not found")
		return
	}
	response.OK(c, p)
}

func (h *AdminHandler) CreatePlan(c *gin.Context) {
	var req struct {
		ProductID       string `json:"product_id" binding:"required"`
		Name            string `json:"name" binding:"required"`
		Slug            string `json:"slug" binding:"required"`
		LicenseType     string `json:"license_type" binding:"required"`
		BillingInterval string `json:"billing_interval"`
		MaxActivations  int    `json:"max_activations"`
		MaxSeats        int    `json:"max_seats"`
		TrialDays       int    `json:"trial_days"`
		GraceDays       int    `json:"grace_days"`
		StripePriceID   string `json:"stripe_price_id"`
		PayPalPlanID    string `json:"paypal_plan_id"`
		LicenseModel    string `json:"license_model"`
		FloatingTimeout int    `json:"floating_timeout"`
		SortOrder       int    `json:"sort_order"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "product_id, name, slug, and license_type are required")
		return
	}

	if err := apperr.ValidateName("name", req.Name); err != nil {
		response.BadRequest(c, err.Message)
		return
	}
	if err := apperr.ValidateSlug(req.Slug); err != nil {
		response.BadRequest(c, err.Message)
		return
	}

	switch req.LicenseType {
	case "subscription", "perpetual", "trial":
	default:
		response.BadRequest(c, "license_type must be subscription, perpetual, or trial")
		return
	}

	if req.MaxActivations > 10000 {
		response.BadRequest(c, "max_activations cannot exceed 10000")
		return
	}
	if req.TrialDays > 365 {
		response.BadRequest(c, "trial_days cannot exceed 365")
		return
	}

	if req.MaxActivations <= 0 {
		req.MaxActivations = 3
	}
	if req.GraceDays <= 0 {
		req.GraceDays = 7
	}

	licenseModel := req.LicenseModel
	if licenseModel == "" {
		licenseModel = "standard"
	}
	if licenseModel != "standard" && licenseModel != "floating" {
		response.BadRequest(c, "license_model must be standard or floating")
		return
	}
	floatingTimeout := req.FloatingTimeout
	if floatingTimeout <= 0 {
		floatingTimeout = 30
	}

	p := &model.Plan{
		ProductID:       req.ProductID,
		Name:            req.Name,
		Slug:            req.Slug,
		LicenseType:     req.LicenseType,
		BillingInterval: req.BillingInterval,
		MaxActivations:  req.MaxActivations,
		MaxSeats:        req.MaxSeats,
		TrialDays:       req.TrialDays,
		GraceDays:       req.GraceDays,
		StripePriceID:   req.StripePriceID,
		PayPalPlanID:    req.PayPalPlanID,
		LicenseModel:    licenseModel,
		FloatingTimeout: floatingTimeout,
		Active:          true,
		SortOrder:       req.SortOrder,
	}
	if err := h.Store.CreatePlan(c, p); err != nil {
		response.Err(c, http.StatusConflict, "DUPLICATE", "plan slug already exists for this product")
		return
	}

	h.Store.Audit(c, &model.AuditLog{
		Entity: "plan", EntityID: p.ID, Action: "created",
		ActorType: "admin", ActorID: adminID(c),
		Changes: map[string]any{"name": req.Name, "product_id": req.ProductID},
	})

	response.Created(c, p)
}

func (h *AdminHandler) UpdatePlan(c *gin.Context) {
	p, err := h.Store.FindPlanByID(c, c.Param("id"))
	if err != nil {
		response.NotFound(c, "plan not found")
		return
	}

	var req struct {
		Name            *string `json:"name"`
		Slug            *string `json:"slug"`
		LicenseType     *string `json:"license_type"`
		BillingInterval *string `json:"billing_interval"`
		MaxActivations  *int    `json:"max_activations"`
		TrialDays       *int    `json:"trial_days"`
		GraceDays       *int    `json:"grace_days"`
		StripePriceID   *string `json:"stripe_price_id"`
		PayPalPlanID    *string `json:"paypal_plan_id"`
		Active          *bool   `json:"active"`
		SortOrder       *int    `json:"sort_order"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "invalid request body")
		return
	}

	if req.Name != nil {
		p.Name = *req.Name
	}
	if req.Slug != nil {
		p.Slug = *req.Slug
	}
	if req.LicenseType != nil {
		p.LicenseType = *req.LicenseType
	}
	if req.BillingInterval != nil {
		p.BillingInterval = *req.BillingInterval
	}
	if req.MaxActivations != nil {
		p.MaxActivations = *req.MaxActivations
	}
	if req.TrialDays != nil {
		p.TrialDays = *req.TrialDays
	}
	if req.GraceDays != nil {
		p.GraceDays = *req.GraceDays
	}
	if req.StripePriceID != nil {
		p.StripePriceID = *req.StripePriceID
	}
	if req.PayPalPlanID != nil {
		p.PayPalPlanID = *req.PayPalPlanID
	}
	if req.Active != nil {
		p.Active = *req.Active
	}
	if req.SortOrder != nil {
		p.SortOrder = *req.SortOrder
	}

	if err := h.Store.UpdatePlan(c, p); err != nil {
		response.Internal(c)
		return
	}
	response.OK(c, p)
}

func (h *AdminHandler) DeletePlan(c *gin.Context) {
	id := c.Param("id")
	count, _ := h.Store.PlanLicenseCount(c, id)
	if count > 0 {
		response.Err(c, http.StatusConflict, "HAS_LICENSES", "cannot delete plan with existing licenses")
		return
	}
	if err := h.Store.DeletePlan(c, id); err != nil {
		response.Internal(c)
		return
	}
	response.NoContent(c)
}

// ─── Entitlements ───

func (h *AdminHandler) CreateEntitlement(c *gin.Context) {
	var req struct {
		PlanID      string `json:"plan_id" binding:"required"`
		Feature     string `json:"feature" binding:"required"`
		ValueType   string `json:"value_type" binding:"required"`
		Value       string `json:"value" binding:"required"`
		QuotaPeriod string `json:"quota_period"`
		QuotaUnit   string `json:"quota_unit"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "plan_id, feature, value_type, and value are required")
		return
	}

	switch req.ValueType {
	case "bool", "int", "string", "quota", "flag":
	default:
		response.BadRequest(c, "value_type must be bool, int, string, quota, or flag")
		return
	}

	e := &model.Entitlement{
		PlanID: req.PlanID, Feature: req.Feature,
		ValueType: req.ValueType, Value: req.Value,
		QuotaPeriod: req.QuotaPeriod, QuotaUnit: req.QuotaUnit,
	}
	if err := h.Store.CreateEntitlement(c, e); err != nil {
		response.Err(c, http.StatusConflict, "DUPLICATE", "entitlement already exists for this plan and feature")
		return
	}
	response.Created(c, e)
}

func (h *AdminHandler) UpdateEntitlement(c *gin.Context) {
	e, err := h.Store.FindEntitlementByID(c, c.Param("id"))
	if err != nil {
		response.NotFound(c, "entitlement not found")
		return
	}
	var req struct {
		Feature     *string `json:"feature"`
		ValueType   *string `json:"value_type"`
		Value       *string `json:"value"`
		QuotaPeriod *string `json:"quota_period"`
		QuotaUnit   *string `json:"quota_unit"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "invalid request body")
		return
	}
	if req.Feature != nil {
		e.Feature = *req.Feature
	}
	if req.ValueType != nil {
		e.ValueType = *req.ValueType
	}
	if req.Value != nil {
		e.Value = *req.Value
	}
	if req.QuotaPeriod != nil {
		e.QuotaPeriod = *req.QuotaPeriod
	}
	if req.QuotaUnit != nil {
		e.QuotaUnit = *req.QuotaUnit
	}

	if err := h.Store.UpdateEntitlement(c, e); err != nil {
		response.Internal(c)
		return
	}
	response.OK(c, e)
}

func (h *AdminHandler) DeleteEntitlement(c *gin.Context) {
	if err := h.Store.DeleteEntitlement(c, c.Param("id")); err != nil {
		response.Internal(c)
		return
	}
	response.NoContent(c)
}

// ─── API Keys ───

func (h *AdminHandler) ListAPIKeys(c *gin.Context) {
	keys, err := h.Store.ListAPIKeys(c, c.Query("product_id"), c.Query("search"))
	if err != nil {
		response.Internal(c)
		return
	}
	response.OK(c, gin.H{"api_keys": keys})
}

func (h *AdminHandler) CreateAPIKey(c *gin.Context) {
	var req struct {
		ProductID string   `json:"product_id" binding:"required"`
		Name      string   `json:"name" binding:"required"`
		Scopes    []string `json:"scopes"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "product_id and name are required")
		return
	}
	if err := apperr.ValidateName("name", req.Name); err != nil {
		response.BadRequest(c, err.Message)
		return
	}

	rawKey := store.GenerateRawAPIKey()
	prefix := rawKey[:12]
	if req.Scopes == nil {
		req.Scopes = []string{}
	}

	ak := &model.APIKey{
		ProductID: req.ProductID,
		Name:      req.Name,
		Prefix:    prefix,
		Scopes:    req.Scopes,
	}
	if err := h.Store.CreateAPIKey(c, ak, rawKey); err != nil {
		response.Internal(c)
		return
	}

	h.Store.Audit(c, &model.AuditLog{
		Entity: "api_key", EntityID: ak.ID, Action: "created",
		ActorType: "admin", ActorID: adminID(c),
		Changes: map[string]any{"name": req.Name, "product_id": req.ProductID},
	})

	response.Created(c, gin.H{
		"id":         ak.ID,
		"product_id": ak.ProductID,
		"name":       ak.Name,
		"key":        rawKey,
		"prefix":     prefix,
		"scopes":     ak.Scopes,
		"created_at": ak.CreatedAt,
	})
}

func (h *AdminHandler) DeleteAPIKey(c *gin.Context) {
	id := c.Param("id")
	if err := h.Store.DeleteAPIKey(c, id); err != nil {
		response.Internal(c)
		return
	}
	h.Store.Audit(c, &model.AuditLog{
		Entity: "api_key", EntityID: id, Action: "deleted",
		ActorType: "admin", ActorID: adminID(c),
	})
	response.NoContent(c)
}

// ─── Licenses ───

func (h *AdminHandler) ListLicenses(c *gin.Context) {
	licenses, total, err := h.Store.ListLicenses(c,
		c.Query("product_id"), c.Query("status"), c.Query("search"),
		queryInt(c, "offset", 0), queryInt(c, "limit", 50))
	if err != nil {
		response.Internal(c)
		return
	}
	response.OK(c, gin.H{"licenses": licenses, "total": total})
}

func (h *AdminHandler) GetLicense(c *gin.Context) {
	l, err := h.Store.FindLicenseByID(c, c.Param("id"))
	if err != nil {
		response.NotFound(c, "license not found")
		return
	}
	response.OK(c, l)
}

func (h *AdminHandler) CreateLicense(c *gin.Context) {
	var req struct {
		ProductID string `json:"product_id" binding:"required"`
		PlanID    string `json:"plan_id" binding:"required"`
		Email     string `json:"email" binding:"required"`
		Notes     string `json:"notes"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "product_id, plan_id, and email are required")
		return
	}

	if appErr := apperr.ValidateEmail(req.Email); appErr != nil {
		response.BadRequest(c, appErr.Message)
		return
	}

	// Look up plan first to determine license type and set appropriate fields
	plan, err := h.Store.FindPlanByID(c, req.PlanID)
	if err != nil {
		response.NotFound(c, "plan not found")
		return
	}

	status := model.StatusActive
	if plan.LicenseType == "trial" {
		status = model.StatusTrialing
	}

	l := &model.License{
		ProductID:  req.ProductID,
		PlanID:     req.PlanID,
		Email:      req.Email,
		LicenseKey: license.GenerateKey(""),
		Status:     status,
		Notes:      req.Notes,
	}

	// Set valid_until for trial licenses
	if plan.LicenseType == "trial" && plan.TrialDays > 0 {
		until := time.Now().Add(time.Duration(plan.TrialDays) * 24 * time.Hour)
		l.ValidUntil = &until
	}

	// Create license and subscription in a single transaction to prevent orphan records
	if err := h.Store.CreateLicenseWithSubscription(c, l, plan); err != nil {
		response.Internal(c)
		return
	}

	h.Store.Audit(c, &model.AuditLog{
		Entity: "license", EntityID: l.ID, Action: "created",
		ActorType: "admin", ActorID: adminID(c),
		Changes: map[string]any{"email": req.Email, "plan_id": req.PlanID},
	})

	if h.Webhook != nil {
		h.Webhook.Dispatch(c, l.ProductID, "license.created", map[string]any{
			"license_id": l.ID, "email": req.Email, "plan_id": req.PlanID,
		})
	}

	response.Created(c, l)
}

func (h *AdminHandler) RevokeLicense(c *gin.Context) {
	id := c.Param("id")
	if err := h.Store.RevokeLicense(c, id); err != nil {
		response.NotFound(c, err.Error())
		return
	}
	h.Store.Audit(c, &model.AuditLog{
		Entity: "license", EntityID: id, Action: "revoked",
		ActorType: "admin", ActorID: adminID(c),
	})
	if h.Webhook != nil {
		if lic, err := h.Store.FindLicenseByID(c, id); err == nil {
			h.Webhook.Dispatch(c, lic.ProductID, "license.revoked", map[string]any{
				"license_id": id, "email": lic.Email,
			})
		}
	}
	response.OK(c, gin.H{"status": "revoked"})
}

func (h *AdminHandler) RefundLicense(c *gin.Context) {
	id := c.Param("id")
	lic, err := h.Store.FindLicenseByID(c, id)
	if err != nil {
		response.NotFound(c, "license not found")
		return
	}

	if lic.StripeSubscriptionID == "" && lic.PayPalSubscriptionID == "" {
		response.BadRequest(c, "license has no payment subscription to refund")
		return
	}

	// Cancel subscription at payment provider
	providerResult := "no_active_subscription"
	if lic.StripeSubscriptionID != "" {
		if _, cancelErr := subscription.Cancel(lic.StripeSubscriptionID, nil); cancelErr != nil {
			providerResult = "stripe_cancel_failed"
			slog.Error("stripe subscription cancel failed", "subscription_id", lic.StripeSubscriptionID, "error", cancelErr)
		} else {
			providerResult = "stripe_subscription_canceled"
		}
	} else if lic.PayPalSubscriptionID != "" && h.PayPalCancel != nil {
		if cancelErr := h.PayPalCancel.CancelSubscriptionByID(c.Request.Context(), lic.PayPalSubscriptionID, "Admin refund"); cancelErr != nil {
			providerResult = "paypal_cancel_failed"
			slog.Error("paypal subscription cancel failed", "subscription_id", lic.PayPalSubscriptionID, "error", cancelErr)
		} else {
			providerResult = "paypal_subscription_canceled"
		}
	}

	// Mark the license as revoked
	lic.Status = model.StatusRevoked
	if err := h.Store.UpdateLicenseAndSubscription(c, lic, "status"); err != nil {
		response.Internal(c)
		return
	}

	h.Store.Audit(c, &model.AuditLog{
		Entity: "license", EntityID: id, Action: "refunded",
		ActorType: "admin", ActorID: adminID(c),
		Changes: map[string]any{"provider_result": providerResult},
	})

	if h.Webhook != nil {
		h.Webhook.Dispatch(c, lic.ProductID, "license.revoked", map[string]any{
			"license_id": id, "email": lic.Email, "reason": "refund",
		})
	}

	response.OK(c, gin.H{"status": "refunded", "provider_result": providerResult})
}

func (h *AdminHandler) SuspendLicense(c *gin.Context) {
	id := c.Param("id")
	if err := h.Store.SuspendLicense(c, id); err != nil {
		response.NotFound(c, err.Error())
		return
	}
	h.Store.Audit(c, &model.AuditLog{
		Entity: "license", EntityID: id, Action: "suspended",
		ActorType: "admin", ActorID: adminID(c),
	})
	if h.Webhook != nil {
		if lic, err := h.Store.FindLicenseByID(c, id); err == nil {
			h.Webhook.Dispatch(c, lic.ProductID, "license.suspended", map[string]any{
				"license_id": id, "email": lic.Email,
			})
		}
	}
	response.OK(c, gin.H{"status": "suspended"})
}

func (h *AdminHandler) ReinstateLicense(c *gin.Context) {
	id := c.Param("id")
	if err := h.Store.ReinstateLicense(c, id); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	h.Store.Audit(c, &model.AuditLog{
		Entity: "license", EntityID: id, Action: "reinstated",
		ActorType: "admin", ActorID: adminID(c),
	})
	if h.Webhook != nil {
		if lic, err := h.Store.FindLicenseByID(c, id); err == nil {
			h.Webhook.Dispatch(c, lic.ProductID, "license.reinstated", map[string]any{
				"license_id": id, "email": lic.Email,
			})
		}
	}
	response.OK(c, gin.H{"status": "active"})
}

func (h *AdminHandler) DeleteActivation(c *gin.Context) {
	id := c.Param("id")
	if err := h.Store.DeleteActivationByID(c, id); err != nil {
		response.Internal(c)
		return
	}
	response.NoContent(c)
}

// ─── Audit Logs ───

func (h *AdminHandler) ListAuditLogs(c *gin.Context) {
	logs, total, err := h.Store.ListAuditLogs(c,
		c.Query("entity"), c.Query("entity_id"),
		queryInt(c, "offset", 0), queryInt(c, "limit", 50))
	if err != nil {
		response.Internal(c)
		return
	}
	response.OK(c, gin.H{"audit_logs": logs, "total": total})
}

// ─── Users ───

func (h *AdminHandler) ListUsers(c *gin.Context) {
	users, total, err := h.Store.ListUsers(c, c.Query("search"), queryInt(c, "offset", 0), queryInt(c, "limit", 50))
	if err != nil {
		response.Internal(c)
		return
	}
	response.OK(c, gin.H{"users": users, "total": total})
}

// ─── Helpers ───

func adminID(c *gin.Context) string {
	v, _ := c.Get("user_id")
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func queryInt(c *gin.Context, key string, def int) int {
	if v := c.Query(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// ─── Usage (admin) ───

func (h *AdminHandler) ListLicenseUsage(c *gin.Context) {
	id := c.Param("id")
	events, total, err := h.Store.ListUsageEvents(c, id, c.Query("feature"),
		queryInt(c, "offset", 0), queryInt(c, "limit", 50))
	if err != nil {
		response.Internal(c)
		return
	}
	counters, _ := h.Store.GetUsageSummary(c, id)
	response.OK(c, gin.H{"events": events, "counters": counters, "total": total})
}

func (h *AdminHandler) ResetLicenseUsage(c *gin.Context) {
	id := c.Param("id")
	var req struct {
		Feature   string `json:"feature" binding:"required"`
		Period    string `json:"period"`
		PeriodKey string `json:"period_key"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "feature is required")
		return
	}
	period := req.Period
	if period == "" {
		period = "monthly"
	}
	periodKey := req.PeriodKey
	if periodKey == "" {
		periodKey = store.CurrentPeriodKey(period)
	}
	if err := h.Store.ResetUsageCounter(c, id, req.Feature, period, periodKey); err != nil {
		response.Internal(c)
		return
	}
	h.Store.Audit(c, &model.AuditLog{
		Entity: "license", EntityID: id, Action: "usage_reset",
		ActorType: "admin", ActorID: adminID(c),
		Changes: map[string]any{"feature": req.Feature, "period": period, "period_key": periodKey},
	})
	response.OK(c, gin.H{"status": "reset"})
}

// ─── Seats (admin) ───

func (h *AdminHandler) ListLicenseSeats(c *gin.Context) {
	id := c.Param("id")
	seats, err := h.Store.ListSeats(c, id)
	if err != nil {
		response.Internal(c)
		return
	}
	count, _ := h.Store.CountActiveSeats(c, id)
	response.OK(c, gin.H{"seats": seats, "active_count": count})
}

// ─── Analytics (admin) ───

func (h *AdminHandler) ListAnalytics(c *gin.Context) {
	productID := c.Query("product_id")
	granularity := c.Query("granularity")
	var from, to time.Time
	if v := c.Query("from"); v != "" {
		from, _ = time.Parse("2006-01-02", v)
	}
	if v := c.Query("to"); v != "" {
		to, _ = time.Parse("2006-01-02", v)
	}
	if from.IsZero() {
		from = time.Now().AddDate(0, -1, 0)
	}
	if to.IsZero() {
		to = time.Now()
	}

	if granularity == "weekly" || granularity == "monthly" {
		snapshots, err := h.Store.ListAnalyticsSnapshotsAggregated(c, productID, from, to, granularity)
		if err != nil {
			response.Internal(c)
			return
		}
		response.OK(c, gin.H{"snapshots": snapshots, "granularity": granularity})
		return
	}

	snapshots, err := h.Store.ListAnalyticsSnapshots(c, productID, from, to)
	if err != nil {
		response.Internal(c)
		return
	}
	response.OK(c, gin.H{"snapshots": snapshots})
}

func analyticsFilter(c *gin.Context) store.AnalyticsFilter {
	f := store.AnalyticsFilter{
		ProductID:   c.Query("product_id"),
		PlanID:      c.Query("plan_id"),
		LicenseType: c.Query("license_type"),
		Status:      c.Query("status"),
	}
	if v := c.Query("from"); v != "" {
		f.From, _ = time.Parse("2006-01-02", v)
	}
	if v := c.Query("to"); v != "" {
		t, err := time.Parse("2006-01-02", v)
		if err == nil {
			// End-of-day: include the entire "to" date
			f.To = t.Add(24*time.Hour - time.Nanosecond)
		}
	}
	return f
}

func (h *AdminHandler) AnalyticsSummary(c *gin.Context) {
	summary, err := h.Store.GetAnalyticsSummary(c, analyticsFilter(c))
	if err != nil {
		response.Internal(c)
		return
	}
	response.OK(c, summary)
}

func (h *AdminHandler) AnalyticsBreakdown(c *gin.Context) {
	dimension := c.Query("dimension")
	if dimension != "status" && dimension != "plan" && dimension != "license_type" {
		response.BadRequest(c, "dimension must be status, plan, or license_type")
		return
	}
	items, err := h.Store.GetLicenseBreakdown(c, analyticsFilter(c), dimension)
	if err != nil {
		response.Internal(c)
		return
	}
	response.OK(c, gin.H{"items": items})
}

func (h *AdminHandler) AnalyticsUsageTop(c *gin.Context) {
	productID := c.Query("product_id")
	var from, to time.Time
	if v := c.Query("from"); v != "" {
		from, _ = time.Parse("2006-01-02", v)
	}
	if v := c.Query("to"); v != "" {
		to, _ = time.Parse("2006-01-02", v)
	}
	limit := queryInt(c, "limit", 10)
	features, err := h.Store.GetTopFeatureUsage(c, productID, from, to, limit)
	if err != nil {
		response.Internal(c)
		return
	}
	response.OK(c, gin.H{"features": features})
}

func (h *AdminHandler) AnalyticsActivationTrend(c *gin.Context) {
	productID := c.Query("product_id")
	var from, to time.Time
	if v := c.Query("from"); v != "" {
		from, _ = time.Parse("2006-01-02", v)
	}
	if v := c.Query("to"); v != "" {
		to, _ = time.Parse("2006-01-02", v)
	}
	trend, err := h.Store.GetActivationTrend(c, productID, from, to)
	if err != nil {
		response.Internal(c)
		return
	}
	response.OK(c, gin.H{"trend": trend})
}

func (h *AdminHandler) AnalyticsInsights(c *gin.Context) {
	f := analyticsFilter(c)
	growth, err := h.Store.GetGrowthMetrics(c, f.ProductID)
	if err != nil {
		response.Internal(c)
		return
	}
	ageDist, _ := h.Store.GetLicenseAgeDistribution(c, f.ProductID)
	topUsers, _ := h.Store.GetTopUsers(c, f.ProductID, queryInt(c, "top_limit", 10))
	retention, _ := h.Store.GetRetentionData(c, f.ProductID, queryInt(c, "months", 6))
	recentActivity, _ := h.Store.GetRecentActivity(c, f.ProductID, queryInt(c, "activity_limit", 20))

	response.OK(c, gin.H{
		"growth":           growth,
		"age_distribution": ageDist,
		"top_users":        topUsers,
		"retention":        retention,
		"recent_activity":  recentActivity,
	})
}

func (h *AdminHandler) GetUserDetail(c *gin.Context) {
	id := c.Param("id")
	detail, err := h.Store.GetUserDetail(c, id)
	if err != nil {
		response.NotFound(c, "user not found")
		return
	}
	response.OK(c, detail)
}

// ─── Addons ───

func (h *AdminHandler) ListAddons(c *gin.Context) {
	addons, err := h.Store.ListAddons(c, c.Query("product_id"), c.Query("search"))
	if err != nil {
		response.Internal(c)
		return
	}
	response.OK(c, gin.H{"addons": addons})
}

func (h *AdminHandler) CreateAddon(c *gin.Context) {
	var req struct {
		ProductID   string `json:"product_id" binding:"required"`
		Name        string `json:"name" binding:"required"`
		Slug        string `json:"slug" binding:"required"`
		Description string `json:"description"`
		Feature     string `json:"feature" binding:"required"`
		ValueType   string `json:"value_type" binding:"required"`
		Value       string `json:"value" binding:"required"`
		QuotaPeriod string `json:"quota_period"`
		QuotaUnit   string `json:"quota_unit"`
		SortOrder   int    `json:"sort_order"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "product_id, name, slug, feature, value_type, and value are required")
		return
	}
	if err := apperr.ValidateName("name", req.Name); err != nil {
		response.BadRequest(c, err.Message)
		return
	}
	if err := apperr.ValidateSlug(req.Slug); err != nil {
		response.BadRequest(c, err.Message)
		return
	}
	switch req.ValueType {
	case "bool", "int", "string", "quota":
	default:
		response.BadRequest(c, "value_type must be bool, int, string, or quota")
		return
	}

	a := &model.Addon{
		ProductID: req.ProductID, Name: req.Name, Slug: req.Slug,
		Description: req.Description, Feature: req.Feature,
		ValueType: req.ValueType, Value: req.Value,
		QuotaPeriod: req.QuotaPeriod, QuotaUnit: req.QuotaUnit,
		Active: true, SortOrder: req.SortOrder,
	}
	if err := h.Store.CreateAddon(c, a); err != nil {
		response.Err(c, 409, "DUPLICATE", "addon slug already exists for this product")
		return
	}
	response.Created(c, a)
}

func (h *AdminHandler) UpdateAddon(c *gin.Context) {
	a, err := h.Store.FindAddonByID(c, c.Param("id"))
	if err != nil {
		response.NotFound(c, "addon not found")
		return
	}

	var req struct {
		Name        *string `json:"name"`
		Description *string `json:"description"`
		Feature     *string `json:"feature"`
		ValueType   *string `json:"value_type"`
		Value       *string `json:"value"`
		QuotaPeriod *string `json:"quota_period"`
		QuotaUnit   *string `json:"quota_unit"`
		Active      *bool   `json:"active"`
		SortOrder   *int    `json:"sort_order"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "invalid request")
		return
	}
	if req.Name != nil {
		a.Name = *req.Name
	}
	if req.Description != nil {
		a.Description = *req.Description
	}
	if req.Feature != nil {
		a.Feature = *req.Feature
	}
	if req.ValueType != nil {
		a.ValueType = *req.ValueType
	}
	if req.Value != nil {
		a.Value = *req.Value
	}
	if req.QuotaPeriod != nil {
		a.QuotaPeriod = *req.QuotaPeriod
	}
	if req.QuotaUnit != nil {
		a.QuotaUnit = *req.QuotaUnit
	}
	if req.Active != nil {
		a.Active = *req.Active
	}
	if req.SortOrder != nil {
		a.SortOrder = *req.SortOrder
	}
	if err := h.Store.UpdateAddon(c, a); err != nil {
		response.Internal(c)
		return
	}
	response.OK(c, a)
}

func (h *AdminHandler) DeleteAddon(c *gin.Context) {
	if err := h.Store.DeleteAddon(c, c.Param("id")); err != nil {
		response.Internal(c)
		return
	}
	response.NoContent(c)
}

func (h *AdminHandler) AddLicenseAddon(c *gin.Context) {
	var req struct {
		AddonID string `json:"addon_id" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "addon_id is required")
		return
	}
	la := &model.LicenseAddon{LicenseID: c.Param("id"), AddonID: req.AddonID, Enabled: true}
	if err := h.Store.AddLicenseAddon(c, la); err != nil {
		response.Internal(c)
		return
	}
	response.Created(c, la)
}

func (h *AdminHandler) RemoveLicenseAddon(c *gin.Context) {
	if err := h.Store.RemoveLicenseAddon(c, c.Param("id"), c.Param("addon_id")); err != nil {
		response.Internal(c)
		return
	}
	response.NoContent(c)
}

func (h *AdminHandler) ListLicenseAddons(c *gin.Context) {
	addons, err := h.Store.ListLicenseAddons(c, c.Param("id"))
	if err != nil {
		response.Internal(c)
		return
	}
	response.OK(c, gin.H{"addons": addons})
}

func (h *AdminHandler) ListFloatingSessions(c *gin.Context) {
	sessions, err := h.Store.ListFloatingSessions(c, c.Param("id"))
	if err != nil {
		response.Internal(c)
		return
	}
	active, _ := h.Store.CountActiveFloating(c, c.Param("id"))
	response.OK(c, gin.H{"sessions": sessions, "active": active})
}

// ─── Change Plan (admin) ───

func (h *AdminHandler) ChangeLicensePlan(c *gin.Context) {
	id := c.Param("id")
	var req struct {
		PlanID string `json:"plan_id" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "plan_id is required")
		return
	}

	l, err := h.Store.FindLicenseByID(c, id)
	if err != nil {
		response.NotFound(c, "license not found")
		return
	}

	plan, err := h.Store.FindPlanByID(c, req.PlanID)
	if err != nil {
		response.NotFound(c, "plan not found")
		return
	}
	if plan.ProductID != l.ProductID {
		response.BadRequest(c, "plan must belong to the same product")
		return
	}

	oldPlanID := l.PlanID
	l.PlanID = req.PlanID
	if err := h.Store.UpdateLicense(c, l, "plan_id"); err != nil {
		response.Internal(c)
		return
	}

	h.Store.Audit(c, &model.AuditLog{
		Entity: "license", EntityID: id, Action: "plan_changed",
		ActorType: "admin", ActorID: adminID(c),
		Changes: map[string]any{"old_plan_id": oldPlanID, "new_plan_id": req.PlanID},
	})

	if h.Webhook != nil {
		h.Webhook.Dispatch(c, l.ProductID, "plan.changed", map[string]any{
			"license_id": id, "old_plan_id": oldPlanID, "new_plan_id": req.PlanID,
		})
	}

	response.OK(c, gin.H{"status": "plan_changed", "plan_id": req.PlanID})
}

// ─── Settings ───

func (h *AdminHandler) GetSettings(c *gin.Context) {
	settings, err := h.Store.GetSettings(c)
	if err != nil {
		response.Internal(c)
		return
	}
	response.OK(c, gin.H{"settings": settings})
}

func (h *AdminHandler) UpdateSettings(c *gin.Context) {
	var req struct {
		Settings map[string]string `json:"settings" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "settings map is required")
		return
	}

	// Validate allowed keys
	allowed := map[string]bool{
		"site_name": true, "timezone": true, "language": true, "brand_color": true, "logo_url": true,
		"smtp_host": true, "smtp_port": true, "smtp_username": true,
		"smtp_password": true, "smtp_from": true,
		"rate_limit_api": true, "rate_limit_admin": true,
		"webhook_max_attempts": true, "webhook_timeout": true,
		"quota_warning_threshold":          true,
		"setup_complete":                   true,
		"email_template_license_created":   true,
		"email_template_license_expiring":  true,
		"email_template_license_expired":   true,
		"email_template_trial_expired":     true,
		"email_template_license_suspended": true,
		"email_template_quota_warning":     true,
		"email_template_seat_invite":       true,
		"email_template_payment_failed":    true,
	}
	for key := range req.Settings {
		if !allowed[key] {
			response.BadRequest(c, "unknown setting: "+key)
			return
		}
	}

	if err := h.Store.SetSettings(c, req.Settings); err != nil {
		response.Internal(c)
		return
	}

	keys := make([]string, 0, len(req.Settings))
	for k := range req.Settings {
		keys = append(keys, k)
	}
	h.Store.Audit(c, &model.AuditLog{
		Entity: "settings", EntityID: "system", Action: "updated",
		ActorType: "admin", ActorID: adminID(c),
		Changes: map[string]any{"keys": keys},
	})

	response.OK(c, gin.H{"status": "saved"})
}

func (h *AdminHandler) SendTestEmail(c *gin.Context) {
	// Just return success for now - the actual email sending would use the SMTP config
	response.OK(c, gin.H{"status": "sent"})
}

// GetEmailTemplates returns all email templates (custom from DB + hardcoded defaults).
func (h *AdminHandler) GetEmailTemplates(c *gin.Context) {
	defaults := service.DefaultTemplates()

	type templateInfo struct {
		Custom  string `json:"custom"`
		Default string `json:"default"`
	}

	result := make(map[string]templateInfo, len(defaults))
	for key, def := range defaults {
		custom, _ := h.Store.GetSetting(c, "email_template_"+key)
		result[key] = templateInfo{Custom: custom, Default: def}
	}

	response.OK(c, gin.H{"templates": result})
}

// ─── Team (Admin Management) ───

// ListTeamMembers returns all platform admins (owner + admin roles).
func (h *AdminHandler) ListTeamMembers(c *gin.Context) {
	admins, err := h.Store.ListAdmins(c)
	if err != nil {
		response.Internal(c)
		return
	}
	response.OK(c, gin.H{"members": admins})
}

// InviteTeamMember promotes an existing user to admin, or creates a placeholder admin user.
// Only owners can invite new admins.
func (h *AdminHandler) InviteTeamMember(c *gin.Context) {
	// Only owner can manage team
	actorEmail, ok := c.Get("email")
	if !ok {
		response.Unauthorized(c, "unauthorized")
		return
	}
	actorUser, err := h.Store.FindUserByEmail(c, actorEmail.(string))
	if err != nil || actorUser.Role != model.RoleOwner {
		response.Forbidden(c, "only the owner can manage team members")
		return
	}

	var req struct {
		Email string `json:"email" binding:"required"`
		Role  string `json:"role"` // "admin" (default) or "owner"
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "email is required")
		return
	}

	// Validate email format
	if appErr := apperr.ValidateEmail(req.Email); appErr != nil {
		response.BadRequest(c, appErr.Message)
		return
	}

	role := req.Role
	if role == "" {
		role = model.RoleAdmin
	}
	if role != model.RoleAdmin && role != model.RoleOwner {
		response.BadRequest(c, "role must be 'admin' or 'owner'")
		return
	}

	// Cannot invite yourself
	if req.Email == actorUser.Email {
		response.BadRequest(c, "cannot change your own role via invite")
		return
	}

	// Find or create user
	user, err := h.Store.FindUserByEmail(c, req.Email)
	if err != nil {
		// User doesn't exist yet — create placeholder (will get proper name on first OAuth login)
		if err := h.Store.CreatePlaceholderUser(c, req.Email, role); err != nil {
			response.Internal(c)
			return
		}
		user, _ = h.Store.FindUserByEmail(c, req.Email)
	} else {
		// User exists — check if already same role (idempotent)
		if user.Role == role {
			response.OK(c, user)
			return
		}
		if err := h.Store.SetUserRole(c, user.ID, role); err != nil {
			response.Internal(c)
			return
		}
		user.Role = role
	}

	h.Store.Audit(c, &model.AuditLog{
		Entity: "team", EntityID: user.ID, Action: "member_invited",
		ActorType: "admin", ActorID: adminID(c),
		Changes: map[string]any{"email": req.Email, "role": role},
	})

	response.OK(c, user)
}

// RemoveTeamMember demotes an admin back to regular user.
// Only owners can remove admins. Cannot remove the last owner.
func (h *AdminHandler) RemoveTeamMember(c *gin.Context) {
	actorEmail, ok := c.Get("email")
	if !ok {
		response.Unauthorized(c, "unauthorized")
		return
	}
	actorUser, err := h.Store.FindUserByEmail(c, actorEmail.(string))
	if err != nil || actorUser.Role != model.RoleOwner {
		response.Forbidden(c, "only the owner can manage team members")
		return
	}

	targetID := c.Param("id")

	// Can't remove yourself
	if targetID == actorUser.ID {
		response.BadRequest(c, "cannot remove yourself from the team")
		return
	}

	target, err := h.Store.FindUserByID(c, targetID)
	if err != nil {
		response.NotFound(c, "user not found")
		return
	}

	if !target.IsAdmin() {
		response.BadRequest(c, "user is not a team member")
		return
	}

	// Prevent removing the last owner
	if target.Role == model.RoleOwner {
		ownerCount, _ := h.Store.CountOwners(c)
		if ownerCount <= 1 {
			response.BadRequest(c, "cannot remove the last owner")
			return
		}
	}

	if err := h.Store.SetUserRole(c, targetID, model.RoleUser); err != nil {
		response.Internal(c)
		return
	}

	h.Store.Audit(c, &model.AuditLog{
		Entity: "team", EntityID: targetID, Action: "member_removed",
		ActorType: "admin", ActorID: adminID(c),
		Changes: map[string]any{"email": target.Email, "previous_role": target.Role},
	})

	response.OK(c, gin.H{"status": "removed"})
}

// ExportLicenses exports all licenses as CSV or JSON.
// GET /api/v1/admin/licenses/export?format=csv&product_id=xxx&status=xxx
func (h *AdminHandler) ExportLicenses(c *gin.Context) {
	format := c.DefaultQuery("format", "csv")
	if format != "csv" && format != "json" {
		response.BadRequest(c, "format must be csv or json")
		return
	}

	licenses, err := h.Store.ExportLicenses(c, c.Query("product_id"), c.Query("status"))
	if err != nil {
		response.Internal(c)
		return
	}

	dateStr := time.Now().Format("2006-01-02")

	if format == "json" {
		filename := fmt.Sprintf("licenses-%s.json", dateStr)
		c.Header("Content-Type", "application/json")
		c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%s", filename))

		type exportLicense struct {
			ID         string `json:"id"`
			Email      string `json:"email"`
			Product    string `json:"product"`
			Plan       string `json:"plan"`
			Status     string `json:"status"`
			LicenseKey string `json:"license_key"`
			ValidFrom  string `json:"valid_from"`
			ValidUntil string `json:"valid_until"`
			CreatedAt  string `json:"created_at"`
		}

		out := make([]exportLicense, 0, len(licenses))
		for _, l := range licenses {
			productName := ""
			if l.Product != nil {
				productName = l.Product.Name
			}
			planName := ""
			if l.Plan != nil {
				planName = l.Plan.Name
			}
			validUntil := ""
			if l.ValidUntil != nil {
				validUntil = l.ValidUntil.Format(time.RFC3339)
			}
			out = append(out, exportLicense{
				ID:         l.ID,
				Email:      l.Email,
				Product:    productName,
				Plan:       planName,
				Status:     l.Status,
				LicenseKey: l.LicenseKey,
				ValidFrom:  l.ValidFrom.Format(time.RFC3339),
				ValidUntil: validUntil,
				CreatedAt:  l.CreatedAt.Format(time.RFC3339),
			})
		}

		enc := json.NewEncoder(c.Writer)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
			slog.Error("export json encode", "error", err)
		}
		return
	}

	// CSV format
	filename := fmt.Sprintf("licenses-%s.csv", dateStr)
	c.Header("Content-Type", "text/csv")
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%s", filename))

	w := csv.NewWriter(c.Writer)
	defer w.Flush()

	_ = w.Write([]string{"id", "email", "product", "plan", "status", "license_key", "valid_from", "valid_until", "created_at"})

	for _, l := range licenses {
		productName := ""
		if l.Product != nil {
			productName = l.Product.Name
		}
		planName := ""
		if l.Plan != nil {
			planName = l.Plan.Name
		}
		validUntil := ""
		if l.ValidUntil != nil {
			validUntil = l.ValidUntil.Format(time.RFC3339)
		}
		_ = w.Write([]string{
			l.ID,
			l.Email,
			productName,
			planName,
			l.Status,
			l.LicenseKey,
			l.ValidFrom.Format(time.RFC3339),
			validUntil,
			l.CreatedAt.Format(time.RFC3339),
		})
	}
}
