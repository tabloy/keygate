package store

import (
	"testing"
	"time"
)

func TestCurrentPeriodKey(t *testing.T) {
	monthly := CurrentPeriodKey("monthly")
	if len(monthly) != 7 { // "2026-03"
		t.Errorf("monthly period key should be 7 chars (YYYY-MM), got %q", monthly)
	}

	daily := CurrentPeriodKey("daily")
	if len(daily) != 10 { // "2026-03-20"
		t.Errorf("daily period key should be 10 chars (YYYY-MM-DD), got %q", daily)
	}

	hourly := CurrentPeriodKey("hourly")
	if len(hourly) != 13 { // "2026-03-20T16"
		t.Errorf("hourly period key should be 13 chars (YYYY-MM-DDTHH), got %q", hourly)
	}

	yearly := CurrentPeriodKey("yearly")
	if len(yearly) != 4 { // "2026"
		t.Errorf("yearly period key should be 4 chars (YYYY), got %q", yearly)
	}

	def := CurrentPeriodKey("unknown")
	now := time.Now().UTC()
	expected := now.Format("2006-01")
	if def != expected {
		t.Errorf("default period key should be %q, got %q", expected, def)
	}
}
