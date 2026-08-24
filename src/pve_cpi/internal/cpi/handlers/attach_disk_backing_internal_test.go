// Package handlers -- internal tests proving attach_disk's co-location
// decision (attachDiskResolveNode) behaves correctly for the "two IDs, one
// export" backing-identity fixture: attach_disk.go itself never compares two
// storage IDs against each other (a disk's storage pool is recovered
// verbatim from its own volid prefix, and the co-location check compares
// NODE names, not storage IDs), so there is no direct backing-comparison
// bug to fix here. These tests instead wire the REAL production
// classification path (pve.NewBackendResolver over a pve.NewStorageInfoCache,
// exactly as main.go wires it) to prove: two storage IDs sharing one
// physical NFS export are BOTH independently classified shared and produce
// identical (no co-location error) behavior regardless of which name holds
// the disk — a spurious co-location failure cannot arise from the naming
// alone.
package handlers

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	sdkclusterstorage "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/clusterstorage"
)

// backingListerFromEntries adapts a slice of row maps to pve.StorageLister.
type backingListerFromEntries struct {
	rows []map[string]any
}

func (l *backingListerFromEntries) ListStorage(_ context.Context, _ *sdkclusterstorage.ListStorageParams) (*sdkclusterstorage.ListStorageResponse, error) {
	resp := make(sdkclusterstorage.ListStorageResponse, 0, len(l.rows))
	for _, row := range l.rows {
		raw, err := json.Marshal(row)
		if err != nil {
			return nil, err
		}
		resp = append(resp, json.RawMessage(raw))
	}
	return &resp, nil
}

// TestAttachDiskResolveNode_TwoIDsOneExport_BothClassifyShared drives
// attachDiskResolveNode directly (no VM client wired: deps.PVE stays nil,
// which pve.FindVMNodeViaCluster treats as "VM not found in cluster
// resources" -- a benign fallback for the shared-backend branch, not a
// failure) for two storage IDs that share one physical NFS export. Both must
// resolve to the resolver's default node with no error: naming alone (two
// IDs vs one export) must never produce a spurious co-location failure.
func TestAttachDiskResolveNode_TwoIDsOneExport_BothClassifyShared(t *testing.T) {
	t.Parallel()
	lister := &backingListerFromEntries{rows: []map[string]any{
		{"storage": "nfs-a", "type": "nfs", "shared": 1, "server": "10.0.0.5", "export": "/tank/proxmox"},
		{"storage": "nfs-b", "type": "nfs", "shared": 1, "server": "10.0.0.5", "export": "/tank/proxmox"},
	}}
	cache := pve.NewStorageInfoCache(lister, time.Minute)
	resolver := pve.NewBackendResolver(nil, cache, "pve")

	for _, storageID := range []string{"nfs-a", "nfs-b"} {
		storageID := storageID
		t.Run(storageID, func(t *testing.T) {
			t.Parallel()
			deps := Deps{Resolver: resolver}
			diskCID := storageID + ":vm-100-disk-0"
			node, vmid, err := attachDiskResolveNode(context.Background(), deps, "100", diskCID, "")
			if err != nil {
				t.Fatalf("attachDiskResolveNode(%s): unexpected error: %v", storageID, err)
			}
			if vmid != 100 {
				t.Errorf("vmid = %d, want 100", vmid)
			}
			if node != "pve" {
				t.Errorf("node = %q, want %q (shared backend default)", node, "pve")
			}
		})
	}
}

// TestAttachDiskResolveNode_TwoIDsOneExport_BackingKeyMatches is a narrower
// unit check confirming the SAME StorageInfoCache (populated by the fixture
// above) reports SameBacking(nfs-a, nfs-b) == true, so the classification
// attach_disk consumes really is backing-aware end to end -- not just
// independently "shared" by type coincidence.
func TestAttachDiskResolveNode_TwoIDsOneExport_BackingKeyMatches(t *testing.T) {
	t.Parallel()
	lister := &backingListerFromEntries{rows: []map[string]any{
		{"storage": "nfs-a", "type": "nfs", "shared": 1, "server": "10.0.0.5", "export": "/tank/proxmox"},
		{"storage": "nfs-b", "type": "nfs", "shared": 1, "server": "10.0.0.5", "export": "/tank/proxmox"},
		{"storage": "nfs-c", "type": "nfs", "shared": 1, "server": "10.0.0.9", "export": "/tank/other"},
	}}
	cache := pve.NewStorageInfoCache(lister, time.Minute)

	infoA, err := cache.Get(context.Background(), "nfs-a")
	if err != nil {
		t.Fatalf("Get nfs-a: %v", err)
	}
	infoB, err := cache.Get(context.Background(), "nfs-b")
	if err != nil {
		t.Fatalf("Get nfs-b: %v", err)
	}
	infoC, err := cache.Get(context.Background(), "nfs-c")
	if err != nil {
		t.Fatalf("Get nfs-c: %v", err)
	}
	if !pve.SameBacking(infoA, infoB) {
		t.Errorf("nfs-a and nfs-b share one export: SameBacking must be true")
	}
	if pve.SameBacking(infoA, infoC) {
		t.Errorf("nfs-a and nfs-c are distinct exports: SameBacking must be false")
	}
}
