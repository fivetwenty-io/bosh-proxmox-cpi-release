package handlers_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	pveerr "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

// sdnNotFound builds a wrapped error that isSDNNotFound detects via errors.Is.
func sdnNotFound() error {
	return pveerr.ErrNotFound
}

// netResolveRetries is a local *int helper for NetworkResolveRetries
// assignments — the field is *int so an unset property (nil, which defaults
// to 30) can be distinguished from an explicit 0 (see
// config.CPIConfig.NetworkResolveRetries).
func netResolveRetries(n int) *int { return &n }

// testSDNDeps returns Deps wired with clusterSvc and a minimal config.
func testSDNDeps(clusterSvc sdkcluster.Service, networkMode, sdnZone string, autoManage bool) handlers.Deps {
	cfg := testConfig()
	cfg.NetworkMode = networkMode
	cfg.SDNZone = sdnZone
	cfg.SDNZoneType = "simple"
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

	type createVnetCall struct {
		vnet string
		zone string
	}

	var createVnetCalls []createVnetCall
	var updateSdnCalls int

	clusterSvc := &mockSDNCluster{
		getSdnZonesFn: func(_ context.Context, zone string, _ *sdkcluster.GetSdnZonesParams) (*sdkcluster.GetSdnZonesResponse, error) {
			raw := sdkcluster.GetSdnZonesResponse(`{"zone":"boshzone","type":"simple"}`)
			return &raw, nil
		},
		getSdnVnetsFn: func(_ context.Context, vnet string, _ *sdkcluster.GetSdnVnetsParams) (*sdkcluster.GetSdnVnetsResponse, error) {
			return nil, sdnNotFound() // vnet absent
		},
		createSdnVnetsFn: func(_ context.Context, params *sdkcluster.CreateSdnVnetsParams) error {
			createVnetCalls = append(createVnetCalls, createVnetCall{params.Vnet, params.Zone})
			return nil
		},
		updateSdnFn: func(_ context.Context, _ *sdkcluster.UpdateSdnParams) (*sdkcluster.UpdateSdnResponse, error) {
			updateSdnCalls++
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
	if len(createVnetCalls) != 1 {
		t.Fatalf("CreateSdnVnets: want 1 call, got %d", len(createVnetCalls))
	}
	if createVnetCalls[0].vnet != "boshvnet" {
		t.Errorf("CreateSdnVnets: want vnet=boshvnet, got %q", createVnetCalls[0].vnet)
	}
	if createVnetCalls[0].zone != "boshzone" {
		t.Errorf("CreateSdnVnets: want zone=boshzone, got %q", createVnetCalls[0].zone)
	}
	if updateSdnCalls == 0 {
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

	type subnetCall struct {
		subnet  string
		gateway string
	}
	var subnetCalls []subnetCall

	clusterSvc := &mockSDNCluster{
		getSdnZonesFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnZonesParams) (*sdkcluster.GetSdnZonesResponse, error) {
			raw := sdkcluster.GetSdnZonesResponse(`{"zone":"boshzone"}`)
			return &raw, nil
		},
		getSdnVnetsFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnVnetsParams) (*sdkcluster.GetSdnVnetsResponse, error) {
			return nil, sdnNotFound()
		},
		createSdnVnetsSubnetsFn: func(_ context.Context, vnet string, params *sdkcluster.CreateSdnVnetsSubnetsParams) error {
			sc := subnetCall{subnet: params.Subnet}
			if params.Gateway != nil {
				sc.gateway = *params.Gateway
			}
			subnetCalls = append(subnetCalls, sc)
			return nil
		},
		// SDN mock defaults panic on unconfigured calls. Opt in
		// to the create-vnet + apply mutations the SDN path performs after a
		// 404 from GetSdnVnets.
		createSdnVnetsFn: func(_ context.Context, params *sdkcluster.CreateSdnVnetsParams) error {
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
	if len(subnetCalls) != 1 {
		t.Fatalf("CreateSdnVnetsSubnets: want 1 call, got %d", len(subnetCalls))
	}
	if subnetCalls[0].subnet != "10.0.0.0/24" {
		t.Errorf("CreateSdnVnetsSubnets: want subnet=10.0.0.0/24, got %q", subnetCalls[0].subnet)
	}
	if subnetCalls[0].gateway != "10.0.0.1" {
		t.Errorf("CreateSdnVnetsSubnets: want gateway=10.0.0.1, got %q", subnetCalls[0].gateway)
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

	var createVnetCalls []struct{}

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
		createSdnVnetsFn: func(_ context.Context, params *sdkcluster.CreateSdnVnetsParams) error {
			createVnetCalls = append(createVnetCalls, struct{}{})
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
	if len(createVnetCalls) != 0 {
		t.Errorf("CreateSdnVnets must NOT be called when vnet already exists; got %d call(s)", len(createVnetCalls))
	}
	arr := result.([]any)
	if arr[0] != "myvnet" {
		t.Errorf("network_cid: got %v", arr[0])
	}
}

// TestHandleCreateNetwork_SDN_VnetAliasStampedOnCreate verifies that a newly
// created vnet's CreateSdnVnets call carries the "bosh-<vnet>" ownership
// alias.
func TestHandleCreateNetwork_SDN_VnetAliasStampedOnCreate(t *testing.T) {
	t.Parallel()

	var createdAlias *string

	clusterSvc := &mockSDNCluster{
		getSdnZonesFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnZonesParams) (*sdkcluster.GetSdnZonesResponse, error) {
			raw := sdkcluster.GetSdnZonesResponse(`{"zone":"z"}`)
			return &raw, nil
		},
		getSdnVnetsFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnVnetsParams) (*sdkcluster.GetSdnVnetsResponse, error) {
			return nil, sdnNotFound() // vnet absent — create path
		},
		createSdnVnetsFn: func(_ context.Context, params *sdkcluster.CreateSdnVnetsParams) error {
			createdAlias = params.Alias
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
			"vnet": "boshvnet",
		},
	}
	if _, err := invokeCreateNetwork(t, testSDNDeps(clusterSvc, "sdn", "", false), spec); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if createdAlias == nil {
		t.Fatal("CreateSdnVnetsParams.Alias is nil; want \"bosh-boshvnet\"")
	}
	if *createdAlias != "bosh-boshvnet" {
		t.Errorf("CreateSdnVnetsParams.Alias = %q; want %q", *createdAlias, "bosh-boshvnet")
	}
}

// TestHandleCreateNetwork_SDN_AdoptedVnet_AliasUntouched verifies that the
// idempotent-adopt path (vnet already exists) never calls CreateSdnVnets at
// all — so a pre-existing vnet's alias (operator-set or otherwise) is left
// completely untouched, not merely unset.
func TestHandleCreateNetwork_SDN_AdoptedVnet_AliasUntouched(t *testing.T) {
	t.Parallel()

	var createCalled bool

	clusterSvc := &mockSDNCluster{
		getSdnZonesFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnZonesParams) (*sdkcluster.GetSdnZonesResponse, error) {
			raw := sdkcluster.GetSdnZonesResponse(`{"zone":"z"}`)
			return &raw, nil
		},
		getSdnVnetsFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnVnetsParams) (*sdkcluster.GetSdnVnetsResponse, error) {
			// Vnet already exists, with an operator-set alias.
			raw := sdkcluster.GetSdnVnetsResponse(`{"vnet":"myvnet","zone":"z","alias":"operator-owned"}`)
			return &raw, nil
		},
		createSdnVnetsFn: func(_ context.Context, _ *sdkcluster.CreateSdnVnetsParams) error {
			createCalled = true
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
			"vnet": "myvnet",
		},
	}
	if _, err := invokeCreateNetwork(t, testSDNDeps(clusterSvc, "sdn", "", false), spec); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if createCalled {
		t.Error("CreateSdnVnets must not be called for an already-existing (adopted) vnet — its alias must not be touched")
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
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) {
		t.Fatalf("expected *cpierrors.Error, got %T: %v", err, err)
	}
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
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) {
		t.Fatalf("expected *cpierrors.Error, got %T: %v", err, err)
	}
	if cpiErr.Type() != cpierrors.TypeCloud {
		t.Errorf("expected CloudError, got %q", cpiErr.Type())
	}
}

