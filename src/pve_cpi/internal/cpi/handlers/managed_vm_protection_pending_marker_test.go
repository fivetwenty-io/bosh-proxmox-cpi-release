package handlers

import (
	"errors"
	"testing"
)

// TestProtectionPendingIsNotTheReturnedDiskMarker keeps the two signals apart.
// The lock-timeout rollback disposes of the VM on the returned-disk marker, so
// a landed disk must never read as that marker.
func TestProtectionPendingIsNotTheReturnedDiskMarker(t *testing.T) {
	t.Parallel()
	pending := &managedDiskProtectionPending{err: errors.New("cut off")}
	if isDiskReturnedAfterLockTimeout(pending) {
		t.Fatal("a pending protection reads as a returned disk, which the lock-timeout rollback would dispose of")
	}
	if isManagedDiskProtectionPending(&diskReturnedAfterLockTimeout{err: errors.New("timeout")}) {
		t.Fatal("a returned disk reads as a pending protection")
	}
}
