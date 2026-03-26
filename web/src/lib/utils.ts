import { type ClassValue, clsx } from "clsx"
import { twMerge } from "tailwind-merge"

export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs))
}

export function formatDate(
  date: string | Date | null | undefined,
  options?: { locale?: string; timezone?: string },
): string {
  if (!date) return "-"
  const locale = options?.locale || document.documentElement.lang || navigator.language || undefined
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
