package handlers

import (
	"encoding/json"
	"testing"
	"time"
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

// TestBuildStemcellProvenanceNotes_FullFields verifies JSON output when all
// fields are non-empty.
func TestBuildStemcellProvenanceNotes_FullFields(t *testing.T) {
	cp := stemcellCloudProps{
		Name:       "bosh-ubuntu-noble",
		Version:    "1.42",
		OSType:     "l26",
		DiskFormat: "qcow2",
	}
	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)

	notes, err := buildStemcellProvenanceNotes(cp, "ab12ef34", "https://example.com/stemcell.tgz", "director-xyz", now, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var p stemcellProvenance
	if err := json.Unmarshal([]byte(notes), &p); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	if p.Name != "bosh-ubuntu-noble" {
		t.Errorf("Name: got %q, want %q", p.Name, "bosh-ubuntu-noble")
	}
	if p.Version != "1.42" {
		t.Errorf("Version: got %q, want %q", p.Version, "1.42")
	}
	if p.OSType != "l26" {
		t.Errorf("OSType: got %q, want %q", p.OSType, "l26")
	}
	if p.DiskFormat != "qcow2" {
		t.Errorf("DiskFormat: got %q, want %q", p.DiskFormat, "qcow2")
	}
	if p.SHA8 != "ab12ef34" {
		t.Errorf("SHA8: got %q, want %q", p.SHA8, "ab12ef34")
	}
	if p.Source != "https://example.com/stemcell.tgz" {
		t.Errorf("Source: got %q", p.Source)
	}
	if p.DirectorID != "director-xyz" {
		t.Errorf("DirectorID: got %q", p.DirectorID)
	}

	// Created must parse as RFC3339 and equal the input UTC time.
	parsed, err := time.Parse(time.RFC3339, p.Created)
	if err != nil {
		t.Fatalf("Created parse error: %v", err)
	}
	if !parsed.Equal(now) {
		t.Errorf("Created: got %q, want %q", p.Created, now.UTC().Format(time.RFC3339))
	}
}

// TestBuildStemcellProvenanceNotes_OmitemptyFields verifies omitempty fields
// are absent from JSON when empty.
func TestBuildStemcellProvenanceNotes_OmitemptyFields(t *testing.T) {
	cp := stemcellCloudProps{
		Name:    "minimal-stemcell",
		Version: "0.1",
		// OSType and DiskFormat intentionally empty
	}
	now := time.Date(2026, 6, 3, 0, 0, 0, 0, time.UTC)

	notes, err := buildStemcellProvenanceNotes(cp, "", "", "", now, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Unmarshal into a generic map to check key presence.
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(notes), &m); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	for _, absent := range []string{"os_type", "disk_format", "source", "director_id"} {
		if _, ok := m[absent]; ok {
			t.Errorf("key %q present in JSON but should be omitted (omitempty, empty value)", absent)
		}
	}

	// name, version, sha8, created must always appear
	for _, required := range []string{"name", "version", "sha8", "created"} {
		if _, ok := m[required]; !ok {
			t.Errorf("key %q missing from JSON", required)
		}
	}
}

// TestBuildStemcellProvenanceNotes_CreatedIsUTC verifies Created is stored in
// UTC even when a non-UTC time.Time is passed.
func TestBuildStemcellProvenanceNotes_CreatedIsUTC(t *testing.T) {
	cp := stemcellCloudProps{Name: "x", Version: "1"}
	loc, _ := time.LoadLocation("America/New_York")
	now := time.Date(2026, 6, 3, 8, 0, 0, 0, loc) // 08:00 ET = 12:00 UTC

	notes, err := buildStemcellProvenanceNotes(cp, "", "", "", now, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var p stemcellProvenance
	if err := json.Unmarshal([]byte(notes), &p); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	parsed, err := time.Parse(time.RFC3339, p.Created)
	if err != nil {
		t.Fatalf("Created parse error: %v", err)
	}

	wantUTC := now.UTC()
	if !parsed.Equal(wantUTC) {
		t.Errorf("Created time mismatch: got %v, want %v", parsed, wantUTC)
	}
	// RFC3339 UTC suffix must be "Z" or "+00:00"
	if parsed.Location() != time.UTC {
		t.Errorf("Created timezone is not UTC: %v", parsed.Location())
	}
}

// ============================================================
// Tests: ParseStemcellRefs and FormatStemcellRefs
// ============================================================

// TestParseStemcellRefs verifies CSV parse including empty-string edge case.
func TestParseStemcellRefs(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		input string
		want  []string
	}{
		{"empty string returns empty slice", "", []string{}},
		{"whitespace-only returns empty slice", "   ", []string{}},
		{"single CID", "template:6042", []string{"template:6042"}},
		{"two CIDs", "template:6042,template:6043", []string{"template:6042", "template:6043"}},
		{"trims whitespace around entries", " template:6042 , template:6043 ", []string{"template:6042", "template:6043"}},
		{"drops empty tokens between commas", "template:6042,,template:6043", []string{"template:6042", "template:6043"}},
		{"single CID with trailing comma", "template:6042,", []string{"template:6042"}},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := ParseStemcellRefs(tc.input)
			if len(got) != len(tc.want) {
				t.Fatalf("ParseStemcellRefs(%q) = %v (len %d), want %v (len %d)",
					tc.input, got, len(got), tc.want, len(tc.want))
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("ParseStemcellRefs(%q)[%d] = %q, want %q", tc.input, i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestParseStemcellRefs_EmptyStringNeverReturnsOneBlankEntry verifies the
// critical invariant: ParseStemcellRefs("") must return []string{}, NOT
// []string{""}, because a single blank entry would be treated as "1 implicit
// ref" and incorrectly block template destruction.
func TestParseStemcellRefs_EmptyStringNeverReturnsOneBlankEntry(t *testing.T) {
	t.Parallel()

	got := ParseStemcellRefs("")
	if len(got) != 0 {
		t.Errorf("ParseStemcellRefs(\"\") returned %d entries %v; want 0 entries",
			len(got), got)
	}
	for _, r := range got {
		if r == "" {
			t.Error("ParseStemcellRefs(\"\") must not return an empty-string entry")
		}
	}
}

// TestFormatStemcellRefs verifies round-trip CSV serialization.
func TestFormatStemcellRefs(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		input []string
		want  string
	}{
		{"nil slice returns empty string", nil, ""},
		{"empty slice returns empty string", []string{}, ""},
		{"single entry", []string{"template:6042"}, "template:6042"},
		{"two entries", []string{"template:6042", "template:6043"}, "template:6042,template:6043"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := FormatStemcellRefs(tc.input)
			if got != tc.want {
				t.Errorf("FormatStemcellRefs(%v) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

// TestParseStemcellRefs_RoundTrip verifies that parse→format→parse is stable.
func TestParseStemcellRefs_RoundTrip(t *testing.T) {
	t.Parallel()

	original := []string{"template:6042", "template:6100", "template:7000"}
	formatted := FormatStemcellRefs(original)
	reparsed := ParseStemcellRefs(formatted)

	if len(reparsed) != len(original) {
		t.Fatalf("round-trip length mismatch: got %d, want %d; formatted=%q", len(reparsed), len(original), formatted)
	}
	for i := range original {
		if reparsed[i] != original[i] {
			t.Errorf("round-trip[%d] = %q, want %q", i, reparsed[i], original[i])
		}
	}
}

// TestBuildStemcellProvenanceNotes_StemcellRefsIncluded verifies that when
// initialCID is non-empty, the stemcell_refs field appears in the JSON output.
func TestBuildStemcellProvenanceNotes_StemcellRefsIncluded(t *testing.T) {
	t.Parallel()

	cp := stemcellCloudProps{Name: "bosh-ubuntu-noble", Version: "1.0"}
	now := time.Date(2026, 6, 12, 0, 0, 0, 0, time.UTC)

	notes, err := buildStemcellProvenanceNotes(cp, "ab12ef34", "", "", now, "template:6042")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var m map[string]any
	if err := json.Unmarshal([]byte(notes), &m); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	refs, ok := m["stemcell_refs"]
	if !ok {
		t.Fatal("stemcell_refs key absent from JSON when initialCID is non-empty")
	}
	if refs != "template:6042" {
		t.Errorf("stemcell_refs = %v, want %q", refs, "template:6042")
	}
}

// TestBuildStemcellProvenanceNotes_StemcellRefsOmittedWhenEmpty verifies that
// when initialCID is empty, the stemcell_refs key is omitted (omitempty).
func TestBuildStemcellProvenanceNotes_StemcellRefsOmittedWhenEmpty(t *testing.T) {
	t.Parallel()

	cp := stemcellCloudProps{Name: "bosh-ubuntu-noble", Version: "1.0"}
	now := time.Date(2026, 6, 12, 0, 0, 0, 0, time.UTC)

	notes, err := buildStemcellProvenanceNotes(cp, "ab12ef34", "", "", now, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var m map[string]any
	if err := json.Unmarshal([]byte(notes), &m); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	if _, ok := m["stemcell_refs"]; ok {
		t.Error("stemcell_refs must be omitted from JSON when initialCID is empty")
	}
}

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

	t.Run("valid JSON with refs parses correctly", func(t *testing.T) {
		t.Parallel()
		input := `{"name":"bosh-ubuntu-noble","version":"1.0","sha8":"ab12ef34","created":"2026-06-12T00:00:00Z","stemcell_refs":"template:6042"}`
		prov, ok := parseStemcellProvenanceFromDescription(input)
		if !ok {
			t.Fatal("expected ok=true for valid JSON")
		}
		if prov.Name != "bosh-ubuntu-noble" {
			t.Errorf("Name = %q, want %q", prov.Name, "bosh-ubuntu-noble")
		}
		if prov.StemcellRefs != "template:6042" {
			t.Errorf("StemcellRefs = %q, want %q", prov.StemcellRefs, "template:6042")
		}
	})

	t.Run("valid JSON without refs field parses with empty StemcellRefs", func(t *testing.T) {
		t.Parallel()
		input := `{"name":"bosh-ubuntu-noble","version":"1.0","sha8":"ab12ef34","created":"2026-06-12T00:00:00Z"}`
		prov, ok := parseStemcellProvenanceFromDescription(input)
		if !ok {
			t.Fatal("expected ok=true for valid JSON")
		}
		if prov.StemcellRefs != "" {
			t.Errorf("StemcellRefs = %q, want empty string", prov.StemcellRefs)
		}
	})
}
