package handlers_test

import (
	"context"
	"errors"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi/handlers"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
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

	stopCalled := false
	deleteCalled := false
	removeCalled := false

	qemuSvc := &mockQEMUService{
		stopFn: func(_ context.Context, node string, vmid int) (string, error) {
			if node != "pve-node1" || vmid != 101 {
				t.Errorf("Stop: unexpected node=%q vmid=%d", node, vmid)
			}
			stopCalled = true
			return "UPID:node:stop-task", nil
		},
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			// No unused entries — guard falls through to destroy.
			return map[string]any{}, nil
		},
	}
	nodesSvc := &mockNodesService{
		deleteQemuFn: func(_ context.Context, node string, vmid string, params *nodes.DeleteQemuParams) (*nodes.DeleteQemuResponse, error) {
			if node != "pve-node1" || vmid != "101" {
				t.Errorf("DeleteQemu: unexpected node=%q vmid=%q", node, vmid)
			}
			if params == nil || params.Purge == nil || !*params.Purge {
				t.Error("DeleteQemu: expected purge=true")
			}
			if params == nil || params.DestroyUnreferencedDisks == nil || !*params.DestroyUnreferencedDisks {
				t.Error("DeleteQemu: expected destroy-unreferenced-disks=true")
			}
			deleteCalled = true
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
			if node != "pve-node1" || vmid != 101 {
				t.Errorf("Remove: unexpected node=%q vmid=%d", node, vmid)
			}
			removeCalled = true
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
	if !stopCalled {
		t.Error("Stop was not called")
	}
	if !deleteCalled {
		t.Error("DeleteQemu was not called")
	}
	if !removeCalled {
		t.Error("Agent.Remove was not called")
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
	cpiErr, ok := err.(*cpierrors.Error)
	if !ok {
		t.Fatalf("expected *cpierrors.Error, got %T: %v", err, err)
	}
	if cpiErr.OkToRetry() {
		t.Errorf("auth failure must not be retriable; OkToRetry()=true; type=%s", cpiErr.Type())
	}
	if cpiErr.Type() == cpierrors.TypeRetriableCloud {
		t.Errorf("auth failure classified as RetriableCloud; want non-retriable TypeCloud; type=%s", cpiErr.Type())
	}
}
