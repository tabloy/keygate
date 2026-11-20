package payment

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/stripe/stripe-go/v82"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
)

// Plans sold per seat (issue #41). The buyer pays Stripe for a quantity
// and the licence holds what that quantity buys (model.Plan.Limits).
// Stripe is the record of the quantity: Keygate asks Stripe for a
// change, and the licence takes the new quantity from a read of the
// subscription after Stripe applied it. The one exception is a
// downgrade, which the licence takes just before Stripe is asked (so
// nothing is taken on the limits it removes) and gives back if Stripe
// does not make it (see changeSubscription).

// checkoutLineItem is what a checkout for plan sells: one licence, or on
// a plan sold per seat the seats asked for (the plan's minimum when the
// caller named none), which the buyer can still change on Stripe's page
// within the plan's range. requested is 0 when no quantity was named.
// Only a subscription is sold per seat; a one-time checkout is one unit.
// A metered price (see priceMetered) bills the usage reported to its
// meter: Stripe refuses a quantity for it, so it carries none, and it is
// not sold per seat.
func checkoutLineItem(plan *model.Plan, priceID string, subscription, metered bool, requested int) (*stripe.CheckoutSessionLineItemParams, error) {
	item := &stripe.CheckoutSessionLineItemParams{Price: stripe.String(priceID), Quantity: stripe.Int64(1)}
	if requested < 0 {
		return nil, errors.New("quantity must be 1 or more")
	}
	if metered {
		if plan.PerSeat || requested > 1 {
			return nil, errors.New("this plan is billed by usage, not sold per seat")
		}
		item.Quantity = nil
		return item, nil
	}
	if !plan.PerSeat || !subscription {
		if requested > 1 {
			return nil, errors.New("this plan is not sold per seat")
		}
		return item, nil
	}
	q := requested
	if q == 0 {
		q = plan.MinQuantity
	}
	if q < plan.MinQuantity || q > plan.MaxQuantity {
		return nil, fmt.Errorf("quantity must be between %d and %d", plan.MinQuantity, plan.MaxQuantity)
	}
	item.Quantity = stripe.Int64(int64(q))
	if plan.MinQuantity < plan.MaxQuantity {
		item.AdjustableQuantity = &stripe.CheckoutSessionLineItemAdjustableQuantityParams{
			Enabled: stripe.Bool(true),
			Minimum: stripe.Int64(int64(plan.MinQuantity)),
			Maximum: stripe.Int64(int64(plan.MaxQuantity)),
		}
	}
	return item, nil
}

// priceMetered reports whether a price bills the usage reported to a
// meter rather than a quantity.
func priceMetered(p *stripe.Price) bool {
	return p != nil && p.Recurring != nil && p.Recurring.UsageType == stripe.PriceRecurringUsageTypeMetered
}

// subscriptionQuantity is the quantity Stripe bills on a subscription's
// item, as a licence holds it: never below one.
func subscriptionQuantity(sub *stripe.Subscription) int {
	if sub == nil || sub.Items == nil || len(sub.Items.Data) == 0 || sub.Items.Data[0] == nil {
		return 1
	}
	return clampQuantity(sub.Items.Data[0].Quantity)
}

// clampQuantity keeps a quantity read from Stripe inside what a licence
// row holds: at least one, and no more than an int carries. Keygate never
// sells fewer than one (min_quantity is at least 1); a subscription the
// seller set to 0 in Stripe keeps its owner's licence, as any licence on
// it would.
func clampQuantity(q int64) int {
	switch {
	case q < 1:
		return 1
	case q > 1<<31-1:
		return 1<<31 - 1
	}
	return int(q)
}

// subscriptionTerms moves lic to what a current read of its subscription
// bills: the quantity and plan of the item whose price is one of the
// licence's product's plans, and whether Stripe holds a change until an
// invoice is paid. It returns the columns it set. A subscription with a
// single item on a price no plan sells any more keeps its plan and still
// takes that item's quantity; one with several items, none of them the
// product's, says nothing about the seats, and they are left alone.
func (h *StripeHandler) subscriptionTerms(ctx context.Context, lic *model.License, data *subscriptionEvent) ([]string, error) {
	if len(data.Items.Data) == 0 {
		return nil, nil
	}
	lic.StripePendingUpdateExpiresAt = nil
	if data.PendingUpdate != nil && data.PendingUpdate.ExpiresAt > 0 {
		expires := time.Unix(data.PendingUpdate.ExpiresAt, 0)
		lic.StripePendingUpdateExpiresAt = &expires
	}
	cols := []string{"stripe_pending_update_expires_at"}
	item, plan, err := h.productItem(ctx, lic.ProductID, data.Items.Data)
	if err != nil {
		return nil, err
	}
	switch {
	case plan != nil:
		lic.Quantity = clampQuantity(item.Quantity)
		// Written even when this copy of the licence already has it: an
		// older read may land in between and set another, and this newer
		// one must put the plan Stripe bills back. Whether it changed is
		// decided under the lock (see announceTermsChanged).
		lic.PlanID = plan.ID
		cols = append(cols, "quantity", "plan_id")
	case len(data.Items.Data) == 1:
		slog.Warn("stripe: subscription bills a price no plan of the product sells, plan left as it is",
			"license_id", lic.ID, "subscription_id", data.ID)
		lic.Quantity = clampQuantity(data.Items.Data[0].Quantity)
		cols = append(cols, "quantity")
	default:
		slog.Warn("stripe: no item of the subscription bills a plan of the product, seats left as they are",
			"license_id", lic.ID, "subscription_id", data.ID, "items", len(data.Items.Data))
	}
	return cols, nil
}

