const BASE = `${import.meta.env.VITE_API_URL || ""}/api/v1`

async function request<T>(path: string, opts?: RequestInit): Promise<T> {
  let res: Response
  try {
    res = await fetch(BASE + path, {
      credentials: "include",
      headers: { "Content-Type": "application/json", ...opts?.headers },
      ...opts,
    })
  } catch (e) {
    // Network-level failure (server down, DNS, CORS). fetch() throws
    // a TypeError here; we surface a friendly message instead of
    // "Failed to fetch" which is meaningless to end users.
    const reason = e instanceof Error ? e.message : String(e)
    throw new Error(`Network error: ${reason}. Is the server reachable?`)
  }

  // Handle session expiry: redirect to login on 401
  if (res.status === 401) {
    // Don't redirect if already on login page or fetching auth state
    if (!window.location.pathname.startsWith("/login") && path !== "/portal/me") {
      window.location.href = "/login"
    }
    throw new Error("Session expired")
  }

  if (res.status === 204) return undefined as T

  // Read the body as text first so an empty or non-JSON response (502
  // from a proxy, server crash mid-response, HTML error page, etc.)
  // doesn't produce the cryptic "Unexpected end of JSON input" — that
  // error confuses users who clicked a normal button.
  const raw = await res.text()
  let json: any = null
  if (raw) {
    try {
      json = JSON.parse(raw)
    } catch {
      // body wasn't JSON — keep `json` null, fall through to text path
    }
  }

  if (!res.ok) {
    const msg =
      json?.error?.message ||
      (typeof json?.error === "string" ? json.error : null) ||
      (raw && raw.length < 200 ? raw : null) ||
      `Request failed (${res.status}${res.statusText ? ` ${res.statusText}` : ""})`
    throw new Error(msg)
  }
  return (json?.data !== undefined ? json.data : json) as T
}

function get<T>(path: string) {
  return request<T>(path)
}
function post<T>(path: string, body?: unknown) {
  return request<T>(path, { method: "POST", body: body ? JSON.stringify(body) : undefined })
}
function put<T>(path: string, body?: unknown) {
  return request<T>(path, { method: "PUT", body: body ? JSON.stringify(body) : undefined })
}
// eslint-disable-next-line @typescript-eslint/no-unused-vars
function patch<T>(path: string, body?: unknown) {
  return request<T>(path, { method: "PATCH", body: body ? JSON.stringify(body) : undefined })
}
function del<T>(path: string) {
  return request<T>(path, { method: "DELETE" })
}

// ─── Site Config (public, no auth) ───
export const site = {
  config: () => get<Record<string, string>>("/config"),
}

// ─── Auth ───
export const auth = {
  me: () =>
    get<{ id: string; email: string; name: string; avatar_url: string; is_admin: boolean; role: string }>("/portal/me"),
  providers: () => get<{ dev_login: boolean; otp: boolean }>("/auth/providers"),
  logout: () => post<void>("/auth/logout"),
  devLogin: (email: string, name: string) => post<{ status: string }>("/auth/dev-login", { email, name }),
  otpSend: (email: string) => post<{ status: string }>("/auth/otp/send", { email }),
  otpVerify: (email: string, code: string) =>
    post<{ status: string; email: string; name: string; is_admin: boolean; role: string }>("/auth/otp/verify", {
      email,
      code,
    }),
}

// ─── Checkout ───
export const checkout = {
  verify: (sessionId: string) =>
    get<{ status: string; email?: string; kind?: string }>(`/checkout/verify?session_id=${sessionId}`),
}

// ─── Invites (public, token-only) ───
// The plain invite token IS the email-ownership proof — no session
// required. Backend collapses bad/expired/already-accepted into one
// generic error so an attacker can't probe token validity.
export const invites = {
  accept: (token: string) =>
    post<{ user_id: string; email: string; license_id: string; product_name?: string; role: string }>(
      "/invites/accept",
      { token },
    ),
}

