package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
)

// --------------------------------------------------------------------------
// planNICs — how networks map onto net{N} slots
// --------------------------------------------------------------------------

func TestPlanNICs_NoGroupsIsOneNICPerNetwork(t *testing.T) {
	networks := map[string]createVMNetworkSpec{
		"default": {Type: nicTypeManual},
		"alpha":   {Type: nicTypeManual},
		"zulu":    {Type: nicTypeManual},
	}
	plan := planNICs(networks)

	// sortedNetworkNames puts `default` first, then alphabetical. Without
	// nic_group each network keeps its own slot, so the mapping is exactly
	// what the CPI produced before nic_group existed.
	want := [][]string{{"default"}, {"alpha"}, {"zulu"}}
	if len(plan) != len(want) {
		t.Fatalf("plan has %d NICs, want %d: %+v", len(plan), len(want), plan)
	}
	for i, names := range want {
		if plan[i].index != i {
			t.Errorf("plan[%d].index = %d; want %d", i, plan[i].index, i)
		}
		if strings.Join(plan[i].names, ",") != strings.Join(names, ",") {
			t.Errorf("plan[%d].names = %v; want %v", i, plan[i].names, names)
		}
	}
}

func TestPlanNICs_SharedGroupCollapsesToOneNIC(t *testing.T) {
	networks := map[string]createVMNetworkSpec{
		"default": {Type: nicTypeManual, NicGroup: "nic0"},
		"ipv6":    {Type: nicTypeManual, NicGroup: "nic0"},
		"second":  {Type: nicTypeManual, NicGroup: "nic1"},
	}
	plan := planNICs(networks)

	if len(plan) != 2 {
		t.Fatalf("plan has %d NICs, want 2: %+v", len(plan), plan)
	}
	// The group takes the slot of its first member in sortedNetworkNames
	// order, so `default`+`ipv6` are net0 and `second` is net1.
	if got := strings.Join(plan[0].names, ","); got != "default,ipv6" {
		t.Errorf("plan[0].names = %v; want [default ipv6]", plan[0].names)
	}
	if got := strings.Join(plan[1].names, ","); got != "second" {
		t.Errorf("plan[1].names = %v; want [second]", plan[1].names)
	}
	if plan[1].index != 1 {
		t.Errorf("plan[1].index = %d; want 1", plan[1].index)
	}
}

func TestPlanNICs_BlankGroupIsNotAGroup(t *testing.T) {
	// Whitespace-only nic_group must not silently join two networks onto one
	// NIC — an empty value means "no group".
	networks := map[string]createVMNetworkSpec{
		"alpha": {Type: nicTypeManual, NicGroup: "  "},
		"bravo": {Type: nicTypeManual, NicGroup: ""},
	}
	plan := planNICs(networks)
	if len(plan) != 2 {
		t.Fatalf("plan has %d NICs, want 2 (blank nic_group must not group): %+v", len(plan), plan)
	}
}

func TestPlanNetworkNames_CoversEveryNetworkOnce(t *testing.T) {
	networks := map[string]createVMNetworkSpec{
		"default": {NicGroup: "g"},
		"ipv6":    {NicGroup: "g"},
		"second":  {},
	}
	names := planNetworkNames(planNICs(networks))
	if len(names) != 3 {
		t.Fatalf("planNetworkNames = %v; want 3 entries", names)
	}
	seen := map[string]int{}
	for _, n := range names {
		seen[n]++
	}
	for name := range networks {
		if seen[name] != 1 {
			t.Errorf("network %q appears %d times in %v; want exactly 1", name, seen[name], names)
		}
	}
}

// --------------------------------------------------------------------------
// netmaskToCIDR — the director sends a family-shaped mask, not a prefix
// --------------------------------------------------------------------------

func TestNetmaskToCIDR(t *testing.T) {
	cases := []struct {
		name    string
		netmask string
		v6      bool
		want    int
	}{
		{"ipv4 /24", "255.255.255.0", false, 24},
		{"ipv4 /16", "255.255.0.0", false, 16},
		{"ipv4 /0", "0.0.0.0", false, 0},
		{"ipv4 empty falls back to host length", "", false, 32},
		{"ipv4 garbage falls back to host length", "not-a-mask", false, 32},
		{"ipv4 wrong shape falls back", "255.255.255", false, 32},
		{"ipv4 out-of-range octet falls back", "255.300.0.0", false, 32},
		{"ipv4 non-contiguous falls back", "255.0.255.0", false, 32},
		// What bosh-director actually sends for an IPv6 subnet: IPAddr#netmask
		// expands every group.
		{"ipv6 /64 expanded", "ffff:ffff:ffff:ffff:0000:0000:0000:0000", true, 64},
		{"ipv6 /64 compressed", "ffff:ffff:ffff:ffff::", true, 64},
		{"ipv6 /48", "ffff:ffff:ffff::", true, 48},
		{"ipv6 /128", "ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff", true, 128},
		{"ipv6 empty falls back to host length", "", true, 128},
		{"ipv6 garbage falls back to host length", "zzz", true, 128},
		{"bare prefix ipv4", "24", false, 24},
		{"bare prefix ipv6", "64", true, 64},
		{"bare prefix out of range for family", "64", false, 32},
		{"bare prefix negative", "-1", false, 32},
		// A dotted-decimal mask paired with an IPv6 address is a config error;
		// the host length is the safe reading, not a silent /24.
		{"ipv4 mask on ipv6 address", "255.255.255.0", true, 128},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if got := netmaskToCIDR(tc.netmask, tc.v6); got != tc.want {
				t.Errorf("netmaskToCIDR(%q, v6=%v) = %d; want %d", tc.netmask, tc.v6, got, tc.want)
			}
		})
	}
}

