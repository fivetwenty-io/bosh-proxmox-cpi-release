// Package handlers tests for the IP-conflict detector.
// Uses package handlers (internal test) to access private functions directly.
package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/agent"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cloudinit"
	sdkcluster "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/clusterstorage"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/qemu"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/storage"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/tasks"
)

// --------------------------------------------------------------------------
// helpers
// --------------------------------------------------------------------------

// ipVMResource builds a cluster ListResources JSON entry for a QEMU VM.
// All test VMs are placed on the single test node "pve1".
func ipVMResource(vmid int, name string) json.RawMessage {
	m := map[string]any{
		"vmid": vmid,
		"node": "pve1",
		"type": "qemu",
	}
	if name != "" {
		m["name"] = name
	}
	b, _ := json.Marshal(m)
	return b
}

// ipListResp wraps raw JSON entries into a *cluster.ListResourcesResponse.
func ipListResp(entries ...json.RawMessage) *sdkcluster.ListResourcesResponse {
	r := sdkcluster.ListResourcesResponse(entries)
	return &r
}

// --------------------------------------------------------------------------
// Unit tests for private helper functions
// --------------------------------------------------------------------------

func TestExtractStaticIP(t *testing.T) {
	cases := []struct {
		name     string
		ipconfig string
		want     string
	}{
		{"dhcp", "ip=dhcp", ""},
		{"static with prefix", "ip=10.0.0.5/24,gw=10.0.0.1", "10.0.0.5"},
		{"static no prefix", "ip=10.0.0.5", "10.0.0.5"},
		{"static no gw", "ip=192.168.1.100/24", "192.168.1.100"},
		{"ip6 auto", "ip6=auto", ""},
		{"ip6 static ignored", "ip6=fd00::1/64", ""},
		{"empty string", "", ""},
		{"only gw", "gw=10.0.0.1", ""},
		{"mixed ip6 and ip4 static", "ip6=auto,ip=10.0.0.5/24", "10.0.0.5"},
		{"DHCP uppercase", "ip=DHCP", ""},
		{"trailing comma", "ip=10.1.2.3/24,", "10.1.2.3"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := extractStaticIP(tc.ipconfig)
			if got != tc.want {
				t.Errorf("extractStaticIP(%q) = %q; want %q", tc.ipconfig, got, tc.want)
			}
		})
	}
}

func TestNicIsOnBridge(t *testing.T) {
	cases := []struct {
		name   string
		netVal string
		bridge string
		want   bool
	}{
		{"exact match", "virtio=aa:bb:cc:dd:ee:ff,bridge=vmbr0", "vmbr0", true},
		{"wrong bridge", "virtio,bridge=vmbr1", "vmbr0", false},
		{"no bridge key", "virtio=aa:bb:cc", "vmbr0", false},
		{"bridge with firewall", "virtio,bridge=vmbr0,firewall=1", "vmbr0", true},
		{"case insensitive bridge key", "virtio,Bridge=vmbr0", "vmbr0", true},
		{"empty netVal", "", "vmbr0", false},
		{"SDN vnet", "virtio,bridge=cpitest0", "cpitest0", true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := nicIsOnBridge(tc.netVal, tc.bridge)
			if got != tc.want {
				t.Errorf("nicIsOnBridge(%q, %q) = %v; want %v", tc.netVal, tc.bridge, got, tc.want)
			}
		})
	}
}

