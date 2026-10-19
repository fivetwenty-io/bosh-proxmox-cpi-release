package version

import (
	"strings"
	"testing"
)

func TestStringDefault(t *testing.T) {
	s := String()
	if !strings.HasPrefix(s, "bosh-proxmox-cpi ") {
		t.Errorf("String() prefix wrong: %q", s)
	}
	if !strings.Contains(s, "dev") {
		t.Errorf("String() missing default version 'dev': %q", s)
	}
	if !strings.Contains(s, "unknown") {
		t.Errorf("String() missing default commit/date 'unknown': %q", s)
	}
	if !strings.Contains(s, "built ") {
		t.Errorf("String() missing 'built ' prefix for date: %q", s)
	}
}

func TestShortDefault(t *testing.T) {
	got := Short()
	if got != Version {
		t.Errorf("Short() = %q, want %q", got, Version)
	}
}

// no t.Parallel: mutates package-level Version/Commit/BuildDate
func TestStringWithOverride(t *testing.T) {
	origVersion := Version
	origCommit := Commit
	origDate := BuildDate
	defer func() {
		Version = origVersion
		Commit = origCommit
		BuildDate = origDate
	}()

	Version = "v1.2.3"
	Commit = "abc1234"
	BuildDate = "2026-05-18T00:00:00Z"

	s := String()
	want := "bosh-proxmox-cpi v1.2.3 (abc1234, built 2026-05-18T00:00:00Z)"
	if s != want {
		t.Errorf("String() = %q, want %q", s, want)
	}
}

// no t.Parallel: mutates package-level Version/BuildDate
func TestStringWithRelease(t *testing.T) {
	origVersion, origDate := Version, BuildDate
	defer func() { Version, BuildDate = origVersion, origDate }()
	Version = "3f2a9c1e"
	BuildDate = "2026-10-19T00:00:00Z"

	cases := []struct {
		release, want string
	}{
		{"0.9.5", "bosh-proxmox-cpi 0.9.5 (package 3f2a9c1e, built 2026-10-19T00:00:00Z)"},
		{"0.9.5+dev.3", "bosh-proxmox-cpi 0.9.5+dev.3 (package 3f2a9c1e, built 2026-10-19T00:00:00Z)"},
		{"", String()},
		{"0.9.5 beta", String()},
		{"0.9.5\nforged line", String()},
		{strings.Repeat("9", maxReleaseVersionLen+1), String()},
	}
	for _, tc := range cases {
		if got := StringWithRelease(tc.release); got != tc.want {
			t.Errorf("StringWithRelease(%q) = %q, want %q", tc.release, got, tc.want)
		}
	}
	if got := ReleaseVersion(strings.Repeat("9", maxReleaseVersionLen)); len(got) != maxReleaseVersionLen {
		t.Errorf("a %d-character release was dropped", maxReleaseVersionLen)
	}
}
