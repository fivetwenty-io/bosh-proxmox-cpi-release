package ha

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

const ourMarker = "bosh-owner=us"

func newTestWriter(api *fakeRuleAPI, nodes []string) (*RuleWriter, *int) {
	sleeps := 0
	return &RuleWriter{
		API:          api,
		Owned:        func(r *Rule) bool { return r.Comment == ourMarker },
		ClusterNodes: nodes,
		Sleep: func(context.Context, time.Duration) error {
			sleeps++
			return nil
		},
	}, &sleeps
}

// ensure plans the one rule want: create it when missing, update it when it
// differs, and nothing once the read holds it.
func ensure(want Rule) Planner {
	return func(set *RuleSet) ([]RuleOp, error) {
		got := set.Find(want.ID)
		switch {
		case got == nil:
			return []RuleOp{{Kind: OpCreate, Rule: want}}, nil
		case !SameRule(&want, got):
			return []RuleOp{{Kind: OpUpdate, Rule: want}}, nil
		}
		return nil, nil
	}
}

func ourPin(id string, strict bool, nodes map[string]int, sid string) Rule {
	r := naRule(id, strict, nodes, sid)
	r.Comment = ourMarker
	return r
}

func ourAA(id string, sids ...string) Rule {
	r := raRule(id, AffinityNegative, sids...)
	r.Comment = ourMarker
	return r
}

func TestApplyCreatesAndStopsOnceConverged(t *testing.T) {
	api := &fakeRuleAPI{}
	w, _ := newTestWriter(api, []string{"a", "b"})
	res, err := w.Apply(context.Background(), "t", ensure(ourAA("aa", "vm:1", "vm:2")))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(api.writes, []string{"create:aa"}) || len(res.Wrote) != 1 || res.Set.Find("aa") == nil {
		t.Fatalf("writes %v result %+v", api.writes, res)
	}
	// A second Apply finds the rule in place and writes nothing.
	res, err = w.Apply(context.Background(), "t", ensure(ourAA("aa", "vm:1", "vm:2")))
	if err != nil || len(res.Wrote) != 0 || len(api.writes) != 1 {
		t.Fatalf("converged apply wrote %v, %v", api.writes, err)
	}
}

func TestApplyTakesTheLockAndPropagatesPlannerErrors(t *testing.T) {
	api := &fakeRuleAPI{}
	w, _ := newTestWriter(api, nil)
	held, released := 0, 0
	w.Lock = func(context.Context) (func(), error) {
		held++
		return func() { released++ }, nil
	}
	boom := errors.New("plan failed")
	if _, err := w.Apply(context.Background(), "t", func(*RuleSet) ([]RuleOp, error) { return nil, boom }); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if held != 1 || released != 1 {
		t.Fatalf("lock held %d released %d", held, released)
	}
	w.Lock = func(context.Context) (func(), error) { return nil, errors.New("busy") }
	if _, err := w.Apply(context.Background(), "t", ensure(ourAA("aa", "vm:1", "vm:2"))); err == nil ||
		len(api.writes) != 0 {
		t.Fatalf("a busy lock wrote %v, %v", api.writes, err)
	}
	api.listErr = map[int]error{2: errors.New("list down")}
	w.Lock = nil
	if _, err := w.Apply(context.Background(), "t", ensure(ourAA("aa", "vm:1", "vm:2"))); err == nil {
		t.Fatal("a failed read was not returned")
	}
}

func TestApplyBacksOffOnDigestConflict(t *testing.T) {
	api := &fakeRuleAPI{rules: []Rule{ourAA("aa", "vm:1", "vm:2")}}
	// Another writer changes the file between our read and our update.
	api.bumpAfter = func(n int) bool { return n == 1 }
	w, sleeps := newTestWriter(api, []string{"a", "b", "c"})
	if _, err := w.Apply(context.Background(), "t", ensure(ourAA("aa", "vm:1", "vm:2", "vm:3"))); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(api.writes, []string{"conflict:aa", "update:aa"}) || *sleeps != 1 {
		t.Fatalf("writes %v sleeps %d", api.writes, *sleeps)
	}
}

