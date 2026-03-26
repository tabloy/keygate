package middleware

import (
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	httpRequestsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "keygate_http_requests_total",
			Help: "Total number of HTTP requests",
		},
		[]string{"method", "path", "status"},
	)

	httpRequestDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "keygate_http_request_duration_seconds",
			Help:    "HTTP request duration in seconds",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"method", "path"},
	)

	httpRequestsInFlight = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "keygate_http_requests_in_flight",
			Help: "Number of HTTP requests currently being processed",
		},
	)

	// Business metrics
	LicenseActivations = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "keygate_license_activations_total",
			Help: "Total license activations",
		},
		[]string{"product_id", "status"}, // status: "activated", "already_activated", "failed"
	)

	LicenseVerifications = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "keygate_license_verifications_total",
			Help: "Total license verifications",
		},
		[]string{"product_id", "result"}, // result: "valid", "expired", "not_found", "not_activated"
	)

	WebhookDeliveries = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "keygate_webhook_deliveries_total",
			Help: "Total webhook delivery attempts",
		},
		[]string{"status"}, // "delivered", "failed", "retrying"
	)

	EmailDeliveries = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "keygate_email_deliveries_total",
			Help: "Total email delivery attempts",
		},
		[]string{"status"}, // "sent", "failed", "queued"
	)

	ActiveLicenses = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "keygate_active_licenses",
			Help: "Current number of active licenses",
		},
	)

	BruteForceBlocks = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "keygate_brute_force_blocks_total",
			Help: "Total brute force lockouts triggered",
		},
	)
)

// PrometheusMetrics is a Gin middleware that records HTTP metrics.
func PrometheusMetrics() gin.HandlerFunc {
	return func(c *gin.Context) {
		httpRequestsInFlight.Inc()
		start := time.Now()

		c.Next()

		duration := time.Since(start).Seconds()
		status := strconv.Itoa(c.Writer.Status())

		// Normalize path to prevent high cardinality
		path := normalizePath(c.FullPath())
		if path == "" {
			path = "unknown"
		}

		httpRequestsTotal.WithLabelValues(c.Request.Method, path, status).Inc()
		httpRequestDuration.WithLabelValues(c.Request.Method, path).Observe(duration)
		httpRequestsInFlight.Dec()
	}
}

// normalizePath reduces cardinality by replacing path params with placeholders.
func normalizePath(path string) string {
	if path == "" {
		return ""
	}
	return path // Gin's FullPath() already returns the template like /api/v1/licenses/:id
}
