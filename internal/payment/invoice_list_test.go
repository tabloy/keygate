package payment

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/tabloy/keygate/internal/model"
)

// The portal shows a paid invoice's total: amount_paid is 0 for one
// settled outside Stripe or from the customer's balance. A total of 0 or
// below is a real amount and has to reach the portal as it is.
func TestListInvoicesReturnsTheBilledTotal(t *testing.T) {
	s, ctx := openStore(t)
	defer s.Close()
	plan := seedPlan(t, s, ctx, "invoicetotal", "subscription")
	suffix := time.Now().Format("150405.000000")
	lic := &model.License{ProductID: plan.ProductID, PlanID: plan.ID, Email: "invoicetotal-" + suffix + "@example.com",
		LicenseKey: "KEY-invoicetotal-" + suffix, Status: model.StatusActive, StripeCustomerID: "cus_invoicetotal_" + suffix}
	if err := s.CreateLicense(ctx, lic); err != nil {
		t.Fatal(err)
	}

	stubStripe(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/invoices" {
			http.Error(w, "unexpected", http.StatusNotFound)
			return
		}
		fmt.Fprint(w, `{"object":"list","has_more":false,"url":"/v1/invoices","data":[
			{"id":"in_card","object":"invoice","status":"paid","amount_due":2500,"amount_paid":2500,"total":2500,"currency":"usd"},
			{"id":"in_out_of_band","object":"invoice","status":"paid","amount_due":2400,"amount_paid":0,"total":2400,"currency":"usd"},
			{"id":"in_balance","object":"invoice","status":"paid","amount_due":0,"amount_paid":0,"total":900,"currency":"usd"},
			{"id":"in_zero","object":"invoice","status":"paid","amount_due":0,"amount_paid":0,"total":0,"currency":"usd"},
			{"id":"in_negative","object":"invoice","status":"paid","amount_due":0,"amount_paid":0,"total":-500,"currency":"usd"}
		]}`)
	})

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/portal/subscription/invoices?license_id="+lic.ID, nil)
	c.Set("email", lic.Email)
	(&StripeHandler{Store: s}).ListInvoices(c)
	if w.Code != http.StatusOK {
		t.Fatalf("list = %d: %s", w.Code, w.Body.String())
	}

	var body struct {
		Data struct {
			Invoices []map[string]any `json:"invoices"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	want := map[string]float64{"in_card": 2500, "in_out_of_band": 2400, "in_balance": 900, "in_zero": 0, "in_negative": -500}
	if len(body.Data.Invoices) != len(want) {
		t.Fatalf("got %d invoices, want %d: %s", len(body.Data.Invoices), len(want), w.Body.String())
	}
	for _, inv := range body.Data.Invoices {
		id, _ := inv["id"].(string)
		total, ok := inv["total"].(float64)
		if !ok {
			t.Errorf("%s: no total in the response: %v", id, inv)
			continue
		}
		if total != want[id] {
			t.Errorf("%s: total = %v, want %v", id, total, want[id])
		}
		if _, ok := inv["amount_paid"]; !ok {
			t.Errorf("%s: amount_paid should stay in the response", id)
		}
	}
}