func TestIsIPv6Address(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"10.0.0.5", false},
		{"fd36:afd4:2c42:1::c8", true},
		{"::1", true},
		// An IPv4-mapped address is IPv4 for addressing purposes.
		{"::ffff:10.0.0.5", false},
		{"dhcp", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := isIPv6Address(tc.in); got != tc.want {
			t.Errorf("isIPv6Address(%q) = %v; want %v", tc.in, got, tc.want)
		}
	}
}

func TestIPToCIDR_UsesFamilyAwarePrefix(t *testing.T) {
	if got := ipToCIDR("10.0.0.5", "255.255.0.0", "", nil, "default"); got != "10.0.0.5/16" {
		t.Errorf("ipToCIDR ipv4 = %q; want 10.0.0.5/16", got)
	}
	got := ipToCIDR("fd36:afd4:2c42:1::c8", "ffff:ffff:ffff:ffff:0000:0000:0000:0000", "", nil, "ipv6")
	if got != "fd36:afd4:2c42:1::c8/64" {
		t.Errorf("ipToCIDR ipv6 = %q; want fd36:afd4:2c42:1::c8/64", got)
	}
}

// A director that sends no netmask must not leave an IPv6 address on a /128
// host route with an off-link gateway — the subnet range carries the same
// prefix length and is used instead.
func TestIPToCIDR_FallsBackToRangeWhenNetmaskUnusable(t *testing.T) {
	cases := []struct {
		name    string
		ip      string
		netmask string
		rng     string
		want    string
	}{
		{"ipv6 empty netmask", "fd36:afd4:2c42:1::c8", "", "fd36:afd4:2c42:1::/64", "fd36:afd4:2c42:1::c8/64"},
		{"ipv4 empty netmask", "10.254.48.190", "", "10.254.0.0/16", "10.254.48.190/16"},
		{"unparseable netmask", "10.254.48.190", "not-a-mask", "10.254.0.0/16", "10.254.48.190/16"},
		{"netmask wins over range", "10.254.48.190", "255.255.255.0", "10.254.0.0/16", "10.254.48.190/24"},
		{"family mismatch falls through to host route", "fd36:afd4:2c42:1::c8", "", "10.254.0.0/16", "fd36:afd4:2c42:1::c8/128"},
		{"no netmask and no range", "10.254.48.190", "", "", "10.254.48.190/32"},
		{"unparseable range", "10.254.48.190", "", "not-a-cidr", "10.254.48.190/32"},
	}
	for _, tc := range cases {
		if got := ipToCIDR(tc.ip, tc.netmask, tc.rng, nil, "n"); got != tc.want {
			t.Errorf("%s: ipToCIDR(%q, %q, %q) = %q; want %q", tc.name, tc.ip, tc.netmask, tc.rng, got, tc.want)
		}
	}
}

// --------------------------------------------------------------------------
// buildIPConfig — one entry per NIC, both families folded together
// --------------------------------------------------------------------------

func buildIPConfigFor(t *testing.T, networks map[string]createVMNetworkSpec) (string, error) {
	t.Helper()
	plan := planNICs(networks)
	if len(plan) != 1 {
		t.Fatalf("expected the fixture to produce exactly 1 NIC, got %d: %+v", len(plan), plan)
	}
	return buildIPConfig(plan[0], networks, log.NewNopLogger())
}

func TestBuildIPConfig_IPv4Only(t *testing.T) {
	got, err := buildIPConfigFor(t, map[string]createVMNetworkSpec{
		"default": {Type: nicTypeManual, IP: "10.0.0.5", Netmask: "255.255.255.0", Gateway: "10.0.0.1"},
	})
	if err != nil {
		t.Fatalf("buildIPConfig: %v", err)
	}
	if got != "ip=10.0.0.5/24,gw=10.0.0.1" {
		t.Errorf("ipconfig = %q; want ip=10.0.0.5/24,gw=10.0.0.1", got)
	}
}

func TestBuildIPConfig_IPv6OnlyUsesIP6Keys(t *testing.T) {
	got, err := buildIPConfigFor(t, map[string]createVMNetworkSpec{
		"default": {
			Type:    nicTypeManual,
			IP:      "fd36:afd4:2c42:1::c8",
			Netmask: "ffff:ffff:ffff:ffff:0000:0000:0000:0000",
			Gateway: "fd36:afd4:2c42:1::1",
		},
	})
	if err != nil {
		t.Fatalf("buildIPConfig: %v", err)
	}
	want := "ip6=fd36:afd4:2c42:1::c8/64,gw6=fd36:afd4:2c42:1::1"
	if got != want {
		t.Errorf("ipconfig = %q; want %q", got, want)
	}
	// PVE rejects an IPv6 address under the IPv4 keys; make the absence explicit.
	if strings.Contains(got, "ip=") || strings.Contains(got, ",gw=") {
		t.Errorf("ipconfig = %q; an IPv6 address must not be emitted under ip=/gw=", got)
	}
}

