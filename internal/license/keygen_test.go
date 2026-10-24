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
	for range 1000 {
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

// TestRandomSegmentUnbiased checks every alphabet character is drawn
// about equally often. The old bound of 248 (a multiple of 31) made 8 of
// the 32 characters come up 7/8 as often as the rest.
func TestRandomSegmentUnbiased(t *testing.T) {
	counts := map[rune]int{}
	const n = 320000
	for _, r := range randomSegment(n) {
		counts[r]++
	}
	if len(counts) != len(alphabet) {
		t.Fatalf("expected all %d characters, got %d", len(alphabet), len(counts))
	}
	want := n / len(alphabet)
	for r, c := range counts {
		if c < want*95/100 || c > want*105/100 {
			t.Errorf("character %q drawn %d times, want about %d", r, c, want)
		}
	}
}

// KeyPrefix reads the prefix off keys this package made, and nothing
// off any other key: there the first segment may be part of the secret.
func TestKeyPrefix(t *testing.T) {
	for _, tc := range []struct{ key, want string }{
		{GenerateKey(""), "KG"},
		{GenerateKey("ACME2"), "ACME2"},
		{"KG-ABCD2345-EFGH6789-JKLM2345-NPQR6789", "KG"},
		{"ABCD-EFGH-JKLM-NPQR", ""},                              // imported, four segments
		{"XY-abcd2345-EFGH6789-JKLM2345-NPQR6789", ""},           // lowercase is not ours
		{"XY-ABCD2345-EFGH6789-JKLM2345-NPQR678", ""},            // short segment
		{"XY-ABCD2345-EFGH6789-JKLM2345-NPQR6780", ""},           // 0 is not in the alphabet
		{"SECRETSECRET-ABCDEFGH-JKLMNPQR-STUVWXYZ-23456789", ""}, // first segment too long to be a prefix
		{"ab-ABCDEFGH-JKLMNPQR-STUVWXYZ-23456789", ""},           // nor lowercase
		{"K-ABCDEFGH-JKLMNPQR-STUVWXYZ-23456789", ""},            // nor one character
		{"NOHYPHENKEY", ""},
		{"", ""},
	} {
		if got := KeyPrefix(tc.key); got != tc.want {
			t.Errorf("KeyPrefix(%q) = %q, want %q", tc.key, got, tc.want)
		}
	}
}
