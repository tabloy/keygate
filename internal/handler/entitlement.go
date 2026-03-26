package handler

import (
	"github.com/gin-gonic/gin"

	"github.com/tabloy/keygate/internal/service"
	"github.com/tabloy/keygate/pkg/response"
)

type EntitlementHandler struct {
	svc *service.EntitlementService
}

func NewEntitlementHandler(svc *service.EntitlementService) *EntitlementHandler {
	return &EntitlementHandler{svc: svc}
}

func (h *EntitlementHandler) Check(c *gin.Context) {
	var req struct {
		LicenseKey string `json:"license_key" binding:"required"`
		Feature    string `json:"feature"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "license_key is required")
		return
	}

	productID, _ := c.Get("product_id")
	result, err := h.svc.Check(c.Request.Context(), service.CheckInput{
		LicenseKey: req.LicenseKey,
		Feature:    req.Feature,
		ProductID:  str(productID),
	})
	if err != nil {
		writeAppErr(c, err)
		return
	}
	response.OK(c, result)
}
