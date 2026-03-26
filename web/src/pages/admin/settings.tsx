import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { ArrowUpCircle, Check, Database, Mail, RefreshCw, Send, Shield, Trash2, UserPlus } from "lucide-react"
import { useEffect, useState } from "react"
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogHeader,
  AlertDialogTitle,
  AlertDialogTrigger,
} from "@/components/ui/alert-dialog"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select"
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { useAuth } from "@/hooks/use-auth"
import { useI18n } from "@/i18n"
import type { User } from "@/lib/api"
import { admin } from "@/lib/api"
import { formatDate } from "@/lib/utils"
import EmailTemplatesManager from "@/pages/admin/email-templates"

const TIMEZONES = [
  { value: "UTC", label: "UTC +0:00", city: "UTC" },
  { value: "Pacific/Midway", label: "UTC -11:00", city: "Midway" },
  { value: "Pacific/Honolulu", label: "UTC -10:00", city: "Honolulu" },
  { value: "America/Anchorage", label: "UTC -9:00", city: "Anchorage" },
  { value: "America/Los_Angeles", label: "UTC -8:00", city: "Los Angeles" },
  { value: "America/Denver", label: "UTC -7:00", city: "Denver" },
  { value: "America/Chicago", label: "UTC -6:00", city: "Chicago" },
  { value: "America/New_York", label: "UTC -5:00", city: "New York" },
  { value: "America/Caracas", label: "UTC -4:00", city: "Caracas" },
  { value: "America/Sao_Paulo", label: "UTC -3:00", city: "Sao Paulo" },
  { value: "Atlantic/South_Georgia", label: "UTC -2:00", city: "South Georgia" },
  { value: "Atlantic/Azores", label: "UTC -1:00", city: "Azores" },
  { value: "Europe/London", label: "UTC +0:00", city: "London" },
  { value: "Europe/Paris", label: "UTC +1:00", city: "Paris / Berlin" },
  { value: "Europe/Helsinki", label: "UTC +2:00", city: "Helsinki / Cairo" },
  { value: "Europe/Moscow", label: "UTC +3:00", city: "Moscow" },
  { value: "Asia/Dubai", label: "UTC +4:00", city: "Dubai" },
  { value: "Asia/Karachi", label: "UTC +5:00", city: "Karachi" },
  { value: "Asia/Kolkata", label: "UTC +5:30", city: "Kolkata / Mumbai" },
  { value: "Asia/Dhaka", label: "UTC +6:00", city: "Dhaka" },
  { value: "Asia/Bangkok", label: "UTC +7:00", city: "Bangkok" },
  { value: "Asia/Shanghai", label: "UTC +8:00", city: "Shanghai / Singapore" },
  { value: "Asia/Tokyo", label: "UTC +9:00", city: "Tokyo / Seoul" },
  { value: "Australia/Sydney", label: "UTC +10:00", city: "Sydney" },
  { value: "Pacific/Noumea", label: "UTC +11:00", city: "Noumea" },
  { value: "Pacific/Auckland", label: "UTC +12:00", city: "Auckland" },
]