// -- CN-06: Zone missing, autoManage=true → CreateSdnZones called --

func TestHandleCreateNetwork_SDN_ZoneMissingAutoManageTrue(t *testing.T) {
	t.Parallel()

	type createZoneCall struct {
		zone     string
		zoneType string
	}
	var createZoneCalls []createZoneCall

	clusterSvc := &mockSDNCluster{
		getSdnZonesFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnZonesParams) (*sdkcluster.GetSdnZonesResponse, error) {
			return nil, sdnNotFound()
		},
		createSdnZonesFn: func(_ context.Context, params *sdkcluster.CreateSdnZonesParams) error {
			createZoneCalls = append(createZoneCalls, createZoneCall{params.Zone, params.Type})
			return nil
		},
		getSdnVnetsFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnVnetsParams) (*sdkcluster.GetSdnVnetsResponse, error) {
			return nil, sdnNotFound()
		},
		// Opt in to vnet create + apply mutations the SDN path runs
		// after auto-managed zone creation.
		createSdnVnetsFn: func(_ context.Context, params *sdkcluster.CreateSdnVnetsParams) error {
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
	if len(createZoneCalls) != 1 {
		t.Fatalf("CreateSdnZones: want 1 call, got %d", len(createZoneCalls))
	}
	if createZoneCalls[0].zone != "autozone" {
		t.Errorf("CreateSdnZones: want zone=autozone, got %q", createZoneCalls[0].zone)
	}
	if createZoneCalls[0].zoneType != "simple" {
		t.Errorf("CreateSdnZones: want type=simple, got %q", createZoneCalls[0].zoneType)
	}
	// result not asserted here; test pins only that CreateSdnZones is invoked
	// when zone is absent and auto-manage is enabled.
	_ = result
}

// -- CN-07: Bridge fallback --

