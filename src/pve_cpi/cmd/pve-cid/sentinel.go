package main

import (
	"encoding/json"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
)

// parkedDiskEntry is a minimal, read-only mirror of internal/pve's private
// parkerProvEntry (parker.go), reimplemented here for the same reason as
// templateProvenance (provenance.go): pve-cid only ever reads this JSON.
type parkedDiskEntry struct {
	DiskCID     string `json:"disk_cid"`
	SourceVMCID string `json:"source_vm_cid,omitempty"`
	ParkedAt    string `json:"parked_at"`
	Node        string `json:"node"`
	DirectorID  string `json:"director_id,omitempty"`
	// Volid and Slot appear on stable-ID entries only: the volid the parker
	// holds (or is receiving) the volume under, and the parker bus slot a
	// transfer targets. Legacy entries are keyed by the bare volid instead.
	Volid string `json:"volid,omitempty"`
	Slot  string `json:"slot,omitempty"`
}

// Sentinel top-level JSON keys, mirrored from internal/pve/attached_disks.go
// and internal/pve/parker.go.
const (
	attachedDisksSentinelKey = "bosh_attached_disks"
	parkedDisksSentinelKey   = "bosh_parked_disks"
)

// readAttachedDiskCID extracts the Director-supplied disk_cid recorded for
// bareVolid in a VM description's bosh_attached_disks sentinel.
//
// Returns ("", false) when description carries no bosh_attached_disks block,
// the block is malformed, or bareVolid has no entry in it — pve-cid treats
// all three as "no sentinel data", matching the CPI's own best-effort
// decode contract (GetAttachedDiskCIDs in internal/pve/attached_disks.go).
// Stable-ID disks are recorded under their bpd- token instead of the bare
// volid; callers pass both candidate keys and the first hit wins.
func readAttachedDiskCID(description string, keys ...string) (string, bool) {
	_, raw := pve.ParseSentinel(description)
	rawDisks, ok := raw[attachedDisksSentinelKey]
	if !ok {
		return "", false
	}
	var disks map[string]string
	if err := json.Unmarshal(rawDisks, &disks); err != nil {
		return "", false
	}
	for _, key := range keys {
		if key == "" {
			continue
		}
		if cid, hit := disks[key]; hit {
			return cid, true
		}
	}
	return "", false
}

// readParkedDiskEntry extracts the parker provenance entry recorded for
// bareVolid (legacy keying) or stableID (bpd- keying) in a VM description's
// bosh_parked_disks sentinel. A stable-ID entry whose Volid field names
// bareVolid also matches, mirroring the CPI's own dual-match removal.
//
// Returns (zero, false) when description carries no bosh_parked_disks block,
// the block is malformed, or no entry matches.
func readParkedDiskEntry(description, bareVolid, stableID string) (parkedDiskEntry, bool) {
	_, raw := pve.ParseSentinel(description)
	rawDisks, ok := raw[parkedDisksSentinelKey]
	if !ok {
		return parkedDiskEntry{}, false
	}
	var disks map[string]parkedDiskEntry
	if err := json.Unmarshal(rawDisks, &disks); err != nil {
		return parkedDiskEntry{}, false
	}
	if stableID != "" {
		if entry, hit := disks[stableID]; hit {
			return entry, true
		}
	}
	if entry, hit := disks[bareVolid]; hit {
		return entry, true
	}
	for _, entry := range disks {
		if entry.Volid != "" && entry.Volid == bareVolid {
			return entry, true
		}
	}
	return parkedDiskEntry{}, false
}
