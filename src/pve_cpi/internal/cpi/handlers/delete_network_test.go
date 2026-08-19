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
	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	pveerr "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
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
	cfg.SDNAutoManageZone = &autoManage
	return handlers.Deps{
		Config: cfg,
		PVE: &mockPVEClient{
			clusterSvc: clusterSvc,
			nodesSvc:   &mockNodesService{},
		},
		Logger: log.NewNopLogger(),
	}
}

// -- DN-01: SDN delete happy path (no subnets, zone not auto-managed) --

func TestHandleDeleteNetwork_SDN_HappyPath(t *testing.T) {
	t.Parallel()

	type deleteVnetCall struct{ vnet string }
	var deleteVnetCalls []deleteVnetCall
	var updateSdnCalls int

	clusterSvc := &mockSDNCluster{
		getSdnVnetsFn: func(_ context.Context, vnet string, _ *sdkcluster.GetSdnVnetsParams) (*sdkcluster.GetSdnVnetsResponse, error) {
			return rawVnet("myzone"), nil
		},
		listSdnVnetsSubnetsFn: func(_ context.Context, _ string, _ *sdkcluster.ListSdnVnetsSubnetsParams) (*sdkcluster.ListSdnVnetsSubnetsResponse, error) {
			empty := sdkcluster.ListSdnVnetsSubnetsResponse{}
			return &empty, nil
		},
		deleteSdnVnetsFn: func(_ context.Context, vnet string, _ *sdkcluster.DeleteSdnVnetsParams) error {
			deleteVnetCalls = append(deleteVnetCalls, deleteVnetCall{vnet})
			return nil
		},
		updateSdnFn: func(_ context.Context, _ *sdkcluster.UpdateSdnParams) (*sdkcluster.UpdateSdnResponse, error) {
			updateSdnCalls++
			return nil, nil
		},
	}

	err := invokeDeleteNetwork(t, testDeleteDeps(clusterSvc, false, ""), "net01")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(deleteVnetCalls) != 1 {
		t.Fatalf("DeleteSdnVnets: want 1 call, got %d", len(deleteVnetCalls))
	}
	if deleteVnetCalls[0].vnet != "net01" {
		t.Errorf("DeleteSdnVnets: want vnet=net01, got %q", deleteVnetCalls[0].vnet)
	}
	if updateSdnCalls == 0 {
		t.Error("UpdateSdn must be called")
	}
}

// -- DN-02: SDN delete with subnets --

func TestHandleDeleteNetwork_SDN_WithSubnets(t *testing.T) {
	t.Parallel()
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
		// Opt in to vnet delete + apply mutations the SDN delete
		// path runs after subnet teardown.
		deleteSdnVnetsFn: func(_ context.Context, _ string, _ *sdkcluster.DeleteSdnVnetsParams) error {
			return nil
		},
		updateSdnFn: func(_ context.Context, _ *sdkcluster.UpdateSdnParams) (*sdkcluster.UpdateSdnResponse, error) {
			return nil, nil
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
	t.Parallel()

	var bridgeDeleteCalls []struct{}

	clusterSvc := &mockSDNCluster{
		getSdnVnetsFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnVnetsParams) (*sdkcluster.GetSdnVnetsResponse, error) {
			return nil, sdnNotFound()
		},
	}
	nodesSvc := &mockBridgeNodes{
		deleteNetwork2Fn: func(_ context.Context, _ string, _ string) error {
			bridgeDeleteCalls = append(bridgeDeleteCalls, struct{}{})
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
	if len(bridgeDeleteCalls) == 0 {
		t.Error("bridge delete must be attempted when SDN probe returns 404")
	}
}

// -- DN-04: Zone kept when pinned (zone == config.SDNZone) --

func TestHandleDeleteNetwork_SDN_ZoneKeptWhenPinned(t *testing.T) {
	t.Parallel()
	var deleteZoneCalls []struct{}
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
			deleteZoneCalls = append(deleteZoneCalls, struct{}{})
			return nil
		},
		// Opt in to vnet delete + apply mutations.
		deleteSdnVnetsFn: func(_ context.Context, _ string, _ *sdkcluster.DeleteSdnVnetsParams) error {
			return nil
		},
		updateSdnFn: func(_ context.Context, _ *sdkcluster.UpdateSdnParams) (*sdkcluster.UpdateSdnResponse, error) {
			return nil, nil
		},
	}
	// auto-manage true, but zone is pinned (== SDNZone)
	err := invokeDeleteNetwork(t, testDeleteDeps(clusterSvc, true, "pinnedzone"), "net01")
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if len(deleteZoneCalls) != 0 {
		t.Errorf("pinned zone (config.SDNZone) must NOT be deleted even with auto-manage=true; got %d call(s)", len(deleteZoneCalls))
	}
}

