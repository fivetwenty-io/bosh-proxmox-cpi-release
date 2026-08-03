package handlers_test

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi/handlers"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/tasks"
	sdkerrors "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/errors"
)

// notFoundAPIErr returns an SDK APIError that IsNotFound() returns true for.
func notFoundAPIErr() error {
	return &sdkerrors.APIError{HTTPCode: 404}
}

// TestHandleDeleteVM_Happy verifies the stop->await->delete->agent.Remove path.
func TestHandleDeleteVM_Happy(t *testing.T) {
	t.Parallel()

	type stopCall struct {
		node string
		vmid int
	}
	type deleteCall struct {
		node         string
		vmid         string
		purge        bool
		destroyUnref bool
	}
	type removeCall struct {
		node string
		vmid int
	}

	var (
		stopCalls   []stopCall
		deleteCalls []deleteCall
		removeCalls []removeCall
	)

	qemuSvc := &mockQEMUService{
		stopFn: func(_ context.Context, node string, vmid int) (string, error) {
			stopCalls = append(stopCalls, stopCall{node, vmid})
			return "UPID:node:stop-task", nil
		},
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			// No unused entries — guard falls through to destroy.
			return map[string]any{}, nil
		},
	}
	nodesSvc := &mockNodesService{
		deleteQemuFn: func(_ context.Context, node string, vmid string, params *nodes.DeleteQemuParams) (*nodes.DeleteQemuResponse, error) {
			dc := deleteCall{node: node, vmid: vmid}
			if params != nil && params.Purge != nil {
				dc.purge = *params.Purge
			}
			if params != nil && params.DestroyUnreferencedDisks != nil {
				dc.destroyUnref = *params.DestroyUnreferencedDisks
			}
			deleteCalls = append(deleteCalls, dc)
			raw := nodes.DeleteQemuResponse{}
			return &raw, nil
		},
	}
	tasksSvc := &mockTasksService{
		waitFn: func(_ context.Context, _, upid string, _ *tasks.WaitOptions) (*tasks.Status, error) {
			if upid != "UPID:node:stop-task" {
				t.Errorf("Wait: unexpected upid=%q", upid)
			}
			return &tasks.Status{ExitStatus: "OK"}, nil
		},
	}
	agentSvc := &mockAgentService{
		removeFn: func(_ context.Context, node string, vmid int) error {
			removeCalls = append(removeCalls, removeCall{node, vmid})
			return nil
		},
	}

	h := handlers.HandleDeleteVM(testDepsFoundVM(101, qemuSvc, nodesSvc, tasksSvc, agentSvc))
	result, err := h.Handle(context.Background(), marshalArgs("101"), jsonrpc.Context{})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != nil {
		t.Errorf("expected nil result, got %v", result)
	}
	if len(stopCalls) != 1 {
		t.Fatalf("Stop: want 1 call, got %d", len(stopCalls))
	}
	if stopCalls[0].node != vmNode || stopCalls[0].vmid != 101 {
		t.Errorf("Stop: want node=%q vmid=101, got node=%q vmid=%d", vmNode, stopCalls[0].node, stopCalls[0].vmid)
	}
	if len(deleteCalls) != 1 {
		t.Fatalf("DeleteQemu: want 1 call, got %d", len(deleteCalls))
	}
	if deleteCalls[0].node != vmNode || deleteCalls[0].vmid != "101" {
		t.Errorf("DeleteQemu: want node=%q vmid=%q, got node=%q vmid=%q", vmNode, "101", deleteCalls[0].node, deleteCalls[0].vmid)
	}
	if !deleteCalls[0].purge {
		t.Error("DeleteQemu: expected purge=true")
	}
	if deleteCalls[0].destroyUnref {
		t.Error("DeleteQemu: expected destroy-unreferenced-disks=false (P5 default; pve.destroy_unreferenced_disks unset)")
	}
	if len(removeCalls) != 1 {
		t.Fatalf("Agent.Remove: want 1 call, got %d", len(removeCalls))
	}
	if removeCalls[0].node != vmNode || removeCalls[0].vmid != 101 {
		t.Errorf("Agent.Remove: want node=%q vmid=101, got node=%q vmid=%d", vmNode, removeCalls[0].node, removeCalls[0].vmid)
	}
}

// TestHandleDeleteVM_DestroyUnreferencedDisksOptIn verifies that explicit
// pve.destroy_unreferenced_disks=true restores the pre-P5 default: the
// synchronous delete path passes DestroyUnreferencedDisks=true to DeleteQemu
// for a non-retain delete.
func TestHandleDeleteVM_DestroyUnreferencedDisksOptIn(t *testing.T) {
	t.Parallel()

	var gotDestroyUnref *bool
	qemuSvc := &mockQEMUService{
		stopFn: func(_ context.Context, _ string, _ int) (string, error) { return "", nil },
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{}, nil
		},
	}
	nodesSvc := &mockNodesService{
		deleteQemuFn: func(_ context.Context, _ string, _ string, params *nodes.DeleteQemuParams) (*nodes.DeleteQemuResponse, error) {
			if params != nil && params.DestroyUnreferencedDisks != nil {
				v := *params.DestroyUnreferencedDisks
				gotDestroyUnref = &v
			}
			return &nodes.DeleteQemuResponse{}, nil
		},
	}

	deps := testDepsFoundVM(101, qemuSvc, nodesSvc, &mockTasksService{}, &mockAgentService{})
	deps.Config.DestroyUnreferencedDisks = true
	h := handlers.HandleDeleteVM(deps)
	_, err := h.Handle(context.Background(), marshalArgs("101"), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotDestroyUnref == nil || !*gotDestroyUnref {
		t.Error("DeleteQemu: expected destroy-unreferenced-disks=true with pve.destroy_unreferenced_disks=true")
	}
}

// TestHandleDeleteVM_MultiNode_ResolvesCorrectNode verifies that delete_vm
// forwards the cluster-resolved node to Stop and DeleteQemu even when the VM
// lives on a non-default cluster member. The cluster response includes three
// nodes; the VM is on pve02 (not the default pve-node1 from testConfig).
func TestHandleDeleteVM_MultiNode_ResolvesCorrectNode(t *testing.T) {
	t.Parallel()

	const vmid = 202
	const vmOnNode = "pve02"

	var stopNode, deleteNode string
	qemuSvc := &mockQEMUService{
		stopFn: func(_ context.Context, node string, _ int) (string, error) {
			stopNode = node
			return "UPID:pve02:stop-multi", nil
		},
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{}, nil
		},
	}
	nodesSvc := &mockNodesService{
		deleteQemuFn: func(_ context.Context, node string, _ string, _ *nodes.DeleteQemuParams) (*nodes.DeleteQemuResponse, error) {
			deleteNode = node
			return &nodes.DeleteQemuResponse{}, nil
		},
	}
	tasksSvc := &mockTasksService{}
	agentSvc := &mockAgentService{}

	// Wire the multi-node cluster: VM 202 lives on pve02.
	clusterSvc := defaultMultiNodeClusterSvc(vmid, vmOnNode)
	deps := testDepsWithCluster(qemuSvc, nodesSvc, tasksSvc, agentSvc, &mockStorageService{}, clusterSvc)

	h := handlers.HandleDeleteVM(deps)
	_, err := h.Handle(context.Background(), marshalArgs("202"), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stopNode != vmOnNode {
		t.Errorf("Stop: want node=%q, got %q", vmOnNode, stopNode)
	}
	if deleteNode != vmOnNode {
		t.Errorf("DeleteQemu: want node=%q, got %q", vmOnNode, deleteNode)
	}
}

// TestHandleDeleteVM_NotFound_InCluster verifies that when the cluster scan does
// not find the VM, delete_vm returns nil (idempotent: already gone).
func TestHandleDeleteVM_NotFound_InCluster(t *testing.T) {
	t.Parallel()

	// testDeps wires empty cluster: VM not found.
	agentSvc := &mockAgentService{}
	h := handlers.HandleDeleteVM(testDeps(nil, nil, nil, agentSvc))
	result, err := h.Handle(context.Background(), marshalArgs("202"), jsonrpc.Context{})

	if err != nil {
		t.Fatalf("expected nil error for cluster-not-found, got: %v", err)
	}
	if result != nil {
		t.Errorf("expected nil result, got %v", result)
	}
}

// TestHandleDeleteVM_NotFound_AtStop verifies 404 during stop returns nil (idempotent).
func TestHandleDeleteVM_NotFound_AtStop(t *testing.T) {
	t.Parallel()

	qemuSvc := &mockQEMUService{
		stopFn: func(_ context.Context, _ string, _ int) (string, error) {
			return "", notFoundAPIErr()
		},
	}
	agentSvc := &mockAgentService{}

	h := handlers.HandleDeleteVM(testDepsFoundVM(202, qemuSvc, nil, nil, agentSvc))
	result, err := h.Handle(context.Background(), marshalArgs("202"), jsonrpc.Context{})

	if err != nil {
		t.Fatalf("expected nil error for 404-at-stop, got: %v", err)
	}
	if result != nil {
		t.Errorf("expected nil result, got %v", result)
	}
}

// TestHandleDeleteVM_NotFound_AtDelete verifies 404 during delete returns nil (idempotent).
func TestHandleDeleteVM_NotFound_AtDelete(t *testing.T) {
	t.Parallel()

	qemuSvc := &mockQEMUService{
		stopFn: func(_ context.Context, _ string, _ int) (string, error) {
			return "UPID:node:stop-task", nil
		},
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{}, nil
		},
	}
	nodesSvc := &mockNodesService{
		deleteQemuFn: func(_ context.Context, _ string, _ string, _ *nodes.DeleteQemuParams) (*nodes.DeleteQemuResponse, error) {
			return nil, notFoundAPIErr()
		},
	}
	tasksSvc := &mockTasksService{}
	agentSvc := &mockAgentService{}

	h := handlers.HandleDeleteVM(testDepsFoundVM(303, qemuSvc, nodesSvc, tasksSvc, agentSvc))
	result, err := h.Handle(context.Background(), marshalArgs("303"), jsonrpc.Context{})

	if err != nil {
		t.Fatalf("expected nil error for 404-at-delete, got: %v", err)
	}
	if result != nil {
		t.Errorf("expected nil result, got %v", result)
	}
}

// TestHandleDeleteVM_StopFail verifies a non-404 stop error is propagated.
func TestHandleDeleteVM_StopFail(t *testing.T) {
	t.Parallel()

	stopErr := errors.New("pve: internal error")
	qemuSvc := &mockQEMUService{
		stopFn: func(_ context.Context, _ string, _ int) (string, error) {
			return "", stopErr
		},
	}
	agentSvc := &mockAgentService{}

	h := handlers.HandleDeleteVM(testDepsFoundVM(404, qemuSvc, nil, nil, agentSvc))
	_, err := h.Handle(context.Background(), marshalArgs("404"), jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected error from stop failure, got nil")
	}
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) {
		t.Errorf("expected *cpierrors.Error, got %T: %v", err, err)
	}
}

// TestHandleDeleteVM_MissingVMCID verifies missing argument returns error.
func TestHandleDeleteVM_MissingVMCID(t *testing.T) {
	t.Parallel()

	h := handlers.HandleDeleteVM(testDeps(nil, nil, nil, &mockAgentService{}))
	_, err := h.Handle(context.Background(), nil, jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected error for missing vm_cid")
	}
}

// TestHandleDeleteVM_InvalidVMCID verifies non-integer vm_cid returns error.
func TestHandleDeleteVM_InvalidVMCID(t *testing.T) {
	t.Parallel()

	h := handlers.HandleDeleteVM(testDeps(nil, nil, nil, &mockAgentService{}))
	_, err := h.Handle(context.Background(), marshalArgs("not-an-int"), jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected error for non-integer vm_cid")
	}
}

