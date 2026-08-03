package handlers

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
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
	Created    string `json:"created"`

	// SHA256 is the full lowercase hex digest of the qcow2 the template was
	// built from. SHA8 (above) remains the tag-key truncation; SHA256 lets a
	// sha-tag dedup hit verify a full-hash match before reuse, mitigating an
	// sha8 (32-bit) collision landing on the wrong template.
	SHA256 string `json:"sha256,omitempty"`
	// Kind mirrors pve.StemcellKind ("light" or "heavy") for the path-identity
	// CID this cache template serves.
	Kind string `json:"kind,omitempty"`
	// CID is the path-identity stemcell CID (":light:..."/":heavy:...") this
	// cache template was built to serve.
	CID string `json:"cid,omitempty"`
	// CreatedBy is the BOSH director UUID that built (froze) this cache
	// template, distinct from DirectorRefs which tracks every director
	// currently holding a live reference.
	CreatedBy string `json:"created_by,omitempty"`
	// DirectorRefs is the SET of BOSH director UUIDs currently referencing
	// this cache template, keyed by jsonrpc.Context.DirectorUUID. An empty
	// set means no director holds this template alive; the last removal
	// triggers destroy (see deregisterStemcellDirectorRef).
	DirectorRefs []string `json:"director_refs,omitempty"`

	DirectorTags map[string]string `json:"director_tags,omitempty"`
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

// buildStemcellProvenanceNotesPath serializes stemcell provenance metadata for
// the path-identity CID design (D1/D3) to JSON for storage in the PVE
// template description field. It records the path CID, its kind, the full
// sha256 digest, the creating director UUID, and seeds DirectorRefs with that
// same creating director as the template's first live reference.
//
// Parameters:
//   - cp: stemcell cloud properties (Name, Version, OSType, DiskFormat).
//   - kind: pve.StemcellKindLight or pve.StemcellKindHeavy.
//   - cid: the path-identity stemcell CID this cache template serves (e.g.
//     ":heavy:local:import/bosh-stemcell-....qcow2").
//   - sha256hex: the full lowercase hex sha256 digest of the qcow2. SHA8 is
//     derived as the first 8 characters; sha256hex shorter than 8 characters
//     (including empty) yields an empty SHA8, matching the
//     BuildStemcellFilename convention for an unknown digest.
//   - source: human-readable origin label (URL, blobstore ref, etc.); scrubbed
//     via log.ScrubMessage exactly as buildStemcellProvenanceNotes does, and
//     omitted from the JSON when empty.
//   - creatingDirectorUUID: the BOSH director UUID that built (froze) this
//     template. Recorded verbatim (including empty) as CreatedBy. The
//     DirectorRefs seed uses directorRefOrSentinel(creatingDirectorUUID)
//     instead — an empty creatingDirectorUUID must seed the same
//     unknownDirectorRef sentinel registerStemcellDirectorRef resolves a
//     UUID-less caller to, or the raw "" entry this seed would otherwise
//     write could never be matched (and removed) by a later deregister call,
//     leaving the template permanently un-destroyable.
//   - now: timestamp recorded as the creation time (stored as RFC3339 UTC).
//   - directorTags: key/value tags supplied via the CPI v3 env argument;
//     recorded as director_tags in the JSON. nil/empty is omitted (omitempty).
//
// Returns the JSON bytes as a string and any marshaling error. The function
// never returns an empty string on success; Created is always present.
func buildStemcellProvenanceNotesPath(
	cp stemcellCloudProps,
	kind pve.StemcellKind,
	cid string,
	sha256hex string,
	source string,
	creatingDirectorUUID string,
	now time.Time,
	directorTags map[string]string,
) (string, error) {
	sha8 := ""
	if len(sha256hex) >= 8 {
		sha8 = strings.ToLower(sha256hex[:8])
	}

	p := stemcellProvenance{
		Name:       cp.Name,
		Version:    cp.Version,
		OSType:     cp.OSType,
		DiskFormat: cp.DiskFormat,
		SHA8:       sha8,
		SHA256:     sha256hex,
		Kind:       string(kind),
		CID:        cid,
		// Scrubbed for the same reason as buildStemcellProvenanceNotes: the
		// notes field persists in /etc/pve/qemu-server/<vmid>.conf, readable
		// by any VM.Audit holder and replicated cluster-wide.
		Source:    log.ScrubMessage(source),
		CreatedBy: creatingDirectorUUID,
		Created:   now.UTC().Format(time.RFC3339),
		// directorRefOrSentinel, not the raw creatingDirectorUUID: see the
		// parameter doc above for why the seed must go through the same
		// sentinel resolution register/deregisterStemcellDirectorRef use.
		DirectorRefs: []string{directorRefOrSentinel(creatingDirectorUUID)},
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
