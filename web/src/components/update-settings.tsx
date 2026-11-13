import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { Check, Copy, Download, KeyRound, Plus, RotateCw, Trash2 } from "lucide-react"
import { Fragment, type ReactNode, useState } from "react"
import { Link } from "react-router-dom"
import { ProductSelect } from "@/components/product-select"
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
import { Dialog, DialogBody, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select"
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { useI18n } from "@/i18n"
import { admin, type Product, RELEASE_CHANNELS, RELEASE_PLATFORMS, type ReleaseSigningKey, site } from "@/lib/api"
import { copyText } from "@/lib/clipboard"
import { compareSemver } from "@/lib/semver"
import { formatDate } from "@/lib/utils"

// Releases belong to products that ship installable binaries. The
// server enforces it; the pickers ask for the same set so an admin never
// fills in a form the server will then refuse.
export const RELEASE_PRODUCT_TYPES = ["desktop", "hybrid"]

// rich fills {name} placeholders in a translated sentence with React
// nodes, so a sentence that embeds code or emphasis stays one string for
// translators instead of being cut into fragments.
export function rich(template: string, values: Record<string, ReactNode>): ReactNode[] {
  return template.split(/(\{\w+\})/).map((part, i) => {
    const name = /^\{(\w+)\}$/.exec(part)?.[1]
    // biome-ignore lint/suspicious/noArrayIndexKey: the pieces of one sentence never reorder
    return name && name in values ? <Fragment key={i}>{values[name]}</Fragment> : part
  })
}

// ─── Update settings ──────────────────────────────────────────────────────
//
// Everything about shipping one product's updates, in the order a
// developer meets it: sign releases, point the app at a feed, decide who
// may download, and finally require old versions to update. Each tab
// offers only choices the server will accept, and says why one is not
// available rather than letting a save fail.

export type SettingsTab = "signing" | "feeds" | "access" | "required"
export const SETTINGS_TABS: SettingsTab[] = ["signing", "feeds", "access", "required"]

export function UpdateSettingsDialog({
  initialProductId,
  initialTab,
  lockProduct,
  onClose,
}: {
  initialProductId: string
  initialTab: SettingsTab
  // Opened from one product (its row, a filtered list, a link): show
  // that product and no picker, so a setting cannot land on another.
  lockProduct?: boolean
  onClose: () => void
}) {
  const { t } = useI18n()
  const [productId, setProductId] = useState(initialProductId)
  const [tab, setTab] = useState<SettingsTab>(initialTab)
  const { data: product } = useQuery({
    queryKey: ["admin", "product", productId],
    queryFn: () => admin.getProduct(productId),
    enabled: !!productId,
  })

  return (
    <Dialog open onOpenChange={onClose}>
      {/* A fixed height: switching tabs, loading and expanding options
        must not resize the dialog under the pointer. Only the tab body
        scrolls, with its scrollbar space reserved so nothing shifts. */}
      <DialogContent className="max-w-2xl h-[min(760px,85vh)]">
        <DialogHeader>
          <DialogTitle>{t("updateSettings.title")}</DialogTitle>
          <DialogDescription>{t("updateSettings.desc")}</DialogDescription>
        </DialogHeader>
        {lockProduct ? (
          <div className="shrink-0 flex items-center gap-2 rounded-md border bg-muted/40 px-3 py-2">
            <span className="text-sm font-medium truncate">{product?.name ?? t("updateSettings.loading")}</span>
            {product && <code className="text-xs text-muted-foreground truncate">{product.slug}</code>}
          </div>
        ) : (
          <div className="shrink-0 space-y-2">
            <Label>{t("common.product")}</Label>
            <ProductSelect
              value={productId}
              onChange={setProductId}
              className="w-full"
              types={RELEASE_PRODUCT_TYPES}
              placeholder={t("updateSettings.selectProduct")}
            />
          </div>
        )}
        {!productId ? (
          <div className="flex flex-1 items-center justify-center text-sm text-muted-foreground">
            {t("updateSettings.selectProductHint")}
          </div>
        ) : !product || product.id !== productId ? (
          <div className="flex-1 animate-pulse rounded-md bg-muted" />
        ) : (
          <Tabs value={tab} onValueChange={(v) => setTab(v as SettingsTab)} className="flex min-h-0 flex-1 flex-col">
            <TabsList className="grid w-full shrink-0 grid-cols-2 sm:grid-cols-4 h-auto">
              <TabsTrigger value="signing">{t("updateSettings.tabSigning")}</TabsTrigger>
              <TabsTrigger value="feeds">{t("updateSettings.tabFeeds")}</TabsTrigger>
              <TabsTrigger value="access">{t("updateSettings.tabAccess")}</TabsTrigger>
              <TabsTrigger value="required">{t("updateSettings.tabRequired")}</TabsTrigger>
            </TabsList>
            <TabsContent value="signing" className={TAB_BODY}>
              <div className="space-y-4">
                <SigningKeysSection productId={product.id} />
                <RequireSigningField product={product} />
              </div>
            </TabsContent>
            <TabsContent value="feeds" className={TAB_BODY}>
              <FeedURLsSection product={product} onOpenAccess={() => setTab("access")} />
            </TabsContent>
            <TabsContent value="access" className={TAB_BODY}>
              <FeedAccessSection product={product} />
            </TabsContent>
            <TabsContent value="required" className={TAB_BODY}>
              <RequiredUpdateSection product={product} />
            </TabsContent>
          </Tabs>
        )}
      </DialogContent>
    </Dialog>
  )
}

const TAB_BODY = "-mx-6 mt-4 min-h-0 flex-1 overflow-y-auto px-6 pb-1 [scrollbar-gutter:stable]"

// useProductUpdate saves product fields from the settings tabs and
// refreshes every view of the product.
function useProductUpdate(product: Product, success: string) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (data: Partial<Product>) => admin.updateProduct(product.id, data),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["admin", "product", product.id] })
      qc.invalidateQueries({ queryKey: ["admin", "products"] })
      showToast(success, "success")
    },
    onError: (e: Error) => showToast(e.message, "error"),
  })
}

