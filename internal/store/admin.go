package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/driver/pgdriver"

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
		q = q.Where("type IN (?)", bun.List(types))
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
	if _, err := q.Exec(ctx); err != nil {
		return err
	}
	// A plan sold per seat keeps 1 in max_seats on a product with seats
	// and 0 on one without (see model.Plan.MaxSeats); a new type moves
	// it with the product.
	if len(cols) == 0 || slices.Contains(cols, "type") {
		seats := 0
		if model.ProductSupports(p.Type, model.CapSeats) {
			seats = 1
		}
		if _, err := db.NewRaw(`UPDATE plans SET max_seats = ? WHERE product_id = ? AND per_seat AND max_seats <> ?`,
			seats, p.ID, seats).Exec(ctx); err != nil {
			return err
		}
	}
	return nil
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
		return deletedOne(tx.NewDelete().Model((*model.Product)(nil)).Where("id = ?", id).Exec(ctx))
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
	return deletedOne(s.DB.NewDelete().Model((*model.Plan)(nil)).Where("id = ?", id).Exec(ctx))
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
	// A plan sold per seat sets no max_seats of its own: the 1 it keeps
	// there moves with the product's type (see UpdateProductIn).
	case !supportsAct && !supportsSeats:
		q = q.Where("max_activations > 0 OR license_model = 'floating' OR (max_seats > 0 AND NOT per_seat)")
	case !supportsAct:
		q = q.Where("max_activations > 0 OR license_model = 'floating'")
	case !supportsSeats:
		q = q.Where("max_seats > 0 AND NOT per_seat")
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
	return deletedOne(s.DB.NewDelete().Model((*model.Entitlement)(nil)).Where("id = ?", id).Exec(ctx))
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
	return deletedOne(s.DB.NewDelete().Model((*model.APIKey)(nil)).Where("id = ?", id).Exec(ctx))
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

// ErrLicenseNotFound: no licence has the id given.
var ErrLicenseNotFound = errors.New("license not found")

func (s *Store) RevokeLicense(ctx context.Context, id string) error {
	return RunInTx(ctx, s.DB, func(ctx context.Context, tx bun.Tx) error {
		res, err := tx.NewUpdate().Model((*model.License)(nil)).
			Set("status = ?, updated_at = now()", model.StatusRevoked).
			Where("id = ?", id).Exec(ctx)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return ErrLicenseNotFound
		}
		if _, err := tx.NewDelete().Model((*model.Activation)(nil)).Where("license_id = ?", id).Exec(ctx); err != nil {
			return err
		}
		if _, err := tx.NewDelete().Model((*model.FloatingSession)(nil)).Where("license_id = ?", id).Exec(ctx); err != nil {
			return err
		}
		return syncSubscriptionStatus(ctx, tx, id, model.StatusRevoked)
	})
}

// ErrLicenseNotSuspendable refuses to suspend a licence that is not in
// use: suspending is for an active, trialing or past-due one. Above all
// not a revoked one — reinstating would then hand back what was revoked.
var ErrLicenseNotSuspendable = errors.New("only an active, trialing or past-due license can be suspended")

func (s *Store) SuspendLicense(ctx context.Context, id string) error {
	now := time.Now()
	return RunInTx(ctx, s.DB, func(ctx context.Context, tx bun.Tx) error {
		res, err := tx.NewUpdate().Model((*model.License)(nil)).
			Set("status = ?, suspended_at = ?, suspended_by = ?, updated_at = ?", model.StatusSuspended, now, model.SuspendedByAdmin, now).
			Where("id = ?", id).
			Where("status IN (?)", bun.In([]string{model.StatusActive, model.StatusTrialing, model.StatusPastDue})).
			Exec(ctx)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			var exists bool
			if err := tx.NewRaw("SELECT EXISTS (SELECT 1 FROM licenses WHERE id = ?)", id).Scan(ctx, &exists); err != nil {
				return err
			}
			if exists {
				return ErrLicenseNotSuspendable
			}
			return ErrLicenseNotFound
		}
		if _, err := tx.NewDelete().Model((*model.FloatingSession)(nil)).Where("license_id = ?", id).Exec(ctx); err != nil {
			return err
		}
		return syncSubscriptionStatus(ctx, tx, id, model.StatusSuspended)
	})
}

