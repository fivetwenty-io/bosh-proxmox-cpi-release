package ha

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdkclient "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/client"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

const stageMarker = "bosh-cpi:v1 owner=us deployment=cf group=router"

// stageRig is one HA resource and the calls made on it, in order.
type stageRig struct {
	events    []string
	exists    bool
	state     string
	comment   string
	failback  *sdkclient.PVEBool
	rebalance *sdkclient.PVEBool

	createErr error
	// createLands makes a failed create land anyway, as a lost answer does.
	createLands bool
	updateErr   error
	updateLands bool
	getErr      error
	deleteErr   error
}

func pveBool(b bool) *sdkclient.PVEBool {
	v := sdkclient.PVEBool(b)
	return &v
}

func (r *stageRig) GetHaResources(_ context.Context, sid string) (*sdkcluster.GetHaResourcesResponse, error) {
	r.events = append(r.events, "get")
	if r.getErr != nil {
		return nil, r.getErr
	}
	if !r.exists {
		return nil, &sdkerrors.APIError{HTTPCode: 404, Message: "no such resource"}
	}
	st, c := r.state, r.comment
	return &sdkcluster.GetHaResourcesResponse{Sid: sid, State: &st, Comment: &c, Failback: r.failback,
		AutoRebalance: r.rebalance}, nil
}

func (r *stageRig) CreateHaResources(_ context.Context, p *sdkcluster.CreateHaResourcesParams) error {
	r.events = append(r.events, "create:"+*p.State)
	if r.createErr != nil && !r.createLands {
		return r.createErr
	}
	if r.exists && r.createErr == nil {
		return errors.New("resource 'vm:101' already defined")
	}
	r.exists, r.state, r.comment = true, *p.State, *p.Comment
	r.failback, r.rebalance = pveBool(*p.Failback), pveBool(*p.AutoRebalance)
	return r.createErr
}

func (r *stageRig) UpdateHaResources(_ context.Context, _ string, p *sdkcluster.UpdateHaResourcesParams) error {
	state := "-"
	if p.State != nil {
		state = *p.State
	}
	r.events = append(r.events, "update:"+state)
	if r.updateErr != nil && !r.updateLands {
		return r.updateErr
	}
	if p.State != nil {
		r.state = *p.State
	}
	if p.Comment != nil {
		r.comment = *p.Comment
	}
	if p.Failback != nil {
		r.failback = pveBool(*p.Failback)
	}
	if p.AutoRebalance != nil {
		r.rebalance = pveBool(*p.AutoRebalance)
	}
	return r.updateErr
}

func (r *stageRig) DeleteHaResources(_ context.Context, _ string, p *sdkcluster.DeleteHaResourcesParams) error {
	purge := p != nil && p.Purge != nil && *p.Purge
	r.events = append(r.events, "delete:purge="+map[bool]string{true: "1", false: "0"}[purge])
	if r.deleteErr != nil {
		return r.deleteErr
	}
	r.exists = false
	return nil
}

func ownedMarker(comment string) bool { return strings.Contains(comment, "owner=us") }

func stageDecision() *VMDecision {
	return &VMDecision{VMID: 101, SID: "vm:101", Node: "a", Outcome: OutcomeRegister, Policy: PolicyShared,
		Admissible: []string{"a", "b"}, Hard: []string{"a", "b"}, Placed: "a"}
}

func (r *stageRig) stager() *Stager { return &Stager{Resources: r, Owned: ownedMarker} }

func TestStageRegistersAnIgnoredResource(t *testing.T) {
	t.Parallel()
	for _, dlb := range []bool{false, true} {
		rig := &stageRig{}
		d := stageDecision()
		d.DLB = dlb
		if err := rig.stager().Stage(context.Background(), d, stageMarker); err != nil {
			t.Fatal(err)
		}
		if !d.StagedIgnored || rig.state != haStateIgnored || rig.comment != stageMarker ||
			rig.failback.Bool() || rig.rebalance.Bool() != dlb {
			t.Fatalf("dlb=%v: staged %+v; decision %+v", dlb, rig, d)
		}
		if !slices.Equal(rig.events, []string{"create:ignored"}) {
			t.Fatalf("one create and nothing else: %v", rig.events)
		}
	}
}