func TestNicIndicesOnBridge(t *testing.T) {
	t.Run("empty bridge returns nil filter", func(t *testing.T) {
		cfg := map[string]any{
			"net0": "virtio,bridge=vmbr0",
		}
		result := nicIndicesOnBridge(cfg, "")
		if result != nil {
			t.Errorf("expected nil for empty bridge, got %v", result)
		}
	})

	t.Run("returns matching NIC indices", func(t *testing.T) {
		cfg := map[string]any{
			"net0": "virtio,bridge=vmbr0",
			"net1": "virtio,bridge=vmbr1",
			"net2": "virtio,bridge=vmbr0",
			"name": "somevm",
		}
		result := nicIndicesOnBridge(cfg, "vmbr0")
		if result == nil {
			t.Fatal("expected non-nil map")
		}
		if _, ok := result[0]; !ok {
			t.Error("expected net0 (index 0) in result")
		}
		if _, ok := result[2]; !ok {
			t.Error("expected net2 (index 2) in result")
		}
		if _, ok := result[1]; ok {
			t.Error("net1 (vmbr1) must not be in result for vmbr0 scan")
		}
	})

	t.Run("non-numeric net key skipped", func(t *testing.T) {
		cfg := map[string]any{
			"netXYZ": "virtio,bridge=vmbr0",
		}
		result := nicIndicesOnBridge(cfg, "vmbr0")
		if len(result) != 0 {
			t.Errorf("expected empty map for non-numeric net key, got %v", result)
		}
	})

	t.Run("non-string net value skipped", func(t *testing.T) {
		cfg := map[string]any{
			"net0": 12345,
		}
		result := nicIndicesOnBridge(cfg, "vmbr0")
		if len(result) != 0 {
			t.Errorf("expected empty map for non-string net value, got %v", result)
		}
	})
}

func TestParseIPConflict(t *testing.T) {
	targetSet := map[string]struct{}{"10.0.0.5": {}}

	t.Run("conflict found on matching bridge NIC", func(t *testing.T) {
		cfg := map[string]any{
			"ipconfig0": "ip=10.0.0.5/24,gw=10.0.0.1",
			"net0":      "virtio,bridge=vmbr0",
		}
		bridgeNICs := map[int]struct{}{0: {}}
		result := parseIPConflict(cfg, targetSet, bridgeNICs, "vmbr0", 42, "my-vm")
		if result == nil {
			t.Fatal("expected conflict, got nil")
		}
		if result.VMID != 42 || result.IP != "10.0.0.5" || result.Name != "my-vm" {
			t.Errorf("unexpected conflict: %+v", result)
		}
	})

	t.Run("no conflict when NIC not on target bridge", func(t *testing.T) {
		cfg := map[string]any{
			"ipconfig0": "ip=10.0.0.5/24",
			"net0":      "virtio,bridge=vmbr1",
		}
		// bridgeNICs does NOT include index 0 (net0 is on vmbr1, not vmbr0).
		bridgeNICs := map[int]struct{}{} // empty = no NIC on target bridge
		result := parseIPConflict(cfg, targetSet, bridgeNICs, "vmbr0", 43, "vm-43")
		if result != nil {
			t.Errorf("expected nil, got %+v", result)
		}
	})

	t.Run("nil bridgeNICs means no filter", func(t *testing.T) {
		cfg := map[string]any{
			"ipconfig0": "ip=10.0.0.5/24",
		}
		result := parseIPConflict(cfg, targetSet, nil, "", 44, "unfiltered-vm")
		if result == nil {
			t.Fatal("expected conflict with nil bridgeNICs, got nil")
		}
	})

	t.Run("malformed ipconfig key suffix skipped", func(t *testing.T) {
		cfg := map[string]any{
			"ipconfigABC": "ip=10.0.0.5/24",
		}
		result := parseIPConflict(cfg, targetSet, nil, "", 45, "vm-45")
		if result != nil {
			t.Errorf("malformed ipconfig key must be skipped, got %+v", result)
		}
	})

	t.Run("DHCP entry not conflicting", func(t *testing.T) {
		cfg := map[string]any{
			"ipconfig0": "ip=dhcp",
		}
		result := parseIPConflict(cfg, targetSet, nil, "", 46, "dhcp-vm")
		if result != nil {
			t.Errorf("DHCP must not conflict, got %+v", result)
		}
	})
}

