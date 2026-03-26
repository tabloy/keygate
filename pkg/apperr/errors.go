package apperr

import "fmt"

// AppError is the standard application error type.
// Handlers translate these into the appropriate HTTP status + response envelope.
type AppError struct {
	Code    string // machine-readable: "LICENSE_NOT_FOUND", "ACTIVATION_LIMIT"
	Message string // human-readable
	Status  int    // HTTP status code
	Details any    // optional structured details
	Cause   error  // underlying error (not exposed to clients)
}

func (e *AppError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.Cause)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *AppError) Unwrap() error { return e.Cause }

// Constructors for common error types.

func New(status int, code, message string) *AppError {
	return &AppError{Code: code, Message: message, Status: status}
}

func Wrap(status int, code, message string, cause error) *AppError {
	return &AppError{Code: code, Message: message, Status: status, Cause: cause}
}

func WithDetails(err *AppError, details any) *AppError {
	err.Details = details
	return err
}

// ─── Predefined errors ───

func NotFound(entity, id string) *AppError {
	return New(404, entity+"_NOT_FOUND", fmt.Sprintf("%s '%s' not found", entity, id))
}

func BadRequest(message string) *AppError {
	return New(400, "BAD_REQUEST", message)
}

func Unauthorized(message string) *AppError {
	return New(401, "UNAUTHORIZED", message)
}

func Forbidden(message string) *AppError {
	return New(403, "FORBIDDEN", message)
}

func Conflict(code, message string) *AppError {
	return New(409, code, message)
}

func Internal(cause error) *AppError {
	return Wrap(500, "INTERNAL_ERROR", "an internal error occurred", cause)
}
