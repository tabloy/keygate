package middleware

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/tabloy/keygate/internal/store"
)

// APIKeyAuth validates the Bearer token as an API key and injects the product context.
func APIKeyAuth(s *store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		raw := extractBearer(c)
		if raw == "" {
			abortWithError(c, http.StatusUnauthorized, "UNAUTHORIZED", "missing api key")
			return
		}

		hash := store.HashAPIKey(raw)
		product, apiKey, err := s.FindProductByAPIKey(c, hash)
		if err != nil {
			abortWithError(c, http.StatusUnauthorized, "UNAUTHORIZED", "invalid api key")
			return
		}

		c.Set("product_id", product.ID)
		c.Set("product", product)
		c.Set("api_key", apiKey)
		c.Next()
	}
}

func extractBearer(c *gin.Context) string {
	h := c.GetHeader("Authorization")
	if strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	return ""
}
