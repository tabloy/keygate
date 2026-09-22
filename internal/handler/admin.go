package handler

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stripe/stripe-go/v82/subscription"
	"github.com/uptrace/bun"

	"github.com/tabloy/keygate/internal/license"
	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/service"
	"github.com/tabloy/keygate/internal/store"
	"github.com/tabloy/keygate/pkg/apperr"
	"github.com/tabloy/keygate/pkg/response"
)

type AdminHandler struct {
	Store   *store.Store
	Webhook *service.WebhookService
	Email   *service.EmailService
	Expiry  *service.ExpiryChecker
	Metered *service.MeteredBillingSyncer
	// FeedURLTTL is how long a presigned artifact URL inside a public
	// update feed stays usable (STORAGE_FEED_URL_TTL). Gating a
	// product's feeds does not reach links already handed out, so an
	// update period may only be configured once they have expired —
	// this is what that wait is measured against. Lower it and the
	// wait shortens, but only for links signed from then on: after
	// lowering it, leave the maintenance features off until the
	// longer-lived links are gone.
	FeedURLTTL time.Duration
	// SubscriptionEnded reports whether Stripe is finished with a
	// subscription — cancelled, expired, or gone from Stripe
	// altogether. Wired in main from the payment package so this
	// handler keeps its distance from the Stripe SDK; nil on installs
	// without Stripe, where there is nothing to confirm against.
	SubscriptionEnded func(ctx context.Context, subscriptionID string) (bool, error)
	// beforeCutoffWrite runs between reading a license (and the plan
	// or product it points at) and the transaction that writes it.
	// Tests use it to commit a change in that window; nil everywhere
	// else.
	beforeCutoffWrite func()
}

// feedNotGated refuses a maintenance change on a product
// whose update feeds are still public: the feed would hand a lapsed
// customer the newer releases, so the period would restrict only
// /license/download. Products that ship no releases have no feed to
// gate. The reverse — turning gating off while such plans exist — is
// refused in UpdateProduct.
func (h *AdminHandler) feedNotGated(c *gin.Context, prod *model.Product) bool {
	problem, err := h.feedGateCheck(c, h.Store.DB, prod)
	return h.writeFeedGateProblem(c, problem, err)
}

// feedGateProblem is a refusal a maintenance write earns from the
// product's feed gating state. It travels as an error so the check
// can run inside the transaction that holds the gating lock.
type feedGateProblem struct {
	code    string
	message string
	details gin.H
}

func (p *feedGateProblem) Error() string { return p.message }

// writeFeedGateProblem answers a refusal (409) or a failed check
// (500) and reports whether it wrote anything.
func (h *AdminHandler) writeFeedGateProblem(c *gin.Context, problem *feedGateProblem, err error) bool {
	if err != nil {
		response.Internal(c)
		return true
	}
	if problem == nil {
		return false
	}
	response.Conflict(c, problem.code, problem.message, problem.details)
	return true
}

// feedGateCheck reports why a plan or license with an update period
// may not be written for this product, or nil when it may.
func (h *AdminHandler) feedGateCheck(ctx context.Context, db bun.IDB, prod *model.Product) (*feedGateProblem, error) {
	if !model.ProductSupports(prod.Type, model.CapReleases) {
		return nil, nil
	}
	if !prod.FeedLicenseRequired {
		return &feedGateProblem{"FEED_NOT_GATED",
			"a plan with an update period or renewals needs the product's update feeds to require the license key (feed_license_required); the public feed would otherwise serve releases past the period",
			gin.H{"product_id": prod.ID}}, nil
	}
	return h.feedDrainCheck(ctx, db, prod)
}

// lockFeedGateIn takes the product's feed gating lock for this
// transaction and re-reads the gating state under it, answering why
// the write must be refused or nil. Checking outside the lock is not enough: an ungate and a
// re-gate committing in between restart the drain, and the trigger
// that guards the write sees only that the gate is on, so a period
// would be set while links from the public interval are still valid.
// Callers lock the row they are about to write first (see
// store.LockLicenseIn and friends): the triggers take this lock while
// the row they fire for is already locked, so reaching for it in the
// other order would deadlock against them.
func (h *AdminHandler) lockFeedGateIn(ctx context.Context, tx bun.Tx, productID string) (*feedGateProblem, error) {
	if err := store.FeedGatingLockIn(ctx, tx, productID); err != nil {
		return nil, err
	}
	prod, err := store.FindProductByIDIn(ctx, tx, productID)
	if err != nil {
		return nil, err
	}
	return h.feedGateCheck(ctx, tx, prod)
}

// createPlan and updatePlan write the plan, under the product's feed
// gating lock when the plan sells an update period or renewals: the
// gating state read before the write must still hold when it lands.
func (h *AdminHandler) createPlan(c *gin.Context, p *model.Plan, needsGatedFeed bool) error {
	if !needsGatedFeed {
		return h.Store.CreatePlan(c, p)
	}
	h.Store.FillPlanIDs(p)
	return h.Store.RunInTx(c, func(ctx context.Context, tx bun.Tx) error {
		if problem, err := h.maintenanceSwitchOff(ctx, tx); err != nil || problem != nil {
			return firstNonNil(err, problem)
		}
		// The row this insert references first, the gate after.
		if err := store.LockReferencedRowsIn(ctx, tx, "", p.ProductID); err != nil {
			return err
		}
		problem, err := h.lockFeedGateIn(ctx, tx, p.ProductID)
		if err != nil {
			return err
		}
		if problem != nil {
			return problem
		}
		return store.CreatePlanIn(ctx, tx, p)
	})
}

// invalidTermsError: the maintenance fields the request asks for do
// not hold together once merged with the plan under its lock.
type invalidTermsError struct{ msg string }

func (e *invalidTermsError) Error() string { return e.msg }

// planTermsRequest is what a plan update asked of the maintenance
// fields: nil means "not in this request", so the value under the
// plan's lock is kept rather than the snapshot this handler read.
type planTermsRequest struct {
	UpdatesDays          *int
	RenewalDays          *int
	StripeRenewalPriceID *string
	LicenseType          *string
}

// carried reports whether the request touched the maintenance fields
// at all — directly, or through a licence type that renormalises them.
func (t planTermsRequest) carried() bool {
	return t.UpdatesDays != nil || t.RenewalDays != nil || t.StripeRenewalPriceID != nil || t.LicenseType != nil
}

// mergeTermsIn rebuilds the maintenance fields from the plan as it
// reads under the lock plus what this request asked for. Two requests
// changing different fields would otherwise write each other's back:
// each holds a snapshot taken before the lock.
func mergeTermsIn(ctx context.Context, tx bun.Tx, p *model.Plan, terms planTermsRequest) (locked *model.Plan, changed bool, err error) {
	locked, err = store.FindPlanByIDIn(ctx, tx, p.ID)
	if err != nil {
		return nil, false, err
	}
	licenseType := locked.LicenseType
	if terms.LicenseType != nil {
		licenseType = *terms.LicenseType
	}
	in := maintenanceFields{
		UpdatesDays:          locked.UpdatesDays,
		RenewalDays:          locked.RenewalDays,
		StripeRenewalPriceID: locked.StripeRenewalPriceID,
	}
	if terms.UpdatesDays != nil {
		in.UpdatesDays = *terms.UpdatesDays
	}
	if terms.RenewalDays != nil {
		in.RenewalDays = *terms.RenewalDays
	}
	if terms.StripeRenewalPriceID != nil {
		in.StripeRenewalPriceID = *terms.StripeRenewalPriceID
	}
	merged, err := normalizeMaintenance(licenseType, in)
	if err != nil {
		// The merge is what the request really asks for, so this is a
		// validation failure like any other — answered 400, not 409.
		return locked, false, &invalidTermsError{err.Error()}
	}
	changed = merged.UpdatesDays != locked.UpdatesDays ||
		merged.RenewalDays != locked.RenewalDays ||
		merged.StripeRenewalPriceID != locked.StripeRenewalPriceID
	p.UpdatesDays, p.RenewalDays, p.StripeRenewalPriceID = merged.UpdatesDays, merged.RenewalDays, merged.StripeRenewalPriceID
	return locked, changed, nil
}

func (h *AdminHandler) updatePlan(c *gin.Context, p *model.Plan, needsGatedFeed, typeChanged bool, terms planTermsRequest, cols []string) error {
	if len(cols) == 0 {
		return nil
	}
	if !needsGatedFeed && !typeChanged && !terms.carried() {
		return h.Store.UpdatePlan(c, p, cols...)
	}
	return h.Store.RunInTx(c, func(ctx context.Context, tx bun.Tx) error {
		// The row lock is what a license insert's foreign key waits
		// on, so a fulfilment either got its license in before this
		// (and the count below refuses the change) or blocks until
		// the new type is committed and builds from that. Rows in the
		// same order everywhere — plan, then product — and the gate
		// after both.
		if err := store.LockPlanIn(ctx, tx, p.ID); err != nil {
			return err
		}
		if err := store.LockReferencedRowsIn(ctx, tx, "", p.ProductID); err != nil {
			return err
		}
		// The maintenance fields are decided here, on the plan as it
		// reads under the lock: what this request asked for over what
		// is there now, not over the snapshot it started from.
		var locked *model.Plan
		termsMoved := false
		if terms.carried() {
			var err error
			if locked, termsMoved, err = mergeTermsIn(ctx, tx, p, terms); err != nil {
				return err
			}
		}
		// Whether this is a type change is decided against the plan
		// under the lock, not the snapshot: a request that reads
		// "perpetual" and writes "perpetual" is a change if another
		// one turned it into a subscription in between — and the
		// licences that appeared under that type would be left with
		// the wrong shape.
		if terms.LicenseType != nil && locked != nil && *terms.LicenseType != locked.LicenseType {
			n, err := store.PlanLicenseCountIn(ctx, tx, p.ID)
			if err != nil {
				return err
			}
			if n > 0 {
				return store.ErrPlanHasLicenses
			}
		}
		// The switch is what stops new terms being sold, so it is
		// consulted when this write actually moves them — repeating
		// the values the plan already has is not a sale.
		sellsTerms := p.UpdatesDays > 0 || p.RenewalDays > 0 || p.StripeRenewalPriceID != ""
		if termsMoved && sellsTerms {
			if problem, err := h.maintenanceSwitchOff(ctx, tx); err != nil || problem != nil {
				return firstNonNil(err, problem)
			}
		}
		if p.UpdatesDays > 0 || p.StripeRenewalPriceID != "" {
			problem, err := h.lockFeedGateIn(ctx, tx, p.ProductID)
			if err != nil {
				return err
			}
			if problem != nil {
				return problem
			}
		}
		return store.UpdatePlanIn(ctx, tx, p, cols...)
	})
}

// changesWith adds a field to an audit entry when it has something to
// say, so the common case stays the same shape it always was.
func changesWith(changes map[string]any, key, value string) map[string]any {
	if value != "" {
		changes[key] = value
	}
	return changes
}

// errStripeBilledPlanChange: the licence is on a live Stripe
// subscription, and this endpoint does not move subscriptions.
var errStripeBilledPlanChange = errors.New("plan changes for a Stripe-billed licence belong in Stripe")

// firstNonNil returns err when it is set, otherwise the problem (as
// an error) — the shape every "check, then write" step inside a
// transaction returns.
func firstNonNil(err error, problem *feedGateProblem) error {
	if err != nil {
		return err
	}
	if problem != nil {
		return problem
	}
	return nil
}

// maintenanceSwitchOff reports the rollout confirmation as a refusal
// the caller can return from inside its transaction: the check before
// the write is cheap and gives the usual message, but the switch can
// go off between the two.
func (h *AdminHandler) maintenanceSwitchOff(ctx context.Context, tx bun.IDB) (*feedGateProblem, error) {
	on, err := store.MaintenanceFeaturesEnabledForWriteIn(ctx, tx)
	if err != nil || on {
		return nil, err
	}
	return &feedGateProblem{"MAINTENANCE_FEATURES_DISABLED",
		"maintenance-period features are switched off: finish rolling every replica to this version, then enable them in Settings (" + store.SettingMaintenanceFeatures + ")", nil}, nil
}

// feedGateRefused answers a refusal that came back from withFeedGate
// and reports whether it wrote the response.
func feedGateRefused(c *gin.Context, err error) bool {
	var problem *feedGateProblem
	if errors.As(err, &problem) {
		response.Conflict(c, problem.code, problem.message, problem.details)
		return true
	}
	return false
}

// feedDrainPending reports whether what the product's public feeds
// already handed out could still be used. The gate does not reach
// those: a shared cache may serve the feed for its max-age, and every
// copy of it carries presigned artifact URLs that stay valid for
// their own lifetime. Until the longer of the two has passed since
// the gate went up, a customer could fetch a release a cutoff
// excludes. A product that never published a release handed out
// nothing and needs no wait. Writes the response when it refuses.
func (h *AdminHandler) feedDrainPending(c *gin.Context, prod *model.Product) bool {
	problem, err := h.feedDrainCheck(c, h.Store.DB, prod)
	return h.writeFeedGateProblem(c, problem, err)
}

func (h *AdminHandler) feedDrainCheck(ctx context.Context, db bun.IDB, prod *model.Product) (*feedGateProblem, error) {
	// The link lifetime comes from the shared bound, so a replica
	// started with a shorter TTL than one of its peers cannot let the
	// wait end early; this replica's own configuration is only the
	// fallback.
	drain, left, err := store.FeedDrainLeftIn(ctx, db, prod, h.FeedURLTTL)
	if err != nil {
		return nil, err
	}
	if left <= 0 {
		return nil, nil
	}
	return &feedGateProblem{"FEED_CACHE_DRAINING",
		fmt.Sprintf("the update feeds were gated %s ago; download links handed out by the public feed stay valid for %s, so an update period can be set in %s (lower STORAGE_FEED_URL_TTL to reduce this)",
			time.Since(*prod.FeedGatedAt).Round(time.Second), drain, left.Round(time.Second)),
		gin.H{"product_id": prod.ID, "retry_after_seconds": int(left.Seconds()) + 1}}, nil
}

// maintenanceGated answers 409 while the switch is off. A settings
// read failure is an error, not an open gate.
func (h *AdminHandler) maintenanceGated(c *gin.Context) bool {
	on, err := h.Store.MaintenanceFeaturesEnabled(c)
	if err != nil {
		response.Internal(c)
		return true
	}
	if on {
		return false
	}
	response.Conflict(c, "MAINTENANCE_FEATURES_DISABLED",
		"maintenance-period features are switched off: finish rolling every replica to this version, then enable them in Settings ("+store.SettingMaintenanceFeatures+")", nil)
	return true
}

func NewAdminHandler(s *store.Store, wh *service.WebhookService, em *service.EmailService, ex *service.ExpiryChecker, ms *service.MeteredBillingSyncer) *AdminHandler {
	return &AdminHandler{Store: s, Webhook: wh, Email: em, Expiry: ex, Metered: ms}
}

// ─── Stats ───

func (h *AdminHandler) Stats(c *gin.Context) {
	stats, err := h.Store.GetStats(c)
	if err != nil {
		response.Internal(c)
		return
	}
	response.OK(c, stats)
}

// ─── Products ───

func (h *AdminHandler) ListProducts(c *gin.Context) {
	// type=desktop,hybrid narrows the catalogue to the kinds the
	// caller can use. An unknown kind is refused rather than ignored:
	// a typo that silently returned everything would have the
	// dashboard offer products its own page cannot accept.
	var types []string
	if v := strings.TrimSpace(c.Query("type")); v != "" {
		for _, t := range strings.Split(v, ",") {
			t = strings.TrimSpace(t)
			if !model.IsValidProductType(t) {
				response.BadRequest(c, "unknown product type: "+t)
				return
			}
			types = append(types, t)
		}
	}
	page := listPage(c)
	products, total, err := h.Store.ListProducts(c, c.Query("search"), types, page)
	if err != nil {
		response.Internal(c)
		return
	}
	listOK(c, "products", products, total, page)
}

func (h *AdminHandler) GetProduct(c *gin.Context) {
	p, err := h.Store.FindProductByID(c, c.Param("id"))
	if err != nil {
		response.NotFound(c, "product not found")
		return
	}
	response.OK(c, p)
}

