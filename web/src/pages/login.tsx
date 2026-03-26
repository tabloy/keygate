import { Terminal } from "lucide-react"
import { useEffect, useState } from "react"
import { Navigate } from "react-router-dom"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Separator } from "@/components/ui/separator"
import { useAuth } from "@/hooks/use-auth"
import { useSiteConfig } from "@/hooks/use-site-config"
import { useI18n } from "@/i18n"
import { auth } from "@/lib/api"

export default function LoginPage() {
  const { t } = useI18n()
  const { site_name, logo_url, attribution_text, attribution_url } = useSiteConfig()
  const { user, loading, refetch } = useAuth()
  const [providers, setProviders] = useState<string[]>([])
  const [devLogin, setDevLogin] = useState(false)
  const [devEmail, setDevEmail] = useState("admin@keygate.dev")
  const [devName, setDevName] = useState("Admin")
  const [devLoading, setDevLoading] = useState(false)
  const [devError, setDevError] = useState("")

  useEffect(() => {
    auth
      .providers()
      .then((r) => {
        setProviders(r.providers)
        setDevLogin(r.dev_login)
      })
      .catch(() => {})
  }, [])

  const handleDevLogin = async (e: React.FormEvent) => {
    e.preventDefault()
    setDevLoading(true)
    setDevError("")
    try {
      await auth.devLogin(devEmail, devName)
      await refetch()
    } catch (err) {
      setDevError(err instanceof Error ? err.message : t("login.failed"))
    } finally {
      setDevLoading(false)
    }
  }

  if (loading) return null
  if (user) return <Navigate to={user.is_admin ? "/admin" : "/portal"} replace />

  return (
    <div className="flex flex-col items-center justify-center min-h-screen bg-muted/30">
      <Card className="w-full max-w-sm">
        <CardHeader className="text-center">
          <div className="flex justify-center mb-2">
            <img src={logo_url || "/logo.svg"} alt={site_name} className="h-12 w-12" />
          </div>
          <CardTitle className="text-2xl">{site_name}</CardTitle>
          <CardDescription>{t("login.subtitle")}</CardDescription>
        </CardHeader>
        <CardContent className="space-y-3">
          {providers.map((p) => (
            <Button key={p} variant="outline" className="w-full capitalize" asChild>
              <a href={`/api/v1/auth/${p}`}>
                {providerIcon(p)} {t("login.signInWith", { provider: p })}
              </a>
            </Button>
          ))}

          {devLogin && (
            <>
              {providers.length > 0 && (
                <div className="relative my-4">
                  <Separator />
                  <span className="absolute left-1/2 top-1/2 -translate-x-1/2 -translate-y-1/2 bg-card px-2 text-xs text-muted-foreground">
                    DEV MODE
                  </span>
                </div>
              )}
              <form onSubmit={handleDevLogin} className="space-y-3">
                <div className="space-y-2">
                  <Label>{t("common.email")}</Label>
                  <Input type="email" value={devEmail} onChange={(e) => setDevEmail(e.target.value)} required />
                </div>
                <div className="space-y-2">
                  <Label>{t("common.name")}</Label>
                  <Input value={devName} onChange={(e) => setDevName(e.target.value)} />
                </div>
                {devError && <p className="text-sm text-destructive">{devError}</p>}
                <Button type="submit" className="w-full" disabled={devLoading}>
                  <Terminal className="h-4 w-4 mr-2" />
                  {devLoading ? t("login.signingIn") : t("login.devLogin")}
                </Button>
                <p className="text-xs text-muted-foreground text-center">{t("login.devNote")}</p>
              </form>
            </>
          )}

          {providers.length === 0 && !devLogin && (
            <p className="text-sm text-muted-foreground text-center">{t("login.noProviders")}</p>
          )}
        </CardContent>
      </Card>
      {/* Attribution required by AGPL v3 Section 7(b) — see NOTICE */}
      <a
        href={attribution_url}
        target="_blank"
        rel="noopener noreferrer"
        className="mt-4 text-[10px] text-muted-foreground/40 hover:text-muted-foreground transition-colors"
      >
        {attribution_text}
      </a>
    </div>
  )
}

function providerIcon(name: string) {
  switch (name) {
    case "github":
      return (
        <svg className="h-4 w-4 mr-2" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true">
          <path d="M12 0c-6.626 0-12 5.373-12 12 0 5.302 3.438 9.8 8.207 11.387.599.111.793-.261.793-.577v-2.234c-3.338.726-4.033-1.416-4.033-1.416-.546-1.387-1.333-1.756-1.333-1.756-1.089-.745.083-.729.083-.729 1.205.084 1.839 1.237 1.839 1.237 1.07 1.834 2.807 1.304 3.492.997.107-.775.418-1.305.762-1.604-2.665-.305-5.467-1.334-5.467-5.931 0-1.311.469-2.381 1.236-3.221-.124-.303-.535-1.524.117-3.176 0 0 1.008-.322 3.301 1.23.957-.266 1.983-.399 3.003-.404 1.02.005 2.047.138 3.006.404 2.291-1.552 3.297-1.23 3.297-1.23.653 1.653.242 2.874.118 3.176.77.84 1.235 1.911 1.235 3.221 0 4.609-2.807 5.624-5.479 5.921.43.372.823 1.102.823 2.222v3.293c0 .319.192.694.801.576 4.765-1.589 8.199-6.086 8.199-11.386 0-6.627-5.373-12-12-12z" />
        </svg>
      )
    case "google":
      return (
        <svg className="h-4 w-4 mr-2" viewBox="0 0 24 24" aria-hidden="true">
          <path
            fill="#4285F4"
            d="M22.56 12.25c0-.78-.07-1.53-.2-2.25H12v4.26h5.92a5.06 5.06 0 0 1-2.2 3.32v2.77h3.57c2.08-1.92 3.28-4.74 3.28-8.1z"
          />
          <path
            fill="#34A853"
            d="M12 23c2.97 0 5.46-.98 7.28-2.66l-3.57-2.77c-.98.66-2.23 1.06-3.71 1.06-2.86 0-5.29-1.93-6.16-4.53H2.18v2.84C3.99 20.53 7.7 23 12 23z"
          />
          <path
            fill="#FBBC05"
            d="M5.84 14.09c-.22-.66-.35-1.36-.35-2.09s.13-1.43.35-2.09V7.07H2.18C1.43 8.55 1 10.22 1 12s.43 3.45 1.18 4.93l2.85-2.22.81-.62z"
          />
          <path
            fill="#EA4335"
            d="M12 5.38c1.62 0 3.06.56 4.21 1.64l3.15-3.15C17.45 2.09 14.97 1 12 1 7.7 1 3.99 3.47 2.18 7.07l3.66 2.84c.87-2.6 3.3-4.53 6.16-4.53z"
          />
        </svg>
      )
    default:
      return null
  }
}
