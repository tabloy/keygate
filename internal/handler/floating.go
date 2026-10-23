package handler

import (
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"github.com/tabloy/keygate/internal/service"
	"github.com/tabloy/keygate/pkg/response"
)

type FloatingHandler struct {
	svc *service.FloatingService
}

func NewFloatingHandler(svc *service.FloatingService) *FloatingHandler {
	return &FloatingHandler{svc: svc}
}

func (h *FloatingHandler) CheckOut(c *gin.Context) {
	var req struct {
		LicenseKey string `json:"license_key" binding:"required"`
		Identifier string `json:"identifier" binding:"required"`
		Label      string `json:"label"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "license_key and identifier are required")
		return
	}
	var ok bool
	if req.Identifier, ok = cleanIdentifier(c, req.Identifier); !ok {
		return
	}
	req.Label = strings.TrimSpace(req.Label)
	if utf8.RuneCountInString(req.Label) > maxLabelLen {
		response.BadRequest(c, "label too long (max 256 characters)")
		return
	}
	productID, _ := c.Get("product_id")
	result, err := h.svc.CheckOut(c.Request.Context(), service.CheckOutInput{
		LicenseKey: req.LicenseKey, Identifier: req.Identifier,
		Label: req.Label, ProductID: str(productID), IPAddress: c.ClientIP(),
	})
	if err != nil {
		writeAppErr(c, err)
		return
	}
	response.OK(c, result)
}

func (h *FloatingHandler) CheckIn(c *gin.Context) {
	var req struct {
		LicenseKey string `json:"license_key" binding:"required"`
		Identifier string `json:"identifier" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "license_key and identifier are required")
		return
	}
	var ok bool
	if req.Identifier, ok = cleanIdentifier(c, req.Identifier); !ok {
		return
	}
	productID, _ := c.Get("product_id")
	err := h.svc.CheckIn(c.Request.Context(), req.LicenseKey, req.Identifier, str(productID))
	if err != nil {
		writeAppErr(c, err)
		return
	}
	response.OK(c, gin.H{"status": "checked_in"})
}

func (h *FloatingHandler) Heartbeat(c *gin.Context) {
	var req struct {
		LicenseKey string `json:"license_key" binding:"required"`
		Identifier string `json:"identifier" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "license_key and identifier are required")
		return
	}
	var ok bool
	if req.Identifier, ok = cleanIdentifier(c, req.Identifier); !ok {
		return
	}
	productID, _ := c.Get("product_id")
	result, err := h.svc.Heartbeat(c.Request.Context(), req.LicenseKey, req.Identifier, str(productID))
	if err != nil {
		writeAppErr(c, err)
		return
	}
	response.OK(c, gin.H{
		"status":            "ok",
		"expires_at":        result.ExpiresAt,
		"remaining_seconds": int(time.Until(result.ExpiresAt).Seconds()),
		"active_sessions":   result.Active,
		"max_sessions":      result.Max,
	})
}
