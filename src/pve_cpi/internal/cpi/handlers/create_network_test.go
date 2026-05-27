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

// invokeCreateNetwork marshals args and calls the handler.
func invokeCreateNetwork(t *testing.T, deps handlers.Deps, args ...any) (any, error) {
	t.Helper()
	h := handlers.HandleCreateNetwork(deps)
	raw := marshalArgs(args...)
	return h.Handle(context.Background(), raw, jsonrpc.Context{})
}

// -- CN-01: SDN happy path (zone exists, vnet absent, no range) --

func TestHandleCreateNetwork_SDN_HappyPath(t *testing.T) {
	t.Parallel()
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
		t.Errorf("bridge must equal vnet name; got %v", cp["bridge"])
	}
}

// -- CN-02: SDN with subnet --

func TestHandleCreateNetwork_SDN_WithSubnet(t *testing.T) {
	t.Parallel()
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
		// SDN mock defaults panic on unconfigured calls. Opt in
		// to the create-vnet + apply mutations the SDN path performs after a
		// 404 from GetSdnVnets.
		createSdnVnetsFn: func(_ context.Context, _ *sdkcluster.CreateSdnVnetsParams) error {
			return nil
		},
		updateSdnFn: func(_ context.Context, _ *sdkcluster.UpdateSdnParams) (*sdkcluster.UpdateSdnResponse, error) {
			return nil, nil
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
	t.Parallel()
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
		// Opt in to UpdateSdn — the SDN path always commits after a
		// successful vnet probe, even on the idempotent re-create branch.
		updateSdnFn: func(_ context.Context, _ *sdkcluster.UpdateSdnParams) (*sdkcluster.UpdateSdnResponse, error) {
			return nil, nil
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
		// Opt in to vnet create + apply mutations the SDN path runs
		// after auto-managed zone creation.
		createSdnVnetsFn: func(_ context.Context, _ *sdkcluster.CreateSdnVnetsParams) error {
			return nil
		},
		updateSdnFn: func(_ context.Context, _ *sdkcluster.UpdateSdnParams) (*sdkcluster.UpdateSdnResponse, error) {
			return nil, nil
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
		// Opt in to vnet create + apply mutations the SDN path runs.
		createSdnVnetsFn: func(_ context.Context, _ *sdkcluster.CreateSdnVnetsParams) error {
			return nil
		},
		updateSdnFn: func(_ context.Context, _ *sdkcluster.UpdateSdnParams) (*sdkcluster.UpdateSdnResponse, error) {
			return nil, nil
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
	t.Parallel()
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

// -- ensure bridge returned in cloud_properties_out uses vnet name not "vmbr"+vnet --

func TestHandleCreateNetwork_SDN_BridgeEqualsVnet(t *testing.T) {
	t.Parallel()
	clusterSvc := &mockSDNCluster{
		getSdnZonesFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnZonesParams) (*sdkcluster.GetSdnZonesResponse, error) {
			raw := sdkcluster.GetSdnZonesResponse(`{"zone":"z"}`)
			return &raw, nil
		},
		getSdnVnetsFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnVnetsParams) (*sdkcluster.GetSdnVnetsResponse, error) {
			return nil, sdnNotFound()
		},
		// Opt in to vnet create + apply mutations.
		createSdnVnetsFn: func(_ context.Context, _ *sdkcluster.CreateSdnVnetsParams) error {
			return nil
		},
		updateSdnFn: func(_ context.Context, _ *sdkcluster.UpdateSdnParams) (*sdkcluster.UpdateSdnResponse, error) {
			return nil, nil
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
		t.Errorf("invariant: bridge=%v, want net01 (the vnet name)", cp["bridge"])
	}
	if strings.HasPrefix(cp["bridge"].(string), "vmbr") {
		t.Errorf("invariant: bridge must not be vmbr-prefixed: %v", cp["bridge"])
	}
}

// -- CN-vnet-required: vnet name missing on SDN path --

func TestHandleCreateNetwork_SDN_VnetRequired(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	clusterSvc := &mockSDNCluster{
		getSdnZonesFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnZonesParams) (*sdkcluster.GetSdnZonesResponse, error) {
			raw := sdkcluster.GetSdnZonesResponse(`{"zone":"z"}`)
			return &raw, nil
		},
		getSdnVnetsFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnVnetsParams) (*sdkcluster.GetSdnVnetsResponse, error) {
			return nil, sdnNotFound()
		},
		// Opt in to vnet create + apply mutations.
		createSdnVnetsFn: func(_ context.Context, _ *sdkcluster.CreateSdnVnetsParams) error {
			return nil
		},
		updateSdnFn: func(_ context.Context, _ *sdkcluster.UpdateSdnParams) (*sdkcluster.UpdateSdnResponse, error) {
			return nil, nil
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
	t.Parallel()
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
// Verifies the rollback apply contract: vnetCreated gates rollback, not spec.Range.

func TestHandleCreateNetwork_SDN_Rollback_SubnetFails(t *testing.T) {
	t.Parallel()
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
// Verifies the rollback apply contract: subnetCreated=true gates subnet delete.

func TestHandleCreateNetwork_SDN_Rollback_ApplyFails(t *testing.T) {
	t.Parallel()
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
// Verifies rollback is gated on *Created bools, not on spec.Range/vnet values.
// When a concurrent apply failure occurs and the vnet was pre-existing, rollback must
// leave the operator's vnet and any pre-existing subnet untouched.

func TestHandleCreateNetwork_SDN_Rollback_PreexistingVnet_NoRollbackDelete(t *testing.T) {
	t.Parallel()
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

// -- CN-RB-04: auto-create zone + vnet; subnet-create fails → rollback deletes both vnet and zone, applies --
//
// Verifies the createdZone=true rollback branch in createNetworkSDN:
//   - Zone absent, sdn_auto_manage_zone=true → CreateSdnZones called → createdZone=true.
//   - Vnet absent → CreateSdnVnets called → vnetCreated=true.
//   - CreateSdnVnetsSubnets returns a non-conflict error → rollback path.
//   - Rollback: DeleteSdnVnets called (vnetCreated=true).
//   - Rollback: DeleteSdnZones called (createdZone=true).
//   - Rollback: UpdateSdn (applySDN) called at least once.
//   - Handler returns the original subnet-create error.

func TestHandleCreateNetwork_SubnetCreateFails_RollsBackZoneAndVnet(t *testing.T) {
	t.Parallel()
	var deleteVnetCalled bool
	var deleteZoneCalled bool
	var updateSdnCalls int
	subnetErr := &pveerr.APIError{} // non-409, non-404 — triggers rollback

	clusterSvc := &mockSDNCluster{
		getSdnZonesFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnZonesParams) (*sdkcluster.GetSdnZonesResponse, error) {
			// Zone absent → triggers auto-create (sdn_auto_manage_zone=true).
			return nil, sdnNotFound()
		},
		createSdnZonesFn: func(_ context.Context, params *sdkcluster.CreateSdnZonesParams) error {
			if params.Zone != "autozone" {
				t.Errorf("createSdnZones: expected zone=autozone, got %q", params.Zone)
			}
			return nil // zone created → createdZone=true
		},
		getSdnVnetsFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnVnetsParams) (*sdkcluster.GetSdnVnetsResponse, error) {
			return nil, sdnNotFound() // vnet absent → will be created
		},
		createSdnVnetsFn: func(_ context.Context, params *sdkcluster.CreateSdnVnetsParams) error {
			if params.Zone != "autozone" {
				t.Errorf("createSdnVnets: expected zone=autozone, got %q", params.Zone)
			}
			return nil // vnet created → vnetCreated=true
		},
		createSdnVnetsSubnetsFn: func(_ context.Context, _ string, _ *sdkcluster.CreateSdnVnetsSubnetsParams) error {
			return subnetErr // non-conflict error → rollback
		},
		deleteSdnVnetsFn: func(_ context.Context, vnet string, _ *sdkcluster.DeleteSdnVnetsParams) error {
			deleteVnetCalled = true
			if vnet != "myvnet" {
				t.Errorf("deleteSdnVnets: expected vnet=myvnet, got %q", vnet)
			}
			return nil
		},
		deleteSdnZonesFn: func(_ context.Context, zone string, _ *sdkcluster.DeleteSdnZonesParams) error {
			deleteZoneCalled = true
			if zone != "autozone" {
				t.Errorf("deleteSdnZones: expected zone=autozone, got %q", zone)
			}
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
			"zone": "autozone",
			"vnet": "myvnet",
		},
	}
	_, err := invokeCreateNetwork(t, testSDNDeps(clusterSvc, "sdn", "", true), spec)
	if err == nil {
		t.Fatal("expected error from subnet-create failure")
	}
	if !deleteVnetCalled {
		t.Error("rollback: DeleteSdnVnets must be called when vnetCreated=true and subnet-create fails")
	}
	if !deleteZoneCalled {
		t.Error("rollback: DeleteSdnZones must be called when createdZone=true and subnet-create fails")
	}
	if updateSdnCalls < 1 {
		t.Errorf("rollback: UpdateSdn (applySDN) must be called at least once after rollback deletes; called %d times", updateSdnCalls)
	}
	// UpdateSdn must NOT be called before rollback (apply only runs for cleanup).
	// The subnet-create failure occurs before the main happy-path apply, so
	// every UpdateSdn call in this test belongs to the rollback path.
}

// ensure testConfig() satisfies compile-time check that *config.CPIConfig is used
var _ *config.CPIConfig = testConfig()

// -- TestCreateNetwork_RollbackSurvivesParentCancel --
//
// Verifies that when the caller's context is cancelled mid-flow, the
// best-effort rollback path still executes its cleanup I/O — i.e. the
// rollback uses a detached context that survives parent cancellation.
//
// Sequence under test:
//  1. GetSdnZones returns the zone.
//  2. GetSdnVnets returns 404.
//  3. CreateSdnVnets succeeds (vnetCreated=true). At this point the test
//     cancels the parent context, simulating an upstream abort.
//  4. CreateSdnVnetsSubnets returns a non-409 error, triggering rollback.
//  5. The rollback must call DeleteSdnVnets and UpdateSdn (apply) even
//     though the parent context is already cancelled.

func TestCreateNetwork_RollbackSurvivesParentCancel(t *testing.T) {
	t.Parallel()
	parentCtx, cancel := context.WithCancel(context.Background())

	var deleteVnetCtxErr error
	var applyCtxErr error
	var rollbackDeleteCalled, rollbackApplyCalled bool

	clusterSvc := &mockSDNCluster{
		getSdnZonesFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnZonesParams) (*sdkcluster.GetSdnZonesResponse, error) {
			raw := sdkcluster.GetSdnZonesResponse(`{"zone":"z"}`)
			return &raw, nil
		},
		getSdnVnetsFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnVnetsParams) (*sdkcluster.GetSdnVnetsResponse, error) {
			return nil, sdnNotFound()
		},
		createSdnVnetsFn: func(_ context.Context, _ *sdkcluster.CreateSdnVnetsParams) error {
			// Vnet created — now cancel the parent ctx, simulating an abort
			// from the upstream caller right before subnet create runs.
			cancel()
			return nil
		},
		createSdnVnetsSubnetsFn: func(_ context.Context, _ string, _ *sdkcluster.CreateSdnVnetsSubnetsParams) error {
			return &pveerr.APIError{} // triggers rollback path
		},
		deleteSdnVnetsFn: func(ctx context.Context, _ string, _ *sdkcluster.DeleteSdnVnetsParams) error {
			rollbackDeleteCalled = true
			deleteVnetCtxErr = ctx.Err()
			return nil
		},
		updateSdnFn: func(ctx context.Context, _ *sdkcluster.UpdateSdnParams) (*sdkcluster.UpdateSdnResponse, error) {
			rollbackApplyCalled = true
			applyCtxErr = ctx.Err()
			return nil, nil
		},
	}

	spec := map[string]any{
		"type":  "manual",
		"range": "10.0.0.0/24",
		"cloud_properties": map[string]any{
			"zone": "z",
			"vnet": "myvnet",
		},
	}
	deps := testSDNDeps(clusterSvc, "sdn", "", false)
	h := handlers.HandleCreateNetwork(deps)
	raw := marshalArgs(spec)
	_, err := h.Handle(parentCtx, raw, jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected subnet-create error to bubble up")
	}
	if !rollbackDeleteCalled {
		t.Fatal("rollback DeleteSdnVnets must run despite parent cancel")
	}
	if !rollbackApplyCalled {
		t.Fatal("rollback UpdateSdn (apply) must run despite parent cancel")
	}
	if deleteVnetCtxErr != nil {
		t.Errorf("rollback DeleteSdnVnets ctx must NOT carry cancel; got %v", deleteVnetCtxErr)
	}
	if applyCtxErr != nil {
		t.Errorf("rollback UpdateSdn ctx must NOT carry cancel; got %v", applyCtxErr)
	}
}

// -- TestCreateNetwork_Concurrent_IdempotentOnExisting --
//
// Verifies that when GetSdnVnets reports the vnet already exists (e.g. a
// concurrent caller created it first), the handler does NOT call
// CreateSdnVnets again and returns the same vnet identity. The handler must
// still commit via UpdateSdn so any pending zone-config state for the
// observed vnet is applied for the new caller's session.

func TestCreateNetwork_Concurrent_IdempotentOnExisting(t *testing.T) {
	t.Parallel()
	var createVnetCalled bool
	var applyCalled bool

	clusterSvc := &mockSDNCluster{
		getSdnZonesFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnZonesParams) (*sdkcluster.GetSdnZonesResponse, error) {
			raw := sdkcluster.GetSdnZonesResponse(`{"zone":"z"}`)
			return &raw, nil
		},
		getSdnVnetsFn: func(_ context.Context, vnet string, _ *sdkcluster.GetSdnVnetsParams) (*sdkcluster.GetSdnVnetsResponse, error) {
			if vnet != "shared" {
				t.Errorf("get vnet name: got %q, want shared", vnet)
			}
			raw := sdkcluster.GetSdnVnetsResponse(`{"vnet":"shared","zone":"z"}`)
			return &raw, nil
		},
		createSdnVnetsFn: func(_ context.Context, _ *sdkcluster.CreateSdnVnetsParams) error {
			createVnetCalled = true
			return nil
		},
		updateSdnFn: func(_ context.Context, _ *sdkcluster.UpdateSdnParams) (*sdkcluster.UpdateSdnResponse, error) {
			applyCalled = true
			return nil, nil
		},
	}

	spec := map[string]any{
		"type": "manual",
		"cloud_properties": map[string]any{
			"zone": "z",
			"vnet": "shared",
		},
	}
	result, err := invokeCreateNetwork(t, testSDNDeps(clusterSvc, "sdn", "", false), spec)
	if err != nil {
		t.Fatalf("idempotent observe must succeed; got: %v", err)
	}
	if createVnetCalled {
		t.Error("CreateSdnVnets must NOT be called when vnet already exists")
	}
	if !applyCalled {
		t.Error("UpdateSdn must still be called to commit any pending state")
	}
	arr, ok := result.([]any)
	if !ok || len(arr) != 3 {
		t.Fatalf("expected 3-element response, got %T %v", result, result)
	}
	if arr[0] != "shared" {
		t.Errorf("network_cid: got %v, want shared", arr[0])
	}
	cp := arr[2].(map[string]any)
	if cp["vnet"] != "shared" || cp["bridge"] != "shared" {
		t.Errorf("idempotent response must echo vnet/bridge=shared; got %v", cp)
	}
}

// -- TestCreateNetwork_ZoneAlreadyExists_NoError --
//
// Verifies that when sdn_auto_manage_zone is true but the requested zone is
// already present in PVE, the handler treats the GetSdnZones success as
// "do not create" and does NOT mark createdZone. A subsequent rollback must
// therefore NOT delete the operator-owned zone, even if a later step fails.

func TestCreateNetwork_ZoneAlreadyExists_NoError(t *testing.T) {
	t.Parallel()
	var createZoneCalled bool
	var deleteZoneCalled bool
	var deleteVnetCalled bool

	clusterSvc := &mockSDNCluster{
		getSdnZonesFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnZonesParams) (*sdkcluster.GetSdnZonesResponse, error) {
			// Zone is present — no 404.
			raw := sdkcluster.GetSdnZonesResponse(`{"zone":"existingzone","type":"simple"}`)
			return &raw, nil
		},
		createSdnZonesFn: func(_ context.Context, _ *sdkcluster.CreateSdnZonesParams) error {
			createZoneCalled = true
			return nil
		},
		getSdnVnetsFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnVnetsParams) (*sdkcluster.GetSdnVnetsResponse, error) {
			return nil, sdnNotFound()
		},
		createSdnVnetsFn: func(_ context.Context, _ *sdkcluster.CreateSdnVnetsParams) error {
			return nil
		},
		createSdnVnetsSubnetsFn: func(_ context.Context, _ string, _ *sdkcluster.CreateSdnVnetsSubnetsParams) error {
			// Force rollback so we can confirm zone is NOT deleted.
			return &pveerr.APIError{}
		},
		deleteSdnZonesFn: func(_ context.Context, _ string, _ *sdkcluster.DeleteSdnZonesParams) error {
			deleteZoneCalled = true
			return nil
		},
		deleteSdnVnetsFn: func(_ context.Context, _ string, _ *sdkcluster.DeleteSdnVnetsParams) error {
			deleteVnetCalled = true
			return nil
		},
		updateSdnFn: func(_ context.Context, _ *sdkcluster.UpdateSdnParams) (*sdkcluster.UpdateSdnResponse, error) {
			return nil, nil
		},
	}

	spec := map[string]any{
		"type":  "manual",
		"range": "10.0.0.0/24",
		"cloud_properties": map[string]any{
			"zone": "existingzone",
			"vnet": "myvnet",
		},
	}
	_, err := invokeCreateNetwork(t, testSDNDeps(clusterSvc, "sdn", "", true), spec)
	if err == nil {
		t.Fatal("expected subnet-create failure to bubble up")
	}
	if createZoneCalled {
		t.Error("CreateSdnZones must NOT be called when zone already exists")
	}
	if !deleteVnetCalled {
		t.Error("rollback DeleteSdnVnets must run because vnetCreated=true")
	}
	if deleteZoneCalled {
		t.Error("rollback must NOT delete a pre-existing zone (createdZone=false)")
	}
}
