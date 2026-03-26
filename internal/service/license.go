package service

import (
	"context"
	"database/sql"
	"errors"
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

type LicenseService struct {
	store      *store.Store
	signingKey string
	logger     *slog.Logger
	failures   FailureTracker
	webhook    *WebhookService
}

func NewLicenseService(s *store.Store, signingKey string, logger *slog.Logger, failures FailureTracker, webhook *WebhookService) *LicenseService {
	return &LicenseService{store: s, signingKey: signingKey, logger: logger, failures: failures, webhook: webhook}
}

// ─── Activate ───

type ActivateInput struct {
	LicenseKey     string
	Identifier     string
	IdentifierType string // "device" | "user"
	Label          string
	IPAddress      string
	ProductID      string // from API key context; empty = skip product check
}

type ActivateResult struct {
	Status    string         `json:"status"` // "activated" | "already_activated"
	LicenseID string         `json:"license_id"`
	Token     string         `json:"token"`
	Meta      map[string]any `json:"meta"`
}

func (s *LicenseService) Activate(ctx context.Context, in ActivateInput) (*ActivateResult, error) {
	if s.failures != nil {
		if blocked, _ := s.failures.IsBlocked("ip:" + in.IPAddress); blocked {
			return nil, apperr.New(429, "LOCKED_OUT", "too many failed attempts")
		}
	}

	if in.IdentifierType == "" {
		in.IdentifierType = "device"
	}

	lic, err := s.store.FindLicenseByKey(ctx, in.LicenseKey)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			if s.failures != nil {
				s.failures.RecordFailure("key:" + in.LicenseKey)
				s.failures.RecordFailure("ip:" + in.IPAddress)
			}
			return nil, apperr.New(404, "LICENSE_NOT_FOUND", "license not found")
		}
		return nil, apperr.Internal(err)
	}

	if in.ProductID != "" && lic.ProductID != in.ProductID {
		if s.failures != nil {
			s.failures.RecordFailure("key:" + in.LicenseKey)
			s.failures.RecordFailure("ip:" + in.IPAddress)
		}
		return nil, apperr.New(404, "LICENSE_NOT_FOUND", "license not found")
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
		ActorType: "apikey", IPAddress: in.IPAddress,
		Changes: map[string]any{"identifier": in.Identifier, "type": in.IdentifierType, "label": in.Label},
	})

	if s.failures != nil {
		s.failures.RecordSuccess("key:" + in.LicenseKey)
		s.failures.RecordSuccess("ip:" + in.IPAddress)
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

// ─── Verify ───

type VerifyInput struct {
	LicenseKey string
	Identifier string
	ProductID  string
	IPAddress  string
}

type VerifyResult struct {
	Status     string         `json:"status"`
	PlanID     string         `json:"plan_id"`
	PlanName   string         `json:"plan_name"`
	ValidUntil *time.Time     `json:"valid_until,omitempty"`
	Features   map[string]any `json:"features"`
	Token      string         `json:"token"`
	GraceDays  int            `json:"grace_days"`
	Meta       map[string]any `json:"meta"`
}

func (s *LicenseService) Verify(ctx context.Context, in VerifyInput) (*VerifyResult, error) {
	if s.failures != nil {
		if blocked, _ := s.failures.IsBlocked("ip:" + in.IPAddress); blocked {
			return nil, apperr.New(429, "LOCKED_OUT", "too many failed attempts")
		}
	}

	lic, err := s.store.FindLicenseByKey(ctx, in.LicenseKey)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			if s.failures != nil {
				s.failures.RecordFailure("key:" + in.LicenseKey)
				s.failures.RecordFailure("ip:" + in.IPAddress)
			}
			return nil, apperr.New(404, "LICENSE_NOT_FOUND", "license not found")
		}
		return nil, apperr.Internal(err)
	}

	if in.ProductID != "" && lic.ProductID != in.ProductID {
		if s.failures != nil {
			s.failures.RecordFailure("key:" + in.LicenseKey)
			s.failures.RecordFailure("ip:" + in.IPAddress)
		}
		return nil, apperr.New(404, "LICENSE_NOT_FOUND", "license not found")
	}

	// Reject suspended/revoked/expired licenses — don't issue fresh tokens
	if err := s.assertUsable(lic); err != nil {
		return nil, err
	}

	act, err := s.store.FindActivation(ctx, lic.ID, in.Identifier)
	if err != nil {
		middleware.LicenseVerifications.WithLabelValues(lic.ProductID, "not_activated").Inc()
		return nil, apperr.New(403, "NOT_ACTIVATED", "identifier not activated for this license")
	}
	_ = s.store.TouchActivation(ctx, act.ID)

	if s.failures != nil {
		s.failures.RecordSuccess("key:" + in.LicenseKey)
		s.failures.RecordSuccess("ip:" + in.IPAddress)
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
		Status:     lic.Status,
		PlanID:     lic.PlanID,
		PlanName:   planName,
		ValidUntil: lic.ValidUntil,
		Features:   s.entitlements(lic),
		Token:      token,
		GraceDays:  s.graceDays(lic),
		Meta:       responseMeta(),
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
		if blocked, _ := s.failures.IsBlocked("ip:" + in.IPAddress); blocked {
			return apperr.New(429, "LOCKED_OUT", "too many failed attempts")
		}
	}

	lic, err := s.store.FindLicenseByKey(ctx, in.LicenseKey)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			if s.failures != nil {
				s.failures.RecordFailure("key:" + in.LicenseKey)
				s.failures.RecordFailure("ip:" + in.IPAddress)
			}
			return apperr.New(404, "LICENSE_NOT_FOUND", "license not found")
		}
		return apperr.Internal(err)
	}

	if in.ProductID != "" && lic.ProductID != in.ProductID {
		if s.failures != nil {
			s.failures.RecordFailure("key:" + in.LicenseKey)
			s.failures.RecordFailure("ip:" + in.IPAddress)
		}
		return apperr.New(404, "LICENSE_NOT_FOUND", "license not found")
	}

	act, err := s.store.FindActivation(ctx, lic.ID, in.Identifier)
	if err != nil {
		return apperr.New(404, "ACTIVATION_NOT_FOUND", "activation not found")
	}

	if err := s.store.DeleteActivation(ctx, act.ID); err != nil {
		return apperr.Internal(err)
	}

	if s.failures != nil {
		s.failures.RecordSuccess("key:" + in.LicenseKey)
		s.failures.RecordSuccess("ip:" + in.IPAddress)
	}

	s.store.Audit(ctx, &model.AuditLog{
		Entity: "license", EntityID: lic.ID, Action: "deactivated",
		ActorType: "apikey", IPAddress: in.IPAddress,
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

func (s *LicenseService) assertUsable(lic *model.License) error {
	now := time.Now()
	switch lic.Status {
	case model.StatusActive, model.StatusTrialing, model.StatusPastDue:
		if lic.ValidUntil != nil && now.After(*lic.ValidUntil) {
			grace := time.Duration(s.graceDays(lic)) * 24 * time.Hour
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

func (s *LicenseService) signToken(lic *model.License, identifier string) (string, error) {
	now := time.Now()
	t := &license.VerifyToken{
		LicenseID:   lic.ID,
		ProductID:   lic.ProductID,
		PlanID:      lic.PlanID,
		Status:      lic.Status,
		Identifier:  identifier,
		Features:    s.entitlements(lic),
		IssuedAt:    now.Unix(),
		ExpiresAt:   now.Add(7 * 24 * time.Hour).Unix(),
		GraceDays:   s.graceDays(lic),
		Fingerprint: license.Fingerprint(identifier, lic.ProductID),
	}
	return license.Sign(t, s.signingKey)
}
