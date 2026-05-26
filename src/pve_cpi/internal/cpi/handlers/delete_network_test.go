package handlers_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi/handlers"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	sdkcluster "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cluster"
	sdknodes "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/nodes"
	pveerr "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/errors"
)

// invokeDeleteNetwork marshals the cid string and calls the handler.
func invokeDeleteNetwork(t *testing.T, deps handlers.Deps, cid string) error {
	t.Helper()
	h := handlers.HandleDeleteNetwork(deps)
	raw, _ := json.Marshal(cid)
	_, err := h.Handle(context.Background(), []json.RawMessage{raw}, jsonrpc.Context{})
	return err
}

// testDeleteDeps builds Deps with the given clusterSvc; uses testConfig defaults.
func testDeleteDeps(clusterSvc sdkcluster.Service, autoManage bool, sdnZone string) handlers.Deps {
	cfg := testConfig()
	cfg.NetworkMode = "auto"
	cfg.SDNZone = sdnZone
	cfg.SDNAutoManageZone = autoManage
	return handlers.Deps{
		Config: cfg,
		PVE: &mockPVEClient{
			clusterSvc: clusterSvc,
			nodesSvc:   &mockNodesService{},
		},
		Logger: log.NewNopLogger(),
	}
}

// rawVnet returns a GetSdnVnetsResponse containing zone field.
func rawVnet(zone string) *sdkcluster.GetSdnVnetsResponse {
	b, _ := json.Marshal(map[string]any{"vnet": "net01", "zone": zone})
	raw := sdkcluster.GetSdnVnetsResponse(b)
	return &raw
}

// -- DN-01: SDN delete happy path (no subnets, zone not auto-managed) --

func TestHandleDeleteNetwork_SDN_HappyPath(t *testing.T) {
	var deleteVnetCalled bool
	var updateSdnCalled bool

	clusterSvc := &mockSDNCluster{
		getSdnVnetsFn: func(_ context.Context, vnet string, _ *sdkcluster.GetSdnVnetsParams) (*sdkcluster.GetSdnVnetsResponse, error) {
			return rawVnet("myzone"), nil
		},
		listSdnVnetsSubnetsFn: func(_ context.Context, _ string, _ *sdkcluster.ListSdnVnetsSubnetsParams) (*sdkcluster.ListSdnVnetsSubnetsResponse, error) {
			empty := sdkcluster.ListSdnVnetsSubnetsResponse{}
			return &empty, nil
		},
		deleteSdnVnetsFn: func(_ context.Context, vnet string, _ *sdkcluster.DeleteSdnVnetsParams) error {
			deleteVnetCalled = true
			if vnet != "net01" {
				t.Errorf("vnet: got %q, want net01", vnet)
			}
			return nil
		},
		updateSdnFn: func(_ context.Context, _ *sdkcluster.UpdateSdnParams) (*sdkcluster.UpdateSdnResponse, error) {
			updateSdnCalled = true
			return nil, nil
		},
	}

	err := invokeDeleteNetwork(t, testDeleteDeps(clusterSvc, false, ""), "net01")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !deleteVnetCalled {
		t.Error("DeleteSdnVnets must be called")
	}
	if !updateSdnCalled {
		t.Error("UpdateSdn must be called")
	}
}

// -- DN-02: SDN delete with subnets --

func TestHandleDeleteNetwork_SDN_WithSubnets(t *testing.T) {
	var deleteSubnetCalls []string

	subnetsResp := sdkcluster.ListSdnVnetsSubnetsResponse{
		json.RawMessage(`{"subnet":"10.0.0.0/24","type":"subnet"}`),
		json.RawMessage(`{"subnet":"10.1.0.0/24","type":"subnet"}`),
	}

	clusterSvc := &mockSDNCluster{
		getSdnVnetsFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnVnetsParams) (*sdkcluster.GetSdnVnetsResponse, error) {
			return rawVnet("z"), nil
		},
		listSdnVnetsSubnetsFn: func(_ context.Context, _ string, _ *sdkcluster.ListSdnVnetsSubnetsParams) (*sdkcluster.ListSdnVnetsSubnetsResponse, error) {
			return &subnetsResp, nil
		},
		deleteSdnVnetsSubnetsFn: func(_ context.Context, _ string, subnet string, _ *sdkcluster.DeleteSdnVnetsSubnetsParams) error {
			deleteSubnetCalls = append(deleteSubnetCalls, subnet)
			return nil
		},
	}

	err := invokeDeleteNetwork(t, testDeleteDeps(clusterSvc, false, ""), "net01")
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if len(deleteSubnetCalls) != 2 {
		t.Errorf("expected 2 subnet deletes, got %d", len(deleteSubnetCalls))
	}
}

