// create_network_vxlan_test.go — turnkey vxlan default path: zone
// auto-creation with derived peers, VNI allocation, EVPN consume/fail-fast,
// and the vlan/qinq tag cap.
package handlers_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

// vxlanTestDeps returns Deps for the turnkey path: network_mode=sdn, zone
// type vxlan, no configured zone, auto-manage left nil (turnkey default
// true). mutate customises the config before wiring.
func vxlanTestDeps(clusterSvc sdkcluster.Service, mutate func(*config.CPIConfig)) handlers.Deps {
	cfg := testConfig()
	cfg.NetworkMode = "sdn"
	cfg.SDNZoneType = "vxlan"
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

// clusterStatusRows marshals (typ, ip, online) triples into a ListStatusResponse.
// online uses the PVE integer-boolean encoding; empty ip omits the field.
func clusterStatusRows(rows ...[3]string) *sdkcluster.ListStatusResponse {
	resp := make(sdkcluster.ListStatusResponse, 0, len(rows))
	for _, r := range rows {
		entry := map[string]any{"type": r[0], "online": r[2] == "1"}
		// PVE encodes online as integer; mirror that exactly.
		online := 0
		if r[2] == "1" {
			online = 1
		}
		entry["online"] = online
		if r[1] != "" {
			entry["ip"] = r[1]
		}
		raw, _ := json.Marshal(entry)
		resp = append(resp, raw)
	}
	return &resp
}

// emptyVnetList returns a ListSdnVnets response with no rows (VNI band all free).
func emptyVnetList(_ context.Context, _ *sdkcluster.ListSdnVnetsParams) (*sdkcluster.ListSdnVnetsResponse, error) {
	empty := sdkcluster.ListSdnVnetsResponse{}
	return &empty, nil
}

// -- turnkey default: zone "bosh" created as vxlan with derived peers --

func TestCreateNetwork_TurnkeyVxlanDefault(t *testing.T) {
	t.Parallel()

	var zoneParams *sdkcluster.CreateSdnZonesParams
	var vnetParams *sdkcluster.CreateSdnVnetsParams
	var subnetCount, updateSdnCalls int

	clusterSvc := &mockSDNCluster{
		getSdnZonesFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnZonesParams) (*sdkcluster.GetSdnZonesResponse, error) {
			return nil, sdnNotFound() // zone absent → turnkey create
		},
		listStatusFn: func(_ context.Context) (*sdkcluster.ListStatusResponse, error) {
			return clusterStatusRows(
				[3]string{"cluster", "", "1"},          // non-node excluded
				[3]string{"node", "192.168.1.20", "1"}, // included
				[3]string{"node", "192.168.1.10", "1"}, // included, sorts first
				[3]string{"node", "192.168.1.30", "0"}, // offline excluded
			), nil
		},
		createSdnZonesFn: func(_ context.Context, params *sdkcluster.CreateSdnZonesParams) error {
			zoneParams = params
			return nil
		},
		getSdnVnetsFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnVnetsParams) (*sdkcluster.GetSdnVnetsResponse, error) {
			return nil, sdnNotFound()
		},
		listSdnVnetsFn: emptyVnetList,
		createSdnVnetsFn: func(_ context.Context, params *sdkcluster.CreateSdnVnetsParams) error {
			vnetParams = params
			return nil
		},
		createSdnVnetsSubnetsFn: func(_ context.Context, _ string, _ *sdkcluster.CreateSdnVnetsSubnetsParams) error {
			subnetCount++
			return nil
		},
		updateSdnFn: func(_ context.Context, _ *sdkcluster.UpdateSdnParams) (*sdkcluster.UpdateSdnResponse, error) {
			updateSdnCalls++
			return nil, nil
		},
	}

	spec := map[string]any{
		"type":    "manual",
		"range":   "10.10.0.0/24",
		"gateway": "10.10.0.1",
		"cloud_properties": map[string]any{
			"vnet": "boshvnet", // no zone anywhere → turnkey "bosh"
		},
	}
	result, err := invokeCreateNetwork(t, vxlanTestDeps(clusterSvc, nil), spec)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if zoneParams == nil {
		t.Fatal("CreateSdnZones was not called")
	}
	if zoneParams.Zone != "bosh" || zoneParams.Type != "vxlan" {
		t.Errorf("zone create: got (%q,%q), want (bosh,vxlan)", zoneParams.Zone, zoneParams.Type)
	}
	if zoneParams.Peers == nil || *zoneParams.Peers != "192.168.1.10,192.168.1.20" {
		t.Errorf("zone peers: got %v, want 192.168.1.10,192.168.1.20", zoneParams.Peers)
	}

	if vnetParams == nil {
		t.Fatal("CreateSdnVnets was not called")
	}
	if vnetParams.Zone != "bosh" {
		t.Errorf("vnet zone: got %q, want bosh", vnetParams.Zone)
	}
	if vnetParams.Tag == nil || *vnetParams.Tag < 5000 || *vnetParams.Tag > 5999 {
		t.Errorf("vnet tag: got %v, want VNI within [5000,5999]", vnetParams.Tag)
	}

	if subnetCount != 1 {
		t.Errorf("subnet creates: got %d, want 1", subnetCount)
	}
	if updateSdnCalls == 0 {
		t.Error("UpdateSdn must be called")
	}

	arr, ok := result.([]any)
	if !ok || len(arr) != 3 {
		t.Fatalf("expected 3-element array, got %T %v", result, result)
	}
	cp, _ := arr[2].(map[string]any)
	if cp["zone"] != "bosh" {
		t.Errorf("cloud_properties_out zone: got %v, want bosh", cp["zone"])
	}
}

