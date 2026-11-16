package handlers

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// The Director creates an instance group's VMs in parallel. Each create tags
// its guest at clone time and stages its HA resource before it takes the rules
// lock and writes the group's rule, and PVE refuses a rule that names a
// resource it does not manage yet. These tests drive those steps in every
// order they can run in, against a stub that enforces the refusal.

// aaStep is one step of one sibling's create: staging its HA resource, or
// writing the group's rule under the rules lock.
type aaStep struct {
	vmid   int
	ensure bool
}

// aaInterleavings returns every order of the steps of creates that each stage
// their resource before they write the rule.
func aaInterleavings(vmids []int) [][]aaStep {
	var out [][]aaStep
	staged := map[int]bool{}
	done := map[int]bool{}
	var walk func(prefix []aaStep)
	walk = func(prefix []aaStep) {
		if len(prefix) == 2*len(vmids) {
			out = append(out, slices.Clone(prefix))
			return
		}
		for _, v := range vmids {
			switch {
			case !staged[v]:
				staged[v] = true
				walk(append(prefix, aaStep{vmid: v}))
				staged[v] = false
			case !done[v]:
				done[v] = true
				walk(append(prefix, aaStep{vmid: v, ensure: true}))
				done[v] = false
			}
		}
	}
	walk(nil)
	return out
}

func describeSteps(steps []aaStep) string {
	parts := make([]string, 0, len(steps))
	for _, st := range steps {
		if st.ensure {
			parts = append(parts, fmt.Sprintf("write(%d)", st.vmid))
		} else {
			parts = append(parts, fmt.Sprintf("stage(%d)", st.vmid))
		}
	}
	return strings.Join(parts, " ")
}

// watchAARule fails the test when a write leaves the group's rule without the
// marker or without a member an earlier write gave it, or deletes it. Only
// creates run here, and no create deletes a rule that has two members.
func watchAARule(t *testing.T, stub *aaClusterStub, order string) {
	t.Helper()
	var prev []string
	stub.onRuleWrite = func(rule string) {
		if rule != aaRule {
			return
		}
		if _, ok := stub.rules[aaRule]; !ok {
			t.Errorf("%s: the rule was deleted (it held %v)", order, prev)
			prev = nil
			return
		}
		if got := stub.ruleComments[aaRule]; got != aaIdent("web").marker() {
			t.Errorf("%s: rule written with comment %q; want the marker", order, got)
		}
		cur := strings.Split(stub.rules[aaRule], ",")
		for _, m := range prev {
			if !slices.Contains(cur, m) {
				t.Errorf("%s: write dropped %s from the rule (%v -> %v)", order, m, prev, cur)
			}
		}
		prev = cur
	}
}

func TestEnsureAntiAffinity_ParallelSiblingCreatesConvergeInEveryOrder(t *testing.T) {
	t.Parallel()
	vmids := []int{101, 102, 103}
	orders := aaInterleavings(vmids)
	if len(orders) != 90 {
		t.Fatalf("interleavings = %d; want 90", len(orders))
	}
	for _, steps := range orders {
		order := describeSteps(steps)
		stub := newAAStub()
		stub.enforceManaged = true
		// Every clone has tagged its guest before any create writes the rule.
		stub.listResourcesFn = aaResourcesFn(aaQEMU(101, aaTag), aaQEMU(102, aaTag), aaQEMU(103, aaTag))
		watchAARule(t, stub, order)
		deps := aaDeps(stub)
		deps.HAProfile = pve9ClusterProfile("pve1", "pve2", "pve3")
		waits := 0
		deps.HARuleSleep = func(context.Context, time.Duration) error {
			waits++
			return nil
		}
		for _, st := range steps {
			if !st.ensure {
				stub.resources[haResourceSid(st.vmid)] = true
				continue
			}
			if err := ensureAntiAffinityMembership(context.Background(), deps, aaIdent("web"), st.vmid,
				log.NewNopLogger()); err != nil {
				t.Fatalf("%s: write(%d) failed, which rolls back a built VM: %v", order, st.vmid, err)
			}
		}
		if got := stub.rules[aaRule]; got != "vm:101,vm:102,vm:103" {
			t.Errorf("%s: rule = %q; want every sibling", order, got)
		}
		if waits != 0 {
			t.Errorf("%s: %d held waits; a rule that names only managed members is never refused", order, waits)
		}
	}
}

