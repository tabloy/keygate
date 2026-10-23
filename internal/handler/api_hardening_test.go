package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/service"
	"github.com/tabloy/keygate/internal/store"
)

// apiFixture is a store with two products, each with a plan and a
// licence, for the admin API checks below.
type apiFixture struct {
	t      *testing.T
	s      *store.Store
	ctx    context.Context
	suffix string
	prodA  *model.Product
	prodB  *model.Product
	planA  *model.Plan
	planB  *model.Plan
	licA   *model.License
	licB   *model.License
}

func newAPIFixture(t *testing.T) *apiFixture {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("skipping integration test: TEST_DATABASE_URL not set")
	}
	s, err := store.New(dsn)
	if err != nil {
		t.Skipf("skipping integration test: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.RunMigrations("../../db/migrations"); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	gin.SetMode(gin.TestMode)
	f := &apiFixture{t: t, s: s, ctx: context.Background(), suffix: strings.ReplaceAll(time.Now().Format("150405.000000"), ".", "")}
	mk := func(name string) (*model.Product, *model.Plan, *model.License) {
		p := &model.Product{Name: name, Slug: "api-" + name + "-" + f.suffix, Type: "desktop"}
		if err := s.CreateProduct(f.ctx, p); err != nil {
			t.Fatalf("create product: %v", err)
		}
		pl := &model.Plan{ProductID: p.ID, Name: "Std", Slug: "api-std-" + name + "-" + f.suffix, LicenseType: "perpetual",
			LicenseModel: "standard", MaxActivations: 3, Active: true}
		if err := s.CreatePlan(f.ctx, pl); err != nil {
			t.Fatalf("create plan: %v", err)
		}
		l := &model.License{ProductID: p.ID, PlanID: pl.ID, Email: name + "-" + f.suffix + "@example.com",
			LicenseKey: "KEY-API-" + name + "-" + f.suffix, Status: model.StatusActive}
		if err := s.CreateLicense(f.ctx, l); err != nil {
			t.Fatalf("create license: %v", err)
		}
		return p, pl, l
	}
	f.prodA, f.planA, f.licA = mk("a")
	f.prodB, f.planB, f.licB = mk("b")
	return f
}

// call runs a handler with a JSON body and returns the status and the
// decoded envelope. key, when set, is the API key the request came with.
func (f *apiFixture) call(fn gin.HandlerFunc, method, target, body string, key *model.APIKey, params ...gin.Param) (int, map[string]any) {
	f.t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(method, target, strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = params
	if key != nil {
		c.Set("api_key", key)
	}
	fn(c)
	out := map[string]any{}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	// c.Writer, not the recorder: a 204 writes no body, so the recorder
	// never sees its status.
	return c.Writer.Status(), out
}

func errCode(out map[string]any) string {
	e, _ := out["error"].(map[string]any)
	code, _ := e["code"].(string)
	return code
}

// The export carries every plaintext key, so a key bound to one product
// exports that product's licences only, like the list does.
func TestExportLicensesHonoursKeyBinding(t *testing.T) {
	f := newAPIFixture(t)
	h := &AdminHandler{Store: f.s}
	bound := &model.APIKey{ProductID: f.prodA.ID, Scopes: []string{model.ScopeLicensesWrite}}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/admin/licenses/export?format=json", nil)
	c.Set("api_key", bound)
	h.ExportLicenses(c)
	if w.Code != http.StatusOK {
		t.Fatalf("bound export = %d: %s", w.Code, w.Body.String())
	}
	var rows []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
		var env struct {
			Data []map[string]any `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
			t.Fatalf("decode export: %v: %s", err, w.Body.String())
		}
		rows = env.Data
	}
	if len(rows) == 0 {
		t.Fatalf("bound export returned no rows: %s", w.Body.String())
	}
	for _, r := range rows {
		if r["license_key"] == f.licB.LicenseKey || r["id"] == f.licB.ID {
			t.Fatalf("bound key exported another product's licence: %v", r)
		}
	}

	code, out := f.call(h.ExportLicenses, http.MethodGet, "/admin/licenses/export?format=json&product_id="+f.prodB.ID, "", bound)
	if code != http.StatusForbidden || errCode(out) != "PRODUCT_SCOPE_MISMATCH" {
		t.Fatalf("bound export of another product = %d %v, want 403 PRODUCT_SCOPE_MISMATCH", code, out)
	}
}

// An admin key reaches every product, so it cannot be bound to one;
// the narrow scopes still can.
func TestAdminKeyCannotBeBound(t *testing.T) {
	f := newAPIFixture(t)
	h := &AdminHandler{Store: f.s}
	code, _ := f.call(h.CreateAPIKey, http.MethodPost, "/admin/api-keys",
		`{"name":"bound admin","product_id":"`+f.prodA.ID+`","scopes":["admin"]}`, nil)
	if code != http.StatusBadRequest {
		t.Fatalf("bound admin key = %d, want 400", code)
	}
	code, out := f.call(h.CreateAPIKey, http.MethodPost, "/admin/api-keys",
		`{"name":"bound ci","product_id":"`+f.prodA.ID+`","scopes":["licenses:write"]}`, nil)
	if code != http.StatusCreated {
		t.Fatalf("bound licenses:write key = %d %v, want 201", code, out)
	}
	id, _ := out["data"].(map[string]any)["id"].(string)
	if code, _ := f.call(h.DeleteAPIKey, http.MethodDelete, "/admin/api-keys/"+id, "", nil, gin.Param{Key: "id", Value: id}); code != http.StatusNoContent {
		t.Fatalf("delete key = %d, want 204", code)
	}
	if code, _ := f.call(h.DeleteAPIKey, http.MethodDelete, "/admin/api-keys/"+id, "", nil, gin.Param{Key: "id", Value: id}); code != http.StatusNotFound {
		t.Fatalf("delete missing key = %d, want 404", code)
	}
}

// An addon attaches only to a licence of its own product, an unknown one
// is a 404 rather than a 500, and attaching twice returns the stored row.
func TestAddLicenseAddonValidates(t *testing.T) {
	f := newAPIFixture(t)
	h := &AdminHandler{Store: f.s}
	mkAddon := func(p *model.Product, feature string) *model.Addon {
		a := &model.Addon{ProductID: p.ID, Name: feature, Slug: feature + "-" + f.suffix, Feature: feature,
			ValueType: "bool", Value: "true", Active: true}
		if err := f.s.CreateAddon(f.ctx, a); err != nil {
			t.Fatalf("create addon: %v", err)
		}
		return a
	}
	own, other := mkAddon(f.prodA, "own"), mkAddon(f.prodB, "other")
	lic := gin.Param{Key: "id", Value: f.licA.ID}
	post := func(addonID string) (int, map[string]any) {
		return f.call(h.AddLicenseAddon, http.MethodPost, "/admin/licenses/x/addons", `{"addon_id":"`+addonID+`"}`, nil, lic)
	}

	if code, out := post("no-such-addon"); code != http.StatusNotFound {
		t.Errorf("unknown addon = %d %v, want 404", code, out)
	}
	if code, out := post(other.ID); code != http.StatusBadRequest || errCode(out) != "ADDON_PRODUCT_MISMATCH" {
		t.Errorf("other product's addon = %d %v, want 400 ADDON_PRODUCT_MISMATCH", code, out)
	}
	code, out := post(own.ID)
	if code != http.StatusCreated {
		t.Fatalf("attach = %d %v, want 201", code, out)
	}
	firstID := out["data"].(map[string]any)["id"]
	code, out = post(own.ID)
	if code != http.StatusOK || out["data"].(map[string]any)["id"] != firstID {
		t.Errorf("attach again = %d %v, want 200 with id %v", code, out, firstID)
	}
	addons, _, err := f.s.ListLicenseAddons(f.ctx, f.licA.ID, store.All)
	if err != nil || len(addons) != 1 || addons[0].ID != firstID {
		t.Errorf("stored attachments = %v (%v), want one with id %v", addons, err, firstID)
	}

	rm := func() int {
		code, _ := f.call(h.RemoveLicenseAddon, http.MethodDelete, "/", "", nil, lic, gin.Param{Key: "addon_id", Value: own.ID})
		return code
	}
	if code := rm(); code != http.StatusNoContent {
		t.Errorf("remove = %d, want 204", code)
	}
	if code := rm(); code != http.StatusNotFound {
		t.Errorf("remove again = %d, want 404", code)
	}
}

// A duplicate on update is a 409 as on create, and a missing parent a
// 404, never a 500.
func TestCatalogConflictsAreNotInternal(t *testing.T) {
	f := newAPIFixture(t)
	h := &AdminHandler{Store: f.s, FeedURLTTL: time.Hour}
	id := func(v string) gin.Param { return gin.Param{Key: "id", Value: v} }

	if code, out := f.call(h.UpdateProduct, http.MethodPut, "/", `{"slug":"`+f.prodB.Slug+`"}`, nil, id(f.prodA.ID)); code != http.StatusConflict || errCode(out) != "DUPLICATE" {
		t.Errorf("product slug taken = %d %v, want 409 DUPLICATE", code, out)
	}
	other := &model.Plan{ProductID: f.prodA.ID, Name: "Other", Slug: "api-other-" + f.suffix, LicenseType: "perpetual",
		LicenseModel: "standard", MaxActivations: 1, Active: true}
	if err := f.s.CreatePlan(f.ctx, other); err != nil {
		t.Fatal(err)
	}
	if code, out := f.call(h.UpdatePlan, http.MethodPut, "/", `{"slug":"`+f.planA.Slug+`"}`, nil, id(other.ID)); code != http.StatusConflict || errCode(out) != "DUPLICATE" {
		t.Errorf("plan slug taken = %d %v, want 409 DUPLICATE", code, out)
	}

	ent := func(feature string) string {
		code, out := f.call(h.CreateEntitlement, http.MethodPost, "/",
			`{"plan_id":"`+f.planA.ID+`","feature":"`+feature+`","value_type":"bool","value":"true"}`, nil)
		if code != http.StatusCreated {
			t.Fatalf("create entitlement %s = %d %v", feature, code, out)
		}
		return out["data"].(map[string]any)["id"].(string)
	}
	ent("one")
	two := ent("two")
	if code, out := f.call(h.UpdateEntitlement, http.MethodPut, "/", `{"feature":"one"}`, nil, id(two)); code != http.StatusConflict || errCode(out) != "DUPLICATE" {
		t.Errorf("entitlement feature taken = %d %v, want 409 DUPLICATE", code, out)
	}
	if code, out := f.call(h.CreateEntitlement, http.MethodPost, "/",
		`{"plan_id":"no-such-plan","feature":"x","value_type":"bool","value":"true"}`, nil); code != http.StatusNotFound {
		t.Errorf("entitlement on unknown plan = %d %v, want 404", code, out)
	}
	if code, _ := f.call(h.DeleteEntitlement, http.MethodDelete, "/", "", nil, id("no-such-entitlement")); code != http.StatusNotFound {
		t.Errorf("delete unknown entitlement = %d, want 404", code)
	}
	if code, out := f.call(h.CreateAddon, http.MethodPost, "/",
		`{"product_id":"no-such-product","name":"X","slug":"x-`+f.suffix+`","feature":"x","value_type":"bool","value":"true"}`, nil); code != http.StatusNotFound {
		t.Errorf("addon on unknown product = %d %v, want 404", code, out)
	}
	if code, _ := f.call(h.DeleteAddon, http.MethodDelete, "/", "", nil, id("no-such-addon")); code != http.StatusNotFound {
		t.Errorf("delete unknown addon = %d, want 404", code)
	}
	if code, _ := f.call(h.DeletePlan, http.MethodDelete, "/", "", nil, id("no-such-plan")); code != http.StatusNotFound {
		t.Errorf("delete unknown plan = %d, want 404", code)
	}
	if code, _ := f.call(h.DeleteProduct, http.MethodDelete, "/", "", nil, id("no-such-product")); code != http.StatusNotFound {
		t.Errorf("delete unknown product = %d, want 404", code)
	}
}

// A webhook subscribes only to events that are sent, for a product that
// exists.
func TestWebhookSubscriptionValidated(t *testing.T) {
	f := newAPIFixture(t)
	h := &WebhookAdminHandler{Store: f.s, Webhook: service.NewWebhookService(f.s, slog.Default(), time.Second, 1, false)}
	create := func(product, url, events string) (int, map[string]any) {
		return f.call(h.CreateWebhook, http.MethodPost, "/",
			`{"product_id":"`+product+`","url":"`+url+`","events":`+events+`}`, nil)
	}
	if code, out := create(f.prodA.ID, "https://example.com/h", `["nope.event"]`); code != http.StatusBadRequest {
		t.Errorf("unknown event = %d %v, want 400", code, out)
	}
	if code, out := create("no-such-product", "https://example.com/h", `["license.created"]`); code != http.StatusNotFound {
		t.Errorf("unknown product = %d %v, want 404", code, out)
	}
	for _, host := range []string{"2130706433", "0x7f000001", "127.1"} {
		if code, out := create(f.prodA.ID, "http://"+host+"/h", `["license.created"]`); code != http.StatusBadRequest {
			t.Errorf("numeric host %s = %d %v, want 400", host, code, out)
		}
	}
	code, out := create(f.prodA.ID, "https://0xbeef.example.com/h", `["release.published","license.updates_changed"]`)
	if code != http.StatusCreated {
		t.Fatalf("valid webhook = %d %v, want 201", code, out)
	}
	whID := out["data"].(map[string]any)["id"].(string)
	if code, out := f.call(h.UpdateWebhook, http.MethodPut, "/", `{"events":["bogus"]}`, nil, gin.Param{Key: "id", Value: whID}); code != http.StatusBadRequest {
		t.Errorf("update to unknown event = %d %v, want 400", code, out)
	}
}

// The SDK endpoints refuse input that used to reach the database: an
// unknown identifier_type (a 500 from the CHECK constraint), an
// identifier of only spaces (stored as "" and holding a slot) and an
// unbounded label. These checks run before the service is called.
func TestActivateInputChecks(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &LicenseHandler{}
	fl := &FloatingHandler{}
	for _, tc := range []struct {
		name string
		fn   gin.HandlerFunc
		body string
	}{
		{"unknown identifier_type", h.Activate, `{"license_key":"K","identifier":"d","identifier_type":"bogus"}`},
		{"identifier_type in capitals", h.Activate, `{"license_key":"K","identifier":"d","identifier_type":"DEVICE"}`},
		{"blank identifier on activate", h.Activate, `{"license_key":"K","identifier":"   "}`},
		{"blank identifier on verify", h.Verify, `{"license_key":"K","identifier":"   "}`},
		{"blank identifier on deactivate", h.Deactivate, `{"license_key":"K","identifier":"\t"}`},
		{"label too long", h.Activate, `{"license_key":"K","identifier":"d","label":"` + strings.Repeat("x", 257) + `"}`},
		{"blank identifier on checkout", fl.CheckOut, `{"license_key":"K","identifier":"  "}`},
		{"blank identifier on heartbeat", fl.Heartbeat, `{"license_key":"K","identifier":"  "}`},
		{"blank identifier on checkin", fl.CheckIn, `{"license_key":"K","identifier":"  "}`},
	} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tc.body))
		c.Request.Header.Set("Content-Type", "application/json")
		tc.fn(c)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s = %d %s, want 400", tc.name, w.Code, w.Body.String())
		}
	}
}

// "Cancel and revoke" changes nothing when Stripe cannot confirm the
// cancel, puts an upgraded licence back on its free plan instead of
// revoking it, and revokes any other.
func TestCancelAndRevoke(t *testing.T) {
	f := newAPIFixture(t)
	setSub := func(l *model.License, sub string) {
		l.StripeSubscriptionID = sub
		l.PaymentProvider = "stripe"
		if err := f.s.UpdateLicense(f.ctx, l, "stripe_subscription_id", "payment_provider"); err != nil {
			t.Fatal(err)
		}
	}
	setSub(f.licA, "sub_cr_"+f.suffix)
	var outcome struct {
		reverted bool
		err      error
	}
	calls := 0
	h := &AdminHandler{Store: f.s, CancelSubscription: func(ctx context.Context, lic *model.License) (bool, error) {
		calls++
		return outcome.reverted, outcome.err
	}}
	status := func() string {
		l, err := f.s.FindLicenseByID(f.ctx, f.licA.ID)
		if err != nil {
			t.Fatal(err)
		}
		return l.Status
	}
	refund := func() (int, map[string]any) {
		return f.call(h.RefundLicense, http.MethodPost, "/", "", nil, gin.Param{Key: "id", Value: f.licA.ID})
	}

	outcome.err = errors.New("stripe down")
	if code, out := refund(); code != http.StatusBadGateway || status() != model.StatusActive {
		t.Fatalf("cancel failed = %d %v, status %s; want 502 and the licence untouched", code, out, status())
	}
	outcome.err, outcome.reverted = nil, true
	if code, out := refund(); code != http.StatusOK || out["data"].(map[string]any)["status"] != "reverted_to_free_plan" || status() != model.StatusActive {
		t.Fatalf("upgraded licence = %d %v, status %s; want 200 reverted_to_free_plan and not revoked", code, out, status())
	}
	outcome.reverted = false
	if code, out := refund(); code != http.StatusOK || status() != model.StatusRevoked {
		t.Fatalf("plain subscription = %d %v, status %s; want 200 and revoked", code, out, status())
	}
	if calls != 3 {
		t.Errorf("cancel hook called %d times, want 3", calls)
	}
	// Revoking twice changes nothing more.
	if code, out := refund(); code != http.StatusOK || status() != model.StatusRevoked {
		t.Fatalf("second cancel = %d %v, status %s; want 200 and still revoked", code, out, status())
	}

	// The upgrade was reverted by a subscription.deleted webhook between
	// the handler's read and the cancel, so the cancel found nothing to
	// revert. The licence is free again and must not be revoked.
	setSub(f.licB, "sub_race_"+f.suffix)
	raced := &AdminHandler{Store: f.s, CancelSubscription: func(ctx context.Context, lic *model.License) (bool, error) {
		l, err := f.s.FindLicenseByID(ctx, lic.ID)
		if err != nil {
			return false, err
		}
		l.StripeSubscriptionID, l.PaymentProvider = "", ""
		if err := f.s.UpdateLicense(ctx, l, "stripe_subscription_id", "payment_provider"); err != nil {
			return false, err
		}
		return false, nil
	}}
	code, out := f.call(raced.RefundLicense, http.MethodPost, "/", "", nil, gin.Param{Key: "id", Value: f.licB.ID})
	if l, _ := f.s.FindLicenseByID(f.ctx, f.licB.ID); code != http.StatusConflict || errCode(out) != "LICENSE_CHANGED" || l.Status != model.StatusActive {
		t.Fatalf("cancel after a concurrent revert = %d %v, status %s; want 409 LICENSE_CHANGED and still active", code, out, l.Status)
	}

	noStripe := &AdminHandler{Store: f.s}
	setSub(f.licB, "sub_cr2_"+f.suffix)
	if code, out := f.call(noStripe.RefundLicense, http.MethodPost, "/", "", nil, gin.Param{Key: "id", Value: f.licB.ID}); code != http.StatusServiceUnavailable || errCode(out) != "STRIPE_NOT_CONFIGURED" {
		t.Errorf("without Stripe = %d %v, want 503 STRIPE_NOT_CONFIGURED", code, out)
	}
}

// Settings nothing reads are refused, and the branding values public
// /config serves must be a URL or path and a hex colour.
func TestSettingsRefuseDeadKeysAndBadBranding(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		key, val string
		ok       bool
	}{
		{"logo_url", "javascript:alert(1)", false},
		{"logo_url", "//evil.example/x.png", false},
		{"logo_url", "https://cdn.example.com/logo.png", true},
		{"logo_url", "/static/logo.png", true},
		{"logo_url", "", true},
		{"brand_color", "red;background:url(x)", false},
		{"brand_color", "#7c3aed", true},
		{"brand_color", "#abc", true},
	} {
		if got := brandingProblem(tc.key, tc.val) == ""; got != tc.ok {
			t.Errorf("%s=%q accepted %v, want %v", tc.key, tc.val, got, tc.ok)
		}
	}
	for _, key := range []string{"rate_limit_api", "rate_limit_admin", "webhook_max_attempts", "webhook_timeout", "quota_warning_threshold"} {
		if settingsWritable[key] {
			t.Errorf("%s is writable but nothing reads it", key)
		}
	}
}
