package main

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/harebalance"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// markedFake is the base cluster with its one resource carrying a rebalance
// mark of a move to n2.
func markedFake() *fakeHA {
	f := haBaseFake()
	f.resources[0].Comment = ha.WithRebalanceMark(haMarker, "n2")
	return f
}

func TestCollectRebalancePendingReportsAMarkNoRunHolds(t *testing.T) {
	t.Parallel()
	for name, edit := range map[string]func(f *fakeHA){
		"no run lock":           func(*fakeHA) {},
		"an expired run lock":   func(f *fakeHA) { f.runLockHeld, f.runLockExpiry = true, haNow.Add(-time.Minute) },
		"a lease that just ran": func(f *fakeHA) { f.runLockHeld, f.runLockExpiry = true, haNow },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := markedFake()
			edit(f)
			r := runCollect(t, f, haCfg("auto", "none"))
			want(t, r, haKindRebalancePending, haSeverityProblem, "vm:100", "a move to n2",
				"pve-ops ha rebalance --apply --director-uuid "+haDirector)
			if f.runLockReads != 1 {
				t.Errorf("run lock reads = %d, want 1", f.runLockReads)
			}
		})
	}
}

func TestCollectRebalancePendingLeavesALiveRunAlone(t *testing.T) {
	t.Parallel()
	for name, expiry := range map[string]time.Time{
		"a live lease":             haNow.Add(time.Hour),
		"a lock with no lease set": {},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := markedFake()
			f.runLockHeld, f.runLockExpiry = true, expiry
			wantNone(t, runCollect(t, f, haCfg("auto", "none")), haKindRebalancePending)
		})
	}
}

func TestCollectRebalancePendingSkipsWhatItDoesNotJudge(t *testing.T) {
	t.Parallel()
	foreign := "bosh-cpi:v1 owner=99999999-2222-3333-4444-555555555555"
	for name, tc := range map[string]struct {
		edit func(f *fakeHA)
		mode string
	}{
		"no mark":             {edit: func(f *fakeHA) { f.resources[0].Comment = haMarker }},
		"mode off":            {edit: func(f *fakeHA) { f.profile = haHealthyProfile("off") }, mode: "off"},
		"other Director's":    {edit: func(f *fakeHA) { f.resources[0].Comment = ha.WithRebalanceMark(foreign, "n2") }},
		"no ownership marker": {edit: func(f *fakeHA) { f.resources[0].Comment = "bosh-rebalance-to=n2" }},
		"resources unread":    {edit: func(f *fakeHA) { f.resourcesErr = errFake }},
		"a mark with no node": {edit: func(f *fakeHA) { f.resources[0].Comment = haMarker + " bosh-rebalance-to=" }},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := markedFake()
			tc.edit(f)
			mode := tc.mode
			if mode == "" {
				mode = "auto"
			}
			wantNone(t, runCollect(t, f, haCfg(mode, "none")), haKindRebalancePending)
			if f.runLockReads != 0 {
				t.Errorf("run lock reads = %d with no mark to judge, want 0", f.runLockReads)
			}
		})
	}
}

func TestCollectRebalancePendingLockUnreadIsUnchecked(t *testing.T) {
	t.Parallel()
	f := markedFake()
	f.runLockErr = errFake
	r := runCollect(t, f, haCfg("auto", "none"))
	wantNone(t, r, haKindRebalancePending)
	want(t, r, haKindUnchecked, haSeverityProblem, "ha-rebalance-lock", "rebalance run lock")
}

func TestCollectRebalancePendingWithoutADirectorNamesTheMarkersOwner(t *testing.T) {
	t.Parallel()
	f := markedFake()
	r := collectHA(context.Background(), f, haCfg("auto", "none"), "", haNow)
	want(t, r, haKindRebalancePending, haSeverityProblem, "--director-uuid "+haDirector)

	f = markedFake()
	f.resources[0].Comment = ha.WithRebalanceMark("bosh-cpi:v1 owner=-", "n2")
	r = collectHA(context.Background(), f, haCfg("auto", "none"), "", haNow)
	want(t, r, haKindRebalancePending, haSeverityProblem, "--director-uuid <director-uuid>")
}

func TestCollectFailbackDriftSkipsARebalanceMark(t *testing.T) {
	t.Parallel()
	f := haBaseFake()
	f.resources = []HAResourceRow{{SID: "vm:100", State: "started", Comment: ha.WithRebalanceMark(haMarker, "n2"),
		Failback: failbackRow(false)}}
	wantNone(t, runCollect(t, f, haCfg("auto", "none")), haKindFailbackOnReplica, haKindFailbackDrift)

	// The same resource without the mark is drift, so the skip is the mark's.
	f = haBaseFake()
	f.resources = []HAResourceRow{{SID: "vm:100", State: "started", Comment: haMarker, Failback: failbackRow(false)}}
	if len(findings(runCollect(t, f, haCfg("auto", "none")), haKindFailbackDrift)) != 1 {
		t.Fatal("the unmarked resource must report failback_drift, or the test proves nothing")
	}
}

