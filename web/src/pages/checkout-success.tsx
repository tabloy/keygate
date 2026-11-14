import { CheckCircle, Loader2 } from "lucide-react"
import { useEffect, useState } from "react"
import { Link } from "react-router-dom"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import { useSiteConfig } from "@/hooks/use-site-config"
import { useI18n } from "@/i18n"
import { checkout, errorCode } from "@/lib/api"

export default function CheckoutSuccessPage() {
  const { t } = useI18n()
  const { site_name, logo_url, attribution_text, attribution_url } = useSiteConfig()
  const [status, setStatus] = useState<"loading" | "ok" | "pending" | "not_applied" | "error">("loading")
  const [email, setEmail] = useState("")
  const [kind, setKind] = useState("")

  useEffect(() => {
    const params = new URLSearchParams(window.location.search)
    const sessionId = params.get("session_id")
    if (!sessionId) {
      setStatus("error")
      return
    }

    checkout
      .verify(sessionId)
      .then((r) => {
        // Paid but not delivered yet: the sync finishes it, so say so
        // rather than spin.
        setStatus(r.status === "ok" || r.status === "not_applied" ? r.status : "pending")
        if (r.email) setEmail(r.email)
        setKind(r.kind ?? "")
      })
      .catch((e) => {
        // Paid, and the delivery that failed is retried (FULFILLMENT_RETRY):
        // the same "processing" answer as a pending one, not a failure.
        setStatus(errorCode(e) === "FULFILLMENT_RETRY" ? "pending" : "error")
      })
  }, [])

  return (
    <div className="flex flex-col items-center justify-center min-h-screen bg-muted/30">
      <Card className="w-full max-w-md text-center">
        <CardHeader>
          <div className="flex justify-center mb-2">
            <img src={logo_url || "/logo.svg"} alt={site_name} className="h-12 w-12" />
          </div>
          <CardTitle className="text-2xl">{site_name}</CardTitle>
        </CardHeader>
        <CardContent className="space-y-4">
          {status === "loading" && (
            <div className="flex flex-col items-center gap-3 py-4">
              <Loader2 className="h-8 w-8 animate-spin text-muted-foreground" />
              <p className="text-muted-foreground">{t("checkout.verifying")}</p>
            </div>
          )}
          {status === "ok" && (
            <div className="flex flex-col items-center gap-3 py-4">
              <CheckCircle className="h-12 w-12 text-green-500" />
              <h3 className="text-xl font-semibold">{t("checkout.success")}</h3>
              {kind === "renewal" || kind === "upgrade" ? (
                <>
                  <p className="text-muted-foreground">
                    {kind === "renewal" ? t("checkout.renewalSuccess") : t("checkout.upgradeSuccess")}
                  </p>
                  <Button className="mt-4" asChild>
                    <Link to="/portal">{t("checkout.backToPortal")}</Link>
                  </Button>
                </>
              ) : (
                <>
                  <p className="text-muted-foreground">
                    {email ? t("checkout.licenseSentTo", { email }) : t("checkout.licenseCreated")}
                  </p>
                  <Button className="mt-4" asChild>
                    <Link to="/login">{t("checkout.viewLicense")}</Link>
                  </Button>
                </>
              )}
            </div>
          )}
          {(status === "pending" || status === "not_applied") && (
            <div className="flex flex-col items-center gap-3 py-4">
              <p className="text-muted-foreground">
                {status === "pending" ? t("checkout.processing") : t("checkout.upgradeNotApplied")}
              </p>
              <Button className="mt-4" asChild>
                <Link to={kind === "renewal" || kind === "upgrade" ? "/portal" : "/login"}>
                  {t("checkout.backToPortal")}
                </Link>
              </Button>
            </div>
          )}
          {status === "error" && (
            <div className="flex flex-col items-center gap-3 py-4">
              <p className="text-muted-foreground">{t("checkout.error")}</p>
            </div>
          )}
        </CardContent>
      </Card>
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
