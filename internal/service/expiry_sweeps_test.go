package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/model"
)

// paidPlan is a monthly subscription plan with grace days of grace.
func (f *fallbackFixture) paidPlan(grace int) *model.Plan {
	f.t.Helper()
	p := f.plan("Pro", "subscription", "")
	if _, err := f.s.DB.NewRaw("UPDATE plans SET grace_days = ? WHERE id = ?", grace, p.ID).Exec(f.ctx); err != nil {
		f.t.Fatal(err)
	}
	return p
}

// licenseIn is a licence on plan with one device, put in status with
// validUntil as its end date (nil: perpetual).
func (f *fallbackFixture) licenseIn(plan *model.Plan, status string, validUntil *time.Time) *model.License {
	f.t.Helper()
	lic := f.license(plan)
	if _, err := f.s.DB.NewRaw("UPDATE licenses SET status = ?, valid_until = ? WHERE id = ?",
		status, validUntil, lic.ID).Exec(f.ctx); err != nil {
		f.t.Fatal(err)
	}
	return f.load(lic)
}

func ago(d time.Duration) *time.Time {
	t := time.Now().Add(-d)
	return &t
}

const day = 24 * time.Hour

// The grace sweep expires a licence exactly when verify stops taking
// its key: an hour inside the grace period both still accept it, an
// hour past it both refuse it. If the two drift, a customer is cut off
// while their key still verifies, or keeps working on a licence the
// server has given up on.
func TestGraceSweep_ExpiresWhenVerifyStops(t *testing.T) {
	f := newFallbackFixture(t)
	for _, grace := range []int{0, 3} {
		plan := f.paidPlan(grace)
		for _, status := range []string{model.StatusActive, model.StatusPastDue} {
			inside := f.licenseIn(plan, status, ago(time.Duration(grace)*day-time.Hour))
			past := f.licenseIn(plan, status, ago(time.Duration(grace)*day+time.Hour))
			if err := LicenseUsable(inside); err != nil {
				t.Fatalf("grace %d, %s, an hour inside: verify refuses it (%v)", grace, status, err)
			}
			if err := LicenseUsable(past); err == nil {
				t.Fatalf("grace %d, %s, an hour past: verify still takes it", grace, status)
			}

			f.c.ExpireGracePeriodLicenses(f.ctx)

			if got := f.load(inside).Status; got != status {
				t.Fatalf("grace %d, %s, an hour inside: the sweep left it %s", grace, status, got)
			}
			if got := f.load(past).Status; got != model.StatusExpired {
				t.Fatalf("grace %d, %s, an hour past: the sweep left it %s, want expired", grace, status, got)
			}
		}
	}
}

// Expiring a licence is said everywhere it shows: its subscription row,
// the audit log, the license.expired webhook and the customer's email.
// A second sweep says nothing more.
func TestGraceSweep_ExpiryIsAnnouncedOnce(t *testing.T) {
	f := newFallbackFixture(t)
	setNotify(t, f.s, "license_expired", "")
	lic := f.licenseIn(f.paidPlan(2), model.StatusActive, ago(3*day))

	f.c.ExpireGracePeriodLicenses(f.ctx)
	f.c.ExpireGracePeriodLicenses(f.ctx)

	if got := f.load(lic).Status; got != model.StatusExpired {
		t.Fatalf("licence is %s, want expired", got)
	}
	if sub, err := f.s.FindSubscriptionByLicense(f.ctx, lic.ID); err != nil || sub.Status != model.StatusExpired {
		t.Fatalf("subscription row %+v (%v); want it expired with the licence", sub, err)
	}
	if n := f.eventually(1, "SELECT count(*) FROM audit_logs WHERE entity_id = ? AND action = 'expired' AND changes->>'reason' = 'grace_period_ended'",
		lic.ID); n != 1 {
		t.Fatalf("%d expired audit entries, want 1", n)
	}
	if n := f.count("SELECT count(*) FROM webhook_deliveries WHERE webhook_id = ? AND event = ? AND payload->'data'->>'license_id' = ? "+
		"AND payload->'data'->>'reason' = 'grace_period_ended' AND payload->'data'->>'email' = ?",
		f.webhook.ID, model.EventLicenseExpired, lic.ID, lic.Email); n != 1 {
		t.Fatalf("%d license.expired deliveries with reason and email, want 1", n)
	}
	// Waits for a second email, which must not come.
	if subjects := f.mail.to(lic.Email, 2); len(subjects) != 1 || subjects[0] != "Fallback App license expired" {
		t.Fatalf("mail sent: %q; want the license expired email once", subjects)
	}
}

