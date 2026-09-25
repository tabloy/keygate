package payment

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stripe/stripe-go/v82"
	"github.com/stripe/stripe-go/v82/checkout/session"
	"github.com/stripe/stripe-go/v82/webhook"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
)

// TestFulfillCheckout_OneLicensePerSession pins the fulfilment
// contract: idempotent per checkout session, never per customer.
// A second paid session for the same email+product must produce a
// second license (issue #22); replaying the same session must not.
//
// Runs without Stripe — no customer ID means no Customer lookup,
// and plan_id metadata resolves the plan locally.
func TestFulfillCheckout_OneLicensePerSession(t *testing.T) {
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

	suffix := time.Now().Format("150405.000")
	prod := &model.Product{Name: "Fulfill Test", Slug: "ful-" + suffix, Type: "hybrid"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatalf("create product: %v", err)
	}
	plan := &model.Plan{
		ProductID: prod.ID, Name: "Perpetual", Slug: "perp-" + suffix,
		LicenseType: "perpetual", LicenseModel: "standard",
	}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatalf("create plan: %v", err)
	}

	h := &StripeHandler{Store: s}
	email := "ful-" + suffix + "@example.com"
	meta := func(session string) map[string]string {
		return map[string]string{"plan_id": plan.ID, "session_id": session}
	}

	fulfilOK(h, ctx, email, "", "", "", meta("cs_test_ful_"+suffix+"_1"), "test")
	fulfilOK(h, ctx, email, "", "", "", meta("cs_test_ful_"+suffix+"_1"), "test") // replay
	fulfilOK(h, ctx, email, "", "", "", meta("cs_test_ful_"+suffix+"_2"), "test")

	lics, err := s.ListLicensesByEmail(ctx, email)
	if err != nil {
		t.Fatalf("list licenses: %v", err)
	}
	var n int
	for _, l := range lics {
		if l.ProductID == prod.ID {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("expected 2 licenses (two sessions, one replayed), got %d", n)
	}
}

