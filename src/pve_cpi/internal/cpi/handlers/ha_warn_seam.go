package handlers

import (
	"sync"
	"sync/atomic"
)

// haResurrectorWarnOnce guards the single HA-vs-resurrector warning create_vm
// emits per CPI process (see warnHAResurrectorConflictOnce for the condition
// and the operator remediation). One warning per process is enough to alert
// the operator without flooding logs on every subsequent HA-registered
// create_vm. Process-scoped, not per-feature-set: the first HA-registered
// create_vm in this process warns; later ones (even under a different feature
// combination) do not repeat it.
//
// Held in an atomic.Pointer rather than a bare sync.Once so the test seam
// below can re-arm it race-safely. The previous shape was a plain package var
// that tests reset with an unsynchronized `haResurrectorWarnOnce = sync.Once{}`
// assignment; that happened to be safe only because every test doing it runs
// sequentially, which nothing enforced. Mirrors the seam pattern used for the
// duplicate-backing warning (internal/pve/backing_warn_seam.go) and the
// backoff curves (internal/pve/pushback_seam.go).
var haResurrectorWarnOnce atomic.Pointer[sync.Once]

func init() {
	haResurrectorWarnOnce.Store(&sync.Once{})
}

// resetHAResurrectorWarnOnceForTest re-arms the process-wide gate for a test
// and returns a restore function, so a test that needs to observe the single
// first-fire is repeat-safe under -count=N.
//
// Tests using this seam must NOT call t.Parallel: the gate is process-wide, so
// two parallel tests would race over which one gets the single firing.
//
//	defer resetHAResurrectorWarnOnceForTest()()
func resetHAResurrectorWarnOnceForTest() func() {
	prev := haResurrectorWarnOnce.Swap(&sync.Once{})
	return func() {
		haResurrectorWarnOnce.Store(prev)
	}
}
