// Package handlers internal tests for the per-NIC vlan and mtu
// cloud_properties keys — plain 802.1Q VLAN tagging and explicit MTU on an
// operator-managed bridge, with no SDN involvement.
package handlers

import (
	"context"
	"strings"
	"testing"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// vlanParsed builds a minimal parsed-args with one dynamic NIC on the named
// bridge and optional per-NIC vlan/mtu/model cloud_properties.
func vlanParsed(bridge string, vlan, mtu any, model string) *createVMParsedArgs {
	cp := map[string]any{nicCPKeyBridge: bridge}
	if vlan != nil {
		cp[nicCPKeyVLAN] = vlan
	}
	if mtu != nil {
		cp[nicCPKeyMTU] = mtu
	}
	if model != "" {
		cp[nicCPKeyModel] = model
	}
	return &createVMParsedArgs{
		networks: map[string]createVMNetworkSpec{
			"default": {Type: "dynamic", CloudProperties: cp},
		},
	}
}

// --------------------------------------------------------------------------
// per-NIC vlan key
// --------------------------------------------------------------------------

func TestConfigureNICs_VLAN_PerNIC_AppendsTag(t *testing.T) {
	cfg := icMinConfig()
	nd := &fwNodesStub{}
	deps := fwDeps(&fwClusterStub{}, nd, cfg)
	shape := &createVMShape{node: "pve1"}

	if _, err := configureNICs(context.Background(), deps, log.NewNopLogger(), vlanParsed("vmbr0", 100, nil, ""), shape, 100); err != nil {
		t.Fatalf("configureNICs: %v", err)
	}
	if !strings.Contains(nd.lastNet[0], ",tag=100") {
		t.Errorf("net0 = %q; want ,tag=100 from per-NIC vlan", nd.lastNet[0])
	}
}

func TestConfigureNICs_VLAN_Absent_NoTag(t *testing.T) {
	cfg := icMinConfig()
	nd := &fwNodesStub{}
	deps := fwDeps(&fwClusterStub{}, nd, cfg)
	shape := &createVMShape{node: "pve1"}

	if _, err := configureNICs(context.Background(), deps, log.NewNopLogger(), vlanParsed("vmbr0", nil, nil, ""), shape, 100); err != nil {
		t.Fatalf("configureNICs: %v", err)
	}
	if strings.Contains(nd.lastNet[0], "tag=") {
		t.Errorf("net0 = %q; must not carry tag= when vlan is unset", nd.lastNet[0])
	}
}

func TestConfigureNICs_VLAN_NetworkDefaultsOverridesPerNIC(t *testing.T) {
	cfg := icMinConfig()
	parsed := vlanParsed("vmbr0", 100, nil, "")
	parsed.cloudProps.NetworkDefaults = map[string]any{nicCPKeyVLAN: 200}
	nd := &fwNodesStub{}
	deps := fwDeps(&fwClusterStub{}, nd, cfg)
	shape := &createVMShape{node: "pve1"}

	if _, err := configureNICs(context.Background(), deps, log.NewNopLogger(), parsed, shape, 100); err != nil {
		t.Fatalf("configureNICs: %v", err)
	}
	if !strings.Contains(nd.lastNet[0], ",tag=200") {
		t.Errorf("net0 = %q; network_defaults.vlan (200) must beat per-NIC vlan (100)", nd.lastNet[0])
	}
}

func TestConfigureNICs_VLAN_FloatFromJSON_Coerced(t *testing.T) {
	// JSON-RPC-decoded cloud_properties carry numbers as float64 — the
	// resolver must accept that shape, not just Go int literals.
	cfg := icMinConfig()
	nd := &fwNodesStub{}
	deps := fwDeps(&fwClusterStub{}, nd, cfg)
	shape := &createVMShape{node: "pve1"}

	if _, err := configureNICs(context.Background(), deps, log.NewNopLogger(), vlanParsed("vmbr0", float64(300), nil, ""), shape, 100); err != nil {
		t.Fatalf("configureNICs: %v", err)
	}
	if !strings.Contains(nd.lastNet[0], ",tag=300") {
		t.Errorf("net0 = %q; want ,tag=300 from a float64-shaped vlan value", nd.lastNet[0])
	}
}

func TestConfigureNICs_VLAN_OutOfRange_Rejected(t *testing.T) {
	cases := []int{0, -1, 4095, 100000}
	for _, v := range cases {
		v := v
		t.Run("", func(t *testing.T) {
			cfg := icMinConfig()
			deps := fwDeps(&fwClusterStub{}, &fwNodesStub{}, cfg)
			shape := &createVMShape{node: "pve1"}

			_, err := configureNICs(context.Background(), deps, log.NewNopLogger(), vlanParsed("vmbr0", v, nil, ""), shape, 100)
			if v == 0 {
				// vlan=0 is indistinguishable from "unset" by design (0 is the
				// zero value) — no error, no tag emitted.
				if err != nil {
					t.Fatalf("vlan=0 must not error (treated as absent): %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("vlan=%d must be rejected (outside 1..4094)", v)
			}
			if !cpierrors.IsType(err, cpierrors.TypeCloud) {
				t.Errorf("expected non-retriable CloudError, got %T: %v", err, err)
			}
			if !strings.Contains(err.Error(), "default") {
				t.Errorf("error must name the offending network %q; got: %v", "default", err)
			}
		})
	}
}

// TestConfigureNICs_VLAN_PerNIC_Malformed_Rejected verifies that a vlan key
// PRESENT with a non-integer value (null, bool, array, unparseable string) is
// rejected with a Cloud error naming the network and key, rather than being
// silently treated as absent (which would attach the NIC untagged on the
// bridge's native/management VLAN with no indication anything was wrong).
func TestConfigureNICs_VLAN_PerNIC_Malformed_Rejected(t *testing.T) {
	cases := []struct {
		name string
		val  any
	}{
		{"null", nil},
		{"bool", true},
		{"array", []any{100}},
		{"unparseable string", "not-a-number"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			cfg := icMinConfig()
			deps := fwDeps(&fwClusterStub{}, &fwNodesStub{}, cfg)
			shape := &createVMShape{node: "pve1"}
			parsed := &createVMParsedArgs{
				networks: map[string]createVMNetworkSpec{
					"default": {Type: "dynamic", CloudProperties: map[string]any{
						nicCPKeyBridge: "vmbr0",
						nicCPKeyVLAN:   tc.val,
					}},
				},
			}

			_, err := configureNICs(context.Background(), deps, log.NewNopLogger(), parsed, shape, 100)
			if err == nil {
				t.Fatalf("vlan=%v (present, malformed) must be rejected, not silently dropped", tc.val)
			}
			if !cpierrors.IsType(err, cpierrors.TypeCloud) {
				t.Errorf("expected non-retriable CloudError, got %T: %v", err, err)
			}
			for _, want := range []string{"default", "vlan"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q must name the network and key %q", err.Error(), want)
				}
			}
		})
	}
}

// TestConfigureNICs_VLAN_NetworkDefaults_Malformed_Rejected is the
// network_defaults counterpart of TestConfigureNICs_VLAN_PerNIC_Malformed_Rejected.
func TestConfigureNICs_VLAN_NetworkDefaults_Malformed_Rejected(t *testing.T) {
	cfg := icMinConfig()
	parsed := vlanParsed("vmbr0", nil, nil, "")
	parsed.cloudProps.NetworkDefaults = map[string]any{nicCPKeyVLAN: []any{100}}
	deps := fwDeps(&fwClusterStub{}, &fwNodesStub{}, cfg)
	shape := &createVMShape{node: "pve1"}

	_, err := configureNICs(context.Background(), deps, log.NewNopLogger(), parsed, shape, 100)
	if err == nil {
		t.Fatal("network_defaults.vlan malformed must be rejected, not silently dropped")
	}
	if !cpierrors.IsType(err, cpierrors.TypeCloud) {
		t.Errorf("expected non-retriable CloudError, got %T: %v", err, err)
	}
	for _, want := range []string{"default", "vlan"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q must name the network and key %q", err.Error(), want)
		}
	}
}

