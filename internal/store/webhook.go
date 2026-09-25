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

func (s *Store) ListWebhooks(ctx context.Context, productID, search string, p Page) ([]*model.Webhook, int, error) {
	var out []*model.Webhook
	q := s.DB.NewSelect().Model(&out).Relation("Product").OrderExpr("webhook.created_at DESC, webhook.id DESC")
	if productID != "" {
		q = q.Where("webhook.product_id = ?", productID)
	}
	if search != "" {
		q = q.Where("webhook.url ILIKE ?", "%"+search+"%")
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

// WebhookDeliveryFilter narrows ListWebhookDeliveries. Status / Event
// are optional — empty values mean "no filter on this dimension".
// Used by the admin UI to drill into "show me only failed deliveries"
// without pulling the whole history.
type WebhookDeliveryFilter struct {
	WebhookID string
	Status    string // pending | delivered | failed
	Event     string // exact event name, e.g. license.created
	Offset    int
	Limit     int
}

// webhookDeliveryOrder is how a delivery listing is ordered.
//
// The trailing id is the tiebreaker, as on every other paginated list.
// Retries of one event land in the same millisecond often enough, and
// with only created_at to go on Postgres may order equal rows
// differently between two queries, so a row can repeat on one page and
// be missing from the next. It is a named constant so the tiebreaker
// can be asserted without a database.
const webhookDeliveryOrder = "created_at DESC, id DESC"

func (s *Store) ListWebhookDeliveries(ctx context.Context, f WebhookDeliveryFilter) ([]*model.WebhookDelivery, int, error) {
	q := s.DB.NewSelect().Model((*model.WebhookDelivery)(nil)).
		Where("webhook_id = ?", f.WebhookID).
		OrderExpr(webhookDeliveryOrder)
	if f.Status != "" {
		q = q.Where("status = ?", f.Status)
	}
	if f.Event != "" {
		q = q.Where("event = ?", f.Event)
	}
	total, err := q.Count(ctx)
	if err != nil {
		return nil, 0, err
	}
	limit := f.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var out []*model.WebhookDelivery
	err = q.Offset(f.Offset).Limit(limit).Scan(ctx, &out)
	return out, total, err
}

// FindWebhookDeliveryByID returns a single delivery row. Used by the
// delivery detail view + the resend endpoint so admins can inspect
// payload / response and trigger a manual re-fire.
func (s *Store) FindWebhookDeliveryByID(ctx context.Context, id string) (*model.WebhookDelivery, error) {
	d := new(model.WebhookDelivery)
	err := s.DB.NewSelect().Model(d).Where("id = ?", id).Scan(ctx)
	return d, err
}

var _ bun.DB
