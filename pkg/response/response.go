package response

import (
	"log/slog"
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

// Internal answers a request that failed for a reason the caller can do
// nothing about, and leaves a record of why.
//
// The body stays deliberately blank: the detail of an internal error is
// for the operator, not for whoever is on the other end of the request.
// But it has to reach the operator, and for a long time it did not.
// Handlers dropped the error and this wrote a fixed string, so a 500 in
// production left nothing behind anywhere: no cause, not even which
// route produced it. An operator watching a customer fail to log in had
// only the status code to go on.
//
// The cause is required, not variadic. It was variadic at first, so a
// call site could leave it out and still compile; 34 of them did, and
// those 500s went on logging nothing but the route, which is the
// problem this was added to fix. Passing nil is still possible but has
// to be written down, where a reviewer can see it.
func Internal(c *gin.Context, cause error) {
	LogInternal(c, cause)
	Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "an internal error occurred")
}

// LogInternal records why a request failed without writing a response.
//
// It exists for the paths that build their own reply from a typed
// error and would otherwise drop the cause on the floor: the reply is
// already decided, but the reason still has to reach the operator.
//
// Required, not variadic, for the same reason as Internal: an optional
// cause is a cause that gets left out.
func LogInternal(c *gin.Context, cause error) {
	// The route template, not the URL that was asked for. Some routes
	// carry a credential in a path segment — the customer portal
	// addresses a licence by its key — so the real path would put that
	// key into every log line this writes, and from there into
	// whatever collects them. The template says which endpoint failed,
	// which is the part worth knowing.
	route := c.FullPath()
	if route == "" {
		// No matched route (a 404 handler, or a write from middleware).
		// There is no template to name, and the raw path is exactly
		// what must not be logged, so say neither.
		route = "(unmatched)"
	}
	attrs := []any{
		"method", c.Request.Method,
		"route", route,
	}
	if id, ok := c.Get("request_id"); ok {
		attrs = append(attrs, "request_id", id)
	}
	if cause != nil {
		attrs = append(attrs, "error", cause.Error())
	}
	slog.Error("request failed", attrs...)
}

// Array returns items, or an empty slice when items is nil.
//
// A Go slice that was never appended to encodes as null, not [], and a
// collection that is sometimes null and sometimes an array is the one
// shape a typed client cannot absorb: every call site needs a special
// case for "no rows" that differs from "one row". listOK already does
// this for paginated lists; this is for the handlers that answer with
// a collection directly.
func Array[T any](items []T) []T {
	if items == nil {
		return []T{}
	}
	return items
}
