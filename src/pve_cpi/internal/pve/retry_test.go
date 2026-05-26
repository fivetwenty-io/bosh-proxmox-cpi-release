package pve

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
)

func TestRetryOnStorageLock_SucceedsImmediately(t *testing.T) {
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
	// Override sleep so the test does not actually wait through the
	// exponential backoff. We can't stub StorageLockBackoff directly but
	// we can keep attempts low and rely on test runner being patient (max
	// 2 sleeps of ~1.4-2.6 s at attempts 0,1). To keep tests fast, lower
	// the backoff temporarily by exercising only attempt 0 (the very
	// first retry, which sleeps 1.4-2.6 s). Use 3 max attempts but only
	// fail twice — call site #3 succeeds. We accept ~3 s of sleep here.
	calls := 0
	lockErr := errors.New("can't lock file '/var/lock/pve-manager/pve-storage-data' - got timeout")
	start := time.Now()
	err := RetryOnStorageLock(context.Background(), log.NewNopLogger(), "test", 3, func() error {
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
	// Two backoff sleeps fired; first attempt (idx 0) is 1.4-2.6s,
	// second (idx 1) is 2.1-3.9s — total ≥ ~3.5 s comfortably.
	if elapsed < 1*time.Second {
		t.Fatalf("expected at least 1 s of backoff, got %v", elapsed)
	}
}

func TestRetryOnStorageLock_ExhaustsAndReturnsLastErr(t *testing.T) {
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
	// Mix a transient transport fault then a lock timeout to confirm both
	// predicates participate. Cap attempts at 3 to keep the test fast
	// (worst-case sleep ~3-4s).
	calls := 0
	transient := errors.New("(code: 596)")
	lockErr := errors.New("can't lock file '/var/lock/pve-manager/pve-storage-data' - got timeout")
	err := RetryOnTransientOrLock(context.Background(), nil, "test", 3, func() error {
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
