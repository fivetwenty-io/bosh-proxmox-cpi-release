package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// templateProvenance is a minimal, read-only mirror of the JSON schema the
// CPI writes to a stemcell-cache template's description field (see
// internal/cpi/handlers/stemcell_provenance.go's stemcellProvenance). It is
// reimplemented here — rather than importing the handlers package, which is
// internal to the CPI's handler layer and mid-churn across other in-flight
// work — because pve-cid only ever reads this JSON, never constructs or
// mutates it. Only the fields pve-cid's locate/stemcells subcommands surface
// are included; unknown fields are ignored by json.Unmarshal.
type templateProvenance struct {
	Name         string   `json:"name"`
	Version      string   `json:"version"`
	SHA8         string   `json:"sha8"`
	SHA256       string   `json:"sha256,omitempty"`
	Kind         string   `json:"kind,omitempty"`
	CID          string   `json:"cid,omitempty"`
	CreatedBy    string   `json:"created_by,omitempty"`
	Created      string   `json:"created"`
	DirectorRefs []string `json:"director_refs,omitempty"`
}

// parseTemplateProvenance best-effort decodes description as JSON.
//
// Returns (provenance, ok):
//   - ok=true: description was non-empty and parsed as JSON.
//   - ok=false: description was empty/whitespace-only or not valid JSON;
//     provenance is the zero value. Callers treat this as "no provenance
//     data available" rather than an error — a template predating the
//     provenance feature, or one an operator hand-edited, still has a valid
//     VMID/node/name worth reporting.
func parseTemplateProvenance(description string) (templateProvenance, bool) {
	if strings.TrimSpace(description) == "" {
		return templateProvenance{}, false
	}
	var p templateProvenance
	if err := json.Unmarshal([]byte(description), &p); err != nil {
		return templateProvenance{}, false
	}
	return p, true
}

// Stemcell provenance tag constants, mirrored from
// internal/cpi/handlers/stemcell_provenance.go (also unexported there, and
// package-internal to handlers — reimplemented here for the same reason as
// templateProvenance above). Pure string-matching constants, not business
// logic: the risk of drift is a stale tag prefix, caught immediately by
// pve-cid's own tests failing to match live PVE tags.
const (
	stemcellMarkerTag        = "bosh-stemcell"
	stemcellNameTagPrefix    = "bosh-stemcell-name-"
	stemcellVersionTagPrefix = "bosh-stemcell-version-"
	stemcellSHATagPrefix     = "bosh-stemcell-sha-"
)

// pveIntBool decodes a PVE boolean, which the API serialises as 1/0 rather
// than true/false. Mirrors internal/pve's own pveBool (unexported there, the
// same reason splitPVETags is duplicated below), including its rejection of
// an unrecognised value: the callers here skip a row whose JSON fails to
// decode, so an unexpected shape drops that one guest from the inventory
// rather than being silently reported as "not a template".
type pveIntBool bool

func (b *pveIntBool) UnmarshalJSON(data []byte) error {
	switch s := strings.Trim(strings.TrimSpace(string(data)), `"`); s {
	case "1", "true":
		*b = true
	case "0", "false", "", "null":
		*b = false
	default:
		return fmt.Errorf("pveIntBool: cannot decode %q as a PVE boolean", s)
	}
	return nil
}

// splitPVETags splits a PVE tags string (semicolon-delimited, comma also
// accepted) into individual tag tokens, trimming whitespace and dropping
// empty tokens. Mirrors internal/pve's own splitPVETags (unexported there).
func splitPVETags(tags string) []string {
	if tags == "" {
		return nil
	}
	normalized := strings.ReplaceAll(tags, ",", ";")
	parts := strings.Split(normalized, ";")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// hasTagToken reports whether want appears as an exact token in tokens
// (whole-token comparison, not substring — see splitPVETags).
func hasTagToken(tokens []string, want string) bool {
	for _, t := range tokens {
		if t == want {
			return true
		}
	}
	return false
}

// tagValue returns the suffix of the first token in tokens that begins with
// prefix, or "" when no token matches.
func tagValue(tokens []string, prefix string) string {
	for _, t := range tokens {
		if strings.HasPrefix(t, prefix) {
			return strings.TrimPrefix(t, prefix)
		}
	}
	return ""
}
