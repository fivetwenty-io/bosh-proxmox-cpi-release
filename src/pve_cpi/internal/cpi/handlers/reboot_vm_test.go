package handlers_test

import (
	"context"
	"errors"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi/handlers"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/tasks"
)

// TestHandleRebootVM_Happy verifies Reset + AwaitTask are called and nil is returned.
func TestHandleRebootVM_Happy(t *testing.T) {
	t.Parallel()

	resetCalled := false
	awaitCalled := false

	qemuSvc := &mockQEMUService{
		resetFn: func(_ context.Context, node string, vmid int) (string, error) {
			if node != "pve-node1" || vmid != 101 {
				t.Errorf("Reset: unexpected node=%q vmid=%d", node, vmid)
			}
			resetCalled = true
			return "UPID:node:reset-task", nil
		},
	}
	tasksSvc := &mockTasksService{
		waitFn: func(_ context.Context, _, upid string, _ *tasks.WaitOptions) (*tasks.Status, error) {
			if upid != "UPID:node:reset-task" {
				t.Errorf("Wait: unexpected upid=%q", upid)
			}
			awaitCalled = true
			return &tasks.Status{ExitStatus: "OK"}, nil
		},
	}

	h := handlers.HandleRebootVM(testDeps(qemuSvc, nil, tasksSvc, &mockAgentService{}))
	result, err := h.Handle(context.Background(), marshalArgs("101"), jsonrpc.Context{})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != nil {
		t.Errorf("expected nil result, got %v", result)
	}
	if !resetCalled {
		t.Error("Reset was not called")
	}
	if !awaitCalled {
		t.Error("Tasks.Wait was not called")
	}
}

// TestHandleRebootVM_NotFound verifies 404 from Reset yields VMNotFound error.
func TestHandleRebootVM_NotFound(t *testing.T) {
	t.Parallel()

	qemuSvc := &mockQEMUService{
		resetFn: func(_ context.Context, _ string, _ int) (string, error) {
			return "", notFoundAPIErr()
		},
	}

	h := handlers.HandleRebootVM(testDeps(qemuSvc, nil, nil, &mockAgentService{}))
	_, err := h.Handle(context.Background(), marshalArgs("777"), jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected VMNotFound error, got nil")
	}
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) {
		t.Fatalf("expected *cpierrors.Error, got %T: %v", err, err)
	}
	if cpiErr.Type() != cpierrors.TypeVMNotFound {
		t.Errorf("error type = %q; want %q", cpiErr.Type(), cpierrors.TypeVMNotFound)
	}
	if cpiErr.OkToRetry() {
		t.Error("VMNotFound should not be retriable")
	}
}

// TestHandleRebootVM_GenericError verifies non-404 Reset errors are propagated as CloudError.
func TestHandleRebootVM_GenericError(t *testing.T) {
	t.Parallel()

	qemuSvc := &mockQEMUService{
		resetFn: func(_ context.Context, _ string, _ int) (string, error) {
			return "", errors.New("pve: kvm not running")
		},
	}

	h := handlers.HandleRebootVM(testDeps(qemuSvc, nil, nil, &mockAgentService{}))
	_, err := h.Handle(context.Background(), marshalArgs("101"), jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected error from Reset failure, got nil")
	}
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) {
		t.Errorf("expected *cpierrors.Error, got %T: %v", err, err)
	}
	if cpiErr.Type() == cpierrors.TypeVMNotFound {
		t.Error("generic error should not be VMNotFound")
	}
}

// TestHandleRebootVM_TaskFail verifies task await failure is propagated.
func TestHandleRebootVM_TaskFail(t *testing.T) {
	t.Parallel()

	qemuSvc := &mockQEMUService{
		resetFn: func(_ context.Context, _ string, _ int) (string, error) {
			return "UPID:node:reset-task", nil
		},
	}
	tasksSvc := &mockTasksService{
		waitFn: func(_ context.Context, _, _ string, _ *tasks.WaitOptions) (*tasks.Status, error) {
			return &tasks.Status{ExitStatus: "ERROR: kvm reset failed"}, nil
		},
	}

	h := handlers.HandleRebootVM(testDeps(qemuSvc, nil, tasksSvc, &mockAgentService{}))
	_, err := h.Handle(context.Background(), marshalArgs("101"), jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected error from task failure, got nil")
	}
}

// TestHandleRebootVM_NoUPID verifies empty UPID (synchronous success) is handled.
func TestHandleRebootVM_NoUPID(t *testing.T) {
	t.Parallel()

	qemuSvc := &mockQEMUService{
		resetFn: func(_ context.Context, _ string, _ int) (string, error) {
			return "", nil // no UPID — reset was synchronous or not tracked
		},
	}

	h := handlers.HandleRebootVM(testDeps(qemuSvc, nil, &mockTasksService{}, &mockAgentService{}))
	result, err := h.Handle(context.Background(), marshalArgs("101"), jsonrpc.Context{})

	if err != nil {
		t.Fatalf("unexpected error for empty UPID: %v", err)
	}
	if result != nil {
		t.Errorf("expected nil result, got %v", result)
	}
}

// TestHandleRebootVM_MissingVMCID verifies missing argument returns error.
func TestHandleRebootVM_MissingVMCID(t *testing.T) {
	t.Parallel()

	h := handlers.HandleRebootVM(testDeps(nil, nil, nil, &mockAgentService{}))
	_, err := h.Handle(context.Background(), nil, jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected error for missing vm_cid")
	}
}

// TestHandleRebootVM_InvalidVMCID verifies non-integer vm_cid returns error.
func TestHandleRebootVM_InvalidVMCID(t *testing.T) {
	t.Parallel()

	h := handlers.HandleRebootVM(testDeps(nil, nil, nil, &mockAgentService{}))
	_, err := h.Handle(context.Background(), marshalArgs("not-a-vmid"), jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected error for non-integer vm_cid")
	}
}
