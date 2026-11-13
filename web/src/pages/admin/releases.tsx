import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import {
  AlertTriangle,
  ChevronRight,
  MoreVertical,
  Package,
  Plus,
  Rocket,
  Settings2,
  Trash2,
  Upload,
  X,
} from "lucide-react"
import { type ChangeEvent, useEffect, useRef, useState } from "react"
import { Link, useSearchParams } from "react-router-dom"
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
import { Dialog, DialogBody, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select"
import {
  RELEASE_PRODUCT_TYPES,
  rich,
  SETTINGS_TABS,
  type SettingsTab,
  UpdateSettingsDialog,
} from "@/components/update-settings"
import { useDateFormatter } from "@/hooks/use-site-config"
import { useI18n } from "@/i18n"
import { admin, RELEASE_CHANNELS, RELEASE_PLATFORMS, type Release, type ReleaseArtifact } from "@/lib/api"
import { compareSemver } from "@/lib/semver"

const PAGE_SIZE = 20

export default function ReleasesPage() {
  const formatDate = useDateFormatter()
  const { t } = useI18n()
  const qc = useQueryClient()
  const [productFilter, setProductFilter] = useState("")
  const [channelFilter, setChannelFilter] = useState("")
  const [statusFilter, setStatusFilter] = useState("")
  const [page, setPage] = useState(0)
  const [creating, setCreating] = useState(false)
  const [yanking, setYanking] = useState<Release | null>(null)
  const [unyanking, setUnyanking] = useState<Release | null>(null)
  const [deleting, setDeleting] = useState<Release | null>(null)
  // ?settings=<product id>&tab=access opens Update settings, so other
  // pages (a plan's update period, for one) can link straight to it.
  const [searchParams, setSearchParams] = useSearchParams()
  const settingsParam = searchParams.get("settings")
  const tabParam = searchParams.get("tab") as SettingsTab | null
  const [settings, setSettings] = useState<{ productId: string; tab: SettingsTab } | null>(
    settingsParam !== null
      ? { productId: settingsParam, tab: tabParam && SETTINGS_TABS.includes(tabParam) ? tabParam : "signing" }
      : null,
  )
  const closeSettings = () => {
    setSettings(null)
    if (settingsParam !== null) setSearchParams({}, { replace: true })
  }
  const [openRelease, setOpenRelease] = useState<Release | null>(null)
  const [confirmPublish, setConfirmPublish] = useState<{ rel: Release; latest: string } | null>(null)

  // Only desktop and hybrid products can own releases, so that is
  // what this page asks for — the question "does this install have a
  // product that can publish?" is the server's to answer, not one to
  // infer from whichever page of the catalogue happened to load. The
  // empty state below turns on the counts, so both are needed.
  const { data: releasableData } = useQuery({
    queryKey: ["admin", "products", "releasable-count"],
    queryFn: () => admin.listProducts({ type: RELEASE_PRODUCT_TYPES.join(","), limit: 1 }),
  })
  const { data: anyProductsData } = useQuery({
    queryKey: ["admin", "products", "any-count"],
    queryFn: () => admin.listProducts({ limit: 1 }),
  })
  const releasableCount = releasableData?.total ?? 0
  const anyProductCount = anyProductsData?.total ?? 0

  const { data, isLoading } = useQuery({
    queryKey: ["admin", "releases", productFilter, channelFilter, statusFilter, page],
    queryFn: () =>
      admin.listReleases({
        product_id: productFilter || undefined,
        channel: channelFilter || undefined,
        status: statusFilter || undefined,
        limit: PAGE_SIZE,
        offset: page * PAGE_SIZE,
      }),
  })
  const releases = data?.releases || []
  const total = data?.total || 0
  const totalPages = Math.max(1, Math.ceil(total / PAGE_SIZE))
  // From the server's list of what is published, not this page: a draft
  // is warned against the newest release whatever filter or page shows it.
  const latestByBucket = computeLatestVersions(data?.latest_published || [])

  const publishMut = useMutation({
    mutationFn: admin.publishRelease,
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["admin", "releases"] })
      showToast(t("releases.toastPublished"), "success")
    },
    onError: (e: Error) => showToast(e.message, "error"),
  })
  const unyankMut = useMutation({
    mutationFn: admin.unyankRelease,
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["admin", "releases"] })
      showToast(t("releases.toastUnyanked"), "success")
      setUnyanking(null)
    },
    onError: (e: Error) => showToast(e.message, "error"),
  })
  const deleteMut = useMutation({
    mutationFn: admin.deleteRelease,
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["admin", "releases"] })
      setDeleting(null)
      showToast(t("releases.toastDraftDeleted"), "success")
    },
    onError: (e: Error) => showToast(e.message, "error"),
  })

  // No release-eligible products. Either no products at all, or the
  // admin has only saas products (which don't ship installable binaries).
  if (releasableCount === 0 && releasableData && !isLoading) {
    const hasAnyProducts = anyProductCount > 0
    return (
      <div className="space-y-6">
        <div>
          <h1 className="text-2xl font-bold tracking-tight sr-only md:not-sr-only">{t("nav.releases")}</h1>
          <p className="text-muted-foreground">{t("releases.subtitle")}</p>
        </div>
        <Card>
          <CardContent className="py-12 text-center">
            <Package className="h-12 w-12 mx-auto text-muted-foreground mb-4" />
            {hasAnyProducts ? (
              <>
                <p className="text-lg font-medium">{t("releases.noEligibleTitle")}</p>
                <p className="text-muted-foreground mt-1 mb-4">{t("releases.noEligibleDesc")}</p>
              </>
            ) : (
              <>
                <p className="text-lg font-medium">{t("releases.noProductsTitle")}</p>
                <p className="text-muted-foreground mt-1 mb-4">{t("releases.noProductsDesc")}</p>
              </>
            )}
            <Button asChild>
              <Link to="/admin/products">
                <Plus className="h-4 w-4 mr-2" />{" "}
                {hasAnyProducts ? t("releases.manageProducts") : t("releases.createProduct")}
              </Link>
            </Button>
          </CardContent>
        </Card>
      </div>
    )
  }

  return (
    <div className="space-y-6">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div className="min-w-0">
          <h1 className="text-2xl font-bold tracking-tight sr-only md:not-sr-only">{t("nav.releases")}</h1>
          <p className="text-muted-foreground">{t("releases.subtitle")}</p>
        </div>
        <div className="flex gap-2">
          <Button variant="outline" onClick={() => setSettings({ productId: productFilter, tab: "signing" })}>
            <Settings2 className="h-4 w-4 mr-2" /> {t("releases.updateSettings")}
          </Button>
          <Button onClick={() => setCreating(true)}>
            <Plus className="h-4 w-4 mr-2" /> {t("releases.newRelease")}
          </Button>
        </div>
      </div>

      <div className="flex flex-wrap gap-3">
        <ProductSelect
          value={productFilter}
          onChange={setProductFilter}
          allLabel={t("releases.allProducts")}
          types={RELEASE_PRODUCT_TYPES}
        />
        <Select value={channelFilter || "all"} onValueChange={(v) => setChannelFilter(v === "all" ? "" : v)}>
          <SelectTrigger className="w-36">
            <SelectValue placeholder={t("releases.allChannels")} />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="all">{t("releases.allChannels")}</SelectItem>
            {RELEASE_CHANNELS.map((c) => (
              <SelectItem key={c} value={c}>
                {c}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        <Select value={statusFilter || "all"} onValueChange={(v) => setStatusFilter(v === "all" ? "" : v)}>
          <SelectTrigger className="w-36">
            <SelectValue placeholder={t("releases.allStatuses")} />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="all">{t("releases.allStatuses")}</SelectItem>
            <SelectItem value="draft">{t("releases.statusDraft")}</SelectItem>
            <SelectItem value="published">{t("releases.statusPublished")}</SelectItem>
            <SelectItem value="yanked">{t("releases.statusYanked")}</SelectItem>
          </SelectContent>
        </Select>
      </div>

      <DataTable>
        <DataTableHeader>
          <DataTableRow>
            <DataTableHead>{t("common.product")}</DataTableHead>
            <DataTableHead>{t("releases.version")}</DataTableHead>
            <DataTableHead>{t("releases.channel")}</DataTableHead>
            <DataTableHead>{t("releases.platforms")}</DataTableHead>
            <DataTableHead>{t("common.status")}</DataTableHead>
            <DataTableHead>{t("common.created")}</DataTableHead>
            <DataTableHead className="text-right">{t("common.actions")}</DataTableHead>
          </DataTableRow>
        </DataTableHeader>
        <DataTableBody>
          {isLoading ? (
            <DataTableEmpty colSpan={7} message={t("common.loading")} />
          ) : releases.length === 0 ? (
            <DataTableEmpty colSpan={7} message={t("releases.empty")} />
          ) : (
            releases.map((rel) => {
              const bucketKey = `${rel.product_id}|${rel.channel}`
              const latestInBucket = latestByBucket.get(bucketKey)
              const isBelowLatest =
                latestInBucket !== undefined &&
                rel.version !== latestInBucket &&
                compareSemver(rel.version, latestInBucket) < 0
              const artifacts = rel.artifacts || []
              const allReady = artifacts.length > 0 && artifacts.every((a) => a.sha256 && a.file_key)

              return (
                <DataTableRow key={rel.id}>
                  <DataTableCell>{rel.product?.name || rel.product_id}</DataTableCell>
                  <DataTableCell className="font-mono text-sm">
                    <button
                      type="button"
                      className="hover:underline"
                      onClick={() => setOpenRelease(rel)}
                      title={t("releases.openDetail")}
                    >
                      {rel.version}
                    </button>
                    {isBelowLatest && (
                      <Badge
                        variant="outline"
                        className="ml-1.5 text-[10px] py-0 px-1.5 border-amber-500 text-amber-700"
                        title={t("releases.belowLatestTitle", { latest: latestInBucket ?? "" })}
                      >
                        {t("releases.belowLatest")}
                      </Badge>
                    )}
                  </DataTableCell>
                  <DataTableCell>
                    <Badge variant="outline" className="capitalize">
                      {rel.channel}
                    </Badge>
                  </DataTableCell>
                  <DataTableCell className="text-sm">
                    {artifacts.length === 0 ? (
                      <span className="text-muted-foreground">—</span>
                    ) : (
                      <span className="font-mono text-xs">
                        {t(artifacts.length === 1 ? "releases.platformCountOne" : "releases.platformCountMany", {
                          count: artifacts.length,
                        })}
                      </span>
                    )}
                  </DataTableCell>
                  <DataTableCell>
                    <StatusBadge status={rel.status} yankedReason={rel.yanked_reason} />
                  </DataTableCell>
                  <DataTableCell className="text-sm text-muted-foreground">{formatDate(rel.created_at)}</DataTableCell>
                  <DataTableCell className="text-right">
                    <DropdownMenu>
                      {/* A row's actions depend on its status (publish a draft,
                        yank a published release, unyank a yanked one), so
                        they live behind one overflow icon under the Actions
                        column instead of a second "Actions" label. */}
                      <DropdownMenuTrigger asChild>
                        <Button
                          variant="ghost"
                          size="icon"
                          title={t("common.actions")}
                          aria-label={t("common.actions")}
                        >
                          <MoreVertical className="h-4 w-4" />
                        </Button>
                      </DropdownMenuTrigger>
                      <DropdownMenuContent align="end">
                        <DropdownMenuItem onClick={() => setOpenRelease(rel)}>
                          <ChevronRight className="h-3.5 w-3.5 mr-2" /> {t("releases.viewArtifacts")}
                        </DropdownMenuItem>
                        <DropdownMenuSeparator />
                        {rel.status === "draft" && allReady && (
                          <DropdownMenuItem
                            onClick={() => {
                              if (isBelowLatest && latestInBucket) {
                                setConfirmPublish({ rel, latest: latestInBucket })
                              } else {
                                publishMut.mutate(rel.id)
                              }
                            }}
                          >
                            <Rocket className="h-3.5 w-3.5 mr-2" /> {t("releases.publish")}
                          </DropdownMenuItem>
                        )}
                        {rel.status === "draft" && !allReady && (
                          <DropdownMenuItem disabled>
                            {t("releases.awaitingArtifacts", {
                              ready: artifacts.filter((a) => a.sha256).length,
                              total: artifacts.length,
                            })}
                          </DropdownMenuItem>
                        )}
                        {rel.status === "published" && (
                          <DropdownMenuItem onClick={() => setYanking(rel)} className="text-destructive">
                            <AlertTriangle className="h-3.5 w-3.5 mr-2" /> {t("releases.yank")}
                          </DropdownMenuItem>
                        )}
                        {rel.status === "yanked" && (
                          <DropdownMenuItem onClick={() => setUnyanking(rel)}>{t("releases.unyank")}</DropdownMenuItem>
                        )}
                        <DropdownMenuSeparator />
                        {rel.status === "draft" ? (
                          <DropdownMenuItem className="text-destructive" onClick={() => setDeleting(rel)}>
                            <Trash2 className="h-3.5 w-3.5 mr-2" /> {t("releases.deleteDraft")}
                          </DropdownMenuItem>
                        ) : (
                          <DropdownMenuItem disabled>
                            <Trash2 className="h-3.5 w-3.5 mr-2" /> {t("releases.deleteYankInstead")}
                          </DropdownMenuItem>
                        )}
                      </DropdownMenuContent>
                    </DropdownMenu>
                  </DataTableCell>
                </DataTableRow>
              )
            })
          )}
        </DataTableBody>
      </DataTable>

      <DataTablePagination
        page={page}
        totalPages={totalPages}
        total={total}
        pageSize={PAGE_SIZE}
        onPageChange={setPage}
      />

      {creating && (
        <CreateReleaseDialog
          onClose={() => setCreating(false)}
          onCreated={(r) => {
            setCreating(false)
            setOpenRelease(r)
          }}
        />
      )}
      {openRelease && <ReleaseDetailDialog release={openRelease} onClose={() => setOpenRelease(null)} />}
      {yanking && <YankDialog release={yanking} onClose={() => setYanking(null)} />}
      {unyanking && (
        <AlertDialog open onOpenChange={() => setUnyanking(null)}>
          <AlertDialogContent>
            <AlertDialogHeader>
              <AlertDialogTitle>{t("releases.unyankTitle", { version: unyanking.version })}</AlertDialogTitle>
              <AlertDialogDescription>
                {t("releases.unyankDesc", { version: unyanking.version })}
              </AlertDialogDescription>
            </AlertDialogHeader>
            <div className="flex justify-end gap-2">
              <AlertDialogCancel>{t("common.cancel")}</AlertDialogCancel>
              <AlertDialogAction onClick={() => unyanking && unyankMut.mutate(unyanking.id)}>
                {t("releases.unyank")}
              </AlertDialogAction>
            </div>
          </AlertDialogContent>
        </AlertDialog>
      )}
      {settings && (
        <UpdateSettingsDialog
          key={`${settings.productId}-${settings.tab}`}
          initialProductId={settings.productId}
          initialTab={settings.tab}
          lockProduct={settings.productId !== ""}
          onClose={closeSettings}
        />
      )}
      {deleting && (
        <AlertDialog open onOpenChange={() => setDeleting(null)}>
          <AlertDialogContent>
            <AlertDialogHeader>
              <AlertDialogTitle>{t("releases.deleteDraftTitle")}</AlertDialogTitle>
              <AlertDialogDescription>{t("releases.deleteDraftDesc")}</AlertDialogDescription>
            </AlertDialogHeader>
            <div className="flex justify-end gap-2 pt-2">
              <AlertDialogCancel>{t("common.cancel")}</AlertDialogCancel>
              <AlertDialogAction onClick={() => deleteMut.mutate(deleting.id)} disabled={deleteMut.isPending}>
                {t("common.delete")}
              </AlertDialogAction>
            </div>
          </AlertDialogContent>
        </AlertDialog>
      )}
      {confirmPublish && (
        <AlertDialog open onOpenChange={() => setConfirmPublish(null)}>
          <AlertDialogContent>
            <AlertDialogHeader>
              <AlertDialogTitle>{t("releases.publishOlderTitle")}</AlertDialogTitle>
              <AlertDialogDescription>
                {rich(t("releases.publishOlderDesc"), {
                  version: <strong>{confirmPublish.rel.version}</strong>,
                  latest: <strong>{confirmPublish.latest}</strong>,
                })}
              </AlertDialogDescription>
            </AlertDialogHeader>
            <div className="flex justify-end gap-2 pt-2">
              <AlertDialogCancel>{t("common.cancel")}</AlertDialogCancel>
              <AlertDialogAction
                onClick={() => {
                  publishMut.mutate(confirmPublish.rel.id)
                  setConfirmPublish(null)
                }}
              >
                {t("releases.publishAnyway")}
              </AlertDialogAction>
            </div>
          </AlertDialogContent>
        </AlertDialog>
      )}
    </div>
  )
}

function StatusBadge({ status, yankedReason }: { status: string; yankedReason?: string }) {
  const { t } = useI18n()
  const label =
    status === "published"
      ? t("releases.badgePublished")
      : status === "yanked"
        ? t("releases.badgeYanked")
        : status === "draft"
          ? t("releases.badgeDraft")
          : status
  const cls =
    status === "published"
      ? "bg-emerald-100 text-emerald-800"
      : status === "yanked"
        ? "bg-red-100 text-red-800"
        : "bg-amber-100 text-amber-800"
  return (
    <Badge className={cls} title={yankedReason}>
      {status === "yanked" && <AlertTriangle className="h-3 w-3 mr-1" />}
      {label}
    </Badge>
  )
}

// ─── Create Release Dialog (release metadata only; no artifacts yet) ──────

function CreateReleaseDialog({ onClose, onCreated }: { onClose: () => void; onCreated: (rel: Release) => void }) {
  const { t } = useI18n()
  const qc = useQueryClient()
  // Empty until picked: the candidates are searched on the server, so
  // there is no "first product" on hand to default to — and defaulting
  // to whichever one happened to load is how a release ends up filed
  // against a product nobody chose.
  const [productId, setProductId] = useState("")
  const [version, setVersion] = useState("")
  const [channel, setChannel] = useState<(typeof RELEASE_CHANNELS)[number]>("stable")
  const [name, setName] = useState("")
  const [releaseNotes, setReleaseNotes] = useState("")
  const [error, setError] = useState("")

  const mut = useMutation({
    mutationFn: () =>
      admin.createRelease({
        product_id: productId,
        version,
        channel,
        name,
        release_notes: releaseNotes,
      }),
    onSuccess: (rel) => {
      qc.invalidateQueries({ queryKey: ["admin", "releases"] })
      showToast(t("releases.toastDraftCreated", { version: rel.version }), "success")
      onCreated(rel)
    },
    onError: (e: Error) => setError(e.message),
  })

  return (
    <Dialog open onOpenChange={onClose}>
      <DialogContent className="max-w-lg h-[min(570px,85vh)]">
        <DialogHeader>
          <DialogTitle>{t("releases.newRelease")}</DialogTitle>
          <DialogDescription>{t("releases.createDesc")}</DialogDescription>
        </DialogHeader>
        <DialogBody>
          <div className="space-y-4 py-2">
            <div className="space-y-2">
              <Label>{t("common.product")}</Label>
              <ProductSelect
                value={productId}
                onChange={setProductId}
                className="w-full"
                types={RELEASE_PRODUCT_TYPES}
              />
            </div>
            <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
              <div className="space-y-2">
                <Label>{t("releases.version")}</Label>
                <Input placeholder="1.2.3" value={version} onChange={(e) => setVersion(e.target.value)} />
              </div>
              <div className="space-y-2">
                <Label>{t("releases.channel")}</Label>
                <Select value={channel} onValueChange={(v) => setChannel(v as typeof channel)}>
                  <SelectTrigger>
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
              </div>
            </div>
            <div className="space-y-2">
              <Label>{t("releases.displayName")}</Label>
              <Input placeholder="MyApp Pro" value={name} onChange={(e) => setName(e.target.value)} />
            </div>
            <div className="space-y-2">
              <Label>{t("releases.releaseNotes")}</Label>
              <textarea
                rows={4}
                placeholder={t("releases.releaseNotesPlaceholder")}
                value={releaseNotes}
                onChange={(e) => setReleaseNotes(e.target.value)}
                className="flex min-h-[60px] w-full rounded-md border border-input bg-background px-3 py-2 text-sm placeholder:text-muted-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
              />
            </div>
            {error && <p className="text-sm text-destructive">{error}</p>}
          </div>
          <div className="flex justify-end gap-2 pt-2">
            <Button variant="outline" onClick={onClose}>
              {t("common.cancel")}
            </Button>
            <Button onClick={() => mut.mutate()} disabled={!productId || !version || mut.isPending}>
              {mut.isPending ? t("releases.creating") : t("releases.createDraft")}
            </Button>
          </div>
        </DialogBody>
      </DialogContent>
    </Dialog>
  )
}

// ─── Release Detail Dialog (manage artifacts) ─────────────────────────────

function ReleaseDetailDialog({ release, onClose }: { release: Release; onClose: () => void }) {
  const { t } = useI18n()
  const qc = useQueryClient()
  const { data: latest } = useQuery({
    queryKey: ["admin", "release", release.id],
    queryFn: () => admin.getRelease(release.id),
    initialData: release,
    refetchInterval: false,
  })
  const rel = latest || release
  const [adding, setAdding] = useState(false)

  const deleteArtifactMut = useMutation({
    mutationFn: (artifactId: string) => admin.deleteArtifact(rel.id, artifactId),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["admin", "release", rel.id] })
      qc.invalidateQueries({ queryKey: ["admin", "releases"] })
    },
    onError: (e: Error) => showToast(e.message, "error"),
  })

  const artifacts = rel.artifacts || []
  const usedPlatforms = new Set(artifacts.map((a) => a.platform))
  const remainingPlatforms = RELEASE_PLATFORMS.filter((p) => !usedPlatforms.has(p))

  return (
    <Dialog open onOpenChange={onClose}>
      <DialogContent className="max-w-2xl h-[min(580px,85vh)]">
        <DialogHeader>
          <DialogTitle>
            {rel.product?.name || rel.product_id} {rel.version}
            <Badge variant="outline" className="ml-2 capitalize text-xs">
              {rel.channel}
            </Badge>
            <StatusBadge status={rel.status} />
          </DialogTitle>
          <DialogDescription>
            {rel.status === "draft"
              ? t("releases.detailDraftDesc")
              : rel.status === "yanked"
                ? t("releases.detailYankedDesc")
                : t("releases.detailPublishedDesc")}
          </DialogDescription>
        </DialogHeader>
        <DialogBody>
          <div className="space-y-4 py-2">
            <div>
              <p className="text-sm font-medium mb-2">{t("releases.artifactsCount", { count: artifacts.length })}</p>
              {artifacts.length === 0 ? (
                <p className="text-xs text-muted-foreground py-4 text-center bg-muted/50 rounded">
                  {t("releases.noArtifacts")}
                </p>
              ) : (
                <div className="space-y-2">
                  {artifacts.map((a) => (
                    <ArtifactRow
                      key={a.id}
                      artifact={a}
                      canEdit={rel.status === "draft"}
                      onDelete={() => deleteArtifactMut.mutate(a.id)}
                    />
                  ))}
                </div>
              )}
            </div>

            {rel.status === "draft" && remainingPlatforms.length > 0 && (
              <Button onClick={() => setAdding(true)} variant="outline" className="w-full">
                <Plus className="h-4 w-4 mr-2" />{" "}
                {t("releases.addArtifactRemaining", { count: remainingPlatforms.length })}
              </Button>
            )}
          </div>

          {adding && (
            <AddArtifactDialog
              release={rel}
              availablePlatforms={remainingPlatforms}
              onClose={() => setAdding(false)}
              onAdded={() => {
                setAdding(false)
                qc.invalidateQueries({ queryKey: ["admin", "release", rel.id] })
                qc.invalidateQueries({ queryKey: ["admin", "releases"] })
              }}
            />
          )}
        </DialogBody>
      </DialogContent>
    </Dialog>
  )
}

