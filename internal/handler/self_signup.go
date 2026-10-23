package handler

import (
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/tabloy/keygate/internal/license"
	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/service"
	"github.com/tabloy/keygate/internal/store"
	"github.com/tabloy/keygate/pkg/apperr"
	"github.com/tabloy/keygate/pkg/response"
)

// Self sign-up: a visitor claims a licence on a free plan by confirming
// an email address with a login code. The plan's /pay link leads here
// when the plan has self_signup on; the licence key is mailed like any
// other, and the visitor ends up signed in to the portal.
//
//	GET  /api/v1/signup/:checkout_id         what the form shows
//	POST /api/v1/signup/:checkout_id/send    {email}
//	POST /api/v1/signup/:checkout_id/verify  {email, code, marketing_consent, consent_text}

// signupPlan returns the plan behind checkout_id when it takes self
// sign-ups. Otherwise it writes the 404 and returns nil: a plan that
// is unknown, inactive, priced or not opened all answer the same.
func (h *AuthHandler) signupPlan(c *gin.Context) *model.Plan {
	checkoutID := c.Param("checkout_id")
	if len(checkoutID) == 8 {
		plan, err := h.Store.FindPlanByCheckoutID(c, checkoutID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			response.Internal(c, err)
			return nil
		}
		if err == nil && selfSignupOpen(plan) {
			return plan
		}
	}
	response.Err(c, http.StatusNotFound, "SIGNUP_UNAVAILABLE", "sign-up is not available for this plan")
	return nil
}

// selfSignupOpen reports whether a visitor may claim a licence on plan
// right now. A plan with an update period never qualifies: the period
// needs the gating checks a sale gets, and a free plan has no use for it.
func selfSignupOpen(plan *model.Plan) bool {
	return plan.Active && plan.SelfSignup && plan.StripePriceID == "" &&
		plan.InitialUpdatesUntil(time.Now()) == nil
}

// SignupInfo handles GET /api/v1/signup/:checkout_id
func (h *AuthHandler) SignupInfo(c *gin.Context) {
	plan := h.signupPlan(c)
	if plan == nil {
		return
	}
	productName := ""
	if p, err := h.Store.FindProductByID(c, plan.ProductID); err == nil {
		productName = p.Name
	}
	response.OK(c, gin.H{
		"product_name": productName,
		"plan_name":    plan.Name,
		"license_type": plan.LicenseType,
		"trial_days":   plan.TrialDays,
		"consent_text": plan.SignupConsentText,
	})
}

