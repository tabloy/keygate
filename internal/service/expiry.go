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
	c.FinishSeatRemovals(ctx)
}

// FinishSeatRemovals repairs what an older version leaves when it runs
// beside this one in a rolling upgrade: a member it removed keeps their
// key and devices (it set removed_at alone), so does one it removed and
// invited back (pending again), and a seat it invited may carry the
// admin role this version dropped. The key no longer opens
// anything, but the devices hold slots; they are removed, and reported
// as deactivated, as a removal does. Idempotent, and a no-op once no
// older version runs.
func (c *ExpiryChecker) FinishSeatRemovals(ctx context.Context) {
	if n, err := c.store.NormalizeSeatRoles(ctx); err != nil {
		c.logger.Error("seat role repair failed", "error", err)
	} else if n > 0 {
		c.logger.Info("seat roles repaired: admin seats are members", "count", n)
	}
	leftovers, err := c.store.FindSeatLeftovers(ctx, 500)
	if err != nil {
		c.logger.Error("seat leftover check failed", "error", err)
		return
	}
	for _, seat := range leftovers {
		devices, done, err := c.store.ClearSeatLeftovers(ctx, seat.ID)
		if err != nil {
			c.logger.Error("seat leftover repair failed", "seat_id", seat.ID, "error", err)
			continue
		}
		if !done {
			continue
		}
		for _, a := range devices {
			c.webhook.Dispatch(ctx, seat.ProductID, "license.deactivated", map[string]any{
				"license_id": seat.LicenseID, "identifier": a.Identifier,
			})
		}
		c.store.Audit(ctx, &model.AuditLog{
			Entity: "seat", EntityID: seat.ID, Action: "seat_removal_finished",
			ActorType: "system", ProductID: seat.ProductID,
			Changes: map[string]any{"license_id": seat.LicenseID, "email": seat.Email, "devices_removed": len(devices)},
		})
		c.logger.Info("seat removal finished", "seat_id", seat.ID, "license_id", seat.LicenseID, "devices_removed", len(devices))
	}
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
			// Re-checked in the write itself, against the plan as it
			// is then: an admin may have extended or reinstated the
			// licence, or given it more grace, since it was read.
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
			ok, err := c.store.ExpireLicenseAfterGrace(ctx, lic.ID)
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

// ExpireTrials ends trials whose period is over. A trial plan that
// names a fallback plan (TrialFallbackPlanID) moves the licence onto it
// instead, so the customer keeps working on, say, the free tier with
// the same key; any other trial is marked expired.
func (c *ExpiryChecker) ExpireTrials(ctx context.Context) {
	licenses, err := c.store.FindExpiredTrials(ctx)
	if err != nil {
		c.logger.Error("trial expiry check failed", "error", err)
		return
	}
	for _, lic := range licenses {
		c.endTrial(ctx, lic)
	}
}

// endTrial ends one trial the sweep found: moved to its fallback plan,
// or marked expired. The licence is re-checked in the write, so a stale
// copy is harmless.
func (c *ExpiryChecker) endTrial(ctx context.Context, lic *model.License) {
	// Decided and done under the locks, not on this read: a fallback named
	// since the sweep read the plan is taken, not expired past.
	res, err := c.store.MoveEndedTrialToFallback(ctx, lic.ID, time.Now())
	if err != nil {
		// Left trialing: the next sweep tries again rather than expiring
		// a licence that was meant to move.
		c.logger.Error("trial end failed; retried next sweep", "id", lic.ID, "error", err)
		return
	}
	productName := ""
	if lic.Product != nil {
		productName = lic.Product.Name
	}
	switch {
	case res.Moved:
		c.store.Audit(ctx, &model.AuditLog{
			Entity: "license", EntityID: lic.ID, Action: "plan_changed",
			ActorType: "system",
			Changes: map[string]any{
				"reason": "trial_ended", "old_plan_id": res.From.ID, "new_plan_id": res.To.ID,
				"status": model.StatusActive, "valid_until": "cleared",
			},
		})
		c.webhook.Dispatch(ctx, lic.ProductID, model.EventPlanChanged, map[string]any{
			"license_id": lic.ID, "email": lic.Email, "old_plan_id": res.From.ID, "new_plan_id": res.To.ID, "reason": "trial_ended",
		})
		c.email.SendTrialExpired(lic.Email, productName, res.To.Name)
		c.logger.Info("trial ended; moved to fallback plan", "id", lic.ID, "plan_id", res.To.ID)
	case res.Expire:
		changes := map[string]any{"reason": "trial_ended"}
		if res.Refusal != "" {
			// The plan named a fallback that no longer qualifies: said
			// here, where support looks for why a trial expired.
			changes["fallback_refused"] = res.Refusal
			c.logger.Warn("trial fallback plan no longer qualifies; the trial expired instead",
				"id", lic.ID, "fallback_plan_id", res.FallbackPlanID, "reason", res.Refusal)
		}
		lic.Status = model.StatusExpired
		c.store.Audit(ctx, &model.AuditLog{
			Entity: "license", EntityID: lic.ID, Action: "expired",
			ActorType: "system",
			Changes:   changes,
		})
		c.webhook.Dispatch(ctx, lic.ProductID, "license.expired", map[string]any{
			"license_id": lic.ID, "email": lic.Email, "reason": "trial_ended",
		})
		c.email.SendTrialExpired(lic.Email, productName, "")
		c.logger.Info("trial expired", "id", lic.ID)
	}
	// Otherwise extended, moved or reinstated since it was read.
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
		productName := ""
		if lic.Product != nil {
			productName = lic.Product.Name
		}
		c.email.SendLicenseExpired(lic.Email, productName)
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
// (audit, sync) don't reset the clock. Each step is claimed
// (ClaimNotification) under a unique tag per past_due_at epoch, and
// queued, so:
//
//   - the same email never fires twice for one episode, even if the
//     checker runs every hour, and even with every replica running it:
//     a check-then-record would let two of them mail the same customer;
//   - an SMTP outage does not lose a step: the queue retries it, and
//     a mail it gives up on opens the step again for a later run;
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
		render  func(productName string) (subject, body string)
	}{
		// Highest threshold first — only one email per check per
		// license, and the "highest reached" wins. Without that,
		// a license that's been past_due for 20 days would receive
		// dunning_first + dunning_second + dunning_final on the
		// FIRST checker run after install.
		{14, "dunning_final", c.email.RenderDunningFinal},
		{7, "dunning_second", c.email.RenderDunningSecond},
		{1, "dunning_first", c.email.RenderPaymentFailed},
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
			token, err := c.store.ClaimNotification(ctx, lic.ID, tag)
			if err != nil {
				c.logger.Error("dunning claim failed", "license_id", lic.ID, "tag", step.tag, "error", err)
				break // the next run tries again
			}
			if token == "" {
				break // already sent the highest-tier, or another pass holds it; nothing lower will fire
			}
			// Queuing the mail and closing the claim share one
			// transaction, as for the other reminders. Failing to queue
			// hands the claim back for the next run; losing the claim
			// means another pass already owns it.
			subject, body := step.render(productName)
			switch err := c.store.EnqueueEmailAndCloseNotification(ctx, lic.Email, subject, body, token); {
			case err == nil:
				c.logger.Info("dunning email queued",
					"license_id", lic.ID, "tag", step.tag,
					"days_past_due", daysPastDue, "episode", episode)
			case errors.Is(err, store.ErrNotificationClaimLost):
				c.logger.Warn("dunning claim taken over; the other pass sends it", "license_id", lic.ID, "tag", step.tag)
			default:
				c.logger.Error("dunning email could not be queued", "license_id", lic.ID, "tag", step.tag, "error", err)
				if rerr := c.store.ReleaseNotification(ctx, token); rerr != nil {
					c.logger.Error("dunning claim not released", "license_id", lic.ID, "error", rerr)
				}
			}
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