func TestIPConflictCloudError_Internal(t *testing.T) {
	conflict := &IPConflict{VMID: 101, Name: "test-vm", IP: "10.0.0.1"}
	err := IPConflictCloudError(conflict, "vmbr0")
	if err == nil {
		t.Fatal("expected non-nil error")
	}
	if err.OkToRetry() {
		t.Error("IP conflict must not be retriable")
	}
	if !cpierrors.IsType(err, cpierrors.TypeCloud) {
		t.Errorf("expected TypeCloud error type")
	}
	msg := err.Error()
	for _, want := range []string{"101", "test-vm", "10.0.0.1", "vmbr0"} {
		found := false
		for i := 0; i <= len(msg)-len(want); i++ {
			if msg[i:i+len(want)] == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("error message %q missing %q", msg, want)
		}
	}
}

// --------------------------------------------------------------------------
// Mocks for detectIPConflict integration tests
// --------------------------------------------------------------------------

// icPVEClient satisfies pve.Client. Only QEMU() and Cluster() return real mocks;
// all other accessors return nil (detectIPConflict only calls those two).
type icPVEClient struct {
	qemuSvc    qemu.Service
	clusterSvc sdkcluster.Service
	nodesSvc   nodes.Service
	poolsSvc   pve.PoolService
}

var _ pve.Client = (*icPVEClient)(nil)

func (c *icPVEClient) QEMU() qemu.Service                     { return c.qemuSvc }
func (c *icPVEClient) Cluster() sdkcluster.Service            { return c.clusterSvc }
func (c *icPVEClient) Storage() storage.Service               { return nil }
func (c *icPVEClient) CloudInit() cloudinit.Service           { return nil }
func (c *icPVEClient) Tasks() tasks.Service                   { return nil }
func (c *icPVEClient) Nodes() nodes.Service                   { return c.nodesSvc }
func (c *icPVEClient) ClusterStorage() clusterstorage.Service { return nil }
func (c *icPVEClient) Pools() pve.PoolService                 { return c.poolsSvc }

// icQEMUService satisfies qemu.Service for IP-conflict tests.
// Only Config() is exercised; all other methods panic on accidental call.
type icQEMUService struct {
	configFn func(ctx context.Context, node string, vmid int) (map[string]any, error)
}

var _ qemu.Service = (*icQEMUService)(nil)

func (m *icQEMUService) Config(ctx context.Context, node string, vmid int) (map[string]any, error) {
	if m.configFn != nil {
		return m.configFn(ctx, node, vmid)
	}
	return map[string]any{}, nil
}
func (m *icQEMUService) Create(_ context.Context, _ string, _ map[string]any) (string, error) {
	panic("icQEMUService.Create: not expected")
}
func (m *icQEMUService) Status(_ context.Context, _ string, _ int) (map[string]any, error) {
	panic("icQEMUService.Status: not expected")
}
func (m *icQEMUService) Start(_ context.Context, _ string, _ int) (string, error) {
	panic("icQEMUService.Start: not expected")
}
func (m *icQEMUService) Stop(_ context.Context, _ string, _ int) (string, error) {
	panic("icQEMUService.Stop: not expected")
}
func (m *icQEMUService) Reset(_ context.Context, _ string, _ int) (string, error) {
	panic("icQEMUService.Reset: not expected")
}
func (m *icQEMUService) Clone(_ context.Context, _ string, _ int, _ map[string]any) (string, error) {
	panic("icQEMUService.Clone: not expected")
}
func (m *icQEMUService) Template(_ context.Context, _ string, _ int) (string, error) {
	panic("icQEMUService.Template: not expected")
}
func (m *icQEMUService) AttachDisk(_ context.Context, _ string, _ int, _ string, _ string, _ *qemu.AttachOpts) (string, error) {
	panic("icQEMUService.AttachDisk: not expected")
}
func (m *icQEMUService) DetachDisk(_ context.Context, _ string, _ int, _ string) error {
	panic("icQEMUService.DetachDisk: not expected")
}
func (m *icQEMUService) ResizeDisk(_ context.Context, _ string, _ int, _ string, _ int) (string, error) {
	panic("icQEMUService.ResizeDisk: not expected")
}
func (m *icQEMUService) Snapshot(_ context.Context, _ string, _ int, _ string, _ map[string]any) (string, error) {
	panic("icQEMUService.Snapshot: not expected")
}
func (m *icQEMUService) DeleteSnapshot(_ context.Context, _ string, _ int, _ string) error {
	panic("icQEMUService.DeleteSnapshot: not expected")
}
func (m *icQEMUService) ListSnapshots(_ context.Context, _ string, _ int) ([]map[string]any, error) {
	panic("icQEMUService.ListSnapshots: not expected")
}
func (m *icQEMUService) RollbackSnapshot(_ context.Context, _ string, _ int, _ string) (string, error) {
	panic("icQEMUService.RollbackSnapshot: not expected")
}