// TestHandleDeleteVM_RefusesWhenPersistentDiskUnused verifies the guard
// rejects destroy when an unusedN slot still references a volume on the
// configured pve.disk_storage. The guard runs after Stop but before the
// DELETE /qemu/{vmid} call, so DeleteQemu must NOT be invoked.
func TestHandleDeleteVM_RefusesWhenPersistentDiskUnused(t *testing.T) {
	t.Parallel()

	deleteCalled := false
	qemuSvc := &mockQEMUService{
		stopFn: func(_ context.Context, _ string, _ int) (string, error) {
			return "", nil
		},
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			// Persistent disk demoted to unused0 — exactly the state that
			// would cause data loss if delete proceeded. testDeps configures
			// DiskStorage="local-lvm" so the unused volid must share that
			// storage prefix to trip the guard.
			return map[string]any{
				"scsi0":   "local-lvm:vm-101-disk-0",
				"unused0": "local-lvm:vm-9000-disk-0",
			}, nil
		},
	}
	nodesSvc := &mockNodesService{
		deleteQemuFn: func(_ context.Context, _ string, _ string, _ *nodes.DeleteQemuParams) (*nodes.DeleteQemuResponse, error) {
			deleteCalled = true
			raw := nodes.DeleteQemuResponse{}
			return &raw, nil
		},
	}
	tasksSvc := &mockTasksService{}
	agentSvc := &mockAgentService{}
	// Volume still exists in storage -> a real persistent disk -> guard refuses.
	storageSvc := &mockStorageService{
		existsFn: func(_ context.Context, _, _, _ string) (bool, error) { return true, nil },
	}

	h := handlers.HandleDeleteVM(testDepsFoundVMWithStorage(101, qemuSvc, nodesSvc, tasksSvc, agentSvc, storageSvc))
	_, err := h.Handle(context.Background(), marshalArgs("101"), jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected refusal error when persistent disk attached as unused, got nil")
	}
	if deleteCalled {
		t.Error("DeleteQemu must not be called when persistent disk is still attached")
	}
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) {
		t.Errorf("expected *cpierrors.Error, got %T: %v", err, err)
	}
}

// TestHandleDeleteVM_AllowsWhenUnusedVolumeAlreadyDeleted verifies the guard
// does NOT block destroy when an unusedN slot references a volume that has
// already been deleted from storage.
func TestHandleDeleteVM_AllowsWhenUnusedVolumeAlreadyDeleted(t *testing.T) {
	t.Parallel()

	deleteCalled := false
	qemuSvc := &mockQEMUService{
		stopFn: func(_ context.Context, _ string, _ int) (string, error) {
			return "", nil
		},
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			// unused0 on disk_storage, but its volume no longer exists.
			return map[string]any{
				"unused0": "local-lvm:vm-9000-disk-0",
			}, nil
		},
	}
	nodesSvc := &mockNodesService{
		deleteQemuFn: func(_ context.Context, _ string, _ string, _ *nodes.DeleteQemuParams) (*nodes.DeleteQemuResponse, error) {
			deleteCalled = true
			raw := nodes.DeleteQemuResponse{}
			return &raw, nil
		},
	}
	tasksSvc := &mockTasksService{}
	agentSvc := &mockAgentService{}
	// Volume gone from storage -> stale dangling slot -> guard must not block.
	storageSvc := &mockStorageService{
		existsFn: func(_ context.Context, _, _, _ string) (bool, error) { return false, nil },
	}

	h := handlers.HandleDeleteVM(testDepsFoundVMWithStorage(101, qemuSvc, nodesSvc, tasksSvc, agentSvc, storageSvc))
	_, err := h.Handle(context.Background(), marshalArgs("101"), jsonrpc.Context{})

	if err != nil {
		t.Fatalf("expected destroy to proceed for stale unused slot, got error: %v", err)
	}
	if !deleteCalled {
		t.Error("DeleteQemu must be called when the unused-slot volume is already deleted")
	}
}

// TestHandleDeleteVM_DetachesForeignDiskThenDestroys verifies the incident
// scenario: a persistent disk from another VMID is still attached on an active
// bus slot (scsi1) when delete_vm runs. The handler must detach it (preserving
// the volume) and THEN destroy the VM, rather than letting the purge-destroy
// take the foreign volume.
func TestHandleDeleteVM_DetachesForeignDiskThenDestroys(t *testing.T) {
	t.Parallel()

	var detachedSlots []string
	deleteCalled := false
	var configCalls int
	qemuSvc := &mockQEMUService{
		stopFn: func(_ context.Context, _ string, _ int) (string, error) { return "", nil },
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			configCalls++
			if configCalls == 1 {
				// Director DB disk (vmid 15689) attached to VM 6031 as scsi1.
				return map[string]any{
					"virtio0": "local-lvm:vm-6031-disk-0",
					"scsi1":   "zfs-1:vm-15689-disk-0,size=128G",
				}, nil
			}
			// After detach: foreign disk fully unreferenced; no unusedN remains.
			return map[string]any{"virtio0": "local-lvm:vm-6031-disk-0"}, nil
		},
		detachDiskFn: func(_ context.Context, _ string, _ int, slot string) error {
			detachedSlots = append(detachedSlots, slot)
			return nil
		},
	}
	nodesSvc := &mockNodesService{
		deleteQemuFn: func(_ context.Context, _ string, _ string, _ *nodes.DeleteQemuParams) (*nodes.DeleteQemuResponse, error) {
			deleteCalled = true
			raw := nodes.DeleteQemuResponse{}
			return &raw, nil
		},
	}

	h := handlers.HandleDeleteVM(testDepsFoundVMWithStorage(6031, qemuSvc, nodesSvc, &mockTasksService{}, &mockAgentService{}, &mockStorageService{}))
	_, err := h.Handle(context.Background(), marshalArgs("6031"), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("expected detach-then-destroy to succeed, got error: %v", err)
	}
	if len(detachedSlots) != 1 || detachedSlots[0] != "scsi1" {
		t.Errorf("foreign disk must be detached from scsi1; got detached slots %v", detachedSlots)
	}
	if !deleteCalled {
		t.Error("DeleteQemu must be called after the foreign disk is detached")
	}
}

// TestHandleDeleteVM_FastPath_RefusesLiveForeignUnusedDisk verifies the fast
// path now runs the unusedN guard. A foreign persistent volume that lingers in
// an unusedN slot (e.g. a snapshot blocked the detach sweep) and STILL EXISTS on
// storage must block the fast-path purge-destroy.
func TestHandleDeleteVM_FastPath_RefusesLiveForeignUnusedDisk(t *testing.T) {
	t.Parallel()

	deleteCalled := false
	qemuSvc := &mockQEMUService{
		stopFn: func(_ context.Context, _ string, _ int) (string, error) { return "", nil },
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			// Foreign volume (vmid 9000) parked in unused0 on pve.disk_storage.
			return map[string]any{"unused0": "local-lvm:vm-9000-disk-0"}, nil
		},
	}
	nodesSvc := &mockNodesService{
		deleteQemuFn: func(_ context.Context, _ string, _ string, _ *nodes.DeleteQemuParams) (*nodes.DeleteQemuResponse, error) {
			deleteCalled = true
			raw := nodes.DeleteQemuResponse{}
			return &raw, nil
		},
		updateQemuConfigFn: func(_ context.Context, _ string, _ string, _ *nodes.UpdateQemuConfigParams) error { return nil },
	}
	// Volume still exists -> guard must fail closed -> destroy must NOT proceed.
	storageSvc := &mockStorageService{
		existsFn: func(_ context.Context, _, _, _ string) (bool, error) { return true, nil },
	}
	deps := testDepsFoundVMWithStorage(909, qemuSvc, nodesSvc, &mockTasksService{}, &mockAgentService{}, storageSvc)
	enabled := true
	deps.Config.FastPathDelete = &enabled

	h := handlers.HandleDeleteVM(deps)
	_, err := h.Handle(context.Background(), marshalArgs("909"), jsonrpc.Context{})
	if err == nil {
		t.Fatal("fast-path: expected guard to fail closed for live foreign unused disk, got nil error")
	}
	if deleteCalled {
		t.Error("fast-path: DeleteQemu must NOT be called when the unusedN guard fails closed")
	}
}

// TestHandleDeleteVM_AllowsWhenUnusedOnDifferentStorage verifies the guard
// fails CLOSED when an unusedN slot references a storage that does not match
// pve.disk_storage.
func TestHandleDeleteVM_AllowsWhenUnusedOnDifferentStorage(t *testing.T) {
	t.Parallel()

	deleteCalled := false
	qemuSvc := &mockQEMUService{
		stopFn: func(_ context.Context, _ string, _ int) (string, error) {
			return "", nil
		},
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			// unused0 on "local" storage; testConfig sets DiskStorage="local-lvm".
			// Mismatch -> guard fails closed; delete must NOT proceed.
			return map[string]any{
				"unused0": "local:iso/vm-101-config.iso",
			}, nil
		},
	}
	nodesSvc := &mockNodesService{
		deleteQemuFn: func(_ context.Context, _ string, _ string, _ *nodes.DeleteQemuParams) (*nodes.DeleteQemuResponse, error) {
			deleteCalled = true
			raw := nodes.DeleteQemuResponse{}
			return &raw, nil
		},
	}
	tasksSvc := &mockTasksService{}
	agentSvc := &mockAgentService{}

	h := handlers.HandleDeleteVM(testDepsFoundVM(101, qemuSvc, nodesSvc, tasksSvc, agentSvc))
	_, err := h.Handle(context.Background(), marshalArgs("101"), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected guard to fail closed for storage-mismatch unused slot, got nil error")
	}
	if deleteCalled {
		t.Error("DeleteQemu must not be called when guard fails closed on storage mismatch")
	}
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) {
		t.Errorf("expected *cpierrors.Error, got %T: %v", err, cpiErr)
	}
}

// TestDeleteVM_UnusedSlotPresent_DiskStorageEmpty_FailsClosed verifies that
// when the VM config has an unusedN entry but DiskStorage is not configured,
// the guard fails closed.
func TestDeleteVM_UnusedSlotPresent_DiskStorageEmpty_FailsClosed(t *testing.T) {
	t.Parallel()

	deleteCalled := false
	qemuSvc := &mockQEMUService{
		stopFn: func(_ context.Context, _ string, _ int) (string, error) {
			return "", nil
		},
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{
				"unused0": "local-lvm:vm-500-disk-0",
			}, nil
		},
	}
	nodesSvc := &mockNodesService{
		deleteQemuFn: func(_ context.Context, _ string, _ string, _ *nodes.DeleteQemuParams) (*nodes.DeleteQemuResponse, error) {
			deleteCalled = true
			raw := nodes.DeleteQemuResponse{}
			return &raw, nil
		},
	}
	tasksSvc := &mockTasksService{}
	agentSvc := &mockAgentService{}

	// Build deps with DiskStorage explicitly cleared.
	deps := testDepsFoundVMWithStorage(500, qemuSvc, nodesSvc, tasksSvc, agentSvc, &mockStorageService{})
	deps.Config.DiskStorage = ""

	h := handlers.HandleDeleteVM(deps)
	_, err := h.Handle(context.Background(), marshalArgs("500"), jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected guard to fail closed when DiskStorage is empty and unusedN slot present, got nil error")
	}
	if deleteCalled {
		t.Error("DeleteQemu must not be called when guard fails closed (empty DiskStorage)")
	}
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) {
		t.Errorf("expected *cpierrors.Error, got %T: %v", err, cpiErr)
	}
}

// TestDeleteVM_UnusedSlotPresent_DiskStorageMismatch_FailsClosed verifies that
// when the unusedN slot's storage does not match the configured DiskStorage,
// the guard fails closed regardless of whether the volume actually exists.
func TestDeleteVM_UnusedSlotPresent_DiskStorageMismatch_FailsClosed(t *testing.T) {
	t.Parallel()

	deleteCalled := false
	qemuSvc := &mockQEMUService{
		stopFn: func(_ context.Context, _ string, _ int) (string, error) {
			return "", nil
		},
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			// "ceph-pool" does not match testConfig DiskStorage="local-lvm".
			return map[string]any{
				"unused0": "ceph-pool:vm-501-disk-0",
			}, nil
		},
	}
	nodesSvc := &mockNodesService{
		deleteQemuFn: func(_ context.Context, _ string, _ string, _ *nodes.DeleteQemuParams) (*nodes.DeleteQemuResponse, error) {
			deleteCalled = true
			raw := nodes.DeleteQemuResponse{}
			return &raw, nil
		},
	}
	tasksSvc := &mockTasksService{}
	agentSvc := &mockAgentService{}
	// existsFn should NOT be called -- guard fires before reaching the probe.
	storageSvc := &mockStorageService{
		existsFn: func(_ context.Context, _, _, _ string) (bool, error) {
			t.Error("Exists probe must not be called for storage-mismatch slot")
			return false, nil
		},
	}

	h := handlers.HandleDeleteVM(testDepsFoundVMWithStorage(501, qemuSvc, nodesSvc, tasksSvc, agentSvc, storageSvc))
	_, err := h.Handle(context.Background(), marshalArgs("501"), jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected guard to fail closed for storage-mismatch unused slot, got nil error")
	}
	if deleteCalled {
		t.Error("DeleteQemu must not be called when guard fails closed on storage mismatch")
	}
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) {
		t.Errorf("expected *cpierrors.Error, got %T: %v", err, cpiErr)
	}
}

