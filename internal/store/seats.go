package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"hash/fnv"
	"time"

	"github.com/tabloy/keygate/internal/model"
	"github.com/uptrace/bun"
)

// SeatInviteTTL is how long a freshly-minted invite token stays
// claimable. 7 days mirrors the GitHub / Slack convention and is
// long enough that "you got invited last weekend" still works.
const SeatInviteTTL = 7 * 24 * time.Hour

// GenerateSeatInviteToken mints a 32-byte random token. The plain
// value is returned to the caller (email + URL); only the SHA256
// hash is persisted, mirroring the api_keys hashing pattern so a DB
// dump can't be replayed as invite acceptance.
func GenerateSeatInviteToken() (plain, hash string) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		// Cryptographic RNG failure is unrecoverable; surface it
		// as an empty token so the caller falls through to "no
		// invite link" rather than emitting a deterministic value.
		return "", ""
	}
	plain = hex.EncodeToString(b)
	hash = HashSeatInviteToken(plain)
	return
}

// HashSeatInviteToken is the canonical way to compute the lookup
// key from a plain token. Exposed (vs an internal helper) so the
// admin and portal handlers share the same hash function — drift
// here would make valid tokens look unknown.
func HashSeatInviteToken(plain string) string {
	h := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(h[:])
}

// ErrUpgradeCheckoutOpen is returned by AddSeatWithinLimit while a
// checkout upgrading the licence to a plan sold per seat is open: its
// seats were counted from the people on the licence when it opened, so
// nobody joins until it is paid or closed.
var ErrUpgradeCheckoutOpen = errors.New("an upgrade checkout is open")

// ErrUpgradeSeatLimit is returned by AddSeatWithinLimit while a
// checkout upgrading the licence to a plan with a seat limit is open
// and the team already fills that limit: the team was checked against
// it when the checkout opened, so it grows only as far as that limit
// meanwhile. Unlike ErrUpgradeCheckoutOpen, paying does not make room.
var ErrUpgradeSeatLimit = errors.New("the plan being upgraded to has no seat left")

// UpgradeCheckoutTTL is how long a checkout upgrading a licence to a
// plan sold per seat or with a seat limit can be paid (its expires_at);
// an abandoned one holds invites up no longer. upgradeCheckoutOpenFor
// adds a margin for the moments between the upgrade row and the
// checkout being created.
const (
	UpgradeCheckoutTTL     = time.Hour
	upgradeCheckoutOpenFor = UpgradeCheckoutTTL + 5*time.Minute
)

// UpgradeLockKey names the advisory lock StartUpgrade holds per licence
// while it opens a checkout, so two clicks cannot both open one. An
// invite waits for it (see AddSeatWithinLimit): the checkout's seats
// are counted under it, and its upgrade is seen once it is open.
func UpgradeLockKey(licenseID string) int64 {
	hash := fnv.New64a()
	hash.Write([]byte("upgrade:" + licenseID))
	return int64(hash.Sum64())
}

// ErrSeatLimitReached is returned by AddSeatWithinLimit when the
// plan's max_seats cap is already hit. Callers translate this to
// 409 SEAT_LIMIT so the SDK can show a meaningful upsell prompt.
var ErrSeatLimitReached = errors.New("seat limit reached")

func (s *Store) CreateSeat(ctx context.Context, seat *model.Seat) error {
	if seat.ID == "" {
		seat.ID = newID()
	}
	_, err := s.DB.NewInsert().Model(seat).Exec(ctx)
	return err
}

// SeatAddOutcome distinguishes the ways AddSeatWithinLimit can succeed.
// The service layer mails the invite for Created, Restored and Resent,
// and fires seat.added for Created and Restored only: a resent invite
// adds no seat. Existed is an idempotent no-op.
type SeatAddOutcome string

const (
	SeatCreated  SeatAddOutcome = "created"  // brand-new seat row inserted
	SeatRestored SeatAddOutcome = "restored" // soft-deleted row reactivated
	SeatExisted  SeatAddOutcome = "existed"  // already an active seat for this email
	SeatResent   SeatAddOutcome = "resent"   // pending invite renewed and sent again
)

