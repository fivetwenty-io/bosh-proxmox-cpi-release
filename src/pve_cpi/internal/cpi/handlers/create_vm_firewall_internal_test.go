// Package handlers internal tests for per-NIC firewall and security-group attach.
package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	sdkcluster "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cluster"
	sdknodes "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/qemu"
)

// fwClusterStubWithFirewall is a compile-time reminder that fwClusterStub already
// satisfies both the groups-present and groups-absent cases used by the new tests.

// fwClusterStub provides ListFirewallGroups; other cluster methods panic via the
// embedded nil interface.
type fwClusterStub struct {
	sdkcluster.Service
	groups  []string
	listErr error
	// SDN vnet listing for the §7.39 consume-side bridge gate. sdnVnets is the
	// set of vnet names ListSdnVnets reports; sdnVnetsErr (when set) forces an
	// error from that call. Only consulted when the gate is enabled.
	sdnVnets         []string
	sdnVnetsErr      error
	listSdnVnetsCall int
}

func (s *fwClusterStub) ListFirewallGroups(_ context.Context) (*sdkcluster.ListFirewallGroupsResponse, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	out := sdkcluster.ListFirewallGroupsResponse{}
	for _, g := range s.groups {
		raw, _ := json.Marshal(map[string]any{"group": g})
		out = append(out, raw)
	}
	return &out, nil
}

func (s *fwClusterStub) ListSdnVnets(_ context.Context, _ *sdkcluster.ListSdnVnetsParams) (*sdkcluster.ListSdnVnetsResponse, error) {
	s.listSdnVnetsCall++
	if s.sdnVnetsErr != nil {
		return nil, s.sdnVnetsErr
	}
	out := sdkcluster.ListSdnVnetsResponse{}
	for _, v := range s.sdnVnets {
		out = append(out, json.RawMessage(`{"vnet":"`+v+`","zone":"z1"}`))
	}
	return &out, nil
}

// fwNodesStub records firewall rule + option calls and the last NIC net map
// applied via UpdateQemuConfig. Other nodes methods panic via the nil embed.
type fwNodesStub struct {
	sdknodes.Service
	ruleActions   []string // "<type>:<action>" per CreateQemuFirewallRules call
	enableOptCall int      // count of UpdateQemuFirewallOptions{Enable:true}
	lastNet       map[int]string
	ruleErr       error
	optErr        error
	// VIP ipset recording fields (§7.14).
	ipsetCreated       []string            // ipset names in creation order
	ipsetEntries       map[string][]string // ipset name → CIDR entries in add order
	ipfilterEnabled    bool                // true if UpdateQemuFirewallOptions{Ipfilter:&true} called
	ipsetCreateErr     error               // if non-nil, returned by CreateQemuFirewallIpset
	ipsetEntryErr      error               // if non-nil, returned by CreateQemuFirewallIpset2
	ipsetCreateErrName string              // if non-empty, only fail CreateQemuFirewallIpset for this name
	// Node-network listing for the §7.39 consume-side bridge gate. nodeIfaces is
	// the set of iface names ListNetwork reports; nodeIfaceErr (when set) forces
	// an error; listNetCalls counts the polls. Only consulted when the gate runs.
	nodeIfaces   []string
	nodeIfaceErr error
	listNetCalls int
}

func (s *fwNodesStub) CreateQemuFirewallRules(_ context.Context, _ string, _ string, p *sdknodes.CreateQemuFirewallRulesParams) error {
	if s.ruleErr != nil {
		return s.ruleErr
	}
	s.ruleActions = append(s.ruleActions, p.Type+":"+p.Action)
	return nil
}

func (s *fwNodesStub) UpdateQemuFirewallOptions(_ context.Context, _ string, _ string, p *sdknodes.UpdateQemuFirewallOptionsParams) error {
	if s.optErr != nil {
		return s.optErr
	}
	if p.Enable != nil && *p.Enable {
		s.enableOptCall++
	}
	// Record ipfilter enable (§7.14).
	if p.Ipfilter != nil && *p.Ipfilter {
		s.ipfilterEnabled = true
	}
	return nil
}