func (h *AdminHandler) CreateProduct(c *gin.Context) {
	var req struct {
		Name string `json:"name" binding:"required"`
		Slug string `json:"slug" binding:"required"`
		Type string `json:"type" binding:"required"`
		// FeedLicenseRequired at creation is the easy moment to gate:
		// a product that has published nothing handed out no feed, so
		// the gate takes effect at once and no drain has to be waited
		// out. Doing it later works too, and then it does.
		FeedLicenseRequired bool `json:"feed_license_required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "name, slug, and type are required")
		return
	}
	if req.Type != "desktop" && req.Type != "saas" && req.Type != "hybrid" {
		response.BadRequest(c, "type must be desktop, saas, or hybrid")
		return
	}
	if err := apperr.ValidateName("name", req.Name); err != nil {
		response.BadRequest(c, err.Message)
		return
	}
	if err := apperr.ValidateSlug(req.Slug); err != nil {
		response.BadRequest(c, err.Message)
		return
	}

	// Same fence as switching the gate on later: while the maintenance
	// features are off, a replica that predates them may still be
	// serving, and it would answer the feed without a licence.
	if req.FeedLicenseRequired && h.maintenanceGated(c) {
		return
	}

	p := &model.Product{Name: req.Name, Slug: req.Slug, Type: req.Type, FeedLicenseRequired: req.FeedLicenseRequired}
	if err := h.Store.CreateProduct(c, p); err != nil {
		response.Err(c, http.StatusConflict, "DUPLICATE", "product slug already exists")
		return
	}

	h.Store.Audit(c, &model.AuditLog{
		Entity: "product", EntityID: p.ID, Action: "created",
		ActorType: "admin", ActorID: adminID(c),
		Changes: map[string]any{"name": req.Name, "slug": req.Slug, "type": req.Type,
			"feed_license_required": req.FeedLicenseRequired},
	})

	response.Created(c, p)
}

func (h *AdminHandler) UpdateProduct(c *gin.Context) {
	p, err := h.Store.FindProductByID(c, c.Param("id"))
	if err != nil {
		response.NotFound(c, "product not found")
		return
	}

	var req struct {
		Name                    string  `json:"name"`
		Slug                    string  `json:"slug"`
		Type                    string  `json:"type"`
		MinimumSupportedVersion *string `json:"minimum_supported_version"`
		MinimumSupportedMessage *string `json:"minimum_supported_message"`
		RequireSigning          *bool   `json:"require_signing"`
		FeedLicenseRequired     *bool   `json:"feed_license_required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "invalid request body")
		return
	}
	// Only the columns this request carries are written. A whole-row
	// write would put back everything the snapshot above holds, and a
	// concurrent change — an ungate and re-gate in particular, whose
	// timestamp decides how long the feeds must drain — would be
	// silently undone by an unrelated rename.
	cols := []string{}
	if req.Name != "" {
		if err := apperr.ValidateName("name", req.Name); err != nil {
			response.BadRequest(c, err.Message)
			return
		}
		p.Name = req.Name
		cols = append(cols, "name")
	}
	if req.Slug != "" {
		if err := apperr.ValidateSlug(req.Slug); err != nil {
			response.BadRequest(c, err.Message)
			return
		}
		p.Slug = req.Slug
		cols = append(cols, "slug")
	}
	prevType := p.Type
	if req.Type != "" {
		if req.Type != "desktop" && req.Type != "saas" && req.Type != "hybrid" {
			response.BadRequest(c, "type must be desktop, saas, or hybrid")
			return
		}
		// Only validate against existing plans when the type is
		// actually changing. Re-saving the same type is a no-op.
		if req.Type != p.Type {
			n, err := h.Store.CountPlansIncompatibleWithType(c, p.ID, req.Type)
			if err != nil {
				response.Internal(c)
				return
			}
			if n > 0 {
				response.Err(c, http.StatusConflict, "INCOMPATIBLE_PLANS",
					fmt.Sprintf("%d existing plan(s) use fields that are not valid for %s products; reset max_activations / max_seats / license_model on those plans first",
						n, req.Type))
				return
			}
		}
		if !model.ProductSupports(req.Type, model.CapReleases) && req.Type != p.Type {
			// Published releases cannot be deleted, so "no releases at
			// all" would make the change impossible for any product
			// that ever shipped. Yanked releases are withdrawn already;
			// only live (published) and draft ones block.
			total, err := h.Store.CountReleases(c, store.ReleaseFilter{ProductID: p.ID})
			if err != nil {
				response.Internal(c)
				return
			}
			yanked, err := h.Store.CountReleases(c, store.ReleaseFilter{ProductID: p.ID, Status: model.ReleaseStatusYanked})
			if err != nil {
				response.Internal(c)
				return
			}
			if n := total - yanked; n > 0 {
				response.Err(c, http.StatusBadRequest, "INCOMPATIBLE_PRODUCT_TYPE",
					fmt.Sprintf("cannot change type to %s: %d release(s) would become unreachable — delete draft releases and yank published ones first", req.Type, n))
				return
			}
		}
		p.Type = req.Type
		cols = append(cols, "type")
	}
	if req.MinimumSupportedVersion != nil {
		v := strings.TrimSpace(*req.MinimumSupportedVersion)
		if v != "" {
			if err := apperr.ValidateSemver(v); err != nil {
				response.BadRequest(c, err.Message)
				return
			}
		}
		p.MinimumSupportedVersion = v
		cols = append(cols, "minimum_supported_version")
	}
	if req.MinimumSupportedMessage != nil {
		msg := strings.TrimSpace(*req.MinimumSupportedMessage)
		if len(msg) > 1024 {
			response.BadRequest(c, "minimum_supported_message must be ≤ 1024 chars")
			return
		}
		p.MinimumSupportedMessage = msg
		cols = append(cols, "minimum_supported_message")
	}
	if req.RequireSigning != nil {
		p.RequireSigning = *req.RequireSigning
		cols = append(cols, "require_signing")
	}
	wasGated, wasReleases := p.FeedLicenseRequired, model.ProductSupports(prevType, model.CapReleases)
	// gatingNow marks a request that switches the gate on. The instant
	// it went up is written by the database when the row is written,
	// not read from this replica's clock before the transaction: the
	// public feed keeps serving until the write commits, and replicas
	// need not agree on the time.
	gatingNow := false
	if req.FeedLicenseRequired != nil {
		if *req.FeedLicenseRequired && !p.FeedLicenseRequired {
			if h.maintenanceGated(c) {
				return
			}
			gatingNow = true
			now := time.Now()
			p.FeedGatedAt = &now
		}
		p.FeedLicenseRequired = *req.FeedLicenseRequired
		cols = append(cols, "feed_license_required")
	}
	// The product must not end up release-capable with public feeds
	// while anything the feed gate protects exists — whether the gate
	// was switched off, or a saas product (no feed, so no gate needed)
	// became a desktop or hybrid one. The database trigger holds the
	// same rule for concurrent writes.
	if model.ProductSupports(p.Type, model.CapReleases) && !p.FeedLicenseRequired && (wasGated || !wasReleases) {
		has, err := h.Store.ProductHasMaintenance(c, p.ID)
		if err != nil {
			response.Internal(c)
			return
		}
		if has && !wasReleases {
			response.Conflict(c, "FEED_NOT_GATED",
				"this product has plans or licenses with an update period; enable feed_license_required in the same change so its new update feeds require the license key", nil)
			return
		}
		if has {
			response.Conflict(c, "BOUNDED_PLANS_EXIST",
				"update feeds must keep requiring the license key while a plan of this product sells an update period or renewals, or a license still has an update cutoff", nil)
			return
		}
	}
	// Giving a product its feeds back is the same exposure as gating
	// them: links handed out while it last served public feeds keep
	// working. A saas product serves no feed, so periods may be set
	// on it freely; turning it back into a desktop or hybrid one must
	// therefore wait out those links, not just switch the gate on.
	// Whether anything the feed gate protects exists is decided under
	// the product's row lock inside the write: a bounded plan or a
	// cutoff may be created on a saas product at any moment — it
	// serves no feed — and one created after a check out here would
	// otherwise get its feeds back without the wait.
	restoringFeeds := !wasReleases && model.ProductSupports(p.Type, model.CapReleases) && p.FeedLicenseRequired
	if h.beforeCutoffWrite != nil {
		h.beforeCutoffWrite()
	}

	// The write happens under the gating lock with the drain checked
	// there: the gate could be switched off and on again between a
	// check and the write, which restarts it.
	//
	// Set when the gate was saved but the type change has to wait for
	// the drain: that half of the request commits, so it travels
	// beside the transaction rather than as its error.
	var waiting *feedGateProblem
	err = h.Store.RunInTx(c, func(ctx context.Context, tx bun.Tx) error {
		// Row first, then the gate: the order the triggers use.
		fresh, err := store.LockProductIn(ctx, tx, p.ID)
		if err != nil {
			return err
		}
		if err := store.FeedGatingLockIn(ctx, tx, p.ID); err != nil {
			return err
		}
		rest := cols
		if gatingNow {
			// The gate goes up in its own statement so the instant it
			// went up is the database's, and so it survives even when
			// the rest of the request is refused below.
			if err := store.UpdateProductGatingNowIn(ctx, tx, p, "feed_license_required"); err != nil {
				return err
			}
			rest = slices.DeleteFunc(slices.Clone(cols), func(c string) bool { return c == "feed_license_required" })
		} else {
			// Never write back the instant this request read: another
			// one may have re-gated since, and the wait runs from that.
			p.FeedGatedAt = fresh.FeedGatedAt
		}
		if restoringFeeds {
			has, err := store.ProductHasMaintenanceIn(ctx, tx, p.ID)
			if err != nil {
				return err
			}
			if !has {
				return store.UpdateProductIn(ctx, tx, p, rest...)
			}
			problem, err := h.feedDrainCheck(ctx, tx, p)
			if err != nil {
				return err
			}
			if problem != nil {
				// The type change waits. Only the gate was written —
				// the rest of the request is refused with it, so a
				// caller reading the 409 as "nothing happened" is
				// wrong about one thing only, and the message says
				// which. Leaving the gate in memory alone would
				// restart the wait on every retry and the product
				// could never get its feeds back.
				problem.message = "the update feeds now require the license key and only that was saved; the type change and any other field in this request were not applied: " + problem.message
				waiting = problem
				return nil
			}
		}
		if len(rest) == 0 {
			return nil
		}
		return store.UpdateProductIn(ctx, tx, p, rest...)
	})
	if err != nil {
		if feedGateRefused(c, err) {
			return
		}
		if feedGateConflict(c, err) {
			return
		}
		response.Internal(c)
		return
	}
	if waiting != nil {
		response.Conflict(c, waiting.code, waiting.message, waiting.details)
		return
	}
	response.OK(c, p)
}

func (h *AdminHandler) DeleteProduct(c *gin.Context) {
	id := c.Param("id")
	// Plans, licences and releases each hold the product by a
	// foreign key the database refuses to break. Counting them here
	// is what turns "an internal error occurred" — with the reason
	// only in Postgres's log — into a sentence saying what to clear
	// first. Licences are named first: they are the one thing an
	// admin cannot simply delete.
	plans, licenses, releases, err := h.Store.ProductBlockers(c, id)
	if err != nil {
		response.Internal(c)
		return
	}
	switch {
	case licenses > 0:
		response.Err(c, http.StatusConflict, "HAS_LICENSES", "cannot delete product with existing licenses")
		return
	case releases > 0:
		response.Err(c, http.StatusConflict, "HAS_RELEASES",
			"cannot delete a product that still has releases; delete them first")
		return
	case plans > 0:
		response.Err(c, http.StatusConflict, "HAS_PLANS",
			"cannot delete a product that still has plans; delete them first")
		return
	}
	if err := h.Store.DeleteProduct(c, id); err != nil {
		response.Internal(c)
		return
	}
	h.Store.Audit(c, &model.AuditLog{
		Entity: "product", EntityID: id, Action: "deleted",
		ActorType: "admin", ActorID: adminID(c),
	})
	response.NoContent(c)
}

// ─── Plans ───

func (h *AdminHandler) ListPlans(c *gin.Context) {
	page := listPage(c)
	plans, total, err := h.Store.ListPlans(c, c.Query("product_id"), c.Query("search"), page)
	if err != nil {
		response.Internal(c)
		return
	}
	listOK(c, "plans", plans, total, page)
}

func (h *AdminHandler) GetPlan(c *gin.Context) {
	p, err := h.Store.FindPlanByID(c, c.Param("id"))
	if err != nil {
		response.NotFound(c, "plan not found")
		return
	}
	response.OK(c, p)
}

// normalizeBillingInterval validates a plan's billing interval against
// its license type. The interval is descriptive (checkout mode comes
// from the Stripe price), so a perpetual plan carries none: it is paid
// once and never renews, and showing "monthly" on it would promise a
// renewal that never happens.
func normalizeBillingInterval(licenseType, interval string) (string, error) {
	switch interval {
	case "", "month", "year":
	default:
		return "", errors.New("billing_interval must be month, year, or empty")
	}
	if licenseType == "perpetual" {
		return "", nil
	}
	return interval, nil
}

// maintenanceFields are the plan fields that describe a perpetual
// license's update period and how more of it is sold.
type maintenanceFields struct {
	UpdatesDays          int
	RenewalDays          int
	StripeRenewalPriceID string
}

// validateStripePriceID refuses anything that is not a Stripe price
// id. A product id or a payment-link id is accepted by nothing
// downstream: Stripe rejects the checkout, and the customer is the
// one who meets the failure, on the plan's buy button.
func validateStripePriceID(field, id string) error {
	id = strings.TrimSpace(id)
	if id == "" || strings.HasPrefix(id, "price_") {
		return nil
	}
	return errors.New(field + " must be a Stripe price id (price_...)")
}

// normalizeMaintenance validates the maintenance fields against the
// license type. Only perpetual licenses have a separate update
// period; on every other type the fields are cleared, mirroring the
// billing interval. A renewal price without a renewal length would
// sell nothing, so the pair is required together.
func normalizeMaintenance(licenseType string, m maintenanceFields) (maintenanceFields, error) {
	if licenseType != "perpetual" {
		return maintenanceFields{}, nil
	}
	m.StripeRenewalPriceID = strings.TrimSpace(m.StripeRenewalPriceID)
	switch {
	case m.UpdatesDays < 0:
		return m, errors.New("updates_days cannot be negative")
	case m.UpdatesDays > 3650:
		return m, errors.New("updates_days cannot exceed 3650")
	case m.RenewalDays < 0:
		return m, errors.New("renewal_days cannot be negative")
	case m.RenewalDays > 3650:
		return m, errors.New("renewal_days cannot exceed 3650")
	case m.StripeRenewalPriceID != "" && m.RenewalDays == 0:
		return m, errors.New("renewal_days is required when stripe_renewal_price_id is set")
	case m.StripeRenewalPriceID != "" && !strings.HasPrefix(m.StripeRenewalPriceID, "price_"):
		return m, errors.New("stripe_renewal_price_id must be a Stripe price id (price_...)")
	}
	return m, nil
}

