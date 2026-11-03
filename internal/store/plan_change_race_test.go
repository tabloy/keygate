package store_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
)

// waitForLockWaiters waits until n statements are waiting on a lock, so a
// test commits the change they wait on only once they all wait for it.
func waitForLockWaiters(t *testing.T, s *store.Store, ctx context.Context, n int) {
	t.Helper()
	for range 200 {
		var waiting int
		if err := s.DB.NewRaw(`SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(ctx, &waiting); err != nil {
			t.Fatal(err)
		}
		if waiting >= n {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("fewer than %d statements waiting on a lock", n)
}

// A plan change that commits while devices and members wait on the licence
// lock binds them to the new plan's limits, not the old plan's they read
// before the lock: the lock statement used to join the plan, and the plan
// read as missing once the change committed, so the old limit stood.
func TestLimitsFollowAPlanChangeMadeWhileWaiting(t *testing.T) {
	s := setupTestDB(t)
	t.Cleanup(func() { s.Close() })
	ctx := context.Background()
	suffix := strings.ReplaceAll(time.Now().Format("150405.000000"), ".", "")
	prod := &model.Product{Name: "Race", Slug: "race-" + suffix, Type: "hybrid"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	plan := func(name string, devices, seats int, perSeat bool) *model.Plan {
		t.Helper()
		p := &model.Plan{ProductID: prod.ID, Name: name, Slug: name + "-" + suffix, LicenseType: "perpetual",
			LicenseModel: "standard", MaxActivations: devices, MaxSeats: seats, Active: true}
		if perSeat {
			// Sold per seat only as a subscription (plans_per_seat_subscription_check).
			p.LicenseType, p.BillingInterval = "subscription", "month"
			p.PerSeat, p.MinQuantity, p.MaxQuantity = true, 1, 10
		}
		if err := s.CreatePlan(ctx, p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	big := plan("big", 5, 0, false)
	small := plan("small", 1, 2, false)
	seated := plan("seated", 1, 1, true)
	key := "KG-RACE-" + suffix
	lic := &model.License{ProductID: prod.ID, PlanID: big.ID, Email: "owner-" + suffix + "@example.com",
		LicenseKey: key, Status: model.StatusActive}
	if err := s.CreateLicense(ctx, lic); err != nil {
		t.Fatal(err)
	}

	// The change holds the licence lock; three activations and three
	// invites wait on it, each with the big plan's limits.
	change, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := change.ExecContext(ctx, "UPDATE licenses SET plan_id = ? WHERE id = ?", small.ID, lic.ID); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := range 3 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = s.ActivateForKey(ctx, &model.Activation{LicenseID: lic.ID,
				Identifier: "device-" + string(rune('a'+i)), IdentifierType: "device"}, big.MaxActivations, key)
		}()
		go func() {
			defer wg.Done()
			_, hash := store.GenerateSeatInviteToken()
			expires := time.Now().Add(time.Hour)
			_, _, _ = s.AddSeatWithinLimit(ctx, &model.Seat{LicenseID: lic.ID,
				Email: "member-" + string(rune('a'+i)) + "-" + suffix + "@example.com", Role: "member",
				InviteTokenHash: hash, InviteExpiresAt: &expires}, big.MaxSeats)
		}()
	}
	waitForLockWaiters(t, s, ctx, 6)
	if err := change.Commit(); err != nil {
		t.Fatal(err)
	}
	wg.Wait()

	if n, err := s.CountActivations(ctx, lic.ID); err != nil || n != small.MaxActivations {
		t.Errorf("activations = %d (%v) on a plan allowing %d", n, err, small.MaxActivations)
	}
	var members int
	if err := s.DB.NewRaw("SELECT count(*) FROM seats WHERE license_id = ? AND removed_at IS NULL", lic.ID).Scan(ctx, &members); err != nil {
		t.Fatal(err)
	}
	if people := members + 1; people > small.MaxSeats {
		t.Errorf("%d people, owner included, on a plan allowing %d", people, small.MaxSeats)
	}

	// A seat count set while the licence moves onto a plan sold per seat
	// is checked against that plan, not refused as not per seat.
	change, err = s.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := change.ExecContext(ctx, "UPDATE licenses SET plan_id = ? WHERE id = ?", seated.ID, lic.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := change.ExecContext(ctx, "DELETE FROM seats WHERE license_id = ?", lic.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := change.ExecContext(ctx, "DELETE FROM activations WHERE license_id = ?", lic.ID); err != nil {
		t.Fatal(err)
	}
	quantity := make(chan error, 1)
	go func() {
		_, err := s.SetLicenseQuantity(ctx, lic.ID, 3)
		quantity <- err
	}()
	waitForLockWaiters(t, s, ctx, 1)
	if err := change.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-quantity; err != nil {
		t.Errorf("seat count set while the licence moved onto a per-seat plan: %v, want it set", err)
	}

	// An expired licence given a new expiry while it moves onto a trial
	// plan takes the trial's status, as it would on that plan.
	trial := &model.Plan{ProductID: prod.ID, Name: "trial", Slug: "trial-" + suffix, LicenseType: "trial",
		LicenseModel: "standard", MaxActivations: 1, TrialDays: 14, Active: true}
	if err := s.CreatePlan(ctx, trial); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.NewRaw("UPDATE licenses SET plan_id = ?, status = 'expired', valid_until = now() - interval '1 day' WHERE id = ?",
		big.ID, lic.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	change, err = s.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := change.ExecContext(ctx, "UPDATE licenses SET plan_id = ? WHERE id = ?", trial.ID, lic.ID); err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(7 * 24 * time.Hour)
	statuses := make(chan string, 1)
	go func() {
		_, status, err := s.SetLicenseValidUntil(ctx, lic.ID, &until)
		if err != nil {
			t.Errorf("new expiry: %v", err)
		}
		statuses <- status
	}()
	waitForLockWaiters(t, s, ctx, 1)
	if err := change.Commit(); err != nil {
		t.Fatal(err)
	}
	if status := <-statuses; status != model.StatusTrialing {
		t.Errorf("new expiry while the licence moved onto a trial plan: %s, want %s", status, model.StatusTrialing)
	}
}

// A device or session that waited on the lock behind the end of the
// licence's subscription is refused: canceled, with its access ended then,
// the licence no longer works. One an operator kept usable with an expiry
// ahead still takes them.
func TestActivateAndCheckOut_RefusedOnceTheSubscriptionEnded(t *testing.T) {
	s := setupTestDB(t)
	t.Cleanup(func() { s.Close() })
	ctx := context.Background()

	ahead := time.Now().Add(30 * 24 * time.Hour)
	lic := newLicenseOn(t, s, ctx, "subscription", model.StatusActive, &ahead)
	if _, err := s.DB.NewRaw("UPDATE plans SET max_activations = 5 WHERE id = ?", lic.PlanID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	end, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := end.ExecContext(ctx, "UPDATE licenses SET status = 'canceled', canceled_at = now(), valid_until = now() WHERE id = ?", lic.ID); err != nil {
		t.Fatal(err)
	}
	activate := make(chan error, 1)
	checkout := make(chan error, 1)
	go func() {
		activate <- s.ActivateForKey(ctx, &model.Activation{LicenseID: lic.ID, Identifier: "behind", IdentifierType: "device"}, 5, lic.LicenseKey)
	}()
	go func() {
		_, err := s.CheckOutFloatingForKey(ctx, &model.FloatingSession{LicenseID: lic.ID, Identifier: "behind",
			ExpiresAt: time.Now().Add(time.Hour)}, 5, lic.LicenseKey)
		checkout <- err
	}()
	waitForLockWaiters(t, s, ctx, 2)
	if err := end.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-activate; !errors.Is(err, store.ErrKeyRevoked) {
		t.Errorf("activate behind the end: err = %v, want ErrKeyRevoked", err)
	}
	if err := <-checkout; !errors.Is(err, store.ErrKeyRevoked) {
		t.Errorf("checkout behind the end: err = %v, want ErrKeyRevoked", err)
	}

	// Kept usable by an operator: canceled, but with an expiry ahead.
	if _, err := s.DB.NewRaw("UPDATE licenses SET valid_until = ? WHERE id = ?", ahead, lic.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.ActivateForKey(ctx, &model.Activation{LicenseID: lic.ID, Identifier: "kept", IdentifierType: "device"}, 5, lic.LicenseKey); err != nil {
		t.Errorf("activate on a canceled licence with an expiry ahead: %v, want it activated", err)
	}
	var st sql.NullString
	if err := s.DB.NewRaw("SELECT status FROM licenses WHERE id = ?", lic.ID).Scan(ctx, &st); err != nil || st.String != model.StatusCanceled {
		t.Fatalf("status %v (%v), want canceled", st, err)
	}
}