func (s *fwNodesStub) UpdateQemuConfig(_ context.Context, _ string, _ string, p *sdknodes.UpdateQemuConfigParams) error {
	s.lastNet = p.Net
	return nil
}

func (s *fwNodesStub) ListNetwork(_ context.Context, _ string, _ *sdknodes.ListNetworkParams) (*sdknodes.ListNetworkResponse, error) {
	s.listNetCalls++
	if s.nodeIfaceErr != nil {
		return nil, s.nodeIfaceErr
	}
	out := sdknodes.ListNetworkResponse{}
	for _, iface := range s.nodeIfaces {
		out = append(out, json.RawMessage(`{"iface":"`+iface+`","type":"bridge"}`))
	}
	return &out, nil
}

// CreateQemuFirewallIpset records the ipset creation (§7.14).
func (s *fwNodesStub) CreateQemuFirewallIpset(_ context.Context, _ string, _ string, p *sdknodes.CreateQemuFirewallIpsetParams) error {
	if s.ipsetCreateErr != nil {
		// If ipsetCreateErrName is set, only fail for that specific name.
		if s.ipsetCreateErrName == "" || s.ipsetCreateErrName == p.Name {
			return s.ipsetCreateErr
		}
	}
	s.ipsetCreated = append(s.ipsetCreated, p.Name)
	return nil
}

// CreateQemuFirewallIpset2 records the ipset entry addition (§7.14).
func (s *fwNodesStub) CreateQemuFirewallIpset2(_ context.Context, _ string, _ string, name string, p *sdknodes.CreateQemuFirewallIpset2Params) error {
	if s.ipsetEntryErr != nil {
		return s.ipsetEntryErr
	}
	if s.ipsetEntries == nil {
		s.ipsetEntries = make(map[string][]string)
	}
	s.ipsetEntries[name] = append(s.ipsetEntries[name], p.Cidr)
	return nil
}

// fwQEMUStub returns a configurable map from Config(). Used by ip_forwarding
// tests that need specific net{N} strings to verify read-modify-write behavior.
type fwQEMUStub struct {
	qemu.Service
	// configResult is returned by Config() — keys are "net0", "net1" etc.
	configResult map[string]any
	configErr    error
}

func (s *fwQEMUStub) Config(_ context.Context, _ string, _ int) (map[string]any, error) {
	if s.configErr != nil {
		return nil, s.configErr
	}
	if s.configResult != nil {
		return s.configResult, nil
	}
	return map[string]any{}, nil
}

func fwDeps(cl *fwClusterStub, nd *fwNodesStub, cfg *config.CPIConfig) Deps {
	return Deps{
		Config: cfg,
		PVE:    &icPVEClient{clusterSvc: cl, nodesSvc: nd, qemuSvc: &fwQEMUStub{}},
		Agent:  &icAgentStub{},
		Logger: log.NewNopLogger(),
	}
}

// fwDepsWithConfig builds Deps with a QEMU stub that returns vmConfig from Config().
func fwDepsWithConfig(cl *fwClusterStub, nd *fwNodesStub, cfg *config.CPIConfig, vmConfig map[string]any) Deps {
	return Deps{
		Config: cfg,
		PVE:    &icPVEClient{clusterSvc: cl, nodesSvc: nd, qemuSvc: &fwQEMUStub{configResult: vmConfig}},
		Agent:  &icAgentStub{},
		Logger: log.NewNopLogger(),
	}
}

// --------------------------------------------------------------------------
// applySecurityGroups
// --------------------------------------------------------------------------

func TestApplySecurityGroups_AttachesAndEnables(t *testing.T) {
	cl := &fwClusterStub{groups: []string{"web", "db"}}
	nd := &fwNodesStub{}
	err := applySecurityGroups(context.Background(), fwDeps(cl, nd, icMinConfig()), "pve1", 100,
		[]string{"web", "db"}, log.NewNopLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"group:web", "group:db"}
	if len(nd.ruleActions) != len(want) {
		t.Fatalf("ruleActions = %v; want %v", nd.ruleActions, want)
	}
	for i, w := range want {
		if nd.ruleActions[i] != w {
			t.Errorf("ruleActions[%d] = %q; want %q", i, nd.ruleActions[i], w)
		}
	}
	if nd.enableOptCall != 1 {
		t.Errorf("VM-level firewall should be enabled exactly once; got %d", nd.enableOptCall)
	}
}

