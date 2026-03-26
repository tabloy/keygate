package payment

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/tabloy/keygate/internal/license"
	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/service"
	"github.com/tabloy/keygate/internal/store"
	"github.com/tabloy/keygate/pkg/response"
)

var paypalClient = &http.Client{Timeout: 15 * time.Second}

type PayPalHandler struct {
	Store        *store.Store
	ClientID     string
	ClientSecret string
	WebhookID    string
	Sandbox      bool
	BaseURL      string
	Email        *service.EmailService
	WebhookSvc   *service.WebhookService
}

func (h *PayPalHandler) apiBase() string {
	if h.Sandbox {
		return "https://api-m.sandbox.paypal.com"
	}
	return "https://api-m.paypal.com"
}

func (h *PayPalHandler) accessToken(ctx context.Context) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, "POST", h.apiBase()+"/v1/oauth2/token",
		bytes.NewBufferString("grant_type=client_credentials"))
	req.SetBasicAuth(h.ClientID, h.ClientSecret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := paypalClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("paypal auth: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	if result.Error != "" {
		return "", fmt.Errorf("paypal auth: %s", result.Error)
	}
	return result.AccessToken, nil
}

// Webhook handles PayPal webhook notifications.
func (h *PayPalHandler) Webhook(c *gin.Context) {
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		response.BadRequest(c, "read failed")
		return
	}

	if !h.verifyWebhook(c, c.Request.Header, body) {
		response.BadRequest(c, "invalid signature")
		return
	}

	var event struct {
		ID        string          `json:"id"`
		EventType string          `json:"event_type"`
		Resource  json.RawMessage `json:"resource"`
	}
	if json.Unmarshal(body, &event) != nil {
		response.BadRequest(c, "invalid payload")
		return
	}

	// Idempotency: atomically check+record to prevent race conditions
	if event.ID != "" && !h.Store.TryRecordProcessedEvent(c, "paypal", event.ID) {
		c.JSON(http.StatusOK, gin.H{"received": true, "skipped": true})
		return
	}

	ctx := c.Request.Context()
	slog.Info("paypal webhook received", "type", event.EventType, "id", event.ID)

	switch event.EventType {
	case "BILLING.SUBSCRIPTION.ACTIVATED":
		h.onSubscriptionActivated(ctx, event.Resource)
	case "BILLING.SUBSCRIPTION.CANCELLED":
		h.onSubscriptionCancelled(ctx, event.Resource)
	case "BILLING.SUBSCRIPTION.SUSPENDED":
		h.onSubscriptionSuspended(ctx, event.Resource)
	case "PAYMENT.SALE.COMPLETED":
		h.onPaymentCompleted(ctx, event.Resource)
	case "PAYMENT.SALE.REFUNDED":
		h.onPaymentRefunded(ctx, event.Resource)
	case "BILLING.SUBSCRIPTION.UPDATED":
		h.onSubscriptionUpdated(ctx, event.Resource)
	case "BILLING.SUBSCRIPTION.EXPIRED":
		h.onSubscriptionExpired(ctx, event.Resource)
	case "BILLING.SUBSCRIPTION.RE-ACTIVATED":
		h.onSubscriptionReactivated(ctx, event.Resource)
	case "BILLING.SUBSCRIPTION.PAYMENT.FAILED":
		h.onPaymentFailed(ctx, event.Resource)
	case "CUSTOMER.DISPUTE.CREATED":
		h.onDisputeCreated(ctx, event.Resource)
	default:
		slog.Warn("paypal webhook: unhandled event type", "type", event.EventType)
	}

	c.JSON(http.StatusOK, gin.H{"received": true})
}

