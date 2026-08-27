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

type smtpConfig struct {
	host     string
	port     string
	username string
	password string
	from     string
	enabled  bool
}

// getConfig returns the active SMTP configuration. Settings stored
// dynamically in the database (via Admin Settings) take precedence over
// the startup environment variables (s.host, s.port, etc.), allowing
// administrators to configure or update SMTP from the web dashboard
// without restarting the server.
func (s *EmailService) getConfig() smtpConfig {
	cfg := smtpConfig{
		host:     s.host,
		port:     s.port,
		username: s.username,
		password: s.password,
		from:     s.from,
	}
	if s.store != nil {
		settings, err := s.store.GetSettings(context.Background())
		if err == nil {
			if v, ok := settings["smtp_host"]; ok && strings.TrimSpace(v) != "" {
				cfg.host = strings.TrimSpace(v)
			}
			if v, ok := settings["smtp_port"]; ok && strings.TrimSpace(v) != "" {
				cfg.port = strings.TrimSpace(v)
			}
			if v, ok := settings["smtp_username"]; ok && (strings.TrimSpace(v) != "" || settings["smtp_host"] != "") {
				cfg.username = strings.TrimSpace(v)
			}
			if v, ok := settings["smtp_password"]; ok && v != "" {
				cfg.password = v
			}
			if v, ok := settings["smtp_from"]; ok && strings.TrimSpace(v) != "" {
				cfg.from = strings.TrimSpace(v)
			}
		}
	}
	if cfg.port == "" {
		cfg.port = "587"
	}
	cfg.enabled = cfg.host != "" && cfg.from != ""
	return cfg
}

func (s *EmailService) IsConfigured() bool { return s.getConfig().enabled }

func NewEmailService(host, port, username, password, from string, logger *slog.Logger, s *store.Store) *EmailService {
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
	cfg := s.getConfig()
	if !cfg.enabled {
		s.logger.Info("email skipped (not configured)", "to", to, "subject", subject)
		return nil
	}

	// Append attribution footer (AGPL v3 Section 7b — see NOTICE)
	if !strings.Contains(htmlBody, branding.Domain) {
		htmlBody = strings.Replace(htmlBody, "</body>", emailFooter()+"</body>", 1)
	}

	msg := strings.Join([]string{
		"From: " + cfg.from,
		"To: " + to,
		"Subject: " + subject,
		"MIME-Version: 1.0",
		"Content-Type: text/html; charset=UTF-8",
		"",
		htmlBody,
	}, "\r\n")

	addr := cfg.host + ":" + cfg.port
	err := s.sendOnceWithConfig(cfg, addr, to, []byte(msg))
	if err != nil {
		// Retry once after a short delay — transient TCP / TLS hiccups.
		s.logger.Warn("email send failed, retrying", "to", to, "error", err)
		time.Sleep(3 * time.Second)
		err = s.sendOnceWithConfig(cfg, addr, to, []byte(msg))
		if err != nil {
			s.logger.Error("email send failed after retry", "to", to, "error", err)
			return fmt.Errorf("email send: %w", err)
		}
	}
	s.logger.Info("email sent", "to", to, "subject", subject)
	return nil
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
//  1. TCP dial (or direct TLS dial if port == 465).
//  2. EHLO (records server-advertised extensions).
//  3. STARTTLS if advertised and not already on TLS — Office 365 /
//     Gmail / most modern submission endpoints require it on port 587.
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
func (s *EmailService) sendOnce(addr, to string, msg []byte) error {
	cfg := s.getConfig()
	return s.sendOnceWithConfig(cfg, addr, to, msg)
}

func (s *EmailService) sendOnceWithConfig(cfg smtpConfig, addr, to string, msg []byte) error {
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

	var conn net.Conn
	dialer := &net.Dialer{Timeout: 30 * time.Second}
	tlsConf := s.tlsConfig
	if tlsConf == nil {
		tlsConf = &tls.Config{ServerName: cfg.host, MinVersion: tls.VersionTLS12}
	}

	if cfg.port == "465" {
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, tlsConf)
	} else {
		conn, err = dialer.Dial("tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	c, err := smtp.NewClient(conn, cfg.host)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("smtp client: %w", err)
	}
	defer c.Close() //nolint:errcheck

	if err := c.Hello(localHostname()); err != nil {
		return fmt.Errorf("ehlo: %w", err)
	}

	// STARTTLS upgrade if not on implicit TLS (port 465) and the server advertises it.
	// (Client.StartTLS internally re-EHLOs so post-TLS extensions land in c.Extension.)
	if cfg.port != "465" {
		if ok, _ := c.Extension("STARTTLS"); ok {
			if err := c.StartTLS(tlsConf); err != nil {
				return fmt.Errorf("starttls: %w", err)
			}
		}
	}

	if cfg.username != "" {
		auth, perr := pickAuth(c, cfg.host, cfg.username, cfg.password)
		if perr != nil {
			return perr
		}
		if err := c.Auth(auth); err != nil {
			return fmt.Errorf("auth: %w", err)
		}
	}

	if err := c.Mail(envelopeFrom); err != nil {
		return fmt.Errorf("mail from: %w", err)
	}
	if err := c.Rcpt(to); err != nil {
		return fmt.Errorf("rcpt to: %w", err)
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("data: %w", err)
	}
	if _, err := w.Write(msg); err != nil {
		_ = w.Close()
		return fmt.Errorf("data write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("data close: %w", err)
	}
	return c.Quit()
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

func (s *EmailService) SendLicenseCreated(to, productName, planName, licenseKey string) {
	body := renderTemplate(s.getTemplate("license_created", tmplLicenseCreated), map[string]string{
		"Product":    productName,
		"Plan":       planName,
		"LicenseKey": licenseKey,
	})
	go func() {
		if err := s.Send(to, "Your license for "+productName, body); err != nil {
			s.logger.Error("email delivery failed", "to", to, "subject", "Your license for "+productName, "error", err)
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

func (s *EmailService) processQueue(ctx context.Context, db *store.Store) {
	emails, err := db.ListPendingEmails(ctx, 20)
	if err != nil {
		return
	}
	for _, e := range emails {
		if err := s.Send(e.ToAddr, e.Subject, e.Body); err != nil {
			db.MarkEmailFailed(ctx, e.ID, err.Error())
		} else {
			db.MarkEmailSent(ctx, e.ID)
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
