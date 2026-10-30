package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
)

// A fallback deleted between the check and the write reads as such, and
// only that foreign key does: a missing product is something else.
func TestIsTrialFallbackMissing(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()
	prod, _, _ := perSeatLicense(t, s, ctx, 1, false)
	suffix := strings.ReplaceAll(time.Now().Format("150405.000000"), ".", "")
	missing := "00000000-0000-0000-0000-000000000000"
	trial := &model.Plan{ProductID: prod.ID, Name: "T", Slug: "t-" + suffix, LicenseType: "trial", LicenseModel: "standard",
		TrialDays: 7, Active: true, TrialFallbackPlanID: missing}
	if err := s.CreatePlan(ctx, trial); !store.IsTrialFallbackMissing(err) {
		t.Fatalf("a missing fallback = %v, want IsTrialFallbackMissing", err)
	}
	orphan := &model.Plan{ProductID: missing, Name: "O", Slug: "o-" + suffix, LicenseType: "perpetual", LicenseModel: "standard", Active: true}
	err := s.CreatePlan(ctx, orphan)
	if err == nil || store.IsTrialFallbackMissing(err) {
		t.Fatalf("a missing product = %v, want a different error", err)
	}
}

// Naming a fallback and editing or deleting that plan serialize on its
// row: whichever goes second sees the first, so a trial plan never ends
// up falling back to a plan that cannot take its trials.
func TestTrialFallbackLocks_Serialize(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()
	prod, _, _ := perSeatLicense(t, s, ctx, 1, false)
	suffix := strings.ReplaceAll(time.Now().Format("150405.000000"), ".", "")
	plan := func(name, licenseType string) *model.Plan {
		p := &model.Plan{ProductID: prod.ID, Name: name, Slug: name + "-" + suffix, LicenseType: licenseType,
			LicenseModel: "standard", TrialDays: 7, Active: true}
		if err := s.CreatePlan(ctx, p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	free, trial := plan("free", "perpetual"), plan("trial", "trial")
	waited := func(done <-chan error) {
		t.Helper()
		select {
		case err := <-done:
			t.Fatalf("did not wait for the other write: %v", err)
		case <-time.After(300 * time.Millisecond):
		}
	}

	// The fallback is named first: the delete waits, then is refused.
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.LockTrialFallbackIn(ctx, tx, free.ID, prod.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.NewRaw("UPDATE plans SET trial_fallback_plan_id = ? WHERE id = ?", free.ID, trial.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.DeletePlanUnlessFallback(ctx, free.ID) }()
	waited(done)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var inUse *store.TrialFallbackInUseError
	if err := <-done; !errors.As(err, &inUse) {
		t.Fatalf("delete after the fallback was named = %v, want TrialFallbackInUseError", err)
	}

	// The plan is priced first: naming it waits, then is refused.
	other := plan("other", "perpetual")
	tx, err = s.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.LockPlanIn(ctx, tx, other.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.NewRaw("UPDATE plans SET stripe_price_id = ? WHERE id = ?", "price_lock_"+suffix, other.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	go func() {
		done <- store.RunInTx(ctx, s.DB, func(ctx context.Context, tx bun.Tx) error {
			return store.LockTrialFallbackIn(ctx, tx, other.ID, prod.ID)
		})
	}()
	waited(done)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var refused *store.TrialFallbackRefusedError
	if err := <-done; !errors.As(err, &refused) {
		t.Fatalf("naming a plan priced meanwhile = %v, want TrialFallbackRefusedError", err)
	}
}

func TestMoveEndedTrial_ExpiryDeletesFloatingSessions(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()
	prod, _, _ := perSeatLicense(t, s, ctx, 1, false)
	suffix := strings.ReplaceAll(time.Now().Format("150405.000000"), ".", "")
	trial := &model.Plan{ProductID: prod.ID, Name: "Trial", Slug: "mve-trial-" + suffix,
		LicenseType: "trial", LicenseModel: "standard", TrialDays: 7, Active: true}
	if err := s.CreatePlan(ctx, trial); err != nil {
		t.Fatal(err)
	}
	ended := time.Now().Add(-time.Hour)
	lic := &model.License{ProductID: prod.ID, PlanID: trial.ID, Email: "mve-" + suffix + "@example.com",
		LicenseKey: "KEY-MVE-" + suffix, Status: model.StatusTrialing, ValidUntil: &ended}
	if err := s.CreateLicense(ctx, lic); err != nil {
		t.Fatal(err)
	}
	sess := &model.FloatingSession{
		LicenseID:  lic.ID,
		Identifier: "machine-1",
		ExpiresAt:  time.Now().Add(time.Hour),
	}
	if err := s.CheckOutFloating(ctx, sess); err != nil {
		t.Fatalf("checkout floating: %v", err)
	}

	move, err := s.MoveEndedTrialToFallback(ctx, lic.ID, time.Now())
	if err != nil || !move.Expire || move.Moved {
		t.Fatalf("MoveEndedTrialToFallback = (%+v, %v), want expiry", move, err)
	}
	count, err := s.DB.NewSelect().Model((*model.FloatingSession)(nil)).Where("license_id = ?", lic.ID).Count(ctx)
	if err != nil || count != 0 {
		t.Errorf("floating sessions after trial expiry = %d (err: %v), want 0", count, err)
	}
}

// The sweep holds the fallback plan while it moves a trial onto it: a
// price written meanwhile (by an older version, or by hand) either waits
// for the move or is seen by it, and then the trial expires instead.
func TestMoveEndedTrial_WaitsForAnEditOfTheFallback(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()
	prod, _, _ := perSeatLicense(t, s, ctx, 1, false)
	suffix := strings.ReplaceAll(time.Now().Format("150405.000000"), ".", "")
	free := &model.Plan{ProductID: prod.ID, Name: "Free", Slug: "mvf-free-" + suffix, LicenseType: "perpetual", LicenseModel: "standard", Active: true}
	if err := s.CreatePlan(ctx, free); err != nil {
		t.Fatal(err)
	}
	trial := &model.Plan{ProductID: prod.ID, Name: "Trial", Slug: "mvf-trial-" + suffix, LicenseType: "trial", LicenseModel: "standard",
		TrialDays: 7, Active: true, TrialFallbackPlanID: free.ID}
	if err := s.CreatePlan(ctx, trial); err != nil {
		t.Fatal(err)
	}
	ended := time.Now().Add(-time.Hour)
	lic := &model.License{ProductID: prod.ID, PlanID: trial.ID, Email: "mvf-" + suffix + "@example.com",
		LicenseKey: "KEY-MVF-" + suffix, Status: model.StatusTrialing, ValidUntil: &ended}
	if err := s.CreateLicense(ctx, lic); err != nil {
		t.Fatal(err)
	}

	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.LockPlanIn(ctx, tx, free.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.NewRaw("UPDATE plans SET stripe_price_id = ? WHERE id = ?", "price_mvf_"+suffix, free.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	type result struct {
		move store.TrialMove
		err  error
	}
	done := make(chan result, 1)
	go func() {
		m, err := s.MoveEndedTrialToFallback(ctx, lic.ID, time.Now())
		done <- result{m, err}
	}()
	select {
	case r := <-done:
		t.Fatalf("the move did not wait for the edit of its fallback: %+v", r)
	case <-time.After(300 * time.Millisecond):
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	r := <-done
	if r.err != nil || r.move.Moved || !r.move.Expire || r.move.Refusal == "" {
		t.Fatalf("move after the fallback was priced: %+v, %v; want expire with a refusal", r.move, r.err)
	}
}

// A switch of the trial plan's fallback serializes with the move: the
// move waits for it and lands on the fallback the plan names now.
func TestMoveEndedTrial_WaitsForASwitchOfTheFallback(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()
	prod, _, _ := perSeatLicense(t, s, ctx, 1, false)
	suffix := strings.ReplaceAll(time.Now().Format("150405.000000"), ".", "")
	plan := func(name, licenseType, fallback string) *model.Plan {
		p := &model.Plan{ProductID: prod.ID, Name: name, Slug: "mvs-" + name + "-" + suffix, LicenseType: licenseType,
			LicenseModel: "standard", TrialDays: 7, Active: true, TrialFallbackPlanID: fallback}
		if err := s.CreatePlan(ctx, p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	old, next := plan("old", "perpetual", ""), plan("next", "perpetual", "")
	trial := plan("trial", "trial", old.ID)
	ended := time.Now().Add(-time.Hour)
	lic := &model.License{ProductID: prod.ID, PlanID: trial.ID, Email: "mvs-" + suffix + "@example.com",
		LicenseKey: "KEY-MVS-" + suffix, Status: model.StatusTrialing, ValidUntil: &ended}
	if err := s.CreateLicense(ctx, lic); err != nil {
		t.Fatal(err)
	}

	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.LockPlanIn(ctx, tx, trial.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.NewRaw("UPDATE plans SET trial_fallback_plan_id = ? WHERE id = ?", next.ID, trial.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	type result struct {
		move store.TrialMove
		err  error
	}
	done := make(chan result, 1)
	go func() {
		m, err := s.MoveEndedTrialToFallback(ctx, lic.ID, time.Now())
		done <- result{m, err}
	}()
	select {
	case r := <-done:
		t.Fatalf("the move did not wait for the switch of its fallback: %+v", r)
	case <-time.After(300 * time.Millisecond):
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	r := <-done
	if r.err != nil || !r.move.Moved || r.move.To == nil || r.move.To.ID != next.ID {
		t.Fatalf("move after the switch: %+v, %v; want it moved to the new fallback", r.move, r.err)
	}
}
