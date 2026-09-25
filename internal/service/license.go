package service

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/tabloy/keygate/internal/branding"
	"github.com/tabloy/keygate/internal/license"
	"github.com/tabloy/keygate/internal/middleware"
	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
	"github.com/tabloy/keygate/pkg/apperr"
)

// FailureTracker tracks failed authentication attempts for brute-force protection.
type FailureTracker interface {
	RecordFailure(key string)
	RecordSuccess(key string)
	IsBlocked(key string) (bool, time.Duration)
}

// licenseSigningKey is satisfied by ed25519.PrivateKey. Aliased
// here so we don't widen the service's exported signature when the
// key format changes; the service only ever signs, never verifies.
type licenseSigningKey = ed25519.PrivateKey

type LicenseService struct {
	store      *store.Store
	signingKey licenseSigningKey
	logger     *slog.Logger
	failures   FailureTracker
	webhook    *WebhookService
}

func NewLicenseService(s *store.Store, signingKey licenseSigningKey, logger *slog.Logger, failures FailureTracker, webhook *WebhookService) *LicenseService {
	return &LicenseService{store: s, signingKey: signingKey, logger: logger, failures: failures, webhook: webhook}
}

// SigningPublicKey returns the ed25519 public key that pairs with
// the service's signing key. Exposed so handlers can serve it on
// /api/v1/license/pubkey without poking at the private half.
func (s *LicenseService) SigningPublicKey() ed25519.PublicKey {
	return license.PublicKey(s.signingKey)
}

// ─── Activate ───

type ActivateInput struct {
	LicenseKey     string
	Identifier     string
	IdentifierType string // "device" | "user"
	Label          string
	IPAddress      string
	// ProductID: optional tenant scope. When non-empty the lookup
	// rejects licenses that don't belong to it. Public SDK endpoints
	// pass empty (license_key alone identifies tenant); server-to-server
	// integrations may pass the product ID for explicit verification.
	ProductID string
}

type ActivateResult struct {
	Status    string         `json:"status"` // "activated" | "already_activated"
	LicenseID string         `json:"license_id"`
	Token     string         `json:"token"`
	Meta      map[string]any `json:"meta"`
}

func (s *LicenseService) Activate(ctx context.Context, in ActivateInput) (*ActivateResult, error) {
	if s.failures != nil {
		if blocked, retryAfter := s.failures.IsBlocked(middleware.FailureKeyIP(in.IPAddress)); blocked {
			return nil, lockedOut(retryAfter)
		}
	}

	if in.IdentifierType == "" {
		in.IdentifierType = "device"
	}

	lic, err := s.store.FindLicenseByKey(ctx, in.LicenseKey)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			if locked := s.recordFailure(in.IPAddress); locked != nil {
				return nil, locked
			}
			return nil, apperr.New(404, "LICENSE_NOT_FOUND", "license not found")
		}
		return nil, apperr.Internal(err)
	}

	if in.ProductID != "" && lic.ProductID != in.ProductID {
		if locked := s.recordFailure(in.IPAddress); locked != nil {
			return nil, locked
		}
		return nil, apperr.New(404, "LICENSE_NOT_FOUND", "license not found")
	}

	// Product-type capability gate: a SaaS product doesn't expose
	// per-device activation. Surfaces as 404 FEATURE_NOT_AVAILABLE so
	// the SDK call looks like the endpoint just doesn't exist for this
	// product — not a license-state failure the customer needs to fix.
	if err := requireProductCapability(lic.Product, model.CapActivations, "device activation"); err != nil {
		return nil, err
	}

	if err := s.assertUsable(lic); err != nil {
		return nil, err
	}

	if existing, err := s.store.FindActivation(ctx, lic.ID, in.Identifier); err == nil {
		_ = s.store.TouchActivation(ctx, existing.ID)
		middleware.LicenseActivations.WithLabelValues(lic.ProductID, "already_activated").Inc()
		token, err := s.signToken(lic, in.Identifier)
		if err != nil {
			return nil, apperr.Internal(err)
		}
		return &ActivateResult{
			Status: "already_activated", LicenseID: lic.ID,
			Token: token,
			Meta:  responseMeta(),
		}, nil
	}

	max := s.maxActivations(lic)
	act := &model.Activation{
		LicenseID: lic.ID, Identifier: in.Identifier,
		IdentifierType: in.IdentifierType, Label: in.Label,
		IPAddress: in.IPAddress,
	}
	if err := s.store.ActivateWithinLimit(ctx, act, max); err != nil {
		// The machine already held a slot. The read above missed it
		// because it ran outside the transaction, so this is the same
		// answer that read would have given: a repeat activation is
		// the licence working, not a failure.
		if errors.Is(err, store.ErrAlreadyActivated) {
			_ = s.store.TouchActivation(ctx, act.ID)
			middleware.LicenseActivations.WithLabelValues(lic.ProductID, "already_activated").Inc()
			token, terr := s.signToken(lic, in.Identifier)
			if terr != nil {
				return nil, apperr.Internal(terr)
			}
			return &ActivateResult{
				Status: "already_activated", LicenseID: lic.ID,
				Token: token,
				Meta:  responseMeta(),
			}, nil
		}
		if err.Error() == "activation limit reached" {
			middleware.LicenseActivations.WithLabelValues(lic.ProductID, "failed").Inc()
			count, _ := s.store.CountActivations(ctx, lic.ID)
			return nil, apperr.WithDetails(
				apperr.Conflict("ACTIVATION_LIMIT", "maximum activations reached"),
				map[string]any{"max": max, "current": count},
			)
		}
		return nil, apperr.Internal(err)
	}

	s.store.Audit(ctx, &model.AuditLog{
		Entity: "license", EntityID: lic.ID, Action: "activated",
		ActorType: "sdk", IPAddress: in.IPAddress,
		Changes: map[string]any{"identifier": in.Identifier, "type": in.IdentifierType, "label": in.Label},
	})

	if s.failures != nil {
		s.failures.RecordSuccess(middleware.FailureKeyIP(in.IPAddress))
	}

	s.logger.Info("license activated",
		"license_id", lic.ID, "identifier", in.Identifier, "type", in.IdentifierType)

	middleware.LicenseActivations.WithLabelValues(lic.ProductID, "activated").Inc()

	if s.webhook != nil {
		s.webhook.Dispatch(ctx, lic.ProductID, "license.activated", map[string]any{
			"license_id": lic.ID, "identifier": in.Identifier, "type": in.IdentifierType,
		})
	}

	token, err := s.signToken(lic, in.Identifier)
	if err != nil {
		return nil, apperr.Internal(err)
	}

	return &ActivateResult{
		Status: "activated", LicenseID: lic.ID,
		Token: token,
		Meta:  responseMeta(),
	}, nil
}