// usableStatusSQL is the status a usable licence takes back: "trialing"
// for a trial issued here, so the hourly trial sweep and the trial-ended
// mail still apply to it, and "active" for everything else.
//
// A Stripe-billed licence is always "active". The plan's type says how
// it started, not where it is now: a trial plan sold through Stripe goes
// on to be paid while the plan stays "trial", and only Stripe knows
// which side of that it is on. Its events move the licence from here —
// the charge at the trial end, a failed payment, a cancellation — while
// "trialing" would hand a paying customer to the trial sweep, which
// expires without the grace period, and cost them the renewal reminder.
const usableStatusSQL = `CASE WHEN COALESCE(licenses.stripe_subscription_id, '') = ''
	AND (SELECT license_type FROM plans WHERE plans.id = licenses.plan_id) = 'trial'
	THEN 'trialing' ELSE 'active' END`

// ErrReinstateNeedsNewExpiry refuses to reinstate an expired or canceled
// licence whose expiry date has passed: it would show as active, yet stay
// unusable and be expired again by the next hourly sweep. Setting a new
// expiry date reactivates it instead (SetLicenseValidUntil).
var ErrReinstateNeedsNewExpiry = errors.New("this license's expiry date has passed — set a new expiry date instead, which reactivates it")

// ReinstateLicense gives a suspended, expired or canceled licence back
// and returns the status it now has. The licence and its subscription
// row change in one transaction.
func (s *Store) ReinstateLicense(ctx context.Context, id string) (string, error) {
	var status string
	err := RunInTx(ctx, s.DB, func(ctx context.Context, tx bun.Tx) error {
		err := tx.NewRaw(`
			UPDATE licenses SET status = `+usableStatusSQL+`,
				suspended_at = NULL, suspended_by = NULL, canceled_at = NULL, updated_at = now()
			WHERE id = ? AND (status = 'suspended' OR
				(status IN ('expired', 'canceled') AND (valid_until IS NULL OR valid_until > now())))
			RETURNING status`, id).Scan(ctx, &status)
		if errors.Is(err, sql.ErrNoRows) {
			var lapsed bool
			if qerr := tx.NewRaw(`SELECT status IN ('expired', 'canceled') AND valid_until <= now()
				FROM licenses WHERE id = ?`, id).Scan(ctx, &lapsed); qerr == nil && lapsed {
				return ErrReinstateNeedsNewExpiry
			}
			return fmt.Errorf("license not found or cannot be reinstated from current status")
		}
		if err != nil {
			return err
		}
		return syncSubscriptionStatus(ctx, tx, id, status)
	})
	return status, err
}

