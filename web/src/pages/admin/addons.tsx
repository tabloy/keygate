import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { Package, Pencil, Plus, Trash2 } from "lucide-react"
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
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select"
import { useI18n } from "@/i18n"
import { type Addon, admin, type Product } from "@/lib/api"
import { boolColor, formatDate } from "@/lib/utils"

export default function AddonsPage() {
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
    queryKey: ["admin", "addons", productFilter, search, pg.page, pg.pageSize],
    queryFn: () => admin.listAddons({ product_id: productFilter, search, ...pg.params }),
  })
  const [creating, setCreating] = useState(false)
  const [editing, setEditing] = useState<Addon | null>(null)
  const [deleting, setDeleting] = useState<Addon | null>(null)

  const products = productsData?.products || []
  const { items: addons, total, totalPages } = pg.from(data, data?.addons)

  const deleteMut = useMutation({
    mutationFn: (id: string) => admin.deleteAddon(id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["admin", "addons"] })
      setDeleting(null)
    },
    onError: (e: Error) => showToast(e.message, "error"),
  })

  if (!isLoading && products.length === 0) {
    return (
      <div className="space-y-6">
        <div>
          <h1 className="text-2xl font-bold tracking-tight sr-only md:not-sr-only">{t("addons.title")}</h1>
          <p className="text-muted-foreground">{t("addons.subtitle")}</p>
        </div>
        <Card>
          <CardContent className="py-12 text-center">
            <Package className="h-12 w-12 mx-auto text-muted-foreground mb-4" />
            <p className="text-lg font-medium">{t("licenses.noProducts")}</p>
            <p className="text-muted-foreground mt-1 mb-4">{t("licenses.noProductsDesc")}</p>
            <Button asChild>
              <Link to="/admin/products">
                <Plus className="h-4 w-4 mr-2" /> {t("products.new")}
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
          <h1 className="text-2xl font-bold tracking-tight sr-only md:not-sr-only">{t("addons.title")}</h1>
          <p className="text-muted-foreground">{t("addons.subtitle")}</p>
        </div>
        <Button onClick={() => setCreating(true)}>
          <Plus className="h-4 w-4 mr-2" /> {t("addons.new")}
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
                    <DataTableHead>{t("common.name")}</DataTableHead>
                    <DataTableHead>{t("common.product")}</DataTableHead>
                    <DataTableHead>{t("plans.feature")}</DataTableHead>
                    <DataTableHead>{t("plans.valueType")}</DataTableHead>
                    <DataTableHead>{t("plans.value")}</DataTableHead>
                    <DataTableHead>{t("common.status")}</DataTableHead>
                    <DataTableHead>{t("common.created")}</DataTableHead>
                    <DataTableHead className="w-24" />
                  </DataTableRow>
                </DataTableHeader>
                <DataTableBody>
                  {addons.length === 0 && <DataTableEmpty colSpan={8} message={t("addons.empty")} />}
                  {addons.map((a: Addon) => (
                    <DataTableRow key={a.id}>
                      <DataTableCell className="font-medium">{a.name}</DataTableCell>
                      <DataTableCell className="text-muted-foreground">{a.product?.name || a.product_id}</DataTableCell>
                      <DataTableCell>{a.feature}</DataTableCell>
                      <DataTableCell>
                        <Badge variant="secondary">{a.value_type}</Badge>
                      </DataTableCell>
                      <DataTableCell>{a.value}</DataTableCell>
                      <DataTableCell>
                        <Badge className={boolColor(a.active)}>
                          {a.active ? t("common.active") : t("common.inactive")}
                        </Badge>
                      </DataTableCell>
                      <DataTableCell className="text-muted-foreground">{formatDate(a.created_at)}</DataTableCell>
                      <DataTableCell>
                        <div className="flex gap-1">
                          <Button variant="ghost" size="icon" onClick={() => setEditing(a)}>
                            <Pencil className="h-4 w-4" />
                          </Button>
                          <Button variant="ghost" size="icon" onClick={() => setDeleting(a)}>
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

      {creating && <AddonDialog open onClose={() => setCreating(false)} products={products} />}
      {editing && <AddonDialog open onClose={() => setEditing(null)} products={products} addon={editing} />}

      <AlertDialog open={!!deleting} onOpenChange={() => setDeleting(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>
              {t("common.delete")} "{deleting?.name}"?
            </AlertDialogTitle>
            <AlertDialogDescription>{t("addons.deleteConfirm")}</AlertDialogDescription>
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

function AddonDialog({
  open,
  onClose,
  products,
  addon,
}: {
  open: boolean
  onClose: () => void
  products: Product[]
  addon?: Addon
}) {
  const { t } = useI18n()
  const qc = useQueryClient()
  const [form, setForm] = useState({
    product_id: addon?.product_id || products[0]?.id || "",
    name: addon?.name || "",
    slug: addon?.slug || "",
    description: addon?.description || "",
    feature: addon?.feature || "",
    value_type: addon?.value_type || "bool",
    value: addon?.value || "true",
    quota_period: addon?.quota_period || "",
    quota_unit: addon?.quota_unit || "",
  })

  const set = (k: string, v: string) => setForm((f) => ({ ...f, [k]: v }))

  // The value's shape follows the type, and the server refuses a pair
  // that does not match (a quota that is not a number would read as
  // "no limit"). Carry the value across the switch rather than
  // leaving the previous type's text behind.
  const setValueType = (v: string) =>
    setForm((f) => ({
      ...f,
      value_type: v,
      value: v === "bool" ? (f.value === "false" ? "false" : "true") : /^\d+$/.test(f.value) ? f.value : "",
      quota_period: v === "quota" ? f.quota_period || "monthly" : "",
    }))

  // Without onError a rejected save looks like nothing happening at
  // all: the dialog stays open, the form keeps its values and the
  // reason the server gave — a slug that is taken, a quota value that
  // is not a number — never reaches the screen.
  const createMut = useMutation({
    mutationFn: () => (addon ? admin.updateAddon(addon.id, form) : admin.createAddon(form)),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["admin", "addons"] })
      if (!addon) showToast(t("toast.addonCreated"), "success")
      onClose()
    },
    onError: (e: Error) => showToast(e.message, "error"),
  })

  return (
    <Dialog open={open} onOpenChange={onClose}>
      <DialogContent className="max-w-lg">
        <DialogHeader>
          <DialogTitle>{addon ? t("addons.edit") : t("addons.new")}</DialogTitle>
          <DialogDescription>{t("addons.formDesc")}</DialogDescription>
        </DialogHeader>
        <form
          onSubmit={(e) => {
            e.preventDefault()
            createMut.mutate()
          }}
          className="flex min-h-0 flex-1 flex-col gap-4"
        >
          <DialogBody className="space-y-4">
            <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
              <div className="space-y-2 sm:col-span-2">
                <Label>{t("common.product")}</Label>
                {/* An addon cannot change product: licences hold it, and
                  the update endpoint does not accept the field. Shown
                  read-only rather than as a control that saves nothing. */}
                <ProductSelect
                  value={form.product_id}
                  onChange={(v) => set("product_id", v)}
                  current={addon?.product}
                  className="w-full"
                  disabled={!!addon}
                />
              </div>
              <div className="space-y-2">
                <Label>{t("common.name")}</Label>
                <Input
                  value={form.name}
                  onChange={(e) => {
                    set("name", e.target.value)
                    if (!addon) set("slug", e.target.value.toLowerCase().replace(/[^a-z0-9]+/g, "-"))
                  }}
                  required
                />
              </div>
              <div className="space-y-2">
                <Label>{t("products.slug")}</Label>
                <Input value={form.slug} onChange={(e) => set("slug", e.target.value)} required />
              </div>
              <div className="space-y-2 sm:col-span-2">
                <Label>{t("addons.description")}</Label>
                <Input value={form.description} onChange={(e) => set("description", e.target.value)} />
              </div>
              <div className="space-y-2">
                <Label>{t("plans.feature")}</Label>
                <Input value={form.feature} onChange={(e) => set("feature", e.target.value)} required />
              </div>
              <div className="space-y-2">
                <Label>{t("plans.valueType")}</Label>
                <Select value={form.value_type} onValueChange={setValueType}>
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
                <Label>{t("plans.value")}</Label>
                {form.value_type === "bool" ? (
                  <Select value={form.value} onValueChange={(v) => set("value", v)}>
                    <SelectTrigger>
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      <SelectItem value="true">{t("addons.valueTrue")}</SelectItem>
                      <SelectItem value="false">{t("addons.valueFalse")}</SelectItem>
                    </SelectContent>
                  </Select>
                ) : (
                  <Input
                    value={form.value}
                    onChange={(e) => set("value", e.target.value)}
                    type={form.value_type === "int" || form.value_type === "quota" ? "number" : "text"}
                    min={0}
                    step={1}
                    required
                  />
                )}
                {form.value_type === "quota" && (
                  <p className="text-xs text-muted-foreground">{t("addons.quotaZeroHint")}</p>
                )}
              </div>
              {form.value_type === "quota" && (
                <>
                  <div className="space-y-2">
                    <Label>{t("plans.quotaPeriod")}</Label>
                    <Select value={form.quota_period} onValueChange={(v) => set("quota_period", v)}>
                      <SelectTrigger>
                        <SelectValue />
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
                    <Label>{t("plans.quotaUnit")}</Label>
                    <Input value={form.quota_unit} onChange={(e) => set("quota_unit", e.target.value)} />
                  </div>
                </>
              )}
            </div>
          </DialogBody>
          <DialogFooter>
            <Button type="button" variant="outline" onClick={onClose}>
              {t("common.cancel")}
            </Button>
            <Button type="submit" disabled={createMut.isPending}>
              {createMut.isPending ? t("common.loading") : t("common.save")}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}
