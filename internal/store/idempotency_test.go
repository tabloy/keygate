package store_test

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/store"
)

// TestIdempotencyClaim_Atomic verifies that under concurrent claims of
// the same (key, endpoint, body_hash), exactly ONE goroutine wins (gets
// nil/nil and proceeds to run the handler) and all others get back the
// existing in-flight row. This is the core safety property of the
// idempotency layer; if it ever regresses, two retries can both run
// `/license/activate` and double-count against `max_activations`.
func TestIdempotencyClaim_Atomic(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	const concurrency = 32
	const key = "atomic-claim-test"
	const endpoint = "/api/v1/license/activate"
	const bodyHash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	// Clean up any prior row from a previous run.
	defer func() {
		_, _ = s.DB.NewRaw(
			`DELETE FROM idempotency_keys WHERE key = ? AND endpoint = ?`,
			key, endpoint).Exec(ctx)
	}()
	_, _ = s.DB.NewRaw(
		`DELETE FROM idempotency_keys WHERE key = ? AND endpoint = ?`,
		key, endpoint).Exec(ctx)

	var winners atomic.Int32
	var inflight atomic.Int32

	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(concurrency)
	for range concurrency {
		go func() {
			defer wg.Done()
			<-start
			row, err := s.IdempotencyClaim(ctx, key, endpoint, bodyHash)
			if err != nil {
				t.Errorf("unexpected err: %v", err)
				return
			}
			if row == nil {
				winners.Add(1)
			} else {
				inflight.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()

	if w := winners.Load(); w != 1 {
		t.Fatalf("exactly one goroutine must claim the slot; got %d winners (and %d in-flight)", w, inflight.Load())
	}
	if i := inflight.Load(); i != concurrency-1 {
		t.Fatalf("all other goroutines must see in-flight; got %d (expected %d)", i, concurrency-1)
	}
}

// TestIdempotencyClaim_BodyMismatch verifies that reusing a key with a
// different body returns ErrIdempotencyBodyMismatch, never silently
// accepts the second body, and never overwrites the first.
func TestIdempotencyClaim_BodyMismatch(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	key := "body-mismatch-" + strconv.FormatInt(int64(testTime()), 10)
	endpoint := "/api/v1/license/activate"
	const hashA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const hashB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	defer func() {
		_, _ = s.DB.NewRaw(
			`DELETE FROM idempotency_keys WHERE key = ? AND endpoint = ?`,
			key, endpoint).Exec(ctx)
	}()

	// First claim: succeeds.
	if row, err := s.IdempotencyClaim(ctx, key, endpoint, hashA); err != nil || row != nil {
		t.Fatalf("first claim should succeed; got row=%v err=%v", row, err)
	}

	// Reuse same key with different body → conflict.
	row, err := s.IdempotencyClaim(ctx, key, endpoint, hashB)
	if !errors.Is(err, store.ErrIdempotencyBodyMismatch) {
		t.Fatalf("expected ErrIdempotencyBodyMismatch, got row=%v err=%v", row, err)
	}
}

// TestIdempotencyComplete_Replay verifies that after Complete, a second
// claim with the same (key, endpoint, hash) returns the cached row with
// ResponseComplete=true.
func TestIdempotencyComplete_Replay(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	key := "replay-" + strconv.FormatInt(int64(testTime()), 10)
	endpoint := "/api/v1/license/activate"
	const hash = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"

	defer func() {
		_, _ = s.DB.NewRaw(
			`DELETE FROM idempotency_keys WHERE key = ? AND endpoint = ?`,
			key, endpoint).Exec(ctx)
	}()

	row, err := s.IdempotencyClaim(ctx, key, endpoint, hash)
	if err != nil || row != nil {
		t.Fatalf("claim: row=%v err=%v", row, err)
	}
	if err := s.IdempotencyComplete(ctx, key, endpoint, 200, `{"success":true}`); err != nil {
		t.Fatalf("complete: %v", err)
	}

	replay, err := s.IdempotencyClaim(ctx, key, endpoint, hash)
	if err != nil {
		t.Fatalf("replay claim: %v", err)
	}
	if replay == nil {
		t.Fatalf("replay must return existing row, got nil (slot was re-claimed!)")
	}
	if !replay.ResponseComplete {
		t.Fatalf("replay must show ResponseComplete=true")
	}
	if replay.ResponseStatus != 200 || replay.ResponseBody != `{"success":true}` {
		t.Fatalf("cached response wrong: status=%d body=%q", replay.ResponseStatus, replay.ResponseBody)
	}
}

// TestIdempotencyAbandon_FreesSlot verifies that abandoning a slot
// (handler crashed, 5xx) lets the next claim succeed fresh — otherwise
// retries get stuck IN_FLIGHT until the 24h TTL.
func TestIdempotencyAbandon_FreesSlot(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	key := "abandon-" + strconv.FormatInt(int64(testTime()), 10)
	endpoint := "/api/v1/license/activate"
	const hash = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"

	defer func() {
		_, _ = s.DB.NewRaw(
			`DELETE FROM idempotency_keys WHERE key = ? AND endpoint = ?`,
			key, endpoint).Exec(ctx)
	}()

	if row, err := s.IdempotencyClaim(ctx, key, endpoint, hash); err != nil || row != nil {
		t.Fatalf("claim: row=%v err=%v", row, err)
	}
	s.IdempotencyAbandon(ctx, key, endpoint)

	// Next claim should succeed again (fresh slot).
	row, err := s.IdempotencyClaim(ctx, key, endpoint, hash)
	if err != nil || row != nil {
		t.Fatalf("re-claim after abandon should succeed; got row=%v err=%v", row, err)
	}
}

// testTime returns a unique-ish time stamp for test key uniqueness
// when tests run in parallel.
func testTime() int64 {
	return time.Now().UnixNano()
}

func TestValidIdempotencyKey(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected bool
	}{
		{name: "valid_uuid", input: "c02a7b8e-1234-4567-89ab-cdef01234567", expected: true},
		{name: "valid_short_key", input: "a", expected: true},
		{name: "valid_256_chars", input: strings.Repeat("a", 256), expected: true},
		{name: "empty_string", input: "", expected: false},
		{name: "oversized_257_chars", input: strings.Repeat("a", 257), expected: false},
		{name: "contains_null_byte", input: "key\x00attack", expected: false},
		{name: "contains_newline", input: "key\nattack", expected: false},
		{name: "contains_carriage_return", input: "key\rattack", expected: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			actual := store.ValidIdempotencyKey(tc.input)
			if actual != tc.expected {
				t.Errorf("ValidIdempotencyKey(%q) = %v; want %v", tc.input, actual, tc.expected)
			}
		})
	}
}
