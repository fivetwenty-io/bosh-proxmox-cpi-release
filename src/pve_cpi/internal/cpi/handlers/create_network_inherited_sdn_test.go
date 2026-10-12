package handlers_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
)

const inheritedSDNEntryHost = "az2.example"

// inheritedSDNCluster returns an SDN mock whose zone lookup answers
// zoneExists, and which counts CreateSdnZones calls.
func inheritedSDNCluster(zoneExists string, createdZones *int) *mockSDNCluster {
	return &mockSDNCluster{
		getSdnZonesFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnZonesParams) (*sdkcluster.GetSdnZonesResponse, error) {
			if zoneExists == "" {
				return nil, sdnNotFound()
			}
			raw := sdkcluster.GetSdnZonesResponse(zoneExists)
			return &raw, nil
		},
		createSdnZonesFn: func(_ context.Context, _ *sdkcluster.CreateSdnZonesParams) error {
			*createdZones++
			return nil
		},
		getSdnVnetsFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnVnetsParams) (*sdkcluster.GetSdnVnetsResponse, error) {
			return nil, sdnNotFound()
		},
		createSdnVnetsFn: func(_ context.Context, _ *sdkcluster.CreateSdnVnetsParams) error { return nil },
		updateSdnFn: func(_ context.Context, _ *sdkcluster.UpdateSdnParams) (*sdkcluster.UpdateSdnResponse, error) {
			return nil, nil
		},
	}
}

// inheritedSDNDeps returns the request Deps WithRequestOverrides would hand
// create_network for an entry on another endpoint that inherited the given
// SDN settings. The job runs bridge mode, so only a network that names its
// own zone reaches the SDN path, which is the case the config-time rule
// cannot see.
func inheritedSDNDeps(clusterSvc sdkcluster.Service, overridden bool, inherited ...string) handlers.Deps {
	cfg := testConfig()
	cfg.Host = inheritedSDNEntryHost
	cfg.NetworkMode = config.NetworkModeBridge
	cfg.SDNZoneType = "vxlan"
	cfg.SDNVxlanPeers = []string{"192.0.2.11", "192.0.2.12"}
	autoManage := true
	cfg.SDNAutoManageZone = &autoManage
	return handlers.Deps{
		Config:               cfg,
		PVE:                  &mockPVEClient{clusterSvc: clusterSvc, nodesSvc: &mockNodesService{}},
		Logger:               log.NewNopLogger(),
		RequestOverridden:    overridden,
		InheritedSDNSettings: inherited,
	}
}

// TestHandleCreateNetwork_SDN_RefusesZoneFromInheritedSettings asserts that
// create_network refuses, without retry, to auto-create a zone on the
// entry's cluster from job-level SDN settings the entry inherited, and that
// it creates the zone as before whenever the zone would not use them.
func TestHandleCreateNetwork_SDN_RefusesZoneFromInheritedSettings(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		overridden bool
		inherited  []string
		zoneExists string
		cp         map[string]any
		wantKeys   []string // nil means the zone is created or reused
	}{
		{
			name: "inherited peers for a vxlan zone", overridden: true,
			inherited: []string{"pve_sdn_vxlan_peers"},
			cp:        map[string]any{"zone": "z1", "vnet": "v1", "vnet_tag": 5100},
			wantKeys:  []string{"pve.sdn_vxlan_peers"},
		},
		{
			name: "inherited zone type the network does not name", overridden: true,
			inherited: []string{"pve_sdn_zone_type"},
			cp:        map[string]any{"zone": "z1", "vnet": "v1", "vnet_tag": 5100},
			wantKeys:  []string{"pve.sdn_zone_type"},
		},
		{
			name: "inherited MTU", overridden: true,
			inherited: []string{"pve_sdn_zone_mtu"},
			cp:        map[string]any{"zone": "z1", "vnet": "v1", "zone_type": "simple"},
			wantKeys:  []string{"pve.sdn_zone_mtu"},
		},
		{
			name: "inherited VNI band for an auto-allocated tag", overridden: true,
			inherited: []string{"pve_sdn_vni_range_start", "pve_sdn_vni_range_end"},
			cp:        map[string]any{"zone": "z1", "vnet": "v1"},
			wantKeys:  []string{"pve.sdn_vni_range_start", "pve.sdn_vni_range_end"},
		},
		{
			name: "peers unused by a simple zone the network names", overridden: true,
			inherited: []string{"pve_sdn_vxlan_peers", "pve_sdn_zone_type"},
			cp:        map[string]any{"zone": "z1", "vnet": "v1", "zone_type": "simple"},
		},
		{
			name: "VNI band unused when the network pins its tag", overridden: true,
			inherited: []string{"pve_sdn_vni_range_start", "pve_sdn_vni_range_end"},
			cp:        map[string]any{"zone": "z1", "vnet": "v1", "vnet_tag": 5100},
		},
		{
			name: "the zone already exists", overridden: true,
			inherited:  []string{"pve_sdn_vxlan_peers", "pve_sdn_zone_mtu"},
			zoneExists: `{"zone":"z1","type":"vxlan"}`,
			cp:         map[string]any{"zone": "z1", "vnet": "v1", "vnet_tag": 5100},
		},
		{
			name:       "a request that kept the job endpoint",
			overridden: false,
			cp:         map[string]any{"zone": "z1", "vnet": "v1", "vnet_tag": 5100},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			created := 0
			deps := inheritedSDNDeps(inheritedSDNCluster(tc.zoneExists, &created), tc.overridden, tc.inherited...)
			_, err := invokeCreateNetwork(t, deps, map[string]any{"type": "manual", "cloud_properties": tc.cp})
			if tc.wantKeys == nil {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				wantCreated := 1
				if tc.zoneExists != "" {
					wantCreated = 0
				}
				if created != wantCreated {
					t.Errorf("CreateSdnZones calls = %d, want %d", created, wantCreated)
				}
				return
			}
			if err == nil {
				t.Fatal("expected create_network to refuse the zone")
			}
			var cpiErr *cpierrors.Error
			if !errors.As(err, &cpiErr) || cpiErr.OkToRetry() {
				t.Errorf("error %v must be a non-retriable CloudError", err)
			}
			for _, want := range append([]string{inheritedSDNEntryHost, `"z1"`}, tc.wantKeys...) {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err.Error(), want)
				}
			}
			if strings.Contains(err.Error(), "192.0.2.11") {
				t.Errorf("error %q carries a setting value; it must name property keys only", err.Error())
			}
			if created != 0 {
				t.Errorf("CreateSdnZones calls = %d, want none for a refused zone", created)
			}
		})
	}
}

