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

func TestSessionIssuanceAndLogoutReportPersistenceFailures(t *testing.T) {
	s := scratchStore(t)
	ctx := context.Background()
	h := &AuthHandler{Store: s, Config: &config.Config{Environment: "development", BaseURL: "http://localhost:9000", JWTSecret: "test-session-durability"}}
	user := &model.User{Email: "durability@example.test", Name: "Ansh Grover", Role: model.RoleUser}
	if err := s.UpsertUser(ctx, user); err != nil {
		t.Fatal(err)
	}
	exec := func(q string) {
		t.Helper()
		if _, err := s.DB.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	exec(`CREATE FUNCTION reject_refresh_write() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'controlled refresh write failure'; END $$`)
	call := func(fn gin.HandlerFunc, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "http://localhost:9000/", strings.NewReader(body))
		c.Request.RemoteAddr = "127.0.0.1:51000"
		c.Request.Header.Set("Content-Type", "application/json")
		c.Request.AddCookie(&http.Cookie{Name: "refresh_token", Value: "existing-test-token"})
		c.Set("user_id", user.ID)
		fn(c)
		return w
	}
	assertFailed := func(w *httptest.ResponseRecorder) {
		t.Helper()
		if w.Code != 500 {
			t.Errorf("status %d, want 500: %s", w.Code, w.Body.String())
		}
		if len(w.Result().Cookies()) != 0 {
			t.Errorf("failed write changed cookies")
		}
	}
	exec(`CREATE TRIGGER reject_refresh_insert BEFORE INSERT ON refresh_tokens FOR EACH ROW EXECUTE FUNCTION reject_refresh_write()`)
	assertFailed(call(h.DevLogin, `{"email":"durability@example.test","name":"Ansh Grover"}`))
	code := "123456"
	if err := s.CreateOTPCode(ctx, &model.OTPCode{Email: user.Email, CodeHash: hashOTPCode(code), ExpiresAt: time.Now().Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	assertFailed(call(h.OTPVerify, `{"email":"durability@example.test","code":"123456"}`))
	exec(`DROP TRIGGER reject_refresh_insert ON refresh_tokens`)
	if w := call(h.DevLogin, `{"email":"durability@example.test","name":"Ansh Grover"}`); w.Code != 200 || len(w.Result().Cookies()) != 2 {
		t.Fatalf("restored login: %d, cookies %d", w.Code, len(w.Result().Cookies()))
	}
	if err := s.CreateRefreshToken(ctx, user.ID, hashToken("existing-test-token"), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	exec(`CREATE TRIGGER reject_refresh_delete BEFORE DELETE ON refresh_tokens FOR EACH ROW EXECUTE FUNCTION reject_refresh_write()`)
	assertFailed(call(h.Logout, `{}`))
	if _, err := s.DB.ExecContext(ctx, "UPDATE refresh_tokens SET revoked_at = now() - interval '10 minutes' WHERE token_hash = ?", hashToken("existing-test-token")); err != nil {
		t.Fatal(err)
	}
	assertFailed(call(h.Refresh, `{}`))
	var count int
	if err := s.DB.NewRaw("SELECT count(*) FROM refresh_tokens WHERE user_id = ?", user.ID).Scan(ctx, &count); err != nil || count < 2 {
		t.Fatalf("failed logout changed durable tokens: count %d, %v", count, err)
	}
	exec(`DROP TRIGGER reject_refresh_delete ON refresh_tokens`)
	if w := call(h.Logout, `{}`); w.Code != 200 {
		t.Fatalf("restored logout: %d %s", w.Code, w.Body.String())
	}
	if err := s.DB.NewRaw("SELECT count(*) FROM refresh_tokens WHERE user_id = ?", user.ID).Scan(ctx, &count); err != nil || count != 0 {
		t.Fatalf("successful logout retained tokens: count %d, %v", count, err)
	}
}
