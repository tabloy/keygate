package handler

import (
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/service"
	"github.com/tabloy/keygate/internal/store"
	"github.com/tabloy/keygate/pkg/response"
)

// ReleaseAdminHandler exposes admin-only release management endpoints.
//
// Industry-aligned API:
//
//	POST   /admin/releases                              Create draft release
//	GET    /admin/releases                              List releases
//	GET    /admin/releases/:id                          Get release with artifacts
//	PATCH  /admin/releases/:id                          Update display name + notes
//	DELETE /admin/releases/:id                          Delete draft (cascades artifacts)
//
//	POST   /admin/releases/:id/artifacts                Add artifact (returns presigned PUT)
//	POST   /admin/releases/:id/artifacts/:aid/finalize  Finalize artifact upload
//	DELETE /admin/releases/:id/artifacts/:aid           Delete artifact (draft only)
//
//	POST   /admin/releases/:id/actions/publish          Publish (signs all artifacts)
//	POST   /admin/releases/:id/actions/yank             Yank
//	POST   /admin/releases/:id/actions/unyank           Unyank
//
// Nested artifact endpoints validate that :aid belongs to :id — a request
// for a real artifact ID under the wrong release returns 404, not the
// artifact's data — to avoid IDOR-style cross-release access.
type ReleaseAdminHandler struct {
	svc   *service.ReleaseService
	store *store.Store
}

func NewReleaseAdminHandler(svc *service.ReleaseService, st *store.Store) *ReleaseAdminHandler {
	return &ReleaseAdminHandler{svc: svc, store: st}
}

const (
	maxReleaseNotes        = 65536
	maxReleaseName         = 256
	maxYankReason          = 1024
	maxFilenameInput       = 256
	maxExpectedSize  int64 = 10 * 1024 * 1024 * 1024
)

// ─── Create draft release ───

// POST /api/v1/admin/releases
//
// Body: { product_id, version, channel?, name?, release_notes? }
func (h *ReleaseAdminHandler) Create(c *gin.Context) {
	var req struct {
		ProductID    string `json:"product_id" binding:"required"`
		Version      string `json:"version" binding:"required"`
		Channel      string `json:"channel"`
		Name         string `json:"name"`
		ReleaseNotes string `json:"release_notes"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "product_id and version are required")
		return
	}
	if len(req.Name) > maxReleaseName {
		response.BadRequest(c, "name too long")
		return
	}
	if len(req.ReleaseNotes) > maxReleaseNotes {
		response.BadRequest(c, "release_notes too long")
		return
	}

	prod, err := h.store.FindProductByID(c.Request.Context(), req.ProductID)
	if err != nil {
		response.NotFound(c, "product not found")
		return
	}
	if !model.ProductSupports(prod.Type, model.CapReleases) {
		response.Err(c, 400, "INCOMPATIBLE_PRODUCT_TYPE",
			"release management is not available for "+prod.Type+" products")
		return
	}
	if !requireKeyProductScope(c, req.ProductID) {
		return
	}

	rel, err := h.svc.CreateRelease(c.Request.Context(), service.CreateReleaseInput{
		ProductID:    req.ProductID,
		Version:      req.Version,
		Channel:      req.Channel,
		Name:         req.Name,
		ReleaseNotes: req.ReleaseNotes,
	})
	if err != nil {
		writeAppErr(c, err)
		return
	}

	h.store.Audit(c.Request.Context(), &model.AuditLog{
		Entity: "release", EntityID: rel.ID, Action: "draft_created",
		ActorType: "admin", ActorID: adminID(c), IPAddress: c.ClientIP(),
		Changes: map[string]any{
			"product_id": req.ProductID,
			"version":    req.Version,
			"channel":    rel.Channel,
		},
	})
	response.Created(c, rel)
}

// ─── Add artifact ───

// POST /api/v1/admin/releases/:id/artifacts
//
// Body: { platform, content_type?, expected_size?, filename? }
func (h *ReleaseAdminHandler) AddArtifact(c *gin.Context) {
	id := c.Param("id")
	if !h.checkReleaseScope(c, id) {
		return
	}
	var req struct {
		Platform     string `json:"platform" binding:"required"`
		ContentType  string `json:"content_type"`
		ExpectedSize int64  `json:"expected_size"`
		Filename     string `json:"filename"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "platform is required")
		return
	}
	if req.ExpectedSize < 0 || req.ExpectedSize > maxExpectedSize {
		response.BadRequest(c, "expected_size out of range")
		return
	}
	if len(req.Filename) > maxFilenameInput {
		response.BadRequest(c, "filename too long")
		return
	}
	if len(req.ContentType) > 128 {
		response.BadRequest(c, "content_type too long")
		return
	}

	out, err := h.svc.AddArtifact(c.Request.Context(), service.AddArtifactInput{
		ReleaseID:    id,
		Platform:     req.Platform,
		ContentType:  req.ContentType,
		ExpectedSize: req.ExpectedSize,
		Filename:     req.Filename,
	})
	if err != nil {
		writeAppErr(c, err)
		return
	}

	h.store.Audit(c.Request.Context(), &model.AuditLog{
		Entity: "release_artifact", EntityID: out.Artifact.ID, Action: "added",
		ActorType: "admin", ActorID: adminID(c), IPAddress: c.ClientIP(),
		Changes: map[string]any{"release_id": id, "platform": req.Platform},
	})
	response.Created(c, out)
}

