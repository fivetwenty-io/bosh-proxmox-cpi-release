package pve

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestAcquireClusterLock_WaitSkipsTheCreateWhileHeld pins that a waiting
// acquire reads the sentinel before each poll and sends no create while
// another owner's live claim stands. PVE takes its cluster-wide user.cfg lock
// for every create it refuses, so a waiter that created on every poll would
// load that lock for the whole wait. The waiter makes one create on arrival
// and one once the holder lets go.
func TestAcquireClusterLock_WaitSkipsTheCreateWhileHeld(t *testing.T) {
	t.Parallel()
	const holdFor = 20 * time.Second
	pools, clk, _ := heldParkerSentinel(holdFor)
	h, err := acquireClusterLockWithClock(context.Background(), pools, "vm-90000", "waiter", time.Minute,
		time.Minute, clk.clock())
	if err != nil || h == nil {
		t.Fatalf("handle=%v err=%v, want the lock once the holder let go", h, err)
	}
	if elapsed := time.Duration(clk.offset.Load()); elapsed < holdFor {
		t.Fatalf("acquired after %s, before the holder let go at %s", elapsed, holdFor)
	}
	if pools.createN != 2 {
		t.Fatalf("the waiter sent %d creates, want 2, one on arrival and one after the release; calls %v",
			pools.createN, pools.calls)
	}
}

// TestAcquireClusterLock_WaitThatRunsOutWhileHeldSendsOneCreate runs a wait
// out against a holder that never leaves. Only the arrival's create is sent,
// and the give-up is the retriable timeout.
func TestAcquireClusterLock_WaitThatRunsOutWhileHeldSendsOneCreate(t *testing.T) {
	t.Parallel()
	pools, clk, _ := heldParkerSentinel(time.Hour)
	_, err := acquireClusterLockWithClock(context.Background(), pools, "vm-90000", "waiter", time.Minute,
		30*time.Second, clk.clock())
	if !errors.Is(err, ErrClusterLockTimeout) || !errors.Is(err, errClusterLockStillHeld) {
		t.Fatalf("err = %v, want the timeout saying the holder still held the lock", err)
	}
	if pools.createN != 1 {
		t.Fatalf("the waiter sent %d creates, want only the one on arrival; calls %v", pools.createN, pools.calls)
	}
}

// TestAcquireClusterLock_WaitStillStealsAnExpiredClaim pins that the read
// before a poll never stops a steal. A holder whose claim expires during the
// wait is read as no longer live, so the poll sends its create, finds the
// sentinel, and steals the expired claim.
func TestAcquireClusterLock_WaitStillStealsAnExpiredClaim(t *testing.T) {
	t.Parallel()
	pools := newFakeLockPools()
	clk := &fakeLockClock{base: time.Now()}
	key := ClusterLockPoolName("vm-90000")
	pools.pools[key] = encodeLockComment("crashed", clk.base.Add(10*time.Second))
	h, err := acquireClusterLockWithClock(context.Background(), pools, "vm-90000", "waiter", time.Minute,
		time.Minute, clk.clock())
	if err != nil || h == nil {
		t.Fatalf("handle=%v err=%v, want the expired claim stolen", h, err)
	}
	if owner, _ := decodeLockOwner(pools.pools[key]); owner != "waiter" {
		t.Fatalf("sentinel %q, want the waiter's claim", pools.pools[key])
	}
	if elapsed := time.Duration(clk.offset.Load()); elapsed < 9*time.Second || elapsed > 15*time.Second {
		t.Fatalf("stole after %s, want soon after the claim expired at 10s", elapsed)
	}
}
