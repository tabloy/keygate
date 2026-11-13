package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/tabloy/keygate/internal/crypto"
	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/service"
)

func TestSigningMutationsRejectInvalidBodies(t *testing.T) {
	s := scratchStore(t)
	ctx := context.Background()
	aead, err := crypto.NewAESGCM(make([]byte, crypto.MasterKeyBytes))
	if err != nil {
		t.Fatal(err)
	}
	svc := service.NewReleaseSigningService(service.ReleaseSigningServiceConfig{Store: s, AEAD: aead})
	h := NewReleaseSigningAdminHandler(svc, s)
	for _, action := range []string{"rotate", "deactivate"} {
		t.Run(action, func(t *testing.T) {
			prod := &model.Product{Name: action, Slug: action, Type: "desktop"}
			if err := s.CreateProduct(ctx, prod); err != nil {
				t.Fatal(err)
			}
			original, err := svc.GenerateForProduct(ctx, prod.ID)
			if err != nil {
				t.Fatal(err)
			}
			call := func(body string) *httptest.ResponseRecorder {
				t.Helper()
				w := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(w)
				method := http.MethodPost
				if action == "deactivate" {
					method = http.MethodDelete
				}
				c.Request = httptest.NewRequest(method, "/", strings.NewReader(body))
				c.Request.Header.Set("Content-Type", "application/json")
				c.Params = gin.Params{{Key: "id", Value: prod.ID}}
				if action == "rotate" {
					h.Rotate(c)
				} else {
					h.Deactivate(c)
				}
				return w
			}
			for _, body := range []string{`{"note":123}`, `{"note":`, `[]`, `{"note":true}`} {
				w := call(body)
				if w.Code != http.StatusBadRequest {
					t.Errorf("body %q: status %d, want 400: %s", body, w.Code, w.Body.String())
				}
				active, err := s.FindActiveSigningKey(ctx, prod.ID)
				if err != nil || active.ID != original.ID {
					t.Fatalf("invalid request changed active key, lookup error: %v", err)
				}
				keys, err := s.ListSigningKeys(ctx, prod.ID)
				if err != nil || len(keys) != 1 {
					t.Fatalf("invalid request changed history: %v, count %d", err, len(keys))
				}
			}
			for _, body := range []string{`{"note":"intentional test rotation"}`, `{}`, ``} {
				if action == "deactivate" {
					if _, err := s.FindActiveSigningKey(ctx, prod.ID); err != nil {
						if _, err := svc.GenerateForProduct(ctx, prod.ID); err != nil {
							t.Fatal(err)
						}
					}
				}
				w := call(body)
				if w.Code < 200 || w.Code >= 300 {
					t.Fatalf("optional note body %q: %d %s", body, w.Code, w.Body.String())
				}
			}
		})
	}
}
