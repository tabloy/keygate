package service

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
	"github.com/tabloy/keygate/pkg/apperr"
)

type SeatService struct {
	store   *store.Store
	webhook *WebhookService
	logger  *slog.Logger
}

func NewSeatService(s *store.Store, wh *WebhookService, logger *slog.Logger) *SeatService {
	return &SeatService{store: s, webhook: wh, logger: logger}
}

type AddSeatInput struct {
	LicenseKey string
	Email      string
	Role       string
	ProductID  string
}

func (s *SeatService) AddSeat(ctx context.Context, in AddSeatInput) (*model.Seat, error) {
	if appErr := apperr.ValidateEmail(in.Email); appErr != nil {
		return nil, appErr
	}
	if in.Role == "" {
		in.Role = "member"
	}
	if in.Role != "owner" && in.Role != "admin" && in.Role != "member" {
		return nil, apperr.BadRequest("role must be owner, admin, or member")
	}

	lic, err := s.store.FindLicenseByKey(ctx, in.LicenseKey)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, apperr.New(404, "LICENSE_NOT_FOUND", "license not found")
		}
		return nil, apperr.Internal(err)
	}
	if in.ProductID != "" && lic.ProductID != in.ProductID {
		return nil, apperr.New(404, "LICENSE_NOT_FOUND", "license not found")
	}

	if lic.Plan != nil && lic.Plan.MaxSeats > 0 {
		count, _ := s.store.CountActiveSeats(ctx, lic.ID)
		if count >= lic.Plan.MaxSeats {
			return nil, apperr.WithDetails(
				apperr.Conflict("SEAT_LIMIT", "maximum seats reached"),
				map[string]any{"max": lic.Plan.MaxSeats, "current": count},
			)
		}
	}

	if existing, err := s.store.FindSeatByEmail(ctx, lic.ID, in.Email); err == nil && existing != nil {
		return existing, nil
	}

	seat := &model.Seat{
		LicenseID: lic.ID,
		Email:     in.Email,
		Role:      in.Role,
	}
	if err := s.store.CreateSeat(ctx, seat); err != nil {
		return nil, apperr.Internal(err)
	}

	s.webhook.Dispatch(ctx, lic.ProductID, model.EventSeatAdded, map[string]any{
		"license_id": lic.ID, "seat_id": seat.ID, "email": in.Email, "role": in.Role,
	})
	s.logger.Info("seat added", "license_id", lic.ID, "email", in.Email)

	return seat, nil
}

func (s *SeatService) RemoveSeat(ctx context.Context, licenseKey, seatID, productID string) error {
	lic, err := s.store.FindLicenseByKey(ctx, licenseKey)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return apperr.New(404, "LICENSE_NOT_FOUND", "license not found")
		}
		return apperr.Internal(err)
	}
	if productID != "" && lic.ProductID != productID {
		return apperr.New(404, "LICENSE_NOT_FOUND", "license not found")
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
	s.logger.Info("seat removed", "license_id", lic.ID, "seat_id", seatID)

	return nil
}

func (s *SeatService) ListSeats(ctx context.Context, licenseKey, productID string) ([]*model.Seat, error) {
	lic, err := s.store.FindLicenseByKey(ctx, licenseKey)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, apperr.New(404, "LICENSE_NOT_FOUND", "license not found")
		}
		return nil, apperr.Internal(err)
	}
	if productID != "" && lic.ProductID != productID {
		return nil, apperr.New(404, "LICENSE_NOT_FOUND", "license not found")
	}

	seats, err := s.store.ListSeats(ctx, lic.ID)
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