func (h *PayPalHandler) onSubscriptionActivated(ctx context.Context, raw json.RawMessage) {
	var sub struct {
		ID     string `json:"id"`
		PlanID string `json:"plan_id"`
		Payer  struct {
			Email string `json:"email_address"`
		} `json:"subscriber"`
	}
	if json.Unmarshal(raw, &sub) != nil || sub.ID == "" {
		return
	}

	plan, err := h.Store.FindPlanByPayPalPlanID(ctx, sub.PlanID)
	if err != nil {
		return
	}

	lic := &model.License{
		ProductID:            plan.ProductID,
		PlanID:               plan.ID,
		Email:                sub.Payer.Email,
		LicenseKey:           license.GenerateKey(""),
		PaymentProvider:      "paypal",
		PayPalSubscriptionID: sub.ID,
		Status:               model.StatusActive,
	}
	_ = h.Store.CreateLicense(ctx, lic)

	// Send license key email via reliable queue
	if sub.Payer.Email != "" {
		product, _ := h.Store.FindProductByID(ctx, plan.ProductID)
		productName := plan.Name
		if product != nil {
			productName = product.Name
		}
		body := fmt.Sprintf(`<!DOCTYPE html>
<html><body style="font-family: -apple-system, sans-serif; max-width: 600px; margin: 0 auto; padding: 20px;">
<h2 style="color: #111;">Your %s License</h2>
<p>Your <strong>%s</strong> license is ready.</p>
<div style="background: #f4f4f5; border-radius: 8px; padding: 16px; margin: 16px 0; font-family: monospace; font-size: 18px; text-align: center; letter-spacing: 2px;">%s</div>
<p style="color: #666; font-size: 14px;">Keep this key safe. You'll need it to activate your software.</p>
</body></html>`, productName, plan.Name, lic.LicenseKey)
		_ = h.Store.EnqueueEmail(ctx, sub.Payer.Email, "Your license for "+productName, body)
	}

	h.Store.Audit(ctx, &model.AuditLog{
		Entity: "license", EntityID: lic.ID, Action: "created",
		ActorType: "webhook",
		Changes:   map[string]any{"provider": "paypal", "email": sub.Payer.Email, "plan": plan.Name},
	})

	if h.WebhookSvc != nil {
		h.WebhookSvc.Dispatch(ctx, lic.ProductID, "license.created", map[string]any{
			"license_id": lic.ID, "email": lic.Email, "plan_id": lic.PlanID,
		})
	}
}

func (h *PayPalHandler) onSubscriptionCancelled(ctx context.Context, raw json.RawMessage) {
	var sub struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(raw, &sub) != nil {
		return
	}

	lic, err := h.Store.FindLicenseByPayPalSubscription(ctx, sub.ID)
	if err != nil {
		return
	}
	lic.Status = model.StatusCanceled
	now := time.Now()
	lic.CanceledAt = &now
	_ = h.Store.UpdateLicenseAndSubscription(ctx, lic, "status", "canceled_at")

	h.Store.Audit(ctx, &model.AuditLog{
		Entity: "license", EntityID: lic.ID, Action: "canceled",
		ActorType: "webhook", Changes: map[string]any{"provider": "paypal"},
	})

	if h.WebhookSvc != nil {
		h.WebhookSvc.Dispatch(ctx, lic.ProductID, "license.canceled", map[string]any{
			"license_id": lic.ID, "email": lic.Email, "reason": "subscription_cancelled",
		})
	}
}

func (h *PayPalHandler) onSubscriptionSuspended(ctx context.Context, raw json.RawMessage) {
	var sub struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(raw, &sub) != nil {
		return
	}

	lic, err := h.Store.FindLicenseByPayPalSubscription(ctx, sub.ID)
	if err != nil {
		return
	}
	lic.Status = model.StatusPastDue
	_ = h.Store.UpdateLicenseAndSubscription(ctx, lic, "status")
}

func (h *PayPalHandler) onPaymentCompleted(ctx context.Context, raw json.RawMessage) {
	var sale struct {
		BillingAgreementID string `json:"billing_agreement_id"`
	}
	if json.Unmarshal(raw, &sale) != nil || sale.BillingAgreementID == "" {
		return
	}

	lic, err := h.Store.FindLicenseByPayPalSubscription(ctx, sale.BillingAgreementID)
	if err != nil {
		return
	}
	lic.Status = model.StatusActive

	// Extend valid_until based on plan billing interval
	if lic.Plan != nil {
		now := time.Now()
		switch lic.Plan.BillingInterval {
		case "month":
			until := now.AddDate(0, 1, 0)
			lic.ValidUntil = &until
		case "year":
			until := now.AddDate(1, 0, 0)
			lic.ValidUntil = &until
		}
	}

	_ = h.Store.UpdateLicenseAndSubscription(ctx, lic, "status", "valid_until")
}

