// Package pve provides task-await and VMID-allocation helpers used by CPI action handlers.
package pve

import (
	"encoding/json"
	"fmt"
)

// UPIDFromRaw extracts a PVE task UPID from a json.RawMessage as returned by
// nodes.Service status calls (e.g. CreateQemuStatusReboot). PVE encodes the
// task identifier either as a bare JSON string or as an object with an
// "upid" field. Returns "" (no error) when raw is empty or carries no UPID.
func UPIDFromRaw(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}

	// Most PVE status endpoints return the UPID as a bare JSON string.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}

	// Some endpoints (and certain PVE versions) return an object containing
	// the UPID at key "upid". Mirror the postUPID handling in the qemu SDK.
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err == nil {
		if v, ok := m["upid"].(string); ok {
			return v, nil
		}
		// Valid object but no "upid" field — not an error, just no UPID.
		return "", nil
	}

	return "", fmt.Errorf("pve: cannot parse UPID from response: %s", string(raw))
}
