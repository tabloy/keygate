import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { Code, Eye, Mail, Pencil, RotateCcw } from "lucide-react"
import { useState } from "react"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import { Dialog, DialogContent, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { useI18n } from "@/i18n"
import { admin } from "@/lib/api"

const TEMPLATE_META: Record<string, { label: string; variables: string[] }> = {
  license_created: { label: "License Created", variables: ["Product", "Plan", "LicenseKey"] },
  license_expiring: { label: "License Expiring", variables: ["Product", "LicenseKey", "ExpiresAt"] },
  license_expired: { label: "License Expired", variables: ["Product"] },
  trial_expired: { label: "Trial Expired", variables: ["Product"] },
  license_suspended: { label: "License Suspended", variables: ["Product", "Reason"] },
  quota_warning: { label: "Quota Warning", variables: ["Product", "Feature", "Used", "Limit", "Pct"] },
  seat_invite: { label: "Seat Invite", variables: ["Product", "Inviter"] },
  payment_failed: { label: "Payment Failed", variables: ["Product"] },
}

const PREVIEW_DATA: Record<string, string> = {
  Product: "AwesomeApp",
  Plan: "Pro",
  LicenseKey: "KG-XXXX-XXXX-XXXX-XXXX",
  ExpiresAt: "2026-06-15",
  Feature: "api_calls",
  Used: "800",
  Limit: "1000",
  Pct: "80",
  Inviter: "admin@company.com",
  Reason: "Policy violation",
}

function fillTemplate(html: string): string {
  let result = html
  for (const [k, v] of Object.entries(PREVIEW_DATA)) {
    result = result.replaceAll(`{{.${k}}}`, v)
  }
  // Remove Go template conditionals for preview
  result = result.replace(/\{\{if [^}]+\}\}/g, "").replace(/\{\{end\}\}/g, "")
  return result
}

export default function EmailTemplatesManager() {
  const { t } = useI18n()
  const qc = useQueryClient()
  const [editing, setEditing] = useState<string | null>(null)
  const [editValue, setEditValue] = useState("")
  const [previewing, setPreviewing] = useState<string | null>(null)

  const { data, isLoading } = useQuery({
    queryKey: ["admin", "email-templates"],
    queryFn: admin.getEmailTemplates,
  })

  const saveMut = useMutation({
    mutationFn: (params: { key: string; value: string }) =>
      admin.updateSettings({ [`email_template_${params.key}`]: params.value }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["admin", "email-templates"] })
      setEditing(null)
    },
  })

  const resetMut = useMutation({
    mutationFn: (key: string) => admin.updateSettings({ [`email_template_${key}`]: "" }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["admin", "email-templates"] }),
  })

  const templates = data?.templates || {}

  const startEdit = (key: string) => {
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

          {isLoading ? (
            <div className="h-32 bg-muted rounded-lg animate-pulse" />
          ) : (
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
                          onClick={() => resetMut.mutate(key)}
                          title={t("settings.resetTemplate")}
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
        <DialogContent className="max-w-3xl max-h-[85vh]">
          <DialogHeader>
            <DialogTitle>
              {t("settings.editTemplate")}: {editing && TEMPLATE_META[editing]?.label}
            </DialogTitle>
          </DialogHeader>
          <div className="space-y-3">
            <div className="text-xs text-muted-foreground">
              {t("settings.templateVariables")}:{" "}
              {editing && TEMPLATE_META[editing]?.variables.map((v) => `{{.${v}}}`).join(", ")}
            </div>
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
                disabled={saveMut.isPending}
              >
                {saveMut.isPending ? t("common.loading") : t("common.save")}
              </Button>
            </div>
          </div>
        </DialogContent>
      </Dialog>

      {/* Preview Dialog — uses sandboxed iframe for safe HTML rendering */}
      <Dialog open={!!previewing} onOpenChange={(open) => !open && setPreviewing(null)}>
        <DialogContent className="max-w-2xl max-h-[85vh]">
          <DialogHeader>
            <DialogTitle>
              {t("settings.templatePreview")}: {previewing && TEMPLATE_META[previewing]?.label}
            </DialogTitle>
          </DialogHeader>
          {previewing && (
            <iframe
              title="Email Preview"
              sandbox=""
              srcDoc={getPreviewHtml(previewing)}
              className="w-full h-96 border rounded-lg bg-white"
            />
          )}
        </DialogContent>
      </Dialog>
    </div>
  )
}
