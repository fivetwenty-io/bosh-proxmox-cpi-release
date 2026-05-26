package handlers_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi/handlers"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	sdkcluster "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cluster"
	sdknodes "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/nodes"
	pveerr "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/errors"
)

// sdnNotFound builds a wrapped error that isSDNNotFound detects via errors.Is.
func sdnNotFound() error {
	return pveerr.ErrNotFound
}

// testSDNDeps returns Deps wired with clusterSvc and a minimal config.
func testSDNDeps(clusterSvc sdkcluster.Service, networkMode, sdnZone string, autoManage bool) handlers.Deps {
	cfg := testConfig()
	cfg.NetworkMode = networkMode
	cfg.SDNZone = sdnZone
	cfg.SDNZoneType = "simple"
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

// testBridgeDeps returns Deps wired with nodesSvc for bridge fallback.
func testBridgeDeps(nodesSvc sdknodes.Service) handlers.Deps {
	cfg := testConfig()
	cfg.NetworkMode = "bridge"
	cfg.NetworkBridge = "vmbr0"
	return handlers.Deps{
		Config: cfg,
		PVE: &mockPVEClient{
			clusterSvc: &mockSDNCluster{},
			nodesSvc:   nodesSvc,
		},
		Logger: log.NewNopLogger(),
	}
}

// invokeCreateNetwork marshals args and calls the handler.
func invokeCreateNetwork(t *testing.T, deps handlers.Deps, args ...any) (any, error) {
	t.Helper()
	h := handlers.HandleCreateNetwork(deps)
	raw := marshalArgs(args...)
	return h.Handle(context.Background(), raw, jsonrpc.Context{})
}

// -- CN-01: SDN happy path (zone exists, vnet absent, no range) --

func TestHandleCreateNetwork_SDN_HappyPath(t *testing.T) {
	var createVnetCalled bool
	var updateSdnCalled bool

	clusterSvc := &mockSDNCluster{
		getSdnZonesFn: func(_ context.Context, zone string, _ *sdkcluster.GetSdnZonesParams) (*sdkcluster.GetSdnZonesResponse, error) {
			raw := sdkcluster.GetSdnZonesResponse(`{"zone":"boshzone","type":"simple"}`)
			return &raw, nil
		},
		getSdnVnetsFn: func(_ context.Context, vnet string, _ *sdkcluster.GetSdnVnetsParams) (*sdkcluster.GetSdnVnetsResponse, error) {
			return nil, sdnNotFound() // vnet absent
		},
		createSdnVnetsFn: func(_ context.Context, params *sdkcluster.CreateSdnVnetsParams) error {
			createVnetCalled = true
			if params.Vnet != "boshvnet" {
				t.Errorf("expected vnet=boshvnet, got %q", params.Vnet)
			}
			if params.Zone != "boshzone" {
				t.Errorf("expected zone=boshzone, got %q", params.Zone)
			}
			return nil
		},
		updateSdnFn: func(_ context.Context, _ *sdkcluster.UpdateSdnParams) (*sdkcluster.UpdateSdnResponse, error) {
			updateSdnCalled = true
			return nil, nil
		},
	}

	spec := map[string]any{
		"type": "manual",
		"cloud_properties": map[string]any{
			"zone": "boshzone",
			"vnet": "boshvnet",
		},
	}
	result, err := invokeCreateNetwork(t, testSDNDeps(clusterSvc, "sdn", "", false), spec)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !createVnetCalled {
		t.Error("CreateSdnVnets must be called")
	}
	if !updateSdnCalled {
		t.Error("UpdateSdn must be called")
	}

	arr, ok := result.([]any)
	if !ok || len(arr) != 3 {
		t.Fatalf("expected 3-element array, got %T %v", result, result)
	}
	if arr[0] != "boshvnet" {
		t.Errorf("network_cid: got %v, want boshvnet", arr[0])
	}
	cp, ok := arr[2].(map[string]any)
	if !ok {
		t.Fatalf("cloud_properties_out is not map: %T", arr[2])
	}
	if cp["bridge"] != "boshvnet" {
		t.Errorf("bridge must equal vnet name (D-06); got %v", cp["bridge"])
	}
}

// -- CN-02: SDN with subnet --

func TestHandleCreateNetwork_SDN_WithSubnet(t *testing.T) {
	var subnetCalled bool
	clusterSvc := &mockSDNCluster{
		getSdnZonesFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnZonesParams) (*sdkcluster.GetSdnZonesResponse, error) {
			raw := sdkcluster.GetSdnZonesResponse(`{"zone":"boshzone"}`)
			return &raw, nil
		},
		getSdnVnetsFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnVnetsParams) (*sdkcluster.GetSdnVnetsResponse, error) {
			return nil, sdnNotFound()
		},
		createSdnVnetsSubnetsFn: func(_ context.Context, vnet string, params *sdkcluster.CreateSdnVnetsSubnetsParams) error {
			subnetCalled = true
			if params.Subnet != "10.0.0.0/24" {
				t.Errorf("subnet: got %q, want 10.0.0.0/24", params.Subnet)
			}
			if params.Gateway == nil || *params.Gateway != "10.0.0.1" {
				t.Errorf("gateway: got %v, want 10.0.0.1", params.Gateway)
			}
			return nil
		},
	}

	spec := map[string]any{
		"type":    "manual",
		"range":   "10.0.0.0/24",
		"gateway": "10.0.0.1",
		"cloud_properties": map[string]any{
			"zone": "boshzone",
			"vnet": "boshvnet",
		},
	}
	result, err := invokeCreateNetwork(t, testSDNDeps(clusterSvc, "sdn", "", false), spec)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !subnetCalled {
		t.Error("CreateSdnVnetsSubnets must be called when range is present")
	}
	addr, ok := result.([]any)[1].(map[string]any)
	if !ok {
		t.Fatalf("addr_props not a map")
	}
	if addr["range"] != "10.0.0.0/24" {
		t.Errorf("range echoed: got %v", addr["range"])
	}
}

