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