// POST /api/v1/admin/releases/:id/artifacts/:aid/finalize
//
// Body: { expected_sha256? }
//
// The server reads the uploaded bytes from storage and computes the
// authoritative sha256 itself. expected_sha256 (legacy alias: sha256)
// is optional — when provided, the server compares it to the computed
// digest and returns 409 SHA256_MISMATCH on disagreement. Clients should
// move to expected_sha256; sha256 is accepted for one release cycle.
func (h *ReleaseAdminHandler) FinalizeArtifact(c *gin.Context) {
	id := c.Param("id")
	aid := c.Param("aid")
	if !h.checkReleaseScope(c, id) {
		return
	}
	if !h.assertArtifactBelongsToRelease(c, id, aid) {
		return
	}

	var req struct {
		ExpectedSHA256 string `json:"expected_sha256"`
		// Legacy alias — drop after one release cycle.
		LegacySHA256 string `json:"sha256"`
	}
	_ = c.ShouldBindJSON(&req) // optional body
	expected := req.ExpectedSHA256
	if expected == "" {
		expected = req.LegacySHA256
	}

	a, err := h.svc.FinalizeArtifact(c.Request.Context(), service.FinalizeArtifactInput{
		ArtifactID:     aid,
		ExpectedSHA256: expected,
	})
	if err != nil {
		writeAppErr(c, err)
		return
	}

	h.store.Audit(c.Request.Context(), &model.AuditLog{
		Entity: "release_artifact", EntityID: aid, Action: "uploaded",
		ActorType: "admin", ActorID: adminID(c), IPAddress: c.ClientIP(),
		Changes: map[string]any{"release_id": id, "sha256": a.SHA256, "size": a.FileSize},
	})
	response.OK(c, a)
}

// DELETE /api/v1/admin/releases/:id/artifacts/:aid
func (h *ReleaseAdminHandler) DeleteArtifact(c *gin.Context) {
	id := c.Param("id")
	aid := c.Param("aid")
	if !h.checkReleaseScope(c, id) {
		return
	}
	if !h.assertArtifactBelongsToRelease(c, id, aid) {
		return
	}
	if err := h.svc.DeleteArtifact(c.Request.Context(), aid); err != nil {
		writeAppErr(c, err)
		return
	}
	h.store.Audit(c.Request.Context(), &model.AuditLog{
		Entity: "release_artifact", EntityID: aid, Action: "deleted",
		ActorType: "admin", ActorID: adminID(c), IPAddress: c.ClientIP(),
	})
	response.OK(c, gin.H{"status": "deleted"})
}

// assertArtifactBelongsToRelease enforces that :aid sits under :id in
// the URL. Without this, a caller could supply any artifact ID and have
// it acted on regardless of which release path they targeted — an
// IDOR-style cross-release leak. Returns false (and writes 404) on
// mismatch or missing artifact.
func (h *ReleaseAdminHandler) assertArtifactBelongsToRelease(c *gin.Context, releaseID, artifactID string) bool {
	a, err := h.store.FindArtifact(c.Request.Context(), artifactID)
	if err != nil {
		response.NotFound(c, "artifact not found")
		return false
	}
	if a.ReleaseID != releaseID {
		response.NotFound(c, "artifact not found")
		return false
	}
	return true
}

