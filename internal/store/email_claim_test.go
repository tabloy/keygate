package store

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/testsupport"

	"github.com/tabloy/keygate/internal/model"
)

// Every replica runs an email processor. Two of them must never take
// the same queued mail: the claim is part of the statement that reads
// it, so the second processor sees the rest of the queue instead.
func TestClaimNextEmailIsExclusive(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("skipping integration test: TEST_DATABASE_URL not set")
	}
	s, err := New(dsn)
	if err != nil {
		t.Skipf("skipping integration test: %v", err)
	}
	defer s.Close()
	if err := s.RunMigrations("../../db/migrations"); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	ctx := context.Background()
	tag := "claim-" + time.Now().Format("150405.000000") + "@example.com"
	defer func() {
		if _, err := s.DB.NewRaw("DELETE FROM email_queue WHERE to_addr = ?", tag).Exec(ctx); err != nil {
			t.Errorf("clean up: %v", err)
		}
	}()
	for range 3 {
		if err := s.EnqueueEmail(ctx, tag, "Reminder", "body"); err != nil {
			t.Fatal(err)
		}
	}
	// The queue is shared with whatever else this database is doing:
	// make these three the oldest so the claims below are this test's
	// rows, and hand back any other row a claim happened to take.
	if _, err := s.DB.NewRaw("UPDATE email_queue SET created_at = '1970-01-01' WHERE to_addr = ?", tag).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	release := func(got []*QueuedEmail) {
		for _, e := range got {
			if e.ToAddr != tag {
				if _, err := s.DB.NewRaw("UPDATE email_queue SET next_retry = NULL WHERE id = ?", e.ID).Exec(ctx); err != nil {
					t.Errorf("release %s: %v", e.ID, err)
				}
			}
		}
	}
	mine := func(got []*QueuedEmail) []string {
		release(got)
		var ids []string
		for _, e := range got {
			if e.ToAddr == tag {
				ids = append(ids, e.ID)
			}
		}
		return ids
	}

	claim := func() []*QueuedEmail {
		t.Helper()
		e, err := s.ClaimNextEmail(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if e == nil {
			return nil
		}
		return []*QueuedEmail{e}
	}
	var a, b []string
	for range 2 {
		a = append(a, mine(claim())...)
	}
	b = mine(claim())
	if len(a) != 2 || len(b) != 1 {
		t.Fatalf("claims: first=%d second=%d want 2 and 1", len(a), len(b))
	}
	for _, id := range a {
		for _, other := range b {
			if id == other {
				t.Fatalf("both processors took %s", id)
			}
		}
	}
	// Nothing is left to take until the leases run out.
	if got := mine(claim()); len(got) != 0 {
		t.Fatalf("claimed mail was still on offer: %v", got)
	}
	// A processor that died mid-send releases them: the lease is a
	// timestamp, not a lock held by a connection.
	if _, err := s.DB.NewRaw("UPDATE email_queue SET next_retry = now() - interval '1 minute' WHERE to_addr = ?", tag).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	var again []string
	for range 3 {
		again = append(again, mine(claim())...)
	}
	if len(again) != 3 {
		t.Fatalf("expired leases: got %d want 3", len(again))
	}
	// A sent mail is out of the queue for good — closed with the
	// token its holder claimed it by.
	var token string
	if err := s.DB.NewRaw("SELECT claim_token FROM email_queue WHERE id = ?", a[0]).Scan(ctx, &token); err != nil {
		t.Fatal(err)
	}
	s.MarkEmailSent(ctx, a[0], "not-the-holder")
	var status string
	if err := s.DB.NewRaw("SELECT status FROM email_queue WHERE id = ?", a[0]).Scan(ctx, &status); err != nil {
		t.Fatal(err)
	}
	if status != "pending" {
		t.Fatalf("a stranger closed the mail: %q", status)
	}
	s.MarkEmailSent(ctx, a[0], token)
	if _, err := s.DB.NewRaw("UPDATE email_queue SET next_retry = NULL WHERE to_addr = ?", tag).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	var left []string
	for range 3 {
		left = append(left, mine(claim())...)
	}
	if len(left) != 2 {
		t.Fatalf("after one was sent: got %d want 2", len(left))
	}
}