// RequireSigningField: what publishing does while the product has no
// active key. With a key, every release is signed either way.
function RequireSigningField({ product }: { product: Product }) {
  const { t } = useI18n()
  const [confirmOff, setConfirmOff] = useState(false)
  const mut = useProductUpdate(product, t("updateSettings.saved"))
  const save = (on: boolean) => mut.mutate({ require_signing: on }, { onSuccess: () => setConfirmOff(false) })
  return (
    <div className="space-y-1">
      <div className="flex items-center gap-3">
        <input
          type="checkbox"
          id="require-signing"
          checked={product.require_signing}
          disabled={mut.isPending}
          onChange={(e) => (e.target.checked ? save(true) : setConfirmOff(true))}
          className="h-4 w-4 rounded border-input accent-primary"
        />
        <Label htmlFor="require-signing">{t("updateSettings.requireSigning")}</Label>
      </div>
      <p className="text-xs text-muted-foreground">{t("updateSettings.requireSigningHint")}</p>
      <AlertDialog open={confirmOff} onOpenChange={setConfirmOff}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{t("updateSettings.unsignedTitle")}</AlertDialogTitle>
            <AlertDialogDescription>{t("updateSettings.unsignedDesc")}</AlertDialogDescription>
          </AlertDialogHeader>
          <div className="flex justify-end gap-2">
            <AlertDialogCancel>{t("common.cancel")}</AlertDialogCancel>
            <AlertDialogAction onClick={() => save(false)} disabled={mut.isPending}>
              {t("updateSettings.allowUnsigned")}
            </AlertDialogAction>
          </div>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  )
}

const MAC_PLATFORMS = RELEASE_PLATFORMS.filter((p) => p.startsWith("darwin-"))
const LOOPBACK_HOSTS = ["localhost", "127.0.0.1", "[::1]", "::1", "0.0.0.0"]

// A BASE_URL only this machine can reach, seen from a dashboard opened
// elsewhere: the server was left on its default and every URL shown
// would be useless in a shipped app.
function unreachableBaseURL(baseURL: string | undefined): boolean {
  if (!baseURL) return false
  try {
    return LOOPBACK_HOSTS.includes(new URL(baseURL).hostname) && !LOOPBACK_HOSTS.includes(window.location.hostname)
  } catch {
    return false
  }
}

