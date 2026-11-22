package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/aapattern"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
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

// TestCollectAntiAffinityLayoutPinRestore pins that a member in no rule whose
// pin still records the priorities its join flattened is advice that names
// the priorities and the fix, that a live hold of the group says the restore
// waits, and that a pin changed after the join says the record is dropped.
func TestCollectAntiAffinityLayoutPinRestore(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		nodes, hold string
		fragments   []string
	}{
		"restore": {nodes: "n1,n2,n3", fragments: []string{"vm:202", "writes back the priorities n1,n2:2",
			"--fix-anti-affinity --apply"}},
		"deferred": {nodes: "n1,n2,n3", hold: " hold=5.299@4000000000", fragments: []string{"vm:202",
			"live holds lapse", "writes back the priorities n1,n2:2"}},
		"hand edit": {nodes: "n1:3,n2,n3", fragments: []string{"vm:202", "several priorities again",
			"drops the record"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			g := newAALayoutGroup()
			f := aaLayoutFake(g, "block", 3)
			f.rules = g.rulesOf([]string{"vm:200", "vm:201"})
			f.rules[0].Comment += tc.hold
			nodes := map[string]int{}
			for e := range strings.SplitSeq(tc.nodes, ",") {
				n, p, _ := strings.Cut(e, ":")
				nodes[n], _ = strconv.Atoi(p)
			}
			f.rules = append(f.rules, ha.Rule{ID: "bosh-na-202", Type: "node-affinity", Nodes: nodes,
				Resources: []string{"vm:202"}, Comment: haMarker + " deployment=- group=- " + ha.PinRestoreKey + "=n1,n2:2"})
			r := runCollect(t, f, haCfg("auto", "none"))
			want(t, r, haKindAAPinRestore, haSeverityAdvice, append([]string{"bosh-na-202"}, tc.fragments...)...)
			wantNone(t, r, haKindAALayoutDrift)
		})
	}
}

// aaCrossBandScope is the scope of one of two entries that share a cluster
// but not a band. Entry a holds VM 200 and has no AZ map, and entry b holds
// VM 201 and maps its one AZ to the nodes n1 and n2. Both write rules, and
// patterns gives each entry's default in that order. self names the entry
// the run serves.
func aaCrossBandScope(t *testing.T, self string, patterns ...string) *haEntryScope {
	t.Helper()
	defaults := []aapattern.Pattern{aapattern.Default(), aapattern.Default()}
	for i, text := range patterns {
		p, err := aapattern.Parse(text)
		if err != nil {
			t.Fatal(err)
		}
		defaults[i] = p
	}
	entries := []haClusterEntry{
		{Name: "a", Band: pve.VMIDBand{Start: 200, End: 200}, AntiAffinity: true, Pattern: defaults[0]},
		{Name: "b", Band: pve.VMIDBand{Start: 201, End: 201}, AntiAffinity: true, Pattern: defaults[1],
			AZMap: map[string][]string{"p9two": {"n1", "n2"}}},
	}
	sc := &haEntryScope{Name: self, Policy: "none", AntiAffinity: true, Leader: true}
	for i := range entries {
		if entries[i].Name == self {
			entries[i].Self = true
			sc.Band = entries[i].Band
		}
	}
	sc.Cluster = entries
	return sc
}