// --------------------------------------------------------------------------
// per-NIC mtu key
// --------------------------------------------------------------------------

func TestConfigureNICs_MTU_PerNIC_ExplicitInherit(t *testing.T) {
	cfg := icMinConfig()
	nd := &fwNodesStub{}
	deps := fwDeps(&fwClusterStub{}, nd, cfg)
	shape := &createVMShape{node: "pve1"}

	if _, err := configureNICs(context.Background(), deps, log.NewNopLogger(), vlanParsed("vmbr0", nil, 1, ""), shape, 100); err != nil {
		t.Fatalf("configureNICs: %v", err)
	}
	if !strings.Contains(nd.lastNet[0], ",mtu=1") {
		t.Errorf("net0 = %q; want ,mtu=1 (explicit inherit) on a plain bridge with no vnet involved", nd.lastNet[0])
	}
}

func TestConfigureNICs_MTU_PerNIC_JumboExplicit(t *testing.T) {
	cfg := icMinConfig()
	nd := &fwNodesStub{}
	deps := fwDeps(&fwClusterStub{}, nd, cfg)
	shape := &createVMShape{node: "pve1"}

	if _, err := configureNICs(context.Background(), deps, log.NewNopLogger(), vlanParsed("vmbr0", nil, 9000, ""), shape, 100); err != nil {
		t.Fatalf("configureNICs: %v", err)
	}
	if !strings.Contains(nd.lastNet[0], ",mtu=9000") {
		t.Errorf("net0 = %q; want ,mtu=9000 (explicit jumbo)", nd.lastNet[0])
	}
}

