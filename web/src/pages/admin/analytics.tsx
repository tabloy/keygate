import { useQuery } from "@tanstack/react-query"
import {
  Activity,
  AlertTriangle,
  Ban,
  BarChart3,
  Clock,
  Key,
  ShieldOff,
  TrendingDown,
  TrendingUp,
  Users,
  XCircle,
  Zap,
} from "lucide-react"
import { useMemo, useState } from "react"
import {
  Area,
  AreaChart,
  Bar,
  BarChart,
  CartesianGrid,
  Cell,
  Pie,
  PieChart,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from "recharts"
import { Badge } from "@/components/ui/badge"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import {
  DataTable,
  DataTableBody,
  DataTableCell,
  DataTableHead,
  DataTableHeader,
  DataTablePagination,
  DataTableRow,
  useClientPagination,
} from "@/components/ui/data-table"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select"
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { useI18n } from "@/i18n"
import type { AggregatedSnapshot, AnalyticsSnapshot } from "@/lib/api"
import { admin } from "@/lib/api"
import { formatDate } from "@/lib/utils"

function defaultFrom(): string {
  const d = new Date()
  d.setDate(d.getDate() - 30)
  return d.toISOString().slice(0, 10)
}

function defaultTo(): string {
  return new Date().toISOString().slice(0, 10)
}

function periodLabel(granularity: string, t: (key: any) => string): string {
  switch (granularity) {
    case "weekly":
      return t("analytics.weekOf")
    case "monthly":
      return t("analytics.month")
    default:
      return t("analytics.date")
  }
}

function snapshotPeriod(s: AnalyticsSnapshot | AggregatedSnapshot, granularity: string): string {
  const raw = "period" in s ? s.period : "date" in s ? s.date : ""
  if (!raw) return "-"
  const d = new Date(raw)
  if (Number.isNaN(d.getTime())) return raw
  const locale = document.documentElement.lang || navigator.language
  switch (granularity) {
    case "monthly":
      return d.toLocaleDateString(locale, { year: "numeric", month: "2-digit" })
    case "weekly":
      return d.toLocaleDateString(locale, { year: "numeric", month: "2-digit", day: "2-digit" })
    default:
      return d.toLocaleDateString(locale, { month: "2-digit", day: "2-digit" })
  }
}

export default function AnalyticsPage() {
  const { t } = useI18n()
  const [productFilter, setProductFilter] = useState<string>("")
  const [from, setFrom] = useState(defaultFrom)
  const [to, setTo] = useState(defaultTo)
  const [granularity, setGranularity] = useState("daily")
  const [planFilter, setPlanFilter] = useState<string>("")
  const [licenseType, setLicenseType] = useState<string>("")
  const [statusFilter, setStatusFilter] = useState<string>("")

  // Products
  const { data: productsData } = useQuery({ queryKey: ["admin", "products"], queryFn: () => admin.listProducts() })
  const products = productsData?.products || []

  // Plans (filtered by product)
  const { data: plansData } = useQuery({
    queryKey: ["admin", "plans", productFilter],
    queryFn: () => admin.listPlans(productFilter || undefined),
  })
  const plans = plansData?.plans || []

  // Shared filter params
  const filterParams = {
    product_id: productFilter || undefined,
    plan_id: planFilter || undefined,
    license_type: licenseType || undefined,
    status: statusFilter || undefined,
    from: from || undefined,
    to: to || undefined,
  }
  const filterKeys = [productFilter, planFilter, licenseType, statusFilter, from, to]

  // Summary
  const { data: summaryData, isLoading: summaryLoading } = useQuery({
    queryKey: ["admin", "analytics", "summary", ...filterKeys],
    queryFn: () => admin.getAnalyticsSummary(filterParams),
  })
  const summary = summaryData

  // Snapshots / Trend data
  const { data: snapshotsData, isLoading: snapshotsLoading } = useQuery({
    queryKey: ["admin", "analytics", "snapshots", productFilter, from, to, granularity],
    queryFn: () =>
      admin.getAnalytics({
        product_id: productFilter || undefined,
        from: from || undefined,
        to: to || undefined,
        granularity: granularity !== "daily" ? granularity : undefined,
      }),
  })
  const snapshots = (snapshotsData?.snapshots || []) as (AnalyticsSnapshot | AggregatedSnapshot)[]

  // Breakdowns
  const { data: statusBreakdown } = useQuery({
    queryKey: ["admin", "analytics", "breakdown", "status", ...filterKeys],
    queryFn: () => admin.getAnalyticsBreakdown({ ...filterParams, dimension: "status" }),
  })
  const { data: planBreakdown } = useQuery({
    queryKey: ["admin", "analytics", "breakdown", "plan", ...filterKeys],
    queryFn: () => admin.getAnalyticsBreakdown({ ...filterParams, dimension: "plan" }),
  })
  const { data: typeBreakdown } = useQuery({
    queryKey: ["admin", "analytics", "breakdown", "license_type", ...filterKeys],
    queryFn: () => admin.getAnalyticsBreakdown({ ...filterParams, dimension: "license_type" }),
  })

  // Usage top features (only supports product + date filters)
  const { data: usageData } = useQuery({
    queryKey: ["admin", "analytics", "usage-top", productFilter, from, to],
    queryFn: () =>
      admin.getAnalyticsUsageTop({
        product_id: productFilter || undefined,
        from: from || undefined,
        to: to || undefined,
      }),
  })
  const features = usageData?.features || []

  // Activation trend (only supports product + date filters)
  const { data: activationData } = useQuery({
    queryKey: ["admin", "analytics", "activation-trend", productFilter, from, to],
    queryFn: () =>
      admin.getAnalyticsActivationTrend({
        product_id: productFilter || undefined,
        from: from || undefined,
        to: to || undefined,
      }),
  })
  const activationTrend = activationData?.trend || []

  // Insights (only supports product filter)
  const { data: insightsData } = useQuery({
    queryKey: ["admin", "analytics", "insights", productFilter],
    queryFn: () => admin.getAnalyticsInsights({ product_id: productFilter || undefined }),
  })
  const insights = insightsData

  const isLoading = summaryLoading || snapshotsLoading

  const {
    page: snapPage,
    setPage: setSnapPage,
    pageSize: snapPageSize,
    setPageSize: setSnapPageSize,
    total: snapTotal,
    totalPages: snapTotalPages,
    paginatedItems: paginatedSnapshots,
  } = useClientPagination(snapshots as any[], 15)

  // Chart data
  const trendChartData = useMemo(
    () =>
      snapshots.map((s) => ({
        period: snapshotPeriod(s, granularity),
        new: s.new_licenses,
        churned: s.churned,
        net: s.new_licenses - s.churned,
        total: s.total_licenses,
        active: s.active_licenses,
      })),
    [snapshots, granularity],
  )

  const activationChartData = useMemo(
    () => activationTrend.map((t) => ({ date: t.date, count: t.count })),
    [activationTrend],
  )

  const summaryCards = [
    { label: t("analytics.totalLicenses"), value: summary?.total_licenses ?? 0, icon: Key, color: "text-blue-600" },
    { label: t("analytics.active"), value: summary?.active_licenses ?? 0, icon: Activity, color: "text-emerald-600" },
    { label: t("analytics.trialing"), value: summary?.trialing_licenses ?? 0, icon: Clock, color: "text-violet-600" },
    {
      label: t("analytics.pastDue"),
      value: summary?.past_due_licenses ?? 0,
      icon: AlertTriangle,
      color: "text-amber-600",
    },
    {
      label: t("analytics.expired"),
      value: summary?.expired_licenses ?? 0,
      icon: TrendingDown,
      color: "text-gray-500",
    },
    { label: t("analytics.canceled"), value: summary?.canceled_licenses ?? 0, icon: XCircle, color: "text-red-500" },
    { label: t("analytics.suspended"), value: summary?.suspended_licenses ?? 0, icon: Ban, color: "text-orange-600" },
    { label: t("analytics.revoked"), value: summary?.revoked_licenses ?? 0, icon: ShieldOff, color: "text-red-700" },
  ]

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-bold tracking-tight">{t("analytics.title")}</h1>
        <p className="text-muted-foreground">{t("analytics.subtitle")}</p>
      </div>

      {/* Filters */}
      <div className="flex gap-4 flex-wrap items-end">
        <div className="space-y-2">
          <Label className="text-xs">{t("common.product")}</Label>
          <Select
            value={productFilter}
            onValueChange={(v) => {
              setProductFilter(v === "all" ? "" : v)
              setPlanFilter("")
            }}
          >
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
        </div>
        <div className="space-y-2">
          <Label className="text-xs">{t("analytics.start")}</Label>
          <Input type="date" value={from} onChange={(e) => setFrom(e.target.value)} className="w-40" />
        </div>
        <div className="space-y-2">
          <Label className="text-xs">{t("analytics.end")}</Label>
          <Input type="date" value={to} onChange={(e) => setTo(e.target.value)} className="w-40" />
        </div>
        <div className="space-y-2">
          <Label className="text-xs">{t("analytics.granularity")}</Label>
          <Select value={granularity} onValueChange={setGranularity}>
            <SelectTrigger className="w-36">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="daily">{t("analytics.daily")}</SelectItem>
              <SelectItem value="weekly">{t("analytics.weekly")}</SelectItem>
              <SelectItem value="monthly">{t("analytics.monthly")}</SelectItem>
            </SelectContent>
          </Select>
        </div>
        <div className="space-y-2">
          <Label className="text-xs">{t("common.plan")}</Label>
          <Select
            value={planFilter}
            onValueChange={(v) => {
              setPlanFilter(v === "all" ? "" : v)
              if (v !== "all") setLicenseType("")
            }}
          >
            <SelectTrigger className="w-44" disabled={!productFilter}>
              <SelectValue placeholder={t("filter.allPlans")} />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="all">{t("filter.allPlans")}</SelectItem>
              {plans.map((p) => (
                <SelectItem key={p.id} value={p.id}>
                  {p.name}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </div>
        <div className="space-y-2">
          <Label className="text-xs">{t("plans.licenseType")}</Label>
          <Select
            value={licenseType}
            onValueChange={(v) => {
              setLicenseType(v === "all" ? "" : v)
              if (v !== "all") setPlanFilter("")
            }}
          >
            <SelectTrigger className="w-40">
              <SelectValue placeholder={t("filter.allTypes")} />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="all">{t("filter.allTypes")}</SelectItem>
              <SelectItem value="subscription">{t("plans.subscription")}</SelectItem>
              <SelectItem value="perpetual">{t("plans.perpetual")}</SelectItem>
              <SelectItem value="trial">{t("plans.trial")}</SelectItem>
            </SelectContent>
          </Select>
        </div>
        <div className="space-y-2">
          <Label className="text-xs">{t("common.status")}</Label>
          <Select value={statusFilter} onValueChange={(v) => setStatusFilter(v === "all" ? "" : v)}>
            <SelectTrigger className="w-40">
              <SelectValue placeholder={t("filter.allStatuses")} />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="all">{t("filter.allStatuses")}</SelectItem>
              <SelectItem value="active">{t("status.active")}</SelectItem>
              <SelectItem value="trialing">{t("status.trialing")}</SelectItem>
              <SelectItem value="past_due">{t("status.past_due")}</SelectItem>
              <SelectItem value="canceled">{t("status.canceled")}</SelectItem>
              <SelectItem value="expired">{t("status.expired")}</SelectItem>
              <SelectItem value="suspended">{t("status.suspended")}</SelectItem>
              <SelectItem value="revoked">{t("status.revoked")}</SelectItem>
            </SelectContent>
          </Select>
        </div>
      </div>

      {isLoading ? (
        <div className="animate-pulse space-y-4">
          <div className="h-32 bg-muted rounded-lg" />
          <div className="h-48 bg-muted rounded-lg" />
        </div>
      ) : (
        <>
          {/* Summary Cards - Row 1 */}
          <div className="grid gap-4 md:grid-cols-2 lg:grid-cols-4">
            {summaryCards.slice(0, 4).map((c) => (
              <Card key={c.label}>
                <CardHeader className="flex flex-row items-center justify-between space-y-0 pb-2">
                  <CardTitle className="text-sm font-medium">{c.label}</CardTitle>
                  <c.icon className={`h-4 w-4 ${c.color}`} />
                </CardHeader>
                <CardContent>
                  <div className="text-3xl font-bold">{c.value.toLocaleString()}</div>
                </CardContent>
              </Card>
            ))}
          </div>

          {/* Summary Cards - Row 2 */}
          <div className="grid gap-4 md:grid-cols-2 lg:grid-cols-4">
            {summaryCards.slice(4).map((c) => (
              <Card key={c.label}>
                <CardHeader className="flex flex-row items-center justify-between space-y-0 pb-2">
                  <CardTitle className="text-sm font-medium">{c.label}</CardTitle>
                  <c.icon className={`h-4 w-4 ${c.color}`} />
                </CardHeader>
                <CardContent>
                  <div className="text-3xl font-bold">{c.value.toLocaleString()}</div>
                </CardContent>
              </Card>
            ))}
          </div>

          {/* Extra info row */}
          <div className="flex gap-6 flex-wrap text-sm">
            <div className="flex items-center gap-2 text-muted-foreground">
              <Zap className="h-4 w-4 text-amber-500" />
              <span>{t("analytics.totalActivations")}:</span>
              <span className="font-semibold text-foreground">
                {(summary?.total_activations ?? 0).toLocaleString()}
              </span>
            </div>
            <div className="flex items-center gap-2 text-muted-foreground">
              <Users className="h-4 w-4 text-blue-500" />
              <span>{t("analytics.totalSeats")}:</span>
              <span className="font-semibold text-foreground">{(summary?.total_seats ?? 0).toLocaleString()}</span>
            </div>
            <div className="flex items-center gap-2 text-muted-foreground">
              <BarChart3 className="h-4 w-4 text-violet-500" />
              <span>{t("analytics.avgActivations")}:</span>
              <span className="font-semibold text-foreground">
                {(summary?.avg_activations_per_license ?? 0).toFixed(1)}
              </span>
            </div>
          </div>

          {/* Tabs */}
          <Tabs defaultValue="trends">
            <TabsList>
              <TabsTrigger value="trends">{t("analytics.trends")}</TabsTrigger>
              <TabsTrigger value="breakdowns">{t("analytics.breakdowns")}</TabsTrigger>
              <TabsTrigger value="usage">{t("analytics.usage")}</TabsTrigger>
              <TabsTrigger value="activations">{t("analytics.activations")}</TabsTrigger>
              <TabsTrigger value="insights">{t("analytics.insights")}</TabsTrigger>
            </TabsList>

            {/* Tab 1: Trends */}
            <TabsContent value="trends">
              {trendChartData.length > 0 ? (
                <div className="space-y-4">
                  {/* Total & Active licenses area chart */}
                  <Card>
                    <CardHeader>
                      <CardTitle className="text-base">{t("analytics.totalLicenses")}</CardTitle>
                    </CardHeader>
                    <CardContent>
                      <ResponsiveContainer width="100%" height={260}>
                        <AreaChart data={trendChartData}>
                          <defs>
                            <linearGradient id="gradTotal" x1="0" y1="0" x2="0" y2="1">
                              <stop offset="5%" stopColor="#7c3aed" stopOpacity={0.15} />
                              <stop offset="95%" stopColor="#7c3aed" stopOpacity={0} />
                            </linearGradient>
                            <linearGradient id="gradActive" x1="0" y1="0" x2="0" y2="1">
                              <stop offset="5%" stopColor="#10b981" stopOpacity={0.15} />
                              <stop offset="95%" stopColor="#10b981" stopOpacity={0} />
                            </linearGradient>
                          </defs>
                          <CartesianGrid strokeDasharray="3 3" stroke="var(--color-border)" />
                          <XAxis
                            dataKey="period"
                            tick={{ fontSize: 11, fill: "var(--color-muted-foreground)" }}
                            tickLine={false}
                            axisLine={false}
                          />
                          <YAxis
                            tick={{ fontSize: 11, fill: "var(--color-muted-foreground)" }}
                            tickLine={false}
                            axisLine={false}
                            width={40}
                            allowDecimals={false}
                          />
                          <Tooltip
                            contentStyle={{
                              fontSize: 12,
                              borderRadius: 8,
                              background: "var(--color-popover)",
                              border: "1px solid var(--color-border)",
                              color: "var(--color-foreground)",
                            }}
                          />
                          <Area
                            type="monotone"
                            dataKey="total"
                            name={t("analytics.totalLicenses")}
                            stroke="#7c3aed"
                            fill="url(#gradTotal)"
                            strokeWidth={2}
                          />
                          <Area
                            type="monotone"
                            dataKey="active"
                            name={t("analytics.active")}
                            stroke="#10b981"
                            fill="url(#gradActive)"
                            strokeWidth={2}
                          />
                        </AreaChart>
                      </ResponsiveContainer>
                    </CardContent>
                  </Card>

                  {/* New vs Churned bar chart */}
                  <Card>
                    <CardHeader>
                      <CardTitle className="text-base">
                        {t("analytics.new")} vs {t("analytics.churned")}
                      </CardTitle>
                    </CardHeader>
                    <CardContent>
                      <ResponsiveContainer width="100%" height={220}>
                        <BarChart data={trendChartData}>
                          <CartesianGrid strokeDasharray="3 3" stroke="var(--color-border)" />
                          <XAxis
                            dataKey="period"
                            tick={{ fontSize: 11, fill: "var(--color-muted-foreground)" }}
                            tickLine={false}
                            axisLine={false}
                          />
                          <YAxis
                            tick={{ fontSize: 11, fill: "var(--color-muted-foreground)" }}
                            tickLine={false}
                            axisLine={false}
                            width={40}
                            allowDecimals={false}
                          />
                          <Tooltip
                            contentStyle={{
                              fontSize: 12,
                              borderRadius: 8,
                              background: "var(--color-popover)",
                              border: "1px solid var(--color-border)",
                              color: "var(--color-foreground)",
                            }}
                          />
                          <Bar dataKey="new" name={t("analytics.new")} fill="#7c3aed" radius={[3, 3, 0, 0]} />
                          <Bar dataKey="churned" name={t("analytics.churned")} fill="#ef4444" radius={[3, 3, 0, 0]} />
                        </BarChart>
                      </ResponsiveContainer>
                    </CardContent>
                  </Card>
                </div>
              ) : (
                <Card>
                  <CardContent className="py-8">
                    <p className="text-sm text-muted-foreground text-center">{t("analytics.noData")}</p>
                  </CardContent>
                </Card>
              )}
            </TabsContent>

            {/* Tab 2: Breakdowns */}
            <TabsContent value="breakdowns">
              <div className="grid gap-4 md:grid-cols-3">
                <BreakdownCard title={t("analytics.byStatus")} items={statusBreakdown?.items || []} />
                <BreakdownCard title={t("analytics.byPlan")} items={planBreakdown?.items || []} />
                <BreakdownCard title={t("analytics.byLicenseType")} items={typeBreakdown?.items || []} />
              </div>
            </TabsContent>

            {/* Tab 3: Usage */}
            <TabsContent value="usage">
              <Card>
                <CardHeader>
                  <CardTitle className="text-base">{t("analytics.topFeatures")}</CardTitle>
                </CardHeader>
                <CardContent>
                  {features.length === 0 ? (
                    <p className="text-sm text-muted-foreground text-center py-8">{t("analytics.noUsageData")}</p>
                  ) : (
                    <div className="space-y-6">
                      <ResponsiveContainer width="100%" height={Math.max(features.length * 40 + 40, 160)}>
                        <BarChart data={features} layout="vertical">
                          <CartesianGrid strokeDasharray="3 3" stroke="var(--color-border)" horizontal={false} />
                          <XAxis
                            type="number"
                            tick={{ fontSize: 11, fill: "var(--color-muted-foreground)" }}
                            tickLine={false}
                            axisLine={false}
                            allowDecimals={false}
                          />
                          <YAxis
                            type="category"
                            dataKey="feature"
                            tick={{ fontSize: 11, fill: "var(--color-muted-foreground)" }}
                            tickLine={false}
                            axisLine={false}
                            width={100}
                          />
                          <Tooltip
                            contentStyle={{
                              fontSize: 12,
                              borderRadius: 8,
                              background: "var(--color-popover)",
                              border: "1px solid var(--color-border)",
                              color: "var(--color-foreground)",
                            }}
                          />
                          <Bar
                            dataKey="total_usage"
                            name={t("analytics.totalUsage")}
                            fill="#8b5cf6"
                            radius={[0, 4, 4, 0]}
                          />
                        </BarChart>
                      </ResponsiveContainer>

                      <DataTable>
                        <DataTableHeader>
                          <DataTableRow>
                            <DataTableHead className="w-12">{t("analytics.rank")}</DataTableHead>
                            <DataTableHead>{t("analytics.feature")}</DataTableHead>
                            <DataTableHead>{t("analytics.totalUsage")}</DataTableHead>
                            <DataTableHead>{t("analytics.uniqueLicenses")}</DataTableHead>
                          </DataTableRow>
                        </DataTableHeader>
                        <DataTableBody>
                          {features.map((f, i) => (
                            <DataTableRow key={f.feature}>
                              <DataTableCell className="font-medium text-muted-foreground">{i + 1}</DataTableCell>
                              <DataTableCell className="font-medium">{f.feature}</DataTableCell>
                              <DataTableCell>{f.total_usage.toLocaleString()}</DataTableCell>
                              <DataTableCell>{f.unique_users.toLocaleString()}</DataTableCell>
                            </DataTableRow>
                          ))}
                        </DataTableBody>
                      </DataTable>
                    </div>
                  )}
                </CardContent>
              </Card>
            </TabsContent>

            {/* Tab 4: Activations */}
            <TabsContent value="activations">
              <Card>
                <CardHeader>
                  <CardTitle className="text-base">{t("analytics.activationTrend")}</CardTitle>
                </CardHeader>
                <CardContent>
                  {activationChartData.length === 0 ? (
                    <p className="text-sm text-muted-foreground text-center py-8">{t("analytics.noActivationData")}</p>
                  ) : (
                    <ResponsiveContainer width="100%" height={280}>
                      <AreaChart data={activationChartData}>
                        <defs>
                          <linearGradient id="gradActivation" x1="0" y1="0" x2="0" y2="1">
                            <stop offset="5%" stopColor="#3b82f6" stopOpacity={0.2} />
                            <stop offset="95%" stopColor="#3b82f6" stopOpacity={0} />
                          </linearGradient>
                        </defs>
                        <CartesianGrid strokeDasharray="3 3" stroke="var(--color-border)" />
                        <XAxis
                          dataKey="date"
                          tick={{ fontSize: 11, fill: "var(--color-muted-foreground)" }}
                          tickLine={false}
                          axisLine={false}
                        />
                        <YAxis
                          tick={{ fontSize: 11, fill: "var(--color-muted-foreground)" }}
                          tickLine={false}
                          axisLine={false}
                          width={40}
                          allowDecimals={false}
                        />
                        <Tooltip
                          contentStyle={{
                            fontSize: 12,
                            borderRadius: 8,
                            background: "var(--color-popover)",
                            border: "1px solid var(--color-border)",
                            color: "var(--color-foreground)",
                          }}
                        />
                        <Area
                          type="monotone"
                          dataKey="count"
                          name={t("analytics.activations")}
                          stroke="#3b82f6"
                          fill="url(#gradActivation)"
                          strokeWidth={2}
                        />
                      </AreaChart>
                    </ResponsiveContainer>
                  )}
                </CardContent>
              </Card>
            </TabsContent>
            {/* Tab 5: Insights */}
            <TabsContent value="insights">
              {!insights ? (
                <Card>
                  <CardContent className="py-8">
                    <p className="text-sm text-muted-foreground text-center">{t("analytics.loadingInsights")}</p>
                  </CardContent>
                </Card>
              ) : (
                <div className="space-y-6">
                  {/* Growth Metrics */}
                  <div className="grid gap-4 md:grid-cols-2 lg:grid-cols-4">
                    <Card>
                      <CardHeader className="flex flex-row items-center justify-between space-y-0 pb-2">
                        <CardTitle className="text-sm font-medium">{t("analytics.growthRate")}</CardTitle>
                        {insights.growth.net_growth_rate >= 0 ? (
                          <TrendingUp className="h-4 w-4 text-emerald-600" />
                        ) : (
                          <TrendingDown className="h-4 w-4 text-red-600" />
                        )}
                      </CardHeader>
                      <CardContent>
                        <div
                          className={`text-3xl font-bold ${insights.growth.net_growth_rate >= 0 ? "text-emerald-600" : "text-red-600"}`}
                        >
                          {insights.growth.net_growth_rate.toFixed(1)}%
                        </div>
                      </CardContent>
                    </Card>
                    <Card>
                      <CardHeader className="flex flex-row items-center justify-between space-y-0 pb-2">
                        <CardTitle className="text-sm font-medium">{t("analytics.trialConversion")}</CardTitle>
                        <Activity className="h-4 w-4 text-violet-600" />
                      </CardHeader>
                      <CardContent>
                        <div className="text-3xl font-bold">{insights.growth.trial_conversion.toFixed(1)}%</div>
                        <p className="text-xs text-muted-foreground mt-1">
                          {insights.growth.converted_trials}/{insights.growth.total_trials} {t("analytics.converted")}
                        </p>
                      </CardContent>
                    </Card>
                    <Card>
                      <CardHeader className="flex flex-row items-center justify-between space-y-0 pb-2">
                        <CardTitle className="text-sm font-medium">{t("analytics.newLast30d")}</CardTitle>
                        <TrendingUp className="h-4 w-4 text-emerald-600" />
                      </CardHeader>
                      <CardContent>
                        <div className="text-3xl font-bold">{insights.growth.new_last_30d.toLocaleString()}</div>
                      </CardContent>
                    </Card>
                    <Card>
                      <CardHeader className="flex flex-row items-center justify-between space-y-0 pb-2">
                        <CardTitle className="text-sm font-medium">{t("analytics.churnedLast30d")}</CardTitle>
                        <TrendingDown className="h-4 w-4 text-red-600" />
                      </CardHeader>
                      <CardContent>
                        <div className="text-3xl font-bold">{insights.growth.churned_last_30d.toLocaleString()}</div>
                      </CardContent>
                    </Card>
                  </div>

                  {/* License Age & Retention */}
                  <div className="grid gap-4 md:grid-cols-2">
                    {/* Age Distribution */}
                    <Card>
                      <CardHeader>
                        <CardTitle className="text-base">{t("analytics.licenseAge")}</CardTitle>
                      </CardHeader>
                      <CardContent>
                        {(insights.age_distribution || []).length === 0 ? (
                          <p className="text-sm text-muted-foreground text-center py-4">{t("common.noData")}</p>
                        ) : (
                          <ResponsiveContainer width="100%" height={200}>
                            <BarChart data={insights.age_distribution}>
                              <CartesianGrid strokeDasharray="3 3" stroke="var(--color-border)" />
                              <XAxis
                                dataKey="bucket"
                                tick={{ fontSize: 11, fill: "var(--color-muted-foreground)" }}
                                tickLine={false}
                                axisLine={false}
                              />
                              <YAxis
                                tick={{ fontSize: 11, fill: "var(--color-muted-foreground)" }}
                                tickLine={false}
                                axisLine={false}
                                width={40}
                                allowDecimals={false}
                              />
                              <Tooltip
                                contentStyle={{
                                  fontSize: 12,
                                  borderRadius: 8,
                                  background: "var(--color-popover)",
                                  border: "1px solid var(--color-border)",
                                  color: "var(--color-foreground)",
                                }}
                              />
                              <Bar
                                dataKey="count"
                                name={t("analytics.totalLicenses")}
                                fill="#7c3aed"
                                radius={[3, 3, 0, 0]}
                              />
                            </BarChart>
                          </ResponsiveContainer>
                        )}
                      </CardContent>
                    </Card>

                    {/* Monthly Retention */}
                    <Card>
                      <CardHeader>
                        <CardTitle className="text-base">{t("analytics.monthlyRetention")}</CardTitle>
                      </CardHeader>
                      <CardContent>
                        {(insights.retention || []).length === 0 ? (
                          <p className="text-sm text-muted-foreground text-center py-4">{t("common.noData")}</p>
                        ) : (
                          <DataTable>
                            <DataTableHeader>
                              <DataTableRow>
                                <DataTableHead>{t("analytics.period")}</DataTableHead>
                                <DataTableHead>{t("analytics.start")}</DataTableHead>
                                <DataTableHead>{t("analytics.end")}</DataTableHead>
                                <DataTableHead>{t("analytics.retentionPct")}</DataTableHead>
                                <DataTableHead>{t("analytics.churnPct")}</DataTableHead>
                              </DataTableRow>
                            </DataTableHeader>
                            <DataTableBody>
                              {insights.retention.map((r) => (
                                <DataTableRow key={r.period}>
                                  <DataTableCell className="font-medium">{r.period}</DataTableCell>
                                  <DataTableCell>{r.start_count.toLocaleString()}</DataTableCell>
                                  <DataTableCell>{r.end_count.toLocaleString()}</DataTableCell>
                                  <DataTableCell>
                                    <span
                                      className={
                                        r.retention_pct > 90
                                          ? "text-emerald-600 font-medium"
                                          : r.retention_pct > 75
                                            ? "text-amber-600 font-medium"
                                            : "text-red-600 font-medium"
                                      }
                                    >
                                      {r.retention_pct.toFixed(1)}%
                                    </span>
                                  </DataTableCell>
                                  <DataTableCell>
                                    <span
                                      className={
                                        r.churn_pct > 25
                                          ? "text-red-600 font-medium"
                                          : r.churn_pct > 10
                                            ? "text-amber-600 font-medium"
                                            : "text-emerald-600 font-medium"
                                      }
                                    >
                                      {r.churn_pct.toFixed(1)}%
                                    </span>
                                  </DataTableCell>
                                </DataTableRow>
                              ))}
                            </DataTableBody>
                          </DataTable>
                        )}
                      </CardContent>
                    </Card>
                  </div>

                  {/* Top Users */}
                  <Card>
                    <CardHeader>
                      <CardTitle className="text-base">{t("analytics.topUsers")}</CardTitle>
                    </CardHeader>
                    <CardContent>
                      {(insights.top_users || []).length === 0 ? (
                        <p className="text-sm text-muted-foreground text-center py-8">{t("analytics.noUserData")}</p>
                      ) : (
                        <DataTable>
                          <DataTableHeader>
                            <DataTableRow>
                              <DataTableHead className="w-12">{t("analytics.rank")}</DataTableHead>
                              <DataTableHead>{t("common.email")}</DataTableHead>
                              <DataTableHead>{t("customers.licenses")}</DataTableHead>
                              <DataTableHead>{t("analytics.active")}</DataTableHead>
                              <DataTableHead>{t("analytics.activations")}</DataTableHead>
                              <DataTableHead>{t("analytics.usage")}</DataTableHead>
                            </DataTableRow>
                          </DataTableHeader>
                          <DataTableBody>
                            {(() => {
                              const maxUserUsage = Math.max(...insights.top_users.map((u) => u.total_usage), 1)
                              return insights.top_users.map((u, i) => (
                                <DataTableRow key={u.user_id}>
                                  <DataTableCell className="font-medium text-muted-foreground">{i + 1}</DataTableCell>
                                  <DataTableCell className="font-medium">{u.email}</DataTableCell>
                                  <DataTableCell>{u.license_count}</DataTableCell>
                                  <DataTableCell>{u.active_count}</DataTableCell>
                                  <DataTableCell>{u.activation_count}</DataTableCell>
                                  <DataTableCell>
                                    <div className="flex items-center gap-2">
                                      <div className="flex-1 max-w-[200px]">
                                        <div
                                          className="h-3 bg-violet-500 rounded-sm"
                                          style={{ width: `${(u.total_usage / maxUserUsage) * 100}%`, minWidth: 4 }}
                                        />
                                      </div>
                                      <span className="text-sm">{u.total_usage.toLocaleString()}</span>
                                    </div>
                                  </DataTableCell>
                                </DataTableRow>
                              ))
                            })()}
                          </DataTableBody>
                        </DataTable>
                      )}
                    </CardContent>
                  </Card>

                  {/* Recent Activity */}
                  <Card>
                    <CardHeader>
                      <CardTitle className="text-base">{t("analytics.recentActivity")}</CardTitle>
                    </CardHeader>
                    <CardContent>
                      {(insights.recent_activity || []).length === 0 ? (
                        <p className="text-sm text-muted-foreground text-center py-8">
                          {t("analytics.noRecentActivity")}
                        </p>
                      ) : (
                        <div className="space-y-2">
                          {insights.recent_activity.map((a) => (
                            <div key={a.id} className="flex items-center gap-3 py-2 border-b last:border-0 text-sm">
                              <span className="text-xs text-muted-foreground w-36 shrink-0">
                                {formatDate(a.created_at)}
                              </span>
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
                    </CardContent>
                  </Card>
                </div>
              )}
            </TabsContent>
          </Tabs>

          {/* Snapshots Raw Data — collapsible */}
          {snapshots.length > 0 && (
            <details className="group">
              <summary className="cursor-pointer text-sm font-medium text-muted-foreground hover:text-foreground transition-colors flex items-center gap-2">
                <span className="transition-transform group-open:rotate-90">▶</span>
                {t("analytics.rawData")} — {t("analytics.snapshots")} ({snapTotal})
              </summary>
              <Card className="mt-3">
                <CardContent className="pt-4">
                  <DataTable>
                    <DataTableHeader>
                      <DataTableRow>
                        <DataTableHead>{periodLabel(granularity, t)}</DataTableHead>
                        <DataTableHead>{t("analytics.totalLicenses")}</DataTableHead>
                        <DataTableHead>{t("analytics.active")}</DataTableHead>
                        <DataTableHead>{t("analytics.new")}</DataTableHead>
                        <DataTableHead>{t("analytics.churned")}</DataTableHead>
                        <DataTableHead>{t("analytics.netGrowth")}</DataTableHead>
                        <DataTableHead>{t("analytics.activations")}</DataTableHead>
                        <DataTableHead>{t("analytics.seats")}</DataTableHead>
                        <DataTableHead>{t("analytics.usage")}</DataTableHead>
                      </DataTableRow>
                    </DataTableHeader>
                    <DataTableBody>
                      {paginatedSnapshots.map((s, i) => {
                        const net = s.new_licenses - s.churned
                        return (
                          <DataTableRow key={("id" in s ? s.id : null) || i}>
                            <DataTableCell className="font-medium">{snapshotPeriod(s, granularity)}</DataTableCell>
                            <DataTableCell>{s.total_licenses}</DataTableCell>
                            <DataTableCell>{s.active_licenses}</DataTableCell>
                            <DataTableCell className="text-emerald-700">{s.new_licenses}</DataTableCell>
                            <DataTableCell className="text-red-700">{s.churned}</DataTableCell>
                            <DataTableCell className={net > 0 ? "text-emerald-700" : net < 0 ? "text-red-700" : ""}>
                              {net > 0 ? "+" : ""}
                              {net}
                            </DataTableCell>
                            <DataTableCell>{s.total_activations}</DataTableCell>
                            <DataTableCell>{s.total_seats}</DataTableCell>
                            <DataTableCell>{s.total_usage}</DataTableCell>
                          </DataTableRow>
                        )
                      })}
                    </DataTableBody>
                  </DataTable>
                  {snapTotal > 0 && (
                    <DataTablePagination
                      page={snapPage}
                      totalPages={snapTotalPages}
                      total={snapTotal}
                      pageSize={snapPageSize}
                      onPageChange={setSnapPage}
                      onPageSizeChange={setSnapPageSize}
                      pageSizeOptions={[10, 15, 30, 50]}
                    />
                  )}
                </CardContent>
              </Card>
            </details>
          )}
        </>
      )}
    </div>
  )
}

const BREAKDOWN_COLORS = ["#7c3aed", "#10b981", "#f59e0b", "#ef4444", "#3b82f6", "#a855f7", "#ec4899", "#14b8a6"]

function BreakdownCard({ title, items }: { title: string; items: { key: string; label: string; count: number }[] }) {
  const { t } = useI18n()
  const total = items.reduce((sum, item) => sum + item.count, 0)

  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-sm font-medium">{title}</CardTitle>
      </CardHeader>
      <CardContent>
        {items.length === 0 ? (
          <p className="text-sm text-muted-foreground text-center py-4">{t("common.noData")}</p>
        ) : (
          <div className="flex flex-col items-center gap-4">
            <ResponsiveContainer width="100%" height={160}>
              <PieChart>
                <Pie
                  data={items}
                  dataKey="count"
                  nameKey="label"
                  cx="50%"
                  cy="50%"
                  innerRadius={40}
                  outerRadius={70}
                  paddingAngle={2}
                >
                  {items.map((item, i) => (
                    <Cell key={item.key} fill={BREAKDOWN_COLORS[i % BREAKDOWN_COLORS.length]} />
                  ))}
                </Pie>
                <Tooltip
                  contentStyle={{
                    fontSize: 12,
                    borderRadius: 8,
                    background: "var(--color-popover)",
                    border: "1px solid var(--color-border)",
                    color: "var(--color-foreground)",
                  }}
                  formatter={(value) => [Number(value).toLocaleString(), ""]}
                />
              </PieChart>
            </ResponsiveContainer>
            <div className="w-full space-y-1.5">
              {items.map((item, i) => {
                const pct = total > 0 ? ((item.count / total) * 100).toFixed(1) : "0.0"
                return (
                  <div key={item.key} className="flex items-center justify-between text-xs">
                    <span className="flex items-center gap-2">
                      <span
                        className="inline-block w-2.5 h-2.5 rounded-full shrink-0"
                        style={{ backgroundColor: BREAKDOWN_COLORS[i % BREAKDOWN_COLORS.length] }}
                      />
                      <span className="font-medium">{item.label}</span>
                    </span>
                    <span className="text-muted-foreground">
                      {item.count.toLocaleString()} ({pct}%)
                    </span>
                  </div>
                )
              })}
            </div>
          </div>
        )}
      </CardContent>
    </Card>
  )
}