// recordFailure files a failed attempt and reports the lockout when
// this was the attempt that caused one. The guard middleware turns
// away everything after it, but the request that crosses the line is
// answered from here — and a plain 404 tells a client nothing about
// the wall it has just walked into, so it comes straight back.
func (s *LicenseService) recordFailure(ip string) *apperr.AppError {
	if s.failures == nil {
		return nil
	}
	key := middleware.FailureKeyIP(ip)
	s.failures.RecordFailure(key)
	if blocked, retryAfter := s.failures.IsBlocked(key); blocked {
		return lockedOut(retryAfter)
	}
	return nil
}

// lockedOut is the refusal a caller meets while their address is
// locked out. It carries how long to wait: the guard middleware sets
// Retry-After when it turns a request away on its own, but the
// request that trips the lock is answered from here, and a client
// told only "too many failed attempts" retries straight away.
func lockedOut(retryAfter time.Duration) *apperr.AppError {
	return apperr.WithDetails(
		apperr.New(429, "LOCKED_OUT", "too many failed attempts"),
		map[string]any{"retry_after": middleware.RetryAfterSeconds(retryAfter)},
	)
}

// ─── Verify ───

type VerifyInput struct {
	LicenseKey string
	Identifier string
	ProductID  string
	IPAddress  string
}

type VerifyResult struct {
	Status     string     `json:"status"`
	PlanID     string     `json:"plan_id"`
	PlanName   string     `json:"plan_name"`
	ValidUntil *time.Time `json:"valid_until,omitempty"`
	// UpdatesUntil is the end of a perpetual license's maintenance
	// period: releases published after it cannot be installed. Absent
	// when the license has no separate limit.
	UpdatesUntil *time.Time     `json:"updates_until,omitempty"`
	Features     map[string]any `json:"features"`
	Token        string         `json:"token"`
	GraceDays    int            `json:"grace_days"`
	Meta         map[string]any `json:"meta"`
	// External identifiers echoed back so the SDK can confirm the
	// license belongs to the workspace it expects. Empty when the
	// license was created without them. Optional in the JSON envelope
	// to keep the payload small for the common case.
	ExternalCustomerID  string `json:"external_customer_id,omitempty"`
	ExternalWorkspaceID string `json:"external_workspace_id,omitempty"`
}

