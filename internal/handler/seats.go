package handler

import (
	"github.com/gin-gonic/gin"

	"github.com/tabloy/keygate/internal/service"
	"github.com/tabloy/keygate/pkg/response"
)

type SeatHandler struct {
	svc *service.SeatService
}

func NewSeatHandler(svc *service.SeatService) *SeatHandler {
	return &SeatHandler{svc: svc}
}

func (h *SeatHandler) AddSeat(c *gin.Context) {
	var req struct {
		LicenseKey string `json:"license_key" binding:"required"`
		Email      string `json:"email" binding:"required"`
		Role       string `json:"role"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "license_key and email are required")
		return
	}

	productID, _ := c.Get("product_id")
	seat, err := h.svc.AddSeat(c.Request.Context(), service.AddSeatInput{
		LicenseKey: req.LicenseKey,
		Email:      req.Email,
		Role:       req.Role,
		ProductID:  str(productID),
	})
	if err != nil {
		writeAppErr(c, err)
		return
	}
	response.OK(c, seat)
}

func (h *SeatHandler) RemoveSeat(c *gin.Context) {
	var req struct {
		LicenseKey string `json:"license_key" binding:"required"`
		SeatID     string `json:"seat_id" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "license_key and seat_id are required")
		return
	}

	productID, _ := c.Get("product_id")
	err := h.svc.RemoveSeat(c.Request.Context(), req.LicenseKey, req.SeatID, str(productID))
	if err != nil {
		writeAppErr(c, err)
		return
	}
	response.OK(c, gin.H{"status": "removed"})
}

func (h *SeatHandler) ListSeats(c *gin.Context) {
	var req struct {
		LicenseKey string `json:"license_key" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "license_key is required")
		return
	}

	productID, _ := c.Get("product_id")
	seats, err := h.svc.ListSeats(c.Request.Context(), req.LicenseKey, str(productID))
	if err != nil {
		writeAppErr(c, err)
		return
	}
	response.OK(c, gin.H{"seats": seats})
}
