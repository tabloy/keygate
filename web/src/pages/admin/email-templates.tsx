import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { Code, Eye, Mail, Pencil, RotateCcw } from "lucide-react"
import { useState } from "react"
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
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import { Dialog, DialogBody, DialogContent, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { type TranslationKeys, useI18n } from "@/i18n"
import { admin } from "@/lib/api"

// Headings for the groups the server puts each email in.
const GROUP_TITLE: Record<string, TranslationKeys> = {
  licenses: "settings.templateGroupLicenses",
  billing: "settings.templateGroupBilling",
  account: "settings.templateGroupAccount",
}

// Label for each email. The list of emails, their group, variables and
// order come from the server (service.emailTemplates), so an email added
// there is listed in its group even before it has a label here.
// fallbackNote: trials that move, or will move, to their plan's fallback
// plan get this email too, so a customised copy should name it with
// {{.FallbackPlan}}; the key of the note that says so.
const TEMPLATE_META: Record<string, { label: string; fallbackNote?: TranslationKeys }> = {
  license_created: { label: "License Created" },
  license_expiring: {
    label: "License Expiring",
    fallbackNote: "settings.templateFallbackNoteExpiring",
  },
  updates_ending: { label: "Updates Ending" },
  license_expired: { label: "License Expired" },
  trial_expired: { label: "Trial Expired", fallbackNote: "settings.templateFallbackNoteExpired" },
  license_suspended: { label: "License Suspended" },
  quota_warning: { label: "Quota Warning" },
  seat_invite: { label: "Seat Invite" },
  trial_ending: { label: "Trial Ending (Stripe)" },
  renewal_reminder: { label: "Renewal Reminder" },
  plan_changed: { label: "Plan Changed" },
  subscription_canceled: { label: "Subscription Canceled" },
  payment_failed: { label: "Payment Failed" },
  dunning_second: { label: "Payment Still Outstanding" },
  dunning_final: { label: "Final Payment Notice" },
  payment_action_required: { label: "Payment Authentication Required" },
  payment_recovered: { label: "Payment Recovered" },
  welcome: { label: "Welcome" },
  login_code: { label: "Login Code" },
  admin_invite: { label: "Admin Invite" },
}

const labelOf = (key: string) => TEMPLATE_META[key]?.label || key

const PREVIEW_DATA: Record<string, string> = {
  Product: "AwesomeApp",
  Plan: "Pro",
  LicenseKey: "KG-XXXX-XXXX-XXXX-XXXX",
  ExpiresAt: "2026-06-15",
  UpdatesUntil: "2027-06-15",
  Feature: "api_calls",
  Used: "800",
  Limit: "1000",
  Pct: "80",
  Inviter: "admin@company.com",
  Reason: "Policy violation",
  DaysLeft: "3",
  IsTrial: "true",
  // Empty: most trials simply end, so that is what the preview shows.
  FallbackPlan: "",
  DownloadURL: "https://example.com/download",
  InviteURL: "https://licenses.example.com/accept-invite?token=…",
  SiteName: "Acme Licensing",
  PortalURL: "https://licenses.example.com/portal",
  Name: "Alex",
  Code: "482913",
  OldPlan: "Basic",
  NewPlan: "Pro",
  Immediate: "true",
  InvoiceURL: "https://invoice.example.com/i/123",
  TrialEnd: "2026-06-15",
  RenewalDate: "2026-06-15",
  Role: "admin",
  LoginURL: "https://licenses.example.com/login",
}

// previewIsExact reports whether fillTemplate can follow every action in
// src: fields, and {{if}} over fields (with "and"), {{else}} and {{end}}.
// Anything else (not, or, with, else if, comparisons, trim markers) is
// shown as an approximation, and the preview says so.
function previewIsExact(src: string): boolean {
  return [...tidyActions(src).matchAll(/\{\{([^}]*)\}\}/g)].every(([, action]) =>
    /^(\.\w+|if (and )?\.\w+( \.\w+)*|else|end)$/.test(action),
  )
}

