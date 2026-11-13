package model

import (
	"slices"
	"time"

	"github.com/uptrace/bun"
)

// ─── User ───

type User struct {
	bun.BaseModel `bun:"table:users"`

	ID        string    `bun:",pk" json:"id"`
	Email     string    `bun:",notnull,unique" json:"email"`
	Name      string    `json:"name"`
	AvatarURL string    `json:"avatar_url,omitempty"`
	Role      string    `bun:",notnull,default:'user'" json:"role"` // owner | admin | user
	CreatedAt time.Time `bun:",nullzero,default:now()" json:"created_at"`
	UpdatedAt time.Time `bun:",nullzero,default:now()" json:"updated_at"`
}

// IsAdmin returns true if the user has admin or owner role.
func (u *User) IsAdmin() bool {
	return u.Role == "owner" || u.Role == "admin"
}

const (
	RoleOwner = "owner"
	RoleAdmin = "admin"
	RoleUser  = "user"
)

type OAuthAccount struct {
	bun.BaseModel `bun:"table:oauth_accounts"`

	ID         string    `bun:",pk" json:"id"`
	UserID     string    `bun:",notnull" json:"user_id"`
	Provider   string    `bun:",notnull" json:"provider"`
	ProviderID string    `bun:",notnull" json:"provider_id"`
	Email      string    `json:"email,omitempty"`
	CreatedAt  time.Time `bun:",nullzero,default:now()" json:"created_at"`
}

type OTPCode struct {
	bun.BaseModel `bun:"table:otp_codes"`

	ID        string    `bun:",pk" json:"id"`
	Email     string    `bun:",notnull" json:"email"`
	CodeHash  string    `bun:",notnull" json:"-"`
	Attempts  int       `bun:",notnull,default:0" json:"attempts"`
	ExpiresAt time.Time `bun:",notnull" json:"expires_at"`
	Used      bool      `bun:",notnull,default:false" json:"used"`
	CreatedAt time.Time `bun:",nullzero,default:now()" json:"created_at"`
}

// ─── Product ───

// Product types — drive capability gating.
//
// The product type decides WHAT CAPABILITIES the product exposes
// (activations / seats / release feeds) — not the commercial model.
// `plan.license_type` (perpetual/subscription/trial) and
// `plan.license_model` (standard/floating) stay independent so a
// desktop product can ship under subscription pricing, and a SaaS
// product can ship under a lifetime perpetual deal.
const (
	ProductTypeDesktop = "desktop"
	ProductTypeSaaS    = "saas"
	ProductTypeHybrid  = "hybrid"
)

// Product capability identifiers. Used by ProductSupports to drive
// runtime guards in service / handler layers.
const (
	// CapActivations: per-device license activation (POST /license/activate
	// /verify/deactivate + floating sessions).
	CapActivations = "activations"
	// CapSeats: per-user seat management. Mutation endpoints live
	// under /portal/seats/* (session-authed) and /invites/accept
	// (public, token-only) — NOT on the SDK /license/* namespace.
	CapSeats = "seats"
	// CapReleases: software update feeds + admin release management.
	CapReleases = "releases"
)

// IsValidProductType reports whether t is one of the three accepted
// product types.
func IsValidProductType(t string) bool {
	return t == ProductTypeDesktop || t == ProductTypeSaaS || t == ProductTypeHybrid
}

// ProductSupports reports whether a product of type `t` exposes the
// given capability. Centralised so service-layer guards and openapi
// docs stay in sync. Note: usage / entitlements / quotas are
// universally available and intentionally not gated here — those
// are billing primitives that any product type may need.
func ProductSupports(t, capability string) bool {
	switch t {
	case ProductTypeDesktop:
		// Local installs: device activations + auto-update feeds.
		// No multi-user seat concept (one customer = one or more devices).
		return capability == CapActivations || capability == CapReleases
	case ProductTypeSaaS:
		// Hosted service: per-user seats. No client binary to update
		// (operator deploys), no per-device activation.
		return capability == CapSeats
	case ProductTypeHybrid:
		// Cloud + installable client: both activation AND seat models
		// matter, plus auto-updates for the installed binary.
		return capability == CapActivations || capability == CapSeats || capability == CapReleases
	}
	return false
}

// ProductTypesSupporting lists the product types that have capability
// (see ProductSupports), for checks made in SQL.
func ProductTypesSupporting(capability string) []string {
	var out []string
	for _, t := range []string{ProductTypeDesktop, ProductTypeSaaS, ProductTypeHybrid} {
		if ProductSupports(t, capability) {
			out = append(out, t)
		}
	}
	return out
}

type Product struct {
	bun.BaseModel `bun:"table:products"`

	ID   string `bun:",pk" json:"id"`
	Name string `bun:",notnull" json:"name"`
	Slug string `bun:",notnull,unique" json:"slug"`
	Type string `bun:",notnull" json:"type"`

	// LicensePrefix is the first segment of the keys this product
	// issues, 2 to 8 uppercase letters or digits. Changing it applies
	// to new keys only: keys are found by their whole value, so the
	// ones already issued keep their prefix and keep working.
	LicensePrefix string `bun:",notnull,default:'KG'" json:"license_prefix"`

	// MinimumSupportedVersion: optional semver floor. When non-empty,
	// auto-update feeds embed this so clients can refuse to keep
	// running an installed build older than the floor — the simple
	// "force upgrade" knob without staged rollout machinery.
	MinimumSupportedVersion string `bun:",notnull,default:''" json:"minimum_supported_version,omitempty"`

	// MinimumSupportedMessage: human-readable note shown alongside the
	// forced-upgrade prompt (e.g. "old TLS protocol no longer accepted").
	MinimumSupportedMessage string `bun:",notnull,default:''" json:"minimum_supported_message,omitempty"`
	// DownloadURL is the vendor's own page where customers download the
	// product, offered to email templates as {{.DownloadURL}}. A stable
	// page, never a signed file link: those expire, can be forwarded,
	// and skip the license check. Empty when not set.
	DownloadURL string `bun:",notnull,default:''" json:"download_url"`

	// RequireSigning: when true (the safe default for new products),
	// publishing a release fails if no active signing key is configured.
	// Flip to false only for products that intentionally ship unsigned
	// builds (CI test artifacts, internal tooling).
	RequireSigning bool `bun:",notnull,default:true" json:"require_signing"`
	// FeedLicenseRequired: the update feeds answer only when the
	// updater sends its license key, so a maintenance period cannot
	// be bypassed by asking for the public feed. Turn on for products
	// sold with a maintenance period, once the updater ships the key.
	FeedLicenseRequired bool `bun:",notnull,default:false" json:"feed_license_required"`
	// FeedGatedAt is when FeedLicenseRequired was last switched on.
	// The public feed is cacheable for a short while; a bounded period
	// is refused until that cache has drained.
	FeedGatedAt *time.Time `json:"feed_gated_at,omitempty"`

	CreatedAt time.Time `bun:",nullzero,default:now()" json:"created_at"`
}

// ─── API Key (programmatic credential, server-to-server) ───
//
// ProductID is OPTIONAL: when empty/null the key is system-wide
// (operator scripts, cross-product migrations); when set, it's bound
// to that specific product (future per-product s2s integration).
// Scopes drive what the key can actually do — see ScopeAdmin etc.
// Empty scopes = the key can do nothing (fail-closed).
type APIKey struct {
	bun.BaseModel `bun:"table:api_keys"`

	ID         string     `bun:",pk" json:"id"`
	ProductID  string     `bun:",nullzero" json:"product_id,omitempty"`
	Name       string     `bun:",notnull" json:"name"`
	KeyHash    string     `bun:",notnull,unique" json:"-"`
	Prefix     string     `bun:",notnull" json:"prefix"`
	Scopes     []string   `bun:",array" json:"scopes"`
	LastUsed   *time.Time `json:"last_used,omitempty"`
	LastUsedIP string     `bun:",notnull,default:''" json:"last_used_ip,omitempty"`
	CreatedAt  time.Time  `bun:",nullzero,default:now()" json:"created_at"`

	Product *Product `bun:"rel:belongs-to,join:product_id=id" json:"product,omitempty"`
}

