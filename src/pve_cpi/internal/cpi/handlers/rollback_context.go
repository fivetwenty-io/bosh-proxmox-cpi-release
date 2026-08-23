package handlers

import (
	"context"
	"time"
)

// rollbackCleanupTimeout bounds handler-level rollback/cleanup work that runs
// on a cancellation-detached context. Detaching from the request ctx is
// correct — cleanup must survive the caller's cancellation or deadline — but
// without re-imposing a deadline the SDK's 30-minute HTTP timeout multiplied
// by retry budgets lets a single rollback hold in-flight semaphore slots and
// Director workers for hours against an unresponsive node. Five minutes
// covers the realistic cleanup shapes: the common case is a destroy call
// plus a task await, but cleanupVM's foreign-disk protection can add config
// reads, per-disk detaches, and — for stable-ID disks — a full parker
// transfer with its own move_disk task await per disk. Exhausting the bound
// fails closed (the purge is refused and the VM preserved), so the cost of a
// too-small budget is unnecessary orphans, not data loss.
const rollbackCleanupTimeout = 5 * time.Minute

// sdnCleanupTimeout bounds delete_network's SDN teardown, which detaches from
// the request ctx so a cancelled caller cannot leave half-applied SDN state.
// Wider than rollbackCleanupTimeout because the teardown is a sequence — one
// delete per subnet, an apply with task await, and a conditional zone
// removal — but still far under both the delete-class operation-timeout
// budget and the SDK's 30-minute transport timeout, so a quorum-less cluster
// stalls this handler for minutes, not hours.
const sdnCleanupTimeout = 5 * time.Minute

// lockReleaseTimeout bounds the detached context cluster-lock releases run
// on. A release is one DeletePool call; 30 seconds absorbs transient
// slowness without letting a dead cluster hold the deferred release open.
const lockReleaseTimeout = 30 * time.Second

// cleanupSweepMaxAttempts bounds the RetryOnTransientOrLock budget on
// best-effort cleanup sweeps (orphan-volume deletes, rollback volume
// removal) and on the best-effort pool operations that run on the
// Director-facing request path (delete_vm's empty-pool reaper,
// set_vm_metadata's pool-move reconcile). These operations typically fire
// right after a storage fault, into the same contended lock, so a
// single-shot call almost always loses the race it was born into; three
// attempts ride the lock backoff curve long enough to outlast a worker
// recycle or a lock hand-off without letting a dead storage backend consume
// the whole detached-context budget that the primary rollback work
// (destroys, task awaits) also draws from, and without absorbing minutes of
// backoff on a request path for an outcome that is cosmetic by contract.
const cleanupSweepMaxAttempts = 3

// rollbackDestroyMaxAttempts bounds the DeleteQemu retry inside cleanupVM.
// The destroy is the most load-bearing cleanup (an unreaped VM is a
// permanent leak), so it gets more attempts than a cosmetic sweep, but not
// the full ten-attempt storage-lock budget: the whole rollback (stop,
// foreign-disk protection, destroy, task await, ISO removal, HA pin
// removal) shares one detached context bounded by rollbackCleanupTimeout,
// and a full lock curve on the destroy alone (~124s of backoff plus ten
// slow lock-wait round trips) could exhaust it and cascade failures into
// the tail steps. Five attempts (~16s of backoff) retain the retry value
// against a lock hand-off while leaving the tail its share of the budget.
const rollbackDestroyMaxAttempts = 5

// detachedContext returns a context that survives parent cancellation but is
// bounded by d, plus the cancel the caller must defer. It replaces bare
// context.WithoutCancel for cleanup paths: detachment without a bound turns
// every hung PVE call into an unbounded stall (see rollbackCleanupTimeout).
func detachedContext(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	base := context.Background()
	if parent != nil {
		base = context.WithoutCancel(parent)
	}
	return context.WithTimeout(base, d)
}
