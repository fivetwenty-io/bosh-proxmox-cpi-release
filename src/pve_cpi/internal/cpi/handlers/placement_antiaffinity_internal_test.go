// Package handlers internal tests for PVE HA anti-affinity rule membership.
package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cluster"
)

// aaClusterStub is an in-memory cluster.Service tracking HA resources and rules.
// Non-overridden methods panic via the embedded nil interface.
type aaClusterStub struct {
	cluster.Service
	resources       map[string]bool   // sid -> registered
	rules           map[string]string // ruleName -> resources CSV
	listResourcesFn func() *cluster.ListResourcesResponse
	createRuleCalls int
	deleteRuleCalls int
	failCreateRule  bool
	failListRules   bool
	// events, when non-nil, records an ordered op log shared with a fake pool
	// service so lock/RMW ordering can be asserted across both services.
	events *[]string
	// dropMemberOnRecreate, when non-empty, simulates a concurrent writer: on the
	// next CreateHaRules the named sid is removed from the persisted CSV, so a
	// read-after-write verify observes the lost member.
	dropMemberOnRecreate string
}

func (s *aaClusterStub) record(ev string) {
	if s.events != nil {
		*s.events = append(*s.events, ev)
	}
}

var _ cluster.Service = (*aaClusterStub)(nil)

func newAAStub() *aaClusterStub {
	return &aaClusterStub{resources: map[string]bool{}, rules: map[string]string{}}
}

func (s *aaClusterStub) ListResources(_ context.Context, _ *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
	s.record("list-resources")
	if s.listResourcesFn != nil {
		return s.listResourcesFn(), nil
	}
	empty := cluster.ListResourcesResponse{}
	return &empty, nil
}

func (s *aaClusterStub) CreateHaResources(_ context.Context, p *cluster.CreateHaResourcesParams) error {
	s.resources[p.Sid] = true
	return nil
}

func (s *aaClusterStub) DeleteHaResources(_ context.Context, sid string, _ *cluster.DeleteHaResourcesParams) error {
	delete(s.resources, sid)
	return nil
}

func (s *aaClusterStub) ListHaRules(_ context.Context, _ *cluster.ListHaRulesParams) (*cluster.ListHaRulesResponse, error) {
	s.record("list-rules")
	if s.failListRules {
		return nil, fmt.Errorf("ha not configured on this cluster")
	}
	out := cluster.ListHaRulesResponse{}
	for name, csv := range s.rules {
		raw, _ := json.Marshal(map[string]any{
			"rule":      name,
			"type":      haRuleType,
			"affinity":  haRuleAffinity,
			"resources": csv,
		})
		out = append(out, raw)
	}
	return &out, nil
}

func (s *aaClusterStub) CreateHaRules(_ context.Context, p *cluster.CreateHaRulesParams) error {
	s.record("create-rule:" + p.Rule)
	s.createRuleCalls++
	if s.failCreateRule {
		return fmt.Errorf("create rule failed")
	}
	resources := p.Resources
	if s.dropMemberOnRecreate != "" {
		// Simulate a concurrent writer dropping a member from the persisted rule.
		resources = dropSidFromCSV(resources, s.dropMemberOnRecreate)
		s.dropMemberOnRecreate = ""
	}
	s.rules[p.Rule] = resources
	return nil
}

func (s *aaClusterStub) DeleteHaRules(_ context.Context, rule string) error {
	s.record("delete-rule:" + rule)
	s.deleteRuleCalls++
	delete(s.rules, rule)
	return nil
}

// dropSidFromCSV returns csv with sid removed, used to simulate a concurrent
// lost-update in read-after-write verify tests.
func dropSidFromCSV(csv, sid string) string {
	parts := strings.Split(csv, ",")
	out := parts[:0]
	for _, p := range parts {
		if p != sid {
			out = append(out, p)
		}
	}
	return strings.Join(out, ",")
}

// aaQEMU builds a /cluster/resources qemu entry with a vmid and tags string.
func aaQEMU(vmid int, tags string) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{"type": "qemu", "vmid": vmid, "tags": tags})
	return raw
}

func aaResourcesFn(entries ...json.RawMessage) func() *cluster.ListResourcesResponse {
	resp := cluster.ListResourcesResponse(entries)
	return func() *cluster.ListResourcesResponse { return &resp }
}

