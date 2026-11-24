package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
)

// A pending invite that expired without being accepted holds nothing:
// nobody ever gained a seat. Nothing sweeps those rows away
// (removed_at stays NULL), so they must not count against the
// license's seat limit forever - an owner whose first couple of
// invites simply went unanswered must still be able to invite someone
// else once those links have expired.
func TestExpiredInviteDoesNotHoldASeat(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()
	_, _, lic := perSeatLicense(t, s, ctx, 2, false) // owner + 1 member seat

	expired := time.Now().Add(-time.Hour)
	stale := &model.Seat{LicenseID: lic.ID, Email: "stale-" + lic.Email, Role: "member", InviteExpiresAt: &expired}
	if _, _, err := s.AddSeatWithinLimit(ctx, stale, 2); err != nil {
		t.Fatalf("seed the stale invite: %v", err)
	}

	// The one member seat is taken by an invite that already expired;
	// a fresh invite must still fit.
	if err := invite(s, ctx, lic, 1, 2); err != nil {
		t.Fatalf("invite past an expired, unaccepted one = %v, want it to fit", err)
	}

	// With both seats genuinely occupied (one live pending invite, one
	// expired-but-uncounted), a third invite is still refused.
	if err := invite(s, ctx, lic, 2, 2); !errors.Is(err, store.ErrSeatLimitReached) {
		t.Fatalf("a third invite with the only live seat already taken = %v, want the seat limit", err)
	}
}
