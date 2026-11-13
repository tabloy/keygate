package handler

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
)

func TestFilteredPlanCandidatesMatchFallbackRules(t *testing.T) {
	f := newAPIFixture(t)
	f.prodA.FeedLicenseRequired = true
	if err := f.s.UpdateProduct(f.ctx, f.prodA); err != nil {
		t.Fatal(err)
	}
	candidates := []*model.Plan{
		{Name: "candidate-free", LicenseType: "subscription", Active: true},
		{Name: "candidate-hidden", LicenseType: "subscription", Active: false},
		{Name: "candidate-paid", LicenseType: "subscription", StripePriceID: "price_policy_" + f.suffix, Active: true},
		{Name: "candidate-trial", LicenseType: "trial", TrialDays: 14, Active: true},
		{Name: "candidate-perseat", LicenseType: "subscription", PerSeat: true, MinQuantity: 1, MaxQuantity: 10, Active: true},
		{Name: "candidate-updates", LicenseType: "perpetual", UpdatesDays: 30, Active: true},
		{Name: "candidate-renewal", LicenseType: "perpetual", RenewalDays: 30, StripeRenewalPriceID: "price_policy_renew_" + f.suffix, Active: true},
	}
	expected := map[string]bool{}
	for _, plan := range candidates {
		plan.ProductID = f.prodA.ID
		plan.Slug = plan.Name + "-" + f.suffix
		plan.LicenseModel = "standard"
		plan.MaxActivations = 1
		if plan.LicenseType == "subscription" {
			plan.BillingInterval = "month"
		}
		if err := f.s.CreatePlan(f.ctx, plan); err != nil {
			t.Fatalf("create %s: %v", plan.Name, err)
		}
		if plan.TrialFallbackRefusal(f.prodA.ID) == "" {
			expected[plan.ID] = true
		}
	}
	filter := store.PlanListFilter{FallbackEligible: true}
	all, total, err := f.s.ListPlans(f.ctx, f.prodA.ID, "candidate-", store.Page{Limit: 20}, filter)
	if err != nil {
		t.Fatal(err)
	}
	if total != len(expected) || len(all) != len(expected) {
		t.Fatalf("eligible count %d/%d, want %d", len(all), total, len(expected))
	}
	for _, plan := range all {
		if !expected[plan.ID] {
			t.Fatalf("ineligible candidate: %s", plan.Name)
		}
	}
	page, total, err := f.s.ListPlans(f.ctx, f.prodA.ID, "candidate-", store.Page{Limit: 1, Offset: 1}, filter)
	if err != nil || len(page) != 1 || total != len(expected) {
		t.Fatalf("filtered pagination: rows=%d total=%d err=%v", len(page), total, err)
	}
	filter.ExcludeID = candidates[0].ID
	all, total, err = f.s.ListPlans(f.ctx, f.prodA.ID, "candidate-", store.Page{Limit: 20}, filter)
	if err != nil || len(all) != 1 || total != 1 || all[0].ID != candidates[1].ID {
		t.Fatalf("exclude/hidden candidate: rows=%d total=%d err=%v", len(all), total, err)
	}
	h := &AdminHandler{Store: f.s}
	path := "/admin/plans?product_id=" + url.QueryEscape(f.prodA.ID) + "&fallback_eligible=true&exclude_id=" + url.QueryEscape(candidates[0].ID) + "&search=candidate-&limit=1"
	code, out := f.call(h.ListPlans, http.MethodGet, path, "", nil)
	if code != 200 {
		t.Fatalf("filtered handler: %d %v", code, out)
	}
	data := out["data"].(map[string]any)
	if data["total"] != float64(1) || len(data["plans"].([]any)) != 1 {
		t.Fatalf("filtered metadata: %v", data["total"])
	}
}

func TestProductMaintenanceReadIncludesFiniteLicenses(t *testing.T) {
	f := newAPIFixture(t)
	h := &AdminHandler{Store: f.s}
	read := func(include bool) map[string]any {
		t.Helper()
		path := "/admin/products/" + f.prodA.ID
		if include {
			path += "?include_maintenance=true"
		}
		code, out := f.call(h.GetProduct, http.MethodGet, path, "", nil, gin.Param{Key: "id", Value: f.prodA.ID})
		if code != 200 {
			t.Fatalf("product read: %d %v", code, out)
		}
		return out["data"].(map[string]any)
	}
	if _, present := read(false)["has_maintenance"]; present {
		t.Fatal("ordinary product contract changed")
	}
	if read(true)["has_maintenance"] != false {
		t.Fatal("empty maintenance requirement was not authoritative")
	}
	f.prodA.FeedLicenseRequired = true
	if err := f.s.UpdateProduct(f.ctx, f.prodA); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.DB.NewRaw("UPDATE licenses SET updates_until = now() WHERE id = ?", f.licA.ID).Exec(f.ctx); err != nil {
		t.Fatal(err)
	}
	if read(true)["has_maintenance"] != true {
		t.Fatal("finite license was omitted without a bounded plan")
	}
}
