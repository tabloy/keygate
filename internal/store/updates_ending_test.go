package store

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/model"
)

// The reminder query must pick active perpetual licenses whose period
// ends inside the window and nothing else: not subscriptions with a
// stale date, not lapsed or far-off periods, not inactive licenses.
func TestFindLicensesWithUpdatesEnding(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("skipping integration test: TEST_DATABASE_URL not set")
	}
	s, err := New(dsn)
	if err != nil {
		t.Skipf("skipping integration test: %v", err)
	}
	defer s.Close()
	if err := s.RunMigrations("../../db/migrations"); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	ctx := context.Background()
	suffix := time.Now().Format("150405.000")
	prod := &model.Product{Name: "Ending", Slug: "ending-" + suffix, Type: "desktop", FeedLicenseRequired: true}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	perp := &model.Plan{ProductID: prod.ID, Name: "P", Slug: "ep-" + suffix, LicenseType: "perpetual", LicenseModel: "standard", RenewalDays: 365, StripeRenewalPriceID: "price_end_" + suffix}
	sub := &model.Plan{ProductID: prod.ID, Name: "S", Slug: "es-" + suffix, LicenseType: "subscription", LicenseModel: "standard"}
	for _, p := range []*model.Plan{perp, sub} {
		if err := s.CreatePlan(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	soon := time.Now().Add(5 * 24 * time.Hour)
	far := time.Now().Add(60 * 24 * time.Hour)
	past := time.Now().Add(-24 * time.Hour)
	mk := func(name string, plan *model.Plan, until *time.Time, status string) {
		l := &model.License{ProductID: prod.ID, PlanID: plan.ID, Email: name + "-" + suffix + "@example.com", LicenseKey: "KEY-" + name + "-" + suffix, Status: status, UpdatesUntil: until}
		if err := s.CreateLicense(ctx, l); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	mk("due", perp, &soon, model.StatusActive)
	mk("far", perp, &far, model.StatusActive)
	mk("past", perp, &past, model.StatusActive)
	mk("revoked", perp, &soon, model.StatusRevoked)
	mk("sub", sub, &soon, model.StatusActive)

	got, err := s.FindLicensesWithUpdatesEnding(ctx, time.Now(), time.Now().Add(14*24*time.Hour))
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	var mine []string
	for _, l := range got {
		if l.ProductID == prod.ID {
			mine = append(mine, l.Email)
			if l.Plan == nil || l.Product == nil {
				t.Fatalf("relations not loaded for %s", l.Email)
			}
		}
	}
	if len(mine) != 1 || mine[0] != "due-"+suffix+"@example.com" {
		t.Fatalf("reminder candidates: %v", mine)
	}
}

// The admin's cutoff edit applies only against the value the admin
// saw; a renewal committed in between makes it return false.
func TestSetLicenseUpdatesUntil_CompareAndSet(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("skipping integration test: TEST_DATABASE_URL not set")
	}
	s, err := New(dsn)
	if err != nil {
		t.Skipf("skipping integration test: %v", err)
	}
	defer s.Close()
	ctx := context.Background()
	suffix := time.Now().Format("150405.000")
	prod := &model.Product{Name: "CAS", Slug: "cas-" + suffix, Type: "desktop", FeedLicenseRequired: true}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	plan := &model.Plan{ProductID: prod.ID, Name: "P", Slug: "cas-" + suffix, LicenseType: "perpetual", LicenseModel: "standard"}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	seen := time.Now().Add(10 * 24 * time.Hour).Truncate(time.Second)
	lic := &model.License{ProductID: prod.ID, PlanID: plan.ID, Email: "cas-" + suffix + "@example.com", LicenseKey: "KEY-cas-" + suffix, Status: model.StatusActive, UpdatesUntil: &seen}
	if err := s.CreateLicense(ctx, lic); err != nil {
		t.Fatal(err)
	}
	// A renewal lands after the admin loaded the license.
	r := &model.LicenseRenewal{LicenseID: lic.ID, StripeCheckoutSessionID: "cs_cas_" + suffix, Days: 365}
	if err := s.ApplyLicenseRenewal(ctx, r); err != nil {
		t.Fatal(err)
	}
	edit := time.Now().Add(30 * 24 * time.Hour)
	if ok, err := s.SetLicenseUpdatesUntil(ctx, lic.ID, &seen, &edit, false); err != nil || ok {
		t.Fatalf("stale edit must be refused: ok=%v err=%v", ok, err)
	}
	got, _ := s.FindLicenseByID(ctx, lic.ID)
	if got.UpdatesUntil == nil || !got.UpdatesUntil.Equal(*r.UpdatesUntil) {
		t.Fatalf("stale edit overwrote the renewal: %v", got.UpdatesUntil)
	}
	if ok, err := s.SetLicenseUpdatesUntil(ctx, lic.ID, r.UpdatesUntil, &edit, false); err != nil || !ok {
		t.Fatalf("edit on the current value: ok=%v err=%v", ok, err)
	}
	// Lifetime grant closes the ledger.
	if ok, err := s.SetLicenseUpdatesUntil(ctx, lic.ID, &edit, nil, true); err != nil || !ok {
		t.Fatalf("lifetime grant: ok=%v err=%v", ok, err)
	}
	row, _ := s.FindLicenseRenewalBySession(ctx, "cs_cas_"+suffix)
	if row.SupersededAt == nil {
		t.Fatal("lifetime grant did not close the ledger")
	}
}

// Two replicas claiming the same reminder: exactly one wins. A claim
// whose sender never marked it sent is won again after the lease; a
// sent one never is.
func TestClaimNotification_LeaseSemantics(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("skipping integration test: TEST_DATABASE_URL not set")
	}
	s, err := New(dsn)
	if err != nil {
		t.Skipf("skipping integration test: %v", err)
	}
	defer s.Close()
	ctx := context.Background()
	suffix := time.Now().Format("150405.000")
	prod := &model.Product{Name: "Claim", Slug: "claim-" + suffix, Type: "desktop"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	plan := &model.Plan{ProductID: prod.ID, Name: "P", Slug: "claim-" + suffix, LicenseType: "perpetual", LicenseModel: "standard"}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	lic := &model.License{ProductID: prod.ID, PlanID: plan.ID, Email: "claim-" + suffix + "@example.com", LicenseKey: "KEY-claim-" + suffix, Status: model.StatusActive}
	if err := s.CreateLicense(ctx, lic); err != nil {
		t.Fatal(err)
	}
	tag := "updates_14d_2027-01-01"
	var first string
	wins := 0
	for range 3 {
		token, err := s.ClaimNotification(ctx, lic.ID, tag)
		if err != nil {
			t.Fatal(err)
		}
		if token != "" {
			wins++
			first = token
		}
	}
	if wins != 1 {
		t.Fatalf("claims won within the lease: %d", wins)
	}
	// The sender died: the lease expires and the next run takes over
	// with a fresh token; the old token can neither close nor release.
	if _, err := s.DB.NewRaw("UPDATE notifications SET claimed_at = now() - interval '11 minutes' WHERE license_id = ? AND tag = ?", lic.ID, tag).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	second, _ := s.ClaimNotification(ctx, lic.ID, tag)
	if second == "" || second == first {
		t.Fatalf("expired unsent claim was not taken over with a new token: %q", second)
	}
	// The stale token can neither close nor release the newer claim.
	if err := s.EnqueueEmailAndCloseNotification(ctx, lic.Email, "s", "b", first); !errors.Is(err, ErrNotificationClaimLost) {
		t.Fatalf("a stale token closed the newer claim: %v", err)
	}
	if err := s.ReleaseNotification(ctx, first); err != nil {
		t.Fatal(err)
	}
	var sent bool
	var exists bool
	if err := s.DB.NewRaw("SELECT sent_at IS NOT NULL, true FROM notifications WHERE license_id = ? AND tag = ?", lic.ID, tag).Scan(ctx, &sent, &exists); err != nil {
		t.Fatalf("claim vanished or unreadable after a stale release: %v", err)
	}
	if sent || !exists {
		t.Fatalf("a stale token changed the newer claim: sent=%v exists=%v", sent, exists)
	}
	// Once sent, never again — even after the lease would have expired.
	if err := s.EnqueueEmailAndCloseNotification(ctx, lic.Email, "s", "b", second); err != nil {
		t.Fatal(err)
	}
	// The rolled-back attempt above must have queued nothing.
	var queued int
	if err := s.DB.NewRaw("SELECT count(*) FROM email_queue WHERE to_addr = ?", lic.Email).Scan(ctx, &queued); err != nil {
		t.Fatal(err)
	}
	if queued != 1 {
		t.Fatalf("queued mails: %d (a lost claim must roll its insert back)", queued)
	}
	if _, err := s.DB.NewRaw("DELETE FROM email_queue WHERE to_addr = ?", lic.Email).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.NewRaw("UPDATE notifications SET claimed_at = now() - interval '11 minutes' WHERE license_id = ? AND tag = ?", lic.ID, tag).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if token, _ := s.ClaimNotification(ctx, lic.ID, tag); token != "" {
		t.Fatal("a sent reminder was claimed again")
	}
	if token, _ := s.ClaimNotification(ctx, lic.ID, "updates_14d_2028-01-01"); token == "" {
		t.Fatal("a different period end is a new reminder")
	}
}
