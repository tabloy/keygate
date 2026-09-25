// Rate limiting middleware with pluggable backends.
// Supports in-memory (single instance) and Redis (multi-instance) backends.
// Set REDIS_URL to enable Redis backend; falls back to in-memory if not set.
package middleware

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
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
}

// NewRedisBackend creates a Redis-backed rate limiter.
func NewRedisBackend(client RedisClient) RateLimitBackend {
	return &redisBackend{client: client}
}

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

func (rb *redisBackend) Allow(key string, rate int, window time.Duration) bool {
	result := rb.client.Eval(
		context.Background(),
		rateLimitScript,
		[]string{"rl:" + key},
		rate,
		int(window.Seconds()),
	)
	count, err := result.Int64()
	if err != nil {
		return true // fail open on Redis errors
	}
	return count <= int64(rate)
}

// ─── Default backend (package-level) ───

var defaultBackend RateLimitBackend = NewMemoryBackend()

// SetRateLimitBackend sets the global rate limit backend (call once at startup).
func SetRateLimitBackend(b RateLimitBackend) {
	defaultBackend = b
}

// ─── Middleware ───

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
			abortWithError(c, http.StatusTooManyRequests, "RATE_LIMITED", "too many requests, please try again later")
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
			abortWithError(c, http.StatusTooManyRequests, "RATE_LIMITED", "too many requests, please try again later")
			return
		}
		c.Next()
	}
}

// RateLimitByIP creates a rate limiter keyed by IP only.
func RateLimitByIP(rate int, window time.Duration) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !defaultBackend.Allow("ip:"+c.ClientIP(), rate, window) {
			abortWithError(c, http.StatusTooManyRequests, "RATE_LIMITED", "too many requests, please try again later")
			return
		}
		c.Next()
	}
}
