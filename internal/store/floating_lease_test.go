package store_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
)

func floatingLicense(t *testing.T, s *store.Store, ctx context.Context) *model.License {
	t.Helper()
	suffix := time.Now().Format("150405.000000")
	product := &model.Product{Name: "Float Test", Slug: "float-test-" + suffix, Type: "desktop"}
	if err := s.CreateProduct(ctx, product); err != nil {
		t.Fatalf("create product: %v", err)
	}
	plan := &model.Plan{
		ProductID: product.ID, Name: "Float", Slug: "float-plan-" + suffix,
		LicenseType: "perpetual", LicenseModel: "floating",
		MaxActivations: 1, FloatingTimeout: 30,
	}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatalf("create plan: %v", err)
	}
	lic := &model.License{
		ProductID: product.ID, PlanID: plan.ID, Email: "float-" + suffix + "@example.com",
		LicenseKey: "FLOATKEY-" + suffix, Status: "active",
	}
	if err := s.CreateLicense(ctx, lic); err != nil {
		t.Fatalf("create license: %v", err)
	}
	return lic
}

func checkOut(t *testing.T, s *store.Store, ctx context.Context, licenseID, identifier string, ttl time.Duration, max int) (*model.FloatingSession, error) {
	t.Helper()
	sess := &model.FloatingSession{
		LicenseID: licenseID, Identifier: identifier,
		ExpiresAt: time.Now().Add(ttl),
	}
	_, err := s.CheckOutFloatingWithLimit(ctx, sess, max)
	return sess, err
}

func expireSessions(t *testing.T, s *store.Store, ctx context.Context, licenseID, identifier string) {
	t.Helper()
	if _, err := s.DB.NewRaw(
		"UPDATE floating_sessions SET expires_at = now() - interval '1 minute' WHERE license_id = ? AND identifier = ?",
		licenseID, identifier,
	).Exec(ctx); err != nil {
		t.Fatalf("expire session: %v", err)
	}
}