// SignupSend handles POST /api/v1/signup/:checkout_id/send
func (h *AuthHandler) SignupSend(c *gin.Context) {
	var req struct {
		Email string `json:"email" binding:"required,email"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "valid email is required")
		return
	}
	email, ok := signupEmail(c, req.Email)
	if !ok {
		return
	}
	plan := h.signupPlan(c)
	if plan == nil {
		return
	}
	// A plan that is full today would only refuse the claim after the
	// code was typed in, so no code is mailed for it. Every address
	// gets this answer, customers included: they sign in through the
	// normal login. The locked count at verify stays the one that
	// decides.
	if plan.SignupDailyCap > 0 {
		n, err := h.Store.CountSelfSignupsToday(c, plan.ID)
		if err != nil {
			response.Internal(c, err)
			return
		}
		if n >= plan.SignupDailyCap {
			signupFull(c)
			return
		}
	}
	// The same code, limits and mail as a login. An address that
	// already holds a licence on the plan gets one too: the verify step
	// signs it in to that licence, so this reply says nothing about
	// which addresses are customers.
	h.sendCode(c, email, false)
}

// signupEmail normalises the address a visitor typed and checks it the
// way the admin licence form does, since it becomes a licence's email.
// On failure it writes the 400 and returns false.
func signupEmail(c *gin.Context, raw string) (string, bool) {
	email := strings.ToLower(strings.TrimSpace(raw))
	if appErr := apperr.ValidateEmail(email); appErr != nil {
		response.BadRequest(c, appErr.Message)
		return "", false
	}
	return email, true
}

func signupFull(c *gin.Context) {
	// The cap counts sign-ups since midnight UTC, so that is when it
	// opens again.
	now := time.Now().UTC()
	midnight := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC)
	c.Header("Retry-After", strconv.Itoa(int(midnight.Sub(now).Seconds())+1))
	response.Err(c, http.StatusTooManyRequests, "SIGNUP_LIMIT_REACHED",
		"this plan has taken all the sign-ups it accepts today; try again tomorrow, or sign in if you already have a license")
}

// SignupVerify handles POST /api/v1/signup/:checkout_id/verify
func (h *AuthHandler) SignupVerify(c *gin.Context) {
	var req struct {
		Email            string `json:"email" binding:"required,email"`
		Code             string `json:"code" binding:"required"`
		MarketingConsent bool   `json:"marketing_consent"`
		// The consent wording the form showed, so a yes is recorded
		// against what the visitor read.
		ConsentText string `json:"consent_text"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "email and code are required")
		return
	}
	email, ok := signupEmail(c, req.Email)
	if !ok {
		return
	}
	plan := h.signupPlan(c)
	if plan == nil {
		return
	}
	if !h.checkCode(c, email, strings.TrimSpace(req.Code)) {
		return
	}

	// One self sign-up licence per address per product: a second claim
	// signs in to the first (on whatever plan it is now) instead of
	// minting another.
	//
	// The account is made only once the address holds a licence. Made
	// before the claim, a claim refused after it (cap reached, plan
	// closed) would leave an account behind, and an account is all
	// signup_mode=licensed_only asks of a login.
	if existing, err := h.Store.FindSelfSignupLicense(c, plan.ProductID, email); err == nil {
		h.signInExisting(c, email, existing)
		return
	} else if !errors.Is(err, sql.ErrNoRows) {
		response.Internal(c, err)
		return
	}

	status := model.StatusActive
	if plan.LicenseType == "trial" {
		status = model.StatusTrialing
	}
	l := &model.License{
		ProductID:       plan.ProductID,
		PlanID:          plan.ID,
		Email:           email,
		LicenseKey:      license.GenerateKey(""),
		Status:          status,
		UpdatesTermsSet: true,
	}
	if plan.LicenseType == "trial" && plan.TrialDays > 0 {
		until := time.Now().Add(time.Duration(plan.TrialDays) * 24 * time.Hour)
		l.ValidUntil = &until
	}
	// Consent counts only when the form asked for it, and only for the
	// wording the plan holds now: a yes given to a label the operator has
	// since edited is not a yes to the new one, so the licence is created
	// without it.
	if plan.SignupConsentText != "" && req.MarketingConsent && req.ConsentText == plan.SignupConsentText {
		now := time.Now()
		l.MarketingConsent = true
		l.MarketingConsentAt = &now
	}

	if err := h.Store.CreateSelfSignupLicense(c, l, plan); err != nil {
		switch {
		case errors.Is(err, store.ErrSelfSignupExists) || store.IsSelfSignupConflict(err):
			// Another confirmation for this address committed first.
			existing, ferr := h.Store.FindSelfSignupLicense(c, plan.ProductID, email)
			if ferr != nil {
				response.Internal(c, ferr)
				return
			}
			h.signInExisting(c, email, existing)
		case errors.Is(err, store.ErrSelfSignupClosed):
			response.Err(c, http.StatusNotFound, "SIGNUP_UNAVAILABLE", "sign-up is not available for this plan")
		case errors.Is(err, store.ErrSignupCapReached):
			signupFull(c)
		case errors.Is(err, store.ErrPlanChanged):
			response.Conflict(c, "PLAN_CHANGED",
				"the plan's license type changed while this license was being created; reload and try again", nil)
		default:
			response.Internal(c, err)
		}
		return
	}

	// The licence is written: from here on a failure must not lose it.
	// A sign-in that fails leaves it mailed and in place, and the next
	// claim signs in to it.
	user, err := h.loginUser(c, email)
	if err != nil {
		slog.Error("self sign-up: licence created but sign-in failed", "license_id", l.ID, "error", err)
	} else if err := h.Store.UpdateLicenseUser(c, l.ID, user.ID); err != nil {
		// The portal finds licences by email too, so this only costs
		// the direct link.
		slog.Warn("self sign-up: failed to link licence to its account", "license_id", l.ID, "error", err)
	}

	h.mailSelfSignupLicense(c, l, plan)

	changes := map[string]any{
		"email": email, "plan_id": plan.ID, "marketing_consent": l.MarketingConsent,
	}
	if l.MarketingConsent {
		// What the visitor agreed to, as worded then: the plan's label
		// can be edited later.
		changes["consent_text"] = plan.SignupConsentText
	}
	actorID := ""
	if user != nil {
		actorID = user.ID
	}
	h.Store.Audit(c, &model.AuditLog{
		Entity: "license", EntityID: l.ID, Action: "created",
		ActorType: model.SignupSourceSelf, ActorID: actorID, IPAddress: c.ClientIP(),
		Changes: changes,
	})

	if h.Webhook != nil {
		h.Webhook.Dispatch(c, l.ProductID, model.EventLicenseCreated, map[string]any{
			"license_id": l.ID, "email": l.Email, "plan_id": l.PlanID,
			"signup_source": l.SignupSource, "marketing_consent": l.MarketingConsent,
		})
	}

	if user == nil {
		response.Internal(c, errors.New("licence created but sign-in failed"))
		return
	}
	h.startSession(c, user)
	response.Created(c, gin.H{"status": "created", "license_id": l.ID})
}

// signInExisting signs email in to the self sign-up licence it already
// holds. The address holds a licence, so making its account here is
// what a login would do anyway. Whether that licence still works goes
// back too, judged the way verification judges it: one that was
// revoked or has run out is still the address's one claim, and the
// form says so rather than "you already have it".
func (h *AuthHandler) signInExisting(c *gin.Context, email string, l *model.License) {
	user, err := h.loginUser(c, email)
	if err != nil {
		response.Internal(c, err)
		return
	}
	h.startSession(c, user)
	response.OK(c, gin.H{
		"status": "existing", "license_id": l.ID,
		"license_status": l.Status, "license_usable": service.LicenseUsable(l) == nil,
	})
}

// mailSelfSignupLicense queues the licence email, the same template a
// purchase gets. Queued rather than sent inline so an SMTP hiccup is
// retried instead of losing the key; the portal shows it either way.
func (h *AuthHandler) mailSelfSignupLicense(c *gin.Context, l *model.License, plan *model.Plan) {
	if h.Email == nil {
		return
	}
	productName, downloadURL := "", ""
	if p, err := h.Store.FindProductByID(c, l.ProductID); err == nil {
		productName, downloadURL = p.Name, p.DownloadURL
	}
	subject, body := h.Email.RenderLicenseCreated(productName, plan.Name, h.Store.DecryptLicenseKey(l), downloadURL)
	if err := h.Store.EnqueueEmail(c, l.Email, subject, body); err != nil {
		slog.Error("self sign-up: failed to queue licence email", "license_id", l.ID, "error", err)
	}
}
