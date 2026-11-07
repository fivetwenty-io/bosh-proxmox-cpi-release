package ha

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// ownedByRig is the ownership check of the barrier rig, whose resource comment
// is "owner" unless a test sets another.
func ownedByRig(comment string) bool { return comment == "owner" }

// leaveRig is a barrier rig whose resource reads and removals can fail.
type leaveRig struct {
	*barrierRig
	getErr error
	delErr error
}

func (r *leaveRig) GetHaResources(ctx context.Context, sid string) (*sdkcluster.GetHaResourcesResponse, error) {
	if r.getErr != nil {
		return nil, r.getErr
	}
	return r.barrierRig.GetHaResources(ctx, sid)
}

func (r *leaveRig) DeleteHaResources(ctx context.Context, sid string, p *sdkcluster.DeleteHaResourcesParams) error {
	if r.delErr != nil {
		r.events = append(r.events, "remove-failed")
		return r.delErr
	}
	if p == nil || p.Purge == nil || !*p.Purge {
		r.events = append(r.events, "remove-without-purge")
	}
	return r.barrierRig.DeleteHaResources(ctx, sid, p)
}

func (r *leaveRig) deps() NarrowDeps {
	d := r.barrierRig.deps()
	d.Resources = r
	return d
}

// droppedAfterRemoval is the CRM view of a VM whose resource was removed: a
// first read, then a round newer than it with an LRM report newer still.
func droppedAfterRemoval() []*pve.HAServiceState {
	return []*pve.HAServiceState{dropped("a", 100, 90, 95), dropped("a", 110, 120, 125)}
}

func TestLeaveHARemovesAnOwnedResourceAndDrains(t *testing.T) {
	r := &leaveRig{barrierRig: &barrierRig{state: "started", settleOn: []string{"a"}, states: droppedAfterRemoval()}}
	loc, err := LeaveHA(context.Background(), r.deps(), 101, "a", time.Minute, ownedByRig)
	if err != nil {
		t.Fatal(err)
	}
	if loc.Node != "a" || !loc.Found {
		t.Fatalf("location %+v, want node a", loc)
	}
	if !slices.Equal(r.events, []string{"remove"}) {
		t.Fatalf("events %v, want one purging removal and no state request", r.events)
	}
}

func TestLeaveHACountsAMissingResourceAsRemoved(t *testing.T) {
	// A crash after an earlier removal leaves no resource; the drain still runs.
	r := &leaveRig{barrierRig: &barrierRig{missing: true, settleOn: []string{"a"}, states: droppedAfterRemoval()}}
	loc, err := LeaveHA(context.Background(), r.deps(), 101, "a", time.Minute, ownedByRig)
	if err != nil {
		t.Fatal(err)
	}
	if loc.Node != "a" || len(r.events) != 0 {
		t.Fatalf("location %+v events %v, want the settled node and no write", loc, r.events)
	}
	if r.reads < 2 {
		t.Fatalf("the drain must read the CRM view, got %d reads", r.reads)
	}
}

func TestLeaveHALeavesAForeignResourceAlone(t *testing.T) {
	r := &leaveRig{barrierRig: &barrierRig{state: "started", comment: strPtr("someone else"),
		settleOn: []string{"a"}, states: droppedAfterRemoval()}}
	_, err := LeaveHA(context.Background(), r.deps(), 101, "a", time.Minute, ownedByRig)
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("err = %v, want retriable", err)
	}
	if len(r.events) != 0 || r.reads != 0 {
		t.Fatalf("a foreign resource must not be written or drained; events %v reads %d", r.events, r.reads)
	}
}

func TestLeaveHALeavesAnErrorStateResourceForTheOperator(t *testing.T) {
	r := &leaveRig{barrierRig: &barrierRig{state: "error", settleOn: []string{"a"}, states: droppedAfterRemoval()}}
	_, err := LeaveHA(context.Background(), r.deps(), 101, "a", time.Minute, ownedByRig)
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) || len(r.events) != 0 {
		t.Fatalf("err %v events %v, want a retriable refusal and no removal", err, r.events)
	}
}

func TestLeaveHAReadAndRemovalFailuresAreRetriable(t *testing.T) {
	for name, r := range map[string]*leaveRig{
		"read":    {barrierRig: &barrierRig{state: "started"}, getErr: errors.New("connection reset")},
		"removal": {barrierRig: &barrierRig{state: "started"}, delErr: errors.New("cfs lock timeout")},
	} {
		t.Run(name, func(t *testing.T) {
			r.settleOn, r.states = []string{"a"}, droppedAfterRemoval()
			_, err := LeaveHA(context.Background(), r.deps(), 101, "a", time.Minute, ownedByRig)
			if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
				t.Fatalf("err = %v, want retriable", err)
			}
			if r.reads != 0 {
				t.Fatalf("a failed removal must not drain; reads %d", r.reads)
			}
		})
	}
}

func TestLeaveHARemovalOfAnAlreadyRemovedResourceDrains(t *testing.T) {
	r := &leaveRig{barrierRig: &barrierRig{state: "started", settleOn: []string{"a"}, states: droppedAfterRemoval()},
		delErr: errors.New("no such resource 'vm:101'")}
	if _, err := LeaveHA(context.Background(), r.deps(), 101, "a", time.Minute, ownedByRig); err != nil {
		t.Fatalf("a removal that finds the resource gone counts as done: %v", err)
	}
}

func TestLeaveHATimesOutWhileTheCRMStillListsTheService(t *testing.T) {
	r := &leaveRig{barrierRig: &barrierRig{state: "started", settleOn: []string{"a"}, states: []*pve.HAServiceState{
		crm("started", 100, 90, 95),
		crm("started", 110, 112, 113),
	}}}
	_, err := LeaveHA(context.Background(), r.deps(), 101, "a", time.Minute, ownedByRig)
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("err = %v, want a retriable drain timeout", err)
	}
	if !slices.Equal(r.events, []string{"remove"}) {
		t.Fatalf("events %v; a failed drain writes nothing back", r.events)
	}
}

func TestLeaveHARefusesInvalidInput(t *testing.T) {
	r := &leaveRig{barrierRig: &barrierRig{state: "started", settleOn: []string{"a"}, states: droppedAfterRemoval()}}
	if _, err := LeaveHA(context.Background(), r.deps(), 0, "a", time.Minute, ownedByRig); err == nil {
		t.Error("VM ID 0 must be refused")
	}
	if _, err := LeaveHA(context.Background(), r.deps(), 101, "a", time.Minute, nil); err == nil {
		t.Error("a missing ownership check must be refused")
	}
	if _, err := LeaveHA(context.Background(), NarrowDeps{}, 101, "a", time.Minute, ownedByRig); err == nil {
		t.Error("missing dependencies must be refused")
	}
	if len(r.events) != 0 {
		t.Fatalf("refused input writes nothing; events %v", r.events)
	}
}

func TestNarrowPending(t *testing.T) {
	cases := []struct {
		name           string
		state, comment *string
		want           bool
	}{
		{"ignored with the token", strPtr("ignored"), strPtr("owner bosh-narrow-from=started"), true},
		{"ignored without the token", strPtr("ignored"), strPtr("owner"), false},
		{"started with the token", strPtr("started"), strPtr("owner bosh-narrow-from=started"), false},
		{"no state", nil, strPtr("owner bosh-narrow-from=started"), false},
		{"no comment", strPtr("ignored"), nil, false},
	}
	for _, tc := range cases {
		if got := NarrowPending(tc.state, tc.comment); got != tc.want {
			t.Errorf("%s: NarrowPending = %v, want %v", tc.name, got, tc.want)
		}
	}
}
