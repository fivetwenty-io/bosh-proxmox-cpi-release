package ha

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

// The rebalance token holds failback off for as long as it is on the
// resource, the same way a fresh failback hold does, and every writer of the
// comment keeps it.

const rebalanceMark = RebalanceMarkKey + "b"

func TestRebalanceMarkTokens(t *testing.T) {
	t.Parallel()
	base := stageMarker + " bosh-dlb=0"
	marked := WithRebalanceMark(base, "b")
	if marked != base+" "+rebalanceMark {
		t.Fatalf("marked %q", marked)
	}
	if again := WithRebalanceMark(marked+" "+FailbackHoldKey+"5", "c"); again != base+" "+FailbackHoldKey+"5 "+
		RebalanceMarkKey+"c" {
		t.Fatalf("a second mark must replace the first: %q", again)
	}
	if node, ok := RebalancePending(&marked); !ok || node != "b" {
		t.Fatalf("pending %q %v", node, ok)
	}
	stripped, had := WithoutRebalanceMark(marked)
	if !had || stripped != base {
		t.Fatalf("stripped %q had %v", stripped, had)
	}
	if again, had := WithoutRebalanceMark(stripped); had || again != base {
		t.Fatalf("a comment without the mark changed: %q had %v", again, had)
	}
	empty := base + " " + RebalanceMarkKey
	for _, c := range []*string{nil, &base, &empty} {
		if node, ok := RebalancePending(c); ok {
			t.Fatalf("no mark reads as one naming %q", node)
		}
	}
	if got := WithRebalanceMark("", "b"); got != rebalanceMark {
		t.Fatalf("an empty comment marked %q", got)
	}
}

func TestFailbackHeldByARebalanceMarkWithoutExpiry(t *testing.T) {
	t.Parallel()
	comment := stageMarker + " " + rebalanceMark
	for _, now := range []time.Time{holdNow, holdNow.Add(24 * time.Hour), time.Unix(0, 0)} {
		if !FailbackHeld(&comment, now) || FailbackAllowed(true, comment, now) {
			t.Fatalf("the rebalance mark must hold failback off at %v", now)
		}
	}
	plain := stageMarker
	if FailbackHeld(&plain, holdNow) || !FailbackAllowed(true, plain, holdNow) {
		t.Fatal("a comment without a mark or a hold holds nothing")
	}
}

func TestReconcileFailbackChangesNothingUnderARebalanceMark(t *testing.T) {
	t.Parallel()
	for _, want := range []bool{true, false} {
		rig := &stageRig{exists: true, state: "started", comment: stageMarker + " " + rebalanceMark,
			failback: pveBool(false)}
		changed, err := ReconcileFailback(context.Background(), rig, "vm:101", want, ownedMarker)
		if err != nil || changed || !slices.Equal(rig.events, []string{"get"}) || rig.failback.Bool() {
			t.Fatalf("want %v: changed %v err %v events %v", want, changed, err, rig.events)
		}
	}
}

func TestStageKeepsFailbackOffAndTheMarkUnderARebalanceMark(t *testing.T) {
	t.Parallel()
	comment := WithDLBRecord(stageMarker, false) + " " + rebalanceMark
	rig := &stageRig{exists: true, state: haStateIgnored, comment: comment, failback: pveBool(true),
		rebalance: pveBool(false)}
	d := stageDecision()
	d.Failback = true
	if err := rig.stagerAt(holdNow).Stage(context.Background(), d, stageMarker); err != nil {
		t.Fatal(err)
	}
	if rig.failback.Bool() || rig.comment != comment {
		t.Fatalf("failback %v comment %q, want off with the mark kept", rig.failback.Bool(), rig.comment)
	}
}

func TestFailbackHoldWritersKeepTheRebalanceMark(t *testing.T) {
	t.Parallel()
	rig := &stageRig{exists: true, state: "started", comment: stageMarker + " " + rebalanceMark,
		failback: pveBool(false)}
	tok, err := HoldFailback(context.Background(), rig, "vm:101", holdNow, ownedMarker)
	if err != nil || rig.comment != stageMarker+" "+rebalanceMark+" "+tok {
		t.Fatalf("hold err %v comment %q", err, rig.comment)
	}
	released, err := ReleaseFailbackHold(context.Background(), rig, "vm:101", tok, ownedMarker)
	if err != nil || !released || rig.comment != stageMarker+" "+rebalanceMark {
		t.Fatalf("release %v err %v comment %q", released, err, rig.comment)
	}
}

func TestNarrowKeepsTheRebalanceMark(t *testing.T) {
	r := &barrierRig{state: "started", settleOn: []string{"a"}, states: inOrderStates(), digests: true}
	r.addToken(rebalanceMark)
	if _, err := NarrowActive(context.Background(), r.deps(), r.input(&pinAB, &pinA)); err != nil {
		t.Fatal(err)
	}
	if r.state != "started" || r.currentComment() != "owner "+rebalanceMark {
		t.Fatalf("state %q comment %q, want started with the mark", r.state, r.currentComment())
	}
}

