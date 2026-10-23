import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { ArrowUpCircle, Check, RefreshCw, Send, Shield, Trash2, UserPlus } from "lucide-react"
import { useEffect, useState } from "react"
import { DEFAULT_REMINDER_DAYS, ReminderDaysInput } from "@/components/reminder-days-input"
import { showToast } from "@/components/toast"
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
import { DataTablePagination, useServerPagination } from "@/components/ui/data-table"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select"
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { useAuth } from "@/hooks/use-auth"
import { useI18n } from "@/i18n"
import { admin } from "@/lib/api"
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

// One switch per automated email an admin may turn off; mirrors
// service.ToggleableEmails on the server. Grouped as the page shows them.
const EMAIL_NOTIFY_GROUPS = [
  { group: "reminders", kinds: ["license_expiring", "renewal_reminder", "updates_ending", "trial_ending"] },
  {
    group: "status",
    kinds: ["license_expired", "trial_expired", "license_suspended", "subscription_canceled", "plan_changed"],
  },
  { group: "billing", kinds: ["payment_failed", "payment_recovered", "payment_action_required"] },
  { group: "usage", kinds: ["quota_warning", "welcome"] },
] as const

type EmailNotifyKind = (typeof EMAIL_NOTIFY_GROUPS)[number]["kinds"][number]
type EmailNotifyKey = `email_notify_${EmailNotifyKind}`
const notifyKey = (kind: EmailNotifyKind): EmailNotifyKey => `email_notify_${kind}`
// Derived from the groups, so a kind cannot be shown without being saved.
const EMAIL_NOTIFY_KEYS: EmailNotifyKey[] = EMAIL_NOTIFY_GROUPS.flatMap((g) => g.kinds.map(notifyKey))

// The webhook event fired alongside an email at the same moment, where
// there is one: an install that turns the email off can send its own
// from that event. (Not "subscription canceled": that mail goes out when
// the customer cancels, license.canceled only when the period ends.)
const EMAIL_NOTIFY_WEBHOOK: Partial<Record<EmailNotifyKind, string>> = {
  license_expired: "license.expired",
  trial_expired: "license.expired",
  license_suspended: "license.suspended",
  plan_changed: "plan.changed",
  payment_failed: "license.payment_failed",
  payment_recovered: "license.payment_recovered",
  quota_warning: "quota.warning",
}

// What an unset key means, so undoing an edit leaves the form clean:
// an email switch is on, the reminder days are the server's default.
const SETTING_DEFAULTS: Partial<Record<string, string>> = {
  expiry_reminder_days: DEFAULT_REMINDER_DAYS.join(","),
  ...Object.fromEntries(EMAIL_NOTIFY_KEYS.map((k) => [k, "true"])),
}
const effective = (key: string, value: string | undefined) => (value ?? "") || (SETTING_DEFAULTS[key] ?? "")

// The keys this form owns. Save posts only these: the settings table
// also holds rows the server writes for itself (Stripe webhook
// credentials) and secrets the API never returns, and PUTing the whole
// GET response back is what used to make every save fail once Stripe
// was configured.
//
// `as const` is load-bearing — set() only accepts a key from this list,
// so adding a field to the form without adding it here is a compile
// error rather than a control that silently never saves.
const FORM_KEYS = [
  "site_name",
  "timezone",
  "signup_mode",
  "brand_color",
  "logo_url",
  "maintenance_features_enabled",
  "email_provider",
  "smtp_from",
  "smtp_host",
  "smtp_port",
  "smtp_username",
  "smtp_password",
  "cloudflare_from",
  "cloudflare_account_id",
  "cloudflare_api_token",
  ...EMAIL_NOTIFY_KEYS,
  "expiry_reminder_days",
] as const

type FormKey = (typeof FORM_KEYS)[number]

