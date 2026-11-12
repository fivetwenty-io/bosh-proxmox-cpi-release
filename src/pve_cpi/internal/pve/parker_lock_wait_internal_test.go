package pve

import (
	"context"
	"errors"
	"testing"
	"time"
)

// heldParkerSentinel plants another owner's live claim on parker 90000's lock
// and releases it once the fake clock reaches holdFor. It returns the pools,
// the clock, and a context that runs the parker lock on that clock.
func heldParkerSentinel(holdFor time.Duration) (*fakeLockPools, *fakeLockClock, context.Context) {
	pools := newFakeLockPools()
	clk := &fakeLockClock{base: time.Now()}
	key := ClusterLockPoolName("vm-90000")
	held := encodeLockComment("holder", clk.base.Add(time.Hour))
	pools.pools[key] = held
	clk.onSleep = func(elapsed time.Duration) {
		if elapsed < holdFor {
			return
		}
		pools.mu.Lock()
		if pools.pools[key] == held {
			delete(pools.pools, key)
		}
		pools.mu.Unlock()
	}
	return pools, clk, withTestParkerLockClock(context.Background(), clk.clock())
}

// TestParkerLockTimeouts_DefaultWaitIsTheWholeTTL pins the default wait of the
// parker protection lock to one whole TTL, the same for every caller, so a
// caller that sets nothing waits out any single holder.
func TestParkerLockTimeouts_DefaultWaitIsTheWholeTTL(t *testing.T) {
	t.Parallel()
	ttl, wait := parkerLockTimeoutsFrom(context.Background())
	if want := parkerProtectionLockTTLNow(); ttl != want || wait != want {
		t.Fatalf("ttl=%s wait=%s, want both to be the lock's TTL, %s", ttl, wait, want)
	}
}

// TestWithParkerProtectionLock_DefaultWaitOutlastsAHolder runs a window on a
// plain context against a holder that keeps the lock for 20 seconds, longer
// than the sweep's wait. The window still runs once the holder lets go.
func TestWithParkerProtectionLock_DefaultWaitOutlastsAHolder(t *testing.T) {
	t.Parallel()
	const holdFor = ParkerLockSweepWait + 5*time.Second
	pools, clk, ctx := heldParkerSentinel(holdFor)
	ran := false
	err := withParkerProtectionLock(ctx, &parkerLockClient{pools: pools}, nil, 90000, "contender",
		func(context.Context) error {
			ran = true
			return nil
		})
	if err != nil || !ran {
		t.Fatalf("err=%v ran=%v, want the window to run once the holder let go", err, ran)
	}
	if elapsed := time.Duration(clk.offset.Load()); elapsed < holdFor {
		t.Fatalf("the window ran after %s, before the holder let go at %s", elapsed, holdFor)
	}
}

// TestWithParkerProtectionLock_SweepWaitGivesUpEarly runs the same holder
// against a context the deferred-teardown sweep caps. The acquire gives up
// retriably once the sweep's wait has passed, and the window never runs.
func TestWithParkerProtectionLock_SweepWaitGivesUpEarly(t *testing.T) {
	t.Parallel()
	const holdFor = ParkerLockSweepWait + 5*time.Second
	pools, clk, ctx := heldParkerSentinel(holdFor)
	ctx = WithParkerLockWaitAtMost(ctx, ParkerLockSweepWait)
	ran := false
	err := withParkerProtectionLock(ctx, &parkerLockClient{pools: pools}, nil, 90000, "sweep",
		func(context.Context) error {
			ran = true
			return nil
		})
	if !errors.Is(err, ErrClusterLockTimeout) || ran {
		t.Fatalf("err=%v ran=%v, want the sweep's acquire to time out without running", err, ran)
	}
	if elapsed := time.Duration(clk.offset.Load()); elapsed < ParkerLockSweepWait || elapsed >= holdFor {
		t.Fatalf("the sweep gave up after %s, want between %s and %s", elapsed, ParkerLockSweepWait, holdFor)
	}
}

// TestWithParkerLockWaitAtMost pins that the cap only ever lowers the wait and
// never touches the TTL.
func TestWithParkerLockWaitAtMost(t *testing.T) {
	t.Parallel()
	ttl := parkerProtectionLockTTLNow()
	cases := []struct {
		name string
		ctx  context.Context
		want time.Duration
	}{
		{"lowers the default", WithParkerLockWaitAtMost(context.Background(), 15*time.Second), 15 * time.Second},
		{"keeps a shorter wait", WithParkerLockWaitAtMost(WithParkerLockWait(context.Background(), time.Second),
			15*time.Second), time.Second},
		{"lowers a longer wait", WithParkerLockWaitAtMost(WithParkerLockWait(context.Background(), time.Hour),
			time.Minute), time.Minute},
		{"ignores a non-positive cap", WithParkerLockWaitAtMost(context.Background(), 0), ttl},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotTTL, gotWait := parkerLockTimeoutsFrom(tc.ctx)
			if gotTTL != ttl || gotWait != tc.want {
				t.Fatalf("ttl=%s wait=%s, want ttl %s and wait %s", gotTTL, gotWait, ttl, tc.want)
			}
		})
	}
}
