package handlers_test

import (
	"context"
	"time"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
)

// fastRetryCtx returns a derived context with the pve retry backoff curves
// (TransientBackoff, StorageLockBackoff) overridden to zero. Tests that
// exercise RetryOnTransient / RetryOnStorageLock / RetryOnTransientOrLock
// loops use this so the suite doesn't pay multi-second sleeps for behaviour
// that has nothing to do with timing.
func fastRetryCtx(parent context.Context) context.Context {
	return pve.WithTestBackoff(parent, func(int) time.Duration { return 0 })
}