func TestApplySecurityGroups_MissingGroupErrors(t *testing.T) {
	cl := &fwClusterStub{groups: []string{"web"}}
	nd := &fwNodesStub{}
	err := applySecurityGroups(context.Background(), fwDeps(cl, nd, icMinConfig()), "pve1", 100,
		[]string{"web", "nope"}, log.NewNopLogger())
	if err == nil {
		t.Fatal("expected CloudError for a missing firewall group")
	}
	if !strings.Contains(err.Error(), "nope") {
		t.Errorf("error %q should name the missing group", err.Error())
	}
	if len(nd.ruleActions) != 0 || nd.enableOptCall != 0 {
		t.Errorf("VM must not be mutated when any group is missing; rules=%v enable=%d",
			nd.ruleActions, nd.enableOptCall)
	}
}

func TestApplySecurityGroups_ListErrorWraps(t *testing.T) {
	cl := &fwClusterStub{listErr: errors.New("pve 503")}
	nd := &fwNodesStub{}
	err := applySecurityGroups(context.Background(), fwDeps(cl, nd, icMinConfig()), "pve1", 100,
		[]string{"web"}, log.NewNopLogger())
	if err == nil {
		t.Fatal("expected error when ListFirewallGroups fails")
	}
	if len(nd.ruleActions) != 0 {
		t.Error("no rules should be attached when group listing fails")
	}
}

func TestApplySecurityGroups_RuleErrorStopsBeforeEnable(t *testing.T) {
	cl := &fwClusterStub{groups: []string{"web"}}
	nd := &fwNodesStub{ruleErr: errors.New("pve 500")}
	err := applySecurityGroups(context.Background(), fwDeps(cl, nd, icMinConfig()), "pve1", 100,
		[]string{"web"}, log.NewNopLogger())
	if err == nil {
		t.Fatal("expected error when rule attach fails")
	}
	if nd.enableOptCall != 0 {
		t.Error("VM firewall must not be enabled when a rule attach fails")
	}
}

func TestListFirewallGroupNames(t *testing.T) {
	cl := &fwClusterStub{groups: []string{"alpha", "beta"}}
	got, err := listFirewallGroupNames(context.Background(), fwDeps(cl, &fwNodesStub{}, icMinConfig()))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, name := range []string{"alpha", "beta"} {
		if _, ok := got[name]; !ok {
			t.Errorf("group %q missing from %v", name, got)
		}
	}
	if len(got) != 2 {
		t.Errorf("got %d groups; want 2", len(got))
	}
}

// --------------------------------------------------------------------------
// per-NIC firewall flag in configureNICs
// --------------------------------------------------------------------------

func TestConfigureNICs_FirewallFlag(t *testing.T) {
	cases := []struct {
		name        string
		globalFWSet bool
		globalFW    bool
		nicFWSet    bool
		nicFW       bool
		wantFlag    bool
	}{
		{"global on, nic unset", true, true, false, false, true},
		{"global off (nil), nic unset", false, false, false, false, false},
		{"global off, nic true overrides", false, false, true, true, true},
		{"global on, nic false overrides", true, true, true, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := icMinConfig()
			if tc.globalFWSet {
				v := tc.globalFW
				cfg.VMFirewall = &v
			}
			cp := map[string]any{}
			if tc.nicFWSet {
				cp["firewall"] = tc.nicFW
			}
			parsed := &createVMParsedArgs{
				cloudProps: createVMCloudProps{},
				networks: map[string]createVMNetworkSpec{
					"default": {Type: "dynamic", CloudProperties: cp},
				},
			}
			nd := &fwNodesStub{}
			deps := fwDeps(&fwClusterStub{}, nd, cfg)
			shape := &createVMShape{node: "pve1"}
			if _, err := configureNICs(context.Background(), deps, log.NewNopLogger(), parsed, shape, 100); err != nil {
				t.Fatalf("configureNICs: %v", err)
			}
			got := strings.Contains(nd.lastNet[0], ",firewall=1")
			if got != tc.wantFlag {
				t.Errorf("net0 = %q; firewall flag present=%v want=%v", nd.lastNet[0], got, tc.wantFlag)
			}
		})
	}
}

