// Package handlers internal tests for §7.34 network_defaults VM-level override.
//
// Precedence (highest first):
//
//	VM cloud_properties.network_defaults[key]
//	  > per-NIC spec.CloudProperties[key]
//	  > resolver default (call struct / profile / config / const)
//
// Keys covered: bridge, model, firewall.
// Absent network_defaults or absent key → unchanged (byte-identical to pre-change).
package handlers

import (
	"context"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
)

// --------------------------------------------------------------------------
// helper — build a minimal parsed args with one NIC
// --------------------------------------------------------------------------

func netDefaultsParsed(cpNetDefaults map[string]any, nicCP map[string]any) *createVMParsedArgs {
	cp := createVMCloudProps{}
	if cpNetDefaults != nil {
		cp.NetworkDefaults = cpNetDefaults
	}
	return &createVMParsedArgs{
		cloudProps: cp,
		networks: map[string]createVMNetworkSpec{
			"default": {Type: "dynamic", CloudProperties: nicCP},
		},
	}
}

// --------------------------------------------------------------------------
// §7.34 (a) — network_defaults.bridge overrides per-NIC spec bridge
// --------------------------------------------------------------------------

func TestNetworkDefaults_BridgeOverridesNICSpec(t *testing.T) {
	cfg := icMinConfig()
	cfg.NetworkBridge = "vmbr0"

	// NIC spec says vmbrY; network_defaults says vmbrX — vmbrX must win.
	parsed := netDefaultsParsed(
		map[string]any{nicCPKeyBridge: "vmbrX"},
		map[string]any{nicCPKeyBridge: "vmbrY"},
	)
	nd := &fwNodesStub{}
	deps := fwDeps(&fwClusterStub{}, nd, cfg)
	shape := &createVMShape{node: "pve1"}

	if _, err := configureNICs(context.Background(), deps, log.NewNopLogger(), parsed, shape, 100); err != nil {
		t.Fatalf("configureNICs: %v", err)
	}
	got := nd.lastNet[0]
	if !strings.Contains(got, "bridge=vmbrX") {
		t.Errorf("net0 = %q; want bridge=vmbrX (network_defaults must beat per-NIC spec)", got)
	}
	if strings.Contains(got, "bridge=vmbrY") {
		t.Errorf("net0 = %q; per-NIC bridge=vmbrY must be suppressed by network_defaults", got)
	}
}

// --------------------------------------------------------------------------
// §7.34 (b) — absent network_defaults → byte-identical to current behavior
// --------------------------------------------------------------------------

func TestNetworkDefaults_AbsentIsIdentical(t *testing.T) {
	cfg := icMinConfig()
	cfg.NetworkBridge = "vmbr0"

	// Baseline: no network_defaults, NIC uses spec bridge vmbr1.
	parsed := netDefaultsParsed(
		nil,
		map[string]any{nicCPKeyBridge: "vmbr1"},
	)
	nd := &fwNodesStub{}
	deps := fwDeps(&fwClusterStub{}, nd, cfg)
	shape := &createVMShape{node: "pve1"}

	if _, err := configureNICs(context.Background(), deps, log.NewNopLogger(), parsed, shape, 100); err != nil {
		t.Fatalf("configureNICs: %v", err)
	}
	got := nd.lastNet[0]
	// virtio is the default model; vmbr1 comes from per-NIC spec.
	want := "virtio,bridge=vmbr1"
	if got != want {
		t.Errorf("net0 = %q; want %q (absent network_defaults must be byte-identical)", got, want)
	}
}

// --------------------------------------------------------------------------
// §7.34 (c) — network_defaults.firewall=true forces firewall on NIC
//
//	network_defaults.firewall=false forces off even if spec set true (VM wins)
//
// --------------------------------------------------------------------------

func TestNetworkDefaults_FirewallTrueOverridesNICFalse(t *testing.T) {
	cfg := icMinConfig()
	// Global firewall off (nil VMFirewall).

	// NIC spec does NOT set firewall; network_defaults forces it on.
	parsed := netDefaultsParsed(
		map[string]any{nicCPKeyFirewall: true},
		map[string]any{},
	)
	nd := &fwNodesStub{}
	deps := fwDeps(&fwClusterStub{}, nd, cfg)
	shape := &createVMShape{node: "pve1"}

	if _, err := configureNICs(context.Background(), deps, log.NewNopLogger(), parsed, shape, 100); err != nil {
		t.Fatalf("configureNICs: %v", err)
	}
	if !strings.Contains(nd.lastNet[0], ",firewall=1") {
		t.Errorf("net0 = %q; want ,firewall=1 from network_defaults", nd.lastNet[0])
	}
}

func TestNetworkDefaults_FirewallFalseOverridesNICTrue(t *testing.T) {
	cfg := icMinConfig()
	v := true
	cfg.VMFirewall = &v // global on

	// NIC spec explicitly enables firewall; network_defaults forces it off.
	parsed := netDefaultsParsed(
		map[string]any{nicCPKeyFirewall: false},
		map[string]any{nicCPKeyFirewall: true},
	)
	nd := &fwNodesStub{}
	deps := fwDeps(&fwClusterStub{}, nd, cfg)
	shape := &createVMShape{node: "pve1"}

	if _, err := configureNICs(context.Background(), deps, log.NewNopLogger(), parsed, shape, 100); err != nil {
		t.Fatalf("configureNICs: %v", err)
	}
	if strings.Contains(nd.lastNet[0], ",firewall=1") {
		t.Errorf("net0 = %q; network_defaults firewall=false must beat per-NIC true", nd.lastNet[0])
	}
}

