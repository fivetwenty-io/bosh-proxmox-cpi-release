// Cross-process advisory cluster mutex built on PVE resource pools.
//
// PVE has no general-purpose cluster key-value store the CPI can use as a
// mutex, but POST /pools is serialized by pmxcfs and rejects a duplicate
// poolid with a 4xx error. That create-or-fail behavior is exactly a
// test-and-set: the process that creates the sentinel pool holds the lock; a
// concurrent process sees the 4xx and either waits or, if the recorded
// expiry has passed, steals the lock.
//
// The lock is advisory and best-effort. It serializes the CPI's own
// read-modify-write on shared HA anti-affinity rules across concurrent create_vm
// invocations on different hosts; it is not a guarantee against a malicious or
// non-CPI mutator of the same pool name. The sentinel poolid is namespaced
// ("bosh-lock-...") to avoid colliding with operator pools.
//
// LIVE-VALIDATION CAVEAT: the exact PVE HTTP status and message for a duplicate
// poolid, and the comment round-trip, are inferred from the API shape and the
// pmxcfs serialization model. Unit tests assert this contract against a fake
// PoolService; a true multi-process race must be validated on a live cluster.
package pve

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	sdkerrors "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/errors"
)

// clusterLockPoolPrefix namespaces every sentinel pool so a lock pool is never
// confused with an operator-managed resource pool.
const clusterLockPoolPrefix = "bosh-lock-"

// clusterLockPollInterval is the base delay between acquire attempts when the
// lock is held by a live (non-expired) owner. A small per-attempt jitter is
// added on top so concurrent waiters do not synchronize.
const clusterLockPollInterval = 500 * time.Millisecond

// ClusterLockHandle is returned by AcquireClusterLock and released via Release.
// It is safe to call Release more than once; the second call is a no-op.
type ClusterLockHandle struct {
	pool     string
	owner    string
	released bool
	pools    PoolService
}

// nowFunc and sleepFunc are seams so tests can drive the acquire loop
// deterministically without real wall-clock sleeps. Production uses time.Now
// and a context-aware sleep.
type lockClock struct {
	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) error
}

func defaultLockClock() lockClock {
	return lockClock{
		now: time.Now,
		sleep: func(ctx context.Context, d time.Duration) error {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.C:
				return nil
			}
		},
	}
}