// TestCollectAntiAffinityLayoutJudgesAGroupAcrossBandsOnce pins that a group
// whose members lie in two entries' bands is judged by exactly one entry,
// chosen over the whole group. Both members carry the strict pin of b's
// two-node AZ, so b placed the group, and only b's run reports the full rule.
// Entry a, which holds one member in its band and has no AZ map, stays quiet.
func TestCollectAntiAffinityLayoutJudgesAGroupAcrossBandsOnce(t *testing.T) {
	t.Parallel()
	g := newAALayoutGroup()
	plain := haCfgWith("auto", "none", `,"anti_affinity":"on"`, `,"placement":{"anti_affinity":{"enabled":true}}`)
	two := haCfgWith("auto", "none", `,"anti_affinity":"on"`,
		`,"placement":{"anti_affinity":{"enabled":true},"az_map":{"p9two":["n1","n2"]}}`)
	layoutKinds := []string{haKindAALayoutDrift, haKindAARuleFull, haKindAAPatternTrimmed, haKindAntiAffinityShort,
		haKindAntiAffinitySplit, haKindAntiAffinitySkipped, haKindAntiAffinityAmbiguous}
	for _, rules := range [][]ha.Rule{nil, g.rulesOf([]string{"vm:200", "vm:201"})} {
		f := aaLayoutFake(g, "block", 2)
		aaTwoNodePins(f)
		f.rules = append(f.rules, rules...)
		wantNone(t, collectHAScoped(context.Background(), f, plain, haDirector, haNow, aaCrossBandScope(t, "a")),
			layoutKinds...)
		r := collectHAScoped(context.Background(), f, two, haDirector, haNow, aaCrossBandScope(t, "b"))
		if rules != nil {
			want(t, r, haKindAARuleFull, haSeverityProblem, g.rule, "2 members on 2 nodes")
		}
	}
}

// TestCollectAntiAffinityLayoutSplitDefaultsAcrossBands pins that the entries
// that write a group's rules are found over the whole group, so a group whose
// members lie in the bands of two entries with different defaults, and whose
// members named no pattern, gets one advice that names both entries, and
// neither entry judges its layout with its own default.
func TestCollectAntiAffinityLayoutSplitDefaultsAcrossBands(t *testing.T) {
	t.Parallel()
	g := newAALayoutGroup()
	cfg := haCfgWith("auto", "none", `,"anti_affinity":"on"`, `,"placement":{"anti_affinity":{"enabled":true}}`)
	f := aaLayoutFake(g, "", 2)
	f.rules = g.rulesOf([]string{"vm:200", "vm:201"})
	advised := 0
	for _, self := range []string{"a", "b"} {
		r := collectHAScoped(context.Background(), f, cfg, haDirector, haNow, aaCrossBandScope(t, self, "block", "window:2"))
		wantNone(t, r, haKindAALayoutDrift, haKindAARuleFull, haKindAAPatternTrimmed)
		for _, fd := range findings(r, haKindAntiAffinityAmbiguous) {
			if !strings.Contains(fd.Detail, "a, b") || !strings.Contains(fd.Detail, "window:2") {
				t.Errorf("entry %s: the advice must name both entries and their defaults: %+v", self, fd)
			}
			advised++
		}
	}
	if advised != 1 {
		t.Errorf("anti_affinity_ambiguous appeared %d times over both entries, want once", advised)
	}
}

// TestCollectAntiAffinityLayoutPlacerBelongsToTheGroupsDirector pins that an
// entry of another Director can't claim a group, though its band holds a
// member and its AZ map fits the members' pins best. The group's members carry
// this Director's owner tag, so entry b, which serves another Director, never
// sees them, and entry a judges the group.
func TestCollectAntiAffinityLayoutPlacerBelongsToTheGroupsDirector(t *testing.T) {
	t.Parallel()
	g := newAALayoutGroup()
	plain := haCfgWith("auto", "none", `,"anti_affinity":"on"`, `,"placement":{"anti_affinity":{"enabled":true}}`)
	f := aaLayoutFake(g, "block", 2)
	aaTwoNodePins(f)
	for i := range f.guests {
		f.guests[i].Tags += ";bosh-owner--" + haDirector
	}
	sc := aaCrossBandScope(t, "a")
	sc.Cluster[0].Owner, sc.Cluster[1].Owner = haDirector, "bbbbbbbb-0000"
	r := collectHAScoped(context.Background(), f, plain, haDirector, haNow, sc)
	want(t, r, haKindAALayoutDrift, haSeverityProblem, g.rule, "could not be planned")
}

