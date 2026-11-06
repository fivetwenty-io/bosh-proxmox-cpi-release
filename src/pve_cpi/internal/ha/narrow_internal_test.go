package ha

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// barrierRig records every step the barrier takes, in order.
type barrierRig struct {
	events     []string
	state      string // the resource's requested state
	missing    bool
	states     []*pve.HAServiceState
	reads      int
	settleOn   []string
	settles    int
	now        time.Time
	reachErr   error
	pinErr     error
	mutErr     error
	snapshot   []Rule
	restored   []Rule
	persisted  []Rule
	persistErr error
	// comment is the resource's comment, "owner" when nil.
	comment *string
}

func (r *barrierRig) currentComment() string {
	if r.comment == nil {
		return "owner"
	}
	return *r.comment
}

func (r *barrierRig) GetHaResources(_ context.Context, sid string) (*sdkcluster.GetHaResourcesResponse, error) {
	if r.missing {
		return nil, &sdkerrors.APIError{HTTPCode: 404, Message: "no such resource"}
	}
	st := r.state
	comment := r.currentComment()
	return &sdkcluster.GetHaResourcesResponse{Sid: sid, State: &st, Comment: &comment}, nil
}

func (r *barrierRig) CreateHaResources(_ context.Context, p *sdkcluster.CreateHaResourcesParams) error {
	r.events = append(r.events, "register:"+*p.State)
	r.state, r.missing = *p.State, false
	if p.Comment != nil {
		c := *p.Comment
		r.comment = &c
	}
	return nil
}

func (r *barrierRig) UpdateHaResources(_ context.Context, _ string, p *sdkcluster.UpdateHaResourcesParams) error {
	r.events = append(r.events, "request:"+*p.State)
	r.state = *p.State
	switch {
	case p.Comment != nil:
		c := *p.Comment
		r.comment = &c
	case p.Delete != nil && *p.Delete == "comment":
		empty := ""
		r.comment = &empty
	}
	return nil
}

func (r *barrierRig) DeleteHaResources(context.Context, string, *sdkcluster.DeleteHaResourcesParams) error {
	r.events = append(r.events, "remove")
	r.missing = true
	return nil
}

func (r *barrierRig) deps() NarrowDeps {
	return NarrowDeps{
		Resources: r,
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
		Mutate: func(_ context.Context, m pve.VMConfigMutation) (pve.VMLocation, error) {
			r.events = append(r.events, "mutate:"+m.Node)
			if r.mutErr != nil {
				return pve.VMLocation{}, r.mutErr
			}
			return pve.VMLocation{Node: m.Node, Found: true}, nil
		},
		Now: func() time.Time { return r.now },
		Sleep: func(context.Context, time.Duration) error {
			r.now = r.now.Add(10 * time.Second)
			return nil
		},
	}
}

func (r *barrierRig) input(old, next *Rule) NarrowInput {
	return NarrowInput{
		VMID: 101, Node: "a", OldPin: old, NewPin: next, SysConsole: true, Budget: time.Minute,
		Mutation: pve.VMConfigMutation{Apply: func(context.Context, string) error { return nil }},
		WritePin: func(_ context.Context, pin *Rule) error {
			id := "none"
			if pin != nil {
				id = pin.NodesParam()
			}
			r.events = append(r.events, "pin:"+id)
			return r.pinErr
		},
		CheckReach: func(_ context.Context, node string) error {
			r.events = append(r.events, "reach:"+node)
			return r.reachErr
		},
		Snapshot: func(context.Context) ([]Rule, error) { return r.snapshot, nil },
		Restore: func(_ context.Context, rules []Rule, pin *Rule) error {
			r.restored = rules
			r.events = append(r.events, "restore:"+pin.NodesParam())
			return nil
		},
		PersistSnapshot: func(_ context.Context, rules []Rule) error {
			r.events = append(r.events, "persist")
			r.persisted = rules
			return r.persistErr
		},
	}
}

func strPtr(s string) *string { return &s }

func crm(raw string, entry, crmStamp, lrmStamp int64) *pve.HAServiceState {
	return &pve.HAServiceState{Known: true, InCRM: true, Raw: raw, Node: "a", EntryTime: entry, CRMStamp: crmStamp,
		LRMStamp: lrmStamp}
}