// checkReleaseScope enforces the api_key.product_id → release.product_id
// match for routes that take a :id release param. Mirrors
// AdminHandler.checkLicenseScope; centralising the lookup keeps the
// scoped-key blast radius consistent across resource families.
func (h *ReleaseAdminHandler) checkReleaseScope(c *gin.Context, releaseID string) bool {
	pid, err := h.store.GetReleaseProductID(c.Request.Context(), releaseID)
	if err != nil {
		response.NotFound(c, "release not found")
		return false
	}
	return requireKeyProductScope(c, pid)
}

// ─── Lifecycle actions ───

// POST /api/v1/admin/releases/:id/actions/publish
func (h *ReleaseAdminHandler) Publish(c *gin.Context) {
	id := c.Param("id")
	if !h.checkReleaseScope(c, id) {
		return
	}
	rel, err := h.svc.Publish(c.Request.Context(), id)
	if err != nil {
		writeAppErr(c, err)
		return
	}
	h.store.Audit(c.Request.Context(), &model.AuditLog{
		Entity: "release", EntityID: id, Action: "published",
		ActorType: "admin", ActorID: adminID(c), IPAddress: c.ClientIP(),
		Changes: map[string]any{"version": rel.Version, "channel": rel.Channel, "artifacts": len(rel.Artifacts)},
	})
	response.OK(c, rel)
}

// POST /api/v1/admin/releases/:id/actions/yank
//
// Body: { reason }
func (h *ReleaseAdminHandler) Yank(c *gin.Context) {
	id := c.Param("id")
	if !h.checkReleaseScope(c, id) {
		return
	}
	var req struct {
		Reason string `json:"reason" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "reason is required")
		return
	}
	req.Reason = strings.TrimSpace(req.Reason)
	if req.Reason == "" {
		response.BadRequest(c, "reason is required")
		return
	}
	if len(req.Reason) > maxYankReason {
		response.BadRequest(c, "reason too long")
		return
	}

	rel, err := h.svc.Yank(c.Request.Context(), id, req.Reason)
	if err != nil {
		writeAppErr(c, err)
		return
	}
	h.store.Audit(c.Request.Context(), &model.AuditLog{
		Entity: "release", EntityID: id, Action: "yanked",
		ActorType: "admin", ActorID: adminID(c), IPAddress: c.ClientIP(),
		Changes: map[string]any{"reason": req.Reason},
	})
	response.OK(c, rel)
}

// POST /api/v1/admin/releases/:id/actions/unyank
func (h *ReleaseAdminHandler) Unyank(c *gin.Context) {
	id := c.Param("id")
	if !h.checkReleaseScope(c, id) {
		return
	}
	rel, err := h.svc.Unyank(c.Request.Context(), id)
	if err != nil {
		writeAppErr(c, err)
		return
	}
	h.store.Audit(c.Request.Context(), &model.AuditLog{
		Entity: "release", EntityID: id, Action: "unyanked",
		ActorType: "admin", ActorID: adminID(c), IPAddress: c.ClientIP(),
	})
	response.OK(c, rel)
}

// ─── Update / Delete ───

// PATCH /api/v1/admin/releases/:id
//
// Body: { name?, release_notes? }
//
// Only display fields are mutable post-create. version / channel /
// status / artifacts are immutable from this endpoint — version &
// channel are part of the release identity (carve a new release if you
// need to change them); status moves through dedicated action endpoints.
func (h *ReleaseAdminHandler) Update(c *gin.Context) {
	id := c.Param("id")
	var req struct {
		Name         *string `json:"name"`
		ReleaseNotes *string `json:"release_notes"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "invalid request body")
		return
	}
	if req.Name == nil && req.ReleaseNotes == nil {
		response.BadRequest(c, "at least one of name, release_notes must be provided")
		return
	}

	rel, err := h.store.FindReleaseByID(c.Request.Context(), id)
	if err != nil {
		if err == store.ErrReleaseNotFound {
			response.NotFound(c, "release not found")
			return
		}
		response.Internal(c, err)
		return
	}
	if !requireKeyProductScope(c, rel.ProductID) {
		return
	}

	name := rel.Name
	notes := rel.ReleaseNotes
	if req.Name != nil {
		v := strings.TrimSpace(*req.Name)
		if len(v) > maxReleaseName {
			response.BadRequest(c, "name too long")
			return
		}
		name = v
	}
	if req.ReleaseNotes != nil {
		v := *req.ReleaseNotes
		if len(v) > maxReleaseNotes {
			response.BadRequest(c, "release_notes too long")
			return
		}
		notes = v
	}

	if err := h.store.UpdateReleaseNotes(c.Request.Context(), id, name, notes); err != nil {
		switch err {
		case store.ErrReleaseNotFound:
			response.NotFound(c, "release not found")
		default:
			response.Internal(c, err)
		}
		return
	}

	h.store.Audit(c.Request.Context(), &model.AuditLog{
		Entity: "release", EntityID: id, Action: "updated",
		ActorType: "admin", ActorID: adminID(c), IPAddress: c.ClientIP(),
	})

	updated, err := h.store.FindReleaseByID(c.Request.Context(), id)
	if err != nil {
		response.Internal(c, err)
		return
	}
	response.OK(c, updated)
}

