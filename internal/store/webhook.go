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
		term := "%" + escapeLike(search) + "%"
		q = q.Where(`webhook.url ILIKE ? ESCAPE '\'`, term)
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

// UpdateWebhook writes only the named columns of w, and reads the row
// back into w as it now stands. Writing the whole row put back what the
// caller had read for the rest, so two edits on two replicas (a new URL
// on one, active=false on the other) kept only one: a receiver switched
// off came back on. sql.ErrNoRows when the webhook is gone.
func (s *Store) UpdateWebhook(ctx context.Context, w *model.Webhook, columns ...string) error {
	w.UpdatedAt = time.Now()
	return s.DB.NewUpdate().Model(w).Column(append(columns, "updated_at")...).WherePK().
		Returning("*").Scan(ctx)
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

// CreateWebhookDelivery writes a delivery its creator is about to send,
// so the creator holds it from the start: the row gets a claim token,
// and d.NextRetry is its lease, as if ClaimPendingDeliveries had
// handed it out.
func (s *Store) CreateWebhookDelivery(ctx context.Context, d *model.WebhookDelivery) error {
	if d.ID == "" {
		d.ID = newID()
	}
	if d.ClaimToken == "" {
		d.ClaimToken = newID()
	}
	_, err := s.DB.NewInsert().Model(d).Exec(ctx)
	return err
}

// ClaimPendingDeliveries takes up to limit due deliveries for this
// sender alone. Every replica runs the retry loop, so reading pending
// rows and then sending them would POST the same delivery id from
// several replicas: the rows are claimed in the statement that reads
// them, and SKIP LOCKED hands a concurrent caller different ones. As
// with ClaimNextEmail, the claim is a lease in next_retry plus a token,
// not a status change, so a sender that dies mid-send releases the row
// when the lease runs out.
//
// The batch shares one lease while its rows wait their turn for a
// sending slot, so each is renewed (RenewWebhookClaim) right before it
// is sent: a row whose lease ran out while it waited has been taken
// over by then, and is not sent twice.
func (s *Store) ClaimPendingDeliveries(ctx context.Context, limit int, lease time.Duration) ([]*model.WebhookDelivery, error) {
	var out []*model.WebhookDelivery
	err := s.DB.NewRaw(`
		WITH claimed AS (
			SELECT id FROM webhook_deliveries
			 WHERE status = 'pending' AND (next_retry IS NULL OR next_retry <= now())
			 ORDER BY created_at ASC LIMIT ?
			 FOR UPDATE SKIP LOCKED
		)
		UPDATE webhook_deliveries d SET next_retry = now() + ?::interval, claim_token = ?
		  FROM claimed WHERE d.id = claimed.id
		RETURNING d.*`,
		limit, lease.String(), newID(),
	).Scan(ctx, &out)
	return out, err
}

// RenewWebhookClaim extends the lease on a delivery the caller holds by
// token, and reports whether it still holds it. False means the lease
// ran out and another sender has taken the row over (or it is no longer
// pending): the caller must not send it.
func (s *Store) RenewWebhookClaim(ctx context.Context, id, token string, lease time.Duration) (bool, error) {
	if token == "" {
		return false, nil
	}
	res, err := s.DB.NewRaw(
		"UPDATE webhook_deliveries SET next_retry = now() + ?::interval WHERE id = ? AND claim_token = ? AND status = 'pending'",
		lease.String(), id, token,
	).Exec(ctx)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// FinishWebhookDelivery records the outcome of an attempt on a delivery
// the caller holds, and lets go of it. It is keyed by the token like
// RenewWebhookClaim: a sender that stalled past its lease must not
// write its result over the new holder's, or put a delivered row back
// to pending. Reports whether the row was still the caller's.
func (s *Store) FinishWebhookDelivery(ctx context.Context, d *model.WebhookDelivery) (bool, error) {
	if d.ClaimToken == "" {
		return false, nil
	}
	res, err := s.DB.NewRaw(`
		UPDATE webhook_deliveries SET
			status = ?, attempts = ?, response_code = ?, response_body = ?,
			next_retry = ?, delivered_at = ?, claim_token = ''
		 WHERE id = ? AND claim_token = ?`,
		d.Status, d.Attempts, d.ResponseCode, d.ResponseBody, d.NextRetry, d.DeliveredAt,
		d.ID, d.ClaimToken,
	).Exec(ctx)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	d.ClaimToken = ""
	return n == 1, nil
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
