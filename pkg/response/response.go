package response

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// Envelope is the standard API response format.
//
//	{"success": true, "data": {...}}
//	{"success": false, "error": {"code": "LICENSE_NOT_FOUND", "message": "..."}}
type Envelope struct {
	Success bool   `json:"success"`
	Data    any    `json:"data,omitempty"`
	Error   *Error `json:"error,omitempty"`
}

type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Details any    `json:"details,omitempty"`
}

func OK(c *gin.Context, data any) {
	c.JSON(http.StatusOK, Envelope{Success: true, Data: data})
}

func Created(c *gin.Context, data any) {
	c.JSON(http.StatusCreated, Envelope{Success: true, Data: data})
}

func NoContent(c *gin.Context) {
	c.Status(http.StatusNoContent)
}

func Err(c *gin.Context, status int, code, message string) {
	c.JSON(status, Envelope{Success: false, Error: &Error{Code: code, Message: message}})
}

func ErrWithDetails(c *gin.Context, status int, code, message string, details any) {
	c.JSON(status, Envelope{Success: false, Error: &Error{Code: code, Message: message, Details: details}})
}

func BadRequest(c *gin.Context, message string) {
	Err(c, http.StatusBadRequest, "BAD_REQUEST", message)
}

func Unauthorized(c *gin.Context, message string) {
	Err(c, http.StatusUnauthorized, "UNAUTHORIZED", message)
}

func Forbidden(c *gin.Context, message string) {
	Err(c, http.StatusForbidden, "FORBIDDEN", message)
}

func NotFound(c *gin.Context, message string) {
	Err(c, http.StatusNotFound, "NOT_FOUND", message)
}

func Conflict(c *gin.Context, code, message string, details any) {
	ErrWithDetails(c, http.StatusConflict, code, message, details)
}

func Internal(c *gin.Context) {
	Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "an internal error occurred")
}
