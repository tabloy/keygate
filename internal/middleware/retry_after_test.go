package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// A lockout with 59.9 seconds left must not tell the client 59: it
// comes back a moment early, gets another 429, and has learnt nothing.
func TestRetryAfterSeconds(t *testing.T) {
	for _, tc := range []struct {
		left time.Duration
		want int
	}{
		{59900 * time.Millisecond, 60},
		{100 * time.Millisecond, 1},
		{time.Second, 1},
		{1500 * time.Millisecond, 2},
		{0, 1},
		{-time.Second, 1},
		{30 * time.Minute, 1800},
	} {
		if got := RetryAfterSeconds(tc.left); got != tc.want {
			t.Errorf("RetryAfterSeconds(%v) = %d, want %d", tc.left, got, tc.want)
		}
	}
}

// A plain rate limit says how long to wait too: a client that reads
// only the header still backs off instead of retrying at once.
func TestRateLimitSendsRetryAfter(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/x", RateLimitByIPScoped("retry-after-test", 1, time.Minute), func(c *gin.Context) { c.Status(http.StatusOK) })
	var w *httptest.ResponseRecorder
	for range 2 {
		w = httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/x", nil))
	}
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") != "60" {
		t.Fatalf("second request = %d, Retry-After %q; want 429 and 60", w.Code, w.Header().Get("Retry-After"))
	}
}
