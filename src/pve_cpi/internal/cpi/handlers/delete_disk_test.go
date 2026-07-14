package handlers_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"

	sdkclusterapi "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/tasks"
	sdkerrors "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/errors"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi/handlers"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
)

// baseDepsForDelete builds Deps for delete_disk tests.
func baseDepsForDelete(t *testing.T, storageSvc *mockStorageService) handlers.Deps {
	t.Helper()
	return handlers.Deps{
		Config: &config.CPIConfig{
			Node:        testNode,
			DiskStorage: storageName,
		},
		PVE:    newHandlerMockClient(storageSvc, nil),
		Logger: log.NewNopLogger(),
	}
}

// ---------------------------------------------------------------------------
// delete_disk tests
// ---------------------------------------------------------------------------

func TestHandleDeleteDisk_Happy(t *testing.T) {
	t.Parallel()

	type deleteVolumeCall struct {
		node    string
		storage string
		volume  string
	}
	var deleteCalls []deleteVolumeCall

	storageSvc := &mockStorageService{
		deleteVolumeFn: func(_ context.Context, node, storage, volume string) error {
			deleteCalls = append(deleteCalls, deleteVolumeCall{node, storage, volume})
			return nil
		},
	}
	deps := baseDepsForDelete(t, storageSvc)

	h := handlers.HandleDeleteDisk(deps)
	result, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(diskCID),
	}, jsonrpc.Context{})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != nil {
		t.Errorf("expected nil result for void method, got %v", result)
	}
	if len(deleteCalls) != 1 {
		t.Fatalf("DeleteVolume: want 1 call, got %d", len(deleteCalls))
	}
	if deleteCalls[0].node != testNode {
		t.Errorf("DeleteVolume: want node=%q, got %q", testNode, deleteCalls[0].node)
	}
	if deleteCalls[0].storage != storageName {
		t.Errorf("DeleteVolume: want storage=%q, got %q", storageName, deleteCalls[0].storage)
	}
	if deleteCalls[0].volume != diskCID {
		t.Errorf("DeleteVolume: want volume=%q, got %q", diskCID, deleteCalls[0].volume)
	}
}

func TestHandleDeleteDisk_NotFound_Idempotent(t *testing.T) {
	t.Parallel()
	// SDK 404 → DeleteVolume already handles it; but test that a not-found
	// error surfacing from a non-SDK path is also treated as success.
	storageSvc := &mockStorageService{
		deleteVolumeFn: func(_ context.Context, _, _, _ string) error {
			// Simulate a not-found error that pve.IsNotFound recognises.
			return &sdkerrors.APIError{
				Message:  "volume not found",
				HTTPCode: 404,
			}
		},
	}
	deps := baseDepsForDelete(t, storageSvc)

	h := handlers.HandleDeleteDisk(deps)
	result, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(diskCID),
	}, jsonrpc.Context{})

	if err != nil {
		t.Fatalf("expected nil error for 404, got: %v", err)
	}
	if result != nil {
		t.Errorf("expected nil result, got %v", result)
	}
}

func TestHandleDeleteDisk_SDKError(t *testing.T) {
	t.Parallel()
	storageSvc := &mockStorageService{
		deleteVolumeFn: func(_ context.Context, _, _, _ string) error {
			return errors.New("storage backend unavailable")
		},
	}
	deps := baseDepsForDelete(t, storageSvc)

	h := handlers.HandleDeleteDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(diskCID),
	}, jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected error from DeleteVolume failure, got nil")
	}
}

func TestHandleDeleteDisk_MalformedCID_NoColon(t *testing.T) {
	t.Parallel()
	storageSvc := &mockStorageService{}
	deps := baseDepsForDelete(t, storageSvc)

	h := handlers.HandleDeleteDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{
		marshal("bad-disk-cid-without-colon"),
	}, jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected error for malformed disk_cid with no colon")
	}
}

func TestHandleDeleteDisk_EmptyCID(t *testing.T) {
	t.Parallel()
	storageSvc := &mockStorageService{}
	deps := baseDepsForDelete(t, storageSvc)

	h := handlers.HandleDeleteDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(""),
	}, jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected error for empty disk_cid")
	}
}

func TestHandleDeleteDisk_TooFewArgs(t *testing.T) {
	t.Parallel()
	storageSvc := &mockStorageService{}
	deps := baseDepsForDelete(t, storageSvc)

	h := handlers.HandleDeleteDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{}, jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected error for missing disk_cid argument")
	}
}

