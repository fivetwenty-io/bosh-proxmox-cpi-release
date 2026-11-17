package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// These tests cover the members a plan leaves out of the group's rules
// because their join would make the rules conflict. Every writer of the group
// decides that from the rules and pins it reads, so a member that a refusal
// or a repair left out stays out while the conflict lasts, and the rest of
// the group keeps its writes.

var aaModes = []string{config.HAModeAuto, config.HAModeManaged}

// managedGroup tags and manages every vmid as a member of the test group.
func managedGroup(stub *aaClusterStub, vmids ...int) {
	entries := make([]json.RawMessage, 0, len(vmids))
	for _, v := range vmids {
		entries = append(entries, aaQEMU(v, aaTag))
	}
	stub.tagged(entries...)
}

// requireLeftOutOnce fails the test unless the plan logged sid as left out
// for a conflict exactly once.
func requireLeftOutOnce(t *testing.T, observed *log.Observer, sid string) {
	t.Helper()
	n := 0
	for _, e := range observed.All() {
		if strings.Contains(e.Message, "member left out of the group's rule, because adding it would make the "+
			"rules conflict") && e.Attrs["sid"] == sid {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("logged %s as left out for a conflict %d times; want once", sid, n)
	}
}

// TestEnsureAntiAffinity_CreateLeavesOutASiblingWhoseStrictPinConflicts pins
// that a create leaves out a sibling whose join would put two strict pins to
// one node in the group's rule. vm:101 is in the rule and vm:103 is out of it,
// and both are pinned strictly to pve1. The create of vm:104 must join the
// rule and leave vm:103 out, in both modes, where before the check refused the
// whole write.
func TestEnsureAntiAffinity_CreateLeavesOutASiblingWhoseStrictPinConflicts(t *testing.T) {
	t.Parallel()
	for _, mode := range aaModes {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			stub := newAAStub()
			stub.enforceManaged = true
			managedGroup(stub, 101, 102, 103, 104)
			stub.seedRule(aaRule, "vm:101,vm:102")
			seedStrictPin(stub, 101, "pve1")
			seedStrictPin(stub, 103, "pve1")
			deps := aaModeDeps(stub, mode, "", "pve1", "pve2", "pve3", "pve4")
			logger, observed := log.NewObservedLogger(log.LevelDebug)
			placedIn, err := ensureAntiAffinityMembershipFor(context.Background(), deps, aaIdent("web"), 104, nil,
				logger)
			if err != nil {
				t.Fatalf("the create must join the rule: %v", err)
			}
			if placedIn != aaRule {
				t.Fatalf("placed in %q; want %q", placedIn, aaRule)
			}
			if got := stub.rules[aaRule]; got != "vm:101,vm:102,vm:104" {
				t.Fatalf("rule = %q; want the create joined and the conflicting sibling left out", got)
			}
			requireLeftOutOnce(t, observed, "vm:103")
		})
	}
}

// TestHealAntiAffinityGroup_LeavesOutMembersWhoseStrictPinsConflict pins that
// a heal with no confined members of its own leaves out every member whose
// join would make the group's rule conflict, in both modes. A removal deleted
// the group's rule, and the heal writes it again from the members it scans.
func TestHealAntiAffinityGroup_LeavesOutMembersWhoseStrictPinsConflict(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		pins map[int]string
		want string
		out  string
	}{
		{
			// Two members pinned strictly to one node cannot sit apart, so
			// the second in sid order stays out.
			name: "two members pinned to the same node",
			pins: map[int]string{101: "pve1", 103: "pve1"},
			want: "vm:101,vm:102,vm:104",
			out:  "vm:103",
		},
		{
			// Three members pinned strictly to the same two of four nodes
			// cannot sit apart, so the third stays out.
			name: "three members pinned to two nodes",
			pins: map[int]string{101: "pve1,pve2", 102: "pve1,pve2", 103: "pve1,pve2"},
			want: "vm:101,vm:102,vm:104",
			out:  "vm:103",
		},
		{
			// Pins to different single nodes leave room for every member.
			name: "members pinned to different nodes",
			pins: map[int]string{101: "pve1", 103: "pve2"},
			want: "vm:101,vm:102,vm:103,vm:104",
		},
	}
	for _, tc := range cases {
		for _, mode := range aaModes {
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				t.Parallel()
				stub := newAAStub()
				stub.enforceManaged = true
				managedGroup(stub, 101, 102, 103, 104)
				for vmid, nodes := range tc.pins {
					seedStrictPin(stub, vmid, nodes)
				}
				deps := aaModeDeps(stub, mode, "", "pve1", "pve2", "pve3", "pve4")
				logger, observed := log.NewObservedLogger(log.LevelDebug)
				if err := healAntiAffinityGroup(context.Background(), deps, aaIdent("web"), "", "", nil,
					logger); err != nil {
					t.Fatalf("the heal must write the group's rule: %v", err)
				}
				if got := stub.rules[aaRule]; got != tc.want {
					t.Fatalf("rule = %q; want %q", got, tc.want)
				}
				if tc.out != "" {
					requireLeftOutOnce(t, observed, tc.out)
				}
			})
		}
	}
}