func (h *PayPalHandler) onPaymentRefunded(ctx context.Context, raw json.RawMessage) {
	var data struct {
		BillingAgreementID string `json:"billing_agreement_id"`
		Amount             struct {
			Total string `json:"total"`
		} `json:"amount"`
	}
	if json.Unmarshal(raw, &data) != nil || data.BillingAgreementID == "" {
		return
	}
	lic, err := h.Store.FindLicenseByPayPalSubscription(ctx, data.BillingAgreementID)
	if err != nil {
		return
	}
	lic.Status = model.StatusRevoked
	_ = h.Store.UpdateLicenseAndSubscription(ctx, lic, "status")

	h.Store.Audit(ctx, &model.AuditLog{
		Entity: "license", EntityID: lic.ID, Action: "revoked",
		ActorType: "webhook",
		Changes:   map[string]any{"reason": "refund", "provider": "paypal"},
	})
}

func (h *PayPalHandler) onSubscriptionUpdated(ctx context.Context, raw json.RawMessage) {
	var sub struct {
		ID     string `json:"id"`
		PlanID string `json:"plan_id"`
		Status string `json:"status"`
	}
	if json.Unmarshal(raw, &sub) != nil || sub.ID == "" {
		return
	}

	lic, err := h.Store.FindLicenseByPayPalSubscription(ctx, sub.ID)
	if err != nil {
		return
	}

	// Map PayPal status to our status
	switch sub.Status {
	case "ACTIVE":
		lic.Status = model.StatusActive
	case "SUSPENDED":
		lic.Status = model.StatusPastDue
	case "CANCELLED":
		lic.Status = model.StatusCanceled
		now := time.Now()
		lic.CanceledAt = &now
	case "EXPIRED":
		lic.Status = model.StatusExpired
	}
	_ = h.Store.UpdateLicenseAndSubscription(ctx, lic, "status", "canceled_at")

	// If plan changed
	if sub.PlanID != "" {
		if newPlan, err := h.Store.FindPlanByPayPalPlanID(ctx, sub.PlanID); err == nil && newPlan.ID != lic.PlanID {
			oldPlanID := lic.PlanID
			lic.PlanID = newPlan.ID
			_ = h.Store.UpdateLicense(ctx, lic, "plan_id")
			h.Store.Audit(ctx, &model.AuditLog{
				Entity: "license", EntityID: lic.ID, Action: "plan_changed",
				ActorType: "webhook",
				Changes:   map[string]any{"old_plan_id": oldPlanID, "new_plan_id": newPlan.ID, "provider": "paypal"},
			})
		}
	}
}

func (h *PayPalHandler) onSubscriptionExpired(ctx context.Context, raw json.RawMessage) {
	var sub struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(raw, &sub) != nil || sub.ID == "" {
		return
	}

	lic, err := h.Store.FindLicenseByPayPalSubscription(ctx, sub.ID)
	if err != nil {
		return
	}
	lic.Status = model.StatusExpired
	_ = h.Store.UpdateLicenseAndSubscription(ctx, lic, "status")

	h.Store.Audit(ctx, &model.AuditLog{
		Entity: "license", EntityID: lic.ID, Action: "expired",
		ActorType: "webhook", Changes: map[string]any{"provider": "paypal"},
	})

	if h.WebhookSvc != nil {
		h.WebhookSvc.Dispatch(ctx, lic.ProductID, "license.expired", map[string]any{
			"license_id": lic.ID, "email": lic.Email,
		})
	}
}

func (h *PayPalHandler) onSubscriptionReactivated(ctx context.Context, raw json.RawMessage) {
	var sub struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(raw, &sub) != nil || sub.ID == "" {
		return
	}

	lic, err := h.Store.FindLicenseByPayPalSubscription(ctx, sub.ID)
	if err != nil {
		return
	}
	lic.Status = model.StatusActive
	lic.SuspendedAt = nil
	lic.CanceledAt = nil
	_ = h.Store.UpdateLicenseAndSubscription(ctx, lic, "status", "suspended_at", "canceled_at")

	h.Store.Audit(ctx, &model.AuditLog{
		Entity: "license", EntityID: lic.ID, Action: "reinstated",
		ActorType: "webhook", Changes: map[string]any{"provider": "paypal"},
	})

	if h.WebhookSvc != nil {
		h.WebhookSvc.Dispatch(ctx, lic.ProductID, "license.reinstated", map[string]any{
			"license_id": lic.ID, "email": lic.Email,
		})
	}
}

