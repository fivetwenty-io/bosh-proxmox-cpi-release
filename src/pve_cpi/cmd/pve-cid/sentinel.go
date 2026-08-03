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
func readAttachedDiskCID(description, bareVolid string) (string, bool) {
	_, raw := pve.ParseSentinel(description)
	rawDisks, ok := raw[attachedDisksSentinelKey]
	if !ok {
		return "", false
	}
	var disks map[string]string
	if err := json.Unmarshal(rawDisks, &disks); err != nil {
		return "", false
	}
	cid, ok := disks[bareVolid]
	return cid, ok
}

// readParkedDiskEntry extracts the parker provenance entry recorded for
// bareVolid in a VM description's bosh_parked_disks sentinel.
//
// Returns (zero, false) when description carries no bosh_parked_disks block,
// the block is malformed, or bareVolid has no entry in it.
func readParkedDiskEntry(description, bareVolid string) (parkedDiskEntry, bool) {
	_, raw := pve.ParseSentinel(description)
	rawDisks, ok := raw[parkedDisksSentinelKey]
	if !ok {
		return parkedDiskEntry{}, false
	}
	var disks map[string]parkedDiskEntry
	if err := json.Unmarshal(rawDisks, &disks); err != nil {
		return parkedDiskEntry{}, false
	}
	entry, ok := disks[bareVolid]
	return entry, ok
}
