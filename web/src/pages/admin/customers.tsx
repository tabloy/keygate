import { useQuery } from "@tanstack/react-query"
import { Eye, Search } from "lucide-react"
import { useState } from "react"
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
} from "@/components/ui/data-table"
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Input } from "@/components/ui/input"
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { useI18n } from "@/i18n"
import type { UserDetail } from "@/lib/api"
import { admin } from "@/lib/api"
import { formatDate, statusColor } from "@/lib/utils"

export default function CustomersPage() {
  const { t } = useI18n()
  const [page, setPage] = useState(0)
  const [search, setSearch] = useState("")
  const limit = 30
  const [viewingUser, setViewingUser] = useState<string | null>(null)

  const { data, isLoading } = useQuery({
    queryKey: ["admin", "users", search, page],
    queryFn: () => admin.listUsers({ search: search || undefined, offset: page * limit, limit }),
  })

  const customers = data?.users || []
  const total = data?.total || 0
  const totalPages = Math.ceil(total / limit)

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-bold tracking-tight">{t("customers.title")}</h1>
        <p className="text-muted-foreground">{t("customers.subtitle", { count: total })}</p>
      </div>

      <div className="flex items-center gap-4">
        <div className="relative flex-1 max-w-sm">
          <Search className="absolute left-3 top-1/2 -translate-y-1/2 h-4 w-4 text-muted-foreground" />
          <Input
            placeholder={t("common.search")}
            value={search}
            onChange={(e) => {
              setSearch(e.target.value)
              setPage(0)
            }}
            className="pl-9"
          />
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
                    <DataTableHead>{t("customers.customer")}</DataTableHead>
                    <DataTableHead>{t("common.email")}</DataTableHead>
                    <DataTableHead>{t("customers.joined")}</DataTableHead>
                    <DataTableHead>{t("customers.lastUpdated")}</DataTableHead>
                    <DataTableHead className="w-16">{t("common.actions")}</DataTableHead>
                  </DataTableRow>
                </DataTableHeader>
                <DataTableBody>
                  {customers.length === 0 && <DataTableEmpty colSpan={5} message={t("customers.empty")} />}
                  {customers.map((u) => (
                    <DataTableRow key={u.id}>
                      <DataTableCell>
                        <div className="flex items-center gap-3">
                          {u.avatar_url ? (
                            <img src={u.avatar_url} className="h-8 w-8 rounded-full" alt="" />
                          ) : (
                            <div className="h-8 w-8 rounded-full bg-muted flex items-center justify-center text-xs font-bold">
                              {u.name?.charAt(0)?.toUpperCase() || u.email.charAt(0).toUpperCase()}
                            </div>
                          )}
                          <span className="font-medium">{u.name || "-"}</span>
                        </div>
                      </DataTableCell>
                      <DataTableCell className="text-muted-foreground">{u.email}</DataTableCell>
                      <DataTableCell className="text-muted-foreground text-xs">
                        {formatDate(u.created_at)}
                      </DataTableCell>
                      <DataTableCell className="text-muted-foreground text-xs">
                        {formatDate(u.updated_at)}
                      </DataTableCell>
                      <DataTableCell>
                        <Button variant="ghost" size="icon" className="h-8 w-8" onClick={() => setViewingUser(u.id)}>
                          <Eye className="h-4 w-4" />
                        </Button>
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
                  pageSize={limit}
                  onPageChange={setPage}
                />
              )}
            </>
          )}
        </CardContent>
      </Card>

      <CustomerDetailDialog
        userId={viewingUser}
        open={!!viewingUser}
        onOpenChange={(open) => {
          if (!open) setViewingUser(null)
        }}
      />
    </div>
  )
}

