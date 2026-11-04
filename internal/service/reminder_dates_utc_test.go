package service

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
	"github.com/tabloy/keygate/pkg/apperr"
)

func TestUTCDate(t *testing.T) {
	auckland := time.FixedZone("NZDT", 13*3600)
	losAngeles := time.FixedZone("PST", -8*3600)
	instant := time.Date(2026, 11, 13, 23, 30, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		in   time.Time
	}{
		{"utc", instant},
		{"ahead of utc", instant.In(auckland)},
		{"behind utc", instant.In(losAngeles)},
	} {
		if got := utcDate(tc.in); got != "2026-11-13" {
			t.Errorf("%s: utcDate = %s, want 2026-11-13", tc.name, got)
		}
	}
}

// openAucklandStore opens the test database with a session time zone of
// Pacific/Auckland (UTC+13 in November), the way a server that is not on
// UTC would; timestamps scanned from it carry that zone.
func openAucklandStore(t *testing.T) *store.Store {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("skipping integration test: TEST_DATABASE_URL not set")
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	s, err := store.New(dsn + sep + "timezone=Pacific/Auckland")
	if err != nil {
		t.Skipf("skipping integration test: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// The expiry reminder prints the day the license ends. It must be the UTC
// day, as the updates-ending reminder already prints, not the day in the
// database session's zone.
func TestExpiryReminder_DateIsUTCWhateverTheSessionZone(t *testing.T) {
	f := newReminderFixture(t)
	ctx := context.Background()
	nz := openAucklandStore(t)

	// Expires 23:30 UTC two days from now: already the next calendar day
	// in Auckland.
	now := time.Now().UTC()
	target := time.Date(now.Year(), now.Month(), now.Day()+2, 23, 30, 0, 0, time.UTC)
	lic := f.license(target.Sub(now))

	fresh, err := nz.FindLicenseByID(ctx, lic.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantDay := target.Format("2006-01-02")
	wrongDay := fresh.ValidUntil.Format("2006-01-02")
	if wantDay == wrongDay {
		t.Fatalf("test setup: the session zone should name a different day than UTC, both say %s", wantDay)
	}

	c := NewExpiryChecker(nz, f.c.email, nil, slog.Default())
	if !c.remindExpiring(ctx, fresh, DefaultReminderDays, time.Now()) {
		t.Fatal("no reminder queued")
	}
	var body string
	if err := f.s.DB.NewRaw("SELECT body FROM email_queue WHERE to_addr = ?", lic.Email).Scan(ctx, &body); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, wantDay) || strings.Contains(body, wrongDay) {
		t.Fatalf("mail should say the license expires on %s (UTC), not %s:\n%s", wantDay, wrongDay, body)
	}
}

// The UPDATES_EXPIRED error names the day the update period ended; it must
// be the UTC day too.
func TestGenerateDownload_UpdatesExpiredNamesUTCDate(t *testing.T) {
	s := openAucklandStore(t)
	if err := s.RunMigrations("../../db/migrations"); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	ctx := context.Background()
	suffix := time.Now().Format("150405.000")

	prod := &model.Product{Name: "UTC Gate", Slug: "utcgate-" + suffix, Type: "desktop", FeedLicenseRequired: true}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	plan := &model.Plan{ProductID: prod.ID, Name: "Perp", Slug: "utcgate-" + suffix, LicenseType: "perpetual", LicenseModel: "standard", MaxActivations: 3}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	rel := &model.Release{ProductID: prod.ID, Version: "2.0.0", Channel: model.ReleaseChannelStable}
	if err := s.CreateRelease(ctx, rel); err != nil {
		t.Fatal(err)
	}
	art := &model.ReleaseArtifact{ReleaseID: rel.ID, Platform: "darwin-arm64"}
	if err := s.CreateArtifact(ctx, art); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateArtifactFile(ctx, art.ID, "k/2.0.0", 10, strings.Repeat("ab", 32), "application/octet-stream"); err != nil {
		t.Fatal(err)
	}
	if err := s.PublishRelease(ctx, rel.ID, false, ""); err != nil {
		t.Fatal(err)
	}

	// Updates ended 23:30 UTC ten days ago; the release came out after that.
	now := time.Now().UTC()
	ended := time.Date(now.Year(), now.Month(), now.Day()-10, 23, 30, 0, 0, time.UTC)
	if _, err := s.DB.NewRaw("UPDATE releases SET published_at = ? WHERE id = ?", ended.Add(48*time.Hour), rel.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	lic := &model.License{ProductID: prod.ID, PlanID: plan.ID, Email: "utc-" + suffix + "@example.com",
		LicenseKey: "KEY-utc-" + suffix, Status: model.StatusActive, UpdatesUntil: &ended}
	if err := s.CreateLicense(ctx, lic); err != nil {
		t.Fatal(err)
	}
	fresh, err := s.FindLicenseByID(ctx, lic.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantDay := ended.Format("2006-01-02")
	wrongDay := fresh.UpdatesUntil.Format("2006-01-02")
	if wantDay == wrongDay {
		t.Fatalf("test setup: the session zone should name a different day than UTC, both say %s", wantDay)
	}

	svc := NewReleaseService(ReleaseServiceConfig{Store: s, Logger: slog.Default()})
	_, err = svc.GenerateDownload(ctx, DownloadInput{LicenseKey: lic.LicenseKey, ProductID: prod.ID, Platform: "darwin-arm64", Version: "2.0.0"})
	var ae *apperr.AppError
	if !errors.As(err, &ae) || ae.Code != "UPDATES_EXPIRED" {
		t.Fatalf("want UPDATES_EXPIRED, got %v", err)
	}
	if !strings.Contains(ae.Message, wantDay) || strings.Contains(ae.Message, wrongDay) {
		t.Fatalf("error should name %s (UTC), not %s: %s", wantDay, wrongDay, ae.Message)
	}
}
