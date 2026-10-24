package handler

import (
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/tabloy/keygate/internal/model"
)

// A product's license prefix starts the keys it issues from then on.
// Keys already issued keep theirs and keep working, and the admin API
// shows each key's own prefix, not the product's current one.
func TestLicensePrefix(t *testing.T) {
	f := newAPIFixture(t)
	h := &AdminHandler{Store: f.s}
	data := func(out map[string]any) map[string]any {
		d, _ := out["data"].(map[string]any)
		return d
	}

	// Creating a product: the prefix is uppercased, KG when left out,
	// and refused when it is not 2 to 8 letters or digits.
	createProduct := func(slug, prefix string) (int, map[string]any) {
		body := `{"name":"Prefix","slug":"pfx-` + slug + `-` + f.suffix + `","type":"desktop"`
		if prefix != "" {
			body += `,"license_prefix":"` + prefix + `"`
		}
		return f.call(h.CreateProduct, http.MethodPost, "/admin/products", body+`}`, nil)
	}
	if code, out := createProduct("lower", " acme "); code != http.StatusCreated || data(out)["license_prefix"] != "ACME" {
		t.Fatalf("create with \" acme \" = %d %v, want 201 with ACME", code, out)
	}
	if code, out := createProduct("none", ""); code != http.StatusCreated || data(out)["license_prefix"] != "KG" {
		t.Fatalf("create without a prefix = %d %v, want 201 with KG", code, out)
	}
	for i, bad := range []string{"A", "ABCDEFGHI", "AC-ME", "ÄB"} {
		if code, out := createProduct("bad"+string(rune('a'+i)), bad); code != http.StatusBadRequest {
			t.Errorf("create with %q = %d %v, want 400", bad, code, out)
		}
	}

	issue := func() (id, key string) {
		t.Helper()
		body := `{"product_id":"` + f.prodA.ID + `","plan_id":"` + f.planA.ID + `","email":"pfx-` + f.suffix + `@example.com"}`
		code, out := f.call(h.CreateLicense, http.MethodPost, "/admin/licenses", body, nil)
		if code != http.StatusCreated {
			t.Fatalf("create license = %d %v", code, out)
		}
		id, _ = data(out)["id"].(string)
		l, err := f.s.FindLicenseByID(f.ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return id, f.s.DecryptLicenseKey(l)
	}
	updateProduct := func(body string) (int, map[string]any) {
		return f.call(h.UpdateProduct, http.MethodPut, "/admin/products/"+f.prodA.ID, body, nil,
			gin.Param{Key: "id", Value: f.prodA.ID})
	}
	prefixNow := func() string {
		t.Helper()
		p, err := f.s.FindProductByID(f.ctx, f.prodA.ID)
		if err != nil {
			t.Fatal(err)
		}
		return p.LicensePrefix
	}

	oldID, oldKey := issue()
	if !strings.HasPrefix(oldKey, "KG-") {
		t.Fatalf("key of a product left at the default = %s, want KG-", oldKey)
	}
	if code, out := updateProduct(`{"license_prefix":"acme2"}`); code != http.StatusOK || data(out)["license_prefix"] != "ACME2" {
		t.Fatalf("change prefix to acme2 = %d %v, want 200 with ACME2", code, out)
	}
	if code, _ := updateProduct(`{"license_prefix":"a"}`); code != http.StatusBadRequest || prefixNow() != "ACME2" {
		t.Fatalf("change prefix to a = %d, prefix now %s; want 400 and ACME2 kept", code, prefixNow())
	}
	if code, _ := updateProduct(`{"name":"Renamed"}`); code != http.StatusOK || prefixNow() != "ACME2" {
		t.Fatalf("rename = %d, prefix now %s; want 200 and ACME2 kept", code, prefixNow())
	}
	newID, newKey := issue()
	if !strings.HasPrefix(newKey, "ACME2-") {
		t.Fatalf("key issued after the change = %s, want ACME2-", newKey)
	}
	if l, err := f.s.FindLicenseByKey(f.ctx, oldKey); err != nil || l.ID != oldID {
		t.Fatalf("key issued before the change no longer found: %v", err)
	}

	// The detail and the list show each key's own prefix. The fixture's
	// license has a key in another format, so it gets no prefix.
	code, out := f.call(h.GetLicense, http.MethodGet, "/admin/licenses/"+oldID, "", nil, gin.Param{Key: "id", Value: oldID})
	if d := data(out); code != http.StatusOK || d["license_key_prefix"] != "KG" || d["license_key_hint"] != oldKey[len(oldKey)-4:] {
		t.Fatalf("detail of the older license = %d %v, want prefix KG and its last four", code, d)
	}
	code, out = f.call(h.ListLicenses, http.MethodGet, "/admin/licenses?product_id="+f.prodA.ID, "", nil)
	prefixes, _ := data(out)["license_key_prefixes"].(map[string]any)
	if code != http.StatusOK || prefixes[oldID] != "KG" || prefixes[newID] != "ACME2" {
		t.Fatalf("list prefixes = %d %v, want KG and ACME2", code, prefixes)
	}
	if p, ok := prefixes[f.licA.ID]; ok {
		t.Fatalf("imported key got prefix %v, want none", p)
	}
}

// A key claimed through self sign-up starts with the prefix of its
// product too.
func TestSelfSignup_UsesProductPrefix(t *testing.T) {
	f := newSignupFixture(t, nil)
	if _, err := f.s.DB.NewUpdate().Model((*model.Product)(nil)).Set("license_prefix = 'FREE'").
		Where("id = ?", f.plan.ProductID).Exec(f.ctx); err != nil {
		t.Fatal(err)
	}
	email := "prefix-" + f.plan.CheckoutID + "@example.com"
	if w, _ := f.verify(email, f.code(email), false); w.Code != http.StatusCreated {
		t.Fatalf("claim: %d %s", w.Code, w.Body.String())
	}
	ls := f.licences(email)
	if len(ls) != 1 {
		t.Fatalf("%d licences after the claim, want 1", len(ls))
	}
	if key := f.s.DecryptLicenseKey(&ls[0]); !strings.HasPrefix(key, "FREE-") {
		t.Fatalf("key claimed through self sign-up = %s, want FREE-", key)
	}
}