// -- CN-03: SDN idempotent re-create (vnet already exists) --

func TestHandleCreateNetwork_SDN_IdempotentVnetExists(t *testing.T) {
	var createVnetCalled bool
	clusterSvc := &mockSDNCluster{
		getSdnZonesFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnZonesParams) (*sdkcluster.GetSdnZonesResponse, error) {
			raw := sdkcluster.GetSdnZonesResponse(`{"zone":"z"}`)
			return &raw, nil
		},
		getSdnVnetsFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnVnetsParams) (*sdkcluster.GetSdnVnetsResponse, error) {
			// Vnet already exists.
			raw := sdkcluster.GetSdnVnetsResponse(`{"vnet":"myvnet","zone":"z"}`)
			return &raw, nil
		},
		createSdnVnetsFn: func(_ context.Context, _ *sdkcluster.CreateSdnVnetsParams) error {
			createVnetCalled = true
			return nil
		},
	}

	spec := map[string]any{
		"type": "manual",
		"cloud_properties": map[string]any{
			"zone": "z",
			"vnet": "myvnet",
		},
	}
	result, err := invokeCreateNetwork(t, testSDNDeps(clusterSvc, "sdn", "", false), spec)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if createVnetCalled {
		t.Error("CreateSdnVnets must NOT be called when vnet already exists")
	}
	arr := result.([]any)
	if arr[0] != "myvnet" {
		t.Errorf("network_cid: got %v", arr[0])
	}
}

// -- CN-04: Bad vnet name (>8 chars) --

func TestHandleCreateNetwork_SDN_BadVnetName(t *testing.T) {
	spec := map[string]any{
		"type": "manual",
		"cloud_properties": map[string]any{
			"zone": "myzone",
			"vnet": "toolongname", // 11 chars
		},
	}
	_, err := invokeCreateNetwork(t, testSDNDeps(&mockSDNCluster{}, "sdn", "", false), spec)
	if err == nil {
		t.Fatal("expected error for long vnet name")
	}
	cpiErr := err.(*cpierrors.Error)
	if cpiErr.Type() != cpierrors.TypeCloud {
		t.Errorf("expected CloudError, got %q", cpiErr.Type())
	}
	if !strings.Contains(cpiErr.Error(), "vnet name") {
		t.Errorf("error should mention vnet name: %v", cpiErr)
	}
}

