package handler

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/service"
)

func TestOTPConfigFailureDoesNotClaimSentOrRevealSignupEligibility(t *testing.T) {
	s := scratchStore(t)
	ctx := context.Background()
	user := &model.User{Email: "known@example.test", Role: model.RoleUser}
	if err := s.UpsertUser(ctx, user); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO settings (key,value) VALUES ('email_provider','smtp'),('smtp_password','enc:v1:invalid-test-ciphertext'),('signup_mode','licensed_only')`); err != nil {
		t.Fatal(err)
	}
	h := &AuthHandler{Store: s, Email: service.NewEmailService("", "", "", "", "", slog.Default(), s)}
	call := func(email string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/auth/otp/send", strings.NewReader(`{"email":"`+email+`"}`))
		c.Request.Header.Set("Content-Type", "application/json")
		h.OTPSend(c)
		return w
	}
	var first string
	for _, email := range []string{user.Email, "unknown@example.test"} {
		w := call(email)
		if w.Code != http.StatusServiceUnavailable {
			t.Errorf("%s: %d %s", email, w.Code, w.Body.String())
		}
		if first == "" {
			first = w.Body.String()
		} else if w.Body.String() != first {
			t.Error("failure revealed signup eligibility")
		}
		count, err := s.CountRecentOTPCodes(ctx, email)
		if err != nil || count != 0 {
			t.Errorf("failed delivery configuration consumed OTP attempts: count %d, %v", count, err)
		}
	}
	if _, err := s.DB.ExecContext(ctx, "DELETE FROM settings WHERE key IN ('email_provider','smtp_password')"); err != nil {
		t.Fatal(err)
	}
	if w := call("unknown@example.test"); w.Code != 200 {
		t.Fatalf("restricted generic response after recovery: %d %s", w.Code, w.Body.String())
	}
}
