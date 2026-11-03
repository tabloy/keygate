package payment

import "testing"

func TestSameReleaseTrain(t *testing.T) {
	cases := []struct {
		have, want string
		ok         bool
	}{
		{"2025-08-27.basil", "2025-08-27.basil", true},
		{"2025-03-31.basil", "2025-08-27.basil", true}, // same train, different date
		{"2020-08-27", "2025-08-27.basil", false},      // pre-train account default
		{"2024-06-20", "2025-08-27.basil", false},
		{"", "2025-08-27.basil", false},
		{"2026-01-01.clover", "2025-08-27.basil", false},
	}
	for _, c := range cases {
		if got := sameReleaseTrain(c.have, c.want); got != c.ok {
			t.Errorf("sameReleaseTrain(%q, %q) = %v, want %v", c.have, c.want, got, c.ok)
		}
	}
}

func TestHasAllEvents(t *testing.T) {
	want := []*string{sp("a"), sp("b")}
	if !hasAllEvents([]string{"b", "a", "c"}, want) || !hasAllEvents([]string{"*"}, want) {
		t.Fatal("superset or wildcard must satisfy")
	}
	if hasAllEvents([]string{"a"}, want) {
		t.Fatal("missing event must not satisfy")
	}
}

func sp(s string) *string { return &s }

func TestSupportedAPIVersion(t *testing.T) {
	for v, ok := range map[string]bool{
		"2025-08-27.basil": true, "2025-03-31.basil": true,
		"2020-08-27": true, "2024-06-20": true,
		"2024-09-30.acacia": true, "2024-12-18.acacia": true,
		"": false, "2026-01-01.clover": false, "2025-08-27.preview": false,
	} {
		if got := supportedAPIVersion(v); got != ok {
			t.Errorf("supportedAPIVersion(%q) = %v, want %v", v, got, ok)
		}
	}
}

func TestIsLocalhostURL(t *testing.T) {
	cases := []struct {
		url  string
		want bool
	}{
		{"http://localhost:8080", true},
		{"http://LOCALHOST", true},
		{"http://localhost.:8080", true},
		{"http://app.localhost:3000", true},
		{"http://127.0.0.1:8080", true},
		{"http://127.1.2.3", true},
		{"http://[::1]:8080", true},
		{"http://0.0.0.0:8080", true},
		{"https://license.example.com", false},
		{"https://localhost-licenses.example.com", false},
		{"https://notlocalhost.example.com", false},
		{"https://127.0.0.1.nip.io", false},
		{"https://license.example.com/localhost", false},
		{"https://keys.example.com/?next=http://127.0.0.1", false},
		{"", false},
	}
	for _, c := range cases {
		if got := IsLocalhostURL(c.url); got != c.want {
			t.Errorf("IsLocalhostURL(%q) = %v, want %v", c.url, got, c.want)
		}
	}
}