// -- DN-05: Zone deleted when owned + empty --

func TestHandleDeleteNetwork_SDN_ZoneDeletedWhenOwnedAndEmpty(t *testing.T) {
	t.Parallel()

	type deleteZoneCall struct{ zone string }
	var deleteZoneCalls []deleteZoneCall
	var applyAfterZoneCalled int

	clusterSvc := &mockSDNCluster{
		getSdnVnetsFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnVnetsParams) (*sdkcluster.GetSdnVnetsResponse, error) {
			return rawVnet("autozone"), nil
		},
		getSdnZonesFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnZonesParams) (*sdkcluster.GetSdnZonesResponse, error) {
			raw := sdkcluster.GetSdnZonesResponse(`{"zone":"autozone","type":"vxlan"}`)
			return &raw, nil
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
			deleteZoneCalls = append(deleteZoneCalls, deleteZoneCall{zone})
			return nil
		},
		updateSdnFn: func(_ context.Context, _ *sdkcluster.UpdateSdnParams) (*sdkcluster.UpdateSdnResponse, error) {
			applyAfterZoneCalled++
			return nil, nil
		},
		// Opt in to vnet delete — the delete path must call this
		// before the zone teardown branch is reached.
		deleteSdnVnetsFn: func(_ context.Context, _ string, _ *sdkcluster.DeleteSdnVnetsParams) error {
			return nil
		},
	}
	// SDNZone="" so zone is not pinned; auto-manage=true
	err := invokeDeleteNetwork(t, testDeleteDeps(clusterSvc, true, ""), "net01")
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if len(deleteZoneCalls) == 0 {
		t.Error("zone must be deleted when auto-manage=true, not pinned, and zone is empty")
	} else if deleteZoneCalls[0].zone != "autozone" {
		t.Errorf("DeleteSdnZones: want zone=autozone, got %q", deleteZoneCalls[0].zone)
	}
	if applyAfterZoneCalled < 2 {
		t.Errorf("UpdateSdn must be called at least twice (after vnet delete, after zone delete); called %d times", applyAfterZoneCalled)
	}
}

// -- DN-06: Zone kept when remaining vnets exist --

func TestHandleDeleteNetwork_SDN_ZoneKeptWhenRemainingVnets(t *testing.T) {
	t.Parallel()

	var deleteZoneCalls []struct{}

	remainingVnets := sdkcluster.ListSdnVnetsResponse{
		json.RawMessage(`{"vnet":"other","zone":"autozone"}`),
	}

	clusterSvc := &mockSDNCluster{
		getSdnVnetsFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnVnetsParams) (*sdkcluster.GetSdnVnetsResponse, error) {
			return rawVnet("autozone"), nil
		},
		getSdnZonesFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnZonesParams) (*sdkcluster.GetSdnZonesResponse, error) {
			raw := sdkcluster.GetSdnZonesResponse(`{"zone":"autozone","type":"vxlan"}`)
			return &raw, nil
		},
		listSdnVnetsSubnetsFn: func(_ context.Context, _ string, _ *sdkcluster.ListSdnVnetsSubnetsParams) (*sdkcluster.ListSdnVnetsSubnetsResponse, error) {
			empty := sdkcluster.ListSdnVnetsSubnetsResponse{}
			return &empty, nil
		},
		listSdnVnetsFn: func(_ context.Context, _ *sdkcluster.ListSdnVnetsParams) (*sdkcluster.ListSdnVnetsResponse, error) {
			return &remainingVnets, nil
		},
		deleteSdnZonesFn: func(_ context.Context, _ string, _ *sdkcluster.DeleteSdnZonesParams) error {
			deleteZoneCalls = append(deleteZoneCalls, struct{}{})
			return nil
		},
		// Opt in to vnet delete + apply mutations.
		deleteSdnVnetsFn: func(_ context.Context, _ string, _ *sdkcluster.DeleteSdnVnetsParams) error {
			return nil
		},
		updateSdnFn: func(_ context.Context, _ *sdkcluster.UpdateSdnParams) (*sdkcluster.UpdateSdnResponse, error) {
			return nil, nil
		},
	}
	err := invokeDeleteNetwork(t, testDeleteDeps(clusterSvc, true, ""), "net01")
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if len(deleteZoneCalls) != 0 {
		t.Errorf("zone must NOT be deleted when remaining vnets exist in zone; got %d call(s)", len(deleteZoneCalls))
	}
}

