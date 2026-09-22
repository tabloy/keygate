package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/uptrace/bun"

	"github.com/tabloy/keygate/internal/model"
)

// ─── Product ───

// ListProducts returns one page of the catalogue and how many
// products the filter matched.
//
// types narrows to the product kinds the caller can actually use — the
// releases pages ask for the ones that ship binaries. Filtering here
// rather than over the page the dashboard happens to hold is the
// difference between "these are the products you can pick" and "these
// are the ones that were on screen".
//
// The order carries id as a tiebreaker: two products created in the
// same instant would otherwise be free to swap places between two
// queries, and under paging that means a row shown twice while
// another is never shown at all.
func (s *Store) ListProducts(ctx context.Context, search string, types []string, p Page) ([]*model.Product, int, error) {
	var out []*model.Product
	q := s.DB.NewSelect().Model(&out).OrderExpr("created_at DESC, id DESC")
	if len(types) > 0 {
		q = q.Where("type IN (?)", bun.In(types))
	}
	if search != "" {
		q = q.Where("name ILIKE ? OR slug ILIKE ?", "%"+search+"%", "%"+search+"%")
	}
	total, err := scanPage(ctx, q, p)
	if err != nil {
		return nil, 0, err
	}
	if p.Limit <= 0 {
		total = len(out)
	}
	return out, total, nil
}

func (s *Store) FindProductByID(ctx context.Context, id string) (*model.Product, error) {
	return FindProductByIDIn(ctx, s.DB, id)
}

// FindProductByIDIn reads the product on a caller's transaction, for
// writers that re-read its gating state under a lock.
func FindProductByIDIn(ctx context.Context, db bun.IDB, id string) (*model.Product, error) {
	p := new(model.Product)
	return p, db.NewSelect().Model(p).Where("id = ?", id).Scan(ctx)
}

func (s *Store) CreateProduct(ctx context.Context, p *model.Product) error {
	if p.ID == "" {
		p.ID = newID()
	}
	// bun's default RETURNING * fills the row back in, which matters
	// here beyond created_at: feed_gated_at is stamped by the database
	// when the row arrives already gated, and the wait before an
	// update period may be sold runs from that instant.
	_, err := s.DB.NewInsert().Model(p).Exec(ctx)
	return err
}

func (s *Store) UpdateProduct(ctx context.Context, p *model.Product) error {
	return UpdateProductIn(ctx, s.DB, p)
}

// UpdateProductGatingNowIn writes the named columns and stamps
// feed_gated_at with the database clock, reading the stored instant
// back into p. The wait a gated feed imposes runs from the moment the
// public feed actually stopped — this write — not from a time read on
// some replica before the transaction began.
func UpdateProductGatingNowIn(ctx context.Context, db bun.IDB, p *model.Product, cols ...string) error {
	q := db.NewUpdate().Model(p).WherePK().
		Set("feed_gated_at = clock_timestamp()").
		Returning("feed_gated_at")
	if len(cols) > 0 {
		q = q.Column(cols...)
	}
	_, err := q.Exec(ctx, &p.FeedGatedAt)
	return err
}

// UpdateProductIn writes the product on a caller's transaction. With
// no columns named it writes the whole row; with columns, only those
// — a caller that has to refuse the rest of a request can still
// commit the part it must not lose.
func UpdateProductIn(ctx context.Context, db bun.IDB, p *model.Product, cols ...string) error {
	q := db.NewUpdate().Model(p).WherePK()
	if len(cols) > 0 {
		q = q.Column(cols...)
	}
	_, err := q.Exec(ctx)
	return err
}

// DeleteProduct removes the product and the rows the 20260515 bundle
// refactor left behind for it.
//
// releases_legacy is that migration's rollback evidence: the old
// releases table, renamed and kept whole so the down migration can
// rename it back. Nothing reads it, no endpoint can clear it, and its
// product_id still restricts — so on an install that upgraded through
// that migration, a product whose current releases have all been
// deleted would still refuse to go, with nothing the admin could do
// about it. The rows for this one product are dropped with it; the
// rest of the table, and the rollback it exists for, are untouched.
func (s *Store) DeleteProduct(ctx context.Context, id string) error {
	return RunInTx(ctx, s.DB, func(ctx context.Context, tx bun.Tx) error {
		// Asked before the delete rather than caught after it: a
		// failed statement poisons the transaction, and an install
		// created after the refactor has no such table.
		var legacy bool
		if err := tx.NewRaw("SELECT to_regclass('public.releases_legacy') IS NOT NULL").Scan(ctx, &legacy); err != nil {
			return err
		}
		if legacy {
			if _, err := tx.NewRaw("DELETE FROM releases_legacy WHERE product_id = ?", id).Exec(ctx); err != nil {
				return err
			}
		}
		_, err := tx.NewDelete().Model((*model.Product)(nil)).Where("id = ?", id).Exec(ctx)
		return err
	})
}

