package service

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"strings"
	"time"

	"github.com/tabloy/keygate/internal/branding"
	"github.com/tabloy/keygate/internal/store"
)

// emailFooter returns the attribution footer appended to all outgoing emails.
func emailFooter() string { return branding.EmailFooter }

type EmailService struct {
	host     string
	port     string
	username string
	password string
	from     string
	enabled  bool
	logger   *slog.Logger
	store    *store.Store
	// tlsConfig overrides the default STARTTLS settings. Production
	// leaves this nil so sendOnce builds the standard verify-the-
	// chain tls.Config. Tests inject a config with
	// InsecureSkipVerify=true so they can wire up an ephemeral
	// self-signed cert without poking holes in production trust.
	tlsConfig *tls.Config
}

func (s *EmailService) IsConfigured() bool {
	ok, err := s.Configured()
	return err == nil && ok
}

// Configured resolves the effective config and reports whether a
// provider is fully set up, keeping a transient config read error
// distinct from a clean "not configured". Callers that would otherwise
// fall back to an insecure path on false — printing an OTP code to the
// log — must use this and fail closed on err, so a DB blip or a decrypt
// failure never leaks a login code to the log.
func (s *EmailService) Configured() (bool, error) {
	cfg, err := s.resolve(context.Background())
	if err != nil {
		return false, err
	}
	return cfg.configured(), nil
}

// Host and From are shown read-only in the dashboard.
func (s *EmailService) Host() string {
	cfg, _ := s.resolve(context.Background())
	return cfg.host
}
func (s *EmailService) From() string {
	cfg, _ := s.resolve(context.Background())
	return cfg.from
}

// EmailStatus is what the settings page shows about email delivery.
type EmailStatus struct {
	Configured bool   `json:"configured"`
	Provider   string `json:"provider"`
	Source     string `json:"source"` // "db" | "env"
	Host       string `json:"host"`   // SMTP only; empty for HTTP providers
	From       string `json:"from"`
}

// Status resolves the effective config once and reports it. The
// credentials themselves are never included.
func (s *EmailService) Status() EmailStatus {
	c, err := s.resolve(context.Background())
	if err != nil {
		// Display only: a read error is not "configured".
		return EmailStatus{Provider: "smtp", Source: "env"}
	}
	return EmailStatus{
		Configured: c.configured(),
		Provider:   c.provider,
		Source:     c.source,
		Host:       c.host,
		From:       c.from,
	}
}

func NewEmailService(host, port, username, password, from string, logger *slog.Logger, s *store.Store) *EmailService {
	// Trim once, here, where the configuration arrives. These come
	// from the environment, and a trailing space in SMTP_PORT is an
	// ordinary typo that used to be read two different ways further
	// in: the TLS decision trimmed it, the dial address did not, so
	// " 465 " chose implicit TLS and then asked to connect to a port
	// of that name. Holding the clean value is what keeps every later
	// reader agreeing about what was configured; it also means a host
	// of only spaces counts as unset, as it should.
	host = strings.TrimSpace(host)
	port = strings.TrimSpace(port)
	from = strings.TrimSpace(from)
	enabled := host != "" && from != ""
	if !enabled {
		logger.Warn("email service disabled: SMTP not configured")
	}
	return &EmailService{
		host: host, port: port, username: username,
		password: password, from: from, enabled: enabled, logger: logger,
		store: s,
	}
}

// getTemplate returns the custom template from DB settings if it exists, otherwise the default.
func (s *EmailService) getTemplate(key, defaultTmpl string) string {
	if s.store == nil {
		return defaultTmpl
	}
	custom, err := s.store.GetSetting(context.Background(), "email_template_"+key)
	if err != nil || custom == "" {
		return defaultTmpl
	}
	return custom
}

// DefaultTemplates returns all default email templates keyed by their setting suffix.
func DefaultTemplates() map[string]string {
	return map[string]string{
		"license_created":   tmplLicenseCreated,
		"license_expiring":  tmplLicenseExpiring,
		"updates_ending":    tmplUpdatesEnding,
		"license_expired":   tmplLicenseExpired,
		"trial_expired":     tmplTrialExpired,
		"license_suspended": tmplLicenseSuspended,
		"quota_warning":     tmplQuotaWarning,
		"seat_invite":       tmplSeatInvite,
		"admin_invite":      tmplAdminInvite,
		"payment_failed":    tmplPaymentFailed,
	}
}

func (s *EmailService) Send(to, subject, htmlBody string) error {
	cfg, err := s.resolve(context.Background())
	if err != nil {
		// A config read error is a real failure, not "skip": returning
		// nil would silently drop the mail (login codes, invites).
		return fmt.Errorf("email config: %w", err)
	}
	if !cfg.configured() {
		// Best-effort: with no email configured there is nothing to
		// send with, and this is a supported mode (OTP codes go to the
		// log). Direct callers treat this as "skipped", not failure.
		// The QUEUE must not go through here — a "skipped" would look
		// like success and mark the mail sent; it uses sendResolved
		// with a config it has already checked.
		s.logger.Info("email skipped (not configured)", "to", to, "subject", subject)
		return nil
	}
	return s.sendResolved(cfg, to, subject, htmlBody)
}

