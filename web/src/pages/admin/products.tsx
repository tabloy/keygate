import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { Cloud, Laptop, Layers, Pencil, Plus, Search, Trash2 } from "lucide-react"
import { useState } from "react"
import { HelpTip } from "@/components/help-tip"
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
import { admin, type Product } from "@/lib/api"
import { formatDate } from "@/lib/utils"

export default function ProductsPage() {
  const { t } = useI18n()
  const qc = useQueryClient()
  const [search, setSearch] = useState("")
  const pg = useServerPagination(10, [search])
  const { data, isLoading } = useQuery({
    queryKey: ["admin", "products", search, pg.page, pg.pageSize],
    queryFn: () => admin.listProducts({ search, ...pg.params }),
  })
  const [editing, setEditing] = useState<Product | null>(null)
  const [creating, setCreating] = useState(false)
  const [deleting, setDeleting] = useState<Product | null>(null)

  const createMut = useMutation({
    mutationFn: admin.createProduct,
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["admin", "products"] })
      setCreating(false)
      showToast(t("toast.productCreated"), "success")
    },
    onError: (e: Error) => showToast(e.message, "error"),
  })
  const updateMut = useMutation({
    mutationFn: ({ id, ...data }: Partial<Product> & { id: string }) => admin.updateProduct(id, data),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["admin", "products"] })
      setEditing(null)
    },
    onError: (e: Error) => showToast(e.message, "error"),
  })
  const deleteMut = useMutation({
    mutationFn: (id: string) => admin.deleteProduct(id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["admin", "products"] })
      setDeleting(null)
      showToast(t("toast.productDeleted"), "success")
    },
    onError: (e: Error) => showToast(e.message, "error"),
  })

  const { items: products, total, totalPages } = pg.from(data, data?.products)

  return (
    <div className="space-y-6">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div className="min-w-0">
          <h1 className="text-2xl font-bold tracking-tight sr-only md:not-sr-only">{t("products.title")}</h1>
          <p className="text-muted-foreground">{t("products.subtitle")}</p>
        </div>
        <Button onClick={() => setCreating(true)}>
          <Plus className="h-4 w-4 mr-2" /> {t("products.new")}
        </Button>
      </div>

      <div className="flex items-center gap-4">
        <div className="relative flex-1 max-w-sm">
          <Search className="absolute left-3 top-1/2 -translate-y-1/2 h-4 w-4 text-muted-foreground" />
          <Input
            placeholder={t("common.search")}
            value={search}
            onChange={(e) => {
              setSearch(e.target.value)
              pg.setPage(0)
            }}
            className="pl-9"
          />
        </div>
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
                    <DataTableHead>{t("products.slug")}</DataTableHead>
                    <DataTableHead>{t("common.type")}</DataTableHead>
                    <DataTableHead>{t("common.created")}</DataTableHead>
                    <DataTableHead className="w-24" />
                  </DataTableRow>
                </DataTableHeader>
                <DataTableBody>
                  {products.length === 0 && <DataTableEmpty colSpan={5} message={t("products.empty")} />}
                  {products.map((p) => (
                    <DataTableRow key={p.id}>
                      <DataTableCell className="font-medium">{p.name}</DataTableCell>
                      <DataTableCell>
                        <code className="text-xs bg-muted px-1.5 py-0.5 rounded">{p.slug}</code>
                      </DataTableCell>
                      <DataTableCell>
                        <Badge variant="secondary">{p.type}</Badge>
                      </DataTableCell>
                      <DataTableCell className="text-muted-foreground text-xs">
                        {formatDate(p.created_at)}
                      </DataTableCell>
                      <DataTableCell>
                        <div className="flex gap-1">
                          <Button variant="ghost" size="icon" onClick={() => setEditing(p)}>
                            <Pencil className="h-4 w-4" />
                          </Button>
                          <Button variant="ghost" size="icon" onClick={() => setDeleting(p)}>
                            <Trash2 className="h-4 w-4 text-destructive" />
                          </Button>
                        </div>
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

      {/* Create. Mounted only while open, so the form starts empty
          every time — the state lives in the component, not the
          dialog Radix unmounts. */}
      {creating && (
        <ProductDialog
          open
          onClose={() => setCreating(false)}
          onSubmit={(d) => createMut.mutate(d)}
          loading={createMut.isPending}
          title={t("products.createTitle")}
        />
      )}

      {/* Edit Dialog */}
      {editing && (
        <ProductDialog
          open
          onClose={() => setEditing(null)}
          product={editing}
          onSubmit={(d) => updateMut.mutate({ id: editing.id, ...d })}
          loading={updateMut.isPending}
          title={t("products.editTitle")}
        />
      )}

      {/* Delete Confirm */}
      <AlertDialog open={!!deleting} onOpenChange={() => setDeleting(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>
              {t("common.delete")} "{deleting?.name}"?
            </AlertDialogTitle>
            <AlertDialogDescription>{t("products.deleteConfirm")}</AlertDialogDescription>
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

function ProductDialog({
  open,
  onClose,
  product,
  onSubmit,
  loading,
  title,
}: {
  open: boolean
  onClose: () => void
  product?: Product
  onSubmit: (data: { name: string; slug: string; type: string; feed_license_required?: boolean }) => void
  loading: boolean
  title: string
}) {
  const { t } = useI18n()
  const [name, setName] = useState(product?.name || "")
  const [slug, setSlug] = useState(product?.slug || "")
  const [type, setType] = useState(product?.type || "desktop")
  const [feedLicenseRequired, setFeedLicenseRequired] = useState(product?.feed_license_required ?? false)

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault()
    onSubmit({ name, slug, type, feed_license_required: feedLicenseRequired })
  }

  return (
    <Dialog open={open} onOpenChange={onClose}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>{title}</DialogTitle>
          <DialogDescription>{t("products.formDesc")}</DialogDescription>
        </DialogHeader>
        <form onSubmit={handleSubmit} className="flex min-h-0 flex-1 flex-col gap-4">
          <DialogBody className="space-y-4">
            <div className="space-y-2">
              <Label>{t("common.name")}</Label>
              <Input
                value={name}
                onChange={(e) => {
                  setName(e.target.value)
                  if (!product)
                    setSlug(
                      e.target.value
                        .toLowerCase()
                        .replace(/[^a-z0-9]+/g, "-")
                        .replace(/(^-|-$)/g, ""),
                    )
                }}
                required
              />
            </div>
            <div className="space-y-2">
              <Label>{t("products.slug")}</Label>
              <Input value={slug} onChange={(e) => setSlug(e.target.value)} required />
            </div>
            <div className="space-y-2">
              <Label>{t("common.type")}</Label>
              <div className="grid grid-cols-1 gap-3 sm:grid-cols-3">
                <ProductTypeCard
                  value="desktop"
                  icon={Laptop}
                  title={t("products.desktop")}
                  description={t("products.desktopDesc")}
                  selected={type === "desktop"}
                  onSelect={() => setType("desktop")}
                />
                <ProductTypeCard
                  value="saas"
                  icon={Cloud}
                  title={t("products.saas")}
                  description={t("products.saasDesc")}
                  selected={type === "saas"}
                  onSelect={() => setType("saas")}
                />
                <ProductTypeCard
                  value="hybrid"
                  icon={Layers}
                  title={t("products.hybrid")}
                  description={t("products.hybridDesc")}
                  selected={type === "hybrid"}
                  onSelect={() => setType("hybrid")}
                />
              </div>
            </div>
            {/* Only products that ship releases have a feed to gate.
              Offered at creation too: a product that has published
              nothing handed out no feed, so gating it then costs no
              waiting at all. */}
            {type !== "saas" && (
              <div className="space-y-1">
                <div className="flex items-center gap-3">
                  <input
                    type="checkbox"
                    id="feed-license-required"
                    checked={feedLicenseRequired}
                    onChange={(e) => setFeedLicenseRequired(e.target.checked)}
                    className="h-4 w-4 rounded border-input accent-primary"
                  />
                  <Label htmlFor="feed-license-required">{t("products.feedLicenseRequired")}</Label>
                  {/* The rollout order and the header/token detail are
                    for the one operator setting this up, not for
                    everyone who opens the dialog: one line here, the
                    rest on hover. */}
                  <HelpTip text={t("products.feedLicenseRequiredHelp")} />
                </div>
                <p className="text-xs text-muted-foreground">{t("products.feedLicenseRequiredHint")}</p>
              </div>
            )}
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
      </DialogContent>
    </Dialog>
  )
}

// ProductTypeCard — the type picker visual. Clicking selects; the
// selected card gets a primary-colored ring. Mirrors the backend
// capability map: each card's description tells admins what the type
// actually enables/disables before they commit.
function ProductTypeCard({
  icon: Icon,
  title,
  description,
  selected,
  onSelect,
}: {
  value: string
  icon: typeof Laptop
  title: string
  description: string
  selected: boolean
  onSelect: () => void
}) {
  return (
    <button
      type="button"
      onClick={onSelect}
      className={
        "flex flex-col items-start gap-2 rounded-md border p-3 text-left transition-colors " +
        (selected ? "border-primary ring-2 ring-primary bg-primary/5" : "border-input hover:bg-muted/40")
      }
    >
      <Icon className={`h-5 w-5 ${selected ? "text-primary" : "text-muted-foreground"}`} />
      <div className="text-sm font-medium">{title}</div>
      <div className="text-xs text-muted-foreground">{description}</div>
    </button>
  )
}
