package service

import (
	"bufio"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"net/smtp"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestSendOnce_AuthPickerMatrix drives the real EmailService.sendOnce
// path against an in-process SMTP server. The server advertises a
// configurable AUTH mechanism list so we can verify the client picks
// the right one in every realistic deployment shape:
//
//	advertised      → expected mechanism
//	----------------|-----------------------------
//	PLAIN LOGIN     → PLAIN (standard preferred)
//	LOGIN           → LOGIN (Office 365 case — issue #2)
//	PLAIN           → PLAIN (Gmail / SES / SendGrid case)
//	XOAUTH2         → error: no supported mechanism
//
// We also cover the no-creds path (anonymous SMTP relay) where the
// client must NOT attempt AUTH at all.
func TestSendOnce_AuthPickerMatrix(t *testing.T) {
	cases := []struct {
		name           string
		advertise      string
		username       string
		wantUsedMech   string // "PLAIN" / "LOGIN" / "" (no auth)
		wantErrSubstr  string // empty → expect success
		wantPlainLogin bool   // if true, server expects valid LOGIN creds back
	}{
		{
			name:         "PLAIN+LOGIN advertised → client picks PLAIN",
			advertise:    "PLAIN LOGIN",
			username:     "alice",
			wantUsedMech: "PLAIN",
		},
		{
			name:           "LOGIN only (Office 365 shape) → client picks LOGIN",
			advertise:      "LOGIN XOAUTH2",
			username:       "alice", // matches the mock's wantUsername
			wantUsedMech:   "LOGIN",
			wantPlainLogin: true,
		},
		{
			name:         "PLAIN only (Gmail / SES) → client picks PLAIN",
			advertise:    "PLAIN",
			username:     "alice",
			wantUsedMech: "PLAIN",
		},
		{
			name:          "no supported mechanism (XOAUTH2 only) → error",
			advertise:     "XOAUTH2",
			username:      "alice",
			wantErrSubstr: "no supported AUTH mechanism",
		},
		{
			name:         "no credentials configured → client skips AUTH entirely",
			advertise:    "PLAIN LOGIN",
			username:     "", // anonymous relay
			wantUsedMech: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newMockSMTP(t, mockSMTPConfig{
				advertiseAuth: tc.advertise,
				wantUsername:  "alice",
				wantPassword:  "s3cret",
			})
			defer srv.Close()

			svc := &EmailService{
				host:     "127.0.0.1",
				port:     fmt.Sprintf("%d", srv.Port()),
				username: tc.username,
				password: "s3cret",
				from:     "noreply@keygate.test",
				enabled:  true,
				logger:   slog.Default(),
				// Test-only: the mock server presents an ephemeral
				// self-signed cert. Trust it without modifying the
				// machine's CA store. Production leaves tlsConfig nil
				// and so verifies the cert chain normally.
				tlsConfig: &tls.Config{ServerName: "127.0.0.1", InsecureSkipVerify: true}, //nolint:gosec
			}

			err := svc.sendOnce(svc.host+":"+svc.port, "to@example.com",
				[]byte("Subject: ok\r\n\r\nhi\r\n"))

			if tc.wantErrSubstr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErrSubstr) {
					t.Fatalf("expected error containing %q, got %v", tc.wantErrSubstr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("sendOnce: %v", err)
			}

			if got := srv.SelectedMech(); got != tc.wantUsedMech {
				t.Errorf("auth mechanism: want %q, got %q", tc.wantUsedMech, got)
			}
			if tc.username != "" && !srv.AuthSucceeded() {
				t.Errorf("server did not record successful auth")
			}
			if tc.username == "" && srv.SelectedMech() != "" {
				t.Errorf("anonymous send should have skipped AUTH, but mech=%q", srv.SelectedMech())
			}
		})
	}
}

// TestSendOnce_RejectsBadLoginCredentials covers the case where the
// AUTH LOGIN mechanism is correctly selected but the credentials are
// wrong — server must respond 535 and the client should surface a
// meaningful error rather than panic or silently succeed.
func TestSendOnce_RejectsBadLoginCredentials(t *testing.T) {
	srv := newMockSMTP(t, mockSMTPConfig{
		advertiseAuth: "LOGIN",
		wantUsername:  "alice",
		wantPassword:  "RIGHT-PASSWORD",
	})
	defer srv.Close()

	svc := &EmailService{
		host:      "127.0.0.1",
		port:      fmt.Sprintf("%d", srv.Port()),
		username:  "alice",
		password:  "WRONG-PASSWORD",
		from:      "noreply@keygate.test",
		enabled:   true,
		logger:    slog.Default(),
		tlsConfig: &tls.Config{ServerName: "127.0.0.1", InsecureSkipVerify: true}, //nolint:gosec
	}

	err := svc.sendOnce(svc.host+":"+svc.port, "to@example.com",
		[]byte("Subject: ok\r\n\r\nhi\r\n"))
	if err == nil {
		t.Fatal("expected auth error with wrong password, got nil")
	}
	if !strings.Contains(err.Error(), "auth") {
		t.Errorf("error should mention auth, got: %v", err)
	}
}

