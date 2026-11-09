package service

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
)

// ReminderDaysSetting holds how many days before a license or trial runs
// out its customer is reminded (issue #36 item 8), e.g. "7,3,1".
const ReminderDaysSetting = "expiry_reminder_days"

// DefaultReminderDays is what an install that never set it uses.
var DefaultReminderDays = []int{7, 3, 1}

const (
	maxReminderDay   = 90
	maxReminderCount = 5
)

// ParseReminderDays reads a comma-separated list of whole days, such as
// "7, 3, 1", into a deduplicated list sorted largest first. Each value
// must be 1–90 and there must be 1–5 of them: turning the reminders off
// is the email's own switch, not an empty list.
func ParseReminderDays(raw string) ([]int, error) {
	var days []int
	for part := range strings.SplitSeq(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		// Plain digits only, like the dashboard input: Atoi alone would
		// also take "+7".
		n, err := strconv.Atoi(part)
		if err != nil || strings.TrimLeft(part, "0123456789") != "" || n < 1 || n > maxReminderDay {
			return nil, fmt.Errorf("reminder days must be whole numbers from 1 to %d, got %q", maxReminderDay, part)
		}
		if !slices.Contains(days, n) {
			days = append(days, n)
		}
	}
	if len(days) == 0 {
		return nil, errors.New("set at least one reminder day (turn the email off to stop reminders)")
	}
	if len(days) > maxReminderCount {
		return nil, fmt.Errorf("at most %d reminder days", maxReminderCount)
	}
	slices.SortFunc(days, func(a, b int) int { return b - a })
	return days, nil
}

// FormatReminderDays is the stored form of a parsed list: "7,3,1".
func FormatReminderDays(days []int) string {
	parts := make([]string, len(days))
	for i, d := range days {
		parts[i] = strconv.Itoa(d)
	}
	return strings.Join(parts, ",")
}

// reminderWindow picks the reminder a license is due for: the smallest
// configured window that still covers the time left. days is sorted
// largest first. One license gets one reminder per window as the date
// approaches; one that only comes in view inside a short window (a
// late extension, a short trial) gets that window's reminder alone, not
// every larger one at once.
func reminderWindow(days []int, left time.Duration) (int, bool) {
	for i := len(days) - 1; i >= 0; i-- {
		if left <= time.Duration(days[i])*24*time.Hour {
			return days[i], true
		}
	}
	return 0, false
}

// daysLeft rounds the time left up to whole days, at least 1: a license
// running out in 30 hours has "2 days" left, one in 2 hours "1 day".
func daysLeft(left time.Duration) int {
	return max(1, int(math.Ceil(left.Hours()/24)))
}

// utcDate is the calendar date a customer is shown for a license date, in
// UTC. Timestamps scanned from the database carry the connection's time
// zone, so formatting them directly names a different day on a server whose
// zone is not UTC; the updates-ending reminder already formats in UTC.
func utcDate(t time.Time) string {
	return t.UTC().Format("2006-01-02")
}

// canRemind reports whether a reminder sweep should queue anything: its
// switch is on and email is set up. Without email the queue does not
// drain, so a reminder queued now would go out whenever email is finally
// configured, long after it stopped being true. Nothing is claimed in
// that case, so the reminder a license is due at that point still goes
// out, once.
func (c *ExpiryChecker) canRemind(kind string) bool {
	if c.email == nil || !c.email.NotifyEnabled(kind) {
		return false
	}
	configured, err := c.email.Configured()
	if err != nil {
		c.logger.Warn("email config unreadable; skipping reminders this run", "kind", kind, "error", err)
		return false
	}
	return configured
}

func (c *ExpiryChecker) reminderDays(ctx context.Context) []int {
	raw, err := c.store.GetSetting(ctx, ReminderDaysSetting)
	if err != nil || strings.TrimSpace(raw) == "" {
		return DefaultReminderDays
	}
	days, err := ParseReminderDays(raw)
	if err != nil {
		// Saved values are validated; this is a hand-edited row.
		c.logger.Warn("invalid expiry reminder days; using the default", "value", raw, "error", err)
		return DefaultReminderDays
	}
	return days
}

