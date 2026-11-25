package service

import (
	"bytes"
	"context"
	"crypto/tls"
	"database/sql"
	"errors"
	"fmt"
	"html"
	"html/template"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"strconv"
	"strings"
	texttemplate "text/template"
	"time"
	"unicode/utf8"

	"github.com/tabloy/keygate/internal/branding"
	"github.com/tabloy/keygate/internal/store"
)

// emailFooter returns the attribution footer appended to all outgoing emails.
func emailFooter() string { return branding.EmailFooter }

// withAttribution makes sure an outgoing body carries the attribution
// footer (AGPL v3 Section 7(b) — see NOTICE). It looks for the footer
// itself, not merely the domain: a body that mentions the domain — a
// product download link, a custom template — must still get the footer.
// A copy the body already carries is taken out first, so the one that
// shows is always the one placed here: a custom template cannot satisfy
// the check with a copy it hides. That also keeps it idempotent. The
// footer goes before the last </body> (any letter case); a body without
// one gets it appended at the end.
func withAttribution(body string) string {
	footer := emailFooter()
	body = strings.ReplaceAll(body, footer, "")
	if i := lastIndexASCIIFold(body, "</body>"); i >= 0 {
		return body[:i] + footer + body[i:]
	}
	return body + footer
}

// lastIndexASCIIFold is strings.LastIndex ignoring ASCII letter case.
// Byte-wise on purpose: strings.ToLower can change the byte length of
// some non-ASCII text, which would shift the index it reports.
func lastIndexASCIIFold(s, sub string) int {
	lower := func(b byte) byte {
		if 'A' <= b && b <= 'Z' {
			return b + 'a' - 'A'
		}
		return b
	}
	for i := len(s) - len(sub); i >= 0; i-- {
		match := true
		for j := 0; j < len(sub); j++ {
			if lower(s[i+j]) != lower(sub[j]) {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

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
	// baseURL is the install's public address (BASE_URL), used to link
	// customers to their portal from templates ({{.PortalURL}}).
	baseURL string
}

// SetBaseURL records the install's public address for portal links.
func (s *EmailService) SetBaseURL(u string) { s.baseURL = strings.TrimRight(strings.TrimSpace(u), "/") }

// templateData adds what every customer template may use: the site's name
// ({{.SiteName}}) and the customer portal link ({{.PortalURL}}). A value
// the caller set wins.
func (s *EmailService) templateData(data map[string]any) map[string]any {
	if _, ok := data["SiteName"]; !ok {
		name := ""
		if s.store != nil {
			name, _ = s.store.GetSetting(context.Background(), "site_name")
		}
		data["SiteName"] = name
	}
	if _, ok := data["PortalURL"]; !ok {
		portal := ""
		if s.baseURL != "" {
			portal = s.baseURL + "/portal"
		}
		data["PortalURL"] = portal
	}
	return data
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

// emailTemplate is one email Keygate sends: the setting suffix it is
// stored under, the group the dashboard lists it in, its default body
// and subject, and the variables the sender passes it. Bodies use
// html/template; subjects use text/template (a subject is a header, not
// HTML) and are kept to one line.
type emailTemplate struct {
	Key     string
	Group   string // "licenses", "billing" or "account"
	Body    string
	Subject string
	// Vars are the email's variables, in the order the dashboard lists
	// them. Customer emails also get SiteName and PortalURL from
	// templateData; customer() adds them here.
	Vars []string
}

func customer(key, group, body, subject string, vars ...string) emailTemplate {
	return emailTemplate{key, group, body, subject, append(vars, "SiteName", "PortalURL")}
}

// staff is an email to a Keygate operator; it is not built with
// templateData, so it gets exactly the variables listed.
func staff(key, group, body, subject string, vars ...string) emailTemplate {
	return emailTemplate{key, group, body, subject, vars}
}

// emailTemplates lists every email Keygate sends. It is the one list
// the template editor, the settings an admin may write
// (email_template_<key> and email_subject_<key>) and the validation are
// built from. Before, only some emails were listed: the rest were built
// inline and could not be changed, so an install that restyled its
// emails still sent the plain defaults for the welcome mail, login codes
// and billing notices. A new email added here is editable at once; give
// it a label in the dashboard (email-templates.tsx) and sample values in
// templateSample.
var emailTemplates = []emailTemplate{
	customer("license_created", "licenses", tmplLicenseCreated, "Your license for {{.Product}}", "Product", "Plan", "LicenseKey", "DownloadURL"),
	customer("license_expiring", "licenses", tmplLicenseExpiring, "{{.Product}} {{if .IsTrial}}trial ending soon{{else}}license expiring soon{{end}}", "Product", "LicenseKey", "ExpiresAt", "DaysLeft", "IsTrial", "FallbackPlan"),
	customer("updates_ending", "licenses", tmplUpdatesEnding, "{{.Product}} updates ending soon", "Product", "LicenseKey", "UpdatesUntil"),
	customer("license_expired", "licenses", tmplLicenseExpired, "{{.Product}} license expired", "Product"),
	customer("trial_expired", "licenses", tmplTrialExpired, "{{.Product}} trial has ended", "Product", "FallbackPlan"),
	customer("license_suspended", "licenses", tmplLicenseSuspended, "{{.Product}} license suspended", "Product", "Reason"),
	customer("quota_warning", "licenses", tmplQuotaWarning, "{{.Product}}: {{.Feature}} quota at {{.Pct}}%", "Product", "Feature", "Used", "Limit", "Pct"),
	customer("seat_invite", "licenses", tmplSeatInvite, "You've been invited to {{.Product}}", "Product", "Inviter", "InviteURL", "DownloadURL"),
	customer("trial_ending", "billing", tmplTrialEnding, "{{.Product}} trial ending soon", "Product", "TrialEnd"),
	customer("renewal_reminder", "billing", tmplRenewalReminder, "{{.Product}} renewal coming up", "Product", "RenewalDate"),
	customer("plan_changed", "billing", tmplPlanChanged, "{{.Product}} plan changed", "Product", "OldPlan", "NewPlan"),
	customer("subscription_canceled", "billing", tmplSubscriptionCanceled, "{{.Product}} subscription canceled", "Product", "Immediate"),
	customer("payment_failed", "billing", tmplPaymentFailed, "{{.Product}} payment failed", "Product"),
	customer("dunning_second", "billing", tmplDunningSecond, "{{.Product}} — payment still outstanding", "Product"),
	customer("dunning_final", "billing", tmplDunningFinal, "{{.Product}} — final payment notice", "Product"),
	customer("payment_action_required", "billing", tmplPaymentActionRequired, "{{.Product}} — payment authentication required", "Product", "InvoiceURL"),
	customer("payment_recovered", "billing", tmplPaymentRecovered, "{{.Product}} — payment recovered", "Product"),
	customer("welcome", "account", tmplWelcome, "Welcome to {{.SiteName}}", "Name"),
	customer("login_code", "account", tmplLoginCode, "Your login code", "Code"),
	staff("admin_invite", "account", tmplAdminInvite, "You've been added to {{.SiteName}}", "SiteName", "Inviter", "Role", "LoginURL"),
}

// requiredTemplateVars are variables a custom body must use, because
// the email is useless without them: a login-code email without the
// code locks every customer out. Saving such a template is refused, and
// a body that still renders without the value falls back to the
// default when it is sent.
var requiredTemplateVars = map[string][]string{
	"login_code": {"Code"},
}

func findEmailTemplate(key string) (emailTemplate, bool) {
	for _, t := range emailTemplates {
		if t.Key == key {
			return t, true
		}
	}
	return emailTemplate{}, false
}

// TemplateKeys returns every email's key, in the dashboard's order.
func TemplateKeys() []string {
	keys := make([]string, len(emailTemplates))
	for i, t := range emailTemplates {
		keys[i] = t.Key
	}
	return keys
}

// DefaultTemplates returns all default email templates keyed by their setting suffix.
func DefaultTemplates() map[string]string {
	m := make(map[string]string, len(emailTemplates))
	for _, t := range emailTemplates {
		m[t.Key] = t.Body
	}
	return m
}

// TemplateInfo describes one editable email for the dashboard.
type TemplateInfo struct {
	Key, Group, Body, Subject string
	Variables                 []string
}

// Templates returns every email, in the dashboard's order.
func Templates() []TemplateInfo {
	out := make([]TemplateInfo, len(emailTemplates))
	for i, t := range emailTemplates {
		out[i] = TemplateInfo{t.Key, t.Group, t.Body, t.Subject, append([]string(nil), t.Vars...)}
	}
	return out
}

// TemplateVariables returns the variables each email may use, keyed like
// DefaultTemplates.
func TemplateVariables() map[string][]string {
	m := make(map[string][]string, len(emailTemplates))
	for _, t := range emailTemplates {
		m[t.Key] = t.Vars
	}
	return m
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
	// Attribution footer (AGPL v3 Section 7b — see NOTICE).
	htmlBody = withAttribution(htmlBody)

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
//
// The subject is built from settings and names (site, product), so a
// line break in one must not start a header of its own; and a header
// carries only ASCII, so the rest is encoded (RFC 2047).
func (s *EmailService) deliver(cfg resolvedConfig, to, subject, htmlBody string) error {
	subject = subjectLine.Replace(subject)
	switch cfg.provider {
	case "cloudflare":
		ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
		defer cancel()
		return s.sendCloudflare(ctx, cfg, to, subject, htmlBody, htmlToText(htmlBody))
	default:
		msg := []byte(strings.Join([]string{
			"From: " + cfg.from,
			"To: " + to,
			"Subject: " + mime.QEncoding.Encode("utf-8", subject),
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
	// Implicit-TLS connections are already encrypted, even if the server
	// still advertises STARTTLS.
	if !cfg.implicitTLS() {
		if ok, _ := c.Extension("STARTTLS"); ok {
			if err := c.StartTLS(tlsCfg); err != nil {
				return smtpErr("starttls", err, false)
			}
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

// subjectLine keeps a subject on one line.
var subjectLine = strings.NewReplacer("\r\n", " ", "\r", " ", "\n", " ")

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
// RenderLicenseCreated builds the license delivery email. downloadURL is
// the product's download page ({{.DownloadURL}}); empty leaves it out.
func (s *EmailService) RenderLicenseCreated(productName, planName, licenseKey, downloadURL string) (subject, body string) {
	return s.render("license_created", s.templateData(map[string]any{
		"Product":     productName,
		"Plan":        planName,
		"LicenseKey":  licenseKey,
		"DownloadURL": downloadURL,
	}))
}

func (s *EmailService) SendLicenseCreated(to, productName, planName, licenseKey, downloadURL string) {
	subject, body := s.RenderLicenseCreated(productName, planName, licenseKey, downloadURL)
	s.sendAsync(to, subject, body)
}

// RenderLicenseExpiring builds the reminder that a license or trial runs
// out in daysLeft days, on expiresAt. The reminder job queues it (see
// ExpiryChecker.remindExpiring) rather than sending it, for the reasons
// RenderUpdatesEnding gives.
//
// fallbackPlan is the plan a trial moves to when it ends (the trial
// plan's TrialFallbackPlanID), or "" when it simply expires: the
// reminder must not tell a customer they will lose access when they
// will in fact carry on, on that plan.
func (s *EmailService) RenderLicenseExpiring(productName, licenseKey, expiresAt string, daysLeft int, trial bool, fallbackPlan string) (subject, body string) {
	return s.render("license_expiring", s.templateData(map[string]any{
		"Product":      productName,
		"LicenseKey":   licenseKey,
		"ExpiresAt":    expiresAt,
		"DaysLeft":     daysLeft,
		"IsTrial":      trial,
		"FallbackPlan": fallbackPlan,
	}))
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
	return s.render("updates_ending", s.templateData(map[string]any{
		"Product":      productName,
		"LicenseKey":   licenseKey,
		"UpdatesUntil": updatesUntil,
	}))
}

// RenderQuotaWarning builds the notice that a metered feature has
// reached pct% of its limit.
func (s *EmailService) RenderQuotaWarning(productName, feature string, used, limit int64, pct int) (subject, body string) {
	return s.render("quota_warning", s.templateData(map[string]any{
		"Product": productName,
		"Feature": feature,
		"Used":    used,
		"Limit":   limit,
		"Pct":     pct,
	}))
}

func (s *EmailService) SendQuotaWarning(to, productName, feature string, used, limit int64, pct int) {
	if s.skipDisabled("quota_warning", to) {
		return
	}
	subject, body := s.RenderQuotaWarning(productName, feature, used, limit, pct)
	s.sendAsync(to, subject, body)
}

// RenderSeatInvite builds the seat claim email. acceptURL is the
// absolute URL pointing at the portal accept-invite page; if the
// template uses {{.InviteURL}} the link renders inline, otherwise we
// append a plain "Accept the invitation: <URL>" block so even a
// template admin who forgot to add the placeholder still ships a
// working link.
func (s *EmailService) RenderSeatInvite(productName, inviterName, acceptURL, downloadURL string) (subject, body string) {
	subject, body = s.render("seat_invite", s.templateData(map[string]any{
		"Product":     productName,
		"Inviter":     inviterName,
		"InviteURL":   acceptURL,
		"DownloadURL": downloadURL,
	}))
	// Defensive fallback: if the rendered body doesn't already
	// contain the claim URL (custom template missed the placeholder),
	// append it so the recipient still has a way in.
	if acceptURL != "" && !strings.Contains(body, acceptURL) {
		body += `<p style="margin-top:24px;font-size:13px;color:#555;">Accept the invitation: <a href="` + html.EscapeString(acceptURL) + `">` + html.EscapeString(acceptURL) + `</a></p>`
	}
	return subject, body
}

// SendSeatInvite delivers the claim link (see RenderSeatInvite).
func (s *EmailService) SendSeatInvite(to, productName, inviterName, acceptURL, downloadURL string) {
	subject, body := s.RenderSeatInvite(productName, inviterName, acceptURL, downloadURL)
	s.sendAsync(to, subject, body)
}

// RenderLicenseExpired builds the notice that a license has expired.
func (s *EmailService) RenderLicenseExpired(productName string) (subject, body string) {
	return s.render("license_expired", s.templateData(map[string]any{
		"Product": productName,
	}))
}

func (s *EmailService) SendLicenseExpired(to, productName string) {
	if s.skipDisabled("license_expired", to) {
		return
	}
	subject, body := s.RenderLicenseExpired(productName)
	s.sendAsync(to, subject, body)
}

// RenderTrialExpired tells a customer their trial has ended. fallbackPlan
// is the plan the licence moved to (the trial plan's
// TrialFallbackPlanID), or "" when it expired: one email for the end of
// a trial either way, so the customer reads what happened to it rather
// than a bare change of plan names, and the operator writes it once in
// the template editor.
func (s *EmailService) RenderTrialExpired(productName, fallbackPlan string) (subject, body string) {
	return s.render("trial_expired", s.templateData(map[string]any{
		"Product":      productName,
		"FallbackPlan": fallbackPlan,
	}))
}

// SendTrialExpired sends the end-of-trial email (see RenderTrialExpired).
func (s *EmailService) SendTrialExpired(to, productName, fallbackPlan string) {
	if s.skipDisabled("trial_expired", to) {
		return
	}
	subject, body := s.RenderTrialExpired(productName, fallbackPlan)
	s.sendAsync(to, subject, body)
}

// RenderLicenseSuspended builds the notice that a license was suspended.
func (s *EmailService) RenderLicenseSuspended(productName, reason string) (subject, body string) {
	return s.render("license_suspended", s.templateData(map[string]any{
		"Product": productName,
		"Reason":  reason,
	}))
}

func (s *EmailService) SendLicenseSuspended(to, productName, reason string) {
	if s.skipDisabled("license_suspended", to) {
		return
	}
	subject, body := s.RenderLicenseSuspended(productName, reason)
	s.sendAsync(to, subject, body)
}

// RenderSubscriptionCanceled builds the cancellation notice: canceled
// now (immediate) or at the end of the current billing period. One
// template covers both, branching on {{.Immediate}}.
func (s *EmailService) RenderSubscriptionCanceled(productName string, immediate bool) (subject, body string) {
	return s.render("subscription_canceled", s.templateData(map[string]any{
		"Product":   productName,
		"Immediate": immediate,
	}))
}

func (s *EmailService) SendSubscriptionCanceled(to, productName string, immediate bool) {
	if s.skipDisabled("subscription_canceled", to) {
		return
	}
	subject, body := s.RenderSubscriptionCanceled(productName, immediate)
	s.sendAsync(to, subject, body)
}

// RenderPaymentFailed builds the first step of the dunning ladder, a
// day into a past-due episode. Like the other dunning steps, the
// reminder job queues it rather than sending it: a mail sent from the
// loop and lost to an SMTP outage would leave the customer untold
// until the license expires, and the queue retries it.
func (s *EmailService) RenderPaymentFailed(productName string) (subject, body string) {
	return s.render("payment_failed", s.templateData(map[string]any{
		"Product": productName,
	}))
}

// RenderDunningSecond builds the dunning step sent a week into a
// past-due episode. The reminder job queues it.
func (s *EmailService) RenderDunningSecond(productName string) (subject, body string) {
	return s.render("dunning_second", s.templateData(map[string]any{
		"Product": productName,
	}))
}

// RenderDunningFinal builds the last dunning step, two weeks into a
// past-due episode. The reminder job queues it.
func (s *EmailService) RenderDunningFinal(productName string) (subject, body string) {
	return s.render("dunning_final", s.templateData(map[string]any{
		"Product": productName,
	}))
}

// RenderAdminInvite builds the admin invite. The login link comes from
// BASE_URL, like every other link Keygate mails: the request that made
// the invite says nothing reliable about the public address, and behind
// a proxy it read as http://127.0.0.1:9000.
func (s *EmailService) RenderAdminInvite(siteName, inviterName, role string) (subject, body string) {
	return s.render("admin_invite", map[string]string{
		"SiteName": siteName,
		"Inviter":  inviterName,
		"Role":     role,
		"LoginURL": s.baseURL + "/login",
	})
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
func (s *EmailService) SendAdminInvite(to, siteName, inviterName, role string) {
	subject, body := s.RenderAdminInvite(siteName, inviterName, role)
	s.sendAsync(to, subject, body)
}

// RenderPaymentRecovered closes the dunning loop: customer fixed the
// card mid-grace and the subscription is back to active. Without
// this, the last touch the user has from us is "payment failed",
// which makes a successful retry feel silent.
func (s *EmailService) RenderPaymentRecovered(productName string) (subject, body string) {
	return s.render("payment_recovered", s.templateData(map[string]any{
		"Product": productName,
	}))
}

func (s *EmailService) SendPaymentRecovered(to, productName string) {
	if s.skipDisabled("payment_recovered", to) {
		return
	}
	subject, body := s.RenderPaymentRecovered(productName)
	s.sendAsync(to, subject, body)
}

// RenderWelcome builds the welcome email for a new account.
func (s *EmailService) RenderWelcome(name string) (subject, body string) {
	// Self-hosted installs set site_name; stock Keygate keeps the
	// project name so an empty setting still greets as Keygate.
	data := s.templateData(map[string]any{"Name": name})
	site, _ := data["SiteName"].(string)
	site = strings.TrimSpace(site)
	if site == "" {
		site = branding.Project
	}
	data["SiteName"] = site
	return s.render("welcome", data)
}

func (s *EmailService) SendWelcome(to, name string) {
	if s.skipDisabled("welcome", to) {
		return
	}
	subject, body := s.RenderWelcome(name)
	s.sendAsync(to, subject, body)
}

// RenderOTPCode builds the login-code email. A customised body that
// does not show the code as readable text would lock the customer out,
// so then the default is sent instead (saving such a template is refused
// too, see ValidateTemplate).
func (s *EmailService) RenderOTPCode(code string) (subject, body string) {
	data := s.templateData(map[string]any{"Code": code})
	subject, body = s.render("login_code", data)
	if !showsText(body, code) {
		s.logger.Error("custom login code template does not show the code; sending the default", "template", "login_code")
		body = renderTemplate(tmplLoginCode, data)
	}
	return subject, body
}

// SendOTPCode renders and sends in the background: rendering reads
// settings, and the handler must answer an allowed address as fast as a
// refused one (see OTPSend).
func (s *EmailService) SendOTPCode(to, code string) {
	go func() {
		subject, body := s.RenderOTPCode(code)
		if err := s.Send(to, subject, body); err != nil {
			s.logger.Error("OTP email delivery failed", "to", to, "error", err)
		}
	}()
}

// RenderPlanChanged builds the notice that a license moved between plans.
func (s *EmailService) RenderPlanChanged(productName, oldPlan, newPlan string) (subject, body string) {
	return s.render("plan_changed", s.templateData(map[string]any{
		"Product": productName,
		"OldPlan": oldPlan,
		"NewPlan": newPlan,
	}))
}

func (s *EmailService) SendPlanChanged(to, productName, oldPlan, newPlan string) {
	if s.skipDisabled("plan_changed", to) {
		return
	}
	subject, body := s.RenderPlanChanged(productName, oldPlan, newPlan)
	s.sendAsync(to, subject, body)
}

// RenderRenewalReminder builds the day-before notice that a Stripe
// subscription renews. The reminder job queues it.
func (s *EmailService) RenderRenewalReminder(productName, renewalDate string) (subject, body string) {
	return s.render("renewal_reminder", s.templateData(map[string]any{
		"Product":     productName,
		"RenewalDate": renewalDate,
	}))
}

// RenderPaymentActionRequired asks the customer to authenticate a
// payment (3-D Secure) on the hosted invoice page.
func (s *EmailService) RenderPaymentActionRequired(productName, invoiceURL string) (subject, body string) {
	return s.render("payment_action_required", s.templateData(map[string]any{
		"Product":    productName,
		"InvoiceURL": invoiceURL,
	}))
}

func (s *EmailService) SendPaymentActionRequired(to, productName, invoiceURL string) {
	if s.skipDisabled("payment_action_required", to) {
		return
	}
	subject, body := s.RenderPaymentActionRequired(productName, invoiceURL)
	s.sendAsync(to, subject, body)
}

// RenderTrialEnding builds Stripe's trial-will-end notice.
func (s *EmailService) RenderTrialEnding(productName, trialEnd string) (subject, body string) {
	return s.render("trial_ending", s.templateData(map[string]any{
		"Product":  productName,
		"TrialEnd": trialEnd,
	}))
}

func (s *EmailService) SendTrialEnding(to, productName, trialEnd string) {
	if s.skipDisabled("trial_ending", to) {
		return
	}
	subject, body := s.RenderTrialEnding(productName, trialEnd)
	s.sendAsync(to, subject, body)
}

// sendAsync sends one rendered email in the background, logging a
// failure: the callers are request handlers and webhooks that must not
// wait on the mail server.
func (s *EmailService) sendAsync(to, subject, body string) {
	go func() {
		if err := s.Send(to, subject, body); err != nil {
			s.logger.Error("email delivery failed", "to", to, "subject", subject, "error", err)
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
		outdated, err := db.IsOutdatedDunningEmail(ctx, e.ID)
		if err != nil {
			// Unknown whether the customer has paid. Nothing is lost by
			// waiting: the claim lapses and the mail comes round again.
			s.logger.Warn("email queue: could not check a dunning notice; leaving it for later", "email_id", e.ID, "error", err)
			continue
		}
		if outdated {
			db.MarkEmailFailedPermanent(ctx, e.ID, e.ClaimToken, "license is no longer in this past-due episode")
			continue
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

// renderEmail renders the admin's custom template for key, falling back to
// the built-in one when the custom template does not render. A template
// can parse yet fail when executed (html/template checks escaping
// contexts then), and mailing its raw source would send the customer
// "{{.LicenseKey}}" instead of their key.
func (s *EmailService) renderEmail(key, defaultTmpl string, data any) string {
	src := s.getTemplate(key, defaultTmpl)
	if src != defaultTmpl {
		out, err := executeTemplate(src, data)
		if err == nil {
			return out
		}
		s.logger.Error("custom email template failed; sending the default", "template", key, "error", err)
	}
	return renderTemplate(defaultTmpl, data)
}

func executeTemplate(src string, data any) (string, error) {
	t, err := template.New("email").Parse(src)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// templateSample has a value of the right type for every variable any
// template may use. Validation renders a candidate with the part of it
// that one email is given (sampleFor).
var templateSample = map[string]any{
	"Product": "Product", "Plan": "Pro", "LicenseKey": "KEY", "ExpiresAt": "2026-01-01",
	"UpdatesUntil": "2026-01-01", "Feature": "api_calls", "Used": int64(80), "Limit": int64(100),
	"Pct": 80, "Inviter": "a@example.com", "InviteURL": "https://example.com/i", "Reason": "reason",
	"DaysLeft": 3, "IsTrial": true, "FallbackPlan": "Free", "DownloadURL": "https://example.com/d", "SiteName": "Site",
	"PortalURL": "https://example.com/portal", "Role": "admin", "LoginURL": "https://example.com/login",
	"Name": "Alex", "Code": "482913", "OldPlan": "Basic", "NewPlan": "Pro", "Immediate": true,
	"InvoiceURL": "https://example.com/invoice", "TrialEnd": "2026-01-01", "RenewalDate": "2026-01-01",
}

// secretTemplateVars may appear in a body but never in a subject: a
// subject is logged when the mail is sent and kept in the email queue
// after the body is wiped (see MarkEmailSent), so a login code, license
// key or invite token there would outlive the mail.
var secretTemplateVars = map[string]bool{"Code": true, "LicenseKey": true, "InviteURL": true}

// maxSubjectTemplate bounds a custom subject template, in characters.
// maxSubjectRendered bounds what it renders to: a subject is one header
// line, and a long one (a printf padding trick, or many multi-byte
// characters once encoded) can pass SMTP's 998-byte line limit and get
// the mail refused. A longer render falls back to the default subject.
const (
	maxSubjectTemplate = 300
	maxSubjectRendered = 200
)

// render builds one email's subject and body from its templates: the
// admin's custom ones when set, the defaults otherwise.
func (s *EmailService) render(key string, data any) (subject, body string) {
	t, ok := findEmailTemplate(key)
	if !ok {
		// Every Render* passes a literal key from emailTemplates, and
		// the tests render each one, so this is a programming error.
		panic("unknown email template " + key)
	}
	return s.renderSubject(key, t.Subject, data), s.renderEmail(key, t.Body, data)
}

// renderSubject is renderEmail for the subject line: the custom subject
// (setting email_subject_<key>) when it is set and renders to a usable
// line, the default otherwise. Secret variables are taken out of the
// data first, so a subject stored past validation cannot carry them.
func (s *EmailService) renderSubject(key, defaultSubj string, data any) string {
	if s.store != nil {
		custom, err := s.store.GetSetting(context.Background(), "email_subject_"+key)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			s.logger.Error("could not read custom email subject; sending the default", "template", key, "error", err)
		}
		if err == nil && strings.TrimSpace(custom) != "" {
			out, rerr := executeSubject(custom, withoutSecrets(data))
			line := strings.TrimSpace(subjectLine.Replace(out))
			if rerr == nil && line != "" && utf8.RuneCountInString(line) <= maxSubjectRendered {
				return line
			}
			s.logger.Error("custom email subject failed, came out empty or too long; sending the default", "template", key, "error", rerr)
		}
	}
	out, err := executeSubject(defaultSubj, data)
	if err != nil {
		// The defaults are checked against every sender's data by the
		// tests; never mail the template source as a subject.
		s.logger.Error("default email subject failed to render", "template", key, "error", err)
		return branding.Project
	}
	return strings.TrimSpace(subjectLine.Replace(out))
}

// withoutSecrets returns data without secretTemplateVars.
func withoutSecrets(data any) any {
	switch d := data.(type) {
	case map[string]any:
		out := make(map[string]any, len(d))
		for k, v := range d {
			if !secretTemplateVars[k] {
				out[k] = v
			}
		}
		return out
	case map[string]string:
		out := make(map[string]string, len(d))
		for k, v := range d {
			if !secretTemplateVars[k] {
				out[k] = v
			}
		}
		return out
	}
	return data
}

// executeSubject renders a subject with text/template: a subject is a
// header, so "&" and "<" in a product name stay as they are rather than
// becoming HTML entities. A variable the email does not have is an
// error, not "<no value>" in the customer's inbox, so the caller falls
// back to the default subject.
func executeSubject(src string, data any) (string, error) {
	t, err := texttemplate.New("subject").Option("missingkey=error").Parse(src)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// sampleFor is templateSample cut down to the variables the email
// stored under key is given, so a template that names another email's
// variable is refused when it is saved instead of rendering blank when
// it is sent. flipped gives the other branch of every {{if}} over a
// bool, and empty optional strings, so a typo in an {{else}} branch the
// first sample never takes is caught too.
func sampleFor(key string, flipped bool) (map[string]any, error) {
	if _, ok := findEmailTemplate(key); !ok {
		return nil, fmt.Errorf("unknown email template %q", key)
	}
	sample := map[string]any{}
	for _, v := range TemplateVariables()[key] {
		val := templateSample[v]
		if flipped {
			switch x := val.(type) {
			case bool:
				val = !x
			case string:
				val = ""
			}
		}
		sample[v] = val
	}
	return sample, nil
}

// ValidateSubject reports whether a custom subject for the email stored
// under key is one line, at most maxSubjectTemplate characters, and
// renders using only that email's variables, spelt right and none of
// them secret.
func ValidateSubject(key, src string) error {
	if strings.ContainsAny(src, "\r\n") {
		return errors.New("a subject must be a single line")
	}
	if utf8.RuneCountInString(src) > maxSubjectTemplate {
		return fmt.Errorf("a subject template may be at most %d characters", maxSubjectTemplate)
	}
	for _, flipped := range []bool{false, true} {
		sample, err := sampleFor(key, flipped)
		if err != nil {
			return err
		}
		if _, err := executeSubject(src, withoutSecrets(sample)); err != nil {
			for v := range secretTemplateVars {
				if strings.Contains(src, "."+v) {
					return fmt.Errorf("{{.%s}} cannot be used in a subject: subjects are logged and kept after sending", v)
				}
			}
			return err
		}
	}
	return nil
}

// ValidateTemplate reports whether a custom body for the email stored
// under key parses and renders, using only that email's variables.
// Parsing alone is not enough: html/template rejects some templates
// only when executed (e.g. "{{if}} branches end in different
// contexts"), which would otherwise surface at send time. A misspelt
// variable is refused too: sending is lenient, so {{.LicenceKey}} would
// otherwise render as nothing and the customer would get a licence email
// without the key. It is rendered with both samples (see sampleFor).
//
// An email in requiredTemplateVars must show those values as text a
// reader can see. The check uses a value no template can contain
// literally, so typing a sample code into the template does not pass it.
func ValidateTemplate(key, src string) error {
	t, err := template.New("email").Option("missingkey=error").Parse(src)
	if err != nil {
		return err
	}
	for _, flipped := range []bool{false, true} {
		sample, err := sampleFor(key, flipped)
		if err != nil {
			return err
		}
		if err := t.Execute(io.Discard, sample); err != nil {
			return err
		}
	}
	required := requiredTemplateVars[key]
	if len(required) == 0 {
		return nil
	}
	sample, _ := sampleFor(key, false)
	for _, v := range required {
		sample[v] = "SENTINEL" + strings.ToUpper(strconv.FormatInt(time.Now().UnixNano(), 36))
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, sample); err != nil {
		return err
	}
	for _, v := range required {
		if !showsText(buf.String(), fmt.Sprint(sample[v])) {
			return fmt.Errorf("this email must show {{.%s}} as text the reader can see", v)
		}
	}
	return nil
}

// showsText reports whether value appears in the readable text of an
// HTML body, not only inside a tag or attribute (a link the reader
// cannot see, an image URL).
func showsText(body, value string) bool {
	return strings.Contains(htmlToText(body), value)
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

const tmplWelcome = `<!DOCTYPE html>
<html><body style="font-family: -apple-system, sans-serif; max-width: 600px; margin: 0 auto; padding: 20px;">
<h2>Welcome to {{.SiteName}}!</h2>
<p>Hi {{.Name}}, your account has been created.</p>
<p>You can manage your licenses and subscriptions from your portal.</p>
</body></html>`

const tmplLicenseCreated = `<!DOCTYPE html>
<html><body style="font-family: -apple-system, sans-serif; max-width: 600px; margin: 0 auto; padding: 20px;">
<h2 style="color: #111;">Your {{.Product}} License</h2>
<p>Your <strong>{{.Plan}}</strong> license is ready.</p>
<div style="background: #f4f4f5; border-radius: 8px; padding: 16px; margin: 16px 0; font-family: monospace; font-size: 18px; text-align: center; letter-spacing: 2px;">
{{.LicenseKey}}
</div>
<p style="color: #666; font-size: 14px;">Keep this key safe. You'll need it to activate your software.</p>
{{if .DownloadURL}}<p style="margin: 24px 0;">
  <a href="{{.DownloadURL}}" style="display: inline-block; padding: 10px 20px; background: #2563eb; color: white; text-decoration: none; border-radius: 6px;">Download {{.Product}}</a>
</p>{{end}}
</body></html>`

const tmplLicenseExpiring = `<!DOCTYPE html>
<html><body style="font-family: -apple-system, sans-serif; max-width: 600px; margin: 0 auto; padding: 20px;">
<h2 style="color: #111;">{{if .IsTrial}}Trial Ending Soon{{else}}License Expiring Soon{{end}}</h2>
<p>Your <strong>{{.Product}}</strong> {{if .IsTrial}}trial ends{{else}}license expires{{end}} on <strong>{{.ExpiresAt}}</strong> (days remaining: {{.DaysLeft}}).</p>
<p>License key: <code>{{.LicenseKey}}</code></p>
<p>{{if .IsTrial}}{{if .FallbackPlan}}After that your license moves to the <strong>{{.FallbackPlan}}</strong> plan, with the same key, within that plan's limits. To keep more than it offers, choose a plan before then.{{else}}Choose a plan before then to keep using it.{{end}}{{else}}Please renew to avoid service interruption.{{end}}</p>
{{if and .IsTrial .PortalURL}}<p><a href="{{.PortalURL}}">Open your account</a></p>{{end}}
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
{{if .DownloadURL}}<p style="font-size: 12px; color: #666;">Download {{.Product}}: <a href="{{.DownloadURL}}">{{.DownloadURL}}</a></p>{{end}}
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
<p>{{if .FallbackPlan}}Your license carries on with the <strong>{{.FallbackPlan}}</strong> plan, with the same key, and that plan's limits now apply. Choose a paid plan from your account to get more.{{else}}Subscribe to a paid plan to continue using all features.{{end}}</p>
{{if .PortalURL}}<p><a href="{{.PortalURL}}">Open your account</a></p>{{end}}
</body></html>`

const tmplSubscriptionCanceled = `<!DOCTYPE html>
<html><body style="font-family: -apple-system, sans-serif; max-width: 600px; margin: 0 auto; padding: 20px;">
{{if .Immediate}}<h2>Subscription Canceled</h2>
<p>Your <strong>{{.Product}}</strong> subscription has been canceled immediately.</p>
<p>Your access has ended. Thank you for being a customer.</p>
{{else}}<h2>Subscription Will Be Canceled</h2>
<p>Your <strong>{{.Product}}</strong> subscription will be canceled at the end of the current billing period.</p>
<p>You can continue using the service until then.</p>
{{end}}</body></html>`

const tmplDunningSecond = `<!DOCTYPE html>
<html><body style="font-family: -apple-system, sans-serif; max-width: 600px; margin: 0 auto; padding: 20px;">
<h2 style="color: #d97706;">Payment Still Outstanding</h2>
<p>We've been unable to process your payment for <strong>{{.Product}}</strong> for over a week.</p>
<p>Please update your payment method to avoid losing access.</p>
</body></html>`

const tmplDunningFinal = `<!DOCTYPE html>
<html><body style="font-family: -apple-system, sans-serif; max-width: 600px; margin: 0 auto; padding: 20px;">
<h2 style="color: #dc2626;">Final Notice — Access Will Be Suspended</h2>
<p>Your <strong>{{.Product}}</strong> payment has been overdue for 14 days.</p>
<p>Your access will be suspended soon if payment is not received.</p>
<p>Please update your payment method immediately.</p>
</body></html>`

const tmplPaymentRecovered = `<!DOCTYPE html>
<html><body style="font-family: -apple-system, sans-serif; max-width: 600px; margin: 0 auto; padding: 20px;">
<h2 style="color: #059669;">Payment Recovered — Thanks!</h2>
<p>Your payment for <strong>{{.Product}}</strong> went through. Your subscription is active again, and access is fully restored.</p>
<p>No further action required.</p>
</body></html>`

const tmplLoginCode = `<!DOCTYPE html>
<html><body style="font-family: -apple-system, sans-serif; max-width: 600px; margin: 0 auto; padding: 20px;">
<h2>Your login code</h2>
<p>Enter this code to sign in:</p>
<div style="background: #f4f4f5; border-radius: 8px; padding: 16px; margin: 16px 0; font-family: monospace; font-size: 32px; text-align: center; letter-spacing: 8px; font-weight: bold;">{{.Code}}</div>
<p style="color: #666; font-size: 14px;">This code expires in 10 minutes. If you didn't request this, ignore this email.</p>
</body></html>`

const tmplPlanChanged = `<!DOCTYPE html>
<html><body style="font-family: -apple-system, sans-serif; max-width: 600px; margin: 0 auto; padding: 20px;">
<h2>Plan Changed</h2>
<p>Your <strong>{{.Product}}</strong> plan has been changed from <strong>{{.OldPlan}}</strong> to <strong>{{.NewPlan}}</strong>.</p>
</body></html>`

const tmplRenewalReminder = `<!DOCTYPE html>
<html><body style="font-family: -apple-system, sans-serif; max-width: 600px; margin: 0 auto; padding: 20px;">
<h2>Renewal Reminder</h2>
<p>Your <strong>{{.Product}}</strong> subscription will renew on <strong>{{.RenewalDate}}</strong>.</p>
<p>No action is needed if you'd like to continue.</p>
</body></html>`

const tmplPaymentActionRequired = `<!DOCTYPE html>
<html><body style="font-family: -apple-system, sans-serif; max-width: 600px; margin: 0 auto; padding: 20px;">
<h2 style="color: #d97706;">Payment Authentication Required</h2>
<p>Your payment for <strong>{{.Product}}</strong> requires additional authentication.</p>
<p><a href="{{.InvoiceURL}}" style="display:inline-block;background:#2563eb;color:white;padding:10px 24px;border-radius:6px;text-decoration:none;">Complete Payment</a></p>
<p style="color:#666;font-size:14px;">If you don't complete this step, your subscription may be interrupted.</p>
</body></html>`

const tmplTrialEnding = `<!DOCTYPE html>
<html><body style="font-family: -apple-system, sans-serif; max-width: 600px; margin: 0 auto; padding: 20px;">
<h2>Your Trial is Ending Soon</h2>
<p>Your <strong>{{.Product}}</strong> trial ends on <strong>{{.TrialEnd}}</strong>.</p>
<p>After the trial, your subscription will begin automatically. No action needed if you'd like to continue.</p>
<p style="color:#666;font-size:14px;">If you'd like to cancel, you can do so from your account portal before the trial ends.</p>
</body></html>`
