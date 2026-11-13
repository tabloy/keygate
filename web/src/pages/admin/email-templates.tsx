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

// fallbackNote: trials that move, or will move, to their plan's fallback
// plan get this email too, so a customised copy should name it with
// {{.FallbackPlan}}; the key of the note that says so.
const TEMPLATE_META: Record<string, { label: string; variables: string[]; fallbackNote?: TranslationKeys }> = {
  // SiteName and PortalURL are available in every template (see
  // EmailService.templateData); they are listed last on each.
  license_created: {
    label: "License Created",
    variables: ["Product", "Plan", "LicenseKey", "DownloadURL", "SiteName", "PortalURL"],
  },
  license_expiring: {
    label: "License Expiring",
    variables: ["Product", "LicenseKey", "ExpiresAt", "DaysLeft", "IsTrial", "FallbackPlan", "SiteName", "PortalURL"],
    fallbackNote: "settings.templateFallbackNoteExpiring",
  },
  updates_ending: {
    label: "Updates Ending",
    variables: ["Product", "LicenseKey", "UpdatesUntil", "SiteName", "PortalURL"],
  },
  license_expired: { label: "License Expired", variables: ["Product", "SiteName", "PortalURL"] },
  trial_expired: {
    label: "Trial Expired",
    variables: ["Product", "FallbackPlan", "SiteName", "PortalURL"],
    fallbackNote: "settings.templateFallbackNoteExpired",
  },
  license_suspended: { label: "License Suspended", variables: ["Product", "Reason", "SiteName", "PortalURL"] },
  quota_warning: {
    label: "Quota Warning",
    variables: ["Product", "Feature", "Used", "Limit", "Pct", "SiteName", "PortalURL"],
  },
  seat_invite: {
    label: "Seat Invite",
    variables: ["Product", "Inviter", "InviteURL", "DownloadURL", "SiteName", "PortalURL"],
  },
  payment_failed: { label: "Payment Failed", variables: ["Product", "SiteName", "PortalURL"] },
}

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
  const [previewing, setPreviewing] = useState<string | null>(null)

  const { data, isLoading, isError, isFetching, refetch } = useQuery({
    queryKey: ["admin", "email-templates"],
    queryFn: admin.getEmailTemplates,
  })

  const readUnavailable = isError || !data
  const readError = isError && (
    <div role="alert" className="rounded-lg border border-destructive/30 p-3 space-y-2">
      <p className="text-sm text-destructive">{t("settings.templateReadFailed")}</p>
      <Button type="button" variant="outline" disabled={isFetching} onClick={() => void refetch()}>
        {t("common.retry")}
      </Button>
    </div>
  )

  const saveMut = useMutation({
    mutationFn: (params: { key: string; value: string }) => {
      if (readUnavailable) throw new Error(t("settings.templateReadFailed"))
      return admin.updateSettings({ [`email_template_${params.key}`]: params.value })
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
    mutationFn: (key: string) => {
      if (readUnavailable) throw new Error(t("settings.templateReadFailed"))
      return admin.updateSettings({ [`email_template_${key}`]: "" })
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["admin", "email-templates"] })
      setConfirmReset(null)
    },
    onError: (e: Error) => showToast(e.message, "error"),
  })

  const templates = data?.templates || {}

  const startEdit = (key: string) => {
    if (readUnavailable || !templates[key]) return
    const tmpl = templates[key]
    setEditValue(tmpl?.custom || tmpl?.default || "")
    setEditing(key)
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

          {readError}
          {isLoading ? (
            <div className="h-32 bg-muted rounded-lg animate-pulse" />
          ) : !data ? null : (
            <div className="space-y-2">
              {Object.entries(TEMPLATE_META).map(([key, meta]) => {
                const tmpl = templates[key]
                const isCustomized = !!tmpl?.custom

                return (
                  <div key={key} className="flex items-center justify-between p-3 border rounded-lg">
                    <div className="flex items-center gap-3">
                      <Code className="h-4 w-4 text-muted-foreground" />
                      <div>
                        <span className="text-sm font-medium">{meta.label}</span>
                        {isCustomized && (
                          <Badge variant="secondary" className="ml-2 text-xs">
                            customized
                          </Badge>
                        )}
                        <div className="text-xs text-muted-foreground mt-0.5">
                          {meta.variables.map((v) => `{{.${v}}}`).join(", ")}
                        </div>
                        {isCustomized && meta.fallbackNote && !tmpl?.custom?.includes(".FallbackPlan") && (
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
                        disabled={readUnavailable}
                      >
                        <Eye className="h-4 w-4" />
                      </Button>
                      <Button
                        variant="ghost"
                        size="icon"
                        className="h-8 w-8"
                        onClick={() => startEdit(key)}
                        title={t("settings.editTemplate")}
                        disabled={readUnavailable}
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
                          disabled={readUnavailable || (resetMut.isPending && resetMut.variables === key)}
                        >
                          <RotateCcw className="h-4 w-4" />
                        </Button>
                      )}
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
              {t("settings.editTemplate")}: {editing && TEMPLATE_META[editing]?.label}
            </DialogTitle>
          </DialogHeader>
          <DialogBody>
            {readError}
            <div className="space-y-3">
              <div className="text-xs text-muted-foreground">
                {t("settings.templateVariables")}:{" "}
                {editing && TEMPLATE_META[editing]?.variables.map((v) => `{{.${v}}}`).join(", ")}
              </div>
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
                  onClick={() => editing && saveMut.mutate({ key: editing, value: editValue })}
                  disabled={readUnavailable || saveMut.isPending}
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
              {t("settings.templatePreview")}: {previewing && TEMPLATE_META[previewing]?.label}
            </DialogTitle>
          </DialogHeader>
          <DialogBody>
            {previewing && !previewIsExact(templates[previewing]?.custom || templates[previewing]?.default || "") && (
              <p className="text-xs text-amber-700 dark:text-amber-400 mb-2">{t("settings.templatePreviewApprox")}</p>
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
                template: (confirmReset && TEMPLATE_META[confirmReset]?.label) || "",
              })}
            </AlertDialogTitle>
            <AlertDialogDescription>{t("settings.resetTemplateDesc")}</AlertDialogDescription>
          </AlertDialogHeader>
          {readError}
          <div className="flex justify-end gap-2">
            <AlertDialogCancel>{t("common.cancel")}</AlertDialogCancel>
            <AlertDialogAction
              disabled={readUnavailable || resetMut.isPending}
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