func TestHandleDeleteDisk_MissingNode(t *testing.T) {
	t.Parallel()
	storageSvc := &mockStorageService{}
	deps := handlers.Deps{
		Config: &config.CPIConfig{
			Node:        "",
			DiskStorage: storageName,
		},
		PVE:    newHandlerMockClient(storageSvc, nil),
		Logger: log.NewNopLogger(),
	}

	h := handlers.HandleDeleteDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(diskCID),
	}, jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected error for missing node")
	}
}

func TestHandleDeleteDisk_EmptyStoragePart(t *testing.T) {
	t.Parallel()
	storageSvc := &mockStorageService{}
	deps := baseDepsForDelete(t, storageSvc)

	h := handlers.HandleDeleteDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(":volume-only"),
	}, jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected error for disk_cid with empty storage part")
	}
}

func TestHandleDeleteDisk_EmptyVolumePart(t *testing.T) {
	t.Parallel()
	storageSvc := &mockStorageService{}
	deps := baseDepsForDelete(t, storageSvc)

	h := handlers.HandleDeleteDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{
		marshal("storage:"),
	}, jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected error for disk_cid with empty volume part")
	}
}

// newHandlerMockClientNoCluster builds a mock client without cluster (cluster not needed for delete/has).
// Reuses newHandlerMockClient with empty cluster VMID list.
func newHandlerMockClientNoCluster(storageSvc *mockStorageService) any /* pve.Client */ {
	return newHandlerMockClient(storageSvc, []int{})
}

// Verify TestHandleDeleteDisk_NotFound_Idempotent covers the branch where SDK
// DeleteVolume itself returns nil (it's already 404-safe). The mock above
// simulates a non-SDK not-found to exercise the CPI-level IsNotFound fallback.

func TestHandleDeleteDisk_SDKDeleteVolumeReturnsNil(t *testing.T) {
	t.Parallel()
	// SDK already returns nil for 404 — most common production path.
	storageSvc := &mockStorageService{
		deleteVolumeFn: func(_ context.Context, _, _, _ string) error {
			return nil // SDK swallowed the 404
		},
	}
	deps := baseDepsForDelete(t, storageSvc)

	h := handlers.HandleDeleteDisk(deps)
	result, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(diskCID),
	}, jsonrpc.Context{})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != nil {
		t.Errorf("expected nil result, got %v", result)
	}
}

// ---------------------------------------------------------------------------
// CID-variant tests
//
// delete_disk is storage-type-agnostic: the handler splits on the first colon
// to extract storage, then forwards the full disk_cid to DeleteVolumeAsync.
// These tests exercise ParseDiskCID with each active local storage CID format,
// confirming the correct storage and full volume strings reach the SDK call.
// There is no divergent handler logic across storage types — the variants
// validate CID identity through the split-on-colon parse.
// ---------------------------------------------------------------------------

func TestHandleDeleteDisk_LVM_CID(t *testing.T) {
	t.Parallel()
	const cid = diskCID
	var capturedStorage, capturedVolume string
	storageSvc := &mockStorageService{
		deleteVolumeFn: func(_ context.Context, _, storage, volume string) error {
			capturedStorage = storage
			capturedVolume = volume
			return nil
		},
	}
	deps := baseDepsForDelete(t, storageSvc)

	h := handlers.HandleDeleteDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{marshal(cid)}, jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedStorage != storageName {
		t.Errorf("storage: got %q, want %q", capturedStorage, storageName)
	}
	if capturedVolume != cid {
		t.Errorf("volume: got %q, want %q", capturedVolume, cid)
	}
}

func TestHandleDeleteDisk_LVMThin_CID(t *testing.T) {
	t.Parallel()
	const cid = "local-lvm-thin:vm-9001-disk-0"
	var capturedStorage, capturedVolume string
	storageSvc := &mockStorageService{
		deleteVolumeFn: func(_ context.Context, _, storage, volume string) error {
			capturedStorage = storage
			capturedVolume = volume
			return nil
		},
	}
	deps := baseDepsForDelete(t, storageSvc)

	h := handlers.HandleDeleteDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{marshal(cid)}, jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedStorage != "local-lvm-thin" {
		t.Errorf("storage: got %q, want %q", capturedStorage, "local-lvm-thin")
	}
	if capturedVolume != cid {
		t.Errorf("volume: got %q, want %q", capturedVolume, cid)
	}
}

func TestHandleDeleteDisk_ZFSPool_CID(t *testing.T) {
	t.Parallel()
	const cid = "local-zfs:vm-9001-disk-0"
	var capturedStorage, capturedVolume string
	storageSvc := &mockStorageService{
		deleteVolumeFn: func(_ context.Context, _, storage, volume string) error {
			capturedStorage = storage
			capturedVolume = volume
			return nil
		},
	}
	deps := baseDepsForDelete(t, storageSvc)

	h := handlers.HandleDeleteDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{marshal(cid)}, jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedStorage != "local-zfs" {
		t.Errorf("storage: got %q, want %q", capturedStorage, "local-zfs")
	}
	if capturedVolume != cid {
		t.Errorf("volume: got %q, want %q", capturedVolume, cid)
	}
}

