package ha

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// moveRig scripts the HA state reads, the settles, and the migrate request of
// a move, and records the requests in order.
type moveRig struct {
	events     []string
	states     []*pve.HAServiceState
	reads      int
	settleOn   []string
	settles    int
	now        time.Time
	migrateErr error
	blocking   []json.RawMessage
}

func (r *moveRig) CreateHaResourcesMigrate(_ context.Context, sid string,
	p *sdkcluster.CreateHaResourcesMigrateParams,
) (*sdkcluster.CreateHaResourcesMigrateResponse, error) {
	r.events = append(r.events, "migrate:"+p.Node)
	if r.migrateErr != nil {
		return nil, r.migrateErr
	}
	return &sdkcluster.CreateHaResourcesMigrateResponse{Sid: sid, RequestedNode: p.Node,
		BlockingResources: r.blocking}, nil
}

func (r *moveRig) deps() NarrowDeps {
	return NarrowDeps{
		Migrate: r,
		ReadState: func(context.Context, int, string) (*pve.HAServiceState, error) {
			i := min(r.reads, len(r.states)-1)
			r.reads++
			return r.states[i], nil
		},
		Settle: func(context.Context, int) (pve.VMLocation, error) {
			i := min(r.settles, len(r.settleOn)-1)
			r.settles++
			return pve.VMLocation{Node: r.settleOn[i], Found: true}, nil
		},
		Now: func() time.Time { return r.now },
		Sleep: func(context.Context, time.Duration) error {
			r.now = r.now.Add(10 * time.Second)
			return nil
		},
	}
}

// managedOn is an HA state that PVE HA holds as started on node.
func managedOn(raw, node string, entry, crmStamp int64) *pve.HAServiceState {
	return &pve.HAServiceState{Known: true, Configured: true, InCRM: true, Requested: "started", Raw: raw,
		Node: node, EntryTime: entry, CRMStamp: crmStamp, LRMStamp: crmStamp + 1}
}

func moveInput() MoveInput {
	return MoveInput{VMID: 101, Node: "b", Set: []string{"a"}, Targets: []string{"a"}, Budget: time.Minute}
}

func moveFailure(t *testing.T, err error) *MoveError {
	t.Helper()
	var me *MoveError
	if !errors.As(err, &me) {
		t.Fatalf("err = %v, want a MoveError", err)
	}
	return me
}

func TestMoveIntoSetMigratesTheVMAndWaitsForIt(t *testing.T) {
	r := &moveRig{settleOn: []string{"b", "a"}, states: []*pve.HAServiceState{
		managedOn("started", "b", 100, 90),
		managedOn("started", "b", 100, 90),
		managedOn("started", "b", 200, 190),
		managedOn("migrate", "b", 210, 205),
		managedOn("started", "a", 220, 215),
	}}
	loc, err := MoveIntoSet(context.Background(), r.deps(), moveInput())
	if err != nil {
		t.Fatal(err)
	}
	if loc.Node != "a" || !slices.Equal(r.events, []string{"migrate:a"}) {
		t.Fatalf("loc %+v events %v", loc, r.events)
	}
}

func TestMoveIntoSetRunsBeforeMigrateRightBeforeTheRequest(t *testing.T) {
	r := &moveRig{settleOn: []string{"b", "a"}, states: []*pve.HAServiceState{
		managedOn("started", "b", 100, 90),
		managedOn("started", "b", 100, 90),
		managedOn("started", "b", 200, 190),
		managedOn("started", "a", 220, 215),
	}}
	in := moveInput()
	in.BeforeMigrate = func(context.Context) error {
		r.events = append(r.events, "before")
		return nil
	}
	if _, err := MoveIntoSet(context.Background(), r.deps(), in); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(r.events, []string{"before", "migrate:a"}) {
		t.Fatalf("events %v, want the hook right before the request", r.events)
	}
}

func TestMoveIntoSetSendsNoRequestWhenBeforeMigrateFails(t *testing.T) {
	r := &moveRig{settleOn: []string{"b"}, states: []*pve.HAServiceState{managedOn("started", "b", 100, 90)}}
	in := moveInput()
	hold := errors.New("cfs lock timeout")
	in.BeforeMigrate = func(context.Context) error { return hold }
	_, err := MoveIntoSet(context.Background(), r.deps(), in)
	if me := moveFailure(t, err); me.Failure != MoveRefused || !errors.Is(err, hold) {
		t.Fatalf("failure %+v, want a refusal that wraps the hook's error", me)
	}
	if len(r.events) != 0 {
		t.Fatalf("events %v, want no request", r.events)
	}
}

func TestMoveIntoSetSkipsBeforeMigrateWhenNothingIsSent(t *testing.T) {
	down := managedOn("started", "b", 100, 90)
	down.NodeHealth = pve.NodeHealthEntry{Name: "b", DeadOrFenced: true}
	for name, r := range map[string]*moveRig{
		"node down": {settleOn: []string{"b"}, states: []*pve.HAServiceState{down}},
		"inside":    {settleOn: []string{"a"}, states: []*pve.HAServiceState{managedOn("migrate", "b", 100, 90)}},
		"no target": {settleOn: []string{"b"}, states: []*pve.HAServiceState{managedOn("started", "b", 100, 90)}},
	} {
		t.Run(name, func(t *testing.T) {
			in := moveInput()
			if name == "no target" {
				in.Targets = nil
			}
			ran := false
			in.BeforeMigrate = func(context.Context) error {
				ran = true
				return nil
			}
			_, _ = MoveIntoSet(context.Background(), r.deps(), in)
			if ran {
				t.Fatal("the hook ran for a move that sends no request")
			}
		})
	}
}

