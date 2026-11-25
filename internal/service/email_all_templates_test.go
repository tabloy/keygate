package service

import (
	"context"
	"fmt"
	"html"
	"log/slog"
	"strings"
	"testing"

	"github.com/tabloy/keygate/internal/branding"
	"github.com/tabloy/keygate/internal/store"
	"github.com/tabloy/keygate/internal/testsupport"
)

// renderers calls each email's Render* with realistic arguments, keyed
// like emailTemplates.
func renderers(s *EmailService) map[string]func() (string, string) {
	return map[string]func() (string, string){
		"license_created":   func() (string, string) { return s.RenderLicenseCreated("App", "Pro", "KG-1", "https://d.example") },
		"license_expiring":  func() (string, string) { return s.RenderLicenseExpiring("App", "KG-1", "2026-01-01", 3, true, "Free") },
		"updates_ending":    func() (string, string) { return s.RenderUpdatesEnding("App", "KG-1", "2026-01-01") },
		"license_expired":   func() (string, string) { return s.RenderLicenseExpired("App") },
		"trial_expired":     func() (string, string) { return s.RenderTrialExpired("App", "Free") },
		"license_suspended": func() (string, string) { return s.RenderLicenseSuspended("App", "abuse") },
		"quota_warning":     func() (string, string) { return s.RenderQuotaWarning("App", "api_calls", 80, 100, 80) },
		"seat_invite": func() (string, string) {
			return s.RenderSeatInvite("App", "a@example.com", "https://x.example/accept?token=t", "https://d.example")
		},
		"trial_ending":            func() (string, string) { return s.RenderTrialEnding("App", "2026-01-01") },
		"renewal_reminder":        func() (string, string) { return s.RenderRenewalReminder("App", "2026-01-01") },
		"plan_changed":            func() (string, string) { return s.RenderPlanChanged("App", "Basic", "Pro") },
		"subscription_canceled":   func() (string, string) { return s.RenderSubscriptionCanceled("App", true) },
		"payment_failed":          func() (string, string) { return s.RenderPaymentFailed("App") },
		"dunning_second":          func() (string, string) { return s.RenderDunningSecond("App") },
		"dunning_final":           func() (string, string) { return s.RenderDunningFinal("App") },
		"payment_action_required": func() (string, string) { return s.RenderPaymentActionRequired("App", "https://i.example/1") },
		"payment_recovered":       func() (string, string) { return s.RenderPaymentRecovered("App") },
		"welcome":                 func() (string, string) { return s.RenderWelcome("Alex") },
		"login_code":              func() (string, string) { return s.RenderOTPCode("123456") },
		"admin_invite":            func() (string, string) { return s.RenderAdminInvite("Acme", "a@example.com", "admin") },
	}
}

// Every email is listed once, in a group, with a default body and
// subject that pass the save-time checks, and has a Render* function.
func TestEveryEmailIsEditable(t *testing.T) {
	seen := map[string]bool{}
	r := renderers(newTemplateTestService())
	for _, tmpl := range emailTemplates {
		if seen[tmpl.Key] {
			t.Fatalf("%s listed twice", tmpl.Key)
		}
		seen[tmpl.Key] = true
		if tmpl.Group != "licenses" && tmpl.Group != "billing" && tmpl.Group != "account" {
			t.Errorf("%s: unknown group %q", tmpl.Key, tmpl.Group)
		}
		if err := ValidateTemplate(tmpl.Key, tmpl.Body); err != nil {
			t.Errorf("%s: default body does not validate: %v", tmpl.Key, err)
		}
		if err := ValidateSubject(tmpl.Key, tmpl.Subject); err != nil {
			t.Errorf("%s: default subject does not validate: %v", tmpl.Key, err)
		}
		for _, v := range tmpl.Vars {
			if _, ok := templateSample[v]; !ok {
				t.Errorf("%s: variable %s missing from templateSample", tmpl.Key, v)
			}
		}
		if r[tmpl.Key] == nil {
			t.Errorf("%s has no renderer in this test", tmpl.Key)
		}
	}
	for _, kind := range ToggleableEmails {
		if !seen[kind] {
			t.Errorf("%s can be switched off but has no editable template", kind)
		}
	}
}