// DELETE /api/v1/admin/releases/:id (draft only; cascades artifacts).
func (h *ReleaseAdminHandler) Delete(c *gin.Context) {
	id := c.Param("id")
	if !h.checkReleaseScope(c, id) {
		return
	}
	if err := h.svc.DeleteDraft(c.Request.Context(), id); err != nil {
		writeAppErr(c, err)
		return
	}
	h.store.Audit(c.Request.Context(), &model.AuditLog{
		Entity: "release", EntityID: id, Action: "deleted",
		ActorType: "admin", ActorID: adminID(c), IPAddress: c.ClientIP(),
	})
	response.OK(c, gin.H{"status": "deleted"})
}

// ─── List / Get ───

// GET /api/v1/admin/releases
func (h *ReleaseAdminHandler) List(c *gin.Context) {
	// Same window every admin list takes, and the same clamp: see
	// listPage. This endpoint used to answer 400 for a limit outside
	// 1..200; it now serves the cap instead, which no caller can be
	// worse off for, and says in the response what it applied.
	page := listPage(c)
	channel := c.Query("channel")
	if channel != "" && !model.IsValidReleaseChannel(channel) {
		response.BadRequest(c, "channel must be stable, beta, alpha, or dev")
		return
	}
	status := c.Query("status")
	if status != "" {
		switch status {
		case model.ReleaseStatusDraft, model.ReleaseStatusPublished, model.ReleaseStatusYanked:
		default:
			response.BadRequest(c, "status must be draft, published, or yanked")
			return
		}
	}

	productID := c.Query("product_id")
	// Same auto-narrowing pattern as AdminHandler.ListLicenses: a
	// product-scoped key never sees other products' releases, even
	// if it forgets the product_id filter.
	if v, ok := c.Get("api_key"); ok {
		if ak, ok := v.(*model.APIKey); ok && ak != nil && ak.ProductID != "" {
			if productID != "" && productID != ak.ProductID {
				response.Err(c, 403, "PRODUCT_SCOPE_MISMATCH",
					"api_key is bound to a different product")
				return
			}
			productID = ak.ProductID
		}
	}

	filter := store.ReleaseFilter{
		ProductID: productID,
		Channel:   channel,
		Status:    status,
		Limit:     page.Limit,
		Offset:    page.Offset,
	}

	releases, err := h.store.ListReleases(c.Request.Context(), filter)
	if err != nil {
		response.Internal(c, err)
		return
	}
	total, err := h.store.CountReleases(c.Request.Context(), filter)
	if err != nil {
		response.Internal(c, err)
		return
	}
	listOK(c, "releases", releases, total, page)
}

// GET /api/v1/admin/releases/:id
func (h *ReleaseAdminHandler) Get(c *gin.Context) {
	id := c.Param("id")
	if !h.checkReleaseScope(c, id) {
		return
	}
	rel, err := h.store.FindReleaseByID(c.Request.Context(), id)
	if err != nil {
		switch err {
		case store.ErrReleaseNotFound:
			response.NotFound(c, "release not found")
		default:
			response.Internal(c, err)
		}
		return
	}
	response.OK(c, rel)
}