// skewedPlan is a dry run that moves two VMs back into their AZ and one to
// even the memory, over a spread of 30 points.
func skewedPlan() handlers.RebalanceReport {
	return handlers.RebalanceReport{Cluster: "prod-east", SpreadBefore: 0.3,
		Gates: []handlers.RebalanceGate{{Name: handlers.RebalanceGateProfile, OK: true}},
		Moves: []handlers.RebalanceMove{{VMID: 1, Phase: harebalance.PhaseRestore},
			{VMID: 2, Phase: harebalance.PhaseRestore}, {VMID: 3, Phase: harebalance.PhaseSpread}}}
}

func TestCollectSpreadSkewedAdvisesTheDryRun(t *testing.T) {
	t.Parallel()
	f := haBaseFake()
	f.plan = skewedPlan()
	r := runCollect(t, f, haCfg("auto", "none"))
	want(t, r, haKindSpreadSkewed, haSeverityAdvice, "prod-east", "2 HA VMs of this Director run outside their AZ",
		"spreads 30 points", "start threshold of 15", "pve-ops ha rebalance --director-uuid "+haDirector)
	if len(f.planOwners) != 1 || f.planOwners[0] != haDirector {
		t.Fatalf("dry runs %v, want one for %s", f.planOwners, haDirector)
	}
}

// separationPlan is the dry run of a lab cluster whose anti-affinity members
// shared nodes, with two separation moves and a spread of 6 points.
func separationPlan() handlers.RebalanceReport {
	return handlers.RebalanceReport{Cluster: "prod-east", SpreadBefore: 0.06,
		Gates: []handlers.RebalanceGate{{Name: handlers.RebalanceGateProfile, OK: true}},
		Moves: []handlers.RebalanceMove{{VMID: 1, Phase: harebalance.PhaseSeparate},
			{VMID: 2, Phase: harebalance.PhaseSeparate}}}
}

func TestCollectSpreadSkewedCountsSeparationMoves(t *testing.T) {
	t.Parallel()
	f := haBaseFake()
	f.plan = separationPlan()
	want(t, runCollect(t, f, haCfg("auto", "none")), haKindSpreadSkewed, haSeverityAdvice, "prod-east",
		"2 moves that pull apart members of an anti-affinity rule sharing a node", "spreads 6 points",
		"pve-ops ha rebalance --director-uuid "+haDirector)

	f = haBaseFake()
	f.plan = separationPlan()
	f.plan.Moves = f.plan.Moves[:1]
	want(t, runCollect(t, f, haCfg("auto", "none")), haKindSpreadSkewed, haSeverityAdvice,
		"1 move that pulls apart members of an anti-affinity rule sharing a node")
}

func TestCollectSpreadSkewedStaysQuietWithNoMoves(t *testing.T) {
	t.Parallel()
	f := haBaseFake()
	f.plan = separationPlan()
	f.plan.Moves = nil
	wantNone(t, runCollect(t, f, haCfg("auto", "none")), haKindSpreadSkewed)
}

func TestCollectSpreadSkewedFiresOnEitherSignal(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		plan  func(p *handlers.RebalanceReport)
		frags []string
	}{
		"VMs outside their AZ alone": {plan: func(p *handlers.RebalanceReport) {
			p.SpreadBefore, p.Moves = 0.05, p.Moves[:1]
		}, frags: []string{"1 HA VM of this Director runs outside its AZ", "spreads 5 points"}},
		"a wide spread alone": {plan: func(p *handlers.RebalanceReport) {
			p.Moves = p.Moves[2:]
		}, frags: []string{"0 HA VMs", "spreads 30 points"}},
		"a gate that may clear": {plan: func(p *handlers.RebalanceReport) {
			p.ExitCode, p.Gates = handlers.RebalanceExitGate,
				append(p.Gates, handlers.RebalanceGate{Name: handlers.RebalanceGateSettled})
		}, frags: []string{"2 HA VMs"}},
		"a cluster with no name": {plan: func(p *handlers.RebalanceReport) { p.Cluster = "" },
			frags: []string{"cluster"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := haBaseFake()
			f.plan = skewedPlan()
			tc.plan(&f.plan)
			want(t, runCollect(t, f, haCfg("auto", "none")), haKindSpreadSkewed, haSeverityAdvice, tc.frags...)
		})
	}
}