// ─── Plan ───

// ListPlans returns one page of a product's plans (or of every
// product's, unfiltered) and how many the filter matched. Entitlements
// are a has-many relation, which bun loads in a query of its own, so
// the limit applies to plans and not to their rows.
func (s *Store) ListPlans(ctx context.Context, productID, search string, p Page) ([]*model.Plan, int, error) {
	var out []*model.Plan
	q := s.DB.NewSelect().Model(&out).Relation("Entitlements").Relation("Product").
		OrderExpr("sort_order ASC, plan.created_at DESC, plan.id DESC")
	if productID != "" {
		q = q.Where("plan.product_id = ?", productID)
	}
	if search != "" {
		q = q.Where("plan.name ILIKE ? OR plan.slug ILIKE ?", "%"+search+"%", "%"+search+"%")
	}
	total, err := scanPage(ctx, q, p)
	if err != nil {
		return nil, 0, err
	}
	if p.Limit <= 0 {
		total = len(out)
	}
	return out, total, nil
}

// FillPlanIDs gives a new plan the ids CreatePlan would, for callers
// that insert it on their own transaction with CreatePlanIn.
func (s *Store) FillPlanIDs(p *model.Plan) {
	if p.ID == "" {
		p.ID = newID()
	}
	if p.CheckoutID == "" {
		p.CheckoutID = shortID()
	}
}

func (s *Store) CreatePlan(ctx context.Context, p *model.Plan) error {
	s.FillPlanIDs(p)
	return RunInTx(ctx, s.DB, func(ctx context.Context, tx bun.Tx) error {
		return CreatePlanIn(ctx, tx, p)
	})
}

// CreatePlanIn writes the plan on a caller's transaction. The caller
// fills the ids (CreatePlan does it), so a plan created under the
// feed gating lock is inserted by the same transaction that checked
// the gate.
func CreatePlanIn(ctx context.Context, db bun.IDB, p *model.Plan) error {
	// The plan's first terms row is written by a trigger, so every
	// writer leaves one — including an older replica or an operator
	// at the psql prompt.
	_, err := db.NewInsert().Model(p).Exec(ctx)
	return err
}

// shortID generates a URL-safe 8-character unique ID for checkout links.
func shortID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)[:8]
}

// ShortID generates a URL-safe 8-character unique ID for checkout links.
// Exported for use by the setup handler.
func ShortID() string { return shortID() }

func (s *Store) UpdatePlan(ctx context.Context, p *model.Plan, cols ...string) error {
	return RunInTx(ctx, s.DB, func(ctx context.Context, tx bun.Tx) error {
		return UpdatePlanIn(ctx, tx, p, cols...)
	})
}

// UpdatePlanIn writes the plan on a caller's transaction. With no
// columns named it writes the whole row; with columns, only those —
// so an edit of one field cannot put another request's change back.
func UpdatePlanIn(ctx context.Context, db bun.IDB, p *model.Plan, cols ...string) error {
	q := db.NewUpdate().Model(p).WherePK()
	if len(cols) > 0 {
		q = q.Column(cols...)
	}
	// The terms history follows from the row itself: a trigger
	// appends to it when this write actually moves updates_days or
	// license_type, so an edit of an unrelated field cannot record a
	// stale value and a write that bypasses this function cannot skip
	// the record.
	_, err := q.Exec(ctx)
	return err
}

// PlanTermsAt returns what the plan was selling at t — the update
// period and the kind of licence — and whether the history reaches
// back that far.
func (s *Store) PlanTermsAt(ctx context.Context, planID string, t time.Time) (days int, licenseType string, known bool, err error) {
	err = s.DB.NewRaw(
		"SELECT updates_days, license_type FROM plan_update_terms WHERE plan_id = ? AND effective_from <= ? ORDER BY effective_from DESC, recorded_at DESC LIMIT 1",
		planID, t,
	).Scan(ctx, &days, &licenseType)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", false, nil
	}
	if err != nil {
		return 0, "", false, err
	}
	return days, licenseType, true, nil
}