// SetLicenseValidUntil moves a licence's expiry and returns its status
// before and after. An expired licence given a future (or no) expiry is
// usable again, so it takes back the status of its plan. Every other
// status is left alone: suspended, revoked and canceled are somebody's
// decision about the licence, and a date edit must not undo them.
//
// The row is locked while the decision is made, so a concurrent
// reinstate, suspend or expiry sweep cannot interleave with it.
func (s *Store) SetLicenseValidUntil(ctx context.Context, id string, validUntil *time.Time) (prev, status string, err error) {
	err = RunInTx(ctx, s.DB, func(ctx context.Context, tx bun.Tx) error {
		var licenseType, stripeSub sql.NullString
		if err := tx.NewRaw(`
			SELECT l.status, p.license_type, l.stripe_subscription_id FROM licenses l
			LEFT JOIN plans p ON p.id = l.plan_id
			WHERE l.id = ? FOR UPDATE OF l`, id).Scan(ctx, &prev, &licenseType, &stripeSub); err != nil {
			return err
		}
		// A trial issued here; a Stripe-billed licence's trial is
		// Stripe's to run (see usableStatusSQL).
		localTrial := licenseType.String == "trial" && stripeSub.String == ""
		status = prev
		if prev == model.StatusExpired && (validUntil == nil || validUntil.After(time.Now())) {
			status = model.StatusActive
			if localTrial {
				status = model.StatusTrialing
			}
		}
		if _, err := tx.NewRaw(`UPDATE licenses SET valid_until = ?, status = ?, updated_at = now() WHERE id = ?`,
			validUntil, status, id).Exec(ctx); err != nil {
			return err
		}
		// A trial's window is derived from the licence's deadline (see
		// SyncLicenseSubscriptionIn), and the customer page shows it.
		// Move it with the date so the two never disagree. Not for a
		// Stripe-billed licence: a date set here does not move Stripe's
		// trial, and on a paid one it would show a trial that is over.
		if localTrial {
			if _, err := tx.NewRaw(`
				UPDATE subscriptions SET trial_end = ?, updated_at = now()
				WHERE id = (SELECT id FROM subscriptions WHERE license_id = ? ORDER BY created_at DESC LIMIT 1)
				  AND trial_start IS NOT NULL`, validUntil, id).Exec(ctx); err != nil {
				return err
			}
		}
		if status == prev {
			return nil
		}
		return syncSubscriptionStatus(ctx, tx, id, status)
	})
	return prev, status, err
}

// ErrQuantityStripeManaged: the licence is billed by a Stripe
// subscription, whose quantity is the one it holds.
var ErrQuantityStripeManaged = errors.New("the quantity of a Stripe-billed license is its subscription's")

// ErrQuantityNotPerSeat: the licence's plan is not sold per seat, so a
// quantity would change nothing it holds.
var ErrQuantityNotPerSeat = errors.New("the license's plan is not sold per seat")

// QuantityRangeError: the quantity is outside what the licence's plan
// sells, Min to Max seats, as a checkout or the portal would refuse it.
type QuantityRangeError struct{ Min, Max int }

func (e *QuantityRangeError) Error() string {
	return fmt.Sprintf("the plan sells %d to %d seats", e.Min, e.Max)
}

// QuantityInUseError: the quantity is below what the licence uses, as
// the portal refuses a downgrade (Code SEATS_IN_USE or DEVICES_IN_USE,
// Kind activations or sessions for the latter). Nobody and nothing is
// removed by a change of seats, so the room is freed first.
type QuantityInUseError struct {
	Code, Kind   string
	InUse, Limit int
}

func (e *QuantityInUseError) Error() string { return e.Code }