func (h *AdminHandler) CreatePlan(c *gin.Context) {
	var req struct {
		ProductID       string `json:"product_id" binding:"required"`
		Name            string `json:"name" binding:"required"`
		Slug            string `json:"slug" binding:"required"`
		LicenseType     string `json:"license_type" binding:"required"`
		BillingInterval string `json:"billing_interval"`
		MaxActivations  int    `json:"max_activations"`
		MaxSeats        int    `json:"max_seats"`
		TrialDays       int    `json:"trial_days"`
		// Pointers so "omitted" (use the default) and an explicit 0
		// ("no grace", "no timeout") stay distinguishable. Before, a
		// zero-grace plan could only be made with a second PUT.
		GraceDays            *int   `json:"grace_days"`
		StripePriceID        string `json:"stripe_price_id"`
		UpdatesDays          int    `json:"updates_days"`
		RenewalDays          int    `json:"renewal_days"`
		StripeRenewalPriceID string `json:"stripe_renewal_price_id"`
		LicenseModel         string `json:"license_model"`
		FloatingTimeout      *int   `json:"floating_timeout"`
		TokenTTLDays         int    `json:"token_ttl_days"`
		SortOrder            int    `json:"sort_order"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "product_id, name, slug, and license_type are required")
		return
	}

	if err := apperr.ValidateName("name", req.Name); err != nil {
		response.BadRequest(c, err.Message)
		return
	}
	if err := apperr.ValidateSlug(req.Slug); err != nil {
		response.BadRequest(c, err.Message)
		return
	}

	switch req.LicenseType {
	case "subscription", "perpetual", "trial":
	default:
		response.BadRequest(c, "license_type must be subscription, perpetual, or trial")
		return
	}
	billingInterval, err := normalizeBillingInterval(req.LicenseType, req.BillingInterval)
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	maint, err := normalizeMaintenance(req.LicenseType, maintenanceFields{
		UpdatesDays: req.UpdatesDays, RenewalDays: req.RenewalDays, StripeRenewalPriceID: req.StripeRenewalPriceID,
	})
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}

	graceDays := 7
	if req.GraceDays != nil {
		graceDays = *req.GraceDays
	}
	// grace_days keeps an explicit 0 (no grace); floating_timeout does
	// not, because a zero session timeout is meaningless and the
	// runtime substitutes 30 anyway — storing 0 would just make the
	// plan page disagree with behaviour.
	floatingTimeout := 30
	if req.FloatingTimeout != nil {
		if *req.FloatingTimeout < 0 {
			response.BadRequest(c, "floating_timeout cannot be negative")
			return
		}
		if *req.FloatingTimeout > 0 {
			floatingTimeout = *req.FloatingTimeout
		}
	}
	if err := validatePlanNumericBounds(planBounds{
		MaxActivations:  req.MaxActivations,
		MaxSeats:        req.MaxSeats,
		TrialDays:       req.TrialDays,
		GraceDays:       graceDays,
		FloatingTimeout: floatingTimeout,
		TokenTTLDays:    req.TokenTTLDays,
	}); err != nil {
		response.BadRequest(c, err.Error())
		return
	}

	licenseModel := req.LicenseModel
	if licenseModel == "" {
		licenseModel = "standard"
	}
	if licenseModel != "standard" && licenseModel != "floating" {
		response.BadRequest(c, "license_model must be standard or floating")
		return
	}
	// Product-type capability gating. The product decides WHAT a plan
	// can configure; price model (perpetual/subscription/trial) stays
	// orthogonal. Example: a saas product's plan must not set
	// max_activations (no per-device licensing). A desktop product's
	// plan must not set max_seats. Hybrid allows both.
	prod, err := h.Store.FindProductByID(c, req.ProductID)
	if err != nil {
		response.NotFound(c, "product not found")
		return
	}
	if req.MaxActivations > 0 && !model.ProductSupports(prod.Type, model.CapActivations) {
		response.Err(c, http.StatusBadRequest, "INCOMPATIBLE_PRODUCT_TYPE",
			"max_activations is not supported for "+prod.Type+" products (use a hybrid or desktop product)")
		return
	}
	if req.MaxSeats > 0 && !model.ProductSupports(prod.Type, model.CapSeats) {
		response.Err(c, http.StatusBadRequest, "INCOMPATIBLE_PRODUCT_TYPE",
			"max_seats is not supported for "+prod.Type+" products (use a hybrid or saas product)")
		return
	}
	// Default MaxActivations only when the product supports activations.
	// Otherwise leave it 0 (the column accepts zero and the runtime
	// guards prevent activation-family endpoints from being called).
	if req.MaxActivations <= 0 && model.ProductSupports(prod.Type, model.CapActivations) {
		req.MaxActivations = 3
	}
	if licenseModel == "floating" && !model.ProductSupports(prod.Type, model.CapActivations) {
		response.Err(c, http.StatusBadRequest, "INCOMPATIBLE_PRODUCT_TYPE",
			"floating license_model is not supported for "+prod.Type+" products")
		return
	}

	req.StripePriceID = strings.TrimSpace(req.StripePriceID)
	if err := validateStripePriceID("stripe_price_id", req.StripePriceID); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	if h.stripePricesTaken(c, req.StripePriceID, maint.StripeRenewalPriceID, "") {
		return
	}
	needsGatedFeed := maint.UpdatesDays > 0 || maint.StripeRenewalPriceID != ""
	if needsGatedFeed && h.maintenanceGated(c) {
		return
	}
	if needsGatedFeed && h.feedNotGated(c, prod) {
		return
	}

	p := &model.Plan{
		ProductID:            req.ProductID,
		Name:                 req.Name,
		Slug:                 req.Slug,
		LicenseType:          req.LicenseType,
		BillingInterval:      billingInterval,
		MaxActivations:       req.MaxActivations,
		MaxSeats:             req.MaxSeats,
		TrialDays:            req.TrialDays,
		GraceDays:            graceDays,
		StripePriceID:        req.StripePriceID,
		UpdatesDays:          maint.UpdatesDays,
		RenewalDays:          maint.RenewalDays,
		StripeRenewalPriceID: maint.StripeRenewalPriceID,
		LicenseModel:         licenseModel,
		FloatingTimeout:      floatingTimeout,
		TokenTTLDays:         req.TokenTTLDays,
		Active:               true,
		SortOrder:            req.SortOrder,
	}
	if err := h.createPlan(c, p, needsGatedFeed); err != nil {
		if feedGateRefused(c, err) {
			return
		}
		if isStripePriceConflict(err) {
			response.Err(c, http.StatusConflict, "STRIPE_PRICE_IN_USE", "stripe_price_id is already used by another plan")
			return
		}
		if feedGateConflict(c, err) {
			return
		}
		response.Err(c, http.StatusConflict, "DUPLICATE", "plan slug already exists for this product")
		return
	}

	h.Store.Audit(c, &model.AuditLog{
		Entity: "plan", EntityID: p.ID, Action: "created",
		ActorType: "admin", ActorID: adminID(c),
		Changes: map[string]any{"name": req.Name, "product_id": req.ProductID},
	})

	response.Created(c, p)
}

func (h *AdminHandler) UpdatePlan(c *gin.Context) {
	p, err := h.Store.FindPlanByID(c, c.Param("id"))
	if err != nil {
		response.NotFound(c, "plan not found")
		return
	}

	var req struct {
		Name                 *string `json:"name"`
		Slug                 *string `json:"slug"`
		LicenseType          *string `json:"license_type"`
		BillingInterval      *string `json:"billing_interval"`
		MaxActivations       *int    `json:"max_activations"`
		MaxSeats             *int    `json:"max_seats"`
		TrialDays            *int    `json:"trial_days"`
		GraceDays            *int    `json:"grace_days"`
		StripePriceID        *string `json:"stripe_price_id"`
		UpdatesDays          *int    `json:"updates_days"`
		RenewalDays          *int    `json:"renewal_days"`
		StripeRenewalPriceID *string `json:"stripe_renewal_price_id"`
		LicenseModel         *string `json:"license_model"`
		FloatingTimeout      *int    `json:"floating_timeout"`
		TokenTTLDays         *int    `json:"token_ttl_days"`
		Active               *bool   `json:"active"`
		SortOrder            *int    `json:"sort_order"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "invalid request body")
		return
	}

	// Capability gating mirrors CreatePlan: validate against the parent
	// product's type. Look up the product once.
	prod, err := h.Store.FindProductByID(c, p.ProductID)
	if err != nil {
		response.Internal(c)
		return
	}
	if req.MaxActivations != nil && *req.MaxActivations > 0 && !model.ProductSupports(prod.Type, model.CapActivations) {
		response.Err(c, http.StatusBadRequest, "INCOMPATIBLE_PRODUCT_TYPE",
			"max_activations is not supported for "+prod.Type+" products")
		return
	}
	if req.MaxSeats != nil && *req.MaxSeats > 0 && !model.ProductSupports(prod.Type, model.CapSeats) {
		response.Err(c, http.StatusBadRequest, "INCOMPATIBLE_PRODUCT_TYPE",
			"max_seats is not supported for "+prod.Type+" products")
		return
	}
	if req.LicenseModel != nil {
		if *req.LicenseModel != "standard" && *req.LicenseModel != "floating" {
			response.BadRequest(c, "license_model must be standard or floating")
			return
		}
		if *req.LicenseModel == "floating" && !model.ProductSupports(prod.Type, model.CapActivations) {
			response.Err(c, http.StatusBadRequest, "INCOMPATIBLE_PRODUCT_TYPE",
				"floating license_model is not supported for "+prod.Type+" products")
			return
		}
	}
	// Mirror CreatePlan's license_type enum guard. Without this, an
	// UPDATE could persist arbitrary strings (e.g. "free", "pro") that
	// downstream branching (trial-only logic, billing routing) would
	// silently treat as the default branch.
	typeChanged := false
	if req.LicenseType != nil {
		switch *req.LicenseType {
		case "subscription", "perpetual", "trial":
		default:
			response.BadRequest(c, "license_type must be subscription, perpetual, or trial")
			return
		}
		// Licenses take their period, billing and update rules from
		// the plan's type; changing it under them would leave every
		// existing license with the wrong state (a subscription
		// license has no update period, a perpetual one no Stripe
		// subscription). Move the licenses to another plan first.
		// The count is only worth trusting when it cannot change
		// before the write: a fulfilment holding a stale copy of the
		// plan would otherwise create the first license — with the
		// subscription row the old type implies — just after the
		// count came back zero. Re-checked under the plan's row lock
		// in the write transaction below; this one keeps the common
		// refusal cheap and its message unchanged.
		typeChanged = *req.LicenseType != p.LicenseType
		if typeChanged {
			if n, err := h.Store.PlanLicenseCount(c, p.ID); err != nil {
				response.Internal(c)
				return
			} else if n > 0 {
				response.Err(c, http.StatusConflict, "HAS_LICENSES",
					"cannot change the license type of a plan that has licenses; move them to another plan first")
				return
			}
		}
	}
	// The interval is checked against the license type the row will
	// have after this update, so switching a plan to perpetual clears
	// a stale interval even when the request doesn't mention it.
	licenseType := p.LicenseType
	if req.LicenseType != nil {
		licenseType = *req.LicenseType
	}
	billingInterval := p.BillingInterval
	if req.BillingInterval != nil {
		billingInterval = *req.BillingInterval
	}
	billingInterval, err = normalizeBillingInterval(licenseType, billingInterval)
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	maintIn := maintenanceFields{UpdatesDays: p.UpdatesDays, RenewalDays: p.RenewalDays, StripeRenewalPriceID: p.StripeRenewalPriceID}
	if req.UpdatesDays != nil {
		maintIn.UpdatesDays = *req.UpdatesDays
	}
	if req.RenewalDays != nil {
		maintIn.RenewalDays = *req.RenewalDays
	}
	if req.StripeRenewalPriceID != nil {
		maintIn.StripeRenewalPriceID = *req.StripeRenewalPriceID
	}
	maint, err := normalizeMaintenance(licenseType, maintIn)
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}

	// Numeric-range validation. Pull from the request when the
	// field was provided, otherwise the existing row value (so a
	// PATCH that doesn't touch a field doesn't accidentally reject
	// a legacy value).
	check := planBounds{
		MaxActivations: p.MaxActivations, MaxSeats: p.MaxSeats,
		TrialDays: p.TrialDays, GraceDays: p.GraceDays,
		FloatingTimeout: p.FloatingTimeout,
		TokenTTLDays:    p.TokenTTLDays,
	}
	if req.MaxActivations != nil {
		check.MaxActivations = *req.MaxActivations
	}
	if req.MaxSeats != nil {
		check.MaxSeats = *req.MaxSeats
	}
	if req.TrialDays != nil {
		check.TrialDays = *req.TrialDays
	}
	if req.GraceDays != nil {
		check.GraceDays = *req.GraceDays
	}
	if req.FloatingTimeout != nil {
		check.FloatingTimeout = *req.FloatingTimeout
	}
	if req.TokenTTLDays != nil {
		check.TokenTTLDays = *req.TokenTTLDays
	}
	if err := validatePlanNumericBounds(check); err != nil {
		response.BadRequest(c, err.Error())
		return
	}

	// Only the columns this request carries are written. A whole-row
	// write would put the snapshot read above back, so an edit of an
	// unrelated field could silently restore update terms another
	// request had just changed — or wipe ones it had just set.
	cols := []string{}
	// Validated on the way in, as the create path does: an update
	// that skipped these could put a name or a slug on a plan that
	// could never have been created with one.
	if req.Name != nil {
		if err := apperr.ValidateName("name", *req.Name); err != nil {
			response.BadRequest(c, err.Message)
			return
		}
		p.Name = *req.Name
		cols = append(cols, "name")
	}
	if req.Slug != nil {
		if err := apperr.ValidateSlug(*req.Slug); err != nil {
			response.BadRequest(c, err.Message)
			return
		}
		p.Slug = *req.Slug
		cols = append(cols, "slug")
	}
	if req.LicenseType != nil {
		p.LicenseType = *req.LicenseType
		cols = append(cols, "license_type")
	}
	// The interval follows the licence type, so it is written
	// whenever either of them is in the request.
	if req.BillingInterval != nil || req.LicenseType != nil {
		cols = append(cols, "billing_interval")
	}
	p.BillingInterval = billingInterval
	// What the plan sold before this request, for the rollout fence
	// below.
	prevDays, prevRenewalDays, prevRenewalPrice := p.UpdatesDays, p.RenewalDays, p.StripeRenewalPriceID
	p.UpdatesDays, p.RenewalDays, p.StripeRenewalPriceID = maint.UpdatesDays, maint.RenewalDays, maint.StripeRenewalPriceID
	// The three travel together: normalising them depends on the
	// licence type, so a type change rewrites them too.
	if req.UpdatesDays != nil || req.RenewalDays != nil || req.StripeRenewalPriceID != nil || req.LicenseType != nil {
		cols = append(cols, "updates_days", "renewal_days", "stripe_renewal_price_id")
	}
	if req.MaxActivations != nil {
		p.MaxActivations = *req.MaxActivations
		cols = append(cols, "max_activations")
	}
	if req.MaxSeats != nil {
		p.MaxSeats = *req.MaxSeats
		cols = append(cols, "max_seats")
	}
	if req.TrialDays != nil {
		p.TrialDays = *req.TrialDays
		cols = append(cols, "trial_days")
	}
	if req.GraceDays != nil {
		p.GraceDays = *req.GraceDays
		cols = append(cols, "grace_days")
	}
	purchasePrice := p.StripePriceID
	if req.StripePriceID != nil {
		purchasePrice = strings.TrimSpace(*req.StripePriceID)
		// Only when this request sets it: a plan that already holds
		// some legacy value can still have its other fields edited.
		if err := validateStripePriceID("stripe_price_id", purchasePrice); err != nil {
			response.BadRequest(c, err.Error())
			return
		}
	}
	if h.stripePricesTaken(c, purchasePrice, maint.StripeRenewalPriceID, p.ID) {
		return
	}
	// While the switch is off a replica that predates this version may
	// be running, and it enforces none of this: any write that leaves
	// the plan selling an update period or renewals is refused, not
	// only the one that starts them. Clearing them all is what an
	// operator does to make a rollback safe, so that stays allowed.
	termsChanged := maint.UpdatesDays != prevDays ||
		maint.RenewalDays != prevRenewalDays ||
		maint.StripeRenewalPriceID != prevRenewalPrice
	sellsTerms := maint.UpdatesDays > 0 || maint.RenewalDays > 0 || maint.StripeRenewalPriceID != ""
	if termsChanged && sellsTerms && h.maintenanceGated(c) {
		return
	}
	needsGatedFeed := maint.UpdatesDays > 0 || maint.StripeRenewalPriceID != ""
	if needsGatedFeed && h.feedNotGated(c, prod) {
		return
	}
	p.StripePriceID = purchasePrice
	if req.StripePriceID != nil {
		cols = append(cols, "stripe_price_id")
	}
	if req.LicenseModel != nil {
		p.LicenseModel = *req.LicenseModel
		cols = append(cols, "license_model")
	}
	if req.FloatingTimeout != nil {
		p.FloatingTimeout = *req.FloatingTimeout
		cols = append(cols, "floating_timeout")
	}
	if req.TokenTTLDays != nil {
		p.TokenTTLDays = *req.TokenTTLDays
		cols = append(cols, "token_ttl_days")
	}
	if req.Active != nil {
		p.Active = *req.Active
		cols = append(cols, "active")
	}
	if req.SortOrder != nil {
		p.SortOrder = *req.SortOrder
		cols = append(cols, "sort_order")
	}

	if h.beforeCutoffWrite != nil {
		h.beforeCutoffWrite()
	}
	terms := planTermsRequest{
		UpdatesDays:          req.UpdatesDays,
		RenewalDays:          req.RenewalDays,
		StripeRenewalPriceID: req.StripeRenewalPriceID,
		LicenseType:          req.LicenseType,
	}
	if err := h.updatePlan(c, p, needsGatedFeed, typeChanged, terms, cols); err != nil {
		var invalid *invalidTermsError
		if errors.As(err, &invalid) {
			response.BadRequest(c, invalid.msg)
			return
		}
		if errors.Is(err, store.ErrPlanHasLicenses) {
			response.Err(c, http.StatusConflict, "HAS_LICENSES",
				"cannot change the license type of a plan that has licenses; move them to another plan first")
			return
		}
		if feedGateRefused(c, err) {
			return
		}
		if isStripePriceConflict(err) {
			response.Err(c, http.StatusConflict, "STRIPE_PRICE_IN_USE", "stripe_price_id is already used by another plan")
			return
		}
		if feedGateConflict(c, err) {
			return
		}
		response.Internal(c)
		return
	}
	response.OK(c, p)
}

// isStripePriceConflict recognises the partial unique index on
// feedGateConflict maps a violation of the feed-gating invariant,
// raised by the database when concurrent requests slipped past the
// handler checks, to the same 409 the checks answer.
func feedGateConflict(c *gin.Context, err error) bool {
	switch {
	case err == nil:
		return false
	case strings.Contains(err.Error(), "products_feed_gate_in_use"):
		response.Conflict(c, "BOUNDED_PLANS_EXIST",
			"update feeds must keep requiring the license key while a plan of this product sells an update period or renewals, or a license still has an update cutoff", nil)
	case strings.Contains(err.Error(), "plans_feed_not_gated"), strings.Contains(err.Error(), "licenses_feed_not_gated"):
		response.Conflict(c, "FEED_NOT_GATED",
			"an update period or renewals need the product's update feeds to require the license key (feed_license_required)", nil)
	default:
		return false
	}
	return true
}

// isStripePriceConflict recognises a duplicate write of
// plans.stripe_price_id. The read-before-write check in
// stripePriceTaken gives the friendly message; the index is what
// makes the invariant hold under concurrent writes.
func isStripePriceConflict(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "idx_plans_stripe_price_unique") ||
		strings.Contains(err.Error(), "plans_renewal_price_disjoint"))
}

// stripePricesTaken rejects a purchase price or renewal price that
// overlaps another mapping. Purchase prices are unique across plans
// (stripePriceTaken); on top of that, renewal prices and purchase
// prices must be disjoint: a replica that predates renewals resolves
// a session by its line-item price alone and would mint a license for
// the plan owning that price instead of extending the intended one.
func (h *AdminHandler) stripePricesTaken(c *gin.Context, purchasePrice, renewalPrice, exceptPlanID string) bool {
	if h.stripePriceTaken(c, purchasePrice, exceptPlanID) {
		return true
	}
	if purchasePrice != "" {
		if other, err := h.Store.FindPlanByStripeRenewalPrice(c, purchasePrice); err == nil && other.ID != exceptPlanID {
			response.Conflict(c, "STRIPE_PRICE_IN_USE",
				"stripe_price_id is the renewal price of plan \""+other.Name+"\"",
				gin.H{"plan_id": other.ID, "product_id": other.ProductID})
			return true
		}
	}
	if renewalPrice != "" {
		if renewalPrice == purchasePrice {
			response.Conflict(c, "STRIPE_PRICE_IN_USE",
				"stripe_renewal_price_id must differ from stripe_price_id", nil)
			return true
		}
		if other, err := h.Store.FindPlanByStripePrice(c, renewalPrice); err == nil && other.ID != exceptPlanID {
			response.Conflict(c, "STRIPE_PRICE_IN_USE",
				"stripe_renewal_price_id is the purchase price of plan \""+other.Name+"\"",
				gin.H{"plan_id": other.ID, "product_id": other.ProductID})
			return true
		}
	}
	return false
}

// stripePriceTaken rejects a Stripe price already mapped to another
// plan. Checkout sessions created outside Keygate (Payment Links)
// resolve their plan by price alone, so the mapping must be unique.
// Writes the 409 response itself and reports whether it did.
func (h *AdminHandler) stripePriceTaken(c *gin.Context, priceID, exceptPlanID string) bool {
	if priceID == "" {
		return false
	}
	other, err := h.Store.FindPlanByStripePrice(c, priceID)
	if err != nil || other.ID == exceptPlanID {
		return false
	}
	response.Conflict(c, "STRIPE_PRICE_IN_USE",
		"stripe_price_id is already used by plan \""+other.Name+"\"",
		gin.H{"plan_id": other.ID, "product_id": other.ProductID})
	return true
}

func (h *AdminHandler) DeletePlan(c *gin.Context) {
	id := c.Param("id")
	count, _ := h.Store.PlanLicenseCount(c, id)
	if count > 0 {
		response.Err(c, http.StatusConflict, "HAS_LICENSES", "cannot delete plan with existing licenses")
		return
	}
	if err := h.Store.DeletePlan(c, id); err != nil {
		response.Internal(c)
		return
	}
	response.NoContent(c)
}

// ─── Entitlements ───

