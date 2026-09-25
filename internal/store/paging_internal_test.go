package store

import (
	"strings"
	"testing"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
)

func sortedQueryString(t *testing.T, s Sort, tiebreak string) string {
	t.Helper()
	db := bun.NewDB(nil, pgdialect.New())
	return applySort(db.NewSelect().Table("licenses").Column("id"), s, tiebreak).String()
}

// The tiebreaker has to reach the SQL, not merely be intended.
//
// This is asserted against the generated statement rather than against
// rows that come back, because the bug it guards is invisible in a
// small table: Postgres will happily return six tied rows in a
// consistent order, and only start reordering them once the plan
// changes under real data. By then the symptom is a customer saying a
// license appeared twice while they were paging.
func TestApplySortAlwaysAppendsTheTiebreaker(t *testing.T) {
	for _, s := range []Sort{
		{Expr: "license.created_at", Desc: true},
		{Expr: "license.email"},
		{Expr: "license.status", Desc: true},
	} {
		sql := sortedQueryString(t, s, "license.id")
		orderBy := sql[strings.Index(sql, "ORDER BY"):]
		if !strings.Contains(orderBy, "license.id DESC") {
			t.Errorf("Sort %+v produced %q, which has no unique tiebreaker", s, orderBy)
		}
		// And it has to come last: a tiebreaker ahead of the chosen
		// column decides the whole order by itself.
		if strings.Index(orderBy, "license.id DESC") < strings.Index(orderBy, s.Expr) {
			t.Errorf("Sort %+v produced %q, tiebreaker is not last", s, orderBy)
		}
	}
}

// Direction and null placement, again read off the statement: "sort by
// expiry, furthest away first" must not answer with the licenses that
// have no expiry at all.
func TestApplySortDirectionAndNulls(t *testing.T) {
	for _, tc := range []struct {
		sort Sort
		want string
	}{
		{Sort{Expr: "license.valid_until"}, "license.valid_until ASC NULLS LAST"},
		{Sort{Expr: "license.valid_until", Desc: true}, "license.valid_until DESC NULLS LAST"},
	} {
		sql := sortedQueryString(t, tc.sort, "license.id")
		if !strings.Contains(sql, tc.want) {
			t.Errorf("Sort %+v produced %q, want it to contain %q", tc.sort, sql, tc.want)
		}
	}
}

// Webhook deliveries do not go through applySort, because the endpoint
// takes no ?sort=; the order is fixed. That put it outside the guard
// above, and it shipped ordered by created_at alone while the contract
// page promised every paginated list a unique tiebreaker.
func TestWebhookDeliveryOrderHasATiebreaker(t *testing.T) {
	parts := strings.Split(webhookDeliveryOrder, ",")
	last := strings.TrimSpace(parts[len(parts)-1])
	if !strings.HasPrefix(last, "id ") && last != "id" {
		t.Errorf("webhookDeliveryOrder = %q; the last key is %q, which is not the unique id",
			webhookDeliveryOrder, last)
	}
	if len(parts) < 2 {
		t.Errorf("webhookDeliveryOrder = %q has only one key, so tied rows have no defined order",
			webhookDeliveryOrder)
	}
}
