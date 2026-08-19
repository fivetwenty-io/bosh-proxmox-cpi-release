package handlers

import (
	"context"
	"errors"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
	sdkcloudinit "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cloudinit"
	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdkclusterstorage "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/clusterstorage"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	sdkqemu "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
	sdkstorage "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/storage"
	sdktasks "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/tasks"
)

// ---------------------------------------------------------------------------
// Local minimal fakes for foreign-disk-detach tests.
// Only Config and DetachDisk are exercised; all other QEMU methods panic.
// ---------------------------------------------------------------------------

type fdQEMU struct {
	sdkqemu.Service
	configCalls int
	configs     []map[string]any // configCalls-th call returns configs[i]
	detachFn    func(ctx context.Context, node string, vmid int, slot string) error
}

func (q *fdQEMU) Config(_ context.Context, _ string, _ int) (map[string]any, error) {
	if q.configCalls < len(q.configs) {
		cfg := q.configs[q.configCalls]
		q.configCalls++
		return cfg, nil
	}
	q.configCalls++
	return map[string]any{}, nil
}

func (q *fdQEMU) DetachDisk(ctx context.Context, node string, vmid int, slot string) error {
	if q.detachFn != nil {
		return q.detachFn(ctx, node, vmid, slot)
	}
	panic("fdQEMU.DetachDisk: not configured")
}

// fdNodes provides the minimum nodes.Service surface for detachForeignActiveDisks,
// which calls nothing on nodes. All methods panic.
type fdNodes struct {
	sdknodes.Service
}

type fdClient struct {
	qemu sdkqemu.Service
}

func (c *fdClient) QEMU() sdkqemu.Service                     { return c.qemu }
func (c *fdClient) Storage() sdkstorage.Service               { return nil }
func (c *fdClient) CloudInit() sdkcloudinit.Service           { return nil }
func (c *fdClient) Tasks() sdktasks.Service                   { return nil }
func (c *fdClient) Nodes() sdknodes.Service                   { return &fdNodes{} }
func (c *fdClient) Cluster() sdkcluster.Service               { return nil }
func (c *fdClient) ClusterStorage() sdkclusterstorage.Service { return nil }
func (c *fdClient) Pools() pve.PoolService                    { return nil }

func fdDeps(qemu sdkqemu.Service) Deps {
	return Deps{
		Config: &config.CPIConfig{
			Node:        "pve-node1",
			DiskStorage: "zfs-1",
		},
		PVE:    &fdClient{qemu: qemu},
		Logger: log.NewNopLogger(),
	}
}

// ---------------------------------------------------------------------------
// TestDetachForeignActiveDisks_ForeignDisk: scsi1 holds a foreign disk.
// Expects DetachDisk("scsi1") then confirms the re-read config has no foreign
// disks, allowing the caller to proceed to destroy.
// ---------------------------------------------------------------------------

func TestDetachForeignActiveDisks_ForeignDisk(t *testing.T) {
	t.Parallel()

	// VM 100 owns virtio0; scsi1 carries vmid 777 (foreign).
	initialCfg := map[string]any{
		"virtio0": "zfs-1:vm-100-disk-0",
		"scsi1":   "zfs-1:vm-777-disk-0,size=128G",
	}
	// A fully successful DetachDisk demotes scsi1 to unusedN AND sweeps it, so
	// the re-read config carries no reference to the foreign volume at all.
	postDetachCfg := map[string]any{
		"virtio0": "zfs-1:vm-100-disk-0",
	}

	var detachSlots []string
	q := &fdQEMU{
		configs: []map[string]any{initialCfg, postDetachCfg},
		detachFn: func(_ context.Context, _ string, _ int, slot string) error {
			detachSlots = append(detachSlots, slot)
			return nil
		},
	}

	err := detachForeignActiveDisks(context.Background(), fdDeps(q), "pve-node1", "100", 100, log.NewNopLogger())
	if err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}
	if len(detachSlots) != 1 || detachSlots[0] != "scsi1" {
		t.Errorf("DetachDisk: want [scsi1], got %v", detachSlots)
	}
}

