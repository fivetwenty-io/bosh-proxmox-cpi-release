// create_network_vlan_test.go — vnet-per-VLAN networks: turnkey vlan zone
// creation with the underlay bridge, 802.1Q tag handling, the vlan-safe
// auto-allocation band, and idempotent vnet adoption.
package handlers_test

import (
	"context"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
)

// vlanTestDeps returns Deps for the vlan path: network_mode=sdn, zone type
// vlan, no configured zone, auto-manage left nil (turnkey default true).
// mutate customises the config before wiring.
func vlanTestDeps(clusterSvc sdkcluster.Service, mutate func(*config.CPIConfig)) handlers.Deps {
	cfg := testConfig()
	cfg.NetworkMode = "sdn"
	cfg.SDNZoneType = "vlan"
	if mutate != nil {
		mutate(cfg)
	}
	return handlers.Deps{
		Config: cfg,
		PVE: &mockPVEClient{
			clusterSvc: clusterSvc,
			nodesSvc:   &mockNodesService{},
		},
		Logger: log.NewNopLogger(),
	}
}

// -- turnkey vlan zone: created with the config underlay bridge --

func TestCreateNetwork_TurnkeyVlanZone_CreatedWithUnderlay(t *testing.T) {
	t.Parallel()

	var zoneParams *sdkcluster.CreateSdnZonesParams
	var vnetParams *sdkcluster.CreateSdnVnetsParams

	clusterSvc := &mockSDNCluster{
		getSdnZonesFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnZonesParams) (*sdkcluster.GetSdnZonesResponse, error) {
			return nil, sdnNotFound() // zone absent → turnkey create
		},
		createSdnZonesFn: func(_ context.Context, params *sdkcluster.CreateSdnZonesParams) error {
			zoneParams = params
			return nil
		},
		getSdnVnetsFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnVnetsParams) (*sdkcluster.GetSdnVnetsResponse, error) {
			return nil, sdnNotFound()
		},
		createSdnVnetsFn: func(_ context.Context, params *sdkcluster.CreateSdnVnetsParams) error {
			vnetParams = params
			return nil
		},
		createSdnVnetsSubnetsFn: func(_ context.Context, _ string, _ *sdkcluster.CreateSdnVnetsSubnetsParams) error {
			return nil
		},
		updateSdnFn: func(_ context.Context, _ *sdkcluster.UpdateSdnParams) (*sdkcluster.UpdateSdnResponse, error) {
			return nil, nil
		},
	}

	spec := map[string]any{
		"type":    "manual",
		"range":   "10.59.0.0/24",
		"gateway": "10.59.0.1",
		"cloud_properties": map[string]any{
			"vnet":     "vlan59",
			"vnet_tag": 59,
		},
	}
	result, err := invokeCreateNetwork(t, vlanTestDeps(clusterSvc, nil), spec)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if zoneParams == nil {
		t.Fatal("CreateSdnZones was not called")
	}
	if zoneParams.Zone != "bosh" || zoneParams.Type != "vlan" {
		t.Errorf("zone create: got (%q,%q), want (bosh,vlan)", zoneParams.Zone, zoneParams.Type)
	}
	if zoneParams.Bridge == nil || *zoneParams.Bridge != "vmbr0" {
		t.Errorf("zone bridge: got %v, want underlay vmbr0 (pve.network_bridge)", zoneParams.Bridge)
	}

	if vnetParams == nil {
		t.Fatal("CreateSdnVnets was not called")
	}
	if vnetParams.Zone != "bosh" {
		t.Errorf("vnet zone: got %q, want bosh", vnetParams.Zone)
	}
	if vnetParams.Tag == nil || *vnetParams.Tag != 59 {
		t.Errorf("vnet tag: got %v, want 802.1Q VLAN ID 59", vnetParams.Tag)
	}

	// VMs join the VLAN by bridge selection: the returned cloud_properties
	// must name the vnet as the bridge.
	arr, ok := result.([]any)
	if !ok || len(arr) != 3 {
		t.Fatalf("expected 3-element array, got %T %v", result, result)
	}
	cp, _ := arr[2].(map[string]any)
	if cp["bridge"] != "vlan59" {
		t.Errorf("cloud_properties_out bridge: got %v, want vlan59 (vnet-as-bridge)", cp["bridge"])
	}
}

// -- vlan zone without an underlay bridge: actionable error, nothing created --

func TestCreateNetwork_VlanZone_MissingUnderlay_Error(t *testing.T) {
	t.Parallel()

	var zoneCreates int

	clusterSvc := &mockSDNCluster{
		getSdnZonesFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnZonesParams) (*sdkcluster.GetSdnZonesResponse, error) {
			return nil, sdnNotFound()
		},
		createSdnZonesFn: func(_ context.Context, _ *sdkcluster.CreateSdnZonesParams) error {
			zoneCreates++
			return nil
		},
	}

	spec := map[string]any{
		"type": "manual",
		"cloud_properties": map[string]any{
			"vnet":     "vlan59",
			"vnet_tag": 59,
		},
	}
	deps := vlanTestDeps(clusterSvc, func(cfg *config.CPIConfig) {
		cfg.NetworkBridge = ""
	})
	_, err := invokeCreateNetwork(t, deps, spec)
	if err == nil {
		t.Fatal("expected missing-underlay error, got nil")
	}
	if !strings.Contains(err.Error(), "pve.network_bridge") {
		t.Errorf("error %q must direct the operator to pve.network_bridge", err.Error())
	}
	if zoneCreates != 0 {
		t.Errorf("CreateSdnZones must not be called; got %d calls", zoneCreates)
	}
}