func TestMoveIntoSetSendsNothingWhenAMigrationInFlightEndsInsideTheSet(t *testing.T) {
	r := &moveRig{settleOn: []string{"a"}, states: []*pve.HAServiceState{managedOn("migrate", "b", 100, 90)}}
	loc, err := MoveIntoSet(context.Background(), r.deps(), moveInput())
	if err != nil || loc.Node != "a" || len(r.events) != 0 {
		t.Fatalf("loc %+v err %v events %v", loc, err, r.events)
	}
}

func TestMoveIntoSetRefusesANodeThatIsDown(t *testing.T) {
	down := managedOn("started", "b", 100, 90)
	down.NodeHealth = pve.NodeHealthEntry{Name: "b", DeadOrFenced: true}
	r := &moveRig{settleOn: []string{"b"}, states: []*pve.HAServiceState{down}}
	_, err := MoveIntoSet(context.Background(), r.deps(), moveInput())
	if me := moveFailure(t, err); me.Failure != MoveNodeDown || !strings.Contains(me.Error(), "node b is offline") {
		t.Fatalf("failure %+v", me)
	}
	if len(r.events) != 0 || r.settles != 0 {
		t.Fatalf("events %v settles %d, want nothing sent and no wait", r.events, r.settles)
	}
}

func TestMoveIntoSetReportsARefusal(t *testing.T) {
	r := &moveRig{settleOn: []string{"b"}, migrateErr: &sdkerrors.APIError{HTTPCode: 500,
		Message: "service 'vm:101' in error state, must be disabled and fixed first"},
		states: []*pve.HAServiceState{managedOn("started", "b", 100, 90)}}
	_, err := MoveIntoSet(context.Background(), r.deps(), moveInput())
	if me := moveFailure(t, err); me.Failure != MoveRefused || !strings.Contains(me.Error(), "refused to migrate vm:101") {
		t.Fatalf("failure %+v", me)
	}
}

func TestMoveIntoSetReportsBlockingResources(t *testing.T) {
	r := &moveRig{settleOn: []string{"b"}, blocking: []json.RawMessage{
		json.RawMessage(`{"sid":"vm:102","cause":"resource-affinity"}`),
	}, states: []*pve.HAServiceState{managedOn("started", "b", 100, 90)}}
	_, err := MoveIntoSet(context.Background(), r.deps(), moveInput())
	me := moveFailure(t, err)
	if me.Failure != MoveRefused || !strings.Contains(me.Error(), "vm:102 has a resource affinity rule") {
		t.Fatalf("failure %+v", me)
	}
}

func TestMoveIntoSetRefusesAResourceThatIsNotStartedOrStopped(t *testing.T) {
	ignored := managedOn("", "b", 100, 90)
	ignored.Requested, ignored.InCRM = "ignored", false
	r := &moveRig{settleOn: []string{"b"}, states: []*pve.HAServiceState{ignored}}
	_, err := MoveIntoSet(context.Background(), r.deps(), moveInput())
	if me := moveFailure(t, err); me.Failure != MoveRefused || len(r.events) != 0 {
		t.Fatalf("failure %+v events %v", me, r.events)
	}
}

func TestMoveIntoSetRefusesWhenNoNodeOfTheSetCanTakeTheVM(t *testing.T) {
	r := &moveRig{settleOn: []string{"b"}, states: []*pve.HAServiceState{managedOn("started", "b", 100, 90)}}
	in := moveInput()
	in.Targets = nil
	_, err := MoveIntoSet(context.Background(), r.deps(), in)
	if me := moveFailure(t, err); me.Failure != MoveRefused || !strings.Contains(me.Error(), "no node of a") {
		t.Fatalf("failure %+v", me)
	}
}

func TestMoveIntoSetNeedsTwoRoundsToSeeADroppedRequest(t *testing.T) {
	// The first round stamped after the request may have read its commands
	// before the request, so only the second one proves the CRM dropped it.
	r := &moveRig{settleOn: []string{"b"}, states: []*pve.HAServiceState{
		managedOn("started", "b", 100, 90),
		managedOn("started", "b", 100, 90),
		managedOn("started", "b", 200, 190),
		managedOn("started", "b", 210, 205),
		managedOn("started", "b", 220, 215),
	}}
	_, err := MoveIntoSet(context.Background(), r.deps(), moveInput())
	me := moveFailure(t, err)
	if me.Failure != MoveIncomplete || !strings.Contains(me.Error(), "left the VM on node b") || r.reads != 5 {
		t.Fatalf("failure %+v reads %d", me, r.reads)
	}
}

func TestMoveIntoSetGivesUpWhenTheBudgetRunsOut(t *testing.T) {
	r := &moveRig{settleOn: []string{"b"}, states: []*pve.HAServiceState{
		managedOn("started", "b", 100, 90),
		managedOn("started", "b", 100, 90),
		managedOn("migrate", "b", 200, 190),
	}}
	_, err := MoveIntoSet(context.Background(), r.deps(), moveInput())
	if me := moveFailure(t, err); me.Failure != MoveIncomplete || !strings.Contains(me.Error(), "within 1m0s") {
		t.Fatalf("failure %+v", me)
	}
}

func TestMoveTargetsOrdersPlaceableNodesByPriority(t *testing.T) {
	p := &Profile{Nodes: []NodeFacts{{Name: "a", Online: true}, {Name: "b", Online: true}, {Name: "c", Online: true},
		{Name: "d"}}}
	pin := naRule("p", true, map[string]int{"a": 0, "b": 0, "c": 2, "d": 2}, "vm:101")
	got := MoveTargets(p, []string{"d", "c", "b", "a"}, &pin, "b")
	if !slices.Equal(got, []string{"c", "a"}) {
		t.Fatalf("targets %v", got)
	}
}