// ─── Portal ───
export const portal = {
  licenses: () => get<{ licenses: PortalLicense[]; renewals_enabled?: boolean }>("/portal/licenses"),
  listPlans: (productId: string) => get<{ plans: Plan[] }>(`/portal/plans?product_id=${productId}`),
  updateProfile: (data: { name: string }) =>
    put<{ id: string; email: string; name: string; avatar_url: string; role: string }>("/portal/profile", data),
  recordUsage: (data: { license_key: string; feature: string; quantity?: number }) =>
    post<{ status?: string }>("/portal/usage", data),
  quotaStatus: (data: { license_key: string; feature: string }) =>
    post<{ used?: number; limit?: number; remaining?: number }>("/portal/usage/status", data),
  // Customer-facing team management for multi-seat plans. Session-
  // authed (cookie); the body's license_key only names the target
  // license — the cookie is the actual authentication.
  // Self-service device/activation management. Backend authorises the
  // license owner OR any accepted seat (member or admin) — a teammate
  // who lost a laptop can free their own slot without a support ticket
  // (internal/handler/portal_activations.go). license_key is in the
  // path; the session cookie is the actual auth.
  listActivations: (licenseKey: string) =>
    get<{ activations: Activation[]; max: number }>(`/portal/licenses/${encodeURIComponent(licenseKey)}/activations`),
  removeActivation: (licenseKey: string, activationId: string) =>
    del<{ status: string }>(
      `/portal/licenses/${encodeURIComponent(licenseKey)}/activations/${encodeURIComponent(activationId)}`,
    ),
  listSeats: (licenseKey: string) => post<{ seats: Seat[] }>("/portal/seats", { license_key: licenseKey }),
  addSeat: (data: { license_key: string; email: string; role?: string }) => post<Seat>("/portal/seats/add", data),
  removeSeat: (data: { license_key: string; seat_id: string }) =>
    post<{ status: string }>("/portal/seats/remove", data),
  changePlan: (data: { license_id: string; new_price_id: string; prorate?: boolean }) =>
    post<{ status: string; new_plan_id: string; new_plan_name: string; proration: string }>(
      "/portal/subscription/change-plan",
      data,
    ),
  cancelSubscription: (data: { license_id: string; immediate?: boolean }) =>
    post<{ status: string; immediate: boolean }>("/portal/subscription/cancel", data),
  getBillingPortal: (data: { license_id: string }) =>
    post<{ url: string }>("/portal/subscription/billing-portal", data),
  getInvoices: (licenseId: string) =>
    get<{ invoices: Invoice[] }>(`/portal/subscription/invoices?license_id=${licenseId}`),
  renewUpdates: (data: { license_id: string }) => post<{ url: string }>("/portal/updates/renew", data),
}

// ─── Admin ───
// Every list endpoint answers with its rows under their own name and
// the same three numbers beside them. `limit` is what the server
// applied, not what was asked for — it clamps.
// Builds a list request's query string. Empty and undefined values
// are left out so a filter that is not set does not become one that
// matches the empty string.
function listQuery(params?: Record<string, string | number | undefined>) {
  const q = new URLSearchParams()
  for (const [k, v] of Object.entries(params || {})) {
    if (v !== undefined && v !== "" && v !== null) q.set(k, String(v))
  }
  return q.toString()
}

export type Paged<T> = T & { total: number; limit: number; offset: number }

