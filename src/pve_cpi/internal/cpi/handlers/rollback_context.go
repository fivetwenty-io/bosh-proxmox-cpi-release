package handlers

import (
	"context"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// rollbackCleanupTimeout bounds handler-level rollback/cleanup work that runs
// on a cancellation-detached context. Detaching from the request ctx is
// correct — cleanup must survive the caller's cancellation or deadline — but
// without re-imposing a deadline the SDK's 30-minute HTTP timeout multiplied
// by retry budgets lets a single rollback hold in-flight semaphore slots and
// Director workers for hours against an unresponsive node.
//
// Everything cleanupVM does draws on this one context, and the worst case
// stacks: the stop call and its task await (up to the 300s AwaitTask
// default), foreign-disk protection with config reads, per-disk detaches,
// and — for stable-ID disks — a full parker transfer with its own move_disk
// task await per disk, the primary destroy's retry budget
// (rollbackDestroyMaxAttempts, ~16s of backoff), the skiplock or
// lock-clear recovery (awaitVMConfigLockClear waits up to
// vmLockClearMaxWait = 120s, and the follow-up skiplock or lock-clear
// destroy carries its own rollbackDestroyMaxAttempts budget, another ~16s
// each), the destroy's task await, then ISO removal and HA-pin removal. Ten minutes gives that
// stack headroom in the common shapes while still bounding a dead node to
// minutes, not hours; the pathological worst case (multiple full task
// awaits back to back) can still exhaust it, and exhausting the bound fails
// closed (the purge is refused, the VM preserved and tagged), so the cost
// of a too-small budget is unnecessary orphans, not data loss.
const rollbackCleanupTimeout = 10 * time.Minute

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

// finishingContext returns the context for work that must finish once it has
// started, such as a create_vm rollback's VM disposal or a disk operation's
// completion audit, plus the cancel the caller must defer. Under a request
// deadline the work keeps the request's values, and it gets at least budget
// from now even when the deadline is nearer or has already passed. A deadline
// further out than budget still applies in full, so the work never gets less
// time than the request had left. The work is detached from the request's
// cancellation, so a deadline that arrives partway through cannot cut it off
// and leave a record that refuses the retry.
//
// A request without a deadline has no deadline to outlive, so the work runs on
// the request's own context, even when a stop signal has already cancelled it.
// That is the case for every CPI call when operation_timeout is off, so the
// work keeps the same unbounded time it has always had there, and a stop
// signal still stops it.
func finishingContext(parent context.Context, budget time.Duration) (context.Context, context.CancelFunc) {
	if parent == nil {
		return context.WithTimeout(context.Background(), budget)
	}
	deadline, bounded := parent.Deadline()
	if !bounded {
		return parent, func() {}
	}
	if left := time.Until(deadline); left > budget {
		budget = left
	}
	return detachedContext(parent, budget)
}

// closingContext is finishingContext with pve.ClusterLockCompletionAllowance
// for completion work that has always run after the request ended, such as the
// audit that returns a disk after a clean lock timeout or delete_vm's readback
// after a retention lock timeout. That work already ran detached for the
// allowance whenever the request had ended, so a request without a deadline
// that a stop signal has cancelled still gets the allowance here, the way it
// did before finishingContext existed.
func closingContext(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := finishingContext(parent, pve.ClusterLockCompletionAllowance)
	if ctx.Err() == nil {
		return ctx, cancel
	}
	cancel()
	return detachedContext(parent, pve.ClusterLockCompletionAllowance)
}
