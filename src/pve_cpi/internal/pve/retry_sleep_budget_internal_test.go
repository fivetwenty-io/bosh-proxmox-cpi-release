package pve

import (
	"testing"
	"time"
)

// TestBackoffCurvesNeverExceedTheirWorstCase draws each shipped curve many
// times and checks that no draw is longer than the same curve evaluated at the
// top of its jitter window, which is what the sleep budgets add up.
func TestBackoffCurvesNeverExceedTheirWorstCase(t *testing.T) {
	curves := []struct {
		name  string
		drawn func(int) time.Duration
		worst func(int) time.Duration
	}{
		{"transient", TransientBackoff, func(n int) time.Duration { return transientBackoff(n, maxJitterDraw) }},
		{"storage_lock", StorageLockBackoff, func(n int) time.Duration { return storageLockBackoff(n, maxJitterDraw) }},
		{"pushback", PushbackBackoff, func(n int) time.Duration { return pushbackBackoff(n, maxJitterDraw) }},
	}
	for _, curve := range curves {
		for attempt := 0; attempt < 12; attempt++ {
			worst := curve.worst(attempt)
			for range 500 {
				if got := curve.drawn(attempt); got > worst {
					t.Fatalf("%s attempt %d drew %s, longer than its worst case %s", curve.name, attempt, got, worst)
				}
			}
		}
	}
}

// TestSleepBudgetsMatchTheShippedCurves pins the budgets to values worked out
// by hand from the shipped curve constants. On every attempt below the caps
// the pushback curve (5s base, x1.5, jitter up to +30%) is the longest, so
// three attempts sleep twice, for at most 6.5s and 9.75s, less one nanosecond
// each because the jitter draw is from a half-open window.
func TestSleepBudgetsMatchTheShippedCurves(t *testing.T) {
	const drawGap = time.Nanosecond
	want := 6500*time.Millisecond + 9750*time.Millisecond - 2*drawGap
	if got := RetryOnTransientOrLockSleepBudget(3); got != want {
		t.Fatalf("RetryOnTransientOrLockSleepBudget(3) = %s, want %s", got, want)
	}
	if got := RetryOnTransientSleepBudget(3); got != want {
		t.Fatalf("RetryOnTransientSleepBudget(3) = %s, want %s", got, want)
	}
	if got := RetryOnTransientOrLockSleepBudget(1); got != 0 {
		t.Fatalf("a single attempt never sleeps, got budget %s", got)
	}
}

// TestSleepBudgetsResolveAttemptsLikeTheLoops checks that a non-positive
// attempt count resolves to the same default the retry loop itself uses.
func TestSleepBudgetsResolveAttemptsLikeTheLoops(t *testing.T) {
	if got, want := RetryOnTransientOrLockSleepBudget(0), RetryOnTransientOrLockSleepBudget(DefaultStorageLockMaxAttempts); got != want {
		t.Fatalf("RetryOnTransientOrLockSleepBudget(0) = %s, want the %d-attempt budget %s", got, DefaultStorageLockMaxAttempts, want)
	}
	if got, want := RetryOnTransientSleepBudget(-1), RetryOnTransientSleepBudget(TransientMaxAttempts()); got != want {
		t.Fatalf("RetryOnTransientSleepBudget(-1) = %s, want the %d-attempt budget %s", got, TransientMaxAttempts(), want)
	}
}

// TestSleepBudgetsFollowConfiguredCurves checks that an operator's longer
// backoff cap raises the budget, since the budget reads the live curves.
func TestSleepBudgetsFollowConfiguredCurves(t *testing.T) {
	shipped := RetryOnTransientOrLockSleepBudget(DefaultStorageLockMaxAttempts)
	restore := SetPushbackBackoffForTest(5000, 600000)
	defer restore()
	raised := RetryOnTransientOrLockSleepBudget(DefaultStorageLockMaxAttempts)
	if raised <= shipped {
		t.Fatalf("budget with a 600s pushback cap = %s, want more than the shipped %s", raised, shipped)
	}
}