// Scope vocabulary.
//
// admin is the wildcard — equivalent to a logged-in admin session,
// matches every admin route. The narrower scopes exist so a CI/CD
// runner or merchant backend can mint a key that's only allowed to
// do its specific job, limiting blast radius if leaked.
//
// We resist the urge to pre-emptively add every read/write split.
// Real customer asks decide what gets added (e.g. usage:write,
// licenses:read). Keep this list short and meaningful.
const (
	ScopeAdmin         = "admin"
	ScopeLicensesWrite = "licenses:write"
	ScopeReleasesWrite = "releases:write"
)

// AllScopes is the closed enumeration used to validate
// CreateAPIKey / RotateAPIKey requests. Unknown scope strings are
// rejected at the boundary so a typo doesn't silently become a
// useless key.
func AllScopes() []string {
	return []string{ScopeAdmin, ScopeLicensesWrite, ScopeReleasesWrite}
}

// IsValidScope reports whether s is a known scope.
func IsValidScope(s string) bool {
	return slices.Contains(AllScopes(), s)
}

func (a *APIKey) GetID() string       { return a.ID }
func (a *APIKey) GetScopes() []string { return a.Scopes }

// HasScope returns true if the key carries the given scope OR the
// admin wildcard. Centralizing this means RequireScope, audit logs,
// and the admin UI all answer the same question the same way.
func (a *APIKey) HasScope(scope string) bool {
	if a == nil {
		return false
	}
	for _, s := range a.Scopes {
		if s == ScopeAdmin || s == scope {
			return true
		}
	}
	return false
}

// ─── Plan ───

type Plan struct {
	bun.BaseModel `bun:"table:plans"`

	ID              string `bun:",pk" json:"id"`
	ProductID       string `bun:",notnull" json:"product_id"`
	Name            string `bun:",notnull" json:"name"`
	Slug            string `bun:",notnull" json:"slug"`
	CheckoutID      string `bun:",unique,notnull" json:"checkout_id"`
	LicenseType     string `bun:",notnull" json:"license_type"`
	BillingInterval string `json:"billing_interval,omitempty"`
	// IMPORTANT: do NOT add bun `default:N` annotations here. Bun
	// translates a zero Go value on a `default:N` field into SQL
	// DEFAULT — which means a deliberate `0` from the handler ends up
	// stored as the column default (e.g. 3). The DB column keeps its
	// CREATE-TABLE default for legacy data, but the handler is now the
	// sole source of truth for these values.
	MaxActivations int    `bun:",notnull" json:"max_activations"`
	TrialDays      int    `bun:",notnull" json:"trial_days"`
	GraceDays      int    `bun:",notnull" json:"grace_days"`
	StripePriceID  string `json:"stripe_price_id,omitempty"`
	// Maintenance period, perpetual plans only. UpdatesDays is how
	// long a purchase includes updates for (0 = for life). A renewal
	// is a one-time purchase of RenewalDays more, sold at
	// StripeRenewalPriceID; both empty means renewals are not offered.
	UpdatesDays          int    `bun:",notnull" json:"updates_days"`
	RenewalDays          int    `bun:",notnull" json:"renewal_days"`
	StripeRenewalPriceID string `bun:",notnull" json:"stripe_renewal_price_id,omitempty"`
	LicenseModel         string `bun:",notnull" json:"license_model"` // standard | floating
	FloatingTimeout      int    `bun:",notnull" json:"floating_timeout"`
	// TokenTTLDays is how long a signed token may be used offline
	// before the client must verify again. 0 = server default.
	TokenTTLDays int `bun:",notnull" json:"token_ttl_days"`
	// SelfSignup lets a visitor claim a licence on this plan from its
	// /pay link by confirming an email address. Only for plans with no
	// Stripe price and no update period. SignupDailyCap limits claims
	// per UTC day (0 = no cap); SignupConsentText, when set, is the
	// label of an optional marketing-consent checkbox on the form.
	SelfSignup        bool   `bun:",notnull" json:"self_signup"`
	SignupDailyCap    int    `bun:",notnull" json:"signup_daily_cap"`
	SignupConsentText string `bun:",notnull" json:"signup_consent_text"`
	// MaxSeats is how many people a license holds, the owner included:
	// 5 is the owner and four members. 0 = no limit. A plan sold per
	// seat does not use it (see Limits); it keeps 1 there, or 0 on a
	// product without seats, so a replica that predates PerSeat holds
	// its licences to the owner alone rather than to nobody's limit.
	MaxSeats int `bun:",notnull" json:"max_seats"`
	// PerSeat sells the plan by the seat: the buyer pays for a quantity
	// between MinQuantity and MaxQuantity, and MaxActivations is per
	// seat. Subscriptions only. A zero bound is never meant, so the
	// column default stands in for it on insert.
	PerSeat     bool      `bun:",notnull" json:"per_seat"`
	MinQuantity int       `bun:",notnull,default:1" json:"min_quantity"`
	MaxQuantity int       `bun:",notnull,default:100" json:"max_quantity"`
	Active      bool      `bun:",notnull,default:true" json:"active"`
	SortOrder   int       `bun:",default:0" json:"sort_order"`
	CreatedAt   time.Time `bun:",nullzero,default:now()" json:"created_at"`

	// TrialFallbackPlanID, on a trial plan, is the plan a licence
	// moves to when its trial ends, in place of expiring: same key,
	// same activations, now on (say) the free tier. Empty = the trial
	// expires. Only a free plan of the same product, not a trial and
	// not sold per seat, may be named (TrialFallbackRefusal); see
	// ExpiryChecker.ExpireTrials.
	TrialFallbackPlanID string `bun:",nullzero" json:"trial_fallback_plan_id,omitempty"`

	Product      *Product       `bun:"rel:belongs-to,join:product_id=id" json:"product,omitempty"`
	Entitlements []*Entitlement `bun:"rel:has-many,join:id=plan_id" json:"entitlements,omitempty"`
}

// MaxPlanQuantity is the most seats a per-seat plan may sell.
const MaxPlanQuantity = 10000

// Limits are how many people and how many devices (or floating
// sessions) a licence on this plan holds when it is paid for quantity
// seats. On a plan sold per seat the people are the seats and the
// devices are MaxActivations for each; on any other plan they are the
// plan's own, whatever the quantity. 0 seats means no limit, as on the
// plan; 0 devices means none.
func (p *Plan) Limits(quantity int) (seats, activations int) {
	if !p.PerSeat {
		return p.MaxSeats, p.MaxActivations
	}
	if quantity < 1 {
		quantity = 1
	}
	return quantity, p.MaxActivations * quantity
}

// ─── Entitlement (feature flags per plan) ───

type Entitlement struct {
	bun.BaseModel `bun:"table:entitlements"`

	ID          string `bun:",pk" json:"id"`
	PlanID      string `bun:",notnull" json:"plan_id"`
	Feature     string `bun:",notnull" json:"feature"`
	ValueType   string `bun:",notnull" json:"value_type"`
	Value       string `bun:",notnull" json:"value"`
	QuotaPeriod string `bun:",default:''" json:"quota_period,omitempty"`
	QuotaUnit   string `bun:",default:''" json:"quota_unit,omitempty"`
	// StripeMeterEventName: the Stripe Billing Meter event_name to
	// emit on each RecordUsage call. Configured per-meter in the
	// merchant's Stripe dashboard. Empty disables metered sync for
	// the feature (Keygate-internal quota only).
	StripeMeterEventName string `bun:",notnull,default:''" json:"stripe_meter_event_name,omitempty"`
}

// ─── License ───