func dropped(node string, entry, crmStamp, lrmStamp int64) *pve.HAServiceState {
	return &pve.HAServiceState{Known: true, Configured: true, Raw: "", Node: node, EntryTime: entry,
		CRMStamp: crmStamp, LRMStamp: lrmStamp}
}

var (
	pinA  = naRule("p", true, map[string]int{"a": 0}, "vm:101")
	pinAB = naRule("p", true, map[string]int{"a": 0, "b": 0}, "vm:101")
)

func TestNarrowRunsTheBarrierInOrder(t *testing.T) {
	r := &barrierRig{state: "started", settleOn: []string{"a"}, states: []*pve.HAServiceState{
		crm("started", 100, 90, 95),
		crm("started", 110, 90, 95),
		crm("started", 110, 112, 113), // the CRM still lists it
		dropped("a", 110, 108, 109),   // a round from before the request
		dropped("a", 110, 120, 115),   // the LRM has not reported since
		dropped("a", 110, 125, 130),
	}}
	loc, err := NarrowActive(context.Background(), r.deps(), r.input(&pinAB, &pinA))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"request:ignored", "pin:a", "mutate:a", "reach:a", "request:started"}
	if loc.Node != "a" || !slices.Equal(r.events, want) {
		t.Fatalf("events %v, want %v", r.events, want)
	}
}

func TestNarrowFollowsACRMMigrationAndRefusesOutsideTheSet(t *testing.T) {
	// The CRM moved the VM to b before it let go, and b is outside {a}.
	r := &barrierRig{state: "started", settleOn: []string{"b"}, states: []*pve.HAServiceState{
		crm("started", 100, 90, 95),
		crm("migrate", 110, 90, 95),
		dropped("a", 110, 120, 125),
		dropped("b", 110, 130, 135),
	}}
	_, err := NarrowActive(context.Background(), r.deps(), r.input(nil, &pinA))
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("err = %v", err)
	}
	if !slices.Equal(r.events, []string{"request:ignored", "request:started"}) {
		t.Fatalf("events %v", r.events)
	}
}

func TestNarrowTimesOutOnARunningMigration(t *testing.T) {
	r := &barrierRig{state: "started", settleOn: []string{"a"}, states: []*pve.HAServiceState{
		crm("started", 100, 90, 95),
		crm("migrate", 110, 112, 113),
	}}
	_, err := NarrowActive(context.Background(), r.deps(), r.input(nil, &pinA))
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) ||
		!slices.Equal(r.events, []string{"request:ignored", "request:started"}) {
		t.Fatalf("err %v events %v", err, r.events)
	}
}

func TestNarrowResumesAnIgnoredResource(t *testing.T) {
	// A crash left the resource ignored. The change no longer narrows, but the
	// barrier still finishes, without a second ignored request.
	r := &barrierRig{state: "ignored", settleOn: []string{"a"}, comment: strPtr("owner bosh-narrow-from=started"),
		states: []*pve.HAServiceState{
			dropped("a", 100, 90, 95),
			dropped("a", 110, 120, 125),
		}}
	if _, err := NarrowActive(context.Background(), r.deps(), r.input(&pinA, &pinA)); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(r.events, []string{"pin:a", "mutate:a", "reach:a", "request:started"}) ||
		r.currentComment() != "owner" {
		t.Fatalf("events %v comment %q", r.events, r.currentComment())
	}
}

func TestNarrowRestoresTheStateItFound(t *testing.T) {
	// A stopped resource goes back to stopped, and the token on the comment
	// names it while the barrier holds the resource.
	r := &barrierRig{state: "stopped", settleOn: []string{"a"}, states: []*pve.HAServiceState{
		crm("stopped", 100, 90, 95),
		dropped("a", 110, 120, 125),
	}}
	in := r.input(nil, &pinA)
	in.Mutation.Apply = func(context.Context, string) error {
		if r.state != "ignored" || r.currentComment() != "owner bosh-narrow-from=stopped" {
			t.Errorf("during the mutation state %s comment %q", r.state, r.currentComment())
		}
		return nil
	}
	if _, err := NarrowActive(context.Background(), r.deps(), in); err != nil {
		t.Fatal(err)
	}
	want := []string{"request:ignored", "pin:a", "mutate:a", "reach:a", "request:stopped"}
	if !slices.Equal(r.events, want) || r.state != "stopped" || r.currentComment() != "owner" {
		t.Fatalf("events %v state %s comment %q", r.events, r.state, r.currentComment())
	}
}

