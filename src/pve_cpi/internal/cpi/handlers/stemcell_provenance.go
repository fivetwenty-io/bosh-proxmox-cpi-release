package handlers

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
)

// Tag constants for stemcell provenance. Defined here to satisfy goconst
// min-occurrences threshold (8) across package handlers where these literals
// appear repeatedly.
const (
	stemcellMarkerTag        = "bosh-stemcell"
	stemcellNameTagPrefix    = "bosh-stemcell-name-"
	stemcellVersionTagPrefix = "bosh-stemcell-version-"
	stemcellSHATagPrefix     = "bosh-stemcell-sha-"
)

// stemcellProvenance is the JSON structure written to a stemcell template's
// description/notes field so operators and automated tooling can inspect
// origin metadata without querying PVE tags.
type stemcellProvenance struct {
	Name         string            `json:"name"`
	Version      string            `json:"version"`
	OSType       string            `json:"os_type,omitempty"`
	DiskFormat   string            `json:"disk_format,omitempty"`
	SHA8         string            `json:"sha8"`
	Source       string            `json:"source,omitempty"`
	DirectorID   string            `json:"director_id,omitempty"`
	Created      string            `json:"created"`
	StemcellRefs string            `json:"stemcell_refs,omitempty"`
	DirectorTags map[string]string `json:"director_tags,omitempty"`
}

// ParseStemcellRefs splits a comma-separated stemcell-refs CSV into a slice of
// CID strings. Empty input returns an empty slice (never returns []string{""}
// for an empty string, which would incorrectly indicate one implicit reference).
// Whitespace around individual entries is trimmed and empty tokens are dropped.
func ParseStemcellRefs(s string) []string {
	if strings.TrimSpace(s) == "" {
		return []string{}
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// FormatStemcellRefs joins a slice of CID strings into the comma-separated CSV
// format stored in stemcellProvenance.StemcellRefs. An empty slice returns "".
func FormatStemcellRefs(refs []string) string {
	return strings.Join(refs, ",")
}

// buildStemcellProvenanceTags returns the ordered list of PVE tags that record
// stemcell identity on a template VM. Empty or all-whitespace values are
// omitted after sanitization so no blank tag is emitted.
//
// Returned entries (in order, each conditional):
//
//  1. stemcellMarkerTag — always present; marks the VM as a BOSH stemcell.
//  2. stemcellNameTagPrefix + sanitized cp.Name — omitted when sanitized form is empty.
//  3. stemcellVersionTagPrefix + sanitized cp.Version — omitted when sanitized form is empty.
//  4. "director--" + sanitized directorID — omitted when directorID is empty or sanitizes to "".
func buildStemcellProvenanceTags(cp stemcellCloudProps, directorID string) []string {
	tags := []string{stemcellMarkerTag}

	if sn := sanitizeTagValue(cp.Name); sn != "" {
		tags = append(tags, stemcellNameTagPrefix+sn)
	}

	if sv := sanitizeTagValue(cp.Version); sv != "" {
		tags = append(tags, stemcellVersionTagPrefix+sv)
	}

	if directorID != "" {
		if sd := sanitizeTagValue(directorID); sd != "" {
			tags = append(tags, "director--"+sd)
		}
	}

	return tags
}

// buildStemcellProvenanceNotes serializes stemcell provenance metadata to JSON
// for storage in the PVE template description field.
//
// Parameters:
//   - cp: stemcell cloud properties (Name, Version, OSType, DiskFormat).
//   - sha8: first 8 hex characters of the stemcell image digest; may be empty.
//   - source: human-readable origin label (URL, blobstore ref, etc.); omitted when empty.
//   - directorID: BOSH director UUID; omitted when empty.
//   - now: timestamp recorded as the creation time (stored as RFC3339 UTC).
//   - initialCID: the BOSH stemcell CID assigned to this template (e.g. "template:6042").
//     When non-empty it is recorded as the first entry in stemcell_refs so that
//     delete_stemcell can track reference counts. When empty, stemcell_refs is
//     omitted (omitempty).
//   - directorTags: key/value tags supplied via the CPI v3 env argument; recorded
//     as director_tags in the JSON. nil/empty is omitted (omitempty). When empty,
//     notes output is byte-identical to the pre-v3 format.
//
// Returns the JSON bytes as a string and any marshaling error. The function
// never returns an empty string on success; Created is always present.
func buildStemcellProvenanceNotes(cp stemcellCloudProps, sha8, source, directorID string, now time.Time, initialCID string, directorTags map[string]string) (string, error) {
	p := stemcellProvenance{
		Name:       cp.Name,
		Version:    cp.Version,
		OSType:     cp.OSType,
		DiskFormat: cp.DiskFormat,
		SHA8:       sha8,
		// The source URL is scrubbed because PVE persists the notes field in
		// /etc/pve/qemu-server/<vmid>.conf — readable by any VM.Audit holder,
		// replicated cluster-wide, captured in config backups, and outliving
		// log rotation. A presigned or userinfo-bearing image_url must not
		// land there verbatim; host, path, bucket, and key survive scrubbing,
		// which is all the provenance value needs.
		Source:     log.ScrubMessage(source),
		DirectorID: directorID,
		Created:    now.UTC().Format(time.RFC3339),
	}
	if initialCID != "" {
		p.StemcellRefs = initialCID
	}
	if len(directorTags) > 0 {
		p.DirectorTags = directorTags
	}
	b, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// parseStemcellProvenanceFromDescription attempts to unmarshal a stemcellProvenance
// from the raw description string. The description may be empty (no notes written),
// a JSON object, or any other text. On parse failure (including non-JSON text)
// it returns a zero-value stemcellProvenance and no error — callers that need to
// distinguish "no data" from "parse error" should check the returned bool.
//
// Returns (provenance, ok):
//   - ok=true: JSON parsed successfully; provenance fields reflect stored values.
//   - ok=false: description was empty or not valid JSON; provenance is zero-value.
func parseStemcellProvenanceFromDescription(description string) (stemcellProvenance, bool) {
	if strings.TrimSpace(description) == "" {
		return stemcellProvenance{}, false
	}
	var p stemcellProvenance
	if err := json.Unmarshal([]byte(description), &p); err != nil {
		return stemcellProvenance{}, false
	}
	return p, true
}
