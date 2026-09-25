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
	rec, exists := bf.attempts[key]
	if !exists || now.Sub(rec.firstFailure) > bf.window {
		bf.attempts[key] = &attemptRecord{failures: 1, firstFailure: now}
		return
	}

	rec.failures++
	if rec.failures >= bf.maxFails {
		// Exponential backoff: lockout * 2^lockoutCount, capped at maxLockout
		duration := bf.lockout
		for i := 0; i < rec.lockoutCount; i++ {
			duration *= 2
			if duration > bf.maxLockout {
				duration = bf.maxLockout
				break
			}
		}
		rec.lockedUntil = now.Add(duration)
		rec.lockoutCount++
		rec.failures = 0
		rec.firstFailure = now
	}
}

// RecordSuccess clears the failure record for a key.
func (bf *BruteForceProtection) RecordSuccess(key string) {
	bf.mu.Lock()
	defer bf.mu.Unlock()
	delete(bf.attempts, key)
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
		bf.mu.Lock()
		now := time.Now()
		for key, rec := range bf.attempts {
			// Remove records that are expired and not locked
			if now.Sub(rec.firstFailure) > bf.window*4 && !rec.lockedUntil.After(now) {
				delete(bf.attempts, key)
			}
		}
		bf.mu.Unlock()
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