func TestNarrowLeavesAnOperatorsIgnoredAlone(t *testing.T) {
	// An ignored resource without the token is the operator's. The barrier
	// writes the pin and the change, and never touches the requested state.
	r := &barrierRig{state: "ignored", settleOn: []string{"a"}, states: []*pve.HAServiceState{
		dropped("a", 100, 90, 95),
		dropped("a", 110, 120, 125),
	}}
	if _, err := NarrowActive(context.Background(), r.deps(), r.input(nil, &pinA)); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(r.events, []string{"pin:a", "mutate:a", "reach:a"}) || r.state != "ignored" {
		t.Fatalf("events %v state %s", r.events, r.state)
	}
	// A failure puts nothing back either.
	r = &barrierRig{state: "ignored", settleOn: []string{"a"}, pinErr: errors.New("pin refused"),
		states: []*pve.HAServiceState{dropped("a", 100, 90, 95), dropped("a", 110, 120, 125)}}
	if _, err := NarrowActive(context.Background(), r.deps(), r.input(nil, &pinA)); err == nil ||
		r.state != "ignored" || slices.ContainsFunc(r.events, func(e string) bool { return e == "request:started" }) {
		t.Fatalf("err %v events %v state %s", err, r.events, r.state)
	}
	// The removal fallback never removes an operator's ignored resource.
	narrowUseRemoval = true
	t.Cleanup(func() { narrowUseRemoval = false })
	r = &barrierRig{state: "ignored", settleOn: []string{"a"}, states: []*pve.HAServiceState{
		dropped("a", 100, 90, 95), dropped("a", 110, 120, 125)}}
	if _, err := NarrowActive(context.Background(), r.deps(), r.input(nil, &pinA)); err != nil ||
		slices.Contains(r.events, "remove") {
		t.Fatalf("err %v events %v", err, r.events)
	}
}

func TestNarrowTreatsAStaleTokenAsTheCurrentState(t *testing.T) {
	// A token on a started resource is stale: the barrier runs in full and
	// puts back started, not the state the token names.
	r := &barrierRig{state: "started", settleOn: []string{"a"}, comment: strPtr("owner bosh-narrow-from=stopped"),
		states: []*pve.HAServiceState{crm("started", 100, 90, 95), dropped("a", 110, 120, 125)}}
	if _, err := NarrowActive(context.Background(), r.deps(), r.input(nil, &pinA)); err != nil {
		t.Fatal(err)
	}
	want := []string{"request:ignored", "pin:a", "mutate:a", "reach:a", "request:started"}
	if !slices.Equal(r.events, want) || r.currentComment() != "owner" {
		t.Fatalf("events %v comment %q", r.events, r.currentComment())
	}
}

func TestNarrowMarkRoundTrips(t *testing.T) {
	for _, tc := range []struct {
		comment *string
		want    string
	}{
		{nil, "bosh-narrow-from=started"},
		{strPtr(""), "bosh-narrow-from=started"},
		{strPtr("bosh-cpi:v1 owner=x"), "bosh-cpi:v1 owner=x bosh-narrow-from=started"},
	} {
		marked := withNarrowMark(tc.comment, "started")
		if marked != tc.want {
			t.Fatalf("withNarrowMark = %q, want %q", marked, tc.want)
		}
		if state, ok := narrowMarkOf(&marked); !ok || state != "started" {
			t.Fatalf("narrowMarkOf(%q) = %q %v", marked, state, ok)
		}
		want := ""
		if tc.comment != nil {
			want = *tc.comment
		}
		if stripped := stripNarrowMark(&marked); *stripped != want {
			t.Fatalf("stripNarrowMark(%q) = %q, want %q", marked, *stripped, want)
		}
	}
	if _, ok := narrowMarkOf(strPtr("bosh-cpi:v1 owner=x")); ok {
		t.Fatal("an unmarked comment reads as marked")
	}
	if stripNarrowMark(nil) != nil {
		t.Fatal("a nil comment came back non-nil")
	}
}