// The grace sweep is for active and past-due licences with an end date.
// Trials are the trial sweep's, a canceled licence stops at its end date
// without being expired, and a perpetual one never ends.
func TestGraceSweep_LeavesOtherLicencesAlone(t *testing.T) {
	f := newFallbackFixture(t)
	plan := f.paidPlan(0)
	kept := map[*model.License]string{
		f.licenseIn(plan, model.StatusTrialing, ago(10*day)):  model.StatusTrialing,
		f.licenseIn(plan, model.StatusCanceled, ago(10*day)):  model.StatusCanceled,
		f.licenseIn(plan, model.StatusSuspended, ago(10*day)): model.StatusSuspended,
		f.licenseIn(plan, model.StatusRevoked, ago(10*day)):   model.StatusRevoked,
		f.licenseIn(plan, model.StatusActive, nil):            model.StatusActive,
	}

	f.c.ExpireGracePeriodLicenses(f.ctx)

	for lic, want := range kept {
		if got := f.load(lic).Status; got != want {
			t.Fatalf("a %s licence was made %s", want, got)
		}
		if n := f.deliveries(lic, model.EventLicenseExpired); n != 0 {
			t.Fatalf("a %s licence got %d license.expired webhooks", want, n)
		}
	}
}

// An upgraded licence is first offered to EndGraceExpiredUpgrade, with
// the cutoff its end date must be before. When the hook takes it, or
// fails, the sweep leaves it; after a failure the next sweep tries again.
func TestGraceSweep_UpgradeHook(t *testing.T) {
	f := newFallbackFixture(t)
	lic := f.licenseIn(f.paidPlan(2), model.StatusActive, ago(3*day))

	var cutoff time.Time
	answer := struct {
		handled bool
		err     error
	}{handled: true}
	f.c.EndGraceExpiredUpgrade = func(_ context.Context, l *model.License, c time.Time) (bool, error) {
		if l.ID != lic.ID {
			return false, nil
		}
		cutoff = c
		return answer.handled, answer.err
	}

	before := time.Now()
	f.c.ExpireGracePeriodLicenses(f.ctx)
	if want := before.Add(-2 * day); cutoff.Before(want.Add(-time.Second)) || cutoff.After(time.Now().Add(-2*day)) {
		t.Fatalf("hook got cutoff %v, want now less the 2 grace days (%v)", cutoff, want)
	}
	if got := f.load(lic).Status; got != model.StatusActive || f.deliveries(lic, model.EventLicenseExpired) != 0 {
		t.Fatalf("the hook took it, but the sweep made it %s", got)
	}

	answer.handled, answer.err = false, errors.New("stripe is down")
	f.c.ExpireGracePeriodLicenses(f.ctx)
	if got := f.load(lic).Status; got != model.StatusActive || f.deliveries(lic, model.EventLicenseExpired) != 0 {
		t.Fatalf("the hook failed, but the sweep made it %s", got)
	}

	answer.err = nil
	f.c.ExpireGracePeriodLicenses(f.ctx)
	if got := f.load(lic).Status; got != model.StatusExpired || f.deliveries(lic, model.EventLicenseExpired) != 1 {
		t.Fatalf("the hook passed on it, but the sweep left it %s", got)
	}
}

// pastDue is a past-due licence with no end date whose dunning clock
// started at since; nil leaves past_due_at unset, as on rows from before
// the column, with updated_at at updated.
func (f *fallbackFixture) pastDue(since, updated *time.Time) *model.License {
	f.t.Helper()
	lic := f.licenseIn(f.free, model.StatusPastDue, nil)
	if _, err := f.s.DB.NewRaw("UPDATE licenses SET past_due_at = ?, updated_at = ? WHERE id = ?",
		since, updated, lic.ID).Exec(f.ctx); err != nil {
		f.t.Fatal(err)
	}
	return f.load(lic)
}

