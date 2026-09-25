package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/crypto"
	"github.com/tabloy/keygate/internal/store"
)

// A 32-byte test key for the settings AEAD.
func testAEAD(t *testing.T) *crypto.AESGCM {
	t.Helper()
	a, err := crypto.NewAESGCM(make([]byte, 32))
	if err != nil {
		t.Fatalf("aead: %v", err)
	}
	return a
}

func providerTestStore(t *testing.T) *store.Store {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	db, err := store.New(dsn)
	if err != nil {
		t.Skipf("store: %v", err)
	}
	db.LicenseKeyAEAD = testAEAD(t)
	t.Cleanup(func() { db.Close() })
	return db
}

// The Cloudflare transport builds the documented request and reads the
// documented response. delivered → nil; permanent bounce → error even
// though success is true; success:false → the API error surfaces.
func TestCloudflareTransport(t *testing.T) {
	cases := []struct {
		name       string
		respStatus int
		resp       string
		wantErr    string
	}{
		{"delivered", 200, `{"success":true,"errors":[],"result":{"delivered":["u@x.com"],"permanent_bounces":[],"queued":[]}}`, ""},
		{"bounce", 200, `{"success":true,"errors":[],"result":{"delivered":[],"permanent_bounces":["u@x.com"],"queued":[]}}`, "permanent bounce"},
		{"api error", 400, `{"success":false,"errors":[{"code":10001,"message":"invalid_request_schema"}],"result":null}`, "invalid_request_schema"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotAuth, gotBody, gotPath string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotAuth = r.Header.Get("Authorization")
				gotPath = r.URL.Path
				b, _ := io.ReadAll(r.Body)
				gotBody = string(b)
				w.WriteHeader(tc.respStatus)
				_, _ = w.Write([]byte(tc.resp))
			}))
			defer srv.Close()

			old := cfEndpoint
			cfEndpoint = srv.URL + "/accounts/%s/email/sending/send"
			defer func() { cfEndpoint = old }()

			svc := &EmailService{logger: slog.Default()}
			cfg := resolvedConfig{provider: "cloudflare", from: "noreply@x.com", apiID: "acct123", apiKey: "tok_abc"}
			err := svc.sendCloudflare(context.Background(), cfg, "u@x.com", "Hi", "<p>Hi</p>", "Hi")

			if tc.wantErr == "" && err != nil {
				t.Fatalf("want success, got %v", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
			// Request shape, checked regardless of outcome.
			if gotAuth != "Bearer tok_abc" {
				t.Errorf("Authorization = %q, want Bearer tok_abc", gotAuth)
			}
			if !strings.Contains(gotPath, "acct123") {
				t.Errorf("account id not in path: %q", gotPath)
			}
			var body map[string]string
			if err := json.Unmarshal([]byte(gotBody), &body); err != nil {
				t.Fatalf("request body not JSON: %v", err)
			}
			for _, f := range []string{"from", "to", "subject", "html", "text"} {
				if body[f] == "" {
					t.Errorf("request body missing %q", f)
				}
			}
		})
	}
}

// resolve reads DB config when a provider is set there, and falls back
// to the env values captured at construction otherwise. All or nothing.
func TestResolveDBOverridesEnv(t *testing.T) {
	db := providerTestStore(t)
	ctx := context.Background()
	// Clean slate for the keys this test touches.
	for _, k := range []string{"email_provider", "cloudflare_from", "cloudflare_account_id", "cloudflare_api_token"} {
		_ = db.DeleteSetting(ctx, k)
	}
	t.Cleanup(func() {
		for _, k := range []string{"email_provider", "cloudflare_from", "cloudflare_account_id", "cloudflare_api_token"} {
			_ = db.DeleteSetting(ctx, k)
		}
	})

	svc := &EmailService{
		host: "smtp.env.example", port: "587", from: "env@example.com",
		logger: slog.Default(), store: db,
	}

	// No DB config → env fallback.
	if c, err := svc.resolve(ctx); err != nil || c.provider != "smtp" || c.source != "env" || c.host != "smtp.env.example" {
		t.Fatalf("env fallback wrong: %+v err=%v", c, err)
	}

	// Configure Cloudflare in the DB → DB wins, env ignored.
	if err := db.SetSettings(ctx, map[string]string{
		"email_provider":        "cloudflare",
		"cloudflare_from":       "db@example.com",
		"cloudflare_account_id": "acct999",
		"cloudflare_api_token":  "tok_secret",
	}); err != nil {
		t.Fatalf("set: %v", err)
	}
	c, err := svc.resolve(ctx)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if c.provider != "cloudflare" || c.source != "db" {
		t.Fatalf("expected cloudflare/db, got %+v", c)
	}
	if c.from != "db@example.com" || c.apiID != "acct999" || c.apiKey != "tok_secret" {
		t.Fatalf("db values wrong: %+v", c)
	}
	if c.host != "" {
		t.Errorf("env host leaked into DB-mode config: %q", c.host)
	}
	if !c.configured() {
		t.Error("cloudflare config should be configured")
	}
}