// -- DN-06b: EVPN zone never deleted, even when empty --

func TestHandleDeleteNetwork_SDN_EVPNZoneNeverDeleted(t *testing.T) {
	t.Parallel()

	var deleteZoneCalls int

	clusterSvc := &mockSDNCluster{
		getSdnVnetsFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnVnetsParams) (*sdkcluster.GetSdnVnetsResponse, error) {
			return rawVnet("evpnz"), nil
		},
		getSdnZonesFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnZonesParams) (*sdkcluster.GetSdnZonesResponse, error) {
			raw := sdkcluster.GetSdnZonesResponse(`{"zone":"evpnz","type":"evpn"}`)
			return &raw, nil
		},
		listSdnVnetsSubnetsFn: func(_ context.Context, _ string, _ *sdkcluster.ListSdnVnetsSubnetsParams) (*sdkcluster.ListSdnVnetsSubnetsResponse, error) {
			empty := sdkcluster.ListSdnVnetsSubnetsResponse{}
			return &empty, nil
		},
		deleteSdnZonesFn: func(_ context.Context, _ string, _ *sdkcluster.DeleteSdnZonesParams) error {
			deleteZoneCalls++
			return nil
		},
		deleteSdnVnetsFn: func(_ context.Context, _ string, _ *sdkcluster.DeleteSdnVnetsParams) error {
			return nil
		},
		updateSdnFn: func(_ context.Context, _ *sdkcluster.UpdateSdnParams) (*sdkcluster.UpdateSdnResponse, error) {
			return nil, nil
		},
	}
	// auto-manage on, zone unpinned, zero remaining vnets — every non-EVPN
	// condition for teardown holds, yet the EVPN guard must retain the zone
	// (before the vnet-emptiness scan: listSdnVnetsFn deliberately nil).
	if err := invokeDeleteNetwork(t, testDeleteDeps(clusterSvc, true, ""), "net01"); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if deleteZoneCalls != 0 {
		t.Errorf("EVPN zone must never be deleted; got %d call(s)", deleteZoneCalls)
	}
}

// -- DN-06c: zone-type lookup failure fails closed against deletion --

func TestHandleDeleteNetwork_SDN_ZoneTypeLookupError_NotDeleted(t *testing.T) {
	t.Parallel()

	var deleteZoneCalls int

	clusterSvc := &mockSDNCluster{
		getSdnVnetsFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnVnetsParams) (*sdkcluster.GetSdnVnetsResponse, error) {
			return rawVnet("autozone"), nil
		},
		getSdnZonesFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnZonesParams) (*sdkcluster.GetSdnZonesResponse, error) {
			return nil, errors.New("pvedaemon hiccup")
		},
		listSdnVnetsSubnetsFn: func(_ context.Context, _ string, _ *sdkcluster.ListSdnVnetsSubnetsParams) (*sdkcluster.ListSdnVnetsSubnetsResponse, error) {
			empty := sdkcluster.ListSdnVnetsSubnetsResponse{}
			return &empty, nil
		},
		deleteSdnZonesFn: func(_ context.Context, _ string, _ *sdkcluster.DeleteSdnZonesParams) error {
			deleteZoneCalls++
			return nil
		},
		deleteSdnVnetsFn: func(_ context.Context, _ string, _ *sdkcluster.DeleteSdnVnetsParams) error {
			return nil
		},
		updateSdnFn: func(_ context.Context, _ *sdkcluster.UpdateSdnParams) (*sdkcluster.UpdateSdnResponse, error) {
			return nil, nil
		},
	}
	if err := invokeDeleteNetwork(t, testDeleteDeps(clusterSvc, true, ""), "net01"); err != nil {
		t.Fatalf("type-lookup failure must not fail the delete: %v", err)
	}
	if deleteZoneCalls != 0 {
		t.Errorf("zone must not be deleted when its type cannot be confirmed; got %d call(s)", deleteZoneCalls)
	}
}

// -- DN-07: Bridge delete --