// Every email uses its custom body and subject, and each sender passes
// every variable the editor offers for it: a subject naming one the
// sender does not pass would fall back to the default and fail here.
func TestEveryEmailUsesItsCustomTemplate(t *testing.T) {
	st := openNotifyStore(t)
	testsupport.LockSettings(t, st.DB)
	ctx := context.Background()
	for _, tmpl := range emailTemplates {
		var body, subject strings.Builder
		fmt.Fprintf(&body, "<p>CUSTOM-%s", tmpl.Key)
		fmt.Fprintf(&subject, "SUBJ-%s", tmpl.Key)
		for _, v := range tmpl.Vars {
			fmt.Fprintf(&body, " {{.%s}}", v)
			if !secretTemplateVars[v] {
				fmt.Fprintf(&subject, " {{.%s}}", v)
			}
		}
		body.WriteString("</p>")
		for k, v := range map[string]string{"email_template_" + tmpl.Key: body.String(), "email_subject_" + tmpl.Key: subject.String()} {
			if err := st.SetSetting(ctx, k, v); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.DeleteSetting(ctx, k) })
		}
	}
	s := NewEmailService("", "", "", "", "", slog.Default(), st)
	s.SetBaseURL("https://licenses.example.com")
	for key, render := range renderers(s) {
		sub, b := render()
		if !strings.HasPrefix(sub, "SUBJ-"+key+" ") {
			t.Errorf("%s: custom subject not used (a variable the sender does not pass?): %q", key, sub)
		}
		if !strings.HasPrefix(b, "<p>CUSTOM-"+key+" ") {
			t.Errorf("%s: custom body not used: %q", key, b)
		}
	}
}

