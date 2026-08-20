package pve

import "sync/atomic"

// Transient retry attempt budget, stored in an atomic so it can be configured
// once at process startup (ConfigureTransientRetry) and overridden race-safely
// in tests. Zero means "use DefaultTransientMaxAttempts", so an unconfigured
// process retries identically to prior releases.
//
// This mirrors the package-level seam pattern used for task-poll cadence
// (task_poll_seam.go) and pushback backoff (pushback_seam.go): BOSH runs one
// CPI process per invocation with a single config, so a startup-time package
// default is the simplest correct wiring and avoids threading an attempt
// budget through every RetryOnTransient call site (most pass 0 today).
//
// Only the attempt budget is configurable for this class. The backoff curve
// (TransientBackoff, 1s..15s) is tuned to pvedaemon's sub-second worker
// restart and stays fixed.
var transientMaxAttemptsOverride atomic.Int64

// ConfigureTransientRetry sets the process-wide transient retry attempt
// budget from operator config. Applied only when maxAttempts > 0, so an unset
// retry.transient block keeps DefaultTransientMaxAttempts. Call once at
// startup, before serving requests.
func ConfigureTransientRetry(maxAttempts int) {
	if maxAttempts > 0 {
		transientMaxAttemptsOverride.Store(int64(maxAttempts))
	}
}

// transientMaxAttemptsDefault returns the effective fallback attempt budget
// used when a RetryOnTransient caller passes maxAttempts <= 0.
func transientMaxAttemptsDefault() int {
	if v := transientMaxAttemptsOverride.Load(); v > 0 {
		return int(v)
	}
	return DefaultTransientMaxAttempts
}

// SetTransientRetryForTest overrides the transient attempt budget for a test
// and returns a restore function. Mirrors SetTaskPollingForTest.
//
//	defer pve.SetTransientRetryForTest(1)()
func SetTransientRetryForTest(maxAttempts int) func() {
	prev := transientMaxAttemptsOverride.Swap(int64(maxAttempts))
	return func() { transientMaxAttemptsOverride.Store(prev) }
}
