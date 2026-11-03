package payment

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"html"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stripe/stripe-go/v82"
	portalsession "github.com/stripe/stripe-go/v82/billingportal/session"
	stripecharge "github.com/stripe/stripe-go/v82/charge"
	"github.com/stripe/stripe-go/v82/checkout/session"
	stripecustomer "github.com/stripe/stripe-go/v82/customer"
	stripeinvoice "github.com/stripe/stripe-go/v82/invoice"
	"github.com/stripe/stripe-go/v82/invoicepayment"
	stripeprice "github.com/stripe/stripe-go/v82/price"
	"github.com/stripe/stripe-go/v82/subscription"
	"github.com/stripe/stripe-go/v82/webhook"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/service"
	"github.com/tabloy/keygate/internal/store"
	"github.com/tabloy/keygate/pkg/apperr"
	"github.com/tabloy/keygate/pkg/response"
	"github.com/uptrace/bun"
)

type StripeHandler struct {
	Store         *store.Store
	WebhookSecret string // initial value from config (backward compat)
	BaseURL       string
	Email         *service.EmailService
	WebhookSvc    *service.WebhookService
	// Livemode is the environment this handler is configured for.
	// Every inbound webhook event whose Livemode differs is rejected
	// with 400 — guards against cross-environment delivery (test
	// secret leaking + replay into prod, or vice versa).
	Livemode bool

	// pendingAfter is where the last SyncPendingCheckouts round
	// stopped when it hit its per-round cap; nil starts from the oldest
	// row. pendingBatchLimit overrides the cap (tests); 0 = default.
	pendingAfter      *store.PendingCheckoutSession
	pendingBatchLimit int
	// cancelSyncAfter is where the last SyncCancelStates run stopped;
	// nil starts from the soonest to expire.
	cancelSyncAfter *store.StripeCancelStateTarget

	// ChangeKey signs subscription change previews (see
	// PreviewSubscriptionChange); every replica must hold the same one.
	ChangeKey     []byte
	changeKeyOnce sync.Once
	ownChangeKey  []byte

	mu            sync.RWMutex
	webhookSecret string // runtime-updatable, guarded by mu
	// prevWebhookSecret is accepted alongside webhookSecret until
	// prevSecretUntil, while a replaced endpoint drains its queue.
	prevWebhookSecret string
	prevSecretUntil   time.Time
}

// SetPreviousWebhookSecret keeps an older signing secret valid until
// `until`, so deliveries still queued for a replaced endpoint verify.
func (h *StripeHandler) SetPreviousWebhookSecret(secret string, until time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.prevWebhookSecret, h.prevSecretUntil = secret, until
}

func (h *StripeHandler) previousWebhookSecret() (string, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.prevWebhookSecret == "" || time.Now().After(h.prevSecretUntil) {
		return "", false
	}
	return h.prevWebhookSecret, true
}

// GetWebhookSecret returns the current webhook signing secret (thread-safe).
func (h *StripeHandler) GetWebhookSecret() string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.webhookSecret
}

// SetWebhookSecret updates the webhook signing secret (thread-safe).
func (h *StripeHandler) SetWebhookSecret(secret string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.webhookSecret = secret
}

// CheckoutByPlan handles GET /pay/:checkout_id — looks up plan by checkout_id,
// creates a Stripe Checkout Session, and redirects to Stripe.
func (h *StripeHandler) CheckoutByPlan(c *gin.Context) {
	checkoutID := c.Param("checkout_id")
	if len(checkoutID) != 8 {
		c.String(http.StatusBadRequest, "invalid checkout id")
		return
	}

	plan, err := h.Store.FindPlanByCheckoutID(c, checkoutID)
	if err != nil || plan == nil {
		c.String(http.StatusNotFound, "plan not found")
		return
	}

	if !plan.Active {
		c.String(http.StatusGone, "this plan is no longer available")
		return
	}
	// ?quantity=N starts a plan sold per seat at N seats.
	requested := 0
	if q := c.Query("quantity"); q != "" {
		n, err := strconv.Atoi(q)
		if err != nil || n < 1 {
			c.String(http.StatusBadRequest, "quantity must be a whole number, 1 or more")
			return
		}
		requested = n
	}
	if !h.boundedPlanSellable(c, plan) {
		c.String(http.StatusServiceUnavailable, "this plan is temporarily unavailable")
		return
	}

	if plan.StripePriceID == "" {
		// A free plan the operator opened for self sign-up takes the
		// visitor to its form; the form's API re-checks the plan.
		if plan.SelfSignup {
			c.Redirect(http.StatusFound, h.BaseURL+"/signup/"+checkoutID)
			return
		}
		c.String(http.StatusServiceUnavailable, "payment not configured for this plan")
		return
	}

	// Determine checkout mode from Stripe Price (source of truth, not local config)
	sp, err := stripeprice.Get(plan.StripePriceID, nil)
	if err != nil {
		slog.Error("stripe: failed to fetch price", "price_id", plan.StripePriceID, "error", err)
		c.String(http.StatusServiceUnavailable, "payment configuration error")
		return
	}
	mode := string(stripe.CheckoutSessionModeSubscription)
	if sp.Type == "one_time" {
		mode = string(stripe.CheckoutSessionModePayment)
	}
	item, err := checkoutLineItem(plan, plan.StripePriceID, mode == string(stripe.CheckoutSessionModeSubscription), priceMetered(sp), requested)
	if err != nil {
		c.String(http.StatusBadRequest, err.Error())
		return
	}

	params := &stripe.CheckoutSessionParams{
		Mode:                stripe.String(mode),
		LineItems:           []*stripe.CheckoutSessionLineItemParams{item},
		SuccessURL:          stripe.String(h.BaseURL + "/checkout/success?session_id={CHECKOUT_SESSION_ID}"),
		CancelURL:           stripe.String(h.BaseURL + "/pricing"),
		AllowPromotionCodes: stripe.Bool(true),
	}
	params.Metadata = map[string]string{
		"plan_id":       plan.ID,
		"product_id":    plan.ProductID,
		metaUpdatesDays: strconv.Itoa(plan.UpdatesDays),
		metaLicenseType: plan.LicenseType,
	}

	s, err := session.New(params)
	if err != nil {
		slog.Error("stripe: failed to create checkout session", "plan_id", plan.ID, "price_id", plan.StripePriceID, "error", err)
		c.String(http.StatusInternalServerError, "checkout unavailable")
		return
	}
	c.Redirect(http.StatusTemporaryRedirect, s.URL)
}

// boundedPlanSellable reports whether a plan that hands out a bounded
// update period may be sold right now. The period is only enforceable
// while every replica runs a version that knows about it, so selling
// one while the operator switch is off would give a customer a cutoff
// a rolled-back deployment cannot hold up. Plans without a period are
// always sellable, and sessions already paid for are still fulfilled —
// refusing those would keep the money and give nothing back.
func (h *StripeHandler) boundedPlanSellable(c *gin.Context, plan *model.Plan) bool {
	if plan.InitialUpdatesUntil(time.Now()) == nil {
		return true
	}
	on, err := h.Store.MaintenanceFeaturesEnabled(c)
	if err != nil {
		slog.Error("stripe checkout: maintenance switch unreadable", "plan_id", plan.ID, "error", err)
		return false
	}
	if !on {
		slog.Warn("stripe checkout: plan sells an update period while maintenance features are switched off",
			"plan_id", plan.ID, "updates_days", plan.UpdatesDays)
		return false
	}
	return true
}

// sellableNow is boundedPlanSellable for the JSON endpoints; it writes
// the response itself and reports whether the caller may continue.
func (h *StripeHandler) sellableNow(c *gin.Context, plan *model.Plan) bool {
	if h.boundedPlanSellable(c, plan) {
		return true
	}
	response.Err(c, http.StatusServiceUnavailable, "PLAN_UNAVAILABLE", "this plan is temporarily unavailable")
	return false
}

func (h *StripeHandler) Webhook(c *gin.Context) {
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		response.BadRequest(c, "read failed")
		return
	}

	secret := h.GetWebhookSecret()
	if secret == "" {
		slog.Error("stripe webhook received but no signing secret configured")
		response.BadRequest(c, "webhook not configured")
		return
	}
	// Signature check here; the API version is checked below against
	// the shapes this build parses. stripe-go's own guard would refuse
	// every pre-2025 event, silently disabling billing for accounts
	// whose endpoint predates the release trains — exactly the installs
	// that set STRIPE_WEBHOOK_SECRET by hand.
	opts := webhook.ConstructEventOptions{IgnoreAPIVersionMismatch: true}
	event, err := webhook.ConstructEventWithOptions(body, c.GetHeader("Stripe-Signature"), secret, opts)
	if err != nil {
		// A replaced endpoint keeps delivering (and retrying) with its
		// old secret for a while; accept it during the drain period.
		if prev, ok := h.previousWebhookSecret(); ok {
			event, err = webhook.ConstructEventWithOptions(body, c.GetHeader("Stripe-Signature"), prev, opts)
		}
	}
	if err != nil {
		// A warning, not an error: anyone can post here, and a request
		// without Stripe's signature is not a fault of this install. A
		// wrong STRIPE_WEBHOOK_SECRET shows up as a run of these.
		slog.Warn("stripe webhook verification failed", "error", err.Error())
		response.BadRequest(c, "invalid signature")
		return
	}
	if !supportedAPIVersion(event.APIVersion) {
		// Not recorded as processed: once the endpoint is recreated on
		// a supported version (or this build upgraded) Stripe's retry
		// of the event will be applied.
		slog.Error("stripe webhook: unsupported API version, event not applied",
			"event_id", event.ID, "event_version", event.APIVersion, "sdk_version", stripe.APIVersion)
		response.Err(c, http.StatusBadRequest, "UNSUPPORTED_API_VERSION",
			"webhook endpoint API version "+event.APIVersion+" is not supported by this build; recreate the endpoint on "+stripe.APIVersion)
		return
	}

	// Livemode gate: even with a valid signature, a test-mode event
	// must not be processed by a live server (or vice versa). Stripe
	// allows separate webhook endpoints per mode, so the secret alone
	// doesn't carry environment info — we check the event flag
	// against our configured mode. Without this, a leaked test secret
	// could replay arbitrary forged events at the production endpoint.
	// The work outlives the request: once a write commits, what it tells
	// (an audit line, a webhook) must go out even if Stripe has hung up,
	// as a retry finds the change made and tells nothing.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), webhookWorkTimeout)
	defer cancel()
	if event.Livemode != h.Livemode {
		slog.Error("stripe webhook livemode mismatch",
			"event_id", event.ID, "event_livemode", event.Livemode,
			"server_livemode", h.Livemode)
		response.BadRequest(c, "livemode mismatch")
		return
	}

	// Idempotency. A claim is taken before handling and a done marker
	// written after; a claim without a done marker that is older than
	// staleClaimAge is taken over, so an event whose handling failed
	// (and whose claim could not be released) is not mistaken for a
	// completed one when Stripe retries it.
	claimed, done, err := h.claimEvent(ctx, event.ID)
	if err != nil {
		slog.Error("stripe webhook: failed to claim event", "id", event.ID, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"received": false, "retry": true})
		return
	}
	if !claimed {
		if done {
			c.JSON(http.StatusOK, gin.H{"received": true, "skipped": true})
			return
		}
		// Another delivery of this event is being handled right now
		// (or failed moments ago and could not release its claim). A
		// 2xx would end Stripe's retries; ask it to come back instead.
		c.JSON(http.StatusServiceUnavailable, gin.H{"received": false, "retry": true, "in_progress": true})
		return
	}

	slog.Info("stripe webhook received", "type", event.Type, "id", event.ID)

	// Handlers report transient failures, of Stripe or of the database;
	// the event claim is then released and Stripe asked to retry, so a
	// lookup or write that failed once does not mean a paid customer
	// without a license or a refunded one that stays active.
	var herr error
	switch event.Type {
	case "checkout.session.completed", "checkout.session.async_payment_succeeded":
		herr = h.onCheckoutCompleted(ctx, event.Data.Raw)
	case "checkout.session.async_payment_failed":
		herr = h.onAsyncPaymentFailed(ctx, event.Data.Raw)
	case "invoice.paid":
		herr = h.onInvoicePaid(ctx, event.Data.Raw)
	case "customer.subscription.updated",
		// A change Stripe held for payment was applied, or ran out:
		// either way the subscription is read and applied as it is now.
		"customer.subscription.pending_update_applied",
		"customer.subscription.pending_update_expired",
		// A pause and a resume too: read as Stripe has the subscription
		// now, a pause delivered after the resume cannot suspend a paying
		// customer, and a failed write is retried like any other read.
		// Without an API key the payload decides, and a late one can.
		"customer.subscription.paused",
		"customer.subscription.resumed":
		herr = h.onSubscriptionUpdated(ctx, event.Data.Raw)
	case "customer.subscription.deleted":
		herr = h.onSubscriptionDeleted(ctx, event.Data.Raw)
	case "invoice.payment_failed":
		herr = h.onPaymentFailed(ctx, event.Data.Raw)
	case "charge.refunded":
		herr = h.onChargeRefundedAt(ctx, event.Data.Raw, event.Created)
	case "charge.dispute.created":
		herr = h.onDisputeCreated(ctx, event.Data.Raw)
	case "charge.dispute.closed":
		herr = h.onDisputeClosed(ctx, event.Data.Raw)
	case "invoice.payment_action_required":
		herr = h.onPaymentActionRequired(ctx, event.Data.Raw)
	case "customer.subscription.trial_will_end":
		herr = h.onTrialWillEnd(ctx, event.Data.Raw)
	case "invoice.upcoming":
		herr = h.onInvoiceUpcoming(ctx, event.Data.Raw)
	case "customer.updated":
		herr = h.onCustomerUpdated(ctx, event.Data.Raw)
	default:
		slog.Warn("stripe webhook: unhandled event type", "type", event.Type)
	}
	if herr != nil {
		slog.Warn("stripe webhook: transient failure, asking Stripe to retry", "type", event.Type, "id", event.ID, "error", herr)
		// Best effort: if this fails the claim goes stale and the
		// retry takes it over once it is old enough.
		finish, cancelFinish := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancelFinish()
		if err := h.release(finish, processedEventDoneProvider, processedEventClaimProvider, event.ID); err != nil {
			slog.Error("stripe webhook: failed to release event claim", "id", event.ID, "error", err)
		}
		c.JSON(http.StatusInternalServerError, gin.H{"received": false, "retry": true})
		return
	}
	// Dropping the in-flight marker is what makes a later resend skip.
	// If that fails, do not tell Stripe the event is delivered: its
	// retry finds the marker and comes back until it is gone. On its own
	// context: a handler that ran out of time still releases or completes.
	finish, cancelFinish := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancelFinish()
	if err := h.Store.CompleteProcessedEvent(finish, processedEventClaimProvider, event.ID); err != nil {
		slog.Error("stripe webhook: failed to record completion", "id", event.ID, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"received": false, "retry": true})
		return
	}

	c.JSON(http.StatusOK, gin.H{"received": true})
}

// Stripe events use two rows while a delivery is in flight and one
// once it is done:
//
//   - the reservation, under the provider name every binary so far has
//     used ("stripe"). Older binaries during a rolling upgrade see it
//     as their own idempotency row and skip the event; rows they write
//     read as done here, never as stale claims.
//   - the in-flight marker ("stripe_claim"), removed on completion.
//     While it exists the event is being handled (or its handler died:
//     after staleClaimAge it is taken over).
const (
	processedEventClaimProvider = "stripe_claim"
	processedEventDoneProvider  = "stripe"
)

// claimEvent takes the processing claim for a Stripe event. claimed
// is false when the event is done (done=true) or another handler
// holds a fresh claim (done=false); a stale claim without a done
// marker is taken over.
func (h *StripeHandler) claimEvent(ctx context.Context, eventID string) (claimed, done bool, err error) {
	return h.reserve(ctx, processedEventDoneProvider, processedEventClaimProvider, eventID, nil)
}

// errClaimTaken rolls the reservation back when another caller took
// the in-flight marker between the two writes.
var errClaimTaken = errors.New("in-flight marker taken")

// reserve implements the two-row claim for events and sessions.
// claimed: this caller now handles it. done: nothing to do (finished,
// or an older binary has it). Neither: another caller is mid-way, or
// (with err) the state could not be established. fulfilled, when
// given, is an extra "already done" check — a license row for a
// session — consulted before taking over a stale in-flight marker.
func (h *StripeHandler) reserve(ctx context.Context, doneProvider, claimProvider, id string, fulfilled func() (bool, error)) (claimed, done bool, err error) {
	// Both rows are written in one transaction. Written separately, a
	// crash between them leaves a reservation with no in-flight
	// marker — which is exactly how a finished event looks, so the
	// next delivery would answer 2xx and do nothing. A refund has no
	// sweeper behind it; it would simply be lost.
	reserved := false
	err = h.Store.RunInTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		var err error
		if reserved, err = store.ClaimProcessedEventIn(ctx, tx, doneProvider, id); err != nil || !reserved {
			return err
		}
		// Ours. An in-flight marker without a reservation can only be
		// an orphan of a failed release; replace it.
		if err := store.DeleteProcessedEventIn(ctx, tx, claimProvider, id); err != nil {
			return err
		}
		if claimed, err = store.ClaimProcessedEventIn(ctx, tx, claimProvider, id); err != nil {
			return err
		}
		if !claimed {
			// Another caller took the marker in between: give the
			// reservation back by rolling this transaction back.
			return errClaimTaken
		}
		return nil
	})
	switch {
	case errors.Is(err, errClaimTaken):
		return false, false, nil
	case err != nil:
		return false, false, err
	case claimed:
		return true, false, nil
	}
	inflight, err := h.Store.HasProcessedEvent(ctx, claimProvider, id)
	if err != nil {
		return false, false, err
	}
	if !inflight {
		return false, true, nil // reservation without marker: done
	}
	if fulfilled != nil {
		if ok, err := fulfilled(); err != nil || ok {
			return false, ok, err
		}
	}
	released, err := h.Store.ReleaseStaleProcessedEvent(ctx, claimProvider, id, staleClaimAge)
	if err != nil || !released {
		return false, false, err // fresh: someone is on it
	}
	slog.Warn("stripe: took over a stale in-flight claim", "provider", claimProvider, "id", id)
	claimed, err = h.Store.ClaimProcessedEvent(ctx, claimProvider, id)
	return claimed, false, err
}

// release undoes a reservation after a failed handling so a retry can
// claim it. Both rows go in one transaction, for the same reason
// reserve writes them in one: between two separate deletes a retry
// can take the event over, and this caller's second delete would then
// remove the new handler's in-flight marker. What is left — a
// reservation with no marker — is exactly how a finished event looks,
// so the retry after that one would answer 2xx and do nothing. For a
// refund, which has no sweeper behind it, that is the event lost.
func (h *StripeHandler) release(ctx context.Context, doneProvider, claimProvider, id string) error {
	return h.Store.RunInTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		if err := store.DeleteProcessedEventIn(ctx, tx, doneProvider, id); err != nil {
			return err
		}
		return store.DeleteProcessedEventIn(ctx, tx, claimProvider, id)
	})
}