// -- DN-03: SDN idempotent 404 (vnet not found → returns nil) --

func TestHandleDeleteNetwork_SDN_Idempotent404(t *testing.T) {
	var bridgeDeleteCalled bool

	clusterSvc := &mockSDNCluster{
		getSdnVnetsFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnVnetsParams) (*sdkcluster.GetSdnVnetsResponse, error) {
			return nil, sdnNotFound()
		},
	}
	nodesSvc := &mockBridgeNodes{
		deleteNetwork2Fn: func(_ context.Context, _ string, _ string) error {
			bridgeDeleteCalled = true
			return pveerr.ErrNotFound // bridge also gone
		},
	}
	cfg := testConfig()
	cfg.NetworkMode = "auto"
	cfg.Node = "pve1"
	deps := handlers.Deps{
		Config: cfg,
		PVE:    &mockPVEClient{clusterSvc: clusterSvc, nodesSvc: nodesSvc},
		Logger: log.NewNopLogger(),
	}
	err := invokeDeleteNetwork(t, deps, "net01")
	if err != nil {
		t.Fatalf("expected nil on idempotent 404, got: %v", err)
	}
	// Bridge path was taken since SDN said 404.
	if !bridgeDeleteCalled {
		t.Error("bridge delete must be attempted when SDN probe returns 404")
	}
}

// -- DN-04: Zone kept when pinned (zone == config.SDNZone) --

func TestHandleDeleteNetwork_SDN_ZoneKeptWhenPinned(t *testing.T) {
	var deleteZoneCalled bool
	clusterSvc := &mockSDNCluster{
		getSdnVnetsFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnVnetsParams) (*sdkcluster.GetSdnVnetsResponse, error) {
			return rawVnet("pinnedzone"), nil
		},
		listSdnVnetsSubnetsFn: func(_ context.Context, _ string, _ *sdkcluster.ListSdnVnetsSubnetsParams) (*sdkcluster.ListSdnVnetsSubnetsResponse, error) {
			empty := sdkcluster.ListSdnVnetsSubnetsResponse{}
			return &empty, nil
		},
		listSdnVnetsFn: func(_ context.Context, _ *sdkcluster.ListSdnVnetsParams) (*sdkcluster.ListSdnVnetsResponse, error) {
			empty := sdkcluster.ListSdnVnetsResponse{}
			return &empty, nil
		},
		deleteSdnZonesFn: func(_ context.Context, _ string, _ *sdkcluster.DeleteSdnZonesParams) error {
			deleteZoneCalled = true
			return nil
		},
	}
	// auto-manage true, but zone is pinned (== SDNZone)
	err := invokeDeleteNetwork(t, testDeleteDeps(clusterSvc, true, "pinnedzone"), "net01")
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if deleteZoneCalled {
		t.Error("pinned zone (config.SDNZone) must NOT be deleted even with auto-manage=true")
	}
}

// -- DN-05: Zone deleted when owned + empty --

func TestHandleDeleteNetwork_SDN_ZoneDeletedWhenOwnedAndEmpty(t *testing.T) {
	var deleteZoneCalled bool
	var applyAfterZoneCalled int

	clusterSvc := &mockSDNCluster{
		getSdnVnetsFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnVnetsParams) (*sdkcluster.GetSdnVnetsResponse, error) {
			return rawVnet("autozone"), nil
		},
		listSdnVnetsSubnetsFn: func(_ context.Context, _ string, _ *sdkcluster.ListSdnVnetsSubnetsParams) (*sdkcluster.ListSdnVnetsSubnetsResponse, error) {
			empty := sdkcluster.ListSdnVnetsSubnetsResponse{}
			return &empty, nil
		},
		listSdnVnetsFn: func(_ context.Context, _ *sdkcluster.ListSdnVnetsParams) (*sdkcluster.ListSdnVnetsResponse, error) {
			// Zero vnets remaining in zone after delete.
			empty := sdkcluster.ListSdnVnetsResponse{}
			return &empty, nil
		},
		deleteSdnZonesFn: func(_ context.Context, zone string, _ *sdkcluster.DeleteSdnZonesParams) error {
			deleteZoneCalled = true
			if zone != "autozone" {
				t.Errorf("zone: got %q, want autozone", zone)
			}
			return nil
		},
		updateSdnFn: func(_ context.Context, _ *sdkcluster.UpdateSdnParams) (*sdkcluster.UpdateSdnResponse, error) {
			applyAfterZoneCalled++
			return nil, nil
		},
	}
	// SDNZone="" so zone is not pinned; auto-manage=true
	err := invokeDeleteNetwork(t, testDeleteDeps(clusterSvc, true, ""), "net01")
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if !deleteZoneCalled {
		t.Error("zone must be deleted when auto-manage=true, not pinned, and zone is empty")
	}
	if applyAfterZoneCalled < 2 {
		t.Errorf("UpdateSdn must be called at least twice (after vnet delete, after zone delete); called %d times", applyAfterZoneCalled)
	}
}

