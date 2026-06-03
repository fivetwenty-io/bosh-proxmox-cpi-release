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
)

// fwClusterStubWithFirewall is a compile-time reminder that fwClusterStub already
// satisfies both the groups-present and groups-absent cases used by the new tests.

// fwClusterStub provides ListFirewallGroups; other cluster methods panic via the
// embedded nil interface.
type fwClusterStub struct {
	sdkcluster.Service
	groups  []string
	listErr error
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

// fwNodesStub records firewall rule + option calls and the last NIC net map
// applied via UpdateQemuConfig. Other nodes methods panic via the nil embed.
type fwNodesStub struct {
	sdknodes.Service
	ruleActions   []string // "<type>:<action>" per CreateQemuFirewallRules call
	enableOptCall int      // count of UpdateQemuFirewallOptions{Enable:true}
	lastNet       map[int]string
	ruleErr       error
	optErr        error
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
	return nil
}

func (s *fwNodesStub) UpdateQemuConfig(_ context.Context, _ string, _ string, p *sdknodes.UpdateQemuConfigParams) error {
	s.lastNet = p.Net
	return nil
}

func fwDeps(cl *fwClusterStub, nd *fwNodesStub, cfg *config.CPIConfig) Deps {
	return Deps{
		Config: cfg,
		PVE:    &icPVEClient{clusterSvc: cl, nodesSvc: nd},
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
