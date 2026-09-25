package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
)

// Six licenses on one product, sharing a created_at so the order is
// decided entirely by the tiebreaker, plus a spread of emails and
// expiry dates (two of which are absent) to sort by.
func seedSortableLicenses(t *testing.T, s *store.Store, ctx context.Context) (productID string, ids []string) {
	t.Helper()
	suffix := time.Now().Format("150405.000000")
	product := &model.Product{Name: "Sort Test", Slug: "sort-test-" + suffix, Type: "desktop"}
	if err := s.CreateProduct(ctx, product); err != nil {
		t.Fatalf("create product: %v", err)
	}
	plan := &model.Plan{
		ProductID: product.ID, Name: "Sort Plan", Slug: "sort-plan-" + suffix,
		LicenseType: "perpetual", LicenseModel: "standard", MaxActivations: 3,
	}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatalf("create plan: %v", err)
	}

	// One timestamp for every row: with no tiebreaker the database is
	// free to hand these back in a different order on every query.
	shared := time.Now().UTC().Truncate(time.Second)
	rows := []struct {
		email string
		until *time.Time
	}{
		{"e-" + suffix + "@example.com", ptr(shared.Add(72 * time.Hour))},
		{"c-" + suffix + "@example.com", nil},
		{"a-" + suffix + "@example.com", ptr(shared.Add(24 * time.Hour))},
		{"f-" + suffix + "@example.com", nil},
		{"b-" + suffix + "@example.com", ptr(shared.Add(48 * time.Hour))},
		{"d-" + suffix + "@example.com", ptr(shared.Add(96 * time.Hour))},
	}
	for i, r := range rows {
		lic := &model.License{
			ProductID: product.ID, PlanID: plan.ID, Email: r.email,
			LicenseKey: "SORTKEY-" + suffix + "-" + string(rune('a'+i)),
			Status:     "active", ValidUntil: r.until,
		}
		if err := s.CreateLicense(ctx, lic); err != nil {
			t.Fatalf("create license %d: %v", i, err)
		}
		ids = append(ids, lic.ID)
	}
	if _, err := s.DB.NewRaw("UPDATE licenses SET created_at = ? WHERE product_id = ?", shared, product.ID).Exec(ctx); err != nil {
		t.Fatalf("flatten created_at: %v", err)
	}
	return product.ID, ids
}

func ptr(t time.Time) *time.Time { return &t }

func listSorted(t *testing.T, s *store.Store, ctx context.Context, productID string, sort store.Sort, limit, offset int) []*model.License {
	t.Helper()
	out, _, err := s.ListLicenses(ctx, store.LicenseListFilter{
		ProductID: productID, Sort: sort, Limit: limit, Offset: offset,
	})
	if err != nil {
		t.Fatalf("ListLicenses: %v", err)
	}
	return out
}

// Ordering by a column every row shares must still be total, or the
// same row turns up on two pages and another turns up on none.
//
// This is the end-to-end half of the check and it is the weaker one:
// Postgres will return six tied rows consistently whether or not the
// tiebreaker is there, so this passing proves the query runs, not that
// the order is total. TestApplySortAlwaysAppendsTheTiebreaker asserts
// the guarantee itself, against the generated SQL.
func TestListLicensesPagesAreStableOnTiedValues(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()
	productID, ids := seedSortableLicenses(t, s, ctx)

	// Every row has the same status and the same created_at, so only
	// the tiebreaker separates them.
	for _, sort := range []store.Sort{
		{Expr: "license.status"},
		{Expr: "license.created_at", Desc: true},
	} {
		seen := map[string]int{}
		for offset := 0; offset < len(ids); offset += 2 {
			for _, l := range listSorted(t, s, ctx, productID, sort, 2, offset) {
				seen[l.ID]++
			}
		}
		if len(seen) != len(ids) {
			t.Errorf("sort %q over three pages of two returned %d distinct licenses, want %d",
				sort.Expr, len(seen), len(ids))
		}
		for id, n := range seen {
			if n != 1 {
				t.Errorf("sort %q returned license %s on %d pages, want 1", sort.Expr, id, n)
			}
		}
	}
}

// Paging the same order twice has to give the same pages. Without the
// tiebreaker this passes by luck often enough to be worth asserting.
func TestListLicensesPagingIsRepeatable(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()
	productID, ids := seedSortableLicenses(t, s, ctx)

	sort := store.Sort{Expr: "license.status"}
	var first []string
	for round := range 3 {
		var got []string
		for offset := 0; offset < len(ids); offset += 2 {
			for _, l := range listSorted(t, s, ctx, productID, sort, 2, offset) {
				got = append(got, l.ID)
			}
		}
		if round == 0 {
			first = got
			continue
		}
		if len(got) != len(first) {
			t.Fatalf("round %d returned %d rows, want %d", round, len(got), len(first))
		}
		for i := range got {
			if got[i] != first[i] {
				t.Errorf("round %d position %d = %s, want %s", round, i, got[i], first[i])
			}
		}
	}
}

