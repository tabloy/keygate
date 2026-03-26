import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { AlertCircle, Check, Copy, Key, Plus, Trash2, Users } from "lucide-react"
import { useState } from "react"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select"
import { Separator } from "@/components/ui/separator"
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { useAuth } from "@/hooks/use-auth"
import { useI18n } from "@/i18n"
import { type Entitlement, type License, portal } from "@/lib/api"
import { cn, formatDate, statusColor } from "@/lib/utils"

export default function PortalLicensesPage() {
  const { t } = useI18n()
  const { data, isLoading } = useQuery({
    queryKey: ["portal", "licenses"],
    queryFn: portal.licenses,
  })

  const licenses = data?.licenses || []

  if (isLoading) {
    return (
      <div className="space-y-4">
        {[1, 2].map((i) => (
          <div key={i} className="h-48 animate-pulse bg-muted rounded-lg" />
        ))}
      </div>
    )
  }

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-bold tracking-tight">{t("portal.myLicenses")}</h1>
        <p className="text-muted-foreground">{t("portal.myLicensesDesc")}</p>
      </div>

      {licenses.length === 0 ? (
        <Card>
          <CardContent className="py-12 text-center">
            <Key className="h-12 w-12 mx-auto text-muted-foreground mb-4" />
            <p className="text-lg font-medium">{t("portal.noLicenses")}</p>
            <p className="text-muted-foreground mt-1">{t("portal.noLicensesDesc")}</p>
          </CardContent>
        </Card>
      ) : (
        <div className="space-y-6">
          {licenses.map((lic) => (
            <LicenseCard key={lic.id} license={lic} />
          ))}
        </div>
      )}
    </div>
  )
}

function LicenseCard({ license: lic }: { license: License }) {
  const { t } = useI18n()
  const [copied, setCopied] = useState(false)
  const [showInvoices, setShowInvoices] = useState(false)
  const [showChangePlan, setShowChangePlan] = useState(false)
  const [showCancel, setShowCancel] = useState(false)
  const productType = lic.product?.type || "perpetual"
  const showUsageAndSeats = productType === "saas" || productType === "hybrid"

  const copyKey = () => {
    navigator.clipboard.writeText(lic.license_key)
    setCopied(true)
    setTimeout(() => setCopied(false), 2000)
  }

  const handleBillingPortal = async () => {
    try {
      const res = await portal.getBillingPortal({ license_id: lic.id })
      window.location.href = res.url
    } catch {
      // ignore
    }
  }

  const defaultTab = showUsageAndSeats ? "overview" : "overview"

  return (
    <Card>
      <CardHeader>
        <div className="flex items-center justify-between">
          <div>
            <CardTitle className="text-lg">{lic.product?.name || "License"}</CardTitle>
            <p className="text-sm text-muted-foreground mt-1">{lic.plan?.name}</p>
          </div>
          <Badge className={statusColor(lic.status)}>{t(`status.${lic.status}` as any)}</Badge>
        </div>
      </CardHeader>
      <CardContent className="space-y-4">
        {/* License key */}
        <div className="flex items-center gap-2 bg-muted rounded-lg px-3 py-2">
          <Key className="h-4 w-4 text-muted-foreground shrink-0" />
          <code className="text-sm flex-1 truncate">{lic.license_key}</code>
          <Button variant="ghost" size="icon" className="h-7 w-7 shrink-0" onClick={copyKey}>
            {copied ? <Check className="h-3 w-3 text-emerald-600" /> : <Copy className="h-3 w-3" />}
          </Button>
        </div>

        {/* Overview stats */}
        <div className="grid grid-cols-2 md:grid-cols-4 gap-4 text-sm">
          <div>
            <p className="text-muted-foreground">{t("portal.validFrom")}</p>
            <p className="font-medium">{formatDate(lic.valid_from)}</p>
          </div>
          <div>
            <p className="text-muted-foreground">{t("licenses.validUntil")}</p>
            <p className="font-medium">{formatDate(lic.valid_until)}</p>
          </div>
          <div>
            <p className="text-muted-foreground">{t("licenses.activations")}</p>
            <p className="font-medium">
              {lic.activations?.length || 0} / {lic.plan?.max_activations || "-"}
            </p>
          </div>
          {showUsageAndSeats && (
            <div>
              <p className="text-muted-foreground">{t("portal.teamMembers")}</p>
              <p className="font-medium">
                {lic.seats?.filter((s) => !s.removed_at).length || 0} / {lic.plan?.max_seats || "-"}
              </p>
            </div>
          )}
        </div>

        {/* Subscription Actions */}
        {(lic.payment_provider === "stripe" || lic.payment_provider === "paypal") && (
          <div className="flex gap-2 flex-wrap">
            {lic.payment_provider === "stripe" && (
              <>
                <Button variant="outline" size="sm" onClick={() => setShowInvoices(true)}>
                  {t("portal.viewInvoices")}
                </Button>
                <Button variant="outline" size="sm" onClick={handleBillingPortal}>
                  {t("portal.updatePayment")}
                </Button>
              </>
            )}
            {(lic.status === "active" || lic.status === "trialing") && (
              <>
                {lic.payment_provider === "stripe" && (
                  <Button variant="outline" size="sm" onClick={() => setShowChangePlan(true)}>
                    {t("portal.changePlan")}
                  </Button>
                )}
                <Button variant="outline" size="sm" className="text-destructive" onClick={() => setShowCancel(true)}>
                  {t("portal.cancelSubscription")}
                </Button>
              </>
            )}
          </div>
        )}

        <Separator />

        {/* Tabs for detailed sections */}
        <Tabs defaultValue={defaultTab}>
          <TabsList>
            <TabsTrigger value="overview">{t("licenses.activations")}</TabsTrigger>
            <TabsTrigger value="entitlements">{t("plans.entitlements")}</TabsTrigger>
            {showUsageAndSeats && <TabsTrigger value="usage">{t("analytics.usage")}</TabsTrigger>}
            {showUsageAndSeats && <TabsTrigger value="seats">{t("licenses.seats")}</TabsTrigger>}
          </TabsList>

          <TabsContent value="overview" className="mt-4">
            <ActivationsSection activations={lic.activations || []} />
          </TabsContent>

          <TabsContent value="entitlements" className="mt-4">
            <EntitlementsSection entitlements={lic.plan?.entitlements || []} />
          </TabsContent>

          {showUsageAndSeats && (
            <TabsContent value="usage" className="mt-4">
              <QuotaUsageSection license={lic} />
            </TabsContent>
          )}

          {showUsageAndSeats && (
            <TabsContent value="seats" className="mt-4">
              <SeatsSection license={lic} />
            </TabsContent>
          )}
        </Tabs>

        {showCancel && (
          <CancelDialog
            licenseId={lic.id}
            provider={lic.payment_provider || ""}
            productName={lic.product?.name || ""}
            onClose={() => setShowCancel(false)}
          />
        )}

        {showInvoices && <InvoicesDialog licenseId={lic.id} onClose={() => setShowInvoices(false)} />}

        {showChangePlan && <ChangePlanDialog license={lic} onClose={() => setShowChangePlan(false)} />}
      </CardContent>
    </Card>
  )
}