// tidyActions writes {{ .Product }} as {{.Product}} and {{ if  .X }} as
// {{if .X}}, as Go reads them the same; a trim marker ({{- or -}}) is
// left alone, so the preview calls it approximate.
function tidyActions(src: string): string {
  return src.replace(/\{\{(\s*)([^{}]*?)(\s*)\}\}/g, (all, _lead, action: string) =>
    action.startsWith("-") || action.endsWith("-") ? all : `{{${action.replace(/\s+/g, " ")}}}`,
  )
}

// fillTemplate previews a template with PREVIEW_DATA. Each {{if}} takes
// the branch the sample data picks, nested ones too: its first branch
// when every .Field it names is set (and not "false"), else its {{else}}.
function fillTemplate(html: string): string {
  const holds = (cond: string) =>
    cond
      .split(/\s+/)
      .filter((word) => word.startsWith("."))
      .every((ref) => {
        const v = PREVIEW_DATA[ref.slice(1)]
        return !!v && v !== "false"
      })
  const open: { shown: boolean; outer: boolean; held: boolean }[] = []
  const shown = () => open.length === 0 || open[open.length - 1].shown
  let result = ""
  for (const part of tidyActions(html).split(/(\{\{if [^}]+\}\}|\{\{else\}\}|\{\{end\}\})/)) {
    if (part.startsWith("{{if ")) {
      const outer = shown()
      const held = holds(part.slice(5, -2))
      open.push({ shown: outer && held, outer, held })
    } else if (part === "{{else}}" && open.length > 0) {
      const top = open[open.length - 1]
      top.shown = top.outer && !top.held
    } else if (part === "{{end}}" && open.length > 0) {
      open.pop()
    } else if (shown()) {
      result += part
    }
  }
  for (const [k, v] of Object.entries(PREVIEW_DATA)) {
    result = result.replaceAll(`{{.${k}}}`, v)
  }
  return result
}