// -- DN-06: Zone kept when remaining vnets exist --

func TestHandleDeleteNetwork_SDN_ZoneKeptWhenRemainingVnets(t *testing.T) {
	var deleteZoneCalled bool

	remainingVnets := sdkcluster.ListSdnVnetsResponse{
		json.RawMessage(`{"vnet":"other","zone":"autozone"}`),
	}

	clusterSvc := &mockSDNCluster{
		getSdnVnetsFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnVnetsParams) (*sdkcluster.GetSdnVnetsResponse, error) {
			return rawVnet("autozone"), nil
		},
		listSdnVnetsSubnetsFn: func(_ context.Context, _ string, _ *sdkcluster.ListSdnVnetsSubnetsParams) (*sdkcluster.ListSdnVnetsSubnetsResponse, error) {
			empty := sdkcluster.ListSdnVnetsSubnetsResponse{}
			return &empty, nil
		},
		listSdnVnetsFn: func(_ context.Context, _ *sdkcluster.ListSdnVnetsParams) (*sdkcluster.ListSdnVnetsResponse, error) {
			return &remainingVnets, nil
		},
		deleteSdnZonesFn: func(_ context.Context, _ string, _ *sdkcluster.DeleteSdnZonesParams) error {
			deleteZoneCalled = true
			return nil
		},
	}
	err := invokeDeleteNetwork(t, testDeleteDeps(clusterSvc, true, ""), "net01")
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if deleteZoneCalled {
		t.Error("zone must NOT be deleted when remaining vnets exist in zone")
	}
}

// -- DN-07: Bridge delete --

func TestHandleDeleteNetwork_Bridge_HappyPath(t *testing.T) {
	var bridgeDeleteCalled bool
	var updateNetworkCalled bool

	nodesSvc := &mockBridgeNodes{
		deleteNetwork2Fn: func(_ context.Context, node string, iface string) error {
			bridgeDeleteCalled = true
			if iface != "vmbr99" {
				t.Errorf("iface: got %q, want vmbr99", iface)
			}
			return nil
		},
		updateNetworkFn: func(_ context.Context, _ string, _ *sdknodes.UpdateNetworkParams) (*sdknodes.UpdateNetworkResponse, error) {
			updateNetworkCalled = true
			return nil, nil
		},
	}

	// SDN probe returns 404 → bridge fallback
	clusterSvc := &mockSDNCluster{
		getSdnVnetsFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnVnetsParams) (*sdkcluster.GetSdnVnetsResponse, error) {
			return nil, sdnNotFound()
		},
	}
	cfg := testConfig()
	cfg.Node = "pvenode"
	deps := handlers.Deps{
		Config: cfg,
		PVE:    &mockPVEClient{clusterSvc: clusterSvc, nodesSvc: nodesSvc},
		Logger: log.NewNopLogger(),
	}
	err := invokeDeleteNetwork(t, deps, "vmbr99")
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if !bridgeDeleteCalled {
		t.Error("DeleteNetwork2 must be called")
	}
	if !updateNetworkCalled {
		t.Error("UpdateNetwork must be called after bridge delete")
	}
}

// -- DN-07b: Bridge fallback when SDN probe returns the "does not exist"
// message rather than an HTTP 404. PVE's GET /cluster/sdn/vnets/<x> on a
// missing vnet returns a generic error (code 0) with this text, not a 404, so
// isSDNNotFound must detect it for the bridge fallback to trigger. --

