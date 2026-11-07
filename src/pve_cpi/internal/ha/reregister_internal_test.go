package ha

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

const readdMarker = "bosh-cpi:v1 owner=us deployment=cf group=router"

// readdRig is one VM with an HA resource, its rules, and its config tags, and
// it records every write in order. fail makes the named step fail once, which
// is how a test stops the order at a crash point.
type readdRig struct {
	events []string

	exists    bool
	requested string
	comment   string
	rebalance bool
	// raw is the CRM state while the CRM lists the service, and listed says
	// whether it does.
	raw    string
	listed bool
	uid    string
	// linger keeps the CRM listing the removed resource in error for that
	// many more reads.
	linger int

	node      string
	health    pve.NodeHealthState
	lrmFresh  bool
	unknown   bool
	power     string
	landing   string
	tick      int64
	rules     []Rule
	tags      []string
	record    *ReaddRecord
	recordErr error

	// decide shapes the decision the order gets.
	decide func(d *VMDecision)
	// unconfined makes the confinement write report an auto failure.
	unconfined string
	watchdog   pve.WatchdogState
	fragile    bool
	managed    bool

	fail map[string]error
	// answered makes the first removal a refusal PVE answered.
	answered bool
}

func newReaddRig() *readdRig {
	return &readdRig{
		exists: true, requested: haStateStarted, comment: readdMarker, raw: haStateError, listed: true, uid: "u1",
		node: "a", health: pve.NodeHealthOnline, lrmFresh: true, power: "running", landing: "a",
		rules:    []Rule{readdPin(false)},
		watchdog: pve.WatchdogArmed,
		fail:     map[string]error{},
	}
}

func readdPin(disabled bool) Rule {
	r := naRule("bosh-na-101", true, map[string]int{"a": 0, "b": 0}, "vm:101")
	r.Comment, r.Disabled = readdMarker, disabled
	return r
}

func (r *readdRig) failOnce(step string) error {
	if err, ok := r.fail[step]; ok {
		delete(r.fail, step)
		r.events = append(r.events, "crash:"+step)
		return err
	}
	return nil
}

var errCrash = errors.New("the process stopped here")

func (r *readdRig) GetHaResources(_ context.Context, sid string) (*sdkcluster.GetHaResourcesResponse, error) {
	if err := r.failOnce("get"); err != nil {
		return nil, err
	}
	if !r.exists {
		return nil, &sdkerrors.APIError{HTTPCode: 404, Message: "no such resource"}
	}
	st, c := r.requested, r.comment
	return &sdkcluster.GetHaResourcesResponse{Sid: sid, State: &st, Comment: &c, AutoRebalance: pveBool(r.rebalance)},
		nil
}

func (r *readdRig) CreateHaResources(_ context.Context, p *sdkcluster.CreateHaResourcesParams) error {
	if err := r.failOnce("stage"); err != nil {
		return err
	}
	if r.exists {
		return errors.New("resource 'vm:101' already defined")
	}
	r.events = append(r.events, "stage:"+*p.State)
	r.exists, r.requested, r.comment, r.listed = true, *p.State, *p.Comment, false
	r.rebalance = p.AutoRebalance != nil && *p.AutoRebalance
	return nil
}

func (r *readdRig) UpdateHaResources(_ context.Context, _ string, p *sdkcluster.UpdateHaResourcesParams) error {
	state := "-"
	if p.State != nil {
		state = *p.State
	}
	if state == "disabled" {
		r.events = append(r.events, "DISABLED")
	}
	if err := r.failOnce("request:" + state); err != nil {
		return err
	}
	r.events = append(r.events, "request:"+state)
	if p.State != nil {
		r.requested = *p.State
		if *p.State == haStateStarted {
			r.listed, r.raw, r.uid = true, haStateStarted, "u2"
		}
	}
	if p.Comment != nil {
		r.comment = *p.Comment
	}
	return nil
}

func (r *readdRig) DeleteHaResources(_ context.Context, _ string, p *sdkcluster.DeleteHaResourcesParams) error {
	if p == nil || p.Purge == nil || !*p.Purge {
		r.events = append(r.events, "remove-without-purge")
	}
	if err := r.failOnce("remove"); err != nil {
		return err
	}
	if r.answered {
		r.answered = false
		r.events = append(r.events, "remove-refused")
		return &sdkerrors.APIError{HTTPCode: 400, Message: "resource is used in rule bosh-na-101"}
	}
	r.events = append(r.events, "remove")
	r.exists = false
	if r.linger == 0 {
		r.listed = false
	}
	return nil
}

func (r *readdRig) readState(context.Context, int, string) (*pve.HAServiceState, error) {
	if err := r.failOnce("read"); err != nil {
		return nil, err
	}
	r.tick++
	if r.unknown {
		return &pve.HAServiceState{SID: "vm:101", Reason: "the entry node is not quorate"}, nil
	}
	e := r.tick * 10
	st := &pve.HAServiceState{
		SID: "vm:101", VMID: 101, Known: true, Configured: r.exists, Node: r.node, CRMPresent: true,
		LRMFresh: r.lrmFresh, EntryTime: e, CRMStamp: e + 1, LRMStamp: e + 2,
		NodeHealth: pve.NodeHealthEntry{Name: r.node, Health: r.health, DeadOrFenced: r.health == pve.NodeHealthDead},
	}
	if r.exists {
		st.Requested = r.requested
	}
	if r.listed {
		st.InCRM, st.Raw, st.UID = true, r.raw, r.uid
		if !r.exists && r.linger > 0 {
			r.linger--
			if r.linger == 0 {
				r.listed = false
			}
		}
	}
	return st, nil
}