// holdGroupEntries are two entries of one Director that share a cluster and a
// band, the way the lab's az2 and az2-client do. az2 has no AZ map, and
// az2-client maps each node to its own AZ. Both use the hold policy and write
// anti-affinity rules.
func holdGroupEntries() []cpiConfigEntry {
	haProps := map[string]any{"local_policy": "hold", "anti_affinity": "on"}
	return []cpiConfigEntry{
		{Name: "az2", Type: "pve", Properties: map[string]any{"pve_ha": haProps,
			"pve_placement": map[string]any{"anti_affinity": map[string]any{"enabled": true}}}},
		{Name: "az2-client", Type: "pve", Properties: map[string]any{"pve_ha": haProps,
			"pve_placement": map[string]any{"anti_affinity": map[string]any{"enabled": true},
				"az_map": map[string]any{"c1": []any{"n1"}, "c2": []any{"n2"}, "c3": []any{"n3"}}}}},
	}
}

// holdGroupFake is a block group of three hold members, each held to its own
// node by a strict one-node pin. The plan of an entry with no AZ map puts the
// first two in the group's base rule, which az2 wrote.
func holdGroupFake(g aaLayoutGroup) *fakeHA {
	f := aaLayoutFake(g, "block", 3)
	for _, guest := range f.guests {
		f.rules = append(f.rules, pinRule(guest.VMID, haMarker+" deployment=- group=-", guest.Node))
	}
	f.rules = append(f.rules, g.rulesOf([]string{"vm:200", "vm:201"})...)
	for i := range f.guests {
		f.guests[i].Tags += ";bosh-owner--" + haDirector
	}
	return f
}

// TestCollectAntiAffinityLayoutHoldGroupBelongsToTheEntryWhoseRulesItHolds
// pins that a group of hold members, each strictly pinned to its own node, is
// judged by the entry whose plan holds the group's rules, not by the entry
// whose one-node AZs fit the pins most tightly. az2's plan holds the base rule
// as it stands, and az2-client's plan would put each member in its own
// one-node AZ and delete the rule, so az2 judges the group and finds nothing,
// and neither run calls the rule full or advises deleting it.
func TestCollectAntiAffinityLayoutHoldGroupBelongsToTheEntryWhoseRulesItHolds(t *testing.T) {
	t.Parallel()
	g := newAALayoutGroup()
	entries := holdGroupEntries()
	base := haCfg("auto", "hold")
	scopes := entryScopes(entries, base, []string{haDirector, haDirector})
	layoutKinds := []string{haKindAALayoutDrift, haKindAARuleFull, haKindAAPatternTrimmed, haKindAntiAffinityShort,
		haKindAntiAffinitySplit, haKindAntiAffinitySkipped, haKindAntiAffinityAmbiguous}
	for i := range entries {
		eff, _, _, err := config.ApplyContextOverrides(base, entries[i].Properties)
		if err != nil {
			t.Fatal(err)
		}
		r := collectHAScoped(context.Background(), holdGroupFake(g), eff, haDirector, haNow, scopes[i])
		wantNone(t, r, layoutKinds...)
	}
}

// holdEntryConfig returns the effective settings of entry i of
// holdGroupEntries, which a run with that entry's request judges under.
func holdEntryConfig(t *testing.T, i int) *config.CPIConfig {
	t.Helper()
	eff, _, _, err := config.ApplyContextOverrides(haCfg("auto", "hold"), holdGroupEntries()[i].Properties)
	if err != nil {
		t.Fatal(err)
	}
	return eff
}

// TestCollectAntiAffinityLayoutHoldPinsDoNotConfineARule pins that a rule
// whose members each hold a strict pin to a node of their own is judged as
// planned. The hold policy writes those pins and a drain moves none of the
// members, so the rule is no sign of another entry's AZ. Members whose pins
// share their nodes are still a rule the plan cannot judge, and a run of one
// entry reports it as advice, because another entry's AZ map may own the group.
func TestCollectAntiAffinityLayoutHoldPinsDoNotConfineARule(t *testing.T) {
	t.Parallel()
	g := newAALayoutGroup()
	wantNone(t, runCollect(t, holdGroupFake(g), holdEntryConfig(t, 0)), haKindAALayoutDrift, haKindAARuleFull)

	f := holdGroupFake(g)
	for i := range f.rules {
		if f.rules[i].ID == "bosh-na-201" {
			f.rules[i].Nodes = map[string]int{"n1": 0}
		}
	}
	want(t, runCollect(t, f, holdEntryConfig(t, 0)), haKindAALayoutDrift, haSeverityAdvice, g.rule,
		"could not be planned", "leave them only the nodes n1", aaOneEntryReason)
}