function ActivationsSection({ activations }: { activations: NonNullable<License["activations"]> }) {
  const { t } = useI18n()
  if (activations.length === 0) {
    return <p className="text-sm text-muted-foreground py-4 text-center">{t("portal.noDevices")}</p>
  }

  return (
    <div className="space-y-2">
      <p className="text-sm font-medium mb-2">{t("portal.activeDevices")}</p>
      {activations.map((act) => (
        <div key={act.id} className="flex items-center justify-between bg-muted/50 rounded px-3 py-2 text-sm">
          <div>
            <code className="text-xs">{act.identifier}</code>
            {act.label && <span className="text-muted-foreground ml-2">({act.label})</span>}
            <span className="text-xs text-muted-foreground ml-2 capitalize">{act.identifier_type}</span>
          </div>
          <span className="text-xs text-muted-foreground">
            {t("portal.lastVerified")} {formatDate(act.last_verified)}
          </span>
        </div>
      ))}
    </div>
  )
}

function EntitlementsSection({ entitlements }: { entitlements: Entitlement[] }) {
  const { t } = useI18n()
  if (entitlements.length === 0) {
    return <p className="text-sm text-muted-foreground py-4 text-center">{t("portal.noEntitlements")}</p>
  }

  return (
    <div className="space-y-2">
      <p className="text-sm font-medium mb-2">{t("portal.featuresIncluded")}</p>
      {entitlements.map((ent) => (
        <div key={ent.id} className="flex items-center justify-between bg-muted/50 rounded px-3 py-2 text-sm">
          <div className="flex items-center gap-2">
            <Check className="h-4 w-4 text-emerald-600 shrink-0" />
            <span className="font-medium">{ent.feature}</span>
          </div>
          <div className="text-muted-foreground text-xs">
            {ent.value_type === "boolean" ? (
              <Badge variant="secondary">{t("common.active")}</Badge>
            ) : ent.value_type === "quota" ? (
              <span>
                {formatNumber(Number(ent.value))} {ent.quota_unit || "units"} / {ent.quota_period || "period"}
              </span>
            ) : (
              <span>{ent.value}</span>
            )}
          </div>
        </div>
      ))}
    </div>
  )
}

