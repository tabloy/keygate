package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tabloy/keygate/internal/config"
	"github.com/tabloy/keygate/internal/model"
)

// A login whose refresh token could not be stored must fail, not hand
// out a session cookie that silently ends a day later. A logout whose
// refresh tokens could not be deleted must fail too, with the cookies
// left alone, rather than claim a logout while the token still renews.
func TestSessionStoreFailuresAreReported(t *testing.T) {
	s := scratchStore(t)
	ctx := context.Background()
	h := &AuthHandler{Store: s, Config: &config.Config{Environment: "development", BaseURL: "http://localhost:9000", JWTSecret: "session-store-failure-test"}}
	user := &model.User{Email: "session@example.test", Name: "Session", Role: model.RoleUser}
	if err := s.UpsertUser(ctx, user); err != nil {
		t.Fatal(err)
	}
	exec := func(q string) {
		t.Helper()
		if _, err := s.DB.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	exec(`CREATE FUNCTION refuse_refresh_write() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'refresh_tokens write refused by test'; END $$`)
	call := func(fn gin.HandlerFunc, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "http://localhost:9000/", strings.NewReader(body))
		c.Request.RemoteAddr = "127.0.0.1:50000"
		c.Request.Header.Set("Content-Type", "application/json")
		c.Request.AddCookie(&http.Cookie{Name: "refresh_token", Value: "the-current-token"})
		c.Set("user_id", user.ID)
		fn(c)
		return w
	}
	failed := func(name string, w *httptest.ResponseRecorder) {
		t.Helper()
		if w.Code != http.StatusInternalServerError {
			t.Errorf("%s: status %d, want 500: %s", name, w.Code, w.Body.String())
		}
		if n := len(w.Result().Cookies()); n != 0 {
			t.Errorf("%s: %d cookies set, want none", name, n)
		}
	}
	login := `{"email":"session@example.test","name":"Session"}`

	exec(`CREATE TRIGGER refuse_refresh_insert BEFORE INSERT ON refresh_tokens FOR EACH ROW EXECUTE FUNCTION refuse_refresh_write()`)
	failed("dev login", call(h.DevLogin, login))
	if err := s.CreateOTPCode(ctx, &model.OTPCode{Email: user.Email, CodeHash: hashOTPCode("123456"), ExpiresAt: time.Now().Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	failed("code login", call(h.OTPVerify, `{"email":"session@example.test","code":"123456"}`))
	exec(`DROP TRIGGER refuse_refresh_insert ON refresh_tokens`)

	if w := call(h.DevLogin, login); w.Code != http.StatusOK || len(w.Result().Cookies()) != 2 {
		t.Fatalf("login once writes work: status %d, %d cookies", w.Code, len(w.Result().Cookies()))
	}
	if err := s.CreateRefreshToken(ctx, user.ID, hashToken("the-current-token"), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	count := func() int {
		var n int
		if err := s.DB.NewRaw("SELECT count(*) FROM refresh_tokens WHERE user_id = ?", user.ID).Scan(ctx, &n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	exec(`CREATE TRIGGER refuse_refresh_delete BEFORE DELETE ON refresh_tokens FOR EACH ROW EXECUTE FUNCTION refuse_refresh_write()`)
	failed("logout", call(h.Logout, `{}`))
	if n := count(); n != 2 {
		t.Fatalf("failed logout left %d tokens, want both still there", n)
	}
	exec(`DROP TRIGGER refuse_refresh_delete ON refresh_tokens`)

	if w := call(h.Logout, `{}`); w.Code != http.StatusOK {
		t.Fatalf("logout once deletes work: status %d: %s", w.Code, w.Body.String())
	}
	if n := count(); n != 0 {
		t.Fatalf("logout left %d tokens, want 0", n)
	}
}
