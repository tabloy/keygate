import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { Package, Pencil, Plus, Trash2 } from "lucide-react"
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
import { admin } from "@/lib/api"
import { boolColor, formatDate } from "@/lib/utils"

export default function AddonsPage() {
  const { t } = useI18n()
  const qc = useQueryClient()
  const [productFilter, setProductFilter] = useState<string>("")
  const [search, setSearch] = useState("")
  const { data: productsData } = useQuery({ queryKey: ["admin", "products"], queryFn: () => admin.listProducts() })
  const { data, isLoading } = useQuery({
    queryKey: ["admin", "addons", productFilter, search],
    queryFn: () => admin.listAddons(productFilter || undefined, search || undefined),
  })
  const [creating, setCreating] = useState(false)
  const [editing, setEditing] = useState<any>(null)
  const [deleting, setDeleting] = useState<any>(null)

  const products = productsData?.products || []
  const addons = data?.addons || []
  const { page, setPage, pageSize, setPageSize, total, totalPages, paginatedItems } = useClientPagination(addons, 10)

  const deleteMut = useMutation({
    mutationFn: (id: string) => admin.deleteAddon(id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["admin", "addons"] })
      setDeleting(null)
    },
  })

  if (!isLoading && products.length === 0) {
    return (
      <div className="space-y-6">
        <div>
          <h1 className="text-2xl font-bold tracking-tight">{t("addons.title")}</h1>
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
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-bold tracking-tight">{t("addons.title")}</h1>
          <p className="text-muted-foreground">{t("addons.subtitle")}</p>
        </div>
        <Button onClick={() => setCreating(true)}>
          <Plus className="h-4 w-4 mr-2" /> {t("addons.new")}
        </Button>
      </div>

      <div className="flex gap-4">
        <Select value={productFilter} onValueChange={(v) => setProductFilter(v === "all" ? "" : v)}>
          <SelectTrigger className="w-48">
            <SelectValue placeholder={t("filter.allProducts")} />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="all">{t("filter.allProducts")}</SelectItem>
            {products.map((p: any) => (
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
                  {paginatedItems.length === 0 && <DataTableEmpty colSpan={8} message={t("addons.empty")} />}
                  {paginatedItems.map((a: any) => (
                    <DataTableRow key={a.id}>
                      <DataTableCell className="font-medium">{a.name}</DataTableCell>
                      <DataTableCell className="text-muted-foreground">
                        {products.find((p: any) => p.id === a.product_id)?.name || a.product_id}
                      </DataTableCell>
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
                  page={page}
                  totalPages={totalPages}
                  total={total}
                  pageSize={pageSize}
                  onPageChange={setPage}
                  onPageSizeChange={setPageSize}
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
  products: any[]
  addon?: any
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

  const createMut = useMutation({
    mutationFn: () => (addon ? admin.updateAddon(addon.id, form) : admin.createAddon(form as any)),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["admin", "addons"] })
      if (!addon) showToast(t("toast.addonCreated"), "success")
      onClose()
    },
  })

  return (
    <Dialog open={open} onOpenChange={onClose}>
      <DialogContent className="max-w-lg max-h-[85vh] overflow-y-auto">
        <DialogHeader>
          <DialogTitle>{addon ? t("addons.edit") : t("addons.new")}</DialogTitle>
          <DialogDescription>{t("addons.formDesc")}</DialogDescription>
        </DialogHeader>
        <form
          onSubmit={(e) => {
            e.preventDefault()
            createMut.mutate()
          }}
          className="space-y-4"
        >
          <div className="grid grid-cols-2 gap-4">
            <div className="space-y-2 col-span-2">
              <Label>{t("common.product")}</Label>
              <Select value={form.product_id} onValueChange={(v) => set("product_id", v)}>
                <SelectTrigger>
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {products.map((p: any) => (
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
            <div className="space-y-2 col-span-2">
              <Label>{t("addons.description")}</Label>
              <Input value={form.description} onChange={(e) => set("description", e.target.value)} />
            </div>
            <div className="space-y-2">
              <Label>{t("plans.feature")}</Label>
              <Input value={form.feature} onChange={(e) => set("feature", e.target.value)} required />
            </div>
            <div className="space-y-2">
              <Label>{t("plans.valueType")}</Label>
              <Select value={form.value_type} onValueChange={(v) => set("value_type", v)}>
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
              <Input value={form.value} onChange={(e) => set("value", e.target.value)} required />
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
          <div className="flex justify-end gap-2 pt-2">
            <Button type="button" variant="outline" onClick={onClose}>
              {t("common.cancel")}
            </Button>
            <Button type="submit" disabled={createMut.isPending}>
              {createMut.isPending ? t("common.loading") : t("common.save")}
            </Button>
          </div>
        </form>
      </DialogContent>
    </Dialog>
  )
}
