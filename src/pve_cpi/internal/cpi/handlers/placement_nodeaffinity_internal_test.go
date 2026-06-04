package handlers

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cluster"
)

// naClusterStub captures the full CreateHaRules params so node-affinity tests can
// assert Type/Nodes/Strict, which aaClusterStub does not track.
type naClusterStub struct {
	cluster.Service
	resources   map[string]bool
	rules       map[string]cluster.CreateHaRulesParams
	createCalls int
	deleteCalls int
	resDelCalls int
}

var _ cluster.Service = (*naClusterStub)(nil)

func newNAStub() *naClusterStub {
	return &naClusterStub{resources: map[string]bool{}, rules: map[string]cluster.CreateHaRulesParams{}}
}

func (s *naClusterStub) CreateHaResources(_ context.Context, p *cluster.CreateHaResourcesParams) error {
	s.resources[p.Sid] = true
	return nil
}

func (s *naClusterStub) DeleteHaResources(_ context.Context, sid string, _ *cluster.DeleteHaResourcesParams) error {
	s.resDelCalls++
	delete(s.resources, sid)
	return nil
}

func (s *naClusterStub) ListHaRules(_ context.Context, _ *cluster.ListHaRulesParams) (*cluster.ListHaRulesResponse, error) {
	out := make(cluster.ListHaRulesResponse, 0, len(s.rules))
	for name, p := range s.rules {
		raw, _ := json.Marshal(map[string]any{"rule": name, "type": p.Type, "resources": p.Resources})
		out = append(out, raw)
	}
	return &out, nil
}

func (s *naClusterStub) CreateHaRules(_ context.Context, p *cluster.CreateHaRulesParams) error {
	s.createCalls++
	s.rules[p.Rule] = *p
	return nil
}

func (s *naClusterStub) DeleteHaRules(_ context.Context, rule string) error {
	s.deleteCalls++
	delete(s.rules, rule)
	return nil
}

func naDeps(stub *naClusterStub) Deps {
	return Deps{
		Config: icMinConfig(),
		PVE:    &icPVEClient{clusterSvc: stub},
		Agent:  &icAgentStub{},
		Logger: log.NewNopLogger(),
	}
}

