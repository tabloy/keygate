import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { Check, Copy, Key, Trash2 } from "lucide-react"
import { useState } from "react"
import { showToast } from "@/components/toast"
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import { Dialog, DialogBody, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select"
import { Separator } from "@/components/ui/separator"
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { useAuth } from "@/hooks/use-auth"
import { type TranslationKeys, useI18n } from "@/i18n"
import {
  type Activation,
  type Entitlement,
  type Invoice,
  type Plan,
  type PortalLicense,
  portal,
  type Seat,
} from "@/lib/api"
import { cn, formatDate, statusColor } from "@/lib/utils"

export default function PortalLicensesPage() {
  const { t } = useI18n()
  const { data, isLoading } = useQuery({
    queryKey: ["portal", "licenses"],
    queryFn: portal.licenses,
  })

  const licenses = data?.licenses || []
  // Renewals can be switched off server-side (during a rollout, or
  // when the merchant stops selling them); don't offer what the
  // endpoint would refuse. Only an explicit true counts: a replica on
  // a version without the field has no renewal endpoint either.
  const renewalsEnabled = data?.renewals_enabled === true

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
            <LicenseCard key={lic.id} license={lic} renewalsEnabled={renewalsEnabled} />
          ))}
        </div>
      )}
    </div>
  )
}

