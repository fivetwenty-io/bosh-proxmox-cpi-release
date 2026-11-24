package handlers

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestCheckAntiAffinityGroups_AnOffGroupWithLeftoverRules pins that the
// check of a group whose members record aa=off keeps off as its pattern,
// lists the delete of every leftover rule as drift, and judges no layout, so
// a rule of three members on three nodes is not reported as full.
func TestCheckAntiAffinityGroups_AnOffGroupWithLeftoverRules(t *testing.T) {
	t.Parallel()
	f := newAACheckFixture(t, 3, "off", 100, 101, 102, 103)
	f.rule(aaRule, "vm:100", "vm:101", "vm:102")
	f.rule(aaRule+"-2", "vm:103", "vm:100")
	c := oneCheck(t, f.in)
	if c.Pattern != "off" || !c.PatternRecorded {
		t.Fatalf("pattern = %q, recorded %v; want off, recorded", c.Pattern, c.PatternRecorded)
	}
	if c.Problem != "" || c.ForeignRule != "" || len(c.Full) != 0 || len(c.Layout) != 0 {
		t.Fatalf("check = %+v; want no problem, no full rule, and no layout", c)
	}
	for _, rule := range []string{aaRule, aaRule + "-2"} {
		if !slices.Contains(c.Drift, "delete "+rule) {
			t.Fatalf("drift = %v; want the delete of %s", c.Drift, rule)
		}
	}
}

// TestCheckAntiAffinityGroups_AnOffGroupWithNothingLeftReadsClean pins that
// an opted-out group with no rule and no hold has no drift and no hold.
func TestCheckAntiAffinityGroups_AnOffGroupWithNothingLeftReadsClean(t *testing.T) {
	t.Parallel()
	c := oneCheck(t, newAACheckFixture(t, 3, "off", 100, 101, 102).in)
	if c.Pattern != "off" || c.Problem != "" || len(c.Drift) != 0 || len(c.Holds) != 0 || len(c.LapsedHolds) != 0 {
		t.Fatalf("check = %+v; want a clean off group", c)
	}
}

// TestCheckAntiAffinityGroups_AnOffGroupReportsItsHoldsAsLeftovers pins that
// a hold an opted-out group still records from before the switch keeps no
// place, so the check lists it with the lapsed holds, which the fix drops,
// and never as a live hold.
func TestCheckAntiAffinityGroups_AnOffGroupReportsItsHoldsAsLeftovers(t *testing.T) {
	t.Parallel()
	f := newAACheckFixture(t, 3, "off", 100, 101, 102)
	until := strconv.FormatInt(f.in.Now.Add(10*time.Minute).Unix(), 10)
	f.in.Resources["vm:100"] += " hold=5.105@" + until
	c := oneCheck(t, f.in)
	if len(c.Holds) != 0 {
		t.Fatalf("live holds = %+v; want none for an off group", c.Holds)
	}
	if len(c.LapsedHolds) != 1 || c.LapsedHolds[0].Index != 5 || !slices.Equal(c.LapsedHolds[0].Carriers,
		[]string{"vm:100"}) {
		t.Fatalf("lapsed holds = %+v; want the hold of index 5 on vm:100", c.LapsedHolds)
	}
}

// TestCheckAntiAffinityGroups_AnOffGroupIsNeverConfined pins that the
// members of an opted-out group whose strict pins confine them to the same
// nodes get no confined problem, which the same group under block gets.
func TestCheckAntiAffinityGroups_AnOffGroupIsNeverConfined(t *testing.T) {
	t.Parallel()
	for pattern, want := range map[string]bool{"block": true, "off": false} {
		f := newAACheckFixture(t, 3, pattern, 100, 101)
		f.strictPin(100, "pve1", "pve2")
		f.strictPin(101, "pve2", "pve1")
		c := oneCheck(t, f.in)
		if got := c.Problem != ""; got != want {
			t.Fatalf("pattern %s: problem = %q; want a problem %v", pattern, c.Problem, want)
		}
	}
}

// TestFixAntiAffinityGroups_AnOffGroupLosesItsRulesAndHolds pins that pve-ops
// ha --fix-anti-affinity --apply on an opted-out group deletes every rule of
// the group and clears the holds recorded on its rules and HA resources, and
// that a second fix finds the group in its layout.
func TestFixAntiAffinityGroups_AnOffGroupLosesItsRulesAndHolds(t *testing.T) {
	t.Parallel()
	stub := newAAStub()
	stub.tagged(aaQEMU(100, aaTag+";index--0"), aaQEMU(101, aaTag+";index--1"), aaQEMU(102, aaTag+";index--2"),
		aaQEMU(103, aaTag+";index--3"))
	markWebPattern(stub, "off", 100, 101, 102, 103)
	until := strconv.FormatInt(time.Now().Add(10*time.Minute).Unix(), 10)
	stub.seedRuleWithComment(aaRule, "vm:100,vm:101,vm:102", aaRuleMarker()+" hold=7.107@"+until)
	stub.seedRule(aaRule+"-2", "vm:103,vm:100")
	stub.resComments["vm:101"] += " hold=6.106@" + until
	deps := aaMetadataDeps(stub)
	got, err := FixAntiAffinityGroups(context.Background(), deps, AntiAffinityFixOptions{DryRun: true})
	if err != nil || len(got) != 1 || got[0].Outcome != AntiAffinityWouldFix || got[0].Pattern != "off" {
		t.Fatalf("dry run = %+v, %v; want one off group that would be fixed", got, err)
	}
	if !strings.Contains(strings.Join(got[0].Drift, "; "), "hold of index 6") {
		t.Fatalf("dry run drift = %v; want the hold on vm:101 dropped", got[0].Drift)
	}
	got, err = FixAntiAffinityGroups(context.Background(), deps, AntiAffinityFixOptions{})
	if err != nil || len(got) != 1 || got[0].Outcome != AntiAffinityFixed {
		t.Fatalf("fix = %+v, %v; want the group fixed", got, err)
	}
	if rules := aaGroupRules(stub); len(rules) != 0 {
		t.Fatalf("rules = %v; want none", rules)
	}
	aaWantNoHolds(t, stub)
	got, err = FixAntiAffinityGroups(context.Background(), deps, AntiAffinityFixOptions{Rule: aaRule})
	if err != nil || len(got) != 1 || got[0].Outcome != AntiAffinityInLayout {
		t.Fatalf("second fix = %+v, %v; want the group in its layout", got, err)
	}
}
