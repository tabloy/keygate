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