func TestHandleDeleteDisk_Dir_CID(t *testing.T) {
	t.Parallel()
	// dir-style CID: path-form volume — the portion after the first colon
	// contains a subpath with vmid prefix and extension.
	// ParseDiskCID splits on the first colon only, so storage="local" and
	// the full volume string including the subpath is forwarded to the SDK.
	const cid = "local:9001/vm-9001-disk-0.raw"
	var capturedStorage, capturedVolume string
	storageSvc := &mockStorageService{
		deleteVolumeFn: func(_ context.Context, _, storage, volume string) error {
			capturedStorage = storage
			capturedVolume = volume
			return nil
		},
	}
	deps := baseDepsForDelete(t, storageSvc)

	h := handlers.HandleDeleteDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{marshal(cid)}, jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedStorage != "local" {
		t.Errorf("storage: got %q, want %q", capturedStorage, "local")
	}
	if capturedVolume != cid {
		t.Errorf("volume: got %q, want full cid %q", capturedVolume, cid)
	}
}

// ---------------------------------------------------------------------------
// Parker unpark integration tests
//
// These tests exercise the 3c block added to HandleDeleteDisk:
//   - parked disk → UnparkDisk → then delete
//   - unpark failure → retriable error, delete not called
//   - disk present but not parked (free-floating) → direct delete
//   - strategy unset/free → zero parker calls (byte-identical path)
// ---------------------------------------------------------------------------

// parkerDeleteClient builds a mockPVEClient wired for parker tests:
//   - cluster scan returns parkerVMID on parkerNode (makes IsDiskParked resolve holder)
//   - QEMU Config returns a map with the disk in scsi0 + bosh-parker tag
//     (so IsDiskParked confirms parked=true) plus a DetachDisk stub
//   - storage DeleteVolumeAsync controlled by storageSvc
//
// When parkerVMID == 0 the cluster response is empty (disk free-floating).
func parkerDeleteClient(
	t *testing.T,
	storageSvc *mockStorageService,
	parkerVMID int,
	parkerNode string,
	detachErr error,
) (*mockPVEClient, *int) {
	t.Helper()
	var detachCalls int

	qemuSvc := &mockQEMUService{
		configFn: func(_ context.Context, _ string, vmid int) (map[string]any, error) {
			if vmid == parkerVMID {
				return map[string]any{
					"tags":  "bosh-parker",
					"scsi0": diskCID,
				}, nil
			}
			return map[string]any{}, nil
		},
		detachDiskFn: func(_ context.Context, _ string, _ int, _ string) error {
			detachCalls++
			return detachErr
		},
	}

	var clusterSvc *mockClusterSvc
	if parkerVMID == 0 {
		// Free-floating disk: no VM holds it.
		clusterSvc = &mockClusterSvc{}
	} else {
		// Parker VM holds the disk.
		clusterSvc = &mockClusterSvc{
			listResourcesFn: func(_ context.Context, _ *sdkclusterapi.ListResourcesParams) (*sdkclusterapi.ListResourcesResponse, error) {
				raw, _ := json.Marshal(map[string]any{
					"vmid": parkerVMID,
					"node": parkerNode,
					"type": "qemu",
				})
				resp := sdkclusterapi.ListResourcesResponse{raw}
				return &resp, nil
			},
		}
	}

	client := &mockPVEClient{
		storageSvc: storageSvc,
		qemuSvc:    qemuSvc,
		clusterSvc: clusterSvc,
	}
	return client, &detachCalls
}

// parkerDepsForDelete builds Deps with ParkedDiskVMIDRangeStart/End set so
// ParkedStrategyActive() returns true. The range covers parkerVMID 90000.
func parkerDepsForDelete(client *mockPVEClient) handlers.Deps {
	return handlers.Deps{
		Config: &config.CPIConfig{
			Node:                     testNode,
			DiskStorage:              storageName,
			DetachedDiskStrategy:     "parked",
			ParkedDiskVMIDRangeStart: 90000,
			ParkedDiskVMIDRangeEnd:   90999,
		},
		PVE:    client,
		Logger: log.NewNopLogger(),
	}
}

