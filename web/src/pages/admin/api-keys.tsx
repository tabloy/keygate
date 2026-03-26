import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { Check, Copy, Eye, EyeOff, Package, Plus, Trash2 } from "lucide-react"
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
import { admin } from "@/lib/api"
import { formatDate } from "@/lib/utils"

export default function APIKeysPage() {
  const { t } = useI18n()
  const qc = useQueryClient()
  const [productFilter, setProductFilter] = useState<string>("")
  const [search, setSearch] = useState("")
  const { data: productsData } = useQuery({ queryKey: ["admin", "products"], queryFn: () => admin.listProducts() })
  const { data, isLoading } = useQuery({
    queryKey: ["admin", "api-keys", productFilter, search],
    queryFn: () => admin.listAPIKeys(productFilter || undefined, search || undefined),
  })
  const [creating, setCreating] = useState(false)
  const [newKey, setNewKey] = useState<string | null>(null)
  const [deleting, setDeleting] = useState<{ id: string; name: string } | null>(null)

  const products = productsData?.products || []
  const keys = data?.api_keys || []
  const { page, setPage, pageSize, setPageSize, total, totalPages, paginatedItems } = useClientPagination(keys, 10)

  const createMut = useMutation({
    mutationFn: admin.createAPIKey,
    onSuccess: (data) => {
      qc.invalidateQueries({ queryKey: ["admin", "api-keys"] })
      setCreating(false)
      setNewKey(data.key)
      showToast(t("toast.apiKeyCreated"), "success")
    },
  })
  const deleteMut = useMutation({
    mutationFn: (id: string) => admin.deleteAPIKey(id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["admin", "api-keys"] })
      setDeleting(null)
    },
  })

  if (products.length === 0 && !isLoading) {
    return (
      <div className="space-y-6">
        <div>
          <h1 className="text-2xl font-bold tracking-tight">{t("apiKeys.title")}</h1>
          <p className="text-muted-foreground">{t("apiKeys.subtitle")}</p>
        </div>
        <Card>
          <CardContent className="py-12 text-center">
            <Package className="h-12 w-12 mx-auto text-muted-foreground mb-4" />
            <p className="text-lg font-medium">{t("licenses.noProducts")}</p>
            <p className="text-muted-foreground mt-1 mb-4">{t("apiKeys.noProductsDesc")}</p>
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
          <h1 className="text-2xl font-bold tracking-tight">{t("apiKeys.title")}</h1>
          <p className="text-muted-foreground">{t("apiKeys.subtitle")}</p>
        </div>
        <Button onClick={() => setCreating(true)}>
          <Plus className="h-4 w-4 mr-2" /> {t("apiKeys.new")}
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
            <DataTable>
              <DataTableHeader>
                <DataTableRow>
                  <DataTableHead>{t("common.name")}</DataTableHead>
                  <DataTableHead>{t("common.product")}</DataTableHead>
                  <DataTableHead>{t("apiKeys.prefix")}</DataTableHead>
                  <DataTableHead>{t("apiKeys.lastUsed")}</DataTableHead>
                  <DataTableHead>{t("common.created")}</DataTableHead>
                  <DataTableHead className="w-16" />
                </DataTableRow>
              </DataTableHeader>
              <DataTableBody>
                {paginatedItems.length === 0 && <DataTableEmpty colSpan={6} message={t("apiKeys.empty")} />}
                {paginatedItems.map((k) => (
                  <DataTableRow key={k.id}>
                    <DataTableCell className="font-medium">{k.name}</DataTableCell>
                    <DataTableCell className="text-muted-foreground">{k.product?.name || "-"}</DataTableCell>
                    <DataTableCell>
                      <code className="text-xs bg-muted px-1.5 py-0.5 rounded">{k.prefix}...</code>
                    </DataTableCell>
                    <DataTableCell className="text-muted-foreground text-xs">{formatDate(k.last_used)}</DataTableCell>
                    <DataTableCell className="text-muted-foreground text-xs">{formatDate(k.created_at)}</DataTableCell>
                    <DataTableCell>
                      <Button variant="ghost" size="icon" onClick={() => setDeleting({ id: k.id, name: k.name })}>
                        <Trash2 className="h-4 w-4 text-destructive" />
                      </Button>
                    </DataTableCell>
                  </DataTableRow>
                ))}
              </DataTableBody>
            </DataTable>
          )}
        </CardContent>
      </Card>

      {total > 0 && (
        <DataTablePagination
          page={page}
          totalPages={totalPages}
          total={total}
          pageSize={pageSize}
          onPageChange={setPage}
          onPageSizeChange={setPageSize}
        />
      )}

      {/* Create */}
      {creating && (
        <CreateAPIKeyDialog
          open
          onClose={() => setCreating(false)}
          products={products}
          onSubmit={(d) => createMut.mutate(d)}
          loading={createMut.isPending}
        />
      )}

      {/* Show new key */}
      {newKey && <NewKeyDialog keyValue={newKey} onClose={() => setNewKey(null)} />}

      {/* Delete */}
      <AlertDialog open={!!deleting} onOpenChange={() => setDeleting(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>
              {t("common.delete")} "{deleting?.name}"?
            </AlertDialogTitle>
            <AlertDialogDescription>{t("apiKeys.deleteConfirm")}</AlertDialogDescription>
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

function CreateAPIKeyDialog({
  open,
  onClose,
  products,
  onSubmit,
  loading,
}: {
  open: boolean
  onClose: () => void
  products: { id: string; name: string }[]
  onSubmit: (d: { product_id: string; name: string }) => void
  loading: boolean
}) {
  const { t } = useI18n()
  const [productId, setProductId] = useState(products[0]?.id || "")
  const [name, setName] = useState("")

  return (
    <Dialog open={open} onOpenChange={onClose}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>{t("apiKeys.new")}</DialogTitle>
          <DialogDescription>{t("apiKeys.subtitle")}</DialogDescription>
        </DialogHeader>
        <form
          onSubmit={(e) => {
            e.preventDefault()
            onSubmit({ product_id: productId, name })
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
            <Label>{t("common.name")}</Label>
            <Input
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="e.g. Production, Development"
              required
            />
          </div>
          <div className="flex justify-end gap-2 pt-2">
            <Button type="button" variant="outline" onClick={onClose}>
              {t("common.cancel")}
            </Button>
            <Button type="submit" disabled={loading}>
              {loading ? t("common.loading") : t("common.create")}
            </Button>
          </div>
        </form>
      </DialogContent>
    </Dialog>
  )
}

function NewKeyDialog({ keyValue, onClose }: { keyValue: string; onClose: () => void }) {
  const { t } = useI18n()
  const [copied, setCopied] = useState(false)
  const [visible, setVisible] = useState(false)

  const copy = () => {
    navigator.clipboard.writeText(keyValue)
    setCopied(true)
    setTimeout(() => setCopied(false), 2000)
  }

  return (
    <Dialog open onOpenChange={onClose}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>{t("apiKeys.created")}</DialogTitle>
          <DialogDescription>{t("apiKeys.createdDesc")}</DialogDescription>
        </DialogHeader>
        <div className="space-y-4">
          <div className="flex items-center gap-2 bg-muted rounded-lg p-3">
            <code className="flex-1 text-sm break-all">
              {visible ? keyValue : `${keyValue.substring(0, 12)}...${"*".repeat(20)}`}
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