// FeedURLsSection: the address each updater checks, ready to paste.
// The base is the server's configured public address, not the one the
// dashboard happens to be opened at.
function FeedURLsSection({ product, onOpenAccess }: { product: Product; onOpenAccess: () => void }) {
  const { t } = useI18n()
  const { data: config } = useQuery({ queryKey: ["site", "config"], queryFn: site.config, staleTime: 300_000 })
  const [channel, setChannel] = useState<string>("stable")
  const [macPlatform, setMacPlatform] = useState<string>("darwin-arm64")
  const [veloPlatform, setVeloPlatform] = useState<string>("windows-x64")
  const base = `${(config?.base_url || window.location.origin).replace(/\/+$/, "")}/api/v1/releases/${product.slug}`
  const channelQuery = channel === "stable" ? "" : `&channel=${channel}`

  return (
    <div className="space-y-5">
      {unreachableBaseURL(config?.base_url) && (
        <p className="rounded-md border border-amber-300 bg-amber-50 px-3 py-2 text-xs text-amber-800 dark:border-amber-700 dark:bg-amber-950 dark:text-amber-300">
          {rich(t("updateSettings.baseUrlWarning"), { url: <code>{config?.base_url}</code> })}
        </p>
      )}
      <div className="space-y-2">
        <Label>{t("updateSettings.channel")}</Label>
        <Select value={channel} onValueChange={setChannel}>
          <SelectTrigger className="w-full sm:w-48">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {RELEASE_CHANNELS.map((c) => (
              <SelectItem key={c} value={c}>
                {c}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        <p className="text-xs text-muted-foreground">{t("updateSettings.channelHint")}</p>
      </div>

      <div className="space-y-2">
        <PlatformPicker label="Sparkle" value={macPlatform} onChange={setMacPlatform} platforms={MAC_PLATFORMS} />
        <PublicKeyField
          label={t("updateSettings.sparkleField")}
          value={`${base}/feed.xml?platform=${macPlatform}${channelQuery}`}
          wrap
        >
          {rich(t("updateSettings.sparkleHint"), { field: <code>CFBundleVersion</code> })}
        </PublicKeyField>
      </div>

      <div className="space-y-2">
        <p className="text-sm font-medium">Tauri</p>
        <PublicKeyField
          label={t("updateSettings.tauriField")}
          value={`${base}/upgrade.json?platform={{target}}-{{arch}}${channelQuery}`}
          wrap
        >
          {rich(t("updateSettings.tauriHint"), {
            target: <code>{"{{target}}"}</code>,
            arch: <code>{"{{arch}}"}</code>,
          })}
        </PublicKeyField>
      </div>

      <div className="space-y-2">
        <PlatformPicker
          label="Velopack"
          value={veloPlatform}
          onChange={setVeloPlatform}
          platforms={[...RELEASE_PLATFORMS]}
        />
        <PublicKeyField label={t("updateSettings.veloField")} value={`${base}/velopack/${veloPlatform}`} wrap>
          {/* Velopack takes the channel from the app, not from the URL, so
            the address above is the same for every channel: say which
            pack command reaches the chosen one instead. */}
          {rich(t(channel === "stable" ? "updateSettings.veloHint" : "updateSettings.veloHintChannel"), {
            betaCmd: <code>vpk pack --channel beta</code>,
            channelCmd: <code>{`vpk pack --channel ${channel}`}</code>,
            channel,
            versionCmd: <code>vpk pack -v</code>,
          })}
        </PublicKeyField>
      </div>

      <div className="rounded-md border bg-muted/40 px-3 py-2 text-xs text-muted-foreground">
        {product.feed_license_required ? (
          rich(t("updateSettings.gatedNote"), {
            token: <code>license_token</code>,
            header: <code>X-License-Token</code>,
          })
        ) : (
          <>
            {t("updateSettings.publicNote")}{" "}
            <button type="button" className="underline" onClick={onOpenAccess}>
              {t("updateSettings.changeAccess")}
            </button>
          </>
        )}
      </div>
    </div>
  )
}

function PlatformPicker({
  label,
  value,
  onChange,
  platforms,
}: {
  label: string
  value: string
  onChange: (v: string) => void
  platforms: string[]
}) {
  return (
    <div className="flex items-center justify-between gap-3">
      <p className="text-sm font-medium">{label}</p>
      <Select value={value} onValueChange={onChange}>
        <SelectTrigger className="w-40 h-8 text-xs">
          <SelectValue />
        </SelectTrigger>
        <SelectContent>
          {platforms.map((p) => (
            <SelectItem key={p} value={p}>
              {p}
            </SelectItem>
          ))}
        </SelectContent>
      </Select>
    </div>
  )
}

// FeedAccessSection: public feeds, or feeds only licensed apps can read.
// The server refuses some switches (maintenance features off, plans that
// sell an update period); those choices are shown disabled with the
// reason instead of failing on save.
function FeedAccessSection({ product }: { product: Product }) {
  const { t } = useI18n()
  const [pending, setPending] = useState<boolean | null>(null)
  const [error, setError] = useState("")
  const mut = useProductUpdate(product, t("updateSettings.saved"))
  const gated = !!product.feed_license_required

  const { data: settingsData } = useQuery({ queryKey: ["admin", "settings"], queryFn: admin.getSettings })
  const { data: plansData } = useQuery({
    queryKey: ["admin", "plans", "for-access", product.id],
    queryFn: () => admin.listPlans({ product_id: product.id, limit: 100 }),
  })
  const { data: publishedData } = useQuery({
    queryKey: ["admin", "releases", "published-count", product.id],
    queryFn: () => admin.listReleases({ product_id: product.id, status: "published", limit: 1 }),
  })
  const maintenanceOn = settingsData?.settings?.maintenance_features_enabled === "true"
  const boundedPlans = (plansData?.plans || []).filter((p) => (p.updates_days ?? 0) > 0 || (p.renewal_days ?? 0) > 0)
  const published = publishedData?.total ?? 0

  const choose = (licensed: boolean) => {
    if (licensed === gated) return
    setError("")
    // Nothing has been published, so nobody holds a feed URL yet.
    if (licensed && published === 0) return save(true)
    setPending(licensed)
  }
  const save = (licensed: boolean) =>
    mut.mutate(
      { feed_license_required: licensed },
      {
        onSuccess: () => setPending(null),
        onError: (e: Error) => {
          setError(e.message)
          setPending(null)
        },
      },
    )

  const publicBlocked = gated && boundedPlans.length > 0
  const licensedBlocked = !gated && !maintenanceOn

  return (
    <div className="space-y-3">
      <AccessOption
        selected={!gated}
        disabled={publicBlocked || mut.isPending}
        onSelect={() => choose(false)}
        title={t("updateSettings.anyone")}
        description={t("updateSettings.anyoneDesc")}
      >
        {publicBlocked && (
          <p className="text-xs text-amber-700 dark:text-amber-400">
            {t(boundedPlans.length === 1 ? "updateSettings.publicBlockedOne" : "updateSettings.publicBlockedMany", {
              plans: boundedPlans.map((p) => p.name).join(", "),
            })}{" "}
            <Link to="/admin/plans" className="underline">
              {t("updateSettings.editPlans")}
            </Link>
          </p>
        )}
      </AccessOption>
      <AccessOption
        selected={gated}
        disabled={licensedBlocked || mut.isPending}
        onSelect={() => choose(true)}
        title={t("updateSettings.licensed")}
        description={t("updateSettings.licensedDesc")}
      >
        {licensedBlocked && (
          <p className="text-xs text-amber-700 dark:text-amber-400">
            {rich(t("updateSettings.licensedBlocked"), {
              settings: (
                <Link to="/admin/settings" className="underline">
                  {t("updateSettings.settingsLink")}
                </Link>
              ),
            })}
          </p>
        )}
        {gated && product.feed_gated_at && (
          <p className="text-xs text-muted-foreground">
            {t("updateSettings.gatedSince", { date: formatDate(product.feed_gated_at) })}
          </p>
        )}
        {!gated && !licensedBlocked && published === 0 && (
          <p className="text-xs text-muted-foreground">{t("updateSettings.nothingPublished")}</p>
        )}
      </AccessOption>
      {error && <p className="text-sm text-destructive">{error}</p>}

      <AlertDialog open={pending !== null} onOpenChange={(o) => !o && setPending(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>
              {pending ? t("updateSettings.confirmGateTitle") : t("updateSettings.confirmPublicTitle")}
            </AlertDialogTitle>
            <AlertDialogDescription>
              {pending ? t("updateSettings.confirmGateDesc") : t("updateSettings.confirmPublicDesc")}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <div className="flex justify-end gap-2">
            <AlertDialogCancel>{t("common.cancel")}</AlertDialogCancel>
            <AlertDialogAction onClick={() => pending !== null && save(pending)} disabled={mut.isPending}>
              {pending ? t("updateSettings.requireLicense") : t("updateSettings.makePublic")}
            </AlertDialogAction>
          </div>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  )
}

function AccessOption({
  selected,
  disabled,
  onSelect,
  title,
  description,
  children,
}: {
  selected: boolean
  disabled: boolean
  onSelect: () => void
  title: string
  description: string
  children?: React.ReactNode
}) {
  return (
    <label
      className={`flex gap-3 rounded-md border p-3 ${selected ? "border-primary bg-primary/5" : ""} ${
        disabled && !selected ? "opacity-70" : "cursor-pointer"
      }`}
    >
      <input
        type="radio"
        name="feed-access"
        checked={selected}
        disabled={disabled && !selected}
        onChange={onSelect}
        className="mt-1 h-4 w-4 accent-primary"
      />
      <div className="space-y-1">
        <p className="text-sm font-medium">{title}</p>
        <p className="text-xs text-muted-foreground">{description}</p>
        {children}
      </div>
    </label>
  )
}

// RequiredUpdateSection: versions older than the chosen release must
// update. The version is picked from published stable releases, so it
// can never name a release that apps cannot get.
function RequiredUpdateSection({ product }: { product: Product }) {
  const { t } = useI18n()
  const mut = useProductUpdate(product, t("updateSettings.saved"))
  const { data, isLoading } = useQuery({
    queryKey: ["admin", "releases", "stable-published", product.id],
    queryFn: () => admin.listReleases({ product_id: product.id, status: "published", channel: "stable", limit: 100 }),
  })
  // Only asked when there is nothing to pick from, to say what to do.
  const { data: draftData } = useQuery({
    queryKey: ["admin", "releases", "draft-count", product.id],
    queryFn: () => admin.listReleases({ product_id: product.id, status: "draft", limit: 1 }),
    enabled: !isLoading && (data?.releases || []).length === 0,
  })
  const versions = [...new Set((data?.releases || []).map((r) => r.version))].sort((a, b) => compareSemver(b, a))
  const newest = versions[0] ?? ""
  const saved = product.minimum_supported_version ?? ""
  const savedMessage = product.minimum_supported_message ?? ""

  const [enabled, setEnabled] = useState(saved !== "")
  const [version, setVersion] = useState(saved)
  const [message, setMessage] = useState(savedMessage)
  const chosen = version || newest
  const dirty = enabled ? saved !== chosen || savedMessage !== message.trim() : saved !== ""
  const unpublished = enabled && chosen !== "" && !versions.includes(chosen)

  if (isLoading) return <div className="h-32 animate-pulse bg-muted rounded-md" />

  // Nothing to pick from and nothing set: say what is missing instead of
  // showing a switch that cannot be turned on.
  if (versions.length === 0 && saved === "") {
    const drafts = draftData?.total ?? 0
    return (
      <div className="rounded-md border bg-muted/40 p-4 space-y-1">
        <p className="text-sm font-medium">{t("updateSettings.noStableTitle")}</p>
        <p className="text-sm text-muted-foreground">
          {t("updateSettings.noStableDesc")}
          {drafts > 0 &&
            ` ${t(drafts === 1 ? "updateSettings.draftsWaitingOne" : "updateSettings.draftsWaitingMany", { count: drafts })}`}
        </p>
      </div>
    )
  }

  const save = () =>
    mut.mutate({
      minimum_supported_version: enabled ? chosen : "",
      minimum_supported_message: enabled ? message.trim() : "",
    })
  const reset = () => {
    setEnabled(saved !== "")
    setVersion(saved)
    setMessage(savedMessage)
  }

  return (
    <div className="space-y-4">
      <div className="space-y-1">
        <div className="flex items-center gap-3">
          <input
            type="checkbox"
            id="require-update"
            checked={enabled}
            onChange={(e) => setEnabled(e.target.checked)}
            className="h-4 w-4 rounded border-input accent-primary"
          />
          <Label htmlFor="require-update">{t("updateSettings.requireUpdate")}</Label>
        </div>
        <p className="text-xs text-muted-foreground">{t("updateSettings.requireUpdateHint")}</p>
      </div>

      {enabled && (
        <>
          <div className="space-y-2">
            <Label>{t("updateSettings.minimumVersion")}</Label>
            <Select value={chosen} onValueChange={setVersion}>
              <SelectTrigger className="w-full sm:w-56">
                <SelectValue placeholder={t("updateSettings.selectVersion")} />
              </SelectTrigger>
              <SelectContent>
                {unpublished && (
                  <SelectItem value={chosen}>{t("updateSettings.versionNotPublished", { version: chosen })}</SelectItem>
                )}
                {versions.map((v) => (
                  <SelectItem key={v} value={v}>
                    {v === newest ? t("updateSettings.versionNewest", { version: v }) : v}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            <p className="text-xs text-muted-foreground">{t("updateSettings.olderMustUpdate")}</p>
            {unpublished && (
              <p className="text-xs text-amber-700 dark:text-amber-400">
                {t("updateSettings.unpublishedWarn", { version: chosen })}{" "}
                {newest ? t("updateSettings.unpublishedCapped", { newest }) : t("updateSettings.unpublishedIgnored")}
              </p>
            )}
          </div>
          <div className="space-y-2">
            <Label htmlFor="require-update-message">{t("updateSettings.messageLabel")}</Label>
            <Input
              id="require-update-message"
              maxLength={1024}
              placeholder={t("updateSettings.messagePlaceholder")}
              value={message}
              onChange={(e) => setMessage(e.target.value)}
            />
          </div>
          {chosen && !unpublished && (
            <div className="rounded-md border bg-muted/40 px-3 py-2 text-xs space-y-1">
              <p className="font-medium">
                {newest && newest !== chosen
                  ? t("updateSettings.previewTitleTo", { version: chosen, newest })
                  : t("updateSettings.previewTitle", { version: chosen })}
              </p>
              <ul className="list-disc pl-4 text-muted-foreground space-y-0.5">
                <li>{t("updateSettings.previewSparkle")}</li>
                <li>
                  {rich(t(message.trim() ? "updateSettings.previewTauriMessage" : "updateSettings.previewTauri"), {
                    rawJson: <code>update.rawJson</code>,
                  })}
                </li>
                <li>{t("updateSettings.previewVelopack")}</li>
              </ul>
            </div>
          )}
        </>
      )}

      <div className="flex justify-end gap-2">
        <Button variant="outline" size="sm" onClick={reset} disabled={!dirty || mut.isPending}>
          {t("updateSettings.reset")}
        </Button>
        <Button size="sm" onClick={save} disabled={!dirty || mut.isPending || (enabled && !chosen)}>
          {mut.isPending ? t("updateSettings.saving") : t("common.save")}
        </Button>
      </div>
    </div>
  )
}

function SigningKeysSection({ productId }: { productId: string }) {
  const { t } = useI18n()
  const qc = useQueryClient()
  const [rotateOpen, setRotateOpen] = useState(false)
  const [deactivateOpen, setDeactivateOpen] = useState(false)

  const { data, isLoading } = useQuery({
    queryKey: ["admin", "signing-keys", productId],
    queryFn: () => admin.listSigningKeys(productId),
  })
  const keys = data?.keys || []
  const active = keys.find((k) => k.active)
  const history = keys.filter((k) => !k.active)

  const generateMut = useMutation({
    mutationFn: () => admin.generateSigningKey(productId),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["admin", "signing-keys", productId] })
      showToast(t("updateSettings.toastKeyGenerated"), "success")
    },
    onError: (e: Error) => showToast(e.message, "error"),
  })

  if (isLoading) return <div className="h-32 animate-pulse bg-muted rounded-md" />

  return (
    <div className="space-y-4">
      {!active ? (
        <Card>
          <CardContent className="py-8 text-center">
            <KeyRound className="h-10 w-10 mx-auto text-muted-foreground mb-3" />
            <p className="font-medium">{t("updateSettings.noKeyTitle")}</p>
            <p className="text-sm text-muted-foreground mb-4">{t("updateSettings.noKeyDesc")}</p>
            <Button onClick={() => generateMut.mutate()} disabled={generateMut.isPending}>
              <Plus className="h-4 w-4 mr-2" />
              {generateMut.isPending ? t("updateSettings.generating") : t("updateSettings.generateKey")}
            </Button>
          </CardContent>
        </Card>
      ) : (
        <ActiveSigningKeyCard
          keyRow={active}
          productId={productId}
          onRotate={() => setRotateOpen(true)}
          onDeactivate={() => setDeactivateOpen(true)}
        />
      )}

      {history.length > 0 && (
        <div>
          <p className="text-sm font-medium mb-2">{t("updateSettings.pastKeys", { count: history.length })}</p>
          <div className="space-y-2">
            {history.map((k) => (
              <div key={k.id} className="bg-muted/50 rounded-md px-3 py-2 text-xs">
                <div className="flex items-center justify-between">
                  <code className="truncate flex-1 mr-2">{k.public_key}</code>
                  <span className="text-muted-foreground shrink-0">
                    {t("updateSettings.rotatedOn", { date: k.rotated_at ? formatDate(k.rotated_at) : "—" })}
                  </span>
                </div>
                {k.note && <p className="text-muted-foreground mt-1">{k.note}</p>}
              </div>
            ))}
          </div>
        </div>
      )}

      {rotateOpen && active && <RotateKeyDialog productId={productId} onClose={() => setRotateOpen(false)} />}
      {deactivateOpen && active && (
        <DeactivateKeyDialog productId={productId} onClose={() => setDeactivateOpen(false)} />
      )}
    </div>
  )
}

// One public key with a copy button. Sparkle and Tauri need the same
// key in different encodings, so the card shows each one ready to paste.
function PublicKeyField({
  label,
  value,
  wrap,
  children,
}: {
  label: string
  value?: string
  // Wrap instead of truncating: a URL's tail (its platform) matters.
  wrap?: boolean
  children: React.ReactNode
}) {
  const { t } = useI18n()
  const [copied, setCopied] = useState(false)
  const copy = async () => {
    setCopied(false)
    if (!value) return
    if (!(await copyText(value, t("common.copyTextFailed")))) return
    setCopied(true)
    setTimeout(() => setCopied(false), 1500)
  }
  return (
    <div>
      <Label className="text-xs">{label}</Label>
      <div className="flex items-center gap-2 mt-1 bg-muted rounded-md px-3 py-2">
        <code className={`text-xs flex-1 font-mono ${wrap ? "break-all" : "truncate"}`}>
          {value ?? t("updateSettings.loading")}
        </code>
        <Button
          variant="ghost"
          size="icon"
          className="h-7 w-7 shrink-0"
          onClick={copy}
          disabled={!value}
          title={t("updateSettings.copy")}
          aria-label={t("updateSettings.copy")}
        >
          {copied ? <Check className="h-3 w-3 text-emerald-600" /> : <Copy className="h-3 w-3" />}
        </Button>
      </div>
      <p className="text-xs text-muted-foreground mt-1">{children}</p>
    </div>
  )
}

function ActiveSigningKeyCard({
  keyRow,
  productId,
  onRotate,
  onDeactivate,
}: {
  keyRow: ReleaseSigningKey
  productId: string
  onRotate: () => void
  onDeactivate: () => void
}) {
  const { t } = useI18n()
  const tauriKey = useQuery({
    queryKey: ["admin", "signing-keys", productId, "tauri", keyRow.id],
    queryFn: () => admin.tauriPublicKey(productId),
  })

  return (
    <Card>
      <CardContent className="py-4 space-y-3">
        <div className="flex items-center justify-between">
          <div>
            <p className="font-medium text-sm">{t("updateSettings.activeKey")}</p>
            <p className="text-xs text-muted-foreground">
              {t("updateSettings.createdOn", { date: formatDate(keyRow.created_at) })}
            </p>
          </div>
          <Badge className="bg-emerald-100 text-emerald-800">{t("updateSettings.activeBadge")}</Badge>
        </div>
        <PublicKeyField label={t("updateSettings.sparkleKey")} value={keyRow.public_key}>
          {rich(t("updateSettings.sparkleKeyHint"), { field: <code>SUPublicEDKey</code> })}
        </PublicKeyField>
        <PublicKeyField label={t("updateSettings.tauriKey")} value={tauriKey.data?.pubkey}>
          {rich(t("updateSettings.tauriKeyHint"), { field: <code>pubkey</code> })}
        </PublicKeyField>
        <div className="flex flex-wrap gap-2">
          <Button variant="outline" size="sm" asChild>
            <a href={admin.publicKeyURL(productId)} download="public_key.pem">
              <Download className="h-3.5 w-3.5 mr-1.5" />
              {t("updateSettings.downloadPem")}
            </a>
          </Button>
          <Button variant="outline" size="sm" onClick={onRotate}>
            <RotateCw className="h-3.5 w-3.5 mr-1.5" />
            {t("updateSettings.rotate")}
          </Button>
          <Button variant="outline" size="sm" className="text-destructive" onClick={onDeactivate}>
            <Trash2 className="h-3.5 w-3.5 mr-1.5" />
            {t("updateSettings.deactivate")}
          </Button>
        </div>
      </CardContent>
    </Card>
  )
}

function RotateKeyDialog({ productId, onClose }: { productId: string; onClose: () => void }) {
  const { t } = useI18n()
  const qc = useQueryClient()
  const [note, setNote] = useState("")
  const mut = useMutation({
    mutationFn: (n: string) => admin.rotateSigningKey(productId, n),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["admin", "signing-keys", productId] })
      showToast(t("updateSettings.toastKeyRotated"), "success")
      onClose()
    },
    onError: (e: Error) => showToast(e.message, "error"),
  })

  return (
    <Dialog open onOpenChange={onClose}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>{t("updateSettings.rotateTitle")}</DialogTitle>
          <DialogDescription>{t("updateSettings.rotateDesc")}</DialogDescription>
        </DialogHeader>
        <DialogBody>
          <div className="space-y-2 py-2">
            <Label>{t("updateSettings.auditReason")}</Label>
            <textarea
              rows={3}
              placeholder={t("updateSettings.rotatePlaceholder")}
              value={note}
              onChange={(e) => setNote(e.target.value)}
              className="flex min-h-[60px] w-full rounded-md border border-input bg-background px-3 py-2 text-sm"
            />
          </div>
          <div className="flex justify-end gap-2 pt-2">
            <Button variant="outline" onClick={onClose}>
              {t("common.cancel")}
            </Button>
            <Button onClick={() => mut.mutate(note)} disabled={mut.isPending}>
              {mut.isPending ? t("updateSettings.rotating") : t("updateSettings.rotate")}
            </Button>
          </div>
        </DialogBody>
      </DialogContent>
    </Dialog>
  )
}

function DeactivateKeyDialog({ productId, onClose }: { productId: string; onClose: () => void }) {
  const { t } = useI18n()
  const qc = useQueryClient()
  const [note, setNote] = useState("")
  const mut = useMutation({
    mutationFn: (n: string) => admin.deactivateSigningKey(productId, n),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["admin", "signing-keys", productId] })
      showToast(t("updateSettings.toastKeyDeactivated"), "success")
      onClose()
    },
    onError: (e: Error) => showToast(e.message, "error"),
  })

  return (
    <Dialog open onOpenChange={onClose}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>{t("updateSettings.deactivateTitle")}</DialogTitle>
          <DialogDescription>{t("updateSettings.deactivateDesc")}</DialogDescription>
        </DialogHeader>
        <DialogBody>
          <div className="space-y-2 py-2">
            <Label>{t("updateSettings.auditReason")}</Label>
            <textarea
              rows={3}
              placeholder={t("updateSettings.deactivatePlaceholder")}
              value={note}
              onChange={(e) => setNote(e.target.value)}
              className="flex min-h-[60px] w-full rounded-md border border-input bg-background px-3 py-2 text-sm"
            />
          </div>
          <div className="flex justify-end gap-2 pt-2">
            <Button variant="outline" onClick={onClose}>
              {t("common.cancel")}
            </Button>
            <Button variant="destructive" onClick={() => mut.mutate(note)} disabled={mut.isPending}>
              {mut.isPending ? t("updateSettings.deactivating") : t("updateSettings.deactivate")}
            </Button>
          </div>
        </DialogBody>
      </DialogContent>
    </Dialog>
  )
}
