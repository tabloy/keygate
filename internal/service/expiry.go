package service

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"time"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
)

type ExpiryChecker struct {
	store   *store.Store
	email   *EmailService
	webhook *WebhookService
	logger  *slog.Logger
	// EndPastDueUpgrade, when set, is asked first about a licence the
	// dunning sweep would expire. It handles one an upgrade moved onto
	// the subscription (back to its free plan rather than expired) and
	// reports whether it did. On an error the licence is left past due,
	// so the next sweep tries again.
	EndPastDueUpgrade func(ctx context.Context, lic *model.License, threshold time.Time) (bool, error)
	// EndGraceExpiredUpgrade is the same for the grace sweep, which runs
	// first and would otherwise expire an upgraded licence on its paid
	// plan, out of the dunning sweep's reach.
	EndGraceExpiredUpgrade func(ctx context.Context, lic *model.License, cutoff time.Time) (bool, error)
}

func NewExpiryChecker(s *store.Store, email *EmailService, wh *WebhookService, logger *slog.Logger) *ExpiryChecker {
	return &ExpiryChecker{store: s, email: email, webhook: wh, logger: logger}
}

// StartExpiryLoop runs all lifecycle checks periodically.
func (c *ExpiryChecker) StartExpiryLoop(ctx context.Context) {
	// Run immediately on startup
	c.RunAll(ctx)

	ticker := time.NewTicker(1 * time.Hour) // check every hour, not daily
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.RunAll(ctx)
		}
	}
}

// RunAll executes all lifecycle checks.
func (c *ExpiryChecker) RunAll(ctx context.Context) {
	c.ExpireGracePeriodLicenses(ctx)
	c.ExpireTrials(ctx)
	c.MarkPastDueAsExpired(ctx)
	c.SendExpiryReminders(ctx)
	c.SendUpdatesEndingReminders(ctx)
	c.SendRenewalReminders(ctx)
	c.SendPaymentFailureReminders(ctx)
	c.CleanupExpiredActivations(ctx)
	c.SyncSubscriptionStates(ctx)
}

// ExpireGracePeriodLicenses marks active/past_due licenses as expired
// when valid_until + grace_days has passed.
func (c *ExpiryChecker) ExpireGracePeriodLicenses(ctx context.Context) {
	licenses, err := c.store.FindLicensesForGraceExpiry(ctx)
	if err != nil {
		c.logger.Error("grace expiry check failed", "error", err)
		return
	}
	for _, lic := range licenses {
		graceDays := 7
		if lic.Plan != nil {
			graceDays = lic.Plan.GraceDays
		}
		grace := time.Duration(graceDays) * 24 * time.Hour
		if time.Now().After(lic.ValidUntil.Add(grace)) {
			// Re-checked in the write itself: an admin may have
			// extended or reinstated the licence since it was read.
			cutoff := time.Now().Add(-grace)
			if c.EndGraceExpiredUpgrade != nil {
				handled, err := c.EndGraceExpiredUpgrade(ctx, lic, cutoff)
				if err != nil {
					c.logger.Error("grace upgrade end failed; retried next sweep", "id", lic.ID, "error", err)
					continue
				}
				if handled {
					continue
				}
			}
			ok, err := c.store.ExpireLicenseIf(ctx, lic.ID,
				[]string{model.StatusActive, model.StatusPastDue}, &cutoff)
			if err != nil {
				c.logger.Error("expire license failed", "id", lic.ID, "error", err)
				continue
			}
			if !ok {
				continue
			}
			lic.Status = model.StatusExpired
			c.store.Audit(ctx, &model.AuditLog{
				Entity: "license", EntityID: lic.ID, Action: "expired",
				ActorType: "system",
				Changes:   map[string]any{"reason": "grace_period_ended"},
			})
			c.webhook.Dispatch(ctx, lic.ProductID, "license.expired", map[string]any{
				"license_id": lic.ID, "email": lic.Email, "reason": "grace_period_ended",
			})
			productName := ""
			if lic.Product != nil {
				productName = lic.Product.Name
			}
			c.email.SendLicenseExpired(lic.Email, productName)
			c.logger.Info("license expired (grace ended)", "id", lic.ID)
		}
	}
}