func TestBuildIPConfig_DualStackOnOneNIC(t *testing.T) {
	got, err := buildIPConfigFor(t, map[string]createVMNetworkSpec{
		"default": {
			Type: nicTypeManual, NicGroup: "nic0",
			IP: "10.254.48.190", Netmask: "255.255.0.0", Gateway: "10.254.0.1",
		},
		"ipv6": {
			Type: nicTypeManual, NicGroup: "nic0",
			IP:      "fd36:afd4:2c42:1::48:190",
			Netmask: "ffff:ffff:ffff:ffff:0000:0000:0000:0000",
			Gateway: "fd36:afd4:2c42:1::1",
		},
	})
	if err != nil {
		t.Fatalf("buildIPConfig: %v", err)
	}
	want := "ip=10.254.48.190/16,gw=10.254.0.1," +
		"ip6=fd36:afd4:2c42:1::48:190/64,gw6=fd36:afd4:2c42:1::1"
	if got != want {
		t.Errorf("ipconfig = %q;\nwant %q", got, want)
	}
}

func TestBuildIPConfig_IPv4OrderIsStableRegardlessOfMemberOrder(t *testing.T) {
	// `ipv6` sorts before `zzz`, so the IPv6 member is visited first here.
	// PVE accepts either order, but a stable rendering keeps the config free
	// of spurious diffs on recreate.
	got, err := buildIPConfigFor(t, map[string]createVMNetworkSpec{
		"ipv6": {
			Type: nicTypeManual, NicGroup: "g",
			IP: "fd00::5", Netmask: "ffff:ffff:ffff:ffff::", Gateway: "fd00::1",
		},
		"zzz": {
			Type: nicTypeManual, NicGroup: "g",
			IP: "10.0.0.5", Netmask: "255.255.255.0", Gateway: "10.0.0.1",
		},
	})
	if err != nil {
		t.Fatalf("buildIPConfig: %v", err)
	}
	if !strings.HasPrefix(got, "ip=10.0.0.5/24,gw=10.0.0.1,ip6=") {
		t.Errorf("ipconfig = %q; want the IPv4 pair rendered first", got)
	}
}

func TestBuildIPConfig_DynamicIsDHCP(t *testing.T) {
	got, err := buildIPConfigFor(t, map[string]createVMNetworkSpec{
		"default": {Type: nicTypeDynamic},
	})
	if err != nil {
		t.Fatalf("buildIPConfig: %v", err)
	}
	if got != "ip=dhcp" {
		t.Errorf("ipconfig = %q; want ip=dhcp", got)
	}
}

func TestBuildIPConfig_DHCPv4PlusStaticV6(t *testing.T) {
	got, err := buildIPConfigFor(t, map[string]createVMNetworkSpec{
		"default": {Type: nicTypeDynamic, NicGroup: "g"},
		"ipv6": {
			Type: nicTypeManual, NicGroup: "g",
			IP: "fd00::5", Netmask: "ffff:ffff:ffff:ffff::", Gateway: "fd00::1",
		},
	})
	if err != nil {
		t.Fatalf("buildIPConfig: %v", err)
	}
	if got != "ip=dhcp,ip6=fd00::5/64,gw6=fd00::1" {
		t.Errorf("ipconfig = %q; want ip=dhcp,ip6=fd00::5/64,gw6=fd00::1", got)
	}
}

func TestBuildIPConfig_VIPOnlyNICEmitsNothing(t *testing.T) {
	got, err := buildIPConfigFor(t, map[string]createVMNetworkSpec{
		"vip": {Type: nicTypeVIP, IP: "10.0.0.99"},
	})
	if err != nil {
		t.Fatalf("buildIPConfig: %v", err)
	}
	if got != "" {
		t.Errorf("ipconfig = %q; a VIP-only NIC needs no ipconfig entry", got)
	}
}

func TestBuildIPConfig_TwoNetworksOfSameFamilyIsAnError(t *testing.T) {
	_, err := buildIPConfigFor(t, map[string]createVMNetworkSpec{
		"alpha": {Type: nicTypeManual, NicGroup: "g", IP: "10.0.0.5", Netmask: "255.255.255.0"},
		"bravo": {Type: nicTypeManual, NicGroup: "g", IP: "10.0.0.6", Netmask: "255.255.255.0"},
	})
	if err == nil {
		t.Fatal("two IPv4 networks on one NIC must be rejected; got no error")
	}
	if !strings.Contains(err.Error(), "IPv4 address") {
		t.Errorf("error = %v; want it to name the contended IPv4 address", err)
	}
}

