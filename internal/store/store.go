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
	role := u.Role
	if role == "" {
		role = model.RoleUser
	}
	_, err := s.DB.NewRaw(`
		INSERT INTO users (id, email, name, avatar_url, role, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, now(), now())
		ON CONFLICT (email) DO UPDATE SET
			name = CASE WHEN EXCLUDED.name != '' THEN EXCLUDED.name ELSE users.name END,
			avatar_url = CASE WHEN EXCLUDED.avatar_url != '' THEN EXCLUDED.avatar_url ELSE users.avatar_url END,
			updated_at = now()
	`, u.ID, u.Email, u.Name, u.AvatarURL, role).Exec(ctx)
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
func (s *Store) ListAdmins(ctx context.Context) ([]*model.User, error) {
	var out []*model.User
	err := s.DB.NewSelect().Model(&out).
		Where("role IN ('owner', 'admin')").
		OrderExpr("created_at ASC").Scan(ctx)
	return out, err
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

	if _, err := tx.NewInsert().Model(l).Exec(ctx); err != nil {
		return err
	}

	if plan != nil && (plan.LicenseType == "subscription" || plan.LicenseType == "trial") {
		sub := &model.Subscription{
			ID:        newID(),
			LicenseID: l.ID,
			PlanID:    plan.ID,
			Status:    l.Status,
		}
		if plan.LicenseType == "trial" && plan.TrialDays > 0 {
			now := time.Now()
			sub.TrialStart = &now
			until := now.Add(time.Duration(plan.TrialDays) * 24 * time.Hour)
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

func (s *Store) FindLicenseByStripeCustomer(ctx context.Context, customerID string) (*model.License, error) {
	l := new(model.License)
	return l, s.DB.NewSelect().Model(l).
		Relation("Plan").Relation("Product").
		Where("license.stripe_customer_id = ?", customerID).
		OrderExpr("license.created_at DESC").Limit(1).
		Scan(ctx)
}

func (s *Store) UpdateLicense(ctx context.Context, l *model.License, cols ...string) error {
	l.UpdatedAt = time.Now()
	cols = append(cols, "updated_at")
	_, err := s.DB.NewUpdate().Model(l).Column(cols...).WherePK().Exec(ctx)
	return err
}

// UpdateLicenseAndSubscription updates both the license status and its linked subscription
// in a single transaction for atomicity.
func (s *Store) UpdateLicenseAndSubscription(ctx context.Context, lic *model.License, cols ...string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	lic.UpdatedAt = time.Now()
	allCols := make([]string, len(cols)+1)
	copy(allCols, cols)
	allCols[len(cols)] = "updated_at"
	if _, err := tx.NewUpdate().Model(lic).Column(allCols...).WherePK().Exec(ctx); err != nil {
		return err
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
		OrderExpr("license.created_at DESC").Scan(ctx)
	return out, err
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

// FindActiveLicenseByEmailAndProduct returns an active or trialing license
// for the given email and product, or nil if none exists.
func (s *Store) UpdateLicenseUser(ctx context.Context, licenseID, userID string) error {
	_, err := s.DB.NewUpdate().Model((*model.License)(nil)).
		Set("user_id = ?", userID).
		Where("id = ?", licenseID).
		Exec(ctx)
	return err
}

func (s *Store) FindActiveLicenseByEmailAndProduct(ctx context.Context, email, productID string) *model.License {
	var lic model.License
	err := s.DB.NewSelect().Model(&lic).
		Where("email = ? AND product_id = ? AND status IN (?, ?)", email, productID, "active", "trialing").
		Limit(1).Scan(ctx)
	if err != nil {
		return nil
	}
	return &lic
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
	Offset              int
	Limit               int
}

func (s *Store) ListLicenses(ctx context.Context, f LicenseListFilter) ([]*model.License, int, error) {
	q := s.DB.NewSelect().Model((*model.License)(nil)).
		Relation("Plan").Relation("Product").
		OrderExpr("license.created_at DESC")
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
	err = q.Offset(f.Offset).Limit(f.Limit).Scan(ctx, &out)
	return out, total, err
}

// ─── Plan ───

func (s *Store) FindPlanByStripePrice(ctx context.Context, priceID string) (*model.Plan, error) {
	p := new(model.Plan)
	return p, s.DB.NewSelect().Model(p).Relation("Entitlements").Where("stripe_price_id = ?", priceID).Scan(ctx)
}

func (s *Store) FindPlanByCheckoutID(ctx context.Context, checkoutID string) (*model.Plan, error) {
	p := new(model.Plan)
	return p, s.DB.NewSelect().Model(p).Relation("Entitlements").Where("checkout_id = ?", checkoutID).Scan(ctx)
}

func (s *Store) FindPlanByID(ctx context.Context, id string) (*model.Plan, error) {
	p := new(model.Plan)
	return p, s.DB.NewSelect().Model(p).Relation("Entitlements").Where("plan.id = ?", id).Scan(ctx)
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
func (s *Store) TryRecordProcessedEvent(ctx context.Context, provider, eventID string) bool {
	var id string
	err := s.DB.NewRaw(
		"INSERT INTO processed_events (id, provider, event_id) VALUES (?, ?, ?) ON CONFLICT (provider, event_id) DO NOTHING RETURNING id",
		newID(), provider, eventID,
	).Scan(ctx, &id)
	// If id is empty, the insert was a no-op (already exists) → skip
	return err == nil && id != ""
}

// ─── Transactional Activation ───

// ActivateWithinLimit atomically creates an activation only if the limit hasn't been reached.
// Locks the license row (not activation rows) to serialize concurrent activations.
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
}

func (s *Store) EnqueueEmail(ctx context.Context, to, subject, body string) error {
	_, err := s.DB.NewRaw(
		"INSERT INTO email_queue (id, to_addr, subject, body, max_attempts) VALUES (?, ?, ?, ?, 5)",
		newID(), to, subject, body,
	).Exec(ctx)
	return err
}

func (s *Store) ListPendingEmails(ctx context.Context, limit int) ([]*QueuedEmail, error) {
	var out []*QueuedEmail
	err := s.DB.NewRaw(
		"SELECT id, to_addr, subject, body, attempts, max_attempts, status, next_retry, error FROM email_queue WHERE status = 'pending' AND (next_retry IS NULL OR next_retry <= now()) ORDER BY created_at ASC LIMIT ?",
		limit,
	).Scan(ctx, &out)
	return out, err
}

func (s *Store) MarkEmailSent(ctx context.Context, id string) {
	_, _ = s.DB.NewRaw(
		"UPDATE email_queue SET status = 'sent', sent_at = now(), attempts = attempts + 1 WHERE id = ?", id,
	).Exec(ctx)
}

func (s *Store) MarkEmailFailed(ctx context.Context, id string, errMsg string) {
	_, _ = s.DB.NewRaw(`
		UPDATE email_queue SET
			attempts = attempts + 1,
			error = ?,
			status = CASE WHEN attempts + 1 >= max_attempts THEN 'failed' ELSE 'pending' END,
			next_retry = CASE WHEN attempts + 1 < max_attempts THEN now() + (interval '1 minute' * power(2, attempts)) ELSE NULL END
		WHERE id = ?
	`, errMsg, id).Exec(ctx)
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
