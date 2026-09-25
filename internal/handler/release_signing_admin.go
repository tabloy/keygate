package handler

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/service"
	"github.com/tabloy/keygate/internal/store"
	"github.com/tabloy/keygate/pkg/response"
)

// ReleaseSigningAdminHandler exposes admin-only signing key management.
//
// Operational note: the API never returns the encrypted private key, only
// the public key and metadata. Callers (the admin UI) embed the public key
// into client app binaries; the private key never leaves the server.
type ReleaseSigningAdminHandler struct {
	svc   *service.ReleaseSigningService
	store *store.Store
}

func NewReleaseSigningAdminHandler(svc *service.ReleaseSigningService, st *store.Store) *ReleaseSigningAdminHandler {
	return &ReleaseSigningAdminHandler{svc: svc, store: st}
}

// POST /api/v1/admin/products/:id/signing-key
//
// Generates a fresh keypair for the product. Fails 409 if an active key
// already exists; the admin must rotate instead.
func (h *ReleaseSigningAdminHandler) Generate(c *gin.Context) {
	productID := c.Param("id")
	if _, err := h.store.FindProductByID(c.Request.Context(), productID); err != nil {
		response.NotFound(c, "product not found")
		return
	}

	key, err := h.svc.GenerateForProduct(c.Request.Context(), productID)
	if err != nil {
		switch {
		case errors.Is(err, service.ErrSigningDisabled):
			response.Err(c, http.StatusServiceUnavailable, "SIGNING_DISABLED",
				"release signing is not configured (RELEASE_KEY_ENCRYPTION_KEY is missing)")
		case errors.Is(err, store.ErrSigningKeyAlreadyActive):
			response.Err(c, http.StatusConflict, "KEY_ALREADY_ACTIVE",
				"this product already has an active signing key — rotate instead")
		default:
			response.Internal(c, err)
		}
		return
	}

	h.store.Audit(c.Request.Context(), &model.AuditLog{
		Entity: "release_signing_key", EntityID: key.ID, Action: "generated",
		ActorType: "admin", ActorID: adminID(c), IPAddress: c.ClientIP(),
		Changes: map[string]any{"product_id": productID, "public_key": key.PublicKey},
	})

	response.Created(c, key)
}

// POST /api/v1/admin/products/:id/signing-key/rotate
//
// Request body: { "note": "..." }  // optional but recommended
//
// Generates a new active key and deactivates the previous one (if any).
func (h *ReleaseSigningAdminHandler) Rotate(c *gin.Context) {
	productID := c.Param("id")
	if _, err := h.store.FindProductByID(c.Request.Context(), productID); err != nil {
		response.NotFound(c, "product not found")
		return
	}

	var req struct {
		Note string `json:"note"`
	}
	_ = c.ShouldBindJSON(&req)
	if len(req.Note) > 256 {
		response.BadRequest(c, "note too long (max 256 chars)")
		return
	}

	key, err := h.svc.RotateForProduct(c.Request.Context(), productID, req.Note)
	if err != nil {
		switch {
		case errors.Is(err, service.ErrSigningDisabled):
			response.Err(c, http.StatusServiceUnavailable, "SIGNING_DISABLED",
				"release signing is not configured")
		default:
			response.Internal(c, err)
		}
		return
	}

	h.store.Audit(c.Request.Context(), &model.AuditLog{
		Entity: "release_signing_key", EntityID: key.ID, Action: "rotated",
		ActorType: "admin", ActorID: adminID(c), IPAddress: c.ClientIP(),
		Changes: map[string]any{
			"product_id": productID,
			"new_key_id": key.ID,
			"public_key": key.PublicKey,
			"note":       req.Note,
		},
	})

	response.Created(c, key)
}

