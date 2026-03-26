package store

import (
	"context"
	"time"

	"github.com/tabloy/keygate/internal/model"
)

func (s *Store) CreateSeat(ctx context.Context, seat *model.Seat) error {
	if seat.ID == "" {
		seat.ID = newID()
	}
	_, err := s.DB.NewInsert().Model(seat).Exec(ctx)
	return err
}

func (s *Store) FindSeatByID(ctx context.Context, id string) (*model.Seat, error) {
	seat := new(model.Seat)
	return seat, s.DB.NewSelect().Model(seat).Where("id = ?", id).Scan(ctx)
}

func (s *Store) FindSeatByEmail(ctx context.Context, licenseID, email string) (*model.Seat, error) {
	seat := new(model.Seat)
	return seat, s.DB.NewSelect().Model(seat).
		Where("license_id = ? AND email = ? AND removed_at IS NULL", licenseID, email).
		Scan(ctx)
}

func (s *Store) ListSeats(ctx context.Context, licenseID string) ([]*model.Seat, error) {
	var out []*model.Seat
	err := s.DB.NewSelect().Model(&out).
		Where("license_id = ? AND removed_at IS NULL", licenseID).
		OrderExpr("created_at ASC").Scan(ctx)
	return out, err
}

func (s *Store) CountActiveSeats(ctx context.Context, licenseID string) (int, error) {
	return s.DB.NewSelect().Model((*model.Seat)(nil)).
		Where("license_id = ? AND removed_at IS NULL", licenseID).Count(ctx)
}

func (s *Store) RemoveSeat(ctx context.Context, id string) error {
	now := time.Now()
	_, err := s.DB.NewUpdate().Model((*model.Seat)(nil)).
		Set("removed_at = ?", now).
		Where("id = ? AND removed_at IS NULL", id).Exec(ctx)
	return err
}
