package handler

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/service"
	"github.com/tabloy/keygate/internal/store"
	"github.com/tabloy/keygate/pkg/apperr"
	"github.com/tabloy/keygate/pkg/response"
)

type WebhookAdminHandler struct {
	Store   *store.Store
	Webhook *service.WebhookService
}

func NewWebhookAdminHandler(s *store.Store, wh *service.WebhookService) *WebhookAdminHandler {
	return &WebhookAdminHandler{Store: s, Webhook: wh}
}

func (h *WebhookAdminHandler) ListWebhooks(c *gin.Context) {
	productID, ok := keyProductFilter(c, c.Query("product_id"))
	if !ok {
		return
	}
	page := listPage(c)
	webhooks, total, err := h.Store.ListWebhooks(c, productID, c.Query("search"), page)
	if err != nil {
		response.Internal(c, err)
		return
	}
	listOK(c, "webhooks", webhooks, total, page)
}

func (h *WebhookAdminHandler) CreateWebhook(c *gin.Context) {
	var req struct {
		ProductID string   `json:"product_id" binding:"required"`
		URL       string   `json:"url" binding:"required"`
		Events    []string `json:"events" binding:"required"`
		Secret    string   `json:"secret"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "product_id, url, and events are required")
		return
	}
	if err := apperr.ValidateURL(req.URL); err != nil {
		response.BadRequest(c, err.Message)
		return
	}
	if err := h.Webhook.ValidateTarget(req.URL); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	if !validWebhookEvents(c, req.Events) {
		return
	}
	if req.Secret == "" {
		req.Secret = service.GenerateWebhookSecret()
	}
	if !requireKeyProductScope(c, req.ProductID) {
		return
	}
	if _, err := h.Store.FindProductByID(c, req.ProductID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			response.NotFound(c, "product not found")
			return
		}
		response.Internal(c, err)
		return
	}

	w := &model.Webhook{
		ProductID: req.ProductID,
		URL:       req.URL,
		Secret:    req.Secret,
		Events:    req.Events,
		Active:    true,
	}
	if err := h.Store.CreateWebhook(c, w); err != nil {
		response.Internal(c, err)
		return
	}

	response.Created(c, gin.H{
		"id":         w.ID,
		"product_id": w.ProductID,
		"url":        w.URL,
		"secret":     req.Secret,
		"events":     w.Events,
		"active":     w.Active,
		"created_at": w.CreatedAt,
	})
}

func (h *WebhookAdminHandler) UpdateWebhook(c *gin.Context) {
	w, ok := h.checkWebhookScope(c, c.Param("id"))
	if !ok {
		return
	}

	var req struct {
		URL    *string  `json:"url"`
		Events []string `json:"events"`
		Active *bool    `json:"active"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "invalid request")
		return
	}
	if req.URL != nil {
		if err := apperr.ValidateURL(*req.URL); err != nil {
			response.BadRequest(c, err.Message)
			return
		}
		if err := h.Webhook.ValidateTarget(*req.URL); err != nil {
			response.BadRequest(c, err.Error())
			return
		}
		w.URL = *req.URL
	}
	if req.Events != nil {
		if !validWebhookEvents(c, req.Events) {
			return
		}
		w.Events = req.Events
	}
	if req.Active != nil {
		w.Active = *req.Active
	}
	if err := h.Store.UpdateWebhook(c, w); err != nil {
		response.Internal(c, err)
		return
	}
	response.OK(c, w)
}

func (h *WebhookAdminHandler) DeleteWebhook(c *gin.Context) {
	if _, ok := h.checkWebhookScope(c, c.Param("id")); !ok {
		return
	}
	if err := h.Store.DeleteWebhook(c, c.Param("id")); err != nil {
		response.Internal(c, err)
		return
	}
	response.NoContent(c)
}

// checkWebhookScope blocks a product-scoped api_key from reaching
// webhooks that belong to another product. Same pattern as
// AdminHandler.checkLicenseScope / ReleaseAdminHandler.checkReleaseScope.
func (h *WebhookAdminHandler) checkWebhookScope(c *gin.Context, webhookID string) (*model.Webhook, bool) {
	w, err := h.Store.FindWebhookByID(c, webhookID)
	if err != nil {
		response.NotFound(c, "webhook not found")
		return nil, false
	}
	if !requireKeyProductScope(c, w.ProductID) {
		return nil, false
	}
	return w, true
}

