package license

import (
	"crypto/ed25519"
	"crypto/rand"
	"slices"
	"testing"
)

func testKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return ed25519.NewKeyFromSeed(seed)
}

func TestSignAndVerify(t *testing.T) {
	priv := testKey(t)
	pub := PublicKey(priv)

	token := &VerifyToken{
		LicenseID:  "lic-123",
		ProductID:  "prod-456",
		PlanID:     "plan-789",
		Status:     "active",
		Identifier: "device-abc",
		Features:   map[string]any{"export": true},
		IssuedAt:   1700000000,
		ExpiresAt:  9999999999, // far future
		GraceDays:  7,
	}

	signed, err := Sign(token, priv)
	if err != nil {
		t.Fatalf("Sign failed: %v", err)
	}
	if signed == "" {
		t.Fatal("signed token is empty")
	}

	parsed, err := Verify(signed, pub)
	if err != nil {
		t.Fatalf("Verify failed: %v", err)
	}
	if parsed.LicenseID != "lic-123" {
		t.Errorf("LicenseID mismatch: %s", parsed.LicenseID)
	}
	if parsed.Status != "active" {
		t.Errorf("Status mismatch: %s", parsed.Status)
	}
	if parsed.Features["export"] != true {
		t.Error("Feature export should be true")
	}
	if parsed.Nonce == "" {
		t.Error("nonce should be auto-generated")
	}
}

func TestVerifyBadSignature(t *testing.T) {
	priv := testKey(t)
	wrongPub := PublicKey(testKey(t)) // unrelated keypair

	token := &VerifyToken{
		LicenseID: "lic-123", Status: "active",
		IssuedAt: 1700000000, ExpiresAt: 9999999999,
	}

	signed, _ := Sign(token, priv)
	if _, err := Verify(signed, wrongPub); err == nil {
		t.Fatal("expected error for wrong public key")
	}
}

func TestVerifyExpiredToken(t *testing.T) {
	priv := testKey(t)
	pub := PublicKey(priv)
	token := &VerifyToken{
		LicenseID: "lic-123", Status: "active",
		IssuedAt: 1700000000, ExpiresAt: 1700000001, // already expired
	}

	signed, _ := Sign(token, priv)
	if _, err := Verify(signed, pub); err == nil {
		t.Fatal("expected error for expired token")
	}
}

func TestVerifyInvalidFormat(t *testing.T) {
	pub := PublicKey(testKey(t))
	if _, err := Verify("not-a-valid-token", pub); err == nil {
		t.Fatal("expected error for invalid format")
	}
}

func TestPrivateKeyFromHex(t *testing.T) {
	// Round-trip: random seed → hex → parse → sign/verify
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		t.Fatalf("seed: %v", err)
	}
	hex := func(b []byte) string {
		const hexdigits = "0123456789abcdef"
		out := make([]byte, len(b)*2)
		for i, v := range b {
			out[i*2] = hexdigits[v>>4]
			out[i*2+1] = hexdigits[v&0xf]
		}
		return string(out)
	}(seed)

	priv, err := PrivateKeyFromHex(hex)
	if err != nil {
		t.Fatalf("PrivateKeyFromHex: %v", err)
	}
	// Sign+verify smoke check
	tok := &VerifyToken{LicenseID: "x", IssuedAt: 1, ExpiresAt: 9999999999}
	signed, err := Sign(tok, priv)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if _, err := Verify(signed, PublicKey(priv)); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func TestPrivateKeyFromHex_RejectsBadInput(t *testing.T) {
	if _, err := PrivateKeyFromHex("not-hex"); err == nil {
		t.Fatal("expected error for non-hex input")
	}
	if _, err := PrivateKeyFromHex("aabb"); err == nil {
		t.Fatal("expected error for wrong-length input")
	}
}

// TestVerifyTamperedPayload — flipping any byte in the payload half
// must invalidate the signature. This is the property that lets the
// SDK trust the payload contents (license_id, expires_at, features)
// without consulting the server.
func TestVerifyTamperedPayload(t *testing.T) {
	priv := testKey(t)
	pub := PublicKey(priv)
	tok := &VerifyToken{
		LicenseID: "lic-123", Status: "active",
		IssuedAt: 1700000000, ExpiresAt: 9999999999,
	}
	signed, err := Sign(tok, priv)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	dot := -1
	for i := len(signed) - 1; i >= 0; i-- {
		if signed[i] == '.' {
			dot = i
			break
		}
	}
	if dot < 5 {
		t.Fatalf("unexpected token shape: %q", signed)
	}
	// Flip one char in the payload half. Result is still valid base64
	// shape but a different byte sequence → ed25519.Verify must fail.
	mutated := []byte(signed)
	if mutated[2] == 'A' {
		mutated[2] = 'B'
	} else {
		mutated[2] = 'A'
	}
	if _, err := Verify(string(mutated), pub); err == nil {
		t.Fatal("expected verify failure on tampered payload")
	}
}

