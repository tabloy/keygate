package store_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
)

// A code allows MaxOTPAttempts guesses however many arrive at once.
// The count used to be read first and raised after the compare, so a
// burst of parallel guesses all saw it under the limit and were all
// compared.
func TestClaimOTPAttemptHoldsTheLimitUnderConcurrency(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()

	email := "attempts-" + time.Now().Format("150405.000000") + "@test.com"
	otp := &model.OTPCode{Email: email, CodeHash: "x", ExpiresAt: time.Now().Add(10 * time.Minute)}
	if err := s.CreateOTPCode(ctx, otp); err != nil {
		t.Fatalf("create code: %v", err)
	}
	defer func() {
		_, _ = s.DB.NewRaw("DELETE FROM otp_codes WHERE email = ?", email).Exec(ctx)
	}()

	const guesses = 20
	var claimed sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	start := make(chan struct{})
	for range guesses {
		claimed.Add(1)
		go func() {
			defer claimed.Done()
			<-start
			_, got, err := s.ClaimOTPAttempt(ctx, otp.ID)
			if err != nil {
				t.Errorf("claim: %v", err)
				return
			}
			if got {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}()
	}
	close(start)
	claimed.Wait()
	if ok != store.MaxOTPAttempts {
		t.Fatalf("%d of %d guesses got an attempt, want %d", ok, guesses, store.MaxOTPAttempts)
	}
	if _, err := s.FindLatestValidOTPCode(ctx, email); err == nil {
		t.Fatal("a code with its attempts used up is still offered")
	}
}

// A used or expired code gives no attempts.
func TestClaimOTPAttemptRefusesUsedAndExpiredCodes(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()

	email := "attempts-dead-" + time.Now().Format("150405.000000") + "@test.com"
	defer func() {
		_, _ = s.DB.NewRaw("DELETE FROM otp_codes WHERE email = ?", email).Exec(ctx)
	}()
	used := &model.OTPCode{Email: email, CodeHash: "x", ExpiresAt: time.Now().Add(10 * time.Minute)}
	expired := &model.OTPCode{Email: email, CodeHash: "y", ExpiresAt: time.Now().Add(-time.Minute)}
	for _, otp := range []*model.OTPCode{used, expired} {
		if err := s.CreateOTPCode(ctx, otp); err != nil {
			t.Fatalf("create code: %v", err)
		}
	}
	if consumed, err := s.ConsumeOTPCode(ctx, used.ID); err != nil || !consumed {
		t.Fatalf("consume: %v %v", consumed, err)
	}
	for name, id := range map[string]string{"used": used.ID, "expired": expired.ID} {
		if _, got, err := s.ClaimOTPAttempt(ctx, id); err != nil || got {
			t.Fatalf("%s code: got an attempt (%v, %v)", name, got, err)
		}
	}
}