// TestHandleDeleteDisk_Parker_Parked_UnparksAndDeletes verifies that when the
// disk is held by a parker VM, delete_disk detaches it from the parker then
// deletes the volume.
func TestHandleDeleteDisk_Parker_Parked_UnparksAndDeletes(t *testing.T) {
	t.Parallel()

	deleteCalled := false
	storageSvc := &mockStorageService{
		deleteVolumeFn: func(_ context.Context, _, _, _ string) error {
			deleteCalled = true
			return nil
		},
	}
	client, detachCalls := parkerDeleteClient(t, storageSvc, 90000, testNode, nil)
	deps := parkerDepsForDelete(client)

	h := handlers.HandleDeleteDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{marshal(diskCID)}, jsonrpc.Context{})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !deleteCalled {
		t.Error("delete_disk: volume delete must be called after unpark")
	}
	if *detachCalls == 0 {
		t.Error("delete_disk: DetachDisk (unpark) must be called for parked disk")
	}
}

// TestHandleDeleteDisk_Parker_UnparkFail_Retriable verifies that when UnparkDisk
// fails, delete_disk returns a retriable error and does NOT delete the volume.
func TestHandleDeleteDisk_Parker_UnparkFail_Retriable(t *testing.T) {
	t.Parallel()

	deleteCalled := false
	storageSvc := &mockStorageService{
		deleteVolumeFn: func(_ context.Context, _, _, _ string) error {
			deleteCalled = true
			return nil
		},
	}
	unparkErr := errors.New("PVE lock contention")
	client, _ := parkerDeleteClient(t, storageSvc, 90000, testNode, unparkErr)
	deps := parkerDepsForDelete(client)

	h := handlers.HandleDeleteDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{marshal(diskCID)}, jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected error when unpark fails, got nil")
	}
	if deleteCalled {
		t.Error("delete_disk must NOT delete volume when unpark fails")
	}
}

// TestHandleDeleteDisk_Parker_NotParked_DirectDelete verifies that when
// ParkedStrategyActive() is true but the disk is free-floating (not on a parker
// VM), delete_disk deletes the volume directly without any DetachDisk call.
func TestHandleDeleteDisk_Parker_NotParked_DirectDelete(t *testing.T) {
	t.Parallel()

	deleteCalled := false
	storageSvc := &mockStorageService{
		deleteVolumeFn: func(_ context.Context, _, _, _ string) error {
			deleteCalled = true
			return nil
		},
	}
	// parkerVMID=0 → cluster returns empty → disk not attached to any VM.
	client, detachCalls := parkerDeleteClient(t, storageSvc, 0, "", nil)
	deps := parkerDepsForDelete(client)

	h := handlers.HandleDeleteDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{marshal(diskCID)}, jsonrpc.Context{})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !deleteCalled {
		t.Error("delete_disk: volume delete must be called for free-floating disk under parked strategy")
	}
	if *detachCalls != 0 {
		t.Errorf("delete_disk: DetachDisk must NOT be called for free-floating disk, got %d calls", *detachCalls)
	}
}

// TestHandleDeleteDisk_Parker_StrategyFree_NoParkerCalls verifies that when
// neither DetachedDiskStrategy=parked nor range fields are set,
// ParkedStrategyActive() returns false and zero parker API calls are made.
func TestHandleDeleteDisk_Parker_StrategyFree_NoParkerCalls(t *testing.T) {
	t.Parallel()

	deleteCalled := false
	configCalled := false
	storageSvc := &mockStorageService{
		deleteVolumeFn: func(_ context.Context, _, _, _ string) error {
			deleteCalled = true
			return nil
		},
	}

	qemuSvc := &mockQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			configCalled = true
			return map[string]any{}, nil
		},
	}
	// Cluster returns a parker-range VM so that if the handler mistakenly calls
	// IsDiskParked it would find a result — confirming the gate truly fires.
	clusterSvc := &mockClusterSvc{
		listResourcesFn: func(_ context.Context, _ *sdkclusterapi.ListResourcesParams) (*sdkclusterapi.ListResourcesResponse, error) {
			raw, _ := json.Marshal(map[string]any{
				"vmid": 90000,
				"node": testNode,
				"type": "qemu",
			})
			resp := sdkclusterapi.ListResourcesResponse{raw}
			return &resp, nil
		},
	}
	client := &mockPVEClient{
		storageSvc: storageSvc,
		qemuSvc:    qemuSvc,
		clusterSvc: clusterSvc,
	}
	deps := handlers.Deps{
		Config: &config.CPIConfig{
			Node:        testNode,
			DiskStorage: storageName,
			// DetachedDiskStrategy unset and no range → ParkedStrategyActive()=false
			// disk_delete_state_guard explicitly off: this test isolates the
			// parker-gate's own QEMU Config() call count from the (Phase 1
			// default-on) owner-lock guard, which would otherwise also call
			// QEMU Config() and confound the assertion below.
			DiskDeleteStateGuard: "off",
		},
		PVE:    client,
		Logger: log.NewNopLogger(),
	}

	h := handlers.HandleDeleteDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{marshal(diskCID)}, jsonrpc.Context{})

	if err != nil {
		t.Fatalf("unexpected error with free strategy: %v", err)
	}
	if !deleteCalled {
		t.Error("delete_disk: volume delete must be called when strategy=free")
	}
	if configCalled {
		t.Error("delete_disk: QEMU Config must NOT be called (zero parker calls) when strategy=free")
	}
}

