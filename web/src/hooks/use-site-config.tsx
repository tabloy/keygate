import { useQuery } from "@tanstack/react-query"
import { createContext, type ReactNode, useContext, useEffect, useMemo } from "react"
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

const SiteConfigContext = createContext<SiteConfig>(defaults)

export function SiteConfigProvider({ children }: { children: ReactNode }) {
  const { data, isPending } = useQuery({
    queryKey: ["site", "config"],
    queryFn: site.config,
    staleTime: 300_000,
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
    document.querySelectorAll<HTMLLinkElement>("link[rel~='icon']").forEach((link) => {
      link.href = data.logo_url || "/favicon.svg"
    })
    if (data.brand_color) {
      document.documentElement.style.setProperty("--color-primary", data.brand_color)
    } else {
      document.documentElement.style.removeProperty("--color-primary")
    }
    document.title = data.site_name || defaults.site_name
    if (data.language && !localStorage.getItem("keygate_locale")) {
      localStorage.setItem("keygate_locale", data.language)
      document.documentElement.lang = data.language
    }
  }, [data])

  return <SiteConfigContext.Provider value={config}>{children}</SiteConfigContext.Provider>
}

export function useSiteConfig() {
  return useContext(SiteConfigContext)
}
