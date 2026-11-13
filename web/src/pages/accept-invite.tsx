import { useMutation } from "@tanstack/react-query"
import { CheckCircle2, XCircle } from "lucide-react"
import { useEffect, useRef } from "react"
import { Link, useSearchParams } from "react-router-dom"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card"
import { useAuth } from "@/hooks/use-auth"
import { useSiteConfig } from "@/hooks/use-site-config"
import { useI18n } from "@/i18n"
import { errorCode, invites, ServiceUnavailableError } from "@/lib/api"

export default function AcceptInvitePage() {
  const { t } = useI18n()
  const { refetch } = useAuth()
  const [params] = useSearchParams()
  const token = params.get("token") || ""

  // Backend collapses bad/expired/already-accepted into one error, so
  // the page tells apart only "no seat is free", whose invitation still
  // stands. It shows the failure itself; the global toast stays out.
  // AcceptInvite also issues a session cookie (see AuthHandler.AcceptInvite);
  // refetch so "Open portal" sees the invitee, same as signup/login.
  const acceptMut = useMutation({
    mutationFn: () => invites.accept(token),
    meta: { ownErrors: true },
    onSuccess: async () => {
      await refetch()
    },
  })

  // Fire exactly once per distinct token. StrictMode double-invokes
  // effects in dev; storing the LAST-fired token (not just a boolean)
  // means: same token in the URL → already fired → no-op. A pasted
  // new token → ref doesn't match → fire again. The ref-keyed-on-
  // token form also satisfies the exhaustive-deps lint cleanly:
  // mutate is referentially stable across renders for react-query,
  // and token is the only thing actually driving re-invocation.
  const fired = useRef<string | null>(null)
  const mutate = acceptMut.mutate
  useEffect(() => {
    if (!token || fired.current === token) return
    fired.current = token
    mutate()
  }, [token, mutate])

  if (!token) {
    return (
      <Shell>
        <Card className="max-w-md">
          <CardHeader>
            <CardTitle className="flex items-center gap-2">
              <XCircle className="h-5 w-5 text-destructive" />
              {t("acceptInvite.missingToken")}
            </CardTitle>
            <CardDescription>{t("acceptInvite.missingTokenDesc")}</CardDescription>
          </CardHeader>
          <CardContent>
            <Button asChild variant="outline">
              <Link to="/">{t("acceptInvite.goHome")}</Link>
            </Button>
          </CardContent>
        </Card>
      </Shell>
    )
  }

  if (acceptMut.isPending || acceptMut.isIdle) {
    return (
      <Shell>
        <p className="text-sm text-muted-foreground">{t("acceptInvite.processing")}</p>
      </Shell>
    )
  }

  if (acceptMut.isError) {
    // Every seat taken: the invitation itself still stands.
    const noSeat = errorCode(acceptMut.error) === "SEAT_LIMIT"
    const unavailable = acceptMut.error instanceof ServiceUnavailableError
    return (
      <Shell>
        <Card className="max-w-md">
          <CardHeader>
            <CardTitle className="flex items-center gap-2">
              <XCircle className="h-5 w-5 text-destructive" />
              {t(
                unavailable
                  ? "acceptInvite.unavailableTitle"
                  : noSeat
                    ? "acceptInvite.noSeatTitle"
                    : "acceptInvite.invalidTitle",
              )}
            </CardTitle>
            <CardDescription>
              {t(
                unavailable
                  ? "acceptInvite.unavailableDesc"
                  : noSeat
                    ? "acceptInvite.noSeatDesc"
                    : "acceptInvite.invalidDesc",
              )}
            </CardDescription>
          </CardHeader>
          <CardContent className="flex flex-wrap gap-2">
            {(unavailable || noSeat) && <Button onClick={() => acceptMut.mutate()}>{t("common.retry")}</Button>}
            {unavailable && (
              <Button asChild variant="outline">
                <Link to="/login">{t("acceptInvite.signIn")}</Link>
              </Button>
            )}
            <Button asChild variant="outline">
              <Link to="/">{t("acceptInvite.goHome")}</Link>
            </Button>
          </CardContent>
        </Card>
      </Shell>
    )
  }

  const data = acceptMut.data!

  return (
    <Shell>
      <Card className="max-w-md">
        <CardHeader>
          <CardTitle className="flex items-center gap-2">
            <CheckCircle2 className="h-5 w-5 text-emerald-600" />
            {data.product_name
              ? t("acceptInvite.acceptedTitle").replace("{product}", data.product_name)
              : t("acceptInvite.acceptedTitleNoProduct")}
          </CardTitle>
          <CardDescription>{t("acceptInvite.acceptedDesc")}</CardDescription>
        </CardHeader>
        <CardContent>
          <Button asChild>
            <Link to="/portal">{t("acceptInvite.openPortal")}</Link>
          </Button>
        </CardContent>
      </Card>
    </Shell>
  )
}

function Shell({ children }: { children: React.ReactNode }) {
  const { attribution_text, attribution_url } = useSiteConfig()
  return (
    <div className="min-h-screen flex flex-col items-center justify-center p-4">
      {children}
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
