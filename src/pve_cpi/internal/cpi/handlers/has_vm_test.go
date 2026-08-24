package handlers_test

import (
	"context"
	"errors"
	"testing"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
)

// TestHandleHasVM_Exists verifies true is returned when the cluster scan finds
// the VM. The handler no longer calls QEMU.Config; the cluster scan is the sole
// existence check.
func TestHandleHasVM_Exists(t *testing.T) {
	t.Parallel()

	// Cluster scan returns vmid 101 on "pve-node1".
	h := handlers.HandleHasVM(testDepsFoundVM(101, nil, nil, nil, &mockAgentService{}))
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

// TestHandleHasVM_NotExists verifies false is returned when the cluster scan
// returns no match for the VM and the per-node config probe proves the
// absence (404 on every node).
func TestHandleHasVM_NotExists(t *testing.T) {
	t.Parallel()

	// testDeps wires an empty cluster list; the authoritative per-node probe
	// then runs and must answer not-found for false to be returned.
	qemuSvc := &mockQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return nil, notFoundAPIErr()
		},
	}
	h := handlers.HandleHasVM(testDeps(qemuSvc, nil, nil, &mockAgentService{}))
	result, err := h.Handle(context.Background(), marshalArgs("999"), jsonrpc.Context{})

	if err != nil {
		t.Fatalf("cluster-not-found should yield false result, not error: %v", err)
	}
	got, ok := result.(bool)
	if !ok {
		t.Fatalf("expected bool result, got %T", result)
	}
	if got {
		t.Error("expected false (VM absent), got true")
	}
}

// TestHandleHasVM_ClusterTransportError verifies that a cluster scan transport
// error is propagated as a CPI error (caller may retry).
func TestHandleHasVM_ClusterTransportError(t *testing.T) {
	t.Parallel()

	transportErr := errors.New("network timeout")
	clusterSvc := &mockClusterSvc{
		listResourcesFn: func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			return nil, transportErr
		},
	}

	h := handlers.HandleHasVM(testDepsWithCluster(nil, nil, nil, &mockAgentService{}, &mockStorageService{}, clusterSvc))
	_, err := h.Handle(context.Background(), marshalArgs("101"), jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected error from cluster transport failure, got nil")
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

// TestHandleHasVM_MultiNode_FoundOnNonDefaultNode verifies that has_vm returns
// true when the cluster scan resolves the VM on a member other than the default
// node. The cluster contains three members; the target VM lives on pve03.
func TestHandleHasVM_MultiNode_FoundOnNonDefaultNode(t *testing.T) {
	t.Parallel()

	const vmid = 303
	const vmOnNode = "pve03"

	clusterSvc := defaultMultiNodeClusterSvc(vmid, vmOnNode)
	deps := testDepsWithCluster(nil, nil, nil, &mockAgentService{}, &mockStorageService{}, clusterSvc)

	h := handlers.HandleHasVM(deps)
	result, err := h.Handle(context.Background(), marshalArgs("303"), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got, ok := result.(bool)
	if !ok {
		t.Fatalf("expected bool result, got %T", result)
	}
	if !got {
		t.Error("expected true (VM exists on pve03), got false")
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