func (s *Store) DeletePlan(ctx context.Context, id string) error {
	_, err := s.DB.NewDelete().Model((*model.Plan)(nil)).Where("id = ?", id).Exec(ctx)
	return err
}

// CountPlansIncompatibleWithType counts plans under the given product
// whose capability fields would become invalid under newType. Used to
// block product-type changes that would silently leave behind plan
// rows with max_activations or floating license_model on a SaaS
// product, or max_seats on a desktop product.
//
// Rules mirror model.ProductSupports:
//   - if newType doesn't support activations → max_activations>0 OR
//     license_model='floating' is a conflict
//   - if newType doesn't support seats → max_seats>0 is a conflict
func (s *Store) CountPlansIncompatibleWithType(ctx context.Context, productID, newType string) (int, error) {
	supportsAct := model.ProductSupports(newType, model.CapActivations)
	supportsSeats := model.ProductSupports(newType, model.CapSeats)
	if supportsAct && supportsSeats {
		return 0, nil
	}
	q := s.DB.NewSelect().Model((*model.Plan)(nil)).Where("product_id = ?", productID)
	switch {
	case !supportsAct && !supportsSeats:
		q = q.Where("max_activations > 0 OR license_model = 'floating' OR max_seats > 0")
	case !supportsAct:
		q = q.Where("max_activations > 0 OR license_model = 'floating'")
	case !supportsSeats:
		q = q.Where("max_seats > 0")
	}
	return q.Count(ctx)
}

// ─── Entitlement ───

func (s *Store) FindEntitlementByID(ctx context.Context, id string) (*model.Entitlement, error) {
	e := new(model.Entitlement)
	return e, s.DB.NewSelect().Model(e).Where("id = ?", id).Scan(ctx)
}

func (s *Store) CreateEntitlement(ctx context.Context, e *model.Entitlement) error {
	if e.ID == "" {
		e.ID = newID()
	}
	_, err := s.DB.NewInsert().Model(e).Exec(ctx)
	return err
}

func (s *Store) UpdateEntitlement(ctx context.Context, e *model.Entitlement) error {
	_, err := s.DB.NewUpdate().Model(e).WherePK().Exec(ctx)
	return err
}

func (s *Store) DeleteEntitlement(ctx context.Context, id string) error {
	_, err := s.DB.NewDelete().Model((*model.Entitlement)(nil)).Where("id = ?", id).Exec(ctx)
	return err
}

// ─── API Key ───

func (s *Store) ListAPIKeys(ctx context.Context, productID, search string, p Page) ([]*model.APIKey, int, error) {
	var out []*model.APIKey
	q := s.DB.NewSelect().Model(&out).Relation("Product").
		OrderExpr("api_key.created_at DESC, api_key.id DESC")
	if productID != "" {
		q = q.Where("api_key.product_id = ?", productID)
	}
	if search != "" {
		q = q.Where("api_key.name ILIKE ? OR api_key.prefix ILIKE ?", "%"+search+"%", "%"+search+"%")
	}
	total, err := scanPage(ctx, q, p)
	if err != nil {
		return nil, 0, err
	}
	if p.Limit <= 0 {
		total = len(out)
	}
	return out, total, nil
}

func (s *Store) CreateAPIKey(ctx context.Context, ak *model.APIKey, rawKey string) error {
	if ak.ID == "" {
		ak.ID = newID()
	}
	ak.KeyHash = HashAPIKey(rawKey)
	_, err := s.DB.NewInsert().Model(ak).Exec(ctx)
	return err
}

func (s *Store) DeleteAPIKey(ctx context.Context, id string) error {
	_, err := s.DB.NewDelete().Model((*model.APIKey)(nil)).Where("id = ?", id).Exec(ctx)
	return err
}

// GetLicenseProductID is a one-column lookup used by handlers that
// only need to know which product a license belongs to (for API key
// scope enforcement). Cheaper than FindLicenseByID which eager-loads
// product, plan, entitlements, and activations.
func (s *Store) GetLicenseProductID(ctx context.Context, id string) (string, error) {
	var pid string
	err := s.DB.NewSelect().Model((*model.License)(nil)).
		Column("product_id").
		Where("id = ?", id).
		Scan(ctx, &pid)
	return pid, err
}

// FindAPIKeyByID is the byID lookup used by the admin/rotate path.
// It does not eager-load the product since the rotate response only
// needs the row's name + scopes for audit context.
func (s *Store) FindAPIKeyByID(ctx context.Context, id string) (*model.APIKey, error) {
	ak := new(model.APIKey)
	err := s.DB.NewSelect().Model(ak).Where("id = ?", id).Scan(ctx)
	if err != nil {
		return nil, err
	}
	return ak, nil
}