// FeedPublicMaxAge is how long shared caches may serve a public
// update feed after it was fetched. Gating a product's feeds does not
// reach a copy already cached, so it is one half of the wait before an
// update period may be enforced on that product; the other half is the
// lifetime of the presigned artifact URLs inside those copies.
const FeedPublicMaxAge = 60 * time.Second

type License struct {
	bun.BaseModel `bun:"table:licenses"`

	ID        string `bun:",pk" json:"id"`
	ProductID string `bun:",notnull" json:"product_id"`
	PlanID    string `bun:",notnull" json:"plan_id"`
	UserID    string `bun:",nullzero" json:"user_id,omitempty"`
	Email     string `bun:",notnull" json:"email"`
	// JSON-hidden on purpose. The key is a credential — it activates
	// the product — so it must not ride along in every payload that
	// happens to contain a license. The three places that legitimately
	// hand it out opt in explicitly: the admin reveal endpoint, the
	// create response (the admin just minted it), and the customer
	// portal (it is their own key). Read it via store.DecryptLicenseKey.
	LicenseKey string `bun:",notnull,unique" json:"-"`
	KeyHash    string `bun:",notnull,default:''" json:"-"` // never exposed in API
	// LicenseKeyEncrypted stores the license key encrypted at rest under
	// HKDF("license-key") subkey of the master encryption key. Phase A:
	// new rows have it populated alongside LicenseKey. Phase B will
	// backfill existing rows. Phase C will drop the plaintext column.
	// Always JSON-hidden; decrypt path is store.DecryptLicenseKey.
	LicenseKeyEncrypted []byte `bun:",nullzero" json:"-"`

	PaymentProvider      string `json:"payment_provider,omitempty"`
	StripeCustomerID     string `json:"stripe_customer_id,omitempty"`
	StripeSubscriptionID string `bun:",unique,nullzero" json:"stripe_subscription_id,omitempty"`
	// StripeSyncedAt is the newest read of the Stripe subscription whose
	// answer has been applied to this licence (see
	// store.UpdateLicenseFromSubscriptionRead); an older one is refused.
	StripeSyncedAt *time.Time `bun:"stripe_synced_at" json:"-"`
	// StripePaymentIntentID is the payment intent of the checkout session
	// that created this license. charge.refunded carries the same id, so
	// a refund can be matched to exactly this license.
	StripePaymentIntentID string `bun:",notnull,default:''" json:"stripe_payment_intent_id,omitempty"`
	// StripeCheckoutSessionID is the checkout session that created the
	// license; fulfilment retries use it to see whether the session
	// already produced one.
	StripeCheckoutSessionID string `bun:",notnull,default:''" json:"stripe_checkout_session_id,omitempty"`
	// Quantity is how many seats the license is paid for: for a Stripe
	// subscription the quantity Stripe bills, read back from it (a
	// downgrade writes it just ahead of Stripe, and puts it back if
	// Stripe does not make it). Only a plan sold per seat turns it into
	// limits (see Plan.Limits). Never 0, so the column default stands in
	// for an unset one on insert.
	Quantity int `bun:",notnull,default:1" json:"quantity"`
	// StripePendingUpdateExpiresAt mirrors the subscription's
	// pending_update: a change Stripe applies once its invoice is paid,
	// and drops at this moment otherwise. Nil when nothing waits.
	StripePendingUpdateExpiresAt *time.Time `bun:"stripe_pending_update_expires_at" json:"stripe_pending_update_expires_at,omitempty"`

	Status     string     `bun:",notnull,default:'active'" json:"status"`
	ValidFrom  time.Time  `bun:",notnull,default:now()" json:"valid_from"`
	ValidUntil *time.Time `json:"valid_until,omitempty"`
	// UpdatesUntil ends the maintenance period of a perpetual license:
	// the license stays valid, but releases published after this date
	// cannot be installed. Nil means no separate limit — the license
	// follows ValidUntil, or includes updates for life.
	UpdatesUntil *time.Time `json:"updates_until,omitempty"`
	// UpdatesTermsSet marks a row whose writer decided the period,
	// including deciding there is none. It tells the database trigger
	// apart from a replica that predates the column and simply left
	// it out; see the migration.
	UpdatesTermsSet bool       `bun:",notnull" json:"-"`
	CanceledAt      *time.Time `json:"canceled_at,omitempty"`
	SuspendedAt     *time.Time `json:"suspended_at,omitempty"`
	// SuspendedBy says who suspended it: SuspendedByAdmin, which only an
	// operator lifts, or SuspendedByStripe (a paused subscription), which
	// Stripe lifts by resuming it. Empty when not suspended.
	SuspendedBy string `bun:",nullzero" json:"suspended_by,omitempty"`
	// PastDueAt anchors the dunning-email ladder. Set by the
	// payment-failed handler when the license first enters past_due;
	// cleared on recovery / cancellation. Reading lic.UpdatedAt as
	// a clock here is wrong — that column bumps on unrelated writes.
	PastDueAt *time.Time `json:"past_due_at,omitempty"`
	Notes     string     `json:"notes,omitempty"`
	OrgName   string     `json:"org_name,omitempty"`

	// External identifiers — opaque strings owned by the merchant.
	// Used to map Keygate licenses to the merchant's own user/tenant
	// model without a separate mapping table on their side.
	ExternalCustomerID  string `bun:",notnull,default:''" json:"external_customer_id,omitempty"`
	ExternalWorkspaceID string `bun:",notnull,default:''" json:"external_workspace_id,omitempty"`

	// SignupSource is SignupSourceSelf for a licence the customer
	// claimed themselves on a self sign-up plan, empty otherwise.
	// MarketingConsent is what they answered to the plan's consent
	// checkbox at that moment, stamped with MarketingConsentAt.
	SignupSource       string     `bun:",notnull,default:''" json:"signup_source,omitempty"`
	MarketingConsent   bool       `bun:",notnull" json:"marketing_consent"`
	MarketingConsentAt *time.Time `json:"marketing_consent_at,omitempty"`
	// SignupPlanID is the plan a self sign-up licence was claimed on.
	// PlanID moves with the licence; this stays, for the daily cap.
	SignupPlanID string `bun:",nullzero" json:"signup_plan_id,omitempty"`

	CreatedAt time.Time `bun:",nullzero,default:now()" json:"created_at"`
	UpdatedAt time.Time `bun:",nullzero,default:now()" json:"updated_at"`

	Product     *Product      `bun:"rel:belongs-to,join:product_id=id" json:"product,omitempty"`
	Plan        *Plan         `bun:"rel:belongs-to,join:plan_id=id" json:"plan,omitempty"`
	Activations []*Activation `bun:"rel:has-many,join:id=license_id" json:"activations,omitempty"`

	// ActivationCount is how many activations this license has, filled
	// in on list responses where loading the activations themselves
	// would mean dragging every row of every license back with the
	// page. It is computed, never stored, so it is scan-only.
	ActivationCount int `bun:"activation_count,scanonly" json:"activation_count"`

	// ActiveSessionCount is how many floating seats are in use right
	// now. A floating plan records occupancy in floating_sessions, not
	// in activations, so ActivationCount answers the wrong question
	// for one: a license with every seat taken has no activation rows
	// at all. Both counts exist because a floating license can still
	// activate devices; which one means "how full is it" depends on
	// the plan's license_model.
	ActiveSessionCount int             `bun:"active_session_count,scanonly" json:"active_session_count"`
	Seats              []*Seat         `bun:"rel:has-many,join:id=license_id" json:"seats,omitempty"`
	Addons             []*LicenseAddon `bun:"rel:has-many,join:id=license_id" json:"addons,omitempty"`

	// SeatLimit and ActivationLimit are what the licence holds, as
	// Limits works them out; set by FillLimits wherever the plan is
	// loaded, so clients show the limit the server enforces instead of
	// working it out from the plan. Absent when the plan is not loaded.
	SeatLimit       *int `bun:"-" json:"seat_limit,omitempty"`
	ActivationLimit *int `bun:"-" json:"activation_limit,omitempty"`

	// KeySeat is the member whose own key found this licence (see
	// Store.FindLicenseByKey); nil when it was found by the licence's
	// key or any other way.
	KeySeat *Seat `bun:"-" json:"-"`
	// KeySeatPaused: KeySeat is beyond the licence's seat limit (see
	// Seat.Paused), so the member's key opens the licence but may not be
	// used. Shown to that member in the portal.
	KeySeatPaused bool `bun:"-" json:"key_paused,omitempty"`
	// WasStripePaused, WasCanceled and WasExpired are the row as the last
	// subscription-driven write found it, under the lock it held until
	// commit (store.updateLicenseAndSubscriptionIn): a write that made it
	// usable ended a Stripe pause or brought an ended licence back, one
	// that canceled it ended the licence.
	// Concurrent events cannot both find the same state, so each change
	// is told once.
	WasStripePaused bool `bun:"-" json:"-"`
	// Whether the row was past due, and since when, as the write found
	// it: a recovery is told once, for the episode it ended.
	WasPastDue     bool       `bun:"-" json:"-"`
	PriorPastDueAt *time.Time `bun:"-" json:"-"`
	WasCanceled    bool       `bun:"-" json:"-"`
	WasExpired     bool       `bun:"-" json:"-"`
	// Whether the row was revoked already: a revocation is told once.
	WasRevoked bool `bun:"-" json:"-"`
}

