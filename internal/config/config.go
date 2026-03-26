package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
)

type Config struct {
	Port        string
	Environment string
	BaseURL     string

	DatabaseURL string

	JWTSecret         string
	LicenseSigningKey string

	GitHubClientID     string
	GitHubClientSecret string
	GoogleClientID     string
	GoogleClientSecret string

	StripeSecretKey     string
	StripeWebhookSecret string

	PayPalClientID     string
	PayPalClientSecret string
	PayPalWebhookID    string
	PayPalSandbox      bool

	WebhookMaxAttempts    int
	WebhookRetryInterval  string
	WebhookHTTPTimeout    string
	QuotaWarningThreshold float64

	SMTPHost     string
	SMTPPort     string
	SMTPUsername string
	SMTPPassword string
	SMTPFrom     string

	RedisURL string

	RateLimitAPI   int
	RateLimitAdmin int

	AdminEmails []string
}

func Load() (*Config, error) {
	_ = godotenv.Load()

	cfg := &Config{
		Port:        envOr("PORT", "9000"),
		Environment: envOr("ENVIRONMENT", "development"),
		BaseURL:     envOr("BASE_URL", "http://localhost:9000"),

		DatabaseURL: os.Getenv("DATABASE_URL"),

		JWTSecret:         os.Getenv("JWT_SECRET"),
		LicenseSigningKey: os.Getenv("LICENSE_SIGNING_KEY"),

		GitHubClientID:     os.Getenv("GITHUB_CLIENT_ID"),
		GitHubClientSecret: os.Getenv("GITHUB_CLIENT_SECRET"),
		GoogleClientID:     os.Getenv("GOOGLE_CLIENT_ID"),
		GoogleClientSecret: os.Getenv("GOOGLE_CLIENT_SECRET"),

		StripeSecretKey:     os.Getenv("STRIPE_SECRET_KEY"),
		StripeWebhookSecret: os.Getenv("STRIPE_WEBHOOK_SECRET"),

		PayPalClientID:     os.Getenv("PAYPAL_CLIENT_ID"),
		PayPalClientSecret: os.Getenv("PAYPAL_CLIENT_SECRET"),
		PayPalWebhookID:    os.Getenv("PAYPAL_WEBHOOK_ID"),
		PayPalSandbox:      os.Getenv("PAYPAL_SANDBOX") == "true",
	}

	cfg.RedisURL = os.Getenv("REDIS_URL")

	cfg.SMTPHost = os.Getenv("SMTP_HOST")
	cfg.SMTPPort = envOr("SMTP_PORT", "587")
	cfg.SMTPUsername = os.Getenv("SMTP_USERNAME")
	cfg.SMTPPassword = os.Getenv("SMTP_PASSWORD")
	cfg.SMTPFrom = os.Getenv("SMTP_FROM")

	cfg.RateLimitAPI = envIntOr("RATE_LIMIT_API", 60)
	cfg.RateLimitAdmin = envIntOr("RATE_LIMIT_ADMIN", 120)

	cfg.WebhookMaxAttempts = envIntOr("WEBHOOK_MAX_ATTEMPTS", 5)
	cfg.WebhookRetryInterval = envOr("WEBHOOK_RETRY_INTERVAL", "30s")
	cfg.WebhookHTTPTimeout = envOr("WEBHOOK_HTTP_TIMEOUT", "10s")
	cfg.QuotaWarningThreshold = envFloatOr("QUOTA_WARNING_THRESHOLD", 0.8)

	if admins := os.Getenv("ADMIN_EMAILS"); admins != "" {
		for _, e := range strings.Split(admins, ",") {
			cfg.AdminEmails = append(cfg.AdminEmails, strings.TrimSpace(e))
		}
	}

	if cfg.DatabaseURL == "" {
		return nil, fmt.Errorf("DATABASE_URL is required")
	}
	if cfg.JWTSecret == "" {
		return nil, fmt.Errorf("JWT_SECRET is required")
	}
	if cfg.LicenseSigningKey == "" {
		return nil, fmt.Errorf("LICENSE_SIGNING_KEY is required")
	}

	return cfg, nil
}

func (c *Config) normalizedEnv() string {
	return strings.ToLower(strings.TrimSpace(c.Environment))
}

func (c *Config) IsProduction() bool { return c.normalizedEnv() == "production" }

func (c *Config) IsDevLoginAllowed() bool { return c.normalizedEnv() == "development" }

// IsAdminEmail checks if an email is in the ADMIN_EMAILS list.
// Used for backward compatibility and initial setup bootstrap.
// In normal operation, admin status is determined by the user's role in the database.
func (c *Config) IsAdminEmail(email string) bool {
	for _, e := range c.AdminEmails {
		if strings.EqualFold(e, email) {
			return true
		}
	}
	return false
}

// ValidateSecurityDefaults checks for common misconfigurations that could
// lead to security vulnerabilities in production deployments.
// Returns a list of warnings (non-fatal) and errors (fatal).
func (c *Config) ValidateSecurityDefaults() (warnings []string, fatal []string) {
	// Validate environment value
	env := strings.ToLower(strings.TrimSpace(c.Environment))
	switch env {
	case "development", "staging", "production":
		// valid
	default:
		fatal = append(fatal, "ENVIRONMENT must be 'development', 'staging', or 'production', got: '"+c.Environment+"'")
	}

	// Fatal: JWT secret too short
	if len(c.JWTSecret) < 32 {
		fatal = append(fatal, "JWT_SECRET must be at least 32 characters")
	}
	if len(c.LicenseSigningKey) < 32 {
		fatal = append(fatal, "LICENSE_SIGNING_KEY must be at least 32 characters")
	}

	if c.IsProduction() {
		// In production, OAuth must be configured
		if c.GitHubClientID == "" && c.GoogleClientID == "" {
			warnings = append(warnings, "SECURITY: no OAuth provider configured — users cannot log in")
		}
		// Must have at least one admin
		if len(c.AdminEmails) == 0 {
			warnings = append(warnings, "SECURITY: ADMIN_EMAILS is empty — no one can access the admin panel")
		}
	}

	if c.IsDevLoginAllowed() {
		warnings = append(warnings, "SECURITY: dev-login is enabled (ENVIRONMENT=development) — do NOT use in production")
	}

	return
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envIntOr(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func envFloatOr(key string, fallback float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return fallback
}
