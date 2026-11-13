package service

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"testing"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
	"github.com/tabloy/keygate/pkg/apperr"
)

func TestQuotaReadsDistinguishMissingCounterFromUnavailable(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	s, err := store.New(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	ctx := context.Background()
	prod := &model.Product{Name: "Quota read", Slug: "quota-read-" + store.NewID(), Type: "desktop"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	plan := &model.Plan{ProductID: prod.ID, Name: "Quota", Slug: "quota", LicenseType: "perpetual", LicenseModel: "standard"}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	for _, e := range []*model.Entitlement{{PlanID: plan.ID, Feature: "requests", ValueType: "quota", Value: "10", QuotaPeriod: "monthly"}, {PlanID: plan.ID, Feature: "export", ValueType: "bool", Value: "true"}} {
		if err := s.CreateEntitlement(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	lic := &model.License{ProductID: prod.ID, PlanID: plan.ID, Email: "quota@example.test", LicenseKey: "KG-QUOTA-" + store.NewID(), Status: model.StatusActive}
	if err := s.CreateLicense(ctx, lic); err != nil {
		t.Fatal(err)
	}
	usage := NewUsageService(s, nil, nil, slog.Default(), 0)
	entitlements := NewEntitlementService(s, slog.Default())
	check := func(used int64) {
		t.Helper()
		q, err := usage.GetQuotaStatus(ctx, lic.LicenseKey, "requests", "")
		if err != nil || q.Used != used || q.Remaining != 10-used {
			t.Fatalf("quota: %+v, %v", q, err)
		}
		e, err := entitlements.Check(ctx, CheckInput{LicenseKey: lic.LicenseKey, Feature: "requests"})
		if err != nil {
			t.Fatal(err)
		}
		f := e.Features["requests"]
		if f.Used == nil || f.Remaining == nil || *f.Used != used || *f.Remaining != 10-used {
			t.Fatalf("entitlement: %+v", f)
		}
	}
	check(0)
	if _, _, err := s.IncrementUsageCounterWithLimit(ctx, lic.ID, "requests", "monthly", store.CurrentPeriodKey("monthly"), 8, 10); err != nil {
		t.Fatal(err)
	}
	check(8)
	if _, err := s.DB.ExecContext(ctx, "ALTER TABLE usage_counters RENAME TO quota_read_failure_counters"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := s.DB.ExecContext(ctx, "ALTER TABLE quota_read_failure_counters RENAME TO usage_counters"); err != nil {
			t.Error(err)
		}
	})
	for name, fn := range map[string]func() error{"quota": func() error { _, err := usage.GetQuotaStatus(ctx, lic.LicenseKey, "requests", ""); return err }, "entitlements": func() error {
		_, err := entitlements.Check(ctx, CheckInput{LicenseKey: lic.LicenseKey, Feature: "requests"})
		return err
	}} {
		var app *apperr.AppError
		if err := fn(); !errors.As(err, &app) || app.Status != 500 {
			t.Errorf("%s: error %v, want internal error", name, err)
		}
	}
	if e, err := entitlements.Check(ctx, CheckInput{LicenseKey: lic.LicenseKey, Feature: "export"}); err != nil || !e.Features["export"].Enabled {
		t.Fatalf("unrelated boolean feature: %+v, %v", e, err)
	}
}