// GraceDays is how many days past ValidUntil the licence's key keeps
// working, from its plan (7 without one). A trial that moves to its
// plan's fallback plan when it ends keeps at least one: the hourly sweep
// moves it, and until then it must not read as expired, as it would on
// a plan with no grace. A trial Stripe bills is Stripe's to settle and
// keeps the plan's number.
// Any other trial has none: the sweep expires it at its end date.
func (l *License) GraceDays() int {
	if l.Plan == nil {
		return 7
	}
	if l.Status == StatusTrialing && l.Plan.TrialFallbackPlanID != "" && l.StripeSubscriptionID == "" {
		return max(l.Plan.GraceDays, 1)
	}
	if l.Status == StatusTrialing && l.StripeSubscriptionID == "" {
		return 0
	}
	return l.Plan.GraceDays
}

// Limits are the people and devices this licence holds (see
// Plan.Limits). Without its plan loaded it holds what a licence with no
// plan always has: no seat limit and three devices.
func (l *License) Limits() (seats, activations int) {
	if l.Plan == nil {
		return 0, 3
	}
	return l.Plan.Limits(l.Quantity)
}

// FillLimits sets SeatLimit and ActivationLimit on each licence whose
// plan is loaded.
func FillLimits(ls ...*License) {
	for _, l := range ls {
		if l == nil || l.Plan == nil {
			continue
		}
		seats, activations := l.Plan.Limits(l.Quantity)
		l.SeatLimit, l.ActivationLimit = &seats, &activations
	}
}

const (
	StatusActive    = "active"
	StatusTrialing  = "trialing"
	StatusPastDue   = "past_due"
	StatusCanceled  = "canceled"
	StatusExpired   = "expired"
	StatusSuspended = "suspended"
	StatusRevoked   = "revoked"
)

// SignupSourceSelf marks a licence claimed through self sign-up
// (License.SignupSource).
const SignupSourceSelf = "self_signup"

// Who suspended a licence (License.SuspendedBy).
const (
	SuspendedByAdmin  = "admin"
	SuspendedByStripe = "stripe"
)

// ─── Activation ───

type Activation struct {
	bun.BaseModel `bun:"table:activations"`

	ID             string    `bun:",pk" json:"id"`
	LicenseID      string    `bun:",notnull" json:"license_id"`
	Identifier     string    `bun:",notnull" json:"identifier"`
	IdentifierType string    `bun:",notnull" json:"identifier_type"`
	Label          string    `json:"label,omitempty"`
	IPAddress      string    `json:"ip_address,omitempty"`
	LastVerified   time.Time `bun:",nullzero,default:now()" json:"last_verified"`
	CreatedAt      time.Time `bun:",nullzero,default:now()" json:"created_at"`
	// SeatID is the member whose key activated the device; empty for
	// the owner's key. Removing the member removes the device.
	SeatID string `bun:",nullzero" json:"seat_id,omitempty"`
	// PredatesMemberKeys marks a device activated with the licence's
	// key before members had keys of their own, when members used that
	// key: it may move, once, to the member whose key activates it (see
	// Store.ActivateForKey).
	PredatesMemberKeys bool `bun:",notnull" json:"-"`
	// MemberEmail is that member's address, filled in where devices are
	// shown (see Store.FillMemberEmails).
	MemberEmail string `bun:"-" json:"member_email,omitempty"`

	License *License `bun:"rel:belongs-to,join:license_id=id" json:"license,omitempty"`
}

// ─── Audit Log ───

type AuditLog struct {
	bun.BaseModel `bun:"table:audit_logs"`

	ID        string         `bun:",pk" json:"id"`
	Entity    string         `bun:",notnull" json:"entity"`
	EntityID  string         `bun:",notnull" json:"entity_id"`
	Action    string         `bun:",notnull" json:"action"`
	ActorID   string         `json:"actor_id,omitempty"`
	ActorType string         `json:"actor_type,omitempty"`
	Changes   map[string]any `bun:"type:jsonb,default:'{}'" json:"changes,omitempty"`
	IPAddress string         `json:"ip_address,omitempty"`
	// The product the line belongs to. Found from the entity when the
	// line is written; a caller deleting the entity sets it first.
	ProductID string    `bun:",nullzero" json:"product_id,omitempty"`
	CreatedAt time.Time `bun:",nullzero,default:now()" json:"created_at"`
}

// ─── Seat ───
type Seat struct {
	bun.BaseModel `bun:"table:seats"`
	ID            string     `bun:",pk" json:"id"`
	LicenseID     string     `bun:",notnull" json:"license_id"`
	UserID        string     `bun:",nullzero" json:"user_id,omitempty"`
	Email         string     `bun:",notnull" json:"email"`
	Role          string     `bun:",notnull,default:'member'" json:"role"`
	InvitedAt     time.Time  `bun:",nullzero,default:now()" json:"invited_at"`
	AcceptedAt    *time.Time `json:"accepted_at,omitempty"`
	RemovedAt     *time.Time `json:"removed_at,omitempty"`
	// InviteTokenHash: SHA256 of the plain token sent to the
	// invitee. Cleared on accept / expire / revoke so the slot
	// frees up. The plain token never lives in the DB.
	InviteTokenHash string `bun:",nullzero" json:"-"`
	// InviteExpiresAt: pinned at invite-time. After this, accepting
	// answers 404 SEAT_INVITE_INVALID and the owner must re-invite.
	InviteExpiresAt *time.Time `json:"invite_expires_at,omitempty"`
	// Paused: the licence holds more people than its seat limit allows,
	// and this member is among the last to have joined beyond it, so
	// their own key may not be used until the owner removes someone or
	// adds seats. Worked out when read (Store.PausedSeats), never stored.
	Paused bool `bun:"-" json:"paused,omitempty"`
	// MemberKey is the member's own license key, made the first time
	// they open the portal after accepting (see Store.MemberKey) and
	// cleared when they are removed. It is kept and found the way a
	// license key is: never in JSON, read through Store.MemberKey.
	MemberKey          string    `bun:",nullzero" json:"-"`
	MemberKeyHash      string    `bun:",nullzero" json:"-"`
	MemberKeyEncrypted []byte    `bun:",nullzero" json:"-"`
	CreatedAt          time.Time `bun:",nullzero,default:now()" json:"created_at"`
	License            *License  `bun:"rel:belongs-to,join:license_id=id" json:"license,omitempty"`
}

