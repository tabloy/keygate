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

type Product struct {
	bun.BaseModel `bun:"table:products"`

	ID   string `bun:",pk" json:"id"`
	Name string `bun:",notnull" json:"name"`
	Slug string `bun:",notnull,unique" json:"slug"`
	Type string `bun:",notnull" json:"type"`

	// MinimumSupportedVersion: optional semver floor. When non-empty,
	// auto-update feeds embed this so clients can refuse to keep
	// running an installed build older than the floor — the simple
	// "force upgrade" knob without staged rollout machinery.
	MinimumSupportedVersion string `bun:",notnull,default:''" json:"minimum_supported_version,omitempty"`

	// MinimumSupportedMessage: human-readable note shown alongside the
	// forced-upgrade prompt (e.g. "old TLS protocol no longer accepted").
	MinimumSupportedMessage string `bun:",notnull,default:''" json:"minimum_supported_message,omitempty"`

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
	TokenTTLDays int       `bun:",notnull" json:"token_ttl_days"`
	MaxSeats     int       `bun:",notnull" json:"max_seats"`
	Active       bool      `bun:",notnull,default:true" json:"active"`
	SortOrder    int       `bun:",default:0" json:"sort_order"`
	CreatedAt    time.Time `bun:",nullzero,default:now()" json:"created_at"`

	Product      *Product       `bun:"rel:belongs-to,join:product_id=id" json:"product,omitempty"`
	Entitlements []*Entitlement `bun:"rel:has-many,join:id=plan_id" json:"entitlements,omitempty"`
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
	// StripePaymentIntentID is the payment intent of the checkout session
	// that created this license. charge.refunded carries the same id, so
	// a refund can be matched to exactly this license.
	StripePaymentIntentID string `bun:",notnull,default:''" json:"stripe_payment_intent_id,omitempty"`
	// StripeCheckoutSessionID is the checkout session that created the
	// license; fulfilment retries use it to see whether the session
	// already produced one.
	StripeCheckoutSessionID string `bun:",notnull,default:''" json:"stripe_checkout_session_id,omitempty"`

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
	CreatedAt time.Time      `bun:",nullzero,default:now()" json:"created_at"`
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
	// InviteExpiresAt: pinned at invite-time. After this, /seats/accept
	// returns 410 GONE and admin must re-issue.
	InviteExpiresAt *time.Time `json:"invite_expires_at,omitempty"`
	CreatedAt       time.Time  `bun:",nullzero,default:now()" json:"created_at"`
	License         *License   `bun:"rel:belongs-to,join:license_id=id" json:"license,omitempty"`
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

// Counts reports whether the renewal still contributes to the
// license's maintenance period.
func (r *LicenseRenewal) Counts() bool { return r.RefundedAt == nil && r.UpdatesUntil != nil }

// ReplayRenewals derives the maintenance end from the ledger, so a
// refund lands on the dates a customer who never bought the refunded
// renewal would have, in whatever order refunds arrive.
//
// Two cursors walk the rows oldest first. observed is the end the
// license had at each point, counting renewals that were later
// refunded, because they were in effect when the next one was bought.
// net is the same walk without refunded renewals; it is the answer.
//
// A row whose recorded previous end is not what observed predicts
// marks a change made outside the ledger, an admin edit. It is
// carried as an offset rather than a new baseline: resetting net to
// the edited date would fold every earlier renewal into it, and
// refunding one of those would then give the money back without
// taking the days. Applying the difference keeps both properties —
// the edit survives, and a refund removes exactly its own renewal.
// Only a move to or from "updates for life" cannot be expressed as
// an offset, and there both cursors restart.
func ReplayRenewals(renewals []*LicenseRenewal) *time.Time {
	var observed, net *time.Time
	for i, r := range renewals {
		switch {
		case i == 0 || SameEnd(r.PreviousUpdatesUntil, observed):
			if i == 0 {
				observed, net = r.PreviousUpdatesUntil, r.PreviousUpdatesUntil
			}
		case r.PreviousUpdatesUntil == nil || observed == nil:
			observed, net = r.PreviousUpdatesUntil, r.PreviousUpdatesUntil
		default:
			if net != nil {
				shifted := net.Add(r.PreviousUpdatesUntil.Sub(*observed))
				net = &shifted
			}
			observed = r.PreviousUpdatesUntil
		}
		if r.UpdatesUntil == nil {
			continue // changed nothing when applied
		}
		end := RenewedUpdatesUntil(observed, r.CreatedAt, r.Days)
		observed = &end
		if r.RefundedAt == nil {
			netEnd := RenewedUpdatesUntil(net, r.CreatedAt, r.Days)
			net = &netEnd
		}
	}
	return net
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
	ID            string     `bun:",pk" json:"id"`
	LicenseID     string     `bun:",notnull" json:"license_id"`
	Feature       string     `bun:",notnull" json:"feature"`
	Quantity      int64      `bun:",notnull" json:"quantity"`
	PeriodKey     string     `bun:",notnull" json:"period_key"`
	Identifier    string     `bun:",notnull,default:''" json:"identifier,omitempty"`
	Synced        bool       `bun:",notnull,default:false" json:"synced"`
	SyncedAt      *time.Time `json:"synced_at,omitempty"`
	ExternalID    string     `json:"external_id,omitempty"`
	// Attempts increments on every push to Stripe (success OR
	// failure). LastError records the most recent failure message
	// so operators can debug stuck rows without tailing logs.
	Attempts  int       `bun:",notnull,default:0" json:"attempts"`
	LastError string    `bun:",notnull,default:''" json:"last_error,omitempty"`
	CreatedAt time.Time `bun:",nullzero,default:now()" json:"created_at"`
}

// Webhook event constants
const (
	EventLicenseCreated    = "license.created"
	EventLicenseCanceled   = "license.canceled"
	EventLicenseSuspended  = "license.suspended"
	EventLicenseReinstated = "license.reinstated"
	EventLicenseRevoked    = "license.revoked"
	EventQuotaWarning      = "quota.warning"
	EventQuotaExceeded     = "quota.exceeded"
	EventSeatAdded         = "seat.added"
	EventSeatRemoved       = "seat.removed"
	EventPlanChanged       = "plan.changed"
	EventReleasePublished  = "release.published"
	EventReleaseYanked     = "release.yanked"
	EventReleaseUnyanked   = "release.unyanked"
)

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

	FileKey     string `bun:",notnull,default:''" json:"file_key"`
	FileSize    int64  `bun:",notnull,default:0" json:"file_size"`
	SHA256      string `bun:",notnull,default:''" json:"sha256"`
	Ed25519Sig  string `bun:",notnull,default:''" json:"ed25519_sig"`
	ContentType string `bun:",notnull,nullzero,default:'application/octet-stream'" json:"content_type"`

	// SigningKeyID identifies which signing key produced Ed25519Sig.
	// Nullable when the artifact was published without signing.
	SigningKeyID string `bun:",nullzero" json:"signing_key_id,omitempty"`

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
