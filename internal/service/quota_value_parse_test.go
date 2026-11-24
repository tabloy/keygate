package service

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
)

// A quota's value is validated as a non-negative integer when written
// through the admin API, but the store layer itself trusts its caller:
// a direct DB write, import, or legacy row could still leave a quota
// entitlement with a non-numeric value. That must not be read as
// "unlimited" (limit 0 means unlimited to quotaRemaining) - it must
// surface as an error, in RecordUsage, GetQuotaStatus, and Check alike.
func TestMalformedQuotaValueIsAnErrorNotUnlimited(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("skipping integration test: TEST_DATABASE_URL not set")
	}
	s, err := store.New(dsn)
	if err != nil {
		t.Skipf("skipping integration test: %v", err)
	}
	defer s.Close()
	if err := s.RunMigrations("../../db/migrations"); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	ctx := context.Background()
	suffix := time.Now().Format("150405.000000")

	prod := &model.Product{Name: "Bad Quota", Slug: "bad-quota-" + suffix, Type: "desktop"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	plan := &model.Plan{ProductID: prod.ID, Name: "Base", Slug: "bad-quota-" + suffix, LicenseType: "perpetual", LicenseModel: "standard"}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	// Bypasses the admin handler's normalizeFeatureValue, as a direct
	// DB write or legacy row would.
	ent := &model.Entitlement{PlanID: plan.ID, Feature: "api_calls", ValueType: "quota", Value: "not-a-number", QuotaPeriod: "monthly"}
	if err := s.CreateEntitlement(ctx, ent); err != nil {
		t.Fatal(err)
	}
	key := "KG-BADQ-" + suffix
	lic := &model.License{ProductID: prod.ID, PlanID: plan.ID, Email: "bad-quota-" + suffix + "@example.com", LicenseKey: key, Status: model.StatusActive}
	if err := s.CreateLicense(ctx, lic); err != nil {
		t.Fatal(err)
	}

	wh := NewWebhookService(s, slog.Default(), 10*time.Second, 5, false)
	us := NewUsageService(s, wh, nil, slog.Default(), 0.8)

	if _, err := us.RecordUsage(ctx, RecordUsageInput{LicenseKey: key, Feature: "api_calls", Quantity: 1}); err == nil {
		t.Fatal("RecordUsage with a malformed quota value = nil error, want it refused")
	}
	if _, err := us.GetQuotaStatus(ctx, key, "api_calls", ""); err == nil {
		t.Fatal("GetQuotaStatus with a malformed quota value = nil error, want it refused")
	}

	es := NewEntitlementService(s, slog.Default())
	if _, err := es.Check(ctx, CheckInput{LicenseKey: key}); err == nil {
		t.Fatal("Check with a malformed quota value = nil error, want it refused")
	}
}
