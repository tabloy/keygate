package license

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// GenerateKey creates a high-entropy license key: PREFIX-XXXXXXXX-XXXXXXXX-XXXXXXXX-XXXXXXXX
// 32 random chars from 31-char alphabet = ~155 bits of entropy.
func GenerateKey(prefix string) string {
	if prefix == "" {
		prefix = "KG"
	}
	segments := make([]string, 4)
	for i := range segments {
		segments[i] = randomSegment(8)
	}
	return prefix + "-" + strings.Join(segments, "-")
}

// HashKey returns a SHA-256 hash of the license key for secure storage.
func HashKey(key string) string {
	h := sha256.Sum256([]byte(normalizeKey(key)))
	return hex.EncodeToString(h[:])
}

// normalizeKey strips whitespace and uppercases for consistent hashing.
func normalizeKey(key string) string {
	return strings.ToUpper(strings.TrimSpace(key))
}

func randomSegment(n int) string {
	// Use rejection sampling to avoid modulo bias.
	// 248 is the largest multiple of 31 that fits in a byte (31*8=248).
	const maxUnbiased = 248
	out := make([]byte, n)
	buf := make([]byte, 1)
	for i := 0; i < n; {
		if _, err := rand.Read(buf); err != nil {
			panic(fmt.Sprintf("crypto/rand: %v", err))
		}
		if buf[0] >= maxUnbiased {
			continue // reject biased values
		}
		out[i] = alphabet[buf[0]%byte(len(alphabet))]
		i++
	}
	return string(out)
}