// The emails that used to be built inline send what they sent before
// when nothing is customised. The expected strings are the old code.
func TestFormerlyInlineEmailsAreUnchanged(t *testing.T) {
	s := newTemplateTestService()
	const p = `R&D "Suite" <Pro>`
	e := html.EscapeString(p)
	const head = "<!DOCTYPE html>\n<html><body style=\"font-family: -apple-system, sans-serif; max-width: 600px; margin: 0 auto; padding: 20px;\">\n"
	type tc struct{ name, gotS, gotB, wantS, wantB string }
	var cases []tc
	add := func(name, gs, gb, ws, wb string) { cases = append(cases, tc{name, gs, gb, ws, wb}) }

	gs, gb := s.RenderSubscriptionCanceled(p, true)
	add("canceled now", gs, gb, p+" subscription canceled", head+`<h2>Subscription Canceled</h2>
<p>Your <strong>`+e+`</strong> subscription has been canceled immediately.</p>
<p>Your access has ended. Thank you for being a customer.</p>
</body></html>`)
	gs, gb = s.RenderSubscriptionCanceled(p, false)
	add("canceled at period end", gs, gb, p+" subscription canceled", head+`<h2>Subscription Will Be Canceled</h2>
<p>Your <strong>`+e+`</strong> subscription will be canceled at the end of the current billing period.</p>
<p>You can continue using the service until then.</p>
</body></html>`)
	gs, gb = s.RenderDunningSecond(p)
	add("dunning second", gs, gb, p+" — payment still outstanding", head+`<h2 style="color: #d97706;">Payment Still Outstanding</h2>
<p>We've been unable to process your payment for <strong>`+e+`</strong> for over a week.</p>
<p>Please update your payment method to avoid losing access.</p>
</body></html>`)
	gs, gb = s.RenderDunningFinal(p)
	add("dunning final", gs, gb, p+" — final payment notice", head+`<h2 style="color: #dc2626;">Final Notice — Access Will Be Suspended</h2>
<p>Your <strong>`+e+`</strong> payment has been overdue for 14 days.</p>
<p>Your access will be suspended soon if payment is not received.</p>
<p>Please update your payment method immediately.</p>
</body></html>`)
	gs, gb = s.RenderPaymentRecovered(p)
	add("payment recovered", gs, gb, p+" — payment recovered", head+`<h2 style="color: #059669;">Payment Recovered — Thanks!</h2>
<p>Your payment for <strong>`+e+`</strong> went through. Your subscription is active again, and access is fully restored.</p>
<p>No further action required.</p>
</body></html>`)
	gs, gb = s.RenderOTPCode("482913")
	add("login code", gs, gb, "Your login code", head+`<h2>Your login code</h2>
<p>Enter this code to sign in:</p>
<div style="background: #f4f4f5; border-radius: 8px; padding: 16px; margin: 16px 0; font-family: monospace; font-size: 32px; text-align: center; letter-spacing: 8px; font-weight: bold;">482913</div>
<p style="color: #666; font-size: 14px;">This code expires in 10 minutes. If you didn't request this, ignore this email.</p>
</body></html>`)
	gs, gb = s.RenderPlanChanged(p, "Basic & Co", "Pro <2>")
	add("plan changed", gs, gb, p+" plan changed", head+`<h2>Plan Changed</h2>
<p>Your <strong>`+e+`</strong> plan has been changed from <strong>`+html.EscapeString("Basic & Co")+`</strong> to <strong>`+html.EscapeString("Pro <2>")+`</strong>.</p>
</body></html>`)
	gs, gb = s.RenderRenewalReminder(p, "2026-12-01")
	add("renewal reminder", gs, gb, p+" renewal coming up", head+`<h2>Renewal Reminder</h2>
<p>Your <strong>`+e+`</strong> subscription will renew on <strong>2026-12-01</strong>.</p>
<p>No action is needed if you'd like to continue.</p>
</body></html>`)
	const invoice = "https://invoice.stripe.com/i/acct_1AbC/test_YWNjdF8x?s=ap"
	gs, gb = s.RenderPaymentActionRequired(p, invoice)
	add("payment action required", gs, gb, p+" — payment authentication required", head+`<h2 style="color: #d97706;">Payment Authentication Required</h2>
<p>Your payment for <strong>`+e+`</strong> requires additional authentication.</p>
<p><a href="`+html.EscapeString(invoice)+`" style="display:inline-block;background:#2563eb;color:white;padding:10px 24px;border-radius:6px;text-decoration:none;">Complete Payment</a></p>
<p style="color:#666;font-size:14px;">If you don't complete this step, your subscription may be interrupted.</p>
</body></html>`)
	gs, gb = s.RenderTrialEnding(p, "2026-12-01")
	add("trial ending", gs, gb, p+" trial ending soon", head+`<h2>Your Trial is Ending Soon</h2>
<p>Your <strong>`+e+`</strong> trial ends on <strong>2026-12-01</strong>.</p>
<p>After the trial, your subscription will begin automatically. No action needed if you'd like to continue.</p>
<p style="color:#666;font-size:14px;">If you'd like to cancel, you can do so from your account portal before the trial ends.</p>
</body></html>`)

	for _, c := range cases {
		if c.gotS != c.wantS {
			t.Errorf("%s: subject\n got %q\nwant %q", c.name, c.gotS, c.wantS)
		}
		if c.gotB != c.wantB {
			t.Errorf("%s: body changed\n got:\n%s\nwant:\n%s", c.name, c.gotB, c.wantB)
		}
	}
}

// html/template writes "+" as &#43; (the old inline code left it as is).
// Mail clients show the same, and the plain-text part decodes it.
func TestPlusSignsReadTheSame(t *testing.T) {
	s := newTemplateTestService()
	_, body := s.RenderPlanChanged("Notepad++", "Basic", "Pro+")
	if !strings.Contains(body, "Notepad&#43;&#43;") {
		t.Fatalf("expected html/template escaping:\n%s", body)
	}
	if text := htmlToText(body); !strings.Contains(text, "Notepad++") || !strings.Contains(text, "Pro+") {
		t.Fatalf("text part shows entities:\n%s", text)
	}
}