// AddSeatWithinLimit is the atomic version of "count + insert" for
// seats. It also returns the devices a restored seat still had from its
// last time on the team (a seat removed by an older version kept them),
// which it removes, so the caller reports them as deactivated. Without this, two concurrent /seats/add requests can both
// pass the count check and both insert — overshooting max_seats.
//
// The fix mirrors ActivateWithinLimit: take a row-level lock on the
// license inside a tx, then count, then insert. We also handle the
// re-invite case here: if a row with this email already exists but
// is soft-deleted (removed_at IS NOT NULL), restore it instead of
// trying to insert a brand-new row that would collide on the
// partial-unique index.
//
// maxSeats is the plan's max_seats: how many people the license holds,
// the owner included. The owner has no seat row (self-invites are
// refused), so the rows may number one less. maxSeats <= 0 means "no
// limit" (uniform with the rest of the code that reads
// `lic.Plan.MaxSeats > 0` as the cap-active sentinel).
func (s *Store) AddSeatWithinLimit(ctx context.Context, seat *model.Seat, maxSeats int) (SeatAddOutcome, []*model.Activation, error) {
	if seat.ID == "" {
		seat.ID = newID()
	}

	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", nil, err
	}
	defer tx.Rollback() //nolint:errcheck

	// An upgrade checkout being opened counts the people first; the
	// invite waits for it to be open, and is then held up by it.
	if _, err := tx.NewRaw("SELECT pg_advisory_xact_lock_shared(?)", UpgradeLockKey(seat.LicenseID)).Exec(ctx); err != nil {
		return "", nil, err
	}

	// Lock the parent license row so concurrent /seats/add for the
	// same license serialize through this critical section, and hold
	// the invite to the seats the licence has now (see
	// lockLicenseLimitsIn).
	held, _, known, err := lockLicenseLimitsIn(ctx, tx, seat.LicenseID)
	if err != nil {
		return "", nil, err
	}
	if known {
		maxSeats = stricterSeatLimit(maxSeats, held)
	}

	// Existing live seat? Two sub-cases:
	//
	//   - already accepted (accepted_at set): idempotent no-op.
	//     Re-inviting someone who's already on the team does nothing.
	//   - still pending (accepted_at NULL): ROTATE the invite token
	//     + refresh expires_at + return SeatResent so the service
	//     sends a fresh email. Without rotation, the old token
	//     (whose email may have been lost / received but ignored)
	//     stays valid forever AND the new email never goes out
	//     because the service skips on SeatExisted.
	//
	// The row is locked: an accept of the old link commits first or
	// waits, so a seat accepted meanwhile is read as accepted rather
	// than given a new invite on top.
	existing := new(model.Seat)
	err = tx.NewSelect().Model(existing).
		// Matched case-insensitively: seats made before addresses were
		// lower-cased may keep mixed case, and are the same person. An
		// accepted seat first, as FindSeatByEmail picks.
		Where("license_id = ? AND lower(email) = ? AND removed_at IS NULL",
			seat.LicenseID, normalizeEmail(seat.Email)).
		OrderExpr("accepted_at IS NULL, created_at ASC, id ASC").
		Limit(1).
		For("UPDATE").
		Scan(ctx)
	if err == nil {
		if existing.AcceptedAt != nil {
			*seat = *existing
			return SeatExisted, nil, tx.Commit()
		}
		// Pending re-invite (the invite sent again) → rotate token,
		// refresh expiry and invited_at, and take the invite's role
		// (member). user_id / accepted_at remain NULL. invited_at is the
		// database's clock, as the first invite's was (its default): this
		// server's may be behind it, and the invite sent again would
		// read as sent before the first. It is read back, so the seat
		// returned matches the one stored.
		var now time.Time
		if err := tx.NewUpdate().Model((*model.Seat)(nil)).
			Set("role = ?", seat.Role).
			Set("invite_token_hash = ?", seat.InviteTokenHash).
			Set("invite_expires_at = ?", seat.InviteExpiresAt).
			Set("invited_at = now()").
			Where("id = ?", existing.ID).
			Returning("invited_at").
			Scan(ctx, &now); err != nil {
			return "", nil, err
		}
		existing.InvitedAt = now
		existing.Role = seat.Role
		existing.InviteTokenHash = seat.InviteTokenHash
		existing.InviteExpiresAt = seat.InviteExpiresAt
		*seat = *existing
		return SeatResent, nil, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", nil, err
	}

	// Someone joining, not an invite sent again, while an upgrade
	// checkout is open: not at all for a plan sold per seat (see
	// ErrUpgradeCheckoutOpen), and only within the seat limit of any
	// other (see ErrUpgradeSeatLimit).
	// A limit of 0 is none.
	var perSeat sql.NullBool
	var upgradeLimit sql.NullInt64
	if err := tx.NewRaw(`SELECT bool_or(p.per_seat), min(NULLIF(p.max_seats, 0))
		FROM license_upgrades u JOIN plans p ON p.id = u.to_plan_id
		WHERE u.license_id = ? AND u.state = ? AND u.created_at > now() - make_interval(secs => ?)`,
		seat.LicenseID, model.UpgradePending, upgradeCheckoutOpenFor.Seconds()).Scan(ctx, &perSeat, &upgradeLimit); err != nil {
		return "", nil, err
	}
	if perSeat.Bool {
		return "", nil, ErrUpgradeCheckoutOpen
	}

	// Capacity check — the members (see memberSeatsSQL), plus one for
	// the owner, who holds a seat without a row.
	if maxSeats > 0 || upgradeLimit.Int64 > 0 {
		var count int
		if err := tx.NewRaw(memberSeatsSQL, seat.LicenseID).Scan(ctx, &count); err != nil {
			return "", nil, err
		}
		if maxSeats > 0 && count+1 >= maxSeats {
			return "", nil, ErrSeatLimitReached
		}
		if upgradeLimit.Int64 > 0 && int64(count+1) >= upgradeLimit.Int64 {
			return "", nil, ErrUpgradeSeatLimit
		}
	}

	// The caller passes seat.InviteTokenHash + seat.InviteExpiresAt
	// already populated. They flow through Created and Restored
	// identically — the new occupant always needs a fresh claim
	// link, even when the slot is being reused after a removal.

	// Restore-on-reinvite: if a removed row exists with the same
	// email, prefer un-removing it over inserting a duplicate. This
	// preserves user_id binding from the original invite (handy when
	// the SSO mapping has been linked) and keeps the audit trail.
	removed := new(model.Seat)
	err = tx.NewSelect().Model(removed).
		Where("license_id = ? AND lower(email) = ? AND removed_at IS NOT NULL",
			seat.LicenseID, normalizeEmail(seat.Email)).
		OrderExpr("removed_at DESC").
		Limit(1).
		Scan(ctx)
	if err == nil {
		// Reset removed_at, refresh role, and mint a NEW invite
		// token+expiry. The old token (if any) is invalidated by
		// being overwritten — both physically (column hash) and
		// logically (the partial unique index forces the new hash
		// to be the only live one for this email).
		// user_id is cleared so the new occupant goes through the
		// accept flow afresh rather than inheriting whoever held
		// the seat last time. So are the key and the devices of its
		// last time on the team: RemoveSeat clears them, but a seat
		// removed by an older version (during a rolling upgrade) kept
		// them, and coming back must not bring that key back to life.
		var stale []*model.Activation
		if err := tx.NewRaw("DELETE FROM activations WHERE seat_id = ? RETURNING *", removed.ID).Scan(ctx, &stale); err != nil {
			return "", nil, err
		}
		if _, err := tx.NewRaw("DELETE FROM floating_sessions WHERE seat_id = ?", removed.ID).Exec(ctx); err != nil {
			return "", nil, err
		}
		if err := tx.NewUpdate().Model((*model.Seat)(nil)).
			Set("removed_at = NULL").
			Set("user_id = NULL").
			Set("accepted_at = NULL").
			Set("member_key = NULL").
			Set("member_key_hash = NULL").
			Set("member_key_encrypted = NULL").
			Set("role = ?", seat.Role).
			Set("invite_token_hash = ?", seat.InviteTokenHash).
			Set("invite_expires_at = ?", seat.InviteExpiresAt).
			// The database's clock, as for an invite sent again above.
			Set("invited_at = now()").
			Where("id = ?", removed.ID).
			Returning("invited_at").
			Scan(ctx, &removed.InvitedAt); err != nil {
			return "", nil, err
		}
		removed.RemovedAt = nil
		removed.UserID = ""
		removed.MemberKey, removed.MemberKeyHash, removed.MemberKeyEncrypted = "", "", nil
		removed.AcceptedAt = nil
		removed.Role = seat.Role
		removed.InviteTokenHash = seat.InviteTokenHash
		removed.InviteExpiresAt = seat.InviteExpiresAt
		*seat = *removed
		return SeatRestored, stale, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", nil, err
	}

	// Fresh insert.
	if _, err := tx.NewInsert().Model(seat).Exec(ctx); err != nil {
		return "", nil, err
	}
	return SeatCreated, nil, tx.Commit()
}