function QuotaUsageSection({ license }: { license: License }) {
  const quotaEntitlements = (license.plan?.entitlements || []).filter((e) => e.value_type === "quota")

  if (quotaEntitlements.length === 0) {
    return <p className="text-sm text-muted-foreground py-4 text-center">No quota-based features.</p>
  }

  return (
    <div className="space-y-4">
      <p className="text-sm font-medium mb-2">Quota Usage</p>
      {quotaEntitlements.map((ent) => (
        <QuotaBar key={ent.id} entitlement={ent} licenseKey={license.license_key} />
      ))}
    </div>
  )
}

function QuotaBar({ entitlement, licenseKey }: { entitlement: Entitlement; licenseKey: string }) {
  const { data, isLoading } = useQuery({
    queryKey: ["portal", "quota", licenseKey, entitlement.feature],
    queryFn: () => portal.quotaStatus({ license_key: licenseKey, feature: entitlement.feature }),
  })

  const limit = Number(entitlement.value) || 0
  const used = data?.used ?? 0
  const percentage = limit > 0 ? Math.min((used / limit) * 100, 100) : 0
  const isWarning = percentage >= 80

  if (isLoading) {
    return <div className="h-12 animate-pulse bg-muted rounded-lg" />
  }

  return (
    <div className="space-y-1.5">
      <div className="flex items-center justify-between text-sm">
        <span className="font-medium">{entitlement.feature}</span>
        <span className={cn("text-xs", isWarning ? "text-amber-600 font-medium" : "text-muted-foreground")}>
          {formatNumber(used)} / {formatNumber(limit)} ({Math.round(percentage)}%)
        </span>
      </div>
      <div className="h-2.5 bg-muted rounded-full overflow-hidden">
        <div
          className={cn(
            "h-full rounded-full transition-all",
            isWarning ? (percentage >= 95 ? "bg-red-500" : "bg-amber-500") : "bg-emerald-500",
          )}
          style={{ width: `${percentage}%` }}
        />
      </div>
      {entitlement.quota_unit && (
        <p className="text-xs text-muted-foreground">
          {entitlement.quota_unit} per {entitlement.quota_period || "period"}
        </p>
      )}
    </div>
  )
}

