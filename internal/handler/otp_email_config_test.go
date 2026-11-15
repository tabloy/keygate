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

// When the email settings cannot be read, no code can be sent, and the
// caller must hear so rather than wait for a mail that never comes. The
// answer is the same for an address that may sign in and one that may
// not, and the failed attempt does not use up one of the three codes an
// address gets per ten minutes.
func TestOTPSendReportsUnreadableEmailSettings(t *testing.T) {
	s := scratchStore(t)
	ctx := context.Background()
	if err := s.UpsertUser(ctx, &model.User{Email: "known@example.test", Role: model.RoleUser}); err != nil {
		t.Fatal(err)
	}
	// A stored secret that cannot be decrypted makes the settings read fail.
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES
		('email_provider', 'smtp'), ('smtp_password', 'enc:v1:not-a-ciphertext'), ('signup_mode', 'licensed_only')`); err != nil {
		t.Fatal(err)
	}
	h := &AuthHandler{Store: s, Email: service.NewEmailService("", "", "", "", "", slog.Default(), s)}
	send := func(email string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/auth/otp/send", strings.NewReader(`{"email":"`+email+`"}`))
		c.Request.Header.Set("Content-Type", "application/json")
		h.OTPSend(c)
		return w
	}

	var first string
	for _, email := range []string{"known@example.test", "stranger@example.test"} {
		w := send(email)
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s: status %d, want 503: %s", email, w.Code, w.Body.String())
		}
		if first == "" {
			first = w.Body.String()
		} else if w.Body.String() != first {
			t.Errorf("answers differ by address: %s vs %s", first, w.Body.String())
		}
		if n, err := s.CountRecentOTPCodes(ctx, email); err != nil || n != 0 {
			t.Errorf("%s: %d codes stored (%v), want 0", email, n, err)
		}
	}

	// Readable again (no email configured: the code goes to the log).
	if _, err := s.DB.ExecContext(ctx, `DELETE FROM settings WHERE key IN ('email_provider', 'smtp_password')`); err != nil {
		t.Fatal(err)
	}
	if w := send("known@example.test"); w.Code != http.StatusOK {
		t.Fatalf("after recovery: status %d: %s", w.Code, w.Body.String())
	}
}