// TODO(storage-network): network-backed storage CID variants (nfs
// "nfs-store:9001/vm-9001-disk-0.qcow2", rbd "ceph-pool:vm-9001-disk-0", cephfs
// "cephfs-pool:vm-9001-disk-0", cifs "cifs-store:9001/vm-9001-disk-0.qcow2") are
// not unit-tested here: their delete path crosses the PVE network-call boundary
// and needs a live shared-storage pool. Add these cases once the integration-test
// harness provisions those pools via env.

// Ensure cluster list not needed when using newHandlerMockClient.
// This validates the delete_disk handler does NOT call NextDiskVMID.
func TestHandleDeleteDisk_NoClusterCallExpected(t *testing.T) {
	t.Parallel()
	clusterCalled := false
	listFn := func(_ context.Context, _ *sdkclusterapi.ListResourcesParams) (*sdkclusterapi.ListResourcesResponse, error) {
		clusterCalled = true
		resp := sdkclusterapi.ListResourcesResponse{}
		return &resp, nil
	}
	storageSvc := &mockStorageService{
		deleteVolumeFn: func(_ context.Context, _, _, _ string) error { return nil },
	}
	client := &mockPVEClient{
		storageSvc: storageSvc,
		clusterSvc: &mockClusterSvc{listResourcesFn: listFn},
	}
	deps := handlers.Deps{
		Config: &config.CPIConfig{
			Node:        testNode,
			DiskStorage: storageName,
			// disk_delete_state_guard explicitly off: this test's assertion is
			// about NextDiskVMID-style cluster calls, not the (Phase 1
			// default-on) owner-lock guard, which also calls the cluster
			// service (FindVMByDiskVolid) and would otherwise confound it.
			DiskDeleteStateGuard: "off",
		},
		PVE:    client,
		Logger: log.NewNopLogger(),
	}

	h := handlers.HandleDeleteDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(diskCID),
	}, jsonrpc.Context{})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if clusterCalled {
		t.Error("delete_disk must not call the cluster service (no VMID allocation needed)")
	}
}

// ---------------------------------------------------------------------------
// disk_delete_state_guard tests
// ---------------------------------------------------------------------------

// guardDeleteAttachedVMID is the VMID every guardDeleteClient test call
// attaches the target disk to. It deliberately differs from the disk-name
// placeholder VMID (9001, embedded in diskCID) to prove the guard resolves
// the CURRENT attachment by config scan, not by the name-embedded VMID.
const guardDeleteAttachedVMID = 9002

// guardDeleteClient wires storage + qemu(config) + cluster services so the
// owner-lock guard can resolve the VM the disk is ATTACHED to and read its
// lock. The disk-name VMID (9001 in diskCID) is only a placeholder; the disk
// is attached to guardDeleteAttachedVMID on testNode, whose config carries
// diskCID at scsi0 plus lock. attachedLock is the config "lock" value.
func guardDeleteClient(
	t *testing.T, storageSvc *mockStorageService, attachedLock string,
) (*mockPVEClient, *int, *int) {
	t.Helper()
	var configReads, listCalls int
	cfg := map[string]any{"scsi0": diskCID}
	if attachedLock != "" {
		cfg["lock"] = attachedLock
	}
	return &mockPVEClient{
		storageSvc: storageSvc,
		qemuSvc: &mockQEMUService{
			configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
				configReads++
				return cfg, nil
			},
		},
		clusterSvc: &mockClusterSvc{
			listResourcesFn: func(_ context.Context, _ *sdkclusterapi.ListResourcesParams) (*sdkclusterapi.ListResourcesResponse, error) {
				listCalls++
				return attachDiskVMOnNode(guardDeleteAttachedVMID, testNode), nil
			},
		},
	}, &configReads, &listCalls
}

func guardDepsForDelete(client *mockPVEClient) handlers.Deps {
	return handlers.Deps{
		Config: &config.CPIConfig{
			Node:                 testNode,
			DiskStorage:          storageName,
			DiskDeleteStateGuard: "on",
		},
		PVE:    client,
		Logger: log.NewNopLogger(),
	}
}