func TestBuildIPConfig_TwoIPv6NetworksIsAnError(t *testing.T) {
	_, err := buildIPConfigFor(t, map[string]createVMNetworkSpec{
		"alpha": {Type: nicTypeManual, NicGroup: "g", IP: "fd00::5", Netmask: "ffff:ffff:ffff:ffff::"},
		"bravo": {Type: nicTypeManual, NicGroup: "g", IP: "fd00::6", Netmask: "ffff:ffff:ffff:ffff::"},
	})
	if err == nil {
		t.Fatal("two IPv6 networks on one NIC must be rejected; got no error")
	}
	if !strings.Contains(err.Error(), "IPv6 address") {
		t.Errorf("error = %v; want it to name the contended IPv6 address", err)
	}
}

// --------------------------------------------------------------------------
// configureNICs end to end — a dual-stack nic_group is one PVE NIC
// --------------------------------------------------------------------------

func TestConfigureNICs_DualStackGroupWritesOneNIC(t *testing.T) {
	cfg := icMinConfig()
	cfg.NetworkBridge = "vmbr0"

	parsed := &createVMParsedArgs{
		networks: map[string]createVMNetworkSpec{
			"default": {
				Type: nicTypeManual, NicGroup: "nic0",
				IP: "10.254.48.190", Netmask: "255.255.0.0", Gateway: "10.254.0.1",
				CloudProperties: map[string]any{},
			},
			"ipv6": {
				Type: nicTypeManual, NicGroup: "nic0",
				IP:              "fd36:afd4:2c42:1::48:190",
				Netmask:         "ffff:ffff:ffff:ffff:0000:0000:0000:0000",
				Gateway:         "fd36:afd4:2c42:1::1",
				CloudProperties: map[string]any{},
			},
		},
	}
	nd := &fwNodesStub{}
	deps := fwDeps(&fwClusterStub{}, nd, cfg)
	shape := &createVMShape{node: "pve1"}

	plan, err := configureNICs(context.Background(), deps, log.NewNopLogger(), parsed, shape, 100)
	if err != nil {
		t.Fatalf("configureNICs: %v", err)
	}

	if len(plan) != 1 {
		t.Fatalf("plan has %d NICs; a dual-stack nic_group is one interface: %+v", len(plan), plan)
	}
	if len(nd.lastNet) != 1 {
		t.Fatalf("wrote %d net entries, want 1: %v", len(nd.lastNet), nd.lastNet)
	}
	if len(nd.lastIpconfig) != 1 {
		t.Fatalf("wrote %d ipconfig entries, want 1: %v", len(nd.lastIpconfig), nd.lastIpconfig)
	}
	got := nd.lastIpconfig[0]
	for _, want := range []string{
		"ip=10.254.48.190/16",
		"gw=10.254.0.1",
		"ip6=fd36:afd4:2c42:1::48:190/64",
		"gw6=fd36:afd4:2c42:1::1",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("ipconfig0 = %q; missing %q", got, want)
		}
	}

	// Both networks must report the one NIC's MAC, or the agent cannot bind
	// either of them to the interface.
	for _, name := range []string{"default", "ipv6"} {
		if mac := parsed.networks[name].MAC; mac != "aa:bb:cc:dd:ee:ff" {
			t.Errorf("networks[%q].MAC = %q; want the net0 MAC aa:bb:cc:dd:ee:ff", name, mac)
		}
	}
}

func TestConfigureNICs_GroupWithConflictingBridgesIsRejected(t *testing.T) {
	cfg := icMinConfig()
	cfg.NetworkBridge = "vmbr0"

	parsed := &createVMParsedArgs{
		networks: map[string]createVMNetworkSpec{
			"default": {
				Type: nicTypeManual, NicGroup: "nic0",
				IP: "10.0.0.5", Netmask: "255.255.255.0",
				CloudProperties: map[string]any{nicCPKeyBridge: "vmbr0"},
			},
			"ipv6": {
				Type: nicTypeManual, NicGroup: "nic0",
				IP: "fd00::5", Netmask: "ffff:ffff:ffff:ffff::",
				CloudProperties: map[string]any{nicCPKeyBridge: "vmbr9"},
			},
		},
	}
	nd := &fwNodesStub{}
	deps := fwDeps(&fwClusterStub{}, nd, cfg)
	shape := &createVMShape{node: "pve1"}

	_, err := configureNICs(context.Background(), deps, log.NewNopLogger(), parsed, shape, 100)
	if err == nil {
		t.Fatal("a nic_group whose members want different bridges must be rejected; got no error")
	}
	if !strings.Contains(err.Error(), "nic_group") {
		t.Errorf("error = %v; want it to explain the nic_group mismatch", err)
	}
}

// --------------------------------------------------------------------------
// MAC propagation — the agent cannot match interfaces without it
// --------------------------------------------------------------------------

