package service

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
	"github.com/tabloy/keygate/pkg/apperr"
)

type FloatingService struct {
	store    *store.Store
	logger   *slog.Logger
	failures FailureTracker
}

func NewFloatingService(s *store.Store, logger *slog.Logger, failures FailureTracker) *FloatingService {
	return &FloatingService{store: s, logger: logger, failures: failures}
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
	// Public SDK endpoint: collapse every "can't help you" condition
	// (missing key, wrong product, non-activation product type,
	// non-floating plan, suspended/revoked/expired) to one 404. Each
	// of these would otherwise be an oracle for license existence or
	// configuration; the legitimate floating client never hits them.
	//
	// Checkout is how a floating licence starts, as activate is for a
	// device one (which refuses floating plans), so each of these 404s
	// counts against the caller's address the way verify's do.
	lic, err := loadLicenseForSDK(ctx, s.store, in.LicenseKey, in.ProductID, model.CapActivations, true)
	if err == nil && (lic.Plan == nil || lic.Plan.LicenseModel != "floating") {
		err = licenseNotFound()
	}
	if err != nil {
		var ae *apperr.AppError
		if errors.As(err, &ae) && ae.Code == "LICENSE_NOT_FOUND" {
			if locked := recordAddressFailure(s.failures, in.IPAddress); locked != nil {
				return nil, locked
			}
		}
		return nil, err
	}

	timeout := lic.Plan.FloatingTimeout
	if timeout <= 0 {
		timeout = 30
	}
	// Floating sessions are counted against the device limit: per seat
	// on a plan sold that way.
	_, maxSessions := lic.Limits()

	expiresAt := time.Now().Add(time.Duration(timeout) * time.Minute)
	sess := &model.FloatingSession{
		LicenseID: lic.ID, Identifier: in.Identifier,
		Label: in.Label, IPAddress: in.IPAddress, ExpiresAt: expiresAt,
		SeatID: keySeatID(lic),
	}

	// Atomically check capacity and create/refresh session using DB-level locking
	// This prevents race conditions where concurrent checkouts could exceed maxSessions
	isNew, err := s.store.CheckOutFloatingForKey(ctx, sess, maxSessions, in.LicenseKey)
	if err != nil {
		// Removed or replaced since it was looked up: a key that no
		// longer exists.
		if errors.Is(err, store.ErrKeyRevoked) {
			return nil, licenseNotFound()
		}
		if errors.Is(err, store.ErrSessionHeld) {
			return nil, apperr.Conflict("SESSION_HELD", "this machine's session was checked out with another key on this license")
		}
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
	lic, err := loadLicenseForSDK(ctx, s.store, licenseKey, productID, model.CapActivations, true)
	if err != nil {
		return err
	}
	// A member's key ends only that member's session.
	if err := s.store.CheckInFloating(ctx, lic.ID, identifier, keySeatID(lic)); err != nil {
		return apperr.Internal(err)
	}
	s.logger.Info("floating checkin", "license_id", lic.ID, "identifier", identifier)
	return nil
}

func (s *FloatingService) Heartbeat(ctx context.Context, licenseKey, identifier, productID string) (*CheckOutResult, error) {
	lic, err := loadLicenseForSDK(ctx, s.store, licenseKey, productID, model.CapActivations, true)
	if err != nil {
		return nil, err
	}
	// As CheckOut: a session renews only on a floating plan. One taken
	// out before the licence moved to a standard plan would otherwise run
	// on beside the devices that plan now counts, a seat on two machines.
	// It ends at its expiry; the machine activates instead.
	if lic.Plan == nil || lic.Plan.LicenseModel != "floating" {
		return nil, licenseNotFound()
	}

	timeout := 30
	if lic.Plan != nil && lic.Plan.FloatingTimeout > 0 {
		timeout = lic.Plan.FloatingTimeout
	}
	_, maxSessions := lic.Limits()

	newExpiry := time.Now().Add(time.Duration(timeout) * time.Minute)
	// A heartbeat that renewed nothing is the answer, not a silent
	// success: either this machine never checked out, or its lease
	// lapsed while it was away and the slot has moved on. Both mean
	// the same thing to the client, which is that it has to check out
	// again before it may carry on.
	// A member's key renews only that member's session.
	renewed, err := s.store.HeartbeatFloating(ctx, lic.ID, identifier, keySeatID(lic), newExpiry)
	if err != nil {
		return nil, apperr.Internal(err)
	}
	if !renewed {
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