export const admin = {
  stats: () => get<Stats>("/admin/stats"),

  listProducts: (params?: { search?: string; type?: string; limit?: number; offset?: number }) =>
    get<Paged<{ products: Product[] }>>(`/admin/products?${listQuery(params)}`),
  getProduct: (id: string) => get<Product>(`/admin/products/${id}`),
  createProduct: (data: { name: string; slug: string; type: string; feed_license_required?: boolean }) =>
    post<Product>("/admin/products", data),
  updateProduct: (id: string, data: Partial<Product>) => put<Product>(`/admin/products/${id}`, data),
  deleteProduct: (id: string) => del(`/admin/products/${id}`),

  listPlans: (params?: { product_id?: string; search?: string; limit?: number; offset?: number }) =>
    get<Paged<{ plans: Plan[] }>>(`/admin/plans?${listQuery(params)}`),
  getPlan: (id: string) => get<Plan>(`/admin/plans/${id}`),
  createPlan: (data: Partial<Plan>) => post<Plan>("/admin/plans", data),
  updatePlan: (id: string, data: Partial<Plan>) => put<Plan>(`/admin/plans/${id}`, data),
  deletePlan: (id: string) => del(`/admin/plans/${id}`),

  createEntitlement: (data: { plan_id: string; feature: string; value_type: string; value: string }) =>
    post<Entitlement>("/admin/entitlements", data),
  updateEntitlement: (id: string, data: Partial<Entitlement>) => put<Entitlement>(`/admin/entitlements/${id}`, data),
  deleteEntitlement: (id: string) => del(`/admin/entitlements/${id}`),

  listLicenses: (params?: {
    product_id?: string
    status?: string
    search?: string
    external_customer_id?: string
    external_workspace_id?: string
    // Server-side ordering. The API refuses a column it does not
    // know rather than quietly serving a different order, so these
    // have to match its allowlist: created_at, valid_until, email,
    // status, product, plan.
    sort?: string
    order?: "asc" | "desc"
    offset?: number
    limit?: number
  }) => {
    const q = new URLSearchParams()
    if (params?.product_id) q.set("product_id", params.product_id)
    if (params?.status) q.set("status", params.status)
    if (params?.search) q.set("search", params.search)
    if (params?.external_customer_id) q.set("external_customer_id", params.external_customer_id)
    if (params?.external_workspace_id) q.set("external_workspace_id", params.external_workspace_id)
    if (params?.sort) q.set("sort", params.sort)
    if (params?.order) q.set("order", params.order)
    if (params?.offset) q.set("offset", String(params.offset))
    if (params?.limit) q.set("limit", String(params.limit))
    return get<{
      licenses: License[]
      total: number
      limit: number
      offset: number
      license_key_hints: Record<string, string>
    }>(`/admin/licenses?${q}`)
  },
  // The licence keeps its own shape; only the key is gone, replaced by
  // a last-four hint.
  getLicense: (id: string) => get<License & { license_key_hint: string }>(`/admin/licenses/${id}`),
  // The key is never in a list or detail payload — one explicit
  // request per key, audited server-side.
  revealLicenseKey: (id: string) => get<{ license_key: string }>(`/admin/licenses/${id}/key`),
  // Re-queues the "here is your key" mail. The address is the one on
  // the licence; the server does not accept one from here.
  resendLicenseEmail: (id: string) =>
    post<{ queued: boolean; email: string }>(`/admin/licenses/${id}/resend-email`, {}),
  createLicense: (data: {
    product_id: string
    plan_id: string
    email: string
    notes?: string
    external_customer_id?: string
    external_workspace_id?: string
    valid_until?: string
  }) => post<License>("/admin/licenses", data),
  // Empty valid_until clears the expiry (perpetual license).
  setLicenseValidUntil: (id: string, validUntil: string) =>
    post<License>(`/admin/licenses/${id}/valid-until`, { valid_until: validUntil }),
  // Empty updates_until means updates for life.
  setLicenseUpdatesUntil: (id: string, updatesUntil: string) =>
    post<License>(`/admin/licenses/${id}/updates-until`, { updates_until: updatesUntil }),
  revokeLicense: (id: string) => post(`/admin/licenses/${id}/revoke`),
  suspendLicense: (id: string) => post(`/admin/licenses/${id}/suspend`),
  reinstateLicense: (id: string) => post(`/admin/licenses/${id}/reinstate`),
  refundLicense: (id: string) => post(`/admin/licenses/${id}/refund`),

  deleteActivation: (id: string) => del(`/admin/activations/${id}`),

  listAPIKeys: (params?: { product_id?: string; search?: string; limit?: number; offset?: number }) =>
    get<Paged<{ api_keys: APIKey[] }>>(`/admin/api-keys?${listQuery(params)}`),
  createAPIKey: (data: { product_id?: string; name: string; scopes?: string[] }) =>
    post<APIKey & { key: string }>("/admin/api-keys", data),
  rotateAPIKey: (id: string) => post<APIKey & { key: string }>(`/admin/api-keys/${id}/rotate`, {}),
  deleteAPIKey: (id: string) => del(`/admin/api-keys/${id}`),

  listWebhooks: (params?: { product_id?: string; search?: string; limit?: number; offset?: number }) =>
    get<Paged<{ webhooks: WebhookConfig[] }>>(`/admin/webhooks?${listQuery(params)}`),
  createWebhook: (data: { product_id: string; url: string; events: string[] }) =>
    post<WebhookConfig & { secret: string }>("/admin/webhooks", data),
  updateWebhook: (id: string, data: Partial<WebhookConfig>) => put<WebhookConfig>(`/admin/webhooks/${id}`, data),
  deleteWebhook: (id: string) => del(`/admin/webhooks/${id}`),
  listWebhookDeliveries: (
    id: string,
    params?: { offset?: number; limit?: number; status?: string; event?: string },
  ) => {
    const q = new URLSearchParams()
    if (params?.offset) q.set("offset", String(params.offset))
    if (params?.limit) q.set("limit", String(params.limit))
    if (params?.status) q.set("status", params.status)
    if (params?.event) q.set("event", params.event)
    return get<{ deliveries: WebhookDeliveryLog[]; total: number }>(`/admin/webhooks/${id}/deliveries?${q}`)
  },
  resendWebhookDelivery: (webhookId: string, deliveryId: string) =>
    post<WebhookDeliveryLog>(`/admin/webhooks/${webhookId}/deliveries/${deliveryId}/resend`),
  testWebhook: (id: string) =>
    post<{ status: string; delivery_id: string; response_code: number; response_body: string }>(
      `/admin/webhooks/${id}/test`,
    ),

  getLicenseUsage: (id: string, params?: { feature?: string; offset?: number; limit?: number }) => {
    const q = new URLSearchParams()
    if (params?.feature) q.set("feature", params.feature)
    if (params?.offset) q.set("offset", String(params.offset))
    if (params?.limit) q.set("limit", String(params.limit))
    return get<{ events: UsageEvent[]; counters: UsageCounter[]; total: number }>(`/admin/licenses/${id}/usage?${q}`)
  },
  resetUsageCounter: (id: string, data: { feature: string; period?: string; period_key?: string }) =>
    post(`/admin/licenses/${id}/usage/reset`, data),

  getLicenseSeats: (id: string, params?: { limit?: number; offset?: number }) =>
    get<Paged<{ seats: Seat[]; active_count: number }>>(`/admin/licenses/${id}/seats?${listQuery(params)}`),

  getAnalytics: (params?: { product_id?: string; from?: string; to?: string; granularity?: string }) => {
    const q = new URLSearchParams()
    if (params?.product_id) q.set("product_id", params.product_id)
    if (params?.from) q.set("from", params.from)
    if (params?.to) q.set("to", params.to)
    if (params?.granularity) q.set("granularity", params.granularity)
    return get<{ snapshots: AnalyticsSnapshot[] | AggregatedSnapshot[] }>(`/admin/analytics?${q}`)
  },

  getAnalyticsSummary: (params?: {
    product_id?: string
    plan_id?: string
    license_type?: string
    status?: string
    from?: string
    to?: string
  }) => {
    const q = new URLSearchParams()
    if (params?.product_id) q.set("product_id", params.product_id)
    if (params?.plan_id) q.set("plan_id", params.plan_id)
    if (params?.license_type) q.set("license_type", params.license_type)
    if (params?.status) q.set("status", params.status)
    if (params?.from) q.set("from", params.from)
    if (params?.to) q.set("to", params.to)
    return get<AnalyticsSummary>(`/admin/analytics/summary?${q}`)
  },

  getAnalyticsBreakdown: (params: {
    product_id?: string
    plan_id?: string
    license_type?: string
    status?: string
    from?: string
    to?: string
    dimension: string
  }) => {
    const q = new URLSearchParams()
    if (params.product_id) q.set("product_id", params.product_id)
    if (params.plan_id) q.set("plan_id", params.plan_id)
    if (params.license_type) q.set("license_type", params.license_type)
    if (params.status) q.set("status", params.status)
    if (params.from) q.set("from", params.from)
    if (params.to) q.set("to", params.to)
    q.set("dimension", params.dimension)
    return get<{ items: BreakdownItem[] }>(`/admin/analytics/breakdown?${q}`)
  },

  getAnalyticsUsageTop: (params?: { product_id?: string; from?: string; to?: string; limit?: number }) => {
    const q = new URLSearchParams()
    if (params?.product_id) q.set("product_id", params.product_id)
    if (params?.from) q.set("from", params.from)
    if (params?.to) q.set("to", params.to)
    if (params?.limit) q.set("limit", String(params.limit))
    return get<{ features: FeatureUsageItem[] }>(`/admin/analytics/usage-top?${q}`)
  },

  getAnalyticsActivationTrend: (params?: { product_id?: string; from?: string; to?: string }) => {
    const q = new URLSearchParams()
    if (params?.product_id) q.set("product_id", params.product_id)
    if (params?.from) q.set("from", params.from)
    if (params?.to) q.set("to", params.to)
    return get<{ trend: TrendPoint[] }>(`/admin/analytics/activation-trend?${q}`)
  },

  // Cuts a licence loose from a Stripe subscription that is over, so
  // it can be managed locally again. Refused (409) while Stripe still
  // has the subscription.
  unlinkStripeSubscription: (id: string) => post<{ status: string }>(`/admin/licenses/${id}/stripe/unlink`, {}),
  changeLicensePlan: (id: string, data: { plan_id: string }) => post(`/admin/licenses/${id}/change-plan`, data),

  // Addons
  listAddons: (params?: { product_id?: string; search?: string; limit?: number; offset?: number }) =>
    get<Paged<{ addons: Addon[] }>>(`/admin/addons?${listQuery(params)}`),
  createAddon: (data: Partial<Addon>) => post<Addon>("/admin/addons", data),
  updateAddon: (id: string, data: Partial<Addon>) => put<Addon>(`/admin/addons/${id}`, data),
  deleteAddon: (id: string) => del(`/admin/addons/${id}`),
  getLicenseAddons: (id: string, params?: { limit?: number; offset?: number }) =>
    get<Paged<{ addons: LicenseAddon[] }>>(`/admin/licenses/${id}/addons?${listQuery(params)}`),
  addLicenseAddon: (id: string, addonId: string) =>
    post<LicenseAddon>(`/admin/licenses/${id}/addons`, { addon_id: addonId }),
  removeLicenseAddon: (id: string, addonId: string) => del(`/admin/licenses/${id}/addons/${addonId}`),
  getFloatingSessions: (id: string, params?: { limit?: number; offset?: number }) =>
    get<Paged<{ sessions: FloatingSession[]; active: number }>>(`/admin/licenses/${id}/floating?${listQuery(params)}`),

  listAuditLogs: (params?: {
    entity?: string
    entity_id?: string
    product_id?: string
    offset?: number
    limit?: number
  }) => {
    const q = new URLSearchParams()
    if (params?.entity) q.set("entity", params.entity)
    if (params?.entity_id) q.set("entity_id", params.entity_id)
    if (params?.product_id) q.set("product_id", params.product_id)
    if (params?.offset) q.set("offset", String(params.offset))
    if (params?.limit) q.set("limit", String(params.limit))
    return get<{ audit_logs: AuditLog[]; total: number }>(`/admin/audit-logs?${q}`)
  },

  listUsers: (params?: { search?: string; offset?: number; limit?: number }) => {
    const q = new URLSearchParams()
    if (params?.search) q.set("search", params.search)
    if (params?.offset) q.set("offset", String(params.offset))
    if (params?.limit) q.set("limit", String(params.limit))
    return get<{ users: User[]; total: number }>(`/admin/users?${q}`)
  },

  // Team (admin management)
  listTeam: (params?: { limit?: number; offset?: number }) =>
    get<Paged<{ members: User[] }>>(`/admin/team?${listQuery(params)}`),
  inviteTeamMember: (data: { email: string; role?: string }) => post<User>("/admin/team", data),
  removeTeamMember: (id: string) => del(`/admin/team/${id}`),

  getAnalyticsInsights: (params?: { product_id?: string }) => {
    const q = new URLSearchParams()
    if (params?.product_id) q.set("product_id", params.product_id)
    return get<AnalyticsInsights>(`/admin/analytics/insights?${q}`)
  },

  getUserDetail: (id: string) => get<UserDetail & { license_key_hints: Record<string, string> }>(`/admin/users/${id}`),

  // Settings
  getSettings: () =>
    get<{
      settings: Record<string, string>
      secrets_set?: Record<string, boolean>
      email?: { configured: boolean; provider: string; source: string; host: string; from: string }
    }>("/admin/settings"),
  updateSettings: (settings: Record<string, string>) => put<{ status: string }>("/admin/settings", { settings }),
  sendTestEmail: (to?: string) => post<{ status: string }>("/admin/settings/test-email", to ? { to } : {}),
  clearSecretSetting: (key: string) => del<{ status: string; key: string }>(`/admin/settings/secrets/${key}`),

  // Email Templates
  getEmailTemplates: () =>
    get<{ templates: Record<string, { custom: string; default: string }> }>("/admin/email-templates"),

  // System
  getVersion: () => get<{ version: string; commit: string; build_date: string }>("/version"),
  checkUpdate: () =>
    get<{
      available: boolean
      latest_version: string
      current_version: string
      release_url?: string
      release_date?: string
      changelog?: string
      update_command?: string
      checked_at?: string
    }>("/admin/system/update-check"),

  // ─── Releases (industry-standard bundle model) ───
  listReleases: (params?: {
    product_id?: string
    channel?: string
    status?: string
    limit?: number
    offset?: number
  }) => {
    const q = new URLSearchParams()
    if (params?.product_id) q.set("product_id", params.product_id)
    if (params?.channel) q.set("channel", params.channel)
    if (params?.status) q.set("status", params.status)
    if (params?.limit) q.set("limit", String(params.limit))
    if (params?.offset) q.set("offset", String(params.offset))
    return get<{ releases: Release[]; total: number; limit: number; offset: number }>(`/admin/releases?${q}`)
  },
  getRelease: (id: string) => get<Release>(`/admin/releases/${id}`),
  createRelease: (data: {
    product_id: string
    version: string
    channel?: string
    name?: string
    release_notes?: string
  }) => post<Release>("/admin/releases", data),
  addArtifact: (
    releaseId: string,
    data: {
      platform: string
      content_type?: string
      expected_size?: number
      filename?: string
    },
  ) =>
    post<{ artifact: ReleaseArtifact; upload_url: string; expires_at: string }>(
      `/admin/releases/${releaseId}/artifacts`,
      data,
    ),
  finalizeArtifact: (releaseId: string, artifactId: string, data: { expected_sha256?: string }) =>
    post<ReleaseArtifact>(`/admin/releases/${releaseId}/artifacts/${artifactId}/finalize`, data),
  deleteArtifact: (releaseId: string, artifactId: string) =>
    del(`/admin/releases/${releaseId}/artifacts/${artifactId}`),
  publishRelease: (id: string) => post<Release>(`/admin/releases/${id}/actions/publish`),
  yankRelease: (id: string, reason: string) => post<Release>(`/admin/releases/${id}/actions/yank`, { reason }),
  unyankRelease: (id: string) => post<Release>(`/admin/releases/${id}/actions/unyank`),
  updateReleaseNotes: (id: string, data: { name?: string; release_notes?: string }) =>
    patch<Release>(`/admin/releases/${id}`, data),
  deleteRelease: (id: string) => del(`/admin/releases/${id}`),

  // ─── Release signing keys (per product) ───
  listSigningKeys: (productId: string) =>
    get<{ keys: ReleaseSigningKey[] }>(`/admin/products/${productId}/signing-keys`),
  generateSigningKey: (productId: string) => post<ReleaseSigningKey>(`/admin/products/${productId}/signing-key`),
  rotateSigningKey: (productId: string, note: string) =>
    post<ReleaseSigningKey>(`/admin/products/${productId}/signing-key/rotate`, { note }),
  deactivateSigningKey: (productId: string, note: string) =>
    request<{ status: string }>(`/admin/products/${productId}/signing-key`, {
      method: "DELETE",
      body: JSON.stringify({ note }),
    }),
  publicKeyURL: (productId: string) => `${BASE}/admin/products/${productId}/signing-key/public.pem`,
}