func TestEnsureAntiAffinity_SiblingMidCreateJoinsThroughItsOwnCreate(t *testing.T) {
	t.Parallel()
	stub := newAAStub()
	stub.enforceManaged = true
	stub.seedRule(aaRule, "vm:100,vm:101")
	stub.resources["vm:100"], stub.resources["vm:101"], stub.resources["vm:102"] = true, true, true
	// vm:103 has cloned and tagged its guest but has not staged its resource.
	stub.listResourcesFn = aaResourcesFn(aaQEMU(100, aaTag), aaQEMU(101, aaTag), aaQEMU(102, aaTag),
		aaQEMU(103, aaTag))
	deps := aaDeps(stub)
	deps.HAProfile = pve9ClusterProfile("pve1", "pve2", "pve3", "pve4")

	if err := ensureAntiAffinityMembership(context.Background(), deps, aaIdent("web"), 102,
		log.NewNopLogger()); err != nil {
		t.Fatalf("vm:102 must join without waiting for vm:103: %v", err)
	}
	if got := stub.rules[aaRule]; got != "vm:100,vm:101,vm:102" {
		t.Fatalf("rule = %q; want vm:103 left for its own create", got)
	}
	stub.resources["vm:103"] = true
	if err := ensureAntiAffinityMembership(context.Background(), deps, aaIdent("web"), 103,
		log.NewNopLogger()); err != nil {
		t.Fatal(err)
	}
	if got := stub.rules[aaRule]; got != "vm:100,vm:101,vm:102,vm:103" {
		t.Errorf("rule = %q; want vm:103 added by its own create", got)
	}
}

// TestEnsureAntiAffinity_SiblingRollbackBetweenPlanAndWriteWaitsAndReplans
// covers a sibling whose create rolls its resource back after this create
// planned the rule with it. PVE refuses the write, and the writer waits before
// it plans again against a fresh read of the resources.
func TestEnsureAntiAffinity_SiblingRollbackBetweenPlanAndWriteWaitsAndReplans(t *testing.T) {
	t.Parallel()
	stub := newAAStub()
	stub.enforceManaged = true
	stub.tagged(aaQEMU(101, aaTag), aaQEMU(102, aaTag), aaQEMU(103, aaTag))
	stub.afterListHaResources = func(call int) {
		if call == 1 {
			delete(stub.resources, "vm:103")
		}
	}
	watchAARule(t, stub, "rollback")
	deps := aaDeps(stub)
	deps.HAProfile = pve9ClusterProfile("pve1", "pve2", "pve3")
	var waits []time.Duration
	deps.HARuleSleep = func(_ context.Context, d time.Duration) error {
		waits = append(waits, d)
		return nil
	}
	if err := ensureAntiAffinityMembership(context.Background(), deps, aaIdent("web"), 102,
		log.NewNopLogger()); err != nil {
		t.Fatalf("a sibling's rollback must not fail this create: %v", err)
	}
	if got := stub.rules[aaRule]; got != "vm:101,vm:102" {
		t.Errorf("rule = %q; want the members that are still managed", got)
	}
	if len(waits) != 1 || waits[0] <= 0 {
		t.Errorf("waits = %v; want one real wait before the second plan", waits)
	}
	if stub.createRuleCalls != 2 {
		t.Errorf("create calls = %d; want the refused one and the one that landed", stub.createRuleCalls)
	}
}

// TestEnsureAntiAffinity_KeepsAMemberThatIsNotManaged pins that the filter
// only decides who joins. A member the rule already holds stays in it, even
// when the resource read does not list it.
func TestEnsureAntiAffinity_KeepsAMemberThatIsNotManaged(t *testing.T) {
	t.Parallel()
	stub := newAAStub()
	stub.seedRule(aaRule, "vm:100,vm:101")
	stub.listResourcesFn = aaResourcesFn(aaQEMU(100, aaTag), aaQEMU(101, aaTag), aaQEMU(102, aaTag))
	stub.resources["vm:101"], stub.resources["vm:102"] = true, true
	if err := ensureAntiAffinityMembership(context.Background(), aaDeps(stub), aaIdent("web"), 102,
		log.NewNopLogger()); err != nil {
		t.Fatal(err)
	}
	if got := stub.rules[aaRule]; got != "vm:100,vm:101,vm:102" {
		t.Errorf("rule = %q; want vm:100 kept", got)
	}
}

func TestEnsureAntiAffinity_FailedResourceReadWritesNothing(t *testing.T) {
	t.Parallel()
	stub := newAAStub()
	stub.tagged(aaQEMU(100, aaTag), aaQEMU(101, aaTag))
	stub.listHaResErr = errors.New("ha resources unavailable")
	err := ensureAntiAffinityMembership(context.Background(), aaDeps(stub), aaIdent("web"), 101, log.NewNopLogger())
	if err == nil || !strings.Contains(err.Error(), "list HA resources") {
		t.Fatalf("err = %v; want the failed resource read", err)
	}
	if stub.createRuleCalls != 0 || stub.updateRuleCalls != 0 || len(stub.rules) != 0 {
		t.Errorf("nothing may be written without the resource read: rules %v", stub.rules)
	}
}