func (h *WebhookAdminHandler) ListDeliveries(c *gin.Context) {
	if _, ok := h.checkWebhookScope(c, c.Param("id")); !ok {
		return
	}
	status := c.Query("status")
	switch status {
	case "", "pending", "delivered", "failed":
	default:
		response.BadRequest(c, "status must be pending, delivered, or failed")
		return
	}
	// The same window and the same three numbers every other list
	// answers with. This one used to read the query itself and reply
	// with total alone, so a client could not work out how many pages
	// there were: the limit it asked for is not necessarily the limit
	// it got.
	page := listPage(c)
	deliveries, total, err := h.Store.ListWebhookDeliveries(c, store.WebhookDeliveryFilter{
		WebhookID: c.Param("id"),
		Status:    status,
		Event:     c.Query("event"),
		Offset:    page.Offset,
		Limit:     page.Limit,
	})
	if err != nil {
		response.Internal(c, err)
		return
	}
	listOK(c, "deliveries", deliveries, total, page)
}

// GET /admin/webhooks/:id/deliveries/:delivery_id
//
// Detail view used by the admin UI to show the full payload + raw
// response body for diagnosing a failed delivery.
func (h *WebhookAdminHandler) GetDelivery(c *gin.Context) {
	wh, ok := h.checkWebhookScope(c, c.Param("id"))
	if !ok {
		return
	}
	d, err := h.Store.FindWebhookDeliveryByID(c, c.Param("delivery_id"))
	if err != nil {
		response.NotFound(c, "delivery not found")
		return
	}
	// Cross-check: a delivery row must belong to the webhook ID in
	// the URL. Without this an admin who knows two webhook IDs +
	// any delivery ID could pull rows from the wrong tenant.
	if d.WebhookID != wh.ID {
		response.NotFound(c, "delivery not found")
		return
	}
	response.OK(c, d)
}

// POST /admin/webhooks/:id/deliveries/:delivery_id/resend
//
// Manual re-fire of a previous delivery. Common during integration:
// the receiver was down / mis-configured and the admin wants to
// replay an event without trying to recreate the original action.
// The replay carries the SAME payload bytes (so receiver-side
// idempotency keys still match) but a fresh X-Keygate-Delivery and
// a new row in the deliveries table so retry counters are clean.
func (h *WebhookAdminHandler) ResendDelivery(c *gin.Context) {
	wh, ok := h.checkWebhookScope(c, c.Param("id"))
	if !ok {
		return
	}
	deliveryID := c.Param("delivery_id")
	orig, err := h.Store.FindWebhookDeliveryByID(c, deliveryID)
	if err != nil {
		response.NotFound(c, "delivery not found")
		return
	}
	if orig.WebhookID != wh.ID {
		response.NotFound(c, "delivery not found")
		return
	}
	fresh, err := h.Webhook.Redispatch(c, deliveryID)
	if err != nil {
		if err == service.ErrWebhookDeliveryNotResendable {
			response.Err(c, 409, "NOT_RESENDABLE",
				"webhook is deleted or inactive — re-enable it before resending")
			return
		}
		response.Internal(c, err)
		return
	}
	h.Store.Audit(c, &model.AuditLog{
		Entity: "webhook_delivery", EntityID: fresh.ID, Action: "resent",
		ActorType: "admin", ActorID: adminID(c),
		Changes: map[string]any{
			"original_delivery_id": deliveryID,
			"webhook_id":           wh.ID,
			"event":                orig.Event,
		},
	})
	response.Created(c, fresh)
}

func (h *WebhookAdminHandler) TestWebhook(c *gin.Context) {
	w, ok := h.checkWebhookScope(c, c.Param("id"))
	if !ok {
		return
	}
	d, err := h.Webhook.DeliverTest(c, w)
	if err != nil {
		if errors.Is(err, service.ErrWebhookInactive) {
			response.Err(c, http.StatusConflict, "WEBHOOK_INACTIVE", "enable the webhook before testing it")
			return
		}
		response.Internal(c, err)
		return
	}
	response.OK(c, gin.H{
		"status":        d.Status, // delivered | pending (retry scheduled) | failed
		"delivery_id":   d.ID,
		"response_code": d.ResponseCode,
		"response_body": d.ResponseBody,
	})
}

// validWebhookEvents checks a subscription list, writing a 400 and
// returning false when it is unusable. A webhook subscribed to nothing,
// or only to names nothing sends, is a row that never fires and reads
// on the dashboard exactly like a working endpoint.
func validWebhookEvents(c *gin.Context, events []string) bool {
	if len(events) == 0 {
		response.BadRequest(c, "at least one event type is required")
		return false
	}
	for _, e := range events {
		if !model.IsValidWebhookEvent(e) {
			response.BadRequest(c, fmt.Sprintf("unknown event %q (valid: %s)", e, strings.Join(model.WebhookEvents, ", ")))
			return false
		}
	}
	return true
}
