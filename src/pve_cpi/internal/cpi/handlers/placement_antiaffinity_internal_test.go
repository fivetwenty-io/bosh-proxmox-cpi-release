// Package handlers internal tests for PVE HA anti-affinity rule membership.
package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
)

// aaClusterStub is an in-memory cluster.Service tracking HA resources and rules.
// Non-overridden methods panic via the embedded nil interface.
type aaClusterStub struct {
	cluster.Service
	resources    map[string]bool   // sid -> registered
	rules        map[string]string // ruleName -> resources CSV
	ruleComments map[string]string // ruleName -> comment
	// ruleTypes, ruleNodes, and ruleStrict hold what a node-affinity rule
	// adds; a rule absent from ruleTypes is a negative resource-affinity rule.
	ruleTypes       map[string]string
	ruleNodes       map[string]string
	ruleStrict      map[string]bool
	resComments     map[string]string // sid -> comment
	listResourcesFn func() *cluster.ListResourcesResponse
	createRuleCalls int
	updateRuleCalls int
	deleteRuleCalls int
	failCreateRule  bool
	failUpdateRule  bool
	// digest changes on every rule write, the way PVE's config digest does.
	digest        int
	failListRules bool
	// events, when non-nil, records an ordered op log shared with a fake pool
	// service so lock/RMW ordering can be asserted across both services.
	events *[]string
	// memberNodes overrides the corosync membership reported by
	// ListConfigNodes; nil means the single default node "pve1".
	memberNodes []string
	// statusFn scripts the /cluster/status response; nil means fully online.
	statusFn func() (*cluster.ListStatusResponse, error)
	// listRulesCalls counts ListHaRules calls, and beforeListRules, when set,
	// runs before each one with its 1-based number, so a test can change a
	// rule between two reads the way a concurrent writer would.
	listRulesCalls  int
	beforeListRules func(call int)
	// getResErr, when non-nil, makes GetHaResources fail with it.
	getResErr error
}

func (s *aaClusterStub) record(ev string) {
	if s.events != nil {
		*s.events = append(*s.events, ev)
	}
}

var _ cluster.Service = (*aaClusterStub)(nil)

func newAAStub() *aaClusterStub {
	return &aaClusterStub{
		resources: map[string]bool{}, rules: map[string]string{},
		ruleComments: map[string]string{}, resComments: map[string]string{},
		ruleTypes: map[string]string{}, ruleNodes: map[string]string{}, ruleStrict: map[string]bool{},
	}
}

// seedRule stores a rule owned by the test Director, the way the CPI writes it.
func (s *aaClusterStub) seedRule(name, csv string) {
	s.seedRuleWithComment(name, csv, aaIdent("web").marker())
}

// seedRuleWithComment stores a rule with an exact comment, such as an old
// CPI's unmarked one or another Director's marker.
func (s *aaClusterStub) seedRuleWithComment(name, csv, comment string) {
	s.rules[name] = csv
	s.ruleComments[name] = comment
}

func (s *aaClusterStub) ListResources(_ context.Context, _ *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
	s.record("list-resources")
	if s.listResourcesFn != nil {
		return s.listResourcesFn(), nil
	}
	empty := cluster.ListResourcesResponse{}
	return &empty, nil
}

// ListConfigNodes reports memberNodes, defaulting to the single test node
// "pve1": every aa fixture row is node-less, and ListGuestsAuthoritative
// needs a non-empty membership.
func (s *aaClusterStub) ListConfigNodes(_ context.Context) (*cluster.ListConfigNodesResponse, error) {
	names := s.memberNodes
	if len(names) == 0 {
		names = []string{"pve1"}
	}
	out := make(cluster.ListConfigNodesResponse, 0, len(names))
	for _, n := range names {
		raw, _ := json.Marshal(map[string]any{"name": n})
		out = append(out, raw)
	}
	return &out, nil
}

func (s *aaClusterStub) CreateHaResources(_ context.Context, p *cluster.CreateHaResourcesParams) error {
	s.resources[p.Sid] = true
	if p.Comment != nil {
		s.resComments[p.Sid] = *p.Comment
	}
	return nil
}

func (s *aaClusterStub) GetHaResources(_ context.Context, sid string) (*cluster.GetHaResourcesResponse, error) {
	if s.getResErr != nil {
		return nil, s.getResErr
	}
	if !s.resources[sid] {
		return nil, fmt.Errorf("no such resource")
	}
	resp := &cluster.GetHaResourcesResponse{}
	if c, ok := s.resComments[sid]; ok {
		resp.Comment = &c
	}
	return resp, nil
}

func (s *aaClusterStub) DeleteHaResources(_ context.Context, sid string, _ *cluster.DeleteHaResourcesParams) error {
	delete(s.resources, sid)
	return nil
}

