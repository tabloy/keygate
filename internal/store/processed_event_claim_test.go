package store

import (
	"context"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestClaimProcessedEvent separates the three outcomes callers must
// tell apart: first claim, already claimed, and database failure.
// Fulfilment treats "already claimed" as done and a failure as
// "retry later"; folding the two together would drop a paid session.
func TestClaimProcessedEvent(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("skipping integration test: TEST_DATABASE_URL not set")
	}
	s, err := New(dsn)
	if err != nil {
		t.Skipf("skipping integration test: %v", err)
	}
	if err := s.RunMigrations("../../db/migrations"); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	ctx := context.Background()
	id := "evt_claim_" + time.Now().Format("150405.000")
	defer func() { _ = s.DeleteProcessedEvent(ctx, "test_claim", id) }()

	claimed, err := s.ClaimProcessedEvent(ctx, "test_claim", id)
	if err != nil || !claimed {
		t.Fatalf("first claim: claimed=%v err=%v", claimed, err)
	}
	claimed, err = s.ClaimProcessedEvent(ctx, "test_claim", id)
	if err != nil || claimed {
		t.Fatalf("second claim: claimed=%v err=%v, want false,nil", claimed, err)
	}

	s.Close()
	claimed, err = s.ClaimProcessedEvent(ctx, "test_claim", id+"-x")
	if err == nil || claimed {
		t.Fatalf("closed store: claimed=%v err=%v, want false,<error>", claimed, err)
	}
}

// TestSetSettings_Atomic: a batch either lands completely or not at
// all — an oversized value that violates the column check must not
// leave the other keys of the same batch written.
func TestSetSettings_Atomic(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("skipping integration test: TEST_DATABASE_URL not set")
	}
	s, err := New(dsn)
	if err != nil {
		t.Skipf("skipping integration test: %v", err)
	}
	defer s.Close()
	if err := s.RunMigrations("../../db/migrations"); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	ctx := context.Background()
	tag := time.Now().Format("150405.000")
	k1, k2 := "test_atomic_a_"+tag, "test_atomic_b_"+tag
	defer s.DeleteSetting(ctx, k1)
	defer s.DeleteSetting(ctx, k2)
	if err := s.SetSettings(ctx, map[string]string{k1: "one", k2: "two"}); err != nil {
		t.Fatalf("set: %v", err)
	}
	// Force a failure on one key via a cancelled context mid-batch is
	// not deterministic; use a key longer than the column allows.
	long := strings.Repeat("k", 10000)
	err = s.SetSettings(ctx, map[string]string{k1: "changed", long: "x"})
	if err == nil {
		// No length limit on this schema: nothing to prove here.
		_ = s.DeleteSetting(ctx, long)
		t.Skip("settings.key has no length constraint; atomicity not observable this way")
	}
	if v, _ := s.GetSetting(ctx, k1); v != "one" {
		t.Fatalf("failed batch leaked a write: %s=%q", k1, v)
	}
}

// TestWithAdvisoryLock_Serialises: two callers cannot be inside the
// locked section at the same time, across separate connections.
func TestWithAdvisoryLock_Serialises(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("skipping integration test: TEST_DATABASE_URL not set")
	}
	s, err := New(dsn)
	if err != nil {
		t.Skipf("skipping integration test: %v", err)
	}
	defer s.Close()
	ctx := context.Background()
	var inside, maxInside int32
	var wg sync.WaitGroup
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = s.WithAdvisoryLock(ctx, 424242, func(ctx context.Context) error {
				n := atomic.AddInt32(&inside, 1)
				for {
					m := atomic.LoadInt32(&maxInside)
					if n <= m || atomic.CompareAndSwapInt32(&maxInside, m, n) {
						break
					}
				}
				time.Sleep(20 * time.Millisecond)
				atomic.AddInt32(&inside, -1)
				return nil
			})
		}()
	}
	wg.Wait()
	if maxInside != 1 {
		t.Fatalf("advisory lock did not serialise: %d callers inside at once", maxInside)
	}
}