// onCheckoutCompleted handles checkout.session.completed and
// checkout.session.async_payment_succeeded. Delayed payment methods
// (bank debits, vouchers) complete the session before the money
// arrives; the session is then still unpaid and must not produce a
// license. Nothing is claimed for it, so the later
// async_payment_succeeded event fulfils it through this same path.
func (h *StripeHandler) onCheckoutCompleted(ctx context.Context, raw json.RawMessage) error {
	var data struct {
		ID              string `json:"id"`
		CustomerEmail   string `json:"customer_email"`
		CustomerDetails *struct {
			Email string `json:"email"`
		} `json:"customer_details"`
		Customer      string            `json:"customer"`
		Subscription  string            `json:"subscription"`
		PaymentIntent string            `json:"payment_intent"`
		PaymentStatus string            `json:"payment_status"`
		Mode          string            `json:"mode"`
		Created       int64             `json:"created"`
		Metadata      map[string]string `json:"metadata"`
	}
	if json.Unmarshal(raw, &data) != nil {
		return nil
	}
	if !sessionPaid(data.PaymentStatus) {
		// Remember it: if async_payment_succeeded is lost, the
		// periodic SyncPendingCheckouts still picks the session up.
		// That marker is the whole point of this branch, so failing
		// to write it fails the event.
		if _, err := h.Store.ClaimProcessedEvent(ctx, pendingSessionProvider, data.ID); err != nil {
			return fmt.Errorf("record pending session %s: %w", data.ID, err)
		}
		slog.Info("stripe checkout: session not paid yet, waiting", "session_id", data.ID, "payment_status", data.PaymentStatus)
		return nil
	}
	email := data.CustomerEmail
	if email == "" && data.CustomerDetails != nil {
		email = data.CustomerDetails.Email
	}
	if data.Metadata == nil {
		data.Metadata = map[string]string{}
	}
	data.Metadata["session_id"] = data.ID
	data.Metadata[metaSessionCreated] = strconv.FormatInt(data.Created, 10)
	ok, err := h.fulfillCheckout(ctx, email, data.Customer, data.Subscription, data.PaymentIntent, data.Metadata, "webhook")
	if !ok {
		// Not fulfilled here — a transient failure, another worker
		// mid-way, or a plan/email the operator still has to fix. The
		// pending marker keeps the session in SyncPendingCheckouts for
		// 30 days; on an error Stripe also retries meanwhile.
		if _, perr := h.Store.ClaimProcessedEvent(ctx, pendingSessionProvider, data.ID); perr != nil {
			return fmt.Errorf("record pending session %s: %w", data.ID, perr)
		}
	}
	return err
}

// supportedAPIVersion reports whether this build parses events
// rendered at the given Stripe API version. The handlers read two
// layouts: the pre-2025-03-31 one (dotless date versions and the
// Acacia train) and the Basil one the SDK is pinned to. Any other
// train may carry fields in places the parsers do not look, and must
// not be acknowledged as applied.
func supportedAPIVersion(v string) bool {
	if v == "" {
		return false
	}
	_, after, ok := strings.Cut(v, ".")
	if !ok {
		return true // yyyy-MM-dd: legacy layout
	}
	switch after {
	case "acacia":
		return true // last pre-Basil train, legacy layout
	}
	return sameReleaseTrain(v, stripe.APIVersion)
}

// pendingSessionProvider keys processed_events rows for sessions that
// completed before their delayed payment settled.
const pendingSessionProvider = "stripe_pending_session"

func (h *StripeHandler) onAsyncPaymentFailed(ctx context.Context, raw json.RawMessage) error {
	var data struct {
		ID           string               `json:"id"`
		Subscription *stripe.Subscription `json:"subscription"` // an ID, or the object expanded
		Metadata     map[string]string    `json:"metadata"`
	}
	if json.Unmarshal(raw, &data) != nil || data.ID == "" {
		return nil
	}
	// An upgrade checkout that will never be paid must not keep the
	// licence from opening another one.
	if err := h.closeFailedUpgrade(ctx, data.ID); err != nil {
		return fmt.Errorf("close failed upgrade %s: %w", data.ID, err)
	}
	// A subscription checkout's subscription outlives its failed first
	// payment in Stripe, and would go on billing for a licence that was
	// never issued: a session never succeeds after this event. Cancelled,
	// unless a licence does follow it after all, or it is not one Keygate
	// sells (the account may bill other products, whose metadata may use
	// the same keys): known, as at fulfilment, by a plan the session's
	// metadata names or the subscription's price.
	if stripe.Key != "" && data.Subscription != nil && data.Subscription.ID != "" {
		_, err := h.Store.FindLicenseByStripeSubscription(ctx, data.Subscription.ID)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			ours := false
			if id := data.Metadata[metaPlanID]; id != "" {
				switch _, err := h.Store.FindPlanByID(ctx, id); {
				case err == nil:
					ours = true
				case !errors.Is(err, sql.ErrNoRows):
					return fmt.Errorf("find plan %s: %w", id, err)
				}
			}
			if !ours {
				plan, _, _, err := h.resolvePlan(ctx, data.Subscription.ID)
				if err != nil {
					return fmt.Errorf("unpaid checkout %s: %w", data.ID, err)
				}
				ours = plan != nil
			}
			if !ours {
				break
			}
			if err := cancelSubscription(data.Subscription.ID); err != nil {
				return fmt.Errorf("unpaid checkout %s: %w", data.ID, err)
			}
		case err != nil:
			return err
		}
	}
	_ = h.Store.DeleteProcessedEvent(ctx, pendingSessionProvider, data.ID)
	slog.Info("stripe checkout: delayed payment failed, session dropped", "session_id", data.ID)
	return nil
}

func sessionPaid(status string) bool {
	return status == "paid" || status == "no_payment_required"
}

// sessionEmail prefers the email Stripe attached to the session and
// falls back to the address typed during Checkout — guest checkouts
// on Payment Links carry only the latter.
func sessionEmail(sess *stripe.CheckoutSession) string {
	if sess.CustomerEmail != "" {
		return sess.CustomerEmail
	}
	if sess.CustomerDetails != nil {
		return sess.CustomerDetails.Email
	}
	return ""
}

// fulfillCheckout creates a license for a completed checkout session
// and reports whether the session is fulfilled (now or earlier).
// Idempotent per session: the session ID is claimed atomically right
// before the license is written, so the webhook, success-page
// verification and periodic sync can all see the same session and
// only one license comes out. A customer who completes a second
// checkout for the same product gets a second license — one paid
// session, one license.
//
// The bool says whether the session is fulfilled (now or earlier). A
// non-nil error is a transient failure (Stripe or database) — the
// caller should retry later; permanent conditions (unknown plan, no
// email) return false with no error.
func (h *StripeHandler) fulfillCheckout(ctx context.Context, email, customerID, subscriptionID, paymentIntentID string, metadata map[string]string, source string) (bool, error) {
	sessionID := ""
	if metadata != nil {
		sessionID = metadata["session_id"]
	}
	if sessionID != "" {
		done, err := h.sessionFulfilled(ctx, sessionID)
		if err != nil {
			return false, fmt.Errorf("check session: %w", err)
		}
		if done {
			return true, nil
		}
	}
	if metadata != nil && metadata[metaKind] == kindRenewal {
		return h.fulfillRenewal(ctx, metadata, sessionID, paymentIntentID, source)
	}
	// An upgrade changes the licence it names; falling through would
	// issue a second licence for it.
	if metadata != nil && metadata[metaKind] == kindUpgrade {
		return h.fulfillUpgrade(ctx, customerID, subscriptionID, paymentIntentID, metadata, sessionID, source)
	}
	var plan *model.Plan
	var sub *stripe.Subscription
	var err error
	// The seats the subscription bills; a one-time purchase is one.
	quantity := 1

	if subscriptionID != "" {
		if plan, quantity, sub, err = h.resolvePlan(ctx, subscriptionID); err != nil {
			return false, fmt.Errorf("resolve plan from subscription: %w", err)
		}
	}
	if plan == nil && metadata != nil && metadata["plan_id"] != "" {
		// FindPlanByID hands back a non-nil empty plan together with
		// the error; a deleted plan must read as "no plan", a database
		// failure as transient — never as an empty plan to insert.
		switch p, err := h.Store.FindPlanByID(ctx, metadata["plan_id"]); {
		case err == nil:
			plan = p
		case !errors.Is(err, sql.ErrNoRows):
			return false, fmt.Errorf("find plan %s: %w", metadata["plan_id"], err)
		}
	}
	// Sessions created outside Keygate (Stripe Payment Links, the
	// merchant's own integration) carry no plan_id metadata, and a
	// one-time payment has no subscription to look up. The line items
	// still say which price was bought — resolve the plan from that.
	if plan == nil && sessionID != "" {
		if plan, err = h.resolvePlanFromLineItems(ctx, sessionID); err != nil {
			return false, fmt.Errorf("resolve plan from line items: %w", err)
		}
	}
	if plan == nil {
		slog.Warn("stripe checkout: could not resolve plan", "subscription_id", subscriptionID, "metadata", metadata, "source", source)
		return false, nil
	}

	// Resolve email from Stripe Customer (authoritative source)
	if customerID != "" {
		cust, err := stripecustomer.Get(customerID, nil)
		switch {
		case err == nil && cust.Email != "":
			email = cust.Email
		case err != nil && !stripeNotFound(err):
			return false, fmt.Errorf("fetch customer: %w", err)
		}
	}

	if email == "" {
		slog.Warn("stripe checkout: no customer email, skipping", "customer_id", customerID, "source", source)
		return false, nil
	}

	// The key is made before the claim, like the lookups above: a
	// failure here leaves the session unclaimed for the next attempt.
	key, err := h.Store.NewLicenseKey(ctx, plan.ProductID)
	if err != nil {
		return false, fmt.Errorf("license key: %w", err)
	}

	// Claim the session only now, after every Stripe lookup succeeded:
	// a transient API error above must leave the session unclaimed so
	// the success page or the periodic sync can pick it up. The insert
	// is atomic, so concurrent callers still produce a single license.
	if sessionID != "" {
		claimed, err := h.claimSession(ctx, sessionID)
		if err != nil {
			return false, fmt.Errorf("claim session: %w", err)
		}
		if !claimed {
			// Fulfilled meanwhile, or claimed by a caller still
			// running. Only the former is a completed fulfilment.
			done, err := h.sessionFulfilled(ctx, sessionID)
			if err != nil {
				return false, fmt.Errorf("check session: %w", err)
			}
			return done, nil
		}
	}

	status := model.StatusActive
	if plan.LicenseType == "trial" {
		status = model.StatusTrialing
	}
	lic := &model.License{
		ProductID:        plan.ProductID,
		PlanID:           plan.ID,
		Email:            email,
		LicenseKey:       key,
		PaymentProvider:  "stripe",
		StripeCustomerID: customerID,
		Status:           status,
		Quantity:         quantity,
	}

	if plan.LicenseType == "trial" && plan.TrialDays > 0 {
		until := time.Now().Add(time.Duration(plan.TrialDays) * 24 * time.Hour)
		lic.ValidUntil = &until
	}
	// A subscription's licence is what the subscription is: trialing or
	// paid, up to the end of its period. Read again once the licence
	// exists (below); this read stands should that one fail.
	subscriptionStart(lic, sub)
	// What the customer bought was a licence of the kind the plan was
	// then. A plan retyped since — perpetual to subscription or trial
	// — cannot deliver it: a one-off payment would become a
	// subscription with no end, or a trial. Nothing here can repair
	// that, so the sale waits for the merchant to put the plan back
	// or refund it.
	// A Stripe Payment Link carries no metadata, so there is no frozen
	// type to compare. The plan's history says what it was selling
	// when the checkout was stamped; failing that — a plan whose
	// history does not reach back — the session's own shape still
	// tells a one-off purchase from a recurring one, which is the
	// difference that matters most.
	soldType := strings.TrimSpace(metadata[metaLicenseType])
	// Present but blank says nothing: that is a session with no
	// frozen type, not a type that fails to match the plan.
	frozen := soldType != ""
	if !frozen {
		if created, ok := sessionCreatedAt(metadata); ok {
			_, sold, known, err := h.Store.PlanTermsAt(ctx, plan.ID, created)
			if err != nil {
				return false, fmt.Errorf("read plan terms for %s: %w", plan.ID, err)
			}
			if known && sold != "" {
				soldType, frozen = sold, true
			}
		}
	}
	if !frozen {
		recurringSold := subscriptionID != ""
		recurringNow := plan.LicenseType == "subscription" || plan.LicenseType == "trial"
		if recurringSold != recurringNow {
			soldType, frozen = "subscription", true
			if !recurringSold {
				soldType = "perpetual"
			}
		}
	}
	if sold := soldType; frozen && sold != plan.LicenseType {
		slog.Error("stripe checkout: paid session left pending, the plan is no longer the kind that was bought",
			"session_id", sessionID, "plan_id", plan.ID, "email", email,
			"sold_as", sold, "plan_is_now", plan.LicenseType,
			"hint", "put the plan's license_type back, move the customer to a plan of the kind they bought, or refund the session")
		if sessionID != "" {
			if derr := h.release(ctx, fulfilledSessionProvider, sessionClaimProvider, sessionID); derr != nil {
				slog.Error("stripe checkout: failed to release session claim", "session_id", sessionID, "error", derr)
			}
		}
		return false, fmt.Errorf("%w: sold as %s, plan is now %s", store.ErrPlanChanged, sold, plan.LicenseType)
	}

	// The update period is the one that was on offer at checkout.
	// Sessions Keygate created carry it; sessions created elsewhere
	// (Stripe Payment Links) carry no terms, so those fall back to
	// the plan as it reads now.
	updatesDays := plan.UpdatesDays
	switch v, frozen := metadata[metaUpdatesDays]; {
	case frozen:
		if d, err := strconv.Atoi(v); err == nil {
			updatesDays = d
		} else {
			slog.Warn("stripe checkout: unreadable updates_days in session metadata, using the plan",
				"session_id", sessionID, "value", v, "plan_id", plan.ID)
		}
	default:
		// A session Keygate did not create — a Stripe Payment Link —
		// carries no terms. The plan's history says which period was
		// on offer when the buyer opened that checkout, whatever the
		// plan has been edited to since.
		if created, ok := sessionCreatedAt(metadata); ok {
			sold, _, known, err := h.Store.PlanTermsAt(ctx, plan.ID, created)
			if err != nil {
				return false, fmt.Errorf("read plan terms for %s: %w", plan.ID, err)
			}
			if known && sold != updatesDays {
				slog.Info("stripe checkout: unmanaged session predates a change to the plan's period; granting the period it was sold at",
					"session_id", sessionID, "plan_id", plan.ID, "granted_days", sold, "current_days", plan.UpdatesDays)
				updatesDays = sold
			}
		}
	}
	lic.UpdatesUntil, lic.UpdatesTermsSet = model.UpdatesUntilFor(plan.LicenseType, updatesDays, time.Now()), true
	// The switch is what stops a period being sold: Keygate's own
	// checkout is refused while it is off. It does not reach the
	// sessions already open in Stripe, and a Payment Link the
	// merchant made answers to nobody at all — a customer can open
	// and pay one at any time. Issuing the period would hand out a cutoff the
	// replicas still serving public feeds cannot hold up, and issuing
	// the licence without it would grant updates for life — more than
	// was sold. The sale stays pending, as it does for a product whose
	// feeds went public: gate it again, switch the features back on,
	// or refund.
	if lic.UpdatesUntil != nil {
		on, serr := h.Store.MaintenanceFeaturesEnabled(ctx)
		if serr != nil {
			return false, fmt.Errorf("read the maintenance switch: %w", serr)
		}
		if !on {
			slog.Error("stripe checkout: paid session left pending, the maintenance features are switched off",
				"session_id", sessionID, "plan_id", plan.ID, "email", email,
				"hint", "switch maintenance_features_enabled back on once every replica understands update periods, or refund the session")
			if sessionID != "" {
				if derr := h.release(ctx, fulfilledSessionProvider, sessionClaimProvider, sessionID); derr != nil {
					slog.Error("stripe checkout: failed to release session claim", "session_id", sessionID, "error", derr)
				}
			}
			return false, store.ErrUpdatePeriodNotEnforceable
		}
	}

	if subscriptionID != "" {
		lic.StripeSubscriptionID = subscriptionID
	}
	lic.StripePaymentIntentID = paymentIntentID
	lic.StripeCheckoutSessionID = sessionID

	// Ensure user record exists so they appear in Customers
	_ = h.Store.UpsertUser(ctx, &model.User{Email: email})

	err = h.Store.CreateLicenseWithSubscription(ctx, lic, plan)
	if errors.Is(err, store.ErrPlanChanged) {
		// The plan was retyped between resolving it and this write.
		// Everything about the license follows from that type, so the
		// claim goes back and the next retry builds from the plan as
		// it reads then.
		slog.Warn("stripe checkout: plan changed type mid-fulfilment, retrying",
			"session_id", sessionID, "plan_id", plan.ID, "email", email, "reason", err)
	}
	if errors.Is(err, store.ErrUpdatePeriodNotEnforceable) {
		// The buyer paid for a period this product cannot enforce
		// right now: its update feeds were made public again after
		// the checkout was opened, or gated again so recently that
		// the links the public feed handed out still work. Issuing
		// the license without the period would hand out updates for
		// life — more than was sold, and nothing takes it back;
		// issuing it with the period would sell a cutoff the buyer
		// can walk around through those links. The sale stays pending
		// instead: the claim is released so a webhook retry, the
		// success page or a sync fulfils it once the feeds are gated
		// and the old links have expired, and the merchant can refund
		// it if that is the answer they want.
		slog.Error("stripe checkout: paid session left pending, the product cannot enforce the update period it includes",
			"session_id", sessionID, "plan_id", plan.ID, "product_id", plan.ProductID, "email", email, "reason", err,
			"hint", "switch feed_license_required back on for the product, wait out the feed drain it names, or refund the session")
		if sessionID != "" {
			if derr := h.release(ctx, fulfilledSessionProvider, sessionClaimProvider, sessionID); derr != nil {
				slog.Error("stripe checkout: failed to release session claim", "session_id", sessionID, "error", derr)
			}
		}
		return false, fmt.Errorf("create license: %w", err)
	}
	if err != nil {
		if store.IsCheckoutSessionConflict(err) {
			// Another worker fulfilled this session while we held (or
			// had lost) the claim — its license stands, ours is refused
			// by the unique index. Nothing to release.
			slog.Warn("stripe checkout: session fulfilled concurrently", "session_id", sessionID)
			return true, nil
		}
		slog.Error("stripe checkout: failed to create license", "email", email, "error", err)
		// Release the claim: the customer has paid, and a later
		// webhook retry, success-page visit or sync must be able to
		// try again instead of short-circuiting on the marker.
		if sessionID != "" {
			if derr := h.release(ctx, fulfilledSessionProvider, sessionClaimProvider, sessionID); derr != nil {
				slog.Error("stripe checkout: failed to release session claim", "session_id", sessionID, "error", derr)
			}
		}
		return false, fmt.Errorf("create license: %w", err)
	}
	if sessionID != "" {
		// The reservation stays as the durable done marker; the
		// in-flight marker goes. Failing here is not fatal: the license
		// (with its session id) already answers every retry.
		if err := h.Store.CompleteProcessedEvent(ctx, sessionClaimProvider, sessionID); err != nil {
			slog.Error("stripe checkout: failed to record session completion", "session_id", sessionID, "error", err)
		}
		// The session is no longer pending; a marker left by an
		// earlier unpaid completion or a racing worker goes away.
		_ = h.Store.DeleteProcessedEvent(ctx, pendingSessionProvider, sessionID)
	}
	// Link license to user
	if u, err := h.Store.FindUserByEmail(ctx, email); err == nil {
		lic.UserID = u.ID
		_ = h.Store.UpdateLicenseUser(ctx, lic.ID, u.ID)
	}

	// One product read for both the name and the download page.
	productName, downloadURL := "Your Software", ""
	if p, err := h.Store.FindProductByID(ctx, plan.ProductID); err == nil {
		if p.Name != "" {
			productName = p.Name
		}
		downloadURL = p.DownloadURL
	} else {
		slog.Warn("product name not found, using fallback", "product_id", plan.ProductID)
	}
	if email != "" {
		// Use DecryptLicenseKey for forward compatibility — Phase C will
		// drop the plaintext column and direct .LicenseKey reads will be empty.
		displayKey := h.Store.DecryptLicenseKey(lic)
		subject := "Your license for " + productName
		var body string
		if h.Email != nil {
			// The same template an admin edits under Settings — a Stripe
			// purchase used to get a hard-coded copy that ignored it (and
			// the product's download link).
			subject, body = h.Email.RenderLicenseCreated(productName, plan.Name, displayKey, downloadURL)
		} else {
			body = fmt.Sprintf(`<!DOCTYPE html>
<html><body style="font-family: -apple-system, sans-serif; max-width: 600px; margin: 0 auto; padding: 20px;">
<h2 style="color: #111;">Your %s License</h2>
<p>Your <strong>%s</strong> license is ready.</p>
<div style="background: #f4f4f5; border-radius: 8px; padding: 16px; margin: 16px 0; font-family: monospace; font-size: 18px; text-align: center; letter-spacing: 2px;">%s</div>
<p style="color: #666; font-size: 14px;">Keep this key safe. You'll need it to activate your software.</p>
</body></html>`, html.EscapeString(productName), html.EscapeString(plan.Name), html.EscapeString(displayKey))
		}
		_ = h.Store.EnqueueEmail(ctx, email, subject, body)
	}

	h.Store.Audit(ctx, &model.AuditLog{
		Entity: "license", EntityID: lic.ID, Action: "created",
		ActorType: source,
		Changes:   map[string]any{"provider": "stripe", "email": email, "plan": plan.Name},
	})

	if h.WebhookSvc != nil {
		h.WebhookSvc.Dispatch(ctx, lic.ProductID, "license.created", map[string]any{
			"license_id": lic.ID, "email": lic.Email, "plan_id": lic.PlanID,
		})
	}

	// The subscription is read again now that the licence exists. The
	// read above came before it, and so may every event so far: the
	// invoice that pays the first period, or a cancellation, finds no
	// licence and is dropped, and nothing else would bring the licence
	// in line until the next renewal. Events from here on find it. Read
	// once the licence is told as created, so an end or a pause it finds
	// is told after that, not before. A read that fails leaves the cancel
	// state to SyncCancelStates, and the rest as the read above set it.
	h.followNewSubscription(ctx, lic)

	slog.Info("license created", "email", email, "plan", plan.Name, "source", source)
	return true, nil
}

