package payment

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
)

// Stripe's webhook, the success page and the periodic sync can all
// apply the same renewal session at once. Exactly one renewal row
// must result, and updates_until must move once.
func TestConcurrentRenewalAppliesOnce(t *testing.T) {
	s, ctx := openStore(t)
	defer s.Close()
	plan := seedMaintenancePlan(t, s, ctx, "rcc")
	current := time.Now().Add(30 * 24 * time.Hour).Truncate(time.Second)
	lic := seedPerpetualLicense(t, s, ctx, plan, &current)
	h := &StripeHandler{Store: s}
	sess := "cs_rcc_" + plan.Slug

	const workers = 8
	var wg sync.WaitGroup
	results := make([]bool, workers)
	errs := make([]error, workers)
	for i := range workers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = h.fulfillCheckout(ctx, lic.Email, "", "", "pi_rcc_"+plan.Slug, renewalMeta(sess, lic.ID, 365), "webhook")
		}(i)
	}
	wg.Wait()
	applied := 0
	for i := range results {
		if errs[i] != nil {
			t.Fatalf("worker %d: %v", i, errs[i])
		}
		if results[i] {
			applied++
		}
	}
	if applied == 0 {
		t.Fatalf("no worker reported the renewal applied")
	}
	var n int
	s.DB.NewRaw("SELECT count(*) FROM license_renewals WHERE license_id = ?", lic.ID).Scan(ctx, &n)
	if n != 1 {
		t.Fatalf("renewal rows: %d", n)
	}
	want := current.Add(365 * 24 * time.Hour)
	if got := updatesUntil(t, s, ctx, lic.ID); got == nil || !sameInstant(*got, want) {
		t.Fatalf("updates_until: got %v want %v", got, want)
	}
	// Two different renewals at the same instant stack.
	wg = sync.WaitGroup{}
	for _, n := range []string{"x", "y"} {
		wg.Add(1)
		go func(n string) {
			defer wg.Done()
			if ok, err := h.fulfillCheckout(ctx, lic.Email, "", "", "pi_rcc_"+n+plan.Slug, renewalMeta("cs_rcc_"+n+plan.Slug, lic.ID, 100), "verify"); err != nil || !ok {
				t.Errorf("renewal %s: ok=%v err=%v", n, ok, err)
			}
		}(n)
	}
	wg.Wait()
	want = want.Add(200 * 24 * time.Hour)
	if got := updatesUntil(t, s, ctx, lic.ID); !sameInstant(*got, want) {
		t.Fatalf("stacked concurrent renewals: got %v want %v", got, want)
	}
}

// charge.refunded and checkout.session.completed for the same renewal
// may be processed at the same time. Whatever the interleaving, the
// end state must be "renewal recorded and refunded, period unchanged":
// the refund must never be lost between the marker check and the
// commit of the renewal.
func TestConcurrentRefundAndFulfilmentNeverLoseTheRefund(t *testing.T) {
	s, ctx := openStore(t)
	defer s.Close()
	plan := seedMaintenancePlan(t, s, ctx, "rrace")
	current := time.Now().Add(30 * 24 * time.Hour).Truncate(time.Second)
	lic := seedPerpetualLicense(t, s, ctx, plan, &current)
	h := &StripeHandler{Store: s}

	for i := range 20 {
		pi := fmt.Sprintf("pi_rrace_%s_%d", plan.Slug, i)
		sess := fmt.Sprintf("cs_rrace_%s_%d", plan.Slug, i)
		refund := fmt.Appendf(nil, `{"id":"ch_rrace_%d","payment_intent":"%s","refunded":true,"metadata":{"kind":"renewal","license_id":"%s"}}`, i, pi, lic.ID)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			if ok, err := h.fulfillCheckout(ctx, lic.Email, "", "", pi, renewalMeta(sess, lic.ID, 365), "webhook"); err != nil || !ok {
				t.Errorf("round %d fulfil: ok=%v err=%v", i, ok, err)
			}
		}()
		go func() {
			defer wg.Done()
			if err := h.onChargeRefunded(ctx, refund); err != nil {
				t.Errorf("round %d refund: %v", i, err)
			}
		}()
		wg.Wait()
		if got := updatesUntil(t, s, ctx, lic.ID); got == nil || !sameInstant(*got, current) {
			t.Fatalf("round %d: refunded renewal left the period at %v (want %v)%s", i, got, current, renewalState(t, s, ctx, lic.ID, sess, pi))
		}
		r, err := s.FindLicenseRenewalBySession(ctx, sess)
		if err != nil || r.RefundedAt == nil {
			t.Fatalf("round %d: renewal not recorded as refunded: %+v %v%s", i, r, err, renewalState(t, s, ctx, lic.ID, sess, pi))
		}
		if left, _ := s.HasProcessedEvent(ctx, renewalRefundProvider, pi); left {
			t.Fatalf("round %d: early-refund marker left behind%s", i, renewalState(t, s, ctx, lic.ID, sess, pi))
		}
	}
}