// ExpireTrials marks trialing licenses as expired when trial period ends.
func (c *ExpiryChecker) ExpireTrials(ctx context.Context) {
	licenses, err := c.store.FindExpiredTrials(ctx)
	if err != nil {
		c.logger.Error("trial expiry check failed", "error", err)
		return
	}
	for _, lic := range licenses {
		now := time.Now()
		ok, err := c.store.ExpireLicenseIf(ctx, lic.ID, []string{model.StatusTrialing}, &now)
		if err != nil {
			c.logger.Error("expire trial failed", "id", lic.ID, "error", err)
			continue
		}
		if !ok {
			continue // extended or changed since it was read
		}
		lic.Status = model.StatusExpired
		c.store.Audit(ctx, &model.AuditLog{
			Entity: "license", EntityID: lic.ID, Action: "expired",
			ActorType: "system",
			Changes:   map[string]any{"reason": "trial_ended"},
		})
		c.webhook.Dispatch(ctx, lic.ProductID, "license.expired", map[string]any{
			"license_id": lic.ID, "email": lic.Email, "reason": "trial_ended",
		})
		productName := ""
		if lic.Product != nil {
			productName = lic.Product.Name
		}
		c.email.SendTrialExpired(lic.Email, productName)
		c.logger.Info("trial expired", "id", lic.ID)
	}
}

// MarkPastDueAsExpired converts long-standing past_due licenses to expired.
// If past_due for more than 30 days, mark as expired.
func (c *ExpiryChecker) MarkPastDueAsExpired(ctx context.Context) {
	threshold := time.Now().Add(-30 * 24 * time.Hour)
	licenses, err := c.store.FindStalePastDueLicenses(ctx, threshold)
	if err != nil {
		c.logger.Error("past_due expiry check failed", "error", err)
		return
	}
	for _, lic := range licenses {
		if c.EndPastDueUpgrade != nil {
			handled, err := c.EndPastDueUpgrade(ctx, lic, threshold)
			if err != nil {
				c.logger.Error("past_due upgrade end failed; retried next sweep", "id", lic.ID, "error", err)
				continue
			}
			if handled {
				continue
			}
		}
		ok, err := c.store.ExpireStalePastDueLicenseIf(ctx, lic.ID, threshold)
		if err != nil {
			c.logger.Error("expire past_due failed", "id", lic.ID, "error", err)
			continue
		}
		if !ok {
			continue // paid, reinstated or changed since it was read
		}
		lic.Status = model.StatusExpired
		c.store.Audit(ctx, &model.AuditLog{
			Entity: "license", EntityID: lic.ID, Action: "expired",
			ActorType: "system",
			Changes:   map[string]any{"reason": "past_due_timeout"},
		})
		c.webhook.Dispatch(ctx, lic.ProductID, model.EventLicenseExpired, map[string]any{
			"license_id": lic.ID, "email": lic.Email, "reason": "past_due_timeout",
		})
		c.logger.Info("past_due expired", "id", lic.ID)
	}
}

