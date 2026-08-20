// Package handlers internal tests for §7.14 VIP allowed-address-pairs ipfilter.
package handlers

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
)

// --------------------------------------------------------------------------
// Unit table: normalizeVIPEntry
// --------------------------------------------------------------------------

func TestNormalizeVIPEntry(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{"bare ipv4 gets /32", "10.0.0.5", "10.0.0.5/32", false},
		{"bare ipv6 gets /128", "::1", "::1/128", false},
		{"valid cidr v4 passthrough", "10.0.0.0/24", "10.0.0.0/24", false},
		{"valid cidr host v4", "10.0.0.5/24", "10.0.0.5/24", false},
		{"valid cidr v6", "2001:db8::/32", "2001:db8::/32", false},
		{"malformed string errors", "bad", "", true},
		{"empty string errors", "", "", true},
		{"whitespace-only errors", "   ", "", true},
		{"ip with whitespace trimmed", " 10.0.0.1 ", "10.0.0.1/32", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := normalizeVIPEntry(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("normalizeVIPEntry(%q): expected error, got %q", tc.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizeVIPEntry(%q): unexpected error: %v", tc.input, err)
			}
			if got != tc.want {
				t.Errorf("normalizeVIPEntry(%q) = %q; want %q", tc.input, got, tc.want)
			}
		})
	}
}

// --------------------------------------------------------------------------
// Unit table: parseVIPEntries
// --------------------------------------------------------------------------

func TestParseVIPEntries(t *testing.T) {
	t.Run("nil cp returns nil nil", func(t *testing.T) {
		got, err := parseVIPEntries(nil)
		if err != nil || got != nil {
			t.Errorf("got %v, %v; want nil nil", got, err)
		}
	})

	t.Run("absent key returns nil nil", func(t *testing.T) {
		got, err := parseVIPEntries(map[string]any{"other": "value"})
		if err != nil || got != nil {
			t.Errorf("got %v, %v; want nil nil", got, err)
		}
	})

	t.Run("nil value returns nil nil", func(t *testing.T) {
		got, err := parseVIPEntries(map[string]any{"allowed_address_pairs": nil})
		if err != nil || got != nil {
			t.Errorf("got %v, %v; want nil nil", got, err)
		}
	})

	t.Run("[]any coerced to normalized strings", func(t *testing.T) {
		cp := map[string]any{"allowed_address_pairs": []any{"10.0.0.5"}}
		got, err := parseVIPEntries(cp)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 1 || got[0] != "10.0.0.5/32" {
			t.Errorf("got %v; want [10.0.0.5/32]", got)
		}
	})

	t.Run("[]string accepted directly", func(t *testing.T) {
		cp := map[string]any{"allowed_address_pairs": []string{"192.168.1.10", "192.168.1.20/24"}}
		got, err := parseVIPEntries(cp)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("got %v; want 2 entries", got)
		}
		if got[0] != "192.168.1.10/32" {
			t.Errorf("got[0] = %q; want 192.168.1.10/32", got[0])
		}
		if got[1] != "192.168.1.20/24" {
			t.Errorf("got[1] = %q; want 192.168.1.20/24", got[1])
		}
	})

	t.Run("non-string element in []any errors", func(t *testing.T) {
		cp := map[string]any{"allowed_address_pairs": []any{42}}
		_, err := parseVIPEntries(cp)
		if err == nil {
			t.Fatal("expected error for non-string element")
		}
		if !strings.Contains(err.Error(), "must be string") {
			t.Errorf("error %q should mention 'must be string'", err.Error())
		}
	})

	t.Run("dedup preserves first-seen order", func(t *testing.T) {
		cp := map[string]any{"allowed_address_pairs": []string{"10.0.0.5", "10.0.0.5"}}
		got, err := parseVIPEntries(cp)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 1 {
			t.Errorf("got %v; want exactly one entry after dedup", got)
		}
		if got[0] != "10.0.0.5/32" {
			t.Errorf("got[0] = %q; want 10.0.0.5/32", got[0])
		}
	})

	t.Run("empty list after dedup returns nil", func(t *testing.T) {
		// Empty []any — no entries at all.
		cp := map[string]any{"allowed_address_pairs": []any{}}
		got, err := parseVIPEntries(cp)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != nil {
			t.Errorf("got %v; want nil for empty list", got)
		}
	})
}

