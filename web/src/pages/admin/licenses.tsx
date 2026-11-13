import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import {
  Ban,
  Check,
  Copy,
  Eye,
  Mail,
  Package,
  Pause,
  Pencil,
  Play,
  Plus,
  RefreshCw,
  RotateCw,
  Search,
  Trash2,
  Unlink,
} from "lucide-react"
import { useRef, useState } from "react"
import { Link } from "react-router-dom"
import { CopyableId } from "@/components/copyable-id"
import { PlanSelect } from "@/components/plan-select"
import { ProductSelect } from "@/components/product-select"
import { showToast } from "@/components/toast"
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogHeader,
  AlertDialogTitle,
  AlertDialogTrigger,
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
  DataTableSortHead,
  useServerPagination,
  useServerSort,
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
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { type TranslationKeys, useI18n } from "@/i18n"
import { type AdminSeat, admin, errorCode, errorDetails, type License, type Plan, type UsageCounter } from "@/lib/api"
import { formatDate, maskKey, statusColor } from "@/lib/utils"

// A date picked in the expiry field means "valid through that whole
// day" in the admin's local timezone, so the license dies at local
// 23:59:59 rather than end-of-day UTC (which renders as an odd
// mid-evening time for anyone west of Greenwich).
function endOfDayISO(date: string): string {
  const [y, m, d] = date.split("-").map(Number)
  return new Date(y, m - 1, d, 23, 59, 59).toISOString()
}