func (r *readdRig) deps() NarrowDeps {
	now := time.Unix(1700000000, 0)
	return NarrowDeps{
		Resources: r,
		ReadState: r.readState,
		Settle: func(context.Context, int) (pve.VMLocation, error) {
			return pve.VMLocation{Node: r.node, Found: true}, nil
		},
		Now: func() time.Time { return now },
		Sleep: func(context.Context, time.Duration) error {
			now = now.Add(time.Second)
			return nil
		},
	}
}

func (r *readdRig) decision(node string) *VMDecision {
	d := &VMDecision{VMID: 101, SID: "vm:101", Node: node, Outcome: OutcomeRegister, Policy: PolicyShared,
		Admissible: []string{"a", "b"}, Hard: []string{"a", "b"}, Placed: node}
	if r.decide != nil {
		r.decide(d)
	}
	return d
}

func (r *readdRig) profile() *Profile {
	return &Profile{Mode: config.HAModeAuto, Effective: EffectiveFull, SysConsole: true}
}

func (r *readdRig) input() ReregisterInput {
	in := ReregisterInput{
		VMID: 101, Node: "a", Profile: r.profile(), PinID: "bosh-na-101", Budget: time.Minute,
		Stager: &Stager{
			Resources: r,
			Owned:     ownedMarker,
			ReadRules: func(context.Context) (*RuleSet, error) {
				if err := r.failOnce("rules"); err != nil {
					return nil, err
				}
				return &RuleSet{Rules: slices.Clone(r.rules), Digest: "d"}, nil
			},
			Watchdog: func(context.Context, string) (pve.WatchdogState, error) { return r.watchdog, nil },
			SetStarted: func(context.Context, int, string) (*pve.SetHAStateResult, error) {
				if err := r.failOnce("wait"); err != nil {
					return nil, err
				}
				return &pve.SetHAStateResult{Outcome: pve.HAStateReached, SID: "vm:101", VMID: 101, Node: r.landing}, nil
			},
			Managed:       r.managed,
			FragileQuorum: r.fragile,
		},
		Power: func(context.Context, string) (string, error) {
			if err := r.failOnce("power"); err != nil {
				return "", err
			}
			return r.power, nil
		},
		ReadIntent: func(context.Context, string) (ReaddIntent, error) {
			if err := r.failOnce("intent"); err != nil {
				return ReaddIntent{}, err
			}
			tag, _, _ := ReaddTagOf(r.tags)
			return ReaddIntent{Tag: tag, Record: r.record, RecordErr: r.recordErr}, nil
		},
		WriteIntent: func(_ context.Context, _ string, tag string, rec ReaddRecord) error {
			if err := r.failOnce("tag"); err != nil {
				return err
			}
			r.events = append(r.events, "tag")
			r.tags = append(WithoutReaddTags(r.tags), tag)
			r.record = &rec
			return nil
		},
		ClearIntent: func(_ context.Context, _ string, outcome ReaddOutcome) error {
			if err := r.failOnce("untag"); err != nil {
				return err
			}
			r.events = append(r.events, "untag:"+string(outcome))
			r.tags, r.record = WithoutReaddTags(r.tags), nil
			return nil
		},
		Decide: func(_ context.Context, node string, _ *pve.HAServiceState, _ ReaddRecord) (*VMDecision, error) {
			if err := r.failOnce("decide"); err != nil {
				return nil, err
			}
			return r.decision(node), nil
		},
		LeaveRules: func(context.Context) error {
			r.events = append(r.events, "leave-rules")
			return r.failOnce("leave-rules")
		},
		Confine: func(_ context.Context, d *VMDecision, rec ReaddRecord, _ string) (Confinement, error) {
			if err := r.failOnce("confine"); err != nil {
				return Confinement{}, err
			}
			if r.unconfined != "" {
				r.events = append(r.events, "unconfined")
				return Confinement{Unconfined: r.unconfined}, nil
			}
			pin := readdPin(false)
			pin.Nodes = map[string]int{}
			for _, n := range d.Admissible {
				pin.Nodes[n] = 0
			}
			r.rules = slices.DeleteFunc(r.rules, func(x Rule) bool { return x.ID == pin.ID })
			r.rules = append(r.rules, pin)
			r.events = append(r.events, "confine")
			return Confinement{Pin: &pin}, nil
		},
		WithoutHA: func(_ context.Context, _ *VMDecision, _ ReaddRecord, _ string) error {
			r.events = append(r.events, "without-ha")
			r.rules = slices.DeleteFunc(r.rules, func(x Rule) bool { return x.ID == "bosh-na-101" })
			return nil
		},
	}
	return in
}

func (r *readdRig) run(t *testing.T, in ReregisterInput) (ReregisterResult, error) {
	t.Helper()
	return ReregisterErrored(context.Background(), r.deps(), in)
}

