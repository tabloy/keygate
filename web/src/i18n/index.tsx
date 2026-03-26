import { createContext, type ReactNode, useCallback, useContext, useEffect, useState } from "react"
import en from "./locales/en"
import zh from "./locales/zh"

type Locale = "en" | "zh"
type TranslationKeys = keyof typeof en
type Translations = Record<TranslationKeys, string>

const locales: Record<Locale, Translations> = { en, zh }

interface I18nContextType {
  locale: Locale
  setLocale: (locale: Locale) => void
  t: (key: TranslationKeys, params?: Record<string, string | number>) => string
}

const I18nContext = createContext<I18nContextType | null>(null)

const STORAGE_KEY = "keygate_locale"

export function I18nProvider({ children }: { children: ReactNode }) {
  const [locale, setLocaleState] = useState<Locale>(() => {
    const saved = localStorage.getItem(STORAGE_KEY) as Locale
    if (saved && locales[saved]) return saved
    const browserLang = navigator.language.toLowerCase()
    if (browserLang.startsWith("zh")) return "zh"
    return "en"
  })

  const setLocale = useCallback((l: Locale) => {
    setLocaleState(l)
    localStorage.setItem(STORAGE_KEY, l)
    document.documentElement.lang = l
  }, [])

  useEffect(() => {
    document.documentElement.lang = locale
  }, [locale])

  const t = useCallback(
    (key: TranslationKeys, params?: Record<string, string | number>): string => {
      let text = locales[locale]?.[key] || locales.en[key] || key
      if (params) {
        for (const [k, v] of Object.entries(params)) {
          text = text.replace(`{${k}}`, String(v))
        }
      }
      return text
    },
    [locale],
  )

  return <I18nContext.Provider value={{ locale, setLocale, t }}>{children}</I18nContext.Provider>
}

export function useI18n() {
  const ctx = useContext(I18nContext)
  if (!ctx) throw new Error("useI18n must be used within I18nProvider")
  return ctx
}

export type { Locale, TranslationKeys }