// TestHandleCreateNetwork_Bridge_IgnoresInheritedSDNSettings asserts that a
// bridge-only network on an entry that inherited SDN settings behaves as
// before.
func TestHandleCreateNetwork_Bridge_IgnoresInheritedSDNSettings(t *testing.T) {
	t.Parallel()
	created := 0
	clusterSvc := inheritedSDNCluster("", &created)
	deps := inheritedSDNDeps(clusterSvc, true, "pve_sdn_vxlan_peers", "pve_sdn_zone_mtu")
	deps.Config.NetworkBridge = "vmbr0"
	deps.PVE = &mockPVEClient{clusterSvc: clusterSvc, nodesSvc: &mockBridgeNodes{}}
	result, err := invokeCreateNetwork(t, deps, map[string]any{
		"type": "manual", "cloud_properties": map[string]any{"bridge": "vmbr0"},
	})
	if err != nil {
		t.Fatalf("unexpected error on the bridge path: %v", err)
	}
	if result == nil {
		t.Error("bridge path returned no result")
	}
	if created != 0 {
		t.Errorf("CreateSdnZones calls = %d, want none on the bridge path", created)
	}
}

// TestWithRequestOverrides_RecordsInheritedSDNSettings asserts that the
// request Deps carry the job SDN settings an entry on another endpoint
// inherited, and nothing on the job endpoint or when the entry names them.
func TestWithRequestOverrides_RecordsInheritedSDNSettings(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		extra map[string]any
		want  []string
	}{
		{"another host inherits the peers", map[string]any{
			"pve_host": inheritedSDNEntryHost, "pve_api_token": overrideTestEntryToken,
		}, []string{"pve_sdn_vxlan_peers"}},
		{"another host names its own peers", map[string]any{
			"pve_host": inheritedSDNEntryHost, "pve_api_token": overrideTestEntryToken,
			"pve_sdn_vxlan_peers": []any{"198.51.100.11"},
		}, nil},
		{"the job endpoint", map[string]any{"pve_vm_storage": "vm-b"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			base := overrideTestBaseConfig()
			base.NetworkMode = config.NetworkModeBridge
			base.SDNZoneType = "vxlan"
			base.SDNVxlanPeers = []string{"192.0.2.11"}
			deps, _, _ := clusterScopeDeps(base)
			got, err := deps.WithRequestOverrides(context.Background(), jsonrpc.Context{Extra: tc.extra})
			if err != nil {
				t.Fatalf("WithRequestOverrides: %v", err)
			}
			if !reflect.DeepEqual(got.InheritedSDNSettings, tc.want) {
				t.Errorf("InheritedSDNSettings = %#v, want %#v", got.InheritedSDNSettings, tc.want)
			}
		})
	}
}
