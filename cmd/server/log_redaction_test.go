package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/tabloy/keygate/pkg/response"
)

// An updater that cannot set headers sends its credential as a query
// parameter. It must not survive into a log line.
func TestRedactPath(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/api/v1/feeds/app/sparkle.xml", "/api/v1/feeds/app/sparkle.xml"},
		{"/f?platform=darwin", "/f?platform=darwin"},
		{"/f?license_key=KG-SECRET-1234", "/f?license_key=redacted"},
		{"/f?platform=darwin&license_key=KG-SECRET&channel=beta", "/f?channel=beta&license_key=redacted&platform=darwin"},
		{"/f?license_token=abc.def", "/f?license_token=redacted"},
		{"/f?license_key=a&license_key=b", "/f?license_key=redacted"},
		{"/f?%zz", "/f?<redacted>"},

		// The customer portal addresses a licence by its key, so the
		// credential is a path segment rather than a parameter and
		// rides on every request a signed-in customer makes there.
		// Redacting only the query string left these lines holding a
		// working key.
		{
			"/api/v1/portal/licenses/KG-AAAA-BBBB-CCCC-DDDD/activations",
			"/api/v1/portal/licenses/redacted/activations",
		},
		{
			"/api/v1/portal/licenses/KG-AAAA-BBBB-CCCC-DDDD/activations/act_123",
			"/api/v1/portal/licenses/redacted/activations/act_123",
		},
		// The key as the last segment, with nothing after it.
		{
			"/api/v1/portal/licenses/KG-AAAA-BBBB-CCCC-DDDD",
			"/api/v1/portal/licenses/redacted",
		},
		// Both kinds at once.
		{
			"/api/v1/portal/licenses/KG-AAAA-BBBB-CCCC-DDDD/activations?license_key=KG-OTHER",
			"/api/v1/portal/licenses/redacted/activations?license_key=redacted",
		},
		// A route that only looks similar keeps its path.
		{"/api/v1/admin/licenses/abc123/key", "/api/v1/admin/licenses/abc123/key"},
	}
	for _, c := range cases {
		if got := redactPath(c.in); got != c.want {
			t.Errorf("redactPath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// The 500 logger names the route it was reached by, never the URL that
// reached it: a licence key sits in the path of every portal
// activation request, and an error there would otherwise write the
// customer's credential into the log.
func TestInternalLogsRouteTemplateNotPath(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	const key = "KG-SECRET1-SECRET2-SECRET3-SECRET4"
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/api/v1/portal/licenses/:license_key/activations", func(c *gin.Context) {
		response.Internal(c, errors.New("database is unreachable"))
	})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/portal/licenses/"+key+"/activations", nil))

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
	logged := buf.String()
	if strings.Contains(logged, key) {
		t.Errorf("the licence key reached the log: %s", logged)
	}
	if !strings.Contains(logged, "/api/v1/portal/licenses/:license_key/activations") {
		t.Errorf("the log does not name the route that failed: %s", logged)
	}
	if !strings.Contains(logged, "database is unreachable") {
		t.Errorf("the log does not carry the cause: %s", logged)
	}
	// The body a caller sees says nothing about any of it.
	if strings.Contains(w.Body.String(), "database is unreachable") || strings.Contains(w.Body.String(), key) {
		t.Errorf("the response leaked internals: %s", w.Body.String())
	}
}

// A panic must not spill the credential the request carried. Gin's
// own recovery dumps the request with only Authorization masked.
func TestRedactedRecoveryLogsNoCredentials(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	r := gin.New()
	r.Use(redactedRecovery())
	r.GET("/boom", func(c *gin.Context) { panic("kaboom") })
	req := httptest.NewRequest(http.MethodGet, "/boom?license_key=KGT-SECRET-VALUE", nil)
	req.Header.Set("X-License-Key", "KGT-HEADER-SECRET")
	req.Header.Set("X-License-Token", "tok.secret")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status: %d want 500", w.Code)
	}
	logged := buf.String()
	for _, secret := range []string{"KGT-SECRET-VALUE", "KGT-HEADER-SECRET", "tok.secret"} {
		if strings.Contains(logged, secret) {
			t.Fatalf("the log kept %q: %s", secret, logged)
		}
	}
	if !strings.Contains(logged, "kaboom") || !strings.Contains(logged, "/boom") {
		t.Fatalf("the log says too little: %s", logged)
	}
}

