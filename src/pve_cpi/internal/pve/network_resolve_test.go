package pve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	sdkcluster "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cluster"
	sdknodes "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/nodes"
)

// nrFakeCluster is a minimal cluster.Service whose ListSdnVnets is driven by a
// per-call function so a test can model a vnet that is staged (pending) before
// it appears in running config, or that never converges.
type nrFakeCluster struct {
	sdkcluster.Service
	calls    int
	listVnet func(call int, p *sdkcluster.ListSdnVnetsParams) (*sdkcluster.ListSdnVnetsResponse, error)
}

func (f *nrFakeCluster) ListSdnVnets(
	_ context.Context, p *sdkcluster.ListSdnVnetsParams,
) (*sdkcluster.ListSdnVnetsResponse, error) {
	f.calls++
	return f.listVnet(f.calls, p)
}

// nrFakeNodes is a minimal nodes.Service whose ListNetwork is driven by a
// per-call function so a test can model a bridge that materializes on a node a
// few polls after it is requested.
type nrFakeNodes struct {
	sdknodes.Service
	calls   int
	listNet func(call int, node string) (*sdknodes.ListNetworkResponse, error)
}

func (f *nrFakeNodes) ListNetwork(
	_ context.Context, node string, _ *sdknodes.ListNetworkParams,
) (*sdknodes.ListNetworkResponse, error) {
	f.calls++
	return f.listNet(f.calls, node)
}

// nrFakeClient embeds Client (nil) so it satisfies the interface; only Cluster()
// and Nodes() are overridden, which is all the network-resolve path touches.
type nrFakeClient struct {
	Client
	cluster sdkcluster.Service
	nodes   sdknodes.Service
}

func (f *nrFakeClient) Cluster() sdkcluster.Service { return f.cluster }
func (f *nrFakeClient) Nodes() sdknodes.Service     { return f.nodes }

// vnetsResp builds a *ListSdnVnetsResponse from the given vnet names.
func vnetsResp(names ...string) *sdkcluster.ListSdnVnetsResponse {
	resp := make(sdkcluster.ListSdnVnetsResponse, 0, len(names))
	for _, n := range names {
		resp = append(resp, json.RawMessage(`{"vnet":"`+n+`","zone":"z1"}`))
	}
	return &resp
}

// ifacesResp builds a *ListNetworkResponse from the given interface names.
func ifacesResp(names ...string) *sdknodes.ListNetworkResponse {
	resp := make(sdknodes.ListNetworkResponse, 0, len(names))
	for _, n := range names {
		resp = append(resp, json.RawMessage(`{"iface":"`+n+`","type":"bridge"}`))
	}
	return &resp
}

// noStepClock is a lockClock that never advances now and whose sleep is a no-op,
// so retry-count exhaustion (not timeout) is the bound under test.
func noStepClock() lockClock {
	t := time.Unix(0, 0)
	return lockClock{
		now:   func() time.Time { return t },
		sleep: func(_ context.Context, _ time.Duration) error { return nil },
	}
}

// ---------------------------------------------------------------------------
// Produce side: WaitForSDNVnetConverged
// ---------------------------------------------------------------------------

func TestWaitForSDNVnetConverged_Disabled_NoCall(t *testing.T) {
	fc := &nrFakeCluster{listVnet: func(int, *sdkcluster.ListSdnVnetsParams) (*sdkcluster.ListSdnVnetsResponse, error) {
		t.Fatalf("ListSdnVnets must not be called when retries<=0")
		return nil, nil
	}}
	c := &nrFakeClient{cluster: fc}
	if err := WaitForSDNVnetConverged(context.Background(), c, "v1", 0, time.Minute); err != nil {
		t.Errorf("disabled: want nil, got %v", err)
	}
}

func TestWaitForSDNVnetConverged_PendingThenAvailable(t *testing.T) {
	fc := &nrFakeCluster{listVnet: func(call int, _ *sdkcluster.ListSdnVnetsParams) (*sdkcluster.ListSdnVnetsResponse, error) {
		if call < 2 {
			return vnetsResp("other"), nil // not yet converged
		}
		return vnetsResp("other", "v1"), nil // converged on 2nd poll
	}}
	c := &nrFakeClient{cluster: fc}
	if err := waitForSDNVnetConverged(context.Background(), c, "v1", 5, time.Minute, noStepClock()); err != nil {
		t.Errorf("converges on 2nd poll: want nil, got %v", err)
	}
	if fc.calls != 2 {
		t.Errorf("want 2 polls, got %d", fc.calls)
	}
}

