package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
)

// rowsFor counts the rows every table with a license_id column holds for
// the licence, so a table added later is covered without editing this.
func rowsFor(t *testing.T, s *store.Store, ctx context.Context, licenseID string) map[string]int {
	t.Helper()
	var tables []string
	if err := s.DB.NewRaw(`SELECT table_name FROM information_schema.columns
		WHERE table_schema = 'public' AND column_name = 'license_id' AND table_name <> 'audit_logs'`).Scan(ctx, &tables); err != nil {
		t.Fatal(err)
	}
	out := map[string]int{}
	for _, tbl := range tables {
		var n int
		if err := s.DB.NewRaw("SELECT count(*) FROM "+tbl+" WHERE license_id = ?", licenseID).Scan(ctx, &n); err != nil {
			t.Fatalf("%s: %v", tbl, err)
		}
		if n > 0 {
			out[tbl] = n
		}
	}
	return out
}

func revokedLicense(t *testing.T, s *store.Store, ctx context.Context) *model.License {
	t.Helper()
	lic := createTestLicense(t, s, ctx)
	if err := s.RevokeLicense(ctx, lic.ID); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond) // createTestLicense names by time
	return lic
}

func exec(t *testing.T, s *store.Store, ctx context.Context, q string, args ...any) {
	t.Helper()
	if _, err := s.DB.NewRaw(q, args...).Exec(ctx); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

// A revoked licence is deleted with everything recorded under it, and
// deleting it again finds nothing.
func TestDeleteLicenseRemovesEverything(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()
	lic := revokedLicense(t, s, ctx)
	exec(t, s, ctx, "INSERT INTO activations (id, license_id, identifier, identifier_type) VALUES (gen_random_uuid()::text, ?, 'dev-1', 'device')", lic.ID)
	exec(t, s, ctx, "INSERT INTO notifications (id, license_id, tag) VALUES (gen_random_uuid()::text, ?, 'test-delete')", lic.ID)
	exec(t, s, ctx, "INSERT INTO seats (id, license_id, email) VALUES (gen_random_uuid()::text, ?, 'member@example.com')", lic.ID)
	exec(t, s, ctx, "INSERT INTO usage_counters (id, license_id, feature, period, period_key) VALUES (gen_random_uuid()::text, ?, 'api_calls', 'monthly', '2026-11')", lic.ID)
	before := rowsFor(t, s, ctx, lic.ID)
	for _, tbl := range []string{"activations", "notifications", "seats", "usage_counters"} {
		if before[tbl] != 1 {
			t.Fatalf("setup: rows %v", before)
		}
	}

	gone, err := s.DeleteLicense(ctx, lic.ID)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if gone.ID != lic.ID || gone.Email != lic.Email || gone.ProductID != lic.ProductID || gone.Status != model.StatusRevoked {
		t.Errorf("returned licence %+v, want the one deleted", gone)
	}
	if _, err := s.FindLicenseByID(ctx, lic.ID); err == nil {
		t.Fatal("the licence is still there")
	}
	if _, err := s.FindLicenseByKey(ctx, lic.LicenseKey); err == nil {
		t.Fatal("the key still finds a licence")
	}
	if left := rowsFor(t, s, ctx, lic.ID); len(left) > 0 {
		t.Fatalf("rows left behind: %v", left)
	}
	if _, err := s.DeleteLicense(ctx, lic.ID); !errors.Is(err, store.ErrLicenseNotFound) {
		t.Fatalf("second delete = %v, want ErrLicenseNotFound", err)
	}
}

// Only a revoked or expired licence can be deleted; one still in use is
// refused and kept.
func TestDeleteLicenseOnlyWhenRevokedOrExpired(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()
	for _, status := range []string{model.StatusActive, "trialing", "suspended", "past_due"} {
		lic := createTestLicense(t, s, ctx)
		exec(t, s, ctx, "UPDATE licenses SET status = ? WHERE id = ?", status, lic.ID)
		if _, err := s.DeleteLicense(ctx, lic.ID); !errors.Is(err, store.ErrLicenseNotRevoked) {
			t.Errorf("%s: delete = %v, want ErrLicenseNotRevoked", status, err)
		}
		if _, err := s.FindLicenseByID(ctx, lic.ID); err != nil {
			t.Errorf("%s: the licence was deleted anyway: %v", status, err)
		}
		time.Sleep(2 * time.Millisecond)
	}
	expired := createTestLicense(t, s, ctx)
	exec(t, s, ctx, "UPDATE licenses SET status = ? WHERE id = ?", model.StatusExpired, expired.ID)
	if _, err := s.DeleteLicense(ctx, expired.ID); err != nil {
		t.Errorf("an expired licence: delete = %v, want it deleted", err)
	}
}

// A revoked licence whose money can still move is refused and kept: a
// payment provider or Stripe link, an update period (a renewal checkout
// leaves no local row until it is paid), an upgrade or a renewal row.
func TestDeleteLicenseRefusesLicensesWithMoneyOnRecord(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()
	for _, tc := range []struct{ name, q string }{
		{"payment provider", "UPDATE licenses SET payment_provider = 'paypal' WHERE id = ?"},
		{"stripe customer", "UPDATE licenses SET stripe_customer_id = 'cus_test_delete_' || id WHERE id = ?"},
		{"stripe subscription", "UPDATE licenses SET stripe_subscription_id = 'sub_test_delete_' || id WHERE id = ?"},
		{"payment intent", "UPDATE licenses SET stripe_payment_intent_id = 'pi_test_delete_' || id WHERE id = ?"},
		{"checkout session", "UPDATE licenses SET stripe_checkout_session_id = 'cs_test_delete_' || id WHERE id = ?"},
		{"update period", "UPDATE licenses SET updates_until = now() + interval '30 days' WHERE id = ?"},
		{"upgrade checkout", "INSERT INTO license_upgrades (id, license_id, stripe_checkout_session_id) VALUES (gen_random_uuid()::text, ?, 'cs_up_' || gen_random_uuid()::text)"},
		{"renewal", "INSERT INTO license_renewals (id, license_id, days) VALUES (gen_random_uuid()::text, ?, 365)"},
	} {
		lic := revokedLicense(t, s, ctx)
		exec(t, s, ctx, tc.q, lic.ID)
		if _, err := s.DeleteLicense(ctx, lic.ID); !errors.Is(err, store.ErrLicenseNotDeletable) {
			t.Errorf("%s: delete = %v, want ErrLicenseNotDeletable", tc.name, err)
		}
		if _, err := s.FindLicenseByID(ctx, lic.ID); err != nil {
			t.Errorf("%s: the licence was deleted anyway: %v", tc.name, err)
		}
	}
	if _, err := s.DeleteLicense(ctx, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, store.ErrLicenseNotFound) {
		t.Errorf("unknown id = %v, want ErrLicenseNotFound", err)
	}
}

// A checkout linking a subscription while the delete runs: the delete
// waits for the licence's row lock and then sees the link, so the
// licence is never deleted out from under a subscription.
func TestDeleteLicenseWaitsForAStripeLink(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()
	lic := revokedLicense(t, s, ctx)

	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := store.LockLicenseIn(ctx, tx, lic.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.NewRaw("UPDATE licenses SET stripe_subscription_id = ? WHERE id = ?", "sub_race_"+lic.ID, lic.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := s.DeleteLicense(ctx, lic.ID)
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("delete did not wait for the checkout's lock: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, store.ErrLicenseNotDeletable) {
			t.Fatalf("delete after the link = %v, want ErrLicenseNotDeletable", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("delete still waiting after the checkout committed")
	}
	if _, err := s.FindLicenseByID(ctx, lic.ID); err != nil {
		t.Fatalf("the licence was deleted: %v", err)
	}
}