// icClusterService satisfies cluster.Service for IP-conflict tests.
// Only ListResources() is wired; all others panic via the embedded nil interface.
type icClusterService struct {
	sdkcluster.Service
	listFn func(context.Context, *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error)
}

var _ sdkcluster.Service = (*icClusterService)(nil)

func (m *icClusterService) ListResources(ctx context.Context, params *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
	if m.listFn != nil {
		return m.listFn(ctx, params)
	}
	empty := sdkcluster.ListResourcesResponse{}
	return &empty, nil
}

// icAgentStub satisfies agent.Agent with no-ops.
type icAgentStub struct{}

func (a *icAgentStub) Configure(_ context.Context, _ string, _ int, _ agent.AgentConfig) error {
	return nil
}
func (a *icAgentStub) Remove(_ context.Context, _ string, _ int) error { return nil }

// icMinConfig returns a minimal *config.CPIConfig for internal tests.
func icMinConfig() *config.CPIConfig {
	v := false
	return &config.CPIConfig{
		Host:           "pve.test.local",
		Port:           8006,
		User:           "root",
		APIToken:       "test-token",
		Node:           "pve-node1",
		VMStorage:      "local-lvm",
		DiskStorage:    "local-lvm",
		NetworkBridge:  "vmbr0",
		AgentMode:      "noagent",
		VMDiskFormat:   "qcow2",
		VerifySSL:      &v,
		VMIDRangeStart: 100,
	}
}

// icDeps builds Deps for detectIPConflict integration tests.
func icDeps(
	listFn func(context.Context, *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error),
	cfgFn func(ctx context.Context, node string, vmid int) (map[string]any, error),
) Deps {
	return Deps{
		Config: icMinConfig(),
		PVE: &icPVEClient{
			qemuSvc:    &icQEMUService{configFn: cfgFn},
			clusterSvc: &icClusterService{listFn: listFn},
		},
		Agent:  &icAgentStub{},
		Logger: log.NewNopLogger(),
	}
}

// --------------------------------------------------------------------------
// Integration-level tests exercising detectIPConflict end-to-end
// --------------------------------------------------------------------------

func TestDetectIPConflict_NilTargets(t *testing.T) {
	called := false
	deps := icDeps(func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
		called = true
		return ipListResp(), nil
	}, nil)
	conflict, err := detectIPConflict(context.Background(), deps, nil, "vmbr0", 0)
	if err != nil || conflict != nil {
		t.Fatalf("nil targetIPs: expected (nil,nil), got (%v,%v)", conflict, err)
	}
	if called {
		t.Error("ListResources must not be called for nil targetIPs")
	}
}

func TestDetectIPConflict_EmptyCluster(t *testing.T) {
	deps := icDeps(func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
		return ipListResp(), nil
	}, nil)
	conflict, err := detectIPConflict(context.Background(), deps, []string{"10.0.0.5"}, "vmbr0", 0)
	if err != nil || conflict != nil {
		t.Fatalf("empty cluster: expected (nil,nil), got (%v,%v)", conflict, err)
	}
}

