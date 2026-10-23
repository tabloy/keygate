package payment

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stripe/stripe-go/v82"
	"github.com/stripe/stripe-go/v82/checkout/session"
	stripeinvoice "github.com/stripe/stripe-go/v82/invoice"
	"github.com/stripe/stripe-go/v82/invoicepayment"
	"github.com/stripe/stripe-go/v82/paymentintent"
	stripeprice "github.com/stripe/stripe-go/v82/price"
	"github.com/stripe/stripe-go/v82/subscription"
	"github.com/uptrace/bun"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
	"github.com/tabloy/keygate/pkg/response"
)

// Upgrading a licence Stripe doesn't bill — a free plan's, issued by an
// admin or claimed through self sign-up — to a paid plan of the same
// product. The customer pays through an ordinary checkout, and the
// session moves *that* licence to the plan: the key and activations
// stay, the entitlements change at the next verify. Without this the
// purchase made a second licence with a new key to activate everywhere.
//
// Each checkout is a row in license_upgrades (see store/upgrades.go). It
// remembers the licence as the upgrade found it, so a full refund or the
// end of the subscription puts the licence back on its free plan rather
// than revoking or cancelling a licence the customer had before paying.
//
// The session names the upgrade, the licence and the plan in its
// metadata (kind "upgrade"); fulfillCheckout hands such sessions here,
// from the webhook, the success page and the sync alike. The payment
// intent (one-time) or the subscription carries the upgrade id too, so a
// refund can find the upgrade even before it was applied.

const (
	kindUpgrade   = "upgrade"
	metaPlanID    = "plan_id"
	metaUpgradeID = "upgrade_id"
)

// upgradeIneligibleProvider records, once per session, that a paid
// upgrade could not be applied, so the audit log says it once.
const upgradeIneligibleProvider = "stripe_upgrade_ineligible"

// errUpgradeIneligible means the licence or plan changed between the
// checkout and its payment so that the upgrade no longer applies.
var errUpgradeIneligible = errors.New("upgrade no longer applies")

// errUpgradeClosed means the upgrade will never apply and nothing is
// left to do for its session: refunded before it was applied, paid for
// a subscription that had already ended, or applied by another caller.
var errUpgradeClosed = errors.New("upgrade closed")

// errUpgradeInProgress means a previous checkout for the licence has
// been paid and is still being applied.
var errUpgradeInProgress = errors.New("an upgrade payment for this license is still being processed")

// upgradeable reports whether lic, on plan current, can be upgraded by a
// checkout: its plan is free, Stripe does not bill it already (that
// licence changes plan through change-plan), and nobody suspended or
// revoked it. A lapsed one may still upgrade — buying is how it comes
// back. A licence an admin issued on a priced plan (paid by invoice,
// through a reseller) is not free and is left alone: "upgrading" a
// lifetime licence to a monthly plan would end it with the subscription.
func upgradeable(lic *model.License, current *model.Plan) bool {
	if current == nil || current.ID != lic.PlanID || !current.IsFree() {
		return false
	}
	if lic.StripeSubscriptionID != "" || lic.PaymentProvider == "stripe" {
		return false
	}
	switch lic.Status {
	case model.StatusActive, model.StatusTrialing, model.StatusExpired, model.StatusCanceled:
		return true
	}
	return false
}

// upgradeTarget reports whether lic may be upgraded to plan: a priced,
// active plan of the same product. Plans selling an update period and
// trial plans are left out — the period needs the gating checks a new
// sale gets, and nobody pays to start a trial.
func upgradeTarget(lic *model.License, plan *model.Plan) bool {
	return plan.Active && plan.ProductID == lic.ProductID && plan.ID != lic.PlanID &&
		plan.StripePriceID != "" && plan.LicenseType != "trial" &&
		plan.InitialUpdatesUntil(time.Now()) == nil
}

// UpgradableLicenses reports which of the licences email owns can be
// upgraded right now: Stripe is configured, the licence qualifies, and
// its product has a plan to upgrade to. The portal shows the Upgrade
// button from this; StartUpgrade checks again regardless.
func (h *StripeHandler) UpgradableLicenses(ctx context.Context, email string, ls []*model.License) (map[string]bool, error) {
	out := map[string]bool{}
	if stripe.Key == "" {
		return out, nil
	}
	var candidates []*model.License
	seen := map[string]bool{}
	var products []string
	for _, l := range ls {
		if !strings.EqualFold(l.Email, email) || !upgradeable(l, l.Plan) {
			continue
		}
		candidates = append(candidates, l)
		if !seen[l.ProductID] {
			seen[l.ProductID] = true
			products = append(products, l.ProductID)
		}
	}
	if len(candidates) == 0 {
		return out, nil
	}
	plans, err := h.Store.ListUpgradeTargetPlans(ctx, products)
	if err != nil {
		return nil, err
	}
	for _, l := range candidates {
		for _, p := range plans {
			if upgradeTarget(l, p) {
				out[l.ID] = true
				break
			}
		}
	}
	return out, nil
}

// upgradeLockKey names the advisory lock StartUpgrade holds per licence,
// so two clicks cannot both open a checkout.
func upgradeLockKey(licenseID string) int64 {
	hash := fnv.New64a()
	hash.Write([]byte("upgrade:" + licenseID))
	return int64(hash.Sum64())
}

