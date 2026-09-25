package middleware

import (
	"testing"
	"time"
)

func TestMemoryBackendAllow(t *testing.T) {
	mb := NewMemoryBackend().(*memoryBackend)

	for i := range 3 {
		if !mb.Allow("test-key", 3, time.Minute) {
			t.Fatalf("request %d should be allowed", i+1)
		}
	}

	if mb.Allow("test-key", 3, time.Minute) {
		t.Fatal("4th request should be denied")
	}
}

func TestMemoryBackendDifferentKeys(t *testing.T) {
	mb := NewMemoryBackend().(*memoryBackend)

	if !mb.Allow("key-a", 2, time.Minute) {
		t.Fatal("key-a request 1 should be allowed")
	}
	if !mb.Allow("key-a", 2, time.Minute) {
		t.Fatal("key-a request 2 should be allowed")
	}
	if mb.Allow("key-a", 2, time.Minute) {
		t.Fatal("key-a request 3 should be denied")
	}

	if !mb.Allow("key-b", 2, time.Minute) {
		t.Fatal("key-b request 1 should be allowed")
	}
}

func TestMemoryBackendWindowReset(t *testing.T) {
	mb := NewMemoryBackend().(*memoryBackend)

	mb.Allow("key", 2, 50*time.Millisecond)
	mb.Allow("key", 2, 50*time.Millisecond)
	if mb.Allow("key", 2, 50*time.Millisecond) {
		t.Fatal("should be denied")
	}

	time.Sleep(60 * time.Millisecond)
	if !mb.Allow("key", 2, 50*time.Millisecond) {
		t.Fatal("should be allowed after window reset")
	}
}

func TestMemoryBackendCleanup(t *testing.T) {
	mb := NewMemoryBackend().(*memoryBackend)

	mb.Allow("old-key", 10, 50*time.Millisecond)

	// Backdate the visitor so its window is long closed.
	mb.mu.Lock()
	mb.visitors["old-key"].windowStart = time.Now().Add(-10 * time.Minute)
	mb.mu.Unlock()

	mb.cleanup()

	mb.mu.Lock()
	_, exists := mb.visitors["old-key"]
	mb.mu.Unlock()

	if exists {
		t.Fatal("old-key should be cleaned up")
	}
}

// The sweeper runs on a fixed one-minute tick, so it has to look at
// each entry's own window. Deleting a counter whose window is still
// open resets the caller's allowance early — with the old flat
// five-minute threshold, an hour-long budget refilled twelve times an
// hour.
func TestMemoryBackendCleanupKeepsLiveLongWindows(t *testing.T) {
	mb := NewMemoryBackend().(*memoryBackend)

	mb.Allow("otp:1.2.3.4", 2, time.Hour)
	mb.Allow("otp:1.2.3.4", 2, time.Hour)

	// Twenty minutes in: past any flat threshold, nowhere near the
	// window's end.
	mb.mu.Lock()
	mb.visitors["otp:1.2.3.4"].windowStart = time.Now().Add(-20 * time.Minute)
	mb.mu.Unlock()

	mb.cleanup()

	if mb.Allow("otp:1.2.3.4", 2, time.Hour) {
		t.Fatal("counter was swept mid-window — the caller got a fresh allowance")
	}
}

// A caller who never stops sending must still roll over when the
// window ends. The previous idle-based reset kept them locked out for
// as long as they kept trying.
func TestMemoryBackendWindowRollsOverWhileBusy(t *testing.T) {
	mb := NewMemoryBackend().(*memoryBackend)

	window := 60 * time.Millisecond
	for range 2 {
		mb.Allow("busy", 2, window)
	}

	// Keep hitting it throughout the window, then check the next one.
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		mb.Allow("busy", 2, window)
		time.Sleep(10 * time.Millisecond)
	}

	if !mb.Allow("busy", 2, window) {
		t.Fatal("still denied after the window closed — reset depends on going idle")
	}
}
