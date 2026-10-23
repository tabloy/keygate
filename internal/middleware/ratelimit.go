// Rate limiting middleware with pluggable backends.
// Supports in-memory (single instance) and Redis (multi-instance) backends.
// Set REDIS_URL to enable Redis backend; falls back to in-memory if not set.
package middleware

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/tabloy/keygate/internal/breaker"
)

// RateLimitBackend abstracts the rate limiting storage.
type RateLimitBackend interface {
	Allow(key string, rate int, window time.Duration) bool
}

// ─── In-Memory Backend (default) ───

type memoryBackend struct {
	mu       sync.Mutex
	visitors map[string]*visitor
}

type visitor struct {
	count int
	// windowStart is when the current window opened, and window is the
	// length it was opened with. Both are needed by the sweeper: it
	// must not drop a counter whose window is still running, or the
	// caller silently gets a fresh allowance.
	windowStart time.Time
	window      time.Duration
}

func NewMemoryBackend() RateLimitBackend {
	mb := &memoryBackend{visitors: make(map[string]*visitor)}
	go func() {
		for {
			time.Sleep(time.Minute)
			mb.cleanup()
		}
	}()
	return mb
}

func (mb *memoryBackend) Allow(key string, rate int, window time.Duration) bool {
	mb.mu.Lock()
	defer mb.mu.Unlock()

	v, exists := mb.visitors[key]
	now := time.Now()

	// Fixed window, matching the Redis backend's INCR+EXPIRE: the
	// counter resets `window` after it opened, not after the caller
	// falls silent. Resetting on idle meant a caller who kept sending
	// never rolled over — they stayed locked out until they went
	// quiet for a full window, which for the hour-long OTP budget is
	// an hour of silence from a whole NATed office.
	if !exists || now.Sub(v.windowStart) >= window {
		mb.visitors[key] = &visitor{count: 1, windowStart: now, window: window}
		return true
	}

	v.count++
	return v.count <= rate
}

// cleanup drops counters whose window has closed. It must respect each
// entry's own window: a flat threshold shorter than the window deletes
// live counters, which hands the caller a fresh allowance — a fixed
// 5-minute sweep turned the hour-long OTP budget into "30 sends per
// five idle minutes", roughly ten times what it advertises.
func (mb *memoryBackend) cleanup() {
	mb.mu.Lock()
	defer mb.mu.Unlock()
	now := time.Now()
	for key, v := range mb.visitors {
		// Short windows linger a little so the map is not rebuilt on
		// every sweep; the entry is expired either way, so keeping it
		// costs nothing but a map slot.
		ttl := max(v.window, 5*time.Minute)
		if now.Sub(v.windowStart) > ttl {
			delete(mb.visitors, key)
		}
	}
}

// ─── Redis Backend (optional) ───

// RedisClient is a minimal interface for Redis operations needed by rate limiting.
// Compatible with github.com/redis/go-redis/v9.
type RedisClient interface {
	Eval(ctx context.Context, script string, keys []string, args ...any) RedisResult
}

// RedisResult is the minimal result interface.
type RedisResult interface {
	Int64() (int64, error)
}

type redisBackend struct {
	client RedisClient
	// prefix namespaces keys per install (e.g. "kg:<hash>:rl:") so two
	// deployments sharing one Redis DB do not share rate-limit counters.
	prefix string
}

// rlOpTimeout bounds one Redis rate-limit call. Without it a slow or
// wedged endpoint would stall every limited request behind go-redis'
// multi-second dial/read timeouts before failing over.
const rlOpTimeout = 250 * time.Millisecond

// Lua script for atomic rate limiting: INCR + EXPIRE in one round trip.
const rateLimitScript = `
local key = KEYS[1]
local limit = tonumber(ARGV[1])
local window = tonumber(ARGV[2])
local current = redis.call('INCR', key)
if current == 1 then
  redis.call('EXPIRE', key, window)
end
return current
`

