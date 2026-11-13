package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/tabloy/keygate/internal/model"
)

func TestCurrentUserLookupDistinguishesUnavailableFromMissing(t *testing.T) {
	s := scratchStore(t)
	user := &model.User{Email: "lookup@example.test", Name: "Ansh Grover", Role: model.RoleUser}
	if err := s.UpsertUser(context.Background(), user); err != nil {
		t.Fatal(err)
	}
	h := &AuthHandler{Store: s}
	check := func(id string, want int) {
		t.Helper()
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/portal/me", nil)
		if id != "" {
			c.Set("user_id", id)
		}
		h.Me(c)
		if w.Code != want {
			t.Fatalf("status %d, want %d: %s", w.Code, want, w.Body.String())
		}
	}
	check(user.ID, http.StatusOK)
	check("", http.StatusUnauthorized)
	check("00000000-0000-0000-0000-000000000000", http.StatusNotFound)
	s.Close()
	check(user.ID, http.StatusInternalServerError)
}