// A secret setting is encrypted at rest and comes back intact.
func TestSecretSettingRoundTrip(t *testing.T) {
	db := providerTestStore(t)
	ctx := context.Background()
	_ = db.DeleteSetting(ctx, "cloudflare_api_token")
	t.Cleanup(func() { _ = db.DeleteSetting(ctx, "cloudflare_api_token") })

	if err := db.SetSettings(ctx, map[string]string{"cloudflare_api_token": "super-secret-token"}); err != nil {
		t.Fatalf("set: %v", err)
	}
	// Raw value in the table must not be the plaintext.
	raw, _ := db.GetSetting(ctx, "cloudflare_api_token")
	if raw == "super-secret-token" {
		t.Fatal("secret stored in plaintext")
	}
	if raw == "" {
		t.Fatal("secret row missing")
	}
	// Decrypted read returns the original.
	got, err := db.GetSecretSetting(ctx, "cloudflare_api_token")
	if err != nil || got != "super-secret-token" {
		t.Fatalf("GetSecretSetting = %q, %v", got, err)
	}
}

func TestHTMLToText(t *testing.T) {
	in := `<html><body><h2>Hi</h2><p>Your code is 123456</p><style>.x{}</style></body></html>`
	out := htmlToText(in)
	if !strings.Contains(out, "Your code is 123456") {
		t.Errorf("text missing content: %q", out)
	}
	if strings.Contains(out, "<") || strings.Contains(out, ".x{}") {
		t.Errorf("tags or style leaked: %q", out)
	}
}

// A claimed queue mail that cannot be delivered must not be marked
// sent. Before, the queue re-resolved config per mail and a mid-batch
// "not configured" made Send return nil, so the row was marked sent
// and its body wiped without ever leaving. The queue now uses a
// captured config and sendResolved, which reports failure as an error.
func TestQueueDoesNotMarkUndeliverableMailSent(t *testing.T) {
	db := providerTestStore(t)
	ctx := context.Background()
	for _, k := range []string{"email_provider", "cloudflare_from", "cloudflare_account_id", "cloudflare_api_token"} {
		_ = db.DeleteSetting(ctx, k)
	}
	t.Cleanup(func() {
		for _, k := range []string{"email_provider", "cloudflare_from", "cloudflare_account_id", "cloudflare_api_token"} {
			_ = db.DeleteSetting(ctx, k)
		}
	})

	// A Cloudflare that fails transiently (5xx): the mail must be kept
	// for retry, never marked sent. (Permanent failures are terminal and
	// wipe the body — see TestQueueMarksPermanentFailureTerminal.)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"success":false,"errors":[{"code":1,"message":"oops"}],"result":null}`))
	}))
	defer srv.Close()
	old := cfEndpoint
	cfEndpoint = srv.URL + "/accounts/%s/email/sending/send"
	defer func() { cfEndpoint = old }()

	if err := db.SetSettings(ctx, map[string]string{
		"email_provider":        "cloudflare",
		"cloudflare_from":       "q@example.com",
		"cloudflare_account_id": "acct",
		"cloudflare_api_token":  "tok",
	}); err != nil {
		t.Fatalf("configure: %v", err)
	}

	to := "queue-undeliverable-" + time.Now().Format("150405.000000") + "@example.com"
	body := `<html><body>KG-QUEUEKEY-QUEUEKEY-QUEUEKEY-QUEUE</body></html>`
	if err := db.EnqueueEmail(ctx, to, "Your license", body); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	t.Cleanup(func() { _, _ = db.DB.NewRaw("DELETE FROM email_queue WHERE to_addr = ?", to).Exec(ctx) })

	svc := &EmailService{logger: slog.Default(), store: db}
	svc.processQueue(ctx, db)

	var status string
	var bodyLen int
	if err := db.DB.NewRaw(
		"SELECT status, length(body) FROM email_queue WHERE to_addr = ?", to,
	).Scan(ctx, &status, &bodyLen); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if status == "sent" {
		t.Error("undeliverable mail was marked sent")
	}
	if bodyLen == 0 {
		t.Error("undeliverable mail had its body wiped (retry would send nothing)")
	}
}