function SeatsSection({ license }: { license: License }) {
  const { t } = useI18n()
  const { user } = useAuth()
  const queryClient = useQueryClient()
  const [addDialogOpen, setAddDialogOpen] = useState(false)
  const [newEmail, setNewEmail] = useState("")
  const [newRole, setNewRole] = useState("member")

  const { data: seatsData, isLoading } = useQuery({
    queryKey: ["portal", "seats", license.license_key],
    queryFn: () => portal.listSeats({ license_key: license.license_key }),
  })

  const seats = seatsData?.seats?.filter((s) => !s.removed_at) || []
  const maxSeats = license.plan?.max_seats || 0
  const canAddSeat = maxSeats === 0 || seats.length < maxSeats

  const currentSeat = seats.find((s) => s.user_id === user?.id || s.email === user?.email)
  const isOwnerOrAdmin = currentSeat?.role === "owner" || currentSeat?.role === "admin" || license.email === user?.email

  const addMutation = useMutation({
    mutationFn: (data: { email: string; role: string }) =>
      portal.addSeat({ license_key: license.license_key, email: data.email, role: data.role }),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["portal", "seats", license.license_key] })
      setAddDialogOpen(false)
      setNewEmail("")
      setNewRole("member")
    },
  })

  const removeMutation = useMutation({
    mutationFn: (seatId: string) => portal.removeSeat({ license_key: license.license_key, seat_id: seatId }),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["portal", "seats", license.license_key] })
    },
  })

  if (isLoading) {
    return <div className="h-24 animate-pulse bg-muted rounded-lg" />
  }

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between">
        <p className="text-sm font-medium">
          {t("portal.teamMembers")} ({seats.length}
          {maxSeats > 0 ? ` / ${maxSeats}` : ""})
        </p>
        {isOwnerOrAdmin && canAddSeat && (
          <Button size="sm" variant="outline" onClick={() => setAddDialogOpen(true)}>
            <Plus className="h-3.5 w-3.5 mr-1" />
            {t("portal.addMember")}
          </Button>
        )}
      </div>

      {seats.length === 0 ? (
        <p className="text-sm text-muted-foreground py-4 text-center">{t("portal.noMembers")}</p>
      ) : (
        <div className="space-y-2">
          {seats.map((seat) => (
            <div key={seat.id} className="flex items-center justify-between bg-muted/50 rounded px-3 py-2 text-sm">
              <div className="flex items-center gap-3">
                <Users className="h-4 w-4 text-muted-foreground shrink-0" />
                <div>
                  <p className="font-medium">{seat.email}</p>
                  <p className="text-xs text-muted-foreground">
                    <Badge variant="secondary" className="text-[10px] mr-1">
                      {seat.role}
                    </Badge>
                    Joined {formatDate(seat.accepted_at || seat.created_at)}
                  </p>
                </div>
              </div>
              {isOwnerOrAdmin && seat.role !== "owner" && (
                <Button
                  variant="ghost"
                  size="icon"
                  className="h-7 w-7 text-muted-foreground hover:text-destructive"
                  onClick={() => removeMutation.mutate(seat.id)}
                  disabled={removeMutation.isPending}
                >
                  <Trash2 className="h-3.5 w-3.5" />
                </Button>
              )}
            </div>
          ))}
        </div>
      )}

      {!canAddSeat && isOwnerOrAdmin && (
        <div className="flex items-center gap-2 text-xs text-amber-600 bg-amber-50 rounded px-3 py-2">
          <AlertCircle className="h-3.5 w-3.5 shrink-0" />
          {t("portal.seatLimit")}
        </div>
      )}

      <Dialog open={addDialogOpen} onOpenChange={setAddDialogOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>{t("portal.addMember")}</DialogTitle>
            <DialogDescription>{t("portal.inviteDesc")}</DialogDescription>
          </DialogHeader>
          <div className="space-y-4 py-2">
            <div className="space-y-2">
              <Label htmlFor="seat-email">{t("common.email")}</Label>
              <Input
                id="seat-email"
                type="email"
                placeholder="colleague@example.com"
                value={newEmail}
                onChange={(e) => setNewEmail(e.target.value)}
              />
            </div>
            <div className="space-y-2">
              <Label htmlFor="seat-role">{t("licenses.role")}</Label>
              <Select value={newRole} onValueChange={setNewRole}>
                <SelectTrigger>
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="member">{t("portal.roleMember")}</SelectItem>
                  <SelectItem value="admin">{t("portal.roleAdmin")}</SelectItem>
                </SelectContent>
              </Select>
            </div>
          </div>
          <div className="flex justify-end gap-2 pt-2">
            <Button variant="outline" onClick={() => setAddDialogOpen(false)}>
              {t("common.cancel")}
            </Button>
            <Button
              onClick={() => addMutation.mutate({ email: newEmail, role: newRole })}
              disabled={!newEmail || addMutation.isPending}
            >
              {addMutation.isPending ? t("common.loading") : t("portal.addMember")}
            </Button>
          </div>
        </DialogContent>
      </Dialog>
    </div>
  )
}

