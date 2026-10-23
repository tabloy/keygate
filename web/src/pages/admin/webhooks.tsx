import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import {
  Check,
  ChevronDown,
  ChevronRight,
  Copy,
  Eye,
  EyeOff,
  History,
  Package,
  Pause,
  Play,
  Plus,
  RefreshCw,
  Send,
  Trash2,
} from "lucide-react"
import { Fragment, useState } from "react"
import { Link } from "react-router-dom"
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
import { type TranslationKeys, useI18n } from "@/i18n"
import { admin, type WebhookConfig } from "@/lib/api"
import { boolColor, formatDate } from "@/lib/utils"

// Must stay in sync with model.WebhookEvents on the server, which
// refuses any other name. Dispatch only delivers to webhooks subscribed
// to the event, so an event missing from this list can never be
// subscribed to from the dashboard.
const WEBHOOK_EVENTS = [
  "license.created",
  "license.activated",
  "license.deactivated",
  "license.expiry_changed",
  "license.updates_changed",
  "license.expired",
  "license.canceled",
  "license.suspended",
  "license.reinstated",
  "license.revoked",
  "license.payment_failed",
  "license.payment_recovered",
  "quota.warning",
  "quota.exceeded",
  "seat.added",
  "seat.removed",
  "plan.changed",
  "release.published",
  "release.yanked",
  "release.unyanked",
]