// SetLicenseQuantity sets how many seats a licence nothing bills holds
// (an offline sale, say) and returns what it had. The licence row is
// locked for the decision, so a Stripe subscription linked meanwhile
// is seen, and refused: its quantity comes from Stripe alone.
func (s *Store) SetLicenseQuantity(ctx context.Context, id string, quantity int) (prev int, err error) {
	err = RunInTx(ctx, s.DB, func(ctx context.Context, tx bun.Tx) error {
		var row struct {
			Quantity       int            `bun:"quantity"`
			Subscription   sql.NullString `bun:"stripe_subscription_id"`
			PerSeat        sql.NullBool   `bun:"per_seat"`
			MinQuantity    sql.NullInt64  `bun:"min_quantity"`
			MaxQuantity    sql.NullInt64  `bun:"max_quantity"`
			MaxActivations sql.NullInt64  `bun:"max_activations"`
			LicenseModel   sql.NullString `bun:"license_model"`
		}
		if err := tx.NewRaw(`
			SELECT l.quantity, l.stripe_subscription_id, p.per_seat, p.min_quantity, p.max_quantity,
				p.max_activations, p.license_model FROM licenses l
			LEFT JOIN plans p ON p.id = l.plan_id
			WHERE l.id = ? FOR UPDATE OF l`, id).Scan(ctx, &row); err != nil {
			return err
		}
		if row.Subscription.String != "" {
			return ErrQuantityStripeManaged
		}
		if !row.PerSeat.Bool {
			return ErrQuantityNotPerSeat
		}
		if min, max := int(row.MinQuantity.Int64), int(row.MaxQuantity.Int64); quantity < min || quantity > max {
			return &QuantityRangeError{Min: min, Max: max}
		}
		prev = row.Quantity
		// Lower: not below what is in use, counted under the lock every
		// slot is taken under. One that leaves a licence over a limit it
		// was already over, but no further, goes ahead, as in the portal.
		if quantity < prev {
			used, err := countInUse(ctx, tx, id)
			if err != nil {
				return err
			}
			if used.People > quantity {
				return &QuantityInUseError{Code: "SEATS_IN_USE", InUse: used.People, Limit: quantity}
			}
			devices := int(row.MaxActivations.Int64) * quantity
			n, kind := used.Activations, "activations"
			if row.LicenseModel.String == "floating" && used.Sessions > devices && used.Sessions >= n {
				n, kind = used.Sessions, "sessions"
			}
			if devices > 0 && n > devices {
				return &QuantityInUseError{Code: "DEVICES_IN_USE", Kind: kind, InUse: n, Limit: devices}
			}
		}
		_, err := tx.NewRaw(`UPDATE licenses SET quantity = ?, updated_at = now() WHERE id = ?`, quantity, id).Exec(ctx)
		return err
	})
	return prev, err
}

// ExpireLicenseIf marks a licence expired only while it still qualifies:
// its status is one of statuses and, when before is set, its valid_until
// is still earlier than before. The expiry sweeps read their candidates
// first and write later. In between an admin may have extended or
// reinstated the licence, and an unconditional write would undo that.
// ok is false when the licence no longer qualified and nothing changed.
func (s *Store) ExpireLicenseIf(ctx context.Context, id string, statuses []string, before *time.Time) (ok bool, err error) {
	return s.expireLicenseWhere(ctx, id, func(q *bun.UpdateQuery) *bun.UpdateQuery {
		q = q.Where("status IN (?)", bun.List(statuses))
		if before != nil {
			q = q.Where("valid_until IS NOT NULL AND valid_until < ?", *before)
		}
		return q
	})
}

// IsTrialFallbackPlanConflict reports whether err is a plans CHECK
// refusing trial_fallback_plan_id: on a plan that is not a trial plan,
// or naming the plan itself. The admin API refuses both first; this is
// the answer when concurrent edits slipped past it.
func IsTrialFallbackPlanConflict(err error) bool {
	var pgErr pgdriver.Error
	return errors.As(err, &pgErr) && pgErr.Field('C') == "23514" &&
		(pgErr.Field('n') == "plans_trial_fallback_trial_only" || pgErr.Field('n') == "plans_trial_fallback_not_self")
}

// TrialMove says what MoveEndedTrialToFallback did.
type TrialMove struct {
	// Moved: the licence is now on To, and From is the trial plan it
	// left.
	Moved bool
	// Expire: the licence still qualifies for the trial sweep, but its
	// plan names no fallback, or one that no longer qualifies (Refusal
	// says why, for the log), or Stripe bills it. It was expired here,
	// in the same transaction and under the same locks that decided it,
	// as it was before fallbacks existed; the caller announces it.
	Expire  bool
	Refusal string
	// FallbackPlanID is the plan the trial plan names, if any.
	FallbackPlanID string
	License        *model.License
	From, To       *model.Plan
}

