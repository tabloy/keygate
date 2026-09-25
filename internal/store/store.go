package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
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
	return &Store{DB: db}, nil
}

func (s *Store) Close() error { return s.DB.Close() }

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
	if _, err := s.DB.ExecContext(ctx, "SELECT pg_advisory_lock(7367616)"); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	defer func() {
		_, _ = s.DB.ExecContext(ctx, "SELECT pg_advisory_unlock(7367616)")
	}()

	// Verify checksums of previously applied migrations
	var existing []struct {
		Filename string `bun:"filename"`
		Checksum string `bun:"checksum"`
	}
	_ = s.DB.NewRaw("SELECT filename, checksum FROM schema_migrations ORDER BY filename").Scan(ctx, &existing)
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
		migCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)

		tx, err := s.DB.BeginTx(migCtx, nil)
		if err != nil {
			cancel()
			return fmt.Errorf("begin tx for %s: %w", entry.Name(), err)
		}

		execErr := func() error {
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
		}()

		cancel()

		if execErr != nil {
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

func (s *Store) UpsertUser(ctx context.Context, u *model.User) error {
	if u.ID == "" {
		u.ID = newID()
	}
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
	return u, s.DB.NewSelect().Model(u).Where("email = ?", email).Scan(ctx)
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

// SetUserRole updates a user's role. Only owners can promote/demote.
func (s *Store) SetUserRole(ctx context.Context, userID, role string) error {
	if role != model.RoleOwner && role != model.RoleAdmin && role != model.RoleUser {
		return fmt.Errorf("invalid role: %s", role)
	}
	_, err := s.DB.NewUpdate().Model((*model.User)(nil)).
		Set("role = ?, updated_at = now()", role).
		Where("id = ?", userID).Exec(ctx)
	return err
}

// CreatePlaceholderUser creates a user with minimal info for team invites.
// The user will get proper name when they first log in.
func (s *Store) CreatePlaceholderUser(ctx context.Context, email, role string) error {
	u := &model.User{
		ID:    newID(),
		Email: email,
		Name:  "",
		Role:  role,
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

// ErrLastOwner is returned by DemoteOwnerAtomic when the demotion
// would leave zero owners. Locked-by-design: callers must NOT bypass
// this check with a raw SetUserRole call from the handler layer.
var ErrLastOwner = errors.New("cannot remove the last owner")

// DemoteOwnerAtomic locks every owner row in a tx, recounts, and
// demotes the target only if at least one owner would remain.
//
// Why a tx with FOR UPDATE? The original handler did
//  1. CountOwners()  (no lock)
//  2. SetUserRole(target, "user")  (separate stmt)
//
// Two concurrent demotions of two DIFFERENT owners — when the org
// has exactly 2 owners — could both pass step 1 (each sees count=2,
// "OK to remove one"), then both step 2 → zero owners. We've seen
// this in production-like load testing.
//
// FOR UPDATE on the owner rows serialises the two demotions: the
// second one waits, re-counts after the first has committed (sees
// count=1), and rejects with ErrLastOwner.
//
// targetID may or may not currently be an owner — if it's not, the
// UPDATE is a no-op (0 rows affected) and we return nil, matching
// the previous handler-side semantics (caller already verified the
// target was admin/owner before reaching us).
func (s *Store) DemoteOwnerAtomic(ctx context.Context, targetID string) error {
	// Acquire a session-scoped advisory lock OUTSIDE the tx so the
	// lock is held by the connection itself. Without this, when
	// bun's pool hands two concurrent calls different connections,
	// FOR UPDATE on per-row owner locks only serialises demotions
	// of the SAME target — two demotions of A and B then both
	// pass count==2 and zero owners remain.
	//
	// pg_advisory_lock(N) blocks until acquired. We release it in
	// a defer; if the process crashes the session ends and Postgres
	// reaps the lock automatically.
	conn, err := s.DB.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.NewRaw("SELECT pg_advisory_lock(8675310)").Exec(ctx); err != nil {
		return err
	}
	defer func() {
		_, _ = conn.NewRaw("SELECT pg_advisory_unlock(8675310)").Exec(context.Background())
	}()

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	var ownerCount int
	if err := tx.NewRaw("SELECT COUNT(*) FROM users WHERE role = 'owner'").Scan(ctx, &ownerCount); err != nil {
		return err
	}

	// Determine the target's current role. If they're an owner AND
	// they're the last one, refuse. If they're admin (not owner) we
	// can demote freely — the last-owner invariant is unaffected.
	var targetRole string
	if err := tx.NewRaw("SELECT role FROM users WHERE id = ?", targetID).Scan(ctx, &targetRole); err != nil {
		return err
	}
	if targetRole == model.RoleOwner && ownerCount <= 1 {
		return ErrLastOwner
	}

	if _, err := tx.NewRaw(
		"UPDATE users SET role = ?, updated_at = now() WHERE id = ?",
		model.RoleUser, targetID,
	).Exec(ctx); err != nil {
		return err
	}

	return tx.Commit()
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

func (s *Store) FindLicenseByKey(ctx context.Context, key string) (*model.License, error) {
	keyHash := license.HashKey(key)
	l := new(model.License)
	err := s.DB.NewSelect().Model(l).
		Relation("Product").
		Relation("Plan").
		Relation("Plan.Entitlements").
		Relation("Activations").
		Where("license.key_hash = ?", keyHash).
		Scan(ctx)
	if err != nil {
		// Fallback to plaintext for un-migrated keys — use fresh model
		// to avoid mixing partial state from the failed hash lookup.
		l = new(model.License)
		return l, s.DB.NewSelect().Model(l).
			Relation("Product").
			Relation("Plan").
			Relation("Plan.Entitlements").
			Relation("Activations").
			Where("license.license_key = ?", key).
			Scan(ctx)
	}
	return l, nil
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
	return s.updateLicenseAndSubscription(ctx, lic, false, cols...)
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
	return s.updateLicenseAndSubscription(ctx, lic, true, cols...)
}

func (s *Store) updateLicenseAndSubscription(ctx context.Context, lic *model.License, stillLinked bool, cols ...string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	lic.UpdatedAt = time.Now()
	allCols := make([]string, len(cols)+1)
	copy(allCols, cols)
	allCols[len(cols)] = "updated_at"
	q := tx.NewUpdate().Model(lic).Column(allCols...).WherePK()
	guarded := stillLinked && lic.StripeSubscriptionID != ""
	if guarded {
		q = q.Where("stripe_subscription_id = ?", lic.StripeSubscriptionID)
	}
	res, err := q.Exec(ctx)
	if err != nil {
		return err
	}
	if guarded {
		if n, err := res.RowsAffected(); err == nil && n == 0 {
			return ErrSubscriptionUnlinked
		}
	}

	// Sync subscription status if one exists
	if _, err := tx.NewRaw(`
		UPDATE subscriptions SET status = ?, updated_at = now()
		WHERE license_id = ? AND status != ?
	`, lic.Status, lic.ID, lic.Status).Exec(ctx); err != nil {
		// Non-fatal: subscription may not exist
		slog.Warn("sync subscription status failed", "license_id", lic.ID, "error", err)
	}

	return tx.Commit()
}

func (s *Store) ListLicensesByEmail(ctx context.Context, email string) ([]*model.License, error) {
	var out []*model.License
	// Include licenses owned by email OR where user has a seat
	err := s.DB.NewSelect().Model(&out).
		Relation("Plan").Relation("Plan.Entitlements").
		Relation("Product").Relation("Activations").Relation("Seats").
		Where("license.email = ? OR license.id IN (SELECT license_id FROM seats WHERE email = ? AND removed_at IS NULL)", email, email).
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
		SELECT 1 FROM users WHERE lower(email) = ?
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
	if f.Search != "" {
		// Only search by email and key prefix — never expose full key via wildcard search.
		q = q.Where("(license.email ILIKE ? OR license.license_key LIKE ?)", "%"+f.Search+"%", f.Search+"%")
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

func (s *Store) DeleteActivation(ctx context.Context, id string) error {
	_, err := s.DB.NewDelete().Model((*model.Activation)(nil)).Where("id = ?", id).Exec(ctx)
	return err
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
	go func() {
		auditCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := s.DB.NewInsert().Model(log).Exec(auditCtx); err != nil {
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
func (s *Store) FindExpiredTrials(ctx context.Context) ([]*model.License, error) {
	var out []*model.License
	err := s.DB.NewSelect().Model(&out).
		Relation("Product").Relation("Plan").
		Where("license.status = 'trialing'").
		Where("license.valid_until IS NOT NULL").
		Where("license.valid_until < ?", time.Now()).
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

// HasNotification checks if a notification with the given tag was already sent for a license.
func (s *Store) HasNotification(ctx context.Context, licenseID, tag string) bool {
	exists, _ := s.DB.NewSelect().
		TableExpr("notifications").
		Where("license_id = ? AND tag = ?", licenseID, tag).
		Exists(ctx)
	return exists
}

// RecordNotification records that a notification was sent for a license.
func (s *Store) RecordNotification(ctx context.Context, licenseID, tag string) {
	_, _ = s.DB.NewRaw(
		"INSERT INTO notifications (id, license_id, tag) VALUES (?, ?, ?) ON CONFLICT (license_id, tag) DO NOTHING",
		newID(), licenseID, tag,
	).Exec(ctx)
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

// RotateRefreshToken atomically marks the token as revoked and
// returns the row. Distinguishes three outcomes:
//
//   - (rt, nil)                    → caller may issue a new token
//   - (rt, ErrRefreshTokenReused)  → REUSE detected; caller MUST
//     wipe every refresh_token for rt.UserID. The returned rt
//     carries the UserID so the caller can do the wipe in one step.
//   - (nil, sql.ErrNoRows)         → token not found / expired;
//     plain 401, no family wipe.
//
// Implemented as a single UPDATE … RETURNING wrapped in a tx that
// takes a SELECT FOR UPDATE on the row first. This serialises
// concurrent rotation attempts of the same token (e.g. two browser
// tabs both refreshing at once) so exactly one wins.
func (s *Store) RotateRefreshToken(ctx context.Context, tokenHash string) (*RefreshToken, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck

	rt := new(RefreshToken)
	scanErr := tx.NewRaw(
		"SELECT id, user_id, token_hash, expires_at, revoked_at FROM refresh_tokens "+
			"WHERE token_hash = ? AND expires_at > now() FOR UPDATE",
		tokenHash,
	).Scan(ctx, rt)
	if scanErr != nil {
		return nil, scanErr
	}

	// Already-rotated token replayed → security incident.
	if rt.RevokedAt != nil {
		_ = tx.Commit() // commit the FOR UPDATE release — no row change
		return rt, ErrRefreshTokenReused
	}

	now := time.Now()
	if _, err := tx.NewRaw(
		"UPDATE refresh_tokens SET revoked_at = ? WHERE id = ?",
		now, rt.ID,
	).Exec(ctx); err != nil {
		return nil, err
	}
	rt.RevokedAt = &now
	return rt, tx.Commit()
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
		Where("attempts < 5").
		OrderExpr("created_at DESC").
		Limit(1).
		Scan(ctx)
	return otp, err
}

func (s *Store) IncrementOTPAttempts(ctx context.Context, id string) error {
	_, err := s.DB.NewUpdate().Model((*model.OTPCode)(nil)).
		Set("attempts = attempts + 1").
		Where("id = ?", id).
		Exec(ctx)
	return err
}

func (s *Store) MarkOTPUsed(ctx context.Context, id string) error {
	_, err := s.DB.NewUpdate().Model((*model.OTPCode)(nil)).
		Set("used = true").
		Where("id = ?", id).
		Exec(ctx)
	return err
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
	return tx.Commit()
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

// ActivateWithinLimit atomically creates an activation only if the limit hasn't been reached.
// Locks the license row (not activation rows) to serialize concurrent activations.
// ErrAlreadyActivated reports that this machine already holds an
// activation on this license, so nothing was taken from the limit.
var ErrAlreadyActivated = errors.New("identifier is already activated")

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
func (s *Store) ActivateWithinLimit(ctx context.Context, act *model.Activation, maxActivations int) error {
	if act.ID == "" {
		act.ID = newID()
	}

	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Lock the license row to serialize concurrent activations
	_, err = tx.NewRaw("SELECT id FROM licenses WHERE id = ? FOR UPDATE", act.LicenseID).Exec(ctx)
	if err != nil {
		return err
	}

	// This machine may already hold a slot, in which case it is not
	// competing for one.
	var existingID string
	switch err := tx.NewRaw(
		"SELECT id FROM activations WHERE license_id = ? AND identifier = ?",
		act.LicenseID, act.Identifier,
	).Scan(ctx, &existingID); {
	case err == nil && existingID != "":
		act.ID = existingID
		return ErrAlreadyActivated
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		return err
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
		"UPDATE licenses SET stripe_subscription_id = NULL, updated_at = now() WHERE id = ? AND stripe_subscription_id = ?",
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
func (s *Store) UpdateLicenseEmailByStripeCustomer(ctx context.Context, customerID, email string) {
	_, _ = s.DB.NewUpdate().Model((*model.License)(nil)).
		Set("email = ?", email).
		Set("updated_at = now()").
		Where("stripe_customer_id = ?", customerID).
		Exec(ctx)
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
	return total, nil
}

// countLicenseKeysUnencrypted returns how many rows still need backfill.
// Used to drive the LicenseKeysUnencrypted gauge — Phase B should not
// flip the read path until this metric is 0 for sustained time.
func (s *Store) countLicenseKeysUnencrypted(ctx context.Context) (int, error) {
	return s.DB.NewSelect().Model((*model.License)(nil)).
		Where("license_key_encrypted IS NULL AND license_key <> ''").
		Count(ctx)
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
	// Only the current period's ledger: renewals from before a reset
	// are recorded as refunded but move nothing.
	var ledger []*model.LicenseRenewal
	if err := tx.NewSelect().Model(&ledger).Where("license_id = ? AND superseded_at IS NULL", r.LicenseID).
		OrderExpr("created_at ASC, id ASC").Scan(ctx); err != nil {
		return err
	}
	now := time.Now()
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