// -- CN-05: Zone missing, autoManage=false --

func TestHandleCreateNetwork_SDN_ZoneMissingAutoManageFalse(t *testing.T) {
	clusterSvc := &mockSDNCluster{
		getSdnZonesFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnZonesParams) (*sdkcluster.GetSdnZonesResponse, error) {
			return nil, sdnNotFound()
		},
	}
	spec := map[string]any{
		"type": "manual",
		"cloud_properties": map[string]any{
			"zone": "missing-zone",
			"vnet": "myvnet",
		},
	}
	_, err := invokeCreateNetwork(t, testSDNDeps(clusterSvc, "sdn", "", false), spec)
	if err == nil {
		t.Fatal("expected error")
	}
	cpiErr := err.(*cpierrors.Error)
	if cpiErr.Type() != cpierrors.TypeCloud {
		t.Errorf("expected CloudError, got %q", cpiErr.Type())
	}
}

// -- CN-06: Zone missing, autoManage=true → CreateSdnZones called --

func TestHandleCreateNetwork_SDN_ZoneMissingAutoManageTrue(t *testing.T) {
	var createZoneCalled bool
	clusterSvc := &mockSDNCluster{
		getSdnZonesFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnZonesParams) (*sdkcluster.GetSdnZonesResponse, error) {
			return nil, sdnNotFound()
		},
		createSdnZonesFn: func(_ context.Context, params *sdkcluster.CreateSdnZonesParams) error {
			createZoneCalled = true
			if params.Zone != "autozone" {
				t.Errorf("zone: got %q, want autozone", params.Zone)
			}
			if params.Type != "simple" {
				t.Errorf("type: got %q, want simple", params.Type)
			}
			return nil
		},
		getSdnVnetsFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnVnetsParams) (*sdkcluster.GetSdnVnetsResponse, error) {
			return nil, sdnNotFound()
		},
	}

	spec := map[string]any{
		"type": "manual",
		"cloud_properties": map[string]any{
			"zone": "autozone",
			"vnet": "myvnet",
		},
	}
	result, err := invokeCreateNetwork(t, testSDNDeps(clusterSvc, "sdn", "", true), spec)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !createZoneCalled {
		t.Error("CreateSdnZones must be called when zone is absent and auto-manage=true")
	}
	_ = result
}

// -- CN-07: Bridge fallback --

func TestHandleCreateNetwork_Bridge_HappyPath(t *testing.T) {
	var createNetworkCalled bool
	var updateNetworkCalled bool

	nodesSvc := &mockBridgeNodes{
		createNetworkFn: func(_ context.Context, node string, params *sdknodes.CreateNetworkParams) error {
			createNetworkCalled = true
			if params.Iface != "vmbr99" {
				t.Errorf("Iface: got %q, want vmbr99", params.Iface)
			}
			if params.Type != "bridge" {
				t.Errorf("Type: got %q, want bridge", params.Type)
			}
			return nil
		},
		updateNetworkFn: func(_ context.Context, _ string, _ *sdknodes.UpdateNetworkParams) (*sdknodes.UpdateNetworkResponse, error) {
			updateNetworkCalled = true
			return nil, nil
		},
	}

	cfg := testConfig()
	cfg.NetworkMode = "bridge"
	cfg.NetworkBridge = "vmbr0"
	deps := handlers.Deps{
		Config: cfg,
		PVE: &mockPVEClient{
			clusterSvc: &mockSDNCluster{},
			nodesSvc:   nodesSvc,
		},
		Logger: log.NewNopLogger(),
	}

	spec := map[string]any{
		"type": "manual",
		"cloud_properties": map[string]any{
			"bridge": "vmbr99",
		},
	}
	result, err := invokeCreateNetwork(t, deps, spec)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !createNetworkCalled {
		t.Error("CreateNetwork must be called")
	}
	if !updateNetworkCalled {
		t.Error("UpdateNetwork must be called")
	}
	arr := result.([]any)
	if arr[0] != "vmbr99" {
		t.Errorf("network_cid: got %v, want vmbr99", arr[0])
	}
}

