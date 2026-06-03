package handlers

import (
	"sync/atomic"
	"time"
)

// healthPollMinIntervalNs is the production floor for the health-check poll
// interval, stored as nanoseconds in an atomic int64 so tests can race-safely
// override it. Default 1s. An operator-supplied IntervalSec of 0 is clamped
// to this value so the loop never becomes a tight busy-loop in production.
var healthPollMinIntervalNs atomic.Int64

func init() {
	healthPollMinIntervalNs.Store(int64(1 * time.Second))
}

// healthPollMinInterval returns the current effective floor as a Duration.
func healthPollMinInterval() time.Duration {
	return time.Duration(healthPollMinIntervalNs.Load())
}

// SetHealthPollMinInterval replaces the floor for the duration of a test and
// returns a restore function. The zero value disables the floor (allows
// unbounded fast polling) so unit tests do not pay real-time waits.
//
//	defer handlers.SetHealthPollMinInterval(0)()
func SetHealthPollMinInterval(d time.Duration) func() {
	prev := healthPollMinIntervalNs.Swap(int64(d))
	return func() { healthPollMinIntervalNs.Store(prev) }
}
