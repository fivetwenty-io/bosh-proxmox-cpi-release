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
