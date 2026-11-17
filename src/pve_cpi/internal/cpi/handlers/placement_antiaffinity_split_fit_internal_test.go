package handlers

import (
	"context"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// These tests cover where split mode puts a sibling whose join conflicts with
// the group's first rule, when the group has more rules than one.

// TestEnsureAntiAffinity_SplitKeepsASiblingOutOfARuleThatAlreadyConflicts
// pins that the plan checks a sibling's join to a later rule the way the
// writer checks that rule before it sends it, counting every conflict on the
// rule. The second rule already conflicts, because vm:104 and vm:105 are both
// pinned strictly to pve2. vm:103 conflicts with the first rule, and its own
// pins conflict already through a second node-affinity rule, so its join to
// the second rule adds no conflict that the rules lacked. The plan must still
// keep it out of the second rule, or the writer refuses that rule's write and
// fails the create. The create must join the first rule, and the second rule
// must keep its members.
func TestEnsureAntiAffinity_SplitKeepsASiblingOutOfARuleThatAlreadyConflicts(t *testing.T) {
	t.Parallel()
	second := aaChunkName(aaRule, 2)
	for _, mode := range aaModes {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			stub := newAAStub()
			stub.enforceManaged = true
			managedGroup(stub, 101, 102, 103, 104, 105, 106)
			stub.seedRule(aaRule, "vm:101,vm:102")
			stub.seedRule(second, "vm:104,vm:105")
			seedStrictPin(stub, 101, "pve1")
			seedStrictPin(stub, 103, "pve1")
			seedStrictPin(stub, 104, "pve2")
			seedStrictPin(stub, 105, "pve2")
			stub.seedRuleWithComment("op-pin-103", "vm:103", "")
			stub.ruleTypes["op-pin-103"], stub.ruleNodes["op-pin-103"], stub.ruleStrict["op-pin-103"] =
				"node-affinity", "pve1", true
			deps := aaModeDeps(stub, mode, config.HAOverflowSplit, "pve1", "pve2", "pve3", "pve4")
			logger, observed := log.NewObservedLogger(log.LevelDebug)
			placedIn, err := ensureAntiAffinityMembershipFor(context.Background(), deps, aaIdent("web"), 106, nil,
				logger)
			if err != nil {
				t.Fatalf("err = %v; want the create to join the first rule with vm:103 left out", err)
			}
			if placedIn != aaRule {
				t.Fatalf("placed in %q; want %q", placedIn, aaRule)
			}
			if got := stub.rules[aaRule]; got != "vm:101,vm:102,vm:106" {
				t.Fatalf("first rule = %q; want vm:101,vm:102,vm:106", got)
			}
			if got := stub.rules[second]; got != "vm:104,vm:105" {
				t.Fatalf("second rule = %q; want it unchanged", got)
			}
			requireLeftOutOnce(t, observed, "vm:103")
		})
	}
}