// ClusterLockPoolName renders the sentinel pool id for a logical lock name,
// e.g. "aa-web" -> "bosh-lock-aa-web". The name is sanitized to the characters
// PVE permits in a poolid (alphanumeric, dash, underscore); any other rune is
// replaced with a dash so an arbitrary instance-group key is always a legal id.
func ClusterLockPoolName(name string) string {
	var b strings.Builder
	b.WriteString(clusterLockPoolPrefix)
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

// AcquireClusterLock acquires the named cross-process lock, blocking until it is
// held, the timeout elapses, or ctx is cancelled.
//
// name is the logical lock key (e.g. an instance-group name); it is namespaced
// and sanitized into a sentinel poolid. owner is a caller-supplied token that
// uniquely identifies this acquirer (e.g. "<request_id>/<pid>/<vmid>"); it is
// stamped into the pool comment for diagnostics and is required (empty owner is
// rejected). ttl bounds how long a held lock is considered live: a holder whose
// recorded expiry has passed is treated as crashed and its lock is stolen.
// timeout bounds the total wait; on timeout a TypeRetriableCloud error is
// returned so the BOSH director re-drives the operation.
//
// The returned handle's Release must be deferred by the caller; it deletes the
// sentinel pool and is idempotent and best-effort.
func AcquireClusterLock(
	ctx context.Context, pools PoolService, name, owner string, ttl, timeout time.Duration,
) (*ClusterLockHandle, error) {
	return acquireClusterLockWithClock(ctx, pools, name, owner, ttl, timeout, defaultLockClock())
}

func acquireClusterLockWithClock(
	ctx context.Context, pools PoolService, name, owner string, ttl, timeout time.Duration, clk lockClock,
) (*ClusterLockHandle, error) {
	if pools == nil {
		return nil, cpierrors.Cloud("AcquireClusterLock: pool service must not be nil")
	}
	if owner == "" {
		return nil, cpierrors.Cloud("AcquireClusterLock: owner token must not be empty")
	}
	if ttl <= 0 {
		return nil, cpierrors.Cloud("AcquireClusterLock: ttl must be positive, got %s", ttl)
	}
	if timeout <= 0 {
		return nil, cpierrors.Cloud("AcquireClusterLock: timeout must be positive, got %s", timeout)
	}

	pool := ClusterLockPoolName(name)
	deadline := clk.now().Add(timeout)

	for {
		comment := encodeLockComment(owner, clk.now().Add(ttl))
		createErr := pools.CreatePool(ctx, pool, comment)
		if createErr == nil {
			// Won the race: the sentinel pool is now ours.
			return &ClusterLockHandle{pool: pool, owner: owner, pools: pools}, nil
		}
		if !isPoolAlreadyExists(createErr) {
			// A non-duplicate failure (auth, transport, pmxcfs error) is mapped to
			// a retriable cloud error so the director re-drives rather than failing
			// the deploy on a transient lock-acquire fault.
			return nil, cpierrors.WrapAs(createErr, cpierrors.TypeRetriableCloud,
				fmt.Sprintf("AcquireClusterLock: create sentinel pool %q", pool))
		}

		// Pool exists: inspect the holder's recorded expiry to decide steal-or-wait.
		if stole, err := tryStealExpired(ctx, pools, pool, owner, ttl, clk); err != nil {
			return nil, err
		} else if stole != nil {
			return stole, nil
		}

		// Held by a live owner: wait and retry until the timeout.
		now := clk.now()
		if !now.Before(deadline) {
			return nil, cpierrors.WrapAs(createErr, cpierrors.TypeRetriableCloud,
				fmt.Sprintf("AcquireClusterLock: timed out after %s waiting for lock %q", timeout, pool))
		}
		wait := clusterLockPollInterval + time.Duration(jitterInt64N(int64(clusterLockPollInterval)))
		if remaining := deadline.Sub(now); wait > remaining {
			wait = remaining
		}
		if sleepErr := clk.sleep(ctx, wait); sleepErr != nil {
			return nil, cpierrors.WrapAs(sleepErr, cpierrors.TypeRetriableCloud,
				fmt.Sprintf("AcquireClusterLock: interrupted waiting for lock %q", pool))
		}
	}
}

// tryStealExpired reads the existing sentinel pool's comment and, when the
// recorded expiry has passed (or the comment is unreadable/malformed), attempts
// to steal the lock via delete+recreate. It returns a non-nil handle ONLY after
// confirming the post-steal pool carries OUR owner token. It returns (nil, nil)
// when the lock is still live or when another stealer won the race, signalling
// the caller to wait/retry.
//
// Residual steal race: two stealers A,B may both read the expired comment,
// both delete, and both try to recreate. One wins CreatePool; the other sees dup
// and loops. However, A (the first creator) can then have its freshly-created
// pool deleted by B's DeletePool (which runs before B's CreatePool), transiently
// giving both A and B a live handle. The post-steal re-read below closes this
// window for the FIRST stealer: if our comment is not present after recreate,
// we yield the handle and loop. The residual window is the period between
// CreatePool success and the re-read GET — a concurrent B delete in that window
// would cause GetPoolComment to see B's owner or nothing, and we correctly loop.
// The read-after-write verify in verifyAntiAffinityMember is the correctness
// backstop: a double-held RMW produces one canonical rule (last writer), and
// verify catches a lost member. Together the two mechanisms bound the impact.
func tryStealExpired(
	ctx context.Context, pools PoolService, pool, owner string, ttl time.Duration, clk lockClock,
) (*ClusterLockHandle, error) {
	comment, found, err := pools.GetPoolComment(ctx, pool)
	if err != nil {
		return nil, cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud,
			fmt.Sprintf("AcquireClusterLock: read holder of lock %q", pool))
	}
	if !found {
		// The pool vanished between CreatePool and the read: another process
		// released it. Signal the caller to retry the create immediately.
		return nil, nil
	}

	exp, ok := decodeLockExpiry(comment)
	live := ok && clk.now().Before(exp)
	if live {
		// A live owner holds it; the caller must wait.
		return nil, nil
	}

	// Expired or unparseable holder: steal by delete+recreate. A not-found on
	// delete means someone else already released/stole it — fall through to the
	// recreate, which is the authoritative test-and-set.
	if delErr := pools.DeletePool(ctx, pool); delErr != nil && !isPoolNotFound(delErr) {
		return nil, cpierrors.WrapAs(delErr, cpierrors.TypeRetriableCloud,
			fmt.Sprintf("AcquireClusterLock: steal-delete lock %q", pool))
	}
	recreateComment := encodeLockComment(owner, clk.now().Add(ttl))
	if createErr := pools.CreatePool(ctx, pool, recreateComment); createErr != nil {
		if isPoolAlreadyExists(createErr) {
			// Another stealer won the recreate; loop back to wait/steal.
			return nil, nil
		}
		return nil, cpierrors.WrapAs(createErr, cpierrors.TypeRetriableCloud,
			fmt.Sprintf("AcquireClusterLock: steal-recreate lock %q", pool))
	}

	// Post-steal owner verification: re-read the pool and confirm
	// our owner token is the one persisted. If a concurrent stealer B deleted our
	// freshly-created pool between our CreatePool and this re-read, we will see B's
	// owner (or nothing) and correctly refuse the handle, looping back to wait/steal.
	verifyComment, verifyFound, verifyErr := pools.GetPoolComment(ctx, pool)
	if verifyErr != nil {
		// Cannot confirm — yield the handle and let the caller retry.
		return nil, cpierrors.WrapAs(verifyErr, cpierrors.TypeRetriableCloud,
			fmt.Sprintf("AcquireClusterLock: verify steal of lock %q", pool))
	}
	if !verifyFound || !strings.Contains(verifyComment, "owner="+owner+" ") {
		// Our comment is not present: another stealer displaced us. Re-loop.
		return nil, nil
	}

	return &ClusterLockHandle{pool: pool, owner: owner, pools: pools}, nil
}