export default function SettingsPage() {
  const { t, locale, setLocale } = useI18n()
  const qc = useQueryClient()
  const { data, isLoading } = useQuery({
    queryKey: ["admin", "settings"],
    queryFn: admin.getSettings,
  })

  const [form, setForm] = useState<Record<string, string>>({})

  useEffect(() => {
    if (!data?.settings) return
    // Re-seed from the server only when the form has no unsaved edits.
    // A background refetch — most of all the one clearing a secret
    // triggers, which flips secrets_set and so changes the query data —
    // would otherwise overwrite the whole form and silently discard any
    // field the admin has edited but not yet saved.
    setForm((prev) => {
      const dirty = FORM_KEYS.some((k) => k in prev && effective(k, prev[k]) !== effective(k, data.settings[k]))
      return dirty ? prev : data.settings
    })
  }, [data])

  // Only the fields this page actually changed are sent. Posting the
  // whole form would let a page opened hours ago write its stale
  // values back — most of all the maintenance switch, where saving an
  // unrelated field would silently re-confirm a rollout that an
  // incompatible build has since switched off.
  const changedSettings = () => {
    const changed = Object.fromEntries(
      FORM_KEYS.filter((k) => k in form && effective(k, form[k]) !== effective(k, data?.settings?.[k])).map((k) => [
        k,
        form[k],
      ]),
    )
    // A fresh install shows "SMTP" via the fallback without writing it
    // into the form, so filling the SMTP fields and saving would send
    // the credentials but not email_provider — the backend would store
    // them and keep using the env fallback, configured but never
    // active. Whenever the save configures a provider's fields, send
    // the active provider with them so it actually takes effect.
    const touchesEmail = Object.keys(changed).some((k) => k.startsWith("smtp_") || k.startsWith("cloudflare_"))
    if (touchesEmail && !("email_provider" in changed)) {
      changed.email_provider = emailProvider
    }
    return changed
  }

  const saveMut = useMutation({
    mutationFn: () => admin.updateSettings(changedSettings()),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["admin", "settings"] })
      // Secrets are write-only: the API never returns them, so replacing
      // one leaves the settings response byte-identical and the [data]
      // effect (guarded by structural sharing) never re-seeds the form.
      // Clear the secret inputs here so they fall back to the "saved"
      // placeholder instead of holding the plaintext — otherwise the
      // field stays dirty, the test button stays disabled, and every
      // later save re-submits the key.
      setForm((f) => {
        const next = { ...f }
        delete next.smtp_password
        delete next.cloudflare_api_token
        return next
      })
      showToast(t("settings.saved"), "success")
    },
    onError: (e: Error) => showToast(e.message, "error"),
  })

  const [testTo, setTestTo] = useState("")
  const testEmailMut = useMutation({
    mutationFn: () => admin.sendTestEmail(testTo.trim() || undefined),
    onSuccess: () => showToast(t("settings.testEmailSent"), "success"),
    onError: (e: Error) => showToast(e.message, "error"),
  })

  // A blank field means "leave the stored secret alone", so removing a
  // saved credential (e.g. purging a rotated-out Cloudflare token) needs
  // its own explicit call — the empty input can't express it.
  const clearSecretMut = useMutation({
    mutationFn: (key: string) => admin.clearSecretSetting(key),
    onSuccess: (_res, key) => {
      qc.invalidateQueries({ queryKey: ["admin", "settings"] })
      setForm((f) => {
        const next = { ...f }
        delete next[key]
        return next
      })
      showToast(t("settings.secretCleared"), "success")
    },
    onError: (e: Error) => showToast(e.message, "error"),
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

  const set = (key: FormKey, value: string) => setForm((f) => ({ ...f, [key]: value }))
  // The provider drives which fields show. Falls back to what the
  // server resolved (DB value, or "smtp" in env-fallback mode).
  const emailProvider = form.email_provider || data?.email?.provider || "smtp"
  // The test sends through the SAVED config, so it must not be offered
  // while the form holds unsaved email changes — otherwise "send test"
  // would verify the old config, not what is on screen.
  const emailDirty = Object.keys(changedSettings()).some(
    (k) => k === "email_provider" || k.startsWith("smtp_") || k.startsWith("cloudflare_"),
  )

  if (isLoading) {
    return (
      <div className="animate-pulse space-y-4">
        <div className="h-32 bg-muted rounded-lg" />
      </div>
    )
  }

  return (
    <div className="space-y-6">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div className="min-w-0">
          <h1 className="text-2xl font-bold tracking-tight sr-only md:not-sr-only">{t("settings.title")}</h1>
          <p className="text-muted-foreground">{t("settings.subtitle")}</p>
        </div>
        {/* The outcome goes to a toast, like every other page. Swapping
            the label to "Saved" resized the button under the cursor and
            was the only place in the dashboard that reported a result
            inside a control. */}
        <Button
          onClick={() => saveMut.mutate()}
          disabled={saveMut.isPending || Object.keys(changedSettings()).length === 0}
        >
          {saveMut.isPending ? t("common.loading") : t("common.save")}
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
              <div className="grid grid-cols-1 gap-6 sm:grid-cols-2">
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
                <div className="space-y-2 sm:col-span-2">
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
              {/* Email is configured here and stored in the database.
                  The SMTP_* env vars are the fallback when nothing is
                  set here — see EmailService.resolve. */}
              {data?.email?.source === "env" && (
                <p className="rounded-md bg-muted px-3 py-2 text-xs text-muted-foreground">
                  {t("settings.emailEnvBanner")}
                </p>
              )}

              <div className="grid grid-cols-1 gap-6 sm:grid-cols-2">
                <div className="space-y-2">
                  <Label>{t("settings.emailProvider")}</Label>
                  <Select value={emailProvider} onValueChange={(v) => set("email_provider", v)}>
                    <SelectTrigger>
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      <SelectItem value="smtp">SMTP</SelectItem>
                      <SelectItem value="cloudflare">Cloudflare Email</SelectItem>
                    </SelectContent>
                  </Select>
                  <p className="text-xs text-muted-foreground">{t("settings.emailProviderDesc")}</p>
                </div>
              </div>

              {emailProvider === "smtp" && (
                <div className="grid grid-cols-1 gap-6 sm:grid-cols-2">
                  <div className="space-y-2">
                    <Label>{t("settings.emailFrom")}</Label>
                    <Input
                      value={form.smtp_from ?? ""}
                      onChange={(e) => set("smtp_from", e.target.value)}
                      placeholder="noreply@yourdomain.com"
                    />
                    <p className="text-xs text-muted-foreground">{t("settings.emailFromDesc")}</p>
                  </div>
                  <div className="space-y-2">
                    <Label>{t("settings.emailHost")}</Label>
                    <Input
                      value={form.smtp_host ?? ""}
                      onChange={(e) => set("smtp_host", e.target.value)}
                      placeholder="smtp.example.com"
                    />
                  </div>
                  <div className="space-y-2">
                    <Label>{t("settings.emailPort")}</Label>
                    <Input
                      value={form.smtp_port ?? ""}
                      onChange={(e) => set("smtp_port", e.target.value)}
                      placeholder="587"
                    />
                  </div>
                  <div className="space-y-2">
                    <Label>{t("settings.emailUsername")}</Label>
                    <Input
                      value={form.smtp_username ?? ""}
                      onChange={(e) => set("smtp_username", e.target.value)}
                      placeholder="apikey"
                      autoComplete="off"
                    />
                  </div>
                  <div className="space-y-2">
                    <Label>{t("settings.emailPassword")}</Label>
                    <div className="flex items-center gap-2">
                      <Input
                        type="password"
                        value={form.smtp_password ?? ""}
                        onChange={(e) => set("smtp_password", e.target.value)}
                        placeholder={data?.secrets_set?.smtp_password ? t("settings.secretSaved") : ""}
                        autoComplete="new-password"
                        className="flex-1"
                      />
                      {data?.secrets_set?.smtp_password && !form.smtp_password && (
                        <Button
                          type="button"
                          variant="outline"
                          size="sm"
                          onClick={() => clearSecretMut.mutate("smtp_password")}
                          disabled={clearSecretMut.isPending}
                        >
                          {t("settings.secretClear")}
                        </Button>
                      )}
                    </div>
                    <p className="text-xs text-muted-foreground">{t("settings.secretKeepHint")}</p>
                  </div>
                </div>
              )}

              {emailProvider === "cloudflare" && (
                <div className="grid grid-cols-1 gap-6 sm:grid-cols-2">
                  <div className="space-y-2">
                    <Label>{t("settings.emailFrom")}</Label>
                    <Input
                      value={form.cloudflare_from ?? ""}
                      onChange={(e) => set("cloudflare_from", e.target.value)}
                      placeholder="noreply@yourdomain.com"
                    />
                    <p className="text-xs text-muted-foreground">{t("settings.emailFromDesc")}</p>
                  </div>
                  <div className="space-y-2">
                    <Label>{t("settings.emailCfAccount")}</Label>
                    <Input
                      value={form.cloudflare_account_id ?? ""}
                      onChange={(e) => set("cloudflare_account_id", e.target.value)}
                      placeholder="Cloudflare account ID"
                    />
                  </div>
                  <div className="space-y-2">
                    <Label>{t("settings.emailApiToken")}</Label>
                    <div className="flex items-center gap-2">
                      <Input
                        type="password"
                        value={form.cloudflare_api_token ?? ""}
                        onChange={(e) => set("cloudflare_api_token", e.target.value)}
                        placeholder={data?.secrets_set?.cloudflare_api_token ? t("settings.secretSaved") : ""}
                        autoComplete="new-password"
                        className="flex-1"
                      />
                      {data?.secrets_set?.cloudflare_api_token && !form.cloudflare_api_token && (
                        <Button
                          type="button"
                          variant="outline"
                          size="sm"
                          onClick={() => clearSecretMut.mutate("cloudflare_api_token")}
                          disabled={clearSecretMut.isPending}
                        >
                          {t("settings.secretClear")}
                        </Button>
                      )}
                    </div>
                    <p className="text-xs text-muted-foreground">{t("settings.secretKeepHint")}</p>
                  </div>
                </div>
              )}

              <div className="flex flex-wrap items-center gap-3 border-t pt-4">
                {data?.email?.configured ? (
                  <span className="flex items-center gap-2 text-sm text-emerald-600">
                    <Check className="h-4 w-4" /> {t("settings.emailConfigured")}
                  </span>
                ) : (
                  <span className="text-sm text-muted-foreground">{t("settings.emailNotConfigured")}</span>
                )}
                <div className="flex-1" />
                <Input
                  className="w-full sm:w-56"
                  value={testTo}
                  onChange={(e) => setTestTo(e.target.value)}
                  placeholder={t("settings.testEmailToPlaceholder")}
                />
                <Button
                  variant="outline"
                  onClick={() => testEmailMut.mutate()}
                  disabled={testEmailMut.isPending || emailDirty || !data?.email?.configured}
                >
                  <Send className="h-4 w-4 mr-2" />
                  {t("settings.testEmail")}
                </Button>
              </div>
              <p className="text-xs text-muted-foreground">
                {emailDirty ? t("settings.testEmailDirtyHint") : t("settings.testEmailSaveHint")}
              </p>
            </CardContent>
          </Card>

          <Card>
            <CardHeader>
              <CardTitle className="text-base">{t("settings.autoEmails")}</CardTitle>
              <p className="text-sm text-muted-foreground">{t("settings.autoEmailsDesc")}</p>
            </CardHeader>
            <CardContent className="grid gap-6 sm:grid-cols-2">
              {EMAIL_NOTIFY_GROUPS.map(({ group, kinds }) => (
                <fieldset key={group} className="space-y-2">
                  <legend className="mb-1 text-sm font-medium">{t(`settings.autoEmailsGroup.${group}`)}</legend>
                  {kinds.map((kind) => {
                    const key = notifyKey(kind)
                    const enabled = form[key] !== "false"
                    return (
                      <div key={kind} className="space-y-2">
                        <div className="flex items-center gap-3">
                          <input
                            type="checkbox"
                            id={key}
                            // Unset means on: only "false" turns an email off.
                            checked={form[key] !== "false"}
                            onChange={(e) => set(key, e.target.checked ? "true" : "false")}
                            className="h-4 w-4 rounded border-input accent-primary"
                          />
                          <Label htmlFor={key} className="font-normal">
                            {t(`settings.notify.${kind}`)}
                          </Label>
                          {EMAIL_NOTIFY_WEBHOOK[kind] && (
                            <span
                              className="rounded bg-muted px-1.5 py-0.5 font-mono text-[10px] text-muted-foreground"
                              title={EMAIL_NOTIFY_WEBHOOK[kind]}
                            >
                              webhook
                            </span>
                          )}
                        </div>
                        {/* When the expiry reminders go out belongs with
                        their switch: editing it is pointless while off. */}
                        {kind === "license_expiring" && (
                          <div className="ml-7">
                            <ReminderDaysInput
                              value={form.expiry_reminder_days}
                              onChange={(v) => set("expiry_reminder_days", v)}
                              disabled={!enabled}
                            />
                          </div>
                        )}
                      </div>
                    )
                  })}
                </fieldset>
              ))}
              <p className="text-xs text-muted-foreground sm:col-span-2">{t("settings.autoEmailsAlways")}</p>
            </CardContent>
          </Card>
        </TabsContent>

        <TabsContent value="templates" className="space-y-6">
          <EmailTemplatesManager />
        </TabsContent>

        <TabsContent value="security" className="space-y-6">
          <Card>
            <CardHeader>
              <CardTitle className="text-base">{t("settings.signup")}</CardTitle>
            </CardHeader>
            <CardContent className="space-y-2">
              <Label>{t("settings.signupMode")}</Label>
              <Select value={form.signup_mode || "open"} onValueChange={(v) => set("signup_mode", v)}>
                <SelectTrigger className="w-full sm:w-72">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="open">{t("settings.signupOpen")}</SelectItem>
                  <SelectItem value="licensed_only">{t("settings.signupLicensedOnly")}</SelectItem>
                </SelectContent>
              </Select>
              <p className="text-xs text-muted-foreground">{t("settings.signupModeDesc")}</p>
            </CardContent>
          </Card>
          <Card>
            <CardHeader>
              <CardTitle>{t("settings.maintenance")}</CardTitle>
            </CardHeader>
            <CardContent className="space-y-2">
              <div className="flex items-center gap-3">
                <input
                  type="checkbox"
                  id="maintenance-features"
                  checked={form.maintenance_features_enabled === "true"}
                  onChange={(e) => set("maintenance_features_enabled", e.target.checked ? "true" : "false")}
                  className="h-4 w-4 rounded border-input accent-primary"
                />
                <Label htmlFor="maintenance-features">{t("settings.maintenanceEnabled")}</Label>
              </div>
              <p className="text-xs text-muted-foreground">{t("settings.maintenanceDesc")}</p>
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
              <div className="grid grid-cols-1 gap-4 text-sm sm:grid-cols-3">
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

  const pg = useServerPagination(20)
  const { data, isLoading } = useQuery({
    queryKey: ["admin", "team", pg.page, pg.pageSize],
    queryFn: () => admin.listTeam(pg.params),
  })

  const inviteMut = useMutation({
    mutationFn: () => admin.inviteTeamMember({ email, role }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["admin", "team"] })
      setEmail("")
    },
    onError: (e: Error) => showToast(e.message, "error"),
  })

  const removeMut = useMutation({
    mutationFn: (id: string) => admin.removeTeamMember(id),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["admin", "team"] }),
    onError: (e: Error) => showToast(e.message, "error"),
  })

  const { items: members, total, totalPages } = pg.from(data, data?.members)

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
          {total > pg.pageSize && (
            <DataTablePagination
              page={pg.page}
              totalPages={totalPages}
              total={total}
              pageSize={pg.pageSize}
              onPageChange={pg.setPage}
              onPageSizeChange={pg.setPageSize}
            />
          )}

          {/* Invite form (owner only).

            The row wraps: three controls do not fit across a phone,
            and unwrapped the address field was left 46px, which is not
            a field. The address keeps a floor wide enough to read what
            you typed, so it is the one that pushes the others down. */}
          {isOwner ? (
            <div className="flex flex-wrap items-end gap-3 pt-4 border-t">
              <div className="min-w-50 flex-1 space-y-2">
                <Label className="text-xs">{t("team.email")}</Label>
                <Input
                  type="email"
                  value={email}
                  onChange={(e) => setEmail(e.target.value)}
                  placeholder="colleague@company.com"
                />
              </div>
              <div className="w-32 shrink-0 space-y-2">
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
