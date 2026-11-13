import { createContext, type ReactNode, useContext, useEffect, useMemo, useState } from "react"
import { site } from "@/lib/api"
import { formatDate } from "@/lib/utils"

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

const SiteConfigContext = createContext<SiteConfig>(defaults)

export function SiteConfigProvider({ children }: { children: ReactNode }) {
  const [config, setConfig] = useState<SiteConfig>(defaults)

  useEffect(() => {
    site
      .config()
      .then((data) => {
        setConfig({
          site_name: data.site_name || "Keygate",
          brand_color: data.brand_color || "",
          logo_url: data.logo_url || "",
          timezone: data.timezone || "UTC",
          language: data.language || "",
          attribution_text: data.attribution_text || "Powered by Keygate",
          attribution_url: data.attribution_url || "https://keygate.app",
          loading: false,
        })
        // Dynamic favicon from custom logo. index.html declares
        // multiple <link rel="icon"> variants and browsers pick their
        // favorite (often the sizes="32x32" one), so rewriting only
        // the first link never visibly changed the tab icon — update
        // them all.
        if (data.logo_url) {
          document.querySelectorAll<HTMLLinkElement>("link[rel~='icon']").forEach((link) => {
            link.href = data.logo_url
          })
        }
        if (data.brand_color) {
          document.documentElement.style.setProperty("--color-primary", data.brand_color)
        }
        if (data.site_name) {
          document.title = data.site_name
        }
        // Set default language if user hasn't explicitly chosen one
        if (data.language && !localStorage.getItem("keygate_locale")) {
          localStorage.setItem("keygate_locale", data.language)
          document.documentElement.lang = data.language
        }
      })
      .catch(() => setConfig({ ...defaults, loading: false }))
  }, [])

  return <SiteConfigContext.Provider value={config}>{children}</SiteConfigContext.Provider>
}

export function useSiteConfig() {
  return useContext(SiteConfigContext)
}

export function useDateFormatter() {
  const { timezone } = useSiteConfig()
  return useMemo(() => {
    let displayTimezone = timezone || "UTC"
    try {
      new Intl.DateTimeFormat(undefined, { timeZone: displayTimezone })
    } catch {
      displayTimezone = "UTC"
    }
    return (date: Parameters<typeof formatDate>[0], options?: Parameters<typeof formatDate>[1]) =>
      formatDate(date, { ...options, timezone: options?.timezone || displayTimezone })
  }, [timezone])
}
