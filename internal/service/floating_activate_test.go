package service

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
	"github.com/tabloy/keygate/pkg/apperr"
)

// A floating plan's limit counts checked-out sessions. Device
// activations used to be counted separately against the same number, so
// a one-seat floating licence ran on two machines at once: one checked
// out, one activated. Activate and verify are now refused on a floating
// plan, for an activation made before (or before the plan was floating)
// as much as for a new one; deactivate still frees the row.
func TestFloatingPlanRefusesDeviceActivation(t *testing.T) {
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
	suffix := time.Now().Format("150405.000000")
	prod := &model.Product{Name: "Floating", Slug: "fl-act-" + suffix, Type: "desktop"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	plan := &model.Plan{ProductID: prod.ID, Name: "Float", Slug: "fl-act-" + suffix, LicenseType: "perpetual",
		LicenseModel: "floating", MaxActivations: 1, FloatingTimeout: 30, Active: true}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	key := "KG-FLACT-" + suffix
	lic := &model.License{ProductID: prod.ID, PlanID: plan.ID, Email: "fl-" + suffix + "@example.com", LicenseKey: key, Status: model.StatusActive}
	if err := s.CreateLicense(ctx, lic); err != nil {
		t.Fatal(err)
	}
	// An activation from before the check.
	if err := s.ActivateWithinLimit(ctx, &model.Activation{LicenseID: lic.ID, Identifier: "old-machine", IdentifierType: "device"}, 5); err != nil {
		t.Fatal(err)
	}

	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	ls := NewLicenseService(s, priv, slog.Default(), nil, nil)
	fs := NewFloatingService(s, slog.Default())
	if _, err := fs.CheckOut(ctx, CheckOutInput{LicenseKey: key, Identifier: "ws-1"}); err != nil {
		t.Fatalf("checkout: %v", err)
	}
	_, err = ls.Activate(ctx, ActivateInput{LicenseKey: key, Identifier: "ws-2"})
	var ae *apperr.AppError
	if !errors.As(err, &ae) || ae.Status != 409 || ae.Code != "FLOATING_LICENSE" {
		t.Fatalf("activate on a floating plan = %v, want 409 FLOATING_LICENSE", err)
	}
	for name, call := range map[string]func() error{
		"re-activate an old activation": func() error {
			_, err := ls.Activate(ctx, ActivateInput{LicenseKey: key, Identifier: "old-machine"})
			return err
		},
		"verify an old activation": func() error {
			_, err := ls.Verify(ctx, VerifyInput{LicenseKey: key, Identifier: "old-machine"})
			return err
		},
	} {
		err := call()
		if !errors.As(err, &ae) || ae.Status != 409 || ae.Code != "FLOATING_LICENSE" {
			t.Errorf("%s = %v, want 409 FLOATING_LICENSE", name, err)
		}
	}
	if err := ls.Deactivate(ctx, DeactivateInput{LicenseKey: key, Identifier: "old-machine"}); err != nil {
		t.Errorf("deactivate the old activation: %v", err)
	}

	// A standard plan moved to floating: its activations stop working too.
	std := &model.Plan{ProductID: prod.ID, Name: "Std", Slug: "fl-std-" + suffix, LicenseType: "perpetual",
		LicenseModel: "standard", MaxActivations: 1, Active: true}
	if err := s.CreatePlan(ctx, std); err != nil {
		t.Fatal(err)
	}
	key2 := "KG-FLACT2-" + suffix
	lic2 := &model.License{ProductID: prod.ID, PlanID: std.ID, Email: "fl2-" + suffix + "@example.com", LicenseKey: key2, Status: model.StatusActive}
	if err := s.CreateLicense(ctx, lic2); err != nil {
		t.Fatal(err)
	}
	if _, err := ls.Activate(ctx, ActivateInput{LicenseKey: key2, Identifier: "desk"}); err != nil {
		t.Fatalf("activate on the standard plan: %v", err)
	}
	if _, err := s.DB.NewUpdate().Model((*model.Plan)(nil)).Set("license_model = 'floating'").Where("id = ?", std.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := ls.Verify(ctx, VerifyInput{LicenseKey: key2, Identifier: "desk"}); !errors.As(err, &ae) || ae.Code != "FLOATING_LICENSE" {
		t.Errorf("verify after the plan became floating = %v, want 409 FLOATING_LICENSE", err)
	}
	if _, err := fs.CheckOut(ctx, CheckOutInput{LicenseKey: key2, Identifier: "laptop"}); err != nil {
		t.Errorf("the one seat is free for checkout: %v", err)
	}
}

// An addon of another product attached to a licence (refused now, but it
// once went through) grants that licence nothing.
func TestOtherProductsAddonGrantsNothing(t *testing.T) {
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
	suffix := time.Now().Format("150405.000000")
	var prods [2]*model.Product
	for i, name := range []string{"mine", "theirs"} {
		prods[i] = &model.Product{Name: name, Slug: "xaddon-" + name + "-" + suffix, Type: "desktop"}
		if err := s.CreateProduct(ctx, prods[i]); err != nil {
			t.Fatal(err)
		}
	}
	plan := &model.Plan{ProductID: prods[0].ID, Name: "Base", Slug: "xaddon-" + suffix, LicenseType: "perpetual", LicenseModel: "standard"}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	lic := &model.License{ProductID: prods[0].ID, PlanID: plan.ID, Email: "x-" + suffix + "@example.com", LicenseKey: "KG-XADDON-" + suffix, Status: model.StatusActive}
	if err := s.CreateLicense(ctx, lic); err != nil {
		t.Fatal(err)
	}
	for _, p := range prods {
		a := &model.Addon{ProductID: p.ID, Name: p.Name, Slug: "xa-" + p.Name + "-" + suffix, Feature: "feat_" + p.Name, ValueType: "bool", Value: "true"}
		if err := s.CreateAddon(ctx, a); err != nil {
			t.Fatal(err)
		}
		if _, err := s.AddLicenseAddon(ctx, &model.LicenseAddon{LicenseID: lic.ID, AddonID: a.ID, Enabled: true}); err != nil {
			t.Fatal(err)
		}
	}
	loaded, err := s.FindLicenseByKey(ctx, lic.LicenseKey)
	if err != nil {
		t.Fatal(err)
	}
	ents, err := licenseFeatures(ctx, s, loaded)
	if err != nil {
		t.Fatal(err)
	}
	got := featureValues(ents)
	if got["feat_mine"] != true || got["feat_theirs"] != nil {
		t.Fatalf("features = %v, want feat_mine only", got)
	}
}