// sendResolved delivers one message with a config the caller has
// already resolved and checked. It always reports failure as an error,
// so a caller that has claimed a queue row never mistakes "could not
// send" for "sent".
func (s *EmailService) sendResolved(cfg resolvedConfig, to, subject, htmlBody string) error {
	// Append attribution footer (AGPL v3 Section 7b — see NOTICE)
	if !strings.Contains(htmlBody, branding.Domain) {
		htmlBody = strings.Replace(htmlBody, "</body>", emailFooter()+"</body>", 1)
	}

	err := s.deliver(cfg, to, subject, htmlBody)
	if err != nil {
		// Only a 429/5xx/network hiccup is worth an immediate retry.
		// A terminal bounce or a credential/config error will not be
		// fixed by waiting 3 seconds, so return at once and let the
		// caller (the queue) decide whether to keep the mail.
		if !isRetryable(err) {
			s.logger.Error("email send failed", "to", to, "error", err, "terminal", isPermanent(err))
			return fmt.Errorf("email send: %w", err)
		}
		s.logger.Warn("email send failed, retrying", "to", to, "error", err)
		time.Sleep(3 * time.Second)
		if err = s.deliver(cfg, to, subject, htmlBody); err != nil {
			s.logger.Error("email send failed after retry", "to", to, "error", err)
			return fmt.Errorf("email send: %w", err)
		}
	}
	s.logger.Info("email sent", "to", to, "subject", subject, "provider", cfg.provider)
	return nil
}

// deliver dispatches one already-shaped message to whichever provider
// the resolved config names. This is the single point every provider
// goes through, so retry, logging and the attribution footer above are
// shared and provider-agnostic.
func (s *EmailService) deliver(cfg resolvedConfig, to, subject, htmlBody string) error {
	switch cfg.provider {
	case "cloudflare":
		ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
		defer cancel()
		return s.sendCloudflare(ctx, cfg, to, subject, htmlBody, htmlToText(htmlBody))
	default:
		msg := []byte(strings.Join([]string{
			"From: " + cfg.from,
			"To: " + to,
			"Subject: " + subject,
			"MIME-Version: 1.0",
			"Content-Type: text/html; charset=UTF-8",
			"",
			htmlBody,
		}, "\r\n"))
		return s.sendSMTP(cfg, cfg.addr(), to, msg)
	}
}

// sendOnce drives the SMTP conversation manually so we can negotiate
// AUTH against whatever mechanism the server actually advertises.
// Go's stdlib smtp.SendMail unconditionally drives the smtp.Auth you
// hand it, even if the server doesn't advertise that mechanism —
// which is exactly why smtp.PlainAuth fails against smtp.office365.com
// (Microsoft advertises LOGIN + XOAUTH2 only; PLAIN is rejected with
// "504 5.7.4 Unrecognized authentication type").
//
// Order of operations:
//  1. TCP dial.
//  2. EHLO (records server-advertised extensions).
//  3. STARTTLS if advertised — Office 365 / Gmail / most modern
//     submission endpoints require it on port 587.
//  4. EHLO again (Client.StartTLS does this internally; the
//     extension list refreshes — AUTH only appears post-TLS on
//     stricter servers).
//  5. Pick AUTH mechanism from the post-TLS list:
//     - PLAIN if advertised (standard, base64 single round-trip)
//     - else LOGIN if advertised (Microsoft, some older relays)
//     - else error out
//  6. MAIL/RCPT/DATA/QUIT.
//
// We intentionally do NOT speak SMTP over plaintext when credentials
// are present — if the server doesn't advertise STARTTLS, the auth
// step returns the "unencrypted connection" error from PlainAuth's
// own guard. That's the safe default; relay-style deployments that
// genuinely want plaintext auth can run their own postfix in front.
// smtpSessionTimeout bounds one SMTP session end to end (after the
// dial). Two attempts fit inside the reminder claim lease, and inside
// the queue claim lease (store.emailClaimLease), with margin.
const smtpSessionTimeout = 2 * time.Minute

// addr is what the dialer is handed.
func (s *EmailService) addr() string { return s.host + ":" + s.port }

// implicitTLS reports whether this server expects TLS from the first
// byte rather than a STARTTLS upgrade.
//
// The port is the signal because that is the only thing an operator
// configures and the only thing the convention is attached to: 465 is
// the submission port reserved for implicit TLS, everything else
// starts in the clear. Deciding by port rather than by a new setting
// means an operator who reads their provider's "use port 465" and
// types it in gets a working install without a second question.
func (s *EmailService) implicitTLS() bool {
	return s.port == "465"
}