// Sorting by expiry must not answer with every license that has none.
// A perpetual license is not "the furthest away" expiry; it is no
// expiry, and it belongs at the end whichever way the column is read.
func TestListLicensesSortsNullsLast(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()
	productID, ids := seedSortableLicenses(t, s, ctx)

	for _, desc := range []bool{false, true} {
		got := listSorted(t, s, ctx, productID, store.Sort{Expr: "license.valid_until", Desc: desc}, len(ids), 0)
		if len(got) != len(ids) {
			t.Fatalf("desc=%v returned %d rows, want %d", desc, len(got), len(ids))
		}
		seenNull := false
		for i, l := range got {
			if l.ValidUntil == nil {
				seenNull = true
				continue
			}
			if seenNull {
				t.Errorf("desc=%v: license with an expiry at position %d comes after one without", desc, i)
			}
		}
		if !seenNull {
			t.Fatal("fixture has no license without an expiry, so this proves nothing")
		}
	}
}

// Ascending and descending have to be genuine opposites, not the same
// list with the arrow flipped.
func TestListLicensesSortDirection(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()
	productID, ids := seedSortableLicenses(t, s, ctx)

	asc := listSorted(t, s, ctx, productID, store.Sort{Expr: "license.email"}, len(ids), 0)
	desc := listSorted(t, s, ctx, productID, store.Sort{Expr: "license.email", Desc: true}, len(ids), 0)
	if len(asc) != len(ids) || len(desc) != len(ids) {
		t.Fatalf("got %d asc / %d desc rows, want %d each", len(asc), len(desc), len(ids))
	}
	for i := 1; i < len(asc); i++ {
		if asc[i-1].Email > asc[i].Email {
			t.Errorf("ascending: %q comes before %q", asc[i-1].Email, asc[i].Email)
		}
	}
	for i := range asc {
		if asc[i].ID != desc[len(desc)-1-i].ID {
			t.Errorf("descending is not the reverse of ascending at position %d", i)
			break
		}
	}
}

// The default is what every caller that does not ask for an order
// gets, and it has to be a working order rather than an empty one.
func TestListLicensesDefaultsToNewestFirst(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()
	productID, ids := seedSortableLicenses(t, s, ctx)

	got := listSorted(t, s, ctx, productID, store.Sort{}, len(ids), 0)
	if len(got) != len(ids) {
		t.Fatalf("zero Sort returned %d rows, want %d", len(got), len(ids))
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].CreatedAt.Before(got[i].CreatedAt) {
			t.Errorf("default order is not newest first at position %d", i)
		}
	}
}

// The count on a list row has to be the number of activations that
// license actually has, and a license with none has to say 0 rather
// than leave the field for the dashboard to guess at.
func TestListLicensesCountsActivations(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()
	productID, ids := seedSortableLicenses(t, s, ctx)

	// Two activations on the first license, one on the second, none
	// on the rest.
	want := map[string]int{ids[0]: 2, ids[1]: 1}
	for id, n := range want {
		for i := range n {
			act := &model.Activation{
				LicenseID: id, Identifier: id + "-dev-" + string(rune('a'+i)), IdentifierType: "device",
			}
			if err := s.CreateActivation(ctx, act); err != nil {
				t.Fatalf("create activation: %v", err)
			}
		}
	}

	for _, l := range listSorted(t, s, ctx, productID, store.Sort{}, len(ids), 0) {
		if got := l.ActivationCount; got != want[l.ID] {
			t.Errorf("license %s ActivationCount = %d, want %d", l.ID, got, want[l.ID])
		}
	}

	// The detail read has to agree with the row that was clicked on.
	one, err := s.FindLicenseByIDWithCounts(ctx, ids[0])
	if err != nil {
		t.Fatalf("FindLicenseByIDWithCounts: %v", err)
	}
	if one.ActivationCount != 2 {
		t.Errorf("detail ActivationCount = %d, want 2", one.ActivationCount)
	}
}

// An empty page must not turn into a query with an empty IN list,
// which is a syntax error in Postgres rather than "no rows".
func TestListLicensesEmptyPageDoesNotQueryActivations(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()
	productID, ids := seedSortableLicenses(t, s, ctx)

	out, total, err := s.ListLicenses(ctx, store.LicenseListFilter{
		ProductID: productID, Limit: 2, Offset: len(ids) + 10,
	})
	if err != nil {
		t.Fatalf("ListLicenses past the end: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("got %d rows past the end, want 0", len(out))
	}
	if total != len(ids) {
		t.Errorf("total = %d, want %d", total, len(ids))
	}
}
