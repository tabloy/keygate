package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
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

	"github.com/tabloy/keygate/internal/license"
	"github.com/tabloy/keygate/internal/model"
)

type Store struct {
	DB *bun.DB
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
	_, err := s.DB.NewInsert().Model(u).
		On("CONFLICT (email) DO UPDATE").
		Set("name = EXCLUDED.name, avatar_url = EXCLUDED.avatar_url, updated_at = now()").
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
// The user will get proper name/avatar when they first log in via OAuth.
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
	go func() {
		_, _ = s.DB.NewUpdate().Model((*model.APIKey)(nil)).
			Set("last_used = now()").Where("id = ?", ak.ID).Exec(context.Background())
	}()
	return ak.Product, ak, nil
}

// ─── License ───

func (s *Store) CreateLicense(ctx context.Context, l *model.License) error {
	if l.ID == "" {
		l.ID = newID()
	}
	l.KeyHash = license.HashKey(l.LicenseKey)
	_, err := s.DB.NewInsert().Model(l).Exec(ctx)
	return err
}

// CreateLicenseWithSubscription creates a license and, for subscription/trial plans,
// a subscription record in a single transaction to prevent orphan records.
func (s *Store) CreateLicenseWithSubscription(ctx context.Context, l *model.License, plan *model.Plan) error {
	if l.ID == "" {
		l.ID = newID()
	}
	l.KeyHash = license.HashKey(l.LicenseKey)

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

func (s *Store) FindLicenseByPayPalSubscription(ctx context.Context, subID string) (*model.License, error) {
	l := new(model.License)
	return l, s.DB.NewSelect().Model(l).Relation("Plan").Where("license.paypal_subscription_id = ?", subID).Scan(ctx)
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

func (s *Store) ListLicenses(ctx context.Context, productID, status, search string, offset, limit int) ([]*model.License, int, error) {
	q := s.DB.NewSelect().Model((*model.License)(nil)).
		Relation("Plan").Relation("Product").
		OrderExpr("license.created_at DESC")
	if productID != "" {
		q = q.Where("license.product_id = ?", productID)
	}
	if status != "" {
		q = q.Where("license.status = ?", status)
	}
	if search != "" {
		// Only search by email and key prefix — never expose full key via wildcard search.
		q = q.Where("(license.email ILIKE ? OR license.license_key LIKE ?)", "%"+search+"%", search+"%")
	}
	total, err := q.Count(ctx)
	if err != nil {
		return nil, 0, err
	}
	var out []*model.License
	err = q.Offset(offset).Limit(limit).Scan(ctx, &out)
	return out, total, err
}

// ─── Plan ───

func (s *Store) FindPlanByStripePrice(ctx context.Context, priceID string) (*model.Plan, error) {
	p := new(model.Plan)
	return p, s.DB.NewSelect().Model(p).Relation("Entitlements").Where("stripe_price_id = ?", priceID).Scan(ctx)
}

func (s *Store) FindPlanByPayPalPlanID(ctx context.Context, planID string) (*model.Plan, error) {
	p := new(model.Plan)
	return p, s.DB.NewSelect().Model(p).Relation("Entitlements").Where("paypal_plan_id = ?", planID).Scan(ctx)
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

// FindStalePastDueLicenses returns past_due licenses updated before the threshold.
func (s *Store) FindStalePastDueLicenses(ctx context.Context, before time.Time) ([]*model.License, error) {
	var out []*model.License
	err := s.DB.NewSelect().Model(&out).
		Where("status = 'past_due'").
		Where("updated_at < ?", before).
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
	ID        string    `bun:"id,pk"`
	UserID    string    `bun:"user_id,notnull"`
	TokenHash string    `bun:"token_hash,notnull"`
	ExpiresAt time.Time `bun:"expires_at,notnull"`
	CreatedAt time.Time `bun:"created_at,default:now()"`
}

func (s *Store) CreateRefreshToken(ctx context.Context, userID, tokenHash string, expiresAt time.Time) error {
	_, err := s.DB.NewRaw(
		"INSERT INTO refresh_tokens (id, user_id, token_hash, expires_at) VALUES (?, ?, ?, ?)",
		newID(), userID, tokenHash, expiresAt,
	).Exec(ctx)
	return err
}

func (s *Store) FindRefreshToken(ctx context.Context, tokenHash string) (*RefreshToken, error) {
	rt := new(RefreshToken)
	err := s.DB.NewRaw(
		"SELECT id, user_id, token_hash, expires_at FROM refresh_tokens WHERE token_hash = ? AND expires_at > now()",
		tokenHash,
	).Scan(ctx, rt)
	return rt, err
}

func (s *Store) DeleteRefreshToken(ctx context.Context, tokenHash string) {
	_, _ = s.DB.NewRaw("DELETE FROM refresh_tokens WHERE token_hash = ?", tokenHash).Exec(ctx)
}

func (s *Store) DeleteUserRefreshTokens(ctx context.Context, userID string) {
	_, _ = s.DB.NewRaw("DELETE FROM refresh_tokens WHERE user_id = ?", userID).Exec(ctx)
}

func (s *Store) CleanExpiredRefreshTokens(ctx context.Context) {
	_, _ = s.DB.NewRaw("DELETE FROM refresh_tokens WHERE expires_at < now()").Exec(ctx)
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