func (s *EmailService) sendSMTP(cfg resolvedConfig, addr, to string, msg []byte) error {
	// The SMTP envelope sender (MAIL FROM, RFC 5321) must be a BARE
	// address — "noreply@x.com", never "Keygate <noreply@x.com>".
	// The display-name form is only legal in the RFC 5322 "From:"
	// header (which Send() builds separately). Strict MTAs like
	// Postmark reject a display-name envelope with
	// "501 Bad sender address syntax". Parse once here so operators
	// can keep configuring the friendly form in SMTP_FROM.
	envelopeFrom, err := parseEnvelopeAddress(cfg.from)
	if err != nil {
		return fmt.Errorf("invalid SMTP_FROM %q: %w", cfg.from, err)
	}
	// CR/LF guard (SMTP injection) — same as net/smtp.SendMail.
	if err := validateSMTPLine(envelopeFrom); err != nil {
		return err
	}
	if err := validateSMTPLine(to); err != nil {
		return err
	}

	tlsCfg := s.tlsConfig
	if tlsCfg == nil {
		tlsCfg = &tls.Config{ServerName: cfg.host, MinVersion: tls.VersionTLS12}
	}

	// Two ways a submission port speaks TLS, and the port says which.
	//
	// 587 and 25 start in the clear and are upgraded with STARTTLS.
	// 465 is TLS from the first byte (RFC 8314 calls this implicit TLS
	// and prefers it): greeting a 465 server in the clear reads its
	// handshake as if it were text, and the session dies at the EHLO
	// with something that looks nothing like a TLS problem. Cloudflare
	// and Fastmail offer 465 only, so this is not an exotic case.
	var conn net.Conn
	if cfg.implicitTLS() {
		d := &net.Dialer{Timeout: 30 * time.Second}
		conn, err = tls.DialWithDialer(d, "tcp", addr, tlsCfg)
		if err != nil {
			return smtpErr("tls dial", err, false)
		}
	} else {
		conn, err = net.DialTimeout("tcp", addr, 30*time.Second)
		if err != nil {
			return smtpErr("dial", err, false)
		}
	}
	// A server that accepts the connection and then stalls must not
	// hold the sender indefinitely: reminder claims are leases, and a
	// send that outlives its lease would be repeated by another
	// replica. One attempt plus the retry stays well inside the lease.
	_ = conn.SetDeadline(time.Now().Add(smtpSessionTimeout))
	c, err := smtp.NewClient(conn, cfg.host)
	if err != nil {
		_ = conn.Close()
		return smtpErr("smtp client", err, false)
	}
	defer c.Close() //nolint:errcheck

	if err := c.Hello(localHostname()); err != nil {
		return smtpErr("ehlo", err, false)
	}

	// STARTTLS upgrade if the server advertises it. (Client.StartTLS
	// internally re-EHLOs so post-TLS extensions land in c.Extension.)
	// Already encrypted on an implicit-TLS port, where the server does
	// not advertise the extension at all.
	if ok, _ := c.Extension("STARTTLS"); ok {
		if err := c.StartTLS(tlsCfg); err != nil {
			return smtpErr("starttls", err, false)
		}
	}

	if cfg.username != "" {
		auth, perr := pickAuth(c, cfg.host, cfg.username, cfg.password)
		if perr != nil {
			return perr
		}
		if err := c.Auth(auth); err != nil {
			return smtpErr("auth", err, false)
		}
	}

	if err := c.Mail(envelopeFrom); err != nil {
		return smtpErr("mail from", err, false)
	}
	if err := c.Rcpt(to); err != nil {
		return smtpErr("rcpt to", err, true)
	}
	w, err := c.Data()
	if err != nil {
		return smtpErr("data", err, false)
	}
	if _, err := w.Write(msg); err != nil {
		_ = w.Close()
		return smtpErr("data write", err, false)
	}
	if err := w.Close(); err != nil {
		return smtpErr("data close", err, false)
	}
	// w.Close() returning nil means the server accepted end-of-DATA
	// (a 250): the message IS delivered. QUIT is only the polite
	// teardown, so a failure here (EOF, RST) must NOT be reported as a
	// send failure — doing so would re-queue an already-delivered mail
	// and, via MarkEmailDeferred, re-send it every 10 minutes forever.
	if err := c.Quit(); err != nil {
		s.logger.Warn("smtp quit failed after delivery (message already accepted)", "to", to, "error", err)
	}
	return nil
}

// smtpErr maps an SMTP-phase failure to the queue's retry policy so an
// immediate retry (sendResolved) and the queue's backoff both apply to
// SMTP the same way they already do to the HTTP provider:
//
//   - No SMTP reply at all (dial, TLS handshake, a dropped connection
//     mid-session): a transport hiccup, worth retrying → retryableError.
//   - A 4xx reply ("421 try again later", greylisting): transient by
//     definition → retryableError.
//   - A 5xx RCPT reply that names a permanently invalid RECIPIENT (by
//     its RFC 3463 enhanced status code): a terminal bounce, the message
//     can never be delivered → permanentError.
//   - Anything else — a 535 auth failure, and crucially a bare 5xx RCPT
//     rejection that is really a relay/policy/config error (530 auth
//     required, 550 relaying denied, 554 sender domain rejected): not the
//     message's fault and no immediate retry helps, but recoverable once
//     an operator fixes the config, so a plain error keeps it (and its
//     body, which holds the licence key) in the queue to retry later.
func smtpErr(phase string, err error, recipient bool) error {
	var proto *textproto.Error
	if errors.As(err, &proto) {
		switch {
		case proto.Code >= 400 && proto.Code < 500:
			return retryable("%s: %w", phase, err)
		case recipient && proto.Code >= 500 && rcptPermanent(proto.Msg):
			return permanent("%s: %w", phase, err)
		default:
			return fmt.Errorf("%s: %w", phase, err)
		}
	}
	// No structured reply → a transport/session failure: transient.
	return retryable("%s: %w", phase, err)
}

// rcptPermanent reports whether a 5xx RCPT rejection names a permanently
// invalid RECIPIENT — the only case where dropping the message (and its
// body) is right. It keys off the RFC 3463 enhanced status code, not the
// bare 5xx, because a 550/554 with no such code, or one in the policy
// (5.7.x) or sender (5.1.7/5.1.8) ranges, is a relay/auth/config
// rejection an operator can fix, and must stay in the queue. Absent a
// recognised recipient-invalid code we assume NOT permanent: such a mail
// falls through to a plain error, which processQueue keeps and retries
// via MarkEmailDeferred rather than ever wiping licence mail on an
// ambiguous 550. The trade-off is that a hard bounce from a server that
// sends no enhanced code is retried indefinitely instead of given up on;
// keeping the key-bearing body is deliberately preferred to reclaiming a
// dead address, and an operator can purge such a row from the queue.
func rcptPermanent(msg string) bool {
	switch enhancedStatusCode(msg) {
	case "5.1.1", // bad destination mailbox address
		"5.1.2",  // bad destination system address
		"5.1.3",  // bad destination mailbox address syntax
		"5.1.6",  // destination mailbox has moved, no forwarding address
		"5.1.10": // recipient address has a null MX
		return true
	default:
		return false
	}
}

