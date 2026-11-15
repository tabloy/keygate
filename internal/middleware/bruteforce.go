package middleware

import (
	"fmt"
	"math"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// BruteForceProtection tracks failed authentication attempts and blocks
// IPs/keys that exceed the threshold. Uses exponential backoff.
type BruteForceProtection struct {
	mu         sync.RWMutex
	attempts   map[string]*attemptRecord
	maxFails   int           // max failures before lockout
	lockout    time.Duration // initial lockout duration
	maxLockout time.Duration // maximum lockout (cap for exponential backoff)
	window     time.Duration // window to count failures
}

type attemptRecord struct {
	failures     int
	firstFailure time.Time
	lockedUntil  time.Time
	lockoutCount int // number of times locked out (for exponential backoff)
}

func NewBruteForceProtection(maxFails int, lockout, maxLockout, window time.Duration) *BruteForceProtection {
	bf := &BruteForceProtection{
		attempts:   make(map[string]*attemptRecord),
		maxFails:   maxFails,
		lockout:    lockout,
		maxLockout: maxLockout,
		window:     window,
	}
	go bf.cleanup()
	return bf
}

// RecordFailure records a failed attempt against a key — in practice
// the caller's address, spelled by FailureKeyIP. Keeping a count per
// licence key was dropped: nothing ever read it, and locking a key
// would have let anyone who learnt it lock out the customer who
// bought it.
func (bf *BruteForceProtection) RecordFailure(key string) {
	bf.mu.Lock()
	defer bf.mu.Unlock()

	now := time.Now()
	rec := bf.attempts[key]
	if rec == nil {
		rec = &attemptRecord{}
		bf.attempts[key] = rec
	}
	if rec.lockedUntil.After(now) {
		// A request that passed the guard just before this lockout began
		// is paid for by it. Counting it would start the next, doubled
		// lockout at once, and the count now outlives the window.
		return
	}
	if now.Sub(rec.firstFailure) > bf.window {
		// A new window starts the failure count again, but not the
		// lockout count: that count is what doubles each further
		// lockout, and a caller back when a lockout longer than the
		// window ends always lands here.
		rec.failures = 0
		rec.firstFailure = now
		// The count lasts as long as the longest lockout after the last
		// one ended. An address many people share (an office, a school)
		// that was locked out once must not have a stray typo, an hour
		// later, start it at the cap.
		if now.Sub(rec.lockedUntil) > bf.maxLockout {
			rec.lockoutCount = 0
		}
	}

	rec.failures++
	if rec.failures >= bf.maxFails {
		// Exponential backoff: lockout * 2^lockoutCount, capped at
		// maxLockout, the first lockout included.
		duration := min(bf.lockout, bf.maxLockout)
		// A lockout of zero or less (BF_LOCKOUT_SECONDS=0) blocks nobody,
		// and must not loop or overflow on a count that keeps growing.
		for i := 0; i < rec.lockoutCount && duration > 0 && duration < bf.maxLockout; i++ {
			duration = min(duration*2, bf.maxLockout)
		}
		rec.lockedUntil = now.Add(duration)
		rec.lockoutCount++
		rec.failures = 0
		rec.firstFailure = now
	}
}

// IsBlocked checks if a key is currently locked out.
func (bf *BruteForceProtection) IsBlocked(key string) (bool, time.Duration) {
	bf.mu.RLock()
	defer bf.mu.RUnlock()

	rec, exists := bf.attempts[key]
	if !exists {
		return false, 0
	}

	now := time.Now()
	if rec.lockedUntil.After(now) {
		return true, rec.lockedUntil.Sub(now)
	}
	return false, 0
}

func (bf *BruteForceProtection) cleanup() {
	for {
		time.Sleep(5 * time.Minute)
		bf.sweep(time.Now())
	}
}

// sweep forgets keys that have gone quiet. A key that was locked out is
// kept until it has been quiet for as long as the longest lockout. The
// sweep used to drop it four windows after its last lockout began,
// which a 30 minute lockout outlasts: a caller who waited a few minutes
// more after it started again from the shortest lockout.
func (bf *BruteForceProtection) sweep(now time.Time) {
	bf.mu.Lock()
	defer bf.mu.Unlock()
	for key, rec := range bf.attempts {
		if rec.lockedUntil.After(now) {
			continue
		}
		if rec.lockoutCount > 0 && now.Sub(rec.lockedUntil) <= bf.maxLockout {
			continue
		}
		if now.Sub(rec.firstFailure) > bf.window*4 {
			delete(bf.attempts, key)
		}
	}
}

// RetryAfterSeconds turns what is left of a lockout into the number a
// client is told to wait. It rounds up: truncating 59.9 to 59 sends a
// well-behaved client back a moment early, to another 429 and no
// better advice than the first one.
func RetryAfterSeconds(retryAfter time.Duration) int {
	seconds := max(int(math.Ceil(retryAfter.Seconds())), 1)
	return seconds
}

// FailureKeyIP is how a failed licence attempt is filed: one namespace
// shared by the middleware that blocks and the service that records,
// because a lockout written under one spelling and looked up under
// another never fires. The service layer builds the same string from
// the address the handler took off the request.
func FailureKeyIP(ip string) string { return "ip:" + ip }

// LicenseBruteForceGuard is a middleware that checks brute-force state before processing.
func LicenseBruteForceGuard(bf *BruteForceProtection) gin.HandlerFunc {
	return func(c *gin.Context) {
		blocked, retryAfter := bf.IsBlocked(FailureKeyIP(c.ClientIP()))
		if blocked {
			BruteForceBlocks.Inc()
			// retry_after lives in `error.details` (NOT alongside
			// code/message) so the response shape stays the canonical
			// envelope: { success, error: { code, message, details? } }.
			retrySec := RetryAfterSeconds(retryAfter)
			c.Header("Retry-After", fmt.Sprintf("%d", retrySec))
			abortWithErrorDetails(c, http.StatusTooManyRequests, "LOCKED_OUT",
				"too many failed attempts, please try again later",
				gin.H{"retry_after": retrySec})
			return
		}

		// Also check by license key if present in request body
		// We'll check post-processing via the context
		c.Set("brute_force", bf)
		c.Next()
	}
}
