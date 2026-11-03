package payment

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/stripe/stripe-go/v82"
	"github.com/stripe/stripe-go/v82/webhookendpoint"
)

const (
	settingWebhookEndpointID = "stripe_webhook_endpoint_id"
	settingWebhookSecret     = "stripe_webhook_secret"
	// A replaced endpoint keeps delivering its queued events with the
	// old secret; these hold that secret and the endpoint to delete
	// once the drain period is over.
	settingWebhookSecretPrevious      = "stripe_webhook_secret_previous"
	settingWebhookSecretPreviousUntil = "stripe_webhook_secret_previous_until"
	settingWebhookRetireEndpointID    = "stripe_webhook_retire_endpoint_id"
)

// webhookDrainPeriod covers Stripe's retry schedule (up to three
// days) so nothing queued for a replaced endpoint is lost.
const webhookDrainPeriod = 72 * time.Hour

var stripeWebhookEvents = []*string{
	stripe.String("checkout.session.completed"),
	stripe.String("checkout.session.async_payment_succeeded"),
	stripe.String("checkout.session.async_payment_failed"),
	stripe.String("invoice.paid"),
	stripe.String("invoice.payment_failed"),
	stripe.String("customer.subscription.deleted"),
	stripe.String("customer.subscription.updated"),
	stripe.String("customer.subscription.pending_update_applied"),
	stripe.String("customer.subscription.pending_update_expired"),
	stripe.String("charge.refunded"),
	stripe.String("charge.dispute.created"),
	stripe.String("charge.dispute.closed"),
	stripe.String("invoice.payment_action_required"),
	stripe.String("customer.subscription.paused"),
	stripe.String("customer.subscription.resumed"),
	stripe.String("customer.subscription.trial_will_end"),
	stripe.String("invoice.upcoming"),
	stripe.String("customer.updated"),
}

// IsLocalhostURL returns true if the URL's host is a local address.
// Stripe cannot deliver webhooks to localhost. Only the host counts: a
// public domain such as localhost-licenses.example.com, or "localhost"
// in the path or query, is not local.
func IsLocalhostURL(baseURL string) bool {
	u, err := url.Parse(baseURL)
	if err != nil {
		return false
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsLoopback() || ip.IsUnspecified())
}

// SetupWebhookEndpoint ensures a Stripe webhook endpoint is configured.
// It runs synchronously on first attempt, then retries in the background if it fails.
func (h *StripeHandler) SetupWebhookEndpoint(ctx context.Context) {
	if err := h.ensureWebhookEndpoint(ctx); err != nil {
		slog.Error("stripe webhook auto-setup failed, will retry every 60s", "error", err)
		go h.retryWebhookSetup(ctx)
		return
	}
	h.RetireReplacedEndpoint(ctx)
}

// webhookSetupLockKey serialises endpoint verification/replacement
// across replicas sharing one database: check, create and persist run
// as one critical section, so two starting replicas cannot both
// create a replacement and leave one enabled with a secret nobody
// stores.
const webhookSetupLockKey = 7367617

func (h *StripeHandler) ensureWebhookEndpoint(ctx context.Context) error {
	return h.Store.WithAdvisoryLock(ctx, webhookSetupLockKey, h.ensureWebhookEndpointLocked)
}