function LicenseCard({ license: lic, renewalsEnabled }: { license: PortalLicense; renewalsEnabled: boolean }) {
  const { t } = useI18n()
  const { user } = useAuth()
  // Seat members see the license too, but renewals, plan changes,
  // cancellation and payment methods belong to the license holder;
  // the server authorises those by the license's own address.
  const isOwner = !!user?.email && user.email.toLowerCase() === lic.email.toLowerCase()
  const [copied, setCopied] = useState(false)
  const [showInvoices, setShowInvoices] = useState(false)
  const [showChangePlan, setShowChangePlan] = useState(false)
  const [showCancel, setShowCancel] = useState(false)
  const productType = lic.product?.type || "perpetual"
  const showUsage = productType === "saas" || productType === "hybrid"
  // Seats UI shows for any plan that *could* have more than one seat.
  // Backend treats max_seats=0 as "no limit", so the only case we hide
  // is max_seats=1 (true single-seat). Perpetual products don't have
  // a portal-managed team, so gate on product type too.
  const seatCap = lic.plan?.max_seats ?? 0
  const showSeats = (productType === "saas" || productType === "hybrid") && seatCap !== 1

  const copyKey = () => {
    navigator.clipboard.writeText(lic.license_key)
    setCopied(true)
    setTimeout(() => setCopied(false), 2000)
  }

  // Change plan, cancel and payment-method updates all act on a Stripe
  // subscription. A one-time purchase has none: the backend answers 400
  // to each, so don't offer them.
  const hasSubscription = lic.payment_provider === "stripe" && !!lic.stripe_subscription_id

  // Maintenance period of a perpetual license. The license never
  // expires; updates_until only bounds which releases may be installed.
  const isPerpetualPlan = lic.plan?.license_type === "perpetual"
  const updatesUntil = lic.updates_until ? new Date(lic.updates_until) : null
  const updatesEnded = updatesUntil !== null && updatesUntil.getTime() < Date.now()
  const updatesEndingSoon =
    updatesUntil !== null && !updatesEnded && updatesUntil.getTime() - Date.now() < 30 * 24 * 60 * 60 * 1000
  const canRenewUpdates =
    renewalsEnabled &&
    isOwner &&
    isPerpetualPlan &&
    updatesUntil !== null &&
    lic.status === "active" &&
    lic.plan?.active !== false &&
    (lic.plan?.renewal_days ?? 0) > 0 &&
    !!lic.plan?.stripe_renewal_price_id
  const [renewing, setRenewing] = useState(false)
  const handleRenewUpdates = async () => {
    setRenewing(true)
    try {
      const res = await portal.renewUpdates({ license_id: lic.id })
      window.location.href = res.url
    } catch (err) {
      showToast(err instanceof Error ? err.message : "An error occurred")
      setRenewing(false)
    }
  }

  const handleBillingPortal = async () => {
    try {
      const res = await portal.getBillingPortal({ license_id: lic.id })
      window.location.href = res.url
    } catch (err) {
      showToast(err instanceof Error ? err.message : "An error occurred")
    }
  }

  const defaultTab = "overview"

  return (
    <Card>
      <CardHeader>
        <div className="flex items-center justify-between">
          <div>
            <CardTitle className="text-lg">{lic.product?.name || "License"}</CardTitle>
            <p className="text-sm text-muted-foreground mt-1">{lic.plan?.name}</p>
          </div>
          <Badge className={statusColor(lic.status)}>{t(`status.${lic.status}` as TranslationKeys)}</Badge>
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
          {showSeats && (
            <div>
              <p className="text-muted-foreground">{t("licenses.seats")}</p>
              <p className="font-medium">{seatCap === 0 ? t("common.unlimited") : seatCap}</p>
            </div>
          )}
        </div>

        {/* Maintenance period */}
        {isPerpetualPlan && (
          <div className="flex items-center justify-between gap-3 flex-wrap text-sm">
            <p className={cn(updatesEnded ? "text-muted-foreground" : "", updatesEndingSoon ? "text-amber-600" : "")}>
              {updatesUntil === null
                ? t("portal.updatesForLife")
                : updatesEnded
                  ? t("portal.updatesEnded", { date: formatDate(lic.updates_until) })
                  : `${t("portal.updatesUntil")} ${formatDate(lic.updates_until)}${updatesEndingSoon ? ` · ${t("portal.updatesEndingSoon")}` : ""}`}
            </p>
            {canRenewUpdates && (
              <Button
                size="sm"
                variant={updatesEnded || updatesEndingSoon ? "default" : "outline"}
                onClick={handleRenewUpdates}
                disabled={renewing}
              >
                {renewing ? t("common.loading") : t("portal.renewUpdates")}
              </Button>
            )}
          </div>
        )}

        {/* Billing actions (license holder only) */}
        {isOwner && lic.payment_provider === "stripe" && (
          <div className="flex gap-2 flex-wrap">
            <Button variant="outline" size="sm" onClick={() => setShowInvoices(true)}>
              {t("portal.viewInvoices")}
            </Button>
            {hasSubscription && (
              <Button variant="outline" size="sm" onClick={handleBillingPortal}>
                {t("portal.updatePayment")}
              </Button>
            )}
            {hasSubscription && (lic.status === "active" || lic.status === "trialing") && (
              <>
                <Button variant="outline" size="sm" onClick={() => setShowChangePlan(true)}>
                  {t("portal.changePlan")}
                </Button>
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
            {showUsage && <TabsTrigger value="usage">{t("analytics.usage")}</TabsTrigger>}
            {showSeats && <TabsTrigger value="seats">{t("portal.teamMembers")}</TabsTrigger>}
          </TabsList>

          <TabsContent value="overview" className="mt-4">
            <ActivationsSection license={lic} />
          </TabsContent>

          <TabsContent value="entitlements" className="mt-4">
            <EntitlementsSection entitlements={lic.plan?.entitlements || []} />
          </TabsContent>

          {showUsage && (
            <TabsContent value="usage" className="mt-4">
              <QuotaUsageSection license={lic} />
            </TabsContent>
          )}

          {showSeats && (
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

function ActivationsSection({ license }: { license: PortalLicense }) {
  const { t } = useI18n()
  const { user } = useAuth()
  const qc = useQueryClient()
  const [removing, setRemoving] = useState<Activation | null>(null)

  // The dedicated activations endpoint requires the caller to be the
  // license owner OR an *accepted* seat (portal_activations.go:84 —
  // "pending invites confer no authority", a deliberate security
  // gate). But /portal/licenses still lists the license for a
  // PENDING (unaccepted) seat too, so naively calling the endpoint
  // here would 404 and render a false "no devices" for a license we
  // just showed. Decide authorisation from the embedded data first;
  // only hit the gated endpoint when authorised. Unauthorised users
  // fall back to the read-only embedded list (the prior behaviour —
  // no regression, no false-empty, no delete controls).
  const myEmail = user?.email?.toLowerCase() ?? ""
  const isOwner = !!myEmail && license.email?.toLowerCase() === myEmail
  const mySeat = (license.seats || []).find((s) => s.email.toLowerCase() === myEmail && !s.removed_at)
  const isAcceptedSeat = !!mySeat?.accepted_at
  const authorized = isOwner || isAcceptedSeat

  const actQuery = useQuery({
    queryKey: ["portal", "activations", license.id],
    queryFn: () => portal.listActivations(license.license_key),
    enabled: authorized,
  })

  // Declared before any early return so the hook order is stable
  // every render (rules of hooks). Only exercised on the authorised
  // manage path below.
  const removeMut = useMutation({
    mutationFn: (activationId: string) => portal.removeActivation(license.license_key, activationId),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["portal", "activations", license.id] })
      // The card header's "N / max" count is rendered from
      // lic.activations, which comes from the separate
      // ["portal","licenses"] query — invalidate it too, else the
      // header keeps showing the pre-delete count while the list
      // below already dropped the row.
      qc.invalidateQueries({ queryKey: ["portal", "licenses"] })
    },
    // Errors surface via the global MutationCache toast (main.tsx).
    // Just close the dialog either way so the row doesn't get stuck.
    onSettled: () => {
      setRemoving(null)
    },
    onError: (e: Error) => showToast(e.message, "error"),
  })

  // Read-only path for unauthorised (pending-seat) viewers, and a
  // defensive fallback if the gated query errors for an authorised
  // user (race / transient). Uses the activations embedded in the
  // ["portal","licenses"] payload, which is populated regardless of
  // seat-accept state — so we never claim "no devices" falsely.
  const useReadOnly = !authorized || actQuery.isError
  if (useReadOnly) {
    const embedded = license.activations || []
    return (
      <div className="space-y-3">
        <p className="text-sm font-medium">
          {t("portal.activeDevices")} ({embedded.length}
          {license.plan?.max_activations ? `/${license.plan.max_activations}` : ""})
        </p>
        {embedded.length === 0 ? (
          <p className="text-sm text-muted-foreground py-4 text-center">{t("portal.noDevices")}</p>
        ) : (
          <div className="space-y-2">
            {embedded.map((act) => (
              <div key={act.id} className="flex items-center justify-between bg-muted/50 rounded px-3 py-2 text-sm">
                <div className="min-w-0 flex-1">
                  <div className="flex items-center gap-2">
                    <code className="text-xs truncate">{act.identifier}</code>
                    {act.label && <span className="text-muted-foreground text-xs">({act.label})</span>}
                    <span className="text-xs text-muted-foreground capitalize">{act.identifier_type}</span>
                  </div>
                  <p className="text-xs text-muted-foreground mt-0.5">
                    {t("portal.lastVerified")} {formatDate(act.last_verified)}
                  </p>
                </div>
              </div>
            ))}
          </div>
        )}
      </div>
    )
  }

  const activations = actQuery.data?.activations || []
  const max = actQuery.data?.max ?? 0

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <p className="text-sm font-medium">
          {t("portal.activeDevices")} ({activations.length}
          {max > 0 ? `/${max}` : ""})
        </p>
      </div>

      {actQuery.isLoading ? (
        <div className="h-16 animate-pulse bg-muted rounded-lg" />
      ) : activations.length === 0 ? (
        <p className="text-sm text-muted-foreground py-4 text-center">{t("portal.noDevices")}</p>
      ) : (
        <div className="space-y-2">
          {activations.map((act) => (
            <div key={act.id} className="flex items-center justify-between bg-muted/50 rounded px-3 py-2 text-sm">
              <div className="min-w-0 flex-1">
                <div className="flex items-center gap-2">
                  <code className="text-xs truncate">{act.identifier}</code>
                  {act.label && <span className="text-muted-foreground text-xs">({act.label})</span>}
                  <span className="text-xs text-muted-foreground capitalize">{act.identifier_type}</span>
                </div>
                <p className="text-xs text-muted-foreground mt-0.5">
                  {t("portal.lastVerified")} {formatDate(act.last_verified)}
                </p>
              </div>
              <Button
                variant="ghost"
                size="icon"
                className="h-7 w-7 shrink-0 text-destructive"
                disabled={removeMut.isPending && removeMut.variables === act.id}
                onClick={() => setRemoving(act)}
                aria-label={t("portal.removeDevice")}
              >
                <Trash2 className="h-3.5 w-3.5" />
              </Button>
            </div>
          ))}
        </div>
      )}

      <AlertDialog open={!!removing} onOpenChange={(o) => !o && setRemoving(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{t("portal.removeDeviceTitle")}</AlertDialogTitle>
            <AlertDialogDescription>
              {t("portal.removeDeviceDesc").replace("{device}", removing?.label || removing?.identifier || "")}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <div className="flex justify-end gap-2">
            <AlertDialogCancel>{t("common.cancel")}</AlertDialogCancel>
            <AlertDialogAction
              className="bg-destructive text-white hover:bg-destructive/90"
              onClick={() => removing && removeMut.mutate(removing.id)}
            >
              {t("portal.removeDevice")}
            </AlertDialogAction>
          </div>
        </AlertDialogContent>
      </AlertDialog>
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

function SeatsSection({ license }: { license: PortalLicense }) {
  const { t } = useI18n()
  const { user } = useAuth()
  const qc = useQueryClient()
  const [showInvite, setShowInvite] = useState(false)
  const [removing, setRemoving] = useState<Seat | null>(null)

  const seatsQuery = useQuery({
    queryKey: ["portal", "seats", license.id],
    queryFn: () => portal.listSeats(license.license_key),
  })

  const removeMut = useMutation({
    mutationFn: (seatId: string) => portal.removeSeat({ license_key: license.license_key, seat_id: seatId }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["portal", "seats", license.id] })
      setRemoving(null)
    },
    // Errors surface via the global MutationCache toast handler in
    // main.tsx — no per-mutation onError needed, else we'd double-
    // toast. Just close the dialog on failure so the row stays put.
    onSettled: () => {
      setRemoving(null)
    },
  })

  const seats = seatsQuery.data?.seats || []
  const activeCount = seats.filter((s) => !s.removed_at).length
  // maxSeats=0 means "no limit" per backend (internal/store/seats.go:81);
  // never gate the Add button in that case.
  const maxSeats = license.plan?.max_seats ?? 0
  const atLimit = maxSeats > 0 && activeCount >= maxSeats

  // canManage mirrors backend portalSeatMutationGuard: the license
  // owner (matched by email, case-insensitive) OR an accepted seat
  // with role=admin. Members and unaccepted invites only see the
  // roster; hiding the buttons prevents the "click → mystery 404 →
  // generic toast" UX failure mode.
  const myEmail = user?.email?.toLowerCase() ?? ""
  const isLicenseOwner = !!myEmail && license.email?.toLowerCase() === myEmail
  const mySeat = seats.find((s) => s.email.toLowerCase() === myEmail && !s.removed_at)
  const isAcceptedAdmin = !!mySeat?.accepted_at && mySeat.role === "admin"
  const canManage = isLicenseOwner || isAcceptedAdmin

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <p className="text-sm font-medium">
          {t("portal.teamMembers")} ({activeCount}/{maxSeats === 0 ? t("common.unlimitedSymbol") : maxSeats})
        </p>
        {canManage && (
          <Button size="sm" disabled={atLimit} onClick={() => setShowInvite(true)}>
            {t("portal.addMember")}
          </Button>
        )}
      </div>

      {canManage && atLimit && <p className="text-xs text-muted-foreground">{t("portal.seatLimit")}</p>}

      {seatsQuery.isLoading ? (
        <div className="h-16 animate-pulse bg-muted rounded-lg" />
      ) : seats.length === 0 ? (
        <p className="text-sm text-muted-foreground py-4 text-center">{t("portal.noMembers")}</p>
      ) : (
        <div className="space-y-2">
          {seats.map((s) => (
            <SeatRow
              key={s.id}
              seat={s}
              canManage={canManage}
              onRemove={() => setRemoving(s)}
              isRemoving={removeMut.isPending && removeMut.variables === s.id}
            />
          ))}
        </div>
      )}

      {showInvite && <InviteSeatDialog license={license} onClose={() => setShowInvite(false)} />}

      <AlertDialog open={!!removing} onOpenChange={(o) => !o && setRemoving(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{t("portal.removeMemberTitle")}</AlertDialogTitle>
            <AlertDialogDescription>
              {t("portal.removeMemberDesc").replace("{email}", removing?.email || "")}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <div className="flex justify-end gap-2">
            <AlertDialogCancel>{t("common.cancel")}</AlertDialogCancel>
            <AlertDialogAction
              className="bg-destructive text-white hover:bg-destructive/90"
              onClick={() => removing && removeMut.mutate(removing.id)}
            >
              {t("portal.removeMember")}
            </AlertDialogAction>
          </div>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  )
}

function SeatRow({
  seat,
  canManage,
  onRemove,
  isRemoving,
}: {
  seat: Seat
  canManage: boolean
  onRemove: () => void
  isRemoving: boolean
}) {
  const { t } = useI18n()
  const accepted = !!seat.accepted_at
  return (
    <div className="flex items-center justify-between bg-muted/50 rounded px-3 py-2 text-sm">
      <div className="min-w-0 flex-1">
        <div className="flex items-center gap-2">
          <span className="font-medium truncate">{seat.email}</span>
          <Badge variant="secondary" className="text-xs capitalize">
            {seat.role === "admin" ? t("portal.roleAdmin") : t("portal.roleMember")}
          </Badge>
          {!accepted && (
            <Badge variant="outline" className="text-xs">
              {t("licenses.invited")}
            </Badge>
          )}
        </div>
        <p className="text-xs text-muted-foreground mt-0.5">
          {accepted ? `${t("portal.lastVerified")} ${formatDate(seat.accepted_at!)}` : formatDate(seat.invited_at)}
        </p>
      </div>
      {canManage && (
        <Button
          variant="ghost"
          size="icon"
          className="h-7 w-7 shrink-0 text-destructive"
          disabled={isRemoving}
          onClick={onRemove}
          aria-label={t("portal.removeMember")}
        >
          <Trash2 className="h-3.5 w-3.5" />
        </Button>
      )}
    </div>
  )
}

function InviteSeatDialog({ license, onClose }: { license: PortalLicense; onClose: () => void }) {
  const { t } = useI18n()
  const qc = useQueryClient()
  const [email, setEmail] = useState("")
  const [role, setRole] = useState<"member" | "admin">("member")

  const inviteMut = useMutation({
    mutationFn: () => portal.addSeat({ license_key: license.license_key, email: email.trim(), role }),
    onSuccess: () => {
      showToast(t("licenses.seatInviteSent"))
      qc.invalidateQueries({ queryKey: ["portal", "seats", license.id] })
      onClose()
    },
    onError: (e: Error) => showToast(e.message, "error"),
  })

  const trimmed = email.trim()
  const isEmail = /^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(trimmed)
  const canSubmit = isEmail && !inviteMut.isPending

  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>{t("licenses.inviteTitle")}</DialogTitle>
          <DialogDescription>{t("licenses.inviteDesc")}</DialogDescription>
        </DialogHeader>
        <DialogBody>
          <div className="space-y-4">
            <div className="space-y-2">
              <Label htmlFor="invite-email">{t("common.email")}</Label>
              <Input
                id="invite-email"
                type="email"
                autoComplete="off"
                autoFocus
                value={email}
                onChange={(e) => setEmail(e.target.value)}
                placeholder="teammate@example.com"
                onKeyDown={(e) => {
                  if (e.key === "Enter" && canSubmit) inviteMut.mutate()
                }}
              />
            </div>

            <div className="space-y-2">
              <Label htmlFor="invite-role">{t("team.role")}</Label>
              <Select value={role} onValueChange={(v) => setRole(v as "member" | "admin")}>
                <SelectTrigger id="invite-role">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="member">{t("portal.roleMember")}</SelectItem>
                  <SelectItem value="admin">{t("portal.roleAdmin")}</SelectItem>
                </SelectContent>
              </Select>
            </div>

            <div className="flex justify-end gap-2">
              <Button variant="outline" onClick={onClose}>
                {t("common.cancel")}
              </Button>
              <Button disabled={!canSubmit} onClick={() => inviteMut.mutate()}>
                {t("licenses.sendInvite")}
              </Button>
            </div>
          </div>
        </DialogBody>
      </DialogContent>
    </Dialog>
  )
}

function QuotaUsageSection({ license }: { license: PortalLicense }) {
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
      return portal.cancelSubscription({ license_id: licenseId, immediate })
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["portal", "licenses"] })
      onClose()
    },
    onError: (e: Error) => showToast(e.message, "error"),
  })

  return (
    <Dialog open onOpenChange={onClose}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>{t("portal.cancelSubscription")}</DialogTitle>
          <DialogDescription>{t("portal.cancelDesc", { product: productName })}</DialogDescription>
        </DialogHeader>
        <DialogBody>
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
        </DialogBody>
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
      <DialogContent className="max-w-2xl">
        <DialogHeader>
          <DialogTitle>{t("portal.invoices")}</DialogTitle>
        </DialogHeader>
        <DialogBody>
          {isLoading ? (
            <div className="h-32 animate-pulse bg-muted rounded-lg" />
          ) : invoices.length === 0 ? (
            <p className="text-sm text-muted-foreground text-center py-8">{t("common.noData")}</p>
          ) : (
            <div className="space-y-2">
              {invoices.map((inv: Invoice) => (
                <div
                  key={inv.id}
                  className="flex items-center justify-between bg-muted/50 rounded-lg px-4 py-3 text-sm"
                >
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
        </DialogBody>
      </DialogContent>
    </Dialog>
  )
}

function ChangePlanDialog({ license, onClose }: { license: PortalLicense; onClose: () => void }) {
  const { t } = useI18n()
  const qc = useQueryClient()

  // Fetch available plans via portal API (not admin)
  const { data: plansData } = useQuery({
    queryKey: ["portal", "plans", license.product_id],
    queryFn: () => portal.listPlans(license.product_id),
  })
  // Only other subscription plans with a Stripe price: a subscription
  // can move between prices, not become a one-time purchase. Mirrors
  // the backend's change-plan gate.
  const plans = (plansData?.plans || []).filter(
    (p: Plan) => p.id !== license.plan_id && p.license_type === "subscription" && p.stripe_price_id,
  )

  const changeMut = useMutation({
    mutationFn: (newPriceId: string) => portal.changePlan({ license_id: license.id, new_price_id: newPriceId }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["portal", "licenses"] })
      onClose()
    },
    onError: (e: Error) => showToast(e.message, "error"),
  })

  return (
    <Dialog open onOpenChange={onClose}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>{t("portal.changePlan")}</DialogTitle>
          <DialogDescription>{t("portal.changePlanDesc")}</DialogDescription>
        </DialogHeader>
        <DialogBody>
          {plans.length === 0 ? (
            <p className="text-sm text-muted-foreground text-center py-4">{t("portal.noOtherPlans")}</p>
          ) : (
            <div className="space-y-2">
              {plans.map((plan: Plan) => (
                <div key={plan.id} className="flex items-center justify-between bg-muted/50 rounded-lg px-4 py-3">
                  <div>
                    <p className="font-medium text-sm">{plan.name}</p>
                    <p className="text-xs text-muted-foreground">
                      {plan.license_type}
                      {plan.billing_interval ? ` · ${plan.billing_interval}` : ""}
                    </p>
                  </div>
                  <Button
                    size="sm"
                    onClick={() => changeMut.mutate(plan.stripe_price_id || "")}
                    disabled={changeMut.isPending}
                  >
                    {changeMut.isPending ? t("common.loading") : t("portal.switchTo")}
                  </Button>
                </div>
              ))}
            </div>
          )}
        </DialogBody>
      </DialogContent>
    </Dialog>
  )
}
