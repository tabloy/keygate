package payment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
	"github.com/tabloy/keygate/internal/testsupport"
)

// aboutDays reports whether a period ends roughly that many days out.
func aboutDays(got *time.Time, days int) bool {
	return got != nil && got.Sub(time.Now().AddDate(0, 0, days)).Abs() < time.Minute
}

func seedMaintenancePlan(t *testing.T, s *store.Store, ctx context.Context, tag string) *model.Plan {
	t.Helper()
	plan := seedPlan(t, s, ctx, tag, "perpetual")
	plan.UpdatesDays, plan.RenewalDays, plan.StripeRenewalPriceID = 365, 365, "price_renew_"+plan.Slug
	if err := s.UpdatePlan(ctx, plan); err != nil {
		t.Fatalf("update plan: %v", err)
	}
	return plan
}

// seedPerpetualLicense creates a license with exactly the given
// period. A nil period is a lifetime grant made after creation (the
// database fills a bounded plan's period on insert, as an admin's
// later clear is the only way to a NULL on such a plan).
func seedPerpetualLicense(t *testing.T, s *store.Store, ctx context.Context, plan *model.Plan, updatesUntil *time.Time) *model.License {
	t.Helper()
	lic := &model.License{
		ProductID: plan.ProductID, PlanID: plan.ID, Email: plan.Slug + "@example.com",
		LicenseKey: "KEY-" + plan.Slug, Status: model.StatusActive,
		PaymentProvider: "stripe", UpdatesUntil: updatesUntil,
	}
	if err := s.CreateLicense(ctx, lic); err != nil {
		t.Fatalf("create license: %v", err)
	}
	if updatesUntil == nil {
		if _, err := s.DB.NewRaw("UPDATE licenses SET updates_until = NULL WHERE id = ?", lic.ID).Exec(ctx); err != nil {
			t.Fatalf("clear period: %v", err)
		}
	}
	return lic
}

func renewalMeta(sessionID, licenseID string, days int) map[string]string {
	return map[string]string{"session_id": sessionID, metaKind: kindRenewal, metaLicenseID: licenseID, metaRenewalDays: fmt.Sprint(days)}
}

func updatesUntil(t *testing.T, s *store.Store, ctx context.Context, id string) *time.Time {
	t.Helper()
	lic, err := s.FindLicenseByID(ctx, id)
	if err != nil {
		t.Fatalf("find license: %v", err)
	}
	return lic.UpdatesUntil
}

func sameInstant(a, b time.Time) bool { return a.Sub(b).Abs() < 2*time.Second }

// auditCountAtLeast waits for the asynchronous audit writer to reach
// want rows, then returns the count it saw.
func auditCountAtLeast(s *store.Store, ctx context.Context, entityID, action string, want int) int {
	var n int
	for range 40 {
		s.DB.NewRaw("SELECT count(*) FROM audit_logs WHERE entity_id = ? AND action = ?", entityID, action).Scan(ctx, &n)
		if n >= want {
			return n
		}
		time.Sleep(50 * time.Millisecond)
	}
	return n
}

func errCode(out map[string]any) string {
	e, _ := out["error"].(map[string]any)
	code, _ := e["code"].(string)
	return code
}

// A paid renewal extends updates_until by the days bought, once: the
// same session applied again (webhook retry, success page, sync)
// changes nothing, and the license itself is untouched.
func TestRenewal_ExtendsOnceAndIsIdempotent(t *testing.T) {
	s, ctx := openStore(t)
	defer s.Close()
	plan := seedMaintenancePlan(t, s, ctx, "renew")
	current := time.Now().Add(100 * 24 * time.Hour).Truncate(time.Second)
	lic := seedPerpetualLicense(t, s, ctx, plan, &current)
	h := &StripeHandler{Store: s}
	sess := "cs_renew_" + plan.Slug

	ok, err := h.fulfillCheckout(ctx, lic.Email, "", "", "pi_renew_"+plan.Slug, renewalMeta(sess, lic.ID, 365), "webhook")
	if err != nil || !ok {
		t.Fatalf("renewal not applied: ok=%v err=%v", ok, err)
	}
	want := current.Add(365 * 24 * time.Hour)
	if got := updatesUntil(t, s, ctx, lic.ID); got == nil || !sameInstant(*got, want) {
		t.Fatalf("updates_until after renewal: got %v want %v", got, want)
	}
	for _, source := range []string{"webhook", "verify", "sync"} {
		ok, err := h.fulfillCheckout(ctx, lic.Email, "", "", "pi_renew_"+plan.Slug, renewalMeta(sess, lic.ID, 365), source)
		if err != nil || !ok {
			t.Fatalf("%s replay: ok=%v err=%v", source, ok, err)
		}
	}
	if got := updatesUntil(t, s, ctx, lic.ID); !sameInstant(*got, want) {
		t.Fatalf("replay moved updates_until to %v", got)
	}
	var n int
	s.DB.NewRaw("SELECT count(*) FROM license_renewals WHERE license_id = ?", lic.ID).Scan(ctx, &n)
	if n != 1 {
		t.Fatalf("renewal rows: %d", n)
	}
	got, _ := s.FindLicenseByID(ctx, lic.ID)
	if got.Status != model.StatusActive || got.ValidUntil != nil {
		t.Fatalf("license changed by renewal: status=%s valid_until=%v", got.Status, got.ValidUntil)
	}
	var licenses int
	s.DB.NewRaw("SELECT count(*) FROM licenses WHERE stripe_checkout_session_id = ?", sess).Scan(ctx, &licenses)
	if licenses != 0 {
		t.Fatalf("renewal session created a license")
	}
}

// A renewal after the period ended starts from now, not from the old
// end: the lapsed months are not sold.
func TestRenewal_AfterExpiryStartsNow(t *testing.T) {
	s, ctx := openStore(t)
	defer s.Close()
	plan := seedMaintenancePlan(t, s, ctx, "late")
	past := time.Now().Add(-30 * 24 * time.Hour)
	lic := seedPerpetualLicense(t, s, ctx, plan, &past)
	h := &StripeHandler{Store: s}

	ok, err := h.fulfillCheckout(ctx, lic.Email, "", "", "pi_late_"+plan.Slug, renewalMeta("cs_late_"+plan.Slug, lic.ID, 90), "verify")
	if err != nil || !ok {
		t.Fatalf("renewal not applied: ok=%v err=%v", ok, err)
	}
	want := time.Now().Add(90 * 24 * time.Hour)
	if got := updatesUntil(t, s, ctx, lic.ID); got == nil || !sameInstant(*got, want) {
		t.Fatalf("late renewal: got %v want about %v", got, want)
	}
}

// A refunded renewal is taken back out of the period. With nothing
// bought since, the previous end is restored exactly; with a later
// renewal on top, only the refunded days go. The license stays active
// either way, and a second refund event changes nothing.
func TestRenewal_RefundRevertsOnlyTheRenewal(t *testing.T) {
	s, ctx := openStore(t)
	defer s.Close()
	plan := seedMaintenancePlan(t, s, ctx, "rrf")
	current := time.Now().Add(50 * 24 * time.Hour).Truncate(time.Second)
	lic := seedPerpetualLicense(t, s, ctx, plan, &current)
	h := &StripeHandler{Store: s}
	renew := func(n string, days int) {
		if ok, err := h.fulfillCheckout(ctx, lic.Email, "", "", "pi_"+n+"_"+plan.Slug, renewalMeta("cs_"+n+"_"+plan.Slug, lic.ID, days), "webhook"); err != nil || !ok {
			t.Fatalf("renewal %s: ok=%v err=%v", n, ok, err)
		}
	}
	refund := func(n string) {
		raw := fmt.Sprintf(`{"id":"ch_%s_%s","payment_intent":"pi_%s_%s","refunded":true}`, n, plan.Slug, n, plan.Slug)
		if err := h.onChargeRefunded(ctx, []byte(raw)); err != nil {
			t.Fatalf("refund %s: %v", n, err)
		}
	}

	renew("a", 365)
	refund("a")
	if got := updatesUntil(t, s, ctx, lic.ID); got == nil || !sameInstant(*got, current) {
		t.Fatalf("refund did not restore previous end: got %v want %v", got, current)
	}
	refund("a")
	if got := updatesUntil(t, s, ctx, lic.ID); !sameInstant(*got, current) {
		t.Fatalf("second refund event moved updates_until to %v", got)
	}

	renew("b", 100)
	renew("c", 200)
	refund("b")
	want := current.Add(200 * 24 * time.Hour)
	if got := updatesUntil(t, s, ctx, lic.ID); !sameInstant(*got, want) {
		t.Fatalf("stacked refund: got %v want %v", got, want)
	}
	got, _ := s.FindLicenseByID(ctx, lic.ID)
	if got.Status != model.StatusActive {
		t.Fatalf("renewal refund revoked the license: %s", got.Status)
	}
	if n := auditCountAtLeast(s, ctx, lic.ID, "updates_renewal_refunded", 2); n != 2 {
		t.Fatalf("refund audit rows: %d", n)
	}
	// A renewal refund on a customer with exactly one license must not
	// fall through to the customer match that revokes purchases.
	if n := auditCount(s, ctx, lic.ID, "revoked"); n != 0 {
		t.Fatalf("renewal refund recorded a revocation")
	}
}