// enhancedStatusCode pulls a leading RFC 3463 "class.subject.detail"
// token (e.g. "5.1.1") off an SMTP reply line, or "" if the line does
// not start with one. net/smtp puts the reply text after the basic code
// in textproto.Error.Msg, and a compliant server leads that text with
// the enhanced code.
func enhancedStatusCode(msg string) string {
	fields := strings.Fields(strings.TrimSpace(msg))
	if len(fields) == 0 {
		return ""
	}
	tok := fields[0]
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return ""
	}
	for _, p := range parts {
		if p == "" {
			return ""
		}
		for _, r := range p {
			if r < '0' || r > '9' {
				return ""
			}
		}
	}
	return tok
}

// pickAuth selects an SMTP AUTH mechanism based on what the server
// advertised in its post-TLS EHLO response. Preference order:
//
//  1. PLAIN — standard, single round-trip, supported by Gmail,
//     SendGrid, Mailgun, Postmark, SES, Postfix.
//  2. LOGIN — non-standard but widely supported; the ONLY mechanism
//     Office 365 / Outlook / Exchange Online accepts for basic auth.
//
// Returns an error if the server requires AUTH but advertises neither
// (typical for OAuth2-only endpoints — those need XOAUTH2 which is a
// separate authentication flow).
func pickAuth(c *smtp.Client, host, username, password string) (smtp.Auth, error) {
	// Client.Extension returns (ok, params): ok = is the extension
	// supported, params = the parameter string (for AUTH this is the
	// space-separated list of mechanisms the server accepts).
	ok, authExt := c.Extension("AUTH")
	if !ok {
		return nil, errors.New("smtp: server does not advertise AUTH (configure SMTP_USERNAME='' for anonymous relays)")
	}
	mechs := strings.ToUpper(authExt)
	switch {
	case strings.Contains(mechs, "PLAIN"):
		return smtp.PlainAuth("", username, password, host), nil
	case strings.Contains(mechs, "LOGIN"):
		return &loginAuth{username: username, password: password, host: host}, nil
	default:
		return nil, fmt.Errorf("smtp: no supported AUTH mechanism (server advertised: %q)", authExt)
	}
}

// loginAuth implements RFC-less SMTP AUTH LOGIN. Microsoft 365 /
// Outlook.com / Exchange Online reject AUTH PLAIN with "504 5.7.4
// Unrecognized authentication type" so we need this fallback.
//
// LOGIN is base64 challenge-response: server sends "Username:" then
// "Password:" (both base64-encoded over the wire, but Client.Auth
// decodes before passing to Next). Some servers omit the trailing
// colon, send lowercase, or use a different word — match leniently.
type loginAuth struct{ username, password, host string }

func (a *loginAuth) Start(server *smtp.ServerInfo) (string, []byte, error) {
	// Only allow LOGIN over TLS. The credentials go on the wire in
	// (base64 of) plaintext — anything else is a credential leak.
	if !server.TLS {
		return "", nil, errors.New("smtp: refusing LOGIN auth on unencrypted connection")
	}
	if server.Name != a.host {
		return "", nil, errors.New("smtp: wrong host name")
	}
	return "LOGIN", nil, nil
}

func (a *loginAuth) Next(fromServer []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}
	// Normalise the prompt: outer trim, drop trailing colon, inner
	// trim (so " Username : " → "username"), lower-case. Tolerant
	// of the variants seen across Microsoft / Postfix / Exim.
	prompt := strings.ToLower(strings.TrimSpace(
		strings.TrimRight(strings.TrimSpace(string(fromServer)), ":"),
	))
	switch prompt {
	case "username", "user name":
		return []byte(a.username), nil
	case "password":
		return []byte(a.password), nil
	default:
		return nil, fmt.Errorf("smtp: unexpected LOGIN challenge: %q", fromServer)
	}
}

// parseEnvelopeAddress extracts the bare RFC 5321 address used for
// the SMTP MAIL FROM command. Accepts both forms operators commonly
// put in SMTP_FROM:
//
//	"noreply@example.com"               → noreply@example.com
//	"Keygate <noreply@example.com>"     → noreply@example.com
//	"\"Keygate Billing\" <a@b.com>"     → a@b.com
//
// The display-name form is kept verbatim in the message's "From:"
// header (built in Send) so recipients still see "Keygate"; only the
// envelope is stripped to the bare address.
func parseEnvelopeAddress(from string) (string, error) {
	from = strings.TrimSpace(from)
	if from == "" {
		return "", errors.New("empty address")
	}
	a, err := mail.ParseAddress(from)
	if err != nil {
		// Fall back: maybe it's already a bare addr-spec that
		// ParseAddress is being strict about (rare). Validate the
		// minimal shape before giving up.
		if strings.Count(from, "@") == 1 && !strings.ContainsAny(from, "<> ") {
			return from, nil
		}
		return "", err
	}
	return a.Address, nil
}

// validateSMTPLine rejects strings containing CR or LF so callers
// can't smuggle additional SMTP commands through a header value.
func validateSMTPLine(s string) error {
	if strings.ContainsAny(s, "\r\n") {
		return errors.New("smtp: line contains CR or LF")
	}
	return nil
}

// localHostname returns the EHLO argument. Some strict servers reject
// "localhost" or empty values; "[127.0.0.1]" is universally accepted.
func localHostname() string { return "[127.0.0.1]" }

// RenderLicenseCreated builds the "here is your key" mail. Subject and
// template live here rather than at the call sites so a resend is byte
// for byte the mail the customer was originally sent, including any
// template the operator has customised in the dashboard.
func (s *EmailService) RenderLicenseCreated(productName, planName, licenseKey string) (subject, body string) {
	body = renderTemplate(s.getTemplate("license_created", tmplLicenseCreated), map[string]string{
		"Product":    productName,
		"Plan":       planName,
		"LicenseKey": licenseKey,
	})
	return "Your license for " + productName, body
}