func TestNarrowResumesAfterEachStep(t *testing.T) {
	// The pin write fails: the barrier asks for started again and returns the
	// error. A failed reach check leaves the resource ignored.
	r := &barrierRig{state: "started", settleOn: []string{"a"}, pinErr: errors.New("pin refused"),
		states: []*pve.HAServiceState{crm("started", 100, 90, 95), dropped("a", 110, 120, 125)}}
	if _, err := NarrowActive(context.Background(), r.deps(), r.input(nil, &pinA)); err == nil ||
		r.state != "started" {
		t.Fatalf("pin failure err %v state %s", err, r.state)
	}
	r = &barrierRig{state: "started", settleOn: []string{"a"}, mutErr: errors.New("config locked"),
		states: []*pve.HAServiceState{crm("started", 100, 90, 95), dropped("a", 110, 120, 125)}}
	if _, err := NarrowActive(context.Background(), r.deps(), r.input(nil, &pinA)); err == nil ||
		r.state != "started" {
		t.Fatalf("mutation failure err %v state %s", err, r.state)
	}
	r = &barrierRig{state: "started", settleOn: []string{"a"}, reachErr: errors.New("no storage"),
		states: []*pve.HAServiceState{crm("started", 100, 90, 95), dropped("a", 110, 120, 125)}}
	_, err := NarrowActive(context.Background(), r.deps(), r.input(nil, &pinA))
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) || r.state != "ignored" {
		t.Fatalf("reach failure err %v state %s", err, r.state)
	}
	// The next call finds it ignored and finishes.
	r.reachErr, r.events, r.reads, r.states = nil, nil, 0,
		[]*pve.HAServiceState{dropped("a", 100, 90, 95), dropped("a", 110, 120, 125)}
	if _, err := NarrowActive(context.Background(), r.deps(), r.input(&pinA, &pinA)); err != nil ||
		r.state != "started" {
		t.Fatalf("resume err %v state %s events %v", err, r.state, r.events)
	}
	if r.currentComment() != "owner" {
		t.Fatalf("the token stayed on the comment: %q", r.currentComment())
	}
}

func TestNarrowWithoutSysConsole(t *testing.T) {
	r := &barrierRig{state: "started"}
	in := r.input(nil, &pinA)
	in.SysConsole = false
	if _, err := NarrowActive(context.Background(), r.deps(), in); !cpierrors.IsType(err,
		cpierrors.TypeRetriableCloud) || len(r.events) != 0 {
		t.Fatalf("err %v events %v", err, r.events)
	}
	in = r.input(&pinA, &pinAB)
	in.SysConsole = false
	if _, err := NarrowActive(context.Background(), r.deps(), in); err != nil ||
		!slices.Equal(r.events, []string{"mutate:a", "pin:a,b"}) {
		t.Fatalf("widening without Sys.Console: err %v events %v", err, r.events)
	}
}

func TestWideningSkipsTheBarrier(t *testing.T) {
	r := &barrierRig{state: "started"}
	if _, err := NarrowActive(context.Background(), r.deps(), r.input(&pinA, nil)); err != nil ||
		!slices.Equal(r.events, []string{"mutate:a", "pin:none"}) {
		t.Fatalf("err %v events %v", err, r.events)
	}
	r = &barrierRig{state: "started"}
	if _, err := NarrowActive(context.Background(), r.deps(), r.input(&pinA, &pinA)); err != nil ||
		!slices.Equal(r.events, []string{"mutate:a"}) {
		t.Fatalf("unchanged pin: err %v events %v", err, r.events)
	}
}

func TestNarrowRefusesTheErrorState(t *testing.T) {
	r := &barrierRig{state: "started", states: []*pve.HAServiceState{crm("error", 100, 90, 95)}}
	if _, err := NarrowActive(context.Background(), r.deps(), r.input(nil, &pinA)); !cpierrors.IsType(err,
		cpierrors.TypeRetriableCloud) || len(r.events) != 0 {
		t.Fatalf("err %v events %v", err, r.events)
	}
	r = &barrierRig{state: "started", settleOn: []string{"a"}, states: []*pve.HAServiceState{
		crm("started", 100, 90, 95), crm("started", 110, 90, 95), crm("error", 110, 112, 113)}}
	if _, err := NarrowActive(context.Background(), r.deps(), r.input(nil, &pinA)); !cpierrors.IsType(err,
		cpierrors.TypeRetriableCloud) || slices.Contains(r.events, "request:disabled") {
		t.Fatalf("drain error: err %v events %v", err, r.events)
	}
	unknown := &barrierRig{state: "started", states: []*pve.HAServiceState{{Known: false, Reason: "no manager"}}}
	if _, err := NarrowActive(context.Background(), unknown.deps(), unknown.input(nil, &pinA)); err == nil {
		t.Fatal("an unknown state was accepted")
	}
}

