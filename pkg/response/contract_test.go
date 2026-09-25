package response

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// An SDK branches on error.code, not on the HTTP status and not on the
// message. That only works if a code means exactly one thing: a client
// that sees INCOMPATIBLE_PRODUCT_TYPE must know whether to treat it as
// a rejected request body or as a state conflict, and it cannot know
// that if the same code arrives as 400 from one endpoint and 409 from
// another.
//
// This reads the server source rather than the routes. An HTTP sweep
// only sees the errors a test manages to provoke, and most of these
// hundred-odd codes need a very specific setup to reach; the source
// has every call site, including the ones no suite exercises.
//
// It parses rather than greps. A first version matched call sites with
// regexps and quietly missed two whole families: the named helpers
// (response.BadRequest and friends, which carry their code and status
// in the function name) and apperr.NotFound("QUOTA", …), which builds
// QUOTA_NOT_FOUND by concatenation and so has no literal to match. A
// missed call site is worse than no check, because it reads as a pass.
// The AST gives every call, and anything it cannot resolve is reported
// instead of skipped.
//
// If this fails, do not widen the map: either give the odd call site
// its own code, or move it to the status the code already means.

// call describes one way the codebase answers with an error.
//
// codeArg/statusArg are argument positions; -1 means the function
// supplies that value itself, from fixedCode/fixedStatus.
type call struct {
	codeArg, statusArg int
	fixedCode          string
	fixedStatus        int
	// suffix is appended to the resolved code argument. It exists for
	// apperr.NotFound(entity, id), which returns entity+"_NOT_FOUND".
	suffix string
}

var errCalls = map[string]call{
	// pkg/response — writes the reply.
	"response.Err":            {statusArg: 1, codeArg: 2},
	"response.ErrWithDetails": {statusArg: 1, codeArg: 2},
	"response.Conflict":       {statusArg: -1, fixedStatus: 409, codeArg: 1},
	"response.BadRequest":     {statusArg: -1, fixedStatus: 400, codeArg: -1, fixedCode: "BAD_REQUEST"},
	"response.Unauthorized":   {statusArg: -1, fixedStatus: 401, codeArg: -1, fixedCode: "UNAUTHORIZED"},
	"response.Forbidden":      {statusArg: -1, fixedStatus: 403, codeArg: -1, fixedCode: "FORBIDDEN"},
	"response.NotFound":       {statusArg: -1, fixedStatus: 404, codeArg: -1, fixedCode: "NOT_FOUND"},
	"response.Internal":       {statusArg: -1, fixedStatus: 500, codeArg: -1, fixedCode: "INTERNAL_ERROR"},

	// pkg/apperr — returns a typed error a handler later writes.
	"apperr.New":          {statusArg: 0, codeArg: 1},
	"apperr.Wrap":         {statusArg: 0, codeArg: 1},
	"apperr.Conflict":     {statusArg: -1, fixedStatus: 409, codeArg: 0},
	"apperr.BadRequest":   {statusArg: -1, fixedStatus: 400, codeArg: -1, fixedCode: "BAD_REQUEST"},
	"apperr.Unauthorized": {statusArg: -1, fixedStatus: 401, codeArg: -1, fixedCode: "UNAUTHORIZED"},
	"apperr.Forbidden":    {statusArg: -1, fixedStatus: 403, codeArg: -1, fixedCode: "FORBIDDEN"},
	"apperr.Internal":     {statusArg: -1, fixedStatus: 500, codeArg: -1, fixedCode: "INTERNAL_ERROR"},
	"apperr.NotFound":     {statusArg: -1, fixedStatus: 404, codeArg: 0, suffix: "_NOT_FOUND"},

	// internal/middleware — package-local wrappers that add the Abort
	// around a pkg/response call. They are named without a package
	// qualifier, so they are matched as bare identifiers.
	//
	// Middleware is the one layer that can refuse a request before any
	// handler runs, so its codes are as much a part of the contract as
	// a handler's, and for a while they were the codes nothing checked.
	"abortWithError":        {statusArg: 1, codeArg: 2},
	"abortWithErrorDetails": {statusArg: 1, codeArg: 2},
	"abortInternal":         {statusArg: -1, fixedStatus: 500, codeArg: -1, fixedCode: "INTERNAL_ERROR"},
}

