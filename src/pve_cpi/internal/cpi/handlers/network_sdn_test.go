// Package handlers_test — unit tests for the applySDN helper in network_sdn.go.
//
// applySDN is unexported; both tests reach it via HandleDeleteNetwork, which
// calls applySDN after deleting a vnet. That path is the minimal SDN flow that
// always exercises applySDN exactly once.
package handlers_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	sdkcluster "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cluster"
	sdktasks "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/tasks"
)

// invokeDeleteNetworkRaw calls HandleDeleteNetwork with raw CID bytes
// and returns the handler error.
func invokeDeleteNetworkRaw(t *testing.T, deps handlers.Deps, cidJSON json.RawMessage) error {
	t.Helper()
	h := handlers.HandleDeleteNetwork(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{cidJSON}, jsonrpc.Context{})
	return err
}

// sdnDeleteOnlyCluster returns a mockSDNCluster wired for a minimal
// delete-vnet + applySDN flow. The updateSdnFn field is left to the caller so
// tests can control what UpdateSdn returns.
func sdnDeleteOnlyCluster(updateFn func(context.Context, *sdkcluster.UpdateSdnParams) (*sdkcluster.UpdateSdnResponse, error)) *mockSDNCluster {
	return &mockSDNCluster{
		// Probe: vnet exists (zone "z").
		getSdnVnetsFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnVnetsParams) (*sdkcluster.GetSdnVnetsResponse, error) {
			return rawVnet("z"), nil
		},
		// No subnets on the vnet.
		listSdnVnetsSubnetsFn: func(_ context.Context, _ string, _ *sdkcluster.ListSdnVnetsSubnetsParams) (*sdkcluster.ListSdnVnetsSubnetsResponse, error) {
			empty := sdkcluster.ListSdnVnetsSubnetsResponse{}
			return &empty, nil
		},
		// Delete vnet succeeds.
		deleteSdnVnetsFn: func(_ context.Context, _ string, _ *sdkcluster.DeleteSdnVnetsParams) error {
			return nil
		},
		// UpdateSdn (applySDN) is caller-controlled.
		updateSdnFn: updateFn,
	}
}

// TestApplySDN_HappyPath verifies that when UpdateSdn returns a well-formed
// UPID with a node field, applySDN awaits the task and the overall handler
// returns nil.
//
// The UPID format is "UPID:<node>:<rest>". Here node is "pve-node1" (matching
// testConfig().Node). The mockTasksService.Wait default returns ExitStatus "OK"
// so AwaitTask succeeds without extra configuration.
func TestApplySDN_HappyPath(t *testing.T) {
	t.Parallel()
	const upid = "UPID:pve-node1:00001234:00000001:aabbccdd:task:pve-node1:"

	var updateSdnCalled bool
	var taskWaitCalled bool

	tasksSvc := &mockTasksService{
		waitFn: func(_ context.Context, node, gotUPID string, _ *sdktasks.WaitOptions) (*sdktasks.Status, error) {
			taskWaitCalled = true
			if node != "pve-node1" {
				t.Errorf("Tasks.Wait node: got %q, want pve-node1", node)
			}
			if gotUPID != upid {
				t.Errorf("Tasks.Wait upid: got %q, want %q", gotUPID, upid)
			}
			return &sdktasks.Status{ExitStatus: "OK"}, nil
		},
	}

	clusterSvc := sdnDeleteOnlyCluster(func(_ context.Context, _ *sdkcluster.UpdateSdnParams) (*sdkcluster.UpdateSdnResponse, error) {
		updateSdnCalled = true
		// Return UPID as a bare JSON string — the standard PVE encoding.
		b, _ := json.Marshal(upid)
		resp := sdkcluster.UpdateSdnResponse(b)
		return &resp, nil
	})

	cfg := testConfig()
	cfg.NetworkMode = "auto"
	// Node matches the node field in the UPID.
	cfg.Node = "pve-node1"

	deps := handlers.Deps{
		Config: cfg,
		PVE: &mockPVEClient{
			clusterSvc: clusterSvc,
			nodesSvc:   &mockNodesService{},
			tasksSvc:   tasksSvc,
		},
		Logger: log.NewNopLogger(),
	}

	cidJSON, _ := json.Marshal("net01")
	if err := invokeDeleteNetworkRaw(t, deps, cidJSON); err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}
	if !updateSdnCalled {
		t.Error("UpdateSdn must be called")
	}
	if !taskWaitCalled {
		t.Error("Tasks.Wait must be called when UPID is present and node is known")
	}
}

// TestApplySDN_MalformedUPID verifies that when UpdateSdn returns a UPID
// whose node field (index 1 in "UPID:<node>:...") is empty AND
// config.Node is also empty, applySDN logs a warning and returns nil — it
// does NOT treat the missing node as a hard failure because the HTTP 200
// already confirmed the apply was accepted.
//
// The UPID "UPID::rest" has an empty node field (parts[1] == ""). With
// config.Node also empty the function hits the warn-and-return path.
func TestApplySDN_MalformedUPID(t *testing.T) {
	t.Parallel()
	const malformedUPID = "UPID::00001234:00000001:aabbccdd:task::"

	var updateSdnCalled bool

	clusterSvc := sdnDeleteOnlyCluster(func(_ context.Context, _ *sdkcluster.UpdateSdnParams) (*sdkcluster.UpdateSdnResponse, error) {
		updateSdnCalled = true
		b, _ := json.Marshal(malformedUPID)
		resp := sdkcluster.UpdateSdnResponse(b)
		return &resp, nil
	})

	cfg := testConfig()
	cfg.NetworkMode = "auto"
	// Empty node: applySDN cannot fall back to config.Node either.
	cfg.Node = ""

	deps := handlers.Deps{
		Config: cfg,
		PVE: &mockPVEClient{
			clusterSvc: clusterSvc,
			nodesSvc:   &mockNodesService{},
			// tasksSvc intentionally nil — Tasks().Wait must NOT be called.
		},
		Logger: log.NewNopLogger(),
	}

	cidJSON, _ := json.Marshal("net01")
	if err := invokeDeleteNetworkRaw(t, deps, cidJSON); err != nil {
		t.Fatalf("applySDN malformed-UPID path must return nil, got: %v", err)
	}
	if !updateSdnCalled {
		t.Error("UpdateSdn must be called")
	}
	// Absence of a panic from tasksSvc (nil) confirms Tasks().Wait was never called.
}