// registeredAgain checks the end state of a finished order: the resource
// requests started with our marker, the pin is there, the tag is gone, and
// nothing ever requested disabled.
func (r *readdRig) registeredAgain(t *testing.T, res ReregisterResult, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("err = %v (events %v)", err, r.events)
	}
	if res.Outcome != ReaddRegistered || res.Node != "a" {
		t.Fatalf("result %+v (events %v)", res, r.events)
	}
	if !r.exists || r.requested != haStateStarted || r.comment != readdMarker {
		t.Fatalf("resource exists=%v requested=%s comment=%q", r.exists, r.requested, r.comment)
	}
	if _, _, ok := ReaddTagOf(r.tags); ok || r.record != nil {
		t.Fatalf("tags %v record %+v", r.tags, r.record)
	}
	if !slices.ContainsFunc(r.rules, func(x Rule) bool { return x.ID == "bosh-na-101" && !x.Disabled }) {
		t.Fatalf("rules %+v", r.rules)
	}
	r.neverDisabled(t)
}

func (r *readdRig) neverDisabled(t *testing.T) {
	t.Helper()
	if slices.Contains(r.events, "DISABLED") || slices.Contains(r.events, "remove-without-purge") {
		t.Fatalf("events %v", r.events)
	}
}

func TestReregisterRunsTheWholeOrder(t *testing.T) {
	r := newReaddRig()
	res, err := r.run(t, r.input())
	r.registeredAgain(t, res, err)
	want := []string{"tag", "remove", "stage:ignored", "confine", "request:started", "untag:registered"}
	if !slices.Equal(r.events, want) {
		t.Fatalf("events %v, want %v", r.events, want)
	}
}

// TestReregisterRecoversFromEveryCrashPoint stops the order after each of its
// steps and checks that the next call finishes it from what it finds.
func TestReregisterRecoversFromEveryCrashPoint(t *testing.T) {
	for _, step := range []string{
		"remove",          // the tag is written and the resource is still in error
		"stage",           // the resource is removed and nothing is staged
		"confine",         // the resource is staged and the rules are not written
		"request:started", // the rules are written and started is not requested
		"wait",            // started is requested and the landing is not checked
		"untag",           // the VM runs under HA and the tag is still there
	} {
		t.Run(step, func(t *testing.T) {
			r := newReaddRig()
			r.fail[step] = errCrash
			if _, err := r.run(t, r.input()); err == nil {
				t.Fatalf("the crash at %s did not stop the order (events %v)", step, r.events)
			}
			if _, _, tagged := ReaddTagOf(r.tags); !tagged {
				t.Fatalf("a crash at %s left no tag (events %v)", step, r.events)
			}
			res, err := r.run(t, r.input())
			r.registeredAgain(t, res, err)
		})
	}
}

func TestReregisterSurvivesTwoCrashesInARow(t *testing.T) {
	r := newReaddRig()
	r.fail["stage"] = errCrash
	_, _ = r.run(t, r.input())
	r.fail["confine"] = errCrash
	_, _ = r.run(t, r.input())
	res, err := r.run(t, r.input())
	r.registeredAgain(t, res, err)
}

func TestReregisterKeepsTheTagAge(t *testing.T) {
	r := newReaddRig()
	old := ReaddTag(time.Unix(1000, 0))
	r.tags = []string{"other", old}
	r.fail["remove"] = errCrash
	_, _ = r.run(t, r.input())
	if !slices.Contains(r.tags, old) || !slices.Contains(r.tags, "other") {
		t.Fatalf("tags %v", r.tags)
	}
}

func TestReregisterPreconditionsWriteNothing(t *testing.T) {
	for name, tc := range map[string]struct {
		setup   func(r *readdRig, in *ReregisterInput)
		blocked bool
		stopped bool
	}{
		"stopped VM": {setup: func(r *readdRig, _ *ReregisterInput) { r.power = "stopped" }, stopped: true},
		"paused VM":  {setup: func(r *readdRig, _ *ReregisterInput) { r.power = "paused" }},
		"power read": {setup: func(r *readdRig, _ *ReregisterInput) { r.fail["power"] = errCrash }},
		"dead node":  {setup: func(r *readdRig, _ *ReregisterInput) { r.health = pve.NodeHealthDead }},
		"maintenance node": {setup: func(r *readdRig, _ *ReregisterInput) {
			r.health = pve.NodeHealthMaintenance
		}},
		"stale LRM":          {setup: func(r *readdRig, _ *ReregisterInput) { r.lrmFresh = false }},
		"non-quorate entry":  {setup: func(r *readdRig, _ *ReregisterInput) { r.unknown = true }},
		"state read failure": {setup: func(r *readdRig, _ *ReregisterInput) { r.fail["read"] = errCrash }},
		"resource read":      {setup: func(r *readdRig, _ *ReregisterInput) { r.fail["get"] = errCrash }},
		"intent read":        {setup: func(r *readdRig, _ *ReregisterInput) { r.fail["intent"] = errCrash }},
		"rules read":         {setup: func(r *readdRig, _ *ReregisterInput) { r.fail["rules"] = errCrash }},
		"no Sys.Console": {setup: func(_ *readdRig, in *ReregisterInput) {
			in.Profile.SysConsole = false
		}, blocked: true},
		"profile without writes": {setup: func(_ *readdRig, in *ReregisterInput) {
			in.Profile.Effective, in.Profile.Reason = EffectiveOff, "pve.ha.mode is off"
		}, blocked: true},
		"uid changed since the decision": {setup: func(_ *readdRig, in *ReregisterInput) {
			in.Decided = &pve.HAServiceState{SID: "vm:101", UID: "u0"}
		}},
		"outside the admissible set": {setup: func(r *readdRig, _ *ReregisterInput) {
			r.decide = func(d *VMDecision) { d.Admissible, d.Hard = []string{"b"}, []string{"b"} }
		}, blocked: true},
		"decision gives no HA": {setup: func(r *readdRig, _ *ReregisterInput) {
			r.decide = func(d *VMDecision) { d.Outcome, d.Reason = OutcomeSkip, "local disk" }
		}, blocked: true},
		"transient decision": {setup: func(r *readdRig, _ *ReregisterInput) {
			r.decide = func(d *VMDecision) { d.Outcome, d.Transient, d.Reason = OutcomeSkip, true, "node c down" }
		}},
		"placeable shortfall": {setup: func(r *readdRig, _ *ReregisterInput) {
			r.decide = func(d *VMDecision) { d.Outcome, d.PlaceableShortfall = OutcomeSkip, true }
		}},
		"tag write": {setup: func(r *readdRig, _ *ReregisterInput) { r.fail["tag"] = errCrash }},
	} {
		t.Run(name, func(t *testing.T) {
			r := newReaddRig()
			in := r.input()
			tc.setup(r, &in)
			_, err := r.run(t, in)
			if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
				t.Fatalf("err = %v", err)
			}
			if got := errors.Is(err, ErrReaddBlocked); got != tc.blocked {
				t.Fatalf("blocked = %v, want %v: %v", got, tc.blocked, err)
			}
			if got := errors.Is(err, ErrReaddVMStopped); got != tc.stopped {
				t.Fatalf("stopped = %v, want %v: %v", got, tc.stopped, err)
			}
			if !r.exists || slices.Contains(r.events, "remove") || slices.Contains(r.events, "tag") {
				t.Fatalf("events %v", r.events)
			}
			r.neverDisabled(t)
		})
	}
}