// The full path: the checkout.session.completed event for a renewal
// session, signed like Stripe's, through the Webhook handler.
func TestRenewal_WebhookEventApplies(t *testing.T) {
	s, ctx := openStore(t)
	defer s.Close()
	plan := seedMaintenancePlan(t, s, ctx, "rwh")
	current := time.Now().Add(10 * 24 * time.Hour).Truncate(time.Second)
	lic := seedPerpetualLicense(t, s, ctx, plan, &current)
	h := &StripeHandler{Store: s}
	secret := "whsec_test_" + plan.Slug
	h.SetWebhookSecret(secret)
	obj := map[string]any{
		"id": "cs_rwh_" + plan.Slug, "object": "checkout.session", "mode": "payment",
		"payment_status": "paid", "payment_intent": "pi_rwh_" + plan.Slug,
		"customer_details": map[string]any{"email": lic.Email},
		"metadata":         map[string]any{metaKind: kindRenewal, metaLicenseID: lic.ID, metaRenewalDays: "365"},
	}
	code, out := signedWebhook(t, h, secret, "evt_rwh_"+plan.Slug, "checkout.session.completed", obj)
	if code != http.StatusOK || out["received"] != true {
		t.Fatalf("webhook: %d %v", code, out)
	}
	want := current.Add(365 * 24 * time.Hour)
	if got := updatesUntil(t, s, ctx, lic.ID); got == nil || !sameInstant(*got, want) {
		t.Fatalf("updates_until: got %v want %v", got, want)
	}
	if s.IsEventProcessed(ctx, pendingSessionProvider, "cs_rwh_"+plan.Slug) {
		t.Fatalf("applied renewal left a pending marker")
	}
}

