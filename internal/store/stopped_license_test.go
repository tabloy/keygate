package store_test

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
)

// A licence that stops (suspended by an operator or by Stripe, expired,
// revoked) holds no floating session: one left from before would come
// back with the licence, heartbeating without a new checkout and counted
// towards the cap. Canceled keeps them: it works to its period's end.
func TestStoppedLicenceHoldsNoFloatingSession(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()
	suffix := time.Now().Format("150405.000000000")
	prod := &model.Product{Name: "Float " + suffix, Slug: "float-stop-" + suffix, Type: "desktop"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	plan := &model.Plan{ProductID: prod.ID, Name: "F", Slug: "float-stop-" + suffix, LicenseType: "subscription",
		LicenseModel: "floating", MaxActivations: 1, Active: true}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	licence := func(tag string) *model.License {
		t.Helper()
		future := time.Now().Add(30 * 24 * time.Hour)
		l := &model.License{ProductID: prod.ID, PlanID: plan.ID, Email: tag + "-" + suffix + "@example.com",
			LicenseKey: "KG-FL-" + tag + "-" + suffix, Status: model.StatusActive, ValidUntil: &future}
		if err := s.CreateLicense(ctx, l); err != nil {
			t.Fatal(err)
		}
		if err := s.CheckOutFloating(ctx, &model.FloatingSession{LicenseID: l.ID, Identifier: "pc-" + tag,
			ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
		return l
	}
	sessions := func(l *model.License) int {
		t.Helper()
		var n int
		if err := s.DB.NewRaw("SELECT count(*) FROM floating_sessions WHERE license_id = ?", l.ID).Scan(ctx, &n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	suspended := licence("suspended")
	if err := s.SuspendLicense(ctx, suspended.ID); err != nil {
		t.Fatal(err)
	}
	if n := sessions(suspended); n != 0 {
		t.Errorf("suspended by an operator: %d sessions left, want 0", n)
	}

	expired := licence("expired")
	if _, err := s.DB.NewRaw("UPDATE licenses SET valid_until = now() - interval '30 days' WHERE id = ?", expired.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.ExpireLicenseAfterGrace(ctx, expired.ID); err != nil || !ok {
		t.Fatalf("expire = %v, %v", ok, err)
	}
	if n := sessions(expired); n != 0 {
		t.Errorf("expired: %d sessions left, want 0", n)
	}

	paused := licence("paused")
	paused.Status, paused.SuspendedBy = model.StatusSuspended, model.SuspendedByStripe
	if err := s.UpdateLicenseAndSubscription(ctx, paused, "status", "suspended_by"); err != nil {
		t.Fatal(err)
	}
	if n := sessions(paused); n != 0 {
		t.Errorf("paused by Stripe: %d sessions left, want 0", n)
	}

	canceled := licence("canceled")
	canceled.Status = model.StatusCanceled
	if err := s.UpdateLicenseAndSubscription(ctx, canceled, "status"); err != nil {
		t.Fatal(err)
	}
	if n := sessions(canceled); n != 1 {
		t.Errorf("canceled (works to its period's end): %d sessions, want 1", n)
	}

	// A stop that left its sessions (an older version, a path that does
	// not end them) is caught by the cleanup.
	leftover := licence("leftover")
	if _, err := s.DB.NewRaw("UPDATE licenses SET status = 'suspended' WHERE id = ?", leftover.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CleanExpiredFloating(ctx); err != nil {
		t.Fatal(err)
	}
	if n := sessions(leftover); n != 0 {
		t.Errorf("cleanup left %d sessions of a suspended licence, want 0", n)
	}
	if n := sessions(canceled); n != 1 {
		t.Errorf("cleanup took the canceled licence's session (%d left)", n)
	}
}

// Yesterday's snapshot is taken once: taken again, it is left as it was.
func TestTakeMissingSnapshots(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()
	suffix := time.Now().Format("150405.000000000")
	prod := &model.Product{Name: "Snap " + suffix, Slug: "snap-" + suffix, Type: "saas"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	day := time.Now().Add(-24 * time.Hour)
	if err := s.TakeMissingSnapshots(ctx, day); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.NewRaw("UPDATE analytics_snapshots SET total_licenses = 42 WHERE product_id = ?", prod.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.TakeMissingSnapshots(ctx, day); err != nil {
		t.Fatal(err)
	}
	var rows, total int
	if err := s.DB.NewRaw("SELECT count(*), max(total_licenses) FROM analytics_snapshots WHERE product_id = ?", prod.ID).Scan(ctx, &rows, &total); err != nil {
		t.Fatal(err)
	}
	if rows != 1 || total != 42 {
		t.Fatalf("snapshots = %d (total %d), want the one taken first, left alone", rows, total)
	}
}

// A snapshot that fails partway through (one of its counts errors, the
// rest would have succeeded) writes nothing at all, rather than a
// zeroed row that would then look like the day was already taken: the
// next run still sees it missing and actually takes it.
//
// To fail exactly one count and nothing else, this runs TakeSnapshot
// as a second Postgres role with SELECT revoked on just the seats
// table - a real permission error on one query, with every other query
// (including the final insert) still able to succeed, so a test that
// instead failed everything at once (e.g. a canceled context) couldn't
// tell this fix apart from the bug it fixes.
func TestTakeSnapshotWritesNothingIfACountFails(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()
	suffix := strings.ReplaceAll(time.Now().Format("150405.000000000"), ".", "")
	prod := &model.Product{Name: "Snap Fail " + suffix, Slug: "snap-fail-" + suffix, Type: "saas"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	day := time.Now().Add(-24 * time.Hour)

	rowCount := func() int {
		t.Helper()
		var n int
		if err := s.DB.NewRaw("SELECT count(*) FROM analytics_snapshots WHERE product_id = ?", prod.ID).Scan(ctx, &n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	dsn := os.Getenv("TEST_DATABASE_URL")
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	role := "snap_fail_" + suffix
	pass := "snap_fail_pw"
	if _, err := s.DB.ExecContext(ctx, `CREATE ROLE "`+role+`" LOGIN PASSWORD '`+pass+`'`); err != nil {
		t.Fatal(err)
	}
	// Runs before the outer `defer s.Close()`, and after `defer
	// restricted.Close()` below (registered later, so it unwinds
	// first): the restricted connection closes, then the role it used
	// is dropped, then the main store closes.
	defer func() {
		if _, err := s.DB.ExecContext(ctx, `DROP OWNED BY "`+role+`"`); err != nil {
			t.Errorf("drop owned by role: %v", err)
		}
		if _, err := s.DB.ExecContext(ctx, `DROP ROLE "`+role+`"`); err != nil {
			t.Errorf("drop role: %v", err)
		}
	}()
	grants := []string{
		`GRANT CONNECT ON DATABASE ` + strings.TrimPrefix(u.Path, "/") + ` TO "` + role + `"`,
		`GRANT USAGE ON SCHEMA public TO "` + role + `"`,
		`GRANT SELECT ON licenses, activations, usage_events TO "` + role + `"`,
		// UPDATE too: CreateAnalyticsSnapshot's ON CONFLICT DO UPDATE
		// needs it even on the insert path, or every insert attempt
		// fails here for an unrelated reason and the test can't tell
		// this fix apart from the bug it fixes.
		`GRANT SELECT, INSERT, UPDATE ON analytics_snapshots TO "` + role + `"`,
	}
	for _, g := range grants {
		if _, err := s.DB.ExecContext(ctx, g); err != nil {
			t.Fatal(err)
		}
	}
	// Deliberately no SELECT on seats: that is the one count this role
	// cannot take.

	restrictedURL := *u
	restrictedURL.User = url.UserPassword(role, pass)
	restricted, err := store.New(restrictedURL.String())
	if err != nil {
		t.Fatal(err)
	}
	defer restricted.Close()

	if err := restricted.TakeSnapshot(ctx, prod.ID, day); err == nil {
		t.Fatal("TakeSnapshot without SELECT on seats = nil error, want a permission error")
	}
	if n := rowCount(); n != 0 {
		t.Fatalf("snapshots after a failed take = %d, want 0 (no zeroed row left behind)", n)
	}

	// Nothing marked the day done, so a later, successful pass (back
	// on the unrestricted store) still takes it.
	if err := s.TakeMissingSnapshots(ctx, day); err != nil {
		t.Fatal(err)
	}
	if n := rowCount(); n != 1 {
		t.Fatalf("snapshots after a successful retry = %d, want 1", n)
	}
}

// An owner or admin stored in mixed case is still left out of the
// customer ranking.
func TestTopUsersLeaveOutAdminsWhateverTheCase(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()
	suffix := time.Now().Format("150405.000000000")
	prod := &model.Product{Name: "Top " + suffix, Slug: "top-admin-" + suffix, Type: "saas"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	plan := &model.Plan{ProductID: prod.ID, Name: "P", Slug: "top-admin-" + suffix, LicenseType: "perpetual",
		LicenseModel: "standard", Active: true}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	admin := "Admin-" + suffix + "@Example.com"
	if _, err := s.DB.NewRaw("INSERT INTO users (id, email, name, role) VALUES (?, ?, 'A', 'admin')", store.NewID(), admin).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	for _, email := range []string{admin, "customer-" + suffix + "@example.com"} {
		if err := s.CreateLicense(ctx, &model.License{ProductID: prod.ID, PlanID: plan.ID, Email: email,
			LicenseKey: "KG-TOP-" + email, Status: model.StatusActive}); err != nil {
			t.Fatal(err)
		}
	}
	top, err := s.GetTopUsers(ctx, prod.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(top) != 1 || top[0].Email != "customer-"+suffix+"@example.com" {
		t.Fatalf("top users = %+v, want the customer alone", top)
	}
}

// A trial that ends with no plan to fall back to expires, and its
// floating sessions end with it, as on every other way a licence stops.
func TestEndedTrialHoldsNoFloatingSession(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()
	suffix := time.Now().Format("150405.000000000")
	prod := &model.Product{Name: "Trial " + suffix, Slug: "trial-float-" + suffix, Type: "desktop"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	plan := &model.Plan{ProductID: prod.ID, Name: "T", Slug: "trial-float-" + suffix, LicenseType: "trial",
		LicenseModel: "floating", MaxActivations: 1, TrialDays: 7, Active: true}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	ended := time.Now().Add(-time.Hour)
	lic := &model.License{ProductID: prod.ID, PlanID: plan.ID, Email: "trial-" + suffix + "@example.com",
		LicenseKey: "KG-TRF-" + suffix, Status: model.StatusTrialing, ValidUntil: &ended}
	if err := s.CreateLicense(ctx, lic); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckOutFloating(ctx, &model.FloatingSession{LicenseID: lic.ID, Identifier: "pc",
		ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MoveEndedTrialToFallback(ctx, lic.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	var status string
	var n int
	if err := s.DB.NewRaw("SELECT status, (SELECT count(*) FROM floating_sessions WHERE license_id = l.id) FROM licenses l WHERE id = ?", lic.ID).Scan(ctx, &status, &n); err != nil {
		t.Fatal(err)
	}
	if status != model.StatusExpired || n != 0 {
		t.Fatalf("ended trial = %s with %d sessions, want expired with none", status, n)
	}
}
