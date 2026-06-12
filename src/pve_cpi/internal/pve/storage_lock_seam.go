package pve

import (
	"sync/atomic"
	"time"
)

// Storage-lock backoff curve defaults, stored in atomics so they can be
// configured once at process startup (ConfigureStorageLockBackoff) and
// overridden race-safely in tests. The shipped defaults reproduce the
// constants StorageLockBackoff used before the retry config block existed, so
// an unconfigured process backs off identically to prior releases.
//
// This mirrors the package-level seam pattern used for pushback cadence
// (pushback_seam.go): BOSH runs one CPI process per invocation with a single
// config, so a startup-time package default is the simplest correct wiring and
// avoids threading a backoff policy through every RetryOnStorageLock /
// RetryOnTransientOrLock call site.
var (
	storageLockBaseNs    atomic.Int64 // default 2000ms
	storageLockCapNs     atomic.Int64 // default 30000ms
	storageLockJitterPct atomic.Int64 // default 30 (percent)
)

const (
	defaultStorageLockSeamBaseMs    = 2000
	defaultStorageLockSeamCapMs     = 30000
	defaultStorageLockSeamJitterPct = 30
)

func init() {
	storageLockBaseNs.Store(int64(defaultStorageLockSeamBaseMs) * int64(time.Millisecond))
	storageLockCapNs.Store(int64(defaultStorageLockSeamCapMs) * int64(time.Millisecond))
	storageLockJitterPct.Store(defaultStorageLockSeamJitterPct)
}

// ConfigureStorageLockBackoff sets the process-wide storage-lock backoff curve
// from operator config. Each argument is applied only when > 0, so a
// partially-specified policy keeps the shipped default for the unset fields.
// capMs is clamped up to baseMs so the curve never has a ceiling below its own
// base. jitterPct is clamped to [0, 100]. Call once at startup, before
// serving requests.
func ConfigureStorageLockBackoff(baseMs, capMs, jitterPct int) {
	if baseMs > 0 {
		storageLockBaseNs.Store(int64(baseMs) * int64(time.Millisecond))
	}
	if capMs > 0 {
		cm := capMs
		if base := int(storageLockBaseNs.Load() / int64(time.Millisecond)); cm < base {
			cm = base
		}
		storageLockCapNs.Store(int64(cm) * int64(time.Millisecond))
	}
	if jitterPct > 0 {
		jp := jitterPct
		if jp > 100 {
			jp = 100
		}
		storageLockJitterPct.Store(int64(jp))
	}
}

// storageLockDefaults returns the current effective storage-lock backoff
// bounds in milliseconds and the jitter percentage.
func storageLockDefaults() (baseMs, capMs, jitterPct int) {
	baseMs = int(storageLockBaseNs.Load() / int64(time.Millisecond))
	capMs = int(storageLockCapNs.Load() / int64(time.Millisecond))
	jitterPct = int(storageLockJitterPct.Load())
	return baseMs, capMs, jitterPct
}

// SetStorageLockBackoffForTest overrides the storage-lock backoff curve for a
// test and returns a restore function. Mirrors SetPushbackBackoffForTest.
//
//	defer pve.SetStorageLockBackoffForTest(100, 1000, 10)()
func SetStorageLockBackoffForTest(baseMs, capMs, jitterPct int) func() {
	prevB := storageLockBaseNs.Swap(int64(baseMs) * int64(time.Millisecond))
	prevC := storageLockCapNs.Swap(int64(capMs) * int64(time.Millisecond))
	prevJ := storageLockJitterPct.Swap(int64(jitterPct))
	return func() {
		storageLockBaseNs.Store(prevB)
		storageLockCapNs.Store(prevC)
		storageLockJitterPct.Store(prevJ)
	}
}
