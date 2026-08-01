package pve

import (
	"sync"
	"time"
)

// ResetVNIZoneListWarnOnce resets the package-level sync.Once guarding the
// zone-level VNI exclusion listing failure warning, for use by pve_test
// package tests that need deterministic once-per-process warn behavior
// across repeated test runs (e.g. `go test -count=2`). Test-only; never
// called from production code.
func ResetVNIZoneListWarnOnce() {
	vniZoneListWarnOnce = sync.Once{}
}

// WithPollIntervalForTest sets the poll interval passed to AwaitTask /
// WaitForSnapshotAbsent's underlying loop. Values ≤ 0 are ignored (default
// applies). Call-scoped (unlike SetTaskPollingForTest's process-wide atomic
// override), so it is safe for t.Parallel() tests that each want a different
// interval without racing each other. Test-only: no production caller
// overrides the poll interval — production call sites only ever tune
// WithMaxWait.
func WithPollIntervalForTest(d time.Duration) AwaitOption {
	return func(o *awaitOptions) {
		if d > 0 {
			o.pollIntervalMs = int(d.Milliseconds())
		}
	}
}
