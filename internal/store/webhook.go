package store

import (
	"context"
	"time"

	"github.com/uptrace/bun"

	"github.com/tabloy/keygate/internal/model"
)

func (s *Store) CreateWebhook(ctx context.Context, w *model.Webhook) error {
	if w.ID == "" {
		w.ID = newID()
	}
	_, err := s.DB.NewInsert().Model(w).Exec(ctx)
	return err
}

func (s *Store) FindWebhookByID(ctx context.Context, id string) (*model.Webhook, error) {
	w := new(model.Webhook)
	return w, s.DB.NewSelect().Model(w).Relation("Product").Where("webhook.id = ?", id).Scan(ctx)
}

func (s *Store) ListWebhooks(ctx context.Context, productID, search string) ([]*model.Webhook, error) {
	var out []*model.Webhook
	q := s.DB.NewSelect().Model(&out).Relation("Product").OrderExpr("webhook.created_at DESC")
	if productID != "" {
		q = q.Where("webhook.product_id = ?", productID)
	}
	if search != "" {
		q = q.Where("webhook.url ILIKE ?", "%"+search+"%")
	}
	err := q.Scan(ctx)
	return out, err
}

func (s *Store) UpdateWebhook(ctx context.Context, w *model.Webhook) error {
	w.UpdatedAt = time.Now()
	_, err := s.DB.NewUpdate().Model(w).WherePK().Exec(ctx)
	return err
}

func (s *Store) DeleteWebhook(ctx context.Context, id string) error {
	_, err := s.DB.NewDelete().Model((*model.Webhook)(nil)).Where("id = ?", id).Exec(ctx)
	return err
}

func (s *Store) FindWebhooksForEvent(ctx context.Context, productID, event string) ([]*model.Webhook, error) {
	var out []*model.Webhook
	err := s.DB.NewSelect().Model(&out).
		Where("product_id = ? AND active = true AND ? = ANY(events)", productID, event).
		Scan(ctx)
	return out, err
}

func (s *Store) CreateWebhookDelivery(ctx context.Context, d *model.WebhookDelivery) error {
	if d.ID == "" {
		d.ID = newID()
	}
	_, err := s.DB.NewInsert().Model(d).Exec(ctx)
	return err
}

func (s *Store) UpdateWebhookDelivery(ctx context.Context, d *model.WebhookDelivery) error {
	_, err := s.DB.NewUpdate().Model(d).WherePK().Exec(ctx)
	return err
}

func (s *Store) ListPendingDeliveries(ctx context.Context, limit int) ([]*model.WebhookDelivery, error) {
	var out []*model.WebhookDelivery
	err := s.DB.NewSelect().Model(&out).
		Where("status = 'pending' AND (next_retry IS NULL OR next_retry <= ?)", time.Now()).
		OrderExpr("created_at ASC").
		Limit(limit).Scan(ctx)
	return out, err
}

func (s *Store) ListWebhookDeliveries(ctx context.Context, webhookID string, offset, limit int) ([]*model.WebhookDelivery, int, error) {
	q := s.DB.NewSelect().Model((*model.WebhookDelivery)(nil)).
		Where("webhook_id = ?", webhookID).OrderExpr("created_at DESC")
	total, err := q.Count(ctx)
	if err != nil {
		return nil, 0, err
	}
	var out []*model.WebhookDelivery
	err = q.Offset(offset).Limit(limit).Scan(ctx, &out)
	return out, total, err
}

var _ bun.DB
