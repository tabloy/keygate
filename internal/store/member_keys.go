package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"

	"github.com/tabloy/keygate/internal/license"
	"github.com/tabloy/keygate/internal/model"
	"github.com/uptrace/bun"
)

// ErrKeyRevoked is returned by ActivateForKey and CheckOutFloatingForKey
// when the key no longer opens the licence the way it did when it was
// looked up: its member was removed or their accept rolled back, the
// licence's key was changed, or the product no longer has seats for a
// member's key to work.
var ErrKeyRevoked = errors.New("license key no longer valid")

// ErrSeatNotJoined is returned by RotateMemberKey for a seat that is
// an invitation not yet accepted, or removed: it has no key.
var ErrSeatNotJoined = errors.New("seat is not a member who has joined")

// memberKeyAAD binds a member key's ciphertext to its seat. The prefix
// keeps it apart from a licence key's, which is bound to the licence ID.
func memberKeyAAD(seatID string) []byte {
	return []byte("seat:" + seatID)
}

// MemberKey returns the member's own key, making it the first time it
// is asked for: a member who has accepted and not been removed always
// has one, anyone else gets "". Making it here rather than when the
// invite is accepted gives the members who joined before keys of their
// own existed one as well, the first time they open the portal.
func (s *Store) MemberKey(ctx context.Context, seat *model.Seat) (string, error) {
	if seat.MemberKeyHash != "" {
		return s.decryptMemberKey(seat), nil
	}
	if seat.AcceptedAt == nil || seat.RemovedAt != nil {
		return "", nil
	}
	var prefix string
	if err := s.DB.NewRaw(`SELECT p.license_prefix FROM licenses l JOIN products p ON p.id = l.product_id
		WHERE l.id = ?`, seat.LicenseID).Scan(ctx, &prefix); err != nil {
		return "", fmt.Errorf("license prefix of seat %s: %w", seat.ID, err)
	}
	key := license.GenerateKey(prefix)
	var ct []byte
	if s.LicenseKeyAEAD != nil {
		var err error
		if ct, err = s.LicenseKeyAEAD.Encrypt([]byte(key), memberKeyAAD(seat.ID)); err != nil {
			return "", fmt.Errorf("encrypt member key: %w", err)
		}
	}
	// Only a seat still without a key, still on the team: a concurrent
	// request may have made one first, and a removal clears it for good.
	res, err := s.DB.NewUpdate().Model((*model.Seat)(nil)).
		Set("member_key = ?", key).
		Set("member_key_hash = ?", license.HashKey(key)).
		Set("member_key_encrypted = ?", ct).
		Where("id = ? AND member_key_hash IS NULL AND removed_at IS NULL AND accepted_at IS NOT NULL", seat.ID).
		Exec(ctx)
	if err != nil {
		return "", err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		seat.MemberKey, seat.MemberKeyHash, seat.MemberKeyEncrypted = key, license.HashKey(key), ct
		return key, nil
	}
	fresh := new(model.Seat)
	if err := s.DB.NewSelect().Model(fresh).Where("id = ?", seat.ID).Scan(ctx); err != nil {
		return "", err
	}
	if fresh.MemberKeyHash == "" || fresh.RemovedAt != nil {
		return "", nil
	}
	*seat = *fresh
	return s.decryptMemberKey(seat), nil
}

// DecryptMemberKey is the member's key as stored, "" when they have
// none yet. Unlike MemberKey it never makes one: it is for showing a
// key's hint in a list, where looking must not change anything.
func (s *Store) DecryptMemberKey(seat *model.Seat) string {
	if seat.MemberKeyHash == "" {
		return ""
	}
	return s.decryptMemberKey(seat)
}

// CountDevicesBySeat is how many devices each member's own key has
// activated on a licence, by seat ID.
func (s *Store) CountDevicesBySeat(ctx context.Context, licenseID string) (map[string]int, error) {
	var rows []struct {
		SeatID string `bun:"seat_id"`
		N      int    `bun:"n"`
	}
	if err := s.DB.NewRaw(`SELECT seat_id, count(*) AS n FROM activations
		WHERE license_id = ? AND seat_id IS NOT NULL GROUP BY seat_id`, licenseID).Scan(ctx, &rows); err != nil {
		return nil, err
	}
	out := make(map[string]int, len(rows))
	for _, r := range rows {
		out[r.SeatID] = r.N
	}
	return out, nil
}