func TestHandleDeleteNetwork_Bridge_HappyPath(t *testing.T) {
	t.Parallel()

	type bridgeDeleteCall struct{ iface string }
	var bridgeDeleteCalls []bridgeDeleteCall
	var updateNetCalls int

	nodesSvc := &mockBridgeNodes{
		deleteNetwork2Fn: func(_ context.Context, node string, iface string) error {
			bridgeDeleteCalls = append(bridgeDeleteCalls, bridgeDeleteCall{iface})
			return nil
		},
		updateNetworkFn: func(_ context.Context, _ string, _ *sdknodes.UpdateNetworkParams) (*sdknodes.UpdateNetworkResponse, error) {
			updateNetCalls++
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
	if len(bridgeDeleteCalls) != 1 {
		t.Fatalf("DeleteNetwork2: want 1 call, got %d", len(bridgeDeleteCalls))
	}
	if bridgeDeleteCalls[0].iface != "vmbr99" {
		t.Errorf("DeleteNetwork2: want iface=vmbr99, got %q", bridgeDeleteCalls[0].iface)
	}
	if updateNetCalls == 0 {
		t.Error("UpdateNetwork must be called after bridge delete")
	}
}

// -- DN-07b: Bridge fallback when SDN probe returns the "does not exist"
// message rather than an HTTP 404. PVE's GET /cluster/sdn/vnets/<x> on a
// missing vnet returns a generic error (code 0) with this text, not a 404, so
// isSDNNotFound must detect it for the bridge fallback to trigger. --

func TestHandleDeleteNetwork_Bridge_SDNDoesNotExistMessage(t *testing.T) {
	t.Parallel()

	type bridgeDeleteCall struct{ iface string }
	var bridgeDeleteCalls []bridgeDeleteCall

	nodesSvc := &mockBridgeNodes{
		deleteNetwork2Fn: func(_ context.Context, _ string, iface string) error {
			bridgeDeleteCalls = append(bridgeDeleteCalls, bridgeDeleteCall{iface})
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
	if len(bridgeDeleteCalls) == 0 {
		t.Error("DeleteNetwork2 must be called via bridge fallback on SDN 'does not exist'")
	} else if bridgeDeleteCalls[0].iface != "vmbr9" {
		t.Errorf("DeleteNetwork2: want iface=vmbr9, got %q", bridgeDeleteCalls[0].iface)
	}
}

// -- DN-08: Bridge idempotent 404 --

func TestHandleDeleteNetwork_Bridge_Idempotent404(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	cfg := testConfig()
	deps := handlers.Deps{Config: cfg, Logger: log.NewNopLogger(), PVE: &mockPVEClient{clusterSvc: &mockSDNCluster{}}}
	h := handlers.HandleDeleteNetwork(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{}, jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error")
	}
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) {
		t.Fatalf("expected *cpierrors.Error, got %T: %v", err, err)
	}
	if cpiErr.Type() != cpierrors.TypeCloud {
		t.Errorf("expected CloudError, got %q", cpiErr.Type())
	}
}

// -- Non-string CID --

func TestHandleDeleteNetwork_NonStringCID(t *testing.T) {
	t.Parallel()
	cfg := testConfig()
	deps := handlers.Deps{Config: cfg, Logger: log.NewNopLogger(), PVE: &mockPVEClient{clusterSvc: &mockSDNCluster{}}}
	h := handlers.HandleDeleteNetwork(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{json.RawMessage(`12345`)}, jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error")
	}
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) {
		t.Fatalf("expected *cpierrors.Error, got %T: %v", err, err)
	}
	if cpiErr.Type() != cpierrors.TypeCloud {
		t.Errorf("expected CloudError, got %q", cpiErr.Type())
	}
}

// -- Empty CID --

func TestHandleDeleteNetwork_EmptyCID(t *testing.T) {
	t.Parallel()
	cfg := testConfig()
	deps := handlers.Deps{Config: cfg, Logger: log.NewNopLogger(), PVE: &mockPVEClient{clusterSvc: &mockSDNCluster{}}}
	h := handlers.HandleDeleteNetwork(deps)
	raw, _ := json.Marshal("   ")
	_, err := h.Handle(context.Background(), []json.RawMessage{raw}, jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error")
	}
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) {
		t.Fatalf("expected *cpierrors.Error, got %T: %v", err, err)
	}
	if cpiErr.Type() != cpierrors.TypeCloud {
		t.Errorf("expected CloudError, got %q", cpiErr.Type())
	}
}

// -- subnet delete errors propagate (non-404) --

func TestHandleDeleteNetwork_SDN_SubnetDeleteError(t *testing.T) {
	t.Parallel()
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
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) {
		t.Fatalf("expected *cpierrors.Error, got %T: %v", err, err)
	}
	if cpiErr.Type() != cpierrors.TypeCloud {
		t.Errorf("expected CloudError, got %q", cpiErr.Type())
	}
}

