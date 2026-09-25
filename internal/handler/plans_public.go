package handler

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stripe/stripe-go/v82"
	stripeprice "github.com/stripe/stripe-go/v82/price"

	"github.com/tabloy/keygate/internal/store"
	"github.com/tabloy/keygate/pkg/response"
)

// PublicPlansHandler serves GET /api/v1/products/:product_slug/plans —
// the plan catalogue a pricing page needs before the visitor has an
// account. /portal/plans covers the same ground but sits behind a
// session, which is no use to an anonymous visitor deciding whether to
// buy.
//
// Price is not a Keygate column: the Stripe Price is the source of
// truth (payment.CheckoutByPlan reads it at checkout time), so this
// handler asks Stripe and caches the answer. Without the cache every
// anonymous page load would fan out one Stripe call per plan, which is
// slow and lets unauthenticated traffic burn our Stripe rate budget —
// the same reason /checkout/verify is throttled.
type PublicPlansHandler struct {
	Store *store.Store
	Log   *slog.Logger

	// cacheMu guards cache and refreshing together — a lock that only
	// covers the map lookup lets every request at the expiry instant
	// start its own Stripe call.
	cacheMu sync.Mutex
	cache   map[string]cachedPrice
	// refreshing marks the price IDs a lookup is already in flight for.
	// Latecomers serve the stale entry instead of piling on: one
	// refresh per price, however many visitors arrive at once.
	refreshing map[string]bool
	cacheTTL   time.Duration
	// retryTTL is how long a failed lookup is remembered. Without it a
	// Stripe outage means every anonymous request retries every plan,
	// turning a public page into an amplifier aimed at Stripe.
	retryTTL time.Duration
	// stripeWait bounds one price lookup. The Stripe SDK's own default
	// is 80s, which on an anonymous endpoint means a stalled Stripe
	// parks request goroutines instead of just leaving the price blank.
	stripeWait time.Duration
	// stripeBudget bounds all of them together. The lookups run one
	// after another, so a per-call limit alone lets a catalogue of ten
	// uncached plans hold a request for ten times that. Once the
	// budget is gone the remaining plans list without a price, which
	// is the same answer they already give when Stripe says no.
	stripeBudget time.Duration
}

type cachedPrice struct {
	amount   int64
	currency string
	// valid marks that amount/currency hold a real answer. A failed
	// lookup keeps the last good one — a slightly stale price beats a
	// blank pricing page.
	valid bool
	// lastTry is when the last lookup finished, successful or not, and
	// failed says which. A failure is remembered so an unreachable
	// Stripe is retried on a timer rather than once per page load.
	lastTry time.Time
	failed  bool
}

func NewPublicPlansHandler(s *store.Store, log *slog.Logger) *PublicPlansHandler {
	if log == nil {
		log = slog.Default()
	}
	return &PublicPlansHandler{
		Store:        s,
		Log:          log,
		cache:        make(map[string]cachedPrice),
		refreshing:   make(map[string]bool),
		cacheTTL:     5 * time.Minute,
		retryTTL:     30 * time.Second,
		stripeWait:   3 * time.Second,
		stripeBudget: 5 * time.Second,
	}
}

func (h *PublicPlansHandler) ListPlans(c *gin.Context) {
	slug := strings.ToLower(strings.TrimSpace(c.Param("product_slug")))
	if slug == "" {
		response.BadRequest(c, "product_slug is required")
		return
	}

	prod, err := h.Store.FindProductBySlug(c, slug)
	if err != nil {
		// "No such product" and "lookup blew up" answer the same, so
		// slug guesses learn nothing from the difference. The operator
		// still needs to tell them apart, and only one of the two is
		// worth a log line.
		if !errors.Is(err, sql.ErrNoRows) {
			h.Log.Error("public plans: product lookup failed", "slug", slug, "error", err)
		}
		response.NotFound(c, "product not found")
		return
	}

	plans, _, err := h.Store.ListPlans(c, prod.ID, "", store.All)
	if err != nil {
		response.Internal(c, err)
		return
	}

	priceCtx, cancelPrices := context.WithTimeout(c, h.stripeBudget)
	defer cancelPrices()

	// Same field selection as /portal/plans: no Stripe secrets, no
	// internal limits like max_activations, just what a pricing page
	// renders plus the checkout_id that starts a purchase.
	active := []gin.H{}
	for _, p := range plans {
		if !p.Active {
			continue
		}
		out := gin.H{
			"id": p.ID, "name": p.Name, "slug": p.Slug,
			"license_type": p.LicenseType, "billing_interval": p.BillingInterval,
			"checkout_id": p.CheckoutID,
			"price":       nil,
			"currency":    nil,
		}
		if p.StripePriceID != "" {
			if price, ok := h.price(priceCtx, p.StripePriceID); ok {
				out["price"] = price.amount
				out["currency"] = price.currency
			}
			// A failed lookup leaves both null instead of failing the
			// listing: the plan is still selectable and checkout reads
			// the price itself, so it stays the source of truth for
			// what actually gets charged.
		}
		active = append(active, out)
	}

	response.OK(c, gin.H{"plans": response.Array(active)})
}

// price returns the Stripe unit amount for priceID, refreshing once the
// cached copy is older than cacheTTL. A stale entry beats no entry when
// Stripe is unreachable — a pricing page showing a slightly old number
// is better than one showing none.
func (h *PublicPlansHandler) price(ctx context.Context, priceID string) (cachedPrice, bool) {
	h.cacheMu.Lock()
	cached, hit := h.cache[priceID]
	current := hit && time.Since(cached.lastTry) < h.refreshAfter(cached)
	if current || h.refreshing[priceID] {
		h.cacheMu.Unlock()
		return cached, cached.valid
	}
	h.refreshing[priceID] = true
	h.cacheMu.Unlock()
	defer func() {
		h.cacheMu.Lock()
		delete(h.refreshing, priceID)
		h.cacheMu.Unlock()
	}()

	ctx, cancel := context.WithTimeout(ctx, h.stripeWait)
	defer cancel()

	sp, err := stripeprice.Get(priceID, &stripe.PriceParams{
		Params: stripe.Params{Context: ctx},
	})
	if err != nil {
		h.Log.Warn("public plans: stripe price lookup failed",
			"price_id", priceID, "error", err)
		// Record the attempt, keep whatever was cached before.
		cached.lastTry = time.Now()
		cached.failed = true
		h.cacheMu.Lock()
		h.cache[priceID] = cached
		h.cacheMu.Unlock()
		return cached, cached.valid
	}

	fresh := cachedPrice{
		amount:   sp.UnitAmount,
		currency: string(sp.Currency),
		valid:    true,
		lastTry:  time.Now(),
	}
	h.cacheMu.Lock()
	h.cache[priceID] = fresh
	h.cacheMu.Unlock()
	return fresh, true
}

// refreshAfter keeps a good answer for cacheTTL and a failure for the
// much shorter retryTTL, so an outage recovers quickly without every
// request paying for the retry.
func (h *PublicPlansHandler) refreshAfter(c cachedPrice) time.Duration {
	if c.failed {
		return h.retryTTL
	}
	return h.cacheTTL
}