// A licence past due for more than 30 days is expired and announced. A
// row from before past_due_at existed counts from its updated_at.
func TestPastDueSweep_ExpiresAfter30Days(t *testing.T) {
	f := newFallbackFixture(t)
	now := time.Now()
	stale := f.pastDue(ago(31*day), &now)
	fresh := f.pastDue(ago(29*day), &now)
	legacyStale := f.pastDue(nil, ago(31*day))
	legacyFresh := f.pastDue(nil, ago(29*day))

	f.c.MarkPastDueAsExpired(f.ctx)
	f.c.MarkPastDueAsExpired(f.ctx)

	for lic, want := range map[*model.License]string{
		stale: model.StatusExpired, fresh: model.StatusPastDue,
		legacyStale: model.StatusExpired, legacyFresh: model.StatusPastDue,
	} {
		if got := f.load(lic).Status; got != want {
			t.Fatalf("past due since %v (updated %v): %s, want %s", lic.PastDueAt, lic.UpdatedAt, got, want)
		}
	}
	// The episode stays on record: it is how a later past_due event is
	// told from a renewal that failed after a grace expiry, and refused.
	if f.load(stale).PastDueAt == nil {
		t.Fatal("the sweep cleared past_due_at")
	}
	if sub, err := f.s.FindSubscriptionByLicense(f.ctx, stale.ID); err != nil || sub.Status != model.StatusExpired {
		t.Fatalf("subscription row %+v (%v); want it expired with the licence", sub, err)
	}
	if n := f.eventually(1, "SELECT count(*) FROM audit_logs WHERE entity_id = ? AND action = 'expired' AND changes->>'reason' = 'past_due_timeout'",
		stale.ID); n != 1 {
		t.Fatalf("%d expired audit entries, want 1", n)
	}
	if n := f.count("SELECT count(*) FROM webhook_deliveries WHERE webhook_id = ? AND event = ? AND payload->'data'->>'license_id' = ? "+
		"AND payload->'data'->>'reason' = 'past_due_timeout'",
		f.webhook.ID, model.EventLicenseExpired, stale.ID); n != 1 {
		t.Fatalf("%d license.expired deliveries with reason past_due_timeout, want 1", n)
	}
	if f.deliveries(fresh, model.EventLicenseExpired) != 0 || f.deliveries(legacyFresh, model.EventLicenseExpired) != 0 {
		t.Fatal("a licence inside its 30 days was announced as expired")
	}
}

// As the grace sweep: the customer whose licence the past-due sweep
// expired gets the email once, with the product named, and one still
// inside its 30 days gets none.
func TestPastDueSweep_EmailsTheCustomerOnce(t *testing.T) {
	f := newFallbackFixture(t)
	setNotify(t, f.s, "license_expired", "")
	now := time.Now()
	stale := f.pastDue(ago(31*day), &now)
	fresh := f.pastDue(ago(29*day), &now)

	f.c.MarkPastDueAsExpired(f.ctx)
	f.c.MarkPastDueAsExpired(f.ctx)

	// Waits for a second email, which must not come.
	if subjects := f.mail.to(stale.Email, 2); len(subjects) != 1 || subjects[0] != "Fallback App license expired" {
		t.Fatalf("mail sent: %q; want the license expired email once", subjects)
	}
	if subjects := f.mail.to(fresh.Email, 0); len(subjects) != 0 {
		t.Fatalf("mail sent inside the 30 days: %q", subjects)
	}
}

// As the grace sweep: EndPastDueUpgrade is asked first, with the 30-day
// threshold, and a licence it takes or fails on stays past due.
func TestPastDueSweep_UpgradeHook(t *testing.T) {
	f := newFallbackFixture(t)
	now := time.Now()
	lic := f.pastDue(ago(31*day), &now)

	var threshold time.Time
	answer := struct {
		handled bool
		err     error
	}{handled: true}
	f.c.EndPastDueUpgrade = func(_ context.Context, l *model.License, th time.Time) (bool, error) {
		if l.ID != lic.ID {
			return false, nil
		}
		threshold = th
		return answer.handled, answer.err
	}

	before := time.Now()
	f.c.MarkPastDueAsExpired(f.ctx)
	if threshold.Before(before.Add(-30*day-time.Second)) || threshold.After(time.Now().Add(-30*day)) {
		t.Fatalf("hook got threshold %v, want now less 30 days", threshold)
	}
	if got := f.load(lic).Status; got != model.StatusPastDue {
		t.Fatalf("the hook took it, but the sweep made it %s", got)
	}

	answer.handled, answer.err = false, errors.New("stripe is down")
	f.c.MarkPastDueAsExpired(f.ctx)
	if got := f.load(lic).Status; got != model.StatusPastDue {
		t.Fatalf("the hook failed, but the sweep made it %s", got)
	}

	answer.err = nil
	f.c.MarkPastDueAsExpired(f.ctx)
	if got := f.load(lic).Status; got != model.StatusExpired {
		t.Fatalf("the hook passed on it, but the sweep left it %s", got)
	}
}

// Devices are cleared from expired and revoked licences only: every
// other status can still be used, or used again, with its devices.
func TestCleanupExpiredActivations(t *testing.T) {
	f := newFallbackFixture(t)
	plan := f.paidPlan(7)
	devices := map[string]int{
		model.StatusExpired: 0, model.StatusRevoked: 0,
		model.StatusActive: 1, model.StatusTrialing: 1, model.StatusPastDue: 1,
		model.StatusCanceled: 1, model.StatusSuspended: 1,
	}
	lics := map[string]*model.License{}
	for status := range devices {
		lics[status] = f.licenseIn(plan, status, ago(day))
	}

	f.c.CleanupExpiredActivations(f.ctx)

	for status, want := range devices {
		if n := f.count("SELECT count(*) FROM activations WHERE license_id = ?", lics[status].ID); n != want {
			t.Fatalf("a %s licence has %d devices, want %d", status, n, want)
		}
	}
}