func (h *PayPalHandler) onPaymentFailed(ctx context.Context, raw json.RawMessage) {
	var sub struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(raw, &sub) != nil || sub.ID == "" {
		return
	}

	lic, err := h.Store.FindLicenseByPayPalSubscription(ctx, sub.ID)
	if err != nil {
		return
	}
	if lic.Status == model.StatusActive {
		lic.Status = model.StatusPastDue
		_ = h.Store.UpdateLicenseAndSubscription(ctx, lic, "status")

		h.Store.Audit(ctx, &model.AuditLog{
			Entity: "license", EntityID: lic.ID, Action: "payment_failed",
			ActorType: "webhook", Changes: map[string]any{"provider": "paypal"},
		})

		if h.WebhookSvc != nil {
			h.WebhookSvc.Dispatch(ctx, lic.ProductID, "license.payment_failed", map[string]any{
				"license_id": lic.ID, "email": lic.Email,
			})
		}
	}
}

func (h *PayPalHandler) onDisputeCreated(ctx context.Context, raw json.RawMessage) {
	var dispute struct {
		DisputeID     string `json:"dispute_id"`
		Reason        string `json:"reason"`
		Status        string `json:"status"`
		DisputeAmount struct {
			Value string `json:"value"`
		} `json:"dispute_amount"`
	}
	if json.Unmarshal(raw, &dispute) != nil {
		return
	}
	h.Store.Audit(ctx, &model.AuditLog{
		Entity: "dispute", EntityID: dispute.DisputeID, Action: "created",
		ActorType: "webhook",
		Changes: map[string]any{
			"reason":   dispute.Reason,
			"status":   dispute.Status,
			"amount":   dispute.DisputeAmount.Value,
			"provider": "paypal",
		},
	})
}