func TestDetectIPConflict_NoConflict(t *testing.T) {
	deps := icDeps(
		func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			return ipListResp(ipVMResource(100, "vm-100"), ipVMResource(101, "vm-101")), nil
		},
		func(_ context.Context, _ string, vmid int) (map[string]any, error) {
			switch vmid {
			case 100:
				return map[string]any{"name": "vm-100", "ipconfig0": "ip=10.0.0.10/24", "net0": "virtio,bridge=vmbr0"}, nil
			case 101:
				return map[string]any{"name": "vm-101", "ipconfig0": "ip=dhcp", "net0": "virtio,bridge=vmbr0"}, nil
			}
			return map[string]any{}, nil
		},
	)
	conflict, err := detectIPConflict(context.Background(), deps, []string{"10.0.0.5"}, "vmbr0", 0)
	if err != nil || conflict != nil {
		t.Fatalf("no-conflict: expected (nil,nil), got (%v,%v)", conflict, err)
	}
}

func TestDetectIPConflict_ConflictFound(t *testing.T) {
	deps := icDeps(
		func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			return ipListResp(ipVMResource(200, "conflict-vm"), ipVMResource(201, "ok-vm")), nil
		},
		func(_ context.Context, _ string, vmid int) (map[string]any, error) {
			switch vmid {
			case 200:
				return map[string]any{"name": "conflict-vm", "ipconfig0": "ip=10.0.0.5/24,gw=10.0.0.1", "net0": "virtio,bridge=vmbr0"}, nil
			case 201:
				return map[string]any{"name": "ok-vm", "ipconfig0": "ip=10.0.0.20/24", "net0": "virtio,bridge=vmbr0"}, nil
			}
			return map[string]any{}, nil
		},
	)
	conflict, err := detectIPConflict(context.Background(), deps, []string{"10.0.0.5"}, "vmbr0", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if conflict == nil {
		t.Fatal("expected conflict, got nil")
	}
	if conflict.VMID != 200 || conflict.IP != "10.0.0.5" || conflict.Name != "conflict-vm" {
		t.Errorf("unexpected conflict fields: %+v", conflict)
	}
}

func TestDetectIPConflict_BridgeFilterPreventsMatch(t *testing.T) {
	// IP matches but NIC is on vmbr1; scanning vmbr0 must not find a conflict.
	deps := icDeps(
		func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			return ipListResp(ipVMResource(300, "other-bridge")), nil
		},
		func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{"name": "other-bridge", "ipconfig0": "ip=10.0.0.5/24", "net0": "virtio,bridge=vmbr1"}, nil
		},
	)
	conflict, err := detectIPConflict(context.Background(), deps, []string{"10.0.0.5"}, "vmbr0", 0)
	if err != nil || conflict != nil {
		t.Fatalf("bridge filter must prevent match: (%v,%v)", conflict, err)
	}
}

func TestDetectIPConflict_NoBridgeFilterMatchesAll(t *testing.T) {
	// Empty bridge disables filter — must find conflict on any NIC/bridge.
	deps := icDeps(
		func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			return ipListResp(ipVMResource(310, "any-bridge")), nil
		},
		func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{"name": "any-bridge", "ipconfig0": "ip=10.0.0.5/24", "net0": "virtio,bridge=vmbr99"}, nil
		},
	)
	conflict, err := detectIPConflict(context.Background(), deps, []string{"10.0.0.5"}, "", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if conflict == nil {
		t.Fatal("expected conflict when no bridge filter, got nil")
	}
}

func TestDetectIPConflict_ConfigFetchErrorSkipped(t *testing.T) {
	// Per-VM Config errors must be logged+skipped, not propagated.
	deps := icDeps(
		func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			return ipListResp(ipVMResource(500, "locked"), ipVMResource(501, "ok")), nil
		},
		func(_ context.Context, _ string, vmid int) (map[string]any, error) {
			if vmid == 500 {
				return nil, errors.New("lock file acquired")
			}
			return map[string]any{"name": "ok", "ipconfig0": "ip=10.0.0.20/24", "net0": "virtio,bridge=vmbr0"}, nil
		},
	)
	conflict, err := detectIPConflict(context.Background(), deps, []string{"10.0.0.5"}, "vmbr0", 0)
	if err != nil {
		t.Fatalf("config fetch error must not propagate: %v", err)
	}
	if conflict != nil {
		t.Fatalf("expected no conflict, got %+v", conflict)
	}
}

