package apperr

import (
	"strings"
	"testing"
)

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
		{"Alice <alice@example.com>", false},          // display-name form
		{" alice@example.com ", false},                // padded
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
		{"   ", false},                     // spaces only
		{"\t\n", false},                    // other ASCII whitespaces only
		{"\u00a0", false},                  // non-breaking space only
		{" X ", true},                      // padding around a real name is fine here
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
		{"https://", false},
		{"http://", false},
		{"https:///hook", false},
		{"http://:8080/hook", false},
		{"https://?token=x", false},
		{"http://user@/hook", false},
		{"https://exa mple.com/hook", false},
		{"https://[::1]:8080/hook", true},
		{"https://user:pass@example.com:8443/hook?x=1", true},
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

func TestValidateLicensePrefix(t *testing.T) {
	for _, tc := range []struct {
		input string
		ok    bool
	}{
		{"KG", true},
		{"ACME", true},
		{"AB12CD34", true},
		{"A", false},
		{"ABCDEFGHI", false},
		{"", false},
		{"acme", false}, // callers uppercase first
		{"AC-ME", false},
		{"AC ME", false},
		{"ÄB", false},
	} {
		err := ValidateLicensePrefix(tc.input)
		if tc.ok && err != nil {
			t.Errorf("ValidateLicensePrefix(%q) = %q, want nil", tc.input, err.Message)
		}
		if !tc.ok && err == nil {
			t.Errorf("ValidateLicensePrefix(%q) = nil, want an error", tc.input)
		}
	}
}

func TestValidateSemver(t *testing.T) {
	tests := []struct {
		input string
		ok    bool
	}{
		{"1.0.0", true},
		{"0.0.1", true},
		{"1.2.3-alpha", true},
		{"1.2.3-alpha.1", true},
		{"1.2.3-0.3.7", true},
		{"1.2.3-x.7.z.92", true},
		{"1.2.3+20130313144700", true},
		{"1.2.3-beta+exp.sha.5114f85", true},
		{"", false},
		{"1.0.0-" + strings.Repeat("a", 58), true}, // 64 characters, the limit
		{"1.0.0-" + strings.Repeat("a", 59), false},
		{"v1.0.0", false},
		{"1.0", false},
		{"1", false},
		{"01.0.0", false},
		{"1.02.0", false},
		{"1.0.03", false},
		{"1.2.3-", false},
		{"1.2.3+", false},
		{"1.2.3-01", false},       // a numeric pre-release with a leading zero
		{"1.2.3-alpha..1", false}, // an empty pre-release identifier
		{"not-a-version", false},
	}
	for _, tt := range tests {
		err := ValidateSemver(tt.input)
		if tt.ok && err != nil {
			t.Errorf("ValidateSemver(%q) = error %q, want nil", tt.input, err.Message)
		}
		if !tt.ok && err == nil {
			t.Errorf("ValidateSemver(%q) = nil, want error", tt.input)
		}
	}
}

func TestValidateHTTPURL(t *testing.T) {
	tests := []struct {
		input string
		ok    bool
	}{
		{"https://example.com/download", true},
		{"http://localhost:9000/logo.png", true},
		{"https://sub.domain.org/path?q=1#frag", true},
		{"", false},
		{"https://example.com/" + strings.Repeat("a", 2028), true}, // 2048 characters, the limit
		{"https://example.com/" + strings.Repeat("a", 2029), false},
		{"ftp://files.example.com", false},
		{"javascript:alert(1)", false},
		{"example.com/download", false},
		{"http:///path", false},
		{"://invalid", false},
	}
	for _, tt := range tests {
		err := ValidateHTTPURL("field", tt.input)
		if tt.ok && err != nil {
			t.Errorf("ValidateHTTPURL(%q) = error %q, want nil", tt.input, err.Message)
		}
		if !tt.ok && err == nil {
			t.Errorf("ValidateHTTPURL(%q) = nil, want error", tt.input)
		}
	}
}