// ─── Types ───

export interface User {
  id: string
  email: string
  name: string
  avatar_url?: string
  role: string // "owner" | "admin" | "user"
  created_at: string
  updated_at: string
}

export interface Product {
  id: string
  name: string
  slug: string
  type: string
  minimum_supported_version?: string
  minimum_supported_message?: string
  require_signing: boolean
  // Update feeds answer only with a license key (maintenance-period products).
  feed_license_required?: boolean
  feed_gated_at?: string
  created_at: string
}

export interface Plan {
  id: string
  product_id: string
  name: string
  slug: string
  checkout_id: string
  license_type: string
  billing_interval?: string
  max_activations: number
  license_model?: string
  floating_timeout?: number
  token_ttl_days?: number
  max_seats: number
  trial_days: number
  grace_days: number
  stripe_price_id?: string
  // Maintenance period (perpetual plans): days of updates a purchase
  // includes (0 = for life) and the one-time renewal sold in the portal.
  updates_days?: number
  renewal_days?: number
  stripe_renewal_price_id?: string
  active: boolean
  sort_order: number
  created_at: string
  product?: Product
  entitlements?: Entitlement[]
}

export interface Entitlement {
  id: string
  plan_id: string
  feature: string
  value_type: string
  value: string
  quota_period?: string
  quota_unit?: string
}