function formatNumber(n: number): string {
  if (n >= 1_000_000_000) return `${(n / 1_000_000_000).toFixed(1)} GB`
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)} MB`
  return n.toLocaleString()
}

function CancelDialog({
  licenseId,
  provider,
  productName,
  onClose,
}: {
  licenseId: string
  provider: string
  productName: string
  onClose: () => void
}) {
  const { t } = useI18n()
  const qc = useQueryClient()
  const [immediate, setImmediate] = useState(false)

  const cancelMut = useMutation({
    mutationFn: () => {
      if (provider === "paypal") {
        return portal.cancelPayPalSubscription({ license_id: licenseId })
      }
      return portal.cancelSubscription({ license_id: licenseId, immediate })
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["portal", "licenses"] })
      onClose()
    },
  })

  return (
    <Dialog open onOpenChange={onClose}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>{t("portal.cancelSubscription")}</DialogTitle>
          <DialogDescription>{t("portal.cancelDesc", { product: productName })}</DialogDescription>
        </DialogHeader>
        <div className="space-y-4">
          {provider === "stripe" && (
            <label className="flex items-center gap-2 text-sm">
              <input
                type="checkbox"
                checked={immediate}
                onChange={(e) => setImmediate(e.target.checked)}
                className="rounded"
              />
              {t("portal.cancelImmediately")}
            </label>
          )}
          <p className="text-sm text-muted-foreground">
            {immediate ? t("portal.cancelImmediateWarning") : t("portal.cancelEndWarning")}
          </p>
          <div className="flex justify-end gap-2">
            <Button variant="outline" onClick={onClose}>
              {t("common.cancel")}
            </Button>
            <Button variant="destructive" onClick={() => cancelMut.mutate()} disabled={cancelMut.isPending}>
              {cancelMut.isPending ? t("common.loading") : t("portal.confirmCancel")}
            </Button>
          </div>
        </div>
      </DialogContent>
    </Dialog>
  )
}

function InvoicesDialog({ licenseId, onClose }: { licenseId: string; onClose: () => void }) {
  const { t } = useI18n()
  const { data, isLoading } = useQuery({
    queryKey: ["portal", "invoices", licenseId],
    queryFn: () => portal.getInvoices(licenseId),
  })
  const invoices = data?.invoices || []

  return (
    <Dialog open onOpenChange={onClose}>
      <DialogContent className="max-w-2xl max-h-[85vh] overflow-y-auto">
        <DialogHeader>
          <DialogTitle>{t("portal.invoices")}</DialogTitle>
        </DialogHeader>
        {isLoading ? (
          <div className="h-32 animate-pulse bg-muted rounded-lg" />
        ) : invoices.length === 0 ? (
          <p className="text-sm text-muted-foreground text-center py-8">{t("common.noData")}</p>
        ) : (
          <div className="space-y-2">
            {invoices.map((inv: any) => (
              <div key={inv.id} className="flex items-center justify-between bg-muted/50 rounded-lg px-4 py-3 text-sm">
                <div>
                  <p className="font-medium">{inv.number || inv.id}</p>
                  <p className="text-xs text-muted-foreground">{new Date(inv.created * 1000).toLocaleDateString()}</p>
                </div>
                <div className="flex items-center gap-3">
                  <Badge
                    className={
                      inv.status === "paid" ? "bg-emerald-100 text-emerald-800" : "bg-amber-100 text-amber-800"
                    }
                  >
                    {inv.status}
                  </Badge>
                  <span className="font-medium">
                    {(inv.amount_paid / 100).toFixed(2)} {inv.currency?.toUpperCase()}
                  </span>
                  {inv.invoice_pdf && (
                    <Button variant="ghost" size="sm" asChild>
                      <a href={inv.invoice_pdf} target="_blank" rel="noopener noreferrer">
                        PDF
                      </a>
                    </Button>
                  )}
                </div>
              </div>
            ))}
          </div>
        )}
      </DialogContent>
    </Dialog>
  )
}

function ChangePlanDialog({ license, onClose }: { license: License; onClose: () => void }) {
  const { t } = useI18n()
  const qc = useQueryClient()

  // Fetch available plans via portal API (not admin)
  const { data: plansData } = useQuery({
    queryKey: ["portal", "plans", license.product_id],
    queryFn: () => portal.listPlans(license.product_id),
  })
  // Filter: only plans with Stripe price, exclude current (PayPal plan changes not supported via self-service)
  const isStripe = license.payment_provider === "stripe"
  const plans = (plansData?.plans || []).filter((p: any) => p.id !== license.plan_id && isStripe && p.stripe_price_id)

  const changeMut = useMutation({
    mutationFn: (newPriceId: string) => portal.changePlan({ license_id: license.id, new_price_id: newPriceId }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["portal", "licenses"] })
      onClose()
    },
  })

  return (
    <Dialog open onOpenChange={onClose}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>{t("portal.changePlan")}</DialogTitle>
          <DialogDescription>{t("portal.changePlanDesc")}</DialogDescription>
        </DialogHeader>
        {plans.length === 0 ? (
          <p className="text-sm text-muted-foreground text-center py-4">{t("portal.noOtherPlans")}</p>
        ) : (
          <div className="space-y-2">
            {plans.map((plan: any) => (
              <div key={plan.id} className="flex items-center justify-between bg-muted/50 rounded-lg px-4 py-3">
                <div>
                  <p className="font-medium text-sm">{plan.name}</p>
                  <p className="text-xs text-muted-foreground">
                    {plan.license_type} · {plan.billing_interval || t("plans.perpetual")}
                  </p>
                </div>
                <Button
                  size="sm"
                  onClick={() => changeMut.mutate(plan.stripe_price_id || plan.paypal_plan_id)}
                  disabled={changeMut.isPending}
                >
                  {changeMut.isPending ? t("common.loading") : t("portal.switchTo")}
                </Button>
              </div>
            ))}
          </div>
        )}
      </DialogContent>
    </Dialog>
  )
}
