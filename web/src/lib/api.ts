const BASE = `${import.meta.env.VITE_API_URL || ""}/api/v1`

async function request<T>(path: string, opts?: RequestInit): Promise<T> {
  const res = await fetch(BASE + path, {
    credentials: "include",
    headers: { "Content-Type": "application/json", ...opts?.headers },
    ...opts,
  })

  // Handle session expiry: redirect to login on 401
  if (res.status === 401) {
    // Don't redirect if already on login page or fetching auth state
    if (!window.location.pathname.startsWith("/login") && path !== "/portal/me") {
      window.location.href = "/login"
    }
    throw new Error("Session expired")
  }

  if (res.status === 204) return undefined as T
  const json = await res.json()
  if (!res.ok) {
    throw new Error(json?.error?.message || json?.error || `Request failed: ${res.status}`)
  }
  return json.data !== undefined ? json.data : json
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
  providers: () => get<{ providers: string[]; dev_login: boolean }>("/auth/providers"),
  logout: () => post<void>("/auth/logout"),
  devLogin: (email: string, name: string) => post<{ status: string }>("/auth/dev-login", { email, name }),
}

// ─── Portal ───
export const portal = {
  licenses: () => get<{ licenses: License[] }>("/portal/licenses"),
  listPlans: (productId: string) => get<{ plans: Plan[] }>(`/portal/plans?product_id=${productId}`),
  updateProfile: (data: { name: string }) =>
    put<{ id: string; email: string; name: string; avatar_url: string; role: string }>("/portal/profile", data),
  recordUsage: (data: { license_key: string; feature: string; quantity?: number }) => post<any>("/portal/usage", data),
  quotaStatus: (data: { license_key: string; feature: string }) => post<any>("/portal/usage/status", data),
  listSeats: (data: { license_key: string }) => post<{ seats: Seat[] }>("/portal/seats", data),
  addSeat: (data: { license_key: string; email: string; role?: string }) => post<Seat>("/portal/seats/add", data),
  removeSeat: (data: { license_key: string; seat_id: string }) => post<any>("/portal/seats/remove", data),
  changePlan: (data: { license_id: string; new_price_id: string; prorate?: boolean }) =>
    post<{ status: string; new_plan_id: string; new_plan_name: string; proration: string }>(
      "/subscription/change-plan",
      data,
    ),
  cancelSubscription: (data: { license_id: string; immediate?: boolean }) =>
    post<{ status: string; immediate: boolean }>("/subscription/cancel", data),
  getBillingPortal: (data: { license_id: string }) => post<{ url: string }>("/subscription/billing-portal", data),
  getInvoices: (licenseId: string) => get<{ invoices: Invoice[] }>(`/subscription/invoices?license_id=${licenseId}`),
  createPayPalSubscription: (data: { plan_id: string; email?: string }) =>
    post<{ subscription_id: string; approve_url: string; status: string }>("/checkout/paypal", data),
  cancelPayPalSubscription: (data: { license_id: string; reason?: string }) =>
    post<{ status: string }>("/subscription/cancel-paypal", data),
}