// AcceptSeatInvite consumes a plain invite token. Atomically:
//
//   - looks up the seat by SHA256(plain)
//   - rejects when removed, expired, or already accepted
//   - looks up or creates a user keyed off the SEAT's email (NOT
//     anything client-supplied — the token is the email-ownership
//     proof)
//   - binds seat.user_id, seat.accepted_at, clears invite_token_hash
//
// Returns the resolved user + seat. ErrSeatInviteInvalid is the
// caller-facing failure for any of: bad token, removed, expired,
// already accepted; collapsing these into one sentinel avoids
// telling an attacker whether a token "was real".
func (s *Store) AcceptSeatInvite(ctx context.Context, plainToken string) (*model.User, *model.Seat, error) {
	if plainToken == "" {
		return nil, nil, ErrSeatInviteInvalid
	}
	hash := HashSeatInviteToken(plainToken)

	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback() //nolint:errcheck

	// FOR UPDATE so a concurrent AddSeatWithinLimit rotation can't
	// silently swap invite_token_hash under us while we're
	// mid-accept. Without the lock, a recipient holding the OLD
	// token could observe the row pre-rotation, then UPDATE after
	// the rotating tx commits — claiming the seat with the old
	// token AND inheriting the new occupant intent. We
	// re-check the hash after locking because the row's hash might
	// have rotated to something else between SELECT and lock
	// acquisition; the seat is only "the invite we were sent" if
	// the hash STILL matches what the token hashes to.
	//
	// The licence first, then the seat, the order every path that
	// takes both keeps: whether a seat is free is decided under the
	// licence lock, as an invite is.
	var licenseID string
	if err := tx.NewRaw("SELECT license_id FROM seats WHERE invite_token_hash = ?", hash).Scan(ctx, &licenseID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, ErrSeatInviteInvalid
		}
		return nil, nil, err
	}
	if err := lockLicenseIn(ctx, tx, licenseID); err != nil {
		return nil, nil, err
	}
	seat := new(model.Seat)
	err = tx.NewSelect().Model(seat).
		Where("invite_token_hash = ? AND license_id = ?", hash, licenseID).
		For("UPDATE").
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, ErrSeatInviteInvalid
		}
		return nil, nil, err
	}
	if seat.InviteTokenHash != hash {
		// Rotated out from under us → the old token is no longer
		// the authoritative invite. Same opaque error as wrong token.
		return nil, nil, ErrSeatInviteInvalid
	}
	if seat.RemovedAt != nil || seat.AcceptedAt != nil {
		return nil, nil, ErrSeatInviteInvalid
	}
	if seat.InviteExpiresAt == nil || seat.InviteExpiresAt.Before(time.Now()) {
		return nil, nil, ErrSeatInviteInvalid
	}
	// Joining needs a free place: with the places all taken (the limit
	// lowered since the invite was sent), the member would only be
	// paused. Refused before anything changes, so the invite stands and
	// works once a seat is free. A seat at the owner's address is the
	// owner, who holds a place already.
	var room struct {
		Limit  int  `bun:"lim"`
		Joined int  `bun:"joined"`
		Owner  bool `bun:"is_owner"`
	}
	if err := tx.NewRaw(`SELECT `+seatLimitSQL+` AS lim,
		(SELECT count(*) FROM seats o WHERE o.license_id = l.id AND o.removed_at IS NULL
			AND o.accepted_at IS NOT NULL AND lower(o.email) <> lower(l.email)) AS joined,
		lower(?) = lower(l.email) AS is_owner
		FROM licenses l JOIN plans pl ON pl.id = l.plan_id WHERE l.id = ?`, seat.Email, licenseID).Scan(ctx, &room); err != nil {
		return nil, nil, err
	}
	if room.Limit > 0 && !room.Owner && room.Joined+1 >= room.Limit {
		return nil, nil, ErrSeatLimitReached
	}

	// Find-or-create user by the SEAT's email (not client input).
	// Case-insensitive lookup: a user who originally registered as
	// "Foo@x.com" (legacy data, before the AddSeat normalization
	// landed) should still be matched when invited as "foo@x.com" —
	// otherwise we'd create a duplicate user and split their
	// identity. AddSeat now lowercases incoming emails, but a seat made
	// before that keeps its capitals, so seat.Email is normalized here.
	user := new(model.User)
	// User rows are stored lower-cased (see normalizeEmail), so a plain
	// comparison is exact and uses users_email_key.
	err = tx.NewSelect().Model(user).Where("email = ?", normalizeEmail(seat.Email)).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		user.ID = newID()
		user.Email = normalizeEmail(seat.Email)
		user.Role = model.RoleUser
		if _, err := tx.NewInsert().Model(user).Exec(ctx); err != nil {
			return nil, nil, err
		}
	} else if err != nil {
		return nil, nil, err
	}

	now := time.Now()
	// A pending seat holds no key; one left from before (an older
	// version removed and re-invited it without clearing) is dropped, so
	// the member gets a new key rather than the one they had before.
	if _, err := tx.NewUpdate().Model((*model.Seat)(nil)).
		Set("user_id = ?", user.ID).
		Set("accepted_at = ?", now).
		Set("invite_token_hash = NULL").
		Set("invite_expires_at = NULL").
		Set("member_key = NULL").
		Set("member_key_hash = NULL").
		Set("member_key_encrypted = NULL").
		Where("id = ?", seat.ID).
		Exec(ctx); err != nil {
		return nil, nil, err
	}
	seat.MemberKey, seat.MemberKeyHash, seat.MemberKeyEncrypted = "", "", nil
	seat.UserID = user.ID
	seat.AcceptedAt = &now
	seat.InviteTokenHash = ""
	seat.InviteExpiresAt = nil

	return user, seat, tx.Commit()
}

