package handlers

import (
	"context"
	"testing"
)

// claimsTaken reports how many VMID claims the rig's lock pools created.
func claimsTaken(p *exclusiveLockPools) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.creates
}

// TestReconcileParkerHA_UnderAQueueDefersTheRetirement proves the parker HA
// pass of a sweep that a handler runs under its retirement queue, as an
// attach's sweep does while it holds the disk's lifecycle lock, takes no claim
// and retires nothing inline, and that the queue's drain retires the empty
// parker afterward.
func TestReconcileParkerHA_UnderAQueueDefersTheRetirement(t *testing.T) {
	r := newEmptiedParkerRig()
	qctx, drain := deferParkerRetirements(context.Background())
	reconcileParkerHA(qctx, r.deps, phNode, phParkerCfg())
	r.requireUntouched(t)
	if n := claimsTaken(r.client.pools()); n != 0 {
		t.Fatalf("the sweep under a queue must take no claim before the drain; claims %d", n)
	}
	drain()
	r.requireRetired(t)
}

// TestReconcileParkerHA_UnderAQueueDefersTheLeftoverRemoval proves the same
// pass runs none of the proof that removes a gone parker's HA leftovers
// inline, and that the queue's drain proves and removes them afterward.
func TestReconcileParkerHA_UnderAQueueDefersTheLeftoverRemoval(t *testing.T) {
	r := newOrphanRig()
	qctx, drain := deferParkerRetirements(context.Background())
	reconcileParkerHA(qctx, r.deps, phNode, phParkerCfg())
	r.requireKept(t, true, true)
	if n := claimsTaken(r.client.pools()); n != 0 {
		t.Fatalf("the sweep under a queue must take no claim before the drain; claims %d", n)
	}
	drain()
	r.requireRemoved(t, true)
}

// TestReconcileParkerHA_WithoutAQueueStillRetiresInline proves a sweep that
// no handler queues, such as the one a detach runs, still retires the empty
// parker and removes the leftovers itself.
func TestReconcileParkerHA_WithoutAQueueStillRetiresInline(t *testing.T) {
	r := newEmptiedParkerRig()
	reconcileParkerHA(context.Background(), r.deps, phNode, phParkerCfg())
	r.requireRetired(t)

	o := newOrphanRig()
	o.sweep(phParkerCfg())
	o.requireRemoved(t, true)
}