func (h *AdminHandler) CreateEntitlement(c *gin.Context) {
	var req struct {
		PlanID               string `json:"plan_id" binding:"required"`
		Feature              string `json:"feature" binding:"required"`
		ValueType            string `json:"value_type" binding:"required"`
		Value                string `json:"value" binding:"required"`
		QuotaPeriod          string `json:"quota_period"`
		QuotaUnit            string `json:"quota_unit"`
		StripeMeterEventName string `json:"stripe_meter_event_name"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "plan_id, feature, value_type, and value are required")
		return
	}

	switch req.ValueType {
	case "bool", "int", "string", "quota", "flag":
	default:
		response.BadRequest(c, "value_type must be bool, int, string, quota, or flag")
		return
	}
	value, period, err := normalizeFeatureValue(req.ValueType, req.Value, req.QuotaPeriod)
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	req.Value, req.QuotaPeriod = value, period
	// binding:"required" only refuses an empty string, and the
	// feature name is what every lookup keys on: " api_calls" would
	// match nothing anyone asks for.
	if req.Feature = strings.TrimSpace(req.Feature); req.Feature == "" {
		response.BadRequest(c, "feature is required")
		return
	}
	// Meter event names only make sense for quota features — they
	// describe how to bill incremental usage. Reject early so a
	// boolean feature doesn't silently carry a useless field.
	if req.StripeMeterEventName != "" && req.ValueType != "quota" {
		response.BadRequest(c, "stripe_meter_event_name requires value_type=quota")
		return
	}

	e := &model.Entitlement{
		PlanID: req.PlanID, Feature: req.Feature,
		ValueType: req.ValueType, Value: req.Value,
		QuotaPeriod: req.QuotaPeriod, QuotaUnit: req.QuotaUnit,
		StripeMeterEventName: req.StripeMeterEventName,
	}
	if err := h.Store.CreateEntitlement(c, e); err != nil {
		response.Err(c, http.StatusConflict, "DUPLICATE", "entitlement already exists for this plan and feature")
		return
	}
	response.Created(c, e)
}

// quotaPeriods are the windows a quota counter resets on
// (store.CurrentPeriodKey). Empty means monthly.
var quotaPeriods = map[string]bool{"hourly": true, "daily": true, "monthly": true, "yearly": true}

// featureValueTypes are the shapes a plan entitlement may take; an
// addon uses the same set without "flag".
var featureValueTypes = map[string]bool{"bool": true, "int": true, "string": true, "quota": true, "flag": true}

// normalizeFeatureValue checks that a value can be read back as the
// type it claims, and returns it in the shape the readers expect.
// Nothing downstream re-checks it, and the failures are silent in the
// worst direction: a quota whose value is not a number parses as 0
// (the error is dropped in service/usage.go), and 0 means "no limit"
// — the customer would get the feature with no cap at all, the
// opposite of what was sold. A bool that is not exactly "true" reads
// as false, and a period nobody recognises quietly becomes monthly.
//
// The trimmed value is returned rather than only checked, because the
// readers do not trim either: strconv.ParseInt(" 10 ") fails just as
// "ten" does, so accepting a value with spaces and storing it as
// typed would put the same 0 in the same place.
//
// The period is returned for the same reason and cleared when the
// feature is not a quota — as normalizeMaintenance clears a plan's
// update terms when it is not perpetual. Refusing instead would mean
// an addon that once was a quota could not be turned into anything
// else without a second request to clear a field the form no longer
// shows.
func normalizeFeatureValue(valueType, value, quotaPeriod string) (string, string, error) {
	value = strings.TrimSpace(value)
	quotaPeriod = strings.TrimSpace(quotaPeriod)
	switch valueType {
	case "bool", "flag":
		if value != "true" && value != "false" {
			return "", "", errors.New("value for a " + valueType + " feature must be \"true\" or \"false\"")
		}
	case "int", "quota":
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return "", "", errors.New("value for a " + valueType + " feature must be a whole number")
		}
		if n < 0 {
			return "", "", errors.New("value for a " + valueType + " feature cannot be negative")
		}
	}
	if valueType != "quota" {
		return value, "", nil
	}
	if quotaPeriod != "" && !quotaPeriods[quotaPeriod] {
		return "", "", errors.New("quota_period must be hourly, daily, monthly, or yearly")
	}
	return value, quotaPeriod, nil
}

func (h *AdminHandler) UpdateEntitlement(c *gin.Context) {
	e, err := h.Store.FindEntitlementByID(c, c.Param("id"))
	if err != nil {
		response.NotFound(c, "entitlement not found")
		return
	}
	var req struct {
		Feature              *string `json:"feature"`
		ValueType            *string `json:"value_type"`
		Value                *string `json:"value"`
		QuotaPeriod          *string `json:"quota_period"`
		QuotaUnit            *string `json:"quota_unit"`
		StripeMeterEventName *string `json:"stripe_meter_event_name"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "invalid request body")
		return
	}
	if req.Feature != nil {
		e.Feature = *req.Feature
	}
	if req.ValueType != nil {
		e.ValueType = *req.ValueType
	}
	if req.Value != nil {
		e.Value = *req.Value
	}
	if req.QuotaPeriod != nil {
		e.QuotaPeriod = *req.QuotaPeriod
	}
	if req.QuotaUnit != nil {
		e.QuotaUnit = *req.QuotaUnit
	}
	if req.StripeMeterEventName != nil {
		// Same gate as CreatePlan: meter wiring requires quota
		// semantics. Comparing against the post-merge ValueType in
		// case the same request also flipped value_type.
		if *req.StripeMeterEventName != "" && e.ValueType != "quota" {
			response.BadRequest(c, "stripe_meter_event_name requires value_type=quota")
			return
		}
		e.StripeMeterEventName = *req.StripeMeterEventName
	}
	// Against the merged row, not the fields this request happened to
	// carry: changing only the value of a quota, or only the type of
	// an entitlement, has to leave a pair that still reads back.
	if !featureValueTypes[e.ValueType] {
		response.BadRequest(c, "value_type must be bool, int, string, quota, or flag")
		return
	}
	if strings.TrimSpace(e.Feature) == "" {
		response.BadRequest(c, "feature is required")
		return
	}
	e.Feature = strings.TrimSpace(e.Feature)
	value, period, verr := normalizeFeatureValue(e.ValueType, e.Value, e.QuotaPeriod)
	if verr != nil {
		response.BadRequest(c, verr.Error())
		return
	}
	e.Value, e.QuotaPeriod = value, period

	if err := h.Store.UpdateEntitlement(c, e); err != nil {
		response.Internal(c)
		return
	}
	response.OK(c, e)
}

func (h *AdminHandler) DeleteEntitlement(c *gin.Context) {
	if err := h.Store.DeleteEntitlement(c, c.Param("id")); err != nil {
		response.Internal(c)
		return
	}
	response.NoContent(c)
}

// ─── API Keys ───

func (h *AdminHandler) ListAPIKeys(c *gin.Context) {
	page := listPage(c)
	keys, total, err := h.Store.ListAPIKeys(c, c.Query("product_id"), c.Query("search"), page)
	if err != nil {
		response.Internal(c)
		return
	}
	listOK(c, "api_keys", keys, total, page)
}

func (h *AdminHandler) CreateAPIKey(c *gin.Context) {
	// product_id is OPTIONAL: leave empty for system-wide keys (used
	// with the `admin` scope for operator scripts / CI/CD that need
	// to manage all products). Per-product keys (future: licenses:read
	// etc.) set product_id explicitly.
	var req struct {
		ProductID string   `json:"product_id"`
		Name      string   `json:"name" binding:"required"`
		Scopes    []string `json:"scopes"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "name is required")
		return
	}
	if req.ProductID != "" {
		if _, err := h.Store.FindProductByID(c, req.ProductID); err != nil {
			response.NotFound(c, "product not found")
			return
		}
	}
	if err := apperr.ValidateName("name", req.Name); err != nil {
		response.BadRequest(c, err.Message)
		return
	}
	// Scope validation: reject typos at the boundary so an
	// unintentional `licneses:write` doesn't become a useless key
	// that's silently locked out of every route. Empty scopes are
	// allowed and produce a fail-closed key (caller may want to set
	// scopes later via rotate or by re-creating).
	if err := validateScopes(req.Scopes); err != nil {
		response.BadRequest(c, err.Error())
		return
	}

	rawKey := store.GenerateRawAPIKey()
	prefix := rawKey[:12]
	if req.Scopes == nil {
		req.Scopes = []string{}
	}

	ak := &model.APIKey{
		ProductID: req.ProductID,
		Name:      req.Name,
		Prefix:    prefix,
		Scopes:    req.Scopes,
	}
	if err := h.Store.CreateAPIKey(c, ak, rawKey); err != nil {
		response.Internal(c)
		return
	}

	h.Store.Audit(c, &model.AuditLog{
		Entity: "api_key", EntityID: ak.ID, Action: "created",
		ActorType: "admin", ActorID: adminID(c),
		Changes: map[string]any{"name": req.Name, "product_id": req.ProductID, "scopes": req.Scopes},
	})

	response.Created(c, gin.H{
		"id":         ak.ID,
		"product_id": ak.ProductID,
		"name":       ak.Name,
		"key":        rawKey,
		"prefix":     prefix,
		"scopes":     ak.Scopes,
		"created_at": ak.CreatedAt,
	})
}

// planBounds carries the numeric fields that share the same range
// rules across CreatePlan + UpdatePlan, so both paths can share one
// validator. Zero is allowed everywhere (interpreted as "no limit"
// or "use the column default"); negative is always invalid; upper
// bounds are sized to "more than any plausible plan" so legitimate
// callers don't trip them.
type planBounds struct {
	MaxActivations  int
	MaxSeats        int
	TrialDays       int
	GraceDays       int
	FloatingTimeout int // minutes
	TokenTTLDays    int // 0 = server default
}

func validatePlanNumericBounds(b planBounds) error {
	switch {
	case b.MaxActivations < 0:
		return fmt.Errorf("max_activations cannot be negative")
	case b.MaxActivations > 10000:
		return fmt.Errorf("max_activations cannot exceed 10000")
	case b.MaxSeats < 0:
		return fmt.Errorf("max_seats cannot be negative")
	case b.MaxSeats > 100000:
		return fmt.Errorf("max_seats cannot exceed 100000")
	case b.TrialDays < 0:
		return fmt.Errorf("trial_days cannot be negative")
	case b.TrialDays > 365:
		return fmt.Errorf("trial_days cannot exceed 365")
	case b.GraceDays < 0:
		return fmt.Errorf("grace_days cannot be negative")
	case b.GraceDays > 365:
		return fmt.Errorf("grace_days cannot exceed 365")
	case b.FloatingTimeout < 0:
		return fmt.Errorf("floating_timeout cannot be negative")
	case b.FloatingTimeout > 1440:
		return fmt.Errorf("floating_timeout cannot exceed 1440 minutes (1 day)")
	case b.TokenTTLDays < 0:
		return fmt.Errorf("token_ttl_days cannot be negative")
	case b.TokenTTLDays > 365:
		return fmt.Errorf("token_ttl_days cannot exceed 365")
	}
	return nil
}

// validateScopes rejects unknown scope strings. Keeping the check at
// the boundary catches typos that would otherwise produce a silently
// powerless key.
func validateScopes(scopes []string) error {
	for _, s := range scopes {
		if !model.IsValidScope(s) {
			return fmt.Errorf("unknown scope %q (valid: %v)", s, model.AllScopes())
		}
	}
	return nil
}

// RotateAPIKey generates a new secret for an existing key. The old
// secret stops working immediately — pre-launch we don't ship a
// grace period because rolling two valid secrets at once doubles the
// blast radius if either leaks during the rotation window.
//
// The key ID is stable so callers can update their config without
// touching the row's name / scopes / product_id.
func (h *AdminHandler) RotateAPIKey(c *gin.Context) {
	id := c.Param("id")
	ak, err := h.Store.FindAPIKeyByID(c, id)
	if err != nil {
		response.NotFound(c, "api key not found")
		return
	}
	rawKey := store.GenerateRawAPIKey()
	prefix := rawKey[:12]
	if err := h.Store.RotateAPIKey(c, ak.ID, rawKey, prefix); err != nil {
		response.Internal(c)
		return
	}
	h.Store.Audit(c, &model.AuditLog{
		Entity: "api_key", EntityID: ak.ID, Action: "rotated",
		ActorType: "admin", ActorID: adminID(c),
		Changes: map[string]any{"name": ak.Name},
	})
	response.OK(c, gin.H{
		"id":     ak.ID,
		"name":   ak.Name,
		"key":    rawKey,
		"prefix": prefix,
		"scopes": ak.Scopes,
	})
}

func (h *AdminHandler) DeleteAPIKey(c *gin.Context) {
	id := c.Param("id")
	if err := h.Store.DeleteAPIKey(c, id); err != nil {
		response.Internal(c)
		return
	}
	h.Store.Audit(c, &model.AuditLog{
		Entity: "api_key", EntityID: id, Action: "deleted",
		ActorType: "admin", ActorID: adminID(c),
	})
	response.NoContent(c)
}

// ─── Licenses ───

// licenseSortColumns is what ?sort= accepts on the license list. Only
// columns the table actually shows are here: a sort the dashboard
// cannot express is a column nobody can read the result of.
//
// Product and plan are joined by name rather than id so the order
// matches what is on screen, and status sorts alphabetically because
// its stored values have no rank worth inventing one for.
var licenseSortColumns = map[string]sortCol{
	"created_at": {Expr: "license.created_at", Desc: true},
	// Soonest first. Asked to order by expiry and not told which way,
	// the question behind it is which licenses are about to lapse,
	// not which ones run longest. The dashboard's first click on this
	// column sends asc for the same reason; leaving the two disagreeing
	// would mean a direct API caller got the opposite answer.
	"valid_until": {Expr: "license.valid_until"},
	"email":       {Expr: "license.email"},
	"status":      {Expr: "license.status"},
	"product":     {Expr: "product.name"},
	"plan":        {Expr: "plan.name"},
}

func (h *AdminHandler) ListLicenses(c *gin.Context) {
	productID := c.Query("product_id")
	// When the request comes from an API key bound to a specific
	// product, override (or fill in) the product_id filter so the
	// list can never leak rows from other products even when the
	// caller forgets — or deliberately omits — the query param.
	if v, ok := c.Get("api_key"); ok {
		if ak, ok := v.(*model.APIKey); ok && ak != nil && ak.ProductID != "" {
			if productID != "" && productID != ak.ProductID {
				response.Err(c, http.StatusForbidden, "PRODUCT_SCOPE_MISMATCH",
					"api_key is bound to a different product")
				return
			}
			productID = ak.ProductID
		}
	}
	page := listPage(c)
	order, ok := listSort(c, licenseSortColumns, "created_at")
	if !ok {
		return
	}
	licenses, total, err := h.Store.ListLicenses(c, store.LicenseListFilter{
		ProductID:           productID,
		Status:              c.Query("status"),
		Search:              c.Query("search"),
		ExternalCustomerID:  c.Query("external_customer_id"),
		ExternalWorkspaceID: c.Query("external_workspace_id"),
		Sort:                order,
		Offset:              page.Offset,
		Limit:               page.Limit,
	})
	if err != nil {
		response.Internal(c)
		return
	}
	// Only the tail of each key travels with the list — enough for an
	// admin to tell two rows apart, useless to anyone who intercepts
	// the payload. The key itself comes from RevealLicenseKey.
	hints := make(map[string]string, len(licenses))
	for _, l := range licenses {
		if hint := h.licenseKeyHint(l); hint != "" {
			hints[l.ID] = hint
		}
	}
	listOK(c, "licenses", licenses, total, page, gin.H{"license_key_hints": hints})
}

func (h *AdminHandler) GetLicense(c *gin.Context) {
	id := c.Param("id")
	l, err := h.Store.FindLicenseByIDWithCounts(c, id)
	if err != nil {
		response.NotFound(c, "license not found")
		return
	}
	if !requireKeyProductScope(c, l.ProductID) {
		return
	}
	// The licence's own fields stay at the top level — clients read
	// .plan_id and .status straight off this object, and the only
	// deliberate break here is that license_key is gone. The hint
	// rides alongside them; the key itself is one explicit request
	// away via RevealLicenseKey.
	response.OK(c, licenseWithHint{License: l, LicenseKeyHint: h.licenseKeyHint(l)})
}

func (h *AdminHandler) CreateLicense(c *gin.Context) {
	var req struct {
		ProductID           string `json:"product_id" binding:"required"`
		PlanID              string `json:"plan_id" binding:"required"`
		Email               string `json:"email" binding:"required"`
		Notes               string `json:"notes"`
		ExternalCustomerID  string `json:"external_customer_id"`
		ExternalWorkspaceID string `json:"external_workspace_id"`
		// ValidUntil sets an explicit expiry (RFC 3339). Empty means the
		// plan decides: trial plans get now+trial_days, everything else
		// is perpetual. When set it wins over the trial default.
		ValidUntil string `json:"valid_until"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "product_id, plan_id, and email are required")
		return
	}

	if appErr := apperr.ValidateEmail(req.Email); appErr != nil {
		response.BadRequest(c, appErr.Message)
		return
	}
	// External IDs are opaque to Keygate but we cap their length to
	// keep them index-friendly. 256 chars is comfortably above what
	// any real identifier scheme produces (UUIDs, Stripe IDs, etc).
	if len(req.ExternalCustomerID) > 256 {
		response.BadRequest(c, "external_customer_id must be ≤ 256 chars")
		return
	}
	if len(req.ExternalWorkspaceID) > 256 {
		response.BadRequest(c, "external_workspace_id must be ≤ 256 chars")
		return
	}

	var validUntil *time.Time
	if req.ValidUntil != "" {
		ts, err := time.Parse(time.RFC3339, req.ValidUntil)
		if err != nil {
			response.BadRequest(c, "valid_until must be an RFC 3339 timestamp")
			return
		}
		if !ts.After(time.Now()) {
			response.BadRequest(c, "valid_until must be in the future")
			return
		}
		validUntil = &ts
	}

	// Look up plan first to determine license type and set appropriate fields
	plan, err := h.Store.FindPlanByID(c, req.PlanID)
	if err != nil {
		response.NotFound(c, "plan not found")
		return
	}
	// Plan/product consistency: a plan belongs to exactly one product
	// (plans.product_id is FK + NOT NULL). Accepting a mismatched
	// req.ProductID would persist a license whose product_id points
	// at a product whose plans don't include this one — that breaks
	// capability gating, billing routing, and every downstream join.
	if plan.ProductID != req.ProductID {
		response.Err(c, http.StatusBadRequest, "PLAN_PRODUCT_MISMATCH",
			"plan does not belong to the requested product")
		return
	}
	// API-key scoping: a key bound to product A can't mint a license
	// for product B even if it carries licenses:write.
	if !requireKeyProductScope(c, req.ProductID) {
		return
	}

	status := model.StatusActive
	if plan.LicenseType == "trial" {
		status = model.StatusTrialing
	}

	l := &model.License{
		ProductID:           req.ProductID,
		PlanID:              req.PlanID,
		Email:               req.Email,
		LicenseKey:          license.GenerateKey(""),
		Status:              status,
		Notes:               req.Notes,
		ExternalCustomerID:  req.ExternalCustomerID,
		ExternalWorkspaceID: req.ExternalWorkspaceID,
	}

	// Set valid_until: an explicit request value wins, otherwise trial
	// plans default to now+trial_days and other plans stay perpetual.
	if validUntil != nil {
		l.ValidUntil = validUntil
	} else if plan.LicenseType == "trial" && plan.TrialDays > 0 {
		until := time.Now().Add(time.Duration(plan.TrialDays) * 24 * time.Hour)
		l.ValidUntil = &until
	}
	// A bounded update period is only enforceable once every replica
	// runs a version that knows about it, so it is issued under the
	// same switch as plan configuration, manual cutoffs and checkout.
	// The database trigger still fills the period for writes this
	// build cannot gate (a replica on the previous version), where a
	// missing cutoff would mean updates for life forever.
	if updates := plan.InitialUpdatesUntil(time.Now()); updates != nil {
		if h.maintenanceGated(c) {
			return
		}
		// The same wait a bounded plan or a manual cutoff serves: a
		// period issued now would be skippable through a copy of the
		// public feed until the links in it expire.
		prod, err := h.Store.FindProductByID(c, l.ProductID)
		if err != nil {
			response.Internal(c)
			return
		}
		if h.feedNotGated(c, prod) {
			return
		}
		l.UpdatesUntil = updates
	}
	l.UpdatesTermsSet = true

	// Create license and subscription in a single transaction to prevent orphan records
	if err := h.Store.CreateLicenseWithSubscription(c, l, plan); err != nil {
		// The product's feeds went public between the check above and
		// this write.
		if errors.Is(err, store.ErrUpdatePeriodNotEnforceable) {
			response.Conflict(c, "FEED_NOT_GATED",
				"the product's update feeds no longer require the license key, so a license with an update period cannot be issued for it",
				gin.H{"product_id": l.ProductID})
			return
		}
		if errors.Is(err, store.ErrPlanChanged) {
			response.Conflict(c, "PLAN_CHANGED",
				"the plan's license type changed while this license was being created; reload and try again", nil)
			return
		}
		response.Internal(c)
		return
	}

	h.Store.Audit(c, &model.AuditLog{
		Entity: "license", EntityID: l.ID, Action: "created",
		ActorType: "admin", ActorID: adminID(c),
		Changes: map[string]any{"email": req.Email, "plan_id": req.PlanID},
	})

	if h.Webhook != nil {
		h.Webhook.Dispatch(c, l.ProductID, "license.created", map[string]any{
			"license_id": l.ID, "email": req.Email, "plan_id": req.PlanID,
		})
	}

	// Email the customer their new license key. Best-effort: SMTP failure
	// is logged inside SendLicenseCreated but doesn't fail the API call —
	// the license is already persisted, and admin can resend manually.
	if h.Email != nil && h.Email.IsConfigured() {
		productName := ""
		if prod, err := h.Store.FindProductByID(c, l.ProductID); err == nil {
			productName = prod.Name
		}
		h.Email.SendLicenseCreated(req.Email, productName, plan.Name, l.LicenseKey)
	}

	// The key is returned exactly here: the admin explicitly created
	// this license and needs to pass the key to the customer. Every
	// later read goes through RevealLicenseKey and is audited.
	c.Header("Cache-Control", "no-store")
	response.Created(c, licenseWithKey{License: l, LicenseKey: h.Store.DecryptLicenseKey(l)})
}

