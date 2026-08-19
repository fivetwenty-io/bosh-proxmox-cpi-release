package handlers

import (
	"context"
	"errors"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	sdkcloudinit "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cloudinit"
	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdkclusterstorage "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/clusterstorage"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	sdkqemu "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
	sdkstorage "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/storage"
	sdktasks "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/tasks"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
)

// ---------------------------------------------------------------------------
// attachEphemeralDisk / cleanupVol: create_vm_disksizing_internal_test.go
// already covers cleanupVol's three real call sites (Config-read failure,
// SCSI-slot exhaustion, AttachDisk failure) and the DeleteVolumeAsync
// UPID-await contract (TestAttachEphemeralDisk_ConfigReadFail_OrphanCleanup,
// _ScsiSlotExhausted, _AttachFail_OrphanCleanup,
// _OrphanCleanup_AwaitsDeleteUPID, _OrphanCleanup_AwaitFailureLoggedNotFatal).
// Every one of those uses a volid already carrying a "<storage>:" prefix, so
// the stor == "" fallback at the top of cleanupVol (pve.ParseDiskCID cannot
// split a bare volid, so cleanupVol falls back to shape.ephemeralStorage)
// stays untested — the one branch called out as "most likely to be wrong".
// These tests close that gap and add the mirror-image negative case (no
// cleanup once AttachDisk itself succeeds).
// ---------------------------------------------------------------------------

// ephemeralQEMU is a minimal sdkqemu.Service fake for attachEphemeralDisk
// tests: configurable Config/AttachDisk behavior.
type ephemeralQEMU struct {
	sdkqemu.Service
	configFn     func() (map[string]any, error)
	attachDiskFn func(volid, bus string, opts *sdkqemu.AttachOpts) (string, error)
}

func (q *ephemeralQEMU) Config(_ context.Context, _ string, _ int) (map[string]any, error) {
	return q.configFn()
}

func (q *ephemeralQEMU) AttachDisk(_ context.Context, _ string, _ int, volid, bus string, opts *sdkqemu.AttachOpts) (string, error) {
	return q.attachDiskFn(volid, bus, opts)
}

// ephemeralStorageSvc is a minimal sdkstorage.Service fake recording
// CreateVolume and DeleteVolumeAsync calls.
type ephemeralStorageSvc struct {
	sdkstorage.Service
	createVolumeReturn string

	deleteAsyncCalls []struct{ node, storage, volume string }
}

func (s *ephemeralStorageSvc) CreateVolume(_ context.Context, _, _ string, _ int, _ string, _ int, _ string) (string, error) {
	return s.createVolumeReturn, nil
}

func (s *ephemeralStorageSvc) DeleteVolumeAsync(_ context.Context, node, storage, volume string) (string, error) {
	s.deleteAsyncCalls = append(s.deleteAsyncCalls, struct{ node, storage, volume string }{node, storage, volume})
	return "", nil
}

// ephemeralClient implements pve.Client, wiring only QEMU()/Storage(); Tasks
// is unused here since DeleteVolumeAsync returns no UPID in these fakes.
type ephemeralClient struct {
	qemu    sdkqemu.Service
	storage sdkstorage.Service
}

func (c *ephemeralClient) QEMU() sdkqemu.Service                     { return c.qemu }
func (c *ephemeralClient) Nodes() sdknodes.Service                   { return nil }
func (c *ephemeralClient) Storage() sdkstorage.Service               { return c.storage }
func (c *ephemeralClient) CloudInit() sdkcloudinit.Service           { return nil }
func (c *ephemeralClient) Tasks() sdktasks.Service                   { return nil }
func (c *ephemeralClient) Cluster() sdkcluster.Service               { return nil }
func (c *ephemeralClient) ClusterStorage() sdkclusterstorage.Service { return nil }
func (c *ephemeralClient) Pools() pve.PoolService                    { return nil }

const ephemeralTestNode = "pve1"
const ephemeralTestStorage = "zfs-ephemeral"
const ephemeralTestVMID = 9101