// -- explicit sdn_vxlan_peers override wins; /cluster/status untouched --

func TestCreateNetwork_VxlanPeersOverride(t *testing.T) {
	t.Parallel()

	var zoneParams *sdkcluster.CreateSdnZonesParams

	clusterSvc := &mockSDNCluster{
		// listStatusFn deliberately nil — explicit peers must never hit
		// /cluster/status; the mock panics if it does.
		getSdnZonesFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnZonesParams) (*sdkcluster.GetSdnZonesResponse, error) {
			return nil, sdnNotFound()
		},
		createSdnZonesFn: func(_ context.Context, params *sdkcluster.CreateSdnZonesParams) error {
			zoneParams = params
			return nil
		},
		getSdnVnetsFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnVnetsParams) (*sdkcluster.GetSdnVnetsResponse, error) {
			return nil, sdnNotFound()
		},
		listSdnVnetsFn: emptyVnetList,
		createSdnVnetsFn: func(_ context.Context, _ *sdkcluster.CreateSdnVnetsParams) error {
			return nil
		},
		updateSdnFn: func(_ context.Context, _ *sdkcluster.UpdateSdnParams) (*sdkcluster.UpdateSdnResponse, error) {
			return nil, nil
		},
	}

	spec := map[string]any{
		"type":             "manual",
		"cloud_properties": map[string]any{"vnet": "boshvnet"},
	}
	deps := vxlanTestDeps(clusterSvc, func(cfg *config.CPIConfig) {
		cfg.SDNVxlanPeers = []string{"172.16.0.1", "172.16.0.2"}
	})
	if _, err := invokeCreateNetwork(t, deps, spec); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if zoneParams == nil || zoneParams.Peers == nil || *zoneParams.Peers != "172.16.0.1,172.16.0.2" {
		t.Fatalf("zone peers: got %+v, want explicit override 172.16.0.1,172.16.0.2", zoneParams)
	}
}

// -- single-node cluster: one self-IP is a valid peer list --