func TestResolveNICMACs_MultiNetworkWithoutMACIsFatal(t *testing.T) {
	cfg := icMinConfig()
	parsed := &createVMParsedArgs{
		networks: map[string]createVMNetworkSpec{
			"default": {Type: nicTypeDynamic, CloudProperties: map[string]any{}},
			"storage": {Type: nicTypeDynamic, CloudProperties: map[string]any{}},
		},
	}
	nd := &fwNodesStub{}
	deps := fwDeps(&fwClusterStub{}, nd, cfg)
	// PVE reporting no NIC at all is the shape that would silently produce a
	// VM whose every interface the agent configures as DHCP.
	deps.PVE = &icPVEClient{
		clusterSvc: &fwClusterStub{},
		nodesSvc:   nd,
		qemuSvc:    &fwQEMUStub{configResult: map[string]any{}},
	}
	shape := &createVMShape{node: "pve1"}

	err := resolveNICMACs(context.Background(), deps, log.NewNopLogger(), parsed, shape, 100, planNICs(parsed.networks))
	if err == nil {
		t.Fatal("a multi-network VM with no MAC must fail the create; got no error")
	}
	if !strings.Contains(err.Error(), "MAC") {
		t.Errorf("error = %v; want it to name the missing MAC", err)
	}
}

func TestResolveNICMACs_SingleNetworkToleratesMissingMAC(t *testing.T) {
	// One network on one interface is the case the BOSH agent can still match
	// without a MAC, and it is how every VM this CPI made before the readback
	// existed came up. Keep it working.
	cfg := icMinConfig()
	parsed := &createVMParsedArgs{
		networks: map[string]createVMNetworkSpec{
			"default": {Type: nicTypeDynamic, CloudProperties: map[string]any{}},
		},
	}
	nd := &fwNodesStub{}
	deps := fwDeps(&fwClusterStub{}, nd, cfg)
	deps.PVE = &icPVEClient{
		clusterSvc: &fwClusterStub{},
		nodesSvc:   nd,
		qemuSvc:    &fwQEMUStub{configResult: map[string]any{}},
	}
	shape := &createVMShape{node: "pve1"}

	if err := resolveNICMACs(context.Background(), deps, log.NewNopLogger(), parsed, shape, 100,
		planNICs(parsed.networks)); err != nil {
		t.Fatalf("single-network VM must tolerate a missing MAC: %v", err)
	}
}

func TestResolveNICMACs_ReadFailureIsFatalOnlyForMultiNetwork(t *testing.T) {
	cfg := icMinConfig()
	nd := &fwNodesStub{}
	readErr := errors.New("pve unreachable")

	multi := &createVMParsedArgs{
		networks: map[string]createVMNetworkSpec{
			"default": {Type: nicTypeDynamic, CloudProperties: map[string]any{}},
			"storage": {Type: nicTypeDynamic, CloudProperties: map[string]any{}},
		},
	}
	deps := fwDeps(&fwClusterStub{}, nd, cfg)
	deps.PVE = &icPVEClient{
		clusterSvc: &fwClusterStub{},
		nodesSvc:   nd,
		qemuSvc:    &fwQEMUStub{configErr: readErr},
	}
	shape := &createVMShape{node: "pve1"}
	if err := resolveNICMACs(context.Background(), deps, log.NewNopLogger(), multi, shape, 100,
		planNICs(multi.networks)); err == nil {
		t.Error("a failed MAC readback on a multi-network VM must fail the create")
	}

	single := &createVMParsedArgs{
		networks: map[string]createVMNetworkSpec{
			"default": {Type: nicTypeDynamic, CloudProperties: map[string]any{}},
		},
	}
	if err := resolveNICMACs(context.Background(), deps, log.NewNopLogger(), single, shape, 100,
		planNICs(single.networks)); err != nil {
		t.Errorf("a failed MAC readback on a single-network VM must stay fail-open: %v", err)
	}
}

func TestResolveNICMACs_SharedNICGroupWithoutMACIsFatal(t *testing.T) {
	// A dual-stack nic_group is one NIC carrying two networks. The agent can
	// tolerate a missing MAC only for a single network on a single interface;
	// with two networks it falls through to configuring every interface as
	// DHCP. So the single-NIC fail-open must not apply to a shared group.
	cfg := icMinConfig()
	nd := &fwNodesStub{}
	readErr := errors.New("pve unreachable")

	newParsed := func() *createVMParsedArgs {
		return &createVMParsedArgs{
			networks: map[string]createVMNetworkSpec{
				"default": {Type: nicTypeManual, NicGroup: "nic0",
					IP: "10.0.0.5", Netmask: "255.255.255.0", CloudProperties: map[string]any{}},
				"ipv6": {Type: nicTypeManual, NicGroup: "nic0",
					IP: "fd00::5", Netmask: "ffff:ffff:ffff:ffff::", CloudProperties: map[string]any{}},
			},
		}
	}
	shape := &createVMShape{node: "pve1"}

	// Readback failure: must be fatal, not fail-open.
	parsed := newParsed()
	deps := fwDeps(&fwClusterStub{}, nd, cfg)
	deps.PVE = &icPVEClient{
		clusterSvc: &fwClusterStub{},
		nodesSvc:   nd,
		qemuSvc:    &fwQEMUStub{configErr: readErr},
	}
	if err := resolveNICMACs(context.Background(), deps, log.NewNopLogger(), parsed, shape, 100,
		planNICs(parsed.networks)); err == nil {
		t.Error("a failed MAC readback on a shared dual-stack NIC must fail the create")
	}

	// PVE reporting no MAC for the shared NIC: must be fatal too.
	parsed = newParsed()
	deps.PVE = &icPVEClient{
		clusterSvc: &fwClusterStub{},
		nodesSvc:   nd,
		qemuSvc:    &fwQEMUStub{configResult: map[string]any{}},
	}
	if err := resolveNICMACs(context.Background(), deps, log.NewNopLogger(), parsed, shape, 100,
		planNICs(parsed.networks)); err == nil {
		t.Error("a shared dual-stack NIC with no MAC from PVE must fail the create")
	}
}