func TestReregisterWithoutSysConsoleNamesTheCommands(t *testing.T) {
	r := newReaddRig()
	in := r.input()
	in.Profile.SysConsole = false
	_, err := r.run(t, in)
	for _, want := range []string{"Sys.Console", "ha-manager remove vm:101", "ha-manager add vm:101 --state started"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("err %v lacks %q", err, want)
		}
	}
}

func TestReregisterRefusesAUIDChangeAfterTheTagWrite(t *testing.T) {
	r := newReaddRig()
	in := r.input()
	in.WriteIntent = func(_ context.Context, _ string, tag string, rec ReaddRecord) error {
		r.tags, r.record = append(r.tags, tag), &rec
		r.uid = "u9" // an operator cleared and re-raised the error meanwhile
		return nil
	}
	_, err := r.run(t, in)
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) || !r.exists || slices.Contains(r.events, "remove") {
		t.Fatalf("err %v events %v", err, r.events)
	}
	// The next call starts from the new episode and finishes.
	res, err := r.run(t, r.input())
	r.registeredAgain(t, res, err)
}

func TestReregisterLeavesTheRulesWhenPVERefusesTheRemoval(t *testing.T) {
	r := newReaddRig()
	r.answered = true
	res, err := r.run(t, r.input())
	r.registeredAgain(t, res, err)
	if !slices.Equal(r.events[:4], []string{"tag", "remove-refused", "leave-rules", "remove"}) {
		t.Fatalf("events %v", r.events)
	}
}

func TestReregisterReportsAFailedRuleLeave(t *testing.T) {
	r := newReaddRig()
	r.answered = true
	r.fail["leave-rules"] = errCrash
	_, err := r.run(t, r.input())
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) || !r.exists {
		t.Fatalf("err %v exists %v", err, r.exists)
	}
}

func TestReregisterCountsAnUnansweredRemovalThatLanded(t *testing.T) {
	r := newReaddRig()
	in := r.input()
	inner := in.Stager.Resources
	in.Stager = &Stager{Resources: &lostAnswer{ResourceAPI: inner, rig: r}, Owned: in.Stager.Owned,
		ReadRules: in.Stager.ReadRules, Watchdog: in.Stager.Watchdog, SetStarted: in.Stager.SetStarted}
	res, err := r.run(t, in)
	r.registeredAgain(t, res, err)
}

// lostAnswer removes the resource and then loses PVE's answer.
type lostAnswer struct {
	ResourceAPI
	rig  *readdRig
	done bool
}

func (l *lostAnswer) DeleteHaResources(ctx context.Context, sid string, p *sdkcluster.DeleteHaResourcesParams) error {
	if err := l.ResourceAPI.DeleteHaResources(ctx, sid, p); err != nil || l.done {
		return err
	}
	l.done = true
	return errors.New("connection reset")
}

func TestReregisterDrainToleratesALingeringErrorEntry(t *testing.T) {
	r := newReaddRig()
	r.linger = 3
	res, err := r.run(t, r.input())
	r.registeredAgain(t, res, err)
}

func TestReregisterIdleWatchdogInAutoRunsWithoutHA(t *testing.T) {
	r := newReaddRig()
	r.fragile, r.watchdog = true, pve.WatchdogIdle
	res, err := r.run(t, r.input())
	if err != nil || res.Outcome != ReaddWithoutHA {
		t.Fatalf("res %+v err %v", res, err)
	}
	if r.exists || slices.ContainsFunc(r.rules, func(x Rule) bool { return x.ID == "bosh-na-101" }) {
		t.Fatalf("exists %v rules %+v", r.exists, r.rules)
	}
	if _, _, ok := ReaddTagOf(r.tags); ok {
		t.Fatalf("tags %v", r.tags)
	}
	r.neverDisabled(t)
}

