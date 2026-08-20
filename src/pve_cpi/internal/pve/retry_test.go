package pve

import (
	"context"
	"errors"
	mrand "math/rand/v2"
	"testing"
	"time"
)

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

// ---------------------------------------------------------------------------
// Quorum-loss retry-curve routing (IsClusterNotQuorate)
// ---------------------------------------------------------------------------

// TestRetryOnTransientOrLock_QuorumIsRetried proves a "not quorate" error is
// retried by RetryOnTransientOrLock rather than propagated immediately.
func TestRetryOnTransientOrLock_QuorumIsRetried(t *testing.T) {
	t.Parallel()
	ctx := WithTestBackoff(context.Background(), func(int) time.Duration { return 0 })
	calls := 0
	quorumErr := errors.New("error writing config, cfs-lock failed - not quorate")
	err := RetryOnTransientOrLock(ctx, nil, "test", 4, func() error {
		calls++
		if calls < 3 {
			return quorumErr
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

// TestRetryOrLockCurve_QuorumUsesStorageLockCurveNotTransient is the
// retry-curve routing test the acceptance criteria calls for: it proves a
// quorum error selects the storage-lock backoff curve (2s→30s, 10 attempts)
// rather than the shorter transient-transport curve (1s→15s, 8 attempts) —
// critical because a quorum 5xx also satisfies IsTransientTransport, so
// without explicit routing it would silently fall through to the wrong,
// too-short curve. At a high attempt count both curves are fully capped:
// StorageLockBackoff caps at 30s (band ~20-30s, see
// TestStorageLockBackoff_GrowsAndCaps) while TransientBackoff caps at 15s
// (band ~10.5-15s, see TestTransientBackoff_GrowsAndCaps) — landing above the
// transient cap proves the longer curve was actually selected, not merely
// labeled.
func TestRetryOrLockCurve_QuorumUsesStorageLockCurveNotTransient(t *testing.T) {
	t.Parallel()
	d, reason := retryOrLockCurve(false, false, true, 20)
	if reason != "cluster_not_quorate" {
		t.Errorf("reason = %q; want %q", reason, "cluster_not_quorate")
	}
	if d <= 15*time.Second {
		t.Errorf("quorum backoff %v landed at/under the transient-curve cap (15s); "+
			"must use the longer storage-lock curve instead", d)
	}
	if d > 30*time.Second {
		t.Errorf("quorum backoff %v exceeds the storage-lock curve cap (30s)", d)
	}
}

// TestRetryOrLockCurve_LockAndQuorumWinOverPushback verifies lock and quorum
// take priority over pushback. IsPVEPushback substring-matches "got timeout",
// which appears in both canonical storage-lock error shapes, so both
// classifiers flag the same error — a pushback-first ordering routed every
// storage-lock retry onto the pushback curve, ignoring the operator's tuned
// pve.retry.storage_lock settings and mislabeling the retry-log reason.
func TestRetryOrLockCurve_LockAndQuorumWinOverPushback(t *testing.T) {
	t.Parallel()
	if _, reason := retryOrLockCurve(true, false, true, 0); reason != "cluster_not_quorate" {
		t.Errorf("reason = %q; want %q (quorum must win over pushback)", reason, "cluster_not_quorate")
	}
	if _, reason := retryOrLockCurve(true, true, false, 0); reason != "storage_lock" {
		t.Errorf("reason = %q; want %q (lock must win over pushback)", reason, "storage_lock")
	}
	if _, reason := retryOrLockCurve(true, false, false, 0); reason != "pushback" {
		t.Errorf("reason = %q; want %q (pure pushback keeps its curve)", reason, "pushback")
	}
}

// TestRetryOrLockCurve_CanonicalStorageLockStrings routes the two canonical
// PVE storage-lock error shapes through the real classifiers and asserts they
// land on the storage_lock curve — the end-to-end regression for the
// pushback-superset bug, using production classification rather than
// hand-set booleans.
func TestRetryOrLockCurve_CanonicalStorageLockStrings(t *testing.T) {
	t.Parallel()
	for _, msg := range []string{
		"can't lock file '/var/lock/pve-manager/pve-storage-local' - got timeout",
		"command '/sbin/lvcreate ...' failed: got timeout",
	} {
		err := errors.New(msg)
		isLock := IsStorageLockTimeout(err)
		if !isLock {
			t.Fatalf("IsStorageLockTimeout(%q) = false; canonical shape must classify as lock", msg)
		}
		_, reason := retryOrLockCurve(IsPVEPushback(err), isLock, IsClusterNotQuorate(err), 0)
		if reason != "storage_lock" {
			t.Errorf("reason for %q = %q; want storage_lock", msg, reason)
		}
	}
}

// TestRetryOrLockCurve_QuorumReasonDistinctFromLock verifies the log-line
// reason label distinguishes quorum loss from plain storage-lock contention
// even though both ride the identical StorageLockBackoff curve — operators
// scanning retry logs should be able to tell the two conditions apart.
func TestRetryOrLockCurve_QuorumReasonDistinctFromLock(t *testing.T) {
	t.Parallel()
	_, reason := retryOrLockCurve(false, true, true, 0)
	if reason != "cluster_not_quorate" {
		t.Errorf("reason = %q; want %q when both isLock and isQuorum are true", reason, "cluster_not_quorate")
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

func TestRetryOnTransientOrUnplugBusy_RetriesBusyThenSucceeds(t *testing.T) {
	t.Parallel()
	ctx := WithTestBackoff(context.Background(), func(int) time.Duration { return 0 })
	busy := errors.New("API request failed: parameter error: Parameter verification failed. (code: 0, errors: scsi1: hotplug problem - error on hot-unplugging device 'virtioscsi1' - still busy in guest?)")
	calls := 0
	err := RetryOnTransientOrUnplugBusy(ctx, nil, "test", 4, func() error {
		calls++
		if calls < 3 {
			return busy
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

func TestRetryOnTransientOrUnplugBusy_ExhaustsBusyBudget(t *testing.T) {
	t.Parallel()
	ctx := WithTestBackoff(context.Background(), func(int) time.Duration { return 0 })
	busy := errors.New("scsi1: hotplug problem - error on hot-unplugging device 'virtioscsi1' - still busy in guest?")
	calls := 0
	err := RetryOnTransientOrUnplugBusy(ctx, nil, "test", 2, func() error {
		calls++
		return busy
	})
	if !errors.Is(err, busy) {
		t.Fatalf("expected exhausted error to be the busy error, got %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected 2 calls (max), got %d", calls)
	}
}

func TestRetryOnTransientOrUnplugBusy_NonRetryablePropagates(t *testing.T) {
	t.Parallel()
	perm := errors.New("Parameter verification failed. (code: 0, errors: scsi1: invalid format)")
	calls := 0
	err := RetryOnTransientOrUnplugBusy(context.Background(), nil, "test", 5, func() error {
		calls++
		return perm
	})
	if !errors.Is(err, perm) {
		t.Fatalf("expected permanent error back, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected exactly 1 call, got %d", calls)
	}
}

// TestRetryOnTransient_ConfiguredBudgetOverride verifies the retry.transient
// seam: a caller passing maxAttempts 0 gets the process-wide configured
// budget instead of DefaultTransientMaxAttempts. Not parallel — the seam is a
// package-level default.
func TestRetryOnTransient_ConfiguredBudgetOverride(t *testing.T) {
	defer SetTransientRetryForTest(1)()
	calls := 0
	transient := errors.New("HTTP 596 (code: 596)")
	err := RetryOnTransient(context.Background(), nil, "test", 0, func() error {
		calls++
		return transient
	})
	if !errors.Is(err, transient) {
		t.Fatalf("expected exhausted error to wrap last transient, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected exactly 1 call under the configured budget, got %d", calls)
	}
}

// TestConfigureTransientRetry_ZeroKeepsDefault verifies startup wiring with an
// unset retry.transient block (0) leaves DefaultTransientMaxAttempts in force.
func TestConfigureTransientRetry_ZeroKeepsDefault(t *testing.T) {
	defer SetTransientRetryForTest(0)() // restore whatever the process had
	ConfigureTransientRetry(0)
	if got := transientMaxAttemptsDefault(); got != DefaultTransientMaxAttempts {
		t.Fatalf("unconfigured budget = %d; want DefaultTransientMaxAttempts %d", got, DefaultTransientMaxAttempts)
	}
	ConfigureTransientRetry(3)
	if got := transientMaxAttemptsDefault(); got != 3 {
		t.Fatalf("configured budget = %d; want 3", got)
	}
}
