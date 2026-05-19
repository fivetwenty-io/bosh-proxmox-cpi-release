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

// TestHandleDeleteVM_Happy verifies the stop→await→delete→agent.Remove path.
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
		configFn: func(_ context.Context, _ string, _ int) (map[string]interface{}, error) {
			// No unused entries — guard falls through to destroy.
			return map[string]interface{}{}, nil
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

	h := handlers.HandleDeleteVM(testDeps(qemuSvc, nodesSvc, tasksSvc, agentSvc))
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

// TestHandleDeleteVM_NotFound_AtStop verifies 404 during stop returns nil (idempotent).
func TestHandleDeleteVM_NotFound_AtStop(t *testing.T) {
	t.Parallel()

	qemuSvc := &mockQEMUService{
		stopFn: func(_ context.Context, _ string, _ int) (string, error) {
			return "", notFoundAPIErr()
		},
	}
	agentSvc := &mockAgentService{}

	h := handlers.HandleDeleteVM(testDeps(qemuSvc, nil, nil, agentSvc))
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
		configFn: func(_ context.Context, _ string, _ int) (map[string]interface{}, error) {
			return map[string]interface{}{}, nil
		},
	}
	nodesSvc := &mockNodesService{
		deleteQemuFn: func(_ context.Context, _ string, _ string, _ *nodes.DeleteQemuParams) (*nodes.DeleteQemuResponse, error) {
			return nil, notFoundAPIErr()
		},
	}
	tasksSvc := &mockTasksService{}
	agentSvc := &mockAgentService{}

	h := handlers.HandleDeleteVM(testDeps(qemuSvc, nodesSvc, tasksSvc, agentSvc))
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

	h := handlers.HandleDeleteVM(testDeps(qemuSvc, nil, nil, agentSvc))
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
		configFn: func(_ context.Context, _ string, _ int) (map[string]interface{}, error) {
			// Persistent disk demoted to unused0 — exactly the state that
			// would cause data loss if delete proceeded. testDeps configures
			// DiskStorage="local-lvm" so the unused volid must share that
			// storage prefix to trip the guard.
			return map[string]interface{}{
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

	h := handlers.HandleDeleteVM(testDeps(qemuSvc, nodesSvc, tasksSvc, agentSvc))
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

// TestHandleDeleteVM_AllowsWhenUnusedOnDifferentStorage verifies the guard
// only protects volumes on pve_disk_storage; unused entries on other
// storages (e.g., a leftover ISO mount) do not block destroy.
func TestHandleDeleteVM_AllowsWhenUnusedOnDifferentStorage(t *testing.T) {
	t.Parallel()

	deleteCalled := false
	qemuSvc := &mockQEMUService{
		stopFn: func(_ context.Context, _ string, _ int) (string, error) {
			return "", nil
		},
		configFn: func(_ context.Context, _ string, _ int) (map[string]interface{}, error) {
			return map[string]interface{}{
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

	h := handlers.HandleDeleteVM(testDeps(qemuSvc, nodesSvc, tasksSvc, agentSvc))
	if _, err := h.Handle(context.Background(), marshalArgs("101"), jsonrpc.Context{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !deleteCalled {
		t.Error("DeleteQemu should run when unused refs are not on disk storage")
	}
}

// TestHandleDeleteVM_AgentRemoveError verifies agent.Remove error is non-fatal.
func TestHandleDeleteVM_AgentRemoveError(t *testing.T) {
	t.Parallel()

	qemuSvc := &mockQEMUService{
		stopFn: func(_ context.Context, _ string, _ int) (string, error) {
			return "", nil // no UPID → skip await
		},
		configFn: func(_ context.Context, _ string, _ int) (map[string]interface{}, error) {
			return map[string]interface{}{}, nil
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

	h := handlers.HandleDeleteVM(testDeps(qemuSvc, nodesSvc, tasksSvc, agentSvc))
	result, err := h.Handle(context.Background(), marshalArgs("505"), jsonrpc.Context{})

	// Agent.Remove failure should NOT cause delete_vm to return an error.
	if err != nil {
		t.Fatalf("agent.Remove error should be non-fatal, got: %v", err)
	}
	if result != nil {
		t.Errorf("expected nil result, got %v", result)
	}
}
