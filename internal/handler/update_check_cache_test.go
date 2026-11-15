package handler

import (
	"testing"
	"time"
)

// A failed update check is asked again after a few minutes; a
// successful one is kept for the hour.
func TestUpdateCheckCacheKeepsFailuresBriefly(t *testing.T) {
	ok, failed := &updateInfo{Latest: "v1.0.0"}, &updateInfo{Error: "failed to reach GitHub"}
	for _, tc := range []struct {
		name string
		info *updateInfo
		age  time.Duration
		want bool
	}{
		{"success, 30 min old", ok, 30 * time.Minute, true},
		{"success, 61 min old", ok, 61 * time.Minute, false},
		{"failure, 1 min old", failed, time.Minute, true},
		{"failure, 6 min old", failed, 6 * time.Minute, false},
	} {
		if got := tc.info.fresh(time.Now().Add(-tc.age)); got != tc.want {
			t.Errorf("%s: fresh = %v, want %v", tc.name, got, tc.want)
		}
	}
}
