package main

import (
	"encoding/json"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
)

// buildSentinelDescription is the test-local mirror of how the CPI itself
// writes a VM description sentinel (see internal/pve/sentinel.go's
// RenderSentinel), used to build fixtures for readAttachedDiskCID and
// readParkedDiskEntry without depending on unexported CPI codec entry
// points.
func buildSentinelDescription(t *testing.T, keys map[string]any) string {
	t.Helper()
	raw := make(map[string]json.RawMessage, len(keys))
	for k, v := range keys {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal %q: %v", k, err)
		}
		raw[k] = b
	}
	desc, err := pve.RenderSentinel("", raw)
	if err != nil {
		t.Fatalf("RenderSentinel: %v", err)
	}
	return desc
}

func TestReadAttachedDiskCID_Found(t *testing.T) {
	desc := buildSentinelDescription(t, map[string]any{
		attachedDisksSentinelKey: map[string]string{
			"local-lvm:vm-100-disk-0": "pvd-abc123",
		},
	})
	cid, ok := readAttachedDiskCID(desc, "local-lvm:vm-100-disk-0")
	if !ok {
		t.Fatal("expected ok=true")
	}
	if cid != "pvd-abc123" {
		t.Errorf("cid = %q, want %q", cid, "pvd-abc123")
	}
}

func TestReadAttachedDiskCID_NotFound(t *testing.T) {
	desc := buildSentinelDescription(t, map[string]any{
		attachedDisksSentinelKey: map[string]string{
			"local-lvm:vm-100-disk-0": "pvd-abc123",
		},
	})
	if _, ok := readAttachedDiskCID(desc, "local-lvm:vm-999-disk-0"); ok {
		t.Error("expected ok=false for an unmatched volid")
	}
}

func TestReadAttachedDiskCID_NoSentinel(t *testing.T) {
	if _, ok := readAttachedDiskCID("plain description, no sentinel", "x:y"); ok {
		t.Error("expected ok=false when no sentinel block is present")
	}
}

func TestReadParkedDiskEntry_Found(t *testing.T) {
	desc := buildSentinelDescription(t, map[string]any{
		parkedDisksSentinelKey: map[string]parkedDiskEntry{
			"local-lvm:vm-100-disk-0": {
				DiskCID:     "pvd-abc123",
				SourceVMCID: "42",
				ParkedAt:    "2026-08-01T00:00:00Z",
				Node:        "pve1",
				DirectorID:  "dir-1",
			},
		},
	})
	entry, ok := readParkedDiskEntry(desc, "local-lvm:vm-100-disk-0", "")
	if !ok {
		t.Fatal("expected ok=true")
	}
	if entry.ParkedAt != "2026-08-01T00:00:00Z" || entry.SourceVMCID != "42" || entry.Node != "pve1" {
		t.Errorf("entry = %+v", entry)
	}
}

func TestReadParkedDiskEntry_NotFound(t *testing.T) {
	desc := buildSentinelDescription(t, map[string]any{
		parkedDisksSentinelKey: map[string]parkedDiskEntry{
			"local-lvm:vm-100-disk-0": {ParkedAt: "2026-08-01T00:00:00Z", Node: "pve1"},
		},
	})
	if _, ok := readParkedDiskEntry(desc, "local-lvm:vm-999-disk-0", ""); ok {
		t.Error("expected ok=false for an unmatched volid")
	}
}

func TestSentinels_CoexistOnOneDescription(t *testing.T) {
	desc := buildSentinelDescription(t, map[string]any{
		attachedDisksSentinelKey: map[string]string{"a:v1": "pvd-1"},
		parkedDisksSentinelKey:   map[string]parkedDiskEntry{"b:v2": {ParkedAt: "t", Node: "n"}},
	})
	if _, ok := readAttachedDiskCID(desc, "a:v1"); !ok {
		t.Error("expected attached sentinel to be found alongside parked sentinel")
	}
	if _, ok := readParkedDiskEntry(desc, "b:v2", ""); !ok {
		t.Error("expected parked sentinel to be found alongside attached sentinel")
	}
}
