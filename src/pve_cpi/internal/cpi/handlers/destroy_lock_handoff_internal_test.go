package handlers

// Which config locks a destroy's retry loop waits out and which it hands to
// the skiplock and lock-clear recovery at once.

import (
	"context"
	"errors"
	"testing"
	"time"

	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

func lock500(lock string) error {
	return sdkerrors.ParseAPIError(500, []byte(`{"message":"unable to destroy VM 100: VM is locked (`+lock+`)\n"}`))
}

// Config-lock refusals outside a 5xx whose text also matches the storage-lock
// or the pushback classifier.
const (
	lockWithStorageLockText = "unable to destroy VM 100: VM is locked (backup): " +
		"cfs-lock 'storage-local' error: got lock request timeout"
	lockWithPushbackText = "unable to destroy VM 100: VM is locked (clone): unable to acquire lock"
)

func TestLockForRecovery(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		err  error
		want bool
	}{
		"nil":               {nil, false},
		"lock in task text": {errors.New(lockedCloneMsg), true},
		"lock as a 500":     {lock500("migrate"), false},
		// The loop returns a suspended lock at once, because only resuming the
		// VM clears it, so the closure leaves it to the loop.
		"suspended lock as a 500": {lock500("suspended"), false},
		// A lock outside a 5xx goes to the recovery even when its text also
		// reads as a storage lock or pushback, which the loop would retry.
		"lock with storage-lock text": {errors.New(lockWithStorageLockText), true},
		"lock with pushback text":     {errors.New(lockWithPushbackText), true},
		"cfs-lock timeout":            {crhLockErr(), false},
		"unrelated":                   {errors.New("permission denied"), false},
	} {
		if got := lockForRecovery(tc.err); got != tc.want {
			t.Errorf("%s: lockForRecovery = %v; want %v", name, got, tc.want)
		}
	}
}

// A lock that PVE answers as a 500 is waited out on the retry loop's lock
// curve, so a short lock clears and the plain destroy lands without any
// skiplock.
func TestCleanupVM_DestroyLockAs500_ClearsBeforeSkiplock(t *testing.T) {
	nodes := &lrNodesStub{deleteErrs: []error{lock500("backup"), nil}}
	q := &lrQEMUStub{}
	deps := Deps{Config: lrRootPamConfig(), PVE: &lrClient{nodes: nodes, qemu: q}, Logger: log.NewNopLogger()}

	cleanupVM(crhCtx(), deps, "pve01", 100, lrEnv(), log.NewNopLogger())

	if len(nodes.deleteCalls) != 2 {
		t.Fatalf("expected 2 DeleteQemu calls (locked 500 + plain retry), got %d", len(nodes.deleteCalls))
	}
	for i, call := range nodes.deleteCalls {
		if call.Skiplock != nil && *call.Skiplock {
			t.Errorf("DeleteQemu call %d carried skiplock; a lock that clears needs none", i)
		}
	}
	if nodes.failedTagCalls != 0 {
		t.Errorf("destroy succeeded; the VM must not be tagged as failed (got %d tag writes)", nodes.failedTagCalls)
	}
}

// delete_vm's destroy hands a task-text lock to the skiplock recovery on the
// first refusal instead of spending the loop's budget on it.
func TestDestroyVMWithRecovery_TaskTextLock_GoesStraightToSkiplock(t *testing.T) {
	nodes := &lrNodesStub{deleteErrs: []error{errors.New(lockedCloneMsg), nil}}
	q := &lrQEMUStub{}
	deps := Deps{Config: lrRootPamConfig(), PVE: &lrClient{nodes: nodes, qemu: q}, Logger: log.NewNopLogger()}

	_, gone, err := destroyStepOnlyBypassingGates(crhCtx(), deps)
	if err != nil || gone {
		t.Fatalf("destroyStepOnlyBypassingGates = gone %v, err %v; want a clean destroy", gone, err)
	}
	if len(nodes.deleteCalls) != 2 {
		t.Fatalf("expected 2 DeleteQemu calls (locked + skiplock), got %d", len(nodes.deleteCalls))
	}
	if call := nodes.deleteCalls[1]; call.Skiplock == nil || !*call.Skiplock {
		t.Error("the second DeleteQemu call must carry skiplock=true")
	}
}