// A reminder is only done when its mail actually goes out. If the
// mail server stays down long enough for the queue to give up, the
// claim that was closed when the mail was queued is opened again, so
// a later pass sends the reminder instead of skipping that licence
// for good.
func TestGivingUpOnAMailReopensItsReminder(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("skipping integration test: TEST_DATABASE_URL not set")
	}
	s, err := New(dsn)
	if err != nil {
		t.Skipf("skipping integration test: %v", err)
	}
	defer s.Close()
	if err := s.RunMigrations("../../db/migrations"); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	ctx := context.Background()
	suffix := time.Now().Format("150405.000000")
	prod := &model.Product{Name: "Remind", Slug: "remind-" + suffix, Type: "desktop", FeedLicenseRequired: true}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	plan := &model.Plan{ProductID: prod.ID, Name: "P", Slug: "rp-" + suffix, LicenseType: "perpetual", LicenseModel: "standard"}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	lic := &model.License{ProductID: prod.ID, PlanID: plan.ID, Email: "remind-" + suffix + "@example.com",
		LicenseKey: "KEY-RM-" + suffix, Status: model.StatusActive, PaymentProvider: "manual"}
	if err := s.CreateLicense(ctx, lic); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := s.DB.NewRaw("DELETE FROM email_queue WHERE to_addr = ?", lic.Email).Exec(ctx); err != nil {
			t.Errorf("clean up: %v", err)
		}
	}()

	tag := "updates-ending-" + suffix
	token, err := s.ClaimNotification(ctx, lic.ID, tag)
	if err != nil || token == "" {
		t.Fatalf("claim: %q %v", token, err)
	}
	if err := s.EnqueueEmailAndCloseNotification(ctx, lic.Email, "Renewal coming up", "body", token); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	// While the mail is queued the reminder counts as handled: no
	// second copy is claimed.
	if again, err := s.ClaimNotification(ctx, lic.ID, tag); err != nil || again != "" {
		t.Fatalf("claimed again while queued: %q %v", again, err)
	}

	// This mail by address, not whatever is next in a queue the rest
	// of the suite is also using.
	var queuedID string
	var maxAttempts int
	if err := s.DB.NewRaw("SELECT id, max_attempts FROM email_queue WHERE to_addr = ?", lic.Email).
		Scan(ctx, &queuedID, &maxAttempts); err != nil {
		t.Fatalf("find the queued mail: %v", err)
	}
	for i := 0; i < maxAttempts; i++ {
		// Each attempt claims the mail again: the token changes with
		// the holder, and a stale one changes nothing.
		var token string
		if err := s.DB.NewRaw("SELECT claim_token FROM email_queue WHERE id = ?", queuedID).Scan(ctx, &token); err != nil {
			t.Fatal(err)
		}
		s.MarkEmailFailed(ctx, queuedID, "stale-token", "smtp down")
		s.MarkEmailFailed(ctx, queuedID, token, "smtp down")
	}
	var status string
	if err := s.DB.NewRaw("SELECT status FROM email_queue WHERE id = ?", queuedID).Scan(ctx, &status); err != nil {
		t.Fatal(err)
	}
	if status != "failed" {
		t.Fatalf("queue status: %q want failed", status)
	}
	// Given up on: the reminder is open again, and the next pass takes
	// it.
	retry, err := s.ClaimNotification(ctx, lic.ID, tag)
	if err != nil || retry == "" {
		t.Fatalf("a reminder whose mail was given up on was not re-sent: %q %v", retry, err)
	}
}

