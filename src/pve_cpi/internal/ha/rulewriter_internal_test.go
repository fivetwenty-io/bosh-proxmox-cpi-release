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

// TestApplyBacksOffBetweenHeldWrites pins the wait a held write that did not
// land pays before the writer plans again. The refusal PVE gives for a member
// whose HA resource is not staged yet is a 500, which reads as held, so
// without the wait five attempts spend themselves in milliseconds.
func TestApplyBacksOffBetweenHeldWrites(t *testing.T) {
	refusal := errors.New("500 Internal Server Error: cannot use unmanaged resource(s) vm:2")
	faults := map[int]ruleFault{}
	for i := range RuleWriterMaxAttempts {
		faults[i] = ruleFault{err: refusal}
	}
	api := &fakeRuleAPI{faults: faults}
	var waits []time.Duration
	w := &RuleWriter{API: api, Sleep: func(_ context.Context, d time.Duration) error {
		waits = append(waits, d)
		return nil
	}}
	_, err := w.Apply(context.Background(), "t", ensure(ourAA("aa", "vm:1", "vm:2")))
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("err = %v, want retriable", err)
	}
	if len(waits) != RuleWriterMaxAttempts-1 {
		t.Fatalf("waits %v, want one before each of the %d later attempts", waits, RuleWriterMaxAttempts-1)
	}
	for i, d := range waits {
		lo := min(ruleBackoffBase<<i, ruleBackoffMax)
		if d < lo || d > lo+lo/2 {
			t.Errorf("wait %d = %v, want within [%v, %v]", i+1, d, lo, lo+lo/2)
		}
	}

	// A held write that lands on a later attempt waits only before that one.
	api = &fakeRuleAPI{faults: map[int]ruleFault{0: {err: refusal}}}
	waits = nil
	w.API = api
	if _, err := w.Apply(context.Background(), "t", ensure(ourAA("aa", "vm:1", "vm:2"))); err != nil {
		t.Fatal(err)
	}
	if len(waits) != 1 || !slices.Equal(api.writes, []string{"create:aa", "create:aa"}) {
		t.Fatalf("waits %v writes %v", waits, api.writes)
	}
}

// TestApplyStopsWaitingWhenTheContextEnds pins that the wait between held
// writes ends with the request.
func TestApplyStopsWaitingWhenTheContextEnds(t *testing.T) {
	api := &fakeRuleAPI{faults: map[int]ruleFault{0: {err: errors.New("i/o timeout")}}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w := &RuleWriter{API: api}
	if _, err := w.Apply(ctx, "t", ensure(ourAA("aa", "vm:1", "vm:2"))); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if len(api.writes) != 1 {
		t.Fatalf("writes %v, want the one held write and no resend", api.writes)
	}
}

// TestApplyDoesNotStartAWaitThatOutrunsTheDeadline pins that a backoff the
// request's deadline leaves no room for is not slept. The Apply returns at
// once with a retriable error, so the Director drives the call again instead
// of the create sleeping into its deadline and failing with a bare one.
func TestApplyDoesNotStartAWaitThatOutrunsTheDeadline(t *testing.T) {
	refusal := errors.New("500 Internal Server Error: cannot use unmanaged resource(s) vm:2")
	api := &fakeRuleAPI{faults: map[int]ruleFault{0: {err: refusal}}}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	slept := 0
	w := &RuleWriter{API: api, Sleep: func(context.Context, time.Duration) error {
		slept++
		return nil
	}}
	_, err := w.Apply(ctx, "t", ensure(ourAA("aa", "vm:1", "vm:2")))
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want a retriable deadline error", err)
	}
	if slept != 0 || len(api.writes) != 1 {
		t.Fatalf("slept %d writes %v, want no wait and no resend", slept, api.writes)
	}
}

// TestApplyWrapsAnEndedWaitAsRetriable pins that a wait the request ends, or
// that a lock which could not be taken again ends, fails the Apply with a
// retriable error that keeps the cause.
func TestApplyWrapsAnEndedWaitAsRetriable(t *testing.T) {
	api := &fakeRuleAPI{faults: map[int]ruleFault{0: {err: errors.New("i/o timeout")}}}
	lost := errors.New("the rules lock could not be taken again")
	w := &RuleWriter{API: api, Backoff: func(context.Context, time.Duration, bool) error { return lost }}
	_, err := w.Apply(context.Background(), "t", ensure(ourAA("aa", "vm:1", "vm:2")))
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) || !errors.Is(err, lost) {
		t.Fatalf("err = %v, want a retriable error that wraps the cause", err)
	}
}

