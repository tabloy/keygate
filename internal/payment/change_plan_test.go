package payment

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/tabloy/keygate/internal/model"
)

// A plan change in the portal is always prorated. The customer could
// once send "prorate": false and move to a dearer plan for the rest of
// the period without paying the difference, then move back before the
// renewal.
func TestChangePlan_AlwaysProrates(t *testing.T) {
	s, ctx := openStore(t)
	t.Cleanup(func() { s.Close() })
	suffix := time.Now().Format("150405.000000")
	prod := &model.Product{Name: "Prorate", Slug: "prorate-" + suffix, Type: "saas"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatalf("create product: %v", err)
	}
	mkPlan := func(name string) *model.Plan {
		p := &model.Plan{ProductID: prod.ID, Name: name, Slug: "pr-" + name + "-" + suffix, LicenseType: "subscription",
			LicenseModel: "standard", StripePriceID: "price_pr_" + name + "_" + suffix, Active: true}
		if err := s.CreatePlan(ctx, p); err != nil {
			t.Fatalf("create plan %s: %v", name, err)
		}
		return p
	}
	basic, business := mkPlan("basic"), mkPlan("business")
	subID := "sub_pr_" + suffix
	lic := &model.License{ProductID: prod.ID, PlanID: basic.ID, Email: "prorate-" + suffix + "@example.com",
		LicenseKey: "KEY-pr-" + suffix, Status: model.StatusActive, PaymentProvider: "stripe", StripeSubscriptionID: subID}
	if err := s.CreateLicense(ctx, lic); err != nil {
		t.Fatalf("create licence: %v", err)
	}

	var mu sync.Mutex
	var behaviours []string
	subJSON := fmt.Sprintf(`{"id":%q,"object":"subscription","status":"active","items":{"object":"list","data":[{"id":"si_pr","object":"subscription_item"}]}}`, subID)
	stubStripe(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/subscriptions/"+subID {
			http.Error(w, "unexpected", http.StatusNotFound)
			return
		}
		if r.Method == http.MethodPost {
			_ = r.ParseForm()
			mu.Lock()
			behaviours = append(behaviours, r.PostForm.Get("proration_behavior"))
			mu.Unlock()
		}
		_, _ = io.WriteString(w, subJSON)
	})
	gin.SetMode(gin.TestMode)
	for _, body := range []string{
		fmt.Sprintf(`{"license_id":%q,"new_price_id":%q,"prorate":false}`, lic.ID, business.StripePriceID),
		fmt.Sprintf(`{"license_id":%q,"new_price_id":%q}`, lic.ID, basic.StripePriceID),
	} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/portal/subscription/change-plan", bytes.NewBufferString(body))
		c.Request.Header.Set("Content-Type", "application/json")
		c.Set("email", lic.Email)
		h := &StripeHandler{Store: s}
		h.ChangePlan(c)
		if w.Code != http.StatusOK {
			t.Fatalf("change plan = %d: %s", w.Code, w.Body.String())
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(behaviours) != 2 || behaviours[0] != "create_prorations" || behaviours[1] != "create_prorations" {
		t.Fatalf("proration_behavior sent to Stripe = %v, want create_prorations both times", behaviours)
	}
}