// TestCollectAntiAffinityLayoutRuleAcrossDomainsIsNotDeleted pins that a run
// whose AZ map puts the members of one of the group's rules in different
// domains does not plan the group, because the CPI never writes a rule across
// domains, so another entry wrote it. The run reports why and offers no
// delete, so a fix with that entry's request leaves the rule alone. A run of
// one entry can't see that entry, so it reports the group as advice that
// names --cpi-config rather than as a problem.
func TestCollectAntiAffinityLayoutRuleAcrossDomainsIsNotDeleted(t *testing.T) {
	t.Parallel()
	g := newAALayoutGroup()
	r := runCollect(t, holdGroupFake(g), holdEntryConfig(t, 1))
	want(t, r, haKindAALayoutDrift, haSeverityAdvice, g.rule, "could not be planned", "vm:200, vm:201",
		"different domains of placement.az_map (c1, c2)", "check the group under --cpi-config", aaOneEntryReason)
	if r.Problems() != 0 {
		t.Errorf("a run of one entry counts a group another entry's AZ map may own as a problem: %+v", r.Findings)
	}
	wantNone(t, r, haKindAARuleFull)
	for _, fd := range r.Findings {
		if strings.Contains(fd.Detail, "delete "+g.rule) {
			t.Errorf("a run under another entry's AZ map offers to delete the rule: %+v", fd)
		}
	}
}

// TestCollectAntiAffinityLayoutContestedDeleteIsNotOffered pins that the
// entry that judges a group does not offer to delete a rule that another of
// the group's owners plans. Three members on the pattern window:2 carry strict
// pins to every node. The entry az1 maps one AZ to all three nodes, so it plans
// the group's rules in that AZ, and az2 has no AZ map, so it plans them over
// the cluster, and the two plans name their rules apart. The cluster holds
// both plans' rules. Each plan deletes the other's rules, and az1 judges the
// group because its AZ holds the pins, so it reports the conflict as advice
// instead of the delete, and az2 reports nothing.
func TestCollectAntiAffinityLayoutContestedDeleteIsNotOffered(t *testing.T) {
	t.Parallel()
	g := newAALayoutGroup()
	haProps := map[string]any{"local_policy": "none", "anti_affinity": "on"}
	entries := []cpiConfigEntry{
		{Name: "az1", Type: "pve", Properties: map[string]any{"pve_ha": haProps,
			"pve_placement": map[string]any{"anti_affinity": map[string]any{"enabled": true},
				"az_map": map[string]any{"z1": []any{"n1", "n2", "n3"}}}}},
		{Name: "az2", Type: "pve", Properties: map[string]any{"pve_ha": haProps,
			"pve_placement": map[string]any{"anti_affinity": map[string]any{"enabled": true}}}},
	}
	base := haCfg("auto", "none")
	effs := make([]*config.CPIConfig, len(entries))
	for i := range entries {
		eff, _, _, err := config.ApplyContextOverrides(base, entries[i].Properties)
		if err != nil {
			t.Fatal(err)
		}
		effs[i] = eff
	}
	fake := func() *fakeHA {
		f := aaLayoutFake(g, "window:2", 3)
		for _, guest := range f.guests {
			f.rules = append(f.rules, pinRule(guest.VMID, haMarker+" deployment=- group=-", "n1", "n2", "n3"))
		}
		for k := range f.guests {
			f.guests[k].Tags += ";bosh-owner--" + haDirector
		}
		return f
	}
	// The rule each entry's plan creates, read from what a run under that
	// entry alone would write on a cluster with no group rule.
	creates := make([]string, len(entries))
	for i := range entries {
		for _, d := range findings(runCollect(t, fake(), effs[i]), haKindAALayoutDrift) {
			if _, rest, ok := strings.Cut(d.Detail, "create "); ok {
				creates[i], _, _ = strings.Cut(rest, " ")
			}
		}
	}
	if creates[0] == "" || creates[1] == "" || creates[0] == creates[1] {
		t.Fatalf("the entries' plans create %q, want one rule each, named apart", creates)
	}
	scopes := entryScopes(entries, base, []string{haDirector, haDirector})
	run := func(i int) *haReport {
		f := fake()
		for _, id := range creates {
			f.rules = append(f.rules, ha.Rule{ID: id, Type: "resource-affinity", Affinity: "negative",
				Comment: g.marker, Resources: []string{"vm:200", "vm:201"}})
		}
		return collectHAScoped(context.Background(), f, effs[i], haDirector, haNow, scopes[i])
	}
	judge := run(0)
	want(t, judge, haKindAntiAffinityAmbiguous, haSeverityAdvice, g.rule, "deletes the rule "+creates[1],
		"the cpi-config entry az2 keeps")
	wantNone(t, judge, haKindAALayoutDrift, haKindAARuleFull)
	wantNone(t, run(1), haKindAALayoutDrift, haKindAARuleFull, haKindAntiAffinityAmbiguous)
}