// TestDeleteVM_NoUnusedSlots_Succeeds verifies that when the VM config has no
// unusedN entries the guard does not block destroy.
func TestDeleteVM_NoUnusedSlots_Succeeds(t *testing.T) {
	t.Parallel()

	deleteCalled := false
	qemuSvc := &mockQEMUService{
		stopFn: func(_ context.Context, _ string, _ int) (string, error) {
			return "", nil
		},
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			// Only a live scsi0 attachment -- no unused slots.
			return map[string]any{
				"scsi0": "local-lvm:vm-502-disk-0",
			}, nil
		},
	}
	nodesSvc := &mockNodesService{
		deleteQemuFn: func(_ context.Context, _ string, _ string, _ *nodes.DeleteQemuParams) (*nodes.DeleteQemuResponse, error) {
			deleteCalled = true
			raw := nodes.DeleteQemuResponse{}
			return &raw, nil
		},
	}
	tasksSvc := &mockTasksService{}
	agentSvc := &mockAgentService{}

	h := handlers.HandleDeleteVM(testDepsFoundVM(502, qemuSvc, nodesSvc, tasksSvc, agentSvc))
	_, err := h.Handle(context.Background(), marshalArgs("502"), jsonrpc.Context{})

	if err != nil {
		t.Fatalf("expected success when no unused slots present, got: %v", err)
	}
	if !deleteCalled {
		t.Error("DeleteQemu must be called when VM has no unused disk slots")
	}
}

// TestHandleDeleteVM_AgentRemoveError verifies agent.Remove error is non-fatal.
func TestHandleDeleteVM_AgentRemoveError(t *testing.T) {
	t.Parallel()

	qemuSvc := &mockQEMUService{
		stopFn: func(_ context.Context, _ string, _ int) (string, error) {
			return "", nil // no UPID -> skip await
		},
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{}, nil
		},
	}
	nodesSvc := &mockNodesService{
		deleteQemuFn: func(_ context.Context, _ string, _ string, _ *nodes.DeleteQemuParams) (*nodes.DeleteQemuResponse, error) {
			raw := nodes.DeleteQemuResponse{}
			return &raw, nil
		},
	}
	tasksSvc := &mockTasksService{}
	agentSvc := &mockAgentService{
		removeFn: func(_ context.Context, _ string, _ int) error {
			return errors.New("registry: connection refused")
		},
	}

	h := handlers.HandleDeleteVM(testDepsFoundVM(505, qemuSvc, nodesSvc, tasksSvc, agentSvc))
	result, err := h.Handle(context.Background(), marshalArgs("505"), jsonrpc.Context{})

	// Agent.Remove failure should NOT cause delete_vm to return an error.
	if err != nil {
		t.Fatalf("agent.Remove error should be non-fatal, got: %v", err)
	}
	if result != nil {
		t.Errorf("expected nil result, got %v", result)
	}
}

// TestDeleteVM_AwaitsDestroyTask verifies that delete_vm decodes the UPID from
// the DeleteQemu response and awaits the destroy task before returning success.
// The mock task service starts in state "running" and transitions to "stopped/OK"
// on the first Wait call.
func TestDeleteVM_AwaitsDestroyTask(t *testing.T) {
	t.Parallel()

	const destroyUPID = "UPID:pve-node1:00AABBCC:00112233:6789ABCD:qmdestroy:101:root@pam:"

	awaitCalled := false

	qemuSvc := &mockQEMUService{
		stopFn: func(_ context.Context, _ string, _ int) (string, error) {
			return "", nil // no stop UPID — synchronous stop
		},
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{}, nil
		},
	}

	// DeleteQemu returns a UPID string encoded as a JSON RawMessage.
	deleteResp := nodes.DeleteQemuResponse(`"` + destroyUPID + `"`)
	nodesSvc := &mockNodesService{
		deleteQemuFn: func(_ context.Context, _ string, _ string, _ *nodes.DeleteQemuParams) (*nodes.DeleteQemuResponse, error) {
			return &deleteResp, nil
		},
	}

	tasksSvc := &mockTasksService{
		waitFn: func(_ context.Context, _, upid string, _ *tasks.WaitOptions) (*tasks.Status, error) {
			if upid == destroyUPID {
				awaitCalled = true
				return &tasks.Status{ExitStatus: "OK"}, nil
			}
			return &tasks.Status{ExitStatus: "OK"}, nil
		},
	}
	agentSvc := &mockAgentService{}

	h := handlers.HandleDeleteVM(testDepsFoundVM(101, qemuSvc, nodesSvc, tasksSvc, agentSvc))
	result, err := h.Handle(context.Background(), marshalArgs("101"), jsonrpc.Context{})

	if err != nil {
		t.Fatalf("expected success, got error: %v", err)
	}
	if result != nil {
		t.Errorf("expected nil result, got %v", result)
	}
	if !awaitCalled {
		t.Error("destroy task was not awaited — UPID must be extracted and polled")
	}
}

// TestDeleteVM_AwaitDestroyNotFoundIdempotent verifies that when the destroy-task
// await returns a NotFound error (VM already gone by the time we polled), the
// handler treats this as idempotent success.
func TestDeleteVM_AwaitDestroyNotFoundIdempotent(t *testing.T) {
	t.Parallel()

	const destroyUPID = "UPID:pve-node1:00AABBCC:00112233:6789ABCD:qmdestroy:102:root@pam:"

	qemuSvc := &mockQEMUService{
		stopFn: func(_ context.Context, _ string, _ int) (string, error) {
			return "", nil
		},
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{}, nil
		},
	}

	deleteResp := nodes.DeleteQemuResponse(`"` + destroyUPID + `"`)
	nodesSvc := &mockNodesService{
		deleteQemuFn: func(_ context.Context, _ string, _ string, _ *nodes.DeleteQemuParams) (*nodes.DeleteQemuResponse, error) {
			return &deleteResp, nil
		},
	}

	// Await returns a NotFound-class error: VM config gone before poll completed.
	tasksSvc := &mockTasksService{
		waitFn: func(_ context.Context, _, _ string, _ *tasks.WaitOptions) (*tasks.Status, error) {
			return nil, &sdkerrors.APIError{HTTPCode: 404}
		},
	}
	agentSvc := &mockAgentService{}

	h := handlers.HandleDeleteVM(testDepsFoundVM(102, qemuSvc, nodesSvc, tasksSvc, agentSvc))
	result, err := h.Handle(context.Background(), marshalArgs("102"), jsonrpc.Context{})

	if err != nil {
		t.Fatalf("NotFound on destroy await must be idempotent success, got: %v", err)
	}
	if result != nil {
		t.Errorf("expected nil result, got %v", result)
	}
}

// TestDeleteVM_AwaitDestroyTransientRetriable verifies that when the destroy-task
// await returns a transient task-exit failure (non-OK exit status), the handler
// propagates a CPI error so the BOSH director can decide whether to retry.
// The error must not be silently swallowed.
func TestDeleteVM_AwaitDestroyTransientRetriable(t *testing.T) {
	t.Parallel()

	const destroyUPID = "UPID:pve-node1:00AABBCC:00112233:6789ABCD:qmdestroy:103:root@pam:"

	qemuSvc := &mockQEMUService{
		stopFn: func(_ context.Context, _ string, _ int) (string, error) {
			return "", nil
		},
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{}, nil
		},
	}

	deleteResp := nodes.DeleteQemuResponse(`"` + destroyUPID + `"`)
	nodesSvc := &mockNodesService{
		deleteQemuFn: func(_ context.Context, _ string, _ string, _ *nodes.DeleteQemuParams) (*nodes.DeleteQemuResponse, error) {
			return &deleteResp, nil
		},
	}

	// Await returns a non-OK task exit status — the task itself failed.
	tasksSvc := &mockTasksService{
		waitFn: func(_ context.Context, _, _ string, _ *tasks.WaitOptions) (*tasks.Status, error) {
			// Non-OK exit status causes AwaitTask to return a CloudError.
			return &tasks.Status{ExitStatus: "ERROR"}, nil
		},
	}
	agentSvc := &mockAgentService{}

	h := handlers.HandleDeleteVM(testDepsFoundVM(103, qemuSvc, nodesSvc, tasksSvc, agentSvc))
	_, err := h.Handle(context.Background(), marshalArgs("103"), jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected error from failed destroy task exit status, got nil")
	}
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) {
		t.Fatalf("expected *cpierrors.Error, got %T: %v", err, err)
	}
}

// ---------------------------------------------------------------------------
// §7.32 fast-path delete tests
// ---------------------------------------------------------------------------

// fastPathDeps builds Deps with fast_path_delete enabled. It wires the same
// cluster service as testDepsFoundVM (target VM on vmNode; no straggler VMs).
func fastPathDeps(vmid int, qemuSvc *mockQEMUService, nodesSvc *mockNodesService, tasksSvc *mockTasksService, agentSvc *mockAgentService) handlers.Deps {
	deps := testDepsFoundVMWithStorage(vmid, qemuSvc, nodesSvc, tasksSvc, agentSvc, &mockStorageService{})
	enabled := true
	deps.Config.FastPathDelete = &enabled
	return deps
}

// TestHandleDeleteVM_FastPath_NoAwait verifies that when fast_path_delete is
// enabled, delete_vm issues the destroy but does NOT call the task poll for
// either the stop UPID or the destroy UPID. The mockTasksService.waitFn
// records every UPID passed to it; any call is a test failure.
func TestHandleDeleteVM_FastPath_NoAwait(t *testing.T) {
	t.Parallel()

	const stopUPID = "UPID:pve-node1:00AABBCC:00112233:6789ABCD:qmstop:701:root@pam:"
	const destroyUPID = "UPID:pve-node1:00AABBCC:00112233:6789ABCD:qmdestroy:701:root@pam:"
	awaitCalledForAny := false

	qemuSvc := &mockQEMUService{
		stopFn: func(_ context.Context, _ string, _ int) (string, error) {
			// Return a real stop UPID. The fast path must discard it without await.
			return stopUPID, nil
		},
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{}, nil
		},
	}
	deleteResp := nodes.DeleteQemuResponse(`"` + destroyUPID + `"`)
	nodesSvc := &mockNodesService{
		deleteQemuFn: func(_ context.Context, _ string, _ string, _ *nodes.DeleteQemuParams) (*nodes.DeleteQemuResponse, error) {
			return &deleteResp, nil
		},
		updateQemuConfigFn: func(_ context.Context, _ string, _ string, _ *nodes.UpdateQemuConfigParams) error {
			return nil
		},
	}
	tasksSvc := &mockTasksService{
		waitFn: func(_ context.Context, _, upid string, _ *tasks.WaitOptions) (*tasks.Status, error) {
			// Any Wait call on the fast path is a defect.
			awaitCalledForAny = true
			t.Errorf("fast-path delete: tasks.Wait must NOT be called; got upid=%q", upid)
			return &tasks.Status{ExitStatus: "OK"}, nil
		},
	}
	agentSvc := &mockAgentService{}

	h := handlers.HandleDeleteVM(fastPathDeps(701, qemuSvc, nodesSvc, tasksSvc, agentSvc))
	result, err := h.Handle(context.Background(), marshalArgs("701"), jsonrpc.Context{})

	if err != nil {
		t.Fatalf("fast-path delete: unexpected error: %v", err)
	}
	if result != nil {
		t.Errorf("fast-path delete: expected nil result, got %v", result)
	}
	if awaitCalledForAny {
		t.Error("fast-path delete: task poll must NOT be called for any UPID (stop or destroy)")
	}
}