func (h *StripeHandler) ensureWebhookEndpointLocked(ctx context.Context) error {
	webhookURL := strings.TrimRight(h.BaseURL, "/") + "/api/v1/webhook/stripe"
	h.loadRotationState(ctx)

	// Check database for existing endpoint
	endpointID, _ := h.Store.GetSetting(ctx, settingWebhookEndpointID)
	secret, _ := h.Store.GetSetting(ctx, settingWebhookSecret)

	// retire is an endpoint to delete once its replacement is live.
	retire := ""
	if endpointID != "" && secret != "" {
		// Verify the endpoint still exists in Stripe
		ep, err := webhookendpoint.Get(endpointID, nil)
		if err == nil && !ep.Deleted && ep.Status == "enabled" && !sameReleaseTrain(ep.APIVersion, stripe.APIVersion) {
			// api_version is fixed at creation. An endpoint on the
			// account's old default version delivers payload shapes
			// this build does not target; replace it with one pinned
			// to the SDK version. The old one keeps delivering until
			// the replacement is created and its secret persisted.
			slog.Warn("stripe webhook endpoint uses another API version, replacing",
				"endpoint_id", endpointID, "endpoint_version", ep.APIVersion, "sdk_version", stripe.APIVersion)
			retire = endpointID
			err = fmt.Errorf("api version %q differs from sdk %q", ep.APIVersion, stripe.APIVersion)
		}
		if err == nil && !ep.Deleted && ep.Status == "enabled" {
			if ep.URL == webhookURL && hasAllEvents(ep.EnabledEvents, stripeWebhookEvents) {
				h.SetWebhookSecret(secret)
				slog.Info("stripe webhook endpoint verified", "endpoint_id", endpointID)
				return nil
			}
			// URL changed (BASE_URL changed) or this build listens to
			// events the endpoint was created without — update it.
			_, err := webhookendpoint.Update(endpointID, &stripe.WebhookEndpointParams{
				URL:           stripe.String(webhookURL),
				EnabledEvents: stripeWebhookEvents,
			})
			if err == nil {
				h.SetWebhookSecret(secret)
				slog.Info("stripe webhook endpoint updated", "endpoint_id", endpointID, "url", webhookURL)
				return nil
			}
			slog.Warn("stripe webhook endpoint update failed, will recreate", "error", err)
		}
		slog.Warn("stripe webhook endpoint not found or disabled, creating new", "old_endpoint_id", endpointID)
	}

	// Create new webhook endpoint
	// Pin the payload shape to the API version this build parses.
	// Endpoints created earlier keep their version; the handlers read
	// both the pre- and post-2025-03-31 shapes.
	ep, err := webhookendpoint.New(&stripe.WebhookEndpointParams{
		URL:           stripe.String(webhookURL),
		EnabledEvents: stripeWebhookEvents,
		APIVersion:    stripe.String(stripe.APIVersion),
		Description:   stripe.String("Keygate auto-managed webhook"),
		Metadata:      map[string]string{"managed_by": "keygate"},
	})
	if err != nil {
		return fmt.Errorf("create stripe webhook endpoint: %w", err)
	}

	// Stripe only returns Secret at creation time. The new credentials
	// and, on a replacement, the old endpoint's drain state are one
	// transaction: a crash can leave either the old endpoint fully in
	// charge or the cutover complete, never a new secret without the
	// old one that its queued deliveries are signed with.
	cutover := map[string]string{
		settingWebhookEndpointID: ep.ID,
		settingWebhookSecret:     ep.Secret,
	}
	var until time.Time
	if retire != "" {
		// Keep the old endpoint and its secret alive for the drain
		// period: deliveries already queued for it (refunds, subscription
		// changes) keep arriving signed with the old secret and are
		// deduplicated by event id. RetireReplacedEndpoint finishes the
		// job once the period is over.
		until = time.Now().Add(webhookDrainPeriod)
		cutover[settingWebhookSecretPrevious] = secret
		cutover[settingWebhookSecretPreviousUntil] = until.Format(time.RFC3339)
		cutover[settingWebhookRetireEndpointID] = retire
	}
	if err := h.Store.SetSettings(ctx, cutover); err != nil {
		// The new endpoint exists but nothing points at it; the old
		// one (if any) keeps working. Remove the orphan so the retry
		// does not accumulate endpoints.
		if _, derr := webhookendpoint.Del(ep.ID, nil); derr != nil {
			slog.Warn("stripe webhook: failed to remove orphan endpoint", "endpoint_id", ep.ID, "error", derr)
		}
		return fmt.Errorf("save webhook settings: %w", err)
	}

	if retire != "" {
		h.SetPreviousWebhookSecret(secret, until)
	}
	h.SetWebhookSecret(ep.Secret)
	slog.Info("stripe webhook endpoint created", "endpoint_id", ep.ID, "url", webhookURL)
	if retire != "" {
		slog.Info("stripe webhook endpoint replaced; old endpoint drains", "old_endpoint_id", retire, "until", until.Format(time.RFC3339))
	}
	return nil
}

// loadRotationState restores the previous secret after a restart
// while a replaced endpoint is still draining.
func (h *StripeHandler) loadRotationState(ctx context.Context) {
	prev, _ := h.Store.GetSetting(ctx, settingWebhookSecretPrevious)
	untilRaw, _ := h.Store.GetSetting(ctx, settingWebhookSecretPreviousUntil)
	if prev == "" || untilRaw == "" {
		return
	}
	if until, err := time.Parse(time.RFC3339, untilRaw); err == nil && time.Now().Before(until) {
		h.SetPreviousWebhookSecret(prev, until)
	}
}

// RetireReplacedEndpoint deletes a replaced endpoint and forgets its
// secret once the drain period has passed. Cheap when nothing is
// pending; called at startup and from the periodic sync.
func (h *StripeHandler) RetireReplacedEndpoint(ctx context.Context) {
	retire, _ := h.Store.GetSetting(ctx, settingWebhookRetireEndpointID)
	untilRaw, _ := h.Store.GetSetting(ctx, settingWebhookSecretPreviousUntil)
	if retire == "" && untilRaw == "" {
		return
	}
	if until, err := time.Parse(time.RFC3339, untilRaw); err == nil && time.Now().Before(until) {
		return
	}
	if retire != "" {
		if _, err := webhookendpoint.Del(retire, nil); err != nil && !stripeNotFound(err) {
			slog.Warn("stripe webhook endpoint retire failed, will retry", "endpoint_id", retire, "error", err)
			return
		}
		slog.Info("stripe webhook endpoint retired", "endpoint_id", retire)
	}
	for _, k := range []string{settingWebhookRetireEndpointID, settingWebhookSecretPrevious, settingWebhookSecretPreviousUntil} {
		_ = h.Store.DeleteSetting(ctx, k)
	}
	h.SetPreviousWebhookSecret("", time.Time{})
}

func (h *StripeHandler) retryWebhookSetup(ctx context.Context) {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := h.ensureWebhookEndpoint(ctx); err != nil {
				slog.Error("stripe webhook auto-setup retry failed", "error", err)
				continue
			}
			slog.Info("stripe webhook auto-setup succeeded on retry")
			return
		}
	}
}

// hasAllEvents reports whether every wanted event is enabled. An
// endpoint subscribed to "*" receives everything.
func hasAllEvents(enabled []string, wanted []*string) bool {
	have := make(map[string]bool, len(enabled))
	for _, e := range enabled {
		if e == "*" {
			return true
		}
		have[e] = true
	}
	for _, w := range wanted {
		if !have[*w] {
			return false
		}
	}
	return true
}

// sameReleaseTrain reports whether events rendered at version `have`
// carry the shapes this build targets (`want`, the SDK's pinned
// version). Versions are "yyyy-MM-dd" (pre-2025, no train) or
// "yyyy-MM-dd.train"; the same train means compatible shapes.
func sameReleaseTrain(have, want string) bool {
	hi, wi := strings.Index(have, "."), strings.Index(want, ".")
	if hi < 0 || wi < 0 {
		return false
	}
	return have[hi+1:] == want[wi+1:]
}
