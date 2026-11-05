import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { Check, Copy, Key, Trash2 } from "lucide-react"
import { useState, useSyncExternalStore } from "react"
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
import { Separator } from "@/components/ui/separator"
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { useAuth } from "@/hooks/use-auth"
import { type TranslationKeys, useI18n } from "@/i18n"
import {
  type Activation,
  type Entitlement,
  errorCode,
  errorDetails,
  type Invoice,
  type Plan,
  type PortalLicense,
  portal,
  type Seat,
} from "@/lib/api"
import { cn, formatDate, statusColor } from "@/lib/utils"
import { formatMoney, PendingChangeNotice, SubscriptionChangeDialog } from "./subscription-change"

export default function PortalLicensesPage() {
  const { t } = useI18n()
  const { data, isLoading } = useQuery({
    queryKey: ["portal", "licenses"],
    queryFn: portal.licenses,
    // Coming back from Stripe's page in another tab, show what was paid,
    // however recently the list was read.
    refetchOnWindowFocus: "always",
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

// changedElsewhere: a 404 on something the page shows, which means it
// changed since the page loaded (the license key replaced in another
// tab or by an admin, a member or device already removed).
function changedElsewhere(e: Error) {
  return ["NOT_FOUND", "LICENSE_NOT_FOUND", "SEAT_NOT_FOUND"].includes(errorCode(e) ?? "")
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
  const [showChangeSeats, setShowChangeSeats] = useState(false)
  const [showUpgrade, setShowUpgrade] = useState(false)
  const [showCancel, setShowCancel] = useState(false)
  const productType = lic.product?.type || "perpetual"
  const showUsage = productType === "saas" || productType === "hybrid"
  // Seats UI shows for any plan that *could* have more than one person.
  // The seat limit counts the owner and 0 means "no limit", so 1 is a
  // single-seat license, hidden unless it somehow has members already
  // (the owner must still be able to see and remove them). A plan sold
  // per seat always shows it: buying more seats is how it grows.
  // Perpetual products don't have a portal-managed team, so gate on
  // product type too. The limit is the server's: on a plan sold per
  // seat it is the seats bought, not the plan's max_seats.
  const seatCap = lic.seat_limit ?? lic.plan?.max_seats ?? 0
  const activationCap = lic.activation_limit ?? lic.plan?.max_activations ?? 0
  const perSeat = !!lic.plan?.per_seat
  const hasMembers = (lic.seats ?? []).some((s) => !s.removed_at && s.email.toLowerCase() !== lic.email?.toLowerCase())
  const showSeats = (productType === "saas" || productType === "hybrid") && (seatCap !== 1 || hasMembers || perSeat)

  const copyKey = () => {
    navigator.clipboard.writeText(lic.license_key)
    setCopied(true)
    setTimeout(() => setCopied(false), 2000)
  }

  // Change plan, cancel and payment-method updates all act on a Stripe
  // subscription. A one-time purchase has none: the backend answers 400
  // to each, so don't offer them.
  const hasSubscription = lic.payment_provider === "stripe" && !!lic.stripe_subscription_id
  // Seats bought through Stripe are changed in the portal by the owner.
  // A plan sold in one fixed number of seats has nothing to change.
  const canChangeSeats =
    isOwner &&
    hasSubscription &&
    perSeat &&
    (lic.plan?.min_quantity ?? 1) < (lic.plan?.max_quantity ?? 100) &&
    (lic.status === "active" || lic.status === "trialing")
  // A licence on a free plan upgrades through a checkout and keeps its
  // key. The server says whether this one can (Stripe configured, a free
  // plan, not suspended or revoked, and a paid plan to move to).
  const canUpgrade = isOwner && lic.upgradable === true

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
  const [tab, setTab] = useState(defaultTab)
  // The team tab goes away when the last member of a single seat
  // license is removed; the panel must not stay selected and blank.
  const activeTab = tab === "seats" && !showSeats ? defaultTab : tab

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
        {!isOwner && <p className="text-xs text-muted-foreground -mt-2">{t("portal.memberKeyHint")}</p>}
        {/* Over the seat limit: the key is shown, and why it does not work. */}
        {!isOwner && lic.key_paused && (
          <p className="rounded-lg border border-amber-300 bg-amber-50 px-3 py-2 text-sm dark:bg-amber-950/30">
            {t("portal.keyPaused")}
          </p>
        )}

        {/* Overview stats */}
        <div className="grid grid-cols-2 md:grid-cols-4 gap-4 text-sm">
          <div>
            <p className="text-muted-foreground">{t("portal.validFrom")}</p>
            <p className="font-medium">{formatDate(lic.valid_from)}</p>
          </div>
          <div>
            <p className="text-muted-foreground">{t("licenses.validUntil")}</p>
            <p className="font-medium">{lic.valid_until ? formatDate(lic.valid_until) : t("licenses.perpetual")}</p>
          </div>
          {/* The count and cap are the team's: a member is told so, as
              the list below holds only their own devices. */}
          <div>
            <p className="text-muted-foreground">{t(isOwner ? "portal.devices" : "portal.teamDevices")}</p>
            <p className="font-medium">
              {lic.activation_count ?? lic.activations?.length ?? 0} / {activationCap || "-"}
            </p>
          </div>
          {/* A member sees who has joined, not pending invitations, so
              a count of seats in use is the owner's to see. */}
          {showSeats && isOwner && (
            <div>
              <p className="text-muted-foreground">{t("licenses.seats")}</p>
              <p className="font-medium">
                {seatCap === 0
                  ? t("common.unlimited")
                  : `${(lic.seats ?? []).filter((s) => !s.removed_at && s.email.toLowerCase() !== lic.email?.toLowerCase()).length + 1} / ${seatCap}`}
              </p>
            </div>
          )}
          {perSeat && !showSeats && (
            <div>
              <p className="text-muted-foreground">{t("portal.seatsBought")}</p>
              <p className="font-medium">{lic.quantity ?? 1}</p>
            </div>
          )}
        </div>

        {isOwner && <PendingChangeNotice license={lic} />}
        {/* Set to end: it runs to the date it is paid to, then stops. */}
        {isOwner &&
          lic.cancel_scheduled &&
          lic.valid_until &&
          ["active", "trialing", "past_due"].includes(lic.status) && (
            <p className="rounded-lg border px-3 py-2 text-sm text-muted-foreground">
              {t("portal.endsOn").replace("{date}", formatDate(lic.valid_until))}
            </p>
          )}

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

        {canUpgrade && (
          <div className="flex gap-2 flex-wrap">
            <Button size="sm" onClick={() => setShowUpgrade(true)}>
              {t("portal.upgrade")}
            </Button>
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
            {canChangeSeats && (
              <Button variant="outline" size="sm" onClick={() => setShowChangeSeats(true)}>
                {t("portal.changeSeats")}
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
        <Tabs value={activeTab} onValueChange={setTab}>
          <TabsList>
            <TabsTrigger value="overview">{t("portal.devices")}</TabsTrigger>
            <TabsTrigger value="entitlements">{t("plans.entitlements")}</TabsTrigger>
            {showUsage && <TabsTrigger value="usage">{t("analytics.usage")}</TabsTrigger>}
            {showSeats && <TabsTrigger value="seats">{t("portal.teamMembers")}</TabsTrigger>}
          </TabsList>

          <TabsContent value="overview" className="mt-4">
            <ActivationsSection license={lic} />
          </TabsContent>

          <TabsContent value="entitlements" className="mt-4">
            <EntitlementsSection entitlements={lic.features ?? lic.plan?.entitlements ?? []} />
          </TabsContent>

          {showUsage && (
            <TabsContent value="usage" className="mt-4">
              <QuotaUsageSection license={lic} />
            </TabsContent>
          )}

          {showSeats && (
            <TabsContent value="seats" className="mt-4">
              {/* More seats only where the plan sells more than the licence has. */}
              <SeatsSection
                license={lic}
                onAddSeats={
                  canChangeSeats && (lic.quantity ?? 1) < (lic.plan?.max_quantity ?? 100)
                    ? () => setShowChangeSeats(true)
                    : undefined
                }
              />
            </TabsContent>
          )}
        </Tabs>

        {showCancel && (
          <CancelDialog
            licenseId={lic.id}
            provider={lic.payment_provider || ""}
            productName={lic.product?.name || ""}
            scheduled={!!lic.cancel_scheduled}
            onClose={() => setShowCancel(false)}
          />
        )}

        {showInvoices && <InvoicesDialog licenseId={lic.id} onClose={() => setShowInvoices(false)} />}

        {showChangePlan && <ChangePlanDialog license={lic} onClose={() => setShowChangePlan(false)} />}
        {showChangeSeats && lic.plan && (
          <SubscriptionChangeDialog license={lic} target={lic.plan} onClose={() => setShowChangeSeats(false)} />
        )}
        {showUpgrade && <UpgradeDialog license={lic} onClose={() => setShowUpgrade(false)} />}
      </CardContent>
    </Card>
  )
}

function ActivationsSection({ license }: { license: PortalLicense }) {
  const { t } = useI18n()
  const { user } = useAuth()
  const qc = useQueryClient()
  // The device asked about stays set while the dialog closes, so its
  // text does not change under the closing animation.
  const [removing, setRemoving] = useState<Activation | null>(null)
  const [confirming, setConfirming] = useState(false)

  // The dedicated activations endpoint requires the caller to be the
  // license owner OR an *accepted* seat (portal_activations.go —
  // "pending invites confer no authority", a deliberate security
  // gate). /portal/licenses lists a licence only to those, but decide
  // from the embedded data anyway and fall back to the read-only
  // embedded list otherwise, so a stale page never claims "no
  // devices". A member sees, and removes, the devices their own key
  // activated; the owner sees everyone's, and whose each is.
  const myEmail = user?.email?.toLowerCase() ?? ""
  const isOwner = !!myEmail && license.email?.toLowerCase() === myEmail
  // An accepted seat at the viewer's address, should one made before
  // addresses were lower-cased leave a pending one beside it.
  const isAcceptedSeat = (license.seats || []).some(
    (s) => s.email.toLowerCase() === myEmail && !s.removed_at && !!s.accepted_at,
  )
  const authorized = isOwner || isAcceptedSeat
  const ownOnly = !isOwner

  const actQuery = useQuery({
    queryKey: ["portal", "activations", license.id, license.license_key],
    queryFn: () => portal.listActivations(license.license_key),
    enabled: authorized,
  })

  // Declared before any early return so the hook order is stable
  // every render (rules of hooks). Only exercised on the authorised
  // manage path below.
  const removeMut = useMutation({
    mutationFn: (activationId: string) => portal.removeActivation(license.license_key, activationId),
    meta: { ownErrors: true },
    onError: (e: Error) => showToast(changedElsewhere(e) ? t("portal.changedElsewhere") : e.message, "error"),
    // Read the list again either way: a device already gone (its
    // member removed, the key changed) must not stay on screen. The
    // card header's "N / max" count is rendered from lic.activations,
    // which comes from the separate ["portal","licenses"] query, so
    // that is read again too. Close the dialog either way.
    onSettled: () => {
      qc.invalidateQueries({ queryKey: ["portal", "activations", license.id] })
      qc.invalidateQueries({ queryKey: ["portal", "licenses"] })
      setConfirming(false)
    },
  })

  // Read-only fallback if the gated query errors (race / transient), or
  // for a viewer the page does not take for authorised. Uses the
  // activations embedded in the ["portal","licenses"] payload, which
  // the server has already narrowed to what this viewer may see — so we
  // never claim "no devices" falsely.
  const useReadOnly = !authorized || actQuery.isError
  if (useReadOnly) {
    const embedded = license.activations || []
    return (
      <div className="space-y-3">
        <p className="text-sm font-medium">
          {ownOnly
            ? `${t("portal.myDevices")} (${embedded.length})`
            : `${t("portal.devices")} (${embedded.length}${
                (license.activation_limit ?? license.plan?.max_activations)
                  ? `/${license.activation_limit ?? license.plan?.max_activations}`
                  : ""
              })`}
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
                    {!ownOnly && act.member_email && ` · ${t("portal.deviceOfMember", { email: act.member_email })}`}
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
  // The licence's limit, as the card shows it: read again with the
  // licence (on focus too), so a change paid in Stripe's tab shows here
  // as on the card.
  const max = license.activation_limit ?? actQuery.data?.max ?? 0

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <p className="text-sm font-medium">
          {ownOnly
            ? `${t("portal.myDevices")} (${activations.length})`
            : `${t("portal.devices")} (${activations.length}${max > 0 ? `/${max}` : ""})`}
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
                  {!ownOnly && act.member_email && ` · ${t("portal.deviceOfMember", { email: act.member_email })}`}
                </p>
              </div>
              <Button
                variant="ghost"
                size="icon"
                className="h-7 w-7 shrink-0 text-destructive"
                disabled={removeMut.isPending && removeMut.variables === act.id}
                onClick={() => {
                  setRemoving(act)
                  setConfirming(true)
                }}
                aria-label={t("portal.removeDevice")}
              >
                <Trash2 className="h-3.5 w-3.5" />
              </Button>
            </div>
          ))}
        </div>
      )}

      <AlertDialog open={confirming} onOpenChange={(o) => !o && setConfirming(false)}>
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

// An on/off feature, read as the server reads it: a bool is on only at
// "true", a flag is off only at "false".
const isSwitch = (ent: Entitlement) => ent.value_type === "bool" || ent.value_type === "flag"
const switchedOn = (ent: Entitlement) => (ent.value_type === "flag" ? ent.value !== "false" : ent.value === "true")

function EntitlementsSection({ entitlements }: { entitlements: Entitlement[] }) {
  const { t } = useI18n()
  // A feature that is switched off is not one the license includes.
  const included = entitlements.filter((ent) => !isSwitch(ent) || switchedOn(ent))
  if (included.length === 0) {
    return <p className="text-sm text-muted-foreground py-4 text-center">{t("portal.noEntitlements")}</p>
  }

  return (
    <div className="space-y-2">
      <p className="text-sm font-medium mb-2">{t("portal.featuresIncluded")}</p>
      {included.map((ent) => (
        <div key={ent.feature} className="flex items-center justify-between bg-muted/50 rounded px-3 py-2 text-sm">
          <div className="flex items-center gap-2">
            <Check className="h-4 w-4 text-emerald-600 shrink-0" />
            <span className="font-medium">{ent.feature}</span>
          </div>
          <div className="text-muted-foreground text-xs">
            {isSwitch(ent) ? (
              <Badge variant="secondary">{t("common.active")}</Badge>
            ) : ent.value_type === "quota" ? (
              <span>
                {/^\+?\d+$/.test(ent.value) && Number(ent.value) > 0
                  ? formatNumber(Number(ent.value))
                  : t("common.unlimited")}{" "}
                {ent.quota_unit || "units"} / {ent.quota_period || "period"}
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

// Resends by seat: in flight, or just made and resting until a time.
// Kept outside the component so a resend still rests after the Members
// tab is left and shown again (the tab unmounts it), and so each row
// keeps its own state while another row is sending.
const resends = new Map<string, "sending" | number>()
const resendListeners = new Set<() => void>()
let resendVersion = 0

function setResend(seatID: string, state: "sending" | number | undefined) {
  if (state === undefined) resends.delete(seatID)
  else resends.set(seatID, state)
  resendVersion++
  for (const l of resendListeners) l()
}

// markSent rests a seat's Resend button after an email went out.
function markSent(seatID: string) {
  const until = Date.now() + RESEND_SENT_MS
  setResend(seatID, until)
  setTimeout(() => {
    if (resends.get(seatID) === until) setResend(seatID, undefined)
  }, RESEND_SENT_MS)
}

function resendState(seatID: string): "idle" | "sending" | "sent" {
  const s = resends.get(seatID)
  if (s === "sending") return "sending"
  return typeof s === "number" && s > Date.now() ? "sent" : "idle"
}

function useResends() {
  return useSyncExternalStore(
    (onChange) => {
      resendListeners.add(onChange)
      return () => resendListeners.delete(onChange)
    },
    () => resendVersion,
  )
}

function SeatsSection({ license, onAddSeats }: { license: PortalLicense; onAddSeats?: () => void }) {
  const { t } = useI18n()
  const { user } = useAuth()
  const qc = useQueryClient()
  const [showInvite, setShowInvite] = useState(false)
  // The seat asked about stays set while the dialog closes, so its text
  // does not change under the closing animation.
  const [removing, setRemoving] = useState<Seat | null>(null)
  const [confirming, setConfirming] = useState(false)

  const seatsQuery = useQuery({
    queryKey: ["portal", "seats", license.id],
    queryFn: () => portal.listSeats(license.license_key),
  })

  const removeMut = useMutation({
    mutationFn: (seatId: string) => portal.removeSeat({ license_key: license.license_key, seat_id: seatId }),
    meta: { ownErrors: true },
    onSuccess: () =>
      showToast(t(removing?.accepted_at ? "licenses.memberRemoved" : "licenses.inviteCancelled"), "success"),
    onError: (e: Error) => showToast(changedElsewhere(e) ? t("portal.changedElsewhere") : e.message, "error"),
    // Read the team again either way. The license list carries the
    // seats that decide whether the team tab shows at all, and the
    // member's devices went with them. Close the dialog either way.
    onSettled: () => {
      qc.invalidateQueries({ queryKey: ["portal", "seats", license.id] })
      qc.invalidateQueries({ queryKey: ["portal", "licenses"] })
      qc.invalidateQueries({ queryKey: ["portal", "activations", license.id] })
      setConfirming(false)
    },
  })

  // Resending is inviting the same address again: the server renews the
  // pending invite's link and mails it, even at the seat limit. Accepted
  // in the meantime, the seat comes back as it is and nothing is sent.
  // Each send replaces the link the last one mailed, so the button says
  // "Sent" for a few seconds before it can be pressed again: a second
  // click straight away would only void the first email.
  useResends()
  const resendMut = useMutation({
    mutationFn: (seat: Seat) => portal.addSeat({ license_key: license.license_key, email: seat.email }),
    meta: { ownErrors: true },
    onMutate: (seat) => setResend(seat.id, "sending"),
    onSuccess: (seat, sent) => {
      showToast(
        t(seat.accepted_at ? "portal.inviteAlreadyJoined" : "portal.inviteResent").replace("{email}", seat.email),
        "success",
      )
      if (seat.accepted_at) setResend(sent.id, undefined)
      else markSent(sent.id)
    },
    // Cancelled elsewhere, sending it again is a new invite, which a full
    // team or an open upgrade checkout refuses.
    onError: (e: Error, seat) => {
      setResend(seat.id, undefined)
      showToast(
        errorCode(e) === "UPGRADE_IN_PROGRESS"
          ? t("portal.inviteDuringUpgrade")
          : errorCode(e) === "UPGRADE_SEAT_LIMIT"
            ? t("portal.inviteUpgradeSeatLimit")
            : errorCode(e) === "SEAT_LIMIT"
              ? t("portal.inviteSeatLimit")
              : changedElsewhere(e)
                ? t("portal.changedElsewhere")
                : e.message,
        "error",
      )
    },
    onSettled: () => {
      qc.invalidateQueries({ queryKey: ["portal", "seats", license.id] })
      qc.invalidateQueries({ queryKey: ["portal", "licenses"] })
    },
  })

  const seats = seatsQuery.data?.seats || []
  // A seat at the owner's own address (left behind when the license
  // email changed to a member's) is the owner, shown once, at the top.
  const ownerEmail = license.email?.toLowerCase() ?? ""
  const members = seats.filter((s) => s.email.toLowerCase() !== ownerEmail)
  // People on the license: the members plus the owner, who holds a seat
  // without a row, as max_seats counts them on the server. A member is
  // sent only those who have joined, so they see no limit beside it.
  const activeCount = members.filter((s) => !s.removed_at).length + 1
  // Members beyond the seat limit (the last to join) whose keys wait for
  // a free place.
  const pausedCount = members.filter((s) => s.paused).length
  // maxSeats=0 means "no limit"; never gate the Add button in that case.
  // The server's limit: the seats bought, on a plan sold per seat.
  const maxSeats = license.seat_limit ?? license.plan?.max_seats ?? 0
  const atLimit = maxSeats > 0 && activeCount >= maxSeats

  // canManage mirrors backend portalSeatMutationGuard: only the license
  // owner (matched by email, case-insensitive) manages the team.
  // Members only see the roster; hiding the buttons prevents the
  // "click → mystery 404 → generic toast" UX failure mode.
  const myEmail = user?.email?.toLowerCase() ?? ""
  const canManage = !!myEmail && license.email?.toLowerCase() === myEmail
  // Invites need a license in use, by the rule the server holds them to
  // (LicenseUsable): an active one up to its end plus the plan's grace
  // days, a canceled one up to its end. The team is still listed, and
  // members removed, whatever its state. A trial that moves to its
  // plan's fallback when it ends keeps at least a day, and any other
  // trial Stripe doesn't bill has none (License.GraceDays).
  const until = license.valid_until ? new Date(license.valid_until).getTime() : null
  let graceDays = license.plan?.grace_days ?? 7
  if (license.status === "trialing" && license.plan && !license.stripe_subscription_id) {
    graceDays = license.plan.trial_fallback_plan_id ? Math.max(graceDays, 1) : 0
  }
  const graceMs = graceDays * 24 * 60 * 60 * 1000
  const inUse =
    (["active", "trialing", "past_due"].includes(license.status) &&
      (until === null || Date.now() <= until + graceMs)) ||
    (license.status === "canceled" && until !== null && Date.now() < until)

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <p className="text-sm font-medium">
          {t("portal.teamMembers")} (
          {canManage ? `${activeCount}/${maxSeats === 0 ? t("common.unlimitedSymbol") : maxSeats}` : activeCount})
        </p>
        {canManage && (
          <Button size="sm" disabled={atLimit || !inUse} onClick={() => setShowInvite(true)}>
            {t("portal.addMember")}
          </Button>
        )}
      </div>

      {canManage && !inUse && <p className="text-xs text-muted-foreground">{t("portal.inviteNeedsActive")}</p>}
      {canManage && pausedCount > 0 && (
        <p className="text-xs text-amber-700">{t("portal.pausedMembers").replace("{count}", String(pausedCount))}</p>
      )}

      {canManage && inUse && atLimit && (
        <div className="flex items-center justify-between gap-2 flex-wrap">
          <div className="space-y-0.5 text-xs text-muted-foreground">
            <p>
              {onAddSeats
                ? t("portal.seatLimitBuyMore")
                : !license.plan?.per_seat
                  ? t("portal.seatLimit")
                  : license.stripe_subscription_id && license.status === "past_due"
                    ? t("portal.seatLimitSettleFirst")
                    : t("portal.seatLimitFull")}
            </p>
            {/* Cancelling an invitation frees a seat for a new one, but
                does not let a paused member back: pausing counts only
                those who joined. */}
            {pausedCount === 0 && members.some((s) => !s.removed_at && !s.accepted_at) && (
              <p>{t("portal.seatLimitPendingHint")}</p>
            )}
          </div>
          {onAddSeats && (
            <Button size="sm" variant="outline" onClick={onAddSeats}>
              {t("portal.addSeats")}
            </Button>
          )}
        </div>
      )}

      {seatsQuery.isLoading ? (
        <div className="h-16 animate-pulse bg-muted rounded-lg" />
      ) : (
        <div className="space-y-2">
          <div className="flex items-center gap-2 bg-muted/50 rounded px-3 py-2 text-sm">
            <span className="font-medium truncate">{license.email}</span>
            <Badge variant="secondary" className="text-xs">
              {t("portal.roleOwner")}
            </Badge>
          </div>
          {members.length === 0 && (
            <p className="text-sm text-muted-foreground py-2 text-center">{t("portal.noMembers")}</p>
          )}
          {members.map((s) => (
            <SeatRow
              key={s.id}
              seat={s}
              canManage={canManage}
              onRemove={() => {
                setRemoving(s)
                setConfirming(true)
              }}
              isRemoving={removeMut.isPending && removeMut.variables === s.id}
              onResend={canManage && inUse ? () => resendMut.mutate(s) : undefined}
              resendState={resendState(s.id)}
            />
          ))}
        </div>
      )}

      {showInvite && <InviteSeatDialog license={license} onClose={() => setShowInvite(false)} />}

      <AlertDialog open={confirming} onOpenChange={(o) => !o && setConfirming(false)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>
              {t(removing?.accepted_at ? "portal.removeMemberTitle" : "portal.cancelInviteTitle")}
            </AlertDialogTitle>
            <AlertDialogDescription>
              {t(removing?.accepted_at ? "portal.removeMemberDesc" : "portal.cancelInviteDesc").replace(
                "{email}",
                removing?.email || "",
              )}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <div className="flex justify-end gap-2">
            {/* "Cancel" beside "Cancel invitation" reads as the same
                thing; a pending invite's dialog says what dismissing does. */}
            <AlertDialogCancel>{t(removing?.accepted_at ? "common.cancel" : "portal.keepInvite")}</AlertDialogCancel>
            <AlertDialogAction
              className="bg-destructive text-white hover:bg-destructive/90"
              onClick={() => removing && removeMut.mutate(removing.id)}
            >
              {t(removing?.accepted_at ? "portal.removeMember" : "portal.cancelInvite")}
            </AlertDialogAction>
          </div>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  )
}

// How long a resent invite's button says "Sent" (see resendMut).
const RESEND_SENT_MS = 10_000

function SeatRow({
  seat,
  canManage,
  onRemove,
  isRemoving,
  onResend,
  resendState,
}: {
  seat: Seat
  canManage: boolean
  onRemove: () => void
  isRemoving: boolean
  // Set while the owner can invite: a pending invite can be sent again.
  onResend?: () => void
  resendState: "idle" | "sending" | "sent"
}) {
  const { t } = useI18n()
  const accepted = !!seat.accepted_at
  const expired = !accepted && !!seat.invite_expires_at && new Date(seat.invite_expires_at).getTime() < Date.now()
  return (
    <div className="flex items-center justify-between bg-muted/50 rounded px-3 py-2 text-sm">
      <div className="min-w-0 flex-1">
        <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
          <span className="font-medium truncate max-w-full" title={seat.email}>
            {seat.email}
          </span>
          <Badge variant="secondary" className="text-xs">
            {t("portal.roleMember")}
          </Badge>
          {!accepted && (
            <Badge variant="outline" className={cn("text-xs", expired && "text-destructive")}>
              {t(expired ? "portal.inviteExpired" : "portal.seatPending")}
            </Badge>
          )}
          {seat.paused && (
            <Badge variant="outline" className="text-xs text-amber-700" title={t("portal.seatPausedHint")}>
              {t("portal.seatPaused")}
            </Badge>
          )}
        </div>
        <p className="text-xs text-muted-foreground mt-0.5">
          {accepted
            ? `${t("portal.seatJoinedOn")} ${formatDate(seat.accepted_at!)}`
            : `${t("portal.seatInvitedOn")} ${formatDate(seat.invited_at)}`}
        </p>
      </div>
      {!accepted && onResend && (
        <Button
          variant="outline"
          size="sm"
          className="h-7 shrink-0 px-2 text-xs"
          disabled={resendState !== "idle"}
          onClick={onResend}
        >
          {resendState === "sent" ? (
            <>
              <Check className="h-3 w-3 text-emerald-600" />
              {t("portal.inviteSentShort")}
            </>
          ) : (
            t("portal.resendInvite")
          )}
        </Button>
      )}
      {canManage && (
        <Button
          variant="ghost"
          size="icon"
          className="h-7 w-7 shrink-0 text-destructive"
          disabled={isRemoving}
          onClick={onRemove}
          aria-label={t(accepted ? "portal.removeMember" : "portal.cancelInvite")}
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

  const inviteMut = useMutation({
    mutationFn: () => portal.addSeat({ license_key: license.license_key, email: email.trim() }),
    // An address already a member comes back as it is, with no email;
    // one already invited gets its invitation sent again, with a new
    // link, as Resend does.
    onSuccess: (seat) => {
      const listed = qc.getQueryData<{ seats: Seat[] }>(["portal", "seats", license.id])?.seats
      const wasPending = listed?.some((s) => s.id === seat.id && !s.accepted_at && !s.removed_at)
      if (wasPending && !seat.accepted_at) markSent(seat.id)
      showToast(
        seat.accepted_at
          ? t("portal.inviteAlreadyJoined").replace("{email}", seat.email)
          : wasPending
            ? t("portal.inviteResent").replace("{email}", seat.email)
            : t("licenses.seatInviteSent"),
        "success",
      )
      qc.invalidateQueries({ queryKey: ["portal", "seats", license.id] })
      qc.invalidateQueries({ queryKey: ["portal", "licenses"] })
      onClose()
    },
    meta: { ownErrors: true },
    onError: (e: Error) => {
      // Full, or no longer taking members (its state or key changed
      // since the page loaded): the card behind is read again, so it
      // shows the license as it is.
      if (["SEAT_LIMIT", "UPGRADE_SEAT_LIMIT", "LICENSE_NOT_FOUND"].includes(errorCode(e) ?? "")) {
        qc.invalidateQueries({ queryKey: ["portal", "licenses"] })
        qc.invalidateQueries({ queryKey: ["portal", "seats", license.id] })
      }
      // Nothing to retry: the dialog goes, and the card shows why.
      if (errorCode(e) === "LICENSE_NOT_FOUND") onClose()
      showToast(
        errorCode(e) === "UPGRADE_IN_PROGRESS"
          ? t("portal.inviteDuringUpgrade")
          : errorCode(e) === "UPGRADE_SEAT_LIMIT"
            ? t("portal.inviteUpgradeSeatLimit")
            : errorCode(e) === "SEAT_LIMIT"
              ? t("portal.inviteSeatLimit")
              : errorCode(e) === "LICENSE_NOT_FOUND"
                ? t("portal.changedElsewhere")
                : e.message,
        "error",
      )
    },
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
                placeholder="member@example.com"
                onKeyDown={(e) => {
                  if (e.key === "Enter" && canSubmit) inviteMut.mutate()
                }}
              />
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
  const { t } = useI18n()
  const quotaEntitlements = (license.features ?? license.plan?.entitlements ?? []).filter(
    (e) => e.value_type === "quota",
  )

  // A paused member's key reads no usage until a seat is free.
  if (license.key_paused) {
    return <p className="text-sm text-muted-foreground py-4 text-center">{t("portal.usagePaused")}</p>
  }

  if (quotaEntitlements.length === 0) {
    return <p className="text-sm text-muted-foreground py-4 text-center">No quota-based features.</p>
  }

  return (
    <div className="space-y-4">
      <p className="text-sm font-medium mb-2">Quota Usage</p>
      {quotaEntitlements.map((ent) => (
        <QuotaBar key={ent.feature} entitlement={ent} licenseKey={license.license_key} />
      ))}
    </div>
  )
}

function QuotaBar({ entitlement, licenseKey }: { entitlement: Entitlement; licenseKey: string }) {
  const { t } = useI18n()
  const { data, isLoading } = useQuery({
    queryKey: ["portal", "quota", licenseKey, entitlement.feature],
    queryFn: () => portal.quotaStatus({ license_key: licenseKey, feature: entitlement.feature }),
  })

  // The limit the server enforces; 0 means no limit, as it does there.
  const limit = data?.limit ?? (Number(entitlement.value) || 0)
  const unlimited = limit <= 0
  const used = data?.used ?? 0
  const percentage = unlimited ? 0 : Math.min((used / limit) * 100, 100)
  const isWarning = percentage >= 80

  if (isLoading) {
    return <div className="h-12 animate-pulse bg-muted rounded-lg" />
  }

  return (
    <div className="space-y-1.5">
      <div className="flex items-center justify-between text-sm">
        <span className="font-medium">{entitlement.feature}</span>
        <span className={cn("text-xs", isWarning ? "text-amber-600 font-medium" : "text-muted-foreground")}>
          {unlimited
            ? `${formatNumber(used)} / ${t("common.unlimited")}`
            : `${formatNumber(used)} / ${formatNumber(limit)} (${Math.round(percentage)}%)`}
        </span>
      </div>
      <div className={cn("h-2.5 bg-muted rounded-full overflow-hidden", unlimited && "hidden")}>
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
  scheduled,
  onClose,
}: {
  licenseId: string
  // Already set to end at the period end: what is left is to end it now.
  scheduled: boolean
  provider: string
  productName: string
  onClose: () => void
}) {
  const { t } = useI18n()
  const qc = useQueryClient()
  const [immediate, setImmediate] = useState(scheduled)

  const cancelMut = useMutation({
    mutationFn: () => {
      return portal.cancelSubscription({ license_id: licenseId, immediate })
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["portal", "licenses"] })
      showToast(t(immediate ? "portal.cancelledNow" : "portal.cancelledAtPeriodEnd"), "success")
      onClose()
    },
    onError: (e: Error) => showToast(e.message, "error"),
  })

  return (
    <Dialog open onOpenChange={onClose}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>{t(scheduled ? "portal.endNowTitle" : "portal.cancelSubscription")}</DialogTitle>
          <DialogDescription>
            {scheduled
              ? t("portal.endNowDesc", { product: productName })
              : t("portal.cancelDesc", { product: productName })}
          </DialogDescription>
        </DialogHeader>
        <DialogBody>
          <div className="space-y-4">
            {provider === "stripe" && !scheduled && (
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
                {cancelMut.isPending ? t("common.loading") : t(scheduled ? "portal.endNow" : "portal.confirmCancel")}
              </Button>
            </div>
          </div>
        </DialogBody>
      </DialogContent>
    </Dialog>
  )
}

function InvoicesDialog({ licenseId, onClose }: { licenseId: string; onClose: () => void }) {
  const { t, locale } = useI18n()
  const { data, isLoading } = useQuery({
    queryKey: ["portal", "invoices", licenseId],
    queryFn: () => portal.getInvoices(licenseId),
  })
  const invoices = data?.invoices || []

  return (
    <Dialog open onOpenChange={onClose}>
      <DialogContent className="max-w-2xl h-[min(640px,85vh)]">
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
                      {formatMoney(inv.status === "paid" ? inv.amount_paid : inv.amount_due, inv.currency, locale)}
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

// planKind is a plan's type and billing period in the reader's language.
function planKind(t: ReturnType<typeof useI18n>["t"], plan: Plan): string {
  const types: Record<string, TranslationKeys> = {
    subscription: "plans.subscription",
    perpetual: "plans.perpetual",
    trial: "plans.trial",
  }
  const intervals: Record<string, TranslationKeys> = { month: "plans.monthly", year: "plans.yearly" }
  const type = types[plan.license_type] ? t(types[plan.license_type]) : plan.license_type
  const interval = plan.billing_interval
    ? intervals[plan.billing_interval]
      ? t(intervals[plan.billing_interval])
      : plan.billing_interval
    : ""
  return interval ? `${type} · ${interval}` : type
}

// UpgradeDialog lists the product's paid plans for a licence nothing is
// billing yet. Picking one goes to Stripe checkout; once paid, the same
// licence moves to that plan.
function UpgradeDialog({ license, onClose }: { license: PortalLicense; onClose: () => void }) {
  const { t } = useI18n()
  const { data: plansData, isLoading } = useQuery({
    queryKey: ["portal", "plans", license.product_id],
    queryFn: () => portal.listPlans(license.product_id),
  })
  // Mirrors the server's upgrade targets: priced, not a trial, and no
  // update period on sale.
  const plans = (plansData?.plans || []).filter(
    (p: Plan) =>
      p.id !== license.plan_id &&
      !!p.stripe_price_id &&
      p.license_type !== "trial" &&
      !(p.license_type === "perpetual" && (p.updates_days ?? 0) > 0),
  )
  const [starting, setStarting] = useState("")
  const start = async (plan: Plan) => {
    setStarting(plan.id)
    try {
      const res = await portal.upgrade({ license_id: license.id, plan_id: plan.id })
      window.location.href = res.url
    } catch (err) {
      const d = errorDetails(err)
      showToast(
        errorCode(err) === "SEATS_IN_USE"
          ? t("portal.upgradeTooManyPeople", { people: Number(d?.in_use ?? 0), max: Number(d?.seat_limit ?? 0) })
          : err instanceof Error
            ? err.message
            : "An error occurred",
        "error",
      )
      setStarting("")
    }
  }

  return (
    <Dialog open onOpenChange={onClose}>
      <DialogContent className="h-[min(560px,85vh)]">
        <DialogHeader>
          <DialogTitle>{t("portal.upgrade")}</DialogTitle>
          <DialogDescription>{t("portal.upgradeDesc")}</DialogDescription>
        </DialogHeader>
        <DialogBody>
          {isLoading ? (
            <p className="text-sm text-muted-foreground text-center py-4">{t("common.loading")}</p>
          ) : plans.length === 0 ? (
            <p className="text-sm text-muted-foreground text-center py-4">{t("portal.noUpgradePlans")}</p>
          ) : (
            <div className="space-y-2">
              {plans.map((plan: Plan) => (
                <div key={plan.id} className="flex items-center justify-between bg-muted/50 rounded-lg px-4 py-3">
                  <div>
                    <p className="font-medium text-sm">{plan.name}</p>
                    <p className="text-xs text-muted-foreground">
                      {planKind(t, plan)}
                      {plan.per_seat ? ` · ${t("portal.perSeatChooseAtCheckout")}` : ""}
                    </p>
                  </div>
                  <Button size="sm" onClick={() => start(plan)} disabled={starting !== ""}>
                    {starting === plan.id ? t("common.loading") : t("portal.upgrade")}
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

function ChangePlanDialog({ license, onClose }: { license: PortalLicense; onClose: () => void }) {
  const { t } = useI18n()

  // Fetch available plans via portal API (not admin)
  const { data: plansData } = useQuery({
    queryKey: ["portal", "plans", license.product_id],
    queryFn: () => portal.listPlans(license.product_id),
  })
  // Only other subscription plans with a Stripe price: a subscription
  // can move between prices, not become a one-time purchase. Mirrors
  // the backend's change-plan gate. With a change waiting for payment,
  // the current plan is offered too: choosing it drops that change.
  const waiting =
    !!license.stripe_pending_update_expires_at && new Date(license.stripe_pending_update_expires_at) > new Date()
  const plans = (plansData?.plans || []).filter(
    (p: Plan) => (p.id !== license.plan_id || waiting) && p.license_type === "subscription" && p.stripe_price_id,
  )

  // Picking a plan opens the change itself: seats for a plan sold per
  // seat, then Stripe's price for the switch, confirmed before anything
  // changes.
  const [chosen, setChosen] = useState<Plan | null>(null)
  if (chosen) {
    return (
      <SubscriptionChangeDialog
        license={license}
        target={chosen}
        keep={chosen.id === license.plan_id}
        onClose={onClose}
        onBack={() => setChosen(null)}
      />
    )
  }

  return (
    <Dialog open onOpenChange={onClose}>
      <DialogContent className="h-[min(560px,85vh)]">
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
                    <p className="font-medium text-sm">
                      {plan.name}
                      {plan.id === license.plan_id && (
                        <span className="text-muted-foreground font-normal"> · {t("portal.currentPlan")}</span>
                      )}
                    </p>
                    <p className="text-xs text-muted-foreground">
                      {planKind(t, plan)}
                      {plan.per_seat ? ` · ${t("portal.perSeat")}` : ""}
                    </p>
                  </div>
                  <Button size="sm" onClick={() => setChosen(plan)}>
                    {plan.id === license.plan_id ? t("portal.keepPlan") : t("portal.switchTo")}
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