// TestHandleDeleteDisk_Guard_OwnerLocked_Retriable: guard on, the owning VM is
// mid-migrate → delete_disk defers with a retriable error and never issues the
// volume delete.
func TestHandleDeleteDisk_Guard_OwnerLocked_Retriable(t *testing.T) {
	t.Parallel()
	deleteCalled := false
	storageSvc := &mockStorageService{
		deleteVolumeFn: func(_ context.Context, _, _, _ string) error {
			deleteCalled = true
			return nil
		},
	}
	// Disk attached to VM 9002 (≠ placeholder name VMID 9001), mid-migrate.
	client, _, _ := guardDeleteClient(t, storageSvc, "migrate")
	deps := guardDepsForDelete(client)

	h := handlers.HandleDeleteDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{marshal(diskCID)}, jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected retriable error when the attached VM is locked, got nil")
	}
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Errorf("expected retriable-cloud error, got %v", err)
	}
	if deleteCalled {
		t.Error("delete_disk must NOT delete the volume while the attached VM is locked")
	}
}

// TestHandleDeleteDisk_Guard_DefaultUnset_OwnerLocked_Retriable proves the
// Phase 1 default-materialization contract at the full handler level: with
// disk_delete_state_guard entirely ABSENT from config (the shape an
// empty/unset manifest property materializes as — Config.DiskDeleteStateGuard
// is the Go zero value ""), the guard is ACTIVE by default. A locked owning
// VM must defer the delete with a retriable error exactly as it would with an
// explicit "on", proving the empty-manifest default is "guard active" end to
// end, not only at the bare accessor level (see TestDiskDeleteStateGuardAccessor
// in the config package for that unit-level check).
func TestHandleDeleteDisk_Guard_DefaultUnset_OwnerLocked_Retriable(t *testing.T) {
	t.Parallel()
	deleteCalled := false
	storageSvc := &mockStorageService{
		deleteVolumeFn: func(_ context.Context, _, _, _ string) error {
			deleteCalled = true
			return nil
		},
	}
	client, configReads, listCalls := guardDeleteClient(t, storageSvc, "migrate")
	deps := handlers.Deps{
		Config: &config.CPIConfig{
			Node:        testNode,
			DiskStorage: storageName,
			// DiskDeleteStateGuard intentionally left unset (empty-manifest
			// shape) — this is the property under test.
		},
		PVE:    client,
		Logger: log.NewNopLogger(),
	}

	h := handlers.HandleDeleteDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{marshal(diskCID)}, jsonrpc.Context{})

	if err == nil {
		t.Fatal("empty-manifest config: expected retriable error when the attached VM is locked, got nil")
	}
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Errorf("empty-manifest config: expected retriable-cloud error, got %v", err)
	}
	if deleteCalled {
		t.Error("empty-manifest config: delete_disk must NOT delete the volume while the attached VM is locked (guard must default to active)")
	}
	if *configReads == 0 || *listCalls == 0 {
		t.Errorf("empty-manifest config: guard must have performed the owner lookup (config=%d, list=%d)", *configReads, *listCalls)
	}
}

// TestHandleDeleteDisk_Guard_OwnerUnlocked_Deletes: guard on, the attached VM
// has no lock → guard passes and the volume is deleted normally.
func TestHandleDeleteDisk_Guard_OwnerUnlocked_Deletes(t *testing.T) {
	t.Parallel()
	deleteCalled := false
	storageSvc := &mockStorageService{
		deleteVolumeFn: func(_ context.Context, _, _, _ string) error {
			deleteCalled = true
			return nil
		},
	}
	client, configReads, _ := guardDeleteClient(t, storageSvc, "")
	deps := guardDepsForDelete(client)

	h := handlers.HandleDeleteDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{marshal(diskCID)}, jsonrpc.Context{})

	if err != nil {
		t.Fatalf("unexpected error when the attached VM is unlocked: %v", err)
	}
	if !deleteCalled {
		t.Error("delete_disk must delete the volume when the attached VM is unlocked")
	}
	if *configReads == 0 {
		t.Error("guard should have read the attached VM config when enabled")
	}
}

