package handlers

import (
	"sync/atomic"
	"time"
)

// deleteStopPollIntervalNs is the wait between VM-status polls while delete_vm
// waits for a stopped VM (see waitForVMStopped), stored as nanoseconds in an
// atomic int64 so tests can race-safely shrink it. Default 2s.
var deleteStopPollIntervalNs atomic.Int64

// deleteStopWaitBudgetNs bounds how long delete_vm waits for the VM to reach
// "stopped" after the stop task completes. Default 120s — a hard qmstop is
// near-instant, but a graceful guest shutdown (or an HA stop request accepted
// just before the CPI deregistered the resource) can take tens of seconds.
var deleteStopWaitBudgetNs atomic.Int64

func init() {
	deleteStopPollIntervalNs.Store(int64(2 * time.Second))
	deleteStopWaitBudgetNs.Store(int64(120 * time.Second))
}

// deleteStopPollInterval returns the current inter-poll wait.
func deleteStopPollInterval() time.Duration {
	return time.Duration(deleteStopPollIntervalNs.Load())
}

// deleteStopWaitBudget returns the current stopped-wait deadline.
func deleteStopWaitBudget() time.Duration {
	return time.Duration(deleteStopWaitBudgetNs.Load())
}

// SetDeleteStopPollInterval replaces the inter-poll wait for the duration of a
// test and returns a restore function.
//
//	defer handlers.SetDeleteStopPollInterval(time.Millisecond)()
func SetDeleteStopPollInterval(d time.Duration) func() {
	prev := deleteStopPollIntervalNs.Swap(int64(d))
	return func() { deleteStopPollIntervalNs.Store(prev) }
}

// SetDeleteStopWaitBudget replaces the stopped-wait deadline for the duration
// of a test and returns a restore function.
//
//	defer handlers.SetDeleteStopWaitBudget(50 * time.Millisecond)()
func SetDeleteStopWaitBudget(d time.Duration) func() {
	prev := deleteStopWaitBudgetNs.Swap(int64(d))
	return func() { deleteStopWaitBudgetNs.Store(prev) }
}
