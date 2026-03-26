package middleware

import (
	"testing"
	"time"
)

func TestMemoryBackendAllow(t *testing.T) {
	mb := NewMemoryBackend().(*memoryBackend)

	for i := 0; i < 3; i++ {
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

	// Backdate the visitor so it appears stale (older than 5-minute threshold)
	mb.mu.Lock()
	mb.visitors["old-key"].lastSeen = time.Now().Add(-10 * time.Minute)
	mb.mu.Unlock()

	mb.cleanup()

	mb.mu.Lock()
	_, exists := mb.visitors["old-key"]
	mb.mu.Unlock()

	if exists {
		t.Fatal("old-key should be cleaned up")
	}
}
