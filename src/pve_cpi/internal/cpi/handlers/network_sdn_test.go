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
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	sdkcluster "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cluster"
	sdktasks "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/tasks"
	sdkerrors "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/errors"
)

// invokeDeleteNetworkRaw calls HandleDeleteNetwork with raw CID bytes
// and returns the handler error.
func invokeDeleteNetworkRaw(t *testing.T, deps handlers.Deps, cidJSON json.RawMessage) error {
	t.Helper()
	return invokeDeleteNetworkRawCtx(t, context.Background(), deps, cidJSON)
}

// invokeDeleteNetworkRawCtx is invokeDeleteNetworkRaw with a caller-supplied
// context (tests use fastRetryCtx to zero the retry backoff curves).
func invokeDeleteNetworkRawCtx(t *testing.T, ctx context.Context, deps handlers.Deps, cidJSON json.RawMessage) error {
	t.Helper()
	h := handlers.HandleDeleteNetwork(deps)
	_, err := h.Handle(ctx, []json.RawMessage{cidJSON}, jsonrpc.Context{})
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

	type taskWaitCall struct {
		node string
		upid string
	}
	var updateSdnCalls int
	var taskWaitCalls []taskWaitCall

	tasksSvc := &mockTasksService{
		waitFn: func(_ context.Context, node, gotUPID string, _ *sdktasks.WaitOptions) (*sdktasks.Status, error) {
			taskWaitCalls = append(taskWaitCalls, taskWaitCall{node, gotUPID})
			return &sdktasks.Status{ExitStatus: "OK"}, nil
		},
	}

	clusterSvc := sdnDeleteOnlyCluster(func(_ context.Context, _ *sdkcluster.UpdateSdnParams) (*sdkcluster.UpdateSdnResponse, error) {
		updateSdnCalls++
		// Return UPID as a bare JSON string — the standard PVE encoding.
		b, _ := json.Marshal(upid)
		resp := sdkcluster.UpdateSdnResponse(b)
		return &resp, nil
	})

	cfg := testConfig()
	cfg.NetworkMode = "auto"
	// Zone teardown is out of scope here — pin auto-manage off so the
	// UPID path is the only SDN surface exercised.
	cfg.SDNAutoManageZone = boolPtr(false)
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
	if updateSdnCalls == 0 {
		t.Error("UpdateSdn must be called")
	}
	if len(taskWaitCalls) != 1 {
		t.Fatalf("Tasks.Wait: want 1 call, got %d", len(taskWaitCalls))
	}
	if taskWaitCalls[0].node != "pve-node1" {
		t.Errorf("Tasks.Wait node: got %q, want pve-node1", taskWaitCalls[0].node)
	}
	if taskWaitCalls[0].upid != upid {
		t.Errorf("Tasks.Wait upid: got %q, want %q", taskWaitCalls[0].upid, upid)
	}
}

// TestApplySDN_TransientFailure_IsRetriable pins applySDN's retriability
// contract: a 5xx from UpdateSdn (pvedaemon worker recycle mid-apply) must
// surface as a retriable error after the transient retry budget is
// exhausted — and the retry wrapper must actually re-attempt the call.
// Previously a raw SDK error was wrapped straight into a non-retriable
// CloudError, permanently failing the deploy on a condition that clears in
// about a second.
func TestApplySDN_TransientFailure_IsRetriable(t *testing.T) {
	t.Parallel()

	var updateSdnCalls int
	clusterSvc := sdnDeleteOnlyCluster(func(_ context.Context, _ *sdkcluster.UpdateSdnParams) (*sdkcluster.UpdateSdnResponse, error) {
		updateSdnCalls++
		return nil, sdkerrors.ParseAPIError(500, []byte(`{"message":"pvedaemon worker exiting"}`))
	})

	cfg := testConfig()
	cfg.NetworkMode = "auto"
	cfg.SDNAutoManageZone = boolPtr(false)

	deps := handlers.Deps{
		Config: cfg,
		PVE: &mockPVEClient{
			clusterSvc: clusterSvc,
			nodesSvc:   &mockNodesService{},
			tasksSvc:   &mockTasksService{},
		},
		Logger: log.NewNopLogger(),
	}

	cidJSON, _ := json.Marshal("net01")
	err := invokeDeleteNetworkRawCtx(t, fastRetryCtx(context.Background()), deps, cidJSON)
	if err == nil {
		t.Fatal("expected error from persistent UpdateSdn 5xx, got nil")
	}
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Errorf("5xx from UpdateSdn must be retriable, got %T %v", err, err)
	}
	if updateSdnCalls < 2 {
		t.Errorf("UpdateSdn calls = %d; the transient retry wrapper must re-attempt", updateSdnCalls)
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

	var updateSdnCalls int

	clusterSvc := sdnDeleteOnlyCluster(func(_ context.Context, _ *sdkcluster.UpdateSdnParams) (*sdkcluster.UpdateSdnResponse, error) {
		updateSdnCalls++
		b, _ := json.Marshal(malformedUPID)
		resp := sdkcluster.UpdateSdnResponse(b)
		return &resp, nil
	})

	cfg := testConfig()
	cfg.NetworkMode = "auto"
	// Zone teardown is out of scope here — pin auto-manage off so the
	// UPID path is the only SDN surface exercised.
	cfg.SDNAutoManageZone = boolPtr(false)
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
	if updateSdnCalls == 0 {
		t.Error("UpdateSdn must be called")
	}
	// Absence of a panic from tasksSvc (nil) confirms Tasks().Wait was never called.
}
