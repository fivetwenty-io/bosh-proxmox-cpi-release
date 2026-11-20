package ha

import (
	"context"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// Every writer of the failback flag respects a fresh hold token. A writer may
// turn failback off while the hold is fresh, and it never turns it on, because
// the VM's pin still prefers the node a move takes the VM away from, and
// failback would pull the VM back there once the resource is under PVE HA
// again. A hold that has expired holds nothing.

var holdNow = time.Unix(1_800_000_000, 0)

func (r *stageRig) stagerAt(now time.Time) *Stager {
	s := r.stager()
	s.Now = func() time.Time { return now }
	return s
}

func TestStageKeepsFailbackOffOnAnExistingResourceUnderAFreshHold(t *testing.T) {
	t.Parallel()
	held := WithFailbackHold(WithDLBRecord(stageMarker, false), holdNow)
	for name, tc := range map[string]struct {
		have *bool
		at   time.Time
		want bool
	}{
		"a fresh hold keeps failback off":         {failbackFlag(false), holdNow.Add(time.Minute), false},
		"a fresh hold turns an on flag off":       {failbackFlag(true), holdNow.Add(time.Minute), false},
		"an unset flag reads on and goes off":     {nil, holdNow.Add(time.Minute), false},
		"an expired hold lets the decision raise": {failbackFlag(false), holdNow.Add(FailbackHoldTTL + time.Second), true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rig := &stageRig{exists: true, state: haStateIgnored, comment: held, rebalance: pveBool(false)}
			if tc.have != nil {
				rig.failback = pveBool(*tc.have)
			}
			d := stageDecision()
			d.Failback = true
			if err := rig.stagerAt(tc.at).Stage(context.Background(), d, stageMarker); err != nil {
				t.Fatal(err)
			}
			if FailbackOf(rig.failback) != tc.want {
				t.Fatalf("failback reads %v, want %v (events %v)", FailbackOf(rig.failback), tc.want, rig.events)
			}
			if tok, ok := FailbackHoldOf(&rig.comment); !ok || tok != FailbackHoldKey+"1800000000" {
				t.Fatalf("the hold token must stay: %q", rig.comment)
			}
		})
	}
}

// A re-registration stages the comment it recorded, and that comment may carry
// a hold. The create then writes failback off while the hold is fresh.
func TestStageCreatesFailbackOffWhenTheMarkerCarriesAFreshHold(t *testing.T) {
	t.Parallel()
	marker := WithFailbackHold(stageMarker, holdNow)
	for _, tc := range []struct {
		at   time.Time
		want bool
	}{
		{holdNow.Add(time.Minute), false},
		{holdNow.Add(FailbackHoldTTL + time.Second), true},
	} {
		rig := &stageRig{}
		d := stageDecision()
		d.Failback = true
		if err := rig.stagerAt(tc.at).Stage(context.Background(), d, marker); err != nil {
			t.Fatal(err)
		}
		if rig.failback == nil || rig.failback.Bool() != tc.want {
			t.Fatalf("at %v: the create wrote failback %v, want %v", tc.at, rig.failback, tc.want)
		}
		if _, ok := FailbackHoldOf(&rig.comment); !ok {
			t.Fatalf("the hold token must stay on the created resource: %q", rig.comment)
		}
	}
}

func TestReconcileFailbackNeverRaisesUnderAFreshHold(t *testing.T) {
	t.Parallel()
	rig := &stageRig{exists: true, state: "started", comment: WithFailbackHold(stageMarker, time.Now()),
		failback: pveBool(false)}
	changed, err := ReconcileFailback(context.Background(), rig, "vm:101", true, ownedMarker)
	if err != nil || changed || FailbackOf(rig.failback) {
		t.Fatalf("changed %v err %v failback %v; a fresh hold keeps failback off", changed, err, rig.failback)
	}

	on := &stageRig{exists: true, state: "started", comment: WithFailbackHold(stageMarker, time.Now()),
		failback: pveBool(true)}
	changed, err = ReconcileFailback(context.Background(), on, "vm:101", false, ownedMarker)
	if err != nil || !changed || FailbackOf(on.failback) {
		t.Fatalf("changed %v err %v failback %v; a hold never stops failback going off", changed, err, on.failback)
	}

	stale := &stageRig{exists: true, state: "started", failback: pveBool(false),
		comment: WithFailbackHold(stageMarker, time.Now().Add(-FailbackHoldTTL-time.Minute))}
	changed, err = ReconcileFailback(context.Background(), stale, "vm:101", true, ownedMarker)
	if err != nil || !changed || !FailbackOf(stale.failback) {
		t.Fatalf("changed %v err %v failback %v; an expired hold holds nothing", changed, err, stale.failback)
	}
}

// The barrier's removal fallback registers the resource again with the
// comment it read, and a fresh hold on that comment keeps failback off.
func TestNarrowRemovalFallbackKeepsFailbackOffUnderAFreshHold(t *testing.T) {
	narrowUseRemoval = true
	t.Cleanup(func() { narrowUseRemoval = false })
	for _, tc := range []struct {
		at   time.Time
		want bool
	}{
		{holdNow.Add(time.Minute), false},
		{holdNow.Add(FailbackHoldTTL + time.Minute), true},
	} {
		comment := WithFailbackHold("owner", holdNow)
		r := &barrierRig{state: "started", settleOn: []string{"a"}, snapshot: []Rule{pinAB}, now: tc.at,
			comment: &comment, states: []*pve.HAServiceState{crm("started", 100, 90, 95),
				crm("started", 100, 90, 95), {Known: true, Node: "a"}}}
		in := r.input(&pinAB, &pinA)
		in.Failback = true
		if _, err := NarrowActive(context.Background(), r.deps(), in); err != nil {
			t.Fatal(err)
		}
		if len(r.created) != 1 {
			t.Fatalf("registered %d times, want 1", len(r.created))
		}
		if got := r.created[0].Failback; got == nil || *got != tc.want {
			t.Fatalf("at %v: failback %v, want %v", tc.at, got, tc.want)
		}
	}
}