// VerifyCheckoutSession handles GET /api/v1/checkout/verify?session_id=xxx
// Called by the success page to verify payment and create license (webhook fallback).
func (h *StripeHandler) VerifyCheckoutSession(c *gin.Context) {
	sessionID := c.Query("session_id")
	if sessionID == "" {
		response.BadRequest(c, "session_id is required")
		return
	}

	sess, err := session.Get(sessionID, &stripe.CheckoutSessionParams{})
	if err != nil {
		response.BadRequest(c, "invalid session")
		return
	}

	if !sessionPaid(string(sess.PaymentStatus)) {
		response.OK(c, gin.H{"status": "pending"})
		return
	}

	// Once begun, the licence and what tells of it (its email and the
	// license.created webhook) are seen through even if the buyer closes
	// the page: nothing else retries what a cancelled request left out.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), webhookWorkTimeout)
	defer cancel()
	ok, err := h.fulfillSession(ctx, sess, "verify")
	if !ok {
		h.Store.TryRecordProcessedEvent(ctx, pendingSessionProvider, sess.ID)
	}
	if err != nil {
		slog.Warn("stripe verify: transient fulfilment failure", "session_id", sess.ID, "error", err)
		response.Err(c, http.StatusServiceUnavailable, "FULFILLMENT_RETRY", "payment confirmed; license delivery is being retried")
		return
	}
	kind := ""
	if k := sess.Metadata[metaKind]; k == kindRenewal || k == kindUpgrade {
		kind = k
	}
	if !ok {
		// Paid, but no license yet (another worker is on it, or the
		// plan mapping needs attention); the sync keeps trying.
		response.OK(c, gin.H{"status": "pending", "email": sessionEmail(sess), "kind": kind})
		return
	}

	status := "ok"
	if kind == kindUpgrade {
		// Done can also mean the upgrade closed without applying: the
		// payment was refunded, or the subscription had ended, first.
		if u, err := h.Store.FindLicenseUpgradeBySession(c, sess.ID); err == nil &&
			(u.State == model.UpgradeRefunded || u.State == model.UpgradeVoid) {
			status = "not_applied"
		}
	}
	response.OK(c, gin.H{"status": status, "email": sessionEmail(sess), "kind": kind})
}

