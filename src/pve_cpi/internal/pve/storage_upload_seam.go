package pve

import "sync/atomic"

// Storage upload attempt budget, stored in an atomic so it can be configured
// once at process startup (ConfigureStorageUploadRetry) and overridden
// race-safely in tests. Zero means "use DefaultStorageUploadMaxAttempts", so
// an unconfigured process retries with the built-in budget.
//
// This mirrors the package-level seam pattern used for the transient budget
// (transient_seam.go): BOSH runs one CPI process per invocation with a single
// config, so a startup-time package default is the simplest correct wiring.
//
// Only the attempt budget is configurable for this class. Uploads ride the
// shared backoff curves (TransientBackoff / StorageLockBackoff) selected per
// fault by RetryOnTransientOrLock; the curves stay fixed.
var storageUploadMaxAttemptsOverride atomic.Int64

// ConfigureStorageUploadRetry sets the process-wide storage upload attempt
// budget from operator config. Applied only when maxAttempts > 0, so an unset
// retry.storage_upload block keeps DefaultStorageUploadMaxAttempts. Call once
// at startup, before serving requests.
func ConfigureStorageUploadRetry(maxAttempts int) {
	if maxAttempts > 0 {
		storageUploadMaxAttemptsOverride.Store(int64(maxAttempts))
	}
}

// StorageUploadMaxAttempts returns the effective process-wide storage upload
// attempt budget: the operator override installed by
// ConfigureStorageUploadRetry when one is set,
// DefaultStorageUploadMaxAttempts otherwise. Upload call sites pass this to
// RetryOnTransientOrLock explicitly because that helper's own zero-value
// fallback resolves to the smaller storage-lock budget.
func StorageUploadMaxAttempts() int {
	if v := storageUploadMaxAttemptsOverride.Load(); v > 0 {
		return int(v)
	}
	return DefaultStorageUploadMaxAttempts
}

// SetStorageUploadRetryForTest overrides the storage upload attempt budget
// for a test and returns a restore function. Mirrors SetTransientRetryForTest.
//
//	defer pve.SetStorageUploadRetryForTest(1)()
func SetStorageUploadRetryForTest(maxAttempts int) func() {
	prev := storageUploadMaxAttemptsOverride.Swap(int64(maxAttempts))
	return func() { storageUploadMaxAttemptsOverride.Store(prev) }
}