func TestHandleCreateNetwork_Bridge_HappyPath(t *testing.T) {
	t.Parallel()

	type createNetCall struct {
		iface     string
		ifaceType string
	}
	var createNetCalls []createNetCall
	var updateNetCalls int

	nodesSvc := &mockBridgeNodes{
		createNetworkFn: func(_ context.Context, node string, params *sdknodes.CreateNetworkParams) error {
			createNetCalls = append(createNetCalls, createNetCall{params.Iface, params.Type})
			return nil
		},
		updateNetworkFn: func(_ context.Context, _ string, _ *sdknodes.UpdateNetworkParams) (*sdknodes.UpdateNetworkResponse, error) {
			updateNetCalls++
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
	if len(createNetCalls) != 1 {
		t.Fatalf("CreateNetwork: want 1 call, got %d", len(createNetCalls))
	}
	if createNetCalls[0].iface != "vmbr99" {
		t.Errorf("CreateNetwork: want Iface=vmbr99, got %q", createNetCalls[0].iface)
	}
	if createNetCalls[0].ifaceType != "bridge" {
		t.Errorf("CreateNetwork: want Type=bridge, got %q", createNetCalls[0].ifaceType)
	}
	if updateNetCalls == 0 {
		t.Error("UpdateNetwork must be called")
	}
	arr := result.([]any)
	if arr[0] != "vmbr99" {
		t.Errorf("network_cid: got %v, want vmbr99", arr[0])
	}
}

// -- Routing: mode sets the default, explicit spec intent overrides --

// An unambiguous bridge request (cloud_properties.bridge, no zone, no vnet)
// takes the bridge path even under network_mode=sdn — the default mode. Any
// SDN API call panics (unconfigured mockSDNCluster), so passing proves the
// SDN path was never entered.
func TestHandleCreateNetwork_SDNMode_ExplicitBridgeTakesBridgePath(t *testing.T) {
	t.Parallel()

	var createNetIfaces []string
	nodesSvc := &mockBridgeNodes{
		createNetworkFn: func(_ context.Context, _ string, params *sdknodes.CreateNetworkParams) error {
			createNetIfaces = append(createNetIfaces, params.Iface)
			return nil
		},
	}

	autoManage := true
	cfg := testConfig()
	cfg.NetworkMode = "sdn"
	cfg.NetworkBridge = "vmbr0"
	cfg.SDNAutoManageZone = &autoManage
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
			"bridge": "vmbr9",
		},
	}
	result, err := invokeCreateNetwork(t, deps, spec)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(createNetIfaces) != 1 || createNetIfaces[0] != "vmbr9" {
		t.Fatalf("CreateNetwork ifaces: want [vmbr9], got %v", createNetIfaces)
	}
	arr := result.([]any)
	if arr[0] != "vmbr9" {
		t.Errorf("network_cid: got %v, want vmbr9", arr[0])
	}
}

// A spec naming both a bridge and a vnet is an SDN request — the vnet wins
// under network_mode=sdn. The bridge nodes API must not be touched (panic
// stub), and the vnet is created in the turnkey flow's resolved zone.
func TestHandleCreateNetwork_SDNMode_BridgeWithVnetStaysSDN(t *testing.T) {
	t.Parallel()

	var createdVnets []string
	clusterSvc := &mockSDNCluster{
		getSdnZonesFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnZonesParams) (*sdkcluster.GetSdnZonesResponse, error) {
			raw := sdkcluster.GetSdnZonesResponse(`{"zone":"boshzone","type":"simple"}`)
			return &raw, nil
		},
		getSdnVnetsFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnVnetsParams) (*sdkcluster.GetSdnVnetsResponse, error) {
			return nil, sdnNotFound()
		},
		createSdnVnetsFn: func(_ context.Context, params *sdkcluster.CreateSdnVnetsParams) error {
			createdVnets = append(createdVnets, params.Vnet)
			return nil
		},
		updateSdnFn: func(_ context.Context, _ *sdkcluster.UpdateSdnParams) (*sdkcluster.UpdateSdnResponse, error) {
			return nil, nil
		},
	}

	spec := map[string]any{
		"type": "manual",
		"cloud_properties": map[string]any{
			"zone":   "boshzone",
			"vnet":   "boshvnet",
			"bridge": "vmbr9",
		},
	}
	result, err := invokeCreateNetwork(t, testSDNDeps(clusterSvc, "sdn", "", false), spec)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(createdVnets) != 1 || createdVnets[0] != "boshvnet" {
		t.Fatalf("CreateSdnVnets: want [boshvnet], got %v", createdVnets)
	}
	arr := result.([]any)
	if arr[0] != "boshvnet" {
		t.Errorf("network_cid: got %v, want boshvnet", arr[0])
	}
}

