package handlers

import (
	"context"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	sdkclusterstorage "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/clusterstorage"
)

// relocationStorage serves the cluster storage definitions the relocation
// check compares backings with.
type relocationStorage struct {
	sdkclusterstorage.Service
	rows *backingListerFromEntries
}

func (s relocationStorage) ListStorage(ctx context.Context, p *sdkclusterstorage.ListStorageParams) (*sdkclusterstorage.ListStorageResponse, error) {
	return s.rows.ListStorage(ctx, p)
}

type relocationClient struct {
	pve.Client
	storage relocationStorage
}

func (c relocationClient) ClusterStorage() sdkclusterstorage.Service { return c.storage }

// TestManagedDiskLifecycleClient_ObserveDiskRelocated pins how the managed
// lifecycle follows a disk that another operation moved while this one
// waited. It takes the new name only for its own disk, only from the name it
// holds, and only on the same physical backing, the way it follows a move it
// observed itself.
func TestManagedDiskLifecycleClient_ObserveDiskRelocated(t *testing.T) {
	t.Parallel()
	const (
		stableID = "bpd-00112233aabbccdd"
		from     = "data:vm-700-disk-1"
		to       = "data:vm-90001-disk-0"
	)
	deps := Deps{PVE: relocationClient{storage: relocationStorage{rows: &backingListerFromEntries{rows: []map[string]any{
		{"storage": "data", "type": "nfs", "shared": 1, "server": "10.0.0.5", "export": "/tank/a"},
		{"storage": "other", "type": "nfs", "shared": 1, "server": "10.0.0.6", "export": "/tank/b"},
	}}}}}
	backing, err := managedDiskActualBacking(context.Background(), deps, "data")
	if err != nil {
		t.Fatalf("backing: %v", err)
	}
	client := func() (*managedDiskLifecycleClient, *managedDiskLifecycle) {
		m := &managedDiskLifecycle{
			deps:            deps,
			external:        true,
			externalBacking: backing,
			disk:            resolvedDisk{volid: from, stableID: stableID},
		}
		return &managedDiskLifecycleClient{lifecycle: m}, m
	}
	var _ pve.DiskRelocationObserver = &managedDiskLifecycleClient{}

	t.Run("same backing", func(t *testing.T) {
		t.Parallel()
		c, m := client()
		if err := c.ObserveDiskRelocated(context.Background(), stableID, from, to); err != nil {
			t.Fatalf("ObserveDiskRelocated: %v", err)
		}
		if m.disk.volid != to {
			t.Fatalf("volid = %q, want %q", m.disk.volid, to)
		}
	})

	for name, tc := range map[string]struct{ stableID, from, to string }{
		"another disk":      {"bpd-ffffffffffffffff", from, to},
		"another start":     {stableID, "data:vm-701-disk-1", to},
		"another backing":   {stableID, from, "other:vm-90001-disk-0"},
		"no storage prefix": {stableID, from, "vm-90001-disk-0"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c, m := client()
			if err := c.ObserveDiskRelocated(context.Background(), tc.stableID, tc.from, tc.to); err == nil {
				t.Fatal("the relocation was accepted")
			}
			if m.disk.volid != from {
				t.Fatalf("volid = %q after a refused relocation, want it unchanged", m.disk.volid)
			}
		})
	}
}
