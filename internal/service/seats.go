package service

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
	"github.com/tabloy/keygate/pkg/apperr"
)

type SeatService struct {
	store   *store.Store
	webhook *WebhookService
	email   *EmailService
	// baseURL is the public origin used to build seat-invite claim
	// links (e.g. https://license.example.com). Empty in tests; the
	// service then falls back to a relative path so the email still
	// reads sensibly without a leading host.
	baseURL string
	logger  *slog.Logger
}

func NewSeatService(s *store.Store, wh *WebhookService, em *EmailService, logger *slog.Logger, baseURL string) *SeatService {
	return &SeatService{store: s, webhook: wh, email: em, logger: logger, baseURL: strings.TrimRight(baseURL, "/")}
}

type AddSeatInput struct {
	LicenseKey string
	Email      string
	Role       string
	ProductID  string
	// ActorUserID identifies the portal user driving the action,
	// for audit. Empty when invoked from an unauthenticated path
	// (none today; kept optional so legacy callers compile).
	ActorUserID string
}

func (s *SeatService) AddSeat(ctx context.Context, in AddSeatInput) (*model.Seat, error) {
	// Normalize first so validation, self-invite check, DB write, and
	// later user-row lookups all agree on the same canonical form. PG
	// is case-sensitive on email; without this, "Foo@x.com" and
	// "foo@x.com" become two separate identities — re-invite hits the
	// wrong row, accept creates a duplicate user, the audit gets ugly.
	in.Email = strings.ToLower(strings.TrimSpace(in.Email))
	if appErr := apperr.ValidateEmail(in.Email); appErr != nil {
		return nil, appErr
	}
	if in.Role == "" {
		in.Role = "member"
	}
	// Seat roles are intentionally 2-tier: admin (can invite + remove
	// other seats) and member (can use the license but not manage the
	// roster). The license owner — identified by `license.email` —
	// is implicitly above both and never needs a seat row of their
	// own; we'd reject self-invite earlier anyway. Accepting a seat
	// `role=owner` would create a redundant + confusing dual-binding,
	// and the Portal UI only ever offers admin/member, so the API
	// matches what the UI can produce.
	if in.Role != "admin" && in.Role != "member" {
		return nil, apperr.BadRequest("role must be admin or member")
	}

	// Public SDK endpoint: any "can't act on this license right now"
	// condition (missing key, wrong product, non-seat product type,
	// suspended/revoked/expired) folds to one 404 so the endpoint
	// can't probe license existence or state.
	lic, err := loadLicenseForSDK(ctx, s.store, in.LicenseKey, in.ProductID, model.CapSeats, true)
	if err != nil {
		return nil, err
	}

	// Self-invite guard. The license owner already has access via
	// the license; spinning up a seat for their own email creates a
	// confusing dual-binding (seat.user_id and license.email both
	// pointing at the same person). Cheap to block here, expensive
	// to untangle later. EqualFold catches "OWNER@x.com" even when
	// the license row stored the email in mixed case.
	if strings.EqualFold(in.Email, lic.Email) {
		return nil, apperr.BadRequest("cannot invite the license owner — they already have access")
	}

	maxSeats := 0
	if lic.Plan != nil {
		maxSeats = lic.Plan.MaxSeats
	}

	// Mint the invite token BEFORE we hand the seat to the store.
	// The plain token only leaves this function inside the email
	// body and (via the SeatCreated/Restored path) the response —
	// it never enters the DB, only its SHA256 hash does.
	plainToken, tokenHash := store.GenerateSeatInviteToken()
	if plainToken == "" {
		return nil, apperr.Internal(errors.New("invite token generation failed"))
	}
	expiresAt := time.Now().Add(store.SeatInviteTTL)
	seat := &model.Seat{
		LicenseID:       lic.ID,
		Email:           in.Email,
		Role:            in.Role,
		InviteTokenHash: tokenHash,
		InviteExpiresAt: &expiresAt,
	}
	// Atomic count-and-insert under a per-license row lock. Without
	// this, two concurrent adds can both pass the count check and
	// both insert, overshooting max_seats. Also handles the
	// restore-on-reinvite case (removed_at IS NOT NULL → un-remove).
	outcome, err := s.store.AddSeatWithinLimit(ctx, seat, maxSeats)
	if err != nil {
		if errors.Is(err, store.ErrSeatLimitReached) {
			current, _ := s.store.CountActiveSeats(ctx, lic.ID)
			return nil, apperr.WithDetails(
				apperr.Conflict("SEAT_LIMIT", "maximum seats reached"),
				map[string]any{"max": maxSeats, "current": current},
			)
		}
		return nil, apperr.Internal(err)
	}

	// SeatExisted → idempotent no-op, suppress webhook + invite so a
	// retry doesn't double-fire. The plain token we just minted is
	// discarded — the existing live invite is what's valid.
	// SeatCreated / SeatRestored both represent a fresh seat-active
	// event; fire the webhook + send the email with the claim link.
	if outcome != store.SeatExisted {
		s.webhook.Dispatch(ctx, lic.ProductID, model.EventSeatAdded, map[string]any{
			"license_id": lic.ID, "seat_id": seat.ID, "email": in.Email,
			"role": in.Role, "outcome": string(outcome),
		})
		if s.email != nil {
			productName := ""
			if lic.Product != nil {
				productName = lic.Product.Name
			}
			s.email.SendSeatInvite(in.Email, productName, lic.Email, s.inviteURL(plainToken))
		}
		s.logger.Info("seat added", "license_id", lic.ID, "email", in.Email, "outcome", outcome)
	}
	// Audit every accepted call (including SeatExisted no-ops). The
	// audit row is the only durable record of an actor attempting
	// a seat change on a customer org — webhooks fire only on real
	// changes, but incident response wants to see attempts too.
	s.store.Audit(ctx, &model.AuditLog{
		Entity: "seat", EntityID: seat.ID, Action: "seat_invited",
		ActorType: "portal_user", ActorID: in.ActorUserID,
		Changes: map[string]any{
			"license_id": lic.ID, "email": in.Email, "role": in.Role,
			"outcome": string(outcome),
		},
	})

	return seat, nil
}

