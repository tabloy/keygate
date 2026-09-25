package handler

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/pkg/response"
)

// OTPSend handles POST /api/v1/auth/otp/send
func (h *AuthHandler) OTPSend(c *gin.Context) {
	var req struct {
		Email string `json:"email" binding:"required,email"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "valid email is required")
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))

	// Rate limit: max 3 OTP requests per email per 10 minutes
	count, err := h.Store.CountRecentOTPCodes(c, email)
	if err != nil {
		response.Internal(c, err)
		return
	}
	if count >= 3 {
		response.Err(c, 429, "RATE_LIMITED", "too many code requests, try again later")
		return
	}

	allowed := h.signupAllowed(c, email)

	code := generateOTPCode()
	codeHash := hashOTPCode(code)
	if !allowed {
		// Store a code nothing can ever present. A blocked request has
		// to leave the same trace as a real one: the rate limit above
		// counts rows, so skipping the write made the limiter itself
		// the oracle the generic response is meant to prevent — a
		// known address answers 429 on the fourth try, a stranger
		// answers 200 forever. The hash of 32 random bytes is not the
		// hash of any six-digit code, so the row cannot be guessed
		// into a login even by accident.
		codeHash = unguessableCodeHash()
	}

	otp := &model.OTPCode{
		Email:     email,
		CodeHash:  codeHash,
		ExpiresAt: time.Now().Add(10 * time.Minute),
	}
	if err := h.Store.CreateOTPCode(c, otp); err != nil {
		response.Internal(c, err)
		return
	}

	if !allowed {
		// The one thing that does not happen is the mail. Saying "no
		// license for that address" would answer the same question out
		// loud; the operator sees the block in the logs instead.
		slog.Warn("otp send blocked: signups restricted to licensed emails",
			"email", email, "ip", c.ClientIP())
		response.OK(c, gin.H{"status": "sent"})
		return
	}

	logCodeFallback := func() {
		slog.Warn("SMTP not configured — OTP code printed to log (configure SMTP for email delivery)",
			"email", email, "code", code)
	}
	switch {
	case h.Email == nil:
		logCodeFallback()
	default:
		configured, cErr := h.Email.Configured()
		switch {
		case cErr != nil:
			// A transient config read error (DB blip, decrypt failure)
			// must NOT fall back to logging the code in plaintext: that
			// would leak a live login code on an infra hiccup. Fail
			// closed — the user simply retries once the read recovers.
			slog.Error("otp email config unavailable; code not sent and not logged",
				"email", email, "error", cErr)
		case configured:
			h.Email.SendOTPCode(email, code)
		default:
			logCodeFallback()
		}
	}

	response.OK(c, gin.H{"status": "sent"})
}

// OTPVerify handles POST /api/v1/auth/otp/verify
func (h *AuthHandler) OTPVerify(c *gin.Context) {
	var req struct {
		Email string `json:"email" binding:"required,email"`
		Code  string `json:"code" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "email and code are required")
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	code := strings.TrimSpace(req.Code)

	otp, err := h.Store.FindLatestValidOTPCode(c, email)

	// Always perform hash comparison to prevent timing-based email enumeration
	expectedHash := hashOTPCode("") // dummy
	otpID := ""
	otpAttempts := 0
	if err == nil && otp != nil {
		expectedHash = otp.CodeHash
		otpID = otp.ID
		otpAttempts = otp.Attempts
	}

	codeMatch := hmac.Equal([]byte(hashOTPCode(code)), []byte(expectedHash))

	if otpID != "" {
		if err := h.Store.IncrementOTPAttempts(c, otpID); err != nil {
			slog.Warn("failed to increment OTP attempts", "id", otpID, "error", err)
		}
	}

	if !codeMatch || otp == nil {
		remaining := 5 - (otpAttempts + 1)
		if remaining <= 0 {
			response.Unauthorized(c, "too many attempts, request a new code")
		} else {
			response.Unauthorized(c, "invalid or expired code")
		}
		return
	}

	if err := h.Store.MarkOTPUsed(c, otpID); err != nil {
		slog.Warn("failed to mark OTP used", "id", otpID, "error", err)
	}

	// Gate the creating half as well. A code minted before the operator
	// switched the setting on would otherwise still mint an account.
	if !h.signupAllowed(c, email) {
		response.Unauthorized(c, "invalid or expired code")
		return
	}

	// Upsert user (create on first login)
	user := &model.User{Email: email}
	if err := h.Store.UpsertUser(c, user); err != nil {
		response.Internal(c, err)
		return
	}
	user, err = h.Store.FindUserByEmail(c, email)
	if err != nil {
		response.Internal(c, err)
		return
	}

	// Auto-promote if email is in ADMIN_EMAILS
	if h.Config.IsAdminEmail(user.Email) && user.Role == model.RoleUser {
		_ = h.Store.SetUserRole(c, user.ID, model.RoleAdmin)
		user.Role = model.RoleAdmin
	}

	// Welcome email for new users
	if h.Email != nil && time.Since(user.CreatedAt) < time.Minute {
		h.Email.SendWelcome(user.Email, user.Name)
	}

	h.issueSession(c, user)

	h.Store.Audit(c, &model.AuditLog{
		Entity: "session", EntityID: user.ID, Action: "login",
		ActorType: "otp", ActorID: user.ID, IPAddress: c.ClientIP(),
		Changes: map[string]any{"email": user.Email},
	})

	response.OK(c, gin.H{
		"status": "ok", "email": user.Email, "name": user.Name,
		"is_admin": user.IsAdmin(), "role": user.Role,
	})
}

// signupAllowed reports whether email may receive a login code.
//
// Default (signup_mode unset or "open") is what Keygate has always
// done: anyone can ask for a code and an account is created on first
// login. Operators who sell to a known customer list can set
// "licensed_only", after which a code only goes to an address that
// already has an account or holds a license — so the endpoint can no
// longer be pointed at arbitrary strangers.
//
// Existing accounts keep working either way: the restriction is on
// creating accounts, not on logging into one that already exists.
func (h *AuthHandler) signupAllowed(c *gin.Context, email string) bool {
	mode, err := h.Store.GetSetting(c, "signup_mode")
	if err != nil {
		// No row is the normal state of an install that never touched
		// the setting, and it means open sign-ups. Any other error is
		// an unread setting, not a known one: treat it the way the
		// lookup below is treated rather than assuming the permissive
		// answer.
		if !errors.Is(err, sql.ErrNoRows) {
			slog.Error("signup gate: reading signup_mode failed", "error", err)
			return false
		}
		return true
	}
	if mode != "licensed_only" {
		return true
	}
	known, err := h.Store.HasAccountOrLicense(c, email)
	if err != nil {
		// Fail closed: this is the gate that stops the endpoint from
		// mailing strangers, and a lookup that did not run is not an
		// answer. The caller still gets the same "sent" reply, so the
		// operator has to find this in the log.
		slog.Error("signup gate lookup failed", "email", email, "error", err)
		return false
	}
	return known
}

// unguessableCodeHash returns a code_hash no submitted code can match:
// the preimage is 32 random bytes, not a six-digit string.
func unguessableCodeHash() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand does not fail in practice; if it ever does, a
		// constant is still not a valid six-digit code's hash.
		return hex.EncodeToString(sha256.New().Sum(nil))
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func generateOTPCode() string {
	n, _ := rand.Int(rand.Reader, big.NewInt(1000000))
	return fmt.Sprintf("%06d", n.Int64())
}

func hashOTPCode(code string) string {
	h := sha256.Sum256([]byte(code))
	return hex.EncodeToString(h[:])
}