func TestReregisterIdleWatchdogInManagedActivates(t *testing.T) {
	r := newReaddRig()
	r.fragile, r.watchdog, r.managed = true, pve.WatchdogIdle, true
	res, err := r.run(t, r.input())
	r.registeredAgain(t, res, err)
}

func TestReregisterConfinementFailureInAutoRunsWithoutHA(t *testing.T) {
	r := newReaddRig()
	r.unconfined = "the node-affinity pin could not be written"
	res, err := r.run(t, r.input())
	if err != nil || res.Outcome != ReaddWithoutHA || r.exists {
		t.Fatalf("res %+v err %v exists %v", res, err, r.exists)
	}
	if !slices.Contains(r.events, "without-ha") || !slices.Contains(r.events, "untag:without_ha") {
		t.Fatalf("events %v", r.events)
	}
}

func TestReregisterConfinementFailureInManagedStaysStaged(t *testing.T) {
	r := newReaddRig()
	r.managed = true
	r.fail["confine"] = cpierrors.Retriable("node-affinity: the write was refused")
	_, err := r.run(t, r.input())
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) || !r.exists || r.requested != haStateIgnored {
		t.Fatalf("err %v exists %v requested %s", err, r.exists, r.requested)
	}
	if _, _, ok := ReaddTagOf(r.tags); !ok {
		t.Fatalf("tags %v", r.tags)
	}
}

func TestReregisterUnverifiedConfinement(t *testing.T) {
	disabledAA := Rule{ID: "bosh-aa-x", Type: RuleTypeResourceAffinity, Affinity: AffinityNegative,
		Resources: []string{"vm:101", "vm:102"}, Comment: readdMarker, Disabled: true}
	for name, managed := range map[string]bool{"auto": false, "managed": true} {
		t.Run(name, func(t *testing.T) {
			r := newReaddRig()
			r.managed = managed
			r.rules = append(r.rules, disabledAA)
			in := r.input()
			confine := in.Confine
			in.Confine = func(ctx context.Context, d *VMDecision, rec ReaddRecord, node string) (Confinement, error) {
				c, err := confine(ctx, d, rec, node)
				c.AntiAffinity = "bosh-aa-x"
				return c, err
			}
			res, err := r.run(t, in)
			if managed {
				if !errors.Is(err, ErrConfinementUnverified) || !r.exists || r.requested != haStateIgnored {
					t.Fatalf("err %v exists %v requested %s", err, r.exists, r.requested)
				}
				return
			}
			if err != nil || res.Outcome != ReaddWithoutHA || r.exists {
				t.Fatalf("res %+v err %v exists %v", res, err, r.exists)
			}
		})
	}
}

func TestReregisterRulesReadFailureDuringVerifyDefers(t *testing.T) {
	r := newReaddRig()
	in := r.input()
	confine := in.Confine
	in.Confine = func(ctx context.Context, d *VMDecision, rec ReaddRecord, node string) (Confinement, error) {
		c, err := confine(ctx, d, rec, node)
		r.fail["rules"] = errCrash
		return c, err
	}
	_, err := r.run(t, in)
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) || !r.exists || r.requested != haStateIgnored {
		t.Fatalf("err %v exists %v requested %s", err, r.exists, r.requested)
	}
	res, err := r.run(t, r.input())
	r.registeredAgain(t, res, err)
}

func TestReregisterLandingOutsideTheSetKeepsTheTag(t *testing.T) {
	r := newReaddRig()
	r.landing = "c"
	_, err := r.run(t, r.input())
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("err %v", err)
	}
	if _, _, ok := ReaddTagOf(r.tags); !ok {
		t.Fatalf("tags %v", r.tags)
	}
	r.landing = "a"
	res, err := r.run(t, r.input())
	r.registeredAgain(t, res, err)
}

func TestReregisterAfterRemovalInAModeThatCannotWriteKeepsTheTag(t *testing.T) {
	r := newReaddRig()
	r.fail["stage"] = errCrash
	_, _ = r.run(t, r.input())
	in := r.input()
	in.Profile.Effective, in.Profile.Reason = EffectiveOff, "pve.ha.mode is off"
	_, err := r.run(t, in)
	if !errors.Is(err, ErrReaddBlocked) || r.exists {
		t.Fatalf("err %v exists %v", err, r.exists)
	}
	if _, _, ok := ReaddTagOf(r.tags); !ok || r.record == nil {
		t.Fatalf("tags %v record %v", r.tags, r.record)
	}
	res, err := r.run(t, r.input())
	r.registeredAgain(t, res, err)
}

func TestReregisterAfterRemovalWithoutARecordFailsClosed(t *testing.T) {
	for name, recErr := range map[string]error{"missing": nil, "unreadable": errors.New("bad json")} {
		t.Run(name, func(t *testing.T) {
			r := newReaddRig()
			r.exists, r.listed = false, false
			r.tags = []string{ReaddTag(time.Unix(1000, 0))}
			r.recordErr = recErr
			_, err := r.run(t, r.input())
			if !errors.Is(err, ErrReaddBlocked) || r.exists {
				t.Fatalf("err %v exists %v", err, r.exists)
			}
			if !strings.Contains(err.Error(), "ha-manager add vm:101 --state started") {
				t.Fatalf("err %v", err)
			}
		})
	}
}

