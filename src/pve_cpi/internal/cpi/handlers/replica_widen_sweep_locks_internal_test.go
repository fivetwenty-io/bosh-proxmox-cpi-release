package handlers

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// rulesLockPool is the pool the HA rules lock creates.
func rulesLockPool() string { return pve.ClusterLockPoolName(antiAffinityLockPrefix + haRulesLockKey) }

// A call on the VM that waits on the VM's claim, such as a set_vm_metadata,
// gets it while the pass checks the VM, because the pass claims the VM only
// around the guard's read and the write.
func TestReplicaWidenPass_AWaitingCallGetsTheVMWhileThePassChecksIt(t *testing.T) {
	shortenVMIDLockWait(t, 300*time.Millisecond)
	s := newSweepRig(t)
	var lockErr error
	s.hooked.onStatus = func(call int) {
		if call == 2 {
			lockErr = withVMIDLockFor(crhCtx(), s.pools, 100, "set_vm_metadata/100", vmidLockTTLNow(),
				log.NewNopLogger(), func() error { return nil })
		}
	}
	res := onlyResult(t, s.run(t, ReplicaWidenOptions{}))
	if lockErr != nil {
		t.Fatalf("a call that waited on the VM's claim while the pass checked the VM failed: %v", lockErr)
	}
	if res.Outcome != ReplicaWidened || !s.widened() {
		t.Fatalf("result %+v, pin %q", res, s.pinNodes())
	}
}

// While the pass holds the claim, its lease is replicaWidenClaimTTL, and a
// call that arrives then gets the VM inside its own wait once the pass is
// done.
func TestReplicaWidenPass_ACallThatArrivesUnderTheClaimGetsTheVMInsideItsWait(t *testing.T) {
	s := newSweepRig(t)
	var expiry time.Time
	var claimedAt time.Time
	done := make(chan error, 1)
	s.onClaimed = func() {
		claimedAt = time.Now()
		expiry, _ = s.pools.expiry(100)
		go func() {
			done <- withVMIDLockFor(crhCtx(), s.pools, 100, "set_vm_metadata/100", vmidLockTTLNow(),
				log.NewNopLogger(), func() error { return nil })
		}()
	}
	res := onlyResult(t, s.run(t, ReplicaWidenOptions{}))
	if res.Outcome != ReplicaWidened {
		t.Fatalf("result %+v", res)
	}
	if expiry.IsZero() || expiry.After(claimedAt.Add(replicaWidenClaimTTL+time.Second)) {
		t.Fatalf("the pass's claim expires at %v, claimed at %v; want a lease of at most %v", expiry, claimedAt,
			replicaWidenClaimTTL)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the call that arrived under the pass's claim failed: %v", err)
		}
	case <-time.After(vmidLockTimeout + 5*time.Second):
		t.Fatal("the call that arrived under the pass's claim never got the VM")
	}
}

// A process that dies while it holds the pass's claim leaves a claim that a
// waiting call steals inside its own wait, so the VM's other calls do not
// fail for the claim's lease.
func TestReplicaWidenPass_AClaimLeftByADeadPassIsStolenInsideAWait(t *testing.T) {
	t.Parallel()
	s := newSweepRig(t)
	var left string
	s.onClaimed = func() { left, _ = s.pools.comment(100) }
	if res := onlyResult(t, s.run(t, ReplicaWidenOptions{})); res.Outcome != ReplicaWidened || left == "" {
		t.Fatalf("result %+v, claim %q", res, left)
	}
	// The pass's process dies with the claim it held.
	s.pools.mu.Lock()
	s.pools.pools[vmidLockPool(100)] = left
	s.pools.mu.Unlock()
	start := time.Now()
	err := withVMIDLockFor(crhCtx(), s.pools, 100, "set_vm_metadata/100", vmidLockTTLNow(), log.NewNopLogger(),
		func() error { return nil })
	if err != nil {
		t.Fatalf("a call waited %v on the dead pass's claim and failed: %v", time.Since(start), err)
	}
}

