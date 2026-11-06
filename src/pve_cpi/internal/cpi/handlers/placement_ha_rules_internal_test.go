package handlers

import (
	"context"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
)

// aaModeDeps returns aaDeps over a cluster whose configured nodes are nodes, in
// mode with the overflow choice overflow ("" keeps the default, skip).
func aaModeDeps(stub *aaClusterStub, mode, overflow string, nodes ...string) Deps {
	deps := aaDeps(stub)
	deps.Config.HA = &config.HAConfig{Mode: haChoiceOf(mode)}
	if overflow != "" {
		deps.Config.HA.AntiAffinityOverflow = haChoiceOf(overflow)
	}
	deps.HAProfile = pve9ClusterProfile(nodes...)
	return deps
}

func TestAntiAffinityOverflowSkipWritesNoRule(t *testing.T) {
	for _, mode := range []string{config.HAModeAuto, config.HAModeManaged} {
		t.Run(mode, func(t *testing.T) {
			stub := newAAStub()
			stub.listResourcesFn = aaResourcesFn(aaQEMU(100, aaTag), aaQEMU(101, aaTag))
			deps := aaModeDeps(stub, mode, "", "pve1", "pve2")
			if err := ensureAntiAffinityMembership(context.Background(), deps, aaIdent("web"), 102,
				log.NewNopLogger()); err != nil {
				t.Fatalf("skip must not fail the call in %s: %v", mode, err)
			}
			if len(stub.rules) != 0 || stub.createRuleCalls != 0 {
				t.Fatalf("an oversized group got a rule: %v", stub.rules)
			}

			// A rule the group already has keeps its members, and nothing is
			// written for the member that would make it too large.
			stub = newAAStub()
			stub.seedRule(aaRule, "vm:100,vm:101")
			stub.listResourcesFn = aaResourcesFn(aaQEMU(100, aaTag), aaQEMU(101, aaTag))
			deps = aaModeDeps(stub, mode, config.HAOverflowSkip, "pve1", "pve2")
			if err := ensureAntiAffinityMembership(context.Background(), deps, aaIdent("web"), 102,
				log.NewNopLogger()); err != nil {
				t.Fatal(err)
			}
			if stub.rules[aaRule] != "vm:100,vm:101" || stub.updateRuleCalls+stub.createRuleCalls+
				stub.deleteRuleCalls != 0 {
				t.Fatalf("skip rewrote the rule: %v", stub.rules)
			}
		})
	}
}

func TestAntiAffinityOverflowSplitWritesRulesThatFit(t *testing.T) {
	for _, mode := range []string{config.HAModeAuto, config.HAModeManaged} {
		t.Run(mode, func(t *testing.T) {
			stub := newAAStub()
			stub.listResourcesFn = aaResourcesFn(aaQEMU(100, aaTag), aaQEMU(101, aaTag), aaQEMU(102, aaTag))
			deps := aaModeDeps(stub, mode, config.HAOverflowSplit, "pve1", "pve2")
			if err := ensureAntiAffinityMembership(context.Background(), deps, aaIdent("web"), 103,
				log.NewNopLogger()); err != nil {
				t.Fatal(err)
			}
			if stub.rules[aaRule] != "vm:100,vm:101" || stub.rules[aaChunkName(aaRule, 2)] != "vm:102,vm:103" ||
				len(stub.rules) != 2 {
				t.Fatalf("split rules = %v", stub.rules)
			}
		})
	}
}

func TestAntiAffinityPlaceableCapAndMaintenance(t *testing.T) {
	profile := func(context.Context) (*ha.Profile, error) {
		p, _ := pve9ClusterProfile("pve1", "pve2", "pve3")(context.Background())
		p.Health = &pve.NodeHealthReport{Nodes: []pve.NodeHealthEntry{
			{Name: "pve1", Placeable: true}, {Name: "pve2", Placeable: true}, {Name: "pve3", Maintenance: true},
		}}
		return p, nil
	}

	// Two placeable nodes: a third member waits for its rule.
	stub := newAAStub()
	stub.seedRule(aaRule, "vm:100,vm:101")
	stub.listResourcesFn = aaResourcesFn(aaQEMU(100, aaTag), aaQEMU(101, aaTag))
	deps := aaDeps(stub)
	deps.HAProfile = profile
	if err := ensureAntiAffinityMembership(context.Background(), deps, aaIdent("web"), 102,
		log.NewNopLogger()); err != nil {
		t.Fatal(err)
	}
	if stub.rules[aaRule] != "vm:100,vm:101" || stub.updateRuleCalls != 0 {
		t.Fatalf("a member joined beyond the placeable nodes: %v", stub.rules)
	}

	// A node entering maintenance never rewrites a rule that already holds
	// every member.
	stub = newAAStub()
	stub.seedRule(aaRule, "vm:100,vm:101,vm:102")
	stub.listResourcesFn = aaResourcesFn(aaQEMU(100, aaTag), aaQEMU(101, aaTag), aaQEMU(102, aaTag))
	deps = aaDeps(stub)
	deps.HAProfile = profile
	if err := ensureAntiAffinityMembership(context.Background(), deps, aaIdent("web"), 102,
		log.NewNopLogger()); err != nil {
		t.Fatal(err)
	}
	if stub.updateRuleCalls+stub.createRuleCalls+stub.deleteRuleCalls != 0 {
		t.Fatalf("maintenance rewrote the rule: update=%d create=%d delete=%d", stub.updateRuleCalls,
			stub.createRuleCalls, stub.deleteRuleCalls)
	}
}

