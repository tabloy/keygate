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

// Releases belong to products that ship installable binaries. The
// server enforces it; the pickers here ask for the same set so an
// admin never fills in a form the server will then refuse.
const RELEASE_PRODUCT_TYPES = ["desktop", "hybrid"]

export default function ReleasesPage() {
  // Only the heading is translated so far: the sidebar names this
  // section from the same key, and the two reading differently on one
  // screen is the confusing part. The rest of the page is still
  // English.
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
  const latestByBucket = computeLatestVersions(releases)

  const publishMut = useMutation({
    mutationFn: admin.publishRelease,
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["admin", "releases"] })
      showToast("Release published", "success")
    },
    onError: (e: Error) => showToast(e.message, "error"),
  })
  const unyankMut = useMutation({
    mutationFn: admin.unyankRelease,
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["admin", "releases"] })
      showToast("Release unyanked", "success")
      setUnyanking(null)
    },
    onError: (e: Error) => showToast(e.message, "error"),
  })
  const deleteMut = useMutation({
    mutationFn: admin.deleteRelease,
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["admin", "releases"] })
      setDeleting(null)
      showToast("Draft deleted", "success")
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
          <p className="text-muted-foreground">
            Distribute software updates to your customers via Sparkle, Velopack, or Tauri.
          </p>
        </div>
        <Card>
          <CardContent className="py-12 text-center">
            <Package className="h-12 w-12 mx-auto text-muted-foreground mb-4" />
            {hasAnyProducts ? (
              <>
                <p className="text-lg font-medium">No release-eligible products</p>
                <p className="text-muted-foreground mt-1 mb-4">
                  Release feeds are available for desktop and hybrid products only. Your existing products are all SaaS
                  — change a product's type or create a new desktop/hybrid one.
                </p>
              </>
            ) : (
              <>
                <p className="text-lg font-medium">No products yet</p>
                <p className="text-muted-foreground mt-1 mb-4">Create a product before publishing releases.</p>
              </>
            )}
            <Button asChild>
              <Link to="/admin/products">
                <Plus className="h-4 w-4 mr-2" /> {hasAnyProducts ? "Manage products" : "Create product"}
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
          <p className="text-muted-foreground">
            Distribute software updates to your customers via Sparkle, Velopack, or Tauri.
          </p>
        </div>
        <div className="flex gap-2">
          <Button variant="outline" onClick={() => setShowSigningKeys(true)}>
            <KeyRound className="h-4 w-4 mr-2" /> Signing keys
          </Button>
          <Button onClick={() => setCreating(true)}>
            <Plus className="h-4 w-4 mr-2" /> New release
          </Button>
        </div>
      </div>

      <div className="flex flex-wrap gap-3">
        <ProductSelect
          value={productFilter}
          onChange={setProductFilter}
          allLabel="All products"
          types={RELEASE_PRODUCT_TYPES}
        />
        <Select value={channelFilter || "all"} onValueChange={(v) => setChannelFilter(v === "all" ? "" : v)}>
          <SelectTrigger className="w-36">
            <SelectValue placeholder="All channels" />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="all">All channels</SelectItem>
            {RELEASE_CHANNELS.map((c) => (
              <SelectItem key={c} value={c}>
                {c}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        <Select value={statusFilter || "all"} onValueChange={(v) => setStatusFilter(v === "all" ? "" : v)}>
          <SelectTrigger className="w-36">
            <SelectValue placeholder="All statuses" />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="all">All statuses</SelectItem>
            <SelectItem value="draft">Draft</SelectItem>
            <SelectItem value="published">Published</SelectItem>
            <SelectItem value="yanked">Yanked</SelectItem>
          </SelectContent>
        </Select>
      </div>

      <DataTable>
        <DataTableHeader>
          <DataTableRow>
            <DataTableHead>Product</DataTableHead>
            <DataTableHead>Version</DataTableHead>
            <DataTableHead>Channel</DataTableHead>
            <DataTableHead>Platforms</DataTableHead>
            <DataTableHead>Status</DataTableHead>
            <DataTableHead>Created</DataTableHead>
            <DataTableHead className="text-right">Actions</DataTableHead>
          </DataTableRow>
        </DataTableHeader>
        <DataTableBody>
          {isLoading ? (
            <DataTableEmpty colSpan={7} message="Loading..." />
          ) : releases.length === 0 ? (
            <DataTableEmpty colSpan={7} message='No releases yet. Click "New release" to start.' />
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
                      title="Open release detail"
                    >
                      {rel.version}
                    </button>
                    {isBelowLatest && (
                      <Badge
                        variant="outline"
                        className="ml-1.5 text-[10px] py-0 px-1.5 border-amber-500 text-amber-700"
                        title={`Below current latest (${latestInBucket})`}
                      >
                        below latest
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
                        {artifacts.length} platform{artifacts.length === 1 ? "" : "s"}
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
                          Actions
                        </Button>
                      </DropdownMenuTrigger>
                      <DropdownMenuContent align="end">
                        <DropdownMenuItem onClick={() => setOpenRelease(rel)}>
                          <ChevronRight className="h-3.5 w-3.5 mr-2" /> View / manage artifacts
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
                            <Rocket className="h-3.5 w-3.5 mr-2" /> Publish
                          </DropdownMenuItem>
                        )}
                        {rel.status === "draft" && !allReady && (
                          <DropdownMenuItem disabled>
                            Awaiting artifacts ({artifacts.filter((a) => a.sha256).length}/{artifacts.length} ready)
                          </DropdownMenuItem>
                        )}
                        {rel.status === "published" && (
                          <DropdownMenuItem onClick={() => setYanking(rel)} className="text-destructive">
                            <AlertTriangle className="h-3.5 w-3.5 mr-2" /> Yank
                          </DropdownMenuItem>
                        )}
                        {rel.status === "yanked" && (
                          <DropdownMenuItem onClick={() => setUnyanking(rel)}>Unyank</DropdownMenuItem>
                        )}
                        <DropdownMenuSeparator />
                        {rel.status === "draft" ? (
                          <DropdownMenuItem className="text-destructive" onClick={() => setDeleting(rel)}>
                            <Trash2 className="h-3.5 w-3.5 mr-2" /> Delete draft
                          </DropdownMenuItem>
                        ) : (
                          <DropdownMenuItem disabled>
                            <Trash2 className="h-3.5 w-3.5 mr-2" /> Delete (yank instead)
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
              <AlertDialogTitle>Unyank v{unyanking.version}?</AlertDialogTitle>
              <AlertDialogDescription>
                This restores the release to the public update feed. SDK clients on the affected channel will start
                receiving v{unyanking.version} as a valid update again.
              </AlertDialogDescription>
            </AlertDialogHeader>
            <div className="flex justify-end gap-2">
              <AlertDialogCancel>Cancel</AlertDialogCancel>
              <AlertDialogAction onClick={() => unyanking && unyankMut.mutate(unyanking.id)}>Unyank</AlertDialogAction>
            </div>
          </AlertDialogContent>
        </AlertDialog>
      )}
      {showSigningKeys && <SigningKeysDialog onClose={() => setShowSigningKeys(false)} />}
      {deleting && (
        <AlertDialog open onOpenChange={() => setDeleting(null)}>
          <AlertDialogContent>
            <AlertDialogHeader>
              <AlertDialogTitle>Delete draft release?</AlertDialogTitle>
              <AlertDialogDescription>
                Permanently removes the draft and its uploaded artifacts. Published or yanked releases cannot be
                deleted.
              </AlertDialogDescription>
            </AlertDialogHeader>
            <div className="flex justify-end gap-2 pt-2">
              <AlertDialogCancel>Cancel</AlertDialogCancel>
              <AlertDialogAction onClick={() => deleteMut.mutate(deleting.id)} disabled={deleteMut.isPending}>
                Delete
              </AlertDialogAction>
            </div>
          </AlertDialogContent>
        </AlertDialog>
      )}
      {confirmPublish && (
        <AlertDialog open onOpenChange={() => setConfirmPublish(null)}>
          <AlertDialogContent>
            <AlertDialogHeader>
              <AlertDialogTitle>Publish a version below current latest?</AlertDialogTitle>
              <AlertDialogDescription>
                Publishing <strong>{confirmPublish.rel.version}</strong>, older than{" "}
                <strong>{confirmPublish.latest}</strong>. Velopack and Tauri reject downgrades by default; Sparkle's
                standard comparator is not SemVer-aware. This release will appear in your history out of order —
                appropriate for backports.
              </AlertDialogDescription>
            </AlertDialogHeader>
            <div className="flex justify-end gap-2 pt-2">
              <AlertDialogCancel>Cancel</AlertDialogCancel>
              <AlertDialogAction
                onClick={() => {
                  publishMut.mutate(confirmPublish.rel.id)
                  setConfirmPublish(null)
                }}
              >
                Publish anyway
              </AlertDialogAction>
            </div>
          </AlertDialogContent>
        </AlertDialog>
      )}
    </div>
  )
}

function StatusBadge({ status, yankedReason }: { status: string; yankedReason?: string }) {
  const cls =
    status === "published"
      ? "bg-emerald-100 text-emerald-800"
      : status === "yanked"
        ? "bg-red-100 text-red-800"
        : "bg-amber-100 text-amber-800"
  return (
    <Badge className={cls} title={yankedReason}>
      {status === "yanked" && <AlertTriangle className="h-3 w-3 mr-1" />}
      {status}
    </Badge>
  )
}

// ─── Create Release Dialog (release metadata only; no artifacts yet) ──────

function CreateReleaseDialog({ onClose, onCreated }: { onClose: () => void; onCreated: (rel: Release) => void }) {
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
      showToast(`Draft ${rel.version} created. Add artifacts to publish.`, "success")
      onCreated(rel)
    },
    onError: (e: Error) => setError(e.message),
  })

  return (
    <Dialog open onOpenChange={onClose}>
      <DialogContent className="max-w-lg">
        <DialogHeader>
          <DialogTitle>New release</DialogTitle>
          <DialogDescription>
            Create the release record. You'll add platform-specific binaries (artifacts) in the next step.
          </DialogDescription>
        </DialogHeader>
        <DialogBody>
          <div className="space-y-4 py-2">
            <div className="space-y-2">
              <Label>Product</Label>
              <ProductSelect
                value={productId}
                onChange={setProductId}
                className="w-full"
                types={RELEASE_PRODUCT_TYPES}
              />
            </div>
            <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
              <div className="space-y-2">
                <Label>Version</Label>
                <Input placeholder="1.2.3" value={version} onChange={(e) => setVersion(e.target.value)} />
              </div>
              <div className="space-y-2">
                <Label>Channel</Label>
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
              <Label>Display name (optional)</Label>
              <Input placeholder="MyApp Pro" value={name} onChange={(e) => setName(e.target.value)} />
            </div>
            <div className="space-y-2">
              <Label>Release notes (optional, markdown)</Label>
              <textarea
                rows={4}
                placeholder="What's new in this version..."
                value={releaseNotes}
                onChange={(e) => setReleaseNotes(e.target.value)}
                className="flex min-h-[60px] w-full rounded-md border border-input bg-background px-3 py-2 text-sm placeholder:text-muted-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
              />
            </div>
            {error && <p className="text-sm text-destructive">{error}</p>}
          </div>
          <div className="flex justify-end gap-2 pt-2">
            <Button variant="outline" onClick={onClose}>
              Cancel
            </Button>
            <Button onClick={() => mut.mutate()} disabled={!productId || !version || mut.isPending}>
              {mut.isPending ? "Creating..." : "Create draft"}
            </Button>
          </div>
        </DialogBody>
      </DialogContent>
    </Dialog>
  )
}