// The subjects are the ones the code used to build by hand.
func TestSubjectsAreUnchanged(t *testing.T) {
	s := newTemplateTestService()
	const p = "Acme & Co <App>"
	check := func(name, got, want string) {
		t.Helper()
		if got != want {
			t.Errorf("%s: subject %q, want %q", name, got, want)
		}
	}
	sub, _ := s.RenderLicenseCreated(p, "Pro", "KG-1", "")
	check("license created", sub, "Your license for "+p)
	sub, _ = s.RenderLicenseExpiring(p, "KG-1", "2026-01-01", 3, false, "")
	check("license expiring", sub, p+" license expiring soon")
	sub, _ = s.RenderLicenseExpiring(p, "KG-1", "2026-01-01", 3, true, "")
	check("trial expiring", sub, p+" trial ending soon")
	sub, _ = s.RenderUpdatesEnding(p, "KG-1", "2026-01-01")
	check("updates ending", sub, p+" updates ending soon")
	sub, _ = s.RenderQuotaWarning(p, "api_calls", 80, 100, 80)
	check("quota warning", sub, p+": api_calls quota at 80%")
	sub, _ = s.RenderSeatInvite(p, "a@example.com", "https://example.com/i", "")
	check("seat invite", sub, "You've been invited to "+p)
	sub, _ = s.RenderLicenseExpired(p)
	check("license expired", sub, p+" license expired")
	sub, _ = s.RenderTrialExpired(p, "")
	check("trial expired", sub, p+" trial has ended")
	sub, _ = s.RenderLicenseSuspended(p, "")
	check("license suspended", sub, p+" license suspended")
	sub, _ = s.RenderPaymentFailed(p)
	check("payment failed", sub, p+" payment failed")
	sub, _ = s.RenderWelcome("Alex")
	check("welcome", sub, "Welcome to "+branding.Project)
	sub, _ = s.RenderAdminInvite("Acme Licensing", "a@example.com", "admin")
	check("admin invite", sub, "You've been added to Acme Licensing")
}

func setCustom(t *testing.T, st *store.Store, kv map[string]string) {
	t.Helper()
	ctx := context.Background()
	for k, v := range kv {
		if err := st.SetSetting(ctx, k, v); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st.DeleteSetting(ctx, k) })
	}
}

// A custom subject falls back to the default when it fails, renders
// empty or too long, or names a secret (stored past validation).
func TestCustomSubjectFallbacks(t *testing.T) {
	st := openNotifyStore(t)
	testsupport.LockSettings(t, st.DB)
	setCustom(t, st, map[string]string{
		"email_subject_dunning_final":   `{{.Nope}}`,
		"email_subject_license_expired": `{{if false}}x{{end}}`,
		"email_subject_plan_changed":    `{{printf "%0250d" 1}}`,
		"email_subject_login_code":      `{{.Code}} is your code`,
		"site_name":                     "Acme Licensing",
	})
	s := NewEmailService("", "", "", "", "", slog.Default(), st)
	for name, got := range map[string]func() (string, string){
		"App — final payment notice": func() (string, string) { return s.RenderDunningFinal("App") },
		"App license expired":        func() (string, string) { return s.RenderLicenseExpired("App") },
		"App plan changed":           func() (string, string) { return s.RenderPlanChanged("App", "A", "B") },
		"Your login code":            func() (string, string) { return s.RenderOTPCode("123456") },
		"Welcome to Acme Licensing":  func() (string, string) { return s.RenderWelcome("Alex") },
	} {
		if sub, _ := got(); sub != name {
			t.Errorf("subject %q, want %q", sub, name)
		}
	}
}