func TestBuildAgentNetworks_CarriesMAC(t *testing.T) {
	networks := map[string]createVMNetworkSpec{
		"default": {Type: nicTypeManual, IP: "10.0.0.5", MAC: "aa:bb:cc:dd:ee:ff"},
		"ipv6":    {Type: nicTypeManual, IP: "fd00::5", MAC: "aa:bb:cc:dd:ee:ff"},
	}
	out := buildAgentNetworks(networks)
	for name := range networks {
		if out[name].MAC != "aa:bb:cc:dd:ee:ff" {
			t.Errorf("agent networks[%q].MAC = %q; want aa:bb:cc:dd:ee:ff", name, out[name].MAC)
		}
	}
}

func TestBuildResponseNetworks_SharedNICReportsOneMACForEveryMember(t *testing.T) {
	networks := map[string]createVMNetworkSpec{
		"default": {Type: nicTypeManual, NicGroup: "g", IP: "10.0.0.5"},
		"ipv6":    {Type: nicTypeManual, NicGroup: "g", IP: "fd00::5"},
		"second":  {Type: nicTypeManual, IP: "10.1.0.5"},
	}
	vmCfg := map[string]any{
		"net0": "virtio=aa:bb:cc:dd:ee:ff,bridge=vmbr0",
		"net1": "virtio=aa:bb:cc:dd:ee:01,bridge=vmbr1",
	}
	out := buildResponseNetworks(networks, planNICs(networks), vmCfg)

	if out["default"].MAC != "aa:bb:cc:dd:ee:ff" || out["ipv6"].MAC != "aa:bb:cc:dd:ee:ff" {
		t.Errorf("grouped networks report MACs %q/%q; both want net0's aa:bb:cc:dd:ee:ff",
			out["default"].MAC, out["ipv6"].MAC)
	}
	if out["second"].MAC != "aa:bb:cc:dd:ee:01" {
		t.Errorf("second.MAC = %q; want net1's aa:bb:cc:dd:ee:01", out["second"].MAC)
	}
}

// --------------------------------------------------------------------------
// Remediation coverage: nic_group typing, address-less group members, and the
// MAC a previous placement attempt left behind.
// --------------------------------------------------------------------------

// The upstream BATS templates write `nic_group: 1` unquoted, so the director
// can hand the CPI a JSON number. Rejecting it would fail create_vm at parse
// time for every VM in the deployment.
func TestCreateVMNetworkSpec_NicGroupAcceptsStringAndNumber(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"string", `{"nic_group":"nic0"}`, "nic0"},
		{"number", `{"nic_group":1}`, "1"},
		{"absent", `{}`, ""},
		{"null", `{"nic_group":null}`, ""},
	}
	for _, tc := range cases {
		var spec createVMNetworkSpec
		if err := json.Unmarshal([]byte(tc.raw), &spec); err != nil {
			t.Fatalf("%s: unmarshal %s: %v", tc.name, tc.raw, err)
		}
		if string(spec.NicGroup) != tc.want {
			t.Errorf("%s: nic_group = %q; want %q", tc.name, spec.NicGroup, tc.want)
		}
	}

	// Two networks written with the same numeric group still land on one NIC.
	var v4, v6 createVMNetworkSpec
	if err := json.Unmarshal([]byte(`{"type":"manual","ip":"10.0.0.5","netmask":"255.255.255.0","nic_group":1}`), &v4); err != nil {
		t.Fatalf("unmarshal v4: %v", err)
	}
	if err := json.Unmarshal([]byte(`{"type":"manual","ip":"fd00::5","netmask":"64","nic_group":1}`), &v6); err != nil {
		t.Fatalf("unmarshal v6: %v", err)
	}
	plan := planNICs(map[string]createVMNetworkSpec{"default": v4, "ipv6": v6})
	if len(plan) != 1 {
		t.Fatalf("numeric nic_group must collapse both networks onto one NIC; got %d NICs", len(plan))
	}
}

// A group member with no address at all (the prefix-delegation network in the
// upstream templates) contributes nothing rather than claiming DHCP over a
// sibling's static address.
func TestBuildIPConfig_AddresslessManualMemberContributesNothing(t *testing.T) {
	got, err := buildIPConfigFor(t, map[string]createVMNetworkSpec{
		"default": {
			Type: nicTypeManual, IP: "10.0.0.5", Netmask: "255.255.255.0",
			Gateway: "10.0.0.1", NicGroup: "nic0",
		},
		"ipv6": {
			Type: nicTypeManual, IP: "fd00::5", Netmask: "64",
			Gateway: "fd00::1", NicGroup: "nic0",
		},
		"prefix": {Type: nicTypeManual, NicGroup: "nic0"},
	})
	if err != nil {
		t.Fatalf("buildIPConfig: %v", err)
	}
	want := "ip=10.0.0.5/24,gw=10.0.0.1,ip6=fd00::5/64,gw6=fd00::1"
	if got != want {
		t.Errorf("ipconfig = %q; want %q", got, want)
	}
}