// ─── Release Detail Dialog (manage artifacts) ─────────────────────────────

function ReleaseDetailDialog({ release, onClose }: { release: Release; onClose: () => void }) {
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
      <DialogContent className="max-w-2xl">
        <DialogHeader>
          <DialogTitle>
            {rel.product?.name || rel.product_id} {rel.version}
            <Badge variant="outline" className="ml-2 capitalize text-xs">
              {rel.channel}
            </Badge>
            <StatusBadge status={rel.status} />
          </DialogTitle>
          <DialogDescription>
            {rel.status === "draft" ? (
              <>Add platform binaries below. Publish when ready.</>
            ) : (
              <>This release is {rel.status}. Artifacts cannot be modified.</>
            )}
          </DialogDescription>
        </DialogHeader>
        <DialogBody>
          <div className="space-y-4 py-2">
            <div>
              <p className="text-sm font-medium mb-2">Artifacts ({artifacts.length})</p>
              {artifacts.length === 0 ? (
                <p className="text-xs text-muted-foreground py-4 text-center bg-muted/50 rounded">
                  No artifacts yet — add at least one platform before publishing.
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
                <Plus className="h-4 w-4 mr-2" /> Add artifact ({remainingPlatforms.length} platforms remaining)
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
  const ready = !!artifact.sha256
  return (
    <div className="flex items-center gap-3 bg-muted/50 rounded px-3 py-2 text-sm">
      <Badge variant="outline" className="font-mono text-[10px]">
        {artifact.platform}
      </Badge>
      <span className="text-muted-foreground text-xs flex-1 truncate">
        {ready ? `${formatBytes(artifact.file_size)} · sha256:${artifact.sha256.slice(0, 12)}…` : "Not uploaded yet"}
      </span>
      {ready && artifact.ed25519_sig && (
        <Badge variant="outline" className="text-[10px]" title={artifact.signing_key_id}>
          signed
        </Badge>
      )}
      {ready ? (
        <Badge className="bg-emerald-100 text-emerald-800 text-[10px]">ready</Badge>
      ) : (
        <Badge className="bg-amber-100 text-amber-800 text-[10px]">pending</Badge>
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
      setError("Platform and file are required")
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
        throw new Error(`Upload failed: ${putResp.status} ${putResp.statusText}`)
      }

      setProgress("finalizing")
      // The server hashes the object itself; the client hash is only a
      // cross-check, and reading a multi-GB installer into memory to
      // compute it fails in most browsers.
      const expected_sha256 = file.size <= CLIENT_HASH_MAX_BYTES ? await sha256Hex(file) : undefined
      await admin.finalizeArtifact(release.id, init.artifact.id, { expected_sha256 })

      showToast(`Artifact for ${platform} uploaded`, "success")
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
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Add artifact</DialogTitle>
          <DialogDescription>
            Upload a platform binary for {release.version}. The file goes directly to storage; we sign + finalize on
            publish.
          </DialogDescription>
        </DialogHeader>
        <DialogBody>
          <div className="space-y-4 py-2">
            <div className="space-y-2">
              <Label>Platform</Label>
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
              <Label>Artifact file</Label>
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
                {progress === "init" && "Reserving artifact slot..."}
                {progress === "uploading" && "Uploading to storage..."}
                {progress === "finalizing" && "Computing SHA-256 + finalizing..."}
              </div>
            )}
          </div>
          <div className="flex justify-end gap-2 pt-2">
            <Button variant="outline" onClick={onClose} disabled={busy}>
              Cancel
            </Button>
            <Button onClick={handleSubmit} disabled={busy || !file || !platform}>
              <Upload className="h-4 w-4 mr-2" />
              {busy ? "Working..." : "Upload"}
            </Button>
          </div>
        </DialogBody>
      </DialogContent>
    </Dialog>
  )
}

function YankDialog({ release, onClose }: { release: Release; onClose: () => void }) {
  const qc = useQueryClient()
  const [reason, setReason] = useState("")
  const yankMut = useMutation({
    mutationFn: (r: string) => admin.yankRelease(release.id, r),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["admin", "releases"] })
      showToast("Release yanked", "success")
      onClose()
    },
    onError: (e: Error) => showToast(e.message, "error"),
  })

  return (
    <Dialog open onOpenChange={onClose}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Yank {release.version}?</DialogTitle>
          <DialogDescription>
            Yanking removes this release (and ALL its artifacts) from update feeds. Existing installs continue working.
            Provide a reason — recorded in the audit log.
          </DialogDescription>
        </DialogHeader>
        <DialogBody>
          <div className="space-y-2 py-2">
            <Label>Reason</Label>
            <textarea
              rows={3}
              placeholder="Critical bug in v1.2.3 affecting Windows users; rollback recommended."
              value={reason}
              onChange={(e) => setReason(e.target.value)}
              className="flex min-h-[60px] w-full rounded-md border border-input bg-background px-3 py-2 text-sm placeholder:text-muted-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
            />
          </div>
          <div className="flex justify-end gap-2 pt-2">
            <Button variant="outline" onClick={onClose}>
              Cancel
            </Button>
            <Button
              variant="destructive"
              onClick={() => yankMut.mutate(reason)}
              disabled={!reason.trim() || yankMut.isPending}
            >
              Yank
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

// ─── SigningKeysDialog (unchanged from before) ────────────────────────────

function SigningKeysDialog({ onClose }: { onClose: () => void }) {
  const [productId, setProductId] = useState("")

  return (
    <Dialog open onOpenChange={onClose}>
      <DialogContent className="max-w-2xl">
        <DialogHeader>
          <DialogTitle>Release signing keys</DialogTitle>
          <DialogDescription>
            Generate an Ed25519 keypair per product. The public key is embedded in your client app; the server signs
            every release artifact with the private key on publish.
          </DialogDescription>
        </DialogHeader>
        <DialogBody>
          <div className="space-y-2 py-2">
            <Label>Product</Label>
            <ProductSelect
              value={productId}
              onChange={setProductId}
              className="w-full"
              types={RELEASE_PRODUCT_TYPES}
              placeholder="Select a product"
            />
          </div>
          {productId && <SigningKeysSection productId={productId} />}
        </DialogBody>
      </DialogContent>
    </Dialog>
  )
}

function SigningKeysSection({ productId }: { productId: string }) {
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
      showToast("Signing key generated", "success")
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
            <p className="font-medium">No active signing key</p>
            <p className="text-sm text-muted-foreground mb-4">
              Publishing is blocked until you generate a key (or turn off require_signing on the product to ship
              unsigned releases).
            </p>
            <Button onClick={() => generateMut.mutate()} disabled={generateMut.isPending}>
              <Plus className="h-4 w-4 mr-2" />
              {generateMut.isPending ? "Generating..." : "Generate signing key"}
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
          <p className="text-sm font-medium mb-2">Past keys ({history.length})</p>
          <div className="space-y-2">
            {history.map((k) => (
              <div key={k.id} className="bg-muted/50 rounded-md px-3 py-2 text-xs">
                <div className="flex items-center justify-between">
                  <code className="truncate flex-1 mr-2">{k.public_key}</code>
                  <span className="text-muted-foreground shrink-0">
                    rotated {k.rotated_at ? formatDate(k.rotated_at) : "—"}
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
            <p className="font-medium text-sm">Active signing key</p>
            <p className="text-xs text-muted-foreground">Created {formatDate(keyRow.created_at)}</p>
          </div>
          <Badge className="bg-emerald-100 text-emerald-800">Active</Badge>
        </div>
        <div>
          <Label className="text-xs">Public key (Ed25519, base64)</Label>
          <div className="flex items-center gap-2 mt-1 bg-muted rounded-md px-3 py-2">
            <code className="text-xs flex-1 truncate font-mono">{keyRow.public_key}</code>
            <Button variant="ghost" size="icon" className="h-7 w-7 shrink-0" onClick={copy}>
              {copied ? <Check className="h-3 w-3 text-emerald-600" /> : <Copy className="h-3 w-3" />}
            </Button>
          </div>
          <p className="text-xs text-muted-foreground mt-1">
            Embed this in your client app's update verifier (Sparkle <code>SUPublicEDKey</code> in Info.plist, or Tauri{" "}
            <code>pubkey</code>).
          </p>
        </div>
        <div className="flex flex-wrap gap-2">
          <Button variant="outline" size="sm" asChild>
            <a href={admin.publicKeyURL(productId)} download="public_key.pem">
              <Download className="h-3.5 w-3.5 mr-1.5" />
              Download .pem
            </a>
          </Button>
          <Button variant="outline" size="sm" onClick={onRotate}>
            <RotateCw className="h-3.5 w-3.5 mr-1.5" />
            Rotate
          </Button>
          <Button variant="outline" size="sm" className="text-destructive" onClick={onDeactivate}>
            <Trash2 className="h-3.5 w-3.5 mr-1.5" />
            Deactivate
          </Button>
        </div>
      </CardContent>
    </Card>
  )
}

function RotateKeyDialog({ productId, onClose }: { productId: string; onClose: () => void }) {
  const qc = useQueryClient()
  const [note, setNote] = useState("")
  const mut = useMutation({
    mutationFn: (n: string) => admin.rotateSigningKey(productId, n),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["admin", "signing-keys", productId] })
      showToast("Signing key rotated", "success")
      onClose()
    },
    onError: (e: Error) => showToast(e.message, "error"),
  })

  return (
    <Dialog open onOpenChange={onClose}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Rotate signing key?</DialogTitle>
          <DialogDescription>
            New keypair generated. Old key is preserved in history but inactive. Existing installs with only the old
            public key embedded will fail to verify new releases.
          </DialogDescription>
        </DialogHeader>
        <DialogBody>
          <div className="space-y-2 py-2">
            <Label>Reason (audit log)</Label>
            <textarea
              rows={3}
              placeholder="Routine rotation; no key compromise."
              value={note}
              onChange={(e) => setNote(e.target.value)}
              className="flex min-h-[60px] w-full rounded-md border border-input bg-background px-3 py-2 text-sm"
            />
          </div>
          <div className="flex justify-end gap-2 pt-2">
            <Button variant="outline" onClick={onClose}>
              Cancel
            </Button>
            <Button onClick={() => mut.mutate(note)} disabled={mut.isPending}>
              {mut.isPending ? "Rotating..." : "Rotate"}
            </Button>
          </div>
        </DialogBody>
      </DialogContent>
    </Dialog>
  )
}

