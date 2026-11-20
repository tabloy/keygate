package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/stripe/stripe-go/v82"

	"github.com/tabloy/keygate/internal/branding"
	"github.com/tabloy/keygate/internal/config"
	"github.com/tabloy/keygate/internal/crypto"
	"github.com/tabloy/keygate/internal/handler"
	"github.com/tabloy/keygate/internal/license"
	"github.com/tabloy/keygate/internal/middleware"
	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/payment"
	"github.com/tabloy/keygate/internal/service"
	"github.com/tabloy/keygate/internal/storage"
	"github.com/tabloy/keygate/internal/store"
	"github.com/tabloy/keygate/internal/version"
	"github.com/tabloy/keygate/pkg/response"
)

// credentialQueryParams are query parameters whose values must never
// reach a log line. An updater sends a signed token this way when it
// cannot set a header; the license key itself is refused in the URL,
// but a client that tries still must not have it logged. The invite
// link (/accept-invite?token=) carries the claim token: a link scanner
// fetching it leaves the token unspent, and whoever reads it in the log
// could accept the invite. Names match whatever their case.
var credentialQueryParams = []string{"license_key", "license_token", "token"}

// credentialPathPrefixes name the routes that carry a credential as a
// path segment rather than a parameter. The customer portal addresses
// a licence by its key, so the key is in the URL of every request a
// signed-in customer makes there, on the way in and in the access log
// on the way out. Redacting only the query string left those lines
// holding a working credential.
//
// Each entry is the literal prefix up to and including the trailing
// slash; the segment that follows it is the secret. The second is the
// same shape without the API prefix: no route serves it, but a request
// for it is logged all the same.
var credentialPathPrefixes = []string{"/api/v1/portal/licenses/", "/portal/licenses/"}

// credentialPathSuffixes name the first segment of each continuation
// that may follow the secret and be kept in a log line. Keeping the
// continuation is what makes an access log worth reading: it says
// which endpoint was called, not merely that something under the
// portal was. Keeping anything *else* is what leaks.
var credentialPathSuffixes = map[string]bool{"activations": true}

// redactCredentialSegments replaces the secret segment of any path
// whose shape is known to carry one.
//
// It keeps the tail of the path only when the shape is exactly the
// one the router matched: one non-empty segment holding the secret,
// then a continuation this file knows by name. Every other shape has
// its whole tail dropped, because in every other shape there is no
// way to say which segment is the secret.
//
// That is not hypothetical. A doubled slash
// (/portal/licenses//KG-SECRET/activations) makes the first segment
// empty, so splitting on the first slash redacts *nothing* and hands
// the key straight to the part that gets kept. An encoded slash
// inside the key splits it in two and leaves the second half behind.
// Neither request reaches a handler, since the router cleans the path
// before matching, but the access log runs on every request that
// arrives, matched or not, and that is where the key would land.
//
// A new sub-route not listed above costs a little log detail until
// someone adds it. The failure runs in the safe direction.
func redactCredentialSegments(urlPath string) string {
	for _, prefix := range credentialPathPrefixes {
		if !strings.HasPrefix(urlPath, prefix) {
			continue
		}
		rest := urlPath[len(prefix):]
		if rest == "" {
			return urlPath
		}
		secret, suffix, hasSuffix := strings.Cut(rest, "/")
		// Only the first segment of the continuation is checked. The
		// secret is always the segment before it, so whatever follows
		// a known continuation (an activation id, say) is not a
		// credential and can stay.
		head, _, _ := strings.Cut(suffix, "/")
		if secret != "" && hasSuffix && credentialPathSuffixes[head] {
			return prefix + "redacted/" + suffix
		}
		return prefix + "redacted"
	}
	// A shape the router would not match, the prefix in another case or
	// with an extra slash or dot segment in it (/api/v1//portal/...,
	// /api/v1/Portal/...), still reaches the access log: the whole tail
	// goes.
	cleaned := strings.ToLower(path.Clean(urlPath))
	for _, prefix := range credentialPathPrefixes {
		if strings.HasPrefix(cleaned, prefix) && len(cleaned) > len(prefix) {
			return prefix + "redacted"
		}
	}
	return urlPath
}

// redactRequestPath renders one request's path and query for a log
// line, with every credential taken out.
//
// The decoded path is what gets matched against the sensitive
// prefixes, because the decoded path is what the router matched to
// choose a handler. RequestURI keeps whatever encoding the client
// sent, so a request for /api/v1/portal/lic%65nses/KG-SECRET/... was
// routed as "licenses" and handled, while a prefix test against the
// raw string saw something else and let the key through.
func redactRequestPath(u *url.URL) string {
	out := redactCredentialSegments(u.Path)
	if u.RawQuery == "" {
		return out
	}
	return redactPath(out + "?" + u.RawQuery)
}

// redactPath replaces the value of every credential query parameter
// in a "path?query" string, leaving everything else as it was.
func redactPath(path string) string {
	i := strings.IndexByte(path, '?')
	if i < 0 {
		return redactCredentialSegments(path)
	}
	path = redactCredentialSegments(path[:i]) + path[i:]
	i = strings.IndexByte(path, '?')
	query, err := url.ParseQuery(path[i+1:])
	if err != nil {
		// Unparseable: drop the query rather than log it unexamined.
		return path[:i] + "?<redacted>"
	}
	redacted := false
	for name := range query {
		for _, credential := range credentialQueryParams {
			if strings.ToLower(name) == credential {
				query.Set(name, "redacted")
				redacted = true
			}
		}
	}
	if !redacted {
		return path
	}
	return path[:i] + "?" + query.Encode()
}

// redactedRecovery replaces gin.Recovery(). Gin's dumps the whole
// request on a panic — and on a broken pipe in any mode — masking
// only Authorization, so the request URI and the license key or
// token headers would end up in the log. This logs the same facts
// with all three redacted, and answers 500 as gin's does. Gin
// handles a broken pipe itself (there is no connection left to
// answer on); its dump goes to io.Discard.
func redactedRecovery() gin.HandlerFunc {
	return gin.CustomRecoveryWithWriter(io.Discard, func(c *gin.Context, err any) {
		fields := []any{
			"method", c.Request.Method,
			"path", redactRequestPath(c.Request.URL),
			"client_ip", c.ClientIP(),
			"panic", fmt.Sprint(err),
			"stack", string(debug.Stack()),
		}
		for _, h := range []string{"X-License-Key", "X-License-Token", "Authorization"} {
			if c.GetHeader(h) != "" {
				fields = append(fields, h, "redacted")
			}
		}
		slog.Error("panic recovered", fields...)
		// The same envelope as every other 500. AbortWithStatus alone
		// sends an empty body, which is the one reply a client cannot
		// decode: a panic is exactly when an SDK is least able to
		// afford a second parsing path. response.Err rather than
		// response.Internal, because the cause is already logged above
		// with the stack.
		response.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR",
			"an internal error occurred")
		c.Abort()
	})
}

// redactedLogFormatter is gin's default log line with the query
// string redacted.
func redactedLogFormatter(p gin.LogFormatterParams) string {
	if p.Latency > time.Minute {
		p.Latency = p.Latency.Truncate(time.Second)
	}
	return fmt.Sprintf("[GIN] %v | %3d | %13v | %15s | %-7s %#v\n%s",
		p.TimeStamp.Format("2006/01/02 - 15:04:05"),
		p.StatusCode,
		p.Latency,
		p.ClientIP,
		p.Method,
		redactPath(p.Path),
		p.ErrorMessage,
	)
}