// A lone manual network with no address keeps its long-standing DHCP meaning.
func TestBuildIPConfig_LoneAddresslessManualStillMeansDHCP(t *testing.T) {
	got, err := buildIPConfigFor(t, map[string]createVMNetworkSpec{
		"default": {Type: nicTypeManual},
	})
	if err != nil {
		t.Fatalf("buildIPConfig: %v", err)
	}
	if got != "ip=dhcp" {
		t.Errorf("ipconfig = %q; want ip=dhcp", got)
	}
}

// createVMWithFallback reuses one parsed-args struct across candidate nodes.
// A MAC stamped by a failed attempt must not survive into the settings the
// next attempt writes when the readback yields nothing.
func TestResolveNICMACs_ClearsStaleMACFromAnEarlierAttempt(t *testing.T) {
	cfg := icMinConfig()
	parsed := &createVMParsedArgs{
		networks: map[string]createVMNetworkSpec{
			"default": {Type: nicTypeDynamic, MAC: "aa:bb:cc:dd:ee:ff", CloudProperties: map[string]any{}},
		},
	}
	nd := &fwNodesStub{}
	deps := fwDeps(&fwClusterStub{}, nd, cfg)
	deps.PVE = &icPVEClient{
		clusterSvc: &fwClusterStub{},
		nodesSvc:   nd,
		// The second attempt's VM reports no NIC: the single-network path is
		// fail-open, so the MAC must simply be gone.
		qemuSvc: &fwQEMUStub{configResult: map[string]any{}},
	}
	shape := &createVMShape{node: "pve2"}

	if err := resolveNICMACs(context.Background(), deps, log.NewNopLogger(), parsed, shape, 100,
		planNICs(parsed.networks)); err != nil {
		t.Fatalf("single-network VM must tolerate a missing MAC: %v", err)
	}
	if got := parsed.networks["default"].MAC; got != "" {
		t.Errorf("MAC = %q; want it cleared so the agent settings omit it", got)
	}
}

// --------------------------------------------------------------------------
// Second remediation round: family inference, ipfilter on IPv6, DNS union,
// mapped-IPv4 literals, and the ipconfig round trip.
// --------------------------------------------------------------------------

// A dynamic network carries no family signal. It may claim IPv4 and nothing
// else — inferring IPv6 for it because IPv4 is taken would make the outcome
// depend on the alphabetical order of the network names.
func TestBuildIPConfig_DynamicMemberNeverInfersIPv6(t *testing.T) {
	// "alpha" sorts before "zulu", so the manual network is seen first.
	_, err := buildIPConfigFor(t, map[string]createVMNetworkSpec{
		"alpha": {Type: nicTypeManual, IP: "10.0.0.5", Netmask: "255.255.255.0", NicGroup: "1"},
		"zulu":  {Type: nicTypeDynamic, NicGroup: "1"},
	})
	if err == nil {
		t.Fatal("a dynamic member alongside a static IPv4 member must be an error, not a silent ip6=auto")
	}
	// And the reverse order must fail the same way rather than succeeding.
	_, revErr := buildIPConfigFor(t, map[string]createVMNetworkSpec{
		"alpha": {Type: nicTypeDynamic, NicGroup: "1"},
		"bravo": {Type: nicTypeManual, IP: "10.0.0.5", Netmask: "255.255.255.0", NicGroup: "1"},
	})
	if revErr == nil {
		t.Fatal("the same group in the opposite name order must fail the same way")
	}
}

// A dynamic IPv4 network and a static IPv6 one is a legitimate shape: DHCP
// for v4, an address for v6.
func TestBuildIPConfig_DynamicPlusStaticIPv6(t *testing.T) {
	got, err := buildIPConfigFor(t, map[string]createVMNetworkSpec{
		"default": {Type: nicTypeDynamic, NicGroup: "1"},
		"ipv6": {
			Type: nicTypeManual, IP: "fd00::5", Netmask: "64",
			Gateway: "fd00::1", NicGroup: "1",
		},
	})
	if err != nil {
		t.Fatalf("buildIPConfig: %v", err)
	}
	want := "ip=dhcp,ip6=fd00::5/64,gw6=fd00::1"
	if got != want {
		t.Errorf("ipconfig = %q; want %q", got, want)
	}
}

// An IPv4-mapped IPv6 literal is an IPv4 address everywhere else in the
// package; PVE rejects the mapped spelling in ipconfig.
func TestIPToCIDR_NormalizesIPv4MappedLiteral(t *testing.T) {
	if got := ipToCIDR("::ffff:10.0.0.5", "255.255.255.0", "", nil, "default"); got != "10.0.0.5/24" {
		t.Errorf("ipToCIDR = %q; want 10.0.0.5/24", got)
	}
}

