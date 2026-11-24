package handlers

import (
	"context"
	"maps"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// markWebPattern marks each VMID a member of the "web" group whose create
// named pattern, born in VMID order so the highest VMID is the newest.
func markWebPattern(stub *aaClusterStub, pattern string, vmids ...int) {
	for _, v := range vmids {
		stub.resources[haResourceSid(v)] = true
		stub.resComments[haResourceSid(v)] = aaIdent("web").resourceMarker(pattern, 1700000000+int64(v))
	}
}

// aaWantNoHolds fails the test when any rule or HA resource of the stub
// still records a hold.
func aaWantNoHolds(t *testing.T, stub *aaClusterStub) {
	t.Helper()
	for sid, c := range stub.resComments {
		if strings.Contains(c, " hold=") {
			t.Errorf("comment of %s = %q; want no hold", sid, c)
		}
	}
	for name, c := range stub.ruleComments {
		if strings.Contains(c, " hold=") {
			t.Errorf("comment of rule %s = %q; want no hold", name, c)
		}
	}
}

// aaGroupRules returns the stub's rules of the "web" group.
func aaGroupRules(stub *aaClusterStub) map[string]string {
	out := map[string]string{}
	for name, csv := range stub.rules {
		if strings.HasPrefix(name, aaRule) {
			out[name] = csv
		}
	}
	return out
}

// TestAAOff_AJoinDeletesEveryRuleOfTheGroup pins that once the newest member
// of a group records aa=off, its set_vm_metadata join deletes every rule of
// the group and writes none, with the rules lock on and off. Afterwards the
// doctor's plan of the group writes nothing.
func TestAAOff_AJoinDeletesEveryRuleOfTheGroup(t *testing.T) {
	t.Parallel()
	for _, lock := range []bool{false, true} {
		t.Run("lock "+strconv.FormatBool(lock), func(t *testing.T) {
			t.Parallel()
			stub := newAAStub()
			newAAHoldFleet(stub, 100, 101, 102, 103, 104)
			markWebPattern(stub, "block", 100, 101, 102, 103)
			markWebPattern(stub, "off", 104)
			stub.seedRule(aaRule, "vm:100,vm:101,vm:102")
			stub.seedRule(aaRule+"-2", "vm:103,vm:104")
			deps := aaReplanDeps(stub, lock)
			joinAntiAffinityAtMetadata(context.Background(), deps, 104, map[string]any{"index": float64(4)},
				log.NewNopLogger())
			if got := aaGroupRules(stub); len(got) != 0 {
				t.Fatalf("rules = %v; want every rule of the opted-out group deleted", got)
			}
			if stub.createRuleCalls != 0 {
				t.Fatalf("create calls = %d; want no rule written", stub.createRuleCalls)
			}
			aaWantNoHolds(t, stub)
			if d := aaDoctorDrift(t, deps); len(d) != 0 {
				t.Fatalf("drift after the join = %v; want none", d)
			}
		})
	}
}

// TestAAOff_ADeleteRecordsNoHold pins that the delete of a member of an
// opted-out group records no hold, before or after its destroy, and plans no
// rule, both with rules left over from before the switch and without any.
func TestAAOff_ADeleteRecordsNoHold(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		lock     bool
		leftover bool
	}{
		"no rules":               {},
		"no rules, locked":       {lock: true},
		"leftover rules":         {leftover: true},
		"leftover rules, locked": {lock: true, leftover: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			stub := newAAStub()
			stub.purgeFromRules = true
			fleet := newAAHoldFleet(stub, 100, 101, 102, 103)
			markWebPattern(stub, "off", 100, 101, 102, 103)
			if tc.leftover {
				stub.seedRule(aaRule, "vm:100,vm:101,vm:102")
			}
			deps := aaReplanDeps(stub, tc.lock)
			if _, err := holdAntiAffinityPlacesBeforeDestroy(ctx, deps, 101, webTags(101),
				log.NewNopLogger()); err != nil {
				t.Fatalf("hold before the destroy: %v", err)
			}
			aaWantNoHolds(t, stub)
			fleet.destroy(101)
			if err := removeAntiAffinityMembership(ctx, deps, 101, webTags(101), log.NewNopLogger()); err != nil {
				t.Fatalf("remove: %v", err)
			}
			if got := aaGroupRules(stub); len(got) != 0 {
				t.Fatalf("rules = %v; want none for an opted-out group", got)
			}
			aaWantNoHolds(t, stub)
			if d := aaDoctorDrift(t, deps); len(d) != 0 {
				t.Fatalf("drift after the delete = %v; want none", d)
			}
		})
	}
}

// TestAAOff_ClearsTheHoldsFromBeforeTheSwitch pins that a hold recorded
// under the old pattern, on a rule and on a member's HA resource, is cleared
// at the group's first plan once its newest member records aa=off, instead
// of being kept until it lapses.
func TestAAOff_ClearsTheHoldsFromBeforeTheSwitch(t *testing.T) {
	t.Parallel()
	for _, lock := range []bool{false, true} {
		t.Run("lock "+strconv.FormatBool(lock), func(t *testing.T) {
			t.Parallel()
			stub := newAAStub()
			newAAHoldFleet(stub, 100, 102, 103, 104)
			markWebPattern(stub, "block", 100, 102, 103)
			markWebPattern(stub, "off", 104)
			until := strconv.FormatInt(time.Now().Add(10*time.Minute).Unix(), 10)
			stub.seedRuleWithComment(aaRule, "vm:100,vm:102,vm:103", aaRuleMarker()+" hold=1.101@"+until)
			stub.resComments["vm:100"] += " hold=5.105@" + until
			deps := aaReplanDeps(stub, lock)
			joinAntiAffinityAtMetadata(context.Background(), deps, 104, map[string]any{"index": float64(4)},
				log.NewNopLogger())
			if got := aaGroupRules(stub); len(got) != 0 {
				t.Fatalf("rules = %v; want none", got)
			}
			aaWantNoHolds(t, stub)
			if d := aaDoctorDrift(t, deps); len(d) != 0 {
				t.Fatalf("drift after the join = %v; want none", d)
			}
		})
	}
}