// errCarriers are structs that hold an error code so a handler can
// write it later. A forwarded code is exactly the risk this test is
// for: nothing stops a carrier built with a code that means 400
// elsewhere from being written out at 409, which is the bug that
// prompted this test. So the carrier's construction sites are read
// like any other call site, and the forward is only accepted when it
// writes at the status the carrier is declared with.
//
// A carrier whose code field is named something other than "code" is
// not waived: its forwards show up as unresolved call sites. A second
// carrier that does use "code" would be waived, and its own literals
// never scanned. There is one carrier today; if that changes, list it.
var errCarriers = map[string]struct {
	codeField string // field name, for keyed literals
	codeIndex int    // field position, for positional literals
	status    int    // the status every site writes this carrier at
}{
	"feedGateProblem": {codeField: "code", codeIndex: 0, status: 409},
}

// carrierFields indexes errCarriers by the field name a forward reads
// (problem.code), with the status that forward must use.
var carrierFields = func() map[string]int {
	out := map[string]int{}
	for _, c := range errCarriers {
		out[c.codeField] = c.status
	}
	return out
}()

var statusNames = map[string]int{
	"StatusBadRequest": 400, "StatusUnauthorized": 401, "StatusPaymentRequired": 402,
	"StatusForbidden": 403, "StatusNotFound": 404, "StatusMethodNotAllowed": 405,
	"StatusConflict": 409, "StatusGone": 410, "StatusPreconditionFailed": 412,
	"StatusRequestEntityTooLarge": 413, "StatusUnprocessableEntity": 422,
	"StatusTooManyRequests": 429, "StatusInternalServerError": 500,
	"StatusNotImplemented": 501, "StatusBadGateway": 502,
	"StatusServiceUnavailable": 503, "StatusGatewayTimeout": 504,
}

type site struct {
	code   string
	status int
	where  string
}

func TestErrorCodeMapsToExactlyOneStatus(t *testing.T) {
	sites, passthrough, unresolved := scanErrorSites(t)

	if len(sites) < 200 {
		// The walk or the parse stopped working. Without this an empty
		// result reads as a clean pass.
		t.Fatalf("found only %d error call sites; the scan is broken, not the code", len(sites))
	}

	// A call site we could not read is not a pass. The pass-throughs
	// are the known exception: a handler re-emitting an *apperr.Error
	// it was handed supplies neither literal, and the code it forwards
	// is already checked where it was built.
	if len(unresolved) > 0 {
		sort.Strings(unresolved)
		t.Errorf("%d error call sites could not be resolved to a literal (code, status).\n"+
			"Make the argument a literal, or extend errCalls:\n\t%s",
			len(unresolved), strings.Join(unresolved, "\n\t"))
	}

	byCode := map[string]map[int][]string{}
	for _, s := range sites {
		if byCode[s.code] == nil {
			byCode[s.code] = map[int][]string{}
		}
		byCode[s.code][s.status] = append(byCode[s.code][s.status], s.where)
	}

	for code, statuses := range byCode {
		if len(statuses) == 1 {
			continue
		}
		var lines []string
		for st, where := range statuses {
			sort.Strings(where)
			lines = append(lines, strconv.Itoa(st)+" at "+strings.Join(dedupe(where), ", "))
		}
		sort.Strings(lines)
		t.Errorf("%s is returned with %d different statuses:\n\t%s",
			code, len(statuses), strings.Join(lines, "\n\t"))
	}

	t.Logf("%d error codes across %d call sites, each code with a single status "+
		"(%d pass-through sites forward an already-checked code)",
		len(byCode), len(sites), passthrough)
}

