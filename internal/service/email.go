package service

import (
	"bytes"
	"context"
	"fmt"
	"html/template"
	"log/slog"
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
}

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
		"payment_failed":    tmplPaymentFailed,
	}
}

func (s *EmailService) Send(to, subject, htmlBody string) error {
	if !s.enabled {
		s.logger.Info("email skipped (not configured)", "to", to, "subject", subject)
		return nil
	}

	// Append attribution footer (AGPL v3 Section 7b — see NOTICE)
	if !strings.Contains(htmlBody, branding.Domain) {
		htmlBody = strings.Replace(htmlBody, "</body>", emailFooter()+"</body>", 1)
	}

	msg := strings.Join([]string{
		"From: " + s.from,
		"To: " + to,
		"Subject: " + subject,
		"MIME-Version: 1.0",
		"Content-Type: text/html; charset=UTF-8",
		"",
		htmlBody,
	}, "\r\n")

	var auth smtp.Auth
	if s.username != "" {
		auth = smtp.PlainAuth("", s.username, s.password, s.host)
	}

	addr := s.host + ":" + s.port
	err := smtp.SendMail(addr, auth, s.from, []string{to}, []byte(msg))
	if err != nil {
		// Retry once after a short delay
		s.logger.Warn("email send failed, retrying", "to", to, "error", err)
		time.Sleep(3 * time.Second)
		err = smtp.SendMail(addr, auth, s.from, []string{to}, []byte(msg))
		if err != nil {
			s.logger.Error("email send failed after retry", "to", to, "error", err)
			return fmt.Errorf("email send: %w", err)
		}
	}
	s.logger.Info("email sent", "to", to, "subject", subject)
	return nil
}

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

func (s *EmailService) SendSeatInvite(to, productName, inviterName string) {
	body := renderTemplate(s.getTemplate("seat_invite", tmplSeatInvite), map[string]string{
		"Product": productName,
		"Inviter": inviterName,
	})
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
<p>Sign in to your account to accept the invitation.</p>
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