func TestApplyGivesUpAfterRepeatedConflicts(t *testing.T) {
	api := &fakeRuleAPI{rules: []Rule{ourAA("aa", "vm:1", "vm:2")}}
	api.bumpAfter = func(int) bool { return true }
	w, sleeps := newTestWriter(api, nil)
	_, err := w.Apply(context.Background(), "t", ensure(ourAA("aa", "vm:1", "vm:2", "vm:3")))
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) || *sleeps != RuleWriterMaxAttempts-1 {
		t.Fatalf("err %v sleeps %d", err, *sleeps)
	}
}

func TestApplySettlesAHeldWriteThatLanded(t *testing.T) {
	api := &fakeRuleAPI{faults: map[int]ruleFault{0: {err: errors.New("connection reset by peer"), land: true}}}
	w, _ := newTestWriter(api, nil)
	res, err := w.Apply(context.Background(), "t", ensure(ourAA("aa", "vm:1", "vm:2")))
	if err != nil || len(res.Wrote) != 1 || !slices.Equal(api.writes, []string{"create:aa"}) {
		t.Fatalf("writes %v, %v", api.writes, err)
	}
}

func TestApplyResendsAHeldWriteThatDidNotLand(t *testing.T) {
	api := &fakeRuleAPI{faults: map[int]ruleFault{0: {err: errors.New("i/o timeout")}}}
	w, _ := newTestWriter(api, nil)
	res, err := w.Apply(context.Background(), "t", ensure(ourAA("aa", "vm:1", "vm:2")))
	if err != nil || len(res.Wrote) != 1 || !slices.Equal(api.writes, []string{"create:aa", "create:aa"}) {
		t.Fatalf("writes %v, %v", api.writes, err)
	}
}

func TestApplyReplansAHeldWriteThatLandedDifferently(t *testing.T) {
	// The create answers "already defined": another writer made the rule with
	// other members, so the writer plans again against what is there.
	api := &fakeRuleAPI{}
	api.onList = func(n int, f *fakeRuleAPI) {
		if n == 2 && f.find("aa") == nil {
			f.rules = append(f.rules, ourAA("aa", "vm:1", "vm:9"))
		}
	}
	api.faults = map[int]ruleFault{0: {err: errors.New("rule 'aa' already defined")}}
	w, _ := newTestWriter(api, nil)
	res, err := w.Apply(context.Background(), "t", ensure(ourAA("aa", "vm:1", "vm:2")))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(api.writes, []string{"create:aa", "update:aa"}) ||
		!slices.Equal(res.Set.Find("aa").Resources, []string{"vm:1", "vm:2"}) {
		t.Fatalf("writes %v rule %+v", api.writes, res.Set.Find("aa"))
	}
}

func TestApplyGivesUpOnHeldWritesThatNeverLand(t *testing.T) {
	faults := map[int]ruleFault{}
	for i := range RuleWriterMaxAttempts {
		faults[i] = ruleFault{err: errors.New("i/o timeout")}
	}
	api := &fakeRuleAPI{faults: faults}
	w, _ := newTestWriter(api, nil)
	_, err := w.Apply(context.Background(), "t", ensure(ourAA("aa", "vm:1", "vm:2")))
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) || len(api.writes) != RuleWriterMaxAttempts {
		t.Fatalf("err %v writes %v", err, api.writes)
	}
	api = &fakeRuleAPI{faults: map[int]ruleFault{0: {err: errors.New("i/o timeout")}}, listErr: map[int]error{
		2: errors.New("list down")}}
	w, _ = newTestWriter(api, nil)
	if _, err := w.Apply(context.Background(), "t", ensure(ourAA("aa", "vm:1", "vm:2"))); !cpierrors.IsType(err,
		cpierrors.TypeRetriableCloud) {
		t.Fatalf("a held write without a re-read = %v", err)
	}
}

