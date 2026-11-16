package handlers

import (
	"context"
	"fmt"
	"math/rand/v2"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
)

// These tests run sibling creates, and a delete, as concurrent writers whose
// API calls interleave, with pve.cluster_lock at its default off. Each writer
// runs on its own goroutine, but only one runs at a time, and a seeded source
// picks which waiting writer takes the next step. So every run is one
// reproducible order of the reads, plans, and writes of the writers, and the
// stub needs no locking.

// aaTurns hands the turn to one worker at a time.
type aaTurns struct {
	park   chan int
	resume []chan struct{}
}

// step parks worker id until the scheduler gives it the next turn.
func (s *aaTurns) step(id int) {
	s.park <- id
	<-s.resume[id]
}

// runAATurns runs workers to completion in the order seed picks and returns
// each worker's error. Each worker gets the step it must take before every
// call that touches the shared stub.
func runAATurns(seed uint64, workers []func(step func()) error) []error {
	n := len(workers)
	s := &aaTurns{park: make(chan int), resume: make([]chan struct{}, n)}
	for i := range n {
		s.resume[i] = make(chan struct{})
	}
	errs := make([]error, n)
	done := make(chan int)
	for i, work := range workers {
		go func() {
			s.step(i)
			errs[i] = work(func() { s.step(i) })
			done <- i
		}()
	}
	waiting := map[int]bool{}
	for range n {
		waiting[<-s.park] = true
	}
	rng := rand.New(rand.NewPCG(seed, 0x5eed))
	for live := n; live > 0; {
		ids := make([]int, 0, len(waiting))
		for id := range waiting {
			ids = append(ids, id)
		}
		sort.Ints(ids)
		next := ids[rng.IntN(len(ids))]
		delete(waiting, next)
		s.resume[next] <- struct{}{}
		select {
		case id := <-s.park:
			waiting[id] = true
		case <-done:
			live--
		}
	}
	return errs
}

// aaGatedCluster is one worker's view of the shared stub. Every call that
// reads or changes the stub first waits for the worker's turn.
type aaGatedCluster struct {
	*aaClusterStub
	step func()
}

func (g *aaGatedCluster) ListResources(ctx context.Context, p *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
	g.step()
	return g.aaClusterStub.ListResources(ctx, p)
}

func (g *aaGatedCluster) ListHaResources(ctx context.Context, p *cluster.ListHaResourcesParams) (*cluster.ListHaResourcesResponse, error) {
	g.step()
	return g.aaClusterStub.ListHaResources(ctx, p)
}

func (g *aaGatedCluster) GetHaResources(ctx context.Context, sid string) (*cluster.GetHaResourcesResponse, error) {
	g.step()
	return g.aaClusterStub.GetHaResources(ctx, sid)
}

func (g *aaGatedCluster) DeleteHaResources(ctx context.Context, sid string, p *cluster.DeleteHaResourcesParams) error {
	g.step()
	return g.aaClusterStub.DeleteHaResources(ctx, sid, p)
}

func (g *aaGatedCluster) ListHaRules(ctx context.Context, p *cluster.ListHaRulesParams) (*cluster.ListHaRulesResponse, error) {
	g.step()
	return g.aaClusterStub.ListHaRules(ctx, p)
}

func (g *aaGatedCluster) CreateHaRules(ctx context.Context, p *cluster.CreateHaRulesParams) error {
	g.step()
	return g.aaClusterStub.CreateHaRules(ctx, p)
}

func (g *aaGatedCluster) UpdateHaRules(ctx context.Context, rule string, p *cluster.UpdateHaRulesParams) error {
	g.step()
	return g.aaClusterStub.UpdateHaRules(ctx, rule, p)
}

func (g *aaGatedCluster) DeleteHaRules(ctx context.Context, rule string) error {
	g.step()
	return g.aaClusterStub.DeleteHaRules(ctx, rule)
}

// aaFleet is the shared state of one run: the stub and the guests that carry
// the group's member tag.
type aaFleet struct {
	stub   *aaClusterStub
	tagged map[int]bool
	// gone holds the sids a delete removes, which a write may drop.
	gone map[string]bool
}