func TestCollectSpreadSkewedStaysQuiet(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		edit  func(f *fakeHA)
		mode  string
		owner string
		runs  int
	}{
		"an even cluster": {edit: func(f *fakeHA) {
			f.plan.SpreadBefore, f.plan.Moves = handlers.RebalanceDefaultStart, f.plan.Moves[2:]
		}, owner: haDirector, runs: 1},
		"no Director":       {edit: func(*fakeHA) {}},
		"mode off":          {edit: func(f *fakeHA) { f.profile = haHealthyProfile("off") }, mode: "off", owner: haDirector},
		"resources unread":  {edit: func(f *fakeHA) { f.resourcesErr = errFake }, owner: haDirector},
		"states unread":     {edit: func(f *fakeHA) { f.statesErr = errFake }, owner: haDirector},
		"rules unread":      {edit: func(f *fakeHA) { f.rulesErr = errFake }, owner: haDirector},
		"guests unread":     {edit: func(f *fakeHA) { f.guestsErr = errFake }, owner: haDirector},
		"guests partial":    {edit: func(f *fakeHA) { f.excluded = []string{"n3"} }, owner: haDirector},
		"the dry run fails": {edit: func(f *fakeHA) { f.planErr = errFake }, owner: haDirector, runs: 1},
		"a dry-run read fails": {edit: func(f *fakeHA) {
			f.plan.ExitCode, f.plan.Halted = handlers.RebalanceExitError, "a read failed"
		}, owner: haDirector, runs: 1},
		"the profile gate refuses": {edit: func(f *fakeHA) {
			f.plan.ExitCode, f.plan.Gates[0].OK = handlers.RebalanceExitGate, false
		}, owner: haDirector, runs: 1},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := haBaseFake()
			f.plan = skewedPlan()
			tc.edit(f)
			mode := tc.mode
			if mode == "" {
				mode = "auto"
			}
			r := collectHA(context.Background(), f, haCfg(mode, "none"), tc.owner, haNow)
			wantNone(t, r, haKindSpreadSkewed)
			if len(f.planOwners) != tc.runs {
				t.Errorf("dry runs = %d, want %d", len(f.planOwners), tc.runs)
			}
		})
	}
}

// rbLockPools answers GetPoolComment for the run lock pool.
type rbLockPools struct {
	pve.PoolService
	comment string
	found   bool
	err     error
	asked   []string
}

func (p *rbLockPools) GetPoolComment(_ context.Context, id string) (string, bool, error) {
	p.asked = append(p.asked, id)
	return p.comment, p.found, p.err
}

type rbLockClient struct {
	pve.Client
	pools pve.PoolService
}

func (c *rbLockClient) Pools() pve.PoolService { return c.pools }

func TestReaderRebalanceRunLock(t *testing.T) {
	t.Parallel()
	exp := time.Unix(1_800_000_000, 0)
	for name, tc := range map[string]struct {
		pools      *rbLockPools
		wantExpiry time.Time
		wantHeld   bool
		wantErr    bool
	}{
		"absent": {pools: &rbLockPools{}},
		"held with a lease": {pools: &rbLockPools{found: true, comment: "owner=x exp=" + strconv.FormatInt(exp.Unix(), 10)},
			wantExpiry: exp, wantHeld: true},
		"held with no lease": {pools: &rbLockPools{found: true, comment: "owner=x"}, wantHeld: true},
		"unreadable":         {pools: &rbLockPools{err: errors.New("403")}, wantErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := &pveHAReader{client: &rbLockClient{pools: tc.pools}}
			expiry, held, err := r.RebalanceRunLock(context.Background())
			if (err != nil) != tc.wantErr || held != tc.wantHeld || !expiry.Equal(tc.wantExpiry) {
				t.Fatalf("expiry %v held %v err %v", expiry, held, err)
			}
			if len(tc.pools.asked) != 1 || tc.pools.asked[0] != "bosh-lock-ha-rebalance" {
				t.Fatalf("asked %v, want the run lock pool", tc.pools.asked)
			}
		})
	}
	if _, _, err := (&pveHAReader{}).RebalanceRunLock(context.Background()); err == nil {
		t.Fatal("a reader with no client must fail the read")
	}
}

func TestReaderRebalancePlanRunsADryRun(t *testing.T) {
	t.Parallel()
	// A reader with no PVE client cannot plan, and RebalanceHA says so
	// before it reads anything, which proves the call reaches it with the
	// doctor's config and owner.
	r := &pveHAReader{cfg: haCfg("auto", "none")}
	if _, err := r.RebalancePlan(context.Background(), haDirector); err == nil {
		t.Fatal("a dry run with no PVE client must fail")
	}
	if _, err := (&pveHAReader{}).RebalancePlan(context.Background(), haDirector); err == nil {
		t.Fatal("a dry run with no config must fail")
	}
}