func scanErrorSites(t *testing.T) (sites []site, passthrough int, unresolved []string) {
	t.Helper()
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			switch info.Name() {
			case "web", "node_modules", ".git", "tmp", "worktrees", "vendor", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return perr
		}
		rel, _ := filepath.Rel(root, path)

		ast.Inspect(f, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.CallExpr:
				name, ok := selectorName(node.Fun)
				if !ok {
					return true
				}
				spec, known := errCalls[name]
				if !known {
					return true
				}
				pos := rel + ":" + strconv.Itoa(fset.Position(node.Pos()).Line)

				code, codeOK := spec.fixedCode, spec.codeArg < 0
				if !codeOK {
					code, codeOK = stringArg(node.Args, spec.codeArg)
					code += spec.suffix
				}
				status, statusOK := spec.fixedStatus, spec.statusArg < 0
				if !statusOK {
					status, statusOK = statusArg(node.Args, spec.statusArg)
				}

				switch {
				case codeOK && statusOK:
					sites = append(sites, site{code: code, status: status, where: rel})
				case !codeOK && !statusOK:
					// response.Err(c, ae.Status, ae.Code, ae.Message):
					// re-emitting an *apperr.AppError built elsewhere,
					// where both halves travel together.
					passthrough++
				case !codeOK && statusOK && forwardsCarrier(node.Args, spec.codeArg, status):
					// response.Conflict(c, problem.code, …): the code
					// comes from a carrier read above, and the status
					// matches what that carrier is declared with.
					passthrough++
				default:
					unresolved = append(unresolved, pos+" ("+name+")")
				}

			case *ast.CompositeLit:
				if id, ok := node.Type.(*ast.Ident); ok {
					if carrier, isCarrier := errCarriers[id.Name]; isCarrier {
						if code, ok := carrierCode(node, carrier.codeField, carrier.codeIndex); ok {
							sites = append(sites, site{code: code, status: carrier.status, where: rel})
						} else {
							unresolved = append(unresolved,
								rel+":"+strconv.Itoa(fset.Position(node.Pos()).Line)+
									" ("+id.Name+" literal)")
						}
						return true
					}
				}
				// &apperr.AppError{Status: 400, Code: "INVALID_INPUT"}
				name, ok := selectorName(node.Type)
				if !ok || name != "apperr.AppError" {
					return true
				}
				var code string
				var status int
				var haveCode, haveStatus bool
				for _, el := range node.Elts {
					kv, ok := el.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					key, ok := kv.Key.(*ast.Ident)
					if !ok {
						continue
					}
					switch key.Name {
					case "Code":
						code, haveCode = literalString(kv.Value)
					case "Status":
						status, haveStatus = literalStatus(kv.Value)
					}
				}
				if haveCode && haveStatus {
					sites = append(sites, site{code: code, status: status, where: rel})
				} else if haveCode != haveStatus {
					unresolved = append(unresolved,
						rel+":"+strconv.Itoa(fset.Position(node.Pos()).Line)+" (apperr.AppError literal)")
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	return sites, passthrough, unresolved
}

// selectorName renders pkg.Name / &pkg.Name for a call or type, and a
// bare name for a call to a function in the same package.
func selectorName(e ast.Expr) (string, bool) {
	if u, ok := e.(*ast.UnaryExpr); ok {
		e = u.X
	}
	if id, ok := e.(*ast.Ident); ok {
		return id.Name, true
	}
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok {
		return "", false
	}
	return pkg.Name + "." + sel.Sel.Name, true
}

// forwardsCarrier reports whether the code argument reads a carrier's
// code field, at the status that carrier is declared with.
func forwardsCarrier(args []ast.Expr, i, status int) bool {
	if i < 0 || i >= len(args) {
		return false
	}
	sel, ok := args[i].(*ast.SelectorExpr)
	if !ok {
		return false
	}
	want, known := carrierFields[sel.Sel.Name]
	return known && want == status
}

// carrierCode reads the code out of a carrier literal, written either
// keyed (Code: "X") or positionally ({"X", msg, nil}).
func carrierCode(lit *ast.CompositeLit, field string, index int) (string, bool) {
	for _, el := range lit.Elts {
		if kv, ok := el.(*ast.KeyValueExpr); ok {
			if key, ok := kv.Key.(*ast.Ident); ok && key.Name == field {
				return literalString(kv.Value)
			}
			return "", false
		}
	}
	if index < len(lit.Elts) {
		return literalString(lit.Elts[index])
	}
	return "", false
}

func stringArg(args []ast.Expr, i int) (string, bool) {
	if i < 0 || i >= len(args) {
		return "", false
	}
	return literalString(args[i])
}

func statusArg(args []ast.Expr, i int) (int, bool) {
	if i < 0 || i >= len(args) {
		return 0, false
	}
	return literalStatus(args[i])
}

func literalString(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	return s, err == nil
}

// literalStatus reads 409 and http.StatusConflict alike.
func literalStatus(e ast.Expr) (int, bool) {
	switch v := e.(type) {
	case *ast.BasicLit:
		if v.Kind != token.INT {
			return 0, false
		}
		n, err := strconv.Atoi(v.Value)
		return n, err == nil
	case *ast.SelectorExpr:
		pkg, ok := v.X.(*ast.Ident)
		if !ok || pkg.Name != "http" {
			return 0, false
		}
		n, ok := statusNames[v.Sel.Name]
		return n, ok
	}
	return 0, false
}

func dedupe(in []string) []string {
	out := in[:0:0]
	for i, s := range in {
		if i == 0 || s != in[i-1] {
			out = append(out, s)
		}
	}
	return out
}
