package config

import "testing"

func TestIsDevLoginAllowed(t *testing.T) {
	tests := []struct {
		env  string
		want bool
	}{
		{"development", true},
		{"Development", true},
		{"DEVELOPMENT", true},
		{" development ", true},
		{"production", false},
		{"staging", false},
		{"", false},
		{"dev", false},
	}
	for _, tt := range tests {
		c := &Config{Environment: tt.env}
		got := c.IsDevLoginAllowed()
		if got != tt.want {
			t.Errorf("IsDevLoginAllowed(%q) = %v, want %v", tt.env, got, tt.want)
		}
	}
}

func TestIsProduction(t *testing.T) {
	tests := []struct {
		env  string
		want bool
	}{
		{"production", true},
		{"development", false},
		{"staging", false},
		{"", false},
	}
	for _, tt := range tests {
		c := &Config{Environment: tt.env}
		if got := c.IsProduction(); got != tt.want {
			t.Errorf("IsProduction(%q) = %v, want %v", tt.env, got, tt.want)
		}
	}
}

func TestIsAdminEmail(t *testing.T) {
	c := &Config{AdminEmails: []string{"admin@keygate.dev", "boss@company.com"}}

	tests := []struct {
		email string
		want  bool
	}{
		{"admin@keygate.dev", true},
		{"ADMIN@KEYGATE.DEV", true},
		{"boss@company.com", true},
		{"user@other.com", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := c.IsAdminEmail(tt.email); got != tt.want {
			t.Errorf("IsAdminEmail(%q) = %v, want %v", tt.email, got, tt.want)
		}
	}
}

func TestValidateSecurityDefaults(t *testing.T) {
	t.Run("valid dev config", func(t *testing.T) {
		c := &Config{
			Environment:       "development",
			JWTSecret:         "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			LicenseSigningKey: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		}
		warnings, fatal := c.ValidateSecurityDefaults()
		if len(fatal) > 0 {
			t.Errorf("unexpected fatal: %v", fatal)
		}
		// Should warn about dev login
		found := false
		for _, w := range warnings {
			if w == "SECURITY: dev-login is enabled (ENVIRONMENT=development) — do NOT use in production" {
				found = true
			}
		}
		if !found {
			t.Error("expected dev-login warning")
		}
	})

	t.Run("short JWT secret", func(t *testing.T) {
		c := &Config{
			Environment:       "production",
			JWTSecret:         "short",
			LicenseSigningKey: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		}
		_, fatal := c.ValidateSecurityDefaults()
		if len(fatal) == 0 {
			t.Error("expected fatal for short JWT secret")
		}
	})

	t.Run("invalid environment", func(t *testing.T) {
		c := &Config{
			Environment:       "typo",
			JWTSecret:         "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			LicenseSigningKey: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		}
		_, fatal := c.ValidateSecurityDefaults()
		if len(fatal) == 0 {
			t.Error("expected fatal for invalid environment")
		}
	})

	t.Run("production without OAuth", func(t *testing.T) {
		c := &Config{
			Environment:       "production",
			JWTSecret:         "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			LicenseSigningKey: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		}
		warnings, _ := c.ValidateSecurityDefaults()
		found := false
		for _, w := range warnings {
			if w == "SECURITY: no OAuth provider configured — users cannot log in" {
				found = true
			}
		}
		if !found {
			t.Error("expected OAuth warning in production")
		}
	})
}
