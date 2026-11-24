package store_test

import (
	"context"
	"strings"
	"testing"

	"github.com/uptrace/bun"
)

// failActivationReads makes every query that reads the activations
// table fail, as a dropped connection or a statement timeout would.
type failActivationReads struct{}

func (failActivationReads) BeforeQuery(ctx context.Context, e *bun.QueryEvent) context.Context {
	if strings.Contains(e.Query, `FROM "activations"`) {
		failed, cancel := context.WithCancel(ctx)
		cancel()
		return failed
	}
	return ctx
}

func (failActivationReads) AfterQuery(context.Context, *bun.QueryEvent) {}

// A dashboard whose counts fail must not be reported as if every
// number were genuinely zero: GetStats returns the error instead of a
// zeroed Stats, so the caller renders an error rather than a
// misleading all-zero dashboard.
func TestGetStatsFailsRatherThanReturningZeros(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()

	healthy := s.DB
	s.DB = healthy.WithQueryHook(failActivationReads{})
	stats, err := s.GetStats(ctx)
	s.DB = healthy
	if err == nil {
		t.Fatal("GetStats returned nil with its activation count failing, want the error")
	}
	if stats != nil {
		t.Fatalf("GetStats returned %+v on error, want nil", stats)
	}
}
