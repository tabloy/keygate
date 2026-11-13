import { type QueryClient, useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { Minus, Plus } from "lucide-react"
import { useEffect, useState } from "react"
import { showToast } from "@/components/toast"
import { Button } from "@/components/ui/button"
import { Dialog, DialogBody, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Input } from "@/components/ui/input"
import { useDateFormatter } from "@/hooks/use-site-config"
import { useI18n } from "@/i18n"
import {
  errorCode,
  errorDetails,
  type Plan,
  type PortalLicense,
  portal,
  type SubscriptionChangePreview,
  type SubscriptionChangeResult,
} from "@/lib/api"

// Stripe amounts are in the currency's smallest unit, by Stripe's own
// count of decimals (not the browser's): none for the zero-decimal
// currencies, three for the three-decimal ones, two for every other,
// ISK and UGX included.
const STRIPE_ZERO_DECIMAL = new Set([
  "BIF",
  "CLP",
  "DJF",
  "GNF",
  "JPY",
  "KMF",
  "KRW",
  "MGA",
  "PYG",
  "RWF",
  "VND",
  "VUV",
  "XAF",
  "XOF",
  "XPF",
])
const STRIPE_THREE_DECIMAL = new Set(["BHD", "JOD", "KWD", "OMR", "TND"])
export function formatMoney(amount: number, currency: string, locale?: string) {
  const code = (currency || "usd").toUpperCase()
  const digits = STRIPE_ZERO_DECIMAL.has(code) ? 0 : STRIPE_THREE_DECIMAL.has(code) ? 3 : 2
  try {
    const fmt = new Intl.NumberFormat(locale, { style: "currency", currency: code })
    return fmt.format(amount / 10 ** digits)
  } catch {
    return `${(amount / 100).toFixed(2)} ${code}`
  }
}

// people is who is on the licence now: the owner and its members.
function peopleOn(license: PortalLicense) {
  const owner = license.email?.toLowerCase() ?? ""
  return (license.seats ?? []).filter((s) => !s.removed_at && s.email.toLowerCase() !== owner).length + 1
}

// clampSeats keeps a seat count inside what the plan sells.
function clampSeats(plan: Plan, n: number) {
  const min = plan.min_quantity ?? 1
  const max = plan.max_quantity ?? 100
  return Math.min(Math.max(n, min), max)
}

// SubscriptionChangeDialog changes what a subscription bills: its seats
// (target is the plan it is on), or its plan (target is another plan).
// A plan sold per seat lets the customer pick the seats first. The
// price shown comes from Stripe's own preview, and is confirmed before
// anything changes.
export function SubscriptionChangeDialog({
  license,
  target,
  onClose,
  onBack,
  keep,
}: {
  license: PortalLicense
  target: Plan
  onClose: () => void
  // onBack returns to the plan list the target was picked from.
  onBack?: () => void
  // keep asks for the terms the subscription has, which drops a change
  // waiting for payment: no seats to pick.
  keep?: boolean
}) {
  const { t, locale } = useI18n()
  const qc = useQueryClient()
  const current = license.quantity ?? 1
  const samePlan = target.id === license.plan_id
  const perSeat = !!target.per_seat
  const min = target.min_quantity ?? 1
  const max = target.max_quantity ?? 100
  const people = peopleOn(license)
  const seatsProduct = license.product?.type === "saas" || license.product?.type === "hybrid"
  // The seats to start from: what the licence has on this plan, and on
  // another plan at least a seat for everyone already on it (a plan not
  // sold per seat keeps a quantity of one, whatever its team).
  const [quantity, setQuantity] = useState(() => {
    if (!perSeat) return 1
    const from = license.plan?.per_seat ? current : 1
    // The seats it has stay as they are, even outside a range the
    // plan has since narrowed: the customer picks a new number.
    if (samePlan) return current
    return clampSeats(target, Math.max(from, seatsProduct ? people : 1))
  })
  const [step, setStep] = useState<"choose" | "review" | "result">(perSeat && !keep ? "choose" : "review")
  const [result, setResult] = useState<SubscriptionChangeResult | null>(null)
  const floating = target.license_model === "floating"
  // A change that lowers a limit below what is in use is refused (the
  // server says so too); one that leaves the licence no further over a
  // limit it was already over goes ahead.
  const seatLimitNow = license.seat_limit ?? 0
  const tooFew = seatsProduct && perSeat && quantity < people && (seatLimitNow === 0 || quantity < seatLimitNow)
  const devicesProduct = license.product?.type === "desktop" || license.product?.type === "hybrid"
  const devicesNow = license.activations?.length ?? 0
  const devicesAfter = (target.max_activations ?? 0) * quantity
  const tooManyDevices =
    devicesProduct && perSeat && devicesNow > devicesAfter && devicesAfter < (license.activation_limit ?? 0)
  // A change waiting for payment is dropped by asking for what the
  // subscription has now.
  const pendingNow =
    !!license.stripe_pending_update_expires_at &&
    new Date(license.stripe_pending_update_expires_at).getTime() > Date.now()
  const unchanged = samePlan && quantity === current && !pendingNow
  // Keeping what the subscription has drops a waiting change, whatever
  // range the plan sells now; any other number is held to the range.
  const keeping = samePlan && quantity === current && pendingNow
  const outOfRange = quantity > 0 && !keeping && (quantity < min || quantity > max)
  // Mid-typing (an emptied box) there is no number to describe.
  const described = quantity > 0 && !outOfRange

  const preview = useQuery({
    queryKey: ["portal", "change-preview", license.id, target.stripe_price_id, quantity],
    queryFn: () =>
      portal.previewChange({
        license_id: license.id,
        price_id: samePlan ? undefined : target.stripe_price_id,
        // Keeping asks for what Stripe bills now, whatever the card says.
        quantity: perSeat && !keep ? quantity : undefined,
      }),
    enabled: step === "review",
    retry: false,
    // A preview's proration date is only good for so long; never reuse
    // an old one when the customer comes back to this step.
    gcTime: 0,
    staleTime: 0,
  })

  const changeMut = useMutation({
    mutationFn: (p: SubscriptionChangePreview) =>
      portal.changeSubscription({ license_id: license.id, preview_token: p.preview_token }),
    meta: { ownErrors: true },
    onSuccess: (res) => {
      // The plan or the seats moved, and with them the team's and the
      // devices' limits: everything the portal shows is read again.
      qc.invalidateQueries({ queryKey: ["portal"] })
      if (res.status === "processing") {
        // Stripe's event usually follows within seconds: the licence is
        // read again then, so "it will show here shortly" holds without a
        // reload, closed dialog or not.
        for (const ms of [5000, 15000]) {
          setTimeout(() => qc.invalidateQueries({ queryKey: ["portal"] }), ms)
        }
      }
      if (res.status === "applied") {
        showToast(
          preview.data && dropsPending(preview.data)
            ? t("portal.pendingDropped")
            : samePlan
              ? t("portal.seatsChanged", { count: res.quantity })
              : t("portal.planChanged", { plan: res.plan_name }),
          "success",
        )
        onClose()
        return
      }
      setResult(res)
      setStep("result")
    },
    // Errors are shown in the dialog, which offers a fresh preview.
  })

  // Too many people for the change: lower the seats, choose more of them
  // on another plan sold per seat, or free room on a plan that is not.
  const seatsMessage = samePlan
    ? "portal.changeSeatsInUse"
    : perSeat
      ? "portal.changePlanSeatsInUse"
      : "portal.changePlanTooManyPeople"
  // Too many devices: remove some, or (on a plan sold per seat) choose
  // more seats.
  const devicesMessage = samePlan
    ? "portal.changeDevicesInUse"
    : perSeat
      ? "portal.changePlanDevicesInUse"
      : "portal.changePlanDevicesInUseFixed"
  // Refusals that say the licence shown is out of date bring it up to
  // date, so what the customer sees next is the subscription as it is.
  // A preview that found the licence out of line with Stripe brought it
  // back in line: the card behind the dialog is read again.
  useEffect(() => {
    const cur = preview.data?.current
    if (cur && (cur.quantity !== (license.quantity ?? 1) || cur.plan_id !== license.plan_id)) {
      refreshBehindPreview(qc)
    }
  }, [preview.data, license.quantity, license.plan_id, qc])
  useEffect(() => {
    for (const err of [preview.error, changeMut.error]) {
      if (err && staleCodes.includes(errorCode(err) ?? "")) refreshBehindPreview(qc)
    }
  }, [preview.error, changeMut.error, qc])
  const errorText = (err: unknown) => {
    const d = errorDetails(err)
    switch (errorCode(err)) {
      case "SEATS_IN_USE":
        return t(seatsMessage, { people: Number(d?.in_use ?? people), seats: Number(d?.seat_limit ?? quantity) })
      case "DEVICES_IN_USE":
        return t(d?.kind === "sessions" ? "portal.changeSessionsInUse" : devicesMessage, {
          inUse: Number(d?.in_use ?? devicesNow),
          limit: Number(d?.activation_limit ?? devicesAfter),
        })
      case "CHANGE_IN_PROGRESS":
        return t("portal.changeInProgress")
      case "DOWNGRADE_NOT_SUPPORTED":
        return t("portal.downgradeNotSupported")
      case "NO_CHANGE":
        return t("portal.noChange")
      case "PAYMENT_METHOD_UNSUPPORTED":
        return t("portal.changePaymentMethodUnsupported")
      case "RATE_LIMITED":
        return t("portal.changeTooFast")
      case "PAYMENTS_UNAVAILABLE":
        return t("portal.changeUnavailable")
      case "PLAN_INACTIVE":
        return t("portal.planInactive")
      case "QUANTITY_OUT_OF_RANGE":
        return t("portal.changeQuantityRange", { min: Number(d?.min ?? min), max: Number(d?.max ?? max) })
      case "PREVIEW_EXPIRED":
        return t("portal.changeExpired")
      case "SUBSCRIPTION_CHANGED":
        return t("portal.changeStale")
      case "SUBSCRIPTION_NOT_ACTIVE":
        return t("portal.changeNotActive")
      case "CHANGE_NOT_SUPPORTED":
        return t("portal.changeNotSupported")
      case "CHANGE_REFUSED":
        return t("portal.changeRefused")
      default:
        return err instanceof Error ? err.message : String(err)
    }
  }

  const title =
    step === "result"
      ? result?.status === "processing"
        ? t("portal.changeConfirmingTitle")
        : t("portal.changePaymentTitle")
      : keep || (samePlan && !perSeat) || (step === "review" && preview.data && dropsPending(preview.data))
        ? t("portal.dropPendingTitle")
        : samePlan
          ? t("portal.changeSeats")
          : t("portal.confirmSwitchTitle", { plan: target.name })
  const busy = changeMut.isPending

  return (
    <Dialog open onOpenChange={(o) => !o && !busy && onClose()}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>{title}</DialogTitle>
          <DialogDescription>
            {step === "choose"
              ? t("portal.changeSeatsDesc", { min, max })
              : step === "review"
                ? preview.data && dropsPending(preview.data)
                  ? t("portal.dropPendingDesc")
                  : t("portal.changeReviewDesc")
                : result?.status === "processing"
                  ? t("portal.changeProcessingDesc")
                  : t("portal.changePaymentNeededDesc")}
          </DialogDescription>
        </DialogHeader>
        <DialogBody>
          {step === "choose" && (
            <div className="space-y-4">
              <div className="flex items-center justify-center gap-3">
                <Button
                  variant="outline"
                  size="icon"
                  aria-label={t("portal.fewerSeats")}
                  disabled={quantity <= min}
                  onClick={() => setQuantity((q) => Math.max(q - 1, min))}
                >
                  <Minus className="h-4 w-4" />
                </Button>
                <Input
                  type="number"
                  inputMode="numeric"
                  aria-label={t("portal.seatCount")}
                  className="w-24 text-center text-lg"
                  min={min}
                  max={max}
                  value={quantity}
                  onChange={(e) => {
                    const n = Math.trunc(Number(e.target.value))
                    if (Number.isFinite(n)) setQuantity(n)
                  }}
                  onBlur={() => setQuantity((q) => clampSeats(target, q || min))}
                />
                <Button
                  variant="outline"
                  size="icon"
                  aria-label={t("portal.moreSeats")}
                  disabled={quantity >= max}
                  onClick={() => setQuantity((q) => Math.min(q + 1, max))}
                >
                  <Plus className="h-4 w-4" />
                </Button>
              </div>
              {described && (
                <p className="text-sm text-center text-muted-foreground">
                  {limitsText(t, quantity, (target.max_activations ?? 0) * quantity, license.product?.type, floating)}
                </p>
              )}
              {seatsProduct && (
                <p className="text-xs text-center text-muted-foreground">
                  {license.seat_limit
                    ? t("portal.seatsInUseNow", { people, current: license.seat_limit })
                    : t("portal.peopleInUseNow", { people })}
                </p>
              )}
              {outOfRange && (
                <p role="alert" className="text-sm text-destructive text-center">
                  {t("portal.changeQuantityRange", { min, max })}
                </p>
              )}
              {described && tooManyDevices && !tooFew && (
                <p role="alert" className="text-sm text-destructive text-center">
                  {t(devicesMessage, {
                    inUse: devicesNow,
                    limit: devicesAfter,
                  })}
                </p>
              )}
              {described && tooFew && (
                <p role="alert" className="text-sm text-destructive text-center">
                  {t(seatsMessage, { people, seats: quantity })}
                </p>
              )}
              <div className="flex justify-end gap-2">
                {/* Picked from the plan list: back to it, not out. */}
                <Button variant="outline" onClick={onBack ?? onClose}>
                  {onBack ? t("common.back") : t("common.cancel")}
                </Button>
                <Button
                  disabled={tooFew || tooManyDevices || unchanged || quantity < 1 || outOfRange}
                  onClick={() => setStep("review")}
                >
                  {t("portal.reviewChange")}
                </Button>
              </div>
            </div>
          )}

          {step === "review" && (
            <div className="space-y-4">
              {/* While a fresh preview loads, the old one is neither shown
                  nor confirmable: its proration date may have run out. */}
              {preview.isFetching ? (
                <div className="h-28 animate-pulse bg-muted rounded-lg" />
              ) : preview.isError ? (
                <p role="alert" className="text-sm text-destructive">
                  {errorText(preview.error)}
                </p>
              ) : preview.data ? (
                <PreviewSummary
                  preview={preview.data}
                  locale={locale}
                  productType={license.product?.type}
                  floating={floating}
                />
              ) : null}
              {changeMut.isError && (
                <p role="alert" className="text-sm text-destructive">
                  {errorText(changeMut.error)}
                </p>
              )}
              <div className="flex justify-end gap-2">
                <Button
                  variant="outline"
                  disabled={busy}
                  onClick={() => {
                    changeMut.reset()
                    if (perSeat && !keep) setStep("choose")
                    else if (onBack) onBack()
                    else onClose()
                  }}
                >
                  {(perSeat && !keep) || onBack ? t("common.back") : t("common.cancel")}
                </Button>
                {changeMut.isError &&
                ["PREVIEW_EXPIRED", "SUBSCRIPTION_CHANGED", "CHANGE_REFUSED"].includes(
                  errorCode(changeMut.error) ?? "",
                ) ? (
                  <Button
                    onClick={() => {
                      changeMut.reset()
                      preview.refetch()
                    }}
                  >
                    {t("portal.reviewAgain")}
                  </Button>
                ) : (
                  <Button
                    disabled={!preview.data || preview.isFetching || preview.isError || busy}
                    onClick={() => preview.data && changeMut.mutate(preview.data)}
                  >
                    {busy
                      ? t("common.loading")
                      : preview.data && dropsPending(preview.data)
                        ? t("portal.dropPendingConfirm")
                        : t("portal.confirmChange")}
                  </Button>
                )}
              </div>
            </div>
          )}

          {step === "result" && result && (
            <ChangeResult
              result={result}
              onClose={onClose}
              onPaying={() => {
                // Back from Stripe's tab, the portal reads the licence again.
                qc.invalidateQueries({ queryKey: ["portal"] })
                onClose()
              }}
            />
          )}
        </DialogBody>
      </DialogContent>
    </Dialog>
  )
}

// limitsText says what a licence holds in the terms its product uses:
// people on a saas product, devices on a desktop one, both on a hybrid.
// 0 people means no limit.
function limitsText(
  t: ReturnType<typeof useI18n>["t"],
  people: number,
  devices: number,
  productType: string | undefined,
  floating = false,
) {
  const peopleText = people > 0 ? t("portal.peopleCount", { count: people }) : t("portal.peopleUnlimited")
  // A floating plan counts machines in use at once, not activations.
  const devicesText = t(floating ? "portal.sessionsCount" : "portal.devicesCount", { count: devices })
  if (productType === "saas") return peopleText
  if (productType === "desktop") return devicesText
  return `${peopleText} · ${devicesText}`
}

// staleCodes are refusals that mean the licence shown is out of date.
const staleCodes = ["NO_CHANGE", "SUBSCRIPTION_CHANGED", "CHANGE_IN_PROGRESS"]

// refreshBehindPreview reads again what the portal shows behind an open
// preview: the licence, its team and its devices, but not the preview
// itself. These refreshes follow from the preview's own answer; reading
// it again would answer again, each time a new object, and loop.
function refreshBehindPreview(qc: QueryClient) {
  qc.invalidateQueries({ queryKey: ["portal"], predicate: (q) => q.queryKey[1] !== "change-preview" })
}

// dropsPending reports a "change" to the terms the subscription has
// already: all it does is drop the change waiting for payment.
function dropsPending(p: SubscriptionChangePreview) {
  return p.replaces_pending && p.current.price_id === p.target.price_id && p.current.quantity === p.target.quantity
}

function PreviewSummary({
  preview: p,
  locale,
  productType,
  floating,
}: {
  preview: SubscriptionChangePreview
  locale: string
  productType: string | undefined
  floating: boolean
}) {
  const formatDate = useDateFormatter()
  const { t } = useI18n()
  if (dropsPending(p)) {
    return <p className="text-sm">{t("portal.dropPendingExplain", { plan: p.current.plan_name })}</p>
  }
  const money = (n: number) => formatMoney(n, p.currency, locale)
  const describe = (side: SubscriptionChangePreview["current"]) =>
    side.per_seat ? t("portal.planWithSeats", { plan: side.plan_name, count: side.quantity }) : side.plan_name
  // Activations only: a floating plan's sessions come and go.
  const devicesOver = !floating && p.target.activation_limit > 0 && p.devices_in_use > p.target.activation_limit
  return (
    <div className="space-y-3 text-sm">
      <div className="rounded-lg bg-muted/50 px-4 py-3 space-y-1">
        <div className="flex justify-between gap-3">
          <span className="text-muted-foreground">{t("portal.changeFrom")}</span>
          <span className="font-medium text-right">{describe(p.current)}</span>
        </div>
        <div className="flex justify-between gap-3">
          <span className="text-muted-foreground">{t("portal.changeTo")}</span>
          <span className="font-medium text-right">{describe(p.target)}</span>
        </div>
        <div className="flex justify-between gap-3">
          <span className="text-muted-foreground">{t("portal.changeIncludes")}</span>
          <span className="text-right">
            {limitsText(t, p.target.seat_limit, p.target.activation_limit, productType, floating)}
          </span>
        </div>
      </div>
      {p.downgrade ? (
        <div className="space-y-1">
          <p className="text-base font-semibold">{t("portal.noChargeToday")}</p>
          {p.next_invoice_at ? (
            <p className="text-muted-foreground">
              {t("portal.downgradeNextInvoice", {
                date: formatDate(new Date(p.next_invoice_at * 1000).toISOString()),
                amount: money(p.next_invoice_total ?? 0),
              })}
            </p>
          ) : null}
          <p className="text-muted-foreground">{t("portal.downgradeNoRefund")}</p>
        </div>
      ) : p.total > 0 && p.due_now === 0 ? (
        <p>{t("portal.creditCoversIt", { amount: money(p.total) })}</p>
      ) : p.total > 0 ? (
        <div className="space-y-1">
          <p className="text-base font-semibold">{t("portal.dueToday", { amount: money(p.due_now) })}</p>
          {p.credit_balance > 0 && (
            <p className="text-muted-foreground">
              {t("portal.creditUsed", { credit: money(Math.min(p.credit_balance, p.total)), total: money(p.total) })}
            </p>
          )}
          {p.due_now > p.total && (
            <p className="text-muted-foreground">{t("portal.owedIncluded", { owed: money(p.due_now - p.total) })}</p>
          )}
          <p className="text-muted-foreground">{t("portal.appliesAfterPayment")}</p>
        </div>
      ) : p.total < 0 ? (
        <p>{t("portal.creditToBalance", { amount: money(-p.total) })}</p>
      ) : (
        <p>{t("portal.noChargeToday")}</p>
      )}
      {devicesOver && (
        <p className="text-amber-600">
          {t("portal.devicesOver", { inUse: p.devices_in_use, limit: p.target.activation_limit })}
        </p>
      )}
      {p.replaces_pending && <p className="text-muted-foreground">{t("portal.replacesPending")}</p>}
      {/* A downgrade of a subscription set to end shows no amount. */}
      {(!p.downgrade || p.next_invoice_at) && (
        <p className="text-xs text-muted-foreground">{t("portal.amountsByStripe")}</p>
      )}
    </div>
  )
}

function ChangeResult({
  result,
  onClose,
  onPaying,
}: {
  result: SubscriptionChangeResult
  onClose: () => void
  onPaying: () => void
}) {
  const formatDate = useDateFormatter()
  const { t } = useI18n()
  if (result.status === "processing") {
    return (
      <div className="space-y-4">
        <p className="text-sm">{t("portal.changeProcessing")}</p>
        <div className="flex justify-end">
          <Button onClick={onClose}>{t("common.close")}</Button>
        </div>
      </div>
    )
  }
  return (
    <div className="space-y-4">
      <p className="text-sm">
        {result.expires_at
          ? t("portal.changeDroppedBy", { date: formatDate(result.expires_at) })
          : t("portal.changeNotYet")}
      </p>
      <div className="flex justify-end gap-2">
        <Button variant="outline" onClick={onClose}>
          {t("portal.payLater")}
        </Button>
        {result.hosted_invoice_url && (
          <Button asChild>
            <a href={result.hosted_invoice_url} target="_blank" rel="noopener noreferrer" onClick={onPaying}>
              {t("portal.payNow")}
            </a>
          </Button>
        )}
      </div>
    </div>
  )
}

// PendingChangeNotice tells the owner a change waits for payment, and
// takes them to Stripe's page for it.
export function PendingChangeNotice({ license }: { license: PortalLicense }) {
  const formatDate = useDateFormatter()
  const { t } = useI18n()
  const qc = useQueryClient()
  const [opening, setOpening] = useState(false)
  const expires = license.stripe_pending_update_expires_at
  if (!expires || new Date(expires).getTime() <= Date.now()) return null
  const open = async () => {
    // The tab opens in the click itself, or a browser may block it; it
    // is sent to Stripe's page once the address is known.
    const tab = window.open("", "_blank")
    setOpening(true)
    try {
      const res = await portal.pendingChange({ license_id: license.id })
      if (res.status === "pending_payment" && res.hosted_invoice_url) {
        if (tab) {
          tab.opener = null
          tab.location.href = res.hosted_invoice_url
        } else {
          window.location.href = res.hosted_invoice_url
        }
      } else {
        tab?.close()
        if (res.status === "pending_payment") showToast(t("portal.pendingNoLink"))
        else showToast(t("portal.pendingGone"), "success")
      }
    } catch (err) {
      tab?.close()
      showToast(
        errorCode(err) === "RATE_LIMITED"
          ? t("portal.changeTooFast")
          : err instanceof Error
            ? err.message
            : String(err),
        "error",
      )
    } finally {
      setOpening(false)
      // Paid, dropped or still waiting: the server read Stripe just now.
      qc.invalidateQueries({ queryKey: ["portal"] })
    }
  }
  return (
    <div className="flex items-center justify-between gap-3 flex-wrap rounded-lg border border-amber-300 bg-amber-50 dark:bg-amber-950/30 px-3 py-2 text-sm">
      <p>{t("portal.pendingChange", { date: formatDate(expires) })}</p>
      <Button size="sm" onClick={open} disabled={opening}>
        {opening ? t("common.loading") : t("portal.payNow")}
      </Button>
    </div>
  )
}
