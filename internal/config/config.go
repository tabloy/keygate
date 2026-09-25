package config

import (
	"encoding/hex"
	"fmt"
	"net"
	"net/url"
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

	StripeSecretKey     string
	StripeWebhookSecret string
	// StripeLivemode tells the webhook handler which environment to
	// trust. A mismatch between this flag and event.Livemode is a
	// configuration error or a forged delivery; either way the
	// handler must reject. Auto-derived from the secret key prefix
	// (sk_live_ vs sk_test_) unless STRIPE_LIVEMODE is set explicitly.
	StripeLivemode bool

	WebhookMaxAttempts   int
	WebhookRetryInterval string
	WebhookHTTPTimeout   string
	// WebhookAllowPrivate lets webhook deliveries reach loopback,
	// private and link-local addresses. Off by default so an admin
	// cannot point a webhook at the metadata service or an internal
	// host; turn it on when the receiver lives on the same network.
	WebhookAllowPrivate   bool
	QuotaWarningThreshold float64

	SMTPHost     string
	SMTPPort     string
	SMTPUsername string
	SMTPPassword string
	SMTPFrom     string

	RedisURL string

	RateLimitAPI   int
	RateLimitAdmin int
	RateLimitAuth  int
	// RateLimitOTPSend caps /auth/otp/send per IP per hour. It is far
	// tighter than RateLimitAuth because this endpoint sends mail to an
	// address the caller supplies: a loose budget turns the install
	// into a mail cannon aimed at third parties, and the bounces land
	// on the operator's sending reputation.
	//
	// 30/hour rather than something stricter because one IP is often
	// one office behind NAT. The hard stop on mailing strangers is
	// signup_mode=licensed_only; this cap just bounds the damage while
	// sign-ups are open.
	RateLimitOTPSend int

	// Brute-force protection on /license/* — caps repeated bad license
	// keys per IP. In tests these defaults are too tight, so they're
	// configurable via env: BF_MAX_FAILS=5 / BF_LOCKOUT=30s / etc.
	BFMaxFails       int
	BFLockoutSeconds int

	AdminEmails []string

	// ─── Storage (release artifacts: R2 / S3 / S3-compatible) ───
	// All fields are optional. The storage subsystem is enabled iff
	// StorageBucket is non-empty and credentials are present. When disabled,
	// release endpoints return 503 — license/billing functions are unaffected.
	StorageEndpoint       string // e.g. https://<account>.r2.cloudflarestorage.com (empty = AWS S3)
	StorageRegion         string // R2 uses "auto"; AWS S3 uses real region
	StorageBucket         string
	StorageAccessKey      string
	StorageSecretKey      string
	StoragePublicURL      string // optional CDN URL prefix for public reads (not used for license-gated downloads)
	StorageForcePathStyle bool   // true for MinIO and some self-hosted S3 gateways

	// Presigned URL TTLs.
	StorageUploadTTL   string // default "1h"
	StorageDownloadTTL string // default "10m"
	StorageFeedURLTTL  string // default "24h"; lifetime of URLs inside public feeds

	// ReleaseKeyEncryptionKey is a 64-char hex string (32 bytes) used as the
	// AES-256-GCM master key for encrypting product release-signing private
	// keys at rest. Required when storage is enabled.
	//
	// Operational notes:
	//   - Generate via: openssl rand -hex 32
	//   - Rotation requires re-encrypting all release_signing_keys rows.
	//     There is no automatic migration on key change — the operator must
	//     run a re-encryption script, otherwise existing keys become
	//     undecryptable and signing fails.
	//   - Losing this key permanently locks all signed releases.
	ReleaseKeyEncryptionKey string

	// MaxReleaseSignSize caps the largest artifact we will sign server-side.
	// Pure Ed25519 requires the full message in memory; 500 MB is a
	// reasonable default that doesn't OOM modest VMs. Larger artifacts
	// must use unsigned mode (Phase 3 will add streaming via tempfile).
	MaxReleaseSignSize int64
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

		StripeSecretKey:     os.Getenv("STRIPE_SECRET_KEY"),
		StripeWebhookSecret: os.Getenv("STRIPE_WEBHOOK_SECRET"),
	}

	envVal, envSet := os.LookupEnv("STRIPE_LIVEMODE")
	cfg.StripeLivemode = deriveLivemode(envVal, envSet, cfg.StripeSecretKey)

	cfg.RedisURL = os.Getenv("REDIS_URL")

	cfg.SMTPHost = os.Getenv("SMTP_HOST")
	cfg.SMTPPort = envOr("SMTP_PORT", "587")
	cfg.SMTPUsername = os.Getenv("SMTP_USERNAME")
	cfg.SMTPPassword = os.Getenv("SMTP_PASSWORD")
	cfg.SMTPFrom = os.Getenv("SMTP_FROM")

	cfg.RateLimitAPI = envIntOr("RATE_LIMIT_API", 60)
	cfg.RateLimitAdmin = envIntOr("RATE_LIMIT_ADMIN", 120)
	cfg.RateLimitAuth = envIntOr("RATE_LIMIT_AUTH", 60)
	cfg.RateLimitOTPSend = envIntOr("RATE_LIMIT_OTP_SEND", 30)
	cfg.BFMaxFails = envIntOr("BF_MAX_FAILS", 5)
	cfg.BFLockoutSeconds = envIntOr("BF_LOCKOUT_SECONDS", 30)

	cfg.WebhookMaxAttempts = envIntOr("WEBHOOK_MAX_ATTEMPTS", 5)
	cfg.WebhookRetryInterval = envOr("WEBHOOK_RETRY_INTERVAL", "30s")
	cfg.WebhookHTTPTimeout = envOr("WEBHOOK_HTTP_TIMEOUT", "10s")
	cfg.WebhookAllowPrivate = strings.EqualFold(os.Getenv("WEBHOOK_ALLOW_PRIVATE"), "true")
	cfg.QuotaWarningThreshold = envFloatOr("QUOTA_WARNING_THRESHOLD", 0.8)

	if admins := os.Getenv("ADMIN_EMAILS"); admins != "" {
		for e := range strings.SplitSeq(admins, ",") {
			cfg.AdminEmails = append(cfg.AdminEmails, strings.TrimSpace(e))
		}
	}

	cfg.StorageEndpoint = os.Getenv("STORAGE_ENDPOINT")
	cfg.StorageRegion = envOr("STORAGE_REGION", "auto")
	cfg.StorageBucket = os.Getenv("STORAGE_BUCKET")
	cfg.StorageAccessKey = os.Getenv("STORAGE_ACCESS_KEY")
	cfg.StorageSecretKey = os.Getenv("STORAGE_SECRET_KEY")
	cfg.StoragePublicURL = os.Getenv("STORAGE_PUBLIC_URL")
	cfg.StorageForcePathStyle = strings.EqualFold(os.Getenv("STORAGE_FORCE_PATH_STYLE"), "true")
	cfg.StorageUploadTTL = envOr("STORAGE_UPLOAD_TTL", "1h")
	cfg.StorageDownloadTTL = envOr("STORAGE_DOWNLOAD_TTL", "10m")
	cfg.StorageFeedURLTTL = envOr("STORAGE_FEED_URL_TTL", "24h")
	cfg.ReleaseKeyEncryptionKey = os.Getenv("RELEASE_KEY_ENCRYPTION_KEY")
	cfg.MaxReleaseSignSize = int64(envIntOr("MAX_RELEASE_SIGN_SIZE_MB", 500)) * 1024 * 1024

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