function DeactivateKeyDialog({ productId, onClose }: { productId: string; onClose: () => void }) {
  const qc = useQueryClient()
  const [note, setNote] = useState("")
  const mut = useMutation({
    mutationFn: (n: string) => admin.deactivateSigningKey(productId, n),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["admin", "signing-keys", productId] })
      showToast("Signing key deactivated", "success")
      onClose()
    },
    onError: (e: Error) => showToast(e.message, "error"),
  })

  return (
    <Dialog open onOpenChange={onClose}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Deactivate signing key?</DialogTitle>
          <DialogDescription>
            Already-published releases keep their signatures. New releases cannot be published until you generate a new
            key, unless require_signing is off for the product — then they ship unsigned and clients with strict
            signature checking will reject them.
          </DialogDescription>
        </DialogHeader>
        <DialogBody>
          <div className="space-y-2 py-2">
            <Label>Reason (audit log)</Label>
            <textarea
              rows={3}
              placeholder="Why are you deactivating?"
              value={note}
              onChange={(e) => setNote(e.target.value)}
              className="flex min-h-[60px] w-full rounded-md border border-input bg-background px-3 py-2 text-sm"
            />
          </div>
          <div className="flex justify-end gap-2 pt-2">
            <Button variant="outline" onClick={onClose}>
              Cancel
            </Button>
            <Button variant="destructive" onClick={() => mut.mutate(note)} disabled={mut.isPending}>
              {mut.isPending ? "Deactivating..." : "Deactivate"}
            </Button>
          </div>
        </DialogBody>
      </DialogContent>
    </Dialog>
  )
}