// TestFulfillCheckout_ResolvesPlanFromLineItems covers sessions that
// were not created by Keygate (Stripe Payment Links): no plan_id
// metadata and, for a one-time payment, no subscription either. The
// price on the session's line items must still map to a plan
// (issue #21). Stripe is stubbed with a local server.
func TestFulfillCheckout_ResolvesPlanFromLineItems(t *testing.T) {
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

	suffix := time.Now().Format("150405.000")
	priceID := "price_li_" + suffix
	sessionID := "cs_test_li_" + suffix

	var hits int32
	stubStripe(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/checkout/sessions/"+sessionID+"/line_items" {
			t.Errorf("unexpected Stripe call: %s %s", r.Method, r.URL.Path)
			http.Error(w, `{"error":{"message":"unexpected"}}`, http.StatusNotFound)
			return
		}
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"object":"list","has_more":false,"url":"%s","data":[{"id":"li_1","object":"item","price":{"id":"%s","object":"price"}}]}`,
			r.URL.Path, priceID)
	})

	prod := &model.Product{Name: "LineItem Test", Slug: "li-" + suffix, Type: "hybrid"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatalf("create product: %v", err)
	}
	plan := &model.Plan{
		ProductID: prod.ID, Name: "Perpetual", Slug: "li-perp-" + suffix,
		LicenseType: "perpetual", LicenseModel: "standard", StripePriceID: priceID,
	}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatalf("create plan: %v", err)
	}

	h := &StripeHandler{Store: s}
	email := "li-" + suffix + "@example.com"
	fulfilOK(h, ctx, email, "", "", "pi_li_"+suffix, map[string]string{"session_id": sessionID}, "test")

	if atomic.LoadInt32(&hits) != 1 {
		t.Fatalf("expected one line_items lookup, got %d", hits)
	}
	lics, err := s.ListLicensesByEmail(ctx, email)
	if err != nil {
		t.Fatalf("list licenses: %v", err)
	}
	if len(lics) != 1 || lics[0].PlanID != plan.ID {
		t.Fatalf("expected one license on plan %s, got %+v", plan.ID, lics)
	}
	if lics[0].StripePaymentIntentID != "pi_li_"+suffix {
		t.Fatalf("payment intent not recorded on license: %q", lics[0].StripePaymentIntentID)
	}
}

// TestChargeRefunded_TargetsPaidLicense: a customer with two licenses
// gets the first purchase refunded. The refund must revoke that
// license, not the newest one. A charge that names only the customer
// (legacy rows without a payment intent) must revoke nothing when the
// customer holds more than one license.
func TestChargeRefunded_TargetsPaidLicense(t *testing.T) {
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

	suffix := time.Now().Format("150405.000")
	prod := &model.Product{Name: "Refund Test", Slug: "ref-" + suffix, Type: "hybrid"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatalf("create product: %v", err)
	}
	plan := &model.Plan{
		ProductID: prod.ID, Name: "Perpetual", Slug: "ref-perp-" + suffix,
		LicenseType: "perpetual", LicenseModel: "standard",
	}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatalf("create plan: %v", err)
	}
	customer := "cus_ref_" + suffix
	mk := func(n string, pi string) *model.License {
		l := &model.License{
			ProductID: prod.ID, PlanID: plan.ID, Email: "ref-" + suffix + "@example.com",
			LicenseKey: "KEY-ref-" + suffix + "-" + n, Status: model.StatusActive,
			PaymentProvider: "stripe", StripeCustomerID: customer, StripePaymentIntentID: pi,
		}
		if err := s.CreateLicense(ctx, l); err != nil {
			t.Fatalf("create license %s: %v", n, err)
		}
		return l
	}
	first := mk("1", "pi_ref_"+suffix+"_1")
	time.Sleep(10 * time.Millisecond) // distinct created_at so "newest" is unambiguous
	second := mk("2", "pi_ref_"+suffix+"_2")

	h := &StripeHandler{Store: s}
	status := func(id string) string {
		l, err := s.FindLicenseByID(ctx, id)
		if err != nil {
			t.Fatalf("find license: %v", err)
		}
		return l.Status
	}

	// Customer-only charge, two licenses: ambiguous → nothing revoked.
	h.onChargeRefunded(ctx, fmt.Appendf(nil, `{"id":"ch_amb_%s","customer":"%s","refunded":true}`, suffix, customer))
	if status(first.ID) != model.StatusActive || status(second.ID) != model.StatusActive {
		t.Fatalf("ambiguous refund revoked a license: first=%s second=%s", status(first.ID), status(second.ID))
	}

	// Refund of the first purchase revokes the first license only.
	h.onChargeRefunded(ctx, fmt.Appendf(nil, `{"id":"ch_1_%s","customer":"%s","payment_intent":"%s","refunded":true}`, suffix, customer, first.StripePaymentIntentID))
	if status(first.ID) != model.StatusRevoked {
		t.Fatalf("first license not revoked: %s", status(first.ID))
	}
	if status(second.ID) != model.StatusActive {
		t.Fatalf("second license wrongly revoked: %s", status(second.ID))
	}
}

// stubStripe points stripe-go at a local server for the duration of
// the test. Handlers must answer every call they expect and fail on
// anything else, so a test also proves which Stripe calls are made.
func stubStripe(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(handler)
	prevKey := stripe.Key
	stripe.Key = "sk_test_stub"
	stripe.SetBackend(stripe.APIBackend, stripe.GetBackendWithConfig(stripe.APIBackend, &stripe.BackendConfig{
		URL: stripe.String(srv.URL), LeveledLogger: &stripe.LeveledLogger{Level: stripe.LevelNull},
	}))
	t.Cleanup(func() {
		stripe.Key = prevKey
		stripe.SetBackend(stripe.APIBackend, nil)
		srv.Close()
	})
}

// TestCheckoutCompleted_WaitsForAsyncPayment: with a delayed payment
// method Stripe completes the session while it is still unpaid and
// sends async_payment_succeeded later. The first event must create
// nothing and must not claim the session; the second must fulfil it.
// The session is a guest checkout, so the email lives only in
// customer_details.
func TestCheckoutCompleted_WaitsForAsyncPayment(t *testing.T) {
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

	suffix := time.Now().Format("150405.000")
	prod := &model.Product{Name: "Async Test", Slug: "async-" + suffix, Type: "hybrid"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatalf("create product: %v", err)
	}
	plan := &model.Plan{
		ProductID: prod.ID, Name: "Perpetual", Slug: "async-perp-" + suffix,
		LicenseType: "perpetual", LicenseModel: "standard",
	}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatalf("create plan: %v", err)
	}

	h := &StripeHandler{Store: s}
	email := "async-" + suffix + "@example.com"
	sessionID := "cs_test_async_" + suffix
	payload := func(status string) []byte {
		return fmt.Appendf(nil, `{"id":"%s","mode":"payment","payment_status":"%s","customer_email":null,"customer":null,
			"customer_details":{"email":"%s"},"payment_intent":"pi_async_%s","metadata":{"plan_id":"%s"}}`,
			sessionID, status, email, suffix, plan.ID)
	}

	h.onCheckoutCompleted(ctx, payload("unpaid"))
	if lics, _ := s.ListLicensesByEmail(ctx, email); len(lics) != 0 {
		t.Fatalf("unpaid session produced %d license(s)", len(lics))
	}
	if s.IsEventProcessed(ctx, sessionClaimProvider, sessionID) {
		t.Fatalf("unpaid session was claimed; async_payment_succeeded could never fulfil it")
	}

	h.onCheckoutCompleted(ctx, payload("paid"))
	lics, err := s.ListLicensesByEmail(ctx, email)
	if err != nil {
		t.Fatalf("list licenses: %v", err)
	}
	if len(lics) != 1 {
		t.Fatalf("paid session produced %d license(s), want 1", len(lics))
	}
	if lics[0].Email != email {
		t.Fatalf("license email %q, want customer_details email %q", lics[0].Email, email)
	}
	if s.IsEventProcessed(ctx, pendingSessionProvider, sessionID) {
		t.Fatalf("pending marker must be cleared once the session is fulfilled")
	}
}

// TestChargeRefunded_SecondFullRefundEventIsNoop: Stripe emits
// charge.refunded per refund; a second one for an already revoked
// license must not add another revoke audit.
func TestChargeRefunded_SecondFullRefundEventIsNoop(t *testing.T) {
	s, ctx := openStore(t)
	defer s.Close()
	plan := seedPlan(t, s, ctx, "refund2", "perpetual")
	lic := &model.License{ProductID: plan.ProductID, PlanID: plan.ID, Email: "r2-" + plan.Slug + "@example.com", LicenseKey: "KEY-r2-" + plan.Slug,
		Status: model.StatusActive, PaymentProvider: "stripe", StripePaymentIntentID: "pi_r2_" + plan.Slug}
	if err := s.CreateLicense(ctx, lic); err != nil {
		t.Fatalf("seed: %v", err)
	}
	h := &StripeHandler{Store: s}
	raw := fmt.Appendf(nil, `{"id":"ch_r2_%s","payment_intent":"%s","refunded":true}`, plan.Slug, lic.StripePaymentIntentID)
	for i := range 2 {
		if err := h.onChargeRefunded(ctx, raw); err != nil {
			t.Fatalf("refund %d: %v", i, err)
		}
	}
	audits := auditCount(s, ctx, lic.ID, "revoked")
	time.Sleep(200 * time.Millisecond) // a second, wrong audit would arrive asynchronously too
	s.DB.NewRaw("SELECT count(*) FROM audit_logs WHERE entity_id = ? AND action = 'revoked'", lic.ID).Scan(ctx, &audits)
	if got, _ := s.FindLicenseByID(ctx, lic.ID); got.Status != model.StatusRevoked || audits != 1 {
		t.Fatalf("status=%s audits=%d, want revoked/1", got.Status, audits)
	}
}

// TestChargeRefunded_SubscriptionViaInvoicePayments: a refunded
// subscription charge carries a payment intent no license recorded
// (renewal invoices get their own) and, on current API versions, no
// invoice id. The invoice is found through /v1/invoice_payments and
// its subscription names the license — while the customer's other
// license stays untouched.
func TestChargeRefunded_SubscriptionViaInvoicePayments(t *testing.T) {
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

	suffix := time.Now().Format("150405.000")
	prod := &model.Product{Name: "SubRefund Test", Slug: "subref-" + suffix, Type: "hybrid"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatalf("create product: %v", err)
	}
	plan := &model.Plan{
		ProductID: prod.ID, Name: "Pro", Slug: "subref-pro-" + suffix,
		LicenseType: "subscription", LicenseModel: "standard",
	}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatalf("create plan: %v", err)
	}
	customer := "cus_subref_" + suffix
	subID := "sub_subref_" + suffix
	renewalPI := "pi_renewal_" + suffix

	subLic := &model.License{
		ProductID: prod.ID, PlanID: plan.ID, Email: "subref-" + suffix + "@example.com",
		LicenseKey: "KEY-subref-" + suffix + "-sub", Status: model.StatusActive,
		PaymentProvider: "stripe", StripeCustomerID: customer,
		StripeSubscriptionID: subID, StripePaymentIntentID: "pi_first_" + suffix,
	}
	if err := s.CreateLicense(ctx, subLic); err != nil {
		t.Fatalf("create sub license: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	otherLic := &model.License{
		ProductID: prod.ID, PlanID: plan.ID, Email: subLic.Email,
		LicenseKey: "KEY-subref-" + suffix + "-other", Status: model.StatusActive,
		PaymentProvider: "stripe", StripeCustomerID: customer, StripePaymentIntentID: "pi_other_" + suffix,
	}
	if err := s.CreateLicense(ctx, otherLic); err != nil {
		t.Fatalf("create other license: %v", err)
	}

	var hits int32
	stubStripe(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/invoice_payments" || r.URL.Query().Get("payment[payment_intent]") != renewalPI || r.URL.Query().Get("payment[type]") != "payment_intent" {
			t.Errorf("unexpected Stripe call: %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
			http.Error(w, `{"error":{"message":"unexpected"}}`, http.StatusNotFound)
			return
		}
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"object":"list","has_more":false,"url":"/v1/invoice_payments","data":[{"id":"inpay_1","object":"invoice_payment","status":"paid",
			"invoice":{"id":"in_1","object":"invoice","parent":{"type":"subscription_details","subscription_details":{"subscription":"%s"}}}}]}`, subID)
	})

	h := &StripeHandler{Store: s}
	h.onChargeRefunded(ctx, fmt.Appendf(nil, `{"id":"ch_ren_%s","customer":"%s","payment_intent":"%s","refunded":true}`, suffix, customer, renewalPI))

	if atomic.LoadInt32(&hits) != 1 {
		t.Fatalf("expected one invoice_payments lookup, got %d", hits)
	}
	got, _ := s.FindLicenseByID(ctx, subLic.ID)
	if got.Status != model.StatusRevoked {
		t.Fatalf("subscription license not revoked: %s", got.Status)
	}
	other, _ := s.FindLicenseByID(ctx, otherLic.ID)
	if other.Status != model.StatusActive {
		t.Fatalf("unrelated license revoked: %s", other.Status)
	}
}