// decryptMemberKey reads a member key the way DecryptLicenseKey reads a
// licence key: the ciphertext when it can, else the plain column.
func (s *Store) decryptMemberKey(seat *model.Seat) string {
	if s.LicenseKeyAEAD != nil && len(seat.MemberKeyEncrypted) > 0 {
		pt, err := s.LicenseKeyAEAD.Decrypt(seat.MemberKeyEncrypted, memberKeyAAD(seat.ID))
		if err == nil {
			return string(pt)
		}
		slog.Warn("member key decrypt failed; falling back to plaintext column",
			"seat_id", seat.ID, "error", err)
		licenseKeyDecryptFailuresInc()
	}
	return seat.MemberKey
}

// backfillMemberKeysEncrypted encrypts the member keys made before
// license key encryption was turned on, as BackfillLicenseKeyEncrypted
// does licence keys, which it is run with. Only live seats hold a key:
// a removal clears it. Each update holds only while the key is the one
// encrypted, so a removal racing it wins. It returns how many were
// encrypted and how many failed.
func (s *Store) backfillMemberKeysEncrypted(ctx context.Context, logger *slog.Logger) (done, failed int, err error) {
	const batchSize = 100
	// Seats that fail stay unencrypted; skip past them instead of
	// selecting them again forever.
	after := ""
	for {
		if err := ctx.Err(); err != nil {
			return done, failed, err
		}
		var batch []*model.Seat
		if err := s.DB.NewSelect().Model(&batch).
			Where(unencryptedMemberKeys+" AND id > ?", after).
			OrderExpr("id").
			Limit(batchSize).
			Scan(ctx); err != nil {
			return done, failed, fmt.Errorf("select member key batch: %w", err)
		}
		for _, seat := range batch {
			after = seat.ID
			ct, err := s.LicenseKeyAEAD.Encrypt([]byte(seat.MemberKey), memberKeyAAD(seat.ID))
			if err != nil {
				logger.Warn("member key backfill: encrypt failed; skipping", "seat_id", seat.ID, "error", err)
				failed++
				continue
			}
			res, err := s.DB.NewUpdate().Model((*model.Seat)(nil)).
				Set("member_key_encrypted = ?", ct).
				Where("id = ? AND member_key = ? AND member_key_encrypted IS NULL", seat.ID, seat.MemberKey).
				Exec(ctx)
			if err != nil {
				logger.Warn("member key backfill: update failed; skipping", "seat_id", seat.ID, "error", err)
				failed++
				continue
			}
			if n, _ := res.RowsAffected(); n > 0 {
				done++
			}
		}
		if len(batch) < batchSize {
			return done, failed, nil
		}
	}
}

// unencryptedMemberKeys selects the seats whose key has no ciphertext.
const unencryptedMemberKeys = "member_key IS NOT NULL AND member_key <> '' AND member_key_encrypted IS NULL"