// renewalState prints what the licence, its ledger and the markers
// looked like when an assertion failed. This test has failed twice in
// several dozen runs of the whole suite and never on its own, so the
// state at the moment of failure is the only way to tell which of the
// two handlers lost — printing it costs nothing until that happens.
func renewalState(t *testing.T, s *store.Store, ctx context.Context, licenseID, sessionID, paymentIntent string) string {
	t.Helper()
	var out strings.Builder
	out.WriteString("\n  state at failure:")
	var until *time.Time
	if err := s.DB.NewRaw("SELECT updates_until FROM licenses WHERE id = ?", licenseID).Scan(ctx, &until); err != nil {
		fmt.Fprintf(&out, "\n    licence: read failed: %v", err)
	} else {
		fmt.Fprintf(&out, "\n    licence updates_until: %v", until)
	}
	rows, err := s.ListLicenseRenewals(ctx, licenseID)
	if err != nil {
		fmt.Fprintf(&out, "\n    ledger: read failed: %v", err)
	}
	for _, r := range rows {
		fmt.Fprintf(&out, "\n    renewal %s: days=%d prev=%v until=%v refunded=%v superseded=%v created=%v",
			r.StripeCheckoutSessionID, r.Days, r.PreviousUpdatesUntil, r.UpdatesUntil, r.RefundedAt, r.SupersededAt, r.CreatedAt)
	}
	for _, m := range []struct{ name, provider, id string }{
		{"early-refund marker", renewalRefundProvider, paymentIntent},
		{"session done", fulfilledSessionProvider, sessionID},
		{"session claim", sessionClaimProvider, sessionID},
	} {
		has, herr := s.HasProcessedEvent(ctx, m.provider, m.id)
		fmt.Fprintf(&out, "\n    %s: %v (%v)", m.name, has, herr)
	}
	return out.String()
}

// Renewals and refunds must not hold one pooled connection for a lock
// while waiting for a second one to do the work: with a pool smaller
// than the number of concurrent handlers, that deadlocks until the
// requests time out. Two connections, eight handlers of each kind.
func TestRenewalLocksDoNotExhaustThePool(t *testing.T) {
	s, ctx := openStore(t)
	defer s.Close()
	s.DB.SetMaxOpenConns(2)
	defer s.DB.SetMaxOpenConns(0)
	plan := seedMaintenancePlan(t, s, ctx, "pool")
	current := time.Now().Add(30 * 24 * time.Hour).Truncate(time.Second)
	lic := seedPerpetualLicense(t, s, ctx, plan, &current)
	h := &StripeHandler{Store: s}

	done := make(chan struct{})
	go func() {
		var wg sync.WaitGroup
		for i := range 8 {
			pi := fmt.Sprintf("pi_pool_%s_%d", plan.Slug, i)
			sess := fmt.Sprintf("cs_pool_%s_%d", plan.Slug, i)
			wg.Add(2)
			go func() {
				defer wg.Done()
				_, _ = h.fulfillCheckout(ctx, lic.Email, "", "", pi, renewalMeta(sess, lic.ID, 10), "webhook")
			}()
			go func() {
				defer wg.Done()
				_ = h.onChargeRefunded(ctx, fmt.Appendf(nil, `{"id":"ch_pool_%d","payment_intent":"%s","refunded":true,"metadata":{"kind":"renewal","license_id":"%s"}}`, i, pi, lic.ID))
			}()
		}
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("handlers deadlocked on the connection pool")
	}
	// Every pair ended refunded with the period unchanged.
	var n int
	s.DB.NewRaw("SELECT count(*) FROM license_renewals WHERE license_id = ? AND refunded_at IS NOT NULL", lic.ID).Scan(ctx, &n)
	if n != 8 {
		t.Fatalf("refunded renewals recorded: %d", n)
	}
	if got := updatesUntil(t, s, ctx, lic.ID); !sameInstant(*got, current) {
		t.Fatalf("period after refunded renewals: %v", got)
	}
}

// A refund and an admin grant of lifetime updates touch the same
// license and renewal rows from two transactions. With mismatched
// lock orders Postgres aborts one of them with a deadlock; both must
// always succeed.
func TestRefundAndAdminEditDoNotDeadlock(t *testing.T) {
	s, ctx := openStore(t)
	defer s.Close()
	plan := seedMaintenancePlan(t, s, ctx, "dl")
	h := &StripeHandler{Store: s}
	for i := range 20 {
		current := time.Now().Add(30 * 24 * time.Hour).Truncate(time.Second)
		lic := &model.License{ProductID: plan.ProductID, PlanID: plan.ID, Email: fmt.Sprintf("dl-%d-%s@example.com", i, plan.Slug),
			LicenseKey: fmt.Sprintf("KEY-dl-%d-%s", i, plan.Slug), Status: model.StatusActive, PaymentProvider: "stripe", UpdatesUntil: &current}
		if err := s.CreateLicense(ctx, lic); err != nil {
			t.Fatal(err)
		}
		pi := fmt.Sprintf("pi_dl_%s_%d", plan.Slug, i)
		if ok, err := h.fulfillCheckout(ctx, lic.Email, "", "", pi, renewalMeta(fmt.Sprintf("cs_dl_%s_%d", plan.Slug, i), lic.ID, 365), "webhook"); err != nil || !ok {
			t.Fatalf("renewal %d: %v %v", i, ok, err)
		}
		got, _ := s.FindLicenseByID(ctx, lic.ID)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			if err := h.onChargeRefunded(ctx, fmt.Appendf(nil, `{"id":"ch_dl_%d","payment_intent":"%s","refunded":true}`, i, pi)); err != nil {
				t.Errorf("round %d refund: %v", i, err)
			}
		}()
		go func() {
			defer wg.Done()
			// Admin grants lifetime updates: license row first, then renewals.
			if _, err := s.SetLicenseUpdatesUntil(ctx, lic.ID, got.UpdatesUntil, nil, true); err != nil {
				t.Errorf("round %d admin edit: %v", i, err)
			}
		}()
		wg.Wait()
	}
}

