package handlers

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// Retirement of a parker the call emptied.
//
// A parker that PVE HA manages is destroyed, with its HA resource and its
// pin, in the same call that removes its last disk. Every path that takes a
// disk off a parker calls retireEmptiedParker once its own work on the disk is
// done: delete_disk (the unpark before the volume delete, and the deallocation
// of a disk the parker owns), the explicit and the retained-ephemeral disk
// cleanups of a managed allocation, attach_disk and every attach that runs
// through its guard (the unpark of a disk on another node, the reassignment
// from a parker on the VM's node, the config-edit fallback of that
// reassignment, and the migration of a disk off a parker of another node), and
// the rollback of a create_disk whose park had landed.
//
// The retirement is the one the parker pool sweep runs (parker_ha.go), under
// the same claim, from the same fresh read, and through the same teardown, so
// it destroys only a parker that is still an empty, stopped parker of this
// Director with no snapshot, whose HA resource carries this Director's marker.
// A parker that PVE HA does not manage stays when it empties, as it always
// has. The retirement never fails the call: a park or a sweep that holds the
// parker's claim, a request with too little time left, an HA stop that is
// still under way when the wait described below runs out, or any failed step
// leaves the parker for the next parker sweep on its node, which retires it.
//
// attach_disk and create_vm run the retirement after the handler returns
// from its work, not where the disk leaves the parker. A retirement can spend
// up to its whole budget on a slow HA stop or destroy, and inline it would do
// that while the call still held the node's in-flight slot, the disk's
// lifecycle lock, and the target VM's locks, so every request waiting on them
// would wait on the retirement too. The handler installs a queue in its
// context (deferParkerRetirements), each emptying path in it adds the parker
// to the queue, and the handler drains the queue once every gate and lock it
// took is released. The deferral is safe because the retirement decides
// nothing from what the path saw. It takes the parker's claim with a single
// try and reads the parker again under it, so a disk that was parked there in
// the meantime, or a rollback that put the disk back, keeps the parker. The
// other emptying paths retire inline, because they hold no gate that other
// requests queue on while they finish.
//
// A retirement the drain runs also waits, bounded, for PVE HA to finish the
// stop that the parker's registration requested (awaitRegistrationStop). A
// fresh deploy needs that wait, because create_disk parks the new disk and
// registers the parker as stopped, and attach_disk takes the disk out a few
// seconds later, while the CRM still reports request_stop. The teardown's HA
// gate counts request_stop as motion and defers on it, so without the wait
// every fresh deploy would leave an empty HA parker for the sweep. The wait
// runs before the retirement takes the parker's claim, so it holds no lock
// that a park waits on, and it runs only in the drain, after the handler
// released every gate and lock it took, so it holds up no other request
// either. An inline retirement reads the HA state once, as it always has,
// because the disk's lifecycle lock may still be held around it.
//
// A parker pool sweep that runs under the queue, as the one an attach runs
// after it parks a free-floating disk on the disk's own node does while it
// holds the disk's lifecycle lock, defers the same way. Its HA pass queues the
// retirement of the empty parker it would retire, and it queues its removal of
// the HA leftovers of gone parkers on the node, whose proof reads the quorum
// and probes every member under a claim. The drain runs the queued
// retirements first and then each node's leftover removal, as the sweep runs
// them inline.

// retireEmptiedParkerFunc is the shape of retireEmptiedParkerNow, named for
// the seam below.
type retireEmptiedParkerFunc func(ctx context.Context, deps Deps, node string, vmid int, trigger string)

// retireEmptiedParkerImpl holds the retirement every emptying path calls,
// which is retireEmptiedParkerNow in every process that is not running a
// test. It is a test seam, for the reason placeParkersInPoolImpl is one: a
// path that dropped the call would still succeed and still pass every
// assertion about the disk, so the only way to catch a missing retirement is
// to watch for the call. It is held in an atomic.Pointer for the reason that
// seam is.
var retireEmptiedParkerImpl atomic.Pointer[retireEmptiedParkerFunc]

