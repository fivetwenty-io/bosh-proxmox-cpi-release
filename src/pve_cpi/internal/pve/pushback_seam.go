package pve

import (
	"sync/atomic"
	"time"
)

// Pushback-backoff curve defaults, stored in atomics so they can be configured
// once at process startup (ConfigurePushbackBackoff) and overridden race-safely
// in tests. The shipped defaults reproduce the constants PushbackBackoff used
// before the retry config block existed, so an unconfigured process backs off
// identically to prior releases.
//
// This mirrors the package-level seam pattern used for task-poll cadence
// (task_poll_seam.go): BOSH runs one CPI process per invocation with a single
// config, so a startup-time package default is the simplest correct wiring and
// avoids threading a backoff policy through every RetryOnTransient(OrLock) call
// site.
var (
	pushbackBaseNs atomic.Int64 // default 5000ms
	pushbackCapNs  atomic.Int64 // default 60000ms
)

const (
	defaultPushbackSeamBaseMs = 5000
	defaultPushbackSeamCapMs  = 60000
)

func init() {
	pushbackBaseNs.Store(int64(defaultPushbackSeamBaseMs) * int64(time.Millisecond))
	pushbackCapNs.Store(int64(defaultPushbackSeamCapMs) * int64(time.Millisecond))
}

// ConfigurePushbackBackoff sets the process-wide pushback-backoff curve from
// operator config. Each argument is applied only when > 0, so a
// partially-specified policy keeps the shipped default for the unset fields.
// capMs is clamped up to baseMs so the curve never has a ceiling below its own
// base. Call once at startup, before serving requests.
func ConfigurePushbackBackoff(baseMs, capMs int) {
	if baseMs > 0 {
		pushbackBaseNs.Store(int64(baseMs) * int64(time.Millisecond))
	}
	if capMs > 0 {
		cm := capMs
		if base := int(pushbackBaseNs.Load() / int64(time.Millisecond)); cm < base {
			cm = base
		}
		pushbackCapNs.Store(int64(cm) * int64(time.Millisecond))
	}
}

// pushbackDefaults returns the current effective pushback-backoff bounds in
// milliseconds.
func pushbackDefaults() (baseMs, capMs int) {
	baseMs = int(pushbackBaseNs.Load() / int64(time.Millisecond))
	capMs = int(pushbackCapNs.Load() / int64(time.Millisecond))
	return baseMs, capMs
}

// SetPushbackBackoffForTest overrides the pushback-backoff curve for a test and
// returns a restore function. Mirrors SetTaskPollingForTest.
//
//	defer pve.SetPushbackBackoffForTest(500, 2000)()
func SetPushbackBackoffForTest(baseMs, capMs int) func() {
	prevB := pushbackBaseNs.Swap(int64(baseMs) * int64(time.Millisecond))
	prevC := pushbackCapNs.Swap(int64(capMs) * int64(time.Millisecond))
	return func() {
		pushbackBaseNs.Store(prevB)
		pushbackCapNs.Store(prevC)
	}
}
