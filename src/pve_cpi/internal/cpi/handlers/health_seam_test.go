package handlers_test

import (
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
)

// setHealthPollMinInterval overrides the production poll-floor for the duration
// of a test. Call with 0 to disable the floor (fast polling in unit tests).
// Returns a restore func; defer it.
func setHealthPollMinInterval(d time.Duration) func() {
	return handlers.SetHealthPollMinInterval(d)
}