// -- CN-08: Missing args --

func TestHandleCreateNetwork_MissingArg(t *testing.T) {
	h := handlers.HandleCreateNetwork(handlers.Deps{Config: testConfig(), Logger: log.NewNopLogger(), PVE: &mockPVEClient{clusterSvc: &mockSDNCluster{}}})
	_, err := h.Handle(context.Background(), []json.RawMessage{}, jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error")
	}
	cpiErr := err.(*cpierrors.Error)
	if cpiErr.Type() != cpierrors.TypeCloud {
		t.Errorf("expected CloudError, got %q", cpiErr.Type())
	}
}

// -- CN-09: No routing info --

func TestHandleCreateNetwork_NoRoutingInfo(t *testing.T) {
	cfg := testConfig()
	cfg.NetworkMode = "auto"
	cfg.NetworkBridge = ""
	cfg.SDNZone = ""
	deps := handlers.Deps{
		Config: cfg,
		PVE:    &mockPVEClient{clusterSvc: &mockSDNCluster{}},
		Logger: log.NewNopLogger(),
	}

	spec := map[string]any{
		"type":             "manual",
		"cloud_properties": map[string]any{}, // no zone, no vnet, no bridge
	}
	_, err := invokeCreateNetwork(t, deps, spec)
	if err == nil {
		t.Fatal("expected error when no routing info")
	}
	cpiErr := err.(*cpierrors.Error)
	if cpiErr.Type() != cpierrors.TypeCloud {
		t.Errorf("expected CloudError, got %q", cpiErr.Type())
	}
}

// -- CN-10: Invalid JSON spec --

func TestHandleCreateNetwork_InvalidJSON(t *testing.T) {
	cfg := testConfig()
	deps := handlers.Deps{Config: cfg, Logger: log.NewNopLogger(), PVE: &mockPVEClient{clusterSvc: &mockSDNCluster{}}}
	h := handlers.HandleCreateNetwork(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{json.RawMessage(`not-json`)}, jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error")
	}
	cpiErr := err.(*cpierrors.Error)
	if cpiErr.Type() != cpierrors.TypeCloud {
		t.Errorf("expected CloudError, got %q", cpiErr.Type())
	}
}

// -- CN-11: config.SDNZone used as zone fallback --

func TestHandleCreateNetwork_SDN_ConfigZoneFallback(t *testing.T) {
	var zoneChecked string
	clusterSvc := &mockSDNCluster{
		getSdnZonesFn: func(_ context.Context, zone string, _ *sdkcluster.GetSdnZonesParams) (*sdkcluster.GetSdnZonesResponse, error) {
			zoneChecked = zone
			raw := sdkcluster.GetSdnZonesResponse(`{"zone":"configzone"}`)
			return &raw, nil
		},
		getSdnVnetsFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnVnetsParams) (*sdkcluster.GetSdnVnetsResponse, error) {
			return nil, sdnNotFound()
		},
	}

	// zone comes from config, not cloud_properties
	spec := map[string]any{
		"type": "manual",
		"cloud_properties": map[string]any{
			"vnet": "myvnet",
			// no zone key
		},
	}
	result, err := invokeCreateNetwork(t, testSDNDeps(clusterSvc, "auto", "configzone", false), spec)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if zoneChecked != "configzone" {
		t.Errorf("expected config SDN zone to be used; got %q", zoneChecked)
	}
	cp := result.([]any)[2].(map[string]any)
	if cp["zone"] != "configzone" {
		t.Errorf("zone in cloud_properties_out: got %v", cp["zone"])
	}
}

// -- CN-12: vnet name with invalid chars --

func TestHandleCreateNetwork_SDN_VnetNameInvalidChars(t *testing.T) {
	spec := map[string]any{
		"type": "manual",
		"cloud_properties": map[string]any{
			"zone": "z",
			"vnet": "my-vnet", // hyphen not allowed
		},
	}
	_, err := invokeCreateNetwork(t, testSDNDeps(&mockSDNCluster{}, "sdn", "", false), spec)
	if err == nil {
		t.Fatal("expected error for vnet name with hyphen")
	}
	cpiErr := err.(*cpierrors.Error)
	if cpiErr.Type() != cpierrors.TypeCloud {
		t.Errorf("expected CloudError, got %q", cpiErr.Type())
	}
}

