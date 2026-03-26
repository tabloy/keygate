package model

import (
	"time"

	"github.com/uptrace/bun"
)

// ─── User (OAuth only) ───

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

// ─── Product ───

type Product struct {
	bun.BaseModel `bun:"table:products"`

	ID        string    `bun:",pk" json:"id"`
	Name      string    `bun:",notnull" json:"name"`
	Slug      string    `bun:",notnull,unique" json:"slug"`
	Type      string    `bun:",notnull" json:"type"`
	CreatedAt time.Time `bun:",nullzero,default:now()" json:"created_at"`
}

// ─── API Key (per product, for client SDK auth) ───

type APIKey struct {
	bun.BaseModel `bun:"table:api_keys"`

	ID        string     `bun:",pk" json:"id"`
	ProductID string     `bun:",notnull" json:"product_id"`
	Name      string     `bun:",notnull" json:"name"`
	KeyHash   string     `bun:",notnull,unique" json:"-"`
	Prefix    string     `bun:",notnull" json:"prefix"`
	Scopes    []string   `bun:",array" json:"scopes"`
	LastUsed  *time.Time `json:"last_used,omitempty"`
	CreatedAt time.Time  `bun:",nullzero,default:now()" json:"created_at"`

	Product *Product `bun:"rel:belongs-to,join:product_id=id" json:"product,omitempty"`
}

func (a *APIKey) GetID() string       { return a.ID }
func (a *APIKey) GetScopes() []string { return a.Scopes }

// ─── Plan ───

type Plan struct {
	bun.BaseModel `bun:"table:plans"`

	ID              string    `bun:",pk" json:"id"`
	ProductID       string    `bun:",notnull" json:"product_id"`
	Name            string    `bun:",notnull" json:"name"`
	Slug            string    `bun:",notnull" json:"slug"`
	LicenseType     string    `bun:",notnull" json:"license_type"`
	BillingInterval string    `json:"billing_interval,omitempty"`
	MaxActivations  int       `bun:",notnull,default:3" json:"max_activations"`
	TrialDays       int       `bun:",default:0" json:"trial_days"`
	GraceDays       int       `bun:",default:7" json:"grace_days"`
	StripePriceID   string    `json:"stripe_price_id,omitempty"`
	PayPalPlanID    string    `bun:"paypal_plan_id" json:"paypal_plan_id,omitempty"`
	LicenseModel    string    `bun:",notnull,default:'standard'" json:"license_model"` // standard | floating
	FloatingTimeout int       `bun:",notnull,default:30" json:"floating_timeout"`      // minutes
	MaxSeats        int       `bun:",notnull,default:0" json:"max_seats"`
	Active          bool      `bun:",notnull,default:true" json:"active"`
	SortOrder       int       `bun:",default:0" json:"sort_order"`
	CreatedAt       time.Time `bun:",nullzero,default:now()" json:"created_at"`

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
}

// ─── License ───

type License struct {
	bun.BaseModel `bun:"table:licenses"`

	ID         string `bun:",pk" json:"id"`
	ProductID  string `bun:",notnull" json:"product_id"`
	PlanID     string `bun:",notnull" json:"plan_id"`
	UserID     string `bun:",nullzero" json:"user_id,omitempty"`
	Email      string `bun:",notnull" json:"email"`
	LicenseKey string `bun:",notnull,unique" json:"license_key"`
	KeyHash    string `bun:",notnull,default:''" json:"-"` // never exposed in API

	PaymentProvider      string `json:"payment_provider,omitempty"`
	StripeCustomerID     string `json:"stripe_customer_id,omitempty"`
	StripeSubscriptionID string `bun:",unique,nullzero" json:"stripe_subscription_id,omitempty"`
	PayPalSubscriptionID string `bun:"paypal_subscription_id,unique,nullzero" json:"paypal_subscription_id,omitempty"`

	Status      string     `bun:",notnull,default:'active'" json:"status"`
	ValidFrom   time.Time  `bun:",notnull,default:now()" json:"valid_from"`
	ValidUntil  *time.Time `json:"valid_until,omitempty"`
	CanceledAt  *time.Time `json:"canceled_at,omitempty"`
	SuspendedAt *time.Time `json:"suspended_at,omitempty"`
	Notes       string     `json:"notes,omitempty"`
	OrgName     string     `json:"org_name,omitempty"`
	CreatedAt   time.Time  `bun:",nullzero,default:now()" json:"created_at"`
	UpdatedAt   time.Time  `bun:",nullzero,default:now()" json:"updated_at"`

	Product     *Product        `bun:"rel:belongs-to,join:product_id=id" json:"product,omitempty"`
	Plan        *Plan           `bun:"rel:belongs-to,join:plan_id=id" json:"plan,omitempty"`
	Activations []*Activation   `bun:"rel:has-many,join:id=license_id" json:"activations,omitempty"`
	Seats       []*Seat         `bun:"rel:has-many,join:id=license_id" json:"seats,omitempty"`
	Addons      []*LicenseAddon `bun:"rel:has-many,join:id=license_id" json:"addons,omitempty"`
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
	CreatedAt     time.Time  `bun:",nullzero,default:now()" json:"created_at"`
	License       *License   `bun:"rel:belongs-to,join:license_id=id" json:"license,omitempty"`
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

// ─── Metered Billing ───
type MeteredBilling struct {
	bun.BaseModel `bun:"table:metered_billing"`
	ID            string     `bun:",pk" json:"id"`
	LicenseID     string     `bun:",notnull" json:"license_id"`
	Feature       string     `bun:",notnull" json:"feature"`
	Quantity      int64      `bun:",notnull" json:"quantity"`
	PeriodKey     string     `bun:",notnull" json:"period_key"`
	Synced        bool       `bun:",notnull,default:false" json:"synced"`
	SyncedAt      *time.Time `json:"synced_at,omitempty"`
	ExternalID    string     `json:"external_id,omitempty"`
	CreatedAt     time.Time  `bun:",nullzero,default:now()" json:"created_at"`
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
)
