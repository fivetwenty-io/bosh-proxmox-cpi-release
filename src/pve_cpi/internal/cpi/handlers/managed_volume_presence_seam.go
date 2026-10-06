package handlers

import (
	"sync/atomic"
	"time"
)

// defaultManagedVolumePresenceRetryDelay is the wait between the reads of a
// storage content listing that failed for a transient reason.
const defaultManagedVolumePresenceRetryDelay = 2 * time.Second

// managedVolumePresenceRetryDelayNs holds the wait between listing reads as
// nanoseconds in an atomic int64, so tests can race-safely shrink it.
var managedVolumePresenceRetryDelayNs atomic.Int64

func init() {
	managedVolumePresenceRetryDelayNs.Store(int64(defaultManagedVolumePresenceRetryDelay))
}

// managedVolumePresenceRetryDelay returns the current wait between reads.
func managedVolumePresenceRetryDelay() time.Duration {
	return time.Duration(managedVolumePresenceRetryDelayNs.Load())
}

// SetManagedVolumePresenceRetryDelay replaces the wait between listing reads
// for the duration of a test and returns a restore function.
//
//	defer handlers.SetManagedVolumePresenceRetryDelay(0)()
func SetManagedVolumePresenceRetryDelay(d time.Duration) func() {
	prev := managedVolumePresenceRetryDelayNs.Swap(int64(d))
	return func() { managedVolumePresenceRetryDelayNs.Store(prev) }
}
