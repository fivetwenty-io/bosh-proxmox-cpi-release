package pve_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// ---------------------------------------------------------------------------
// IsPVEPushback
// ---------------------------------------------------------------------------

func TestIsPVEPushback_Nil(t *testing.T) {
	t.Parallel()
	if pve.IsPVEPushback(nil) {
		t.Error("nil must not be pushback")
	}
}

func TestIsPVEPushback_HTTP429(t *testing.T) {
	t.Parallel()
	err := makeAPIErr(429, "too many requests")
	if !pve.IsPVEPushback(err) {
		t.Errorf("HTTP 429 APIError must be pushback; err=%v", err)
	}
}

func TestIsPVEPushback_TooManyRequestsPhrase(t *testing.T) {
	t.Parallel()
	err := errors.New("PVE API error: too many requests from this client")
	if !pve.IsPVEPushback(err) {
		t.Errorf("phrase 'too many requests' must be pushback; err=%v", err)
	}
}

func TestIsPVEPushback_WorkerBusyPhrase(t *testing.T) {
	t.Parallel()
	err := errors.New("worker busy, try again")
	if !pve.IsPVEPushback(err) {
		t.Errorf("phrase 'worker busy' must be pushback; err=%v", err)
	}
}

func TestIsPVEPushback_WorkerPoolPhrase(t *testing.T) {
	t.Parallel()
	err := errors.New("worker pool exhausted")
	if !pve.IsPVEPushback(err) {
		t.Errorf("phrase 'worker pool' must be pushback; err=%v", err)
	}
}

func TestIsPVEPushback_UnableToAcquireLockPhrase(t *testing.T) {
	t.Parallel()
	err := errors.New("unable to acquire lock for resource")
	if !pve.IsPVEPushback(err) {
		t.Errorf("phrase 'unable to acquire lock' must be pushback; err=%v", err)
	}
}

func TestIsPVEPushback_LockAcquireTimeoutPhrase(t *testing.T) {
	t.Parallel()
	err := errors.New("lock-acquire timeout reached")
	if !pve.IsPVEPushback(err) {
		t.Errorf("phrase 'lock-acquire timeout' must be pushback; err=%v", err)
	}
}

func TestIsPVEPushback_GotTimeoutPhrase(t *testing.T) {
	t.Parallel()
	err := errors.New("pvedaemon request: got timeout")
	if !pve.IsPVEPushback(err) {
		t.Errorf("phrase 'got timeout' must be pushback; err=%v", err)
	}
}

// TestIsPVEPushback_Resolved4xxWithPushbackPhrase_NotPushback pins the
// resolved-verdict guard: a non-429 4xx whose body happens to contain a
// pushback phrase ("got timeout") is a settled answer about the request, and
// must not spend a pushback backoff. A 429 carrying the same body stays
// pushback.
func TestIsPVEPushback_Resolved4xxWithPushbackPhrase_NotPushback(t *testing.T) {
	t.Parallel()
	err := makeAPIErr(403, "permission check: got timeout")
	if pve.IsPVEPushback(err) {
		t.Errorf("a resolved 403 must NOT be pushback even with a matching phrase in the body; err=%v", err)
	}
	err = makeAPIErr(400, "parameter verification: got timeout")
	if pve.IsPVEPushback(err) {
		t.Errorf("a resolved 400 must NOT be pushback even with a matching phrase in the body; err=%v", err)
	}
	err = makeAPIErr(429, "too many requests: got timeout")
	if !pve.IsPVEPushback(err) {
		t.Errorf("HTTP 429 stays pushback regardless of body; err=%v", err)
	}
}

func TestIsPVEPushback_Unrelated_400(t *testing.T) {
	t.Parallel()
	err := makeAPIErr(400, "bad request")
	if pve.IsPVEPushback(err) {
		t.Errorf("HTTP 400 must NOT be pushback; err=%v", err)
	}
}

func TestIsPVEPushback_Unrelated_403(t *testing.T) {
	t.Parallel()
	err := makeAPIErr(403, "permission denied")
	if pve.IsPVEPushback(err) {
		t.Errorf("HTTP 403 must NOT be pushback; err=%v", err)
	}
}

func TestIsPVEPushback_Unrelated_404(t *testing.T) {
	t.Parallel()
	err := makeAPIErr(404, "not found")
	if pve.IsPVEPushback(err) {
		t.Errorf("HTTP 404 must NOT be pushback; err=%v", err)
	}
}

