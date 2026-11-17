package handlers

import (
	"context"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// seedMemberPin stores the CPI pin of vmid with nodes, strictness, and comment.
func seedMemberPin(stub *aaClusterStub, vmid int, nodes string, strict bool, comment string) string {
	pin := naRuleNameFor(vmid)
	stub.seedRuleWithComment(pin, haResourceSid(vmid), comment)
	stub.ruleTypes[pin] = ha.RuleTypeNodeAffinity
	stub.ruleNodes[pin] = nodes
	stub.ruleStrict[pin] = strict
	return pin
}

// TestAntiAffinityWidensASoftSingleNodeMemberPin shows that a plan of the
// group rewrites this Director's soft pin that names one node so it names
// every configured node, even when the group's rule already holds every
// member. Such a pin is the placed-node pin an earlier create wrote under
// ha-rebalance-on-start, and with a sibling on its node the CRM had nowhere to
// move the VM.
func TestAntiAffinityWidensASoftSingleNodeMemberPin(t *testing.T) {
	ours := haIdentity{Owner: aaDirector}.marker()
	stub := newAAStub()
	stub.seedRule(aaRule, "vm:100,vm:101")
	pin100 := seedMemberPin(stub, 100, "pve2", false, ours)
	pin101 := seedMemberPin(stub, 101, "pve1", false, ours)
	stub.listResourcesFn = aaResourcesFn(aaQEMU(100, aaTag), aaQEMU(101, aaTag))
	deps := aaDeps(stub)
	deps.HAProfile = pve9ClusterProfile("pve1", "pve2", "pve3")
	if err := ensureAntiAffinityMembership(context.Background(), deps, aaIdent("web"), 101,
		log.NewNopLogger()); err != nil {
		t.Fatal(err)
	}
	for _, pin := range []string{pin100, pin101} {
		if stub.ruleNodes[pin] != "pve1,pve2,pve3" || stub.ruleStrict[pin] || stub.ruleComments[pin] != ours {
			t.Errorf("pin %s nodes %q strict %v comment %q", pin, stub.ruleNodes[pin], stub.ruleStrict[pin],
				stub.ruleComments[pin])
		}
	}
	if stub.rules[aaRule] != "vm:100,vm:101" {
		t.Fatalf("the group's rule changed: %q", stub.rules[aaRule])
	}

	// A second plan finds nothing left to write.
	writes := stub.createRuleCalls + stub.updateRuleCalls + stub.deleteRuleCalls
	if err := ensureAntiAffinityMembership(context.Background(), deps, aaIdent("web"), 101,
		log.NewNopLogger()); err != nil {
		t.Fatal(err)
	}
	if got := stub.createRuleCalls + stub.updateRuleCalls + stub.deleteRuleCalls; got != writes {
		t.Fatalf("a settled group was written again: %d writes, then %d", writes, got)
	}
}

// TestAntiAffinityLeavesConfiningAndForeignMemberPins shows that the widening
// touches only a soft pin this Director wrote. A strict pin confines its VM on
// purpose, as a hold or a replica pin does, and an unmarked pin or another
// Director's is not this Director's to change.
func TestAntiAffinityLeavesConfiningAndForeignMemberPins(t *testing.T) {
	ours := haIdentity{Owner: aaDirector}.marker()
	stub := newAAStub()
	stub.seedRule(aaRule, "vm:100,vm:101,vm:102")
	hold := seedMemberPin(stub, 100, "pve2", true, ours)
	unmarked := seedMemberPin(stub, 101, "pve1", false, "")
	foreign := seedMemberPin(stub, 102, "pve3", false, haIdentity{Owner: aaOtherDirector}.marker())
	stub.listResourcesFn = aaResourcesFn(aaQEMU(100, aaTag), aaQEMU(101, aaTag), aaQEMU(102, aaTag))
	deps := aaDeps(stub)
	deps.HAProfile = pve9ClusterProfile("pve1", "pve2", "pve3")
	if err := ensureAntiAffinityMembership(context.Background(), deps, aaIdent("web"), 102,
		log.NewNopLogger()); err != nil {
		t.Fatal(err)
	}
	for pin, want := range map[string]string{hold: "pve2", unmarked: "pve1", foreign: "pve3"} {
		if stub.ruleNodes[pin] != want {
			t.Errorf("pin %s nodes %q, want %q", pin, stub.ruleNodes[pin], want)
		}
	}
	if !stub.ruleStrict[hold] || stub.updateRuleCalls != 0 {
		t.Fatalf("hold pin strict %v, updates %d", stub.ruleStrict[hold], stub.updateRuleCalls)
	}
}