// TestLoginAuth_RefusesPlaintextConnection pins the security guard:
// LOGIN sends credentials in (base64 of) plaintext, so the auth
// implementation must refuse to start without TLS. Without this guard
// a misconfigured SMTP_PORT=25 + no STARTTLS path would leak the
// password to the wire.
func TestLoginAuth_RefusesPlaintextConnection(t *testing.T) {
	a := &loginAuth{username: "u", password: "p", host: "example.com"}
	_, _, err := a.Start(&smtp.ServerInfo{TLS: false, Name: "example.com"})
	if err == nil {
		t.Fatal("loginAuth.Start should refuse non-TLS connection")
	}
	if !strings.Contains(err.Error(), "unencrypted") && !strings.Contains(err.Error(), "TLS") {
		t.Errorf("error should mention TLS / unencrypted, got: %v", err)
	}
}

// TestLoginAuth_WrongHostname — when the server identity in the TLS
// handshake doesn't match the configured host, refuse to send
// credentials. Prevents downgrade-style attacks where a MITM presents
// itself as a different domain.
func TestLoginAuth_WrongHostname(t *testing.T) {
	a := &loginAuth{username: "u", password: "p", host: "expected.com"}
	_, _, err := a.Start(&smtp.ServerInfo{TLS: true, Name: "attacker.com"})
	if err == nil {
		t.Fatal("loginAuth.Start should refuse wrong host name")
	}
}

// TestLoginAuth_PromptVariants — Microsoft 365 sends "Username:" /
// "Password:". Some legacy servers send "User Name:" or lowercased
// variants. The picker has to be lenient on whitespace + casing +
// trailing colon, but reject unknown prompts.
func TestLoginAuth_PromptVariants(t *testing.T) {
	a := &loginAuth{username: "alice", password: "s3cret", host: "ex.com"}
	for _, prompt := range []string{"Username:", "username:", "User Name:", "USERNAME", " Username : "} {
		resp, err := a.Next([]byte(prompt), true)
		if err != nil {
			t.Errorf("prompt %q: unexpected error: %v", prompt, err)
			continue
		}
		if string(resp) != "alice" {
			t.Errorf("prompt %q: want alice, got %q", prompt, resp)
		}
	}
	for _, prompt := range []string{"Password:", "password", "PASSWORD:"} {
		resp, err := a.Next([]byte(prompt), true)
		if err != nil {
			t.Errorf("prompt %q: unexpected error: %v", prompt, err)
			continue
		}
		if string(resp) != "s3cret" {
			t.Errorf("prompt %q: want s3cret, got %q", prompt, resp)
		}
	}
	// Unknown prompt must error.
	if _, err := a.Next([]byte("Token:"), true); err == nil {
		t.Error("unknown prompt should error")
	}
	// more=false means the auth dialog is over — no more challenges.
	if resp, err := a.Next([]byte("anything"), false); err != nil || resp != nil {
		t.Errorf("Next(more=false) should return (nil,nil), got (%q,%v)", resp, err)
	}
}

// TestValidateSMTPLine blocks header-injection attempts via CR or LF.
func TestValidateSMTPLine(t *testing.T) {
	good := []string{"alice@example.com", "user.name+tag@host.io"}
	for _, s := range good {
		if err := validateSMTPLine(s); err != nil {
			t.Errorf("good input %q rejected: %v", s, err)
		}
	}
	bad := []string{"bad@ex.com\r\nRCPT TO:<evil>", "bad@ex.com\nINJECT", "\rfoo"}
	for _, s := range bad {
		if err := validateSMTPLine(s); err == nil {
			t.Errorf("CRLF injection in %q should have been rejected", s)
		}
	}
}