// Whatever configureNICs writes, the conflict scan has to be able to read
// back. This is the pairing that keeps the two from drifting apart.
func TestIPConfigRoundTripsThroughExtractStaticIPs(t *testing.T) {
	cases := []struct {
		name     string
		networks map[string]createVMNetworkSpec
		want     []string
	}{
		{
			name: "dual stack",
			networks: map[string]createVMNetworkSpec{
				"default": {
					Type: nicTypeManual, IP: "10.254.48.190", Netmask: "255.255.0.0",
					Gateway: "10.254.0.1", NicGroup: "1",
				},
				"ipv6": {
					Type: nicTypeManual, IP: "fd36:afd4:2c42:1::48:190", Netmask: "64",
					Gateway: "fd36:afd4:2c42:1::1", NicGroup: "1",
				},
			},
			want: []string{"10.254.48.190", "fd36:afd4:2c42:1::48:190"},
		},
		{
			name: "mapped ipv4 literal",
			networks: map[string]createVMNetworkSpec{
				"default": {Type: nicTypeManual, IP: "::ffff:10.0.0.5", Netmask: "255.255.255.0"},
			},
			want: []string{"10.0.0.5"},
		},
		{
			name: "dhcp carries no static address",
			networks: map[string]createVMNetworkSpec{
				"default": {Type: nicTypeDynamic},
			},
			want: nil,
		},
	}
	for _, tc := range cases {
		cfg, err := buildIPConfigFor(t, tc.networks)
		if err != nil {
			t.Fatalf("%s: buildIPConfig: %v", tc.name, err)
		}
		got := extractStaticIPs(cfg)
		if len(got) != len(tc.want) {
			t.Fatalf("%s: extractStaticIPs(%q) = %v; want %v", tc.name, cfg, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("%s: extractStaticIPs(%q)[%d] = %q; want %q", tc.name, cfg, i, got[i], tc.want[i])
			}
		}
	}
}

// Creating ipfilter-net{N} replaces the set PVE would have generated, so a
// NIC with an IPv6 address needs its link-local and multicast ranges or
// neighbor discovery stops and IPv6 goes dark.
func TestBuildIPSetEntries_IPv6NICKeepsNeighborDiscovery(t *testing.T) {
	entries := buildIPSetEntries(
		[]string{"10.254.48.190", "fd36:afd4:2c42:1::48:190"},
		[]string{"10.254.48.200/32"},
	)
	want := []string{
		"10.254.48.190/32",
		"fd36:afd4:2c42:1::48:190/128",
		"10.254.48.200/32",
		"fe80::/10",
		"ff02::/16",
	}
	if len(entries) != len(want) {
		t.Fatalf("entries = %v; want %v", entries, want)
	}
	for i := range want {
		if entries[i] != want[i] {
			t.Errorf("entries[%d] = %q; want %q", i, entries[i], want[i])
		}
	}

	// An IPv4-only NIC must be byte-identical to what it was before.
	v4 := buildIPSetEntries([]string{"10.254.48.190"}, []string{"10.254.48.200/32"})
	if len(v4) != 2 || v4[0] != "10.254.48.190/32" || v4[1] != "10.254.48.200/32" {
		t.Errorf("IPv4-only entries = %v; want the address and the VIP only", v4)
	}
}

// A nic_group folds two networks onto net0, so the IPv6 network must not
// shift the forwarding write onto a net1 that does not exist.
func TestApplyIPForwarding_GroupedNICWritesTheGroupsSlot(t *testing.T) {
	networks := map[string]createVMNetworkSpec{
		"default": {
			Type: nicTypeManual, IP: "10.0.0.5", Netmask: "255.255.255.0",
			NicGroup: "1", CloudProperties: map[string]any{},
		},
		// The member that asks for forwarding is the second one on the NIC.
		"ipv6": {
			Type: nicTypeManual, IP: "fd00::5", Netmask: "64", NicGroup: "1",
			CloudProperties: map[string]any{"ip_forwarding": true},
		},
	}
	nd := &fwNodesStub{}
	deps := fwDepsWithConfig(&fwClusterStub{}, nd, icMinConfig(), map[string]any{
		"net0": "virtio=AA:BB:CC:DD:EE:00,bridge=vmbr0,firewall=1",
	})

	if err := applyIPForwarding(context.Background(), deps, "pve1", 100, networks, log.NewNopLogger()); err != nil {
		t.Fatalf("applyIPForwarding: %v", err)
	}
	if len(nd.lastNet) != 1 {
		t.Fatalf("expected exactly one NIC write, got %v", nd.lastNet)
	}
	got, ok := nd.lastNet[0]
	if !ok {
		t.Fatalf("forwarding was written to %v; want net0, the group's slot", nd.lastNet)
	}
	if !strings.Contains(got, "firewall=0") {
		t.Errorf("net0 = %q; want the firewall token turned off", got)
	}
	if !strings.Contains(got, "bridge=vmbr0") || !strings.Contains(got, "AA:BB:CC:DD:EE:00") {
		t.Errorf("net0 = %q; want the read-modify-write to preserve bridge and MAC", got)
	}
}