func newAAFleet() *aaFleet {
	f := &aaFleet{stub: newAAStub(), tagged: map[int]bool{}, gone: map[string]bool{}}
	f.stub.enforceManaged = true
	f.stub.strictCreate = true
	f.stub.purgeFromRules = true
	f.stub.listResourcesFn = func() *cluster.ListResourcesResponse {
		vmids := make([]int, 0, len(f.tagged))
		for v := range f.tagged {
			vmids = append(vmids, v)
		}
		sort.Ints(vmids)
		resp := make(cluster.ListResourcesResponse, 0, len(vmids))
		for _, v := range vmids {
			resp = append(resp, aaQEMU(v, aaTag))
		}
		return &resp
	}
	return f
}

// member seeds a sibling whose create has finished: tagged, managed, marked,
// and in the rule.
func (f *aaFleet) member(vmid int) {
	sid := haResourceSid(vmid)
	f.tagged[vmid] = true
	f.stub.resources[sid] = true
	f.stub.resComments[sid] = haIdentity{Owner: aaDirector}.marker()
}

// deps returns a worker's Deps, with every stub call and every backoff wait
// taken as a step, so the other workers move while this one waits.
func (f *aaFleet) deps(step func()) Deps {
	gated := &aaGatedCluster{aaClusterStub: f.stub, step: step}
	deps := aaDeps(f.stub)
	deps.PVE = &icPVEClient{
		clusterSvc: gated,
		nodesSvc: &icNodesService{listFn: func(ctx context.Context, p *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
			return gated.ListResources(ctx, p)
		}},
	}
	deps.HAProfile = pve9ClusterProfile("pve1", "pve2", "pve3", "pve4", "pve5")
	deps.HARuleSleep = func(context.Context, time.Duration) error {
		step()
		return nil
	}
	return deps
}

// create is one sibling's create: its clone tags the guest, it stages its HA
// resource, and then it writes the group's rule.
func (f *aaFleet) create(vmid int) func(step func()) error {
	return func(step func()) error {
		deps := f.deps(step)
		f.tagged[vmid] = true
		step()
		sid := haResourceSid(vmid)
		f.stub.resources[sid] = true
		f.stub.resComments[sid] = haIdentity{Owner: aaDirector}.marker()
		step()
		return ensureAntiAffinityMembership(context.Background(), deps, aaIdent("web"), vmid, log.NewNopLogger())
	}
}

// remove is delete_vm's cleanup of vmid: it takes the VM out of its rule and
// purges its HA resource.
func (f *aaFleet) remove(vmid int) func(step func()) error {
	f.gone[haResourceSid(vmid)] = true
	return func(step func()) error {
		return removeAntiAffinityMembership(context.Background(), f.deps(step), vmid, log.NewNopLogger())
	}
}

// watch fails the run when a write leaves the rule without the marker, or
// drops a member that no delete removed.
func (f *aaFleet) watch(t *testing.T, run string) {
	t.Helper()
	var prev []string
	f.stub.onRuleWrite = func(rule string) {
		if rule != aaRule {
			return
		}
		if got := f.stub.ruleComments[aaRule]; got != aaIdent("web").marker() {
			t.Errorf("%s: rule written with comment %q; want the marker", run, got)
		}
		cur := strings.Split(f.stub.rules[aaRule], ",")
		for _, m := range prev {
			if !f.gone[m] && !slices.Contains(cur, m) {
				t.Errorf("%s: a write dropped %s from the rule (%v -> %v)", run, m, prev, cur)
			}
		}
		prev = cur
	}
}

const aaConcurrentSeeds = 400

