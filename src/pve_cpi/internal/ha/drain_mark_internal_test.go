package ha

import (
	"context"
	"errors"
	"testing"
)

// The drain token records on a VM's HA resource that pve-ops ha drain stopped
// the VM, and every writer of the comment keeps it.

const drainMark = DrainMarkKey + "pve1"

func TestDrainMarkTokens(t *testing.T) {
	t.Parallel()
	base := stageMarker + " bosh-dlb=0"
	marked := WithDrainMark(base, "pve1")
	if marked != base+" "+drainMark {
		t.Fatalf("marked %q", marked)
	}
	if again := WithDrainMark(marked+" "+rebalanceMark, "pve2"); again != base+" "+rebalanceMark+" "+
		DrainMarkKey+"pve2" {
		t.Fatalf("a second mark must replace the first: %q", again)
	}
	if node, ok := DrainStopped(&marked); !ok || node != "pve1" {
		t.Fatalf("stopped %q %v", node, ok)
	}
	stripped, had := WithoutDrainMark(marked)
	if !had || stripped != base {
		t.Fatalf("stripped %q had %v", stripped, had)
	}
	if again, had := WithoutDrainMark(stripped); had || again != base {
		t.Fatalf("a comment without the mark changed: %q had %v", again, had)
	}
	empty := base + " " + DrainMarkKey
	for _, c := range []*string{nil, &base, &empty} {
		if node, ok := DrainStopped(c); ok {
			t.Fatalf("no mark reads as one naming %q", node)
		}
	}
	if got := WithDrainMark("", "pve1"); got != drainMark {
		t.Fatalf("an empty comment marked %q", got)
	}
}

func TestStageKeepsTheDrainMark(t *testing.T) {
	t.Parallel()
	comment := WithDLBRecord(stageMarker, false) + " " + drainMark
	rig := &stageRig{exists: true, state: haStateIgnored, comment: comment, failback: pveBool(false),
		rebalance: pveBool(false)}
	if err := rig.stagerAt(holdNow).Stage(context.Background(), stageDecision(), stageMarker); err != nil {
		t.Fatal(err)
	}
	if rig.comment != comment {
		t.Fatalf("comment %q, want the drain mark kept", rig.comment)
	}
}

func TestStopForDrainStopsAndMarksInOneWrite(t *testing.T) {
	t.Parallel()
	rig := &stageRig{exists: true, state: "started", comment: stageMarker + " bosh-dlb=0 bosh-future=1",
		failback: pveBool(true)}
	read, _ := rig.GetHaResources(context.Background(), "vm:101")
	ok, err := StopForDrain(context.Background(), rig, "vm:101", read, "pve1")
	if err != nil || !ok || rig.updates != 1 {
		t.Fatalf("ok %v err %v updates %d", ok, err, rig.updates)
	}
	if rig.state != "stopped" || rig.comment != stageMarker+" bosh-dlb=0 bosh-future=1 "+drainMark ||
		!rig.failback.Bool() {
		t.Fatalf("state %q comment %q failback %v", rig.state, rig.comment, rig.failback.Bool())
	}

	// Another writer lands between the read and the write, so PVE refuses
	// it, and nothing changes.
	stale := &stageRig{exists: true, state: "started", comment: stageMarker}
	read, _ = stale.GetHaResources(context.Background(), "vm:101")
	stale.beforeUpdate = func(int) { stale.version++ }
	if ok, err := StopForDrain(context.Background(), stale, "vm:101", read, "pve1"); err != nil || ok ||
		stale.state != "started" || stale.comment != stageMarker {
		t.Fatalf("a refused write: ok %v err %v state %q comment %q", ok, err, stale.state, stale.comment)
	}

	// A read that names no digest writes nothing.
	blind := &stageRig{exists: true, state: "started", comment: stageMarker, noDigest: true}
	read, _ = blind.GetHaResources(context.Background(), "vm:101")
	if ok, err := StopForDrain(context.Background(), blind, "vm:101", read, "pve1"); err != nil || ok ||
		blind.updates != 0 {
		t.Fatalf("no digest: ok %v err %v updates %d", ok, err, blind.updates)
	}

	failing := &stageRig{exists: true, state: "started", comment: stageMarker, updateErr: errors.New("500")}
	read, _ = failing.GetHaResources(context.Background(), "vm:101")
	if _, err := StopForDrain(context.Background(), failing, "vm:101", read, "pve1"); err == nil {
		t.Fatal("a write that fails returned no error")
	}
	if _, err := StopForDrain(context.Background(), rig, "vm:101", read, ""); err == nil {
		t.Fatal("a stop that names no node must be refused")
	}
}

func TestStartAfterDrainStartsAndDropsTheMarkInOneWrite(t *testing.T) {
	t.Parallel()
	rig := &stageRig{exists: true, state: "stopped",
		comment: stageMarker + " " + drainMark + " " + FailbackHoldKey + "9"}
	read, _ := rig.GetHaResources(context.Background(), "vm:101")
	ok, err := StartAfterDrain(context.Background(), rig, "vm:101", read)
	if err != nil || !ok || rig.updates != 1 || rig.state != "started" ||
		rig.comment != stageMarker+" "+FailbackHoldKey+"9" {
		t.Fatalf("ok %v err %v updates %d state %q comment %q", ok, err, rig.updates, rig.state, rig.comment)
	}
	stale := &stageRig{exists: true, state: "stopped", comment: stageMarker + " " + drainMark}
	read, _ = stale.GetHaResources(context.Background(), "vm:101")
	stale.beforeUpdate = func(int) { stale.version++ }
	if ok, err := StartAfterDrain(context.Background(), stale, "vm:101", read); err != nil || ok ||
		stale.state != "stopped" || stale.comment != stageMarker+" "+drainMark {
		t.Fatalf("a refused write must leave the VM stopped with its mark: ok %v err %v", ok, err)
	}
}
