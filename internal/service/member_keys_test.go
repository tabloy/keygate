package service

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
	"github.com/tabloy/keygate/pkg/apperr"
)

// A member's own key works everywhere the licence's key does, keeps to
// the member's own devices, and stops with everything it activated when
// the member is removed.
func TestMemberKeyThroughTheSDK(t *testing.T) {
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
	suffix := time.Now().Format("150405.000000")
	prod := &model.Product{Name: "Team app", Slug: "member-key-" + suffix, Type: "hybrid"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	plan := &model.Plan{ProductID: prod.ID, Name: "Team", Slug: "member-key-" + suffix, LicenseType: "perpetual",
		LicenseModel: "standard", MaxActivations: 5, MaxSeats: 5, Active: true}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	ownerKey := "KG-MEMBER-" + suffix
	lic := &model.License{ProductID: prod.ID, PlanID: plan.ID, Email: "owner-" + suffix + "@example.com",
		LicenseKey: ownerKey, Status: model.StatusActive}
	if err := s.CreateLicense(ctx, lic); err != nil {
		t.Fatal(err)
	}
	plain, hash := store.GenerateSeatInviteToken()
	expires := time.Now().Add(time.Hour)
	seat := &model.Seat{LicenseID: lic.ID, Email: "alice-" + suffix + "@example.com", Role: "member",
		InviteTokenHash: hash, InviteExpiresAt: &expires}
	if _, _, err := s.AddSeatWithinLimit(ctx, seat, 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AcceptSeatInvite(ctx, plain); err != nil {
		t.Fatal(err)
	}
	seat, _ = s.FindSeatByID(ctx, seat.ID)
	aliceKey, err := s.MemberKey(ctx, seat)
	if err != nil || aliceKey == "" {
		t.Fatalf("member key %q: %v", aliceKey, err)
	}

	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	ls := NewLicenseService(s, priv, slog.Default(), nil, nil)
	seats := NewSeatService(s, NewWebhookService(s, slog.Default(), time.Second, 1, true), nil, slog.Default(), "")
	notFound := func(err error) bool {
		var ae *apperr.AppError
		return errors.As(err, &ae) && ae.Code == "LICENSE_NOT_FOUND"
	}
	activate := func(key, device string) error {
		_, err := ls.Activate(ctx, ActivateInput{LicenseKey: key, Identifier: device})
		return err
	}
	owned := func(device string) (string, bool) {
		a, err := s.FindActivation(ctx, lic.ID, device)
		if err != nil {
			return "", false
		}
		return a.SeatID, true
	}

	for _, d := range []struct{ key, device string }{
		{ownerKey, "owner-pc"}, {aliceKey, "alice-pc"}, {aliceKey, "alice-laptop"},
	} {
		if err := activate(d.key, d.device); err != nil {
			t.Fatalf("activate %s: %v", d.device, err)
		}
	}
	if who, _ := owned("alice-pc"); who != seat.ID {
		t.Fatalf("alice-pc is %q's, want alice's", who)
	}
	// The licence's key may ask about a member's device, which stays hers.
	if err := activate(ownerKey, "alice-pc"); err != nil {
		t.Fatalf("activate alice's device with the licence's key: %v", err)
	}
	if who, _ := owned("alice-pc"); who != seat.ID {
		t.Fatalf("alice-pc moved to %q", who)
	}
	if who, _ := owned("owner-pc"); who != "" {
		t.Fatalf("owner-pc is %q's, want the owner's", who)
	}
	if _, err := ls.Verify(ctx, VerifyInput{LicenseKey: aliceKey, Identifier: "alice-pc"}); err != nil {
		t.Fatalf("verify with the member key: %v", err)
	}
	// A member's key vouches for the member's devices only; the
	// licence's key for any.
	if _, err := ls.Verify(ctx, VerifyInput{LicenseKey: aliceKey, Identifier: "owner-pc"}); !notFound(err) {
		t.Fatalf("verify the owner's device with alice's key = %v, want LICENSE_NOT_FOUND", err)
	}
	if _, err := ls.Verify(ctx, VerifyInput{LicenseKey: ownerKey, Identifier: "alice-pc"}); err != nil {
		t.Fatalf("verify alice's device with the licence's key: %v", err)
	}

	// A machine activated with the licence's key before members had keys
	// of their own, when members used that key, activated again with
	// the member's, moves to the member, who can then release it and
	// takes it with them.
	if err := activate(ownerKey, "legacy-pc"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.NewRaw("UPDATE activations SET predates_member_keys = true WHERE license_id = ? AND identifier = 'legacy-pc'",
		lic.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	// The owner's device from now on is not a member's to take.
	var ae *apperr.AppError
	if err := activate(aliceKey, "owner-pc"); !errors.As(err, &ae) || ae.Code != "DEVICE_HELD" {
		t.Fatalf("activate the owner's device with alice's key = %v, want DEVICE_HELD", err)
	}
	if err := activate(aliceKey, "legacy-pc"); err != nil {
		t.Fatalf("re-activate with the member key: %v", err)
	}
	if who, _ := owned("legacy-pc"); who != seat.ID {
		t.Fatalf("legacy-pc is %q's, want moved to alice", who)
	}
	if _, err := ls.Verify(ctx, VerifyInput{LicenseKey: aliceKey, Identifier: "legacy-pc"}); err != nil {
		t.Fatalf("verify the moved device with alice's key: %v", err)
	}
	// Audit rows are written in the background.
	var moves int
	for range 40 {
		if err := s.DB.NewRaw("SELECT count(*) FROM audit_logs WHERE entity_id = ? AND action = 'device_moved_to_member'",
			lic.ID).Scan(ctx, &moves); err != nil {
			t.Fatal(err)
		}
		if moves > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if moves != 1 {
		t.Fatalf("%d move audits, want 1", moves)
	}

	// A member's key releases only the member's own devices.
	if err := ls.Deactivate(ctx, DeactivateInput{LicenseKey: aliceKey, Identifier: "owner-pc"}); !notFound(err) {
		t.Fatalf("member key releasing the owner's device = %v, want LICENSE_NOT_FOUND", err)
	}
	if _, ok := owned("owner-pc"); !ok {
		t.Fatal("the member key released the owner's device")
	}
	if err := ls.Deactivate(ctx, DeactivateInput{LicenseKey: aliceKey, Identifier: "alice-laptop"}); err != nil {
		t.Fatalf("member key releasing their own device: %v", err)
	}

	// Over the seat limit (the plan lowered to the owner alone), alice is
	// paused: activating says so, verifying answers as any key that may
	// not be used does; the owner carries on. Room again, she does too.
	if _, err := s.DB.NewRaw("UPDATE plans SET max_seats = 1 WHERE id = ?", plan.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	var paused *apperr.AppError
	if err := activate(aliceKey, "alice-new"); !errors.As(err, &paused) || paused.Code != "SEAT_PAUSED" || paused.Status != 403 {
		t.Fatalf("activate while paused = %v, want 403 SEAT_PAUSED", err)
	}
	if _, err := ls.Verify(ctx, VerifyInput{LicenseKey: aliceKey, Identifier: "alice-pc"}); !notFound(err) {
		t.Fatalf("verify while paused = %v, want LICENSE_NOT_FOUND", err)
	}
	if _, err := ls.Verify(ctx, VerifyInput{LicenseKey: ownerKey, Identifier: "owner-pc"}); err != nil {
		t.Fatalf("the owner stopped with the member paused: %v", err)
	}
	// Usage, entitlements and releases go through the same gate: the
	// paused key is refused before the feature is even looked up.
	us := NewUsageService(s, NewWebhookService(s, slog.Default(), time.Second, 1, true), nil, slog.Default(), 0.8)
	if _, err := us.GetQuotaStatus(ctx, aliceKey, "api_calls", ""); !notFound(err) {
		t.Fatalf("usage while paused = %v, want LICENSE_NOT_FOUND", err)
	}
	if _, err := us.GetQuotaStatus(ctx, ownerKey, "api_calls", ""); notFound(err) {
		t.Fatalf("usage for the owner = %v, want the licence found", err)
	}
	if listed, err := seats.ListSeats(ctx, ownerKey, "", lic.Email); err != nil || len(listed) != 1 || !listed[0].Paused {
		t.Fatalf("seats listed while alice is paused: %v, %+v; want her marked paused", err, listed)
	}
	if _, err := s.DB.NewRaw("UPDATE plans SET max_seats = 5 WHERE id = ?", plan.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := ls.Verify(ctx, VerifyInput{LicenseKey: aliceKey, Identifier: "alice-pc"}); err != nil {
		t.Fatalf("verify once there is room again: %v", err)
	}

	// Removed, the member's key and devices stop.
	if err := seats.RemoveSeat(ctx, ownerKey, seat.ID, "", "", lic.Email); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"alice-pc", "legacy-pc"} {
		if _, ok := owned(d); ok {
			t.Fatalf("a removed member's device %s stayed", d)
		}
	}
	if _, err := ls.Verify(ctx, VerifyInput{LicenseKey: aliceKey, Identifier: "alice-pc"}); !notFound(err) {
		t.Fatalf("verify with a removed member's key = %v, want LICENSE_NOT_FOUND", err)
	}
	if err := activate(aliceKey, "alice-pc"); !notFound(err) {
		t.Fatalf("activate with a removed member's key = %v, want LICENSE_NOT_FOUND", err)
	}
	if _, err := ls.Verify(ctx, VerifyInput{LicenseKey: ownerKey, Identifier: "owner-pc"}); err != nil {
		t.Fatalf("the owner's device stopped with the member: %v", err)
	}
}

// Floating sessions through the SDK with members' keys: another
// member's session is refused at checkout, not renewed, and not
// released; a removed member's key checks nothing out.
func TestMemberKeyFloatingThroughTheSDK(t *testing.T) {
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
	suffix := time.Now().Format("150405.000000")
	prod := &model.Product{Name: "Team floating", Slug: "member-fl-" + suffix, Type: "hybrid"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	plan := &model.Plan{ProductID: prod.ID, Name: "Float", Slug: "member-fl-" + suffix, LicenseType: "perpetual",
		LicenseModel: "floating", FloatingTimeout: 30, MaxActivations: 5, MaxSeats: 5, Active: true}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	ownerKey := "KG-MEMBERFL-" + suffix
	lic := &model.License{ProductID: prod.ID, PlanID: plan.ID, Email: "owner-fl-" + suffix + "@example.com",
		LicenseKey: ownerKey, Status: model.StatusActive}
	if err := s.CreateLicense(ctx, lic); err != nil {
		t.Fatal(err)
	}
	member := func(who string) (*model.Seat, string) {
		t.Helper()
		plain, hash := store.GenerateSeatInviteToken()
		expires := time.Now().Add(time.Hour)
		seat := &model.Seat{LicenseID: lic.ID, Email: who + "-fl-" + suffix + "@example.com", Role: "member",
			InviteTokenHash: hash, InviteExpiresAt: &expires}
		if _, _, err := s.AddSeatWithinLimit(ctx, seat, 0); err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.AcceptSeatInvite(ctx, plain); err != nil {
			t.Fatal(err)
		}
		seat, _ = s.FindSeatByID(ctx, seat.ID)
		key, err := s.MemberKey(ctx, seat)
		if err != nil || key == "" {
			t.Fatalf("member key: %v", err)
		}
		return seat, key
	}
	alice, aliceKey := member("alice")
	_, bobKey := member("bob")
	fs := NewFloatingService(s, slog.Default(), nil)
	code := func(err error) string {
		var ae *apperr.AppError
		if errors.As(err, &ae) {
			return ae.Code
		}
		return ""
	}

	if _, err := fs.CheckOut(ctx, CheckOutInput{LicenseKey: aliceKey, Identifier: "alice-ws"}); err != nil {
		t.Fatalf("alice checks out: %v", err)
	}
	if _, err := fs.CheckOut(ctx, CheckOutInput{LicenseKey: bobKey, Identifier: "alice-ws"}); code(err) != "SESSION_HELD" {
		t.Fatalf("bob checking out alice's machine = %v, want SESSION_HELD", err)
	}
	if _, err := fs.Heartbeat(ctx, bobKey, "alice-ws", ""); code(err) != "SESSION_NOT_FOUND" {
		t.Fatalf("bob renewing alice's session = %v, want SESSION_NOT_FOUND", err)
	}
	if err := fs.CheckIn(ctx, bobKey, "alice-ws", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.Heartbeat(ctx, aliceKey, "alice-ws", ""); err != nil {
		t.Fatalf("bob's check-in ended alice's session: %v", err)
	}
	if _, err := fs.Heartbeat(ctx, ownerKey, "alice-ws", ""); err != nil {
		t.Fatalf("the licence's key renewing alice's session: %v", err)
	}

	seats := NewSeatService(s, NewWebhookService(s, slog.Default(), time.Second, 1, true), nil, slog.Default(), "")
	if err := seats.RemoveSeat(ctx, ownerKey, alice.ID, "", "", lic.Email); err != nil {
		t.Fatal(err)
	}
	if active, _ := s.CountActiveFloating(ctx, lic.ID); active != 0 {
		t.Fatalf("%d sessions left after alice was removed, want 0", active)
	}
	if _, err := fs.CheckOut(ctx, CheckOutInput{LicenseKey: aliceKey, Identifier: "alice-ws"}); code(err) != "LICENSE_NOT_FOUND" {
		t.Fatalf("checkout with a removed member's key = %v, want LICENSE_NOT_FOUND", err)
	}
}