// defaultFeedURLTTL mirrors the STORAGE_FEED_URL_TTL default: what a
// public feed's presigned links live for unless the operator says
// otherwise.
const defaultFeedURLTTL = 24 * time.Hour

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	// Security checks on startup
	warnings, fatal := cfg.ValidateSecurityDefaults()
	for _, w := range warnings {
		log.Printf("WARNING: %s", w)
	}
	for _, f := range fatal {
		log.Printf("FATAL: %s", f)
	}
	if len(fatal) > 0 {
		log.Fatalf("security validation failed — fix the above errors before starting")
	}

	db, err := store.New(cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("database: %v", err)
	}
	if err := db.RunMigrations("db/migrations"); err != nil {
		log.Fatalf("migrations: %v", err)
	}
	defer db.Close()

	logger := newLogger(os.Stdout)

	// Optional Redis/Valkey backend for rate limiting shared across
	// instances. Falls back to in-memory when REDIS_URL is unset or
	// unreachable.
	if redisClient := setupRedis(cfg.RedisURL, redisNamespace(cfg.BaseURL), logger); redisClient != nil {
		defer func() { _ = redisClient.Close() }()
	}

	if cfg.StripeSecretKey != "" {
		stripe.Key = cfg.StripeSecretKey
	}

	webhookHTTPTimeout := positiveDuration(cfg.WebhookHTTPTimeout, 10*time.Second)
	webhookRetryInterval := positiveDuration(cfg.WebhookRetryInterval, 30*time.Second)

	bf := middleware.NewBruteForceProtection(
		cfg.BFMaxFails,
		time.Duration(cfg.BFLockoutSeconds)*time.Second,
		30*time.Minute,
		5*time.Minute,
	)
	webhookSvc := service.NewWebhookService(db, logger, webhookHTTPTimeout, cfg.WebhookMaxAttempts, cfg.WebhookAllowPrivate)
	emailSvc := service.NewEmailService(cfg.SMTPHost, cfg.SMTPPort, cfg.SMTPUsername, cfg.SMTPPassword, cfg.SMTPFrom, logger, db)
	emailSvc.SetBaseURL(cfg.BaseURL)
	// LICENSE_SIGNING_KEY is a 32-byte ed25519 seed in hex. Parsed
	// once here so an invalid value fails fast at startup rather
	// than the first /license/activate call.
	licenseSigningPriv, err := license.PrivateKeyFromHex(cfg.LicenseSigningKey)
	if err != nil {
		logger.Error("LICENSE_SIGNING_KEY is unusable", "error", err)
		os.Exit(1)
	}
	licenseSvc := service.NewLicenseService(db, licenseSigningPriv, logger, bf, webhookSvc)
	usageSvc := service.NewUsageService(db, webhookSvc, emailSvc, logger, cfg.QuotaWarningThreshold)
	seatSvc := service.NewSeatService(db, webhookSvc, emailSvc, logger, cfg.BaseURL)
	entitlementSvc := service.NewEntitlementService(db, logger)
	floatingSvc := service.NewFloatingService(db, logger, bf)

	// ─── Release subsystem (storage + service) ───
	// When STORAGE_* env vars aren't configured we fall back to a Disabled
	// stub so the server still boots. Release endpoints will return 503 when
	// they reach storage; license/billing functions are unaffected.
	var releaseStorage storage.Storage = storage.Disabled{}
	if cfg.IsStorageEnabled() {
		s3, err := storage.NewS3(context.Background(), storage.S3Config{
			Endpoint:       cfg.StorageEndpoint,
			Region:         cfg.StorageRegion,
			Bucket:         cfg.StorageBucket,
			AccessKey:      cfg.StorageAccessKey,
			SecretKey:      cfg.StorageSecretKey,
			ForcePathStyle: cfg.StorageForcePathStyle,
		})
		if err != nil {
			logger.Error("storage: init failed; release endpoints will be disabled", "error", err)
		} else {
			releaseStorage = s3
			logger.Info("storage: initialized", "endpoint", cfg.StorageEndpoint, "bucket", cfg.StorageBucket)
		}
	} else {
		logger.Info("storage: disabled (STORAGE_BUCKET/ACCESS_KEY/SECRET_KEY not all set)")
	}
	uploadTTL, err := time.ParseDuration(cfg.StorageUploadTTL)
	if err != nil {
		logger.Warn("storage: invalid STORAGE_UPLOAD_TTL, using service default",
			"value", cfg.StorageUploadTTL, "error", err)
		uploadTTL = 0 // service applies its own default
	}
	downloadTTL, err := time.ParseDuration(cfg.StorageDownloadTTL)
	if err != nil {
		logger.Warn("storage: invalid STORAGE_DOWNLOAD_TTL, using service default",
			"value", cfg.StorageDownloadTTL, "error", err)
		downloadTTL = 0
	}
	// Normalised here, not per consumer: the admin API, the store's
	// fulfilment guard and the public feed all measure the same wait
	// against it, and a zero left for each of them to interpret is
	// how they came to disagree once already.
	// Longer than store.MaxDurationSetting counts as invalid: S3 and
	// compatible stores refuse to sign a link for that long, and it is
	// the same ceiling the recorded bound is held to.
	feedTTL, err := time.ParseDuration(cfg.StorageFeedURLTTL)
	if err != nil || feedTTL <= 0 || feedTTL > store.MaxDurationSetting {
		logger.Warn("storage: invalid STORAGE_FEED_URL_TTL, using the default",
			"value", cfg.StorageFeedURLTTL, "max", store.MaxDurationSetting,
			"default", defaultFeedURLTTL, "error", err)
		feedTTL = defaultFeedURLTTL
	}

	// Master encryption key drives two independent features via HKDF:
	//   - "license-key":                 AEAD over license_key plaintext at rest.
	//                                    Works even when storage is disabled.
	//   - "release-signing-private-key": AEAD over per-product Ed25519
	//                                    private keys. Requires storage.
	//
	// Subkeys are purpose-isolated — ciphertext from one cannot decrypt
	// under another.
	var releaseSigner *service.ReleaseSigningService
	if cfg.IsMasterEncryptionKeyConfigured() {
		masterRaw, err := hex.DecodeString(cfg.ReleaseKeyEncryptionKey)
		if err != nil {
			logger.Error("master key hex decode failed; license/release encryption disabled", "error", err)
		} else {
			// Wire license-key encryption — orthogonal to storage.
			db.LicenseKeyAEAD = crypto.MustDeriveAEAD(masterRaw, "license-key")
			logger.Info("license key encryption: enabled")

			// Best-effort backfill of unencrypted historical rows. Non-blocking,
			// resumable across restarts. No timeout — runs until done or shutdown.
			go func() {
				n, err := db.BackfillLicenseKeyEncrypted(context.Background(), logger)
				if err != nil {
					logger.Warn("license key backfill failed (will retry next start)",
						"encrypted_so_far", n, "error", err)
					return
				}
				if n > 0 {
					logger.Info("license key backfill complete", "encrypted", n)
				}
			}()

			// Release signing requires storage in addition to the master key.
			if cfg.IsStorageEnabled() {
				releaseAEAD := crypto.MustDeriveAEAD(masterRaw, "release-signing-private-key")
				releaseSigner = service.NewReleaseSigningService(service.ReleaseSigningServiceConfig{
					Store:       db,
					Storage:     releaseStorage,
					AEAD:        releaseAEAD,
					Logger:      logger,
					MaxSignSize: cfg.MaxReleaseSignSize,
				})
				logger.Info("release signing: enabled", "max_sign_mb", cfg.MaxReleaseSignSize/(1024*1024))

				// Artifacts signed before Tauri signatures were stored get
				// one from the key that signed them. Non-blocking and
				// idempotent, like the license key backfill above.
				go func() {
					n, err := releaseSigner.BackfillTauriSignatures(context.Background())
					if err != nil {
						logger.Warn("tauri signature backfill failed (will retry next start)",
							"filled_so_far", n, "error", err)
						return
					}
					if n > 0 {
						logger.Info("tauri signature backfill complete", "filled", n)
					}
				}()
			}
		}
	} else {
		logger.Warn("license key encryption: DISABLED (RELEASE_KEY_ENCRYPTION_KEY not set)")
	}

	releaseSvc := service.NewReleaseService(service.ReleaseServiceConfig{
		Store:       db,
		Storage:     releaseStorage,
		Signer:      releaseSigner,
		Logger:      logger,
		Webhook:     webhookSvc,
		UploadTTL:   uploadTTL,
		DownloadTTL: downloadTTL,
	})

	licenseH := handler.NewLicenseHandler(licenseSvc)
	authH := &handler.AuthHandler{Store: db, Config: cfg, Email: emailSvc, Webhook: webhookSvc}
	stripeH := &payment.StripeHandler{
		Store:         db,
		WebhookSecret: cfg.StripeWebhookSecret,
		BaseURL:       cfg.BaseURL,
		Email:         emailSvc,
		WebhookSvc:    webhookSvc,
		Livemode:      cfg.StripeLivemode,
		// Previews of subscription changes are signed with a key every
		// replica derives from the same secret.
		ChangeKey: changePreviewKey(cfg.JWTSecret),
	}
	// Initialize thread-safe webhook secret with config value
	stripeH.SetWebhookSecret(cfg.StripeWebhookSecret)
	expiryChecker := service.NewExpiryChecker(db, emailSvc, webhookSvc, logger)
	if cfg.StripeSecretKey != "" {
		expiryChecker.EndPastDueUpgrade = stripeH.EndPastDueUpgrade
		expiryChecker.EndGraceExpiredUpgrade = stripeH.EndGraceExpiredUpgrade
	}
	meteredSyncer := service.NewMeteredBillingSyncer(db, logger)
	adminH := handler.NewAdminHandler(db, webhookSvc, emailSvc, expiryChecker, meteredSyncer)
	// An update period may only be written once the links a public
	// feed handed out have expired, and this is how long they live.
	// The admin API and the fulfilment guard measure the same wait
	// against the same value. Links already signed keep the lifetime
	// they were signed with, so after lowering STORAGE_FEED_URL_TTL
	// leave the maintenance features off until the longer-lived ones
	// are gone.
	db.SetFeedURLTTL(feedTTL)
	adminH.FeedURLTTL = feedTTL
	adminH.Seats = seatSvc
	// Whether a Stripe subscription is over is Stripe's answer to
	// give; the admin API asks through this rather than reaching for
	// the SDK itself. Left nil without an API key, where the question
	// cannot be put at all.
	if cfg.StripeSecretKey != "" {
		adminH.SubscriptionEnded = stripeH.SubscriptionEnded
		adminH.CancelSubscription = stripeH.CancelForAdmin
		adminH.FollowSubscription = stripeH.FollowSubscription
	}
	// Recorded across replicas and across restarts: links signed
	// before this start keep the lifetime they were signed with, so
	// the wait is measured against the longest this install is known
	// to have used, never merely the one configured now. A database
	// that cannot answer stops the boot rather than let this replica
	// run with a shorter one.
	if bound, err := db.RaiseDurationSetting(context.Background(), store.SettingFeedURLTTLBound, feedTTL); err != nil {
		logger.Error("could not record the feed url ttl bound",
			"setting", store.SettingFeedURLTTLBound, "error", err)
		os.Exit(1)
	} else if bound > feedTTL {
		logger.Info("feed url ttl: this install has signed longer-lived links before",
			"configured", feedTTL, "bound", bound,
			"hint", "lower the "+store.SettingFeedURLTTLBound+" setting once every replica runs the shorter TTL and the older links have expired")
	}
	usageH := handler.NewUsageHandler(usageSvc)
	seatH := handler.NewSeatHandler(seatSvc)
	entitlementH := handler.NewEntitlementHandler(entitlementSvc)
	floatingH := handler.NewFloatingHandler(floatingSvc)
	webhookAdminH := handler.NewWebhookAdminHandler(db, webhookSvc)
	systemH := handler.NewSystemHandler(db)
	releaseAdminH := handler.NewReleaseAdminHandler(releaseSvc, db)
	releasePublicH := handler.NewReleasePublicHandler(handler.ReleasePublicConfig{
		Service:     releaseSvc,
		Store:       db,
		Storage:     releaseStorage,
		Logger:      logger,
		BaseURL:     cfg.BaseURL,
		DownloadTTL: downloadTTL,
		FeedTTL:     feedTTL,
		VerifyKey:   licenseSvc.SigningPublicKey(),
	})
	releaseSigningH := handler.NewReleaseSigningAdminHandler(releaseSigner, db)
	publicPlansH := handler.NewPublicPlansHandler(db, logger)

	// Sync ADMIN_EMAILS to database roles (backward compatibility / initial setup)
	if len(cfg.AdminEmails) > 0 {
		if err := db.SyncAdminEmails(context.Background(), cfg.AdminEmails); err != nil {
			logger.Error("sync admin emails failed", "error", err)
		}
	}
	if email, err := db.EnsureOwner(context.Background(), cfg.AdminEmails); err != nil {
		logger.Error("ensure owner failed", "error", err)
	} else if email != "" {
		logger.Info("no owner found, promoted an admin to owner", "email", email)
	}

	ctx, cancel := context.WithCancel(context.Background())

	// Auto-setup Stripe webhook endpoint if:
	// - Stripe secret key is configured
	// - No manual webhook secret override via env var
	// - BASE_URL is not localhost (Stripe can't deliver to localhost)
	if cfg.StripeSecretKey != "" && cfg.StripeWebhookSecret == "" && !payment.IsLocalhostURL(cfg.BaseURL) {
		stripeH.SetupWebhookEndpoint(ctx)
	}

	go webhookSvc.StartRetryLoop(ctx, webhookRetryInterval)

	go floatingSvc.StartCleanupLoop(ctx, time.Minute)

	go expiryChecker.StartExpiryLoop(ctx)

	// Metered billing sync. 5-minute cadence — Stripe aggregates
	// server-side so sub-minute polling burns rate budget for no
	// real benefit. Only starts when a Stripe key is configured;
	// otherwise the dispatcher would error every cycle.
	if cfg.StripeSecretKey != "" {
		go meteredSyncer.StartSyncLoop(ctx, 5*time.Minute)
	}

	go emailSvc.StartEmailQueueProcessor(ctx, db)

	go systemH.StartAutoCheck(ctx.Done())

	// Cleanup expired transient rows every hour. Grouped together
	// because each table grows unboundedly otherwise:
	//   - OTP codes: short TTL, but new rows on every login attempt.
	//   - Refresh tokens: rotation leaves revoked-but-not-yet-expired
	//     rows behind for reuse-detection; safe to drop only past the
	//     original 30-day expires_at.
	//   - Idempotency keys: 24h TTL per row, lots of rows for high-
	//     traffic SDK endpoints (activate / usage / floating-checkout).
	go func() {
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				db.CleanExpiredOTPs(context.Background())
				db.CleanExpiredRefreshTokens(context.Background())
				_, _ = db.IdempotencyPruneExpired(context.Background())
			}
		}
	}()

	// Periodic Stripe checkout sync — catches missed webhooks
	if cfg.StripeSecretKey != "" {
		go func() {
			// Unblock the reminders for subscriptions whose cancel state
			// predates this release without waiting a tick.
			stripeH.SyncCancelStates(ctx)
			ticker := time.NewTicker(5 * time.Minute)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					stripeH.SyncRecentCheckouts(ctx)
					stripeH.SyncPendingCheckouts(ctx)
					stripeH.RetireReplacedEndpoint(ctx)
					stripeH.SyncCancelStates(ctx)
					stripeH.SyncLapsedTermHolds(ctx)
				}
			}
		}()
	}

	// Yesterday's analytics snapshot, taken at start and checked for every
	// hour, so a server restarted more than once a day still takes one
	// (see store.TakeMissingSnapshots).
	go func() {
		snapshot := func() {
			if err := db.TakeMissingSnapshots(context.Background(), time.Now().Add(-24*time.Hour)); err != nil {
				logger.Error("analytics snapshot failed", "error", err)
			}
		}
		snapshot()
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				snapshot()
			}
		}
	}()

	if cfg.IsProduction() {
		gin.SetMode(gin.ReleaseMode)
	}
	// gin.Default() logs the query string as it came in, and updaters
	// that cannot set headers send the license key (or its token) as
	// a query parameter — it would land in every log line, and from
	// there in whatever collects them. Same engine, redacted path.
	r := gin.New()

	// Gin's default is to answer /api/v1/admin/products/ with a 301 to
	// the slash-less path, carrying an HTML body. That redirect runs
	// before NoRoute, so it is an /api/ response that is not the
	// envelope, and a client that does not follow redirects gets HTML
	// where it expects JSON. Off, so a stray trailing slash lands in
	// NoRoute and is answered 404 like any other unknown path.
	r.RedirectTrailingSlash = false

	r.Use(gin.LoggerWithFormatter(redactedLogFormatter), redactedRecovery())

	proxies, err := trustProxies(r, cfg.TrustedProxies)
	if err != nil {
		logger.Error("TRUSTED_PROXIES is not a list of addresses", "error", err)
		os.Exit(1)
	}
	switch {
	case len(proxies) == 0:
		logger.Info("client address: no proxy trusted", "trusted_proxies", "none")
	case trustsEveryone(proxies):
		logger.Warn("TRUSTED_PROXIES trusts every address: any client can choose the address its rate limits and lockouts count under",
			"trusted_proxies", strings.Join(proxies, ","))
	default:
		logger.Info("client address: X-Forwarded-For believed from", "trusted_proxies", strings.Join(proxies, ","))
	}

	r.Use(middleware.RequestID())
	r.Use(middleware.PrometheusMetrics())

	// Security headers & attribution (AGPL v3 Section 7b — see NOTICE)
	r.Use(func(c *gin.Context) {
		c.Header(branding.HeaderKey, branding.Project)
		c.Header("X-Frame-Options", "DENY")
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("X-XSS-Protection", "1; mode=block")
		c.Header("Referrer-Policy", "strict-origin-when-cross-origin")
		if strings.HasPrefix(cfg.BaseURL, "https://") {
			c.Header("Strict-Transport-Security", "max-age=31536000; includeSubDomains; preload")
		}
		c.Next()
	})

	r.Use(func(c *gin.Context) {
		if origin := c.GetHeader("Origin"); origin != "" {
			if cfg.IsProduction() && origin != cfg.BaseURL {
				if c.Request.Method == "OPTIONS" {
					// Envelope, not an empty 403. A browser never
					// reads a preflight body, but anything else that
					// gets here does, and every other refusal in the
					// API is shaped this way.
					response.Err(c, http.StatusForbidden, "FORBIDDEN",
						"origin not allowed")
					c.Abort()
					return
				}
				c.Next()
				return
			}
			c.Header("Access-Control-Allow-Origin", origin)
			c.Header("Access-Control-Allow-Methods", "GET,POST,PUT,PATCH,DELETE,OPTIONS")
			c.Header("Access-Control-Allow-Headers", "Authorization,Content-Type")
			c.Header("Access-Control-Allow-Credentials", "true")
			if c.Request.Method == "OPTIONS" {
				c.AbortWithStatus(http.StatusNoContent)
				return
			}
		}
		c.Next()
	})

	// After the attribution, security and CORS headers, so a body refused
	// as too large is answered with them like any other response (AGPL v3
	// Section 7b: X-Powered-By on every API response), and a browser can
	// read the refusal. None of those read the body; every handler comes
	// after this.
	r.Use(middleware.BodyLimit(middleware.MaxRequestBody))

	r.GET("/metrics", gin.WrapH(promhttp.Handler()))

	r.GET("/health", func(c *gin.Context) {
		status := "ok"
		checks := gin.H{}

		// DB check
		if err := db.DB.PingContext(c.Request.Context()); err != nil {
			// The cause goes to the log, not to a public endpoint: a
			// driver error can carry host names and user names.
			status = "degraded"
			checks["database"] = "error"
			logger.Error("health check: database unreachable", "error", err)
		} else {
			checks["database"] = "ok"
		}

		code := http.StatusOK
		if status != "ok" {
			code = http.StatusServiceUnavailable
		}
		c.JSON(code, gin.H{"status": status, "checks": checks, "version": version.Version})
	})

	// API documentation (public, read-only)
	docsPage := func(c *gin.Context) {
		c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(`<!DOCTYPE html>
<html><head>
<title>Keygate API</title>
<meta charset="utf-8"/>
<meta name="viewport" content="width=device-width,initial-scale=1"/>
</head><body>
<script id="api-reference" data-url="/docs/openapi.yaml" data-configuration='{"hideTryIt":true}'></script>
<script src="https://cdn.jsdelivr.net/npm/@scalar/api-reference"></script>
</body></html>`))
	}

	// The same handler on both spellings. RedirectTrailingSlash is off
	// for the sake of the API contract, and a person typing the slash
	// should still get the page rather than a JSON 404. Registered
	// twice rather than re-entered through HandleContext, which would
	// run the whole middleware chain a second time and bill the
	// request twice in the access log and in the metrics.
	r.GET("/docs", docsPage)
	r.GET("/docs/", docsPage)
	r.StaticFile("/docs/openapi.yaml", "docs/openapi.yaml")

	v1 := r.Group("/api/v1")

	v1.GET("/version", systemH.GetVersion)

	setupH := handler.NewSetupHandler(db, cfg.AdminEmails)
	v1.GET("/setup/status", setupH.Status)
	v1.POST("/setup/initialize", setupH.Initialize)

	// Public site config (no auth — used by login page, branding)
	v1.GET("/config", func(c *gin.Context) {
		settings, _ := db.GetPublicSettings(c)
		if settings == nil {
			settings = make(map[string]string)
		}
		// The public address clients reach this server at, for the
		// dashboard to show copyable feed URLs.
		settings["base_url"] = cfg.BaseURL
		// Attribution: AGPL v3 Section 7(b) — see NOTICE
		settings["attribution_text"] = branding.Tagline
		settings["attribution_url"] = branding.URL
		response.OK(c, settings)
	})

	// License-verification public key. SDKs fetch this once (or
	// hardcode it after first run) to verify the offline ed25519
	// token they get back from /license/activate and /license/verify.
	// Long-lived so it can be cached aggressively; rotation is an
	// operator action that forces clients to re-fetch.
	licensePubHex := hex.EncodeToString(licenseSvc.SigningPublicKey())
	v1.GET("/license/pubkey", func(c *gin.Context) {
		c.Header("Cache-Control", "public, max-age=3600")
		response.OK(c, gin.H{
			"algorithm":  "ed25519",
			"public_key": licensePubHex,
			"format":     "hex",
		})
	})

	// Public license endpoints.
	//
	// Auth model: license_key in body IS the credential. We dropped the
	// API-key gate because api_keys embedded in shipped client binaries
	// are not real secrets — anyone with the binary can extract them, so
	// requiring them was security theater while adding integration
	// friction. Tenant scoping still holds: each license row carries a
	// product_id FK, so a license_key can only operate on its own
	// product's data.
	//
	// Defenses kept:
	//   - LicenseBruteForceGuard — locks an IP out of /license/* after
	//     repeated failed activate / verify / deactivate / floating
	//     checkout calls
	//   - RateLimitByIPScoped — per-route-family request cap (raised
	//     because legitimate SDK clients poll on a timer). Every family
	//     has its own bucket: SDK polling from an office NAT must not
	//     eat the login budget of the people behind the same address.
	//
	// api_keys table + admin CRUD remain for server-to-server use cases
	// (a customer's backend querying license state).
	licRateLimit := max(cfg.RateLimitAPI*2, 120)
	lic := v1.Group("/license",
		middleware.LicenseBruteForceGuard(bf),
		middleware.RateLimitByIPScoped("license", licRateLimit, time.Minute))
	{
		// Idempotent-by-design endpoints (apply Idempotency-Key middleware
		// to write paths only — read-style verifies are already idempotent).
		//
		// Scope rule for this group: license_key is the SDK credential
		// — it identifies a device for verify / activate / usage /
		// download. It is NOT a seat-management credential. Seat
		// add/remove/list moved to /portal/seats/* (session auth on
		// the logged-in customer); seat-invite acceptance moved to
		// /invites/accept (public, opaque-token only). The reason is
		// trust-model alignment: a license_key sitting on the user's
		// laptop shouldn't be able to silently change the org's
		// member roster.
		idem := middleware.Idempotency(db)
		lic.POST("/activate", idem, licenseH.Activate)
		lic.POST("/usage", idem, usageH.RecordUsage)
		lic.POST("/floating/checkout", idem, floatingH.CheckOut)

		// Read / pure-status endpoints — no idempotency layer needed.
		lic.POST("/verify", licenseH.Verify)
		lic.POST("/deactivate", licenseH.Deactivate)
		lic.POST("/entitlements", entitlementH.Check)
		lic.POST("/usage/status", usageH.GetQuotaStatus)
		lic.POST("/floating/checkin", floatingH.CheckIn)
		lic.POST("/floating/heartbeat", floatingH.Heartbeat)
		lic.POST("/download", releasePublicH.Download)
	}

	// Public invite acceptance — the token is proof of email
	// ownership (we mailed it to the invitee), so no session auth
	// is required, but we DO want rate limiting against token
	// guessing. Live outside /license/* on purpose: this endpoint
	// has nothing to do with the SDK identity.
	v1.POST("/invites/accept",
		middleware.RateLimitByIPScoped("invites", 60, time.Minute),
		authH.AcceptInvite(seatSvc))

	// Release feed endpoints.
	//
	// Auth model: every channel (stable / beta / alpha / dev) is public,
	// unless the product sets feed_license_required: its feeds then need
	// a license key or token, and only serve releases inside the
	// license's update period. Trust comes from the artifact's ed25519
	// signature, verified by Sparkle/Velopack/Tauri on the client, not
	// from URL secrecy.
	//
	// Feed clients poll on a timer — sometimes once a minute. We give
	// the per-IP bucket 4× the regular API limit so a single host
	// running multiple installed products doesn't trip the 60/min default.
	feedRateLimit := max(cfg.RateLimitAPI*4, 240)
	feedMW := []gin.HandlerFunc{
		middleware.RateLimitByIPScoped("feed", feedRateLimit, time.Minute),
	}
	v1.GET("/releases/:product_slug/feed.xml", append(feedMW, releasePublicH.FeedSparkle)...)
	v1.GET("/releases/:product_slug/feed.json", append(feedMW, releasePublicH.FeedVelopack)...)
	v1.GET("/releases/:product_slug/velopack/*path", append(feedMW, releasePublicH.FeedVelopackIndex)...)
	v1.GET("/releases/:product_slug/upgrade.json", append(feedMW, releasePublicH.FeedTauri)...)

	// Old /releases/feed.* (no product slug, license-key auth) → 410 Gone
	// with a migration hint. Pre-launch hard cutover; no live clients.
	feedGone := func(c *gin.Context) {
		response.Err(c, http.StatusGone, "FEED_PATH_REMOVED",
			"feed URL changed: use /api/v1/releases/{product_slug}/feed.xml|feed.json|upgrade.json. "+
				"Feeds are public; trust is established via the artifact's ed25519 signature.")
	}
	v1.GET("/releases/feed.xml", feedGone)
	v1.GET("/releases/feed.json", feedGone)
	v1.GET("/releases/upgrade.json", feedGone)
	v1.GET("/releases/feed", feedGone)

	auth := v1.Group("/auth", middleware.RateLimitByIPScoped("auth", cfg.RateLimitAuth, time.Minute))
	{
		auth.GET("/providers", authH.Providers)
		// Its own bucket, an order of magnitude tighter than the group:
		// this is the one endpoint that mails a caller-chosen address.
		auth.POST("/otp/send",
			middleware.RateLimitByIPScoped("otp_send", cfg.RateLimitOTPSend, time.Hour),
			authH.OTPSend)
		auth.POST("/otp/verify", authH.OTPVerify)
		auth.POST("/dev-login", authH.DevLogin)
		auth.POST("/logout", middleware.SessionAuth(cfg.JWTSecret, db.FindUserIsAdmin), authH.Logout)
	}
	// Session renewal has a bucket of its own. The dashboard renews
	// silently, so every page load of a signed-out visitor tries it once;
	// in the group bucket that would spend the login budget an office
	// behind one NAT address shares.
	v1.POST("/auth/refresh",
		middleware.RateLimitByIPScoped("auth_refresh", cfg.RateLimitAuth, time.Minute),
		authH.Refresh)

	// Anonymous plan catalogue for a pricing page — the visitor has no
	// account yet, so /portal/plans (session-gated) can't serve them.
	// Rate-limited because each plan may trigger a Stripe price lookup.
	//
	// Its own bucket: RateLimitByIP keys every route as "ip:<addr>",
	// so an unscoped limiter here would spend the same counter the
	// /auth group checks against its much lower budget — a pricing
	// page busy enough behind one NAT would lock that office out of
	// logging in.
	v1.GET("/products/:product_slug/plans",
		middleware.RateLimitByIPScoped("public_plans", cfg.RateLimitAPI, time.Minute),
		publicPlansH.ListPlans)

	v1.POST("/webhook/stripe", middleware.RateLimitByIPScoped("stripe_webhook", 60, time.Minute), stripeH.Webhook)
	// Stripe verify is hit by every successful checkout return, so the
	// limit is generous, but the endpoint must NOT be naked: each call
	// proxies to Stripe's API and an attacker could otherwise force us
	// to burn rate-budget against Stripe.
	v1.GET("/checkout/verify",
		middleware.RateLimitByIPScoped("checkout_verify", 60, time.Minute),
		stripeH.VerifyCheckoutSession)

	// Unified checkout: GET /pay/:checkout_id → Stripe
	r.GET("/pay/:checkout_id", stripeH.CheckoutByPlan)

	// Self sign-up for free plans (/pay sends those plans to the form).
	// The code-sending step shares the login's buckets, so it cannot be
	// used to mail more codes than the login form could.
	v1.GET("/signup/:checkout_id",
		middleware.RateLimitByIPScoped("public_plans", cfg.RateLimitAPI, time.Minute),
		authH.SignupInfo)
	signup := v1.Group("/signup", middleware.RateLimitByIPScoped("auth", cfg.RateLimitAuth, time.Minute))
	{
		signup.POST("/:checkout_id/send",
			middleware.RateLimitByIPScoped("otp_send", cfg.RateLimitOTPSend, time.Hour),
			authH.SignupSend)
		signup.POST("/:checkout_id/verify", authH.SignupVerify)
	}

	portal := v1.Group("/portal", middleware.SessionAuth(cfg.JWTSecret, db.FindUserIsAdmin))
	{
		portal.GET("/me", authH.Me)
		portal.GET("/licenses", func(c *gin.Context) {
			emailVal, _ := c.Get("email")
			emailStr, ok := emailVal.(string)
			if !ok || emailStr == "" {
				response.Unauthorized(c, "unauthorized")
				return
			}
			licenses, err := db.ListLicensesByEmail(c, emailStr)
			if err != nil {
				response.Internal(c, err)
				return
			}
			// model.License hides the key so it can't leak from admin
			// payloads by accident. Here it is the customer's own
			// credential and the portal genuinely needs it — it is
			// both displayed and used to address the activation and
			// seat endpoints — so re-attach it explicitly: the
			// owner's key for the owner, a member's own for them.
			type portalLicense struct {
				*model.License
				LicenseKey string `json:"license_key"`
				// Upgradable says the owner can upgrade it to a paid
				// plan now (see StripeHandler.UpgradableLicenses).
				Upgradable bool `json:"upgradable"`
				// CancelScheduled says its subscription ends at the
				// period end rather than renews; told to the owner.
				CancelScheduled bool `json:"cancel_scheduled,omitempty"`
				// Features are what the license grants, its addons
				// included, as verify and the token read them.
				Features []*model.Entitlement `json:"features"`
			}
			model.FillLimits(licenses...)
			upgradable, err := stripeH.UpgradableLicenses(c, emailStr, licenses)
			if err != nil {
				response.Internal(c, err)
				return
			}
			ids := make([]string, len(licenses))
			for i, l := range licenses {
				ids[i] = l.ID
			}
			ending, err := db.CancelScheduledLicenses(c, ids)
			if err != nil {
				response.Internal(c, err)
				return
			}
			out := make([]portalLicense, 0, len(licenses))
			for _, l := range licenses {
				key, ok, err := handler.PortalLicenseView(c, db, l, emailStr)
				if err != nil {
					response.Internal(c, err)
					return
				}
				if !ok {
					continue
				}
				features, err := service.LicenseFeatures(c, db, l)
				if err != nil {
					response.Internal(c, err)
					return
				}
				if features == nil {
					features = []*model.Entitlement{}
				}
				out = append(out, portalLicense{License: l, LicenseKey: key, Upgradable: upgradable[l.ID],
					CancelScheduled: ending[l.ID] && strings.EqualFold(l.Email, emailStr), Features: features})
			}
			// The portal hides the renewal button while the switch is
			// off; the endpoint refuses the sale regardless.
			renewalsEnabled, err := db.MaintenanceFeaturesEnabled(c)
			if err != nil {
				response.Internal(c, err)
				return
			}
			c.Header("Cache-Control", "no-store")
			response.OK(c, gin.H{"licenses": out, "renewals_enabled": renewalsEnabled})
		})
		portal.PUT("/profile", func(c *gin.Context) {
			userID, _ := c.Get("user_id")
			uid, ok := userID.(string)
			if !ok || uid == "" {
				response.Unauthorized(c, "unauthorized")
				return
			}

			var req struct {
				Name string `json:"name"`
			}
			if err := c.ShouldBindJSON(&req); err != nil {
				response.BadRequest(c, "invalid request")
				return
			}

			name, ok := profileName(req.Name)
			if !ok {
				response.BadRequest(c, "name too long (max 100 characters)")
				return
			}

			if err := db.UpdateUserProfile(c, uid, name); err != nil {
				response.Internal(c, err)
				return
			}

			user, err := db.FindUserByID(c, uid)
			if err != nil {
				response.Internal(c, err)
				return
			}

			response.OK(c, gin.H{
				"id": user.ID, "email": user.Email, "name": user.Name,
				"avatar_url": user.AvatarURL, "role": user.Role,
			})
		})
		portal.GET("/plans", func(c *gin.Context) {
			productID := c.Query("product_id")
			if productID == "" {
				response.BadRequest(c, "product_id is required")
				return
			}
			plans, _, err := db.ListPlans(c, productID, "", store.All)
			if err != nil {
				response.Internal(c, err)
				return
			}
			// Only return active plans with public info
			var active []gin.H
			for _, p := range plans {
				if p.Active {
					active = append(active, gin.H{
						"id": p.ID, "name": p.Name, "slug": p.Slug,
						"license_type": p.LicenseType, "billing_interval": p.BillingInterval,
						"stripe_price_id": p.StripePriceID, "checkout_id": p.CheckoutID,
						// The portal's upgrade offers no plan selling an
						// update period; it needs this to leave them out.
						"updates_days": p.UpdatesDays,
						// A plan sold per seat lets the customer pick the
						// seats when switching to it, and the portal shows
						// what they include.
						"per_seat": p.PerSeat, "min_quantity": p.MinQuantity, "max_quantity": p.MaxQuantity,
						"max_activations": p.MaxActivations, "max_seats": p.MaxSeats, "license_model": p.LicenseModel,
					})
				}
			}
			response.OK(c, gin.H{"plans": response.Array(active)})
		})
		// The portal guards: licence operations for the owner and its
		// accepted members, team changes for the owner alone.
		portalLicenseGuard := handler.PortalLicenseGuard(db)
		portalSeatMutationGuard := handler.PortalSeatOwnerGuard(db)
		portal.POST("/usage", portalLicenseGuard, usageH.RecordUsage)
		portal.POST("/usage/status", portalLicenseGuard, usageH.GetQuotaStatus)
		portal.POST("/seats", portalLicenseGuard, seatH.ListSeats)
		portal.POST("/seats/add", portalSeatMutationGuard, seatH.AddSeat)
		portal.POST("/seats/remove", portalSeatMutationGuard, seatH.RemoveSeat)

		// Self-service activation management — solves the "I lost my
		// laptop, my activation slot is stuck" support ticket.
		portalActH := handler.NewPortalActivationsHandler(db, webhookSvc)
		portal.GET("/licenses/:license_key/activations", portalActH.List)
		portal.DELETE("/licenses/:license_key/activations/:activation_id", portalActH.Delete)

		// Subscription self-service. The handlers already verify
		// email ownership of the target license against the session
		// (see stripe.go), so nesting under /portal makes the URL
		// match the trust model rather than reading like a system
		// endpoint sitting at the top level.
		// Seats and plan changes, previewed first. Each call reads
		// Stripe, so they have a bucket of their own.
		subChangeLimit := middleware.RateLimitByIPScoped("subscription_change", 30, time.Minute)
		portal.POST("/subscription/change-plan", subChangeLimit, stripeH.ChangePlan)
		portal.POST("/subscription/preview", subChangeLimit, stripeH.PreviewSubscriptionChange)
		portal.POST("/subscription/change", subChangeLimit, stripeH.ChangeSubscription)
		portal.POST("/subscription/pending", subChangeLimit, stripeH.PendingSubscriptionChange)
		portal.POST("/subscription/cancel", stripeH.CancelSubscription)
		portal.POST("/subscription/billing-portal", stripeH.CreatePortalSession)
		portal.GET("/subscription/invoices", stripeH.ListInvoices)
		// Maintenance renewal for perpetual licenses: a one-time
		// checkout that extends updates_until.
		portal.POST("/updates/renew", stripeH.RenewUpdates)
		portal.POST("/licenses/upgrade", stripeH.StartUpgrade)
	}

	// Admin route layout: three groups under /admin, all sharing the
	// same Session-or-APIKey + rate-limit middlewares, differing only
	// in which scope(s) RequireScope accepts.
	//
	//   - admin: routes that only an interactive admin (or a key with
	//     the `admin` wildcard scope) can hit. Everything not in the
	//     other two buckets defaults here.
	//   - licWrite: license CRUD + lifecycle actions. Also reachable
	//     by API keys with `licenses:write`. Intended for merchant
	//     backends that provision licenses from their own checkout.
	//   - relWrite: release CRUD + artifact upload + publish/yank.
	//     Also reachable by `releases:write` keys for CI/CD that
	//     publishes builds.
	//
	// Signing key endpoints stay admin-only — rotating product
	// signing material is a one-off operator action, not a CI/CD
	// concern, and a leaked CI key shouldn't be able to swap the
	// product's release-signing identity.
	baseAdminMW := []gin.HandlerFunc{
		middleware.SessionOrAPIKey(cfg.JWTSecret, db, db.FindUserIsAdmin),
		middleware.RateLimitByIPScoped("admin", cfg.RateLimitAdmin, time.Minute),
	}
	adminMW := append([]gin.HandlerFunc{}, baseAdminMW...)
	adminMW = append(adminMW, middleware.RequireScope(model.ScopeAdmin))

	licWriteMW := append([]gin.HandlerFunc{}, baseAdminMW...)
	licWriteMW = append(licWriteMW, middleware.RequireScope(model.ScopeAdmin, model.ScopeLicensesWrite))

	relWriteMW := append([]gin.HandlerFunc{}, baseAdminMW...)
	relWriteMW = append(relWriteMW, middleware.RequireScope(model.ScopeAdmin, model.ScopeReleasesWrite))

	admin := v1.Group("/admin", adminMW...)
	licWrite := v1.Group("/admin", licWriteMW...)
	relWrite := v1.Group("/admin", relWriteMW...)
	{
		admin.GET("/stats", adminH.Stats)

		admin.GET("/products", adminH.ListProducts)
		admin.GET("/products/:id", adminH.GetProduct)
		admin.POST("/products", adminH.CreateProduct)
		admin.PUT("/products/:id", adminH.UpdateProduct)
		admin.DELETE("/products/:id", adminH.DeleteProduct)

		admin.GET("/plans", adminH.ListPlans)
		admin.GET("/plans/:id", adminH.GetPlan)
		admin.POST("/plans", adminH.CreatePlan)
		admin.PUT("/plans/:id", adminH.UpdatePlan)
		admin.GET("/plans/:id/seat-impact", adminH.PlanSeatImpact)
		admin.DELETE("/plans/:id", adminH.DeletePlan)

		admin.POST("/entitlements", adminH.CreateEntitlement)
		admin.PUT("/entitlements/:id", adminH.UpdateEntitlement)
		admin.DELETE("/entitlements/:id", adminH.DeleteEntitlement)

		// ─── License CRUD + lifecycle: also reachable by licenses:write keys ───
		licWrite.GET("/licenses", adminH.ListLicenses)
		licWrite.GET("/licenses/export", adminH.ExportLicenses)
		licWrite.GET("/licenses/:id", adminH.GetLicense)
		licWrite.GET("/licenses/:id/activations", adminH.ListLicenseActivations)
		licWrite.GET("/licenses/:id/key", adminH.RevealLicenseKey)
		licWrite.POST("/licenses/:id/rotate-key", adminH.RotateLicenseKey)
		// Mails the key to the address on the licence, for a send
		// that never arrived. Recipient is not caller-supplied.
		licWrite.POST("/licenses/:id/resend-email", adminH.ResendLicenseEmail)
		licWrite.POST("/licenses", adminH.CreateLicense)
		licWrite.POST("/licenses/:id/refund", adminH.RefundLicense)
		licWrite.POST("/licenses/:id/revoke", adminH.RevokeLicense)
		licWrite.POST("/licenses/:id/suspend", adminH.SuspendLicense)
		licWrite.POST("/licenses/:id/reinstate", adminH.ReinstateLicense)
		licWrite.POST("/licenses/:id/valid-until", adminH.SetLicenseValidUntil)
		licWrite.POST("/licenses/:id/quantity", adminH.SetLicenseQuantity)
		licWrite.POST("/licenses/:id/updates-until", adminH.SetLicenseUpdatesUntil)
		licWrite.POST("/licenses/:id/change-plan", adminH.ChangeLicensePlan)
		// Cuts a licence loose from a subscription Stripe has
		// finished with, so it can be managed locally again.
		licWrite.POST("/licenses/:id/stripe/unlink", adminH.UnlinkStripeSubscription)
		licWrite.GET("/licenses/:id/usage", adminH.ListLicenseUsage)
		licWrite.POST("/licenses/:id/usage/reset", adminH.ResetLicenseUsage)
		licWrite.GET("/licenses/:id/seats", adminH.ListLicenseSeats)
		licWrite.GET("/licenses/:id/seats/:seat_id/key", adminH.RevealMemberKey)
		licWrite.POST("/licenses/:id/seats/:seat_id/rotate-key", adminH.RotateMemberKey)
		licWrite.DELETE("/licenses/:id/seats/:seat_id", adminH.RemoveLicenseSeat)

		licWrite.GET("/licenses/:id/addons", adminH.ListLicenseAddons)
		licWrite.POST("/licenses/:id/addons", adminH.AddLicenseAddon)
		licWrite.DELETE("/licenses/:id/addons/:addon_id", adminH.RemoveLicenseAddon)
		licWrite.GET("/licenses/:id/floating", adminH.ListFloatingSessions)

		licWrite.DELETE("/activations/:id", adminH.DeleteActivation)

		admin.GET("/api-keys", adminH.ListAPIKeys)
		admin.POST("/api-keys", adminH.CreateAPIKey)
		admin.POST("/api-keys/:id/rotate", adminH.RotateAPIKey)
		admin.DELETE("/api-keys/:id", adminH.DeleteAPIKey)

		admin.GET("/webhooks", webhookAdminH.ListWebhooks)
		admin.POST("/webhooks", webhookAdminH.CreateWebhook)
		admin.PUT("/webhooks/:id", webhookAdminH.UpdateWebhook)
		admin.DELETE("/webhooks/:id", webhookAdminH.DeleteWebhook)
		admin.GET("/webhooks/:id/deliveries", webhookAdminH.ListDeliveries)
		admin.GET("/webhooks/:id/deliveries/:delivery_id", webhookAdminH.GetDelivery)
		admin.POST("/webhooks/:id/deliveries/:delivery_id/resend", webhookAdminH.ResendDelivery)
		admin.POST("/webhooks/:id/test", webhookAdminH.TestWebhook)

		admin.GET("/addons", adminH.ListAddons)
		admin.POST("/addons", adminH.CreateAddon)
		admin.PUT("/addons/:id", adminH.UpdateAddon)
		admin.DELETE("/addons/:id", adminH.DeleteAddon)

		admin.GET("/settings", adminH.GetSettings)
		admin.PUT("/settings", adminH.UpdateSettings)
		admin.DELETE("/settings/secrets/:key", adminH.ClearSecretSetting)
		admin.POST("/settings/test-email", adminH.SendTestEmail)
		admin.POST("/system/run-expiry-checks", adminH.RunExpiryChecks)
		admin.POST("/system/run-metered-sync", adminH.RunMeteredSync)
		admin.GET("/email-templates", adminH.GetEmailTemplates)

		admin.GET("/team", adminH.ListTeamMembers)
		admin.POST("/team", adminH.InviteTeamMember)
		admin.DELETE("/team/:id", adminH.RemoveTeamMember)

		admin.GET("/system/update-check", systemH.CheckUpdate)
		admin.GET("/system/migrations", systemH.GetMigrationStatus)

		admin.GET("/analytics", adminH.ListAnalytics)
		admin.GET("/analytics/summary", adminH.AnalyticsSummary)
		admin.GET("/analytics/breakdown", adminH.AnalyticsBreakdown)
		admin.GET("/analytics/usage-top", adminH.AnalyticsUsageTop)
		admin.GET("/analytics/activation-trend", adminH.AnalyticsActivationTrend)
		admin.GET("/analytics/insights", adminH.AnalyticsInsights)
		admin.GET("/audit-logs", adminH.ListAuditLogs)
		admin.GET("/users", adminH.ListUsers)
		admin.GET("/users/:id", adminH.GetUserDetail)

		// ─── Releases (industry-standard bundle model) ───
		// Resource: release with multiple platform artifacts (mirrors
		// GitHub Releases / Keygen). Action endpoints under /actions/.
		// Reachable by `releases:write` keys so CI/CD can ship builds
		// without holding the admin wildcard.
		relWrite.GET("/releases", releaseAdminH.List)
		relWrite.GET("/releases/:id", releaseAdminH.Get)
		relWrite.POST("/releases", releaseAdminH.Create)
		relWrite.PATCH("/releases/:id", releaseAdminH.Update)
		// Backward-compat shim: old PATCH /releases/:id/notes — drop after one cycle.
		relWrite.PATCH("/releases/:id/notes", releaseAdminH.Update)
		relWrite.DELETE("/releases/:id", releaseAdminH.Delete)

		relWrite.POST("/releases/:id/artifacts", releaseAdminH.AddArtifact)
		relWrite.POST("/releases/:id/artifacts/:aid/finalize", releaseAdminH.FinalizeArtifact)
		relWrite.DELETE("/releases/:id/artifacts/:aid", releaseAdminH.DeleteArtifact)

		relWrite.POST("/releases/:id/actions/publish", releaseAdminH.Publish)
		relWrite.POST("/releases/:id/actions/yank", releaseAdminH.Yank)
		relWrite.POST("/releases/:id/actions/unyank", releaseAdminH.Unyank)

		// ─── Release signing keys (per product) ───
		admin.POST("/products/:id/signing-key", releaseSigningH.Generate)
		admin.POST("/products/:id/signing-key/rotate", releaseSigningH.Rotate)
		admin.DELETE("/products/:id/signing-key", releaseSigningH.Deactivate)
		admin.GET("/products/:id/signing-keys", releaseSigningH.List)
		admin.GET("/products/:id/signing-key/public.pem", releaseSigningH.DownloadPublicKey)
		admin.GET("/products/:id/signing-key/tauri-pubkey", releaseSigningH.DownloadPublicKeyTauri)

	}

	serveFrontend(r)

	// Anything that reaches the router without matching a route. Gin's
	// built-in answer is the string "404 page not found" as
	// text/plain, which is the one reply this API gives that is not
	// the envelope: a client decoding every response the same way hits
	// a parse error instead of a code it can act on, and that is
	// exactly what a typo'd path, a removed endpoint or an SDK newer
	// than the server produces.
	//
	// The frontend is unaffected. serveFrontend runs as middleware and
	// answers every non-API path itself, letting only /api, /pay,
	// /health, /metrics and /docs through to the router, so this is
	// reached for those prefixes and for installs with no web/dist.
	//
	// The message names nothing from the request. One route addresses
	// a licence by its key (/portal/licenses/:license_key/...), so
	// echoing the path back would hand that key to whatever reads the
	// response — and a near-miss on that path is precisely when this
	// handler runs.
	r.NoRoute(func(c *gin.Context) {
		response.NotFound(c, "no such endpoint")
	})

	// A client that sends its headers or body a byte at a time holds a
	// connection, and its goroutine, until it is done; these bound how
	// long that can be. No write timeout: an export or a slow download
	// may take longer to send than any fixed limit.
	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		logger.Info("Keygate starting", "port", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("server stopped", "error", err)
			os.Exit(1)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	logger.Info("shutting down")

	cancel() // cancel background context — stops all goroutines

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutdownCancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("server forced shutdown", "error", err)
		os.Exit(1)
	}
	logger.Info("server exited gracefully")
}

