package ha

import (
	"context"
	"errors"
	"slices"
	"testing"

	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

func TestReconcileFailbackWritesOnlyADifference(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		have *bool
		want bool
		set  bool
	}{
		"off to on":           {failbackFlag(false), true, true},
		"on to off":           {failbackFlag(true), false, true},
		"unset reads as on":   {nil, true, false},
		"unset is turned off": {nil, false, true},
		"off stays off":       {failbackFlag(false), false, false},
		"on stays on":         {failbackFlag(true), true, false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rig := &stageRig{exists: true, state: "started", comment: stageMarker}
			if c.have != nil {
				rig.failback = pveBool(*c.have)
			}
			changed, err := ReconcileFailback(context.Background(), rig, "vm:101", c.want, ownedMarker)
			if err != nil {
				t.Fatal(err)
			}
			if changed != c.set {
				t.Fatalf("changed = %v, want %v (events %v)", changed, c.set, rig.events)
			}
			want := []string{"get"}
			if c.set {
				want = append(want, "update:-")
			}
			if !slices.Equal(rig.events, want) {
				t.Fatalf("events %v, want %v", rig.events, want)
			}
			if FailbackOf(rig.failback) != c.want {
				t.Fatalf("failback reads %v, want %v", FailbackOf(rig.failback), c.want)
			}
		})
	}
}

func TestReconcileFailbackLeavesResourcesItMustNotTouch(t *testing.T) {
	t.Parallel()
	cases := map[string]*stageRig{
		"a missing resource": {},
		"another Director's": {exists: true, state: "started", comment: "bosh-cpi:v1 owner=them"},
		"an operator's":      {exists: true, state: "started", comment: "kept by hand"},
	}
	for name, rig := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rig.failback = pveBool(false)
			changed, err := ReconcileFailback(context.Background(), rig, "vm:101", true, ownedMarker)
			if err != nil || changed {
				t.Fatalf("changed = %v, err = %v; the resource must be left alone", changed, err)
			}
			if !slices.Equal(rig.events, []string{"get"}) {
				t.Fatalf("only a read is allowed: %v", rig.events)
			}
		})
	}
}

func TestReconcileFailbackWritesAResourceTheBarrierHolds(t *testing.T) {
	t.Parallel()
	rig := &stageRig{exists: true, state: haStateIgnored, comment: withNarrowMark(strPtr(stageMarker), "started"),
		failback: pveBool(true)}
	changed, err := ReconcileFailback(context.Background(), rig, "vm:101", false, ownedMarker)
	if err != nil || !changed || FailbackOf(rig.failback) {
		t.Fatalf("changed = %v, err = %v, failback %v; the barrier leaves failback alone, so it is written",
			changed, err, rig.failback)
	}
	if rig.state != haStateIgnored || !NarrowPending(&rig.state, &rig.comment) {
		t.Fatalf("the write must leave the barrier's state and token: %q %q", rig.state, rig.comment)
	}
}

func TestReconcileFailbackReturnsReadAndWriteErrors(t *testing.T) {
	t.Parallel()
	read := &stageRig{getErr: errors.New("500 proxy loop")}
	if _, err := ReconcileFailback(context.Background(), read, "vm:101", false, ownedMarker); err == nil {
		t.Fatal("a failed read must be returned")
	}
	gone := &stageRig{getErr: &sdkerrors.APIError{HTTPCode: 404, Message: "no such resource"}}
	if changed, err := ReconcileFailback(context.Background(), gone, "vm:101", false, ownedMarker); err != nil ||
		changed {
		t.Fatalf("a missing resource is not an error: %v, %v", changed, err)
	}
	write := &stageRig{exists: true, state: "started", comment: stageMarker, failback: pveBool(true),
		updateErr: errors.New("500 cfs lock")}
	if _, err := ReconcileFailback(context.Background(), write, "vm:101", false, ownedMarker); err == nil {
		t.Fatal("a failed write must be returned")
	}
}

func TestExpectedFailback(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		f     FailbackFacts
		want  bool
		known bool
	}{
		"an all-shared VM":                    {FailbackFacts{Enabled: true}, true, true},
		"the key is off":                      {FailbackFacts{}, false, true},
		"a replicate VM":                      {FailbackFacts{Enabled: true, Replicate: true}, false, true},
		"a VM with a local volume":            {FailbackFacts{Enabled: true, Local: true}, false, true},
		"a replicate VM with unknown storage": {FailbackFacts{Enabled: true, Replicate: true, Unknown: true}, false, true},
		"an unknown storage":                  {FailbackFacts{Enabled: true, Unknown: true}, false, false},
		"a strict self pin":                   {FailbackFacts{Enabled: true, SelfPinned: true}, false, false},
	}
	for name, c := range cases {
		want, known := ExpectedFailback(c.f)
		if want != c.want || known != c.known {
			t.Errorf("%s: ExpectedFailback = %v, %v; want %v, %v", name, want, known, c.want, c.known)
		}
	}
}

func failbackFlag(b bool) *bool { return &b }
