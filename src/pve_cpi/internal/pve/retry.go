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
	"sync"
	"time"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
)

// jitterMu guards jitterSource so tests can swap it without a data race.
var jitterMu sync.Mutex

// jitterSource is the RNG used by TransientBackoff and StorageLockBackoff.
// The default is a package-level Rand seeded from the global source so
// behavior is identical to the prior direct mrand.Int64N calls. Tests in
// package pve may swap this directly under jitterMu for deterministic output.
var jitterSource = mrand.New(mrand.NewPCG(mrand.Uint64(), mrand.Uint64())) //nolint:gosec // backoff jitter; non-cryptographic

// jitterInt64N returns a non-negative random int64 in [0, n) using the
// package-level jitterSource, holding jitterMu for the duration.
// Panics if n <= 0 (same contract as mrand.Int64N).
func jitterInt64N(n int64) int64 {
	jitterMu.Lock()
	defer jitterMu.Unlock()
	return jitterSource.Int64N(n)
}

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
		jitter = time.Duration(jitterInt64N(jitterWindow))
	}
	out := d - d*3/10 + jitter
	if out > maxBackoff {
		out = maxBackoff
	}
	return out
}

// testBackoffKey is the context key used by WithTestBackoff to install a
// deterministic backoff function for use by the RetryOn* helpers. Test-only.
type testBackoffKey struct{}

// WithTestBackoff returns a derived context that overrides the backoff curve
// used by RetryOnStorageLock, RetryOnTransient, and RetryOnTransientOrLock.
// The returned duration is slept verbatim (return 0 to skip). Intended for
// tests to skip multi-second exponential backoff without changing the retry
// loop's other semantics (attempt count, context cancellation, log lines).
//
// Production code MUST NOT call this — leave the default curves in place.
// The seam costs one context.Value lookup per retry cycle, which is
// negligible compared to a real PVE round-trip.
func WithTestBackoff(ctx context.Context, fn func(attempt int) time.Duration) context.Context {
	return context.WithValue(ctx, testBackoffKey{}, fn)
}

// backoffFromCtx returns the test backoff override installed by
// WithTestBackoff, or nil if none.
func backoffFromCtx(ctx context.Context) func(attempt int) time.Duration {
	fn, _ := ctx.Value(testBackoffKey{}).(func(attempt int) time.Duration)
	return fn
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
		jitter = time.Duration(jitterInt64N(jitterWindow))
	}
	out := d - d*3/10 + jitter
	if out > maxBackoff {
		out = maxBackoff
	}
	return out
}

// StorageLockBackoffCap returns the maximum duration that StorageLockBackoff
// can return. Exported for tests that need to compare curve ceilings.
func StorageLockBackoffCap() time.Duration { return 30 * time.Second }

// PushbackBackoffCap returns the maximum duration that PushbackBackoff can
// return. Reads the current process-wide cap from the pushback seam so the
// operator's configured value (or the shipped 60s default) is always returned.
func PushbackBackoffCap() time.Duration {
	_, capMs := pushbackDefaults()
	return time.Duration(capMs) * time.Millisecond
}

// PushbackBackoff returns the sleep duration after the attempt-th (0-indexed)
// failed call that hit PVE rate-limiting or worker-pool exhaustion:
// exponential base × 1.5^attempt with ±30% jitter, capped at the configured
// ceiling. Default base 5s, default cap 60s — both operator-configurable via
// ConfigurePushbackBackoff.
//
// The longer base (5s vs 2s) and cap (60s vs 30s) relative to
// StorageLockBackoff reflect that PVE worker-pool saturation takes longer to
// drain than a single per-storage lock hold.
func PushbackBackoff(attempt int) time.Duration {
	baseMs, capMs := pushbackDefaults()
	base := time.Duration(baseMs) * time.Millisecond
	maxBackoff := time.Duration(capMs) * time.Millisecond
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
		jitter = time.Duration(jitterInt64N(jitterWindow))
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
		if override := backoffFromCtx(ctx); override != nil {
			d = override(attempt)
		}
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

// RetryOnTransient invokes op up to maxAttempts times, retrying when the
// returned error is a transient transport-layer fault (IsTransientTransport)
// or a PVE rate-limit / worker-pool exhaustion signal (IsPVEPushback).
// Other errors (or success) return immediately. Backoff curve: pushback errors
// use PushbackBackoff; transient errors use TransientBackoff. Context
// cancellation short-circuits the sleep.
//
// Use for SDK calls that don't touch the per-storage lock (config edits,
// listings, login) but can still die to a pvedaemon worker recycling or a
// temporary cluster-saturation pushback.
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
		isPushback := IsPVEPushback(err)
		if !isPushback && !IsTransientTransport(err) {
			return err
		}
		lastErr = err
		if attempt == maxAttempts-1 {
			break
		}
		var d time.Duration
		var reason string
		if isPushback {
			d = PushbackBackoff(attempt)
			reason = "pushback"
		} else {
			d = TransientBackoff(attempt)
			reason = "transient_transport"
		}
		if override := backoffFromCtx(ctx); override != nil {
			d = override(attempt)
		}
		if logger != nil {
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

// retryOrLockCurve selects the backoff curve and reason label for a
// RetryOnTransientOrLock retry iteration. Pushback takes priority (longest
// curve) because worker-pool saturation is the most conservative condition;
// within non-pushback errors, storage-lock dominates transient transport.
func retryOrLockCurve(isPushback, isLock bool, attempt int) (d time.Duration, reason string) {
	switch {
	case isPushback:
		return PushbackBackoff(attempt), "pushback"
	case isLock:
		return StorageLockBackoff(attempt), "storage_lock"
	default:
		return TransientBackoff(attempt), "transient_transport"
	}
}

// RetryOnTransientOrLock invokes op up to maxAttempts times, retrying when the
// returned error is a per-storage lockfile timeout (IsStorageLockTimeout), a
// transient transport-layer fault (IsTransientTransport), or a PVE rate-limit /
// worker-pool exhaustion signal (IsPVEPushback). Backoff curve priority:
// pushback (PushbackBackoff) > storage lock (StorageLockBackoff) > transient
// (TransientBackoff). Pushback is the most conservative because worker-pool
// saturation takes the longest to drain.
//
// Use for storage-touching SDK calls that may collide on any of the three
// conditions. Backoff continues from a single attempt counter across all
// failure modes.
//
// maxAttempts ≤ 0 falls back to DefaultStorageLockMaxAttempts (the longer of
// the non-pushback budgets — the helper subsumes all three failure modes).
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
		isPushback := IsPVEPushback(err)
		isLock := IsStorageLockTimeout(err)
		isTransient := IsTransientTransport(err)
		if !isPushback && !isLock && !isTransient {
			return err
		}
		lastErr = err
		if attempt == maxAttempts-1 {
			break
		}
		d, reason := retryOrLockCurve(isPushback, isLock, attempt)
		if override := backoffFromCtx(ctx); override != nil {
			d = override(attempt)
		}
		if logger != nil {
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
