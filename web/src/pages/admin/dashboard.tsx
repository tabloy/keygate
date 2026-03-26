import { useQuery } from "@tanstack/react-query"
import { Activity, ArrowUpCircle, Key, Package, Users } from "lucide-react"
import { Link } from "react-router-dom"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import {
  DataTable,
  DataTableBody,
  DataTableCell,
  DataTableHead,
  DataTableHeader,
  DataTableRow,
} from "@/components/ui/data-table"
import { useI18n } from "@/i18n"
import { admin } from "@/lib/api"
import { formatDate, statusColor } from "@/lib/utils"

export default function DashboardPage() {
  const { t } = useI18n()
  const { data: stats, isLoading } = useQuery({ queryKey: ["admin", "stats"], queryFn: admin.stats })
  const { data: updateData } = useQuery({
    queryKey: ["admin", "update-check"],
    queryFn: admin.checkUpdate,
    staleTime: 60 * 60 * 1000,
  })

  if (isLoading || !stats) {
    return (
      <div className="animate-pulse space-y-4">
        <div className="h-32 bg-muted rounded-lg" />
        <div className="h-32 bg-muted rounded-lg" />
      </div>
    )
  }

  const cards = [
    { label: t("dashboard.totalLicenses"), value: stats.total_licenses, icon: Key, color: "text-primary" },
    { label: t("dashboard.activeLicenses"), value: stats.active_licenses, icon: Activity, color: "text-emerald-600" },
    { label: t("dashboard.totalActivations"), value: stats.total_activations, icon: Users, color: "text-primary/70" },
    { label: t("dashboard.products"), value: stats.total_products, icon: Package, color: "text-amber-600" },
  ]

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-bold tracking-tight">{t("dashboard.title")}</h1>
        <p className="text-muted-foreground">{t("dashboard.subtitle")}</p>
      </div>

      {updateData?.available && (
        <div className="flex items-center justify-between rounded-lg border border-blue-200 bg-blue-50 px-4 py-3">
          <div className="flex items-center gap-3">
            <ArrowUpCircle className="h-5 w-5 text-blue-600" />
            <div>
              <p className="text-sm font-medium text-blue-900">
                {t("dashboard.updateAvailable", { version: updateData.latest_version })}
              </p>
              {updateData.update_command && (
                <code className="text-xs text-blue-700 font-mono mt-0.5 block">{updateData.update_command}</code>
              )}
            </div>
          </div>
          <div className="flex items-center gap-2">
            <Link to="/admin/settings?tab=system">
              <Button size="sm" variant="outline" className="text-blue-700 border-blue-300">
                {t("dashboard.viewUpdate")}
              </Button>
            </Link>
            {updateData.release_url && (
              <a href={updateData.release_url} target="_blank" rel="noopener noreferrer">
                <Button size="sm" className="bg-blue-600 hover:bg-blue-700">
                  {t("dashboard.releaseNotes")}
                </Button>
              </a>
            )}
          </div>
        </div>
      )}

      <div className="grid gap-4 md:grid-cols-2 lg:grid-cols-4">
        {cards.map((c) => (
          <Card key={c.label}>
            <CardHeader className="flex flex-row items-center justify-between space-y-0 pb-2">
              <CardTitle className="text-sm font-medium">{c.label}</CardTitle>
              <c.icon className={`h-4 w-4 ${c.color}`} />
            </CardHeader>
            <CardContent>
              <div className="text-3xl font-bold">{c.value}</div>
            </CardContent>
          </Card>
        ))}
      </div>

      {/* Status breakdown */}
      {Object.keys(stats.by_status).length > 0 && (
        <Card>
          <CardHeader>
            <CardTitle className="text-base">{t("dashboard.statusDistribution")}</CardTitle>
          </CardHeader>
          <CardContent>
            <div className="flex flex-wrap gap-3">
              {Object.entries(stats.by_status).map(([status, count]) => (
                <div key={status} className="flex items-center gap-2">
                  <Badge className={statusColor(status)}>{t(`status.${status}` as any)}</Badge>
                  <span className="text-sm font-medium">{count}</span>
                </div>
              ))}
            </div>
          </CardContent>
        </Card>
      )}

      {/* Recent licenses */}
      {stats.recent_licenses?.length > 0 && (
        <Card>
          <CardHeader>
            <CardTitle className="text-base">{t("dashboard.recentLicenses")}</CardTitle>
          </CardHeader>
          <CardContent>
            <DataTable>
              <DataTableHeader>
                <DataTableRow>
                  <DataTableHead>{t("common.email")}</DataTableHead>
                  <DataTableHead>{t("common.product")}</DataTableHead>
                  <DataTableHead>{t("common.plan")}</DataTableHead>
                  <DataTableHead>{t("common.status")}</DataTableHead>
                  <DataTableHead>{t("common.created")}</DataTableHead>
                </DataTableRow>
              </DataTableHeader>
              <DataTableBody>
                {stats.recent_licenses.map((lic) => (
                  <DataTableRow key={lic.id}>
                    <DataTableCell>
                      <Link to={`/admin/licenses?id=${lic.id}`} className="font-medium hover:underline">
                        {lic.email}
                      </Link>
                    </DataTableCell>
                    <DataTableCell className="text-muted-foreground">{lic.product?.name || "-"}</DataTableCell>
                    <DataTableCell className="text-muted-foreground">{lic.plan?.name || "-"}</DataTableCell>
                    <DataTableCell>
                      <Badge className={statusColor(lic.status)}>{t(`status.${lic.status}` as any)}</Badge>
                    </DataTableCell>
                    <DataTableCell className="text-muted-foreground text-xs">
                      {formatDate(lic.created_at)}
                    </DataTableCell>
                  </DataTableRow>
                ))}
              </DataTableBody>
            </DataTable>
          </CardContent>
        </Card>
      )}
    </div>
  )
}