// lockLicenseIn takes the licence row lock that activations serialise
// on (see lockLicenseLimitsIn), so a key cleared under it cannot gain a
// device afterwards.
func lockLicenseIn(ctx context.Context, tx bun.Tx, licenseID string) error {
	var id string
	err := tx.NewRaw("SELECT id FROM licenses WHERE id = ? FOR UPDATE", licenseID).Scan(ctx, &id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	return err
}

// seatLimitSQL is a licence's seat limit, the owner included, as
// Plan.Limits reads it: the seats bought on a plan sold per seat, else
// the plan's max_seats; 0 is no limit. l is the licence, pl its plan.
const seatLimitSQL = `(CASE WHEN pl.per_seat THEN GREATEST(l.quantity, 1) ELSE pl.max_seats END)`

// seatPausedSQL is true for a joined seat s of licence l (plan pl) whose
// member is beyond the seat limit: the owner holds one place, and the
// members who joined first hold the rest, so those who joined last wait
// until a place is free. Nothing is stored: removing a member or adding
// seats lets the next in line back at once. A seat at the owner's own
// address is the owner, never paused, and counts no place of its own.
const seatPausedSQL = seatLimitSQL + ` > 0 AND lower(s.email) <> lower(l.email)
	AND (SELECT count(*) FROM seats o WHERE o.license_id = s.license_id AND o.removed_at IS NULL
		AND o.accepted_at IS NOT NULL AND lower(o.email) <> lower(l.email)
		AND (o.accepted_at, o.id) < (s.accepted_at, s.id)) >= ` + seatLimitSQL + ` - 1`

// SeatPaused reports whether the member of a joined seat is paused (see
// seatPausedSQL).
func (s *Store) SeatPaused(ctx context.Context, seatID string) (bool, error) {
	var paused bool
	err := s.DB.NewRaw(`SELECT COALESCE((SELECT `+seatPausedSQL+` FROM seats s
		JOIN licenses l ON l.id = s.license_id JOIN plans pl ON pl.id = l.plan_id
		WHERE s.id = ? AND s.removed_at IS NULL AND s.accepted_at IS NOT NULL), false)`, seatID).Scan(ctx, &paused)
	return paused, err
}

// PausedSeats returns the joined seats of a licence whose members are
// paused, by seat ID: the rule of seatPausedSQL, ranked in one pass
// rather than counted again for each seat, as a list of a large licence
// would. A member's place in the join order is n, and the owner holds
// the first seat, so those from the limit on wait.
func (s *Store) PausedSeats(ctx context.Context, licenseID string) (map[string]bool, error) {
	var ids []string
	if err := s.DB.NewRaw(`SELECT id FROM (
		SELECT s.id, `+seatLimitSQL+` AS lim,
			row_number() OVER (ORDER BY s.accepted_at, s.id) AS n
		FROM seats s JOIN licenses l ON l.id = s.license_id JOIN plans pl ON pl.id = l.plan_id
		WHERE s.license_id = ? AND s.removed_at IS NULL AND s.accepted_at IS NOT NULL
			AND lower(s.email) <> lower(l.email)) ranked
		WHERE lim > 0 AND n >= lim`,
		licenseID).Scan(ctx, &ids); err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out, nil
}

// MarkPausedSeats sets Seat.Paused on seats of one licence.
func (s *Store) MarkPausedSeats(ctx context.Context, licenseID string, seats []*model.Seat) error {
	if len(seats) == 0 {
		return nil
	}
	paused, err := s.PausedSeats(ctx, licenseID)
	if err != nil {
		return err
	}
	for _, seat := range seats {
		seat.Paused = paused[seat.ID]
	}
	return nil
}

// keyHeldIn reports whether key still opens the licence the way the
// caller found it: as the member's key when seatID is set (while the
// product has seats, as FindLicenseByKey holds it), else as the
// licence's own. Nor once the licence is stopped (see stoppedStatuses),
// or canceled with its expiry passed, as the licence checks refuse it: the
// caller read it usable before taking the licence lock, and a revoke,
// suspend, expiry or end of subscription that committed in between must
// not be followed by a new device or session on it.
// canceledEndedSQL is a canceled licence whose expiry has passed, or that
// has none: no longer usable, as the licence checks have it, though
// canceled is not among stoppedStatuses (one may work while an expiry an
// operator set lies ahead). col prefixes the licence's columns: "l." for
// a query that names the licence l, "" for one that reads it alone.
func canceledEndedSQL(col string) string {
	return col + `status = 'canceled' AND (` + col + `valid_until IS NULL OR ` + col + `valid_until <= now())`
}

func keyHeldIn(ctx context.Context, tx bun.Tx, licenseID, seatID, key string) (bool, error) {
	var held bool
	var err error
	stopped := bun.In(stoppedStatuses)
	if seatID != "" {
		// Not while the member is paused (over the seat limit): read
		// under the licence lock, so a seat change cannot slip between.
		err = tx.NewRaw(`SELECT EXISTS (SELECT 1 FROM seats s
			JOIN licenses l ON l.id = s.license_id JOIN products p ON p.id = l.product_id
			JOIN plans pl ON pl.id = l.plan_id
			WHERE s.id = ? AND s.license_id = ? AND s.member_key_hash = ? AND s.removed_at IS NULL
			AND s.accepted_at IS NOT NULL AND p.type IN (?) AND l.status NOT IN (?) AND NOT (`+canceledEndedSQL("l.")+`)
			AND NOT (`+seatPausedSQL+`))`,
			seatID, licenseID, license.HashKey(key), bun.In(model.ProductTypesSupporting(model.CapSeats)), stopped).Scan(ctx, &held)
	} else {
		err = tx.NewRaw(`SELECT EXISTS (SELECT 1 FROM licenses
			WHERE id = ? AND (key_hash = ? OR license_key = ?) AND status NOT IN (?) AND NOT (`+canceledEndedSQL("")+`))`,
			licenseID, license.HashKey(key), key, stopped).Scan(ctx, &held)
	}
	return held, err
}

// RotateLicenseKey gives the licence a new key. The old one stops
// working at once, and the devices it activated and the floating
// sessions it took are removed, since they can no longer check in; the
// members' own, on their own keys, stay. It returns the new key and the
// devices removed. Only an admin reissues a key: a licence's key is
// fixed once issued, as far as its customer is concerned.
func (s *Store) RotateLicenseKey(ctx context.Context, licenseID string) (string, []*model.Activation, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", nil, err
	}
	defer tx.Rollback() //nolint:errcheck

	var prefix string
	if err := tx.NewRaw(`SELECT p.license_prefix FROM licenses l JOIN products p ON p.id = l.product_id
		WHERE l.id = ? FOR UPDATE OF l`, licenseID).Scan(ctx, &prefix); err != nil {
		return "", nil, err
	}
	key := license.GenerateKey(prefix)
	var ct []byte
	if s.LicenseKeyAEAD != nil {
		if ct, err = s.LicenseKeyAEAD.Encrypt([]byte(key), []byte(licenseID)); err != nil {
			return "", nil, fmt.Errorf("encrypt license key: %w", err)
		}
	}
	if _, err := tx.NewUpdate().Model((*model.License)(nil)).
		Set("license_key = ?", key).
		Set("key_hash = ?", license.HashKey(key)).
		Set("license_key_encrypted = ?", ct).
		Set("updated_at = now()").
		Where("id = ?", licenseID).
		Exec(ctx); err != nil {
		return "", nil, err
	}
	var removed []*model.Activation
	if err := tx.NewRaw("DELETE FROM activations WHERE license_id = ? AND seat_id IS NULL RETURNING *",
		licenseID).Scan(ctx, &removed); err != nil {
		return "", nil, err
	}
	if _, err := tx.NewRaw("DELETE FROM floating_sessions WHERE license_id = ? AND seat_id IS NULL",
		licenseID).Exec(ctx); err != nil {
		return "", nil, err
	}
	return key, removed, tx.Commit()
}