// With the rules lock on, the pass takes the VM's journal record, then the
// VM's claim, then the rules lock, each once, and it gives the rules lock up
// before it releases the claim, so the claim's pool calls never run while
// every other rule write in the cluster waits. A narrowing that waits on the
// rules lock writes after the pass, so its pin is what stays.
func TestReplicaWidenPass_TakesTheRealRulesLockInOrder(t *testing.T) {
	defer pve.SetClusterLockGraceForTest(0)()
	s := newSweepRig(t)
	s.deps.Config.ClusterLock = "pool"
	var mu sync.Mutex
	var order []string
	s.pools.onCreate = func(poolID string) {
		mu.Lock()
		defer mu.Unlock()
		switch poolID {
		case rulesLockPool():
			order = append(order, "rules")
		case vmidLockPool(100):
			order = append(order, "claim")
		default:
			order = append(order, poolID)
		}
	}
	var released []string
	s.pools.onDelete = func(poolID string) {
		mu.Lock()
		defer mu.Unlock()
		switch poolID {
		case rulesLockPool():
			released = append(released, "rules")
		case vmidLockPool(100):
			released = append(released, "claim")
		}
	}
	narrowed := time.Now().Truncate(time.Second)
	entered := make(chan bool, 1)
	finish := make(chan struct{})
	done := make(chan error, 1)
	s.onClaimed = func() {
		go func() {
			ctx := pve.WithClusterLockPollForTest(crhCtx(), 10*time.Millisecond)
			done <- withHARulesLock(ctx, s.deps, 100, log.NewNopLogger(), func(context.Context) error {
				entered <- s.widened()
				<-finish
				s.setPin("pve01", func(c string) string { return ha.WithReplicaChanged(c, narrowed) })
				return nil
			})
		}()
	}
	p := s.pass(ReplicaWidenOptions{})
	p.record = func(_ context.Context, _ int, _ map[string]any, fn func(*aj.Handle) error) error {
		mu.Lock()
		order = append(order, "record")
		mu.Unlock()
		return fn(nil)
	}
	report, err := p.run(crhCtx())
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	got := strings.Join(order, ",")
	gotReleased := strings.Join(released, ",")
	mu.Unlock()
	close(finish)
	if res := onlyResult(t, report); res.Outcome != ReplicaWidened {
		t.Fatalf("result %+v", res)
	}
	if !strings.HasPrefix(got, "record,claim,rules") {
		t.Fatalf("lock order %s, want the record, then the claim, then the rules lock", got)
	}
	if !strings.HasPrefix(gotReleased, "rules,claim") {
		t.Fatalf("release order %s, want the rules lock given up before the claim", gotReleased)
	}
	if sawWidened := <-entered; !sawWidened {
		t.Fatal("the narrowing ran inside the pass's hold of the rules lock")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if s.widened() || s.pinNodes() != "pve01" {
		t.Fatalf("pin %q %q; the narrowing that waited on the lock must stay", s.pinNodes(), s.pinComment())
	}
}

// A busy cluster answers every call in 800 milliseconds, and the rules lock
// pays its production grace. The pass still widens the pin inside its
// claim's lease, and it never reports the VM as failed for the time its
// locks took or for a write it made.
func TestReplicaWidenPass_ASlowClusterWidensWithoutAFailure(t *testing.T) {
	// The production grace, which TestMain shortens to nothing.
	defer pve.SetClusterLockGraceForTest(2500 * time.Millisecond)()
	s := newSweepRig(t)
	s.deps.Config.ClusterLock = "pool"
	s.slow(800 * time.Millisecond)
	res := onlyResult(t, s.run(t, ReplicaWidenOptions{}))
	if res.Outcome == ReplicaWidenFailed {
		t.Fatalf("result %+v; a slow cluster must not fail the VM", res)
	}
	if (res.Outcome == ReplicaWidened) != s.widened() {
		t.Fatalf("result %+v, pin %q %q; the report must say what the pass did to the pin", res, s.pinNodes(),
			s.pinComment())
	}
	if res.Outcome != ReplicaWidened {
		t.Fatalf("result %+v; at 800 milliseconds a call, the claim's lease covers the locks and the write", res)
	}
}

// A pass whose budget leaves no time to wait on a lock, once the lock's own
// margin is set aside, skips the VM and says why, rather than failing it or
// blaming a holder that does not exist.
func TestReplicaWidenPass_AShortBudgetSkipsRatherThanFails(t *testing.T) {
	s := newSweepRig(t)
	s.deps.Config.ClusterLock = "pool"
	ctx, cancel := context.WithTimeout(crhCtx(), 12*time.Second)
	defer cancel()
	report, err := s.pass(ReplicaWidenOptions{}).run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	res := onlyResult(t, report)
	if res.Outcome != ReplicaWidenSkipped || !strings.Contains(res.Reason, "time") || s.widened() {
		t.Fatalf("result %+v; want a skip that says the pass had too little time for its locks", res)
	}
	if strings.Contains(res.Reason, "another call") {
		t.Fatalf("result %+v; no other call holds a lock", res)
	}
}

// A rules lock another call holds is a skip at once, and the pass gives up
// its claim and waits for nothing.
func TestReplicaWidenPass_AHeldRulesLockSkipsAtOnce(t *testing.T) {
	defer pve.SetClusterLockGraceForTest(0)()
	s := newSweepRig(t)
	s.deps.Config.ClusterLock = "pool"
	s.pools.mu.Lock()
	s.pools.pools[rulesLockPool()] = "owner=create_vm/101@other exp=" + itoa(int(time.Now().Add(time.Hour).Unix()))
	s.pools.mu.Unlock()
	start := time.Now()
	res := onlyResult(t, s.run(t, ReplicaWidenOptions{}))
	if res.Outcome != ReplicaWidenSkipped || !strings.Contains(res.Reason, "HA rules lock") || s.widened() {
		t.Fatalf("result %+v", res)
	}
	if s.pools.held(100) || s.stub.updateCalls != 0 {
		t.Fatal("the pass left the VM claimed or wrote its pin without the rules lock")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("the pass waited %v on a held rules lock", elapsed)
	}
}

// A pass that finds no pool service claims nothing, widens nothing, and
// halts, since no VM can be claimed.
func TestReplicaWidenPass_NoPoolServiceHalts(t *testing.T) {
	s := newSweepRig(t)
	s.addPinnedVMs(101)
	s.deps.PVE = noPoolsClient{s.deps.PVE.(*sweepPoolsClient).attachRunClient}
	report := s.run(t, ReplicaWidenOptions{})
	if report.Halted == "" || report.Count(ReplicaWidened) != 0 || s.widened() {
		t.Fatalf("report %+v, pin %q; want a halt and no widening", report, s.pinNodes())
	}
	if s.stub.updateCalls != 0 {
		t.Fatal("the pass wrote a pin with no claim")
	}
}

// noPoolsClient is the rig's client with no pool service.
type noPoolsClient struct{ *attachRunClient }

func (noPoolsClient) Pools() pve.PoolService { return nil }

// A rebuild token that reaches the pin after the listing holds the pin on
// the source.
func TestReplicaWidenPass_RebuildTokenAfterTheListingHoldsThePin(t *testing.T) {
	s := newSweepRig(t)
	s.cluster.onResource = func(int) {
		s.setPin("pve01", func(c string) string {
			return ha.WithReplicaRebuild(ha.WithReplicaChanged(c, s.changed), "pve02")
		})
	}
	res := onlyResult(t, s.run(t, ReplicaWidenOptions{}))
	if res.Outcome != ReplicaWidenSkipped || !strings.Contains(res.Reason, "rebuild") || s.widened() {
		t.Fatalf("result %+v, pin %q", res, s.pinNodes())
	}
	if s.pools.held(100) || s.stub.updateCalls != 0 {
		t.Fatal("the pass wrote a pin that carries the rebuild token")
	}
}

// The check under the record reads the job's status again, and a status that
// no longer covers the change holds the pin.
func TestReplicaWidenPass_TheCheckReadsTheStatusAgain(t *testing.T) {
	s := newSweepRig(t)
	s.hooked.onStatus = func(call int) {
		if call == 2 {
			s.repl.status = ha.ReplicaSyncStatus{LastSync: s.changed.Add(-time.Minute)}
		}
	}
	res := onlyResult(t, s.run(t, ReplicaWidenOptions{}))
	if res.Outcome != ReplicaWidenSkipped || s.widened() || s.pools.creates != 0 {
		t.Fatalf("result %+v, claims %d", res, s.pools.creates)
	}
}

// A pin with no change time is stamped and not widened, and a dry run says
// so rather than that it would widen.
func TestReplicaWidenPass_UnstampedPin(t *testing.T) {
	unstamp := func(s *sweepRig) { s.setPin("pve01", nil) }
	t.Run("apply", func(t *testing.T) {
		s := newSweepRig(t)
		unstamp(s)
		res := onlyResult(t, s.run(t, ReplicaWidenOptions{}))
		if res.Outcome != ReplicaWidenSkipped || s.widened() {
			t.Fatalf("result %+v, pin %q", res, s.pinNodes())
		}
		if _, ok := ha.ReplicaChanged(s.pinComment()); !ok {
			t.Fatalf("pin comment %q; want it stamped", s.pinComment())
		}
	})
	t.Run("dry run", func(t *testing.T) {
		s := newSweepRig(t)
		unstamp(s)
		res := onlyResult(t, s.run(t, ReplicaWidenOptions{DryRun: true}))
		if res.Outcome != ReplicaWidenSkipped || !strings.Contains(res.Reason, "records no time") {
			t.Fatalf("result %+v; want a skip that says the pin needs a stamp first", res)
		}
		if _, ok := ha.ReplicaChanged(s.pinComment()); ok || s.stub.updateCalls != 0 {
			t.Fatal("a dry run stamped the pin")
		}
	})
}

// A pin another call widened after the listing is a skip, and it does not
// count toward the widening cap.
func TestReplicaWidenPass_AlreadyWidenedPinIsNotCounted(t *testing.T) {
	s := newSweepRig(t)
	s.addPinnedVMs(101)
	s.cluster.onResource = func(call int) {
		if call == 1 {
			s.setPin("pve01,pve02", func(c string) string {
				return ha.WithReplicaPartner(ha.WithReplicaChanged(c, s.changed), "pve02")
			})
		}
	}
	report := s.run(t, ReplicaWidenOptions{MaxWidened: 1})
	if len(report.Results) == 0 {
		t.Fatal("no results")
	}
	first := report.Results[0]
	if first.VMID != 100 || first.Outcome != ReplicaWidenSkipped || !strings.Contains(first.Reason, "already") {
		t.Fatalf("result %+v; want the already widened pin skipped", first)
	}
	if report.Count(ReplicaWidened) > 1 || report.Deferred != 0 {
		t.Fatalf("results %+v, deferred %d; a skip must not use the widening cap", report.Results, report.Deferred)
	}
}

// Two jobs of this Director for one VM fail the VM, since an operator must
// remove one.
func TestReplicaWidenPass_DuplicateJobsFail(t *testing.T) {
	s := newSweepRig(t)
	s.repl.addJob("100-1", "pve01", "pve03", renderReplicaJobMarker(aaDirector, 1))
	res := onlyResult(t, s.run(t, ReplicaWidenOptions{}))
	if res.Outcome != ReplicaWidenFailed || s.widened() {
		t.Fatalf("result %+v; want a failure an operator sees", res)
	}
	for _, want := range []string{"100-0", "100-1", "pvesr delete 100-1", "until an operator removes"} {
		if !strings.Contains(res.Reason, want) {
			t.Fatalf("reason %q; want it to name both jobs and how to remove the extra one (%q)", res.Reason, want)
		}
	}
}

// The log line for a VM with two jobs of this Director says that an operator
// must act, once for the sweep, and never that a later sweep fixes it.
func TestReplicaWidenPass_DuplicateJobsLogWhatTheOperatorMustDo(t *testing.T) {
	s := newSweepRig(t)
	s.repl.addJob("100-1", "pve01", "pve03", renderReplicaJobMarker(aaDirector, 1))
	report := s.run(t, ReplicaWidenOptions{})
	logger, obs := log.NewObservedLogger(log.LevelDebug)
	logReplicaWidenReport(logger, report)
	warns := 0
	for _, e := range obs.All() {
		if e.Level != log.LevelWarn {
			continue
		}
		warns++
		if strings.Contains(e.Message, "tries again") || !strings.Contains(e.Message, "operator") {
			t.Fatalf("warn %q; want it to say an operator must act, not that a later sweep fixes it", e.Message)
		}
		if reason, _ := e.Attrs["reason"].(string); !strings.Contains(reason, "pvesr delete 100-1") {
			t.Fatalf("warn reason %q; want the job to remove", reason)
		}
	}
	if warns != 1 {
		t.Fatalf("warns %d; want one for the VM", warns)
	}
}

// A Director UUID with surrounding spaces names the same Director in every
// step, so the pass widens.
func TestReplicaWidenPass_PaddedDirectorUUID(t *testing.T) {
	s := newSweepRig(t)
	s.deps.RequestDirectorUUID = "  " + aaDirector + "\n"
	res := onlyResult(t, s.run(t, ReplicaWidenOptions{}))
	if res.Outcome != ReplicaWidened || !s.widened() {
		t.Fatalf("result %+v, pin %q %q", res, s.pinNodes(), s.pinComment())
	}
}

// A sweep that panics is logged and recovered, and the call's own answer
// stands.
func TestWithReplicaWidenSweep_RecoversAPanic(t *testing.T) {
	s := newSweepRig(t)
	s.cluster.listPanic = "listing exploded"
	h := withReplicaWidenSweep(handlerFunc(func(ctx context.Context) (any, error) {
		return "answer", withHARulesLock(ctx, s.deps, 100, s.deps.Logger, func(context.Context) error { return nil })
	}))
	out, err := h.Handle(crhCtx(), nil, jsonrpc.Context{})
	if out != "answer" || err != nil {
		t.Fatalf("handler answer %v %v; want the call's own success", out, err)
	}
	if s.cluster.listCalls != 1 {
		t.Fatalf("list calls %d; want the sweep to have run", s.cluster.listCalls)
	}
}

// The sweep's budget leaves the releases after it inside the request's
// deadline, and a deadline too close for both skips the sweep.
func TestReplicaWidenSweepBudget(t *testing.T) {
	if budget, ok := replicaWidenSweepBudgetFor(context.Background()); !ok || budget != replicaWidenSweepBudget {
		t.Fatalf("no deadline: budget %v ok %v", budget, ok)
	}
	near, cancel := context.WithTimeout(context.Background(),
		replicaWidenReleaseReserve+2*replicaWidenSweepMinBudget-time.Second)
	defer cancel()
	if budget, ok := replicaWidenSweepBudgetFor(near); ok {
		t.Fatalf("near deadline: budget %v; want no sweep", budget)
	}
	// A budget that leaves the locks no time once their margin is set aside
	// is no sweep at all.
	short, cancelShort := context.WithTimeout(context.Background(), replicaWidenReleaseReserve+2*12*time.Second+
		time.Second)
	defer cancelShort()
	if budget, ok := replicaWidenSweepBudgetFor(short); ok {
		t.Fatalf("short deadline: budget %v; the locks' margin of %v leaves it no time to take a lock", budget,
			pve.ClusterLockContextMargin)
	}
	far, cancel2 := context.WithTimeout(context.Background(), replicaWidenReleaseReserve+2*replicaWidenSweepBudget+
		10*time.Second)
	defer cancel2()
	if budget, ok := replicaWidenSweepBudgetFor(far); !ok || budget != replicaWidenSweepBudget {
		t.Fatalf("far deadline: budget %v ok %v", budget, ok)
	}
	mid, cancel3 := context.WithTimeout(context.Background(), replicaWidenReleaseReserve+
		2*replicaWidenSweepMinBudget+20*time.Second)
	defer cancel3()
	budget, ok := replicaWidenSweepBudgetFor(mid)
	if dl, _ := mid.Deadline(); !ok || time.Now().Add(budget+replicaWidenReleaseReserve).After(dl) {
		t.Fatalf("mid deadline: budget %v ok %v; the sweep and the releases must end before the deadline",
			budget, ok)
	}

	s := newSweepRig(t)
	h := withReplicaWidenSweep(handlerFunc(func(ctx context.Context) (any, error) {
		return "answer", withHARulesLock(ctx, s.deps, 100, s.deps.Logger, func(context.Context) error { return nil })
	}))
	ctx, cancel4 := context.WithTimeout(crhCtx(), replicaWidenReleaseReserve+time.Second)
	defer cancel4()
	if out, err := h.Handle(ctx, nil, jsonrpc.Context{}); out != "answer" || err != nil {
		t.Fatalf("handler answer %v %v", out, err)
	}
	if s.cluster.listCalls != 0 {
		t.Fatal("the sweep ran with too little time left for it and its releases")
	}
}

// A pass whose context ends while it holds the rules lock and the claim, as
// pve-cid's does on an interrupt, stops and releases both.
func TestReplicaWidenPass_AnEndedContextReleasesTheLocks(t *testing.T) {
	defer pve.SetClusterLockGraceForTest(0)()
	s := newSweepRig(t)
	s.deps.Config.ClusterLock = "pool"
	ctx, cancel := context.WithCancel(crhCtx())
	defer cancel()
	s.onClaimed = cancel
	report, _ := s.pass(ReplicaWidenOptions{}).run(ctx)
	if report.Count(ReplicaWidened) != 0 || s.widened() {
		t.Fatalf("results %+v, pin %q; a pass whose context ended wrote the pin", report.Results, s.pinNodes())
	}
	if s.pools.held(100) {
		t.Fatal("the pass left VM 100 claimed")
	}
	s.pools.mu.Lock()
	_, rules := s.pools.pools[rulesLockPool()]
	s.pools.mu.Unlock()
	if rules {
		t.Fatal("the pass left the HA rules lock held")
	}
}

// lostAnswerCluster applies each rule write and then never answers it, as a
// PVE that applied the write and then stalled does, so the write ends with
// its context.
type lostAnswerCluster struct{ *sweepCluster }

func (c *lostAnswerCluster) UpdateHaRules(ctx context.Context, rule string, p *cluster.UpdateHaRulesParams) error {
	if err := c.sweepCluster.UpdateHaRules(ctx, rule, p); err != nil {
		return err
	}
	<-ctx.Done()
	return ctx.Err()
}

// ListHaRules fails on an ended context, as a real client's request does.
func (c *lostAnswerCluster) ListHaRules(ctx context.Context, p *cluster.ListHaRulesParams) (
	*cluster.ListHaRulesResponse, error,
) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return c.sweepCluster.ListHaRules(ctx, p)
}