// TestEnsureAntiAffinity_UnlockedConcurrentCreatesConverge runs three and then
// four sibling creates with the rules lock off, their reads, plans, and writes
// interleaved in many orders. Every create must finish within the writer's
// attempt budget, no write may drop a member or the marker, and the rule must
// end with every sibling.
func TestEnsureAntiAffinity_UnlockedConcurrentCreatesConverge(t *testing.T) {
	t.Parallel()
	for _, vmids := range [][]int{{101, 102, 103}, {101, 102, 103, 104}} {
		want := make([]string, 0, len(vmids))
		for _, v := range vmids {
			want = append(want, haResourceSid(v))
		}
		for seed := range uint64(aaConcurrentSeeds) {
			run := fmt.Sprintf("%d creates, seed %d", len(vmids), seed)
			f := newAAFleet()
			f.watch(t, run)
			workers := make([]func(step func()) error, 0, len(vmids))
			for _, v := range vmids {
				workers = append(workers, f.create(v))
			}
			for i, err := range runAATurns(seed, workers) {
				if err != nil {
					t.Fatalf("%s: create of %d failed, which rolls back a built VM: %v", run, vmids[i], err)
				}
			}
			if got := f.stub.rules[aaRule]; got != strings.Join(want, ",") {
				t.Fatalf("%s: rule = %q; want every sibling", run, got)
			}
		}
	}
}

// TestEnsureAntiAffinity_UnlockedDeleteAlongsideCreates runs delete_vm's
// cleanup of one member while two siblings are created, with the rules lock
// off. A create may put the deleting member back into the rule after the
// delete took it out, while its resource is still there, and the purge that
// follows takes it out again. Whatever the order, the rule must end with the
// remaining member and both new siblings, and without the deleted member.
func TestEnsureAntiAffinity_UnlockedDeleteAlongsideCreates(t *testing.T) {
	t.Parallel()
	for seed := range uint64(aaConcurrentSeeds) {
		run := fmt.Sprintf("seed %d", seed)
		f := newAAFleet()
		f.member(100)
		f.member(101)
		f.stub.seedRule(aaRule, "vm:100,vm:101")
		f.watch(t, run)
		errs := runAATurns(seed, []func(step func()) error{f.create(102), f.remove(100), f.create(103)})
		for i, err := range errs {
			if err != nil {
				t.Fatalf("%s: worker %d failed: %v", run, i, err)
			}
		}
		if got := f.stub.rules[aaRule]; got != "vm:101,vm:102,vm:103" {
			t.Fatalf("%s: rule = %q; want the deleted member gone and both siblings in", run, got)
		}
		if f.stub.resources["vm:100"] {
			t.Fatalf("%s: the deleted member's HA resource is still there", run)
		}
	}
}

// TestEnsureAntiAffinity_LockedWaitReleasesTheRulesLock pins that a create
// whose rule write PVE refused waits with the rules lock released, and takes
// it again before it plans the write again. Holding the lock through the wait
// would hold up every other rule write in the cluster.
func TestEnsureAntiAffinity_LockedWaitReleasesTheRulesLock(t *testing.T) {
	t.Parallel()
	events := []string{}
	stub := newAAStub()
	stub.events = &events
	stub.enforceManaged = true
	stub.tagged(aaQEMU(101, aaTag), aaQEMU(102, aaTag), aaQEMU(103, aaTag))
	// vm:103's create rolls back between this create's plan and its write.
	stub.afterListHaResources = func(call int) {
		if call == 1 {
			delete(stub.resources, "vm:103")
		}
	}
	pools := newAALockPools(&events)
	deps := aaDepsLock(aaLockConfig("pool", false, 30), stub, pools)
	deps.HAProfile = pve9ClusterProfile("pve1", "pve2", "pve3")
	waits := 0
	deps.HARuleSleep = func(context.Context, time.Duration) error {
		waits++
		if _, held := pools.pools[aaLock]; held {
			t.Error("the backoff ran with the rules lock held")
		}
		events = append(events, "wait")
		return nil
	}
	if err := ensureAntiAffinityMembership(context.Background(), deps, aaIdent("web"), 102,
		log.NewNopLogger()); err != nil {
		t.Fatal(err)
	}
	if waits != 1 || stub.rules[aaRule] != "vm:101,vm:102" {
		t.Fatalf("waits = %d rule = %q; want one wait and the managed members", waits, stub.rules[aaRule])
	}
	var order []string
	for _, ev := range events {
		switch {
		case ev == "lock-create:"+aaLock, ev == "lock-delete:"+aaLock, ev == "wait",
			strings.HasPrefix(ev, "create-rule:"):
			order = append(order, ev)
		}
	}
	want := []string{
		"lock-create:" + aaLock, "create-rule:" + aaRule, "lock-delete:" + aaLock, "wait",
		"lock-create:" + aaLock, "create-rule:" + aaRule, "lock-delete:" + aaLock,
	}
	if !slices.Equal(order, want) {
		t.Fatalf("order = %v; want %v", order, want)
	}
	if len(pools.pools) != 0 {
		t.Fatalf("lock pools left behind: %v", pools.pools)
	}
}

