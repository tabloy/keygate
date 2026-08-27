import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import {
  AlertTriangle,
  Check,
  ChevronRight,
  Copy,
  Download,
  KeyRound,
  Package,
  Plus,
  Rocket,
  RotateCw,
  Trash2,
  Upload,
  X,
} from "lucide-react"
import { type ChangeEvent, useEffect, useRef, useState } from "react"
import { Link } from "react-router-dom"
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
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog"
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
import { useI18n } from "@/i18n"
import {
  admin,
  RELEASE_CHANNELS,
  RELEASE_PLATFORMS,
  type Release,
  type ReleaseArtifact,
  type ReleaseSigningKey,
} from "@/lib/api"
import { formatDate } from "@/lib/utils"

const PAGE_SIZE = 20

export default function ReleasesPage() {
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
  const [showSigningKeys, setShowSigningKeys] = useState(false)
  const [openRelease, setOpenRelease] = useState<Release | null>(null)
  const [confirmPublish, setConfirmPublish] = useState<{ rel: Release; latest: string } | null>(null)

  const { data: productsData } = useQuery({
    queryKey: ["admin", "products"],
    queryFn: () => admin.listProducts(),
  })
  // Only desktop + hybrid products can own releases. SaaS products
  // are filtered out everywhere release-ish: list filter, create
  // dialog, signing-key dialog. Mirrors the backend capability gate.
  const products = (productsData?.products || []).filter((p) => p.type !== "saas")

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
  const latestByBucket = computeLatestVersions(releases)

  const publishMut = useMutation({
    mutationFn: admin.publishRelease,
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["admin", "releases"] })
      showToast(t("releases.releasePublishedToast"), "success")
    },
    onError: (e: Error) => showToast(e.message, "error"),
  })
  const unyankMut = useMutation({
    mutationFn: admin.unyankRelease,
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["admin", "releases"] })
      showToast(t("releases.releaseUnyankedToast"), "success")
      setUnyanking(null)
    },
    onError: (e: Error) => showToast(e.message, "error"),
  })
  const deleteMut = useMutation({
    mutationFn: admin.deleteRelease,
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["admin", "releases"] })
      setDeleting(null)
      showToast(t("releases.draftDeletedToast"), "success")
    },
    onError: (e: Error) => showToast(e.message, "error"),
  })

  // No release-eligible products. Either no products at all, or the
  // admin has only saas products (which don't ship installable binaries).
  if (products.length === 0 && !isLoading) {
    const hasAnyProducts = (productsData?.products || []).length > 0
    return (
      <div className="space-y-6">
        <div>
          <h1 className="text-2xl font-bold tracking-tight">{t("releases.title")}</h1>
          <p className="text-muted-foreground">{t("releases.subtitle")}</p>
        </div>
        <Card>
          <CardContent className="py-12 text-center">
            <Package className="h-12 w-12 mx-auto text-muted-foreground mb-4" />
            {hasAnyProducts ? (
              <>
                <p className="text-lg font-medium">{t("releases.noEligibleProducts")}</p>
                <p className="text-muted-foreground mt-1 mb-4">{t("releases.noEligibleProductsDesc")}</p>
              </>
            ) : (
              <>
                <p className="text-lg font-medium">{t("releases.noProducts")}</p>
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
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-bold tracking-tight">{t("releases.title")}</h1>
          <p className="text-muted-foreground">{t("releases.subtitle")}</p>
        </div>
        <div className="flex gap-2">
          <Button variant="outline" onClick={() => setShowSigningKeys(true)}>
            <KeyRound className="h-4 w-4 mr-2" /> {t("releases.signingKeys")}
          </Button>
          <Button onClick={() => setCreating(true)}>
            <Plus className="h-4 w-4 mr-2" /> {t("releases.new")}
          </Button>
        </div>
      </div>

      <div className="flex flex-wrap gap-3">
        <Select value={productFilter || "all"} onValueChange={(v) => setProductFilter(v === "all" ? "" : v)}>
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
                        title={`Below current latest (${latestInBucket})`}
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
                        {artifacts.length === 1
                          ? t("releases.platformsCount", { count: 1 })
                          : t("releases.platformsCountPlural", { count: artifacts.length })}
                      </span>
                    )}
                  </DataTableCell>
                  <DataTableCell>
                    <StatusBadge status={rel.status} yankedReason={rel.yanked_reason} />
                  </DataTableCell>
                  <DataTableCell className="text-sm text-muted-foreground">{formatDate(rel.created_at)}</DataTableCell>
                  <DataTableCell className="text-right">
                    <DropdownMenu>
                      <DropdownMenuTrigger asChild>
                        <Button variant="ghost" size="sm">
                          {t("common.actions")}
                        </Button>
                      </DropdownMenuTrigger>
                      <DropdownMenuContent align="end">
                        <DropdownMenuItem onClick={() => setOpenRelease(rel)}>
                          <ChevronRight className="h-3.5 w-3.5 mr-2" /> {t("releases.viewManageArtifacts")}
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
                          <DropdownMenuItem onClick={() => setUnyanking(rel)}>
                            {t("releases.unyank")}
                          </DropdownMenuItem>
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
          products={products}
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
      {showSigningKeys && <SigningKeysDialog products={products} onClose={() => setShowSigningKeys(false)} />}
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
              <AlertDialogTitle>{t("releases.publishBelowLatestTitle")}</AlertDialogTitle>
              <AlertDialogDescription>
                {t("releases.publishBelowLatestDesc", {
                  version: confirmPublish.rel.version,
                  latest: confirmPublish.latest,
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
  const cls =
    status === "published"
      ? "bg-emerald-100 text-emerald-800"
      : status === "yanked"
        ? "bg-red-100 text-red-800"
        : "bg-amber-100 text-amber-800"

  const label =
    status === "published"
      ? t("releases.statusPublished")
      : status === "yanked"
        ? t("releases.statusYanked")
        : status === "draft"
          ? t("releases.statusDraft")
          : status

  return (
    <Badge className={cls} title={yankedReason}>
      {status === "yanked" && <AlertTriangle className="h-3 w-3 mr-1" />}
      {label}
    </Badge>
  )
}

// ─── Create Release Dialog (release metadata only; no artifacts yet) ──────

function CreateReleaseDialog({
  products,
  onClose,
  onCreated,
}: {
  products: { id: string; name: string }[]
  onClose: () => void
  onCreated: (rel: Release) => void
}) {
  const { t } = useI18n()
  const qc = useQueryClient()
  const [productId, setProductId] = useState(products[0]?.id || "")
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
      showToast(t("releases.draftCreatedToast", { version: rel.version }), "success")
      onCreated(rel)
    },
    onError: (e: Error) => setError(e.message),
  })

  return (
    <Dialog open onOpenChange={onClose}>
      <DialogContent className="max-w-lg">
        <DialogHeader>
          <DialogTitle>{t("releases.createTitle")}</DialogTitle>
          <DialogDescription>{t("releases.createDesc")}</DialogDescription>
        </DialogHeader>
        <div className="space-y-4 py-2">
          <div className="space-y-2">
            <Label>{t("common.product")}</Label>
            <Select value={productId} onValueChange={setProductId}>
              <SelectTrigger>
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {products.map((p) => (
                  <SelectItem key={p.id} value={p.id}>
                    {p.name}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
          <div className="grid grid-cols-2 gap-3">
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
            <Label>{t("releases.displayNameOptional")}</Label>
            <Input placeholder={t("releases.displayNamePlaceholder")} value={name} onChange={(e) => setName(e.target.value)} />
          </div>
          <div className="space-y-2">
            <Label>{t("releases.releaseNotesOptional")}</Label>
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
            {mut.isPending ? t("releases.creating") : t("releases.createDraftBtn")}
          </Button>
        </div>
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
      <DialogContent className="max-w-2xl max-h-[85vh] overflow-y-auto">
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
              : t("releases.detailNonDraftDesc", { status: rel.status })}
          </DialogDescription>
        </DialogHeader>

        <div className="space-y-4 py-2">
          <div>
            <p className="text-sm font-medium mb-2">{t("releases.artifactsHeading", { count: artifacts.length })}</p>
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
      <span className="text-muted-foreground text-xs flex-1 truncate">
        {ready ? `${formatBytes(artifact.file_size)} · sha256:${artifact.sha256.slice(0, 12)}…` : t("releases.notUploadedYet")}
      </span>
      {ready ? (
        <Badge className="bg-emerald-100 text-emerald-800 text-[10px]">{t("releases.statusReady")}</Badge>
      ) : (
        <Badge className="bg-amber-100 text-amber-800 text-[10px]">{t("releases.statusPending")}</Badge>
      )}
      {canEdit && (
        <Button variant="ghost" size="icon" className="h-7 w-7 text-destructive" onClick={onDelete}>
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
        throw new Error(t("releases.uploadFailed", { status: String(putResp.status), statusText: putResp.statusText }))
      }

      setProgress("finalizing")
      const sha256 = await sha256Hex(file)
      await admin.finalizeArtifact(release.id, init.artifact.id, { sha256 })

      showToast(t("releases.artifactUploadedToast", { platform }), "success")
      onAdded()
    } catch (e) {
      const msg = e instanceof Error ? e.message : String(e)
      setError(msg)
      setProgress("idle")
    }
  }

  const busy = progress !== "idle"

  return (
    <Dialog open onOpenChange={busy ? undefined : onClose}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>{t("releases.addArtifactTitle")}</DialogTitle>
          <DialogDescription>{t("releases.addArtifactDesc", { version: release.version })}</DialogDescription>
        </DialogHeader>
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
            <input ref={fileInputRef} type="file" onChange={onFileChange} disabled={busy} className="text-sm w-full" />
            {file && (
              <p className="text-xs text-muted-foreground">
                {file.name} · {formatBytes(file.size)}
              </p>
            )}
          </div>
          {error && <p className="text-sm text-destructive">{error}</p>}
          {busy && (
            <div className="text-sm space-y-1 bg-muted rounded-md p-3">
              {progress === "init" && t("releases.reservingSlot")}
              {progress === "uploading" && t("releases.uploadingToStorage")}
              {progress === "finalizing" && t("releases.computingSha")}
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
      showToast(t("releases.releaseYankedToast"), "success")
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
        <div className="space-y-2 py-2">
          <Label>{t("releases.reason")}</Label>
          <textarea
            rows={3}
            placeholder={t("releases.yankReasonPlaceholder")}
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

function parseSemver(v: string): [number, number, number, string] | null {
  const m = /^(\d+)\.(\d+)\.(\d+)(?:-([0-9A-Za-z.-]+))?(?:\+[0-9A-Za-z.-]+)?$/.exec(v)
  if (!m) return null
  for (const part of [m[1], m[2], m[3]]) {
    if (part.length > 1 && part[0] === "0") return null
  }
  const pre = m[4] ?? ""
  if (pre) {
    for (const id of pre.split(".")) {
      if (id === "") return null
      if (/^\d+$/.test(id) && id.length > 1 && id[0] === "0") return null
    }
  }
  return [Number(m[1]), Number(m[2]), Number(m[3]), pre]
}

function compareSemver(a: string, b: string): number {
  const pa = parseSemver(a)
  const pb = parseSemver(b)
  if (!pa && !pb) return 0
  if (!pa) return -1
  if (!pb) return 1
  for (let i = 0; i < 3; i++) {
    if (pa[i] !== pb[i]) return (pa[i] as number) - (pb[i] as number)
  }
  const preA = pa[3] as string
  const preB = pb[3] as string
  if (preA === preB) return 0
  if (preA === "") return 1
  if (preB === "") return -1
  const partsA = preA.split(".")
  const partsB = preB.split(".")
  const len = Math.max(partsA.length, partsB.length)
  for (let i = 0; i < len; i++) {
    const ai = partsA[i]
    const bi = partsB[i]
    if (ai === undefined) return -1
    if (bi === undefined) return 1
    const aNum = /^\d+$/.test(ai)
    const bNum = /^\d+$/.test(bi)
    if (aNum && bNum) {
      const diff = Number(ai) - Number(bi)
      if (diff !== 0) return diff
    } else if (aNum) {
      return -1
    } else if (bNum) {
      return 1
    } else if (ai !== bi) {
      return ai < bi ? -1 : 1
    }
  }
  return 0
}

// Channel fallback chain (mirrors server behavior).
const CHANNEL_FALLBACK: Record<string, string[]> = {
  stable: ["stable"],
  beta: ["beta", "stable"],
  alpha: ["alpha", "beta", "stable"],
  dev: ["dev", "alpha", "beta", "stable"],
}

function computeLatestVersions(releases: Release[]): Map<string, string> {
  const perChannelMax = new Map<string, string>()
  for (const r of releases) {
    if (r.status !== "published") continue
    const key = `${r.product_id}|${r.channel}`
    const cur = perChannelMax.get(key)
    if (!cur || compareSemver(r.version, cur) > 0) {
      perChannelMax.set(key, r.version)
    }
  }
  const out = new Map<string, string>()
  for (const [key] of perChannelMax) {
    const [productID, channel] = key.split("|")
    const chain = CHANNEL_FALLBACK[channel] ?? [channel]
    let max = ""
    for (const ch of chain) {
      const v = perChannelMax.get(`${productID}|${ch}`)
      if (v && (!max || compareSemver(v, max) > 0)) max = v
    }
    if (max) out.set(key, max)
  }
  return out
}

// ─── SigningKeysDialog ───────────────────────────────────────────────────

function SigningKeysDialog({ products, onClose }: { products: { id: string; name: string }[]; onClose: () => void }) {
  const { t } = useI18n()
  const [productId, setProductId] = useState(products[0]?.id || "")

  return (
    <Dialog open onOpenChange={onClose}>
      <DialogContent className="max-w-2xl max-h-[85vh] overflow-y-auto">
        <DialogHeader>
          <DialogTitle>{t("releases.signingKeysTitle")}</DialogTitle>
          <DialogDescription>{t("releases.signingKeysDesc")}</DialogDescription>
        </DialogHeader>
        <div className="space-y-2 py-2">
          <Label>{t("common.product")}</Label>
          <Select value={productId} onValueChange={setProductId}>
            <SelectTrigger>
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              {products.map((p) => (
                <SelectItem key={p.id} value={p.id}>
                  {p.name}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </div>
        {productId && <SigningKeysSection productId={productId} />}
      </DialogContent>
    </Dialog>
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
      showToast(t("releases.keyGeneratedToast"), "success")
    },
    onError: (e: Error) => showToast(e.message, "error"),
  })

  if (isLoading) return <div className="h-32 animate-pulse bg-muted rounded-md mt-4" />

  return (
    <div className="space-y-4 mt-4">
      {!active ? (
        <Card>
          <CardContent className="py-8 text-center">
            <KeyRound className="h-10 w-10 mx-auto text-muted-foreground mb-3" />
            <p className="font-medium">{t("releases.noActiveKey")}</p>
            <p className="text-sm text-muted-foreground mb-4">
              {t("releases.noActiveKeyDesc")}
            </p>
            <Button onClick={() => generateMut.mutate()} disabled={generateMut.isPending}>
              <Plus className="h-4 w-4 mr-2" />
              {generateMut.isPending ? t("releases.generating") : t("releases.generateKey")}
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
          <p className="text-sm font-medium mb-2">{t("releases.pastKeysHeading", { count: history.length })}</p>
          <div className="space-y-2">
            {history.map((k) => (
              <div key={k.id} className="bg-muted/50 rounded-md px-3 py-2 text-xs">
                <div className="flex items-center justify-between">
                  <code className="truncate flex-1 mr-2">{k.public_key}</code>
                  <span className="text-muted-foreground shrink-0">
                    {t("releases.rotatedAt", { date: k.rotated_at ? formatDate(k.rotated_at) : "—" })}
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
  const [copied, setCopied] = useState(false)
  const copy = () => {
    navigator.clipboard.writeText(keyRow.public_key)
    setCopied(true)
    setTimeout(() => setCopied(false), 1500)
  }

  return (
    <Card>
      <CardContent className="py-4 space-y-3">
        <div className="flex items-center justify-between">
          <div>
            <p className="font-medium text-sm">{t("releases.activeSigningKey")}</p>
            <p className="text-xs text-muted-foreground">{t("releases.createdDate", { date: formatDate(keyRow.created_at) })}</p>
          </div>
          <Badge className="bg-emerald-100 text-emerald-800">{t("common.active")}</Badge>
        </div>
        <div>
          <Label className="text-xs">{t("releases.publicKeyLabel")}</Label>
          <div className="flex items-center gap-2 mt-1 bg-muted rounded-md px-3 py-2">
            <code className="text-xs flex-1 truncate font-mono">{keyRow.public_key}</code>
            <Button variant="ghost" size="icon" className="h-7 w-7 shrink-0" onClick={copy}>
              {copied ? <Check className="h-3 w-3 text-emerald-600" /> : <Copy className="h-3 w-3" />}
            </Button>
          </div>
          <p className="text-xs text-muted-foreground mt-1">
            {t("releases.publicKeyEmbedHint")}
          </p>
        </div>
        <div className="flex flex-wrap gap-2">
          <Button variant="outline" size="sm" asChild>
            <a href={admin.publicKeyURL(productId)} download="public_key.pem">
              <Download className="h-3.5 w-3.5 mr-1.5" />
              {t("releases.downloadPem")}
            </a>
          </Button>
          <Button variant="outline" size="sm" onClick={onRotate}>
            <RotateCw className="h-3.5 w-3.5 mr-1.5" />
            {t("releases.rotate")}
          </Button>
          <Button variant="outline" size="sm" className="text-destructive" onClick={onDeactivate}>
            <Trash2 className="h-3.5 w-3.5 mr-1.5" />
            {t("releases.deactivate")}
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
      showToast(t("releases.keyRotatedToast"), "success")
      onClose()
    },
    onError: (e: Error) => showToast(e.message, "error"),
  })

  return (
    <Dialog open onOpenChange={onClose}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>{t("releases.rotateTitle")}</DialogTitle>
          <DialogDescription>{t("releases.rotateDesc")}</DialogDescription>
        </DialogHeader>
        <div className="space-y-2 py-2">
          <Label>{t("releases.rotateReasonLabel")}</Label>
          <textarea
            rows={3}
            placeholder={t("releases.rotateReasonPlaceholder")}
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
            {mut.isPending ? t("releases.rotating") : t("releases.rotate")}
          </Button>
        </div>
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
      showToast(t("releases.keyDeactivatedToast"), "success")
      onClose()
    },
    onError: (e: Error) => showToast(e.message, "error"),
  })

  return (
    <Dialog open onOpenChange={onClose}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>{t("releases.deactivateTitle")}</DialogTitle>
          <DialogDescription>{t("releases.deactivateDesc")}</DialogDescription>
        </DialogHeader>
        <div className="space-y-2 py-2">
          <Label>{t("releases.rotateReasonLabel")}</Label>
          <textarea
            rows={3}
            placeholder={t("releases.deactivateReasonPlaceholder")}
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
            {mut.isPending ? t("releases.deactivating") : t("releases.deactivate")}
          </Button>
        </div>
      </DialogContent>
    </Dialog>
  )
}