func (s *EmailService) SendLicenseCreated(to, productName, planName, licenseKey string) {
	subject, body := s.RenderLicenseCreated(productName, planName, licenseKey)
	go func() {
		if err := s.Send(to, subject, body); err != nil {
			s.logger.Error("email delivery failed", "to", to, "subject", subject, "error", err)
		}
	}()
}

func (s *EmailService) SendLicenseExpiring(to, productName, licenseKey, expiresAt string) {
	body := renderTemplate(s.getTemplate("license_expiring", tmplLicenseExpiring), map[string]string{
		"Product":    productName,
		"LicenseKey": licenseKey,
		"ExpiresAt":  expiresAt,
	})
	go func() {
		if err := s.Send(to, productName+" license expiring soon", body); err != nil {
			s.logger.Error("email delivery failed", "to", to, "subject", productName+" license expiring soon", "error", err)
		}
	}()
}

// RenderUpdatesEnding builds the 14-day notice that a perpetual
// license's maintenance period ends; the license itself keeps working
// past that date. The caller queues it rather than sending it: the
// reminder job runs inside the hourly expiry loop, serially over
// every due license, and an SMTP server that accepts connections and
// then stalls would hold the whole loop — payment reminders, renewal
// reminders, cleanup — behind it. The queue is durable and retries
// with backoff, so queuing is also what makes the reminder survive a
// crash.
func (s *EmailService) RenderUpdatesEnding(productName, licenseKey, updatesUntil string) (subject, body string) {
	body = renderTemplate(s.getTemplate("updates_ending", tmplUpdatesEnding), map[string]string{
		"Product":      productName,
		"LicenseKey":   licenseKey,
		"UpdatesUntil": updatesUntil,
	})
	return productName + " updates ending soon", body
}

func (s *EmailService) SendQuotaWarning(to, productName, feature string, used, limit int64, pct int) {
	body := renderTemplate(s.getTemplate("quota_warning", tmplQuotaWarning), map[string]any{
		"Product": productName,
		"Feature": feature,
		"Used":    used,
		"Limit":   limit,
		"Pct":     pct,
	})
	subject := fmt.Sprintf("%s: %s quota at %d%%", productName, feature, pct)
	go func() {
		if err := s.Send(to, subject, body); err != nil {
			s.logger.Error("email delivery failed", "to", to, "subject", subject, "error", err)
		}
	}()
}

// SendSeatInvite delivers the claim link. acceptURL is the absolute
// URL pointing at the portal accept-invite page; if the template
// uses {{InviteURL}} the link renders inline, otherwise we append a
// plain "Accept the invitation: <URL>" block so even a template
// admin who forgot to add the placeholder still ships a working
// link.
func (s *EmailService) SendSeatInvite(to, productName, inviterName, acceptURL string) {
	tmpl := s.getTemplate("seat_invite", tmplSeatInvite)
	body := renderTemplate(tmpl, map[string]string{
		"Product":   productName,
		"Inviter":   inviterName,
		"InviteURL": acceptURL,
	})
	// Defensive fallback: if the rendered body doesn't already
	// contain the claim URL (custom template missed the placeholder),
	// append it so the recipient still has a way in.
	if acceptURL != "" && !strings.Contains(body, acceptURL) {
		body += `<p style="margin-top:24px;font-size:13px;color:#555;">Accept the invitation: <a href="` + acceptURL + `">` + acceptURL + `</a></p>`
	}
	go func() {
		if err := s.Send(to, "You've been invited to "+productName, body); err != nil {
			s.logger.Error("email delivery failed", "to", to, "subject", "You've been invited to "+productName, "error", err)
		}
	}()
}

func (s *EmailService) SendLicenseExpired(to, productName string) {
	body := renderTemplate(s.getTemplate("license_expired", tmplLicenseExpired), map[string]string{
		"Product": productName,
	})
	go func() {
		if err := s.Send(to, productName+" license expired", body); err != nil {
			s.logger.Error("email delivery failed", "to", to, "subject", productName+" license expired", "error", err)
		}
	}()
}

func (s *EmailService) SendTrialExpired(to, productName string) {
	body := renderTemplate(s.getTemplate("trial_expired", tmplTrialExpired), map[string]string{
		"Product": productName,
	})
	go func() {
		if err := s.Send(to, productName+" trial has ended", body); err != nil {
			s.logger.Error("email delivery failed", "to", to, "subject", productName+" trial has ended", "error", err)
		}
	}()
}

func (s *EmailService) SendLicenseSuspended(to, productName, reason string) {
	body := renderTemplate(s.getTemplate("license_suspended", tmplLicenseSuspended), map[string]string{
		"Product": productName,
		"Reason":  reason,
	})
	go func() {
		if err := s.Send(to, productName+" license suspended", body); err != nil {
			s.logger.Error("email delivery failed", "to", to, "subject", productName+" license suspended", "error", err)
		}
	}()
}

func (s *EmailService) SendSubscriptionCanceled(to, productName string, immediate bool) {
	var tmpl string
	if immediate {
		tmpl = `<!DOCTYPE html>
<html><body style="font-family: -apple-system, sans-serif; max-width: 600px; margin: 0 auto; padding: 20px;">
<h2>Subscription Canceled</h2>
<p>Your <strong>` + productName + `</strong> subscription has been canceled immediately.</p>
<p>Your access has ended. Thank you for being a customer.</p>
</body></html>`
	} else {
		tmpl = `<!DOCTYPE html>
<html><body style="font-family: -apple-system, sans-serif; max-width: 600px; margin: 0 auto; padding: 20px;">
<h2>Subscription Will Be Canceled</h2>
<p>Your <strong>` + productName + `</strong> subscription will be canceled at the end of the current billing period.</p>
<p>You can continue using the service until then.</p>
</body></html>`
	}
	go func() {
		if err := s.Send(to, productName+" subscription canceled", tmpl); err != nil {
			s.logger.Error("email delivery failed", "to", to, "error", err)
		}
	}()
}