// SendUpdatesEndingReminders tells holders of perpetual licenses that
// their maintenance period ends within 14 days, once per period end.
// Only plans that sell renewals get the mail: without one there is
// nothing the customer can do about it, and the license keeps
// working either way.
func (c *ExpiryChecker) SendUpdatesEndingReminders(ctx context.Context) {
	// Turned off: skip without recording anything, so switching it back
	// on reminds licenses still inside the window.
	if !c.email.NotifyEnabled("updates_ending") {
		return
	}
	from := time.Now()
	to := from.Add(14 * 24 * time.Hour)
	licenses, err := c.store.FindLicensesWithUpdatesEnding(ctx, from, to)
	if err != nil {
		c.logger.Error("updates ending reminder check failed", "error", err)
		return
	}
	for _, lic := range licenses {
		if lic.Plan == nil || !lic.Plan.OffersRenewal() {
			continue
		}
		// A renewal moves updates_until, so the tag carries the date:
		// the next period end gets its own reminder.
		// Claim before sending: every replica runs this loop, and the
		// unique (license, tag) row is what keeps one mail per period.
		tag := "updates_14d_" + lic.UpdatesUntil.Format("2006-01-02")
		token, err := c.store.ClaimNotification(ctx, lic.ID, tag)
		if err != nil {
			c.logger.Error("updates ending reminder claim failed", "license_id", lic.ID, "error", err)
			continue
		}
		if token == "" {
			continue
		}
		productName := ""
		if lic.Product != nil {
			productName = lic.Product.Name
		}
		// Queued, not sent: this loop must not wait on SMTP, and the
		// queue retries on its own. Queuing the mail and closing the
		// lease share one transaction, so a crash cannot leave a
		// queued mail whose lease later expires and queues a second
		// copy. Failing to queue hands the claim back for the next
		// run; losing the claim means another pass already owns it.
		subject, body := c.email.RenderUpdatesEnding(productName, c.store.DecryptLicenseKey(lic), lic.UpdatesUntil.UTC().Format("2006-01-02"))
		switch err := c.store.EnqueueEmailAndCloseNotification(ctx, lic.Email, subject, body, token); {
		case err == nil:
			c.logger.Info("updates ending reminder queued", "license_id", lic.ID)
		case errors.Is(err, store.ErrNotificationClaimLost):
			c.logger.Warn("updates ending reminder claim taken over; the other pass sends it", "license_id", lic.ID)
		default:
			c.logger.Error("updates ending reminder could not be queued", "license_id", lic.ID, "error", err)
			if rerr := c.store.ReleaseNotification(ctx, token); rerr != nil {
				c.logger.Error("updates ending reminder claim not released", "license_id", lic.ID, "error", rerr)
			}
		}
	}
}

// CleanupExpiredActivations removes activations for expired/revoked licenses.
func (c *ExpiryChecker) CleanupExpiredActivations(ctx context.Context) {
	count, err := c.store.DeleteExpiredActivations(ctx)
	if err != nil {
		c.logger.Error("cleanup activations failed", "error", err)
		return
	}
	if count > 0 {
		c.logger.Info("cleaned up expired activations", "count", count)
	}
}