// TestSyncPendingCheckouts: a session that completed unpaid is
// remembered; the sync fetches it again and fulfils it once Stripe
// reports it paid, and forgets sessions that expired unpaid. This is
// the recovery path for a lost async_payment_succeeded event, which
// can arrive days after the session was created.
func TestSyncPendingCheckouts(t *testing.T) {
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

	suffix := time.Now().Format("150405.000")
	prod := &model.Product{Name: "Pending Test", Slug: "pend-" + suffix, Type: "hybrid"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatalf("create product: %v", err)
	}
	plan := &model.Plan{
		ProductID: prod.ID, Name: "Perpetual", Slug: "pend-perp-" + suffix,
		LicenseType: "perpetual", LicenseModel: "standard",
	}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatalf("create plan: %v", err)
	}
	email := "pend-" + suffix + "@example.com"
	paidLater := "cs_test_pend_paid_" + suffix
	expired := "cs_test_pend_exp_" + suffix
	unpaid := func(id string) []byte {
		return fmt.Appendf(nil, `{"id":"%s","mode":"payment","status":"complete","payment_status":"unpaid","customer_details":{"email":"%s"},"metadata":{"plan_id":"%s"}}`, id, email, plan.ID)
	}

	h := &StripeHandler{Store: s}
	h.onCheckoutCompleted(ctx, unpaid(paidLater))
	h.onCheckoutCompleted(ctx, unpaid(expired))
	if rows, _ := s.ListPendingCheckoutSessions(ctx, time.Hour, nil, 1000); len(rows) < 2 {
		t.Fatalf("expected both sessions pending, got %v", rows)
	}

	stubStripe(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/checkout/sessions/" + paidLater:
			fmt.Fprintf(w, `{"id":"%s","object":"checkout.session","mode":"payment","status":"complete","payment_status":"paid","payment_intent":"pi_pend_%s","customer_details":{"email":"%s"},"metadata":{"plan_id":"%s"}}`, paidLater, suffix, email, plan.ID)
		case "/v1/checkout/sessions/" + expired:
			fmt.Fprintf(w, `{"id":"%s","object":"checkout.session","mode":"payment","status":"expired","payment_status":"unpaid","metadata":{"plan_id":"%s"}}`, expired, plan.ID)
		default:
			// Pending rows left by other tests or e2e runs share this
			// database; the sync logs a warning for them and moves on.
			http.Error(w, `{"error":{"type":"invalid_request_error","message":"no such session"}}`, http.StatusNotFound)
		}
	})

	h.SyncPendingCheckouts(ctx)
	h.SyncPendingCheckouts(ctx) // second pass must not fulfil twice

	lics, _ := s.ListLicensesByEmail(ctx, email)
	if len(lics) != 1 || lics[0].StripePaymentIntentID != "pi_pend_"+suffix {
		t.Fatalf("expected exactly one license from the settled session, got %+v", lics)
	}
	rows, _ := s.ListPendingCheckoutSessions(ctx, time.Hour, nil, 1000)
	for _, r := range rows {
		if r.SessionID == paidLater || r.SessionID == expired {
			t.Fatalf("session %s still pending after sync", r.SessionID)
		}
	}
}