// ---------------------------------------------------------------------------
// TestDetachForeignActiveDisks_DetachFails: DetachDisk returns error.
// The function must return a retriable error and NOT proceed to destroy.
// ---------------------------------------------------------------------------

func TestDetachForeignActiveDisks_DetachFails(t *testing.T) {
	t.Parallel()

	initialCfg := map[string]any{
		"virtio0": "zfs-1:vm-100-disk-0",
		"scsi1":   "zfs-1:vm-777-disk-0,size=128G",
	}

	detachErr := errors.New("pve: lock timeout")
	q := &fdQEMU{
		configs: []map[string]any{initialCfg},
		detachFn: func(_ context.Context, _ string, _ int, _ string) error {
			return detachErr
		},
	}

	err := detachForeignActiveDisks(context.Background(), fdDeps(q), "pve-node1", "100", 100, log.NewNopLogger())
	if err == nil {
		t.Fatal("expected error when DetachDisk fails, got nil")
	}
	// The error must be retriable so the Director retries.
	type retriableChecker interface{ OkToRetry() bool }
	if rc, ok := err.(retriableChecker); !ok || !rc.OkToRetry() {
		t.Errorf("error must be retriable; got: %v (type %T)", err, err)
	}
}

// ---------------------------------------------------------------------------
// TestDetachForeignActiveDisks_OwnedOnly: all disks belong to ownerVMID.
// DetachDisk must NOT be called; no error returned.
// ---------------------------------------------------------------------------

func TestDetachForeignActiveDisks_OwnedOnly(t *testing.T) {
	t.Parallel()

	cfg := map[string]any{
		"virtio0": "zfs-1:vm-100-disk-0",
		"scsi0":   "zfs-1:vm-100-disk-1",
		"ide2":    "local-lvm:vm-100-cloudinit",
	}

	q := &fdQEMU{
		configs: []map[string]any{cfg},
		detachFn: func(_ context.Context, _ string, _ int, slot string) error {
			t.Errorf("DetachDisk must not be called for owned-only VM; got slot=%q", slot)
			return nil
		},
	}

	err := detachForeignActiveDisks(context.Background(), fdDeps(q), "pve-node1", "100", 100, log.NewNopLogger())
	if err != nil {
		t.Fatalf("expected nil error for owned-only VM, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// TestDetachForeignActiveDisks_SilentNoOpStillActive: DetachDisk returns nil but
// the disk remains on its active slot in the re-read config (SDK regression /
// race). The destroy must be refused with a retriable error rather than letting
// the purge take the still-attached volume.
// ---------------------------------------------------------------------------

func TestDetachForeignActiveDisks_SilentNoOpStillActive(t *testing.T) {
	t.Parallel()

	cfg := map[string]any{
		"virtio0": "zfs-1:vm-100-disk-0",
		"scsi1":   "zfs-1:vm-777-disk-0,size=128G",
	}
	// Both reads return the same config: the foreign disk is still on scsi1
	// after the (silently no-op) detach.
	q := &fdQEMU{
		configs: []map[string]any{cfg, cfg},
		detachFn: func(_ context.Context, _ string, _ int, _ string) error {
			return nil // pretends to succeed but changes nothing
		},
	}

	err := detachForeignActiveDisks(context.Background(), fdDeps(q), "pve-node1", "100", 100, log.NewNopLogger())
	if err == nil {
		t.Fatal("expected refusal when foreign disk remains on an active slot after detach, got nil")
	}
	type retriableChecker interface{ OkToRetry() bool }
	if rc, ok := err.(retriableChecker); !ok || !rc.OkToRetry() {
		t.Errorf("silent-no-op active-disk refusal must be retriable; got: %v (type %T)", err, err)
	}
}