func TestWaitForSDNVnetConverged_StaysPending_Retriable(t *testing.T) {
	fc := &nrFakeCluster{listVnet: func(int, *sdkcluster.ListSdnVnetsParams) (*sdkcluster.ListSdnVnetsResponse, error) {
		return vnetsResp("other"), nil // never converges
	}}
	c := &nrFakeClient{cluster: fc}
	err := waitForSDNVnetConverged(context.Background(), c, "v1", 2, time.Minute, noStepClock())
	if err == nil || !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Errorf("never converges: want retriable-cloud, got %v", err)
	}
	if fc.calls != 3 { // 1 initial + 2 retries
		t.Errorf("want 3 polls (1+2 retries), got %d", fc.calls)
	}
}

func TestWaitForSDNVnetConverged_Timeout_Retriable(t *testing.T) {
	fc := &nrFakeCluster{listVnet: func(int, *sdkcluster.ListSdnVnetsParams) (*sdkcluster.ListSdnVnetsResponse, error) {
		return vnetsResp("other"), nil
	}}
	c := &nrFakeClient{cluster: fc}
	// step (10s) far exceeds timeout (1s): poll 1 fails, the clock advances 10s
	// past the 1s deadline during the sleep, so poll 2 runs and then the deadline
	// check returns — exactly two polls, well short of the 1000-retry budget.
	err := waitForSDNVnetConverged(context.Background(), c, "v1", 1000, time.Second, fixedClock(time.Unix(0, 0), 10*time.Second))
	if err == nil || !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Errorf("timeout: want retriable-cloud, got %v", err)
	}
	if fc.calls != 2 {
		t.Errorf("deadline must trip after exactly two polls; got %d polls", fc.calls)
	}
}

func TestWaitForSDNVnetConverged_ListError_Propagates(t *testing.T) {
	fc := &nrFakeCluster{listVnet: func(int, *sdkcluster.ListSdnVnetsParams) (*sdkcluster.ListSdnVnetsResponse, error) {
		return nil, errors.New("boom")
	}}
	c := &nrFakeClient{cluster: fc}
	if err := waitForSDNVnetConverged(context.Background(), c, "v1", 3, time.Minute, noStepClock()); err == nil {
		t.Errorf("list error: want error, got nil")
	}
}

