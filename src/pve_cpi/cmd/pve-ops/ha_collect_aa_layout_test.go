package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/aapattern"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// aaLayoutGroup is the "web" group of deployment "cf" owned by the test
// Director, with its rule name derived the way the CPI derives it.
type aaLayoutGroup struct {
	rule   string
	marker string
}

func newAALayoutGroup() aaLayoutGroup {
	sum := sha256.Sum256([]byte(haDirector + "\x00cf\x00web"))
	return aaLayoutGroup{
		rule:   "bosh-aa-" + hex.EncodeToString(sum[:])[:12],
		marker: haMarker + " deployment=cf group=web",
	}
}

// aaLayoutFake is a cluster of three nodes whose guests are the members of
// g, one per VMID from 200, each with its index tag and an HA resource that
// carries the group's marker and pattern.
func aaLayoutFake(g aaLayoutGroup, pattern string, members int) *fakeHA {
	f := haBaseFake()
	f.guests = nil
	f.resources = nil
	comment := g.marker
	if pattern != "" {
		comment += " aa=" + pattern
	}
	for i := range members {
		id := 200 + i
		f.guests = append(f.guests, pve.GuestRef{VMID: id, Node: "n" + strconv.Itoa(i%3+1), Status: "running",
			Tags: "bosh-cpi;" + g.rule + ";index--" + strconv.Itoa(i)})
		f.memory[id] = haGiB
		f.resources = append(f.resources, HAResourceRow{SID: "vm:" + strconv.Itoa(id), State: "started",
			Comment: comment + " born=" + strconv.Itoa(1700000000+i)})
	}
	return f
}

func (g aaLayoutGroup) rulesOf(names ...[]string) []ha.Rule {
	out := make([]ha.Rule, 0, len(names))
	for i, sids := range names {
		id := g.rule
		if i > 0 {
			id += "-" + strconv.Itoa(i+1)
		}
		out = append(out, ha.Rule{ID: id, Type: "resource-affinity", Affinity: "negative", Comment: g.marker,
			Resources: sids})
	}
	return out
}

// TestCollectAntiAffinityLayoutFullRuleAndDrift pins that a rule an older CPI
// filled to the node count is a problem that names the fix, and that the
// planned rewrite to one member fewer is reported as drift.
func TestCollectAntiAffinityLayoutFullRuleAndDrift(t *testing.T) {
	t.Parallel()
	g := newAALayoutGroup()
	f := aaLayoutFake(g, "", 3)
	f.rules = g.rulesOf([]string{"vm:200", "vm:201", "vm:202"})
	r := runCollect(t, f, haCfg("auto", "none"))
	want(t, r, haKindAARuleFull, haSeverityProblem, g.rule, "3 members on 3 nodes", "--fix-anti-affinity --apply")
	want(t, r, haKindAALayoutDrift, haSeverityAdvice, g.rule, "update "+g.rule+" with vm:200,vm:201")
	wantNone(t, r, haKindAAPatternTrimmed, haKindAAPatternBlocksDrain, haKindAntiAffinityShort,
		haKindAntiAffinitySplit, haKindAntiAffinitySkipped)
}

// TestCollectAntiAffinityLayoutInPlanReadsClean pins that a group whose rules
// hold the plan gets no layout finding. Its members record their pattern, as
// every member this release creates does, so the rules need no record of it.
func TestCollectAntiAffinityLayoutInPlanReadsClean(t *testing.T) {
	t.Parallel()
	g := newAALayoutGroup()
	f := aaLayoutFake(g, "block", 2)
	f.rules = g.rulesOf([]string{"vm:200", "vm:201"})
	wantNone(t, runCollect(t, f, haCfg("auto", "none")), haKindAARuleFull, haKindAALayoutDrift,
		haKindAAPatternTrimmed, haKindAAPatternBlocksDrain, haKindAntiAffinityShort)
}