// inviteURL builds the absolute claim URL for the invite email. We
// keep the path stable (/accept-invite?token=...) so the frontend
// route is a fixed contract — the email body never embeds a deep
// link to internal admin paths.
func (s *SeatService) inviteURL(plainToken string) string {
	if s.baseURL == "" {
		return "/accept-invite?token=" + plainToken
	}
	return s.baseURL + "/accept-invite?token=" + plainToken
}

// AcceptSeatInviteResult is what /seats/accept returns. We hand back
// enough context for the client to redirect into the portal with a
// pre-selected license — the seat row itself stays internal.
type AcceptSeatInviteResult struct {
	UserID      string `json:"user_id"`
	Email       string `json:"email"`
	LicenseID   string `json:"license_id"`
	ProductName string `json:"product_name,omitempty"`
	Role        string `json:"role"`
}

// AcceptSeatInvite is the portal-facing entry point for claiming a
// seat invitation. The plain token IS the email-ownership proof, so
// we don't require a prior session. We auto-create a user record
// (keyed off the seat's email, NOT client input) if none exists.
//
// Failure cases (bad token, expired, removed, already accepted)
// all collapse to a single SEAT_INVITE_INVALID error so an attacker
// can't probe token validity by response shape.
func (s *SeatService) AcceptSeatInvite(ctx context.Context, plainToken string) (*AcceptSeatInviteResult, error) {
	user, seat, err := s.store.AcceptSeatInvite(ctx, plainToken)
	if err != nil {
		if errors.Is(err, store.ErrSeatInviteInvalid) {
			return nil, apperr.New(404, "SEAT_INVITE_INVALID",
				"this invitation is invalid or has expired")
		}
		return nil, apperr.Internal(err)
	}
	// Refuse to claim a seat on a license that's no longer in good
	// standing. We undo the user_id + accepted_at fields so the row
	// is back to "invited" state and the owner can re-invite cleanly,
	// but the original token is deliberately *not* restored (see
	// store.RollbackSeatAccept) — the plaintext has already been
	// processed once, so re-issuing it would be a replay risk.
	// Folded into the same opaque error so an attacker probing
	// tokens can't tell license-state from invite-state.
	lic, lerr := s.store.FindLicenseByID(ctx, seat.LicenseID)
	if lerr != nil {
		return nil, apperr.Internal(lerr)
	}
	switch lic.Status {
	case "suspended", "revoked", "canceled", "expired":
		if rbErr := s.store.RollbackSeatAccept(ctx, seat.ID); rbErr != nil {
			s.logger.Error("seat accept rollback failed", "seat_id", seat.ID, "err", rbErr)
		}
		return nil, apperr.New(404, "SEAT_INVITE_INVALID",
			"this invitation is invalid or has expired")
	}
	productName := ""
	if lic.Product != nil {
		productName = lic.Product.Name
	}
	return &AcceptSeatInviteResult{
		UserID:      user.ID,
		Email:       user.Email,
		LicenseID:   seat.LicenseID,
		ProductName: productName,
		Role:        seat.Role,
	}, nil
}

func (s *SeatService) RemoveSeat(ctx context.Context, licenseKey, seatID, productID, actorUserID string) error {
	lic, err := loadLicenseForSDK(ctx, s.store, licenseKey, productID, model.CapSeats, true)
	if err != nil {
		return err
	}

	seat, err := s.store.FindSeatByID(ctx, seatID)
	if err != nil {
		return apperr.NotFound("SEAT", seatID)
	}
	if seat.LicenseID != lic.ID {
		return apperr.NotFound("SEAT", seatID)
	}

	if err := s.store.RemoveSeat(ctx, seatID); err != nil {
		return apperr.Internal(err)
	}

	s.webhook.Dispatch(ctx, lic.ProductID, model.EventSeatRemoved, map[string]any{
		"license_id": lic.ID, "seat_id": seatID, "email": seat.Email,
	})
	s.store.Audit(ctx, &model.AuditLog{
		Entity: "seat", EntityID: seatID, Action: "seat_removed",
		ActorType: "portal_user", ActorID: actorUserID,
		Changes: map[string]any{"license_id": lic.ID, "email": seat.Email},
	})
	s.logger.Info("seat removed", "license_id", lic.ID, "seat_id", seatID)

	return nil
}

func (s *SeatService) ListSeats(ctx context.Context, licenseKey, productID string) ([]*model.Seat, error) {
	lic, err := loadLicenseForSDK(ctx, s.store, licenseKey, productID, model.CapSeats, true)
	if err != nil {
		return nil, err
	}

	seats, _, err := s.store.ListSeats(ctx, lic.ID, store.All)
	if err != nil {
		return nil, apperr.Internal(err)
	}
	return seats, nil
}

func (s *SeatService) CheckSeatAccess(ctx context.Context, licenseKey, email, productID string) (bool, error) {
	lic, err := s.store.FindLicenseByKey(ctx, licenseKey)
	if err != nil {
		return false, nil
	}
	if productID != "" && lic.ProductID != productID {
		return false, nil
	}
	if lic.Plan == nil || lic.Plan.MaxSeats == 0 {
		return true, nil
	}
	_, err = s.store.FindSeatByEmail(ctx, lic.ID, email)
	return err == nil, nil
}