func (s *EmailService) SendPaymentFailed(to, productName string) {
	body := renderTemplate(s.getTemplate("payment_failed", tmplPaymentFailed), map[string]string{
		"Product": productName,
	})
	go func() {
		if err := s.Send(to, productName+" payment failed", body); err != nil {
			s.logger.Error("email delivery failed", "to", to, "subject", productName+" payment failed", "error", err)
		}
	}()
}

func (s *EmailService) SendDunningSecond(to, productName string) {
	body := `<!DOCTYPE html>
<html><body style="font-family: -apple-system, sans-serif; max-width: 600px; margin: 0 auto; padding: 20px;">
<h2 style="color: #d97706;">Payment Still Outstanding</h2>
<p>We've been unable to process your payment for <strong>` + productName + `</strong> for over a week.</p>
<p>Please update your payment method to avoid losing access.</p>
</body></html>`
	go func() {
		if err := s.Send(to, productName+" — payment still outstanding", body); err != nil {
			s.logger.Error("email delivery failed", "to", to, "error", err)
		}
	}()
}

func (s *EmailService) SendDunningFinal(to, productName string) {
	body := `<!DOCTYPE html>
<html><body style="font-family: -apple-system, sans-serif; max-width: 600px; margin: 0 auto; padding: 20px;">
<h2 style="color: #dc2626;">Final Notice — Access Will Be Suspended</h2>
<p>Your <strong>` + productName + `</strong> payment has been overdue for 14 days.</p>
<p>Your access will be suspended soon if payment is not received.</p>
<p>Please update your payment method immediately.</p>
</body></html>`
	go func() {
		if err := s.Send(to, productName+" — final payment notice", body); err != nil {
			s.logger.Error("email delivery failed", "to", to, "error", err)
		}
	}()
}

// SendAdminInvite notifies a Keygate platform operator that they
// were added (or promoted) to the admin team. The "invite" is
// really a role grant — the recipient can log in via email-OTP
// immediately and the email's job is just to tell them that
// happened.
//
// Best-effort: a delivery failure does NOT roll back the role
// grant on the InviteTeamMember handler. The recipient can still
// learn out-of-band that they're admin.
func (s *EmailService) SendAdminInvite(to, siteName, inviterName, role, loginURL string) {
	body := renderTemplate(s.getTemplate("admin_invite", tmplAdminInvite), map[string]string{
		"SiteName": siteName,
		"Inviter":  inviterName,
		"Role":     role,
		"LoginURL": loginURL,
	})
	go func() {
		subj := "You've been added to " + siteName
		if err := s.Send(to, subj, body); err != nil {
			s.logger.Error("email delivery failed", "to", to, "subject", subj, "error", err)
		}
	}()
}

// SendPaymentRecovered closes the dunning loop: customer fixed the
// card mid-grace and the subscription is back to active. Without
// this, the last touch the user has from us is "payment failed",
// which makes a successful retry feel silent.
func (s *EmailService) SendPaymentRecovered(to, productName string) {
	body := `<!DOCTYPE html>
<html><body style="font-family: -apple-system, sans-serif; max-width: 600px; margin: 0 auto; padding: 20px;">
<h2 style="color: #059669;">Payment Recovered — Thanks!</h2>
<p>Your payment for <strong>` + productName + `</strong> went through. Your subscription is active again, and access is fully restored.</p>
<p>No further action required.</p>
</body></html>`
	go func() {
		if err := s.Send(to, productName+" — payment recovered", body); err != nil {
			s.logger.Error("email delivery failed", "to", to, "error", err)
		}
	}()
}

func (s *EmailService) SendWelcome(to, name string) {
	body := `<!DOCTYPE html>
<html><body style="font-family: -apple-system, sans-serif; max-width: 600px; margin: 0 auto; padding: 20px;">
<h2>Welcome to Keygate!</h2>
<p>Hi ` + name + `, your account has been created.</p>
<p>You can manage your licenses and subscriptions from your portal.</p>
</body></html>`
	go func() {
		if err := s.Send(to, "Welcome to Keygate", body); err != nil {
			s.logger.Error("email delivery failed", "to", to, "error", err)
		}
	}()
}

func (s *EmailService) SendOTPCode(to, code string) {
	body := `<!DOCTYPE html>
<html><body style="font-family: -apple-system, sans-serif; max-width: 600px; margin: 0 auto; padding: 20px;">
<h2>Your login code</h2>
<p>Enter this code to sign in:</p>
<div style="background: #f4f4f5; border-radius: 8px; padding: 16px; margin: 16px 0; font-family: monospace; font-size: 32px; text-align: center; letter-spacing: 8px; font-weight: bold;">` + code + `</div>
<p style="color: #666; font-size: 14px;">This code expires in 10 minutes. If you didn't request this, ignore this email.</p>
</body></html>`
	go func() {
		if err := s.Send(to, "Your login code", body); err != nil {
			s.logger.Error("OTP email delivery failed", "to", to, "error", err)
		}
	}()
}

func (s *EmailService) SendPlanChanged(to, productName, oldPlan, newPlan string) {
	body := `<!DOCTYPE html>
<html><body style="font-family: -apple-system, sans-serif; max-width: 600px; margin: 0 auto; padding: 20px;">
<h2>Plan Changed</h2>
<p>Your <strong>` + productName + `</strong> plan has been changed from <strong>` + oldPlan + `</strong> to <strong>` + newPlan + `</strong>.</p>
</body></html>`
	go func() {
		if err := s.Send(to, productName+" plan changed", body); err != nil {
			s.logger.Error("email delivery failed", "to", to, "error", err)
		}
	}()
}