// RenewUpdates refuses when there is nothing to sell and otherwise
// starts a payment-mode session that names the license and freezes
// the days bought.
func TestRenewUpdates_Endpoint(t *testing.T) {
	s, ctx := openStore(t)
	testsupport.LockSettings(t, s.DB)
	defer s.Close()
	gin.SetMode(gin.TestMode)
	plan := seedMaintenancePlan(t, s, ctx, "rep")
	current := time.Now().Add(10 * 24 * time.Hour)
	lic := seedPerpetualLicense(t, s, ctx, plan, &current)
	forever := seedPerpetualLicense(t, s, ctx, &model.Plan{ID: plan.ID, ProductID: plan.ProductID, Slug: plan.Slug + "-life"}, nil)
	// Selling renewals is gated on the operator switch.
	if err := s.SetSettings(ctx, map[string]string{store.SettingMaintenanceFeatures: "true"}); err != nil {
		t.Fatal(err)
	}
	h := &StripeHandler{Store: s, BaseURL: "https://keygate.example"}

	call := func(email, licenseID string) (int, map[string]any) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/portal/updates/renew", strings.NewReader(fmt.Sprintf(`{"license_id":%q}`, licenseID)))
		c.Request.Header.Set("Content-Type", "application/json")
		c.Set("email", email)
		h.RenewUpdates(c)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	if code, _ := call("someone-else@example.com", lic.ID); code != http.StatusForbidden {
		t.Fatalf("other user's license: %d", code)
	}
	// The login normalises the address to lower case; Stripe may have
	// stored it with capitals. Ownership must not depend on case.
	if code, out := call(strings.ToUpper(forever.Email), forever.ID); code == http.StatusForbidden {
		t.Fatalf("owner refused because of address case: %d %v", code, out)
	}
	if code, out := call(forever.Email, forever.ID); code != http.StatusBadRequest || errCode(out) != "RENEWAL_NOT_AVAILABLE" {
		t.Fatalf("updates-for-life license: %d %v", code, out)
	}

	var form url.Values
	priceType := "one_time"
	priceActive := true
	stubStripe(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/prices/"):
			// The renewal price is read before the session is made.
			fmt.Fprintf(w, `{"id":%q,"object":"price","type":%q,"active":%t}`,
				strings.TrimPrefix(r.URL.Path, "/v1/prices/"), priceType, priceActive)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/checkout/sessions":
			body, _ := io.ReadAll(r.Body)
			form, _ = url.ParseQuery(string(body))
			fmt.Fprint(w, `{"id":"cs_rep_1","object":"checkout.session","url":"https://checkout.stripe.com/c/pay/cs_rep_1"}`)
		default:
			t.Errorf("unexpected Stripe call %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	code, out := call(lic.Email, lic.ID)
	if code != http.StatusOK {
		t.Fatalf("renew: %d %v", code, out)
	}
	if data, _ := out["data"].(map[string]any); data == nil || data["url"] != "https://checkout.stripe.com/c/pay/cs_rep_1" {
		t.Fatalf("renew response: %v", out)
	}
	checks := map[string]string{
		"mode": "payment", "line_items[0][price]": plan.StripeRenewalPriceID, "customer_email": lic.Email,
		"metadata[kind]": kindRenewal, "metadata[license_id]": lic.ID, "metadata[renewal_days]": "365",
		"payment_intent_data[metadata][kind]": kindRenewal, "payment_intent_data[metadata][license_id]": lic.ID,
		"success_url": "https://keygate.example/checkout/success?session_id={CHECKOUT_SESSION_ID}",
	}
	for k, want := range checks {
		if got := form.Get(k); got != want {
			t.Errorf("session param %s: got %q want %q", k, got, want)
		}
	}

	// A recurring or archived price cannot back a one-time renewal;
	// the customer gets a clear refusal instead of a 500 from Stripe.
	priceType = "recurring"
	if code, out := call(lic.Email, lic.ID); code != http.StatusServiceUnavailable || errCode(out) != "RENEWAL_UNAVAILABLE" {
		t.Fatalf("recurring renewal price: %d %v", code, out)
	}
	priceType, priceActive = "one_time", false
	if code, out := call(lic.Email, lic.ID); code != http.StatusServiceUnavailable || errCode(out) != "RENEWAL_UNAVAILABLE" {
		t.Fatalf("archived renewal price: %d %v", code, out)
	}
	priceActive = true

	plan.StripeRenewalPriceID = ""
	if err := s.UpdatePlan(ctx, plan); err != nil {
		t.Fatalf("update plan: %v", err)
	}
	if code, out := call(lic.Email, lic.ID); code != http.StatusBadRequest || errCode(out) != "RENEWAL_NOT_AVAILABLE" {
		t.Fatalf("plan without renewal: %d %v", code, out)
	}
}

// A period that had lapsed is revived by renewal A, B is stacked on
// it, and both are refunded. The ledger replay must land back on the
// original lapsed date, not on A's purchase time.
func TestRenewal_RefundsOfRevivedPeriodRestoreOriginalEnd(t *testing.T) {
	s, ctx := openStore(t)
	defer s.Close()
	plan := seedMaintenancePlan(t, s, ctx, "rev")
	expired := time.Now().Add(-40 * 24 * time.Hour).Truncate(time.Second)
	lic := seedPerpetualLicense(t, s, ctx, plan, &expired)
	h := &StripeHandler{Store: s}
	renew := func(n string, days int) {
		if ok, err := h.fulfillCheckout(ctx, lic.Email, "", "", "pi_"+n+"_"+plan.Slug, renewalMeta("cs_"+n+"_"+plan.Slug, lic.ID, days), "webhook"); err != nil || !ok {
			t.Fatalf("renewal %s: ok=%v err=%v", n, ok, err)
		}
	}
	refund := func(n string) {
		raw := fmt.Sprintf(`{"id":"ch_%s_%s","payment_intent":"pi_%s_%s","refunded":true}`, n, plan.Slug, n, plan.Slug)
		if err := h.onChargeRefunded(ctx, []byte(raw)); err != nil {
			t.Fatalf("refund %s: %v", n, err)
		}
	}
	renew("a", 365)
	renew("b", 100)
	aEnd := time.Now().Add(365 * 24 * time.Hour)
	if got := updatesUntil(t, s, ctx, lic.ID); !sameInstant(*got, aEnd.Add(100*24*time.Hour)) {
		t.Fatalf("after a+b: %v", got)
	}
	refund("a")
	// B alone on a lapsed period starts from its own purchase time.
	if got := updatesUntil(t, s, ctx, lic.ID); !sameInstant(*got, time.Now().Add(100*24*time.Hour)) {
		t.Fatalf("after refunding a: %v", got)
	}
	refund("b")
	if got := updatesUntil(t, s, ctx, lic.ID); got == nil || !sameInstant(*got, expired) {
		t.Fatalf("after refunding both: got %v want original %v", got, expired)
	}
}

// An admin edit between renewals is kept: the refund then only takes
// its own days off the edited end.
func TestRenewal_RefundKeepsAdminEdit(t *testing.T) {
	s, ctx := openStore(t)
	defer s.Close()
	plan := seedMaintenancePlan(t, s, ctx, "adm")
	current := time.Now().Add(20 * 24 * time.Hour).Truncate(time.Second)
	lic := seedPerpetualLicense(t, s, ctx, plan, &current)
	h := &StripeHandler{Store: s}
	if ok, err := h.fulfillCheckout(ctx, lic.Email, "", "", "pi_adm_"+plan.Slug, renewalMeta("cs_adm_"+plan.Slug, lic.ID, 365), "webhook"); err != nil || !ok {
		t.Fatalf("renewal: %v %v", ok, err)
	}
	edited := time.Now().Add(1000 * 24 * time.Hour).Truncate(time.Second)
	if _, err := s.DB.NewRaw("UPDATE licenses SET updates_until = ? WHERE id = ?", edited, lic.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if err := h.onChargeRefunded(ctx, fmt.Appendf(nil, `{"id":"ch_adm","payment_intent":"pi_adm_%s","refunded":true}`, plan.Slug)); err != nil {
		t.Fatal(err)
	}
	if got := updatesUntil(t, s, ctx, lic.ID); !sameInstant(*got, edited.Add(-365*24*time.Hour)) {
		t.Fatalf("refund after admin edit: got %v want %v", got, edited.Add(-365*24*time.Hour))
	}
}

// A license granted updates for life while its renewal checkout was
// pending has nothing to extend. The paid session is not fulfilled:
// no ledger row, claim released, so it stays pending for an operator.
// Refunding it in Stripe closes the loop: the refund event leaves the
// marker, and the next attempt records the renewal as refunded.
func TestRenewal_LifetimeLicenseIsNotFulfilled(t *testing.T) {
	s, ctx := openStore(t)
	defer s.Close()
	plan := seedMaintenancePlan(t, s, ctx, "life")
	lic := seedPerpetualLicense(t, s, ctx, plan, nil)
	h := &StripeHandler{Store: s}
	sess, pi := "cs_life_"+plan.Slug, "pi_life_"+plan.Slug
	ok, err := h.fulfillCheckout(ctx, lic.Email, "", "", pi, renewalMeta(sess, lic.ID, 365), "sync")
	if err != nil || ok {
		t.Fatalf("renewal on a lifetime license must not be fulfilled: ok=%v err=%v", ok, err)
	}
	if got := updatesUntil(t, s, ctx, lic.ID); got != nil {
		t.Fatalf("lifetime license got a finite period: %v", got)
	}
	if _, err := s.FindLicenseRenewalBySession(ctx, sess); err == nil {
		t.Fatal("an unapplied renewal must leave no ledger row")
	}
	if done, _ := h.sessionFulfilled(ctx, sess); done {
		t.Fatal("session must not read as fulfilled")
	}
	if n := auditCount(s, ctx, lic.ID, "updates_renewal_ineligible"); n != 1 {
		t.Fatalf("ineligible audit rows: %d", n)
	}
	// Retries change nothing and do not pile up audit rows: the
	// session stays pending and is re-tried every sync round.
	for range 3 {
		if ok, err := h.fulfillCheckout(ctx, lic.Email, "", "", pi, renewalMeta(sess, lic.ID, 365), "sync"); err != nil || ok {
			t.Fatalf("retry: ok=%v err=%v", ok, err)
		}
	}
	time.Sleep(300 * time.Millisecond)
	if n := auditCount(s, ctx, lic.ID, "updates_renewal_ineligible"); n != 1 {
		t.Fatalf("ineligible audit rows after retries: %d", n)
	}
	// The operator refunds it in Stripe.
	raw := fmt.Sprintf(`{"id":"ch_life","payment_intent":"%s","refunded":true,"metadata":{"kind":"renewal","license_id":"%s"}}`, pi, lic.ID)
	if err := h.onChargeRefunded(ctx, []byte(raw)); err != nil {
		t.Fatal(err)
	}
	if ok, err := h.fulfillCheckout(ctx, lic.Email, "", "", pi, renewalMeta(sess, lic.ID, 365), "sync"); err != nil || !ok {
		t.Fatalf("after refund: ok=%v err=%v", ok, err)
	}
	r, err := s.FindLicenseRenewalBySession(ctx, sess)
	if err != nil || r.RefundedAt == nil || r.UpdatesUntil != nil {
		t.Fatalf("refunded renewal not recorded: %+v %v", r, err)
	}
	if got := updatesUntil(t, s, ctx, lic.ID); got != nil {
		t.Fatalf("license changed: %v", got)
	}
	if left, _ := s.HasProcessedEvent(ctx, renewalIneligibleProvider, sess); left {
		t.Fatal("ineligible marker left behind after the session was settled")
	}
}

// A license moved off its perpetual plan while the checkout was
// pending is just as ineligible.
func TestRenewal_NonPerpetualLicenseIsNotFulfilled(t *testing.T) {
	s, ctx := openStore(t)
	defer s.Close()
	plan := seedMaintenancePlan(t, s, ctx, "nonperp")
	current := time.Now().Add(30 * 24 * time.Hour).Truncate(time.Second)
	lic := seedPerpetualLicense(t, s, ctx, plan, &current)
	sub := &model.Plan{ProductID: plan.ProductID, Name: "Sub", Slug: "sub-" + plan.Slug, LicenseType: "subscription", LicenseModel: "standard"}
	if err := s.CreatePlan(ctx, sub); err != nil {
		t.Fatal(err)
	}
	// A stale date on a subscription license, as a row written before
	// the trigger that now clears one on a plan change would look.
	// The move clears it; putting it back without touching plan_id
	// leaves exactly that row.
	if _, err := s.DB.NewRaw("UPDATE licenses SET plan_id = ? WHERE id = ?", sub.ID, lic.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.NewRaw("UPDATE licenses SET updates_until = ? WHERE id = ?", current, lic.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	h := &StripeHandler{Store: s}
	ok, err := h.fulfillCheckout(ctx, lic.Email, "", "", "pi_nonperp_"+plan.Slug, renewalMeta("cs_nonperp_"+plan.Slug, lic.ID, 365), "webhook")
	if err != nil || ok {
		t.Fatalf("renewal on a subscription license must not be fulfilled: ok=%v err=%v", ok, err)
	}
	if got := updatesUntil(t, s, ctx, lic.ID); !sameInstant(*got, current) {
		t.Fatalf("stale period was extended: %v", got)
	}
}

// Stripe may deliver charge.refunded before checkout.session.completed
// is processed. The charge carries the renewal metadata, so it must
// neither reach the purchase path (which would revoke the customer's
// only license) nor be forgotten: the renewal, applied later, is
// reverted immediately.
func TestRenewal_RefundBeforeFulfilment(t *testing.T) {
	s, ctx := openStore(t)
	defer s.Close()
	plan := seedMaintenancePlan(t, s, ctx, "early")
	current := time.Now().Add(30 * 24 * time.Hour).Truncate(time.Second)
	lic := seedPerpetualLicense(t, s, ctx, plan, &current)
	if _, err := s.DB.NewRaw("UPDATE licenses SET stripe_customer_id = ? WHERE id = ?", "cus_early_"+plan.Slug, lic.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	h := &StripeHandler{Store: s}
	pi := "pi_early_" + plan.Slug
	raw := fmt.Sprintf(`{"id":"ch_early","customer":"cus_early_%s","payment_intent":"%s","refunded":true,"metadata":{"kind":"renewal","license_id":"%s"}}`, plan.Slug, pi, lic.ID)
	if err := h.onChargeRefunded(ctx, []byte(raw)); err != nil {
		t.Fatalf("early refund: %v", err)
	}
	got, _ := s.FindLicenseByID(ctx, lic.ID)
	if got.Status != model.StatusActive {
		t.Fatalf("early renewal refund revoked the customer's only license: %s", got.Status)
	}
	if ok, err := h.fulfillCheckout(ctx, lic.Email, "", "", pi, renewalMeta("cs_early_"+plan.Slug, lic.ID, 365), "webhook"); err != nil || !ok {
		t.Fatalf("late completion: %v %v", ok, err)
	}
	if got := updatesUntil(t, s, ctx, lic.ID); !sameInstant(*got, current) {
		t.Fatalf("refunded renewal was applied: %v", got)
	}
	r, err := s.FindLicenseRenewalBySession(ctx, "cs_early_"+plan.Slug)
	if err != nil || r.RefundedAt == nil || r.UpdatesUntil != nil {
		t.Fatalf("ledger row should be refunded with no effect: %+v %v", r, err)
	}
	if ok, err := h.fulfillCheckout(ctx, lic.Email, "", "", pi, renewalMeta("cs_early_"+plan.Slug, lic.ID, 365), "verify"); err != nil || !ok {
		t.Fatalf("replay: %v %v", ok, err)
	}
	if got := updatesUntil(t, s, ctx, lic.ID); !sameInstant(*got, current) {
		t.Fatalf("replay applied a refunded renewal: %v", got)
	}
	// A refund of an unknown payment intent without renewal metadata
	// still takes the purchase path (customer fallback). Stripe is
	// asked for invoice payments on the way; none exist.
	stubStripe(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/invoice_payments" {
			t.Errorf("unexpected Stripe call %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		fmt.Fprint(w, `{"object":"list","has_more":false,"url":"/v1/invoice_payments","data":[]}`)
	})
	if err := h.onChargeRefunded(ctx, fmt.Appendf(nil, `{"id":"ch_buy","customer":"cus_early_%s","payment_intent":"pi_buy_%s","refunded":true}`, plan.Slug, plan.Slug)); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.FindLicenseByID(ctx, lic.ID); got.Status != model.StatusRevoked {
		t.Fatalf("purchase refund did not revoke: %s", got.Status)
	}
}

// An early refund is the first ledger row. It must still record the
// end the license had, or a later renewal's refund would replay from
// nothing and grant updates for life instead of restoring the lapsed
// date.
func TestRenewal_EarlyRefundKeepsLedgerBaseline(t *testing.T) {
	s, ctx := openStore(t)
	defer s.Close()
	plan := seedMaintenancePlan(t, s, ctx, "base")
	expired := time.Now().Add(-40 * 24 * time.Hour).Truncate(time.Second)
	lic := seedPerpetualLicense(t, s, ctx, plan, &expired)
	h := &StripeHandler{Store: s}
	refund := func(n string, meta bool) {
		m := ""
		if meta {
			m = fmt.Sprintf(`,"metadata":{"kind":"renewal","license_id":"%s"}`, lic.ID)
		}
		if err := h.onChargeRefunded(ctx, fmt.Appendf(nil, `{"id":"ch_%s","payment_intent":"pi_%s_%s","refunded":true%s}`, n, n, plan.Slug, m)); err != nil {
			t.Fatalf("refund %s: %v", n, err)
		}
	}
	renew := func(n string) {
		if ok, err := h.fulfillCheckout(ctx, lic.Email, "", "", "pi_"+n+"_"+plan.Slug, renewalMeta("cs_"+n+"_"+plan.Slug, lic.ID, 365), "webhook"); err != nil || !ok {
			t.Fatalf("renewal %s: %v %v", n, ok, err)
		}
	}
	refund("x", true) // refund first, then the completion arrives
	renew("x")
	if got := updatesUntil(t, s, ctx, lic.ID); !sameInstant(*got, expired) {
		t.Fatalf("early-refunded renewal changed the period: %v", got)
	}
	renew("y")
	if got := updatesUntil(t, s, ctx, lic.ID); !sameInstant(*got, time.Now().Add(365*24*time.Hour)) {
		t.Fatalf("renewal y: %v", got)
	}
	refund("y", false)
	if got := updatesUntil(t, s, ctx, lic.ID); got == nil || !sameInstant(*got, expired) {
		t.Fatalf("after refunding y: got %v want the original lapsed date %v", got, expired)
	}
}

// The reviewer's case end to end: a renewal recorded with no effect
// while the license had updates for life, then an admin sets a lapsed
// cutoff, the customer renews, and that renewal is refunded. The
// cutoff must come back; lifetime updates must not.
func TestRenewal_NoEffectRowDoesNotBecomeLifetimeBaseline(t *testing.T) {
	s, ctx := openStore(t)
	defer s.Close()
	plan := seedMaintenancePlan(t, s, ctx, "nbl")
	lic := seedPerpetualLicense(t, s, ctx, plan, nil) // lifetime at first
	h := &StripeHandler{Store: s}
	// A renewal refunded before it was applied is the one kind of
	// no-effect row the ledger can hold.
	if err := h.onChargeRefunded(ctx, fmt.Appendf(nil, `{"id":"ch_nbl1","payment_intent":"pi_nbl1_%s","refunded":true,"metadata":{"kind":"renewal","license_id":"%s"}}`, plan.Slug, lic.ID)); err != nil {
		t.Fatal(err)
	}
	if ok, err := h.fulfillCheckout(ctx, lic.Email, "", "", "pi_nbl1_"+plan.Slug, renewalMeta("cs_nbl1_"+plan.Slug, lic.ID, 365), "webhook"); err != nil || !ok {
		t.Fatalf("no-effect renewal: %v %v", ok, err)
	}
	cutoff := time.Now().Add(-10 * 24 * time.Hour).Truncate(time.Second)
	if _, err := s.DB.NewRaw("UPDATE licenses SET updates_until = ? WHERE id = ?", cutoff, lic.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if ok, err := h.fulfillCheckout(ctx, lic.Email, "", "", "pi_nbl2_"+plan.Slug, renewalMeta("cs_nbl2_"+plan.Slug, lic.ID, 365), "webhook"); err != nil || !ok {
		t.Fatalf("renewal: %v %v", ok, err)
	}
	if err := h.onChargeRefunded(ctx, fmt.Appendf(nil, `{"id":"ch_nbl2","payment_intent":"pi_nbl2_%s","refunded":true}`, plan.Slug)); err != nil {
		t.Fatal(err)
	}
	if got := updatesUntil(t, s, ctx, lic.ID); got == nil || !sameInstant(*got, cutoff) {
		t.Fatalf("refund granted lifetime updates instead of restoring the cutoff: got %v want %v", got, cutoff)
	}
}

// A renewed license moved to a subscription and back gets a fresh
// period; the renewals of the old period are closed with it, so their
// later refund is recorded but cannot shorten the new period.
func TestRenewal_RefundOfSupersededRenewalLeavesNewPeriod(t *testing.T) {
	s, ctx := openStore(t)
	defer s.Close()
	plan := seedMaintenancePlan(t, s, ctx, "epoch")
	current := time.Now().Add(20 * 24 * time.Hour).Truncate(time.Second)
	lic := seedPerpetualLicense(t, s, ctx, plan, &current)
	h := &StripeHandler{Store: s}
	if ok, err := h.fulfillCheckout(ctx, lic.Email, "", "", "pi_epoch_"+plan.Slug, renewalMeta("cs_epoch_"+plan.Slug, lic.ID, 365), "webhook"); err != nil || !ok {
		t.Fatalf("renewal: %v %v", ok, err)
	}
	// leave perpetual: period cleared, ledger closed
	lic.UpdatesUntil = nil
	if err := s.UpdateLicenseAndSupersedeRenewals(ctx, lic, "updates_until"); err != nil {
		t.Fatal(err)
	}
	// re-enter perpetual: fresh period
	fresh := time.Now().Add(365 * 24 * time.Hour).Truncate(time.Second)
	lic.UpdatesUntil = &fresh
	if err := s.UpdateLicenseAndSupersedeRenewals(ctx, lic, "updates_until"); err != nil {
		t.Fatal(err)
	}
	if err := h.onChargeRefunded(ctx, fmt.Appendf(nil, `{"id":"ch_epoch","payment_intent":"pi_epoch_%s","refunded":true}`, plan.Slug)); err != nil {
		t.Fatal(err)
	}
	if got := updatesUntil(t, s, ctx, lic.ID); got == nil || !sameInstant(*got, fresh) {
		t.Fatalf("refund of a superseded renewal moved the new period: got %v want %v", got, fresh)
	}
	r, err := s.FindLicenseRenewalBySession(ctx, "cs_epoch_"+plan.Slug)
	if err != nil || r.RefundedAt == nil || r.SupersededAt == nil {
		t.Fatalf("old renewal should be superseded and refunded: %+v %v", r, err)
	}
	// A renewal bought in the new period still works and refunds normally.
	if ok, err := h.fulfillCheckout(ctx, lic.Email, "", "", "pi_epoch2_"+plan.Slug, renewalMeta("cs_epoch2_"+plan.Slug, lic.ID, 100), "webhook"); err != nil || !ok {
		t.Fatalf("renewal 2: %v %v", ok, err)
	}
	if got := updatesUntil(t, s, ctx, lic.ID); !sameInstant(*got, fresh.Add(100*24*time.Hour)) {
		t.Fatalf("renewal in new period: %v", got)
	}
	if err := h.onChargeRefunded(ctx, fmt.Appendf(nil, `{"id":"ch_epoch2","payment_intent":"pi_epoch2_%s","refunded":true}`, plan.Slug)); err != nil {
		t.Fatal(err)
	}
	if got := updatesUntil(t, s, ctx, lic.ID); !sameInstant(*got, fresh) {
		t.Fatalf("refund in new period: got %v want %v", got, fresh)
	}
}

// The in-flight claim marker may outlive the commit when its cleanup
// fails. The ledger row is the proof of fulfilment: every check must
// read the session as done, retries must not re-apply, and the
// pending sweep must drop the session.
func TestRenewal_LingeringClaimMarkerReadsAsDone(t *testing.T) {
	s, ctx := openStore(t)
	defer s.Close()
	plan := seedMaintenancePlan(t, s, ctx, "linger")
	current := time.Now().Add(30 * 24 * time.Hour).Truncate(time.Second)
	lic := seedPerpetualLicense(t, s, ctx, plan, &current)
	h := &StripeHandler{Store: s}
	sess := "cs_linger_" + plan.Slug
	if ok, err := h.fulfillCheckout(ctx, lic.Email, "", "", "pi_linger_"+plan.Slug, renewalMeta(sess, lic.ID, 365), "webhook"); err != nil || !ok {
		t.Fatalf("renewal: %v %v", ok, err)
	}
	// Simulate the failed cleanup: the claim marker is back, and a
	// racing worker recorded the session as pending.
	if _, err := s.ClaimProcessedEvent(ctx, sessionClaimProvider, sess); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimProcessedEvent(ctx, pendingSessionProvider, sess); err != nil {
		t.Fatal(err)
	}
	if done, err := h.sessionFulfilled(ctx, sess); err != nil || !done {
		t.Fatalf("session with a ledger row must read as fulfilled: %v %v", done, err)
	}
	if ok, err := h.fulfillCheckout(ctx, lic.Email, "", "", "pi_linger_"+plan.Slug, renewalMeta(sess, lic.ID, 365), "sync"); err != nil || !ok {
		t.Fatalf("retry: %v %v", ok, err)
	}
	want := current.Add(365 * 24 * time.Hour)
	if got := updatesUntil(t, s, ctx, lic.ID); !sameInstant(*got, want) {
		t.Fatalf("retry re-applied the renewal: %v", got)
	}
	if err := s.DeleteFulfilledPendingSessions(ctx); err != nil {
		t.Fatal(err)
	}
	if left, _ := s.HasProcessedEvent(ctx, pendingSessionProvider, sess); left {
		t.Fatal("pending marker of an applied renewal was not swept")
	}
	// The simulated leftover is not real state; drop it so the
	// invariant check over the shared test database stays clean.
	_ = s.DeleteProcessedEvent(ctx, sessionClaimProvider, sess)
}

// A renewal that revived a lapsed period covered the lapse as well as
// its days. When an admin then shifts the end and the renewal is
// refunded, the result is the original lapsed cutoff plus the admin's
// shift — not the purchase time plus the shift.
func TestRenewal_RefundAfterEditOnRevivedPeriod(t *testing.T) {
	s, ctx := openStore(t)
	defer s.Close()
	plan := seedMaintenancePlan(t, s, ctx, "revedit")
	lapsed := time.Now().Add(-60 * 24 * time.Hour).Truncate(time.Second)
	lic := seedPerpetualLicense(t, s, ctx, plan, &lapsed)
	h := &StripeHandler{Store: s}
	if ok, err := h.fulfillCheckout(ctx, lic.Email, "", "", "pi_revedit_"+plan.Slug, renewalMeta("cs_revedit_"+plan.Slug, lic.ID, 365), "webhook"); err != nil || !ok {
		t.Fatalf("renewal: %v %v", ok, err)
	}
	got := updatesUntil(t, s, ctx, lic.ID) // about now + 365d
	shifted := got.Add(30 * 24 * time.Hour)
	if _, err := s.DB.NewRaw("UPDATE licenses SET updates_until = ? WHERE id = ?", shifted, lic.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if err := h.onChargeRefunded(ctx, fmt.Appendf(nil, `{"id":"ch_revedit","payment_intent":"pi_revedit_%s","refunded":true}`, plan.Slug)); err != nil {
		t.Fatal(err)
	}
	want := lapsed.Add(30 * 24 * time.Hour)
	if got := updatesUntil(t, s, ctx, lic.ID); got == nil || !sameInstant(*got, want) {
		t.Fatalf("refund after edit on a revived period: got %v want original cutoff + 30d = %v", got, want)
	}
}

// A plan that hands out a bounded update period is not sellable while
// the operator switch is off: a rolled-back deployment could not
// enforce the cutoff it would grant.
func TestCheckoutByPlan_BoundedPlanNeedsTheSwitch(t *testing.T) {
	s, ctx := openStore(t)
	testsupport.LockSettings(t, s.DB)
	defer s.Close()
	gin.SetMode(gin.TestMode)
	plan := seedMaintenancePlan(t, s, ctx, "sell")
	plan.UpdatesDays = 365
	if err := s.UpdatePlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	plain := seedPlan(t, s, ctx, "sellplain", "perpetual")
	h := &StripeHandler{Store: s, BaseURL: "https://keygate.example"}
	get := func(checkoutID string) (int, string) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/pay/"+checkoutID, nil)
		c.Params = gin.Params{{Key: "checkout_id", Value: checkoutID}}
		h.CheckoutByPlan(c)
		return w.Code, w.Body.String()
	}
	if err := s.SetSettings(ctx, map[string]string{store.SettingMaintenanceFeatures: "false"}); err != nil {
		t.Fatal(err)
	}
	code, body := get(plan.CheckoutID)
	if code != http.StatusServiceUnavailable || !strings.Contains(body, "temporarily unavailable") {
		t.Fatalf("bounded plan with the switch off: %d %q", code, body)
	}
	// A plan without an update period is unaffected: it gets as far as
	// the price check, which is what an unconfigured plan answers.
	if code, body := get(plain.CheckoutID); code != http.StatusServiceUnavailable || !strings.Contains(body, "payment not configured") {
		t.Fatalf("plain plan blocked by the switch: %d %q", code, body)
	}
	// Switch on: the bounded plan reaches the same price check.
	if err := s.SetSettings(ctx, map[string]string{store.SettingMaintenanceFeatures: "true"}); err != nil {
		t.Fatal(err)
	}
	if code, body := get(plan.CheckoutID); code != http.StatusServiceUnavailable || !strings.Contains(body, "payment not configured") {
		t.Fatalf("bounded plan with the switch on: %d %q", code, body)
	}
}

// The update period a purchase includes is the one that was on offer
// when the session was created. Editing the plan before the payment
// settles must not change what the customer bought — in particular it
// must not take a lifetime entitlement away from someone who paid for
// one. Sessions created outside Keygate carry no terms and fall back
// to the plan.
func TestFulfillCheckout_UpdatePeriodFrozenAtCheckout(t *testing.T) {
	s, ctx := openStore(t)
	defer s.Close()
	bounded := seedMaintenancePlan(t, s, ctx, "frozen")
	h := &StripeHandler{Store: s}
	meta := func(session, planID string, days *int) map[string]string {
		m := map[string]string{"session_id": session, "plan_id": planID}
		if days != nil {
			m[metaUpdatesDays] = fmt.Sprint(*days)
		}
		return m
	}
	days := func(n int) *int { return &n }
	period := func(email string) *time.Time {
		var u *time.Time
		if err := s.DB.NewRaw("SELECT updates_until FROM licenses WHERE email = ?", email).Scan(ctx, &u); err != nil {
			t.Fatalf("read period: %v", err)
		}
		return u
	}

	// Bought with 365 days on offer; the plan is cut to 30 before the
	// webhook lands. The customer keeps the 365 they paid for.
	sold := "sold-" + bounded.Slug + "@example.com"
	bounded.UpdatesDays = 30
	if err := s.UpdatePlan(ctx, bounded); err != nil {
		t.Fatal(err)
	}
	if ok, err := h.fulfillCheckout(ctx, sold, "", "", "pi_frozen_1_"+bounded.Slug, meta("cs_frozen_1_"+bounded.Slug, bounded.ID, days(365)), "webhook"); err != nil || !ok {
		t.Fatalf("fulfil: %v %v", ok, err)
	}
	if got := period(sold); got == nil || got.Sub(time.Now().Add(365*24*time.Hour)).Abs() > time.Minute {
		t.Fatalf("period not frozen at the purchased 365 days: %v", got)
	}

	// Bought with updates for life; the plan becomes bounded before
	// the webhook lands. The entitlement is not taken away.
	life := "life-" + bounded.Slug + "@example.com"
	bounded.UpdatesDays = 365
	if err := s.UpdatePlan(ctx, bounded); err != nil {
		t.Fatal(err)
	}
	if ok, err := h.fulfillCheckout(ctx, life, "", "", "pi_frozen_2_"+bounded.Slug, meta("cs_frozen_2_"+bounded.Slug, bounded.ID, days(0)), "webhook"); err != nil || !ok {
		t.Fatalf("fulfil: %v %v", ok, err)
	}
	if got := period(life); got != nil {
		t.Fatalf("a lifetime purchase was given a cutoff: %v", got)
	}

	// A Payment Link session carries no terms: the plan as it reads
	// now is the only thing to go on.
	link := "link-" + bounded.Slug + "@example.com"
	if ok, err := h.fulfillCheckout(ctx, link, "", "", "pi_frozen_3_"+bounded.Slug, meta("cs_frozen_3_"+bounded.Slug, bounded.ID, nil), "webhook"); err != nil || !ok {
		t.Fatalf("fulfil: %v %v", ok, err)
	}
	if got := period(link); got == nil || got.Sub(time.Now().Add(365*24*time.Hour)).Abs() > time.Minute {
		t.Fatalf("session without terms should follow the plan: %v", got)
	}
}

// A Stripe Payment Link the merchant made carries no terms. If the
// plan's period changed after that checkout was opened, the buyer was
// shown the previous one and must get it; an unchanged plan, or a
// session opened after the change, follows the plan as it reads now.
func TestFulfillCheckout_UnmanagedSessionUsesTermsAtCheckout(t *testing.T) {
	s, ctx := openStore(t)
	defer s.Close()
	plan := seedMaintenancePlan(t, s, ctx, "plink")
	h := &StripeHandler{Store: s}
	period := func(email string) *time.Time {
		var u *time.Time
		if err := s.DB.NewRaw("SELECT updates_until FROM licenses WHERE email = ?", email).Scan(ctx, &u); err != nil {
			t.Fatalf("read period: %v", err)
		}
		return u
	}
	// Payment Link metadata: no plan_id, no terms — only what the
	// handler injects. The plan is resolved from the line items in
	// production; here it is named so the test stays offline.
	link := func(session string, created time.Time) map[string]string {
		return map[string]string{
			"session_id": session, "plan_id": plan.ID,
			metaSessionCreated: fmt.Sprint(created.Unix()),
		}
	}
	about := func(got *time.Time, days int) bool {
		return got != nil && got.Sub(time.Now().AddDate(0, 0, days)).Abs() < time.Minute
	}

	// The plan has been selling 365 for a month. Backdating the
	// history it recorded at seed time keeps the checkout stamps —
	// which Stripe gives at second resolution — apart from the edits
	// below without the test having to wait for the clock.
	if _, err := s.DB.NewRaw("DELETE FROM plan_update_terms WHERE plan_id = ?", plan.ID).Exec(ctx); err != nil {
		t.Fatalf("clear terms: %v", err)
	}
	if _, err := s.DB.NewRaw(
		"INSERT INTO plan_update_terms (id, plan_id, updates_days, effective_from) VALUES (?, ?, 365, now() - interval '30 days')",
		"seeded-"+plan.ID, plan.ID,
	).Exec(ctx); err != nil {
		t.Fatalf("backdate terms: %v", err)
	}

	// Plan untouched: the current period applies.
	before := "plink-a-" + plan.Slug + "@example.com"
	if ok, err := h.fulfillCheckout(ctx, before, "", "", "pi_plink_a_"+plan.Slug, link("cs_plink_a_"+plan.Slug, time.Now().Add(-time.Hour)), "webhook"); err != nil || !ok {
		t.Fatalf("fulfil: %v %v", ok, err)
	}
	if got := period(before); !about(got, 365) {
		t.Fatalf("unchanged plan: got %v want ~365 days", got)
	}

	// The merchant cuts the period twice: 365 to 180, then to 30. A
	// checkout opened while it still said 365 must get 365, not the
	// value before the most recent edit.
	opened := time.Now().Add(-time.Hour)
	for _, days := range []int{180, 30} {
		plan.UpdatesDays = days
		if err := s.UpdatePlan(ctx, plan); err != nil {
			t.Fatal(err)
		}
	}
	old := "plink-b-" + plan.Slug + "@example.com"
	if ok, err := h.fulfillCheckout(ctx, old, "", "", "pi_plink_b_"+plan.Slug, link("cs_plink_b_"+plan.Slug, opened), "sync"); err != nil || !ok {
		t.Fatalf("fulfil: %v %v", ok, err)
	}
	if got := period(old); !about(got, 365) {
		t.Fatalf("checkout opened before two changes: got %v want ~365 days", got)
	}

	// A checkout opened after both changes gets the current period.
	// One second of clock separates it from the edits, as a Stripe
	// session stamp cannot resolve any finer.
	time.Sleep(1100 * time.Millisecond)
	fresh := "plink-c-" + plan.Slug + "@example.com"
	if ok, err := h.fulfillCheckout(ctx, fresh, "", "", "pi_plink_c_"+plan.Slug, link("cs_plink_c_"+plan.Slug, time.Now()), "webhook"); err != nil || !ok {
		t.Fatalf("fulfil: %v %v", ok, err)
	}
	if got := period(fresh); !about(got, 30) {
		t.Fatalf("checkout opened after the changes: got %v want ~30 days", got)
	}

	// Keygate's own checkout still wins over both: it carries terms.
	managed := "plink-d-" + plan.Slug + "@example.com"
	m := link("cs_plink_d_"+plan.Slug, opened)
	m[metaUpdatesDays] = "10"
	if ok, err := h.fulfillCheckout(ctx, managed, "", "", "pi_plink_d_"+plan.Slug, m, "webhook"); err != nil || !ok {
		t.Fatalf("fulfil: %v %v", ok, err)
	}
	if got := period(managed); !about(got, 10) {
		t.Fatalf("frozen terms must win: got %v want ~10 days", got)
	}
}

// A bounded checkout can be paid long after it was opened. If the
// merchant meanwhile made the plan unlimited and took the gate off
// the product's update feeds, the period the buyer paid for cannot be
// enforced any more. Issuing the license without it would grant
// updates for life — more than was sold, and irreversible — so the
// paid session stays pending for the merchant to gate the feeds again
// or refund it.
func TestFulfillCheckout_UngatedProductLeavesTheSalePending(t *testing.T) {
	s, ctx := openStore(t)
	defer s.Close()
	plan := seedMaintenancePlan(t, s, ctx, "ungate")
	h := &StripeHandler{Store: s}
	// The merchant stops selling a period and takes the gate off.
	plan.UpdatesDays, plan.RenewalDays, plan.StripeRenewalPriceID = 0, 0, ""
	if err := s.UpdatePlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.NewRaw("UPDATE products SET feed_license_required = false WHERE id = ?", plan.ProductID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	// The stale checkout carries the period that was on offer.
	session := "cs_ungated_" + plan.Slug
	meta := map[string]string{"session_id": session, "plan_id": plan.ID, metaUpdatesDays: "365"}
	email := "ungated-" + plan.Slug + "@example.com"
	ok, err := h.fulfillCheckout(ctx, email, "", "", "pi_ungated_"+plan.Slug, meta, "webhook")
	if ok || !errors.Is(err, store.ErrUpdatePeriodNotEnforceable) {
		t.Fatalf("fulfil: ok=%v err=%v want the sale left pending", ok, err)
	}
	var n int
	if err := s.DB.NewRaw("SELECT count(*) FROM licenses WHERE email = ?", email).Scan(ctx, &n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("a license was issued anyway: %d", n)
	}
	// Gating the feeds again is all it takes: the next retry fulfils
	// the sale with the period that was paid for.
	if _, err := s.DB.NewRaw("UPDATE products SET feed_license_required = true, feed_gated_at = now() - interval '30 days' WHERE id = ?", plan.ProductID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if ok, err := h.fulfillCheckout(ctx, email, "", "", "pi_ungated_"+plan.Slug, meta, "webhook"); err != nil || !ok {
		t.Fatalf("retry after gating: %v %v", ok, err)
	}
	var until *time.Time
	if err := s.DB.NewRaw("SELECT updates_until FROM licenses WHERE email = ?", email).Scan(ctx, &until); err != nil {
		t.Fatalf("read period: %v", err)
	}
	if until == nil || !aboutDays(until, 365) {
		t.Fatalf("the retry must grant the period that was sold: %v", until)
	}
}

// A product gated again while the links its public feed handed out
// are still usable cannot enforce a cutoff yet. The sale waits rather
// than issue a period the buyer could walk around — or a lifetime the
// merchant never sold — and goes through unchanged once those links
// have expired.
func TestFulfillCheckout_DrainingProductWaitsForTheLinksToExpire(t *testing.T) {
	s, ctx := openStore(t)
	defer s.Close()
	plan := seedMaintenancePlan(t, s, ctx, "drain")
	h := &StripeHandler{Store: s}
	rel := &model.Release{ProductID: plan.ProductID, Version: "1.0.0", Channel: model.ReleaseChannelStable}
	if err := s.CreateRelease(ctx, rel); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.NewRaw("UPDATE releases SET status = 'published', published_at = now() WHERE id = ?", rel.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	// Gated a moment ago: what the public feed handed out still works.
	if _, err := s.DB.NewRaw("UPDATE products SET feed_gated_at = now() WHERE id = ?", plan.ProductID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	meta := map[string]string{"session_id": "cs_draining_" + plan.Slug, "plan_id": plan.ID, metaUpdatesDays: "365"}
	email := "draining-" + plan.Slug + "@example.com"
	ok, err := h.fulfillCheckout(ctx, email, "", "", "pi_draining_"+plan.Slug, meta, "webhook")
	if ok || !errors.Is(err, store.ErrUpdatePeriodNotEnforceable) {
		t.Fatalf("fulfil: ok=%v err=%v want the sale left pending", ok, err)
	}
	var n int
	if err := s.DB.NewRaw("SELECT count(*) FROM licenses WHERE email = ?", email).Scan(ctx, &n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("a license was issued during the drain: %d", n)
	}
	// The links have expired: the same retry issues what was sold.
	if _, err := s.DB.NewRaw("UPDATE products SET feed_gated_at = now() - interval '30 days' WHERE id = ?", plan.ProductID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if ok, err := h.fulfillCheckout(ctx, email, "", "", "pi_draining_"+plan.Slug, meta, "webhook"); err != nil || !ok {
		t.Fatalf("retry after the drain: %v %v", ok, err)
	}
	var until *time.Time
	if err := s.DB.NewRaw("SELECT updates_until FROM licenses WHERE email = ?", email).Scan(ctx, &until); err != nil {
		t.Fatalf("read period: %v", err)
	}
	if until == nil || !aboutDays(until, 365) {
		t.Fatalf("the buyer must get the period that was sold: %v", until)
	}
}

// A renewal session stays payable for hours. A licence suspended or
// revoked in the meantime has nothing worth extending — and its owner
// could not use the updates anyway — so the payment is not treated as
// fulfilled: it waits for an operator to refund it or make the
// licence eligible again.
func TestRenewal_SuspendedLicenseIsNotExtended(t *testing.T) {
	s, ctx := openStore(t)
	defer s.Close()
	plan := seedMaintenancePlan(t, s, ctx, "susp")
	current := time.Now().Add(30 * 24 * time.Hour).Truncate(time.Second)
	lic := seedPerpetualLicense(t, s, ctx, plan, &current)
	if _, err := s.DB.NewRaw("UPDATE licenses SET status = ? WHERE id = ?", model.StatusSuspended, lic.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	h := &StripeHandler{Store: s}
	session := "cs_susp_" + plan.Slug
	ok, err := h.fulfillCheckout(ctx, lic.Email, "", "", "pi_susp_"+plan.Slug, renewalMeta(session, lic.ID, 365), "webhook")
	if err != nil || ok {
		t.Fatalf("renewal on a suspended licence: ok=%v err=%v", ok, err)
	}
	if got := updatesUntil(t, s, ctx, lic.ID); got == nil || !sameInstant(*got, current) {
		t.Fatalf("the period moved: %v want %v", got, current)
	}
	// Made active again, the same retry applies it.
	if _, err := s.DB.NewRaw("UPDATE licenses SET status = ? WHERE id = ?", model.StatusActive, lic.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if ok, err := h.fulfillCheckout(ctx, lic.Email, "", "", "pi_susp_"+plan.Slug, renewalMeta(session, lic.ID, 365), "webhook"); err != nil || !ok {
		t.Fatalf("retry after reinstating: ok=%v err=%v", ok, err)
	}
	if got := updatesUntil(t, s, ctx, lic.ID); got == nil || sameInstant(*got, current) {
		t.Fatalf("the reinstated licence was not extended: %v", got)
	}
}

// A Stripe Payment Link the merchant made answers to nobody: a
// customer can open and pay one while the maintenance features are
// switched off. Issuing the period would hand out a cutoff the
// replicas still serving public feeds cannot hold up, and issuing the
// licence without it would grant more than was sold — so the sale
// waits for the operator.
func TestFulfillCheckout_SwitchOffLeavesABoundedSalePending(t *testing.T) {
	s, ctx := openStore(t)
	testsupport.LockSettings(t, s.DB)
	defer s.Close()
	plan := seedMaintenancePlan(t, s, ctx, "swoff")
	h := &StripeHandler{Store: s}
	prev, _ := s.GetSetting(ctx, store.SettingMaintenanceFeatures)
	defer func() {
		if prev != "" {
			if err := s.SetSettings(ctx, map[string]string{store.SettingMaintenanceFeatures: prev}); err != nil {
				t.Errorf("restore the switch: %v", err)
			}
		}
	}()
	if err := s.SetSettings(ctx, map[string]string{store.SettingMaintenanceFeatures: "false"}); err != nil {
		t.Fatal(err)
	}

	meta := map[string]string{"session_id": "cs_swoff_" + plan.Slug, "plan_id": plan.ID, metaUpdatesDays: "365"}
	email := "swoff-" + plan.Slug + "@example.com"
	ok, err := h.fulfillCheckout(ctx, email, "", "", "pi_swoff_"+plan.Slug, meta, "webhook")
	if ok || !errors.Is(err, store.ErrUpdatePeriodNotEnforceable) {
		t.Fatalf("fulfil with the switch off: ok=%v err=%v want the sale left pending", ok, err)
	}
	var n int
	if err := s.DB.NewRaw("SELECT count(*) FROM licenses WHERE email = ?", email).Scan(ctx, &n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("a licence was issued while the features were off: %d", n)
	}
	// Switched back on, the same retry issues what was sold.
	if err := s.SetSettings(ctx, map[string]string{store.SettingMaintenanceFeatures: "true"}); err != nil {
		t.Fatal(err)
	}
	if ok, err := h.fulfillCheckout(ctx, email, "", "", "pi_swoff_"+plan.Slug, meta, "webhook"); err != nil || !ok {
		t.Fatalf("retry after switching on: %v %v", ok, err)
	}
	var until *time.Time
	if err := s.DB.NewRaw("SELECT updates_until FROM licenses WHERE email = ?", email).Scan(ctx, &until); err != nil {
		t.Fatal(err)
	}
	if until == nil || !aboutDays(until, 365) {
		t.Fatalf("the retry must grant the period that was sold: %v", until)
	}
}

// A checkout is for a licence of the kind the plan was when it was
// opened. If the plan is retyped before the payment settles, nothing
// can deliver what was bought — a one-off payment would become a
// subscription with no end, or a trial — so the sale waits for the
// merchant.
func TestFulfillCheckout_RetypedPlanLeavesTheSalePending(t *testing.T) {
	s, ctx := openStore(t)
	defer s.Close()
	plan := seedMaintenancePlan(t, s, ctx, "retyped")
	h := &StripeHandler{Store: s}
	// The plan the customer bought was perpetual with a period; it is
	// a subscription by the time the payment lands.
	if _, err := s.DB.NewRaw("UPDATE plans SET license_type = 'subscription', billing_interval = 'month', updates_days = 0, renewal_days = 0, stripe_renewal_price_id = '' WHERE id = ?", plan.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	meta := map[string]string{
		"session_id": "cs_retyped_" + plan.Slug, "plan_id": plan.ID,
		metaUpdatesDays: "365", metaLicenseType: "perpetual",
	}
	email := "retyped-" + plan.Slug + "@example.com"
	ok, err := h.fulfillCheckout(ctx, email, "", "", "pi_retyped_"+plan.Slug, meta, "webhook")
	if ok || !errors.Is(err, store.ErrPlanChanged) {
		t.Fatalf("fulfil against a retyped plan: ok=%v err=%v want the sale left pending", ok, err)
	}
	var n int
	if err := s.DB.NewRaw("SELECT count(*) FROM licenses WHERE email = ?", email).Scan(ctx, &n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("a licence of the wrong kind was issued: %d", n)
	}
	// Put the plan back and the same retry delivers what was sold.
	if _, err := s.DB.NewRaw("UPDATE plans SET license_type = 'perpetual', billing_interval = '', updates_days = 365 WHERE id = ?", plan.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if ok, err := h.fulfillCheckout(ctx, email, "", "", "pi_retyped_"+plan.Slug, meta, "webhook"); err != nil || !ok {
		t.Fatalf("retry after putting the plan back: %v %v", ok, err)
	}
	var until *time.Time
	if err := s.DB.NewRaw("SELECT updates_until FROM licenses WHERE email = ?", email).Scan(ctx, &until); err != nil {
		t.Fatal(err)
	}
	if until == nil || !aboutDays(until, 365) {
		t.Fatalf("the buyer must get the period that was sold: %v", until)
	}
}

// A metadata key that is present but blank says nothing about what
// was sold. Treating it as a frozen type would compare "" against the
// plan and leave a perfectly ordinary sale pending forever.
func TestFulfillCheckout_BlankFrozenTypeIsNotAMismatch(t *testing.T) {
	s, ctx := openStore(t)
	defer s.Close()
	plan := seedMaintenancePlan(t, s, ctx, "blanktype")
	h := &StripeHandler{Store: s}
	meta := map[string]string{
		"session_id": "cs_blanktype_" + plan.Slug, "plan_id": plan.ID,
		metaLicenseType: "  ", metaUpdatesDays: "365",
	}
	email := "blanktype-" + plan.Slug + "@example.com"
	if ok, err := h.fulfillCheckout(ctx, email, "", "", "pi_blanktype_"+plan.Slug, meta, "webhook"); err != nil || !ok {
		t.Fatalf("a blank frozen type left the sale pending: ok=%v err=%v", ok, err)
	}
	var until *time.Time
	if err := s.DB.NewRaw("SELECT updates_until FROM licenses WHERE email = ?", email).Scan(ctx, &until); err != nil {
		t.Fatal(err)
	}
	if until == nil || !aboutDays(until, 365) {
		t.Fatalf("the buyer must get the period that was sold: %v", until)
	}
}

// A Payment Link carries no frozen terms, but the session's own shape
// says which kind of licence it bought: a subscription session has a
// subscription behind it, a one-off payment does not. A plan retyped
// across that line since the checkout opened cannot deliver what was
// bought, so the sale waits.
func TestFulfillCheckout_UnmanagedSessionChecksTheSoldKind(t *testing.T) {
	s, ctx := openStore(t)
	defer s.Close()
	plan := seedMaintenancePlan(t, s, ctx, "mode")
	h := &StripeHandler{Store: s}
	// Bought as a one-off perpetual purchase through a Payment Link;
	// the plan is a subscription by the time the payment lands.
	if _, err := s.DB.NewRaw("UPDATE plans SET license_type = 'subscription', billing_interval = 'month', updates_days = 0, renewal_days = 0, stripe_renewal_price_id = '' WHERE id = ?", plan.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	meta := map[string]string{"session_id": "cs_mode_" + plan.Slug, "plan_id": plan.ID}
	email := "mode-" + plan.Slug + "@example.com"
	ok, err := h.fulfillCheckout(ctx, email, "", "", "pi_mode_"+plan.Slug, meta, "webhook")
	if ok || !errors.Is(err, store.ErrPlanChanged) {
		t.Fatalf("one-off session against a subscription plan: ok=%v err=%v want the sale left pending", ok, err)
	}
	var n int
	if err := s.DB.NewRaw("SELECT count(*) FROM licenses WHERE email = ?", email).Scan(ctx, &n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("a licence of the wrong kind was issued: %d", n)
	}
	// The same session against the plan it was bought from goes
	// through.
	if _, err := s.DB.NewRaw("UPDATE plans SET license_type = 'perpetual', billing_interval = '', updates_days = 365 WHERE id = ?", plan.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if ok, err := h.fulfillCheckout(ctx, email, "", "", "pi_mode_"+plan.Slug, meta, "webhook"); err != nil || !ok {
		t.Fatalf("retry against the plan it was bought from: %v %v", ok, err)
	}
	// The other direction — a subscription session against a plan
	// that is now perpetual — takes the same branch; it cannot be
	// driven from here because resolving that session's plan goes to
	// Stripe.
}