var htmlTagPattern = regexp.MustCompile(`(?i)<[/!]?[a-z][^<>]*>|<!--[\s\S]*?-->`)

// positiveDuration parses a duration setting, falling back to def when
// it does not parse or is not above zero. Zero is not "no limit" for the
// webhook settings: a zero HTTP timeout leaves a send unbounded past the
// lease that holds its delivery, so another replica sends it again, and
// a zero retry interval makes the retry ticker panic.
func positiveDuration(s string, def time.Duration) time.Duration {
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return def
	}
	return d
}

// profileName is the name a profile update stores: trimmed, at most 100
// characters, with HTML tags stripped. ok is false when it is longer.
//
// The length is checked before the tags are stripped. Stripping takes a
// pass per level of nested tags, so on an unbounded input its time grows
// with the square of the length: a 60 KB name of nested tags took 30 s.
func profileName(raw string) (name string, ok bool) {
	name = strings.TrimSpace(raw)
	if utf8.RuneCountInString(name) > 100 {
		return "", false
	}
	return stripHTMLTags(name), true
}

// stripHTMLTags removes all HTML tags from a string to prevent XSS. It
// takes a pass per level of nested tags; callers bound its input (see
// profileName).
func stripHTMLTags(s string) string {
	for htmlTagPattern.MatchString(s) {
		s = htmlTagPattern.ReplaceAllString(s, "")
	}
	return s
}