func aaDeps(stub *aaClusterStub) Deps {
	return Deps{
		Config: icMinConfig(),
		PVE:    &icPVEClient{clusterSvc: stub},
		Agent:  &icAgentStub{},
		Logger: log.NewNopLogger(),
	}
}

// --------------------------------------------------------------------------
// ensureAntiAffinityMembership
// --------------------------------------------------------------------------

func TestEnsureAntiAffinity_CreatesRuleWithTwoMembers(t *testing.T) {
	stub := newAAStub()
	// One existing same-group member already on the cluster.
	stub.listResourcesFn = aaResourcesFn(aaQEMU(100, "deployment--cf;job--web;index--0"))

	if err := ensureAntiAffinityMembership(context.Background(), aaDeps(stub), "web", 101, log.NewNopLogger()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := stub.rules["bosh-aa-web"]; got != "vm:100,vm:101" {
		t.Errorf("rule resources = %q; want %q", got, "vm:100,vm:101")
	}
	if !stub.resources["vm:101"] {
		t.Error("vm:101 should be registered as an HA resource")
	}
}

func TestEnsureAntiAffinity_SingleMemberNoRule(t *testing.T) {
	stub := newAAStub() // no existing same-group VMs
	if err := ensureAntiAffinityMembership(context.Background(), aaDeps(stub), "web", 100, log.NewNopLogger()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(stub.rules) != 0 {
		t.Errorf("no rule expected with a single member; got %v", stub.rules)
	}
	if !stub.resources["vm:100"] {
		t.Error("vm:100 should still be registered as an HA resource")
	}
}

func TestEnsureAntiAffinity_IdempotentNoRecreate(t *testing.T) {
	stub := newAAStub()
	stub.rules["bosh-aa-web"] = "vm:100,vm:101"
	stub.listResourcesFn = aaResourcesFn(
		aaQEMU(100, "job--web"),
		aaQEMU(101, "job--web"),
	)
	if err := ensureAntiAffinityMembership(context.Background(), aaDeps(stub), "web", 101, log.NewNopLogger()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stub.createRuleCalls != 0 || stub.deleteRuleCalls != 0 {
		t.Errorf("membership unchanged should not recreate rule; create=%d delete=%d",
			stub.createRuleCalls, stub.deleteRuleCalls)
	}
}

func TestEnsureAntiAffinity_AppendRecreatesRule(t *testing.T) {
	stub := newAAStub()
	stub.rules["bosh-aa-web"] = "vm:100,vm:101"
	// A third member now exists; ensuring the newcomer must grow the rule.
	stub.listResourcesFn = aaResourcesFn(
		aaQEMU(100, "job--web"),
		aaQEMU(101, "job--web"),
	)
	if err := ensureAntiAffinityMembership(context.Background(), aaDeps(stub), "web", 102, log.NewNopLogger()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := stub.rules["bosh-aa-web"]; got != "vm:100,vm:101,vm:102" {
		t.Errorf("rule resources = %q; want %q", got, "vm:100,vm:101,vm:102")
	}
	if stub.createRuleCalls != 1 || stub.deleteRuleCalls != 1 {
		t.Errorf("expected one delete+create recreate; create=%d delete=%d",
			stub.createRuleCalls, stub.deleteRuleCalls)
	}
}

func TestEnsureAntiAffinity_EmptyGroupNoOp(t *testing.T) {
	stub := newAAStub()
	if err := ensureAntiAffinityMembership(context.Background(), aaDeps(stub), "", 100, log.NewNopLogger()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(stub.resources) != 0 || len(stub.rules) != 0 {
		t.Error("empty group key must be a no-op")
	}
}

func TestEnsureAntiAffinity_ListRulesFailureReturnsError(t *testing.T) {
	stub := newAAStub()
	stub.failListRules = true
	stub.listResourcesFn = aaResourcesFn(aaQEMU(100, "job--web"))
	err := ensureAntiAffinityMembership(context.Background(), aaDeps(stub), "web", 101, log.NewNopLogger())
	if err == nil {
		t.Fatal("expected error when ListHaRules fails (caller logs it best-effort)")
	}
}

// --------------------------------------------------------------------------
// removeAntiAffinityMembership
// --------------------------------------------------------------------------

func TestRemoveAntiAffinity_RecreatesWithoutVMID(t *testing.T) {
	stub := newAAStub()
	stub.rules["bosh-aa-web"] = "vm:100,vm:101,vm:102"
	if err := removeAntiAffinityMembership(context.Background(), aaDeps(stub), 101, log.NewNopLogger()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := stub.rules["bosh-aa-web"]; got != "vm:100,vm:102" {
		t.Errorf("rule resources = %q; want %q", got, "vm:100,vm:102")
	}
}

func TestRemoveAntiAffinity_DeletesWhenSingleMemberRemains(t *testing.T) {
	stub := newAAStub()
	stub.rules["bosh-aa-web"] = "vm:100,vm:101"
	if err := removeAntiAffinityMembership(context.Background(), aaDeps(stub), 101, log.NewNopLogger()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := stub.rules["bosh-aa-web"]; ok {
		t.Errorf("rule should be deleted when only one member remains; got %v", stub.rules)
	}
}

func TestRemoveAntiAffinity_NonMemberNoOp(t *testing.T) {
	stub := newAAStub()
	stub.rules["bosh-aa-web"] = "vm:100,vm:102"
	if err := removeAntiAffinityMembership(context.Background(), aaDeps(stub), 999, log.NewNopLogger()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := stub.rules["bosh-aa-web"]; got != "vm:100,vm:102" {
		t.Errorf("rule should be unchanged for a non-member; got %q", got)
	}
}

func TestRemoveAntiAffinity_IgnoresForeignRules(t *testing.T) {
	stub := newAAStub()
	stub.rules["operator-custom-rule"] = "vm:100,vm:101"
	if err := removeAntiAffinityMembership(context.Background(), aaDeps(stub), 101, log.NewNopLogger()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := stub.rules["operator-custom-rule"]; got != "vm:100,vm:101" {
		t.Errorf("non bosh-aa rule must not be touched; got %q", got)
	}
}

// --------------------------------------------------------------------------
// helpers
// --------------------------------------------------------------------------

func TestParseHaResources_Forms(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want []string
	}{
		{"csv", `"vm:100,vm:101"`, []string{"vm:100", "vm:101"}},
		{"array", `["vm:100","vm:101"]`, []string{"vm:100", "vm:101"}},
		{"object", `{"vm:100":{},"vm:101":{}}`, []string{"vm:100", "vm:101"}},
		{"empty", ``, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseHaResources(json.RawMessage(tc.raw))
			if len(got) != len(tc.want) {
				t.Fatalf("len = %d; want %d (%v)", len(got), len(tc.want), got)
			}
			for _, w := range tc.want {
				if _, ok := got[w]; !ok {
					t.Errorf("missing %q in %v", w, got)
				}
			}
		})
	}
}

func TestSidsCSV_Deterministic(t *testing.T) {
	in := map[string]struct{}{"vm:102": {}, "vm:100": {}, "vm:101": {}}
	if got := sidsCSV(in); got != "vm:100,vm:101,vm:102" {
		t.Errorf("sidsCSV = %q; want sorted CSV", got)
	}
}

func TestTagsContain(t *testing.T) {
	cases := []struct {
		tags string
		want string
		ok   bool
	}{
		{"deployment--cf;job--web;index--0", "job--web", true},
		{"deployment--cf,job--web,index--0", "job--web", true},
		{"job--web-worker", "job--web", false}, // exact match only
		{"", "job--web", false},
		{"job--web", "", false},
	}
	for _, tc := range cases {
		if got := tagsContain(tc.tags, tc.want); got != tc.ok {
			t.Errorf("tagsContain(%q,%q) = %v; want %v", tc.tags, tc.want, got, tc.ok)
		}
	}
}

func TestAntiAffinityGroupTag(t *testing.T) {
	env := map[string]any{
		"bosh": map[string]any{
			"group":  "director-cf-web",
			"groups": []any{"director", "cf", "web", "director-cf", "director-cf-web"},
		},
	}
	// Disabled config -> empty tag.
	if got := antiAffinityGroupTag(icMinConfig(), env); got != "" {
		t.Errorf("disabled anti-affinity should yield empty tag; got %q", got)
	}
	// Enabled -> job-derived tag.
	cfg := icMinConfig()
	enabled := true
	cfg.Placement = &config.PlacementConfig{
		AntiAffinity: &config.AntiAffinityConfig{Enabled: &enabled},
	}
	if got := antiAffinityGroupTag(cfg, env); got != "job--web" {
		t.Errorf("antiAffinityGroupTag = %q; want %q", got, "job--web")
	}
}
