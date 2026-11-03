package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/tabloy/keygate/internal/crypto"
	"github.com/tabloy/keygate/internal/license"
	"github.com/tabloy/keygate/internal/model"
)

type Store struct {
	DB *bun.DB
	// LicenseKeyAEAD is optional: when set, license keys are AES-GCM
	// encrypted at rest in license_key_encrypted alongside the plaintext
	// column. Reads prefer the encrypted column with fallback to plaintext.
	// nil means encryption is disabled (legacy mode).
	LicenseKeyAEAD *crypto.AESGCM
	// feedTTL is how long this install signs the presigned links
	// inside a public feed for; see SetFeedURLTTL.
	feedTTL time.Duration
	// dsn opens the pool RunMigrations uses; see migrationDB.
	dsn string
}

func New(dsn string) (*Store, error) {
	sqldb := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn)))
	sqldb.SetMaxOpenConns(25)
	sqldb.SetMaxIdleConns(5)
	sqldb.SetConnMaxLifetime(5 * time.Minute)
	sqldb.SetConnMaxIdleTime(2 * time.Minute)

	db := bun.NewDB(sqldb, pgdialect.New())
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("ping: %w", err)
	}
	return &Store{DB: db, dsn: dsn}, nil
}

func (s *Store) Close() error { return s.DB.Close() }

const (
	// migrationLockWait is how long RunMigrations waits for another
	// instance to finish its migrations before giving up.
	migrationLockWait = 10 * time.Minute
	// migrationTimeout bounds each statement of a migration, and with
	// migrationGrace the whole of one migration file.
	migrationTimeout = 5 * time.Minute
	// migrationGrace is how much longer the client waits than the server
	// is told to, so the server's own timeout ends a statement or a lock
	// wait first. A client deadline does not end it: the driver only
	// closes its socket, and the backend runs on, or stays queued for the
	// lock, after the client has given up.
	migrationGrace = 30 * time.Second
)

// migrationTableLockWait bounds how long one migration waits for a lock
// on a table it changes. While it waits, every query on that table
// queues behind it, and so does every query on the tables it has
// already locked, so a migration stuck behind a long transaction (an
// export, a session idle in a transaction) would hold up all that
// traffic for as long as it waited. Instead it gives up quickly, rolls
// back, and is tried again after migrationLockBackoff; each file runs in
// one transaction, so a retry starts clean. A deadlock the server breaks
// against the migration is retried the same way. Variables so tests can
// shorten them.
var (
	migrationTableLockWait = 2 * time.Second
	migrationLockBackoff   = func(attempt int) time.Duration { return time.Duration(min(attempt, 5)) * time.Second }
	// migrationLockAttempts is how many times one migration is tried
	// before RunMigrations gives up: about four and a half minutes with
	// the above.
	migrationLockAttempts = 40
)

// migrationDB opens the connection RunMigrations holds its lock and
// applies migrations on. The driver bounds every read, at 10 seconds by
// default, deadline or not, which the store's pool keeps for ordinary
// queries; here it would fail any migration longer than that, an index
// on a large table say, and any wait for another instance's run. This
// one waits as long as the longest of those, so that a read that never
// returns, a COMMIT say, still ends; lock_timeout, statement_timeout and
// the contexts RunMigrations sets are what end the rest.
func (s *Store) migrationDB() (*bun.DB, error) {
	if s.dsn == "" {
		return nil, errors.New("run migrations: store not opened with New")
	}
	sqldb := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(s.dsn),
		pgdriver.WithReadTimeout(migrationLockWait+migrationGrace)))
	sqldb.SetMaxOpenConns(1)
	return bun.NewDB(sqldb, pgdialect.New()), nil
}

// RunMigrations executes all .up.sql files from the migrations directory in order.
// Guarantees:
//   - Advisory lock prevents concurrent execution across multiple instances
//   - Each migration + its tracking record run in the SAME transaction (atomic)
//   - Checksum validation detects tampered migration files
//   - Timeout protection prevents indefinite blocking
//   - Failed migrations are fully rolled back — no partial state
func (s *Store) RunMigrations(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read migrations dir: %w", err)
	}

	ctx := context.Background()

	// Create migrations tracking table (idempotent)
	_, _ = s.DB.ExecContext(ctx,
		`CREATE TABLE IF NOT EXISTS schema_migrations (
			filename TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			checksum TEXT NOT NULL DEFAULT ''
		)`)
	_, _ = s.DB.ExecContext(ctx,
		`ALTER TABLE schema_migrations ADD COLUMN IF NOT EXISTS checksum TEXT NOT NULL DEFAULT ''`)

	// Acquire advisory lock to prevent concurrent migration across instances.
	// Lock ID 7367616 = crc32("keygate_migrations") — unique per application.
	// The lock belongs to a session, so it is taken, held and released on
	// one pinned connection, and the migrations run on that connection
	// too. Through the pool, the lock lived on whichever connection ran
	// the statement: the pool closes a connection that outlives
	// ConnMaxLifetime, which dropped the lock partway through a long run,
	// and an unlock could land on another connection, leaving the lock
	// held by an idle one. Running the migrations on the lock's session
	// means the lock cannot end while one still runs: a session the
	// server ends (idle_session_timeout, a dropped connection) ends the
	// migration with it, and a migration whose client gave up keeps the
	// lock until its backend has finished.
	mdb, err := s.migrationDB()
	if err != nil {
		return err
	}
	defer mdb.Close()
	// The context covers connecting too, which the pool leaves unbounded.
	lockCtx, cancelLock := context.WithTimeout(ctx, migrationLockWait+migrationGrace)
	defer cancelLock()
	lockConn, err := mdb.Conn(lockCtx)
	if err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	defer lockConn.Close()
	// lock_timeout alone bounds the wait: a statement_timeout set for the
	// role or database would otherwise cut it short.
	if _, err := lockConn.ExecContext(lockCtx, fmt.Sprintf("SET lock_timeout = %d; SET statement_timeout = 0", migrationLockWait.Milliseconds())); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	if _, err := lockConn.ExecContext(lockCtx, "SELECT pg_advisory_lock(7367616)"); err != nil {
		if isLockTimeout(err) {
			return fmt.Errorf("acquire migration lock: another instance has been migrating for over %v: %w", migrationLockWait, err)
		}
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		var released bool
		if err := lockConn.NewRaw("SELECT pg_advisory_unlock(7367616)").Scan(unlockCtx, &released); err != nil {
			slog.Warn("release migration lock", "error", err)
		} else if !released {
			// Another session answered, as behind a pooler in transaction
			// mode, which session locks do not survive.
			slog.Warn("release migration lock: not held by this session")
		}
	}()

	// Verify checksums of previously applied migrations
	var existing []struct {
		Filename string `bun:"filename"`
		Checksum string `bun:"checksum"`
	}
	// A failed read must not pass for "none applied", which would run
	// every migration again.
	if err := s.DB.NewRaw("SELECT filename, checksum FROM schema_migrations ORDER BY filename").Scan(ctx, &existing); err != nil {
		return fmt.Errorf("read applied migrations: %w", err)
	}
	checksumMap := make(map[string]string, len(existing))
	for _, e := range existing {
		checksumMap[e.Filename] = e.Checksum
	}

	applied := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".up.sql") {
			continue
		}

		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return fmt.Errorf("read %s: %w", entry.Name(), err)
		}
		checksum := checksumBytes(data)

		// Already applied — verify checksum hasn't changed
		if existingCS, ok := checksumMap[entry.Name()]; ok {
			if existingCS != "" && existingCS != checksum {
				slog.Error("migration file modified after apply",
					"file", entry.Name(), "expected", existingCS, "actual", checksum)
				return fmt.Errorf("migration %s has been modified (checksum mismatch: %s != %s). "+
					"Do not edit applied migrations — create a new migration instead",
					entry.Name(), existingCS, checksum)
			}
			continue
		}

		// Apply migration: SQL execution + tracking record in ONE transaction
		applyOnce := func() error {
			migCtx, cancel := context.WithTimeout(ctx, migrationTimeout+migrationGrace)
			defer cancel()

			tx, err := lockConn.BeginTx(migCtx, nil)
			if err != nil {
				return fmt.Errorf("begin tx for %s: %w", entry.Name(), err)
			}
			if _, err := tx.ExecContext(migCtx, fmt.Sprintf("SET LOCAL statement_timeout = %d; SET LOCAL lock_timeout = %d",
				migrationTimeout.Milliseconds(), migrationTableLockWait.Milliseconds())); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("begin tx for %s: %w", entry.Name(), err)
			}

			if _, err := tx.ExecContext(migCtx, string(data)); err != nil {
				errMsg := err.Error()
				// Handle pre-existing objects (from before migration tracking was added)
				if strings.Contains(errMsg, "already exists") || strings.Contains(errMsg, "42P07") ||
					strings.Contains(errMsg, "42701") {
					// Rollback the failed DDL, then record it outside the tx
					_ = tx.Rollback()
					slog.Warn("migration objects already exist (marking as done)", "file", entry.Name())
					_, _ = s.DB.NewRaw(
						"INSERT INTO schema_migrations (filename, checksum) VALUES (?, ?) ON CONFLICT (filename) DO UPDATE SET checksum = ?",
						entry.Name(), checksum, checksum,
					).Exec(ctx)
					return nil
				}
				_ = tx.Rollback()
				return fmt.Errorf("apply %s: %w", entry.Name(), err)
			}

			// Record migration in the SAME transaction — atomic with the DDL
			if _, err := tx.NewRaw(
				"INSERT INTO schema_migrations (filename, checksum) VALUES (?, ?) ON CONFLICT (filename) DO UPDATE SET checksum = ?",
				entry.Name(), checksum, checksum).Exec(migCtx); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("record migration %s: %w", entry.Name(), err)
			}

			if err := tx.Commit(); err != nil {
				return fmt.Errorf("commit %s: %w", entry.Name(), err)
			}
			return nil
		}

		var execErr error
		for attempt := 1; ; attempt++ {
			execErr = applyOnce()
			if execErr == nil || !(isLockTimeout(execErr) || isDeadlock(execErr)) || attempt == migrationLockAttempts {
				break
			}
			wait := migrationLockBackoff(attempt)
			slog.Warn("migration could not lock a table in time; retrying",
				"file", entry.Name(), "attempt", attempt, "lock_wait", migrationTableLockWait, "retry_in", wait,
				"error", execErr, "long_transactions", s.longTransactions(ctx, lockConn))
			time.Sleep(wait)
		}
		if execErr != nil {
			if isLockTimeout(execErr) || isDeadlock(execErr) {
				return fmt.Errorf("%w (a long transaction kept a table locked through %d attempts; end it and restart: %s)",
					execErr, migrationLockAttempts, s.longTransactions(ctx, lockConn))
			}
			return execErr
		}

		applied++
		slog.Info("migration applied", "file", entry.Name(), "checksum", checksum)
	}

	if applied > 0 {
		slog.Info("migrations complete", "applied", applied)
	}
	return nil
}

// longTransactions describes the oldest transactions open on this
// database, for the log when a migration cannot get its locks: one of
// them is likely what holds the table.
func (s *Store) longTransactions(ctx context.Context, conn bun.Conn) string {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var rows []struct {
		PID   int    `bun:"pid"`
		State string `bun:"state"`
		Age   string `bun:"age"`
		Query string `bun:"query"`
	}
	if err := conn.NewRaw(`SELECT pid, coalesce(state, '') AS state,
		date_trunc('second', now() - xact_start)::text AS age, left(coalesce(query, ''), 120) AS query
		FROM pg_stat_activity
		WHERE datname = current_database() AND pid <> pg_backend_pid() AND xact_start < now() - interval '1 second'
		ORDER BY xact_start LIMIT 5`).Scan(qctx, &rows); err != nil {
		return "unknown (" + err.Error() + ")"
	}
	if len(rows) == 0 {
		return "none open now"
	}
	parts := make([]string, len(rows))
	for i, r := range rows {
		parts[i] = fmt.Sprintf("pid %d %s for %s: %s", r.PID, r.State, r.Age, r.Query)
	}
	return strings.Join(parts, "; ")
}

func checksumBytes(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:8]) // short 16-char checksum
}

type AppliedMigration struct {
	Filename  string    `bun:"filename" json:"filename"`
	AppliedAt time.Time `bun:"applied_at" json:"applied_at"`
}

func (s *Store) ListAppliedMigrations(ctx context.Context) ([]*AppliedMigration, error) {
	var out []*AppliedMigration
	err := s.DB.NewRaw(
		"SELECT filename, applied_at FROM schema_migrations ORDER BY filename ASC",
	).Scan(ctx, &out)
	return out, err
}

func newID() string { return uuid.NewString() }

// NewID generates a new UUID. Exported for use by setup handler.
func NewID() string { return newID() }

// ─── User ───

// normalizeEmail is the form an address is compared and keyed in.
// Mail providers treat addresses case-insensitively in practice, so
// "Foo@x.com" and "foo@x.com" are one person. Licences and seats keep
// the address as typed (that is what mail is sent to) and are matched
// case-insensitively; a user row is a login identity and is stored in
// this form, the same one OTP and OAuth sign-in already use.
func normalizeEmail(e string) string { return strings.ToLower(strings.TrimSpace(e)) }

func (s *Store) UpsertUser(ctx context.Context, u *model.User) error {
	if u.ID == "" {
		u.ID = newID()
	}
	// Checkout hands over the address as the buyer typed it; keyed as
	// typed it would miss the account they sign in to and create a
	// second one beside it.
	u.Email = normalizeEmail(u.Email)
	// A blank incoming value means "the caller doesn't know", not "clear
	// it". Login upserts the user with only an email — OTP has no name
	// to offer, and neither does the Stripe checkout path — so writing
	// EXCLUDED.name straight in wiped the display name its owner had
	// set in the portal, on every single login.
	_, err := s.DB.NewInsert().Model(u).
		On("CONFLICT (email) DO UPDATE").
		Set(`name = COALESCE(NULLIF(EXCLUDED.name, ''), "user".name),
			avatar_url = COALESCE(NULLIF(EXCLUDED.avatar_url, ''), "user".avatar_url),
			updated_at = now()`).
		Exec(ctx)
	return err
}

func (s *Store) FindUserByEmail(ctx context.Context, email string) (*model.User, error) {
	u := new(model.User)
	return u, s.DB.NewSelect().Model(u).Where("email = ?", normalizeEmail(email)).Scan(ctx)
}

func (s *Store) FindUserByID(ctx context.Context, id string) (*model.User, error) {
	u := new(model.User)
	return u, s.DB.NewSelect().Model(u).Where("id = ?", id).Scan(ctx)
}

// UpdateUserProfile updates a user's display name.
// Only the name can be changed by the user — email and role are controlled by the system.
func (s *Store) UpdateUserProfile(ctx context.Context, userID, name string) error {
	_, err := s.DB.NewUpdate().Model((*model.User)(nil)).
		Set("name = ?, updated_at = now()", name).
		Where("id = ?", userID).Exec(ctx)
	return err
}

func (s *Store) UpsertOAuth(ctx context.Context, a *model.OAuthAccount) error {
	if a.ID == "" {
		a.ID = newID()
	}
	_, err := s.DB.NewInsert().Model(a).
		On("CONFLICT (provider, provider_id) DO UPDATE").
		Set("email = EXCLUDED.email").
		Exec(ctx)
	return err
}

// ─── Admin Role Management ───

// SyncAdminEmails promotes users whose emails are in the ADMIN_EMAILS list to admin role,
// and ensures at least one owner exists. Called on startup for backward compatibility.
func (s *Store) SyncAdminEmails(ctx context.Context, adminEmails []string) error {
	if len(adminEmails) == 0 {
		return nil
	}

	// Check if any owner exists
	ownerExists, _ := s.DB.NewSelect().Model((*model.User)(nil)).
		Where("role = 'owner'").Exists(ctx)

	for i, email := range adminEmails {
		role := model.RoleAdmin
		if i == 0 && !ownerExists {
			role = model.RoleOwner // First admin email becomes owner if no owner exists
		}
		_, _ = s.DB.NewRaw(`
			UPDATE users SET role = ?, updated_at = now()
			WHERE email = ? AND role = 'user'
		`, role, email).Exec(ctx)
	}
	return nil
}

// FindUserIsAdmin checks if a user has admin privileges by querying the database.
// This is called on every authenticated request to ensure role changes take effect immediately.
func (s *Store) FindUserIsAdmin(ctx context.Context, userID string) bool {
	var role string
	err := s.DB.NewRaw("SELECT role FROM users WHERE id = ?", userID).Scan(ctx, &role)
	if err != nil {
		return false
	}
	return role == model.RoleOwner || role == model.RoleAdmin
}

