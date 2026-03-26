package store

import (
	"context"
	"fmt"
	"time"

	"github.com/tabloy/keygate/internal/model"
)

func (s *Store) RecordUsageEvent(ctx context.Context, e *model.UsageEvent) error {
	if e.ID == "" {
		e.ID = newID()
	}
	_, err := s.DB.NewInsert().Model(e).Exec(ctx)
	return err
}

func (s *Store) IncrementUsageCounter(ctx context.Context, licenseID, feature, period, periodKey string, quantity int64) (*model.UsageCounter, error) {
	c := &model.UsageCounter{
		ID:        newID(),
		LicenseID: licenseID,
		Feature:   feature,
		Period:    period,
		PeriodKey: periodKey,
		Used:      quantity,
	}
	_, err := s.DB.NewInsert().Model(c).
		On("CONFLICT (license_id, feature, period, period_key) DO UPDATE").
		Set("used = usage_counter.used + EXCLUDED.used, updated_at = now()").
		Returning("*").
		Exec(ctx)
	if err != nil {
		return nil, err
	}
	return s.GetUsageCounter(ctx, licenseID, feature, period, periodKey)
}

// IncrementUsageCounterWithLimit atomically increments the counter only if the new value
// would not exceed the limit. Returns the updated counter and whether the increment was accepted.
// Uses SELECT FOR UPDATE to prevent race conditions where concurrent requests could both
// pass the limit check and exceed the quota.
func (s *Store) IncrementUsageCounterWithLimit(ctx context.Context, licenseID, feature, period, periodKey string, quantity, limit int64) (*model.UsageCounter, bool, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()

	_, err = tx.NewRaw(`
		INSERT INTO usage_counters (id, license_id, feature, period, period_key, used, updated_at)
		VALUES (?, ?, ?, ?, ?, 0, now())
		ON CONFLICT (license_id, feature, period, period_key) DO NOTHING
	`, newID(), licenseID, feature, period, periodKey).Exec(ctx)
	if err != nil {
		return nil, false, err
	}

	var currentUsed int64
	err = tx.NewRaw(`
		SELECT used FROM usage_counters
		WHERE license_id = ? AND feature = ? AND period = ? AND period_key = ?
		FOR UPDATE
	`, licenseID, feature, period, periodKey).Scan(ctx, &currentUsed)
	if err != nil {
		return nil, false, err
	}

	if limit > 0 && currentUsed+quantity > limit {
		c := &model.UsageCounter{
			LicenseID: licenseID,
			Feature:   feature,
			Period:    period,
			PeriodKey: periodKey,
			Used:      currentUsed,
		}
		if err := tx.Commit(); err != nil {
			return nil, false, err
		}
		return c, false, nil
	}

	c := new(model.UsageCounter)
	err = tx.NewRaw(`
		UPDATE usage_counters SET used = used + ?, updated_at = now()
		WHERE license_id = ? AND feature = ? AND period = ? AND period_key = ?
		RETURNING *
	`, quantity, licenseID, feature, period, periodKey).Scan(ctx, c)
	if err != nil {
		return nil, false, err
	}

	if err := tx.Commit(); err != nil {
		return nil, false, err
	}

	return c, true, nil
}

func (s *Store) GetUsageCounter(ctx context.Context, licenseID, feature, period, periodKey string) (*model.UsageCounter, error) {
	c := new(model.UsageCounter)
	err := s.DB.NewSelect().Model(c).
		Where("license_id = ? AND feature = ? AND period = ? AND period_key = ?",
			licenseID, feature, period, periodKey).
		Scan(ctx)
	return c, err
}

func (s *Store) GetUsageSummary(ctx context.Context, licenseID string) ([]*model.UsageCounter, error) {
	var out []*model.UsageCounter
	err := s.DB.NewSelect().Model(&out).
		Where("license_id = ?", licenseID).
		OrderExpr("feature, period").
		Scan(ctx)
	return out, err
}

func (s *Store) ListUsageEvents(ctx context.Context, licenseID, feature string, offset, limit int) ([]*model.UsageEvent, int, error) {
	q := s.DB.NewSelect().Model((*model.UsageEvent)(nil)).
		Where("license_id = ?", licenseID).
		OrderExpr("recorded_at DESC")
	if feature != "" {
		q = q.Where("feature = ?", feature)
	}
	total, err := q.Count(ctx)
	if err != nil {
		return nil, 0, err
	}
	var out []*model.UsageEvent
	err = q.Offset(offset).Limit(limit).Scan(ctx, &out)
	return out, total, err
}

func (s *Store) ResetUsageCounter(ctx context.Context, licenseID, feature, period, periodKey string) error {
	_, err := s.DB.NewUpdate().Model((*model.UsageCounter)(nil)).
		Set("used = 0, updated_at = now()").
		Where("license_id = ? AND feature = ? AND period = ? AND period_key = ?",
			licenseID, feature, period, periodKey).
		Exec(ctx)
	return err
}

func CurrentPeriodKey(period string) string {
	now := time.Now().UTC()
	switch period {
	case "hourly":
		return now.Format("2006-01-02T15")
	case "daily":
		return now.Format("2006-01-02")
	case "monthly":
		return now.Format("2006-01")
	case "yearly":
		return fmt.Sprintf("%d", now.Year())
	default:
		return now.Format("2006-01")
	}
}
