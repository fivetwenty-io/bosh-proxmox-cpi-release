package handlers

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"

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
// parker's claim, a request with too little time left, or any failed step
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
}

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
	if q, ok := ctx.Value(retirementQueueKey{}).(*retirementQueue); ok && q != nil {
		q.add(queuedRetirement{deps: deps, node: node, vmid: vmid, trigger: trigger, keepGoing: ctx.Done() == nil})
		return
	}
	(*retireEmptiedParkerImpl.Load())(ctx, deps, node, vmid, trigger)
}

// retirementQueueKey keys the queue deferParkerRetirements installs.
type retirementQueueKey struct{}

// queuedRetirement is one retirement a handler runs once it has released its
// gates and locks.
type queuedRetirement struct {
	deps    Deps
	node    string
	vmid    int
	trigger string
	// keepGoing is set when the path called the retirement on a context free
	// of the request's cancellation, as create_disk's rollback does, so the
	// drain runs it free of that cancellation too.
	keepGoing bool
}

// retirementQueue holds the retirements a handler deferred, each parker once.
type retirementQueue struct {
	mu      sync.Mutex
	pending []queuedRetirement
}

func (q *retirementQueue) add(e queuedRetirement) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i := range q.pending {
		if q.pending[i].node == e.node && q.pending[i].vmid == e.vmid {
			return
		}
	}
	q.pending = append(q.pending, e)
}

func (q *retirementQueue) take() []queuedRetirement {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := q.pending
	q.pending = nil
	return out
}

// deferParkerRetirements makes every retirement of an emptied parker called
// on the returned context wait until the returned drain runs, as the comment
// at the top of this file describes. The handler defers the drain first, so
// it runs after every gate and lock the handler takes later is released. A
// context that already carries a queue keeps it, and the drain returned then
// does nothing, so the outermost handler drains. The drain runs each
// retirement on ctx, the context the handler had before the queue, which
// carries none of the claims or locks the path held.
func deferParkerRetirements(ctx context.Context) (context.Context, func()) {
	if q, ok := ctx.Value(retirementQueueKey{}).(*retirementQueue); ok && q != nil {
		return ctx, func() {}
	}
	q := &retirementQueue{}
	return context.WithValue(ctx, retirementQueueKey{}, q), func() {
		pending := q.take()
		for i := range pending {
			e := &pending[i]
			rctx := ctx
			if e.keepGoing {
				rctx = context.WithoutCancel(ctx)
			}
			(*retireEmptiedParkerImpl.Load())(rctx, e.deps, e.node, e.vmid, e.trigger)
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
// in the meantime.
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
		logger.Debug("parker HA: the parker the call emptied is not one PVE HA manages for this Director, or it " +
			"holds something still, so it stays")
		return
	}
	if !r.loadProfile(rctx) {
		return
	}
	if err := r.retireWithBudget(vmid); err != nil {
		logger.Info("parker HA: the emptied parker was not retired; the next parker sweep on its node retires it",
			log.Err(err))
	}
}
