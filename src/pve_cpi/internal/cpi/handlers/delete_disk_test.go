package handlers_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"

	sdkclusterapi "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cluster"
	sdkerrors "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/errors"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
)

// baseDepsForDelete builds Deps for delete_disk tests.
func baseDepsForDelete(t *testing.T, storageSvc *mockStorageService) handlers.Deps {
	t.Helper()
	return handlers.Deps{
		Config: &config.CPIConfig{
			Node:        "pve1",
			DiskStorage: "local-lvm",
		},
		PVE:    newHandlerMockClient(storageSvc, nil),
		Logger: log.NewNopLogger(),
	}
}

// ---------------------------------------------------------------------------
// delete_disk tests
// ---------------------------------------------------------------------------

func TestHandleDeleteDisk_Happy(t *testing.T) {
	var deleteCalled bool
	storageSvc := &mockStorageService{
		deleteVolumeFn: func(_ context.Context, node, storage, volume string) error {
			deleteCalled = true
			if node != "pve1" {
				t.Errorf("unexpected node %q", node)
			}
			if storage != "local-lvm" {
				t.Errorf("unexpected storage %q", storage)
			}
			if volume != "local-lvm:vm-9001-disk-0" {
				t.Errorf("unexpected volume %q", volume)
			}
			return nil
		},
	}
	deps := baseDepsForDelete(t, storageSvc)

	h := handlers.HandleDeleteDisk(deps)
	result, err := h.Handle(context.Background(), []json.RawMessage{
		marshal("local-lvm:vm-9001-disk-0"),
	}, jsonrpc.Context{})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != nil {
		t.Errorf("expected nil result for void method, got %v", result)
	}
	if !deleteCalled {
		t.Error("expected DeleteVolume to be called")
	}
}

func TestHandleDeleteDisk_NotFound_Idempotent(t *testing.T) {
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
		marshal("local-lvm:vm-9001-disk-0"),
	}, jsonrpc.Context{})

	if err != nil {
		t.Fatalf("expected nil error for 404, got: %v", err)
	}
	if result != nil {
		t.Errorf("expected nil result, got %v", result)
	}
}

func TestHandleDeleteDisk_SDKError(t *testing.T) {
	storageSvc := &mockStorageService{
		deleteVolumeFn: func(_ context.Context, _, _, _ string) error {
			return errors.New("storage backend unavailable")
		},
	}
	deps := baseDepsForDelete(t, storageSvc)

	h := handlers.HandleDeleteDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{
		marshal("local-lvm:vm-9001-disk-0"),
	}, jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected error from DeleteVolume failure, got nil")
	}
}

func TestHandleDeleteDisk_MalformedCID_NoColon(t *testing.T) {
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
	storageSvc := &mockStorageService{}
	deps := baseDepsForDelete(t, storageSvc)

	h := handlers.HandleDeleteDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{}, jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected error for missing disk_cid argument")
	}
}

func TestHandleDeleteDisk_MissingNode(t *testing.T) {
	storageSvc := &mockStorageService{}
	deps := handlers.Deps{
		Config: &config.CPIConfig{
			Node:        "",
			DiskStorage: "local-lvm",
		},
		PVE:    newHandlerMockClient(storageSvc, nil),
		Logger: log.NewNopLogger(),
	}

	h := handlers.HandleDeleteDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{
		marshal("local-lvm:vm-9001-disk-0"),
	}, jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected error for missing node")
	}
}

