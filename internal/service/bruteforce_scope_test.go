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

	"github.com/tabloy/keygate/internal/middleware"
	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
	"github.com/tabloy/keygate/pkg/apperr"
)

// bruteForceFixture is a store, a real failure tracker with the default
// limits (5 failures in 5 minutes), and one license of each model.
type bruteForceFixture struct {
	s           *store.Store
	bf          *middleware.BruteForceProtection
	standardKey string
	floatingKey string
}

func newBruteForceFixture(t *testing.T) *bruteForceFixture {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("skipping integration test: TEST_DATABASE_URL not set")
	}
	s, err := store.New(dsn)
	if err != nil {
		t.Skipf("skipping integration test: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.RunMigrations("../../db/migrations"); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	ctx := context.Background()
	suffix := time.Now().Format("150405.000000")
	prod := &model.Product{Name: "Brute force", Slug: "bf-" + suffix, Type: "desktop"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	f := &bruteForceFixture{
		s:  s,
		bf: middleware.NewBruteForceProtection(5, 30*time.Second, 30*time.Minute, 5*time.Minute),
	}
	for _, model_ := range []string{"standard", "floating"} {
		plan := &model.Plan{ProductID: prod.ID, Name: model_, Slug: "bf-" + model_ + "-" + suffix, LicenseType: "perpetual",
			LicenseModel: model_, MaxActivations: 3, FloatingTimeout: 30, Active: true}
		if err := s.CreatePlan(ctx, plan); err != nil {
			t.Fatal(err)
		}
		key := "KG-BF-" + model_ + "-" + suffix
		lic := &model.License{ProductID: prod.ID, PlanID: plan.ID, Email: "bf-" + suffix + "@example.com", LicenseKey: key, Status: model.StatusActive}
		if err := s.CreateLicense(ctx, lic); err != nil {
			t.Fatal(err)
		}
		if model_ == "standard" {
			f.standardKey = key
		} else {
			f.floatingKey = key
		}
	}
	return f
}

func isLockedOut(err error) bool {
	var ae *apperr.AppError
	return errors.As(err, &ae) && ae.Code == "LOCKED_OUT"
}

// A valid key verified from the same address must not wipe the failures
// that address has piled up: anyone holding one licence could otherwise
// guess four keys, verify their own, and guess four more, forever.
func TestValidKeyCannotResetAddressFailures(t *testing.T) {
	f := newBruteForceFixture(t)
	ctx := context.Background()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	ls := NewLicenseService(f.s, priv, slog.Default(), f.bf, nil)
	const ip = "203.0.113.7"
	if _, err := ls.Activate(ctx, ActivateInput{LicenseKey: f.standardKey, Identifier: "own-machine", IPAddress: ip}); err != nil {
		t.Fatalf("activate own key: %v", err)
	}

	guesses := 0
	for range 3 {
		for range 4 {
			guesses++
			_, err := ls.Verify(ctx, VerifyInput{LicenseKey: "KG-GUESS-" + time.Now().Format("150405.000000"), Identifier: "x", IPAddress: ip})
			if isLockedOut(err) {
				if guesses != 5 {
					t.Fatalf("locked out after %d guesses, want 5", guesses)
				}
				return
			}
		}
		if _, err := ls.Verify(ctx, VerifyInput{LicenseKey: f.standardKey, Identifier: "own-machine", IPAddress: ip}); err != nil {
			if isLockedOut(err) && guesses >= 5 {
				return
			}
			t.Fatalf("verify own key: %v", err)
		}
	}
	t.Fatalf("%d guesses interleaved with valid verifies were never locked out", guesses)
}

// Checkout is the way into a floating licence, which activate and
// verify refuse, so its 404s count like theirs: unknown keys and keys
// that are not floating alike. The lockout is the caller's address
// only.
func TestFloatingCheckoutCountsFailures(t *testing.T) {
	f := newBruteForceFixture(t)
	ctx := context.Background()
	fs := NewFloatingService(f.s, slog.Default(), f.bf)
	const ip = "203.0.113.8"
	checkout := func(key, from string) error {
		_, err := fs.CheckOut(ctx, CheckOutInput{LicenseKey: key, Identifier: "ws-1", IPAddress: from})
		return err
	}

	if err := checkout(f.floatingKey, ip); err != nil {
		t.Fatalf("checkout with the real key: %v", err)
	}
	attempts := []string{"KG-NOPE-1", "KG-NOPE-2", f.standardKey, "KG-NOPE-3"}
	for _, key := range attempts {
		err := checkout(key, ip)
		var ae *apperr.AppError
		if !errors.As(err, &ae) || ae.Code != "LICENSE_NOT_FOUND" {
			t.Fatalf("checkout %q = %v, want LICENSE_NOT_FOUND", key, err)
		}
	}
	if err := checkout("KG-NOPE-4", ip); !isLockedOut(err) {
		t.Fatalf("fifth failed checkout = %v, want LOCKED_OUT", err)
	}
	if blocked, _ := f.bf.IsBlocked(middleware.FailureKeyIP(ip)); !blocked {
		t.Fatal("address not blocked after five failed checkouts")
	}
	if err := checkout(f.floatingKey, "203.0.113.9"); err != nil {
		t.Fatalf("another address with the real key: %v", err)
	}
}
