package store

import (
	"context"
	"errors"
	"time"

	"github.com/tabloy/keygate/internal/model"
)

// ErrFloatingLimitReached is returned when all floating sessions are in use.
var ErrFloatingLimitReached = errors.New("floating session limit reached")

func (s *Store) CheckOutFloating(ctx context.Context, sess *model.FloatingSession) error {
	if sess.ID == "" {
		sess.ID = newID()
	}
	_, err := s.DB.NewInsert().Model(sess).
		On("CONFLICT (license_id, identifier) DO UPDATE").
		Set("heartbeat = now(), expires_at = EXCLUDED.expires_at, ip_address = EXCLUDED.ip_address, label = EXCLUDED.label").
		Exec(ctx)
	return err
}

// CheckOutFloatingWithLimit atomically creates a floating session only if the active session
// count is below maxSessions. Uses SELECT FOR UPDATE to prevent race conditions.
// Returns the created/refreshed session and whether it was a new checkout.
func (s *Store) CheckOutFloatingWithLimit(ctx context.Context, sess *model.FloatingSession, maxSessions int) (isNew bool, err error) {
	if sess.ID == "" {
		sess.ID = newID()
	}

	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	// Check if this identifier already has an active session
	var existingID string
	scanErr := tx.NewRaw(`
		SELECT id FROM floating_sessions
		WHERE license_id = ? AND identifier = ? AND expires_at > now()
		FOR UPDATE
	`, sess.LicenseID, sess.Identifier).Scan(ctx, &existingID)

	if scanErr == nil && existingID != "" {
		// Refresh existing session
		_, err = tx.NewRaw(`
			UPDATE floating_sessions SET heartbeat = now(), expires_at = ?, ip_address = ?, label = ?
			WHERE id = ?
		`, sess.ExpiresAt, sess.IPAddress, sess.Label, existingID).Exec(ctx)
		if err != nil {
			return false, err
		}
		sess.ID = existingID
		return false, tx.Commit()
	}

	// Lock the license row to serialize concurrent checkouts for the same license.
	// FOR UPDATE on floating_sessions would lock nothing if no sessions exist,
	// allowing two concurrent checkouts to both succeed and exceed maxSessions.
	_, err = tx.NewRaw(`
		SELECT id FROM licenses WHERE id = ? FOR UPDATE
	`, sess.LicenseID).Exec(ctx)
	if err != nil {
		return false, err
	}

	// Now safely count active sessions (no other checkout can modify them while we hold the lock)
	var activeCount int
	err = tx.NewRaw(`
		SELECT COUNT(*) FROM floating_sessions
		WHERE license_id = ? AND expires_at > now()
	`, sess.LicenseID).Scan(ctx, &activeCount)
	if err != nil {
		return false, err
	}

	if activeCount >= maxSessions {
		return false, ErrFloatingLimitReached
	}

	// Create new session
	_, err = tx.NewInsert().Model(sess).Exec(ctx)
	if err != nil {
		return false, err
	}

	return true, tx.Commit()
}

func (s *Store) CheckInFloating(ctx context.Context, licenseID, identifier string) error {
	_, err := s.DB.NewDelete().Model((*model.FloatingSession)(nil)).
		Where("license_id = ? AND identifier = ?", licenseID, identifier).Exec(ctx)
	return err
}

func (s *Store) HeartbeatFloating(ctx context.Context, licenseID, identifier string, newExpiry time.Time) error {
	_, err := s.DB.NewUpdate().Model((*model.FloatingSession)(nil)).
		Set("heartbeat = now(), expires_at = ?", newExpiry).
		Where("license_id = ? AND identifier = ?", licenseID, identifier).Exec(ctx)
	return err
}

func (s *Store) CountActiveFloating(ctx context.Context, licenseID string) (int, error) {
	return s.DB.NewSelect().Model((*model.FloatingSession)(nil)).
		Where("license_id = ? AND expires_at > now()", licenseID).Count(ctx)
}

func (s *Store) FindFloatingSession(ctx context.Context, licenseID, identifier string) (*model.FloatingSession, error) {
	sess := new(model.FloatingSession)
	return sess, s.DB.NewSelect().Model(sess).
		Where("license_id = ? AND identifier = ? AND expires_at > now()", licenseID, identifier).Scan(ctx)
}

func (s *Store) ListFloatingSessions(ctx context.Context, licenseID string) ([]*model.FloatingSession, error) {
	var out []*model.FloatingSession
	err := s.DB.NewSelect().Model(&out).
		Where("license_id = ? AND expires_at > now()", licenseID).
		OrderExpr("checked_out ASC").Scan(ctx)
	return out, err
}

func (s *Store) CleanExpiredFloating(ctx context.Context) (int, error) {
	res, err := s.DB.NewDelete().Model((*model.FloatingSession)(nil)).
		Where("expires_at <= now()").Exec(ctx)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}