// Inverse of endOfDayISO for prefilling the date input: the stored
// instant rendered as a local YYYY-MM-DD (slicing the UTC string
// would land on the next day for anyone west of Greenwich).
function localDateValue(iso: string): string {
  const d = new Date(iso)
  const pad = (n: number) => String(n).padStart(2, "0")
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`
}

export default function LicensesPage() {
  const { t, locale } = useI18n()
  const qc = useQueryClient()
  const [productFilter, setProductFilter] = useState<string>("")
  const [statusFilter, setStatusFilter] = useState<string>("")
  const [search, setSearch] = useState("")
  // Reordering reshuffles every page, so the sort state joins the
  // filters that send the pager back to page one.
  const srt = useServerSort("created_at", "desc")
  const pg = useServerPagination(20, [productFilter, statusFilter, search, srt.sort, srt.order])

  const { data: productsData } = useQuery({
    // One row is all this page needs from the catalogue: whether the
    // install has any product at all, and which one a new form starts
    // on. The pickers fetch their own candidates, with search.
    queryKey: ["admin", "products", "newest"],
    queryFn: () => admin.listProducts({ limit: 1 }),
  })
  const { data, isLoading } = useQuery({
    queryKey: ["admin", "licenses", productFilter, statusFilter, search, srt.sort, srt.order, pg.page, pg.pageSize],
    queryFn: () =>
      admin.listLicenses({
        product_id: productFilter || undefined,
        status: statusFilter || undefined,
        search: search || undefined,
        ...srt.params,
        ...pg.params,
      }),
  })

  const [creating, setCreating] = useState(false)
  const [viewing, setViewing] = useState<string | null>(null)

  const products = productsData?.products || []
  const { items: licenses, total, totalPages } = pg.from(data, data?.licenses)

  // Set as the form is submitted, before the mutation is pending, so a
  // double click issues one license, not two.
  const issuing = useRef(false)
  const createMut = useMutation({
    mutationFn: admin.createLicense,
    onSuccess: () => {
      // The dashboard's totals and the product's counts change too.
      qc.invalidateQueries({ queryKey: ["admin"] })
      setCreating(false)
      showToast(t("toast.licenseCreated"), "success")
    },
    onError: (e: Error) => showToast(e.message, "error"),
    onSettled: () => {
      issuing.current = false
    },
  })

  if (!isLoading && products.length === 0) {
    return (
      <div className="space-y-6">
        <div>
          <h1 className="text-2xl font-bold tracking-tight sr-only md:not-sr-only">{t("licenses.title")}</h1>
          <p className="text-muted-foreground">Manage software licenses.</p>
        </div>
        <Card>
          <CardContent className="py-12 text-center">
            <Package className="h-12 w-12 mx-auto text-muted-foreground mb-4" />
            <p className="text-lg font-medium">{t("licenses.noProducts")}</p>
            <p className="text-muted-foreground mt-1 mb-4">{t("licenses.noProductsDesc")}</p>
            <Button asChild>
              <Link to="/admin/products">
                <Plus className="h-4 w-4 mr-2" /> Create Product
              </Link>
            </Button>
          </CardContent>
        </Card>
      </div>
    )
  }

  return (
    <div className="space-y-6">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div className="min-w-0">
          <h1 className="text-2xl font-bold tracking-tight sr-only md:not-sr-only">{t("licenses.title")}</h1>
          <p className="text-muted-foreground">
            {t(total === 1 ? "licenses.totalOne" : "licenses.totalMany", { count: total.toLocaleString(locale) })}
          </p>
        </div>
        <Button onClick={() => setCreating(true)}>
          <Plus className="h-4 w-4 mr-2" /> {t("licenses.issue")}
        </Button>
      </div>

      {/* Three controls do not fit side by side on a phone, so they
        wrap, and the search box takes the whole first row rather than
        being squeezed to a few characters. */}
      <div className="flex flex-wrap items-center gap-3">
        <div className="relative w-full sm:w-auto sm:flex-1 sm:max-w-sm">
          <Search className="absolute left-3 top-1/2 -translate-y-1/2 h-4 w-4 text-muted-foreground" />
          <Input
            placeholder={t("licenses.searchPlaceholder")}
            value={search}
            onChange={(e) => setSearch(e.target.value)}
            className="pl-9"
          />
        </div>
        {/* The two dropdowns share one row on a phone rather than
          taking one each: they fit side by side, and the search box
          above them is the control that needs the full width. */}
        <div className="flex w-full gap-3 sm:w-auto">
          <ProductSelect
            value={productFilter}
            onChange={setProductFilter}
            allLabel={t("filter.allProducts")}
            className="flex-1 sm:w-48 sm:flex-none"
          />
          <Select value={statusFilter} onValueChange={(v) => setStatusFilter(v === "all" ? "" : v)}>
            <SelectTrigger className="flex-1 sm:w-40 sm:flex-none">
              <SelectValue placeholder={t("filter.allStatuses")} />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="all">{t("filter.allStatuses")}</SelectItem>
              {["active", "trialing", "past_due", "canceled", "expired", "suspended", "revoked"].map((s) => (
                <SelectItem key={s} value={s}>
                  {t(`status.${s}` as TranslationKeys)}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </div>
      </div>

      <Card>
        <CardContent className="pt-6">
          {isLoading ? (
            <div className="h-64 animate-pulse bg-muted rounded-lg" />
          ) : (
            <>
              <DataTable>
                <DataTableHeader>
                  <DataTableRow>
                    <DataTableSortHead sort={srt} column="email">
                      {t("common.email")}
                    </DataTableSortHead>
                    <DataTableHead>{t("licenses.licenseKey")}</DataTableHead>
                    <DataTableSortHead sort={srt} column="product">
                      {t("common.product")}
                    </DataTableSortHead>
                    <DataTableSortHead sort={srt} column="plan">
                      {t("common.plan")}
                    </DataTableSortHead>
                    <DataTableHead>{t("licenses.activations")}</DataTableHead>
                    <DataTableSortHead sort={srt} column="status">
                      {t("common.status")}
                    </DataTableSortHead>
                    <DataTableSortHead sort={srt} column="valid_until" firstOrder="asc">
                      {t("licenses.validUntil")}
                    </DataTableSortHead>
                    <DataTableSortHead sort={srt} column="created_at" firstOrder="desc">
                      {t("common.created")}
                    </DataTableSortHead>
                    <DataTableHead className="w-16 text-right">{t("common.actions")}</DataTableHead>
                  </DataTableRow>
                </DataTableHeader>
                <DataTableBody>
                  {licenses.length === 0 && <DataTableEmpty colSpan={9} message={t("licenses.empty")} />}
                  {licenses.map((lic) => (
                    <DataTableRow key={lic.id}>
                      <DataTableCell className="font-medium">{lic.email}</DataTableCell>
                      <DataTableCell>
                        <code className="text-xs bg-muted px-1.5 py-0.5 rounded whitespace-nowrap">
                          {maskKey(data?.license_key_hints?.[lic.id], data?.license_key_prefixes?.[lic.id])}
                        </code>
                      </DataTableCell>
                      <DataTableCell className="text-muted-foreground">{lic.product?.name || "-"}</DataTableCell>
                      <DataTableCell className="text-muted-foreground">{lic.plan?.name || "-"}</DataTableCell>
                      {/* What "in use" counts depends on the plan. A
                        floating plan holds its seats in its own table,
                        so reading activations there reports an empty
                        licence however full it is. SaaS is seat-based
                        and has neither, so it gets nothing rather than
                        a number that never moves. */}
                      <DataTableCell className="text-muted-foreground text-xs whitespace-nowrap">
                        {lic.product?.type === "saas"
                          ? "-"
                          : `${
                              lic.plan?.license_model === "floating"
                                ? (lic.active_session_count ?? 0)
                                : (lic.activation_count ?? 0)
                            } / ${(lic.activation_limit ?? lic.plan?.max_activations) || "-"}`}
                      </DataTableCell>
                      <DataTableCell>
                        <Badge className={statusColor(lic.status)}>
                          {t(`status.${lic.status}` as TranslationKeys)}
                        </Badge>
                      </DataTableCell>
                      <DataTableCell className="text-muted-foreground text-xs">
                        {lic.valid_until ? formatDate(lic.valid_until) : t("licenses.perpetual")}
                      </DataTableCell>
                      <DataTableCell className="text-muted-foreground text-xs">
                        {formatDate(lic.created_at)}
                      </DataTableCell>
                      <DataTableCell className="text-right">
                        <Button
                          variant="ghost"
                          size="icon"
                          title={t("licenses.detail")}
                          aria-label={t("licenses.detail")}
                          onClick={() => setViewing(lic.id)}
                        >
                          <Eye className="h-4 w-4" />
                        </Button>
                      </DataTableCell>
                    </DataTableRow>
                  ))}
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

      {/* Create */}
      {creating && (
        <CreateLicenseDialog
          open
          onClose={() => setCreating(false)}
          products={products}
          onSubmit={(d) => {
            if (issuing.current) return
            issuing.current = true
            createMut.mutate(d)
          }}
          loading={createMut.isPending}
        />
      )}

      {/* Detail */}
      {viewing && <LicenseDetail id={viewing} onClose={() => setViewing(null)} />}
    </div>
  )
}

function CreateLicenseDialog({
  open,
  onClose,
  products,
  onSubmit,
  loading,
}: {
  open: boolean
  onClose: () => void
  products: { id: string; name: string; type?: string }[]
  onSubmit: (d: {
    product_id: string
    plan_id: string
    email: string
    notes?: string
    external_customer_id?: string
    external_workspace_id?: string
    valid_until?: string
  }) => void
  loading: boolean
}) {
  const { t } = useI18n()
  const [productId, setProductId] = useState(products[0]?.id || "")
  const [email, setEmail] = useState("")
  const [notes, setNotes] = useState("")
  const [planId, setPlanId] = useState("")
  const [externalCustomerID, setExternalCustomerID] = useState("")
  const [externalWorkspaceID, setExternalWorkspaceID] = useState("")
  const [validUntil, setValidUntil] = useState("")

  // Show a hint right under the product dropdown so admin knows what
  // they're committing to — saas licenses don't get device activation,
  // desktop licenses don't get seats, etc. Mirrors the backend
  // capability gate so admins can predict downstream behavior.
  // Same reason as the plan form: the picked product may be outside
  // the page the picker loaded, and the hint below (and the capability
  // it implies) has to describe the product actually chosen.
  const { data: selectedProduct } = useQuery({
    queryKey: ["admin", "product", productId],
    queryFn: () => admin.getProduct(productId),
    enabled: !!productId,
  })
  const productType = selectedProduct?.type
  const typeHintKey =
    productType === "desktop"
      ? "products.desktopDesc"
      : productType === "saas"
        ? "products.saasDesc"
        : productType === "hybrid"
          ? "products.hybridDesc"
          : null

  return (
    <Dialog open={open} onOpenChange={onClose}>
      <DialogContent className="h-[min(680px,85vh)]">
        <DialogHeader>
          <DialogTitle>{t("licenses.issue")}</DialogTitle>
          <DialogDescription>{t("licenses.issueDesc")}</DialogDescription>
        </DialogHeader>
        <form
          onSubmit={(e) => {
            e.preventDefault()
            onSubmit({
              product_id: productId,
              plan_id: planId,
              email,
              notes,
              external_customer_id: externalCustomerID.trim() || undefined,
              external_workspace_id: externalWorkspaceID.trim() || undefined,
              valid_until: validUntil ? endOfDayISO(validUntil) : undefined,
            })
          }}
          className="flex min-h-0 flex-1 flex-col gap-4"
        >
          <DialogBody className="space-y-4">
            <div className="space-y-2">
              <Label>{t("common.product")}</Label>
              <ProductSelect
                value={productId}
                onChange={(v) => {
                  setProductId(v)
                  setPlanId("")
                }}
                className="w-full"
                withType
              />
              {typeHintKey && <p className="text-xs text-muted-foreground">{t(typeHintKey)}</p>}
            </div>
            <div className="space-y-2">
              <Label>{t("common.plan")}</Label>
              <PlanSelect
                productId={productId}
                value={planId}
                onChange={setPlanId}
                placeholder={t("licenses.selectPlan")}
                className="w-full"
                withType
              />
            </div>
            <div className="space-y-2">
              <Label>{t("common.email")}</Label>
              <Input type="email" value={email} onChange={(e) => setEmail(e.target.value)} required />
            </div>
            <div className="space-y-2">
              <Label>{t("licenses.notesOptional")}</Label>
              <Input value={notes} onChange={(e) => setNotes(e.target.value)} />
            </div>
            <div className="space-y-2">
              <Label>{t("licenses.validUntilOptional")}</Label>
              <Input
                type="date"
                value={validUntil}
                min={localDateValue(new Date().toISOString())}
                onChange={(e) => setValidUntil(e.target.value)}
              />
              <p className="text-xs text-muted-foreground">{t("licenses.validUntilHint")}</p>
            </div>
            {/* External identifiers — opaque strings the merchant uses
              to map their own user/workspace model to this license.
              Both optional; leave blank if not integrating with an
              external system. */}
            <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
              <div className="space-y-2">
                <Label>{t("licenses.externalCustomerID")}</Label>
                <Input
                  value={externalCustomerID}
                  onChange={(e) => setExternalCustomerID(e.target.value)}
                  placeholder={t("licenses.externalIDPlaceholder")}
                  maxLength={256}
                />
              </div>
              <div className="space-y-2">
                <Label>{t("licenses.externalWorkspaceID")}</Label>
                <Input
                  value={externalWorkspaceID}
                  onChange={(e) => setExternalWorkspaceID(e.target.value)}
                  placeholder={t("licenses.externalIDPlaceholder")}
                  maxLength={256}
                />
              </div>
            </div>
          </DialogBody>
          <DialogFooter>
            <Button type="button" variant="outline" onClick={onClose}>
              {t("common.cancel")}
            </Button>
            <Button type="submit" disabled={loading || !planId}>
              {loading ? t("common.loading") : t("licenses.issue")}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

function LicenseDetail({ id, onClose }: { id: string; onClose: () => void }) {
  const { t } = useI18n()
  const qc = useQueryClient()
  const { data: lic, isLoading } = useQuery({
    queryKey: ["admin", "license", id],
    queryFn: () => admin.getLicense(id),
  })
  const [copied, setCopied] = useState(false)
  const [changingPlan, setChangingPlan] = useState(false)
  // null = not editing; "" = editing with empty value (perpetual)
  const [editingValidUntil, setEditingValidUntil] = useState<string | null>(null)

  const revokeMut = useMutation({
    mutationFn: () => admin.revokeLicense(id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["admin"] })
    },
    onError: (e: Error) => showToast(e.message, "error"),
  })
  const resendMut = useMutation({
    mutationFn: () => admin.resendLicenseEmail(id),
    // Queued, not delivered: the mail goes into the retry queue and
    // leaves over SMTP a moment later, so the toast says it is on its
    // way rather than claiming it has arrived.
    onSuccess: (r) => showToast(t("toast.licenseEmailQueued", { email: r.email }), "success"),
    onError: (e: Error) => showToast(e.message, "error"),
  })
  const suspendMut = useMutation({
    mutationFn: () => admin.suspendLicense(id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["admin"] })
    },
    onError: (e: Error) => showToast(e.message, "error"),
  })
  // Unlinking is a deliberate act with Stripe's answer behind it: the
  // server asks Stripe whether the subscription is really over and
  // refuses while it is not, so this button cannot quietly detach a
  // licence that is still being billed.
  const [confirmUnlink, setConfirmUnlink] = useState(false)
  const unlinkMut = useMutation({
    mutationFn: () => admin.unlinkStripeSubscription(id),
    onSuccess: (r) => {
      qc.invalidateQueries({ queryKey: ["admin"] })
      setConfirmUnlink(false)
      showToast(r.status === "unlinked" ? t("licenses.unlinkDone") : t("licenses.unlinkNotLinked"), "success")
    },
    onError: (e: Error) => showToast(e.message, "error"),
  })
  const reinstateMut = useMutation({
    mutationFn: () => admin.reinstateLicense(id),
    onSuccess: (res) => {
      qc.invalidateQueries({ queryKey: ["admin"] })
      // Reinstated, but not usable: its expiry ran out while it was
      // suspended, or its Stripe subscription, which it now follows, is
      // paused, ended or unpaid.
      if (res && res.usable === false) {
        if (!lic?.stripe_subscription_id) showToast(t("licenses.reinstatedLapsed"))
        else if (res.status === "suspended") showToast(t("licenses.reinstatedStripePaused"))
        else if (res.status === "canceled") showToast(t("licenses.reinstatedStripeEnded"))
        else showToast(t("licenses.reinstatedStripeUnpaid"))
      }
    },
    onError: (e: Error) => showToast(e.message, "error"),
  })
  const refundMut = useMutation({
    mutationFn: () => admin.refundLicense(id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["admin"] })
    },
    onError: (e: Error) => showToast(e.message, "error"),
  })
  const validUntilMut = useMutation({
    mutationFn: (validUntil: string) => admin.setLicenseValidUntil(id, validUntil),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["admin"] })
      setEditingValidUntil(null)
    },
    onError: (e: Error) => showToast(e.message, "error"),
  })
  const [editingUpdatesUntil, setEditingUpdatesUntil] = useState<string | null>(null)
  const updatesUntilMut = useMutation({
    mutationFn: (updatesUntil: string) => admin.setLicenseUpdatesUntil(id, updatesUntil),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["admin"] })
      setEditingUpdatesUntil(null)
    },
    onError: (e: Error) =>
      showToast(
        errorCode(e) === "MAINTENANCE_FEATURES_DISABLED" ? t("licenses.maintenanceDisabled") : e.message,
        "error",
      ),
  })
  // Activation deletion is destructive — wrap in a confirmation
  // state so a stray ghost-click on the trash icon (icons sit
  // close together in the row) doesn't immediately revoke a device.
  const [confirmDeactivation, setConfirmDeactivation] = useState<{ id: string; label: string } | null>(null)
  const deleteActMut = useMutation({
    mutationFn: admin.deleteActivation,
    onSuccess: () => {
      // The list behind the dialog counts devices too.
      qc.invalidateQueries({ queryKey: ["admin"] })
      setConfirmDeactivation(null)
    },
    onError: (e: Error) => showToast(e.message, "error"),
  })

  // The key is not in the detail payload — it is fetched per click and
  // the server audits each reveal. Keep it in component state only, so
  // it lives no longer than the open dialog.
  const [revealed, setRevealedKey] = useState<string | null>(null)
  // A key whose hint is no longer the licence's (reissued meanwhile) is
  // not shown or copied.
  const revealedKey = revealed && (!lic?.license_key_hint || revealed.endsWith(lic.license_key_hint)) ? revealed : null
  const revealMut = useMutation({
    mutationFn: () => admin.revealLicenseKey(id),
    onSuccess: (r) => setRevealedKey(r.license_key),
    onError: (e: Error) => showToast(e.message, "error"),
  })
  // A new key for the licence: the old one and the devices it activated
  // stop; members keep their own keys. The new key is shown at once, as
  // a reveal would.
  const [confirmRotate, setConfirmRotate] = useState(false)
  // Set as the confirmation is pressed, before the mutation is pending,
  // so a double click cannot reissue twice.
  const rotating = useRef(false)
  const rotateMut = useMutation({
    mutationFn: () => admin.rotateLicenseKey(id),
    meta: { ownErrors: true },
    onSuccess: (r) => {
      setRevealedKey(r.license_key)
      showToast(t("licenses.rotateKeyDone"), "success")
      qc.invalidateQueries({ queryKey: ["admin", "license", id] })
      qc.invalidateQueries({ queryKey: ["admin", "licenses"] })
    },
    onError: (e: Error) => showToast(e.message, "error"),
    onSettled: () => {
      rotating.current = false
      setConfirmRotate(false)
    },
  })
  const copyKey = () => {
    const write = (key: string) => {
      navigator.clipboard.writeText(key)
      setCopied(true)
      setTimeout(() => setCopied(false), 2000)
    }
    if (revealedKey) {
      write(revealedKey)
      return
    }
    revealMut.mutate(undefined, { onSuccess: (r) => write(r.license_key) })
  }

  return (
    <Dialog open onOpenChange={onClose}>
      {/* One frame for all three tabs. Sizing to the open tab made the
          overlay jump between Info (long) and Usage/Seats (short), and
          put the scrollbar on the whole dialog including its header. */}
      <DialogContent className="flex h-[85vh] max-w-2xl flex-col overflow-hidden">
        <DialogHeader>
          <DialogTitle>{t("licenses.detail")}</DialogTitle>
          <DialogDescription>{lic?.email}</DialogDescription>
          {lic && <CopyableId id={lic.id} />}
        </DialogHeader>
        {isLoading || !lic ? (
          <div className="h-48 animate-pulse bg-muted rounded-lg" />
        ) : (
          <Tabs defaultValue="info" className="flex min-h-0 flex-1 flex-col">
            <TabsList className="shrink-0">
              <TabsTrigger value="info">{t("licenses.info")}</TabsTrigger>
              <TabsTrigger value="usage">{t("licenses.usage")}</TabsTrigger>
              <TabsTrigger value="seats">{t("licenses.seats")}</TabsTrigger>
            </TabsList>

            <TabsContent value="info" className="-mr-6 min-h-0 flex-1 overflow-y-auto pr-6">
              <div className="space-y-6">
                {/* Info */}
                <div className="grid grid-cols-1 gap-4 text-sm sm:grid-cols-2">
                  <div>
                    <p className="text-muted-foreground">{t("licenses.licenseKey")}</p>
                    <div className="flex items-center gap-2 mt-1">
                      <code className="bg-muted px-2 py-1 rounded text-xs">
                        {revealedKey ?? maskKey(lic.license_key_hint, lic.license_key_prefix)}
                      </code>
                      {!revealedKey && (
                        <Button
                          variant="ghost"
                          size="icon"
                          className="h-6 w-6"
                          title={t("licenses.revealKey")}
                          disabled={revealMut.isPending}
                          onClick={() => revealMut.mutate()}
                        >
                          <Eye className="h-3 w-3" />
                        </Button>
                      )}
                      <Button variant="ghost" size="icon" className="h-6 w-6" onClick={copyKey}>
                        {copied ? <Check className="h-3 w-3 text-emerald-600" /> : <Copy className="h-3 w-3" />}
                      </Button>
                      <Button
                        variant="ghost"
                        size="icon"
                        className="h-6 w-6"
                        title={t("licenses.rotateKey")}
                        aria-label={t("licenses.rotateKey")}
                        disabled={rotateMut.isPending}
                        onClick={() => setConfirmRotate(true)}
                      >
                        <RotateCw className="h-3 w-3" />
                      </Button>
                    </div>
                  </div>
                  <div>
                    <p className="text-muted-foreground">{t("common.status")}</p>
                    <Badge className={`mt-1 ${statusColor(lic.status)}`}>
                      {t(`status.${lic.status}` as TranslationKeys)}
                    </Badge>
                  </div>
                  <div>
                    <p className="text-muted-foreground">{t("common.product")}</p>
                    <p className="mt-1 font-medium">{lic.product?.name || "-"}</p>
                  </div>
                  <div>
                    <p className="text-muted-foreground">{t("common.plan")}</p>
                    <p className="mt-1 font-medium">{lic.plan?.name || "-"}</p>
                  </div>
                  {lic.plan?.per_seat && <QuantityField license={lic} />}
                  <div>
                    <p className="text-muted-foreground">{t("licenses.validFrom")}</p>
                    <p className="mt-1">{formatDate(lic.valid_from)}</p>
                  </div>
                  <div>
                    <p className="text-muted-foreground">{t("licenses.validUntil")}</p>
                    {editingValidUntil === null ? (
                      <div className="flex items-center gap-1 mt-1">
                        <p>{lic.valid_until ? formatDate(lic.valid_until) : t("licenses.perpetual")}</p>
                        {/* A subscription owns the expiry of the licence it
                          bills: editing it here would be overwritten on the
                          next renewal. A one-time purchase renews nothing,
                          and a licence unlinked from a finished subscription
                          has nobody left to own it. */}
                        {!lic.stripe_subscription_id && (
                          <Button
                            variant="ghost"
                            size="icon"
                            className="h-6 w-6"
                            title={t("licenses.validUntilEdit")}
                            onClick={() => setEditingValidUntil(lic.valid_until ? localDateValue(lic.valid_until) : "")}
                          >
                            <Pencil className="h-3 w-3" />
                          </Button>
                        )}
                      </div>
                    ) : (
                      <div className="mt-1 space-y-1">
                        <div className="flex items-center gap-1">
                          <Input
                            type="date"
                            className="h-7 w-40 text-xs"
                            value={editingValidUntil}
                            onChange={(e) => setEditingValidUntil(e.target.value)}
                          />
                          <Button
                            size="sm"
                            className="h-7"
                            disabled={validUntilMut.isPending}
                            onClick={() =>
                              validUntilMut.mutate(editingValidUntil ? endOfDayISO(editingValidUntil) : "")
                            }
                          >
                            {t("common.save")}
                          </Button>
                          <Button size="sm" variant="ghost" className="h-7" onClick={() => setEditingValidUntil(null)}>
                            {t("common.cancel")}
                          </Button>
                        </div>
                        <p className="text-xs text-muted-foreground">{t("licenses.validUntilClear")}</p>
                        {/* Saving a future date (or none) on an expired
                          license reactivates it — say so before the admin
                          saves, so the status change is not a surprise. */}
                        {lic.status === "expired" && (
                          <p className="text-xs text-amber-600">{t("licenses.validUntilExpiredHint")}</p>
                        )}
                      </div>
                    )}
                  </div>
                  {/* Maintenance period, perpetual plans only. Not owned by
                    Stripe: a paid renewal extends from whatever is set here. */}
                  {lic.plan?.license_type === "perpetual" && (
                    <div>
                      <p className="text-muted-foreground">{t("licenses.updatesUntil")}</p>
                      {editingUpdatesUntil === null ? (
                        <div className="flex items-center gap-1 mt-1">
                          <p>{lic.updates_until ? formatDate(lic.updates_until) : t("portal.updatesForLife")}</p>
                          <Button
                            variant="ghost"
                            size="icon"
                            className="h-6 w-6"
                            title={t("licenses.updatesUntilEdit")}
                            onClick={() =>
                              setEditingUpdatesUntil(lic.updates_until ? localDateValue(lic.updates_until) : "")
                            }
                          >
                            <Pencil className="h-3 w-3" />
                          </Button>
                        </div>
                      ) : (
                        <div className="mt-1 space-y-1">
                          <div className="flex items-center gap-1">
                            <Input
                              type="date"
                              className="h-7 w-40 text-xs"
                              value={editingUpdatesUntil}
                              onChange={(e) => setEditingUpdatesUntil(e.target.value)}
                            />
                            <Button
                              size="sm"
                              className="h-7"
                              disabled={updatesUntilMut.isPending}
                              onClick={() =>
                                updatesUntilMut.mutate(editingUpdatesUntil ? endOfDayISO(editingUpdatesUntil) : "")
                              }
                            >
                              {t("common.save")}
                            </Button>
                            <Button
                              size="sm"
                              variant="ghost"
                              className="h-7"
                              onClick={() => setEditingUpdatesUntil(null)}
                            >
                              {t("common.cancel")}
                            </Button>
                          </div>
                          <p className="text-xs text-muted-foreground">{t("licenses.updatesUntilClear")}</p>
                        </div>
                      )}
                    </div>
                  )}
                  {lic.payment_provider && (
                    <div>
                      <p className="text-muted-foreground">{t("licenses.payment")}</p>
                      <Badge variant="secondary" className="mt-1">
                        {lic.payment_provider}
                      </Badge>
                    </div>
                  )}
                  {/* The link, not the provider, is what decides: a
                      licence can carry a subscription id with no
                      payment_provider recorded, and it is the link that
                      holds the plan and the expiry hostage. */}
                  {lic.stripe_subscription_id && (
                    <div>
                      <p className="text-muted-foreground">{t("licenses.subscription")}</p>
                      <div className="mt-1 flex items-center gap-2">
                        <code className="max-w-[18ch] truncate text-xs" title={lic.stripe_subscription_id}>
                          {lic.stripe_subscription_id}
                        </code>
                        <Button
                          variant="ghost"
                          size="sm"
                          className="h-6 px-2 text-xs"
                          onClick={() => setConfirmUnlink(true)}
                        >
                          <Unlink className="mr-1 h-3 w-3" />
                          {t("licenses.unlink")}
                        </Button>
                      </div>
                    </div>
                  )}
                  <div>
                    <p className="text-muted-foreground">{t("common.created")}</p>
                    <p className="mt-1">{formatDate(lic.created_at)}</p>
                  </div>
                </div>
                {lic.notes && (
                  <div className="text-sm">
                    <p className="text-muted-foreground">{t("licenses.notes")}</p>
                    <p className="mt-1">{lic.notes}</p>
                  </div>
                )}
                {lic.signup_source === "self_signup" && (
                  <div className="grid grid-cols-1 gap-4 text-sm sm:grid-cols-2">
                    <div>
                      <p className="text-muted-foreground">{t("licenses.source")}</p>
                      <p className="mt-1">{t("licenses.sourceSelfSignup")}</p>
                    </div>
                    <div>
                      <p className="text-muted-foreground">{t("licenses.marketingConsent")}</p>
                      <p className="mt-1">
                        {lic.marketing_consent && lic.marketing_consent_at
                          ? t("licenses.marketingConsentYes", { date: formatDate(lic.marketing_consent_at) })
                          : t("common.no")}
                      </p>
                    </div>
                  </div>
                )}
                {(lic.external_customer_id || lic.external_workspace_id) && (
                  <div className="grid grid-cols-1 gap-4 text-sm sm:grid-cols-2">
                    {lic.external_customer_id && (
                      <div>
                        <p className="text-muted-foreground">{t("licenses.externalCustomerID")}</p>
                        <p className="mt-1 font-mono text-xs break-all">{lic.external_customer_id}</p>
                      </div>
                    )}
                    {lic.external_workspace_id && (
                      <div>
                        <p className="text-muted-foreground">{t("licenses.externalWorkspaceID")}</p>
                        <p className="mt-1 font-mono text-xs break-all">{lic.external_workspace_id}</p>
                      </div>
                    )}
                  </div>
                )}

                {/* Actions */}
                <div className="flex gap-2 flex-wrap">
                  {/* Sends the key to the address on the licence again,
                    for a first send that never arrived. Behind a confirm
                    because it mails a credential, and disabled while
                    in flight so a second click cannot post a second
                    copy. */}
                  {lic.email && (
                    <AlertDialog>
                      <AlertDialogTrigger asChild>
                        {/* Only a key that works is mailed; the server
                          refuses the rest. */}
                        <Button variant="outline" size="sm" disabled={resendMut.isPending || !lic.key_usable}>
                          <Mail className="h-4 w-4 mr-1" /> {t("licenses.resendEmail")}
                        </Button>
                      </AlertDialogTrigger>
                      <AlertDialogContent>
                        <AlertDialogHeader>
                          <AlertDialogTitle>{t("licenses.resendEmail")}</AlertDialogTitle>
                          <AlertDialogDescription>
                            {t("licenses.resendEmailConfirm", { email: lic.email })}
                          </AlertDialogDescription>
                        </AlertDialogHeader>
                        <div className="flex justify-end gap-2 mt-4">
                          <AlertDialogCancel>{t("common.cancel")}</AlertDialogCancel>
                          <AlertDialogAction onClick={() => resendMut.mutate()}>
                            {t("licenses.resendEmailAction")}
                          </AlertDialogAction>
                        </div>
                      </AlertDialogContent>
                    </AlertDialog>
                  )}
                  {/* Same states the server suspends: a licence in use. */}
                  {(lic.status === "active" || lic.status === "trialing" || lic.status === "past_due") && (
                    <AlertDialog>
                      <AlertDialogTrigger asChild>
                        <Button variant="outline" size="sm" disabled={suspendMut.isPending}>
                          <Pause className="h-4 w-4 mr-1" /> {t("licenses.suspend")}
                        </Button>
                      </AlertDialogTrigger>
                      <AlertDialogContent>
                        <AlertDialogHeader>
                          <AlertDialogTitle>{t("licenses.suspend")}</AlertDialogTitle>
                          <AlertDialogDescription>{t("licenses.suspendConfirm")}</AlertDialogDescription>
                        </AlertDialogHeader>
                        <div className="flex justify-end gap-2 mt-4">
                          <AlertDialogCancel>{t("common.cancel")}</AlertDialogCancel>
                          <AlertDialogAction onClick={() => suspendMut.mutate()}>
                            {t("licenses.suspend")}
                          </AlertDialogAction>
                        </div>
                      </AlertDialogContent>
                    </AlertDialog>
                  )}
                  {/* Mirrors store.ReinstateLicense. An expired or canceled
                    license whose expiry date has passed would stay unusable
                    after a reinstate; setting a new expiry date (which
                    reactivates it) is the way back for those. */}
                  {(lic.status === "suspended" ||
                    ((lic.status === "expired" || lic.status === "canceled") &&
                      (!lic.valid_until || new Date(lic.valid_until) > new Date()))) && (
                    <Button
                      variant="outline"
                      size="sm"
                      onClick={() => reinstateMut.mutate()}
                      disabled={reinstateMut.isPending}
                    >
                      <Play className="h-4 w-4 mr-1" /> {t("licenses.reinstate")}
                    </Button>
                  )}
                  {lic.status !== "revoked" && (
                    <AlertDialog>
                      <AlertDialogTrigger asChild>
                        <Button variant="destructive" size="sm" disabled={revokeMut.isPending}>
                          <Ban className="h-4 w-4 mr-1" /> {t("licenses.revoke")}
                        </Button>
                      </AlertDialogTrigger>
                      <AlertDialogContent>
                        <AlertDialogHeader>
                          <AlertDialogTitle>{t("licenses.revoke")}</AlertDialogTitle>
                          <AlertDialogDescription>{t("licenses.revokeConfirm")}</AlertDialogDescription>
                        </AlertDialogHeader>
                        <div className="flex justify-end gap-2 mt-4">
                          <AlertDialogCancel>{t("common.cancel")}</AlertDialogCancel>
                          <AlertDialogAction
                            className="bg-destructive text-white hover:bg-destructive/90"
                            onClick={() => revokeMut.mutate()}
                          >
                            {t("licenses.revoke")}
                          </AlertDialogAction>
                        </div>
                      </AlertDialogContent>
                    </AlertDialog>
                  )}
                  {/* Cancel and revoke (the /refund route): only a licence
                    a subscription bills has one to cancel. */}
                  {lic.stripe_subscription_id && lic.status !== "revoked" && (
                    <AlertDialog>
                      <AlertDialogTrigger asChild>
                        <Button
                          variant="outline"
                          size="sm"
                          className="text-destructive border-destructive"
                          disabled={refundMut.isPending}
                        >
                          {t("licenses.refund")}
                        </Button>
                      </AlertDialogTrigger>
                      <AlertDialogContent>
                        <AlertDialogHeader>
                          <AlertDialogTitle>{t("licenses.refund")}</AlertDialogTitle>
                          <AlertDialogDescription>{t("licenses.refundConfirm")}</AlertDialogDescription>
                        </AlertDialogHeader>
                        <div className="flex justify-end gap-2 mt-4">
                          <AlertDialogCancel>{t("common.cancel")}</AlertDialogCancel>
                          <AlertDialogAction
                            className="bg-destructive text-white hover:bg-destructive/90"
                            onClick={() => refundMut.mutate()}
                          >
                            {t("licenses.refund")}
                          </AlertDialogAction>
                        </div>
                      </AlertDialogContent>
                    </AlertDialog>
                  )}
                  {/* A Stripe subscription owns the plan of the licence it
                    bills (the server refuses); a revoked one has none to
                    change. */}
                  <Button
                    variant="outline"
                    size="sm"
                    disabled={!!lic.stripe_subscription_id || lic.status === "revoked"}
                    onClick={() => setChangingPlan(true)}
                  >
                    {t("licenses.changePlan")}
                  </Button>
                </div>

                {/* Activations — hidden for SaaS products (which don't
                  use per-device activation; their user model is seats). */}
                {lic.product?.type !== "saas" && (
                  <>
                    <Separator />
                    <div>
                      <h3 className="font-semibold mb-3">
                        {t("licenses.activations")} ({lic.activations?.length || 0} /{" "}
                        {(lic.activation_limit ?? lic.plan?.max_activations) || "-"})
                      </h3>
                      {lic.activations && lic.activations.length > 0 ? (
                        <DataTable>
                          <DataTableHeader>
                            <DataTableRow>
                              <DataTableHead>{t("licenses.identifier")}</DataTableHead>
                              <DataTableHead>{t("licenses.type")}</DataTableHead>
                              <DataTableHead>{t("licenses.label")}</DataTableHead>
                              <DataTableHead>{t("licenses.keyHolder")}</DataTableHead>
                              <DataTableHead>{t("licenses.lastVerified")}</DataTableHead>
                              <DataTableHead className="w-12" />
                            </DataTableRow>
                          </DataTableHeader>
                          <DataTableBody>
                            {lic.activations.map((act) => (
                              <DataTableRow key={act.id}>
                                <DataTableCell>
                                  <code className="block max-w-[22ch] break-all text-xs" title={act.identifier}>
                                    {act.identifier}
                                  </code>
                                </DataTableCell>
                                <DataTableCell>
                                  <Badge variant="secondary">{act.identifier_type}</Badge>
                                </DataTableCell>
                                <DataTableCell className="text-muted-foreground">{act.label || "-"}</DataTableCell>
                                <DataTableCell className="text-muted-foreground text-xs break-all">
                                  {act.member_email ?? t("licenses.keyHolderLicense")}
                                </DataTableCell>
                                <DataTableCell className="text-muted-foreground text-xs">
                                  {formatDate(act.last_verified)}
                                </DataTableCell>
                                <DataTableCell>
                                  <Button
                                    variant="ghost"
                                    size="icon"
                                    className="h-7 w-7"
                                    onClick={() =>
                                      setConfirmDeactivation({ id: act.id, label: act.label || act.identifier })
                                    }
                                    disabled={deleteActMut.isPending && deleteActMut.variables === act.id}
                                  >
                                    <Trash2 className="h-3 w-3 text-destructive" />
                                  </Button>
                                </DataTableCell>
                              </DataTableRow>
                            ))}
                          </DataTableBody>
                        </DataTable>
                      ) : (
                        <p className="text-sm text-muted-foreground">{t("licenses.noActivations")}</p>
                      )}
                    </div>
                  </>
                )}

                {/* Entitlements */}
                {lic.plan?.entitlements && lic.plan.entitlements.length > 0 && (
                  <>
                    <Separator />
                    <div>
                      <h3 className="font-semibold mb-3">{t("plans.entitlements")}</h3>
                      <div className="grid grid-cols-1 gap-2 sm:grid-cols-2">
                        {lic.plan.entitlements.map((e) => (
                          <div key={e.id} className="flex justify-between bg-muted/50 rounded px-3 py-2 text-sm">
                            <span className="font-medium">{e.feature}</span>
                            <span className="text-muted-foreground">{e.value}</span>
                          </div>
                        ))}
                      </div>
                    </div>
                  </>
                )}
              </div>
            </TabsContent>

            <TabsContent value="usage" className="-mr-6 min-h-0 flex-1 overflow-y-auto pr-6">
              <UsageTab licenseId={id} />
            </TabsContent>

            <TabsContent value="seats" className="-mr-6 min-h-0 flex-1 overflow-y-auto pr-6">
              <SeatsTab
                licenseId={id}
                maxSeats={lic?.seat_limit ?? lic?.plan?.max_seats ?? 0}
                hasSeats={lic?.product?.type === "saas" || lic?.product?.type === "hybrid"}
                ownerEmail={lic?.email ?? ""}
              />
            </TabsContent>
          </Tabs>
        )}

        {/* Change Plan Dialog */}
        {changingPlan && lic && (
          <ChangePlanDialog
            licenseId={id}
            productId={lic.product_id}
            currentPlanId={lic.plan_id}
            currentPlan={lic.plan}
            onClose={() => setChangingPlan(false)}
          />
        )}
        <AlertDialog open={confirmUnlink} onOpenChange={() => setConfirmUnlink(false)}>
          <AlertDialogContent>
            <AlertDialogHeader>
              <AlertDialogTitle>{t("licenses.unlinkTitle")}</AlertDialogTitle>
              <AlertDialogDescription>{t("licenses.unlinkConfirm")}</AlertDialogDescription>
            </AlertDialogHeader>
            <div className="flex justify-end gap-2">
              <AlertDialogCancel>{t("common.cancel")}</AlertDialogCancel>
              <AlertDialogAction onClick={() => unlinkMut.mutate()} disabled={unlinkMut.isPending}>
                {unlinkMut.isPending ? t("common.loading") : t("licenses.unlink")}
              </AlertDialogAction>
            </div>
          </AlertDialogContent>
        </AlertDialog>
        <AlertDialog open={confirmRotate} onOpenChange={(o) => !o && !rotateMut.isPending && setConfirmRotate(false)}>
          <AlertDialogContent>
            <AlertDialogHeader>
              <AlertDialogTitle>{t("licenses.rotateKeyTitle")}</AlertDialogTitle>
              <AlertDialogDescription>
                {t("licenses.rotateKeyDesc")} {t("licenses.rotateKeyAdminNote")}
              </AlertDialogDescription>
            </AlertDialogHeader>
            <div className="flex justify-end gap-2">
              <AlertDialogCancel disabled={rotateMut.isPending}>{t("common.cancel")}</AlertDialogCancel>
              <AlertDialogAction
                className="bg-destructive text-white hover:bg-destructive/90"
                disabled={rotateMut.isPending}
                onClick={(e) => {
                  e.preventDefault()
                  if (rotating.current) return
                  rotating.current = true
                  rotateMut.mutate()
                }}
              >
                {t("licenses.rotateKey")}
              </AlertDialogAction>
            </div>
          </AlertDialogContent>
        </AlertDialog>
        <AlertDialog open={!!confirmDeactivation} onOpenChange={() => setConfirmDeactivation(null)}>
          <AlertDialogContent>
            <AlertDialogHeader>
              <AlertDialogTitle>
                {t("licenses.deactivateTitle", { device: confirmDeactivation?.label || "" })}
              </AlertDialogTitle>
              <AlertDialogDescription>{t("licenses.deactivateDesc")}</AlertDialogDescription>
            </AlertDialogHeader>
            <div className="flex justify-end gap-2">
              <AlertDialogCancel>{t("common.cancel")}</AlertDialogCancel>
              <AlertDialogAction
                className="bg-destructive text-white hover:bg-destructive/90"
                onClick={() => confirmDeactivation && deleteActMut.mutate(confirmDeactivation.id)}
              >
                {t("licenses.deactivate")}
              </AlertDialogAction>
            </div>
          </AlertDialogContent>
        </AlertDialog>
      </DialogContent>
    </Dialog>
  )
}

function UsageTab({ licenseId }: { licenseId: string }) {
  const { t } = useI18n()
  const qc = useQueryClient()
  const pg = useServerPagination(20)
  const { data, isLoading } = useQuery({
    queryKey: ["admin", "license-usage", licenseId, pg.page, pg.pageSize],
    queryFn: () => admin.getLicenseUsage(licenseId, pg.params),
  })

  // confirmResetUsage holds the counter row pending confirmation.
  // Reset is destructive: it zeroes the counter for the live period,
  // which most plans will treat as "free re-use until the next
  // reset" — a stray click can effectively grant the customer free
  // usage. The AlertDialog forces an intentional click.
  const [confirmResetUsage, setConfirmResetUsage] = useState<UsageCounter | null>(null)
  const resetMut = useMutation({
    mutationFn: (c: UsageCounter) =>
      admin.resetUsageCounter(licenseId, { feature: c.feature, period: c.period, period_key: c.period_key }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["admin", "license-usage", licenseId] })
      setConfirmResetUsage(null)
    },
    onError: (e: Error) => showToast(e.message, "error"),
  })

  const counters = data?.counters || []
  const { items: events, total, totalPages } = pg.from(data, data?.events)

  if (isLoading) return <div className="h-32 animate-pulse bg-muted rounded-lg" />

  return (
    <div className="space-y-6 pt-2">
      {/* Counters */}
      <div>
        <h3 className="font-semibold mb-3">{t("licenses.usageCounters")}</h3>
        {counters.length === 0 ? (
          <p className="text-sm text-muted-foreground">{t("licenses.noUsageCounters")}</p>
        ) : (
          <DataTable>
            <DataTableHeader>
              <DataTableRow>
                <DataTableHead>{t("licenses.feature")}</DataTableHead>
                <DataTableHead>{t("licenses.period")}</DataTableHead>
                <DataTableHead>{t("licenses.periodKey")}</DataTableHead>
                <DataTableHead>{t("licenses.used")}</DataTableHead>
                <DataTableHead>{t("licenses.updated")}</DataTableHead>
                <DataTableHead className="w-12" />
              </DataTableRow>
            </DataTableHeader>
            <DataTableBody>
              {counters.map((c) => (
                <DataTableRow key={c.id}>
                  <DataTableCell className="font-medium">{c.feature}</DataTableCell>
                  <DataTableCell>
                    <Badge variant="secondary">{c.period}</Badge>
                  </DataTableCell>
                  <DataTableCell className="text-muted-foreground text-xs">{c.period_key}</DataTableCell>
                  <DataTableCell className="font-medium">{c.used}</DataTableCell>
                  <DataTableCell className="text-muted-foreground text-xs">{formatDate(c.updated_at)}</DataTableCell>
                  <DataTableCell>
                    <Button
                      variant="ghost"
                      size="icon"
                      className="h-7 w-7"
                      title={t("licenses.resetCounter")}
                      onClick={() => setConfirmResetUsage(c)}
                      disabled={resetMut.isPending && resetMut.variables?.id === c.id}
                    >
                      <RefreshCw className="h-3 w-3" />
                    </Button>
                  </DataTableCell>
                </DataTableRow>
              ))}
            </DataTableBody>
          </DataTable>
        )}
      </div>

      <Separator />

      {/* Recent events */}
      <div>
        <h3 className="font-semibold mb-3">{t("licenses.recentEvents")}</h3>
        {events.length === 0 ? (
          <p className="text-sm text-muted-foreground">{t("licenses.noUsageEvents")}</p>
        ) : (
          <>
            <DataTable>
              <DataTableHeader>
                <DataTableRow>
                  <DataTableHead>{t("licenses.feature")}</DataTableHead>
                  <DataTableHead>{t("licenses.quantity")}</DataTableHead>
                  <DataTableHead>{t("licenses.ip")}</DataTableHead>
                  <DataTableHead>{t("licenses.recorded")}</DataTableHead>
                </DataTableRow>
              </DataTableHeader>
              <DataTableBody>
                {events.map((e) => (
                  <DataTableRow key={e.id}>
                    <DataTableCell className="font-medium">{e.feature}</DataTableCell>
                    <DataTableCell>{e.quantity}</DataTableCell>
                    <DataTableCell className="text-muted-foreground text-xs">{e.ip_address || "-"}</DataTableCell>
                    <DataTableCell className="text-muted-foreground text-xs">{formatDate(e.recorded_at)}</DataTableCell>
                  </DataTableRow>
                ))}
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
      </div>
      <AlertDialog open={!!confirmResetUsage} onOpenChange={() => setConfirmResetUsage(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>
              {t("licenses.resetCounterTitle", {
                feature: confirmResetUsage
                  ? `${confirmResetUsage.feature} (${confirmResetUsage.period} ${confirmResetUsage.period_key})`
                  : "",
              })}
            </AlertDialogTitle>
            <AlertDialogDescription>{t("licenses.resetCounterDesc")}</AlertDialogDescription>
          </AlertDialogHeader>
          <div className="flex justify-end gap-2">
            <AlertDialogCancel>{t("common.cancel")}</AlertDialogCancel>
            <AlertDialogAction onClick={() => confirmResetUsage && resetMut.mutate(confirmResetUsage)}>
              {t("licenses.resetCounter")}
            </AlertDialogAction>
          </div>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  )
}

// SeatsTab is the licence's members as support sees them: each one's
// own key (masked; shown on request, audited, as the licence's key is),
// their devices, and what support does for a customer: take a member off
// or cancel an invitation, and reissue a member's key that got out.
//
// It does not invite. Adding someone to a customer's licence is the
// owner's to decide; an operator doing it from here would change who
// uses the licence without their consent.
function SeatsTab({
  licenseId,
  maxSeats,
  hasSeats,
  ownerEmail,
}: {
  licenseId: string
  maxSeats: number
  hasSeats: boolean
  ownerEmail: string
}) {
  const { t } = useI18n()
  const qc = useQueryClient()
  const pg = useServerPagination(20)
  const { data, isLoading } = useQuery({
    queryKey: ["admin", "license-seats", licenseId, pg.page, pg.pageSize],
    queryFn: () => admin.getLicenseSeats(licenseId, pg.params),
  })
  // Keys shown, by seat, only while this tab is open. One whose hint no
  // longer matches the seat's (reissued meanwhile, by another admin or
  // through the API) is dropped rather than shown or copied.
  const [shown, setShown] = useState<Record<string, string>>({})
  const shownKey = (seat: AdminSeat) => {
    const key = shown[seat.id]
    return key && (!seat.key_hint || key.endsWith(seat.key_hint)) ? key : undefined
  }
  // Set as a confirmation is pressed, before the mutation is pending, so
  // a double click cannot send it twice.
  const submitting = useRef(false)
  // The server's codes, in the admin's language.
  const seatError = (e: Error) =>
    errorCode(e) === "SEAT_NOT_JOINED"
      ? t("licenses.seatNotJoined")
      : ["SEAT_NOT_FOUND", "NOT_FOUND", "LICENSE_NOT_FOUND"].includes(errorCode(e) ?? "")
        ? t("licenses.seatGone")
        : e.message
  const [copied, setCopied] = useState<string | null>(null)
  // The seat a confirmation asks about stays set while it closes.
  const [asking, setAsking] = useState<{ seat: AdminSeat; action: "remove" | "reissue" } | null>(null)
  const [confirming, setConfirming] = useState(false)
  const refresh = () => {
    qc.invalidateQueries({ queryKey: ["admin", "license-seats", licenseId] })
    qc.invalidateQueries({ queryKey: ["admin", "license", licenseId] })
    qc.invalidateQueries({ queryKey: ["admin", "licenses"] })
  }

  const revealMut = useMutation({
    mutationFn: (seat: AdminSeat) => admin.revealMemberKey(licenseId, seat.id),
    meta: { ownErrors: true },
    onSuccess: (r, seat) => setShown((m) => ({ ...m, [seat.id]: r.license_key })),
    onError: (e: Error) => {
      refresh()
      showToast(seatError(e), "error")
    },
  })
  const reissueMut = useMutation({
    mutationFn: (seat: AdminSeat) => admin.rotateMemberKey(licenseId, seat.id),
    meta: { ownErrors: true },
    onSuccess: (r, seat) => {
      setShown((m) => ({ ...m, [seat.id]: r.license_key }))
      showToast(t("licenses.memberKeyReissued"), "success")
    },
    onError: (e: Error) => showToast(seatError(e), "error"),
    onSettled: () => {
      submitting.current = false
      refresh()
      setConfirming(false)
    },
  })
  const removeMut = useMutation({
    mutationFn: (seat: AdminSeat) => admin.removeLicenseSeat(licenseId, seat.id),
    meta: { ownErrors: true },
    onSuccess: (_, seat) =>
      showToast(t(seat.accepted_at ? "licenses.memberRemoved" : "licenses.inviteCancelled"), "success"),
    onError: (e: Error) => showToast(seatError(e), "error"),
    onSettled: () => {
      submitting.current = false
      refresh()
      setConfirming(false)
    },
  })
  const copy = (seat: AdminSeat) => {
    const write = (key: string) => {
      navigator.clipboard.writeText(key)
      setCopied(seat.id)
      setTimeout(() => setCopied((c) => (c === seat.id ? null : c)), 2000)
    }
    const key = shownKey(seat)
    if (key) write(key)
    else revealMut.mutate(seat, { onSuccess: (r) => write(r.license_key) })
  }

  const { items: seats, total, totalPages } = pg.from(data, data?.seats)
  // active_count is the members; on a product with seats max_seats
  // counts the owner too, who holds one without a row.
  const activeCount = (data?.active_count ?? 0) + (hasSeats ? 1 : 0)
  const busy = reissueMut.isPending || removeMut.isPending

  if (isLoading) return <div className="h-32 animate-pulse bg-muted rounded-lg" />

  return (
    <div className="space-y-4 pt-2">
      <h3 className="font-semibold">
        {t("licenses.seats")} (
        {maxSeats > 0
          ? t("licenses.seatsUsedOf").replace("{used}", String(activeCount)).replace("{max}", String(maxSeats))
          : t("licenses.seatsUsed").replace("{used}", String(activeCount))}
        )
      </h3>
      {hasSeats && <p className="text-xs text-muted-foreground">{t("licenses.seatsIncludeOwner")}</p>}
      {seats.length === 0 ? (
        <p className="text-sm text-muted-foreground">{t(hasSeats ? "licenses.noMembers" : "licenses.noSeats")}</p>
      ) : (
        <DataTable>
          <DataTableHeader>
            <DataTableRow>
              <DataTableHead>{t("common.email")}</DataTableHead>
              <DataTableHead>{t("licenses.activations")}</DataTableHead>
              <DataTableHead className="text-right">{t("common.actions")}</DataTableHead>
            </DataTableRow>
          </DataTableHeader>
          <DataTableBody>
            {seats.map((s) => {
              const joined = !!s.accepted_at && !s.removed_at
              const expired =
                !s.accepted_at && !!s.invite_expires_at && new Date(s.invite_expires_at).getTime() < Date.now()
              return (
                <DataTableRow key={s.id}>
                  <DataTableCell>
                    <div className="font-medium break-all">{s.email}</div>
                    <div className="mt-1 flex flex-wrap items-center gap-1.5">
                      {/* A seat at the owner's own address (the license email
                        changed to a member's) is the owner, not counted as a
                        member in active_count. */}
                      <Badge variant="secondary">
                        {s.email.toLowerCase() === ownerEmail.toLowerCase()
                          ? t("portal.roleOwner")
                          : t("portal.roleMember")}
                      </Badge>
                      {joined && s.paused ? (
                        <Badge className="bg-amber-100 text-amber-800" title={t("portal.seatPausedHint")}>
                          {t("portal.seatPaused")}
                        </Badge>
                      ) : joined ? (
                        <Badge className="bg-emerald-100 text-emerald-800">{t("licenses.active")}</Badge>
                      ) : expired ? (
                        <Badge className="bg-red-100 text-red-800">{t("portal.inviteExpired")}</Badge>
                      ) : (
                        <Badge className="bg-amber-100 text-amber-800">{t("licenses.pending")}</Badge>
                      )}
                      <span className="text-xs text-muted-foreground">
                        {joined
                          ? `${t("licenses.accepted")} ${formatDate(s.accepted_at)}`
                          : `${t("licenses.invited")} ${formatDate(s.invited_at)}`}
                      </span>
                    </div>
                    {/* The member's own key, under the address so the row
                      fits a narrow screen. */}
                    {joined && (
                      <div className="mt-1.5 flex flex-wrap items-center gap-1">
                        <span className="basis-full text-xs text-muted-foreground sm:basis-auto">
                          {t("licenses.memberKey")}
                        </span>
                        <code
                          className={`min-w-0 bg-muted px-1.5 py-0.5 rounded text-xs ${shownKey(s) ? "break-all" : "whitespace-nowrap"}`}
                        >
                          {shownKey(s) ?? maskKey(s.key_hint, s.key_prefix)}
                        </code>
                        {!shownKey(s) && (
                          <Button
                            variant="ghost"
                            size="icon"
                            className="h-6 w-6 shrink-0"
                            title={t("licenses.revealKey")}
                            aria-label={t("licenses.revealKey")}
                            disabled={revealMut.isPending && revealMut.variables?.id === s.id}
                            onClick={() => revealMut.mutate(s)}
                          >
                            <Eye className="h-3 w-3" />
                          </Button>
                        )}
                        <Button
                          variant="ghost"
                          size="icon"
                          className="h-6 w-6 shrink-0"
                          title={t("common.copy")}
                          aria-label={t("common.copy")}
                          onClick={() => copy(s)}
                        >
                          {copied === s.id ? (
                            <Check className="h-3 w-3 text-emerald-600" />
                          ) : (
                            <Copy className="h-3 w-3" />
                          )}
                        </Button>
                      </div>
                    )}
                  </DataTableCell>
                  <DataTableCell className="text-muted-foreground">{joined ? s.device_count : "-"}</DataTableCell>
                  <DataTableCell className="text-right whitespace-nowrap">
                    {joined && (
                      <Button
                        variant="ghost"
                        size="icon"
                        className="h-7 w-7"
                        title={t("licenses.rotateKey")}
                        aria-label={t("licenses.rotateKey")}
                        disabled={busy}
                        onClick={() => {
                          setAsking({ seat: s, action: "reissue" })
                          setConfirming(true)
                        }}
                      >
                        <RotateCw className="h-3.5 w-3.5" />
                      </Button>
                    )}
                    <Button
                      variant="ghost"
                      size="icon"
                      className="h-7 w-7 text-destructive"
                      title={t(joined ? "portal.removeMember" : "portal.cancelInvite")}
                      aria-label={t(joined ? "portal.removeMember" : "portal.cancelInvite")}
                      disabled={busy}
                      onClick={() => {
                        setAsking({ seat: s, action: "remove" })
                        setConfirming(true)
                      }}
                    >
                      <Trash2 className="h-3.5 w-3.5" />
                    </Button>
                  </DataTableCell>
                </DataTableRow>
              )
            })}
          </DataTableBody>
        </DataTable>
      )}
      {total > pg.pageSize && (
        <DataTablePagination
          page={pg.page}
          totalPages={totalPages}
          total={total}
          pageSize={pg.pageSize}
          onPageChange={pg.setPage}
          onPageSizeChange={pg.setPageSize}
        />
      )}

      <AlertDialog open={confirming} onOpenChange={(o) => !o && !busy && setConfirming(false)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>
              {asking?.action === "reissue"
                ? t("licenses.reissueMemberKeyTitle").replace("{email}", asking.seat.email)
                : t(asking?.seat.accepted_at ? "portal.removeMemberTitle" : "portal.cancelInviteTitle")}
            </AlertDialogTitle>
            <AlertDialogDescription>
              {(asking?.action === "reissue"
                ? t("licenses.reissueMemberKeyDesc")
                : t(asking?.seat.accepted_at ? "licenses.adminRemoveMemberDesc" : "licenses.adminCancelInviteDesc")
              ).replace("{email}", asking?.seat.email ?? "")}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <div className="flex justify-end gap-2">
            <AlertDialogCancel disabled={busy}>
              {t(asking?.action === "remove" && !asking.seat.accepted_at ? "portal.keepInvite" : "common.cancel")}
            </AlertDialogCancel>
            <AlertDialogAction
              className="bg-destructive text-white hover:bg-destructive/90"
              disabled={busy}
              onClick={(e) => {
                e.preventDefault()
                if (!asking || submitting.current) return
                submitting.current = true
                if (asking.action === "reissue") reissueMut.mutate(asking.seat)
                else removeMut.mutate(asking.seat)
              }}
            >
              {asking?.action === "reissue"
                ? t("licenses.rotateKey")
                : t(asking?.seat.accepted_at ? "portal.removeMember" : "portal.cancelInvite")}
            </AlertDialogAction>
          </div>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  )
}

function ChangePlanDialog({
  licenseId,
  productId,
  currentPlanId,
  currentPlan,
  onClose,
}: {
  licenseId: string
  productId: string
  currentPlanId: string
  // The plan the licence is on now, so the box shows it even when the
  // product has more plans than one page of candidates.
  currentPlan?: Plan | null
  onClose: () => void
}) {
  const { t } = useI18n()
  const qc = useQueryClient()
  const [planId, setPlanId] = useState(currentPlanId)

  const changeMut = useMutation({
    mutationFn: () => admin.changeLicensePlan(licenseId, { plan_id: planId }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["admin"] })
      onClose()
    },
    onError: (e: Error) => showToast(e.message, "error"),
  })

  return (
    <Dialog open onOpenChange={onClose}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>{t("licenses.changePlan")}</DialogTitle>
          <DialogDescription>{t("licenses.changePlanDesc")}</DialogDescription>
        </DialogHeader>
        <form
          onSubmit={(e) => {
            e.preventDefault()
            changeMut.mutate()
          }}
          className="flex min-h-0 flex-1 flex-col gap-4"
        >
          <DialogBody className="space-y-4">
            <div className="space-y-2">
              <Label>{t("common.plan")}</Label>
              <PlanSelect
                productId={productId}
                value={planId}
                onChange={setPlanId}
                current={currentPlan}
                placeholder={t("licenses.selectPlan")}
                className="w-full"
                withType
              />
            </div>
          </DialogBody>
          <DialogFooter>
            <Button type="button" variant="outline" onClick={onClose}>
              {t("common.cancel")}
            </Button>
            <Button type="submit" disabled={changeMut.isPending || planId === currentPlanId}>
              {changeMut.isPending ? t("common.loading") : t("licenses.changePlan")}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

// QuantityField shows the seats a license on a plan sold per seat holds.
// A Stripe subscription's seats are Stripe's, changed by the customer in
// the portal or in Stripe; any other license's are set here.
function QuantityField({ license }: { license: License }) {
  const { t } = useI18n()
  const qc = useQueryClient()
  const [editing, setEditing] = useState(false)
  const [value, setValue] = useState(String(license.quantity ?? 1))
  const n = Number(value)
  // What the plan sells, as a checkout or the portal would allow.
  const min = license.plan?.min_quantity ?? 1
  const max = license.plan?.max_quantity ?? 10000
  const valid = Number.isInteger(n) && n >= min && n <= max
  const saveMut = useMutation({
    mutationFn: () => admin.setLicenseQuantity(license.id, n),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["admin", "license", license.id] })
      qc.invalidateQueries({ queryKey: ["admin", "licenses"] })
      setEditing(false)
      showToast(t("licenses.quantitySaved"), "success")
    },
    meta: { ownErrors: true },
    onError: (e: Error) => {
      // Billed by Stripe, or moved off a plan sold per seat, meanwhile:
      // the field shows the licence as it is now.
      if (errorCode(e) === "STRIPE_MANAGED" || errorCode(e) === "NOT_PER_SEAT") {
        qc.invalidateQueries({ queryKey: ["admin", "license", license.id] })
        setEditing(false)
      }
      // Below what the licence uses: the same refusal the portal gives
      // its owner, who frees the room first.
      const d = errorDetails(e) ?? {}
      showToast(
        errorCode(e) === "STRIPE_MANAGED"
          ? t("licenses.quantityStripeManaged")
          : errorCode(e) === "NOT_PER_SEAT"
            ? t("licenses.quantityNotPerSeat")
            : errorCode(e) === "QUANTITY_OUT_OF_RANGE"
              ? t("licenses.quantityOutOfRange")
                  .replace("{min}", String(d.min ?? min))
                  .replace("{max}", String(d.max ?? max))
              : errorCode(e) === "SEATS_IN_USE"
                ? t("portal.changeSeatsInUse")
                    .replace("{people}", String(d.in_use ?? ""))
                    .replace("{seats}", String(d.seat_limit ?? n))
                : errorCode(e) === "DEVICES_IN_USE"
                  ? t(d.kind === "sessions" ? "portal.changeSessionsInUse" : "portal.changeDevicesInUse")
                      .replace("{inUse}", String(d.in_use ?? ""))
                      .replace("{limit}", String(d.activation_limit ?? ""))
                  : e.message,
        "error",
      )
    },
  })
  const stripeManaged = !!license.stripe_subscription_id
  return (
    <div>
      <p className="text-muted-foreground">{t("licenses.seatsBought")}</p>
      {editing ? (
        <div className="mt-1 flex flex-wrap items-center gap-2">
          <Input
            type="number"
            min={min}
            max={max}
            className="h-8 w-24"
            aria-label={t("licenses.seatsBought")}
            value={value}
            autoFocus
            onChange={(e) => setValue(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter" && valid && !saveMut.isPending) saveMut.mutate()
            }}
          />
          <Button size="sm" className="h-8" disabled={!valid || saveMut.isPending} onClick={() => saveMut.mutate()}>
            {t("common.save")}
          </Button>
          <Button size="sm" variant="ghost" className="h-8" onClick={() => setEditing(false)}>
            {t("common.cancel")}
          </Button>
          {/* Why Save may be off: the plan's range. */}
          <p className={`basis-full text-xs ${valid ? "text-muted-foreground" : "text-destructive"}`}>
            {t("licenses.quantityOutOfRange").replace("{min}", String(min)).replace("{max}", String(max))}
          </p>
        </div>
      ) : (
        <div className="mt-1 flex items-center gap-2">
          <span className="font-medium">{license.quantity ?? 1}</span>
          {stripeManaged ? (
            <span className="text-xs text-muted-foreground">{t("licenses.quantityStripe")}</span>
          ) : (
            <Button
              variant="ghost"
              size="sm"
              className="h-6 px-2 text-xs"
              onClick={() => {
                setValue(String(license.quantity ?? 1))
                setEditing(true)
              }}
            >
              {t("common.edit")}
            </Button>
          )}
        </div>
      )}
    </div>
  )
}
