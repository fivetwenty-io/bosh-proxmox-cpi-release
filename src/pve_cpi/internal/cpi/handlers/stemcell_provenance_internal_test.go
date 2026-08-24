package handlers

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// TestBuildStemcellProvenanceTags_WithDirector verifies all four tags are
// emitted when all fields are populated.
func TestBuildStemcellProvenanceTags_WithDirector(t *testing.T) {
	cp := stemcellCloudProps{
		Name:    "Ubuntu Noble",
		Version: "1.42",
	}
	directorID := "abc-123"

	// sanitizeTagValue preserves [A-Za-z0-9-] and replaces everything else
	// with "-". Spaces → "-", dots → "-", uppercase kept.
	// "Ubuntu Noble" → "Ubuntu-Noble"
	// "1.42"         → "1-42"
	// "abc-123"      → "abc-123"
	got := buildStemcellProvenanceTags(cp, directorID)

	want := []string{
		stemcellMarkerTag,
		stemcellNameTagPrefix + "Ubuntu-Noble",
		stemcellVersionTagPrefix + "1-42",
		"director--" + "abc-123",
	}

	if len(got) != len(want) {
		t.Fatalf("tag count: got %d, want %d; got=%v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("tag[%d]: got %q, want %q", i, got[i], want[i])
		}
	}
}

// TestBuildStemcellProvenanceTags_NoDirector verifies director tag is omitted
// when directorID is empty.
func TestBuildStemcellProvenanceTags_NoDirector(t *testing.T) {
	cp := stemcellCloudProps{
		Name:    "bosh-ubuntu-jammy",
		Version: "2.0",
	}

	got := buildStemcellProvenanceTags(cp, "")

	want := []string{
		stemcellMarkerTag,
		stemcellNameTagPrefix + "bosh-ubuntu-jammy",
		stemcellVersionTagPrefix + "2-0",
	}

	if len(got) != len(want) {
		t.Fatalf("tag count: got %d, want %d; got=%v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("tag[%d]: got %q, want %q", i, got[i], want[i])
		}
	}
}

// TestBuildStemcellProvenanceTags_DirectorSanitizesToEmpty verifies director
// tag is omitted when directorID sanitizes to "".
func TestBuildStemcellProvenanceTags_DirectorSanitizesToEmpty(t *testing.T) {
	cp := stemcellCloudProps{
		Name:    "test-stemcell",
		Version: "3",
	}
	// All characters outside [A-Za-z0-9-] collapse to dashes, then trimmed.
	// "..." → "---" → trimmed → ""
	directorID := "..."

	got := buildStemcellProvenanceTags(cp, directorID)

	// director tag must be absent
	for _, tag := range got {
		if len(tag) >= 10 && tag[:10] == "director--" {
			t.Errorf("unexpected director tag %q when sanitized directorID is empty", tag)
		}
	}

	// marker must still be present
	if len(got) == 0 || got[0] != stemcellMarkerTag {
		t.Errorf("stemcellMarkerTag missing; got=%v", got)
	}
}

// TestBuildStemcellProvenanceTags_EmptyNameAndVersion verifies name/version
// tags are omitted when cp fields are empty.
func TestBuildStemcellProvenanceTags_EmptyNameAndVersion(t *testing.T) {
	cp := stemcellCloudProps{}

	got := buildStemcellProvenanceTags(cp, "")

	if len(got) != 1 {
		t.Fatalf("expected only marker tag; got %v", got)
	}
	if got[0] != stemcellMarkerTag {
		t.Errorf("got[0]=%q, want %q", got[0], stemcellMarkerTag)
	}
}