export default function WebhooksPage() {
  const { t } = useI18n()
  const qc = useQueryClient()
  const [productFilter, setProductFilter] = useState<string>("")
  const [search, setSearch] = useState("")
  const pg = useServerPagination(10, [productFilter, search])
  const { data: productsData } = useQuery({
    // One row is all this page needs from the catalogue: whether the
    // install has any product at all, and which one a new form starts
    // on. The pickers fetch their own candidates, with search.
    queryKey: ["admin", "products", "newest"],
    queryFn: () => admin.listProducts({ limit: 1 }),
  })
  const { data, isLoading } = useQuery({
    queryKey: ["admin", "webhooks", productFilter, search, pg.page, pg.pageSize],
    queryFn: () => admin.listWebhooks({ product_id: productFilter, search, ...pg.params }),
  })
  const [creating, setCreating] = useState(false)
  const [newSecret, setNewSecret] = useState<string | null>(null)
  const [deleting, setDeleting] = useState<WebhookConfig | null>(null)
  const [viewingDeliveries, setViewingDeliveries] = useState<string | null>(null)

  const products = productsData?.products || []
  const { items: paginatedWebhooks, total: wTotal, totalPages: wTotalPages } = pg.from(data, data?.webhooks)

  const createMut = useMutation({
    mutationFn: admin.createWebhook,
    onSuccess: (data) => {
      qc.invalidateQueries({ queryKey: ["admin", "webhooks"] })
      setCreating(false)
      setNewSecret(data.secret)
      showToast(t("toast.webhookCreated"), "success")
    },
    onError: (e: Error) => showToast(e.message, "error"),
  })
  const toggleMut = useMutation({
    mutationFn: ({ id, active }: { id: string; active: boolean }) => admin.updateWebhook(id, { active }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["admin", "webhooks"] })
    },
    onError: (e: Error) => showToast(e.message, "error"),
  })
  const deleteMut = useMutation({
    mutationFn: (id: string) => admin.deleteWebhook(id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["admin", "webhooks"] })
      setDeleting(null)
    },
    onError: (e: Error) => showToast(e.message, "error"),
  })
  const testMut = useMutation({
    mutationFn: (id: string) => admin.testWebhook(id),
    onSuccess: (res) => {
      // The server waits for the receiver, so this is the real result,
      // not "queued".
      if (res.status === "delivered") {
        showToast(t("webhooks.testDelivered", { code: res.response_code }), "success")
      } else {
        const detail = res.response_code ? `HTTP ${res.response_code}` : res.response_body || t("webhooks.testFailed")
        showToast(t("webhooks.testFailedWith", { detail }), "error")
      }
      qc.invalidateQueries({ queryKey: ["admin", "webhook-deliveries"] })
    },
    onError: (err: Error) => {
      showToast(err.message || t("webhooks.testFailed"), "error")
    },
  })

  if (products.length === 0 && !isLoading) {
    return (
      <div className="space-y-6">
        <div>
          <h1 className="text-2xl font-bold tracking-tight sr-only md:not-sr-only">{t("webhooks.title")}</h1>
          <p className="text-muted-foreground">{t("webhooks.subtitle")}</p>
        </div>
        <Card>
          <CardContent className="py-12 text-center">
            <Package className="h-12 w-12 mx-auto text-muted-foreground mb-4" />
            <p className="text-lg font-medium">{t("licenses.noProducts")}</p>
            <p className="text-muted-foreground mt-1 mb-4">{t("webhooks.noProductsDesc")}</p>
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
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div className="min-w-0">
          <h1 className="text-2xl font-bold tracking-tight sr-only md:not-sr-only">{t("webhooks.title")}</h1>
          <p className="text-muted-foreground">{t("webhooks.subtitle")}</p>
        </div>
        <Button onClick={() => setCreating(true)}>
          <Plus className="h-4 w-4 mr-2" /> {t("webhooks.new")}
        </Button>
      </div>

      <div className="flex flex-wrap gap-3">
        <ProductSelect value={productFilter} onChange={setProductFilter} allLabel={t("filter.allProducts")} />
        <Input
          placeholder={t("common.search")}
          value={search}
          onChange={(e) => setSearch(e.target.value)}
          className="w-full sm:w-64"
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
                    <DataTableHead>URL</DataTableHead>
                    <DataTableHead>{t("common.product")}</DataTableHead>
                    <DataTableHead>{t("webhooks.events")}</DataTableHead>
                    <DataTableHead>{t("common.status")}</DataTableHead>
                    <DataTableHead>{t("common.created")}</DataTableHead>
                    <DataTableHead className="w-40 text-right">{t("common.actions")}</DataTableHead>
                  </DataTableRow>
                </DataTableHeader>
                <DataTableBody>
                  {paginatedWebhooks.length === 0 && <DataTableEmpty colSpan={6} message={t("webhooks.empty")} />}
                  {paginatedWebhooks.map((wh) => (
                    <DataTableRow key={wh.id}>
                      <DataTableCell className="font-medium max-w-xs truncate">
                        <code className="text-xs">{wh.url}</code>
                      </DataTableCell>
                      <DataTableCell className="text-muted-foreground">{wh.product?.name || "-"}</DataTableCell>
                      <DataTableCell>
                        <div className="flex flex-wrap gap-1">
                          {wh.events.slice(0, 3).map((e) => (
                            <Badge key={e} variant="secondary" className="text-xs">
                              {e}
                            </Badge>
                          ))}
                          {wh.events.length > 3 && (
                            <Badge variant="secondary" className="text-xs">
                              +{wh.events.length - 3}
                            </Badge>
                          )}
                        </div>
                      </DataTableCell>
                      <DataTableCell>
                        <Badge className={boolColor(wh.active)}>
                          {wh.active ? t("common.active") : t("webhooks.inactive")}
                        </Badge>
                      </DataTableCell>
                      <DataTableCell className="text-muted-foreground text-xs">
                        {formatDate(wh.created_at)}
                      </DataTableCell>
                      <DataTableCell>
                        {/* One icon per action, each named by its tooltip and
                          aria-label. The icons say what happens: a send for the
                          test event, pause/play for stopping or resuming
                          delivery (not an eye, which reads as "view"), and the
                          log opens from a history icon. Pending state is per
                          row via mutation.variables, so one busy webhook does
                          not disable the others. */}
                        <div className="flex items-center justify-end gap-1">
                          <IconAction label={t("webhooks.deliveries")} onClick={() => setViewingDeliveries(wh.id)}>
                            <History className="h-4 w-4" />
                          </IconAction>
                          <IconAction
                            label={t("webhooks.sendTest")}
                            onClick={() => testMut.mutate(wh.id)}
                            disabled={testMut.isPending && testMut.variables === wh.id}
                          >
                            <Send className="h-4 w-4" />
                          </IconAction>
                          <IconAction
                            label={wh.active ? t("webhooks.disable") : t("webhooks.enable")}
                            onClick={() => toggleMut.mutate({ id: wh.id, active: !wh.active })}
                            disabled={toggleMut.isPending && toggleMut.variables?.id === wh.id}
                          >
                            {wh.active ? <Pause className="h-4 w-4" /> : <Play className="h-4 w-4" />}
                          </IconAction>
                          <IconAction label={t("common.delete")} onClick={() => setDeleting(wh)}>
                            <Trash2 className="h-4 w-4 text-destructive" />
                          </IconAction>
                        </div>
                      </DataTableCell>
                    </DataTableRow>
                  ))}
                </DataTableBody>
              </DataTable>
              {wTotal > 0 && (
                <DataTablePagination
                  page={pg.page}
                  totalPages={wTotalPages}
                  total={wTotal}
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
        <CreateWebhookDialog
          open
          onClose={() => setCreating(false)}
          products={products}
          onSubmit={(d) => createMut.mutate(d)}
          loading={createMut.isPending}
        />
      )}

      {/* Show new secret */}
      {newSecret && <SecretDialog secret={newSecret} onClose={() => setNewSecret(null)} />}

      {/* Deliveries */}
      {viewingDeliveries && (
        <DeliveryLogDialog webhookId={viewingDeliveries} onClose={() => setViewingDeliveries(null)} />
      )}

      {/* Delete */}
      <AlertDialog open={!!deleting} onOpenChange={() => setDeleting(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{t("common.delete")} webhook?</AlertDialogTitle>
            <AlertDialogDescription>{t("webhooks.deleteConfirm")}</AlertDialogDescription>
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

function CreateWebhookDialog({
  open,
  onClose,
  products,
  onSubmit,
  loading,
}: {
  open: boolean
  onClose: () => void
  products: { id: string; name: string }[]
  onSubmit: (d: { product_id: string; url: string; events: string[] }) => void
  loading: boolean
}) {
  const { t } = useI18n()
  const [productId, setProductId] = useState(products[0]?.id || "")
  const [url, setUrl] = useState("")
  const [selectedEvents, setSelectedEvents] = useState<string[]>([])

  const toggleEvent = (event: string) => {
    setSelectedEvents((prev) => (prev.includes(event) ? prev.filter((e) => e !== event) : [...prev, event]))
  }

  return (
    <Dialog open={open} onOpenChange={onClose}>
      <DialogContent className="max-w-lg">
        <DialogHeader>
          <DialogTitle>{t("webhooks.new")}</DialogTitle>
          <DialogDescription>{t("webhooks.newDesc")}</DialogDescription>
        </DialogHeader>
        <form
          onSubmit={(e) => {
            e.preventDefault()
            onSubmit({ product_id: productId, url, events: selectedEvents })
          }}
          className="flex min-h-0 flex-1 flex-col gap-4"
        >
          <DialogBody className="space-y-4">
            <div className="space-y-2">
              <Label>{t("common.product")}</Label>
              <ProductSelect value={productId} onChange={setProductId} className="w-full" />
            </div>
            <div className="space-y-2">
              <Label>URL</Label>
              <Input
                type="url"
                value={url}
                onChange={(e) => setUrl(e.target.value)}
                placeholder="https://..."
                required
              />
            </div>
            <div className="space-y-2">
              <Label>{t("webhooks.events")}</Label>
              <div className="grid grid-cols-1 gap-2 sm:grid-cols-2">
                {WEBHOOK_EVENTS.map((event) => (
                  <label
                    key={event}
                    className="flex items-center gap-2 rounded border px-3 py-2 text-sm cursor-pointer hover:bg-accent"
                  >
                    <input
                      type="checkbox"
                      checked={selectedEvents.includes(event)}
                      onChange={() => toggleEvent(event)}
                      className="rounded"
                    />
                    {event}
                  </label>
                ))}
              </div>
            </div>
          </DialogBody>
          <DialogFooter>
            <Button type="button" variant="outline" onClick={onClose}>
              {t("common.cancel")}
            </Button>
            <Button type="submit" disabled={loading || selectedEvents.length === 0}>
              {loading ? t("common.loading") : t("common.create")}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

function SecretDialog({ secret, onClose }: { secret: string; onClose: () => void }) {
  const { t } = useI18n()
  const [copied, setCopied] = useState(false)
  const [visible, setVisible] = useState(false)

  const copy = () => {
    navigator.clipboard.writeText(secret)
    setCopied(true)
    setTimeout(() => setCopied(false), 2000)
  }

  return (
    <Dialog open onOpenChange={onClose}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>{t("webhooks.secretCreated")}</DialogTitle>
          <DialogDescription>{t("webhooks.secretDesc")}</DialogDescription>
        </DialogHeader>
        <DialogBody>
          <div className="space-y-4">
            <div className="flex items-center gap-2 bg-muted rounded-lg p-3">
              <code className="flex-1 text-sm break-all">
                {visible ? secret : `${secret.substring(0, 12)}...${"*".repeat(20)}`}
              </code>
              <Button variant="ghost" size="icon" className="shrink-0" onClick={() => setVisible(!visible)}>
                {visible ? <EyeOff className="h-4 w-4" /> : <Eye className="h-4 w-4" />}
              </Button>
              <Button variant="ghost" size="icon" className="shrink-0" onClick={copy}>
                {copied ? <Check className="h-4 w-4 text-emerald-600" /> : <Copy className="h-4 w-4" />}
              </Button>
            </div>
            <div className="flex justify-end">
              <Button onClick={onClose}>Done</Button>
            </div>
          </div>
        </DialogBody>
      </DialogContent>
    </Dialog>
  )
}

function DeliveryLogDialog({ webhookId, onClose }: { webhookId: string; onClose: () => void }) {
  const { t } = useI18n()
  const qc = useQueryClient()
  const [page, setPage] = useState(0)
  const [statusFilter, setStatusFilter] = useState<string>("")
  const limit = 20
  const { data, isLoading } = useQuery({
    queryKey: ["admin", "webhook-deliveries", webhookId, page, statusFilter],
    queryFn: () =>
      admin.listWebhookDeliveries(webhookId, {
        offset: page * limit,
        limit,
        status: statusFilter || undefined,
      }),
  })
  const resendMut = useMutation({
    mutationFn: (deliveryId: string) => admin.resendWebhookDelivery(webhookId, deliveryId),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["admin", "webhook-deliveries", webhookId] })
      showToast(t("webhooks.resendQueued"), "success")
    },
    onError: (err: Error) => {
      showToast(err.message || t("webhooks.resendFailed"), "error")
    },
  })

  const deliveries = data?.deliveries || []
  const total = data?.total || 0
  const totalPages = Math.ceil(total / limit)
  const [expandedId, setExpandedId] = useState<string | null>(null)

  return (
    <Dialog open onOpenChange={onClose}>
      <DialogContent className="max-w-3xl h-[min(760px,85vh)]">
        <DialogHeader>
          <DialogTitle>{t("webhooks.deliveries")}</DialogTitle>
          <DialogDescription>
            {total} {t("webhooks.deliveriesCount")}
          </DialogDescription>
        </DialogHeader>
        {/* Status filter: lets admins drill into failed deliveries
          without scrolling past every delivered row. It sits outside the
          scrolling body, so it stays in view and nothing clips it. The
          page resets on change so the first matching page shows. */}
        <div className="flex shrink-0 flex-wrap items-center gap-2">
          <span className="text-xs text-muted-foreground">{t("common.status")}</span>
          {(["", "pending", "delivered", "failed"] as const).map((s) => (
            <Button
              key={s || "all"}
              size="sm"
              variant={statusFilter === s ? "default" : "outline"}
              className="h-7 px-2.5 text-xs"
              onClick={() => {
                setStatusFilter(s)
                setPage(0)
              }}
            >
              {s === "" ? t("webhooks.statusAll") : t(`status.${s}` as const)}
            </Button>
          ))}
        </div>
        <DialogBody>
          {isLoading ? (
            <div className="h-48 animate-pulse bg-muted rounded-lg" />
          ) : deliveries.length === 0 ? (
            <p className="text-sm text-muted-foreground py-8 text-center">{t("webhooks.noDeliveries")}</p>
          ) : (
            <div className="space-y-4">
              {/* One card per delivery rather than a six column table: the
                fields wrap on a narrow screen instead of scrolling the row
                (and the Resend button) out of reach. */}
              <ul className="divide-y rounded-md border">
                {deliveries.map((d) => (
                  <Fragment key={d.id}>
                    <li>
                      <button
                        type="button"
                        className="flex w-full flex-wrap items-center gap-x-3 gap-y-1.5 px-3 py-2.5 text-left hover:bg-muted/50"
                        aria-expanded={expandedId === d.id}
                        onClick={() => setExpandedId(expandedId === d.id ? null : d.id)}
                      >
                        {expandedId === d.id ? (
                          <ChevronDown className="h-4 w-4 shrink-0" />
                        ) : (
                          <ChevronRight className="h-4 w-4 shrink-0" />
                        )}
                        <Badge variant="secondary" className="text-xs">
                          {d.event}
                        </Badge>
                        <Badge
                          className={
                            d.status === "delivered"
                              ? "bg-emerald-100 text-emerald-800"
                              : d.status === "failed"
                                ? "bg-red-100 text-red-800"
                                : "bg-amber-100 text-amber-800"
                          }
                        >
                          {t(`status.${d.status}` as TranslationKeys)}
                        </Badge>
                        <span className="text-xs text-muted-foreground">
                          {t("webhooks.response")} {d.response_code ?? "-"} · {t("webhooks.attempts")} {d.attempts}
                        </span>
                        <span className="ml-auto text-xs text-muted-foreground">{formatDate(d.delivered_at)}</span>
                      </button>
                      {expandedId === d.id && (
                        <div className="space-y-3 border-t bg-muted/20 px-3 py-3">
                          <div className="flex flex-wrap items-center justify-between gap-2">
                            <span className="text-[11px] font-mono text-muted-foreground break-all">id: {d.id}</span>
                            {/* Resend re-fires the same event payload as a
                              new delivery. Disabled while in flight so a slow
                              connection cannot double send. */}
                            <Button
                              size="sm"
                              variant="outline"
                              onClick={() => resendMut.mutate(d.id)}
                              disabled={resendMut.isPending}
                            >
                              <RefreshCw className="h-3 w-3 mr-1" />
                              {resendMut.isPending && resendMut.variables === d.id
                                ? t("common.loading")
                                : t("webhooks.resend")}
                            </Button>
                          </div>
                          {d.payload && (
                            <div>
                              <p className="text-xs font-medium text-muted-foreground mb-1">{t("webhooks.payload")}</p>
                              <pre className="text-xs bg-muted rounded p-2 overflow-auto max-h-48 whitespace-pre-wrap break-all">
                                {JSON.stringify(d.payload, null, 2)}
                              </pre>
                            </div>
                          )}
                          {d.response_body && (
                            <div>
                              <p className="text-xs font-medium text-muted-foreground mb-1">
                                {t("webhooks.responseBody")}
                              </p>
                              <pre className="text-xs bg-muted rounded p-2 overflow-auto max-h-48 whitespace-pre-wrap break-all">
                                {d.response_body}
                              </pre>
                            </div>
                          )}
                        </div>
                      )}
                    </li>
                  </Fragment>
                ))}
              </ul>
              {total > 0 && (
                <DataTablePagination
                  page={page}
                  totalPages={totalPages}
                  total={total}
                  pageSize={limit}
                  onPageChange={setPage}
                />
              )}
            </div>
          )}
        </DialogBody>
      </DialogContent>
    </Dialog>
  )
}

// An icon button whose name is its tooltip and its accessible label.
function IconAction({
  label,
  onClick,
  disabled,
  children,
}: {
  label: string
  onClick: () => void
  disabled?: boolean
  children: React.ReactNode
}) {
  return (
    <Button variant="ghost" size="icon" title={label} aria-label={label} onClick={onClick} disabled={disabled}>
      {children}
    </Button>
  )
}