func TestDetectIPConflict_TransientListError(t *testing.T) {
	sentinel := errors.New("connection refused")
	deps := icDeps(func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
		return nil, sentinel
	}, nil)
	conflict, err := detectIPConflict(context.Background(), deps, []string{"10.0.0.5"}, "vmbr0", 0)
	if err == nil {
		t.Fatal("expected error from ListResources, got nil")
	}
	if conflict != nil {
		t.Fatalf("expected nil conflict on error, got %+v", conflict)
	}
}

func TestDetectIPConflict_MultipleTargetIPs(t *testing.T) {
	deps := icDeps(
		func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			return ipListResp(ipVMResource(700, "vm-700")), nil
		},
		func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{"name": "vm-700", "ipconfig0": "ip=10.0.0.10/24", "net0": "virtio,bridge=vmbr0"}, nil
		},
	)
	conflict, err := detectIPConflict(context.Background(), deps, []string{"10.0.0.1", "10.0.0.10", "10.0.0.20"}, "vmbr0", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if conflict == nil || conflict.IP != "10.0.0.10" {
		t.Fatalf("expected conflict at 10.0.0.10, got %v", conflict)
	}
}

func TestDetectIPConflict_SecondNICConflict(t *testing.T) {
	deps := icDeps(
		func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			return ipListResp(ipVMResource(900, "dual-nic")), nil
		},
		func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{
				"name": "dual-nic", "ipconfig0": "ip=10.0.0.10/24", "net0": "virtio,bridge=vmbr0",
				"ipconfig1": "ip=192.168.0.5/24", "net1": "virtio,bridge=vmbr0",
			}, nil
		},
	)
	conflict, err := detectIPConflict(context.Background(), deps, []string{"192.168.0.5"}, "vmbr0", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if conflict == nil || conflict.IP != "192.168.0.5" {
		t.Fatalf("expected conflict at 192.168.0.5, got %v", conflict)
	}
}

func TestDetectIPConflict_MalformedResourceRowSkipped(t *testing.T) {
	bad := json.RawMessage(`{"vmid":"not-an-int"}`)
	deps := icDeps(
		func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			return ipListResp(bad, ipVMResource(800, "good")), nil
		},
		func(_ context.Context, _ string, vmid int) (map[string]any, error) {
			if vmid == 800 {
				return map[string]any{"name": "good", "ipconfig0": "ip=10.0.0.50/24", "net0": "virtio,bridge=vmbr0"}, nil
			}
			return map[string]any{}, nil
		},
	)
	// 10.0.0.5 != 10.0.0.50 — no conflict; malformed row silently dropped.
	conflict, err := detectIPConflict(context.Background(), deps, []string{"10.0.0.5"}, "vmbr0", 0)
	if err != nil || conflict != nil {
		t.Fatalf("malformed row must be skipped: (%v,%v)", conflict, err)
	}
}

func TestDetectIPConflict_ContextCancellation(t *testing.T) {
	// Pre-cancelled context must not hang or panic.
	entries := make([]json.RawMessage, 20)
	for i := range entries {
		entries[i] = ipVMResource(1000+i, fmt.Sprintf("vm-%d", 1000+i))
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	deps := icDeps(
		func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			return ipListResp(entries...), nil
		},
		func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{"ipconfig0": "ip=10.0.0.100/24"}, nil
		},
	)
	_, _ = detectIPConflict(ctx, deps, []string{"10.0.0.100"}, "vmbr0", 0)
}

func TestDetectIPConflict_NilVMConfig(t *testing.T) {
	// Config returns nil map (not an error) — must be skipped safely.
	deps := icDeps(
		func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			return ipListResp(ipVMResource(1100, "nil-cfg-vm")), nil
		},
		func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return nil, nil // nil map, no error
		},
	)
	conflict, err := detectIPConflict(context.Background(), deps, []string{"10.0.0.5"}, "vmbr0", 0)
	if err != nil || conflict != nil {
		t.Fatalf("nil VM config must be skipped: (%v,%v)", conflict, err)
	}
}