func TestCreateNetwork_VxlanSingleNode(t *testing.T) {
	t.Parallel()

	var zoneParams *sdkcluster.CreateSdnZonesParams

	clusterSvc := &mockSDNCluster{
		getSdnZonesFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnZonesParams) (*sdkcluster.GetSdnZonesResponse, error) {
			return nil, sdnNotFound()
		},
		listStatusFn: func(_ context.Context) (*sdkcluster.ListStatusResponse, error) {
			return clusterStatusRows([3]string{"node", "10.0.0.5", "1"}), nil
		},
		createSdnZonesFn: func(_ context.Context, params *sdkcluster.CreateSdnZonesParams) error {
			zoneParams = params
			return nil
		},
		getSdnVnetsFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnVnetsParams) (*sdkcluster.GetSdnVnetsResponse, error) {
			return nil, sdnNotFound()
		},
		listSdnVnetsFn: emptyVnetList,
		createSdnVnetsFn: func(_ context.Context, _ *sdkcluster.CreateSdnVnetsParams) error {
			return nil
		},
		updateSdnFn: func(_ context.Context, _ *sdkcluster.UpdateSdnParams) (*sdkcluster.UpdateSdnResponse, error) {
			return nil, nil
		},
	}

	spec := map[string]any{
		"type":             "manual",
		"cloud_properties": map[string]any{"vnet": "boshvnet"},
	}
	if _, err := invokeCreateNetwork(t, vxlanTestDeps(clusterSvc, nil), spec); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if zoneParams == nil || zoneParams.Peers == nil || *zoneParams.Peers != "10.0.0.5" {
		t.Fatalf("zone peers: got %+v, want single self-IP 10.0.0.5", zoneParams)
	}
}

// -- zero derivable peers: hard error, nothing created --

func TestCreateNetwork_VxlanZeroPeers_Error(t *testing.T) {
	t.Parallel()

	var zoneCreates int

	clusterSvc := &mockSDNCluster{
		getSdnZonesFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnZonesParams) (*sdkcluster.GetSdnZonesResponse, error) {
			return nil, sdnNotFound()
		},
		listStatusFn: func(_ context.Context) (*sdkcluster.ListStatusResponse, error) {
			// Only offline / ip-less rows → zero derivable peers.
			return clusterStatusRows(
				[3]string{"node", "", "1"},
				[3]string{"node", "10.0.0.9", "0"},
			), nil
		},
		createSdnZonesFn: func(_ context.Context, _ *sdkcluster.CreateSdnZonesParams) error {
			zoneCreates++
			return nil
		},
	}

	spec := map[string]any{
		"type":             "manual",
		"cloud_properties": map[string]any{"vnet": "boshvnet"},
	}
	_, err := invokeCreateNetwork(t, vxlanTestDeps(clusterSvc, nil), spec)
	if err == nil {
		t.Fatal("expected zero-peers error, got nil")
	}
	if !strings.Contains(err.Error(), "sdn_vxlan_peers") {
		t.Errorf("error %q must direct the operator to sdn_vxlan_peers", err.Error())
	}
	if zoneCreates != 0 {
		t.Errorf("CreateSdnZones must not be called; got %d calls", zoneCreates)
	}
}

// -- EVPN zone absent: fail fast, never auto-create --

func TestCreateNetwork_EVPNAbsent_FailsFast(t *testing.T) {
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
			"zone":      "evpnz",
			"zone_type": "evpn",
			"vnet":      "boshvnet",
		},
	}
	_, err := invokeCreateNetwork(t, vxlanTestDeps(clusterSvc, nil), spec)
	if err == nil {
		t.Fatal("expected EVPN fail-fast error, got nil")
	}
	for _, want := range []string{"EVPN", "never creates", "controller"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q must mention %q", err.Error(), want)
		}
	}
	if zoneCreates != 0 {
		t.Errorf("CreateSdnZones must not be called for EVPN; got %d calls", zoneCreates)
	}
}

// -- EVPN zone present: consumed (vnet with VNI), zone never touched --

