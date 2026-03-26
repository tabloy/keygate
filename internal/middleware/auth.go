package middleware

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
)

type Claims struct {
	UserID  string `json:"uid"`
	Email   string `json:"email"`
	Name    string `json:"name"`
	IsAdmin bool   `json:"adm,omitempty"`
	jwt.RegisteredClaims
}

func IssueJWT(secret, userID, email, name string, isAdmin bool, ttl time.Duration) (string, error) {
	claims := Claims{
		UserID:  userID,
		Email:   email,
		Name:    name,
		IsAdmin: isAdmin,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(ttl)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(secret))
}

// AdminChecker checks if a user has admin privileges by user ID.
// Injected at startup — queries the database for the user's role.
type AdminChecker func(ctx context.Context, userID string) bool

// SessionAuth validates a JWT from the Authorization header or session cookie.
// Admin status is checked at request time (from DB, not JWT claims) for security —
// this ensures role changes take effect immediately without waiting for JWT expiry.
func SessionAuth(secret string, adminCheck ...AdminChecker) gin.HandlerFunc {
	var checkAdmin AdminChecker
	if len(adminCheck) > 0 {
		checkAdmin = adminCheck[0]
	}

	return func(c *gin.Context) {
		raw := extractBearer(c)
		if raw == "" {
			if cookie, err := c.Cookie("session"); err == nil && cookie != "" {
				raw = cookie
			}
		}
		if raw == "" {
			abortWithError(c, http.StatusUnauthorized, "UNAUTHORIZED", "unauthorized")
			return
		}

		claims := &Claims{}
		tok, err := jwt.ParseWithClaims(raw, claims, func(*jwt.Token) (any, error) {
			return []byte(secret), nil
		})
		if err != nil || !tok.Valid {
			abortWithError(c, http.StatusUnauthorized, "UNAUTHORIZED", "invalid or expired token")
			return
		}

		c.Set("user_id", claims.UserID)
		c.Set("email", claims.Email)
		c.Set("name", claims.Name)

		// Determine admin status at request time from database role
		isAdmin := false
		if checkAdmin != nil {
			isAdmin = checkAdmin(c.Request.Context(), claims.UserID)
		} else {
			isAdmin = claims.IsAdmin
		}
		c.Set("is_admin", isAdmin)
		c.Next()
	}
}

func AdminOnly() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Verify SessionAuth ran first
		if _, exists := c.Get("user_id"); !exists {
			abortWithError(c, http.StatusUnauthorized, "UNAUTHORIZED", "unauthorized")
			return
		}
		v, _ := c.Get("is_admin")
		if v != true {
			abortWithError(c, http.StatusForbidden, "FORBIDDEN", "admin required")
			return
		}
		c.Next()
	}
}

// RequireScope checks the API key has a required scope.
func RequireScope(scope string) gin.HandlerFunc {
	return func(c *gin.Context) {
		v, exists := c.Get("api_key")
		if !exists {
			abortWithError(c, http.StatusForbidden, "FORBIDDEN", "missing api key context")
			return
		}
		ak := v.(interface{ GetScopes() []string })
		scopes := ak.GetScopes()
		if len(scopes) == 0 {
			c.Next()
			return
		}
		for _, s := range scopes {
			if strings.EqualFold(s, scope) {
				c.Next()
				return
			}
		}
		abortWithError(c, http.StatusForbidden, "FORBIDDEN", "insufficient scope")
	}
}