// A trial licence's end is written twice — into the licence and into
// the subscription behind it — and both readings must come from the
// same plan. A trial length changed between them is refused rather
// than written as two different dates.
func TestCreateLicenseRefusesARetimedTrial(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("skipping integration test: TEST_DATABASE_URL not set")
	}
	s, err := New(dsn)
	if err != nil {
		t.Skipf("skipping integration test: %v", err)
	}
	defer s.Close()
	if err := s.RunMigrations("../../db/migrations"); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	ctx := context.Background()
	suffix := time.Now().Format("150405.000000")
	prod := &model.Product{Name: "Trial", Slug: "trial-" + suffix, Type: "desktop"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	plan := &model.Plan{ProductID: prod.ID, Name: "T", Slug: "tr-" + suffix, LicenseType: "trial", LicenseModel: "standard", TrialDays: 14}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	stale := *plan
	if _, err := s.DB.NewRaw("UPDATE plans SET trial_days = 30 WHERE id = ?", plan.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	until := time.Now().AddDate(0, 0, stale.TrialDays)
	lic := &model.License{ProductID: prod.ID, PlanID: plan.ID, Email: "trial-" + suffix + "@example.com",
		LicenseKey: "KEY-TR-" + suffix, Status: model.StatusTrialing, PaymentProvider: "stripe", ValidUntil: &until}
	if err := s.CreateLicenseWithSubscription(ctx, lic, &stale); !errors.Is(err, ErrPlanChanged) {
		t.Fatalf("create from a retimed trial plan: %v want ErrPlanChanged", err)
	}
	var n int
	if err := s.DB.NewRaw("SELECT count(*) FROM licenses WHERE email = ?", lic.Email).Scan(ctx, &n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("a licence was written with two different trial ends: %d", n)
	}
	// Rebuilt from the plan as it reads now, the licence and its
	// subscription agree.
	fresh, err := s.FindPlanByID(ctx, plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	freshUntil := time.Now().AddDate(0, 0, fresh.TrialDays)
	lic2 := &model.License{ProductID: prod.ID, PlanID: plan.ID, Email: "trial2-" + suffix + "@example.com",
		LicenseKey: "KEY-TR2-" + suffix, Status: model.StatusTrialing, PaymentProvider: "stripe", ValidUntil: &freshUntil}
	if err := s.CreateLicenseWithSubscription(ctx, lic2, fresh); err != nil {
		t.Fatalf("create from the fresh plan: %v", err)
	}
	var trialEnd time.Time
	if err := s.DB.NewRaw("SELECT trial_end FROM subscriptions WHERE license_id = ?", lic2.ID).Scan(ctx, &trialEnd); err != nil {
		t.Fatal(err)
	}
	if trialEnd.Sub(freshUntil).Abs() > time.Minute {
		t.Fatalf("the subscription ends at %v, the licence at %v", trialEnd, freshUntil)
	}
}

// The rollout confirmation is the only fence left between a
// mixed-version fleet and a period no old replica enforces, so it is
// read on the transaction that writes the licence and held until it
// commits — a check made before the write would let an operator
// switch it off in between.
func TestCreateLicenseHoldsTheMaintenanceSwitch(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("skipping integration test: TEST_DATABASE_URL not set")
	}
	s, err := New(dsn)
	if err != nil {
		t.Skipf("skipping integration test: %v", err)
	}
	defer s.Close()
	if err := s.RunMigrations("../../db/migrations"); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	testsupport.LockSettings(t, s.DB)
	ctx := context.Background()
	prev, _ := s.GetSetting(ctx, SettingMaintenanceFeatures)
	defer func() {
		if prev != "" {
			if err := s.SetSettings(ctx, map[string]string{SettingMaintenanceFeatures: prev}); err != nil {
				t.Errorf("restore the switch: %v", err)
			}
		}
	}()
	suffix := time.Now().Format("150405.000000")
	drained := time.Now().Add(-30 * 24 * time.Hour)
	prod := &model.Product{Name: "Fence", Slug: "fence-" + suffix, Type: "desktop", FeedLicenseRequired: true, FeedGatedAt: &drained}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	plan := &model.Plan{ProductID: prod.ID, Name: "B", Slug: "fb-" + suffix, LicenseType: "perpetual", LicenseModel: "standard", UpdatesDays: 365}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	until := time.Now().AddDate(0, 0, 365)
	newLicence := func(tag string) *model.License {
		return &model.License{ProductID: prod.ID, PlanID: plan.ID, Email: tag + "-" + suffix + "@example.com",
			LicenseKey: "KEY-" + tag + "-" + suffix, Status: model.StatusActive, PaymentProvider: "stripe",
			UpdatesUntil: &until, UpdatesTermsSet: true}
	}

	if err := s.SetSettings(ctx, map[string]string{SettingMaintenanceFeatures: "false"}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateLicenseWithSubscription(ctx, newLicence("off"), plan); !errors.Is(err, ErrUpdatePeriodNotEnforceable) {
		t.Fatalf("write with the switch off: %v want ErrUpdatePeriodNotEnforceable", err)
	}
	if err := s.SetSettings(ctx, map[string]string{SettingMaintenanceFeatures: "true"}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateLicenseWithSubscription(ctx, newLicence("on"), plan); err != nil {
		t.Fatalf("write with the switch on: %v", err)
	}
}

// An admin issuing a licence derives its update period from the plan
// they read; a purchase carries the period frozen at checkout. Both
// were decided against a snapshot, so a plan whose period moved since
// is read again rather than written from stale terms.
func TestCreateLicenseRefusesAMovedUpdatePeriod(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("skipping integration test: TEST_DATABASE_URL not set")
	}
	s, err := New(dsn)
	if err != nil {
		t.Skipf("skipping integration test: %v", err)
	}
	defer s.Close()
	if err := s.RunMigrations("../../db/migrations"); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	testsupport.LockSettings(t, s.DB)
	ctx := context.Background()
	if err := s.SetSettings(ctx, map[string]string{SettingMaintenanceFeatures: "true"}); err != nil {
		t.Fatal(err)
	}
	suffix := time.Now().Format("150405.000000")
	drained := time.Now().Add(-30 * 24 * time.Hour)
	prod := &model.Product{Name: "Moved", Slug: "moved-" + suffix, Type: "desktop", FeedLicenseRequired: true, FeedGatedAt: &drained}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	plan := &model.Plan{ProductID: prod.ID, Name: "B", Slug: "mv-" + suffix, LicenseType: "perpetual", LicenseModel: "standard", UpdatesDays: 365}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	// The admin's copy still says 365; another request grants updates
	// for life before this licence is written.
	stale := *plan
	if _, err := s.DB.NewRaw("UPDATE plans SET updates_days = 0 WHERE id = ?", plan.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	until := stale.InitialUpdatesUntil(time.Now())
	if until == nil {
		t.Fatal("the snapshot sells no period")
	}
	lic := &model.License{ProductID: prod.ID, PlanID: plan.ID, Email: "moved-" + suffix + "@example.com",
		LicenseKey: "KEY-MV-" + suffix, Status: model.StatusActive, PaymentProvider: "manual",
		UpdatesUntil: until, UpdatesTermsSet: true}
	if err := s.CreateLicenseWithSubscription(ctx, lic, &stale); !errors.Is(err, ErrPlanChanged) {
		t.Fatalf("write from a stale period: %v want ErrPlanChanged", err)
	}
	var n int
	if err := s.DB.NewRaw("SELECT count(*) FROM licenses WHERE email = ?", lic.Email).Scan(ctx, &n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("a licence was written with a period the plan no longer sells: %d", n)
	}
	// Read again, the same issuance grants what the plan says now.
	fresh, err := s.FindPlanByID(ctx, plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	lic2 := &model.License{ProductID: prod.ID, PlanID: plan.ID, Email: "moved2-" + suffix + "@example.com",
		LicenseKey: "KEY-MV2-" + suffix, Status: model.StatusActive, PaymentProvider: "manual",
		UpdatesUntil: fresh.InitialUpdatesUntil(time.Now()), UpdatesTermsSet: true}
	if err := s.CreateLicenseWithSubscription(ctx, lic2, fresh); err != nil {
		t.Fatalf("write from the fresh plan: %v", err)
	}
	var stored *time.Time
	if err := s.DB.NewRaw("SELECT updates_until FROM licenses WHERE id = ?", lic2.ID).Scan(ctx, &stored); err != nil {
		t.Fatal(err)
	}
	if stored != nil {
		t.Fatalf("updates for life was written as a cutoff: %v", stored)
	}
}
