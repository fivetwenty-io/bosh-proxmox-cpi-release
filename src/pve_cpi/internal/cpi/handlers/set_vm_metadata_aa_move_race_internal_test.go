package handlers

import (
	"context"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/aapattern"
)

// coloWantNoRuleOnOneNode fails the test when a rule of the stub pairs two
// members that sit on one node now. PVE HA moves both members of such a
// pair, which the move exists to avoid.
func (g *coloGroup) coloWantNoRuleOnOneNode(t *testing.T, when string) {
	t.Helper()
	g.mu.Lock()
	defer g.mu.Unlock()
	for name, csv := range g.stub.rules {
		seen := map[string]string{}
		for sid := range strings.SplitSeq(csv, ",") {
			vmid, ok := haSidVMID(sid)
			if !ok {
				continue
			}
			node := g.nodes[vmid]
			if other, dup := seen[node]; dup {
				t.Errorf("%s: rule %s = %q pairs %s and %s on %s", when, name, csv, other, sid, node)
			}
			seen[node] = sid
		}
	}
}

// TestColoMove_ASiblingLeavesAMovingMemberOutOfItsWrite models a parallel
// deploy in which the call for index 0 moves its VM off index 1's node, and
// the call for index 2 reaches its rule write while that move is still in
// flight. The sibling must not write the rule that pairs indexes 0 and 1,
// because both still sit on pve1 and PVE HA would move both of them. It
// writes the rest of the layout, and the mover's own join writes the pair's
// rule after the move, so each rule is written once and the pair costs one
// migration.
func TestColoMove_ASiblingLeavesAMovingMemberOutOfItsWrite(t *testing.T) {
	t.Parallel()
	g := newColoGroup("pve1", "pve1", "pve2", "pve3")
	dir := coloClaimDir(t)
	hA, hC := g.harness(), g.harness()
	depsC := g.deps(hC, dir)
	writes := map[string]int{}
	g.stub.onRuleWrite = func(rule string) {
		writes[rule]++
		g.coloWantNoRuleOnOneNode(t, "after the write of "+rule)
	}
	inner := hA.migrate
	siblingRan := false
	hA.migrate = func(ctx context.Context, vmid int, from, to string) error {
		if vmid == 100 && !siblingRan {
			siblingRan = true
			if live := coloLiveClaims(t, dir); len(live) != 1 || live[0].VMID != 100 {
				t.Errorf("live claims while 100 moves = %+v; want the mover's claim on 100", live)
			}
			g.join(depsC, 102)
			if got := g.stub.rules[aaRule]; got != "" {
				t.Errorf("rule %s = %q after the sibling's write; want it left to the mover", aaRule, got)
			}
		}
		return inner(ctx, vmid, from, to)
	}
	g.tag(101, 103)
	g.join(g.deps(hA, dir), 100)
	if !siblingRan {
		t.Fatal("the sibling never joined")
	}
	want := map[string]string{
		aaRule:                         "vm:100,vm:101",
		aapattern.ChunkName(aaRule, 2): "vm:102,vm:103",
	}
	if !maps.Equal(g.stub.rules, want) {
		t.Fatalf("rules = %v; want %v", g.stub.rules, want)
	}
	for name, n := range writes {
		if n != 1 {
			t.Errorf("rule %s was written %d times; want once", name, n)
		}
	}
	if got := slices.Concat(hA.moves(), hC.moves()); len(got) != 1 || got[0].vmid != 100 {
		t.Fatalf("migrations = %+v; want only the move of 100", got)
	}
}

// TestColoMove_ALandedMoveNoLongerHoldsTheMember pins that a live move claim
// keeps its member out of a new rule only while the member sits on another
// node than the claim names. Once the move has landed, a sibling writes the
// pair as usual, because the pair no longer shares a node.
func TestColoMove_ALandedMoveNoLongerHoldsTheMember(t *testing.T) {
	t.Parallel()
	g := newColoGroup("pve1", "pve1", "pve2", "pve3")
	dir := coloClaimDir(t)
	hA, hC := g.harness(), g.harness()
	depsC := g.deps(hC, dir)
	inner := hA.migrate
	hA.migrate = func(ctx context.Context, vmid int, from, to string) error {
		err := inner(ctx, vmid, from, to)
		g.join(depsC, 102)
		if got := g.stub.rules[aaRule]; got != "vm:100,vm:101" {
			t.Errorf("rule %s = %q after the sibling's write; want the pair written once 100 has landed",
				aaRule, got)
		}
		return err
	}
	g.tag(101, 103)
	g.join(g.deps(hA, dir), 100)
	g.coloWantNoRuleOnOneNode(t, "after the deploy")
}