// ErrSeatInviteInvalid is the single failure sentinel for accept —
// covers unknown / expired / removed / already-accepted tokens so
// the public endpoint doesn't reveal which condition was hit.
var ErrSeatInviteInvalid = errors.New("seat invite invalid or expired")

// RollbackSeatAccept undoes the AcceptSeatInvite UPDATE when a
// post-accept policy check fails — e.g. the license is suspended.
// We clear user_id + accepted_at so the seat row is recoverable
// (the owner can re-invite the same email and the unique partial
// index won't bite), but we deliberately leave invite_token_hash
// + invite_expires_at NULL: the plaintext token has already been
// processed once, so we don't keep it valid. The customer-facing
// flow is: re-invite from the portal, which mints a fresh token.
//
// A key made in the moment the seat stood accepted is cleared, and
// anything it took with it removed, the way RemoveSeat does and under
// the same licence row lock. It returns the devices removed.
func (s *Store) RollbackSeatAccept(ctx context.Context, seatID string) ([]*model.Activation, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck

	var licenseID string
	if err := tx.NewRaw("SELECT license_id FROM seats WHERE id = ?", seatID).Scan(ctx, &licenseID); err != nil {
		return nil, err
	}
	if err := lockLicenseIn(ctx, tx, licenseID); err != nil {
		return nil, err
	}
	if _, err := tx.NewUpdate().Model((*model.Seat)(nil)).
		Set("user_id = NULL").
		Set("accepted_at = NULL").
		Set("member_key = NULL").
		Set("member_key_hash = NULL").
		Set("member_key_encrypted = NULL").
		Where("id = ?", seatID).
		Exec(ctx); err != nil {
		return nil, err
	}
	var removed []*model.Activation
	if err := tx.NewRaw("DELETE FROM activations WHERE seat_id = ? RETURNING *", seatID).Scan(ctx, &removed); err != nil {
		return nil, err
	}
	if _, err := tx.NewRaw("DELETE FROM floating_sessions WHERE seat_id = ?", seatID).Exec(ctx); err != nil {
		return nil, err
	}
	return removed, tx.Commit()
}