func init() {
	production := retireEmptiedParkerFunc(retireEmptiedParkerNow)
	retireEmptiedParkerImpl.Store(&production)
	parkerRetireSettleWaitNs.Store(int64(parkerRetireSettleWaitDefault))
}

// parkerRetireSettleWaitDefault is how long a drained retirement waits for
// PVE HA to finish the stop that the parker's registration requested
// (awaitRegistrationStop). The CRM runs a round about every ten seconds, and
// the stop of a parker that never ran takes two or three of them. One sends
// request_stop to the node's LRM, which finds the parker already stopped, and
// a later one records stopped. Thirty seconds covers three rounds.
const parkerRetireSettleWaitDefault = 30 * time.Second

// parkerRetireSettleWaitNs holds the wait as nanoseconds in an atomic, so a
// test can shorten it race-safely.
var parkerRetireSettleWaitNs atomic.Int64

func parkerRetireSettleWait() time.Duration { return time.Duration(parkerRetireSettleWaitNs.Load()) }

// setRetireEmptiedParkerForTest replaces the retirement for the duration of a
// test and returns a restore function. The seam is process-wide, so tests
// using it must not call t.Parallel.
func setRetireEmptiedParkerForTest(fn retireEmptiedParkerFunc) func() {
	prev := retireEmptiedParkerImpl.Swap(&fn)
	return func() { retireEmptiedParkerImpl.Store(prev) }
}

// retireEmptiedParker retires parker vmid on node when the call has just
// taken its last disk off it and PVE HA manages it, as the comment above
// describes. trigger names the path that emptied the parker, for the log. It
// never returns an error, because the work it follows has already landed.
func retireEmptiedParker(ctx context.Context, deps Deps, node string, vmid int, trigger string) {
	if vmid <= 0 || strings.TrimSpace(node) == "" {
		return
	}
	if q := retirementQueueFrom(ctx); q != nil {
		q.add(queuedRetirement{deps: deps, node: node, vmid: vmid, trigger: trigger, keepGoing: detachedFromRequest(ctx)})
		return
	}
	(*retireEmptiedParkerImpl.Load())(ctx, deps, node, vmid, trigger)
}

// retirementQueueKey keys the queue deferParkerRetirements installs.
type retirementQueueKey struct{}

// drainedRetirementKey marks the context a queued retirement runs on, which
// carries none of the gates or locks the handler held.
type drainedRetirementKey struct{}

// drainedRetirement reports whether ctx is that of a retirement the drain of a
// handler's queue runs.
func drainedRetirement(ctx context.Context) bool {
	drained, _ := ctx.Value(drainedRetirementKey{}).(bool)
	return drained
}

// queuedRetirement is one retirement a handler runs once it has released its
// gates and locks.
type queuedRetirement struct {
	deps    Deps
	node    string
	vmid    int
	trigger string
	// keepGoing is set when the path called the retirement on a context free
	// of the request's cancellation (detachedFromRequest), as create_disk's
	// rollback and every caller on a detachedContext or finishingContext do,
	// so the drain runs it free of that cancellation too.
	keepGoing bool
}

// queuedOrphanScan is one node's removal of the HA leftovers of gone parkers
// that a sweep deferred (reconcileParkerOrphansNow).
type queuedOrphanScan struct {
	deps Deps
	node string
	cfg  pve.ParkerConfig
	// keepGoing is set as it is for a queuedRetirement.
	keepGoing bool
}

// retirementQueue holds the retirements a handler deferred, each parker once,
// and the leftover removals its sweeps deferred, each node once. An entry
// queued twice keeps its first form, which runs free of the request's
// cancellation when either caller's context was.
type retirementQueue struct {
	mu      sync.Mutex
	pending []queuedRetirement
	scans   []queuedOrphanScan
}

// retirementQueueFrom returns the queue deferParkerRetirements installed in
// ctx, or nil.
func retirementQueueFrom(ctx context.Context) *retirementQueue {
	q, _ := ctx.Value(retirementQueueKey{}).(*retirementQueue)
	return q
}

