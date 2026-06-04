package handlers

import (
	"sync/atomic"
	"time"
)

// resizeConvergencePollIntervalNs is the wait between size-convergence polls in
// resize_disk (§7.27), stored as nanoseconds in an atomic int64 so tests can
// race-safely shrink it. Default 2s — long enough not to add poll pressure to a
// busy cluster, short enough to converge promptly on a healthy backend.
var resizeConvergencePollIntervalNs atomic.Int64

func init() {
	resizeConvergencePollIntervalNs.Store(int64(2 * time.Second))
}

// resizeConvergencePollInterval returns the current inter-poll wait.
func resizeConvergencePollInterval() time.Duration {
	return time.Duration(resizeConvergencePollIntervalNs.Load())
}

// SetResizeConvergencePollInterval replaces the inter-poll wait for the duration
// of a test and returns a restore function. A tiny value keeps convergence unit
// tests instant.
//
//	defer handlers.SetResizeConvergencePollInterval(time.Millisecond)()
func SetResizeConvergencePollInterval(d time.Duration) func() {
	prev := resizeConvergencePollIntervalNs.Swap(int64(d))
	return func() { resizeConvergencePollIntervalNs.Store(prev) }
}
