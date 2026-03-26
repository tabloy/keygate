import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { Check, ChevronDown, ChevronRight, Copy, Eye, EyeOff, Package, Play, Plus, Trash2 } from "lucide-react"
import { useState } from "react"
import { Link } from "react-router-dom"
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
  useClientPagination,
} from "@/components/ui/data-table"
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select"
import { useI18n } from "@/i18n"
import { admin, type WebhookConfig } from "@/lib/api"
import { boolColor, formatDate } from "@/lib/utils"

const WEBHOOK_EVENTS = [
  "license.created",
  "license.canceled",
  "license.suspended",
  "license.reinstated",
  "license.revoked",
  "quota.warning",
  "quota.exceeded",
  "seat.added",
  "seat.removed",
  "plan.changed",
]

export default function WebhooksPage() {
  const { t } = useI18n()
  const qc = useQueryClient()
  const [productFilter, setProductFilter] = useState<string>("")
  const [search, setSearch] = useState("")
  const { data: productsData } = useQuery({ queryKey: ["admin", "products"], queryFn: () => admin.listProducts() })
  const { data, isLoading } = useQuery({
    queryKey: ["admin", "webhooks", productFilter, search],
    queryFn: () => admin.listWebhooks(productFilter || undefined, search || undefined),
  })
  const [creating, setCreating] = useState(false)
  const [newSecret, setNewSecret] = useState<string | null>(null)
  const [deleting, setDeleting] = useState<WebhookConfig | null>(null)
  const [viewingDeliveries, setViewingDeliveries] = useState<string | null>(null)

  const products = productsData?.products || []
  const webhooks = data?.webhooks || []
  const {
    page: wPage,
    setPage: wSetPage,
    pageSize: wPageSize,
    setPageSize: wSetPageSize,
    total: wTotal,
    totalPages: wTotalPages,
    paginatedItems: paginatedWebhooks,
  } = useClientPagination(webhooks, 10)

  const createMut = useMutation({
    mutationFn: admin.createWebhook,
    onSuccess: (data) => {
      qc.invalidateQueries({ queryKey: ["admin", "webhooks"] })
      setCreating(false)
      setNewSecret(data.secret)
      showToast(t("toast.webhookCreated"), "success")
    },
  })
  const toggleMut = useMutation({
    mutationFn: ({ id, active }: { id: string; active: boolean }) => admin.updateWebhook(id, { active }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["admin", "webhooks"] })
    },
  })
  const deleteMut = useMutation({
    mutationFn: (id: string) => admin.deleteWebhook(id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["admin", "webhooks"] })
      setDeleting(null)
    },
  })
  const testMut = useMutation({
    mutationFn: (id: string) => admin.testWebhook(id),
  })

  if (products.length === 0 && !isLoading) {
    return (
      <div className="space-y-6">
        <div>
          <h1 className="text-2xl font-bold tracking-tight">{t("webhooks.title")}</h1>
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
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-bold tracking-tight">{t("webhooks.title")}</h1>
          <p className="text-muted-foreground">{t("webhooks.subtitle")}</p>
        </div>
        <Button onClick={() => setCreating(true)}>
          <Plus className="h-4 w-4 mr-2" /> {t("webhooks.new")}
        </Button>
      </div>

      <div className="flex gap-4">
        <Select value={productFilter} onValueChange={(v) => setProductFilter(v === "all" ? "" : v)}>
          <SelectTrigger className="w-48">
            <SelectValue placeholder={t("filter.allProducts")} />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="all">{t("filter.allProducts")}</SelectItem>
            {products.map((p) => (
              <SelectItem key={p.id} value={p.id}>
                {p.name}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        <Input
          placeholder={t("common.search")}
          value={search}
          onChange={(e) => setSearch(e.target.value)}
          className="w-64"
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
                    <DataTableHead className="w-32" />
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
                        <div className="flex gap-1">
                          <Button
                            variant="ghost"
                            size="icon"
                            title={wh.active ? t("common.inactive") : t("common.active")}
                            onClick={() => toggleMut.mutate({ id: wh.id, active: !wh.active })}
                          >
                            {wh.active ? <Eye className="h-4 w-4" /> : <EyeOff className="h-4 w-4" />}
                          </Button>
                          <Button
                            variant="ghost"
                            size="icon"
                            title={t("webhooks.test")}
                            onClick={() => testMut.mutate(wh.id)}
                            disabled={testMut.isPending}
                          >
                            <Play className="h-4 w-4" />
                          </Button>
                          <Button
                            variant="ghost"
                            size="icon"
                            title={t("webhooks.deliveries")}
                            onClick={() => setViewingDeliveries(wh.id)}
                          >
                            <ChevronRight className="h-4 w-4" />
                          </Button>
                          <Button variant="ghost" size="icon" onClick={() => setDeleting(wh)}>
                            <Trash2 className="h-4 w-4 text-destructive" />
                          </Button>
                        </div>
                      </DataTableCell>
                    </DataTableRow>
                  ))}
                </DataTableBody>
              </DataTable>
              {wTotal > 0 && (
                <DataTablePagination
                  page={wPage}
                  totalPages={wTotalPages}
                  total={wTotal}
                  pageSize={wPageSize}
                  onPageChange={wSetPage}
                  onPageSizeChange={wSetPageSize}
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
      <DialogContent className="max-w-lg max-h-[85vh] overflow-y-auto">
        <DialogHeader>
          <DialogTitle>{t("webhooks.new")}</DialogTitle>
          <DialogDescription>{t("webhooks.newDesc")}</DialogDescription>
        </DialogHeader>
        <form
          onSubmit={(e) => {
            e.preventDefault()
            onSubmit({ product_id: productId, url, events: selectedEvents })
          }}
          className="space-y-4"
        >
          <div className="space-y-2">
            <Label>{t("common.product")}</Label>
            <Select value={productId} onValueChange={setProductId}>
              <SelectTrigger>
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {products.map((p) => (
                  <SelectItem key={p.id} value={p.id}>
                    {p.name}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
          <div className="space-y-2">
            <Label>URL</Label>
            <Input type="url" value={url} onChange={(e) => setUrl(e.target.value)} placeholder="https://..." required />
          </div>
          <div className="space-y-2">
            <Label>{t("webhooks.events")}</Label>
            <div className="grid grid-cols-2 gap-2">
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
          <div className="flex justify-end gap-2 pt-2">
            <Button type="button" variant="outline" onClick={onClose}>
              {t("common.cancel")}
            </Button>
            <Button type="submit" disabled={loading || selectedEvents.length === 0}>
              {loading ? t("common.loading") : t("common.create")}
            </Button>
          </div>
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
      </DialogContent>
    </Dialog>
  )
}

function DeliveryLogDialog({ webhookId, onClose }: { webhookId: string; onClose: () => void }) {
  const { t } = useI18n()
  const [page, setPage] = useState(0)
  const limit = 20
  const { data, isLoading } = useQuery({
    queryKey: ["admin", "webhook-deliveries", webhookId, page],
    queryFn: () => admin.listWebhookDeliveries(webhookId, { offset: page * limit, limit }),
  })

  const deliveries = data?.deliveries || []
  const total = data?.total || 0
  const totalPages = Math.ceil(total / limit)
  const [expandedId, setExpandedId] = useState<string | null>(null)

  return (
    <Dialog open onOpenChange={onClose}>
      <DialogContent className="max-w-3xl max-h-[85vh] overflow-y-auto">
        <DialogHeader>
          <DialogTitle>{t("webhooks.deliveries")}</DialogTitle>
          <DialogDescription>{total} deliveries total</DialogDescription>
        </DialogHeader>
        {isLoading ? (
          <div className="h-48 animate-pulse bg-muted rounded-lg" />
        ) : deliveries.length === 0 ? (
          <p className="text-sm text-muted-foreground py-8 text-center">{t("webhooks.noDeliveries")}</p>
        ) : (
          <div className="space-y-4">
            <DataTable>
              <DataTableHeader>
                <DataTableRow>
                  <DataTableHead className="w-8" />
                  <DataTableHead>{t("webhooks.event")}</DataTableHead>
                  <DataTableHead>{t("common.status")}</DataTableHead>
                  <DataTableHead>{t("webhooks.response")}</DataTableHead>
                  <DataTableHead>{t("webhooks.attempts")}</DataTableHead>
                  <DataTableHead>{t("webhooks.delivered")}</DataTableHead>
                </DataTableRow>
              </DataTableHeader>
              <DataTableBody>
                {deliveries.map((d) => (
                  <>
                    <DataTableRow
                      key={d.id}
                      className="cursor-pointer"
                      onClick={() => setExpandedId(expandedId === d.id ? null : d.id)}
                    >
                      <DataTableCell>
                        {expandedId === d.id ? (
                          <ChevronDown className="h-4 w-4" />
                        ) : (
                          <ChevronRight className="h-4 w-4" />
                        )}
                      </DataTableCell>
                      <DataTableCell>
                        <Badge variant="secondary" className="text-xs">
                          {d.event}
                        </Badge>
                      </DataTableCell>
                      <DataTableCell>
                        <Badge
                          className={
                            d.status === "delivered"
                              ? "bg-emerald-100 text-emerald-800"
                              : d.status === "failed"
                                ? "bg-red-100 text-red-800"
                                : "bg-amber-100 text-amber-800"
                          }
                        >
                          {t(`status.${d.status}` as any)}
                        </Badge>
                      </DataTableCell>
                      <DataTableCell className="text-muted-foreground">{d.response_code ?? "-"}</DataTableCell>
                      <DataTableCell className="text-muted-foreground">{d.attempts}</DataTableCell>
                      <DataTableCell className="text-muted-foreground text-xs">
                        {formatDate(d.delivered_at)}
                      </DataTableCell>
                    </DataTableRow>
                    {expandedId === d.id && (
                      <DataTableRow key={`${d.id}-detail`}>
                        <DataTableCell colSpan={6}>
                          <div className="space-y-2 p-2">
                            {d.payload && (
                              <div>
                                <p className="text-xs font-medium text-muted-foreground mb-1">Payload</p>
                                <pre className="text-xs bg-muted rounded p-2 overflow-auto max-h-40">
                                  {JSON.stringify(d.payload, null, 2)}
                                </pre>
                              </div>
                            )}
                            {d.response_body && (
                              <div>
                                <p className="text-xs font-medium text-muted-foreground mb-1">Response Body</p>
                                <pre className="text-xs bg-muted rounded p-2 overflow-auto max-h-40">
                                  {d.response_body}
                                </pre>
                              </div>
                            )}
                          </div>
                        </DataTableCell>
                      </DataTableRow>
                    )}
                  </>
                ))}
              </DataTableBody>
            </DataTable>
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
      </DialogContent>
    </Dialog>
  )
}
