package handlers

import (
	"context"
	"strings"
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
	(*retireEmptiedParkerImpl.Load())(ctx, deps, node, vmid, trigger)
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