// TestFulfillCheckout_ReleasesClaimWhenLicenseWriteFails: the session
// is claimed right before the license insert. If that insert fails,
// the claim must go away, otherwise every later webhook, success-page
// visit and sync short-circuits on it and a paid customer never gets
// a license. The failure is forced through the unique
// stripe_subscription_id: the first attempt reuses a taken id.
func TestFulfillCheckout_ReleasesClaimWhenLicenseWriteFails(t *testing.T) {
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

	suffix := time.Now().Format("150405.000")
	prod := &model.Product{Name: "Claim Test", Slug: "claim-" + suffix, Type: "hybrid"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatalf("create product: %v", err)
	}
	plan := &model.Plan{
		ProductID: prod.ID, Name: "Pro", Slug: "claim-pro-" + suffix,
		LicenseType: "subscription", LicenseModel: "standard",
	}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatalf("create plan: %v", err)
	}
	taken := "sub_taken_" + suffix
	if err := s.CreateLicense(ctx, &model.License{
		ProductID: prod.ID, PlanID: plan.ID, Email: "owner-" + suffix + "@example.com",
		LicenseKey: "KEY-claim-" + suffix, Status: model.StatusActive, StripeSubscriptionID: taken,
	}); err != nil {
		t.Fatalf("seed license: %v", err)
	}

	// The subscription lookup is stubbed to fail so the plan comes
	// from metadata and no other Stripe call is needed.
	stubStripe(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"type":"invalid_request_error","message":"no such subscription"}}`, http.StatusNotFound)
	})

	h := &StripeHandler{Store: s}
	email := "claim-" + suffix + "@example.com"
	sessionID := "cs_test_claim_" + suffix
	meta := map[string]string{"plan_id": plan.ID, "session_id": sessionID}

	if fulfilOK(h, ctx, email, "", taken, "", meta, "test") {
		t.Fatalf("fulfilment reported success although the license insert must have failed")
	}
	if s.IsEventProcessed(ctx, sessionClaimProvider, sessionID) {
		t.Fatalf("session still claimed after a failed license write")
	}
	if lics, _ := s.ListLicensesByEmail(ctx, email); len(lics) != 0 {
		t.Fatalf("unexpected license after failed write: %+v", lics)
	}

	// Retry with a free subscription id succeeds.
	if !fulfilOK(h, ctx, email, "", "sub_free_"+suffix, "", meta, "test") {
		t.Fatalf("retry did not fulfil")
	}
	if lics, _ := s.ListLicensesByEmail(ctx, email); len(lics) != 1 {
		t.Fatalf("expected one license after retry, got %d", len(lics))
	}
	if !s.IsEventProcessed(ctx, fulfilledSessionProvider, sessionID) || s.IsEventProcessed(ctx, sessionClaimProvider, sessionID) {
		t.Fatalf("after success the claim must have become the done marker")
	}
}

// TestListPendingCheckoutSessions_OldestFirst: the sync processes a
// bounded batch per round, so ordering must start at the oldest row
// or a backlog above the batch size would never drain.
func TestListPendingCheckoutSessions_OldestFirst(t *testing.T) {
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
	suffix := time.Now().Format("150405.000")
	ids := []string{"cs_order_a_" + suffix, "cs_order_b_" + suffix, "cs_order_c_" + suffix}
	for i, id := range ids {
		if !s.TryRecordProcessedEvent(ctx, pendingSessionProvider, id) {
			t.Fatalf("record %s", id)
		}
		// Push each row further into the past: a is oldest.
		if _, err := s.DB.NewUpdate().TableExpr("processed_events").
			Set("created_at = now() - make_interval(hours => ?)", 3-i).
			Where("provider = ? AND event_id = ?", pendingSessionProvider, id).Exec(ctx); err != nil {
			t.Fatalf("backdate: %v", err)
		}
		// defer, not t.Cleanup: the store is closed by the earlier
		// defer before Cleanup functions run.
		defer s.DeleteProcessedEvent(ctx, pendingSessionProvider, id)
	}
	// Page with size 1 through everything: keyset paging must visit
	// each row exactly once, oldest first, regardless of what else
	// the shared database holds.
	var mine []string
	var after *store.PendingCheckoutSession
	for range 10000 {
		rows, err := s.ListPendingCheckoutSessions(ctx, 24*time.Hour, after, 1)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(rows) == 0 {
			break
		}
		if strings.HasSuffix(rows[0].SessionID, suffix) {
			mine = append(mine, rows[0].SessionID)
		}
		after = &rows[0]
	}
	if len(mine) != 3 || mine[0] != ids[0] || mine[1] != ids[1] || mine[2] != ids[2] {
		t.Fatalf("expected oldest-first %v, got %v", ids, mine)
	}
}

// TestFulfillCheckout_InFlightClaimIsNotFulfilment: a claim row
// without a license means another caller is still working (or died).
// Reporting it as fulfilled would let the pending sync drop its
// marker before any license exists. A claim older than staleClaimAge
// is taken over instead.
func TestFulfillCheckout_InFlightClaimIsNotFulfilment(t *testing.T) {
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

	suffix := time.Now().Format("150405.000")
	prod := &model.Product{Name: "Inflight Test", Slug: "infl-" + suffix, Type: "hybrid"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatalf("create product: %v", err)
	}
	plan := &model.Plan{ProductID: prod.ID, Name: "Perpetual", Slug: "infl-perp-" + suffix, LicenseType: "perpetual", LicenseModel: "standard"}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatalf("create plan: %v", err)
	}
	h := &StripeHandler{Store: s}
	email := "infl-" + suffix + "@example.com"
	sessionID := "cs_test_infl_" + suffix
	meta := map[string]string{"plan_id": plan.ID, "session_id": sessionID}

	// Someone else holds a fresh claim and has not produced a license.
	if !s.TryRecordProcessedEvent(ctx, fulfilledSessionProvider, sessionID) || !s.TryRecordProcessedEvent(ctx, sessionClaimProvider, sessionID) {
		t.Fatalf("seed claim")
	}
	if fulfilOK(h, ctx, email, "", "", "", meta, "test") {
		t.Fatalf("in-flight claim reported as fulfilled")
	}
	if lics, _ := s.ListLicensesByEmail(ctx, email); len(lics) != 0 {
		t.Fatalf("license created despite foreign claim: %+v", lics)
	}

	// The claim goes stale: it is taken over and fulfilment proceeds.
	if _, err := s.DB.NewUpdate().TableExpr("processed_events").
		Set("created_at = now() - make_interval(secs => ?)", int(staleClaimAge.Seconds())+60).
		Where("provider = ? AND event_id = ?", sessionClaimProvider, sessionID).Exec(ctx); err != nil {
		t.Fatalf("backdate claim: %v", err)
	}
	if !fulfilOK(h, ctx, email, "", "", "", meta, "test") {
		t.Fatalf("stale claim was not taken over")
	}
	lics, _ := s.ListLicensesByEmail(ctx, email)
	if len(lics) != 1 || lics[0].StripeCheckoutSessionID != sessionID {
		t.Fatalf("expected one license carrying the session id, got %+v", lics)
	}
	// And a replay now sees the license, not just the claim.
	if !fulfilOK(h, ctx, email, "", "", "", meta, "test") {
		t.Fatalf("replay after fulfilment must report fulfilled")
	}
	if lics, _ := s.ListLicensesByEmail(ctx, email); len(lics) != 1 {
		t.Fatalf("replay duplicated the license")
	}
}

// TestSyncRecentCheckouts_RecordsUnpaidSessions: reconciliation that
// finds a completed-but-unpaid session (delayed payment, completed
// webhook lost) must remember it for SyncPendingCheckouts instead of
// dropping it.
func TestSyncRecentCheckouts_RecordsUnpaidSessions(t *testing.T) {
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
	suffix := time.Now().Format("150405.000")
	unpaidID := "cs_test_recon_unpaid_" + suffix
	defer s.DeleteProcessedEvent(ctx, pendingSessionProvider, unpaidID)

	stubStripe(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/checkout/sessions" {
			t.Errorf("unexpected Stripe call: %s %s", r.Method, r.URL.Path)
			http.Error(w, `{"error":{"message":"unexpected"}}`, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"object":"list","has_more":false,"url":"/v1/checkout/sessions","data":[{"id":"%s","object":"checkout.session","mode":"payment","status":"complete","payment_status":"unpaid","metadata":{}}]}`, unpaidID)
	})
	h := &StripeHandler{Store: s}
	h.SyncRecentCheckouts(ctx)
	if !s.IsEventProcessed(ctx, pendingSessionProvider, unpaidID) {
		t.Fatalf("unpaid session found by reconciliation was not recorded as pending")
	}
	if s.IsEventProcessed(ctx, "stripe_fulfill", unpaidID) {
		t.Fatalf("unpaid session must not be claimed as fulfilled")
	}
}