// GET /api/v1/admin/products/:id/signing-keys
//
// Returns the full signing-key history for a product. Active key first,
// then inactive sorted by creation time desc.
func (h *ReleaseSigningAdminHandler) List(c *gin.Context) {
	productID := c.Param("id")
	if _, err := h.store.FindProductByID(c.Request.Context(), productID); err != nil {
		response.NotFound(c, "product not found")
		return
	}
	keys, err := h.store.ListSigningKeys(c.Request.Context(), productID)
	if err != nil {
		response.Internal(c, err)
		return
	}
	response.OK(c, gin.H{"keys": response.Array(keys)})
}

// GET /api/v1/admin/products/:id/signing-key/public.pem
//
// Returns the active public key as a PEM document for direct download.
// Operators embed this file in their client app source tree (e.g.
// public_key.pem next to Sparkle's Info.plist or Tauri's tauri.conf.json).
func (h *ReleaseSigningAdminHandler) DownloadPublicKey(c *gin.Context) {
	productID := c.Param("id")
	if _, err := h.store.FindProductByID(c.Request.Context(), productID); err != nil {
		response.NotFound(c, "product not found")
		return
	}
	pemBytes, err := h.svc.ExportPublicKeyPEM(c.Request.Context(), productID)
	if err != nil {
		switch {
		case errors.Is(err, service.ErrSigningKeyMissing):
			response.NotFound(c, "no active signing key for this product — generate one first")
		default:
			response.Internal(c, err)
		}
		return
	}
	c.Header("Content-Disposition", `attachment; filename="public_key.pem"`)
	c.Data(http.StatusOK, "application/x-pem-file", pemBytes)
}

// GET /api/v1/admin/products/:id/signing-key/tauri-pubkey
//
// Returns the active public key in Tauri's minisign-wrapped format
// (base64 of "Ed" + 8-byte key_id + 32-byte raw key). Tauri's
// `tauri.conf.json -> bundle.updater.pubkey` field expects exactly
// this shape; the raw PEM/Sparkle key fails Tauri's verifier.
func (h *ReleaseSigningAdminHandler) DownloadPublicKeyTauri(c *gin.Context) {
	productID := c.Param("id")
	if _, err := h.store.FindProductByID(c.Request.Context(), productID); err != nil {
		response.NotFound(c, "product not found")
		return
	}
	wrapped, err := h.svc.ExportPublicKeyTauri(c.Request.Context(), productID)
	if err != nil {
		switch {
		case errors.Is(err, service.ErrSigningKeyMissing):
			response.NotFound(c, "no active signing key for this product — generate one first")
		default:
			response.Internal(c, err)
		}
		return
	}
	response.OK(c, gin.H{"pubkey": wrapped})
}

// DELETE /api/v1/admin/products/:id/signing-key
//
// Deactivates the current active key without creating a replacement.
// Future releases will be unsigned until a new key is generated.
//
// Request body: { "note": "..." }  // optional
func (h *ReleaseSigningAdminHandler) Deactivate(c *gin.Context) {
	productID := c.Param("id")
	if _, err := h.store.FindProductByID(c.Request.Context(), productID); err != nil {
		response.NotFound(c, "product not found")
		return
	}

	key, err := h.store.FindActiveSigningKey(c.Request.Context(), productID)
	if err != nil {
		if errors.Is(err, store.ErrActiveSigningKeyMissing) {
			response.NotFound(c, "no active signing key for this product")
			return
		}
		response.Internal(c, err)
		return
	}

	var req struct {
		Note string `json:"note"`
	}
	_ = c.ShouldBindJSON(&req)
	if len(req.Note) > 256 {
		response.BadRequest(c, "note too long (max 256 chars)")
		return
	}

	if err := h.store.DeactivateSigningKey(c.Request.Context(), key.ID, req.Note); err != nil {
		response.Internal(c, err)
		return
	}

	h.store.Audit(c.Request.Context(), &model.AuditLog{
		Entity: "release_signing_key", EntityID: key.ID, Action: "deactivated",
		ActorType: "admin", ActorID: adminID(c), IPAddress: c.ClientIP(),
		Changes: map[string]any{"product_id": productID, "note": req.Note},
	})

	response.OK(c, gin.H{"status": "deactivated"})
}