// RotateAPIKey swaps the secret for an existing row. The ID, name,
// scopes, and product binding are preserved so callers can update
// their config without re-registering the key. last_used/last_used_ip
// are reset since the previous physical secret is no longer valid.
func (s *Store) RotateAPIKey(ctx context.Context, id, rawKey, prefix string) error {
	_, err := s.DB.NewUpdate().Model((*model.APIKey)(nil)).
		Set("key_hash = ?", HashAPIKey(rawKey)).
		Set("prefix = ?", prefix).
		Set("last_used = NULL").
		Set("last_used_ip = ''").
		Where("id = ?", id).
		Exec(ctx)
	return err
}

// GenerateRawAPIKey creates a raw API key string like "kg_live_xxxxxxxx..."
func GenerateRawAPIKey() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return "kg_live_" + hex.EncodeToString(b)
}

// ─── License (admin) ───

func (s *Store) FindLicenseByID(ctx context.Context, id string) (*model.License, error) {
	l := new(model.License)
	return l, s.DB.NewSelect().Model(l).
		Relation("Plan").
		Relation("Plan.Entitlements").
		Relation("Product").
		Relation("Activations").
		Where("license.id = ?", id).
		Scan(ctx)
}

// FindLicenseByIDWithCounts is FindLicenseByID with the computed
// fields a list response carries filled in, so a caller reading one
// license sees the same numbers as the row it clicked on.
func (s *Store) FindLicenseByIDWithCounts(ctx context.Context, id string) (*model.License, error) {
	l, err := s.FindLicenseByID(ctx, id)
	if err != nil {
		return nil, err
	}
	l.ActivationCount = len(l.Activations)
	if l.Plan != nil && l.Plan.LicenseModel == "floating" {
		n, err := s.CountActiveFloating(ctx, l.ID)
		if err != nil {
			return nil, err
		}
		l.ActiveSessionCount = n
	}
	return l, nil
}

func (s *Store) RevokeLicense(ctx context.Context, id string) error {
	_, _ = s.DB.NewDelete().Model((*model.Activation)(nil)).Where("license_id = ?", id).Exec(ctx)
	res, err := s.DB.NewUpdate().Model((*model.License)(nil)).
		Set("status = ?, updated_at = ?", model.StatusRevoked, time.Now()).
		Where("id = ?", id).Exec(ctx)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("license not found")
	}
	_, _ = s.DB.NewRaw(`UPDATE subscriptions SET status = ?, updated_at = now() WHERE license_id = ?`, model.StatusRevoked, id).Exec(ctx)
	return nil
}

func (s *Store) SuspendLicense(ctx context.Context, id string) error {
	now := time.Now()
	res, err := s.DB.NewUpdate().Model((*model.License)(nil)).
		Set("status = ?, suspended_at = ?, updated_at = ?", model.StatusSuspended, now, now).
		Where("id = ?", id).Exec(ctx)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("license not found")
	}
	_, _ = s.DB.NewRaw(`UPDATE subscriptions SET status = ?, updated_at = now() WHERE license_id = ?`, model.StatusSuspended, id).Exec(ctx)
	return nil
}

func (s *Store) ReinstateLicense(ctx context.Context, id string) error {
	res, err := s.DB.NewUpdate().Model((*model.License)(nil)).
		Set("status = ?, suspended_at = NULL, canceled_at = NULL, updated_at = ?", model.StatusActive, time.Now()).
		Where("id = ?", id).
		Where("status IN ('suspended', 'expired', 'canceled')").
		Exec(ctx)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("license not found or cannot be reinstated from current status")
	}
	_, _ = s.DB.NewRaw(`UPDATE subscriptions SET status = ?, updated_at = now() WHERE license_id = ?`, model.StatusActive, id).Exec(ctx)
	return nil
}

// ExportLicenses returns all licenses matching filters (no pagination).
func (s *Store) ExportLicenses(ctx context.Context, productID, status string) ([]*model.License, error) {
	var out []*model.License
	q := s.DB.NewSelect().Model(&out).
		Relation("Plan").Relation("Product").
		OrderExpr("license.created_at DESC")
	if productID != "" {
		q = q.Where("license.product_id = ?", productID)
	}
	if status != "" {
		q = q.Where("license.status = ?", status)
	}
	err := q.Scan(ctx)
	return out, err
}

// ─── Audit Log ───