func TestIsPVEPushback_StorageLockNoOverlap(t *testing.T) {
	t.Parallel()
	// "can't lock file ... got timeout" matches BOTH IsPVEPushback (via "got
	// timeout" phrase) AND IsStorageLockTimeout. Verify IsPVEPushback returns
	// true (superset) and IsStorageLockTimeout is independently still true.
	err := errors.New("can't lock file '/var/lock/pve-manager/pve-storage-data' - got timeout")
	if !pve.IsPVEPushback(err) {
		t.Error("storage lock timeout must also be classified as pushback (superset)")
	}
	if !pve.IsStorageLockTimeout(err) {
		t.Error("IsStorageLockTimeout must still return true (no regression)")
	}
}

func TestIsPVEPushback_Unrelated_PlainError(t *testing.T) {
	t.Parallel()
	err := errors.New("VM 131 already exists")
	if pve.IsPVEPushback(err) {
		t.Errorf("unrelated error must NOT be pushback; err=%v", err)
	}
}

// TestWrapError_429_IsRetriable is the key safety invariant: HTTP 429 was
// previously non-retriable (generic 4xx branch). After wiring IsPVEPushback
// into WrapError it must become retriable.
func TestWrapError_429_IsRetriable(t *testing.T) {
	t.Parallel()
	err := makeAPIErr(429, "too many requests")
	wrapped := pve.WrapError(err)
	if wrapped == nil {
		t.Fatal("WrapError returned nil")
	}
	if !cpiErrIsRetriable(t, wrapped) {
		t.Errorf("HTTP 429 must map to RetriableCloudError; got %T %v", wrapped, wrapped)
	}
}

// ---------------------------------------------------------------------------
// PushbackBackoff curve properties
// ---------------------------------------------------------------------------

func TestPushbackBackoff_GrowsAndCaps(t *testing.T) {
	t.Parallel()
	// At attempt 0 the floor is 5s − 30% = 3.5s; ceiling 5s + 30% = 6.5s.
	d0 := pve.PushbackBackoff(0)
	if d0 < 3*time.Second || d0 > 7*time.Second {
		t.Errorf("attempt 0 backoff out of expected band [3s,7s]: %v", d0)
	}
	dBig := pve.PushbackBackoff(20)
	if dBig > 60*time.Second {
		t.Errorf("attempt 20 backoff exceeds cap (60s): %v", dBig)
	}
	if dBig < 40*time.Second {
		t.Errorf("attempt 20 backoff below floor at cap (40s): %v", dBig)
	}
}

func TestPushbackBackoff_CapGreaterThanStorageLock(t *testing.T) {
	t.Parallel()
	// PushbackBackoff cap (60s) must be strictly greater than StorageLockBackoff
	// cap (30s). Verified via the exported cap accessors.
	if pve.PushbackBackoffCap() <= pve.StorageLockBackoffCap() {
		t.Errorf("PushbackBackoff cap (%v) must be > StorageLockBackoff cap (%v)",
			pve.PushbackBackoffCap(), pve.StorageLockBackoffCap())
	}
}

func TestPushbackBackoff_ZeroJitterNoPanic(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("PushbackBackoff panicked: %v", r)
		}
	}()
	for attempt := 0; attempt < 30; attempt++ {
		_ = pve.PushbackBackoff(attempt)
	}
}

// ---------------------------------------------------------------------------
// ConfigurePushbackBackoff seam
// ---------------------------------------------------------------------------

// NOTE: these tests mutate process-wide pushback defaults and must NOT run in
// parallel with each other. Each restores via SetPushbackBackoffForTest.

func TestConfigurePushbackBackoff_SetsBaseAndCap(t *testing.T) {
	restore := pve.SetPushbackBackoffForTest(5000, 60000)
	defer restore()

	pve.ConfigurePushbackBackoff(8000, 90000)
	capDur := pve.PushbackBackoffCap()
	if capDur != 90*time.Second {
		t.Errorf("cap = %v, want 90s", capDur)
	}
	// Verify base via PushbackBackoff(0): floor = base * 0.7, ceil = base * 1.3.
	d := pve.PushbackBackoff(0)
	if d < 5*time.Second || d > 11*time.Second {
		t.Errorf("attempt-0 backoff %v outside [5s,11s] for base=8s", d)
	}
}

func TestConfigurePushbackBackoff_ZeroIgnored(t *testing.T) {
	restore := pve.SetPushbackBackoffForTest(5000, 60000)
	defer restore()

	pve.ConfigurePushbackBackoff(0, 0) // both ≤ 0 → no change
	capDur := pve.PushbackBackoffCap()
	if capDur != 60*time.Second {
		t.Errorf("cap = %v, want unchanged 60s", capDur)
	}
}

