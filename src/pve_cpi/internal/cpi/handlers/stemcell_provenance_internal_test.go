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

	notes, err := buildStemcellProvenanceNotes(cp, "ab12ef34", "https://example.com/stemcell.tgz", "director-xyz", now)
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

	notes, err := buildStemcellProvenanceNotes(cp, "", "", "", now)
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

	notes, err := buildStemcellProvenanceNotes(cp, "", "", "", now)
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
