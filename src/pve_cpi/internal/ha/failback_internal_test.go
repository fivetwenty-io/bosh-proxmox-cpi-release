package ha

import (
	"context"
	"errors"
	"slices"
	"testing"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
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

// casRig is one HA resource whose writes PVE checks against the digest of
// resources.cfg, which changes with every write that changes the file.
type casRig struct {
	version   int
	failback  bool
	writes    []bool
	updateErr error
}

func (r *casRig) digest() string { return "digest-" + string(rune('a'+r.version)) }

func (r *casRig) GetHaResources(_ context.Context, sid string) (*sdkcluster.GetHaResourcesResponse, error) {
	c := stageMarker
	return &sdkcluster.GetHaResourcesResponse{Sid: sid, Comment: &c, Failback: pveBool(r.failback),
		Digest: r.digest()}, nil
}

func (r *casRig) UpdateHaResources(_ context.Context, _ string, p *sdkcluster.UpdateHaResourcesParams) error {
	if r.updateErr != nil {
		return r.updateErr
	}
	if p.Digest != nil && *p.Digest != r.digest() {
		return errors.New("detected modified configuration - file changed by other user")
	}
	r.writes = append(r.writes, *p.Failback)
	if r.failback != *p.Failback {
		r.failback = *p.Failback
		r.version++
	}
	return nil
}

func TestRaiseFailbackWritesOnlyAgainstTheDigestItRead(t *testing.T) {
	t.Parallel()
	rig := &casRig{}
	read := rig.digest()
	raised, err := RaiseFailback(context.Background(), rig, "vm:101", read)
	if err != nil || !raised || !rig.failback {
		t.Fatalf("raised = %v, err = %v, failback %v; a write against the current digest lands", raised, err,
			rig.failback)
	}

	stale := &casRig{}
	read = stale.digest()
	// Another process turns failback off, and back on, and off again after
	// the read, so the file changed even though the flag reads as it did.
	stale.version += 2
	raised, err = RaiseFailback(context.Background(), stale, "vm:101", read)
	if err != nil || raised || stale.failback || len(stale.writes) != 0 {
		t.Fatalf("raised = %v, err = %v, writes %v; a write against a stale digest must not land", raised, err,
			stale.writes)
	}
}

func TestRaiseFailbackNeedsADigest(t *testing.T) {
	t.Parallel()
	rig := &casRig{}
	raised, err := RaiseFailback(context.Background(), rig, "vm:101", "")
	if err != nil || raised || len(rig.writes) != 0 {
		t.Fatalf("raised = %v, err = %v, writes %v; without a digest nothing is written", raised, err, rig.writes)
	}
}

func TestRaiseFailbackReturnsOtherWriteErrors(t *testing.T) {
	t.Parallel()
	rig := &casRig{updateErr: errors.New("500 cfs lock")}
	if _, err := RaiseFailback(context.Background(), rig, "vm:101", rig.digest()); err == nil {
		t.Fatal("a failed write that is not a digest conflict must be returned")
	}
}