// frontendAssetExts are the extensions a browser asks for as files,
// where the SPA shell would be the wrong answer.
var frontendAssetExts = map[string]bool{
	".js": true, ".mjs": true, ".css": true, ".map": true, ".json": true,
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".svg": true, ".webp": true, ".avif": true,
	".ico": true, ".woff": true, ".woff2": true, ".ttf": true, ".otf": true, ".wasm": true,
}

// isFrontendAsset reports whether a path was asked for as a file. A
// route of the app's own ("/licenses/1.0", say) has an extension too,
// so the list is explicit rather than "anything with a dot".
func isFrontendAsset(clean string) bool {
	// clean is a URL path (forward slashes); use path.Ext, not filepath.Ext.
	return strings.HasPrefix(clean, "/assets/") || frontendAssetExts[strings.ToLower(path.Ext(clean))]
}

// newLogger builds the application logger and makes it the one that
// package-level slog calls reach.
//
// The SetDefault is the whole point. Services are handed this logger
// explicitly, but pkg/response is not: it writes the reason behind
// every 500 through the package-level slog.Error, because a response
// helper that had to be constructed with a logger would have to be
// threaded through every handler that answers an error. Without
// SetDefault those lines went to slog's built-in default, which is a
// text handler on stderr, while everything else went to this one as
// JSON on stdout. A deployment collecting the JSON stream got every
// log line the application writes except the ones explaining its
// 500s, which are the lines worth having.
// It also reroutes the log package: after SetDefault, log.Print and
// log.Fatalf are written through this handler, at Info. That is why
// everything in main past this point logs through the returned logger
// rather than through log. A log.Fatalf here would still exit, but it
// would announce the server refusing to start as an INFO line, and a
// deployment that pages on level=ERROR would never hear about it.
func newLogger(w io.Writer) *slog.Logger {
	logger := slog.New(slog.NewJSONHandler(w, nil))
	slog.SetDefault(logger)
	return logger
}

