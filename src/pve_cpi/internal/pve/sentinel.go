// Sentinel codec shared by every pve-package provenance store that persists
// structured metadata inside a VM's PVE description field using the wire
// format <!--BOSH:{...}-->. Multiple independent top-level JSON keys coexist
// in one sentinel block (e.g. bosh_parked_disks, bosh_attached_disks); each
// caller extracts and deletes its own key and passes the remainder through
// as raw so unrelated keys survive a read-modify-write cycle untouched.
package pve

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// sentinelPattern matches <!--BOSH:{...}--> in a VM description. ParseSentinel
// and RenderSentinel are exported because the handlers package's description
// writers (set_disk_metadata, set_vm_metadata) share the same wire format and
// must pass foreign top-level keys through untouched — every sentinel block on
// a VM description coexists via distinct top-level JSON keys.
var sentinelPattern = regexp.MustCompile(`<!--BOSH:(.*?)-->`)

// ParseSentinel extracts the description text outside the sentinel (nonBOSH)
// and the full top-level key map (raw) from desc. Corrupted sentinel JSON ->
// empty raw map (sentinel rebuilt from scratch on next write; nonBOSH text is
// still preserved since it is captured before the JSON decode is attempted).
func ParseSentinel(desc string) (nonBOSH string, raw map[string]json.RawMessage) {
	nonBOSH = desc
	raw = make(map[string]json.RawMessage)

	m := sentinelPattern.FindStringSubmatchIndex(desc)
	if m == nil {
		return
	}
	jsonStr := desc[m[2]:m[3]]
	nonBOSH = strings.TrimSpace(desc[:m[0]])

	if err := json.Unmarshal([]byte(jsonStr), &raw); err != nil {
		raw = make(map[string]json.RawMessage)
		return
	}
	return
}

// RenderSentinel builds the full description string from the nonBOSH prefix
// and the merged top-level key map. Returns nonBOSH unchanged (no sentinel
// block emitted) when raw is empty — avoids writing an empty <!--BOSH:{}-->.
func RenderSentinel(nonBOSH string, raw map[string]json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return nonBOSH, nil
	}
	sentinel, err := json.Marshal(raw)
	if err != nil {
		return "", err
	}
	newDesc := fmt.Sprintf("<!--BOSH:%s-->", string(sentinel))
	if nonBOSH != "" {
		newDesc = nonBOSH + "\n" + newDesc
	}
	return newDesc, nil
}

// DescriptionFromConfig extracts the "description" field from a VM config map
// as returned by QEMU().Config, defaulting to "" when the field is absent or
// not a string (PVE always returns a string when present; the type guard only
// protects against a malformed or mocked response).
func DescriptionFromConfig(cfg map[string]any) string {
	if v, ok := cfg["description"]; ok {
		if s, ok2 := v.(string); ok2 {
			return s
		}
	}
	return ""
}