// The shapes a real URL takes around a credential segment: a
// trailing slash, extra segments, a query string, a traversal
// attempt, a different case. None of them may leave the key in the
// line, and a route that merely looks similar must be left alone.
func TestRedactPathEdgeCases(t *testing.T) {
	const secret = "KG-AAAA-BBBB-CCCC-DDDD"
	cases := []string{
		"/api/v1/portal/licenses/" + secret,
		"/api/v1/portal/licenses/" + secret + "/",
		"/api/v1/portal/licenses/" + secret + "/activations",
		"/api/v1/portal/licenses/" + secret + "/activations/x/y/z",
		"/api/v1/portal/licenses/" + secret + "?a=1",
		"/api/v1/portal/licenses/" + secret + "/activations?license_key=" + secret,
		"/api/v1/portal/licenses/" + secret + "/../../etc",
		"/api/v1/portal/licenses/" + strings.ToLower(secret) + "/activations",
	}
	for _, in := range cases {
		got := redactPath(in)
		if strings.Contains(got, secret) || strings.Contains(got, strings.ToLower(secret)) {
			t.Errorf("the credential survived: redactPath(%q) = %q", in, got)
		}
	}
	// A route that only resembles the credential-bearing one keeps
	// its path: redacting by shape must not become redacting by
	// guesswork.
	for _, in := range []string{
		"/api/v1/portal/licenses",
		"/api/v1/admin/licenses/abc/key",
		"/api/v1/portal/seats/abc",
	} {
		if got := redactPath(in); got != in {
			t.Errorf("an unrelated route was rewritten: redactPath(%q) = %q", in, got)
		}
	}
}

// A path that does not have the credential as one clean segment.
//
// The router cleans a path before matching, so none of these reached
// a handler, but every request reaches the access log, which is where
// the key would have shown up. The empty segment is the dangerous
// one: splitting on the first slash makes the *empty* piece look like
// the secret, and the real key survives in what is kept after it.
func TestRedactPathWithMalformedCredentialSegment(t *testing.T) {
	const secret = "KG-AAAA-BBBB-CCCC-DDDD"
	cases := []string{
		// A doubled slash: the first segment is empty.
		"/api/v1/portal/licenses//" + secret + "/activations",
		"/api/v1/portal/licenses//" + secret,
		"/api/v1/portal/licenses///" + secret + "/activations",
		// An encoded slash inside the key, which url.URL.Path has
		// already decoded by the time this sees it, splitting the key
		// across two segments.
		"/api/v1/portal/licenses/KG-AAAA/BBBB-CCCC-DDDD/activations",
		// An unknown continuation: nothing says which segment is what.
		"/api/v1/portal/licenses/" + secret + "/activations/extra",
		"/api/v1/portal/licenses//" + secret + "/activations?x=1",
	}
	for _, in := range cases {
		got := redactPath(in)
		for _, part := range []string{secret, "BBBB-CCCC-DDDD"} {
			if strings.Contains(got, part) {
				t.Errorf("the credential survived: redactPath(%q) = %q", in, got)
				break
			}
		}
	}
}

// With no matched route there is no template to name, and the raw
// path is exactly what must not be logged. Saying neither is the
// only safe answer.
func TestInternalOnUnmatchedRouteNamesNoPath(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	const secret = "KG-SECRET1-SECRET2-SECRET3-SECRET4"
	gin.SetMode(gin.TestMode)
	r := gin.New()
	// NoRoute leaves FullPath() empty, which is the case the
	// fallback exists for.
	r.NoRoute(func(c *gin.Context) { response.Internal(c, errors.New("boom")) })

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/portal/licenses/"+secret+"/activations", nil))

	if got := buf.String(); strings.Contains(got, secret) {
		t.Errorf("the credential reached the log on an unmatched route: %s", got)
	}
	if !strings.Contains(buf.String(), "unmatched") {
		t.Errorf("an unmatched route should say so: %s", buf.String())
	}
}