export interface License {
  id: string
  product_id: string
  plan_id: string
  user_id?: string
  email: string
  // Present only where the server deliberately attaches it: the
  // customer portal (their own key) and the create-license response.
  // Admin list/detail carry a last-four hint instead — see PortalLicense.
  license_key?: string
  payment_provider?: string
  // Set only for subscription-backed Stripe licenses. A one-time
  // purchase has no subscription, so nothing to change, cancel, or
  // update a payment method for.
  stripe_subscription_id?: string
  status: string
  valid_from: string
  valid_until?: string
  // End of a perpetual license's update period; absent = no separate limit.
  updates_until?: string
  canceled_at?: string
  suspended_at?: string
  org_name?: string
  notes?: string
  external_customer_id?: string
  external_workspace_id?: string
  created_at: string
  updated_at: string
  product?: Product
  plan?: Plan
  // How many activations the licence has. Present on list rows, where
  // the activations themselves are not; the detail payload carries
  // both and they agree.
  activation_count?: number
  // Floating seats in use right now. A floating plan keeps occupancy
  // in its own table, so activation_count reads zero for one however
  // full it is; this is the number that answers "how full" there.
  active_session_count?: number
  activations?: Activation[]
  seats?: Seat[]
  addons?: LicenseAddon[]
}