// ─── Admin ───
export const admin = {
  stats: () => get<Stats>("/admin/stats"),

  listProducts: (params?: { search?: string }) => {
    const q = new URLSearchParams()
    if (params?.search) q.set("search", params.search)
    return get<{ products: Product[] }>(`/admin/products?${q}`)
  },
  getProduct: (id: string) => get<Product>(`/admin/products/${id}`),
  createProduct: (data: { name: string; slug: string; type: string }) => post<Product>("/admin/products", data),
  updateProduct: (id: string, data: Partial<Product>) => put<Product>(`/admin/products/${id}`, data),
  deleteProduct: (id: string) => del(`/admin/products/${id}`),

  listPlans: (productId?: string, search?: string) => {
    const q = new URLSearchParams()
    if (productId) q.set("product_id", productId)
    if (search) q.set("search", search)
    return get<{ plans: Plan[] }>(`/admin/plans?${q}`)
  },
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
    offset?: number
    limit?: number
  }) => {
    const q = new URLSearchParams()
    if (params?.product_id) q.set("product_id", params.product_id)
    if (params?.status) q.set("status", params.status)
    if (params?.search) q.set("search", params.search)
    if (params?.offset) q.set("offset", String(params.offset))
    if (params?.limit) q.set("limit", String(params.limit))
    return get<{ licenses: License[]; total: number }>(`/admin/licenses?${q}`)
  },
  getLicense: (id: string) => get<License>(`/admin/licenses/${id}`),
  createLicense: (data: { product_id: string; plan_id: string; email: string; notes?: string }) =>
    post<License>("/admin/licenses", data),
  revokeLicense: (id: string) => post(`/admin/licenses/${id}/revoke`),
  suspendLicense: (id: string) => post(`/admin/licenses/${id}/suspend`),
  reinstateLicense: (id: string) => post(`/admin/licenses/${id}/reinstate`),
  refundLicense: (id: string) => post(`/admin/licenses/${id}/refund`),

  deleteActivation: (id: string) => del(`/admin/activations/${id}`),

  listAPIKeys: (productId?: string, search?: string) => {
    const q = new URLSearchParams()
    if (productId) q.set("product_id", productId)
    if (search) q.set("search", search)
    return get<{ api_keys: APIKey[] }>(`/admin/api-keys?${q}`)
  },
  createAPIKey: (data: { product_id: string; name: string; scopes?: string[] }) =>
    post<APIKey & { key: string }>("/admin/api-keys", data),
  deleteAPIKey: (id: string) => del(`/admin/api-keys/${id}`),

  listWebhooks: (productId?: string, search?: string) => {
    const q = new URLSearchParams()
    if (productId) q.set("product_id", productId)
    if (search) q.set("search", search)
    return get<{ webhooks: WebhookConfig[] }>(`/admin/webhooks?${q}`)
  },
  createWebhook: (data: { product_id: string; url: string; events: string[] }) =>
    post<WebhookConfig & { secret: string }>("/admin/webhooks", data),
  updateWebhook: (id: string, data: Partial<WebhookConfig>) => put<WebhookConfig>(`/admin/webhooks/${id}`, data),
  deleteWebhook: (id: string) => del(`/admin/webhooks/${id}`),
  listWebhookDeliveries: (id: string, params?: { offset?: number; limit?: number }) => {
    const q = new URLSearchParams()
    if (params?.offset) q.set("offset", String(params.offset))
    if (params?.limit) q.set("limit", String(params.limit))
    return get<{ deliveries: WebhookDeliveryLog[]; total: number }>(`/admin/webhooks/${id}/deliveries?${q}`)
  },
  testWebhook: (id: string) => post(`/admin/webhooks/${id}/test`),

  getLicenseUsage: (id: string, params?: { feature?: string; offset?: number; limit?: number }) => {
    const q = new URLSearchParams()
    if (params?.feature) q.set("feature", params.feature)
    if (params?.offset) q.set("offset", String(params.offset))
    if (params?.limit) q.set("limit", String(params.limit))
    return get<{ events: UsageEvent[]; counters: UsageCounter[]; total: number }>(`/admin/licenses/${id}/usage?${q}`)
  },
  resetUsageCounter: (id: string, data: { feature: string; period?: string; period_key?: string }) =>
    post(`/admin/licenses/${id}/usage/reset`, data),

  getLicenseSeats: (id: string) => get<{ seats: Seat[]; active_count: number }>(`/admin/licenses/${id}/seats`),

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

  changeLicensePlan: (id: string, data: { plan_id: string }) => post(`/admin/licenses/${id}/change-plan`, data),

  // Addons
  listAddons: (productId?: string, search?: string) => {
    const q = new URLSearchParams()
    if (productId) q.set("product_id", productId)
    if (search) q.set("search", search)
    return get<{ addons: Addon[] }>(`/admin/addons?${q}`)
  },
  createAddon: (data: Partial<Addon>) => post<Addon>("/admin/addons", data),
  updateAddon: (id: string, data: Partial<Addon>) => put<Addon>(`/admin/addons/${id}`, data),
  deleteAddon: (id: string) => del(`/admin/addons/${id}`),
  getLicenseAddons: (id: string) => get<{ addons: LicenseAddon[] }>(`/admin/licenses/${id}/addons`),
  addLicenseAddon: (id: string, addonId: string) =>
    post<LicenseAddon>(`/admin/licenses/${id}/addons`, { addon_id: addonId }),
  removeLicenseAddon: (id: string, addonId: string) => del(`/admin/licenses/${id}/addons/${addonId}`),
  getFloatingSessions: (id: string) =>
    get<{ sessions: FloatingSession[]; active: number }>(`/admin/licenses/${id}/floating`),

  listAuditLogs: (params?: { entity?: string; entity_id?: string; offset?: number; limit?: number }) => {
    const q = new URLSearchParams()
    if (params?.entity) q.set("entity", params.entity)
    if (params?.entity_id) q.set("entity_id", params.entity_id)
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
  listTeam: () => get<{ members: User[] }>("/admin/team"),
  inviteTeamMember: (data: { email: string; role?: string }) => post<User>("/admin/team", data),
  removeTeamMember: (id: string) => del(`/admin/team/${id}`),

  getAnalyticsInsights: (params?: { product_id?: string }) => {
    const q = new URLSearchParams()
    if (params?.product_id) q.set("product_id", params.product_id)
    return get<AnalyticsInsights>(`/admin/analytics/insights?${q}`)
  },

  getUserDetail: (id: string) => get<UserDetail>(`/admin/users/${id}`),

  // Settings
  getSettings: () => get<{ settings: Record<string, string> }>("/admin/settings"),
  updateSettings: (settings: Record<string, string>) => put<{ status: string }>("/admin/settings", { settings }),
  sendTestEmail: () => post<{ status: string }>("/admin/settings/test-email"),

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
  getMigrations: () => get<{ migrations: { filename: string; applied_at: string }[] }>("/admin/system/migrations"),
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
  created_at: string
}

export interface Plan {
  id: string
  product_id: string
  name: string
  slug: string
  license_type: string
  billing_interval?: string
  max_activations: number
  license_model?: string
  floating_timeout?: number
  max_seats: number
  trial_days: number
  grace_days: number
  stripe_price_id?: string
  paypal_plan_id?: string
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
  license_key: string
  payment_provider?: string
  status: string
  valid_from: string
  valid_until?: string
  canceled_at?: string
  suspended_at?: string
  org_name?: string
  notes?: string
  created_at: string
  updated_at: string
  product?: Product
  plan?: Plan
  activations?: Activation[]
  seats?: Seat[]
  addons?: LicenseAddon[]
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
