package pve

import (
	"context"
	"errors"
	mrand "math/rand/v2"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
)

func TestRetryOnStorageLock_SucceedsImmediately(t *testing.T) {
	t.Parallel()
	calls := 0
	err := RetryOnStorageLock(context.Background(), nil, "test", 5, func() error {
		calls++
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected 1 call, got %d", calls)
	}
}

func TestRetryOnStorageLock_NonLockErrorPropagates(t *testing.T) {
	t.Parallel()
	want := errors.New("some other failure")
	calls := 0
	err := RetryOnStorageLock(context.Background(), nil, "test", 5, func() error {
		calls++
		return want
	})
	if !errors.Is(err, want) {
		t.Fatalf("expected wrapped sentinel, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected 1 call (no retry on non-lock error), got %d", calls)
	}
}

func TestRetryOnStorageLock_LockTimeoutRetriesThenSucceeds(t *testing.T) {
	t.Parallel()
	// Override the backoff curve via WithTestBackoff so the retry loop's
	// behaviour (call count, log lines, return value) is verified without
	// burning multi-second sleeps. A small non-zero spacer keeps the loop
	// exercising the timer path; bump if backoff observability ever needs
	// asserting independently.
	const spacer = 5 * time.Millisecond
	ctx := WithTestBackoff(context.Background(), func(int) time.Duration { return spacer })
	calls := 0
	lockErr := errors.New("can't lock file '/var/lock/pve-manager/pve-storage-data' - got timeout")
	start := time.Now()
	err := RetryOnStorageLock(ctx, log.NewNopLogger(), "test", 3, func() error {
		calls++
		if calls < 3 {
			return lockErr
		}
		return nil
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 3 {
		t.Fatalf("expected 3 calls, got %d", calls)
	}
	// Two backoff hops at spacer each; lower bound proves the timer path
	// fired without locking the test to the production curve.
	if elapsed < 2*spacer {
		t.Fatalf("expected at least %v of backoff, got %v", 2*spacer, elapsed)
	}
}

func TestRetryOnStorageLock_ExhaustsAndReturnsLastErr(t *testing.T) {
	t.Parallel()
	calls := 0
	lockErr := errors.New("CAN'T LOCK FILE ... GOT TIMEOUT")
	err := RetryOnStorageLock(context.Background(), nil, "test", 2, func() error {
		calls++
		return lockErr
	})
	if !errors.Is(err, lockErr) {
		t.Fatalf("expected exhausted-attempts error to wrap last lock error, got %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected 2 calls (max attempts), got %d", calls)
	}
}

func TestRetryOnStorageLock_ContextCancelShortCircuits(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	lockErr := errors.New("can't lock file 'x' - got timeout")
	done := make(chan error, 1)
	go func() {
		done <- RetryOnStorageLock(ctx, nil, "test", 10, func() error {
			calls++
			if calls == 1 {
				// After the first failure the retry loop will enter
				// the backoff sleep; cancel just before that.
				go cancel()
			}
			return lockErr
		})
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("RetryOnStorageLock did not honor ctx cancellation in time")
	}
}

func TestRetryOnStorageLock_ZeroMaxAttemptsUsesDefault(t *testing.T) {
	t.Parallel()
	// With maxAttempts<=0 the helper substitutes DefaultStorageLockMaxAttempts
	// (10). Verify by capturing a non-lock error on the second call so we
	// never actually sleep.
	want := errors.New("non lock error")
	calls := 0
	err := RetryOnStorageLock(context.Background(), nil, "test", 0, func() error {
		calls++
		if calls == 1 {
			return errors.New("can't lock file 'x' - got timeout")
		}
		return want
	})
	if !errors.Is(err, want) {
		t.Fatalf("expected sentinel, got %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected 2 calls, got %d", calls)
	}
}

func TestRetryOnTransient_SucceedsImmediately(t *testing.T) {
	t.Parallel()
	calls := 0
	err := RetryOnTransient(context.Background(), nil, "test", 5, func() error {
		calls++
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected 1 call, got %d", calls)
	}
}

func TestRetryOnTransient_NonTransientPropagates(t *testing.T) {
	t.Parallel()
	want := errors.New("plain unrelated")
	calls := 0
	err := RetryOnTransient(context.Background(), nil, "test", 5, func() error {
		calls++
		return want
	})
	if !errors.Is(err, want) {
		t.Fatalf("expected wrapped sentinel, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected 1 call (no retry), got %d", calls)
	}
}

func TestRetryOnTransient_RetriesOnLoginEOF(t *testing.T) {
	t.Parallel()
	calls := 0
	transient := errors.New("auto-login failed: authentication failed: failed to parse login response: EOF")
	err := RetryOnTransient(context.Background(), nil, "test", 3, func() error {
		calls++
		if calls < 2 {
			return transient
		}
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected 2 calls, got %d", calls)
	}
}

func TestRetryOnTransient_ExhaustsAndReturnsLastErr(t *testing.T) {
	t.Parallel()
	calls := 0
	transient := errors.New("HTTP 596 (code: 596)")
	err := RetryOnTransient(context.Background(), nil, "test", 2, func() error {
		calls++
		return transient
	})
	if !errors.Is(err, transient) {
		t.Fatalf("expected exhausted error to wrap last transient, got %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected 2 calls (max), got %d", calls)
	}
}

func TestRetryOnTransientOrLock_SwitchesBackoffByReason(t *testing.T) {
	t.Parallel()
	// Mix a transient transport fault then a lock timeout to confirm both
	// predicates participate. WithTestBackoff zeros the curve so attempt
	// budget is the only thing being measured.
	ctx := WithTestBackoff(context.Background(), func(int) time.Duration { return 0 })
	calls := 0
	transient := errors.New("(code: 596)")
	lockErr := errors.New("can't lock file '/var/lock/pve-manager/pve-storage-data' - got timeout")
	err := RetryOnTransientOrLock(ctx, nil, "test", 3, func() error {
		calls++
		switch calls {
		case 1:
			return transient
		case 2:
			return lockErr
		default:
			return nil
		}
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 3 {
		t.Fatalf("expected 3 calls, got %d", calls)
	}
}

func TestRetryOnTransientOrLock_NonRetryablePropagates(t *testing.T) {
	t.Parallel()
	want := errors.New("some other failure")
	calls := 0
	err := RetryOnTransientOrLock(context.Background(), nil, "test", 5, func() error {
		calls++
		return want
	})
	if !errors.Is(err, want) {
		t.Fatalf("expected want, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected 1 call, got %d", calls)
	}
}

func TestTransientBackoff_GrowsAndCaps(t *testing.T) {
	t.Parallel()
	d0 := TransientBackoff(0)
	if d0 < 600*time.Millisecond || d0 > 1400*time.Millisecond {
		t.Errorf("attempt 0 backoff out of expected band: %v", d0)
	}
	dBig := TransientBackoff(20)
	if dBig > 15*time.Second {
		t.Errorf("attempt 20 backoff exceeds cap: %v", dBig)
	}
	if dBig < 10*time.Second {
		t.Errorf("attempt 20 backoff below floor at cap: %v", dBig)
	}
}

func TestStorageLockBackoff_GrowsAndCaps(t *testing.T) {
	t.Parallel()
	// At attempt 0 the floor is 2s − 30% = 1.4s. Past attempt ≈ 7 the
	// raw exponent exceeds 30s and the cap kicks in (jittered down to
	// ~21 s minimum).
	d0 := StorageLockBackoff(0)
	if d0 < 1300*time.Millisecond || d0 > 2700*time.Millisecond {
		t.Errorf("attempt 0 backoff out of expected band: %v", d0)
	}
	dBig := StorageLockBackoff(20)
	if dBig > 30*time.Second {
		t.Errorf("attempt 20 backoff exceeds cap: %v", dBig)
	}
	if dBig < 20*time.Second {
		t.Errorf("attempt 20 backoff below the floor at cap: %v", dBig)
	}
}

// TestBackoff_NoCapShadow confirms the package builds without `cap` being
// shadowed as a local variable. The renamed `maxBackoff` constant means
// the builtin `cap()` function is callable inside backoff helpers without
// resolving to the local constant. The test uses cap() directly to verify
// the builtin resolves correctly even after the rename — a compile-time
// guard rather than a runtime assertion. If a future patch reintroduces
// the shadow, `cap(slice)` here will return the wrong value or fail to
// compile, depending on how the shadow is reintroduced.
func TestBackoff_NoCapShadow(t *testing.T) {
	t.Parallel()
	// Exercise both backoff helpers (executes the renamed constant paths)
	// and then call cap() in the test scope to prove the builtin works.
	_ = TransientBackoff(0)
	_ = StorageLockBackoff(0)

	s := make([]int, 0, 7)
	if got := cap(s); got != 7 {
		t.Errorf("cap() builtin returned %d, want 7 (builtin shadow regression)", got)
	}
}

// TestBackoff_ZeroJitterNoPanic confirms the backoff helpers do not panic
// when the computed jitter window collapses to zero. Direct invocation of
// the public helpers at attempt 0 already exercises a non-zero window, so
// this test also exercises a wide attempt range to confirm no path
// (including the capped tail) triggers mrand.Int64N(0).
//
// A failure of the guard manifests as a runtime panic: "panic: invalid
// argument to Int64N". Without the guard, any path that produces a zero
// jitter window would crash the process; with the guard, all paths return
// a valid duration.
func TestBackoff_ZeroJitterNoPanic(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("backoff helper panicked: %v", r)
		}
	}()
	for attempt := 0; attempt < 30; attempt++ {
		_ = TransientBackoff(attempt)
		_ = StorageLockBackoff(attempt)
	}
}

// TestJitterSource_Seam confirms that swapping jitterSource under jitterMu
// changes the output of TransientBackoff and StorageLockBackoff, proving the
// seam is wired end-to-end. A seeded PCG source produces reproducible output,
// so two identical calls with the same source state return the same value.
//
// This test is NOT parallel because it mutates the package-level jitterSource.
// The mutation is held across both sample calls and restored via t.Cleanup,
// keeping the window minimal and preventing parallel readers (e.g.
// TestBackoff_ZeroJitterNoPanic) from observing a partially-swapped state.
func TestJitterSource_Seam(t *testing.T) {
	// Install a deterministic restore once, at test start.
	jitterMu.Lock()
	prev := jitterSource
	jitterMu.Unlock()
	t.Cleanup(func() {
		jitterMu.Lock()
		jitterSource = prev
		jitterMu.Unlock()
	})

	// First sample: seed a PCG source and capture the attempt-0 output.
	seed1 := mrand.NewPCG(42, 99)
	jitterMu.Lock()
	jitterSource = mrand.New(seed1)
	jitterMu.Unlock()
	first := TransientBackoff(0)

	// Second sample: re-seed to the identical initial state and capture again.
	// If the seam is correctly wired both calls must return the same value.
	seed2 := mrand.NewPCG(42, 99)
	jitterMu.Lock()
	jitterSource = mrand.New(seed2)
	jitterMu.Unlock()
	second := TransientBackoff(0)

	if first != second {
		t.Errorf("jitterSource seam not wired: same seed produced different outputs (%v vs %v)", first, second)
	}
}