// JoinedSeats is the members who have accepted and are still on the
// team: the team as a member sees it. Pending invitations, and the
// addresses they were sent to, are the owner's to see.
func JoinedSeats(seats []*Seat) []*Seat {
	out := make([]*Seat, 0, len(seats))
	for _, s := range seats {
		if s.AcceptedAt != nil && s.RemovedAt == nil {
			out = append(out, s)
		}
	}
	return out
}

// ─── Usage ───
type UsageEvent struct {
	bun.BaseModel `bun:"table:usage_events"`
	ID            string         `bun:",pk" json:"id"`
	LicenseID     string         `bun:",notnull" json:"license_id"`
	Feature       string         `bun:",notnull" json:"feature"`
	Quantity      int64          `bun:",notnull,default:1" json:"quantity"`
	Metadata      map[string]any `bun:"type:jsonb,default:'{}'" json:"metadata,omitempty"`
	IPAddress     string         `json:"ip_address,omitempty"`
	RecordedAt    time.Time      `bun:",nullzero,default:now()" json:"recorded_at"`
}

type UsageCounter struct {
	bun.BaseModel `bun:"table:usage_counters"`
	ID            string    `bun:",pk" json:"id"`
	LicenseID     string    `bun:",notnull" json:"license_id"`
	Feature       string    `bun:",notnull" json:"feature"`
	Period        string    `bun:",notnull" json:"period"`
	PeriodKey     string    `bun:",notnull" json:"period_key"`
	Used          int64     `bun:",notnull,default:0" json:"used"`
	UpdatedAt     time.Time `bun:",nullzero,default:now()" json:"updated_at"`
}

// ─── Webhook ───
type Webhook struct {
	bun.BaseModel `bun:"table:webhooks"`
	ID            string    `bun:",pk" json:"id"`
	ProductID     string    `bun:",notnull" json:"product_id"`
	URL           string    `bun:",notnull" json:"url"`
	Secret        string    `bun:",notnull" json:"-"`
	Events        []string  `bun:",array" json:"events"`
	Active        bool      `bun:",notnull,default:true" json:"active"`
	CreatedAt     time.Time `bun:",nullzero,default:now()" json:"created_at"`
	UpdatedAt     time.Time `bun:",nullzero,default:now()" json:"updated_at"`
	Product       *Product  `bun:"rel:belongs-to,join:product_id=id" json:"product,omitempty"`
}

type WebhookDelivery struct {
	bun.BaseModel `bun:"table:webhook_deliveries"`
	ID            string         `bun:",pk" json:"id"`
	WebhookID     string         `bun:",notnull" json:"webhook_id"`
	Event         string         `bun:",notnull" json:"event"`
	Payload       map[string]any `bun:"type:jsonb,default:'{}'" json:"payload"`
	ResponseCode  int            `json:"response_code,omitempty"`
	ResponseBody  string         `json:"response_body,omitempty"`
	Attempts      int            `bun:",notnull,default:0" json:"attempts"`
	NextRetry     *time.Time     `json:"next_retry,omitempty"`
	Status        string         `bun:",notnull,default:'pending'" json:"status"`
	CreatedAt     time.Time      `bun:",nullzero,default:now()" json:"created_at"`
	DeliveredAt   *time.Time     `json:"delivered_at,omitempty"`
	// ClaimToken identifies the sender holding the row while it is
	// sent (see Store.ClaimPendingDeliveries); empty when nobody does.
	ClaimToken string `bun:",notnull,default:''" json:"-"`
}

// ─── Subscription ───
type Subscription struct {
	bun.BaseModel      `bun:"table:subscriptions"`
	ID                 string         `bun:",pk" json:"id"`
	LicenseID          string         `bun:",notnull" json:"license_id"`
	UserID             string         `bun:",nullzero" json:"user_id,omitempty"`
	PlanID             string         `bun:",notnull" json:"plan_id"`
	Status             string         `bun:",notnull,default:'active'" json:"status"`
	PaymentProvider    string         `json:"payment_provider,omitempty"`
	ExternalID         string         `json:"external_id,omitempty"`
	CurrentPeriodStart *time.Time     `json:"current_period_start,omitempty"`
	CurrentPeriodEnd   *time.Time     `json:"current_period_end,omitempty"`
	CancelAtPeriodEnd  bool           `bun:",notnull,default:false" json:"cancel_at_period_end"`
	CanceledAt         *time.Time     `json:"canceled_at,omitempty"`
	TrialStart         *time.Time     `json:"trial_start,omitempty"`
	TrialEnd           *time.Time     `json:"trial_end,omitempty"`
	Metadata           map[string]any `bun:"type:jsonb,default:'{}'" json:"metadata,omitempty"`
	CreatedAt          time.Time      `bun:",nullzero,default:now()" json:"created_at"`
	UpdatedAt          time.Time      `bun:",nullzero,default:now()" json:"updated_at"`
	License            *License       `bun:"rel:belongs-to,join:license_id=id" json:"license,omitempty"`
	Plan               *Plan          `bun:"rel:belongs-to,join:plan_id=id" json:"plan,omitempty"`
}

// ─── Analytics ───
type AnalyticsSnapshot struct {
	bun.BaseModel    `bun:"table:analytics_snapshots"`
	ID               string    `bun:",pk" json:"id"`
	Date             time.Time `bun:",notnull" json:"date"`
	ProductID        string    `bun:",notnull" json:"product_id"`
	TotalLicenses    int       `bun:",notnull,default:0" json:"total_licenses"`
	ActiveLicenses   int       `bun:",notnull,default:0" json:"active_licenses"`
	NewLicenses      int       `bun:",notnull,default:0" json:"new_licenses"`
	Churned          int       `bun:",notnull,default:0" json:"churned"`
	TotalActivations int       `bun:",notnull,default:0" json:"total_activations"`
	TotalSeats       int       `bun:",notnull,default:0" json:"total_seats"`
	TotalUsage       int64     `bun:",notnull,default:0" json:"total_usage"`
	CreatedAt        time.Time `bun:",nullzero,default:now()" json:"created_at"`
}

// ─── Floating Session ───
type FloatingSession struct {
	bun.BaseModel `bun:"table:floating_sessions"`
	ID            string    `bun:",pk" json:"id"`
	LicenseID     string    `bun:",notnull" json:"license_id"`
	Identifier    string    `bun:",notnull" json:"identifier"`
	Label         string    `json:"label,omitempty"`
	IPAddress     string    `json:"ip_address,omitempty"`
	CheckedOut    time.Time `bun:",nullzero,default:now()" json:"checked_out"`
	ExpiresAt     time.Time `bun:",notnull" json:"expires_at"`
	Heartbeat     time.Time `bun:",nullzero,default:now()" json:"heartbeat"`
	// SeatID is the member whose key checked the session out; empty for
	// the license's key. Removing the member ends it.
	SeatID string `bun:",nullzero" json:"seat_id,omitempty"`
}

// ─── Addon ───
type Addon struct {
	bun.BaseModel `bun:"table:addons"`
	ID            string    `bun:",pk" json:"id"`
	ProductID     string    `bun:",notnull" json:"product_id"`
	Name          string    `bun:",notnull" json:"name"`
	Slug          string    `bun:",notnull" json:"slug"`
	Description   string    `json:"description,omitempty"`
	Feature       string    `bun:",notnull" json:"feature"`
	ValueType     string    `bun:",notnull" json:"value_type"`
	Value         string    `bun:",notnull" json:"value"`
	QuotaPeriod   string    `bun:",default:''" json:"quota_period,omitempty"`
	QuotaUnit     string    `bun:",default:''" json:"quota_unit,omitempty"`
	Active        bool      `bun:",notnull,default:true" json:"active"`
	SortOrder     int       `bun:",default:0" json:"sort_order"`
	CreatedAt     time.Time `bun:",nullzero,default:now()" json:"created_at"`
	Product       *Product  `bun:"rel:belongs-to,join:product_id=id" json:"product,omitempty"`
}