func TestCreateNetwork_EVPNPresent_Consumed(t *testing.T) {
	t.Parallel()

	var vnetParams *sdkcluster.CreateSdnVnetsParams

	clusterSvc := &mockSDNCluster{
		getSdnZonesFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnZonesParams) (*sdkcluster.GetSdnZonesResponse, error) {
			raw := sdkcluster.GetSdnZonesResponse(`{"zone":"evpnz","type":"evpn"}`)
			return &raw, nil
		},
		// createSdnZonesFn / deleteSdnZonesFn deliberately nil — any zone
		// mutation on an operator EVPN zone panics the test.
		getSdnVnetsFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnVnetsParams) (*sdkcluster.GetSdnVnetsResponse, error) {
			return nil, sdnNotFound()
		},
		listSdnVnetsFn: emptyVnetList,
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
			"zone": "evpnz",
			"vnet": "boshvnet",
		},
	}
	if _, err := invokeCreateNetwork(t, vxlanTestDeps(clusterSvc, nil), spec); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if vnetParams == nil {
		t.Fatal("CreateSdnVnets was not called")
	}
	if vnetParams.Tag == nil || *vnetParams.Tag < 5000 || *vnetParams.Tag > 5999 {
		t.Errorf("EVPN vnet tag: got %v, want VNI within [5000,5999]", vnetParams.Tag)
	}
}

// -- explicit cloud_properties.vnet_tag bypasses the allocator --

func TestCreateNetwork_ExplicitVnetTag_BypassesAllocator(t *testing.T) {
	t.Parallel()

	var vnetParams *sdkcluster.CreateSdnVnetsParams

	clusterSvc := &mockSDNCluster{
		// listSdnVnetsFn deliberately nil — an explicit tag must never
		// invoke the allocator; the mock panics if it does.
		getSdnZonesFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnZonesParams) (*sdkcluster.GetSdnZonesResponse, error) {
			raw := sdkcluster.GetSdnZonesResponse(`{"zone":"overlay","type":"vxlan"}`)
			return &raw, nil
		},
		getSdnVnetsFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnVnetsParams) (*sdkcluster.GetSdnVnetsResponse, error) {
			return nil, sdnNotFound()
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
			"zone":     "overlay",
			"vnet":     "boshvnet",
			"vnet_tag": 7777,
		},
	}
	if _, err := invokeCreateNetwork(t, vxlanTestDeps(clusterSvc, nil), spec); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if vnetParams == nil || vnetParams.Tag == nil || *vnetParams.Tag != 7777 {
		t.Fatalf("vnet tag: got %+v, want explicit 7777", vnetParams)
	}
}

// -- vnet_tag outside 24-bit VNI space rejected before any PVE call --

func TestCreateNetwork_VnetTagOutOfRange_RejectedPrePVE(t *testing.T) {
	t.Parallel()

	// All mock fns nil — any PVE call panics, proving preflight rejection.
	clusterSvc := &mockSDNCluster{}

	spec := map[string]any{
		"type": "manual",
		"cloud_properties": map[string]any{
			"zone":     "overlay",
			"vnet":     "boshvnet",
			"vnet_tag": 16777216,
		},
	}
	_, err := invokeCreateNetwork(t, vxlanTestDeps(clusterSvc, nil), spec)
	if err == nil {
		t.Fatal("expected out-of-range vnet_tag error, got nil")
	}
	if !strings.Contains(err.Error(), "16777215") {
		t.Errorf("error %q must state the 24-bit ceiling", err.Error())
	}
}

// -- live vlan zone with an over-cap band: actionable error (backstop) --
//
// The config-level validation only sees the CONFIGURED zone type (vxlan
// here), so an explicit 5000+ band passes Load; the allocation-time check is
// the backstop when the live zone's effective type turns out to be vlan.

func TestCreateNetwork_VlanBandAbove4094_Rejected(t *testing.T) {
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
			"zone": "vlanz",
			"vnet": "boshvnet",
		},
	}
	deps := vxlanTestDeps(clusterSvc, func(cfg *config.CPIConfig) {
		cfg.SDNVNIRangeStart = 5000
		cfg.SDNVNIRangeEnd = 5999
	})
	_, err := invokeCreateNetwork(t, deps, spec)
	if err == nil {
		t.Fatal("expected vlan band error, got nil")
	}
	if !strings.Contains(err.Error(), "4094") {
		t.Errorf("error %q must state the vlan tag maximum 4094", err.Error())
	}
}

// -- never-defaulted config + live vlan zone: fallback band is vlan-safe --

