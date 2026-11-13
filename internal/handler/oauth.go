package handler

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/tabloy/keygate/internal/config"
	"github.com/tabloy/keygate/internal/middleware"
	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/service"
	"github.com/tabloy/keygate/internal/store"
	"github.com/tabloy/keygate/pkg/apperr"
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
	Store   *store.Store
	Config  *config.Config
	Email   *service.EmailService
	Webhook *service.WebhookService
	// beforeConsumeCode runs between a code matching and it being
	// marked used. Tests use it to hold several requests in that
	// window; nil everywhere else.
	beforeConsumeCode func()
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
		if err := h.Store.DeleteRefreshToken(c, hashToken(raw)); err != nil {
			response.Internal(c, err)
			return
		}
	}
	if uid, ok := userID.(string); ok && uid != "" {
		if err := h.Store.DeleteUserRefreshTokens(c, uid); err != nil {
			response.Internal(c, err)
			return
		}
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

	// Rotation, the new token and the user read commit together or not at
	// all: a failure leaves the presented token usable, so the client's
	// retry works however long the outage lasts. A grace renewal (another
	// tab, or a retry whose first response was lost) also hands out a
	// refresh token: the browser may never have seen the one the first
	// renewal issued, and would otherwise present this rotated token a
	// day on and be taken for theft.
	rawRefresh := randomHex(32)
	ren, err := h.Store.RenewRefreshToken(c, hashToken(raw), hashToken(rawRefresh), time.Now().Add(refreshTokenTTL))
	if errors.Is(err, store.ErrRefreshTokenReused) {
		// REUSE DETECTED: a token that was already rotated has been
		// presented again. Either the legit user replayed a stale
		// cookie, OR an attacker captured a token. We can't tell
		// which, so we take the safe-by-default action: wipe every
		// refresh_token for the user. Both parties (legit + attacker)
		// lose their refresh capability; legit user has to re-auth
		// from scratch.
		if revokeErr := h.Store.DeleteUserRefreshTokens(c, ren.Token.UserID); revokeErr != nil {
			response.Internal(c, revokeErr)
			return
		}
		slog.Warn("refresh token reuse detected — revoking all user tokens",
			"user_id", ren.Token.UserID, "token_id", ren.Token.ID)
		// Clear the cookie on the client so the next page load
		// doesn't try the dead token again.
		setSecureCookie(c, "refresh_token", "", -1, "/api/v1/auth/refresh", h.requestIsHTTPS(c), true)
		response.Unauthorized(c, "refresh token reuse detected")
		return
	}
	if errors.Is(err, sql.ErrNoRows) {
		response.Unauthorized(c, "invalid refresh token")
		return
	}
	if errors.Is(err, store.ErrRefreshUserNotFound) {
		response.Unauthorized(c, "user not found")
		return
	}
	if err != nil {
		// A database failure is not a sign-out: answer 5xx so the client
		// keeps the user signed in and retries later.
		response.Internal(c, err)
		return
	}

	h.issueSessionCookie(c, ren.User)
	h.setRefreshCookie(c, rawRefresh, ren.ExpiresAt)
	response.OK(c, gin.H{"status": "refreshed"})
}

// refreshTokenTTL is how long a refresh token lives.
const refreshTokenTTL = 30 * 24 * time.Hour

func (h *AuthHandler) issueSession(c *gin.Context, user *model.User) error {
	rawRefresh := randomHex(32)
	expiresAt := time.Now().Add(refreshTokenTTL)
	if err := h.Store.CreateRefreshToken(c, user.ID, hashToken(rawRefresh), expiresAt); err != nil {
		return err
	}
	h.issueSessionCookie(c, user)
	h.setRefreshCookie(c, rawRefresh, expiresAt)
	return nil
}

// setRefreshCookie sets the refresh cookie to a token expiring at expiresAt.
func (h *AuthHandler) setRefreshCookie(c *gin.Context, rawRefresh string, expiresAt time.Time) {
	maxAge := int(time.Until(expiresAt).Seconds())
	setSecureCookie(c, "refresh_token", rawRefresh, maxAge, "/api/v1/auth/refresh", h.requestIsHTTPS(c), true)
}