// licenseWithKey re-attaches the credential to a license payload for
// the few responses allowed to carry it. model.License hides the field
// so it can never leak by accident; opting in is deliberate and local.
type licenseWithKey struct {
	*model.License
	LicenseKey string `json:"license_key"`
}

// licenseWithHint is a licence payload with the last four characters of
// its key attached, for the detail view.
type licenseWithHint struct {
	*model.License
	LicenseKeyHint string `json:"license_key_hint"`
}

// licenseKeyHint is the last four characters of a key — a row label,
// not a credential.
func (h *AdminHandler) licenseKeyHint(l *model.License) string {
	key := h.Store.DecryptLicenseKey(l)
	r := []rune(key)
	if len(r) <= 4 {
		return string(r)
	}
	return string(r[len(r)-4:])
}

// RevealLicenseKey returns one plaintext license key.
// GET /api/v1/admin/licenses/:id/key
//
// Split out from the list and detail payloads so that browsing the
// dashboard, exporting a report, or holding a licenses:write API key
// does not spray every customer's credential across logs, proxies and
// browser caches. Each reveal is a deliberate act and is audited.
func (h *AdminHandler) RevealLicenseKey(c *gin.Context) {
	id := c.Param("id")
	if !h.checkLicenseScope(c, id) {
		return
	}
	l, err := h.Store.FindLicenseByID(c, id)
	if err != nil {
		response.NotFound(c, "license not found")
		return
	}

	key := h.Store.DecryptLicenseKey(l)
	if key == "" {
		// Ciphertext unreadable or the master key is gone —
		// DecryptLicenseKey has already logged it and bumped the metric.
		response.Err(c, http.StatusServiceUnavailable, "LICENSE_KEY_UNAVAILABLE",
			"license key could not be decrypted")
		return
	}

	// The audit records that a reveal happened, never the key.
	h.Store.Audit(c, &model.AuditLog{
		Entity: "license", EntityID: l.ID, Action: "key_revealed",
		ActorType: "admin", ActorID: adminID(c), IPAddress: c.ClientIP(),
		Changes: map[string]any{"email": l.Email},
	})

	c.Header("Cache-Control", "no-store")
	response.OK(c, gin.H{"license_key": key})
}

// ResendLicenseEmail mails the customer their key again, for when the
// original send was lost: SMTP was down when the license was created,
// the mail hit a spam folder, or the customer deleted it.
//
// The recipient is never taken from the request body. This endpoint
// puts a license key in an inbox, so letting the caller name the
// address would turn the admin API into a way to post somebody else's
// key wherever they liked. It is the address on the license or
// nothing.
//
// The mail is queued rather than sent inline. An SMTP server that
// accepts the connection and then stalls would otherwise hold this
// HTTP request open for the full session timeout, and a send that
// failed would simply be gone. Queued, it retries with backoff and
// survives a restart, which is the whole point of a resend button.
func (h *AdminHandler) ResendLicenseEmail(c *gin.Context) {
	id := c.Param("id")
	if !h.checkLicenseScope(c, id) {
		return
	}
	l, err := h.Store.FindLicenseByID(c, id)
	if err != nil {
		response.NotFound(c, "license not found")
		return
	}
	if l.Email == "" {
		response.Err(c, http.StatusConflict, "NO_EMAIL",
			"this license has no email address on file")
		return
	}
	if h.Email == nil || !h.Email.IsConfigured() {
		response.Err(c, http.StatusConflict, "SMTP_NOT_CONFIGURED",
			"SMTP is not configured on this server, so no mail can be sent")
		return
	}

	key := h.Store.DecryptLicenseKey(l)
	if key == "" {
		// Ciphertext unreadable or the master key is gone —
		// DecryptLicenseKey has already logged it and bumped the metric.
		response.Err(c, http.StatusServiceUnavailable, "LICENSE_KEY_UNAVAILABLE",
			"license key could not be decrypted")
		return
	}

	productName, planName := "", ""
	if l.Product != nil {
		productName = l.Product.Name
	}
	if l.Plan != nil {
		planName = l.Plan.Name
	}
	subject, body := h.Email.RenderLicenseCreated(productName, planName, key)
	if err := h.Store.EnqueueEmail(c, l.Email, subject, body); err != nil {
		response.Err(c, http.StatusInternalServerError, "EMAIL_QUEUE_FAILED",
			"could not queue the email")
		return
	}

	// Audited with the same weight as a reveal: both put the key
	// somewhere a person can read it.
	h.Store.Audit(c, &model.AuditLog{
		Entity: "license", EntityID: l.ID, Action: "key_emailed",
		ActorType: "admin", ActorID: adminID(c), IPAddress: c.ClientIP(),
		Changes: map[string]any{"email": l.Email},
	})

	response.OK(c, gin.H{"queued": true, "email": l.Email})
}

func (h *AdminHandler) RevokeLicense(c *gin.Context) {
	id := c.Param("id")
	if !h.checkLicenseScope(c, id) {
		return
	}
	if err := h.Store.RevokeLicense(c, id); err != nil {
		response.NotFound(c, err.Error())
		return
	}
	h.Store.Audit(c, &model.AuditLog{
		Entity: "license", EntityID: id, Action: "revoked",
		ActorType: "admin", ActorID: adminID(c),
	})
	if h.Webhook != nil {
		if lic, err := h.Store.FindLicenseByID(c, id); err == nil {
			h.Webhook.Dispatch(c, lic.ProductID, "license.revoked", map[string]any{
				"license_id": id, "email": lic.Email,
			})
		}
	}
	response.OK(c, gin.H{"status": "revoked"})
}

func (h *AdminHandler) RefundLicense(c *gin.Context) {
	id := c.Param("id")
	lic, err := h.Store.FindLicenseByID(c, id)
	if err != nil {
		response.NotFound(c, "license not found")
		return
	}
	if !requireKeyProductScope(c, lic.ProductID) {
		return
	}

	if lic.StripeSubscriptionID == "" {
		response.BadRequest(c, "license has no payment subscription to refund")
		return
	}

	// Cancel subscription at Stripe
	providerResult := "no_active_subscription"
	if _, cancelErr := subscription.Cancel(lic.StripeSubscriptionID, nil); cancelErr != nil {
		providerResult = "stripe_cancel_failed"
		slog.Error("stripe subscription cancel failed", "subscription_id", lic.StripeSubscriptionID, "error", cancelErr)
	} else {
		providerResult = "stripe_subscription_canceled"
	}

	// Mark the license as revoked
	lic.Status = model.StatusRevoked
	if err := h.Store.UpdateLicenseAndSubscription(c, lic, "status"); err != nil {
		response.Internal(c)
		return
	}

	h.Store.Audit(c, &model.AuditLog{
		Entity: "license", EntityID: id, Action: "refunded",
		ActorType: "admin", ActorID: adminID(c),
		Changes: map[string]any{"provider_result": providerResult},
	})

	if h.Webhook != nil {
		h.Webhook.Dispatch(c, lic.ProductID, "license.revoked", map[string]any{
			"license_id": id, "email": lic.Email, "reason": "refund",
		})
	}

	response.OK(c, gin.H{"status": "refunded", "provider_result": providerResult})
}

func (h *AdminHandler) SuspendLicense(c *gin.Context) {
	id := c.Param("id")
	if !h.checkLicenseScope(c, id) {
		return
	}
	if err := h.Store.SuspendLicense(c, id); err != nil {
		response.NotFound(c, err.Error())
		return
	}
	h.Store.Audit(c, &model.AuditLog{
		Entity: "license", EntityID: id, Action: "suspended",
		ActorType: "admin", ActorID: adminID(c),
	})
	if lic, err := h.Store.FindLicenseByID(c, id); err == nil {
		if h.Webhook != nil {
			h.Webhook.Dispatch(c, lic.ProductID, "license.suspended", map[string]any{
				"license_id": id, "email": lic.Email,
			})
		}
		if h.Email != nil && h.Email.IsConfigured() && lic.Email != "" {
			productName := ""
			if prod, perr := h.Store.FindProductByID(c, lic.ProductID); perr == nil {
				productName = prod.Name
			}
			h.Email.SendLicenseSuspended(lic.Email, productName, "")
		}
	}
	response.OK(c, gin.H{"status": "suspended"})
}

func (h *AdminHandler) ReinstateLicense(c *gin.Context) {
	id := c.Param("id")
	if !h.checkLicenseScope(c, id) {
		return
	}
	if err := h.Store.ReinstateLicense(c, id); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	h.Store.Audit(c, &model.AuditLog{
		Entity: "license", EntityID: id, Action: "reinstated",
		ActorType: "admin", ActorID: adminID(c),
	})
	if h.Webhook != nil {
		if lic, err := h.Store.FindLicenseByID(c, id); err == nil {
			h.Webhook.Dispatch(c, lic.ProductID, "license.reinstated", map[string]any{
				"license_id": id, "email": lic.Email,
			})
		}
	}
	response.OK(c, gin.H{"status": "active"})
}

// SetLicenseValidUntil sets or clears a license's expiry date. An
// empty valid_until makes the license perpetual. Extending an
// already-expired license does not change its status — use
// /reinstate for that (the two concerns stay separate so an
// accidental date edit can't silently re-arm a revoked customer).
func (h *AdminHandler) SetLicenseValidUntil(c *gin.Context) {
	id := c.Param("id")
	if !h.checkLicenseScope(c, id) {
		return
	}
	var req struct {
		ValidUntil string `json:"valid_until"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "invalid request body")
		return
	}

	lic, err := h.Store.FindLicenseByID(c, id)
	if err != nil {
		response.NotFound(c, "license not found")
		return
	}

	// A subscription owns the expiry of the licence it bills: the next
	// renewal webhook overwrites whatever is set here, so accepting
	// the edit would look like it worked and then silently revert.
	// The dashboard hides the control; this closes the same door on
	// the API, which licenses:write API keys also reach.
	//
	// The link is what decides, not the payment provider. A one-time
	// Stripe purchase renews nothing and has no webhook that would
	// come back for the date, and a licence unlinked from a finished
	// subscription has nothing pointing at it at all — refusing those
	// would leave an expiry nobody on either side could set.
	if lic.StripeSubscriptionID != "" {
		response.Conflict(c, "STRIPE_MANAGED",
			"expiry for Stripe-billed licenses is managed by the subscription", nil)
		return
	}

	var validUntil *time.Time
	if req.ValidUntil != "" {
		ts, err := time.Parse(time.RFC3339, req.ValidUntil)
		if err != nil {
			response.BadRequest(c, "valid_until must be an RFC 3339 timestamp or empty")
			return
		}
		// Same rule as CreateLicense. Back-dating is not an "expire now"
		// shortcut: the grace-expiry sweep would pick the license up and
		// email the customer that it expired, so a mistyped year turns
		// into customer-facing mail. Use revoke/suspend to end a license.
		if !ts.After(time.Now()) {
			response.BadRequest(c, "valid_until must be in the future")
			return
		}
		validUntil = &ts
	}

	lic.ValidUntil = validUntil
	if err := h.Store.UpdateLicense(c, lic, "valid_until"); err != nil {
		response.Internal(c)
		return
	}

	h.Store.Audit(c, &model.AuditLog{
		Entity: "license", EntityID: id, Action: "valid_until_changed",
		ActorType: "admin", ActorID: adminID(c),
		Changes: map[string]any{"valid_until": req.ValidUntil},
	})
	if h.Webhook != nil {
		// null valid_until means perpetual — send it explicitly so an
		// integration can tell "cleared" apart from "field omitted".
		payload := map[string]any{"license_id": id, "email": lic.Email, "valid_until": nil}
		if validUntil != nil {
			payload["valid_until"] = validUntil.Format(time.RFC3339)
		}
		h.Webhook.Dispatch(c, lic.ProductID, "license.expiry_changed", payload)
	}
	response.OK(c, lic)
}

// SetLicenseUpdatesUntil moves or clears the end of a license's
// maintenance period. Unlike valid_until this is not owned by Stripe:
// a paid renewal extends from whatever end is set here. Past dates
// are accepted — "no more updates as of last month" is a legitimate
// correction and has no side effect on the license itself.
//
// POST /admin/licenses/:id/updates-until  { updates_until: RFC 3339 | "" }
func (h *AdminHandler) SetLicenseUpdatesUntil(c *gin.Context) {
	id := c.Param("id")
	if !h.checkLicenseScope(c, id) {
		return
	}
	var req struct {
		UpdatesUntil string `json:"updates_until"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "invalid request body")
		return
	}
	lic, err := h.Store.FindLicenseByID(c, id)
	if err != nil {
		response.NotFound(c, "license not found")
		return
	}
	// Only perpetual licenses have a period separate from valid_until.
	plan, err := h.Store.FindPlanByID(c, lic.PlanID)
	if err != nil {
		response.Internal(c)
		return
	}
	if plan.LicenseType != "perpetual" {
		response.Err(c, http.StatusBadRequest, "NOT_PERPETUAL",
			"only perpetual licenses have an update period; others follow valid_until")
		return
	}
	var updatesUntil *time.Time
	if req.UpdatesUntil != "" {
		ts, err := time.Parse(time.RFC3339, req.UpdatesUntil)
		if err != nil {
			response.BadRequest(c, "updates_until must be an RFC 3339 timestamp or empty")
			return
		}
		updatesUntil = &ts
	}
	// A finite cutoff is a maintenance feature like any other: a
	// replica on the previous version would ignore it, and a public
	// feed would serve past it.
	if updatesUntil != nil {
		if h.maintenanceGated(c) {
			return
		}
		prod, err := h.Store.FindProductByID(c, lic.ProductID)
		if err != nil {
			response.Internal(c)
			return
		}
		if h.feedNotGated(c, prod) {
			return
		}
	}
	// Granting updates for life ends the current period for good;
	// renewals bought for it are closed so a later refund cannot
	// take days off a period set afterwards. A finite edit keeps
	// the ledger: refunds then still remove what was paid for.
	// The write applies only if the period is still what was read
	// above: a renewal committing in between must not be overwritten.
	// A finite cutoff is written under the product's feed gating lock,
	// with the gating state re-read there: an ungate and re-gate
	// committing after the check above would restart the drain, and
	// the trigger on the write only sees that the gate is on.
	if h.beforeCutoffWrite != nil {
		h.beforeCutoffWrite()
	}
	applied, moved := false, false
	err = h.Store.RunInTx(c, func(ctx context.Context, tx bun.Tx) error {
		// Row first, then the gate: the order the triggers use.
		locked, err := store.LockLicenseIn(ctx, tx, lic.ID)
		if err != nil {
			return err
		}
		// The plan read above may have been changed underneath this
		// edit. A license moved off a perpetual plan has no period of
		// its own — and a plan change leaves the same NULL the admin
		// saw, so the compare-and-set below would not notice.
		lockedPlan, err := store.FindPlanByIDIn(ctx, tx, locked.PlanID)
		if err != nil {
			return err
		}
		if lockedPlan.LicenseType != "perpetual" || locked.ProductID != lic.ProductID {
			moved = true
			return nil
		}
		if updatesUntil != nil {
			if problem, err := h.maintenanceSwitchOff(ctx, tx); err != nil || problem != nil {
				return firstNonNil(err, problem)
			}
			problem, err := h.lockFeedGateIn(ctx, tx, locked.ProductID)
			if err != nil {
				return err
			}
			if problem != nil {
				return problem
			}
		}
		applied, err = store.SetLicenseUpdatesUntilIn(ctx, tx, lic.ID, lic.UpdatesUntil, updatesUntil, updatesUntil == nil)
		return err
	})
	if err != nil {
		if feedGateRefused(c, err) {
			return
		}
		if feedGateConflict(c, err) {
			return
		}
		response.Internal(c)
		return
	}
	if moved {
		response.Conflict(c, "LICENSE_CHANGED",
			"the license moved to another plan or product while you were editing it; reload and try again", nil)
		return
	}
	if !applied {
		response.Conflict(c, "LICENSE_CHANGED",
			"the license's update period changed while you were editing it; reload and try again", nil)
		return
	}
	lic.UpdatesUntil = updatesUntil
	h.Store.Audit(c, &model.AuditLog{
		Entity: "license", EntityID: id, Action: "updates_until_changed",
		ActorType: "admin", ActorID: adminID(c),
		Changes: map[string]any{"updates_until": req.UpdatesUntil},
	})
	response.OK(c, lic)
}

