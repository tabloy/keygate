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

func (s *Store) ListAddons(ctx context.Context, productID, search string, p Page) ([]*model.Addon, int, error) {
	var out []*model.Addon
	// The product travels with the addon. The dashboard used to name
	// it by looking the id up in the full product list it happened to
	// have loaded; with that list now a page, a row whose product is
	// not on it would have shown a bare uuid.
	q := s.DB.NewSelect().Model(&out).Relation("Product").
		OrderExpr("addon.sort_order ASC, addon.created_at DESC, addon.id DESC")
	if productID != "" {
		q = q.Where("addon.product_id = ?", productID)
	}
	if search != "" {
		q = q.Where("addon.name ILIKE ? OR addon.feature ILIKE ?", "%"+search+"%", "%"+search+"%")
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

func (s *Store) UpdateAddon(ctx context.Context, a *model.Addon) error {
	_, err := s.DB.NewUpdate().Model(a).WherePK().Exec(ctx)
	return err
}

func (s *Store) DeleteAddon(ctx context.Context, id string) error {
	return deletedOne(s.DB.NewDelete().Model((*model.Addon)(nil)).Where("id = ?", id).Exec(ctx))
}

// ─── License Addons ───

// AddLicenseAddon attaches an addon to a licence, or enables it again
// when it is already attached. la is filled from the stored row, so an
// existing attachment comes back with its own id; created reports
// whether the row is new.
func (s *Store) AddLicenseAddon(ctx context.Context, la *model.LicenseAddon) (created bool, err error) {
	if la.ID == "" {
		la.ID = newID()
	}
	id := la.ID
	err = s.DB.NewInsert().Model(la).
		On("CONFLICT (license_id, addon_id) DO UPDATE").
		Set("enabled = EXCLUDED.enabled").
		Returning("*").Scan(ctx)
	return la.ID == id, err
}

func (s *Store) RemoveLicenseAddon(ctx context.Context, licenseID, addonID string) error {
	return deletedOne(s.DB.NewDelete().Model((*model.LicenseAddon)(nil)).
		Where("license_id = ? AND addon_id = ?", licenseID, addonID).Exec(ctx))
}

func (s *Store) ListLicenseAddons(ctx context.Context, licenseID string, p Page) ([]*model.LicenseAddon, int, error) {
	var out []*model.LicenseAddon
	q := s.DB.NewSelect().Model(&out).Relation("Addon").
		Where("license_addon.license_id = ? AND license_addon.enabled = true", licenseID).
		OrderExpr("license_addon.created_at ASC, license_addon.id ASC")
	total, err := scanPage(ctx, q, p)
	if err != nil {
		return nil, 0, err
	}
	if p.Limit <= 0 {
		total = len(out)
	}
	return out, total, nil
}