func TestApplyRefusesAnInfeasibleWriteBeforeSending(t *testing.T) {
	api := &fakeRuleAPI{}
	w, _ := newTestWriter(api, []string{"a", "b"})
	_, err := w.Apply(context.Background(), "t", ensure(ourAA("aa", "vm:1", "vm:2", "vm:3")))
	if !errors.Is(err, ErrRuleInfeasible) || len(api.writes) != 0 {
		t.Fatalf("err %v writes %v", err, api.writes)
	}
	// With the configured nodes unknown the writer leaves the check to PVE.
	w.ClusterNodes = nil
	if _, err := w.Apply(context.Background(), "t", ensure(ourAA("aa", "vm:1", "vm:2", "vm:3"))); err != nil {
		t.Fatal(err)
	}
}

func TestApplyCarriesInfeasibleOnAPVERefusal(t *testing.T) {
	refusal := &sdkerrors.APIError{HTTPCode: 400, Message: "invalid format",
		Errors: map[string]string{"resources": "rule must specify at least two resources"}}
	api := &fakeRuleAPI{faults: map[int]ruleFault{0: {err: refusal}}}
	w, _ := newTestWriter(api, nil)
	_, err := w.Apply(context.Background(), "t", ensure(ourAA("aa", "vm:1", "vm:2")))
	var apiErr *sdkerrors.APIError
	if !errors.Is(err, ErrRuleInfeasible) || !errors.As(err, &apiErr) || !cpierrors.IsType(err, cpierrors.TypeCloud) {
		t.Fatalf("err = %v", err)
	}
	if len(api.writes) != 1 {
		t.Fatalf("a refused write was sent again: %v", api.writes)
	}
}

func TestApplyKeepsOtherRefusalsApartFromInfeasible(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		status string
	}{
		{"permission", &sdkerrors.APIError{HTTPCode: 403, Message: "Permission check failed (/, Sys.Console)"},
			"HTTP 403"},
		{"parameter", &sdkerrors.APIError{HTTPCode: 400, Message: "Parameter verification failed.",
			Errors: map[string]string{"nodes": "invalid format"}}, "HTTP 400"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := &fakeRuleAPI{faults: map[int]ruleFault{0: {err: tc.err}}}
			w, _ := newTestWriter(api, nil)
			_, err := w.Apply(context.Background(), "t", ensure(ourAA("aa", "vm:1", "vm:2")))
			if errors.Is(err, ErrRuleInfeasible) || !errors.Is(err, ErrRuleRefused) ||
				!cpierrors.IsType(err, cpierrors.TypeCloud) || !strings.Contains(err.Error(), tc.status) {
				t.Fatalf("err = %v", err)
			}
			if len(api.writes) != 1 {
				t.Fatalf("a refused write was sent again: %v", api.writes)
			}
		})
	}
}

