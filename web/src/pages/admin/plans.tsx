import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { Copy, Package, Pencil, Plus, Trash2 } from "lucide-react"
import { useEffect, useRef, useState } from "react"
import { Link } from "react-router-dom"
import { CopyableId } from "@/components/copyable-id"
import { ProductSelect } from "@/components/product-select"
import { queryReadState } from "@/components/query-read-state"
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
import { Card, CardContent } from "@/components/ui/card"
import {
  DataTable,
  DataTableBody,
  DataTableCell,
  DataTableEmpty,
  DataTableHead,
  DataTableHeader,
  DataTablePagination,
  DataTableRow,
  useServerPagination,
} from "@/components/ui/data-table"
import {
  Dialog,
  DialogBody,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select"
import { Separator } from "@/components/ui/separator"
import { useI18n } from "@/i18n"
import { admin, type Entitlement, errorCode, errorDetails, type Plan } from "@/lib/api"

// model.MaxPlanQuantity: the most seats a plan sold per seat can sell.
const MAX_PLAN_QUANTITY = 10000

import { boolColor, formatDate } from "@/lib/utils"

export default function PlansPage() {
  const { t } = useI18n()
  const qc = useQueryClient()
  const [productFilter, setProductFilter] = useState<string>("")
  const [search, setSearch] = useState("")
  const pg = useServerPagination(10, [productFilter, search])
  const readQuery0 = useQuery({
    // One row is all this page needs from the catalogue: whether the
    // install has any product at all, and which one a new form starts
    // on. The pickers fetch their own candidates, with search.
    queryKey: ["admin", "products", "newest"],
    queryFn: () => admin.listProducts({ limit: 1 }),
  })
  const { data: productsData } = readQuery0
  const readQuery1 = useQuery({
    queryKey: ["admin", "plans", productFilter, search, pg.page, pg.pageSize],
    queryFn: () => admin.listPlans({ product_id: productFilter, search, ...pg.params }),
  })
  const { data, isLoading } = readQuery1
  const [creating, setCreating] = useState(false)
  const [editing, setEditing] = useState<Plan | null>(null)
  const [deleting, setDeleting] = useState<Plan | null>(null)
  // An edit that lowers max_seats below what some licenses already hold,
  // waiting for the admin to confirm it.
  // The content stays set after closing, so the dialog's closing
  // animation does not show it emptied; only the open flag clears.
  const [seatWarning, setSeatWarning] = useState<{ update: Partial<Plan> & { id: string }; over: number } | null>(null)
  const [seatWarningOpen, setSeatWarningOpen] = useState(false)
  // While the impact is being fetched the save button stays busy. Each
  // check takes a number, and opening or closing the editor moves it on:
  // an answer for an older number (an editor since closed, even one
  // reopened on the same plan) is dropped, success or error, and does
  // not clear the busy state of a newer check.
  const [checkingSeats, setCheckingSeats] = useState(false)
  const checkSeq = useRef(0)
  const openEditor = (plan: Plan | null) => {
    checkSeq.current++
    setCheckingSeats(false)
    setEditing(plan)
  }

  const products = productsData?.products || []
  const { items: plans, total, totalPages } = pg.from(data, data?.plans)

  const createMut = useMutation({
    mutationFn: admin.createPlan,
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["admin", "plans"] })
      setCreating(false)
      showToast(t("toast.planCreated"), "success")
    },
    meta: { ownErrors: true },
    onError: (e: Error) => showToast(e.message, "error"),
  })
  // A plan trial plans fall back to keeps qualifying: say which ones.
  const planError = (e: Error) => {
    if (errorCode(e) !== "TRIAL_FALLBACK_IN_USE") return e.message
    const names = errorDetails(e)?.plans
    return t("plans.trialFallbackTargetInUse", {
      plans: Array.isArray(names) ? names.join(t("common.listSeparator")) : "",
    })
  }
  const updateMut = useMutation({
    mutationFn: ({ id, ...data }: Partial<Plan> & { id: string }) => admin.updatePlan(id, data),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["admin", "plans"] })
      openEditor(null)
    },
    meta: { ownErrors: true },
    onError: (e: Error) =>
      showToast(errorCode(e) === "PER_SEAT_LOCKED" ? t("plans.perSeatLockedError") : planError(e), "error"),
  })
  // A plan's limits apply to every license on it at once. Lowering
  // max_seats pauses the members who joined last on the licenses
  // already above it, so the admin sees how many first.
  const saveEdit = async (plan: Plan, update: Partial<Plan> & { id: string }) => {
    const next = update.max_seats ?? plan.max_seats
    const prev = plan.max_seats
    // A plan sold per seat holds the seats each license bought, not
    // max_seats, so there is nothing to check.
    const perSeat = update.per_seat ?? plan.per_seat
    if (!perSeat && next > 0 && (prev === 0 || next < prev)) {
      if (checkingSeats) return
      const seq = ++checkSeq.current
      setCheckingSeats(true)
      try {
        const { licenses_over } = await admin.planSeatImpact(plan.id, next)
        if (checkSeq.current !== seq) return
        if (licenses_over > 0) {
          setSeatWarning({ update, over: licenses_over })
          setSeatWarningOpen(true)
          return
        }
      } catch (e) {
        if (checkSeq.current === seq) showToast((e as Error).message, "error")
        return
      } finally {
        if (checkSeq.current === seq) setCheckingSeats(false)
      }
    }
    updateMut.mutate(update)
  }
  const deleteMut = useMutation({
    mutationFn: (id: string) => admin.deletePlan(id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["admin", "plans"] })
      setDeleting(null)
      showToast(t("toast.planDeleted"), "success")
    },
    meta: { ownErrors: true },
    onError: (e: Error) => showToast(planError(e), "error"),
  })

  const readState = queryReadState([readQuery0, readQuery1], [readQuery0])
  if (readState.blocked) return readState.blocked

  if (!isLoading && products.length === 0) {
    return (
      <div className="space-y-6">
        {readState.notice}
        <div>
          <h1 className="text-2xl font-bold tracking-tight sr-only md:not-sr-only">{t("plans.title")}</h1>
          <p className="text-muted-foreground">{t("plans.subtitle")}</p>
        </div>
        <Card>
          <CardContent className="py-12 text-center">
            <Package className="h-12 w-12 mx-auto text-muted-foreground mb-4" />
            <p className="text-lg font-medium">{t("licenses.noProducts")}</p>
            <p className="text-muted-foreground mt-1 mb-4">{t("licenses.noProductsDesc")}</p>
            <Button asChild>
              <Link to="/admin/products">
                <Plus className="h-4 w-4 mr-2" /> {t("products.createTitle")}
              </Link>
            </Button>
          </CardContent>
        </Card>
      </div>
    )
  }

  return (
    <div className="space-y-6">
      {readState.notice}
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div className="min-w-0">
          <h1 className="text-2xl font-bold tracking-tight sr-only md:not-sr-only">{t("plans.title")}</h1>
          <p className="text-muted-foreground">{t("plans.subtitle")}</p>
        </div>
        <Button onClick={() => setCreating(true)}>
          <Plus className="h-4 w-4 mr-2" /> {t("plans.new")}
        </Button>
      </div>

      <div className="flex flex-wrap gap-3">
        <Input
          placeholder={t("common.search")}
          value={search}
          onChange={(e) => setSearch(e.target.value)}
          className="w-full sm:w-64"
        />
        {/* "all" maps back to no filter here: it used to be sent as a
            product_id of its own, which matched nothing. */}
        <ProductSelect
          value={productFilter}
          onChange={setProductFilter}
          allLabel={t("plans.allProducts")}
          className="w-full sm:w-48"
        />
      </div>

      <Card>
        <CardContent className="pt-6">
          {isLoading ? (
            <div className="h-32 animate-pulse bg-muted rounded-lg" />
          ) : (
            <>
              <DataTable>
                <DataTableHeader>
                  <DataTableRow>
                    <DataTableHead>{t("common.name")}</DataTableHead>
                    <DataTableHead>{t("common.product")}</DataTableHead>
                    <DataTableHead>{t("plans.licenseType")}</DataTableHead>
                    <DataTableHead>{t("plans.maxActivations")}</DataTableHead>
                    <DataTableHead>{t("plans.maxSeats")}</DataTableHead>
                    <DataTableHead>{t("common.status")}</DataTableHead>
                    <DataTableHead>{t("common.created")}</DataTableHead>
                    <DataTableHead className="w-24 text-right">{t("common.actions")}</DataTableHead>
                  </DataTableRow>
                </DataTableHeader>
                <DataTableBody>
                  {plans.length === 0 && <DataTableEmpty colSpan={8} message={t("plans.empty")} />}
                  {plans.map((p) => {
                    const prodType = p.product?.type
                    const supportsActivations = prodType === "desktop" || prodType === "hybrid"
                    const supportsSeats = prodType === "saas" || prodType === "hybrid"
                    return (
                      <DataTableRow key={p.id}>
                        <DataTableCell className="font-medium">{p.name}</DataTableCell>
                        <DataTableCell className="text-muted-foreground">
                          {p.product?.name || p.product_id}
                        </DataTableCell>
                        <DataTableCell>
                          <Badge variant="secondary">{p.license_type}</Badge>
                        </DataTableCell>
                        <DataTableCell>
                          {supportsActivations ? (
                            p.per_seat ? (
                              t("plans.perSeatValue", { count: p.max_activations })
                            ) : (
                              p.max_activations
                            )
                          ) : (
                            <span className="text-muted-foreground" title={t("plans.notApplicableForType")}>
                              —
                            </span>
                          )}
                        </DataTableCell>
                        <DataTableCell>
                          {p.per_seat && supportsSeats ? (
                            t("plans.perSeatRange", { min: p.min_quantity ?? 1, max: p.max_quantity ?? 100 })
                          ) : supportsSeats ? (
                            p.max_seats || 0
                          ) : (
                            <span className="text-muted-foreground" title={t("plans.notApplicableForType")}>
                              —
                            </span>
                          )}
                        </DataTableCell>
                        <DataTableCell>
                          <div className="flex flex-wrap gap-1">
                            <Badge className={boolColor(p.active)}>
                              {p.active ? t("common.active") : t("common.inactive")}
                            </Badge>
                            {p.self_signup && <Badge variant="outline">{t("plans.selfSignup")}</Badge>}
                          </div>
                        </DataTableCell>
                        <DataTableCell className="text-muted-foreground text-xs">
                          {formatDate(p.created_at)}
                        </DataTableCell>
                        <DataTableCell>
                          <div className="flex justify-end gap-1">
                            {p.checkout_id && (
                              <Button
                                variant="ghost"
                                size="icon"
                                title={t("plans.copyCheckoutLink")}
                                onClick={() => {
                                  const url = `${window.location.origin}/pay/${p.checkout_id}`
                                  navigator.clipboard.writeText(url)
                                  showToast(t("plans.linkCopied"), "success")
                                }}
                              >
                                <Copy className="h-4 w-4" />
                              </Button>
                            )}
                            <Button variant="ghost" size="icon" onClick={() => openEditor(p)}>
                              <Pencil className="h-4 w-4" />
                            </Button>
                            <Button variant="ghost" size="icon" onClick={() => setDeleting(p)}>
                              <Trash2 className="h-4 w-4 text-destructive" />
                            </Button>
                          </div>
                        </DataTableCell>
                      </DataTableRow>
                    )
                  })}
                </DataTableBody>
              </DataTable>
              {total > 0 && (
                <DataTablePagination
                  page={pg.page}
                  totalPages={totalPages}
                  total={total}
                  pageSize={pg.pageSize}
                  onPageChange={pg.setPage}
                  onPageSizeChange={pg.setPageSize}
                />
              )}
            </>
          )}
        </CardContent>
      </Card>

      {/* Create. Mounted only while open: the dialog keeps its form in
          component state, so a permanently mounted one would hand the
          next "New plan" whatever was typed — or created — last time. */}
      {creating && (
        <PlanDialog
          open
          onClose={() => setCreating(false)}
          products={products}
          onSubmit={(d) => createMut.mutate(d)}
          loading={createMut.isPending}
          title={t("plans.createTitle")}
        />
      )}

      {/* Edit */}
      {editing && (
        <PlanDialog
          open
          onClose={() => openEditor(null)}
          plan={editing}
          products={products}
          onSubmit={(d) => saveEdit(editing, { id: editing.id, ...d })}
          loading={updateMut.isPending || checkingSeats}
          title={t("plans.editTitle")}
        />
      )}

      {/* Lowering max seats below what some licenses hold */}
      <AlertDialog open={seatWarningOpen} onOpenChange={setSeatWarningOpen}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{t("plans.seatsLowerTitle")}</AlertDialogTitle>
            <AlertDialogDescription>
              {t("plans.seatsLowerConfirm")
                .replace("{count}", String(seatWarning?.over ?? 0))
                .replace("{max}", String(seatWarning?.update.max_seats ?? 0))}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <div className="flex justify-end gap-2">
            <AlertDialogCancel>{t("common.cancel")}</AlertDialogCancel>
            <AlertDialogAction
              onClick={() => {
                if (seatWarning) updateMut.mutate(seatWarning.update)
                setSeatWarningOpen(false)
              }}
            >
              {t("common.save")}
            </AlertDialogAction>
          </div>
        </AlertDialogContent>
      </AlertDialog>

      {/* Delete */}
      <AlertDialog open={!!deleting} onOpenChange={() => setDeleting(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>
              {t("common.delete")} "{deleting?.name}"?
            </AlertDialogTitle>
            <AlertDialogDescription>{t("plans.deleteConfirm")}</AlertDialogDescription>
          </AlertDialogHeader>
          <div className="flex justify-end gap-2">
            <AlertDialogCancel>{t("common.cancel")}</AlertDialogCancel>
            <AlertDialogAction
              className="bg-destructive text-white hover:bg-destructive/90"
              onClick={() => deleting && deleteMut.mutate(deleting.id)}
            >
              {t("common.delete")}
            </AlertDialogAction>
          </div>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  )
}

function PlanDialog({
  open,
  onClose,
  plan,
  products,
  onSubmit,
  loading,
  title,
}: {
  open: boolean
  onClose: () => void
  plan?: Plan
  products: { id: string; name: string; type?: string }[]
  onSubmit: (data: Partial<Plan>) => void
  loading: boolean
  title: string
}) {
  const { t } = useI18n()
  const [form, setForm] = useState({
    product_id: plan?.product_id || products[0]?.id || "",
    name: plan?.name || "",
    slug: plan?.slug || "",
    license_type: plan?.license_type || "subscription",
    // An existing plan keeps its stored interval; the server omits an
    // empty one from JSON, so "undefined" here means "none", not
    // "unset". Only a brand-new plan defaults to monthly.
    billing_interval: plan ? (plan.license_type === "perpetual" ? "" : (plan.billing_interval ?? "")) : "month",
    max_activations: plan?.max_activations ?? 3,
    max_seats: plan?.max_seats ?? 1,
    trial_days: plan?.trial_days ?? 0,
    grace_days: plan?.grace_days ?? 7,
    license_model: plan?.license_model || "standard",
    floating_timeout: plan?.floating_timeout ?? 30,
    token_ttl_days: plan?.token_ttl_days ?? 0,
    active: plan?.active ?? true,
    stripe_price_id: plan?.stripe_price_id || "",
    updates_days: plan?.updates_days ?? 0,
    renewal_days: plan?.renewal_days ?? 0,
    stripe_renewal_price_id: plan?.stripe_renewal_price_id || "",
    self_signup: plan?.self_signup ?? false,
    signup_daily_cap: plan?.signup_daily_cap ?? 0,
    signup_consent_text: plan?.signup_consent_text || "",
    trial_fallback_plan_id: plan?.trial_fallback_plan_id || "",
    per_seat: plan?.per_seat ?? false,
    min_quantity: plan?.min_quantity ?? 1,
    max_quantity: plan?.max_quantity ?? 100,
  })

  const set = (key: string, val: string | number | boolean) => setForm((f) => ({ ...f, [key]: val }))

  // A perpetual plan is paid once and never renews, so it has no
  // billing interval. The server clears it too; mirror that here so
  // the form doesn't show a period the plan will never bill on.
  const isPerpetual = form.license_type === "perpetual"
  // Self sign-up hands licences out for free, so the server refuses it
  // on a plan with a price, an update period or renewals. The option is
  // only offered where it can be saved.
  const charges =
    !!form.stripe_price_id.trim() ||
    (isPerpetual && (form.updates_days > 0 || form.renewal_days > 0 || !!form.stripe_renewal_price_id.trim()))
  const setLicenseType = (v: string) => {
    setForm((f) => ({ ...f, license_type: v, billing_interval: v === "perpetual" ? "" : f.billing_interval }))
  }
  // Only subscriptions are sold per seat; the option goes away, and off,
  // for any other type.
  const canPerSeat = form.license_type === "subscription"
  const perSeat = canPerSeat && form.per_seat

  // Capability map keyed by the currently-selected product's type.
  // Mirrors backend model.ProductSupports — DO NOT diverge or admins
  // will see fields the server then rejects on submit.
  // The plan's own product when editing (it may be outside the page
  // the picker offers), the picked one when creating.
  // Read the product by id rather than looking it up in whatever page
  // the picker happens to hold: a product found by search is not on
  // that page, and falling back to "hybrid" offered fields the server
  // then refused with INCOMPATIBLE_PRODUCT_TYPE.
  const { data: pickedProduct } = useQuery({
    queryKey: ["admin", "product", form.product_id],
    queryFn: () => admin.getProduct(form.product_id),
    enabled: !!form.product_id && !plan?.product,
  })
  // Plans a trial can fall back to when it ends: free, non-trial plans
  // of the same product (mirrors model.Plan.TrialFallbackRefusal). The
  // one already saved stays listed even if it no longer qualifies, so
  // the form shows what the plan holds rather than silently dropping it,
  // marked so the admin knows trials will expire instead (the sweep
  // re-checks the plan and does not move them).
  const isTrial = form.license_type === "trial"
  const {
    data: siblingPlans,
    isLoading: siblingPlansLoading,
    isError: siblingPlansFailed,
  } = useQuery({
    queryKey: ["admin", "plans", "trial-fallback", form.product_id],
    queryFn: () => admin.listPlans({ product_id: form.product_id, limit: 100 }),
    enabled: isTrial && !!form.product_id,
  })
  const qualifiesAsFallback = (p: Plan) =>
    p.id !== plan?.id &&
    p.license_type !== "trial" &&
    !p.per_seat &&
    !p.stripe_price_id &&
    !p.updates_days &&
    !p.renewal_days &&
    !p.stripe_renewal_price_id
  const fallbackOptions = (siblingPlans?.plans ?? []).filter(
    (p) => p.id === form.trial_fallback_plan_id || qualifiesAsFallback(p),
  )
  const selectedFallback = fallbackOptions.find((p) => p.id === form.trial_fallback_plan_id)
  const selectedProduct = plan?.product || pickedProduct
  const productType = selectedProduct?.type || "hybrid"
  const supports = {
    activations: productType === "desktop" || productType === "hybrid",
    seats: productType === "saas" || productType === "hybrid",
  }

  // When the product type doesn't expose a capability, force the
  // corresponding numeric field to 0 on submit (avoids sending
  // max_activations=3 for a saas product just because the form
  // pre-filled it before the product was selected).
  const handleSubmit = () => {
    // The range a plan sold per seat is bought in, checked here so the
    // error is in the admin's language (the server checks it too).
    if (
      perSeat &&
      !(form.min_quantity >= 1 && form.min_quantity <= form.max_quantity && form.max_quantity <= MAX_PLAN_QUANTITY)
    ) {
      showToast(t("plans.quantityRangeInvalid", { max: MAX_PLAN_QUANTITY }), "error")
      return
    }
    const payload: Partial<Plan> & Record<string, unknown> = { ...form }
    if (!supports.activations) {
      payload.max_activations = 0
      payload.license_model = "standard"
      payload.floating_timeout = 0
      payload.token_ttl_days = 0
    }
    if (!supports.seats) {
      payload.max_seats = 0
    }
    // A plan sold per seat holds the seats bought; the server keeps
    // max_seats for it. On any other plan the bounds are not used.
    payload.per_seat = perSeat
    if (perSeat) {
      delete payload.max_seats
    } else {
      delete payload.min_quantity
      delete payload.max_quantity
    }
    // A price, an update period or renewals set here switch self
    // sign-up off rather than have the save refused.
    if (charges) {
      payload.self_signup = false
    }
    // With sign-up off its settings are hidden, so they go back as the
    // plan had them: a value typed and then hidden must not fail the
    // save where the operator can no longer see it.
    if (!payload.self_signup) {
      payload.signup_daily_cap = plan?.signup_daily_cap ?? 0
      payload.signup_consent_text = plan?.signup_consent_text ?? ""
    }
    // Only a trial plan has trials to end; the server clears it too.
    if (!isTrial) {
      payload.trial_fallback_plan_id = ""
    }
    onSubmit(payload)
  }

  return (
    <Dialog open={open} onOpenChange={onClose}>
      <DialogContent className="max-w-lg h-[85vh]">
        <DialogHeader>
          <DialogTitle>{title}</DialogTitle>
          <DialogDescription>{t("plans.formDesc")}</DialogDescription>
          {plan && <CopyableId id={plan.id} />}
        </DialogHeader>
        <form
          onSubmit={(e) => {
            e.preventDefault()
            handleSubmit()
          }}
          className="flex min-h-0 flex-1 flex-col gap-4"
        >
          <DialogBody className="space-y-4">
            <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
              <div className="space-y-2 sm:col-span-2">
                <Label>{t("common.product")}</Label>

                {/* A plan cannot move to another product — its licences
                  and entitlements belong to this one, and the update
                  endpoint takes no product_id. */}
                <ProductSelect
                  value={form.product_id}
                  // A fallback belongs to one product: picking another
                  // product drops it rather than submit a hidden plan.
                  onChange={(v) => setForm((f) => ({ ...f, product_id: v, trial_fallback_plan_id: "" }))}
                  current={plan?.product}
                  className="w-full"
                  disabled={!!plan}
                  withType
                />
                {/* Capability hint — what this product type allows on plans */}
                <p className="text-xs text-muted-foreground">
                  {productType === "desktop" && t("plans.hintDesktop")}
                  {productType === "saas" && t("plans.hintSaas")}
                  {productType === "hybrid" && t("plans.hintHybrid")}
                </p>
              </div>
              <div className="space-y-2">
                <Label>{t("common.name")}</Label>
                <Input
                  value={form.name}
                  onChange={(e) => {
                    set("name", e.target.value)
                    if (!plan) set("slug", e.target.value.toLowerCase().replace(/[^a-z0-9]+/g, "-"))
                  }}
                  required
                />
              </div>
              <div className="space-y-2">
                <Label>{t("products.slug")}</Label>
                <Input value={form.slug} onChange={(e) => set("slug", e.target.value)} required />
              </div>
              <div className="space-y-2">
                <Label>{t("plans.licenseType")}</Label>
                <Select value={form.license_type} onValueChange={setLicenseType}>
                  <SelectTrigger>
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="subscription">{t("plans.subscription")}</SelectItem>
                    <SelectItem value="perpetual">{t("plans.perpetual")}</SelectItem>
                    <SelectItem value="trial">{t("plans.trial")}</SelectItem>
                  </SelectContent>
                </Select>
              </div>
              <div className="space-y-2">
                <Label>{t("plans.billingInterval")}</Label>
                <Select
                  value={form.billing_interval || "none"}
                  onValueChange={(v) => set("billing_interval", v === "none" ? "" : v)}
                  disabled={isPerpetual}
                >
                  <SelectTrigger>
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="none">{t("plans.none")}</SelectItem>
                    <SelectItem value="month">{t("plans.monthly")}</SelectItem>
                    <SelectItem value="year">{t("plans.yearly")}</SelectItem>
                  </SelectContent>
                </Select>
              </div>
              {canPerSeat && (
                <div className="space-y-2 sm:col-span-2">
                  <div className="flex items-center gap-3">
                    <input
                      type="checkbox"
                      checked={form.per_seat}
                      // Set when the plan is created (the server refuses a switch).
                      disabled={!!plan}
                      onChange={(e) => set("per_seat", e.target.checked)}
                      className="h-4 w-4 rounded border-input accent-primary"
                      id="plan-per-seat"
                    />
                    <Label htmlFor="plan-per-seat">{t("plans.perSeat")}</Label>
                  </div>
                  <p className="text-xs text-muted-foreground">
                    {productType === "desktop"
                      ? t("plans.perSeatHintDesktop")
                      : productType === "saas"
                        ? t("plans.perSeatHintSaas")
                        : t("plans.perSeatHintHybrid")}
                  </p>
                  {plan && <p className="text-xs text-muted-foreground">{t("plans.perSeatLocked")}</p>}
                </div>
              )}
              {perSeat && (
                <>
                  <div className="space-y-2">
                    <Label>{t("plans.minQuantity")}</Label>
                    <Input
                      type="number"
                      min={1}
                      max={10000}
                      value={form.min_quantity}
                      onChange={(e) => set("min_quantity", Number(e.target.value))}
                    />
                  </div>
                  <div className="space-y-2">
                    <Label>{t("plans.maxQuantity")}</Label>
                    <Input
                      type="number"
                      min={1}
                      max={10000}
                      value={form.max_quantity}
                      onChange={(e) => set("max_quantity", Number(e.target.value))}
                    />
                  </div>
                </>
              )}
              {supports.activations && (
                <div className="space-y-2">
                  <Label>
                    {perSeat
                      ? form.license_model === "floating"
                        ? t("plans.sessionsPerSeat")
                        : t("plans.devicesPerSeat")
                      : t("plans.maxActivations")}
                  </Label>
                  <Input
                    type="number"
                    min={1}
                    max={10000}
                    value={form.max_activations}
                    onChange={(e) => set("max_activations", Number(e.target.value))}
                  />
                </div>
              )}
              {supports.seats && !perSeat && (
                <div className="space-y-2">
                  <Label>{t("plans.maxSeats")}</Label>
                  <Input
                    type="number"
                    min={0}
                    max={100001}
                    value={form.max_seats}
                    onChange={(e) => set("max_seats", Number(e.target.value))}
                  />
                  <p className="text-xs text-muted-foreground">{t("plans.maxSeatsHint")}</p>
                </div>
              )}
              {supports.activations && (
                <div className="space-y-2">
                  <Label>{t("plans.licenseModel")}</Label>
                  <Select value={form.license_model} onValueChange={(v) => set("license_model", v)}>
                    <SelectTrigger>
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      <SelectItem value="standard">{t("plans.modelStandard")}</SelectItem>
                      <SelectItem value="floating">{t("plans.modelFloating")}</SelectItem>
                    </SelectContent>
                  </Select>
                </div>
              )}
              {supports.activations && form.license_model === "floating" && (
                <div className="space-y-2">
                  <Label>{t("plans.floatingTimeout")}</Label>
                  <Input
                    type="number"
                    min={1}
                    max={1440}
                    value={form.floating_timeout}
                    onChange={(e) => set("floating_timeout", Number(e.target.value))}
                  />
                </div>
              )}
              {supports.activations && (
                <div className="space-y-2">
                  <Label>{t("plans.tokenTtlDays")}</Label>
                  <Input
                    type="number"
                    min={0}
                    max={365}
                    value={form.token_ttl_days}
                    onChange={(e) => set("token_ttl_days", Number(e.target.value))}
                  />
                  <p className="text-xs text-muted-foreground">{t("plans.tokenTtlDaysHint")}</p>
                </div>
              )}
              <div className="space-y-2">
                <Label>{t("plans.trialDays")}</Label>
                <Input
                  type="number"
                  min={0}
                  max={365}
                  value={form.trial_days}
                  onChange={(e) => set("trial_days", Number(e.target.value))}
                />
              </div>
              <div className="space-y-2">
                <Label>{t("plans.graceDays")}</Label>
                <Input
                  type="number"
                  min={0}
                  max={365}
                  value={form.grace_days}
                  onChange={(e) => set("grace_days", Number(e.target.value))}
                />
              </div>
              {isTrial && (
                <div className="space-y-2 sm:col-span-2">
                  <Label>{t("plans.trialFallback")}</Label>
                  <Select
                    value={form.trial_fallback_plan_id || "none"}
                    onValueChange={(v) => set("trial_fallback_plan_id", v === "none" ? "" : v)}
                    // Editing, the saved choice stays as it is until its
                    // options are known, rather than cleared unseen.
                    disabled={!!plan && (siblingPlansLoading || siblingPlansFailed)}
                  >
                    <SelectTrigger>
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      <SelectItem value="none">{t("plans.trialFallbackNone")}</SelectItem>
                      {fallbackOptions.map((p) => (
                        <SelectItem key={p.id} value={p.id}>
                          {p.name}
                          {!p.active && ` (${t("common.inactive")})`}
                          {!qualifiesAsFallback(p) && ` (${t("plans.trialFallbackNoLongerQualifies")})`}
                        </SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                  {siblingPlansLoading ? (
                    <p className="text-xs text-muted-foreground">{t("common.loading")}</p>
                  ) : siblingPlansFailed ? (
                    <p className="text-xs text-destructive">
                      {t(plan ? "plans.trialFallbackLoadFailed" : "plans.trialFallbackLoadFailedNew")}
                    </p>
                  ) : selectedFallback && !qualifiesAsFallback(selectedFallback) ? (
                    <p className="text-xs text-amber-700 dark:text-amber-400">{t("plans.trialFallbackInvalid")}</p>
                  ) : siblingPlans && fallbackOptions.length === 0 ? (
                    <p className="text-xs text-muted-foreground">
                      {t(plan ? "plans.trialFallbackNoneAvailableEdit" : "plans.trialFallbackNoneAvailable")}
                    </p>
                  ) : (
                    // What applies after the move, for what the product has.
                    <p className="text-xs text-muted-foreground">
                      {t("plans.trialFallbackHint", {
                        devices: !supports.activations
                          ? ""
                          : t(
                              supports.seats ? "plans.trialFallbackHintDevicesSeats" : "plans.trialFallbackHintDevices",
                            ),
                        seats: supports.seats ? t("plans.trialFallbackHintSeats") : "",
                      })}
                    </p>
                  )}
                </div>
              )}
              <div className="space-y-2 flex items-center gap-3 pt-5">
                <input
                  type="checkbox"
                  checked={form.active}
                  onChange={(e) => set("active", e.target.checked)}
                  className="h-4 w-4 rounded border-input accent-primary"
                  id="plan-active"
                />
                <Label htmlFor="plan-active">{t("common.active")}</Label>
              </div>
              <div className="space-y-2">
                <Label>{t("plans.stripePriceId")}</Label>
                <Input
                  value={form.stripe_price_id}
                  onChange={(e) => set("stripe_price_id", e.target.value)}
                  placeholder="price_..."
                />
              </div>
              {/* Self sign-up: free plans only. The /pay link becomes a
                form where visitors confirm their email to get a key. */}
              {charges && plan?.self_signup && (
                <p className="text-xs text-amber-600">{t("plans.selfSignupTurnsOff")}</p>
              )}
              {!charges && (
                <div className="space-y-2">
                  <div className="flex items-center gap-3">
                    <input
                      type="checkbox"
                      checked={form.self_signup}
                      onChange={(e) => set("self_signup", e.target.checked)}
                      className="h-4 w-4 rounded border-input accent-primary"
                      id="plan-self-signup"
                    />
                    <Label htmlFor="plan-self-signup">{t("plans.selfSignup")}</Label>
                  </div>
                  <p className="text-xs text-muted-foreground">{t("plans.selfSignupHint")}</p>
                </div>
              )}
              {!charges && form.self_signup && (
                <>
                  <div className="space-y-2">
                    <Label>{t("plans.signupDailyCap")}</Label>
                    <Input
                      type="number"
                      min={0}
                      max={100000}
                      value={form.signup_daily_cap}
                      onChange={(e) => set("signup_daily_cap", Number(e.target.value))}
                    />
                    <p className="text-xs text-muted-foreground">{t("plans.signupDailyCapHint")}</p>
                  </div>
                  <div className="space-y-2">
                    <Label>{t("plans.signupConsentText")}</Label>
                    <Input
                      value={form.signup_consent_text}
                      maxLength={500}
                      onChange={(e) => set("signup_consent_text", e.target.value)}
                    />
                    <p className="text-xs text-muted-foreground">{t("plans.signupConsentTextHint")}</p>
                  </div>
                </>
              )}
              {/* Maintenance period: the license never expires, but only
                releases published before updates_until can be installed.
                The server clears these on non-perpetual plans. */}
              {isPerpetual && (
                <>
                  <div className="space-y-2">
                    <Label>{t("plans.updatesDays")}</Label>
                    <Input
                      type="number"
                      min={0}
                      max={3650}
                      value={form.updates_days}
                      onChange={(e) => set("updates_days", Number(e.target.value))}
                    />
                    <p className="text-xs text-muted-foreground">{t("plans.updatesDaysHint")}</p>
                    {/* The server refuses an update period while the
                      product's feeds are public; say so before saving,
                      with the way to change it. */}
                    {selectedProduct && supports.activations && !selectedProduct.feed_license_required && (
                      <p className="text-xs text-amber-700 dark:text-amber-400">
                        {t("plans.feedNotGated")}{" "}
                        <Link to={`/admin/releases?settings=${selectedProduct.id}&tab=access`} className="underline">
                          {t("plans.feedNotGatedLink")}
                        </Link>
                      </p>
                    )}
                  </div>
                  <div className="space-y-2">
                    <Label>{t("plans.renewalDays")}</Label>
                    <Input
                      type="number"
                      min={0}
                      max={3650}
                      value={form.renewal_days}
                      onChange={(e) => set("renewal_days", Number(e.target.value))}
                    />
                  </div>
                  <div className="space-y-2">
                    <Label>{t("plans.stripeRenewalPriceId")}</Label>
                    <Input
                      value={form.stripe_renewal_price_id}
                      onChange={(e) => set("stripe_renewal_price_id", e.target.value)}
                      placeholder="price_..."
                    />
                    <p className="text-xs text-muted-foreground">{t("plans.renewalHint")}</p>
                  </div>
                </>
              )}
            </div>
          </DialogBody>
          <DialogFooter>
            <Button type="button" variant="outline" onClick={onClose}>
              {t("common.cancel")}
            </Button>
            <Button type="submit" disabled={loading}>
              {loading ? t("common.loading") : t("common.save")}
            </Button>
          </DialogFooter>
        </form>

        {/* Entitlements section (only when editing) */}
        {plan && (
          <>
            <Separator />
            <EntitlementSection planId={plan.id} entitlements={plan.entitlements || []} />
          </>
        )}
      </DialogContent>
    </Dialog>
  )
}

function EntitlementSection({ planId, entitlements: initial }: { planId: string; entitlements: Entitlement[] }) {
  const { t } = useI18n()
  const qc = useQueryClient()
  const [entitlements, setEntitlements] = useState(initial)
  const [adding, setAdding] = useState(false)
  const [newEnt, setNewEnt] = useState({
    feature: "",
    value_type: "bool",
    value: "true",
    quota_period: "",
    quota_unit: "",
    stripe_meter_event_name: "",
  })
  // confirmDelete shows the AlertDialog with the feature name before
  // the irreversible delete fires. The button used to call
  // deleteMut.mutate(e.id) directly — a stray click wiped a plan
  // entitlement with no recourse.
  const [confirmDelete, setConfirmDelete] = useState<{ id: string; feature: string } | null>(null)

  useEffect(() => {
    setEntitlements(initial)
  }, [initial])

  const createMut = useMutation({
    mutationFn: () =>
      admin.createEntitlement({
        plan_id: planId,
        feature: newEnt.feature,
        value_type: newEnt.value_type,
        value: newEnt.value,
        ...(newEnt.value_type === "quota"
          ? {
              quota_period: newEnt.quota_period,
              quota_unit: newEnt.quota_unit,
              // Empty string means "no Stripe metered binding" —
              // safe to always pass, server treats "" identically.
              stripe_meter_event_name: newEnt.stripe_meter_event_name,
            }
          : {}),
      } as {
        plan_id: string
        feature: string
        value_type: string
        value: string
      }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["admin", "plans"] })
      setAdding(false)
      setNewEnt({
        feature: "",
        value_type: "bool",
        value: "true",
        quota_period: "",
        quota_unit: "",
        stripe_meter_event_name: "",
      })
    },
    onError: (e: Error) => showToast(e.message, "error"),
  })

  const deleteMut = useMutation({
    mutationFn: (id: string) => admin.deleteEntitlement(id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["admin", "plans"] })
      setConfirmDelete(null)
    },
    onError: (e: Error) => showToast(e.message, "error"),
  })

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <h4 className="text-sm font-semibold">{t("plans.entitlements")}</h4>
        <Button type="button" variant="outline" size="sm" onClick={() => setAdding(!adding)}>
          <Plus className="h-3 w-3 mr-1" /> {t("common.create")}
        </Button>
      </div>
      {entitlements.length > 0 && (
        <div className="space-y-2">
          {entitlements.map((e) => (
            <div key={e.id} className="flex items-center justify-between bg-muted/50 rounded px-3 py-2 text-sm">
              <div>
                <span className="font-medium">{e.feature}</span>
                <span className="text-muted-foreground ml-2">
                  ({e.value_type}: {e.value})
                </span>
                {e.quota_period && (
                  <span className="text-muted-foreground ml-1">
                    / {e.quota_period}
                    {e.quota_unit ? ` (${e.quota_unit})` : ""}
                  </span>
                )}
              </div>
              <Button
                type="button"
                variant="ghost"
                size="icon"
                className="h-7 w-7"
                onClick={() => setConfirmDelete({ id: e.id, feature: e.feature })}
              >
                <Trash2 className="h-3 w-3 text-destructive" />
              </Button>
            </div>
          ))}
        </div>
      )}
      {adding && (
        <div className="border rounded p-3 space-y-3">
          <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
            <div className="space-y-2">
              <Label className="text-xs">{t("plans.feature")}</Label>
              <Input
                value={newEnt.feature}
                onChange={(e) => setNewEnt((n) => ({ ...n, feature: e.target.value }))}
                placeholder="e.g. api_calls"
              />
            </div>
            <div className="space-y-2">
              <Label className="text-xs">{t("plans.valueType")}</Label>
              <Select value={newEnt.value_type} onValueChange={(v) => setNewEnt((n) => ({ ...n, value_type: v }))}>
                <SelectTrigger>
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="bool">{t("plans.boolean")}</SelectItem>
                  <SelectItem value="int">{t("plans.integer")}</SelectItem>
                  <SelectItem value="string">{t("plans.string")}</SelectItem>
                  <SelectItem value="quota">{t("plans.quota")}</SelectItem>
                </SelectContent>
              </Select>
            </div>
            <div className="space-y-2">
              <Label className="text-xs">{t("plans.value")}</Label>
              <Input
                value={newEnt.value}
                onChange={(e) => setNewEnt((n) => ({ ...n, value: e.target.value }))}
                placeholder="e.g. true, 100"
              />
            </div>
            {newEnt.value_type === "quota" && (
              <>
                <div className="space-y-2">
                  <Label className="text-xs">{t("plans.quotaPeriod")}</Label>
                  <Select
                    value={newEnt.quota_period}
                    onValueChange={(v) => setNewEnt((n) => ({ ...n, quota_period: v }))}
                  >
                    <SelectTrigger>
                      <SelectValue placeholder="Select period" />
                    </SelectTrigger>
                    <SelectContent>
                      <SelectItem value="hourly">{t("plans.hourly")}</SelectItem>
                      <SelectItem value="daily">{t("plans.daily")}</SelectItem>
                      <SelectItem value="monthly">{t("plans.monthly")}</SelectItem>
                      <SelectItem value="yearly">{t("plans.yearly")}</SelectItem>
                    </SelectContent>
                  </Select>
                </div>
                <div className="space-y-2">
                  <Label className="text-xs">{t("plans.quotaUnit")}</Label>
                  <Input
                    value={newEnt.quota_unit}
                    onChange={(e) => setNewEnt((n) => ({ ...n, quota_unit: e.target.value }))}
                    placeholder="e.g. requests, tokens"
                  />
                </div>
                {/* Optional Stripe Billing Meter binding. When non-empty,
                    every RecordUsage call for this feature enqueues a
                    meter event the background sync pushes to Stripe.
                    Configure the meter (and event_name) in your Stripe
                    dashboard first. */}
                <div className="space-y-2 sm:col-span-2">
                  <Label className="text-xs">{t("plans.stripeMeterEventName")}</Label>
                  <Input
                    value={newEnt.stripe_meter_event_name}
                    onChange={(e) => setNewEnt((n) => ({ ...n, stripe_meter_event_name: e.target.value }))}
                    placeholder="api_calls (configured in Stripe dashboard)"
                  />
                  <p className="text-[10px] text-muted-foreground">{t("plans.stripeMeterEventNameHelp")}</p>
                </div>
              </>
            )}
          </div>
          <div className="flex justify-end gap-2">
            <Button type="button" variant="outline" size="sm" onClick={() => setAdding(false)}>
              {t("common.cancel")}
            </Button>
            <Button
              type="button"
              size="sm"
              disabled={createMut.isPending || !newEnt.feature}
              onClick={() => createMut.mutate()}
            >
              {createMut.isPending ? t("common.loading") : t("plans.addEntitlement")}
            </Button>
          </div>
        </div>
      )}

      <AlertDialog open={!!confirmDelete} onOpenChange={() => setConfirmDelete(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>
              {t("plans.deleteEntitlementTitle", { feature: confirmDelete?.feature || "" })}
            </AlertDialogTitle>
            <AlertDialogDescription>{t("plans.deleteEntitlementDesc")}</AlertDialogDescription>
          </AlertDialogHeader>
          <div className="flex justify-end gap-2">
            <AlertDialogCancel>{t("common.cancel")}</AlertDialogCancel>
            <AlertDialogAction
              className="bg-destructive text-white hover:bg-destructive/90"
              onClick={() => confirmDelete && deleteMut.mutate(confirmDelete.id)}
            >
              {t("common.delete")}
            </AlertDialogAction>
          </div>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  )
}
