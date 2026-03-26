import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { Check, Key, Mail, Pencil, Shield, User, X } from "lucide-react"
import { useState } from "react"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import { Input } from "@/components/ui/input"
import { Separator } from "@/components/ui/separator"
import { useAuth } from "@/hooks/use-auth"
import { useI18n } from "@/i18n"
import { portal } from "@/lib/api"
import { formatDate, statusColor } from "@/lib/utils"

export default function PortalAccountPage() {
  const { t } = useI18n()
  const { user, refetch } = useAuth()
  const qc = useQueryClient()
  const [editing, setEditing] = useState(false)
  const [name, setName] = useState("")

  const { data, isLoading } = useQuery({
    queryKey: ["portal", "licenses"],
    queryFn: portal.licenses,
  })

  const saveMut = useMutation({
    mutationFn: () => portal.updateProfile({ name: name.trim() }),
    onSuccess: () => {
      setEditing(false)
      refetch() // Refresh auth user context
      qc.invalidateQueries({ queryKey: ["portal"] })
    },
  })

  const licenses = data?.licenses || []
  const activeLicenses = licenses.filter((l) => l.status === "active" || l.status === "trialing")

  const startEdit = () => {
    setName(user?.name || "")
    setEditing(true)
  }

  const cancelEdit = () => {
    setEditing(false)
    setName("")
  }

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-bold tracking-tight">{t("portal.account")}</h1>
        <p className="text-muted-foreground">{t("portal.accountDesc")}</p>
      </div>

      {/* Profile card */}
      <Card>
        <CardHeader className="flex flex-row items-center justify-between">
          <CardTitle className="text-lg">{t("portal.profile")}</CardTitle>
          {!editing && (
            <Button variant="outline" size="sm" onClick={startEdit}>
              <Pencil className="h-3.5 w-3.5 mr-1.5" />
              {t("portal.editProfile")}
            </Button>
          )}
        </CardHeader>
        <CardContent className="space-y-4">
          <div className="flex items-center gap-4">
            {user?.avatar_url ? (
              <img src={user.avatar_url} alt={user.name} className="h-16 w-16 rounded-full border" />
            ) : (
              <div className="h-16 w-16 rounded-full bg-primary text-primary-foreground flex items-center justify-center text-xl font-bold">
                {user?.name?.charAt(0)?.toUpperCase() || user?.email?.charAt(0)?.toUpperCase()}
              </div>
            )}
            <div className="flex-1">
              {editing ? (
                <div className="flex items-center gap-2">
                  <div className="flex-1 space-y-2">
                    <label htmlFor="profile-name" className="text-xs text-muted-foreground">
                      {t("portal.displayName")}
                    </label>
                    <Input
                      id="profile-name"
                      value={name}
                      onChange={(e) => setName(e.target.value)}
                      maxLength={100}
                      placeholder={t("portal.displayName")}
                      autoFocus
                      onKeyDown={(e) => {
                        if (e.key === "Enter" && name.trim()) saveMut.mutate()
                        if (e.key === "Escape") cancelEdit()
                      }}
                    />
                  </div>
                  <div className="flex gap-1 mt-5">
                    <Button
                      size="icon"
                      variant="ghost"
                      className="h-8 w-8"
                      onClick={() => saveMut.mutate()}
                      disabled={!name.trim() || saveMut.isPending}
                    >
                      <Check className="h-4 w-4 text-emerald-600" />
                    </Button>
                    <Button size="icon" variant="ghost" className="h-8 w-8" onClick={cancelEdit}>
                      <X className="h-4 w-4" />
                    </Button>
                  </div>
                </div>
              ) : (
                <>
                  <p className="text-lg font-semibold">{user?.name || "-"}</p>
                  <div className="flex items-center gap-1.5 text-sm text-muted-foreground">
                    <Mail className="h-3.5 w-3.5" />
                    {user?.email}
                  </div>
                </>
              )}
              {saveMut.isSuccess && !editing && (
                <p className="text-xs text-emerald-600 mt-1">{t("portal.profileSaved")}</p>
              )}
            </div>
          </div>

          <Separator />

          <div className="grid grid-cols-1 sm:grid-cols-3 gap-4 text-sm">
            <div>
              <p className="text-muted-foreground">{t("portal.accountType")}</p>
              <div className="flex items-center gap-1.5 mt-0.5">
                {user?.is_admin ? (
                  <>
                    <Shield className="h-3.5 w-3.5 text-primary" />
                    <span className="font-medium">{t("portal.administrator")}</span>
                    <Badge variant="secondary" className="ml-1 text-xs">
                      {user.role}
                    </Badge>
                  </>
                ) : (
                  <>
                    <User className="h-3.5 w-3.5" />
                    <span className="font-medium">{t("portal.userRole")}</span>
                  </>
                )}
              </div>
            </div>
            <div>
              <p className="text-muted-foreground">{t("dashboard.activeLicenses")}</p>
              <p className="font-medium mt-0.5">{activeLicenses.length}</p>
            </div>
            <div>
              <p className="text-muted-foreground">{t("common.email")}</p>
              <p className="font-medium mt-0.5 text-muted-foreground text-xs">{user?.email}</p>
            </div>
          </div>
        </CardContent>
      </Card>

      {/* License overview */}
      <Card>
        <CardHeader>
          <CardTitle className="text-lg">{t("portal.licenseOverview")}</CardTitle>
        </CardHeader>
        <CardContent>
          {isLoading ? (
            <div className="space-y-3">
              {[1, 2].map((i) => (
                <div key={i} className="h-16 animate-pulse bg-muted rounded-lg" />
              ))}
            </div>
          ) : licenses.length === 0 ? (
            <div className="py-8 text-center">
              <Key className="h-10 w-10 mx-auto text-muted-foreground mb-3" />
              <p className="text-muted-foreground">{t("portal.noLicensesAccount")}</p>
            </div>
          ) : (
            <div className="space-y-2">
              {licenses.map((lic) => (
                <div key={lic.id} className="flex items-center justify-between bg-muted/50 rounded-lg px-4 py-3">
                  <div>
                    <p className="font-medium text-sm">{lic.product?.name || "License"}</p>
                    <p className="text-xs text-muted-foreground">
                      {lic.plan?.name} &middot; {t("licenses.validUntil")} {formatDate(lic.valid_until)}
                    </p>
                  </div>
                  <Badge className={statusColor(lic.status)}>{t(`status.${lic.status}` as any)}</Badge>
                </div>
              ))}
            </div>
          )}
        </CardContent>
      </Card>
    </div>
  )
}