// delete_vm's destroy loop waits out a config lock answered as a 500 for only
// rollbackDestroyMaxAttempts attempts, about 30s of backoff, and then hands it
// to the skiplock recovery, so the recovery still has time left on the
// operation's context.
func TestDestroyVMWithRecovery_LockAs500_IsBoundedBeforeSkiplock(t *testing.T) {
	errs := make([]error, 0, rollbackDestroyMaxAttempts+1)
	for range rollbackDestroyMaxAttempts {
		errs = append(errs, lock500("clone"))
	}
	nodes := &lrNodesStub{deleteErrs: append(errs, nil)}
	q := &lrQEMUStub{}
	deps := Deps{Config: lrRootPamConfig(), PVE: &lrClient{nodes: nodes, qemu: q}, Logger: log.NewNopLogger()}
	var sleeps []int
	ctx := pve.WithTestBackoff(context.Background(), func(attempt int) time.Duration {
		sleeps = append(sleeps, attempt)
		return 0
	})

	_, gone, err := destroyStepOnlyBypassingGates(ctx, deps)
	if err != nil || gone {
		t.Fatalf("destroyStepOnlyBypassingGates = gone %v, err %v; want a clean skiplock destroy", gone, err)
	}
	if len(nodes.deleteCalls) != rollbackDestroyMaxAttempts+1 {
		t.Fatalf("DeleteQemu calls = %d; want %d plain attempts and one skiplock", len(nodes.deleteCalls),
			rollbackDestroyMaxAttempts)
	}
	for i, call := range nodes.deleteCalls[:rollbackDestroyMaxAttempts] {
		if call.Skiplock != nil && *call.Skiplock {
			t.Errorf("DeleteQemu call %d carried skiplock before the plain budget ran out", i)
		}
	}
	if call := nodes.deleteCalls[rollbackDestroyMaxAttempts]; call.Skiplock == nil || !*call.Skiplock {
		t.Error("the call after the plain budget must carry skiplock=true")
	}
	if len(sleeps) != rollbackDestroyMaxAttempts-1 {
		t.Errorf("backoffs = %v; want %d, one between each plain attempt", sleeps, rollbackDestroyMaxAttempts-1)
	}
	var nominal time.Duration
	for _, attempt := range sleeps {
		nominal += pve.StorageLockBackoff(attempt)
	}
	if nominal > time.Minute {
		t.Errorf("nominal lock backoff = %s; want the wait bounded to tens of seconds", nominal)
	}
}

// A suspended lock answered as a 500 is not waited out at all, because only
// resuming the VM clears it, so it reaches the skiplock recovery at once.
func TestDestroyVMWithRecovery_SuspendedLockAs500_ReturnsAtOnce(t *testing.T) {
	nodes := &lrNodesStub{deleteErrs: []error{lock500("suspended"), nil}}
	q := &lrQEMUStub{}
	deps := Deps{Config: lrRootPamConfig(), PVE: &lrClient{nodes: nodes, qemu: q}, Logger: log.NewNopLogger()}

	_, gone, err := destroyStepOnlyBypassingGates(crhCtx(), deps)
	if err != nil || gone {
		t.Fatalf("destroyStepOnlyBypassingGates = gone %v, err %v; want a clean destroy", gone, err)
	}
	if len(nodes.deleteCalls) != 2 {
		t.Fatalf("expected 2 DeleteQemu calls (suspended lock + skiplock), got %d", len(nodes.deleteCalls))
	}
	if call := nodes.deleteCalls[1]; call.Skiplock == nil || !*call.Skiplock {
		t.Error("the second DeleteQemu call must carry skiplock=true")
	}
}

// A config lock outside a 5xx whose text also reads as a storage lock or as
// pushback goes to the skiplock recovery on the first refusal.
func TestDestroyVMWithRecovery_LockWithRetryableText_GoesStraightToSkiplock(t *testing.T) {
	for name, msg := range map[string]string{
		"storage-lock text": lockWithStorageLockText,
		"pushback text":     lockWithPushbackText,
	} {
		t.Run(name, func(t *testing.T) {
			nodes := &lrNodesStub{deleteErrs: []error{errors.New(msg), nil}}
			q := &lrQEMUStub{}
			deps := Deps{Config: lrRootPamConfig(), PVE: &lrClient{nodes: nodes, qemu: q}, Logger: log.NewNopLogger()}

			_, gone, err := destroyStepOnlyBypassingGates(crhCtx(), deps)
			if err != nil || gone {
				t.Fatalf("destroyStepOnlyBypassingGates = gone %v, err %v; want a clean destroy", gone, err)
			}
			if len(nodes.deleteCalls) != 2 {
				t.Fatalf("expected 2 DeleteQemu calls (locked + skiplock), got %d", len(nodes.deleteCalls))
			}
			if call := nodes.deleteCalls[1]; call.Skiplock == nil || !*call.Skiplock {
				t.Error("the second DeleteQemu call must carry skiplock=true")
			}
		})
	}
}