func (s *LicenseService) Verify(ctx context.Context, in VerifyInput) (*VerifyResult, error) {
	if s.failures != nil {
		if blocked, retryAfter := s.failures.IsBlocked(middleware.FailureKeyIP(in.IPAddress)); blocked {
			return nil, lockedOut(retryAfter)
		}
	}

	lic, err := s.store.FindLicenseByKey(ctx, in.LicenseKey)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			if locked := s.recordFailure(in.IPAddress); locked != nil {
				return nil, locked
			}
			return nil, apperr.New(404, "LICENSE_NOT_FOUND", "license not found")
		}
		return nil, apperr.Internal(err)
	}

	if in.ProductID != "" && lic.ProductID != in.ProductID {
		if locked := s.recordFailure(in.IPAddress); locked != nil {
			return nil, locked
		}
		return nil, apperr.New(404, "LICENSE_NOT_FOUND", "license not found")
	}

	// Verify is part of the activation flow — also gated by product type.
	if err := requireProductCapability(lic.Product, model.CapActivations, "device verification"); err != nil {
		return nil, err
	}

	// Verify is unauthenticated. We collapse all "license-knowable" failures
	// (suspended / revoked / expired / wrong device) into the same 404 the
	// non-existent-key path returns, so the endpoint can't be used to probe
	// which license_key strings are real. Paid users learn lifecycle state
	// via email + the (session-auth) portal, not via this routine call.
	if err := s.assertUsable(lic); err != nil {
		locked := s.recordFailure(in.IPAddress)
		middleware.LicenseVerifications.WithLabelValues(lic.ProductID, "unusable").Inc()
		if locked != nil {
			return nil, locked
		}
		return nil, licenseNotFound()
	}

	act, err := s.store.FindActivation(ctx, lic.ID, in.Identifier)
	if err != nil {
		middleware.LicenseVerifications.WithLabelValues(lic.ProductID, "not_activated").Inc()
		// Not yet activated for this device → looks identical to "no
		// such license". Otherwise this is the loudest existence
		// oracle: 403 NOT_ACTIVATED tells the attacker a key is real.
		return nil, licenseNotFound()
	}
	_ = s.store.TouchActivation(ctx, act.ID)

	if s.failures != nil {
		s.failures.RecordSuccess(middleware.FailureKeyIP(in.IPAddress))
	}

	planName := ""
	if lic.Plan != nil {
		planName = lic.Plan.Name
	}

	middleware.LicenseVerifications.WithLabelValues(lic.ProductID, "valid").Inc()

	token, err := s.signToken(lic, in.Identifier)
	if err != nil {
		return nil, apperr.Internal(err)
	}

	return &VerifyResult{
		Status:              lic.Status,
		PlanID:              lic.PlanID,
		PlanName:            planName,
		ValidUntil:          lic.ValidUntil,
		UpdatesUntil:        lic.EffectiveUpdatesUntil(),
		Features:            s.entitlements(lic),
		Token:               token,
		GraceDays:           s.effectiveGraceDays(lic),
		Meta:                responseMeta(),
		ExternalCustomerID:  lic.ExternalCustomerID,
		ExternalWorkspaceID: lic.ExternalWorkspaceID,
	}, nil
}

// ─── Deactivate ───

type DeactivateInput struct {
	LicenseKey string
	Identifier string
	ProductID  string
	IPAddress  string
}

func (s *LicenseService) Deactivate(ctx context.Context, in DeactivateInput) error {
	if s.failures != nil {
		if blocked, retryAfter := s.failures.IsBlocked(middleware.FailureKeyIP(in.IPAddress)); blocked {
			return lockedOut(retryAfter)
		}
	}

	lic, err := s.store.FindLicenseByKey(ctx, in.LicenseKey)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			if locked := s.recordFailure(in.IPAddress); locked != nil {
				return locked
			}
			return apperr.New(404, "LICENSE_NOT_FOUND", "license not found")
		}
		return apperr.Internal(err)
	}

	if in.ProductID != "" && lic.ProductID != in.ProductID {
		if locked := s.recordFailure(in.IPAddress); locked != nil {
			return locked
		}
		return apperr.New(404, "LICENSE_NOT_FOUND", "license not found")
	}

	if err := requireProductCapability(lic.Product, model.CapActivations, "device deactivation"); err != nil {
		return err
	}

	act, err := s.store.FindActivation(ctx, lic.ID, in.Identifier)
	if err != nil {
		// Same 404 LICENSE_NOT_FOUND as "license missing" so deactivate
		// can't be used to probe for real keys ("oh, this key returned
		// ACTIVATION_NOT_FOUND, that's specific — must exist").
		return licenseNotFound()
	}

	if err := s.store.DeleteActivation(ctx, act.ID); err != nil {
		return apperr.Internal(err)
	}

	if s.failures != nil {
		s.failures.RecordSuccess(middleware.FailureKeyIP(in.IPAddress))
	}

	s.store.Audit(ctx, &model.AuditLog{
		Entity: "license", EntityID: lic.ID, Action: "deactivated",
		ActorType: "sdk", IPAddress: in.IPAddress,
		Changes: map[string]any{"identifier": in.Identifier},
	})

	s.logger.Info("license deactivated", "license_id", lic.ID, "identifier", in.Identifier)

	if s.webhook != nil {
		s.webhook.Dispatch(ctx, lic.ProductID, "license.deactivated", map[string]any{
			"license_id": lic.ID, "identifier": in.Identifier,
		})
	}

	return nil
}