// TestHandleDeleteDisk_Guard_Off_NoOwnerLookup proves the guard is byte-identical
// to the pre-Phase-1 default when explicitly disabled: with
// disk_delete_state_guard: "off", neither the cluster scan nor the config
// read runs, and the delete proceeds. (As of Phase 1, an UNSET property now
// defaults to "on" — see TestHandleDeleteDisk_Guard_OwnerLocked_Retriable and
// friends for that default-enabled behavior; this test covers the explicit
// opt-out.)
func TestHandleDeleteDisk_Guard_Off_NoOwnerLookup(t *testing.T) {
	t.Parallel()
	deleteCalled := false
	storageSvc := &mockStorageService{
		deleteVolumeFn: func(_ context.Context, _, _, _ string) error {
			deleteCalled = true
			return nil
		},
	}
	// The attached VM would be locked, but the guard is explicitly OFF → it must not look.
	client, configReads, listCalls := guardDeleteClient(t, storageSvc, "migrate")
	deps := handlers.Deps{
		Config: &config.CPIConfig{
			Node:                 testNode,
			DiskStorage:          storageName,
			DiskDeleteStateGuard: "off",
		},
		PVE:    client,
		Logger: log.NewNopLogger(),
	}

	h := handlers.HandleDeleteDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{marshal(diskCID)}, jsonrpc.Context{})

	if err != nil {
		t.Fatalf("unexpected error with guard off: %v", err)
	}
	if !deleteCalled {
		t.Error("guard off: delete must proceed")
	}
	if *configReads != 0 || *listCalls != 0 {
		t.Errorf("guard off: must not query owner (config=%d, list=%d)", *configReads, *listCalls)
	}
}

// TestHandleDeleteDisk_Guard_NotAttached_Deletes: guard on, but the disk is
// attached to no VM (the normal pre-delete state) → guard finds no owner and
// the delete runs.
func TestHandleDeleteDisk_Guard_NotAttached_Deletes(t *testing.T) {
	t.Parallel()
	deleteCalled := false
	storageSvc := &mockStorageService{
		deleteVolumeFn: func(_ context.Context, _, _, _ string) error {
			deleteCalled = true
			return nil
		},
	}
	client := &mockPVEClient{
		storageSvc: storageSvc,
		qemuSvc:    &mockQEMUService{},
		clusterSvc: &mockClusterSvc{
			listResourcesFn: func(_ context.Context, _ *sdkclusterapi.ListResourcesParams) (*sdkclusterapi.ListResourcesResponse, error) {
				empty := sdkclusterapi.ListResourcesResponse{}
				return &empty, nil
			},
		},
	}
	deps := guardDepsForDelete(client)

	h := handlers.HandleDeleteDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{marshal(diskCID)}, jsonrpc.Context{})

	if err != nil {
		t.Fatalf("unexpected error when disk is not attached: %v", err)
	}
	if !deleteCalled {
		t.Error("guard on + not attached: delete must proceed")
	}
}

// ---------------------------------------------------------------------------
// §7.32 fast-path delete_disk tests
// ---------------------------------------------------------------------------

// fastPathDepsForDisk builds Deps with fast_path_delete enabled for delete_disk.
func fastPathDepsForDisk(t *testing.T, storageSvc *mockStorageService) handlers.Deps {
	t.Helper()
	deps := baseDepsForDelete(t, storageSvc)
	enabled := true
	deps.Config.FastPathDelete = &enabled
	return deps
}

// TestHandleDeleteDisk_FastPath_NoAwait verifies that when fast_path_delete is
// enabled, delete_disk issues DeleteVolumeAsync but does NOT call task await.
// The mockStorageService.DeleteVolumeAsync returns a UPID; if tasksSvc.Wait
// were called (it is not wired in baseDepsForDelete), the test would panic,
// confirming await is skipped.
func TestHandleDeleteDisk_FastPath_NoAwait(t *testing.T) {
	t.Parallel()

	// Return a non-empty UPID from DeleteVolumeAsync to prove the handler
	// does not pass it to AwaitTask (which would require tasksSvc.waitFn).
	deleteAsyncCalled := false
	storageSvc := &mockStorageService{
		deleteVolumeFn: func(_ context.Context, _, _, _ string) error {
			// DeleteVolumeAsync delegates to DeleteVolume in the mock.
			deleteAsyncCalled = true
			return nil
		},
	}

	deps := fastPathDepsForDisk(t, storageSvc)
	h := handlers.HandleDeleteDisk(deps)
	result, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(diskCID),
	}, jsonrpc.Context{})

	if err != nil {
		t.Fatalf("fast-path delete_disk: unexpected error: %v", err)
	}
	if result != nil {
		t.Errorf("fast-path delete_disk: expected nil result, got %v", result)
	}
	if !deleteAsyncCalled {
		t.Error("fast-path delete_disk: DeleteVolumeAsync must be called")
	}
}