// allow runs the script under a short timeout and reports the decision
// plus any backend error, so a caller can degrade instead of blocking.
func (rb *redisBackend) allow(key string, rate int, window time.Duration) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), rlOpTimeout)
	defer cancel()
	result := rb.client.Eval(ctx, rateLimitScript, []string{rb.prefix + key}, rate, int(window.Seconds()))
	count, err := result.Int64()
	if err != nil {
		return true, err
	}
	return count <= int64(rate), nil
}

// resilientBackend uses Redis for shared, cross-instance limiting but
// degrades to a per-instance in-memory limiter whenever Redis errors, so
// a slow or down endpoint neither stalls requests (each try is bounded by
// rlOpTimeout) nor silently drops all limiting. A breaker stops probing a
// dead endpoint on every request, then lets one request retry after a
// cooldown.
type resilientBackend struct {
	redis  *redisBackend
	memory RateLimitBackend
	br     *breaker.Breaker
}

// NewResilientRedisBackend wires a Redis rate limiter that falls back to
// an in-memory limiter on Redis failure. prefix namespaces keys per
// install.
func NewResilientRedisBackend(client RedisClient, prefix string) RateLimitBackend {
	return &resilientBackend{
		redis:  &redisBackend{client: client, prefix: prefix},
		memory: NewMemoryBackend(),
		br:     breaker.New(5, 5*time.Second),
	}
}

func (r *resilientBackend) Allow(key string, rate int, window time.Duration) bool {
	if !r.br.Allow() {
		return r.memory.Allow(key, rate, window)
	}
	ok, err := r.redis.allow(key, rate, window)
	if err != nil {
		r.br.Failure()
		return r.memory.Allow(key, rate, window)
	}
	r.br.Success()
	return ok
}

// ─── Default backend (package-level) ───

var defaultBackend RateLimitBackend = NewMemoryBackend()

// SetRateLimitBackend sets the global rate limit backend (call once at startup).
func SetRateLimitBackend(b RateLimitBackend) {
	defaultBackend = b
}

// ─── Middleware ───

// rateLimited answers 429. Retry-After is the whole window: the backend
// does not say when this caller's count falls back under the limit, and
// a full window always clears it.
func rateLimited(c *gin.Context, window time.Duration) {
	c.Header("Retry-After", strconv.Itoa(max(1, int(window.Seconds()))))
	abortWithError(c, http.StatusTooManyRequests, "RATE_LIMITED", "too many requests, please try again later")
}

// RateLimit creates a rate limiting middleware using the configured backend.
func RateLimit(rate int, window time.Duration) gin.HandlerFunc {
	return func(c *gin.Context) {
		key := c.ClientIP()
		if ak, exists := c.Get("api_key"); exists {
			if apiKey, ok := ak.(interface{ GetID() string }); ok {
				key = "apikey:" + apiKey.GetID()
			}
		}

		if !defaultBackend.Allow(key, rate, window) {
			rateLimited(c, window)
			return
		}
		c.Next()
	}
}

// RateLimitByIPScoped is RateLimitByIP with its own counter.
//
// RateLimitByIP buckets on "ip:<addr>" alone, so two of them on one
// route share a single counter and fight over whose rate/window wins.
// A scope gives an endpoint that needs a tighter budget than its group
// — /auth/otp/send, which mails an address the caller chose — a bucket
// of its own instead of eating into the shared one.
func RateLimitByIPScoped(scope string, rate int, window time.Duration) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !defaultBackend.Allow("ip:"+scope+":"+c.ClientIP(), rate, window) {
			rateLimited(c, window)
			return
		}
		c.Next()
	}
}

// RateLimitByIP creates a rate limiter keyed by IP only.
func RateLimitByIP(rate int, window time.Duration) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !defaultBackend.Allow("ip:"+c.ClientIP(), rate, window) {
			rateLimited(c, window)
			return
		}
		c.Next()
	}
}