// ─── Helpers ───

// licenseNotFound returns a generic 404 used for unauthenticated public
// SDK responses. We deliberately do NOT distinguish between
//
//   - the license_key doesn't exist
//   - the license belongs to another product
//   - the license is suspended / revoked / expired / past-grace
//
// because all of those reveal the EXISTENCE of a real key. The portal
// (session-authenticated) and admin endpoints surface the specific
// status; SDK clients learn details via email or by checking the portal.
func licenseNotFound() error {
	return apperr.New(404, "LICENSE_NOT_FOUND", "license not found")
}

// loadLicenseForSDK is the canonical license-lookup pattern for
// unauthenticated, license-key-bearing SDK endpoints (download,
// usage, seats, floating, entitlements). It performs every check
// (key lookup → product scope → product capability → lifecycle
// usability) and collapses every failure into the same 404
// LICENSE_NOT_FOUND so the endpoint surface can't be used to probe
// which license keys are real, which products own them, or which
// lifecycle state they're in.
//
//   - capability == "": skip the capability check
//   - usabilityRequired == false: skip the lifecycle check (use for
//     read-only endpoints that should still respond to suspended
//     licenses; today every caller passes true)
//
// Admin endpoints (session-authenticated) MUST NOT use this helper —
// they need specific reasons (LICENSE_EXPIRED, INCOMPATIBLE_PRODUCT_TYPE,
// etc.) to drive UI affordances.
func loadLicenseForSDK(ctx context.Context, st *store.Store, key, productID, capability string, usabilityRequired bool) (*model.License, error) {
	lic, err := st.FindLicenseByKey(ctx, key)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, licenseNotFound()
		}
		return nil, apperr.Internal(err)
	}
	if productID != "" && lic.ProductID != productID {
		return nil, licenseNotFound()
	}
	if capability != "" {
		if lic.Product == nil || !model.ProductSupports(lic.Product.Type, capability) {
			return nil, licenseNotFound()
		}
	}
	if usabilityRequired && !IsLicenseUsable(lic) {
		return nil, licenseNotFound()
	}
	return lic, nil
}

// requireProductCapability is the central guard for product-type
// capability gates. It returns nil when the product supports the
// requested capability, or a 404 FEATURE_NOT_AVAILABLE error when it
// doesn't. We use 404 instead of 400 because from the caller's
// perspective the endpoint simply doesn't exist for this product —
// matching how a missing route would look.
func requireProductCapability(prod *model.Product, capability, hint string) error {
	if prod == nil {
		return apperr.Internal(fmt.Errorf("product not loaded for capability check"))
	}
	if model.ProductSupports(prod.Type, capability) {
		return nil
	}
	return apperr.New(404, "FEATURE_NOT_AVAILABLE",
		fmt.Sprintf("%s is not available for %s products", hint, prod.Type))
}

