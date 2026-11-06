package pve

// The guest-config lock case of RetryOnTransientOrLock. A lock most often
// arrives as a plain 500, which IsTransientTransport also matches, so these
// pin that it rides the storage-lock curve under its own reason, that a lock
// in task text is retried too, and that a suspended guest's lock is not.

import (
	"context"
	"errors"
	"testing"
	"time"

	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

func TestRetryOnTransientOrLock_ConfigLockIsRetried(t *testing.T) {
	t.Parallel()
	for name, lockErr := range map[string]error{
		"500":       sdkerrors.ParseAPIError(500, []byte(`{"message":"VM 4401 is locked (backup)\n"}`)),
		"task text": errors.New("task failed: unable to resize disk: VM is locked (snapshot)"),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var waits []int
			ctx := WithTestBackoff(context.Background(), func(attempt int) time.Duration {
				waits = append(waits, attempt)
				return 0
			})
			calls := 0
			err := RetryOnTransientOrLock(ctx, nil, "test", 5, func() error {
				calls++
				if calls < 4 {
					return lockErr
				}
				return nil
			})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if calls != 4 || len(waits) != 3 {
				t.Errorf("calls = %d, waits = %v; want 4 calls with 3 backoffs", calls, waits)
			}
		})
	}
}

func TestRetryOnTransientOrLock_ConfigLockExhaustsWithTheLockError(t *testing.T) {
	t.Parallel()
	ctx := WithTestBackoff(context.Background(), func(int) time.Duration { return 0 })
	lockErr := sdkerrors.ParseAPIError(500, []byte(`{"message":"VM 4402 is locked (create)\n"}`))
	calls := 0
	err := RetryOnTransientOrLock(ctx, nil, "test", 3, func() error {
		calls++
		return lockErr
	})
	if !IsVMConfigLocked(err) || VMConfigLockType(err) != "create" {
		t.Fatalf("err = %v; want the lock error back so skiplock recovery can read it", err)
	}
	if calls != 3 {
		t.Errorf("calls = %d; want 3", calls)
	}
}

func TestRetryOnTransientOrLock_SuspendedLockIsNotRetried(t *testing.T) {
	t.Parallel()
	ctx := WithTestBackoff(context.Background(), func(int) time.Duration {
		t.Error("a suspended lock must not back off")
		return 0
	})
	lockErr := sdkerrors.ParseAPIError(500, []byte(`{"message":"VM 4403 is locked (suspended)\n"}`))
	calls := 0
	err := RetryOnTransientOrLock(ctx, nil, "test", 5, func() error {
		calls++
		return lockErr
	})
	if !IsVMConfigLocked(err) || calls != 1 {
		t.Fatalf("err = %v after %d calls; want the lock error at once", err, calls)
	}
}

func TestRetryOrLockCurve_ConfigLockRidesTheStorageLockCurve(t *testing.T) {
	t.Parallel()
	lockErr := sdkerrors.ParseAPIError(500, []byte(`{"message":"VM 4404 is locked (migrate)\n"}`))
	if !IsTransientTransport(lockErr) {
		t.Fatal("precondition: a lock 500 also reads as transient")
	}
	d, reason := retryOrLockCurve(IsPVEPushback(lockErr), IsStorageLockTimeout(lockErr),
		IsClusterNotQuorate(lockErr), isSettlingVMConfigLock(lockErr), 20)
	if reason != "vm_config_lock" {
		t.Errorf("reason = %q; want vm_config_lock", reason)
	}
	if d <= 15*time.Second || d > 30*time.Second {
		t.Errorf("backoff %v; want the storage-lock curve above the 15s transient cap", d)
	}
	// Quorum loss keeps its own label when both match.
	if _, reason := retryOrLockCurve(false, false, true, true, 0); reason != "cluster_not_quorate" {
		t.Errorf("reason = %q; want cluster_not_quorate", reason)
	}
	// A config lock outranks storage-lock contention and pushback for the label.
	if _, reason := retryOrLockCurve(true, true, false, true, 0); reason != "vm_config_lock" {
		t.Errorf("reason = %q; want vm_config_lock", reason)
	}
}

func TestIsRetryableOrLockFault_ConfigLock(t *testing.T) {
	t.Parallel()
	if !IsRetryableOrLockFault(errors.New("VM 4405 is locked (clone)")) {
		t.Error("a clone lock in task text belongs to the loop's retried classes")
	}
	if IsRetryableOrLockFault(errors.New("VM 4405 is locked (suspended)")) {
		t.Error("a suspended lock is not retried by the loop")
	}
	if isSettlingVMConfigLock(errors.New("unrelated failure")) {
		t.Error("an error that is no lock is not a settling lock")
	}
}
