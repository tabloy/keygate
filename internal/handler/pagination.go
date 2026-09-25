package handler

import (
	"maps"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/tabloy/keygate/internal/store"
	"github.com/tabloy/keygate/pkg/response"
)

// Every list endpoint answers the same two query parameters and the
// same three numbers, so a client that can page one of them can page
// all of them.
//
// The limit is clamped rather than refused: a caller asking for more
// than the cap gets the cap, and the limit that was actually applied
// comes back in the response, which is what a page count has to be
// worked out from. Without that echo a dashboard asking for 1000 and
// silently served 200 would draw a pager five times too short.
const (
	defaultListLimit = 50
	maxListLimit     = 200
)

// listPage reads the window a list request asks for. Anything
// nonsensical — zero, negative, unparseable — reads as "not asked
// for" and takes the default; in particular a limit of 0 does not
// mean "everything", which is a request no HTTP caller can make.
func listPage(c *gin.Context) store.Page {
	limit := queryInt(c, "limit", defaultListLimit)
	if limit < 1 {
		limit = defaultListLimit
	}
	if limit > maxListLimit {
		limit = maxListLimit
	}
	offset := max(queryInt(c, "offset", 0), 0)
	return store.Page{Limit: limit, Offset: offset}
}

// listOK writes the one shape every list endpoint answers with: the
// rows under their own name, and total/limit/offset beside them.
//
// An empty list is an empty array, never null: a client that iterates
// the field should not have to special-case "no rows" differently
// from "one row".
func listOK[T any](c *gin.Context, key string, items []T, total int, p store.Page, extra ...gin.H) {
	if items == nil {
		items = []T{}
	}
	body := gin.H{key: items, "total": total, "limit": p.Limit, "offset": p.Offset}
	for _, e := range extra {
		maps.Copy(body, e)
	}
	response.OK(c, body)
}

// sortCol is one entry in an endpoint's sortable-column allowlist:
// the SQL expression the client-facing name resolves to, and the
// direction that name reads in when the client has not said. Dates
// want newest first; names want A to Z. A single default per endpoint
// would get one of the two wrong on the first click.
type sortCol struct {
	Expr string
	Desc bool
}

// listSort reads ?sort= and ?order= for a list endpoint.
//
// `allowed` maps the names a client may ask for to the column each one
// means. The map is the whole of the validation: a name that is not a
// key is refused with 400, so nothing a caller typed is ever spliced
// into an ORDER BY. Refusing rather than quietly falling back to the
// default is deliberate — a client that misspells a column and is
// served a different order has no way to notice.
//
// An absent ?sort= is not an error; it takes the endpoint's default,
// which is what every caller that does not care about order gets.
func listSort(c *gin.Context, allowed map[string]sortCol, defCol string) (store.Sort, bool) {
	name := c.Query("sort")
	if name == "" {
		name = defCol
	}
	col, ok := allowed[name]
	if !ok {
		names := make([]string, 0, len(allowed))
		for k := range allowed {
			names = append(names, k)
		}
		sort.Strings(names)
		response.Err(c, http.StatusBadRequest, "INVALID_SORT",
			"unknown sort column "+strconv.Quote(clipForMessage(name))+", expected one of: "+strings.Join(names, ", "))
		return store.Sort{}, false
	}

	desc := col.Desc
	switch c.Query("order") {
	case "":
		// Not asked for: the column's natural direction.
	case "asc":
		desc = false
	case "desc":
		desc = true
	default:
		response.Err(c, http.StatusBadRequest, "INVALID_ORDER",
			`order must be "asc" or "desc"`)
		return store.Sort{}, false
	}
	return store.Sort{Expr: col.Expr, Desc: desc}, true
}

// clipForMessage bounds a piece of the request that is quoted back in
// an error.
//
// Saying which column was not recognised is what makes the refusal
// useful, but the value came from the caller and a query string can
// carry a great deal of it. Echoing it whole turns a 400 into an
// amplifier: fifty kilobytes in, fifty kilobytes back, and the same
// again in every log line that records the error. A name long enough
// to be cut off is a name that was never going to match.
func clipForMessage(s string) string {
	const max = 64
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
