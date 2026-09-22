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

	// Lock the license row first, so everything below runs one caller
	// at a time for this license.
	//
	// The order matters. When the "has this machine already got a
	// session" check ran before the lock, two requests from the SAME
	// machine could both miss it: one took the slot, and the other
	// then counted the slot it had just been given and refused it with
	// FLOATING_LIMIT. A client retrying a timed-out checkout, or an
	// app opened twice, was told the license it already holds is full.
	//
	// Locking the license rather than the sessions is deliberate:
	// FOR UPDATE on floating_sessions locks nothing when there are no
	// rows yet, which would let two first-ever checkouts both pass the
	// count and exceed the limit.
	_, err = tx.NewRaw(`
		SELECT id FROM licenses WHERE id = ? FOR UPDATE
	`, sess.LicenseID).Exec(ctx)
	if err != nil {
		return false, err
	}

	// This machine may already be holding a running lease, in which
	// case it is renewing rather than competing for a slot.
	var existingID string
	scanErr := tx.NewRaw(`
		SELECT id FROM floating_sessions
		WHERE license_id = ? AND identifier = ? AND expires_at > now()
		FOR UPDATE
	`, sess.LicenseID, sess.Identifier).Scan(ctx, &existingID)

	if scanErr == nil && existingID != "" {
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

	// Take the slot.
	//
	// An upsert rather than a plain insert, because this machine may
	// still own a row: the refresh branch above only matches a lease
	// that is still running, so a client that was offline longer than
	// the timeout falls through to here with its lapsed row still on
	// the table. (license_id, identifier) is unique, so inserting over
	// it failed, and a machine that dropped off the network could
	// never check out again until the cleanup sweep happened to
	// remove the row. Reusing it is also the right thing on its own
	// terms: one machine, one row, whose lease starts again now.
	//
	// The id is read back because a conflict keeps the row that is
	// already there, and the caller hands its id to the client as the
	// session id.
	_, err = tx.NewInsert().Model(sess).
		On("CONFLICT (license_id, identifier) DO UPDATE").
		Set("heartbeat = now(), expires_at = EXCLUDED.expires_at, ip_address = EXCLUDED.ip_address, label = EXCLUDED.label").
		Returning("id").
		Exec(ctx, &sess.ID)
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

// HeartbeatFloating extends a session that is still running, and
// reports whether there was one to extend.
//
// The `expires_at > now()` guard is what keeps the concurrency cap
// honest. Without it a client whose lease had already lapsed could
// raise itself from the dead with a heartbeat: the slot it used to
// hold has by then been handed to somebody else by CheckOut, and
// renewing the old row puts the license over its limit with neither
// call ever seeing the other. A machine that drops off the network
// for longer than the timeout has lost its seat, and has to compete
// for one again through CheckOut like any other.
func (s *Store) HeartbeatFloating(ctx context.Context, licenseID, identifier string, newExpiry time.Time) (bool, error) {
	res, err := s.DB.NewUpdate().Model((*model.FloatingSession)(nil)).
		Set("heartbeat = now(), expires_at = ?", newExpiry).
		Where("license_id = ? AND identifier = ? AND expires_at > now()", licenseID, identifier).
		Exec(ctx)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
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

func (s *Store) ListFloatingSessions(ctx context.Context, licenseID string, p Page) ([]*model.FloatingSession, int, error) {
	var out []*model.FloatingSession
	q := s.DB.NewSelect().Model(&out).
		Where("license_id = ? AND expires_at > now()", licenseID).
		OrderExpr("checked_out ASC, id ASC")
	total, err := scanPage(ctx, q, p)
	if err != nil {
		return nil, 0, err
	}
	if p.Limit <= 0 {
		total = len(out)
	}
	return out, total, nil
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