// TestSyncPendingCheckouts_TakesOverStaleClaim: a worker claimed the
// session and died before writing the license. The pending sync must
// still see the session, and fulfilment must take the stale claim
// over — otherwise a paid delayed-payment session is lost forever.
func TestSyncPendingCheckouts_TakesOverStaleClaim(t *testing.T) {
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
	suffix := time.Now().Format("150405.000")
	prod := &model.Product{Name: "Stale Test", Slug: "stale-" + suffix, Type: "hybrid"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatalf("create product: %v", err)
	}
	plan := &model.Plan{ProductID: prod.ID, Name: "Perpetual", Slug: "stale-perp-" + suffix, LicenseType: "perpetual", LicenseModel: "standard"}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatalf("create plan: %v", err)
	}
	email := "stale-" + suffix + "@example.com"
	sessionID := "cs_test_stale_" + suffix
	if !s.TryRecordProcessedEvent(ctx, pendingSessionProvider, sessionID) || !s.TryRecordProcessedEvent(ctx, fulfilledSessionProvider, sessionID) || !s.TryRecordProcessedEvent(ctx, sessionClaimProvider, sessionID) {
		t.Fatalf("seed rows")
	}
	defer s.DeleteProcessedEvent(ctx, pendingSessionProvider, sessionID)
	if _, err := s.DB.NewUpdate().TableExpr("processed_events").
		Set("created_at = now() - make_interval(secs => ?)", int(staleClaimAge.Seconds())+60).
		Where("provider = ? AND event_id = ?", sessionClaimProvider, sessionID).Exec(ctx); err != nil {
		t.Fatalf("backdate claim: %v", err)
	}
	visible := false
	for rows, _ := s.ListPendingCheckoutSessions(ctx, time.Hour, nil, 1000); len(rows) > 0 && !visible; rows = rows[1:] {
		visible = rows[0].SessionID == sessionID
	}
	if !visible {
		t.Fatalf("pending session hidden by a stale claim")
	}
	stubStripe(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/checkout/sessions/"+sessionID {
			fmt.Fprintf(w, `{"id":"%s","object":"checkout.session","mode":"payment","status":"complete","payment_status":"paid","customer_details":{"email":"%s"},"metadata":{"plan_id":"%s"}}`, sessionID, email, plan.ID)
			return
		}
		http.Error(w, `{"error":{"type":"invalid_request_error","message":"no such session"}}`, http.StatusNotFound)
	})
	h := &StripeHandler{Store: s}
	h.SyncPendingCheckouts(ctx)
	lics, _ := s.ListLicensesByEmail(ctx, email)
	if len(lics) != 1 || lics[0].StripeCheckoutSessionID != sessionID {
		t.Fatalf("stale claim not taken over by the pending sync: %+v", lics)
	}
	if s.IsEventProcessed(ctx, pendingSessionProvider, sessionID) {
		t.Fatalf("pending marker not cleared after fulfilment")
	}
}

// TestFulfillCheckout_LosingSessionRaceIsFulfilled: the unique index
// on stripe_checkout_session_id refuses a second license for one
// session. A worker hitting it must report the session fulfilled and
// must not release the claim.
func TestFulfillCheckout_LosingSessionRaceIsFulfilled(t *testing.T) {
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
	suffix := time.Now().Format("150405.000")
	prod := &model.Product{Name: "Race Test", Slug: "race-" + suffix, Type: "hybrid"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatalf("create product: %v", err)
	}
	plan := &model.Plan{ProductID: prod.ID, Name: "Perpetual", Slug: "race-perp-" + suffix, LicenseType: "perpetual", LicenseModel: "standard"}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatalf("create plan: %v", err)
	}
	sessionID := "cs_test_race_" + suffix
	// The other worker's license already exists for this session,
	// with the done marker it writes right after.
	if err := s.CreateLicense(ctx, &model.License{ProductID: prod.ID, PlanID: plan.ID, Email: "winner-" + suffix + "@example.com",
		LicenseKey: "KEY-race-" + suffix, Status: model.StatusActive, StripeCheckoutSessionID: sessionID}); err != nil {
		t.Fatalf("seed license: %v", err)
	}
	s.TryRecordProcessedEvent(ctx, fulfilledSessionProvider, sessionID)
	dup := &model.License{ProductID: prod.ID, PlanID: plan.ID, Email: "loser-" + suffix + "@example.com",
		LicenseKey: "KEY-race2-" + suffix, Status: model.StatusActive, StripeCheckoutSessionID: sessionID}
	err = s.CreateLicenseWithSubscription(ctx, dup, plan)
	if err == nil || !store.IsCheckoutSessionConflict(err) {
		t.Fatalf("second license for one session must hit the unique index, got %v", err)
	}
	// Through fulfilCheckout: sessionFulfilled short-circuits, so no
	// claim is written and the answer is "fulfilled".
	h := &StripeHandler{Store: s}
	if !fulfilOK(h, ctx, "loser-"+suffix+"@example.com", "", "", "", map[string]string{"plan_id": plan.ID, "session_id": sessionID}, "test") {
		t.Fatalf("session with an existing license must report fulfilled")
	}
}

// fulfilOK is the bool half of fulfillCheckout for tests that only
// care whether the session ended up fulfilled.
func fulfilOK(h *StripeHandler, ctx context.Context, email, customerID, subscriptionID, paymentIntentID string, metadata map[string]string, source string) bool {
	ok, _ := h.fulfillCheckout(ctx, email, customerID, subscriptionID, paymentIntentID, metadata, source)
	return ok
}

func openStore(t *testing.T) (*store.Store, context.Context) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("skipping integration test: TEST_DATABASE_URL not set")
	}
	s, err := store.New(dsn)
	if err != nil {
		t.Skipf("skipping integration test: %v", err)
	}
	if err := s.RunMigrations("../../db/migrations"); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	return s, context.Background()
}

func seedPlan(t *testing.T, s *store.Store, ctx context.Context, tag, licenseType string) *model.Plan {
	t.Helper()
	suffix := tag + "-" + time.Now().Format("150405.000")
	prod := &model.Product{Name: tag, Slug: "p-" + suffix, Type: "hybrid", FeedLicenseRequired: true}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatalf("create product: %v", err)
	}
	plan := &model.Plan{ProductID: prod.ID, Name: tag, Slug: "pl-" + suffix, LicenseType: licenseType, LicenseModel: "standard"}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatalf("create plan: %v", err)
	}
	return plan
}

// signedWebhook posts an event through the real Webhook handler with
// a valid signature, the way Stripe would.
func signedWebhook(t *testing.T, h *StripeHandler, secret string, eventID, eventType string, obj any) (int, map[string]any) {
	return signedWebhookVersion(t, h, secret, eventID, eventType, obj, stripe.APIVersion)
}

func signedWebhookVersion(t *testing.T, h *StripeHandler, secret string, eventID, eventType string, obj any, apiVersion string) (int, map[string]any) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"id": eventID, "object": "event", "type": eventType, "livemode": false, "api_version": apiVersion, "created": time.Now().Unix(), "data": map[string]any{"object": obj}})
	sig := webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{Payload: body, Secret: secret})
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/wh", h.Webhook)
	req := httptest.NewRequest(http.MethodPost, "/wh", strings.NewReader(string(body)))
	req.Header.Set("Stripe-Signature", sig.Header)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

// TestFulfillCheckout_LegacyDoneMarker: licenses created before
// session ids were stored have no session on the row; the migration
// renamed their claim to a done marker. A replay of such a session
// must be "fulfilled" — not a stale claim to take over and not a
// second license.
func TestFulfillCheckout_LegacyDoneMarker(t *testing.T) {
	s, ctx := openStore(t)
	defer s.Close()
	plan := seedPlan(t, s, ctx, "legacy", "perpetual")
	sessionID := "cs_test_legacy_" + plan.Slug
	if !s.TryRecordProcessedEvent(ctx, fulfilledSessionProvider, sessionID) {
		t.Fatalf("seed done marker")
	}
	defer s.DeleteProcessedEvent(ctx, fulfilledSessionProvider, sessionID)
	h := &StripeHandler{Store: s}
	email := "legacy-" + plan.Slug + "@example.com"
	ok, err := h.fulfillCheckout(ctx, email, "", "", "", map[string]string{"plan_id": plan.ID, "session_id": sessionID}, "test")
	if !ok || err != nil {
		t.Fatalf("legacy done session: ok=%v err=%v", ok, err)
	}
	if lics, _ := s.ListLicensesByEmail(ctx, email); len(lics) != 0 {
		t.Fatalf("legacy done session produced a new license: %+v", lics)
	}
}

