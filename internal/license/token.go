package license

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// VerifyToken is returned to clients for offline license verification.
// Signed with HMAC-SHA256 so the client can validate without calling the server.
type VerifyToken struct {
	LicenseID   string         `json:"lid"`
	ProductID   string         `json:"pid"`
	PlanID      string         `json:"pln"`
	Status      string         `json:"sts"`
	Identifier  string         `json:"did"`
	Features    map[string]any `json:"ftr,omitempty"`
	IssuedAt    int64          `json:"iat"`
	ExpiresAt   int64          `json:"exp"`
	GraceDays   int            `json:"grc"`
	Nonce       string         `json:"nce"`           // unique per-issuance to prevent replay
	Fingerprint string         `json:"fpr,omitempty"` // SHA256(identifier+product_id) for binding
}

// Sign produces a signed token string: base64url(payload).base64url(hmac).
func Sign(t *VerifyToken, secret string) (string, error) {
	if t.Nonce == "" {
		nonce := make([]byte, 16)
		if _, err := rand.Read(nonce); err != nil {
			return "", fmt.Errorf("nonce: %w", err)
		}
		t.Nonce = base64.RawURLEncoding.EncodeToString(nonce)
	}
	payload, err := json.Marshal(t)
	if err != nil {
		return "", fmt.Errorf("marshal: %w", err)
	}
	b64 := base64.RawURLEncoding.EncodeToString(payload)
	sig := hmacSHA256(b64, secret)
	return b64 + "." + sig, nil
}

// Verify parses a signed token, checks signature and expiry.
func Verify(raw, secret string) (*VerifyToken, error) {
	idx := strings.LastIndexByte(raw, '.')
	if idx < 0 {
		return nil, fmt.Errorf("invalid token format")
	}
	b64, sig := raw[:idx], raw[idx+1:]

	if !hmac.Equal([]byte(sig), []byte(hmacSHA256(b64, secret))) {
		return nil, fmt.Errorf("invalid signature")
	}

	payload, err := base64.RawURLEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}

	var t VerifyToken
	if err := json.Unmarshal(payload, &t); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}

	if t.ExpiresAt > 0 && time.Now().Unix() > t.ExpiresAt {
		return nil, fmt.Errorf("token expired")
	}

	return &t, nil
}

// Fingerprint creates a binding hash for the token to prevent cross-device replay.
func Fingerprint(identifier, productID string) string {
	h := sha256.Sum256([]byte(identifier + ":" + productID))
	return hex.EncodeToString(h[:8]) // 64-bit truncated hash
}

func hmacSHA256(msg, key string) string {
	h := hmac.New(sha256.New, []byte(key))
	h.Write([]byte(msg))
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}
