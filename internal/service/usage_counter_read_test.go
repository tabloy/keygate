package service

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/tabloy/keygate/internal/model"
)

type fakeUsageCounterReader struct {
	counter *model.UsageCounter
	err     error
}

func (f fakeUsageCounterReader) GetUsageCounter(context.Context, string, string, string, string) (*model.UsageCounter, error) {
	return f.counter, f.err
}

// A quota's usage read must not turn a failed read into "nothing used":
// that would report the whole quota as available to a license that may
// have used all of it. Only a missing counter row means zero.
func TestPeriodUsage(t *testing.T) {
	ctx := context.Background()
	dbDown := errors.New("connection refused")

	cases := []struct {
		name    string
		reader  fakeUsageCounterReader
		want    int64
		wantErr error
	}{
		{
			name:   "counter exists",
			reader: fakeUsageCounterReader{counter: &model.UsageCounter{Used: 80}},
			want:   80,
		},
		{
			name: "no counter yet",
			// The store hands back a zero-value struct with the error,
			// as bun's Scan does.
			reader: fakeUsageCounterReader{counter: &model.UsageCounter{}, err: sql.ErrNoRows},
			want:   0,
		},
		{
			name:    "read fails",
			reader:  fakeUsageCounterReader{counter: &model.UsageCounter{}, err: dbDown},
			wantErr: dbDown,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := periodUsage(ctx, tc.reader, "lic", "api_calls", "monthly", "2026-11")
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("used = %d, want %d", got, tc.want)
			}
		})
	}
}
