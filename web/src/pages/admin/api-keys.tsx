import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { Check, Copy, Eye, EyeOff, Package, Plus, RefreshCw, Trash2 } from "lucide-react"
import { useState } from "react"
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
import { useI18n } from "@/i18n"
import { admin } from "@/lib/api"
import { formatDate } from "@/lib/utils"

export default function APIKeysPage() {
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
    queryKey: ["admin", "api-keys", productFilter, search, pg.page, pg.pageSize],
    queryFn: () => admin.listAPIKeys({ product_id: productFilter, search, ...pg.params }),
  })
  const [creating, setCreating] = useState(false)
  const [newKey, setNewKey] = useState<string | null>(null)
  const [deleting, setDeleting] = useState<{ id: string; name: string } | null>(null)
  const [rotating, setRotating] = useState<{ id: string; name: string } | null>(null)

  const products = productsData?.products || []
  const { items: keys, total, totalPages } = pg.from(data, data?.api_keys)

  const createMut = useMutation({
    mutationFn: admin.createAPIKey,
    onSuccess: (data) => {
      qc.invalidateQueries({ queryKey: ["admin", "api-keys"] })
      setCreating(false)
      setNewKey(data.key)
      showToast(t("toast.apiKeyCreated"), "success")
    },
    onError: (e: Error) => showToast(e.message, "error"),
  })
  const deleteMut = useMutation({
    mutationFn: (id: string) => admin.deleteAPIKey(id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["admin", "api-keys"] })
      setDeleting(null)
    },
    onError: (e: Error) => showToast(e.message, "error"),
  })
  const rotateMut = useMutation({
    mutationFn: (id: string) => admin.rotateAPIKey(id),
    onSuccess: (data) => {
      qc.invalidateQueries({ queryKey: ["admin", "api-keys"] })
      setRotating(null)
      // Same NewKeyDialog used for create — admins copy the new
      // secret immediately or lose it.
      setNewKey(data.key)
      showToast(`${t("apiKeys.rotate")} ✓`, "success")
    },
    onError: (e: Error) => showToast(e.message, "error"),
  })

  if (products.length === 0 && !isLoading) {
    return (
      <div className="space-y-6">
        <div>
          <h1 className="text-2xl font-bold tracking-tight sr-only md:not-sr-only">{t("apiKeys.title")}</h1>
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
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div className="min-w-0">
          <h1 className="text-2xl font-bold tracking-tight sr-only md:not-sr-only">{t("apiKeys.title")}</h1>
          <p className="text-muted-foreground">{t("apiKeys.subtitle")}</p>
        </div>
        <Button onClick={() => setCreating(true)}>
          <Plus className="h-4 w-4 mr-2" /> {t("apiKeys.new")}
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
            <DataTable>
              <DataTableHeader>
                <DataTableRow>
                  <DataTableHead>{t("common.name")}</DataTableHead>
                  <DataTableHead>{t("apiKeys.scopes")}</DataTableHead>
                  <DataTableHead>{t("apiKeys.prefix")}</DataTableHead>
                  <DataTableHead>{t("apiKeys.lastUsed")}</DataTableHead>
                  <DataTableHead>{t("common.created")}</DataTableHead>
                  <DataTableHead className="w-24" />
                </DataTableRow>
              </DataTableHeader>
              <DataTableBody>
                {keys.length === 0 && <DataTableEmpty colSpan={6} message={t("apiKeys.empty")} />}
                {keys.map((k) => (
                  <DataTableRow key={k.id}>
                    <DataTableCell className="font-medium">{k.name}</DataTableCell>
                    <DataTableCell>
                      <div className="flex flex-wrap gap-1">
                        {(k.scopes || []).length === 0 && (
                          <span className="text-xs text-muted-foreground italic">{t("apiKeys.scopesNone")}</span>
                        )}
                        {(k.scopes || []).map((s) => (
                          <Badge key={s} variant="secondary" className="text-xs">
                            {s}
                          </Badge>
                        ))}
                      </div>
                    </DataTableCell>
                    <DataTableCell>
                      <code className="text-xs bg-muted px-1.5 py-0.5 rounded">{k.prefix}...</code>
                    </DataTableCell>
                    <DataTableCell className="text-muted-foreground text-xs">
                      {k.last_used ? (
                        <div className="space-y-0.5">
                          <div>{formatDate(k.last_used)}</div>
                          {k.last_used_ip && (
                            <div className="text-[10px] opacity-70">
                              {t("apiKeys.lastUsedIP")} {k.last_used_ip}
                            </div>
                          )}
                        </div>
                      ) : (
                        "-"
                      )}
                    </DataTableCell>
                    <DataTableCell className="text-muted-foreground text-xs">{formatDate(k.created_at)}</DataTableCell>
                    <DataTableCell>
                      <div className="flex gap-1">
                        <Button
                          variant="ghost"
                          size="icon"
                          title={t("apiKeys.rotate")}
                          onClick={() => setRotating({ id: k.id, name: k.name })}
                        >
                          <RefreshCw className="h-4 w-4" />
                        </Button>
                        <Button variant="ghost" size="icon" onClick={() => setDeleting({ id: k.id, name: k.name })}>
                          <Trash2 className="h-4 w-4 text-destructive" />
                        </Button>
                      </div>
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
          page={pg.page}
          totalPages={totalPages}
          total={total}
          pageSize={pg.pageSize}
          onPageChange={pg.setPage}
          onPageSizeChange={pg.setPageSize}
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

      {/* Rotate */}
      <AlertDialog open={!!rotating} onOpenChange={() => setRotating(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>
              {t("apiKeys.rotateTitle")} — "{rotating?.name}"
            </AlertDialogTitle>
            <AlertDialogDescription>{t("apiKeys.rotateConfirm")}</AlertDialogDescription>
          </AlertDialogHeader>
          <div className="flex justify-end gap-2">
            <AlertDialogCancel>{t("common.cancel")}</AlertDialogCancel>
            <AlertDialogAction onClick={() => rotating && rotateMut.mutate(rotating.id)}>
              {t("apiKeys.rotate")}
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

// Scope catalogue. Order = display order in the dialog. Keep in
// sync with model.AllScopes() on the server — new scopes need a row
// here so admins can grant them via UI, not just curl.
const SCOPE_OPTIONS: {
  value: string
  labelKey: "apiKeys.scopeAdmin" | "apiKeys.scopeLicensesWrite" | "apiKeys.scopeReleasesWrite"
  descKey: "apiKeys.scopeAdminDesc" | "apiKeys.scopeLicensesWriteDesc" | "apiKeys.scopeReleasesWriteDesc"
}[] = [
  { value: "admin", labelKey: "apiKeys.scopeAdmin", descKey: "apiKeys.scopeAdminDesc" },
  { value: "licenses:write", labelKey: "apiKeys.scopeLicensesWrite", descKey: "apiKeys.scopeLicensesWriteDesc" },
  { value: "releases:write", labelKey: "apiKeys.scopeReleasesWrite", descKey: "apiKeys.scopeReleasesWriteDesc" },
]

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
  onSubmit: (d: { product_id?: string; name: string; scopes: string[] }) => void
  loading: boolean
}) {
  const { t } = useI18n()
  // Default: admin scope checked. Most admins minting their first
  // key want the wildcard. Granular scopes are an opt-in for CI/CD
  // or merchant backends where blast-radius matters.
  const [scopes, setScopes] = useState<string[]>(["admin"])
  const [productId, setProductId] = useState(products[0]?.id || "")
  const [name, setName] = useState("")
  const [error, setError] = useState("")

  // The server rejects unknown scopes and empty scope arrays produce
  // a fail-closed key. Block submit client-side so the user gets a
  // pointer rather than a stale-looking 400.
  const canSubmit = !!name.trim() && scopes.length > 0

  const toggleScope = (value: string) => {
    setError("")
    setScopes((prev) => (prev.includes(value) ? prev.filter((s) => s !== value) : [...prev, value]))
  }

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
            if (scopes.length === 0) {
              setError(t("apiKeys.atLeastOneScope"))
              return
            }
            // product_id is informational/scoping for now — the
            // server only treats it as a free-text foreign key.
            // Leave it unset for system-wide keys (the common case
            // for admin / *:write scopes).
            onSubmit({
              product_id: productId || undefined,
              name: name.trim(),
              scopes,
            })
          }}
          className="flex min-h-0 flex-1 flex-col gap-4"
        >
          <DialogBody className="space-y-4">
            <div className="space-y-2">
              <Label>{t("apiKeys.scopes")}</Label>
              <div className="rounded-md border divide-y">
                {SCOPE_OPTIONS.map((opt) => (
                  <label key={opt.value} className="flex items-start gap-3 px-3 py-2 cursor-pointer hover:bg-muted/40">
                    <input
                      type="checkbox"
                      checked={scopes.includes(opt.value)}
                      onChange={() => toggleScope(opt.value)}
                      className="mt-1 h-4 w-4 accent-primary"
                    />
                    <div className="flex-1">
                      <p className="text-sm font-medium">
                        {t(opt.labelKey)}{" "}
                        <code className="text-[10px] text-muted-foreground bg-muted px-1 rounded">{opt.value}</code>
                      </p>
                      <p className="text-xs text-muted-foreground mt-0.5">{t(opt.descKey)}</p>
                    </div>
                  </label>
                ))}
              </div>
              {error && <p className="text-xs text-destructive">{error}</p>}
            </div>

            {/* Product binding is optional and mostly informational —
              shown only when no admin scope is granted, since admin
              keys span products anyway. */}
            {!scopes.includes("admin") && products.length > 0 && (
              <div className="space-y-2">
                <Label>
                  {t("common.product")} ({t("common.optional")})
                </Label>
                <ProductSelect value={productId} onChange={setProductId} noneLabel="—" className="w-full" />
              </div>
            )}

            <div className="space-y-2">
              <Label>{t("common.name")}</Label>
              <Input
                value={name}
                onChange={(e) => setName(e.target.value)}
                placeholder="e.g. CI provisioning, Acme prod backend"
                required
              />
            </div>
          </DialogBody>
          <DialogFooter>
            <Button type="button" variant="outline" onClick={onClose}>
              {t("common.cancel")}
            </Button>
            <Button type="submit" disabled={loading || !canSubmit}>
              {loading ? t("common.loading") : t("common.create")}
            </Button>
          </DialogFooter>
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
        <DialogBody>
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
        </DialogBody>
      </DialogContent>
    </Dialog>
  )
}
