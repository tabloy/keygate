package store

import (
	"context"
	"time"

	"github.com/tabloy/keygate/internal/model"
)

func (s *Store) CreateSubscription(ctx context.Context, sub *model.Subscription) error {
	if sub.ID == "" {
		sub.ID = newID()
	}
	_, err := s.DB.NewInsert().Model(sub).Exec(ctx)
	return err
}

func (s *Store) FindSubscriptionByID(ctx context.Context, id string) (*model.Subscription, error) {
	sub := new(model.Subscription)
	return sub, s.DB.NewSelect().Model(sub).
		Relation("License").Relation("Plan").
		Where("subscription.id = ?", id).Scan(ctx)
}

func (s *Store) FindSubscriptionByLicense(ctx context.Context, licenseID string) (*model.Subscription, error) {
	sub := new(model.Subscription)
	return sub, s.DB.NewSelect().Model(sub).
		Relation("Plan").
		Where("subscription.license_id = ?", licenseID).
		OrderExpr("subscription.created_at DESC").Limit(1).Scan(ctx)
}

func (s *Store) FindSubscriptionByExternal(ctx context.Context, provider, externalID string) (*model.Subscription, error) {
	sub := new(model.Subscription)
	return sub, s.DB.NewSelect().Model(sub).
		Relation("License").Relation("Plan").
		Where("subscription.payment_provider = ? AND subscription.external_id = ?", provider, externalID).Scan(ctx)
}

func (s *Store) ListSubscriptionsByUser(ctx context.Context, userID string) ([]*model.Subscription, error) {
	var out []*model.Subscription
	err := s.DB.NewSelect().Model(&out).
		Relation("License").Relation("Plan").Relation("Plan.Product").
		Where("subscription.user_id = ?", userID).
		OrderExpr("subscription.created_at DESC").Scan(ctx)
	return out, err
}

func (s *Store) UpdateSubscription(ctx context.Context, sub *model.Subscription, cols ...string) error {
	sub.UpdatedAt = time.Now()
	cols = append(cols, "updated_at")
	_, err := s.DB.NewUpdate().Model(sub).Column(cols...).WherePK().Exec(ctx)
	return err
}

func (s *Store) ListSubscriptions(ctx context.Context, productID, status string, offset, limit int) ([]*model.Subscription, int, error) {
	q := s.DB.NewSelect().Model((*model.Subscription)(nil)).
		Relation("License").Relation("Plan").Relation("Plan.Product").
		OrderExpr("subscription.created_at DESC")
	if productID != "" {
		q = q.Where("subscription.plan_id IN (SELECT id FROM plans WHERE product_id = ?)", productID)
	}
	if status != "" {
		q = q.Where("subscription.status = ?", status)
	}
	total, err := q.Count(ctx)
	if err != nil {
		return nil, 0, err
	}
	var out []*model.Subscription
	err = q.Offset(offset).Limit(limit).Scan(ctx, &out)
	return out, total, err
}
