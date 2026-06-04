package pve

import (
	"sync/atomic"
	"time"
)

// Task-polling cadence defaults, stored in atomics so they can be configured
// once at process startup (ConfigureTaskPolling) and overridden race-safely in
// tests. The shipped defaults reproduce the constants AwaitTask used before the
// retry config block existed, so an unconfigured process polls identically to
// prior releases.
//
// This mirrors the package-level seam pattern already used for the health-check
// poll floor: BOSH runs one CPI process per invocation with a single config, so
// a startup-time package default is the simplest correct wiring and avoids
// threading a poll policy through every one of the ~20 AwaitTask call sites.
var (
	taskPollIntervalNs    atomic.Int64 // default 2000ms
	taskPollMaxIntervalNs atomic.Int64 // default 10000ms
	taskPollJitterPct     atomic.Int64 // default 10
)

// adaptivePollEnabled gates progress-aware adaptive task polling (§7.28). When
// false (default) AwaitTask uses the SDK's fixed-interval Wait, byte-identical
// to prior releases. When true, AwaitTask runs a CPI-owned loop that derives the
// poll interval from the task's reported progress, falling back to the fixed
// cadence when progress is absent.
var adaptivePollEnabled atomic.Bool

// ConfigureAdaptiveTaskPoll sets the process-wide adaptive-poll toggle from
// operator config. Call once at startup, before serving requests.
func ConfigureAdaptiveTaskPoll(enabled bool) { adaptivePollEnabled.Store(enabled) }

// SetAdaptiveTaskPollForTest overrides the adaptive-poll toggle for a test and
// returns a restore function.
//
//	defer pve.SetAdaptiveTaskPollForTest(true)()
func SetAdaptiveTaskPollForTest(enabled bool) func() {
	prev := adaptivePollEnabled.Swap(enabled)
	return func() { adaptivePollEnabled.Store(prev) }
}

func init() {
	taskPollIntervalNs.Store(int64(defaultPollIntervalMs) * int64(time.Millisecond))
	taskPollMaxIntervalNs.Store(int64(defaultPollIntervalMs*5) * int64(time.Millisecond))
	taskPollJitterPct.Store(10)
}

// ConfigureTaskPolling sets the process-wide task-poll cadence from operator
// config. Each argument is applied only when > 0, so a partially-specified
// policy keeps the shipped default for the unset fields. maxIntervalMs is
// clamped up to intervalMs so the SDK never sees a maximum below the base
// interval. Call once at startup, before serving requests.
func ConfigureTaskPolling(intervalMs, maxIntervalMs, jitterPct int) {
	if intervalMs > 0 {
		taskPollIntervalNs.Store(int64(intervalMs) * int64(time.Millisecond))
	}
	if maxIntervalMs > 0 {
		mi := maxIntervalMs
		if base := int(taskPollIntervalNs.Load() / int64(time.Millisecond)); mi < base {
			mi = base
		}
		taskPollMaxIntervalNs.Store(int64(mi) * int64(time.Millisecond))
	}
	if jitterPct > 0 {
		taskPollJitterPct.Store(int64(jitterPct))
	}
}

// taskPollDefaults returns the current effective poll cadence in the units the
// SDK WaitOptions expects (milliseconds for the intervals, percent for jitter).
func taskPollDefaults() (intervalMs, maxIntervalMs, jitterPct int) {
	intervalMs = int(taskPollIntervalNs.Load() / int64(time.Millisecond))
	maxIntervalMs = int(taskPollMaxIntervalNs.Load() / int64(time.Millisecond))
	jitterPct = int(taskPollJitterPct.Load())
	return intervalMs, maxIntervalMs, jitterPct
}

// SetTaskPollingForTest overrides the poll cadence for a test and returns a
// restore function. Mirrors SetHealthPollMinInterval.
//
//	defer pve.SetTaskPollingForTest(1, 1, 0)()
func SetTaskPollingForTest(intervalMs, maxIntervalMs, jitterPct int) func() {
	prevI := taskPollIntervalNs.Swap(int64(intervalMs) * int64(time.Millisecond))
	prevM := taskPollMaxIntervalNs.Swap(int64(maxIntervalMs) * int64(time.Millisecond))
	prevJ := taskPollJitterPct.Swap(int64(jitterPct))
	return func() {
		taskPollIntervalNs.Store(prevI)
		taskPollMaxIntervalNs.Store(prevM)
		taskPollJitterPct.Store(prevJ)
	}
}