func TestHandleDeleteNetwork_Bridge_SDNDoesNotExistMessage(t *testing.T) {
	var bridgeDeleteCalled bool

	nodesSvc := &mockBridgeNodes{
		deleteNetwork2Fn: func(_ context.Context, _ string, iface string) error {
			bridgeDeleteCalled = true
			if iface != "vmbr9" {
				t.Errorf("iface: got %q, want vmbr9", iface)
			}
			return nil
		},
		updateNetworkFn: func(_ context.Context, _ string, _ *sdknodes.UpdateNetworkParams) (*sdknodes.UpdateNetworkResponse, error) {
			return nil, nil
		},
	}
	clusterSvc := &mockSDNCluster{
		getSdnVnetsFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnVnetsParams) (*sdkcluster.GetSdnVnetsResponse, error) {
			return nil, errors.New(
				`cluster.GetSdnVnets: GET "/cluster/sdn/vnets/vmbr9" failed: ` +
					`API request failed: sdn vnet 'vmbr9' does not exist (code: 0)`)
		},
	}
	cfg := testConfig()
	cfg.Node = "pvenode"
	deps := handlers.Deps{
		Config: cfg,
		PVE:    &mockPVEClient{clusterSvc: clusterSvc, nodesSvc: nodesSvc},
		Logger: log.NewNopLogger(),
	}
	if err := invokeDeleteNetwork(t, deps, "vmbr9"); err != nil {
		t.Fatalf("unexpected error — bridge fallback should have run: %v", err)
	}
	if !bridgeDeleteCalled {
		t.Error("DeleteNetwork2 must be called via bridge fallback on SDN 'does not exist'")
	}
}

// -- DN-08: Bridge idempotent 404 --

func TestHandleDeleteNetwork_Bridge_Idempotent404(t *testing.T) {
	nodesSvc := &mockBridgeNodes{
		deleteNetwork2Fn: func(_ context.Context, _ string, _ string) error {
			return pveerr.ErrNotFound
		},
	}
	clusterSvc := &mockSDNCluster{
		getSdnVnetsFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnVnetsParams) (*sdkcluster.GetSdnVnetsResponse, error) {
			return nil, sdnNotFound()
		},
	}
	cfg := testConfig()
	cfg.Node = "pvenode"
	deps := handlers.Deps{
		Config: cfg,
		PVE:    &mockPVEClient{clusterSvc: clusterSvc, nodesSvc: nodesSvc},
		Logger: log.NewNopLogger(),
	}
	err := invokeDeleteNetwork(t, deps, "vmbr99")
	if err != nil {
		t.Fatalf("expected nil on idempotent 404, got: %v", err)
	}
}

// -- DN-09: Missing args --

func TestHandleDeleteNetwork_MissingArg(t *testing.T) {
	cfg := testConfig()
	deps := handlers.Deps{Config: cfg, Logger: log.NewNopLogger(), PVE: &mockPVEClient{clusterSvc: &mockSDNCluster{}}}
	h := handlers.HandleDeleteNetwork(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{}, jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error")
	}
	cpiErr := err.(*cpierrors.Error)
	if cpiErr.Type() != cpierrors.TypeCloud {
		t.Errorf("expected CloudError, got %q", cpiErr.Type())
	}
}

// -- Non-string CID --

func TestHandleDeleteNetwork_NonStringCID(t *testing.T) {
	cfg := testConfig()
	deps := handlers.Deps{Config: cfg, Logger: log.NewNopLogger(), PVE: &mockPVEClient{clusterSvc: &mockSDNCluster{}}}
	h := handlers.HandleDeleteNetwork(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{json.RawMessage(`12345`)}, jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error")
	}
	cpiErr := err.(*cpierrors.Error)
	if cpiErr.Type() != cpierrors.TypeCloud {
		t.Errorf("expected CloudError, got %q", cpiErr.Type())
	}
}

// -- Empty CID --

func TestHandleDeleteNetwork_EmptyCID(t *testing.T) {
	cfg := testConfig()
	deps := handlers.Deps{Config: cfg, Logger: log.NewNopLogger(), PVE: &mockPVEClient{clusterSvc: &mockSDNCluster{}}}
	h := handlers.HandleDeleteNetwork(deps)
	raw, _ := json.Marshal("   ")
	_, err := h.Handle(context.Background(), []json.RawMessage{raw}, jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error")
	}
	cpiErr := err.(*cpierrors.Error)
	if cpiErr.Type() != cpierrors.TypeCloud {
		t.Errorf("expected CloudError, got %q", cpiErr.Type())
	}
}

// -- subnet delete errors propagate (non-404) --