// -- ensure bridge returned in cloud_properties_out uses vnet name not "vmbr"+vnet (D-06) --

func TestHandleCreateNetwork_SDN_BridgeEqualsVnet(t *testing.T) {
	clusterSvc := &mockSDNCluster{
		getSdnZonesFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnZonesParams) (*sdkcluster.GetSdnZonesResponse, error) {
			raw := sdkcluster.GetSdnZonesResponse(`{"zone":"z"}`)
			return &raw, nil
		},
		getSdnVnetsFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnVnetsParams) (*sdkcluster.GetSdnVnetsResponse, error) {
			return nil, sdnNotFound()
		},
	}
	spec := map[string]any{
		"type": "manual",
		"cloud_properties": map[string]any{
			"zone": "z",
			"vnet": "net01",
		},
	}
	result, err := invokeCreateNetwork(t, testSDNDeps(clusterSvc, "sdn", "", false), spec)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	cp := result.([]any)[2].(map[string]any)
	if cp["bridge"] != "net01" {
		t.Errorf("D-06 violation: bridge=%v, want net01 (the vnet name)", cp["bridge"])
	}
	if strings.HasPrefix(cp["bridge"].(string), "vmbr") {
		t.Errorf("D-06 violation: bridge must not be vmbr-prefixed: %v", cp["bridge"])
	}
}

// -- CN-vnet-required: vnet name missing on SDN path --

func TestHandleCreateNetwork_SDN_VnetRequired(t *testing.T) {
	cfg := testConfig()
	cfg.NetworkMode = "sdn"
	cfg.SDNZone = "myzone"
	deps := handlers.Deps{
		Config: cfg,
		PVE:    &mockPVEClient{clusterSvc: &mockSDNCluster{}},
		Logger: log.NewNopLogger(),
	}
	spec := map[string]any{
		"type":             "manual",
		"cloud_properties": map[string]any{"zone": "myzone"}, // no vnet
	}
	_, err := invokeCreateNetwork(t, deps, spec)
	if err == nil {
		t.Fatal("expected error when vnet is missing on SDN path")
	}
	cpiErr := err.(*cpierrors.Error)
	if cpiErr.Type() != cpierrors.TypeCloud {
		t.Errorf("expected CloudError, got %q", cpiErr.Type())
	}
}

// -- bridge falls back to config.NetworkBridge when cloud_properties.bridge absent --

func TestHandleCreateNetwork_Bridge_FallsBackToConfigBridge(t *testing.T) {
	var ifaceUsed string
	nodesSvc := &mockBridgeNodes{
		createNetworkFn: func(_ context.Context, _ string, params *sdknodes.CreateNetworkParams) error {
			ifaceUsed = params.Iface
			return nil
		},
	}
	cfg := testConfig()
	cfg.NetworkMode = "bridge"
	cfg.NetworkBridge = "vmbr0" // from config
	deps := handlers.Deps{
		Config: cfg,
		PVE:    &mockPVEClient{clusterSvc: &mockSDNCluster{}, nodesSvc: nodesSvc},
		Logger: log.NewNopLogger(),
	}
	spec := map[string]any{
		"type":             "manual",
		"cloud_properties": map[string]any{}, // no bridge key
	}
	result, err := invokeCreateNetwork(t, deps, spec)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if ifaceUsed != "vmbr0" {
		t.Errorf("expected config.NetworkBridge=vmbr0 used, got %q", ifaceUsed)
	}
	if result.([]any)[0] != "vmbr0" {
		t.Errorf("network_cid: got %v", result.([]any)[0])
	}
}

// -- ensure config.Node is used as bridge node when cloud_properties.node absent --

