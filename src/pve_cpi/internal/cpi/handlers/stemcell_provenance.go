package handlers

import (
	"encoding/json"
	"time"
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
	Name       string `json:"name"`
	Version    string `json:"version"`
	OSType     string `json:"os_type,omitempty"`
	DiskFormat string `json:"disk_format,omitempty"`
	SHA8       string `json:"sha8"`
	Source     string `json:"source,omitempty"`
	DirectorID string `json:"director_id,omitempty"`
	Created    string `json:"created"`
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
//
// Returns the JSON bytes as a string and any marshaling error. The function
// never returns an empty string on success; Created is always present.
func buildStemcellProvenanceNotes(cp stemcellCloudProps, sha8, source, directorID string, now time.Time) (string, error) {
	p := stemcellProvenance{
		Name:       cp.Name,
		Version:    cp.Version,
		OSType:     cp.OSType,
		DiskFormat: cp.DiskFormat,
		SHA8:       sha8,
		Source:     source,
		DirectorID: directorID,
		Created:    now.UTC().Format(time.RFC3339),
	}
	b, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
