package handler

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/tabloy/keygate/internal/config"
	"github.com/tabloy/keygate/internal/middleware"
	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/service"
	"github.com/tabloy/keygate/internal/store"
	"github.com/tabloy/keygate/pkg/response"
)

// requestIsHTTPS reports whether the browser reached this server over
// TLS — directly, through a proxy that terminated it, or because the
// install says so itself (BASE_URL), for a proxy that forwards
// nothing. It decides the Secure attribute on the session cookies,
// and the request's own scheme is what must decide it: a Secure
// cookie handed to a browser over plain HTTP is dropped without a
// word, so the login answers 200 and every request after it 401s.
// The environment name says nothing about how the browser got here.
//
// A client that puts X-Forwarded-Proto: https on a plain request only
// stops its own cookie from being stored; it cannot take the
// attribute off anyone else's.
func (h *AuthHandler) requestIsHTTPS(c *gin.Context) bool {
	if c.Request.TLS != nil {
		return true
	}
	// A chain of proxies appends: the first entry is the scheme the
	// browser used.
	proto, _, _ := strings.Cut(c.GetHeader("X-Forwarded-Proto"), ",")
	if strings.EqualFold(strings.TrimSpace(proto), "https") {
		return true
	}
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(h.Config.BaseURL)), "https://")
}

// setSecureCookie sets a cookie with SameSite=Lax for CSRF protection.
// secure marks it HTTPS-only; see requestIsHTTPS for who may set it.
func setSecureCookie(c *gin.Context, name, value string, maxAge int, path string, secure, httpOnly bool) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     name,
		Value:    value,
		MaxAge:   maxAge,
		Path:     path,
		Secure:   secure,
		HttpOnly: httpOnly,
		SameSite: http.SameSiteLaxMode,
	})
}

type AuthHandler struct {
	Store  *store.Store
	Config *config.Config
	Email  *service.EmailService
}

func (h *AuthHandler) Me(c *gin.Context) {
	uid, _ := c.Get("user_id")
	uidStr, ok := uid.(string)
	if !ok || uidStr == "" {
		response.Unauthorized(c, "unauthorized")
		return
	}
	user, err := h.Store.FindUserByID(c, uidStr)
	if err != nil {
		response.NotFound(c, "user not found")
		return
	}
	response.OK(c, gin.H{
		"id": user.ID, "email": user.Email, "name": user.Name,
		"avatar_url": user.AvatarURL, "is_admin": user.IsAdmin(), "role": user.Role,
	})
}

func (h *AuthHandler) Logout(c *gin.Context) {
	userID, _ := c.Get("user_id")
	if raw, err := c.Cookie("refresh_token"); err == nil && raw != "" {
		h.Store.DeleteRefreshToken(c, hashToken(raw))
	}
	// Delete user's other refresh tokens to fully invalidate session
	if uid, ok := userID.(string); ok && uid != "" {
		h.Store.DeleteUserRefreshTokens(c, uid)
		h.Store.Audit(c, &model.AuditLog{
			Entity: "session", EntityID: uid, Action: "logout",
			ActorType: "user", ActorID: uid, IPAddress: c.ClientIP(),
		})
	}
	setSecureCookie(c, "session", "", -1, "/", h.requestIsHTTPS(c), true)
	setSecureCookie(c, "refresh_token", "", -1, "/api/v1/auth/refresh", h.requestIsHTTPS(c), true)
	response.OK(c, gin.H{"status": "logged_out"})
}

func (h *AuthHandler) Refresh(c *gin.Context) {
	raw, err := c.Cookie("refresh_token")
	if err != nil || raw == "" {
		response.Unauthorized(c, "no refresh token")
		return
	}

	tokenHash := hashToken(raw)
	rt, err := h.Store.RotateRefreshToken(c, tokenHash)
	if errors.Is(err, store.ErrRefreshTokenReused) {
		// REUSE DETECTED: a token that was already rotated has been
		// presented again. Either the legit user replayed a stale
		// cookie, OR an attacker captured a token. We can't tell
		// which, so we take the safe-by-default action: wipe every
		// refresh_token for the user. Both parties (legit + attacker)
		// lose their refresh capability; legit user has to re-auth
		// from scratch.
		h.Store.DeleteUserRefreshTokens(c, rt.UserID)
		slog.Warn("refresh token reuse detected — revoking all user tokens",
			"user_id", rt.UserID, "token_id", rt.ID)
		// Clear the cookie on the client so the next page load
		// doesn't try the dead token again.
		setSecureCookie(c, "refresh_token", "", -1, "/api/v1/auth/refresh", h.requestIsHTTPS(c), true)
		response.Unauthorized(c, "refresh token reuse detected")
		return
	}
	if err != nil {
		response.Unauthorized(c, "invalid refresh token")
		return
	}

	user, err := h.Store.FindUserByID(c, rt.UserID)
	if err != nil {
		response.Unauthorized(c, "user not found")
		return
	}

	// Token already marked revoked atomically by RotateRefreshToken.
	// Just issue the new session — the new token is a fresh row,
	// the old row stays in DB with revoked_at set so a future
	// replay of the old hash will trip ErrRefreshTokenReused.
	h.issueSession(c, user)
	response.OK(c, gin.H{"status": "refreshed"})
}