func TestStageAdoptsOurOwnIgnoredResource(t *testing.T) {
	t.Parallel()
	rig := &stageRig{exists: true, state: haStateIgnored, comment: "bosh-cpi:v1 owner=us deployment=- group=-",
		failback: pveBool(true)}
	d := stageDecision()
	if err := rig.stager().Stage(context.Background(), d, stageMarker); err != nil {
		t.Fatal(err)
	}
	if !d.StagedIgnored || rig.comment != stageMarker || rig.failback.Bool() {
		t.Fatalf("the comment and flags are rewritten: %+v", rig)
	}
	if !slices.Equal(rig.events, []string{"create:ignored", "get", "update:-"}) {
		t.Fatalf("events %v", rig.events)
	}

	same := &stageRig{exists: true, state: haStateIgnored, comment: stageMarker, failback: pveBool(false)}
	if err := same.stager().Stage(context.Background(), stageDecision(), stageMarker); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(same.events, []string{"create:ignored", "get"}) {
		t.Fatalf("a resource that already matches needs no update: %v", same.events)
	}
}

func TestStageCountsALostCreateAnswerThatLanded(t *testing.T) {
	t.Parallel()
	rig := &stageRig{createErr: errors.New("500 proxy loop timed out (HTTP500)"), createLands: true}
	d := stageDecision()
	if err := rig.stager().Stage(context.Background(), d, stageMarker); err != nil {
		t.Fatalf("a create that landed is staged: %v", err)
	}
	if !d.StagedIgnored {
		t.Fatal("the decision must be marked staged")
	}
}

func TestStageRefusals(t *testing.T) {
	t.Parallel()
	held := errors.New("500 proxy loop timed out (HTTP500)")
	cases := []struct {
		name string
		rig  *stageRig
		d    *VMDecision
		mark string
		want string
	}{
		{"decision registers nothing", &stageRig{}, &VMDecision{VMID: 101, SID: "vm:101", Outcome: OutcomeSkip},
			stageMarker, "does not register"},
		{"foreign marker", &stageRig{}, stageDecision(), "owner=them", "ownership marker"},
		{"foreign resource", &stageRig{exists: true, state: haStateIgnored, comment: "owner=them"}, stageDecision(),
			stageMarker, "does not carry this Director's ownership marker"},
		{"resource the CRM manages", &stageRig{exists: true, state: haStateStarted, comment: stageMarker},
			stageDecision(), stageMarker, "requests started rather than ignored"},
		{"held create and a failed read", &stageRig{createErr: held, getErr: held}, stageDecision(), stageMarker,
			"register vm:101 as ignored"},
		{"held create that never landed", &stageRig{createErr: held}, stageDecision(), stageMarker,
			"register vm:101 as ignored"},
		{"update of an adopted resource fails", &stageRig{exists: true, state: haStateIgnored,
			comment: "owner=us old", updateErr: held}, stageDecision(), stageMarker, "set the staged flags"},
	}
	for _, tc := range cases {
		err := tc.rig.stager().Stage(context.Background(), tc.d, tc.mark)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want %q", tc.name, err, tc.want)
			continue
		}
		if tc.d.StagedIgnored {
			t.Errorf("%s: a refused stage must not mark the decision staged", tc.name)
		}
	}
	if err := (&Stager{}).Stage(context.Background(), stageDecision(), stageMarker); err == nil {
		t.Error("a stager without a client must refuse")
	}
	foreign := &stageRig{exists: true, state: haStateIgnored, comment: "owner=them"}
	_ = foreign.stager().Stage(context.Background(), stageDecision(), stageMarker)
	if foreign.comment != "owner=them" || slices.Contains(foreign.events, "update:-") {
		t.Fatalf("a foreign resource is left alone: %+v", foreign)
	}
}

// verifyPin is the strict pin the stage verifies for VM 101.
func verifyPin() *Rule {
	return &Rule{ID: "bosh-na-101", Type: RuleTypeNodeAffinity, Affinity: AffinityPositive, Strict: true,
		Nodes: map[string]int{"a": 2, "b": 0}, Resources: []string{"vm:101"}, Comment: stageMarker}
}

func verifyAA() Rule {
	return Rule{ID: "bosh-aa-x", Type: RuleTypeResourceAffinity, Affinity: AffinityNegative,
		Resources: []string{"vm:101", "vm:102"}, Comment: stageMarker}
}

func readRules(rules ...Rule) func(context.Context) (*RuleSet, error) {
	return func(context.Context) (*RuleSet, error) { return &RuleSet{Rules: rules, Digest: "d"}, nil }
}