func TestAntiAffinityFlattensMemberPinPriorities(t *testing.T) {
	stub := newAAStub()
	pin := naRuleNameFor(100)
	stub.seedRuleWithComment(pin, "vm:100", haIdentity{Owner: aaDirector}.marker())
	stub.ruleTypes[pin] = ha.RuleTypeNodeAffinity
	stub.ruleNodes[pin] = "pve1:2,pve2:1,pve3"
	stub.listResourcesFn = aaResourcesFn(aaQEMU(100, aaTag))
	deps := aaDeps(stub)
	deps.HAProfile = pve9ClusterProfile("pve1", "pve2", "pve3")
	if err := ensureAntiAffinityMembership(context.Background(), deps, aaIdent("web"), 101,
		log.NewNopLogger()); err != nil {
		t.Fatal(err)
	}
	if stub.rules[aaRule] != "vm:100,vm:101" || stub.ruleNodes[pin] != "pve1,pve2,pve3" {
		t.Fatalf("rule %q pin nodes %q", stub.rules[aaRule], stub.ruleNodes[pin])
	}
}

func naClusterDeps(stub *naClusterStub, nodes ...string) Deps {
	deps := naDeps(stub)
	deps.HAProfile = pve9ClusterProfile(nodes...)
	return deps
}

func TestMergedPinIntersectsAZAndPCIInEitherOrder(t *testing.T) {
	az, pci := []string{"pve01", "pve02"}, []string{"pve02"}
	for name, order := range map[string][2][]string{"az then pci": {az, pci}, "pci then az": {pci, az}} {
		t.Run(name, func(t *testing.T) {
			stub := newNAStub()
			deps := naClusterDeps(stub, "pve01", "pve02", "pve03")
			for _, nodes := range order {
				if err := ensureNodeAffinityPin(context.Background(), deps, 100, nodes, true,
					log.NewNopLogger()); err != nil {
					t.Fatal(err)
				}
			}
			rule := stub.rules[naRuleNameFor(100)]
			if rule.Nodes == nil || *rule.Nodes != "pve02" || rule.Strict == nil || !*rule.Strict {
				t.Fatalf("merged pin nodes %v strict %v", ptrStr(rule.Nodes), rule.Strict)
			}
		})
	}
}

func TestMergedPinPrioritiesFlattenInAResourceAffinityRule(t *testing.T) {
	req := pinRequest{Preferred: []string{"pve01", "pve02"}, Placed: "pve01", Register: true}
	stub := newNAStub()
	deps := naClusterDeps(stub, "pve01", "pve02", "pve03")
	if err := ensureMergedPin(context.Background(), deps, 100, req, log.NewNopLogger()); err != nil {
		t.Fatal(err)
	}
	rule := stub.rules[naRuleNameFor(100)]
	if got := ptrStr(rule.Nodes); got != "pve01:2,pve02:1,pve03" || (rule.Strict != nil && *rule.Strict) {
		t.Fatalf("preferred pin nodes %q strict %v", got, rule.Strict)
	}

	stub = newNAStub()
	negative := ha.AffinityNegative
	stub.rules["bosh-aa-x"] = cluster.CreateHaRulesParams{Rule: "bosh-aa-x", Type: ha.RuleTypeResourceAffinity,
		Affinity: &negative, Resources: "vm:100,vm:101"}
	deps = naClusterDeps(stub, "pve01", "pve02", "pve03")
	if err := ensureMergedPin(context.Background(), deps, 100, req, log.NewNopLogger()); err != nil {
		t.Fatal(err)
	}
	if got := ptrStr(stub.rules[naRuleNameFor(100)].Nodes); got != "pve01,pve02,pve03" {
		t.Fatalf("pin of a VM in an anti-affinity rule kept priorities: %q", got)
	}
}

