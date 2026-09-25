package payment

import (
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/model"
)

// TestConcurrentDeliveriesProduceOneLicense hammers one paid checkout
// session from every direction at once: the same webhook event
// delivered repeatedly, distinct events for the same session
// (completed + async_payment_succeeded), and the success-page
// verification. Exactly one license, one done marker, no pending
// marker left behind.
func TestConcurrentDeliveriesProduceOneLicense(t *testing.T) {
	s, ctx := openStore(t)
	defer s.Close()
	plan := seedPlan(t, s, ctx, "concur", "perpetual")
	sessionID := "cs_test_concur_" + plan.Slug
	email := "concur-" + plan.Slug + "@example.com"
	stubStripe(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/checkout/sessions/"+sessionID {
			time.Sleep(20 * time.Millisecond) // widen the race window
			fmt.Fprintf(w, `{"id":"%s","object":"checkout.session","mode":"payment","status":"complete","payment_status":"paid","payment_intent":"pi_concur","customer_details":{"email":"%s"},"metadata":{"plan_id":"%s"}}`, sessionID, email, plan.ID)
			return
		}
		http.Error(w, `{"error":{"type":"invalid_request_error","message":"no such object"}}`, http.StatusNotFound)
	})
	h := &StripeHandler{Store: s}
	secret := "whsec_concur_" + plan.Slug
	h.SetWebhookSecret(secret)
	obj := map[string]any{"id": sessionID, "object": "checkout.session", "mode": "payment", "payment_status": "paid",
		"customer_details": map[string]any{"email": email}, "metadata": map[string]any{"plan_id": plan.ID}}

	var wg sync.WaitGroup
	codes := make(chan int, 64)
	for i := range 8 {
		wg.Add(3)
		go func() {
			defer wg.Done()
			c, _ := signedWebhook(t, h, secret, "evt_same_"+plan.Slug, "checkout.session.completed", obj)
			codes <- c
		}()
		go func(i int) {
			defer wg.Done()
			c, _ := signedWebhook(t, h, secret, fmt.Sprintf("evt_async_%s_%d", plan.Slug, i), "checkout.session.async_payment_succeeded", obj)
			codes <- c
		}(i)
		go func() {
			defer wg.Done()
			// success page: fetches the session from (stubbed) Stripe and fulfils
			sess, _ := fetchSession(sessionID)
			if sess != nil {
				h.fulfillSession(ctx, sess, "verify")
			}
		}()
	}
	wg.Wait()
	close(codes)
	for c := range codes {
		if c != http.StatusOK && c != http.StatusServiceUnavailable {
			t.Fatalf("unexpected webhook status %d", c)
		}
	}

	lics, _ := s.ListLicensesByEmail(ctx, email)
	if len(lics) != 1 || lics[0].StripeCheckoutSessionID != sessionID {
		t.Fatalf("expected exactly one license for the session, got %d", len(lics))
	}
	if !s.IsEventProcessed(ctx, fulfilledSessionProvider, sessionID) {
		t.Fatalf("done marker missing")
	}
	// Whatever raced, the pending sync must find nothing left to do.
	h.SyncPendingCheckouts(ctx)
	rows, _ := s.ListPendingCheckoutSessions(ctx, time.Hour, nil, 10000)
	for _, r := range rows {
		if r.SessionID == sessionID {
			t.Fatalf("session still pending after everything settled")
		}
	}
	if lics, _ = s.ListLicensesByEmail(ctx, email); len(lics) != 1 {
		t.Fatalf("pending sync duplicated the license")
	}
}

// TestConcurrentRefundEventsRevokeOnce: the same refund event landing
// several times at once revokes once and audits once.
func TestConcurrentRefundEventsRevokeOnce(t *testing.T) {
	s, ctx := openStore(t)
	defer s.Close()
	plan := seedPlan(t, s, ctx, "refconc", "perpetual")
	lic := &model.License{ProductID: plan.ProductID, PlanID: plan.ID, Email: "rc-" + plan.Slug + "@example.com", LicenseKey: "KEY-rc-" + plan.Slug,
		Status: model.StatusActive, PaymentProvider: "stripe", StripePaymentIntentID: "pi_rc_" + plan.Slug}
	if err := s.CreateLicense(ctx, lic); err != nil {
		t.Fatalf("seed: %v", err)
	}
	h := &StripeHandler{Store: s}
	secret := "whsec_rc_" + plan.Slug
	h.SetWebhookSecret(secret)
	charge := map[string]any{"id": "ch_rc_" + plan.Slug, "object": "charge", "payment_intent": lic.StripePaymentIntentID, "refunded": true}
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() { defer wg.Done(); signedWebhook(t, h, secret, "evt_rc_"+plan.Slug, "charge.refunded", charge) }()
	}
	wg.Wait()
	got, _ := s.FindLicenseByID(ctx, lic.ID)
	if got.Status != model.StatusRevoked {
		t.Fatalf("not revoked: %s", got.Status)
	}
	audits := auditCount(s, ctx, lic.ID, "revoked")
	time.Sleep(200 * time.Millisecond)
	s.DB.NewRaw("SELECT count(*) FROM audit_logs WHERE entity_id = ? AND action = 'revoked'", lic.ID).Scan(ctx, &audits)
	if audits != 1 {
		t.Fatalf("expected one revoke audit, got %d", audits)
	}
}
