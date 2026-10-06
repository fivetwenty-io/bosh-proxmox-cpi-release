package handlers

import (
	"errors"
	"fmt"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// TestIsLockWaitWithoutEntry_CountsEveryAcquireThatNeverEnteredTheLock checks
// that each way a lock acquire gives up before the guarded work, including a
// claim confirmed too late to use, is classified the same way, and that an
// ordinary failure is not.
func TestIsLockWaitWithoutEntry_CountsEveryAcquireThatNeverEnteredTheLock(t *testing.T) {
	for _, sentinel := range []error{
		pve.ErrClusterLockTimeout,
		pve.ErrClusterLockStateUnknown,
		pve.ErrClusterLockClaimTooShort,
		pve.ErrClusterLockInterrupted,
		errManagedRequestEnded,
		errManagedAdmissionReadFailed,
	} {
		wrapped := fmt.Errorf("attach disk: %w", sentinel)
		if !isLockWaitWithoutEntry(wrapped) {
			t.Errorf("isLockWaitWithoutEntry(%v) = false, want true", wrapped)
		}
	}
	if isLockWaitWithoutEntry(errors.New("move disk failed")) {
		t.Error("an ordinary failure was classified as a lock wait without entry")
	}
	if isLockWaitWithoutEntry(nil) {
		t.Error("a nil error was classified as a lock wait without entry")
	}
}
