package payment

import (
	"strings"
	"testing"

	"github.com/tabloy/keygate/internal/model"
)

// A key bought through Stripe starts with the prefix of its product.
func TestFulfillCheckout_UsesProductPrefix(t *testing.T) {
	s, ctx := openStore(t)
	defer s.Close()
	plan := seedPlan(t, s, ctx, "prefix", "perpetual")
	if _, err := s.DB.NewUpdate().Model((*model.Product)(nil)).Set("license_prefix = 'SHOP'").
		Where("id = ?", plan.ProductID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	h := &StripeHandler{Store: s}
	email := "prefix-" + plan.Slug + "@example.com"
	if !fulfilOK(h, ctx, email, "", "", "", map[string]string{"plan_id": plan.ID, "session_id": "cs_test_prefix_" + plan.Slug}, "test") {
		t.Fatal("checkout not fulfilled")
	}
	lics, err := s.ListLicensesByEmail(ctx, email)
	if err != nil || len(lics) != 1 {
		t.Fatalf("licenses after checkout: %d, %v", len(lics), err)
	}
	if key := s.DecryptLicenseKey(lics[0]); !strings.HasPrefix(key, "SHOP-") {
		t.Fatalf("key bought through Stripe = %s, want SHOP-", key)
	}
}