// serveFrontend serves the React SPA from web/dist if it exists.
func serveFrontend(r *gin.Engine) {
	distPath := "web/dist"
	if _, err := os.Stat(distPath); os.IsNotExist(err) {
		return
	}

	indexHTML, err := os.ReadFile(distPath + "/index.html")
	if err != nil {
		slog.Warn("web/dist exists but index.html not found", "error", err)
		return
	}

	r.Use(func(c *gin.Context) {
		reqPath := c.Request.URL.Path

		// Let backend routes pass through.
		if strings.HasPrefix(reqPath, "/api/") || strings.HasPrefix(reqPath, "/pay/") || reqPath == "/health" || reqPath == "/metrics" || reqPath == "/docs" || strings.HasPrefix(reqPath, "/docs/") {
			c.Next()
			return
		}

		// Clean with path (URL) rules, not filepath. HTTP paths always use
		// '/', and filepath.Clean on Windows turns "/assets/…" into
		// `\assets\…`, which breaks the "/assets/" prefix check used for
		// immutable Cache-Control on hashed builds.
		// Uses c.File() instead of c.FileFromFS() to avoid http.FileServer's
		// implicit 301 redirects (e.g. /index.html → /).
		if clean := path.Clean(reqPath); clean != "/" && clean != "/index.html" {
			// path.Clean leaves a backslash alone (%5C in the URL), and on
			// Windows Join would take it as a separator and look outside
			// dist: ServeFile refuses the file, but the answer still says
			// it exists. Localize refuses a backslash, a drive letter and
			// a device name (NUL, COM1) rather than turn them into a path,
			// as http.Dir does.
			if rel, err := filepath.Localize(strings.TrimPrefix(clean, "/")); err == nil {
				filePath := filepath.Join(distPath, rel)
				if info, err := os.Stat(filePath); err == nil && !info.IsDir() {
					if strings.HasPrefix(clean, "/assets/") {
						// Every name under /assets carries a content hash,
						// so one URL never changes what it holds.
						c.Header("Cache-Control", "public, max-age=31536000, immutable")
					}
					c.File(filePath)
					c.Abort()
					return
				}
			}
			// A file that is missing is a 404, not the app shell. A tab
			// still holding an older index.html asks for the asset that
			// build referenced; answering with HTML makes the browser
			// refuse a script it was promised as JavaScript, and the
			// page comes up blank with nothing in the server log to say
			// why. A 404 sends the reload it actually needs.
			if isFrontendAsset(clean) {
				c.Status(http.StatusNotFound)
				c.Abort()
				return
			}
		}

		// SPA fallback. index.html is re-read here rather than served
		// from the copy taken at boot: a frontend rebuilt or
		// redeployed under a running server would otherwise keep
		// handing out a shell that points at assets it has deleted,
		// until someone restarts the process. The boot copy stays as
		// the fallback for a read that fails.
		html := indexHTML
		if fresh, err := os.ReadFile(filepath.Join(distPath, "index.html")); err == nil {
			html = fresh
		}
		// The shell names hashed assets, so it must not be held by a
		// cache across a deploy; what it points at may be cached
		// forever.
		c.Header("Cache-Control", "no-cache")
		c.Data(http.StatusOK, "text/html; charset=utf-8", html)
		c.Abort()
	})
}

// changePreviewKey is the key subscription change previews are signed
// with, kept apart from the session tokens the same secret signs.
func changePreviewKey(secret string) []byte {
	sum := sha256.Sum256([]byte("keygate subscription change preview\x00" + secret))
	return sum[:]
}
