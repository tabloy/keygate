package license

import (
	"strings"
	"testing"
)

func TestGenerateKey(t *testing.T) {
	key := GenerateKey("")
	if !strings.HasPrefix(key, "KG-") {
		t.Errorf("expected prefix KG-, got %s", key)
	}
	parts := strings.Split(key, "-")
	if len(parts) != 5 {
		t.Errorf("expected 5 parts, got %d: %s", len(parts), key)
	}
	for i := 1; i < 5; i++ {
		if len(parts[i]) != 8 {
			t.Errorf("segment %d should be 8 chars, got %d: %s", i, len(parts[i]), parts[i])
		}
	}
}

func TestGenerateKeyCustomPrefix(t *testing.T) {
	key := GenerateKey("PF")
	if !strings.HasPrefix(key, "PF-") {
		t.Errorf("expected prefix PF-, got %s", key)
	}
}

func TestGenerateKeyUniqueness(t *testing.T) {
	keys := make(map[string]bool)
	for i := 0; i < 1000; i++ {
		k := GenerateKey("")
		if keys[k] {
			t.Fatalf("duplicate key generated: %s", k)
		}
		keys[k] = true
	}
}

func TestHashKey(t *testing.T) {
	key := "KG-ABCDEFGH-12345678-ABCDEFGH-12345678"
	h1 := HashKey(key)
	h2 := HashKey(key)
	if h1 != h2 {
		t.Error("same key should produce same hash")
	}
	if len(h1) != 64 {
		t.Errorf("hash should be 64 hex chars, got %d", len(h1))
	}

	// Different key = different hash
	other := "KG-ZZZZZZZZ-99999999-ZZZZZZZZ-99999999"
	h3 := HashKey(other)
	if h1 == h3 {
		t.Error("different keys should produce different hashes")
	}
}

func TestHashKeyNormalization(t *testing.T) {
	h1 := HashKey("KG-ABCD-1234-ABCD-1234")
	h2 := HashKey("  kg-abcd-1234-abcd-1234  ")
	if h1 != h2 {
		t.Error("normalization should make these equal")
	}
}