// A seat invite whose custom body leaves out the link still carries it.
func TestSeatInviteKeepsTheLink(t *testing.T) {
	st := openNotifyStore(t)
	testsupport.LockSettings(t, st.DB)
	setCustom(t, st, map[string]string{"email_template_seat_invite": `<p>Join {{.Product}}</p>`})
	s := NewEmailService("", "", "", "", "", slog.Default(), st)
	const url = "https://x.example/accept?token=t"
	if _, body := s.RenderSeatInvite("App", "a@example.com", url, ""); !strings.Contains(body, url) {
		t.Fatalf("invite link missing:\n%s", body)
	}
}

// A login-code email must show the code as text the reader can see:
// saving one that does not is refused, and one stored anyway is replaced
// by the default when sent.
func TestLoginCodeEmailAlwaysShowsTheCode(t *testing.T) {
	for _, bad := range []string{
		`<p>Welcome back!</p>`,
		`<a href="{{.PortalURL}}?code={{.Code}}">Sign in</a>`,
		`<p title="{{.Code}}">Welcome</p>`,
		`<p>Your code is 482913</p>`, // the sample value typed in
	} {
		if err := ValidateTemplate("login_code", bad); err == nil {
			t.Errorf("login code template accepted: %s", bad)
		}
	}
	if err := ValidateTemplate("login_code", `<p>{{.Code}}</p>`); err != nil {
		t.Fatalf("a login code template with {{.Code}} was refused: %v", err)
	}
	if err := ValidateTemplate("license_created", `<p>Thanks</p>`); err != nil {
		t.Fatalf("other emails have no required variable: %v", err)
	}

	st := openNotifyStore(t)
	testsupport.LockSettings(t, st.DB)
	setCustom(t, st, map[string]string{"email_template_login_code": `<a href="https://x.example/?c={{.Code}}">Sign in</a>`})
	s := NewEmailService("", "", "", "", "", slog.Default(), st)
	if _, body := s.RenderOTPCode("777111"); !showsText(body, "777111") {
		t.Fatalf("login code not readable in the email:\n%s", body)
	}
}

func TestValidateSubject(t *testing.T) {
	for _, tc := range []struct {
		key, src string
		ok       bool
	}{
		{"license_created", "{{.Product}} licence ready", true},
		{"welcome", "Welcome to {{.SiteName}}, {{.Name}}", true},
		{"plan_changed", "Line one\nBcc: x@example.com", false},
		{"plan_changed", "{{.Prodcut}} typo", false},
		{"license_expiring", "{{if .IsTrial}}unclosed", false},
		{"license_expiring", "{{if .IsTrial}}{{.Product}}{{else}}{{.Prodcut}}{{end}}", false}, // typo in the other branch
		{"plan_changed", strings.Repeat("x", maxSubjectTemplate), true},
		{"plan_changed", strings.Repeat("x", maxSubjectTemplate+1), false},
		{"plan_changed", strings.Repeat("订", maxSubjectTemplate), true}, // characters, not bytes
		{"welcome", "{{.Product}} hi", false},                           // another email's variable
		{"login_code", "{{.Code}} is your code", false},                 // secret
		{"license_created", "Key {{.LicenseKey}}", false},
		{"seat_invite", "Join via {{.InviteURL}}", false},
		{"no_such_email", "Hello", false},
	} {
		if err := ValidateSubject(tc.key, tc.src); (err == nil) != tc.ok {
			t.Errorf("ValidateSubject(%s, %.40q) = %v, want ok=%v", tc.key, tc.src, err, tc.ok)
		}
	}
	if err := ValidateTemplate("welcome", "<p>{{.LicenseKey}}</p>"); err == nil {
		t.Error("a welcome body using another email's variable was accepted")
	}
	if err := ValidateTemplate("subscription_canceled", "{{if .Immediate}}<p>{{.Product}}</p>{{else}}<p>{{.Prodcut}}</p>{{end}}"); err == nil {
		t.Error("a typo in the branch the sample does not take was accepted")
	}
	if err := ValidateTemplate("no_such_email", "<p>x</p>"); err == nil {
		t.Error("a template for an unknown email was accepted")
	}
}
