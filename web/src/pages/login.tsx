import { Mail, Terminal } from "lucide-react"
import { useEffect, useRef, useState } from "react"
import { Navigate, useSearchParams } from "react-router-dom"
import { ServiceUnavailableScreen } from "@/components/service-unavailable"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Separator } from "@/components/ui/separator"
import { useAuth } from "@/hooks/use-auth"
import { useSiteConfig } from "@/hooks/use-site-config"
import { useI18n } from "@/i18n"
import { auth, setup } from "@/lib/api"

export default function LoginPage() {
  const { t } = useI18n()
  const { site_name, logo_url, attribution_text, attribution_url } = useSiteConfig()
  const { user, loading, unavailable, refetch } = useAuth()
  const [params] = useSearchParams()
  const justSetUp = params.get("setup") === "done"
  const [needsSetup, setNeedsSetup] = useState(false)
  const [devLogin, setDevLogin] = useState(false)
  const [devEmail, setDevEmail] = useState("admin@keygate.dev")
  const [devName, setDevName] = useState("Admin")
  const [devLoading, setDevLoading] = useState(false)
  const [devError, setDevError] = useState("")

  // OTP state
  const [otpStep, setOtpStep] = useState<"email" | "code">("email")
  const [otpEmail, setOtpEmail] = useState(params.get("email") ?? "")
  const [otpCode, setOtpCode] = useState("")
  const otpPending = useRef(false)
  const [otpLoading, setOtpLoading] = useState(false)
  const [otpError, setOtpError] = useState("")
  const [otpCooldown, setOtpCooldown] = useState(0)

  // A fresh install has no admins or products yet: send the first visitor to setup.
  useEffect(() => {
    setup
      .status()
      .then((r) => setNeedsSetup(r.needed))
      .catch(() => {})
  }, [])

  useEffect(() => {
    auth
      .providers()
      .then((r) => {
        setDevLogin(r.dev_login)
      })
      .catch(() => {})
  }, [])

  // Cooldown timer for resend
  useEffect(() => {
    if (otpCooldown <= 0) return
    const timer = setTimeout(() => setOtpCooldown(otpCooldown - 1), 1000)
    return () => clearTimeout(timer)
  }, [otpCooldown])

  const handleOtpSend = async (e: React.FormEvent) => {
    e.preventDefault()
    if (otpPending.current) return
    otpPending.current = true
    setOtpLoading(true)
    setOtpError("")
    try {
      await auth.otpSend(otpEmail)
      setOtpStep("code")
      setOtpCooldown(60)
    } catch (err) {
      setOtpError(err instanceof Error ? err.message : t("login.failed"))
    } finally {
      otpPending.current = false
      setOtpLoading(false)
    }
  }

  const handleOtpResend = async () => {
    if (otpCooldown > 0 || otpPending.current) return
    otpPending.current = true
    setOtpLoading(true)
    setOtpError("")
    try {
      await auth.otpSend(otpEmail)
      setOtpCooldown(60)
    } catch (err) {
      setOtpError(err instanceof Error ? err.message : t("login.failed"))
    } finally {
      otpPending.current = false
      setOtpLoading(false)
    }
  }

  const handleOtpVerify = async (e: React.FormEvent) => {
    e.preventDefault()
    if (otpPending.current) return
    otpPending.current = true
    setOtpLoading(true)
    setOtpError("")
    try {
      await auth.otpVerify(otpEmail, otpCode)
      await refetch()
    } catch (err) {
      setOtpError(err instanceof Error ? err.message : t("login.failed"))
    } finally {
      otpPending.current = false
      setOtpLoading(false)
    }
  }

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
  if (needsSetup) return <Navigate to="/setup" replace />
  if (user) return <Navigate to={user.is_admin ? "/admin" : "/portal"} replace />
  // A signed-in user whose session cannot be confirmed right now would
  // otherwise be shown a login form they do not need.
  if (unavailable) return <ServiceUnavailableScreen onRetry={refetch} />

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
          {justSetUp && otpStep === "email" && (
            <p className="rounded-md bg-emerald-50 px-3 py-2 text-sm text-emerald-800">{t("login.setupDone")}</p>
          )}
          {/* OTP Email Step */}
          {otpStep === "email" && (
            <form onSubmit={handleOtpSend} className="space-y-3">
              <div className="space-y-2">
                <Label>{t("common.email")}</Label>
                <Input
                  type="email"
                  placeholder="you@example.com"
                  value={otpEmail}
                  onChange={(e) => setOtpEmail(e.target.value)}
                  required
                  autoFocus
                />
              </div>
              {otpError && <p className="text-sm text-destructive">{otpError}</p>}
              <Button type="submit" className="w-full" disabled={otpLoading}>
                <Mail className="h-4 w-4 mr-2" />
                {otpLoading ? t("login.sendingCode") : t("login.sendCode")}
              </Button>
            </form>
          )}

          {/* OTP Code Step */}
          {otpStep === "code" && (
            <form onSubmit={handleOtpVerify} className="space-y-3">
              <p className="text-sm text-muted-foreground text-center">{t("login.codeSentTo", { email: otpEmail })}</p>
              <div className="space-y-2">
                <Input
                  type="text"
                  inputMode="numeric"
                  pattern="[0-9]*"
                  maxLength={6}
                  placeholder="000000"
                  value={otpCode}
                  onChange={(e) => setOtpCode(e.target.value.replace(/\D/g, ""))}
                  required
                  autoFocus
                  className="text-center text-2xl tracking-[0.5em] font-mono"
                />
              </div>
              {otpError && <p className="text-sm text-destructive">{otpError}</p>}
              <Button type="submit" className="w-full" disabled={otpLoading || otpCode.length !== 6}>
                {otpLoading ? t("login.verifying") : t("login.verify")}
              </Button>
              <div className="flex justify-between text-xs">
                <button
                  type="button"
                  className="text-muted-foreground hover:text-foreground"
                  disabled={otpLoading}
                  onClick={() => {
                    if (otpPending.current) return
                    setOtpStep("email")
                    setOtpCode("")
                    setOtpError("")
                  }}
                >
                  {t("login.changeEmail")}
                </button>
                <button
                  type="button"
                  className="text-muted-foreground hover:text-foreground disabled:opacity-50"
                  disabled={otpCooldown > 0 || otpLoading}
                  onClick={handleOtpResend}
                >
                  {otpCooldown > 0 ? t("login.resendIn", { seconds: String(otpCooldown) }) : t("login.resendCode")}
                </button>
              </div>
            </form>
          )}

          {/* Dev Login (development only) */}
          {devLogin && otpStep === "email" && (
            <>
              <div className="relative my-4">
                <Separator />
                <span className="absolute left-1/2 top-1/2 -translate-x-1/2 -translate-y-1/2 bg-card px-2 text-xs text-muted-foreground">
                  DEV MODE
                </span>
              </div>
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
