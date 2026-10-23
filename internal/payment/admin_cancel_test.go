package payment

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/service"
)

// The admin "cancel and revoke" puts an upgraded licence back on its
// free plan, as a refund in Stripe does, rather than leaving the caller
// to revoke a licence the customer had before paying.
func TestCancelForAdmin_RevertsAnUpgrade(t *testing.T) {
	f := newUpgradeFixture(t, "admcancel")
	sessionID, subID := "cs_adm_"+f.tag, "sub_adm_"+f.tag
	f.applySub(t, sessionID, subID)
	canceled := 0
	stubStripe(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && r.URL.Path == "/v1/subscriptions/"+subID {
			canceled++
			fmt.Fprintf(w, `{"id":"%s","object":"subscription","status":"canceled"}`, subID)
			return
		}
		http.Error(w, "unexpected", http.StatusNotFound)
	})
	reverted, err := f.h.CancelForAdmin(f.ctx, f.reload(t))
	if err != nil || !reverted {
		t.Fatalf("CancelForAdmin = %v, %v; want reverted", reverted, err)
	}
	if canceled != 1 {
		t.Fatalf("subscription cancelled %d times, want 1", canceled)
	}
	f.assertBackOnFree(t, sessionID, upgradeEndAdmin)
}

// For a licence no upgrade moved, the subscription is cancelled and the
// caller revokes; a cancel Stripe cannot confirm is an error, so the
// caller changes nothing.
func TestCancelForAdmin_PlainSubscription(t *testing.T) {
	f := newUpgradeFixture(t, "admplain")
	subID := "sub_plain_" + f.tag
	l := f.reload(t)
	l.StripeSubscriptionID, l.PaymentProvider = subID, "stripe"
	if err := f.s.UpdateLicense(f.ctx, l, "stripe_subscription_id", "payment_provider"); err != nil {
		t.Fatal(err)
	}
	fail := true
	stubStripe(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodDelete && fail:
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":{"type":"api_error","message":"boom"}}`)
		case r.Method == http.MethodDelete:
			fmt.Fprintf(w, `{"id":"%s","object":"subscription","status":"canceled"}`, subID)
		default: // the follow-up read: still active
			fmt.Fprintf(w, `{"id":"%s","object":"subscription","status":"active"}`, subID)
		}
	})
	if reverted, err := f.h.CancelForAdmin(f.ctx, f.reload(t)); err == nil || reverted {
		t.Fatalf("failed cancel = %v, %v; want an error", reverted, err)
	}
	fail = false
	if reverted, err := f.h.CancelForAdmin(f.ctx, f.reload(t)); err != nil || reverted {
		t.Fatalf("cancel = %v, %v; want cancelled, not reverted", reverted, err)
	}
}

// An unpaid subscription is never deleted, so the update that reports
// it is the only moment its licence ends: license.canceled goes out
// then, once, however often Stripe repeats the update.
func TestUnpaidSubscriptionSendsCanceledOnce(t *testing.T) {
	f := newUpgradeFixture(t, "unpaidev")
	var mu sync.Mutex
	var got []map[string]any
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Event string         `json:"event"`
			Data  map[string]any `json:"data"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Event == model.EventLicenseCanceled {
			mu.Lock()
			got = append(got, body.Data)
			mu.Unlock()
		}
	}))
	defer receiver.Close()
	if err := f.s.CreateWebhook(f.ctx, &model.Webhook{ProductID: f.lic.ProductID, URL: receiver.URL, Secret: "whsec_t",
		Active: true, Events: []string{model.EventLicenseCanceled}}); err != nil {
		t.Fatal(err)
	}
	f.h.WebhookSvc = service.NewWebhookService(f.s, slog.Default(), 2*time.Second, 1, true)

	subID := "sub_unpaid_" + f.tag
	l := f.reload(t)
	l.StripeSubscriptionID, l.PaymentProvider = subID, "stripe"
	if err := f.s.UpdateLicense(f.ctx, l, "stripe_subscription_id", "payment_provider"); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := f.h.onSubscriptionUpdated(f.ctx, fmt.Appendf(nil, `{"id":"%s","status":"unpaid"}`, subID)); err != nil {
			t.Fatalf("unpaid update: %v", err)
		}
	}
	if s := f.reload(t).Status; s != model.StatusCanceled {
		t.Fatalf("status %q, want canceled", s)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond) // room for a second one to arrive
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0]["license_id"] != f.lic.ID || got[0]["reason"] != "subscription_unpaid" {
		t.Fatalf("license.canceled deliveries = %v, want one for %s with reason subscription_unpaid", got, f.lic.ID)
	}
}