// StartUpgrade starts a checkout that moves one of the caller's
// licences to a paid plan. A checkout opened earlier for the licence is
// expired first: two paid checkouts would leave the second subscription
// billing with nothing to apply it to.
//
// POST /portal/licenses/upgrade  { license_id, plan_id }  → { url }
func (h *StripeHandler) StartUpgrade(c *gin.Context) {
	var req struct {
		LicenseID string `json:"license_id" binding:"required"`
		PlanID    string `json:"plan_id" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "license_id and plan_id are required")
		return
	}
	lic, err := h.Store.FindLicenseByID(c, req.LicenseID)
	if err != nil {
		response.NotFound(c, "license not found")
		return
	}
	// Only the owner buys for a licence, as with renewals and plan
	// changes; seat holders use it but do not pay for it.
	emailVal, _ := c.Get("email")
	if e, ok := emailVal.(string); !ok || !strings.EqualFold(lic.Email, e) {
		response.Forbidden(c, "not your license")
		return
	}
	plan, err := h.Store.FindPlanByID(c, req.PlanID)
	if err != nil {
		response.NotFound(c, "plan not found")
		return
	}
	current, err := h.Store.FindPlanByID(c, lic.PlanID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		response.Internal(c, err)
		return
	}
	if !upgradeable(lic, current) || !upgradeTarget(lic, plan) {
		response.Err(c, http.StatusBadRequest, "UPGRADE_NOT_AVAILABLE", "this license cannot be upgraded to that plan")
		return
	}

	// Stripe decides what the price is, as for CheckoutByPlan; the
	// session mode has to match the licence the plan issues, or the
	// customer would pay once for a subscription or monthly for a
	// perpetual licence.
	sp, perr := stripeprice.Get(plan.StripePriceID, nil)
	mode := stripe.CheckoutSessionModeSubscription
	if perr == nil && sp.Type == "one_time" {
		mode = stripe.CheckoutSessionModePayment
	}
	wantMode := stripe.CheckoutSessionModeSubscription
	if plan.LicenseType == "perpetual" {
		wantMode = stripe.CheckoutSessionModePayment
	}
	if perr != nil || !sp.Active || mode != wantMode {
		slog.Error("stripe upgrade: the plan's price cannot be sold as this plan's licence type",
			"plan_id", plan.ID, "price_id", plan.StripePriceID, "license_type", plan.LicenseType, "error", perr)
		response.Err(c, http.StatusServiceUnavailable, "UPGRADE_UNAVAILABLE",
			"upgrades to this plan are temporarily unavailable; please try again later")
		return
	}

	var url string
	err = h.Store.WithXactLock(c, upgradeLockKey(lic.ID), func(ctx context.Context, tx bun.Tx) error {
		// The pending rows first: a fulfilment of one of them holds its
		// row, so this waits for it to commit. Only then is the licence
		// read again, so an upgrade applied meanwhile is seen and the
		// licence, billed already, is refused another checkout.
		pending, err := store.ListPendingUpgradesIn(ctx, tx, lic.ID)
		if err != nil {
			return err
		}
		now := new(model.License)
		if err := tx.NewSelect().Model(now).Where("id = ?", lic.ID).Scan(ctx); err != nil {
			return err
		}
		cur, err := store.FindPlanByIDIn(ctx, tx, now.PlanID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if !upgradeable(now, cur) {
			return errUpgradeIneligible
		}
		for _, p := range pending {
			if err := expireCheckout(p.StripeCheckoutSessionID); err != nil {
				return err
			}
			if err := store.EndLicenseUpgradeIn(ctx, tx, p, model.UpgradeExpired, "replaced"); err != nil {
				return err
			}
		}

		upgradeID := store.NewLicenseUpgradeID()
		tags := map[string]string{metaKind: kindUpgrade, metaUpgradeID: upgradeID, metaLicenseID: lic.ID}
		params := &stripe.CheckoutSessionParams{
			Mode: stripe.String(string(mode)),
			LineItems: []*stripe.CheckoutSessionLineItemParams{
				{Price: stripe.String(plan.StripePriceID), Quantity: stripe.Int64(1)},
			},
			SuccessURL:          stripe.String(h.BaseURL + "/checkout/success?session_id={CHECKOUT_SESSION_ID}"),
			CancelURL:           stripe.String(h.BaseURL + "/portal"),
			AllowPromotionCodes: stripe.Bool(true),
		}
		if now.StripeCustomerID != "" {
			params.Customer = stripe.String(now.StripeCustomerID)
		} else {
			params.CustomerEmail = stripe.String(now.Email)
		}
		params.Metadata = map[string]string{
			metaKind:        kindUpgrade,
			metaUpgradeID:   upgradeID,
			metaLicenseID:   lic.ID,
			metaPlanID:      plan.ID,
			"product_id":    plan.ProductID,
			metaLicenseType: plan.LicenseType,
		}
		// The payment (one-time) or the subscription names the upgrade
		// too: a refund arrives with nothing else to find it by.
		if mode == stripe.CheckoutSessionModePayment {
			params.PaymentIntentData = &stripe.CheckoutSessionPaymentIntentDataParams{Metadata: tags}
		} else {
			params.SubscriptionData = &stripe.CheckoutSessionSubscriptionDataParams{Metadata: tags}
		}
		sess, err := session.New(params)
		if err != nil {
			return fmt.Errorf("create checkout session: %w", err)
		}
		if err := store.CreateLicenseUpgradeIn(ctx, tx, &model.LicenseUpgrade{
			ID: upgradeID, LicenseID: lic.ID, ToPlanID: plan.ID, StripeCheckoutSessionID: sess.ID,
		}); err != nil {
			// Not recorded: close the checkout rather than leave one open
			// that nothing would replace. Fulfilment copes with a session
			// that has no row, should it be paid regardless.
			_ = expireCheckout(sess.ID)
			return fmt.Errorf("record upgrade: %w", err)
		}
		url = sess.URL
		return nil
	})
	if errors.Is(err, errUpgradeIneligible) {
		response.Err(c, http.StatusBadRequest, "UPGRADE_NOT_AVAILABLE", "this license cannot be upgraded to that plan")
		return
	}
	if errors.Is(err, errUpgradeInProgress) {
		response.Err(c, http.StatusConflict, "UPGRADE_IN_PROGRESS",
			"a payment for an upgrade of this license is still being processed; try again in a few minutes")
		return
	}
	if err != nil {
		slog.Error("stripe upgrade: failed to start checkout", "license_id", lic.ID, "error", err)
		response.Internal(c, err)
		return
	}
	response.OK(c, gin.H{"url": url})
}

// expireCheckout closes a checkout session so it can no longer be paid.
// One that is already expired is fine; one that has been completed
// answers errUpgradeInProgress, since its payment is on its way.
func expireCheckout(sessionID string) error {
	if stripe.Key == "" {
		return nil
	}
	if _, err := session.Expire(sessionID, nil); err == nil {
		return nil
	}
	sess, err := session.Get(sessionID, nil)
	if stripeNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read checkout session %s: %w", sessionID, err)
	}
	switch sess.Status {
	case stripe.CheckoutSessionStatusExpired:
		return nil
	case stripe.CheckoutSessionStatusComplete:
		return errUpgradeInProgress
	}
	return fmt.Errorf("checkout session %s could not be expired (status %s)", sessionID, sess.Status)
}

// subscriptionLive reports whether a subscription in this status still
// bills and grants access.
func subscriptionLive(status string) bool {
	switch status {
	case "active", "trialing", "past_due":
		return true
	}
	return false
}

// upgradeStillPaid asks Stripe whether what the session paid for still
// stands: the subscription is live, or the one-time payment was not
// refunded. "" when it does (or Stripe is not configured, or cannot see
// the object); otherwise the state the upgrade should close in.
func (h *StripeHandler) upgradeStillPaid(subscriptionID, paymentIntentID string) (closeAs string, cur *subscriptionEvent, err error) {
	if stripe.Key == "" {
		return "", nil, nil
	}
	if subscriptionID != "" {
		cur, err := h.currentSubscription(subscriptionID)
		if err != nil {
			return "", nil, err
		}
		if cur != nil && !subscriptionLive(cur.Status) {
			// Not over either (unpaid, paused, incomplete): paying its open
			// invoice later would bill for an upgrade that never applied.
			if cur.Status != "canceled" && cur.Status != "incomplete_expired" {
				if err := cancelSubscription(subscriptionID); err != nil {
					return "", nil, err
				}
			}
			return model.UpgradeVoid, cur, nil
		}
		return "", cur, nil
	}
	if paymentIntentID == "" {
		return "", nil, nil
	}
	params := &stripe.PaymentIntentParams{}
	params.AddExpand("latest_charge")
	pi, err := paymentintent.Get(paymentIntentID, params)
	if stripeNotFound(err) {
		return "", nil, nil
	}
	if err != nil {
		return "", nil, fmt.Errorf("fetch payment intent %s: %w", paymentIntentID, err)
	}
	if pi.LatestCharge != nil && pi.LatestCharge.Refunded {
		return model.UpgradeRefunded, nil, nil
	}
	return "", nil, nil
}

// fulfillUpgrade applies a paid upgrade session to the licence it
// names. Idempotent per session: the upgrade row says whether it was
// applied (or closed), whatever the licence looks like since. Returns
// false with no error when the upgrade cannot apply (the licence was
// billed, suspended or revoked meanwhile); the session then stays
// pending for an operator to refund, and that refund closes it.
func (h *StripeHandler) fulfillUpgrade(ctx context.Context, customerID, subscriptionID, paymentIntentID string,
	metadata map[string]string, sessionID, source string) (bool, error) {
	licenseID, planID := metadata[metaLicenseID], metadata[metaPlanID]
	if sessionID == "" || licenseID == "" || planID == "" {
		slog.Error("stripe upgrade: session is missing its licence or plan", "session_id", sessionID)
		return false, nil
	}

	// Read before Stripe is asked, so the subscription read below can be
	// applied after the commit as a read taken at this instant.
	asOf, err := h.Store.StripeReadStamp(ctx)
	if err != nil {
		return false, err
	}
	closeAs, cur, err := h.upgradeStillPaid(subscriptionID, paymentIntentID)
	if err != nil {
		return false, err
	}

	claimed, err := h.claimSession(ctx, sessionID)
	if err != nil {
		return false, fmt.Errorf("claim session: %w", err)
	}
	if !claimed {
		return h.sessionFulfilled(ctx, sessionID)
	}

	var oldPlanID string
	var lic *model.License
	var newPlan *model.Plan
	var upgrade *model.LicenseUpgrade
	// closedNow: this call closed the upgrade (refunded, or its
	// subscription ended, before it applied). The close has to commit,
	// so the transaction returns nil and this says what happened.
	closedNow := false
	err = h.Store.RunInTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		locked, err := store.LockLicenseIn(ctx, tx, licenseID)
		if errors.Is(err, sql.ErrNoRows) {
			return errUpgradeIneligible
		}
		if err != nil {
			return err
		}
		u, err := store.LockLicenseUpgradeBySessionIn(ctx, tx, sessionID)
		if errors.Is(err, sql.ErrNoRows) {
			// A checkout whose row was never written (StartUpgrade
			// failed after creating it) and was paid regardless.
			u = &model.LicenseUpgrade{ID: metadata[metaUpgradeID], LicenseID: licenseID, ToPlanID: planID, StripeCheckoutSessionID: sessionID}
			if err := store.CreateLicenseUpgradeIn(ctx, tx, u); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		upgrade = u
		switch u.State {
		case model.UpgradePending, model.UpgradeExpired:
			// Expired only means a newer checkout replaced it after it
			// was paid; the payment still stands.
		default:
			return errUpgradeClosed
		}
		if closeAs != "" {
			reason := "refunded_before_applied"
			if closeAs == model.UpgradeVoid {
				reason = "subscription_ended_before_applied"
			}
			if err := store.EndLicenseUpgradeIn(ctx, tx, u, closeAs, reason); err != nil {
				return err
			}
			closedNow = true
			return nil
		}
		if err := store.LockReferencedRowsIn(ctx, tx, planID, locked.ProductID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return errUpgradeIneligible
			}
			return err
		}
		plan, err := store.FindPlanByIDIn(ctx, tx, planID)
		if err != nil {
			return err
		}
		current, err := store.FindPlanByIDIn(ctx, tx, locked.PlanID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		// The licence and the plan as they read now: both may have
		// changed since the customer opened the checkout. The plan has
		// to issue the licence type that was sold, and a subscription
		// plan needs the subscription that pays for it.
		if !upgradeable(locked, current) || plan.ProductID != locked.ProductID || plan.ID == locked.PlanID ||
			plan.LicenseType != metadata[metaLicenseType] || plan.InitialUpdatesUntil(time.Now()) != nil ||
			(plan.LicenseType == "subscription") != (subscriptionID != "") {
			return errUpgradeIneligible
		}

		u.ToPlanID = plan.ID
		u.StripePaymentIntentID, u.StripeSubscriptionID = paymentIntentID, subscriptionID
		u.FromPlanID, u.FromStatus = locked.PlanID, locked.Status
		u.FromValidUntil, u.FromUpdatesUntil = locked.ValidUntil, locked.UpdatesUntil
		u.FromUpdatesTermsSet, u.FromPaymentProvider = locked.UpdatesTermsSet, locked.PaymentProvider
		u.FromStripeCustomerID, u.FromCanceledAt = locked.StripeCustomerID, locked.CanceledAt
		if err := store.MarkLicenseUpgradeAppliedIn(ctx, tx, u); err != nil {
			return err
		}

		oldPlanID = locked.PlanID
		l := *locked
		l.PlanID = plan.ID
		l.Status = model.StatusActive
		// A subscription's end comes from Stripe (read below once this
		// commits), a perpetual licence has none; a free plan's
		// deadline goes either way.
		l.ValidUntil = nil
		l.CanceledAt = nil
		l.PastDueAt = nil
		// The target sells no update period: a subscription follows
		// valid_until, a perpetual licence gets updates for life.
		l.UpdatesUntil = nil
		l.UpdatesTermsSet = true
		l.PaymentProvider = "stripe"
		if customerID != "" {
			l.StripeCustomerID = customerID
		}
		l.StripeSubscriptionID = subscriptionID
		l.StripePaymentIntentID = paymentIntentID
		l.StripeCheckoutSessionID = sessionID
		cols := []string{"plan_id", "status", "valid_until", "canceled_at", "past_due_at",
			"updates_until", "updates_terms_set", "payment_provider", "stripe_customer_id",
			"stripe_subscription_id", "stripe_payment_intent_id", "stripe_checkout_session_id"}
		// The subscription as just read from Stripe gives the licence its
		// period and status in the same write, rather than in a sync
		// after the commit that nothing retries if it fails.
		if cur != nil {
			if end := cur.PeriodEnd(); end > 0 {
				until := time.Unix(end, 0)
				l.ValidUntil = &until
			}
			switch cur.Status {
			case "trialing":
				l.Status = model.StatusTrialing
			case "past_due":
				l.Status = model.StatusPastDue
				pastDue := time.Now()
				l.PastDueAt = &pastDue
			}
			l.StripeSyncedAt = &asOf
			cols = append(cols, "stripe_synced_at")
		}
		update := store.UpdateLicenseIn
		if locked.UpdatesUntil != nil {
			// The old period's renewals belong to a period this licence
			// no longer has.
			update = store.UpdateLicenseAndSupersedeRenewalsIn
		}
		if err := update(ctx, tx, &l, cols...); err != nil {
			return err
		}
		if err := store.SyncLicenseSubscriptionIn(ctx, tx, l.ID, plan, l.Status, l.ValidUntil); err != nil {
			return err
		}
		// A subscription row reused from an earlier upgrade must not hand
		// this subscription its cancel state.
		if err := store.ResetSubscriptionCancelStateIn(ctx, tx, l.ID); err != nil {
			return err
		}
		lic, newPlan = &l, plan
		return nil
	})
	if err == nil && closedNow {
		err = errUpgradeClosed
	}
	switch {
	case err == nil:
	case store.IsCheckoutSessionConflict(err):
		slog.Warn("stripe upgrade: session applied concurrently", "session_id", sessionID)
		return true, nil
	case errors.Is(err, errUpgradeClosed):
		// Nothing will ever apply for this session: it is done.
		if err := h.Store.CompleteProcessedEvent(ctx, sessionClaimProvider, sessionID); err != nil {
			slog.Error("stripe upgrade: failed to record session completion", "session_id", sessionID, "error", err)
		}
		_ = h.Store.DeleteProcessedEvent(ctx, pendingSessionProvider, sessionID)
		if closedNow {
			h.Store.Audit(ctx, &model.AuditLog{
				Entity: "license", EntityID: licenseID, Action: "upgrade_not_applied",
				ActorType: source,
				Changes: map[string]any{
					"session_id": sessionID, "plan_id": planID, "state": upgrade.State, "reason": upgrade.EndReason,
					"provider": "stripe",
				},
			})
			slog.Warn("stripe upgrade: paid upgrade not applied, the payment no longer stands",
				"license_id", licenseID, "session_id", sessionID, "state", upgrade.State, "reason", upgrade.EndReason)
		}
		return true, nil
	case errors.Is(err, errUpgradeIneligible):
		if derr := h.release(ctx, fulfilledSessionProvider, sessionClaimProvider, sessionID); derr != nil {
			slog.Error("stripe upgrade: failed to release session claim", "session_id", sessionID, "error", derr)
		}
		if first, _ := h.Store.ClaimProcessedEvent(ctx, upgradeIneligibleProvider, sessionID); first {
			h.Store.Audit(ctx, &model.AuditLog{
				Entity: "license", EntityID: licenseID, Action: "upgrade_ineligible",
				ActorType: source,
				Changes: map[string]any{
					"session_id": sessionID, "plan_id": planID, "payment_intent": paymentIntentID,
					"subscription": subscriptionID, "provider": "stripe",
				},
			})
		}
		slog.Warn("stripe upgrade: paid upgrade cannot be applied — the licence was removed, revoked, suspended or is billed by Stripe already, or the plan changed; refund it (and cancel the subscription, for a subscription plan)",
			"license_id", licenseID, "plan_id", planID, "session_id", sessionID, "source", source)
		return false, nil
	default:
		if derr := h.release(ctx, fulfilledSessionProvider, sessionClaimProvider, sessionID); derr != nil {
			slog.Error("stripe upgrade: failed to release session claim", "session_id", sessionID, "error", derr)
		}
		return false, fmt.Errorf("apply upgrade: %w", err)
	}
	if err := h.Store.CompleteProcessedEvent(ctx, sessionClaimProvider, sessionID); err != nil {
		slog.Error("stripe upgrade: failed to record session completion", "session_id", sessionID, "error", err)
	}
	_ = h.Store.DeleteProcessedEvent(ctx, pendingSessionProvider, sessionID)
	_ = h.Store.DeleteProcessedEvent(ctx, upgradeIneligibleProvider, sessionID)

	// Whether the subscription renews or ends at its period end. The
	// state was reset in the transaction; should this fail, the
	// cancel-state sync asks Stripe about it.
	if cur != nil {
		if err := h.Store.SetSubscriptionCancelScheduled(ctx, lic.ID, cur.EndsThisPeriod(), asOf); err != nil {
			slog.Warn("stripe upgrade: cancel state not recorded yet; the sync will", "license_id", lic.ID, "error", err)
		}
	}

	h.Store.Audit(ctx, &model.AuditLog{
		Entity: "license", EntityID: lic.ID, Action: "plan_changed",
		ActorType: source,
		Changes: map[string]any{
			"old_plan_id": oldPlanID, "new_plan_id": newPlan.ID, "kind": kindUpgrade,
			"upgrade_id": upgrade.ID, "session_id": sessionID, "provider": "stripe",
		},
	})
	if h.WebhookSvc != nil {
		h.WebhookSvc.Dispatch(ctx, lic.ProductID, model.EventPlanChanged, map[string]any{
			"license_id": lic.ID, "old_plan_id": oldPlanID, "new_plan_id": newPlan.ID,
		})
	}
	h.mailPlanChanged(ctx, lic, oldPlanID, newPlan.Name)
	slog.Info("stripe upgrade: applied", "license_id", lic.ID, "old_plan", oldPlanID, "new_plan", newPlan.ID, "source", source)
	return true, nil
}

func (h *StripeHandler) mailPlanChanged(ctx context.Context, lic *model.License, oldPlanID, newPlanName string) {
	if h.Email == nil || lic.Email == "" {
		return
	}
	productName, oldPlanName := "", ""
	if p, err := h.Store.FindProductByID(ctx, lic.ProductID); err == nil {
		productName = p.Name
	}
	if p, err := h.Store.FindPlanByID(ctx, oldPlanID); err == nil {
		oldPlanName = p.Name
	}
	h.Email.SendPlanChanged(lic.Email, productName, oldPlanName, newPlanName)
}

// Why an applied upgrade was undone (LicenseUpgrade.EndReason).
const (
	upgradeEndRefund      = "refund"
	upgradeEndCanceled    = "subscription_ended"
	upgradeEndUnpaid      = "subscription_unpaid"
	upgradeEndPastDue     = "past_due_timeout"
	upgradeEndGrace       = "grace_period_ended"
	upgradeEndRefundEarly = "refunded_before_applied"
	upgradeEndAdmin       = "admin_canceled"
)

// revertUpgrade undoes an applied upgrade: the licence goes back to the
// plan, status and dates it had before, keeping its key. An upgrade no
// longer applied (reverted meanwhile, by another event) reports nothing
// done, which is not an error. See store.UpgradeRevert for the outcomes
// a caller has to handle.
func (h *StripeHandler) revertUpgrade(ctx context.Context, upgradeID, reason, source string) (store.UpgradeRevert, error) {
	var res store.UpgradeRevert
	err := h.Store.RunInTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		u, err := store.LockLicenseUpgradeIn(ctx, tx, upgradeID)
		if err != nil {
			return err
		}
		if u.State != model.UpgradeApplied {
			return nil
		}
		res, err = store.RevertLicenseUpgradeIn(ctx, tx, u, reason)
		return err
	})
	if err != nil {
		return res, fmt.Errorf("revert upgrade %s: %w", upgradeID, err)
	}
	switch {
	case res.FreePlanGone:
		slog.Error("stripe upgrade: the free plan this licence came from was deleted; it is handled as before upgrades existed",
			"upgrade_id", upgradeID, "license_id", res.License.ID, "reason", reason)
		h.Store.Audit(ctx, &model.AuditLog{
			Entity: "license", EntityID: res.License.ID, Action: "upgrade_not_reverted",
			ActorType: source,
			Changes:   map[string]any{"upgrade_id": upgradeID, "reason": reason, "why": "free_plan_deleted", "provider": "stripe"},
		})
	case res.Unlinked && res.License != nil:
		slog.Info("stripe upgrade: licence no longer carries the upgrade, left as it is",
			"upgrade_id", upgradeID, "license_id", res.License.ID, "reason", reason)
	}
	if !res.Reverted {
		return res, nil
	}
	lic, plan := res.License, res.Plan
	h.Store.Audit(ctx, &model.AuditLog{
		Entity: "license", EntityID: lic.ID, Action: "upgrade_reverted",
		ActorType: source,
		Changes: map[string]any{
			"upgrade_id": upgradeID, "reason": reason, "old_plan_id": res.PrevPlanID, "new_plan_id": plan.ID,
			"status": lic.Status, "provider": "stripe",
		},
	})
	if h.WebhookSvc != nil {
		h.WebhookSvc.Dispatch(ctx, lic.ProductID, model.EventPlanChanged, map[string]any{
			"license_id": lic.ID, "old_plan_id": res.PrevPlanID, "new_plan_id": plan.ID, "reason": reason,
		})
	}
	h.mailPlanChanged(ctx, lic, res.PrevPlanID, plan.Name)
	slog.Info("stripe upgrade: reverted", "license_id", lic.ID, "upgrade_id", upgradeID, "reason", reason)
	return res, nil
}

// cancelSubscription ends a subscription in Stripe now. One already
// ended, or one this key cannot see, is left as it is.
func cancelSubscription(subscriptionID string) error {
	if stripe.Key == "" || subscriptionID == "" {
		return nil
	}
	_, err := subscription.Cancel(subscriptionID, nil)
	if err == nil || stripeNotFound(err) {
		return nil
	}
	sub, gerr := subscription.Get(subscriptionID, nil)
	if gerr == nil && (sub.Status == stripe.SubscriptionStatusCanceled || sub.Status == stripe.SubscriptionStatusIncompleteExpired) {
		return nil
	}
	return fmt.Errorf("cancel subscription %s: %w", subscriptionID, err)
}

// appliedUpgradeFor returns the upgrade in effect on lic when it is the
// one subscriptionID pays for, or nil.
func (h *StripeHandler) appliedUpgradeFor(ctx context.Context, lic *model.License, subscriptionID string) (*model.LicenseUpgrade, error) {
	u, err := h.Store.FindAppliedUpgrade(ctx, lic.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if subscriptionID == "" || u.StripeSubscriptionID != subscriptionID {
		return nil, nil
	}
	return u, nil
}

// endSubscriptionUpgrade handles the end of a subscription an upgrade
// started: the licence goes back to its free plan. cancelFirst cancels
// the subscription in Stripe first (unpaid, lapsed), since paying its
// open invoice later would otherwise revive a subscription no licence
// follows any more.
//
// handled false: the licence is not an upgraded one (or its free plan
// is gone), and the caller's ordinary handling applies. An error means
// the cancel or the revert failed; the caller must have the event (or
// the sweep) try again rather than fall back to the ordinary handling,
// which would leave the licence ended on the paid plan for good.
func (h *StripeHandler) endSubscriptionUpgrade(ctx context.Context, lic *model.License, subscriptionID, reason, source string, cancelFirst bool) (bool, error) {
	u, err := h.appliedUpgradeFor(ctx, lic, subscriptionID)
	if err != nil || u == nil {
		return false, err
	}
	if cancelFirst {
		if err := cancelSubscription(subscriptionID); err != nil {
			return true, err
		}
	}
	res, err := h.revertUpgrade(ctx, u.ID, reason, source)
	if err != nil {
		return true, err
	}
	return !res.FreePlanGone, nil
}

// CancelForAdmin ends a licence's Stripe subscription for the admin
// "cancel and revoke" action. A licence an upgrade moved onto that
// subscription goes back to its free plan instead (reverted true), as a
// full refund in Stripe would have it; any other is left for the caller
// to revoke. The subscription is cancelled first either way, and one
// Stripe has already ended is fine. An error means it may still be
// billing, so the caller must change nothing.
func (h *StripeHandler) CancelForAdmin(ctx context.Context, lic *model.License) (reverted bool, err error) {
	handled, err := h.endSubscriptionUpgrade(ctx, lic, lic.StripeSubscriptionID, upgradeEndAdmin, "admin", true)
	if handled || err != nil {
		return handled, err
	}
	return false, cancelSubscription(lic.StripeSubscriptionID)
}

// EndPastDueUpgrade is the dunning sweep's hook, for a licence past due
// since before threshold (see endLapsedUpgrade).
func (h *StripeHandler) EndPastDueUpgrade(ctx context.Context, lic *model.License, threshold time.Time) (bool, error) {
	return h.endLapsedUpgrade(ctx, lic, upgradeEndPastDue, func(l *model.License) bool {
		since := l.UpdatedAt
		if l.PastDueAt != nil {
			since = *l.PastDueAt // as the sweep reads it: COALESCE(past_due_at, updated_at)
		}
		return l.Status == model.StatusPastDue && since.Before(threshold)
	})
}

// EndGraceExpiredUpgrade is the grace sweep's hook, for a licence whose
// valid_until is before cutoff (see endLapsedUpgrade).
func (h *StripeHandler) EndGraceExpiredUpgrade(ctx context.Context, lic *model.License, cutoff time.Time) (bool, error) {
	return h.endLapsedUpgrade(ctx, lic, upgradeEndGrace, func(l *model.License) bool {
		return (l.Status == model.StatusActive || l.Status == model.StatusPastDue) &&
			l.ValidUntil != nil && l.ValidUntil.Before(cutoff)
	})
}

// endLapsedUpgrade is what the expiry sweeps do with a licence an
// upgrade moved onto a subscription, instead of expiring it: it goes
// back to its free plan, and the subscription is cancelled so a late
// payment does not pay for nothing.
//
// The sweep read the licence earlier, so it is read again here and must
// still qualify, and Stripe is asked before anything is cancelled. A
// subscription Stripe still has as active or trialing means only the
// local copy is stale: it is synced and kept. handled false lets the
// sweep expire the licence as usual (it was not upgraded, or its free
// plan is gone); an error, or a licence that no longer qualifies, is
// reported handled so the sweep leaves it alone this round.
func (h *StripeHandler) endLapsedUpgrade(ctx context.Context, lic *model.License, reason string, qualifies func(*model.License) bool) (bool, error) {
	u, err := h.appliedUpgradeFor(ctx, lic, lic.StripeSubscriptionID)
	if err != nil || u == nil {
		return false, err
	}
	now, err := h.Store.FindLicenseByID(ctx, lic.ID)
	if err != nil {
		return true, err
	}
	if !qualifies(now) || now.StripeSubscriptionID != u.StripeSubscriptionID {
		return true, nil
	}
	if stripe.Key != "" {
		asOf, err := h.Store.StripeReadStamp(ctx)
		if err != nil {
			return true, err
		}
		cur, err := h.currentSubscription(u.StripeSubscriptionID)
		if err != nil {
			return true, err
		}
		if cur != nil && (cur.Status == "active" || cur.Status == "trialing") {
			return true, h.syncFromCurrent(ctx, now, cur, asOf, "expiry_sweep")
		}
	}
	return h.endSubscriptionUpgrade(ctx, now, u.StripeSubscriptionID, reason, "system", true)
}

// chargeInvoice is the invoice a charge paid, once looked up, so the
// purchase path that follows does not ask Stripe a second time.
type chargeInvoice struct {
	looked  bool
	invoice *stripe.Invoice // nil: the charge paid no invoice
}

// refundUpgrade handles a full refund of a charge that paid for an
// upgrade. An applied upgrade is reverted, one not yet applied is closed
// so it never applies; for a subscription the subscription is cancelled
// first. Reports whether the charge belonged to an upgrade at all, and
// the charge's invoice if it had to look it up.
func (h *StripeHandler) refundUpgrade(ctx context.Context, chargeID, paymentIntentID, invoiceID string, chargeMeta map[string]string) (bool, chargeInvoice, error) {
	var u *model.LicenseUpgrade
	var looked chargeInvoice
	subscriptionID := ""
	find := func(fn func() (*model.LicenseUpgrade, error)) error {
		if u != nil {
			return nil
		}
		found, err := fn()
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		u = found
		return nil
	}
	if paymentIntentID != "" {
		if err := find(func() (*model.LicenseUpgrade, error) {
			return h.Store.FindLicenseUpgradeByPaymentIntent(ctx, paymentIntentID)
		}); err != nil {
			return false, looked, err
		}
	}
	if id := chargeMeta[metaUpgradeID]; id != "" && chargeMeta[metaKind] == kindUpgrade {
		if err := find(func() (*model.LicenseUpgrade, error) { return h.Store.FindLicenseUpgradeByID(ctx, id) }); err != nil {
			return false, looked, err
		}
	}
	if u == nil && paymentIntentID != "" {
		// A one-time purchase (not an upgrade: one applied would have
		// been found by its payment intent above) is the purchase path's
		// to handle, from the database alone.
		if _, err := h.Store.FindLicenseByStripePaymentIntent(ctx, paymentIntentID); err == nil {
			return false, looked, nil
		} else if !errors.Is(err, sql.ErrNoRows) {
			return false, looked, err
		}
	}
	if u == nil && stripe.Key != "" && (invoiceID != "" || paymentIntentID != "") {
		inv, err := h.invoiceForCharge(invoiceID, paymentIntentID)
		if err != nil {
			return false, looked, err
		}
		looked = chargeInvoice{looked: true, invoice: inv}
		if inv != nil && inv.Parent != nil && inv.Parent.SubscriptionDetails != nil {
			details := inv.Parent.SubscriptionDetails
			if details.Subscription != nil {
				subscriptionID = details.Subscription.ID
				if err := find(func() (*model.LicenseUpgrade, error) {
					return h.Store.FindLicenseUpgradeBySubscription(ctx, subscriptionID)
				}); err != nil {
					return false, looked, err
				}
			}
			if id := details.Metadata[metaUpgradeID]; id != "" && details.Metadata[metaKind] == kindUpgrade {
				if err := find(func() (*model.LicenseUpgrade, error) { return h.Store.FindLicenseUpgradeByID(ctx, id) }); err != nil {
					return false, looked, err
				}
			}
		}
	}
	if u == nil {
		return false, looked, nil
	}
	if subscriptionID == "" {
		subscriptionID = u.StripeSubscriptionID
	}

	switch u.State {
	case model.UpgradeApplied:
		if err := cancelSubscription(subscriptionID); err != nil {
			return true, looked, err
		}
		res, err := h.revertUpgrade(ctx, u.ID, upgradeEndRefund, "webhook")
		if err != nil {
			return true, looked, err
		}
		if res.FreePlanGone {
			// Nothing to go back to: a refunded purchase, as before.
			return false, looked, nil
		}
	case model.UpgradePending, model.UpgradeExpired:
		if err := cancelSubscription(subscriptionID); err != nil {
			return true, looked, err
		}
		closed := false
		err := h.Store.RunInTx(ctx, func(ctx context.Context, tx bun.Tx) error {
			locked, err := store.LockLicenseUpgradeIn(ctx, tx, u.ID)
			if err != nil {
				return err
			}
			switch locked.State {
			case model.UpgradePending, model.UpgradeExpired:
				closed = true
				return store.EndLicenseUpgradeIn(ctx, tx, locked, model.UpgradeRefunded, upgradeEndRefundEarly)
			case model.UpgradeApplied:
				// Applied between the read and the lock: revert below.
				return nil
			}
			return nil
		})
		if err != nil {
			return true, looked, err
		}
		if !closed {
			res, err := h.revertUpgrade(ctx, u.ID, upgradeEndRefund, "webhook")
			if err != nil {
				return true, looked, err
			}
			if res.FreePlanGone {
				return false, looked, nil
			}
		} else {
			h.Store.Audit(ctx, &model.AuditLog{
				Entity: "license", EntityID: u.LicenseID, Action: "upgrade_refunded",
				ActorType: "webhook",
				Changes:   map[string]any{"upgrade_id": u.ID, "charge_id": chargeID, "provider": "stripe"},
			})
		}
	default:
		// Reverted, refunded or void already: a second refund event. One
		// closed because its free plan was gone is still the purchase
		// path's to revoke, if that step failed the first time.
		if strings.HasSuffix(u.EndReason, ":free_plan_gone") {
			return false, looked, nil
		}
	}
	return true, looked, nil
}

// invoiceForCharge returns the invoice a charge paid, or nil. Charges on
// current API versions carry no invoice field; the payment intent leads
// to it through the invoice's payments, as in licenseForCharge.
func (h *StripeHandler) invoiceForCharge(invoiceID, paymentIntentID string) (*stripe.Invoice, error) {
	if invoiceID != "" {
		inv, err := stripeinvoice.Get(invoiceID, nil)
		if stripeNotFound(err) {
			return nil, nil
		}
		return inv, err
	}
	params := &stripe.InvoicePaymentListParams{
		Payment: &stripe.InvoicePaymentListPaymentParams{
			Type:          stripe.String("payment_intent"),
			PaymentIntent: stripe.String(paymentIntentID),
		},
	}
	params.AddExpand("data.invoice")
	params.Filters.AddFilter("limit", "", "1")
	iter := invoicepayment.List(params)
	if iter.Next() {
		return iter.InvoicePayment().Invoice, nil
	}
	if err := iter.Err(); err != nil && !stripeNotFound(err) {
		return nil, fmt.Errorf("list invoice payments: %w", err)
	}
	return nil, nil
}

// closeFailedUpgrade closes the upgrade of a checkout whose delayed
// payment failed: it will never be paid, and left pending it would keep
// the licence from opening another checkout.
func (h *StripeHandler) closeFailedUpgrade(ctx context.Context, sessionID string) error {
	return h.Store.RunInTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		u, err := store.LockLicenseUpgradeBySessionIn(ctx, tx, sessionID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if u.State != model.UpgradePending && u.State != model.UpgradeExpired {
			return nil
		}
		return store.EndLicenseUpgradeIn(ctx, tx, u, model.UpgradeVoid, "payment_failed")
	})
}
