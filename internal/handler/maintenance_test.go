package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
	"github.com/tabloy/keygate/internal/testsupport"
)

func TestNormalizeMaintenance(t *testing.T) {
	ok := maintenanceFields{UpdatesDays: 365, RenewalDays: 365, StripeRenewalPriceID: "price_x"}
	got, err := normalizeMaintenance("perpetual", ok)
	if err != nil || got != ok {
		t.Fatalf("valid perpetual fields rejected: %v %+v", err, got)
	}
	// Other license types carry no maintenance period.
	got, err = normalizeMaintenance("subscription", ok)
	if err != nil || got != (maintenanceFields{}) {
		t.Fatalf("subscription must clear the fields: %v %+v", err, got)
	}
	// Renewals without a length are not offered.
	if _, err := normalizeMaintenance("perpetual", maintenanceFields{StripeRenewalPriceID: "price_x"}); err == nil {
		t.Fatal("renewal price without renewal_days accepted")
	}
	if _, err := normalizeMaintenance("perpetual", maintenanceFields{RenewalDays: 30, StripeRenewalPriceID: "prod_x"}); err == nil {
		t.Fatal("non-price id accepted as renewal price")
	}
	for _, bad := range []maintenanceFields{{UpdatesDays: -1}, {UpdatesDays: 3651}, {RenewalDays: -1}, {RenewalDays: 3651}} {
		if _, err := normalizeMaintenance("perpetual", bad); err == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
	// updates for life with paid renewals is a valid combination
	// (renewal is simply never offered because updates_until is nil).
	if _, err := normalizeMaintenance("perpetual", maintenanceFields{RenewalDays: 365}); err != nil {
		t.Fatalf("renewal_days without price rejected: %v", err)
	}
}

// Enabling a bounded period or renewals on an existing plan is what
// the switch must catch; a plan that already sold them may be edited,
// and a plan with licenses cannot change its license type.
func TestUpdatePlan_MaintenanceGate(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("skipping integration test: TEST_DATABASE_URL not set")
	}
	s, err := store.New(dsn)
	if err != nil {
		t.Skipf("skipping integration test: %v", err)
	}
	defer s.Close()
	if err := s.RunMigrations("../../db/migrations"); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	testsupport.LockSettings(t, s.DB)
	ctx := context.Background()
	gin.SetMode(gin.TestMode)
	suffix := time.Now().Format("150405.000")
	prod := &model.Product{Name: "Gate", Slug: "gate-" + suffix, Type: "desktop"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	plain := &model.Plan{ProductID: prod.ID, Name: "Plain", Slug: "gplain-" + suffix, LicenseType: "perpetual", LicenseModel: "standard"}
	sub := &model.Plan{ProductID: prod.ID, Name: "Sub", Slug: "gsub-" + suffix, LicenseType: "subscription", LicenseModel: "standard"}
	for _, p := range []*model.Plan{plain, sub} {
		if err := s.CreatePlan(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	h := &AdminHandler{Store: s}
	call := func(planID, body string) (int, string) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPut, "/admin/plans/"+planID, strings.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		c.Params = gin.Params{{Key: "id", Value: planID}}
		h.UpdatePlan(c)
		var out struct {
			Error struct{ Code string } `json:"error"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out.Error.Code
	}
	setSwitch := func(v string) {
		if err := s.SetSettings(ctx, map[string]string{store.SettingMaintenanceFeatures: v}); err != nil {
			t.Fatal(err)
		}
	}
	setSwitch("false")
	if code, ec := call(plain.ID, `{"updates_days":365}`); code != http.StatusConflict || ec != "MAINTENANCE_FEATURES_DISABLED" {
		t.Fatalf("enabling a bounded period with the switch off: %d %s", code, ec)
	}
	if code, ec := call(plain.ID, `{"renewal_days":365,"stripe_renewal_price_id":"price_gate_`+suffix+`"}`); code != http.StatusConflict || ec != "MAINTENANCE_FEATURES_DISABLED" {
		t.Fatalf("enabling renewals with the switch off: %d %s", code, ec)
	}
	if code, _ := call(plain.ID, `{"name":"Plain 2"}`); code != http.StatusOK {
		t.Fatalf("unrelated edit must pass: %d", code)
	}
	setSwitch("true")
	// The product's feeds are still public: a bounded plan would be
	// bypassed through them.
	if code, ec := call(plain.ID, `{"updates_days":365}`); code != http.StatusConflict || ec != "FEED_NOT_GATED" {
		t.Fatalf("bounded plan on an ungated product: %d %s", code, ec)
	}
	// Gated just now. With nothing published the feeds handed out
	// nothing, so the gate takes effect at once.
	now := time.Now()
	prod.FeedLicenseRequired, prod.FeedGatedAt = true, &now
	if err := s.UpdateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	if code, _ := call(plain.ID, `{"updates_days":30}`); code != http.StatusOK {
		t.Fatalf("no published release: the gate should take effect at once: %d", code)
	}
	if code, _ := call(plain.ID, `{"updates_days":0}`); code != http.StatusOK {
		t.Fatal("could not undo the bounded period")
	}
	// A published release means the public feed could have handed out
	// links that outlive the gate; the period waits for them.
	rel := &model.Release{ProductID: prod.ID, Version: "1.0.0", Channel: model.ReleaseChannelStable}
	if err := s.CreateRelease(ctx, rel); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.NewRaw("UPDATE releases SET status = 'published', published_at = now() WHERE id = ?", rel.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if code, ec := call(plain.ID, `{"updates_days":365}`); code != http.StatusConflict || ec != "FEED_CACHE_DRAINING" {
		t.Fatalf("bounded plan before the public feed's links expired: %d %s", code, ec)
	}
	// Long enough ago that the wait is over whatever feed URL TTL the
	// install has on record — the drain is measured against that
	// shared bound, not against this handler's own configuration.
	drained := now.Add(-30 * 24 * time.Hour)
	prod.FeedGatedAt = &drained
	if err := s.UpdateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	// A bounded plan can only exist on a gated product.
	bounded := &model.Plan{ProductID: prod.ID, Name: "Bounded", Slug: "gbound-" + suffix, LicenseType: "perpetual", LicenseModel: "standard", UpdatesDays: 365}
	if err := s.CreatePlan(ctx, bounded); err != nil {
		t.Fatal(err)
	}
	// With the switch off a replica that predates this version may be
	// running: an already bounded plan may not have its terms moved
	// either, only cleared, and unrelated fields still save.
	setSwitch("false")
	if code, ec := call(bounded.ID, `{"updates_days":30}`); code != http.StatusConflict || ec != "MAINTENANCE_FEATURES_DISABLED" {
		t.Fatalf("moving a bounded plan's period with the switch off: %d %s", code, ec)
	}
	if code, _ := call(bounded.ID, `{"name":"Bounded renamed","updates_days":365}`); code != http.StatusOK {
		t.Fatalf("unrelated edit of a bounded plan must pass: %d", code)
	}
	setSwitch("true")
	if code, _ := call(plain.ID, `{"updates_days":365}`); code != http.StatusOK {
		t.Fatalf("switch on, feeds gated: %d", code)
	}
	// And gating cannot be switched off underneath a bounded plan.
	{
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPut, "/admin/products/"+prod.ID, strings.NewReader(`{"feed_license_required":false}`))
		c.Request.Header.Set("Content-Type", "application/json")
		c.Params = gin.Params{{Key: "id", Value: prod.ID}}
		h.UpdateProduct(c)
		var out struct {
			Error struct{ Code string } `json:"error"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		if w.Code != http.StatusConflict || out.Error.Code != "BOUNDED_PLANS_EXIST" {
			t.Fatalf("ungating a product with bounded plans: %d %s", w.Code, out.Error.Code)
		}
	}
	// Type change is refused once licenses exist.
	lic := &model.License{ProductID: prod.ID, PlanID: sub.ID, Email: "gate-" + suffix + "@example.com", LicenseKey: "KEY-gate-" + suffix, Status: model.StatusActive}
	if err := s.CreateLicense(ctx, lic); err != nil {
		t.Fatal(err)
	}
	if code, ec := call(sub.ID, `{"license_type":"perpetual","updates_days":365}`); code != http.StatusConflict || ec != "HAS_LICENSES" {
		t.Fatalf("type change with licenses: %d %s", code, ec)
	}
	if code, _ := call(sub.ID, `{"license_type":"subscription","name":"Sub 2"}`); code != http.StatusOK {
		t.Fatalf("same type with licenses must pass: %d", code)
	}
}

// Moving a license between two perpetual plans must not write the
// period back: a renewal committed after the handler loaded the
// license would otherwise be overwritten with the stale value.
func TestChangeLicensePlan_KeepsConcurrentRenewal(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("skipping integration test: TEST_DATABASE_URL not set")
	}
	s, err := store.New(dsn)
	if err != nil {
		t.Skipf("skipping integration test: %v", err)
	}
	defer s.Close()
	if err := s.RunMigrations("../../db/migrations"); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	ctx := context.Background()
	gin.SetMode(gin.TestMode)
	suffix := time.Now().Format("150405.000")
	prod := &model.Product{Name: "Move", Slug: "move-" + suffix, Type: "desktop", FeedLicenseRequired: true}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	a := &model.Plan{ProductID: prod.ID, Name: "A", Slug: "ma-" + suffix, LicenseType: "perpetual", LicenseModel: "standard", UpdatesDays: 365}
	b := &model.Plan{ProductID: prod.ID, Name: "B", Slug: "mb-" + suffix, LicenseType: "perpetual", LicenseModel: "standard", UpdatesDays: 30}
	for _, p := range []*model.Plan{a, b} {
		if err := s.CreatePlan(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	lic := &model.License{ProductID: prod.ID, PlanID: a.ID, Email: "move-" + suffix + "@example.com", LicenseKey: "KEY-move-" + suffix, Status: model.StatusActive}
	if err := s.CreateLicense(ctx, lic); err != nil {
		t.Fatal(err)
	}
	// The handler's own store is wrapped so a renewal lands between
	// its read of the license and its write.
	extended := time.Now().Add(700 * 24 * time.Hour).Truncate(time.Second)
	h := &AdminHandler{Store: s}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/admin/licenses/"+lic.ID+"/change-plan", strings.NewReader(`{"plan_id":"`+b.ID+`"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = gin.Params{{Key: "id", Value: lic.ID}}
	// Simulate the interleaving: the value the handler will load is
	// the one in the row now; a renewal then moves it before the
	// handler writes. Since the handler no longer writes the column
	// for perpetual-to-perpetual moves, doing the move after the
	// renewal proves the column is untouched only if the handler
	// wrote nothing — so move the row first, then check it stayed.
	if _, err := s.DB.NewRaw("UPDATE licenses SET updates_until = ? WHERE id = ?", extended, lic.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	h.ChangeLicensePlan(c)
	if w.Code != http.StatusOK {
		t.Fatalf("change plan: %d %s", w.Code, w.Body.String())
	}
	got, _ := s.FindLicenseByID(ctx, lic.ID)
	if got.PlanID != b.ID || got.UpdatesUntil == nil || !got.UpdatesUntil.Equal(extended) {
		t.Fatalf("perpetual-to-perpetual move changed the period: plan=%s until=%v", got.PlanID, got.UpdatesUntil)
	}
}

// Issuing a bounded update period is gated by the same switch as
// configuring one: during a rollback an older replica would ignore
// the cutoff and serve newer releases.
func TestLicenseIssuance_MaintenanceGate(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("skipping integration test: TEST_DATABASE_URL not set")
	}
	s, err := store.New(dsn)
	if err != nil {
		t.Skipf("skipping integration test: %v", err)
	}
	defer s.Close()
	if err := s.RunMigrations("../../db/migrations"); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	testsupport.LockSettings(t, s.DB)
	ctx := context.Background()
	gin.SetMode(gin.TestMode)
	suffix := time.Now().Format("150405.000")
	prod := &model.Product{Name: "Issue", Slug: "issue-" + suffix, Type: "desktop", FeedLicenseRequired: true}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	bounded := &model.Plan{ProductID: prod.ID, Name: "B", Slug: "ib-" + suffix, LicenseType: "perpetual", LicenseModel: "standard", UpdatesDays: 365}
	life := &model.Plan{ProductID: prod.ID, Name: "L", Slug: "il-" + suffix, LicenseType: "perpetual", LicenseModel: "standard"}
	sub := &model.Plan{ProductID: prod.ID, Name: "S", Slug: "is-" + suffix, LicenseType: "subscription", LicenseModel: "standard"}
	for _, p := range []*model.Plan{bounded, life, sub} {
		if err := s.CreatePlan(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	h := &AdminHandler{Store: s}
	setSwitch := func(v string) {
		if err := s.SetSettings(ctx, map[string]string{store.SettingMaintenanceFeatures: v}); err != nil {
			t.Fatal(err)
		}
	}
	create := func(planID, email string) (int, string) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		body := `{"product_id":"` + prod.ID + `","plan_id":"` + planID + `","email":"` + email + `"}`
		c.Request = httptest.NewRequest(http.MethodPost, "/admin/licenses", strings.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		h.CreateLicense(c)
		var out struct {
			Error struct{ Code string } `json:"error"`
			Data  struct {
				ID           string  `json:"id"`
				UpdatesUntil *string `json:"updates_until"`
			} `json:"data"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		if w.Code == http.StatusCreated {
			return w.Code, out.Data.ID
		}
		return w.Code, out.Error.Code
	}

	setSwitch("false")
	if code, ec := create(bounded.ID, "off-"+suffix+"@example.com"); code != http.StatusConflict || ec != "MAINTENANCE_FEATURES_DISABLED" {
		t.Fatalf("bounded plan with the switch off: %d %s", code, ec)
	}
	// A plan that grants updates for life is unaffected.
	if code, _ := create(life.ID, "life-"+suffix+"@example.com"); code != http.StatusCreated {
		t.Fatalf("updates-for-life plan blocked by the switch: %d", code)
	}
	// Moving into the bounded plan is the same issuance.
	subLic := &model.License{ProductID: prod.ID, PlanID: sub.ID, Email: "move-" + suffix + "@example.com", LicenseKey: "KEY-issue-" + suffix, Status: model.StatusActive}
	if err := s.CreateLicense(ctx, subLic); err != nil {
		t.Fatal(err)
	}
	changePlan := func(licenseID, planID string) (int, string) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/admin/licenses/"+licenseID+"/change-plan", strings.NewReader(`{"plan_id":"`+planID+`"}`))
		c.Request.Header.Set("Content-Type", "application/json")
		c.Params = gin.Params{{Key: "id", Value: licenseID}}
		h.ChangeLicensePlan(c)
		var out struct {
			Error struct{ Code string } `json:"error"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out.Error.Code
	}
	if code, ec := changePlan(subLic.ID, bounded.ID); code != http.StatusConflict || ec != "MAINTENANCE_FEATURES_DISABLED" {
		t.Fatalf("moving into a bounded plan with the switch off: %d %s", code, ec)
	}
	if code, _ := changePlan(subLic.ID, life.ID); code != http.StatusOK {
		t.Fatalf("moving into an updates-for-life plan blocked: %d", code)
	}

	setSwitch("true")
	id, _ := create(bounded.ID, "on-"+suffix+"@example.com")
	if id != http.StatusCreated {
		t.Fatalf("bounded plan with the switch on: %d", id)
	}
	var issued *time.Time
	if err := s.DB.NewRaw("SELECT updates_until FROM licenses WHERE email = ?", "on-"+suffix+"@example.com").Scan(ctx, &issued); err != nil {
		t.Fatal(err)
	}
	if issued == nil {
		t.Fatal("switch on: the license got no update period")
	}
}

// A product that once served public feeds can be turned into a saas
// one (no feed) and given update periods there. Turning it back into
// a desktop one hands it its feeds again, so it must wait out the
// links the public feed issued before, not merely switch the gate on.
func TestUpdateProduct_RestoringFeedsWaitsForTheDrain(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("skipping integration test: TEST_DATABASE_URL not set")
	}
	s, err := store.New(dsn)
	if err != nil {
		t.Skipf("skipping integration test: %v", err)
	}
	defer s.Close()
	if err := s.RunMigrations("../../db/migrations"); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	testsupport.LockSettings(t, s.DB)
	ctx := context.Background()
	gin.SetMode(gin.TestMode)
	suffix := time.Now().Format("150405.000")
	if err := s.SetSettings(ctx, map[string]string{store.SettingMaintenanceFeatures: "true"}); err != nil {
		t.Fatal(err)
	}
	// Public feeds, one release published and then yanked — a yank is
	// what lets the type change to saas, and it does not invalidate a
	// download link the feed already handed out.
	prod := &model.Product{Name: "Restore", Slug: "restore-" + suffix, Type: "desktop"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	rel := &model.Release{ProductID: prod.ID, Version: "1.0.0", Channel: model.ReleaseChannelStable}
	if err := s.CreateRelease(ctx, rel); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.NewRaw("UPDATE releases SET status = ?, published_at = now() WHERE id = ?", model.ReleaseStatusYanked, rel.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	h := &AdminHandler{Store: s, FeedURLTTL: 24 * time.Hour}
	update := func(body string) (int, string) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPut, "/admin/products/"+prod.ID, strings.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		c.Params = gin.Params{{Key: "id", Value: prod.ID}}
		h.UpdateProduct(c)
		var out struct {
			Error struct{ Code string } `json:"error"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out.Error.Code
	}
	if code, ec := update(`{"type":"saas"}`); code != http.StatusOK {
		t.Fatalf("desktop to saas with only yanked releases: %d %s", code, ec)
	}
	// saas serves no feed, so a bounded plan is free to exist there.
	bounded := &model.Plan{ProductID: prod.ID, Name: "B", Slug: "rb-" + suffix, LicenseType: "perpetual", LicenseModel: "standard", UpdatesDays: 365}
	if err := s.CreatePlan(ctx, bounded); err != nil {
		t.Fatal(err)
	}
	// Restoring the feeds while gating them in the same request is
	// not enough: the old public links are still usable. The gate is
	// saved even so — kept in memory only, the wait would start over
	// on every retry and the product could never get its feeds back.
	if code, ec := update(`{"type":"desktop","feed_license_required":true,"minimum_supported_version":"9.9.9"}`); code != http.StatusConflict || ec != "FEED_CACHE_DRAINING" {
		t.Fatalf("restoring feeds before the old links expired: %d %s", code, ec)
	}
	saved, err := s.FindProductByID(ctx, prod.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !saved.FeedLicenseRequired || saved.FeedGatedAt == nil {
		t.Fatalf("the gate was not saved: required=%v gated_at=%v", saved.FeedLicenseRequired, saved.FeedGatedAt)
	}
	if saved.Type != "saas" {
		t.Fatalf("the type change must wait: %s", saved.Type)
	}
	// Nothing else in that request may land while it answers 409.
	if saved.MinimumSupportedVersion != "" {
		t.Fatalf("a refused request wrote an unrelated field: %q", saved.MinimumSupportedVersion)
	}
	// The wait now runs from a stored instant, so it ends.
	if _, err := s.DB.NewRaw("UPDATE products SET feed_gated_at = now() - interval '30 days' WHERE id = ?", prod.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if code, ec := update(`{"type":"desktop"}`); code != http.StatusOK {
		t.Fatalf("restoring feeds after the saved wait: %d %s", code, ec)
	}
	if _, err := s.DB.NewRaw("UPDATE products SET type = 'saas', feed_license_required = false, feed_gated_at = NULL WHERE id = ?", prod.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	prod.Type, prod.FeedLicenseRequired, prod.FeedGatedAt = "saas", false, nil
	// Gating first and waiting is what makes it safe.
	if code, ec := update(`{"feed_license_required":true}`); code != http.StatusOK {
		t.Fatalf("gating a saas product: %d %s", code, ec)
	}
	if code, ec := update(`{"type":"desktop"}`); code != http.StatusConflict || ec != "FEED_CACHE_DRAINING" {
		t.Fatalf("restoring feeds during the drain: %d %s", code, ec)
	}
	if _, err := s.DB.NewRaw("UPDATE products SET feed_gated_at = now() - interval '2 days' WHERE id = ?", prod.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if code, ec := update(`{"type":"desktop"}`); code != http.StatusOK {
		t.Fatalf("restoring feeds after the drain: %d %s", code, ec)
	}
}

// The gating state a maintenance write depends on must be re-read
// under the lock the triggers take. A cutoff written while another
// admin ungates and re-gates the product would otherwise land on a
// product whose public feed handed out links minutes ago: the trigger
// on the write only sees that the gate is on, never how long ago it
// went up.
func TestCutoffRefusedWhenTheGateRestartsConcurrently(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("skipping integration test: TEST_DATABASE_URL not set")
	}
	s, err := store.New(dsn)
	if err != nil {
		t.Skipf("skipping integration test: %v", err)
	}
	defer s.Close()
	if err := s.RunMigrations("../../db/migrations"); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	testsupport.LockSettings(t, s.DB)
	ctx := context.Background()
	gin.SetMode(gin.TestMode)
	suffix := time.Now().Format("150405.000")
	if err := s.SetSettings(ctx, map[string]string{store.SettingMaintenanceFeatures: "true"}); err != nil {
		t.Fatal(err)
	}
	// A gated product that published a release and has long since
	// drained, so a cutoff is allowed as things stand.
	drained := time.Now().Add(-30 * 24 * time.Hour)
	prod := &model.Product{Name: "Race", Slug: "race-" + suffix, Type: "desktop", FeedLicenseRequired: true, FeedGatedAt: &drained}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	rel := &model.Release{ProductID: prod.ID, Version: "1.0.0", Channel: model.ReleaseChannelStable}
	if err := s.CreateRelease(ctx, rel); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.NewRaw("UPDATE releases SET status = 'published', published_at = now() WHERE id = ?", rel.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	plan := &model.Plan{ProductID: prod.ID, Name: "P", Slug: "rp-" + suffix, LicenseType: "perpetual", LicenseModel: "standard"}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	lic := &model.License{ProductID: prod.ID, PlanID: plan.ID, Email: "race-" + suffix + "@example.com",
		LicenseKey: "KEY-RACE-" + suffix, Status: model.StatusActive, PaymentProvider: "manual"}
	if err := s.CreateLicense(ctx, lic); err != nil {
		t.Fatal(err)
	}

	// The gate goes down and up again — the drain restarts — after
	// the handler has read the product but before it writes.
	regated := make(chan struct{})
	h := &AdminHandler{Store: s, FeedURLTTL: time.Hour, beforeCutoffWrite: func() {
		if _, err := s.DB.NewRaw("UPDATE products SET feed_license_required = false WHERE id = ?", prod.ID).Exec(ctx); err != nil {
			t.Errorf("ungate: %v", err)
		}
		if _, err := s.DB.NewRaw("UPDATE products SET feed_license_required = true, feed_gated_at = now() WHERE id = ?", prod.ID).Exec(ctx); err != nil {
			t.Errorf("re-gate: %v", err)
		}
		close(regated)
	}}
	body := `{"updates_until":"` + time.Now().Add(24*time.Hour).UTC().Format(time.RFC3339) + `"}`
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPut, "/admin/licenses/"+lic.ID+"/updates-until", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = gin.Params{{Key: "id", Value: lic.ID}}
	h.SetLicenseUpdatesUntil(c)
	<-regated

	var out struct {
		Error struct{ Code string } `json:"error"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if w.Code != http.StatusConflict || out.Error.Code != "FEED_CACHE_DRAINING" {
		t.Fatalf("cutoff written while the drain had restarted: %d %s", w.Code, out.Error.Code)
	}
	got, err := s.FindLicenseByID(ctx, lic.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.UpdatesUntil != nil {
		t.Fatalf("the license kept a cutoff: %v", got.UpdatesUntil)
	}
}

// A cutoff must not land on a license that has meanwhile left its
// perpetual plan: the plan change leaves the same NULL period the
// admin saw, so the compare-and-set alone would not notice.
func TestCutoffRefusedAfterThePlanChanges(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("skipping integration test: TEST_DATABASE_URL not set")
	}
	s, err := store.New(dsn)
	if err != nil {
		t.Skipf("skipping integration test: %v", err)
	}
	defer s.Close()
	if err := s.RunMigrations("../../db/migrations"); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	testsupport.LockSettings(t, s.DB)
	ctx := context.Background()
	gin.SetMode(gin.TestMode)
	suffix := time.Now().Format("150405.000")
	if err := s.SetSettings(ctx, map[string]string{store.SettingMaintenanceFeatures: "true"}); err != nil {
		t.Fatal(err)
	}
	drained := time.Now().Add(-30 * 24 * time.Hour)
	prod := &model.Product{Name: "Moved", Slug: "moved-" + suffix, Type: "desktop", FeedLicenseRequired: true, FeedGatedAt: &drained}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	perp := &model.Plan{ProductID: prod.ID, Name: "P", Slug: "mp-" + suffix, LicenseType: "perpetual", LicenseModel: "standard"}
	sub := &model.Plan{ProductID: prod.ID, Name: "S", Slug: "ms-" + suffix, LicenseType: "subscription", LicenseModel: "standard", BillingInterval: "month"}
	for _, p := range []*model.Plan{perp, sub} {
		if err := s.CreatePlan(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	lic := &model.License{ProductID: prod.ID, PlanID: perp.ID, Email: "moved-" + suffix + "@example.com",
		LicenseKey: "KEY-MOVED-" + suffix, Status: model.StatusActive, PaymentProvider: "manual"}
	if err := s.CreateLicense(ctx, lic); err != nil {
		t.Fatal(err)
	}

	// The license moves to a subscription plan after the handler read
	// it and before it writes.
	h := &AdminHandler{Store: s, FeedURLTTL: time.Hour, beforeCutoffWrite: func() {
		if _, err := s.DB.NewRaw("UPDATE licenses SET plan_id = ? WHERE id = ?", sub.ID, lic.ID).Exec(ctx); err != nil {
			t.Errorf("move plan: %v", err)
		}
	}}
	body := `{"updates_until":"` + time.Now().Add(24*time.Hour).UTC().Format(time.RFC3339) + `"}`
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPut, "/admin/licenses/"+lic.ID+"/updates-until", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = gin.Params{{Key: "id", Value: lic.ID}}
	h.SetLicenseUpdatesUntil(c)

	var out struct {
		Error struct{ Code string } `json:"error"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if w.Code != http.StatusConflict || out.Error.Code != "LICENSE_CHANGED" {
		t.Fatalf("cutoff written after the license left its perpetual plan: %d %s", w.Code, out.Error.Code)
	}
	got, err := s.FindLicenseByID(ctx, lic.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.UpdatesUntil != nil {
		t.Fatalf("a subscription license kept a cutoff: %v", got.UpdatesUntil)
	}
}

// Moving a license onto a perpetual plan with an update period is an
// issuance like any other: it may not happen while the links the
// product's public feed handed out are still usable, or the customer
// could fetch releases past the cutoff it just got.
func TestChangePlanWaitsForTheFeedDrain(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("skipping integration test: TEST_DATABASE_URL not set")
	}
	s, err := store.New(dsn)
	if err != nil {
		t.Skipf("skipping integration test: %v", err)
	}
	defer s.Close()
	if err := s.RunMigrations("../../db/migrations"); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	testsupport.LockSettings(t, s.DB)
	ctx := context.Background()
	gin.SetMode(gin.TestMode)
	suffix := time.Now().Format("150405.000")
	if err := s.SetSettings(ctx, map[string]string{store.SettingMaintenanceFeatures: "true"}); err != nil {
		t.Fatal(err)
	}
	// Gated a moment ago, with a release the public feed has served.
	now := time.Now()
	prod := &model.Product{Name: "Move", Slug: "move-" + suffix, Type: "desktop", FeedLicenseRequired: true, FeedGatedAt: &now}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	rel := &model.Release{ProductID: prod.ID, Version: "1.0.0", Channel: model.ReleaseChannelStable}
	if err := s.CreateRelease(ctx, rel); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.NewRaw("UPDATE releases SET status = 'published', published_at = now() WHERE id = ?", rel.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	sub := &model.Plan{ProductID: prod.ID, Name: "S", Slug: "vs-" + suffix, LicenseType: "subscription", LicenseModel: "standard", BillingInterval: "month"}
	bounded := &model.Plan{ProductID: prod.ID, Name: "B", Slug: "vb-" + suffix, LicenseType: "perpetual", LicenseModel: "standard", UpdatesDays: 365}
	for _, p := range []*model.Plan{sub, bounded} {
		if err := s.CreatePlan(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	lic := &model.License{ProductID: prod.ID, PlanID: sub.ID, Email: "move-" + suffix + "@example.com",
		LicenseKey: "KEY-MOVE-" + suffix, Status: model.StatusActive, PaymentProvider: "manual"}
	if err := s.CreateLicense(ctx, lic); err != nil {
		t.Fatal(err)
	}

	h := &AdminHandler{Store: s, FeedURLTTL: time.Hour}
	change := func() (int, string) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/admin/licenses/"+lic.ID+"/plan",
			strings.NewReader(`{"plan_id":"`+bounded.ID+`"}`))
		c.Request.Header.Set("Content-Type", "application/json")
		c.Params = gin.Params{{Key: "id", Value: lic.ID}}
		h.ChangeLicensePlan(c)
		var out struct {
			Error struct{ Code string } `json:"error"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out.Error.Code
	}

	if code, ec := change(); code != http.StatusConflict || ec != "FEED_CACHE_DRAINING" {
		t.Fatalf("plan change during the drain: %d %s", code, ec)
	}
	got, err := s.FindLicenseByID(ctx, lic.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.PlanID != sub.ID || got.UpdatesUntil != nil {
		t.Fatalf("the license moved anyway: plan=%s until=%v", got.PlanID, got.UpdatesUntil)
	}
	// Once the old links have expired the move goes through with the
	// plan's period.
	if _, err := s.DB.NewRaw("UPDATE products SET feed_gated_at = now() - interval '30 days' WHERE id = ?", prod.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if code, ec := change(); code != http.StatusOK {
		t.Fatalf("plan change after the drain: %d %s", code, ec)
	}
	got, err = s.FindLicenseByID(ctx, lic.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.PlanID != bounded.ID || got.UpdatesUntil == nil {
		t.Fatalf("the move did not grant the period: plan=%s until=%v", got.PlanID, got.UpdatesUntil)
	}
}

// Concurrent product updates must not undo each other's feed gate:
// the instant the gate went up decides how long the feeds drain, so a
// rename that started earlier may not write back the timestamp it
// read.
func TestProductUpdateKeepsAConcurrentGate(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("skipping integration test: TEST_DATABASE_URL not set")
	}
	s, err := store.New(dsn)
	if err != nil {
		t.Skipf("skipping integration test: %v", err)
	}
	defer s.Close()
	if err := s.RunMigrations("../../db/migrations"); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	ctx := context.Background()
	gin.SetMode(gin.TestMode)
	suffix := time.Now().Format("150405.000")
	old := time.Now().Add(-30 * 24 * time.Hour)
	prod := &model.Product{Name: "Gate", Slug: "gate-" + suffix, Type: "desktop", FeedLicenseRequired: true, FeedGatedAt: &old}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	h := &AdminHandler{Store: s, FeedURLTTL: time.Hour}

	// Another request ungates and re-gates the product after this one
	// read it, so the rename below carries a stale timestamp.
	if _, err := s.DB.NewRaw("UPDATE products SET feed_license_required = false WHERE id = ?", prod.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.NewRaw("UPDATE products SET feed_license_required = true, feed_gated_at = now() WHERE id = ?", prod.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPut, "/admin/products/"+prod.ID, strings.NewReader(`{"name":"Renamed"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = gin.Params{{Key: "id", Value: prod.ID}}
	h.UpdateProduct(c)
	if w.Code != http.StatusOK {
		t.Fatalf("rename: %d %s", w.Code, w.Body.String())
	}
	got, err := s.FindProductByID(ctx, prod.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "Renamed" {
		t.Fatalf("the rename did not land: %q", got.Name)
	}
	if got.FeedGatedAt == nil || got.FeedGatedAt.Before(time.Now().Add(-time.Hour)) {
		t.Fatalf("the rename put back the old gate instant: %v", got.FeedGatedAt)
	}
}

// Switching the gate on records the instant the database wrote it,
// not one read on this replica before the transaction: the public
// feed keeps serving until the write commits.
func TestGatingStampsTheDatabaseClock(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("skipping integration test: TEST_DATABASE_URL not set")
	}
	s, err := store.New(dsn)
	if err != nil {
		t.Skipf("skipping integration test: %v", err)
	}
	defer s.Close()
	if err := s.RunMigrations("../../db/migrations"); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	testsupport.LockSettings(t, s.DB)
	ctx := context.Background()
	gin.SetMode(gin.TestMode)
	suffix := time.Now().Format("150405.000")
	if err := s.SetSettings(ctx, map[string]string{store.SettingMaintenanceFeatures: "true"}); err != nil {
		t.Fatal(err)
	}
	prod := &model.Product{Name: "Stamp", Slug: "stamp-" + suffix, Type: "desktop"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	h := &AdminHandler{Store: s, FeedURLTTL: time.Hour}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPut, "/admin/products/"+prod.ID, strings.NewReader(`{"feed_license_required":true}`))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = gin.Params{{Key: "id", Value: prod.ID}}
	h.UpdateProduct(c)
	if w.Code != http.StatusOK {
		t.Fatalf("gating: %d %s", w.Code, w.Body.String())
	}
	var stamped, dbNow time.Time
	if err := s.DB.NewRaw("SELECT feed_gated_at, clock_timestamp() FROM products WHERE id = ?", prod.ID).Scan(ctx, &stamped, &dbNow); err != nil {
		t.Fatal(err)
	}
	if stamped.IsZero() || dbNow.Sub(stamped) > time.Minute || stamped.After(dbNow) {
		t.Fatalf("gate instant %v is not the database's own (now %v)", stamped, dbNow)
	}
}

// A paid fulfilment writing a bounded license and an admin editing
// the same product take the same two locks. Taking them in opposite
// orders makes Postgres abort one of them — losing either the
// fulfilment or the edit — so every writer takes the rows first and
// the feed gate second.
func TestGateAndRowLocksDoNotDeadlock(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("skipping integration test: TEST_DATABASE_URL not set")
	}
	s, err := store.New(dsn)
	if err != nil {
		t.Skipf("skipping integration test: %v", err)
	}
	defer s.Close()
	if err := s.RunMigrations("../../db/migrations"); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	testsupport.LockSettings(t, s.DB)
	ctx := context.Background()
	gin.SetMode(gin.TestMode)
	suffix := time.Now().Format("150405.000")
	if err := s.SetSettings(ctx, map[string]string{store.SettingMaintenanceFeatures: "true"}); err != nil {
		t.Fatal(err)
	}
	drained := time.Now().Add(-30 * 24 * time.Hour)
	prod := &model.Product{Name: "Lock", Slug: "lock-" + suffix, Type: "desktop", FeedLicenseRequired: true, FeedGatedAt: &drained}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	plan := &model.Plan{ProductID: prod.ID, Name: "B", Slug: "lb-" + suffix, LicenseType: "perpetual", LicenseModel: "standard", UpdatesDays: 365}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	h := &AdminHandler{Store: s, FeedURLTTL: time.Hour}

	for i := range 20 {
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			until := time.Now().AddDate(0, 0, 365)
			lic := &model.License{ProductID: prod.ID, PlanID: plan.ID,
				Email:      fmt.Sprintf("lock-%d-%s@example.com", i, suffix),
				LicenseKey: fmt.Sprintf("KEY-LOCK-%d-%s", i, suffix),
				Status:     model.StatusActive, PaymentProvider: "stripe",
				UpdatesUntil: &until, UpdatesTermsSet: true}
			if err := s.CreateLicenseWithSubscription(ctx, lic, plan); err != nil {
				t.Errorf("round %d fulfilment: %v", i, err)
			}
		}()
		go func() {
			defer wg.Done()
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPut, "/admin/products/"+prod.ID,
				strings.NewReader(fmt.Sprintf(`{"name":"Lock %d"}`, i)))
			c.Request.Header.Set("Content-Type", "application/json")
			c.Params = gin.Params{{Key: "id", Value: prod.ID}}
			h.UpdateProduct(c)
			if w.Code != http.StatusOK {
				t.Errorf("round %d product edit: %d %s", i, w.Code, w.Body.String())
			}
		}()
		wg.Wait()
	}
}

// The target plan decides what happens to the update period, so it is
// read under lock inside the write. A plan that turned perpetual
// after this handler read it must not be treated as the subscription
// it looked like: that branch clears the period and closes the
// renewal ledger the customer paid into.
func TestChangePlanReadsTheTargetPlanUnderLock(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("skipping integration test: TEST_DATABASE_URL not set")
	}
	s, err := store.New(dsn)
	if err != nil {
		t.Skipf("skipping integration test: %v", err)
	}
	defer s.Close()
	if err := s.RunMigrations("../../db/migrations"); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	testsupport.LockSettings(t, s.DB)
	ctx := context.Background()
	gin.SetMode(gin.TestMode)
	suffix := time.Now().Format("150405.000")
	if err := s.SetSettings(ctx, map[string]string{store.SettingMaintenanceFeatures: "true"}); err != nil {
		t.Fatal(err)
	}
	drained := time.Now().Add(-30 * 24 * time.Hour)
	prod := &model.Product{Name: "Race2", Slug: "race2-" + suffix, Type: "desktop", FeedLicenseRequired: true, FeedGatedAt: &drained}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	from := &model.Plan{ProductID: prod.ID, Name: "From", Slug: "cf-" + suffix, LicenseType: "perpetual", LicenseModel: "standard", UpdatesDays: 365}
	// Reads as a subscription plan now; another admin retypes it
	// before this request writes.
	target := &model.Plan{ProductID: prod.ID, Name: "To", Slug: "ct-" + suffix, LicenseType: "subscription", LicenseModel: "standard", BillingInterval: "month"}
	for _, p := range []*model.Plan{from, target} {
		if err := s.CreatePlan(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	until := time.Now().AddDate(0, 0, 200).Truncate(time.Second)
	lic := &model.License{ProductID: prod.ID, PlanID: from.ID, Email: "race2-" + suffix + "@example.com",
		LicenseKey: "KEY-RACE2-" + suffix, Status: model.StatusActive, PaymentProvider: "stripe",
		UpdatesUntil: &until, UpdatesTermsSet: true}
	if err := s.CreateLicense(ctx, lic); err != nil {
		t.Fatal(err)
	}
	renewal := &model.LicenseRenewal{LicenseID: lic.ID, Days: 365}
	if err := s.ApplyLicenseRenewal(ctx, renewal); err != nil {
		t.Fatal(err)
	}
	paidUntil := updatesUntilOf(t, s, ctx, lic.ID)

	h := &AdminHandler{Store: s, FeedURLTTL: time.Hour}
	// The retype lands between the handler's read and its write.
	h.beforeCutoffWrite = func() {
		if _, err := s.DB.NewRaw("UPDATE plans SET license_type = 'perpetual', billing_interval = '', updates_days = 30 WHERE id = ?", target.ID).Exec(ctx); err != nil {
			t.Errorf("retype: %v", err)
		}
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/admin/licenses/"+lic.ID+"/change-plan",
		strings.NewReader(`{"plan_id":"`+target.ID+`"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = gin.Params{{Key: "id", Value: lic.ID}}
	h.ChangeLicensePlan(c)
	if w.Code != http.StatusOK {
		t.Fatalf("change plan: %d %s", w.Code, w.Body.String())
	}
	// Perpetual to perpetual: the paid period stays, and so does the
	// ledger behind it.
	got := updatesUntilOf(t, s, ctx, lic.ID)
	if got == nil || got.Sub(*paidUntil).Abs() > time.Second {
		t.Fatalf("the paid period was lost: got %v want %v", got, paidUntil)
	}
	var open int
	if err := s.DB.NewRaw("SELECT count(*) FROM license_renewals WHERE license_id = ? AND superseded_at IS NULL", lic.ID).Scan(ctx, &open); err != nil {
		t.Fatal(err)
	}
	if open != 1 {
		t.Fatalf("the paid renewal was superseded: %d rows still open", open)
	}
}

func updatesUntilOf(t *testing.T, s *store.Store, ctx context.Context, id string) *time.Time {
	t.Helper()
	var u *time.Time
	if err := s.DB.NewRaw("SELECT updates_until FROM licenses WHERE id = ?", id).Scan(ctx, &u); err != nil {
		t.Fatal(err)
	}
	return u
}

// While the rollout confirmation is off, a replica that predates this
// version may be running and enforces none of the maintenance terms.
// A plan that already sells them may then only have them cleared, not
// changed — a longer period or a different renewal price is just as
// unenforceable as a new one.
func TestPlanTermsFrozenWhileTheSwitchIsOff(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("skipping integration test: TEST_DATABASE_URL not set")
	}
	s, err := store.New(dsn)
	if err != nil {
		t.Skipf("skipping integration test: %v", err)
	}
	defer s.Close()
	if err := s.RunMigrations("../../db/migrations"); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	testsupport.LockSettings(t, s.DB)
	ctx := context.Background()
	gin.SetMode(gin.TestMode)
	suffix := time.Now().Format("150405.000")
	drained := time.Now().Add(-30 * 24 * time.Hour)
	prod := &model.Product{Name: "Frozen", Slug: "frozen-" + suffix, Type: "desktop", FeedLicenseRequired: true, FeedGatedAt: &drained}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	plan := &model.Plan{ProductID: prod.ID, Name: "B", Slug: "fb-" + suffix, LicenseType: "perpetual", LicenseModel: "standard",
		UpdatesDays: 365, RenewalDays: 365, StripeRenewalPriceID: "price_frozen_" + suffix}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	h := &AdminHandler{Store: s, FeedURLTTL: time.Hour}
	update := func(body string) (int, string) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPut, "/admin/plans/"+plan.ID, strings.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		c.Params = gin.Params{{Key: "id", Value: plan.ID}}
		h.UpdatePlan(c)
		var out struct {
			Error struct{ Code string } `json:"error"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out.Error.Code
	}
	if err := s.SetSettings(ctx, map[string]string{store.SettingMaintenanceFeatures: "false"}); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := s.SetSettings(ctx, map[string]string{store.SettingMaintenanceFeatures: "true"}); err != nil {
			t.Errorf("restore the switch: %v", err)
		}
	}()

	// Already selling 365: stretching it, or moving the renewal
	// price, is refused while the switch is off.
	if code, ec := update(`{"updates_days":3650,"renewal_days":365,"stripe_renewal_price_id":"price_frozen_` + suffix + `"}`); code != http.StatusConflict || ec != "MAINTENANCE_FEATURES_DISABLED" {
		t.Fatalf("stretching the period with the switch off: %d %s", code, ec)
	}
	if code, ec := update(`{"updates_days":365,"renewal_days":365,"stripe_renewal_price_id":"price_other_` + suffix + `"}`); code != http.StatusConflict || ec != "MAINTENANCE_FEATURES_DISABLED" {
		t.Fatalf("changing the renewal price with the switch off: %d %s", code, ec)
	}
	// An edit that leaves the terms alone still works.
	if code, ec := update(`{"name":"Renamed","updates_days":365,"renewal_days":365,"stripe_renewal_price_id":"price_frozen_` + suffix + `"}`); code != http.StatusOK {
		t.Fatalf("unrelated edit with the switch off: %d %s", code, ec)
	}
	// And clearing them — what an operator does before rolling back —
	// is allowed.
	if code, ec := update(`{"updates_days":0,"renewal_days":0,"stripe_renewal_price_id":""}`); code != http.StatusOK {
		t.Fatalf("clearing the terms with the switch off: %d %s", code, ec)
	}
	got, err := s.FindPlanByID(ctx, plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.UpdatesDays != 0 || got.RenewalDays != 0 || got.StripeRenewalPriceID != "" {
		t.Fatalf("the terms were not cleared: %+v", got)
	}
}

// Whether anything the feed gate protects exists is decided under the
// product's lock: a bounded plan may be created on a saas product at
// any moment — it serves no feed — and one created after the handler
// looked would otherwise let the product get its feeds back without
// waiting out the links its public feed handed out.
func TestRestoringFeedsSeesAPlanCreatedMeanwhile(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("skipping integration test: TEST_DATABASE_URL not set")
	}
	s, err := store.New(dsn)
	if err != nil {
		t.Skipf("skipping integration test: %v", err)
	}
	defer s.Close()
	if err := s.RunMigrations("../../db/migrations"); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	testsupport.LockSettings(t, s.DB)
	ctx := context.Background()
	gin.SetMode(gin.TestMode)
	suffix := time.Now().Format("150405.000")
	if err := s.SetSettings(ctx, map[string]string{store.SettingMaintenanceFeatures: "true"}); err != nil {
		t.Fatal(err)
	}
	// A saas product that published releases while it still served
	// feeds, gated a moment ago.
	now := time.Now()
	prod := &model.Product{Name: "Late", Slug: "late-" + suffix, Type: "saas", FeedLicenseRequired: true, FeedGatedAt: &now}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	rel := &model.Release{ProductID: prod.ID, Version: "1.0.0", Channel: model.ReleaseChannelStable}
	if err := s.CreateRelease(ctx, rel); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.NewRaw("UPDATE releases SET status = 'published', published_at = now() WHERE id = ?", rel.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	h := &AdminHandler{Store: s, FeedURLTTL: time.Hour}
	// The bounded plan appears after the handler has read the product.
	h.beforeCutoffWrite = func() {
		plan := &model.Plan{ProductID: prod.ID, Name: "B", Slug: "lb-" + suffix, LicenseType: "perpetual", LicenseModel: "standard", UpdatesDays: 365}
		if err := s.CreatePlan(ctx, plan); err != nil {
			t.Errorf("create bounded plan: %v", err)
		}
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPut, "/admin/products/"+prod.ID, strings.NewReader(`{"type":"desktop"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = gin.Params{{Key: "id", Value: prod.ID}}
	h.UpdateProduct(c)

	var out struct {
		Error struct{ Code string } `json:"error"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if w.Code != http.StatusConflict || out.Error.Code != "FEED_CACHE_DRAINING" {
		t.Fatalf("restoring feeds past a plan created meanwhile: %d %s", w.Code, out.Error.Code)
	}
	got, err := s.FindProductByID(ctx, prod.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Type != "saas" {
		t.Fatalf("the product got its feeds back: %s", got.Type)
	}
}

// A plan edit writes only the fields it carries. Two requests that
// started from the same snapshot must not undo each other: a rename
// may not put back update terms another request has just changed.
func TestPlanEditWritesOnlyItsOwnFields(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("skipping integration test: TEST_DATABASE_URL not set")
	}
	s, err := store.New(dsn)
	if err != nil {
		t.Skipf("skipping integration test: %v", err)
	}
	defer s.Close()
	if err := s.RunMigrations("../../db/migrations"); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	testsupport.LockSettings(t, s.DB)
	ctx := context.Background()
	gin.SetMode(gin.TestMode)
	suffix := time.Now().Format("150405.000")
	if err := s.SetSettings(ctx, map[string]string{store.SettingMaintenanceFeatures: "true"}); err != nil {
		t.Fatal(err)
	}
	drained := time.Now().Add(-30 * 24 * time.Hour)
	prod := &model.Product{Name: "Merge", Slug: "merge-" + suffix, Type: "desktop", FeedLicenseRequired: true, FeedGatedAt: &drained}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	plan := &model.Plan{ProductID: prod.ID, Name: "B", Slug: "mb-" + suffix, LicenseType: "perpetual", LicenseModel: "standard", UpdatesDays: 365}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	h := &AdminHandler{Store: s, FeedURLTTL: time.Hour}
	// Another request shortens the period after this one read the plan.
	h.beforeCutoffWrite = func() {
		if _, err := s.DB.NewRaw("UPDATE plans SET updates_days = 30 WHERE id = ?", plan.ID).Exec(ctx); err != nil {
			t.Errorf("concurrent edit: %v", err)
		}
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPut, "/admin/plans/"+plan.ID, strings.NewReader(`{"name":"Renamed"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = gin.Params{{Key: "id", Value: plan.ID}}
	h.UpdatePlan(c)
	if w.Code != http.StatusOK {
		t.Fatalf("rename: %d %s", w.Code, w.Body.String())
	}
	got, err := s.FindPlanByID(ctx, plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "Renamed" {
		t.Fatalf("the rename did not land: %q", got.Name)
	}
	if got.UpdatesDays != 30 {
		t.Fatalf("the rename put the old update period back: %d days", got.UpdatesDays)
	}
}

// Turning the feed gate on with a direct UPDATE — an operator at the
// psql prompt, or an older tool — must still record when it went up.
// A NULL there reads as "nothing to wait out", and a period could be
// sold while the public feed's links still work.
func TestDirectGateWriteIsStamped(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("skipping integration test: TEST_DATABASE_URL not set")
	}
	s, err := store.New(dsn)
	if err != nil {
		t.Skipf("skipping integration test: %v", err)
	}
	defer s.Close()
	if err := s.RunMigrations("../../db/migrations"); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	ctx := context.Background()
	suffix := time.Now().Format("150405.000")
	prod := &model.Product{Name: "Direct", Slug: "direct-" + suffix, Type: "desktop"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.NewRaw("UPDATE products SET feed_license_required = true WHERE id = ?", prod.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := s.FindProductByID(ctx, prod.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.FeedGatedAt == nil {
		t.Fatal("a direct gate write left no timestamp")
	}
	// A product created gated is stamped the same way.
	born := &model.Product{Name: "Born", Slug: "born-" + suffix, Type: "desktop", FeedLicenseRequired: true}
	if err := s.CreateProduct(ctx, born); err != nil {
		t.Fatal(err)
	}
	fresh, err := s.FindProductByID(ctx, born.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.FeedGatedAt == nil {
		t.Fatal("a product created gated left no timestamp")
	}
	// An explicit instant is kept: the API sets it itself.
	long := time.Now().Add(-30 * 24 * time.Hour)
	explicit := &model.Product{Name: "Explicit", Slug: "explicit-" + suffix, Type: "desktop", FeedLicenseRequired: true, FeedGatedAt: &long}
	if err := s.CreateProduct(ctx, explicit); err != nil {
		t.Fatal(err)
	}
	kept, err := s.FindProductByID(ctx, explicit.ID)
	if err != nil {
		t.Fatal(err)
	}
	if kept.FeedGatedAt == nil || kept.FeedGatedAt.Sub(long).Abs() > time.Second {
		t.Fatalf("an explicit gate instant was overwritten: %v", kept.FeedGatedAt)
	}
}

// Two requests changing different maintenance fields must not write
// each other's back: each holds a snapshot taken before the plan's
// lock, so the fields are merged under it.
func TestConcurrentTermEditsMerge(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("skipping integration test: TEST_DATABASE_URL not set")
	}
	s, err := store.New(dsn)
	if err != nil {
		t.Skipf("skipping integration test: %v", err)
	}
	defer s.Close()
	if err := s.RunMigrations("../../db/migrations"); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	testsupport.LockSettings(t, s.DB)
	ctx := context.Background()
	gin.SetMode(gin.TestMode)
	suffix := time.Now().Format("150405.000")
	if err := s.SetSettings(ctx, map[string]string{store.SettingMaintenanceFeatures: "true"}); err != nil {
		t.Fatal(err)
	}
	drained := time.Now().Add(-30 * 24 * time.Hour)
	prod := &model.Product{Name: "Terms3", Slug: "terms3-" + suffix, Type: "desktop", FeedLicenseRequired: true, FeedGatedAt: &drained}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	plan := &model.Plan{ProductID: prod.ID, Name: "B", Slug: "t3-" + suffix, LicenseType: "perpetual", LicenseModel: "standard",
		UpdatesDays: 365, RenewalDays: 365, StripeRenewalPriceID: "price_t3_" + suffix}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	h := &AdminHandler{Store: s, FeedURLTTL: time.Hour}
	// Request A lands first, changing only the period.
	h.beforeCutoffWrite = func() {
		if _, err := s.DB.NewRaw("UPDATE plans SET updates_days = 730 WHERE id = ?", plan.ID).Exec(ctx); err != nil {
			t.Errorf("concurrent edit: %v", err)
		}
		if err := s.SetSettings(ctx, map[string]string{}); err != nil {
			t.Errorf("noop: %v", err)
		}
	}
	// Request B carries only the renewal days.
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPut, "/admin/plans/"+plan.ID, strings.NewReader(`{"renewal_days":180}`))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = gin.Params{{Key: "id", Value: plan.ID}}
	h.UpdatePlan(c)
	if w.Code != http.StatusOK {
		t.Fatalf("renewal-days edit: %d %s", w.Code, w.Body.String())
	}
	got, err := s.FindPlanByID(ctx, plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RenewalDays != 180 {
		t.Fatalf("the request's own field did not land: %d", got.RenewalDays)
	}
	if got.UpdatesDays != 730 {
		t.Fatalf("the other request's period was written back: %d days", got.UpdatesDays)
	}
	// And the history agrees with the row.
	var latest int
	if err := s.DB.NewRaw(
		"SELECT updates_days FROM plan_update_terms WHERE plan_id = ? ORDER BY effective_from DESC, recorded_at DESC LIMIT 1", plan.ID,
	).Scan(ctx, &latest); err != nil {
		t.Fatal(err)
	}
	if latest != 365 && latest != 730 {
		t.Fatalf("the terms history disagrees with the plan: %d days", latest)
	}
}

// Whether a plan update changes the licence type is decided against
// the plan under the lock. A request that read "perpetual" and writes
// "perpetual" is a change when another request turned it into a
// subscription meanwhile — and the licences issued under that type
// would be left with the wrong shape.
func TestPlanTypeChangeSeenAgainstTheLockedPlan(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("skipping integration test: TEST_DATABASE_URL not set")
	}
	s, err := store.New(dsn)
	if err != nil {
		t.Skipf("skipping integration test: %v", err)
	}
	defer s.Close()
	if err := s.RunMigrations("../../db/migrations"); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	ctx := context.Background()
	gin.SetMode(gin.TestMode)
	suffix := time.Now().Format("150405.000")
	prod := &model.Product{Name: "Retype2", Slug: "retype2-" + suffix, Type: "desktop"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	plan := &model.Plan{ProductID: prod.ID, Name: "P", Slug: "rt2-" + suffix, LicenseType: "perpetual", LicenseModel: "standard"}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	h := &AdminHandler{Store: s, FeedURLTTL: time.Hour}
	// Another request turns it into a subscription and a licence is
	// issued under that type, after this one read the plan.
	h.beforeCutoffWrite = func() {
		if _, err := s.DB.NewRaw("UPDATE plans SET license_type = 'subscription', billing_interval = 'month' WHERE id = ?", plan.ID).Exec(ctx); err != nil {
			t.Errorf("retype: %v", err)
			return
		}
		lic := &model.License{ProductID: prod.ID, PlanID: plan.ID, Email: "rt2-" + suffix + "@example.com",
			LicenseKey: "KEY-RT2-" + suffix, Status: model.StatusActive, PaymentProvider: "manual"}
		if err := s.CreateLicense(ctx, lic); err != nil {
			t.Errorf("issue licence: %v", err)
		}
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	// Reads perpetual, writes perpetual: a no-op to this request, a
	// type change by the time it holds the lock.
	c.Request = httptest.NewRequest(http.MethodPut, "/admin/plans/"+plan.ID, strings.NewReader(`{"license_type":"perpetual"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = gin.Params{{Key: "id", Value: plan.ID}}
	h.UpdatePlan(c)

	var out struct {
		Error struct{ Code string } `json:"error"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if w.Code != http.StatusConflict || out.Error.Code != "HAS_LICENSES" {
		t.Fatalf("type change under existing licences: %d %s", w.Code, out.Error.Code)
	}
	got, err := s.FindPlanByID(ctx, plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.LicenseType != "subscription" {
		t.Fatalf("the type was changed under the licence: %s", got.LicenseType)
	}
}

// The drain reads the recorded bound on every check, so a replica
// with a short TTL cannot decide that links another replica signed
// for longer have expired — and a bound raised while this replica
// runs takes effect without a restart.
func TestFeedDrainFollowsTheRecordedBound(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("skipping integration test: TEST_DATABASE_URL not set")
	}
	s, err := store.New(dsn)
	if err != nil {
		t.Skipf("skipping integration test: %v", err)
	}
	defer s.Close()
	if err := s.RunMigrations("../../db/migrations"); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	ctx := context.Background()
	gin.SetMode(gin.TestMode)
	prevBound, _ := s.GetSetting(ctx, store.SettingFeedURLTTLBound)
	defer func() {
		if prevBound != "" {
			if err := s.SetSettings(ctx, map[string]string{store.SettingFeedURLTTLBound: prevBound}); err != nil {
				t.Errorf("restore the bound: %v", err)
			}
		}
	}()
	suffix := time.Now().Format("150405.000")
	// Gated two hours ago, with a release the public feed served.
	gated := time.Now().Add(-2 * time.Hour)
	prod := &model.Product{Name: "Bound", Slug: "bound-" + suffix, Type: "desktop", FeedLicenseRequired: true, FeedGatedAt: &gated}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	rel := &model.Release{ProductID: prod.ID, Version: "1.0.0", Channel: model.ReleaseChannelStable}
	if err := s.CreateRelease(ctx, rel); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.NewRaw("UPDATE releases SET status = 'published', published_at = now() WHERE id = ?", rel.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}

	// This replica signs links for a minute, so on its own reading
	// the wait is long over.
	h := &AdminHandler{Store: s, FeedURLTTL: time.Minute}
	drained := func() bool {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
		return !h.feedDrainPending(c, prod)
	}
	if err := s.SetSettings(ctx, map[string]string{store.SettingFeedURLTTLBound: "1m0s"}); err != nil {
		t.Fatal(err)
	}
	if !drained() {
		t.Fatal("a bound as short as this replica's TTL should leave nothing to wait for")
	}
	// Another replica signs them for a day and records it: the wait
	// is not over, and this replica sees that without restarting.
	if got, err := s.RaiseDurationSetting(ctx, store.SettingFeedURLTTLBound, 24*time.Hour); err != nil || got != 24*time.Hour {
		t.Fatalf("raise the bound: %v %v", got, err)
	}
	if drained() {
		t.Fatal("a longer bound recorded by another replica was ignored")
	}
	// A recorded bound nobody can read is not permission to fall back
	// to this replica's own minute: the check refuses, loudly.
	if err := s.SetSettings(ctx, map[string]string{store.SettingFeedURLTTLBound: "sometime"}); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	if pending := h.feedDrainPending(c, prod); !pending || w.Code != http.StatusInternalServerError {
		t.Fatalf("an unreadable bound: pending=%v code=%d want true 500", pending, w.Code)
	}
}

// Lowering the bound is allowed — it is how an operator says the
// longer-lived links are gone — but only to a sane positive
// duration: the drain is measured against it.
func TestFeedURLTTLBoundIsValidated(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("skipping integration test: TEST_DATABASE_URL not set")
	}
	s, err := store.New(dsn)
	if err != nil {
		t.Skipf("skipping integration test: %v", err)
	}
	defer s.Close()
	if err := s.RunMigrations("../../db/migrations"); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	ctx := context.Background()
	gin.SetMode(gin.TestMode)
	before, _ := s.GetSetting(ctx, store.SettingFeedURLTTLBound)
	defer func() {
		if before != "" {
			if err := s.SetSettings(ctx, map[string]string{store.SettingFeedURLTTLBound: before}); err != nil {
				t.Errorf("restore: %v", err)
			}
		}
	}()
	h := &AdminHandler{Store: s, FeedURLTTL: time.Hour}
	save := func(value string) int {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPut, "/admin/settings",
			strings.NewReader(`{"settings":{"feed_url_ttl_bound":"`+value+`"}}`))
		c.Request.Header.Set("Content-Type", "application/json")
		h.UpdateSettings(c)
		return w.Code
	}
	for _, bad := range []string{"0s", "-1h", "soon", "", "240h"} {
		if code := save(bad); code != http.StatusBadRequest {
			t.Fatalf("saving %q as the bound: %d want 400", bad, code)
		}
	}
	if code := save("2h"); code != http.StatusOK {
		t.Fatalf("lowering it to a sane value: %d", code)
	}
	if got, err := s.GetSetting(ctx, store.SettingFeedURLTTLBound); err != nil || got != "2h0m0s" {
		t.Fatalf("stored bound: %q (%v)", got, err)
	}
}
