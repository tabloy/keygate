package service

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
	"github.com/tabloy/keygate/internal/testsupport"
)

func openNotifyStore(t *testing.T) *store.Store {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("skipping integration test: TEST_DATABASE_URL not set")
	}
	s, err := store.New(dsn)
	if err != nil {
		t.Skipf("skipping integration test: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.RunMigrations("../../db/migrations"); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	return s
}

// setNotify writes (or with value "" removes) an email switch. The
// settings lock puts the original state back when the test ends: the
// settings are global.
func setNotify(t *testing.T, s *store.Store, kind, value string) {
	t.Helper()
	testsupport.LockSettings(t, s.DB)
	ctx := context.Background()
	key := NotifySettingKey(kind)
	if value == "" {
		_ = s.DeleteSetting(ctx, key)
		return
	}
	if err := s.SetSetting(ctx, key, value); err != nil {
		t.Fatal(err)
	}
}

// Unset means on; only "false" turns an email off.
func TestNotifyEnabled(t *testing.T) {
	s := openNotifyStore(t)
	e := NewEmailService("", "", "", "", "", slog.Default(), s)
	for _, tc := range []struct {
		value string
		want  bool
	}{{"", true}, {"true", true}, {"false", false}} {
		setNotify(t, s, "license_suspended", tc.value)
		if got := e.NotifyEnabled("license_suspended"); got != tc.want {
			t.Fatalf("value %q: NotifyEnabled = %v, want %v", tc.value, got, tc.want)
		}
	}
	var nilService *EmailService
	if !nilService.NotifyEnabled("license_suspended") {
		t.Fatal("no email service configured must not read as switched off")
	}
}

// A reminder job that is switched off returns before reading anything:
// nothing queued, nothing recorded, so switching it back on reminds the
// licenses still inside the window. (The "on" path is covered by
// TestSendUpdatesEndingReminders_QueuesAndClosesTheClaim.)
func TestUpdatesEndingReminderRespectsTheSwitch(t *testing.T) {
	s := openNotifyStore(t)
	ctx := context.Background()
	suffix := time.Now().Format("150405.000000")
	prod := &model.Product{Name: "Switch", Slug: "switch-" + suffix, Type: "desktop", FeedLicenseRequired: true}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	plan := &model.Plan{ProductID: prod.ID, Name: "P", Slug: "switch-" + suffix, LicenseType: "perpetual", LicenseModel: "standard", RenewalDays: 365, StripeRenewalPriceID: "price_switch_" + suffix}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	soon := time.Now().Add(5 * 24 * time.Hour)
	email := "switch-" + suffix + "@example.com"
	lic := &model.License{ProductID: prod.ID, PlanID: plan.ID, Email: email, LicenseKey: "KEY-switch-" + suffix, Status: model.StatusActive, UpdatesUntil: &soon}
	if err := s.CreateLicense(ctx, lic); err != nil {
		t.Fatal(err)
	}
	c := NewExpiryChecker(s, NewEmailService("127.0.0.1", "1", "", "", "keygate@test.local", slog.Default(), s), nil, slog.Default())

	setNotify(t, s, "updates_ending", "false")
	c.SendUpdatesEndingReminders(ctx)
	var queued, recorded int
	_ = s.DB.NewRaw("SELECT count(*) FROM email_queue WHERE to_addr = ?", email).Scan(ctx, &queued)
	_ = s.DB.NewRaw("SELECT count(*) FROM notifications WHERE license_id = ?", lic.ID).Scan(ctx, &recorded)
	if queued != 0 || recorded != 0 {
		t.Fatalf("switched off: queued=%d recorded=%d, want nothing", queued, recorded)
	}
}

// Dunning records each step it sends. Switched off it must record
// nothing, so switching back on still sends the step that is due.
func TestDunningRespectsTheSwitch(t *testing.T) {
	s := openNotifyStore(t)
	ctx := context.Background()
	suffix := time.Now().Format("150405.000000")
	prod := &model.Product{Name: "Dunning", Slug: "dunning-sw-" + suffix, Type: "desktop"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	plan := &model.Plan{ProductID: prod.ID, Name: "P", Slug: "dunning-sw-" + suffix, LicenseType: "subscription", LicenseModel: "standard", BillingInterval: "month"}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	pastDue := time.Now().Add(-15 * 24 * time.Hour)
	email := "dunning-sw-" + suffix + "@example.com"
	lic := &model.License{ProductID: prod.ID, PlanID: plan.ID, Email: email, LicenseKey: "KEY-dunning-sw-" + suffix,
		Status: model.StatusPastDue, PastDueAt: &pastDue}
	if err := s.CreateLicense(ctx, lic); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = s.DB.NewRaw("UPDATE licenses SET status = 'expired' WHERE id = ?", lic.ID).Exec(context.Background())
		_, _ = s.DB.NewRaw("DELETE FROM email_queue WHERE to_addr = ?", email).Exec(context.Background())
	})
	c := NewExpiryChecker(s, NewEmailService("127.0.0.1", "1", "", "", "keygate@test.local", slog.Default(), s), nil, slog.Default())
	recorded := func() int {
		t.Helper()
		var n int
		if err := s.DB.NewRaw("SELECT count(*) FROM notifications WHERE license_id = ? AND tag LIKE 'dunning_%'", lic.ID).Scan(ctx, &n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	setNotify(t, s, "payment_failed", "false")
	c.SendPaymentFailureReminders(ctx)
	if n := recorded(); n != 0 {
		t.Fatalf("switched off: %d dunning steps recorded, want none", n)
	}

	setNotify(t, s, "payment_failed", "")
	c.SendPaymentFailureReminders(ctx)
	var final int
	if err := s.DB.NewRaw("SELECT count(*) FROM notifications WHERE license_id = ? AND tag LIKE 'dunning_final:%'", lic.ID).Scan(ctx, &final); err != nil {
		t.Fatal(err)
	}
	if final != 1 {
		t.Fatalf("switched back on: dunning_final recorded %d times, want 1", final)
	}
}