func (s *EmailService) SendRenewalReminder(to, productName, renewalDate string) {
	body := `<!DOCTYPE html>
<html><body style="font-family: -apple-system, sans-serif; max-width: 600px; margin: 0 auto; padding: 20px;">
<h2>Renewal Reminder</h2>
<p>Your <strong>` + productName + `</strong> subscription will renew on <strong>` + renewalDate + `</strong>.</p>
<p>No action is needed if you'd like to continue.</p>
</body></html>`
	go func() {
		if err := s.Send(to, productName+" renewal coming up", body); err != nil {
			s.logger.Error("email delivery failed", "to", to, "error", err)
		}
	}()
}

func (s *EmailService) SendPaymentActionRequired(to, productName, invoiceURL string) {
	body := `<!DOCTYPE html>
<html><body style="font-family: -apple-system, sans-serif; max-width: 600px; margin: 0 auto; padding: 20px;">
<h2 style="color: #d97706;">Payment Authentication Required</h2>
<p>Your payment for <strong>` + productName + `</strong> requires additional authentication.</p>
<p><a href="` + invoiceURL + `" style="display:inline-block;background:#2563eb;color:white;padding:10px 24px;border-radius:6px;text-decoration:none;">Complete Payment</a></p>
<p style="color:#666;font-size:14px;">If you don't complete this step, your subscription may be interrupted.</p>
</body></html>`
	go func() {
		if err := s.Send(to, productName+" — payment authentication required", body); err != nil {
			s.logger.Error("email delivery failed", "to", to, "error", err)
		}
	}()
}

func (s *EmailService) SendTrialEnding(to, productName, trialEnd string) {
	body := `<!DOCTYPE html>
<html><body style="font-family: -apple-system, sans-serif; max-width: 600px; margin: 0 auto; padding: 20px;">
<h2>Your Trial is Ending Soon</h2>
<p>Your <strong>` + productName + `</strong> trial ends on <strong>` + trialEnd + `</strong>.</p>
<p>After the trial, your subscription will begin automatically. No action needed if you'd like to continue.</p>
<p style="color:#666;font-size:14px;">If you'd like to cancel, you can do so from your account portal before the trial ends.</p>
</body></html>`
	go func() {
		if err := s.Send(to, productName+" trial ending soon", body); err != nil {
			s.logger.Error("email delivery failed", "to", to, "error", err)
		}
	}()
}

// StartEmailQueueProcessor processes queued emails periodically.
func (s *EmailService) StartEmailQueueProcessor(ctx context.Context, db *store.Store) {
	// Process immediately on start
	s.processQueue(ctx, db)

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.processQueue(ctx, db)
		}
	}
}

// emailQueueBatch bounds one pass of the queue, so a backlog is worked
// through over several ticks instead of in one long run.
const emailQueueBatch = 20

func (s *EmailService) processQueue(ctx context.Context, db *store.Store) {
	// With no email configured there is nothing to send with, and
	// draining the queue anyway destroys what is in it: Send returns
	// nil for a provider it cannot reach, so every mail is claimed,
	// skipped, and then marked delivered. Running without email is a
	// supported mode — the sample env ships it empty and OTP codes go
	// to the log — so a licence key queued by Stripe fulfilment would
	// be silently consumed on exactly the installs least able to
	// notice. Left alone, the backlog goes out once email is set up.
	//
	// resolve(), not s.enabled: email may be configured in the DB even
	// when the boot-time env was empty, and the backlog must drain in
	// that case too.
	//
	// Resolve ONCE and reuse it for the whole batch. If it were
	// re-resolved per mail (as Send does), an admin who clears a
	// credential mid-batch would flip resolve() to "not configured",
	// Send would return nil, and the claimed mail would be marked sent
	// and have its body wiped without ever leaving. A captured config
	// either sends or fails with an error that keeps the row.
	cfg, err := s.resolve(ctx)
	if err != nil {
		s.logger.Error("email queue: could not resolve config; leaving backlog", "error", err)
		return
	}
	if !cfg.configured() {
		return
	}
	for range emailQueueBatch {
		if ctx.Err() != nil {
			return
		}
		// Claimed one at a time, immediately before sending it: the
		// lease has to outlive this send alone, not the whole batch.
		e, err := db.ClaimNextEmail(ctx)
		if err != nil || e == nil {
			return
		}
		if err := s.sendResolved(cfg, e.ToAddr, e.Subject, e.Body); err != nil {
			switch {
			case isPermanent(err):
				// Terminal bounce: retrying just re-delivers to a bad
				// address. Give up at once and drop the body.
				db.MarkEmailFailedPermanent(ctx, e.ID, e.ClaimToken, err.Error())
			case isRetryable(err):
				// A 429/5xx/network hiccup: back off and try again, up
				// to max_attempts.
				db.MarkEmailFailed(ctx, e.ID, e.ClaimToken, err.Error())
			default:
				// A config/credential error (bad token, wrong account
				// id, relay/auth rejection): not the message's fault and
				// no retry helps until an operator fixes it. Keep the
				// mail and its body without burning max_attempts, so a
				// token rotation can't silently discard licence mail.
				db.MarkEmailDeferred(ctx, e.ID, e.ClaimToken, err.Error())
			}
		} else {
			db.MarkEmailSent(ctx, e.ID, e.ClaimToken)
		}
	}
}

func renderTemplate(tmplStr string, data any) string {
	t, err := template.New("email").Parse(tmplStr)
	if err != nil {
		return tmplStr
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return tmplStr
	}
	return buf.String()
}