func (s *Store) FindSeatByID(ctx context.Context, id string) (*model.Seat, error) {
	seat := new(model.Seat)
	return seat, s.DB.NewSelect().Model(seat).Where("id = ?", id).Scan(ctx)
}

// FindSeatByEmail is the live seat the address holds on the licence,
// matched case-insensitively, as the portal's licence list matches it:
// seats made before addresses were lower-cased may keep mixed case. An
// accepted seat wins over one still pending, as PortalViewerSeat picks.
func (s *Store) FindSeatByEmail(ctx context.Context, licenseID, email string) (*model.Seat, error) {
	seat := new(model.Seat)
	return seat, s.DB.NewSelect().Model(seat).
		Where("license_id = ? AND lower(email) = ? AND removed_at IS NULL", licenseID, normalizeEmail(email)).
		// An accepted seat first, should a legacy pending one share the
		// address in another case.
		OrderExpr("accepted_at IS NULL, created_at ASC, id ASC").
		Limit(1).
		Scan(ctx)
}

func (s *Store) ListSeats(ctx context.Context, licenseID string, p Page) ([]*model.Seat, int, error) {
	var out []*model.Seat
	q := s.DB.NewSelect().Model(&out).
		Where("license_id = ? AND removed_at IS NULL", licenseID).
		OrderExpr("created_at ASC, id ASC")
	total, err := scanPage(ctx, q, p)
	if err != nil {
		return nil, 0, err
	}
	if p.Limit <= 0 {
		total = len(out)
	}
	return out, total, nil
}