// productItem finds the item of a subscription whose price is one of the
// product's plans. plan is nil when none is.
func (h *StripeHandler) productItem(ctx context.Context, productID string, items []subscriptionItemEvent) (*subscriptionItemEvent, *model.Plan, error) {
	for i := range items {
		it := &items[i]
		if it.Price == nil || it.Price.ID == "" {
			continue
		}
		plan, err := h.planForPrice(ctx, it.Price.ID)
		if err != nil {
			return nil, nil, fmt.Errorf("plan for price %s: %w", it.Price.ID, err)
		}
		if plan != nil && plan.ProductID == productID {
			return it, plan, nil
		}
	}
	return nil, nil, nil
}

// announceTermsChanged tells the audit log and the merchant's webhooks
// that a licence's plan or quantity moved, comparing what the write
// found under the lock (before) with what it wrote. A column the write
// did not carry is no change, and neither is a value it found already
// in place: the delivery that moved it has announced it.
func (h *StripeHandler) announceTermsChanged(ctx context.Context, lic *model.License, before store.LicenseTerms, cols []string, source string) {
	if slices.Contains(cols, "plan_id") && before.PlanID != lic.PlanID {
		h.Store.Audit(ctx, &model.AuditLog{
			Entity: "license", EntityID: lic.ID, Action: "plan_changed",
			ActorType: "webhook",
			Changes:   map[string]any{"old_plan_id": before.PlanID, "new_plan_id": lic.PlanID, "provider": "stripe", "source": source},
		})
		if h.WebhookSvc != nil {
			h.WebhookSvc.Dispatch(ctx, lic.ProductID, model.EventPlanChanged, map[string]any{
				"license_id": lic.ID, "old_plan_id": before.PlanID, "new_plan_id": lic.PlanID,
			})
		}
	}
	if slices.Contains(cols, "quantity") && before.Quantity != lic.Quantity && h.seatsMatter(ctx, before.PlanID, lic.PlanID) {
		h.Store.Audit(ctx, &model.AuditLog{
			Entity: "license", EntityID: lic.ID, Action: "quantity_changed",
			ActorType: "webhook",
			Changes:   map[string]any{"old_quantity": before.Quantity, "new_quantity": lic.Quantity, "provider": "stripe", "source": source},
		})
		if h.WebhookSvc != nil {
			h.WebhookSvc.Dispatch(ctx, lic.ProductID, model.EventLicenseQuantityChanged, map[string]any{
				"license_id": lic.ID, "email": lic.Email, "plan_id": lic.PlanID,
				"old_quantity": before.Quantity, "new_quantity": lic.Quantity,
			})
		}
	}
}

// seatsMatter reports whether a move in quantity between these plans
// moves a limit: one plan or the other sold per seat. On a plan that is
// not (a quantity a seller set in Stripe, first read after the upgrade
// that added the column), it moves nothing worth announcing.
func (h *StripeHandler) seatsMatter(ctx context.Context, planIDs ...string) bool {
	for _, id := range planIDs {
		plan, err := h.Store.FindPlanByID(ctx, id)
		if err != nil || plan.PerSeat {
			return true
		}
	}
	return false
}

// heldChangeInvoice reports whether a failed invoice is for a change to
// the subscription that left the subscription in good standing: a
// change Stripe held until paid, now paid, replaced or dropped, or still
// waiting. Its failure is no reason to dun the customer. A change that
// did put the subscription behind (one made in Stripe without holding
// it) shows as past_due there, and is dunned as usual. cur is the
// subscription as Stripe has it now (nil when it cannot be read).
func heldChangeInvoice(inv *invoiceEvent, cur *subscriptionEvent) bool {
	return inv.BillingReason == "subscription_update" &&
		cur != nil && (cur.Status == "active" || cur.Status == "trialing")
}

// changeOnly reports whether an invoice bills only the difference a
// change to the subscription made for the rest of its period: raised for
// a subscription update, every line a proration. A change that started a
// new period (monthly to yearly, a trial ended early) bills that period
// on the same invoice; refunding it is refunding the subscription. A
// list Stripe cut short is not known to be prorations only.
func changeOnly(inv *stripe.Invoice) bool {
	if inv == nil || inv.BillingReason != stripe.InvoiceBillingReasonSubscriptionUpdate ||
		inv.Lines == nil || inv.Lines.HasMore || len(inv.Lines.Data) == 0 {
		return false
	}
	for _, l := range inv.Lines.Data {
		if !prorationLine(l) {
			return false
		}
	}
	return true
}

// prorationLine reports whether a line is a proration, in either shape
// Stripe raises one in: on the subscription item itself, and as the
// pending proration invoice item a change to the subscription raises.
// invoiceLine.billsSubscription reads both; reading only the first here
// takes a refunded change for a refunded subscription, which revokes the
// licence and cancels what pays for it.
func prorationLine(l *stripe.InvoiceLineItem) bool {
	if l == nil || l.Parent == nil {
		return false
	}
	if d := l.Parent.SubscriptionItemDetails; d != nil && d.Proration {
		return true
	}
	d := l.Parent.InvoiceItemDetails
	return d != nil && d.Proration
}
