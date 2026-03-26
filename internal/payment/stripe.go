package payment

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stripe/stripe-go/v82"
	portalsession "github.com/stripe/stripe-go/v82/billingportal/session"
	"github.com/stripe/stripe-go/v82/checkout/session"
	stripeinvoice "github.com/stripe/stripe-go/v82/invoice"
	"github.com/stripe/stripe-go/v82/subscription"
	"github.com/stripe/stripe-go/v82/webhook"

	"github.com/tabloy/keygate/internal/license"
	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/service"
	"github.com/tabloy/keygate/internal/store"
	"github.com/tabloy/keygate/pkg/response"
)

type StripeHandler struct {
	Store         *store.Store
	WebhookSecret string
	BaseURL       string
	Email         *service.EmailService
	WebhookSvc    *service.WebhookService
}

func (h *StripeHandler) CreateCheckoutSession(c *gin.Context) {
	var req struct {
		PriceID    string `json:"price_id" binding:"required"`
		Email      string `json:"email"`
		SuccessURL string `json:"success_url"`
		CancelURL  string `json:"cancel_url"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "price_id is required")
		return
	}

	plan, err := h.Store.FindPlanByStripePrice(c, req.PriceID)
	if err != nil || plan == nil {
		response.BadRequest(c, "invalid price_id")
		return
	}

	success := req.SuccessURL
	if success == "" {
		success = h.BaseURL + "/checkout/success?session_id={CHECKOUT_SESSION_ID}"
	}
	cancel := req.CancelURL
	if cancel == "" {
		cancel = h.BaseURL + "/pricing"
	}

	mode := string(stripe.CheckoutSessionModeSubscription)
	if plan.LicenseType == "perpetual" {
		mode = string(stripe.CheckoutSessionModePayment)
	}

	params := &stripe.CheckoutSessionParams{
		Mode: stripe.String(mode),
		LineItems: []*stripe.CheckoutSessionLineItemParams{
			{Price: stripe.String(req.PriceID), Quantity: stripe.Int64(1)},
		},
		SuccessURL:          stripe.String(success),
		CancelURL:           stripe.String(cancel),
		AllowPromotionCodes: stripe.Bool(true),
	}
	params.Metadata = map[string]string{
		"plan_id":    plan.ID,
		"product_id": plan.ProductID,
	}
	if req.Email != "" {
		params.CustomerEmail = stripe.String(req.Email)
	}

	s, err := session.New(params)
	if err != nil {
		response.Internal(c)
		return
	}
	response.OK(c, gin.H{"url": s.URL, "session_id": s.ID})
}

func (h *StripeHandler) Webhook(c *gin.Context) {
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		response.BadRequest(c, "read failed")
		return
	}

	event, err := webhook.ConstructEvent(body, c.GetHeader("Stripe-Signature"), h.WebhookSecret)
	if err != nil {
		response.BadRequest(c, "invalid signature")
		return
	}

	// Idempotency: atomically check+record to prevent race conditions
	if !h.Store.TryRecordProcessedEvent(c, "stripe", event.ID) {
		c.JSON(http.StatusOK, gin.H{"received": true, "skipped": true})
		return
	}

	ctx := c.Request.Context()
	slog.Info("stripe webhook received", "type", event.Type, "id", event.ID)

	switch event.Type {
	case "checkout.session.completed":
		h.onCheckoutCompleted(ctx, event.Data.Raw)
	case "invoice.paid":
		h.onInvoicePaid(ctx, event.Data.Raw)
	case "customer.subscription.updated":
		h.onSubscriptionUpdated(ctx, event.Data.Raw)
	case "customer.subscription.deleted":
		h.onSubscriptionDeleted(ctx, event.Data.Raw)
	case "invoice.payment_failed":
		h.onPaymentFailed(ctx, event.Data.Raw)
	case "charge.refunded":
		h.onChargeRefunded(ctx, event.Data.Raw)
	case "charge.dispute.created":
		h.onDisputeCreated(ctx, event.Data.Raw)
	case "charge.dispute.closed":
		h.onDisputeClosed(ctx, event.Data.Raw)
	case "invoice.payment_action_required":
		h.onPaymentActionRequired(ctx, event.Data.Raw)
	case "customer.subscription.paused":
		h.onSubscriptionPaused(ctx, event.Data.Raw)
	case "customer.subscription.resumed":
		h.onSubscriptionResumed(ctx, event.Data.Raw)
	case "customer.subscription.trial_will_end":
		h.onTrialWillEnd(ctx, event.Data.Raw)
	case "invoice.upcoming":
		h.onInvoiceUpcoming(ctx, event.Data.Raw)
	case "customer.updated":
		h.onCustomerUpdated(ctx, event.Data.Raw)
	default:
		slog.Warn("stripe webhook: unhandled event type", "type", event.Type)
	}

	c.JSON(http.StatusOK, gin.H{"received": true})
}

func (h *StripeHandler) onCheckoutCompleted(ctx context.Context, raw json.RawMessage) {
	var data struct {
		CustomerEmail string            `json:"customer_email"`
		Customer      string            `json:"customer"`
		Subscription  string            `json:"subscription"`
		PaymentIntent string            `json:"payment_intent"`
		Mode          string            `json:"mode"`
		Metadata      map[string]string `json:"metadata"`
	}
	if json.Unmarshal(raw, &data) != nil {
		return
	}

	var plan *model.Plan

	if data.Subscription != "" {
		plan = h.resolvePlan(ctx, data.Subscription)
	} else if data.Metadata != nil && data.Metadata["plan_id"] != "" {
		plan, _ = h.Store.FindPlanByID(ctx, data.Metadata["plan_id"])
	}

	if plan == nil {
		return
	}

	status := model.StatusActive
	if plan.LicenseType == "trial" {
		status = model.StatusTrialing
	}

	lic := &model.License{
		ProductID:        plan.ProductID,
		PlanID:           plan.ID,
		Email:            data.CustomerEmail,
		LicenseKey:       license.GenerateKey(""),
		PaymentProvider:  "stripe",
		StripeCustomerID: data.Customer,
		Status:           status,
	}

	if plan.LicenseType == "trial" && plan.TrialDays > 0 {
		until := time.Now().Add(time.Duration(plan.TrialDays) * 24 * time.Hour)
		lic.ValidUntil = &until
	}

	if data.Subscription != "" {
		lic.StripeSubscriptionID = data.Subscription
	}

	_ = h.Store.CreateLicenseWithSubscription(ctx, lic, plan)

	productName := h.productName(ctx, plan.ProductID)
	if data.CustomerEmail != "" {
		body := fmt.Sprintf(`<!DOCTYPE html>
<html><body style="font-family: -apple-system, sans-serif; max-width: 600px; margin: 0 auto; padding: 20px;">
<h2 style="color: #111;">Your %s License</h2>
<p>Your <strong>%s</strong> license is ready.</p>
<div style="background: #f4f4f5; border-radius: 8px; padding: 16px; margin: 16px 0; font-family: monospace; font-size: 18px; text-align: center; letter-spacing: 2px;">%s</div>
<p style="color: #666; font-size: 14px;">Keep this key safe. You'll need it to activate your software.</p>
</body></html>`, productName, plan.Name, lic.LicenseKey)
		_ = h.Store.EnqueueEmail(ctx, data.CustomerEmail, "Your license for "+productName, body)
	}

	h.Store.Audit(ctx, &model.AuditLog{
		Entity: "license", EntityID: lic.ID, Action: "created",
		ActorType: "webhook",
		Changes:   map[string]any{"provider": "stripe", "email": data.CustomerEmail, "plan": plan.Name, "mode": data.Mode},
	})

	if h.WebhookSvc != nil {
		h.WebhookSvc.Dispatch(ctx, lic.ProductID, "license.created", map[string]any{
			"license_id": lic.ID, "email": lic.Email, "plan_id": lic.PlanID,
		})
	}
}

func (h *StripeHandler) onInvoicePaid(ctx context.Context, raw json.RawMessage) {
	var data struct {
		Subscription string `json:"subscription"`
		PeriodEnd    int64  `json:"period_end"`
	}
	if json.Unmarshal(raw, &data) != nil || data.Subscription == "" {
		return
	}

	lic, err := h.Store.FindLicenseByStripeSubscription(ctx, data.Subscription)
	if err != nil {
		return
	}
	until := time.Unix(data.PeriodEnd, 0)
	lic.ValidUntil = &until
	lic.Status = model.StatusActive
	_ = h.Store.UpdateLicenseAndSubscription(ctx, lic, "valid_until", "status")
}

func (h *StripeHandler) onSubscriptionUpdated(ctx context.Context, raw json.RawMessage) {
	var data struct {
		ID               string `json:"id"`
		Status           string `json:"status"`
		CurrentPeriodEnd int64  `json:"current_period_end"`
	}
	if json.Unmarshal(raw, &data) != nil {
		return
	}

	lic, err := h.Store.FindLicenseByStripeSubscription(ctx, data.ID)
	if err != nil {
		return
	}

	switch data.Status {
	case "active":
		lic.Status = model.StatusActive
	case "past_due":
		lic.Status = model.StatusPastDue
	case "trialing":
		lic.Status = model.StatusTrialing
	case "canceled", "unpaid":
		lic.Status = model.StatusCanceled
		now := time.Now()
		lic.CanceledAt = &now
	}

	until := time.Unix(data.CurrentPeriodEnd, 0)
	lic.ValidUntil = &until
	_ = h.Store.UpdateLicenseAndSubscription(ctx, lic, "status", "valid_until", "canceled_at")
}

func (h *StripeHandler) onSubscriptionDeleted(ctx context.Context, raw json.RawMessage) {
	var data struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(raw, &data) != nil {
		return
	}

	lic, err := h.Store.FindLicenseByStripeSubscription(ctx, data.ID)
	if err != nil {
		return
	}
	lic.Status = model.StatusCanceled
	now := time.Now()
	lic.CanceledAt = &now
	_ = h.Store.UpdateLicenseAndSubscription(ctx, lic, "status", "canceled_at")

	h.Store.Audit(ctx, &model.AuditLog{
		Entity: "license", EntityID: lic.ID, Action: "canceled",
		ActorType: "webhook", Changes: map[string]any{"provider": "stripe"},
	})

	if h.WebhookSvc != nil {
		h.WebhookSvc.Dispatch(ctx, lic.ProductID, "license.canceled", map[string]any{
			"license_id": lic.ID, "email": lic.Email, "reason": "subscription_deleted",
		})
	}
}

func (h *StripeHandler) onPaymentFailed(ctx context.Context, raw json.RawMessage) {
	var data struct {
		Subscription string `json:"subscription"`
	}
	if json.Unmarshal(raw, &data) != nil || data.Subscription == "" {
		return
	}

	lic, err := h.Store.FindLicenseByStripeSubscription(ctx, data.Subscription)
	if err != nil {
		return
	}

	if lic.Status == model.StatusActive {
		lic.Status = model.StatusPastDue
		_ = h.Store.UpdateLicenseAndSubscription(ctx, lic, "status")

		h.Store.Audit(ctx, &model.AuditLog{
			Entity: "license", EntityID: lic.ID, Action: "payment_failed",
			ActorType: "webhook", Changes: map[string]any{"provider": "stripe"},
		})

		if h.WebhookSvc != nil {
			h.WebhookSvc.Dispatch(ctx, lic.ProductID, "license.payment_failed", map[string]any{
				"license_id": lic.ID, "email": lic.Email,
			})
		}
	}
}

func (h *StripeHandler) onChargeRefunded(ctx context.Context, raw json.RawMessage) {
	var data struct {
		ID             string `json:"id"`
		Customer       string `json:"customer"`
		Amount         int64  `json:"amount"`
		AmountRefunded int64  `json:"amount_refunded"`
		Refunded       bool   `json:"refunded"`
		PaymentIntent  string `json:"payment_intent"`
	}
	if json.Unmarshal(raw, &data) != nil {
		return
	}

	lic, err := h.Store.FindLicenseByStripeCustomer(ctx, data.Customer)
	if err != nil {
		return
	}

	if data.Refunded {
		lic.Status = model.StatusRevoked
		_ = h.Store.UpdateLicenseAndSubscription(ctx, lic, "status")

		h.Store.Audit(ctx, &model.AuditLog{
			Entity: "license", EntityID: lic.ID, Action: "revoked",
			ActorType: "webhook",
			Changes:   map[string]any{"reason": "full_refund", "provider": "stripe", "charge_id": data.ID},
		})
	} else if data.AmountRefunded > 0 {
		h.Store.Audit(ctx, &model.AuditLog{
			Entity: "license", EntityID: lic.ID, Action: "partial_refund",
			ActorType: "webhook",
			Changes:   map[string]any{"amount_refunded": data.AmountRefunded, "provider": "stripe"},
		})
	}
}

func (h *StripeHandler) CancelSubscription(c *gin.Context) {
	var req struct {
		LicenseID string `json:"license_id" binding:"required"`
		Immediate bool   `json:"immediate"` // false = cancel at period end (default)
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

	emailVal, _ := c.Get("email")
	if e, ok := emailVal.(string); !ok || lic.Email != e {
		response.Forbidden(c, "not your license")
		return
	}

	if lic.StripeSubscriptionID == "" {
		response.BadRequest(c, "no active subscription")
		return
	}

	if req.Immediate {
		_, err = subscription.Cancel(lic.StripeSubscriptionID, nil)
	} else {
		_, err = subscription.Update(lic.StripeSubscriptionID, &stripe.SubscriptionParams{
			CancelAtPeriodEnd: stripe.Bool(true),
		})
	}
	if err != nil {
		response.Internal(c)
		return
	}

	if req.Immediate {
		lic.Status = model.StatusCanceled
		now := time.Now()
		lic.CanceledAt = &now
		_ = h.Store.UpdateLicense(c, lic, "status", "canceled_at")
	}

	h.Store.Audit(c, &model.AuditLog{
		Entity: "license", EntityID: lic.ID, Action: "cancel_requested",
		ActorType: "user",
		Changes:   map[string]any{"immediate": req.Immediate},
	})

	productName := ""
	if lic.Product != nil {
		productName = lic.Product.Name
	} else if p, err := h.Store.FindProductByID(c, lic.ProductID); err == nil {
		productName = p.Name
	}
	if h.Email != nil {
		h.Email.SendSubscriptionCanceled(lic.Email, productName, req.Immediate)
	}

	response.OK(c, gin.H{
		"status":    "canceled",
		"immediate": req.Immediate,
	})
}

func (h *StripeHandler) ChangePlan(c *gin.Context) {
	var req struct {
		LicenseID  string `json:"license_id" binding:"required"`
		NewPriceID string `json:"new_price_id" binding:"required"`
		Prorate    *bool  `json:"prorate"` // default true
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "license_id and new_price_id are required")
		return
	}

	newPlan, err := h.Store.FindPlanByStripePrice(c, req.NewPriceID)
	if err != nil || newPlan == nil {
		response.BadRequest(c, "invalid new_price_id")
		return
	}

	lic, err := h.Store.FindLicenseByID(c, req.LicenseID)
	if err != nil {
		response.NotFound(c, "license not found")
		return
	}

	if lic.StripeSubscriptionID == "" {
		response.BadRequest(c, "license has no Stripe subscription")
		return
	}

	if newPlan.ProductID != lic.ProductID {
		response.BadRequest(c, "new plan must belong to the same product")
		return
	}

	sub, err := subscription.Get(lic.StripeSubscriptionID, nil)
	if err != nil || len(sub.Items.Data) == 0 {
		response.Internal(c)
		return
	}

	prorationBehavior := "create_prorations"
	if req.Prorate != nil && !*req.Prorate {
		prorationBehavior = "none"
	}

	params := &stripe.SubscriptionParams{
		ProrationBehavior: stripe.String(prorationBehavior),
		Items: []*stripe.SubscriptionItemsParams{
			{
				ID:    stripe.String(sub.Items.Data[0].ID),
				Price: stripe.String(req.NewPriceID),
			},
		},
	}

	updatedSub, err := subscription.Update(lic.StripeSubscriptionID, params)
	if err != nil {
		response.Internal(c)
		return
	}

	oldPlanID := lic.PlanID
	lic.PlanID = newPlan.ID
	_ = h.Store.UpdateLicense(c, lic, "plan_id")

	if subRecord, err := h.Store.FindSubscriptionByLicense(c, lic.ID); err == nil {
		subRecord.PlanID = newPlan.ID
		_ = h.Store.UpdateSubscription(c, subRecord, "plan_id")
	}
	_ = updatedSub // used for audit context

	h.Store.Audit(c, &model.AuditLog{
		Entity: "license", EntityID: lic.ID, Action: "plan_changed",
		ActorType: "user",
		Changes: map[string]any{
			"old_plan_id": oldPlanID, "new_plan_id": newPlan.ID,
			"proration": prorationBehavior,
		},
	})

	response.OK(c, gin.H{
		"status":        "plan_changed",
		"new_plan_id":   newPlan.ID,
		"new_plan_name": newPlan.Name,
		"proration":     prorationBehavior,
	})
}

func (h *StripeHandler) onDisputeCreated(ctx context.Context, raw json.RawMessage) {
	var dispute struct {
		ID       string `json:"id"`
		Charge   string `json:"charge"`
		Reason   string `json:"reason"`
		Status   string `json:"status"`
		Amount   int64  `json:"amount"`
		Currency string `json:"currency"`
	}
	if json.Unmarshal(raw, &dispute) != nil {
		return
	}

	h.Store.Audit(ctx, &model.AuditLog{
		Entity: "dispute", EntityID: dispute.ID, Action: "created",
		ActorType: "webhook",
		Changes: map[string]any{
			"charge_id": dispute.Charge,
			"reason":    dispute.Reason,
			"amount":    dispute.Amount,
			"status":    dispute.Status,
		},
	})
}

func (h *StripeHandler) onDisputeClosed(ctx context.Context, raw json.RawMessage) {
	var dispute struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Reason string `json:"reason"`
	}
	if json.Unmarshal(raw, &dispute) != nil {
		return
	}

	h.Store.Audit(ctx, &model.AuditLog{
		Entity: "dispute", EntityID: dispute.ID, Action: "closed",
		ActorType: "webhook",
		Changes:   map[string]any{"status": dispute.Status, "reason": dispute.Reason},
	})
}

// CreatePortalSession creates a Stripe billing portal session for the user to manage payment methods.
func (h *StripeHandler) CreatePortalSession(c *gin.Context) {
	var req struct {
		LicenseID string `json:"license_id" binding:"required"`
		ReturnURL string `json:"return_url"`
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

	if lic.StripeCustomerID == "" {
		response.BadRequest(c, "no Stripe customer associated")
		return
	}

	returnURL := req.ReturnURL
	if returnURL == "" {
		returnURL = h.BaseURL + "/portal"
	}

	params := &stripe.BillingPortalSessionParams{
		Customer:  stripe.String(lic.StripeCustomerID),
		ReturnURL: stripe.String(returnURL),
	}
	s, err := portalsession.New(params)
	if err != nil {
		response.Internal(c)
		return
	}

	response.OK(c, gin.H{"url": s.URL})
}

// ListInvoices returns invoice history for a license's Stripe customer.
func (h *StripeHandler) ListInvoices(c *gin.Context) {
	licenseID := c.Query("license_id")
	if licenseID == "" {
		response.BadRequest(c, "license_id is required")
		return
	}

	lic, err := h.Store.FindLicenseByID(c, licenseID)
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

	if lic.StripeCustomerID == "" {
		response.OK(c, gin.H{"invoices": []any{}})
		return
	}

	params := &stripe.InvoiceListParams{
		Customer: stripe.String(lic.StripeCustomerID),
	}
	params.Filters.AddFilter("limit", "", "20")

	type invoiceItem struct {
		ID          string `json:"id"`
		Number      string `json:"number"`
		Status      string `json:"status"`
		AmountDue   int64  `json:"amount_due"`
		AmountPaid  int64  `json:"amount_paid"`
		Currency    string `json:"currency"`
		Created     int64  `json:"created"`
		PeriodStart int64  `json:"period_start"`
		PeriodEnd   int64  `json:"period_end"`
		InvoicePDF  string `json:"invoice_pdf"`
		HostedURL   string `json:"hosted_url"`
	}

	var invoices []invoiceItem
	iter := stripeinvoice.List(params)
	for iter.Next() {
		inv := iter.Invoice()
		invoices = append(invoices, invoiceItem{
			ID:          inv.ID,
			Number:      inv.Number,
			Status:      string(inv.Status),
			AmountDue:   inv.AmountDue,
			AmountPaid:  inv.AmountPaid,
			Currency:    string(inv.Currency),
			Created:     inv.Created,
			PeriodStart: inv.PeriodStart,
			PeriodEnd:   inv.PeriodEnd,
			InvoicePDF:  inv.InvoicePDF,
			HostedURL:   inv.HostedInvoiceURL,
		})
	}
	if err := iter.Err(); err != nil {
		response.Internal(c)
		return
	}

	response.OK(c, gin.H{"invoices": invoices})
}

func (h *StripeHandler) onPaymentActionRequired(ctx context.Context, raw json.RawMessage) {
	var data struct {
		Subscription     string `json:"subscription"`
		HostedInvoiceURL string `json:"hosted_invoice_url"`
	}
	if json.Unmarshal(raw, &data) != nil || data.Subscription == "" {
		return
	}
	lic, err := h.Store.FindLicenseByStripeSubscription(ctx, data.Subscription)
	if err != nil {
		return
	}
	// Notify user to complete 3DS/SCA authentication
	productName := h.productName(ctx, lic.ProductID)
	if h.Email != nil {
		h.Email.SendPaymentActionRequired(lic.Email, productName, data.HostedInvoiceURL)
	}
	h.Store.Audit(ctx, &model.AuditLog{
		Entity: "license", EntityID: lic.ID, Action: "payment_action_required",
		ActorType: "webhook", Changes: map[string]any{"provider": "stripe"},
	})
}

func (h *StripeHandler) onSubscriptionPaused(ctx context.Context, raw json.RawMessage) {
	var data struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(raw, &data) != nil {
		return
	}
	lic, err := h.Store.FindLicenseByStripeSubscription(ctx, data.ID)
	if err != nil {
		return
	}
	lic.Status = model.StatusSuspended
	now := time.Now()
	lic.SuspendedAt = &now
	_ = h.Store.UpdateLicenseAndSubscription(ctx, lic, "status", "suspended_at")

	h.Store.Audit(ctx, &model.AuditLog{
		Entity: "license", EntityID: lic.ID, Action: "suspended",
		ActorType: "webhook", Changes: map[string]any{"reason": "subscription_paused", "provider": "stripe"},
	})
	if h.WebhookSvc != nil {
		h.WebhookSvc.Dispatch(ctx, lic.ProductID, "license.suspended", map[string]any{
			"license_id": lic.ID, "email": lic.Email, "reason": "subscription_paused",
		})
	}
}

func (h *StripeHandler) onSubscriptionResumed(ctx context.Context, raw json.RawMessage) {
	var data struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(raw, &data) != nil {
		return
	}
	lic, err := h.Store.FindLicenseByStripeSubscription(ctx, data.ID)
	if err != nil {
		return
	}
	lic.Status = model.StatusActive
	lic.SuspendedAt = nil
	_ = h.Store.UpdateLicenseAndSubscription(ctx, lic, "status", "suspended_at")

	h.Store.Audit(ctx, &model.AuditLog{
		Entity: "license", EntityID: lic.ID, Action: "reinstated",
		ActorType: "webhook", Changes: map[string]any{"reason": "subscription_resumed", "provider": "stripe"},
	})
	if h.WebhookSvc != nil {
		h.WebhookSvc.Dispatch(ctx, lic.ProductID, "license.reinstated", map[string]any{
			"license_id": lic.ID, "email": lic.Email,
		})
	}
}

func (h *StripeHandler) onTrialWillEnd(ctx context.Context, raw json.RawMessage) {
	var data struct {
		ID       string `json:"id"`
		TrialEnd int64  `json:"trial_end"`
	}
	if json.Unmarshal(raw, &data) != nil {
		return
	}
	lic, err := h.Store.FindLicenseByStripeSubscription(ctx, data.ID)
	if err != nil {
		return
	}
	productName := h.productName(ctx, lic.ProductID)
	trialEnd := time.Unix(data.TrialEnd, 0).Format("2006-01-02")
	if h.Email != nil {
		h.Email.SendTrialEnding(lic.Email, productName, trialEnd)
	}
}

func (h *StripeHandler) onInvoiceUpcoming(ctx context.Context, raw json.RawMessage) {
	var data struct {
		Customer     string `json:"customer"`
		Subscription string `json:"subscription"`
		AmountDue    int64  `json:"amount_due"`
		Currency     string `json:"currency"`
	}
	if json.Unmarshal(raw, &data) != nil || data.Subscription == "" {
		return
	}
	lic, err := h.Store.FindLicenseByStripeSubscription(ctx, data.Subscription)
	if err != nil {
		return
	}
	// Renewal reminder is handled by expiry checker, but this is a backup from Stripe
	// Just audit it
	h.Store.Audit(ctx, &model.AuditLog{
		Entity: "license", EntityID: lic.ID, Action: "invoice_upcoming",
		ActorType: "webhook", Changes: map[string]any{"amount_due": data.AmountDue, "currency": data.Currency},
	})
}

func (h *StripeHandler) onCustomerUpdated(ctx context.Context, raw json.RawMessage) {
	var data struct {
		ID    string `json:"id"`
		Email string `json:"email"`
	}
	if json.Unmarshal(raw, &data) != nil || data.ID == "" {
		return
	}
	// Update email on all licenses for this customer
	if data.Email != "" {
		h.Store.UpdateLicenseEmailByStripeCustomer(ctx, data.ID, data.Email)
	}
}

func (h *StripeHandler) productName(ctx context.Context, productID string) string {
	if p, err := h.Store.FindProductByID(ctx, productID); err == nil {
		return p.Name
	}
	return ""
}

func (h *StripeHandler) resolvePlan(ctx context.Context, subID string) *model.Plan {
	sub, err := subscription.Get(subID, nil)
	if err != nil || len(sub.Items.Data) == 0 {
		return nil
	}
	plan, err := h.Store.FindPlanByStripePrice(ctx, sub.Items.Data[0].Price.ID)
	if err != nil {
		return nil
	}
	return plan
}
