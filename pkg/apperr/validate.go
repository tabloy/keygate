package apperr

import (
	"net/mail"
	"strings"
)

func ValidateEmail(email string) *AppError {
	if email == "" {
		return BadRequest("email is required")
	}
	if len(email) > 254 {
		return BadRequest("email is too long")
	}
	addr, err := mail.ParseAddress(email)
	if err != nil {
		return BadRequest("invalid email format")
	}
	if !strings.Contains(addr.Address, "@") {
		return BadRequest("invalid email format")
	}
	return nil
}

func ValidateName(field, value string) *AppError {
	if value == "" {
		return &AppError{Status: 400, Code: "INVALID_INPUT", Message: field + " is required"}
	}
	if len(value) > 200 {
		return &AppError{Status: 400, Code: "INVALID_INPUT", Message: field + " must be at most 200 characters"}
	}
	return nil
}

func ValidateSlug(value string) *AppError {
	if value == "" {
		return &AppError{Status: 400, Code: "INVALID_INPUT", Message: "slug is required"}
	}
	if len(value) < 2 {
		return &AppError{Status: 400, Code: "INVALID_INPUT", Message: "slug must be at least 2 characters"}
	}
	if len(value) > 100 {
		return &AppError{Status: 400, Code: "INVALID_INPUT", Message: "slug must be at most 100 characters"}
	}
	if value[0] == '-' || value[len(value)-1] == '-' {
		return &AppError{Status: 400, Code: "INVALID_INPUT", Message: "slug must not start or end with a hyphen"}
	}
	for _, c := range value {
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-') {
			return &AppError{Status: 400, Code: "INVALID_INPUT", Message: "slug must contain only lowercase letters, numbers, and hyphens (a-z, 0-9, -)"}
		}
	}
	return nil
}

func ValidateURL(value string) *AppError {
	if value == "" {
		return &AppError{Status: 400, Code: "INVALID_INPUT", Message: "url is required"}
	}
	if len(value) > 2048 {
		return &AppError{Status: 400, Code: "INVALID_INPUT", Message: "url is too long"}
	}
	if !strings.HasPrefix(value, "https://") && !strings.HasPrefix(value, "http://") {
		return &AppError{Status: 400, Code: "INVALID_INPUT", Message: "url must start with https:// or http://"}
	}
	return nil
}