// -- explicit VLAN ID above the 802.1Q cap rejected on a vlan zone --

func TestCreateNetwork_VlanExplicitTagAbove4094_Rejected(t *testing.T) {
	t.Parallel()

	clusterSvc := &mockSDNCluster{
		getSdnZonesFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnZonesParams) (*sdkcluster.GetSdnZonesResponse, error) {
			raw := sdkcluster.GetSdnZonesResponse(`{"zone":"vlanz","type":"vlan"}`)
			return &raw, nil
		},
		getSdnVnetsFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnVnetsParams) (*sdkcluster.GetSdnVnetsResponse, error) {
			return nil, sdnNotFound()
		},
	}

	spec := map[string]any{
		"type": "manual",
		"cloud_properties": map[string]any{
			"zone":     "vlanz",
			"vnet":     "vlan4095",
			"vnet_tag": 4095,
		},
	}
	_, err := invokeCreateNetwork(t, vlanTestDeps(clusterSvc, nil), spec)
	if err == nil {
		t.Fatal("expected over-cap vnet_tag error, got nil")
	}
	if !strings.Contains(err.Error(), "4094") {
		t.Errorf("error %q must state the vlan tag maximum 4094", err.Error())
	}
}

// -- auto-allocation draws from the configured band on a vlan zone --

func TestCreateNetwork_VlanAutoAllocation_WithinConfiguredBand(t *testing.T) {
	t.Parallel()

	var vnetParams *sdkcluster.CreateSdnVnetsParams

	clusterSvc := &mockSDNCluster{
		getSdnZonesFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnZonesParams) (*sdkcluster.GetSdnZonesResponse, error) {
			raw := sdkcluster.GetSdnZonesResponse(`{"zone":"vlanz","type":"vlan"}`)
			return &raw, nil
		},
		getSdnVnetsFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnVnetsParams) (*sdkcluster.GetSdnVnetsResponse, error) {
			return nil, sdnNotFound()
		},
		listSdnVnetsFn: emptyVnetList,
		listSdnZonesFn: func(_ context.Context, _ *sdkcluster.ListSdnZonesParams) (*sdkcluster.ListSdnZonesResponse, error) {
			empty := sdkcluster.ListSdnZonesResponse{}
			return &empty, nil
		},
		createSdnVnetsFn: func(_ context.Context, params *sdkcluster.CreateSdnVnetsParams) error {
			vnetParams = params
			return nil
		},
		updateSdnFn: func(_ context.Context, _ *sdkcluster.UpdateSdnParams) (*sdkcluster.UpdateSdnResponse, error) {
			return nil, nil
		},
	}

	spec := map[string]any{
		"type": "manual",
		"cloud_properties": map[string]any{
			"zone": "vlanz",
			"vnet": "vlanauto",
		},
	}
	// ApplyDefaults with zone type vlan picks the vlan-safe 2000..2999 band;
	// the allocated 802.1Q tag must land inside it.
	deps := vlanTestDeps(clusterSvc, func(cfg *config.CPIConfig) {
		cfg.ApplyDefaults()
	})
	if _, err := invokeCreateNetwork(t, deps, spec); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if vnetParams == nil {
		t.Fatal("CreateSdnVnets was not called")
	}
	if vnetParams.Tag == nil || *vnetParams.Tag < 2000 || *vnetParams.Tag > 2999 {
		t.Errorf("vnet tag: got %v, want auto-allocated tag within the vlan-safe band [2000,2999]", vnetParams.Tag)
	}
}

// -- pre-existing vnet adopted, not recreated --

func TestCreateNetwork_VlanExistingVnet_Adopted(t *testing.T) {
	t.Parallel()

	var vnetCreates int

	clusterSvc := &mockSDNCluster{
		getSdnZonesFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnZonesParams) (*sdkcluster.GetSdnZonesResponse, error) {
			raw := sdkcluster.GetSdnZonesResponse(`{"zone":"vlanz","type":"vlan"}`)
			return &raw, nil
		},
		getSdnVnetsFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnVnetsParams) (*sdkcluster.GetSdnVnetsResponse, error) {
			raw := sdkcluster.GetSdnVnetsResponse(`{"vnet":"vlan59","zone":"vlanz","tag":59}`)
			return &raw, nil
		},
		createSdnVnetsFn: func(_ context.Context, _ *sdkcluster.CreateSdnVnetsParams) error {
			vnetCreates++
			return nil
		},
		updateSdnFn: func(_ context.Context, _ *sdkcluster.UpdateSdnParams) (*sdkcluster.UpdateSdnResponse, error) {
			return nil, nil
		},
	}

	spec := map[string]any{
		"type": "manual",
		"cloud_properties": map[string]any{
			"zone":     "vlanz",
			"vnet":     "vlan59",
			"vnet_tag": 59,
		},
	}
	result, err := invokeCreateNetwork(t, vlanTestDeps(clusterSvc, nil), spec)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if vnetCreates != 0 {
		t.Errorf("CreateSdnVnets must not be called for a pre-existing vnet; got %d calls", vnetCreates)
	}
	arr, ok := result.([]any)
	if !ok || len(arr) != 3 {
		t.Fatalf("expected 3-element array, got %T %v", result, result)
	}
	cp, _ := arr[2].(map[string]any)
	if cp["bridge"] != "vlan59" {
		t.Errorf("cloud_properties_out bridge: got %v, want vlan59", cp["bridge"])
	}
}
