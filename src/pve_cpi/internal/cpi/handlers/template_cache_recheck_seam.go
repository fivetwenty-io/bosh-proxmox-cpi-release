package handlers

import (
	"sync/atomic"
	"time"
)

// templateCacheRecheckDelayNs is the wait between the bounded re-check
// attempts create_vm makes after a stemcell-cache lookup miss (see
// resolveTemplateCacheTargetSettled), stored as nanoseconds in an atomic
// int64 so tests can race-safely shrink it. Default 750ms; with
// templateCacheRecheckAttempts that spans roughly 1.5s of index lag, several
// times the ~1s /cluster/resources delay measured against live PVE 9.2.
var templateCacheRecheckDelayNs atomic.Int64

func init() {
	templateCacheRecheckDelayNs.Store(int64(750 * time.Millisecond))
}

// templateCacheRecheckDelay returns the current inter-attempt wait.
func templateCacheRecheckDelay() time.Duration {
	return time.Duration(templateCacheRecheckDelayNs.Load())
}

// SetTemplateCacheRecheckDelay replaces the inter-attempt wait for the
// duration of a test and returns a restore function.
//
//	defer handlers.SetTemplateCacheRecheckDelay(0)()
func SetTemplateCacheRecheckDelay(d time.Duration) func() {
	prev := templateCacheRecheckDelayNs.Swap(int64(d))
	return func() { templateCacheRecheckDelayNs.Store(prev) }
}