func TestHandleCreateNetwork_Bridge_UsesConfigNode(t *testing.T) {
	var nodeUsed string
	nodesSvc := &mockBridgeNodes{
		createNetworkFn: func(_ context.Context, node string, _ *sdknodes.CreateNetworkParams) error {
			nodeUsed = node
			return nil
		},
	}
	cfg := testConfig()
	cfg.NetworkMode = "bridge"
	cfg.Node = "mynode"
	cfg.NetworkBridge = "vmbr0"
	deps := handlers.Deps{
		Config: cfg,
		PVE:    &mockPVEClient{clusterSvc: &mockSDNCluster{}, nodesSvc: nodesSvc},
		Logger: log.NewNopLogger(),
	}
	spec := map[string]any{"type": "manual", "cloud_properties": map[string]any{}}
	_, err := invokeCreateNetwork(t, deps, spec)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if nodeUsed != "mynode" {
		t.Errorf("expected config.Node=mynode, got %q", nodeUsed)
	}
}

// -- bridge returns node in cloud_properties_out --

func TestHandleCreateNetwork_Bridge_CloudPropsOut(t *testing.T) {
	nodesSvc := &mockBridgeNodes{}
	cfg := testConfig()
	cfg.NetworkMode = "bridge"
	cfg.Node = "pve1"
	cfg.NetworkBridge = "vmbr5"
	deps := handlers.Deps{
		Config: cfg,
		PVE:    &mockPVEClient{clusterSvc: &mockSDNCluster{}, nodesSvc: nodesSvc},
		Logger: log.NewNopLogger(),
	}
	spec := map[string]any{"type": "manual", "cloud_properties": map[string]any{}}
	result, err := invokeCreateNetwork(t, deps, spec)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	cp := result.([]any)[2].(map[string]any)
	if cp["bridge"] != "vmbr5" {
		t.Errorf("bridge: got %v, want vmbr5", cp["bridge"])
	}
	if cp["node"] != "pve1" {
		t.Errorf("node: got %v, want pve1", cp["node"])
	}
}

// -- addr_properties reserved field is always empty []string{} --

func TestHandleCreateNetwork_SDN_ReservedIsEmptySlice(t *testing.T) {
	clusterSvc := &mockSDNCluster{
		getSdnZonesFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnZonesParams) (*sdkcluster.GetSdnZonesResponse, error) {
			raw := sdkcluster.GetSdnZonesResponse(`{"zone":"z"}`)
			return &raw, nil
		},
		getSdnVnetsFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnVnetsParams) (*sdkcluster.GetSdnVnetsResponse, error) {
			return nil, sdnNotFound()
		},
	}
	spec := map[string]any{"type": "manual", "cloud_properties": map[string]any{"zone": "z", "vnet": "net01"}}
	result, err := invokeCreateNetwork(t, testSDNDeps(clusterSvc, "sdn", "", false), spec)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	addr := result.([]any)[1].(map[string]any)
	reserved, ok := addr["reserved"]
	if !ok {
		t.Error("addr_properties must contain reserved key")
	}
	if reserved == nil {
		t.Error("reserved must not be nil")
	}
}

// -- bridge CreateNetwork error surfaces as CloudError --

func TestHandleCreateNetwork_Bridge_CreateError(t *testing.T) {
	nodesSvc := &mockBridgeNodes{
		createNetworkFn: func(_ context.Context, _ string, _ *sdknodes.CreateNetworkParams) error {
			return &pveerr.APIError{} // generic API error
		},
	}
	cfg := testConfig()
	cfg.NetworkMode = "bridge"
	cfg.NetworkBridge = "vmbr0"
	deps := handlers.Deps{
		Config: cfg,
		PVE:    &mockPVEClient{clusterSvc: &mockSDNCluster{}, nodesSvc: nodesSvc},
		Logger: log.NewNopLogger(),
	}
	spec := map[string]any{"type": "manual", "cloud_properties": map[string]any{}}
	_, err := invokeCreateNetwork(t, deps, spec)
	if err == nil {
		t.Fatal("expected error")
	}
	cpiErr := err.(*cpierrors.Error)
	if cpiErr.Type() != cpierrors.TypeCloud {
		t.Errorf("expected CloudError, got %q", cpiErr.Type())
	}
}

