// Package handlers -- internal tests proving deriveDiskFaultConstraints
// (create_vm_placement.go) behaves correctly for the "two IDs, one export"
// backing-identity fixture. deriveDiskFaultConstraints itself never compares
// two storage IDs against each other: it classifies each disk's pool
// independently via backend.Kind() and then compares NODE names (for local
// disks) or AZ labels (for shared disks) -- never storage IDs -- so there is
// no direct backing-comparison bug to fix here. These tests wire the REAL
// production classification path (pve.NewBackendResolver over a
// pve.NewStorageInfoCache, exactly as main.go wires it) to prove two shared
// disks on different-but-backing-equivalent storage IDs merge their AZ
// constraints identically to two disks on the same ID, and that genuinely
// distinct backings remain unaffected.
package handlers

import (
	"context"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
)

// buildFaultDomainDepsRealCache builds Deps for deriveDiskFaultConstraints
// backed by the REAL pve.NewBackendResolver/pve.NewStorageInfoCache pair
// (main.go's production wiring), fed by rows -- unlike buildFaultDomainDeps
// (create_vm_internal_test.go), which uses a name-matching static resolver
// that cannot exercise BackingKey-based classification at all.
func buildFaultDomainDepsRealCache(rows []map[string]any) Deps {
	cfg := &config.CPIConfig{
		Host:          "pve.test",
		Node:          "",
		VMStorage:     "local-lvm",
		NetworkBridge: "vmbr0",
	}
	lister := &backingListerFromEntries{rows: rows}
	cache := pve.NewStorageInfoCache(lister, time.Minute)
	return Deps{
		Config:   cfg,
		Logger:   log.NewNopLogger(),
		Resolver: pve.NewBackendResolver(nil, cache, ""),
	}
}

// TestDeriveDiskFaultConstraints_TwoIDsOneExport_AZConstraintsMerge verifies
// that two shared-backend disks on DIFFERENT storage IDs that share one
// physical NFS export both classify Shared (via the real cache) and
// contribute their AZ labels identically to the same-ID case: no spurious
// classification divergence from the naming alone.
func TestDeriveDiskFaultConstraints_TwoIDsOneExport_AZConstraintsMerge(t *testing.T) {
	t.Parallel()
	deps := buildFaultDomainDepsRealCache([]map[string]any{
		{"storage": "nfs-a", "type": "nfs", "shared": 1, "server": "10.0.0.5", "export": "/tank/proxmox"},
		{"storage": "nfs-b", "type": "nfs", "shared": 1, "server": "10.0.0.5", "export": "/tank/proxmox"},
	})
	cidA := encodeSharedCID(t, "nfs-a", "zone-a")
	cidB := encodeSharedCID(t, "nfs-b", "zone-a")

	c, err := deriveDiskFaultConstraints(context.Background(), deps, []string{cidA, cidB})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.requiredLocalNode != "" {
		t.Errorf("two shared disks: requiredLocalNode = %q; want empty", c.requiredLocalNode)
	}
	if _, ok := c.requiredAZs["zone-a"]; !ok || len(c.requiredAZs) != 1 {
		t.Errorf("requiredAZs = %v; want exactly {zone-a}", c.requiredAZs)
	}
}

// TestDeriveDiskFaultConstraints_TwoIDsOneExport_DistinctBacking_StillWorks
// is the distinct-backing counterpart: two shared disks on storage IDs with
// genuinely different physical exports still classify Shared independently
// (nfs is shared by type regardless of backing) and merge distinct AZ labels
// exactly as before backing-identity existed.
func TestDeriveDiskFaultConstraints_TwoIDsOneExport_DistinctBacking_StillWorks(t *testing.T) {
	t.Parallel()
	deps := buildFaultDomainDepsRealCache([]map[string]any{
		{"storage": "nfs-a", "type": "nfs", "shared": 1, "server": "10.0.0.5", "export": "/tank/proxmox"},
		{"storage": "nfs-c", "type": "nfs", "shared": 1, "server": "10.0.0.9", "export": "/tank/other"},
	})
	cidA := encodeSharedCID(t, "nfs-a", "zone-a")
	cidC := encodeSharedCID(t, "nfs-c", "zone-b")

	c, err := deriveDiskFaultConstraints(context.Background(), deps, []string{cidA, cidC})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(c.requiredAZs) != 2 {
		t.Fatalf("requiredAZs = %v; want {zone-a, zone-b}", c.requiredAZs)
	}
	for _, az := range []string{"zone-a", "zone-b"} {
		if _, ok := c.requiredAZs[az]; !ok {
			t.Errorf("requiredAZs missing %q: got %v", az, c.requiredAZs)
		}
	}
}