// isLoopbackURL reports whether a URL points at this machine. Browsers
// treat those as trustworthy even over plain HTTP, so a local trial
// needs no warning about TLS.
func isLoopbackURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	host := u.Hostname()
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

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

// deriveLivemode decides whether the server should treat Stripe
// events as live (real money) or test. Order of precedence:
//
//  1. STRIPE_LIVEMODE env (case-insensitive "true" or "1") wins.
//  2. sk_live_… / rk_live_… secret key prefix → true.
//  3. sk_test_… / rk_test_… → false.
//  4. anything else (including unset) → false. Safe default:
//     operators must opt INTO live mode rather than fall into it
//     by accident. Mismatched events get a 400 in the webhook
//     handler, so this default minimises the blast radius of a
//     half-configured deployment.
//
// Pulled out as a free function so it can be unit-tested without
// the full Load() side-effects (godotenv, ADMIN_EMAILS parsing, etc).
func deriveLivemode(envVal string, envSet bool, secretKey string) bool {
	if envSet {
		return strings.EqualFold(envVal, "true") || envVal == "1"
	}
	return strings.HasPrefix(secretKey, "sk_live_") ||
		strings.HasPrefix(secretKey, "rk_live_")
}

// IsStorageEnabled reports whether the storage subsystem (release artifacts)
// has the minimum required configuration. Endpoint/region/path-style are
// optional — only bucket+credentials are mandatory.
func (c *Config) IsStorageEnabled() bool {
	return c.StorageBucket != "" &&
		c.StorageAccessKey != "" &&
		c.StorageSecretKey != ""
}