function ArtifactRow({
  artifact,
  canEdit,
  onDelete,
}: {
  artifact: ReleaseArtifact
  canEdit: boolean
  onDelete: () => void
}) {
  const { t } = useI18n()
  const ready = !!artifact.sha256
  return (
    <div className="flex items-center gap-3 bg-muted/50 rounded px-3 py-2 text-sm">
      <Badge variant="outline" className="font-mono text-[10px]">
        {artifact.platform}
      </Badge>
      <span className="text-muted-foreground text-xs flex-1 truncate" title={artifact.filename || undefined}>
        {artifact.filename && <span className="text-foreground">{artifact.filename} · </span>}
        {ready
          ? `${formatBytes(artifact.file_size)} · sha256:${artifact.sha256.slice(0, 12)}…`
          : t("releases.notUploaded")}
      </span>
      {ready && artifact.ed25519_sig && (
        <Badge variant="outline" className="text-[10px]" title={artifact.signing_key_id}>
          {t("releases.signed")}
        </Badge>
      )}
      {ready ? (
        <Badge className="bg-emerald-100 text-emerald-800 text-[10px]">{t("releases.ready")}</Badge>
      ) : (
        <Badge className="bg-amber-100 text-amber-800 text-[10px]">{t("releases.pending")}</Badge>
      )}
      {canEdit && (
        <Button
          variant="ghost"
          size="icon"
          className="h-7 w-7 text-destructive"
          onClick={onDelete}
          title={t("releases.removeArtifact")}
          aria-label={t("releases.removeArtifact")}
        >
          <X className="h-3.5 w-3.5" />
        </Button>
      )}
    </div>
  )
}

