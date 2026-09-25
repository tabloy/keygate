package store_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
)

func activationLicense(t *testing.T, s *store.Store, ctx context.Context, max int) *model.License {
	t.Helper()
	suffix := time.Now().Format("150405.000000")
	product := &model.Product{Name: "Act Test", Slug: "act-test-" + suffix, Type: "desktop"}
	if err := s.CreateProduct(ctx, product); err != nil {
		t.Fatalf("create product: %v", err)
	}
	plan := &model.Plan{
		ProductID: product.ID, Name: "Act", Slug: "act-plan-" + suffix,
		LicenseType: "perpetual", LicenseModel: "standard", MaxActivations: max,
	}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatalf("create plan: %v", err)
	}
	lic := &model.License{
		ProductID: product.ID, PlanID: plan.ID, Email: "act-" + suffix + "@example.com",
		LicenseKey: "ACTKEY-" + suffix, Status: "active",
	}
	if err := s.CreateLicense(ctx, lic); err != nil {
		t.Fatalf("create license: %v", err)
	}
	return lic
}

func activate(s *store.Store, ctx context.Context, licenseID, identifier string, max int) error {
	return s.ActivateWithinLimit(ctx, &model.Activation{
		LicenseID: licenseID, Identifier: identifier, IdentifierType: "device",
	}, max)
}

// One machine asking twice at once is a repeat, not a contest.
//
// The service looks for an existing activation before calling, but
// that read is not in this transaction, so two requests from the same
// machine can both miss it. At the cap the second used to count the
// slot the first had just taken and report the licence full; below the
// cap it used to hit the unique index and surface as a 500. Both are
// what a client retrying a timed-out request actually does.
func TestConcurrentActivationFromOneMachine(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()

	for _, max := range []int{1, 5} {
		t.Run(fmt.Sprintf("max=%d", max), func(t *testing.T) {
			lic := activationLicense(t, s, ctx, max)

			const rounds, attempts = 15, 8
			for round := range rounds {
				start := make(chan struct{})
				errs := make(chan error, attempts)
				var wg sync.WaitGroup
				for range attempts {
					wg.Add(1)
					go func() {
						defer wg.Done()
						<-start
						errs <- activate(s, ctx, lic.ID, "one-machine", max)
					}()
				}
				close(start)
				wg.Wait()
				close(errs)

				for err := range errs {
					// Either it took the slot, or it was told the slot
					// is already this machine's. Nothing else is a
					// correct answer.
					if err != nil && !errors.Is(err, store.ErrAlreadyActivated) {
						t.Fatalf("round %d: concurrent activation from one machine failed: %v", round, err)
					}
				}
			}

			n, err := s.CountActivations(ctx, lic.ID)
			if err != nil {
				t.Fatalf("count: %v", err)
			}
			if n != 1 {
				t.Errorf("one machine produced %d activations, want 1", n)
			}
		})
	}
}

// Different machines still compete, and the cap still decides.
func TestConcurrentActivationRespectsTheLimit(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()

	const max, contenders = 3, 12
	lic := activationLicense(t, s, ctx, max)

	granted := make(chan bool, contenders)
	var wg sync.WaitGroup
	for i := range contenders {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			granted <- activate(s, ctx, lic.ID, fmt.Sprintf("machine-%02d", n), max) == nil
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
		t.Errorf("%d of %d machines were activated, want exactly %d", ok, contenders, max)
	}
	n, err := s.CountActivations(ctx, lic.ID)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != max {
		t.Errorf("activations = %d, want %d", n, max)
	}
}

// A repeat from a machine that already holds a slot reports itself as
// such and hands back the row that exists, so the caller can touch it
// rather than create a second one.
func TestRepeatActivationReturnsExistingRow(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()
	lic := activationLicense(t, s, ctx, 2)

	first := &model.Activation{LicenseID: lic.ID, Identifier: "laptop", IdentifierType: "device"}
	if err := s.ActivateWithinLimit(ctx, first, 2); err != nil {
		t.Fatalf("first activation: %v", err)
	}
	again := &model.Activation{LicenseID: lic.ID, Identifier: "laptop", IdentifierType: "device"}
	err := s.ActivateWithinLimit(ctx, again, 2)
	if !errors.Is(err, store.ErrAlreadyActivated) {
		t.Fatalf("second activation returned %v, want ErrAlreadyActivated", err)
	}
	if again.ID != first.ID {
		t.Errorf("the repeat reported id %s, want the existing %s", again.ID, first.ID)
	}
}
