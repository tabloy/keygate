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

func TestRevokeLicense_DeletesActivationsAndFloatingSessions(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()

	lic := newLicenseOn(t, s, ctx, "subscription", model.StatusActive, nil)

	act := &model.Activation{
		LicenseID:      lic.ID,
		Identifier:     "machine-1",
		IdentifierType: "device",
	}
	if err := s.CreateActivation(ctx, act); err != nil {
		t.Fatalf("create activation: %v", err)
	}

	sess := &model.FloatingSession{
		LicenseID:  lic.ID,
		Identifier: "machine-1",
		ExpiresAt:  time.Now().Add(time.Hour),
	}
	if err := s.CheckOutFloating(ctx, sess); err != nil {
		t.Fatalf("checkout floating: %v", err)
	}

	// Verify activation and session exist before revoking
	actCount, err := s.CountActivations(ctx, lic.ID)
	if err != nil || actCount != 1 {
		t.Fatalf("expected 1 activation, got %d (err: %v)", actCount, err)
	}
	sessCount, err := s.DB.NewSelect().Model((*model.FloatingSession)(nil)).Where("license_id = ?", lic.ID).Count(ctx)
	if err != nil || sessCount != 1 {
		t.Fatalf("expected 1 floating session, got %d (err: %v)", sessCount, err)
	}

	if err := s.RevokeLicense(ctx, lic.ID); err != nil {
		t.Fatalf("RevokeLicense: %v", err)
	}

	if st, _ := licenseState(t, s, ctx, lic.ID); st != model.StatusRevoked {
		t.Errorf("license status = %s, want %s", st, model.StatusRevoked)
	}
	if st, _ := subscriptionState(t, s, ctx, lic.ID); st != model.StatusRevoked {
		t.Errorf("subscription status = %s, want %s", st, model.StatusRevoked)
	}

	actCount, err = s.CountActivations(ctx, lic.ID)
	if err != nil || actCount != 0 {
		t.Errorf("expected 0 activations after revoke, got %d (err: %v)", actCount, err)
	}
	sessCount, err = s.DB.NewSelect().Model((*model.FloatingSession)(nil)).Where("license_id = ?", lic.ID).Count(ctx)
	if err != nil || sessCount != 0 {
		t.Errorf("expected 0 floating sessions after revoke, got %d (err: %v)", sessCount, err)
	}
}

func TestRevokeLicense_NotFound(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()

	err := s.RevokeLicense(ctx, "nonexistent-id")
	if !errors.Is(err, store.ErrLicenseNotFound) {
		t.Fatalf("err = %v, want ErrLicenseNotFound", err)
	}
}

func TestSuspendLicense_SyncsSubscription(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()

	lic := newLicenseOn(t, s, ctx, "subscription", model.StatusActive, nil)

	if err := s.SuspendLicense(ctx, lic.ID); err != nil {
		t.Fatalf("SuspendLicense: %v", err)
	}

	if st, _ := licenseState(t, s, ctx, lic.ID); st != model.StatusSuspended {
		t.Errorf("license status = %s, want %s", st, model.StatusSuspended)
	}
	if st, _ := subscriptionState(t, s, ctx, lic.ID); st != model.StatusSuspended {
		t.Errorf("subscription status = %s, want %s", st, model.StatusSuspended)
	}

	// Re-suspending an already suspended license should return ErrLicenseNotSuspendable
	err := s.SuspendLicense(ctx, lic.ID)
	if !errors.Is(err, store.ErrLicenseNotSuspendable) {
		t.Errorf("err = %v, want ErrLicenseNotSuspendable", err)
	}
}

func TestSuspendLicense_DeletesFloatingSessions(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()

	lic := newLicenseOn(t, s, ctx, "subscription", model.StatusActive, nil)
	sess := &model.FloatingSession{
		LicenseID:  lic.ID,
		Identifier: "machine-1",
		ExpiresAt:  time.Now().Add(time.Hour),
	}
	if err := s.CheckOutFloating(ctx, sess); err != nil {
		t.Fatalf("checkout floating: %v", err)
	}

	if err := s.SuspendLicense(ctx, lic.ID); err != nil {
		t.Fatalf("SuspendLicense: %v", err)
	}

	count, err := s.DB.NewSelect().Model((*model.FloatingSession)(nil)).Where("license_id = ?", lic.ID).Count(ctx)
	if err != nil || count != 0 {
		t.Errorf("floating sessions after suspend = %d (err: %v), want 0", count, err)
	}
}

func TestExpireLicense_DeletesFloatingSessions(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()

	lic := newLicenseOn(t, s, ctx, "subscription", model.StatusPastDue, nil)
	sess := &model.FloatingSession{
		LicenseID:  lic.ID,
		Identifier: "machine-1",
		ExpiresAt:  time.Now().Add(time.Hour),
	}
	if err := s.CheckOutFloating(ctx, sess); err != nil {
		t.Fatalf("checkout floating: %v", err)
	}

	ok, err := s.ExpireLicenseIf(ctx, lic.ID, []string{model.StatusPastDue}, nil)
	if err != nil || !ok {
		t.Fatalf("ExpireLicenseIf = (%t, %v), want (true, nil)", ok, err)
	}
	if st, _ := licenseState(t, s, ctx, lic.ID); st != model.StatusExpired {
		t.Errorf("license status = %s, want %s", st, model.StatusExpired)
	}

	count, err := s.DB.NewSelect().Model((*model.FloatingSession)(nil)).Where("license_id = ?", lic.ID).Count(ctx)
	if err != nil || count != 0 {
		t.Errorf("floating sessions after expire = %d (err: %v), want 0", count, err)
	}
}

func TestSuspendLicense_NotFound(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()

	err := s.SuspendLicense(ctx, "nonexistent-id")
	if !errors.Is(err, store.ErrLicenseNotFound) {
		t.Fatalf("err = %v, want ErrLicenseNotFound", err)
	}
}

// A failed subscription update undoes the whole revoke: the licence is
// not left revoked with its devices gone and its subscription active.
func TestRevokeLicense_FailedSubscriptionSyncRollsBack(t *testing.T) {
	s := setupTestDB(t)
	t.Cleanup(func() { s.Close() })
	ctx := context.Background()
	lic := newLicenseOn(t, s, ctx, "subscription", model.StatusActive, nil)
	if err := s.CreateActivation(ctx, &model.Activation{LicenseID: lic.ID, Identifier: "machine-1", IdentifierType: "device"}); err != nil {
		t.Fatal(err)
	}

	fn := "fail_revoke_" + strings.ReplaceAll(lic.ID, "-", "")
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

	err := s.RevokeLicense(ctx, lic.ID)
	if !strings.Contains(fmt.Sprint(err), "database unavailable") {
		t.Fatalf("err = %v, want the trigger's error", err)
	}
	if st, _ := licenseState(t, s, ctx, lic.ID); st != model.StatusActive {
		t.Errorf("license status = %s, want it still active", st)
	}
	if st, _ := subscriptionState(t, s, ctx, lic.ID); st != model.StatusActive {
		t.Errorf("subscription status = %s, want it still active", st)
	}
	if n, err := s.CountActivations(ctx, lic.ID); err != nil || n != 1 {
		t.Errorf("activations = %d (err %v), want the device kept", n, err)
	}
}