// TestWebhook_TransientRefundLookupRetries: a renewal refund with no
// invoice on the charge needs /v1/invoice_payments. When that call
// fails, the webhook must release its event claim and answer 5xx so
// Stripe retries; the retry, with Stripe healthy, revokes the license.
func TestWebhook_TransientRefundLookupRetries(t *testing.T) {
	s, ctx := openStore(t)
	defer s.Close()
	plan := seedPlan(t, s, ctx, "refundretry", "subscription")
	customer := "cus_rr_" + plan.Slug
	subID := "sub_rr_" + plan.Slug
	lic := &model.License{ProductID: plan.ProductID, PlanID: plan.ID, Email: "rr-" + plan.Slug + "@example.com", LicenseKey: "KEY-rr-" + plan.Slug,
		Status: model.StatusActive, PaymentProvider: "stripe", StripeCustomerID: customer, StripeSubscriptionID: subID}
	other := &model.License{ProductID: plan.ProductID, PlanID: plan.ID, Email: lic.Email, LicenseKey: "KEY-rr2-" + plan.Slug,
		Status: model.StatusActive, PaymentProvider: "stripe", StripeCustomerID: customer}
	for _, l := range []*model.License{lic, other} {
		if err := s.CreateLicense(ctx, l); err != nil {
			t.Fatalf("seed license: %v", err)
		}
	}
	var healthy atomic.Bool
	stubStripe(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/v1/invoice_payments" {
			http.Error(w, `{"error":{"type":"invalid_request_error","message":"unexpected"}}`, http.StatusNotFound)
			return
		}
		if !healthy.Load() {
			http.Error(w, `{"error":{"type":"api_error","message":"temporarily unavailable"}}`, http.StatusInternalServerError)
			return
		}
		fmt.Fprintf(w, `{"object":"list","has_more":false,"url":"/v1/invoice_payments","data":[{"id":"inpay_1","object":"invoice_payment","status":"paid","invoice":{"id":"in_1","object":"invoice","parent":{"type":"subscription_details","subscription_details":{"subscription":"%s"}}}}]}`, subID)
	})
	h := &StripeHandler{Store: s}
	h.SetWebhookSecret("whsec_test_" + plan.Slug)
	eventID := "evt_rr_" + plan.Slug
	charge := map[string]any{"id": "ch_rr_" + plan.Slug, "object": "charge", "customer": customer, "payment_intent": "pi_renewal_" + plan.Slug, "refunded": true}

	code, out := signedWebhook(t, h, "whsec_test_"+plan.Slug, eventID, "charge.refunded", charge)
	if code != http.StatusInternalServerError || out["retry"] != true {
		t.Fatalf("outage: want 500 retry, got %d %v", code, out)
	}
	if s.IsEventProcessed(ctx, processedEventClaimProvider, eventID) || s.IsEventProcessed(ctx, processedEventDoneProvider, eventID) {
		t.Fatalf("event claim not released after transient failure")
	}
	if got, _ := s.FindLicenseByID(ctx, lic.ID); got.Status != model.StatusActive {
		t.Fatalf("license changed during outage: %s", got.Status)
	}

	healthy.Store(true)
	code, out = signedWebhook(t, h, "whsec_test_"+plan.Slug, eventID, "charge.refunded", charge)
	if code != http.StatusOK || out["received"] != true || out["skipped"] == true {
		t.Fatalf("retry: want 200 received, got %d %v", code, out)
	}
	if got, _ := s.FindLicenseByID(ctx, lic.ID); got.Status != model.StatusRevoked {
		t.Fatalf("retry did not revoke: %s", got.Status)
	}
	if got, _ := s.FindLicenseByID(ctx, other.ID); got.Status != model.StatusActive {
		t.Fatalf("other license touched: %s", got.Status)
	}
}

// TestWebhook_TransientLineItemsFailureStaysRetryable: a paid
// Payment Link session whose line-items lookup fails must not be
// acknowledged as done. The webhook asks Stripe to retry and records
// the session as pending, so the sync fulfils it even after Stripe's
// retry window and outside the recent-checkout window.
func TestWebhook_TransientLineItemsFailureStaysRetryable(t *testing.T) {
	s, ctx := openStore(t)
	defer s.Close()
	plan := seedPlan(t, s, ctx, "liretry", "perpetual")
	price := "price_liretry_" + plan.Slug
	plan.StripePriceID = price
	if err := s.UpdatePlan(ctx, plan); err != nil {
		t.Fatalf("set price: %v", err)
	}
	sessionID := "cs_test_liretry_" + plan.Slug
	email := "li-" + plan.Slug + "@example.com"
	defer s.DeleteProcessedEvent(ctx, pendingSessionProvider, sessionID)
	var healthy atomic.Bool
	stubStripe(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/checkout/sessions/" + sessionID + "/line_items":
			if !healthy.Load() {
				http.Error(w, `{"error":{"type":"api_error","message":"temporarily unavailable"}}`, http.StatusInternalServerError)
				return
			}
			fmt.Fprintf(w, `{"object":"list","has_more":false,"url":"%s","data":[{"id":"li_1","object":"item","price":{"id":"%s","object":"price"}}]}`, r.URL.Path, price)
		case "/v1/checkout/sessions/" + sessionID:
			fmt.Fprintf(w, `{"id":"%s","object":"checkout.session","mode":"payment","status":"complete","payment_status":"paid","payment_intent":"pi_li_%s","customer_details":{"email":"%s"},"metadata":{}}`, sessionID, plan.Slug, email)
		default:
			http.Error(w, `{"error":{"type":"invalid_request_error","message":"no such object"}}`, http.StatusNotFound)
		}
	})
	h := &StripeHandler{Store: s}
	h.SetWebhookSecret("whsec_test_" + plan.Slug)
	eventID := "evt_li_" + plan.Slug
	sess := map[string]any{"id": sessionID, "object": "checkout.session", "mode": "payment", "payment_status": "paid", "customer_details": map[string]any{"email": email}, "metadata": map[string]any{}}

	code, out := signedWebhook(t, h, "whsec_test_"+plan.Slug, eventID, "checkout.session.completed", sess)
	if code != http.StatusInternalServerError || out["retry"] != true {
		t.Fatalf("outage: want 500 retry, got %d %v", code, out)
	}
	if s.IsEventProcessed(ctx, processedEventClaimProvider, eventID) || s.IsEventProcessed(ctx, processedEventDoneProvider, eventID) {
		t.Fatalf("event claim not released")
	}
	if !s.IsEventProcessed(ctx, pendingSessionProvider, sessionID) {
		t.Fatalf("session not recorded as pending for durable retry")
	}
	if s.IsEventProcessed(ctx, sessionClaimProvider, sessionID) {
		t.Fatalf("session claimed although nothing was fulfilled")
	}

	// Stripe's retries have run out; the pending sync gets it later.
	healthy.Store(true)
	h.SyncPendingCheckouts(ctx)
	lics, _ := s.ListLicensesByEmail(ctx, email)
	if len(lics) != 1 || lics[0].PlanID != plan.ID {
		t.Fatalf("pending sync did not fulfil after recovery: %+v", lics)
	}
	if s.IsEventProcessed(ctx, pendingSessionProvider, sessionID) {
		t.Fatalf("pending marker not cleared")
	}
}