func TestEnsureNodeAffinityPin_CreatesStrictRule(t *testing.T) {
	stub := newNAStub()
	if err := ensureNodeAffinityPin(context.Background(), naDeps(stub), 100,
		[]string{"pve02", "pve01"}, true, log.NewNopLogger()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	rule, ok := stub.rules["bosh-na-100"]
	if !ok {
		t.Fatal("expected rule bosh-na-100")
	}
	if rule.Type != "node-affinity" {
		t.Errorf("Type = %q; want node-affinity", rule.Type)
	}
	if rule.Resources != "vm:100" {
		t.Errorf("Resources = %q; want vm:100", rule.Resources)
	}
	if rule.Nodes == nil || *rule.Nodes != "pve01,pve02" {
		t.Errorf("Nodes = %v; want sorted pve01,pve02", rule.Nodes)
	}
	if rule.Strict == nil || !*rule.Strict {
		t.Errorf("Strict = %v; want true", rule.Strict)
	}
	if !stub.resources["vm:100"] {
		t.Error("vm:100 must be registered as an HA resource")
	}
}

func TestEnsureNodeAffinityPin_NonStrict(t *testing.T) {
	stub := newNAStub()
	if err := ensureNodeAffinityPin(context.Background(), naDeps(stub), 7,
		[]string{"pve01"}, false, log.NewNopLogger()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	rule := stub.rules["bosh-na-7"]
	if rule.Strict == nil || *rule.Strict {
		t.Errorf("Strict = %v; want false (preferred pin)", rule.Strict)
	}
}

func TestEnsureNodeAffinityPin_EmptyNodesNoOp(t *testing.T) {
	stub := newNAStub()
	if err := ensureNodeAffinityPin(context.Background(), naDeps(stub), 100,
		[]string{"", "  "}, true, log.NewNopLogger()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stub.createCalls != 0 {
		t.Errorf("empty node set must create no rule; createCalls=%d", stub.createCalls)
	}
}

func TestEnsureNodeAffinityPin_RefreshesExisting(t *testing.T) {
	stub := newNAStub()
	// Pre-seed a stale rule (e.g. a create_vm retry).
	stub.rules["bosh-na-100"] = cluster.CreateHaRulesParams{Rule: "bosh-na-100", Type: "node-affinity"}
	if err := ensureNodeAffinityPin(context.Background(), naDeps(stub), 100,
		[]string{"pve03"}, true, log.NewNopLogger()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stub.deleteCalls != 1 {
		t.Errorf("stale rule must be deleted before recreate; deleteCalls=%d", stub.deleteCalls)
	}
	if got := stub.rules["bosh-na-100"]; got.Nodes == nil || *got.Nodes != "pve03" {
		t.Errorf("refreshed rule Nodes = %v; want pve03", got.Nodes)
	}
}

func TestRemoveNodeAffinityPin_DeletesRuleAndResource(t *testing.T) {
	stub := newNAStub()
	stub.rules["bosh-na-100"] = cluster.CreateHaRulesParams{Rule: "bosh-na-100"}
	stub.resources["vm:100"] = true

	if err := removeNodeAffinityPin(context.Background(), naDeps(stub), 100, log.NewNopLogger()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := stub.rules["bosh-na-100"]; ok {
		t.Error("rule bosh-na-100 must be deleted")
	}
	if stub.resources["vm:100"] {
		t.Error("HA resource vm:100 must be deregistered")
	}
}

func TestRemoveNodeAffinityPin_IdempotentWhenAbsent(t *testing.T) {
	stub := newNAStub() // nothing to delete
	if err := removeNodeAffinityPin(context.Background(), naDeps(stub), 100, log.NewNopLogger()); err != nil {
		t.Fatalf("absent rule/resource must be a no-op; got %v", err)
	}
}

// naPinConfig builds a Config with pinning enabled and the given AZ map.
func naPinConfig(azMap map[string][]string) *config.CPIConfig {
	cfg := icMinConfig()
	cfg.Placement = &config.PlacementConfig{
		PinAZViaHARules: boolPtrHelper(true),
		PinAZStrict:     boolPtrHelper(true),
		AZMap:           azMap,
	}
	return cfg
}

func boolPtrHelper(b bool) *bool { return &b }

func TestPinAZForNode_SingularAndPlural(t *testing.T) {
	cfg := naPinConfig(map[string][]string{"z1": {"pve01", "pve02"}, "z2": {"pve03"}})
	// Singular form.
	if got := pinAZForNode(createVMCloudProps{AvailabilityZone: "z1"}, cfg, "pve02"); got != "z1" {
		t.Errorf("singular: pinAZForNode = %q; want z1", got)
	}
	// Plural form: the VM requested [z2, z1] and landed on pve03 (in z2).
	if got := pinAZForNode(createVMCloudProps{AvailabilityZones: []string{"z2", "z1"}}, cfg, "pve03"); got != "z2" {
		t.Errorf("plural: pinAZForNode = %q; want z2", got)
	}
	// Node not in any requested AZ (e.g. config.node fallback) → "".
	if got := pinAZForNode(createVMCloudProps{AvailabilityZone: "z1"}, cfg, "pve09"); got != "" {
		t.Errorf("non-member node: pinAZForNode = %q; want empty", got)
	}
}

func TestApplyAZNodeAffinityPin_PluralAZ_PinsChosenAZ(t *testing.T) {
	stub := newNAStub()
	deps := Deps{
		Config: naPinConfig(map[string][]string{"z1": {"pve01"}, "z2": {"pve02", "pve03"}}),
		PVE:    &icPVEClient{clusterSvc: stub},
		Agent:  &icAgentStub{},
		Logger: log.NewNopLogger(),
	}
	// Plural-AZ VM placed on pve03 (in z2). This is the regression the CRITICAL
	// fix closes: the old code read the singular AvailabilityZone (empty here)
	// and never pinned.
	cp := createVMCloudProps{AvailabilityZones: []string{"z1", "z2"}}
	applyAZNodeAffinityPin(context.Background(), deps, 100, cp, "pve03", log.NewNopLogger())

	rule, ok := stub.rules["bosh-na-100"]
	if !ok {
		t.Fatal("plural-AZ VM must be pinned (bosh-na-100 missing)")
	}
	if rule.Nodes == nil || *rule.Nodes != "pve02,pve03" {
		t.Errorf("pin Nodes = %v; want z2 node set pve02,pve03", rule.Nodes)
	}
}

func TestApplyAZNodeAffinityPin_DisabledNoOp(t *testing.T) {
	stub := newNAStub()
	deps := naDeps(stub) // icMinConfig has no Placement → pin disabled
	applyAZNodeAffinityPin(context.Background(), deps, 100,
		createVMCloudProps{AvailabilityZone: "z1"}, "pve01", log.NewNopLogger())
	if stub.createCalls != 0 {
		t.Errorf("pin disabled must create no rule; createCalls=%d", stub.createCalls)
	}
}

func TestApplyAZNodeAffinityPin_FallbackNodeSkips(t *testing.T) {
	stub := newNAStub()
	deps := Deps{
		Config: naPinConfig(map[string][]string{"z1": {"pve01"}}),
		PVE:    &icPVEClient{clusterSvc: stub},
		Agent:  &icAgentStub{},
		Logger: log.NewNopLogger(),
	}
	// VM requested z1 but landed on config.node "pve99" (fallback) → no AZ
	// membership → no pin (skipped, not errored).
	applyAZNodeAffinityPin(context.Background(), deps, 100,
		createVMCloudProps{AvailabilityZone: "z1"}, "pve99", log.NewNopLogger())
	if stub.createCalls != 0 {
		t.Errorf("fallback node outside AZ must not pin; createCalls=%d", stub.createCalls)
	}
}

func TestNARuleNameFor(t *testing.T) {
	if got := naRuleNameFor(100); got != "bosh-na-100" {
		t.Errorf("naRuleNameFor(100) = %q; want bosh-na-100", got)
	}
}

func TestSortedNodesCSV_DedupAndSort(t *testing.T) {
	if got := sortedNodesCSV([]string{"pve02", "pve01", "pve02", " ", "pve03"}); got != "pve01,pve02,pve03" {
		t.Errorf("sortedNodesCSV = %q; want pve01,pve02,pve03", got)
	}
}
