package pve

import (
	"context"
	"sync"
	"sync/atomic"
)

// duplicateBackingWarnOnce gates the duplicate-backing warning to a single
// firing per process, across EVERY path that builds a full StorageInfo index —
// StorageInfoCache.refresh (the deploy path) and create_stemcell's PolicyDeps
// adapter (the `bosh upload-stemcell` path, which never touches the cache).
// A process-wide gate rather than a per-cache one because the two paths would
// otherwise each warn about the same storage.cfg, and because storage.cfg
// duplicate-backing misconfiguration is static operator state: it does not
// flip on a routine TTL refresh, so re-warning would be process-lifetime log
// noise for a condition that cannot change without an operator edit and a CPI
// restart.
//
// Held in an atomic.Pointer rather than a plain sync.Once so
// ResetDuplicateBackingWarnOnceForTest can swap in a fresh gate race-safely.
// This mirrors the package-level seam pattern used for the backoff curves
// (pushback_seam.go, storage_lock_seam.go).
var duplicateBackingWarnOnce atomic.Pointer[sync.Once]

func init() {
	duplicateBackingWarnOnce.Store(&sync.Once{})
}

// WarnDuplicateBackingStoragesOnce runs WarnDuplicateBackingStorages at most
// once per process. Callers pass the FULL storage index they just decoded;
// passing a partial index would let whichever path happens to run first fix a
// misleading (incomplete) warning as the process's only one.
//
// Safe to call from any path that has just decoded /storage — it is pure
// logging over the caller's slice and never re-enters the caller.
func WarnDuplicateBackingStoragesOnce(ctx context.Context, infos []StorageInfo) {
	duplicateBackingWarnOnce.Load().Do(func() {
		WarnDuplicateBackingStorages(ctx, infos)
	})
}

// ResetDuplicateBackingWarnOnceForTest re-arms the process-wide gate for a test
// and returns a restore function. Exported (in a non-test file) so tests in
// other packages — the create_stemcell PolicyDeps path lives in
// internal/cpi/handlers — can drive the warning deterministically.
//
// Tests using this seam must NOT call t.Parallel: the gate is process-wide, so
// two parallel tests would race over which one gets the single firing.
//
//	defer pve.ResetDuplicateBackingWarnOnceForTest()()
func ResetDuplicateBackingWarnOnceForTest() func() {
	prev := duplicateBackingWarnOnce.Swap(&sync.Once{})
	return func() {
		duplicateBackingWarnOnce.Store(prev)
	}
}