// ListAuditLogs returns paginated audit-log rows. Filters compose
// with AND. The optional productID filter resolves the parent product
// across the entity types that carry a product_id FK (license, plan,
// addon, webhook, release, api_key, plus the product row itself). It
// is best-effort: 2-hop entities (seat, activation, release_artifact)
// aren't matched and silently fall out of the filtered view.
func (s *Store) ListAuditLogs(ctx context.Context, entity, entityID, productID string, offset, limit int) ([]*model.AuditLog, int, error) {
	q := s.DB.NewSelect().Model((*model.AuditLog)(nil)).OrderExpr("created_at DESC, id DESC")
	if entity != "" {
		q = q.Where("entity = ?", entity)
	}
	if entityID != "" {
		q = q.Where("entity_id = ?", entityID)
	}
	if productID != "" {
		q = q.Where(`(
            (entity = 'product' AND entity_id = ?)
         OR entity_id IN (SELECT id FROM licenses  WHERE product_id = ?)
         OR entity_id IN (SELECT id FROM plans     WHERE product_id = ?)
         OR entity_id IN (SELECT id FROM addons    WHERE product_id = ?)
         OR entity_id IN (SELECT id FROM webhooks  WHERE product_id = ?)
         OR entity_id IN (SELECT id FROM releases  WHERE product_id = ?)
         OR entity_id IN (SELECT id FROM api_keys  WHERE product_id = ?)
        )`, productID, productID, productID, productID, productID, productID, productID)
	}
	total, err := q.Count(ctx)
	if err != nil {
		return nil, 0, err
	}
	var out []*model.AuditLog
	err = q.Offset(offset).Limit(limit).Scan(ctx, &out)
	return out, total, err
}

// ─── Stats ───

type Stats struct {
	TotalLicenses    int              `json:"total_licenses"`
	ActiveLicenses   int              `json:"active_licenses"`
	TotalActivations int              `json:"total_activations"`
	TotalProducts    int              `json:"total_products"`
	TotalSeats       int              `json:"total_seats"`
	TotalUsageEvents int              `json:"total_usage_events"`
	TotalWebhooks    int              `json:"total_webhooks"`
	ByStatus         map[string]int   `json:"by_status"`
	RecentLicenses   []*model.License `json:"recent_licenses"`
}

func (s *Store) GetStats(ctx context.Context) (*Stats, error) {
	stats := &Stats{ByStatus: make(map[string]int)}

	stats.TotalLicenses, _ = s.DB.NewSelect().Model((*model.License)(nil)).Count(ctx)
	stats.ActiveLicenses, _ = s.DB.NewSelect().Model((*model.License)(nil)).Where("status = 'active'").Count(ctx)
	stats.TotalActivations, _ = s.DB.NewSelect().Model((*model.Activation)(nil)).Count(ctx)
	stats.TotalProducts, _ = s.DB.NewSelect().Model((*model.Product)(nil)).Count(ctx)
	stats.TotalSeats, _ = s.DB.NewSelect().Model((*model.Seat)(nil)).Where("removed_at IS NULL").Count(ctx)
	stats.TotalUsageEvents, _ = s.DB.NewSelect().Model((*model.UsageEvent)(nil)).Count(ctx)
	stats.TotalWebhooks, _ = s.DB.NewSelect().Model((*model.Webhook)(nil)).Where("active = true").Count(ctx)

	type statusCount struct {
		Status string `bun:"status"`
		Count  int    `bun:"count"`
	}
	var counts []statusCount
	_ = s.DB.NewSelect().Model((*model.License)(nil)).
		ColumnExpr("status, count(*) as count").
		Group("status").Scan(ctx, &counts)
	for _, c := range counts {
		stats.ByStatus[c.Status] = c.Count
	}

	var recent []*model.License
	_ = s.DB.NewSelect().Model(&recent).
		Relation("Product").Relation("Plan").
		OrderExpr("license.created_at DESC").Limit(10).Scan(ctx)
	stats.RecentLicenses = recent

	return stats, nil
}

// ─── Users ───

// ListUsers returns only customers (role='user'). Admins are managed separately.
func (s *Store) ListUsers(ctx context.Context, search string, offset, limit int) ([]*model.User, int, error) {
	q := s.DB.NewSelect().Model((*model.User)(nil)).Where("role = 'user'")
	if search != "" {
		q = q.Where("(email ILIKE ? OR name ILIKE ?)", "%"+search+"%", "%"+search+"%")
	}
	total, err := q.Count(ctx)
	if err != nil {
		return nil, 0, err
	}
	var out []*model.User
	err = q.OrderExpr("created_at DESC, id DESC").
		Offset(offset).Limit(limit).Scan(ctx, &out)
	return out, total, err
}