// TestRulesLockHold_WaitsThatKeepTheLockStayInsideTheBudget pins the bound on
// a writer that must keep the rules lock through its waits, here delete_vm's
// cleanup, whose writes PVE keeps refusing. The waits of one hold add up to
// rulesLockHeldWaitBudget and no more, and the lock is held through all of
// them.
func TestRulesLockHold_WaitsThatKeepTheLockStayInsideTheBudget(t *testing.T) {
	t.Parallel()
	stub := newAAStub()
	stub.seedRule(aaRule, "vm:100,vm:101,vm:102")
	stub.failUpdateRule = true
	pools := newAALockPools(nil)
	deps := aaDepsLock(aaLockConfig("pool", false, 30), stub, pools)
	var waits []time.Duration
	deps.HARuleSleep = func(_ context.Context, d time.Duration) error {
		waits = append(waits, d)
		if _, held := pools.pools[aaLock]; !held {
			t.Error("a writer that keeps the lock waited without it")
		}
		return nil
	}
	err := removeAntiAffinityMembership(context.Background(), deps, 100, log.NewNopLogger())
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("err = %v; want the retriable give-up", err)
	}
	var total time.Duration
	for _, d := range waits {
		total += d
	}
	if len(waits) == 0 || total != rulesLockHeldWaitBudget {
		t.Fatalf("waits %v add up to %v; want exactly %v", waits, total, rulesLockHeldWaitBudget)
	}
}

// TestDeletedGroupRules pins which deleted rules make a removal write a group's
// rules again: only a rule of the group its marker names, owned by this
// Director, and each group once.
func TestDeletedGroupRules(t *testing.T) {
	t.Parallel()
	web := aaIdent("web")
	other := haIdentity{Owner: "dir-2", Deployment: web.Deployment, Group: "web"}
	got := deletedGroupRules(map[string]string{
		aaRule:                   web.marker(),
		aaRule + "-2":            web.marker(),
		"bosh-aa-ffffffffffff":   web.marker(),
		other.ruleName():         other.marker(),
		aaIdent("db").ruleName(): "made by hand",
	}, aaDirector)
	if len(got) != 1 || got[0] != web {
		t.Fatalf("groups = %+v; want only %+v", got, web)
	}
}

// TestRemoveAntiAffinity_LockedDeleteDoesNotRewrite pins that with the rules
// lock on, a removal that deletes the group's rule writes nothing after it,
// because the lock kept every create out between its read and its delete.
func TestRemoveAntiAffinity_LockedDeleteDoesNotRewrite(t *testing.T) {
	t.Parallel()
	stub := newAAStub()
	stub.seedRule(aaRule, "vm:100,vm:101")
	stub.tagged(aaQEMU(100, aaTag), aaQEMU(101, aaTag), aaQEMU(102, aaTag))
	stub.resources["vm:101"], stub.resources["vm:102"] = true, true
	deps := aaDepsLock(aaLockConfig("pool", false, 30), stub, newAALockPools(nil))
	if err := removeAntiAffinityMembership(context.Background(), deps, 100, log.NewNopLogger()); err != nil {
		t.Fatal(err)
	}
	if _, ok := stub.rules[aaRule]; ok {
		t.Fatalf("rule = %q; want it deleted and not written again", stub.rules[aaRule])
	}
}
