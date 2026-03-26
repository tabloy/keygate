package store

import (
	"context"
	"time"

	"github.com/tabloy/keygate/internal/model"
)

func (s *Store) UpsertMeteredBilling(ctx context.Context, licenseID, feature, periodKey string, quantity int64) error {
	m := &model.MeteredBilling{
		ID:        newID(),
		LicenseID: licenseID,
		Feature:   feature,
		Quantity:  quantity,
		PeriodKey: periodKey,
	}
	_, err := s.DB.NewInsert().Model(m).
		On("CONFLICT (license_id, feature, period_key) DO UPDATE").
		Set("quantity = EXCLUDED.quantity, synced = false").Exec(ctx)
	return err
}

func (s *Store) ListUnsyncedMetered(ctx context.Context, limit int) ([]*model.MeteredBilling, error) {
	var out []*model.MeteredBilling
	err := s.DB.NewSelect().Model(&out).
		Where("synced = false").OrderExpr("created_at ASC").Limit(limit).Scan(ctx)
	return out, err
}

func (s *Store) MarkMeteredSynced(ctx context.Context, id, externalID string) error {
	now := time.Now()
	_, err := s.DB.NewUpdate().Model((*model.MeteredBilling)(nil)).
		Set("synced = true, synced_at = ?, external_id = ?", now, externalID).
		Where("id = ?", id).Exec(ctx)
	return err
}

func (s *Store) ListMeteredBilling(ctx context.Context, licenseID string) ([]*model.MeteredBilling, error) {
	var out []*model.MeteredBilling
	err := s.DB.NewSelect().Model(&out).
		Where("license_id = ?", licenseID).OrderExpr("created_at DESC").Scan(ctx)
	return out, err
}