// ─── Add Artifact Dialog (browser direct upload) ───────────────────────────

function AddArtifactDialog({
  release,
  availablePlatforms,
  onClose,
  onAdded,
}: {
  release: Release
  availablePlatforms: readonly string[]
  onClose: () => void
  onAdded: () => void
}) {
  const { t } = useI18n()
  const qc = useQueryClient()
  const [platform, setPlatform] = useState(availablePlatforms[0] || "")
  const [file, setFile] = useState<File | null>(null)
  const fileInputRef = useRef<HTMLInputElement>(null)
  const [progress, setProgress] = useState<"idle" | "init" | "uploading" | "finalizing">("idle")
  const [error, setError] = useState("")

  useEffect(() => {
    if (!availablePlatforms.includes(platform) && availablePlatforms.length > 0) {
      setPlatform(availablePlatforms[0])
    }
  }, [availablePlatforms, platform])

  const onFileChange = (e: ChangeEvent<HTMLInputElement>) => {
    const f = e.target.files?.[0]
    if (f) setFile(f)
  }

  const handleSubmit = async () => {
    setError("")
    if (!platform || !file) {
      setError(t("releases.platformAndFileRequired"))
      return
    }
    try {
      setProgress("init")
      const init = await admin.addArtifact(release.id, {
        platform,
        content_type: file.type || "application/octet-stream",
        expected_size: file.size,
        filename: file.name,
      })

      setProgress("uploading")
      const putResp = await fetch(init.upload_url, {
        method: "PUT",
        body: file,
        headers: { "Content-Type": file.type || "application/octet-stream" },
      })
      if (!putResp.ok) {
        throw new Error(t("releases.uploadFailed", { status: putResp.status, statusText: putResp.statusText }))
      }

      setProgress("finalizing")
      // The server hashes the object itself; the client hash is only a
      // cross-check, and reading a multi-GB installer into memory to
      // compute it fails in most browsers.
      const expected_sha256 = file.size <= CLIENT_HASH_MAX_BYTES ? await sha256Hex(file) : undefined
      await admin.finalizeArtifact(release.id, init.artifact.id, { expected_sha256 })

      showToast(t("releases.toastArtifactUploaded", { platform }), "success")
      onAdded()
    } catch (e) {
      const msg = e instanceof Error ? e.message : String(e)
      setError(msg)
      setProgress("idle")
      // The artifact row may already exist (pending) even though the
      // upload failed; refresh so it shows and a retry doesn't hit 409.
      qc.invalidateQueries({ queryKey: ["admin", "release", release.id] })
    }
  }

  const busy = progress !== "idle"

  return (
    <Dialog open onOpenChange={busy ? undefined : onClose}>
      <DialogContent className="h-[min(420px,85vh)]">
        <DialogHeader>
          <DialogTitle>{t("releases.addArtifact")}</DialogTitle>
          <DialogDescription>{t("releases.addArtifactDesc", { version: release.version })}</DialogDescription>
        </DialogHeader>
        <DialogBody>
          <div className="space-y-4 py-2">
            <div className="space-y-2">
              <Label>{t("releases.platform")}</Label>
              <Select value={platform} onValueChange={setPlatform} disabled={busy}>
                <SelectTrigger>
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {availablePlatforms.map((p) => (
                    <SelectItem key={p} value={p}>
                      {p}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
            <div className="space-y-2">
              <Label>{t("releases.artifactFile")}</Label>
              <input
                ref={fileInputRef}
                type="file"
                onChange={onFileChange}
                disabled={busy}
                className="text-sm w-full"
              />
              {file && (
                <p className="text-xs text-muted-foreground">
                  {file.name} · {formatBytes(file.size)}
                </p>
              )}
            </div>
            {error && <p className="text-sm text-destructive">{error}</p>}
            {busy && (
              <div className="text-sm space-y-1 bg-muted rounded-md p-3">
                {progress === "init" && t("releases.progressInit")}
                {progress === "uploading" && t("releases.progressUploading")}
                {progress === "finalizing" && t("releases.progressFinalizing")}
              </div>
            )}
          </div>
          <div className="flex justify-end gap-2 pt-2">
            <Button variant="outline" onClick={onClose} disabled={busy}>
              {t("common.cancel")}
            </Button>
            <Button onClick={handleSubmit} disabled={busy || !file || !platform}>
              <Upload className="h-4 w-4 mr-2" />
              {busy ? t("releases.working") : t("releases.upload")}
            </Button>
          </div>
        </DialogBody>
      </DialogContent>
    </Dialog>
  )
}

function YankDialog({ release, onClose }: { release: Release; onClose: () => void }) {
  const { t } = useI18n()
  const qc = useQueryClient()
  const [reason, setReason] = useState("")
  const yankMut = useMutation({
    mutationFn: (r: string) => admin.yankRelease(release.id, r),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["admin", "releases"] })
      showToast(t("releases.toastYanked"), "success")
      onClose()
    },
    onError: (e: Error) => showToast(e.message, "error"),
  })

  return (
    <Dialog open onOpenChange={onClose}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>{t("releases.yankTitle", { version: release.version })}</DialogTitle>
          <DialogDescription>{t("releases.yankDesc")}</DialogDescription>
        </DialogHeader>
        <DialogBody>
          <div className="space-y-2 py-2">
            <Label>{t("releases.reason")}</Label>
            <textarea
              rows={3}
              placeholder={t("releases.yankPlaceholder")}
              value={reason}
              onChange={(e) => setReason(e.target.value)}
              className="flex min-h-[60px] w-full rounded-md border border-input bg-background px-3 py-2 text-sm placeholder:text-muted-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
            />
          </div>
          <div className="flex justify-end gap-2 pt-2">
            <Button variant="outline" onClick={onClose}>
              {t("common.cancel")}
            </Button>
            <Button
              variant="destructive"
              onClick={() => yankMut.mutate(reason)}
              disabled={!reason.trim() || yankMut.isPending}
            >
              {t("releases.yank")}
            </Button>
          </div>
        </DialogBody>
      </DialogContent>
    </Dialog>
  )
}