function CustomerDetailDialog({
  userId,
  open,
  onOpenChange,
}: {
  userId: string | null
  open: boolean
  onOpenChange: (open: boolean) => void
}) {
  const { t } = useI18n()
  const { data, isLoading } = useQuery({
    queryKey: ["admin", "user-detail", userId],
    queryFn: () => admin.getUserDetail(userId!),
    enabled: !!userId,
  })

  const detail: UserDetail | undefined = data

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-w-3xl max-h-[85vh] overflow-y-auto">
        <DialogHeader>
          <DialogTitle>{t("customers.detail")}</DialogTitle>
          <DialogDescription>{detail?.user?.email || t("common.loading")}</DialogDescription>
        </DialogHeader>

        {isLoading || !detail ? (
          <div className="space-y-4">
            <div className="h-24 bg-muted rounded-lg animate-pulse" />
            <div className="h-48 bg-muted rounded-lg animate-pulse" />
          </div>
        ) : (
          <Tabs defaultValue="overview">
            <TabsList>
              <TabsTrigger value="overview">{t("customers.overview")}</TabsTrigger>
              <TabsTrigger value="licenses">{t("customers.licenses")}</TabsTrigger>
              <TabsTrigger value="subscriptions">{t("customers.subscriptions")}</TabsTrigger>
              <TabsTrigger value="activity">{t("customers.activity")}</TabsTrigger>
            </TabsList>

            {/* Overview Tab */}
            <TabsContent value="overview">
              <div className="space-y-4">
                {/* User info */}
                <Card>
                  <CardContent className="pt-6">
                    <div className="flex items-center gap-4">
                      {detail.user.avatar_url ? (
                        <img src={detail.user.avatar_url} className="h-14 w-14 rounded-full" alt="" />
                      ) : (
                        <div className="h-14 w-14 rounded-full bg-muted flex items-center justify-center text-lg font-bold">
                          {detail.user.name?.charAt(0)?.toUpperCase() || detail.user.email.charAt(0).toUpperCase()}
                        </div>
                      )}
                      <div>
                        <h3 className="text-lg font-semibold">{detail.user.name || "-"}</h3>
                        <p className="text-sm text-muted-foreground">{detail.user.email}</p>
                        <div className="flex gap-4 mt-1 text-xs text-muted-foreground">
                          <span>
                            {t("customers.joined")} {formatDate(detail.user.created_at)}
                          </span>
                          <span>
                            {t("customers.lastUpdated")} {formatDate(detail.user.updated_at)}
                          </span>
                        </div>
                      </div>
                    </div>
                  </CardContent>
                </Card>

                {/* Summary stats */}
                <div className="grid grid-cols-5 gap-3">
                  {[
                    { label: t("analytics.totalLicenses"), value: detail.licenses?.length ?? 0 },
                    {
                      label: t("analytics.active"),
                      value: detail.licenses?.filter((l) => l.status === "active").length ?? 0,
                    },
                    { label: t("customers.totalUsage"), value: detail.total_usage ?? 0 },
                    { label: t("customers.activeSeats"), value: detail.active_seats ?? 0 },
                    { label: t("analytics.activations"), value: detail.activations ?? 0 },
                  ].map((s) => (
                    <Card key={s.label}>
                      <CardContent className="pt-4 pb-3 text-center">
                        <div className="text-2xl font-bold">{s.value.toLocaleString()}</div>
                        <p className="text-xs text-muted-foreground">{s.label}</p>
                      </CardContent>
                    </Card>
                  ))}
                </div>
              </div>
            </TabsContent>

            {/* Licenses Tab */}
            <TabsContent value="licenses">
              {(detail.licenses || []).length === 0 ? (
                <Card>
                  <CardContent className="py-8">
                    <p className="text-sm text-muted-foreground text-center">No licenses found.</p>
                  </CardContent>
                </Card>
              ) : (
                <DataTable>
                  <DataTableHeader>
                    <DataTableRow>
                      <DataTableHead>{t("common.product")}</DataTableHead>
                      <DataTableHead>{t("common.plan")}</DataTableHead>
                      <DataTableHead>{t("common.status")}</DataTableHead>
                      <DataTableHead>{t("licenses.licenseKey")}</DataTableHead>
                      <DataTableHead>{t("licenses.validUntil")}</DataTableHead>
                      <DataTableHead>{t("common.created")}</DataTableHead>
                    </DataTableRow>
                  </DataTableHeader>
                  <DataTableBody>
                    {detail.licenses.map((l) => (
                      <DataTableRow key={l.id}>
                        <DataTableCell className="font-medium">{l.product?.name || l.product_id}</DataTableCell>
                        <DataTableCell>{l.plan?.name || l.plan_id}</DataTableCell>
                        <DataTableCell>
                          <Badge className={statusColor(l.status)}>{t(`status.${l.status}` as any)}</Badge>
                        </DataTableCell>
                        <DataTableCell>
                          <code className="text-xs bg-muted px-1.5 py-0.5 rounded font-mono">
                            {l.license_key.length > 16 ? `${l.license_key.slice(0, 16)}...` : l.license_key}
                          </code>
                        </DataTableCell>
                        <DataTableCell className="text-xs text-muted-foreground">
                          {l.valid_until ? formatDate(l.valid_until) : "-"}
                        </DataTableCell>
                        <DataTableCell className="text-xs text-muted-foreground">
                          {formatDate(l.created_at)}
                        </DataTableCell>
                      </DataTableRow>
                    ))}
                  </DataTableBody>
                </DataTable>
              )}
            </TabsContent>

            {/* Subscriptions Tab */}
            <TabsContent value="subscriptions">
              {(detail.subscriptions || []).length === 0 ? (
                <Card>
                  <CardContent className="py-8">
                    <p className="text-sm text-muted-foreground text-center">No subscriptions found.</p>
                  </CardContent>
                </Card>
              ) : (
                <DataTable>
                  <DataTableHeader>
                    <DataTableRow>
                      <DataTableHead>{t("common.plan")}</DataTableHead>
                      <DataTableHead>{t("common.status")}</DataTableHead>
                      <DataTableHead>{t("customers.provider")}</DataTableHead>
                      <DataTableHead>{t("customers.periodRange")}</DataTableHead>
                      <DataTableHead>{t("customers.cancelAtEnd")}</DataTableHead>
                      <DataTableHead>{t("common.created")}</DataTableHead>
                    </DataTableRow>
                  </DataTableHeader>
                  <DataTableBody>
                    {detail.subscriptions.map((sub) => (
                      <DataTableRow key={sub.id}>
                        <DataTableCell className="font-medium">{sub.plan?.name || sub.plan_id}</DataTableCell>
                        <DataTableCell>
                          <Badge className={statusColor(sub.status)}>{t(`status.${sub.status}` as any)}</Badge>
                        </DataTableCell>
                        <DataTableCell className="text-muted-foreground">{sub.payment_provider || "-"}</DataTableCell>
                        <DataTableCell className="text-xs text-muted-foreground">
                          <div>
                            {sub.current_period_start ? formatDate(sub.current_period_start) : "-"}
                            {" - "}
                            {sub.current_period_end ? formatDate(sub.current_period_end) : "-"}
                          </div>
                          {sub.trial_start && (
                            <div className="text-violet-600 mt-0.5">
                              Trial: {formatDate(sub.trial_start)} - {formatDate(sub.trial_end)}
                            </div>
                          )}
                        </DataTableCell>
                        <DataTableCell>
                          {sub.cancel_at_period_end ? (
                            <Badge variant="destructive">{t("common.yes")}</Badge>
                          ) : (
                            <span className="text-muted-foreground text-xs">{t("common.no")}</span>
                          )}
                        </DataTableCell>
                        <DataTableCell className="text-xs text-muted-foreground">
                          {formatDate(sub.created_at)}
                        </DataTableCell>
                      </DataTableRow>
                    ))}
                  </DataTableBody>
                </DataTable>
              )}
            </TabsContent>

            {/* Activity Tab */}
            <TabsContent value="activity">
              {(detail.recent_audit_logs || []).length === 0 ? (
                <Card>
                  <CardContent className="py-8">
                    <p className="text-sm text-muted-foreground text-center">No recent activity.</p>
                  </CardContent>
                </Card>
              ) : (
                <div className="space-y-2">
                  {detail.recent_audit_logs.map((a) => (
                    <div key={a.id} className="flex items-center gap-3 py-2 border-b last:border-0 text-sm">
                      <span className="text-xs text-muted-foreground w-36 shrink-0">{formatDate(a.created_at)}</span>
                      <Badge variant="outline" className="shrink-0">
                        {a.entity}
                      </Badge>
                      <Badge
                        className={
                          a.action.includes("create")
                            ? "bg-emerald-100 text-emerald-800"
                            : a.action.includes("delete") || a.action.includes("revoke")
                              ? "bg-red-100 text-red-800"
                              : a.action.includes("update")
                                ? "bg-blue-100 text-blue-800"
                                : a.action.includes("suspend")
                                  ? "bg-orange-100 text-orange-800"
                                  : "bg-gray-100 text-gray-800"
                        }
                      >
                        {a.action}
                      </Badge>
                      <span
                        className="font-mono text-xs text-muted-foreground truncate max-w-[180px]"
                        title={a.entity_id}
                      >
                        {a.entity_id.length > 12 ? `${a.entity_id.slice(0, 12)}...` : a.entity_id}
                      </span>
                    </div>
                  ))}
                </div>
              )}
            </TabsContent>
          </Tabs>
        )}
      </DialogContent>
    </Dialog>
  )
}
