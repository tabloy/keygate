import { useQuery } from "@tanstack/react-query"
import { useState } from "react"
import { Navigate } from "react-router-dom"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { useSiteConfig } from "@/hooks/use-site-config"
import { useI18n } from "@/i18n"
import { errorCode, setup } from "@/lib/api"

type ProductType = "desktop" | "saas" | "hybrid"

function slugify(s: string) {
  return s
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "")
}

// First run: creates the owner account, the site name and the first
// product with a starter plan, through POST /setup/initialize. The
// server refuses the call once an owner, an admin or a product exists,
// so the page is only reachable while setup is still needed.
export default function SetupPage() {
  const { t } = useI18n()
  const { logo_url, attribution_text, attribution_url } = useSiteConfig()
  const status = useQuery({ queryKey: ["setup-status"], queryFn: setup.status, retry: false })

  const [adminName, setAdminName] = useState("")
  const [adminEmail, setAdminEmail] = useState("")
  const [siteName, setSiteName] = useState("")
  const [productName, setProductName] = useState("")
  const [productSlug, setProductSlug] = useState("")
  const [slugTouched, setSlugTouched] = useState(false)
  const [productType, setProductType] = useState<ProductType>("desktop")
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState("")

  if (status.isLoading) return null
  if (status.data && !status.data.needed) return <Navigate to="/login" replace />

  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    setSaving(true)
    setError("")
    try {
      await setup.initialize({
        admin_name: adminName,
        admin_email: adminEmail,
        site_name: siteName,
        product_name: productName,
        product_slug: productSlug,
        product_type: productType,
      })
      // A full load rather than a client side route change, so the site
      // configuration (name, branding) is fetched again with the new values.
      window.location.replace(`/login?email=${encodeURIComponent(adminEmail.trim().toLowerCase())}&setup=done`)
    } catch (err) {
      if (errorCode(err) === "SETUP_EMAIL_NOT_ALLOWED") setError(t("setup.emailNotAllowed"))
      else setError(err instanceof Error ? err.message : t("setup.failed"))
    } finally {
      setSaving(false)
    }
  }

  const types: ProductType[] = ["desktop", "saas", "hybrid"]

  return (
    <div className="flex flex-col items-center justify-center min-h-screen bg-muted/30 py-10">
      <Card className="w-full max-w-md">
        <CardHeader className="text-center">
          <div className="flex justify-center mb-2">
            <img src={logo_url || "/logo.svg"} alt="" className="h-12 w-12" />
          </div>
          <CardTitle className="text-2xl">{t("setup.title")}</CardTitle>
          <CardDescription>{t("setup.subtitle")}</CardDescription>
        </CardHeader>
        <CardContent>
          {status.isError ? (
            <div className="space-y-3 text-center">
              <p className="text-sm text-destructive">{t("setup.statusFailed")}</p>
              <Button variant="outline" disabled={status.isFetching} onClick={() => status.refetch()}>
                {t("common.retry")}
              </Button>
            </div>
          ) : (
            <form onSubmit={submit} className="space-y-5">
              <fieldset className="space-y-3">
                <legend className="text-sm font-medium">{t("setup.you")}</legend>
                <div className="space-y-1.5">
                  <Label htmlFor="admin-name">{t("common.name")}</Label>
                  <Input id="admin-name" value={adminName} onChange={(e) => setAdminName(e.target.value)} required />
                </div>
                <div className="space-y-1.5">
                  <Label htmlFor="admin-email">{t("common.email")}</Label>
                  <Input
                    id="admin-email"
                    type="email"
                    value={adminEmail}
                    onChange={(e) => setAdminEmail(e.target.value)}
                    required
                  />
                  <p className="text-xs text-muted-foreground">{t("setup.emailHint")}</p>
                </div>
                <div className="space-y-1.5">
                  <Label htmlFor="site-name">{t("setup.siteName")}</Label>
                  <Input
                    id="site-name"
                    value={siteName}
                    onChange={(e) => setSiteName(e.target.value)}
                    placeholder={t("setup.siteNamePlaceholder")}
                    required
                  />
                </div>
              </fieldset>

              <fieldset className="space-y-3">
                <legend className="text-sm font-medium">{t("setup.firstProduct")}</legend>
                <div className="space-y-1.5">
                  <Label htmlFor="product-name">{t("common.name")}</Label>
                  <Input
                    id="product-name"
                    value={productName}
                    onChange={(e) => {
                      setProductName(e.target.value)
                      if (!slugTouched) setProductSlug(slugify(e.target.value))
                    }}
                    required
                  />
                </div>
                <div className="space-y-1.5">
                  <Label htmlFor="product-slug">{t("setup.productSlug")}</Label>
                  <Input
                    id="product-slug"
                    value={productSlug}
                    onChange={(e) => {
                      setSlugTouched(true)
                      setProductSlug(e.target.value)
                    }}
                    required
                  />
                  <p className="text-xs text-muted-foreground">{t("setup.slugHint")}</p>
                </div>
                <div className="space-y-2" role="radiogroup" aria-label={t("setup.productType")}>
                  <Label>{t("setup.productType")}</Label>
                  {types.map((type) => (
                    <label
                      key={type}
                      className={`flex cursor-pointer gap-3 rounded-md border p-3 text-sm ${
                        productType === type ? "border-primary bg-primary/5" : ""
                      }`}
                    >
                      <input
                        type="radio"
                        name="product-type"
                        value={type}
                        checked={productType === type}
                        onChange={() => setProductType(type)}
                        className="mt-0.5"
                      />
                      <span>
                        <span className="font-medium">{t(`products.${type}`)}</span>
                        <span className="block text-xs text-muted-foreground">{t(`products.${type}Desc`)}</span>
                      </span>
                    </label>
                  ))}
                </div>
              </fieldset>

              {error && <p className="text-sm text-destructive">{error}</p>}
              <Button type="submit" className="w-full" disabled={saving}>
                {saving ? t("setup.creating") : t("setup.create")}
              </Button>
              <p className="text-xs text-muted-foreground text-center">{t("setup.after")}</p>
            </form>
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
