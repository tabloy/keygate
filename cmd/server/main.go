package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/stripe/stripe-go/v82"

	"github.com/tabloy/keygate/internal/branding"
	"github.com/tabloy/keygate/internal/config"
	"github.com/tabloy/keygate/internal/handler"
	"github.com/tabloy/keygate/internal/middleware"
	"github.com/tabloy/keygate/internal/oauth"
	"github.com/tabloy/keygate/internal/payment"
	"github.com/tabloy/keygate/internal/service"
	"github.com/tabloy/keygate/internal/store"
	"github.com/tabloy/keygate/internal/version"
	"github.com/tabloy/keygate/pkg/response"
)

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

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	// Optional Redis-backed rate limiting
	if cfg.RedisURL != "" {
		logger.Info("Redis rate limiting enabled", "url", cfg.RedisURL)
		// To enable: import github.com/redis/go-redis/v9 and uncomment:
		// rdb := redis.NewClient(&redis.Options{Addr: cfg.RedisURL})
		// middleware.SetRateLimitBackend(middleware.NewRedisBackend(rdb))
	}

	if cfg.StripeSecretKey != "" {
		stripe.Key = cfg.StripeSecretKey
	}

	oauthReg := oauth.NewRegistry()
	if cfg.GitHubClientID != "" {
		oauthReg.Register(&oauth.GitHub{ClientID: cfg.GitHubClientID, ClientSecret: cfg.GitHubClientSecret})
	}
	if cfg.GoogleClientID != "" {
		oauthReg.Register(&oauth.Google{ClientID: cfg.GoogleClientID, ClientSecret: cfg.GoogleClientSecret})
	}

	webhookHTTPTimeout, err := time.ParseDuration(cfg.WebhookHTTPTimeout)
	if err != nil {
		webhookHTTPTimeout = 10 * time.Second
	}
	webhookRetryInterval, err := time.ParseDuration(cfg.WebhookRetryInterval)
	if err != nil {
		webhookRetryInterval = 30 * time.Second
	}

	bf := middleware.NewBruteForceProtection(5, 30*time.Second, 30*time.Minute, 5*time.Minute)
	webhookSvc := service.NewWebhookService(db, logger, webhookHTTPTimeout, cfg.WebhookMaxAttempts)
	licenseSvc := service.NewLicenseService(db, cfg.LicenseSigningKey, logger, bf, webhookSvc)
	usageSvc := service.NewUsageService(db, webhookSvc, logger, cfg.QuotaWarningThreshold)
	seatSvc := service.NewSeatService(db, webhookSvc, logger)
	entitlementSvc := service.NewEntitlementService(db, logger)
	floatingSvc := service.NewFloatingService(db, logger)
	emailSvc := service.NewEmailService(cfg.SMTPHost, cfg.SMTPPort, cfg.SMTPUsername, cfg.SMTPPassword, cfg.SMTPFrom, logger, db)

	licenseH := handler.NewLicenseHandler(licenseSvc)
	oauthH := &handler.OAuthHandler{Store: db, Config: cfg, Registry: oauthReg, Email: emailSvc}
	stripeH := &payment.StripeHandler{Store: db, WebhookSecret: cfg.StripeWebhookSecret, BaseURL: cfg.BaseURL, Email: emailSvc, WebhookSvc: webhookSvc}
	paypalH := &payment.PayPalHandler{
		Store: db, ClientID: cfg.PayPalClientID, ClientSecret: cfg.PayPalClientSecret,
		WebhookID: cfg.PayPalWebhookID, Sandbox: cfg.PayPalSandbox, BaseURL: cfg.BaseURL, Email: emailSvc, WebhookSvc: webhookSvc,
	}
	adminH := handler.NewAdminHandler(db, webhookSvc)
	if cfg.PayPalClientID != "" {
		adminH.PayPalCancel = paypalH
	}
	usageH := handler.NewUsageHandler(usageSvc)
	seatH := handler.NewSeatHandler(seatSvc)
	entitlementH := handler.NewEntitlementHandler(entitlementSvc)
	floatingH := handler.NewFloatingHandler(floatingSvc)
	webhookAdminH := handler.NewWebhookAdminHandler(db, webhookSvc)
	systemH := handler.NewSystemHandler(db)

	// Sync ADMIN_EMAILS to database roles (backward compatibility / initial setup)
	if len(cfg.AdminEmails) > 0 {
		if err := db.SyncAdminEmails(context.Background(), cfg.AdminEmails); err != nil {
			logger.Error("sync admin emails failed", "error", err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	go webhookSvc.StartRetryLoop(ctx, webhookRetryInterval)

	go floatingSvc.StartCleanupLoop(ctx, time.Minute)

	expiryChecker := service.NewExpiryChecker(db, emailSvc, webhookSvc, logger)
	go expiryChecker.StartExpiryLoop(ctx)

	go emailSvc.StartEmailQueueProcessor(ctx, db)

	go systemH.StartAutoCheck(ctx.Done())

	go func() {
		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := db.TakeAllSnapshots(context.Background(), time.Now().Add(-24*time.Hour)); err != nil {
					logger.Error("analytics snapshot failed", "error", err)
				}
			}
		}
	}()

	if cfg.IsProduction() {
		gin.SetMode(gin.ReleaseMode)
	}
	r := gin.Default()
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
					c.AbortWithStatus(http.StatusForbidden)
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

	r.GET("/metrics", gin.WrapH(promhttp.Handler()))

	r.GET("/health", func(c *gin.Context) {
		status := "ok"
		checks := gin.H{}

		// DB check
		if err := db.DB.PingContext(c.Request.Context()); err != nil {
			status = "degraded"
			checks["database"] = "error: " + err.Error()
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
	r.GET("/docs", func(c *gin.Context) {
		c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(`<!DOCTYPE html>
<html><head>
<title>Keygate API</title>
<meta charset="utf-8"/>
<meta name="viewport" content="width=device-width,initial-scale=1"/>
</head><body>
<script id="api-reference" data-url="/docs/openapi.yaml" data-configuration='{"hideTryIt":true}'></script>
<script src="https://cdn.jsdelivr.net/npm/@scalar/api-reference"></script>
</body></html>`))
	})
	r.StaticFile("/docs/openapi.yaml", "docs/openapi.yaml")

	v1 := r.Group("/api/v1")

	v1.GET("/version", systemH.GetVersion)

	setupH := handler.NewSetupHandler(db)
	v1.GET("/setup/status", setupH.Status)
	v1.POST("/setup/initialize", setupH.Initialize)

	// Public site config (no auth — used by login page, branding)
	v1.GET("/config", func(c *gin.Context) {
		settings, _ := db.GetPublicSettings(c)
		if settings == nil {
			settings = make(map[string]string)
		}
		// Attribution: AGPL v3 Section 7(b) — see NOTICE
		settings["attribution_text"] = branding.Tagline
		settings["attribution_url"] = branding.URL
		response.OK(c, settings)
	})

	lic := v1.Group("/license", middleware.LicenseBruteForceGuard(bf), middleware.APIKeyAuth(db), middleware.RateLimit(cfg.RateLimitAPI, time.Minute))
	{
		lic.POST("/activate", licenseH.Activate)
		lic.POST("/verify", licenseH.Verify)
		lic.POST("/deactivate", licenseH.Deactivate)
		lic.POST("/entitlements", entitlementH.Check)
		lic.POST("/usage", usageH.RecordUsage)
		lic.POST("/usage/status", usageH.GetQuotaStatus)
		lic.POST("/seats", seatH.ListSeats)
		lic.POST("/seats/add", seatH.AddSeat)
		lic.POST("/seats/remove", seatH.RemoveSeat)
		lic.POST("/floating/checkout", floatingH.CheckOut)
		lic.POST("/floating/checkin", floatingH.CheckIn)
		lic.POST("/floating/heartbeat", floatingH.Heartbeat)
	}

	auth := v1.Group("/auth", middleware.RateLimitByIP(20, time.Minute))
	{
		auth.GET("/providers", oauthH.Providers)
		auth.POST("/dev-login", oauthH.DevLogin)
		auth.GET("/:provider", oauthH.Redirect)
		auth.GET("/:provider/callback", oauthH.Callback)
		auth.POST("/logout", middleware.SessionAuth(cfg.JWTSecret, db.FindUserIsAdmin), oauthH.Logout)
		auth.POST("/refresh", oauthH.Refresh)
	}

	v1.POST("/webhook/stripe", middleware.RateLimitByIP(60, time.Minute), stripeH.Webhook)
	v1.POST("/webhook/paypal", middleware.RateLimitByIP(60, time.Minute), paypalH.Webhook)
	v1.POST("/checkout/stripe", stripeH.CreateCheckoutSession)
	v1.POST("/subscription/change-plan", middleware.SessionAuth(cfg.JWTSecret, db.FindUserIsAdmin), stripeH.ChangePlan)
	v1.POST("/subscription/cancel", middleware.SessionAuth(cfg.JWTSecret, db.FindUserIsAdmin), stripeH.CancelSubscription)
	v1.POST("/subscription/billing-portal", middleware.SessionAuth(cfg.JWTSecret, db.FindUserIsAdmin), stripeH.CreatePortalSession)
	v1.GET("/subscription/invoices", middleware.SessionAuth(cfg.JWTSecret, db.FindUserIsAdmin), stripeH.ListInvoices)
	v1.POST("/checkout/paypal", paypalH.CreateSubscription)
	v1.POST("/subscription/cancel-paypal", middleware.SessionAuth(cfg.JWTSecret, db.FindUserIsAdmin), paypalH.CancelSubscription)

	portal := v1.Group("/portal", middleware.SessionAuth(cfg.JWTSecret, db.FindUserIsAdmin))
	{
		portal.GET("/me", oauthH.Me)
		portal.GET("/licenses", func(c *gin.Context) {
			emailVal, _ := c.Get("email")
			emailStr, ok := emailVal.(string)
			if !ok || emailStr == "" {
				response.Unauthorized(c, "unauthorized")
				return
			}
			licenses, err := db.ListLicensesByEmail(c, emailStr)
			if err != nil {
				response.Internal(c)
				return
			}
			response.OK(c, gin.H{"licenses": licenses})
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

			// Sanitize: trim whitespace, strip HTML tags, limit length
			name := strings.TrimSpace(req.Name)
			// Strip HTML tags to prevent XSS
			name = stripHTMLTags(name)
			if len(name) > 100 {
				response.BadRequest(c, "name too long (max 100 characters)")
				return
			}

			if err := db.UpdateUserProfile(c, uid, name); err != nil {
				response.Internal(c)
				return
			}

			user, err := db.FindUserByID(c, uid)
			if err != nil {
				response.Internal(c)
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
			plans, err := db.ListPlans(c, productID, "")
			if err != nil {
				response.Internal(c)
				return
			}
			// Only return active plans with public info
			var active []gin.H
			for _, p := range plans {
				if p.Active {
					active = append(active, gin.H{
						"id": p.ID, "name": p.Name, "slug": p.Slug,
						"license_type": p.LicenseType, "billing_interval": p.BillingInterval,
						"stripe_price_id": p.StripePriceID, "paypal_plan_id": p.PayPalPlanID,
					})
				}
			}
			response.OK(c, gin.H{"plans": active})
		})
		// Portal license operations — verify the license belongs to the logged-in user
		portalLicenseGuard := func(c *gin.Context) {
			// Read request body to extract license_key, then restore it
			body, _ := io.ReadAll(c.Request.Body)
			c.Request.Body = io.NopCloser(bytes.NewBuffer(body))
			var req struct {
				LicenseKey string `json:"license_key"`
			}
			if json.Unmarshal(body, &req) != nil || req.LicenseKey == "" {
				c.Next()
				return
			}
			email, _ := c.Get("email")
			emailStr, _ := email.(string)
			if emailStr == "" {
				response.Unauthorized(c, "unauthorized")
				c.Abort()
				return
			}
			// Check license belongs to user (by email or seat)
			lic, err := db.FindLicenseByKey(c, req.LicenseKey)
			if err != nil {
				c.Next() // let handler return proper 404
				return
			}
			if lic.Email != emailStr {
				// Check if user has a seat on this license
				if _, err := db.FindSeatByEmail(c, lic.ID, emailStr); err != nil {
					response.Forbidden(c, "this license does not belong to you")
					c.Abort()
					return
				}
			}
			c.Next()
		}
		// Seat mutation guard — only license owner or seat admin/owner can add/remove seats
		portalSeatMutationGuard := func(c *gin.Context) {
			body, _ := io.ReadAll(c.Request.Body)
			c.Request.Body = io.NopCloser(bytes.NewBuffer(body))
			var req struct {
				LicenseKey string `json:"license_key"`
			}
			if json.Unmarshal(body, &req) != nil || req.LicenseKey == "" {
				c.Next()
				return
			}
			email, _ := c.Get("email")
			emailStr, _ := email.(string)
			lic, err := db.FindLicenseByKey(c, req.LicenseKey)
			if err != nil {
				c.Next()
				return
			}
			// License owner can always manage seats
			if lic.Email == emailStr {
				c.Next()
				return
			}
			// Seat-based access: only owner/admin role can mutate
			seat, err := db.FindSeatByEmail(c, lic.ID, emailStr)
			if err != nil || (seat.Role != "owner" && seat.Role != "admin") {
				response.Forbidden(c, "only license owner or admin can manage seats")
				c.Abort()
				return
			}
			c.Next()
		}
		portal.POST("/usage", portalLicenseGuard, usageH.RecordUsage)
		portal.POST("/usage/status", portalLicenseGuard, usageH.GetQuotaStatus)
		portal.POST("/seats", portalLicenseGuard, seatH.ListSeats)
		portal.POST("/seats/add", portalSeatMutationGuard, seatH.AddSeat)
		portal.POST("/seats/remove", portalSeatMutationGuard, seatH.RemoveSeat)
	}

	admin := v1.Group("/admin", middleware.SessionAuth(cfg.JWTSecret, db.FindUserIsAdmin), middleware.AdminOnly(), middleware.RateLimitByIP(cfg.RateLimitAdmin, time.Minute))
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
		admin.DELETE("/plans/:id", adminH.DeletePlan)

		admin.POST("/entitlements", adminH.CreateEntitlement)
		admin.PUT("/entitlements/:id", adminH.UpdateEntitlement)
		admin.DELETE("/entitlements/:id", adminH.DeleteEntitlement)

		admin.GET("/licenses", adminH.ListLicenses)
		admin.GET("/licenses/export", adminH.ExportLicenses)
		admin.GET("/licenses/:id", adminH.GetLicense)
		admin.POST("/licenses", adminH.CreateLicense)
		admin.POST("/licenses/:id/refund", adminH.RefundLicense)
		admin.POST("/licenses/:id/revoke", adminH.RevokeLicense)
		admin.POST("/licenses/:id/suspend", adminH.SuspendLicense)
		admin.POST("/licenses/:id/reinstate", adminH.ReinstateLicense)
		admin.POST("/licenses/:id/change-plan", adminH.ChangeLicensePlan)
		admin.GET("/licenses/:id/usage", adminH.ListLicenseUsage)
		admin.POST("/licenses/:id/usage/reset", adminH.ResetLicenseUsage)
		admin.GET("/licenses/:id/seats", adminH.ListLicenseSeats)

		admin.GET("/licenses/:id/addons", adminH.ListLicenseAddons)
		admin.POST("/licenses/:id/addons", adminH.AddLicenseAddon)
		admin.DELETE("/licenses/:id/addons/:addon_id", adminH.RemoveLicenseAddon)
		admin.GET("/licenses/:id/floating", adminH.ListFloatingSessions)

		admin.DELETE("/activations/:id", adminH.DeleteActivation)

		admin.GET("/api-keys", adminH.ListAPIKeys)
		admin.POST("/api-keys", adminH.CreateAPIKey)
		admin.DELETE("/api-keys/:id", adminH.DeleteAPIKey)

		admin.GET("/webhooks", webhookAdminH.ListWebhooks)
		admin.POST("/webhooks", webhookAdminH.CreateWebhook)
		admin.PUT("/webhooks/:id", webhookAdminH.UpdateWebhook)
		admin.DELETE("/webhooks/:id", webhookAdminH.DeleteWebhook)
		admin.GET("/webhooks/:id/deliveries", webhookAdminH.ListDeliveries)
		admin.POST("/webhooks/:id/test", webhookAdminH.TestWebhook)

		admin.GET("/addons", adminH.ListAddons)
		admin.POST("/addons", adminH.CreateAddon)
		admin.PUT("/addons/:id", adminH.UpdateAddon)
		admin.DELETE("/addons/:id", adminH.DeleteAddon)

		admin.GET("/settings", adminH.GetSettings)
		admin.PUT("/settings", adminH.UpdateSettings)
		admin.POST("/settings/test-email", adminH.SendTestEmail)
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
	}

	serveFrontend(r)

	srv := &http.Server{
		Addr:    ":" + cfg.Port,
		Handler: r,
	}

	go func() {
		log.Printf("Keygate starting on :%s", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("Shutting down server...")

	cancel() // cancel background context — stops all goroutines

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutdownCancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Fatalf("server forced shutdown: %v", err)
	}
	log.Println("Server exited gracefully")
}

// stripHTMLTags removes all HTML tags from a string to prevent XSS.
func stripHTMLTags(s string) string {
	var result strings.Builder
	inTag := false
	for _, r := range s {
		if r == '<' {
			inTag = true
			continue
		}
		if r == '>' {
			inTag = false
			continue
		}
		if !inTag {
			result.WriteRune(r)
		}
	}
	return result.String()
}

// serveFrontend serves the React SPA from web/dist if it exists.
func serveFrontend(r *gin.Engine) {
	distPath := "web/dist"
	if _, err := os.Stat(distPath); os.IsNotExist(err) {
		return
	}

	indexHTML, err := os.ReadFile(distPath + "/index.html")
	if err != nil {
		log.Printf("WARNING: web/dist exists but index.html not found: %v", err)
		return
	}

	r.Use(func(c *gin.Context) {
		path := c.Request.URL.Path

		// Let backend routes pass through.
		if strings.HasPrefix(path, "/api/") || path == "/health" || path == "/metrics" || strings.HasPrefix(path, "/docs") {
			c.Next()
			return
		}

		// Try to serve a static file using path.Clean to prevent traversal.
		// Uses c.File() instead of c.FileFromFS() to avoid http.FileServer's
		// implicit 301 redirects (e.g. /index.html → /).
		if clean := filepath.Clean(path); clean != "/" && clean != "/index.html" {
			filePath := filepath.Join(distPath, clean)
			if info, err := os.Stat(filePath); err == nil && !info.IsDir() {
				c.File(filePath)
				c.Abort()
				return
			}
		}

		// SPA fallback: serve cached index.html for all other routes.
		c.Data(http.StatusOK, "text/html; charset=utf-8", indexHTML)
		c.Abort()
	})
}