// RotateMemberKey gives a member a new key of their own, as
// RotateLicenseKey does the licence's: the old one stops working at
// once, and the devices it activated and the floating sessions it took
// are removed; everyone else's keys and devices stay. It runs under the
// licence row lock that activations decide under, so an activation with
// the old key cannot land after it. It returns the new key and the
// devices removed, ErrSeatNotJoined when the seat has no key to replace,
// and sql.ErrNoRows when there is no such seat.
func (s *Store) RotateMemberKey(ctx context.Context, seatID string) (string, []*model.Activation, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", nil, err
	}
	defer tx.Rollback() //nolint:errcheck

	// The licence first, as RemoveSeat takes it, then the seat as it is
	// under that lock.
	var licenseID string
	if err := tx.NewRaw("SELECT license_id FROM seats WHERE id = ?", seatID).Scan(ctx, &licenseID); err != nil {
		return "", nil, err
	}
	var prefix string
	if err := tx.NewRaw(`SELECT p.license_prefix FROM licenses l JOIN products p ON p.id = l.product_id
		WHERE l.id = ? FOR UPDATE OF l`, licenseID).Scan(ctx, &prefix); err != nil {
		return "", nil, err
	}
	seat := new(model.Seat)
	if err := tx.NewSelect().Model(seat).Where("id = ?", seatID).For("UPDATE").Scan(ctx); err != nil {
		return "", nil, err
	}
	if seat.AcceptedAt == nil || seat.RemovedAt != nil {
		return "", nil, ErrSeatNotJoined
	}

	key := license.GenerateKey(prefix)
	var ct []byte
	if s.LicenseKeyAEAD != nil {
		if ct, err = s.LicenseKeyAEAD.Encrypt([]byte(key), memberKeyAAD(seatID)); err != nil {
			return "", nil, fmt.Errorf("encrypt member key: %w", err)
		}
	}
	if _, err := tx.NewUpdate().Model((*model.Seat)(nil)).
		Set("member_key = ?", key).
		Set("member_key_hash = ?", license.HashKey(key)).
		Set("member_key_encrypted = ?", ct).
		Where("id = ?", seatID).
		Exec(ctx); err != nil {
		return "", nil, err
	}
	var removed []*model.Activation
	if err := tx.NewRaw("DELETE FROM activations WHERE seat_id = ? RETURNING *", seatID).Scan(ctx, &removed); err != nil {
		return "", nil, err
	}
	if _, err := tx.NewRaw("DELETE FROM floating_sessions WHERE seat_id = ?", seatID).Exec(ctx); err != nil {
		return "", nil, err
	}
	return key, removed, tx.Commit()
}

// SeatLeftover is a removed seat that still holds what a removal clears.
type SeatLeftover struct {
	ID        string `bun:"id"`
	LicenseID string `bun:"license_id"`
	ProductID string `bun:"product_id"`
	Email     string `bun:"email"`
}