func TestConfigureNICs_MTU_PerNIC_OutOfRange_Rejected(t *testing.T) {
	cases := []int{2, 575, 65521, 100000}
	for _, v := range cases {
		v := v
		t.Run("", func(t *testing.T) {
			cfg := icMinConfig()
			deps := fwDeps(&fwClusterStub{}, &fwNodesStub{}, cfg)
			shape := &createVMShape{node: "pve1"}

			_, err := configureNICs(context.Background(), deps, log.NewNopLogger(), vlanParsed("vmbr0", nil, v, ""), shape, 100)
			if err == nil {
				t.Fatalf("mtu=%d must be rejected (neither 1 nor within 576..65520)", v)
			}
			if !cpierrors.IsType(err, cpierrors.TypeCloud) {
				t.Errorf("expected non-retriable CloudError, got %T: %v", err, err)
			}
		})
	}
}

func TestConfigureNICs_MTU_PerNIC_NonVirtioModel_Rejected(t *testing.T) {
	cfg := icMinConfig()
	deps := fwDeps(&fwClusterStub{}, &fwNodesStub{}, cfg)
	shape := &createVMShape{node: "pve1"}

	_, err := configureNICs(context.Background(), deps, log.NewNopLogger(), vlanParsed("vmbr0", nil, 9000, "e1000"), shape, 100)
	if err == nil {
		t.Fatal("mtu on a non-virtio model must be rejected (PVE rejects the option)")
	}
	for _, want := range []string{"default", "e1000"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q must name the NIC and model %q", err.Error(), want)
		}
	}
}

func TestConfigureNICs_MTU_ExplicitWinsOverVnetInheritance(t *testing.T) {
	// bug/interaction: an explicit per-NIC mtu must win over the
	// automatic vnet-derived mtu=1 inheritance — never emit both mtu=
	// segments.
	cfg := icMinConfig()
	cfg.NetworkMode = networkModeSDN
	cl := &fwClusterStub{sdnVnets: []string{"boshvnet"}}
	nd := &fwNodesStub{}
	deps := fwDeps(cl, nd, cfg)
	shape := &createVMShape{node: "pve1"}

	if _, err := configureNICs(context.Background(), deps, log.NewNopLogger(), vlanParsed("boshvnet", nil, 9000, ""), shape, 100); err != nil {
		t.Fatalf("configureNICs: %v", err)
	}
	got := nd.lastNet[0]
	if !strings.Contains(got, ",mtu=9000") {
		t.Errorf("net0 = %q; explicit mtu=9000 must win over vnet mtu=1 inheritance", got)
	}
	if strings.Count(got, "mtu=") != 1 {
		t.Errorf("net0 = %q; must carry exactly one mtu= segment", got)
	}
}