// TestCollectAntiAffinityLayoutPatternFindings pins the two pattern findings.
// A window of two over six members on three nodes asks for rules that the
// drain-safety check leaves out, and the same pattern with drain safety given
// up is reported as one that blocks drains, while its full rules are advice.
func TestCollectAntiAffinityLayoutPatternFindings(t *testing.T) {
	t.Parallel()
	g := newAALayoutGroup()
	r := runCollect(t, aaLayoutFake(g, "window:2", 6), haCfg("auto", "none"))
	want(t, r, haKindAAPatternTrimmed, haSeverityAdvice, g.rule, "window:2", "drains")
	wantNone(t, r, haKindAAPatternBlocksDrain)

	f := aaLayoutFake(g, "window:2!unsafe", 3)
	f.rules = g.rulesOf([]string{"vm:200", "vm:201", "vm:202"})
	r = runCollect(t, f, haCfg("auto", "none"))
	want(t, r, haKindAAPatternBlocksDrain, haSeverityAdvice, g.rule, "drain_safe to false")
	want(t, r, haKindAARuleFull, haSeverityAdvice, g.rule, "drain_safe false allows")
}

// aaEntryScope is the scope of entry az2, band 200-299, which shares the
// cluster and band with entry az2-window, whose default pattern is window:2.
// writes says whether az2 itself writes anti-affinity rules.
func aaEntryScope(t *testing.T, writes bool) *haEntryScope {
	t.Helper()
	window, err := aapattern.Parse("window:2")
	if err != nil {
		t.Fatal(err)
	}
	band := pve.VMIDBand{Start: 200, End: 299}
	return &haEntryScope{
		Name: "az2", Band: band, AntiAffinity: writes, Leader: true,
		Peers: []string{"az2-window"}, PeerAntiAffinity: []bool{true}, PeerPolicies: []string{"none"}, Policy: "none",
		Cluster: []haClusterEntry{
			{Name: "az2", Band: band, AntiAffinity: writes, Pattern: aapattern.Default(), Self: true},
			{Name: "az2-window", Band: band, AntiAffinity: true, Pattern: window},
		},
	}
}

// TestCollectAntiAffinityLayoutUsesTheOwningEntrysDefault pins that under
// --cpi-config a group whose members named no pattern is judged by the entry
// that writes its rules, with that entry's default. The run of az2, which
// writes none, leaves the group alone. The run of az2-window, which shares
// its band and writes rules with window:2, plans the six members on three
// nodes with that window and trims it, though the run's own config keeps the
// block default and az2-window is not the band's first entry.
func TestCollectAntiAffinityLayoutUsesTheOwningEntrysDefault(t *testing.T) {
	t.Parallel()
	g := newAALayoutGroup()
	f := aaLayoutFake(g, "", 6)
	f.rules = g.rulesOf([]string{"vm:200", "vm:201"}, []string{"vm:202", "vm:203"}, []string{"vm:204", "vm:205"})
	off := haCfgWith("auto", "none", `,"anti_affinity":"off"`, `,"placement":{"anti_affinity":{"enabled":true}}`)
	r := collectHAScoped(context.Background(), f, off, "", haNow, aaEntryScope(t, false))
	wantNone(t, r, haKindAAPatternTrimmed, haKindAALayoutDrift, haKindAARuleFull)

	sc := aaEntryScope(t, false)
	sc.Name, sc.AntiAffinity, sc.Leader = "az2-window", true, false
	sc.Peers, sc.PeerAntiAffinity = []string{"az2"}, []bool{false}
	sc.Cluster[0].Self, sc.Cluster[1].Self = false, true
	on := haCfgWith("auto", "none", `,"anti_affinity":"on"`, `,"placement":{"anti_affinity":{"enabled":true}}`)
	r = collectHAScoped(context.Background(), f, on, "", haNow, sc)
	want(t, r, haKindAAPatternTrimmed, haSeverityAdvice, g.rule, "window:2")
}