// The portal always receives the key: it is the customer's own
// credential and also names the license in the activation and seat
// endpoints. Admin payloads never carry it.
export interface PortalLicense extends License {
  license_key: string
}

export interface Activation {
  id: string
  license_id: string
  identifier: string
  identifier_type: string
  label?: string
  ip_address?: string
  last_verified: string
  created_at: string
}

export interface APIKey {
  id: string
  product_id: string
  name: string
  prefix: string
  scopes: string[]
  last_used?: string
  last_used_ip?: string
  created_at: string
  product?: Product
}

export interface AuditLog {
  id: string
  entity: string
  entity_id: string
  action: string
  actor_id?: string
  actor_type?: string
  changes?: Record<string, unknown>
  ip_address?: string
  created_at: string
}

export interface Stats {
  total_licenses: number
  active_licenses: number
  total_activations: number
  total_products: number
  total_seats: number
  total_usage_events: number
  total_webhooks: number
  by_status: Record<string, number>
  recent_licenses: License[]
}

export interface Seat {
  id: string
  license_id: string
  email: string
  role: string
  user_id?: string
  invited_at: string
  accepted_at?: string
  removed_at?: string
  created_at: string
}

export interface UsageEvent {
  id: string
  license_id: string
  feature: string
  quantity: number
  metadata?: Record<string, unknown>
  ip_address?: string
  recorded_at: string
}

