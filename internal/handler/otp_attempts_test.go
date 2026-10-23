package handler

import (
	"net/http"
	"strings"
	"testing"

	"github.com/tabloy/keygate/internal/store"
)

// Five wrong guesses use the code up: the right one is refused after.
func TestCheckCode_AttemptLimit(t *testing.T) {
	f := newSignupFixture(t, nil)
	email := "limit-" + f.plan.CheckoutID + "@example.com"
	code := f.code(email)
	wrong := "000000"
	if code == wrong {
		wrong = "111111"
	}
	for i := range store.MaxOTPAttempts {
		w, _ := f.verify(email, wrong, false)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("wrong guess #%d: %d %s", i+1, w.Code, w.Body.String())
		}
		// The last one says so.
		if last := i == store.MaxOTPAttempts-1; last != strings.Contains(w.Body.String(), "too many attempts") {
			t.Fatalf("wrong guess #%d: %s", i+1, w.Body.String())
		}
	}
	if w, _ := f.verify(email, code, false); w.Code != http.StatusUnauthorized {
		t.Fatalf("right code after the limit: %d %s", w.Code, w.Body.String())
	}
	if n := len(f.licences(email)); n != 0 {
		t.Fatalf("%d licences after the limit, want 0", n)
	}
}
