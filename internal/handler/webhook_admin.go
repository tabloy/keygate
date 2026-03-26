package handler

import (
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
	webhooks, err := h.Store.ListWebhooks(c, c.Query("product_id"), c.Query("search"))
	if err != nil {
		response.Internal(c)
		return
	}
	response.OK(c, gin.H{"webhooks": webhooks})
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
	if len(req.Events) == 0 {
		response.BadRequest(c, "at least one event type is required")
		return
	}
	if req.Secret == "" {
		req.Secret = service.GenerateWebhookSecret()
	}

	w := &model.Webhook{
		ProductID: req.ProductID,
		URL:       req.URL,
		Secret:    req.Secret,
		Events:    req.Events,
		Active:    true,
	}
	if err := h.Store.CreateWebhook(c, w); err != nil {
		response.Internal(c)
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
	w, err := h.Store.FindWebhookByID(c, c.Param("id"))
	if err != nil {
		response.NotFound(c, "webhook not found")
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
		w.URL = *req.URL
	}
	if req.Events != nil {
		w.Events = req.Events
	}
	if req.Active != nil {
		w.Active = *req.Active
	}
	if err := h.Store.UpdateWebhook(c, w); err != nil {
		response.Internal(c)
		return
	}
	response.OK(c, w)
}

func (h *WebhookAdminHandler) DeleteWebhook(c *gin.Context) {
	if err := h.Store.DeleteWebhook(c, c.Param("id")); err != nil {
		response.Internal(c)
		return
	}
	response.NoContent(c)
}

func (h *WebhookAdminHandler) ListDeliveries(c *gin.Context) {
	deliveries, total, err := h.Store.ListWebhookDeliveries(c, c.Param("id"),
		queryInt(c, "offset", 0), queryInt(c, "limit", 50))
	if err != nil {
		response.Internal(c)
		return
	}
	response.OK(c, gin.H{"deliveries": deliveries, "total": total})
}

func (h *WebhookAdminHandler) TestWebhook(c *gin.Context) {
	w, err := h.Store.FindWebhookByID(c, c.Param("id"))
	if err != nil {
		response.NotFound(c, "webhook not found")
		return
	}
	h.Webhook.Dispatch(c, w.ProductID, "webhook.test", map[string]any{
		"webhook_id": w.ID, "message": "This is a test delivery from Keygate.",
	})
	response.OK(c, gin.H{"status": "test dispatched"})
}