// TestEnsureAntiAffinity_CreateWhoseOwnPinConflictsIsStillRefused pins that
// the plan never holds out the create's own VM. vm:101 is in the rule, and
// both it and the create's vm:103 are pinned strictly to pve1. The writer's
// check must refuse the join, so the caller fails or logs it the way its mode
// says, and the rule must keep its members.
func TestEnsureAntiAffinity_CreateWhoseOwnPinConflictsIsStillRefused(t *testing.T) {
	t.Parallel()
	for _, mode := range aaModes {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			stub := newAAStub()
			stub.enforceManaged = true
			managedGroup(stub, 101, 102, 103)
			stub.seedRule(aaRule, "vm:101,vm:102")
			seedStrictPin(stub, 101, "pve1")
			seedStrictPin(stub, 103, "pve1")
			deps := aaModeDeps(stub, mode, "", "pve1", "pve2", "pve3")
			logger, _ := log.NewObservedLogger(log.LevelDebug)
			_, err := ensureAntiAffinityMembershipFor(context.Background(), deps, aaIdent("web"), 103, nil, logger)
			if !errors.Is(err, ha.ErrRuleInfeasible) {
				t.Fatalf("err = %v; want the writer's refusal of the conflicting join", err)
			}
			if got := stub.rules[aaRule]; got != "vm:101,vm:102" {
				t.Fatalf("rule = %q; want it unchanged", got)
			}
		})
	}
}

// TestEnsureAntiAffinity_CreateAfterARepairLeavesTheRuleRight pins what a
// create does when only PVE reports the conflict of a sibling's pin. A repair
// took vm:103 out of the group's rule to keep its strict pin in effect, and
// the CPI's own checks see no conflict, so the create of vm:104 writes vm:103
// back. PVE flags the pin, the repair reverts the write, and the create's heal
// writes the rule with vm:104 and without vm:103. In managed mode the create
// still fails, because its writer's repair ran.
func TestEnsureAntiAffinity_CreateAfterARepairLeavesTheRuleRight(t *testing.T) {
	t.Parallel()
	for _, mode := range aaModes {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			stub := newAAStub()
			stub.enforceManaged = true
			managedGroup(stub, 101, 102, 103, 104)
			stub.seedRule(aaRule, "vm:101,vm:102")
			pin := seedStrictPin(stub, 103, "pve1")
			pinOutWhileInRule(stub, pin, aaRule, "vm:103")
			deps := aaModeDeps(stub, mode, "", "pve1", "pve2", "pve3", "pve4")
			logger, _ := log.NewObservedLogger(log.LevelDebug)
			err := ensureAntiAffinityMembership(context.Background(), deps, aaIdent("web"), 104, logger)
			switch {
			case mode == config.HAModeManaged && !errors.Is(err, ha.ErrRuleNotInEffect):
				t.Fatalf("err = %v; want the managed writer's repair to fail the create", err)
			case mode == config.HAModeAuto && err != nil:
				t.Fatalf("err = %v; want the auto writer to log the repair", err)
			}
			if got := stub.rules[aaRule]; got != "vm:101,vm:102,vm:104" {
				t.Fatalf("rule = %q; want the create joined and the pinned sibling left out", got)
			}
			if errs := stub.ruleErrors(pin); len(errs) > 0 {
				t.Fatalf("pin %s is out of effect: %v", pin, errs)
			}
		})
	}
}

// TestHealAntiAffinityGroup_HealsAgainAfterARepairConfinesAMember pins that a
// heal whose write a pin repair reverts runs its next round with the confined
// member held out, in managed mode as in auto. A removal deleted the group's
// rule, and only PVE reports that vm:103's strict pin conflicts with it, so
// the first round writes all three members. The repair reverts that create,
// which leaves no rule, and the second round must write the other two.
func TestHealAntiAffinityGroup_HealsAgainAfterARepairConfinesAMember(t *testing.T) {
	t.Parallel()
	for _, mode := range aaModes {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			stub := newAAStub()
			stub.enforceManaged = true
			managedGroup(stub, 101, 102, 103)
			pin := seedStrictPin(stub, 103, "pve1")
			pinOutWhileInRule(stub, pin, aaRule, "vm:103")
			deps := aaModeDeps(stub, mode, "", "pve1", "pve2", "pve3")
			logger, _ := log.NewObservedLogger(log.LevelDebug)
			deleted := map[string]string{aaRule: aaIdent("web").marker()}
			if err := healAfterUnlockedDeletes(context.Background(), deps, deleted, aaDirector, "", nil,
				logger); err != nil {
				t.Fatalf("the heal must write the rule again without the pinned member: %v", err)
			}
			if got := stub.rules[aaRule]; got != "vm:101,vm:102" {
				t.Fatalf("rule = %q; want the other two members, without the pinned one", got)
			}
			if errs := stub.ruleErrors(pin); len(errs) > 0 {
				t.Fatalf("pin %s is out of effect after the heal: %v", pin, errs)
			}
		})
	}
}