func (q *retirementQueue) add(e queuedRetirement) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i := range q.pending {
		if q.pending[i].node == e.node && q.pending[i].vmid == e.vmid {
			q.pending[i].keepGoing = q.pending[i].keepGoing || e.keepGoing
			return
		}
	}
	q.pending = append(q.pending, e)
}

// holds reports whether the queue holds a retirement of parker vmid on node.
func (q *retirementQueue) holds(node string, vmid int) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i := range q.pending {
		if q.pending[i].node == node && q.pending[i].vmid == vmid {
			return true
		}
	}
	return false
}

func (q *retirementQueue) addOrphanScan(e queuedOrphanScan) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i := range q.scans {
		if q.scans[i].node == e.node {
			q.scans[i].keepGoing = q.scans[i].keepGoing || e.keepGoing
			return
		}
	}
	q.scans = append(q.scans, e)
}

func (q *retirementQueue) take() ([]queuedRetirement, []queuedOrphanScan) {
	q.mu.Lock()
	defer q.mu.Unlock()
	pending, scans := q.pending, q.scans
	q.pending, q.scans = nil, nil
	return pending, scans
}

// deferParkerRetirements makes every retirement of an emptied parker called
// on the returned context wait until the returned drain runs, as the comment
// at the top of this file describes. The handler defers the drain first, so
// it runs after every gate and lock the handler takes later is released. A
// context that already carries a queue keeps it, and the drain returned then
// does nothing, so the outermost handler drains. The drain runs each
// retirement, and then each node's deferred leftover removal, on ctx, the
// context the handler had before the queue, which carries none of the claims
// or locks the path held, and no queue that a sweep could defer to again. Each
// retirement's context is marked as drained, which lets it wait for PVE HA to
// finish stopping the parker.
func deferParkerRetirements(ctx context.Context) (context.Context, func()) {
	if q := retirementQueueFrom(ctx); q != nil {
		return ctx, func() {}
	}
	q := &retirementQueue{}
	runCtx := func(keepGoing bool) context.Context {
		if keepGoing {
			return context.WithoutCancel(ctx)
		}
		return ctx
	}
	return context.WithValue(ctx, retirementQueueKey{}, q), func() {
		pending, scans := q.take()
		for i := range pending {
			e := &pending[i]
			rctx := context.WithValue(runCtx(e.keepGoing), drainedRetirementKey{}, true)
			(*retireEmptiedParkerImpl.Load())(rctx, e.deps, e.node, e.vmid, e.trigger)
		}
		for i := range scans {
			e := &scans[i]
			reconcileParkerOrphansNow(runCtx(e.keepGoing), e.deps, e.node, e.cfg)
		}
	}
}

// retireEmptiedHolder retires the parker a disk left, when holder names one.
func retireEmptiedHolder(ctx context.Context, deps Deps, holder pve.DiskHolder, trigger string) {
	if !holder.Found || !holder.IsParker {
		return
	}
	retireEmptiedParker(ctx, deps, holder.Node, holder.VMID, trigger)
}

// retireEmptiedParkerNow is the production retirement. It decides from reads
// that take no lock first, so a parker that still holds something, or that
// PVE HA does not manage, costs one read of its HA resource and one of its
// config. A parker the reads find empty and registered is retired under its
// claim and a retirement's own budget, exactly as the sweep retires it,
// including the fresh read under the claim that keeps a parker a park reached
// in the meantime. A retirement the drain runs first waits for PVE HA to finish
// a stop the parker's registration requested.
func retireEmptiedParkerNow(ctx context.Context, deps Deps, node string, vmid int, trigger string) {
	if deps.Config == nil {
		return
	}
	r, ok := newParkerHAPass(ctx, deps, node, parkerReadConfigFor(deps))
	if !ok {
		return
	}
	r.retireOnly = true
	logger := r.logger.With(log.Int("parker_vmid", vmid), log.String("node", node), log.String("trigger", trigger))
	r.logger = logger

	rctx, cancel := context.WithTimeout(r.base, parkerHAReconcileTimeout)
	defer cancel()
	snap, err := r.read(rctx, vmid)
	if err != nil {
		logger.Info("parker HA: the emptied parker could not be read; the next parker sweep on its node retires it "+
			"if it is still empty", log.Err(err))
		return
	}
	if r.action(vmid, snap) != parkerHARetire {
		logger.Info("parker HA: the parker the call emptied is not one PVE HA manages for this Director, or it " +
			"holds something still, so it stays")
		return
	}
	if !r.loadProfile(rctx) {
		return
	}
	if drainedRetirement(ctx) && !r.awaitRegistrationStop(vmid) {
		return
	}
	if err := r.retireWithBudget(vmid); err != nil {
		logger.Info("parker HA: the emptied parker was not retired; the next parker sweep on its node retires it",
			log.Err(err))
	}
}