func TestWaitForSDNVnetConverged_NilClient_NoOp(t *testing.T) {
	if err := WaitForSDNVnetConverged(context.Background(), nil, "v1", 5, time.Minute); err != nil {
		t.Errorf("nil client: want nil, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Consume side: ResolveNodeBridgeOnNode
// ---------------------------------------------------------------------------

func TestResolveNodeBridgeOnNode_Disabled_NoCall(t *testing.T) {
	fc := &nrFakeCluster{listVnet: func(int, *sdkcluster.ListSdnVnetsParams) (*sdkcluster.ListSdnVnetsResponse, error) {
		t.Fatalf("must not list vnets when retries<=0")
		return nil, nil
	}}
	c := &nrFakeClient{cluster: fc}
	if err := ResolveNodeBridgeOnNode(context.Background(), c, "pve1", "v1", 0, time.Minute); err != nil {
		t.Errorf("disabled: want nil, got %v", err)
	}
}

func TestResolveNodeBridgeOnNode_NonSDNBridge_Skips(t *testing.T) {
	// vmbr0 is not an SDN vnet, so the gate must fail open without touching the
	// node network endpoint.
	fc := &nrFakeCluster{listVnet: func(int, *sdkcluster.ListSdnVnetsParams) (*sdkcluster.ListSdnVnetsResponse, error) {
		return vnetsResp("v1"), nil
	}}
	fn := &nrFakeNodes{listNet: func(int, string) (*sdknodes.ListNetworkResponse, error) {
		t.Fatalf("must not list node network for a non-SDN bridge")
		return nil, nil
	}}
	c := &nrFakeClient{cluster: fc, nodes: fn}
	if err := resolveNodeBridgeOnNode(context.Background(), c, "pve1", "vmbr0", 5, time.Minute, noStepClock()); err != nil {
		t.Errorf("external bridge: want nil, got %v", err)
	}
}

func TestResolveNodeBridgeOnNode_BridgePresent_Proceeds(t *testing.T) {
	fc := &nrFakeCluster{listVnet: func(int, *sdkcluster.ListSdnVnetsParams) (*sdkcluster.ListSdnVnetsResponse, error) {
		return vnetsResp("v1"), nil
	}}
	fn := &nrFakeNodes{listNet: func(int, string) (*sdknodes.ListNetworkResponse, error) {
		return ifacesResp("vmbr0", "v1"), nil
	}}
	c := &nrFakeClient{cluster: fc, nodes: fn}
	if err := resolveNodeBridgeOnNode(context.Background(), c, "pve1", "v1", 5, time.Minute, noStepClock()); err != nil {
		t.Errorf("bridge present: want nil, got %v", err)
	}
	if fn.calls != 1 {
		t.Errorf("want 1 node-network poll, got %d", fn.calls)
	}
}

func TestResolveNodeBridgeOnNode_BridgeAppearsWithinRetries(t *testing.T) {
	fc := &nrFakeCluster{listVnet: func(int, *sdkcluster.ListSdnVnetsParams) (*sdkcluster.ListSdnVnetsResponse, error) {
		return vnetsResp("v1"), nil
	}}
	fn := &nrFakeNodes{listNet: func(call int, _ string) (*sdknodes.ListNetworkResponse, error) {
		if call < 3 {
			return ifacesResp("vmbr0"), nil // bridge not yet on node
		}
		return ifacesResp("vmbr0", "v1"), nil // appears on 3rd poll
	}}
	c := &nrFakeClient{cluster: fc, nodes: fn}
	if err := resolveNodeBridgeOnNode(context.Background(), c, "pve1", "v1", 5, time.Minute, noStepClock()); err != nil {
		t.Errorf("bridge appears within retries: want nil, got %v", err)
	}
	if fn.calls != 3 {
		t.Errorf("want 3 node-network polls, got %d", fn.calls)
	}
}

func TestResolveNodeBridgeOnNode_BridgeAbsent_Retriable(t *testing.T) {
	fc := &nrFakeCluster{listVnet: func(int, *sdkcluster.ListSdnVnetsParams) (*sdkcluster.ListSdnVnetsResponse, error) {
		return vnetsResp("v1"), nil
	}}
	fn := &nrFakeNodes{listNet: func(int, string) (*sdknodes.ListNetworkResponse, error) {
		return ifacesResp("vmbr0"), nil // never appears
	}}
	c := &nrFakeClient{cluster: fc, nodes: fn}
	err := resolveNodeBridgeOnNode(context.Background(), c, "pve1", "v1", 2, time.Minute, noStepClock())
	if err == nil || !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Errorf("bridge absent: want retriable-cloud, got %v", err)
	}
	if fn.calls != 3 { // 1 initial + 2 retries
		t.Errorf("want 3 node-network polls, got %d", fn.calls)
	}
}

func TestResolveNodeBridgeOnNode_VnetListError_FailsOpen(t *testing.T) {
	// Cannot determine SDN membership → fail open rather than block the deploy.
	fc := &nrFakeCluster{listVnet: func(int, *sdkcluster.ListSdnVnetsParams) (*sdkcluster.ListSdnVnetsResponse, error) {
		return nil, errors.New("permission denied")
	}}
	c := &nrFakeClient{cluster: fc, nodes: &nrFakeNodes{}}
	if err := resolveNodeBridgeOnNode(context.Background(), c, "pve1", "v1", 5, time.Minute, noStepClock()); err != nil {
		t.Errorf("vnet list error: want fail-open (nil), got %v", err)
	}
}

func TestResolveNodeBridgeOnNode_NodeNetworkError_KeepsPolling(t *testing.T) {
	// A transient node-network read failure must not abort the deploy; it counts
	// as "not yet resolved" so the bounded poll keeps trying, then yields retriable.
	fc := &nrFakeCluster{listVnet: func(int, *sdkcluster.ListSdnVnetsParams) (*sdkcluster.ListSdnVnetsResponse, error) {
		return vnetsResp("v1"), nil
	}}
	fn := &nrFakeNodes{listNet: func(int, string) (*sdknodes.ListNetworkResponse, error) {
		return nil, errors.New("node unreachable")
	}}
	c := &nrFakeClient{cluster: fc, nodes: fn}
	err := resolveNodeBridgeOnNode(context.Background(), c, "pve1", "v1", 1, time.Minute, noStepClock())
	if err == nil || !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Errorf("node-network error: want retriable-cloud after budget, got %v", err)
	}
	if fn.calls != 4 { // 2 polls (1 initial + 1 retry) × (any_bridge attempt + plain-listing fallback), all errored
		t.Errorf("want 4 node-network calls, got %d", fn.calls)
	}
}

func TestResolveNodeBridgeOnNode_NilClient_NoOp(t *testing.T) {
	if err := ResolveNodeBridgeOnNode(context.Background(), nil, "pve1", "v1", 5, time.Minute); err != nil {
		t.Errorf("nil client: want nil, got %v", err)
	}
}

// nrParamsFakeNodes is nrFakeNodes with the ListNetworkParams surfaced, so a
// test can model PVE 9.2 semantics where the plain node-network listing omits
// SDN vnet interfaces and only type=any_bridge includes them.
type nrParamsFakeNodes struct {
	sdknodes.Service
	calls   int
	listNet func(call int, params *sdknodes.ListNetworkParams) (*sdknodes.ListNetworkResponse, error)
}

func (f *nrParamsFakeNodes) ListNetwork(
	_ context.Context, _ string, params *sdknodes.ListNetworkParams,
) (*sdknodes.ListNetworkResponse, error) {
	f.calls++
	return f.listNet(f.calls, params)
}

// TestResolveNodeBridgeOnNode_PVE9ListingNeedsAnyBridgeFilter: on PVE 9.2 the
// plain GET /nodes/<node>/network response contains only /etc/network/interfaces
// entries — realized SDN vnet bridges appear ONLY when type=any_bridge is
// passed. The realization poll must use that filter, or a live bridge is never
// observed and every SDN create_vm exhausts the retry budget.
func TestResolveNodeBridgeOnNode_PVE9ListingNeedsAnyBridgeFilter(t *testing.T) {
	fc := &nrFakeCluster{listVnet: func(int, *sdkcluster.ListSdnVnetsParams) (*sdkcluster.ListSdnVnetsResponse, error) {
		return vnetsResp("v1"), nil
	}}
	fn := &nrParamsFakeNodes{listNet: func(_ int, params *sdknodes.ListNetworkParams) (*sdknodes.ListNetworkResponse, error) {
		if params != nil && params.Type != nil && *params.Type == "any_bridge" {
			resp := sdknodes.ListNetworkResponse{
				json.RawMessage(`{"iface":"vmbr0","type":"bridge"}`),
				json.RawMessage(`{"iface":"v1","type":"vnet"}`),
			}
			return &resp, nil
		}
		return ifacesResp("vmbr0"), nil // plain listing: SDN vnet absent
	}}
	c := &nrFakeClient{cluster: fc, nodes: fn}
	if err := resolveNodeBridgeOnNode(context.Background(), c, "pve1", "v1", 3, time.Minute, noStepClock()); err != nil {
		t.Errorf("realized bridge visible only via any_bridge filter: want nil, got %v", err)
	}
}

// TestResolveNodeBridgeOnNode_AnyBridgeFilterRejected_FallsBack: a PVE release
// that rejects the type=any_bridge filter value must not wedge the gate — the
// poll falls back to the plain listing, which on such releases includes SDN
// vnet interfaces.
func TestResolveNodeBridgeOnNode_AnyBridgeFilterRejected_FallsBack(t *testing.T) {
	fc := &nrFakeCluster{listVnet: func(int, *sdkcluster.ListSdnVnetsParams) (*sdkcluster.ListSdnVnetsResponse, error) {
		return vnetsResp("v1"), nil
	}}
	fn := &nrParamsFakeNodes{listNet: func(_ int, params *sdknodes.ListNetworkParams) (*sdknodes.ListNetworkResponse, error) {
		if params != nil && params.Type != nil {
			return nil, fmt.Errorf("400 Parameter verification failed (type)")
		}
		return ifacesResp("vmbr0", "v1"), nil
	}}
	c := &nrFakeClient{cluster: fc, nodes: fn}
	if err := resolveNodeBridgeOnNode(context.Background(), c, "pve1", "v1", 3, time.Minute, noStepClock()); err != nil {
		t.Errorf("filter rejected: want fallback success, got %v", err)
	}
}