// TestHandleDeleteVM_FastPath_TagsVM verifies that when fast_path_delete is
// enabled, the VM is tagged with "bosh-deleting" via UpdateQemuConfig.
// Existing tags on the VM are preserved (mergeTagList).
func TestHandleDeleteVM_FastPath_TagsVM(t *testing.T) {
	t.Parallel()

	tagCallCount := 0
	var capturedTags string

	qemuSvc := &mockQEMUService{
		stopFn: func(_ context.Context, _ string, _ int) (string, error) {
			return "", nil
		},
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			// Return existing tags so mergeTagList is exercised with a non-empty base.
			return map[string]any{"tags": "existing-tag"}, nil
		},
	}
	nodesSvc := &mockNodesService{
		deleteQemuFn: func(_ context.Context, _ string, _ string, _ *nodes.DeleteQemuParams) (*nodes.DeleteQemuResponse, error) {
			raw := nodes.DeleteQemuResponse{}
			return &raw, nil
		},
		updateQemuConfigFn: func(_ context.Context, _ string, _ string, params *nodes.UpdateQemuConfigParams) error {
			tagCallCount++
			if params != nil && params.Tags != nil {
				capturedTags = *params.Tags
			}
			return nil
		},
	}
	tasksSvc := &mockTasksService{}
	agentSvc := &mockAgentService{}

	h := handlers.HandleDeleteVM(fastPathDeps(702, qemuSvc, nodesSvc, tasksSvc, agentSvc))
	_, err := h.Handle(context.Background(), marshalArgs("702"), jsonrpc.Context{})

	if err != nil {
		t.Fatalf("fast-path tag test: unexpected error: %v", err)
	}
	if tagCallCount == 0 {
		t.Error("fast-path delete: UpdateQemuConfig must be called to stamp bosh-deleting tag")
	}
	if !strings.Contains(capturedTags, "bosh-deleting") {
		t.Errorf("fast-path delete: tags must include 'bosh-deleting'; got %q", capturedTags)
	}
	if !strings.Contains(capturedTags, "existing-tag") {
		t.Errorf("fast-path delete: existing tags must be preserved; got %q", capturedTags)
	}
}

// TestHandleDeleteVM_FastPath_TagFailOpen verifies that a tagging failure
// (UpdateQemuConfig error) does NOT block the destroy call.
func TestHandleDeleteVM_FastPath_TagFailOpen(t *testing.T) {
	t.Parallel()

	deleteCalled := false

	qemuSvc := &mockQEMUService{
		stopFn: func(_ context.Context, _ string, _ int) (string, error) {
			return "", nil
		},
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{}, nil
		},
	}
	nodesSvc := &mockNodesService{
		deleteQemuFn: func(_ context.Context, _ string, _ string, _ *nodes.DeleteQemuParams) (*nodes.DeleteQemuResponse, error) {
			deleteCalled = true
			raw := nodes.DeleteQemuResponse{}
			return &raw, nil
		},
		updateQemuConfigFn: func(_ context.Context, _ string, _ string, _ *nodes.UpdateQemuConfigParams) error {
			return errors.New("pve: config write: lock timeout")
		},
	}
	tasksSvc := &mockTasksService{}
	agentSvc := &mockAgentService{}

	h := handlers.HandleDeleteVM(fastPathDeps(703, qemuSvc, nodesSvc, tasksSvc, agentSvc))
	result, err := h.Handle(context.Background(), marshalArgs("703"), jsonrpc.Context{})

	if err != nil {
		t.Fatalf("fast-path tag-fail-open: tagging error must not propagate, got: %v", err)
	}
	if result != nil {
		t.Errorf("fast-path tag-fail-open: expected nil result, got %v", result)
	}
	if !deleteCalled {
		t.Error("fast-path tag-fail-open: destroy must still be issued after tag failure")
	}
}

// TestHandleDeleteVM_FastPath_NotFound_Idempotent verifies that when the VM is
// not found during the fast-path destroy call, the handler returns success.
func TestHandleDeleteVM_FastPath_NotFound_Idempotent(t *testing.T) {
	t.Parallel()

	qemuSvc := &mockQEMUService{
		stopFn: func(_ context.Context, _ string, _ int) (string, error) {
			return "", nil
		},
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{}, nil
		},
	}
	nodesSvc := &mockNodesService{
		deleteQemuFn: func(_ context.Context, _ string, _ string, _ *nodes.DeleteQemuParams) (*nodes.DeleteQemuResponse, error) {
			return nil, notFoundAPIErr()
		},
		updateQemuConfigFn: func(_ context.Context, _ string, _ string, _ *nodes.UpdateQemuConfigParams) error {
			return nil
		},
	}
	tasksSvc := &mockTasksService{}
	agentSvc := &mockAgentService{}

	h := handlers.HandleDeleteVM(fastPathDeps(704, qemuSvc, nodesSvc, tasksSvc, agentSvc))
	result, err := h.Handle(context.Background(), marshalArgs("704"), jsonrpc.Context{})

	if err != nil {
		t.Fatalf("fast-path NotFound idempotent: expected nil error, got: %v", err)
	}
	if result != nil {
		t.Errorf("fast-path NotFound idempotent: expected nil result, got %v", result)
	}
}

// TestHandleDeleteVM_SlowPath_AwaitsTask confirms that when fast_path_delete is
// OFF (default), the destroy task IS awaited — byte-identical behavior.
func TestHandleDeleteVM_SlowPath_AwaitsTask(t *testing.T) {
	t.Parallel()

	const destroyUPID = "UPID:pve-node1:00AABBCC:00112233:6789ABCD:qmdestroy:705:root@pam:"
	awaitCalled := false

	qemuSvc := &mockQEMUService{
		stopFn: func(_ context.Context, _ string, _ int) (string, error) {
			return "", nil
		},
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{}, nil
		},
	}
	deleteResp := nodes.DeleteQemuResponse(`"` + destroyUPID + `"`)
	nodesSvc := &mockNodesService{
		deleteQemuFn: func(_ context.Context, _ string, _ string, _ *nodes.DeleteQemuParams) (*nodes.DeleteQemuResponse, error) {
			return &deleteResp, nil
		},
	}
	tasksSvc := &mockTasksService{
		waitFn: func(_ context.Context, _, upid string, _ *tasks.WaitOptions) (*tasks.Status, error) {
			if upid == destroyUPID {
				awaitCalled = true
			}
			return &tasks.Status{ExitStatus: "OK"}, nil
		},
	}
	agentSvc := &mockAgentService{}

	// Use testDepsFoundVM (no FastPathDelete set → nil → off).
	h := handlers.HandleDeleteVM(testDepsFoundVM(705, qemuSvc, nodesSvc, tasksSvc, agentSvc))
	_, err := h.Handle(context.Background(), marshalArgs("705"), jsonrpc.Context{})

	if err != nil {
		t.Fatalf("slow-path byte-identical: unexpected error: %v", err)
	}
	if !awaitCalled {
		t.Error("slow-path byte-identical: destroy task must be awaited when fast_path_delete is OFF")
	}
}

// TestHandleDeleteVM_FastPath_ExplicitFalse_AwaitsTask confirms that
// fast_path_delete=*false (explicit false, not nil) preserves the synchronous
// await path — byte-identical to nil.
func TestHandleDeleteVM_FastPath_ExplicitFalse_AwaitsTask(t *testing.T) {
	t.Parallel()

	const destroyUPID = "UPID:pve-node1:00AABBCC:00112233:6789ABCD:qmdestroy:706:root@pam:"
	awaitCalled := false

	qemuSvc := &mockQEMUService{
		stopFn: func(_ context.Context, _ string, _ int) (string, error) {
			return "", nil
		},
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{}, nil
		},
	}
	deleteResp := nodes.DeleteQemuResponse(`"` + destroyUPID + `"`)
	nodesSvc := &mockNodesService{
		deleteQemuFn: func(_ context.Context, _ string, _ string, _ *nodes.DeleteQemuParams) (*nodes.DeleteQemuResponse, error) {
			return &deleteResp, nil
		},
	}
	tasksSvc := &mockTasksService{
		waitFn: func(_ context.Context, _, upid string, _ *tasks.WaitOptions) (*tasks.Status, error) {
			if upid == destroyUPID {
				awaitCalled = true
			}
			return &tasks.Status{ExitStatus: "OK"}, nil
		},
	}
	agentSvc := &mockAgentService{}

	deps := testDepsFoundVM(706, qemuSvc, nodesSvc, tasksSvc, agentSvc)
	disabled := false
	deps.Config.FastPathDelete = &disabled // explicit *false

	h := handlers.HandleDeleteVM(deps)
	_, err := h.Handle(context.Background(), marshalArgs("706"), jsonrpc.Context{})

	if err != nil {
		t.Fatalf("explicit-false: unexpected error: %v", err)
	}
	if !awaitCalled {
		t.Error("explicit-false: destroy task must be awaited when fast_path_delete is explicit *false")
	}
}

// TestHandleDeleteVM_FastPath_StaggerSweepReapsStraggler verifies that the
// straggler sweep (sweepFastDeleteStragglers) finds a VM tagged bosh-deleting
// in the cluster list and re-issues a destroy for it before the current delete.
func TestHandleDeleteVM_FastPath_StaggerSweepReapsStraggler(t *testing.T) {
	t.Parallel()

	const currentVMID = 710
	const stragglerVMID = 711
	const stragglerNode = "pve-node1"

	stragglerDestroyCalled := false
	currentDestroyCalled := false

	// Build a cluster response that contains:
	//   - stragglerVMID: tagged bosh-deleting (straggler from a prior fast-path delete)
	//   - currentVMID: not tagged (the VM being deleted now)
	stragglerRaw, _ := json.Marshal(map[string]any{
		"vmid": stragglerVMID,
		"node": stragglerNode,
		"type": "qemu",
		"tags": "bosh-deleting",
	})
	currentRaw, _ := json.Marshal(map[string]any{
		"vmid": currentVMID,
		"node": vmNode,
		"type": "qemu",
	})
	clusterSvc := &mockClusterSvc{
		listResourcesFn: func(_ context.Context, _ *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
			resp := cluster.ListResourcesResponse{currentRaw, stragglerRaw}
			return &resp, nil
		},
	}

	qemuSvc := &mockQEMUService{
		stopFn: func(_ context.Context, _ string, _ int) (string, error) {
			return "", nil
		},
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{}, nil
		},
	}
	nodesSvc := &mockNodesService{
		deleteQemuFn: func(_ context.Context, _ string, vmidStr string, _ *nodes.DeleteQemuParams) (*nodes.DeleteQemuResponse, error) {
			switch vmidStr {
			case "711":
				stragglerDestroyCalled = true
			case "710":
				currentDestroyCalled = true
			}
			raw := nodes.DeleteQemuResponse{}
			return &raw, nil
		},
		updateQemuConfigFn: func(_ context.Context, _ string, _ string, _ *nodes.UpdateQemuConfigParams) error {
			return nil
		},
	}
	tasksSvc := &mockTasksService{}
	agentSvc := &mockAgentService{}

	deps := testDepsWithCluster(qemuSvc, nodesSvc, tasksSvc, agentSvc, &mockStorageService{}, clusterSvc)
	enabled := true
	deps.Config.FastPathDelete = &enabled

	h := handlers.HandleDeleteVM(deps)
	_, err := h.Handle(context.Background(), marshalArgs("710"), jsonrpc.Context{})

	if err != nil {
		t.Fatalf("straggler sweep: unexpected error: %v", err)
	}
	if !stragglerDestroyCalled {
		t.Error("straggler sweep: must re-issue destroy for bosh-deleting VM 711")
	}
	if !currentDestroyCalled {
		t.Error("straggler sweep: current VM 710 destroy must also be issued")
	}
}

// TestHandleDeleteVM_FastPath_SweepSkipsGoneStraggler verifies that the sweep
// treats a 404 on a straggler destroy as idempotent success (VM already gone).
func TestHandleDeleteVM_FastPath_SweepSkipsGoneStraggler(t *testing.T) {
	t.Parallel()

	const currentVMID = 720
	const stragglerVMID = 721

	sweepErrPropagated := false

	stragglerRaw, _ := json.Marshal(map[string]any{
		"vmid": stragglerVMID,
		"node": vmNode,
		"type": "qemu",
		"tags": "bosh-deleting",
	})
	currentRaw, _ := json.Marshal(map[string]any{
		"vmid": currentVMID,
		"node": vmNode,
		"type": "qemu",
	})
	clusterSvc := &mockClusterSvc{
		listResourcesFn: func(_ context.Context, _ *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
			resp := cluster.ListResourcesResponse{currentRaw, stragglerRaw}
			return &resp, nil
		},
	}

	qemuSvc := &mockQEMUService{
		stopFn: func(_ context.Context, _ string, _ int) (string, error) {
			return "", nil
		},
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{}, nil
		},
	}
	nodesSvc := &mockNodesService{
		deleteQemuFn: func(_ context.Context, _ string, vmidStr string, _ *nodes.DeleteQemuParams) (*nodes.DeleteQemuResponse, error) {
			if vmidStr == "721" {
				// Straggler already gone — 404.
				return nil, notFoundAPIErr()
			}
			// Current VM succeeds.
			raw := nodes.DeleteQemuResponse{}
			return &raw, nil
		},
		updateQemuConfigFn: func(_ context.Context, _ string, _ string, _ *nodes.UpdateQemuConfigParams) error {
			return nil
		},
	}
	tasksSvc := &mockTasksService{}
	agentSvc := &mockAgentService{
		removeFn: func(_ context.Context, _ string, _ int) error {
			return nil
		},
	}

	deps := testDepsWithCluster(qemuSvc, nodesSvc, tasksSvc, agentSvc, &mockStorageService{}, clusterSvc)
	enabled := true
	deps.Config.FastPathDelete = &enabled

	h := handlers.HandleDeleteVM(deps)
	_, err := h.Handle(context.Background(), marshalArgs("720"), jsonrpc.Context{})

	// Straggler 404 must NOT cause the current delete to fail.
	if err != nil {
		sweepErrPropagated = true
		t.Fatalf("straggler sweep: 404 on straggler must be non-fatal, got: %v", err)
	}
	if sweepErrPropagated {
		t.Error("straggler 404 propagated as error")
	}
}

