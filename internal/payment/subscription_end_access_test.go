package payment

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stripe/stripe-go/v82"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/service"
)

// A subscription that ends ends its licence then, whatever date the
// licence held. A renewal that failed has already moved that date to the
// end of the period nobody paid for: a yearly plan whose payment retries
// ran out went on working for 338 more days.
func TestSubscriptionEndEndsAccessNow(t *testing.T) {
	f := newPauseFixture(t)
	unpaidYear := "valid_until = now() + interval '338 days', past_due_at = now() - interval '28 days'"

	deleted := f.licence("deleted", model.StatusPastDue, unpaidYear)
	if err := f.h.onSubscriptionDeleted(f.ctx, []byte(fmt.Sprintf(`{"id":%q}`, deleted.StripeSubscriptionID))); err != nil {
		t.Fatal(err)
	}
	readCanceled := f.licence("readcanceled", model.StatusPastDue, unpaidYear)
	f.stripeSays(readCanceled, "canceled")
	f.event(readCanceled)
	unpaid := f.licence("unpaid", model.StatusPastDue, unpaidYear)
	f.stripeSays(unpaid, "unpaid")
	f.event(unpaid)

	for name, lic := range map[string]*model.License{"deleted": deleted, "read canceled": readCanceled, "unpaid": unpaid} {
		got, err := f.h.Store.FindLicenseByID(f.ctx, lic.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != model.StatusCanceled || got.ValidUntil == nil || got.ValidUntil.After(time.Now()) || service.IsLicenseUsable(got) {
			t.Errorf("%s: %s until %v, usable %v; want canceled, ended now and not usable",
				name, got.Status, got.ValidUntil, service.IsLicenseUsable(got))
		}
	}

	// A deletion delivered again leaves when it ended as it was.
	first, err := f.h.Store.FindLicenseByID(f.ctx, deleted.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.h.onSubscriptionDeleted(f.ctx, []byte(fmt.Sprintf(`{"id":%q}`, deleted.StripeSubscriptionID))); err != nil {
		t.Fatal(err)
	}
	again, err := f.h.Store.FindLicenseByID(f.ctx, deleted.ID)
	if err != nil {
		t.Fatal(err)
	}
	if first.CanceledAt == nil || again.CanceledAt == nil || !again.CanceledAt.Equal(*first.CanceledAt) || !again.ValidUntil.Equal(*first.ValidUntil) {
		t.Errorf("deletion delivered again: canceled %v until %v, want %v until %v unchanged",
			again.CanceledAt, again.ValidUntil, first.CanceledAt, first.ValidUntil)
	}

	// An expiry already behind stays where it was: the end of a period
	// cancelled ahead, which the deletion arrives after.
	lapsed := f.licence("lapsed", model.StatusActive, "valid_until = date_trunc('second', now()) - interval '1 hour'")
	before, err := f.h.Store.FindLicenseByID(f.ctx, lapsed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.h.onSubscriptionDeleted(f.ctx, []byte(fmt.Sprintf(`{"id":%q}`, lapsed.StripeSubscriptionID))); err != nil {
		t.Fatal(err)
	}
	after, err := f.h.Store.FindLicenseByID(f.ctx, lapsed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != model.StatusCanceled || after.ValidUntil == nil || !after.ValidUntil.Equal(*before.ValidUntil) {
		t.Errorf("deletion after the period end: %s until %v, want canceled until %v", after.Status, after.ValidUntil, before.ValidUntil)
	}
}

// Lifting an operator's suspension hands the licence back to its
// subscription as Stripe has it now: while it was suspended, Stripe's
// writes were refused, whether a renewal paid, a pause or the end.
func TestReinstateFollowsTheSubscription(t *testing.T) {
	f := newPauseFixture(t)
	reinstate := func(lic *model.License) *model.License {
		t.Helper()
		cur, err := f.h.Store.FindLicenseByID(f.ctx, lic.ID)
		if err != nil {
			t.Fatal(err)
		}
		follow, err := f.h.FollowSubscription(f.ctx, cur)
		if err != nil {
			t.Fatalf("read before reinstating: %v", err)
		}
		if _, err := f.h.Store.ReinstateLicense(f.ctx, lic.ID); err != nil {
			t.Fatal(err)
		}
		if err := follow(f.ctx); err != nil {
			t.Fatalf("apply after reinstating: %v", err)
		}
		got, err := f.h.Store.FindLicenseByID(f.ctx, lic.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	suspended := "suspended_by = 'admin', suspended_at = now()"

	// Renewed while suspended: the date it held has passed, the paid one
	// is a month ahead.
	renewed := f.licence("renewed", model.StatusSuspended, suspended+", valid_until = now() - interval '10 days'")
	f.stripeSays(renewed, "active")
	if got := reinstate(renewed); got.Status != model.StatusActive || got.ValidUntil == nil ||
		got.ValidUntil.Before(time.Now().Add(29*24*time.Hour)) || !service.IsLicenseUsable(got) {
		t.Errorf("renewed while suspended: %s until %v; want active to the paid period's end", got.Status, got.ValidUntil)
	}

	// Paused in Stripe meanwhile: paused once lifted, a pause the
	// subscription's resume lifts.
	paused := f.licence("paused", model.StatusSuspended, suspended)
	f.stripeSays(paused, "paused")
	if got := reinstate(paused); got.Status != model.StatusSuspended || got.SuspendedBy != model.SuspendedByStripe || service.IsLicenseUsable(got) {
		t.Errorf("paused while suspended: %s by %q; want suspended by Stripe", got.Status, got.SuspendedBy)
	}

	// Ended meanwhile.
	ended := f.licence("ended", model.StatusSuspended, suspended)
	f.stripeSays(ended, "canceled")
	if got := reinstate(ended); got.Status != model.StatusCanceled || service.IsLicenseUsable(got) {
		t.Errorf("ended while suspended: %s until %v; want canceled and not usable", got.Status, got.ValidUntil)
	}

	// Past due meanwhile: within dunning, usable, and the failed payment,
	// whose event the suspension refused, is reported once.
	pastDue := f.licence("pastdue", model.StatusSuspended, suspended)
	f.stripeSays(pastDue, "past_due")
	if got := reinstate(pastDue); got.Status != model.StatusPastDue || got.PastDueAt == nil || !service.IsLicenseUsable(got) {
		t.Errorf("past due while suspended: %s since %v; want past due and usable", got.Status, got.PastDueAt)
	}
	failures := func(lic *model.License) int {
		t.Helper()
		var n int
		if err := f.h.Store.DB.NewRaw("SELECT count(*) FROM notifications WHERE license_id = ? AND tag LIKE 'payment_failed:%'", lic.ID).Scan(f.ctx, &n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := failures(pastDue); n != 1 {
		t.Errorf("past due once reinstated: %d payment failures reported, want 1", n)
	}
	if n := failures(renewed); n != 0 {
		t.Errorf("renewed once reinstated: %d payment failures reported, want none", n)
	}
}

// What the second read finds is what is applied: a pause made between
// the read before the suspension was lifted and the lift, whose event the
// suspension refused, is not missed.
func TestReinstateAppliesTheReadTakenAfterTheLift(t *testing.T) {
	f := newPauseFixture(t)
	lic := f.licence("between", model.StatusSuspended, "suspended_by = 'admin', suspended_at = now()")
	f.stripeSays(lic, "active")
	cur, err := f.h.Store.FindLicenseByID(f.ctx, lic.ID)
	if err != nil {
		t.Fatal(err)
	}
	follow, err := f.h.FollowSubscription(f.ctx, cur)
	if err != nil {
		t.Fatal(err)
	}
	f.stripeSays(lic, "paused")
	if _, err := f.h.Store.ReinstateLicense(f.ctx, lic.ID); err != nil {
		t.Fatal(err)
	}
	if err := follow(f.ctx); err != nil {
		t.Fatal(err)
	}
	if st := f.state(lic); st != "suspended:stripe" {
		t.Fatalf("paused between the read and the lift: %s, want suspended:stripe", st)
	}
}

// Cancelling at once a subscription that already ended is the end asked
// for: Stripe answers 404, and the portal answers it as done, not 500.
// Only Stripe saying the subscription ended counts.
func TestPortalImmediateCancelOfAnEndedSubscription(t *testing.T) {
	f := newUpgradeFixture(t, "cancelended")
	subID := "sub_ce_" + f.tag
	l := f.reload(t)
	l.StripeSubscriptionID, l.PaymentProvider, l.Status = subID, "stripe", model.StatusActive
	if err := f.s.UpdateLicense(f.ctx, l, "stripe_subscription_id", "payment_provider", "status"); err != nil {
		t.Fatal(err)
	}
	visible := true
	stubStripe(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && visible {
			fmt.Fprintf(w, `{"id":%q,"object":"subscription","status":"canceled"}`, subID)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","code":"resource_missing","message":"No such subscription"}}`))
	})
	cancel := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/portal/subscription/cancel",
			bytes.NewBufferString(fmt.Sprintf(`{"license_id":%q,"immediate":true}`, f.lic.ID)))
		c.Request.Header.Set("Content-Type", "application/json")
		c.Set("email", f.lic.Email)
		f.h.CancelSubscription(c)
		return w
	}

	visible = false
	if w := cancel(); w.Code != http.StatusConflict || !bytes.Contains(w.Body.Bytes(), []byte("STRIPE_NOT_VISIBLE")) {
		t.Fatalf("cancel of a subscription this key cannot see = %d %s, want 409 STRIPE_NOT_VISIBLE", w.Code, w.Body.String())
	}
	if st := f.reload(t).Status; st != model.StatusActive {
		t.Fatalf("status %s after a cancel Stripe did not confirm, want active", st)
	}

	visible = true
	if w := cancel(); w.Code != http.StatusOK {
		t.Fatalf("cancel of an ended subscription = %d %s, want 200", w.Code, w.Body.String())
	}
	got := f.reload(t)
	if got.Status != model.StatusCanceled || service.IsLicenseUsable(got) {
		t.Fatalf("after the cancel: %s, usable %v; want canceled", got.Status, service.IsLicenseUsable(got))
	}

	// Asked again: answered as done, with nothing written or told again.
	if w := cancel(); w.Code != http.StatusOK {
		t.Fatalf("second cancel = %d %s, want 200", w.Code, w.Body.String())
	}
	again := f.reload(t)
	if again.Status != model.StatusCanceled || !again.CanceledAt.Equal(*got.CanceledAt) || !again.ValidUntil.Equal(*got.ValidUntil) {
		t.Fatalf("second cancel moved the licence: canceled %v until %v, want %v until %v",
			again.CanceledAt, again.ValidUntil, got.CanceledAt, got.ValidUntil)
	}
	time.Sleep(500 * time.Millisecond) // audit lines land in the background
	var n int
	if err := f.s.DB.NewRaw("SELECT count(*) FROM audit_logs WHERE entity_id = ? AND action = 'cancel_requested'", f.lic.ID).Scan(f.ctx, &n); err != nil || n != 1 {
		t.Fatalf("%d cancel_requested lines (%v), want 1", n, err)
	}
}

// Without a Stripe key, the portal's subscription endpoints and the
// checkout success page's check answer that payments are not configured,
// and call nobody: a call to Stripe without
// a key fails, and used to be made all the same, answered as a 500.
func TestPortalSubscriptionEndpointsWithoutStripe(t *testing.T) {
	f := newUpgradeFixture(t, "nostripe")
	l := f.reload(t)
	l.StripeSubscriptionID, l.StripeCustomerID, l.PaymentProvider = "sub_ns_"+f.tag, "cus_ns_"+f.tag, "stripe"
	if err := f.s.UpdateLicense(f.ctx, l, "stripe_subscription_id", "stripe_customer_id", "payment_provider"); err != nil {
		t.Fatal(err)
	}
	calls := 0
	stubStripe(t, func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusInternalServerError)
	})
	stripe.Key = "" // stubStripe sets one and puts the previous back afterwards
	call := func(method, path, body string, handle gin.HandlerFunc) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		var req *http.Request
		if body != "" {
			req = httptest.NewRequest(method, path, bytes.NewBufferString(body))
			req.Header.Set("Content-Type", "application/json")
		} else {
			req = httptest.NewRequest(method, path, nil)
		}
		c.Request = req
		c.Set("email", f.lic.Email)
		handle(c)
		return w
	}
	body := fmt.Sprintf(`{"license_id":%q,"immediate":true}`, f.lic.ID)
	for name, w := range map[string]*httptest.ResponseRecorder{
		"cancel":          call(http.MethodPost, "/portal/subscription/cancel", body, f.h.CancelSubscription),
		"billing portal":  call(http.MethodPost, "/portal/subscription/billing-portal", body, f.h.CreatePortalSession),
		"invoices":        call(http.MethodGet, "/portal/subscription/invoices?license_id="+f.lic.ID, "", f.h.ListInvoices),
		"checkout verify": call(http.MethodGet, "/checkout/verify?session_id=cs_test_none", "", f.h.VerifyCheckoutSession),
	} {
		if w.Code != http.StatusServiceUnavailable || !bytes.Contains(w.Body.Bytes(), []byte("PAYMENTS_UNAVAILABLE")) {
			t.Errorf("%s without Stripe = %d %s, want 503 PAYMENTS_UNAVAILABLE", name, w.Code, w.Body.String())
		}
	}
	if calls != 0 {
		t.Errorf("%d calls to Stripe without a key, want none", calls)
	}
	if st := f.reload(t).Status; st != model.StatusActive {
		t.Errorf("status %s after a cancel that could not be made, want active", st)
	}
}