// UnlinkStripeSubscription cuts a licence loose from a Stripe
// subscription that is over, so it can be managed locally again —
// moved to another plan, above all, which is refused while the link
// stands because nothing here can move the subscription itself.
//
// The check is Stripe's, not ours: no local state proves the billing
// stopped (suspend and revoke never reach Stripe, and "canceled" is
// also what an `unpaid` subscription reads as, which paying the
// invoice revives). SubscriptionEnded asks Stripe and is wired in
// main; without it there is nothing to confirm with and the endpoint
// refuses.
//
// Afterwards a refund of that subscription's last invoice no longer
// finds the licence by subscription id — it falls back to the payment
// intent and the customer — which is why this is a deliberate action
// and not something inferred from a webhook.
//
// POST /admin/licenses/:id/stripe/unlink
func (h *AdminHandler) UnlinkStripeSubscription(c *gin.Context) {
	id := c.Param("id")
	if !h.checkLicenseScope(c, id) {
		return
	}
	lic, err := h.Store.FindLicenseByID(c, id)
	if err != nil {
		response.NotFound(c, "license not found")
		return
	}
	if lic.StripeSubscriptionID == "" {
		response.OK(c, gin.H{"status": "not_linked"})
		return
	}
	if h.SubscriptionEnded == nil {
		response.Err(c, http.StatusServiceUnavailable, "STRIPE_UNAVAILABLE",
			"Stripe is not configured on this install, so the subscription's state cannot be confirmed")
		return
	}
	ended, err := h.SubscriptionEnded(c, lic.StripeSubscriptionID)
	if err != nil {
		// Stripe's own refusals carry their own wording — a key that
		// cannot see the subscription is a different problem from a
		// call that failed.
		var ae *apperr.AppError
		if errors.As(err, &ae) {
			response.Err(c, ae.Status, ae.Code, ae.Message)
			return
		}
		response.Err(c, http.StatusBadGateway, "STRIPE_UNAVAILABLE",
			"could not ask Stripe about this subscription; try again")
		return
	}
	if !ended {
		response.Conflict(c, "SUBSCRIPTION_LIVE",
			"Stripe still has this subscription; cancel it there (or from the customer portal) before unlinking", nil)
		return
	}
	subscriptionID := lic.StripeSubscriptionID
	// Cleared only while the licence still points at the subscription
	// Stripe just answered about. A checkout that linked a new one in
	// the meantime is not what the admin confirmed, and unlinking it
	// would cut a subscription that is still billing.
	cleared, err := h.Store.ClearStripeSubscription(c, id, subscriptionID)
	if err != nil {
		response.Internal(c)
		return
	}
	if !cleared {
		response.Conflict(c, "SUBSCRIPTION_CHANGED",
			"this license moved to a different Stripe subscription while you were looking at it; reload and check again", nil)
		return
	}
	h.Store.Audit(c, &model.AuditLog{
		Entity: "license", EntityID: id, Action: "stripe_unlinked",
		ActorType: "admin", ActorID: adminID(c),
		Changes: map[string]any{"stripe_subscription_id": subscriptionID},
	})
	response.OK(c, gin.H{"status": "unlinked"})
}

func (h *AdminHandler) DeleteActivation(c *gin.Context) {
	id := c.Param("id")
	pid, err := h.Store.GetActivationProductID(c, id)
	if err != nil {
		response.NotFound(c, "activation not found")
		return
	}
	if !requireKeyProductScope(c, pid) {
		return
	}
	if err := h.Store.DeleteActivationByID(c, id); err != nil {
		response.Internal(c)
		return
	}
	response.NoContent(c)
}

// ─── Audit Logs ───

func (h *AdminHandler) ListAuditLogs(c *gin.Context) {
	page := listPage(c)
	logs, total, err := h.Store.ListAuditLogs(c,
		c.Query("entity"), c.Query("entity_id"), c.Query("product_id"),
		page.Offset, page.Limit)
	if err != nil {
		response.Internal(c)
		return
	}
	listOK(c, "audit_logs", logs, total, page)
}

// ─── Users ───

func (h *AdminHandler) ListUsers(c *gin.Context) {
	page := listPage(c)
	users, total, err := h.Store.ListUsers(c, c.Query("search"), page.Offset, page.Limit)
	if err != nil {
		response.Internal(c)
		return
	}
	listOK(c, "users", users, total, page)
}

// ─── Helpers ───