func TestVerifyAcceptsEveryFormOfThePin(t *testing.T) {
	t.Parallel()
	want := verifyPin()
	for name, got := range map[string]*Rule{
		"as written": want, "flattened": Flattened(want), "confinement only": ConfinementOnly(want),
	} {
		s := &Stager{ReadRules: readRules(*got, verifyAA())}
		if err := s.Verify(context.Background(), stageDecision(), want, "bosh-aa-x"); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestVerifyRefusesAConfinementNotInEffect(t *testing.T) {
	t.Parallel()
	want := verifyPin()
	off := want.Clone()
	off.Disabled = true
	broken := want.Clone()
	broken.Errors = []string{"resources: conflict"}
	wider := want.Clone()
	wider.Nodes = map[string]int{"a": 0, "b": 0, "c": 0}
	aaOff := verifyAA()
	aaOff.Disabled = true
	aaBroken := verifyAA()
	aaBroken.Errors = []string{"resources: infeasible"}
	aaWithout := verifyAA()
	aaWithout.Resources = []string{"vm:102", "vm:103"}
	aaPositive := verifyAA()
	aaPositive.Affinity = AffinityPositive
	opPin := Rule{ID: "op", Type: RuleTypeNodeAffinity, Strict: true, Nodes: map[string]int{"a": 0},
		Resources: []string{"vm:101"}, Disabled: true}
	cases := []struct {
		name  string
		rules []Rule
		want  string
	}{
		{"pin missing", []Rule{verifyAA()}, "pin \"bosh-na-101\" is missing"},
		{"pin disabled", []Rule{off, verifyAA()}, "is disabled"},
		{"pin in error", []Rule{broken, verifyAA()}, "has errors"},
		{"pin differs", []Rule{wider, verifyAA()}, "differs in"},
		{"anti-affinity missing", []Rule{*want}, "anti-affinity rule \"bosh-aa-x\" is missing"},
		{"anti-affinity disabled", []Rule{*want, aaOff}, "anti-affinity rule \"bosh-aa-x\" is disabled"},
		{"anti-affinity in error", []Rule{*want, aaBroken}, "infeasible"},
		{"anti-affinity without the VM", []Rule{*want, aaWithout}, "does not hold vm:101"},
		{"anti-affinity of the wrong kind", []Rule{*want, aaPositive}, "is not an anti-affinity rule"},
		{"another confinement disabled", []Rule{*want, verifyAA(), opPin}, "confinement rule \"op\" is disabled"},
	}
	for _, managed := range []bool{false, true} {
		for _, tc := range cases {
			s := &Stager{ReadRules: readRules(tc.rules...), Managed: managed}
			err := s.Verify(context.Background(), stageDecision(), want, "bosh-aa-x")
			if !errors.Is(err, ErrConfinementUnverified) || !strings.Contains(err.Error(), tc.want) ||
				!cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
				t.Errorf("managed=%v %s: got %v, want a retriable confinement error naming %q", managed, tc.name,
					err, tc.want)
			}
		}
	}
}

func TestVerifyANonStrictPin(t *testing.T) {
	t.Parallel()
	loose := verifyPin()
	loose.Strict = false
	off := loose.Clone()
	off.Disabled = true
	auto := &Stager{ReadRules: readRules(off)}
	if err := auto.Verify(context.Background(), stageDecision(), loose, ""); err != nil {
		t.Fatalf("auto logs a non-strict pin that is not in effect: %v", err)
	}
	managed := &Stager{ReadRules: readRules(off), Managed: true}
	err := managed.Verify(context.Background(), stageDecision(), loose, "")
	if !errors.Is(err, ErrRuleNotInEffect) || errors.Is(err, ErrConfinementUnverified) {
		t.Fatalf("managed fails a non-strict pin not in effect: %v", err)
	}
	ok := &Stager{ReadRules: readRules(*loose), Managed: true}
	if err := ok.Verify(context.Background(), stageDecision(), loose, ""); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyInputs(t *testing.T) {
	t.Parallel()
	if err := (&Stager{ReadRules: readRules()}).Verify(context.Background(), stageDecision(), nil, ""); err == nil {
		t.Error("no pin must be an error")
	}
	if err := (&Stager{}).Verify(context.Background(), stageDecision(), verifyPin(), ""); err == nil {
		t.Error("no reader must be an error")
	}
	failing := &Stager{ReadRules: func(context.Context) (*RuleSet, error) { return nil, errors.New("503") }}
	err := failing.Verify(context.Background(), stageDecision(), verifyPin(), "")
	if err == nil || !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Errorf("a failed read is retriable: %v", err)
	}
	empty := &Stager{ReadRules: func(context.Context) (*RuleSet, error) { return nil, nil }}
	if err := empty.Verify(context.Background(), stageDecision(), verifyPin(), ""); err == nil {
		t.Error("a read that returns no rule set must be an error")
	}
}

func TestCheckWatchdog(t *testing.T) {
	t.Parallel()
	read := func(state pve.WatchdogState, err error) func(context.Context, string) (pve.WatchdogState, error) {
		return func(context.Context, string) (pve.WatchdogState, error) { return state, err }
	}
	cases := []struct {
		name    string
		s       *Stager
		refused bool
	}{
		{"quorum not fragile", &Stager{Watchdog: read(pve.WatchdogIdle, nil)}, false},
		{"armed", &Stager{FragileQuorum: true, Watchdog: read(pve.WatchdogArmed, nil)}, false},
		{"idle in auto", &Stager{FragileQuorum: true, Watchdog: read(pve.WatchdogIdle, nil)}, true},
		{"unknown in auto", &Stager{FragileQuorum: true, Watchdog: read(pve.WatchdogUnknown, nil)}, true},
		{"read fails in auto", &Stager{FragileQuorum: true, Watchdog: read("", errors.New("595"))}, true},
		{"no reader in auto", &Stager{FragileQuorum: true}, true},
		{"idle in managed", &Stager{FragileQuorum: true, Managed: true, Watchdog: read(pve.WatchdogIdle, nil)},
			false},
		{"unknown in managed", &Stager{FragileQuorum: true, Managed: true,
			Watchdog: read(pve.WatchdogUnknown, nil)}, false},
	}
	for _, tc := range cases {
		err := tc.s.CheckWatchdog(context.Background(), stageDecision(), "a")
		if tc.refused != errors.Is(err, ErrWatchdogNotArmed) || !tc.refused && err != nil {
			t.Errorf("%s: got %v, refused want %v", tc.name, err, tc.refused)
		}
	}
}

func startedAt(node string, outcome pve.HAStateOutcome) func(context.Context, int, string) (*pve.SetHAStateResult, error) {
	return func(context.Context, int, string) (*pve.SetHAStateResult, error) {
		return &pve.SetHAStateResult{Outcome: outcome, SID: "vm:101", VMID: 101, Node: node}, nil
	}
}

func TestActivateRequestsStartedAndChecksTheLanding(t *testing.T) {
	t.Parallel()
	rig := &stageRig{exists: true, state: haStateIgnored, comment: stageMarker}
	d := stageDecision()
	d.MarkStaged()
	s := rig.stager()
	s.SetStarted = func(context.Context, int, string) (*pve.SetHAStateResult, error) {
		if d.StagedIgnored || rig.state != haStateStarted {
			t.Error("the decision is marked activated and started requested before the wait")
		}
		return &pve.SetHAStateResult{Outcome: pve.HAStateReached, Node: "b"}, nil
	}
	got, err := s.Activate(context.Background(), d, "a")
	if err != nil || got != "b" {
		t.Fatalf("a landing inside the hard set is fine: %q %v", got, err)
	}
	if !slices.Equal(rig.events, []string{"update:started"}) {
		t.Fatalf("events %v", rig.events)
	}
}

func TestActivateFailures(t *testing.T) {
	t.Parallel()
	held := errors.New("500 proxy loop timed out (HTTP500)")
	cases := []struct {
		name   string
		rig    *stageRig
		set    func(context.Context, int, string) (*pve.SetHAStateResult, error)
		want   string
		landed string
	}{
		{"landing outside the hard set", &stageRig{exists: true, state: haStateIgnored},
			startedAt("c", pve.HAStateReached), "outside its hard admissible set", "c"},
		{"error state while running", &stageRig{exists: true, state: haStateIgnored},
			startedAt("a", pve.HAStateNeedsReregistration), "ha-manager remove vm:101", ""},
		{"not managed", &stageRig{exists: true, state: haStateIgnored}, startedAt("a", pve.HAStateNotManaged),
			"did not reach started", ""},
		{"no result", &stageRig{exists: true, state: haStateIgnored},
			func(context.Context, int, string) (*pve.SetHAStateResult, error) { return nil, nil }, "no result", ""},
		{"wait fails", &stageRig{exists: true, state: haStateIgnored},
			func(context.Context, int, string) (*pve.SetHAStateResult, error) {
				return nil, cpierrors.Retriable("motion wait expired")
			}, "motion wait expired", ""},
		{"held request that did not land", &stageRig{exists: true, state: haStateIgnored, updateErr: held},
			startedAt("a", pve.HAStateReached), "request started for vm:101", ""},
	}
	for _, tc := range cases {
		s := tc.rig.stager()
		s.SetStarted = tc.set
		d := stageDecision()
		d.MarkStaged()
		landed, err := s.Activate(context.Background(), d, "a")
		if err == nil || !strings.Contains(err.Error(), tc.want) || landed != tc.landed {
			t.Errorf("%s: got %q %v, want %q", tc.name, landed, err, tc.want)
		}
		if d.StagedIgnored {
			t.Errorf("%s: the decision stays activated, so a repair never removes it directly", tc.name)
		}
	}

	lost := &stageRig{exists: true, state: haStateIgnored, updateErr: held, updateLands: true}
	s := lost.stager()
	s.SetStarted = startedAt("a", pve.HAStateReached)
	if _, err := s.Activate(context.Background(), stageDecision(), "a"); err != nil {
		t.Fatalf("a held request that landed goes on: %v", err)
	}
	if _, err := (&Stager{}).Activate(context.Background(), stageDecision(), "a"); err == nil {
		t.Fatal("a stager without a client must refuse")
	}
	skip := &VMDecision{VMID: 101, SID: "vm:101", Outcome: OutcomeSkip}
	if _, err := s.Activate(context.Background(), skip, "a"); err == nil {
		t.Fatal("a decision that registers nothing must refuse")
	}
}

func TestUnstage(t *testing.T) {
	t.Parallel()
	rig := &stageRig{exists: true, state: haStateIgnored, comment: stageMarker}
	d := stageDecision()
	d.MarkStaged()
	if err := rig.stager().Unstage(context.Background(), d, "watchdog idle"); err != nil {
		t.Fatal(err)
	}
	if rig.exists || !slices.Equal(rig.events, []string{"get", "delete:purge=1"}) {
		t.Fatalf("the staged resource is purged: %+v", rig)
	}
	if d.StagedIgnored || d.Registers() || d.Policy != PolicyNone || d.Reason != "watchdog idle" {
		t.Fatalf("the decision registers nothing now: %+v", d)
	}

	gone := &stageRig{}
	d = stageDecision()
	d.MarkStaged()
	if err := gone.stager().Unstage(context.Background(), d, "x"); err != nil || d.Registers() {
		t.Fatalf("a resource already gone counts as removed: %v", err)
	}

	cases := []struct {
		name   string
		rig    *stageRig
		staged bool
		want   string
	}{
		{"never staged", &stageRig{exists: true, state: haStateIgnored, comment: stageMarker}, false,
			"was not staged"},
		{"now started", &stageRig{exists: true, state: haStateStarted, comment: stageMarker}, true,
			"requests started"},
		{"foreign", &stageRig{exists: true, state: haStateIgnored, comment: "owner=them"}, true, "ownership marker"},
		{"read fails", &stageRig{getErr: errors.New("503")}, true, "read vm:101 before its removal"},
		{"delete fails", &stageRig{exists: true, state: haStateIgnored, comment: stageMarker,
			deleteErr: errors.New("500")}, true, "remove the staged vm:101"},
	}
	for _, tc := range cases {
		d := stageDecision()
		d.StagedIgnored = tc.staged
		err := tc.rig.stager().Unstage(context.Background(), d, "x")
		if err == nil || !strings.Contains(err.Error(), tc.want) || !d.Registers() {
			t.Errorf("%s: got %v, want %q and the decision kept", tc.name, err, tc.want)
		}
	}
	if err := (&Stager{}).Unstage(context.Background(), stageDecision(), "x"); err == nil {
		t.Error("a stager without a client must refuse")
	}
}

func TestVolumeRecordHelpers(t *testing.T) {
	t.Parallel()
	vols := []VMVolume{{Volume: "nfs:101/a.raw"}, {Volume: "ceph:vm-101-disk-0"}, {Volume: "nfs:101/b.raw"},
		{Volume: "none"}, {Volume: ":x"}}
	if got := VolumeStorages(vols); !slices.Equal(got, []string{"ceph", "nfs"}) {
		t.Fatalf("VolumeStorages = %v", got)
	}
	c := WithVolumeRecord(stageMarker, []string{"nfs", "ceph"})
	if c != stageMarker+" bosh-vols=ceph,nfs" {
		t.Fatalf("WithVolumeRecord = %q", c)
	}
	if again := WithVolumeRecord(c, []string{"local-lvm"}); again != stageMarker+" bosh-vols=local-lvm" {
		t.Fatalf("a second record replaces the first: %q", again)
	}
	if dropped := WithVolumeRecord(c, nil); dropped != stageMarker {
		t.Fatalf("an empty list drops the record: %q", dropped)
	}
	if got, ok := VolumeRecord(c + " dead-node-from=a"); !ok || !slices.Equal(got, []string{"ceph", "nfs"}) {
		t.Fatalf("VolumeRecord = %v %v", got, ok)
	}
	for _, bad := range []string{"", stageMarker, stageMarker + " bosh-vols=", stageMarker + " bosh-vols=,"} {
		if _, ok := VolumeRecord(bad); ok {
			t.Fatalf("%q carries no record", bad)
		}
	}
}

func TestDecisionPin(t *testing.T) {
	t.Parallel()
	configured := []string{"a", "b", "c"}
	strict, err := DecisionPin(stageDecision(), "bosh-na-101", stageMarker, []string{"nfs"}, configured, false, false)
	if err != nil || !strict.Strict || strict.Nodes["a"] != PinPriorityPlaced || len(strict.Nodes) != 2 {
		t.Fatalf("a hard set smaller than the cluster gives a strict pin: %+v %v", strict, err)
	}
	if got, ok := VolumeRecord(strict.Comment); !ok || !slices.Equal(got, []string{"nfs"}) {
		t.Fatalf("the pin records the volumes: %q", strict.Comment)
	}
	flat, err := DecisionPin(stageDecision(), "bosh-na-101", stageMarker, []string{"nfs"}, configured, false, true)
	if err != nil || flat.MultiplePriorities() {
		t.Fatalf("a member of a resource-affinity rule gets one priority: %+v %v", flat, err)
	}

	free := stageDecision()
	free.Hard, free.Admissible = nil, configured
	record, err := DecisionPin(free, "bosh-na-101", stageMarker, []string{"ceph"}, configured, false, false)
	if err != nil || record.Strict || len(record.Nodes) != 3 || record.MultiplePriorities() {
		t.Fatalf("a VM with no constraint gets the record pin over every node: %+v %v", record, err)
	}
	if !slices.Equal(record.Resources, []string{"vm:101"}) || !strings.HasPrefix(record.Comment, stageMarker) {
		t.Fatalf("record pin %+v", record)
	}
	rebal, err := DecisionPin(free, "bosh-na-101", stageMarker, []string{"ceph"}, configured, true, false)
	if err != nil || rebal.Strict || len(rebal.Nodes) != 1 || rebal.Nodes["a"] != PinPriorityRest {
		t.Fatalf("rebalance on start names the placed node alone: %+v %v", rebal, err)
	}

	skip := &VMDecision{VMID: 101, SID: "vm:101", Outcome: OutcomeSkip}
	if _, err := DecisionPin(skip, "p", stageMarker, []string{"ceph"}, configured, false, false); err == nil {
		t.Fatal("a decision that registers nothing has no pin")
	}
	if _, err := DecisionPin(stageDecision(), "p", stageMarker, nil, configured, false, false); err == nil {
		t.Fatal("a pin without a volume record must refuse")
	}
	none := stageDecision()
	none.Hard, none.Admissible = nil, nil
	if _, err := DecisionPin(none, "p", stageMarker, []string{"ceph"}, configured, false, false); err == nil {
		t.Fatal("a decision with no admissible node must refuse")
	}
	empty := stageDecision()
	empty.Hard = []string{"z"}
	if _, err := DecisionPin(empty, "p", stageMarker, []string{"ceph"}, configured, false, false); err == nil {
		t.Fatal("a hard set outside the cluster must refuse")
	}
	if RecordPin("p", "vm:101", "", nil) != nil {
		t.Fatal("a record pin with no node is nil")
	}
}

func TestIsResourceNotFound(t *testing.T) {
	t.Parallel()
	for _, err := range []error{&sdkerrors.APIError{HTTPCode: 404}, errors.New("no such resource"),
		errors.New("resource 'vm:1' does not exist"), errors.New("service 'vm:1' not defined")} {
		if !isResourceNotFound(err) {
			t.Errorf("%v reads as not found", err)
		}
	}
	if isResourceNotFound(nil) || isResourceNotFound(errors.New("500")) {
		t.Error("nil and a server error are not not-found")
	}
}