// Release deletes the sentinel pool, freeing the lock. It is idempotent: a
// second call is a no-op, and a not-found pool is treated as success (the lock
// may already have been stolen after expiry). All other delete failures are
// returned for the caller to log; Release never blocks the surrounding
// operation. A nil handle Release is a no-op.
func (h *ClusterLockHandle) Release(ctx context.Context) error {
	if h == nil || h.released {
		return nil
	}
	h.released = true
	if err := h.pools.DeletePool(ctx, h.pool); err != nil && !isPoolNotFound(err) {
		return cpierrors.Wrap(err, fmt.Sprintf("ReleaseClusterLock: delete sentinel pool %q", h.pool))
	}
	return nil
}

// PoolName returns the sentinel pool id backing this lock (diagnostics/tests).
func (h *ClusterLockHandle) PoolName() string {
	if h == nil {
		return ""
	}
	return h.pool
}

// lockCommentPrefix and lockCommentExpKey frame the structured comment stamped
// on a sentinel pool: "owner=<token> exp=<unix-seconds>".
const (
	lockCommentOwnerKey = "owner="
	lockCommentExpKey   = "exp="
)

// encodeLockComment renders the sentinel pool comment for an owner and expiry.
func encodeLockComment(owner string, exp time.Time) string {
	return lockCommentOwnerKey + owner + " " + lockCommentExpKey + strconv.FormatInt(exp.Unix(), 10)
}

// decodeLockExpiry extracts the expiry time from a sentinel pool comment.
// ok is false when no parseable exp= token is present, in which case the caller
// treats the holder as expired (stale/foreign comment → reclaimable).
func decodeLockExpiry(comment string) (time.Time, bool) {
	for _, field := range strings.Fields(comment) {
		if rest, found := strings.CutPrefix(field, lockCommentExpKey); found {
			secs, err := strconv.ParseInt(rest, 10, 64)
			if err != nil {
				return time.Time{}, false
			}
			return time.Unix(secs, 0), true
		}
	}
	return time.Time{}, false
}

// isPoolAlreadyExists reports whether err positively indicates the sentinel pool
// id is already taken — the duplicate-create signal that means "lock held".
//
// Fail-closed contract: only returns true when we can POSITIVELY classify the
// error as "duplicate pool". An unrecognised error is NOT treated as a duplicate;
// the caller maps it to a retriable failure rather than erroneously concluding
// the lock is held. This avoids the fail-open footgun where an unrelated 4xx
// (e.g. an auth error or a parameter error) is mistaken for "lock already held"
// and causes spurious steal/wait behaviour.
//
// Primary check: SDK sentinel ErrConflict (HTTP 409), which PVE uses for
// duplicate resource creation. Secondary: the string "already exists" /
// "already defined" covers versions that return non-409 text errors for dups.
// LIVE-VALIDATION CAVEAT: exact PVE HTTP code for duplicate poolid is inferred
// (409 is the standard conflict code; PVE pool creation may also use 500 with
// a text body — test against a live cluster and add to secondary if needed).
func isPoolAlreadyExists(err error) bool {
	if err == nil {
		return false
	}
	// Prefer SDK sentinel — errors.Is traverses the Unwrap chain.
	if errors.Is(err, sdkerrors.ErrConflict) {
		return true
	}
	// Secondary: text substring for PVE versions that use non-409 codes.
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "already exists") ||
		strings.Contains(msg, "already defined")
}