// issueSessionCookie sets the 24-hour session cookie alone.
func (h *AuthHandler) issueSessionCookie(c *gin.Context, user *model.User) {
	// JWT includes admin claim for convenience, but the authoritative check
	// happens at request time via DB role lookup in SessionAuth middleware.
	token, _ := middleware.IssueJWT(
		h.Config.JWTSecret, user.ID, user.Email, user.Name,
		user.IsAdmin(), 24*time.Hour,
	)
	setSecureCookie(c, "session", token, 24*3600, "/", h.requestIsHTTPS(c), true)
}

func hashToken(raw string) string {
	h := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(h[:])
}

func (h *AuthHandler) Providers(c *gin.Context) {
	response.OK(c, gin.H{"dev_login": h.devLoginOpen(c), "otp": true})
}

// devLoginOpen reports whether this request may use dev-login: the
// server allows it (ENVIRONMENT=development, BASE_URL on this machine;
// see Config.IsDevLoginAllowed), and the request comes from this
// machine. ENVIRONMENT defaults to development, so a server started
// with no configuration and reachable from elsewhere would otherwise
// hand an owner session to anyone who asked.
//
// From this machine means: the TCP peer is loopback, the request names
// this machine as its host, and it carries no forwarding header. A
// reverse proxy on the same machine makes every visitor's peer
// loopback, but it passes on the public host name or says whom it
// forwards for; the Vite dev proxy does neither (it rewrites Host to
// the target and adds no X-Forwarded-For).
func (h *AuthHandler) devLoginOpen(c *gin.Context) bool {
	if !h.Config.IsDevLoginAllowed() {
		return false
	}
	for _, name := range []string{"X-Forwarded-For", "X-Forwarded-Host", "Forwarded", "X-Real-IP"} {
		if c.Request.Header.Get(name) != "" {
			return false
		}
	}
	peer, _, err := net.SplitHostPort(c.Request.RemoteAddr)
	if err != nil {
		return false
	}
	if ip := net.ParseIP(peer); ip == nil || !ip.IsLoopback() {
		return false
	}
	return isLoopbackHost(c.Request.Host)
}

// isLoopbackHost reports whether a Host header, with or without a
// port, names this machine.
func isLoopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.ToLower(strings.Trim(host, "[]"))
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// DevLogin is a development-only endpoint that creates a session without
// email OTP, for a caller on the machine running a development server
// (see devLoginOpen). Anywhere else it does not exist.
func (h *AuthHandler) DevLogin(c *gin.Context) {
	if !h.devLoginOpen(c) {
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
	// The address rides in the session token too: one of any length made
	// a cookie past what a client reads.
	if err := apperr.ValidateEmail(req.Email); err != nil {
		response.BadRequest(c, err.Message)
		return
	}
	if req.Name == "" {
		req.Name = "Dev User"
	}
	// The name rides in the session token, as in setup.
	if err := apperr.ValidateName("name", req.Name); err != nil {
		response.BadRequest(c, err.Message)
		return
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

	if err := h.issueSession(c, user); err != nil {
		response.Internal(c, err)
		return
	}
	h.Store.Audit(c, &model.AuditLog{
		Entity: "session", EntityID: user.ID, Action: "login",
		ActorType: "dev_login", ActorID: user.ID, IPAddress: c.ClientIP(),
		Changes: map[string]any{"email": user.Email},
	})
	// Auto-promote if email is in ADMIN_EMAILS and user is currently just 'user'
	if h.Config.IsAdminEmail(user.Email) && user.Role == model.RoleUser {
		if role, err := h.Store.PromoteAdminEmail(c, user.ID); err == nil {
			user.Role = role
		}
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
		if lerr != nil {
			response.Internal(c, lerr)
			return
		}
		if err := h.issueSession(c, user); err != nil {
			response.Internal(c, err)
			return
		}
		response.OK(c, res)
	}
}