// -- CN-RB-01: subnet-create failure → vnet rollback + applySDN(rollback) called; original error returned --
//
// Verifies F-1 (rollback apply) and F-7 (vnetCreated gates rollback, not spec.Range).

func TestHandleCreateNetwork_SDN_Rollback_SubnetFails(t *testing.T) {
	var deleteVnetCalled bool
	var updateSdnCalls int
	subnetErr := &pveerr.APIError{} // non-409, non-404 error

	clusterSvc := &mockSDNCluster{
		getSdnZonesFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnZonesParams) (*sdkcluster.GetSdnZonesResponse, error) {
			raw := sdkcluster.GetSdnZonesResponse(`{"zone":"z"}`)
			return &raw, nil
		},
		getSdnVnetsFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnVnetsParams) (*sdkcluster.GetSdnVnetsResponse, error) {
			return nil, sdnNotFound() // vnet absent → will be created
		},
		createSdnVnetsFn: func(_ context.Context, _ *sdkcluster.CreateSdnVnetsParams) error {
			return nil // vnet created successfully
		},
		createSdnVnetsSubnetsFn: func(_ context.Context, _ string, _ *sdkcluster.CreateSdnVnetsSubnetsParams) error {
			return subnetErr // subnet fails
		},
		deleteSdnVnetsFn: func(_ context.Context, _ string, _ *sdkcluster.DeleteSdnVnetsParams) error {
			deleteVnetCalled = true
			return nil
		},
		updateSdnFn: func(_ context.Context, _ *sdkcluster.UpdateSdnParams) (*sdkcluster.UpdateSdnResponse, error) {
			updateSdnCalls++
			return nil, nil
		},
	}

	spec := map[string]any{
		"type":    "manual",
		"range":   "10.0.0.0/24",
		"gateway": "10.0.0.1",
		"cloud_properties": map[string]any{
			"zone": "z",
			"vnet": "myvnet",
		},
	}
	_, err := invokeCreateNetwork(t, testSDNDeps(clusterSvc, "sdn", "", false), spec)
	if err == nil {
		t.Fatal("expected error from subnet-create failure")
	}
	if !deleteVnetCalled {
		t.Error("rollback: DeleteSdnVnets must be called when vnetCreated=true and subnet-create fails")
	}
	if updateSdnCalls < 1 {
		t.Errorf("rollback: UpdateSdn (applySDN) must be called at least once after rollback deletes; called %d times", updateSdnCalls)
	}
}

// -- CN-RB-02: apply failure after vnet+subnet created → rollback deletes both; applySDN(rollback) called --
//
// Verifies F-1 (rollback apply) and F-7 (subnetCreated=true gates subnet delete).

func TestHandleCreateNetwork_SDN_Rollback_ApplyFails(t *testing.T) {
	var deleteVnetCalled bool
	var deleteSubnetCalled bool
	applyCallCount := 0
	firstApply := true

	clusterSvc := &mockSDNCluster{
		getSdnZonesFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnZonesParams) (*sdkcluster.GetSdnZonesResponse, error) {
			raw := sdkcluster.GetSdnZonesResponse(`{"zone":"z"}`)
			return &raw, nil
		},
		getSdnVnetsFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnVnetsParams) (*sdkcluster.GetSdnVnetsResponse, error) {
			return nil, sdnNotFound()
		},
		createSdnVnetsFn: func(_ context.Context, _ *sdkcluster.CreateSdnVnetsParams) error {
			return nil
		},
		createSdnVnetsSubnetsFn: func(_ context.Context, _ string, _ *sdkcluster.CreateSdnVnetsSubnetsParams) error {
			return nil
		},
		deleteSdnVnetsFn: func(_ context.Context, _ string, _ *sdkcluster.DeleteSdnVnetsParams) error {
			deleteVnetCalled = true
			return nil
		},
		deleteSdnVnetsSubnetsFn: func(_ context.Context, _ string, _ string, _ *sdkcluster.DeleteSdnVnetsSubnetsParams) error {
			deleteSubnetCalled = true
			return nil
		},
		updateSdnFn: func(_ context.Context, _ *sdkcluster.UpdateSdnParams) (*sdkcluster.UpdateSdnResponse, error) {
			applyCallCount++
			if firstApply {
				firstApply = false
				return nil, &pveerr.APIError{} // main apply fails
			}
			return nil, nil // rollback apply succeeds
		},
	}

	spec := map[string]any{
		"type":    "manual",
		"range":   "10.0.0.0/24",
		"gateway": "10.0.0.1",
		"cloud_properties": map[string]any{
			"zone": "z",
			"vnet": "myvnet",
		},
	}
	_, err := invokeCreateNetwork(t, testSDNDeps(clusterSvc, "sdn", "", false), spec)
	if err == nil {
		t.Fatal("expected error when main apply fails")
	}
	if !deleteSubnetCalled {
		t.Error("rollback: DeleteSdnVnetsSubnets must be called when subnetCreated=true and apply fails")
	}
	if !deleteVnetCalled {
		t.Error("rollback: DeleteSdnVnets must be called when vnetCreated=true and apply fails")
	}
	if applyCallCount < 2 {
		t.Errorf("rollback: UpdateSdn must be called at least twice (main attempt + rollback apply); called %d times", applyCallCount)
	}
}

