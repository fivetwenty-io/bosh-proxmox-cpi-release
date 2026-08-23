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