// assertUsable checks lifecycle status — usable now or not. The returned
// error is the specific reason (LICENSE_EXPIRED / CANCELED / etc.); the
// caller decides whether to surface it (Activate path: yes, paid users
// renewing) or collapse to licenseNotFound() (Verify / Deactivate: no,
// avoid existence oracle).
func (s *LicenseService) assertUsable(lic *model.License) error {
	now := time.Now()
	switch lic.Status {
	case model.StatusActive, model.StatusTrialing, model.StatusPastDue:
		if lic.ValidUntil != nil && now.After(*lic.ValidUntil) {
			grace := time.Duration(s.effectiveGraceDays(lic)) * 24 * time.Hour
			if now.After(lic.ValidUntil.Add(grace)) {
				return apperr.New(403, "LICENSE_EXPIRED", "license has expired")
			}
		}
		return nil
	case model.StatusCanceled:
		if lic.ValidUntil != nil && now.Before(*lic.ValidUntil) {
			return nil
		}
		return apperr.New(403, "LICENSE_CANCELED", "license has been canceled")
	case model.StatusSuspended:
		return apperr.New(403, "LICENSE_SUSPENDED", "license has been suspended")
	case model.StatusRevoked:
		return apperr.New(403, "LICENSE_REVOKED", "license has been revoked")
	case model.StatusExpired:
		return apperr.New(403, "LICENSE_EXPIRED", "license has expired")
	default:
		return apperr.New(403, "LICENSE_INVALID", "license is not valid")
	}
}

func (s *LicenseService) maxActivations(lic *model.License) int {
	if lic.Plan != nil {
		return lic.Plan.MaxActivations
	}
	return 3
}

func (s *LicenseService) graceDays(lic *model.License) int {
	if lic.Plan != nil {
		return lic.Plan.GraceDays
	}
	return 7
}

// effectiveGraceDays is the grace this licence actually gets, which is
// not always the plan's number: a canceled licence is paid up to
// ValidUntil and stops dead there.
//
// assertUsable, the signed token and the verify envelope must all read
// the same value. When they drift, the client is told a grace period
// the server will not honour — it keeps working offline for a week
// while every online call already returns LICENSE_CANCELED.
func (s *LicenseService) effectiveGraceDays(lic *model.License) int {
	switch lic.Status {
	case model.StatusActive, model.StatusTrialing, model.StatusPastDue:
		return s.graceDays(lic)
	default:
		return 0
	}
}

func (s *LicenseService) entitlements(lic *model.License) map[string]any {
	m := make(map[string]any)
	if lic.Plan == nil {
		return m
	}
	for _, e := range lic.Plan.Entitlements {
		switch e.ValueType {
		case "bool":
			m[e.Feature] = e.Value == "true"
		default:
			m[e.Feature] = e.Value
		}
	}
	return m
}

func responseMeta() map[string]any {
	return map[string]any{"server": branding.Project, "url": branding.URL}
}

// tokenTTL is the default check-in interval: how long a signed token
// stays usable offline before the client has to verify again. A plan
// can override it with token_ttl_days. It is a check-in interval, not
// a licence term — see the clamp below.
const tokenTTL = 7 * 24 * time.Hour

func (s *LicenseService) tokenTTLFor(lic *model.License) time.Duration {
	if lic.Plan != nil && lic.Plan.TokenTTLDays > 0 {
		return time.Duration(lic.Plan.TokenTTLDays) * 24 * time.Hour
	}
	return tokenTTL
}

func (s *LicenseService) signToken(lic *model.License, identifier string) (string, error) {
	now := time.Now()

	// A token must never outlive the licence it was issued for. With a
	// flat 7-day TTL, a plan whose grace period is shorter than that
	// handed out tokens that kept verifying offline after the licence
	// was truly dead: valid_until tomorrow + grace_days 0 still bought
	// six extra days. Clamp to whichever comes first.
	expiresAt := now.Add(s.tokenTTLFor(lic))

	// Grace comes from effectiveGraceDays so the token, the envelope
	// around it and assertUsable cannot disagree about when this
	// licence dies.
	grace := s.effectiveGraceDays(lic)

	var validUntil int64
	if lic.ValidUntil != nil {
		validUntil = lic.ValidUntil.Unix()
		deadline := lic.ValidUntil.Add(time.Duration(grace) * 24 * time.Hour)
		if deadline.Before(expiresAt) {
			expiresAt = deadline
		}
	}

	var updatesUntil int64
	if u := lic.EffectiveUpdatesUntil(); u != nil {
		updatesUntil = u.Unix()
	}

	t := &license.VerifyToken{
		LicenseID:    lic.ID,
		ProductID:    lic.ProductID,
		PlanID:       lic.PlanID,
		Status:       lic.Status,
		Identifier:   identifier,
		Features:     s.entitlements(lic),
		IssuedAt:     now.Unix(),
		ExpiresAt:    expiresAt.Unix(),
		ValidUntil:   validUntil,
		UpdatesUntil: updatesUntil,
		GraceDays:    grace,
		Fingerprint:  license.Fingerprint(identifier, lic.ProductID),
	}
	return license.Sign(t, s.signingKey)
}
