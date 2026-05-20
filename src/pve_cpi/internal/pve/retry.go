package pve

import (
	"context"
	mrand "math/rand/v2"
	"time"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
)

// DefaultStorageLockMaxAttempts is the bound on per-storage lock retries when
// callers do not specify their own. Each retry waits seconds, not ms (see
// StorageLockBackoff), so even at max-attempts the total wall time is
// bounded under PVE's task watchdog window.
const DefaultStorageLockMaxAttempts = 10

// StorageLockBackoff returns the sleep duration after the attempt-th
// (0-indexed) failed lock acquisition: exponential 2s × 1.5^attempt with
// ±30% jitter, capped at 30s. PVE serialises every per-storage operation
// (import, resize, alloc, free, snapshot) through the same lockfile so
// retrying immediately wins nothing — pause seconds, let the holder finish.
func StorageLockBackoff(attempt int) time.Duration {
	const cap = 30 * time.Second
	base := 2 * time.Second
	factor := 1.0
	for i := 0; i < attempt; i++ {
		factor *= 1.5
	}
	d := time.Duration(float64(base) * factor)
	if d > cap {
		d = cap
	}
	jitter := time.Duration(mrand.Int64N(int64(d) * 6 / 10))
	out := d - d*3/10 + jitter
	if out > cap {
		out = cap
	}
	return out
}

// RetryOnStorageLock invokes op up to maxAttempts times, retrying only when
// the returned error is a PVE per-storage lockfile timeout
// (IsStorageLockTimeout). Other errors (or success) return immediately.
//
// Used to absorb contention against /var/lock/pve-manager/pve-storage-<name>
// from parallel qmcreate / qm resize / qm set / pvesm alloc / pvesm free
// operations during bursts of concurrent CPI calls. Backoff is the package's
// StorageLockBackoff curve. ctx cancellation short-circuits the sleep.
//
// label is an opaque tag used in the retry-log line (e.g. "create_disk",
// "configdrive_upload") so operators can see which surface is contending.
// logger may be nil; if non-nil, one Info line per retry is emitted.
//
// maxAttempts ≤ 0 falls back to DefaultStorageLockMaxAttempts.
func RetryOnStorageLock(
	ctx context.Context,
	logger *log.Logger,
	label string,
	maxAttempts int,
	op func() error,
) error {
	if maxAttempts <= 0 {
		maxAttempts = DefaultStorageLockMaxAttempts
	}
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		err := op()
		if err == nil {
			return nil
		}
		if !IsStorageLockTimeout(err) {
			return err
		}
		lastErr = err
		if attempt == maxAttempts-1 {
			break
		}
		d := StorageLockBackoff(attempt)
		if logger != nil {
			logger.Info("pve: storage lock timeout, retrying",
				log.String("op", label),
				log.Int("attempt", attempt+1),
				log.Int("max_attempts", maxAttempts),
				log.Int("backoff_ms", int(d/time.Millisecond)),
				log.String("error", err.Error()),
			)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(d):
		}
	}
	return lastErr
}
