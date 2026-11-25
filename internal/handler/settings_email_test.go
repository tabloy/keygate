package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/tabloy/keygate/internal/service"
	"github.com/tabloy/keygate/internal/store"
	"github.com/tabloy/keygate/internal/testsupport"
)

// Every email's body and subject can be saved from the dashboard, and
// nothing else under those prefixes can.
func TestEveryEmailTemplateIsWritable(t *testing.T) {
	for _, tmpl := range service.Templates() {
		for _, setting := range []string{"email_template_" + tmpl.Key, "email_subject_" + tmpl.Key} {
			if !settingsWritable[setting] {
				t.Errorf("%s is not writable", setting)
			}
		}
	}
	for _, setting := range []string{"email_template_no_such_email", "email_subject_no_such_email"} {
		if settingsWritable[setting] {
			t.Errorf("%s is writable but no email uses it", setting)
		}
	}
}

func openEmailTemplateStore(t *testing.T) *store.Store {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("skipping integration test: TEST_DATABASE_URL not set")
	}
	s, err := store.New(dsn)
	if err != nil {
		t.Skipf("skipping integration test: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.RunMigrations("../../db/migrations"); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	testsupport.LockSettings(t, s.DB)
	return s
}

// The save-time rules are enforced where the dashboard saves: a refused
// value answers 400 and stores nothing, and a value saved under older,
// looser rules can be sent back unchanged.
func TestEmailTemplateSettingsAreValidatedOnSave(t *testing.T) {
	s := openEmailTemplateStore(t)
	ctx := context.Background()
	gin.SetMode(gin.TestMode)
	h := &AdminHandler{Store: s}
	keys := []string{"email_template_login_code", "email_subject_plan_changed", "email_subject_welcome",
		"email_subject_login_code", "email_template_license_expired", "email_template_welcome"}
	for _, k := range keys {
		_ = s.DeleteSetting(ctx, k)
		t.Cleanup(func() { _ = s.DeleteSetting(ctx, k) })
	}
	save := func(settings map[string]string) int {
		body, _ := json.Marshal(map[string]any{"settings": settings})
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPut, "/admin/settings", strings.NewReader(string(body)))
		c.Request.Header.Set("Content-Type", "application/json")
		h.UpdateSettings(c)
		return w.Code
	}
	for name, bad := range map[string]map[string]string{
		"login code without the code":   {"email_template_login_code": "<p>Welcome back</p>"},
		"login code only in a link":     {"email_template_login_code": `<a href="https://x.example/?c={{.Code}}">Sign in</a>`},
		"subject on two lines":          {"email_subject_plan_changed": "one\ntwo"},
		"another email's variable":      {"email_subject_welcome": "{{.Product}} hi"},
		"secret in a subject":           {"email_subject_login_code": "{{.Code}} is your code"},
		"a valid key next to a bad one": {"email_template_welcome": "<p>Hi {{.Name}}</p>", "email_subject_plan_changed": "{{.Prodcut}}"},
	} {
		if code := save(bad); code != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400", name, code)
		}
	}
	for _, k := range keys {
		if v, err := s.GetSetting(ctx, k); err == nil && v != "" {
			t.Errorf("%s was stored (%q) although the save was refused", k, v)
		}
	}
	if code := save(map[string]string{"email_template_welcome": "<p>Hi {{.Name}}</p>", "email_subject_welcome": "Hello from {{.SiteName}}"}); code != http.StatusOK {
		t.Fatalf("a valid welcome template was refused: %d", code)
	}
	// Saved before per-email checks existed: the licence key is not one
	// of license_expired's variables. Sending it back unchanged is fine.
	old := "<p>{{.Product}} {{.LicenseKey}}</p>"
	if err := s.SetSetting(ctx, "email_template_license_expired", old); err != nil {
		t.Fatal(err)
	}
	if code := save(map[string]string{"email_template_license_expired": old}); code != http.StatusOK {
		t.Errorf("re-sending an unchanged older template: got %d, want 200", code)
	}
	if code := save(map[string]string{"email_template_license_expired": old + " "}); code != http.StatusBadRequest {
		t.Errorf("changing it is judged by today's rules: got %d, want 400", code)
	}
}

// The editor gets every email with its group, variables and subject.
func TestGetEmailTemplatesListsEveryEmail(t *testing.T) {
	s := openEmailTemplateStore(t)
	gin.SetMode(gin.TestMode)
	h := &AdminHandler{Store: s}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/admin/email-templates", nil)
	h.GetEmailTemplates(c)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	var resp struct {
		Data struct {
			Templates map[string]map[string]any `json:"templates"`
			Order     []string                  `json:"order"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	all := service.Templates()
	if len(resp.Data.Order) != len(all) || len(resp.Data.Templates) != len(all) {
		t.Fatalf("listed %d/%d emails, want %d", len(resp.Data.Order), len(resp.Data.Templates), len(all))
	}
	for i, tmpl := range all {
		got := resp.Data.Templates[tmpl.Key]
		vars, _ := got["variables"].([]any)
		if resp.Data.Order[i] != tmpl.Key || got["default"] != tmpl.Body || got["subject_default"] != tmpl.Subject ||
			got["group"] != tmpl.Group || len(vars) != len(tmpl.Variables) {
			t.Errorf("%s listed wrongly: %v", tmpl.Key, got)
		}
	}
}