// TestWebhook_APIVersionGate: legacy date versions and the SDK's
// train are applied; an unknown train is refused without being
// recorded, so Stripe's retry lands once the endpoint is fixed.
func TestWebhook_APIVersionGate(t *testing.T) {
	s, ctx := openStore(t)
	defer s.Close()
	h := &StripeHandler{Store: s}
	secret := "whsec_gate_" + time.Now().Format("150405.000")
	h.SetWebhookSecret(secret)
	obj := map[string]any{"id": "sub_gate", "object": "subscription", "status": "active"}

	code, out := signedWebhookVersion(t, h, secret, "evt_gate_future_"+secret, "customer.subscription.updated", obj, "2027-01-01.clover")
	if code != http.StatusBadRequest {
		t.Fatalf("future train: want 400, got %d %v", code, out)
	}
	if s.IsEventProcessed(ctx, processedEventClaimProvider, "evt_gate_future_"+secret) || s.IsEventProcessed(ctx, processedEventDoneProvider, "evt_gate_future_"+secret) {
		t.Fatalf("unsupported event must not be recorded as processed")
	}
	code, _ = signedWebhookVersion(t, h, secret, "evt_gate_legacy_"+secret, "customer.subscription.updated", obj, "2020-08-27")
	if code != http.StatusOK {
		t.Fatalf("legacy version: want 200, got %d", code)
	}
	code, _ = signedWebhookVersion(t, h, secret, "evt_gate_acacia_"+secret, "customer.subscription.updated", obj, "2024-09-30.acacia")
	if code != http.StatusOK {
		t.Fatalf("acacia version: want 200, got %d", code)
	}
	code, _ = signedWebhookVersion(t, h, secret, "evt_gate_basil_"+secret, "customer.subscription.updated", obj, stripe.APIVersion)
	if code != http.StatusOK {
		t.Fatalf("sdk version: want 200, got %d", code)
	}
}

// TestWebhook_PreviousSecretDuringDrain: after an endpoint replacement
// the old endpoint still delivers with its old secret; those events
// verify until the drain period ends, then are refused.
func TestWebhook_PreviousSecretDuringDrain(t *testing.T) {
	s, ctx := openStore(t)
	defer s.Close()
	_ = ctx
	h := &StripeHandler{Store: s}
	tag := time.Now().Format("150405.000")
	h.SetWebhookSecret("whsec_new_" + tag)
	h.SetPreviousWebhookSecret("whsec_old_"+tag, time.Now().Add(time.Hour))
	obj := map[string]any{"id": "sub_drain", "object": "subscription", "status": "active"}

	if code, _ := signedWebhook(t, h, "whsec_old_"+tag, "evt_drain_old_"+tag, "customer.subscription.updated", obj); code != http.StatusOK {
		t.Fatalf("old secret within drain: want 200, got %d", code)
	}
	if code, _ := signedWebhook(t, h, "whsec_new_"+tag, "evt_drain_new_"+tag, "customer.subscription.updated", obj); code != http.StatusOK {
		t.Fatalf("new secret: want 200, got %d", code)
	}
	h.SetPreviousWebhookSecret("whsec_old_"+tag, time.Now().Add(-time.Second))
	if code, _ := signedWebhook(t, h, "whsec_old_"+tag, "evt_drain_late_"+tag, "customer.subscription.updated", obj); code != http.StatusBadRequest {
		t.Fatalf("old secret after drain: want 400, got %d", code)
	}
	if code, _ := signedWebhook(t, h, "whsec_other_"+tag, "evt_drain_bad_"+tag, "customer.subscription.updated", obj); code != http.StatusBadRequest {
		t.Fatalf("unknown secret: want 400, got %d", code)
	}
}

// TestWebhook_StaleEventClaimIsRetried: an event claim without a done
// marker (handling failed and the release failed too) must not make
// Stripe's retry report "skipped" forever; once stale it is taken
// over. A fresh claim and a done event stay skipped.
func TestWebhook_StaleEventClaimIsRetried(t *testing.T) {
	s, ctx := openStore(t)
	defer s.Close()
	h := &StripeHandler{Store: s}
	tag := time.Now().Format("150405.000")
	secret := "whsec_claim_" + tag
	h.SetWebhookSecret(secret)
	obj := map[string]any{"id": "sub_claim", "object": "subscription", "status": "active"}

	fresh := "evt_claim_fresh_" + tag
	s.TryRecordProcessedEvent(ctx, processedEventDoneProvider, fresh)
	s.TryRecordProcessedEvent(ctx, processedEventClaimProvider, fresh)
	defer s.DeleteProcessedEvent(ctx, processedEventClaimProvider, fresh)
	defer s.DeleteProcessedEvent(ctx, processedEventDoneProvider, fresh)
	if code, out := signedWebhook(t, h, secret, fresh, "customer.subscription.updated", obj); code != http.StatusServiceUnavailable || out["retry"] != true {
		t.Fatalf("fresh claim without done marker must ask for a retry, got %d %v", code, out)
	}
	if !s.IsEventProcessed(ctx, processedEventClaimProvider, fresh) {
		t.Fatalf("in-progress marker must survive a concurrent delivery")
	}

	stale := "evt_claim_stale_" + tag
	s.TryRecordProcessedEvent(ctx, processedEventDoneProvider, stale)
	s.TryRecordProcessedEvent(ctx, processedEventClaimProvider, stale)
	s.DB.NewUpdate().TableExpr("processed_events").Set("created_at = now() - make_interval(secs => ?)", int(staleClaimAge.Seconds())+60).
		Where("provider = ? AND event_id = ?", processedEventClaimProvider, stale).Exec(ctx)
	if code, out := signedWebhook(t, h, secret, stale, "customer.subscription.updated", obj); code != http.StatusOK || out["skipped"] == true {
		t.Fatalf("stale claim must be taken over and processed, got %d %v", code, out)
	}
	if !s.IsEventProcessed(ctx, processedEventDoneProvider, stale) || s.IsEventProcessed(ctx, processedEventClaimProvider, stale) {
		t.Fatalf("after processing only the done row must remain")
	}
	// Now done: a resend stays skipped.
	if _, out := signedWebhook(t, h, secret, stale, "customer.subscription.updated", obj); out["skipped"] != true {
		t.Fatalf("done event must stay skipped, got %v", out)
	}
	// An old binary's row (reservation only, written mid-rollout)
	// reads as done too, however old it is.
	legacy := "evt_claim_legacy_" + tag
	s.TryRecordProcessedEvent(ctx, processedEventDoneProvider, legacy)
	s.DB.NewUpdate().TableExpr("processed_events").Set("created_at = now() - interval '1 day'").Where("event_id = ?", legacy).Exec(ctx)
	if _, out := signedWebhook(t, h, secret, legacy, "customer.subscription.updated", obj); out["skipped"] != true {
		t.Fatalf("old-binary row must read as done, got %v", out)
	}
}