// TestCollectAntiAffinityLayoutOwnersThatDisagree pins that a group whose
// owning entries set different default patterns, and whose members named
// none, is not judged against either layout and gets one advice that names
// the entries and their patterns. A group whose members named a pattern is
// planned with it and judged as usual.
func TestCollectAntiAffinityLayoutOwnersThatDisagree(t *testing.T) {
	t.Parallel()
	g := newAALayoutGroup()
	rules := g.rulesOf([]string{"vm:200", "vm:201"}, []string{"vm:202", "vm:203"}, []string{"vm:204", "vm:205"})
	cfg := haCfgWith("auto", "none", `,"anti_affinity":"on"`, `,"placement":{"anti_affinity":{"enabled":true}}`)

	f := aaLayoutFake(g, "", 6)
	f.rules = rules
	r := collectHAScoped(context.Background(), f, cfg, "", haNow, aaEntryScope(t, true))
	want(t, r, haKindAntiAffinityAmbiguous, haSeverityAdvice, g.rule, "az2, az2-window", "block", "window:2")
	wantNone(t, r, haKindAAPatternTrimmed, haKindAALayoutDrift, haKindAARuleFull)

	f = aaLayoutFake(g, "block", 6)
	f.rules = rules
	r = collectHAScoped(context.Background(), f, cfg, "", haNow, aaEntryScope(t, true))
	wantNone(t, r, haKindAntiAffinityAmbiguous, haKindAAPatternTrimmed, haKindAALayoutDrift, haKindAARuleFull)
}

// TestCollectAntiAffinityLayoutLapsedHolds pins that a hold the rules still
// record after it lapsed is advice, whether a member with its index took the
// place or it expired, and that neither reads as drift.
func TestCollectAntiAffinityLayoutLapsedHolds(t *testing.T) {
	t.Parallel()
	for hold, fragments := range map[string][]string{
		"1.199@4000000000": {"index 1 ", "taken by a member with its index", "--fix-anti-affinity --apply"},
		"5.299@1700000000": {"index 5 ", "expired at 2023-11-14T22:13:20Z", "--fix-anti-affinity --apply"},
	} {
		g := newAALayoutGroup()
		f := aaLayoutFake(g, "block", 2)
		f.rules = g.rulesOf([]string{"vm:200", "vm:201"})
		f.rules[0].Comment += " hold=" + hold
		r := runCollect(t, f, haCfg("auto", "none"))
		want(t, r, haKindAAHoldLapsed, haSeverityAdvice, append([]string{g.rule}, fragments...)...)
		wantNone(t, r, haKindAALayoutDrift, haKindAARuleFull)
	}
}

// TestCollectAntiAffinityLayoutLiveHoldReadsClean pins that a live hold of an
// index no member has is neither drift nor a lapsed hold.
func TestCollectAntiAffinityLayoutLiveHoldReadsClean(t *testing.T) {
	t.Parallel()
	g := newAALayoutGroup()
	f := aaLayoutFake(g, "block", 2)
	f.rules = g.rulesOf([]string{"vm:200", "vm:201"})
	f.rules[0].Comment += " hold=2.299@4000000000"
	wantNone(t, runCollect(t, f, haCfg("auto", "none")), haKindAAHoldLapsed, haKindAALayoutDrift,
		haKindAARuleFull)
}