// TestCollectAntiAffinityLayoutPinRestoreGoesToTheEntryWhoseAZThePinPrefers
// pins that the doctor reports a pin restore on the entry whose AZ the
// member's soft pin prefers. Two entries reach one cluster and band. The
// first has no AZ map, and the second maps each node to an AZ of its own and
// writes soft AZ pins. The group's one member left sits in no rule, and its
// flattened pin records that it preferred n1, which only the second entry's
// AZ c1 names. Neither entry's plan writes anything, and no strict pin tells
// them apart, so the preference decides, and only the second entry's run
// reports the restore.
func TestCollectAntiAffinityLayoutPinRestoreGoesToTheEntryWhoseAZThePinPrefers(t *testing.T) {
	t.Parallel()
	g := newAALayoutGroup()
	haProps := map[string]any{"local_policy": "hold", "anti_affinity": "on"}
	entries := []cpiConfigEntry{
		{Name: "az2", Type: "pve", Properties: map[string]any{"pve_ha": haProps,
			"pve_placement": map[string]any{"anti_affinity": map[string]any{"enabled": true}}}},
		{Name: "client", Type: "pve", Properties: map[string]any{
			"pve_ha": map[string]any{"local_policy": "hold", "anti_affinity": "on", "pin_az": "on",
				"pin_az_strict": false},
			"pve_placement": map[string]any{"anti_affinity": map[string]any{"enabled": true},
				"az_map": map[string]any{"c1": []any{"n1"}, "c2": []any{"n2"}, "c3": []any{"n3"}}}}},
	}
	base := haCfg("auto", "hold")
	effs := make([]*config.CPIConfig, len(entries))
	for i := range entries {
		eff, _, _, err := config.ApplyContextOverrides(base, entries[i].Properties)
		if err != nil {
			t.Fatal(err)
		}
		effs[i] = eff
	}
	scopes := entryScopes(entries, base, []string{haDirector, haDirector})
	run := func(i int) *haReport {
		f := aaLayoutFake(g, "block", 1)
		f.guests[0].Tags += ";bosh-owner--" + haDirector
		pin := pinRule(200, haMarker+" deployment=- group=- "+ha.PinRestoreKey+"=n1:2,n2,n3", "n1", "n2", "n3")
		pin.Strict = false
		f.rules = append(f.rules, pin)
		return collectHAScoped(context.Background(), f, effs[i], haDirector, haNow, scopes[i])
	}
	wantNone(t, run(0), haKindAAPinRestore)
	want(t, run(1), haKindAAPinRestore, haSeverityAdvice, "bosh-na-200", "writes back the priorities")
}

// aaOneEntryReason is the words a run of one entry adds to a group that only
// another entry's AZ map may explain.
const aaOneEntryReason = "another cpis entry's placement.az_map may own the group"

