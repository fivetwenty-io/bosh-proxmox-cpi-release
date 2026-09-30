package pve

import (
	"context"
	"errors"
	"testing"
	"time"
)

// sleepRecordingClock returns a lockClock that advances by exactly the
// duration each sleep asks for and records every one of them, so a test can
// see the cadence an acquire waits on without spending that time.
func sleepRecordingClock(start time.Time, slept *[]time.Duration) lockClock {
	cur := start
	return lockClock{
		now: func() time.Time { return cur },
		sleep: func(_ context.Context, d time.Duration) error {
			*slept = append(*slept, d)
			cur = cur.Add(d)
			return nil
		},
	}
}

func TestClusterLockPollFor_DefaultsToTheInterval(t *testing.T) {
	t.Parallel()
	if got := clusterLockPollFor(context.Background()); got != clusterLockPollInterval {
		t.Fatalf("poll on a bare context = %s, want %s", got, clusterLockPollInterval)
	}
}

func TestClusterLockPollFor_ReadsTheContextOverride(t *testing.T) {
	t.Parallel()
	ctx := WithClusterLockPollForTest(context.Background(), 7*time.Millisecond)
	if got := clusterLockPollFor(ctx); got != 7*time.Millisecond {
		t.Fatalf("poll with an override = %s, want 7ms", got)
	}
}

func TestWithClusterLockPollForTest_IgnoresNonPositive(t *testing.T) {
	t.Parallel()
	for _, d := range []time.Duration{0, -time.Millisecond} {
		base := context.Background()
		if ctx := WithClusterLockPollForTest(base, d); ctx != base {
			t.Errorf("a poll of %s changed the context", d)
		}
	}
}

func TestClusterLockPollWait_UsesTheContextPoll(t *testing.T) {
	t.Parallel()
	const poll = 10 * time.Millisecond
	ctx := WithClusterLockPollForTest(context.Background(), poll)
	now := time.Unix(1000, 0)
	deadline := now.Add(time.Hour)
	for range 200 {
		// The wait is the poll plus a jitter below one poll.
		if wait := clusterLockPollWait(ctx, now, deadline); wait < poll || wait >= 2*poll {
			t.Fatalf("wait = %s, want at least %s and under %s", wait, poll, 2*poll)
		}
	}
}

func TestClusterLockPollWait_NeverPassesTheDeadline(t *testing.T) {
	t.Parallel()
	now := time.Unix(1000, 0)
	deadline := now.Add(2 * time.Millisecond)
	for name, ctx := range map[string]context.Context{
		"default poll":  context.Background(),
		"override poll": WithClusterLockPollForTest(context.Background(), 10*time.Millisecond),
	} {
		if wait := clusterLockPollWait(ctx, now, deadline); wait != 2*time.Millisecond {
			t.Errorf("%s: wait = %s, want the 2ms left before the deadline", name, wait)
		}
	}
}

// TestAcquireClusterLock_WaitsOnTheContextPoll waits out a live holder under a
// context that carries a short poll. Every sleep follows that poll, the wait
// still ends in the timeout at its deadline, and an acquire on a bare context
// keeps the production cadence.
func TestAcquireClusterLock_WaitsOnTheContextPoll(t *testing.T) {
	t.Parallel()
	const poll = 10 * time.Millisecond
	const timeout = time.Second
	start := time.Unix(1000, 0)

	f := newFakeLockPools()
	f.pools["bosh-lock-web"] = encodeLockComment("other-owner", time.Unix(100000, 0))
	var slept []time.Duration
	ctx := WithClusterLockPollForTest(context.Background(), poll)
	_, err := acquireClusterLockWithClock(ctx, f, "web", "me", time.Minute, timeout, sleepRecordingClock(start, &slept))
	if !errors.Is(err, ErrClusterLockTimeout) {
		t.Fatalf("want the lock timeout, got %v", err)
	}
	if len(slept) < int(timeout/(2*poll)) {
		t.Fatalf("the acquire slept %d times, want the many polls a %s wait at %s makes", len(slept), timeout, poll)
	}
	var total time.Duration
	for _, d := range slept {
		if d >= 2*poll {
			t.Fatalf("the acquire slept %s, longer than the context's poll allows", d)
		}
		total += d
	}
	if total != timeout {
		t.Fatalf("the sleeps add up to %s, want the whole %s wait", total, timeout)
	}

	f = newFakeLockPools()
	f.pools["bosh-lock-web"] = encodeLockComment("other-owner", time.Unix(100000, 0))
	slept = nil
	_, err = acquireClusterLockWithClock(context.Background(), f, "web", "me", time.Minute, timeout, sleepRecordingClock(start, &slept))
	if !errors.Is(err, ErrClusterLockTimeout) {
		t.Fatalf("want the lock timeout on a bare context, got %v", err)
	}
	if len(slept) == 0 || slept[0] < clusterLockPollInterval {
		t.Fatalf("a bare context slept %v, want the %s production poll first", slept, clusterLockPollInterval)
	}
}