// TestHandleDeleteDisk_FastPath_NotFound_Idempotent verifies that a 404 from
// DeleteVolumeAsync on the fast path still returns success.
func TestHandleDeleteDisk_FastPath_NotFound_Idempotent(t *testing.T) {
	t.Parallel()

	storageSvc := &mockStorageService{
		deleteVolumeFn: func(_ context.Context, _, _, _ string) error {
			return &sdkerrors.APIError{HTTPCode: 404, Message: "volume not found"}
		},
	}

	deps := fastPathDepsForDisk(t, storageSvc)
	h := handlers.HandleDeleteDisk(deps)
	result, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(diskCID),
	}, jsonrpc.Context{})

	if err != nil {
		t.Fatalf("fast-path delete_disk NotFound idempotent: expected nil error, got: %v", err)
	}
	if result != nil {
		t.Errorf("fast-path delete_disk NotFound idempotent: expected nil result, got %v", result)
	}
}

// TestHandleDeleteDisk_SlowPath_AwaitsTask confirms that when fast_path_delete
// is OFF (default, nil), delete_disk awaits the UPID returned by
// DeleteVolumeAsync before returning. The mock returns a real UPID; if the
// fast path were active the handler would skip the await and awaitCalled would
// stay false. The slow path must call tasks.Wait for the UPID.
func TestHandleDeleteDisk_SlowPath_AwaitsTask(t *testing.T) {
	t.Parallel()

	const imgdelUPID = "UPID:pve1:00AABBCC:00112233:6789ABCD:imgdel:local-lvm:root@pam:"
	awaitCalled := false

	storageSvc := &mockStorageService{
		deleteVolumeAsyncFn: func(_ context.Context, _, _, _ string) (string, error) {
			// Return a real UPID — the slow path must await it.
			return imgdelUPID, nil
		},
	}
	tasksSvc := &mockTasksService{
		waitFn: func(_ context.Context, _, upid string, _ *tasks.WaitOptions) (*tasks.Status, error) {
			if upid == imgdelUPID {
				awaitCalled = true
			}
			return &tasks.Status{ExitStatus: "OK"}, nil
		},
	}
	client := &mockPVEClient{
		tasksSvc:   tasksSvc,
		storageSvc: storageSvc,
		clusterSvc: &mockClusterSvc{},
	}
	deps := handlers.Deps{
		Config: &config.CPIConfig{
			Node:        testNode,
			DiskStorage: storageName,
			// FastPathDelete is nil → off (slow path)
		},
		PVE:    client,
		Logger: log.NewNopLogger(),
	}

	h := handlers.HandleDeleteDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(diskCID),
	}, jsonrpc.Context{})

	if err != nil {
		t.Fatalf("slow-path delete_disk: unexpected error: %v", err)
	}
	if !awaitCalled {
		t.Error("slow-path delete_disk: tasks.Wait must be called for the imgdel UPID when fast_path_delete is OFF")
	}
}

// TestHandleDeleteDisk_WithoutClusterService verifies that delete_disk succeeds
// when the client is built without a populated cluster VMID list. The handler
// must not require the cluster service for volume deletion. newHandlerMockClientNoCluster
// wires an empty cluster list, confirming the delete path never touches VMID allocation.
func TestHandleDeleteDisk_WithoutClusterService(t *testing.T) {
	t.Parallel()

	type deleteVolumeCall struct {
		storage string
		volume  string
	}
	var deleteCalls []deleteVolumeCall

	storageSvc := &mockStorageService{
		deleteVolumeFn: func(_ context.Context, _, storage, volume string) error {
			deleteCalls = append(deleteCalls, deleteVolumeCall{storage, volume})
			return nil
		},
	}

	// newHandlerMockClientNoCluster explicitly passes an empty cluster VMID list,
	// asserting delete_disk works without any cluster state wired. The function
	// returns interface{} (the underlying value is a pve.Client); assert the type
	// so Deps.PVE can accept it.
	client := newHandlerMockClientNoCluster(storageSvc).(pve.Client)
	deps := handlers.Deps{
		Config: &config.CPIConfig{
			Node:        testNode,
			DiskStorage: storageName,
		},
		PVE:    client,
		Logger: log.NewNopLogger(),
	}

	h := handlers.HandleDeleteDisk(deps)
	result, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(diskCID),
	}, jsonrpc.Context{})

	if err != nil {
		t.Fatalf("unexpected error with no-cluster client: %v", err)
	}
	if result != nil {
		t.Errorf("expected nil result, got %v", result)
	}
	if len(deleteCalls) == 0 {
		t.Error("expected DeleteVolume to be called via no-cluster client")
	} else {
		if deleteCalls[0].storage != storageName {
			t.Errorf("DeleteVolume: want storage=%q, got %q", storageName, deleteCalls[0].storage)
		}
		if deleteCalls[0].volume != diskCID {
			t.Errorf("DeleteVolume: want volume=%q, got %q", diskCID, deleteCalls[0].volume)
		}
	}
}