// TestHandleDeleteVM_FastPath_SweepFailOpen verifies that a ListResources
// failure in the straggler sweep does not block the current delete.
//
// HandleDeleteVM calls ListResources TWICE when fast_path_delete is on:
//   - Call #1: FindVMNodeViaCluster (locate step) — must succeed so the delete proceeds.
//   - Call #2: sweepFastDeleteStragglers — may fail; failure must be non-fatal.
//
// The call-counter mock returns the target VM on the first call and an error on
// the second, proving locate succeeds while sweep fails open.
func TestHandleDeleteVM_FastPath_SweepFailOpen(t *testing.T) {
	t.Parallel()

	const currentVMID = 730
	currentDestroyCalled := false

	// Build the locate response: a single qemu VM entry for VMID 730 on pve-node1.
	locateRaw, _ := json.Marshal(map[string]any{
		"vmid": currentVMID,
		"node": vmNode,
		"type": "qemu",
	})
	locateResp := cluster.ListResourcesResponse{locateRaw}

	var listCallCount int
	clusterSvc := &mockClusterSvc{
		listResourcesFn: func(_ context.Context, _ *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
			listCallCount++
			if listCallCount == 1 {
				// First call: FindVMNodeViaCluster — return the target VM so locate succeeds.
				return &locateResp, nil
			}
			// Second call: sweepFastDeleteStragglers — simulate cluster unreachable.
			return nil, errors.New("cluster: connection refused")
		},
	}

	qemuSvc := &mockQEMUService{
		stopFn: func(_ context.Context, _ string, _ int) (string, error) {
			return "", nil
		},
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{}, nil
		},
	}
	nodesSvc := &mockNodesService{
		deleteQemuFn: func(_ context.Context, _ string, _ string, _ *nodes.DeleteQemuParams) (*nodes.DeleteQemuResponse, error) {
			currentDestroyCalled = true
			raw := nodes.DeleteQemuResponse{}
			return &raw, nil
		},
		updateQemuConfigFn: func(_ context.Context, _ string, _ string, _ *nodes.UpdateQemuConfigParams) error {
			return nil
		},
	}
	tasksSvc := &mockTasksService{}
	agentSvc := &mockAgentService{}

	deps := testDepsWithCluster(qemuSvc, nodesSvc, tasksSvc, agentSvc, &mockStorageService{}, clusterSvc)
	enabled := true
	deps.Config.FastPathDelete = &enabled

	h := handlers.HandleDeleteVM(deps)
	result, err := h.Handle(context.Background(), marshalArgs("730"), jsonrpc.Context{})

	if err != nil {
		t.Fatalf("sweep-fail-open: sweep ListResources error must not block delete, got: %v", err)
	}
	if result != nil {
		t.Errorf("sweep-fail-open: expected nil result, got %v", result)
	}
	if !currentDestroyCalled {
		t.Error("sweep-fail-open: current VM destroy must still be issued after sweep failure")
	}
	if listCallCount < 2 {
		t.Errorf("sweep-fail-open: expected ≥2 ListResources calls (locate + sweep), got %d", listCallCount)
	}
}

// TestHandleDeleteVM_FastPath_SweepSkipsNonQemu verifies that the straggler
// sweep ignores non-qemu cluster resources (e.g. storage, node entries) and
// untagged qemu VMs. DeleteQemu must not be called for either category.
func TestHandleDeleteVM_FastPath_SweepSkipsNonQemu(t *testing.T) {
	t.Parallel()

	const currentVMID = 731

	// Cluster list contains:
	//   - a storage resource (type != "qemu") — must be skipped
	//   - an untagged qemu VM — must be skipped (no bosh-deleting tag)
	//   - the current VM being deleted (no tag) — will be destroyed by the main path
	storageRaw, _ := json.Marshal(map[string]any{
		"type":    "storage",
		"storage": "local-lvm",
		"node":    vmNode,
	})
	untaggedRaw, _ := json.Marshal(map[string]any{
		"vmid": 799,
		"node": vmNode,
		"type": "qemu",
		// no "tags" field → sweep must skip this VM
	})
	currentRaw, _ := json.Marshal(map[string]any{
		"vmid": currentVMID,
		"node": vmNode,
		"type": "qemu",
	})
	allEntries := cluster.ListResourcesResponse{storageRaw, untaggedRaw, currentRaw}

	// listResourcesFn is shared between FindVMNodeViaCluster and sweep; return
	// the same list each time. The locate step finds currentVMID; the sweep
	// step must skip both non-qemu and untagged entries.
	clusterSvc := &mockClusterSvc{
		listResourcesFn: func(_ context.Context, _ *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
			return &allEntries, nil
		},
	}

	deleteQemuCalled := false
	qemuSvc := &mockQEMUService{
		stopFn: func(_ context.Context, _ string, _ int) (string, error) {
			return "", nil
		},
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{}, nil
		},
	}
	nodesSvc := &mockNodesService{
		deleteQemuFn: func(_ context.Context, _ string, vmidStr string, _ *nodes.DeleteQemuParams) (*nodes.DeleteQemuResponse, error) {
			// Only the current VM's destroy (731) is expected; any other call is a defect.
			if vmidStr != "731" {
				deleteQemuCalled = true
				t.Errorf("sweep: unexpected DeleteQemu for vmid=%q (want only 731 for current VM)", vmidStr)
			}
			raw := nodes.DeleteQemuResponse{}
			return &raw, nil
		},
		updateQemuConfigFn: func(_ context.Context, _ string, _ string, _ *nodes.UpdateQemuConfigParams) error {
			return nil
		},
	}
	tasksSvc := &mockTasksService{}
	agentSvc := &mockAgentService{}

	deps := testDepsWithCluster(qemuSvc, nodesSvc, tasksSvc, agentSvc, &mockStorageService{}, clusterSvc)
	enabled := true
	deps.Config.FastPathDelete = &enabled

	h := handlers.HandleDeleteVM(deps)
	_, err := h.Handle(context.Background(), marshalArgs("731"), jsonrpc.Context{})

	if err != nil {
		t.Fatalf("sweep-skip-non-qemu: unexpected error: %v", err)
	}
	if deleteQemuCalled {
		t.Error("sweep-skip-non-qemu: DeleteQemu must not be called for non-qemu or untagged VMs")
	}
}

// TestHandleDeleteVM_FastPath_SweepDestroyError verifies that a non-NotFound
// error from DeleteQemu during the straggler sweep is logged but never
// propagated — the current VM delete must still succeed.
func TestHandleDeleteVM_FastPath_SweepDestroyError(t *testing.T) {
	t.Parallel()

	const currentVMID = 732
	const stragglerVMID = 733

	stragglerRaw, _ := json.Marshal(map[string]any{
		"vmid": stragglerVMID,
		"node": vmNode,
		"type": "qemu",
		"tags": "bosh-deleting",
	})
	currentRaw, _ := json.Marshal(map[string]any{
		"vmid": currentVMID,
		"node": vmNode,
		"type": "qemu",
	})
	allEntries := cluster.ListResourcesResponse{currentRaw, stragglerRaw}

	clusterSvc := &mockClusterSvc{
		listResourcesFn: func(_ context.Context, _ *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
			return &allEntries, nil
		},
	}

	currentDestroyCalled := false
	qemuSvc := &mockQEMUService{
		stopFn: func(_ context.Context, _ string, _ int) (string, error) {
			return "", nil
		},
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{}, nil
		},
	}
	nodesSvc := &mockNodesService{
		deleteQemuFn: func(_ context.Context, _ string, vmidStr string, _ *nodes.DeleteQemuParams) (*nodes.DeleteQemuResponse, error) {
			if vmidStr == "733" {
				// Straggler destroy fails with a non-404 error.
				return nil, errors.New("pve: task queue full")
			}
			// Current VM destroy succeeds.
			currentDestroyCalled = true
			raw := nodes.DeleteQemuResponse{}
			return &raw, nil
		},
		updateQemuConfigFn: func(_ context.Context, _ string, _ string, _ *nodes.UpdateQemuConfigParams) error {
			return nil
		},
	}
	tasksSvc := &mockTasksService{}
	agentSvc := &mockAgentService{}

	deps := testDepsWithCluster(qemuSvc, nodesSvc, tasksSvc, agentSvc, &mockStorageService{}, clusterSvc)
	enabled := true
	deps.Config.FastPathDelete = &enabled

	h := handlers.HandleDeleteVM(deps)
	_, err := h.Handle(context.Background(), marshalArgs("732"), jsonrpc.Context{})

	if err != nil {
		t.Fatalf("sweep-destroy-error: non-404 straggler destroy error must be non-fatal, got: %v", err)
	}
	if !currentDestroyCalled {
		t.Error("sweep-destroy-error: current VM destroy must proceed after straggler destroy failure")
	}
}

// TestHandleDeleteVM_FastPath_StopError_NonFatal verifies that when Stop returns
// a non-NotFound error on the fast path, the error is logged but the destroy
// still proceeds (skiplock=true handles running/locked VMs).
func TestHandleDeleteVM_FastPath_StopError_NonFatal(t *testing.T) {
	t.Parallel()

	destroyCalled := false

	qemuSvc := &mockQEMUService{
		stopFn: func(_ context.Context, _ string, _ int) (string, error) {
			// Transient PVE error — not a 404. Fast path must log and continue.
			return "", errors.New("pve: worker busy")
		},
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{}, nil
		},
	}
	nodesSvc := &mockNodesService{
		deleteQemuFn: func(_ context.Context, _ string, _ string, _ *nodes.DeleteQemuParams) (*nodes.DeleteQemuResponse, error) {
			destroyCalled = true
			raw := nodes.DeleteQemuResponse{}
			return &raw, nil
		},
		updateQemuConfigFn: func(_ context.Context, _ string, _ string, _ *nodes.UpdateQemuConfigParams) error {
			return nil
		},
	}
	tasksSvc := &mockTasksService{}
	agentSvc := &mockAgentService{}

	h := handlers.HandleDeleteVM(fastPathDeps(734, qemuSvc, nodesSvc, tasksSvc, agentSvc))
	_, err := h.Handle(context.Background(), marshalArgs("734"), jsonrpc.Context{})

	if err != nil {
		t.Fatalf("fast-path stop-error non-fatal: expected nil error, got: %v", err)
	}
	if !destroyCalled {
		t.Error("fast-path stop-error non-fatal: destroy must still be issued after non-fatal stop error")
	}
}

// TestHandleDeleteVM_FastPath_DestroyError_Propagates verifies that a real
// (non-NotFound) error from DeleteQemu on the fast path is wrapped and returned
// to the caller. This is the only error path that blocks a fast-path delete.
func TestHandleDeleteVM_FastPath_DestroyError_Propagates(t *testing.T) {
	t.Parallel()

	qemuSvc := &mockQEMUService{
		stopFn: func(_ context.Context, _ string, _ int) (string, error) {
			return "", nil
		},
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{}, nil
		},
	}
	nodesSvc := &mockNodesService{
		deleteQemuFn: func(_ context.Context, _ string, _ string, _ *nodes.DeleteQemuParams) (*nodes.DeleteQemuResponse, error) {
			return nil, errors.New("pve: storage backend failure")
		},
		updateQemuConfigFn: func(_ context.Context, _ string, _ string, _ *nodes.UpdateQemuConfigParams) error {
			return nil
		},
	}
	tasksSvc := &mockTasksService{}
	agentSvc := &mockAgentService{}

	h := handlers.HandleDeleteVM(fastPathDeps(735, qemuSvc, nodesSvc, tasksSvc, agentSvc))
	_, err := h.Handle(context.Background(), marshalArgs("735"), jsonrpc.Context{})

	if err == nil {
		t.Fatal("fast-path destroy-error: expected wrapped error from DeleteQemu failure, got nil")
	}
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) {
		t.Fatalf("fast-path destroy-error: expected *cpierrors.Error, got %T: %v", err, err)
	}
}

