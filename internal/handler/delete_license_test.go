package handler

import (
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/service"
)

// Deleting a licence: one still in use is refused; a key bound to
// another product is refused (a backstop: admin keys cannot be bound
// today); the delete writes one audit line that keeps the product, the
// key hint and never the key, and sends license.deleted, without the key,
// with what a receiver needs to match it; a second delete finds nothing
// and records and sends nothing; a licence with a payment on record is
// refused and kept.
func TestDeleteLicense(t *testing.T) {
	f := newAPIFixture(t)
	wh := service.NewWebhookService(f.s, slog.Default(), time.Second, 1, true)
	h := &AdminHandler{Store: f.s, Webhook: wh}
	// Subscribed through the API, which accepts only registered events.
	hooks := &WebhookAdminHandler{Store: f.s, Webhook: wh}
	code, out := f.call(hooks.CreateWebhook, http.MethodPost, "/admin/webhooks",
		`{"product_id":"`+f.prodA.ID+`","url":"http://127.0.0.1:9/hook","events":["license.deleted"]}`, nil)
	if code != http.StatusCreated {
		t.Fatalf("subscribing to license.deleted: %d %v", code, out)
	}
	hookID := out["data"].(map[string]any)["id"].(string)
	del := func(id string, key *model.APIKey) (int, map[string]any) {
		return f.call(h.DeleteLicense, http.MethodDelete, "/admin/licenses/"+id, "", key, gin.Param{Key: "id", Value: id})
	}
	sent := func() (int, string) {
		t.Helper()
		var n int
		var payload string
		if err := f.s.DB.NewRaw("SELECT count(*), coalesce(max(payload::text), '') FROM webhook_deliveries WHERE webhook_id = ? AND event = ?",
			hookID, model.EventLicenseDeleted).Scan(f.ctx, &n, &payload); err != nil {
			t.Fatal(err)
		}
		return n, payload
	}
	auditLines := func() int {
		var n int
		_ = f.s.DB.NewRaw("SELECT count(*) FROM audit_logs WHERE entity = 'license' AND entity_id = ? AND action = 'deleted'", f.licA.ID).Scan(f.ctx, &n)
		return n
	}
	lic := f.licA

	if code, out := del(lic.ID, nil); code != http.StatusConflict || errCode(out) != "LICENSE_NOT_REVOKED" {
		t.Fatalf("an active licence = %d %v, want 409 LICENSE_NOT_REVOKED", code, out)
	}
	if err := f.s.RevokeLicense(f.ctx, lic.ID); err != nil {
		t.Fatal(err)
	}

	boundB := &model.APIKey{ProductID: f.prodB.ID, Scopes: []string{model.ScopeAdmin}}
	if code, out := del(lic.ID, boundB); code != http.StatusForbidden || errCode(out) != "PRODUCT_SCOPE_MISMATCH" {
		t.Fatalf("a key bound to another product = %d %v, want 403 PRODUCT_SCOPE_MISMATCH", code, out)
	}
	if _, err := f.s.FindLicenseByID(f.ctx, lic.ID); err != nil {
		t.Fatalf("refused delete removed the licence: %v", err)
	}

	if code, out := del(lic.ID, nil); code != http.StatusNoContent {
		t.Fatalf("delete = %d %v, want 204", code, out)
	}
	var pid, changes sql.NullString
	for range 30 {
		if err := f.s.DB.NewRaw("SELECT product_id, changes::text FROM audit_logs WHERE entity = 'license' AND entity_id = ? AND action = 'deleted'",
			lic.ID).Scan(f.ctx, &pid, &changes); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if pid.String != f.prodA.ID {
		t.Errorf("audit product %q, want %q (the licence is gone when the writer looks)", pid.String, f.prodA.ID)
	}
	var ch map[string]any
	_ = json.Unmarshal([]byte(changes.String), &ch)
	if ch["key_hint"] != lic.LicenseKey[len(lic.LicenseKey)-4:] || ch["email"] != lic.Email || strings.Contains(changes.String, lic.LicenseKey) {
		t.Errorf("audit changes %s: want the email and key hint, never the key", changes.String)
	}
	n, payload := sent()
	if n != 1 || !strings.Contains(payload, lic.ID) || !strings.Contains(payload, lic.Email) || !strings.Contains(payload, lic.PlanID) ||
		!strings.Contains(payload, `"status": "revoked"`) || strings.Contains(payload, lic.LicenseKey) {
		t.Errorf("license.deleted deliveries %d, payload %s", n, payload)
	}

	if code, out := del(lic.ID, nil); code != http.StatusNotFound {
		t.Errorf("second delete = %d %v, want 404", code, out)
	}
	time.Sleep(1500 * time.Millisecond) // the audit writer is asynchronous
	if lines := auditLines(); lines != 1 {
		t.Errorf("%d audit lines after a second delete, want 1", lines)
	}
	if n, _ := sent(); n != 1 {
		t.Errorf("%d license.deleted deliveries after a second delete, want 1", n)
	}

	if err := f.s.RevokeLicense(f.ctx, f.licB.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.DB.NewRaw("UPDATE licenses SET stripe_customer_id = 'cus_x_' || id WHERE id = ?", f.licB.ID).Exec(f.ctx); err != nil {
		t.Fatal(err)
	}
	if code, out := del(f.licB.ID, nil); code != http.StatusConflict || errCode(out) != "LICENSE_NOT_DELETABLE" {
		t.Errorf("a licence with a payment on record = %d %v, want 409 LICENSE_NOT_DELETABLE", code, out)
	}
	if _, err := f.s.FindLicenseByID(f.ctx, f.licB.ID); err != nil {
		t.Errorf("that licence was deleted: %v", err)
	}
}