// ─── License Addon ───
type LicenseAddon struct {
	bun.BaseModel `bun:"table:license_addons"`
	ID            string    `bun:",pk" json:"id"`
	LicenseID     string    `bun:",notnull" json:"license_id"`
	AddonID       string    `bun:",notnull" json:"addon_id"`
	Enabled       bool      `bun:",notnull,default:true" json:"enabled"`
	CreatedAt     time.Time `bun:",nullzero,default:now()" json:"created_at"`
	Addon         *Addon    `bun:"rel:belongs-to,join:addon_id=id" json:"addon,omitempty"`
}

// ─── Plan Update Terms ───
//
// One row per period a plan has sold, with the instant it took
// effect. A checkout Keygate created carries the period it was sold
// at; a Stripe Payment Link the merchant made carries nothing, and
// this is what tells fulfilment which period the buyer was shown.
type PlanUpdateTerms struct {
	bun.BaseModel `bun:"table:plan_update_terms"`

	ID          string `bun:",pk" json:"id"`
	PlanID      string `bun:",notnull" json:"plan_id"`
	UpdatesDays int    `bun:",notnull" json:"updates_days"`
	// LicenseType is the kind of licence the plan sold then: a Stripe
	// Payment Link carries no terms, and a session's own shape cannot
	// tell a subscription from a trial.
	LicenseType string `bun:",notnull" json:"license_type"`
	// EffectiveFrom is a whole second, the one after the edit: a
	// Stripe session carries its created time in whole seconds, so a
	// change made during that same second must not apply to it.
	EffectiveFrom time.Time `bun:",notnull,nullzero" json:"effective_from"`
	// RecordedAt is when the row was actually written, and orders two
	// edits that land inside one second.
	RecordedAt time.Time `bun:",notnull,nullzero" json:"recorded_at"`
}

// ─── License Renewal ───
//
// One paid extension of a perpetual license's maintenance period.
// The checkout session keeps fulfilment idempotent, the payment
// intent lets a refund find the renewal it reverses, and
// PreviousUpdatesUntil is what that refund restores.
type LicenseRenewal struct {
	bun.BaseModel `bun:"table:license_renewals"`

	ID                      string     `bun:",pk" json:"id"`
	LicenseID               string     `bun:",notnull" json:"license_id"`
	StripeCheckoutSessionID string     `bun:",notnull,default:''" json:"stripe_checkout_session_id,omitempty"`
	StripePaymentIntentID   string     `bun:",notnull,default:''" json:"stripe_payment_intent_id,omitempty"`
	Days                    int        `bun:",notnull" json:"days"`
	PreviousUpdatesUntil    *time.Time `json:"previous_updates_until,omitempty"`
	// UpdatesUntil is the end this renewal produced; nil when it
	// changed nothing (applied to a license with updates for life,
	// or refunded before it was applied).
	UpdatesUntil *time.Time `json:"updates_until,omitempty"`
	RefundedAt   *time.Time `json:"refunded_at,omitempty"`
	// SupersededAt is set when the license's period was reset outside
	// the ledger; the renewal then belongs to a previous period.
	SupersededAt *time.Time `json:"superseded_at,omitempty"`
	// CreatedAt is the instant the renewal was applied and the "now"
	// its end was computed from, so the ledger can be replayed.
	CreatedAt time.Time `bun:",nullzero,default:now()" json:"created_at"`
}

// License upgrade states (LicenseUpgrade.State).
const (
	UpgradePending  = "pending"
	UpgradeApplied  = "applied"
	UpgradeReverted = "reverted"
	UpgradeExpired  = "expired"
	UpgradeRefunded = "refunded"
	UpgradeVoid     = "void"
)

// LicenseUpgrade is one checkout that upgrades a licence Stripe does
// not bill (a free plan's) to a paid plan. Applied, the From* fields
// hold the licence as the upgrade found it, so a full refund or the end
// of the subscription can put it back instead of revoking it.
type LicenseUpgrade struct {
	bun.BaseModel `bun:"table:license_upgrades"`

	ID                      string     `bun:",pk" json:"id"`
	LicenseID               string     `bun:",notnull" json:"license_id"`
	ToPlanID                string     `bun:",nullzero" json:"to_plan_id,omitempty"`
	State                   string     `bun:",notnull" json:"state"`
	StripeCheckoutSessionID string     `bun:",notnull" json:"stripe_checkout_session_id"`
	StripePaymentIntentID   string     `bun:",notnull,default:''" json:"stripe_payment_intent_id,omitempty"`
	StripeSubscriptionID    string     `bun:",notnull,default:''" json:"stripe_subscription_id,omitempty"`
	FromPlanID              string     `bun:",nullzero" json:"from_plan_id,omitempty"`
	FromStatus              string     `bun:",notnull,default:''" json:"from_status,omitempty"`
	FromValidUntil          *time.Time `json:"from_valid_until,omitempty"`
	FromUpdatesUntil        *time.Time `json:"from_updates_until,omitempty"`
	FromUpdatesTermsSet     bool       `bun:",notnull" json:"from_updates_terms_set"`
	FromPaymentProvider     string     `bun:",notnull,default:''" json:"from_payment_provider,omitempty"`
	FromStripeCustomerID    string     `bun:",notnull,default:''" json:"from_stripe_customer_id,omitempty"`
	FromCanceledAt          *time.Time `json:"from_canceled_at,omitempty"`
	FromQuantity            int        `bun:",notnull,default:1" json:"from_quantity"`
	EndReason               string     `bun:",notnull,default:''" json:"end_reason,omitempty"`
	CreatedAt               time.Time  `bun:",nullzero,default:now()" json:"created_at"`
	AppliedAt               *time.Time `json:"applied_at,omitempty"`
	EndedAt                 *time.Time `json:"ended_at,omitempty"`
}

// Counts reports whether the renewal still contributes to the
// license's maintenance period.
func (r *LicenseRenewal) Counts() bool { return r.RefundedAt == nil && r.UpdatesUntil != nil }

// ReplayRenewals derives the maintenance end from the ledger, so a
// refund lands on the dates a customer who never bought the refunded
// renewal would have, in whatever order refunds arrive.
//
// Each row records the end the license had when it was bought. The
// replay predicts that end from the rows before it: every renewal that
// was in effect then, so one refunded later still counts, and one
// refunded earlier does not, as its refund had already taken it back
// out. The answer is the walk of every row without the refunded ones.
//
// A row whose recorded previous end is not what the replay predicts
// marks a change made outside the ledger, an admin edit. It is
// carried as an offset rather than a new baseline: resetting to the
// edited date would fold every earlier renewal into it, and refunding
// one of those would then give the money back without taking the
// days. Applying the difference keeps both properties — the edit
// survives, and a refund removes exactly its own renewal. Only a move
// to or from "updates for life" cannot be expressed as an offset, and
// there the walk restarts from the recorded end.
func ReplayRenewals(renewals []*LicenseRenewal) *time.Time {
	// What happened outside the ledger just before each row: a restart
	// from its recorded end, or an admin edit carried as a shift.
	type outside struct {
		restart bool
		shift   time.Duration
	}
	before := make([]outside, len(renewals))
	// walk replays the first n rows, the outside changes included, each
	// adding its days when counts says it was in effect.
	walk := func(n int, counts func(*LicenseRenewal) bool) *time.Time {
		var end *time.Time
		for k, r := range renewals[:n] {
			switch o := before[k]; {
			case o.restart:
				end = r.PreviousUpdatesUntil
			case o.shift != 0 && end != nil:
				shifted := end.Add(o.shift)
				end = &shifted
			}
			if r.UpdatesUntil != nil && counts(r) { // nil: changed nothing when applied
				renewed := RenewedUpdatesUntil(end, r.CreatedAt, r.Days)
				end = &renewed
			}
		}
		return end
	}
	for i, r := range renewals {
		predicted := walk(i, func(p *LicenseRenewal) bool {
			return p.RefundedAt == nil || !p.RefundedAt.Before(r.CreatedAt)
		})
		switch {
		case i == 0:
			before[i].restart = true
		case SameEnd(r.PreviousUpdatesUntil, predicted):
		case r.PreviousUpdatesUntil == nil || predicted == nil:
			before[i].restart = true
		default:
			before[i].shift = r.PreviousUpdatesUntil.Sub(*predicted)
		}
	}
	return walk(len(renewals), func(p *LicenseRenewal) bool { return p.RefundedAt == nil })
}