// sessionFulfilled reports whether a license already exists for the
// checkout session. A claim row alone is not proof: its owner may
// still be running, or may have failed and released it.
func (h *StripeHandler) sessionFulfilled(ctx context.Context, sessionID string) (bool, error) {
	_, err := h.Store.FindLicenseByStripeCheckoutSession(ctx, sessionID)
	if err == nil {
		return true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	// A renewal session's record is its ledger row, not a license. It
	// is the durable proof; a claim marker left behind by a failed
	// cleanup after the commit must not read as still in flight.
	if _, err := h.Store.FindLicenseRenewalBySession(ctx, sessionID); err == nil {
		return true, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	// So is an upgrade's, once it applied or closed: a reverted upgrade
	// leaves the licence without the session id.
	if done, err := h.upgradeSessionDone(ctx, sessionID); err != nil || done {
		return done, err
	}
	reserved, err := h.Store.HasProcessedEvent(ctx, fulfilledSessionProvider, sessionID)
	if err != nil || !reserved {
		return false, err
	}
	inflight, err := h.Store.HasProcessedEvent(ctx, sessionClaimProvider, sessionID)
	return !inflight, err
}

// Checkout sessions follow the same two-row scheme as events: the
// reservation under the name every binary so far has used
// ("stripe_fulfill"), plus an in-flight marker until the license is
// committed. Sessions older binaries fulfilled — before the upgrade or
// while overlapping it — carry only the reservation and read as done;
// their licenses have no session id, so that row is their record.
const (
	sessionClaimProvider     = "stripe_fulfill_claim"
	fulfilledSessionProvider = "stripe_fulfill"
)

// SubscriptionEnded reports whether Stripe is finished with a
// subscription: cancelled, never completed, or no longer there at
// all. The admin API asks before letting a licence off a Stripe
// subscription, because no local state answers it — "canceled" is
// also what an unpaid subscription reads as, and paying its invoice
// brings it back.
func (h *StripeHandler) SubscriptionEnded(ctx context.Context, subscriptionID string) (bool, error) {
	sub, err := subscription.Get(subscriptionID, nil)
	if err != nil {
		// A 404 says this API key cannot see the subscription, not
		// that the subscription is over: a key rotated to another
		// account, or pointed at the other side of test/live, answers
		// exactly the same way about one that is still charging a
		// card. Only Stripe saying "ended" is evidence of an ending.
		if stripeNotFound(err) {
			return false, apperr.New(409, "STRIPE_NOT_VISIBLE",
				"Stripe cannot see this subscription with the API key this install uses — check that the key belongs to the right account and to the same test/live mode; until then there is no way to tell a deleted subscription from one that is still billing")
		}
		return false, err
	}
	switch sub.Status {
	case stripe.SubscriptionStatusCanceled, stripe.SubscriptionStatusIncompleteExpired:
		return true, nil
	}
	return false, nil
}

// FollowSubscription is for an operator lifting a suspension from a
// licence on a Stripe subscription. While the licence was suspended, the
// subscription's writes were refused (see store.ErrLicenseRestricted):
// a renewal paid, a pause, a payment that failed. Lifting the
// suspension hands the licence back to the subscription as Stripe has it
// now.
//
// It reads the subscription before the suspension is lifted, so a Stripe
// that cannot answer leaves the licence suspended, and returns what
// applies it once lifted: a fresh read, taken then so that a change
// whose event was refused in between is not missed, or this one should
// that read fail.
func (h *StripeHandler) FollowSubscription(ctx context.Context, lic *model.License) (func(context.Context) error, error) {
	first, firstAsOf, err := h.readForFollow(ctx, lic.StripeSubscriptionID)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context) error {
		cur, asOf, err := h.readForFollow(ctx, lic.StripeSubscriptionID)
		if err != nil {
			slog.Warn("stripe: subscription not read again after the suspension was lifted, applying the earlier read",
				"license_id", lic.ID, "subscription_id", lic.StripeSubscriptionID, "error", err)
			cur, asOf = first, firstAsOf
		}
		fresh, err := h.Store.FindLicenseByID(ctx, lic.ID)
		if err != nil {
			return err
		}
		// A downgrade Stripe has not made yet holds the plan and seats
		// back; the status was written all the same.
		if err := h.syncFromCurrent(ctx, fresh, cur, asOf, "admin.reinstate"); err != nil && !errors.Is(err, store.ErrTermsHeld) {
			return err
		}
		// Past due as it comes back: the failed payment was not reported
		// while the licence was suspended, its event refused. It is now,
		// once per episode, as invoice.payment_failed reports it.
		now, err := h.Store.FindLicenseByID(ctx, lic.ID)
		if err != nil {
			return err
		}
		if now.Status == model.StatusPastDue && now.PastDueAt != nil {
			return h.notifyPaymentFailed(ctx, now, lic.StripeSubscriptionID)
		}
		return nil
	}, nil
}

// readForFollow reads a subscription for FollowSubscription, after the
// read stamp it is applied with.
func (h *StripeHandler) readForFollow(ctx context.Context, subscriptionID string) (*subscriptionEvent, time.Time, error) {
	asOf, err := h.Store.StripeReadStamp(ctx)
	if err != nil {
		return nil, time.Time{}, err
	}
	cur, err := h.currentSubscriptionCtx(ctx, subscriptionID)
	if err != nil {
		return nil, time.Time{}, err
	}
	if cur == nil {
		// As for SubscriptionEnded: a key that cannot see the
		// subscription cannot tell one still billing from one that
		// ended, nor a paid renewal from a pause.
		return nil, time.Time{}, apperr.New(409, "STRIPE_NOT_VISIBLE",
			"Stripe cannot see this license's subscription with the API key this install uses — check that the key belongs to the right account and to the same test/live mode; the license stays suspended until its subscription can be read")
	}
	return cur, asOf, nil
}

// currentSubscription fetches a subscription as Stripe has it now, in the
// shape of a subscription event. nil, nil when the event payload has to
// stand in for the licence's status and period: no API key configured,
// or Stripe answers 404 — which says this key cannot see the
// subscription, not that it is gone (see SubscriptionEnded), and the
// payload was signed by Stripe either way. The cancel state is not taken
// from it (see onSubscriptionUpdated). Any other failure is returned so
// the webhook is retried.
func (h *StripeHandler) currentSubscription(id string) (*subscriptionEvent, error) {
	return h.currentSubscriptionCtx(context.Background(), id)
}

// currentSubscriptionCtx is currentSubscription bounded by ctx.
func (h *StripeHandler) currentSubscriptionCtx(ctx context.Context, id string) (*subscriptionEvent, error) {
	if stripe.Key == "" {
		return nil, nil
	}
	sub, err := subscription.Get(id, &stripe.SubscriptionParams{Params: stripe.Params{Context: ctx}})
	if stripeNotFound(err) {
		slog.Warn("stripe: subscription not visible to this API key, using the event payload", "subscription_id", id)
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("fetch subscription %s: %w", id, err)
	}
	var cur subscriptionEvent
	if sub.LastResponse == nil || json.Unmarshal(sub.LastResponse.RawJSON, &cur) != nil || cur.ID != id {
		return nil, fmt.Errorf("fetch subscription %s: unreadable response", id)
	}
	return &cur, nil
}

// heldTermsSyncBatch caps the Stripe lookups SyncLapsedTermHolds makes
// per run.
const heldTermsSyncBatch = 50

// A lapsed hold whose check could not finish is checked again after
// heldTermsRetry; after heldTermsUnseenRetry when this API key cannot see
// the subscription (another account, or the other test/live mode), which
// a corrected key may still settle.
const (
	heldTermsRetry       = 10 * time.Minute
	heldTermsUnseenRetry = time.Hour
)

// SyncLapsedTermHolds settles downgrades whose outcome was never known:
// once a licence's hold runs out (see store.LowerLicenseTerms), its
// subscription is read and applied, so a licence lowered for a change
// Stripe never made gets the terms it pays for back without waiting for
// an event that may never come. A read applied settles the hold, whatever
// it wrote (a subscription that has ended holds nothing). A check that
// cannot finish keeps the record, and is tried again later, behind the
// others.
func (h *StripeHandler) SyncLapsedTermHolds(ctx context.Context) {
	holds, err := h.Store.FindLapsedTermHolds(ctx, heldTermsSyncBatch)
	if err != nil {
		slog.Warn("stripe held-terms sync: listing failed", "error", err)
		return
	}
	for _, hold := range holds {
		h.settleLapsedHold(ctx, hold)
	}
}

// settleLapsedHold checks one lapsed hold against Stripe, under the
// licence's change lock, so no change the customer starts meanwhile is
// caught up in it; one under way leaves it for the next run.
func (h *StripeHandler) settleLapsedHold(ctx context.Context, hold store.LapsedTermHold) {
	id := hold.LicenseID
	postpone := func(after time.Duration, why string, err error) {
		slog.Warn("stripe held-terms sync: "+why+"; checked again later", "license_id", id, "error", err)
		if err := h.Store.PostponeTermsCheck(ctx, hold, after); err != nil {
			slog.Warn("stripe held-terms sync: next check not scheduled", "license_id", id, "error", err)
		}
	}
	settle := func() {
		if err := h.Store.SettleLapsedHold(ctx, hold); err != nil {
			slog.Warn("stripe held-terms sync: hold not ended", "license_id", id, "error", err)
		}
	}
	lock, err := h.Store.LockLicenseChange(ctx, id, 0)
	if err != nil {
		return // a change is under way, or the lock could not be had: next run
	}
	defer lock.Unlock()
	lic, err := h.Store.FindLicenseByID(ctx, id)
	if err != nil {
		postpone(heldTermsRetry, "licence unreadable", err)
		return
	}
	if lic.StripeSubscriptionID == "" {
		settle() // unlinked: nothing left to hold for
		return
	}
	asOf, err := h.Store.StripeReadStamp(ctx)
	if err != nil {
		postpone(heldTermsRetry, "clock unreadable", err)
		return
	}
	// Bounded: the licence's changes wait on its lock meanwhile.
	readCtx, cancel := context.WithTimeout(ctx, changeStripeTimeout)
	defer cancel()
	cur, err := h.currentSubscriptionCtx(readCtx, lic.StripeSubscriptionID)
	if err != nil {
		postpone(heldTermsRetry, "lookup failed", err)
		return
	}
	if cur == nil {
		postpone(heldTermsUnseenRetry, "subscription not visible to this API key", nil)
		return
	}
	if err := h.syncFromCurrent(ctx, lic, cur, asOf, "held_terms_sync"); err != nil {
		postpone(heldTermsRetry, "licence not updated", err)
		return
	}
	settle()
}

// cancelStateSyncBatch caps the Stripe lookups SyncCancelStates makes per
// run: well inside Stripe's rate limits, and the soonest-expiring
// subscriptions go first.
const cancelStateSyncBatch = 100

// SyncCancelStates records, for Stripe subscriptions whose cancel state
// was never recorded, whether each renews or ends at the period end:
// subscriptions from before the state was kept, and every new one, whose
// state is read here once its licence exists (see fulfillCheckout).
// Until a licence is synced, the reminders send it neither "renews
// tomorrow" nor "expiring" (see store.cancelStateSQL).
func (h *StripeHandler) SyncCancelStates(ctx context.Context) {
	targets, err := h.Store.FindStripeCancelStatesToSync(ctx, h.cancelSyncAfter, cancelStateSyncBatch)
	if err != nil {
		slog.Warn("stripe cancel-state sync: listing failed", "error", err)
		return
	}
	// Continue after this batch next run, and start over once the walk
	// reaches the end: a subscription that stays unknown is asked about
	// once per walk instead of blocking the ones behind it. A failure
	// stops the run where it is, so the next run asks again from there.
	for i := range targets {
		if !h.syncCancelState(ctx, targets[i]) {
			return
		}
		h.cancelSyncAfter = &targets[i]
	}
	if len(targets) < cancelStateSyncBatch {
		h.cancelSyncAfter = nil
	}
}

// syncCancelState asks Stripe about one subscription and records whether
// it renews or ends at the period end. It reports whether the walk may
// move past it: true once recorded, or on a 404, which stays unknown —
// it says this API key cannot see the subscription (another account,
// the other test/live mode), not that it ended, and guessing "ends"
// would mail a renewing customer that their licence is expiring. Any
// other failure (rate limit, outage, database) reports false: the caller
// stops and the next run asks about this one again.
func (h *StripeHandler) syncCancelState(ctx context.Context, tg store.StripeCancelStateTarget) bool {
	asOf, err := h.Store.StripeReadStamp(ctx)
	if err != nil {
		slog.Warn("stripe cancel-state sync: clock read failed", "error", err)
		return false
	}
	sub, err := subscription.Get(tg.SubscriptionID, nil)
	if err != nil {
		slog.Warn("stripe cancel-state sync: lookup failed", "license_id", tg.LicenseID, "subscription_id", tg.SubscriptionID, "error", err)
		return stripeNotFound(err)
	}
	// Only while still unknown: a webhook may have recorded a newer state
	// since the lookup above.
	if err := h.Store.RecordSubscriptionCancelStateIfUnknown(ctx, tg.LicenseID, subscriptionEndsThisPeriod(sub), asOf); err != nil {
		slog.Warn("stripe cancel-state sync: record failed", "license_id", tg.LicenseID, "error", err)
		return false
	}
	return true
}

// subscriptionEndsThisPeriod applies subscriptionEvent.EndsThisPeriod to
// a subscription fetched from the API.
func subscriptionEndsThisPeriod(sub *stripe.Subscription) bool {
	ev := subscriptionEvent{Status: string(sub.Status), CancelAtPeriodEnd: sub.CancelAtPeriodEnd, CancelAt: sub.CancelAt}
	if sub.Items != nil {
		for _, it := range sub.Items.Data {
			if it == nil {
				continue
			}
			ev.Items.Data = append(ev.Items.Data, subscriptionItemEvent{CurrentPeriodEnd: it.CurrentPeriodEnd})
		}
	}
	return ev.EndsThisPeriod()
}

// stripeNotFound reports a 404 from Stripe: the object is gone for
// good, which is a permanent condition, not a transient failure.
func stripeNotFound(err error) bool {
	var serr *stripe.Error
	return errors.As(err, &serr) && serr.HTTPStatusCode == http.StatusNotFound
}

// stripeForbidden reports an API key that may not make the call: a
// restricted key without that permission.
func stripeForbidden(err error) bool {
	var serr *stripe.Error
	return errors.As(err, &serr) && serr.HTTPStatusCode == http.StatusForbidden
}

// staleClaimAge bounds how long a fulfilment claim may sit without a
// license before another caller takes it over — long enough for the
// slowest legitimate fulfilment (a few Stripe calls), short enough
// that a crashed process does not strand a paid session.
const staleClaimAge = 10 * time.Minute

// webhookWorkTimeout bounds a webhook's database work once it is
// detached from the request, well inside staleClaimAge. Calls to Stripe
// keep the client's own timeouts and retries.
const webhookWorkTimeout = 2 * time.Minute

// claimSession takes the fulfilment claim for a session. A claim
// left behind by a caller that died mid-way is released after
// staleClaimAge; a claim released by a failed insert is simply gone.
//
// A database error is returned as such: the caller must treat the
// session as not fulfilled and keep it scheduled for retry.
func (h *StripeHandler) claimSession(ctx context.Context, sessionID string) (bool, error) {
	claimed, _, err := h.reserve(ctx, fulfilledSessionProvider, sessionClaimProvider, sessionID, func() (bool, error) {
		_, err := h.Store.FindLicenseByStripeCheckoutSession(ctx, sessionID)
		if err == nil {
			return true, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return false, err
		}
		return h.upgradeSessionDone(ctx, sessionID)
	})
	return claimed, err
}

// upgradeSessionDone reports whether a session belongs to an upgrade
// that has applied or closed. Its row is the record: a reverted upgrade
// leaves no session id on the licence.
func (h *StripeHandler) upgradeSessionDone(ctx context.Context, sessionID string) (bool, error) {
	u, err := h.Store.FindLicenseUpgradeBySession(ctx, sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return u.State != model.UpgradePending && u.State != model.UpgradeExpired, nil
}

// fulfillSession fulfils a session fetched from the Stripe API.
func (h *StripeHandler) fulfillSession(ctx context.Context, sess *stripe.CheckoutSession, source string) (bool, error) {
	custID := ""
	if sess.Customer != nil {
		custID = sess.Customer.ID
	}
	subID := ""
	if sess.Subscription != nil {
		subID = sess.Subscription.ID
	}
	meta := sess.Metadata
	if meta == nil {
		meta = map[string]string{}
	}
	meta["session_id"] = sess.ID
	meta[metaSessionCreated] = strconv.FormatInt(sess.Created, 10)
	return h.fulfillCheckout(ctx, sessionEmail(sess), custID, subID, paymentIntentID(sess), meta, source)
}

// SyncPendingCheckouts re-checks sessions that completed before their
// delayed payment (bank debit, voucher) settled. Those can pay hours
// or days later, outside the window SyncRecentCheckouts scans, so a
// lost async_payment_succeeded event would otherwise leave a paying
// customer without a license. Only sessions this server saw complete
// unpaid are polled; each is dropped once fulfilled or expired.
func (h *StripeHandler) SyncPendingCheckouts(ctx context.Context) {
	pageSize, maxPerRound := 100, 1000
	if h.pendingBatchLimit > 0 {
		maxPerRound = h.pendingBatchLimit
		if pageSize > maxPerRound {
			pageSize = maxPerRound
		}
	}
	// A worker that lost the race records a pending marker after the
	// winner already cleared it; sweep those so the backlog only holds
	// sessions that still need work.
	if err := h.Store.DeleteFulfilledPendingSessions(ctx); err != nil {
		slog.Warn("stripe sync: failed to sweep fulfilled pending sessions", "error", err)
	}
	// Continue where the previous round stopped, so a backlog larger
	// than one round is walked to the end across rounds before the
	// walk starts over; nothing waits behind the oldest thousand.
	after := h.pendingAfter
	for seen := 0; seen < maxPerRound; {
		rows, err := h.Store.ListPendingCheckoutSessions(ctx, 30*24*time.Hour, after, pageSize)
		if err != nil {
			slog.Error("stripe sync: failed to list pending sessions", "error", err)
			return
		}
		if len(rows) == 0 {
			h.pendingAfter = nil // walked to the end; next round starts over
			return
		}
		for i := range rows {
			h.syncPendingSession(ctx, rows[i].SessionID)
		}
		seen += len(rows)
		after = &rows[len(rows)-1]
	}
	h.pendingAfter = after
}

func (h *StripeHandler) syncPendingSession(ctx context.Context, id string) {
	{
		sess, err := session.Get(id, nil)
		if err != nil {
			slog.Warn("stripe sync: failed to fetch pending session", "session_id", id, "error", err)
			return
		}
		switch {
		case sessionPaid(string(sess.PaymentStatus)):
			if ok, err := h.fulfillSession(ctx, sess, "sync"); !ok || err != nil {
				return // try again next round
			}
		case sess.Status == stripe.CheckoutSessionStatusExpired:
		default:
			return
		}
		_ = h.Store.DeleteProcessedEvent(ctx, pendingSessionProvider, id)
	}
}

func paymentIntentID(sess *stripe.CheckoutSession) string {
	if sess == nil || sess.PaymentIntent == nil {
		return ""
	}
	return sess.PaymentIntent.ID
}

// SyncRecentCheckouts scans Stripe for completed checkout sessions in the last
// interval and creates licenses for any that were missed by webhooks.
func (h *StripeHandler) SyncRecentCheckouts(ctx context.Context) {
	// List checkout sessions completed in the last 10 minutes
	cutoff := time.Now().Add(-10 * time.Minute).Unix()
	params := &stripe.CheckoutSessionListParams{
		Status: stripe.String("complete"),
	}
	params.Filters.AddFilter("created", "gte", fmt.Sprintf("%d", cutoff))
	params.Filters.AddFilter("limit", "", "50")

	iter := session.List(params)
	for iter.Next() {
		sess := iter.CheckoutSession()
		if !sessionPaid(string(sess.PaymentStatus)) {
			// Completed on a delayed payment method and the completed
			// webhook was missed too: track it like the webhook would.
			h.Store.TryRecordProcessedEvent(ctx, pendingSessionProvider, sess.ID)
			continue
		}

		if ok, _ := h.fulfillSession(ctx, sess, "sync"); !ok {
			// Outside the creation window the pending marker is the
			// only way back to this session.
			h.Store.TryRecordProcessedEvent(ctx, pendingSessionProvider, sess.ID)
		}
	}
	if err := iter.Err(); err != nil {
		slog.Error("stripe sync: failed to list sessions", "error", err)
	}
}

// invoiceEvent is the part of an invoice webhook payload Keygate acts
// on. Stripe moved the subscription reference in API version
// 2025-03-31: older versions put it at invoice.subscription, current
// ones under invoice.parent.subscription_details.subscription. Both
// shapes arrive in practice, since a webhook endpoint keeps the API
// version it was created with.
type invoiceEvent struct {
	Subscription string `json:"subscription"`
	PeriodEnd    int64  `json:"period_end"`
	Customer     string `json:"customer"`
	AmountDue    int64  `json:"amount_due"`
	AmountPaid   int64  `json:"amount_paid"`
	// Total is what the invoice billed. A renewal paid from the
	// customer's credit balance has amount_paid 0 but a total.
	Total            int64  `json:"total"`
	Currency         string `json:"currency"`
	HostedInvoiceURL string `json:"hosted_invoice_url"`
	// BillingReason is why Stripe raised the invoice; a change to the
	// subscription invoiced at once says "subscription_update".
	BillingReason string `json:"billing_reason"`
	Parent        *struct {
		SubscriptionDetails *struct {
			Subscription string `json:"subscription"`
		} `json:"subscription_details"`
	} `json:"parent"`
	Lines struct {
		Data []invoiceLine `json:"data"`
	} `json:"lines"`
}

// invoiceLine is the part of an invoice line ServiceEnd reads. Where a
// line comes from is told by type/proration up to API version
// 2025-03-31 and by parent from then on (both appear on older versions
// too); either is read.
type invoiceLine struct {
	Period struct {
		End int64 `json:"end"`
	} `json:"period"`
	Amount    int64  `json:"amount"`
	Type      string `json:"type"`
	Proration bool   `json:"proration"`
	Parent    *struct {
		Type               string `json:"type"`
		InvoiceItemDetails *struct {
			Proration bool `json:"proration"`
		} `json:"invoice_item_details"`
	} `json:"parent"`
}

// billsSubscription reports whether the line pays for the subscription
// itself: its recurring charge, or a proration from a change to it.
// Other invoice items — a one-off fee, say a year of support added to
// the invoice — carry a service period for revenue recognition that
// says nothing about how long the licence runs. A credit (negative
// amount) is not paid service either: moving from a yearly to a monthly
// price credits the unused year with a line running to the old year's
// end. A zero line still counts — a 100% coupon or a free plan.
func (l *invoiceLine) billsSubscription() bool {
	if l.Amount < 0 {
		return false
	}
	if l.Type == "subscription" || l.Proration {
		return true
	}
	if l.Parent == nil {
		return false
	}
	return l.Parent.Type == "subscription_item_details" ||
		(l.Parent.InvoiceItemDetails != nil && l.Parent.InvoiceItemDetails.Proration)
}

// ServiceEnd returns how far the invoice pays for the subscription: the
// latest period end among the lines that bill it, else the invoice's own
// period_end. They differ on a subscription's first invoice and on one
// raised mid-cycle — a prorated upgrade or seat billed at once — whose
// period_end is the moment it was raised, while the subscription's lines
// run to the end of the billing period. Zero when neither is set.
func (e *invoiceEvent) ServiceEnd() int64 {
	var end int64
	for i := range e.Lines.Data {
		if l := &e.Lines.Data[i]; l.billsSubscription() && l.Period.End > end {
			end = l.Period.End
		}
	}
	if end == 0 {
		end = e.PeriodEnd
	}
	return end
}

// SubscriptionID returns the subscription the invoice belongs to, in
// either payload shape, or "" for one-off invoices.
func (e *invoiceEvent) SubscriptionID() string {
	if e.Subscription != "" {
		return e.Subscription
	}
	if e.Parent != nil && e.Parent.SubscriptionDetails != nil {
		return e.Parent.SubscriptionDetails.Subscription
	}
	return ""
}

// subscriptionEvent is the part of a subscription webhook payload
// Keygate acts on. current_period_end moved from the subscription to
// its items in API version 2025-03-31; read both.
type subscriptionEvent struct {
	ID               string `json:"id"`
	Status           string `json:"status"`
	CurrentPeriodEnd int64  `json:"current_period_end"`
	TrialEnd         int64  `json:"trial_end"`
	// Set when the subscription will end instead of renewing: at the
	// period end, or at a chosen date (cancel_at).
	CancelAtPeriodEnd bool  `json:"cancel_at_period_end"`
	CancelAt          int64 `json:"cancel_at"`
	Items             struct {
		Data []subscriptionItemEvent `json:"data"`
	} `json:"items"`
	// PendingUpdate is a change Stripe holds until its invoice is paid
	// (payment_behavior=pending_if_incomplete); the items above are
	// still the ones in force meanwhile.
	PendingUpdate *struct {
		ExpiresAt         int64                   `json:"expires_at"`
		SubscriptionItems []subscriptionItemEvent `json:"subscription_items"`
	} `json:"pending_update"`
}

// subscriptionItemEvent is the part of a subscription item Keygate
// reads: when its period ends, how many it bills, and at which price.
type subscriptionItemEvent struct {
	CurrentPeriodEnd int64 `json:"current_period_end"`
	Quantity         int64 `json:"quantity"`
	Price            *struct {
		ID string `json:"id"`
	} `json:"price"`
}

// EndsThisPeriod reports whether the subscription stops at the end of the
// current billing period instead of renewing: cancel_at_period_end, or a
// cancel_at no later than the period end. A cancel_at further out still
// renews this period, so it is not "ending" yet; the
// customer.subscription.updated that Stripe sends at each renewal
// re-evaluates it. With no period end to compare against, a scheduled
// cancel_at counts as ending.
func (e *subscriptionEvent) EndsThisPeriod() bool {
	// Already over (a missed customer.subscription.deleted): no renewal.
	if e.CancelAtPeriodEnd || e.Status == "canceled" || e.Status == "incomplete_expired" {
		return true
	}
	if e.CancelAt <= 0 {
		return false
	}
	end := e.PeriodEnd()
	return end == 0 || e.CancelAt <= end
}

// PeriodEnd returns the end of the current billing period: the
// subscription-level value when present, else the latest item period
// end. Zero when neither is set — callers must not write that.
func (e *subscriptionEvent) PeriodEnd() int64 {
	if e.CurrentPeriodEnd > 0 {
		return e.CurrentPeriodEnd
	}
	var end int64
	for _, it := range e.Items.Data {
		if it.CurrentPeriodEnd > end {
			end = it.CurrentPeriodEnd
		}
	}
	return end
}

// applyLicenseFromSubscription writes what a subscription event says
// about its licence and reports whether the write landed.
//
// "No longer linked" is the ordinary outcome it looks like: an admin
// unlinked the licence after Stripe confirmed the subscription had
// ended, and this event — already in flight, or replayed — must not
// put the old billing state back.
//
// Callers must stop on false. Everything that follows one of these
// writes describes it to the outside world — an audit line, a
// license.canceled webhook, a dunning email — and saying a licence was
// cancelled while the row stays active is worse than silence: the
// downstream system revokes access the database still grants.
//
// A write that failed, rather than one refused as unlinked, stale or
// restricted, also returns its error when it came from a current read
// (asOf set); a webhook handler returns it, so Stripe retries the
// event, and the retry reads the subscription afresh. A write from an
// event's payload alone, made only when Stripe cannot be read (no API
// key, or a 404), is not retried: Stripe retries for days and in no
// particular order, and a failed payment retried after the invoice was
// paid would dun a customer who is paying. Losing it costs less: a
// later event sets the status again, except a lost pause, which leaves
// the licence usable until the subscription changes. A failure before
// the write (the lookup, the read stamp) is still retried, and on such
// an install that retry writes its payload late; that is rare, and
// accepted.
//
// asOf, when set, is the StripeReadStamp taken before the Stripe read the
// write comes from: a newer read already applied wins, and this one is
// dropped (see store.UpdateLicenseFromSubscriptionRead).
func (h *StripeHandler) applyLicenseFromSubscription(ctx context.Context, lic *model.License, event string, asOf *time.Time, cols ...string) (bool, error) {
	var err error
	if asOf != nil {
		err = h.Store.UpdateLicenseFromSubscriptionRead(ctx, lic, *asOf, cols...)
	} else {
		err = h.Store.UpdateLicenseFromSubscription(ctx, lic, cols...)
	}
	landed, err := h.subscriptionWriteLanded(lic, event, err)
	if asOf == nil {
		err = nil // from a payload: not retried, see above
	}
	return landed, err
}

// subscriptionWriteLanded reports whether a write from a subscription
// event landed, logging why when it did not (see
// applyLicenseFromSubscription). The error is set only for a write that
// failed; a refused one returns false and nil.
func (h *StripeHandler) subscriptionWriteLanded(lic *model.License, event string, err error) (bool, error) {
	if errors.Is(err, store.ErrSubscriptionUnlinked) {
		slog.Info("stripe webhook: license was unlinked from this subscription, event ignored",
			"event", event, "license_id", lic.ID, "subscription_id", lic.StripeSubscriptionID)
		return false, nil
	}
	if errors.Is(err, store.ErrStaleSubscriptionRead) {
		slog.Info("stripe webhook: a newer read of this subscription is already applied, event ignored",
			"event", event, "license_id", lic.ID, "subscription_id", lic.StripeSubscriptionID)
		return false, nil
	}
	if errors.Is(err, store.ErrLicenseRestricted) {
		slog.Info("stripe webhook: license is revoked, suspended by an operator or ended; event not applied",
			"event", event, "license_id", lic.ID, "subscription_id", lic.StripeSubscriptionID)
		return false, nil
	}
	if err != nil {
		slog.Error("stripe webhook: license write failed",
			"event", event, "license_id", lic.ID, "error", err)
		return false, err
	}
	return true, nil
}

// endLicenseFromSubscription is applyLicenseFromSubscription for a
// subscription that is over (see store.UpdateLicenseFromSubscriptionEnded).
// Callers must stop on false, for the same reason. A failed write is
// always retried: Stripe never revives an ended subscription, so a late
// retry has no newer state to undo, and no later event ends the licence.
func (h *StripeHandler) endLicenseFromSubscription(ctx context.Context, lic *model.License, cols ...string) (bool, error) {
	asOf, err := h.Store.StripeReadStamp(ctx)
	if err == nil {
		err = h.Store.UpdateLicenseFromSubscriptionEnded(ctx, lic, asOf, cols...)
	}
	if errors.Is(err, store.ErrSubscriptionUnlinked) || errors.Is(err, store.ErrLicenseRestricted) {
		slog.Info("stripe webhook: license unlinked or revoked, deletion not applied",
			"event", "customer.subscription.deleted", "license_id", lic.ID, "subscription_id", lic.StripeSubscriptionID)
		return false, nil
	}
	if err != nil {
		slog.Error("stripe webhook: license write failed",
			"event", "customer.subscription.deleted", "license_id", lic.ID, "error", err)
		return false, err
	}
	return true, nil
}

func (h *StripeHandler) onInvoicePaid(ctx context.Context, raw json.RawMessage) error {
	var data invoiceEvent
	if json.Unmarshal(raw, &data) != nil || data.SubscriptionID() == "" {
		return nil
	}

	lic, err := h.Store.FindLicenseByStripeSubscription(ctx, data.SubscriptionID())
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}

	// A paid invoice grants access only as the subscription stands now
	// (Stripe's own advice): one paid for a subscription that has since
	// ended, or the $0 invoice that opens a trial, is no reason to make
	// the licence active. So the subscription is read and applied like a
	// customer.subscription.updated.
	asOf, err := h.Store.StripeReadStamp(ctx)
	if err != nil {
		return err
	}
	cur, err := h.currentSubscription(data.SubscriptionID())
	if err != nil {
		return err
	}
	if cur != nil {
		return h.syncFromCurrent(ctx, lic, cur, asOf, "invoice.paid")
	}

	// No current read (no API key, or Stripe answers 404): the invoice
	// itself, which the write still checks against the licence's state
	// (see store.ErrLicenseRestricted).
	// A payment extends access and never shortens it: a mid-cycle invoice
	// must not pull the date in to the moment it was raised. Shortening
	// is subscription.updated's and .deleted's to do, from Stripe's
	// current state. Without any period end the date is left alone
	// rather than set to the epoch.
	if end := data.ServiceEnd(); end > 0 {
		if until := time.Unix(end, 0); lic.ValidUntil == nil || until.After(*lic.ValidUntil) {
			lic.ValidUntil = &until
		}
	}
	// A $0 invoice pays for nothing: the one that opens a trial leaves it
	// a trial, and one for a paused licence (a trial's opening delivered
	// late) leaves the pause. It moves only the date, so a copy read
	// before a concurrent resume or pause cannot write a stale status.
	cols := []string{"valid_until"}
	lifts := data.AmountPaid > 0 || data.Total > 0 || (lic.Status != model.StatusTrialing && lic.Status != model.StatusSuspended)
	if !lifts && lic.Status == model.StatusSuspended {
		return nil
	}
	if lifts {
		lic.Status = model.StatusActive
		lic.PastDueAt = nil
		lic.SuspendedAt, lic.SuspendedBy = nil, ""
		cols = append(cols, "status", "past_due_at", "suspended_at", "suspended_by")
	}
	if landed, err := h.applyLicenseFromSubscription(ctx, lic, "invoice.paid", nil, cols...); !landed {
		return err
	}
	// Written active with no suspension: a pause the row had when it was
	// locked has ended, or an expiry for non-payment, paid now.
	if lifts && lic.WasStripePaused {
		h.announceSuspension(ctx, lic, "reinstated", model.EventLicenseReinstated, "subscription_resumed")
	} else if lifts && lic.WasExpired {
		h.announceSuspension(ctx, lic, "reinstated", model.EventLicenseReinstated, "subscription_revived")
	}

	// Recovery notification — shares the dedup path with
	// onSubscriptionUpdated. Some flows emit invoice.paid without a
	// matching subscription.updated, others emit both; both call
	// this helper which fires at most once per cycle (per episode).
	if lifts && lic.WasPastDue {
		h.notifyPaymentRecovered(ctx, lic, lic.PriorPastDueAt)
	}
	return nil
}

// onSubscriptionUpdated applies a subscription change to its licence. A
// non-nil error means a transient failure: the webhook releases the event
// and Stripe retries it, and every step here is safe to repeat.
func (h *StripeHandler) onSubscriptionUpdated(ctx context.Context, raw json.RawMessage) error {
	var data subscriptionEvent
	if json.Unmarshal(raw, &data) != nil {
		return nil // malformed: a retry would not parse either
	}

	lic, err := h.Store.FindLicenseByStripeSubscription(ctx, data.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // not one of ours (or already unlinked)
	}
	if err != nil {
		return err
	}

	// Stripe does not deliver events in order: a "cancel at period end"
	// sent before a "resumed" can arrive after it, as can an "active"
	// sent before a cancellation. Act on the subscription as Stripe has
	// it now; the event only says which one changed.
	asOf, err := h.Store.StripeReadStamp(ctx)
	if err != nil {
		return err
	}
	cur, err := h.currentSubscription(data.ID)
	if err != nil {
		return err
	}
	if cur != nil {
		return h.syncFromCurrent(ctx, lic, cur, asOf, "customer.subscription.updated")
	}
	// Without a current read (no API key, or Stripe answers 404) the
	// payload is history, possibly older than a state already recorded,
	// and nothing would correct it later: the cancel state is left as it
	// is, and an unknown one stays unknown — no reminder rather than a
	// wrong one — until a webhook or the sync can read Stripe.
	return h.applySubscriptionState(ctx, lic, &data, nil, "customer.subscription.updated")
}

// syncFromCurrent applies a subscription as just read from Stripe (after
// asOf, a StripeReadStamp) to its licence: whether it renews, then its
// status and period.
func (h *StripeHandler) syncFromCurrent(ctx context.Context, lic *model.License, cur *subscriptionEvent, asOf time.Time, event string) error {
	// Whether it renews this period decides which reminder the customer
	// gets (renewal vs expiry); a resumed subscription clears it again.
	// Written first, and a failure is returned so Stripe retries: the
	// licence below is still untouched then, so the retry redoes the
	// whole event — including the past_due → active recovery mail,
	// which keys on the status this write has not yet changed.
	if err := h.Store.SetSubscriptionCancelScheduled(ctx, lic.ID, cur.EndsThisPeriod(), asOf); err != nil {
		return fmt.Errorf("record cancel-at-period-end: %w", err)
	}
	return h.applySubscriptionState(ctx, lic, cur, &asOf, event)
}

// applySubscriptionState writes a subscription's status and period to its
// licence, and sends the recovery notice on past_due → active. readAt is
// the StripeReadStamp taken before data was read from Stripe, nil when
// data is an event payload. The write checks the licence's own state
// (store.ErrLicenseRestricted): nothing here undoes an operator's
// suspension or revocation, or revives an ended subscription from a
// payload. An error means the event is to be retried, for instance
// when the licence write from a current read failed, an upgraded
// licence could not be put back on its free plan, or a downgrade still
// holds the plan and seats.
func (h *StripeHandler) applySubscriptionState(ctx context.Context, lic *model.License, data *subscriptionEvent, readAt *time.Time, event string) error {
	termsHeld := false
	cols := []string{"status", "canceled_at", "past_due_at"}
	usable := false

	switch data.Status {
	case "active":
		lic.Status = model.StatusActive
		// Recovery: customer fixed the card. Clear the dunning
		// anchor so a fresh past_due episode in the future starts
		// the ladder from day 0, not from the original failure.
		lic.PastDueAt = nil
		usable = true
	case "past_due":
		// Entering past_due starts an episode now. A licence already in
		// one keeps its start: the store reads the row under its lock
		// and keeps past_due_at, so repeated payment_failed webhooks do
		// not push the day-7 / day-14 reminders out. Not this copy's
		// date: the row may have recovered since it was read.
		lic.Status = model.StatusPastDue
		now := time.Now()
		lic.PastDueAt = &now
		usable = true
	case "trialing":
		lic.Status = model.StatusTrialing
		usable = true
	case "paused":
		// A trial that ended without a way to pay, or a subscription
		// paused in Stripe. Suspended by Stripe: usable again lifts it. The write is refused for a licence an
		// operator suspended or that ended; on one Stripe already paused
		// it keeps the pause's start and only records the read.
		lic.Status = model.StatusSuspended
		now := time.Now()
		lic.SuspendedAt = &now
		lic.SuspendedBy = model.SuspendedByStripe
		cols = append(cols, "suspended_at", "suspended_by")
	case "canceled", "unpaid":
		// An upgraded licence goes back to the free plan it came from.
		// An unpaid subscription is cancelled first: paying its open
		// invoice later would revive it with no licence following it.
		reason := upgradeEndCanceled
		if data.Status == "unpaid" {
			reason = upgradeEndUnpaid
		}
		if handled, err := h.endSubscriptionUpgrade(ctx, lic, lic.StripeSubscriptionID, reason, "webhook", data.Status == "unpaid"); handled || err != nil {
			return err
		}
		lic.Status = model.StatusCanceled
		if lic.CanceledAt == nil {
			now := time.Now()
			lic.CanceledAt = &now
		}
		lic.PastDueAt = nil
		// An ended subscription holds no change for payment.
		lic.StripePendingUpdateExpiresAt = nil
		cols = append(cols, "stripe_pending_update_expires_at")
		if endAccessNow(lic) {
			cols = append(cols, "valid_until")
		}
	}
	if usable {
		// Usable again lifts a Stripe pause; an operator's suspension
		// the write itself refuses to override. A licence an unpaid
		// subscription ended, paid since, is no longer canceled.
		lic.CanceledAt = nil
		lic.SuspendedAt, lic.SuspendedBy = nil, ""
		cols = append(cols, "suspended_at", "suspended_by")
	}

	// Only move valid_until when the payload carries a period end;
	// writing the zero value would expire the license on the spot. Not
	// for an ended subscription either: the end of its last period is no
	// reason for a canceled licence to stay usable longer.
	if end := data.PeriodEnd(); end > 0 && lic.Status != model.StatusCanceled {
		until := time.Unix(end, 0)
		lic.ValidUntil = &until
		cols = append(cols, "valid_until")
	}
	// What the subscription bills, from a current read of a live one
	// only: a payload may be older than a change already recorded, and
	// nothing orders it against one.
	var termCols []string
	if readAt != nil && usable {
		var err error
		if termCols, err = h.subscriptionTerms(ctx, lic, data); err != nil {
			return err
		}
		cols = append(cols, termCols...)
	}
	if len(termCols) == 0 {
		if landed, err := h.applyLicenseFromSubscription(ctx, lic, event, readAt, cols...); !landed {
			return err
		}
	} else {
		before, written, err := h.Store.UpdateLicenseFromSubscriptionReadTerms(ctx, lic, *readAt, cols...)
		if errors.Is(err, store.ErrTermsHeld) {
			// Written but for the plan and seats a downgrade holds: the
			// rest goes on, and the event is asked for again at the end.
			termsHeld, err = true, nil
		}
		landed, werr := h.subscriptionWriteLanded(lic, event, err)
		// The plan and seats are announced when they moved, a suspended
		// licence's included (see the store's write).
		if written {
			h.announceTermsChanged(ctx, lic, before, termCols, event)
		}
		if !landed {
			// A write that failed (not one refused as stale, unlinked or
			// restricted) is asked for again: the event is retried, as
			// nothing else may bring these seats back in line.
			if werr != nil {
				return fmt.Errorf("license %s: %w", lic.ID, werr)
			}
			return nil
		}
	}

	// Recovery notification fires only on past_due → active, as the row
	// was when this write locked it (not this copy, read before), for
	// the episode it ended. Routed through notifyPaymentRecovered so
	// concurrent webhooks (Stripe sometimes sends invoice.paid +
	// customer.subscription.updated in parallel) collapse into one
	// email + one webhook dispatch.
	if lic.WasPastDue && lic.Status == model.StatusActive {
		h.notifyPaymentRecovered(ctx, lic, lic.PriorPastDueAt)
	}
	// A pause starting or ending is told once, on the write that made it:
	// whether the row was already paused is read under its row lock.
	switch {
	case data.Status == "paused" && !lic.WasStripePaused:
		h.announceSuspension(ctx, lic, "suspended", model.EventLicenseSuspended, "subscription_paused")
	case usable && lic.WasStripePaused:
		h.announceSuspension(ctx, lic, "reinstated", model.EventLicenseReinstated, "subscription_resumed")
	case usable && (lic.WasCanceled || lic.WasExpired):
		// Unpaid (told as canceled) or past due too long (told as
		// expired), and usable again: told it is back, as written.
		// Stripe leaves unpaid only when paid, even straight into the
		// next past due; the store keeps a licence expired for
		// non-payment from past due, and a canceled one from a payload.
		h.announceSuspension(ctx, lic, "reinstated", model.EventLicenseReinstated, "subscription_revived")
	}
	// The licence's end is told once, by the write that ended it: this
	// one, or the deletion, whichever finds it not yet canceled.
	if lic.Status == model.StatusCanceled && !lic.WasCanceled {
		reason := "subscription_deleted"
		if data.Status == "unpaid" {
			reason = "subscription_unpaid"
		}
		h.announceCanceled(ctx, lic, "webhook", reason)
	}
	if termsHeld {
		// The plan and seats this read shows wait for the downgrade's hold
		// to end; an event is retried, and read afresh, until then.
		return fmt.Errorf("license %s: %w", lic.ID, store.ErrTermsHeld)
	}
	return nil
}

// announceCanceled records and dispatches the end of a licence's
// subscription.
func (h *StripeHandler) announceCanceled(ctx context.Context, lic *model.License, actor, reason string) {
	h.Store.Audit(ctx, &model.AuditLog{
		Entity: "license", EntityID: lic.ID, Action: "canceled",
		ActorType: actor, Changes: map[string]any{"provider": "stripe", "reason": reason},
	})
	if h.WebhookSvc != nil {
		h.WebhookSvc.Dispatch(ctx, lic.ProductID, model.EventLicenseCanceled, map[string]any{
			"license_id": lic.ID, "email": lic.Email, "reason": reason,
		})
	}
}

// announceSuspension records and dispatches a Stripe pause starting
// (action "suspended") or ending ("reinstated").
func (h *StripeHandler) announceSuspension(ctx context.Context, lic *model.License, action, event, reason string) {
	h.Store.Audit(ctx, &model.AuditLog{
		Entity: "license", EntityID: lic.ID, Action: action,
		ActorType: "webhook", Changes: map[string]any{"reason": reason, "provider": "stripe"},
	})
	if h.WebhookSvc == nil {
		return
	}
	payload := map[string]any{"license_id": lic.ID, "email": lic.Email}
	if action == "suspended" {
		payload["reason"] = reason
	}
	h.WebhookSvc.Dispatch(ctx, lic.ProductID, event, payload)
}

// notifyPaymentRecovered fires the recovery email + webhook exactly
// once per past_due episode.
//
// `since` is the past_due_at the recovery write found under its lock,
// before clearing the column. It enters the notification tag
// so a license that lapses → recovers → lapses → recovers in the
// same year gets the email twice (once per episode), instead of
// being permanently silenced after the first recovery by the
// notifications (license_id, tag) UNIQUE constraint.
//
// episode == 0 is a defensive fallback for legacy rows that
// somehow lack past_due_at; we still dedupe on the bare tag in
// that case to avoid a double-send within the single missing
// episode.
func (h *StripeHandler) notifyPaymentRecovered(ctx context.Context, lic *model.License, since *time.Time) {
	tag := "payment_recovered"
	if since != nil {
		tag = fmt.Sprintf("payment_recovered:%d", since.Unix())
	}
	// Claimed before anything is sent: of writes racing to tell the same
	// recovery, one sends. A claim the database could not record still
	// sends, as it did before: better a rare duplicate than no notice.
	claimed, err := h.Store.TryRecordNotification(ctx, lic.ID, tag)
	if err != nil {
		slog.Error("stripe webhook: recovery notice not recorded, sending anyway", "license_id", lic.ID, "error", err)
	} else if !claimed {
		return
	}
	productName := ""
	if p, err := h.Store.FindProductByID(ctx, lic.ProductID); err == nil {
		productName = p.Name
	}
	if h.Email != nil {
		h.Email.SendPaymentRecovered(lic.Email, productName)
	}
	if h.WebhookSvc != nil {
		h.WebhookSvc.Dispatch(ctx, lic.ProductID, "license.payment_recovered", map[string]any{
			"license_id": lic.ID, "email": lic.Email,
		})
	}
}

func (h *StripeHandler) onSubscriptionDeleted(ctx context.Context, raw json.RawMessage) error {
	var data struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(raw, &data) != nil {
		return nil
	}

	lic, err := h.Store.FindLicenseByStripeSubscription(ctx, data.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // not one of ours (or already unlinked)
	}
	if err != nil {
		// No later event ends this licence: a lookup that failed is
		// retried, not taken as "not ours".
		return err
	}
	// An upgraded licence goes back to the free plan it came from; a
	// failure is retried rather than ending the licence on the paid plan.
	if handled, err := h.endSubscriptionUpgrade(ctx, lic, data.ID, upgradeEndCanceled, "webhook", false); handled || err != nil {
		return err
	}
	lic.Status = model.StatusCanceled
	// When it ended stays when it first did: a deletion delivered again
	// does not move it.
	if lic.CanceledAt == nil {
		now := time.Now()
		lic.CanceledAt = &now
	}
	lic.PastDueAt = nil
	// Deletion is final: the write always applies. The end is told here
	// unless the licence was already canceled (by an unpaid or canceled
	// read, or the portal's immediate cancel), which told it then. The
	// write also counts as a read taken now, so a subscription.updated
	// whose read started before it (and may still have seen "active")
	// cannot bring the licence back afterwards.
	lic.StripePendingUpdateExpiresAt = nil
	cols := []string{"status", "canceled_at", "past_due_at", "stripe_pending_update_expires_at"}
	if endAccessNow(lic) {
		cols = append(cols, "valid_until")
	}
	if landed, err := h.endLicenseFromSubscription(ctx, lic, cols...); !landed {
		return err
	}
	if !lic.WasCanceled {
		h.announceCanceled(ctx, lic, "webhook", "subscription_deleted")
	}
	return nil
}

// endAccessNow brings the expiry of a licence whose subscription ended in
// to now, and reports whether it moved. Stripe ends a subscription when
// nothing more is owed: at the end of a period cancelled ahead, at once
// when someone cancels it now, or when its payment retries run out. A
// renewal that failed has already moved the period end to the end of
// the period nobody paid for, so that date is no reason to keep the
// licence usable. An earlier expiry stays as it is.
func endAccessNow(lic *model.License) bool {
	now := time.Now()
	if lic.ValidUntil != nil && !lic.ValidUntil.After(now) {
		return false
	}
	lic.ValidUntil = &now
	return true
}

// onPaymentFailed starts dunning for a failed invoice. A non-nil error
// is a transient failure; the event is retried.
func (h *StripeHandler) onPaymentFailed(ctx context.Context, raw json.RawMessage) error {
	var data invoiceEvent
	if json.Unmarshal(raw, &data) != nil || data.SubscriptionID() == "" {
		return nil
	}

	lic, err := h.Store.FindLicenseByStripeSubscription(ctx, data.SubscriptionID())
	if errors.Is(err, sql.ErrNoRows) {
		return nil // not one of ours (or already unlinked)
	}
	if err != nil {
		return err
	}
	// The status follows the subscription as Stripe has it now, like an
	// update, so a failure delivered after the invoice was paid cannot
	// dun a paying customer and a failed write is retried. Then the
	// failure is reported if the licence, as written, is past due.
	asOf, err := h.Store.StripeReadStamp(ctx)
	if err != nil {
		return err
	}
	cur, err := h.currentSubscription(data.SubscriptionID())
	if err != nil {
		return fmt.Errorf("read subscription %s: %w", data.SubscriptionID(), err)
	}
	if heldChangeInvoice(&data, cur) {
		// The invoice of a change Stripe holds until it is paid (more
		// seats, a dearer plan). Its failure leaves the subscription as
		// it was, paid and in force: the change just does not happen,
		// and the portal offers the payment again. No dunning.
		slog.Info("stripe webhook: payment for a held subscription change failed, licence left as it is",
			"license_id", lic.ID, "subscription_id", data.SubscriptionID())
		return nil
	}
	if cur != nil {
		// A downgrade still held back is retried for its plan and seats,
		// but the status was written: the failure is reported now.
		serr := h.syncFromCurrent(ctx, lic, cur, asOf, "invoice.payment_failed")
		if serr != nil && !errors.Is(serr, store.ErrTermsHeld) {
			return serr
		}
		now, err := h.Store.FindLicenseByID(ctx, lic.ID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil // deleted meanwhile: nothing left to report on
		}
		if err != nil {
			return err
		}
		if now.Status == model.StatusPastDue && now.PastDueAt != nil {
			if err := h.notifyPaymentFailed(ctx, now, data.SubscriptionID()); err != nil {
				return err
			}
		}
		return serr
	}

	// No current read (no API key, or Stripe answers 404): the payload.
	switch lic.Status {
	case model.StatusActive, model.StatusTrialing:
		// Trialing too: the charge at the end of a trial failing is where
		// a trial's dunning begins. (Invoices during a trial are for
		// nothing, so they cannot fail.)
		now := time.Now()
		lic.Status = model.StatusPastDue
		lic.PastDueAt = &now
		if landed, err := h.applyLicenseFromSubscription(ctx, lic, "invoice.payment_failed", nil, "status", "past_due_at"); !landed {
			return err
		}
	case model.StatusPastDue:
		// Already moved by a customer.subscription.updated that arrived
		// first — Stripe sends the two in either order. The failure is
		// still reported below, once per episode.
		if lic.PastDueAt == nil {
			return nil
		}
	default:
		return nil
	}

	// A payload write is never retried: on a retry hours later the
	// licence may be paid again, and this payload would dun it. A notice
	// that could not be recorded is logged instead.
	if err := h.notifyPaymentFailed(ctx, lic, data.SubscriptionID()); err != nil {
		slog.Error("stripe webhook: payment failure not reported", "license_id", lic.ID, "error", err)
	}
	return nil
}

// notifyPaymentFailed reports a failed payment once per past_due episode
// (keyed by when it began): Stripe sends invoice.payment_failed for every
// failed retry of the invoice, and the episode may already have been
// entered by subscription.updated. Recorded only while the licence is
// still past_due on this subscription in this episode: a payment may
// have recovered it meanwhile, or an admin unlinked it, and a stale
// license.payment_failed would tell downstream to act on a licence that
// is fine. An error recording it is returned, and the event retried.
func (h *StripeHandler) notifyPaymentFailed(ctx context.Context, lic *model.License, subscriptionID string) error {
	recorded, err := h.Store.TryRecordPastDueNotification(ctx, lic.ID, subscriptionID, *lic.PastDueAt,
		fmt.Sprintf("payment_failed:%d", lic.PastDueAt.Unix()))
	if !recorded {
		return err
	}
	h.Store.Audit(ctx, &model.AuditLog{
		Entity: "license", EntityID: lic.ID, Action: "payment_failed",
		ActorType: "webhook", Changes: map[string]any{"provider": "stripe"},
	})
	if h.WebhookSvc != nil {
		h.WebhookSvc.Dispatch(ctx, lic.ProductID, "license.payment_failed", map[string]any{
			"license_id": lic.ID, "email": lic.Email,
		})
	}
	return nil
}

// refundedCharge is the part of a charge a refund, or a lost dispute,
// is handled from.
type refundedCharge struct {
	ID             string            `json:"id"`
	Customer       string            `json:"customer"`
	Amount         int64             `json:"amount"`
	AmountRefunded int64             `json:"amount_refunded"`
	Refunded       bool              `json:"refunded"`
	PaymentIntent  string            `json:"payment_intent"`
	Invoice        string            `json:"invoice"`
	Metadata       map[string]string `json:"metadata"`
}

// onChargeRefunded handles a refund as if made now.
func (h *StripeHandler) onChargeRefunded(ctx context.Context, raw json.RawMessage) error {
	return h.onChargeRefundedAt(ctx, raw, time.Now().Unix())
}

// onChargeRefundedAt handles a refund made at the given time (the
// event's), so a retry delivered after a period ended reads it as the
// first delivery did.
func (h *StripeHandler) onChargeRefundedAt(ctx context.Context, raw json.RawMessage, at int64) error {
	var data refundedCharge
	if json.Unmarshal(raw, &data) != nil {
		return nil
	}
	return h.takeBackCharge(ctx, data, "refund", at)
}

// takeBackCharge undoes what a charge paid for once its money is given
// back: refunded at refundedAt (cause "refund"), or lost in a dispute
// ("dispute_lost", refundedAt 0), which ends what it paid for whatever
// period that was.
func (h *StripeHandler) takeBackCharge(ctx context.Context, data refundedCharge, cause string, refundedAt int64) error {

	// A renewal's payment intent is on its renewal row, not on a
	// license; looked up first so the customer fallback below cannot
	// mistake a refunded renewal for a refunded purchase. Done under
	// the renewal lock so the lookup and the fulfilment of the same
	// payment intent cannot interleave.
	if data.PaymentIntent != "" {
		handled := false
		var reverted *model.LicenseRenewal
		var partialOf *model.LicenseRenewal
		earlyRefund := false
		err := h.Store.WithXactLock(ctx, renewalLockKey(data.PaymentIntent, ""), func(ctx context.Context, tx bun.Tx) error {
			renewal, err := store.FindLicenseRenewalByPaymentIntentIn(ctx, tx, data.PaymentIntent)
			if err == nil {
				handled = true
				switch {
				case !data.Refunded:
					if data.AmountRefunded > 0 {
						partialOf = renewal
					}
				case renewal.RefundedAt != nil:
					// a second event for the same refund
				default:
					if err := store.RevertLicenseRenewalIn(ctx, tx, renewal); err != nil {
						return fmt.Errorf("revert renewal %s: %w", renewal.ID, err)
					}
					reverted = renewal
				}
				return nil
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("locate renewal for charge %s: %w", data.ID, err)
			}
			// No row yet, but the charge says it paid for a renewal:
			// Stripe may deliver the refund before the completion, or
			// fulfilment may still be pending. It must never reach the
			// purchase path below. A full refund is remembered so the
			// renewal, once applied, is recorded as refunded.
			if data.Metadata[metaKind] == kindRenewal {
				handled = true
				if data.Refunded {
					if _, err := store.ClaimProcessedEventIn(ctx, tx, renewalRefundProvider, data.PaymentIntent); err != nil {
						return fmt.Errorf("record early renewal refund %s: %w", data.PaymentIntent, err)
					}
					earlyRefund = true
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
		if partialOf != nil {
			h.Store.Audit(ctx, &model.AuditLog{
				Entity: "license", EntityID: partialOf.LicenseID, Action: "partial_refund",
				ActorType: "webhook",
				Changes:   map[string]any{"amount_refunded": data.AmountRefunded, "provider": "stripe", "renewal_id": partialOf.ID},
			})
		}
		if reverted != nil {
			h.Store.Audit(ctx, &model.AuditLog{
				Entity: "license", EntityID: reverted.LicenseID, Action: "updates_renewal_refunded",
				ActorType: "webhook",
				Changes:   map[string]any{"days": reverted.Days, "provider": "stripe", "charge_id": data.ID, "renewal_id": reverted.ID},
			})
			h.notifyUpdatesChanged(ctx, reverted.LicenseID, "renewal_refunded")
		}
		if earlyRefund {
			slog.Info("stripe refund: renewal refunded before it was applied", "payment_intent", data.PaymentIntent, "license_id", data.Metadata[metaLicenseID])
		}
		if handled {
			return nil
		}
	} else if data.Metadata[metaKind] == kindRenewal {
		return nil
	}

	// A full refund of an upgrade puts the licence back on its free plan
	// (or keeps an upgrade not yet applied from ever applying); revoking
	// it would take away the licence the customer had before paying.
	var inv chargeInvoice
	if data.Refunded {
		var handled bool
		var err error
		if handled, inv, err = h.refundUpgrade(ctx, data.ID, data.PaymentIntent, data.Invoice, data.Metadata, refundedAt); handled || err != nil {
			return err
		}
	}

	lic, paid, err := h.licenseForCharge(ctx, data.ID, data.PaymentIntent, data.Invoice, data.Customer, inv)
	if err != nil {
		return fmt.Errorf("locate license for charge %s: %w", data.ID, err)
	}
	if lic == nil {
		return nil
	}

	// A charge for a change made mid-period (more seats, a dearer plan)
	// paid that difference, not the licence. Refunding it gives the
	// difference back; the subscription, and the licence it pays for,
	// carry on. Fewer seats or the old plan is a change to make in
	// Stripe, which the next subscription event brings here.
	if data.Refunded && cause == "refund" && changeOnly(paid) {
		h.Store.Audit(ctx, &model.AuditLog{
			Entity: "license", EntityID: lic.ID, Action: "change_refunded",
			ActorType: "webhook",
			Changes:   map[string]any{"amount_refunded": data.AmountRefunded, "provider": "stripe", "charge_id": data.ID, "invoice_id": paid.ID},
		})
		return nil
	}

	if data.Refunded {
		// Only the licence's own payment takes it back: one found
		// through the customer alone may have paid for something else.
		// Left to an admin, as is any doubt about what was paid for.
		if paid == nil && (data.PaymentIntent == "" || data.PaymentIntent != lic.StripePaymentIntentID) {
			h.Store.Audit(ctx, &model.AuditLog{
				Entity: "license", EntityID: lic.ID, Action: "refund_unmatched",
				ActorType: "webhook",
				Changes:   map[string]any{"cause": cause, "amount_refunded": data.AmountRefunded, "provider": "stripe", "charge_id": data.ID, "customer_id": data.Customer},
			})
			return nil
		}
		// A period already over, refunded: the subscription, and the
		// licence it pays for, carry on. Ending them is a cancel in
		// Stripe, which arrives as its own event. A lost dispute stops
		// the billing whatever it was over (refundedAt 0): it closes
		// months after the charge, and billing the same card again
		// invites another.
		if pastInvoice(paid, refundedAt) {
			h.Store.Audit(ctx, &model.AuditLog{
				Entity: "license", EntityID: lic.ID, Action: "past_invoice_refunded",
				ActorType: "webhook",
				Changes:   map[string]any{"cause": cause, "amount_refunded": data.AmountRefunded, "provider": "stripe", "charge_id": data.ID, "invoice_id": paid.ID},
			})
			return nil
		}
		// A refund does not end the subscription in Stripe: left running,
		// it would go on billing a revoked licence. Cancelled first, as
		// for a refunded upgrade, and even for a licence already revoked
		// (cancelling twice is harmless); a cancel that failed changes
		// nothing here and the event is retried. Only when the money was
		// the subscription's own invoice: a licence found by a one-time
		// payment was not bought by its subscription.
		if paid != nil {
			if err := cancelSubscription(lic.StripeSubscriptionID); err != nil {
				return fmt.Errorf("refunded licence %s: %w", lic.ID, err)
			}
		}
		if lic.Status == model.StatusRevoked {
			return nil // a second refund event for the same purchase; nothing more to take away
		}
		lic.Status = model.StatusRevoked
		if err := h.Store.UpdateLicenseAndSubscription(ctx, lic, "status"); err != nil {
			return fmt.Errorf("revoke license %s: %w", lic.ID, err)
		}
		if lic.WasRevoked {
			return nil // a concurrent event revoked it first, and told it
		}

		h.Store.Audit(ctx, &model.AuditLog{
			Entity: "license", EntityID: lic.ID, Action: "revoked",
			ActorType: "webhook",
			Changes:   map[string]any{"reason": revokeReason(cause), "provider": "stripe", "charge_id": data.ID},
		})
		if h.WebhookSvc != nil {
			h.WebhookSvc.Dispatch(ctx, lic.ProductID, model.EventLicenseRevoked, map[string]any{
				"license_id": lic.ID, "email": lic.Email, "reason": cause,
			})
		}
	} else if data.AmountRefunded > 0 {
		h.Store.Audit(ctx, &model.AuditLog{
			Entity: "license", EntityID: lic.ID, Action: "partial_refund",
			ActorType: "webhook",
			Changes:   map[string]any{"amount_refunded": data.AmountRefunded, "provider": "stripe"},
		})
	}
	return nil
}

// Checkout session metadata for maintenance renewals. A renewal
// session is created by RenewUpdates only: it names the license it
// extends and freezes the days bought, so a plan edited between
// checkout and fulfilment cannot change what the customer paid for.
const (
	metaKind        = "kind"
	kindRenewal     = "renewal"
	metaLicenseID   = "license_id"
	metaRenewalDays = "renewal_days"
	// metaUpdatesDays freezes the update period a purchase includes,
	// as the plan read when the session was created. The plan may be
	// edited before the payment settles; the customer gets what was
	// on offer when they paid, not what the plan says later.
	metaUpdatesDays = "updates_days"
	// metaLicenseType freezes what kind of licence the purchase is
	// for. The period alone is not enough: a plan retyped to
	// subscription or trial before the payment settles would have
	// fulfilment build something the customer never bought — a
	// subscription with no end from a one-off payment, or a trial.
	metaLicenseType = "license_type"
	// metaSessionCreated carries the instant Stripe created the
	// checkout session. Injected locally like session_id, never sent
	// to Stripe; it dates a session against a later plan edit.
	metaSessionCreated = "session_created"
)

// sessionCreatedAt reports when Stripe created the checkout session,
// and whether that is known at all.
func sessionCreatedAt(metadata map[string]string) (time.Time, bool) {
	sec, err := strconv.ParseInt(metadata[metaSessionCreated], 10, 64)
	if err != nil || sec <= 0 {
		return time.Time{}, false
	}
	return time.Unix(sec, 0), true
}

// renewalRefundProvider keys processed_events rows for renewals whose
// full refund arrived before the renewal itself was applied; keyed by
// payment intent, the one id both events share.
const renewalRefundProvider = "stripe_renewal_refunded"

// renewalIneligibleProvider marks renewal sessions the operator has
// already been told about: the paid session stays pending and is
// retried every sync round, and one audit entry per session is enough.
const renewalIneligibleProvider = "stripe_renewal_ineligible"

// renewalLockKey names the transaction-scoped advisory lock under
// which a renewal is applied and refunded. Both handlers read and
// write the same two facts (the renewal row, the early-refund marker)
// from independent webhook deliveries; without mutual exclusion a
// refund can slip between the marker check and the commit and be
// lost. Keyed by payment intent, the id both events share; a session
// without one (nothing was charged) is keyed by itself.
func renewalLockKey(paymentIntentID, sessionID string) int64 {
	hash := fnv.New64a()
	if paymentIntentID != "" {
		hash.Write([]byte("renewal:" + paymentIntentID))
	} else {
		hash.Write([]byte("renewal-session:" + sessionID))
	}
	return int64(hash.Sum64())
}

// fulfillRenewal applies a paid renewal session to its license. Same
// contract as fulfillCheckout: (true, nil) applied or already applied,
// (false, nil) needs operator attention, (false, err) retry later.
//
// It does not consult the rollout switch, and that is deliberate: switching the maintenance
// features off stops new bounded licences being issued and stops new
// renewal checkouts being created, but a renewal checkout already out
// there is paid for an update period the licence *already has* — no
// data shape a replica on the previous version has not already been
// living with. Refusing it would keep the money and give nothing
// back, which is worse than extending a date those replicas ignore
// either way. Switching the features off does not reach the checkout
// sessions already open in Stripe.
func (h *StripeHandler) fulfillRenewal(ctx context.Context, metadata map[string]string, sessionID, paymentIntentID, source string) (bool, error) {
	licenseID := metadata[metaLicenseID]
	days, _ := strconv.Atoi(metadata[metaRenewalDays])
	if sessionID == "" || licenseID == "" || days <= 0 {
		slog.Warn("stripe renewal: session lacks renewal metadata", "session_id", sessionID, "metadata", metadata, "source", source)
		return false, nil
	}
	if _, err := h.Store.FindLicenseRenewalBySession(ctx, sessionID); err == nil {
		return true, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("check renewal: %w", err)
	}
	lic, err := h.Store.FindLicenseByID(ctx, licenseID)
	if errors.Is(err, sql.ErrNoRows) {
		slog.Warn("stripe renewal: license no longer exists", "session_id", sessionID, "license_id", licenseID)
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("find license %s: %w", licenseID, err)
	}

	claimed, done, err := h.reserve(ctx, fulfilledSessionProvider, sessionClaimProvider, sessionID, func() (bool, error) {
		_, err := h.Store.FindLicenseRenewalBySession(ctx, sessionID)
		if err == nil {
			return true, nil
		}
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	})
	if err != nil {
		return false, fmt.Errorf("claim session: %w", err)
	}
	if !claimed {
		if done {
			return true, nil // applied by another caller meanwhile
		}
		return h.sessionFulfilled(ctx, sessionID)
	}

	renewal := &model.LicenseRenewal{
		LicenseID:               lic.ID,
		StripeCheckoutSessionID: sessionID,
		StripePaymentIntentID:   paymentIntentID,
		Days:                    days,
	}
	// A full refund that arrived first means there is nothing to
	// grant: the ledger gets the row, the license stays as it is.
	// Marker check, eligibility and apply all happen in one
	// transaction under the renewal lock, so a refund handled
	// meanwhile either sees the committed row or has left its marker
	// before this check, and an admin edit cannot slip between the
	// eligibility check and the write.
	refundedFirst := false
	err = h.Store.WithXactLock(ctx, renewalLockKey(paymentIntentID, sessionID), func(ctx context.Context, tx bun.Tx) error {
		if paymentIntentID != "" {
			var err error
			if refundedFirst, err = store.HasProcessedEventIn(ctx, tx, renewalRefundProvider, paymentIntentID); err != nil {
				return fmt.Errorf("check early refund: %w", err)
			}
		}
		if refundedFirst {
			if err := store.RecordRefundedRenewalIn(ctx, tx, renewal); err != nil {
				return err
			}
			return store.DeleteProcessedEventIn(ctx, tx, renewalRefundProvider, paymentIntentID)
		}
		return store.ApplyLicenseRenewalIn(ctx, tx, renewal)
	})
	switch {
	case err == nil:
	case store.IsRenewalSessionConflict(err):
		slog.Warn("stripe renewal: session applied concurrently", "session_id", sessionID)
		return true, nil
	case errors.Is(err, store.ErrRenewalIneligible):
		// The customer paid, but the license has nothing to extend
		// any more (updates for life, or no longer perpetual). Not
		// fulfilled: the session stays pending for an operator, who
		// refunds it in Stripe — that refund's event then records
		// the renewal as refunded and clears the session — or gives
		// the license a finite period again, after which the sync
		// applies it.
		if derr := h.release(ctx, fulfilledSessionProvider, sessionClaimProvider, sessionID); derr != nil {
			slog.Error("stripe renewal: failed to release session claim", "session_id", sessionID, "error", derr)
		}
		if first, _ := h.Store.ClaimProcessedEvent(ctx, renewalIneligibleProvider, sessionID); first {
			h.Store.Audit(ctx, &model.AuditLog{
				Entity: "license", EntityID: lic.ID, Action: "updates_renewal_ineligible",
				ActorType: "webhook",
				Changes:   map[string]any{"session_id": sessionID, "payment_intent": paymentIntentID, "days": days, "provider": "stripe", "source": source},
			})
		}
		slog.Warn("stripe renewal: paid renewal cannot be applied — the licence is not active, or has no finite update period; refund it, or make it eligible again",
			"license_id", lic.ID, "session_id", sessionID, "payment_intent", paymentIntentID, "source", source)
		return false, nil
	default:
		if derr := h.release(ctx, fulfilledSessionProvider, sessionClaimProvider, sessionID); derr != nil {
			slog.Error("stripe renewal: failed to release session claim", "session_id", sessionID, "error", derr)
		}
		return false, fmt.Errorf("apply renewal: %w", err)
	}
	if err := h.Store.CompleteProcessedEvent(ctx, sessionClaimProvider, sessionID); err != nil {
		slog.Error("stripe renewal: failed to record session completion", "session_id", sessionID, "error", err)
	}
	_ = h.Store.DeleteProcessedEvent(ctx, pendingSessionProvider, sessionID)
	_ = h.Store.DeleteProcessedEvent(ctx, renewalIneligibleProvider, sessionID)

	action, until := "updates_renewed", ""
	if refundedFirst {
		action = "updates_renewal_refunded"
	} else {
		until = renewal.UpdatesUntil.Format(time.RFC3339)
	}
	h.Store.Audit(ctx, &model.AuditLog{
		Entity: "license", EntityID: lic.ID, Action: action,
		ActorType: "webhook",
		Changes: map[string]any{
			"days": days, "updates_until": until,
			"session_id": sessionID, "provider": "stripe", "source": source,
		},
	})
	if !refundedFirst {
		h.notifyUpdatesChanged(ctx, lic.ID, "renewal")
	}
	slog.Info("stripe renewal: applied", "license_id", lic.ID, "days", days, "action", action, "source", source)
	return true, nil
}

// notifyUpdatesChanged sends license.updates_changed with the licence's
// update period as it now stands, read back after the write. null means
// no cutoff, as on the admin route that sends the same event.
func (h *StripeHandler) notifyUpdatesChanged(ctx context.Context, licenseID, reason string) {
	if h.WebhookSvc == nil {
		return
	}
	lic, err := h.Store.FindLicenseByID(ctx, licenseID)
	if err != nil {
		slog.Error("stripe renewal: license.updates_changed not sent", "license_id", licenseID, "error", err)
		return
	}
	payload := map[string]any{"license_id": lic.ID, "email": lic.Email, "updates_until": nil, "reason": reason}
	if until := lic.EffectiveUpdatesUntil(); until != nil {
		payload["updates_until"] = until.Format(time.RFC3339)
	}
	h.WebhookSvc.Dispatch(ctx, lic.ProductID, model.EventLicenseUpdatesChanged, payload)
}

// RenewUpdates starts a one-time checkout that extends the
// maintenance period of one of the caller's perpetual licenses.
//
// POST /portal/updates/renew  { license_id }  → { url }
func (h *StripeHandler) RenewUpdates(c *gin.Context) {
	var req struct {
		LicenseID string `json:"license_id" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "license_id is required")
		return
	}
	lic, err := h.Store.FindLicenseByID(c, req.LicenseID)
	if err != nil {
		response.NotFound(c, "license not found")
		return
	}
	// Ownership is by address, case-insensitively: the session email
	// is normalised to lower case at login while fulfilment stores
	// the address as Stripe sent it.
	emailVal, _ := c.Get("email")
	if e, ok := emailVal.(string); !ok || !strings.EqualFold(lic.Email, e) {
		response.Forbidden(c, "not your license")
		return
	}
	plan, err := h.Store.FindPlanByID(c, lic.PlanID)
	if err != nil {
		response.Internal(c, err)
		return
	}
	// Nothing to sell: the plan offers no renewal, or the license
	// includes updates for life.
	if !plan.OffersRenewal() || lic.UpdatesUntil == nil {
		response.Err(c, http.StatusBadRequest, "RENEWAL_NOT_AVAILABLE", "this license does not offer update renewals")
		return
	}
	if lic.Status != model.StatusActive {
		response.Err(c, http.StatusBadRequest, "LICENSE_NOT_ACTIVE", "only active licenses can renew updates")
		return
	}
	// Selling is gated by the operator's switch: while it is off,
	// replicas that do not understand renewal fulfilment or refunds
	// may still be serving. Fulfilment of an already paid renewal is
	// deliberately NOT gated — refusing that would keep the money and
	// give nothing back.
	on, err := h.Store.MaintenanceFeaturesEnabled(c)
	if err != nil {
		response.Internal(c, err)
		return
	}
	if !on {
		response.Conflict(c, "MAINTENANCE_FEATURES_DISABLED",
			"update renewals are not on sale right now; try again later", nil)
		return
	}

	// A renewal is bought once, so the session is always payment mode
	// and the price must be a live one-time price. Stripe, not local
	// config, is the source of truth for what a price is (the same
	// reason CheckoutByPlan reads it); handing a recurring price to a
	// payment-mode session would fail inside Stripe and reach the
	// customer as a 500.
	sp, perr := stripeprice.Get(plan.StripeRenewalPriceID, nil)
	if perr != nil || sp.Type != "one_time" || !sp.Active {
		switch {
		case perr != nil:
			slog.Error("stripe renewal: failed to fetch the renewal price",
				"plan_id", plan.ID, "price_id", plan.StripeRenewalPriceID, "error", perr)
		case sp.Type != "one_time":
			slog.Error("stripe renewal: the plan's renewal price is recurring; a renewal is a one-time purchase",
				"plan_id", plan.ID, "price_id", plan.StripeRenewalPriceID, "price_type", sp.Type)
		default:
			slog.Error("stripe renewal: the plan's renewal price is archived in Stripe",
				"plan_id", plan.ID, "price_id", plan.StripeRenewalPriceID)
		}
		response.Err(c, http.StatusServiceUnavailable, "RENEWAL_UNAVAILABLE",
			"update renewals are temporarily unavailable; please try again later")
		return
	}

	params := &stripe.CheckoutSessionParams{
		Mode: stripe.String(string(stripe.CheckoutSessionModePayment)),
		LineItems: []*stripe.CheckoutSessionLineItemParams{
			{Price: stripe.String(plan.StripeRenewalPriceID), Quantity: stripe.Int64(1)},
		},
		SuccessURL:          stripe.String(h.BaseURL + "/checkout/success?session_id={CHECKOUT_SESSION_ID}"),
		CancelURL:           stripe.String(h.BaseURL + "/portal"),
		AllowPromotionCodes: stripe.Bool(true),
	}
	if lic.StripeCustomerID != "" {
		params.Customer = stripe.String(lic.StripeCustomerID)
	} else {
		params.CustomerEmail = stripe.String(lic.Email)
	}
	params.Metadata = map[string]string{
		metaKind:        kindRenewal,
		metaLicenseID:   lic.ID,
		metaRenewalDays: strconv.Itoa(plan.RenewalDays),
		"product_id":    lic.ProductID,
	}
	// The charge inherits the payment intent's metadata, so a
	// charge.refunded that arrives before the renewal is applied can
	// still be told apart from a purchase refund.
	params.PaymentIntentData = &stripe.CheckoutSessionPaymentIntentDataParams{
		Metadata: map[string]string{metaKind: kindRenewal, metaLicenseID: lic.ID},
	}
	sess, err := session.New(params)
	if err != nil {
		slog.Error("stripe renewal: failed to create checkout session", "license_id", lic.ID, "error", err)
		response.Internal(c, err)
		return
	}
	response.OK(c, gin.H{"url": sess.URL})
}

func (h *StripeHandler) CancelSubscription(c *gin.Context) {
	var req struct {
		LicenseID string `json:"license_id" binding:"required"`
		Immediate bool   `json:"immediate"` // false = cancel at period end (default)
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "license_id is required")
		return
	}

	lic, err := h.Store.FindLicenseByID(c, req.LicenseID)
	if err != nil {
		response.NotFound(c, "license not found")
		return
	}

	emailVal, _ := c.Get("email")
	if e, ok := emailVal.(string); !ok || !strings.EqualFold(lic.Email, e) {
		response.Forbidden(c, "not your license")
		return
	}

	if lic.StripeSubscriptionID == "" {
		response.BadRequest(c, "no active subscription")
		return
	}

	// Once Stripe has the cancel, the writes and what they tell finish
	// even if the customer closes the page.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), webhookWorkTimeout)
	defer cancel()
	if req.Immediate {
		// Stripe answers 404 to cancelling a subscription that already
		// ended, which is the end asked for. Only Stripe saying so counts:
		// a key that cannot see a subscription answers the same way.
		alreadyEnded := false
		if _, err = subscription.Cancel(lic.StripeSubscriptionID, nil); err != nil {
			ended, eerr := h.SubscriptionEnded(ctx, lic.StripeSubscriptionID)
			var ae *apperr.AppError
			switch {
			case eerr == nil && ended:
				alreadyEnded = true
			case errors.As(eerr, &ae):
				response.Err(c, ae.Status, ae.Code, ae.Message)
				return
			default:
				response.Internal(c, err)
				return
			}
		}
		// Ended before this cancel, and the licence with it (a second
		// click, or the deletion got here first): the cancel was asked and
		// told then, and nothing is written or told again.
		if alreadyEnded && lic.Status == model.StatusCanceled {
			response.OK(c, gin.H{"status": "canceled", "immediate": true})
			return
		}
		// A licence an upgrade moved onto this subscription goes back to
		// its free plan, as the deletion would put it: it has not ended,
		// and nothing says it has. A revert that failed is the deletion's
		// to do again.
		handled, uerr := h.endSubscriptionUpgrade(ctx, lic, lic.StripeSubscriptionID, upgradeEndCanceled, "user", false)
		if uerr != nil {
			slog.Warn("stripe: upgrade not reverted on cancel, left to the deletion", "license_id", lic.ID, "error", uerr)
		}
		if handled || uerr != nil {
			h.cancelRequested(ctx, c, lic, req.Immediate)
			return
		}
		now := time.Now()
		lic.Status = model.StatusCanceled
		lic.CanceledAt = &now
		lic.ValidUntil = &now
		lic.PastDueAt = nil
		// Final, like customer.subscription.deleted: stamped so a read
		// taken before the cancel (which may still have seen "active")
		// cannot bring the licence back.
		at, err := h.Store.StripeReadStamp(ctx)
		if err == nil {
			err = h.Store.UpdateLicenseFromSubscriptionEnded(ctx, lic, at, "status", "canceled_at", "valid_until", "past_due_at")
		}
		// The deletion Stripe sends next finds it canceled and tells
		// nothing: the end is told here. A write that failed leaves the
		// ending, and telling it, to the deletion.
		if err != nil {
			slog.Warn("stripe: cancel not written, left to the deletion", "license_id", lic.ID, "error", err)
		} else if !lic.WasCanceled {
			h.announceCanceled(ctx, lic, "user", "subscription_deleted")
		}
	} else {
		// Before the update: Stripe's answer is at least this new.
		cancelAsOf, stampErr := h.Store.StripeReadStamp(ctx)
		sub, updateErr := subscription.Update(lic.StripeSubscriptionID, &stripe.SubscriptionParams{
			CancelAtPeriodEnd: stripe.Bool(true),
		})
		if updateErr != nil {
			response.Internal(c, updateErr)
			return
		}
		// Set ValidUntil to when Stripe will cancel the subscription;
		// without that date in the answer, the webhook that follows
		// sets it rather than the epoch. Stamped like other subscription
		// writes: an unguarded UpdateLicense could land after an
		// immediate cancel and push valid_until back out, restoring
		// access on a canceled licence.
		if sub.CancelAt > 0 {
			periodEnd := time.Unix(sub.CancelAt, 0)
			lic.ValidUntil = &periodEnd
			if stampErr != nil {
				slog.Warn("stripe: cancel-at-period-end valid_until not stamped", "license_id", lic.ID, "error", stampErr)
			} else if err := h.Store.UpdateLicenseFromSubscriptionRead(ctx, lic, cancelAsOf, "valid_until"); err != nil {
				// Stale or restricted (immediate cancel already ended it):
				// the subscription.updated webhook that follows records
				// the period end if it still should.
				if !errors.Is(err, store.ErrStaleSubscriptionRead) &&
					!errors.Is(err, store.ErrLicenseRestricted) &&
					!errors.Is(err, store.ErrSubscriptionUnlinked) {
					slog.Warn("stripe: cancel-at-period-end valid_until not written", "license_id", lic.ID, "error", err)
				}
			}
		}
		// It now runs out rather than renews: the expiry reminder applies,
		// not "renews tomorrow". The subscription.updated webhook that
		// follows records the same.
		if stampErr != nil {
			slog.Warn("stripe: failed to record cancel-at-period-end", "license_id", lic.ID, "error", stampErr)
		} else if err := h.Store.SetSubscriptionCancelScheduled(ctx, lic.ID, true, cancelAsOf); err != nil {
			// The subscription.updated webhook Stripe sends for this
			// change records it as well.
			slog.Warn("stripe: failed to record cancel-at-period-end", "license_id", lic.ID, "error", err)
		}
	}

	h.cancelRequested(ctx, c, lic, req.Immediate)
}

// cancelRequested records a customer's cancel, tells them, and answers.
func (h *StripeHandler) cancelRequested(ctx context.Context, c *gin.Context, lic *model.License, immediate bool) {
	h.Store.Audit(ctx, &model.AuditLog{
		Entity: "license", EntityID: lic.ID, Action: "cancel_requested",
		ActorType: "user",
		Changes:   map[string]any{"immediate": immediate},
	})

	productName := ""
	if lic.Product != nil {
		productName = lic.Product.Name
	} else if p, err := h.Store.FindProductByID(ctx, lic.ProductID); err == nil {
		productName = p.Name
	}
	if h.Email != nil {
		h.Email.SendSubscriptionCanceled(lic.Email, productName, immediate)
	}

	response.OK(c, gin.H{
		"status":    "canceled",
		"immediate": immediate,
	})
}

// ChangePlan moves a subscription to another plan, keeping its seats,
// without a preview: the API form of ChangeSubscription. It is charged
// the same way (see subscription_change.go): a dearer plan applies once
// the difference is paid, so status may be pending_payment, with the
// invoice to pay at hosted_invoice_url, or processing when Stripe's
// answer did not come back.
//
// There is no proration switch. It was the customer's to set, and
// "none" let one move to a dearer plan for the rest of a period without
// paying the difference, then move back before renewal.
func (h *StripeHandler) ChangePlan(c *gin.Context) {
	var req struct {
		LicenseID  string `json:"license_id" binding:"required"`
		NewPriceID string `json:"new_price_id" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "license_id and new_price_id are required")
		return
	}
	out, ok := h.changeSubscription(c, changeRequest{LicenseID: req.LicenseID, PriceID: req.NewPriceID, made: true}, time.Now().Unix(), nil)
	if !ok {
		return
	}
	// The fields this endpoint has always answered with, beside the
	// outcome.
	out["new_plan_id"], out["new_plan_name"] = out["plan_id"], out["plan_name"]
	out["proration"] = "always_invoice"
	if out["downgrade"] == true || out["keep"] == true {
		out["proration"] = "none" // billed from the next invoice, or nothing changed
	}
	if out["status"] == changeApplied {
		out["status"] = "plan_changed"
	}
	if out["status"] == changeProcessing {
		c.JSON(http.StatusAccepted, response.Envelope{Success: true, Data: out})
		return
	}
	response.OK(c, out)
}

// licenseForCharge finds the license a charge paid for.
//
//   - One-time purchases: the charge's payment intent is the one the
//     checkout session recorded on the license.
//   - Subscription invoices: older API versions put the invoice id on
//     the charge; current ones (2025-03-31+) don't, so the invoice is
//     found through its payments, filtered by the payment intent.
//     Either way the invoice's subscription names the license.
//   - Customer only: a customer can hold several licenses, so the id
//     alone is trusted only when it names exactly one, and even then
//     only to note the refund there: the charge may have paid for
//     something else, and revoking a guess would take a paid license
//     away from the wrong purchase.
//
// nil,nil means no license is attributable (a permanent condition);
// an error means a lookup failed and the event should be retried.
// known carries the charge's invoice when a caller already asked
// Stripe for it. inv is the invoice the licence was found through, nil
// when it was not found through one.
func (h *StripeHandler) licenseForCharge(ctx context.Context, chargeID, paymentIntent, invoiceID, customerID string, known chargeInvoice) (lic *model.License, inv *stripe.Invoice, err error) {
	if paymentIntent != "" {
		lic, err := h.Store.FindLicenseByStripePaymentIntent(ctx, paymentIntent)
		if err == nil {
			return lic, nil, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, nil, err
		}
	}
	// An invoice that bills a subscription no licence follows is not one
	// of ours (another product's, or one an admin unlinked): the
	// customer's licence is not what it paid for.
	if known.looked {
		if known.invoice != nil {
			if lic, err := h.licenseForInvoice(ctx, known.invoice); err != nil || lic != nil || billsSubscription(known.invoice) {
				return lic, known.invoice, err
			}
		}
	} else if stripe.Key != "" {
		if invoiceID != "" {
			inv, err := stripeinvoice.Get(invoiceID, nil)
			if err != nil && !stripeNotFound(err) {
				return nil, nil, err
			}
			if err == nil {
				if lic, err := h.licenseForInvoice(ctx, inv); err != nil || lic != nil || billsSubscription(inv) {
					return lic, inv, err
				}
			}
		}
		if paymentIntent != "" {
			params := &stripe.InvoicePaymentListParams{
				Payment: &stripe.InvoicePaymentListPaymentParams{
					Type:          stripe.String("payment_intent"),
					PaymentIntent: stripe.String(paymentIntent),
				},
			}
			params.Status = stripe.String("paid") // one payment may be tried against several invoices
			params.AddExpand("data.invoice")
			params.Filters.AddFilter("limit", "", "1")
			iter := invoicepayment.List(params)
			if iter.Next() {
				paid := iter.InvoicePayment().Invoice
				if lic, err := h.licenseForInvoice(ctx, paid); err != nil || lic != nil || billsSubscription(paid) {
					return lic, paid, err
				}
			} else if err := iter.Err(); err != nil && !stripeNotFound(err) {
				return nil, nil, fmt.Errorf("list invoice payments: %w", err)
			}
		}
	}
	if customerID == "" {
		return nil, nil, nil
	}
	ls, err := h.Store.ListLicensesByStripeCustomer(ctx, customerID)
	if err != nil {
		return nil, nil, err
	}
	if len(ls) == 0 {
		return nil, nil, nil
	}
	if len(ls) > 1 {
		slog.Warn("stripe charge: customer holds several licenses, not acting on charge",
			"charge_id", chargeID, "customer_id", customerID, "licenses", len(ls))
		h.Store.Audit(ctx, &model.AuditLog{
			Entity: "charge", EntityID: chargeID, Action: "unmatched",
			ActorType: "webhook",
			Changes:   map[string]any{"customer_id": customerID, "licenses": len(ls), "provider": "stripe"},
		})
		return nil, nil, nil
	}
	return ls[0], nil, nil
}

// revokeReason is the audit reason for a licence revoked by cause.
func revokeReason(cause string) string {
	if cause == "refund" {
		return "full_refund"
	}
	return cause
}

// pastInvoice reports whether inv billed a subscription for a period
// over by at: every line's period had ended. Its own lines say so; the
// subscription's latest invoice does not, as a change made mid-period
// raises a newer one. A list Stripe cut short is not known to be over:
// prorations come first, the period's own line may be on a later page.
// Nothing is past at 0.
func pastInvoice(inv *stripe.Invoice, at int64) bool {
	if at == 0 || !billsSubscription(inv) || inv.Lines == nil || inv.Lines.HasMore || len(inv.Lines.Data) == 0 {
		return false
	}
	for _, l := range inv.Lines.Data {
		if l == nil || l.Period == nil || l.Period.End > at {
			return false
		}
	}
	return true
}

// billsSubscription reports whether an invoice was raised for a
// subscription.
func billsSubscription(inv *stripe.Invoice) bool {
	return inv != nil && inv.Parent != nil && inv.Parent.SubscriptionDetails != nil &&
		inv.Parent.SubscriptionDetails.Subscription != nil && inv.Parent.SubscriptionDetails.Subscription.ID != ""
}

func (h *StripeHandler) licenseForInvoice(ctx context.Context, inv *stripe.Invoice) (*model.License, error) {
	if inv == nil || inv.Parent == nil || inv.Parent.SubscriptionDetails == nil || inv.Parent.SubscriptionDetails.Subscription == nil {
		return nil, nil
	}
	lic, err := h.Store.FindLicenseByStripeSubscription(ctx, inv.Parent.SubscriptionDetails.Subscription.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return lic, nil
}

// disputeEvent is the part of a dispute its events are handled from.
type disputeEvent struct {
	ID            string                `json:"id"`
	Status        string                `json:"status"`
	Reason        string                `json:"reason"`
	Amount        int64                 `json:"amount"`
	Charge        *stripe.Charge        `json:"charge"`         // an ID, or the object expanded
	PaymentIntent *stripe.PaymentIntent `json:"payment_intent"` // likewise
}

func (h *StripeHandler) onDisputeCreated(ctx context.Context, raw json.RawMessage) error {
	var dispute disputeEvent
	if json.Unmarshal(raw, &dispute) != nil {
		return nil
	}
	// A bank debit's dispute (SEPA, ACH, Bacs) is final from the start:
	// created lost, and no closing event is promised.
	if err := h.takeBackLostDispute(ctx, dispute); err != nil {
		return err
	}
	charge := ""
	if dispute.Charge != nil {
		charge = dispute.Charge.ID
	}
	h.Store.Audit(ctx, &model.AuditLog{
		Entity: "dispute", EntityID: dispute.ID, Action: "created",
		ActorType: "webhook",
		Changes: map[string]any{
			"charge_id": charge,
			"reason":    dispute.Reason,
			"amount":    dispute.Amount,
			"status":    dispute.Status,
		},
	})
	return nil
}

func (h *StripeHandler) onDisputeClosed(ctx context.Context, raw json.RawMessage) error {
	var dispute disputeEvent
	if json.Unmarshal(raw, &dispute) != nil {
		return nil
	}
	if err := h.takeBackLostDispute(ctx, dispute); err != nil {
		return err
	}
	h.Store.Audit(ctx, &model.AuditLog{
		Entity: "dispute", EntityID: dispute.ID, Action: "closed",
		ActorType: "webhook",
		Changes:   map[string]any{"status": dispute.Status, "reason": dispute.Reason},
	})
	return nil
}

// takeBackLostDispute handles a lost dispute as a full refund of its
// charge: the payment is taken back for good. Won, withdrawn or still
// open, the payment stands. Safe to repeat: both a dispute's events
// may say lost.
func (h *StripeHandler) takeBackLostDispute(ctx context.Context, dispute disputeEvent) error {
	if dispute.Status != "lost" || dispute.Charge == nil || dispute.Charge.ID == "" {
		return nil
	}
	// The charge's metadata says what it paid for: a renewal not yet
	// applied is known by nothing else. Read unless the event has it;
	// a key that may not read charges goes on with the dispute's
	// payment intent, which finds the rest.
	ch := dispute.Charge
	if ch.Metadata == nil && stripe.Key != "" {
		got, err := stripecharge.Get(ch.ID, nil)
		switch {
		case err == nil:
			ch = got
		case stripeForbidden(err):
			slog.Warn("stripe webhook: the API key cannot read charges, using the dispute's payment intent", "charge_id", ch.ID)
		case !stripeNotFound(err):
			return fmt.Errorf("read charge %s: %w", ch.ID, err)
		}
	}
	c := refundedCharge{ID: ch.ID, Amount: dispute.Amount, AmountRefunded: dispute.Amount, Refunded: true, Metadata: ch.Metadata}
	if ch.Customer != nil {
		c.Customer = ch.Customer.ID
	}
	if ch.PaymentIntent != nil {
		c.PaymentIntent = ch.PaymentIntent.ID
	} else if dispute.PaymentIntent != nil {
		c.PaymentIntent = dispute.PaymentIntent.ID
	}
	if err := h.takeBackCharge(ctx, c, "dispute_lost", 0); err != nil {
		return fmt.Errorf("lost dispute %s: %w", dispute.ID, err)
	}
	return nil
}

// CreatePortalSession creates a Stripe billing portal session for the user to manage payment methods.
func (h *StripeHandler) CreatePortalSession(c *gin.Context) {
	var req struct {
		LicenseID string `json:"license_id" binding:"required"`
		ReturnURL string `json:"return_url"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "license_id is required")
		return
	}

	lic, err := h.Store.FindLicenseByID(c, req.LicenseID)
	if err != nil {
		response.NotFound(c, "license not found")
		return
	}

	// Verify ownership
	emailVal, _ := c.Get("email")
	if e, ok := emailVal.(string); !ok || !strings.EqualFold(lic.Email, e) {
		response.Forbidden(c, "not your license")
		return
	}

	if lic.StripeCustomerID == "" {
		response.BadRequest(c, "no Stripe customer associated")
		return
	}

	// Stripe sends the customer back to return_url, so only a page of
	// this site is accepted; anything else falls back to the portal.
	returnURL := req.ReturnURL
	base := strings.TrimRight(h.BaseURL, "/")
	if returnURL != base && !strings.HasPrefix(returnURL, base+"/") {
		returnURL = base + "/portal"
	}

	params := &stripe.BillingPortalSessionParams{
		Customer:  stripe.String(lic.StripeCustomerID),
		ReturnURL: stripe.String(returnURL),
	}
	s, err := portalsession.New(params)
	if err != nil {
		response.Internal(c, err)
		return
	}

	response.OK(c, gin.H{"url": s.URL})
}

// ListInvoices returns invoice history for a license's Stripe customer.
func (h *StripeHandler) ListInvoices(c *gin.Context) {
	licenseID := c.Query("license_id")
	if licenseID == "" {
		response.BadRequest(c, "license_id is required")
		return
	}

	lic, err := h.Store.FindLicenseByID(c, licenseID)
	if err != nil {
		response.NotFound(c, "license not found")
		return
	}

	// Verify ownership
	emailVal, _ := c.Get("email")
	if e, ok := emailVal.(string); !ok || !strings.EqualFold(lic.Email, e) {
		response.Forbidden(c, "not your license")
		return
	}

	if lic.StripeCustomerID == "" {
		response.OK(c, gin.H{"invoices": []any{}})
		return
	}

	params := &stripe.InvoiceListParams{
		Customer: stripe.String(lic.StripeCustomerID),
	}
	params.Filters.AddFilter("limit", "", "20")

	type invoiceItem struct {
		ID          string `json:"id"`
		Number      string `json:"number"`
		Status      string `json:"status"`
		AmountDue   int64  `json:"amount_due"`
		AmountPaid  int64  `json:"amount_paid"`
		Currency    string `json:"currency"`
		Created     int64  `json:"created"`
		PeriodStart int64  `json:"period_start"`
		PeriodEnd   int64  `json:"period_end"`
		InvoicePDF  string `json:"invoice_pdf"`
		HostedURL   string `json:"hosted_url"`
	}

	var invoices []invoiceItem
	iter := stripeinvoice.List(params)
	// The iterator pages on by itself; checking the count first keeps
	// this to the 20 newest, one request to Stripe.
	for len(invoices) < 20 && iter.Next() {
		inv := iter.Invoice()
		invoices = append(invoices, invoiceItem{
			ID:          inv.ID,
			Number:      inv.Number,
			Status:      string(inv.Status),
			AmountDue:   inv.AmountDue,
			AmountPaid:  inv.AmountPaid,
			Currency:    string(inv.Currency),
			Created:     inv.Created,
			PeriodStart: inv.PeriodStart,
			PeriodEnd:   inv.PeriodEnd,
			InvoicePDF:  inv.InvoicePDF,
			HostedURL:   inv.HostedInvoiceURL,
		})
	}
	if err := iter.Err(); err != nil {
		response.Internal(c, err)
		return
	}

	response.OK(c, gin.H{"invoices": response.Array(invoices)})
}

// The events below only send an email or write an audit line. A lookup
// that failed is retried, not taken as "not ours"; it comes before
// anything is sent, so a retry sends nothing twice.
func (h *StripeHandler) onPaymentActionRequired(ctx context.Context, raw json.RawMessage) error {
	var data invoiceEvent
	if json.Unmarshal(raw, &data) != nil || data.SubscriptionID() == "" {
		return nil
	}
	lic, err := h.Store.FindLicenseByStripeSubscription(ctx, data.SubscriptionID())
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	// Notify user to complete 3DS/SCA authentication
	productName := h.productName(ctx, lic.ProductID)
	if h.Email != nil {
		h.Email.SendPaymentActionRequired(lic.Email, productName, data.HostedInvoiceURL)
	}
	h.Store.Audit(ctx, &model.AuditLog{
		Entity: "license", EntityID: lic.ID, Action: "payment_action_required",
		ActorType: "webhook", Changes: map[string]any{"provider": "stripe"},
	})
	return nil
}

func (h *StripeHandler) onTrialWillEnd(ctx context.Context, raw json.RawMessage) error {
	var data struct {
		ID       string `json:"id"`
		TrialEnd int64  `json:"trial_end"`
	}
	if json.Unmarshal(raw, &data) != nil {
		return nil
	}
	lic, err := h.Store.FindLicenseByStripeSubscription(ctx, data.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	productName := h.productName(ctx, lic.ProductID)
	trialEnd := time.Unix(data.TrialEnd, 0).Format("2006-01-02")
	if h.Email != nil {
		h.Email.SendTrialEnding(lic.Email, productName, trialEnd)
	}
	return nil
}

func (h *StripeHandler) onInvoiceUpcoming(ctx context.Context, raw json.RawMessage) error {
	var data invoiceEvent
	if json.Unmarshal(raw, &data) != nil || data.SubscriptionID() == "" {
		return nil
	}
	lic, err := h.Store.FindLicenseByStripeSubscription(ctx, data.SubscriptionID())
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	// Renewal reminder is handled by expiry checker, but this is a backup from Stripe
	// Just audit it
	h.Store.Audit(ctx, &model.AuditLog{
		Entity: "license", EntityID: lic.ID, Action: "invoice_upcoming",
		ActorType: "webhook", Changes: map[string]any{"amount_due": data.AmountDue, "currency": data.Currency},
	})
	return nil
}

func (h *StripeHandler) onCustomerUpdated(ctx context.Context, raw json.RawMessage) error {
	var data struct {
		ID    string `json:"id"`
		Email string `json:"email"`
	}
	if json.Unmarshal(raw, &data) != nil || data.ID == "" {
		return nil
	}
	// The account may have customers of other products: none of ours,
	// nothing to read or write.
	if ls, err := h.Store.ListLicensesByStripeCustomer(ctx, data.ID); err != nil {
		return fmt.Errorf("licences of customer %s: %w", data.ID, err)
	} else if len(ls) == 0 {
		return nil
	}
	// The address as Stripe has it now: events arrive in any order, and
	// a read cannot put back one changed since, so a failure is retried.
	if stripe.Key != "" {
		cur, err := stripecustomer.Get(data.ID, nil)
		switch {
		case stripeNotFound(err):
			return nil
		case stripeForbidden(err):
			// A restricted key without customer access: the payload,
			// as without a key, rather than retrying for days.
			slog.Warn("stripe webhook: the API key cannot read customers, using the event's email", "customer_id", data.ID)
		case err != nil:
			return fmt.Errorf("read customer %s: %w", data.ID, err)
		default:
			if cur.Deleted || cur.Email == "" {
				return nil
			}
			return h.Store.UpdateLicenseEmailByStripeCustomer(ctx, data.ID, cur.Email)
		}
	}
	// Without an API key, the payload. Not retried: a retry hours later
	// could put back an address changed since.
	if data.Email == "" {
		return nil
	}
	if err := h.Store.UpdateLicenseEmailByStripeCustomer(ctx, data.ID, data.Email); err != nil {
		slog.Error("stripe webhook: customer email not updated", "customer_id", data.ID, "error", err)
	}
	return nil
}

func (h *StripeHandler) productName(ctx context.Context, productID string) string {
	if p, err := h.Store.FindProductByID(ctx, productID); err == nil && p.Name != "" {
		return p.Name
	}
	slog.Warn("product name not found, using fallback", "product_id", productID)
	return "Your Software"
}

// resolvePlanFromLineItems maps a checkout session to a plan through
// the price on its first line item. Keygate creates single-item
// sessions; a multi-item session built elsewhere fulfils its first
// item only.
func (h *StripeHandler) resolvePlanFromLineItems(ctx context.Context, sessionID string) (*model.Plan, error) {
	params := &stripe.CheckoutSessionListLineItemsParams{Session: stripe.String(sessionID)}
	params.Filters.AddFilter("limit", "", "1")
	iter := session.ListLineItems(params)
	if !iter.Next() {
		if err := iter.Err(); err != nil && !stripeNotFound(err) {
			return nil, err
		}
		return nil, nil
	}
	item := iter.LineItem()
	if item.Price == nil || item.Price.ID == "" {
		return nil, nil
	}
	return h.planForPrice(ctx, item.Price.ID)
}

// resolvePlan maps a subscription to a plan through its first item's
// price. nil,nil when the subscription has no items or the price is
// not mapped; an error for Stripe or database failures.
//
// quantity is what the subscription bills (see subscriptionQuantity),
// 1 when Stripe cannot see it.
func (h *StripeHandler) resolvePlan(ctx context.Context, subID string) (plan *model.Plan, quantity int, sub *stripe.Subscription, err error) {
	sub, err = subscription.Get(subID, nil)
	if err != nil {
		if stripeNotFound(err) {
			return nil, 1, nil, nil
		}
		return nil, 1, nil, err
	}
	if sub.Items == nil || len(sub.Items.Data) == 0 {
		return nil, 1, sub, nil
	}
	// The item whose price a plan sells: an add-on the seller put on the
	// subscription next to it says nothing about the licence.
	for _, it := range sub.Items.Data {
		if it == nil || it.Price == nil {
			continue
		}
		plan, err = h.planForPrice(ctx, it.Price.ID)
		if err != nil || plan != nil {
			return plan, clampQuantity(it.Quantity), sub, err
		}
	}
	// No item a plan sells: one item is still the seats bought (the plan
	// comes from the session); with several, which one is unknown.
	if len(sub.Items.Data) == 1 {
		return nil, subscriptionQuantity(sub), sub, nil
	}
	return nil, 1, sub, nil
}

// subscriptionStart sets a new licence from the subscription it is
// issued for: trialing while the subscription is, and valid to the end
// of the period it bills. Nothing without a subscription, or a period.
func subscriptionStart(lic *model.License, sub *stripe.Subscription) {
	if sub == nil {
		return
	}
	switch sub.Status {
	case stripe.SubscriptionStatusTrialing:
		lic.Status = model.StatusTrialing
	case stripe.SubscriptionStatusActive:
		lic.Status = model.StatusActive
	}
	var end int64
	if sub.Items != nil {
		for _, it := range sub.Items.Data {
			if it != nil && it.CurrentPeriodEnd > end {
				end = it.CurrentPeriodEnd
			}
		}
	}
	if end > 0 {
		until := time.Unix(end, 0)
		lic.ValidUntil = &until
	}
}

// followNewSubscription applies the subscription, read now, to the
// licence just issued for it (see fulfillCheckout). Best effort: the
// licence is issued either way.
func (h *StripeHandler) followNewSubscription(ctx context.Context, lic *model.License) {
	if lic.StripeSubscriptionID == "" {
		return
	}
	asOf, err := h.Store.StripeReadStamp(ctx)
	if err != nil {
		slog.Warn("stripe checkout: new licence not read against its subscription", "license_id", lic.ID, "error", err)
		return
	}
	cur, err := h.currentSubscriptionCtx(ctx, lic.StripeSubscriptionID)
	if err != nil || cur == nil {
		slog.Warn("stripe checkout: new licence not read against its subscription", "license_id", lic.ID, "error", err)
		return
	}
	if err := h.syncFromCurrent(ctx, lic, cur, asOf, "checkout.session.completed"); err != nil {
		slog.Warn("stripe checkout: new licence not brought in line with its subscription", "license_id", lic.ID, "error", err)
	}
}

// planForPrice distinguishes "no plan uses this price" from a
// database failure.
func (h *StripeHandler) planForPrice(ctx context.Context, priceID string) (*model.Plan, error) {
	plan, err := h.Store.FindPlanByStripePrice(ctx, priceID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return plan, nil
}
