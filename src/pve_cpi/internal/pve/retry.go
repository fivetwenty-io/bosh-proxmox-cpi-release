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
var jitterSource = mrand.New(mrand.NewPCG(mrand.Uint64(), mrand.Uint64())) // #nosec G404 -- backoff jitter; non-cryptographic

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
// used by RetryOnTransient and RetryOnTransientOrLock.
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
// (0-indexed) failed lock acquisition: exponential base × 1.5^attempt with
// ±jitterPct% jitter, capped at the configured ceiling. Default base 2s,
// cap 30s, jitter ±30%. PVE serialises every per-storage operation (import,
// resize, alloc, free, snapshot) through the same lockfile so retrying
// immediately wins nothing — pause seconds, let the holder finish.
//
// The curve is configurable via ConfigureStorageLockBackoff (called once at
// startup from operator config); tests may override via SetStorageLockBackoffForTest.
func StorageLockBackoff(attempt int) time.Duration {
	baseMs, capMs, jPct := storageLockDefaults()
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
	// jitterPct is in [0,100]. jitterWindow = d * 2*jPct/100 (±jPct% of d).
	// Guard against Int64N(0) which panics.
	if jPct < 0 {
		jPct = 0
	}
	if jPct > 100 {
		jPct = 100
	}
	jitterWindow := int64(d) * int64(jPct) * 2 / 100
	var jitter time.Duration
	if jitterWindow > 0 {
		jitter = time.Duration(jitterInt64N(jitterWindow))
	}
	// Shift the window so it is centered: subtract jPct% then add the draw.
	out := d - time.Duration(int64(d)*int64(jPct)/100) + jitter
	if out > maxBackoff {
		out = maxBackoff
	}
	return out
}

// StorageLockBackoffCap returns the maximum duration that StorageLockBackoff
// can return. Reads the current process-wide cap from the storage-lock seam
// so the operator's configured value (or the shipped 30s default) is always
// returned.
func StorageLockBackoffCap() time.Duration {
	_, capMs, _ := storageLockDefaults()
	return time.Duration(capMs) * time.Millisecond
}

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
// maxAttempts ≤ 0 falls back to DefaultTransientMaxAttempts, or the
// operator override installed by ConfigureTransientRetry.
func RetryOnTransient(
	ctx context.Context,
	logger *log.Logger,
	label string,
	maxAttempts int,
	op func() error,
) error {
	if maxAttempts <= 0 {
		maxAttempts = transientMaxAttemptsDefault()
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
				log.Err(err),
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
// RetryOnTransientOrLock retry iteration. Quorum loss and storage-lock
// contention are checked BEFORE pushback: IsPVEPushback substring-matches
// "got timeout", which is present in both canonical storage-lock error
// shapes, so a pushback-first ordering routed every storage-lock error onto
// the pushback curve — silently ignoring the operator's tuned
// pve.retry.storage_lock settings and mislabeling the log reason as
// "pushback" for a storage-subsystem condition. Quorum loss and storage-lock
// contention share the StorageLockBackoff curve (2s→30s) — both are "wait
// for someone else to finish/recover" conditions on a similar timescale;
// isQuorum before isLock only gives the log line an accurate reason label.
// Genuine pushback (worker-pool saturation, rate limiting) still gets the
// most conservative PushbackBackoff curve when no lock/quorum signal matched.
func retryOrLockCurve(isPushback, isLock, isQuorum bool, attempt int) (d time.Duration, reason string) {
	switch {
	case isQuorum:
		return StorageLockBackoff(attempt), "cluster_not_quorate"
	case isLock:
		return StorageLockBackoff(attempt), "storage_lock"
	case isPushback:
		return PushbackBackoff(attempt), "pushback"
	default:
		return TransientBackoff(attempt), "transient_transport"
	}
}

// RetryOnTransientOrLock invokes op up to maxAttempts times, retrying when the
// returned error is a per-storage lockfile timeout (IsStorageLockTimeout), a
// cluster quorum-loss condition (IsClusterNotQuorate), a transient transport-
// layer fault (IsTransientTransport), or a PVE rate-limit / worker-pool
// exhaustion signal (IsPVEPushback). Backoff curve priority: quorum loss /
// storage lock (StorageLockBackoff) > pushback (PushbackBackoff) > transient
// (TransientBackoff) — lock/quorum first because IsPVEPushback is a textual
// superset of the storage-lock shapes (see retryOrLockCurve).
//
// Quorum loss rides the storage-lock curve rather than the transient curve
// deliberately: a quorum 5xx also satisfies IsTransientTransport (it is a
// plain 5xx at the transport level), so without this explicit routing it
// would fall through to the shorter TransientBackoff curve (1s→15s, 8
// attempts) — mismatched to a minutes-scale condition (node loss below
// majority, corosync partition) that the seconds-scale worker-cycling curve
// was never tuned for.
//
// Use for storage-touching SDK calls that may collide on any of these
// conditions. Backoff continues from a single attempt counter across all
// failure modes.
//
// maxAttempts ≤ 0 falls back to DefaultStorageLockMaxAttempts (the longer of
// the non-pushback budgets — the helper subsumes all failure modes).
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
		isQuorum := IsClusterNotQuorate(err)
		isTransient := IsTransientTransport(err)
		if !isPushback && !isLock && !isQuorum && !isTransient {
			return err
		}
		lastErr = err
		if attempt == maxAttempts-1 {
			break
		}
		d, reason := retryOrLockCurve(isPushback, isLock, isQuorum, attempt)
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
				log.Err(err),
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

// RetryOnTransientOrUnplugBusy invokes op like RetryOnTransient — retrying
// transport faults (IsTransientTransport) and pushback (IsPVEPushback) — and
// additionally retries PVE's hot-unplug "still busy in guest" rejection
// (IsHotUnplugBusy) on the same TransientBackoff curve. QEMU can hold a drive
// busy for a few seconds after a snapshot or an I/O burst; the settle window
// fits comfortably inside the transient budget (~30s across
// DefaultTransientMaxAttempts), while a disk the guest genuinely holds keeps
// failing and surfaces after the bound.
//
// Use for hot-unplug config edits (detach_disk); other callers have no
// hot-unplug surface and should stay on RetryOnTransient.
//
// maxAttempts ≤ 0 falls back to DefaultTransientMaxAttempts, or the
// operator override installed by ConfigureTransientRetry.
func RetryOnTransientOrUnplugBusy(
	ctx context.Context,
	logger *log.Logger,
	label string,
	maxAttempts int,
	op func() error,
) error {
	if maxAttempts <= 0 {
		maxAttempts = transientMaxAttemptsDefault()
	}
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		err := op()
		if err == nil {
			return nil
		}
		isPushback := IsPVEPushback(err)
		isBusy := IsHotUnplugBusy(err)
		if !isPushback && !isBusy && !IsTransientTransport(err) {
			return err
		}
		lastErr = err
		if attempt == maxAttempts-1 {
			break
		}
		d := TransientBackoff(attempt)
		reason := "transient_transport"
		if isPushback {
			d = PushbackBackoff(attempt)
			reason = "pve_pushback"
		} else if isBusy {
			reason = "hot_unplug_busy"
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
				log.Err(err),
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
