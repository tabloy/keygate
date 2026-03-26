package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/tabloy/keygate/internal/model"
)

// ─── Product ───

func (s *Store) ListProducts(ctx context.Context, search string) ([]*model.Product, error) {
	var out []*model.Product
	q := s.DB.NewSelect().Model(&out).OrderExpr("created_at DESC")
	if search != "" {
		q = q.Where("name ILIKE ? OR slug ILIKE ?", "%"+search+"%", "%"+search+"%")
	}
	err := q.Scan(ctx)
	return out, err
}

func (s *Store) FindProductByID(ctx context.Context, id string) (*model.Product, error) {
	p := new(model.Product)
	return p, s.DB.NewSelect().Model(p).Where("id = ?", id).Scan(ctx)
}

func (s *Store) CreateProduct(ctx context.Context, p *model.Product) error {
	if p.ID == "" {
		p.ID = newID()
	}
	_, err := s.DB.NewInsert().Model(p).Exec(ctx)
	return err
}

func (s *Store) UpdateProduct(ctx context.Context, p *model.Product) error {
	_, err := s.DB.NewUpdate().Model(p).WherePK().Exec(ctx)
	return err
}

func (s *Store) DeleteProduct(ctx context.Context, id string) error {
	_, err := s.DB.NewDelete().Model((*model.Product)(nil)).Where("id = ?", id).Exec(ctx)
	return err
}

// ─── Plan ───

func (s *Store) ListPlans(ctx context.Context, productID, search string) ([]*model.Plan, error) {
	var out []*model.Plan
	q := s.DB.NewSelect().Model(&out).Relation("Entitlements").Relation("Product").OrderExpr("sort_order ASC, plan.created_at DESC")
	if productID != "" {
		q = q.Where("plan.product_id = ?", productID)
	}
	if search != "" {
		q = q.Where("plan.name ILIKE ? OR plan.slug ILIKE ?", "%"+search+"%", "%"+search+"%")
	}
	err := q.Scan(ctx)
	return out, err
}

func (s *Store) CreatePlan(ctx context.Context, p *model.Plan) error {
	if p.ID == "" {
		p.ID = newID()
	}
	_, err := s.DB.NewInsert().Model(p).Exec(ctx)
	return err
}

func (s *Store) UpdatePlan(ctx context.Context, p *model.Plan) error {
	_, err := s.DB.NewUpdate().Model(p).WherePK().Exec(ctx)
	return err
}

func (s *Store) DeletePlan(ctx context.Context, id string) error {
	_, err := s.DB.NewDelete().Model((*model.Plan)(nil)).Where("id = ?", id).Exec(ctx)
	return err
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

func (s *Store) ListAPIKeys(ctx context.Context, productID, search string) ([]*model.APIKey, error) {
	var out []*model.APIKey
	q := s.DB.NewSelect().Model(&out).Relation("Product").OrderExpr("api_key.created_at DESC")
	if productID != "" {
		q = q.Where("api_key.product_id = ?", productID)
	}
	if search != "" {
		q = q.Where("api_key.name ILIKE ? OR api_key.prefix ILIKE ?", "%"+search+"%", "%"+search+"%")
	}
	err := q.Scan(ctx)
	return out, err
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

func (s *Store) ListAuditLogs(ctx context.Context, entity, entityID string, offset, limit int) ([]*model.AuditLog, int, error) {
	q := s.DB.NewSelect().Model((*model.AuditLog)(nil)).OrderExpr("created_at DESC")
	if entity != "" {
		q = q.Where("entity = ?", entity)
	}
	if entityID != "" {
		q = q.Where("entity_id = ?", entityID)
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
	err = q.OrderExpr("created_at DESC").
		Offset(offset).Limit(limit).Scan(ctx, &out)
	return out, total, err
}

// ─── Activation (admin) ───

func (s *Store) DeleteActivationByID(ctx context.Context, id string) error {
	_, err := s.DB.NewDelete().Model((*model.Activation)(nil)).Where("id = ?", id).Exec(ctx)
	return err
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

func (s *Store) PlanLicenseCount(ctx context.Context, planID string) (int, error) {
	return s.DB.NewSelect().Model((*model.License)(nil)).Where("plan_id = ?", planID).Count(ctx)
}