// The ledger is replayed in created_at order and every refund is
// computed from it, so that order has to be the order the renewals
// were actually applied in. Each row is stamped after its license row
// lock, from the database clock, so several renewals landing at once
// record a contiguous chain — previous end of one row is the end the
// row before it produced. A chain with a gap reads as an admin edit
// and a later refund would restore the wrong date.
func TestConcurrentRenewalsRecordAContiguousLedger(t *testing.T) {
	s, ctx := openStore(t)
	defer s.Close()
	plan := seedMaintenancePlan(t, s, ctx, "chain")
	current := time.Now().Add(30 * 24 * time.Hour).Truncate(time.Second)
	lic := seedPerpetualLicense(t, s, ctx, plan, &current)
	h := &StripeHandler{Store: s}

	const n = 8
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pi := fmt.Sprintf("pi_chain_%s_%d", plan.Slug, i)
			sess := fmt.Sprintf("cs_chain_%s_%d", plan.Slug, i)
			if ok, err := h.fulfillCheckout(ctx, lic.Email, "", "", pi, renewalMeta(sess, lic.ID, 30), "webhook"); err != nil || !ok {
				t.Errorf("renewal %d: ok=%v err=%v", i, ok, err)
			}
		}()
	}
	wg.Wait()

	rows, err := s.ListLicenseRenewals(ctx, lic.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != n {
		t.Fatalf("ledger rows: %d want %d", len(rows), n)
	}
	for i, r := range rows {
		if i == 0 {
			continue
		}
		if !model.SameEnd(r.PreviousUpdatesUntil, rows[i-1].UpdatesUntil) {
			t.Fatalf("row %d starts at %v but the row before it ended at %v — the ledger is out of order",
				i, r.PreviousUpdatesUntil, rows[i-1].UpdatesUntil)
		}
	}
	// Replaying that chain must land exactly on the license as it is:
	// nothing in it may read as an edit someone made by hand.
	got := updatesUntil(t, s, ctx, lic.ID)
	replayed := model.ReplayRenewals(rows)
	if got == nil || replayed == nil || !sameInstant(*got, *replayed) {
		t.Fatalf("replay says %v, the license says %v", replayed, got)
	}
}

// The reservation and the in-flight marker are written together. A
// crash between them would leave a reservation alone — which is
// exactly how a finished event looks — and the next delivery would
// answer 2xx and do nothing. A refund has no sweeper behind it, so it
// would be lost for good.
func TestEventClaimLeavesNoHalfState(t *testing.T) {
	s, ctx := openStore(t)
	defer s.Close()
	h := &StripeHandler{Store: s}
	id := "evt_half_" + time.Now().Format("150405.000000")

	claimed, done, err := h.claimEvent(ctx, id)
	if err != nil || !claimed || done {
		t.Fatalf("first claim: claimed=%v done=%v err=%v", claimed, done, err)
	}
	// While it is held, a second delivery neither claims nor reads it
	// as finished.
	claimed, done, err = h.claimEvent(ctx, id)
	if err != nil || claimed || done {
		t.Fatalf("second claim while in flight: claimed=%v done=%v err=%v", claimed, done, err)
	}
	// Both rows exist, or neither: a reservation on its own is the
	// half state this must never produce.
	reserved, err := s.HasProcessedEvent(ctx, processedEventDoneProvider, id)
	if err != nil {
		t.Fatal(err)
	}
	inflight, err := s.HasProcessedEvent(ctx, processedEventClaimProvider, id)
	if err != nil {
		t.Fatal(err)
	}
	if !reserved || !inflight {
		t.Fatalf("half state after a claim: reserved=%v inflight=%v", reserved, inflight)
	}
	// Completed: the reservation stands alone and the event reads as
	// done, which is what completion means.
	if err := s.CompleteProcessedEvent(ctx, processedEventClaimProvider, id); err != nil {
		t.Fatal(err)
	}
	claimed, done, err = h.claimEvent(ctx, id)
	if err != nil || claimed || !done {
		t.Fatalf("claim after completion: claimed=%v done=%v err=%v", claimed, done, err)
	}
}