// --------------------------------------------------------------------------
// §7.34 (d) — network_defaults.model overrides per-NIC spec model
// --------------------------------------------------------------------------

func TestNetworkDefaults_ModelOverridesNICSpec(t *testing.T) {
	cfg := icMinConfig()

	parsed := netDefaultsParsed(
		map[string]any{nicCPKeyModel: "e1000"},
		map[string]any{nicCPKeyModel: "rtl8139"},
	)
	nd := &fwNodesStub{}
	deps := fwDeps(&fwClusterStub{}, nd, cfg)
	shape := &createVMShape{node: "pve1"}

	if _, err := configureNICs(context.Background(), deps, log.NewNopLogger(), parsed, shape, 100); err != nil {
		t.Fatalf("configureNICs: %v", err)
	}
	got := nd.lastNet[0]
	if !strings.HasPrefix(got, "e1000,") {
		t.Errorf("net0 = %q; want e1000 model from network_defaults", got)
	}
	if strings.HasPrefix(got, "rtl8139,") {
		t.Errorf("net0 = %q; per-NIC rtl8139 must be suppressed by network_defaults", got)
	}
}

// --------------------------------------------------------------------------
// §7.34 (e) — partial network_defaults: only bridge set, model/firewall unchanged
// --------------------------------------------------------------------------

func TestNetworkDefaults_PartialOnlyBridge(t *testing.T) {
	cfg := icMinConfig()
	cfg.NetworkBridge = "vmbr0"

	// NIC spec sets model=e1000; network_defaults only overrides bridge.
	// Result: model from NIC spec, bridge from network_defaults.
	parsed := netDefaultsParsed(
		map[string]any{nicCPKeyBridge: "vmbr99"},
		map[string]any{nicCPKeyModel: "e1000"},
	)
	nd := &fwNodesStub{}
	deps := fwDeps(&fwClusterStub{}, nd, cfg)
	shape := &createVMShape{node: "pve1"}

	if _, err := configureNICs(context.Background(), deps, log.NewNopLogger(), parsed, shape, 100); err != nil {
		t.Fatalf("configureNICs: %v", err)
	}
	got := nd.lastNet[0]
	if !strings.HasPrefix(got, "e1000,") {
		t.Errorf("net0 = %q; want e1000 model from per-NIC spec (network_defaults did not set model)", got)
	}
	if !strings.Contains(got, "bridge=vmbr99") {
		t.Errorf("net0 = %q; want bridge=vmbr99 from network_defaults", got)
	}
}

// --------------------------------------------------------------------------
// §7.34 unknown keys in network_defaults — must be ignored gracefully
// --------------------------------------------------------------------------

func TestNetworkDefaults_UnknownKeysIgnored(t *testing.T) {
	cfg := icMinConfig()

	parsed := netDefaultsParsed(
		map[string]any{nicCPKeyBridge: "vmbr5", "mtu": 9000, "unknown_future_key": "value"},
		map[string]any{},
	)
	nd := &fwNodesStub{}
	deps := fwDeps(&fwClusterStub{}, nd, cfg)
	shape := &createVMShape{node: "pve1"}

	// Must not error on unknown keys.
	if _, err := configureNICs(context.Background(), deps, log.NewNopLogger(), parsed, shape, 100); err != nil {
		t.Fatalf("configureNICs with unknown network_defaults keys: %v", err)
	}
	got := nd.lastNet[0]
	if !strings.Contains(got, "bridge=vmbr5") {
		t.Errorf("net0 = %q; known bridge key must still apply despite unknown siblings", got)
	}
}

// --------------------------------------------------------------------------
// §7.34 multi-NIC: network_defaults applies to ALL NICs uniformly
// --------------------------------------------------------------------------

func TestNetworkDefaults_AppliesToAllNICs(t *testing.T) {
	cfg := icMinConfig()
	cfg.NetworkBridge = "vmbr0"

	cp := createVMCloudProps{
		NetworkDefaults: map[string]any{nicCPKeyBridge: "vmbrShared"},
	}
	parsed := &createVMParsedArgs{
		cloudProps: cp,
		networks: map[string]createVMNetworkSpec{
			"default": {Type: "dynamic", CloudProperties: map[string]any{}},
			"storage": {Type: "dynamic", CloudProperties: map[string]any{nicCPKeyBridge: "vmbrStorage"}},
		},
	}
	nd := &fwNodesStub{}
	deps := fwDeps(&fwClusterStub{}, nd, cfg)
	shape := &createVMShape{node: "pve1"}

	if _, err := configureNICs(context.Background(), deps, log.NewNopLogger(), parsed, shape, 100); err != nil {
		t.Fatalf("configureNICs: %v", err)
	}
	// Both NICs should use vmbrShared (network_defaults wins over per-NIC spec).
	for idx, netStr := range nd.lastNet {
		if !strings.Contains(netStr, "bridge=vmbrShared") {
			t.Errorf("net%d = %q; want bridge=vmbrShared from network_defaults", idx, netStr)
		}
	}
}