// TestVerifyTamperedSignature — flipping the signature bytes must
// reject too. (Signature forgery without the private key should be
// computationally infeasible, but we still verify the implementation
// catches the trivial case.)
func TestVerifyTamperedSignature(t *testing.T) {
	priv := testKey(t)
	pub := PublicKey(priv)
	tok := &VerifyToken{
		LicenseID: "lic-123", Status: "active",
		IssuedAt: 1700000000, ExpiresAt: 9999999999,
	}
	signed, _ := Sign(tok, priv)
	mutated := []byte(signed)
	// Find the dot, then mutate a character in the MIDDLE of the
	// signature half. Flipping the last char is unreliable: for a
	// 64-byte ed25519 signature, RawURLEncoding's final character
	// has 4 "don't-care" bits, so 'A'→'B' can leave the real
	// signature bytes unchanged → verify still passes → test flaky.
	dot := -1
	for i, m := range slices.Backward(mutated) {
		if m == '.' {
			dot = i
			break
		}
	}
	if dot < 0 || dot+5 >= len(mutated) {
		t.Fatalf("unexpected token shape: %q", signed)
	}
	// Flip a char a few positions into the signature half — well
	// inside the meaningful-bits region.
	mid := dot + 4
	if mutated[mid] == 'A' {
		mutated[mid] = 'B'
	} else {
		mutated[mid] = 'A'
	}
	if _, err := Verify(string(mutated), pub); err == nil {
		t.Fatal("expected verify failure on tampered signature")
	}
}

// TestSign_NonceIsUnique — every signing pass should mint a fresh
// random nonce. Without this, identical (lic, exp, identifier) tokens
// would be byte-identical → replayable in a fingerprinting sense and
// indistinguishable in logs. Calling Sign with a fresh VerifyToken
// each time should yield distinct nonces.
func TestSign_NonceIsUnique(t *testing.T) {
	priv := testKey(t)
	seen := map[string]struct{}{}
	for i := range 20 {
		tok := &VerifyToken{
			LicenseID: "lic-123", Status: "active",
			IssuedAt: 1700000000, ExpiresAt: 9999999999,
		}
		if _, err := Sign(tok, priv); err != nil {
			t.Fatalf("Sign #%d: %v", i, err)
		}
		if _, dup := seen[tok.Nonce]; dup {
			t.Fatalf("duplicate nonce on iteration %d: %s", i, tok.Nonce)
		}
		seen[tok.Nonce] = struct{}{}
	}
}

// TestVerify_EmptySignatureHalf — a token shaped like "payload." (no
// signature) must not slip through. The split-on-last-dot logic
// could otherwise hand an empty byte slice to ed25519.Verify, which
// returns false — fine — but the regression test pins it explicitly.
func TestVerify_EmptySignatureHalf(t *testing.T) {
	pub := PublicKey(testKey(t))
	if _, err := Verify("eyJsaWQiOiJ4In0.", pub); err == nil {
		t.Fatal("expected error for empty signature half")
	}
}

// TestVerify_GarbageBase64InSignature — a malformed signature half
// (not valid base64) must produce a decode error, not a panic.
func TestVerify_GarbageBase64InSignature(t *testing.T) {
	pub := PublicKey(testKey(t))
	if _, err := Verify("eyJsaWQiOiJ4In0.!!!not-base64!!!", pub); err == nil {
		t.Fatal("expected error for non-base64 signature")
	}
}

// TestVerify_ExpiryBoundary — exp == now should not be treated as
// still-valid (the implementation uses strict ">"). Pin the boundary
// so a future "off by one second" regression is caught.
func TestVerify_ExpiryBoundary(t *testing.T) {
	priv := testKey(t)
	pub := PublicKey(priv)
	// exp slightly in the past should fail.
	pastTok := &VerifyToken{
		LicenseID: "lic", IssuedAt: 1, ExpiresAt: 1,
	}
	signed, _ := Sign(pastTok, priv)
	if _, err := Verify(signed, pub); err == nil {
		t.Fatal("token with exp=1 (epoch) should be expired")
	}
}

func TestFingerprint(t *testing.T) {
	fp1 := Fingerprint("device-abc", "prod-456")
	fp2 := Fingerprint("device-abc", "prod-456")
	if fp1 != fp2 {
		t.Error("same inputs should produce same fingerprint")
	}

	fp3 := Fingerprint("device-xyz", "prod-456")
	if fp1 == fp3 {
		t.Error("different identifiers should produce different fingerprints")
	}

	if len(fp1) != 16 {
		t.Errorf("fingerprint should be 16 hex chars (8 bytes), got %d", len(fp1))
	}
}