// TestAAOff_RestoresThePinDespiteAHoldFromBeforeTheSwitch pins that a member
// that leaves its last rule because its group opted out gets the priorities
// of its pin back at the same join, even though the rule recorded a live
// hold, because the off pattern keeps no hold to wait for.
func TestAAOff_RestoresThePinDespiteAHoldFromBeforeTheSwitch(t *testing.T) {
	t.Parallel()
	stub, deps, pin := restoreFixture(t)
	joinWeb(t, deps, 103, 3, log.NewNopLogger())
	if got := stub.rules[aaRule]; got != "vm:101,vm:102,vm:103" {
		t.Fatalf("rule = %q", got)
	}
	until := strconv.FormatInt(time.Now().Add(10*time.Minute).Unix(), 10)
	stub.ruleComments[aaRule] += " hold=0.99@" + until
	stub.tagged(aaQEMU(101, aaTag+";index--1"), aaQEMU(102, aaTag+";index--2"), aaQEMU(103, aaTag+";index--3"),
		aaQEMU(104, aaTag+";index--4"))
	stub.resComments["vm:104"] = aaIdent("web").resourceMarker("off", 1800000000)
	joinWeb(t, deps, 104, 4, log.NewNopLogger())
	if got := aaGroupRules(stub); len(got) != 0 {
		t.Fatalf("rules = %v; want the opted-out group's rule deleted", got)
	}
	if got := stub.ruleNodes[pin]; got != restoreLabPin {
		t.Fatalf("pin = %q; want %q restored at the join that deleted the rule", got, restoreLabPin)
	}
	aaWantNoHolds(t, stub)
}

// TestAAOff_TheMoverLeavesAnOptedOutJoiner pins that set_vm_metadata does not
// move a joiner of an opted-out group off a member's node, because no rule
// will pair them.
func TestAAOff_TheMoverLeavesAnOptedOutJoiner(t *testing.T) {
	t.Parallel()
	stub := coloPair("pve1", "pve1")
	for _, v := range []int{100, 101} {
		stub.resComments[haResourceSid(v)] = aaIdent("web").resourceMarker("off", coloNow().Add(-time.Minute).Unix())
	}
	h := newColoHarness()
	coloJoin(coloDeps(stub, h, coloClaimDir(t)), 101, "1", log.NewNopLogger())
	coloWantNoMove(t, h)
	if got := aaGroupRules(stub); len(got) != 0 {
		t.Fatalf("rules = %v; want none", got)
	}
}

// TestAAOff_TheNewestMemberDecidesBothWays pins the newest-member rule in
// both directions. A group whose newest member records off while the older
// ones record block plans no rule, and a group whose newest member records
// block while the older ones record off plans the block rules again.
func TestAAOff_TheNewestMemberDecidesBothWays(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		older, newest string
		seeded        bool
		want          map[string]string
	}{
		"off after block": {older: "block", newest: "off", seeded: true, want: map[string]string{}},
		"block after off": {older: "off", newest: "block", want: map[string]string{aaRule: "vm:100,vm:101,vm:102"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			stub := newAAStub()
			newAAHoldFleet(stub, 100, 101, 102, 103)
			markWebPattern(stub, tc.older, 100, 101, 102)
			markWebPattern(stub, tc.newest, 103)
			if tc.seeded {
				stub.seedRule(aaRule, "vm:100,vm:101,vm:102")
			}
			deps := aaReplanDeps(stub, false)
			joinAntiAffinityAtMetadata(context.Background(), deps, 103, map[string]any{"index": float64(3)},
				log.NewNopLogger())
			if got := aaGroupRules(stub); !maps.Equal(got, tc.want) {
				t.Fatalf("rules = %v; want %v", got, tc.want)
			}
			if d := aaDoctorDrift(t, deps); len(d) != 0 {
				t.Fatalf("drift after the join = %v; want none", d)
			}
		})
	}
}

// TestAAOff_ASettledJoinSkipsTheRulesLock pins that a join in an opted-out
// group that has nothing to write takes the settled path, which skips the
// rules lock, and reports no rule, as a block group whose rule is in place
// does. A join that still finds a leftover rule to delete takes the lock.
func TestAAOff_ASettledJoinSkipsTheRulesLock(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		pattern  string
		seed     string
		settled  bool
		wantRule string
	}{
		"off, nothing to write":    {pattern: "off", settled: true},
		"off, a rule to delete":    {pattern: "off", seed: "vm:100,vm:101,vm:102"},
		"block, its rule in place": {pattern: "block", seed: "vm:100,vm:101,vm:102", settled: true, wantRule: aaRule},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			stub := newAAStub()
			newAAHoldFleet(stub, 100, 101, 102)
			markWebPattern(stub, tc.pattern, 100, 101, 102)
			if tc.seed != "" {
				stub.seedRule(aaRule, tc.seed)
			}
			rule, ok := antiAffinityJoinSettled(context.Background(), aaReplanDeps(stub, true), aaIdent("web"), 102, 2)
			if ok != tc.settled || rule != tc.wantRule {
				t.Fatalf("settled = (%q, %v); want (%q, %v)", rule, ok, tc.wantRule, tc.settled)
			}
		})
	}
}