// ─── Helpers ───

function formatBytes(n: number): string {
  if (n >= 1024 * 1024 * 1024) return `${(n / (1024 * 1024 * 1024)).toFixed(2)} GB`
  if (n >= 1024 * 1024) return `${(n / (1024 * 1024)).toFixed(2)} MB`
  if (n >= 1024) return `${(n / 1024).toFixed(1)} KB`
  return `${n} B`
}

const CLIENT_HASH_MAX_BYTES = 256 * 1024 * 1024

async function sha256Hex(file: File): Promise<string> {
  const buf = await file.arrayBuffer()
  const hash = await crypto.subtle.digest("SHA-256", buf)
  const bytes = new Uint8Array(hash)
  let hex = ""
  for (let i = 0; i < bytes.length; i++) {
    hex += bytes[i].toString(16).padStart(2, "0")
  }
  return hex
}

// Channel fallback chain (mirrors server behavior).
const CHANNEL_FALLBACK: Record<string, string[]> = {
  stable: ["stable"],
  beta: ["beta", "stable"],
  alpha: ["alpha", "beta", "stable"],
  dev: ["dev", "alpha", "beta", "stable"],
}

function computeLatestVersions(
  published: { product_id: string; channel: string; version: string }[],
): Map<string, string> {
  const perChannelMax = new Map<string, string>()
  const products = new Set<string>()
  for (const r of published) {
    products.add(r.product_id)
    const key = `${r.product_id}|${r.channel}`
    const cur = perChannelMax.get(key)
    if (!cur || compareSemver(r.version, cur) > 0) {
      perChannelMax.set(key, r.version)
    }
  }
  // Every channel a draft can be on, including one with nothing published
  // yet: a beta draft below the stable release is behind what beta users
  // already get, through the fallback chain.
  const out = new Map<string, string>()
  for (const productID of products) {
    for (const [channel, chain] of Object.entries(CHANNEL_FALLBACK)) {
      let max = ""
      for (const ch of chain) {
        const v = perChannelMax.get(`${productID}|${ch}`)
        if (v && (!max || compareSemver(v, max) > 0)) max = v
      }
      if (max) out.set(`${productID}|${channel}`, max)
    }
  }
  return out
}