func TestNarrowOnAVMOutsideHA(t *testing.T) {
	r := &barrierRig{missing: true}
	if _, err := NarrowActive(context.Background(), r.deps(), r.input(nil, &pinA)); err != nil ||
		!slices.Equal(r.events, []string{"mutate:a", "reach:a"}) {
		t.Fatalf("err %v events %v", err, r.events)
	}
	if _, err := NarrowActive(context.Background(), r.deps(), NarrowInput{VMID: 101}); err == nil {
		t.Fatal("a call without a mutation was accepted")
	}
}

func TestNarrowRemovalFallback(t *testing.T) {
	narrowUseRemoval = true
	t.Cleanup(func() { narrowUseRemoval = false })
	aa := ourAA("aa", "vm:101", "vm:102")
	r := &barrierRig{state: "started", settleOn: []string{"a"}, snapshot: []Rule{aa, pinAB},
		states: []*pve.HAServiceState{crm("started", 100, 90, 95), crm("started", 100, 90, 95),
			{Known: true, Node: "a"}}}
	if _, err := NarrowActive(context.Background(), r.deps(), r.input(&pinAB, &pinA)); err != nil {
		t.Fatal(err)
	}
	want := []string{"persist", "remove", "mutate:a", "reach:a", "register:ignored", "restore:a", "request:started"}
	if !slices.Equal(r.events, want) || len(r.restored) != 1 || r.restored[0].ID != "aa" ||
		len(r.persisted) != 2 || r.currentComment() != "owner" {
		t.Fatalf("events %v restored %+v persisted %d comment %q", r.events, r.restored, len(r.persisted),
			r.currentComment())
	}

	// A failed mutation registers the resource again and leaves it ignored.
	r = &barrierRig{state: "started", settleOn: []string{"a"}, mutErr: errors.New("locked"),
		states: []*pve.HAServiceState{crm("started", 100, 90, 95), {Known: true, Node: "a"}}}
	if _, err := NarrowActive(context.Background(), r.deps(), r.input(nil, &pinA)); err == nil ||
		r.state != "ignored" || slices.Contains(r.events, "request:started") {
		t.Fatalf("err %v events %v", err, r.events)
	}
	// The resource it left carries the token, so the next call restores
	// started.
	if from, ok := narrowMarkOf(r.comment); !ok || from != "started" {
		t.Fatalf("the re-registered resource lacks the token: %q", r.currentComment())
	}
	in := r.input(nil, &pinA)
	in.Snapshot = nil
	r.state, r.comment = "started", nil
	if _, err := NarrowActive(context.Background(), r.deps(), in); err == nil {
		t.Fatal("the fallback ran without a snapshot")
	}

	// Without a place to persist the snapshot the fallback removes nothing.
	r = &barrierRig{state: "started", settleOn: []string{"a"}, states: []*pve.HAServiceState{
		crm("started", 100, 90, 95), {Known: true, Node: "a"}}}
	in = r.input(nil, &pinA)
	in.PersistSnapshot = nil
	if _, err := NarrowActive(context.Background(), r.deps(), in); err == nil || len(r.events) != 0 {
		t.Fatalf("the fallback ran without persisting: err %v events %v", err, r.events)
	}
	// A failed persist removes nothing.
	r = &barrierRig{state: "started", settleOn: []string{"a"}, persistErr: errors.New("journal full"),
		states: []*pve.HAServiceState{crm("started", 100, 90, 95), {Known: true, Node: "a"}}}
	_, err := NarrowActive(context.Background(), r.deps(), r.input(nil, &pinA))
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) || !slices.Equal(r.events, []string{"persist"}) {
		t.Fatalf("a failed persist: err %v events %v", err, r.events)
	}
}
