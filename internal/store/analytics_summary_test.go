package store_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
)

// With a status picked on the Analytics page, the summary's devices are
// those of the licences in that status, so their average is over those
// licences too. It was over every licence: the per-status counts leave
// the status filter out, and the average divided by their total.
func TestAnalyticsSummaryAverageFollowsStatusFilter(t *testing.T) {
	s := setupTestDB(t)
	t.Cleanup(func() { s.Close() })
	ctx := context.Background()

	suffix := strings.ReplaceAll(time.Now().Format("150405.000000"), ".", "")
	prod := &model.Product{Name: "Summary", Slug: "summary-" + suffix, Type: "desktop"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	plan := &model.Plan{ProductID: prod.ID, Name: "Pro", Slug: "summary-pro-" + suffix, LicenseType: "perpetual", LicenseModel: "standard"}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	n := 0
	licence := func(status string, devices int) {
		t.Helper()
		n++
		l := &model.License{ProductID: prod.ID, PlanID: plan.ID, Email: "summary-" + suffix + "@example.com",
			LicenseKey: "KG-SUM-" + suffix + "-" + string(rune('a'+n)), Status: status}
		if err := s.CreateLicense(ctx, l); err != nil {
			t.Fatal(err)
		}
		for i := range devices {
			if err := s.CreateActivation(ctx, &model.Activation{LicenseID: l.ID,
				Identifier: l.LicenseKey + "-device-" + string(rune('a'+i)), IdentifierType: "device"}); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Expired licences hold no devices: the hourly sweep removes them.
	licence(model.StatusActive, 3)
	licence(model.StatusTrialing, 1)
	licence(model.StatusExpired, 0)
	licence(model.StatusExpired, 0)
	licence(model.StatusExpired, 0)

	for _, tc := range []struct {
		status  string
		devices int
		avg     float64
	}{
		{"", 4, 0.8},
		{model.StatusActive, 3, 3},
		{model.StatusTrialing, 1, 1},
		{model.StatusExpired, 0, 0},
	} {
		sum, err := s.GetAnalyticsSummary(ctx, store.AnalyticsFilter{ProductID: prod.ID, Status: tc.status})
		if err != nil {
			t.Fatal(err)
		}
		if sum.TotalActivations != tc.devices || sum.AvgActivationsPerLicense != tc.avg {
			t.Errorf("status %q: %d devices, %.2f a licence; want %d, %.2f",
				tc.status, sum.TotalActivations, sum.AvgActivationsPerLicense, tc.devices, tc.avg)
		}
		// The per-status counts stay unfiltered, as before.
		if sum.TotalLicenses != 5 || sum.ActiveLicenses != 1 || sum.TrialingLicenses != 1 || sum.ExpiredLicenses != 3 {
			t.Errorf("status %q: counts %d total, %d active, %d trialing, %d expired; want 5, 1, 1, 3",
				tc.status, sum.TotalLicenses, sum.ActiveLicenses, sum.TrialingLicenses, sum.ExpiredLicenses)
		}
	}
}