// A machine that fell off the network for longer than the lease has
// lost its slot, and a heartbeat must not hand it back.
//
// The slot it used to hold has by then been checked out by somebody
// else. Renewing the lapsed row would put the license over its limit,
// with neither call ever seeing the other, which defeats the only
// thing a floating license promises.
func TestHeartbeatDoesNotReviveAnExpiredSession(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()
	lic := floatingLicense(t, s, ctx)

	if _, err := checkOut(t, s, ctx, lic.ID, "laptop-a", time.Hour, 1); err != nil {
		t.Fatalf("first checkout: %v", err)
	}
	expireSessions(t, s, ctx, lic.ID, "laptop-a")

	// The freed slot goes to another machine.
	if _, err := checkOut(t, s, ctx, lic.ID, "laptop-b", time.Hour, 1); err != nil {
		t.Fatalf("takeover checkout: %v", err)
	}

	renewed, err := s.HeartbeatFloating(ctx, lic.ID, "laptop-a", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if renewed {
		t.Error("heartbeat renewed a lease that had already lapsed")
	}

	active, err := s.CountActiveFloating(ctx, lic.ID)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if active != 1 {
		t.Errorf("active sessions = %d, want 1 (the limit)", active)
	}
}

// Heartbeating an identifier that never checked out is not a success
// either: the client has nothing to extend and has to check out.
func TestHeartbeatUnknownIdentifier(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()
	lic := floatingLicense(t, s, ctx)

	renewed, err := s.HeartbeatFloating(ctx, lic.ID, "never-checked-out", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if renewed {
		t.Error("heartbeat reported success for an identifier with no session")
	}
}

// A live lease is extended, which is the case the endpoint exists for.
func TestHeartbeatExtendsALiveSession(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()
	lic := floatingLicense(t, s, ctx)

	if _, err := checkOut(t, s, ctx, lic.ID, "laptop-a", 2*time.Minute, 1); err != nil {
		t.Fatalf("checkout: %v", err)
	}
	newExpiry := time.Now().Add(time.Hour)
	renewed, err := s.HeartbeatFloating(ctx, lic.ID, "laptop-a", newExpiry)
	if err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if !renewed {
		t.Fatal("heartbeat did not extend a running lease")
	}
	var extended bool
	if err := s.DB.NewRaw(
		"SELECT expires_at > now() + interval '30 minutes' FROM floating_sessions WHERE license_id = ? AND identifier = ?",
		lic.ID, "laptop-a",
	).Scan(ctx, &extended); err != nil {
		t.Fatalf("read expiry: %v", err)
	}
	if !extended {
		t.Error("lease was reported renewed but its expiry did not move")
	}
}

// The other half of the same story: a machine whose lease lapsed must
// be able to take a slot again.
//
// (license_id, identifier) is unique and the lapsed row stays on the
// table until the cleanup sweep, so a plain insert here failed and the
// machine could not come back at all. Intermittent connectivity is the
// normal case for a floating client, not an edge one.
func TestCheckOutAfterOwnLeaseExpired(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()
	lic := floatingLicense(t, s, ctx)

	first, err := checkOut(t, s, ctx, lic.ID, "laptop-a", time.Hour, 1)
	if err != nil {
		t.Fatalf("first checkout: %v", err)
	}
	expireSessions(t, s, ctx, lic.ID, "laptop-a")

	again, err := checkOut(t, s, ctx, lic.ID, "laptop-a", time.Hour, 1)
	if err != nil {
		t.Fatalf("checkout after own lease expired: %v", err)
	}
	if again.ID != first.ID {
		t.Errorf("session id changed from %s to %s; one machine should keep one row", first.ID, again.ID)
	}

	active, err := s.CountActiveFloating(ctx, lic.ID)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if active != 1 {
		t.Errorf("active sessions = %d, want 1", active)
	}

	var rows int
	if err := s.DB.NewRaw(
		"SELECT count(*) FROM floating_sessions WHERE license_id = ? AND identifier = ?",
		lic.ID, "laptop-a",
	).Scan(ctx, &rows); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if rows != 1 {
		t.Errorf("machine has %d rows, want 1", rows)
	}
}

// Coming back must still respect the cap: if the slot was taken while
// this machine was away, it waits rather than squeezing in beside it.
func TestCheckOutAfterExpiryStillRespectsTheLimit(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()
	lic := floatingLicense(t, s, ctx)

	if _, err := checkOut(t, s, ctx, lic.ID, "laptop-a", time.Hour, 1); err != nil {
		t.Fatalf("first checkout: %v", err)
	}
	expireSessions(t, s, ctx, lic.ID, "laptop-a")
	if _, err := checkOut(t, s, ctx, lic.ID, "laptop-b", time.Hour, 1); err != nil {
		t.Fatalf("takeover: %v", err)
	}

	if _, err := checkOut(t, s, ctx, lic.ID, "laptop-a", time.Hour, 1); err == nil {
		t.Error("expired machine checked out while the only slot was held by another")
	}

	active, err := s.CountActiveFloating(ctx, lic.ID)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if active != 1 {
		t.Errorf("active sessions = %d, want 1", active)
	}
}

// One machine asking twice at once is renewing, not competing.
//
// The check for "this machine already holds a lease" used to run
// before the license lock, so two requests from the same client could
// both miss it: one took the slot and the other counted that slot and
// refused it. A client retrying a timed-out checkout was told the
// license it already holds is full.
func TestConcurrentCheckOutFromOneMachineDoesNotRefuseItself(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()
	lic := floatingLicense(t, s, ctx)

	// The window is narrow, so the attempts are released from a
	// barrier and the whole thing is repeated: one round that happens
	// to serialise proves nothing.
	const rounds, attempts = 25, 8
	seen := map[string]bool{}
	for round := 0; round < rounds; round++ {
		start := make(chan struct{})
		errs := make(chan error, attempts)
		ids := make(chan string, attempts)
		var wg sync.WaitGroup
		for i := 0; i < attempts; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				sess, err := checkOut(t, s, ctx, lic.ID, "one-machine", time.Hour, 1)
				errs <- err
				ids <- sess.ID
			}()
		}
		close(start)
		wg.Wait()
		close(errs)
		close(ids)

		for err := range errs {
			if err != nil {
				t.Fatalf("round %d: a concurrent checkout from the same machine was refused: %v", round, err)
			}
		}
		for id := range ids {
			seen[id] = true
		}
	}
	if len(seen) != 1 {
		t.Errorf("one machine ended up with %d session ids, want 1", len(seen))
	}

	var rows int
	if err := s.DB.NewRaw(
		"SELECT count(*) FROM floating_sessions WHERE license_id = ?", lic.ID,
	).Scan(ctx, &rows); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if rows != 1 {
		t.Errorf("license has %d session rows, want 1", rows)
	}
}