func adminID(c *gin.Context) string {
	v, _ := c.Get("user_id")
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func queryInt(c *gin.Context, key string, def int) int {
	if v := c.Query(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// checkLicenseScope is the per-handler entry point for API-key
// product scoping on license-id-bearing routes. Looks up the
// license's product_id and defers to requireKeyProductScope. Returns
// true on pass (caller continues); false after writing a 404 or 403
// (caller must `return`).
func (h *AdminHandler) checkLicenseScope(c *gin.Context, licenseID string) bool {
	pid, err := h.Store.GetLicenseProductID(c, licenseID)
	if err != nil {
		// sql.ErrNoRows is the dominant failure here; surface as 404.
		// Other DB errors are rare enough that 404 is still a safe
		// answer — we'd hit them again in the real handler.
		response.NotFound(c, "license not found")
		return false
	}
	return requireKeyProductScope(c, pid)
}

// requireKeyProductScope blocks an API key bound to product A from
// touching resources that belong to product B. Returns true to
// continue, false after writing a 403 (caller should `return`).
//
//   - session auth → true (admin dashboards span all products)
//   - api_key with empty product_id → true (system-wide key)
//   - api_key with matching product_id → true
//   - api_key with different product_id → 403 + false
//
// Call AFTER resolving the resource's product_id. Two-hop lookups
// (e.g. seat → license → product) just pass the final product_id
// here; this helper doesn't try to be clever.
func requireKeyProductScope(c *gin.Context, resourceProductID string) bool {
	v, ok := c.Get("api_key")
	if !ok {
		return true
	}
	ak, ok := v.(*model.APIKey)
	if !ok || ak == nil || ak.ProductID == "" {
		return true
	}
	if ak.ProductID == resourceProductID {
		return true
	}
	response.Err(c, http.StatusForbidden, "PRODUCT_SCOPE_MISMATCH",
		"api_key is bound to a different product")
	return false
}

// ─── Usage (admin) ───

func (h *AdminHandler) ListLicenseUsage(c *gin.Context) {
	id := c.Param("id")
	if !h.checkLicenseScope(c, id) {
		return
	}
	page := listPage(c)
	events, total, err := h.Store.ListUsageEvents(c, id, c.Query("feature"), page.Offset, page.Limit)
	if err != nil {
		response.Internal(c)
		return
	}
	counters, _ := h.Store.GetUsageSummary(c, id)
	listOK(c, "events", events, total, page, gin.H{"counters": counters})
}

func (h *AdminHandler) ResetLicenseUsage(c *gin.Context) {
	id := c.Param("id")
	if !h.checkLicenseScope(c, id) {
		return
	}
	var req struct {
		Feature   string `json:"feature" binding:"required"`
		Period    string `json:"period"`
		PeriodKey string `json:"period_key"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "feature is required")
		return
	}
	period := req.Period
	if period == "" {
		period = "monthly"
	}
	periodKey := req.PeriodKey
	if periodKey == "" {
		periodKey = store.CurrentPeriodKey(period)
	}
	if err := h.Store.ResetUsageCounter(c, id, req.Feature, period, periodKey); err != nil {
		response.Internal(c)
		return
	}
	h.Store.Audit(c, &model.AuditLog{
		Entity: "license", EntityID: id, Action: "usage_reset",
		ActorType: "admin", ActorID: adminID(c),
		Changes: map[string]any{"feature": req.Feature, "period": period, "period_key": periodKey},
	})
	response.OK(c, gin.H{"status": "reset"})
}

// ─── Seats (admin) ───

func (h *AdminHandler) ListLicenseSeats(c *gin.Context) {
	id := c.Param("id")
	if !h.checkLicenseScope(c, id) {
		return
	}
	page := listPage(c)
	seats, total, err := h.Store.ListSeats(c, id, page)
	if err != nil {
		response.Internal(c)
		return
	}
	count, _ := h.Store.CountActiveSeats(c, id)
	listOK(c, "seats", seats, total, page, gin.H{"active_count": count})
}

// ─── Analytics (admin) ───

func (h *AdminHandler) ListAnalytics(c *gin.Context) {
	productID := c.Query("product_id")
	granularity := c.Query("granularity")
	var from, to time.Time
	if v := c.Query("from"); v != "" {
		from, _ = time.Parse("2006-01-02", v)
	}
	if v := c.Query("to"); v != "" {
		to, _ = time.Parse("2006-01-02", v)
	}
	if from.IsZero() {
		from = time.Now().AddDate(0, -1, 0)
	}
	if to.IsZero() {
		to = time.Now()
	}

	// Analytics is the one list that is not paged. It is a time
	// series: the caller already bounds it by asking for a date range,
	// every row is one day (or week, or month) of it, and the chart
	// that reads it draws the whole window — handing it page 2 of a
	// line would draw a line with a hole in it.
	if granularity == "weekly" || granularity == "monthly" {
		snapshots, err := h.Store.ListAnalyticsSnapshotsAggregated(c, productID, from, to, granularity)
		if err != nil {
			response.Internal(c)
			return
		}
		response.OK(c, gin.H{"snapshots": snapshots, "granularity": granularity})
		return
	}

	snapshots, err := h.Store.ListAnalyticsSnapshots(c, productID, from, to)
	if err != nil {
		response.Internal(c)
		return
	}
	response.OK(c, gin.H{"snapshots": snapshots})
}

func analyticsFilter(c *gin.Context) store.AnalyticsFilter {
	f := store.AnalyticsFilter{
		ProductID:   c.Query("product_id"),
		PlanID:      c.Query("plan_id"),
		LicenseType: c.Query("license_type"),
		Status:      c.Query("status"),
	}
	if v := c.Query("from"); v != "" {
		f.From, _ = time.Parse("2006-01-02", v)
	}
	if v := c.Query("to"); v != "" {
		t, err := time.Parse("2006-01-02", v)
		if err == nil {
			// End-of-day: include the entire "to" date
			f.To = t.Add(24*time.Hour - time.Nanosecond)
		}
	}
	return f
}

func (h *AdminHandler) AnalyticsSummary(c *gin.Context) {
	summary, err := h.Store.GetAnalyticsSummary(c, analyticsFilter(c))
	if err != nil {
		response.Internal(c)
		return
	}
	response.OK(c, summary)
}

func (h *AdminHandler) AnalyticsBreakdown(c *gin.Context) {
	dimension := c.Query("dimension")
	if dimension != "status" && dimension != "plan" && dimension != "license_type" {
		response.BadRequest(c, "dimension must be status, plan, or license_type")
		return
	}
	items, err := h.Store.GetLicenseBreakdown(c, analyticsFilter(c), dimension)
	if err != nil {
		response.Internal(c)
		return
	}
	response.OK(c, gin.H{"items": items})
}

func (h *AdminHandler) AnalyticsUsageTop(c *gin.Context) {
	productID := c.Query("product_id")
	var from, to time.Time
	if v := c.Query("from"); v != "" {
		from, _ = time.Parse("2006-01-02", v)
	}
	if v := c.Query("to"); v != "" {
		to, _ = time.Parse("2006-01-02", v)
	}
	limit := queryInt(c, "limit", 10)
	features, err := h.Store.GetTopFeatureUsage(c, productID, from, to, limit)
	if err != nil {
		response.Internal(c)
		return
	}
	response.OK(c, gin.H{"features": features})
}

func (h *AdminHandler) AnalyticsActivationTrend(c *gin.Context) {
	productID := c.Query("product_id")
	var from, to time.Time
	if v := c.Query("from"); v != "" {
		from, _ = time.Parse("2006-01-02", v)
	}
	if v := c.Query("to"); v != "" {
		to, _ = time.Parse("2006-01-02", v)
	}
	trend, err := h.Store.GetActivationTrend(c, productID, from, to)
	if err != nil {
		response.Internal(c)
		return
	}
	response.OK(c, gin.H{"trend": trend})
}

func (h *AdminHandler) AnalyticsInsights(c *gin.Context) {
	f := analyticsFilter(c)
	growth, err := h.Store.GetGrowthMetrics(c, f.ProductID)
	if err != nil {
		response.Internal(c)
		return
	}
	ageDist, _ := h.Store.GetLicenseAgeDistribution(c, f.ProductID)
	topUsers, _ := h.Store.GetTopUsers(c, f.ProductID, queryInt(c, "top_limit", 10))
	retention, _ := h.Store.GetRetentionData(c, f.ProductID, queryInt(c, "months", 6))
	recentActivity, _ := h.Store.GetRecentActivity(c, f.ProductID, queryInt(c, "activity_limit", 20))

	response.OK(c, gin.H{
		"growth":           growth,
		"age_distribution": ageDist,
		"top_users":        topUsers,
		"retention":        retention,
		"recent_activity":  recentActivity,
	})
}

func (h *AdminHandler) GetUserDetail(c *gin.Context) {
	id := c.Param("id")
	detail, err := h.Store.GetUserDetail(c, id)
	if err != nil {
		response.NotFound(c, "user not found")
		return
	}
	// Same rule as the licenses list: the customer view shows which
	// license is which, not the credential itself. The detail keeps
	// its existing shape and the hints ride alongside it.
	hints := make(map[string]string, len(detail.Licenses))
	for _, l := range detail.Licenses {
		if hint := h.licenseKeyHint(l); hint != "" {
			hints[l.ID] = hint
		}
	}
	response.OK(c, struct {
		*store.UserDetail
		LicenseKeyHints map[string]string `json:"license_key_hints"`
	}{UserDetail: detail, LicenseKeyHints: hints})
}

// ─── Addons ───

func (h *AdminHandler) ListAddons(c *gin.Context) {
	page := listPage(c)
	addons, total, err := h.Store.ListAddons(c, c.Query("product_id"), c.Query("search"), page)
	if err != nil {
		response.Internal(c)
		return
	}
	listOK(c, "addons", addons, total, page)
}

func (h *AdminHandler) CreateAddon(c *gin.Context) {
	var req struct {
		ProductID   string `json:"product_id" binding:"required"`
		Name        string `json:"name" binding:"required"`
		Slug        string `json:"slug" binding:"required"`
		Description string `json:"description"`
		Feature     string `json:"feature" binding:"required"`
		ValueType   string `json:"value_type" binding:"required"`
		Value       string `json:"value" binding:"required"`
		QuotaPeriod string `json:"quota_period"`
		QuotaUnit   string `json:"quota_unit"`
		SortOrder   int    `json:"sort_order"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "product_id, name, slug, feature, value_type, and value are required")
		return
	}
	if err := apperr.ValidateName("name", req.Name); err != nil {
		response.BadRequest(c, err.Message)
		return
	}
	if err := apperr.ValidateSlug(req.Slug); err != nil {
		response.BadRequest(c, err.Message)
		return
	}
	switch req.ValueType {
	case "bool", "int", "string", "quota":
	default:
		response.BadRequest(c, "value_type must be bool, int, string, or quota")
		return
	}
	value, period, verr := normalizeFeatureValue(req.ValueType, req.Value, req.QuotaPeriod)
	if verr != nil {
		response.BadRequest(c, verr.Error())
		return
	}
	req.Value, req.QuotaPeriod = value, period
	if req.Feature = strings.TrimSpace(req.Feature); req.Feature == "" {
		response.BadRequest(c, "feature is required")
		return
	}

	a := &model.Addon{
		ProductID: req.ProductID, Name: req.Name, Slug: req.Slug,
		Description: req.Description, Feature: req.Feature,
		ValueType: req.ValueType, Value: req.Value,
		QuotaPeriod: req.QuotaPeriod, QuotaUnit: req.QuotaUnit,
		Active: true, SortOrder: req.SortOrder,
	}
	if err := h.Store.CreateAddon(c, a); err != nil {
		// The insert fails for two quite different reasons and the
		// admin has to be able to tell them apart.
		if strings.Contains(err.Error(), "foreign key") {
			response.BadRequest(c, "product_id does not exist")
			return
		}
		response.Err(c, 409, "DUPLICATE", "addon slug already exists for this product")
		return
	}
	response.Created(c, a)
}

func (h *AdminHandler) UpdateAddon(c *gin.Context) {
	a, err := h.Store.FindAddonByID(c, c.Param("id"))
	if err != nil {
		response.NotFound(c, "addon not found")
		return
	}

	var req struct {
		Name *string `json:"name"`
		// The slug is an addon's handle in the admin UI and nothing
		// reads it beyond the per-product unique index, so a typo is
		// worth being able to fix — as it already is on a plan.
		Slug        *string `json:"slug"`
		Description *string `json:"description"`
		Feature     *string `json:"feature"`
		ValueType   *string `json:"value_type"`
		Value       *string `json:"value"`
		QuotaPeriod *string `json:"quota_period"`
		QuotaUnit   *string `json:"quota_unit"`
		Active      *bool   `json:"active"`
		SortOrder   *int    `json:"sort_order"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "invalid request")
		return
	}
	if req.Name != nil {
		a.Name = *req.Name
	}
	if req.Slug != nil {
		a.Slug = *req.Slug
	}
	if req.Description != nil {
		a.Description = *req.Description
	}
	if req.Feature != nil {
		a.Feature = *req.Feature
	}
	if req.ValueType != nil {
		a.ValueType = *req.ValueType
	}
	if req.Value != nil {
		a.Value = *req.Value
	}
	if req.QuotaPeriod != nil {
		a.QuotaPeriod = *req.QuotaPeriod
	}
	if req.QuotaUnit != nil {
		a.QuotaUnit = *req.QuotaUnit
	}
	if req.Active != nil {
		a.Active = *req.Active
	}
	if req.SortOrder != nil {
		a.SortOrder = *req.SortOrder
	}
	// The create path checks all of this; an update went straight to
	// the database, so a name, a type or a value refused at creation
	// could be put on the same addon a moment later.
	if req.Name != nil {
		if err := apperr.ValidateName("name", a.Name); err != nil {
			response.BadRequest(c, err.Message)
			return
		}
	}
	if req.Slug != nil {
		if err := apperr.ValidateSlug(a.Slug); err != nil {
			response.BadRequest(c, err.Message)
			return
		}
	}
	switch a.ValueType {
	case "bool", "int", "string", "quota":
	default:
		response.BadRequest(c, "value_type must be bool, int, string, or quota")
		return
	}
	if strings.TrimSpace(a.Feature) == "" {
		response.BadRequest(c, "feature is required")
		return
	}
	a.Feature = strings.TrimSpace(a.Feature)
	value, period, verr := normalizeFeatureValue(a.ValueType, a.Value, a.QuotaPeriod)
	if verr != nil {
		response.BadRequest(c, verr.Error())
		return
	}
	a.Value, a.QuotaPeriod = value, period
	if err := h.Store.UpdateAddon(c, a); err != nil {
		if strings.Contains(err.Error(), "duplicate") || strings.Contains(err.Error(), "unique") {
			response.Err(c, 409, "DUPLICATE", "addon slug already exists for this product")
			return
		}
		response.Internal(c)
		return
	}
	response.OK(c, a)
}

func (h *AdminHandler) DeleteAddon(c *gin.Context) {
	if err := h.Store.DeleteAddon(c, c.Param("id")); err != nil {
		response.Internal(c)
		return
	}
	response.NoContent(c)
}

func (h *AdminHandler) AddLicenseAddon(c *gin.Context) {
	id := c.Param("id")
	if !h.checkLicenseScope(c, id) {
		return
	}
	var req struct {
		AddonID string `json:"addon_id" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "addon_id is required")
		return
	}
	la := &model.LicenseAddon{LicenseID: id, AddonID: req.AddonID, Enabled: true}
	if err := h.Store.AddLicenseAddon(c, la); err != nil {
		response.Internal(c)
		return
	}
	response.Created(c, la)
}

func (h *AdminHandler) RemoveLicenseAddon(c *gin.Context) {
	id := c.Param("id")
	if !h.checkLicenseScope(c, id) {
		return
	}
	if err := h.Store.RemoveLicenseAddon(c, id, c.Param("addon_id")); err != nil {
		response.Internal(c)
		return
	}
	response.NoContent(c)
}

func (h *AdminHandler) ListLicenseAddons(c *gin.Context) {
	id := c.Param("id")
	if !h.checkLicenseScope(c, id) {
		return
	}
	page := listPage(c)
	addons, total, err := h.Store.ListLicenseAddons(c, id, page)
	if err != nil {
		response.Internal(c)
		return
	}
	listOK(c, "addons", addons, total, page)
}

func (h *AdminHandler) ListFloatingSessions(c *gin.Context) {
	id := c.Param("id")
	if !h.checkLicenseScope(c, id) {
		return
	}
	page := listPage(c)
	sessions, total, err := h.Store.ListFloatingSessions(c, id, page)
	if err != nil {
		response.Internal(c)
		return
	}
	active, _ := h.Store.CountActiveFloating(c, id)
	listOK(c, "sessions", sessions, total, page, gin.H{"active": active})
}

// ─── Change Plan (admin) ───

func (h *AdminHandler) ChangeLicensePlan(c *gin.Context) {
	id := c.Param("id")
	var req struct {
		PlanID string `json:"plan_id" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "plan_id is required")
		return
	}

	l, err := h.Store.FindLicenseByID(c, id)
	if err != nil {
		response.NotFound(c, "license not found")
		return
	}
	if !requireKeyProductScope(c, l.ProductID) {
		return
	}

	plan, err := h.Store.FindPlanByID(c, req.PlanID)
	if err != nil {
		response.NotFound(c, "plan not found")
		return
	}
	if plan.ProductID != l.ProductID {
		response.BadRequest(c, "plan must belong to the same product")
		return
	}

	oldPlanID := l.PlanID
	oldPlan, err := h.Store.FindPlanByID(c, oldPlanID)
	if err != nil {
		response.Internal(c)
		return
	}
	l.PlanID = req.PlanID
	// An early refusal with the usual message: the switch is read
	// again inside the transaction, where the plan that decides
	// whether a period is granted at all is the locked one.
	if plan.LicenseType == "perpetual" && oldPlan.LicenseType != "perpetual" &&
		plan.InitialUpdatesUntil(time.Now()) != nil && h.maintenanceGated(c) {
		return
	}
	// What happens to the update period follows the license types,
	// and both plans are read again under lock inside the write: a
	// plan retyped between this handler's read and its write would
	// otherwise decide the license's shape by a stale reading — most
	// of all when the target has just become perpetual, where the
	// stale branch would clear a paid period and close its renewal
	// ledger.
	moved := false
	// What the re-shape decided, for the audit entry: support reads
	// that log to answer "why is this licence active now" and "who
	// moved its expiry date".
	newStatus, newValidUntil := "", ""
	if h.beforeCutoffWrite != nil {
		h.beforeCutoffWrite()
	}
	err = h.Store.RunInTx(c, func(ctx context.Context, tx bun.Tx) error {
		// Rows first, then the gate: the license being written, and
		// the plan it moves to, whose key this write references.
		locked, err := store.LockLicenseIn(ctx, tx, l.ID)
		if err != nil {
			return err
		}
		if err := store.LockReferencedRowsIn(ctx, tx, req.PlanID, ""); err != nil {
			return err
		}
		if locked.PlanID != oldPlanID {
			moved = true
			return nil
		}
		newPlan, err := store.FindPlanByIDIn(ctx, tx, req.PlanID)
		if err != nil {
			return err
		}
		fromPlan, err := store.FindPlanByIDIn(ctx, tx, locked.PlanID)
		if err != nil {
			return err
		}
		if newPlan.ProductID != locked.ProductID {
			moved = true
			return nil
		}

		// While a licence is tied to a Stripe subscription, its plan
		// is Stripe's to change, not this endpoint's: nothing here
		// moves the subscription, so Stripe would keep charging the
		// old price while the admin panel, the portal and the
		// subscription row all showed the new plan — and nothing
		// reconciles it afterwards, since
		// customer.subscription.updated syncs status and dates but
		// never plan_id. The portal's change-plan does it in the
		// right order, Stripe first. A plan's Stripe price is unique,
		// so there is no same-price move to wave through either.
		//
		// No local state is taken as proof that the billing ended:
		// suspending and revoking never reach Stripe at all, and even
		// "canceled" is what Stripe's `unpaid` is written as, which
		// an invoice paid later revives. The way out is
		// UnlinkStripeSubscription — a deliberate act with Stripe's
		// own answer behind it.
		if locked.StripeSubscriptionID != "" && newPlan.ID != locked.PlanID {
			return errStripeBilledPlanChange
		}

		// The maintenance period follows the license type. Leaving
		// perpetual clears it (subscriptions follow valid_until);
		// entering perpetual starts the new plan's period; moving
		// between perpetual plans keeps what the customer already
		// has, and is not written at all — a renewal may have
		// committed since this handler read the license, and writing
		// the old value back would take that paid time away. A change
		// of license type starts a new period, and the renewal ledger
		// of the old one is closed with it.
		update := store.UpdateLicenseIn
		cols := []string{"plan_id"}
		grantsPeriod := false
		switch {
		case newPlan.LicenseType != "perpetual":
			l.UpdatesUntil = nil
			cols = append(cols, "updates_until")
			update = store.UpdateLicenseAndSupersedeRenewalsIn
		case fromPlan.LicenseType != "perpetual":
			updates := newPlan.InitialUpdatesUntil(time.Now())
			l.UpdatesUntil = updates
			grantsPeriod = updates != nil
			cols = append(cols, "updates_until")
			update = store.UpdateLicenseAndSupersedeRenewalsIn
		default:
			l.UpdatesUntil = locked.UpdatesUntil
		}
		// A licence takes the shape the new plan would have issued it
		// in. Without this it keeps the old type's: a trial licence
		// moved to a perpetual plan stays "trialing" with the trial's
		// valid_until, so the customer who just bought it is refused
		// on the day the trial would have ended — and the hourly
		// trial sweep marks it expired. Only the status a plan change
		// can speak for is touched: suspended and revoked are
		// somebody's decision about this licence, not about its plan.
		if fromPlan.LicenseType != newPlan.LicenseType {
			status := locked.Status
			if newPlan.LicenseType == "trial" {
				if status == model.StatusActive || status == model.StatusTrialing {
					status = model.StatusTrialing
				}
			} else if status == model.StatusTrialing {
				// Only the trial's own status is this endpoint's to
				// settle: "trialing" describes the plan the licence
				// was on, and nothing else can clear it (reinstate
				// takes suspended, expired and canceled — not this).
				// An expired licence is left expired: moving a plan
				// is not evidence that anyone paid, and giving access
				// back has its own deliberate action.
				status = model.StatusActive
			}
			if status != locked.Status {
				l.Status = status
				newStatus = status
				cols = append(cols, "status")
			}
			// The deadline moves only when a trial is on one side of
			// the change, because only then is it the plan's: a trial
			// plan issues one, and it leaves with the trial. Between
			// two plans that issue none — perpetual to subscription,
			// say — whatever date the licence carries is an admin's
			// own, set through the expiry control and shown in the
			// dashboard, and clearing it here would hand the customer
			// a licence that never expires without saying so.
			if fromPlan.LicenseType == "trial" || newPlan.LicenseType == "trial" {
				var until *time.Time
				if newPlan.LicenseType == "trial" && newPlan.TrialDays > 0 {
					t := time.Now().Add(time.Duration(newPlan.TrialDays) * 24 * time.Hour)
					until = &t
				}
				if !model.SameEnd(until, locked.ValidUntil) {
					l.ValidUntil = until
					cols = append(cols, "valid_until")
					newValidUntil = "cleared"
					if until != nil {
						newValidUntil = until.Format(time.RFC3339)
					}
				}
			}
		}

		if grantsPeriod {
			// Granting a period here is an issuance like any other:
			// every replica must understand it, the product's feeds
			// must require the key, and what the public feed handed
			// out before the gate went up must have expired.
			if problem, err := h.maintenanceSwitchOff(ctx, tx); err != nil || problem != nil {
				return firstNonNil(err, problem)
			}
			problem, err := h.lockFeedGateIn(ctx, tx, locked.ProductID)
			if err != nil {
				return err
			}
			if problem != nil {
				return problem
			}
		}
		if err := update(ctx, tx, l, cols...); err != nil {
			return err
		}
		// The subscription row is part of the licence's shape, not a
		// separate record to drift from it: issuance writes one for
		// trial and subscription plans, and nothing downstream would
		// correct it — the hourly sync only repairs expired, canceled
		// and revoked, so a licence that left a trial would read
		// "active" here and "trialing" there for good.
		effectiveStatus := locked.Status
		if newStatus != "" {
			effectiveStatus = newStatus
		}
		// The deadline the licence actually carries after this write:
		// the re-shape above sets it only when the plan's type
		// changed, and the subscription's trial window is derived
		// from it rather than reckoned again.
		effectiveUntil := locked.ValidUntil
		if slices.Contains(cols, "valid_until") {
			effectiveUntil = l.ValidUntil
		}
		return store.SyncLicenseSubscriptionIn(ctx, tx, l.ID, newPlan, effectiveStatus, effectiveUntil)
	})
	if err != nil {
		if errors.Is(err, errStripeBilledPlanChange) {
			// Says what to do next, in order: the portal moves the
			// subscription in Stripe as well, and cancelling alone is
			// not enough — the link stays until it is cut, which is
			// what Unlink does once Stripe confirms the subscription
			// is over.
			response.Conflict(c, "STRIPE_MANAGED",
				"this license is billed by a Stripe subscription: change the plan from the customer portal, which moves the subscription in Stripe too — or cancel the subscription in Stripe and then use Unlink on this license", nil)
			return
		}
		if feedGateRefused(c, err) {
			return
		}
		if feedGateConflict(c, err) {
			return
		}
		response.Internal(c)
		return
	}
	if moved {
		response.Conflict(c, "LICENSE_CHANGED",
			"the license moved to another plan while you were editing it; reload and try again", nil)
		return
	}

	h.Store.Audit(c, &model.AuditLog{
		Entity: "license", EntityID: id, Action: "plan_changed",
		ActorType: "admin", ActorID: adminID(c),
		Changes: changesWith(changesWith(
			map[string]any{"old_plan_id": oldPlanID, "new_plan_id": req.PlanID},
			"status", newStatus), "valid_until", newValidUntil),
	})

	if h.Webhook != nil {
		h.Webhook.Dispatch(c, l.ProductID, "plan.changed", map[string]any{
			"license_id": id, "old_plan_id": oldPlanID, "new_plan_id": req.PlanID,
		})
	}

	if h.Email != nil && h.Email.IsConfigured() && l.Email != "" {
		productName := ""
		if prod, perr := h.Store.FindProductByID(c, l.ProductID); perr == nil {
			productName = prod.Name
		}
		h.Email.SendPlanChanged(l.Email, productName, oldPlan.Name, plan.Name)
	}

	response.OK(c, gin.H{"status": "plan_changed", "plan_id": req.PlanID})
}

// ─── Settings ───

// settingsWritable lists the keys the dashboard may write. Anything
// else is rejected so a typo can't quietly create a dead row.
var settingsWritable = map[string]bool{
	"site_name": true, "timezone": true, "language": true, "brand_color": true, "logo_url": true,
	"signup_mode":    true,
	"rate_limit_api": true, "rate_limit_admin": true,
	"webhook_max_attempts": true, "webhook_timeout": true,
	"quota_warning_threshold":          true,
	"setup_complete":                   true,
	"maintenance_features_enabled":     true,
	"feed_url_ttl_bound":               true,
	"email_template_license_created":   true,
	"email_template_license_expiring":  true,
	"email_template_updates_ending":    true,
	"email_template_license_expired":   true,
	"email_template_trial_expired":     true,
	"email_template_license_suspended": true,
	"email_template_quota_warning":     true,
	"email_template_seat_invite":       true,
	"email_template_payment_failed":    true,
}

// settingsSecret lists keys that can be written but never read back.
// GetSettings reports only whether a value is stored; a blank
// submission means "leave it alone", because a form that cannot show
// the current value would otherwise wipe it on every save.
//
// Currently empty. SMTP used to live here, but the mailer only ever
// read the environment, so the stored values were dead — the keys
// were removed rather than wired up, and SMTP stays env-only like
// the rest of the server config. The mechanism is kept for the next
// secret that genuinely needs to be set from the dashboard.
var settingsSecret = map[string]bool{}

// settingsEnum lists the settings whose value is a fixed choice rather
// than free text. Saving a typo here fails quietly and in the unsafe
// direction: signupAllowed reads anything that is not exactly
// "licensed_only" as open registration, so "licensedonly" would report
// saved while leaving the endpoint open to strangers.
var settingsEnum = map[string][]string{
	"signup_mode": {"open", "licensed_only"},
}

// settingsServerOwned lists keys the server writes for itself — the
// Stripe webhook auto-setup stores the endpoint id and signing secret
// here. They are neither readable nor writable over the API.
//
// Leaving them in the GET response also broke saving outright: the
// dashboard seeds its form from that response and PUTs the whole map
// back, so once Stripe was configured every save failed with
// "unknown setting: stripe_webhook_secret".
var settingsServerOwned = map[string]bool{
	"stripe_webhook_secret":                true,
	"stripe_webhook_endpoint_id":           true,
	"stripe_webhook_secret_previous":       true,
	"stripe_webhook_secret_previous_until": true,
	"stripe_webhook_retire_endpoint_id":    true,
}

func (h *AdminHandler) GetSettings(c *gin.Context) {
	settings, err := h.Store.GetSettings(c)
	if err != nil {
		response.Internal(c)
		return
	}

	// An admin-scoped API key reaches this endpoint too, so the
	// response must not double as a way to read secrets back out of
	// the database.
	out := make(map[string]string, len(settings))
	secretsSet := make(map[string]bool, len(settingsSecret))
	for k := range settingsSecret {
		secretsSet[k] = false
	}
	for k, v := range settings {
		switch {
		case settingsServerOwned[k]:
			continue
		case settingsSecret[k]:
			secretsSet[k] = v != ""
		default:
			out[k] = v
		}
	}

	// SMTP is configured from the environment; the dashboard shows
	// where mail goes so an admin can tell "not set up" from "set up
	// but broken" before pressing the test button.
	email := gin.H{"configured": false, "host": "", "from": ""}
	if h.Email != nil {
		email = gin.H{"configured": h.Email.IsConfigured(), "host": h.Email.Host(), "from": h.Email.From()}
	}
	response.OK(c, gin.H{"settings": out, "secrets_set": secretsSet, "email": email})
}

func (h *AdminHandler) UpdateSettings(c *gin.Context) {
	var req struct {
		Settings map[string]string `json:"settings" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "settings map is required")
		return
	}

	for key, val := range req.Settings {
		if !settingsWritable[key] {
			response.BadRequest(c, "unknown setting: "+key)
			return
		}
		if allowed, ok := settingsEnum[key]; ok && !slices.Contains(allowed, val) {
			response.BadRequest(c, "invalid value for "+key+": "+val)
			return
		}
	}

	// Blank secret means "unchanged", so an ordinary form save doesn't
	// wipe a password the form was never able to display. Removing one
	// is a separate, explicit call — see ClearSecretSetting.
	writes := make(map[string]string, len(req.Settings))
	for key, val := range req.Settings {
		if settingsSecret[key] && val == "" {
			continue
		}
		writes[key] = val
	}
	if len(writes) == 0 {
		response.OK(c, gin.H{"status": "saved"})
		return
	}
	// The wait a gated feed imposes is measured against this, so a
	// value that does not parse — or one that is zero, negative or
	// absurd — would end the wait early or never. Lowering it is
	// allowed: it is how an operator says the longer-lived links are
	// gone, after every replica has moved to the shorter TTL.
	if v, ok := writes[store.SettingFeedURLTTLBound]; ok {
		d, perr := store.ParseDurationSetting(store.SettingFeedURLTTLBound, v)
		if perr != nil {
			response.BadRequest(c, store.SettingFeedURLTTLBound+" must be a positive duration of at most "+
				store.MaxDurationSetting.String()+", such as 24h0m0s: the wait before an update period may be sold is measured against it")
			return
		}
		writes[store.SettingFeedURLTTLBound] = d.String()
	}

	// A custom email template that does not parse is not rejected by
	// anything downstream: the renderer gives up and mails the
	// template source, so the customer receives "{{.LicenseKey}}"
	// where their key should be. Empty means "back to the default".
	for key, value := range writes {
		if !strings.HasPrefix(key, "email_template_") || value == "" {
			continue
		}
		if _, err := template.New(key).Parse(value); err != nil {
			response.BadRequest(c, key+" is not a valid template: "+err.Error())
			return
		}
	}

	if err := h.Store.SetSettings(c, writes); err != nil {
		response.Internal(c)
		return
	}

	keys := make([]string, 0, len(writes))
	for k := range writes {
		keys = append(keys, k)
	}
	h.Store.Audit(c, &model.AuditLog{
		Entity: "settings", EntityID: "system", Action: "updated",
		ActorType: "admin", ActorID: adminID(c),
		Changes: map[string]any{"keys": keys},
	})

	response.OK(c, gin.H{"status": "saved"})
}

