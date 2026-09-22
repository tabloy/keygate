package store

import (
	"context"

	"github.com/uptrace/bun"
)

// Page is the slice of a list a caller wants. It is the store's half
// of the contract the admin API answers with: a bounded window plus
// the number of rows the filter matched, so a client can tell how far
// the list goes without asking for all of it.
//
// A Limit of zero or less means "every row". Only callers inside this
// process build one of those — the analytics sweep that walks every
// product, the public plan list of a single product. A request never
// can: the handler clamps what it reads from the query string before
// it gets here, so an unbounded page cannot be asked for over HTTP.
type Page struct {
	Limit  int
	Offset int
}

// All is that unbounded page, named so the call sites that mean it say so.
var All = Page{}

// scanPage runs the query for one page and returns how many rows the
// filter matched in total.
//
// The count is a second statement, so it is only run when a limit is
// in force — for an unbounded page the rows in hand are the total, and
// asking the database to count them again would be a query for
// nothing. The count ignores limit and offset, which is what makes it
// a total rather than a page size.
func scanPage(ctx context.Context, q *bun.SelectQuery, p Page) (int, error) {
	if p.Limit <= 0 {
		return 0, q.Scan(ctx)
	}
	if p.Offset > 0 {
		q = q.Offset(p.Offset)
	}
	return q.Limit(p.Limit).ScanAndCount(ctx)
}

// Sort is a validated ordering for a list query.
//
// Expr is a SQL expression, so it must never be built from anything a
// caller sent. The handler picks it out of a fixed map keyed by the
// name the client asked for; an unrecognised name is refused there and
// never reaches this struct. Keeping the resolved expression here
// rather than the client's word for it is what makes the concatenation
// in applySort safe.
type Sort struct {
	Expr string
	Desc bool
}

// applySort puts a stable ORDER BY on a paged query.
//
// The tiebreaker is not decoration. Paging is OFFSET/LIMIT over a
// fresh query per page, so rows that compare equal under the chosen
// column may come back in a different order each time; a row can then
// appear on two pages, or on none. A unique column last makes the
// order total, which is what makes the pages line up.
//
// NULLS LAST on both directions is deliberate. valid_until is empty
// for a perpetual license, and an order by expiry that leads with
// every license that never expires has not answered the question that
// was asked, whichever way round it was asked.
func applySort(q *bun.SelectQuery, s Sort, tiebreak string) *bun.SelectQuery {
	dir := "ASC"
	if s.Desc {
		dir = "DESC"
	}
	return q.OrderExpr(s.Expr + " " + dir + " NULLS LAST").OrderExpr(tiebreak + " DESC")
}
