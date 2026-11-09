package pve

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// listingSendByWire is the send-by wire in front of a pool store that also
// offers the pool listing the expired-lock sweep reads.
type listingSendByWire struct {
	*sendByWire
	lister PoolCommentLister
}

func (l *listingSendByWire) ListPoolComments(ctx context.Context) (map[string]string, error) {
	return l.lister.ListPoolComments(ctx)
}

// TestSweepExpiredLockSentinel_SendByPassedLeavesTheSentinel refuses the
// sweep's delete for its send-by deadline. None of it reached PVE, so the
// sweep reports that it deleted nothing and returns no error, rather than
// wrapping the refusal into its own error. The sentinel is left for the next
// sweep.
func TestSweepExpiredLockSentinel_SendByPassedLeavesTheSentinel(t *testing.T) {
	t.Parallel()
	start := time.Unix(10_000, 0)
	name := ClusterLockPoolName(diskTransferLockName("expired"))
	expired := encodeLockComment("crashed", start.Add(-time.Hour))
	f := newFakeLockPools()
	f.pools[name] = expired
	clk := fixedClock(start, time.Second)
	// The delete arrives exactly at its deadline, which counts as passed.
	wire := &sendByWire{PoolService: f, now: func() time.Time { return clk.now().Add(clusterLockStealBudget) }}

	deleted, err := sweepExpiredLockSentinel(context.Background(), wire, name, expired, clk)
	if deleted || err != nil {
		t.Fatalf("sweepExpiredLockSentinel = (%v, %v), want (false, nil) for a delete refused by its send-by deadline", deleted, err)
	}
	deletes := wire.deletes()
	if len(deletes) != 1 || !deletes[0].refused {
		t.Fatalf("want one refused delete, got %+v", deletes)
	}
	if want := start.Add(clusterLockStealBudget); !deletes[0].deadline.Equal(want) {
		t.Fatalf("the sweep's send-by deadline is %v, want the re-read's start plus the steal budget (%v)", deletes[0].deadline, want)
	}
	if f.deleteN != 0 || f.pools[name] != expired {
		t.Fatalf("the refused delete reached the store: deletes=%d claim=%q", f.deleteN, f.pools[name])
	}
}

// TestSweepExpiredLockSentinel_MarksOnlyTheDelete sweeps a sentinel whose
// delete goes out in time. The delete carries the steal's marker with the
// re-read's start plus the steal budget, and the re-read carries none.
func TestSweepExpiredLockSentinel_MarksOnlyTheDelete(t *testing.T) {
	t.Parallel()
	start := time.Unix(10_000, 0)
	name := ClusterLockPoolName(diskTransferLockName("expired"))
	expired := encodeLockComment("crashed", start.Add(-time.Hour))
	f := newFakeLockPools()
	f.pools[name] = expired
	clk := fixedClock(start, time.Second)
	wire := &sendByWire{PoolService: f, now: clk.now}

	deleted, err := sweepExpiredLockSentinel(context.Background(), wire, name, expired, clk)
	if !deleted || err != nil {
		t.Fatalf("sweepExpiredLockSentinel = (%v, %v), want the sentinel deleted", deleted, err)
	}
	calls := wire.recorded()
	if len(calls) != 2 || calls[0].method != http.MethodGet || calls[0].guarded || calls[0].steal {
		t.Fatalf("want an unmarked re-read and then the delete, got %+v", calls)
	}
	if !calls[1].guarded || calls[1].refused || !calls[1].deadline.Equal(start.Add(clusterLockStealBudget)) {
		t.Fatalf("the sweep's delete did not carry the steal's deadline: %+v", calls[1])
	}
}

// TestSweepExpiredDiskTransferLocks_SendByRefusalIsNotAnError runs the whole
// sweep over two expired per-disk sentinels. The first delete is refused for
// its deadline and the second goes out in time. The sweep removes one, leaves
// the other for the next sweep, and reports no error.
func TestSweepExpiredDiskTransferLocks_SendByRefusalIsNotAnError(t *testing.T) {
	t.Parallel()
	now := time.Now()
	expired := encodeLockComment("crashed", now.Add(-time.Minute))
	first := ClusterLockPoolName(diskTransferLockName("a"))
	second := ClusterLockPoolName(diskTransferLockName("b"))
	store := &listingLockPools{fakeLockPools: newFakeLockPools()}
	store.pools[first] = expired
	store.pools[second] = expired
	clk := lockClock{now: func() time.Time { return now }, sleep: func(context.Context, time.Duration) error { return nil }}
	wire := &sendByWire{PoolService: store}
	wire.now = func() time.Time {
		if len(wire.deletes()) == 0 {
			return now.Add(clusterLockStealBudget + time.Second)
		}
		return now
	}
	client := &parkerLockClient{pools: &listingSendByWire{sendByWire: wire, lister: store}}

	removed, err := SweepExpiredDiskTransferLocks(withTestParkerLockClock(context.Background(), clk), client, nil)
	if err != nil || removed != 1 {
		t.Fatalf("removed=%d err=%v, want one sentinel removed and no error for the refused delete", removed, err)
	}
	if store.pools[first] != expired {
		t.Fatalf("the sentinel whose delete was refused is gone: %q", store.pools[first])
	}
	if _, ok := store.pools[second]; ok {
		t.Fatal("the sentinel whose delete went out in time is still there")
	}
}