// ClearSecretSetting removes a stored secret. UpdateSettings treats a
// blank value as "leave it alone" — otherwise every save from a form
// that cannot display the current value would wipe it — so deleting one
// needs its own call. Restricted to settingsSecret: the server-owned
// Stripe keys must not be clearable, since dropping the signing secret
// would silently break webhook verification.
func (h *AdminHandler) ClearSecretSetting(c *gin.Context) {
	key := c.Param("key")
	if !settingsSecret[key] {
		response.NotFound(c, "no such secret setting")
		return
	}

	if err := h.Store.DeleteSetting(c, key); err != nil {
		response.Internal(c)
		return
	}

	h.Store.Audit(c, &model.AuditLog{
		Entity: "settings", EntityID: "system", Action: "secret_cleared",
		ActorType: "admin", ActorID: adminID(c),
		Changes: map[string]any{"key": key},
	})

	response.OK(c, gin.H{"status": "cleared", "key": key})
}

// RunMeteredSync triggers one drain pass of the Stripe meter-event
// queue on demand. Useful for operators who just attached an
// entitlement to a Stripe meter and want to ship the backlog
// immediately, and for end-to-end tests that don't want to wait
// for the 5-minute loop.
func (h *AdminHandler) RunMeteredSync(c *gin.Context) {
	if h.Metered == nil {
		response.Err(c, http.StatusServiceUnavailable, "METERED_SYNC_NOT_AVAILABLE",
			"metered billing syncer is not wired on this server")
		return
	}
	pushed := h.Metered.RunOnce(c.Request.Context(), 200)
	response.OK(c, gin.H{"pushed": pushed})
}

// RunExpiryChecks triggers one full pass of the lifecycle checker on
// demand. Useful for admins (apply new grace_days immediately, debug
// "did expiry mailer run last night?") and for end-to-end tests of the
// expiring / dunning / renewal email paths that are otherwise on an
// hourly cron.
func (h *AdminHandler) RunExpiryChecks(c *gin.Context) {
	if h.Expiry == nil {
		response.Err(c, http.StatusServiceUnavailable, "EXPIRY_NOT_AVAILABLE",
			"expiry checker is not wired on this server")
		return
	}
	h.Expiry.RunAll(c.Request.Context())
	response.OK(c, gin.H{"status": "ran"})
}

// SendTestEmail sends a real test message through the configured SMTP
// server so the admin can verify host/port/credentials end-to-end.
// Body: { "to": "user@example.com" } — optional; defaults to the
// logged-in admin's own email address.
func (h *AdminHandler) SendTestEmail(c *gin.Context) {
	if h.Email == nil || !h.Email.IsConfigured() {
		response.Err(c, http.StatusServiceUnavailable, "EMAIL_NOT_CONFIGURED",
			"SMTP is not configured on this server — set SMTP_HOST / SMTP_FROM / etc.")
		return
	}
	var req struct {
		To string `json:"to"`
	}
	_ = c.ShouldBindJSON(&req) // body optional
	to := strings.TrimSpace(req.To)
	if to == "" {
		if v, ok := c.Get("email"); ok {
			to, _ = v.(string)
		}
	}
	if err := apperr.ValidateEmail(to); err != nil {
		response.BadRequest(c, err.Message)
		return
	}
	if err := h.Email.Send(to,
		"Keygate test email",
		`<p>Hello! This is a test email from Keygate.</p>`+
			`<p>If you can read this, your SMTP setup is working.</p>`); err != nil {
		response.Err(c, http.StatusBadGateway, "EMAIL_SEND_FAILED", err.Error())
		return
	}
	response.OK(c, gin.H{"status": "sent", "to": to})
}

// GetEmailTemplates returns all email templates (custom from DB + hardcoded defaults).
func (h *AdminHandler) GetEmailTemplates(c *gin.Context) {
	defaults := service.DefaultTemplates()

	type templateInfo struct {
		Custom  string `json:"custom"`
		Default string `json:"default"`
	}

	result := make(map[string]templateInfo, len(defaults))
	for key, def := range defaults {
		custom, _ := h.Store.GetSetting(c, "email_template_"+key)
		result[key] = templateInfo{Custom: custom, Default: def}
	}

	response.OK(c, gin.H{"templates": result})
}

// ─── Team (Admin Management) ───

// ListTeamMembers returns all platform admins (owner + admin roles).
func (h *AdminHandler) ListTeamMembers(c *gin.Context) {
	page := listPage(c)
	admins, total, err := h.Store.ListAdmins(c, page)
	if err != nil {
		response.Internal(c)
		return
	}
	listOK(c, "members", admins, total, page)
}

// InviteTeamMember promotes an existing user to admin, or creates a placeholder admin user.
// Only owners can invite new admins.
func (h *AdminHandler) InviteTeamMember(c *gin.Context) {
	// Only owner can manage team
	actorEmail, ok := c.Get("email")
	if !ok {
		response.Unauthorized(c, "unauthorized")
		return
	}
	actorUser, err := h.Store.FindUserByEmail(c, actorEmail.(string))
	if err != nil || actorUser.Role != model.RoleOwner {
		response.Forbidden(c, "only the owner can manage team members")
		return
	}

	var req struct {
		Email string `json:"email" binding:"required"`
		Role  string `json:"role"` // "admin" (default) or "owner"
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "email is required")
		return
	}

	// Normalize before validation + lookups so case variants don't
	// create duplicate user rows or skip the self-invite guard.
	req.Email = strings.ToLower(strings.TrimSpace(req.Email))
	if appErr := apperr.ValidateEmail(req.Email); appErr != nil {
		response.BadRequest(c, appErr.Message)
		return
	}

	role := req.Role
	if role == "" {
		role = model.RoleAdmin
	}
	if role != model.RoleAdmin && role != model.RoleOwner {
		response.BadRequest(c, "role must be 'admin' or 'owner'")
		return
	}

	// Cannot invite yourself. EqualFold so "Owner@x.com" still trips
	// the guard even if the actor row was stored mixed-case.
	if strings.EqualFold(req.Email, actorUser.Email) {
		response.BadRequest(c, "cannot change your own role via invite")
		return
	}

	// Find or create user. Track whether THIS request changed
	// anything — if not, skip the notification email to avoid
	// spamming the invitee on repeated owner clicks.
	roleChanged := false
	user, err := h.Store.FindUserByEmail(c, req.Email)
	if err != nil {
		// User doesn't exist yet — create placeholder (will get proper name on first login)
		if err := h.Store.CreatePlaceholderUser(c, req.Email, role); err != nil {
			response.Internal(c)
			return
		}
		user, _ = h.Store.FindUserByEmail(c, req.Email)
		roleChanged = true
	} else {
		// User exists — check if already same role (idempotent)
		if user.Role == role {
			response.OK(c, user)
			return
		}
		if err := h.Store.SetUserRole(c, user.ID, role); err != nil {
			response.Internal(c)
			return
		}
		user.Role = role
		roleChanged = true
	}

	h.Store.Audit(c, &model.AuditLog{
		Entity: "team", EntityID: user.ID, Action: "member_invited",
		ActorType: "admin", ActorID: adminID(c),
		Changes: map[string]any{"email": req.Email, "role": role},
	})

	// Notify the new admin via email. Best-effort: a send failure
	// does NOT roll back the role grant (OTP login still works
	// out-of-band). The inviter (actor) name is shown so the
	// recipient knows who added them, which helps spot social
	// engineering ("why am I suddenly admin on Keygate?").
	if roleChanged && h.Email != nil && h.Email.IsConfigured() {
		siteName, _ := h.Store.GetSetting(c, "site_name")
		if siteName == "" {
			siteName = "Keygate"
		}
		baseURL, _ := h.Store.GetSetting(c, "base_url")
		if baseURL == "" {
			baseURL = adminBaseURL(c)
		}
		loginURL := strings.TrimRight(baseURL, "/") + "/login"
		inviter := actorUser.Name
		if inviter == "" {
			inviter = actorUser.Email
		}
		h.Email.SendAdminInvite(req.Email, siteName, inviter, role, loginURL)
	}

	response.OK(c, user)
}

// adminBaseURL infers the public base URL from the inbound request
// when the settings table doesn't have an explicit BASE_URL. Used
// only as a fallback for the admin invite email's login link.
func adminBaseURL(c *gin.Context) string {
	scheme := "http"
	if c.Request.TLS != nil || c.GetHeader("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	host := c.Request.Host
	if forwarded := c.GetHeader("X-Forwarded-Host"); forwarded != "" {
		host = forwarded
	}
	return scheme + "://" + host
}

// RemoveTeamMember demotes an admin back to regular user.
// Only owners can remove admins. Cannot remove the last owner.
func (h *AdminHandler) RemoveTeamMember(c *gin.Context) {
	actorEmail, ok := c.Get("email")
	if !ok {
		response.Unauthorized(c, "unauthorized")
		return
	}
	actorUser, err := h.Store.FindUserByEmail(c, actorEmail.(string))
	if err != nil || actorUser.Role != model.RoleOwner {
		response.Forbidden(c, "only the owner can manage team members")
		return
	}

	targetID := c.Param("id")

	// Can't remove yourself
	if targetID == actorUser.ID {
		response.BadRequest(c, "cannot remove yourself from the team")
		return
	}

	target, err := h.Store.FindUserByID(c, targetID)
	if err != nil {
		response.NotFound(c, "user not found")
		return
	}

	if !target.IsAdmin() {
		response.BadRequest(c, "user is not a team member")
		return
	}

	// Atomic demote: a SELECT … FOR UPDATE inside DemoteOwnerAtomic
	// serialises concurrent demotion attempts so the last-owner
	// invariant holds even under racing requests. The non-atomic
	// "count then update" pattern that lived here previously could
	// be tricked into zero owners by two simultaneous calls.
	if err := h.Store.DemoteOwnerAtomic(c, targetID); err != nil {
		if errors.Is(err, store.ErrLastOwner) {
			response.BadRequest(c, "cannot remove the last owner")
			return
		}
		response.Internal(c)
		return
	}

	h.Store.Audit(c, &model.AuditLog{
		Entity: "team", EntityID: targetID, Action: "member_removed",
		ActorType: "admin", ActorID: adminID(c),
		Changes: map[string]any{"email": target.Email, "previous_role": target.Role},
	})

	response.OK(c, gin.H{"status": "removed"})
}

// ExportLicenses exports all licenses as CSV or JSON.
// GET /api/v1/admin/licenses/export?format=csv&product_id=xxx&status=xxx
func (h *AdminHandler) ExportLicenses(c *gin.Context) {
	format := c.DefaultQuery("format", "csv")
	if format != "csv" && format != "json" {
		response.BadRequest(c, "format must be csv or json")
		return
	}

	licenses, err := h.Store.ExportLicenses(c, c.Query("product_id"), c.Query("status"))
	if err != nil {
		response.Internal(c)
		return
	}

	// The export deliberately carries plaintext keys — that is its
	// purpose, e.g. migrating off Keygate. But it is a bulk credential
	// dump, so unlike a single reveal it must leave a trace of who
	// pulled it and how much.
	h.Store.Audit(c, &model.AuditLog{
		Entity: "license", EntityID: "export", Action: "keys_exported",
		ActorType: "admin", ActorID: adminID(c), IPAddress: c.ClientIP(),
		Changes: map[string]any{
			"format": format, "count": len(licenses),
			"product_id": c.Query("product_id"), "status": c.Query("status"),
		},
	})

	dateStr := time.Now().Format("2006-01-02")

	if format == "json" {
		filename := fmt.Sprintf("licenses-%s.json", dateStr)
		c.Header("Content-Type", "application/json")
		c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%s", filename))

		type exportLicense struct {
			ID         string `json:"id"`
			Email      string `json:"email"`
			Product    string `json:"product"`
			Plan       string `json:"plan"`
			Status     string `json:"status"`
			LicenseKey string `json:"license_key"`
			ValidFrom  string `json:"valid_from"`
			ValidUntil string `json:"valid_until"`
			// UpdatesUntil is the end of a perpetual license's
			// maintenance period; empty means no separate limit. The
			// export exists to migrate off Keygate, so an entitlement
			// the license actually has must travel with it.
			UpdatesUntil string `json:"updates_until"`
			CreatedAt    string `json:"created_at"`
		}

		out := make([]exportLicense, 0, len(licenses))
		for _, l := range licenses {
			productName := ""
			if l.Product != nil {
				productName = l.Product.Name
			}
			planName := ""
			if l.Plan != nil {
				planName = l.Plan.Name
			}
			validUntil := ""
			if l.ValidUntil != nil {
				validUntil = l.ValidUntil.Format(time.RFC3339)
			}
			updatesUntil := ""
			if u := l.EffectiveUpdatesUntil(); u != nil {
				updatesUntil = u.Format(time.RFC3339)
			}
			out = append(out, exportLicense{
				ID:           l.ID,
				Email:        l.Email,
				Product:      productName,
				Plan:         planName,
				Status:       l.Status,
				LicenseKey:   h.Store.DecryptLicenseKey(l),
				ValidFrom:    l.ValidFrom.Format(time.RFC3339),
				ValidUntil:   validUntil,
				UpdatesUntil: updatesUntil,
				CreatedAt:    l.CreatedAt.Format(time.RFC3339),
			})
		}

		enc := json.NewEncoder(c.Writer)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
			slog.Error("export json encode", "error", err)
		}
		return
	}

	// CSV format
	filename := fmt.Sprintf("licenses-%s.csv", dateStr)
	c.Header("Content-Type", "text/csv")
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%s", filename))

	w := csv.NewWriter(c.Writer)
	defer w.Flush()

	_ = w.Write([]string{"id", "email", "product", "plan", "status", "license_key", "valid_from", "valid_until", "updates_until", "created_at"})

	for _, l := range licenses {
		productName := ""
		if l.Product != nil {
			productName = l.Product.Name
		}
		planName := ""
		if l.Plan != nil {
			planName = l.Plan.Name
		}
		validUntil := ""
		if l.ValidUntil != nil {
			validUntil = l.ValidUntil.Format(time.RFC3339)
		}
		updatesUntil := ""
		if u := l.EffectiveUpdatesUntil(); u != nil {
			updatesUntil = u.Format(time.RFC3339)
		}
		_ = w.Write(csvSafeRow([]string{
			l.ID,
			l.Email,
			productName,
			planName,
			l.Status,
			h.Store.DecryptLicenseKey(l),
			l.ValidFrom.Format(time.RFC3339),
			validUntil,
			updatesUntil,
			l.CreatedAt.Format(time.RFC3339),
		}))
	}
}

// csvSafeRow neutralises cells a spreadsheet would run as a formula.
// Email, product and plan names are user-controlled, and "+foo@x.com"
// is a perfectly valid address that Excel treats as =+foo@x.com. A
// leading apostrophe makes the cell literal text.
func csvSafeRow(cells []string) []string {
	for i, c := range cells {
		if c == "" {
			continue
		}
		switch c[0] {
		case '=', '+', '-', '@', '\t', '\r':
			cells[i] = "'" + c
		}
	}
	return cells
}