// SendExpiryReminders emails customers whose license or trial runs out
// soon, at the configured number of days before (ReminderDaysSetting).
// Licenses on a Stripe subscription that will renew are left out: they
// get the renewal reminder instead (see FindLicensesForExpiryReminder).
func (c *ExpiryChecker) SendExpiryReminders(ctx context.Context) {
	// Turned off: record nothing, so switching it back on reminds the
	// licenses still inside a window (each once, for the window it is in).
	if !c.canRemind("license_expiring") {
		return
	}
	days := c.reminderDays(ctx)
	now := time.Now()
	licenses, err := c.store.FindLicensesForExpiryReminder(ctx, now, now.Add(time.Duration(days[0])*24*time.Hour))
	if err != nil {
		c.logger.Error("expiry reminder check failed", "error", err)
		return
	}
	for _, lic := range licenses {
		c.remindExpiring(ctx, lic, days, now)
	}
}

// remindExpiring queues the reminder lic is due for, if any, and reports
// whether it did. Each reminder is recorded per license, per expiry date
// and per window, so an extended license is reminded again for its new
// date and no window goes out twice. The record is claimed before the
// mail is queued (two replicas cannot both send it) and closed in the
// same transaction that queues it; the durable queue retries delivery,
// so an SMTP outage or a crash does not lose the reminder.
func (c *ExpiryChecker) remindExpiring(ctx context.Context, lic *model.License, days []int, now time.Time) bool {
	if lic.ValidUntil == nil {
		return false
	}
	left := lic.ValidUntil.Sub(now)
	window, ok := reminderWindow(days, left)
	if !ok {
		return false
	}
	sent, err := c.store.ExpiryRemindersSent(ctx, lic.ID, *lic.ValidUntil)
	if err != nil {
		c.logger.Error("expiry reminder lookup failed", "license_id", lic.ID, "error", err)
		return false
	}
	// A window at or inside this one has gone out for this date already:
	// a reminder further out would arrive out of order.
	if slices.ContainsFunc(sent, func(d int) bool { return d <= window }) {
		return false
	}
	token, err := c.store.ClaimNotification(ctx, lic.ID, ExpiryReminderTag(*lic.ValidUntil, window))
	if err != nil || token == "" {
		return false // another replica holds it, or the claim failed
	}
	productName := ""
	if lic.Product != nil {
		productName = lic.Product.Name
	}
	trial := lic.Status == model.StatusTrialing
	subject, body := c.email.RenderLicenseExpiring(productName, c.store.DecryptLicenseKey(lic),
		utcDate(*lic.ValidUntil), daysLeft(left), trial, c.trialFallbackName(ctx, lic, trial))
	switch err := c.store.EnqueueEmailAndCloseNotification(ctx, lic.Email, subject, body, token); {
	case err == nil:
		c.logger.Info("expiry reminder queued", "license_id", lic.ID, "window_days", window)
		return true
	case errors.Is(err, store.ErrNotificationClaimLost):
		return false
	default:
		// Give the claim back so the next run tries again.
		_ = c.store.ReleaseNotification(ctx, token)
		c.logger.Error("expiry reminder enqueue failed", "license_id", lic.ID, "error", err)
		return false
	}
}

// ExpiryReminderTag records one reminder window for one expiry date.
func ExpiryReminderTag(validUntil time.Time, window int) string {
	return fmt.Sprintf("expiry:%s:%dd", validUntil.UTC().Format(time.RFC3339), window)
}

// trialFallbackName is the name of the plan lic moves to when its trial
// ends, as ExpireTrials will see it, or "" when the trial will simply
// expire. A lookup failure says "" too: the reminder then reads as it
// did before fallbacks existed, which is still true of the deadline.
func (c *ExpiryChecker) trialFallbackName(ctx context.Context, lic *model.License, trial bool) string {
	if !trial || lic.Plan == nil || lic.Plan.TrialFallbackPlanID == "" || lic.StripeSubscriptionID != "" {
		return ""
	}
	to, err := c.store.FindPlanByID(ctx, lic.Plan.TrialFallbackPlanID)
	if err != nil || to.TrialFallbackRefusal(lic.ProductID) != "" {
		return ""
	}
	return to.Name
}