export default function SettingsPage() {
  const { t, locale, setLocale } = useI18n()
  const qc = useQueryClient()
  const { data, isLoading } = useQuery({
    queryKey: ["admin", "settings"],
    queryFn: admin.getSettings,
  })

  const [form, setForm] = useState<Record<string, string>>({})
  const [saved, setSaved] = useState(false)

  useEffect(() => {
    if (data?.settings) {
      setForm(data.settings)
    }
  }, [data])

  const saveMut = useMutation({
    mutationFn: () => admin.updateSettings(form),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["admin", "settings"] })
      setSaved(true)
      setTimeout(() => setSaved(false), 3000)
    },
  })

  const testEmailMut = useMutation({
    mutationFn: admin.sendTestEmail,
  })

  const { data: versionData } = useQuery({
    queryKey: ["admin", "version"],
    queryFn: admin.getVersion,
  })

  const {
    data: updateData,
    refetch: recheckUpdate,
    isFetching: updateChecking,
  } = useQuery({
    queryKey: ["admin", "update-check"],
    queryFn: admin.checkUpdate,
    staleTime: 60 * 60 * 1000, // cache 1 hour
  })

  const { data: migrationsData } = useQuery({
    queryKey: ["admin", "migrations"],
    queryFn: admin.getMigrations,
  })

  const set = (key: string, value: string) => setForm((f) => ({ ...f, [key]: value }))

  if (isLoading) {
    return (
      <div className="animate-pulse space-y-4">
        <div className="h-32 bg-muted rounded-lg" />
      </div>
    )
  }

  return (
    <div className="space-y-6">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-bold tracking-tight">{t("settings.title")}</h1>
          <p className="text-muted-foreground">{t("settings.subtitle")}</p>
        </div>
        <Button onClick={() => saveMut.mutate()} disabled={saveMut.isPending}>
          {saved ? (
            <>
              <Check className="h-4 w-4 mr-2" /> {t("settings.saved")}
            </>
          ) : saveMut.isPending ? (
            t("common.loading")
          ) : (
            t("common.save")
          )}
        </Button>
      </div>

      <Tabs defaultValue="general">
        <TabsList>
          <TabsTrigger value="general">{t("settings.general")}</TabsTrigger>
          <TabsTrigger value="team">{t("team.title")}</TabsTrigger>
          <TabsTrigger value="email">{t("settings.email")}</TabsTrigger>
          <TabsTrigger value="templates">{t("settings.emailTemplates")}</TabsTrigger>
          <TabsTrigger value="security">{t("settings.security")}</TabsTrigger>
          <TabsTrigger value="system">{t("settings.system")}</TabsTrigger>
        </TabsList>

        <TabsContent value="general" className="space-y-6">
          <Card>
            <CardHeader>
              <CardTitle className="text-base">{t("settings.general")}</CardTitle>
            </CardHeader>
            <CardContent className="space-y-6">
              <div className="grid grid-cols-2 gap-6">
                <div className="space-y-2">
                  <Label>{t("settings.siteName")}</Label>
                  <Input
                    value={form.site_name || ""}
                    onChange={(e) => set("site_name", e.target.value)}
                    placeholder="Keygate"
                  />
                  <p className="text-xs text-muted-foreground">{t("settings.siteNameDesc")}</p>
                </div>
                <div className="space-y-2">
                  <Label>{t("settings.timezone")}</Label>
                  <Select value={form.timezone || "UTC"} onValueChange={(v) => set("timezone", v)}>
                    <SelectTrigger>
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      {TIMEZONES.map((tz) => (
                        <SelectItem key={tz.value} value={tz.value}>
                          <span className="font-mono text-xs">{tz.label}</span>
                          <span className="ml-2 text-muted-foreground">{tz.city}</span>
                        </SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                  <p className="text-xs text-muted-foreground">{t("settings.timezoneDesc")}</p>
                </div>
                <div className="space-y-2">
                  <Label>{t("settings.language")}</Label>
                  <Select value={locale} onValueChange={(v) => setLocale(v as "en" | "zh")}>
                    <SelectTrigger>
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      <SelectItem value="en">English</SelectItem>
                      <SelectItem value="zh">中文</SelectItem>
                    </SelectContent>
                  </Select>
                  <p className="text-xs text-muted-foreground">{t("settings.languageDesc")}</p>
                </div>
                <div className="space-y-2">
                  <Label>{t("settings.brandColor")}</Label>
                  <div className="flex items-center gap-3">
                    <input
                      type="color"
                      value={form.brand_color || "#7c3aed"}
                      onChange={(e) => set("brand_color", e.target.value)}
                      className="h-9 w-14 rounded border cursor-pointer"
                    />
                    <Input
                      value={form.brand_color || ""}
                      onChange={(e) => set("brand_color", e.target.value)}
                      placeholder="#7c3aed"
                      className="w-32 font-mono text-sm"
                    />
                    {form.brand_color && (
                      <Button variant="ghost" size="sm" onClick={() => set("brand_color", "")}>
                        {t("settings.resetTemplate")}
                      </Button>
                    )}
                  </div>
                  <p className="text-xs text-muted-foreground">{t("settings.brandColorDesc")}</p>
                </div>
                <div className="space-y-2 col-span-2">
                  <Label>{t("settings.logoUrl")}</Label>
                  <div className="flex items-center gap-3">
                    {form.logo_url && <img src={form.logo_url} alt="Custom logo" className="h-8 w-8 rounded border" />}
                    <Input
                      value={form.logo_url || ""}
                      onChange={(e) => set("logo_url", e.target.value)}
                      placeholder="https://example.com/logo.svg"
                      className="flex-1"
                    />
                  </div>
                  <p className="text-xs text-muted-foreground">{t("settings.logoUrlDesc")}</p>
                </div>
              </div>
            </CardContent>
          </Card>
        </TabsContent>

        <TabsContent value="team" className="space-y-6">
          <TeamManagement />
        </TabsContent>

        <TabsContent value="email" className="space-y-6">
          <Card>
            <CardHeader>
              <CardTitle className="text-base">{t("settings.email")}</CardTitle>
            </CardHeader>
            <CardContent className="space-y-6">
              <div className="grid grid-cols-2 gap-6">
                <div className="space-y-2">
                  <Label>{t("settings.smtpHost")}</Label>
                  <Input
                    value={form.smtp_host || ""}
                    onChange={(e) => set("smtp_host", e.target.value)}
                    placeholder="smtp.example.com"
                  />
                </div>
                <div className="space-y-2">
                  <Label>{t("settings.smtpPort")}</Label>
                  <Input
                    value={form.smtp_port || ""}
                    onChange={(e) => set("smtp_port", e.target.value)}
                    placeholder="587"
                  />
                </div>
                <div className="space-y-2">
                  <Label>{t("settings.smtpUsername")}</Label>
                  <Input value={form.smtp_username || ""} onChange={(e) => set("smtp_username", e.target.value)} />
                </div>
                <div className="space-y-2">
                  <Label>{t("settings.smtpPassword")}</Label>
                  <Input
                    type="password"
                    value={form.smtp_password || ""}
                    onChange={(e) => set("smtp_password", e.target.value)}
                  />
                </div>
                <div className="space-y-2 col-span-2">
                  <Label>{t("settings.smtpFrom")}</Label>
                  <Input
                    type="email"
                    value={form.smtp_from || ""}
                    onChange={(e) => set("smtp_from", e.target.value)}
                    placeholder="noreply@example.com"
                  />
                  <p className="text-xs text-muted-foreground">{t("settings.smtpFromDesc")}</p>
                </div>
              </div>
              <div className="flex items-center gap-4 pt-2">
                <Button variant="outline" onClick={() => testEmailMut.mutate()} disabled={testEmailMut.isPending}>
                  <Send className="h-4 w-4 mr-2" />
                  {t("settings.testEmail")}
                </Button>
                {testEmailMut.isSuccess && (
                  <span className="text-sm text-emerald-600 flex items-center gap-1">
                    <Mail className="h-4 w-4" /> {t("settings.testEmailSent")}
                  </span>
                )}
              </div>
            </CardContent>
          </Card>
        </TabsContent>

        <TabsContent value="templates" className="space-y-6">
          <EmailTemplatesManager />
        </TabsContent>

        <TabsContent value="security" className="space-y-6">
          <Card>
            <CardHeader>
              <CardTitle className="text-base">{t("settings.rateLimit")}</CardTitle>
            </CardHeader>
            <CardContent className="space-y-6">
              <div className="grid grid-cols-2 gap-6">
                <div className="space-y-2">
                  <Label>{t("settings.rateLimitApi")}</Label>
                  <Input
                    type="number"
                    value={form.rate_limit_api || ""}
                    onChange={(e) => set("rate_limit_api", e.target.value)}
                    placeholder="60"
                  />
                  <p className="text-xs text-muted-foreground">{t("settings.rateLimitApiDesc")}</p>
                </div>
                <div className="space-y-2">
                  <Label>{t("settings.rateLimitAdmin")}</Label>
                  <Input
                    type="number"
                    value={form.rate_limit_admin || ""}
                    onChange={(e) => set("rate_limit_admin", e.target.value)}
                    placeholder="120"
                  />
                  <p className="text-xs text-muted-foreground">{t("settings.rateLimitAdminDesc")}</p>
                </div>
              </div>
            </CardContent>
          </Card>

          <Card>
            <CardHeader>
              <CardTitle className="text-base">{t("settings.webhookConfig")}</CardTitle>
            </CardHeader>
            <CardContent className="space-y-6">
              <div className="grid grid-cols-2 gap-6">
                <div className="space-y-2">
                  <Label>{t("settings.webhookMaxAttempts")}</Label>
                  <Input
                    type="number"
                    value={form.webhook_max_attempts || ""}
                    onChange={(e) => set("webhook_max_attempts", e.target.value)}
                    placeholder="5"
                  />
                </div>
                <div className="space-y-2">
                  <Label>{t("settings.webhookTimeout")}</Label>
                  <Input
                    value={form.webhook_timeout || ""}
                    onChange={(e) => set("webhook_timeout", e.target.value)}
                    placeholder="10s"
                  />
                </div>
                <div className="space-y-2">
                  <Label>{t("settings.quotaThreshold")}</Label>
                  <Input
                    type="number"
                    step="0.1"
                    min="0"
                    max="1"
                    value={form.quota_warning_threshold || ""}
                    onChange={(e) => set("quota_warning_threshold", e.target.value)}
                    placeholder="0.8"
                  />
                  <p className="text-xs text-muted-foreground">{t("settings.quotaThresholdDesc")}</p>
                </div>
              </div>
            </CardContent>
          </Card>
        </TabsContent>

        <TabsContent value="system" className="space-y-6">
          {/* Version Info */}
          <Card>
            <CardHeader>
              <CardTitle className="text-base">{t("settings.versionInfo")}</CardTitle>
            </CardHeader>
            <CardContent className="space-y-4">
              <div className="grid grid-cols-3 gap-4 text-sm">
                <div>
                  <p className="text-muted-foreground">{t("settings.currentVersion")}</p>
                  <p className="font-mono font-semibold mt-1">{versionData?.version || "dev"}</p>
                </div>
                <div>
                  <p className="text-muted-foreground">{t("settings.commitHash")}</p>
                  <p className="font-mono mt-1">{versionData?.commit || "-"}</p>
                </div>
                <div>
                  <p className="text-muted-foreground">{t("settings.buildDate")}</p>
                  <p className="mt-1">{versionData?.build_date || "-"}</p>
                </div>
              </div>

              <div className="flex items-center gap-3 pt-2">
                <Button variant="outline" size="sm" onClick={() => recheckUpdate()} disabled={updateChecking}>
                  <RefreshCw className={`h-4 w-4 mr-2 ${updateChecking ? "animate-spin" : ""}`} />
                  {t("settings.checkUpdate")}
                </Button>

                {updateData &&
                  (updateData.available ? (
                    <div className="flex items-center gap-2 bg-blue-50 text-blue-800 rounded-lg px-4 py-2 text-sm">
                      <ArrowUpCircle className="h-4 w-4" />
                      <span>{t("settings.updateAvailable", { version: updateData.latest_version })}</span>
                      {updateData.release_url && (
                        <a
                          href={updateData.release_url}
                          target="_blank"
                          rel="noopener noreferrer"
                          className="underline font-medium ml-1"
                        >
                          {t("settings.viewRelease")}
                        </a>
                      )}
                    </div>
                  ) : (
                    <span className="text-sm text-emerald-600 flex items-center gap-1">
                      <Check className="h-4 w-4" />
                      {t("settings.upToDate")}
                    </span>
                  ))}
              </div>

              {updateData?.changelog && updateData.available && (
                <div className="mt-4">
                  <p className="text-sm font-medium mb-2">{t("settings.changelog")}</p>
                  <pre className="text-xs bg-muted rounded-lg p-4 overflow-auto max-h-48 whitespace-pre-wrap">
                    {updateData.changelog}
                  </pre>
                </div>
              )}
            </CardContent>
          </Card>

          {/* Database Migrations */}
          <Card>
            <CardHeader>
              <CardTitle className="text-base flex items-center gap-2">
                <Database className="h-4 w-4" />
                {t("settings.migrations")}
              </CardTitle>
            </CardHeader>
            <CardContent>
              {migrationsData?.migrations && migrationsData.migrations.length > 0 ? (
                <div className="space-y-1">
                  {migrationsData.migrations.map((m) => (
                    <div
                      key={m.filename}
                      className="flex items-center justify-between text-sm py-1.5 border-b last:border-0"
                    >
                      <code className="text-xs">{m.filename}</code>
                      <span className="text-xs text-muted-foreground">{formatDate(m.applied_at)}</span>
                    </div>
                  ))}
                </div>
              ) : (
                <p className="text-sm text-muted-foreground">{t("common.noData")}</p>
              )}
            </CardContent>
          </Card>
        </TabsContent>
      </Tabs>
    </div>
  )
}

function TeamManagement() {
  const { t } = useI18n()
  const { user } = useAuth()
  const qc = useQueryClient()
  const [email, setEmail] = useState("")
  const [role, setRole] = useState("admin")
  const isOwner = user?.role === "owner"

  const { data, isLoading } = useQuery({
    queryKey: ["admin", "team"],
    queryFn: admin.listTeam,
  })

  const inviteMut = useMutation({
    mutationFn: () => admin.inviteTeamMember({ email, role }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["admin", "team"] })
      setEmail("")
    },
  })

  const removeMut = useMutation({
    mutationFn: (id: string) => admin.removeTeamMember(id),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["admin", "team"] }),
  })

  const members: User[] = data?.members || []

  return (
    <div className="space-y-6">
      <Card>
        <CardHeader>
          <CardTitle className="text-base flex items-center gap-2">
            <Shield className="h-4 w-4" />
            {t("team.title")}
          </CardTitle>
        </CardHeader>
        <CardContent className="space-y-4">
          <p className="text-sm text-muted-foreground">{t("team.subtitle")}</p>

          {/* Current members */}
          {isLoading ? (
            <div className="h-24 bg-muted rounded-lg animate-pulse" />
          ) : members.length === 0 ? (
            <p className="text-sm text-muted-foreground py-4">{t("team.empty")}</p>
          ) : (
            <div className="space-y-2">
              {members.map((m) => (
                <div key={m.id} className="flex items-center justify-between py-2 px-3 rounded-lg border">
                  <div className="flex items-center gap-3">
                    {m.avatar_url ? (
                      <img src={m.avatar_url} className="h-8 w-8 rounded-full" alt="" />
                    ) : (
                      <div className="h-8 w-8 rounded-full bg-muted flex items-center justify-center text-xs font-bold">
                        {m.name?.charAt(0)?.toUpperCase() || m.email.charAt(0).toUpperCase()}
                      </div>
                    )}
                    <div>
                      <div className="font-medium text-sm">{m.name || m.email}</div>
                      <div className="text-xs text-muted-foreground">{m.email}</div>
                    </div>
                  </div>
                  <div className="flex items-center gap-2">
                    <Badge variant={m.role === "owner" ? "default" : "secondary"}>{m.role}</Badge>
                    {isOwner && m.id !== user?.id && (
                      <AlertDialog>
                        <AlertDialogTrigger asChild>
                          <Button variant="ghost" size="icon" className="h-8 w-8 text-destructive">
                            <Trash2 className="h-4 w-4" />
                          </Button>
                        </AlertDialogTrigger>
                        <AlertDialogContent>
                          <AlertDialogHeader>
                            <AlertDialogTitle>{t("team.remove")}</AlertDialogTitle>
                            <AlertDialogDescription>{t("team.removeConfirm")}</AlertDialogDescription>
                          </AlertDialogHeader>
                          <div className="flex justify-end gap-2 mt-4">
                            <AlertDialogCancel>{t("common.cancel")}</AlertDialogCancel>
                            <AlertDialogAction onClick={() => removeMut.mutate(m.id)}>
                              {t("team.remove")}
                            </AlertDialogAction>
                          </div>
                        </AlertDialogContent>
                      </AlertDialog>
                    )}
                  </div>
                </div>
              ))}
            </div>
          )}

          {/* Invite form (owner only) */}
          {isOwner ? (
            <div className="flex items-end gap-3 pt-4 border-t">
              <div className="flex-1 space-y-2">
                <Label className="text-xs">{t("team.email")}</Label>
                <Input
                  type="email"
                  value={email}
                  onChange={(e) => setEmail(e.target.value)}
                  placeholder="colleague@company.com"
                />
              </div>
              <div className="w-32 space-y-2">
                <Label className="text-xs">{t("team.role")}</Label>
                <Select value={role} onValueChange={setRole}>
                  <SelectTrigger>
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="admin">Admin</SelectItem>
                    <SelectItem value="owner">Owner</SelectItem>
                  </SelectContent>
                </Select>
              </div>
              <Button onClick={() => inviteMut.mutate()} disabled={!email || inviteMut.isPending}>
                <UserPlus className="h-4 w-4 mr-2" />
                {t("team.invite")}
              </Button>
            </div>
          ) : (
            <p className="text-sm text-muted-foreground pt-4 border-t">{t("team.ownerOnly")}</p>
          )}
        </CardContent>
      </Card>
    </div>
  )
}