func TestReregisterAfterRemovalWhenTheDecisionDropsHA(t *testing.T) {
	r := newReaddRig()
	r.fail["stage"] = errCrash
	_, _ = r.run(t, r.input())
	r.decide = func(d *VMDecision) { d.Outcome, d.Reason = OutcomeSkip, "local disk" }
	res, err := r.run(t, r.input())
	if err != nil || res.Outcome != ReaddWithoutHA || r.exists {
		t.Fatalf("res %+v err %v exists %v", res, err, r.exists)
	}
	if slices.ContainsFunc(r.rules, func(x Rule) bool { return x.ID == "bosh-na-101" }) {
		t.Fatalf("rules %+v", r.rules)
	}
}

func TestReregisterAfterRemovalOutsideTheSetKeepsTheTag(t *testing.T) {
	r := newReaddRig()
	r.fail["stage"] = errCrash
	_, _ = r.run(t, r.input())
	r.decide = func(d *VMDecision) { d.Admissible, d.Hard = []string{"b"}, []string{"b"} }
	_, err := r.run(t, r.input())
	if !errors.Is(err, ErrReaddBlocked) || r.exists {
		t.Fatalf("err %v exists %v", err, r.exists)
	}
	if _, _, ok := ReaddTagOf(r.tags); !ok {
		t.Fatalf("tags %v", r.tags)
	}
}

func TestReregisterNothingToDo(t *testing.T) {
	healthy := newReaddRig()
	healthy.raw = haStateStarted
	res, err := healthy.run(t, healthy.input())
	if err != nil || res.Outcome != ReaddNothing || len(healthy.events) != 0 {
		t.Fatalf("res %+v err %v events %v", res, err, healthy.events)
	}
	gone := newReaddRig()
	gone.exists, gone.listed = false, false
	res, err = gone.run(t, gone.input())
	if err != nil || res.Outcome != ReaddNothing || len(gone.events) != 0 {
		t.Fatalf("res %+v err %v events %v", res, err, gone.events)
	}
}

func TestReregisterWaitsForAPendingBarrier(t *testing.T) {
	r := newReaddRig()
	r.raw, r.requested = haStateStarted, haStateIgnored
	r.comment = withNarrowMark(strPtr(readdMarker), haStateStarted)
	r.tags = []string{ReaddTag(time.Unix(1000, 0))}
	res, err := r.run(t, r.input())
	if err != nil || res.Outcome != ReaddNothing || len(r.events) != 0 {
		t.Fatalf("res %+v err %v events %v", res, err, r.events)
	}
}

func TestReregisterForeignResource(t *testing.T) {
	untagged := newReaddRig()
	untagged.comment = "operator"
	_, err := untagged.run(t, untagged.input())
	if !errors.Is(err, ErrReaddBlocked) || !untagged.exists || len(untagged.events) != 0 {
		t.Fatalf("err %v events %v", err, untagged.events)
	}
	tagged := newReaddRig()
	tagged.comment, tagged.raw = "operator", haStateStarted
	tagged.tags = []string{ReaddTag(time.Unix(1000, 0))}
	res, err := tagged.run(t, tagged.input())
	if err != nil || res.Outcome != ReaddForeign || !slices.Equal(tagged.events, []string{"untag:foreign"}) {
		t.Fatalf("res %+v err %v events %v", res, err, tagged.events)
	}
}

func TestReregisterResumeActiveWithAnotherRequestedState(t *testing.T) {
	r := newReaddRig()
	r.raw, r.requested = haStateStopped, haStateStopped
	r.tags = []string{ReaddTag(time.Unix(1000, 0))}
	res, err := r.run(t, r.input())
	if err != nil || res.Outcome != ReaddRegistered || !slices.Equal(r.events, []string{"untag:registered"}) {
		t.Fatalf("res %+v err %v events %v", res, err, r.events)
	}
}

func TestReregisterResumeActiveRefusals(t *testing.T) {
	setup := func() *readdRig {
		r := newReaddRig()
		r.raw = haStateStarted
		r.tags = []string{ReaddTag(time.Unix(1000, 0))}
		return r
	}
	noHA := setup()
	noHA.decide = func(d *VMDecision) { d.Outcome, d.Reason = OutcomeSkip, "local disk" }
	if _, err := noHA.run(t, noHA.input()); !errors.Is(err, ErrReaddBlocked) {
		t.Fatalf("err %v", err)
	}
	readOnly := setup()
	in := readOnly.input()
	in.Profile.Effective = EffectiveOff
	if _, err := readOnly.run(t, in); !errors.Is(err, ErrReaddBlocked) {
		t.Fatalf("err %v", err)
	}
	outside := setup()
	outside.landing = "c"
	if _, err := outside.run(t, outside.input()); !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("err %v", err)
	}
	for name, outcome := range map[string]pve.HAStateOutcome{
		"needs reregistration": pve.HAStateNeedsReregistration, "other": pve.HAStateOutcome("timeout"),
	} {
		r := setup()
		in := r.input()
		in.Stager.SetStarted = func(context.Context, int, string) (*pve.SetHAStateResult, error) {
			return &pve.SetHAStateResult{Outcome: outcome, SID: "vm:101", VMID: 101, Node: "a"}, nil
		}
		if _, err := r.run(t, in); !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
			t.Fatalf("%s: err %v", name, err)
		}
	}
	nilResult := setup()
	in = nilResult.input()
	in.Stager.SetStarted = func(context.Context, int, string) (*pve.SetHAStateResult, error) { return nil, nil }
	if _, err := nilResult.run(t, in); !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("err %v", err)
	}
}