// SameEnd compares two optional instants that went through Postgres
// (microseconds) and Go (nanoseconds).
func SameEnd(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Sub(*b).Abs() < time.Second
}

// EffectiveUpdatesUntil is the maintenance end that applies now. Only
// perpetual plans have one: a license moved to a subscription follows
// valid_until again even if a date was left behind on the row. With
// the plan not loaded the stored date is used as-is.
func (l *License) EffectiveUpdatesUntil() *time.Time {
	if l.Plan != nil && l.Plan.LicenseType != "perpetual" {
		return nil
	}
	return l.UpdatesUntil
}

// UpdatesUntilFor is the maintenance end a purchase of days grants.
// Only perpetual licenses have a separate period, and zero days means
// updates for life; both answer nil.
func UpdatesUntilFor(licenseType string, days int, now time.Time) *time.Time {
	if licenseType != "perpetual" || days <= 0 {
		return nil
	}
	until := now.Add(time.Duration(days) * 24 * time.Hour)
	return &until
}

// IsFree reports whether Keygate charges nothing for the plan: no
// purchase price, no update period and no renewals. It is the rule the
// plans_self_signup_free constraint holds a self sign-up plan to, and
// the one an upgrade checks the licence's current plan against.
func (p *Plan) IsFree() bool {
	return p.StripePriceID == "" && p.UpdatesDays == 0 && p.RenewalDays == 0 && p.StripeRenewalPriceID == ""
}

// TrialFallbackRefusal says why p cannot be the plan that licences of a
// trial plan of productID move to when their trial ends, or "" when it
// can. Only a free, non-trial plan of the same product qualifies: a
// paid one would hand out what is sold for nothing (and a perpetual
// plan with an update period would also need the gating a sale gets),
// and a trial one would only start another trial. Nor one sold per
// seat: its seats are bought, and a licence moved onto it would hold
// one, below what the plan sells. The admin API asks
// when the plan is saved and the expiry sweep asks again before it
// moves a licence, since the target may have been repriced since.
// Whether the target is active does not matter: active decides what is
// on sale, and an operator may well keep the free tier off the public
// plan list while trials still land on it.
func (p *Plan) TrialFallbackRefusal(productID string) string {
	switch {
	case p.ProductID != productID:
		return "must be a plan of the same product"
	case p.LicenseType == "trial":
		return "cannot be a trial plan"
	case p.PerSeat:
		return "cannot be a plan sold per seat"
	case !p.IsFree():
		return "must be a free plan (no stripe_price_id, update period or renewals)"
	}
	return ""
}

// InitialUpdatesUntil is the maintenance period a new license on this
// plan starts with, as the plan reads right now.
func (p *Plan) InitialUpdatesUntil(now time.Time) *time.Time {
	return UpdatesUntilFor(p.LicenseType, p.UpdatesDays, now)
}

// OffersRenewal reports whether customers can buy more maintenance
// for licenses on this plan through Stripe. A deactivated plan was
// taken off sale: like checkout and plan changes, renewals stop too.
func (p *Plan) OffersRenewal() bool {
	return p.Active && p.LicenseType == "perpetual" && p.RenewalDays > 0 && p.StripeRenewalPriceID != ""
}

// RenewedUpdatesUntil is the maintenance end after buying days more:
// a renewal before expiry extends from the current end, a renewal
// after expiry starts from now — the lapsed time is not sold twice
// and not given away either.
func RenewedUpdatesUntil(current *time.Time, now time.Time, days int) time.Time {
	base := now
	if current != nil && current.After(now) {
		base = *current
	}
	return base.Add(time.Duration(days) * 24 * time.Hour)
}

// ─── Metered Billing (event log) ───
//
// One row per RecordUsage call that targets a metered entitlement.
// Quantity is the DELTA contributed by that call (not the running
// total) — Stripe's Billing Meter API accumulates server-side per
// customer + event_name.
//
// Identifier is the stable token Keygate hands to Stripe as the
// meter event's `identifier` field; Stripe dedupes retries over a
// rolling 24-hour window using it, so our sync job can call as
// many times as it wants without double-counting.
type MeteredBilling struct {
	bun.BaseModel `bun:"table:metered_billing"`
	ID            string `bun:",pk" json:"id"`
	LicenseID     string `bun:",notnull" json:"license_id"`
	Feature       string `bun:",notnull" json:"feature"`
	Quantity      int64  `bun:",notnull" json:"quantity"`
	PeriodKey     string `bun:",notnull" json:"period_key"`
	Identifier    string `bun:",notnull,default:''" json:"identifier,omitempty"`
	// StripeMeterEventName and StripeCustomerID are where the usage is
	// billed: the meter and customer it was recorded against. Editing
	// the entitlement or the license later does not move it. Empty on
	// rows queued before they were kept, and the customer on usage
	// recorded before the license had one: those are resolved from
	// the license when the row syncs.
	StripeMeterEventName string     `bun:",notnull,default:''" json:"stripe_meter_event_name,omitempty"`
	StripeCustomerID     string     `bun:",notnull,default:''" json:"stripe_customer_id,omitempty"`
	Synced               bool       `bun:",notnull,default:false" json:"synced"`
	SyncedAt             *time.Time `json:"synced_at,omitempty"`
	ExternalID           string     `json:"external_id,omitempty"`
	// Attempts counts the pushes to Stripe that failed. LastError
	// records the most recent failure message so operators can debug
	// stuck rows without tailing logs.
	Attempts  int       `bun:",notnull,default:0" json:"attempts"`
	LastError string    `bun:",notnull,default:''" json:"last_error,omitempty"`
	CreatedAt time.Time `bun:",nullzero,default:now()" json:"created_at"`
}

// Webhook event constants
const (
	EventLicenseCreated          = "license.created"
	EventLicenseActivated        = "license.activated"
	EventLicenseDeactivated      = "license.deactivated"
	EventLicenseExpiryChanged    = "license.expiry_changed"
	EventLicenseUpdatesChanged   = "license.updates_changed"
	EventLicenseExpired          = "license.expired"
	EventLicenseCanceled         = "license.canceled"
	EventLicenseSuspended        = "license.suspended"
	EventLicenseReinstated       = "license.reinstated"
	EventLicenseRevoked          = "license.revoked"
	EventLicensePaymentFailed    = "license.payment_failed"
	EventLicensePaymentRecovered = "license.payment_recovered"
	EventLicenseQuantityChanged  = "license.quantity_changed"
	EventQuotaWarning            = "quota.warning"
	EventQuotaExceeded           = "quota.exceeded"
	EventSeatAdded               = "seat.added"
	EventSeatRemoved             = "seat.removed"
	EventPlanChanged             = "plan.changed"
	EventReleasePublished        = "release.published"
	EventReleaseYanked           = "release.yanked"
	EventReleaseUnyanked         = "release.unyanked"
)

// WebhookEvents is every event a webhook can subscribe to. Delivery
// matches the name exactly, so a subscription to anything else would
// never fire; it is refused instead. webhook.test is sent to an
// endpoint on request whatever it subscribes to, so it is not here.
var WebhookEvents = []string{
	EventLicenseCreated, EventLicenseActivated, EventLicenseDeactivated,
	EventLicenseExpiryChanged, EventLicenseUpdatesChanged, EventLicenseExpired,
	EventLicenseCanceled, EventLicenseSuspended, EventLicenseReinstated,
	EventLicenseRevoked, EventLicensePaymentFailed, EventLicensePaymentRecovered,
	EventLicenseQuantityChanged, EventQuotaWarning, EventQuotaExceeded,
	EventSeatAdded, EventSeatRemoved, EventPlanChanged, EventReleasePublished, EventReleaseYanked, EventReleaseUnyanked,
}

