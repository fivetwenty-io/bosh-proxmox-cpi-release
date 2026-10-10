package handlers

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// placeParkersInPoolFunc is the shape of pve.PlaceParkersInPool, named so the
// seam below can hold it in an atomic pointer and so a test can declare a
// replacement without restating the parameter list.
type placeParkersInPoolFunc func(
	ctx context.Context,
	c pve.Client,
	logger *log.Logger,
	node string,
	cfg pve.ParkerConfig,
) error

// placeParkersInPoolImpl holds the pool placement sweep every park funnel runs,
// which is pve.PlaceParkersInPool in every process that is not running a test.
// It is a test seam rather than an abstraction over placement, and production
// never swaps it.
//
// It exists because the sweep is best-effort by design, so a funnel that
// dropped the call would still return success, still park the disk, and still
// pass every assertion about the park itself. The only way to catch a missing
// sweep is to watch for the call.
//
// It is held in an atomic.Pointer for the reason parkDiskImpl is, which is that
// a plain package var swapped by a test is an unsynchronized write the race
// detector is entitled to flag.
var placeParkersInPoolImpl atomic.Pointer[placeParkersInPoolFunc]

func init() {
	production := placeParkersInPoolFunc(pve.PlaceParkersInPool)
	placeParkersInPoolImpl.Store(&production)
}

// sweepParkerPool places every parker on node that carries cfg's own prefix
// into the configured parker pool, after a park or a transfer has already
// succeeded. A parker of another prefix is left where it is. Callers discard
// the outcome, because pool membership is cosmetic and a park that has landed
// is never taken back for it, so the sweep's own warnings are the operator's
// record and this line is only a marker for the request log.
//
// It always passes the unguarded client, which unguardedPVE finds by walking
// deps.PVE out of whatever allocation guard wraps it. No call for our pool may
// run through a managed allocation guard, because the guard's admission hook
// refuses every pool outside bosh-lock- and a refusal poisons the whole
// allocation. The funnels cannot do that unwrapping themselves, because the
// managed detach and attach paths shadow their own deps with a guarded copy and
// a funnel cannot tell which of the two it was handed, so it happens here once
// on behalf of every one of them.
//
// The same pass deletes the per-disk transfer locks whose claims have expired
// (sweepExpiredDiskLocks), so a disk that never transfers again doesn't keep
// the sentinel a crashed transfer left.
func sweepParkerPool(ctx context.Context, deps Deps, node string, cfg pve.ParkerConfig) {
	logger := deps.Log(ctx)
	client := unguardedPVE(deps.PVE)
	if err := (*placeParkersInPoolImpl.Load())(ctx, client, logger, node, cfg); err != nil {
		logger.Debug("the parker pool sweep reported failures; the parkers it could not place stay where they are",
			log.String("pool", cfg.Pool),
			log.String("node", node),
			log.Err(err),
		)
	}
	sweepExpiredDiskLocks(ctx, client, logger)
}

// sweepExpiredDiskLocksFunc is the shape of pve.SweepExpiredDiskTransferLocks,
// named for the seam below.
type sweepExpiredDiskLocksFunc func(ctx context.Context, c pve.Client, logger *log.Logger) (int, error)

// sweepExpiredDiskLocksImpl holds the expired per-disk lock sweep, which is
// pve.SweepExpiredDiskTransferLocks in every process that is not running a
// test. It is held in an atomic.Pointer for the reason placeParkersInPoolImpl
// is.
var sweepExpiredDiskLocksImpl atomic.Pointer[sweepExpiredDiskLocksFunc]

func init() {
	production := sweepExpiredDiskLocksFunc(pve.SweepExpiredDiskTransferLocks)
	sweepExpiredDiskLocksImpl.Store(&production)
}

// expiredDiskLockSweepTimeout bounds the expired per-disk lock sweep, so a
// slow pool listing never holds up the request whose park has already landed.
const expiredDiskLockSweepTimeout = 15 * time.Second

// sweepExpiredDiskLocks runs the expired per-disk lock sweep on client, which
// must be the unguarded client for the reason sweepParkerPool gives. It is
// best-effort like the pool placement, so every failure is only logged.
func sweepExpiredDiskLocks(ctx context.Context, client pve.Client, logger *log.Logger) {
	sweepCtx, cancel := context.WithTimeout(ctx, expiredDiskLockSweepTimeout)
	defer cancel()
	if _, err := (*sweepExpiredDiskLocksImpl.Load())(sweepCtx, client, logger); err != nil {
		logger.Debug("the expired per-disk lock sweep reported failures; the sentinels it could not delete stay until the next sweep",
			log.Err(err),
		)
	}
}

// setSweepExpiredDiskLocksForTest replaces the expired per-disk lock sweep for
// the duration of a test and returns a restore function. Like
// setPlaceParkersInPoolForTest, the seam is process-wide, so tests using it
// must not call t.Parallel.
func setSweepExpiredDiskLocksForTest(fn sweepExpiredDiskLocksFunc) func() {
	prev := sweepExpiredDiskLocksImpl.Swap(&fn)
	return func() { sweepExpiredDiskLocksImpl.Store(prev) }
}

// setPlaceParkersInPoolForTest replaces the sweep for the duration of a test
// and returns a restore function.
//
// The swap itself is race-safe, but the seam is still process-wide, so two
// tests that swap it at the same time would take each other's replacement.
// Tests using it must not call t.Parallel, exactly as the tests using
// setParkDiskForTest must not.
//
//	defer setPlaceParkersInPoolForTest(fn)()
func setPlaceParkersInPoolForTest(fn placeParkersInPoolFunc) func() {
	prev := placeParkersInPoolImpl.Swap(&fn)
	return func() { placeParkersInPoolImpl.Store(prev) }
}