func TestReregisterStopAfterRemoval(t *testing.T) {
	r := newReaddRig()
	r.decide = func(d *VMDecision) { d.Outcome, d.Reason = OutcomeSkip, "local disk" }
	in := r.input()
	in.StopAfterRemoval = true
	res, err := r.run(t, in)
	if err != nil || res.Outcome != ReaddRemoved || r.exists {
		t.Fatalf("res %+v err %v exists %v", res, err, r.exists)
	}
	if !slices.Equal(r.events, []string{"tag", "remove", "untag:removed"}) {
		t.Fatalf("events %v", r.events)
	}
}

func TestReregisterBetweenRunsOnlyInTheRemovingCall(t *testing.T) {
	r := newReaddRig()
	in := r.input()
	in.Between = func(context.Context, string) (pve.VMLocation, error) {
		r.events = append(r.events, "between")
		return pve.VMLocation{Node: "a", Found: true}, nil
	}
	res, err := r.run(t, in)
	r.registeredAgain(t, res, err)
	if !slices.Equal(r.events[:3], []string{"tag", "remove", "between"}) {
		t.Fatalf("events %v", r.events)
	}
}

func TestReregisterBetweenFailureLeavesTheTag(t *testing.T) {
	r := newReaddRig()
	in := r.input()
	in.Between = func(context.Context, string) (pve.VMLocation, error) {
		return pve.VMLocation{}, cpierrors.Retriable("the attach failed")
	}
	if _, err := r.run(t, in); err == nil || r.exists {
		t.Fatalf("err %v exists %v", err, r.exists)
	}
	if _, _, ok := ReaddTagOf(r.tags); !ok {
		t.Fatalf("tags %v", r.tags)
	}
	res, err := r.run(t, r.input())
	r.registeredAgain(t, res, err)
}

func TestReregisterDecisionFailure(t *testing.T) {
	r := newReaddRig()
	r.decide = func(d *VMDecision) { d.Outcome, d.Err = OutcomeFail, cpierrors.Cloud("legacy disk") }
	if _, err := r.run(t, r.input()); err == nil || r.exists == false {
		t.Fatalf("err %v exists %v", err, r.exists)
	}
	r.decide = func(d *VMDecision) { d.Outcome, d.Reason = OutcomeFail, "no reason" }
	if _, err := r.run(t, r.input()); err == nil {
		t.Fatal("a failing decision with no error fails too")
	}
	r.fail["decide"] = errCrash
	if _, err := r.run(t, r.input()); !errors.Is(err, errCrash) {
		t.Fatalf("err %v", err)
	}
}

func TestReregisterSnapshotUsesTheRecordedPinWhenTheLiveOneIsGone(t *testing.T) {
	r := newReaddRig()
	r.rules = nil
	r.record = &ReaddRecord{Comment: readdMarker, PinNodes: []string{"a"}, PinStrict: true}
	r.tags = []string{ReaddTag(time.Unix(1000, 0))}
	var got ReaddRecord
	in := r.input()
	write := in.WriteIntent
	in.WriteIntent = func(ctx context.Context, node, tag string, rec ReaddRecord) error {
		got = rec
		return write(ctx, node, tag, rec)
	}
	res, err := r.run(t, in)
	r.registeredAgain(t, res, err)
	if !got.PinStrict || !slices.Equal(got.PinNodes, []string{"a"}) || got.Comment != readdMarker {
		t.Fatalf("record %+v", got)
	}
}

func TestReregisterSnapshotStripsTheNarrowMark(t *testing.T) {
	r := newReaddRig()
	r.comment = withNarrowMark(strPtr(readdMarker), haStateStarted)
	r.rebalance = true
	var got ReaddRecord
	in := r.input()
	write := in.WriteIntent
	in.WriteIntent = func(ctx context.Context, node, tag string, rec ReaddRecord) error {
		got = rec
		return write(ctx, node, tag, rec)
	}
	res, err := r.run(t, in)
	r.registeredAgain(t, res, err)
	if got.Comment != readdMarker || !got.AutoRebalance || !got.PinStrict {
		t.Fatalf("record %+v", got)
	}
}

func TestReregisterRefusesInvalidInput(t *testing.T) {
	r := newReaddRig()
	for name, edit := range map[string]func(in *ReregisterInput){
		"vmid":    func(in *ReregisterInput) { in.VMID = 0 },
		"node":    func(in *ReregisterInput) { in.Node = " " },
		"profile": func(in *ReregisterInput) { in.Profile = nil },
		"stager":  func(in *ReregisterInput) { in.Stager = nil },
		"power":   func(in *ReregisterInput) { in.Power = nil },
		"pin":     func(in *ReregisterInput) { in.PinID = "" },
	} {
		in := r.input()
		edit(&in)
		if _, err := r.run(t, in); err == nil || cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
			t.Fatalf("%s: err %v", name, err)
		}
	}
	deps := r.deps()
	deps.ReadState = nil
	if _, err := ReregisterErrored(context.Background(), deps, r.input()); err == nil {
		t.Fatal("a missing state read is refused")
	}
}

