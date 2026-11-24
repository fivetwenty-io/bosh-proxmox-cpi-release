package handlers

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/spreadclaim"
)

// TestAAOff_RestoresThePinOfAMovingMemberAtAHeldIndex pins that the plan of
// an opted-out group keeps every member in its layout input, so the pin
// restore reaches a member that sits in no rule at the index of a hold from
// before the switch while another call moves it. Under a block pattern that
// member waits for the hold, and under off no hold is kept, so its flattened
// pin gets its priorities back at this plan.
func TestAAOff_RestoresThePinOfAMovingMemberAtAHeldIndex(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	stub := newAAStub()
	stub.tagged(aaQEMUOn(100, "pve1", aaTag+";index--0"), aaQEMUOn(101, "pve2", aaTag+";index--1"),
		aaQEMUOn(102, "pve1", aaTag+";index--2"))
	markWebPattern(stub, "block", 100, 101)
	markWebPattern(stub, "off", 102)
	until := strconv.FormatInt(time.Now().Add(10*time.Minute).Unix(), 10)
	stub.seedRuleWithComment(aaRule, "vm:100,vm:101", aaRuleMarker()+" hold=2.99@"+until)
	pin := naRuleNameFor(102)
	stub.seedRuleWithComment(pin, "vm:102", haIdentity{Owner: aaDirector}.marker()+" "+ha.PinRestoreKey+"="+
		restoreLabPin)
	stub.ruleTypes[pin] = ha.RuleTypeNodeAffinity
	stub.ruleNodes[pin] = "pve1,pve2,pve3"
	deps := aaMetadataDeps(stub)
	profile, err := deps.HAProfile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	scan, err := collectGroupMemberSids(ctx, deps, aaIdent("web"))
	if err != nil {
		t.Fatal(err)
	}
	set, err := readHARuleSet(ctx, deps.PVE.Cluster())
	if err != nil {
		t.Fatal(err)
	}
	planner := newGroupPlanner(ctx, deps, profile, aaIdent("web"), scan, log.NewNopLogger())
	planner.sid, planner.index, planner.checkSelf = "vm:100", 0, true
	planner.liveMoves = func(string) ([]spreadclaim.Live, error) {
		return []spreadclaim.Live{{Node: "pve3", VMID: 102}}, nil
	}
	if _, err := planner.plan(set); err != nil {
		t.Fatalf("plan: %v", err)
	}
	if !planner.moving["vm:102"] {
		t.Fatalf("moving = %v; want vm:102 moving", planner.moving)
	}
	for _, r := range planner.pinRestores(set) {
		if r.SID == "vm:102" {
			return
		}
	}
	t.Fatalf("pin restores = %v; want one for vm:102", planner.pinRestores(set))
}
