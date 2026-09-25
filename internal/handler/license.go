package handler

import (
	"errors"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/tabloy/keygate/internal/service"
	"github.com/tabloy/keygate/pkg/apperr"
	"github.com/tabloy/keygate/pkg/response"
)

const maxIdentifierLen = 256

type LicenseHandler struct {
	svc *service.LicenseService
}

func NewLicenseHandler(svc *service.LicenseService) *LicenseHandler {
	return &LicenseHandler{svc: svc}
}

func (h *LicenseHandler) Activate(c *gin.Context) {
	var req struct {
		LicenseKey     string `json:"license_key" binding:"required"`
		Identifier     string `json:"identifier" binding:"required"`
		IdentifierType string `json:"identifier_type"`
		Label          string `json:"label"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "license_key and identifier are required")
		return
	}
	req.Identifier = strings.TrimSpace(req.Identifier)
	if len(req.Identifier) > maxIdentifierLen {
		response.BadRequest(c, "identifier too long")
		return
	}

	productID, _ := c.Get("product_id")
	result, err := h.svc.Activate(c.Request.Context(), service.ActivateInput{
		LicenseKey:     req.LicenseKey,
		Identifier:     req.Identifier,
		IdentifierType: req.IdentifierType,
		Label:          req.Label,
		IPAddress:      c.ClientIP(),
		ProductID:      str(productID),
	})
	if err != nil {
		writeAppErr(c, err)
		return
	}
	response.OK(c, result)
}

func (h *LicenseHandler) Verify(c *gin.Context) {
	var req struct {
		LicenseKey string `json:"license_key" binding:"required"`
		Identifier string `json:"identifier" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "license_key and identifier are required")
		return
	}
	req.Identifier = strings.TrimSpace(req.Identifier)
	if len(req.Identifier) > maxIdentifierLen {
		response.BadRequest(c, "identifier too long")
		return
	}

	productID, _ := c.Get("product_id")
	result, err := h.svc.Verify(c.Request.Context(), service.VerifyInput{
		LicenseKey: req.LicenseKey,
		Identifier: req.Identifier,
		ProductID:  str(productID),
		IPAddress:  c.ClientIP(),
	})
	if err != nil {
		writeAppErr(c, err)
		return
	}
	response.OK(c, result)
}

func (h *LicenseHandler) Deactivate(c *gin.Context) {
	var req struct {
		LicenseKey string `json:"license_key" binding:"required"`
		Identifier string `json:"identifier" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "license_key and identifier are required")
		return
	}
	req.Identifier = strings.TrimSpace(req.Identifier)
	if len(req.Identifier) > maxIdentifierLen {
		response.BadRequest(c, "identifier too long")
		return
	}

	productID, _ := c.Get("product_id")
	err := h.svc.Deactivate(c.Request.Context(), service.DeactivateInput{
		LicenseKey: req.LicenseKey,
		Identifier: req.Identifier,
		ProductID:  str(productID),
		IPAddress:  c.ClientIP(),
	})
	if err != nil {
		writeAppErr(c, err)
		return
	}
	response.OK(c, gin.H{"status": "deactivated"})
}

// writeAppErr translates an AppError into a consistent API response.
func writeAppErr(c *gin.Context, err error) {
	var ae *apperr.AppError
	if errors.As(err, &ae) {
		if ae.Details != nil {
			// A refusal that says how long to wait says it in the
			// header too, so a client that reads neither the body nor
			// our error codes still backs off.
			if d, ok := ae.Details.(map[string]any); ok {
				if secs, ok := d["retry_after"].(int); ok && secs > 0 {
					c.Header("Retry-After", strconv.Itoa(secs))
				}
			}
			response.ErrWithDetails(c, ae.Status, ae.Code, ae.Message, ae.Details)
		} else {
			response.Err(c, ae.Status, ae.Code, ae.Message)
		}
		// A 500 carries a cause the client never sees. It was being
		// dropped here, which left every SDK-facing failure —
		// activate, verify, usage, floating — as a bare status code
		// with nothing in the log to explain it. The 4xx answers are
		// self-explanatory and stay quiet.
		if ae.Status >= 500 && ae.Cause != nil {
			response.LogInternal(c, ae.Cause)
		}
		return
	}
	response.Internal(c, err)
}

func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