func TestHandleDeleteDisk_EmptyStoragePart(t *testing.T) {
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
	// SDK already returns nil for 404 — most common production path.
	storageSvc := &mockStorageService{
		deleteVolumeFn: func(_ context.Context, _, _, _ string) error {
			return nil // SDK swallowed the 404
		},
	}
	deps := baseDepsForDelete(t, storageSvc)

	h := handlers.HandleDeleteDisk(deps)
	result, err := h.Handle(context.Background(), []json.RawMessage{
		marshal("local-lvm:vm-9001-disk-0"),
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
	const cid = "local-lvm:vm-9001-disk-0"
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
	if capturedStorage != "local-lvm" {
		t.Errorf("storage: got %q, want %q", capturedStorage, "local-lvm")
	}
	if capturedVolume != cid {
		t.Errorf("volume: got %q, want %q", capturedVolume, cid)
	}
}

func TestHandleDeleteDisk_LVMThin_CID(t *testing.T) {
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

// TODO(storage-network): nfs — wired to PVE network-call boundary, stubbed pending
// live shared-storage test infrastructure. Storage: nfs-store:9001/vm-9001-disk-0.qcow2. Re-enable when
// integration-test harness provides a nfs pool via env.
//
// func TestHandleDeleteDisk_NFS_CID(t *testing.T) { ... }

// TODO(storage-network): rbd — wired to PVE network-call boundary, stubbed pending
// live shared-storage test infrastructure. Storage: ceph-pool:vm-9001-disk-0. Re-enable when
// integration-test harness provides a rbd pool via env.
//
// func TestHandleDeleteDisk_RBD_CID(t *testing.T) { ... }

// TODO(storage-network): cephfs — wired to PVE network-call boundary, stubbed pending
// live shared-storage test infrastructure. Storage: cephfs-pool:vm-9001-disk-0. Re-enable when
// integration-test harness provides a cephfs pool via env.
//
// func TestHandleDeleteDisk_CephFS_CID(t *testing.T) { ... }

// TODO(storage-network): cifs — wired to PVE network-call boundary, stubbed pending
// live shared-storage test infrastructure. Storage: cifs-store:9001/vm-9001-disk-0.qcow2. Re-enable when
// integration-test harness provides a cifs pool via env.
//
// func TestHandleDeleteDisk_CIFS_CID(t *testing.T) { ... }

// Ensure cluster list not needed when using newHandlerMockClient.
// This validates the delete_disk handler does NOT call NextDiskVMID.
func TestHandleDeleteDisk_NoClusterCallExpected(t *testing.T) {
	clusterCalled := false
	listFn := func(_ context.Context, _ *sdkclusterapi.ListResourcesParams) (*sdkclusterapi.ListResourcesResponse, error) {
		clusterCalled = true
		resp := sdkclusterapi.ListResourcesResponse{}
		return &resp, nil
	}
	storageSvc := &mockStorageService{
		deleteVolumeFn: func(_ context.Context, _, _, _ string) error { return nil },
	}
	client := &handlerMockClient{
		storageSvc: storageSvc,
		clusterSvc: &mockClusterServiceForHandlers{listFn: listFn},
	}
	deps := handlers.Deps{
		Config: &config.CPIConfig{
			Node:        "pve1",
			DiskStorage: "local-lvm",
		},
		PVE:    client,
		Logger: log.NewNopLogger(),
	}

	h := handlers.HandleDeleteDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{
		marshal("local-lvm:vm-9001-disk-0"),
	}, jsonrpc.Context{})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if clusterCalled {
		t.Error("delete_disk must not call the cluster service (no VMID allocation needed)")
	}
}

// TestHandleDeleteDisk_WithoutClusterService verifies that delete_disk succeeds
// when the client is built without a populated cluster VMID list. The handler
// must not require the cluster service for volume deletion. newHandlerMockClientNoCluster
// wires an empty cluster list, confirming the delete path never touches VMID allocation.
func TestHandleDeleteDisk_WithoutClusterService(t *testing.T) {
	var deleteCalled bool
	storageSvc := &mockStorageService{
		deleteVolumeFn: func(_ context.Context, _, storage, volume string) error {
			deleteCalled = true
			if storage != "local-lvm" {
				t.Errorf("expected storage %q, got %q", "local-lvm", storage)
			}
			if volume != "local-lvm:vm-9001-disk-0" {
				t.Errorf("expected volume %q, got %q", "local-lvm:vm-9001-disk-0", volume)
			}
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
			Node:        "pve1",
			DiskStorage: "local-lvm",
		},
		PVE:    client,
		Logger: log.NewNopLogger(),
	}

	h := handlers.HandleDeleteDisk(deps)
	result, err := h.Handle(context.Background(), []json.RawMessage{
		marshal("local-lvm:vm-9001-disk-0"),
	}, jsonrpc.Context{})

	if err != nil {
		t.Fatalf("unexpected error with no-cluster client: %v", err)
	}
	if result != nil {
		t.Errorf("expected nil result, got %v", result)
	}
	if !deleteCalled {
		t.Error("expected DeleteVolume to be called via no-cluster client")
	}
}