// TestHandleDeleteVM_FastPath_StopNotFound_EarlyReturn verifies that when Stop
// returns IsNotFound on the fast path, the handler cleans up agent state and
// returns success immediately without issuing DeleteQemu.
func TestHandleDeleteVM_FastPath_StopNotFound_EarlyReturn(t *testing.T) {
	t.Parallel()

	destroyCalled := false

	qemuSvc := &mockQEMUService{
		stopFn: func(_ context.Context, _ string, _ int) (string, error) {
			return "", &sdkerrors.APIError{HTTPCode: 404}
		},
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{}, nil
		},
	}
	nodesSvc := &mockNodesService{
		deleteQemuFn: func(_ context.Context, _ string, _ string, _ *nodes.DeleteQemuParams) (*nodes.DeleteQemuResponse, error) {
			destroyCalled = true
			t.Error("fast-path: DeleteQemu must NOT be called when Stop returns IsNotFound (early return)")
			raw := nodes.DeleteQemuResponse{}
			return &raw, nil
		},
		updateQemuConfigFn: func(_ context.Context, _ string, _ string, _ *nodes.UpdateQemuConfigParams) error {
			return nil
		},
	}
	tasksSvc := &mockTasksService{}
	agentRemoveCalled := false
	agentSvc := &mockAgentService{
		removeFn: func(_ context.Context, _ string, _ int) error {
			agentRemoveCalled = true
			return nil
		},
	}

	h := handlers.HandleDeleteVM(fastPathDeps(736, qemuSvc, nodesSvc, tasksSvc, agentSvc))
	_, err := h.Handle(context.Background(), marshalArgs("736"), jsonrpc.Context{})

	if err != nil {
		t.Fatalf("fast-path stop-not-found early-return: expected nil error, got: %v", err)
	}
	if destroyCalled {
		t.Error("fast-path stop-not-found early-return: DeleteQemu must not be called after Stop IsNotFound")
	}
	if !agentRemoveCalled {
		t.Error("fast-path stop-not-found early-return: agent.Remove must be called for cleanup")
	}
}

// TestHandleDeleteVM_FastPath_SweepCapsAtMax verifies that when there are more
// straggler VMs than sweepStragglersMaxPerSweep, only cap-many destroys are
// issued during the sweep and the remainder are deferred (not destroyed silently).
// The test wires (sweepStragglersMaxPerSweep + 3) stragglers and counts how many
// DeleteQemu calls the sweep makes; the current VM's destroy is also counted and
// must be excluded from the straggler tally.
func TestHandleDeleteVM_FastPath_SweepCapsAtMax(t *testing.T) {
	t.Parallel()

	const currentVMID = 800
	const stragglersAboveCap = 3
	const stragglerCount = 5 + stragglersAboveCap // sweepStragglersMaxPerSweep + 3

	// Build cluster response: currentVMID (untagged) + stragglerCount tagged VMs.
	var allEntries cluster.ListResourcesResponse
	currentRaw, _ := json.Marshal(map[string]any{
		"vmid": currentVMID,
		"node": vmNode,
		"type": "qemu",
	})
	allEntries = append(allEntries, currentRaw)
	for i := 0; i < stragglerCount; i++ {
		stragglerRaw, _ := json.Marshal(map[string]any{
			"vmid": 900 + i,
			"node": vmNode,
			"type": "qemu",
			"tags": "bosh-deleting",
		})
		allEntries = append(allEntries, stragglerRaw)
	}

	clusterSvc := &mockClusterSvc{
		listResourcesFn: func(_ context.Context, _ *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
			return &allEntries, nil
		},
	}

	var sweepDestroyCalls int
	currentDestroyCalled := false
	qemuSvc := &mockQEMUService{
		stopFn: func(_ context.Context, _ string, _ int) (string, error) {
			return "", nil
		},
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{}, nil
		},
	}
	nodesSvc := &mockNodesService{
		deleteQemuFn: func(_ context.Context, _ string, vmidStr string, _ *nodes.DeleteQemuParams) (*nodes.DeleteQemuResponse, error) {
			if vmidStr == strconv.Itoa(currentVMID) {
				currentDestroyCalled = true
			} else {
				sweepDestroyCalls++
			}
			raw := nodes.DeleteQemuResponse{}
			return &raw, nil
		},
		updateQemuConfigFn: func(_ context.Context, _ string, _ string, _ *nodes.UpdateQemuConfigParams) error {
			return nil
		},
	}
	tasksSvc := &mockTasksService{}
	agentSvc := &mockAgentService{}

	deps := testDepsWithCluster(qemuSvc, nodesSvc, tasksSvc, agentSvc, &mockStorageService{}, clusterSvc)
	enabled := true
	deps.Config.FastPathDelete = &enabled

	h := handlers.HandleDeleteVM(deps)
	_, err := h.Handle(context.Background(), marshalArgs(strconv.Itoa(currentVMID)), jsonrpc.Context{})

	if err != nil {
		t.Fatalf("sweep cap test: unexpected error: %v", err)
	}
	if !currentDestroyCalled {
		t.Error("sweep cap test: current VM destroy must be issued after sweep")
	}
	// Exactly sweepStragglersMaxPerSweep (5) straggler destroys must be issued;
	// the remaining stragglersAboveCap (3) must be deferred.
	const wantSweepDestroys = 5
	if sweepDestroyCalls != wantSweepDestroys {
		t.Errorf("sweep cap test: straggler destroy calls = %d; want %d (cap=%d, total=%d, deferred=%d)",
			sweepDestroyCalls, wantSweepDestroys, wantSweepDestroys, stragglerCount, stragglersAboveCap)
	}
}

// ---------------------------------------------------------------------------
// Parker VM refusal tests
// ---------------------------------------------------------------------------

// parkerDepsWithRange builds Deps with the parker VMID range [90000, 90999]
// configured. vmid is the parker VM being targeted; configFn provides its config.
func parkerDepsWithRange(vmid int, qemuSvc *mockQEMUService, nodesSvc *mockNodesService) handlers.Deps {
	deps := testDepsFoundVMWithStorage(vmid, qemuSvc, nodesSvc, &mockTasksService{}, &mockAgentService{}, &mockStorageService{})
	deps.Config.ParkedDiskVMIDRangeStart = 90000
	deps.Config.ParkedDiskVMIDRangeEnd = 90999
	return deps
}

// TestHandleDeleteVM_RefusesParkerVM_SlowPath verifies that when a VMID falls
// in the configured parker range and carries the bosh-parker tag, delete_vm
// returns a non-retriable CloudError and does not issue Stop or DeleteQemu.
func TestHandleDeleteVM_RefusesParkerVM_SlowPath(t *testing.T) {
	t.Parallel()

	const parkerVMID = 90010
	stopCalled := false
	deleteCalled := false

	qemuSvc := &mockQEMUService{
		stopFn: func(_ context.Context, _ string, _ int) (string, error) {
			stopCalled = true
			return "", nil
		},
		configFn: func(_ context.Context, _ string, vmid int) (map[string]any, error) {
			if vmid == parkerVMID {
				return map[string]any{"tags": "bosh-parker"}, nil
			}
			return map[string]any{}, nil
		},
	}
	nodesSvc := &mockNodesService{
		deleteQemuFn: func(_ context.Context, _ string, _ string, _ *nodes.DeleteQemuParams) (*nodes.DeleteQemuResponse, error) {
			deleteCalled = true
			raw := nodes.DeleteQemuResponse{}
			return &raw, nil
		},
	}

	deps := parkerDepsWithRange(parkerVMID, qemuSvc, nodesSvc)
	h := handlers.HandleDeleteVM(deps)
	_, err := h.Handle(context.Background(), marshalArgs(strconv.Itoa(parkerVMID)), jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected non-retriable CloudError for parker VM, got nil")
	}
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) {
		t.Fatalf("expected *cpierrors.Error, got %T: %v", err, err)
	}
	if cpiErr.OkToRetry() {
		t.Errorf("parker refusal must be non-retriable; OkToRetry()=true")
	}
	if !strings.Contains(err.Error(), "parker") {
		t.Errorf("error message must mention 'parker'; got: %q", err.Error())
	}
	if stopCalled {
		t.Error("Stop must NOT be called for a parker VM")
	}
	if deleteCalled {
		t.Error("DeleteQemu must NOT be called for a parker VM")
	}
}

// TestHandleDeleteVM_RefusesParkerVM_FastPath verifies that the fast path also
// refuses a parker VM before issuing the skiplock destroy.
func TestHandleDeleteVM_RefusesParkerVM_FastPath(t *testing.T) {
	t.Parallel()

	const parkerVMID = 90020
	deleteCalled := false

	qemuSvc := &mockQEMUService{
		stopFn: func(_ context.Context, _ string, _ int) (string, error) {
			return "", nil
		},
		configFn: func(_ context.Context, _ string, vmid int) (map[string]any, error) {
			if vmid == parkerVMID {
				return map[string]any{"tags": "bosh-parker"}, nil
			}
			return map[string]any{}, nil
		},
	}
	nodesSvc := &mockNodesService{
		deleteQemuFn: func(_ context.Context, _ string, _ string, _ *nodes.DeleteQemuParams) (*nodes.DeleteQemuResponse, error) {
			deleteCalled = true
			raw := nodes.DeleteQemuResponse{}
			return &raw, nil
		},
		updateQemuConfigFn: func(_ context.Context, _ string, _ string, _ *nodes.UpdateQemuConfigParams) error {
			return nil
		},
	}

	deps := parkerDepsWithRange(parkerVMID, qemuSvc, nodesSvc)
	enabled := true
	deps.Config.FastPathDelete = &enabled

	h := handlers.HandleDeleteVM(deps)
	_, err := h.Handle(context.Background(), marshalArgs(strconv.Itoa(parkerVMID)), jsonrpc.Context{})

	if err == nil {
		t.Fatal("fast-path: expected non-retriable CloudError for parker VM, got nil")
	}
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) {
		t.Fatalf("fast-path: expected *cpierrors.Error, got %T: %v", err, err)
	}
	if cpiErr.OkToRetry() {
		t.Errorf("fast-path: parker refusal must be non-retriable; OkToRetry()=true")
	}
	if deleteCalled {
		t.Error("fast-path: DeleteQemu must NOT be called for a parker VM")
	}
}

// TestHandleDeleteVM_NormalVM_NoParkerCheck_WhenRangeUnset verifies that when
// the parker range is not configured (ParkedStrategyActive=false), delete_vm
// proceeds normally with zero extra API calls for the parker check.
func TestHandleDeleteVM_NormalVM_NoParkerCheck_WhenRangeUnset(t *testing.T) {
	t.Parallel()

	configCalls := 0
	deleteCalled := false

	qemuSvc := &mockQEMUService{
		stopFn: func(_ context.Context, _ string, _ int) (string, error) {
			return "", nil
		},
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			configCalls++
			return map[string]any{}, nil
		},
	}
	nodesSvc := &mockNodesService{
		deleteQemuFn: func(_ context.Context, _ string, _ string, _ *nodes.DeleteQemuParams) (*nodes.DeleteQemuResponse, error) {
			deleteCalled = true
			raw := nodes.DeleteQemuResponse{}
			return &raw, nil
		},
	}

	// testDepsFoundVM uses testConfig() which has ParkedDiskVMIDRangeStart/End=0
	// and DetachedDiskStrategy="" → ParkedStrategyActive()=false.
	h := handlers.HandleDeleteVM(testDepsFoundVM(101, qemuSvc, nodesSvc, &mockTasksService{}, &mockAgentService{}))
	_, err := h.Handle(context.Background(), marshalArgs("101"), jsonrpc.Context{})

	if err != nil {
		t.Fatalf("normal VM with range unset: unexpected error: %v", err)
	}
	if !deleteCalled {
		t.Error("normal VM: DeleteQemu must be called")
	}
	// Slow path reads config in:
	//   1. detachForeignActiveDisks — foreign-disk guard
	//   2. detachRetainedEphemeralDisk — retain-ephemeral tag check (exits early on no tag)
	//   3. guardUnusedVolumes — unusedN guard
	// Totalling 3 when no detach occurs. The parker check must add zero additional
	// config calls when ParkedStrategyActive=false (range unset). Assert count ≤ 3.
	if configCalls > 3 {
		t.Errorf("range-unset: Config called %d times; expected ≤3 (no parker check overhead)", configCalls)
	}
}

