package store

import (
	"context"

	"github.com/tabloy/keygate/internal/model"
)

// ─── Addons ───

func (s *Store) CreateAddon(ctx context.Context, a *model.Addon) error {
	if a.ID == "" {
		a.ID = newID()
	}
	_, err := s.DB.NewInsert().Model(a).Exec(ctx)
	return err
}

func (s *Store) FindAddonByID(ctx context.Context, id string) (*model.Addon, error) {
	a := new(model.Addon)
	return a, s.DB.NewSelect().Model(a).Where("id = ?", id).Scan(ctx)
}

func (s *Store) ListAddons(ctx context.Context, productID, search string) ([]*model.Addon, error) {
	var out []*model.Addon
	q := s.DB.NewSelect().Model(&out).OrderExpr("sort_order ASC, created_at DESC")
	if productID != "" {
		q = q.Where("product_id = ?", productID)
	}
	if search != "" {
		q = q.Where("name ILIKE ? OR feature ILIKE ?", "%"+search+"%", "%"+search+"%")
	}
	err := q.Scan(ctx)
	return out, err
}

func (s *Store) UpdateAddon(ctx context.Context, a *model.Addon) error {
	_, err := s.DB.NewUpdate().Model(a).WherePK().Exec(ctx)
	return err
}

func (s *Store) DeleteAddon(ctx context.Context, id string) error {
	_, err := s.DB.NewDelete().Model((*model.Addon)(nil)).Where("id = ?", id).Exec(ctx)
	return err
}

// ─── License Addons ───

func (s *Store) AddLicenseAddon(ctx context.Context, la *model.LicenseAddon) error {
	if la.ID == "" {
		la.ID = newID()
	}
	_, err := s.DB.NewInsert().Model(la).
		On("CONFLICT (license_id, addon_id) DO UPDATE").
		Set("enabled = EXCLUDED.enabled").Exec(ctx)
	return err
}

func (s *Store) RemoveLicenseAddon(ctx context.Context, licenseID, addonID string) error {
	_, err := s.DB.NewDelete().Model((*model.LicenseAddon)(nil)).
		Where("license_id = ? AND addon_id = ?", licenseID, addonID).Exec(ctx)
	return err
}

func (s *Store) ListLicenseAddons(ctx context.Context, licenseID string) ([]*model.LicenseAddon, error) {
	var out []*model.LicenseAddon
	err := s.DB.NewSelect().Model(&out).Relation("Addon").
		Where("license_addon.license_id = ? AND license_addon.enabled = true", licenseID).Scan(ctx)
	return out, err
}