func TestConfigureNICs_MTU_NetworkDefaultsOverridesPerNIC(t *testing.T) {
	cfg := icMinConfig()
	parsed := vlanParsed("vmbr0", nil, 1400, "")
	parsed.cloudProps.NetworkDefaults = map[string]any{nicCPKeyMTU: 9000}
	nd := &fwNodesStub{}
	deps := fwDeps(&fwClusterStub{}, nd, cfg)
	shape := &createVMShape{node: "pve1"}

	if _, err := configureNICs(context.Background(), deps, log.NewNopLogger(), parsed, shape, 100); err != nil {
		t.Fatalf("configureNICs: %v", err)
	}
	if !strings.Contains(nd.lastNet[0], ",mtu=9000") {
		t.Errorf("net0 = %q; network_defaults.mtu (9000) must beat per-NIC mtu (1400)", nd.lastNet[0])
	}
}

// --------------------------------------------------------------------------
// IP-conflict pre-check resolves (bridge, vlan) via the SAME
// precedence configureNICs uses.
// --------------------------------------------------------------------------

// TestCollectStaticIPsForConflictCheck_NetworkDefaultsBridgeVisible is the
// direct regression test: a VM-level network_defaults.bridge
// override was previously invisible to the IP-conflict pre-check, so the
// duplicate-IP guard silently scanned the wrong bridge (or none) and never
// caught a real conflict.
func TestCollectStaticIPsForConflictCheck_NetworkDefaultsBridgeVisible(t *testing.T) {
	cfg := icMinConfig()
	cfg.NetworkBridge = "vmbr0"

	parsed := &createVMParsedArgs{
		cloudProps: createVMCloudProps{
			NetworkDefaults: map[string]any{nicCPKeyBridge: "vmbrX"},
		},
		networks: map[string]createVMNetworkSpec{
			"default": {
				Type:            "manual",
				IP:              "10.0.0.5",
				Netmask:         "255.255.255.0",
				CloudProperties: map[string]any{nicCPKeyBridge: "vmbrY"},
			},
		},
	}

	result := collectStaticIPsForConflictCheck(parsed, cfg)
	domain := nicL2Domain{bridge: "vmbrX", vlan: 0}
	ips, ok := result[domain]
	if !ok {
		t.Fatalf("expected domain %+v (network_defaults.bridge) in result, got %+v", domain, result)
	}
	if len(ips) != 1 || ips[0] != "10.0.0.5" {
		t.Errorf("expected [10.0.0.5] under %+v, got %v", domain, ips)
	}
	if _, wrongDomain := result[nicL2Domain{bridge: "vmbrY", vlan: 0}]; wrongDomain {
		t.Error("must not group under the per-NIC spec bridge when network_defaults overrides it")
	}
}

// TestCollectStaticIPsForConflictCheck_VLANGroupsSeparateDomains is the
// direct regression test at the pre-check layer: two static IPs
// on the same bridge but different VLANs must land in separate map entries
// so a later same-bridge, different-vlan reuse of an address is not flagged.
func TestCollectStaticIPsForConflictCheck_VLANGroupsSeparateDomains(t *testing.T) {
	cfg := icMinConfig()
	cfg.NetworkBridge = "vmbr0"

	parsed := &createVMParsedArgs{
		networks: map[string]createVMNetworkSpec{
			"a": {
				Type: "manual", IP: "10.0.0.5", Netmask: "255.255.255.0",
				CloudProperties: map[string]any{nicCPKeyBridge: "vmbr0", nicCPKeyVLAN: 10},
			},
			"b": {
				Type: "manual", IP: "10.0.0.5", Netmask: "255.255.255.0",
				CloudProperties: map[string]any{nicCPKeyBridge: "vmbr0", nicCPKeyVLAN: 20},
			},
		},
	}

	result := collectStaticIPsForConflictCheck(parsed, cfg)
	if len(result) != 2 {
		t.Fatalf("expected 2 separate (bridge,vlan) domains, got %d: %+v", len(result), result)
	}
	for _, vlan := range []int{10, 20} {
		domain := nicL2Domain{bridge: "vmbr0", vlan: vlan}
		if ips, ok := result[domain]; !ok || len(ips) != 1 || ips[0] != "10.0.0.5" {
			t.Errorf("expected [10.0.0.5] under %+v, got %v (present=%v)", domain, ips, ok)
		}
	}
}