// MoveEndedTrialToFallback moves a licence whose trial ended before
// before onto the plan its trial plan names in trial_fallback_plan_id,
// in the shape that plan issues licences in: active, with no expiry and
// no update period — the same re-shape an admin's change-plan gives a
// licence leaving a trial. The key, activations, seats and add-ons stay
// as they are, so the customer keeps working on the new plan.
//
// It locks the licence and re-checks it first: the sweep read it
// earlier, and an admin may have extended, moved or reinstated it since
// (neither Moved nor Expire is set then; nothing changed). The fallback
// is locked against a concurrent edit and judged again here, as it
// stands now, so a plan repriced after it was named can never be handed
// out for nothing.
//
// A licence Stripe bills is left to Stripe, which owns its plan (see
// the admin change-plan): it expires as before.
func (s *Store) MoveEndedTrialToFallback(ctx context.Context, id string, before time.Time) (TrialMove, error) {
	var out TrialMove
	err := RunInTx(ctx, s.DB, func(ctx context.Context, tx bun.Tx) error {
		out = TrialMove{}
		lic, err := LockLicenseIn(ctx, tx, id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if lic.Status != model.StatusTrialing || lic.ValidUntil == nil || !lic.ValidUntil.Before(before) {
			return nil
		}
		// The trial plan held too (the licence, then its plan, then the
		// fallback): a switch of its fallback either waits for this move
		// or is what it reads, never a fallback it no longer names.
		from := new(model.Plan)
		if err := tx.NewSelect().Model(from).Where("id = ?", lic.PlanID).For("SHARE").Scan(ctx); err != nil {
			return err
		}
		out.FallbackPlanID = from.TrialFallbackPlanID
		// Expiring it is decided and done under these locks, so a fallback
		// named after the decision cannot be expired past.
		expire := func(refusal string) error {
			out.Expire, out.Refusal = true, refusal
			if _, err := tx.NewRaw("UPDATE licenses SET status = ?, updated_at = now() WHERE id = ?",
				model.StatusExpired, lic.ID).Exec(ctx); err != nil {
				return err
			}
			return syncSubscriptionStatus(ctx, tx, lic.ID, model.StatusExpired)
		}
		if from.TrialFallbackPlanID == "" || lic.StripeSubscriptionID != "" {
			return expire("")
		}
		// FOR SHARE: blocks an edit of the fallback (a new price, a
		// new type) until this write commits, without blocking the
		// key-share locks other licences take on it.
		to := new(model.Plan)
		err = tx.NewSelect().Model(to).Where("id = ?", from.TrialFallbackPlanID).For("SHARE").Scan(ctx)
		if errors.Is(err, sql.ErrNoRows) {
			return expire("plan not found")
		}
		if err != nil {
			return err
		}
		if why := to.TrialFallbackRefusal(lic.ProductID); why != "" {
			return expire(why)
		}
		l := *lic
		l.PlanID, l.Status, l.ValidUntil, l.UpdatesUntil = to.ID, model.StatusActive, nil, nil
		if err := UpdateLicenseAndSupersedeRenewalsIn(ctx, tx, &l,
			"plan_id", "status", "valid_until", "updates_until"); err != nil {
			return err
		}
		if err := SyncLicenseSubscriptionIn(ctx, tx, l.ID, to, l.Status, nil); err != nil {
			return err
		}
		out = TrialMove{Moved: true, FallbackPlanID: to.ID, License: &l, From: from, To: to}
		return nil
	})
	return out, err
}

// ExpireStalePastDueLicenseIf is ExpireLicenseIf for the dunning sweep:
// the licence must still be past_due, and past due since before the
// threshold — a licence that was paid and fell past due again after the
// sweep read it starts a fresh dunning clock.
func (s *Store) ExpireStalePastDueLicenseIf(ctx context.Context, id string, threshold time.Time) (bool, error) {
	return s.expireLicenseWhere(ctx, id, func(q *bun.UpdateQuery) *bun.UpdateQuery {
		return q.Where("status = ?", model.StatusPastDue).
			Where("COALESCE(past_due_at, updated_at) < ?", threshold)
	})
}

func (s *Store) expireLicenseWhere(ctx context.Context, id string, cond func(*bun.UpdateQuery) *bun.UpdateQuery) (ok bool, err error) {
	err = RunInTx(ctx, s.DB, func(ctx context.Context, tx bun.Tx) error {
		q := cond(tx.NewUpdate().Model((*model.License)(nil)).
			Set("status = ?", model.StatusExpired).
			Set("updated_at = now()").
			Where("id = ?", id))
		res, err := q.Exec(ctx)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		ok = true
		if _, err := tx.NewDelete().Model((*model.FloatingSession)(nil)).Where("license_id = ?", id).Exec(ctx); err != nil {
			return err
		}
		return syncSubscriptionStatus(ctx, tx, id, model.StatusExpired)
	})
	return ok, err
}

// syncSubscriptionStatus mirrors a licence's new status onto its
// subscription row, if it has one.
func syncSubscriptionStatus(ctx context.Context, tx bun.Tx, licenseID, status string) error {
	_, err := tx.NewRaw(`UPDATE subscriptions SET status = ?, updated_at = now() WHERE license_id = ? AND status != ?`,
		status, licenseID, status).Exec(ctx)
	return err
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

// ProductOfAudited is the product an audited entity belongs to, or ""
// when it has none, is gone, or cannot be read: an audit line is written
// regardless. A handler deleting an entity asks before the delete.
func (s *Store) ProductOfAudited(ctx context.Context, entity, entityID string) string {
	var q string
	switch entity {
	case "product":
		return entityID
	case "license", "plan", "addon", "webhook", "release", "api_key", "release_signing_key":
		q = "SELECT product_id FROM " + map[string]string{
			"license": "licenses", "plan": "plans", "addon": "addons", "webhook": "webhooks",
			"release": "releases", "api_key": "api_keys", "release_signing_key": "release_signing_keys",
		}[entity] + " WHERE id = ?"
	case "seat":
		q = "SELECT l.product_id FROM seats x JOIN licenses l ON l.id = x.license_id WHERE x.id = ?"
	case "activation":
		q = "SELECT l.product_id FROM activations x JOIN licenses l ON l.id = x.license_id WHERE x.id = ?"
	case "entitlement":
		q = "SELECT p.product_id FROM entitlements x JOIN plans p ON p.id = x.plan_id WHERE x.id = ?"
	case "release_artifact":
		q = "SELECT r.product_id FROM release_artifacts x JOIN releases r ON r.id = x.release_id WHERE x.id = ?"
	case "webhook_delivery":
		q = "SELECT w.product_id FROM webhook_deliveries x JOIN webhooks w ON w.id = x.webhook_id WHERE x.id = ?"
	default:
		return ""
	}
	var pid sql.NullString
	if err := s.DB.NewRaw(q, entityID).Scan(ctx, &pid); err != nil {
		return ""
	}
	return pid.String
}

// ListAuditLogs returns paginated audit-log rows. Filters compose
// with AND.
// auditOfProduct narrows an audit_logs query to one product: the lines
// recorded with it, and lines written before the product was recorded
// that are about the product or something that still belongs to it. The
// audit page and the analytics activity feed both scope through here.
func auditOfProduct(q *bun.SelectQuery, productID string) *bun.SelectQuery {
	return q.Where(`(product_id = ? OR (product_id IS NULL AND (
            (entity = 'product' AND entity_id = ?)
         OR entity_id IN (SELECT id FROM licenses  WHERE product_id = ?)
         OR entity_id IN (SELECT id FROM plans     WHERE product_id = ?)
         OR entity_id IN (SELECT id FROM addons    WHERE product_id = ?)
         OR entity_id IN (SELECT id FROM webhooks  WHERE product_id = ?)
         OR entity_id IN (SELECT id FROM releases  WHERE product_id = ?)
         OR entity_id IN (SELECT id FROM api_keys  WHERE product_id = ?)
        )))`, productID, productID, productID, productID, productID, productID, productID, productID)
}

func (s *Store) ListAuditLogs(ctx context.Context, entity, entityID, productID string, offset, limit int) ([]*model.AuditLog, int, error) {
	q := s.DB.NewSelect().Model((*model.AuditLog)(nil)).OrderExpr("created_at DESC, id DESC")
	if entity != "" {
		q = q.Where("entity = ?", entity)
	}
	if entityID != "" {
		q = q.Where("entity_id = ?", entityID)
	}
	if productID != "" {
		q = auditOfProduct(q, productID)
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

// FindActivationByID loads one activation.
func (s *Store) FindActivationByID(ctx context.Context, id string) (*model.Activation, error) {
	a := new(model.Activation)
	err := s.DB.NewSelect().Model(a).Where("id = ?", id).Scan(ctx)
	return a, err
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

// TrialFallbackInUseError: trial plans name the plan as their fallback,
// so it cannot stop being one they can move to, nor go.
type TrialFallbackInUseError struct{ Plans []string }

func (e *TrialFallbackInUseError) Error() string {
	return "the fallback of trial plans " + strings.Join(e.Plans, ", ")
}

// TrialFallbackRefusedError: the plan named as a trial's fallback cannot
// be one, as it reads under its lock; the message says why, worded as
// the admin API's own check words it.
type TrialFallbackRefusedError struct{ Msg string }

func (e *TrialFallbackRefusedError) Error() string { return e.Msg }

// GuardTrialFallbackTargetIn refuses with *TrialFallbackInUseError when
// trial plans name planID as their fallback. The caller holds planID's
// row lock (LockPlanIn), which a trial plan naming it waits on (see
// LockTrialFallbackIn): either that write committed first and is seen
// here, or it waits and then sees this one.
func GuardTrialFallbackTargetIn(ctx context.Context, tx bun.IDB, planID string) error {
	var names []string
	err := tx.NewRaw("SELECT name FROM plans WHERE trial_fallback_plan_id = ? ORDER BY name", planID).Scan(ctx, &names)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if len(names) > 0 {
		return &TrialFallbackInUseError{Plans: names}
	}
	return nil
}

// LockTrialFallbackIn takes a share lock on targetID, the plan a trial
// plan of productID is to name as its fallback, and checks it can be
// one as it reads under the lock: an edit or delete of the target holds
// its row lock across its own check (GuardTrialFallbackTargetIn), so the
// two never both pass.
func LockTrialFallbackIn(ctx context.Context, tx bun.IDB, targetID, productID string) error {
	target := new(model.Plan)
	err := tx.NewSelect().Model(target).Where("id = ?", targetID).For("SHARE").Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return &TrialFallbackRefusedError{Msg: "trial_fallback_plan_id: plan not found"}
	}
	if err != nil {
		return err
	}
	if why := target.TrialFallbackRefusal(productID); why != "" {
		return &TrialFallbackRefusedError{Msg: "trial_fallback_plan_id " + why}
	}
	return nil
}

// DeletePlanUnlessFallback deletes a plan, unless licences are on it
// (ErrPlanHasLicenses) or trial plans name it as their fallback
// (*TrialFallbackInUseError): the checks and the delete under the plan's
// row lock, which a licence insert's foreign key waits on.
func (s *Store) DeletePlanUnlessFallback(ctx context.Context, id string) error {
	return RunInTx(ctx, s.DB, func(ctx context.Context, tx bun.Tx) error {
		if err := LockPlanIn(ctx, tx, id); err != nil {
			return err
		}
		n, err := PlanLicenseCountIn(ctx, tx, id)
		if err != nil {
			return err
		}
		if n > 0 {
			return ErrPlanHasLicenses
		}
		if err := GuardTrialFallbackTargetIn(ctx, tx, id); err != nil {
			return err
		}
		return deletedOne(tx.NewDelete().Model((*model.Plan)(nil)).Where("id = ?", id).Exec(ctx))
	})
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