export default function EmailTemplatesManager() {
  const { t } = useI18n()
  const qc = useQueryClient()
  const [editing, setEditing] = useState<string | null>(null)
  const [editValue, setEditValue] = useState("")
  const [editSubject, setEditSubject] = useState("")
  const [previewing, setPreviewing] = useState<string | null>(null)

  const { data, isLoading } = useQuery({
    queryKey: ["admin", "email-templates"],
    queryFn: admin.getEmailTemplates,
  })

  const saveMut = useMutation({
    // Only what was edited is sent, so changing the subject does not
    // re-save (and re-validate) a body saved under older rules. A body or
    // subject equal to the default is stored empty, so the email keeps
    // following the default if a later version changes it.
    mutationFn: (params: { key: string; value: string; subject: string }) => {
      const tmpl = templates[params.key]
      const changes: Record<string, string> = {}
      if (params.value !== (tmpl?.custom || tmpl?.default || "")) {
        changes[`email_template_${params.key}`] =
          params.value.trim() === (tmpl?.default || "").trim() ? "" : params.value
      }
      const subject = params.subject.trim()
      if (subject !== (tmpl?.subject_custom || tmpl?.subject_default || "")) {
        changes[`email_subject_${params.key}`] = subject === (tmpl?.subject_default || "") ? "" : subject
      }
      return Object.keys(changes).length ? admin.updateSettings(changes) : Promise.resolve(null)
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["admin", "email-templates"] })
      setEditing(null)
    },
    onError: (e: Error) => showToast(e.message, "error"),
  })

  // confirmReset holds the template key pending revert. Reset
  // discards the admin's custom HTML and falls back to the bundled
  // default — non-recoverable without an external backup. The
  // AlertDialog stops a misclick from wiping work.
  const [confirmReset, setConfirmReset] = useState<string | null>(null)
  const resetMut = useMutation({
    mutationFn: (key: string) => admin.updateSettings({ [`email_template_${key}`]: "", [`email_subject_${key}`]: "" }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["admin", "email-templates"] })
      setConfirmReset(null)
    },
    onError: (e: Error) => showToast(e.message, "error"),
  })

  const templates = data?.templates || {}
  const order = data?.order || []
  const variablesOf = (key: string) => templates[key]?.variables || []
  const groupOf = (key: string) => templates[key]?.group || ""

  const startEdit = (key: string) => {
    const tmpl = templates[key]
    setEditValue(tmpl?.custom || tmpl?.default || "")
    setEditSubject(tmpl?.subject_custom || tmpl?.subject_default || "")
    setEditing(key)
  }

  const getPreviewSubject = (key: string) => {
    const tmpl = templates[key]
    if (!tmpl) return ""
    return fillTemplate(tmpl.subject_custom || tmpl.subject_default || "")
  }

  const getPreviewHtml = (key: string) => {
    const tmpl = templates[key]
    if (!tmpl) return ""
    return fillTemplate(tmpl.custom || tmpl.default)
  }

  return (
    <div className="space-y-4">
      <Card>
        <CardHeader>
          <CardTitle className="text-base flex items-center gap-2">
            <Mail className="h-4 w-4" />
            {t("settings.emailTemplates")}
          </CardTitle>
        </CardHeader>
        <CardContent className="space-y-2">
          <p className="text-sm text-muted-foreground mb-4">{t("settings.templateDesc")}</p>

          {isLoading ? (
            <div className="h-32 bg-muted rounded-lg animate-pulse" />
          ) : (
            <div className="space-y-2">
              {order.map((key, i) => {
                const meta = TEMPLATE_META[key]
                const tmpl = templates[key]
                const isCustomized = !!tmpl?.custom || !!tmpl?.subject_custom
                const newGroup = i === 0 || groupOf(order[i - 1]) !== groupOf(key)

                return (
                  <div key={key}>
                    {newGroup && (
                      <h3 className="text-xs font-semibold uppercase tracking-wide text-muted-foreground pt-3 pb-1">
                        {groupOf(key) in GROUP_TITLE ? t(GROUP_TITLE[groupOf(key)]) : groupOf(key)}
                      </h3>
                    )}
                    <div className="flex items-center justify-between p-3 border rounded-lg">
                      <div className="flex items-center gap-3">
                        <Code className="h-4 w-4 text-muted-foreground" />
                        <div>
                          <span className="text-sm font-medium">{labelOf(key)}</span>
                          {isCustomized && (
                            <Badge variant="secondary" className="ml-2 text-xs">
                              customized
                            </Badge>
                          )}
                          <div className="text-xs text-muted-foreground mt-0.5">
                            {variablesOf(key)
                              .map((v) => `{{.${v}}}`)
                              .join(", ")}
                          </div>
                          {!!tmpl?.custom && meta?.fallbackNote && !tmpl.custom.includes(".FallbackPlan") && (
                            <div className="text-xs text-amber-700 dark:text-amber-400 mt-0.5">
                              {t(meta.fallbackNote)}
                            </div>
                          )}
                        </div>
                      </div>
                      <div className="flex items-center gap-1">
                        <Button
                          variant="ghost"
                          size="icon"
                          className="h-8 w-8"
                          onClick={() => setPreviewing(key)}
                          title={t("settings.templatePreview")}
                        >
                          <Eye className="h-4 w-4" />
                        </Button>
                        <Button
                          variant="ghost"
                          size="icon"
                          className="h-8 w-8"
                          onClick={() => startEdit(key)}
                          title={t("settings.editTemplate")}
                        >
                          <Pencil className="h-4 w-4" />
                        </Button>
                        {isCustomized && (
                          <Button
                            variant="ghost"
                            size="icon"
                            className="h-8 w-8 text-orange-600"
                            onClick={() => setConfirmReset(key)}
                            title={t("settings.resetTemplate")}
                            disabled={resetMut.isPending && resetMut.variables === key}
                          >
                            <RotateCcw className="h-4 w-4" />
                          </Button>
                        )}
                      </div>
                    </div>
                  </div>
                )
              })}
            </div>
          )}
        </CardContent>
      </Card>

      {/* Edit Dialog */}
      <Dialog open={!!editing} onOpenChange={(open) => !open && setEditing(null)}>
        <DialogContent className="max-w-3xl">
          <DialogHeader>
            <DialogTitle>
              {t("settings.editTemplate")}: {editing && labelOf(editing)}
            </DialogTitle>
          </DialogHeader>
          <DialogBody>
            <div className="space-y-3">
              <div className="text-xs text-muted-foreground">
                {t("settings.templateVariables")}:{" "}
                {editing &&
                  variablesOf(editing)
                    .map((v) => `{{.${v}}}`)
                    .join(", ")}
              </div>
              <label className="block text-xs font-medium" htmlFor="email-template-subject">
                {t("settings.templateSubject")}
              </label>
              <input
                id="email-template-subject"
                className="w-full font-mono text-xs p-2 border rounded-lg bg-muted/50 focus:outline-none focus:ring-2 focus:ring-primary"
                value={editSubject}
                maxLength={300}
                onChange={(e) => setEditSubject(e.target.value.replace(/[\r\n]+/g, " "))}
              />
              {editing && TEMPLATE_META[editing]?.fallbackNote && !editValue.includes(".FallbackPlan") && (
                <div className="text-xs text-amber-700 dark:text-amber-400">
                  {t(TEMPLATE_META[editing].fallbackNote as TranslationKeys)}
                </div>
              )}
              <textarea
                className="w-full h-64 font-mono text-xs p-3 border rounded-lg bg-muted/50 resize-y focus:outline-none focus:ring-2 focus:ring-primary"
                value={editValue}
                onChange={(e) => setEditValue(e.target.value)}
              />
              <div className="flex justify-end gap-2">
                <Button variant="outline" onClick={() => setEditing(null)}>
                  {t("common.cancel")}
                </Button>
                <Button
                  onClick={() => editing && saveMut.mutate({ key: editing, value: editValue, subject: editSubject })}
                  disabled={saveMut.isPending}
                >
                  {saveMut.isPending ? t("common.loading") : t("common.save")}
                </Button>
              </div>
            </div>
          </DialogBody>
        </DialogContent>
      </Dialog>

      {/* Preview Dialog — uses sandboxed iframe for safe HTML rendering */}
      <Dialog open={!!previewing} onOpenChange={(open) => !open && setPreviewing(null)}>
        <DialogContent className="max-w-2xl h-[min(760px,85vh)]">
          <DialogHeader>
            <DialogTitle>
              {t("settings.templatePreview")}: {previewing && labelOf(previewing)}
            </DialogTitle>
          </DialogHeader>
          <DialogBody>
            {previewing &&
              !(
                previewIsExact(templates[previewing]?.custom || templates[previewing]?.default || "") &&
                previewIsExact(templates[previewing]?.subject_custom || templates[previewing]?.subject_default || "")
              ) && (
                <p className="text-xs text-amber-700 dark:text-amber-400 mb-2">{t("settings.templatePreviewApprox")}</p>
              )}
            {previewing && (
              <p className="text-sm mb-2">
                <span className="text-muted-foreground">{t("settings.templateSubject")}:</span>{" "}
                {getPreviewSubject(previewing)}
              </p>
            )}
            {previewing && (
              <iframe
                title="Email Preview"
                sandbox=""
                srcDoc={getPreviewHtml(previewing)}
                className="w-full h-96 border rounded-lg bg-white"
              />
            )}
          </DialogBody>
        </DialogContent>
      </Dialog>

      <AlertDialog open={!!confirmReset} onOpenChange={() => setConfirmReset(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>
              {t("settings.resetTemplateTitle", {
                template: (confirmReset && labelOf(confirmReset)) || "",
              })}
            </AlertDialogTitle>
            <AlertDialogDescription>{t("settings.resetTemplateDesc")}</AlertDialogDescription>
          </AlertDialogHeader>
          <div className="flex justify-end gap-2">
            <AlertDialogCancel>{t("common.cancel")}</AlertDialogCancel>
            <AlertDialogAction
              className="bg-destructive text-white hover:bg-destructive/90"
              onClick={() => confirmReset && resetMut.mutate(confirmReset)}
            >
              {t("settings.resetTemplate")}
            </AlertDialogAction>
          </div>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  )
}