func TestConfigurePushbackBackoff_CapClampedUpToBase(t *testing.T) {
	restore := pve.SetPushbackBackoffForTest(5000, 60000)
	defer restore()

	pve.ConfigurePushbackBackoff(20000, 1000) // cap(1s) < base(20s) → clamp cap up
	capDur := pve.PushbackBackoffCap()
	if capDur < 20*time.Second {
		t.Errorf("cap = %v, must be >= base (20s) after clamp", capDur)
	}
}

func TestConfigurePushbackBackoff_Defaults5s60s(t *testing.T) {
	restore := pve.SetPushbackBackoffForTest(5000, 60000)
	defer restore()

	// Shipped defaults: base=5000ms, cap=60000ms.
	capDur := pve.PushbackBackoffCap()
	if capDur != 60*time.Second {
		t.Errorf("default cap = %v, want 60s", capDur)
	}
	d := pve.PushbackBackoff(0)
	if d < 3*time.Second || d > 7*time.Second {
		t.Errorf("attempt-0 default backoff %v outside [3s,7s]", d)
	}
}

// ---------------------------------------------------------------------------
// RetryOnTransient — pushback wiring
// ---------------------------------------------------------------------------

func TestRetryOnTransient_PushbackIsRetried(t *testing.T) {
	t.Parallel()
	ctx := pve.WithTestBackoff(context.Background(), func(int) time.Duration { return 0 })
	calls := 0
	pushback429 := makeAPIErr(429, "too many requests")
	err := pve.RetryOnTransient(ctx, nil, "test", 4, func() error {
		calls++
		if calls < 3 {
			return pushback429
		}
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 3 {
		t.Fatalf("expected 3 calls (2 pushback retries then success), got %d", calls)
	}
}

func TestRetryOnTransient_PureTransientStillRetried(t *testing.T) {
	t.Parallel()
	ctx := pve.WithTestBackoff(context.Background(), func(int) time.Duration { return 0 })
	calls := 0
	// "auto-login failed" phrase triggers IsTransientTransport.
	transientErr := errors.New("auto-login failed: backend worker exited")
	err := pve.RetryOnTransient(ctx, nil, "test", 3, func() error {
		calls++
		if calls < 2 {
			return transientErr
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

func TestRetryOnTransient_NonRetriableReturnedImmediately(t *testing.T) {
	t.Parallel()
	want := errors.New("VM 100 not found")
	calls := 0
	err := pve.RetryOnTransient(context.Background(), nil, "test", 5, func() error {
		calls++
		return want
	})
	if !errors.Is(err, want) {
		t.Fatalf("expected sentinel, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected 1 call (no retry on non-retriable), got %d", calls)
	}
}

// ---------------------------------------------------------------------------
// RetryOnTransientOrLock — pushback wiring
// ---------------------------------------------------------------------------

func TestRetryOnTransientOrLock_PushbackIsRetried(t *testing.T) {
	t.Parallel()
	ctx := pve.WithTestBackoff(context.Background(), func(int) time.Duration { return 0 })
	calls := 0
	pushbackErr := errors.New("worker busy, try again")
	err := pve.RetryOnTransientOrLock(ctx, nil, "test", 4, func() error {
		calls++
		if calls < 3 {
			return pushbackErr
		}
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 3 {
		t.Fatalf("expected 3 calls, got %d", calls)
	}
}

func TestRetryOnTransientOrLock_PushbackPriorityOverLock(t *testing.T) {
	// An error that satisfies BOTH IsPVEPushback (via "got timeout" phrase) AND
	// IsStorageLockTimeout must select the pushback curve (higher priority).
	// We verify this indirectly by confirming the loop retries; curve choice is
	// exercised through coverage.
	t.Parallel()
	ctx := pve.WithTestBackoff(context.Background(), func(int) time.Duration { return 0 })
	calls := 0
	bothErr := errors.New("can't lock file '/var/lock/pve-manager/pve-storage-data' - got timeout")
	err := pve.RetryOnTransientOrLock(ctx, nil, "test", 3, func() error {
		calls++
		if calls < 2 {
			return bothErr
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

func TestRetryOnTransientOrLock_429RawRetried(t *testing.T) {
	// HTTP 429 (raw SDK APIError, before WrapError) must be retried in
	// RetryOnTransientOrLock via the IsPVEPushback path.
	t.Parallel()
	ctx := pve.WithTestBackoff(context.Background(), func(int) time.Duration { return 0 })
	calls := 0
	raw429 := makeAPIErr(429, "too many requests")
	err := pve.RetryOnTransientOrLock(ctx, nil, "test", 4, func() error {
		calls++
		if calls < 3 {
			return raw429
		}
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 3 {
		t.Fatalf("expected 3 calls, got %d", calls)
	}
}
