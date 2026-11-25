package service

import (
	"context"
	"html/template"
	"log/slog"
	"strings"
	"testing"

	"github.com/tabloy/keygate/internal/testsupport"
)

func newTemplateTestService() *EmailService {
	s := NewEmailService("", "", "", "", "", slog.Default(), nil)
	s.SetBaseURL("https://licenses.example.com/")
	return s
}

// The download button appears only when the product has a download page.
func TestRenderLicenseCreated_DownloadURL(t *testing.T) {
	s := newTemplateTestService()

	_, with := s.RenderLicenseCreated("Acme App", "Pro", "KG-1", "https://example.com/download")
	if !strings.Contains(with, `href="https://example.com/download"`) || !strings.Contains(with, "Download Acme App") {
		t.Fatalf("download button missing:\n%s", with)
	}

	_, without := s.RenderLicenseCreated("Acme App", "Pro", "KG-1", "")
	if strings.Contains(without, "Download") {
		t.Fatalf("download button shown without a download page:\n%s", without)
	}
}

// Every customer template can link the portal; the URL is built from
// BASE_URL without a doubled slash.
func TestTemplateData_PortalURL(t *testing.T) {
	s := newTemplateTestService()
	got := s.templateData(map[string]any{})
	if got["PortalURL"] != "https://licenses.example.com/portal" {
		t.Fatalf("PortalURL = %v", got["PortalURL"])
	}
	if _, ok := got["SiteName"]; !ok {
		t.Fatal("SiteName must always be set, even when empty")
	}
	// A caller's own value wins.
	if got := s.templateData(map[string]any{"PortalURL": "x"}); got["PortalURL"] != "x" {
		t.Fatal("templateData overwrote a caller value")
	}
	// No BASE_URL configured: empty, never a relative "/portal".
	bare := NewEmailService("", "", "", "", "", slog.Default(), nil)
	if got := bare.templateData(map[string]any{}); got["PortalURL"] != "" {
		t.Fatalf("PortalURL without BASE_URL = %v", got["PortalURL"])
	}
}

// The expiring email says "trial" for trials and gives the days left.
func TestLicenseExpiringTemplate_TrialWordingAndDaysLeft(t *testing.T) {
	s := newTemplateTestService()
	render := func(trial bool) string {
		return renderTemplate(tmplLicenseExpiring, s.templateData(map[string]any{
			"Product": "Acme App", "LicenseKey": "KG-1", "ExpiresAt": "2026-10-08", "DaysLeft": 3, "IsTrial": trial,
		}))
	}
	trial, paid := render(true), render(false)
	if !strings.Contains(trial, "Trial Ending Soon") || !strings.Contains(trial, "days remaining: 3") {
		t.Fatalf("trial wording wrong:\n%s", trial)
	}
	if !strings.Contains(paid, "License Expiring Soon") || strings.Contains(paid, "trial") {
		t.Fatalf("license wording wrong:\n%s", paid)
	}
}

// The end of a trial says what became of the licence: carried on with
// the fallback plan, named and escaped, or ended.
func TestTrialExpiredTemplate_FallbackPlan(t *testing.T) {
	s := newTemplateTestService()
	render := func(fallback string) string {
		return renderTemplate(tmplTrialExpired, s.templateData(map[string]any{"Product": "Acme App", "FallbackPlan": fallback}))
	}
	moved, ended := render("Free <b>tier</b>"), render("")
	if !strings.Contains(moved, "carries on with the <strong>Free &lt;b&gt;tier&lt;/b&gt;</strong> plan") || !strings.Contains(moved, "that plan's limits now apply") {
		t.Fatalf("moved wording wrong or unescaped:\n%s", moved)
	}
	if strings.Contains(ended, "carries on") || !strings.Contains(ended, "Subscribe to a paid plan") {
		t.Fatalf("ended wording wrong:\n%s", ended)
	}
}

// A template that parses but fails when rendered is refused on save.
func TestValidateTemplate_RejectsRenderTimeErrors(t *testing.T) {
	bad := `{{if .DownloadURL}}<a href="{{.DownloadURL}}{{end}}">x</a>`
	if _, err := template.New("t").Parse(bad); err != nil {
		t.Fatalf("precondition: should parse: %v", err)
	}
	if ValidateTemplate("license_created", bad) == nil {
		t.Fatal("a template that fails at render time was accepted")
	}
	if err := ValidateTemplate("license_created", tmplLicenseCreated); err != nil {
		t.Fatalf("the default template was rejected: %v", err)
	}
	// A misspelt variable would render as nothing: the key missing from
	// the licence email.
	if ValidateTemplate("license_created", `<p>{{.LicenceKey}}</p>`) == nil {
		t.Fatal("a template naming an unknown variable was accepted")
	}
	// Every built-in template passes, so one copied into the editor and
	// tweaked is not refused for the variables it came with.
	for name, src := range map[string]string{
		"license_created": tmplLicenseCreated, "license_expiring": tmplLicenseExpiring,
		"updates_ending": tmplUpdatesEnding, "quota_warning": tmplQuotaWarning,
		"seat_invite": tmplSeatInvite, "admin_invite": tmplAdminInvite,
		"license_suspended": tmplLicenseSuspended, "payment_failed": tmplPaymentFailed,
		"license_expired": tmplLicenseExpired, "trial_expired": tmplTrialExpired,
	} {
		if err := ValidateTemplate(name, src); err != nil {
			t.Errorf("built-in %s template rejected: %v", name, err)
		}
	}
}

// A stored custom template that fails at send time falls back to the
// built-in one — never the raw template source.
func TestRenderEmail_FallsBackToDefault(t *testing.T) {
	s := openNotifyStore(t)
	testsupport.LockSettings(t, s.DB)
	ctx := context.Background()
	key := "email_template_license_created"
	if err := s.SetSetting(ctx, key, `{{if .DownloadURL}}<a href="{{.DownloadURL}}{{end}}">{{.LicenseKey}}</a>`); err != nil {
		t.Fatal(err)
	}
	e := NewEmailService("", "", "", "", "", slog.Default(), s)
	_, body := e.RenderLicenseCreated("Acme", "Pro", "KG-REAL-KEY", "https://example.com/dl")
	if strings.Contains(body, "{{") || !strings.Contains(body, "KG-REAL-KEY") {
		t.Fatalf("broken custom template not replaced by the default:\n%s", body)
	}
}

// The admin invite links to BASE_URL's login page, not to whatever host
// the inviting request came through.
func TestRenderAdminInvite_LoginURL(t *testing.T) {
	s := newTemplateTestService()
	subject, body := s.RenderAdminInvite("Acme", "Owner", "admin")
	if subject != "You've been added to Acme" || !strings.Contains(body, `https://licenses.example.com/login`) {
		t.Fatalf("subject %q, body:\n%s", subject, body)
	}
}