// aaTwoNodeScope is the scope of one of three entries that share a cluster
// and a VM ID band, the way the lab's cpi-config lays them out. The first
// entry has no AZ map, the second writes no anti-affinity rules, and the third
// maps its one AZ to the nodes n1 and n2. self names the entry the run serves.
func aaTwoNodeScope(self string) *haEntryScope {
	band := pve.VMIDBand{Start: 200, End: 299}
	entries := []haClusterEntry{
		{Name: "az2", Band: band, AntiAffinity: true, Pattern: aapattern.Default()},
		{Name: "az2-soft", Band: band, AntiAffinity: false, Pattern: aapattern.Default()},
		{Name: "az2-two", Band: band, AntiAffinity: true, Pattern: aapattern.Default(),
			AZMap: map[string][]string{"p9two": {"n1", "n2"}}},
	}
	sc := &haEntryScope{Name: self, Band: band, Policy: "none", Leader: self == "az2"}
	for i := range entries {
		if entries[i].Name == self {
			entries[i].Self = true
			sc.AntiAffinity = entries[i].AntiAffinity
			continue
		}
		sc.Peers = append(sc.Peers, entries[i].Name)
		sc.PeerPolicies = append(sc.PeerPolicies, "none")
		sc.PeerAntiAffinity = append(sc.PeerAntiAffinity, entries[i].AntiAffinity)
	}
	sc.Cluster = entries
	return sc
}

// aaTwoNodePins gives each member of the fake the strict AZ pin the entry
// with the two-node AZ writes, which names n1 and n2 and ranks the member's
// own node first.
func aaTwoNodePins(f *fakeHA) {
	for _, g := range f.guests {
		nodes := map[string]int{"n1": 1, "n2": 1}
		nodes[g.Node] = 2
		f.rules = append(f.rules, ha.Rule{ID: "bosh-na-" + strconv.Itoa(g.VMID), Type: "node-affinity",
			Strict: true, Nodes: nodes, Resources: []string{"vm:" + strconv.Itoa(g.VMID)},
			Comment: haMarker + " deployment=- group=-"})
	}
}

// TestCollectAntiAffinityLayoutJudgesAGroupByTheEntryThatPlacedIt pins that
// under --cpi-config a group is judged only by the entry whose AZ map holds
// its members' strict pins. Two members pinned to the two-node AZ of az2-two
// get no rule there, so az2, which has no AZ map and would plan the group
// over the cluster's three nodes, does not judge it and reports no drift, and
// az2-two does judge it, though it is not the first entry of the band, and
// reports a full rule the two-node AZ cannot hold.
func TestCollectAntiAffinityLayoutJudgesAGroupByTheEntryThatPlacedIt(t *testing.T) {
	t.Parallel()
	g := newAALayoutGroup()
	plain := haCfgWith("auto", "none", `,"anti_affinity":"on"`, `,"placement":{"anti_affinity":{"enabled":true}}`)
	two := haCfgWith("auto", "none", `,"anti_affinity":"on"`,
		`,"placement":{"anti_affinity":{"enabled":true},"az_map":{"p9two":["n1","n2"]}}`)
	layoutKinds := []string{haKindAALayoutDrift, haKindAARuleFull, haKindAAPatternTrimmed, haKindAntiAffinityShort,
		haKindAntiAffinitySplit, haKindAntiAffinitySkipped, haKindAntiAffinityAmbiguous}

	f := aaLayoutFake(g, "block", 2)
	aaTwoNodePins(f)
	wantNone(t, collectHAScoped(context.Background(), f, plain, haDirector, haNow, aaTwoNodeScope("az2")),
		layoutKinds...)
	wantNone(t, collectHAScoped(context.Background(), f, two, haDirector, haNow, aaTwoNodeScope("az2-two")),
		layoutKinds...)

	f = aaLayoutFake(g, "block", 2)
	aaTwoNodePins(f)
	f.rules = append(f.rules, g.rulesOf([]string{"vm:200", "vm:201"})...)
	wantNone(t, collectHAScoped(context.Background(), f, plain, haDirector, haNow, aaTwoNodeScope("az2")),
		layoutKinds...)
	r := collectHAScoped(context.Background(), f, two, haDirector, haNow, aaTwoNodeScope("az2-two"))
	want(t, r, haKindAARuleFull, haSeverityProblem, g.rule, "2 members on 2 nodes")
	want(t, r, haKindAALayoutDrift, haSeverityAdvice, g.rule, "delete "+g.rule)
}
