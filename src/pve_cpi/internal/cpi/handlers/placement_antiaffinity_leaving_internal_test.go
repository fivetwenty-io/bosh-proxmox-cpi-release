package handlers

import (
	"context"
	"slices"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

func TestHAMarker_ReadsAndRewritesTheLeavingMembers(t *testing.T) {
	t.Parallel()
	base := aaIdent("web").marker()
	got := withLeavingMembers(base, []string{"vm:105", "vm:101", "vm:105"})
	if got != base+" left=101.105" {
		t.Fatalf("withLeavingMembers = %q", got)
	}
	m, ok := parseHaMarker(got)
	if !ok || !slices.Equal(m.Left, []string{"vm:101", "vm:105"}) {
		t.Fatalf("parsed marker = %+v, %v; want both leaving members", m, ok)
	}
	if again := withLeavingMembers(got, []string{"vm:105"}); again != base+" left=105" {
		t.Fatalf("rewrite = %q; want the old list replaced", again)
	}
	if cleared := withLeavingMembers(got, nil); cleared != base {
		t.Fatalf("clear = %q; want the plain marker", cleared)
	}
	if foreign := withLeavingMembers("operator note", []string{"vm:101"}); foreign != "operator note" {
		t.Fatalf("a comment with no marker must stay as it is, got %q", foreign)
	}
}

// TestRemoveAntiAffinity_RecordsTheLeavingMember pins that the write that
// takes a member out of a rule names it in the rule's marker, in the same
// write, so no sibling's plan reads the rule without it and without the
// record.
func TestRemoveAntiAffinity_RecordsTheLeavingMember(t *testing.T) {
	t.Parallel()
	comments := map[string]string{}
	set := &ha.RuleSet{Rules: []ha.Rule{{ID: aaRule, Type: haRuleType, Affinity: haRuleAffinity,
		Resources: []string{"vm:100", "vm:101", "vm:102"}, Comment: aaIdent("web").marker()}}}
	ops := planAntiAffinityRemoval(set, "vm:101", aaDirector, comments)
	if len(ops) != 1 || ops[0].Rule.Comment != aaIdent("web").marker()+" left=101" {
		t.Fatalf("ops = %+v; want one update that records vm:101 as leaving", ops)
	}
}

// TestEnsureAntiAffinity_LeavesALeavingMemberOut pins that a sibling's plan
// does not put back a member whose delete took it out of its rule but has
// not purged its HA resource yet, and that a later write drops the record
// once the resource is gone.
func TestEnsureAntiAffinity_LeavesALeavingMemberOut(t *testing.T) {
	t.Parallel()
	stub := newAAStub()
	stub.tagged(aaQEMU(100, aaTag), aaQEMU(101, aaTag), aaQEMU(102, aaTag), aaQEMU(103, aaTag),
		aaQEMU(104, aaTag))
	for _, v := range []int{100, 101, 102, 103, 104} {
		stub.resComments[haResourceSid(v)] = aaIdent("web").marker()
	}
	stub.seedRuleWithComment(aaRule, "vm:100,vm:102", aaIdent("web").marker()+" left=101")
	deps := aaDeps(stub)
	deps.HAProfile = pve9ClusterProfile("pve1", "pve2", "pve3", "pve4", "pve5")
	if err := ensureAntiAffinityMembership(context.Background(), deps, aaIdent("web"), 103,
		log.NewNopLogger()); err != nil {
		t.Fatal(err)
	}
	if got := stub.rules[aaRule]; got != "vm:100,vm:102,vm:103,vm:104" {
		t.Fatalf("rule = %q; want the leaving vm:101 left out", got)
	}
	if got := stub.ruleComments[aaRule]; got != aaIdent("web").marker()+" left=101" {
		t.Fatalf("comment = %q; want the record kept while vm:101 is still an HA resource", got)
	}
	// The purge lands. vm:104 leaves too, and its own record joins the
	// first, because a removal does not read which HA resources are left.
	delete(stub.resources, "vm:101")
	if err := removeAntiAffinityMembership(context.Background(), deps, 104, "", log.NewNopLogger()); err != nil {
		t.Fatal(err)
	}
	if got := stub.ruleComments[aaRule]; got != aaIdent("web").marker()+" left=101.104" {
		t.Fatalf("comment = %q; want both leaving members recorded", got)
	}
	// The next write of a joining member drops the records whose HA
	// resources are gone.
	stub.tagged(aaQEMU(100, aaTag), aaQEMU(102, aaTag), aaQEMU(103, aaTag), aaQEMU(105, aaTag))
	if err := ensureAntiAffinityMembership(context.Background(), deps, aaIdent("web"), 105,
		log.NewNopLogger()); err != nil {
		t.Fatal(err)
	}
	if got := stub.rules[aaRule]; got != "vm:100,vm:102,vm:103,vm:105" {
		t.Fatalf("rule = %q; want vm:105 in it", got)
	}
	if got := stub.ruleComments[aaRule]; got != aaIdent("web").marker() {
		t.Fatalf("comment = %q; want the stale records dropped", got)
	}
}

// TestEnsureAntiAffinity_ARecreatedVMIDJoinsDespiteARecord pins that a VM's
// own join is never held back by a record its VMID's earlier owner left, and
// that its write clears the record.
func TestEnsureAntiAffinity_ARecreatedVMIDJoinsDespiteARecord(t *testing.T) {
	t.Parallel()
	stub := newAAStub()
	stub.tagged(aaQEMU(100, aaTag), aaQEMU(101, aaTag), aaQEMU(102, aaTag))
	stub.seedRuleWithComment(aaRule, "vm:100,vm:102", aaIdent("web").marker()+" left=101")
	deps := aaDeps(stub)
	deps.HAProfile = pve9ClusterProfile("pve1", "pve2", "pve3", "pve4")
	if err := ensureAntiAffinityMembership(context.Background(), deps, aaIdent("web"), 101,
		log.NewNopLogger()); err != nil {
		t.Fatal(err)
	}
	if got := stub.rules[aaRule]; got != "vm:100,vm:101,vm:102" {
		t.Fatalf("rule = %q; want the recreated vm:101 in it", got)
	}
	if got := stub.ruleComments[aaRule]; got != aaIdent("web").marker() {
		t.Fatalf("comment = %q; want the record cleared", got)
	}
}