func TestRebalancePin(t *testing.T) {
	rebalance := func(on bool) func(context.Context) (*ha.Profile, error) {
		return func(context.Context) (*ha.Profile, error) {
			p, _ := pve9ClusterProfile("pve01", "pve02", "pve03")(context.Background())
			p.CRS = ha.CRS{Known: true, RebalanceOnStart: on}
			return p, nil
		}
	}
	registered := func() *naClusterStub {
		stub := newNAStub()
		stub.resources["vm:100"] = true
		return stub
	}

	stub := registered()
	deps := naDeps(stub)
	deps.HAProfile = rebalance(true)
	if err := ensureRebalancePin(context.Background(), deps, 100, createVMCloudProps{}, "pve02",
		log.NewNopLogger()); err != nil {
		t.Fatal(err)
	}
	rule, ok := stub.rules[naRuleNameFor(100)]
	if !ok || ptrStr(rule.Nodes) != "pve02" || (rule.Strict != nil && *rule.Strict) {
		t.Fatalf("placed-node pin = %+v", rule)
	}

	for name, tc := range map[string]struct {
		stub *naClusterStub
		cp   createVMCloudProps
		on   bool
	}{
		"dlb":           {registered(), createVMCloudProps{AvailabilityZone: "dlb"}, true},
		"unregistered":  {newNAStub(), createVMCloudProps{}, true},
		"rebalance off": {registered(), createVMCloudProps{}, false},
	} {
		deps := naDeps(tc.stub)
		deps.HAProfile = rebalance(tc.on)
		if err := ensureRebalancePin(context.Background(), deps, 100, tc.cp, "pve02", log.NewNopLogger()); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(tc.stub.rules) != 0 {
			t.Fatalf("%s: wrote %v", name, tc.stub.rules)
		}
	}
}

func TestMergedPinRefusesAnotherDirectorsPin(t *testing.T) {
	stub := newNAStub()
	other := haIdentity{Owner: aaOtherDirector}.marker()
	nodes := "pve01"
	stub.rules[naRuleNameFor(100)] = cluster.CreateHaRulesParams{Rule: naRuleNameFor(100),
		Type: ha.RuleTypeNodeAffinity, Resources: "vm:100", Nodes: &nodes, Comment: &other}
	deps := naClusterDeps(stub, "pve01", "pve02")
	err := ensureMergedPin(context.Background(), deps, 100, pinRequest{Hard: []string{"pve02"}, Placed: "pve02"},
		log.NewNopLogger())
	if err == nil || stub.updateCalls != 0 {
		t.Fatalf("another Director's pin was rewritten: err %v updates %d", err, stub.updateCalls)
	}
	deps.RequestDirectorUUID = ""
	if err := ensureMergedPin(context.Background(), deps, 101, pinRequest{Hard: []string{"pve02"}},
		log.NewNopLogger()); err == nil {
		t.Fatal("a pin without a director_uuid was written")
	}
}

// haRulesReadClient serves only the cluster service, for the rule observers.
type haRulesReadClient struct {
	pve.Client
	svc cluster.Service
}

func (c *haRulesReadClient) Cluster() cluster.Service { return c.svc }

func TestObserveUpdatedHARule(t *testing.T) {
	stub := newNAStub()
	nodes, strict, comment := "pve01:2,pve02", true, "marker"
	name := naRuleNameFor(100)
	stub.rules[name] = cluster.CreateHaRulesParams{Rule: name, Type: ha.RuleTypeNodeAffinity, Resources: "vm:100",
		Nodes: &nodes, Strict: &strict, Comment: &comment}
	m := &managedVMAllocation{vmid: 100, deps: Deps{PVE: &haRulesReadClient{svc: stub}}}
	resources := "vm:100"
	call := func(params *cluster.UpdateHaRulesParams, rule string) error {
		return m.observeUpdatedHARule(context.Background(), ManagedAllocationMutation{
			Args: map[string]any{"rule": rule, managedArgumentParams: params}})
	}
	params := &cluster.UpdateHaRulesParams{Type: ha.RuleTypeNodeAffinity, Resources: &resources, Nodes: &nodes,
		Strict: &strict, Comment: &comment}
	if err := call(params, name); err != nil {
		t.Fatalf("a matching readback failed: %v", err)
	}
	loose := false
	mismatch := *params
	mismatch.Strict = &loose
	if err := call(&mismatch, name); err == nil || !strings.Contains(err.Error(), "strict") {
		t.Fatalf("a strict mismatch passed: %v", err)
	}
	if err := call(params, "bosh-na-999"); err == nil {
		t.Fatal("an absent rule passed")
	}
	if err := call(nil, name); err == nil {
		t.Fatal("an update without parameters passed")
	}
}
