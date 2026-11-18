package store_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/tabloy/keygate/internal/model"
)

// failLicenseReads makes every query that reads the licenses table fail,
// as a dropped connection or a statement timeout would.
type failLicenseReads struct{}

func (failLicenseReads) BeforeQuery(ctx context.Context, e *bun.QueryEvent) context.Context {
	if strings.Contains(e.Query, `FROM "licenses"`) {
		failed, cancel := context.WithCancel(ctx)
		cancel()
		return failed
	}
	return ctx
}

func (failLicenseReads) AfterQuery(context.Context, *bun.QueryEvent) {}

// A snapshot whose counts fail is not saved: its zeros would stand as
// the day's figures for good, since a day is taken once. The next run
// takes the day again, with the real counts.
func TestSnapshotWithAFailedCountIsNotSaved(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()
	suffix := time.Now().Format("150405.000000000")
	prod := &model.Product{Name: "Snap fail " + suffix, Slug: "snap-fail-" + suffix, Type: "saas"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	plan := &model.Plan{ProductID: prod.ID, Name: "P", Slug: "snap-fail-" + suffix, LicenseType: "perpetual",
		LicenseModel: "standard", Active: true}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(30 * 24 * time.Hour)
	lic := &model.License{ProductID: prod.ID, PlanID: plan.ID, Email: "snap-fail-" + suffix + "@example.com",
		LicenseKey: "KG-SNAP-" + suffix, Status: model.StatusActive, ValidUntil: &future}
	if err := s.CreateLicense(ctx, lic); err != nil {
		t.Fatal(err)
	}
	day := time.Now().Add(-24 * time.Hour)

	healthy := s.DB
	s.DB = healthy.WithQueryHook(failLicenseReads{})
	err := s.TakeSnapshot(ctx, prod.ID, day)
	s.DB = healthy
	if err == nil {
		t.Fatal("TakeSnapshot returned nil with its license counts failing, want the error")
	}

	var rows int
	if err := s.DB.NewRaw("SELECT count(*) FROM analytics_snapshots WHERE product_id = ?", prod.ID).Scan(ctx, &rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("%d snapshot(s) saved from failed counts, want none", rows)
	}

	if err := s.TakeMissingSnapshots(ctx, day); err != nil {
		t.Fatal(err)
	}
	var total, active int
	if err := s.DB.NewRaw("SELECT total_licenses, active_licenses FROM analytics_snapshots WHERE product_id = ?", prod.ID).Scan(ctx, &total, &active); err != nil {
		t.Fatal(err)
	}
	if total != 1 || active != 1 {
		t.Fatalf("snapshot taken on the next run: %d licenses, %d active; want 1 and 1", total, active)
	}
}