// memberSeatsSQL counts a license's members: its seats that are not
// removed, leaving out one held by the owner's own address. Self-invites
// are refused, but the license email can change afterwards (a Stripe
// customer's new address) to that of an existing member, who would
// otherwise count twice, once as the owner and once as a member.
//
// An invite that expired without being accepted holds nothing: nothing
// ever sweeps those rows away (removed_at stays NULL), so counting them
// here would let a handful of unanswered invites permanently occupy a
// license's seats, refusing new invites the owner has every right to
// send. Only a *pending* invite still within its window, or an already
// accepted seat, counts.
const memberSeatsSQL = `SELECT COUNT(*) FROM seats s JOIN licenses l ON l.id = s.license_id
	WHERE s.license_id = ? AND s.removed_at IS NULL AND lower(s.email) <> lower(l.email)
		AND (s.accepted_at IS NOT NULL OR s.invite_expires_at IS NULL OR s.invite_expires_at > now())`

// CountLicensesOverSeatLimit returns how many licenses on the plan have
// more people joined, the owner included, than maxSeats would allow:
// those on which lowering the plan's max_seats to it would pause the
// members who joined last (see seatPausedSQL). Pending invitations are
// not counted; they only wait for a free seat. 0 when maxSeats <= 0 (no
// limit).
func (s *Store) CountLicensesOverSeatLimit(ctx context.Context, planID string, maxSeats int) (int, error) {
	if maxSeats <= 0 {
		return 0, nil
	}
	var n int
	err := s.DB.NewRaw(`SELECT COUNT(*) FROM licenses l WHERE l.plan_id = ? AND 1 + (
		SELECT COUNT(*) FROM seats s
		WHERE s.license_id = l.id AND s.removed_at IS NULL AND s.accepted_at IS NOT NULL
			AND lower(s.email) <> lower(l.email)) > ?`,
		planID, maxSeats).Scan(ctx, &n)
	return n, err
}

// CountActiveSeats returns how many members a license has, the owner
// not included (see memberSeatsSQL).
func (s *Store) CountActiveSeats(ctx context.Context, licenseID string) (int, error) {
	return CountActiveSeatsIn(ctx, s.DB, licenseID)
}

// CountActiveSeatsIn is CountActiveSeats on a caller's transaction.
func CountActiveSeatsIn(ctx context.Context, db bun.IDB, licenseID string) (int, error) {
	var n int
	err := db.NewRaw(memberSeatsSQL, licenseID).Scan(ctx, &n)
	return n, err
}

// RemoveSeat takes the member off the licence. Their key is cleared and
// the devices and floating sessions it took are removed with it, in one
// transaction under the licence row lock those take, so none slips in
// afterwards (see ActivateForKey). It returns the devices removed, and
// whether the member was removed just now rather than already gone.
func (s *Store) RemoveSeat(ctx context.Context, id string) ([]*model.Activation, bool, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback() //nolint:errcheck

	var licenseID string
	if err := tx.NewRaw("SELECT license_id FROM seats WHERE id = ?", id).Scan(ctx, &licenseID); err != nil {
		return nil, false, err
	}
	if err := lockLicenseIn(ctx, tx, licenseID); err != nil {
		return nil, false, err
	}
	res, err := tx.NewUpdate().Model((*model.Seat)(nil)).
		Set("removed_at = ?", time.Now()).
		Where("id = ? AND removed_at IS NULL", id).Exec(ctx)
	if err != nil {
		return nil, false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, false, err
	}
	// Whether or not it was removed just now: a removed member has no
	// key and no devices.
	if _, err := tx.NewUpdate().Model((*model.Seat)(nil)).
		Set("member_key = NULL").
		Set("member_key_hash = NULL").
		Set("member_key_encrypted = NULL").
		Where("id = ?", id).Exec(ctx); err != nil {
		return nil, false, err
	}
	var removed []*model.Activation
	if err := tx.NewRaw("DELETE FROM activations WHERE seat_id = ? RETURNING *", id).Scan(ctx, &removed); err != nil {
		return nil, false, err
	}
	if _, err := tx.NewRaw("DELETE FROM floating_sessions WHERE seat_id = ?", id).Exec(ctx); err != nil {
		return nil, false, err
	}
	return removed, n == 1, tx.Commit()
}