func newEphemeralShape() *createVMShape {
	return &createVMShape{
		node:             ephemeralTestNode,
		ephemeralStorage: ephemeralTestStorage,
		ephemeralDiskGiB: 10,
		vmDiskFormat:     "raw",
	}
}

// TestAttachEphemeralDisk_BareVolidFromCreateVolume_FallsBackToShapeStorage
// covers the stor == "" fallback in cleanupVol: when CreateVolume echoes a
// bare volume name with no "<storage>:" prefix (ParseDiskCID cannot split
// it), cleanupVol must fall back to shape.ephemeralStorage rather than
// calling DeleteVolumeAsync with an empty storage argument.
func TestAttachEphemeralDisk_BareVolidFromCreateVolume_FallsBackToShapeStorage(t *testing.T) {
	t.Parallel()

	const bareVolid = "vm-9101-ephemeral-0" // no "<storage>:" prefix
	stor := &ephemeralStorageSvc{createVolumeReturn: bareVolid}
	q := &ephemeralQEMU{
		configFn: func() (map[string]any, error) {
			return nil, errors.New("simulated: Config transport failure")
		},
	}
	deps := Deps{PVE: &ephemeralClient{qemu: q, storage: stor}, Logger: log.NewNopLogger()}

	_, err := attachEphemeralDisk(context.Background(), deps, log.NewNopLogger(), newEphemeralShape(), ephemeralTestVMID)
	if err == nil {
		t.Fatal("expected error when QEMU().Config fails")
	}
	if len(stor.deleteAsyncCalls) != 1 {
		t.Fatalf("DeleteVolumeAsync: want 1 cleanup call, got %d", len(stor.deleteAsyncCalls))
	}
	got := stor.deleteAsyncCalls[0]
	if got.storage != ephemeralTestStorage {
		t.Errorf("DeleteVolumeAsync storage = %q; want fallback to shape.ephemeralStorage %q (ParseDiskCID could not split the bare volid)",
			got.storage, ephemeralTestStorage)
	}
	if got.volume != bareVolid {
		t.Errorf("DeleteVolumeAsync volume = %q; want the bare volid %q unchanged", got.volume, bareVolid)
	}
	if got.node != ephemeralTestNode {
		t.Errorf("DeleteVolumeAsync node = %q; want %q", got.node, ephemeralTestNode)
	}
}

// TestAttachEphemeralDisk_AttachSucceeds_NoCleanup verifies the deliberate
// asymmetry in attachEphemeralDisk: once AttachDisk itself succeeds,
// cleanupVol must NOT run — the disk is already attached to the VM, so the
// VM-rollback defer in createVM (purge=true) owns destroying it along with
// the VM, not a second independent volume delete.
func TestAttachEphemeralDisk_AttachSucceeds_NoCleanup(t *testing.T) {
	t.Parallel()

	const createdVolid = ephemeralTestStorage + ":vm-9101-ephemeral-0"
	stor := &ephemeralStorageSvc{createVolumeReturn: createdVolid}
	q := &ephemeralQEMU{
		configFn: func() (map[string]any, error) {
			return map[string]any{"virtio0": "zfs-1:vm-9101-disk-0"}, nil
		},
		attachDiskFn: func(_, _ string, _ *sdkqemu.AttachOpts) (string, error) {
			return "UPID:pve1:1:2:qmattach:9101:root@pam:", nil
		},
	}
	deps := Deps{PVE: &ephemeralClient{qemu: q, storage: stor}, Logger: log.NewNopLogger()}

	devPath, err := attachEphemeralDisk(context.Background(), deps, log.NewNopLogger(), newEphemeralShape(), ephemeralTestVMID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if devPath == "" {
		t.Error("expected a non-empty device path on success")
	}
	if len(stor.deleteAsyncCalls) != 0 {
		t.Errorf("DeleteVolumeAsync must NOT be called once AttachDisk succeeded, got %d calls", len(stor.deleteAsyncCalls))
	}
}