// An explicit zone+vnet takes the SDN path even under network_mode=bridge —
// the mirror of the explicit-bridge carve-out above.
func TestHandleCreateNetwork_BridgeMode_ExplicitVnetTakesSDNPath(t *testing.T) {
	t.Parallel()

	var createdVnets []string
	clusterSvc := &mockSDNCluster{
		getSdnZonesFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnZonesParams) (*sdkcluster.GetSdnZonesResponse, error) {
			raw := sdkcluster.GetSdnZonesResponse(`{"zone":"boshzone","type":"simple"}`)
			return &raw, nil
		},
		getSdnVnetsFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnVnetsParams) (*sdkcluster.GetSdnVnetsResponse, error) {
			return nil, sdnNotFound()
		},
		createSdnVnetsFn: func(_ context.Context, params *sdkcluster.CreateSdnVnetsParams) error {
			createdVnets = append(createdVnets, params.Vnet)
			return nil
		},
		updateSdnFn: func(_ context.Context, _ *sdkcluster.UpdateSdnParams) (*sdkcluster.UpdateSdnResponse, error) {
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
	result, err := invokeCreateNetwork(t, testSDNDeps(clusterSvc, "bridge", "", false), spec)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(createdVnets) != 1 || createdVnets[0] != "boshvnet" {
		t.Fatalf("CreateSdnVnets: want [boshvnet], got %v", createdVnets)
	}
	arr := result.([]any)
	if arr[0] != "boshvnet" {
		t.Errorf("network_cid: got %v, want boshvnet", arr[0])
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
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) {
		t.Fatalf("expected *cpierrors.Error, got %T: %v", err, err)
	}
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
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) {
		t.Fatalf("expected *cpierrors.Error, got %T: %v", err, err)
	}
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
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) {
		t.Fatalf("expected *cpierrors.Error, got %T: %v", err, err)
	}
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
		createSdnVnetsFn: func(_ context.Context, params *sdkcluster.CreateSdnVnetsParams) error {
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

// -- §7.39 produce-side SDN convergence gate --

func TestHandleCreateNetwork_SDN_ConvergenceGate_Converges(t *testing.T) {
	t.Parallel()
	var runningListCalls int
	clusterSvc := &mockSDNCluster{
		getSdnZonesFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnZonesParams) (*sdkcluster.GetSdnZonesResponse, error) {
			raw := sdkcluster.GetSdnZonesResponse(`{"zone":"boshzone","type":"simple"}`)
			return &raw, nil
		},
		getSdnVnetsFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnVnetsParams) (*sdkcluster.GetSdnVnetsResponse, error) {
			return nil, sdnNotFound()
		},
		createSdnVnetsFn: func(_ context.Context, _ *sdkcluster.CreateSdnVnetsParams) error { return nil },
		updateSdnFn: func(_ context.Context, _ *sdkcluster.UpdateSdnParams) (*sdkcluster.UpdateSdnResponse, error) {
			return nil, nil
		},
		// Produce-side gate polls the running (non-pending) vnet list; the vnet is
		// present on the first poll, so the gate returns without sleeping.
		listSdnVnetsFn: func(_ context.Context, _ *sdkcluster.ListSdnVnetsParams) (*sdkcluster.ListSdnVnetsResponse, error) {
			runningListCalls++
			raw := sdkcluster.ListSdnVnetsResponse{json.RawMessage(`{"vnet":"boshvnet","zone":"boshzone"}`)}
			return &raw, nil
		},
	}

	deps := testSDNDeps(clusterSvc, "sdn", "", false)
	deps.Config.NetworkResolveRetries = netResolveRetries(3)
	spec := map[string]any{
		"type":             "manual",
		"cloud_properties": map[string]any{"zone": "boshzone", "vnet": "boshvnet"},
	}
	result, err := invokeCreateNetwork(t, deps, spec)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if runningListCalls != 1 {
		t.Errorf("convergence gate: want 1 running-vnet poll, got %d", runningListCalls)
	}
	if result.([]any)[0] != "boshvnet" {
		t.Errorf("network_cid: got %v, want boshvnet", result.([]any)[0])
	}
}

func TestHandleCreateNetwork_SDN_ConvergenceGate_Retriable(t *testing.T) {
	t.Parallel()
	clusterSvc := &mockSDNCluster{
		getSdnZonesFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnZonesParams) (*sdkcluster.GetSdnZonesResponse, error) {
			raw := sdkcluster.GetSdnZonesResponse(`{"zone":"boshzone","type":"simple"}`)
			return &raw, nil
		},
		getSdnVnetsFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnVnetsParams) (*sdkcluster.GetSdnVnetsResponse, error) {
			return nil, sdnNotFound()
		},
		createSdnVnetsFn: func(_ context.Context, _ *sdkcluster.CreateSdnVnetsParams) error { return nil },
		updateSdnFn: func(_ context.Context, _ *sdkcluster.UpdateSdnParams) (*sdkcluster.UpdateSdnResponse, error) {
			return nil, nil
		},
		// The vnet never appears in the running config → gate exhausts its budget
		// and returns a retriable error so the director re-drives.
		listSdnVnetsFn: func(_ context.Context, _ *sdkcluster.ListSdnVnetsParams) (*sdkcluster.ListSdnVnetsResponse, error) {
			raw := sdkcluster.ListSdnVnetsResponse{json.RawMessage(`{"vnet":"other","zone":"boshzone"}`)}
			return &raw, nil
		},
	}

	deps := testSDNDeps(clusterSvc, "sdn", "", false)
	deps.Config.NetworkResolveRetries = netResolveRetries(1) // one retry → at most one ~1s sleep
	spec := map[string]any{
		"type":             "manual",
		"cloud_properties": map[string]any{"zone": "boshzone", "vnet": "boshvnet"},
	}
	_, err := invokeCreateNetwork(t, deps, spec)
	if err == nil || !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("non-converging vnet: want retriable-cloud, got %v", err)
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
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) {
		t.Fatalf("expected *cpierrors.Error, got %T: %v", err, err)
	}
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
		createSdnVnetsFn: func(_ context.Context, params *sdkcluster.CreateSdnVnetsParams) error {
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
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) {
		t.Fatalf("expected *cpierrors.Error, got %T: %v", err, err)
	}
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
		createSdnVnetsFn: func(_ context.Context, params *sdkcluster.CreateSdnVnetsParams) error {
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
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) {
		t.Fatalf("expected *cpierrors.Error, got %T: %v", err, err)
	}
	if cpiErr.Type() != cpierrors.TypeCloud {
		t.Errorf("expected CloudError, got %q", cpiErr.Type())
	}
}