// And the cap still holds when the contenders really are different
// machines: exactly maxSessions of them get in, no matter how many ask
// at once.
func TestConcurrentCheckOutRespectsTheCap(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()
	lic := floatingLicense(t, s, ctx)

	// floatingLicense caps at 1; widen it for this one.
	const max = 3
	const contenders = 12
	granted := make(chan bool, contenders)
	var wg sync.WaitGroup
	for i := 0; i < contenders; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			_, err := checkOut(t, s, ctx, lic.ID, fmt.Sprintf("machine-%02d", n), time.Hour, max)
			granted <- err == nil
		}(i)
	}
	wg.Wait()
	close(granted)

	ok := 0
	for g := range granted {
		if g {
			ok++
		}
	}
	if ok != max {
		t.Errorf("%d of %d contenders got a slot, want exactly %d", ok, contenders, max)
	}
	active, err := s.CountActiveFloating(ctx, lic.ID)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if active != max {
		t.Errorf("active sessions = %d, want %d", active, max)
	}
}

// The admin list's "in use" number has to mean what the plan means by
// it. A floating plan keeps its seats in floating_sessions, so reading
// activations reports an empty license however full it is, which is
// exactly backwards on the screen an admin checks for spare capacity.
func TestFloatingLicenseReportsSessionsNotActivations(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()
	lic := floatingLicense(t, s, ctx)

	// floatingLicense caps at 1; two seats make the counts distinct.
	if _, err := s.DB.NewRaw("UPDATE plans SET max_activations = 3 WHERE id = ?", lic.PlanID).Exec(ctx); err != nil {
		t.Fatalf("widen plan: %v", err)
	}
	for _, id := range []string{"seat-a", "seat-b"} {
		if _, err := checkOut(t, s, ctx, lic.ID, id, time.Hour, 3); err != nil {
			t.Fatalf("checkout %s: %v", id, err)
		}
	}

	out, _, err := s.ListLicenses(ctx, store.LicenseListFilter{ProductID: lic.ProductID, Limit: 10})
	if err != nil {
		t.Fatalf("ListLicenses: %v", err)
	}
	var row *model.License
	for _, l := range out {
		if l.ID == lic.ID {
			row = l
		}
	}
	if row == nil {
		t.Fatal("the license is missing from its own product's list")
	}
	if row.ActiveSessionCount != 2 {
		t.Errorf("ActiveSessionCount = %d, want 2", row.ActiveSessionCount)
	}
	if row.ActivationCount != 0 {
		t.Errorf("ActivationCount = %d, want 0: floating seats are not activations", row.ActivationCount)
	}

	// The detail read has to agree with the row.
	one, err := s.FindLicenseByIDWithCounts(ctx, lic.ID)
	if err != nil {
		t.Fatalf("FindLicenseByIDWithCounts: %v", err)
	}
	if one.ActiveSessionCount != 2 {
		t.Errorf("detail ActiveSessionCount = %d, want 2", one.ActiveSessionCount)
	}

	// An expired seat is not in use, so it must not be counted.
	expireSessions(t, s, ctx, lic.ID, "seat-a")
	one, err = s.FindLicenseByIDWithCounts(ctx, lic.ID)
	if err != nil {
		t.Fatalf("FindLicenseByIDWithCounts: %v", err)
	}
	if one.ActiveSessionCount != 1 {
		t.Errorf("after one lease lapsed ActiveSessionCount = %d, want 1", one.ActiveSessionCount)
	}
}

// A standard plan has no sessions, and must not be asked about them.
func TestStandardLicenseHasNoSessionCount(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()
	_, ids := seedSortableLicenses(t, s, ctx)

	act := &model.Activation{LicenseID: ids[0], Identifier: ids[0] + "-dev", IdentifierType: "device"}
	if err := s.CreateActivation(ctx, act); err != nil {
		t.Fatalf("create activation: %v", err)
	}
	one, err := s.FindLicenseByIDWithCounts(ctx, ids[0])
	if err != nil {
		t.Fatalf("FindLicenseByIDWithCounts: %v", err)
	}
	if one.ActivationCount != 1 {
		t.Errorf("ActivationCount = %d, want 1", one.ActivationCount)
	}
	if one.ActiveSessionCount != 0 {
		t.Errorf("ActiveSessionCount = %d on a standard plan, want 0", one.ActiveSessionCount)
	}
}
