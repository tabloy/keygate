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
// 32 random chars from a 32-char alphabet = 160 bits of entropy.
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

// KeyPrefix is the first segment of a key this package generated: the
// prefix of the product that issued it, whatever that product's prefix
// is now. Empty for any other key, such as one imported from another
// system, where the first segment may be part of the secret.
func KeyPrefix(key string) string {
	segments := strings.Split(key, "-")
	if len(segments) != 5 || !isPrefix(segments[0]) {
		return ""
	}
	for _, seg := range segments[1:] {
		if len(seg) != 8 || strings.Trim(seg, alphabet) != "" {
			return ""
		}
	}
	return segments[0]
}

// isPrefix reports whether s has the shape of a product's prefix: 2 to
// 8 uppercase letters or digits.
func isPrefix(s string) bool {
	if len(s) < 2 || len(s) > 8 {
		return false
	}
	for _, c := range s {
		if (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
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
	// Rejection sampling avoids modulo bias: only byte values below the
	// largest multiple of the alphabet length are used. With the current
	// 32 characters that is every byte, but the bound follows the
	// alphabet so a change to it cannot bring the bias back.
	const maxUnbiased = 256 - 256%len(alphabet)
	out := make([]byte, n)
	buf := make([]byte, 1)
	for i := 0; i < n; {
		if _, err := rand.Read(buf); err != nil {
			panic(fmt.Sprintf("crypto/rand: %v", err))
		}
		if int(buf[0]) >= maxUnbiased {
			continue // reject biased values
		}
		out[i] = alphabet[buf[0]%byte(len(alphabet))]
		i++
	}
	return string(out)
}
