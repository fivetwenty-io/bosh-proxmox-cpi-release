package stemcellfetch

import (
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

func TestBuildFetchedFilename_Happy(t *testing.T) {
	t.Parallel()
	name, version, sha := "ubuntu-jammy", "1.438", "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890"
	got := BuildFetchedFilename(name, version, sha)
	want := pve.BuildStemcellFilename(name, version, sha)
	if got != want {
		t.Errorf("BuildFetchedFilename = %q; want %q", got, want)
	}
	if !strings.HasSuffix(got, ".qcow2") {
		t.Errorf("expected .qcow2 suffix, got %q", got)
	}
}

func TestBuildFetchedFilename_ShortSHA(t *testing.T) {
	t.Parallel()
	// sha shorter than 8 chars → "00000000" placeholder embedded.
	got := BuildFetchedFilename("ubuntu-jammy", "1.438", "abc")
	want := pve.BuildStemcellFilename("ubuntu-jammy", "1.438", "abc")
	if got != want {
		t.Errorf("BuildFetchedFilename short-sha = %q; want %q", got, want)
	}
	if !strings.Contains(got, "00000000") {
		t.Errorf("expected 00000000 placeholder in %q", got)
	}
}

func TestFilenamePrefixForDedup_Standard(t *testing.T) {
	t.Parallel()
	got := FilenamePrefixForDedup("ubuntu-jammy", "1.438")
	want := "bosh-stemcell-ubuntu-jammy-1.438-"
	if got != want {
		t.Errorf("FilenamePrefixForDedup = %q; want %q", got, want)
	}
	// Verify it is actually a prefix of a real filename built with any sha8.
	full := BuildFetchedFilename("ubuntu-jammy", "1.438", "deadbeef12345678")
	if !strings.HasPrefix(full, got) {
		t.Errorf("full filename %q does not start with prefix %q", full, got)
	}
}

func TestFilenamePrefixForDedup_EmptyInputs(t *testing.T) {
	t.Parallel()
	// Empty name and version must not panic; result is a non-empty string
	// (sanitizer maps empty to empty then BuildStemcellFilename returns a
	// degenerate but non-empty filename).
	got := FilenamePrefixForDedup("", "")
	if got == "" {
		t.Error("FilenamePrefixForDedup(\"\",\"\") returned empty string; expected at least a partial prefix")
	}
}

func TestFilenamePrefixForDedup_SpecialChars(t *testing.T) {
	t.Parallel()
	// Special chars in name/version get sanitized to dashes; prefix still
	// matches files built from the same inputs.
	got := FilenamePrefixForDedup("Ubuntu Jammy!", "1.438 RC1")
	full := BuildFetchedFilename("Ubuntu Jammy!", "1.438 RC1", "cafebabe12345678")
	if !strings.HasPrefix(full, got) {
		t.Errorf("full filename %q does not start with prefix %q", full, got)
	}
}