// FindSeatLeftovers returns up to limit seats not on the team, removed
// or invited and not yet accepted, that still hold a key, devices or
// floating sessions. This version never leaves any on such a seat
// (RemoveSeat clears them, and only an accepted member gets a key), so
// only what an older version left during a rolling upgrade is found: a
// member it removed (it set removed_at alone), or removed and invited
// back.
func (s *Store) FindSeatLeftovers(ctx context.Context, limit int) ([]SeatLeftover, error) {
	var out []SeatLeftover
	err := s.DB.NewRaw(`SELECT s.id, s.license_id, l.product_id, s.email FROM seats s
		JOIN licenses l ON l.id = s.license_id
		WHERE (s.removed_at IS NOT NULL OR s.accepted_at IS NULL) AND (s.member_key_hash IS NOT NULL
			OR EXISTS (SELECT 1 FROM activations a WHERE a.seat_id = s.id)
			OR EXISTS (SELECT 1 FROM floating_sessions f WHERE f.seat_id = s.id))
		ORDER BY s.created_at LIMIT ?`, limit).Scan(ctx, &out)
	return out, err
}

// ClearSeatLeftovers finishes a removal an older version left undone: the
// seat's key is cleared and its devices and floating sessions removed,
// under the licence row lock as RemoveSeat does. Only while the seat is
// still not on the team (removed, or pending): one accepted meanwhile is
// left alone. It returns the devices removed, and whether anything was
// done.
func (s *Store) ClearSeatLeftovers(ctx context.Context, seatID string) ([]*model.Activation, bool, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback() //nolint:errcheck

	var licenseID string
	if err := tx.NewRaw("SELECT license_id FROM seats WHERE id = ?", seatID).Scan(ctx, &licenseID); err != nil {
		return nil, false, err
	}
	if err := lockLicenseIn(ctx, tx, licenseID); err != nil {
		return nil, false, err
	}
	var state struct {
		OffTeam bool `bun:"off_team"`
		HasKey  bool `bun:"has_key"`
	}
	if err := tx.NewRaw(`SELECT removed_at IS NOT NULL OR accepted_at IS NULL AS off_team, member_key_hash IS NOT NULL AS has_key
		FROM seats WHERE id = ? FOR UPDATE`, seatID).Scan(ctx, &state); err != nil {
		return nil, false, err
	}
	if !state.OffTeam {
		return nil, false, nil
	}
	if _, err := tx.NewUpdate().Model((*model.Seat)(nil)).
		Set("member_key = NULL").
		Set("member_key_hash = NULL").
		Set("member_key_encrypted = NULL").
		Where("id = ?", seatID).Exec(ctx); err != nil {
		return nil, false, err
	}
	var devices []*model.Activation
	if err := tx.NewRaw("DELETE FROM activations WHERE seat_id = ? RETURNING *", seatID).Scan(ctx, &devices); err != nil {
		return nil, false, err
	}
	res, err := tx.NewRaw("DELETE FROM floating_sessions WHERE seat_id = ?", seatID).Exec(ctx)
	if err != nil {
		return nil, false, err
	}
	sessions, _ := res.RowsAffected()
	// Done only when something was left: a second sweep (another
	// replica's, say) that finds it cleared reports nothing.
	return devices, state.HasKey || len(devices) > 0 || sessions > 0, tx.Commit()
}

// NormalizeSeatRoles turns any seat with the admin role into a member:
// the role is gone (20261028_member_keys), but an older version still
// running during a rolling upgrade may have written one since. It
// returns how many it changed.
func (s *Store) NormalizeSeatRoles(ctx context.Context) (int, error) {
	res, err := s.DB.NewRaw("UPDATE seats SET role = 'member' WHERE role <> 'member'").Exec(ctx)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// FillMemberEmails sets MemberEmail on the devices members' keys
// activated, so a list of devices says whose each is.
func (s *Store) FillMemberEmails(ctx context.Context, rows []*model.Activation) error {
	var ids []string
	for _, a := range rows {
		if a.SeatID != "" {
			ids = append(ids, a.SeatID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	var seats []struct {
		ID    string `bun:"id"`
		Email string `bun:"email"`
	}
	if err := s.DB.NewRaw("SELECT id, email FROM seats WHERE id IN (?)", bun.In(ids)).Scan(ctx, &seats); err != nil {
		return err
	}
	emails := make(map[string]string, len(seats))
	for _, st := range seats {
		emails[st.ID] = st.Email
	}
	for _, a := range rows {
		a.MemberEmail = emails[a.SeatID]
	}
	return nil
}