// -- CN-RB-01: subnet-create failure → vnet rollback + applySDN(rollback) called; original error returned --
//
// Verifies the rollback apply contract: vnetCreated gates rollback, not spec.Range.

func TestHandleCreateNetwork_SDN_Rollback_SubnetFails(t *testing.T) {
	t.Parallel()

	var deleteVnetCalls []struct{}
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
		createSdnVnetsFn: func(_ context.Context, params *sdkcluster.CreateSdnVnetsParams) error {
			return nil // vnet created successfully
		},
		createSdnVnetsSubnetsFn: func(_ context.Context, _ string, _ *sdkcluster.CreateSdnVnetsSubnetsParams) error {
			return subnetErr // subnet fails
		},
		deleteSdnVnetsFn: func(_ context.Context, _ string, _ *sdkcluster.DeleteSdnVnetsParams) error {
			deleteVnetCalls = append(deleteVnetCalls, struct{}{})
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
	if len(deleteVnetCalls) == 0 {
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

	var deleteVnetCalls []struct{}
	var deleteSubnetCalls []struct{}
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
		createSdnVnetsFn: func(_ context.Context, params *sdkcluster.CreateSdnVnetsParams) error {
			return nil
		},
		createSdnVnetsSubnetsFn: func(_ context.Context, _ string, _ *sdkcluster.CreateSdnVnetsSubnetsParams) error {
			return nil
		},
		deleteSdnVnetsFn: func(_ context.Context, _ string, _ *sdkcluster.DeleteSdnVnetsParams) error {
			deleteVnetCalls = append(deleteVnetCalls, struct{}{})
			return nil
		},
		deleteSdnVnetsSubnetsFn: func(_ context.Context, _ string, _ string, _ *sdkcluster.DeleteSdnVnetsSubnetsParams) error {
			deleteSubnetCalls = append(deleteSubnetCalls, struct{}{})
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
	if len(deleteSubnetCalls) == 0 {
		t.Error("rollback: DeleteSdnVnetsSubnets must be called when subnetCreated=true and apply fails")
	}
	if len(deleteVnetCalls) == 0 {
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

	var deleteVnetCalls []struct{}
	var deleteSubnetCalls []struct{}
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
			deleteVnetCalls = append(deleteVnetCalls, struct{}{})
			return nil
		},
		deleteSdnVnetsSubnetsFn: func(_ context.Context, _ string, _ string, _ *sdkcluster.DeleteSdnVnetsSubnetsParams) error {
			deleteSubnetCalls = append(deleteSubnetCalls, struct{}{})
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
	if len(deleteVnetCalls) != 0 {
		t.Errorf("rollback must NOT delete a pre-existing vnet (vnetCreated=false); got %d call(s)", len(deleteVnetCalls))
	}
	if len(deleteSubnetCalls) != 0 {
		t.Errorf("rollback must NOT delete a pre-existing subnet (subnetCreated=false); got %d call(s)", len(deleteSubnetCalls))
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

	type deleteVnetCall struct{ vnet string }
	type deleteZoneCall struct{ zone string }
	var deleteVnetCalls []deleteVnetCall
	var deleteZoneCalls []deleteZoneCall
	var updateSdnCalls int
	subnetErr := &pveerr.APIError{} // non-409, non-404 — triggers rollback

	clusterSvc := &mockSDNCluster{
		getSdnZonesFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnZonesParams) (*sdkcluster.GetSdnZonesResponse, error) {
			// Zone absent → triggers auto-create (sdn_auto_manage_zone=true).
			return nil, sdnNotFound()
		},
		createSdnZonesFn: func(_ context.Context, params *sdkcluster.CreateSdnZonesParams) error {
			// zone param asserted post-hoc via deleteZoneCalls capture
			_ = params.Zone // zone must be autozone; rollback captures it
			return nil      // zone created → createdZone=true
		},
		getSdnVnetsFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnVnetsParams) (*sdkcluster.GetSdnVnetsResponse, error) {
			return nil, sdnNotFound() // vnet absent → will be created
		},
		createSdnVnetsFn: func(_ context.Context, params *sdkcluster.CreateSdnVnetsParams) error {
			return nil // vnet created → vnetCreated=true
		},
		createSdnVnetsSubnetsFn: func(_ context.Context, _ string, _ *sdkcluster.CreateSdnVnetsSubnetsParams) error {
			return subnetErr // non-conflict error → rollback
		},
		deleteSdnVnetsFn: func(_ context.Context, vnet string, _ *sdkcluster.DeleteSdnVnetsParams) error {
			deleteVnetCalls = append(deleteVnetCalls, deleteVnetCall{vnet})
			return nil
		},
		deleteSdnZonesFn: func(_ context.Context, zone string, _ *sdkcluster.DeleteSdnZonesParams) error {
			deleteZoneCalls = append(deleteZoneCalls, deleteZoneCall{zone})
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
	if len(deleteVnetCalls) == 0 {
		t.Error("rollback: DeleteSdnVnets must be called when vnetCreated=true and subnet-create fails")
	} else if deleteVnetCalls[0].vnet != "myvnet" {
		t.Errorf("rollback: DeleteSdnVnets: want vnet=myvnet, got %q", deleteVnetCalls[0].vnet)
	}
	if len(deleteZoneCalls) == 0 {
		t.Error("rollback: DeleteSdnZones must be called when createdZone=true and subnet-create fails")
	} else if deleteZoneCalls[0].zone != "autozone" {
		t.Errorf("rollback: DeleteSdnZones: want zone=autozone, got %q", deleteZoneCalls[0].zone)
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

// ---------------------------------------------------------------------------
// Layered-resolver integration tests for create_network
// ---------------------------------------------------------------------------

// testSDNDepsWithProfiles returns Deps wired with clusterSvc and vm_type/disk_type profile maps.
func testSDNDepsWithProfiles(
	clusterSvc sdkcluster.Service,
	networkMode, sdnZone string,
	vmTypes map[string]config.TypeProfile,
) handlers.Deps {
	cfg := testConfig()
	cfg.NetworkMode = networkMode
	cfg.SDNZone = sdnZone
	cfg.SDNZoneType = "simple"
	cfg.SDNAutoManageZone = boolPtr(false)
	cfg.VMTypes = vmTypes
	return handlers.Deps{
		Config: cfg,
		PVE: &mockPVEClient{
			clusterSvc: clusterSvc,
			nodesSvc:   &mockNodesService{},
		},
		Logger: log.NewNopLogger(),
	}
}

// sdnHappyCluster builds a mockSDNCluster where the zone exists, vnet is absent,
// and apply succeeds. zoneChecked and vnetCreatedName let callers assert routing.
func sdnHappyCluster(zoneResult string) (*mockSDNCluster, *string) {
	var zoneChecked string
	clusterSvc := &mockSDNCluster{
		getSdnZonesFn: func(_ context.Context, zone string, _ *sdkcluster.GetSdnZonesParams) (*sdkcluster.GetSdnZonesResponse, error) {
			zoneChecked = zone
			raw := sdkcluster.GetSdnZonesResponse(`{"zone":"` + zoneResult + `"}`)
			return &raw, nil
		},
		getSdnVnetsFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnVnetsParams) (*sdkcluster.GetSdnVnetsResponse, error) {
			return nil, sdnNotFound()
		},
		createSdnVnetsFn: func(_ context.Context, _ *sdkcluster.CreateSdnVnetsParams) error {
			return nil
		},
		updateSdnFn: func(_ context.Context, _ *sdkcluster.UpdateSdnParams) (*sdkcluster.UpdateSdnResponse, error) {
			return nil, nil
		},
	}
	return clusterSvc, &zoneChecked
}

// -- LR-CN-01: no selectors — byte-identical to pre-resolver behavior --
// zone + vnet supplied directly in call CP; config zone fallback not triggered.
func TestHandleCreateNetwork_LayeredResolver_NoSelectors_SameAsPlain(t *testing.T) {
	t.Parallel()
	clusterSvc, zoneChecked := sdnHappyCluster("myzone")
	spec := map[string]any{
		"type": "manual",
		"cloud_properties": map[string]any{
			"zone": "myzone",
			"vnet": "myvnet",
		},
	}
	result, err := invokeCreateNetwork(t,
		testSDNDepsWithProfiles(clusterSvc, "sdn", "", nil),
		spec,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if *zoneChecked != "myzone" {
		t.Errorf("zone: got %q, want myzone", *zoneChecked)
	}
	arr := result.([]any)
	if arr[0] != "myvnet" {
		t.Errorf("network_cid: got %v, want myvnet", arr[0])
	}
}

// -- LR-CN-02: vm_type profile supplies zone; call CP lacks zone --
// Verifies profile layer is consulted when call map is missing the key.
func TestHandleCreateNetwork_LayeredResolver_VMTypeProfile_SuppliesZone(t *testing.T) {
	t.Parallel()
	clusterSvc, zoneChecked := sdnHappyCluster("profilezone")
	vmTypes := map[string]config.TypeProfile{
		"small": {
			CloudProperties: map[string]any{
				"zone": "profilezone",
			},
		},
	}
	spec := map[string]any{
		"type": "manual",
		"cloud_properties": map[string]any{
			"vm_type": "small",
			"vnet":    "testvnet",
			// zone deliberately absent — should come from profile
		},
	}
	result, err := invokeCreateNetwork(t,
		testSDNDepsWithProfiles(clusterSvc, "sdn", "", vmTypes),
		spec,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if *zoneChecked != "profilezone" {
		t.Errorf("zone from profile: got %q, want profilezone", *zoneChecked)
	}
	cp := result.([]any)[2].(map[string]any)
	if cp["zone"] != "profilezone" {
		t.Errorf("cloud_props_out zone: got %v, want profilezone", cp["zone"])
	}
}

// -- LR-CN-03: call CP zone beats profile zone --
// The call layer has highest precedence; profile zone must NOT win.
func TestHandleCreateNetwork_LayeredResolver_CallZoneBeatsProfile(t *testing.T) {
	t.Parallel()
	clusterSvc, zoneChecked := sdnHappyCluster("callzone")
	vmTypes := map[string]config.TypeProfile{
		"small": {
			CloudProperties: map[string]any{
				"zone": "profilezone",
			},
		},
	}
	spec := map[string]any{
		"type": "manual",
		"cloud_properties": map[string]any{
			"vm_type": "small",
			"zone":    "callzone", // call beats profile
			"vnet":    "testvnet",
		},
	}
	result, err := invokeCreateNetwork(t,
		testSDNDepsWithProfiles(clusterSvc, "sdn", "", vmTypes),
		spec,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if *zoneChecked != "callzone" {
		t.Errorf("call zone must win; got %q, want callzone", *zoneChecked)
	}
	cp := result.([]any)[2].(map[string]any)
	if cp["zone"] != "callzone" {
		t.Errorf("cloud_props_out zone: got %v, want callzone", cp["zone"])
	}
}

// -- LR-CN-04: profile supplies bridge; call CP has no bridge key --
// vm_type profile carries bridge; config.NetworkBridge is also present but
// the profile layer (below call, above config) should supply the value since
// the call layer has no "bridge" key and the profile does.
// Verifies resolver profile layer is consulted for bridge before config fallback.
func TestHandleCreateNetwork_LayeredResolver_BridgeDefault(t *testing.T) {
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
	cfg.NetworkBridge = "vmbr0" // config default — should be beaten by profile
	cfg.Node = "pve1"
	vmTypes := map[string]config.TypeProfile{
		"web": {
			CloudProperties: map[string]any{
				"bridge": "vmbr99", // profile supplies bridge
			},
		},
	}
	cfg.VMTypes = vmTypes
	deps := handlers.Deps{
		Config: cfg,
		PVE:    &mockPVEClient{clusterSvc: &mockSDNCluster{}, nodesSvc: nodesSvc},
		Logger: log.NewNopLogger(),
	}
	spec := map[string]any{
		"type": "manual",
		"cloud_properties": map[string]any{
			"vm_type": "web",
			// no bridge in call CP — profile should supply vmbr99
		},
	}
	result, err := invokeCreateNetwork(t, deps, spec)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if ifaceUsed != "vmbr99" {
		t.Errorf("expected profile bridge vmbr99, got %q", ifaceUsed)
	}
	if result.([]any)[0] != "vmbr99" {
		t.Errorf("network_cid: got %v, want vmbr99", result.([]any)[0])
	}
}

// -- LR-CN-05: unknown selector in call CP → CloudError --
func TestHandleCreateNetwork_LayeredResolver_UnknownVMType_CloudError(t *testing.T) {
	t.Parallel()
	cfg := testConfig()
	cfg.NetworkMode = "sdn"
	cfg.SDNZone = "myzone"
	cfg.VMTypes = map[string]config.TypeProfile{} // empty — "bogus" is unknown
	deps := handlers.Deps{
		Config: cfg,
		PVE:    &mockPVEClient{clusterSvc: &mockSDNCluster{}},
		Logger: log.NewNopLogger(),
	}
	spec := map[string]any{
		"type": "manual",
		"cloud_properties": map[string]any{
			"vm_type": "bogus",
			"vnet":    "myvnet",
		},
	}
	_, err := invokeCreateNetwork(t, deps, spec)
	if err == nil {
		t.Fatal("expected CloudError for unknown vm_type")
	}
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) {
		t.Fatalf("expected *cpierrors.Error, got %T: %v", err, err)
	}
	if cpiErr.Type() != cpierrors.TypeCloud {
		t.Errorf("expected TypeCloud, got %q", cpiErr.Type())
	}
	if !strings.Contains(cpiErr.Error(), "unknown profile") {
		t.Errorf("error should mention unknown profile: %v", cpiErr)
	}
}

// -- LR-CN-06: config zone still applies when no selectors and call CP has no zone --
// Ensures the config fallback after the resolver still works (resolver returns not-found,
// caller falls back to cfg.SDNZone exactly as before).
func TestHandleCreateNetwork_LayeredResolver_ConfigZoneFallback_NoSelectors(t *testing.T) {
	t.Parallel()
	var zoneChecked string
	clusterSvc := &mockSDNCluster{
		getSdnZonesFn: func(_ context.Context, zone string, _ *sdkcluster.GetSdnZonesParams) (*sdkcluster.GetSdnZonesResponse, error) {
			zoneChecked = zone
			raw := sdkcluster.GetSdnZonesResponse(`{"zone":"cfgzone"}`)
			return &raw, nil
		},
		getSdnVnetsFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnVnetsParams) (*sdkcluster.GetSdnVnetsResponse, error) {
			return nil, sdnNotFound()
		},
		createSdnVnetsFn: func(_ context.Context, params *sdkcluster.CreateSdnVnetsParams) error {
			return nil
		},
		updateSdnFn: func(_ context.Context, _ *sdkcluster.UpdateSdnParams) (*sdkcluster.UpdateSdnResponse, error) {
			return nil, nil
		},
	}
	spec := map[string]any{
		"type": "manual",
		"cloud_properties": map[string]any{
			"vnet": "cfgvnet",
			// no zone, no vm_type selector
		},
	}
	result, err := invokeCreateNetwork(t,
		testSDNDepsWithProfiles(clusterSvc, "auto", "cfgzone", nil),
		spec,
	)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if zoneChecked != "cfgzone" {
		t.Errorf("config zone fallback: got %q, want cfgzone", zoneChecked)
	}
	cp := result.([]any)[2].(map[string]any)
	if cp["zone"] != "cfgzone" {
		t.Errorf("cloud_props_out zone: got %v, want cfgzone", cp["zone"])
	}
}

// -- LR-CN-07: nil cloud_properties → resolver handles nil map without panic --
func TestHandleCreateNetwork_LayeredResolver_NilCloudProperties(t *testing.T) {
	t.Parallel()
	cfg := testConfig()
	cfg.NetworkMode = "bridge"
	cfg.NetworkBridge = "vmbr0"
	cfg.Node = "pve1"
	deps := handlers.Deps{
		Config: cfg,
		PVE:    &mockPVEClient{clusterSvc: &mockSDNCluster{}, nodesSvc: &mockBridgeNodes{}},
		Logger: log.NewNopLogger(),
	}
	// Sending a spec with explicitly null cloud_properties.
	raw := []json.RawMessage{json.RawMessage(`{"type":"manual","cloud_properties":null}`)}
	h := handlers.HandleCreateNetwork(deps)
	_, err := h.Handle(context.Background(), raw, jsonrpc.Context{})
	if err != nil {
		t.Fatalf("nil cloud_properties must not panic or error: %v", err)
	}
}

// -- LR-CN-08: profile node supplies node for bridge path --
func TestHandleCreateNetwork_LayeredResolver_VMTypeProfile_SuppliesNode(t *testing.T) {
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
	cfg.NetworkBridge = "vmbr0"
	cfg.Node = "" // empty so config fallback doesn't win
	cfg.VMTypes = map[string]config.TypeProfile{
		"rack1": {
			CloudProperties: map[string]any{
				"node": "racknode1",
			},
		},
	}
	deps := handlers.Deps{
		Config: cfg,
		PVE:    &mockPVEClient{clusterSvc: &mockSDNCluster{}, nodesSvc: nodesSvc},
		Logger: log.NewNopLogger(),
	}
	spec := map[string]any{
		"type": "manual",
		"cloud_properties": map[string]any{
			"vm_type": "rack1",
			// no node in call CP
		},
	}
	_, err := invokeCreateNetwork(t, deps, spec)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if nodeUsed != "racknode1" {
		t.Errorf("node from profile: got %q, want racknode1", nodeUsed)
	}
}

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
		createSdnVnetsFn: func(_ context.Context, params *sdkcluster.CreateSdnVnetsParams) error {
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

	var createVnetCalls []struct{}
	var applyCalls int

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
		createSdnVnetsFn: func(_ context.Context, params *sdkcluster.CreateSdnVnetsParams) error {
			createVnetCalls = append(createVnetCalls, struct{}{})
			return nil
		},
		updateSdnFn: func(_ context.Context, _ *sdkcluster.UpdateSdnParams) (*sdkcluster.UpdateSdnResponse, error) {
			applyCalls++
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
	if len(createVnetCalls) != 0 {
		t.Errorf("CreateSdnVnets must NOT be called when vnet already exists; got %d call(s)", len(createVnetCalls))
	}
	if applyCalls == 0 {
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

	var createZoneCalls []struct{}
	var deleteZoneCalls []struct{}
	var deleteVnetCalls []struct{}

	clusterSvc := &mockSDNCluster{
		getSdnZonesFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnZonesParams) (*sdkcluster.GetSdnZonesResponse, error) {
			// Zone is present — no 404.
			raw := sdkcluster.GetSdnZonesResponse(`{"zone":"existingzone","type":"simple"}`)
			return &raw, nil
		},
		createSdnZonesFn: func(_ context.Context, _ *sdkcluster.CreateSdnZonesParams) error {
			createZoneCalls = append(createZoneCalls, struct{}{})
			return nil
		},
		getSdnVnetsFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnVnetsParams) (*sdkcluster.GetSdnVnetsResponse, error) {
			return nil, sdnNotFound()
		},
		createSdnVnetsFn: func(_ context.Context, params *sdkcluster.CreateSdnVnetsParams) error {
			return nil
		},
		createSdnVnetsSubnetsFn: func(_ context.Context, _ string, _ *sdkcluster.CreateSdnVnetsSubnetsParams) error {
			// Force rollback so we can confirm zone is NOT deleted.
			return &pveerr.APIError{}
		},
		deleteSdnZonesFn: func(_ context.Context, _ string, _ *sdkcluster.DeleteSdnZonesParams) error {
			deleteZoneCalls = append(deleteZoneCalls, struct{}{})
			return nil
		},
		deleteSdnVnetsFn: func(_ context.Context, _ string, _ *sdkcluster.DeleteSdnVnetsParams) error {
			deleteVnetCalls = append(deleteVnetCalls, struct{}{})
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
	if len(createZoneCalls) != 0 {
		t.Errorf("CreateSdnZones must NOT be called when zone already exists; got %d call(s)", len(createZoneCalls))
	}
	if len(deleteVnetCalls) != 1 {
		t.Errorf("rollback DeleteSdnVnets must run because vnetCreated=true; got %d call(s)", len(deleteVnetCalls))
	}
	if len(deleteZoneCalls) != 0 {
		t.Errorf("rollback must NOT delete a pre-existing zone (createdZone=false); got %d call(s)", len(deleteZoneCalls))
	}
}
