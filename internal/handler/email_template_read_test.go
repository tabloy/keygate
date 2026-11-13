package handler

import (
	"database/sql"
	"errors"
	"net/http"
	"os"
	"testing"

	"github.com/tabloy/keygate/internal/store"
)

func TestEmailTemplatesReadFailure(t *testing.T) {
	f := newAPIFixture(t)
	broken, err := store.New(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	if err = broken.Close(); err != nil {
		t.Fatal(err)
	}
	h := &AdminHandler{Store: broken}
	code, out := f.call(h.GetEmailTemplates, http.MethodGet, "/admin/email-templates", "", nil)
	if code != http.StatusInternalServerError {
		t.Fatalf("failed template read = %d, want 500; response keys: %v", code, out["success"])
	}
}

func TestEmailTemplatesKnownDefaultsAndCustomization(t *testing.T) {
	f := newAPIFixture(t)
	const key = "email_template_license_created"
	original, err := f.s.GetSetting(f.ctx, key)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatal(err)
	}
	existed := err == nil
	t.Cleanup(func() {
		if existed {
			if err := f.s.SetSetting(f.ctx, key, original); err != nil {
				t.Error(err)
			}
		} else {
			if _, err := f.s.DB.NewDelete().Model((*store.Setting)(nil)).Where("key = ?", key).Exec(f.ctx); err != nil {
				t.Error(err)
			}
		}
	})
	if _, err := f.s.DB.NewDelete().Model((*store.Setting)(nil)).Where("key = ?", key).Exec(f.ctx); err != nil {
		t.Fatal(err)
	}
	h := &AdminHandler{Store: f.s}
	read := func() map[string]any {
		t.Helper()
		code, out := f.call(h.GetEmailTemplates, http.MethodGet, "/admin/email-templates", "", nil)
		if code != http.StatusOK {
			t.Fatalf("read = %d", code)
		}
		return out["data"].(map[string]any)["templates"].(map[string]any)["license_created"].(map[string]any)
	}
	first := read()
	if first["custom"] != "" || first["default"] == "" {
		t.Fatal("known missing customization did not preserve the default")
	}
	const custom = "<p>Controlled customization</p><p>Powered by Keygate</p>"
	if err := f.s.SetSetting(f.ctx, key, custom); err != nil {
		t.Fatal(err)
	}
	if read()["custom"] != custom {
		t.Fatal("saved customization missing")
	}
}