// SendPaymentFailureReminders walks every past_due license up the
// dunning ladder. Anchored on lic.PastDueAt so unrelated row writes
// (audit, sync) don't reset the clock. Each step is gated by
// HasNotification + a unique tag per past_due_at epoch, so:
//
//   - the same email never fires twice for one episode, even if the
//     checker runs every hour;
//   - if the checker misses a window (server down) it still fires
//     the next time it wakes up — there's no narrow `>=N && <N+1`
//     window that can silently swallow a reminder;
//   - a customer who recovers and then lapses again later gets a
//     full fresh ladder for the second episode (the tag includes
//     past_due_at's epoch).
func (c *ExpiryChecker) SendPaymentFailureReminders(ctx context.Context) {
	// Switched off: send nothing and record nothing. Each step records
	// its tag as sent, so recording one that was skipped would leave the
	// reminder lost for good once the email is switched back on.
	if c.email == nil || !c.email.NotifyEnabled("payment_failed") {
		return
	}
	var licenses []*model.License
	err := c.store.DB.NewSelect().Model(&licenses).
		Relation("Product").
		Where("license.status = 'past_due' AND license.past_due_at IS NOT NULL").
		Scan(ctx)
	if err != nil || len(licenses) == 0 {
		return
	}

	steps := []struct {
		minDays int
		tag     string
		send    func(to, productName string)
	}{
		// Highest threshold first — only one email per check per
		// license, and the "highest reached" wins. Without that,
		// a license that's been past_due for 20 days would receive
		// dunning_first + dunning_second + dunning_final on the
		// FIRST checker run after install.
		{14, "dunning_final", c.email.SendDunningFinal},
		{7, "dunning_second", c.email.SendDunningSecond},
		{1, "dunning_first", c.email.SendPaymentFailed},
	}

	now := time.Now()
	for _, lic := range licenses {
		if lic.PastDueAt == nil {
			continue
		}
		daysPastDue := int(now.Sub(*lic.PastDueAt).Hours() / 24)
		productName := ""
		if lic.Product != nil {
			productName = lic.Product.Name
		}
		// Tag scope: per-episode. Encoding past_due_at's Unix epoch
		// means the next past_due cycle for the same license gets a
		// brand-new tag namespace.
		episode := lic.PastDueAt.Unix()

		for _, step := range steps {
			if daysPastDue < step.minDays {
				continue
			}
			tag := step.tag + ":" + strconvI64(episode)
			if c.store.HasNotification(ctx, lic.ID, tag) {
				break // already sent the highest-tier; nothing lower will fire
			}
			step.send(lic.Email, productName)
			c.store.RecordNotification(ctx, lic.ID, tag)
			c.logger.Info("dunning email sent",
				"license_id", lic.ID, "tag", step.tag,
				"days_past_due", daysPastDue, "episode", episode)
			break // only one email per checker run per license
		}
	}
}

// strconvI64 keeps the small helper local so the expiry file stays
// self-contained. Tagging on epoch ints avoids any timezone / DST
// drift you'd get from a date-string anchor.
func strconvI64(n int64) string {
	return strconv.FormatInt(n, 10)
}

// SendRenewalReminders tells customers, the day before, that their Stripe
// subscription renews. It covers exactly the licenses the expiry reminder
// leaves out — an active Stripe subscription not set to cancel — so no
// license gets both "renews tomorrow" and "expiring" (see
// FindLicensesForRenewalReminder). The wide [now, now+25h] scan with a
// per-date tag means an outage during the narrow window does not skip
// the email, and every renewal is reminded, not only the first. Queued
// through the durable mail queue like the other reminders.
func (c *ExpiryChecker) SendRenewalReminders(ctx context.Context) {
	if !c.email.NotifyEnabled("renewal_reminder") {
		return
	}
	now := time.Now()
	licenses, err := c.store.FindLicensesForRenewalReminder(ctx, now, now.Add(25*time.Hour))
	if err != nil {
		c.logger.Error("renewal reminder check failed", "error", err)
		return
	}
	for _, lic := range licenses {
		if lic.ValidUntil == nil || c.store.RenewalReminderSentLegacy(ctx, lic.ID, *lic.ValidUntil) {
			continue
		}
		token, err := c.store.ClaimNotification(ctx, lic.ID, "renewal_24h:"+lic.ValidUntil.UTC().Format(time.RFC3339))
		if err != nil || token == "" {
			continue
		}
		productName := ""
		if lic.Product != nil {
			productName = lic.Product.Name
		}
		subject, body := c.email.RenderRenewalReminder(productName, lic.ValidUntil.Format("2006-01-02"))
		switch err := c.store.EnqueueEmailAndCloseNotification(ctx, lic.Email, subject, body, token); {
		case err == nil, errors.Is(err, store.ErrNotificationClaimLost):
		default:
			_ = c.store.ReleaseNotification(ctx, token)
			c.logger.Error("renewal reminder enqueue failed", "license_id", lic.ID, "error", err)
		}
	}
}

// SyncSubscriptionStates is a fallback that syncs subscription table with license status.
// This catches cases where Stripe webhooks were missed.
func (c *ExpiryChecker) SyncSubscriptionStates(ctx context.Context) {
	if err := c.store.SyncSubscriptionStatuses(ctx); err != nil {
		c.logger.Error("subscription sync failed", "error", err)
	}
}