func (h *AuthHandler) issueSession(c *gin.Context, user *model.User) {
	// JWT includes admin claim for convenience, but the authoritative check
	// happens at request time via DB role lookup in SessionAuth middleware.
	token, _ := middleware.IssueJWT(
		h.Config.JWTSecret, user.ID, user.Email, user.Name,
		user.IsAdmin(), 24*time.Hour,
	)
	setSecureCookie(c, "session", token, 24*3600, "/", h.requestIsHTTPS(c), true)

	// Long-lived refresh token (30 days)
	rawRefresh := randomHex(32)
	refreshHash := hashToken(rawRefresh)
	expiresAt := time.Now().Add(30 * 24 * time.Hour)
	_ = h.Store.CreateRefreshToken(c, user.ID, refreshHash, expiresAt)
	setSecureCookie(c, "refresh_token", rawRefresh, 30*24*3600, "/api/v1/auth/refresh", h.requestIsHTTPS(c), true)
}

func hashToken(raw string) string {
	h := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(h[:])
}

func (h *AuthHandler) Providers(c *gin.Context) {
	devLogin := h.Config.IsDevLoginAllowed()
	response.OK(c, gin.H{"dev_login": devLogin, "otp": true})
}

// DevLogin is a development-only endpoint that creates a session without email OTP.
// Security: This endpoint is ONLY available when BOTH conditions are met:
//  1. ENVIRONMENT is explicitly set to "development"
//  2. The server is NOT listening on a public interface (checked via BASE_URL)
//
// This prevents accidental exposure in production deployments.
func (h *AuthHandler) DevLogin(c *gin.Context) {
	if !h.Config.IsDevLoginAllowed() {
		response.NotFound(c, "not found")
		return
	}
	// Block dev-login on public-facing hosts
	base := h.Config.BaseURL
	if !strings.Contains(base, "localhost") && !strings.Contains(base, "127.0.0.1") {
		response.NotFound(c, "not found")
		return
	}

	var req struct {
		Email string `json:"email" binding:"required"`
		Name  string `json:"name"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "email is required")
		return
	}
	// Normalize for parity with the OTP/setup paths — otherwise
	// "Foo@x.com" and "foo@x.com" become two distinct user rows.
	req.Email = strings.ToLower(strings.TrimSpace(req.Email))
	if req.Name == "" {
		req.Name = "Dev User"
	}

	user := &model.User{Email: req.Email, Name: req.Name}
	if err := h.Store.UpsertUser(c, user); err != nil {
		response.Internal(c, err)
		return
	}
	user, err := h.Store.FindUserByEmail(c, req.Email)
	if err != nil {
		response.Internal(c, err)
		return
	}

	h.issueSession(c, user)
	h.Store.Audit(c, &model.AuditLog{
		Entity: "session", EntityID: user.ID, Action: "login",
		ActorType: "dev_login", ActorID: user.ID, IPAddress: c.ClientIP(),
		Changes: map[string]any{"email": user.Email},
	})
	// Auto-promote if email is in ADMIN_EMAILS and user is currently just 'user'
	if h.Config.IsAdminEmail(user.Email) && user.Role == model.RoleUser {
		_ = h.Store.SetUserRole(c, user.ID, model.RoleAdmin)
		user.Role = model.RoleAdmin
	}

	response.OK(c, gin.H{
		"status": "ok", "email": user.Email, "name": user.Name,
		"is_admin": user.IsAdmin(), "role": user.Role,
	})
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// AcceptInvite consumes a seat-invite token and, on success, ALSO
// issues a session for the invitee. Reasoning: the plain token in
// the email is proof of email ownership (we mailed it there), so
// requiring the invitee to *separately* OTP themselves in after
// clicking the link is theater — and bad UX. They'd hit /login and
// have to round-trip through the same mailbox. By issuing a session
// here, "click link → land in portal" is one step.
//
// Service is provided as a parameter rather than as a field so we
// don't have to widen AuthHandler's surface; main.go closes over it.
func (h *AuthHandler) AcceptInvite(svc *service.SeatService) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req struct {
			Token string `json:"token" binding:"required"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			response.BadRequest(c, "token is required")
			return
		}
		res, err := svc.AcceptSeatInvite(c.Request.Context(), req.Token)
		if err != nil {
			writeAppErr(c, err)
			return
		}
		// Re-load the user so issueSession sees the canonical row
		// (id + role + name). AcceptSeatInvite returned the user
		// model but it was constructed inside the tx and may lack
		// fields populated by triggers.
		user, lerr := h.Store.FindUserByID(c, res.UserID)
		if lerr == nil {
			h.issueSession(c, user)
		}
		response.OK(c, res)
	}
}
