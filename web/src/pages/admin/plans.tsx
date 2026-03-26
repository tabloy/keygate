import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { Package, Pencil, Plus, Trash2 } from "lucide-react"
import { useEffect, useState } from "react"
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
import { Separator } from "@/components/ui/separator"
import { useI18n } from "@/i18n"
import { admin, type Entitlement, type Plan } from "@/lib/api"
import { boolColor, formatDate } from "@/lib/utils"

export default function PlansPage() {
  const { t } = useI18n()
  const qc = useQueryClient()
  const [productFilter, setProductFilter] = useState<string>("")
  const [search, setSearch] = useState("")
  const { data: productsData } = useQuery({ queryKey: ["admin", "products"], queryFn: () => admin.listProducts() })
  const { data, isLoading } = useQuery({
    queryKey: ["admin", "plans", productFilter, search],
    queryFn: () => admin.listPlans(productFilter || undefined, search || undefined),
  })
  const [creating, setCreating] = useState(false)
  const [editing, setEditing] = useState<Plan | null>(null)
  const [deleting, setDeleting] = useState<Plan | null>(null)

  const products = productsData?.products || []
  const plans = data?.plans || []
  const { page, setPage, pageSize, setPageSize, total, totalPages, paginatedItems } = useClientPagination(plans, 10)

  const createMut = useMutation({
    mutationFn: admin.createPlan,
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["admin", "plans"] })
      setCreating(false)
      showToast(t("toast.planCreated"), "success")
    },
  })
  const updateMut = useMutation({
    mutationFn: ({ id, ...data }: Partial<Plan> & { id: string }) => admin.updatePlan(id, data),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["admin", "plans"] })
      setEditing(null)
    },
  })
  const deleteMut = useMutation({
    mutationFn: (id: string) => admin.deletePlan(id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["admin", "plans"] })
      setDeleting(null)
      showToast(t("toast.planDeleted"), "success")
    },
  })

  if (!isLoading && products.length === 0) {
    return (
      <div className="space-y-6">
        <div>
          <h1 className="text-2xl font-bold tracking-tight">{t("plans.title")}</h1>
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
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-bold tracking-tight">{t("plans.title")}</h1>
          <p className="text-muted-foreground">{t("plans.subtitle")}</p>
        </div>
        <Button onClick={() => setCreating(true)}>
          <Plus className="h-4 w-4 mr-2" /> {t("plans.new")}
        </Button>
      </div>

      <div className="flex gap-4">
        <Input
          placeholder={t("common.search")}
          value={search}
          onChange={(e) => setSearch(e.target.value)}
          className="w-64"
        />
        <Select value={productFilter} onValueChange={setProductFilter}>
          <SelectTrigger className="w-48">
            <SelectValue placeholder={t("plans.allProducts")} />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="all">{t("plans.allProducts")}</SelectItem>
            {products.map((p) => (
              <SelectItem key={p.id} value={p.id}>
                {p.name}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
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
                    <DataTableHead>{t("common.status")}</DataTableHead>
                    <DataTableHead>{t("common.created")}</DataTableHead>
                    <DataTableHead className="w-24" />
                  </DataTableRow>
                </DataTableHeader>
                <DataTableBody>
                  {plans.length === 0 && <DataTableEmpty colSpan={7} message={t("plans.empty")} />}
                  {paginatedItems.map((p) => (
                    <DataTableRow key={p.id}>
                      <DataTableCell className="font-medium">{p.name}</DataTableCell>
                      <DataTableCell className="text-muted-foreground">
                        {products.find((pr) => pr.id === p.product_id)?.name || p.product_id}
                      </DataTableCell>
                      <DataTableCell>
                        <Badge variant="secondary">{p.license_type}</Badge>
                      </DataTableCell>
                      <DataTableCell>{p.max_activations}</DataTableCell>
                      <DataTableCell>
                        <Badge className={boolColor(p.active)}>
                          {p.active ? t("common.active") : t("common.inactive")}
                        </Badge>
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

      {/* Create */}
      <PlanDialog
        open={creating}
        onClose={() => setCreating(false)}
        products={products}
        onSubmit={(d) => createMut.mutate(d)}
        loading={createMut.isPending}
        title={t("plans.createTitle")}
      />

      {/* Edit */}
      {editing && (
        <PlanDialog
          open
          onClose={() => setEditing(null)}
          plan={editing}
          products={products}
          onSubmit={(d) => updateMut.mutate({ id: editing.id, ...d })}
          loading={updateMut.isPending}
          title={t("plans.editTitle")}
        />
      )}

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
  products: { id: string; name: string }[]
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
    billing_interval: plan?.billing_interval || "month",
    max_activations: plan?.max_activations ?? 3,
    max_seats: plan?.max_seats ?? 1,
    trial_days: plan?.trial_days ?? 0,
    grace_days: plan?.grace_days ?? 7,
    active: plan?.active ?? true,
    stripe_price_id: plan?.stripe_price_id || "",
    paypal_plan_id: plan?.paypal_plan_id || "",
  })

  const set = (key: string, val: string | number | boolean) => setForm((f) => ({ ...f, [key]: val }))

  return (
    <Dialog open={open} onOpenChange={onClose}>
      <DialogContent className="max-w-lg max-h-[85vh] overflow-y-auto">
        <DialogHeader>
          <DialogTitle>{title}</DialogTitle>
          <DialogDescription>{t("plans.formDesc")}</DialogDescription>
        </DialogHeader>
        <form
          onSubmit={(e) => {
            e.preventDefault()
            onSubmit(form)
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
              <Select value={form.license_type} onValueChange={(v) => set("license_type", v)}>
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
            <div className="space-y-2">
              <Label>{t("plans.maxActivations")}</Label>
              <Input
                type="number"
                min={1}
                value={form.max_activations}
                onChange={(e) => set("max_activations", Number(e.target.value))}
              />
            </div>
            <div className="space-y-2">
              <Label>{t("plans.maxSeats")}</Label>
              <Input
                type="number"
                min={1}
                value={form.max_seats}
                onChange={(e) => set("max_seats", Number(e.target.value))}
              />
            </div>
            <div className="space-y-2">
              <Label>{t("plans.trialDays")}</Label>
              <Input
                type="number"
                min={0}
                value={form.trial_days}
                onChange={(e) => set("trial_days", Number(e.target.value))}
              />
            </div>
            <div className="space-y-2">
              <Label>{t("plans.graceDays")}</Label>
              <Input
                type="number"
                min={0}
                value={form.grace_days}
                onChange={(e) => set("grace_days", Number(e.target.value))}
              />
            </div>
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
            <div className="space-y-2">
              <Label>{t("plans.paypalPlanId")}</Label>
              <Input
                value={form.paypal_plan_id}
                onChange={(e) => set("paypal_plan_id", e.target.value)}
                placeholder="P-..."
              />
            </div>
          </div>
          <div className="flex justify-end gap-2 pt-2">
            <Button type="button" variant="outline" onClick={onClose}>
              {t("common.cancel")}
            </Button>
            <Button type="submit" disabled={loading}>
              {loading ? t("common.loading") : t("common.save")}
            </Button>
          </div>
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
  })

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
        ...(newEnt.value_type === "quota" ? { quota_period: newEnt.quota_period, quota_unit: newEnt.quota_unit } : {}),
      } as { plan_id: string; feature: string; value_type: string; value: string }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["admin", "plans"] })
      setAdding(false)
      setNewEnt({ feature: "", value_type: "bool", value: "true", quota_period: "", quota_unit: "" })
    },
  })

  const deleteMut = useMutation({
    mutationFn: (id: string) => admin.deleteEntitlement(id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["admin", "plans"] })
    },
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
                onClick={() => deleteMut.mutate(e.id)}
              >
                <Trash2 className="h-3 w-3 text-destructive" />
              </Button>
            </div>
          ))}
        </div>
      )}
      {adding && (
        <div className="border rounded p-3 space-y-3">
          <div className="grid grid-cols-2 gap-3">
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
    </div>
  )
}