// TestCollectAntiAffinityLayoutForeignNamedRuleIsAdviceForOneEntry pins that a
// run of one entry reports a group whose plan would rename a rule that its own
// AZ map never names that way as advice that names --cpi-config, because that
// rule most likely belongs to another entry's AZ map, which the run can't see.
// Under --cpi-config, where no entry's plan keeps the rule, it stays a problem.
func TestCollectAntiAffinityLayoutForeignNamedRuleIsAdviceForOneEntry(t *testing.T) {
	t.Parallel()
	g := newAALayoutGroup()
	mapped := haCfgWith("auto", "none", `,"anti_affinity":"on"`,
		`,"placement":{"anti_affinity":{"enabled":true},"az_map":{"c1":["n1"],"c2":["n2"],"c3":["n3"]}}`)
	fake := func() *fakeHA {
		f := aaLayoutFake(g, "block", 4)
		f.rules = g.rulesOf([]string{"vm:200", "vm:201"}, []string{"vm:202", "vm:203"})
		return f
	}
	r := runCollect(t, fake(), mapped)
	want(t, r, haKindAALayoutDrift, haSeverityAdvice, g.rule, "could not be planned", g.rule+"-2",
		"never gives a rule of the whole cluster that name", aaOneEntryReason, "pve-ops ha --cpi-config")
	if r.Problems() != 0 {
		t.Errorf("a run of one entry counts a rule another entry's AZ map may name as a problem: %+v", r.Findings)
	}

	sc := &haEntryScope{Name: "only", Band: pve.VMIDBand{Start: 200, End: 299}, Policy: "none", AntiAffinity: true,
		Leader: true}
	sc.Cluster = []haClusterEntry{{Name: "only", Band: sc.Band, AntiAffinity: true, Pattern: aapattern.Default(),
		AZMap: map[string][]string{"c1": {"n1"}, "c2": {"n2"}, "c3": {"n3"}}, Self: true}}
	scoped := collectHAScoped(context.Background(), fake(), mapped, haDirector, haNow, sc)
	want(t, scoped, haKindAALayoutDrift, haSeverityProblem, g.rule, "could not be planned", g.rule+"-2")
}

// TestCollectAntiAffinityLayoutRuleOfAnotherNumberingIsAdviceForOneEntry pins
// the doctor for an entry with no az_map whose cluster another entry numbers
// by three AZs. That entry names the group's second pair -4, which this
// entry's numbering would rename to -2, so a run of this entry alone reports
// the group as advice that names --cpi-config and plans no rename. A pair the
// entry numbered itself and that lost a member is still its own drift.
func TestCollectAntiAffinityLayoutRuleOfAnotherNumberingIsAdviceForOneEntry(t *testing.T) {
	t.Parallel()
	g := newAALayoutGroup()
	plain := haCfgWith("auto", "none", `,"anti_affinity":"on"`, `,"placement":{"anti_affinity":{"enabled":true}}`)
	f := aaLayoutFake(g, "block", 4)
	f.rules = g.rulesOf([]string{"vm:200", "vm:201"})
	f.rules = append(f.rules, ha.Rule{ID: g.rule + "-4", Type: "resource-affinity", Affinity: "negative",
		Comment: g.marker, Resources: []string{"vm:202", "vm:203"}})
	r := runCollect(t, f, plain)
	want(t, r, haKindAALayoutDrift, haSeverityAdvice, g.rule, "could not be planned", g.rule+"-4", "index 2",
		aaOneEntryReason, "pve-ops ha --cpi-config")
	if r.Problems() != 0 {
		t.Errorf("a run of one entry counts a rule another entry's numbering names as a problem: %+v", r.Findings)
	}

	f = aaLayoutFake(g, "block", 4)
	f.rules = g.rulesOf([]string{"vm:200", "vm:201"}, []string{"vm:202"})
	r = runCollect(t, f, plain)
	want(t, r, haKindAALayoutDrift, haSeverityAdvice, g.rule, "differ from the layout", "update "+g.rule+"-2")
}