func TestCreateNetwork_VlanBareConfig_FallbackBandVlanSafe(t *testing.T) {
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
			"vnet": "boshvnet",
		},
	}
	// No ApplyDefaults, band fields zero: the handler-level fallback must
	// pick a vlan-safe band, mirroring the config-level default.
	if _, err := invokeCreateNetwork(t, vxlanTestDeps(clusterSvc, nil), spec); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if vnetParams == nil {
		t.Fatal("CreateSdnVnets was not called")
	}
	if vnetParams.Tag == nil || *vnetParams.Tag < 2000 || *vnetParams.Tag > 2999 {
		t.Errorf("vnet tag: got %v, want fallback allocation within [2000,2999]", vnetParams.Tag)
	}
}

// -- pre-existing simple zone: vnet stays untagged --

func TestCreateNetwork_SimpleZone_VnetWithoutTag(t *testing.T) {
	t.Parallel()

	var vnetParams *sdkcluster.CreateSdnVnetsParams

	clusterSvc := &mockSDNCluster{
		getSdnZonesFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnZonesParams) (*sdkcluster.GetSdnZonesResponse, error) {
			raw := sdkcluster.GetSdnZonesResponse(`{"zone":"labz","type":"simple"}`)
			return &raw, nil
		},
		getSdnVnetsFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnVnetsParams) (*sdkcluster.GetSdnVnetsResponse, error) {
			return nil, sdnNotFound()
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
			"zone": "labz",
			"vnet": "boshvnet",
		},
	}
	if _, err := invokeCreateNetwork(t, vxlanTestDeps(clusterSvc, nil), spec); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if vnetParams == nil {
		t.Fatal("CreateSdnVnets was not called")
	}
	if vnetParams.Tag != nil {
		t.Errorf("simple-zone vnet must be untagged; got tag %d", *vnetParams.Tag)
	}
}

// -- VNI band exhausted: error names the config keys --

func TestCreateNetwork_VNIExhaustion_ActionableError(t *testing.T) {
	t.Parallel()

	clusterSvc := &mockSDNCluster{
		getSdnZonesFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnZonesParams) (*sdkcluster.GetSdnZonesResponse, error) {
			raw := sdkcluster.GetSdnZonesResponse(`{"zone":"overlay","type":"vxlan"}`)
			return &raw, nil
		},
		getSdnVnetsFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnVnetsParams) (*sdkcluster.GetSdnVnetsResponse, error) {
			return nil, sdnNotFound()
		},
		listSdnVnetsFn: func(_ context.Context, _ *sdkcluster.ListSdnVnetsParams) (*sdkcluster.ListSdnVnetsResponse, error) {
			rows := sdkcluster.ListSdnVnetsResponse{
				json.RawMessage(`{"vnet":"a","zone":"overlay","tag":6000}`),
				json.RawMessage(`{"vnet":"b","zone":"overlay","tag":6001}`),
			}
			return &rows, nil
		},
		// This test is specifically about vnet-tag exhaustion; the §1.7
		// zone-level exclusion check (NextVNI now also calls ListSdnZones)
		// contributes nothing extra here, so return an empty zone list.
		listSdnZonesFn: func(_ context.Context, _ *sdkcluster.ListSdnZonesParams) (*sdkcluster.ListSdnZonesResponse, error) {
			empty := sdkcluster.ListSdnZonesResponse{}
			return &empty, nil
		},
	}

	spec := map[string]any{
		"type": "manual",
		"cloud_properties": map[string]any{
			"zone": "overlay",
			"vnet": "boshvnet",
		},
	}
	deps := vxlanTestDeps(clusterSvc, func(cfg *config.CPIConfig) {
		cfg.SDNVNIRangeStart = 6000
		cfg.SDNVNIRangeEnd = 6001
	})
	_, err := invokeCreateNetwork(t, deps, spec)
	if err == nil {
		t.Fatal("expected VNI exhaustion error, got nil")
	}
	for _, want := range []string{"sdn_vni_range_start", "vnet_tag"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q must mention %q", err.Error(), want)
		}
	}
}

// -- sdn_zone_mtu flows into the zone-create payload --

