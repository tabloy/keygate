package breaker

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestOpensAfterTripAndRecovers(t *testing.T) {
	b := New(3, 50*time.Millisecond)
	for range 2 {
		b.Failure()
	}
	if !b.Allow() {
		t.Fatal("should stay closed below the trip count")
	}
	b.Failure()
	if b.Allow() {
		t.Fatal("should open at the trip count")
	}
	time.Sleep(60 * time.Millisecond)
	if !b.Allow() {
		t.Fatal("one probe should be allowed after the cooldown")
	}
	b.Success()
	// Closed again: every call is allowed, not only the one probe a
	// half-open breaker lets through, so the second call matters too.
	for i := range 2 {
		if !b.Allow() {
			t.Fatalf("call %d refused: a successful probe should close the breaker", i+1)
		}
	}
}

func TestSuccessResetsFailureCount(t *testing.T) {
	b := New(3, time.Minute)
	b.Failure()
	b.Failure()
	b.Success()
	b.Failure()
	b.Failure()
	if !b.Allow() {
		t.Fatal("failures separated by a success must not trip the breaker")
	}
}

// After the cooldown exactly one of many concurrent callers may probe.
func TestSingleProbeAfterCooldown(t *testing.T) {
	b := New(1, 20*time.Millisecond)
	b.Failure()
	time.Sleep(30 * time.Millisecond)
	var allowed atomic.Int32
	var wg sync.WaitGroup
	for range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if b.Allow() {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()
	if n := allowed.Load(); n != 1 {
		t.Fatalf("expected exactly 1 probe, got %d", n)
	}
}

// A failed probe keeps the breaker open for another cooldown.
func TestFailedProbeStaysOpen(t *testing.T) {
	b := New(5, 20*time.Millisecond)
	for range 5 {
		b.Failure()
	}
	time.Sleep(30 * time.Millisecond)
	if !b.Allow() {
		t.Fatal("probe should be allowed")
	}
	b.Failure()
	if b.Allow() {
		t.Fatal("breaker must stay open after a failed probe")
	}
}