const tmplLicenseCreated = `<!DOCTYPE html>
<html><body style="font-family: -apple-system, sans-serif; max-width: 600px; margin: 0 auto; padding: 20px;">
<h2 style="color: #111;">Your {{.Product}} License</h2>
<p>Your <strong>{{.Plan}}</strong> license is ready.</p>
<div style="background: #f4f4f5; border-radius: 8px; padding: 16px; margin: 16px 0; font-family: monospace; font-size: 18px; text-align: center; letter-spacing: 2px;">
{{.LicenseKey}}
</div>
<p style="color: #666; font-size: 14px;">Keep this key safe. You'll need it to activate your software.</p>
</body></html>`

const tmplLicenseExpiring = `<!DOCTYPE html>
<html><body style="font-family: -apple-system, sans-serif; max-width: 600px; margin: 0 auto; padding: 20px;">
<h2 style="color: #111;">License Expiring Soon</h2>
<p>Your <strong>{{.Product}}</strong> license expires on <strong>{{.ExpiresAt}}</strong>.</p>
<p>License key: <code>{{.LicenseKey}}</code></p>
<p>Please renew to avoid service interruption.</p>
</body></html>`

const tmplUpdatesEnding = `<!DOCTYPE html>
<html><body style="font-family: -apple-system, sans-serif; max-width: 600px; margin: 0 auto; padding: 20px;">
<h2 style="color: #111;">Updates Ending Soon</h2>
<p>Your <strong>{{.Product}}</strong> license includes updates until <strong>{{.UpdatesUntil}}</strong>.</p>
<p>License key: <code>{{.LicenseKey}}</code></p>
<p>The software keeps working after that date; versions released later need a renewed update period. You can renew from your account portal.</p>
</body></html>`

const tmplQuotaWarning = `<!DOCTYPE html>
<html><body style="font-family: -apple-system, sans-serif; max-width: 600px; margin: 0 auto; padding: 20px;">
<h2 style="color: #d97706;">Quota Warning: {{.Feature}}</h2>
<p>Your <strong>{{.Product}}</strong> {{.Feature}} usage is at <strong>{{.Pct}}%</strong>.</p>
<p>Used: {{.Used}} / {{.Limit}}</p>
<p>Consider upgrading your plan to avoid interruptions.</p>
</body></html>`

const tmplSeatInvite = `<!DOCTYPE html>
<html><body style="font-family: -apple-system, sans-serif; max-width: 600px; margin: 0 auto; padding: 20px;">
<h2 style="color: #111;">You've Been Invited</h2>
<p><strong>{{.Inviter}}</strong> has invited you to join <strong>{{.Product}}</strong>.</p>
<p style="margin: 24px 0;">
  <a href="{{.InviteURL}}" style="display: inline-block; padding: 10px 20px; background: #2563eb; color: white; text-decoration: none; border-radius: 6px;">Accept the invitation</a>
</p>
<p style="font-size: 12px; color: #666;">Or paste this link into your browser: <a href="{{.InviteURL}}">{{.InviteURL}}</a></p>
<p style="font-size: 12px; color: #999;">This link expires in 7 days.</p>
</body></html>`

const tmplAdminInvite = `<!DOCTYPE html>
<html><body style="font-family: -apple-system, sans-serif; max-width: 600px; margin: 0 auto; padding: 20px;">
<h2 style="color: #111;">You've been added to {{.SiteName}}</h2>
<p><strong>{{.Inviter}}</strong> added you as a <strong>{{.Role}}</strong> on the <strong>{{.SiteName}}</strong> admin team.</p>
<p>Sign in with this email using the email-OTP login to access the admin panel:</p>
<p style="margin: 24px 0;">
  <a href="{{.LoginURL}}" style="display: inline-block; padding: 10px 20px; background: #2563eb; color: white; text-decoration: none; border-radius: 6px;">Sign in</a>
</p>
<p style="font-size: 12px; color: #666;">Or paste this link into your browser: <a href="{{.LoginURL}}">{{.LoginURL}}</a></p>
</body></html>`

const tmplLicenseSuspended = `<!DOCTYPE html>
<html><body style="font-family: -apple-system, sans-serif; max-width: 600px; margin: 0 auto; padding: 20px;">
<h2 style="color: #dc2626;">License Suspended</h2>
<p>Your <strong>{{.Product}}</strong> license has been suspended.</p>
{{if .Reason}}<p>Reason: {{.Reason}}</p>{{end}}
</body></html>`

const tmplPaymentFailed = `<!DOCTYPE html>
<html><body style="font-family: -apple-system, sans-serif; max-width: 600px; margin: 0 auto; padding: 20px;">
<h2 style="color: #d97706;">Payment Failed</h2>
<p>We couldn't process your payment for <strong>{{.Product}}</strong>.</p>
<p>Please update your payment method to avoid service interruption.</p>
</body></html>`

const tmplLicenseExpired = `<!DOCTYPE html>
<html><body style="font-family: -apple-system, sans-serif; max-width: 600px; margin: 0 auto; padding: 20px;">
<h2 style="color: #dc2626;">License Expired</h2>
<p>Your <strong>{{.Product}}</strong> license has expired.</p>
<p>Please renew your subscription to continue using the software.</p>
</body></html>`

const tmplTrialExpired = `<!DOCTYPE html>
<html><body style="font-family: -apple-system, sans-serif; max-width: 600px; margin: 0 auto; padding: 20px;">
<h2 style="color: #d97706;">Trial Period Ended</h2>
<p>Your <strong>{{.Product}}</strong> trial has ended.</p>
<p>Subscribe to a paid plan to continue using all features.</p>
</body></html>`