// TestParseEnvelopeAddress pins the RFC 5321/5322 split that broke
// Postmark ("501 Bad sender address syntax"): SMTP_FROM may carry a
// display name for the header, but the envelope MAIL FROM must be
// the bare addr-spec.
func TestParseEnvelopeAddress(t *testing.T) {
	ok := []struct{ in, want string }{
		{"noreply@example.com", "noreply@example.com"},
		{"Keygate <noreply@example.com>", "noreply@example.com"},
		{`"Keygate Billing" <billing@example.com>`, "billing@example.com"},
		{"  Keygate <noreply@example.com>  ", "noreply@example.com"},
		{"a.b+tag@sub.example.co.uk", "a.b+tag@sub.example.co.uk"},
	}
	for _, tc := range ok {
		got, err := parseEnvelopeAddress(tc.in)
		if err != nil {
			t.Errorf("parseEnvelopeAddress(%q) errored: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseEnvelopeAddress(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	bad := []string{"", "   ", "not-an-email", "two@@at.com", "name <>"}
	for _, in := range bad {
		if got, err := parseEnvelopeAddress(in); err == nil {
			t.Errorf("parseEnvelopeAddress(%q) should have errored, got %q", in, got)
		}
	}
}

// ─── In-process SMTP test server ────────────────────────────

type mockSMTPConfig struct {
	advertiseAuth string // space-separated mechanisms
	wantUsername  string
	wantPassword  string
}

type mockSMTP struct {
	t       *testing.T
	ln      net.Listener
	wg      sync.WaitGroup
	tlsCert tls.Certificate

	mu            sync.Mutex
	selectedMech  string
	authSucceeded bool
	gotFrom       string
	gotTo         []string
	gotData       string
}

func newMockSMTP(t *testing.T, cfg mockSMTPConfig) *mockSMTP {
	t.Helper()
	cert := genSelfSignedCert(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &mockSMTP{t: t, ln: ln, tlsCert: cert}
	srv.wg.Add(1)
	go srv.acceptLoop(cfg)
	return srv
}

func (s *mockSMTP) Port() int            { return s.ln.Addr().(*net.TCPAddr).Port }
func (s *mockSMTP) SelectedMech() string { s.mu.Lock(); defer s.mu.Unlock(); return s.selectedMech }
func (s *mockSMTP) AuthSucceeded() bool  { s.mu.Lock(); defer s.mu.Unlock(); return s.authSucceeded }
func (s *mockSMTP) Close()               { _ = s.ln.Close(); s.wg.Wait() }

func (s *mockSMTP) acceptLoop(cfg mockSMTPConfig) {
	defer s.wg.Done()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.wg.Add(1)
		go func(c net.Conn) {
			defer s.wg.Done()
			defer c.Close()
			s.handle(c, cfg)
		}(conn)
	}
}

// handle drives one SMTP session. Minimal but realistic:
//   - 220 banner
//   - EHLO → 250 multi-line (advertise STARTTLS + AUTH)
//   - STARTTLS → 220 → wrap conn in TLS
//   - EHLO again (post-TLS, same advertisement)
//   - AUTH PLAIN <b64> → check creds → 235 / 535
//     AUTH LOGIN → 334 base64("Username:") → 334 base64("Password:") → 235/535
//   - MAIL FROM / RCPT TO / DATA / . / QUIT
func (s *mockSMTP) handle(c net.Conn, cfg mockSMTPConfig) {
	fmt.Fprint(c, "220 mockSMTP ready\r\n")
	r := bufio.NewReader(c)
	var inTLS bool
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.TrimSpace(line)
		upper := strings.ToUpper(cmd)
		switch {
		case strings.HasPrefix(upper, "EHLO"), strings.HasPrefix(upper, "HELO"):
			// Multi-line 250 reply.
			fmt.Fprint(c, "250-mockSMTP\r\n")
			if !inTLS {
				fmt.Fprint(c, "250-STARTTLS\r\n")
			}
			// Only advertise AUTH post-TLS to mimic Office 365.
			if inTLS && cfg.advertiseAuth != "" {
				fmt.Fprintf(c, "250-AUTH %s\r\n", cfg.advertiseAuth)
			}
			fmt.Fprint(c, "250 HELP\r\n")

		case upper == "STARTTLS":
			fmt.Fprint(c, "220 Ready to start TLS\r\n")
			tlsConn := tls.Server(c, &tls.Config{
				Certificates:       []tls.Certificate{s.tlsCert},
				InsecureSkipVerify: true,
			})
			if err := tlsConn.Handshake(); err != nil {
				return
			}
			c = tlsConn
			r = bufio.NewReader(c)
			inTLS = true

		case strings.HasPrefix(upper, "AUTH PLAIN"):
			s.mu.Lock()
			s.selectedMech = "PLAIN"
			s.mu.Unlock()
			// Format: "AUTH PLAIN <base64>"
			parts := strings.SplitN(cmd, " ", 3)
			if len(parts) < 3 {
				fmt.Fprint(c, "501 Syntax error\r\n")
				continue
			}
			raw, err := base64.StdEncoding.DecodeString(parts[2])
			if err != nil {
				fmt.Fprint(c, "501 base64 decode\r\n")
				continue
			}
			// PLAIN format: \x00 identity \x00 username \x00 password
			segments := strings.Split(string(raw), "\x00")
			if len(segments) != 3 || segments[1] != cfg.wantUsername || segments[2] != cfg.wantPassword {
				fmt.Fprint(c, "535 Authentication credentials invalid\r\n")
				continue
			}
			s.mu.Lock()
			s.authSucceeded = true
			s.mu.Unlock()
			fmt.Fprint(c, "235 Authentication succeeded\r\n")

		case upper == "AUTH LOGIN":
			s.mu.Lock()
			s.selectedMech = "LOGIN"
			s.mu.Unlock()
			// Challenge: base64("Username:")
			fmt.Fprintf(c, "334 %s\r\n", base64.StdEncoding.EncodeToString([]byte("Username:")))
			userLine, err := r.ReadString('\n')
			if err != nil {
				return
			}
			user, err := base64.StdEncoding.DecodeString(strings.TrimSpace(userLine))
			if err != nil || string(user) != cfg.wantUsername {
				fmt.Fprint(c, "535 Authentication credentials invalid\r\n")
				continue
			}
			fmt.Fprintf(c, "334 %s\r\n", base64.StdEncoding.EncodeToString([]byte("Password:")))
			passLine, err := r.ReadString('\n')
			if err != nil {
				return
			}
			pass, err := base64.StdEncoding.DecodeString(strings.TrimSpace(passLine))
			if err != nil || string(pass) != cfg.wantPassword {
				fmt.Fprint(c, "535 Authentication credentials invalid\r\n")
				continue
			}
			s.mu.Lock()
			s.authSucceeded = true
			s.mu.Unlock()
			fmt.Fprint(c, "235 Authentication succeeded\r\n")

		case strings.HasPrefix(upper, "MAIL FROM"):
			s.mu.Lock()
			s.gotFrom = cmd
			s.mu.Unlock()
			fmt.Fprint(c, "250 OK\r\n")

		case strings.HasPrefix(upper, "RCPT TO"):
			s.mu.Lock()
			s.gotTo = append(s.gotTo, cmd)
			s.mu.Unlock()
			fmt.Fprint(c, "250 OK\r\n")

		case upper == "DATA":
			fmt.Fprint(c, "354 End data with <CR><LF>.<CR><LF>\r\n")
			var data strings.Builder
			for {
				dl, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if dl == ".\r\n" || dl == ".\n" {
					break
				}
				data.WriteString(dl)
			}
			s.mu.Lock()
			s.gotData = data.String()
			s.mu.Unlock()
			fmt.Fprint(c, "250 OK\r\n")

		case upper == "QUIT":
			fmt.Fprint(c, "221 Bye\r\n")
			return

		case upper == "":
			// keep reading

		default:
			// Unknown command — return 502 like a real server.
			fmt.Fprint(c, "502 Command not implemented\r\n")
		}

	}
}

// genSelfSignedCert creates an ephemeral RSA cert for the in-process
// SMTP server's STARTTLS. The client trusts it via InsecureSkipVerify
// since we control both sides of the conversation.
func genSelfSignedCert(t *testing.T) tls.Certificate {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa: %v", err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"127.0.0.1"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv}
}

func newMockImplicitTLSSMTP(t *testing.T, cfg mockSMTPConfig) *mockSMTP {
	t.Helper()
	cert := genSelfSignedCert(t)
	tlsConf := &tls.Config{Certificates: []tls.Certificate{cert}}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", tlsConf)
	if err != nil {
		t.Fatalf("tls listen: %v", err)
	}
	srv := &mockSMTP{t: t, ln: ln, tlsCert: cert}
	srv.wg.Add(1)
	go srv.acceptImplicitTLSLoop(cfg)
	return srv
}

func (s *mockSMTP) acceptImplicitTLSLoop(cfg mockSMTPConfig) {
	defer s.wg.Done()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.wg.Add(1)
		go func(c net.Conn) {
			defer s.wg.Done()
			defer c.Close() //nolint:errcheck
			s.handleImplicitTLS(c, cfg)
		}(conn)
	}
}

func (s *mockSMTP) handleImplicitTLS(c net.Conn, cfg mockSMTPConfig) {
	fmt.Fprint(c, "220 mockSMTP implicit TLS ready\r\n")
	r := bufio.NewReader(c)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.TrimSpace(line)
		upper := strings.ToUpper(cmd)
		switch {
		case strings.HasPrefix(upper, "EHLO"), strings.HasPrefix(upper, "HELO"):
			fmt.Fprint(c, "250-mockSMTP\r\n")
			if cfg.advertiseAuth != "" {
				fmt.Fprintf(c, "250-AUTH %s\r\n", cfg.advertiseAuth)
			}
			fmt.Fprint(c, "250 HELP\r\n")

		case strings.HasPrefix(upper, "AUTH PLAIN"):
			s.mu.Lock()
			s.selectedMech = "PLAIN"
			s.mu.Unlock()
			parts := strings.SplitN(cmd, " ", 3)
			if len(parts) < 3 {
				fmt.Fprint(c, "501 Syntax error\r\n")
				continue
			}
			raw, err := base64.StdEncoding.DecodeString(parts[2])
			if err != nil {
				fmt.Fprint(c, "501 base64 decode\r\n")
				continue
			}
			segments := strings.Split(string(raw), "\x00")
			if len(segments) != 3 || segments[1] != cfg.wantUsername || segments[2] != cfg.wantPassword {
				fmt.Fprint(c, "535 Authentication credentials invalid\r\n")
				continue
			}
			s.mu.Lock()
			s.authSucceeded = true
			s.mu.Unlock()
			fmt.Fprint(c, "235 Authentication succeeded\r\n")

		case strings.HasPrefix(upper, "MAIL FROM"):
			s.mu.Lock()
			s.gotFrom = cmd
			s.mu.Unlock()
			fmt.Fprint(c, "250 OK\r\n")

		case strings.HasPrefix(upper, "RCPT TO"):
			s.mu.Lock()
			s.gotTo = append(s.gotTo, cmd)
			s.mu.Unlock()
			fmt.Fprint(c, "250 OK\r\n")

		case upper == "DATA":
			fmt.Fprint(c, "354 End data with <CR><LF>.<CR><LF>\r\n")
			var data strings.Builder
			for {
				dl, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if dl == ".\r\n" || dl == ".\n" {
					break
				}
				data.WriteString(dl)
			}
			s.mu.Lock()
			s.gotData = data.String()
			s.mu.Unlock()
			fmt.Fprint(c, "250 OK\r\n")

		case upper == "QUIT":
			fmt.Fprint(c, "221 Bye\r\n")
			return

		default:
			fmt.Fprint(c, "502 Command not implemented\r\n")
		}
	}
}

