import { useQuery } from "@tanstack/react-query"
import { createContext, type ReactNode, useContext, useEffect, useMemo } from "react"
import { type Locale, useI18n } from "@/i18n"
import { site } from "@/lib/api"

interface SiteConfig {
  site_name: string
  brand_color: string
  logo_url: string
  timezone: string
  language: string
  attribution_text: string
  attribution_url: string
  loading: boolean
}

const defaults: SiteConfig = {
  site_name: "Keygate",
  brand_color: "",
  logo_url: "",
  timezone: "UTC",
  language: "",
  attribution_text: "Powered by Keygate",
  attribution_url: "https://keygate.app",
  loading: true,
}

// The settings page invalidates this key on save, so a new name, logo
// or colour shows at once instead of on the next reload.
export const SITE_CONFIG_KEY = ["site", "config"] as const

const SiteConfigContext = createContext<SiteConfig>(defaults)

export function SiteConfigProvider({ children }: { children: ReactNode }) {
  const { setLocale } = useI18n()
  const { data, isPending } = useQuery({
    queryKey: SITE_CONFIG_KEY,
    queryFn: site.config,
    staleTime: Number.POSITIVE_INFINITY,
    retry: false,
  })

  const config = useMemo<SiteConfig>(
    () => ({
      site_name: data?.site_name || defaults.site_name,
      brand_color: data?.brand_color || "",
      logo_url: data?.logo_url || "",
      timezone: data?.timezone || defaults.timezone,
      language: data?.language || "",
      attribution_text: data?.attribution_text || defaults.attribution_text,
      attribution_url: data?.attribution_url || defaults.attribution_url,
      loading: isPending,
    }),
    [data, isPending],
  )

  useEffect(() => {
    if (!data) return
    // Dynamic favicon from custom logo. index.html declares multiple
    // <link rel="icon"> variants and browsers pick their favorite (often
    // the sizes="32x32" one), so rewriting only the first link never
    // visibly changed the tab icon — update them all. A logo removed in
    // settings goes back to the bundled icon.
    document.querySelectorAll<HTMLLinkElement>("link[rel~='icon']").forEach((link) => {
      link.href = data.logo_url || "/favicon.svg"
    })
    if (data.brand_color) {
      document.documentElement.style.setProperty("--color-primary", data.brand_color)
    } else {
      document.documentElement.style.removeProperty("--color-primary")
    }
    document.title = data.site_name || defaults.site_name
  }, [data])

  // The site's default language, for a visitor who has not picked one.
  // Applied to the page now, not just stored for the next load.
  const language = data?.language
  useEffect(() => {
    if ((language === "en" || language === "zh") && !localStorage.getItem("keygate_locale")) {
      setLocale(language as Locale)
    }
  }, [language, setLocale])

  return <SiteConfigContext.Provider value={config}>{children}</SiteConfigContext.Provider>
}

export function useSiteConfig() {
  return useContext(SiteConfigContext)
}