func TestReaddTagCodec(t *testing.T) {
	at := time.Unix(1700000000, 0)
	tag := ReaddTag(at)
	if tag != "bosh-ha-readd-1700000000" {
		t.Fatalf("tag %q", tag)
	}
	if got := ReaddTag(time.Time{}); got != "bosh-ha-readd-0" {
		t.Fatalf("zero time tag %q", got)
	}
	if got, ok := ReaddTagTime(" BOSH-HA-READD-1700000000 "); !ok || !got.Equal(at) {
		t.Fatalf("time %v %v", got, ok)
	}
	for _, bad := range []string{"bosh-ha-readd-", "bosh-ha-readd-x", "bosh-ha-readd-1-2", "bosh-ha-managed",
		"bosh-ha-readd-99999999999999999999"} {
		if _, ok := ReaddTagTime(bad); ok {
			t.Fatalf("%q parsed", bad)
		}
	}
	tags := []string{"x", ReaddTag(time.Unix(200, 0)), ReaddTag(time.Unix(100, 0))}
	if got, when, ok := ReaddTagOf(tags); !ok || got != ReaddTag(time.Unix(100, 0)) || when.Unix() != 100 {
		t.Fatalf("oldest %q %v %v", got, when, ok)
	}
	if _, _, ok := ReaddTagOf([]string{"x"}); ok {
		t.Fatal("no tag")
	}
	if got := WithoutReaddTags(tags); !slices.Equal(got, []string{"x"}) {
		t.Fatalf("without %v", got)
	}
}

func TestReaddRecordCodec(t *testing.T) {
	desc, err := pve.RenderSentinel("operator text", nil)
	if err != nil {
		t.Fatal(err)
	}
	if rec, err := ReaddRecordOf(desc); rec != nil || err != nil {
		t.Fatalf("absent: %+v %v", rec, err)
	}
	want := &ReaddRecord{Comment: readdMarker, AutoRebalance: true, PinNodes: []string{"a", "b"}, PinStrict: true}
	with, err := WithReaddRecord(desc, want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ReaddRecordOf(with)
	if err != nil || got == nil || got.Comment != want.Comment || !got.AutoRebalance || !got.PinStrict ||
		!slices.Equal(got.PinNodes, want.PinNodes) {
		t.Fatalf("round trip %+v %v", got, err)
	}
	nonBOSH, _ := pve.ParseSentinel(with)
	if nonBOSH != "operator text" {
		t.Fatalf("operator text %q", nonBOSH)
	}
	without, err := WithReaddRecord(with, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rec, err := ReaddRecordOf(without); rec != nil || err != nil {
		t.Fatalf("removed: %+v %v", rec, err)
	}
	if same, err := WithReaddRecord(desc, nil); err != nil || same != desc {
		t.Fatalf("no record to remove: %q %v", same, err)
	}
	empty, err := WithReaddRecord(desc, &ReaddRecord{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ReaddRecordOf(empty); err == nil {
		t.Fatal("a record with no comment is refused")
	}
	_, raw := pve.ParseSentinel(desc)
	raw[ReaddRecordKey] = []byte(`"not an object"`)
	bad, err := pve.RenderSentinel("", raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ReaddRecordOf(bad); err == nil {
		t.Fatal("an undecodable record is refused")
	}
}

func TestNarrowHandsAnErroredResourceToTheReregistration(t *testing.T) {
	r := &barrierRig{state: "started", settleOn: []string{"a"}, states: []*pve.HAServiceState{
		crm("error", 100, 90, 95),
	}}
	in := r.input(&pinAB, &pinA)
	var landed []string
	in.Reregister = func(ctx context.Context, st *pve.HAServiceState,
		between func(context.Context, string) (pve.VMLocation, error)) (pve.VMLocation, error) {
		if st.Raw != "error" {
			t.Fatalf("state %+v", st)
		}
		for _, node := range []string{"b", "a"} {
			loc, err := between(ctx, node)
			if err == nil {
				landed = append(landed, loc.Node)
			}
		}
		return pve.VMLocation{Node: "a", Found: true}, nil
	}
	loc, err := NarrowActive(context.Background(), r.deps(), in)
	if err != nil || loc.Node != "a" {
		t.Fatalf("loc %+v err %v", loc, err)
	}
	// b lies outside the narrower pin, so only the change on a lands, and the
	// barrier never requests a state.
	want := []string{"mutate:b", "reach:b", "mutate:a", "reach:a"}
	if !slices.Equal(r.events, want) || !slices.Equal(landed, []string{"a"}) {
		t.Fatalf("events %v landed %v", r.events, landed)
	}

	plain := &barrierRig{state: "started", settleOn: []string{"a"}, states: []*pve.HAServiceState{
		crm("error", 100, 90, 95),
	}}
	_, err = NarrowActive(context.Background(), plain.deps(), plain.input(&pinAB, &pinA))
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) || len(plain.events) != 0 {
		t.Fatalf("err %v events %v", err, plain.events)
	}
}

func TestNarrowHandsAMutationFailureBackFromTheReregistration(t *testing.T) {
	r := &barrierRig{state: "started", settleOn: []string{"a"}, mutErr: errCrash, states: []*pve.HAServiceState{
		crm("error", 100, 90, 95),
	}}
	in := r.input(&pinAB, &pinA)
	in.Reregister = func(ctx context.Context, _ *pve.HAServiceState,
		between func(context.Context, string) (pve.VMLocation, error)) (pve.VMLocation, error) {
		return between(ctx, "a")
	}
	if _, err := NarrowActive(context.Background(), r.deps(), in); !errors.Is(err, errCrash) {
		t.Fatalf("err %v", err)
	}
}
