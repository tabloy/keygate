package middleware

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/tabloy/keygate/internal/store"
)

// requireStore spins up a real store against TEST_DATABASE_URL. The
// idempotency layer is tightly coupled to PostgreSQL semantics
// (INSERT ... ON CONFLICT DO NOTHING + SELECT FOR UPDATE behavior),
// so a mock would defeat the purpose. Skip cleanly when unconfigured.
func requireStore(t *testing.T) *store.Store {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("skipping idempotency middleware test: TEST_DATABASE_URL not set")
	}
	s, err := store.New(dsn)
	if err != nil {
		t.Skipf("skipping idempotency middleware test: %v", err)
	}
	if err := s.RunMigrations("../../db/migrations"); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	return s
}

// makeApp wires the Idempotency middleware in front of `handler` on
// POST /test, mirroring the production registration shape so the
// middleware's view of the path + method is identical to real usage.
func makeApp(s *store.Store, handler gin.HandlerFunc) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/test", Idempotency(s), handler)
	return r
}

func postJSON(r *gin.Engine, key, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/test",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	r.ServeHTTP(w, req)
	return w
}

// TestIdempotency_5xxNotCached — when the handler returns 500, the
// middleware MUST NOT cache the response. The slot is abandoned so
// a retry hits the handler again. Without this, a transient DB blip
// would forever "succeed" with a cached 500.
func TestIdempotency_5xxNotCached(t *testing.T) {
	s := requireStore(t)
	defer s.Close()

	var attempts atomic.Int32
	handler := func(c *gin.Context) {
		attempts.Add(1)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "transient"})
	}
	r := makeApp(s, handler)

	key := "idem-5xx-test-" + time.Now().Format("20060102150405.000000000")
	body := `{"x":1}`

	// First call — handler runs, returns 500. Middleware must NOT
	// store the response_complete=true row.
	w1 := postJSON(r, key, body)
	if w1.Code != 500 {
		t.Fatalf("first call: expected 500, got %d", w1.Code)
	}

	// DB invariant: no slot left behind.
	var count int
	if err := s.DB.NewRaw(
		"SELECT COUNT(*) FROM idempotency_keys WHERE key=?",
		key,
	).Scan(context.Background(), &count); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if count != 0 {
		t.Errorf("after 5xx: expected 0 idempotency rows, got %d (slot leaked)", count)
	}

	// Retry — handler runs AGAIN (no cache).
	w2 := postJSON(r, key, body)
	if w2.Code != 500 {
		t.Fatalf("retry: expected 500, got %d", w2.Code)
	}
	if a := attempts.Load(); a != 2 {
		t.Fatalf("handler attempt count: want 2 (no caching), got %d", a)
	}

	// Cleanup
	_, _ = s.DB.NewRaw("DELETE FROM idempotency_keys WHERE key=?", key).Exec(context.Background())
}

// TestIdempotency_4xxIsCached — counter-pin to the 5xx case: a 4xx
// (e.g. validation error) is deterministic, so we DO cache it.
// Retries return the same 4xx without re-running the handler.
func TestIdempotency_4xxIsCached(t *testing.T) {
	s := requireStore(t)
	defer s.Close()

	var attempts atomic.Int32
	handler := func(c *gin.Context) {
		attempts.Add(1)
		c.JSON(http.StatusBadRequest, gin.H{"error": "validation"})
	}
	r := makeApp(s, handler)

	key := "idem-4xx-test-" + time.Now().Format("20060102150405.000000000")
	body := `{"x":2}`

	if w1 := postJSON(r, key, body); w1.Code != 400 {
		t.Fatalf("first call: expected 400, got %d", w1.Code)
	}
	if w2 := postJSON(r, key, body); w2.Code != 400 {
		t.Fatalf("replay: expected 400, got %d", w2.Code)
	}
	if a := attempts.Load(); a != 1 {
		t.Fatalf("handler attempts: want 1 (cached on replay), got %d", a)
	}

	_, _ = s.DB.NewRaw("DELETE FROM idempotency_keys WHERE key=?", key).Exec(context.Background())
}

// TestIdempotency_PanicAbandonsSlot — if the handler panics mid-
// flight, the deferred cleanup must abandon the slot so the
// next retry can claim fresh. Without this, an in-flight slot
// stays IN_FLIGHT for the full 24h TTL.
func TestIdempotency_PanicAbandonsSlot(t *testing.T) {
	s := requireStore(t)
	defer s.Close()

	handler := func(c *gin.Context) {
		// Gin's default recovery catches the panic and turns it into
		// a 500 — that's the realistic failure mode. Idempotency
		// middleware's defer should still abandon the slot.
		panic("boom")
	}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(gin.Recovery())
	r.POST("/test", Idempotency(s), handler)

	key := "idem-panic-test-" + time.Now().Format("20060102150405.000000000")
	body := `{"x":3}`

	w := postJSON(r, key, body)
	if w.Code != 500 {
		t.Fatalf("expected 500 from panicked handler, got %d", w.Code)
	}

	// Slot must be gone (Recovery returns 500 → middleware abandons).
	var count int
	_ = s.DB.NewRaw("SELECT COUNT(*) FROM idempotency_keys WHERE key=?", key).
		Scan(context.Background(), &count)
	if count != 0 {
		t.Errorf("after panic: expected 0 rows, got %d (slot leaked)", count)
	}

	_, _ = s.DB.NewRaw("DELETE FROM idempotency_keys WHERE key=?", key).Exec(context.Background())
}