// A client controls how much of the path it percent-encodes, and the
// router does not care: gin matches on the decoded path, so
// /api/v1/portal/lic%65nses/KG-SECRET/activations reaches the same
// handler as the plain spelling. The recovery logger used to test the
// prefix against the encoding the client chose, which meant any
// request that spelled a static segment differently carried its
// credential into the log.
func TestRecoveryRedactsPercentEncodedPaths(t *testing.T) {
	const secret = "KG-SECRET1-SECRET2-SECRET3-SECRET4"
	cases := []string{
		"/api/v1/portal/licenses/" + secret + "/activations",
		"/api/v1/portal/lic%65nses/" + secret + "/activations",
		"/api/v1/portal/licenses/" + secret + "/activation%73",
		"/api/v1/portal/licenses/" + secret + "/activations?license_key=" + secret,
		"/api/v1/portal/%6cicenses/" + secret,
	}

	gin.SetMode(gin.TestMode)
	for _, path := range cases {
		var buf bytes.Buffer
		prev := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))

		r := gin.New()
		r.Use(redactedRecovery())
		r.GET("/api/v1/portal/licenses/:license_key", func(c *gin.Context) { panic("boom") })
		r.GET("/api/v1/portal/licenses/:license_key/activations", func(c *gin.Context) { panic("boom") })
		r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", path, nil))

		logged := buf.String()
		slog.SetDefault(prev)

		if strings.Contains(logged, secret) {
			t.Errorf("the credential survived a panic on %q:\n%s", path, logged)
		}
	}
}

// The reason behind a 500 has to land in the log the operator
// actually collects.
//
// pkg/response writes it through the package-level slog, so it goes
// wherever slog's default points. main builds a JSON logger on stdout
// and hands it to the services; for a while it never made that logger
// the default, so these lines alone went to slog's built-in text
// handler on stderr. Everything the application logged reached the
// JSON pipeline except the explanations for its own 500s, and a test
// that installs its own default logger cannot see that, because
// installing one is the very step main was missing.
//
// So this drives the real helper through the real constructor.
func TestInternalErrorReachesTheConfiguredLogger(t *testing.T) {
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })

	var buf bytes.Buffer
	newLogger(&buf) // as main does, minus the os.Stdout

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/api/v1/thing", func(c *gin.Context) {
		response.Internal(c, errors.New("the cause"))
	})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/thing", nil))

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
	line := buf.String()
	if line == "" {
		t.Fatal("nothing reached the configured logger; the 500's reason went elsewhere")
	}
	// JSON, not the default text handler.
	var got map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &got); err != nil {
		t.Fatalf("configured logger produced %q, which is not the JSON it was built for: %v", line, err)
	}
	if got["msg"] != "request failed" {
		t.Errorf(`msg = %v, want "request failed"`, got["msg"])
	}
	if got["error"] != "the cause" {
		t.Errorf("the cause did not reach the log: error = %v", got["error"])
	}
	if got["route"] != "/api/v1/thing" {
		t.Errorf("route = %v, want /api/v1/thing", got["route"])
	}
}

// A panic still has to answer with the envelope.
//
// The recovery handler used to call AbortWithStatus, which sends a 500
// with an empty body. That is the one reply a client cannot decode,
// and a panic is exactly when an SDK can least afford a second parsing
// path. The body must carry no detail about the panic either: that
// belongs in the log, which the test above covers.
func TestPanicAnswersWithTheEnvelope(t *testing.T) {
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(redactedRecovery())
	r.GET("/boom", func(c *gin.Context) { panic("kaboom") })

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/boom", nil))

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
	var env struct {
		Success bool `json:"success"`
		Error   struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("body %q is not the envelope: %v", w.Body.String(), err)
	}
	if env.Success {
		t.Error("success = true on a panic")
	}
	if env.Error.Code != "INTERNAL_ERROR" {
		t.Errorf("code = %q, want INTERNAL_ERROR", env.Error.Code)
	}
	if strings.Contains(w.Body.String(), "kaboom") {
		t.Errorf("the panic value reached the client: %s", w.Body.String())
	}
}
