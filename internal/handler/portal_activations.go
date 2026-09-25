package handler

import (
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
	"github.com/tabloy/keygate/pkg/response"
)

// PortalActivationsHandler exposes self-service activation management
// to the license owner (the user matching license.email) OR any
// accepted seat on the license (admin or member). The "member can
// also manage activation slots" rule matches the use case: a
// teammate who lost their laptop must be able to free their own
// slot without filing a support ticket.
//
//	GET    /api/v1/portal/licenses/:license_key/activations
//	DELETE /api/v1/portal/licenses/:license_key/activations/:activation_id
//
// The classic UX problem this solves: "I lost my old laptop, all 3
// activation slots are taken, I can't activate the new machine".
// Without self-service, that's a customer-support ticket every time.
type PortalActivationsHandler struct {
	store *store.Store
}

func NewPortalActivationsHandler(s *store.Store) *PortalActivationsHandler {
	return &PortalActivationsHandler{store: s}
}

// portalActivationView is the slimmed-down activation row exposed to
// the owner. Hides internal fields (license_id) but keeps anything
// useful for "which device am I deleting" decisions.
type portalActivationView struct {
	ID             string    `json:"id"`
	Identifier     string    `json:"identifier"`
	IdentifierType string    `json:"identifier_type"`
	Label          string    `json:"label,omitempty"`
	IPAddress      string    `json:"ip_address,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	// No omitempty: it does nothing on a struct, and this column has a
	// default of now(), so the field is always present anyway.
	LastVerified time.Time `json:"last_verified"`
}

// resolveOwnedLicense loads the license at :license_key and verifies
// the logged-in user owns it (matches license.email OR has a seat).
// Returns nil-without-writing if not authorised; the response is
// already written when err==true.
func (h *PortalActivationsHandler) resolveOwnedLicense(c *gin.Context) (*model.License, bool) {
	keyRaw := strings.TrimSpace(c.Param("license_key"))
	if keyRaw == "" {
		response.BadRequest(c, "license_key is required in the URL path")
		return nil, false
	}

	emailVal, _ := c.Get("email")
	emailStr, _ := emailVal.(string)
	if emailStr == "" {
		response.Unauthorized(c, "unauthorized")
		return nil, false
	}

	lic, err := h.store.FindLicenseByKey(c.Request.Context(), keyRaw)
	if err != nil {
		// Don't differentiate "no such license" from "you don't own it"
		// — both 404. Stops portal from being a license-key existence
		// oracle for an authenticated but malicious user.
		response.NotFound(c, "license not found")
		return nil, false
	}

	if strings.EqualFold(lic.Email, emailStr) {
		return lic, true
	}
	// ACCEPTED seat (admin or member) can also manage activations on
	// their license. Pending invites confer no authority — possessing
	// an unclaimed invite token shouldn't let an OTP-logged-in user
	// burn through activation slots before going through accept.
	if seat, err := h.store.FindSeatByEmail(c.Request.Context(), lic.ID, emailStr); err == nil && seat.AcceptedAt != nil {
		return lic, true
	}
	response.NotFound(c, "license not found")
	return nil, false
}

// GET /api/v1/portal/licenses/:license_key/activations
func (h *PortalActivationsHandler) List(c *gin.Context) {
	lic, ok := h.resolveOwnedLicense(c)
	if !ok {
		return
	}
	rows, err := h.store.ListActivations(c.Request.Context(), lic.ID)
	if err != nil {
		response.Internal(c, err)
		return
	}
	out := make([]portalActivationView, 0, len(rows))
	for _, a := range rows {
		out = append(out, portalActivationView{
			ID:             a.ID,
			Identifier:     a.Identifier,
			IdentifierType: a.IdentifierType,
			Label:          a.Label,
			IPAddress:      a.IPAddress,
			CreatedAt:      a.CreatedAt,
			LastVerified:   a.LastVerified,
		})
	}
	response.OK(c, gin.H{"activations": response.Array(out), "max": maxActivationsForLicense(lic)})
}

// DELETE /api/v1/portal/licenses/:license_key/activations/:activation_id
func (h *PortalActivationsHandler) Delete(c *gin.Context) {
	lic, ok := h.resolveOwnedLicense(c)
	if !ok {
		return
	}
	activationID := strings.TrimSpace(c.Param("activation_id"))
	if activationID == "" {
		response.BadRequest(c, "activation_id is required")
		return
	}

	// Verify the activation actually belongs to THIS license.
	// Without this check the URL pattern would let any portal user
	// delete any activation if they knew its UUID.
	rows, err := h.store.ListActivations(c.Request.Context(), lic.ID)
	if err != nil {
		response.Internal(c, err)
		return
	}
	belongs := false
	for _, a := range rows {
		if a.ID == activationID {
			belongs = true
			break
		}
	}
	if !belongs {
		response.NotFound(c, "activation not found")
		return
	}

	if err := h.store.DeleteActivationByID(c.Request.Context(), activationID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// Race: activation was already gone. Treat as success.
			response.OK(c, gin.H{"status": "deleted"})
			return
		}
		response.Internal(c, err)
		return
	}

	h.store.Audit(c.Request.Context(), &model.AuditLog{
		Entity: "activation", EntityID: activationID, Action: "deleted",
		ActorType: "portal", ActorID: emailFromContext(c), IPAddress: c.ClientIP(),
		Changes: map[string]any{"license_id": lic.ID, "via": "self_service"},
	})
	response.OK(c, gin.H{"status": "deleted"})
}

// maxActivationsForLicense returns the activation cap for a license
// (driven by the plan's max_activations). Used for UI hints like
// "2 / 3 devices in use".
func maxActivationsForLicense(lic *model.License) int {
	if lic.Plan != nil && lic.Plan.MaxActivations > 0 {
		return lic.Plan.MaxActivations
	}
	return 0
}

func emailFromContext(c *gin.Context) string {
	v, _ := c.Get("email")
	s, _ := v.(string)
	return s
}
