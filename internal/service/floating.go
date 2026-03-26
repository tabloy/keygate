package service

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"time"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
	"github.com/tabloy/keygate/pkg/apperr"
)

type FloatingService struct {
	store  *store.Store
	logger *slog.Logger
}

func NewFloatingService(s *store.Store, logger *slog.Logger) *FloatingService {
	return &FloatingService{store: s, logger: logger}
}

type CheckOutInput struct {
	LicenseKey string
	Identifier string
	Label      string
	ProductID  string
	IPAddress  string
}

type CheckOutResult struct {
	SessionID string    `json:"session_id"`
	ExpiresAt time.Time `json:"expires_at"`
	Active    int       `json:"active_sessions"`
	Max       int       `json:"max_sessions"`
}

func (s *FloatingService) CheckOut(ctx context.Context, in CheckOutInput) (*CheckOutResult, error) {
	lic, err := s.store.FindLicenseByKey(ctx, in.LicenseKey)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, apperr.New(404, "LICENSE_NOT_FOUND", "license not found")
		}
		return nil, apperr.Internal(err)
	}
	if in.ProductID != "" && lic.ProductID != in.ProductID {
		return nil, apperr.New(404, "LICENSE_NOT_FOUND", "license not found")
	}

	if lic.Plan == nil || lic.Plan.LicenseModel != "floating" {
		return nil, apperr.BadRequest("license does not support floating model")
	}

	if lic.Status != model.StatusActive && lic.Status != model.StatusTrialing {
		return nil, apperr.New(403, "LICENSE_NOT_ACTIVE", "license is not active")
	}

	timeout := lic.Plan.FloatingTimeout
	if timeout <= 0 {
		timeout = 30
	}
	maxSessions := lic.Plan.MaxActivations

	expiresAt := time.Now().Add(time.Duration(timeout) * time.Minute)
	sess := &model.FloatingSession{
		LicenseID: lic.ID, Identifier: in.Identifier,
		Label: in.Label, IPAddress: in.IPAddress, ExpiresAt: expiresAt,
	}

	// Atomically check capacity and create/refresh session using DB-level locking
	// This prevents race conditions where concurrent checkouts could exceed maxSessions
	isNew, err := s.store.CheckOutFloatingWithLimit(ctx, sess, maxSessions)
	if err != nil {
		if errors.Is(err, store.ErrFloatingLimitReached) {
			active, _ := s.store.CountActiveFloating(ctx, lic.ID)
			return nil, apperr.WithDetails(
				apperr.Conflict("FLOATING_LIMIT", "all floating sessions are in use"),
				map[string]any{"active": active, "max": maxSessions},
			)
		}
		return nil, apperr.Internal(err)
	}

	active, _ := s.store.CountActiveFloating(ctx, lic.ID)
	if isNew {
		s.logger.Info("floating checkout", "license_id", lic.ID, "identifier", in.Identifier, "active", active)
	}

	return &CheckOutResult{
		SessionID: sess.ID, ExpiresAt: expiresAt,
		Active: active, Max: maxSessions,
	}, nil
}

func (s *FloatingService) CheckIn(ctx context.Context, licenseKey, identifier, productID string) error {
	lic, err := s.store.FindLicenseByKey(ctx, licenseKey)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return apperr.New(404, "LICENSE_NOT_FOUND", "license not found")
		}
		return apperr.Internal(err)
	}
	if productID != "" && lic.ProductID != productID {
		return apperr.New(404, "LICENSE_NOT_FOUND", "license not found")
	}
	if err := s.store.CheckInFloating(ctx, lic.ID, identifier); err != nil {
		return apperr.Internal(err)
	}
	s.logger.Info("floating checkin", "license_id", lic.ID, "identifier", identifier)
	return nil
}

func (s *FloatingService) Heartbeat(ctx context.Context, licenseKey, identifier, productID string) (*CheckOutResult, error) {
	lic, err := s.store.FindLicenseByKey(ctx, licenseKey)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, apperr.New(404, "LICENSE_NOT_FOUND", "license not found")
		}
		return nil, apperr.Internal(err)
	}
	if productID != "" && lic.ProductID != productID {
		return nil, apperr.New(404, "LICENSE_NOT_FOUND", "license not found")
	}

	timeout := 30
	maxSessions := 3
	if lic.Plan != nil {
		if lic.Plan.FloatingTimeout > 0 {
			timeout = lic.Plan.FloatingTimeout
		}
		maxSessions = lic.Plan.MaxActivations
	}

	newExpiry := time.Now().Add(time.Duration(timeout) * time.Minute)
	if err := s.store.HeartbeatFloating(ctx, lic.ID, identifier, newExpiry); err != nil {
		return nil, apperr.NotFound("SESSION", identifier)
	}

	active, _ := s.store.CountActiveFloating(ctx, lic.ID)
	return &CheckOutResult{ExpiresAt: newExpiry, Active: active, Max: maxSessions}, nil
}

// CleanExpired removes expired floating sessions. Called periodically.
func (s *FloatingService) CleanExpired(ctx context.Context) {
	n, err := s.store.CleanExpiredFloating(ctx)
	if err != nil {
		s.logger.Error("floating cleanup failed", "error", err)
		return
	}
	if n > 0 {
		s.logger.Info("floating sessions cleaned", "count", n)
	}
}

// StartCleanupLoop periodically cleans expired sessions.
func (s *FloatingService) StartCleanupLoop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.CleanExpired(ctx)
		}
	}
}