// CreateSubscription creates a PayPal subscription for a plan.
func (h *PayPalHandler) CreateSubscription(c *gin.Context) {
	var req struct {
		PlanID    string `json:"plan_id" binding:"required"`
		Email     string `json:"email"`
		ReturnURL string `json:"return_url"`
		CancelURL string `json:"cancel_url"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "plan_id is required")
		return
	}

	// Validate plan exists and has PayPal plan ID
	plan, err := h.Store.FindPlanByID(c, req.PlanID)
	if err != nil || plan == nil {
		response.BadRequest(c, "invalid plan_id")
		return
	}
	if plan.PayPalPlanID == "" {
		response.BadRequest(c, "plan has no PayPal plan ID configured")
		return
	}

	returnURL := req.ReturnURL
	if returnURL == "" {
		returnURL = h.BaseURL + "/checkout/success"
	}
	cancelURL := req.CancelURL
	if cancelURL == "" {
		cancelURL = h.BaseURL + "/pricing"
	}

	token, err := h.accessToken(c)
	if err != nil {
		response.Internal(c)
		return
	}

	subBody := map[string]any{
		"plan_id": plan.PayPalPlanID,
		"application_context": map[string]any{
			"return_url":  returnURL,
			"cancel_url":  cancelURL,
			"brand_name":  "Keygate",
			"user_action": "SUBSCRIBE_NOW",
			"payment_method": map[string]any{
				"payer_selected":  "PAYPAL",
				"payee_preferred": "IMMEDIATE_PAYMENT_REQUIRED",
			},
		},
	}
	if req.Email != "" {
		subBody["subscriber"] = map[string]any{
			"email_address": req.Email,
		}
	}

	data, _ := json.Marshal(subBody)
	httpReq, _ := http.NewRequestWithContext(c, "POST",
		h.apiBase()+"/v1/billing/subscriptions", bytes.NewReader(data))
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("Prefer", "return=representation")

	resp, err := paypalClient.Do(httpReq)
	if err != nil {
		response.Internal(c)
		return
	}
	defer resp.Body.Close()

	var result struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Links  []struct {
			Href string `json:"href"`
			Rel  string `json:"rel"`
		} `json:"links"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil || result.ID == "" {
		response.Internal(c)
		return
	}

	// Find the approve URL
	approveURL := ""
	for _, link := range result.Links {
		if link.Rel == "approve" {
			approveURL = link.Href
			break
		}
	}

	response.OK(c, gin.H{
		"subscription_id": result.ID,
		"approve_url":     approveURL,
		"status":          result.Status,
	})
}

// CancelSubscription cancels a PayPal subscription.
func (h *PayPalHandler) CancelSubscription(c *gin.Context) {
	var req struct {
		LicenseID string `json:"license_id" binding:"required"`
		Reason    string `json:"reason"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "license_id is required")
		return
	}

	lic, err := h.Store.FindLicenseByID(c, req.LicenseID)
	if err != nil {
		response.NotFound(c, "license not found")
		return
	}

	// Verify ownership
	emailVal, _ := c.Get("email")
	if e, ok := emailVal.(string); !ok || lic.Email != e {
		response.Forbidden(c, "not your license")
		return
	}

	if lic.PayPalSubscriptionID == "" {
		response.BadRequest(c, "no PayPal subscription")
		return
	}

	token, err := h.accessToken(c)
	if err != nil {
		response.Internal(c)
		return
	}

	reason := req.Reason
	if reason == "" {
		reason = "Customer requested cancellation"
	}

	cancelBody, _ := json.Marshal(map[string]string{"reason": reason})
	httpReq, _ := http.NewRequestWithContext(c, "POST",
		h.apiBase()+"/v1/billing/subscriptions/"+lic.PayPalSubscriptionID+"/cancel",
		bytes.NewReader(cancelBody))
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+token)

	resp, err := paypalClient.Do(httpReq)
	if err != nil {
		response.Internal(c)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		response.Internal(c)
		return
	}

	lic.Status = model.StatusCanceled
	now := time.Now()
	lic.CanceledAt = &now
	_ = h.Store.UpdateLicenseAndSubscription(c, lic, "status", "canceled_at")

	h.Store.Audit(c, &model.AuditLog{
		Entity: "license", EntityID: lic.ID, Action: "canceled",
		ActorType: "user", Changes: map[string]any{"provider": "paypal", "reason": reason},
	})

	if h.Email != nil {
		productName := ""
		if p, err := h.Store.FindProductByID(c, lic.ProductID); err == nil {
			productName = p.Name
		}
		h.Email.SendSubscriptionCanceled(lic.Email, productName, true)
	}

	response.OK(c, gin.H{"status": "canceled"})
}

// CancelSubscriptionByID cancels a PayPal subscription by its ID.
// This is used by admin handlers to cancel subscriptions during refunds.
func (h *PayPalHandler) CancelSubscriptionByID(ctx context.Context, subscriptionID, reason string) error {
	if reason == "" {
		reason = "Canceled by admin"
	}

	token, err := h.accessToken(ctx)
	if err != nil {
		return fmt.Errorf("paypal auth: %w", err)
	}

	cancelBody, _ := json.Marshal(map[string]string{"reason": reason})
	httpReq, err := http.NewRequestWithContext(ctx, "POST",
		h.apiBase()+"/v1/billing/subscriptions/"+subscriptionID+"/cancel",
		bytes.NewReader(cancelBody))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+token)

	resp, err := paypalClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("paypal cancel: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("paypal cancel failed (status %d): %s", resp.StatusCode, string(body))
	}

	return nil
}

// verifyWebhook calls PayPal's verify-webhook-signature API.
func (h *PayPalHandler) verifyWebhook(ctx context.Context, headers http.Header, body []byte) bool {
	token, err := h.accessToken(ctx)
	if err != nil {
		return false
	}

	payload := map[string]any{
		"auth_algo":         headers.Get("Paypal-Auth-Algo"),
		"cert_url":          headers.Get("Paypal-Cert-Url"),
		"transmission_id":   headers.Get("Paypal-Transmission-Id"),
		"transmission_sig":  headers.Get("Paypal-Transmission-Sig"),
		"transmission_time": headers.Get("Paypal-Transmission-Time"),
		"webhook_id":        h.WebhookID,
		"webhook_event":     json.RawMessage(body),
	}
	data, _ := json.Marshal(payload)

	req, _ := http.NewRequestWithContext(ctx, "POST",
		h.apiBase()+"/v1/notifications/verify-webhook-signature", bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := paypalClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	var result struct {
		VerificationStatus string `json:"verification_status"`
	}
	if json.NewDecoder(resp.Body).Decode(&result) != nil {
		return false
	}
	return result.VerificationStatus == "SUCCESS"
}