// The queue path must never read "could not send" as success. Send is
// best-effort and returns nil when email is not configured (OTP falls
// back to the log); sendResolved, which the queue uses, must instead
// return an error for any unusable config. This is the exact contract
// that keeps a claimed mail from being marked sent and wiped.
func TestSendResolvedReportsFailureWhereSendSkips(t *testing.T) {
	svc := &EmailService{logger: slog.Default()} // store nil -> env config, unconfigured

	if err := svc.Send("x@y.com", "s", "<p>b</p>"); err != nil {
		t.Errorf("Send should skip (nil) when unconfigured, got %v", err)
	}
	if err := svc.sendResolved(resolvedConfig{provider: "smtp"}, "x@y.com", "s", "<p>b</p>"); err == nil {
		t.Error("sendResolved returned nil for an unusable config; the queue would mark the mail sent")
	}
}

// Without a master key, a secret setting must be refused, not stored in
// the clear. A plaintext credential in the settings table that the
// admin believes is encrypted is the exact leak this guards.
func TestSecretRefusedWithoutEncryptionKey(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	db, err := store.New(dsn)
	if err != nil {
		t.Skipf("store: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	db.LicenseKeyAEAD = nil // no master key configured
	ctx := context.Background()
	_ = db.DeleteSetting(ctx, "smtp_password")
	t.Cleanup(func() { _ = db.DeleteSetting(ctx, "smtp_password") })

	err = db.SetSettings(ctx, map[string]string{"smtp_password": "hunter2"})
	if !errors.Is(err, store.ErrSecretEncryptionUnavailable) {
		t.Fatalf("want ErrSecretEncryptionUnavailable, got %v", err)
	}
	// Nothing must have been written.
	if v, _ := db.GetSetting(ctx, "smtp_password"); v != "" {
		t.Errorf("secret was persisted despite no key: %q", v)
	}

	// A non-secret setting still saves fine without a key.
	if err := db.SetSettings(ctx, map[string]string{"site_name": "NoKey OK"}); err != nil {
		t.Errorf("non-secret save should not require a key: %v", err)
	}
	_ = db.DeleteSetting(ctx, "site_name")
}

// A prefixed (encrypted) secret with no master key must be an error,
// not the ciphertext returned as if it were the value. Returning
// "enc:v1:..." would be fed to SMTP AUTH / Cloudflare and fail every
// send while the status might still read configured.
func TestGetSecretRefusesDecryptWithoutKey(t *testing.T) {
	db := providerTestStore(t) // has AEAD
	ctx := context.Background()
	_ = db.DeleteSetting(ctx, "cloudflare_api_token")
	t.Cleanup(func() { _ = db.DeleteSetting(ctx, "cloudflare_api_token") })

	// Store it encrypted (AEAD present), then drop the key.
	if err := db.SetSettings(ctx, map[string]string{"cloudflare_api_token": "tok"}); err != nil {
		t.Fatalf("set: %v", err)
	}
	db.LicenseKeyAEAD = nil

	if _, err := db.GetSecretSetting(ctx, "cloudflare_api_token"); !errors.Is(err, store.ErrSecretEncryptionUnavailable) {
		t.Fatalf("want ErrSecretEncryptionUnavailable, got %v", err)
	}
}

// resolve must not turn a read error into an env fallback or a silent
// "not configured". Send surfaces it as a failure instead of dropping
// the mail.
func TestSendFailsOnConfigReadError(t *testing.T) {
	db := providerTestStore(t)
	ctx := context.Background()
	// A DB-configured Cloudflare with an encrypted token, then no key:
	// resolve's GetSecretSetting errors, and Send must report it.
	for _, k := range []string{"email_provider", "cloudflare_from", "cloudflare_account_id", "cloudflare_api_token"} {
		_ = db.DeleteSetting(ctx, k)
	}
	t.Cleanup(func() {
		for _, k := range []string{"email_provider", "cloudflare_from", "cloudflare_account_id", "cloudflare_api_token"} {
			_ = db.DeleteSetting(ctx, k)
		}
	})
	if err := db.SetSettings(ctx, map[string]string{
		"email_provider": "cloudflare", "cloudflare_from": "c@x.com",
		"cloudflare_account_id": "acct", "cloudflare_api_token": "tok",
	}); err != nil {
		t.Fatalf("set: %v", err)
	}
	db.LicenseKeyAEAD = nil // now the token can't be decrypted

	svc := &EmailService{logger: slog.Default(), store: db}
	if err := svc.Send("u@x.com", "s", "<p>b</p>"); err == nil {
		t.Fatal("Send returned nil despite an unreadable config; the mail would be silently dropped")
	}
}

// Retry policy: a permanent Cloudflare failure (4xx / success:false /
// bounce) is sent once and not retried; a transient one (5xx) gets the
// single retry. Retrying a permanent bounce re-delivers to a bad
// address and burns sending reputation.
func TestCloudflareRetryOnlyTransient(t *testing.T) {
	cases := []struct {
		name          string
		status        int
		body          string
		wantHits      int32
		wantPermanent bool
		wantRetryable bool
	}{
		// 4xx credential/config: try once, keep in queue (not terminal).
		{"config 400", 400, `{"success":false,"errors":[{"code":10001,"message":"bad"}]}`, 1, false, false},
		{"auth 401", 401, `{"success":false,"errors":[{"code":10000,"message":"unauthorized"}]}`, 1, false, false},
		// recipient bounce: terminal, no retry.
		{"permanent bounce", 200, `{"success":true,"result":{"delivered":[],"permanent_bounces":["u@x.com"]}}`, 1, true, false},
		// 5xx: transient, one immediate retry.
		{"transient 500", 500, `{"success":false,"errors":[{"code":1,"message":"oops"}]}`, 2, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var hits int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&hits, 1)
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			old := cfEndpoint
			cfEndpoint = srv.URL + "/accounts/%s/email/sending/send"
			defer func() { cfEndpoint = old }()

			svc := &EmailService{logger: slog.Default()}
			cfg := resolvedConfig{provider: "cloudflare", from: "f@x.com", apiID: "a", apiKey: "k"}
			err := svc.sendResolved(cfg, "u@x.com", "s", "<p>b</p>")
			if err == nil {
				t.Fatal("expected an error")
			}
			if got := atomic.LoadInt32(&hits); got != tc.wantHits {
				t.Errorf("provider hit %d times, want %d", got, tc.wantHits)
			}
			if isPermanent(err) != tc.wantPermanent {
				t.Errorf("isPermanent=%v, want %v", isPermanent(err), tc.wantPermanent)
			}
			if isRetryable(err) != tc.wantRetryable {
				t.Errorf("isRetryable=%v, want %v", isRetryable(err), tc.wantRetryable)
			}
		})
	}
}

