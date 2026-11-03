package store_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
)

// Two webhooks for one subscription can overlap, and the one that read
// Stripe first may write last. The newest read wins: an older answer is
// dropped whatever order the writes land in, and the "only if unknown"
// writer of the background sync never replaces a recorded state.
func TestCancelState_NewestReadWins(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()
	future := time.Now().Add(20 * 24 * time.Hour)
	lic := newLicenseOn(t, s, ctx, "subscription", model.StatusActive, &future)
	stamp := func() time.Time {
		t.Helper()
		at, err := s.StripeReadStamp(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return at
	}
	ends := func() (known, value bool) {
		t.Helper()
		var v *bool
		if err := s.DB.NewRaw(`SELECT cancel_at_period_end FROM subscriptions
			WHERE license_id = ? AND cancel_state_synced_at IS NOT NULL ORDER BY created_at DESC LIMIT 1`, lic.ID).Scan(ctx, &v); err != nil || v == nil {
			return false, false
		}
		return true, *v
	}

	older := stamp() // the "canceled" webhook asks Stripe first...
	newer := stamp() // ...the "resumed" one after the customer resumed
	if err := s.SetSubscriptionCancelScheduled(ctx, lic.ID, false, newer); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSubscriptionCancelScheduled(ctx, lic.ID, true, older); err != nil {
		t.Fatal(err)
	}
	if known, v := ends(); !known || v {
		t.Fatalf("older read overwrote the newer one: known=%v ends=%v", known, v)
	}

	// The background sync only fills a gap, even with a newer read.
	if err := s.RecordSubscriptionCancelStateIfUnknown(ctx, lic.ID, true, stamp()); err != nil {
		t.Fatal(err)
	}
	if _, v := ends(); v {
		t.Fatal("sync replaced a recorded state")
	}

	// A newer webhook read does replace it.
	if err := s.SetSubscriptionCancelScheduled(ctx, lic.ID, true, stamp()); err != nil {
		t.Fatal(err)
	}
	if _, v := ends(); !v {
		t.Fatal("newer read was not recorded")
	}
}

// The licence write from a subscription read follows the same rule: the
// newest read wins whatever order the writes land in, and an unlinked
// licence still reads as unlinked, not as a stale read.
func TestUpdateLicenseFromSubscriptionRead_NewestWins(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()
	future := time.Now().Add(20 * 24 * time.Hour)
	lic := newLicenseOn(t, s, ctx, "subscription", model.StatusPastDue, &future)
	sub := "sub_read_" + lic.ID
	if _, err := s.DB.NewRaw("UPDATE licenses SET stripe_subscription_id = ? WHERE id = ?", sub, lic.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	lic.StripeSubscriptionID = sub
	stamp := func() time.Time {
		t.Helper()
		at, err := s.StripeReadStamp(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return at
	}
	status := func() string {
		t.Helper()
		st, _ := licenseState(t, s, ctx, lic.ID)
		return st
	}

	older, newer := stamp(), stamp()
	// The newer read (recovered: active) lands first...
	lic.Status = model.StatusActive
	if err := s.UpdateLicenseFromSubscriptionRead(ctx, lic, newer, "status"); err != nil {
		t.Fatal(err)
	}
	// ...the older one (still past_due) after it, and is refused.
	lic.Status = model.StatusPastDue
	if err := s.UpdateLicenseFromSubscriptionRead(ctx, lic, older, "status"); !errors.Is(err, store.ErrStaleSubscriptionRead) {
		t.Fatalf("older read: err=%v, want ErrStaleSubscriptionRead", err)
	}
	if st := status(); st != model.StatusActive {
		t.Fatalf("older read overwrote the newer one: %s", st)
	}

	// A newer read still applies.
	lic.Status = model.StatusPastDue
	if err := s.UpdateLicenseFromSubscriptionRead(ctx, lic, stamp(), "status"); err != nil {
		t.Fatal(err)
	}
	if st := status(); st != model.StatusPastDue {
		t.Fatalf("newer read not applied: %s", st)
	}

	// Unlinked meanwhile: reported as unlinked.
	if _, err := s.DB.NewRaw("UPDATE licenses SET stripe_subscription_id = NULL WHERE id = ?", lic.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateLicenseFromSubscriptionRead(ctx, lic, stamp(), "status"); !errors.Is(err, store.ErrSubscriptionUnlinked) {
		t.Fatalf("unlinked: err=%v, want ErrSubscriptionUnlinked", err)
	}
}

// A past_due notice is recorded once, and only while the licence is still
// past_due on that subscription in that episode: not after a recovery, an
// unlink, or once another episode has begun.
func TestTryRecordPastDueNotification(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()
	future := time.Now().Add(20 * 24 * time.Hour)
	entered := time.Now().Add(-time.Hour)
	lic := newLicenseOn(t, s, ctx, "subscription", model.StatusPastDue, &future)
	sub := "sub_pastdue_" + lic.ID
	if _, err := s.DB.NewRaw("UPDATE licenses SET stripe_subscription_id = ?, past_due_at = ? WHERE id = ?", sub, entered, lic.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	tag := func(at time.Time) string { return "payment_failed:" + at.Format("20060102150405") }

	if ok, err := s.TryRecordPastDueNotification(ctx, lic.ID, sub, entered, tag(entered)); !ok || err != nil {
		t.Fatalf("past_due on its subscription: not recorded (%v)", err)
	}
	if ok, _ := s.TryRecordPastDueNotification(ctx, lic.ID, sub, entered, tag(entered)); ok {
		t.Fatal("recorded twice")
	}
	other := entered.Add(-24 * time.Hour)
	if ok, _ := s.TryRecordPastDueNotification(ctx, lic.ID, sub, other, tag(other)); ok {
		t.Fatal("recorded for an episode the licence is not in")
	}
	if ok, _ := s.TryRecordPastDueNotification(ctx, lic.ID, "sub_other", entered, tag(entered)+":x"); ok {
		t.Fatal("recorded for a subscription the licence is not on")
	}
	if _, err := s.DB.NewRaw("UPDATE licenses SET status = 'active', past_due_at = NULL WHERE id = ?", lic.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.TryRecordPastDueNotification(ctx, lic.ID, sub, entered, tag(entered)+":y"); ok {
		t.Fatal("recorded after the licence recovered")
	}
}

// A write driven by the subscription answers to the licence's own state:
// payment gives back what non-payment or expiry took, never what an
// operator took, nor revives an ended subscription from a payload — and a
// refused write changes nothing, dates included.
func TestStripeWritesRespectLicenceState(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()
	until := time.Now().Add(10 * 24 * time.Hour).Truncate(time.Second)
	later := until.Add(30 * 24 * time.Hour)
	stamp := func() time.Time {
		t.Helper()
		at, err := s.StripeReadStamp(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return at
	}
	mk := func(status string, setup string) *model.License {
		t.Helper()
		lic := newLicenseOn(t, s, ctx, "subscription", status, &until)
		lic.StripeSubscriptionID = "sub_state_" + lic.ID
		if _, err := s.DB.NewRaw("UPDATE licenses SET stripe_subscription_id = ? WHERE id = ?", lic.StripeSubscriptionID, lic.ID).Exec(ctx); err != nil {
			t.Fatal(err)
		}
		if setup != "" {
			if _, err := s.DB.NewRaw("UPDATE licenses SET "+setup+" WHERE id = ?", lic.ID).Exec(ctx); err != nil {
				t.Fatal(err)
			}
		}
		return lic
	}
	state := func(lic *model.License) (string, string, time.Time) {
		t.Helper()
		var st, by string
		var vu time.Time
		if err := s.DB.NewRaw("SELECT status, COALESCE(suspended_by, ''), valid_until FROM licenses WHERE id = ?", lic.ID).Scan(ctx, &st, &by, &vu); err != nil {
			t.Fatal(err)
		}
		return st, by, vu
	}
	toActive := func(lic *model.License) *model.License {
		c := *lic
		c.Status, c.ValidUntil, c.SuspendedAt, c.SuspendedBy = model.StatusActive, &later, nil, ""
		return &c
	}
	cols := []string{"status", "valid_until", "suspended_at", "suspended_by"}

	revoked := mk(model.StatusRevoked, "")
	if err := s.UpdateLicenseFromSubscriptionRead(ctx, toActive(revoked), stamp(), cols...); !errors.Is(err, store.ErrLicenseRestricted) {
		t.Fatalf("revoked, current read: err=%v, want ErrLicenseRestricted", err)
	}
	if err := s.UpdateLicenseFromSubscriptionEnded(ctx, revoked, stamp(), "status"); !errors.Is(err, store.ErrLicenseRestricted) {
		t.Fatalf("revoked, deletion: err=%v, want ErrLicenseRestricted", err)
	}
	if st, _, vu := state(revoked); st != model.StatusRevoked || !vu.Equal(until) {
		t.Fatalf("revoked licence changed: %s %v", st, vu)
	}

	admin := mk(model.StatusActive, "")
	if err := s.SuspendLicense(ctx, admin.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateLicenseFromSubscriptionRead(ctx, toActive(admin), stamp(), cols...); !errors.Is(err, store.ErrLicenseRestricted) {
		t.Fatalf("operator-suspended, current read: err=%v, want ErrLicenseRestricted", err)
	}
	if err := s.UpdateLicenseFromSubscription(ctx, toActive(admin), cols...); !errors.Is(err, store.ErrLicenseRestricted) {
		t.Fatalf("operator-suspended, payload: err=%v, want ErrLicenseRestricted", err)
	}
	paused := *admin
	paused.Status, paused.SuspendedBy = model.StatusSuspended, model.SuspendedByStripe
	if err := s.UpdateLicenseFromSubscription(ctx, &paused, "status", "suspended_by"); !errors.Is(err, store.ErrLicenseRestricted) {
		t.Fatalf("pause over an operator's suspension: err=%v, want ErrLicenseRestricted", err)
	}
	if st, by, vu := state(admin); st != model.StatusSuspended || by != model.SuspendedByAdmin || !vu.Equal(until) {
		t.Fatalf("operator's suspension changed: %s by=%s %v", st, by, vu)
	}
	if err := s.UpdateLicenseFromSubscriptionEnded(ctx, &model.License{ID: admin.ID, StripeSubscriptionID: admin.StripeSubscriptionID, Status: model.StatusCanceled}, stamp(), "status"); err != nil {
		t.Fatalf("deletion of an operator-suspended licence: %v", err)
	}

	stripePaused := mk(model.StatusSuspended, "suspended_by = 'stripe', suspended_at = now()")
	if err := s.UpdateLicenseFromSubscription(ctx, toActive(stripePaused), cols...); err != nil {
		t.Fatalf("Stripe pause lifted by a payload: %v", err)
	}
	if st, by, vu := state(stripePaused); st != model.StatusActive || by != "" || !vu.Equal(later) {
		t.Fatalf("Stripe pause not lifted: %s by=%s %v", st, by, vu)
	}

	canceled := mk(model.StatusCanceled, "")
	if err := s.UpdateLicenseFromSubscription(ctx, toActive(canceled), cols...); !errors.Is(err, store.ErrLicenseRestricted) {
		t.Fatalf("canceled, payload: err=%v, want ErrLicenseRestricted", err)
	}
	if st, _, vu := state(canceled); st != model.StatusCanceled || !vu.Equal(until) {
		t.Fatalf("payload revived a canceled licence: %s %v", st, vu)
	}
	if err := s.UpdateLicenseFromSubscriptionRead(ctx, toActive(canceled), stamp(), cols...); err != nil {
		t.Fatalf("canceled (was unpaid), current read says live: %v", err)
	}
	if st, _, _ := state(canceled); st != model.StatusActive {
		t.Fatalf("current read did not restore the unpaid-canceled licence: %s", st)
	}
}

// Cancel-at-period-end must not push valid_until out after an immediate
// cancel already ended the licence: that would restore access until the
// period end on a canceled row.
func TestDateOnlyValidUntil_RefusedAfterImmediateCancel(t *testing.T) {
	s := setupTestDB(t)
	t.Cleanup(func() { s.Close() })
	ctx := context.Background()

	until := time.Now().Add(30 * 24 * time.Hour).Truncate(time.Second)
	lic := newLicenseOn(t, s, ctx, "subscription", model.StatusActive, &until)
	sub := "sub_capend_" + lic.ID
	linkSubscription(t, s, ctx, lic.ID, sub)

	endedAt := time.Now().Truncate(time.Second)
	stamp, err := s.StripeReadStamp(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateLicenseFromSubscriptionEnded(ctx, &model.License{
		ID: lic.ID, StripeSubscriptionID: sub, Status: model.StatusCanceled,
		CanceledAt: &endedAt, ValidUntil: &endedAt,
	}, stamp, "status", "canceled_at", "valid_until"); err != nil {
		t.Fatal(err)
	}

	// A later period-end stamp must still not extend the paid-through date.
	later, err := s.StripeReadStamp(ctx)
	if err != nil {
		t.Fatal(err)
	}
	periodEnd := time.Now().Add(20 * 24 * time.Hour).Truncate(time.Second)
	err = s.UpdateLicenseFromSubscriptionRead(ctx, &model.License{
		ID: lic.ID, StripeSubscriptionID: sub, Status: model.StatusActive, ValidUntil: &periodEnd,
	}, later, "valid_until")
	if !errors.Is(err, store.ErrLicenseRestricted) {
		t.Fatalf("period-end after immediate cancel: err=%v, want ErrLicenseRestricted", err)
	}
	var got time.Time
	var st string
	if err := s.DB.NewRaw("SELECT status, valid_until FROM licenses WHERE id = ?", lic.ID).Scan(ctx, &st, &got); err != nil {
		t.Fatal(err)
	}
	if st != model.StatusCanceled || !got.Equal(endedAt) {
		t.Fatalf("after refused period-end: %s until %v, want canceled until %v", st, got, endedAt)
	}

	// An active licence still accepts a stamped period-end date.
	active := newLicenseOn(t, s, ctx, "subscription", model.StatusActive, &until)
	asub := "sub_capend_ok_" + active.ID
	linkSubscription(t, s, ctx, active.ID, asub)
	okStamp, err := s.StripeReadStamp(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateLicenseFromSubscriptionRead(ctx, &model.License{
		ID: active.ID, StripeSubscriptionID: asub, Status: model.StatusActive, ValidUntil: &periodEnd,
	}, okStamp, "valid_until"); err != nil {
		t.Fatalf("period-end on an active licence: %v", err)
	}
	if err := s.DB.NewRaw("SELECT valid_until FROM licenses WHERE id = ?", active.ID).Scan(ctx, &got); err != nil || !got.Equal(periodEnd) {
		t.Fatalf("active period-end valid_until=%v (err %v), want %v", got, err, periodEnd)
	}
}

// A failed update of the subscription row fails the write: the transaction
// is aborted, so the licence update is rolled back with it.
func TestUpdateLicenseFromSubscriptionEnded_FailedSubscriptionSync(t *testing.T) {
	s := setupTestDB(t)
	t.Cleanup(func() { s.Close() })
	ctx := context.Background()
	lic := newLicenseOn(t, s, ctx, "subscription", model.StatusActive, nil)
	sub := "sub_end_" + lic.ID
	linkSubscription(t, s, ctx, lic.ID, sub)

	fn := "fail_sub_" + strings.ReplaceAll(lic.ID, "-", "")
	if _, err := s.DB.ExecContext(ctx, fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'database unavailable'; END $$`, fn)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = s.DB.ExecContext(context.Background(), fmt.Sprintf("DROP TRIGGER IF EXISTS %s ON subscriptions", fn))
		_, _ = s.DB.ExecContext(context.Background(), fmt.Sprintf("DROP FUNCTION IF EXISTS %s()", fn))
	})
	if _, err := s.DB.ExecContext(ctx, fmt.Sprintf(`CREATE TRIGGER %s BEFORE UPDATE ON subscriptions FOR EACH ROW
		WHEN (NEW.license_id = '%s') EXECUTE FUNCTION %s()`, fn, lic.ID, fn)); err != nil {
		t.Fatal(err)
	}
	err := s.UpdateLicenseFromSubscriptionEnded(ctx, &model.License{ID: lic.ID, StripeSubscriptionID: sub, Status: model.StatusCanceled}, time.Now(), "status")
	if st, _ := licenseState(t, s, ctx, lic.ID); !strings.Contains(fmt.Sprint(err), "database unavailable") || st != model.StatusActive {
		t.Fatalf("subscription update failed: err=%v, licence %s; want the trigger's error and the licence still active", err, st)
	}
}