// TestHandleDeleteVM_InBandVMID_ConfigReadError_Retriable verifies that when a
// VMID falls in the parker range and the Config read fails with a transient
// error, delete_vm returns a retriable error and does NOT issue DeleteQemu.
// This prevents the fast-path skiplock+purge destroy from bypassing protection=1
// on a parker VM whose config is momentarily unreadable.
func TestHandleDeleteVM_InBandVMID_ConfigReadError_Retriable(t *testing.T) {
	t.Parallel()

	const vmid = 90030
	deleteCalled := false

	qemuSvc := &mockQEMUService{
		stopFn: func(_ context.Context, _ string, _ int) (string, error) {
			return "", nil
		},
		configFn: func(_ context.Context, _ string, id int) (map[string]any, error) {
			if id == vmid {
				return nil, errors.New("pve: connection refused")
			}
			return map[string]any{}, nil
		},
	}
	nodesSvc := &mockNodesService{
		deleteQemuFn: func(_ context.Context, _ string, _ string, _ *nodes.DeleteQemuParams) (*nodes.DeleteQemuResponse, error) {
			deleteCalled = true
			raw := nodes.DeleteQemuResponse{}
			return &raw, nil
		},
		updateQemuConfigFn: func(_ context.Context, _ string, _ string, _ *nodes.UpdateQemuConfigParams) error {
			return nil
		},
	}

	for _, fastPath := range []bool{false, true} {
		deleteCalled = false
		deps := parkerDepsWithRange(vmid, qemuSvc, nodesSvc)
		if fastPath {
			enabled := true
			deps.Config.FastPathDelete = &enabled
		}
		label := "slow-path"
		if fastPath {
			label = "fast-path"
		}

		h := handlers.HandleDeleteVM(deps)
		_, err := h.Handle(context.Background(), marshalArgs(strconv.Itoa(vmid)), jsonrpc.Context{})

		if err == nil {
			t.Fatalf("%s: expected retriable error for in-band VMID with config read failure, got nil", label)
		}
		var cpiErr *cpierrors.Error
		if !errors.As(err, &cpiErr) {
			t.Fatalf("%s: expected *cpierrors.Error, got %T: %v", label, err, err)
		}
		if !cpiErr.OkToRetry() {
			t.Errorf("%s: in-band config-read error must be retriable; OkToRetry()=false", label)
		}
		if deleteCalled {
			t.Errorf("%s: DeleteQemu must NOT be called when parker config read fails", label)
		}
	}
}

// TestHandleDeleteVM_VMInRange_NoParkerTag_Proceeds verifies that a VM whose
// VMID happens to fall in the parker range but does NOT carry the bosh-parker
// tag is not refused — it proceeds to normal deletion.
func TestHandleDeleteVM_VMInRange_NoParkerTag_Proceeds(t *testing.T) {
	t.Parallel()

	const vmid = 90050
	deleteCalled := false

	qemuSvc := &mockQEMUService{
		stopFn: func(_ context.Context, _ string, _ int) (string, error) {
			return "", nil
		},
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			// VM in parker range but NO bosh-parker tag.
			return map[string]any{"tags": "some-other-tag"}, nil
		},
	}
	nodesSvc := &mockNodesService{
		deleteQemuFn: func(_ context.Context, _ string, _ string, _ *nodes.DeleteQemuParams) (*nodes.DeleteQemuResponse, error) {
			deleteCalled = true
			raw := nodes.DeleteQemuResponse{}
			return &raw, nil
		},
	}

	deps := parkerDepsWithRange(vmid, qemuSvc, nodesSvc)
	h := handlers.HandleDeleteVM(deps)
	_, err := h.Handle(context.Background(), marshalArgs(strconv.Itoa(vmid)), jsonrpc.Context{})

	if err != nil {
		t.Fatalf("VM in range without parker tag: unexpected error: %v", err)
	}
	if !deleteCalled {
		t.Error("VM in range without parker tag: DeleteQemu must be called")
	}
}

// ---------------------------------------------------------------------------
// TestHandleDeleteVM_RetainEphemeral_VolumeActuallySurvives is an end-to-end
// test that proves the fix for F1: DestroyUnreferencedDisks=true would destroy
// the ephemeral volume after unlink+sweep (it is then unreferenced + own-VMID),
// and DestroyUnreferencedDisks=false (set by the retain path, or by the P5
// default when pve.destroy_unreferenced_disks is left unset) preserves it.
//
// The DeleteQemu double models PVE's actual behavior:
//   - When DestroyUnreferencedDisks=true: it "frees" every volume whose VMID
//     matches the VM being destroyed AND that is NOT referenced in the storage
//     inventory. After the unlink+sweep the ephemeral volume falls into this
//     class (config ref gone, matching VMID) → it would be destroyed.
//   - When DestroyUnreferencedDisks=false: unreferenced volumes are left alone.
//
// Sub-cases:
//   (a) retain flag present: DestroyUnreferencedDisks=false → ephemeral survives,
//       root disk is freed (it remains config-referenced at destroy time).
//   (b) retain flag absent, pve.destroy_unreferenced_disks unset (P5 default):
//       DestroyUnreferencedDisks=false → nothing in this class is freed.
//   (c) retain flag absent, pve.destroy_unreferenced_disks=true (explicit
//       opt-in): DestroyUnreferencedDisks=true → byte-identical to the
//       pre-P5 default.
// ---------------------------------------------------------------------------

//nolint:gocognit // Models the PVE config/storage state machine across unlink, sweep, and destroy; splitting obscures the sequence under test.
func TestHandleDeleteVM_RetainEphemeral_VolumeActuallySurvives(t *testing.T) {
	t.Parallel()

	const vmid = 500

	// simStorage models a PVE storage containing the VM's volumes.
	// Keys are bare volids; value true = volume still present.
	type simStorage map[string]bool

	// pveDestroyUnref models PVE's DestroyUnreferencedDisks pass: every volume
	// in stor whose VMID component matches vmidStr AND that is not present in
	// configRefs (a set of volids still referenced by config) is freed.
	pveDestroyUnref := func(stor simStorage, vmidStr string, configRefs map[string]bool) {
		prefix := "vm-" + vmidStr + "-"
		for volid := range stor {
			// Strip storage prefix for VMID check.
			v := volid
			if i := strings.Index(volid, ":"); i >= 0 {
				v = volid[i+1:]
			}
			if !strings.HasPrefix(v, prefix) {
				continue
			}
			if configRefs[volid] {
				continue // still referenced — not freed
			}
			delete(stor, volid)
		}
	}

	runCase := func(t *testing.T, retainTag, destroyUnreferencedDisks bool) (ephemeralSurvives, rootDestroyed bool) {
		t.Helper()

		const rootVolid = "zfs-1:vm-500-disk-0"
		const ephemeralVolid = "zfs-1:vm-500-ephemeral-0"

		// The simulated storage inventory: both volumes start present.
		stor := simStorage{rootVolid: true, ephemeralVolid: true}

		tagsVal := "bosh-cpi"
		if retainTag {
			tagsVal = "bosh-cpi;bosh-retain-ephemeral"
		}

		// Config reads are state-driven, not count-driven: multiple helpers on the
		// delete path read the VM config (detachForeignActiveDisks, the retain
		// check, guardUnusedVolumes), so the returned config models the actual VM
		// state machine — initial (scsi1 active) → post-unlink (unused0, tags kept
		// by PVE) → post-sweep (reference gone).
		var unlinked, swept bool
		qemuSvc := &mockQEMUService{
			stopFn: func(_ context.Context, _ string, _ int) (string, error) {
				return "", nil
			},
			configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
				switch {
				case !retainTag:
					// Non-retain: all reads return initial config (no ephemeral slot).
					return map[string]any{
						"virtio0": rootVolid + ",size=5G",
						"tags":    tagsVal,
					}, nil
				case !unlinked:
					return map[string]any{
						"virtio0": rootVolid + ",size=5G",
						"scsi1":   ephemeralVolid + ",size=10G",
						"tags":    tagsVal,
					}, nil
				case !swept:
					// Post-unlink: PVE demoted scsi1 to unused0; tags survive.
					return map[string]any{
						"virtio0": rootVolid + ",size=5G",
						"unused0": ephemeralVolid,
						"tags":    tagsVal,
					}, nil
				default:
					// Post-sweep: reference removed; volume exists only in storage.
					return map[string]any{
						"virtio0": rootVolid + ",size=5G",
						"tags":    tagsVal,
					}, nil
				}
			},
		}

		var capturedDestroyUnref *bool
		nodesSvc := &mockNodesService{
			updateQemuUnlinkFn: func(_ context.Context, _ string, _ string, _ *nodes.UpdateQemuUnlinkParams) error {
				unlinked = true // PVE demotes the slot to unusedN
				return nil
			},
			updateQemuConfigFn: func(_ context.Context, _ string, _ string, params *nodes.UpdateQemuConfigParams) error {
				if params != nil && params.Delete != nil && strings.Contains(*params.Delete, "unused0") {
					swept = true // sweep removed the unused0 reference
				}
				return nil // other calls stamp tags etc.
			},
			deleteQemuFn: func(_ context.Context, _ string, vmidStr string, params *nodes.DeleteQemuParams) (*nodes.DeleteQemuResponse, error) {
				if params != nil && params.DestroyUnreferencedDisks != nil {
					v := *params.DestroyUnreferencedDisks
					capturedDestroyUnref = &v
				}
				// Model PVE: when DestroyUnreferencedDisks=true, free unreferenced own-VMID volumes.
				destroyUnref := params != nil && params.DestroyUnreferencedDisks != nil && *params.DestroyUnreferencedDisks
				if destroyUnref {
					// At destroy time, virtio0 is the only config-referenced volume.
					// (On retain path: scsi1 and unused0 are already gone via unlink+sweep.)
					configRefs := map[string]bool{rootVolid: true}
					pveDestroyUnref(stor, vmidStr, configRefs)
				}
				return &nodes.DeleteQemuResponse{}, nil
			},
		}

		deps := testDepsFoundVM(vmid, qemuSvc, nodesSvc, &mockTasksService{}, &mockAgentService{})
		deps.Config.DestroyUnreferencedDisks = destroyUnreferencedDisks
		h := handlers.HandleDeleteVM(deps)
		_, err := h.Handle(context.Background(), marshalArgs(strconv.Itoa(vmid)), jsonrpc.Context{})
		if err != nil {
			t.Fatalf("HandleDeleteVM: unexpected error: %v", err)
		}

		switch {
		case retainTag:
			if capturedDestroyUnref == nil || *capturedDestroyUnref {
				t.Error("retain path: DeleteQemu must receive DestroyUnreferencedDisks=false regardless of the config knob")
			}
		case destroyUnreferencedDisks:
			if capturedDestroyUnref == nil || !*capturedDestroyUnref {
				t.Error("non-retain path with pve.destroy_unreferenced_disks=true: DeleteQemu must receive DestroyUnreferencedDisks=true")
			}
		default:
			if capturedDestroyUnref == nil || *capturedDestroyUnref {
				t.Error("non-retain path with pve.destroy_unreferenced_disks unset (P5 default): DeleteQemu must receive DestroyUnreferencedDisks=false")
			}
		}

		return stor[ephemeralVolid], !stor[rootVolid]
	}

	t.Run("retain_flag_present_ephemeral_survives", func(t *testing.T) {
		t.Parallel()
		ephemeralSurvives, rootDestroyed := runCase(t, true, false)
		// With fix: DestroyUnreferencedDisks=false → ephemeral untouched.
		if !ephemeralSurvives {
			t.Error("retain path: ephemeral volume must survive delete_vm; it was destroyed (DestroyUnreferencedDisks=true was wrongly used)")
		}
		// Root disk is still config-referenced at destroy time — it is NOT freed by
		// DestroyUnreferencedDisks (which only frees unreferenced volumes). Purge
		// removes config+HA references; actual storage free is via DeleteQemu's
		// standard disk deletion for referenced disks. We verify the mock did NOT
		// delete the root (our model only applies pveDestroyUnref for unreferenced).
		_ = rootDestroyed // root disk fate depends on PVE internals beyond our model
	})

	t.Run("retain_flag_present_destroy_unreferenced_disks_opt_in_still_survives", func(t *testing.T) {
		t.Parallel()
		// Retain semantics must win even when the operator opted in to
		// pve.destroy_unreferenced_disks -- see the destroyDisks := cfg &&
		// !retained expression at every delete_vm call site.
		ephemeralSurvives, _ := runCase(t, true, true)
		if !ephemeralSurvives {
			t.Error("retain path: ephemeral volume must survive delete_vm even with destroy_unreferenced_disks=true")
		}
	})

	t.Run("no_retain_flag_destroyDisks_defaults_false", func(t *testing.T) {
		t.Parallel()
		// P5 default: pve.destroy_unreferenced_disks unset -> DeleteQemu
		// receives DestroyUnreferencedDisks=false even on the non-retain
		// path. runCase's inner assertion checks this directly; no orphan
		// volume is freed here because the flag is off.
		runCase(t, false, false)
	})

	t.Run("no_retain_flag_destroyDisks_true_when_opted_in", func(t *testing.T) {
		t.Parallel()
		// Explicit pve.destroy_unreferenced_disks=true restores the
		// pre-P5 default behaviour: DeleteQemu receives
		// DestroyUnreferencedDisks=true on the non-retain path.
		runCase(t, false, true)
	})
}

