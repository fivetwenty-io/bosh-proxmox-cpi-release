// White-box tests for buildUserAgent (§7.52).
// Uses package pve to access the unexported helper directly.
package pve

import (
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/version"
)

// TestBuildUserAgent_NoOperatorID verifies that a config with no operator_id
// produces exactly "BOSH-PVE-CPI/<version>" with no trailing space or suffix.
func TestBuildUserAgent_NoOperatorID(t *testing.T) {
	t.Parallel()
	cfg := &config.CPIConfig{}
	want := "BOSH-PVE-CPI/" + version.Short()
	got := buildUserAgent(cfg)
	if got != want {
		t.Errorf("buildUserAgent (no operator_id): got %q, want %q", got, want)
	}
}

// TestBuildUserAgent_WithOperatorID verifies that a non-empty operator_id is
// appended as " pid-<value>".
func TestBuildUserAgent_WithOperatorID(t *testing.T) {
	t.Parallel()
	cfg := &config.CPIConfig{OperatorID: "acme"}
	want := "BOSH-PVE-CPI/" + version.Short() + " pid-acme"
	got := buildUserAgent(cfg)
	if got != want {
		t.Errorf("buildUserAgent (operator_id=acme): got %q, want %q", got, want)
	}
}

// TestBuildUserAgent_NoTrailingSpace confirms no trailing space when
// operator_id is the empty string.
func TestBuildUserAgent_NoTrailingSpace(t *testing.T) {
	t.Parallel()
	cfg := &config.CPIConfig{OperatorID: ""}
	got := buildUserAgent(cfg)
	if strings.HasSuffix(got, " ") {
		t.Errorf("buildUserAgent: unexpected trailing space in %q", got)
	}
}

// TestBuildUserAgent_VersionShort confirms version.Short() (not String()) is used.
// version.Short() returns only the semver portion without commit/date noise.
func TestBuildUserAgent_VersionShort(t *testing.T) {
	t.Parallel()
	cfg := &config.CPIConfig{}
	got := buildUserAgent(cfg)
	// Must start with the prefix and embed Short(), not the full String().
	prefix := "BOSH-PVE-CPI/"
	if !strings.HasPrefix(got, prefix) {
		t.Errorf("buildUserAgent: got %q, want prefix %q", got, prefix)
	}
	ver := got[len(prefix):]
	// Strip optional operator suffix.
	if idx := strings.Index(ver, " "); idx >= 0 {
		ver = ver[:idx]
	}
	if ver != version.Short() {
		t.Errorf("buildUserAgent: version portion %q != version.Short() %q", ver, version.Short())
	}
	// Must not contain newline, commit, or date info from version.String().
	if strings.Contains(got, "\n") || strings.Contains(got, "built") {
		t.Errorf("buildUserAgent: contains version.String() noise in %q", got)
	}
}

// TestBuildUserAgent_SpecialCharsPassThrough verifies that operator_id values
// containing hyphens, underscores, and digits pass through unmodified.
func TestBuildUserAgent_SpecialCharsPassThrough(t *testing.T) {
	t.Parallel()
	cases := []struct {
		id   string
		want string
	}{
		{"my-org_1", "BOSH-PVE-CPI/" + version.Short() + " pid-my-org_1"},
		{"123", "BOSH-PVE-CPI/" + version.Short() + " pid-123"},
		{"prod.east", "BOSH-PVE-CPI/" + version.Short() + " pid-prod.east"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.id, func(t *testing.T) {
			t.Parallel()
			cfg := &config.CPIConfig{OperatorID: tc.id}
			got := buildUserAgent(cfg)
			if got != tc.want {
				t.Errorf("buildUserAgent(%q): got %q, want %q", tc.id, got, tc.want)
			}
		})
	}
}
