package service

import (
	"context"
	"log/slog"
	"time"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
)

type ExpiryChecker struct {
	store   *store.Store
	email   *EmailService
	webhook *WebhookService
	logger  *slog.Logger
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
		graceEnd := lic.ValidUntil.Add(time.Duration(graceDays) * 24 * time.Hour)
		if time.Now().After(graceEnd) {
			lic.Status = model.StatusExpired
			if err := c.store.UpdateLicenseAndSubscription(ctx, lic, "status"); err != nil {
				c.logger.Error("expire license failed", "id", lic.ID, "error", err)
				continue
			}
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
		lic.Status = model.StatusExpired
		if err := c.store.UpdateLicenseAndSubscription(ctx, lic, "status"); err != nil {
			c.logger.Error("expire trial failed", "id", lic.ID, "error", err)
			continue
		}
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
		lic.Status = model.StatusExpired
		if err := c.store.UpdateLicenseAndSubscription(ctx, lic, "status"); err != nil {
			continue
		}
		c.store.Audit(ctx, &model.AuditLog{
			Entity: "license", EntityID: lic.ID, Action: "expired",
			ActorType: "system",
			Changes:   map[string]any{"reason": "past_due_timeout"},
		})
		c.logger.Info("past_due expired", "id", lic.ID)
	}
}

// SendExpiryReminders sends notifications for upcoming expirations.
// Uses a notified_at tracking to prevent duplicate emails.
func (c *ExpiryChecker) SendExpiryReminders(ctx context.Context) {
	reminders := []struct {
		days int
		tag  string
	}{
		{7, "expiry_7d"},
		{3, "expiry_3d"},
		{1, "expiry_1d"},
	}

	for _, r := range reminders {
		from := time.Now()
		to := from.Add(time.Duration(r.days) * 24 * time.Hour)
		licenses, err := c.store.FindExpiringLicenses(ctx, from, to)
		if err != nil {
			c.logger.Error("expiry reminder check failed", "error", err)
			continue
		}
		for _, lic := range licenses {
			// Check if we already sent this reminder
			if c.store.HasNotification(ctx, lic.ID, r.tag) {
				continue
			}
			productName := ""
			if lic.Product != nil {
				productName = lic.Product.Name
			}
			expiresAt := ""
			if lic.ValidUntil != nil {
				expiresAt = lic.ValidUntil.Format("2006-01-02")
			}
			c.email.SendLicenseExpiring(lic.Email, productName, lic.LicenseKey, expiresAt)
			c.store.RecordNotification(ctx, lic.ID, r.tag)
			c.logger.Info("expiry reminder sent", "license_id", lic.ID, "days", r.days)
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

// SendPaymentFailureReminders sends escalating emails for past_due licenses.
func (c *ExpiryChecker) SendPaymentFailureReminders(ctx context.Context) {
	// Find past_due licenses
	var licenses []*model.License
	err := c.store.DB.NewSelect().Model(&licenses).
		Relation("Product").
		Where("license.status = 'past_due'").
		Scan(ctx)
	if err != nil || len(licenses) == 0 {
		return
	}

	for _, lic := range licenses {
		daysPastDue := int(time.Since(lic.UpdatedAt).Hours() / 24)
		productName := ""
		if lic.Product != nil {
			productName = lic.Product.Name
		}

		var tag string
		switch {
		case daysPastDue >= 14 && daysPastDue < 15:
			tag = "dunning_final"
		case daysPastDue >= 7 && daysPastDue < 8:
			tag = "dunning_second"
		case daysPastDue >= 1 && daysPastDue < 2:
			tag = "dunning_first"
		default:
			continue
		}

		if c.store.HasNotification(ctx, lic.ID, tag) {
			continue
		}

		switch tag {
		case "dunning_first":
			c.email.SendPaymentFailed(lic.Email, productName)
		case "dunning_second":
			c.email.SendDunningSecond(lic.Email, productName)
		case "dunning_final":
			c.email.SendDunningFinal(lic.Email, productName)
		}

		c.store.RecordNotification(ctx, lic.ID, tag)
		c.logger.Info("dunning email sent", "license_id", lic.ID, "tag", tag, "days_past_due", daysPastDue)
	}
}

// SendRenewalReminders notifies users 24 hours before renewal.
func (c *ExpiryChecker) SendRenewalReminders(ctx context.Context) {
	from := time.Now().Add(23 * time.Hour)
	to := time.Now().Add(25 * time.Hour)
	licenses, err := c.store.FindExpiringLicenses(ctx, from, to)
	if err != nil {
		return
	}
	for _, lic := range licenses {
		if lic.Status != model.StatusActive {
			continue
		}
		// Only for subscription type
		if lic.Plan == nil || lic.Plan.LicenseType != "subscription" {
			continue
		}
		tag := "renewal_24h"
		if c.store.HasNotification(ctx, lic.ID, tag) {
			continue
		}
		productName := ""
		if lic.Product != nil {
			productName = lic.Product.Name
		}
		renewalDate := ""
		if lic.ValidUntil != nil {
			renewalDate = lic.ValidUntil.Format("2006-01-02")
		}
		c.email.SendRenewalReminder(lic.Email, productName, renewalDate)
		c.store.RecordNotification(ctx, lic.ID, tag)
	}
}

// SyncSubscriptionStates is a fallback that syncs subscription table with license status.
// This catches cases where Stripe/PayPal webhooks were missed.
func (c *ExpiryChecker) SyncSubscriptionStates(ctx context.Context) {
	if err := c.store.SyncSubscriptionStatuses(ctx); err != nil {
		c.logger.Error("subscription sync failed", "error", err)
	}
}