// A pin write whose answer never comes after PVE applied it is reported as
// the widening it was, because the pass reads the pin again and finds its
// write.
func TestReplicaWidenPass_AWriteWhoseAnswerIsLostCountsWhenItLanded(t *testing.T) {
	s := newSweepRig(t)
	s.deps.PVE.(*sweepPoolsClient).attachRunClient.cluster = &lostAnswerCluster{sweepCluster: s.cluster}
	res := onlyResult(t, s.run(t, ReplicaWidenOptions{}))
	if !s.widened() {
		t.Fatalf("result %+v, pin %q %q; the write must have landed", res, s.pinNodes(), s.pinComment())
	}
	if res.Outcome != ReplicaWidened {
		t.Fatalf("result %+v; a write that landed is a widening, whatever its answer said", res)
	}
}

// A pass whose locks and reads use up its claim's window before the pin
// write skips the VM and writes nothing, rather than start a write it could
// not finish inside its lease.
func TestReplicaWidenPass_AClaimWindowThatRanShortSkipsBeforeTheWrite(t *testing.T) {
	s := newSweepRig(t)
	s.onClaimed = func() { time.Sleep(replicaWidenClaimTTL) }
	res := onlyResult(t, s.run(t, ReplicaWidenOptions{}))
	if s.widened() {
		t.Fatalf("result %+v; a pass past its claim's window must not write the pin", res)
	}
	if res.Outcome != ReplicaWidenSkipped || !strings.Contains(res.Reason, "claim") {
		t.Fatalf("result %+v; want a skip that says the claim ran short", res)
	}
}