// -- CN-RB-03: vnet pre-exists (GetSdnVnets ok → vnetCreated=false); later failure must NOT delete vnet/subnet --
//
// Verifies F-7: rollback is gated on *Created bools, not on spec.Range/vnet values.
// When a concurrent apply failure occurs and the vnet was pre-existing, rollback must
// leave the operator's vnet and any pre-existing subnet untouched.

func TestHandleCreateNetwork_SDN_Rollback_PreexistingVnet_NoRollbackDelete(t *testing.T) {
	var deleteVnetCalled bool
	var deleteSubnetCalled bool
	applyFail := true

	clusterSvc := &mockSDNCluster{
		getSdnZonesFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnZonesParams) (*sdkcluster.GetSdnZonesResponse, error) {
			raw := sdkcluster.GetSdnZonesResponse(`{"zone":"z"}`)
			return &raw, nil
		},
		getSdnVnetsFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnVnetsParams) (*sdkcluster.GetSdnVnetsResponse, error) {
			// Vnet already exists → vnetCreated stays false.
			raw := sdkcluster.GetSdnVnetsResponse(`{"vnet":"myvnet","zone":"z"}`)
			return &raw, nil
		},
		createSdnVnetsSubnetsFn: func(_ context.Context, _ string, _ *sdkcluster.CreateSdnVnetsSubnetsParams) error {
			// Subnet pre-exists → 409 → subnetCreated stays false.
			return pveerr.ErrConflict
		},
		deleteSdnVnetsFn: func(_ context.Context, _ string, _ *sdkcluster.DeleteSdnVnetsParams) error {
			deleteVnetCalled = true
			return nil
		},
		deleteSdnVnetsSubnetsFn: func(_ context.Context, _ string, _ string, _ *sdkcluster.DeleteSdnVnetsSubnetsParams) error {
			deleteSubnetCalled = true
			return nil
		},
		updateSdnFn: func(_ context.Context, _ *sdkcluster.UpdateSdnParams) (*sdkcluster.UpdateSdnResponse, error) {
			if applyFail {
				applyFail = false
				return nil, &pveerr.APIError{} // main apply fails
			}
			return nil, nil
		},
	}

	spec := map[string]any{
		"type":    "manual",
		"range":   "10.0.0.0/24",
		"gateway": "10.0.0.1",
		"cloud_properties": map[string]any{
			"zone": "z",
			"vnet": "myvnet",
		},
	}
	_, err := invokeCreateNetwork(t, testSDNDeps(clusterSvc, "sdn", "", false), spec)
	if err == nil {
		t.Fatal("expected error when main apply fails")
	}
	if deleteVnetCalled {
		t.Error("rollback must NOT delete a pre-existing vnet (vnetCreated=false)")
	}
	if deleteSubnetCalled {
		t.Error("rollback must NOT delete a pre-existing subnet (subnetCreated=false)")
	}
}

// ensure testConfig() satisfies compile-time check that *config.CPIConfig is used
var _ *config.CPIConfig = testConfig()