func TestHandleDeleteNetwork_SDN_SubnetDeleteError(t *testing.T) {
	subnetsResp := sdkcluster.ListSdnVnetsSubnetsResponse{
		json.RawMessage(`{"subnet":"10.0.0.0/24","type":"subnet"}`),
	}
	clusterSvc := &mockSDNCluster{
		getSdnVnetsFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnVnetsParams) (*sdkcluster.GetSdnVnetsResponse, error) {
			return rawVnet("z"), nil
		},
		listSdnVnetsSubnetsFn: func(_ context.Context, _ string, _ *sdkcluster.ListSdnVnetsSubnetsParams) (*sdkcluster.ListSdnVnetsSubnetsResponse, error) {
			return &subnetsResp, nil
		},
		deleteSdnVnetsSubnetsFn: func(_ context.Context, _ string, _ string, _ *sdkcluster.DeleteSdnVnetsSubnetsParams) error {
			return &pveerr.APIError{} // non-404 error
		},
	}
	err := invokeDeleteNetwork(t, testDeleteDeps(clusterSvc, false, ""), "net01")
	if err == nil {
		t.Fatal("expected error on subnet delete failure")
	}
	cpiErr := err.(*cpierrors.Error)
	if cpiErr.Type() != cpierrors.TypeCloud {
		t.Errorf("expected CloudError, got %q", cpiErr.Type())
	}
}

// -- SDN probe error (not 404) surfaces as error --

func TestHandleDeleteNetwork_SDN_ProbeError(t *testing.T) {
	clusterSvc := &mockSDNCluster{
		getSdnVnetsFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnVnetsParams) (*sdkcluster.GetSdnVnetsResponse, error) {
			return nil, &pveerr.APIError{} // non-404
		},
	}
	err := invokeDeleteNetwork(t, testDeleteDeps(clusterSvc, false, ""), "net01")
	if err == nil {
		t.Fatal("expected error on SDN probe failure")
	}
	cpiErr := err.(*cpierrors.Error)
	if cpiErr.Type() != cpierrors.TypeCloud {
		t.Errorf("expected CloudError, got %q", cpiErr.Type())
	}
}

// -- zone auto-manage=false: zone never deleted even when empty --

func TestHandleDeleteNetwork_SDN_ZoneNotDeletedWhenAutoManageFalse(t *testing.T) {
	var deleteZoneCalled bool
	clusterSvc := &mockSDNCluster{
		getSdnVnetsFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnVnetsParams) (*sdkcluster.GetSdnVnetsResponse, error) {
			return rawVnet("myzone"), nil
		},
		listSdnVnetsSubnetsFn: func(_ context.Context, _ string, _ *sdkcluster.ListSdnVnetsSubnetsParams) (*sdkcluster.ListSdnVnetsSubnetsResponse, error) {
			empty := sdkcluster.ListSdnVnetsSubnetsResponse{}
			return &empty, nil
		},
		listSdnVnetsFn: func(_ context.Context, _ *sdkcluster.ListSdnVnetsParams) (*sdkcluster.ListSdnVnetsResponse, error) {
			empty := sdkcluster.ListSdnVnetsResponse{}
			return &empty, nil
		},
		deleteSdnZonesFn: func(_ context.Context, _ string, _ *sdkcluster.DeleteSdnZonesParams) error {
			deleteZoneCalled = true
			return nil
		},
	}
	// auto-manage=false
	err := invokeDeleteNetwork(t, testDeleteDeps(clusterSvc, false, ""), "net01")
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if deleteZoneCalled {
		t.Error("zone must NOT be deleted when SDNAutoManageZone=false")
	}
}

// -- SDN delete vnet returns 404 (already gone): idempotent --

func TestHandleDeleteNetwork_SDN_VnetAlreadyGone(t *testing.T) {
	clusterSvc := &mockSDNCluster{
		getSdnVnetsFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnVnetsParams) (*sdkcluster.GetSdnVnetsResponse, error) {
			return rawVnet("z"), nil
		},
		listSdnVnetsSubnetsFn: func(_ context.Context, _ string, _ *sdkcluster.ListSdnVnetsSubnetsParams) (*sdkcluster.ListSdnVnetsSubnetsResponse, error) {
			empty := sdkcluster.ListSdnVnetsSubnetsResponse{}
			return &empty, nil
		},
		deleteSdnVnetsFn: func(_ context.Context, _ string, _ *sdkcluster.DeleteSdnVnetsParams) error {
			return pveerr.ErrNotFound // already gone
		},
	}
	err := invokeDeleteNetwork(t, testDeleteDeps(clusterSvc, false, ""), "net01")
	if err != nil {
		t.Fatalf("expected nil on idempotent vnet-already-gone, got: %v", err)
	}
}