// -- SDN probe error (not 404) surfaces as error --

func TestHandleDeleteNetwork_SDN_ProbeError(t *testing.T) {
	t.Parallel()
	clusterSvc := &mockSDNCluster{
		getSdnVnetsFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnVnetsParams) (*sdkcluster.GetSdnVnetsResponse, error) {
			return nil, &pveerr.APIError{} // non-404
		},
	}
	err := invokeDeleteNetwork(t, testDeleteDeps(clusterSvc, false, ""), "net01")
	if err == nil {
		t.Fatal("expected error on SDN probe failure")
	}
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) {
		t.Fatalf("expected *cpierrors.Error, got %T: %v", err, err)
	}
	if cpiErr.Type() != cpierrors.TypeCloud {
		t.Errorf("expected CloudError, got %q", cpiErr.Type())
	}
}

// -- zone auto-manage=false: zone never deleted even when empty --

func TestHandleDeleteNetwork_SDN_ZoneNotDeletedWhenAutoManageFalse(t *testing.T) {
	t.Parallel()

	var deleteZoneCalls []struct{}
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
			deleteZoneCalls = append(deleteZoneCalls, struct{}{})
			return nil
		},
		// Opt in to vnet delete + apply mutations.
		deleteSdnVnetsFn: func(_ context.Context, _ string, _ *sdkcluster.DeleteSdnVnetsParams) error {
			return nil
		},
		updateSdnFn: func(_ context.Context, _ *sdkcluster.UpdateSdnParams) (*sdkcluster.UpdateSdnResponse, error) {
			return nil, nil
		},
	}
	// auto-manage=false
	err := invokeDeleteNetwork(t, testDeleteDeps(clusterSvc, false, ""), "net01")
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if len(deleteZoneCalls) != 0 {
		t.Errorf("zone must NOT be deleted when SDNAutoManageZone=false; got %d call(s)", len(deleteZoneCalls))
	}
}

// -- SDN delete vnet returns 404 (already gone): idempotent --

func TestHandleDeleteNetwork_SDN_VnetAlreadyGone(t *testing.T) {
	t.Parallel()
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
		// Opt in to apply mutation — delete path always calls
		// UpdateSdn after the (idempotent) vnet delete.
		updateSdnFn: func(_ context.Context, _ *sdkcluster.UpdateSdnParams) (*sdkcluster.UpdateSdnResponse, error) {
			return nil, nil
		},
	}
	err := invokeDeleteNetwork(t, testDeleteDeps(clusterSvc, false, ""), "net01")
	if err != nil {
		t.Fatalf("expected nil on idempotent vnet-already-gone, got: %v", err)
	}
}

// -- TestDeleteNetwork_NotFound_Idempotent --
//
// Verifies the end-to-end idempotency contract: when the SDN probe reports
// the vnet is absent AND the bridge fallback also returns 404, the handler
// must report success. This is the canonical "already deleted by a prior
// run" shape that BOSH retries can hit.