// ListAdmins returns all users with admin or owner role.
func (s *Store) ListAdmins(ctx context.Context, p Page) ([]*model.User, int, error) {
	var out []*model.User
	q := s.DB.NewSelect().Model(&out).
		Where("role IN ('owner', 'admin')").
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

// CreatePlaceholderUser creates a plain user with minimal info for team
// invites; the invite then grants the role through SetRoleAtomic. The
// user will get proper name when they first log in.
func (s *Store) CreatePlaceholderUser(ctx context.Context, email string) error {
	u := &model.User{
		ID:    newID(),
		Email: email,
		Name:  "",
		Role:  model.RoleUser,
	}
	_, err := s.DB.NewInsert().Model(u).
		On("CONFLICT (email) DO NOTHING"). // Don't overwrite existing user
		Exec(ctx)
	return err
}

// CountOwners returns the number of users with the 'owner' role.
func (s *Store) CountOwners(ctx context.Context) (int, error) {
	return s.DB.NewSelect().Model((*model.User)(nil)).
		Where("role = 'owner'").Count(ctx)
}

// SetupNeeded reports whether this database still waits for its first
// owner. Setup creates that owner without authentication, so anything
// showing the install is in use closes it: setup_complete, or an owner, an
// admin or a product in the database. Counting owners alone left setup
// open on installs whose admins never became owner (#42). Pass the setup
// transaction as db to check under its lock.
func (s *Store) SetupNeeded(ctx context.Context, db bun.IDB) (bool, error) {
	complete, err := getSettingIn(ctx, db, "setup_complete")
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if complete == "true" {
		return false, nil
	}
	var installed bool
	err = db.NewRaw(`SELECT EXISTS (SELECT 1 FROM users WHERE role IN ('owner', 'admin'))
		OR EXISTS (SELECT 1 FROM products)`).Scan(ctx, &installed)
	return !installed, err
}

// EnsureOwner promotes an admin to owner when admins exist but no owner
// does, and returns the promoted email ("" when nothing changed). It picks
// the admin listed first in ADMIN_EMAILS, else the earliest admin.
// Installs from before setup existed, and admins who first signed in
// through ADMIN_EMAILS after startup, were left with no owner, so no one
// could manage roles.
func (s *Store) EnsureOwner(ctx context.Context, adminEmails []string) (string, error) {
	var email string
	err := s.DB.NewRaw(`UPDATE users SET role = 'owner', updated_at = now()
		WHERE id = (SELECT id FROM users WHERE role = 'admin'
			ORDER BY array_position(?::text[], email) NULLS LAST, created_at, id LIMIT 1)
		AND NOT EXISTS (SELECT 1 FROM users WHERE role = 'owner')
		RETURNING email`, pgdialect.Array(adminEmails)).Scan(ctx, &email)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return email, err
}

// PromoteAdminEmail raises a plain user listed in ADMIN_EMAILS to admin,
// or to owner when the install has no owner yet, and returns the role
// the user ends up with.
func (s *Store) PromoteAdminEmail(ctx context.Context, userID string) (string, error) {
	var role string
	err := s.DB.NewRaw(`UPDATE users SET updated_at = now(), role = CASE
			WHEN EXISTS (SELECT 1 FROM users WHERE role = 'owner') THEN 'admin' ELSE 'owner' END
		WHERE id = ? AND role = 'user'
		RETURNING role`, userID).Scan(ctx, &role)
	if errors.Is(err, sql.ErrNoRows) {
		err = s.DB.NewRaw("SELECT role FROM users WHERE id = ?", userID).Scan(ctx, &role)
	}
	return role, err
}

// ErrLastOwner is returned when a role change would leave zero owners.
// Every change that can take the owner role away goes through
// SetRoleAtomic; raw UPDATEs that only grant a role (setup,
// ADMIN_EMAILS promotion, EnsureOwner) cannot break the invariant.
var ErrLastOwner = errors.New("cannot remove the last owner")

// ErrNotOwner is returned by SetRoleAtomic when the acting user is no
// longer an owner by the time the team lock is held.
var ErrNotOwner = errors.New("only an owner can manage team members")

// ErrUserNotFound is returned by SetRoleAtomic when the target user does
// not exist.
var ErrUserNotFound = errors.New("user not found")

// TeamLockKey serializes every team role change.
const TeamLockKey = 8675310

// SetRoleAtomic changes a team member's role under one transaction lock
// shared by every team change, and reports the role the target held
// before, read under that lock, and whether the role changed. It
// recounts owners inside the lock: a change that takes the owner role
// from the last owner fails with ErrLastOwner. When actorID is set, the
// actor must still be an owner under the lock, or it fails with
// ErrNotOwner; the handler's own check ran before the lock, and a
// concurrent change may have demoted the actor since. An empty actorID
// skips that check, for changes the system makes on its own.
//
// Why the lock? Counting owners and then updating in separate
// statements let two concurrent demotions of two different owners both
// see count=2 and leave zero owners; inviting an owner as admin skipped
// the check entirely. Row locks alone only serialize changes to the
// same target, so one advisory lock covers them all. Comparing the
// current role inside it also keeps concurrent identical changes from
// each reporting a change.
func (s *Store) SetRoleAtomic(ctx context.Context, actorID, targetID, role string) (string, bool, error) {
	if role != model.RoleOwner && role != model.RoleAdmin && role != model.RoleUser {
		return "", false, fmt.Errorf("invalid role: %s", role)
	}
	var targetRole string
	changed := false
	err := s.WithXactLock(ctx, TeamLockKey, func(ctx context.Context, tx bun.Tx) error {
		if actorID != "" {
			var actorRole string
			err := tx.NewRaw("SELECT role FROM users WHERE id = ?", actorID).Scan(ctx, &actorRole)
			if errors.Is(err, sql.ErrNoRows) || (err == nil && actorRole != model.RoleOwner) {
				return ErrNotOwner
			}
			if err != nil {
				return err
			}
		}

		err := tx.NewRaw("SELECT role FROM users WHERE id = ?", targetID).Scan(ctx, &targetRole)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrUserNotFound
		}
		if err != nil {
			return err
		}
		if targetRole == role {
			return nil
		}
		if targetRole == model.RoleOwner {
			var ownerCount int
			if err := tx.NewRaw("SELECT COUNT(*) FROM users WHERE role = 'owner'").Scan(ctx, &ownerCount); err != nil {
				return err
			}
			if ownerCount <= 1 {
				return ErrLastOwner
			}
		}

		if _, err := tx.NewRaw("UPDATE users SET role = ?, updated_at = now() WHERE id = ?", role, targetID).Exec(ctx); err != nil {
			return err
		}
		changed = true
		return nil
	})
	return targetRole, changed, err
}

// ─── API Key ───

func HashAPIKey(raw string) string {
	h := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(h[:])
}

func (s *Store) FindProductByAPIKey(ctx context.Context, keyHash string) (*model.Product, *model.APIKey, error) {
	ak := new(model.APIKey)
	err := s.DB.NewSelect().Model(ak).
		Relation("Product").
		Where("key_hash = ?", keyHash).
		Scan(ctx)
	if err != nil {
		return nil, nil, err
	}
	return ak.Product, ak, nil
}

// TouchAPIKey records the most recent successful auth for a key.
// Fire-and-forget: failure must not block the request. Called from
// the auth middleware AFTER the request context has the client IP,
// because the lookup path (FindProductByAPIKey) doesn't see it.
func (s *Store) TouchAPIKey(id, ip string) {
	go func() {
		_, _ = s.DB.NewUpdate().Model((*model.APIKey)(nil)).
			Set("last_used = now()").
			Set("last_used_ip = ?", ip).
			Where("id = ?", id).
			Exec(context.Background())
	}()
}

// ─── License ───

// DecryptLicenseKey returns the plaintext license key for a row.
//
// Read order:
//  1. If LicenseKeyEncrypted is populated AND the AEAD is configured,
//     decrypt and return that. Failure logs at WARN + bumps the
//     LicenseKeyDecryptFailures metric so ops can detect ciphertext
//     corruption (and, post-Phase C, the empty-result that follows).
//     We still fall back to plaintext during Phase A/B so a corrupted row
//     doesn't black-hole the license; Phase C drops the plaintext column,
//     after which a decrypt failure surfaces as an empty key — which the
//     metric makes visible.
//  2. Else return LicenseKey (plaintext column) — used during transition
//     for un-migrated rows and when encryption is unconfigured.
//
// Callers that need to display the key (admin API, customer portal,
// post-purchase email) MUST go through this path rather than reading
// model.License.LicenseKey directly. Phase C will null those direct reads.
func (s *Store) DecryptLicenseKey(l *model.License) string {
	if l == nil {
		return ""
	}
	if s.LicenseKeyAEAD != nil && len(l.LicenseKeyEncrypted) > 0 {
		pt, err := s.LicenseKeyAEAD.Decrypt(l.LicenseKeyEncrypted, []byte(l.ID))
		if err == nil {
			return string(pt)
		}
		slog.Warn("license key decrypt failed; falling back to plaintext column",
			"license_id", l.ID,
			"ciphertext_bytes", len(l.LicenseKeyEncrypted),
			"error", err)
		// metric bump — observable via /metrics
		licenseKeyDecryptFailuresInc()
	}
	return l.LicenseKey
}

// prepareLicenseForInsert fills in the derived fields a license needs at
// insert time: ID, KeyHash, and (when encryption is configured) the
// AES-GCM ciphertext of the plaintext key bound to the license ID via AAD.
//
// Order matters: ID must be assigned BEFORE encryption so the AAD is set.
// AAD = license.ID prevents an attacker who somehow swaps ciphertext rows
// from being able to "move" a license key between IDs.
func (s *Store) prepareLicenseForInsert(l *model.License) error {
	if l.ID == "" {
		l.ID = newID()
	}
	l.KeyHash = license.HashKey(l.LicenseKey)
	if s.LicenseKeyAEAD != nil && l.LicenseKey != "" {
		ct, err := s.LicenseKeyAEAD.Encrypt([]byte(l.LicenseKey), []byte(l.ID))
		if err != nil {
			return fmt.Errorf("encrypt license key: %w", err)
		}
		l.LicenseKeyEncrypted = ct
	}
	return nil
}

// NewLicenseKey generates a key for a license of the product, with the
// prefix that product has now.
func (s *Store) NewLicenseKey(ctx context.Context, productID string) (string, error) {
	var prefix string
	if err := s.DB.NewSelect().Table("products").Column("license_prefix").
		Where("id = ?", productID).Scan(ctx, &prefix); err != nil {
		return "", fmt.Errorf("license prefix of product %s: %w", productID, err)
	}
	return license.GenerateKey(prefix), nil
}

func (s *Store) CreateLicense(ctx context.Context, l *model.License) error {
	if err := s.prepareLicenseForInsert(l); err != nil {
		return err
	}
	_, err := s.DB.NewInsert().Model(l).Exec(ctx)
	return err
}

// CreateLicenseWithSubscription creates a license and, for subscription/trial plans,
// a subscription record in a single transaction to prevent orphan records.
func (s *Store) CreateLicenseWithSubscription(ctx context.Context, l *model.License, plan *model.Plan) error {
	return s.createLicenseWithSubscription(ctx, l, plan, nil)
}

// createLicenseWithSubscription is CreateLicenseWithSubscription with
// an optional guard, run on the transaction once the plan and product
// rows are locked and before anything is written; an error from it
// rolls the whole write back.
func (s *Store) createLicenseWithSubscription(ctx context.Context, l *model.License, plan *model.Plan,
	guard func(ctx context.Context, tx bun.Tx) error) error {
	if err := s.prepareLicenseForInsert(l); err != nil {
		return err
	}

	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// The rows this insert references are locked first and the gate
	// after: the other order deadlocks against a product or plan
	// edit. Locking them also pins the plan for the checks below.
	if err := LockReferencedRowsIn(ctx, tx, l.PlanID, l.ProductID); err != nil {
		return err
	}
	if guard != nil {
		if err := guard(ctx, tx); err != nil {
			return err
		}
	}

	// The plan the caller read may have been retyped since — a
	// fulfilment can sit between reading the plan and being paid, and
	// the type decides the whole shape of the license: its status,
	// valid_until, update period and whether it carries a
	// subscription row. None of that can be repaired here, so a
	// license built from a plan that has since changed type is not
	// written at all; the caller reads the plan again and rebuilds.
	var planType string
	var planTrialDays, planUpdatesDays int
	if plan != nil {
		if err := tx.NewRaw("SELECT license_type, trial_days, updates_days FROM plans WHERE id = ?", plan.ID).
			Scan(ctx, &planType, &planTrialDays, &planUpdatesDays); err != nil {
			return err
		}
		if planType != plan.LicenseType {
			return fmt.Errorf("%w: %s is now %s, not %s", ErrPlanChanged, plan.ID, planType, plan.LicenseType)
		}
		// The update period the caller derived, or the terms it froze
		// at checkout, were both decided against this snapshot. A plan
		// whose period moved since — to a different length, or to
		// updates for life — has to be read again: the admin path
		// would otherwise write the period it read a moment ago, and
		// a purchase would be fulfilled against terms nobody can
		// account for.
		if planUpdatesDays != plan.UpdatesDays {
			return fmt.Errorf("%w: %s now grants %d update days, not %d",
				ErrPlanChanged, plan.ID, planUpdatesDays, plan.UpdatesDays)
		}
		// A trial licence's end is computed twice from the plan: once
		// by the caller into valid_until, once here into the
		// subscription's trial_end. Both have to come from the same
		// reading, or the licence and the subscription behind it end
		// on different days — and it is valid_until that decides
		// whether the customer can still use it.
		if planType == "trial" && planTrialDays != plan.TrialDays {
			return fmt.Errorf("%w: %s now grants %d trial days, not %d",
				ErrPlanChanged, plan.ID, planTrialDays, plan.TrialDays)
		}
	}

	// A finite period is only worth writing while the product's feeds
	// can enforce it. The gating state is checked here, under the
	// lock the trigger takes, because the caller may have decided the
	// period long before this write — a checkout that was paid days
	// after it was opened — and the product may have been ungated, or
	// gated again, since. The trigger only sees whether the gate is
	// on: it would reject the first case for good and wave the second
	// one through while the public feed's links are still usable.
	if l.UpdatesUntil != nil {
		// The rollout confirmation is read here, on this transaction,
		// and held until it commits: callers check it before they get
		// this far, and an operator switching it off in between would
		// otherwise still see a finite cutoff written afterwards. It
		// is the only fence left between a mixed-version fleet and a
		// period no old replica enforces.
		on, err := MaintenanceFeaturesEnabledForWriteIn(ctx, tx)
		if err != nil {
			return err
		}
		if !on {
			return fmt.Errorf("%w: the maintenance features are switched off", ErrUpdatePeriodNotEnforceable)
		}
		if err := FeedGatingLockIn(ctx, tx, l.ProductID); err != nil {
			return err
		}
		ok, why, err := FeedsCanEnforcePeriodsIn(ctx, tx, l.ProductID, s.feedURLTTL())
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: %s", ErrUpdatePeriodNotEnforceable, why)
		}
	}

	if _, err := tx.NewInsert().Model(l).Exec(ctx); err != nil {
		return err
	}

	if plan != nil && (planType == "subscription" || planType == "trial") {
		sub := &model.Subscription{
			ID:        newID(),
			LicenseID: l.ID,
			PlanID:    plan.ID,
			Status:    l.Status,
		}
		if planType == "trial" && planTrialDays > 0 {
			now := time.Now()
			sub.TrialStart = &now
			until := now.Add(time.Duration(planTrialDays) * 24 * time.Hour)
			sub.TrialEnd = &until
		}
		if _, err := tx.NewInsert().Model(sub).Exec(ctx); err != nil {
			return err
		}
	}

	return tx.Commit()
}

