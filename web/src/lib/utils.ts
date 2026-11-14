import { type ClassValue, clsx } from "clsx"
import { twMerge } from "tailwind-merge"

export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs))
}

// dateLocale is the locale dates are written in: the browser's language
// and region preference (en-US → 09/30/2026, en-GB → 30/09/2026,
// zh-CN → 2026/09/30), not the dashboard's UI language — an English
// dashboard does not mean the reader writes dates the American way.
export function dateLocale(): string | undefined {
  return navigator.languages?.[0] || navigator.language || undefined
}

export function formatDate(
  date: string | Date | null | undefined,
  options?: { locale?: string; timezone?: string },
): string {
  if (!date) return "-"
  const locale = options?.locale || dateLocale()
  const tz = options?.timezone || Intl.DateTimeFormat().resolvedOptions().timeZone
  return new Date(date).toLocaleDateString(locale, {
    year: "numeric",
    month: "2-digit",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
    timeZone: tz,
  })
}

export function boolColor(active: boolean): string {
  return active ? "bg-emerald-100 text-emerald-800" : "bg-gray-100 text-gray-800"
}

export function statusColor(status: string): string {
  switch (status) {
    case "active":
      return "bg-emerald-100 text-emerald-800"
    case "trialing":
      return "bg-blue-100 text-blue-800"
    case "past_due":
      return "bg-amber-100 text-amber-800"
    case "canceled":
      return "bg-gray-100 text-gray-800"
    case "expired":
      return "bg-red-100 text-red-700"
    case "suspended":
      return "bg-orange-100 text-orange-800"
    case "revoked":
      return "bg-red-200 text-red-900"
    default:
      return "bg-gray-100 text-gray-800"
  }
}

// Rows show only the tail of a key, after the prefix of the product
// that issued it. The full value never rides in a list or detail
// payload: it is fetched per reveal and audited.
export function maskKey(hint?: string, prefix?: string): string {
  if (!hint) return "••••"
  return prefix ? `${prefix}-••••-${hint}` : `••••-${hint}`
}

// formatBytes converts a raw byte count into a human-readable string
// (e.g. for release artifact sizes).
export function formatBytes(n: number): string {
  if (n >= 1024 * 1024 * 1024) return `${(n / (1024 * 1024 * 1024)).toFixed(2)} GB`
  if (n >= 1024 * 1024) return `${(n / (1024 * 1024)).toFixed(2)} MB`
  if (n >= 1024) return `${(n / 1024).toFixed(1)} KB`
  return `${n} B`
}

// formatQuota formats a quota or usage count for display, abbreviating
// large numbers (e.g. for activation or seat counts).
export function formatQuota(n: number): string {
  if (n >= 1_000_000_000) return `${(n / 1_000_000_000).toFixed(1)} GB`
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)} MB`
  return n.toLocaleString()
}

// Stripe currency helpers — zero-decimal and three-decimal currency sets.
// formatMoney converts a Stripe amount (smallest currency unit) into a
// human-readable string using the browser's locale.
const STRIPE_ZERO_DECIMAL = new Set([
  "BIF",
  "CLP",
  "DJF",
  "GNF",
  "JPY",
  "KMF",
  "KRW",
  "MGA",
  "PYG",
  "RWF",
  "UGX",
  "VND",
  "VUV",
  "XAF",
  "XOF",
  "XPF",
])
const STRIPE_THREE_DECIMAL = new Set(["BHD", "JOD", "KWD", "OMR", "TND"])

export function formatMoney(amount: number, currency: string, locale?: string): string {
  const code = (currency || "usd").toUpperCase()
  const digits = STRIPE_ZERO_DECIMAL.has(code) ? 0 : STRIPE_THREE_DECIMAL.has(code) ? 3 : 2
  try {
    const fmt = new Intl.NumberFormat(locale, { style: "currency", currency: code })
    return fmt.format(amount / 10 ** digits)
  } catch {
    return `${(amount / 100).toFixed(2)} ${code}`
  }
}
