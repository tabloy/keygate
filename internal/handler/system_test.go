package handler

import "testing"

func TestSemverNewer(t *testing.T) {
	tests := []struct {
		latest, current string
		want            bool
	}{
		// Basic comparisons
		{"1.0.1", "1.0.0", true},
		{"1.1.0", "1.0.0", true},
		{"2.0.0", "1.0.0", true},
		{"1.0.0", "1.0.0", false}, // same version
		{"1.0.0", "1.0.1", false}, // current is newer
		{"1.0.0", "2.0.0", false},

		// Multi-digit
		{"1.10.0", "1.9.0", true},
		{"1.0.10", "1.0.9", true},
		{"10.0.0", "9.0.0", true},

		// Pre-release suffix stripped
		{"1.1.0-beta", "1.0.0", true},
		{"1.0.0", "1.0.0-beta", false}, // both parse to 1.0.0

		// Edge cases
		{"", "1.0.0", false},
		{"1.0.0", "", false},
		{"1.0.0", "dev", false},
		{"", "", false},

		// Partial versions
		{"1.1", "1.0", true},
		{"1", "0", true},
	}

	for _, tt := range tests {
		got := semverNewer(tt.latest, tt.current)
		if got != tt.want {
			t.Errorf("semverNewer(%q, %q) = %v, want %v", tt.latest, tt.current, got, tt.want)
		}
	}
}

func TestStripV(t *testing.T) {
	tests := []struct{ in, want string }{
		{"v1.0.0", "1.0.0"},
		{"1.0.0", "1.0.0"},
		{"v", ""},
		{"", ""},
	}
	for _, tt := range tests {
		if got := stripV(tt.in); got != tt.want {
			t.Errorf("stripV(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
