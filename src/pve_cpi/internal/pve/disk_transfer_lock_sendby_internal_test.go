package pve

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/endpointpool"
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
// sweep, and a Debug line names it, because this is the one skipped sentinel
// that the next sweep meets again.
func TestSweepExpiredLockSentinel_SendByPassedLeavesTheSentinel(t *testing.T) {
	t.Parallel()
	start := time.Unix(10_000, 0)
	name := ClusterLockPoolName(diskTransferLockName("expired"))
	expired := encodeLockComment("crashed", start.Add(-time.Hour))
	f := newFakeLockPools()
	f.pools[name] = expired
	clk := fixedClock(start, time.Second)
	// The delete arrives no earlier than its deadline, which counts as passed.
	wire := &sendByWire{PoolService: f, now: func() time.Time { return time.Now().Add(clusterLockStealBudget) }}
	logger, obs := log.NewObservedLogger(log.LevelDebug)

	before := time.Now()
	deleted, err := sweepExpiredLockSentinel(context.Background(), wire, name, expired, clk, logger)
	after := time.Now()
	if deleted || err != nil {
		t.Fatalf("sweepExpiredLockSentinel = (%v, %v), want (false, nil) for a delete refused by its send-by deadline", deleted, err)
	}
	deletes := wire.deletes()
	if len(deletes) != 1 || !deletes[0].refused {
		t.Fatalf("want one refused delete, got %+v", deletes)
	}
	assertStealDeadline(t, deletes[0].deadline, before, after, clusterLockStealBudget)
	if f.deleteN != 0 || f.pools[name] != expired {
		t.Fatalf("the refused delete reached the store: deletes=%d claim=%q", f.deleteN, f.pools[name])
	}
	entries := obs.All()
	if len(entries) != 1 || entries[0].Level != log.LevelDebug || entries[0].Attrs["pool"] != name {
		t.Fatalf("want one Debug line naming %s, got %+v", name, entries)
	}
}

// TestSweepExpiredLockSentinel_OtherSkipsLogNothing leaves a sentinel whose
// claim changed and one that is gone. Neither repeats on the next sweep for
// the same reason, so neither logs the send-by line.
func TestSweepExpiredLockSentinel_OtherSkipsLogNothing(t *testing.T) {
	t.Parallel()
	start := time.Unix(10_000, 0)
	name := ClusterLockPoolName(diskTransferLockName("expired"))
	expired := encodeLockComment("crashed", start.Add(-time.Hour))
	for label, stored := range map[string]string{"changed": encodeLockComment("other", start.Add(time.Hour)), "gone": ""} {
		t.Run(label, func(t *testing.T) {
			f := newFakeLockPools()
			if stored != "" {
				f.pools[name] = stored
			}
			clk := fixedClock(start, time.Second)
			wire := &sendByWire{PoolService: f, now: time.Now}
			logger, obs := log.NewObservedLogger(log.LevelDebug)
			deleted, err := sweepExpiredLockSentinel(context.Background(), wire, name, expired, clk, logger)
			if deleted || err != nil {
				t.Fatalf("sweepExpiredLockSentinel = (%v, %v), want (false, nil)", deleted, err)
			}
			if obs.Len() != 0 || len(wire.deletes()) != 0 {
				t.Fatalf("a sentinel skipped before its delete logged or sent: logs %+v deletes %+v", obs.All(), wire.deletes())
			}
		})
	}
}

// TestSweepExpiredLockSentinel_RefusalWithoutLoggerIsQuiet refuses the delete
// with no logger, as the callers that pass none do, and the sweep still leaves
// the sentinel without an error.
func TestSweepExpiredLockSentinel_RefusalWithoutLoggerIsQuiet(t *testing.T) {
	t.Parallel()
	start := time.Unix(10_000, 0)
	name := ClusterLockPoolName(diskTransferLockName("expired"))
	expired := encodeLockComment("crashed", start.Add(-time.Hour))
	f := newFakeLockPools()
	f.pools[name] = expired
	refusal := sdkSendByRefusal(http.MethodDelete, endpointpool.APIPrefix+"/pools", endpointpool.ErrSendByPassed)
	f.deleteFn = func(string) error { return refusal }
	deleted, err := sweepExpiredLockSentinel(context.Background(), f, name, expired, fixedClock(start, time.Second), nil)
	if deleted || err != nil {
		t.Fatalf("sweepExpiredLockSentinel = (%v, %v), want (false, nil)", deleted, err)
	}
	if f.pools[name] != expired {
		t.Fatalf("the refused delete disturbed the claim: %q", f.pools[name])
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
	wire := &sendByWire{PoolService: f, now: time.Now}

	before := time.Now()
	deleted, err := sweepExpiredLockSentinel(context.Background(), wire, name, expired, clk, nil)
	after := time.Now()
	if !deleted || err != nil {
		t.Fatalf("sweepExpiredLockSentinel = (%v, %v), want the sentinel deleted", deleted, err)
	}
	calls := wire.recorded()
	if len(calls) != 2 || calls[0].method != http.MethodGet || calls[0].guarded || calls[0].steal {
		t.Fatalf("want an unmarked re-read and then the delete, got %+v", calls)
	}
	if !calls[1].guarded || calls[1].refused {
		t.Fatalf("the sweep's delete did not carry the steal's marker: %+v", calls[1])
	}
	assertStealDeadline(t, calls[1].deadline, before, after, clusterLockStealBudget)
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
			return time.Now().Add(clusterLockStealBudget + time.Second)
		}
		return time.Now()
	}
	client := &parkerLockClient{pools: &listingSendByWire{sendByWire: wire, lister: store}}

	logger, obs := log.NewObservedLogger(log.LevelDebug)
	removed, err := SweepExpiredDiskTransferLocks(withTestParkerLockClock(context.Background(), clk), client, logger)
	if err != nil || removed != 1 {
		t.Fatalf("removed=%d err=%v, want one sentinel removed and no error for the refused delete", removed, err)
	}
	var debug []log.Entry
	for _, entry := range obs.All() {
		if entry.Level == log.LevelDebug {
			debug = append(debug, entry)
		}
	}
	if len(debug) != 1 || debug[0].Attrs["pool"] != first {
		t.Fatalf("want one Debug line naming the sentinel left for its deadline, got %+v", obs.All())
	}
	if store.pools[first] != expired {
		t.Fatalf("the sentinel whose delete was refused is gone: %q", store.pools[first])
	}
	if _, ok := store.pools[second]; ok {
		t.Fatal("the sentinel whose delete went out in time is still there")
	}
}