func TestSendOnce_ImplicitTLS465(t *testing.T) {
	srv := newMockImplicitTLSSMTP(t, mockSMTPConfig{
		advertiseAuth: "PLAIN",
		wantUsername:  "bob",
		wantPassword:  "secret",
	})
	defer srv.Close()

	svc := &EmailService{
		host:      "127.0.0.1",
		port:      "465",
		username:  "bob",
		password:  "secret",
		from:      "system@keygate.test",
		enabled:   true,
		logger:    slog.Default(),
		tlsConfig: &tls.Config{InsecureSkipVerify: true, ServerName: "127.0.0.1"},
	}

	addr := fmt.Sprintf("127.0.0.1:%d", srv.Port())
	msg := []byte("Subject: test 465\r\n\r\nhello over implicit tls")
	cfg := svc.getConfig()
	cfg.port = "465"
	if err := svc.sendOnceWithConfig(cfg, addr, "recipient@test.com", msg); err != nil {
		t.Fatalf("sendOnceWithConfig failed on 465: %v", err)
	}

	if !srv.AuthSucceeded() {
		t.Errorf("expected AUTH to succeed over implicit TLS")
	}
}

func TestEmailService_DynamicConfig(t *testing.T) {
	// 1. Without store or env: not configured
	svc := NewEmailService("", "", "", "", "", slog.Default(), nil)
	if svc.IsConfigured() {
		t.Errorf("expected IsConfigured() to be false when nothing is set")
	}

	// 2. Struct env settings only
	svcEnv := NewEmailService("smtp.example.com", "587", "user", "pass", "noreply@example.com", slog.Default(), nil)
	if !svcEnv.IsConfigured() {
		t.Errorf("expected IsConfigured() to be true with env values")
	}
	cfg := svcEnv.getConfig()
	if cfg.host != "smtp.example.com" || cfg.port != "587" {
		t.Errorf("unexpected cfg: %+v", cfg)
	}
}