func (s *aaClusterStub) ListHaRules(_ context.Context, _ *cluster.ListHaRulesParams) (*cluster.ListHaRulesResponse, error) {
	s.record("list-rules")
	s.listRulesCalls++
	if s.beforeListRules != nil {
		s.beforeListRules(s.listRulesCalls)
	}
	if s.failListRules {
		return nil, fmt.Errorf("ha not configured on this cluster")
	}
	out := cluster.ListHaRulesResponse{}
	for name, csv := range s.rules {
		row := map[string]any{
			"rule":      name,
			"type":      haRuleType,
			"resources": csv,
			"comment":   s.ruleComments[name],
			"digest":    fmt.Sprint(s.digest),
		}
		if typ, ok := s.ruleTypes[name]; ok {
			row["type"] = typ
		} else {
			row["affinity"] = haRuleAffinity
		}
		if nodes, ok := s.ruleNodes[name]; ok {
			row["nodes"] = nodes
		}
		if s.ruleStrict[name] {
			row["strict"] = 1
		}
		raw, _ := json.Marshal(row)
		out = append(out, raw)
	}
	return &out, nil
}

// dropOnRead makes every rule read from the call-th on lose sid, the way a
// concurrent writer that removed it after this create's writes would.
func (s *aaClusterStub) dropOnRead(call int, sid string) {
	s.beforeListRules = func(n int) {
		if n < call {
			return
		}
		for name, csv := range s.rules {
			s.rules[name] = dropSidFromCSV(csv, sid)
		}
	}
}

func (s *aaClusterStub) CreateHaRules(_ context.Context, p *cluster.CreateHaRulesParams) error {
	s.record("create-rule:" + p.Rule)
	s.createRuleCalls++
	if s.failCreateRule {
		return fmt.Errorf("create rule failed")
	}
	s.rules[p.Rule] = p.Resources
	if p.Comment != nil {
		s.ruleComments[p.Rule] = *p.Comment
	}
	if p.Type != haRuleType {
		s.ruleTypes[p.Rule] = p.Type
	}
	if p.Nodes != nil {
		s.ruleNodes[p.Rule] = *p.Nodes
	}
	if p.Strict != nil {
		s.ruleStrict[p.Rule] = *p.Strict
	}
	s.digest++
	return nil
}

func (s *aaClusterStub) UpdateHaRules(_ context.Context, rule string, p *cluster.UpdateHaRulesParams) error {
	s.record("update-rule:" + rule)
	s.updateRuleCalls++
	if s.failUpdateRule {
		return fmt.Errorf("update rule failed")
	}
	if _, ok := s.rules[rule]; !ok {
		return fmt.Errorf("no such ha rule '%s'", rule)
	}
	if p.Digest != nil && *p.Digest != fmt.Sprint(s.digest) {
		return errors.New("detected modified configuration - file changed by other user")
	}
	if p.Resources != nil {
		s.rules[rule] = *p.Resources
	}
	if p.Comment != nil {
		s.ruleComments[rule] = *p.Comment
	}
	if p.Nodes != nil {
		s.ruleNodes[rule] = *p.Nodes
	}
	if p.Strict != nil {
		s.ruleStrict[rule] = *p.Strict
	}
	s.digest++
	return nil
}

func (s *aaClusterStub) DeleteHaRules(_ context.Context, rule string) error {
	s.record("delete-rule:" + rule)
	s.deleteRuleCalls++
	delete(s.rules, rule)
	delete(s.ruleComments, rule)
	delete(s.ruleTypes, rule)
	delete(s.ruleNodes, rule)
	delete(s.ruleStrict, rule)
	s.digest++
	return nil
}

// dropSidFromCSV returns csv with sid removed, used to simulate a concurrent
// lost update in read-after-write verify tests.
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
		Config:              icMinConfig(),
		RequestDirectorUUID: aaDirector,
		HAProfile:           pve9Profile,
		PVE: &icPVEClient{
			clusterSvc: stub,
			nodesSvc: &icNodesService{listFn: func(ctx context.Context, p *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
				return stub.ListResources(ctx, p)
			}},
		},
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
	stub.listResourcesFn = aaResourcesFn(aaQEMU(100, aaTag+";index--0"))

	if err := ensureAntiAffinityMembership(context.Background(), aaDeps(stub), aaIdent("web"), 101, log.NewNopLogger()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := stub.rules[aaRule]; got != "vm:100,vm:101" {
		t.Errorf("rule resources = %q; want %q", got, "vm:100,vm:101")
	}
	if !stub.resources["vm:101"] {
		t.Error("vm:101 should be registered as an HA resource")
	}
}