export interface UsageCounter {
  id: string
  license_id: string
  feature: string
  period: string
  period_key: string
  used: number
  updated_at: string
}

export interface WebhookConfig {
  id: string
  product_id: string
  url: string
  events: string[]
  active: boolean
  created_at: string
  updated_at: string
  product?: Product
}

export interface WebhookDeliveryLog {
  id: string
  webhook_id: string
  event: string
  payload?: Record<string, unknown>
  response_code?: number
  response_body?: string
  status: string
  attempts: number
  created_at: string
  delivered_at?: string
}

export interface AnalyticsSnapshot {
  id: string
  date: string
  product_id: string
  total_licenses: number
  active_licenses: number
  new_licenses: number
  churned: number
  total_activations: number
  total_seats: number
  total_usage: number
}

export interface AnalyticsSummary {
  total_licenses: number
  active_licenses: number
  trialing_licenses: number
  expired_licenses: number
  canceled_licenses: number
  suspended_licenses: number
  revoked_licenses: number
  past_due_licenses: number
  total_activations: number
  total_seats: number
  avg_activations_per_license: number
}

export interface BreakdownItem {
  key: string
  label: string
  count: number
}

export interface FeatureUsageItem {
  feature: string
  total_usage: number
  unique_users: number
}

export interface TrendPoint {
  date: string
  count: number
}