// TestBuildStemcellProvenanceTags_SanitizationCasesPreserved verifies
// sanitizeTagValue behavior: uppercase preserved, dots/spaces replaced.
func TestBuildStemcellProvenanceTags_SanitizationCasesPreserved(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"Ubuntu Noble", "Ubuntu-Noble"},
		{"ubuntu-jammy", "ubuntu-jammy"},
		{"1.42", "1-42"},
		{"1.42.0-build.12", "1-42-0-build-12"},
		{"stem_cell", "stem-cell"},
		// leading/trailing non-alnum are trimmed
		{"--foo--", "foo"},
		{"", ""},
	}

	for _, tc := range cases {
		got := sanitizeTagValue(tc.input)
		if got != tc.want {
			t.Errorf("sanitizeTagValue(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

// ============================================================
// Tests: ParseStemcellRefs and FormatStemcellRefs
// ============================================================

// TestParseStemcellProvenanceFromDescription verifies JSON parse and the
// ok=false path for empty/non-JSON input.
func TestParseStemcellProvenanceFromDescription(t *testing.T) {
	t.Parallel()

	t.Run("empty description returns ok=false", func(t *testing.T) {
		t.Parallel()
		_, ok := parseStemcellProvenanceFromDescription("")
		if ok {
			t.Error("expected ok=false for empty description")
		}
	})

	t.Run("whitespace-only returns ok=false", func(t *testing.T) {
		t.Parallel()
		_, ok := parseStemcellProvenanceFromDescription("  \n\t  ")
		if ok {
			t.Error("expected ok=false for whitespace-only description")
		}
	})

	t.Run("non-JSON text returns ok=false", func(t *testing.T) {
		t.Parallel()
		_, ok := parseStemcellProvenanceFromDescription("director: cf\ndeployment: cf\n")
		if ok {
			t.Error("expected ok=false for non-JSON text")
		}
	})

	t.Run("valid JSON with director refs parses correctly", func(t *testing.T) {
		t.Parallel()
		input := `{"name":"bosh-ubuntu-noble","version":"1.0","sha8":"ab12ef34","created":"2026-06-12T00:00:00Z","director_refs":["uuid-a","uuid-b"]}`
		prov, ok := parseStemcellProvenanceFromDescription(input)
		if !ok {
			t.Fatal("expected ok=true for valid JSON")
		}
		if prov.Name != "bosh-ubuntu-noble" {
			t.Errorf("Name = %q, want %q", prov.Name, "bosh-ubuntu-noble")
		}
		if len(prov.DirectorRefs) != 2 || prov.DirectorRefs[0] != "uuid-a" || prov.DirectorRefs[1] != "uuid-b" {
			t.Errorf("DirectorRefs = %v, want [uuid-a uuid-b]", prov.DirectorRefs)
		}
	})

	t.Run("valid JSON without refs field parses with empty DirectorRefs", func(t *testing.T) {
		t.Parallel()
		input := `{"name":"bosh-ubuntu-noble","version":"1.0","sha8":"ab12ef34","created":"2026-06-12T00:00:00Z"}`
		prov, ok := parseStemcellProvenanceFromDescription(input)
		if !ok {
			t.Fatal("expected ok=true for valid JSON")
		}
		if len(prov.DirectorRefs) != 0 {
			t.Errorf("DirectorRefs = %v, want empty", prov.DirectorRefs)
		}
	})
}

// ============================================================
// Tests: buildStemcellProvenanceNotesPath (D1/D3 path-identity CID design)
// ============================================================

// TestBuildStemcellProvenanceNotesPath_FullFields verifies every field lands
// in the JSON output, SHA8 is derived as the first 8 lowercased characters of
// sha256hex, and DirectorRefs seeds with the creating director as its sole
// initial entry.
func TestBuildStemcellProvenanceNotesPath_FullFields(t *testing.T) {
	t.Parallel()

	cp := stemcellCloudProps{
		Name:       "bosh-ubuntu-noble",
		Version:    "1.42",
		OSType:     "l26",
		DiskFormat: "qcow2",
	}
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	const sha256hex = "AB12EF34CD56AB12EF34CD56AB12EF34CD56AB12EF34CD56AB12EF34CD56AB"
	const cid = ":heavy:local:import/bosh-stemcell-ubuntu-noble-1.42-ab12ef34.qcow2"
	directorTags := map[string]string{"env": "prod"}

	notes, err := buildStemcellProvenanceNotesPath(cp, pve.StemcellKindHeavy, cid, sha256hex, "https://example.com/stemcell.tgz", "director-xyz", now, directorTags)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var p stemcellProvenance
	if err := json.Unmarshal([]byte(notes), &p); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	if p.Name != cp.Name {
		t.Errorf("Name: got %q, want %q", p.Name, cp.Name)
	}
	if p.Version != cp.Version {
		t.Errorf("Version: got %q, want %q", p.Version, cp.Version)
	}
	if p.OSType != cp.OSType {
		t.Errorf("OSType: got %q, want %q", p.OSType, cp.OSType)
	}
	if p.DiskFormat != cp.DiskFormat {
		t.Errorf("DiskFormat: got %q, want %q", p.DiskFormat, cp.DiskFormat)
	}
	wantSHA8 := strings.ToLower(sha256hex[:8])
	if p.SHA8 != wantSHA8 {
		t.Errorf("SHA8: got %q, want %q", p.SHA8, wantSHA8)
	}
	if p.SHA256 != sha256hex {
		t.Errorf("SHA256: got %q, want %q", p.SHA256, sha256hex)
	}
	if p.Kind != string(pve.StemcellKindHeavy) {
		t.Errorf("Kind: got %q, want %q", p.Kind, pve.StemcellKindHeavy)
	}
	if p.CID != cid {
		t.Errorf("CID: got %q, want %q", p.CID, cid)
	}
	if p.CreatedBy != "director-xyz" {
		t.Errorf("CreatedBy: got %q, want %q", p.CreatedBy, "director-xyz")
	}
	if len(p.DirectorRefs) != 1 || p.DirectorRefs[0] != "director-xyz" {
		t.Errorf("DirectorRefs: got %v, want [director-xyz]", p.DirectorRefs)
	}
	if p.Source == "" || strings.Contains(p.Source, "\x00") {
		t.Errorf("Source unexpectedly empty/corrupted: %q", p.Source)
	}
	if p.DirectorTags["env"] != "prod" {
		t.Errorf("DirectorTags: got %v, want env=prod", p.DirectorTags)
	}

	parsed, err := time.Parse(time.RFC3339, p.Created)
	if err != nil {
		t.Fatalf("Created parse error: %v", err)
	}
	if !parsed.Equal(now) {
		t.Errorf("Created: got %q, want %q", p.Created, now.Format(time.RFC3339))
	}
}

// TestBuildStemcellProvenanceNotesPath_LightKind verifies Kind serializes as
// "light" for pve.StemcellKindLight.
func TestBuildStemcellProvenanceNotesPath_LightKind(t *testing.T) {
	t.Parallel()

	cp := stemcellCloudProps{Name: "s", Version: "1"}
	now := time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)

	notes, err := buildStemcellProvenanceNotesPath(cp, pve.StemcellKindLight, ":light:nfs:import/x.qcow2", "", "", "director-a", now, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var p stemcellProvenance
	if err := json.Unmarshal([]byte(notes), &p); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if p.Kind != string(pve.StemcellKindLight) {
		t.Errorf("Kind: got %q, want %q", p.Kind, pve.StemcellKindLight)
	}
}

// TestBuildStemcellProvenanceNotesPath_ShortSHA256_EmptySHA8 verifies that a
// sha256hex shorter than 8 characters (including the empty string — the
// legitimate server-download-without-sha256 case) yields an empty SHA8,
// matching the BuildStemcellFilename convention for an unknown digest.
func TestBuildStemcellProvenanceNotesPath_ShortSHA256_EmptySHA8(t *testing.T) {
	t.Parallel()

	cp := stemcellCloudProps{Name: "s", Version: "1"}
	now := time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)

	cases := []string{"", "ab12"}
	for _, sha := range cases {
		sha := sha
		t.Run(fmt.Sprintf("sha256=%q", sha), func(t *testing.T) {
			t.Parallel()
			notes, err := buildStemcellProvenanceNotesPath(cp, pve.StemcellKindHeavy, ":heavy:local:import/x.qcow2", sha, "", "director-a", now, nil)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			var p stemcellProvenance
			if err := json.Unmarshal([]byte(notes), &p); err != nil {
				t.Fatalf("unmarshal failed: %v", err)
			}
			if p.SHA8 != "" {
				t.Errorf("SHA8: got %q, want empty for short/empty sha256hex %q", p.SHA8, sha)
			}
		})
	}
}

// TestBuildStemcellProvenanceNotesPath_DirectorTagsOmittedWhenEmpty verifies
// director_tags is omitted (omitempty) when directorTags is nil.
func TestBuildStemcellProvenanceNotesPath_DirectorTagsOmittedWhenEmpty(t *testing.T) {
	t.Parallel()

	cp := stemcellCloudProps{Name: "s", Version: "1"}
	now := time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)

	notes, err := buildStemcellProvenanceNotesPath(cp, pve.StemcellKindHeavy, ":heavy:local:import/x.qcow2", "", "", "director-a", now, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(notes), &m); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if _, ok := m["director_tags"]; ok {
		t.Error("director_tags must be omitted when directorTags is nil (omitempty)")
	}
}