func TestDeleteNetwork_NotFound_Idempotent(t *testing.T) {
	t.Parallel()
	clusterSvc := &mockSDNCluster{
		getSdnVnetsFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnVnetsParams) (*sdkcluster.GetSdnVnetsResponse, error) {
			return nil, sdnNotFound()
		},
	}
	nodesSvc := &mockBridgeNodes{
		deleteNetwork2Fn: func(_ context.Context, _ string, _ string) error {
			return pveerr.ErrNotFound
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
	if err := invokeDeleteNetwork(t, deps, "anything"); err != nil {
		t.Fatalf("idempotent NotFound must return nil; got: %v", err)
	}
}

// -- TestDeleteNetwork_ZoneAutoDelete_OnlyWhenAllConditionsHold --
//
// Table-driven test for the zone auto-delete guards in
// maybeDeleteOrphanedZone. The zone must only be deleted when ALL of the
// following hold:
//  1. config.SDNAutoManageZone is enabled
//  2. zone observed on vnet != config.SDNZone (the pinned zone is preserved)
//  3. the zone is not an EVPN zone (covered by the dedicated EVPN test)
//  4. no remaining vnets reference the zone
//
// Each row asserts whether DeleteSdnZones was called.

func TestDeleteNetwork_ZoneAutoDelete_OnlyWhenAllConditionsHold(t *testing.T) {
	t.Parallel()
	type row struct {
		name              string
		autoManage        bool
		configSDNZone     string
		observedZone      string
		remainingVnets    sdkcluster.ListSdnVnetsResponse
		wantDeleteZone    bool
		wantApplyAfterMin int // minimum UpdateSdn calls (1 for vnet delete; 2 if zone delete too)
	}

	rows := []row{
		{
			name:              "auto_manage_off_never_deletes_zone",
			autoManage:        false,
			configSDNZone:     "",
			observedZone:      "anyzone",
			remainingVnets:    sdkcluster.ListSdnVnetsResponse{},
			wantDeleteZone:    false,
			wantApplyAfterMin: 1,
		},
		{
			name:              "auto_manage_on_but_zone_is_pinned_config_zone",
			autoManage:        true,
			configSDNZone:     "pinned",
			observedZone:      "pinned",
			remainingVnets:    sdkcluster.ListSdnVnetsResponse{},
			wantDeleteZone:    false,
			wantApplyAfterMin: 1,
		},
		{
			name:          "auto_manage_on_zone_unpinned_but_other_vnets_remain",
			autoManage:    true,
			configSDNZone: "",
			observedZone:  "shared",
			remainingVnets: sdkcluster.ListSdnVnetsResponse{
				json.RawMessage(`{"vnet":"other","zone":"shared"}`),
			},
			wantDeleteZone:    false,
			wantApplyAfterMin: 1,
		},
		{
			name:              "auto_manage_on_zone_unpinned_zone_empty_deletes",
			autoManage:        true,
			configSDNZone:     "",
			observedZone:      "orphan",
			remainingVnets:    sdkcluster.ListSdnVnetsResponse{},
			wantDeleteZone:    true,
			wantApplyAfterMin: 2,
		},
	}

	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			var deleteZoneCalls []struct{}
			var updateSdnCalls int
			remainingCopy := r.remainingVnets

			clusterSvc := &mockSDNCluster{
				getSdnVnetsFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnVnetsParams) (*sdkcluster.GetSdnVnetsResponse, error) {
					raw, _ := json.Marshal(map[string]any{"vnet": "net01", "zone": r.observedZone})
					out := sdkcluster.GetSdnVnetsResponse(raw)
					return &out, nil
				},
				getSdnZonesFn: func(_ context.Context, zone string, _ *sdkcluster.GetSdnZonesParams) (*sdkcluster.GetSdnZonesResponse, error) {
					raw, _ := json.Marshal(map[string]any{"zone": zone, "type": "vxlan"})
					out := sdkcluster.GetSdnZonesResponse(raw)
					return &out, nil
				},
				listSdnVnetsSubnetsFn: func(_ context.Context, _ string, _ *sdkcluster.ListSdnVnetsSubnetsParams) (*sdkcluster.ListSdnVnetsSubnetsResponse, error) {
					empty := sdkcluster.ListSdnVnetsSubnetsResponse{}
					return &empty, nil
				},
				listSdnVnetsFn: func(_ context.Context, _ *sdkcluster.ListSdnVnetsParams) (*sdkcluster.ListSdnVnetsResponse, error) {
					return &remainingCopy, nil
				},
				deleteSdnVnetsFn: func(_ context.Context, _ string, _ *sdkcluster.DeleteSdnVnetsParams) error {
					return nil
				},
				deleteSdnZonesFn: func(_ context.Context, _ string, _ *sdkcluster.DeleteSdnZonesParams) error {
					deleteZoneCalls = append(deleteZoneCalls, struct{}{})
					return nil
				},
				updateSdnFn: func(_ context.Context, _ *sdkcluster.UpdateSdnParams) (*sdkcluster.UpdateSdnResponse, error) {
					updateSdnCalls++
					return nil, nil
				},
			}

			err := invokeDeleteNetwork(t, testDeleteDeps(clusterSvc, r.autoManage, r.configSDNZone), "net01")
			if err != nil {
				t.Fatalf("unexpected: %v", err)
			}
			gotDeleteZone := len(deleteZoneCalls) > 0
			if gotDeleteZone != r.wantDeleteZone {
				t.Errorf("DeleteSdnZones called=%v, want=%v", gotDeleteZone, r.wantDeleteZone)
			}
			if updateSdnCalls < r.wantApplyAfterMin {
				t.Errorf("UpdateSdn calls=%d, want >= %d", updateSdnCalls, r.wantApplyAfterMin)
			}
		})
	}
}
