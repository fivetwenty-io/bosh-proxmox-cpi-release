package main

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"testing"

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
// hold the plan gets no layout finding.
func TestCollectAntiAffinityLayoutInPlanReadsClean(t *testing.T) {
	t.Parallel()
	g := newAALayoutGroup()
	f := aaLayoutFake(g, "", 2)
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