// --------------------------------------------------------------------------
// Self-exclusion regression tests (the primary bug fix)
// --------------------------------------------------------------------------

// TestDetectIPConflict_SelfExclusion_NoConflict verifies that a VM whose vmid
// matches excludeVMID is skipped even when its ipconfig holds a target IP.
// This is the exact scenario that caused the self-conflict bug: the new VM's
// own ipconfig was seen by the scanner and incorrectly flagged as a conflict.
func TestDetectIPConflict_SelfExclusion_NoConflict(t *testing.T) {
	// VMID 9999 is the "just created" VM; it holds the target IP.
	const newVMID = 9999
	const targetIP = "10.50.0.250"

	deps := icDeps(
		func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			// The cluster list includes the new VM itself (as PVE would return it).
			return ipListResp(ipVMResource(newVMID, "new-vm")), nil
		},
		func(_ context.Context, _ string, vmid int) (map[string]any, error) {
			if vmid == newVMID {
				// New VM's ipconfig already reflects the assigned IP — this is
				// what configureNICs wrote before detectIPConflict was called.
				return map[string]any{
					"name":      "new-vm",
					"ipconfig0": "ip=" + targetIP + "/24,gw=10.50.0.1",
					"net0":      "virtio=de:ad:be:ef:00:01,bridge=vmbr0",
				}, nil
			}
			return map[string]any{}, nil
		},
	)

	// Pass excludeVMID = newVMID — the new VM must be skipped.
	conflict, err := detectIPConflict(context.Background(), deps, []string{targetIP}, "vmbr0", newVMID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if conflict != nil {
		t.Fatalf("self-exclusion: new VM must not conflict with itself, got conflict: %+v", conflict)
	}
}

// TestDetectIPConflict_SelfExclusion_ExternalConflictStillDetected verifies
// that when excludeVMID skips the new VM but a DIFFERENT VM also holds the
// same IP, that external conflict is still caught.
func TestDetectIPConflict_SelfExclusion_ExternalConflictStillDetected(t *testing.T) {
	const newVMID = 9998
	const externalVMID = 9997
	const targetIP = "10.50.0.100"

	deps := icDeps(
		func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			// Both the new VM and an external VM appear in the cluster list.
			return ipListResp(
				ipVMResource(newVMID, "new-vm"),
				ipVMResource(externalVMID, "external-vm"),
			), nil
		},
		func(_ context.Context, _ string, vmid int) (map[string]any, error) {
			switch vmid {
			case newVMID:
				// New VM has the target IP (should be excluded by excludeVMID).
				return map[string]any{
					"name":      "new-vm",
					"ipconfig0": "ip=" + targetIP + "/24,gw=10.50.0.1",
					"net0":      "virtio=de:ad:be:ef:00:01,bridge=vmbr0",
				}, nil
			case externalVMID:
				// External VM ALSO holds the target IP — genuine conflict.
				return map[string]any{
					"name":      "external-vm",
					"ipconfig0": "ip=" + targetIP + "/24,gw=10.50.0.1",
					"net0":      "virtio=aa:bb:cc:dd:ee:ff,bridge=vmbr0",
				}, nil
			}
			return map[string]any{}, nil
		},
	)

	// excludeVMID skips newVMID; externalVMID must still be detected.
	conflict, err := detectIPConflict(context.Background(), deps, []string{targetIP}, "vmbr0", newVMID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if conflict == nil {
		t.Fatal("external VM holding the same IP must still be detected as a conflict")
	}
	if conflict.VMID != externalVMID {
		t.Errorf("expected conflict from VMID %d, got VMID %d", externalVMID, conflict.VMID)
	}
	if conflict.IP != targetIP {
		t.Errorf("expected conflict IP %q, got %q", targetIP, conflict.IP)
	}
}
