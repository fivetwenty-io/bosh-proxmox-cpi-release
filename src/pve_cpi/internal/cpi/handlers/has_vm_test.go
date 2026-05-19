package handlers_test

import (
	"context"
	"errors"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi/handlers"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
)

// TestHandleHasVM_Exists verifies true is returned when VM config fetch succeeds.
func TestHandleHasVM_Exists(t *testing.T) {
	t.Parallel()

	qemuSvc := &mockQEMUService{
		configFn: func(_ context.Context, node string, vmid int) (map[string]interface{}, error) {
			if node != "pve-node1" || vmid != 101 {
				t.Errorf("Config: unexpected node=%q vmid=%d", node, vmid)
			}
			return map[string]interface{}{"cores": 2.0, "memory": 2048.0}, nil
		},
	}

	h := handlers.HandleHasVM(testDeps(qemuSvc, nil, nil, &mockAgentService{}))
	result, err := h.Handle(context.Background(), marshalArgs("101"), jsonrpc.Context{})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got, ok := result.(bool)
	if !ok {
		t.Fatalf("expected bool result, got %T", result)
	}
	if !got {
		t.Error("expected true (VM exists), got false")
	}
}

// TestHandleHasVM_NotExists verifies false is returned on 404.
func TestHandleHasVM_NotExists(t *testing.T) {
	t.Parallel()

	qemuSvc := &mockQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]interface{}, error) {
			return nil, notFoundAPIErr()
		},
	}

	h := handlers.HandleHasVM(testDeps(qemuSvc, nil, nil, &mockAgentService{}))
	result, err := h.Handle(context.Background(), marshalArgs("999"), jsonrpc.Context{})

	if err != nil {
		t.Fatalf("404 should yield false result, not error: %v", err)
	}
	got, ok := result.(bool)
	if !ok {
		t.Fatalf("expected bool result, got %T", result)
	}
	if got {
		t.Error("expected false (VM absent), got true")
	}
}

// TestHandleHasVM_SDKError verifies non-404 SDK errors are propagated.
func TestHandleHasVM_SDKError(t *testing.T) {
	t.Parallel()

	sdkErr := errors.New("network timeout")
	qemuSvc := &mockQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]interface{}, error) {
			return nil, sdkErr
		},
	}

	h := handlers.HandleHasVM(testDeps(qemuSvc, nil, nil, &mockAgentService{}))
	_, err := h.Handle(context.Background(), marshalArgs("101"), jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected error from SDK failure, got nil")
	}
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) {
		t.Errorf("expected *cpierrors.Error, got %T: %v", err, err)
	}
}

// TestHandleHasVM_MissingVMCID verifies missing argument returns error.
func TestHandleHasVM_MissingVMCID(t *testing.T) {
	t.Parallel()

	h := handlers.HandleHasVM(testDeps(nil, nil, nil, &mockAgentService{}))
	_, err := h.Handle(context.Background(), nil, jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected error for missing vm_cid")
	}
}

// TestHandleHasVM_InvalidVMCID verifies non-integer vm_cid returns error.
func TestHandleHasVM_InvalidVMCID(t *testing.T) {
	t.Parallel()

	h := handlers.HandleHasVM(testDeps(nil, nil, nil, &mockAgentService{}))
	_, err := h.Handle(context.Background(), marshalArgs("abc-xyz"), jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected error for non-integer vm_cid")
	}
}

// TestHandleHasVM_EmptyVMCID verifies empty string vm_cid returns error.
func TestHandleHasVM_EmptyVMCID(t *testing.T) {
	t.Parallel()

	h := handlers.HandleHasVM(testDeps(nil, nil, nil, &mockAgentService{}))
	_, err := h.Handle(context.Background(), marshalArgs(""), jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected error for empty vm_cid")
	}
}

// TestHandleHasVM_ZeroVMID verifies zero VMID is rejected.
func TestHandleHasVM_ZeroVMID(t *testing.T) {
	t.Parallel()

	h := handlers.HandleHasVM(testDeps(nil, nil, nil, &mockAgentService{}))
	_, err := h.Handle(context.Background(), marshalArgs("0"), jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected error for VMID=0")
	}
}
