package apperr

import "testing"

func TestValidateEmail(t *testing.T) {
	tests := []struct {
		input string
		ok    bool
	}{
		{"user@example.com", true},
		{"a@b.co", true},
		{"test+tag@gmail.com", true},
		{"user@sub.domain.com", true},
		{"", false},
		{"not-email", false},
		{"@no-local.com", false},
		{"no-at-sign", false},
		{string(make([]byte, 255)) + "@x.com", false}, // too long
	}
	for _, tt := range tests {
		err := ValidateEmail(tt.input)
		if tt.ok && err != nil {
			t.Errorf("ValidateEmail(%q) = error %q, want nil", tt.input, err.Message)
		}
		if !tt.ok && err == nil {
			t.Errorf("ValidateEmail(%q) = nil, want error", tt.input)
		}
	}
}

func TestValidateName(t *testing.T) {
	tests := []struct {
		input string
		ok    bool
	}{
		{"Valid Name", true},
		{"X", true},
		{string(make([]byte, 200)), true},  // exactly 200
		{"", false},                        // empty
		{string(make([]byte, 201)), false}, // 201
	}
	for _, tt := range tests {
		err := ValidateName("field", tt.input)
		if tt.ok && err != nil {
			t.Errorf("ValidateName(%d chars) = error, want nil", len(tt.input))
		}
		if !tt.ok && err == nil {
			t.Errorf("ValidateName(%d chars) = nil, want error", len(tt.input))
		}
	}
}

func TestValidateSlug(t *testing.T) {
	tests := []struct {
		input string
		ok    bool
	}{
		{"my-app", true},
		{"app123", true},
		{"ab", true},
		{string(make([]byte, 100)), false}, // 100 'a's but wait - need valid chars
		{"", false},
		{"x", false}, // too short
		{"UpperCase", false},
		{"has space", false},
		{"-leading", false},
		{"trailing-", false},
		{"a@b", false},
		{"a.b", false},
		{"a_b", false},
	}
	for _, tt := range tests {
		err := ValidateSlug(tt.input)
		if tt.ok && err != nil {
			t.Errorf("ValidateSlug(%q) = error %q, want nil", tt.input, err.Message)
		}
		if !tt.ok && err == nil {
			t.Errorf("ValidateSlug(%q) = nil, want error", tt.input)
		}
	}
}

func TestValidateURL(t *testing.T) {
	tests := []struct {
		input string
		ok    bool
	}{
		{"https://example.com/webhook", true},
		{"http://localhost:9000/hook", true},
		{"https://hooks.company.io/api/v1", true},
		{"", false},
		{"example.com", false},
		{"ftp://files.com", false},
		{"not-a-url", false},
	}
	for _, tt := range tests {
		err := ValidateURL(tt.input)
		if tt.ok && err != nil {
			t.Errorf("ValidateURL(%q) = error %q, want nil", tt.input, err.Message)
		}
		if !tt.ok && err == nil {
			t.Errorf("ValidateURL(%q) = nil, want error", tt.input)
		}
	}
}