// TestIdempotency_InFlight409 — two truly concurrent requests with
// the same Idempotency-Key + same body: exactly ONE handler
// invocation; the second caller gets 409 IDEMPOTENCY_IN_FLIGHT.
//
// We force the race window with a sleep inside the handler so the
// second goroutine's Claim hits the row while ResponseComplete=false.
func TestIdempotency_InFlight409(t *testing.T) {
	s := requireStore(t)
	defer s.Close()

	var handlerRunning sync.WaitGroup
	handlerRunning.Add(1)
	release := make(chan struct{})
	var handlerEntries atomic.Int32

	handler := func(c *gin.Context) {
		// First entry: signal that we're running, then wait for the
		// second goroutine to have hit the middleware. After that we
		// finish normally.
		if handlerEntries.Add(1) == 1 {
			handlerRunning.Done()
			<-release
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	}
	r := makeApp(s, handler)

	key := "idem-inflight-" + time.Now().Format("20060102150405.000000000")
	body := `{"x":4}`

	var w1, w2 *httptest.ResponseRecorder
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		w1 = postJSON(r, key, body)
	}()
	go func() {
		defer wg.Done()
		// Wait for the first goroutine to enter the handler so the
		// row exists with ResponseComplete=false. Then we fire.
		handlerRunning.Wait()
		// Tiny grace to ensure the first one's row is committed to DB
		// (Claim does INSERT before the handler runs, so this should
		// be a no-op but reduces flakiness on slow CI).
		time.Sleep(50 * time.Millisecond)
		w2 = postJSON(r, key, body)
		close(release) // let the first one finish
	}()
	wg.Wait()

	// At least one must be the in-flight 409; the other the eventual 200.
	codes := []int{w1.Code, w2.Code}
	var got200, got409 int
	for _, c := range codes {
		switch c {
		case 200:
			got200++
		case 409:
			got409++
		}
	}
	if got200 != 1 || got409 != 1 {
		t.Fatalf("expected exactly one 200 and one 409, got w1=%d w2=%d", w1.Code, w2.Code)
	}

	// The 409 response must carry the IDEMPOTENCY_IN_FLIGHT code so
	// clients can branch on it (and not retry instantly).
	var conflictBody *httptest.ResponseRecorder
	if w1.Code == 409 {
		conflictBody = w1
	} else {
		conflictBody = w2
	}
	if !bytes.Contains(conflictBody.Body.Bytes(), []byte("IDEMPOTENCY_IN_FLIGHT")) {
		t.Errorf("409 body missing IDEMPOTENCY_IN_FLIGHT code: %s", conflictBody.Body.String())
	}

	// Handler executed exactly once.
	if a := handlerEntries.Load(); a != 1 {
		t.Errorf("expected exactly 1 handler invocation, got %d", a)
	}

	// Cleanup
	_, _ = s.DB.NewRaw("DELETE FROM idempotency_keys WHERE key=?", key).Exec(context.Background())
}

// Sanity: confirm the ErrIdempotencyBodyMismatch path still fires
// after our middleware changes. (Belt-and-suspenders — exercised by
// shell tests too, but pinning it here catches regressions in unit-
// test speed.)
func TestIdempotency_BodyMismatchAfterCache(t *testing.T) {
	s := requireStore(t)
	defer s.Close()

	handler := func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) }
	r := makeApp(s, handler)

	key := "idem-bm-" + time.Now().Format("20060102150405.000000000")
	if w := postJSON(r, key, `{"a":1}`); w.Code != 200 {
		t.Fatalf("first: %d", w.Code)
	}
	w2 := postJSON(r, key, `{"a":2}`)
	if w2.Code != 422 {
		t.Errorf("expected 422 for body mismatch, got %d", w2.Code)
	}

	// Sentinel error sanity (not surfaced through HTTP but pin the
	// store-level guarantee that the middleware relies on).
	// body_hash CHECK constraint requires 64-char lowercase hex.
	const validButDifferentHash = "deadbeef" + "00000000000000000000000000000000000000000000000000000000"
	_, err := s.IdempotencyClaim(context.Background(), key, "/test", validButDifferentHash)
	if !errors.Is(err, store.ErrIdempotencyBodyMismatch) {
		t.Errorf("expected ErrIdempotencyBodyMismatch, got %v", err)
	}

	_, _ = s.DB.NewRaw("DELETE FROM idempotency_keys WHERE key=?", key).Exec(context.Background())
}