// ─── Activation (admin) ───

func (s *Store) DeleteActivationByID(ctx context.Context, id string) error {
	_, err := s.DB.NewDelete().Model((*model.Activation)(nil)).Where("id = ?", id).Exec(ctx)
	return err
}

// GetActivationProductID resolves the product behind an activation
// in a single query. Used by the activation delete handler to enforce
// API-key product scoping without loading the full activation row.
func (s *Store) GetActivationProductID(ctx context.Context, activationID string) (string, error) {
	var pid string
	err := s.DB.NewRaw(`
        SELECT l.product_id
        FROM activations a JOIN licenses l ON l.id = a.license_id
        WHERE a.id = ?`, activationID).Scan(ctx, &pid)
	return pid, err
}

func (s *Store) ListActivations(ctx context.Context, licenseID string) ([]*model.Activation, error) {
	var out []*model.Activation
	err := s.DB.NewSelect().Model(&out).Where("license_id = ?", licenseID).
		OrderExpr("created_at DESC").Scan(ctx)
	return out, err
}

func (s *Store) FindProductBySlug(ctx context.Context, slug string) (*model.Product, error) {
	p := new(model.Product)
	return p, s.DB.NewSelect().Model(p).Where("slug = ?", slug).Scan(ctx)
}

func (s *Store) ProductLicenseCount(ctx context.Context, productID string) (int, error) {
	return s.DB.NewSelect().Model((*model.License)(nil)).Where("product_id = ?", productID).Count(ctx)
}

// ProductBlockers counts what a product delete would have to destroy
// and the database refuses to: plans, licences and releases all hold
// it by a foreign key that restricts. All three are counted in one
// call so the caller can name the one that matters most rather than
// whichever query happened to run first.
//
// What is NOT counted is what the database removes with the product
// (api keys, webhooks, addons, signing keys, analytics): those
// cascade by design.
func (s *Store) ProductBlockers(ctx context.Context, productID string) (plans, licenses, releases int, err error) {
	if plans, err = s.DB.NewSelect().Model((*model.Plan)(nil)).
		Where("product_id = ?", productID).Count(ctx); err != nil {
		return 0, 0, 0, err
	}
	if licenses, err = s.ProductLicenseCount(ctx, productID); err != nil {
		return 0, 0, 0, err
	}
	if releases, err = s.DB.NewSelect().Model((*model.Release)(nil)).
		Where("product_id = ?", productID).Count(ctx); err != nil {
		return 0, 0, 0, err
	}
	return plans, licenses, releases, nil
}

func (s *Store) PlanLicenseCount(ctx context.Context, planID string) (int, error) {
	return PlanLicenseCountIn(ctx, s.DB, planID)
}

// PlanLicenseCountIn counts on a caller's transaction, for a writer
// that holds the plan's row lock across the count and the write.
func PlanLicenseCountIn(ctx context.Context, db bun.IDB, planID string) (int, error) {
	return db.NewSelect().Model((*model.License)(nil)).Where("plan_id = ?", planID).Count(ctx)
}

// ErrPlanHasLicenses: the plan's license type was to change, but a
// license appeared on it — the type decides what a license carries
// (a subscription row, an update period), so the licenses move first.
var ErrPlanHasLicenses = errors.New("plan has licenses")

// ProductHasMaintenance reports whether the product has anything the
// feed gate protects: a plan selling a bounded update period or
// renewals, or a license with a finite cutoff (which outlives the
// plan setting that created it).
func (s *Store) ProductHasMaintenance(ctx context.Context, productID string) (bool, error) {
	return ProductHasMaintenanceIn(ctx, s.DB, productID)
}

// ProductHasMaintenanceIn answers on a caller's transaction, for a
// writer that decides under the product's lock.
func ProductHasMaintenanceIn(ctx context.Context, db bun.IDB, productID string) (bool, error) {
	plans, err := db.NewSelect().Model((*model.Plan)(nil)).
		Where("product_id = ? AND (updates_days > 0 OR stripe_renewal_price_id <> '')", productID).Exists(ctx)
	if err != nil || plans {
		return plans, err
	}
	return db.NewSelect().Model((*model.License)(nil)).
		Where("product_id = ? AND updates_until IS NOT NULL", productID).Exists(ctx)
}
