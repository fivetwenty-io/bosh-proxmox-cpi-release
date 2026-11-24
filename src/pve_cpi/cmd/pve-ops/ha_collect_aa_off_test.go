package main

import (
	"context"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/aapattern"
)

// TestCollectAntiAffinityOffGroupWithLeftoverRules pins that a group whose
// members record aa=off and that still has rules gets one aa_group_off
// advice, which lists the deletes and names the fix, and no layout finding,
// so a rule of three members on three nodes is not reported as full.
func TestCollectAntiAffinityOffGroupWithLeftoverRules(t *testing.T) {
	t.Parallel()
	g := newAALayoutGroup()
	f := aaLayoutFake(g, "off", 3)
	f.rules = g.rulesOf([]string{"vm:200", "vm:201", "vm:202"})
	r := runCollect(t, f, haCfg("auto", "none"))
	want(t, r, haKindAAGroupOff, haSeverityAdvice, g.rule, "pattern off", "delete "+g.rule,
		"--fix-anti-affinity --apply")
	wantNone(t, r, haKindAARuleFull, haKindAALayoutDrift, haKindAAHoldLapsed, haKindAAPatternTrimmed,
		haKindAntiAffinityShort, haKindAntiAffinitySplit, haKindRuleProblem)
}

// TestCollectAntiAffinityOffGroupWithLeftoverHolds pins that an opted-out
// group with no rule left but a hold still recorded on a member's HA
// resource gets one aa_group_off advice that names the hold, and no
// aa_hold_lapsed advice of its own.
func TestCollectAntiAffinityOffGroupWithLeftoverHolds(t *testing.T) {
	t.Parallel()
	g := newAALayoutGroup()
	f := aaLayoutFake(g, "off", 2)
	f.resources[0].Comment += " hold=5.299@4000000000"
	r := runCollect(t, f, haCfg("auto", "none"))
	want(t, r, haKindAAGroupOff, haSeverityAdvice, g.rule, "hold of index 5", "vm:200")
	wantNone(t, r, haKindAAHoldLapsed, haKindAALayoutDrift)
}

// TestCollectAntiAffinityOffGroupWithNothingLeftReadsClean pins that an
// opted-out group with no rule and no hold gets no finding at all from the
// layout check.
func TestCollectAntiAffinityOffGroupWithNothingLeftReadsClean(t *testing.T) {
	t.Parallel()
	g := newAALayoutGroup()
	r := runCollect(t, aaLayoutFake(g, "off", 3), haCfg("auto", "none"))
	wantNone(t, r, haKindAAGroupOff, haKindAARuleFull, haKindAALayoutDrift, haKindAAHoldLapsed,
		haKindAntiAffinityShort, haKindAntiAffinitySplit, haKindAntiAffinitySkipped)
}

// TestCollectAntiAffinityOwnersThatDisagreeOnOff pins that off counts as a
// pattern when the entries that own a group disagree on their defaults, so a
// group whose members named none gets the advice that names both entries and
// their patterns.
func TestCollectAntiAffinityOwnersThatDisagreeOnOff(t *testing.T) {
	t.Parallel()
	g := newAALayoutGroup()
	f := aaLayoutFake(g, "", 6)
	f.rules = g.rulesOf([]string{"vm:200", "vm:201"}, []string{"vm:202", "vm:203"}, []string{"vm:204", "vm:205"})
	sc := aaEntryScope(t, true)
	sc.Cluster[1].Name, sc.Cluster[1].Pattern = "az2-off", aapattern.Off()
	sc.Peers = []string{"az2-off"}
	cfg := haCfgWith("auto", "none", `,"anti_affinity":"on"`, `,"placement":{"anti_affinity":{"enabled":true}}`)
	r := collectHAScoped(context.Background(), f, cfg, "", haNow, sc)
	want(t, r, haKindAntiAffinityAmbiguous, haSeverityAdvice, g.rule, "az2, az2-off", "block", "az2-off off")
	wantNone(t, r, haKindAAGroupOff, haKindAALayoutDrift, haKindAARuleFull)
}