// FindLicenseByKey finds the licence a key opens: the licence's own key,
// or a team member's (see Store.MemberKey), which opens the licence the
// member belongs to and sets KeySeat to their seat. A removed member's
// key is cleared, so it opens nothing; nor does a member's key while the
// product has no seats (its type changed), when the team cannot be
// managed and so a member could not be removed.
func (s *Store) FindLicenseByKey(ctx context.Context, key string) (*model.License, error) {
	keyHash := license.HashKey(key)
	l, err := s.findLicenseWhere(ctx, "license.key_hash = ?", keyHash)
	if err == nil || !errors.Is(err, sql.ErrNoRows) {
		return l, err
	}
	seat := new(model.Seat)
	err = s.DB.NewSelect().Model(seat).
		Where("member_key_hash = ? AND removed_at IS NULL AND accepted_at IS NOT NULL", keyHash).
		Scan(ctx)
	if err == nil {
		l, err := s.findLicenseWhere(ctx, "license.id = ?", seat.LicenseID)
		if err != nil {
			return l, err
		}
		if l.Product == nil || !model.ProductSupports(l.Product.Type, model.CapSeats) {
			return nil, sql.ErrNoRows
		}
		if l.KeySeatPaused, err = s.SeatPaused(ctx, seat.ID); err != nil {
			return nil, err
		}
		l.KeySeat = seat
		return l, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	// Fallback to plaintext for un-migrated keys.
	return s.findLicenseWhere(ctx, "license.license_key = ?", key)
}

func (s *Store) findLicenseWhere(ctx context.Context, where string, arg any) (*model.License, error) {
	// A fresh model each time, so a failed lookup leaves nothing behind.
	l := new(model.License)
	return l, s.DB.NewSelect().Model(l).
		Relation("Product").
		Relation("Plan").
		Relation("Plan.Entitlements").
		Relation("Activations").
		Where(where, arg).
		Scan(ctx)
}

func (s *Store) FindLicenseByStripeSubscription(ctx context.Context, subID string) (*model.License, error) {
	l := new(model.License)
	return l, s.DB.NewSelect().Model(l).Where("stripe_subscription_id = ?", subID).Scan(ctx)
}

// IsCheckoutSessionConflict recognises the unique index on
// licenses.stripe_checkout_session_id: a second license for one paid
// checkout session.
func IsCheckoutSessionConflict(err error) bool {
	return err != nil && strings.Contains(err.Error(), "idx_licenses_stripe_checkout_session")
}

func (s *Store) FindLicenseByStripeCheckoutSession(ctx context.Context, sessionID string) (*model.License, error) {
	l := new(model.License)
	return l, s.DB.NewSelect().Model(l).Where("stripe_checkout_session_id = ?", sessionID).Scan(ctx)
}

func (s *Store) FindLicenseByStripePaymentIntent(ctx context.Context, paymentIntentID string) (*model.License, error) {
	l := new(model.License)
	return l, s.DB.NewSelect().Model(l).Where("stripe_payment_intent_id = ?", paymentIntentID).Scan(ctx)
}

// ListLicensesByStripeCustomer returns every license bought under a
// Stripe customer, newest first.
func (s *Store) ListLicensesByStripeCustomer(ctx context.Context, customerID string) ([]*model.License, error) {
	var ls []*model.License
	err := s.DB.NewSelect().Model(&ls).
		Where("stripe_customer_id = ?", customerID).
		OrderExpr("created_at DESC").
		Scan(ctx)
	return ls, err
}

func (s *Store) UpdateLicense(ctx context.Context, l *model.License, cols ...string) error {
	return UpdateLicenseIn(ctx, s.DB, l, cols...)
}

// UpdateLicenseIn writes the named columns on a caller's transaction,
// for writers that hold a lock across a check and this write.
func UpdateLicenseIn(ctx context.Context, db bun.IDB, l *model.License, cols ...string) error {
	l.UpdatedAt = time.Now()
	cols = append(cols, "updated_at")
	_, err := db.NewUpdate().Model(l).Column(cols...).WherePK().Exec(ctx)
	return err
}

// ErrSubscriptionUnlinked reports that the licence no longer points at
// the subscription the caller resolved it from, so the write was not
// made. It is the expected answer, not a failure: an admin unlinked
// the licence while a Stripe event for the old subscription was in
// flight, and that event must not put subscription-managed state back
// on a licence that is now managed locally.
var ErrSubscriptionUnlinked = errors.New("license no longer linked to this subscription")

// UpdateLicenseAndSubscription writes the licence and mirrors its
// status onto the subscription row, both in one transaction.
func (s *Store) UpdateLicenseAndSubscription(ctx context.Context, lic *model.License, cols ...string) error {
	return s.updateLicenseAndSubscription(ctx, lic, false, nil, cols...)
}

// UpdateLicenseFromSubscription is the same write for a caller that
// found this licence *by* its subscription id — every Stripe
// subscription webhook does.
//
// It applies only while the licence still carries that id. Between the
// read and this write an admin may have unlinked it — a deliberate act
// taken only after Stripe confirmed the subscription was over — and an
// unconditional write by primary key would undo it, restoring a status
// and a period nothing in Stripe backs any more. Then it answers
// ErrSubscriptionUnlinked and writes nothing.
func (s *Store) UpdateLicenseFromSubscription(ctx context.Context, lic *model.License, cols ...string) error {
	return s.updateLicenseAndSubscription(ctx, lic, true, nil, cols...)
}

// ErrLicenseRestricted reports that a Stripe-driven write was refused
// because the licence's own state forbids it: revoked, suspended by an
// operator, canceled with nothing but an event payload (no current read
// of Stripe) saying otherwise, or a pause meeting a licence an operator
// suspended or that ended. Nothing was written — no status, no
// dates — and the caller must not announce the change.
var ErrLicenseRestricted = errors.New("license state does not allow this subscription change")

// ErrStaleSubscriptionRead reports that a newer read of the licence's
// Stripe subscription has already been applied, so this older one was
// not. Like ErrSubscriptionUnlinked it is an expected answer.
var ErrStaleSubscriptionRead = errors.New("a newer read of this subscription is already applied")

// UpdateLicenseFromSubscriptionRead is UpdateLicenseFromSubscription for
// a write made from Stripe's answer to a read taken after asOf (a
// StripeReadStamp). Two webhooks for one subscription can overlap and
// the one that asked Stripe first may write last; the newest read wins,
// in the same UPDATE that makes the write, and an older one gets
// ErrStaleSubscriptionRead. The last change in Stripe is followed by a
// webhook whose read starts after it, so the licence ends on the final
// state.
func (s *Store) UpdateLicenseFromSubscriptionRead(ctx context.Context, lic *model.License, asOf time.Time, cols ...string) error {
	return s.updateLicenseAndSubscription(ctx, lic, true, &asOf, cols...)
}

// LicenseTerms are what a licence is paid for: its plan and how many
// seats.
type LicenseTerms struct {
	PlanID   string `bun:"plan_id"`
	Quantity int    `bun:"quantity"`
}

// UpdateLicenseFromSubscriptionReadTerms is UpdateLicenseFromSubscriptionRead
// for a write that may move the licence's plan or quantity. It returns
// them as they stood under the row lock just before the write, so the
// caller announces a change only when this write is the one that made
// it: two deliveries of the same change both find the old values in
// their own copy of the licence, and only the first finds them here.
//
// A licence an operator suspended refuses the write (ErrLicenseRestricted)
// but still takes the plan and seats Stripe bills, so it does not come
// back from the suspension with seats nobody pays for. written says the
// terms were written, either way: not while a downgrade holds them (see
// LowerLicenseTerms) and this read still shows the old ones.
func (s *Store) UpdateLicenseFromSubscriptionReadTerms(ctx context.Context, lic *model.License, asOf time.Time, cols ...string) (before LicenseTerms, written bool, err error) {
	restricted, held := false, false
	err = RunInTx(ctx, s.DB, func(ctx context.Context, tx bun.Tx) error {
		var row struct {
			LicenseTerms
			Held bool `bun:"held"`
		}
		if err := tx.NewRaw(`SELECT plan_id, quantity, COALESCE(terms_held_until > clock_timestamp(), false) AS held
			FROM licenses WHERE id = ? FOR UPDATE`, lic.ID).Scan(ctx, &row); errors.Is(err, sql.ErrNoRows) {
			return ErrSubscriptionUnlinked // a licence that is gone is unlinked too
		} else if err != nil {
			return err
		}
		before = row.LicenseTerms
		// While a downgrade may still be with Stripe (see
		// LowerLicenseTerms) the plan and seats stay as it set them, unless
		// this read shows them: then Stripe has made it, and the hold goes.
		shows := lic.Quantity == before.Quantity && (!slices.Contains(cols, "plan_id") || lic.PlanID == before.PlanID)
		if row.Held && !shows {
			cols = slices.DeleteFunc(slices.Clone(cols), func(c string) bool { return c == "plan_id" || c == "quantity" })
			held = true
		}
		written = !row.Held || shows
		err := updateLicenseAndSubscriptionIn(ctx, tx, lic, true, &asOf, cols...)
		// A hold the read shows the change for ends; so does one whose
		// time is up, once terms are written over it.
		if err == nil && slices.Contains(cols, "quantity") {
			if _, err := tx.NewRaw("UPDATE licenses SET terms_held_until = NULL, terms_check_after = NULL WHERE id = ? AND terms_held_until IS NOT NULL", lic.ID).Exec(ctx); err != nil {
				return err
			}
		}
		if errors.Is(err, ErrLicenseRestricted) && slices.Contains(cols, "quantity") {
			planID := before.PlanID
			if slices.Contains(cols, "plan_id") {
				planID = lic.PlanID
			}
			res, qerr := tx.NewRaw(`UPDATE licenses SET quantity = ?, plan_id = ?, stripe_synced_at = ?, updated_at = now()
				WHERE id = ? AND stripe_subscription_id = ? AND (stripe_synced_at IS NULL OR stripe_synced_at <= ?)`,
				lic.Quantity, planID, asOf, lic.ID, lic.StripeSubscriptionID, asOf).Exec(ctx)
			if qerr != nil {
				return qerr
			}
			if n, _ := res.RowsAffected(); n == 0 {
				return err
			}
			// The subscription row names the plan too.
			if _, qerr := tx.NewRaw(`UPDATE subscriptions SET plan_id = ?, updated_at = now()
				WHERE license_id = ? AND plan_id <> ?`, planID, lic.ID, planID).Exec(ctx); qerr != nil {
				return qerr
			}
			restricted = true
			return nil
		}
		return err
	})
	if err != nil {
		return before, false, err
	}
	if restricted {
		return before, written, ErrLicenseRestricted
	}
	if held {
		return before, written, ErrTermsHeld
	}
	return before, written, nil
}

// ErrTermsHeld: the read was written but for the plan and seats, which a
// downgrade holds (see LowerLicenseTerms) and the read did not show. A
// caller that can be asked again later (a webhook) is, once the hold is
// over.
var ErrTermsHeld = errors.New("license terms are held by a downgrade")

// LicenseTermsHeld reports whether a downgrade holds the licence's terms
// now (see LowerLicenseTerms).
func (s *Store) LicenseTermsHeld(ctx context.Context, licenseID string) (bool, error) {
	var held bool
	err := s.DB.NewRaw("SELECT COALESCE(terms_held_until > clock_timestamp(), false) FROM licenses WHERE id = ?", licenseID).Scan(ctx, &held)
	return held, err
}

// FindLapsedTermHolds lists licences whose downgrade hold ran out with
// no read since to settle it: the change's outcome was never known, and
// no event may come to say. One whose last check could not finish waits
// until its next check is due, behind the others. limit caps the batch.
func (s *Store) FindLapsedTermHolds(ctx context.Context, limit int) ([]LapsedTermHold, error) {
	var holds []LapsedTermHold
	err := s.DB.NewRaw(`SELECT id, terms_held_until FROM licenses
		WHERE terms_held_until IS NOT NULL AND terms_held_until <= clock_timestamp()
		AND (terms_check_after IS NULL OR terms_check_after <= clock_timestamp())
		ORDER BY terms_check_after NULLS FIRST, terms_held_until LIMIT ?`, limit).Scan(ctx, &holds)
	return holds, err
}

// LapsedTermHold is a hold FindLapsedTermHolds found: the licence, and
// the hold's end, which names it (a downgrade since holds until later).
type LapsedTermHold struct {
	LicenseID string    `bun:"id"`
	Until     time.Time `bun:"terms_held_until"`
}

// PostponeTermsCheck puts off the next check of a lapsed hold by after,
// keeping its record: the check could not finish, and is tried again
// then. A hold placed since is another downgrade's, and is left alone.
func (s *Store) PostponeTermsCheck(ctx context.Context, h LapsedTermHold, after time.Duration) error {
	_, err := s.DB.NewRaw(`UPDATE licenses SET terms_check_after = clock_timestamp() + make_interval(secs => ?)
		WHERE id = ? AND terms_held_until = ?`, after.Seconds(), h.LicenseID, h.Until).Exec(ctx)
	return err
}

// SettleLapsedHold ends a lapsed hold once a check settled it. A hold
// placed since is another downgrade's, and stays.
func (s *Store) SettleLapsedHold(ctx context.Context, h LapsedTermHold) error {
	_, err := s.DB.NewRaw(`UPDATE licenses SET terms_held_until = NULL, terms_check_after = NULL
		WHERE id = ? AND terms_held_until = ?`, h.LicenseID, h.Until).Exec(ctx)
	return err
}

// ReleaseLicenseTerms ends a downgrade's hold, once Stripe is known to
// have made it: the next read sets the terms as Stripe has them.
func (s *Store) ReleaseLicenseTerms(ctx context.Context, licenseID string) error {
	_, err := s.DB.NewRaw("UPDATE licenses SET terms_held_until = NULL, terms_check_after = NULL WHERE id = ?", licenseID).Exec(ctx)
	return err
}

// UpdateLicenseFromSubscriptionEnded is the write for a subscription that
// is over (customer.subscription.deleted). It always applies — nothing in
// Stripe can bring the subscription back, so a newer read can only agree
// — and it moves the licence's read stamp up to asOf, so a read taken
// before the deletion (one that may still have seen "active") is refused
// afterwards. The stamp never moves back.
func (s *Store) UpdateLicenseFromSubscriptionEnded(ctx context.Context, lic *model.License, asOf time.Time, cols ...string) error {
	return RunInTx(ctx, s.DB, func(ctx context.Context, tx bun.Tx) error {
		var recorded []*time.Time
		if err := tx.NewRaw("SELECT stripe_synced_at FROM licenses WHERE id = ? FOR UPDATE", lic.ID).Scan(ctx, &recorded); err != nil {
			return err
		}
		stamp := asOf
		if len(recorded) > 0 && recorded[0] != nil && recorded[0].After(stamp) {
			stamp = *recorded[0]
		}
		lic.StripeSyncedAt = &stamp
		return updateLicenseAndSubscriptionIn(ctx, tx, lic, true, nil, append(cols, "stripe_synced_at")...)
	})
}

func (s *Store) updateLicenseAndSubscription(ctx context.Context, lic *model.License, stillLinked bool, asOf *time.Time, cols ...string) error {
	return RunInTx(ctx, s.DB, func(ctx context.Context, tx bun.Tx) error {
		return updateLicenseAndSubscriptionIn(ctx, tx, lic, stillLinked, asOf, cols...)
	})
}

func updateLicenseAndSubscriptionIn(ctx context.Context, tx bun.Tx, lic *model.License, stillLinked bool, asOf *time.Time, cols ...string) error {
	// The row as this write finds it, under the lock the write keeps
	// until commit: whether it ends a Stripe pause is told once, however
	// many events race to apply the same resume.
	var prior struct {
		Status      string     `bun:"status"`
		By          string     `bun:"by"`
		PastDueAt   *time.Time `bun:"past_due_at"`
		SuspendedAt *time.Time `bun:"suspended_at"`
	}
	if err := tx.NewRaw(`SELECT status, COALESCE(suspended_by, '') AS by, past_due_at, suspended_at FROM licenses WHERE id = ? FOR UPDATE`, lic.ID).
		Scan(ctx, &prior); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	lic.WasStripePaused = prior.Status == model.StatusSuspended && prior.By == model.SuspendedByStripe
	lic.WasCanceled = prior.Status == model.StatusCanceled
	lic.WasExpired = prior.Status == model.StatusExpired
	lic.WasRevoked = prior.Status == model.StatusRevoked
	lic.WasPastDue, lic.PriorPastDueAt = prior.Status == model.StatusPastDue, prior.PastDueAt
	// A licence already past due keeps the moment its episode began: two
	// events entering it at once would each stamp their own, and the
	// later one would restart the dunning ladder and the episode's notice.
	if lic.Status == model.StatusPastDue && prior.Status == model.StatusPastDue && prior.PastDueAt != nil && slices.Contains(cols, "past_due_at") {
		lic.PastDueAt = prior.PastDueAt
	}
	// Likewise a pause Stripe already made keeps its start when a later
	// read shows it again.
	if lic.Status == model.StatusSuspended && lic.WasStripePaused && prior.SuspendedAt != nil && slices.Contains(cols, "suspended_at") {
		lic.SuspendedAt = prior.SuspendedAt
	}
	lic.UpdatedAt = time.Now()
	allCols := append(append([]string{}, cols...), "updated_at")
	// A subscription that ends ends a pause Stripe made with it; an
	// operator's suspension stays on record.
	if lic.Status == model.StatusCanceled && lic.WasStripePaused && !slices.Contains(cols, "suspended_by") {
		lic.SuspendedAt, lic.SuspendedBy = nil, ""
		allCols = append(allCols, "suspended_at", "suspended_by")
	}
	if asOf != nil {
		lic.StripeSyncedAt = asOf
		allCols = append(allCols, "stripe_synced_at")
	}
	q := tx.NewUpdate().Model(lic).Column(allCols...).WherePK()
	guarded := stillLinked && lic.StripeSubscriptionID != ""
	if guarded {
		q = q.Where("stripe_subscription_id = ?", lic.StripeSubscriptionID)
	}
	if asOf != nil {
		q = q.Where("stripe_synced_at IS NULL OR stripe_synced_at <= ?", *asOf)
	}
	// A write driven by the subscription (stillLinked) answers to the
	// licence's own state, checked here, in the write, so a suspension or
	// revocation made a moment earlier is never overwritten: payment can
	// give back what non-payment or expiry took, never what an operator
	// took, nor revive a subscription that ended.
	if stillLinked {
		q = q.Where("status <> ?", model.StatusRevoked)
		// A date-only write must not push valid_until out on a licence
		// that already ended: cancel-at-period-end racing an immediate
		// cancel would restore access until the period end.
		if slices.Contains(cols, "valid_until") && !slices.Contains(cols, "status") {
			q = q.Where("status NOT IN (?, ?, ?)",
				model.StatusCanceled, model.StatusExpired, model.StatusRevoked)
		}
		switch lic.Status {
		case model.StatusActive, model.StatusTrialing, model.StatusPastDue:
			q = q.Where("NOT (status = ? AND COALESCE(suspended_by, ?) <> ?)",
				model.StatusSuspended, model.SuspendedByAdmin, model.SuspendedByStripe)
			// Nor an operator's suspension that non-payment then ended:
			// canceled, the suspension still on record.
			q = q.Where("NOT (status = ? AND COALESCE(suspended_by, '') = ?)",
				model.StatusCanceled, model.SuspendedByAdmin)
			// Stripe never brings a canceled subscription back, so only a
			// current read saying it is live (a canceled licence that was
			// "unpaid", now paid) may revive one — an event payload is
			// history and may predate the end.
			if asOf == nil {
				q = q.Where("status <> ?", model.StatusCanceled)
			}
			// Still unpaid is no reason to bring back a licence that
			// expired for want of payment (past due first, as the sweep
			// leaves it): only payment does. One that expired at the end
			// of its grace may still go into dunning.
			if lic.Status == model.StatusPastDue {
				q = q.Where("NOT (status = ? AND past_due_at IS NOT NULL)", model.StatusExpired)
			}
		case model.StatusSuspended:
			// A pause must not replace an operator's suspension: it would
			// turn it into one a resume lifts. Nor does it reach a licence
			// whose subscription ended. A pause Stripe already made is
			// written again: it changes nothing, but records this read, so
			// an older one (a resume read before the pause) is refused.
			q = q.Where("status <> ?", model.StatusCanceled).
				Where("NOT (status = ? AND COALESCE(suspended_by, ?) <> ?)",
					model.StatusSuspended, model.SuspendedByAdmin, model.SuspendedByStripe)
		}
	}
	res, err := q.Exec(ctx)
	if err != nil {
		return err
	}
	// Paused, expired or revoked by Stripe: its floating sessions end as
	// an operator's suspension ends them, so a resume brings back the
	// licence and not the leases it held.
	if n, err := res.RowsAffected(); err == nil && n > 0 && slices.Contains(cols, "status") {
		if err := endFloatingSessionsIfStoppedIn(ctx, tx, lic.ID, lic.Status); err != nil {
			return err
		}
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 && (guarded || asOf != nil || stillLinked) {
		var now struct {
			Linked bool       `bun:"linked"`
			Synced *time.Time `bun:"synced"`
		}
		// A licence that is gone is unlinked too; a read that failed is
		// a failure, which the caller retries rather than ignores.
		err := tx.NewRaw("SELECT COALESCE(stripe_subscription_id, '') = ? AS linked, stripe_synced_at AS synced FROM licenses WHERE id = ?",
			lic.StripeSubscriptionID, lic.ID).Scan(ctx, &now)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && !now.Linked) {
			return ErrSubscriptionUnlinked
		}
		if err != nil {
			return err
		}
		if asOf != nil && now.Synced != nil && now.Synced.After(*asOf) {
			return ErrStaleSubscriptionRead
		}
		return ErrLicenseRestricted
	}

	// The subscription row follows the status this write set. A write
	// that left the status alone (an invoice moving only the date) syncs
	// nothing: its copy may predate a status written since.
	if slices.Contains(cols, "status") {
		if err := syncSubscriptionStatus(ctx, tx, lic.ID, lic.Status); err != nil {
			return err
		}
	}
	// The subscription row names the plan too; a plan the licence moved
	// to in Stripe moves it with the licence, or the two disagree.
	if slices.Contains(cols, "plan_id") {
		if _, err := tx.NewRaw(`UPDATE subscriptions SET plan_id = ?, updated_at = now()
			WHERE license_id = ? AND plan_id <> ?`, lic.PlanID, lic.ID, lic.PlanID).Exec(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) ListLicensesByEmail(ctx context.Context, email string) ([]*model.License, error) {
	var out []*model.License
	// Include licenses owned by email OR where user has a seat they have
	// accepted: an invite not yet taken up shows nothing. Matched
	// case-insensitively: a licence keeps the address as it was typed,
	// and the portal session carries the lower-cased sign-in address.
	e := normalizeEmail(email)
	err := s.DB.NewSelect().Model(&out).
		Relation("Plan").Relation("Plan.Entitlements").
		Relation("Product").Relation("Activations").Relation("Seats").
		// One IN over a UNION ALL rather than "a = ? OR id IN (...)":
		// the OR form makes Postgres scan every licence instead of
		// using idx_licenses_email_lower.
		Where(`license.id IN (
			SELECT id FROM licenses WHERE lower(email) = ?
			UNION ALL
			SELECT license_id FROM seats WHERE lower(email) = ? AND removed_at IS NULL AND accepted_at IS NOT NULL)`, e, e).
		OrderExpr("license.created_at DESC, license.id DESC").Scan(ctx)
	if err != nil {
		return nil, err
	}
	// The computed counts are filled here rather than by the caller, so
	// a license that leaves this store is complete however it is read.
	// The activations themselves are loaded above, so that count is
	// their length; floating seats live in their own table and need
	// the extra query.
	for _, l := range out {
		l.ActivationCount = len(l.Activations)
	}
	if err := s.fillFloatingCounts(ctx, out); err != nil {
		return nil, err
	}
	return out, nil
}

// HasAccountOrLicense reports whether email already belongs to this
// installation: an existing user, a license owner, or a live seat.
//
// The comparison is case-insensitive on purpose. Login lowercases what
// the visitor types, but the addresses this checks against are typed by
// someone else — an admin filling in the license form, or Stripe
// echoing back whatever the customer entered at checkout — so
// "Alice@example.com" on the license and "alice@example.com" at the
// login box are routinely the same person. Comparing them literally
// would lock that customer out of an installation that runs
// signup_mode=licensed_only.
func (s *Store) HasAccountOrLicense(ctx context.Context, email string) (bool, error) {
	e := strings.ToLower(strings.TrimSpace(email))
	if e == "" {
		return false, nil
	}
	var exists bool
	err := s.DB.NewRaw(`SELECT EXISTS (
		SELECT 1 FROM users WHERE email = ?
		UNION ALL
		SELECT 1 FROM licenses WHERE lower(email) = ?
		UNION ALL
		SELECT 1 FROM seats WHERE lower(email) = ? AND removed_at IS NULL
	)`, e, e, e).Scan(ctx, &exists)
	return exists, err
}

func (s *Store) UpdateLicenseUser(ctx context.Context, licenseID, userID string) error {
	_, err := s.DB.NewUpdate().Model((*model.License)(nil)).
		Set("user_id = ?", userID).
		Where("id = ?", licenseID).
		Exec(ctx)
	return err
}

// LicenseListFilter narrows ListLicenses queries. New filters slot
// in here rather than as positional params so callers (handler +
// future internal uses) don't have to grow a long argument list.
type LicenseListFilter struct {
	ProductID           string
	Status              string
	Search              string
	ExternalCustomerID  string
	ExternalWorkspaceID string
	// Sort is the column the caller asked to order by. The zero
	// value orders by newest first, which is what every caller that
	// does not care about order wants.
	Sort   Sort
	Offset int
	// Limit must be positive. Unlike Page, a zero here is not "every
	// row": bun drops the LIMIT clause entirely, and the whole table
	// then comes back and has its activations counted in one IN list.
	// The handler clamps what it reads from the query string before
	// it gets here, so no request can ask for that.
	Limit int
}

// escapeLike makes a search term match literally in LIKE / ILIKE ...
// ESCAPE '\': its %, _ and \ stand for themselves.
func escapeLike(term string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(term)
}

func (s *Store) ListLicenses(ctx context.Context, f LicenseListFilter) ([]*model.License, int, error) {
	sort := f.Sort
	if sort.Expr == "" {
		sort = Sort{Expr: "license.created_at", Desc: true}
	}
	q := s.DB.NewSelect().Model((*model.License)(nil)).
		Relation("Plan").Relation("Product")
	q = applySort(q, sort, "license.id")
	if f.ProductID != "" {
		q = q.Where("license.product_id = ?", f.ProductID)
	}
	if f.Status != "" {
		q = q.Where("license.status = ?", f.Status)
	}
	if f.ExternalCustomerID != "" {
		q = q.Where("license.external_customer_id = ?", f.ExternalCustomerID)
	}
	if f.ExternalWorkspaceID != "" {
		q = q.Where("license.external_workspace_id = ?", f.ExternalWorkspaceID)
	}
	if search := strings.TrimSpace(f.Search); search != "" {
		// An address matches in part, literally (% and _ are not
		// wildcards); a key only whole, by its hash, the licence's or a
		// member's. A match on part of a key would let a search guess
		// one a character at a time, around the audited reveal.
		term := "%" + escapeLike(search) + "%"
		hash := license.HashKey(search)
		q = q.Where(`(license.email ILIKE ? ESCAPE '\' OR license.key_hash = ?
			OR EXISTS (SELECT 1 FROM seats s WHERE s.license_id = license.id AND s.removed_at IS NULL
				AND (s.email ILIKE ? ESCAPE '\' OR s.member_key_hash = ?)))`,
			term, hash, term, hash)
	}
	total, err := q.Count(ctx)
	if err != nil {
		return nil, 0, err
	}
	var out []*model.License
	if err := q.Offset(f.Offset).Limit(f.Limit).Scan(ctx, &out); err != nil {
		return nil, 0, err
	}
	if err := s.fillActivationCounts(ctx, out); err != nil {
		return nil, 0, err
	}
	if err := s.fillFloatingCounts(ctx, out); err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

// fillFloatingCounts populates ActiveSessionCount for the floating
// licenses on one page.
//
// Only floating plans are asked about: on any other plan the number
// would always be zero, and the query is skipped entirely when a page
// has none, which is the common case.
func (s *Store) fillFloatingCounts(ctx context.Context, licenses []*model.License) error {
	ids := make([]string, 0, len(licenses))
	for _, l := range licenses {
		if l.Plan != nil && l.Plan.LicenseModel == "floating" {
			ids = append(ids, l.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	var rows []struct {
		LicenseID string `bun:"license_id"`
		N         int    `bun:"n"`
	}
	err := s.DB.NewSelect().Model((*model.FloatingSession)(nil)).
		ColumnExpr("license_id").
		ColumnExpr("count(*) AS n").
		Where("license_id IN (?) AND expires_at > now()", bun.List(ids)).
		GroupExpr("license_id").
		Scan(ctx, &rows)
	if err != nil {
		return err
	}
	counts := make(map[string]int, len(rows))
	for _, r := range rows {
		counts[r.LicenseID] = r.N
	}
	for _, l := range licenses {
		l.ActiveSessionCount = counts[l.ID]
	}
	return nil
}

// fillActivationCounts populates ActivationCount for one page of
// licenses.
//
// It is a second statement rather than a joined count because the
// first one is what the pager counts rows with: a GROUP BY on the
// paged query would have to survive Count(), and a has-many relation
// would pull every activation row of every license on the page back
// just to take its length. One extra query over at most a page of ids
// is both cheaper and easier to be sure of.
func (s *Store) fillActivationCounts(ctx context.Context, licenses []*model.License) error {
	if len(licenses) == 0 {
		return nil
	}
	ids := make([]string, 0, len(licenses))
	for _, l := range licenses {
		ids = append(ids, l.ID)
	}
	var rows []struct {
		LicenseID string `bun:"license_id"`
		N         int    `bun:"n"`
	}
	err := s.DB.NewSelect().Model((*model.Activation)(nil)).
		ColumnExpr("license_id").
		ColumnExpr("count(*) AS n").
		Where("license_id IN (?)", bun.List(ids)).
		GroupExpr("license_id").
		Scan(ctx, &rows)
	if err != nil {
		return err
	}
	counts := make(map[string]int, len(rows))
	for _, r := range rows {
		counts[r.LicenseID] = r.N
	}
	for _, l := range licenses {
		l.ActivationCount = counts[l.ID]
	}
	return nil
}

// ─── Plan ───

func (s *Store) FindPlanByStripePrice(ctx context.Context, priceID string) (*model.Plan, error) {
	p := new(model.Plan)
	return p, s.DB.NewSelect().Model(p).Relation("Entitlements").Where("stripe_price_id = ?", priceID).Scan(ctx)
}

// FindPlanByStripeRenewalPrice returns a plan selling renewals at the
// given price; several plans may share one, the first is returned.
func (s *Store) FindPlanByStripeRenewalPrice(ctx context.Context, priceID string) (*model.Plan, error) {
	p := new(model.Plan)
	return p, s.DB.NewSelect().Model(p).Where("stripe_renewal_price_id = ?", priceID).Limit(1).Scan(ctx)
}

func (s *Store) FindPlanByCheckoutID(ctx context.Context, checkoutID string) (*model.Plan, error) {
	p := new(model.Plan)
	return p, s.DB.NewSelect().Model(p).Relation("Entitlements").Where("checkout_id = ?", checkoutID).Scan(ctx)
}

func (s *Store) FindPlanByID(ctx context.Context, id string) (*model.Plan, error) {
	return FindPlanByIDIn(ctx, s.DB, id)
}

// FindPlanByIDIn reads the plan on a caller's transaction, for writers
// that must see the plan a locked license points at right now.
func FindPlanByIDIn(ctx context.Context, db bun.IDB, id string) (*model.Plan, error) {
	p := new(model.Plan)
	return p, db.NewSelect().Model(p).Relation("Entitlements").Where("plan.id = ?", id).Scan(ctx)
}

// ─── Activation ───

func (s *Store) CreateActivation(ctx context.Context, a *model.Activation) error {
	if a.ID == "" {
		a.ID = newID()
	}
	_, err := s.DB.NewInsert().Model(a).Exec(ctx)
	return err
}

func (s *Store) FindActivation(ctx context.Context, licenseID, identifier string) (*model.Activation, error) {
	a := new(model.Activation)
	return a, s.DB.NewSelect().Model(a).
		Where("license_id = ? AND identifier = ?", licenseID, identifier).Scan(ctx)
}

func (s *Store) TouchActivation(ctx context.Context, id string) error {
	_, err := s.DB.NewUpdate().Model((*model.Activation)(nil)).
		Set("last_verified = now()").Where("id = ?", id).Exec(ctx)
	return err
}

// DeleteActivation removes one activation: sql.ErrNoRows when it is
// already gone, so of two releasing the same device only one goes on
// to audit it and fire license.deactivated.
func (s *Store) DeleteActivation(ctx context.Context, id string) error {
	return deletedOne(s.DB.NewDelete().Model((*model.Activation)(nil)).Where("id = ?", id).Exec(ctx))
}

func (s *Store) CountActivations(ctx context.Context, licenseID string) (int, error) {
	return s.DB.NewSelect().Model((*model.Activation)(nil)).
		Where("license_id = ?", licenseID).Count(ctx)
}

// FindExpiringLicenses returns active licenses that expire between `from` and `to`.
func (s *Store) FindExpiringLicenses(ctx context.Context, from, to time.Time) ([]*model.License, error) {
	var out []*model.License
	err := s.DB.NewSelect().Model(&out).
		Relation("Product").
		Relation("Plan").
		Where("license.status IN ('active', 'trialing')").
		Where("license.valid_until IS NOT NULL").
		Where("license.valid_until >= ?", from).
		Where("license.valid_until <= ?", to).
		OrderExpr("license.valid_until ASC").
		Scan(ctx)
	return out, err
}

// ─── Audit ───

func (s *Store) Audit(ctx context.Context, log *model.AuditLog) {
	if log.ID == "" {
		log.ID = newID()
	}
	entry := *log
	go func() {
		auditCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if entry.ProductID == "" {
			entry.ProductID = s.ProductOfAudited(auditCtx, entry.Entity, entry.EntityID)
		}
		if _, err := s.DB.NewInsert().Model(&entry).Exec(auditCtx); err != nil {
			slog.Error("audit log write failed", "entity", log.Entity, "entity_id", log.EntityID, "action", log.Action, "error", err)
		}
	}()
}

// FindLicensesForGraceExpiry returns active/past_due licenses that have passed valid_until.
func (s *Store) FindLicensesForGraceExpiry(ctx context.Context) ([]*model.License, error) {
	var out []*model.License
	err := s.DB.NewSelect().Model(&out).
		Relation("Product").Relation("Plan").
		Where("license.status IN ('active', 'past_due')").
		Where("license.valid_until IS NOT NULL").
		Where("license.valid_until < ?", time.Now()).
		Scan(ctx)
	return out, err
}

// FindExpiredTrials returns trialing licenses that have passed valid_until.
//
// A trial a Stripe subscription bills is Stripe's to end: at its end
// Stripe charges and turns it active, or past due, and says so by
// webhook, which may come some time after valid_until. Expiring it first
// would tell a customer who is paying that their trial ended, and clear
// their devices. It is left to Stripe for the plan's grace days, and only
// past them, with nothing heard, expired as a trial (webhooks lost for
// good). At least a day, whatever the grace days: Stripe's charge and
// webhook take that much. (Whether the key works meanwhile is the grace
// days' own rule, see assertUsable; with none it stops at valid_until.)
func (s *Store) FindExpiredTrials(ctx context.Context) ([]*model.License, error) {
	var out []*model.License
	now := time.Now()
	err := s.DB.NewSelect().Model(&out).
		Relation("Product").Relation("Plan").
		Where("license.status = 'trialing'").
		Where("license.valid_until IS NOT NULL").
		Where("license.valid_until < ?", now).
		Where(`(COALESCE(license.stripe_subscription_id, '') = ''
			OR license.valid_until < ?::timestamptz - make_interval(days => GREATEST(COALESCE("plan"."grace_days", 7), 1)))`, now).
		Scan(ctx)
	return out, err
}

// FindStalePastDueLicenses returns past_due licenses whose dunning
// clock (past_due_at) crossed the threshold. Falls back to
// updated_at when past_due_at is unset, so legacy rows that pre-date
// the column don't get stranded.
func (s *Store) FindStalePastDueLicenses(ctx context.Context, before time.Time) ([]*model.License, error) {
	var out []*model.License
	err := s.DB.NewSelect().Model(&out).
		Where("status = 'past_due'").
		Where("COALESCE(past_due_at, updated_at) < ?", before).
		Scan(ctx)
	return out, err
}

// DeleteExpiredActivations removes activations for expired/revoked licenses.
func (s *Store) DeleteExpiredActivations(ctx context.Context) (int, error) {
	res, err := s.DB.NewDelete().
		TableExpr("activations").
		Where("license_id IN (SELECT id FROM licenses WHERE status IN ('expired', 'revoked'))").
		Exec(ctx)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// SyncSubscriptionStatuses syncs subscription.status with its license.status for consistency.
func (s *Store) SyncSubscriptionStatuses(ctx context.Context) error {
	_, err := s.DB.NewRaw(`
		UPDATE subscriptions SET status = l.status, updated_at = now()
		FROM licenses l
		WHERE subscriptions.license_id = l.id
		AND subscriptions.status != l.status
		AND l.status IN ('expired', 'canceled', 'revoked')
	`).Exec(ctx)
	return err
}

// cancelStateSQL is what Stripe last told us about a licence's newest
// subscription: true when it ends at the period end, false when it
// renews, NULL when nothing has been recorded yet (rows from before the
// state was kept, until SyncCancelStates or a webhook fills it in).
const cancelStateSQL = `(SELECT sub.cancel_at_period_end FROM subscriptions sub
	WHERE sub.license_id = license.id AND sub.cancel_state_synced_at IS NOT NULL
	ORDER BY sub.created_at DESC LIMIT 1)`

// FindLicensesForExpiryReminder returns active and trialing licenses that
// run out between from and to and will not renew on their own: no Stripe
// subscription, or one Stripe confirmed ends at the period end. One that
// renews gets the renewal reminder instead (FindLicensesForRenewalReminder);
// one whose state is not known yet gets neither until it is — a late
// reminder beats a wrong one.
func (s *Store) FindLicensesForExpiryReminder(ctx context.Context, from, to time.Time) ([]*model.License, error) {
	var out []*model.License
	err := s.DB.NewSelect().Model(&out).
		Relation("Product").
		Relation("Plan").
		Where("license.status IN ('active', 'trialing')").
		Where("(COALESCE(license.stripe_subscription_id, '') = '' OR "+cancelStateSQL+" IS TRUE)").
		Where("license.valid_until IS NOT NULL").
		Where("license.valid_until >= ?", from).
		Where("license.valid_until <= ?", to).
		OrderExpr("license.valid_until ASC").
		Scan(ctx)
	return out, err
}

// FindLicensesForRenewalReminder returns active licenses whose Stripe
// subscription renews between from and to: billed by Stripe, and Stripe
// confirmed it is not set to end at the period end.
func (s *Store) FindLicensesForRenewalReminder(ctx context.Context, from, to time.Time) ([]*model.License, error) {
	var out []*model.License
	err := s.DB.NewSelect().Model(&out).
		Relation("Product").
		Where("license.status = 'active'").
		Where("COALESCE(license.stripe_subscription_id, '') <> ''").
		Where(cancelStateSQL+" IS FALSE").
		Where("license.valid_until IS NOT NULL").
		Where("license.valid_until >= ?", from).
		Where("license.valid_until <= ?", to).
		Scan(ctx)
	return out, err
}

// StripeReadStamp reads the database clock for a caller about to ask
// Stripe for a subscription. Whatever Stripe answers is at least as new
// as this moment, so the stamp orders the answers: see
// SetSubscriptionCancelScheduled and UpdateLicenseFromSubscriptionRead.
// The database clock, not this process's, so instances whose clocks
// drift still agree on the order.
func (s *Store) StripeReadStamp(ctx context.Context) (time.Time, error) {
	return ledgerStamp(ctx, s.DB)
}

// SetSubscriptionCancelScheduled records what Stripe says about a
// licence's subscription — set to end at the period end, or renewing —
// as read from Stripe after asOf (a StripeReadStamp taken before the
// read). The reminders read it to tell "renews" from "runs out".
//
// The newest read wins, not the last write: two webhooks for one
// subscription can overlap, and the one that asked Stripe first may
// write last. Its answer is then older than the one already recorded
// and is dropped. The last change in Stripe is always followed by a
// webhook whose read starts after it, so that read — the newest — sees
// the final state.
func (s *Store) SetSubscriptionCancelScheduled(ctx context.Context, licenseID string, scheduled bool, asOf time.Time) error {
	return s.setCancelState(ctx, licenseID, scheduled, asOf, false)
}

// CancelScheduledLicenses reports which of the licences have a
// subscription set to end at its period end rather than renew, as last
// recorded from Stripe (SetSubscriptionCancelScheduled).
func (s *Store) CancelScheduledLicenses(ctx context.Context, licenseIDs []string) (map[string]bool, error) {
	out := make(map[string]bool, len(licenseIDs))
	if len(licenseIDs) == 0 {
		return out, nil
	}
	// As the reminders read it (cancelStateSQL): the newest recorded
	// state, for a licence a Stripe subscription still bills and that
	// is still running. One unlinked or ended has nothing left to end.
	var ids []string
	if err := s.DB.NewRaw(`SELECT license.id FROM licenses license
		WHERE license.id IN (?) AND COALESCE(license.stripe_subscription_id, '') <> ''
			AND license.status IN (?) AND `+cancelStateSQL+` IS TRUE`,
		bun.In(licenseIDs), bun.In([]string{model.StatusActive, model.StatusTrialing, model.StatusPastDue})).Scan(ctx, &ids); err != nil {
		return nil, err
	}
	for _, id := range ids {
		out[id] = true
	}
	return out, nil
}

// RecordSubscriptionCancelStateIfUnknown records the state only while
// none is recorded yet: the background sync and fulfilment only fill
// the gap, and leave anything a webhook recorded alone.
func (s *Store) RecordSubscriptionCancelStateIfUnknown(ctx context.Context, licenseID string, scheduled bool, asOf time.Time) error {
	return s.setCancelState(ctx, licenseID, scheduled, asOf, true)
}

// setCancelState writes the cancel state under a lock on the licence
// row, so writers for one licence go one at a time and the check on
// what is recorded holds until the write. A Stripe-billed licence with
// no subscription row (one created before such rows were kept) gets
// one, so the state has somewhere to live.
func (s *Store) setCancelState(ctx context.Context, licenseID string, scheduled bool, asOf time.Time, onlyIfUnknown bool) error {
	return RunInTx(ctx, s.DB, func(ctx context.Context, tx bun.Tx) error {
		var locked []string
		if err := tx.NewRaw(`SELECT id FROM licenses WHERE id = ? FOR UPDATE`, licenseID).Scan(ctx, &locked); err != nil {
			return err
		}
		if len(locked) == 0 {
			return nil // licence gone: nothing to record against
		}
		var recorded []time.Time
		if err := tx.NewRaw(`SELECT max(cancel_state_synced_at) FROM subscriptions
			WHERE license_id = ? HAVING max(cancel_state_synced_at) IS NOT NULL`, licenseID).Scan(ctx, &recorded); err != nil {
			return err
		}
		if len(recorded) > 0 && (onlyIfUnknown || !asOf.After(recorded[0])) {
			return nil // known already, or by a read at least as new
		}
		res, err := tx.NewRaw(`UPDATE subscriptions
			SET cancel_at_period_end = ?, cancel_state_synced_at = ?, updated_at = now()
			WHERE license_id = ?`, scheduled, asOf, licenseID).Exec(ctx)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			return nil
		}
		_, err = tx.NewRaw(`INSERT INTO subscriptions
			(id, license_id, plan_id, status, payment_provider, external_id, cancel_at_period_end, cancel_state_synced_at)
			SELECT ?, l.id, l.plan_id, l.status, 'stripe', COALESCE(l.stripe_subscription_id, ''), ?, ?
			FROM licenses l WHERE l.id = ?`, newID(), scheduled, asOf, licenseID).Exec(ctx)
		return err
	})
}

// StripeCancelStateTarget is a Stripe subscription whose cancel state has
// not been recorded yet (see FindStripeCancelStatesToSync).
type StripeCancelStateTarget struct {
	LicenseID      string `bun:"license_id"`
	SubscriptionID string `bun:"subscription_id"`
	// SortKey is its place in the walk (valid_until, 'infinity' when
	// unset, as text), so a walk resumes after it even if the licence's
	// own date has moved since.
	SortKey string `bun:"sort_key"`
}

// FindStripeCancelStatesToSync lists live Stripe-billed licences whose
// subscription cancel state has never been recorded, soonest to expire
// first, so the reminders that are due next are unblocked first. A
// non-nil after continues the walk past that target, so subscriptions
// Stripe keeps failing to answer for (a 404 stays unknown) cannot hold
// the head of the list and starve the rest.
func (s *Store) FindStripeCancelStatesToSync(ctx context.Context, after *StripeCancelStateTarget, limit int) ([]StripeCancelStateTarget, error) {
	afterKey, afterID := "", ""
	if after != nil {
		afterKey, afterID = after.SortKey, after.LicenseID
	}
	var out []StripeCancelStateTarget
	err := s.DB.NewRaw(`SELECT l.id AS license_id, l.stripe_subscription_id AS subscription_id,
			COALESCE(l.valid_until, 'infinity')::text AS sort_key
		FROM licenses l
		WHERE COALESCE(l.stripe_subscription_id, '') <> ''
		  AND l.status IN ('active', 'trialing', 'past_due')
		  AND NOT EXISTS (SELECT 1 FROM subscriptions s
			WHERE s.license_id = l.id AND s.cancel_state_synced_at IS NOT NULL)
		  AND (?::text = '' OR (COALESCE(l.valid_until, 'infinity'), l.id) > (NULLIF(?::text, '')::timestamptz, ?::text))
		ORDER BY COALESCE(l.valid_until, 'infinity'), l.id
		LIMIT ?`, afterKey, afterKey, afterID, limit).Scan(ctx, &out)
	return out, err
}

// legacyExpiryTags are the tags expiry reminders were recorded under
// before they carried the expiry date: one per license, ever.
var legacyExpiryTags = map[string]int{"expiry_7d": 7, "expiry_3d": 3, "expiry_1d": 1}

// ExpiryRemindersSent lists the reminder windows (in days) already sent
// for this license and expiry date. A reminder recorded in the old,
// dateless form counts when it was sent inside its window before this
// date, so licenses mid-way through their reminders at the upgrade are
// not reminded twice.
func (s *Store) ExpiryRemindersSent(ctx context.Context, licenseID string, validUntil time.Time) ([]int, error) {
	var rows []struct {
		Tag    string    `bun:"tag"`
		SentAt time.Time `bun:"sent_at"`
	}
	prefix := "expiry:" + validUntil.UTC().Format(time.RFC3339) + ":"
	// Only reminders actually queued count. A claim whose sender died
	// before queuing has no sent_at; the next run takes it over.
	err := s.DB.NewRaw(`SELECT tag, sent_at FROM notifications
		WHERE license_id = ? AND sent_at IS NOT NULL
		  AND (starts_with(tag, ?) OR tag IN ('expiry_7d', 'expiry_3d', 'expiry_1d'))`,
		licenseID, prefix).Scan(ctx, &rows)
	if err != nil {
		return nil, err
	}
	var days []int
	for _, r := range rows {
		if d, ok := legacyExpiryTags[r.Tag]; ok {
			// Sent for this date only if it fell inside that window
			// (plus a day of slack for the hourly sweep).
			if r.SentAt.After(validUntil.Add(-time.Duration(d+1) * 24 * time.Hour)) {
				days = append(days, d)
			}
			continue
		}
		var d int
		if _, err := fmt.Sscanf(strings.TrimPrefix(r.Tag, prefix), "%dd", &d); err == nil {
			days = append(days, d)
		}
	}
	return days, nil
}

// RenewalReminderSentLegacy reports a renewal reminder recorded in the
// old, dateless form ("renewal_24h") for this renewal date, so the
// upgrade does not repeat one already sent.
func (s *Store) RenewalReminderSentLegacy(ctx context.Context, licenseID string, validUntil time.Time) bool {
	exists, _ := s.DB.NewSelect().TableExpr("notifications").
		Where("license_id = ? AND tag = 'renewal_24h' AND sent_at > ?", licenseID, validUntil.Add(-48*time.Hour)).
		Exists(ctx)
	return exists
}

// HasNotification checks if a notification with the given tag was already sent for a license.
func (s *Store) HasNotification(ctx context.Context, licenseID, tag string) bool {
	exists, _ := s.DB.NewSelect().
		TableExpr("notifications").
		Where("license_id = ? AND tag = ?", licenseID, tag).
		Exists(ctx)
	return exists
}

// TryRecordNotification records tag for a licence and reports whether
// this call recorded it: of several concurrent callers exactly one goes
// on to send.
func (s *Store) TryRecordNotification(ctx context.Context, licenseID, tag string) (bool, error) {
	res, err := s.DB.NewRaw(
		"INSERT INTO notifications (id, license_id, tag) VALUES (?, ?, ?) ON CONFLICT (license_id, tag) DO NOTHING",
		newID(), licenseID, tag,
	).Exec(ctx)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// RecordNotification records that a notification was sent for a license.
func (s *Store) RecordNotification(ctx context.Context, licenseID, tag string) {
	_, _ = s.DB.NewRaw(
		"INSERT INTO notifications (id, license_id, tag) VALUES (?, ?, ?) ON CONFLICT (license_id, tag) DO NOTHING",
		newID(), licenseID, tag,
	).Exec(ctx)
}

// TryRecordPastDueNotification records tag for a licence only while it
// is still past_due on subscriptionID, in the episode that began at
// pastDueAt (to the second), and reports whether this call recorded it:
// of several concurrent callers exactly one goes on to send, and none
// does once the licence has recovered, been unlinked or begun another
// episode. A database error is returned, for the caller to retry: a
// guess either way would send a duplicate or miss the notice.
func (s *Store) TryRecordPastDueNotification(ctx context.Context, licenseID, subscriptionID string, pastDueAt time.Time, tag string) (bool, error) {
	res, err := s.DB.NewRaw(`INSERT INTO notifications (id, license_id, tag)
		SELECT ?, l.id, ? FROM licenses l
		WHERE l.id = ? AND l.stripe_subscription_id = ? AND l.status = 'past_due'
		  AND floor(extract(epoch FROM l.past_due_at)) = ?
		ON CONFLICT (license_id, tag) DO NOTHING`,
		newID(), tag, licenseID, subscriptionID, pastDueAt.Unix()).Exec(ctx)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// notificationLease is how long a reminder claim may stay unsent
// before another run takes it over — the sender crashed or was
// restarted between claiming and sending. SMTP sessions are bounded
// well inside it (service.smtpSessionTimeout).
const notificationLease = 10 * time.Minute

// ClaimNotification takes the lease on the (license, tag) pair before
// anything is sent. It returns a token identifying this claim; empty
// when the lease was not won. The unique index is the lock: with
// every replica running the reminder loop, a check-then-record would
// let two of them mail the same customer. A row already marked sent
// is never won again; an unsent row is won again once its lease has
// expired, and the take-over issues a fresh token so the previous
// holder can no longer complete or release it.
func (s *Store) ClaimNotification(ctx context.Context, licenseID, tag string) (token string, err error) {
	token = newID()
	var id string
	err = s.DB.NewRaw(
		`INSERT INTO notifications (id, license_id, tag, sent_at, claimed_at) VALUES (?, ?, ?, NULL, now())
		 ON CONFLICT (license_id, tag) DO UPDATE SET claimed_at = now(), id = EXCLUDED.id
		 WHERE notifications.sent_at IS NULL AND notifications.claimed_at < now() - ?::interval
		 RETURNING id`,
		token, licenseID, tag, notificationLease.String(),
	).Scan(ctx, &id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if id != token {
		return "", nil
	}
	return token, nil
}

// ErrNotificationClaimLost: the reminder claim the caller held was
// taken over by another pass while it worked. That pass owns the
// reminder now, so nothing was queued.
var ErrNotificationClaimLost = errors.New("reminder claim taken over")

// EnqueueEmailAndCloseNotification queues one mail and closes the
// reminder claim that produced it, in a single transaction. Doing
// them separately would let a crash in between leave a queued mail
// with an open claim, and the next pass would queue a second copy.
// A claim taken over meanwhile rolls the insert back.
func (s *Store) EnqueueEmailAndCloseNotification(ctx context.Context, to, subject, body, token string) error {
	return RunInTx(ctx, s.DB, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewRaw(
			"INSERT INTO email_queue (id, to_addr, subject, body, max_attempts, notification_id) VALUES (?, ?, ?, ?, 5, ?)",
			newID(), to, subject, body, token,
		).Exec(ctx); err != nil {
			return err
		}
		res, err := tx.NewRaw("UPDATE notifications SET sent_at = now() WHERE id = ? AND sent_at IS NULL", token).Exec(ctx)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotificationClaimLost
		}
		return nil
	})
}

// ReleaseNotification gives the claim the token names back, for a
// delivery that failed after the claim; the next run tries again. A
// token whose claim was taken over meanwhile changes nothing.
func (s *Store) ReleaseNotification(ctx context.Context, token string) error {
	_, err := s.DB.NewDelete().TableExpr("notifications").
		Where("id = ? AND sent_at IS NULL", token).Exec(ctx)
	return err
}

// ─── Refresh Tokens ───

type RefreshToken struct {
	ID        string     `bun:"id,pk"`
	UserID    string     `bun:"user_id,notnull"`
	TokenHash string     `bun:"token_hash,notnull"`
	ExpiresAt time.Time  `bun:"expires_at,notnull"`
	CreatedAt time.Time  `bun:"created_at,default:now()"`
	RevokedAt *time.Time `bun:"revoked_at"`
}

// ErrRefreshTokenReused is returned by RotateRefreshToken when a
// caller presents a token that has already been rotated (revoked_at
// is set). This is the security signal — the caller should revoke
// every refresh_token for the same user, since either the legit user
// or an attacker holding the captured old token will be cut off.
var ErrRefreshTokenReused = errors.New("refresh token reuse detected")

// RefreshReuseGrace is how long a just-rotated refresh token is still
// honoured. Several tabs whose sessions lapse together all renew with
// the same cookie, and a refresh whose response was lost is retried with
// it; without a grace period every one of those would look like token
// theft and sign the user out everywhere. Kept short so a captured token
// is useless almost at once. Revocation by logout or by reuse detection
// deletes the row instead, so it is never covered by this window.
const RefreshReuseGrace = 30 * time.Second

// ErrRefreshUserNotFound is returned when a refresh token's user no
// longer exists.
var ErrRefreshUserNotFound = errors.New("refresh token user not found")

func (s *Store) CreateRefreshToken(ctx context.Context, userID, tokenHash string, expiresAt time.Time) error {
	_, err := s.DB.NewRaw(
		"INSERT INTO refresh_tokens (id, user_id, token_hash, expires_at) VALUES (?, ?, ?, ?)",
		newID(), userID, tokenHash, expiresAt,
	).Exec(ctx)
	return err
}

// FindRefreshToken returns an *active* refresh token (not expired,
// not revoked). Used for read-only checks that don't rotate. The
// rotation path uses RotateRefreshToken instead, which distinguishes
// "missing" from "reused".
func (s *Store) FindRefreshToken(ctx context.Context, tokenHash string) (*RefreshToken, error) {
	rt := new(RefreshToken)
	err := s.DB.NewRaw(
		"SELECT id, user_id, token_hash, expires_at, revoked_at FROM refresh_tokens "+
			"WHERE token_hash = ? AND expires_at > now() AND revoked_at IS NULL",
		tokenHash,
	).Scan(ctx, rt)
	return rt, err
}

// RefreshRenewal is a successful RenewRefreshToken: the presented token,
// its user, and when the new token expires.
type RefreshRenewal struct {
	Token     *RefreshToken
	User      *model.User
	ExpiresAt time.Time
	// Grace is set when the presented token had been rotated within
	// RefreshReuseGrace: another tab, or a retry whose first response
	// was lost.
	Grace bool
}

// RenewRefreshToken exchanges a refresh token for a new one (newHash,
// expiring at expiresAt) and reads its user, all in one transaction: it
// either succeeds whole or leaves the presented token as it was. That is
// what keeps a failure retryable — a token consumed by a renewal whose
// answer never reached the browser would be presented again later and
// read as theft. Outcomes:
//
//   - (renewal, nil): issue the session and the new token. On a grace
//     renewal the presented token stays as it was and the new token
//     keeps its expiry instead of expiresAt, so a stolen token replayed
//     inside the window is worth no more than it already was.
//   - (renewal with Token, ErrRefreshTokenReused): a rotated token
//     presented after the window. The caller MUST wipe every refresh
//     token of Token.UserID.
//   - (nil, sql.ErrNoRows): unknown or expired token; a plain 401.
//   - (nil, ErrRefreshUserNotFound): the user is gone; a plain 401.
//   - (nil, other error): nothing changed; answer 5xx so the client
//     retries.
//
// The row is taken FOR UPDATE first, so concurrent renewals with one
// token (several tabs) serialise: one rotates, the rest are grace.
func (s *Store) RenewRefreshToken(ctx context.Context, tokenHash, newHash string, expiresAt time.Time) (*RefreshRenewal, error) {
	var out *RefreshRenewal
	err := RunInTx(ctx, s.DB, func(ctx context.Context, tx bun.Tx) error {
		rt := new(RefreshToken)
		if err := tx.NewRaw(
			"SELECT id, user_id, token_hash, expires_at, revoked_at FROM refresh_tokens "+
				"WHERE token_hash = ? AND expires_at > now() FOR UPDATE",
			tokenHash,
		).Scan(ctx, rt); err != nil {
			return err
		}
		ren := &RefreshRenewal{Token: rt, ExpiresAt: expiresAt}

		// Already-rotated token presented again. Within the grace period it
		// is a concurrent renewal (another tab, a retried request); after it,
		// a replay — treat as a security incident. Both the rotation time and
		// this check use the database clock, so replicas whose clocks drift
		// apart still agree on the window.
		if rt.RevokedAt != nil {
			if err := tx.NewRaw("SELECT ?::timestamptz > now() - make_interval(secs => ?)",
				*rt.RevokedAt, RefreshReuseGrace.Seconds()).Scan(ctx, &ren.Grace); err != nil {
				return err
			}
			if !ren.Grace {
				out = ren
				return ErrRefreshTokenReused
			}
			if rt.ExpiresAt.Before(ren.ExpiresAt) {
				ren.ExpiresAt = rt.ExpiresAt
			}
		} else {
			var revokedAt time.Time
			if err := tx.NewRaw(
				"UPDATE refresh_tokens SET revoked_at = now() WHERE id = ? RETURNING revoked_at",
				rt.ID,
			).Scan(ctx, &revokedAt); err != nil {
				return err
			}
			rt.RevokedAt = &revokedAt
		}

		user := new(model.User)
		if err := tx.NewSelect().Model(user).Where("id = ?", rt.UserID).Scan(ctx); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrRefreshUserNotFound
			}
			return err
		}
		ren.User = user
		if _, err := tx.NewRaw(
			"INSERT INTO refresh_tokens (id, user_id, token_hash, expires_at) VALUES (?, ?, ?, ?)",
			newID(), rt.UserID, newHash, ren.ExpiresAt,
		).Exec(ctx); err != nil {
			return err
		}
		out = ren
		return nil
	})
	if errors.Is(err, ErrRefreshTokenReused) {
		return out, err
	}
	if err != nil {
		return nil, err
	}
	return out, nil
}

// DeleteRefreshToken removes a token outright. Used on logout, where
// we want the token gone immediately rather than just revoked
// (logout is explicit user intent — no need for reuse-detection
// state to outlive it).
func (s *Store) DeleteRefreshToken(ctx context.Context, tokenHash string) {
	_, _ = s.DB.NewRaw("DELETE FROM refresh_tokens WHERE token_hash = ?", tokenHash).Exec(ctx)
}

// DeleteUserRefreshTokens wipes every refresh token for a user.
// Called on reuse detection (security) and on user-requested
// "log out everywhere".
func (s *Store) DeleteUserRefreshTokens(ctx context.Context, userID string) {
	_, _ = s.DB.NewRaw("DELETE FROM refresh_tokens WHERE user_id = ?", userID).Exec(ctx)
}

func (s *Store) CleanExpiredRefreshTokens(ctx context.Context) {
	_, _ = s.DB.NewRaw("DELETE FROM refresh_tokens WHERE expires_at < now()").Exec(ctx)
}

// ─── OTP Codes ───

func (s *Store) CreateOTPCode(ctx context.Context, otp *model.OTPCode) error {
	if otp.ID == "" {
		otp.ID = newID()
	}
	_, err := s.DB.NewInsert().Model(otp).Exec(ctx)
	return err
}

func (s *Store) CountRecentOTPCodes(ctx context.Context, email string) (int, error) {
	count, err := s.DB.NewSelect().Model((*model.OTPCode)(nil)).
		Where("email = ? AND created_at > now() - interval '10 minutes'", email).
		Count(ctx)
	return count, err
}

func (s *Store) FindLatestValidOTPCode(ctx context.Context, email string) (*model.OTPCode, error) {
	otp := new(model.OTPCode)
	err := s.DB.NewSelect().Model(otp).
		Where("email = ?", email).
		Where("used = false").
		Where("expires_at > now()").
		Where("attempts < ?", MaxOTPAttempts).
		OrderExpr("created_at DESC").
		Limit(1).
		Scan(ctx)
	return otp, err
}

// MaxOTPAttempts is how many guesses one login code allows.
const MaxOTPAttempts = 5

// ClaimOTPAttempt takes one of the code's attempts and returns how many
// it has used now. ok is false when none was left, or the code was used
// or expired meanwhile: the guess must then not be compared at all.
//
// The attempt is taken before the compare, in one statement. Read
// first and counted after, a burst of parallel guesses all saw a count
// under the limit and were all compared.
func (s *Store) ClaimOTPAttempt(ctx context.Context, id string) (attempts int, ok bool, err error) {
	err = s.DB.NewRaw(`UPDATE otp_codes SET attempts = attempts + 1
		WHERE id = ? AND attempts < ? AND used = false AND expires_at > now()
		RETURNING attempts`, id, MaxOTPAttempts).Scan(ctx, &attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return attempts, true, nil
}

// ConsumeOTPCode marks an unused code used and reports whether this
// call was the one that did it. Two requests presenting the same code
// at once both pass the hash check; only one of them gets true here.
func (s *Store) ConsumeOTPCode(ctx context.Context, id string) (bool, error) {
	res, err := s.DB.NewUpdate().Model((*model.OTPCode)(nil)).
		Set("used = true").
		Where("id = ?", id).
		Where("used = false").
		Exec(ctx)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

func (s *Store) CleanExpiredOTPs(ctx context.Context) {
	_, _ = s.DB.NewDelete().Model((*model.OTPCode)(nil)).
		Where("expires_at < now()").
		Exec(ctx)
}

// ─── Processed Events (webhook idempotency) ───

// TryRecordProcessedEvent atomically records a processed event.
// Returns true if this is the first time the event was recorded (should be processed).
// Returns false if the event was already recorded (should be skipped).
// IsEventProcessed reports whether a marker exists; a database error
// reads as "no". Use HasProcessedEvent where the difference matters.
func (s *Store) IsEventProcessed(ctx context.Context, provider, eventID string) bool {
	exists, err := s.HasProcessedEvent(ctx, provider, eventID)
	return err == nil && exists
}

// HasProcessedEvent reports whether a marker exists, keeping database
// failures apart from a missing row: a takeover decision must not be
// made on a lookup that did not run.
func (s *Store) HasProcessedEvent(ctx context.Context, provider, eventID string) (bool, error) {
	return s.DB.NewSelect().TableExpr("processed_events").
		Where("provider = ? AND event_id = ?", provider, eventID).Exists(ctx)
}

// CompleteProcessedEvent removes the in-flight marker of a claim; the
// reservation row (written under the name older binaries use) stays
// as the done marker, so one row per event remains.
func (s *Store) CompleteProcessedEvent(ctx context.Context, claimProvider, eventID string) error {
	return s.DeleteProcessedEvent(ctx, claimProvider, eventID)
}

// WithAdvisoryLock runs fn while holding a session-level advisory
// lock on a pinned connection, serialising the section across every
// replica that shares the database. The lock is released when fn
// returns, or by Postgres if the process dies.
func (s *Store) WithAdvisoryLock(ctx context.Context, key int64, fn func(ctx context.Context) error) error {
	conn, err := s.DB.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.NewRaw("SELECT pg_advisory_lock(?)", key).Exec(ctx); err != nil {
		return err
	}
	defer func() {
		_, _ = conn.NewRaw("SELECT pg_advisory_unlock(?)", key).Exec(context.Background())
	}()
	return fn(ctx)
}

// WithXactLock runs fn inside one transaction that holds a
// transaction-scoped advisory lock on key. Unlike WithAdvisoryLock it
// pins no extra connection: the lock lives on the transaction's own
// connection and every statement in fn runs on it, so a burst of
// callers cannot hold the pool hostage while waiting for a second
// connection to do the work. The lock goes with the commit or
// rollback.
func (s *Store) WithXactLock(ctx context.Context, key int64, fn func(ctx context.Context, tx bun.Tx) error) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.NewRaw("SELECT pg_advisory_xact_lock(?)", key).Exec(ctx); err != nil {
		return err
	}
	if err := fn(ctx, tx); err != nil {
		return err
	}
	return commitTx(ctx, tx)
}

// DeleteProcessedEvent forgets a recorded event.
func (s *Store) DeleteProcessedEvent(ctx context.Context, provider, eventID string) error {
	return DeleteProcessedEventIn(ctx, s.DB, provider, eventID)
}

// DeleteProcessedEventIn is DeleteProcessedEvent on a given connection
// or transaction.
func DeleteProcessedEventIn(ctx context.Context, db bun.IDB, provider, eventID string) error {
	_, err := db.NewDelete().TableExpr("processed_events").
		Where("provider = ? AND event_id = ?", provider, eventID).Exec(ctx)
	return err
}

// HasProcessedEventIn is HasProcessedEvent on a given connection or
// transaction.
func HasProcessedEventIn(ctx context.Context, db bun.IDB, provider, eventID string) (bool, error) {
	return db.NewSelect().TableExpr("processed_events").
		Where("provider = ? AND event_id = ?", provider, eventID).Exists(ctx)
}

// ClaimProcessedEventIn is ClaimProcessedEvent on a given connection
// or transaction.
func ClaimProcessedEventIn(ctx context.Context, db bun.IDB, provider, eventID string) (bool, error) {
	var id string
	err := db.NewRaw(
		"INSERT INTO processed_events (id, provider, event_id) VALUES (?, ?, ?) ON CONFLICT (provider, event_id) DO NOTHING RETURNING id",
		newID(), provider, eventID,
	).Scan(ctx, &id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return id != "", nil
}

// PendingCheckoutSession is one row of the delayed-payment backlog.
type PendingCheckoutSession struct {
	SessionID string    `bun:"event_id"`
	CreatedAt time.Time `bun:"created_at"`
}

// ListPendingCheckoutSessions pages through Stripe checkout sessions
// that completed unpaid (recorded under stripe_pending_session) and
// were not fulfilled since, oldest first, no older than maxAge. Pass
// the last row of the previous page as `after` to continue; a nil
// `after` starts from the beginning. Keyset paging means a round
// visits every row once, so a stuck batch never hides newer rows.
func (s *Store) ListPendingCheckoutSessions(ctx context.Context, maxAge time.Duration, after *PendingCheckoutSession, limit int) ([]PendingCheckoutSession, error) {
	var rows []PendingCheckoutSession
	q := s.DB.NewSelect().TableExpr("processed_events AS p").
		ColumnExpr("p.event_id, p.created_at").
		Where("p.provider = 'stripe_pending_session'").
		Where("p.created_at > now() - make_interval(secs => ?)", int(maxAge.Seconds())).
		// A license, not a claim, is what ends a pending session: a
		// claim can be stale (its owner died) and the sync must keep
		// coming back so fulfilment can take it over.
		Where("NOT EXISTS (SELECT 1 FROM licenses l WHERE l.stripe_checkout_session_id = p.event_id)")
	if after != nil {
		q = q.Where("(p.created_at, p.event_id) > (?, ?)", after.CreatedAt, after.SessionID)
	}
	err := q.OrderExpr("p.created_at ASC, p.event_id ASC").Limit(limit).Scan(ctx, &rows)
	return rows, err
}

// DeleteFulfilledPendingSessions removes pending-session markers for
// sessions that already produced a license.
func (s *Store) DeleteFulfilledPendingSessions(ctx context.Context) error {
	_, err := s.DB.NewDelete().TableExpr("processed_events AS p").
		Where("p.provider = 'stripe_pending_session'").
		Where("EXISTS (SELECT 1 FROM licenses l WHERE l.stripe_checkout_session_id = p.event_id)" +
			" OR EXISTS (SELECT 1 FROM license_renewals r WHERE r.stripe_checkout_session_id = p.event_id)").
		Exec(ctx)
	return err
}

// ReleaseStaleProcessedEvent drops a recorded event older than
// maxAge. Used for fulfilment claims whose owner died mid-way.
func (s *Store) ReleaseStaleProcessedEvent(ctx context.Context, provider, eventID string, maxAge time.Duration) (bool, error) {
	res, err := s.DB.NewDelete().TableExpr("processed_events").
		Where("provider = ? AND event_id = ?", provider, eventID).
		Where("created_at < now() - make_interval(secs => ?)", int(maxAge.Seconds())).
		Exec(ctx)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (s *Store) TryRecordProcessedEvent(ctx context.Context, provider, eventID string) bool {
	claimed, err := s.ClaimProcessedEvent(ctx, provider, eventID)
	return err == nil && claimed
}

// ClaimProcessedEvent atomically records an event. claimed is false
// when the event was already recorded; err reports a database failure,
// which callers must not confuse with "already processed".
func (s *Store) ClaimProcessedEvent(ctx context.Context, provider, eventID string) (claimed bool, err error) {
	var id string
	err = s.DB.NewRaw(
		"INSERT INTO processed_events (id, provider, event_id) VALUES (?, ?, ?) ON CONFLICT (provider, event_id) DO NOTHING RETURNING id",
		newID(), provider, eventID,
	).Scan(ctx, &id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil // conflict: DO NOTHING returned no row
	}
	if err != nil {
		return false, err
	}
	return id != "", nil
}

// ─── Transactional Activation ───

// lockLicenseLimitsIn takes the licence row lock every "count, then take
// a slot" check serialises on, and reads the limits the licence holds
// under it (see model.Plan.Limits). The caller's limit came from a read
// made before the lock, and a subscription read can lower the licence's
// seats in between; checking against the stricter of the two keeps a
// slot from being taken on a limit that is already gone. known is false
// when the licence or its plan is not there to read, and the caller's
// limit stands alone.
func lockLicenseLimitsIn(ctx context.Context, tx bun.Tx, licenseID string) (seats, activations int, known bool, err error) {
	var rows []struct {
		Quantity       int   `bun:"quantity"`
		PerSeat        *bool `bun:"per_seat"`
		MaxSeats       *int  `bun:"max_seats"`
		MaxActivations *int  `bun:"max_activations"`
	}
	if err := tx.NewRaw(`SELECT l.quantity, p.per_seat, p.max_seats, p.max_activations
		FROM licenses l LEFT JOIN plans p ON p.id = l.plan_id
		WHERE l.id = ? FOR UPDATE OF l`, licenseID).Scan(ctx, &rows); err != nil {
		return 0, 0, false, err
	}
	if len(rows) == 0 || rows[0].PerSeat == nil || rows[0].MaxSeats == nil || rows[0].MaxActivations == nil {
		return 0, 0, false, nil
	}
	r := rows[0]
	plan := model.Plan{PerSeat: *r.PerSeat, MaxSeats: *r.MaxSeats, MaxActivations: *r.MaxActivations}
	seats, activations = plan.Limits(r.Quantity)
	return seats, activations, true, nil
}

// licenseChangeLock is the advisory lock class a change to a licence's
// subscription takes, keyed by the licence (see LockLicenseChange).
const licenseChangeLock = 41041

// ErrChangeInProgress: another change to the licence's subscription is
// with Stripe right now.
var ErrChangeInProgress = errors.New("a change to this license is being made")

// LockLicenseChange takes the licence's change lock: one change to its
// subscription at a time. It waits up to wait for a change already under
// way, then gives up with ErrChangeInProgress; nothing else waits for it.
// Unlock gives it back; should the process stop first, the database
// gives it back with the connection.
func (s *Store) LockLicenseChange(ctx context.Context, licenseID string, wait time.Duration) (*ChangeLock, error) {
	conn, err := s.DB.Conn(ctx)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(wait)
	for {
		var got bool
		if err := conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock(?, hashtext(?))", licenseChangeLock, licenseID).Scan(&got); err != nil {
			// The lock may have been taken all the same: the connection
			// is dropped rather than handed back holding it.
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
			_ = conn.Close()
			return nil, err
		}
		if got {
			return &ChangeLock{conn: conn, licenseID: licenseID}, nil
		}
		if time.Now().After(deadline) {
			_ = conn.Close()
			return nil, ErrChangeInProgress
		}
		select {
		case <-ctx.Done():
			_ = conn.Close()
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// ChangeLock is a licence's change lock, on a connection of its own.
type ChangeLock struct {
	conn      bun.Conn
	licenseID string
}

// Unlock gives the change lock back.
func (l *ChangeLock) Unlock() {
	l.exec("SELECT pg_advisory_unlock_all()")
	_ = l.conn.Close()
}

// exec runs an unlock with a context of its own: the request's may be
// over, and no lock may outlive it on a pooled connection.
func (l *ChangeLock) exec(query string, args ...any) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := l.conn.ExecContext(ctx, query, args...); err != nil {
		// Handed back to the pool, a connection whose unlock failed would
		// still hold the lock: it is dropped instead.
		_ = l.conn.Raw(func(any) error { return driver.ErrBadConn })
	}
}

// InUse is what a licence uses of its limits: its people (the owner
// included), activations, and floating sessions running now.
type InUse struct {
	People, Activations, Sessions int
}

// CountInUse counts what a licence uses of its limits.
func (s *Store) CountInUse(ctx context.Context, licenseID string) (InUse, error) {
	return countInUse(ctx, s.DB, licenseID)
}

// CountInUseIn is CountInUse on a caller's transaction.
func CountInUseIn(ctx context.Context, db bun.IDB, licenseID string) (InUse, error) {
	return countInUse(ctx, db, licenseID)
}

func countInUse(ctx context.Context, db bun.IDB, licenseID string) (InUse, error) {
	var u InUse
	if err := db.NewRaw(memberSeatsSQL, licenseID).Scan(ctx, &u.People); err != nil {
		return u, err
	}
	u.People++
	if err := db.NewRaw("SELECT COUNT(*) FROM activations WHERE license_id = ?", licenseID).Scan(ctx, &u.Activations); err != nil {
		return u, err
	}
	if err := db.NewRaw("SELECT COUNT(*) FROM floating_sessions WHERE license_id = ? AND expires_at > now()", licenseID).Scan(ctx, &u.Sessions); err != nil {
		return u, err
	}
	return u, nil
}

// LowerLicenseTerms moves a licence to lower terms ahead of the change
// to its subscription that makes them, so nothing is taken meanwhile on
// limits about to go: under the licence row lock (the one every slot is
// taken under), what is in use is counted, check may refuse (its error
// is returned as is, and nothing changes), and the terms are written.
// The read stamp moves to now, so a read of the subscription from before
// cannot put the old terms back; and the terms are held for hold, so no
// read until then that still shows the old ones can either, whatever
// becomes of the request that lowered them (a lost answer, a restart).
// A read that shows the new terms ends the hold, as do
// ReleaseLicenseTerms and RestoreLicenseTerms. It returns the terms
// the licence had, for RestoreLicenseTerms should Stripe not make the
// change.
func (s *Store) LowerLicenseTerms(ctx context.Context, licenseID string, to LicenseTerms, hold time.Duration, check func(InUse) error) (prev LicenseTerms, err error) {
	err = RunInTx(ctx, s.DB, func(ctx context.Context, tx bun.Tx) error {
		if err := tx.NewRaw("SELECT plan_id, quantity FROM licenses WHERE id = ? FOR UPDATE", licenseID).Scan(ctx, &prev); err != nil {
			return err
		}
		u, err := countInUse(ctx, tx, licenseID)
		if err != nil {
			return err
		}
		if err := check(u); err != nil {
			return err
		}
		if _, err := tx.NewRaw(`UPDATE licenses SET plan_id = ?, quantity = ?, stripe_synced_at = clock_timestamp(),
			terms_held_until = clock_timestamp() + make_interval(secs => ?), terms_check_after = NULL, updated_at = now()
			WHERE id = ?`, to.PlanID, to.Quantity, hold.Seconds(), licenseID).Exec(ctx); err != nil {
			return err
		}
		return syncSubscriptionPlanIn(ctx, tx, licenseID, to.PlanID)
	})
	return prev, err
}

// syncSubscriptionPlanIn moves the licence's subscription row to the plan
// the licence is now on. The row names the plan by a foreign key: one
// left on a plan the licence has left keeps that plan from being
// deleted, with nothing on it the admin could see or move.
func syncSubscriptionPlanIn(ctx context.Context, db bun.IDB, licenseID, planID string) error {
	_, err := db.NewRaw(`UPDATE subscriptions SET plan_id = ?, updated_at = now()
		WHERE license_id = ? AND plan_id <> ?`, planID, licenseID, planID).Exec(ctx)
	return err
}

// MarkLicenseRead moves the licence's read stamp to now: every read of
// its subscription that began before is out of date. A downgrade Stripe
// may have made marks it so before its hold can end, so that no read
// from before the change can put the old terms back.
func (s *Store) MarkLicenseRead(ctx context.Context, licenseID string) error {
	_, err := s.DB.NewRaw(`UPDATE licenses SET stripe_synced_at = GREATEST(stripe_synced_at, clock_timestamp()) WHERE id = ?`, licenseID).Exec(ctx)
	return err
}

// RestoreLicenseTerms puts back the terms LowerLicenseTerms replaced,
// for a change Stripe did not make, unless something has moved them
// since.
func (s *Store) RestoreLicenseTerms(ctx context.Context, licenseID string, lowered, prev LicenseTerms) error {
	return RunInTx(ctx, s.DB, func(ctx context.Context, tx bun.Tx) error {
		res, err := tx.NewRaw(`UPDATE licenses SET plan_id = ?, quantity = ?, stripe_synced_at = clock_timestamp(),
			terms_held_until = NULL, terms_check_after = NULL, updated_at = now()
			WHERE id = ? AND plan_id = ? AND quantity = ?`, prev.PlanID, prev.Quantity, licenseID, lowered.PlanID, lowered.Quantity).Exec(ctx)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil // moved since; its subscription row moved with it
		}
		return syncSubscriptionPlanIn(ctx, tx, licenseID, prev.PlanID)
	})
}

// stricterSeatLimit is the lower of two seat limits, where 0 means no
// limit at all.
func stricterSeatLimit(a, b int) int {
	switch {
	case a <= 0:
		return b
	case b <= 0:
		return a
	}
	return min(a, b)
}

// ActivateWithinLimit atomically creates an activation only if the limit hasn't been reached.
// Locks the license row (not activation rows) to serialize concurrent activations.
// ErrAlreadyActivated reports that this machine already holds an
// activation on this license, so nothing was taken from the limit.
var ErrAlreadyActivated = errors.New("identifier is already activated")

// ErrDeviceMoved is ErrAlreadyActivated for a device ActivateForKey has
// just moved from the licence's key to the member's.
var ErrDeviceMoved = fmt.Errorf("%w: moved to the member's key", ErrAlreadyActivated)

// ErrDeviceHeld is returned by ActivateForKey when the machine is the
// device of another key on the licence.
var ErrDeviceHeld = errors.New("device held by another key on the licence")

// ActivateWithinLimit takes one activation slot for a machine, or says
// why it could not.
//
// Every decision happens under the license row lock. The service also
// looks for an existing activation before calling, which handles the
// ordinary repeat, but that read is not in this transaction: two
// requests from the SAME machine could both miss it and arrive here
// together. Whichever way the limit fell, the second one was wrong.
// At the cap it counted the slot the first had just taken and answered
// "activation limit reached", telling a machine the license it already
// holds is full. Below the cap both passed the count and the second
// insert hit the unique index on (license_id, identifier), which
// surfaced as a 500. A client retrying a timed-out activation, or an
// app opened twice, met one or the other.
//
// It checks no key. Every path a client reaches goes through
// ActivateForKey, which re-checks the key it was given under the same
// lock; this one is for callers that hold none, such as tests.
func (s *Store) ActivateWithinLimit(ctx context.Context, act *model.Activation, maxActivations int) error {
	return s.activateWithinLimit(ctx, act, maxActivations, "")
}

// ActivateForKey is ActivateWithinLimit for a client presenting key, the
// member's key when act.SeatID is set, else the licence's own (see
// FindLicenseByKey). A key that stopped opening the licence since it was
// looked up (its member removed or their accept rolled back, the
// licence's key changed, the product's seats gone) is refused with
// ErrKeyRevoked: each of those takes the licence row lock this decides
// under, or is re-checked under it, so no device is left on a key that
// no longer works. A machine already
// activated stays whoever's activated it: another key holder's key is
// refused with ErrDeviceHeld, as for a floating session, while the
// licence's own key, which manages every device, finds it activated.
// The one move is a device activated with the licence's key before
// members had keys of their own (PredatesMemberKeys), when members
// used that key: it moves, once, to the member whose key activates it,
// so a member who used the shared key carries their machine onto their
// own and a change of the licence's key later leaves it alone.
func (s *Store) ActivateForKey(ctx context.Context, act *model.Activation, maxActivations int, key string) error {
	return s.activateWithinLimit(ctx, act, maxActivations, key)
}

func (s *Store) activateWithinLimit(ctx context.Context, act *model.Activation, maxActivations int, key string) error {
	if act.ID == "" {
		act.ID = newID()
	}

	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Lock the license row to serialize concurrent activations, and
	// hold the activation to the limit the licence has now: its seats
	// may have gone down since the caller read it.
	_, held, known, err := lockLicenseLimitsIn(ctx, tx, act.LicenseID)
	if err != nil {
		return err
	}
	if known {
		maxActivations = min(maxActivations, held)
	}

	if key != "" {
		held, err := keyHeldIn(ctx, tx, act.LicenseID, act.SeatID, key)
		if err != nil {
			return err
		}
		if !held {
			return ErrKeyRevoked
		}
	}

	// This machine may already hold a slot, in which case it is not
	// competing for one.
	var existing []struct {
		ID                 string         `bun:"id"`
		SeatID             sql.NullString `bun:"seat_id"`
		PredatesMemberKeys bool           `bun:"predates_member_keys"`
	}
	// FOR UPDATE: a release of the device, which takes no licence lock,
	// waits for this decision rather than slipping in between the read
	// and a move.
	if err := tx.NewRaw(
		"SELECT id, seat_id, predates_member_keys FROM activations WHERE license_id = ? AND identifier = ? FOR UPDATE",
		act.LicenseID, act.Identifier,
	).Scan(ctx, &existing); err != nil {
		return err
	}
	if len(existing) > 0 {
		act.ID = existing[0].ID
		holder := existing[0].SeatID
		switch {
		case key == "" || act.SeatID == "" || holder.String == act.SeatID:
			return ErrAlreadyActivated
		case holder.Valid || !existing[0].PredatesMemberKeys:
			return ErrDeviceHeld
		}
		res, err := tx.NewRaw(`UPDATE activations SET seat_id = ?, predates_member_keys = false
			WHERE id = ? AND seat_id IS NULL AND predates_member_keys`, act.SeatID, act.ID).Exec(ctx)
		if err != nil {
			return err
		}
		moved, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		if moved != 1 {
			return ErrAlreadyActivated
		}
		return ErrDeviceMoved
	}

	// Now safely count existing activations
	var count int
	err = tx.NewRaw(
		"SELECT COUNT(*) FROM activations WHERE license_id = ?",
		act.LicenseID,
	).Scan(ctx, &count)
	if err != nil {
		return err
	}

	if count >= maxActivations {
		return fmt.Errorf("activation limit reached")
	}

	_, err = tx.NewInsert().Model(act).Exec(ctx)
	if err != nil {
		return err
	}

	return tx.Commit()
}

// ─── Email Queue ───

type QueuedEmail struct {
	ID          string     `bun:"id,pk"`
	ToAddr      string     `bun:"to_addr"`
	Subject     string     `bun:"subject"`
	Body        string     `bun:"body"`
	Attempts    int        `bun:"attempts"`
	MaxAttempts int        `bun:"max_attempts"`
	Status      string     `bun:"status"`
	NextRetry   *time.Time `bun:"next_retry"`
	Error       string     `bun:"error"`
	// ClaimToken names the processor that holds this mail. Every
	// finishing write carries it, so one that stalled past its lease
	// cannot report on the mail somebody else has taken over.
	ClaimToken string `bun:"claim_token"`
}

func (s *Store) EnqueueEmail(ctx context.Context, to, subject, body string) error {
	_, err := s.DB.NewRaw(
		"INSERT INTO email_queue (id, to_addr, subject, body, max_attempts) VALUES (?, ?, ?, ?, 5)",
		newID(), to, subject, body,
	).Exec(ctx)
	return err
}

// emailClaimLease is how long a claimed mail stays invisible to other
// processors. One mail is claimed immediately before it is sent, so
// the lease has to cover a single send — two SMTP sessions of
// smtpSessionTimeout each, plus the dials — and no more: a processor
// killed mid-send holds the mail only until the lease runs out.
const emailClaimLease = 10 * time.Minute

// ClaimNextEmail takes one queued mail for this processor alone, or
// nil when the queue has nothing due. Every replica runs a processor,
// so reading pending rows and then sending them would deliver the
// same mail several times: the row is claimed in the same statement
// that reads it, and SKIP LOCKED hands the next caller a different
// one. The claim is a lease written into next_retry rather than a
// status change, so a processor that dies mid-send releases the mail
// instead of stranding it.
//
// One mail at a time on purpose: a batch would share one lease while
// the processor sends them one by one, and the last mail's lease
// could expire — and another replica send it — before its turn came.
func (s *Store) ClaimNextEmail(ctx context.Context) (*QueuedEmail, error) {
	var out []*QueuedEmail
	err := s.DB.NewRaw(`
		WITH claimed AS (
			SELECT id FROM email_queue
			 WHERE status = 'pending' AND (next_retry IS NULL OR next_retry <= now())
			 ORDER BY created_at ASC LIMIT 1
			 FOR UPDATE SKIP LOCKED
		)
		UPDATE email_queue q SET next_retry = now() + ?::interval, claim_token = ?
		  FROM claimed WHERE q.id = claimed.id
		RETURNING q.id, q.to_addr, q.subject, q.body, q.attempts, q.max_attempts, q.status, q.next_retry, q.error, q.claim_token`,
		emailClaimLease.String(), newID(),
	).Scan(ctx, &out)
	if err != nil || len(out) == 0 {
		return nil, err
	}
	return out[0], nil
}

// MarkEmailSent closes the mail this processor holds. The token is
// what it holds it by: a processor that stalled past its lease finds
// the row taken over and changes nothing.
func (s *Store) MarkEmailSent(ctx context.Context, id, token string) {
	// The body goes with it.
	//
	// These mails carry license keys: fulfilment, the maintenance
	// reminder, an admin resend. The queue is a delivery buffer, not
	// an archive, and a delivered mail has no further use for its
	// text, so keeping it would leave a credential in the database for
	// as long as the row lives, which is forever. What an operator
	// needs afterwards is who it went to, what it was about and when,
	// and those all stay.
	//
	// Rows written before this was added still hold their text. They
	// are left alone on purpose: licenses.license_key is itself still
	// plaintext until Phase C drops it, so clearing the queue's copy
	// today changes nothing about what a database dump exposes.
	// Clearing them belongs with Phase C, which would otherwise leave
	// this table as the last plaintext copy of every key it ever sent.
	res, err := s.DB.NewRaw(
		"UPDATE email_queue SET status = 'sent', sent_at = now(), attempts = attempts + 1, body = '' WHERE id = ? AND claim_token = ?",
		id, token,
	).Exec(ctx)
	if err != nil {
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		slog.Warn("email queue: a mail was taken over before this send finished", "email_id", id)
	}
}

// MarkEmailFailed records a failed attempt on the mail this processor
// holds, keyed by the same token as MarkEmailSent: a stalled
// processor must not push the row another one is sending back to
// pending, or reopen a reminder that has meanwhile gone out.
// MarkEmailDeferred reschedules a mail that failed for a reason only an
// operator can fix — a bad SMTP/Cloudflare credential, a wrong account
// id, an auth rejection — rather than anything wrong with the message
// itself. Unlike MarkEmailFailed it does NOT count against max_attempts
// and does NOT drop the body: the mail carries a licence key and must
// survive until the config is corrected, however long that takes.
// Otherwise a token rotation that lasted past the five backoff attempts
// (~15 minutes) would wipe licence mail for good. The row goes back to
// pending on a fixed delay; a later pass sends it once resolve() returns
// a working provider.
func (s *Store) MarkEmailDeferred(ctx context.Context, id, token, errMsg string) {
	_, err := s.DB.NewRaw(`
		UPDATE email_queue SET
			error = ?,
			status = 'pending',
			next_retry = now() + interval '10 minutes',
			claim_token = ''
		WHERE id = ? AND claim_token = ?
	`, errMsg, id, token).Exec(ctx)
	if err != nil {
		slog.Warn("email queue: could not defer mail", "email_id", id, "error", err)
	}
}

func (s *Store) MarkEmailFailed(ctx context.Context, id, token, errMsg string) {
	_ = RunInTx(ctx, s.DB, func(ctx context.Context, tx bun.Tx) error {
		var status, notificationID string
		err := tx.NewRaw(`
			UPDATE email_queue SET
				attempts = attempts + 1,
				error = ?,
				status = CASE WHEN attempts + 1 >= max_attempts THEN 'failed' ELSE 'pending' END,
				next_retry = CASE WHEN attempts + 1 < max_attempts THEN now() + (interval '1 minute' * power(2, attempts)) ELSE NULL END,
				claim_token = '',
				-- Kept while there are attempts left, because a retry
				-- sends this same text. Dropped once the mail is given
				-- up on, for the reason MarkEmailSent drops it: a row
				-- nobody will send again must not keep the license key
				-- it was carrying.
				body = CASE WHEN attempts + 1 >= max_attempts THEN '' ELSE body END
			WHERE id = ? AND claim_token = ?
			RETURNING status, notification_id
		`, errMsg, id, token).Scan(ctx, &status, &notificationID)
		if errors.Is(err, sql.ErrNoRows) {
			slog.Warn("email queue: a mail was taken over before this attempt finished", "email_id", id)
			return nil
		}
		if err != nil {
			return err
		}
		if status != "failed" || notificationID == "" {
			return nil
		}
		// The mail is given up on, so the reminder it carried was
		// never delivered: the claim that was closed when it was
		// queued is opened again, and a later pass sends it once the
		// mail server is back. A claim taken over meanwhile has a
		// different id and is left alone.
		_, err = tx.NewRaw(
			"UPDATE notifications SET sent_at = NULL, claimed_at = to_timestamp(0) WHERE id = ?", notificationID,
		).Exec(ctx)
		return err
	})
}

// MarkEmailFailedPermanent gives up on a mail immediately, without
// scheduling a retry. It is for failures that retrying cannot fix — a
// rejected request, bad credentials, a permanent bounce — where trying
// again just re-delivers to a bad address and burns sending reputation.
// The body is dropped for the same reason MarkEmailSent drops it: a row
// nobody will send again must not keep the credential it carried.
func (s *Store) MarkEmailFailedPermanent(ctx context.Context, id, token, errMsg string) {
	_, err := s.DB.NewRaw(`
		UPDATE email_queue SET
			attempts = attempts + 1,
			error = ?,
			status = 'failed',
			next_retry = NULL,
			claim_token = '',
			body = ''
		WHERE id = ? AND claim_token = ?
	`, errMsg, id, token).Exec(ctx)
	if err != nil {
		slog.Warn("email queue: could not mark mail permanently failed", "email_id", id, "error", err)
	}
}

// ClearStripeSubscription unlinks a licence from the Stripe
// subscription the caller confirmed is over, and reports whether it
// was still linked to that one.
//
// The expected id is part of the write: what the admin was shown, and
// what Stripe answered about, is the subscription being cut loose. If
// the licence has moved to another one in between, this changes
// nothing and says so, rather than detaching a subscription nobody
// asked about.
//
// NULL, not the empty string: the column carries a plain UNIQUE
// constraint, so a second licence cleared to an empty string would
// collide with the first one — while NULLs do not collide at all,
// which is why every licence without a subscription holds NULL.
func (s *Store) ClearStripeSubscription(ctx context.Context, licenseID, expectedSubID string) (bool, error) {
	res, err := s.DB.NewRaw(
		"UPDATE licenses SET stripe_subscription_id = NULL, stripe_pending_update_expires_at = NULL, terms_held_until = NULL, terms_check_after = NULL, updated_at = now() WHERE id = ? AND stripe_subscription_id = ?",
		licenseID, expectedSubID,
	).Exec(ctx)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// UpdateLicenseEmailByStripeCustomer updates email on all licenses for a Stripe customer.
func (s *Store) UpdateLicenseEmailByStripeCustomer(ctx context.Context, customerID, email string) error {
	_, err := s.DB.NewUpdate().Model((*model.License)(nil)).
		Set("email = ?", email).
		Set("updated_at = now()").
		Where("stripe_customer_id = ?", customerID).
		Exec(ctx)
	return err
}

// FindAllLicensesByStripeCustomer returns all licenses for a Stripe customer.
func (s *Store) FindAllLicensesByStripeCustomer(ctx context.Context, customerID string) ([]*model.License, error) {
	var out []*model.License
	err := s.DB.NewSelect().Model(&out).
		Relation("Plan").Relation("Product").
		Where("license.stripe_customer_id = ?", customerID).
		Scan(ctx)
	return out, err
}

// BackfillKeyHashes updates all licenses that don't have a key_hash yet.
func (s *Store) BackfillKeyHashes(ctx context.Context) error {
	var licenses []*model.License
	err := s.DB.NewSelect().Model(&licenses).
		Where("key_hash = ''").
		Scan(ctx)
	if err != nil {
		return err
	}
	for _, l := range licenses {
		l.KeyHash = license.HashKey(l.LicenseKey)
		_, err := s.DB.NewUpdate().Model(l).Column("key_hash").WherePK().Exec(ctx)
		if err != nil {
			return err
		}
	}
	return nil
}

// BackfillLicenseKeyEncrypted streams licenses where the encrypted column
// is NULL and populates it by encrypting the existing plaintext.
//
// Concurrency-safe: each UPDATE includes `AND license_key = ?` so a
// concurrent admin operation that rotates the key invalidates this
// backfill's update for that row (RowsAffected = 0). The next backfill
// run will re-process the row with the new plaintext.
//
// Idempotent: safe to call repeatedly. Each row is processed in a small
// SELECT/UPDATE batch (default 100) so the function can run in production
// without holding long locks. Progress + remaining are logged.
//
// No-op when LicenseKeyAEAD is nil (encryption is not configured).
//
// Per-row encrypt failures are logged + counted but DON'T abort the run —
// one bad row shouldn't stop the rest of the backfill. The function
// returns the count of successfully-encrypted rows, then the next
// invocation can retry the failures.
func (s *Store) BackfillLicenseKeyEncrypted(ctx context.Context, logger *slog.Logger) (int, error) {
	if s.LicenseKeyAEAD == nil {
		return 0, nil
	}
	if logger == nil {
		logger = slog.Default()
	}

	// Initial gauge snapshot so /metrics exposes the starting state.
	if remaining, err := s.countLicenseKeysUnencrypted(ctx); err == nil {
		LicenseKeysUnencrypted.Set(float64(remaining))
		if remaining > 0 {
			logger.Info("license key backfill starting", "remaining", remaining)
		}
	}

	const batchSize = 100
	total := 0
	skippedRaces := 0
	failed := 0
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		var batch []*model.License
		err := s.DB.NewSelect().Model(&batch).
			Where("license_key_encrypted IS NULL AND license_key <> ''").
			Limit(batchSize).
			Scan(ctx)
		if err != nil {
			return total, fmt.Errorf("select batch: %w", err)
		}
		if len(batch) == 0 {
			break
		}
		for _, l := range batch {
			ct, err := s.LicenseKeyAEAD.Encrypt([]byte(l.LicenseKey), []byte(l.ID))
			if err != nil {
				logger.Warn("license key backfill: encrypt failed; skipping",
					"license_id", l.ID, "error", err)
				failed++
				continue
			}
			// TOCTOU guard: only update if the plaintext we just encrypted is
			// still the current plaintext. If an admin rotated the key
			// between our SELECT and this UPDATE, RowsAffected = 0 and we
			// skip — the next backfill picks up the new plaintext.
			res, err := s.DB.NewUpdate().Model((*model.License)(nil)).
				Set("license_key_encrypted = ?", ct).
				Where("id = ? AND license_key = ? AND license_key_encrypted IS NULL", l.ID, l.LicenseKey).
				Exec(ctx)
			if err != nil {
				logger.Warn("license key backfill: update failed; skipping",
					"license_id", l.ID, "error", err)
				failed++
				continue
			}
			if n, _ := res.RowsAffected(); n == 0 {
				skippedRaces++
				continue
			}
			total++
		}
		logger.Info("license key backfill progress",
			"encrypted_total", total, "skipped_concurrent", skippedRaces, "failed", failed)
		// Refresh gauge so ops can watch progress live.
		if remaining, err := s.countLicenseKeysUnencrypted(ctx); err == nil {
			LicenseKeysUnencrypted.Set(float64(remaining))
		}
		// If batch was full, loop for the next one. Else we're done.
		if len(batch) < batchSize {
			break
		}
	}
	// Members' keys, made before encryption was turned on.
	members, memberFailures, err := s.backfillMemberKeysEncrypted(ctx, logger)
	total += members
	if members > 0 || memberFailures > 0 {
		logger.Info("member key backfill", "encrypted", members, "failed", memberFailures)
	}
	if remaining, err := s.countLicenseKeysUnencrypted(ctx); err == nil {
		LicenseKeysUnencrypted.Set(float64(remaining))
	}
	return total, err
}

// countLicenseKeysUnencrypted returns how many rows still need backfill,
// members' keys included. Used to drive the LicenseKeysUnencrypted
// gauge — Phase B should not flip the read path until this metric is 0
// for sustained time.
func (s *Store) countLicenseKeysUnencrypted(ctx context.Context) (int, error) {
	licences, err := s.DB.NewSelect().Model((*model.License)(nil)).
		Where("license_key_encrypted IS NULL AND license_key <> ''").
		Count(ctx)
	if err != nil {
		return 0, err
	}
	members, err := s.DB.NewSelect().Model((*model.Seat)(nil)).Where(unencryptedMemberKeys).Count(ctx)
	return licences + members, err
}

// ─── License Renewals (maintenance period) ───

// IsRenewalSessionConflict reports the unique-index violation raised
// when a second worker records the same renewal checkout session.
func IsRenewalSessionConflict(err error) bool {
	return err != nil && strings.Contains(err.Error(), "idx_license_renewals_session")
}

func (s *Store) FindLicenseRenewalBySession(ctx context.Context, sessionID string) (*model.LicenseRenewal, error) {
	r := new(model.LicenseRenewal)
	return r, s.DB.NewSelect().Model(r).Where("stripe_checkout_session_id = ?", sessionID).Scan(ctx)
}

func (s *Store) FindLicenseRenewalByPaymentIntent(ctx context.Context, paymentIntentID string) (*model.LicenseRenewal, error) {
	return FindLicenseRenewalByPaymentIntentIn(ctx, s.DB, paymentIntentID)
}

// FindLicenseRenewalByPaymentIntentIn looks the renewal up on a given
// connection or transaction. It takes no row lock: every path that
// writes renewals locks the license row first and the renewal rows
// after it, and a lock taken here in the other order could deadlock
// against an admin edit. Callers that change the row re-read it under
// the license lock (RevertLicenseRenewalIn).
func FindLicenseRenewalByPaymentIntentIn(ctx context.Context, db bun.IDB, paymentIntentID string) (*model.LicenseRenewal, error) {
	r := new(model.LicenseRenewal)
	return r, db.NewSelect().Model(r).Where("stripe_payment_intent_id = ?", paymentIntentID).Scan(ctx)
}

// ListLicenseRenewals returns a license's renewal ledger, oldest
// first, refunded rows included.
func (s *Store) ListLicenseRenewals(ctx context.Context, licenseID string) ([]*model.LicenseRenewal, error) {
	var out []*model.LicenseRenewal
	err := s.DB.NewSelect().Model(&out).Where("license_id = ?", licenseID).
		OrderExpr("created_at ASC, id ASC").Scan(ctx)
	return out, err
}

// ErrRenewalIneligible: the license no longer has a finite update
// period on a perpetual plan, so a paid renewal cannot be applied.
// It is not recorded as fulfilled; the payment needs an operator.
var ErrRenewalIneligible = errors.New("license has no finite update period to extend")

// ApplyLicenseRenewal records a paid renewal of r.Days and moves the
// license's updates_until in one transaction; see ApplyLicenseRenewalIn.
func (s *Store) ApplyLicenseRenewal(ctx context.Context, r *model.LicenseRenewal) error {
	return RunInTx(ctx, s.DB, func(ctx context.Context, tx bun.Tx) error {
		return ApplyLicenseRenewalIn(ctx, tx, r)
	})
}

// RunInTx runs fn in one transaction of this store.
func (s *Store) RunInTx(ctx context.Context, fn func(ctx context.Context, tx bun.Tx) error) error {
	return RunInTx(ctx, s.DB, fn)
}

// RunInTx runs fn in a transaction on db and commits when it returns
// nil; any error rolls the transaction back.
func RunInTx(ctx context.Context, db *bun.DB, fn func(ctx context.Context, tx bun.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(ctx, tx); err != nil {
		return err
	}
	return commitTx(ctx, tx)
}

// commitTx commits, first making sure the transaction can still commit.
// pgdriver answers nil to a COMMIT that Postgres turned into a rollback,
// so a failed statement whose error was dropped would read as a write
// that landed. Any statement on an aborted transaction fails (25P02);
// this one costs a round trip and nothing else.
func commitTx(ctx context.Context, tx bun.Tx) error {
	if _, err := tx.ExecContext(ctx, "SELECT 1"); err != nil {
		return fmt.Errorf("transaction cannot commit: %w", err)
	}
	return tx.Commit()
}

// SetFeedURLTTL records how long this install signs the presigned
// links inside a public feed for (STORAGE_FEED_URL_TTL). The store
// needs it for the same reason the admin API does: a licence with a
// finite update period may only be written once the links the public
// feed handed out have expired, and that is how long they live.
func (s *Store) SetFeedURLTTL(d time.Duration) { s.feedTTL = d }

// feedURLTTL falls back to the configured default, so a store built
// without one is conservative rather than instant.
func (s *Store) feedURLTTL() time.Duration {
	if s.feedTTL > 0 {
		return s.feedTTL
	}
	return defaultFeedURLTTL
}

// defaultFeedURLTTL mirrors the STORAGE_FEED_URL_TTL default.
const defaultFeedURLTTL = 24 * time.Hour

// ErrPlanChanged: the plan a license was built from has since
// changed its license type. Everything about the row follows from
// that type — status, valid_until, the update period, whether a
// subscription row belongs to it — so the half-built license is
// refused rather than written. Reading the plan again and rebuilding
// is the fix, which is what a webhook retry does.
var ErrPlanChanged = errors.New("the plan changed type while this license was being created")

// ErrUpdatePeriodNotEnforceable: the license was to be written with a
// finite update period the product cannot enforce yet — its update
// feeds are public, or they were gated so recently that what the
// public feed handed out still works. Writing the period anyway would
// sell a cutoff the customer can walk around; writing the license
// without it would grant more than was sold. The caller leaves the
// paid purchase pending instead, and the reason travels with the
// error so it can say which of the two it is.
var ErrUpdatePeriodNotEnforceable = errors.New("update period is not enforceable for this product")

// LockLicenseIn, LockPlanIn and LockProductIn take the row lock a
// write of that row would take, so a caller can hold it before
// reaching for the feed gating lock. Order matters: the triggers take
// the gating lock while the row they fire for is already locked, so
// every writer takes the row first and the gate second.
func LockLicenseIn(ctx context.Context, tx bun.IDB, id string) (*model.License, error) {
	lic := new(model.License)
	return lic, tx.NewSelect().Model(lic).Where("id = ?", id).For("UPDATE").Scan(ctx)
}

func LockPlanIn(ctx context.Context, tx bun.IDB, id string) error {
	return tx.NewSelect().Model((*model.Plan)(nil)).Column("id").Where("id = ?", id).For("UPDATE").Scan(ctx, new(string))
}

func LockProductIn(ctx context.Context, tx bun.IDB, id string) (*model.Product, error) {
	p := new(model.Product)
	return p, tx.NewSelect().Model(p).Where("id = ?", id).For("UPDATE").Scan(ctx)
}

// LockReferencedRowsIn takes the row locks an insert's foreign keys
// would take anyway — the plan and the product a license points at —
// before its caller reaches for the feed gating lock.
//
// Order is the whole point. Writers that touch a product's gating
// state take the row first and the gate second (see LockLicenseIn):
// an insert that took the gate first and then met the plan's or the
// product's key lock inside its own INSERT would be waiting for a row
// while holding the gate a row-holder is waiting for, and Postgres
// would abort one of them — a paid fulfilment or an admin edit,
// depending on which lost.
func LockReferencedRowsIn(ctx context.Context, db bun.IDB, planID, productID string) error {
	if planID != "" {
		var id string
		if err := db.NewRaw("SELECT id FROM plans WHERE id = ? FOR KEY SHARE", planID).Scan(ctx, &id); err != nil {
			return err
		}
	}
	if productID != "" {
		var id string
		if err := db.NewRaw("SELECT id FROM products WHERE id = ? FOR KEY SHARE", productID).Scan(ctx, &id); err != nil {
			return err
		}
	}
	return nil
}

// FeedGatingLockIn takes the product's feed gating lock for this
// transaction — the lock the triggers take — so the gating state read
// after it cannot change before the transaction commits.
func FeedGatingLockIn(ctx context.Context, tx bun.IDB, productID string) error {
	_, err := tx.NewRaw("SELECT feed_gating_lock(?)", productID).Exec(ctx)
	return err
}

// ledgerStamp is the instant a renewal row is recorded at. The ledger
// is replayed in created_at order and each row's dates are recomputed
// from its own stamp, so the order it records must be the order the
// renewals were actually applied in. Taken here — after the caller
// has the license row lock, and from the database clock rather than
// the replica's — because a time read before the lock can be
// overtaken: the transaction that computed it first may commit
// second, and two replicas need not agree on the clock at all. Rows
// for one license are written under that lock, so their stamps are
// strictly ordered.
func ledgerStamp(ctx context.Context, tx bun.IDB) (time.Time, error) {
	var t time.Time
	err := tx.NewRaw("SELECT clock_timestamp()").Scan(ctx, &t)
	return t, err
}

// ApplyLicenseRenewalIn records a paid renewal of r.Days and moves the
// license's updates_until, both on the given transaction, so a crash
// between the two can never leave a renewal that was paid for but not
// applied, or applied but not recorded. The new end is computed under
// a row lock from the end the license has at that moment, so two
// renewals applied at once stack instead of overwriting each other.
// Eligibility is checked under the same lock: a license that
// meanwhile got updates for life or left its perpetual plan has
// nothing to extend, and the renewal is refused with
// ErrRenewalIneligible rather than recorded as fulfilled for no
// benefit. The computed dates are written back into r.
func ApplyLicenseRenewalIn(ctx context.Context, tx bun.IDB, r *model.LicenseRenewal) error {
	if r.ID == "" {
		r.ID = newID()
	}
	lic := new(model.License)
	if err := tx.NewSelect().Model(lic).Where("id = ?", r.LicenseID).For("UPDATE").Scan(ctx); err != nil {
		return err
	}
	now, err := ledgerStamp(ctx, tx)
	if err != nil {
		return err
	}
	r.CreatedAt = now
	var planType string
	if err := tx.NewRaw("SELECT license_type FROM plans WHERE id = ?", lic.PlanID).Scan(ctx, &planType); err != nil {
		return err
	}
	// The portal only offers a renewal on an active licence, but the
	// session stays payable for hours: one suspended, expired or
	// revoked since then has nothing worth extending, and the
	// customer could not use the updates anyway.
	if lic.UpdatesUntil == nil || planType != "perpetual" || lic.Status != model.StatusActive {
		return ErrRenewalIneligible
	}
	r.PreviousUpdatesUntil = lic.UpdatesUntil
	end := model.RenewedUpdatesUntil(lic.UpdatesUntil, now, r.Days)
	r.UpdatesUntil = &end
	if _, err := tx.NewInsert().Model(r).Exec(ctx); err != nil {
		return err
	}
	_, err = tx.NewUpdate().Model((*model.License)(nil)).
		Set("updates_until = ?", r.UpdatesUntil).
		Set("updated_at = now()").
		Where("id = ?", r.LicenseID).Exec(ctx)
	return err
}

// RecordRefundedRenewal writes a renewal that was refunded before it
// could be applied; see RecordRefundedRenewalIn.
func (s *Store) RecordRefundedRenewal(ctx context.Context, r *model.LicenseRenewal) error {
	return RunInTx(ctx, s.DB, func(ctx context.Context, tx bun.Tx) error {
		return RecordRefundedRenewalIn(ctx, tx, r)
	})
}

// RecordRefundedRenewalIn writes a renewal that was refunded before it
// could be applied. It changes nothing on the license and only keeps
// the ledger complete, so retries of the session see it as done. The
// row still captures the end the license had at that moment: the
// ledger replay starts from the first row's previous end, and a nil
// there would read as "updates for life" once later renewals are
// refunded.
func RecordRefundedRenewalIn(ctx context.Context, tx bun.IDB, r *model.LicenseRenewal) error {
	if r.ID == "" {
		r.ID = newID()
	}
	lic := new(model.License)
	if err := tx.NewSelect().Model(lic).Where("id = ?", r.LicenseID).For("UPDATE").Scan(ctx); err != nil {
		return err
	}
	now, err := ledgerStamp(ctx, tx)
	if err != nil {
		return err
	}
	r.PreviousUpdatesUntil = lic.UpdatesUntil
	r.CreatedAt, r.RefundedAt, r.UpdatesUntil = now, &now, nil
	_, err = tx.NewInsert().Model(r).Exec(ctx)
	return err
}

// RevertLicenseRenewal takes a refunded renewal back out of the
// license's maintenance period; see RevertLicenseRenewalIn.
func (s *Store) RevertLicenseRenewal(ctx context.Context, r *model.LicenseRenewal) error {
	return RunInTx(ctx, s.DB, func(ctx context.Context, tx bun.Tx) error {
		return RevertLicenseRenewalIn(ctx, tx, r)
	})
}

// RevertLicenseRenewalIn takes a refunded renewal back out of the
// license's maintenance period. The new end is replayed from the
// ledger without this renewal, so refunds in any order land on the
// same dates a customer who never bought them would have. If the
// license's current end is not what the ledger predicts, an admin
// edited it since; then only this renewal's days are subtracted so
// the edit survives. A license with updates for life, or a renewal
// from a previous period, is left alone. Marks the renewal refunded;
// a second call is a no-op.
//
// Lock order is license row first, renewal rows after — the same
// order the admin edits use — so the two can never deadlock. The
// renewal is re-read under that lock, since it was looked up before.
func RevertLicenseRenewalIn(ctx context.Context, tx bun.IDB, r *model.LicenseRenewal) error {
	if r.RefundedAt != nil {
		return nil
	}
	lic := new(model.License)
	if err := tx.NewSelect().Model(lic).Where("id = ?", r.LicenseID).For("UPDATE").Scan(ctx); err != nil {
		return err
	}
	if err := tx.NewSelect().Model(r).Where("id = ?", r.ID).Scan(ctx); err != nil {
		return err
	}
	if r.RefundedAt != nil {
		return nil
	}
	// On the ledger's clock, as created_at is: the replay compares a
	// refund with the purchases after it.
	now, err := ledgerStamp(ctx, tx)
	if err != nil {
		return err
	}
	// Only the current period's ledger: renewals from before a reset
	// are recorded as refunded but move nothing.
	var ledger []*model.LicenseRenewal
	if err := tx.NewSelect().Model(&ledger).Where("license_id = ? AND superseded_at IS NULL", r.LicenseID).
		OrderExpr("created_at ASC, id ASC").Scan(ctx); err != nil {
		return err
	}
	if lic.UpdatesUntil != nil && r.UpdatesUntil != nil && r.SupersededAt == nil {
		predicted := model.ReplayRenewals(ledger)
		for _, row := range ledger {
			if row.ID == r.ID {
				row.RefundedAt = &now
			}
		}
		without := model.ReplayRenewals(ledger)
		var restored *time.Time
		switch {
		case predicted != nil && without != nil:
			// Whatever the admin moved the end by since the ledger
			// last agreed with it is kept as an offset on the replay
			// without this renewal. Subtracting the renewal's days
			// would be wrong for a renewal that revived a lapsed
			// period: its effect also covered the lapse.
			delta := lic.UpdatesUntil.Sub(*predicted)
			t := without.Add(delta)
			restored = &t
		default:
			t := lic.UpdatesUntil.Add(-time.Duration(r.Days) * 24 * time.Hour)
			restored = &t
		}
		if _, err := tx.NewUpdate().Model((*model.License)(nil)).
			Set("updates_until = ?", restored).
			Set("updated_at = now()").
			Where("id = ?", r.LicenseID).Exec(ctx); err != nil {
			return err
		}
	}
	if _, err := tx.NewUpdate().Model((*model.LicenseRenewal)(nil)).
		Set("refunded_at = ?", now).
		Where("id = ? AND refunded_at IS NULL", r.ID).Exec(ctx); err != nil {
		return err
	}
	r.RefundedAt = &now
	return nil
}

// UpdateLicenseAndSupersedeRenewals writes the given license columns
// and closes the renewal ledger in one transaction. Used when the
// maintenance period is reset outside the ledger — the license leaves
// or re-enters a perpetual plan, or is granted updates for life — so
// refunds of earlier renewals cannot reach the new period.
func (s *Store) UpdateLicenseAndSupersedeRenewals(ctx context.Context, l *model.License, cols ...string) error {
	return RunInTx(ctx, s.DB, func(ctx context.Context, tx bun.Tx) error {
		return UpdateLicenseAndSupersedeRenewalsIn(ctx, tx, l, cols...)
	})
}

// UpdateLicenseAndSupersedeRenewalsIn does the same on a caller's
// transaction.
func UpdateLicenseAndSupersedeRenewalsIn(ctx context.Context, db bun.IDB, l *model.License, cols ...string) error {
	if err := UpdateLicenseIn(ctx, db, l, cols...); err != nil {
		return err
	}
	_, err := db.NewUpdate().Model((*model.LicenseRenewal)(nil)).
		Set("superseded_at = now()").
		Where("license_id = ? AND superseded_at IS NULL", l.ID).Exec(ctx)
	return err
}

// SetLicenseUpdatesUntil is the admin's edit of a license's period
// end, done under the row lock the renewal path uses. It applies only
// if the license still has the end the admin saw (expected); a paid
// renewal committed meanwhile makes it return false so the edit is
// redone on the current value instead of silently overwriting the
// extension. supersede closes the renewal ledger (lifetime grant).
func (s *Store) SetLicenseUpdatesUntil(ctx context.Context, licenseID string, expected, next *time.Time, supersede bool) (bool, error) {
	applied := false
	err := RunInTx(ctx, s.DB, func(ctx context.Context, tx bun.Tx) error {
		var err error
		applied, err = SetLicenseUpdatesUntilIn(ctx, tx, licenseID, expected, next, supersede)
		return err
	})
	return applied, err
}

// SetLicenseUpdatesUntilIn is SetLicenseUpdatesUntil on a caller's
// transaction, for writers that must hold a lock across the check and
// the write.
func SetLicenseUpdatesUntilIn(ctx context.Context, tx bun.IDB, licenseID string, expected, next *time.Time, supersede bool) (bool, error) {
	lic := new(model.License)
	if err := tx.NewSelect().Model(lic).Where("id = ?", licenseID).For("UPDATE").Scan(ctx); err != nil {
		return false, err
	}
	if !model.SameEnd(lic.UpdatesUntil, expected) {
		return false, nil
	}
	// The admin decided this period, including deciding there is none.
	// Without the mark a cleared cutoff reads as "nobody decided yet"
	// — the state a replica that predates the column leaves — and the
	// next write that carries plan_id, even to the same plan, would
	// have the trigger fill the plan's period back in and take the
	// grant of updates for life away again.
	if _, err := tx.NewUpdate().Model((*model.License)(nil)).
		Set("updates_until = ?", next).
		Set("updates_terms_set = true").
		Set("updated_at = now()").
		Where("id = ?", licenseID).Exec(ctx); err != nil {
		return false, err
	}
	if supersede {
		if _, err := tx.NewUpdate().Model((*model.LicenseRenewal)(nil)).
			Set("superseded_at = now()").
			Where("license_id = ? AND superseded_at IS NULL", licenseID).Exec(ctx); err != nil {
			return false, err
		}
	}
	return true, nil
}

// FindLicensesWithUpdatesEnding lists active perpetual licenses whose
// maintenance period ends inside [from, to], with product and plan
// loaded for the reminder email.
func (s *Store) FindLicensesWithUpdatesEnding(ctx context.Context, from, to time.Time) ([]*model.License, error) {
	var out []*model.License
	err := s.DB.NewSelect().Model(&out).
		Relation("Product").
		Relation("Plan").
		Where("license.status = 'active'").
		Where("plan.license_type = 'perpetual'").
		Where("license.updates_until IS NOT NULL").
		Where("license.updates_until >= ?", from).
		Where("license.updates_until <= ?", to).
		OrderExpr("license.updates_until ASC").
		Scan(ctx)
	return out, err
}