func TestCreateNetwork_ZoneMTUForwarded(t *testing.T) {
	t.Parallel()

	var zoneParams *sdkcluster.CreateSdnZonesParams

	clusterSvc := &mockSDNCluster{
		getSdnZonesFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnZonesParams) (*sdkcluster.GetSdnZonesResponse, error) {
			return nil, sdnNotFound()
		},
		listStatusFn: func(_ context.Context) (*sdkcluster.ListStatusResponse, error) {
			return clusterStatusRows([3]string{"node", "10.0.0.5", "1"}), nil
		},
		createSdnZonesFn: func(_ context.Context, params *sdkcluster.CreateSdnZonesParams) error {
			zoneParams = params
			return nil
		},
		getSdnVnetsFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnVnetsParams) (*sdkcluster.GetSdnVnetsResponse, error) {
			return nil, sdnNotFound()
		},
		listSdnVnetsFn: emptyVnetList,
		createSdnVnetsFn: func(_ context.Context, _ *sdkcluster.CreateSdnVnetsParams) error {
			return nil
		},
		updateSdnFn: func(_ context.Context, _ *sdkcluster.UpdateSdnParams) (*sdkcluster.UpdateSdnResponse, error) {
			return nil, nil
		},
	}

	spec := map[string]any{
		"type":             "manual",
		"cloud_properties": map[string]any{"vnet": "boshvnet"},
	}
	deps := vxlanTestDeps(clusterSvc, func(cfg *config.CPIConfig) {
		mtu := int64(8950)
		cfg.SDNZoneMTU = &mtu
	})
	if _, err := invokeCreateNetwork(t, deps, spec); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if zoneParams == nil || zoneParams.Mtu == nil || *zoneParams.Mtu != 8950 {
		t.Fatalf("zone mtu: got %+v, want 8950", zoneParams)
	}
}

// -- regression: explicit auto mode with no zone/vnet still takes the bridge path --

func TestCreateNetwork_AutoModeNoZone_BridgeFallback(t *testing.T) {
	t.Parallel()

	// SDN mock fns all nil — the bridge path must never touch SDN.
	clusterSvc := &mockSDNCluster{}

	var bridgeCreates int
	nodesSvc := &mockBridgeNodes{
		createNetworkFn: func(_ context.Context, _ string, _ *sdknodes.CreateNetworkParams) error {
			bridgeCreates++
			return nil
		},
	}

	cfg := testConfig()
	cfg.NetworkMode = "auto"
	deps := handlers.Deps{
		Config: cfg,
		PVE: &mockPVEClient{
			clusterSvc: clusterSvc,
			nodesSvc:   nodesSvc,
		},
		Logger: log.NewNopLogger(),
	}

	spec := map[string]any{
		"type":             "manual",
		"cloud_properties": map[string]any{},
	}
	if _, err := invokeCreateNetwork(t, deps, spec); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if bridgeCreates != 1 {
		t.Errorf("bridge creates: got %d, want 1 (bridge path)", bridgeCreates)
	}
}

// -- explicit vnet_tag on a simple zone is a config contradiction --

func TestCreateNetwork_VnetTagOnSimpleZone_Error(t *testing.T) {
	t.Parallel()

	clusterSvc := &mockSDNCluster{
		getSdnZonesFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnZonesParams) (*sdkcluster.GetSdnZonesResponse, error) {
			raw := sdkcluster.GetSdnZonesResponse(`{"zone":"labz","type":"simple"}`)
			return &raw, nil
		},
		getSdnVnetsFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnVnetsParams) (*sdkcluster.GetSdnVnetsResponse, error) {
			return nil, sdnNotFound()
		},
	}

	spec := map[string]any{
		"type": "manual",
		"cloud_properties": map[string]any{
			"zone":     "labz",
			"vnet":     "boshvnet",
			"vnet_tag": 100,
		},
	}
	_, err := invokeCreateNetwork(t, vxlanTestDeps(clusterSvc, nil), spec)
	if err == nil {
		t.Fatal("expected vnet_tag-on-simple-zone error, got nil")
	}
	if !strings.Contains(err.Error(), "simple") {
		t.Errorf("error %q must name the offending zone type", err.Error())
	}
}