// IsValidWebhookEvent reports whether e is in WebhookEvents.
func IsValidWebhookEvent(e string) bool {
	return slices.Contains(WebhookEvents, e)
}

// ─── Release (logical release event) ───
//
// A release is a versioned event scoped to a product. It contains zero or
// more platform-specific Artifacts. Lifecycle (draft/published/yanked)
// applies to the whole release; yanking pulls every artifact from the
// feed at once. This matches GitHub Releases / Keygen / npm conventions.
type Release struct {
	bun.BaseModel `bun:"table:releases"`

	ID        string `bun:",pk" json:"id"`
	ProductID string `bun:",notnull" json:"product_id"`

	// Version is unique per product. v1.2.3 is one release; multiple
	// platform binaries live as Artifacts under it.
	Version string `bun:",notnull" json:"version"`
	// Channel uses nullzero so Go zero-value "" delegates to SQL DEFAULT 'stable'.
	Channel string `bun:",notnull,nullzero,default:'stable'" json:"channel"`

	Name         string `bun:",notnull,default:''" json:"name"`
	ReleaseNotes string `bun:",notnull,default:''" json:"release_notes"`

	// Status uses nullzero so Go zero-value delegates to SQL DEFAULT 'draft'.
	Status       string `bun:",notnull,nullzero,default:'draft'" json:"status"`
	YankedReason string `bun:",notnull,default:''" json:"yanked_reason,omitempty"`

	PublishedAt *time.Time `json:"published_at,omitempty"`
	YankedAt    *time.Time `json:"yanked_at,omitempty"`
	CreatedAt   time.Time  `bun:",nullzero,default:now()" json:"created_at"`
	UpdatedAt   time.Time  `bun:",nullzero,default:now()" json:"updated_at"`

	Product   *Product           `bun:"rel:belongs-to,join:product_id=id" json:"product,omitempty"`
	Artifacts []*ReleaseArtifact `bun:"rel:has-many,join:id=release_id" json:"artifacts,omitempty"`
}

// ReleaseArtifact is a per-platform binary within a Release.
//
// Each artifact carries its own sha256 + ed25519_sig (per-platform binaries
// have different bytes, so signatures must be per-artifact). One artifact
// per (release_id, platform) tuple — enforced by DB UNIQUE.
//
// Ed25519Sig format contract:
//
//	raw base64-encoded 64-byte Ed25519 signature of the artifact bytes.
//	88 characters when base64-padded. Empty string = unsigned.
//	Feed renderers convert to per-target format (Sparkle uses as-is in
//	sparkle:edSignature; Tauri/Velopack adapt as needed).
type ReleaseArtifact struct {
	bun.BaseModel `bun:"table:release_artifacts"`

	ID        string `bun:",pk" json:"id"`
	ReleaseID string `bun:",notnull" json:"release_id"`

	Platform string `bun:",notnull" json:"platform"`
	// Filename is the name the file had when it was uploaded, shown in
	// the dashboard. Storage keys stay generated (see buildFileKey).
	// Empty for artifacts uploaded before it was recorded.
	Filename string `bun:",notnull,default:''" json:"filename"`

	FileKey     string `bun:",notnull,default:''" json:"file_key"`
	FileSize    int64  `bun:",notnull,default:0" json:"file_size"`
	SHA256      string `bun:",notnull,default:''" json:"sha256"`
	Ed25519Sig  string `bun:",notnull,default:''" json:"ed25519_sig"`
	ContentType string `bun:",notnull,nullzero,default:'application/octet-stream'" json:"content_type"`

	// SigningKeyID identifies which signing key produced Ed25519Sig.
	// Nullable when the artifact was published without signing.
	SigningKeyID string `bun:",nullzero" json:"signing_key_id,omitempty"`

	// TauriSignature is the minisign signature file Tauri's updater
	// verifies, base64 encoded: Ed25519Sig plus a trusted comment and a
	// global signature over both. Made with the same key as Ed25519Sig.
	// Empty = unsigned.
	TauriSignature string `bun:",notnull,default:''" json:"-"`

	CreatedAt time.Time `bun:",nullzero,default:now()" json:"created_at"`
	UpdatedAt time.Time `bun:",nullzero,default:now()" json:"updated_at"`

	Release *Release `bun:"rel:belongs-to,join:release_id=id" json:"-"`
}

// IsUploaded reports whether the artifact has both a storage key and a
// sha256, meaning the upload+finalize cycle is complete and the artifact
// can be part of a published release.
func (a *ReleaseArtifact) IsUploaded() bool {
	return a != nil && a.FileKey != "" && a.SHA256 != ""
}

// Release status constants.
const (
	ReleaseStatusDraft     = "draft"
	ReleaseStatusPublished = "published"
	ReleaseStatusYanked    = "yanked"
)

// ─── ReleaseSigningKey (per-product Ed25519 keypair for artifact signing) ───
//
// The private key is encrypted at rest using AES-256-GCM under the master
// key from RELEASE_KEY_ENCRYPTION_KEY. The PrivateKeyEncrypted field is
// JSON-hidden — it should never appear in any API response.
//
// At most one row per product has Active=true (enforced by a partial unique
// index in the migration). Rotation deactivates the current row and inserts
// a new one in a single transaction.
type ReleaseSigningKey struct {
	bun.BaseModel `bun:"table:release_signing_keys"`

	ID                  string     `bun:",pk" json:"id"`
	ProductID           string     `bun:",notnull" json:"product_id"`
	PublicKey           string     `bun:",notnull" json:"public_key"`
	PrivateKeyEncrypted []byte     `bun:",notnull" json:"-"`
	Active              bool       `bun:",notnull,default:true" json:"active"`
	Note                string     `bun:",notnull,default:''" json:"note,omitempty"`
	CreatedAt           time.Time  `bun:",nullzero,default:now()" json:"created_at"`
	RotatedAt           *time.Time `json:"rotated_at,omitempty"`
}

// Release channel constants.
const (
	ReleaseChannelStable = "stable"
	ReleaseChannelBeta   = "beta"
	ReleaseChannelAlpha  = "alpha"
	ReleaseChannelDev    = "dev"
)

// IsValidReleaseChannel reports whether c is one of the allowed channels.
func IsValidReleaseChannel(c string) bool {
	switch c {
	case ReleaseChannelStable, ReleaseChannelBeta, ReleaseChannelAlpha, ReleaseChannelDev:
		return true
	}
	return false
}

// IdempotencyKey caches the response of an idempotent POST so a retry
// with the same `Idempotency-Key` header returns the original outcome.
// (key, endpoint) is composite primary key.
type IdempotencyKey struct {
	bun.BaseModel `bun:"table:idempotency_keys"`

	Key      string `bun:",pk" json:"key"`
	Endpoint string `bun:",pk" json:"endpoint"`

	BodyHash string `bun:",notnull" json:"body_hash"`

	ResponseStatus   int    `bun:",notnull,default:0" json:"response_status"`
	ResponseBody     string `bun:",notnull,default:''" json:"response_body"`
	ResponseComplete bool   `bun:",notnull,default:false" json:"response_complete"`

	CreatedAt time.Time `bun:",nullzero,default:now()" json:"created_at"`
	ExpiresAt time.Time `bun:",nullzero" json:"expires_at"`
}

// API key scopes — reserved for future server-to-server integrations
// (e.g. license:read, license:write). No scope is currently enforced
// at the route layer; api_keys are programmatic credentials waiting on
// a use-case. The Scopes []string column on api_keys is preserved so
// rolling out a future scope is a route-layer change only.
