package store

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/model"
)

func TestEscapeLike(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", ""},
		{"plain", "plain"},
		{"kg_live_ab", `kg\_live\_ab`},
		{"100%", `100\%`},
		{`dir\file`, `dir\\file`},
		{`\_%\\`, `\\\_\%\\\\`},
	} {
		if got := escapeLike(tc.in); got != tc.want {
			t.Errorf("escapeLike(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Search terms in the admin list queries match literally: %, _ and \
// stand for themselves rather than acting as LIKE wildcards or the
// escape character.
func TestAdminListSearchesAreLiteral(t *testing.T) {
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
	suffix := time.Now().Format("150405.000000")

	underTerm := "kg_live_" + suffix
	pctTerm := "100%-" + suffix
	slashTerm := `dir\file-` + suffix
	terms := []string{underTerm, pctTerm, slashTerm}
	allQueries := []string{underTerm, pctTerm, slashTerm, "_", "%", `\`}

	// Each decoy value matches one of the terms above if _, % or \ is
	// left unescaped in ILIKE, while containing none of those characters
	// itself.
	decoyText := "kgxlive1" + suffix + " 100pct-" + suffix + " dirfile-" + suffix
	decoySlug := "kgxlive1" + suffix + "-100pct-" + suffix + "-dirfile-" + suffix

	targetProd := &model.Product{Name: pctTerm + " " + slashTerm, Slug: underTerm, Type: "desktop"}
	decoyProd := &model.Product{Name: decoyText, Slug: decoySlug, Type: "desktop"}
	for _, p := range []*model.Product{targetProd, decoyProd} {
		if err := s.CreateProduct(ctx, p); err != nil {
			t.Fatalf("create product: %v", err)
		}
	}
	for _, q := range terms {
		got, total, err := s.ListProducts(ctx, q, nil, Page{Limit: 50})
		if err != nil {
			t.Fatalf("ListProducts(%q): %v", q, err)
		}
		if total != 1 || len(got) != 1 || got[0].ID != targetProd.ID {
			t.Errorf("ListProducts(%q) matched %d rows (total=%d), want only target product %s", q, len(got), total, targetProd.ID)
		}
	}
	for _, q := range []string{"_", "%", `\`} {
		got, _, err := s.ListProducts(ctx, q, nil, All)
		if err != nil {
			t.Fatalf("ListProducts(%q): %v", q, err)
		}
		foundTarget := false
		for _, p := range got {
			if p.ID == targetProd.ID {
				foundTarget = true
			}
			if p.ID == decoyProd.ID {
				t.Errorf("ListProducts(%q) matched decoy product %s", q, decoyProd.ID)
			}
		}
		if !foundTarget {
			t.Errorf("ListProducts(%q) did not find target product %s", q, targetProd.ID)
		}
	}

	targetPlan := &model.Plan{
		ProductID: targetProd.ID, Name: pctTerm + " " + slashTerm, Slug: underTerm,
		LicenseType: "perpetual", LicenseModel: "standard",
	}
	decoyPlan := &model.Plan{
		ProductID: targetProd.ID, Name: decoyText, Slug: decoySlug,
		LicenseType: "perpetual", LicenseModel: "standard",
	}
	for _, p := range []*model.Plan{targetPlan, decoyPlan} {
		if err := s.CreatePlan(ctx, p); err != nil {
			t.Fatalf("create plan: %v", err)
		}
	}
	for _, q := range allQueries {
		got, total, err := s.ListPlans(ctx, targetProd.ID, q, Page{Limit: 50})
		if err != nil {
			t.Fatalf("ListPlans(%q): %v", q, err)
		}
		if total != 1 || len(got) != 1 || got[0].ID != targetPlan.ID {
			t.Errorf("ListPlans(%q) matched %d rows (total=%d), want only target plan %s", q, len(got), total, targetPlan.ID)
		}
	}

	targetKey := &model.APIKey{
		ProductID: targetProd.ID, Name: pctTerm + " " + slashTerm,
		Prefix: underTerm, Scopes: []string{model.ScopeLicensesWrite},
	}
	decoyKey := &model.APIKey{
		ProductID: targetProd.ID, Name: decoyText,
		Prefix: decoySlug, Scopes: []string{model.ScopeLicensesWrite},
	}
	for _, ak := range []*model.APIKey{targetKey, decoyKey} {
		if err := s.CreateAPIKey(ctx, ak, GenerateRawAPIKey()); err != nil {
			t.Fatalf("create api key: %v", err)
		}
	}
	for _, q := range allQueries {
		got, total, err := s.ListAPIKeys(ctx, targetProd.ID, q, Page{Limit: 50})
		if err != nil {
			t.Fatalf("ListAPIKeys(%q): %v", q, err)
		}
		if total != 1 || len(got) != 1 || got[0].ID != targetKey.ID {
			t.Errorf("ListAPIKeys(%q) matched %d rows (total=%d), want only target key %s", q, len(got), total, targetKey.ID)
		}
	}

	targetAddon := &model.Addon{
		ProductID: targetProd.ID, Name: pctTerm + " " + slashTerm, Slug: "addon-target-" + suffix,
		Feature: underTerm, ValueType: "bool", Value: "true", Active: true,
	}
	decoyAddon := &model.Addon{
		ProductID: targetProd.ID, Name: decoyText, Slug: "addon-decoy-" + suffix,
		Feature: decoySlug, ValueType: "bool", Value: "true", Active: true,
	}
	for _, a := range []*model.Addon{targetAddon, decoyAddon} {
		if err := s.CreateAddon(ctx, a); err != nil {
			t.Fatalf("create addon: %v", err)
		}
	}
	for _, q := range allQueries {
		got, total, err := s.ListAddons(ctx, targetProd.ID, q, Page{Limit: 50})
		if err != nil {
			t.Fatalf("ListAddons(%q): %v", q, err)
		}
		if total != 1 || len(got) != 1 || got[0].ID != targetAddon.ID {
			t.Errorf("ListAddons(%q) matched %d rows (total=%d), want only target addon %s", q, len(got), total, targetAddon.ID)
		}
	}

	targetWebhook := &model.Webhook{
		ProductID: targetProd.ID, URL: "https://example.com/" + underTerm + "?q=" + pctTerm + "&p=" + slashTerm,
		Secret: "s", Events: []string{"license.created"}, Active: true,
	}
	decoyWebhook := &model.Webhook{
		ProductID: targetProd.ID, URL: "https://example.com/" + decoySlug,
		Secret: "s", Events: []string{"license.created"}, Active: true,
	}
	for _, w := range []*model.Webhook{targetWebhook, decoyWebhook} {
		if err := s.CreateWebhook(ctx, w); err != nil {
			t.Fatalf("create webhook: %v", err)
		}
	}
	for _, q := range allQueries {
		got, total, err := s.ListWebhooks(ctx, targetProd.ID, q, Page{Limit: 50})
		if err != nil {
			t.Fatalf("ListWebhooks(%q): %v", q, err)
		}
		if total != 1 || len(got) != 1 || got[0].ID != targetWebhook.ID {
			t.Errorf("ListWebhooks(%q) matched %d rows (total=%d), want only target webhook %s", q, len(got), total, targetWebhook.ID)
		}
	}

	targetUser := &model.User{Email: underTerm + "@example.com", Name: pctTerm + " " + slashTerm}
	decoyUser := &model.User{Email: decoySlug + "@example.com", Name: decoyText}
	defer func() {
		_, _ = s.DB.NewRaw("DELETE FROM users WHERE email IN (?, ?)", targetUser.Email, decoyUser.Email).Exec(ctx)
	}()
	for _, u := range []*model.User{targetUser, decoyUser} {
		if err := s.UpsertUser(ctx, u); err != nil {
			t.Fatalf("upsert user: %v", err)
		}
	}
	for _, q := range terms {
		got, total, err := s.ListUsers(ctx, q, 0, 50)
		if err != nil {
			t.Fatalf("ListUsers(%q): %v", q, err)
		}
		if total != 1 || len(got) != 1 || got[0].ID != targetUser.ID {
			t.Errorf("ListUsers(%q) matched %d rows (total=%d), want only target user %s", q, len(got), total, targetUser.ID)
		}
	}
	for _, q := range []string{"_", "%", `\`} {
		got, _, err := s.ListUsers(ctx, q, 0, 500)
		if err != nil {
			t.Fatalf("ListUsers(%q): %v", q, err)
		}
		foundTarget := false
		for _, u := range got {
			if u.ID == targetUser.ID {
				foundTarget = true
			}
			if u.ID == decoyUser.ID {
				t.Errorf("ListUsers(%q) matched decoy user %s", q, decoyUser.ID)
			}
		}
		if !foundTarget {
			t.Errorf("ListUsers(%q) did not find target user %s", q, targetUser.ID)
		}
	}
}