// TestApplyTellsBackoffWhetherItHasWritten pins the idle flag. A wait before
// the first write may release the rules lock, and the Apply then takes its
// baseline from the next read. A wait after a write keeps the lock, because a
// repair may have to revert that write against the baseline.
func TestApplyTellsBackoffWhetherItHasWritten(t *testing.T) {
	refusal := errors.New("500 Internal Server Error: cannot use unmanaged resource(s) vm:3")
	both := func(set *RuleSet) ([]RuleOp, error) {
		for _, want := range []Rule{ourAA("a", "vm:1", "vm:2"), ourAA("b", "vm:3", "vm:4")} {
			if set.Find(want.ID) == nil {
				return []RuleOp{{Kind: OpCreate, Rule: want}}, nil
			}
		}
		return nil, nil
	}

	var idles []bool
	backoff := func(_ context.Context, _ time.Duration, idle bool) error {
		idles = append(idles, idle)
		return nil
	}
	api := &fakeRuleAPI{faults: map[int]ruleFault{0: {err: refusal}}}
	w := &RuleWriter{API: api, Backoff: backoff}
	if _, err := w.Apply(context.Background(), "t", both); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(idles, []bool{true}) {
		t.Fatalf("idle flags %v, want one idle wait before the first write", idles)
	}

	idles = nil
	api = &fakeRuleAPI{faults: map[int]ruleFault{1: {err: refusal}}}
	w.API = api
	if _, err := w.Apply(context.Background(), "t", both); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(idles, []bool{false}) {
		t.Fatalf("idle flags %v, want one wait that keeps the lock after rule a landed", idles)
	}
	if !slices.Equal(api.writes, []string{"create:a", "create:b", "create:b"}) {
		t.Fatalf("writes %v", api.writes)
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
		// The revert's delete is reported with the comment the rule had, so
		// a caller can tell whose group rule the repair removed.
		if len(res.Repaired) != 1 || res.Repaired[0].Kind != OpDelete || res.Repaired[0].Rule.ID != "aa" ||
			res.Repaired[0].Rule.Comment != ourMarker {
			t.Fatalf("repaired = %+v; want the delete of aa with its comment", res.Repaired)
		}
		if !slices.Equal(res.Confined, []string{"vm:1"}) {
			t.Fatalf("confined = %v; want the pinned VM", res.Confined)
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
	res := &ApplyResult{Set: set}
	if err := w.confinementOnly(context.Background(), res, set.Find("pin")); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(api.writes, []string{"update:aa", "update:pin"}) ||
		!slices.Equal(api.find("aa").Resources, []string{"vm:2", "vm:3"}) || api.find("pin").NodesParam() != "a" {
		t.Fatalf("writes %v rules %+v", api.writes, api.rules)
	}
	if len(res.Repaired) != 2 || res.Repaired[0].Kind != OpUpdate || res.Repaired[0].Rule.ID != "aa" ||
		!slices.Equal(res.Repaired[0].Rule.Resources, []string{"vm:2", "vm:3"}) ||
		res.Repaired[1].Kind != OpUpdate || res.Repaired[1].Rule.ID != "pin" {
		t.Fatalf("repaired = %+v; want the update of aa and of the pin", res.Repaired)
	}
}

// TestConfinementOnlyReportsTheDeleteOfARuleItLeavesTooSmall pins that the
// rewrite deletes an anti-affinity rule that would keep one member, and
// reports that delete with the comment the rule had.
func TestConfinementOnlyReportsTheDeleteOfARuleItLeavesTooSmall(t *testing.T) {
	pin := ourPin("pin", true, map[string]int{"a": 2}, "vm:1")
	api := &fakeRuleAPI{rules: []Rule{ourAA("aa", "vm:1", "vm:2"),
		ourPin("p2", true, map[string]int{"a": 0}, "vm:2"), pin}}
	w, _ := newTestWriter(api, []string{"a", "b", "c"})
	set, err := w.read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	res := &ApplyResult{Set: set}
	if err := w.confinementOnly(context.Background(), res, set.Find("pin")); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(api.writes, []string{"delete:aa", "update:pin"}) || api.find("aa") != nil {
		t.Fatalf("writes %v rules %+v", api.writes, api.rules)
	}
	if len(res.Repaired) != 2 || res.Repaired[0].Kind != OpDelete || res.Repaired[0].Rule.ID != "aa" ||
		res.Repaired[0].Rule.Comment != ourMarker {
		t.Fatalf("repaired = %+v; want the delete of aa with its comment first", res.Repaired)
	}
}

// TestConfinementOnlyCarriesWhatTheRuleItDeletesRecordsFirst pins that the
// rewrite asks CarryHolds what keeps the record of an anti-affinity rule it
// deletes, with a fresh read of the rules, and sends those writes before the
// delete. Each carry write is reported among the repair's writes, and the
// writer asks again after each one, so a carry that has landed is not sent
// twice.
func TestConfinementOnlyCarriesWhatTheRuleItDeletesRecordsFirst(t *testing.T) {
	pin := ourPin("pin", true, map[string]int{"a": 2}, "vm:1")
	api := &fakeRuleAPI{rules: []Rule{ourAA("aa", "vm:1", "vm:2"), ourAA("aa2", "vm:3", "vm:4"),
		ourPin("p2", true, map[string]int{"a": 0}, "vm:2"), pin}}
	w, _ := newTestWriter(api, []string{"a", "b", "c"})
	const carried = ourMarker + " carried"
	var asked []string
	w.CarryHolds = func(_ context.Context, set *RuleSet, gone *Rule, leaving []string) ([]RuleOp, error) {
		asked = append(asked, gone.ID)
		if gone.ID != "aa" || set.Find("aa") == nil || !slices.Equal(leaving, []string{"vm:1"}) {
			t.Errorf("CarryHolds got rule %q leaving %v; want aa leaving vm:1, with a read that still holds it",
				gone.ID, leaving)
		}
		next := set.Find("aa2").Clone()
		if next.Comment == carried {
			return nil, nil
		}
		next.Comment = carried
		return []RuleOp{{Kind: OpUpdate, Rule: next}}, nil
	}
	set, err := w.read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	res := &ApplyResult{Set: set}
	if err := w.confinementOnly(context.Background(), res, set.Find("pin")); err != nil {
		t.Fatal(err)
	}
	wantWrites := []string{"update:aa2", "delete:aa", "update:pin"}
	if !slices.Equal(api.writes, wantWrites) || api.find("aa2").Comment != carried {
		t.Fatalf("writes %v rules %+v; want the carry, then the delete, then the pin", api.writes, api.rules)
	}
	if !slices.Equal(asked, []string{"aa", "aa"}) {
		t.Errorf("CarryHolds was asked about %v; want aa before the carry and again after it", asked)
	}
	if len(res.Repaired) != 3 || res.Repaired[0].Rule.ID != "aa2" || res.Repaired[1].Kind != OpDelete ||
		res.Repaired[1].Rule.ID != "aa" {
		t.Fatalf("repaired = %+v; want the carry, then the delete of aa", res.Repaired)
	}
}

// TestConfinementOnlyStopsWhenTheCarryFails pins that a CarryHolds that
// can't keep what the rule records stops the repair before it deletes the
// rule, and the repair returns the carry's error.
func TestConfinementOnlyStopsWhenTheCarryFails(t *testing.T) {
	pin := ourPin("pin", true, map[string]int{"a": 2}, "vm:1")
	api := &fakeRuleAPI{rules: []Rule{ourAA("aa", "vm:1", "vm:2"),
		ourPin("p2", true, map[string]int{"a": 0}, "vm:2"), pin}}
	w, _ := newTestWriter(api, []string{"a", "b", "c"})
	carryErr := errors.New("the HA resources could not be listed")
	w.CarryHolds = func(context.Context, *RuleSet, *Rule, []string) ([]RuleOp, error) {
		return nil, carryErr
	}
	set, err := w.read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	res := &ApplyResult{Set: set}
	if err := w.confinementOnly(context.Background(), res, set.Find("pin")); !errors.Is(err, carryErr) {
		t.Fatalf("err = %v; want the carry's error", err)
	}
	if len(api.writes) != 0 || api.find("aa") == nil {
		t.Fatalf("writes %v; want the rule left in place", api.writes)
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

// TestRepairCarriesTheResourceHoldsBeforeItRemovesTheResource pins that the
// last step of an unrepairable pin's repair asks CarryResourceHolds what
// keeps the records the VM's HA resource carries, sends those writes before
// it removes the resource, and keeps the resource when the carry fails.
func TestRepairCarriesTheResourceHoldsBeforeItRemovesTheResource(t *testing.T) {
	const carried = ourMarker + " carried"
	for name, fail := range map[string]bool{"carried": false, "carry fails": true} {
		t.Run(name, func(t *testing.T) {
			api := &fakeRuleAPI{evaluate: unrepairable, rules: []Rule{ourAA("aa", "vm:2", "vm:3")}}
			w, _ := newTestWriter(api, nil)
			var removed []string
			var asked []string
			w.Repair = RepairDeps{StagedIgnored: func(string) bool { return true },
				RemoveResource: func(_ context.Context, sid string) error {
					if api.find("aa").Comment != carried {
						t.Errorf("the resource went before the carry landed; writes %v", api.writes)
					}
					removed = append(removed, sid)
					return nil
				}}
			w.CarryResourceHolds = func(_ context.Context, set *RuleSet, sid string) ([]RuleOp, error) {
				asked = append(asked, sid)
				if fail {
					return nil, errors.New("the HA resources could not be listed")
				}
				next := set.Find("aa").Clone()
				if next.Comment == carried {
					return nil, nil
				}
				next.Comment = carried
				return []RuleOp{{Kind: OpUpdate, Rule: next}}, nil
			}
			_, err := w.Apply(context.Background(), "t", func(set *RuleSet) ([]RuleOp, error) {
				if set.Find("pin") == nil {
					return []RuleOp{{Kind: OpCreate, Rule: ourPin("pin", true, map[string]int{"a": 0}, "vm:101")}}, nil
				}
				return nil, nil
			})
			if fail {
				if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) || len(removed) != 0 {
					t.Fatalf("err = %v, removed %v; want the resource kept with a retriable error", err, removed)
				}
				return
			}
			if err != nil || !slices.Equal(removed, []string{"vm:101"}) {
				t.Fatalf("err = %v, removed %v; want vm:101 removed", err, removed)
			}
			if !slices.Equal(asked, []string{"vm:101", "vm:101"}) {
				t.Errorf("CarryResourceHolds was asked about %v; want vm:101 before the carry and again after it",
					asked)
			}
		})
	}
}