func TestEnsureAntiAffinity_SingleMemberNoRule(t *testing.T) {
	stub := newAAStub() // no existing same-group VMs
	if err := ensureAntiAffinityMembership(context.Background(), aaDeps(stub), aaIdent("web"), 100, log.NewNopLogger()); err != nil {
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
	stub.seedRule(aaRule, "vm:100,vm:101")
	stub.listResourcesFn = aaResourcesFn(
		aaQEMU(100, aaTag),
		aaQEMU(101, aaTag),
	)
	if err := ensureAntiAffinityMembership(context.Background(), aaDeps(stub), aaIdent("web"), 101, log.NewNopLogger()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stub.createRuleCalls != 0 || stub.deleteRuleCalls != 0 {
		t.Errorf("membership unchanged should not recreate rule; create=%d delete=%d",
			stub.createRuleCalls, stub.deleteRuleCalls)
	}
}

func TestEnsureAntiAffinity_AppendUpdatesRuleInPlace(t *testing.T) {
	stub := newAAStub()
	stub.seedRule(aaRule, "vm:100,vm:101")
	// A third member now exists; ensuring the newcomer must grow the rule.
	stub.listResourcesFn = aaResourcesFn(
		aaQEMU(100, aaTag),
		aaQEMU(101, aaTag),
	)
	if err := ensureAntiAffinityMembership(context.Background(), aaDeps(stub), aaIdent("web"), 102, log.NewNopLogger()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := stub.rules[aaRule]; got != "vm:100,vm:101,vm:102" {
		t.Errorf("rule resources = %q; want %q", got, "vm:100,vm:101,vm:102")
	}
	// The rule changes with one PUT and is never deleted, so there is no
	// window in which the group has no rule.
	if stub.updateRuleCalls != 1 || stub.createRuleCalls != 0 || stub.deleteRuleCalls != 0 {
		t.Errorf("expected one in-place update; update=%d create=%d delete=%d",
			stub.updateRuleCalls, stub.createRuleCalls, stub.deleteRuleCalls)
	}
}

// TestEnsureAntiAffinity_StaleScanNeverShrinksRule pins the never-shrink
// contract: the persisted HA rule carries a member (vm:200) whose guest
// exists but does not carry the group tag yet (a same-group twin mid-create;
// tags land after the create commits). Ensuring a newcomer must union the
// existing rule's members into the scanned set; the removal path is the sole
// shrink authority, so the rewritten rule keeps vm:200.
func TestEnsureAntiAffinity_StaleScanNeverShrinksRule(t *testing.T) {
	stub := newAAStub()
	stub.seedRule(aaRule, "vm:100,vm:200")
	// The scan sees vm:100 tagged; vm:200 is a live guest with no tag yet.
	stub.listResourcesFn = aaResourcesFn(aaQEMU(100, aaTag), aaQEMU(200, ""))

	if err := ensureAntiAffinityMembership(context.Background(), aaDeps(stub), aaIdent("web"), 101, log.NewNopLogger()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := stub.rules[aaRule]; got != "vm:100,vm:101,vm:200" {
		t.Errorf("rule resources = %q; want %q (vm:200 must survive the tag-stale scan)", got, "vm:100,vm:101,vm:200")
	}
}

// TestEnsureAntiAffinity_DeadRuleMemberDropped is the counterpart bound: a
// rule member whose guest exists nowhere in the fleet (destroyed outside the
// CPI, or a failed delete_vm HA cleanup) must not be resurrected into the
// recreated rule, because PVE no longer has that resource and a rule naming
// it would wedge every later create in the group.
func TestEnsureAntiAffinity_DeadRuleMemberDropped(t *testing.T) {
	stub := newAAStub()
	stub.seedRule(aaRule, "vm:100,vm:999")
	// vm:999 appears in no node's listing: its guest is gone.
	stub.listResourcesFn = aaResourcesFn(aaQEMU(100, aaTag))

	if err := ensureAntiAffinityMembership(context.Background(), aaDeps(stub), aaIdent("web"), 101, log.NewNopLogger()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := stub.rules[aaRule]; got != "vm:100,vm:101" {
		t.Errorf("rule resources = %q; want %q (dead vm:999 must be dropped)", got, "vm:100,vm:101")
	}
}

// TestEnsureAntiAffinity_OfflineExcludedMemberKept: when the enumeration
// tolerated an offline cluster member, a rule sid absent from the live set is
// unenumerated, not provably gone, so the union must keep it. Dropping it
// would recreate the rule without the guest that node hosts and silently
// void the spread guarantee for the reboot window.
func TestEnsureAntiAffinity_OfflineExcludedMemberKept(t *testing.T) {
	stub := newAAStub()
	stub.seedRule(aaRule, "vm:100,vm:1042")
	stub.memberNodes = []string{"pve1", "pve2"}
	stub.statusFn = func() (*cluster.ListStatusResponse, error) {
		resp := cluster.ListStatusResponse{
			json.RawMessage(`{"type": "cluster", "name": "lab", "quorate": 1}`),
			json.RawMessage(`{"type": "node", "name": "pve1", "online": 1}`),
			json.RawMessage(`{"type": "node", "name": "pve2", "online": 0}`),
		}
		return &resp, nil
	}
	// vm:1042 lives on the offline pve2, so the tolerant scan cannot see it.
	stub.listResourcesFn = aaResourcesFn(aaQEMU(100, aaTag))

	if err := ensureAntiAffinityMembership(context.Background(), aaDeps(stub), aaIdent("web"), 101, log.NewNopLogger()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := stub.rules[aaRule]; got != "vm:100,vm:101,vm:1042" {
		t.Errorf("rule resources = %q; want %q (vm:1042 is unenumerated, not gone, and must survive)",
			got, "vm:100,vm:101,vm:1042")
	}
}

func TestEnsureAntiAffinity_EmptyGroupNoOp(t *testing.T) {
	stub := newAAStub()
	if err := ensureAntiAffinityMembership(context.Background(), aaDeps(stub), aaIdent(""), 100, log.NewNopLogger()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(stub.resources) != 0 || len(stub.rules) != 0 {
		t.Error("empty group key must be a no-op")
	}
}

func TestEnsureAntiAffinity_ListRulesFailureReturnsError(t *testing.T) {
	stub := newAAStub()
	stub.failListRules = true
	stub.listResourcesFn = aaResourcesFn(aaQEMU(100, aaTag))
	err := ensureAntiAffinityMembership(context.Background(), aaDeps(stub), aaIdent("web"), 101, log.NewNopLogger())
	if err == nil {
		t.Fatal("expected error when ListHaRules fails (caller logs it best-effort)")
	}
}

// --------------------------------------------------------------------------
// removeAntiAffinityMembership
// --------------------------------------------------------------------------

func TestRemoveAntiAffinity_RecreatesWithoutVMID(t *testing.T) {
	stub := newAAStub()
	stub.seedRule(aaRule, "vm:100,vm:101,vm:102")
	if err := removeAntiAffinityMembership(context.Background(), aaDeps(stub), 101, log.NewNopLogger()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := stub.rules[aaRule]; got != "vm:100,vm:102" {
		t.Errorf("rule resources = %q; want %q", got, "vm:100,vm:102")
	}
}

func TestRemoveAntiAffinity_DeletesWhenSingleMemberRemains(t *testing.T) {
	stub := newAAStub()
	stub.seedRule(aaRule, "vm:100,vm:101")
	if err := removeAntiAffinityMembership(context.Background(), aaDeps(stub), 101, log.NewNopLogger()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := stub.rules[aaRule]; ok {
		t.Errorf("rule should be deleted when only one member remains; got %v", stub.rules)
	}
}

func TestRemoveAntiAffinity_NonMemberNoOp(t *testing.T) {
	stub := newAAStub()
	stub.seedRule(aaRule, "vm:100,vm:102")
	if err := removeAntiAffinityMembership(context.Background(), aaDeps(stub), 999, log.NewNopLogger()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := stub.rules[aaRule]; got != "vm:100,vm:102" {
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
	deps := Deps{Config: icMinConfig(), RequestDirectorUUID: aaDirector}
	// Disabled config -> empty tag.
	if got := antiAffinityGroupTag(deps, env); got != "" {
		t.Errorf("disabled anti-affinity should yield empty tag; got %q", got)
	}
	// Enabled -> the hashed member tag, which is also the rule name.
	enabled := true
	deps.Config.Placement = &config.PlacementConfig{
		AntiAffinity: &config.AntiAffinityConfig{Enabled: &enabled},
	}
	want := haIdentity{Owner: aaDirector, Deployment: "cf", Group: "web"}.ruleName()
	if got := antiAffinityGroupTag(deps, env); got != want {
		t.Errorf("antiAffinityGroupTag = %q; want %q", got, want)
	}
	// No Director UUID -> no tag, because no rule could be marked.
	deps.RequestDirectorUUID = ""
	if got := antiAffinityGroupTag(deps, env); got != "" {
		t.Errorf("a request without a director_uuid should yield an empty tag; got %q", got)
	}
}

// ListStatus delegates to statusFn when scripted; otherwise it reports no
// offline members (the fixture cluster is fully online).
func (s *aaClusterStub) ListStatus(context.Context) (*cluster.ListStatusResponse, error) {
	if s.statusFn != nil {
		return s.statusFn()
	}
	empty := cluster.ListStatusResponse{}
	return &empty, nil
}