func TestIsFeasibilityRefusal(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{nil, false},
		{&sdkerrors.APIError{HTTPCode: 400, Message: "rule 'x' is infeasible"}, true},
		{&sdkerrors.APIError{HTTPCode: 400, Message: "invalid format", Errors: map[string]string{
			"resources": "rule defines more resources than available nodes"}}, true},
		{&sdkerrors.APIError{HTTPCode: 403, Message: "rule defines more resources than available nodes"}, true},
		{&sdkerrors.APIError{HTTPCode: 403, Message: "Permission check failed"}, false},
		{&sdkerrors.APIError{HTTPCode: 404, Message: "no such rule"}, false},
		{&sdkerrors.APIError{HTTPCode: 500, Message: "rule is infeasible"}, false},
		{errors.New("rule is infeasible"), false},
	} {
		if got := IsFeasibilityRefusal(tc.err); got != tc.want {
			t.Errorf("IsFeasibilityRefusal(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

func TestClassifyRuleWrite(t *testing.T) {
	del := RuleOp{Kind: OpDelete, Rule: Rule{ID: "x"}}
	create := RuleOp{Kind: OpCreate, Rule: Rule{ID: "x"}}
	for _, tc := range []struct {
		op   RuleOp
		err  error
		want writeOutcome
	}{
		{del, nil, writeLanded},
		{del, errors.New("no such rule 'x'"), writeLanded},
		{create, errors.New("rule 'x' already defined"), writeHeld},
		{create, errors.New("detected modified configuration"), writeConflict},
		{create, &sdkerrors.APIError{HTTPCode: 400, Message: "bad"}, writeRejected},
		{RuleOp{Kind: OpUpdate, Rule: Rule{ID: "x"}}, &sdkerrors.APIError{HTTPCode: 404, Message: "no such rule"},
			writeConflict},
		{RuleOp{Kind: OpUpdate, Rule: Rule{ID: "x"}}, &sdkerrors.APIError{HTTPCode: 403, Message: "denied"},
			writeRejected},
		{create, &sdkerrors.APIError{HTTPCode: 503, Message: "busy"}, writeHeld},
		{create, errors.New("EOF"), writeHeld},
	} {
		if got := classifyRuleWrite(tc.op, tc.err); got != tc.want {
			t.Errorf("%s %v = %v, want %v", tc.op.Kind, tc.err, got, tc.want)
		}
	}
	if IsDigestConflict(nil) {
		t.Fatal("nil is a conflict")
	}
}

func TestApplyDeletesAndUpdatesFields(t *testing.T) {
	disabled := ourPin("p", true, map[string]int{"a": 0}, "vm:1")
	disabled.Disabled = true
	api := &fakeRuleAPI{rules: []Rule{disabled, ourAA("gone", "vm:1", "vm:2")}}
	w, _ := newTestWriter(api, nil)
	want := naRule("p", false, map[string]int{"a": 2, "b": 0}, "vm:1")
	plan := func(set *RuleSet) ([]RuleOp, error) {
		if set.Find("gone") != nil {
			return []RuleOp{{Kind: OpDelete, Rule: Rule{ID: "gone"}}}, nil
		}
		return ensure(want)(set)
	}
	res, err := w.Apply(context.Background(), "t", plan)
	if err != nil {
		t.Fatal(err)
	}
	got := res.Set.Find("p")
	if !slices.Equal(api.writes, []string{"delete:gone", "update:p"}) || got.Disabled || got.Comment != "" ||
		got.Strict || got.NodesParam() != "a:2,b" {
		t.Fatalf("writes %v rule %+v", api.writes, got)
	}
}

func TestVerifyPreferenceRuleNotInEffect(t *testing.T) {
	for _, managed := range []bool{false, true} {
		api := &fakeRuleAPI{evaluate: func(f *fakeRuleAPI) {
			if r := f.find("pref"); r != nil {
				r.Disabled = true
			}
		}}
		w, _ := newTestWriter(api, nil)
		w.Managed = managed
		_, err := w.Apply(context.Background(), "t", func(set *RuleSet) ([]RuleOp, error) {
			if set.Find("pref") == nil {
				return []RuleOp{{Kind: OpCreate, Rule: ourPin("pref", false, map[string]int{"a": 0}, "vm:1")}}, nil
			}
			return nil, nil
		})
		if managed != errors.Is(err, ErrRuleNotInEffect) {
			t.Fatalf("managed=%v err = %v", managed, err)
		}
		if !managed && err != nil {
			t.Fatalf("auto failed: %v", err)
		}
	}
}

func TestVerifyLeavesAnAlreadyFailingRuleAlone(t *testing.T) {
	stale := ourAA("stale", "vm:7", "vm:8")
	stale.Disabled = true
	api := &fakeRuleAPI{rules: []Rule{stale}}
	w, _ := newTestWriter(api, nil)
	w.Managed = true
	if _, err := w.Apply(context.Background(), "t", ensure(ourAA("aa", "vm:1", "vm:2"))); err != nil {
		t.Fatalf("a rule that was failing before the write failed the call: %v", err)
	}
}

func TestRepairRevertsAWriteThatDisabledAPin(t *testing.T) {
	pin := ourPin("pin", true, map[string]int{"a": 2, "b": 0}, "vm:1")
	api := &fakeRuleAPI{rules: []Rule{pin}, evaluate: pveFlagsPriorityPins}
	for _, managed := range []bool{false, true} {
		api.rules, api.writes = []Rule{pin.Clone()}, nil
		w, _ := newTestWriter(api, nil)
		w.Managed = managed
		res, err := w.Apply(context.Background(), "t", func(set *RuleSet) ([]RuleOp, error) {
			if len(api.writes) == 0 {
				return []RuleOp{{Kind: OpCreate, Rule: ourAA("aa", "vm:1", "vm:2")}}, nil
			}
			return nil, nil
		})
		if managed != errors.Is(err, ErrRuleNotInEffect) || (!managed && err != nil) {
			t.Fatalf("managed=%v err %v", managed, err)
		}
		// The pin was in effect before, so the write that disabled it is undone.
		if !slices.Equal(api.writes, []string{"create:aa", "delete:aa"}) || api.find("aa") != nil {
			t.Fatalf("writes %v", api.writes)
		}
		if p := res.Set.Find("pin"); p == nil || len(p.Errors) != 0 {
			t.Fatalf("pin after the repair = %+v", p)
		}
	}
}

func TestRepairRewritesANewPinAsConfinementOnly(t *testing.T) {
	api := &fakeRuleAPI{rules: []Rule{ourAA("aa", "vm:1", "vm:2")}, evaluate: pveFlagsPriorityPins}
	w, _ := newTestWriter(api, nil)
	res, err := w.Apply(context.Background(), "t", func(set *RuleSet) ([]RuleOp, error) {
		if set.Find("pin") == nil {
			return []RuleOp{{Kind: OpCreate, Rule: ourPin("pin", true, map[string]int{"a": 2, "b": 0}, "vm:1")}}, nil
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	p := res.Set.Find("pin")
	if p == nil || len(p.Errors) != 0 || p.MultiplePriorities() || !p.Strict || res.Set.Find("aa") == nil {
		t.Fatalf("pin %+v writes %v", p, api.writes)
	}
}

func TestConfinementOnlyDropsTheVMFromTheConflictingRule(t *testing.T) {
	// The CPI's checks name the anti-affinity rule, whose two members are pinned
	// to one node. The rewrite takes the pinned VM out of it and strips the
	// pin's priorities.
	pin := ourPin("pin", true, map[string]int{"a": 2}, "vm:1")
	api := &fakeRuleAPI{rules: []Rule{ourAA("aa", "vm:1", "vm:2", "vm:3"),
		ourPin("p2", true, map[string]int{"a": 0}, "vm:2"), pin}}
	w, _ := newTestWriter(api, []string{"a", "b", "c"})
	set, err := w.read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := w.confinementOnly(context.Background(), &ApplyResult{Set: set}, set.Find("pin")); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(api.writes, []string{"update:aa", "update:pin"}) ||
		!slices.Equal(api.find("aa").Resources, []string{"vm:2", "vm:3"}) || api.find("pin").NodesParam() != "a" {
		t.Fatalf("writes %v rules %+v", api.writes, api.rules)
	}
}

func unrepairable(f *fakeRuleAPI) {
	if p := f.find("pin"); p != nil {
		p.Errors = []string{"resources: unrepairable"}
	}
}

func goodState(sid, uid string) *pve.HAServiceState {
	return &pve.HAServiceState{SID: sid, Known: true, InCRM: true, UID: uid, Raw: haStateStarted, Node: "a",
		NodeHealth: pve.NodeHealthEntry{Health: pve.NodeHealthOnline, ProxyTarget: true}, LRMFresh: true}
}

func applyUnrepairable(t *testing.T, rd RepairDeps) error {
	t.Helper()
	api := &fakeRuleAPI{evaluate: unrepairable}
	w, _ := newTestWriter(api, nil)
	w.Repair = rd
	_, err := w.Apply(context.Background(), "t", func(set *RuleSet) ([]RuleOp, error) {
		if set.Find("pin") == nil {
			return []RuleOp{{Kind: OpCreate, Rule: ourPin("pin", true, map[string]int{"a": 0}, "vm:101")}}, nil
		}
		return nil, nil
	})
	return err
}

func TestRepairRemovesTheResourceOnlyWhenProvenSafe(t *testing.T) {
	var removed []string
	remove := func(_ context.Context, sid string) error {
		removed = append(removed, sid)
		return nil
	}
	if err := applyUnrepairable(t, RepairDeps{}); !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("no removal path = %v", err)
	}
	if err := applyUnrepairable(t, RepairDeps{RemoveResource: remove,
		StagedIgnored: func(string) bool { return true }}); err != nil || !slices.Equal(removed, []string{"vm:101"}) {
		t.Fatalf("staged removal = %v, removed %v", err, removed)
	}

	removed = nil
	uid := func(string) (string, bool) { return "u1", true }
	if err := applyUnrepairable(t, RepairDeps{RemoveResource: remove, DecisionUID: uid,
		ReadState: func(context.Context, int) (*pve.HAServiceState, error) {
			return goodState("vm:101", "u1"), nil
		}}); err != nil || len(removed) != 1 {
		t.Fatalf("proven removal = %v, removed %v", err, removed)
	}

	// A second VM, or a changed episode, keeps its resource.
	removed = nil
	for name, rd := range map[string]RepairDeps{
		"no decision read": {RemoveResource: remove},
		"uid changed": {RemoveResource: remove, DecisionUID: uid,
			ReadState: func(context.Context, int) (*pve.HAServiceState, error) { return goodState("vm:101", "u2"), nil }},
		"stale lrm": {RemoveResource: remove, DecisionUID: uid,
			ReadState: func(context.Context, int) (*pve.HAServiceState, error) {
				st := goodState("vm:101", "u1")
				st.LRMFresh = false
				return st, nil
			}},
		"unknown uid": {RemoveResource: remove, DecisionUID: func(string) (string, bool) { return "", false },
			ReadState: func(context.Context, int) (*pve.HAServiceState, error) { return goodState("vm:101", "u1"), nil }},
		"read fails": {RemoveResource: remove, DecisionUID: uid,
			ReadState: func(context.Context, int) (*pve.HAServiceState, error) { return nil, errors.New("down") }},
	} {
		if err := applyUnrepairable(t, rd); !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if len(removed) != 0 {
		t.Fatalf("an unproven removal ran: %v", removed)
	}
}

func TestRemovalBlocker(t *testing.T) {
	cases := map[string]func(*pve.HAServiceState){
		"unknown":   func(s *pve.HAServiceState) { s.Known, s.Reason = false, "manager status missing" },
		"other sid": func(s *pve.HAServiceState) { s.SID = "vm:2" },
		"not crm":   func(s *pve.HAServiceState) { s.InCRM = false },
		"migrating": func(s *pve.HAServiceState) { s.Raw = "migrate" },
		"offline":   func(s *pve.HAServiceState) { s.NodeHealth.Health = pve.NodeHealthUnknown },
		"fenced":    func(s *pve.HAServiceState) { s.NodeHealth.DeadOrFenced = true },
		"stale lrm": func(s *pve.HAServiceState) { s.LRMFresh = false },
	}
	for name, mut := range cases {
		st := goodState("vm:1", "u")
		mut(st)
		if removalBlocker(st, "vm:1", "u") == "" {
			t.Errorf("%s: removal allowed", name)
		}
	}
	if why := removalBlocker(nil, "vm:1", "u"); !strings.Contains(why, "unknown") {
		t.Fatalf("nil state = %q", why)
	}
	stopped := goodState("vm:1", "u")
	stopped.Raw = haStateStopped
	if why := removalBlocker(stopped, "vm:1", "u"); why != "" {
		t.Fatalf("a settled state was blocked: %s", why)
	}
	if _, ok := sidVMID("ct:1"); ok {
		t.Fatal("a container sid parsed as a VM")
	}
}
