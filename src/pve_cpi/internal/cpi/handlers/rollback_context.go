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
// Director workers for hours against an unresponsive node. Two minutes
// mirrors the dispatcher middleware's rollback bound (internal/cpi
// middleware.go) and comfortably covers the realistic cleanup shapes: a
// destroy call plus a task await, or a volume delete plus await.
const rollbackCleanupTimeout = 2 * time.Minute

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
