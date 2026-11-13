package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/tabloy/keygate/internal/model"
)

func TestSigningProductLookupDistinguishesUnavailableFromMissing(t *testing.T) {
	s := scratchStore(t)
	product := &model.Product{Name: "Lookup", Slug: "lookup", Type: "desktop"}
	if err := s.CreateProduct(context.Background(), product); err != nil {
		t.Fatal(err)
	}
	h := NewReleaseSigningAdminHandler(nil, s)
	handlers := map[string]gin.HandlerFunc{"generate": h.Generate, "rotate": h.Rotate, "list": h.List, "pem": h.DownloadPublicKey, "tauri": h.DownloadPublicKeyTauri, "deactivate": h.Deactivate}
	call := func(t *testing.T, fn gin.HandlerFunc, id string, want int) {
		t.Helper()
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
		c.Params = gin.Params{{Key: "id", Value: id}}
		fn(c)
		if w.Code != want {
			t.Errorf("status %d, want %d: %s", w.Code, want, w.Body.String())
		}
	}
	for name, fn := range handlers {
		t.Run(name+"/missing", func(t *testing.T) { call(t, fn, "00000000-0000-0000-0000-000000000000", http.StatusNotFound) })
	}
	s.Close()
	for name, fn := range handlers {
		t.Run(name+"/unavailable", func(t *testing.T) { call(t, fn, product.ID, http.StatusInternalServerError) })
	}
}