export interface AggregatedSnapshot {
  period: string
  total_licenses: number
  active_licenses: number
  new_licenses: number
  churned: number
  total_activations: number
  total_seats: number
  total_usage: number
}

export interface FloatingSession {
  id: string
  license_id: string
  identifier: string
  label?: string
  ip_address?: string
  checked_out: string
  expires_at: string
  heartbeat: string
}

export interface Addon {
  id: string
  product_id: string
  name: string
  slug: string
  description?: string
  feature: string
  value_type: string
  value: string
  quota_period?: string
  quota_unit?: string
  active: boolean
  sort_order: number
  created_at: string
  product?: Product
}

export interface LicenseAddon {
  id: string
  license_id: string
  addon_id: string
  enabled: boolean
  created_at: string
  addon?: Addon
}

export interface MeteredBilling {
  id: string
  license_id: string
  feature: string
  quantity: number
  period_key: string
  synced: boolean
  created_at: string
}

export interface Subscription {
  id: string
  license_id: string
  user_id?: string
  plan_id: string
  status: string
  payment_provider?: string
  external_id?: string
  current_period_start?: string
  current_period_end?: string
  cancel_at_period_end: boolean
  canceled_at?: string
  trial_start?: string
  trial_end?: string
  metadata?: Record<string, unknown>
  created_at: string
  updated_at: string
  license?: License
  plan?: Plan
}

export interface GrowthMetrics {
  net_growth_rate: number
  trial_conversion: number
  avg_license_age_days: number
  median_license_age_days: number
  total_trials: number
  converted_trials: number
  new_last_30d: number
  churned_last_30d: number
}

export interface RetentionData {
  period: string
  start_count: number
  end_count: number
  retention_pct: number
  churn_pct: number
}

export interface LicenseAgeDistribution {
  bucket: string
  count: number
}

export interface RecentActivity {
  id: string
  entity: string
  entity_id: string
  action: string
  actor_type: string
  created_at: string
}

export interface TopUser {
  email: string
  user_id: string
  license_count: number
  active_count: number
  total_usage: number
  activation_count: number
}

export interface AnalyticsInsights {
  growth: GrowthMetrics
  age_distribution: LicenseAgeDistribution[]
  top_users: TopUser[]
  retention: RetentionData[]
  recent_activity: RecentActivity[]
}

export interface Invoice {
  id: string
  number: string
  status: string
  amount_due: number
  amount_paid: number
  currency: string
  created: number
  period_start: number
  period_end: number
  invoice_pdf: string
  hosted_url: string
}

export interface UserDetail {
  user: User
  licenses: License[]
  subscriptions: Subscription[]
  total_usage: number
  active_seats: number
  activations: number
  recent_audit_logs: AuditLog[]
}

// Release: a logical version event for a product. Contains many platform
// artifacts. Lifecycle status is on the release; yank affects all artifacts.
export interface Release {
  id: string
  product_id: string
  version: string
  channel: "stable" | "beta" | "alpha" | "dev"
  name: string
  release_notes: string
  status: "draft" | "published" | "yanked"
  yanked_reason?: string
  published_at?: string
  yanked_at?: string
  created_at: string
  updated_at: string
  product?: Product
  artifacts?: ReleaseArtifact[]
}

// ReleaseArtifact: per-platform binary inside a Release.
export interface ReleaseArtifact {
  id: string
  release_id: string
  platform: string
  file_key: string
  file_size: number
  sha256: string
  ed25519_sig: string
  content_type: string
  signing_key_id?: string
  created_at: string
  updated_at: string
}

export interface ReleaseSigningKey {
  id: string
  product_id: string
  public_key: string
  active: boolean
  note?: string
  created_at: string
  rotated_at?: string
}

export const RELEASE_PLATFORMS = [
  "darwin-arm64",
  "darwin-x64",
  "windows-arm64",
  "windows-x64",
  "linux-arm64",
  "linux-x64",
  "linux-armhf",
] as const

export const RELEASE_CHANNELS = ["stable", "beta", "alpha", "dev"] as const