// IsMasterEncryptionKeyConfigured reports whether the operator supplied the
// AES-256 master key that's used to derive subkeys for:
//   - license_key at-rest encryption
//   - release artifact signing private keys
//
// These two features are independently useful: a deployment that never
// distributes binaries can still benefit from license-key encryption.
// We therefore treat the master key as orthogonal to storage.
func (c *Config) IsMasterEncryptionKeyConfigured() bool {
	return len(c.ReleaseKeyEncryptionKey) == 64
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
	// LICENSE_SIGNING_KEY must be a 32-byte ed25519 seed, hex-encoded
	// (64 chars). It signs the offline-verifiable license token that
	// SDKs hand to desktop clients. Ed25519 + hex-encoded seed is the
	// industry-standard format for env-var-style key distribution.
	if seed, err := hex.DecodeString(strings.TrimSpace(c.LicenseSigningKey)); err != nil || len(seed) != 32 {
		fatal = append(fatal,
			"LICENSE_SIGNING_KEY must be a 32-byte ed25519 seed in hex (64 chars); generate with 'openssl rand -hex 32'")
	}

	if c.IsProduction() {
		// Must have at least one admin
		if len(c.AdminEmails) == 0 {
			warnings = append(warnings, "SECURITY: ADMIN_EMAILS is empty — no one can access the admin panel")
		}
		// Sessions ride a cookie. Over plain HTTP it travels in the
		// clear and cannot carry the Secure attribute — the browser
		// would drop it and every login would end in a 401 on the
		// next request. Anything more than a local trial belongs
		// behind TLS.
		if !strings.HasPrefix(strings.ToLower(c.BaseURL), "https://") && !isLoopbackURL(c.BaseURL) {
			warnings = append(warnings, "SECURITY: BASE_URL is not https — session cookies travel unprotected; put TLS in front of Keygate")
		}
	}

	if c.IsDevLoginAllowed() {
		warnings = append(warnings, "SECURITY: dev-login is enabled (ENVIRONMENT=development) — do NOT use in production")
	}

	// Storage: validate that partial config doesn't silently disable releases.
	// If any storage field is set, all required fields must be set.
	storageFieldsSet := c.StorageBucket != "" ||
		c.StorageAccessKey != "" ||
		c.StorageSecretKey != "" ||
		c.StorageEndpoint != ""
	if storageFieldsSet && !c.IsStorageEnabled() {
		fatal = append(fatal, "STORAGE_*: partial config detected — STORAGE_BUCKET, STORAGE_ACCESS_KEY, and STORAGE_SECRET_KEY must all be set together (or all empty to disable releases)")
	}

	// Release signing master key: required iff storage is enabled. Length
	// must be exactly 64 hex chars (32 bytes for AES-256). We don't accept
	// the "fallback to a derived key" mode — operators must explicitly own
	// this secret because losing it means losing the ability to verify
	// past signatures.
	// Master encryption key validation: required when storage is enabled
	// (release signing needs it) AND validated even when only set without
	// storage (license-key encryption uses it independently).
	switch {
	case c.IsStorageEnabled() && c.ReleaseKeyEncryptionKey == "":
		fatal = append(fatal, "RELEASE_KEY_ENCRYPTION_KEY is required when storage is enabled — generate via: openssl rand -hex 32")
	case c.ReleaseKeyEncryptionKey == "":
		// Not provided and not required — license encryption stays disabled,
		// release signing isn't applicable. Surfaced as a warning since this
		// disables a security feature.
		warnings = append(warnings, "SECURITY: RELEASE_KEY_ENCRYPTION_KEY is not set — license keys are stored in plaintext")
	case len(c.ReleaseKeyEncryptionKey) != 64:
		fatal = append(fatal, "RELEASE_KEY_ENCRYPTION_KEY must be exactly 64 hex chars (32 bytes for AES-256)")
	default:
		if _, err := hex.DecodeString(c.ReleaseKeyEncryptionKey); err != nil {
			fatal = append(fatal, "RELEASE_KEY_ENCRYPTION_KEY is not valid hex: "+err.Error())
		}
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
