package store

import (
	"context"
	"database/sql/driver"
	"time"

	"github.com/tabloy/keygate/internal/model"
)

// meteredSyncLockKey names the advisory lock one metered sync pass
// holds (see TryMeteredSyncLock), apart from the other advisory locks.
const meteredSyncLockKey int64 = 0x6b676d657465 // "kgmete"

// TryMeteredSyncLock takes the metered sync lock if no other pass holds
// it, on any replica. Every replica runs the sync loop, and the rows a
// pass reads are not claimed: two passes at once pushed each row twice
// and counted two attempts for it, so a short Stripe outage spent a
// row's retries at twice the rate. ok is false when another pass is
// running; release must be called once the pass is done otherwise.
//
// The lock is held by a connection of its own, for the pass, and goes
// with it if the process dies.
func (s *Store) TryMeteredSyncLock(ctx context.Context) (release func(), ok bool, err error) {
	conn, err := s.DB.DB.Conn(ctx)
	if err != nil {
		return nil, false, err
	}
	if err := conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1)", meteredSyncLockKey).Scan(&ok); err != nil {
		// The lock may have been taken and the answer lost: the
		// connection is discarded, not pooled, which releases it.
		_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		_ = conn.Close()
		return nil, false, err
	}
	if !ok {
		_ = conn.Close()
		return nil, false, nil
	}
	return func() {
		// A fresh context: the pass's may be done, and a lock left on a
		// pooled connection would hold every later pass off.
		unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := conn.ExecContext(unlockCtx, "SELECT pg_advisory_unlock($1)", meteredSyncLockKey); err != nil {
			// Unsure whether it was released: the connection is
			// discarded rather than returned to the pool, which
			// releases it.
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
		_ = conn.Close()
	}, true, nil
}

// InsertMeteredEvent appends an event-log row to be pushed to Stripe
// by MeteredBillingSync. quantity is the DELTA from this single
// usage call (not the cumulative total).
//
// The identifier is the row's ID — opaque to callers, used as the
// Stripe meter event's identifier so retries dedupe within Stripe's
// rolling 24h window.
//
// eventName and customerID are the Stripe meter and customer the usage
// is recorded against. The row is sent there, whatever the entitlement
// or the license say by the time it syncs.
func (s *Store) InsertMeteredEvent(ctx context.Context, licenseID, feature, periodKey, eventName, customerID string, quantity int64) error {
	id := newID()
	m := &model.MeteredBilling{
		ID:                   id,
		LicenseID:            licenseID,
		Feature:              feature,
		Quantity:             quantity,
		PeriodKey:            periodKey,
		Identifier:           id,
		StripeMeterEventName: eventName,
		StripeCustomerID:     customerID,
	}
	_, err := s.DB.NewInsert().Model(m).Exec(ctx)
	return err
}

// ListUnsyncedMetered returns pending rows that still have attempts
// left. Rows at the ceiling are excluded here, not just skipped by the
// caller: they are never going to sync, and with created_at ordering
// enough of them at the head of the queue would fill every batch and
// starve rows that could.
func (s *Store) ListUnsyncedMetered(ctx context.Context, limit, maxAttempts int) ([]*model.MeteredBilling, error) {
	return s.ListUnsyncedMeteredAfter(ctx, nil, limit, maxAttempts)
}

// ListUnsyncedMeteredAfter is ListUnsyncedMetered from after the row
// last (nil: from the start), in (created_at, id) order: a pass sending
// the whole queue in batches goes on past rows it tried, whether they
// were sent or failed, rather than meeting a failed one again at the
// head of the queue and spending its retries at once.
func (s *Store) ListUnsyncedMeteredAfter(ctx context.Context, last *model.MeteredBilling, limit, maxAttempts int) ([]*model.MeteredBilling, error) {
	var out []*model.MeteredBilling
	q := s.DB.NewSelect().Model(&out).
		Where("synced = false AND attempts < ?", maxAttempts)
	if last != nil {
		q = q.Where("(created_at, id) > (?, ?)", last.CreatedAt, last.ID)
	}
	err := q.OrderExpr("created_at ASC, id ASC").Limit(limit).Scan(ctx)
	return out, err
}

func (s *Store) MarkMeteredSynced(ctx context.Context, id, externalID string) error {
	now := time.Now()
	_, err := s.DB.NewUpdate().Model((*model.MeteredBilling)(nil)).
		Set("synced = true, synced_at = ?, external_id = ?, last_error = ''", now, externalID).
		Where("id = ?", id).Exec(ctx)
	return err
}

// RecordMeteredSyncFailure stamps the row with the failure reason
// and bumps the attempts counter. We DON'T flip synced=true — the
// next sync pass tries again. last_error is human-readable and
// surfaces in the admin UI so operators can investigate without
// log-diving.
func (s *Store) RecordMeteredSyncFailure(ctx context.Context, id, reason string) error {
	const maxLastErr = 512
	if len(reason) > maxLastErr {
		reason = reason[:maxLastErr]
	}
	_, err := s.DB.NewUpdate().Model((*model.MeteredBilling)(nil)).
		Set("attempts = attempts + 1, last_error = ?", reason).
		Where("id = ?", id).Exec(ctx)
	return err
}

// MarkMeteredAttempted bumps attempts without changing synced state.
// Called on every push regardless of outcome so retry budgets are
// visible. Successful pushes also call MarkMeteredSynced which
// already clears last_error.
func (s *Store) MarkMeteredAttempted(ctx context.Context, id string) error {
	_, err := s.DB.NewUpdate().Model((*model.MeteredBilling)(nil)).
		Set("attempts = attempts + 1").
		Where("id = ?", id).Exec(ctx)
	return err
}

func (s *Store) ListMeteredBilling(ctx context.Context, licenseID string) ([]*model.MeteredBilling, error) {
	var out []*model.MeteredBilling
	err := s.DB.NewSelect().Model(&out).
		Where("license_id = ?", licenseID).OrderExpr("created_at DESC").Scan(ctx)
	return out, err
}
