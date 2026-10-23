package service

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/license"
	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
	"github.com/tabloy/keygate/pkg/apperr"
)

// An addon on a license must reach every place features are read: the
// verify answer, the offline token, usage limits and usage status, not
// only the entitlements endpoint. Before licenseFeatures, verify and the token
// carried plan features only and usage enforced the plan's quota.
func TestAddonsReachVerifyTokenAndUsage(t *testing.T) {
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

	prod := &model.Product{Name: "Addons", Slug: "addons-" + suffix, Type: "desktop"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	plan := &model.Plan{ProductID: prod.ID, Name: "Base", Slug: "addons-" + suffix, LicenseType: "perpetual", LicenseModel: "standard"}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	for _, e := range []*model.Entitlement{
		{PlanID: plan.ID, Feature: "export", ValueType: "bool", Value: "false"},
		{PlanID: plan.ID, Feature: "api_calls", ValueType: "quota", Value: "2", QuotaPeriod: "monthly", StripeMeterEventName: "api_calls_meter"},
	} {
		if err := s.CreateEntitlement(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	key := "KG-ADDON-" + suffix
	lic := &model.License{ProductID: prod.ID, PlanID: plan.ID, Email: "addons-" + suffix + "@example.com", LicenseKey: key, Status: model.StatusActive}
	if err := s.CreateLicense(ctx, lic); err != nil {
		t.Fatal(err)
	}
	var addonIDs []string
	for i, a := range []*model.Addon{
		{Feature: "export", ValueType: "bool", Value: "true"},
		// A different period from the plan's: usage and its status must
		// both count against the addon's daily counter.
		{Feature: "api_calls", ValueType: "quota", Value: "5", QuotaPeriod: "daily"},
		{Feature: "sso", ValueType: "bool", Value: "true"},
		{Feature: "exports_per_day", ValueType: "quota", Value: "3", QuotaPeriod: "daily"},
	} {
		a.ProductID, a.Name, a.Slug = prod.ID, a.Feature, a.Feature+"-"+suffix
		if err := s.CreateAddon(ctx, a); err != nil {
			t.Fatalf("addon %d: %v", i, err)
		}
		if _, err := s.AddLicenseAddon(ctx, &model.LicenseAddon{LicenseID: lic.ID, AddonID: a.ID, Enabled: true}); err != nil {
			t.Fatal(err)
		}
		addonIDs = append(addonIDs, a.ID)
	}

	loaded, err := s.FindLicenseByKey(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	ls := &LicenseService{store: s, signingKey: priv}
	raw, features, err := ls.signToken(ctx, loaded, "device-1")
	if err != nil {
		t.Fatal(err)
	}
	tok, err := license.Verify(raw, license.PublicKey(priv))
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]any{"export": true, "sso": true, "api_calls": "5", "exports_per_day": "3"} {
		if features[name] != want {
			t.Errorf("verify feature %s = %v, want %v", name, features[name], want)
		}
		if tok.Features[name] != want {
			t.Errorf("token feature %s = %v, want %v", name, tok.Features[name], want)
		}
	}

	// A real webhook service: crossing the warning threshold dispatches one.
	wh := NewWebhookService(s, slog.Default(), 10*time.Second, 5, false)
	us := NewUsageService(s, wh, nil, slog.Default(), 0.8)
	for i := 1; i <= 5; i++ {
		if _, err := us.RecordUsage(ctx, RecordUsageInput{LicenseKey: key, Feature: "api_calls", Quantity: 1}); err != nil {
			t.Fatalf("use %d of the addon's 5 refused: %v", i, err)
		}
	}
	// The addon raised the limit but billing still goes to the plan's
	// Stripe meter: every recorded use is queued as a metered event.
	var metered int64
	if err := s.DB.NewRaw("SELECT COALESCE(SUM(quantity), 0) FROM metered_billing WHERE license_id = ? AND feature = 'api_calls'", lic.ID).Scan(ctx, &metered); err != nil {
		t.Fatalf("read metered events: %v", err)
	}
	if metered != 5 {
		t.Errorf("metered events for api_calls = %d, want 5: the addon must not drop the plan's Stripe meter", metered)
	}

	_, err = us.RecordUsage(ctx, RecordUsageInput{LicenseKey: key, Feature: "api_calls", Quantity: 1})
	var ae *apperr.AppError
	if !errors.As(err, &ae) || ae.Code != "QUOTA_EXCEEDED" {
		t.Fatalf("use 6 should exceed the addon quota, got %v", err)
	}

	// The status reports the quota that was enforced: the addon's limit
	// and period, and the counter usage went to.
	st, err := us.GetQuotaStatus(ctx, key, "api_calls", "")
	if err != nil {
		t.Fatalf("usage status: %v", err)
	}
	if st.Limit != 5 || st.Used != 5 || st.Remaining != 0 || st.Period != "daily" {
		t.Errorf("api_calls status = %+v, want limit 5, used 5, remaining 0, daily", st)
	}
	// A quota only an addon grants: usage is accepted, so status must not 404.
	if _, err := us.RecordUsage(ctx, RecordUsageInput{LicenseKey: key, Feature: "exports_per_day", Quantity: 1}); err != nil {
		t.Fatalf("addon only quota refused: %v", err)
	}
	st, err = us.GetQuotaStatus(ctx, key, "exports_per_day", "")
	if err != nil {
		t.Fatalf("status of an addon only quota: %v", err)
	}
	if st.Limit != 3 || st.Used != 1 || st.Remaining != 2 {
		t.Errorf("exports_per_day status = %+v, want limit 3, used 1, remaining 2", st)
	}

	// Without the addons the plan's own features apply again.
	for _, id := range addonIDs {
		if err := s.RemoveLicenseAddon(ctx, lic.ID, id); err != nil {
			t.Fatal(err)
		}
	}
	_, features, err = ls.signToken(ctx, loaded, "device-1")
	if err != nil {
		t.Fatal(err)
	}
	if features["export"] != false || features["api_calls"] != "2" || features["sso"] != nil || features["exports_per_day"] != nil {
		t.Errorf("after removing addons got %v, want the plan's features only", features)
	}
}