func TestMarkRebalanceTurnsFailbackOffAndAddsTheMarkInOneWrite(t *testing.T) {
	t.Parallel()
	rig := &stageRig{exists: true, state: "started", comment: stageMarker + " bosh-dlb=0 bosh-future=1",
		failback: pveBool(true)}
	read, _ := rig.GetHaResources(context.Background(), "vm:101")
	ok, err := MarkRebalance(context.Background(), rig, "vm:101", read, "b")
	if err != nil || !ok {
		t.Fatalf("ok %v err %v", ok, err)
	}
	if rig.failback.Bool() || rig.comment != stageMarker+" bosh-dlb=0 bosh-future=1 "+rebalanceMark {
		t.Fatalf("failback %v comment %q", rig.failback.Bool(), rig.comment)
	}

	// Another writer lands between the read and the write, so PVE refuses
	// it, and nothing changes.
	stale := &stageRig{exists: true, state: "started", comment: stageMarker, failback: pveBool(true)}
	read, _ = stale.GetHaResources(context.Background(), "vm:101")
	stale.beforeUpdate = func(int) { stale.version++ }
	ok, err = MarkRebalance(context.Background(), stale, "vm:101", read, "b")
	if err != nil || ok || stale.comment != stageMarker || !stale.failback.Bool() {
		t.Fatalf("a refused write: ok %v err %v comment %q", ok, err, stale.comment)
	}

	// A read that names no digest writes nothing.
	blind := &stageRig{exists: true, state: "started", comment: stageMarker, noDigest: true}
	read, _ = blind.GetHaResources(context.Background(), "vm:101")
	if ok, err := MarkRebalance(context.Background(), blind, "vm:101", read, "b"); err != nil || ok ||
		blind.updates != 0 {
		t.Fatalf("no digest: ok %v err %v updates %d", ok, err, blind.updates)
	}

	failing := &stageRig{exists: true, state: "started", comment: stageMarker, updateErr: errors.New("500")}
	read, _ = failing.GetHaResources(context.Background(), "vm:101")
	if _, err := MarkRebalance(context.Background(), failing, "vm:101", read, "b"); err == nil {
		t.Fatal("a write that fails returned no error")
	}
}

func TestUnmarkRebalanceSetsFailbackAndDropsTheMarkInOneWrite(t *testing.T) {
	t.Parallel()
	for _, want := range []bool{true, false} {
		rig := &stageRig{exists: true, state: "started", failback: pveBool(false),
			comment: stageMarker + " " + rebalanceMark + " " + FailbackHoldKey + "9"}
		read, _ := rig.GetHaResources(context.Background(), "vm:101")
		ok, err := UnmarkRebalance(context.Background(), rig, "vm:101", read, want, time.Now())
		if err != nil || !ok || rig.failback.Bool() != want || rig.comment != stageMarker+" "+FailbackHoldKey+"9" {
			t.Fatalf("want %v: ok %v err %v failback %v comment %q", want, ok, err, rig.failback.Bool(),
				rig.comment)
		}
		if rig.updates != 1 {
			t.Fatalf("updates %d, want one write", rig.updates)
		}
	}
	stale := &stageRig{exists: true, state: "started", failback: pveBool(false),
		comment: stageMarker + " " + rebalanceMark}
	read, _ := stale.GetHaResources(context.Background(), "vm:101")
	stale.beforeUpdate = func(int) { stale.version++ }
	if ok, err := UnmarkRebalance(context.Background(), stale, "vm:101", read, true, time.Now()); err != nil || ok ||
		stale.failback.Bool() || stale.comment != stageMarker+" "+rebalanceMark {
		t.Fatalf("a refused write must leave the mark and failback off: ok %v err %v", ok, err)
	}
}

func TestUnmarkRebalanceKeepsFailbackOffUnderAFreshHold(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_800_000_000, 0)
	rig := &stageRig{exists: true, state: "started", failback: pveBool(false),
		comment: WithFailbackHold(stageMarker+" "+rebalanceMark, now.Add(-time.Minute))}
	read, _ := rig.GetHaResources(context.Background(), "vm:101")
	ok, err := UnmarkRebalance(context.Background(), rig, "vm:101", read, true, now)
	if err != nil || !ok || rig.failback.Bool() {
		t.Fatalf("ok %v err %v failback %v", ok, err, rig.failback.Bool())
	}
	if _, held := FailbackHoldOf(&rig.comment); !held {
		t.Fatalf("the hold is gone: %q", rig.comment)
	}
}