func TestQueueMarksPermanentFailureTerminal(t *testing.T) {
	db := providerTestStore(t)
	ctx := context.Background()
	for _, k := range []string{"email_provider", "cloudflare_from", "cloudflare_account_id", "cloudflare_api_token"} {
		_ = db.DeleteSetting(ctx, k)
	}
	t.Cleanup(func() {
		for _, k := range []string{"email_provider", "cloudflare_from", "cloudflare_account_id", "cloudflare_api_token"} {
			_ = db.DeleteSetting(ctx, k)
		}
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Bounce whatever recipient this request carried — a permanent
		// recipient bounce is the one terminal case.
		var req struct {
			To string `json:"to"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		_, _ = w.Write([]byte(`{"success":true,"result":{"delivered":[],"permanent_bounces":["` + req.To + `"]}}`))
	}))
	defer srv.Close()
	old := cfEndpoint
	cfEndpoint = srv.URL + "/accounts/%s/email/sending/send"
	defer func() { cfEndpoint = old }()

	if err := db.SetSettings(ctx, map[string]string{
		"email_provider": "cloudflare", "cloudflare_from": "q@example.com",
		"cloudflare_account_id": "acct", "cloudflare_api_token": "tok",
	}); err != nil {
		t.Fatalf("configure: %v", err)
	}
	to := "queue-permanent-" + time.Now().Format("150405.000000") + "@example.com"
	if err := db.EnqueueEmail(ctx, to, "Your license", "<html><body>KG-PERMKEY-PERMKEY-PERMKEY-PERM</body></html>"); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	t.Cleanup(func() { _, _ = db.DB.NewRaw("DELETE FROM email_queue WHERE to_addr = ?", to).Exec(ctx) })

	svc := &EmailService{logger: slog.Default(), store: db}
	svc.processQueue(ctx, db)

	var status string
	var bodyLen int
	if err := db.DB.NewRaw("SELECT status, length(body) FROM email_queue WHERE to_addr = ?", to).Scan(ctx, &status, &bodyLen); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if status != "failed" {
		t.Errorf("permanent failure should be terminal (failed), got %q", status)
	}
	if bodyLen != 0 {
		t.Errorf("terminal-failed mail should not keep its body, len=%d", bodyLen)
	}
}

// A credential/config failure (401/403/wrong account id) is recoverable
// once an operator fixes it, so the queue must KEEP the mail and its
// body for a later attempt — never wipe it as terminal. This is the
// token-rotation case where licence mail would otherwise be lost.
func TestQueueKeepsConfigErrorForRetry(t *testing.T) {
	db := providerTestStore(t)
	ctx := context.Background()
	for _, k := range []string{"email_provider", "cloudflare_from", "cloudflare_account_id", "cloudflare_api_token"} {
		_ = db.DeleteSetting(ctx, k)
	}
	t.Cleanup(func() {
		for _, k := range []string{"email_provider", "cloudflare_from", "cloudflare_account_id", "cloudflare_api_token"} {
			_ = db.DeleteSetting(ctx, k)
		}
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401) // expired / wrong token
		_, _ = w.Write([]byte(`{"success":false,"errors":[{"code":10000,"message":"unauthorized"}]}`))
	}))
	defer srv.Close()
	old := cfEndpoint
	cfEndpoint = srv.URL + "/accounts/%s/email/sending/send"
	defer func() { cfEndpoint = old }()

	if err := db.SetSettings(ctx, map[string]string{
		"email_provider": "cloudflare", "cloudflare_from": "q@example.com",
		"cloudflare_account_id": "acct", "cloudflare_api_token": "stale",
	}); err != nil {
		t.Fatalf("configure: %v", err)
	}
	to := "queue-config-" + time.Now().Format("150405.000000") + "@example.com"
	if err := db.EnqueueEmail(ctx, to, "Your license", "<html><body>KG-CFGKEY-CFGKEY-CFGKEY-CFGK</body></html>"); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	t.Cleanup(func() { _, _ = db.DB.NewRaw("DELETE FROM email_queue WHERE to_addr = ?", to).Exec(ctx) })

	svc := &EmailService{logger: slog.Default(), store: db}
	svc.processQueue(ctx, db)

	var status string
	var bodyLen int
	if err := db.DB.NewRaw("SELECT status, length(body) FROM email_queue WHERE to_addr = ?", to).Scan(ctx, &status, &bodyLen); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if status == "failed" {
		t.Error("a config error was marked terminal; the mail is lost on token rotation")
	}
	if status == "sent" {
		t.Error("a config error was marked sent")
	}
	if bodyLen == 0 {
		t.Error("config-error mail had its body wiped; fixing the token cannot recover it")
	}
}
