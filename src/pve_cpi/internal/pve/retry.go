// Note: time.After timers used in the select statements below are reaped by
// the Go 1.23+ runtime, which tracks and collects them even when the select
// arm is never reached. This module requires Go 1.23+ (go.mod declares go
// 1.25). If a toolchain downgrade below 1.23 is ever proposed, convert each
// time.After call to the NewTimer/Stop pattern explicitly to avoid goroutine
// leaks on ctx.Done cancellations.
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

// DefaultTransientMaxAttempts bounds transport-layer retries (pvedaemon worker
// cycle, pveproxy backend stalls, auth-ticket EOF). Worker restart is
// sub-second so a higher attempt count is cheap and rarely consumed in full.
const DefaultTransientMaxAttempts = 8

// TransientBackoff returns the sleep duration after the attempt-th (0-indexed)
// failed transport call: exponential 1s × 1.5^attempt with ±30% jitter, capped
// at 15s. Tuned for pvedaemon worker recycling, which completes in roughly a
// second; longer waits buy nothing.
func TransientBackoff(attempt int) time.Duration {
	// maxBackoff renamed from the builtin-shadowing `cap` so go vet stops
	// flagging this scope and the symbol is unambiguous when reading the
	// jitter math below.
	const maxBackoff = 15 * time.Second
	base := 1 * time.Second
	factor := 1.0
	for i := 0; i < attempt; i++ {
		factor *= 1.5
	}
	d := time.Duration(float64(base) * factor)
	if d > maxBackoff {
		d = maxBackoff
	}
	// Guard against Int64N(0) which panics. With a zero/negative jitter
	// window fall back to the deterministic base delay.
	jitterWindow := int64(d) * 6 / 10
	var jitter time.Duration
	if jitterWindow > 0 {
		jitter = time.Duration(mrand.Int64N(jitterWindow)) // #nosec G404 -- backoff jitter; non-cryptographic
	}
	out := d - d*3/10 + jitter
	if out > maxBackoff {
		out = maxBackoff
	}
	return out
}

// StorageLockBackoff returns the sleep duration after the attempt-th
// (0-indexed) failed lock acquisition: exponential 2s × 1.5^attempt with
// ±30% jitter, capped at 30s. PVE serialises every per-storage operation
// (import, resize, alloc, free, snapshot) through the same lockfile so
// retrying immediately wins nothing — pause seconds, let the holder finish.
func StorageLockBackoff(attempt int) time.Duration {
	// maxBackoff renamed from the builtin-shadowing `cap` so go vet stops
	// flagging this scope; jitter math reads cleaner with the explicit name.
	const maxBackoff = 30 * time.Second
	base := 2 * time.Second
	factor := 1.0
	for i := 0; i < attempt; i++ {
		factor *= 1.5
	}
	d := time.Duration(float64(base) * factor)
	if d > maxBackoff {
		d = maxBackoff
	}
	// Guard against Int64N(0) which panics. With a zero/negative jitter
	// window fall back to the deterministic base delay.
	jitterWindow := int64(d) * 6 / 10
	var jitter time.Duration
	if jitterWindow > 0 {
		jitter = time.Duration(mrand.Int64N(jitterWindow)) // #nosec G404 -- backoff jitter; non-cryptographic
	}
	out := d - d*3/10 + jitter
	if out > maxBackoff {
		out = maxBackoff
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

// RetryOnTransient invokes op up to maxAttempts times, retrying only when the
// returned error is a transient transport-layer fault (IsTransientTransport).
// Other errors (or success) return immediately. Backoff uses TransientBackoff.
// Context cancellation short-circuits the sleep.
//
// Use for SDK calls that don't touch the per-storage lock (config edits,
// listings, login) but can still die to a pvedaemon worker recycling.
//
// maxAttempts ≤ 0 falls back to DefaultTransientMaxAttempts.
func RetryOnTransient(
	ctx context.Context,
	logger *log.Logger,
	label string,
	maxAttempts int,
	op func() error,
) error {
	if maxAttempts <= 0 {
		maxAttempts = DefaultTransientMaxAttempts
	}
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		err := op()
		if err == nil {
			return nil
		}
		if !IsTransientTransport(err) {
			return err
		}
		lastErr = err
		if attempt == maxAttempts-1 {
			break
		}
		d := TransientBackoff(attempt)
		if logger != nil {
			logger.Info("pve: transient transport fault, retrying",
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

// RetryOnTransientOrLock invokes op up to maxAttempts times, retrying when the
// returned error is either a per-storage lockfile timeout (IsStorageLockTimeout)
// or a transient transport-layer fault (IsTransientTransport). The longer
// StorageLockBackoff curve is used because storage-lock holds dominate the
// wait when both failure modes coexist; a sub-second transport retry inside
// a 30-second lock window is just noise.
//
// Use for storage-touching SDK calls that may collide on either condition.
// Backoff continues from a single attempt counter — a deploy that hits a
// transient fault on attempt 3 then a lock timeout on attempt 4 keeps
// climbing the same curve rather than restarting from zero.
//
// maxAttempts ≤ 0 falls back to DefaultStorageLockMaxAttempts (the longer of
// the two budgets — the helper subsumes both failure modes).
func RetryOnTransientOrLock(
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
		isLock := IsStorageLockTimeout(err)
		isTransient := IsTransientTransport(err)
		if !isLock && !isTransient {
			return err
		}
		lastErr = err
		if attempt == maxAttempts-1 {
			break
		}
		// Transport faults clear in sub-second, lock holds in tens of
		// seconds; pick the shorter curve when only the transport mode
		// fired so a fast retry doesn't waste the lock-tuned budget.
		var d time.Duration
		if isLock {
			d = StorageLockBackoff(attempt)
		} else {
			d = TransientBackoff(attempt)
		}
		if logger != nil {
			reason := "transient_transport"
			if isLock {
				reason = "storage_lock"
			}
			logger.Info("pve: retrying after retryable fault",
				log.String("op", label),
				log.String("reason", reason),
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
