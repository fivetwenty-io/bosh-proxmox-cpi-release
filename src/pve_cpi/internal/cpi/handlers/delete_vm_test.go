package handlers_test

import (
	"context"
	"encoding/json"
	"errors"
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
	if !deleteCalls[0].destroyUnref {
		t.Error("DeleteQemu: expected destroy-unreferenced-disks=true")
	}
	if len(removeCalls) != 1 {
		t.Fatalf("Agent.Remove: want 1 call, got %d", len(removeCalls))
	}
	if removeCalls[0].node != vmNode || removeCalls[0].vmid != 101 {
		t.Errorf("Agent.Remove: want node=%q vmid=101, got node=%q vmid=%d", vmNode, removeCalls[0].node, removeCalls[0].vmid)
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
// configured pve_disk_storage. The guard runs after Stop but before the
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

// TestHandleDeleteVM_AllowsWhenUnusedOnDifferentStorage verifies the guard
// fails CLOSED when an unusedN slot references a storage that does not match
// pve_disk_storage.
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
