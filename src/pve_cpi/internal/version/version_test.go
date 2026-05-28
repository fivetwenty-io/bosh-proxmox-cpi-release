package version

import (
	"strings"
	"testing"
)

func TestStringDefault(t *testing.T) {
	s := String()
	if !strings.HasPrefix(s, "bosh-pve-cpi ") {
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
	want := "bosh-pve-cpi v1.2.3 (abc1234, built 2026-05-18T00:00:00Z)"
	if s != want {
		t.Errorf("String() = %q, want %q", s, want)
	}
}