// --------------------------------------------------------------------------
// enableVMFirewall
// --------------------------------------------------------------------------

func TestEnableVMFirewall_CallsUpdateOptionsOnce(t *testing.T) {
	nd := &fwNodesStub{}
	if err := enableVMFirewall(context.Background(), fwDeps(&fwClusterStub{}, nd, icMinConfig()), "pve1", 200, log.NewNopLogger()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if nd.enableOptCall != 1 {
		t.Errorf("enableOptCall = %d; want 1", nd.enableOptCall)
	}
	if len(nd.ruleActions) != 0 {
		t.Errorf("enableVMFirewall must not attach any group rules; got %v", nd.ruleActions)
	}
}

func TestEnableVMFirewall_PropagatesError(t *testing.T) {
	nd := &fwNodesStub{optErr: errors.New("pve 503")}
	err := enableVMFirewall(context.Background(), fwDeps(&fwClusterStub{}, nd, icMinConfig()), "pve1", 200, log.NewNopLogger())
	if err == nil {
		t.Fatal("expected error when UpdateQemuFirewallOptions fails")
	}
}

// --------------------------------------------------------------------------
// applySecurityGroups uses enableVMFirewall — verify no double-enable
// --------------------------------------------------------------------------

func TestApplySecurityGroups_EnableCalledExactlyOnce(t *testing.T) {
	// Two groups: applySecurityGroups internally calls enableVMFirewall once.
	cl := &fwClusterStub{groups: []string{"app", "db"}}
	nd := &fwNodesStub{}
	if err := applySecurityGroups(context.Background(), fwDeps(cl, nd, icMinConfig()), "pve1", 300,
		[]string{"app", "db"}, log.NewNopLogger()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if nd.enableOptCall != 1 {
		t.Errorf("VM firewall must be enabled exactly once even with multiple groups; got %d", nd.enableOptCall)
	}
}

// --------------------------------------------------------------------------
// resolveEffectiveSecurityGroups
// --------------------------------------------------------------------------

func TestResolveEffectiveSecurityGroups_CallBeatsProfile(t *testing.T) {
	cfg := icMinConfig()
	cfg.VMTypes = map[string]config.TypeProfile{
		"web": {CloudProperties: map[string]any{"security_groups": []any{"from-profile"}}},
	}
	callCP := map[string]any{"vm_type": "web", "security_groups": []any{"from-call"}}
	got, err := resolveEffectiveSecurityGroups(callCP, cfg, []string{"from-call"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0] != "from-call" {
		t.Errorf("got %v; want [from-call]", got)
	}
}

func TestResolveEffectiveSecurityGroups_ProfileUsedWhenCallEmpty(t *testing.T) {
	cfg := icMinConfig()
	cfg.VMTypes = map[string]config.TypeProfile{
		"web": {CloudProperties: map[string]any{"security_groups": []any{"from-profile"}}},
	}
	callCP := map[string]any{"vm_type": "web"}
	got, err := resolveEffectiveSecurityGroups(callCP, cfg, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0] != "from-profile" {
		t.Errorf("got %v; want [from-profile]", got)
	}
}

func TestResolveEffectiveSecurityGroups_GlobalDefaultFallback(t *testing.T) {
	cfg := icMinConfig()
	cfg.SecurityGroups = []string{"global-default"}
	callCP := map[string]any{}
	got, err := resolveEffectiveSecurityGroups(callCP, cfg, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0] != "global-default" {
		t.Errorf("got %v; want [global-default]", got)
	}
}

func TestResolveEffectiveSecurityGroups_AllEmptyReturnsNil(t *testing.T) {
	cfg := icMinConfig()
	callCP := map[string]any{}
	got, err := resolveEffectiveSecurityGroups(callCP, cfg, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %v; want empty", got)
	}
}

func TestResolveEffectiveSecurityGroups_UnknownVMTypeErrors(t *testing.T) {
	cfg := icMinConfig()
	callCP := map[string]any{"vm_type": "does-not-exist"}
	_, err := resolveEffectiveSecurityGroups(callCP, cfg, nil)
	if err == nil {
		t.Fatal("expected CloudError for unknown vm_type")
	}
	if !strings.Contains(err.Error(), "does-not-exist") {
		t.Errorf("error %q should name the unknown selector", err.Error())
	}
}

// --------------------------------------------------------------------------
// resolveEffectiveFirewall
// --------------------------------------------------------------------------

func TestResolveEffectiveFirewall_CallCPTrue(t *testing.T) {
	cfg := icMinConfig()
	callCP := map[string]any{"firewall": true}
	got, err := resolveEffectiveFirewall(callCP, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got {
		t.Error("expected firewall=true from call cloud_properties")
	}
}

func TestResolveEffectiveFirewall_CallCPFalseExplicit(t *testing.T) {
	cfg := icMinConfig()
	v := true
	cfg.VMFirewall = &v
	callCP := map[string]any{"firewall": false}
	got, err := resolveEffectiveFirewall(callCP, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got {
		t.Error("explicit false in call cloud_properties must override config true")
	}
}

func TestResolveEffectiveFirewall_ProfileTrue(t *testing.T) {
	cfg := icMinConfig()
	cfg.VMTypes = map[string]config.TypeProfile{
		"secure": {CloudProperties: map[string]any{"firewall": true}},
	}
	callCP := map[string]any{"vm_type": "secure"}
	got, err := resolveEffectiveFirewall(callCP, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got {
		t.Error("expected firewall=true from vm_type profile")
	}
}

func TestResolveEffectiveFirewall_ConfigDefault(t *testing.T) {
	cfg := icMinConfig()
	v := true
	cfg.VMFirewall = &v
	callCP := map[string]any{}
	got, err := resolveEffectiveFirewall(callCP, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got {
		t.Error("expected firewall=true from config.VMFirewall")
	}
}

func TestResolveEffectiveFirewall_NilConfigDefaultIsFalse(t *testing.T) {
	cfg := icMinConfig()
	// VMFirewall deliberately nil — ensure no firewall calls would occur.
	callCP := map[string]any{}
	got, err := resolveEffectiveFirewall(callCP, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got {
		t.Error("nil VMFirewall config must return false (zero firewall API calls)")
	}
}

// --------------------------------------------------------------------------
// nicIPForwardingEnabled
// --------------------------------------------------------------------------

func TestNicIPForwardingEnabled(t *testing.T) {
	cases := []struct {
		name string
		cp   map[string]any
		want bool
	}{
		{"nil map", nil, false},
		{"absent key", map[string]any{}, false},
		{"false explicit", map[string]any{"ip_forwarding": false}, false},
		{"true", map[string]any{"ip_forwarding": true}, true},
		{"wrong type string", map[string]any{"ip_forwarding": "true"}, false},
		{"wrong type int", map[string]any{"ip_forwarding": 1}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := nicIPForwardingEnabled(tc.cp); got != tc.want {
				t.Errorf("nicIPForwardingEnabled(%v) = %v; want %v", tc.cp, got, tc.want)
			}
		})
	}
}

// --------------------------------------------------------------------------
// patchNICFirewallToken
// --------------------------------------------------------------------------

func TestPatchNICFirewallToken(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		enabled bool
		want    string
	}{
		{
			"strips firewall=1 and appends firewall=0",
			"virtio=aa:bb:cc:dd:ee:ff,bridge=vmbr0,firewall=1",
			false,
			"virtio=aa:bb:cc:dd:ee:ff,bridge=vmbr0,firewall=0",
		},
		{
			"adds firewall=0 when not present",
			"virtio=aa:bb:cc:dd:ee:ff,bridge=vmbr0",
			false,
			"virtio=aa:bb:cc:dd:ee:ff,bridge=vmbr0,firewall=0",
		},
		{
			"sets firewall=1",
			"virtio=aa:bb:cc:dd:ee:ff,bridge=vmbr0",
			true,
			"virtio=aa:bb:cc:dd:ee:ff,bridge=vmbr0,firewall=1",
		},
		{
			"replaces existing firewall=0 with firewall=1",
			"virtio=aa:bb:cc:dd:ee:ff,bridge=vmbr0,firewall=0",
			true,
			"virtio=aa:bb:cc:dd:ee:ff,bridge=vmbr0,firewall=1",
		},
		{
			"empty string gets firewall=0",
			"",
			false,
			"firewall=0",
		},
		{
			"preserves queues token",
			"virtio=aa:bb,bridge=vmbr0,firewall=1,queues=4",
			false,
			"virtio=aa:bb,bridge=vmbr0,queues=4,firewall=0",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := patchNICFirewallToken(tc.input, tc.enabled)
			if got != tc.want {
				t.Errorf("patchNICFirewallToken(%q, %v) = %q; want %q", tc.input, tc.enabled, got, tc.want)
			}
		})
	}
}

// --------------------------------------------------------------------------
// applyIPForwarding — byte-identical when no NIC has ip_forwarding=true
// --------------------------------------------------------------------------

func TestApplyIPForwarding_NoForwardingNICsNoCalls(t *testing.T) {
	nd := &fwNodesStub{}
	networks := map[string]createVMNetworkSpec{
		"default": {Type: "manual", IP: "10.0.0.1", CloudProperties: map[string]any{}},
		"second":  {Type: "dynamic", CloudProperties: map[string]any{"ip_forwarding": false}},
	}
	err := applyIPForwarding(context.Background(), fwDeps(&fwClusterStub{}, nd, icMinConfig()), "pve1", 100, networks, log.NewNopLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if nd.lastNet != nil {
		t.Errorf("expected no UpdateQemuConfig call; got net=%v", nd.lastNet)
	}
}

// --------------------------------------------------------------------------
// applyIPForwarding — read-modify-write: preserves existing NIC tokens
// --------------------------------------------------------------------------

func TestApplyIPForwarding_SetsFirewallZeroOnForwardingNIC(t *testing.T) {
	// Simulate PVE having assigned a MAC after VM start. The full net string
	// including MAC and bridge must be preserved; only firewall token is changed.
	existingNet1 := "virtio=aa:bb:cc:dd:ee:ff,bridge=vmbr0,firewall=1"
	vmConfig := map[string]any{
		"net0": "virtio=11:22:33:44:55:66,bridge=vmbr0",
		"net1": existingNet1,
	}
	nd := &fwNodesStub{}
	networks := map[string]createVMNetworkSpec{
		"default": {Type: "manual", IP: "10.0.0.1", CloudProperties: map[string]any{}},
		// "wan" sorts after "default" → net1.
		"wan": {Type: "dynamic", CloudProperties: map[string]any{"ip_forwarding": true}},
	}
	err := applyIPForwarding(context.Background(),
		fwDepsWithConfig(&fwClusterStub{}, nd, icMinConfig(), vmConfig),
		"pve1", 200, networks, log.NewNopLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// net[1] must carry the full NIC string with firewall=0 (not "firewall=0" alone).
	if nd.lastNet == nil {
		t.Fatal("expected UpdateQemuConfig call; got nil")
	}
	val, ok := nd.lastNet[1]
	if !ok {
		t.Fatalf("expected net[1] in UpdateQemuConfig; got %v", nd.lastNet)
	}
	// The patched string must contain the original MAC and bridge, and firewall=0.
	if !strings.Contains(val, "aa:bb:cc:dd:ee:ff") {
		t.Errorf("net[1] = %q; must preserve MAC address", val)
	}
	if !strings.Contains(val, "bridge=vmbr0") {
		t.Errorf("net[1] = %q; must preserve bridge", val)
	}
	if !strings.Contains(val, "firewall=0") {
		t.Errorf("net[1] = %q; must contain firewall=0", val)
	}
	if strings.Contains(val, "firewall=1") {
		t.Errorf("net[1] = %q; must NOT contain firewall=1", val)
	}
	// net[0] (default, no ip_forwarding) must NOT be in the map.
	if _, has := nd.lastNet[0]; has {
		t.Errorf("net[0] must not be set; got %q", nd.lastNet[0])
	}
}

func TestApplyIPForwarding_MultipleForwardingNICsEachGetUpdate(t *testing.T) {
	// Both NICs have ip_forwarding=true. The stub records the LAST UpdateQemuConfig
	// call; verify the second NIC ("wan") was written with firewall=0.
	vmConfig := map[string]any{
		"net0": "virtio=11:22:33:44:55:00,bridge=vmbr0,firewall=1",
		"net1": "virtio=aa:bb:cc:dd:ee:ff,bridge=vmbr1,firewall=1",
	}
	nd := &fwNodesStub{}
	networks := map[string]createVMNetworkSpec{
		"default": {Type: "dynamic", CloudProperties: map[string]any{"ip_forwarding": true}},
		"wan":     {Type: "dynamic", CloudProperties: map[string]any{"ip_forwarding": true}},
	}
	err := applyIPForwarding(context.Background(),
		fwDepsWithConfig(&fwClusterStub{}, nd, icMinConfig(), vmConfig),
		"pve1", 300, networks, log.NewNopLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// lastNet holds the LAST UpdateQemuConfig call (for "wan" at net1).
	if nd.lastNet == nil {
		t.Fatal("expected UpdateQemuConfig calls; got nil")
	}
	val, ok := nd.lastNet[1]
	if !ok {
		t.Errorf("expected net[1] in last UpdateQemuConfig; got %v", nd.lastNet)
	}
	if !strings.Contains(val, "firewall=0") {
		t.Errorf("net[1] = %q; must contain firewall=0", val)
	}
	if !strings.Contains(val, "aa:bb:cc:dd:ee:ff") {
		t.Errorf("net[1] = %q; must preserve MAC", val)
	}
}

func TestApplyIPForwarding_UpdateQemuConfigErrorPropagates(t *testing.T) {
	// The fwNodesStub always succeeds for UpdateQemuConfig. Use a dedicated
	// stub that fails so the error propagation path is exercised.
	// Supply a non-empty net0 string so the absent-key guard does not skip.
	failNd := &failUpdateQemuConfigStub{}
	networks := map[string]createVMNetworkSpec{
		"default": {Type: "dynamic", CloudProperties: map[string]any{"ip_forwarding": true}},
	}
	vmConfig := map[string]any{"net0": "virtio=aa:bb:cc:dd:ee:ff,bridge=vmbr0,firewall=1"}
	deps := Deps{
		Config: icMinConfig(),
		PVE: &icPVEClient{
			clusterSvc: &fwClusterStub{},
			nodesSvc:   failNd,
			qemuSvc:    &fwQEMUStub{configResult: vmConfig},
		},
		Agent:  &icAgentStub{},
		Logger: log.NewNopLogger(),
	}
	err := applyIPForwarding(context.Background(), deps, "pve1", 400, networks, log.NewNopLogger())
	if err == nil {
		t.Fatal("expected error when UpdateQemuConfig fails")
	}
}

func TestApplyIPForwarding_QEMUConfigReadErrorPropagates(t *testing.T) {
	// If reading the current VM config fails, applyIPForwarding must return an error.
	nd := &fwNodesStub{}
	networks := map[string]createVMNetworkSpec{
		"default": {Type: "dynamic", CloudProperties: map[string]any{"ip_forwarding": true}},
	}
	deps := Deps{
		Config: icMinConfig(),
		PVE: &icPVEClient{
			clusterSvc: &fwClusterStub{},
			nodesSvc:   nd,
			qemuSvc:    &fwQEMUStub{configErr: errors.New("pve: config read failed")},
		},
		Agent:  &icAgentStub{},
		Logger: log.NewNopLogger(),
	}
	err := applyIPForwarding(context.Background(), deps, "pve1", 401, networks, log.NewNopLogger())
	if err == nil {
		t.Fatal("expected error when QEMU Config() fails")
	}
	if nd.lastNet != nil {
		t.Error("no UpdateQemuConfig should be called when config read fails")
	}
}

func TestApplyIPForwarding_AbsentNICKeySkipsWithWarning(t *testing.T) {
	// When PVE Config() returns a map that does not contain net{i} for an
	// ip_forwarding NIC, the function must skip that NIC (warn + continue)
	// rather than write a bare "firewall=0" that would destroy model/bridge.
	nd := &fwNodesStub{}
	// vmConfig deliberately omits net1 (the "wan" NIC).
	vmConfig := map[string]any{
		"net0": "virtio=11:22:33:44:55:66,bridge=vmbr0",
		// net1 absent
	}
	networks := map[string]createVMNetworkSpec{
		"default": {Type: "manual", IP: "10.0.0.1", CloudProperties: map[string]any{}},
		"wan":     {Type: "dynamic", CloudProperties: map[string]any{"ip_forwarding": true}},
	}
	err := applyIPForwarding(context.Background(),
		fwDepsWithConfig(&fwClusterStub{}, nd, icMinConfig(), vmConfig),
		"pve1", 402, networks, log.NewNopLogger())
	// Must not error — guard skips the NIC rather than failing.
	if err != nil {
		t.Fatalf("unexpected error when net key absent: %v", err)
	}
	// Must not call UpdateQemuConfig for the absent NIC.
	if nd.lastNet != nil {
		t.Errorf("UpdateQemuConfig must not be called when net key is absent; got net=%v", nd.lastNet)
	}
}

// failUpdateQemuConfigStub returns an error from UpdateQemuConfig only.
type failUpdateQemuConfigStub struct {
	fwNodesStub
}

func (s *failUpdateQemuConfigStub) UpdateQemuConfig(_ context.Context, _ string, _ string, _ *sdknodes.UpdateQemuConfigParams) error {
	return errors.New("pve: update config failed")
}

// --------------------------------------------------------------------------
// ipfilter exclusion for ip_forwarding NICs (integration with applyVIPAllowedAddressPairs)
// --------------------------------------------------------------------------

func TestApplyVIPAllowedAddressPairs_IPForwardingNICExcludedFromIpfilter(t *testing.T) {
	// NIC "default" is static with VIPs; NIC "wan" has ip_forwarding=true and no VIPs.
	// Only "default" should have an ipset seeded. "wan" must be excluded from fwCount.
	nd := &fwNodesStub{nodeIfaces: []string{"vmbr0"}}
	networks := map[string]createVMNetworkSpec{
		"default": {
			Type: "manual", IP: "10.0.0.1", Netmask: "255.255.255.0",
			CloudProperties: map[string]any{
				"allowed_address_pairs": []any{"10.0.0.100"},
			},
		},
		"wan": {
			Type: "dynamic",
			CloudProperties: map[string]any{
				"ip_forwarding": true,
				"firewall":      true, // firewall=true but ip_forwarding overrides
			},
		},
	}
	cfg := icMinConfig()
	fwTrue := true
	cfg.VMFirewall = &fwTrue

	err := applyVIPAllowedAddressPairs(context.Background(), fwDeps(&fwClusterStub{}, nd, cfg), "pve1", 500, networks, log.NewNopLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Only ipfilter-net0 (for "default") should exist.
	if len(nd.ipsetCreated) != 1 || nd.ipsetCreated[0] != "ipfilter-net0" {
		t.Errorf("ipsetCreated = %v; want [ipfilter-net0]", nd.ipsetCreated)
	}
	// wan (net1) must NOT have an ipset.
	if _, has := nd.ipsetEntries["ipfilter-net1"]; has {
		t.Errorf("ip_forwarding NIC (net1) must not have an ipset")
	}
}