// awaitRegistrationStop waits while PVE HA is still carrying out the stop that
// the parker's registration requested, and reports whether the retirement goes
// on. It holds no claim and no lock while it waits, so a park into the parker
// goes ahead, and the read under the claim that follows then finds the disk
// and keeps the parker.
//
// The wait covers one state only. That state is a resource that requests
// stopped while the CRM reports request_stop or has not processed it yet.
// Every other answer, a read that failed included, ends the wait at once and leaves the decision
// to the teardown's HA gate, which reads the state again under the claim and
// defers on anything that does not let the destroy proceed, as it always has.
//
// The wait lasts parkerRetireSettleWait at most, cut short so the request
// still has a retirement's budget left after it, and it never starts when the
// request has less than that left. A stop that outlasts it, or a request that
// ends during it, leaves the parker as it was, with nothing written, and is
// logged with the reason. The parker is then still an empty parker that PVE HA
// manages, so the parker pool sweep that the next park on its node runs
// retires it, unless that park reuses it.
func (r *parkerHA) awaitRegistrationStop(vmid int) bool {
	ctx := r.base
	if ctx == nil {
		ctx = context.Background()
	}
	budget := parkerRetireSettleWait()
	deadline, _ := haDeleteWaitLimit(ctx, budget, parkerRetireBudget())
	if !time.Now().Before(deadline) {
		return true
	}
	start := time.Now()
	waiting := false
	for {
		st, err := readHADeleteState(ctx, r.deps, parkerRetireCaller, vmid, r.node)
		if err != nil {
			return true
		}
		disp, reason := st.Disposition()
		if !parkerStopInProgress(st, disp) {
			if waiting {
				r.logger.Info("parker HA: PVE HA finished stopping the emptied parker, so the retirement goes on",
					log.String("raw_state", st.Raw), log.String("waited", time.Since(start).Round(time.Second).String()))
			}
			return true
		}
		fields := []log.Field{
			log.String("sid", st.SID), log.String("raw_state", st.Raw), log.String("requested", st.Requested),
			log.String("reason", reason), log.String("waited", time.Since(start).Round(time.Second).String()),
			log.String("wait", budget.String()),
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			r.logger.Info("parker HA: PVE HA was still stopping the emptied parker when the wait for the stop ran "+
				"out, so it stays; the next parker sweep on its node retires it", fields...)
			return false
		}
		if !waiting {
			waiting = true
			r.logger.Info("parker HA: PVE HA is still stopping the emptied parker, as its registration asked, so the "+
				"retirement waits for the stop", fields...)
		}
		if err := sleepOrDone(ctx, min(haDeleteMotionPoll(), remaining)); err != nil {
			r.logger.Info("parker HA: the request ended while the retirement waited for PVE HA to stop the emptied "+
				"parker, so it stays; the next parker sweep on its node retires it", append(fields, log.Err(err))...)
			return false
		}
	}
}

// parkerStopInProgress reports whether st shows the stop that a parker's
// registration requested still under way. That is a resource that requests
// stopped while the CRM reports request_stop or has not processed it yet,
// which the state table answers with a wait for motion.
func parkerStopInProgress(st *pve.HAServiceState, disp pve.HADisposition) bool {
	return disp == pve.HAWaitForMotion && st.Requested == parkerHAStateStopped &&
		(st.Raw == haRawStateRequestStop || !st.InCRM)
}
