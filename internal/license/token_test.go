package license

import "testing"

func TestSignAndVerify(t *testing.T) {
	secret := "test-secret-key"
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

	signed, err := Sign(token, secret)
	if err != nil {
		t.Fatalf("Sign failed: %v", err)
	}
	if signed == "" {
		t.Fatal("signed token is empty")
	}

	parsed, err := Verify(signed, secret)
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
	secret := "test-secret"
	token := &VerifyToken{
		LicenseID: "lic-123", Status: "active",
		IssuedAt: 1700000000, ExpiresAt: 9999999999,
	}

	signed, _ := Sign(token, secret)
	_, err := Verify(signed, "wrong-secret")
	if err == nil {
		t.Fatal("expected error for wrong secret")
	}
}

func TestVerifyExpiredToken(t *testing.T) {
	secret := "test-secret"
	token := &VerifyToken{
		LicenseID: "lic-123", Status: "active",
		IssuedAt: 1700000000, ExpiresAt: 1700000001, // already expired
	}

	signed, _ := Sign(token, secret)
	_, err := Verify(signed, secret)
	if err == nil {
		t.Fatal("expected error for expired token")
	}
}

func TestVerifyInvalidFormat(t *testing.T) {
	_, err := Verify("not-a-valid-token", "secret")
	if err == nil {
		t.Fatal("expected error for invalid format")
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