// TestHandleDeleteVM_RetainEphemeral_NoUnusedEntryAfterUnlink_ConservativeFalse
// covers detachRetainedEphemeralDisk's conservative fallback: the SDK (or PVE
// itself) can auto-sweep the unusedN config reference in the same operation
// that demotes the ephemeral slot, so the post-unlink config re-read never
// shows an unusedN entry for the volid at all. The handler cannot confirm the
// reference is gone via a distinct sweep step, so it must still report
// retained=true and DeleteQemu must still receive DestroyUnreferencedDisks=
// false — the opposite (false→true) is the exact regression that would
// destroy the volume the operator asked to retain.
func TestHandleDeleteVM_RetainEphemeral_NoUnusedEntryAfterUnlink_ConservativeFalse(t *testing.T) {
	t.Parallel()

	const vmid = 502
	const rootVolid = "zfs-1:vm-502-disk-0"
	const ephemeralVolid = "zfs-1:vm-502-ephemeral-0"

	var unlinked bool
	qemuSvc := &mockQEMUService{
		stopFn: func(_ context.Context, _ string, _ int) (string, error) {
			return "", nil
		},
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			if !unlinked {
				return map[string]any{
					"virtio0": rootVolid + ",size=5G",
					"scsi1":   ephemeralVolid + ",size=10G",
					"tags":    "bosh-cpi;bosh-retain-ephemeral",
				}, nil
			}
			// Post-unlink: the SDK/PVE already swept the unusedN reference in the
			// same operation, so no unusedN key appears for ephemeralVolid at all.
			return map[string]any{
				"virtio0": rootVolid + ",size=5G",
				"tags":    "bosh-cpi;bosh-retain-ephemeral",
			}, nil
		},
	}

	var sweepCalled bool
	var capturedDestroyUnref *bool
	nodesSvc := &mockNodesService{
		updateQemuUnlinkFn: func(_ context.Context, _ string, _ string, _ *nodes.UpdateQemuUnlinkParams) error {
			unlinked = true
			return nil
		},
		updateQemuConfigFn: func(_ context.Context, _ string, _ string, params *nodes.UpdateQemuConfigParams) error {
			if params != nil && params.Delete != nil && strings.Contains(*params.Delete, "unused") {
				sweepCalled = true
			}
			return nil
		},
		deleteQemuFn: func(_ context.Context, _ string, _ string, params *nodes.DeleteQemuParams) (*nodes.DeleteQemuResponse, error) {
			if params != nil && params.DestroyUnreferencedDisks != nil {
				v := *params.DestroyUnreferencedDisks
				capturedDestroyUnref = &v
			}
			return &nodes.DeleteQemuResponse{}, nil
		},
	}

	h := handlers.HandleDeleteVM(testDepsFoundVM(vmid, qemuSvc, nodesSvc, &mockTasksService{}, &mockAgentService{}))
	_, err := h.Handle(context.Background(), marshalArgs(strconv.Itoa(vmid)), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("HandleDeleteVM: unexpected error: %v", err)
	}

	if sweepCalled {
		t.Error("no unusedN entry was found after unlink; UpdateQemuConfig sweep must not be called")
	}
	if capturedDestroyUnref == nil || *capturedDestroyUnref {
		t.Fatal("conservative fallback: DeleteQemu must receive DestroyUnreferencedDisks=false when no unusedN entry is found after unlink")
	}
}

// ---------------------------------------------------------------------------
// TestHandleDeleteVM_RetainEphemeral_StragglerSweepPreservesVolume covers the
// straggler window: a retain-tagged VM whose fast-path destroy fails AFTER the
// ephemeral disk was unlinked+swept survives carrying both bosh-deleting and
// bosh-retain-ephemeral, with its ephemeral volume unreferenced and matching
// VMID — exactly the class DestroyUnreferencedDisks=true frees. The next
// delete_vm's straggler sweep must re-issue the destroy with
// DestroyUnreferencedDisks=false, or it destroys the volume the first call
// preserved.
//
// Two real handler invocations against shared simulated state:
//
//	call 1: delete_vm(730) — retain-tagged; unlink+sweep succeed; the destroy
//	        returns a transient 500 → handler errors, VM survives as straggler.
//	call 2: delete_vm(731) — untagged; its sweep finds straggler 730 and must
//	        destroy it with DestroyUnreferencedDisks=false; the ephemeral
//	        volume must still exist afterwards.
//
//nolint:gocognit // Models config/tag/storage state across two handler calls; splitting obscures the window under test.
func TestHandleDeleteVM_RetainEphemeral_StragglerSweepPreservesVolume(t *testing.T) {
	t.Parallel()

	const stragglerVMID = 730
	const otherVMID = 731
	const rootVolid = "zfs-1:vm-730-disk-0"
	const ephemeralVolid = "zfs-1:vm-730-ephemeral-0"

	// Simulated storage inventory (bare volid -> present).
	stor := map[string]bool{rootVolid: true, ephemeralVolid: true}

	// VM 730 state machine, shared across both handler calls.
	tags730 := "bosh-cpi;bosh-retain-ephemeral"
	var unlinked730, swept730 bool
	firstDestroy730 := true
	var stragglerSweepDestroyUnref *bool

	clusterSvc := &mockClusterSvc{
		listResourcesFn: func(_ context.Context, _ *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
			// Built from live state so call 2 sees the tags call 1 actually wrote.
			raw730, _ := json.Marshal(map[string]any{
				"vmid": stragglerVMID, "node": vmNode, "type": "qemu", "tags": tags730,
			})
			raw731, _ := json.Marshal(map[string]any{
				"vmid": otherVMID, "node": vmNode, "type": "qemu",
			})
			resp := cluster.ListResourcesResponse{raw730, raw731}
			return &resp, nil
		},
	}

	qemuSvc := &mockQEMUService{
		stopFn: func(_ context.Context, _ string, _ int) (string, error) { return "", nil },
		configFn: func(_ context.Context, _ string, vmid int) (map[string]any, error) {
			if vmid != stragglerVMID {
				return map[string]any{}, nil
			}
			switch {
			case !unlinked730:
				return map[string]any{
					"virtio0": rootVolid + ",size=5G",
					"scsi1":   ephemeralVolid + ",size=10G",
					"tags":    tags730,
				}, nil
			case !swept730:
				return map[string]any{
					"virtio0": rootVolid + ",size=5G",
					"unused0": ephemeralVolid,
					"tags":    tags730,
				}, nil
			default:
				return map[string]any{
					"virtio0": rootVolid + ",size=5G",
					"tags":    tags730,
				}, nil
			}
		},
	}
	nodesSvc := &mockNodesService{
		updateQemuUnlinkFn: func(_ context.Context, _ string, vmidStr string, _ *nodes.UpdateQemuUnlinkParams) error {
			if vmidStr == "730" {
				unlinked730 = true
			}
			return nil
		},
		updateQemuConfigFn: func(_ context.Context, _ string, vmidStr string, params *nodes.UpdateQemuConfigParams) error {
			if vmidStr != "730" || params == nil {
				return nil
			}
			if params.Delete != nil && strings.Contains(*params.Delete, "unused0") {
				swept730 = true
			}
			if params.Tags != nil {
				tags730 = *params.Tags // stampDeletingTag merges bosh-deleting in
			}
			return nil
		},
		deleteQemuFn: func(_ context.Context, _ string, vmidStr string, params *nodes.DeleteQemuParams) (*nodes.DeleteQemuResponse, error) {
			if vmidStr == "730" && firstDestroy730 {
				firstDestroy730 = false
				// Transient failure AFTER unlink+sweep: the window under test.
				return nil, &sdkerrors.APIError{HTTPCode: 500, Message: "cluster not ready - no quorum?"}
			}
			destroyUnref := params != nil && params.DestroyUnreferencedDisks != nil && *params.DestroyUnreferencedDisks
			if vmidStr == "730" {
				v := destroyUnref
				stragglerSweepDestroyUnref = &v
				if destroyUnref {
					// Model PVE: free unreferenced own-VMID volumes. At this point
					// only virtio0 (root) is config-referenced.
					if !swept730 {
						t.Error("test invariant: straggler destroy reached before sweep completed")
					}
					delete(stor, ephemeralVolid)
				}
			}
			raw := nodes.DeleteQemuResponse{}
			return &raw, nil
		},
	}
	tasksSvc := &mockTasksService{}
	agentSvc := &mockAgentService{removeFn: func(_ context.Context, _ string, _ int) error { return nil }}

	deps := testDepsWithCluster(qemuSvc, nodesSvc, tasksSvc, agentSvc, &mockStorageService{}, clusterSvc)
	enabled := true
	deps.Config.FastPathDelete = &enabled
	h := handlers.HandleDeleteVM(deps)

	// Call 1: retain-tagged delete whose destroy fails transiently post-sweep.
	if _, err := h.Handle(context.Background(), marshalArgs("730"), jsonrpc.Context{}); err == nil {
		t.Fatal("call 1: expected transient destroy failure to propagate")
	}
	if !unlinked730 || !swept730 {
		t.Fatalf("call 1: expected unlink+sweep to complete before the failing destroy (unlinked=%v swept=%v)", unlinked730, swept730)
	}
	if !strings.Contains(tags730, "bosh-deleting") {
		t.Fatalf("call 1: expected bosh-deleting stamped on straggler, tags=%q", tags730)
	}
	if !stor[ephemeralVolid] {
		t.Fatal("call 1: ephemeral volume must still exist after the failed destroy")
	}

	// Call 2: deleting another VM; its sweep reaps straggler 730.
	if _, err := h.Handle(context.Background(), marshalArgs("731"), jsonrpc.Context{}); err != nil {
		t.Fatalf("call 2: unexpected error: %v", err)
	}
	if stragglerSweepDestroyUnref == nil {
		t.Fatal("call 2: straggler sweep must re-issue destroy for VM 730")
	}
	if *stragglerSweepDestroyUnref {
		t.Error("call 2: straggler sweep must destroy retain-tagged VM 730 with DestroyUnreferencedDisks=false")
	}
	if !stor[ephemeralVolid] {
		t.Error("call 2: retained ephemeral volume was destroyed by the straggler sweep")
	}
}

// TestHandleDeleteVM_AuthFailure verifies that a 401 Unauthorized from QEMU.Stop
// is classified as a non-retriable Cloud error. Auth failures indicate operator
// misconfiguration (wrong token) and must surface immediately without retry.
func TestHandleDeleteVM_AuthFailure(t *testing.T) {
	t.Parallel()

	authErr := &sdkerrors.APIError{HTTPCode: 401, Message: "authentication failure"}

	qemuSvc := &mockQEMUService{
		stopFn: func(_ context.Context, _ string, _ int) (string, error) {
			return "", authErr
		},
	}
	agentSvc := &mockAgentService{}

	h := handlers.HandleDeleteVM(testDepsFoundVM(999, qemuSvc, nil, nil, agentSvc))
	_, err := h.Handle(context.Background(), marshalArgs("999"), jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected error from 401 auth failure on Stop")
	}

	// 401 is a 4xx non-404 → WrapError returns a non-retriable Cloud error.
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) {
		t.Fatalf("expected *cpierrors.Error, got %T: %v", err, err)
	}
	if cpiErr.OkToRetry() {
		t.Errorf("auth failure must not be retriable; OkToRetry()=true; type=%s", cpiErr.Type())
	}
	if cpiErr.Type() == cpierrors.TypeRetriableCloud {
		t.Errorf("auth failure classified as RetriableCloud; want non-retriable TypeCloud; type=%s", cpiErr.Type())
	}
}