// TestCheckoutCompleted_UnfulfilledIsScheduled: when fulfilment does
// not happen here — another worker holds a fresh claim — the webhook
// must not just acknowledge; the session is recorded as pending so
// the sync picks it up if that worker never finishes.
func TestCheckoutCompleted_UnfulfilledIsScheduled(t *testing.T) {
	s, ctx := openStore(t)
	defer s.Close()
	plan := seedPlan(t, s, ctx, "unful", "perpetual")
	sessionID := "cs_test_unful_" + plan.Slug
	defer s.DeleteProcessedEvent(ctx, pendingSessionProvider, sessionID)
	defer s.DeleteProcessedEvent(ctx, sessionClaimProvider, sessionID)
	defer s.DeleteProcessedEvent(ctx, fulfilledSessionProvider, sessionID)
	s.TryRecordProcessedEvent(ctx, fulfilledSessionProvider, sessionID) // someone else, mid-way:
	s.TryRecordProcessedEvent(ctx, sessionClaimProvider, sessionID)     // reservation + in-flight marker
	h := &StripeHandler{Store: s}
	raw := fmt.Appendf(nil, `{"id":"%s","mode":"payment","payment_status":"paid","customer_details":{"email":"u-%s@example.com"},"metadata":{"plan_id":"%s"}}`, sessionID, plan.Slug, plan.ID)
	if err := h.onCheckoutCompleted(ctx, raw); err != nil {
		t.Fatalf("no transient error expected, got %v", err)
	}
	if !s.IsEventProcessed(ctx, pendingSessionProvider, sessionID) {
		t.Fatalf("unfulfilled paid session was not scheduled for retry")
	}
	if lics, _ := s.ListLicensesByEmail(ctx, "u-"+plan.Slug+"@example.com"); len(lics) != 0 {
		t.Fatalf("license created despite foreign claim")
	}
}

func fetchSession(id string) (*stripe.CheckoutSession, error) {
	return session.Get(id, nil)
}

// auditCount waits for the asynchronous audit writer and returns how
// many rows match; polls up to two seconds.
func auditCount(s *store.Store, ctx context.Context, entityID, action string) int {
	var n int
	for range 40 {
		s.DB.NewRaw("SELECT count(*) FROM audit_logs WHERE entity_id = ? AND action = ?", entityID, action).Scan(ctx, &n)
		if n > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	return n
}

// TestFulfillCheckout_DeletedPlanIsNotAnEmptyPlan: metadata naming a
// plan that no longer exists must read as "no plan" (no license, no
// claim, no error), not as an empty plan to insert.
func TestFulfillCheckout_DeletedPlanIsNotAnEmptyPlan(t *testing.T) {
	s, ctx := openStore(t)
	defer s.Close()
	// Without a plan from metadata the line items are consulted; the
	// session does not exist at Stripe either.
	stubStripe(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"type":"invalid_request_error","message":"no such session"}}`, http.StatusNotFound)
	})
	h := &StripeHandler{Store: s}
	sessionID := "cs_test_noplan_" + time.Now().Format("150405.000")
	ok, err := h.fulfillCheckout(ctx, "np@example.com", "", "", "", map[string]string{"plan_id": "00000000-0000-0000-0000-000000000000", "session_id": sessionID}, "test")
	if ok || err != nil {
		t.Fatalf("deleted plan: ok=%v err=%v, want false,nil", ok, err)
	}
	if s.IsEventProcessed(ctx, sessionClaimProvider, sessionID) {
		t.Fatalf("session claimed although no plan could be resolved")
	}
}

// TestClaimsAreVisibleToOlderBinaries: during a rolling upgrade the
// previous binary reserves events under "stripe" and sessions under
// "stripe_fulfill" with a plain insert. Our in-flight claim must make
// that insert conflict, or the old replica processes concurrently.
func TestClaimsAreVisibleToOlderBinaries(t *testing.T) {
	s, ctx := openStore(t)
	defer s.Close()
	h := &StripeHandler{Store: s}
	tag := time.Now().Format("150405.000")
	eventID, sessionID := "evt_vis_"+tag, "cs_vis_"+tag
	defer s.DeleteProcessedEvent(ctx, processedEventDoneProvider, eventID)
	defer s.DeleteProcessedEvent(ctx, processedEventClaimProvider, eventID)
	defer s.DeleteProcessedEvent(ctx, fulfilledSessionProvider, sessionID)
	defer s.DeleteProcessedEvent(ctx, sessionClaimProvider, sessionID)

	if claimed, done, err := h.claimEvent(ctx, eventID); !claimed || done || err != nil {
		t.Fatalf("claim event: %v %v %v", claimed, done, err)
	}
	if s.TryRecordProcessedEvent(ctx, "stripe", eventID) {
		t.Fatalf("old binary could reserve an event we hold")
	}
	if claimed, err := h.claimSession(ctx, sessionID); !claimed || err != nil {
		t.Fatalf("claim session: %v %v", claimed, err)
	}
	if s.TryRecordProcessedEvent(ctx, "stripe_fulfill", sessionID) {
		t.Fatalf("old binary could reserve a session we hold")
	}
	// And the other way round: a row the old binary wrote first is
	// "done" for us, so we neither claim nor take it over.
	oldEvent := "evt_old_" + tag
	s.TryRecordProcessedEvent(ctx, "stripe", oldEvent)
	defer s.DeleteProcessedEvent(ctx, "stripe", oldEvent)
	if claimed, done, _ := h.claimEvent(ctx, oldEvent); claimed || !done {
		t.Fatalf("old binary's reservation must read as done")
	}
}

// TestSyncPendingCheckouts_CursorContinues: with more pending rows
// than one round may visit, the next round continues after the last
// row instead of re-polling the same oldest batch.
func TestSyncPendingCheckouts_CursorContinues(t *testing.T) {
	s, ctx := openStore(t)
	defer s.Close()
	tag := time.Now().Format("150405.000")
	// Three of ours, plus whatever else the shared database holds:
	// count only ours, and place them at the very end (newest) so any
	// foreign rows come first and prove the cursor walks past them.
	ids := []string{"cs_cur_a_" + tag, "cs_cur_b_" + tag, "cs_cur_c_" + tag}
	for _, id := range ids {
		s.TryRecordProcessedEvent(ctx, pendingSessionProvider, id)
		defer s.DeleteProcessedEvent(ctx, pendingSessionProvider, id)
	}
	var seen []string
	stubStripe(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/checkout/sessions/cs_cur_") {
			seen = append(seen, strings.TrimPrefix(r.URL.Path, "/v1/checkout/sessions/"))
		}
		// Still unpaid: stays pending.
		http.Error(w, `{"error":{"type":"invalid_request_error","message":"no such session"}}`, http.StatusNotFound)
	})
	h := &StripeHandler{Store: s, pendingBatchLimit: 2}
	for i := 0; i < 50 && len(seen) < 3; i++ {
		h.SyncPendingCheckouts(ctx)
	}
	if len(seen) < 3 {
		t.Fatalf("rounds with a cap of 2 never reached the newest rows: %v", seen)
	}
}
