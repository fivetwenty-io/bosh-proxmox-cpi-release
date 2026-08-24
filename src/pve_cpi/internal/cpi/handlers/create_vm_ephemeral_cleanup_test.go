package handlers

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	sdkcloudinit "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cloudinit"
	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdkclusterstorage "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/clusterstorage"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	sdkqemu "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
	sdkstorage "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/storage"
	sdktasks "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/tasks"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
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
	createVolumeFn     func(attempt int) (string, error)
	existsFn           func(volume string) (bool, error)

	createVolumeCalls int
	existsCalls       []string
	deleteAsyncCalls  []struct{ node, storage, volume string }
}

func (s *ephemeralStorageSvc) Exists(_ context.Context, _, _, volume string) (bool, error) {
	s.existsCalls = append(s.existsCalls, volume)
	if s.existsFn != nil {
		return s.existsFn(volume)
	}
	return false, nil
}

func (s *ephemeralStorageSvc) CreateVolume(_ context.Context, _, _ string, _ int, _ string, _ int, _ string) (string, error) {
	s.createVolumeCalls++
	if s.createVolumeFn != nil {
		return s.createVolumeFn(s.createVolumeCalls)
	}
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

// TestAttachEphemeralDisk_StorageLockContention_RetriesInPlace reproduces the
// live CF-deploy failure: many parallel create_vm calls allocate ephemeral
// volumes on the same shared pool, and the cfs-lock losers die with "got lock
// request timeout". Failing straight back to the Director re-runs the whole
// create_vm only to re-enter the same contention window, so the allocation
// must back off and retry in place, the same way create_disk's
// attemptCreateVolume does.
func TestAttachEphemeralDisk_StorageLockContention_RetriesInPlace(t *testing.T) {
	t.Parallel()

	const createdVolid = ephemeralTestStorage + ":vm-9101-ephemeral-0"
	// Exact live shape (task 258 on Hilal's cluster): an API-level 500 whose
	// body is the pmxcfs lock timeout prose.
	lockErr := errors.New("API request failed: cfs-lock 'storage-rbd' error: got lock request timeout\n (code: 0)")
	stor := &ephemeralStorageSvc{
		createVolumeFn: func(attempt int) (string, error) {
			if attempt == 1 {
				return "", lockErr
			}
			return createdVolid, nil
		},
	}
	q := &ephemeralQEMU{
		configFn: func() (map[string]any, error) {
			return map[string]any{"virtio0": "zfs-1:vm-9101-disk-0"}, nil
		},
		attachDiskFn: func(_, _ string, _ *sdkqemu.AttachOpts) (string, error) {
			return "UPID:pve1:1:2:qmattach:9101:root@pam:", nil
		},
	}
	deps := Deps{PVE: &ephemeralClient{qemu: q, storage: stor}, Logger: log.NewNopLogger()}

	ctx := pve.WithTestBackoff(context.Background(), func(int) time.Duration { return 0 })
	devPath, err := attachEphemeralDisk(ctx, deps, log.NewNopLogger(), newEphemeralShape(), ephemeralTestVMID)
	if err != nil {
		t.Fatalf("the second attempt succeeded; attachEphemeralDisk must ride out the lock timeout: %v", err)
	}
	if devPath == "" {
		t.Error("expected a non-empty device path on success")
	}
	if stor.createVolumeCalls != 2 {
		t.Errorf("CreateVolume calls = %d, want 2 (one lock timeout, one retry)", stor.createVolumeCalls)
	}
}

// TestAttachEphemeralDisk_ExhaustedLockRetries_SweepsCommittedVolume covers
// the drop-after-commit residue: when every CreateVolume attempt fails on
// lock contention but PVE committed the volume anyway (the storage task ran
// after the API answer was lost), the failure path must probe for the
// canonical volid and remove it, so the Director's redo with a fresh VMID
// does not orphan the committed volume.
func TestAttachEphemeralDisk_ExhaustedLockRetries_SweepsCommittedVolume(t *testing.T) {
	t.Parallel()

	lockErr := errors.New("API request failed: cfs-lock 'storage-rbd' error: got lock request timeout\n (code: 0)")
	stor := &ephemeralStorageSvc{
		createVolumeFn: func(int) (string, error) { return "", lockErr },
		existsFn:       func(string) (bool, error) { return true, nil },
	}
	deps := Deps{PVE: &ephemeralClient{qemu: &ephemeralQEMU{}, storage: stor}, Logger: log.NewNopLogger()}

	ctx := pve.WithTestBackoff(context.Background(), func(int) time.Duration { return 0 })
	if _, err := attachEphemeralDisk(ctx, deps, log.NewNopLogger(), newEphemeralShape(), ephemeralTestVMID); err == nil {
		t.Fatal("expected the exhausted lock retries to surface an error")
	}
	if stor.createVolumeCalls != pve.DefaultStorageLockMaxAttempts {
		t.Errorf("CreateVolume calls = %d, want the full default budget %d",
			stor.createVolumeCalls, pve.DefaultStorageLockMaxAttempts)
	}
	const canonical = ephemeralTestStorage + ":vm-9101-ephemeral-0"
	if len(stor.existsCalls) != 1 || stor.existsCalls[0] != canonical {
		t.Fatalf("Exists probes = %v, want exactly one for %q", stor.existsCalls, canonical)
	}
	if len(stor.deleteAsyncCalls) != 1 {
		t.Fatalf("DeleteVolumeAsync: want 1 sweep call, got %d", len(stor.deleteAsyncCalls))
	}
	got := stor.deleteAsyncCalls[0]
	if got.node != ephemeralTestNode || got.storage != ephemeralTestStorage || got.volume != canonical {
		t.Errorf("sweep called with (node=%q storage=%q volume=%q), want (%q, %q, %q)",
			got.node, got.storage, got.volume, ephemeralTestNode, ephemeralTestStorage, canonical)
	}
}

// TestAttachEphemeralDisk_PermanentCreateError_NoRetry pins the boundary of
// the in-place retry: a verdict about the request (here a 4xx-shaped
// rejection) must surface immediately, not burn the lock-contention budget.
func TestAttachEphemeralDisk_PermanentCreateError_NoRetry(t *testing.T) {
	t.Parallel()

	stor := &ephemeralStorageSvc{
		createVolumeFn: func(int) (string, error) {
			return "", errors.New("parameter verification failed: invalid format")
		},
	}
	deps := Deps{PVE: &ephemeralClient{qemu: &ephemeralQEMU{}, storage: stor}, Logger: log.NewNopLogger()}

	ctx := pve.WithTestBackoff(context.Background(), func(int) time.Duration { return 0 })
	if _, err := attachEphemeralDisk(ctx, deps, log.NewNopLogger(), newEphemeralShape(), ephemeralTestVMID); err == nil {
		t.Fatal("expected the permanent CreateVolume error to propagate")
	}
	if stor.createVolumeCalls != 1 {
		t.Errorf("CreateVolume calls = %d, want exactly 1 (no retry on a permanent error)", stor.createVolumeCalls)
	}
	if len(stor.deleteAsyncCalls) != 0 {
		t.Errorf("DeleteVolumeAsync calls = %d, want 0 (the existence probe answered false, so nothing to sweep)",
			len(stor.deleteAsyncCalls))
	}
}