// --------------------------------------------------------------------------
// Unit table: validateVIPAllowedAddressPairs
// --------------------------------------------------------------------------

func TestValidateVIPAllowedAddressPairs(t *testing.T) {
	t.Run("valid entries returns nil", func(t *testing.T) {
		nets := map[string]createVMNetworkSpec{
			"default": {
				Type:            "manual",
				IP:              "10.0.0.5",
				Netmask:         "255.255.255.0",
				CloudProperties: map[string]any{"allowed_address_pairs": []string{"10.0.0.100"}},
			},
		}
		if err := validateVIPAllowedAddressPairs(nets); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("malformed entry returns Cloud error mentioning bad value", func(t *testing.T) {
		nets := map[string]createVMNetworkSpec{
			"default": {
				Type:            "manual",
				IP:              "10.0.0.5",
				CloudProperties: map[string]any{"allowed_address_pairs": []string{"not-an-ip"}},
			},
		}
		err := validateVIPAllowedAddressPairs(nets)
		if err == nil {
			t.Fatal("expected CloudError for malformed entry")
		}
		if !strings.Contains(err.Error(), "not-an-ip") {
			t.Errorf("error %q should mention the bad value", err.Error())
		}
	})

	t.Run("no allowed_address_pairs anywhere returns nil", func(t *testing.T) {
		nets := map[string]createVMNetworkSpec{
			"default": {Type: "dynamic", CloudProperties: map[string]any{}},
		}
		if err := validateVIPAllowedAddressPairs(nets); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

// --------------------------------------------------------------------------
// Behavior tests using fwNodesStub
// --------------------------------------------------------------------------

// vipDeps builds a Deps with fwNodesStub and the specified global firewall setting.
//
//nolint:unparam // globalFW parameterized for legibility; future test variants may vary it
func vipDeps(nd *fwNodesStub, globalFW bool) Deps {
	cfg := icMinConfig()
	v := globalFW
	cfg.VMFirewall = &v
	return fwDeps(&fwClusterStub{}, nd, cfg)
}

// makeStaticNet builds a createVMNetworkSpec for a static NIC.
func makeStaticNet(ip string, vips []string, nicFWOverride *bool) createVMNetworkSpec {
	cp := map[string]any{}
	if len(vips) > 0 {
		cp["allowed_address_pairs"] = vips
	}
	if nicFWOverride != nil {
		cp["firewall"] = *nicFWOverride
	}
	return createVMNetworkSpec{
		Type:            "manual",
		IP:              ip,
		Netmask:         "255.255.255.0",
		CloudProperties: cp,
	}
}

// makeDHCPNet builds a dynamic NIC spec.
func makeDHCPNet(vips []string, nicFWOverride *bool) createVMNetworkSpec {
	cp := map[string]any{}
	if len(vips) > 0 {
		cp["allowed_address_pairs"] = vips
	}
	if nicFWOverride != nil {
		cp["firewall"] = *nicFWOverride
	}
	return createVMNetworkSpec{
		Type:            "dynamic",
		CloudProperties: cp,
	}
}

// --------------------------------------------------------------------------
// Test 1: one firewalled static NIC + 1 VIP
// --------------------------------------------------------------------------

func TestApplyVIPAllowedAddressPairs_HappyPath(t *testing.T) {
	nd := &fwNodesStub{}
	deps := vipDeps(nd, true) // global firewall=true

	nets := map[string]createVMNetworkSpec{
		"default": makeStaticNet("10.0.0.5", []string{"10.0.0.100"}, nil),
	}

	err := applyVIPAllowedAddressPairs(context.Background(), deps, "pve1", 100, nets, log.NewNopLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// ipset "ipfilter-net0" must be created.
	if len(nd.ipsetCreated) != 1 || nd.ipsetCreated[0] != "ipfilter-net0" {
		t.Errorf("ipsetCreated = %v; want [ipfilter-net0]", nd.ipsetCreated)
	}

	// Entries: primary IP first, then VIP. Order matters.
	wantEntries := []string{"10.0.0.5/32", "10.0.0.100/32"}
	gotEntries := nd.ipsetEntries["ipfilter-net0"]
	if len(gotEntries) != len(wantEntries) {
		t.Fatalf("ipset entries = %v; want %v", gotEntries, wantEntries)
	}
	for i, w := range wantEntries {
		if gotEntries[i] != w {
			t.Errorf("entries[%d] = %q; want %q (order matters: primary first)", i, gotEntries[i], w)
		}
	}

	if !nd.ipfilterEnabled {
		t.Error("ipfilterEnabled must be true after successful VIP seeding")
	}
}

// --------------------------------------------------------------------------
// Test 2: no allowed_address_pairs → zero PVE calls
// --------------------------------------------------------------------------

func TestApplyVIPAllowedAddressPairs_NoVIPs_ByteIdentical(t *testing.T) {
	nd := &fwNodesStub{}
	deps := vipDeps(nd, true)

	nets := map[string]createVMNetworkSpec{
		"default": makeStaticNet("10.0.0.5", nil, nil),
	}

	err := applyVIPAllowedAddressPairs(context.Background(), deps, "pve1", 100, nets, log.NewNopLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(nd.ipsetCreated) != 0 {
		t.Errorf("ipsetCreated = %v; want zero PVE calls when no VIPs", nd.ipsetCreated)
	}
	if len(nd.ipsetEntries) != 0 {
		t.Errorf("ipsetEntries = %v; want zero PVE calls when no VIPs", nd.ipsetEntries)
	}
	if nd.ipfilterEnabled {
		t.Error("ipfilterEnabled must be false when no VIPs are declared")
	}
}

// --------------------------------------------------------------------------
// Test 3: VIP on NIC with firewall disabled → no ipset, no ipfilter
// --------------------------------------------------------------------------

func TestApplyVIPAllowedAddressPairs_VIPOnNonFirewalledNIC_NoMutate(t *testing.T) {
	nd := &fwNodesStub{}
	// Global firewall=true but per-NIC override disables it for this NIC.
	// This verifies that the per-NIC override is respected — the NIC-level
	// firewall=false takes precedence even when the global default is true.
	falseVal := false
	deps := vipDeps(nd, true)

	nets := map[string]createVMNetworkSpec{
		"default": makeStaticNet("10.0.0.5", []string{"10.0.0.100"}, &falseVal),
	}

	err := applyVIPAllowedAddressPairs(context.Background(), deps, "pve1", 100, nets, log.NewNopLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// VIP present but firewall disabled on NIC → warn path; no PVE mutation.
	if len(nd.ipsetCreated) != 0 {
		t.Errorf("ipsetCreated = %v; want no ipset creation when NIC has firewall disabled", nd.ipsetCreated)
	}
	if nd.ipfilterEnabled {
		t.Error("ipfilterEnabled must be false when NIC firewall is disabled")
	}
}

// --------------------------------------------------------------------------
// Test 4: VIP NIC static+firewalled but another firewalled NIC is DHCP
// --------------------------------------------------------------------------

func TestApplyVIPAllowedAddressPairs_DHCPFirewalledNIC_GuardSkip(t *testing.T) {
	nd := &fwNodesStub{}
	deps := vipDeps(nd, true) // global firewall=true for all NICs

	nets := map[string]createVMNetworkSpec{
		// "default" = net0: static, firewalled, has VIP
		"default": makeStaticNet("10.0.0.5", []string{"10.0.0.100"}, nil),
		// "vip" sorts after "default" alphabetically → net1: DHCP, firewalled (no override)
		"vip": makeDHCPNet(nil, nil),
	}

	err := applyVIPAllowedAddressPairs(context.Background(), deps, "pve1", 100, nets, log.NewNopLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Safety guard: DHCP firewalled NIC present → entire VIP step skipped.
	if len(nd.ipsetCreated) != 0 {
		t.Errorf("ipsetCreated = %v; want no ipset when firewalled DHCP NIC present", nd.ipsetCreated)
	}
	if nd.ipfilterEnabled {
		t.Error("ipfilterEnabled must be false when firewalled DHCP NIC present")
	}
}

// --------------------------------------------------------------------------
// Test 5: CreateQemuFirewallIpset returns generic error → fail-open
// --------------------------------------------------------------------------

func TestApplyVIPAllowedAddressPairs_IpsetCreateError_FailOpen(t *testing.T) {
	nd := &fwNodesStub{
		ipsetCreateErr: errors.New("pve 500: internal error"),
	}
	deps := vipDeps(nd, true)

	nets := map[string]createVMNetworkSpec{
		"default": makeStaticNet("10.0.0.5", []string{"10.0.0.100"}, nil),
	}

	err := applyVIPAllowedAddressPairs(context.Background(), deps, "pve1", 100, nets, log.NewNopLogger())
	if err != nil {
		t.Fatalf("best-effort must return nil on ipset create failure; got: %v", err)
	}

	// ipfilter must NOT be enabled when ipset creation failed.
	if nd.ipfilterEnabled {
		t.Error("ipfilterEnabled must be false when ipset create failed (fail-open)")
	}
}

// --------------------------------------------------------------------------
// Test 6: CreateQemuFirewallIpset2 returns error → fail-open
// --------------------------------------------------------------------------

func TestApplyVIPAllowedAddressPairs_IpsetEntryError_FailOpen(t *testing.T) {
	nd := &fwNodesStub{
		ipsetEntryErr: errors.New("pve 500: cannot add entry"),
	}
	deps := vipDeps(nd, true)

	nets := map[string]createVMNetworkSpec{
		"default": makeStaticNet("10.0.0.5", []string{"10.0.0.100"}, nil),
	}

	err := applyVIPAllowedAddressPairs(context.Background(), deps, "pve1", 100, nets, log.NewNopLogger())
	if err != nil {
		t.Fatalf("best-effort must return nil on ipset entry failure; got: %v", err)
	}

	if nd.ipfilterEnabled {
		t.Error("ipfilterEnabled must be false when ipset entry add failed (fail-open)")
	}
}

// --------------------------------------------------------------------------
// Test 7: CreateQemuFirewallIpset "already exists" → tolerated, entries added
// --------------------------------------------------------------------------

func TestApplyVIPAllowedAddressPairs_IpsetAlreadyExists_Tolerated(t *testing.T) {
	nd := &fwNodesStub{
		ipsetCreateErr: errors.New("already exists"),
	}
	deps := vipDeps(nd, true)

	nets := map[string]createVMNetworkSpec{
		"default": makeStaticNet("10.0.0.5", []string{"10.0.0.100"}, nil),
	}

	err := applyVIPAllowedAddressPairs(context.Background(), deps, "pve1", 100, nets, log.NewNopLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Entries still added even though ipset "already exists".
	gotEntries := nd.ipsetEntries["ipfilter-net0"]
	if len(gotEntries) != 2 {
		t.Errorf("ipset entries = %v; want 2 entries despite already-exists error", gotEntries)
	}

	// ipfilter must be enabled.
	if !nd.ipfilterEnabled {
		t.Error("ipfilterEnabled must be true when already-exists is tolerated")
	}
}

// --------------------------------------------------------------------------
// Test 8: two firewalled static NICs — VIP only on net1; both ipsets seeded
// --------------------------------------------------------------------------

func TestApplyVIPAllowedAddressPairs_TwoFirewalledNICs_BothSeeded(t *testing.T) {
	nd := &fwNodesStub{}
	deps := vipDeps(nd, true) // global firewall=true

	// "default" → net0 (no VIPs); "extra" → net1 (has VIP).
	// sortedNetworkNames: "default" first (special), then "extra" alphabetical.
	nets := map[string]createVMNetworkSpec{
		"default": makeStaticNet("10.0.0.5", nil, nil),
		"extra":   makeStaticNet("10.0.0.6", []string{"10.0.0.200"}, nil),
	}

	err := applyVIPAllowedAddressPairs(context.Background(), deps, "pve1", 100, nets, log.NewNopLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Both ipsets must be created.
	if len(nd.ipsetCreated) != 2 {
		t.Fatalf("ipsetCreated = %v; want [ipfilter-net0 ipfilter-net1]", nd.ipsetCreated)
	}

	// net0: seeded with its own primaryIP only (no VIPs on that NIC).
	net0Entries := nd.ipsetEntries["ipfilter-net0"]
	if len(net0Entries) != 1 || net0Entries[0] != "10.0.0.5/32" {
		t.Errorf("net0 entries = %v; want [10.0.0.5/32] (primary only, no lockout)", net0Entries)
	}

	// net1: seeded with its own primaryIP + VIP.
	net1Entries := nd.ipsetEntries["ipfilter-net1"]
	if len(net1Entries) != 2 {
		t.Fatalf("net1 entries = %v; want 2 entries (primary + VIP)", net1Entries)
	}
	if net1Entries[0] != "10.0.0.6/32" {
		t.Errorf("net1 entries[0] = %q; want 10.0.0.6/32 (primary first)", net1Entries[0])
	}
	if net1Entries[1] != "10.0.0.200/32" {
		t.Errorf("net1 entries[1] = %q; want 10.0.0.200/32 (VIP second)", net1Entries[1])
	}

	// ipfilter enabled after ALL seeded.
	if !nd.ipfilterEnabled {
		t.Error("ipfilterEnabled must be true after both NICs seeded")
	}
}

// --------------------------------------------------------------------------
// Helper: sortedVIPKeys — test utility for deterministic ipset map iteration.
// --------------------------------------------------------------------------

// sortedVIPKeys returns the sorted keys of a map, used in assertions where
// ipset entry maps must be iterated in a deterministic order.
func sortedVIPKeys(m map[string][]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func TestSortedVIPKeys(t *testing.T) {
	m := map[string][]string{
		"ipfilter-net1": {"a"},
		"ipfilter-net0": {"b"},
	}
	got := sortedVIPKeys(m)
	if len(got) != 2 || got[0] != "ipfilter-net0" || got[1] != "ipfilter-net1" {
		t.Errorf("sortedVIPKeys = %v; want [ipfilter-net0 ipfilter-net1]", got)
	}
}

// --------------------------------------------------------------------------
// FIX 1 — HIGH #3: unparseable primary IP → lockout prevented (fails-closed)
// --------------------------------------------------------------------------

// TestApplyVIPAllowedAddressPairs_FirewalledStaticNIC_UnparseablePrimaryIP_NoEnable
// verifies that when a firewalled static NIC has an unparseable primary IP,
// applyVIPAllowedAddressPairs does NOT enable ipfilter (lockout prevention).
// Inputs covered: "10.0.0/24" (missing octet), "host.example" (hostname), "10.0.0.5/33" (bad prefix).
func TestApplyVIPAllowedAddressPairs_FirewalledStaticNIC_UnparseablePrimaryIP_NoEnable(t *testing.T) {
	badIPs := []string{"10.0.0/24", "host.example", "10.0.0.5/33"}
	for _, badIP := range badIPs {
		t.Run("badIP="+badIP, func(t *testing.T) {
			nd := &fwNodesStub{}
			deps := vipDeps(nd, true) // global firewall=true

			// NIC is fw+static but primary IP is unparseable.
			nets := map[string]createVMNetworkSpec{
				"default": makeStaticNet(badIP, []string{"10.0.0.100"}, nil),
			}

			err := applyVIPAllowedAddressPairs(context.Background(), deps, "pve1", 100, nets, log.NewNopLogger())
			if err != nil {
				t.Fatalf("best-effort must return nil even on unparseable primary IP; got: %v", err)
			}

			// ipfilter must NOT be enabled — enabling without a valid primary /32 would lock out the NIC.
			if nd.ipfilterEnabled {
				t.Errorf("ipfilterEnabled must be false when primary IP %q is unparseable (lockout prevention)", badIP)
			}
			// No partial ipset should exist either (guard fires before seeding).
			if len(nd.ipsetCreated) != 0 {
				t.Errorf("ipsetCreated = %v; want no ipset when primary IP unparseable", nd.ipsetCreated)
			}
		})
	}
}

// --------------------------------------------------------------------------
// FIX 2 — MEDIUM #12: ipfilter enable call must also set VM-level Enable=true
// --------------------------------------------------------------------------

// TestApplyVIPAllowedAddressPairs_HappyPath_VMFirewallEnabled asserts that the
// successful ipfilter-enable path sets BOTH Enable=true and Ipfilter=true,
// so ipfilter is not inert on VMs where VM-level firewall was not previously on.
func TestApplyVIPAllowedAddressPairs_HappyPath_VMFirewallEnabled(t *testing.T) {
	nd := &fwNodesStub{}
	deps := vipDeps(nd, true)

	nets := map[string]createVMNetworkSpec{
		"default": makeStaticNet("10.0.0.5", []string{"10.0.0.100"}, nil),
	}

	err := applyVIPAllowedAddressPairs(context.Background(), deps, "pve1", 100, nets, log.NewNopLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !nd.ipfilterEnabled {
		t.Error("ipfilterEnabled must be true after successful VIP seeding")
	}
	// VM-level Enable must also be set true so ipfilter is not inert.
	if nd.enableOptCall < 1 {
		t.Errorf("enableOptCall = %d; VM-level firewall Enable must be set true alongside Ipfilter (ipfilter is inert without it)", nd.enableOptCall)
	}
}

// TestApplyVIPAllowedAddressPairs_TwoFirewalledNICs_VMFirewallEnabled mirrors the
// TwoFirewalledNICs test and also asserts VM-level Enable is set.
func TestApplyVIPAllowedAddressPairs_TwoFirewalledNICs_VMFirewallEnabled(t *testing.T) {
	nd := &fwNodesStub{}
	deps := vipDeps(nd, true)

	nets := map[string]createVMNetworkSpec{
		"default": makeStaticNet("10.0.0.5", nil, nil),
		"extra":   makeStaticNet("10.0.0.6", []string{"10.0.0.200"}, nil),
	}

	err := applyVIPAllowedAddressPairs(context.Background(), deps, "pve1", 100, nets, log.NewNopLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !nd.ipfilterEnabled {
		t.Error("ipfilterEnabled must be true after both NICs seeded")
	}
	if nd.enableOptCall < 1 {
		t.Errorf("enableOptCall = %d; VM-level firewall Enable must be set true alongside Ipfilter", nd.enableOptCall)
	}
}

// --------------------------------------------------------------------------
// FIX 4 — MEDIUM #11: primary==VIP dedup (buildIPSetEntries collapses duplicate)
// --------------------------------------------------------------------------

// TestBuildIPSetEntries_PrimaryEqualsVIP_Deduped verifies that when the primary
// IP and a VIP entry normalize to the same /32, the ipset contains exactly one
// entry — no duplicates.
func TestBuildIPSetEntries_PrimaryEqualsVIP_Deduped(t *testing.T) {
	// primary "10.0.0.5" → "10.0.0.5/32"; VIP "10.0.0.5" also → "10.0.0.5/32"
	entries := buildIPSetEntries([]string{"10.0.0.5"}, []string{"10.0.0.5/32"})
	if len(entries) != 1 {
		t.Fatalf("buildIPSetEntries with primary==VIP got %v (len=%d); want exactly 1 entry", entries, len(entries))
	}
	if entries[0] != "10.0.0.5/32" {
		t.Errorf("entries[0] = %q; want 10.0.0.5/32", entries[0])
	}
}

// TestApplyVIPAllowedAddressPairs_PrimaryEqualsVIP_SingleEntry asserts the full
// applyVIPAllowedAddressPairs path produces exactly one ipset entry when the
// NIC's primary IP matches its only VIP entry.
func TestApplyVIPAllowedAddressPairs_PrimaryEqualsVIP_SingleEntry(t *testing.T) {
	nd := &fwNodesStub{}
	deps := vipDeps(nd, true)

	nets := map[string]createVMNetworkSpec{
		// primary IP == only VIP → should collapse to exactly one entry.
		"default": makeStaticNet("10.0.0.5", []string{"10.0.0.5"}, nil),
	}

	err := applyVIPAllowedAddressPairs(context.Background(), deps, "pve1", 100, nets, log.NewNopLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	entries := nd.ipsetEntries["ipfilter-net0"]
	if len(entries) != 1 {
		t.Fatalf("ipset entries = %v (len=%d); want exactly 1 entry when primary==VIP", entries, len(entries))
	}
	if entries[0] != "10.0.0.5/32" {
		t.Errorf("entries[0] = %q; want 10.0.0.5/32", entries[0])
	}
}
