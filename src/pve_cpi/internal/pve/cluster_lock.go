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
//
// SCOPE: this lock is PER-CLUSTER, never cross-cluster. pmxcfs is the
// per-cluster corosync-backed filesystem that serializes POST /pools; two
// independent PVE clusters each run their own pmxcfs instance, so the same
// sentinel poolid can be created simultaneously in both clusters and each
// will believe it holds the lock. Every RMW this lock protects — VM/disk tag
// and notes updates, stemcell reference counts, anti-affinity HA rule
// membership — is therefore only serialized against other CPI processes
// pointed at THIS cluster. On storage shared between two independent
// clusters (see internal/cpi/handlers/vmid_lock.go and the
// pve.destroy_unreferenced_disks config doc for the resulting data-loss
// path), cross-cluster safety comes from disjoint VMID banding per CPI
// config, not from this lock.
package pve

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

// clusterLockPoolPrefix namespaces every sentinel pool so a lock pool is never
// confused with an operator-managed resource pool.
const clusterLockPoolPrefix = "bosh-lock-"

// clusterLockPollInterval is the base delay between acquire attempts when the
// lock is held by a live (non-expired) owner. A small per-attempt jitter is
// added on top so concurrent waiters do not synchronize.
const clusterLockPollInterval = 500 * time.Millisecond

// clusterLockStealBudget bounds one steal. It runs from the moment the stealer
// sends the re-read that confirms the expired claim to the moment its delete of
// that claim returns. A stealer that has not finished its delete within the
// budget abandons the steal instead of creating, and the next poll starts over
// with an ordinary create, so running over costs one poll and nothing else.
//
// A journal-managed steal delete makes several PVE calls in that span: the
// re-read, the guard's admission reads, the guard's own read of the claim, the
// delete, and the readback, plus two journal writes. The change that set this
// value measured the span, and on a cluster that answers in tens of
// milliseconds two seconds is several times its worst case.
const clusterLockStealBudget = 2 * time.Second

// clusterLockRoundTrip is the allowance for one PVE API round trip in the lock
// arithmetic below.
const clusterLockRoundTrip = 500 * time.Millisecond

// clusterLockDefaultGrace is how long an acquire that takes the grace waits
// after its create before the second readback that confirms its claim. It is
// the steal budget plus one round trip, so a stealer whose re-read went out
// before our create has, by the time we read again, either deleted our sentinel
// or run past its budget and abandoned the steal. Both the steal path and the
// release margin depend on this value.
const clusterLockDefaultGrace = clusterLockStealBudget + clusterLockRoundTrip

// clusterLockGraceNs holds the grace pause that the locks taking it wait out.
// Tests shorten it through SetClusterLockGraceForTest.
var clusterLockGraceNs atomic.Int64

func init() { clusterLockGraceNs.Store(int64(clusterLockDefaultGrace)) }

// clusterLockGrace returns the current grace pause.
func clusterLockGrace() time.Duration { return time.Duration(clusterLockGraceNs.Load()) }

// SetClusterLockGraceForTest replaces the grace pause for a test and returns a
// function that restores it. Production code leaves the grace at
// clusterLockDefaultGrace. A test that changes it must not run in parallel with
// tests that take a lock with the grace.
//
//	defer pve.SetClusterLockGraceForTest(0)()
func SetClusterLockGraceForTest(d time.Duration) func() {
	prev := clusterLockGraceNs.Swap(int64(d))
	return func() { clusterLockGraceNs.Store(prev) }
}

// clusterLockPollNs holds the poll interval acquires wait on. Tests shorten it
// through SetClusterLockPollForTest.
var clusterLockPollNs atomic.Int64

func init() { clusterLockPollNs.Store(int64(clusterLockPollInterval)) }

// clusterLockPoll returns the current poll interval.
func clusterLockPoll() time.Duration { return time.Duration(clusterLockPollNs.Load()) }

// SetClusterLockPollForTest replaces the poll interval for a test and returns a
// function that restores it. Production code leaves the poll at
// clusterLockPollInterval. A test that changes it must not run in parallel with
// tests that take a lock.
//
//	defer pve.SetClusterLockPollForTest(time.Millisecond)()
func SetClusterLockPollForTest(d time.Duration) func() {
	prev := clusterLockPollNs.Swap(int64(d))
	return func() { clusterLockPollNs.Store(prev) }
}

// ClusterLockOption adjusts one acquire.
type ClusterLockOption func(*clusterLockSettings)

type clusterLockSettings struct{ grace bool }

// WithCreateGrace makes the acquire wait out the grace pause after every
// create that PVE accepts, a steal's or an ordinary one, and confirm its claim
// a second time before it takes the lock. The parker protection-window lock
// and the anti-affinity lock take it. The per-VMID lock does not, and
// withVMIDLock says why its remaining window is accepted.
func WithCreateGrace() ClusterLockOption {
	return func(s *clusterLockSettings) { s.grace = true }
}

// graceDuration is the pause this acquire waits after a create, which is zero
// without WithCreateGrace.
func (s clusterLockSettings) graceDuration() time.Duration {
	if !s.grace {
		return 0
	}
	return clusterLockGrace()
}

// releaseMargin is how close to its expiry a claim may be and still be
// deleted by Release. It covers a stealer's whole budget, the grace a stealer's
// create waits before it trusts its claim, and the round trip of our own
// delete. A lock without the grace drops that term.
func (s clusterLockSettings) releaseMargin() time.Duration {
	return clusterLockStealBudget + s.graceDuration() + clusterLockRoundTrip
}

// expectedLockClaimKey carries the claim a sentinel delete expects to remove.
type expectedLockClaimKey struct{}

// WithExpectedLockClaim tells the pool service which claim a sentinel delete
// expects to remove. PVE has no conditional delete, so a service that can read
// the sentinel right before it deletes, as the managed allocation guard does,
// refuses with ErrLockClaimChanged when the claim differs. A service that
// cannot check deletes as before.
func WithExpectedLockClaim(ctx context.Context, claim string) context.Context {
	return context.WithValue(ctx, expectedLockClaimKey{}, claim)
}

// ExpectedLockClaim returns the claim WithExpectedLockClaim set, if any.
func ExpectedLockClaim(ctx context.Context) (string, bool) {
	claim, ok := ctx.Value(expectedLockClaimKey{}).(string)
	return claim, ok
}

// ErrLockClaimChanged is a sentinel delete refused because the sentinel no
// longer holds the claim the caller expected to remove. Nothing was deleted.
var ErrLockClaimChanged = errors.New("lock sentinel no longer holds the expected claim")

// ClusterLockHandle is returned by AcquireClusterLock and released via Release.
// It is safe to call Release more than once; the second call is a no-op.
type ClusterLockHandle struct {
	pool  string
	owner string
	// claim is the comment the confirming readback returned, which is what a
	// guarded delete compares against. PVE is not proven to store a comment
	// byte for byte as it was sent, so the claim never comes from our own copy.
	claim    string
	settings clusterLockSettings
	released bool
	pools    PoolService
	// expiry is the expiry our claim records, in the whole seconds the comment
	// carries, and now is the clock that judges it. Release refuses to delete
	// a claim that is close to it, and WindowDeadline measures from it.
	expiry time.Time
	now    func() time.Time
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
// opts adjust the acquire. A lock whose double holder would do real harm
// passes WithCreateGrace.
//
// The returned handle's Release must be deferred by the caller; it deletes the
// sentinel pool and is idempotent and best-effort.
func AcquireClusterLock(
	ctx context.Context, pools PoolService, name, owner string, ttl, timeout time.Duration, opts ...ClusterLockOption,
) (*ClusterLockHandle, error) {
	return acquireClusterLockWithClock(ctx, pools, name, owner, ttl, timeout, defaultLockClock(), opts...)
}

func acquireClusterLockWithClock(
	ctx context.Context, pools PoolService, name, owner string, ttl, timeout time.Duration, clk lockClock, opts ...ClusterLockOption,
) (*ClusterLockHandle, error) {
	var settings clusterLockSettings
	for _, opt := range opts {
		opt(&settings)
	}
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
	deadline, clamped := clusterLockDeadline(ctx, clk.now(), timeout)
	timedOut := func(cause error) error {
		message := fmt.Sprintf("AcquireClusterLock: timed out after %s waiting for lock %q", timeout, pool)
		if clamped {
			message = fmt.Sprintf("AcquireClusterLock: timed out waiting for lock %q before the request's deadline", pool)
		}
		return cpierrors.WrapAs(errors.Join(cause, ErrClusterLockTimeout), cpierrors.TypeRetriableCloud, message)
	}
	if !clk.now().Before(deadline) {
		// The request does not leave the margin its caller needs after the
		// wait, so the acquire gives up before it creates anything.
		return nil, timedOut(ErrClusterLockNoTimeToWait)
	}

	for {
		expiry := claimExpiry(clk.now(), ttl)
		comment := encodeLockComment(owner, expiry)
		createErr := pools.CreatePool(ctx, pool, comment)
		if createErr == nil {
			// The create succeeded, but that alone does not make the sentinel
			// ours. A stealer that judged an older claim expired can delete the
			// sentinel we just created, so confirm our claim, twice for a lock
			// that takes the grace, before taking the handle.
			handle, confirmErr := confirmLockCreate(ctx, pools, pool, owner, expiry, deadline, settings, clk)
			if confirmErr != nil || handle != nil {
				return handle, confirmErr
			}
			// We were displaced, and someone else holds the sentinel now, or
			// nobody does. Fall through to the same steal-or-wait decision a
			// refused create takes.
		} else if !isPoolAlreadyExists(createErr) {
			// A non-duplicate failure (auth, transport, pmxcfs error) is mapped to
			// a retriable cloud error so the director re-drives rather than failing
			// the deploy on a transient lock-acquire fault.
			return nil, cpierrors.WrapAs(createErr, cpierrors.TypeRetriableCloud,
				fmt.Sprintf("AcquireClusterLock: create sentinel pool %q", pool))
		}

		// Pool exists: inspect the holder's recorded expiry to decide steal-or-wait.
		if stole, err := tryStealExpired(ctx, pools, pool, owner, ttl, deadline, settings, clk); err != nil {
			return nil, err
		} else if stole != nil {
			return stole, nil
		}

		// Held by a live owner: wait and retry until the timeout.
		now := clk.now()
		if !now.Before(deadline) {
			if createErr == nil {
				createErr = errors.New("sentinel displaced after create")
			}
			return nil, timedOut(createErr)
		}
		if sleepErr := clk.sleep(ctx, clusterLockPollWait(now, deadline)); sleepErr != nil {
			return nil, cpierrors.WrapAs(errors.Join(sleepErr, ErrClusterLockInterrupted), cpierrors.TypeRetriableCloud,
				fmt.Sprintf("AcquireClusterLock: interrupted waiting for lock %q", pool))
		}
	}
}

// clusterLockReleaseTimeout bounds a release or an abandon of a sentinel the
// lock code holds, which runs on a detached context after the work it guarded.
const clusterLockReleaseTimeout = 10 * time.Second

// ClusterLockCompletionAllowance is the time a caller needs after a lock wait
// gives up to close out its request: the reads that settle planned protection
// steps and re-read the disk's ownership, and the journal writes that record
// the outcome. A caller that closes out on a detached context, because its
// request context has already ended, uses it as that context's bound.
const ClusterLockCompletionAllowance = 5 * time.Second

// clusterLockContextMargin is how much of a request's own deadline a lock wait
// leaves unused. It covers the release of the sentinel and the caller's
// completion after the wait, and it leaves the dispatcher a moment to write
// the response once the handler returns. A wait that would run into it gives
// up with ErrClusterLockTimeout instead of being cut short by the deadline.
const clusterLockContextMargin = clusterLockReleaseTimeout + ClusterLockCompletionAllowance

// ClusterLockContextMargin exports clusterLockContextMargin for callers that
// size a request's deadline around a lock wait, such as the storage-journal
// CLI's cleanup budget.
const ClusterLockContextMargin = clusterLockContextMargin

// clusterLockDeadline is when an acquire started at now stops waiting. It is
// now plus timeout, or the request's deadline less clusterLockContextMargin
// when that comes first, and clamped reports that the request's deadline set
// it.
func clusterLockDeadline(ctx context.Context, now time.Time, timeout time.Duration) (time.Time, bool) {
	deadline := now.Add(timeout)
	if requestDeadline, ok := ctx.Deadline(); ok {
		if limit := requestDeadline.Add(-clusterLockContextMargin); limit.Before(deadline) {
			return limit, true
		}
	}
	return deadline, false
}

// clusterLockPollWait is how long an acquire waits before its next attempt,
// which is the poll interval plus jitter, cut short so it never runs past
// deadline.
func clusterLockPollWait(now, deadline time.Time) time.Duration {
	poll := clusterLockPoll()
	wait := poll + time.Duration(jitterInt64N(int64(poll)))
	if remaining := deadline.Sub(now); wait > remaining {
		wait = remaining
	}
	return wait
}

// tryStealExpired reads the existing sentinel pool's comment and, when the
// recorded expiry has passed (or the comment is unreadable/malformed), attempts
// to steal the lock via delete+recreate. It returns a non-nil handle ONLY after
// confirmLockCreate has confirmed the new sentinel carries OUR claim. It returns
// (nil, nil) when the lock is still live, when the claim changed before the
// delete, when the steal ran over its budget, or when another acquirer won the
// recreate, signalling the caller to wait/retry.
//
// PVE has no compare-and-delete, so two stealers that both judged the same
// claim expired race each other, and a stale delete can remove the claim the
// faster stealer just created and confirmed. Three measures narrow that race.
//
//   - The stealer re-reads the claim immediately before its delete and abandons
//     the steal when the claim changed in any way. The delete carries the
//     expected claim, and the managed allocation guard reads the sentinel once
//     more right before it calls PVE and refuses with ErrLockClaimChanged when
//     the claim differs. The guard gives the same answer for a sentinel that
//     has already vanished, so a steal whose sentinel another request released
//     waits one poll before it creates.
//   - The steal budget runs from the moment the re-read is sent, not from when
//     it returns. A stealer that has not finished its delete within
//     clusterLockStealBudget of that moment abandons the steal without creating.
//   - A lock that takes WithCreateGrace makes every create, a steal's or a plain
//     one, wait out a grace pause of at least the budget plus a round trip and
//     confirm its claim a second time.
//
// What remains for such a lock is this. Two holders can overlap only when a
// stealer stalls between its last re-read and its delete for longer than the
// grace pause, so that its delete lands after the rightful holder's second
// confirmation. A lock without the grace keeps the first two measures, and
// only a stale delete can still overlap two holders. The read-after-write
// verify in verifyAntiAffinityMember remains the correctness backstop for the
// anti-affinity lock, because a double-held RMW produces one canonical rule,
// the last writer's, and the verify catches a member that rule lost.
func tryStealExpired(
	ctx context.Context, pools PoolService, pool, owner string, ttl time.Duration, deadline time.Time,
	settings clusterLockSettings, clk lockClock,
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

	// Expired or unparseable holder. Read it again immediately before the
	// delete, and steal only the exact claim that was judged expired. The
	// budget starts before the re-read is sent, because the claim may already
	// have changed while the re-read was in flight.
	started := clk.now()
	current, stillFound, err := pools.GetPoolComment(ctx, pool)
	if err != nil {
		return nil, cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud,
			fmt.Sprintf("AcquireClusterLock: re-read holder of lock %q", pool))
	}
	if !stillFound || current != comment {
		// Someone released, stole, or recreated it since the first read. The
		// next poll decides afresh.
		return nil, nil
	}
	if clk.now().Sub(started) > clusterLockStealBudget {
		// The re-read itself ran over the budget, so its answer is older than
		// another acquirer's grace covers. Leave the sentinel alone.
		return nil, nil
	}
	delErr := pools.DeletePool(WithExpectedLockClaim(ctx, comment), pool)
	switch {
	case delErr == nil, isPoolNotFound(delErr):
	case errors.Is(delErr, ErrLockClaimChanged):
		return nil, nil
	default:
		return nil, cpierrors.WrapAs(delErr, cpierrors.TypeRetriableCloud,
			fmt.Sprintf("AcquireClusterLock: steal-delete lock %q", pool))
	}
	if clk.now().Sub(started) > clusterLockStealBudget {
		// The delete finished outside the budget, so our re-read may be older
		// than another acquirer's grace. Abandon the steal; the sentinel is
		// gone, and the next poll creates it the ordinary way.
		return nil, nil
	}
	expiry := claimExpiry(clk.now(), ttl)
	recreateComment := encodeLockComment(owner, expiry)
	if createErr := pools.CreatePool(ctx, pool, recreateComment); createErr != nil {
		if isPoolAlreadyExists(createErr) {
			// Another acquirer won the recreate; loop back to wait/steal.
			return nil, nil
		}
		return nil, cpierrors.WrapAs(createErr, cpierrors.TypeRetriableCloud,
			fmt.Sprintf("AcquireClusterLock: steal-recreate lock %q", pool))
	}
	return confirmLockCreate(ctx, pools, pool, owner, expiry, deadline, settings, clk)
}

// confirmLockCreate decides whether a create that PVE accepted gave us the
// lock. It reads the sentinel back at once. A lock that takes the grace then
// waits out the grace pause and reads it again, and a stealer whose re-read
// went out before our create has deleted our sentinel by then unless it
// stalled for longer than the grace. Only when every read shows our claim does
// it return a handle, carrying the claim the last read returned. It returns
// (nil, nil) when we were displaced.
//
// A read that fails decides nothing, so the same pass reads again on the poll
// cadence until the acquire's deadline, which keeps the retries inside the
// wait the caller already allowed. Failing at once would leave the sentinel we
// just created standing for a whole TTL while the caller could not tell
// whether it holds the lock. When the reads still fail at the deadline, the
// acquire returns ErrClusterLockStateUnknown without a handle, and on the way
// out abandonLockCreate removes the sentinel only when a fresh read proves it
// is ours.
func confirmLockCreate(
	ctx context.Context, pools PoolService, pool, owner string, expiry, deadline time.Time,
	settings clusterLockSettings, clk lockClock,
) (*ClusterLockHandle, error) {
	passes := 1
	if settings.grace {
		passes = 2
	}
	var claim string
	for pass := 0; pass < passes; pass++ {
		if pass == 1 {
			if err := clk.sleep(ctx, settings.graceDuration()); err != nil {
				return nil, abandonLockCreate(ctx, pools, pool, owner, settings, clk,
					cpierrors.WrapAs(errors.Join(err, ErrClusterLockInterrupted), cpierrors.TypeRetriableCloud,
						fmt.Sprintf("AcquireClusterLock: interrupted confirming lock %q", pool)))
			}
		}
		comment, mine, err := sentinelClaim(ctx, pools, pool, owner)
		for err != nil {
			now := clk.now()
			if !now.Before(deadline) {
				return nil, abandonLockCreate(ctx, pools, pool, owner, settings, clk, lockStateUnknown(pool, err))
			}
			if sleepErr := clk.sleep(ctx, clusterLockPollWait(now, deadline)); sleepErr != nil {
				return nil, abandonLockCreate(ctx, pools, pool, owner, settings, clk,
					lockStateUnknown(pool, errors.Join(err, sleepErr)))
			}
			comment, mine, err = sentinelClaim(ctx, pools, pool, owner)
		}
		if !mine {
			return nil, nil
		}
		claim = comment
	}
	return &ClusterLockHandle{
		pool: pool, owner: owner, claim: claim, settings: settings, pools: pools, expiry: expiry, now: clk.now,
	}, nil
}

// lockStateUnknown is the error an acquire returns when it created its
// sentinel but no read of it answered before the deadline.
func lockStateUnknown(pool string, cause error) error {
	return cpierrors.WrapAs(errors.Join(cause, ErrClusterLockStateUnknown), cpierrors.TypeRetriableCloud,
		fmt.Sprintf("AcquireClusterLock: could not confirm who holds lock %q", pool))
}

// abandonLockCreate gives up on a create whose ownership could not be
// confirmed, and it returns cause. Our sentinel would otherwise block every
// other acquirer until its TTL, so it reads the sentinel on a detached context
// and deletes it when that read proves the claim is ours. The delete carries
// the comment the read returned, never the one we sent, so a guarded pool
// service compares two reads. It leaves the sentinel alone when the read
// fails, names another owner, cannot be parsed, or shows a claim within the
// release margin of its expiry, the same rules Release follows.
func abandonLockCreate(
	ctx context.Context, pools PoolService, pool, owner string, settings clusterLockSettings, clk lockClock, cause error,
) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), clusterLockReleaseTimeout)
	defer cancel()
	comment, found, err := pools.GetPoolComment(cleanupCtx, pool)
	if err != nil || !found {
		return cause
	}
	holder, ownerOK := decodeLockOwner(comment)
	exp, expOK := decodeLockExpiry(comment)
	if !ownerOK || !expOK || holder != owner || !clk.now().Add(settings.releaseMargin()).Before(exp) {
		return cause
	}
	// A failed delete leaves the sentinel to its TTL steal, which is where it
	// stood before this attempt, so cause is still the error to return.
	_ = pools.DeletePool(WithExpectedLockClaim(cleanupCtx, comment), pool)
	return cause
}

// sentinelClaim reads the sentinel back and returns its comment and whether it
// carries owner's claim. A read that fails is returned, because it cannot tell
// us who holds the lock.
func sentinelClaim(ctx context.Context, pools PoolService, pool, owner string) (string, bool, error) {
	comment, found, err := pools.GetPoolComment(ctx, pool)
	if err != nil {
		return "", false, err
	}
	return comment, found && strings.Contains(comment, lockCommentOwnerKey+owner+" "), nil
}

// claimExpiry is the expiry a claim made at now records. The comment carries
// whole seconds, and every other acquirer judges the claim by that value, so
// the handle keeps the same truncated time rather than the finer one it
// started from.
func claimExpiry(now time.Time, ttl time.Duration) time.Time {
	return time.Unix(now.Add(ttl).Unix(), 0)
}

// WindowDeadline returns when work under this lock must stop so that reserve
// still fits before the claim's recorded expiry. The expiry was fixed before
// the create, so the guard admission, the confirming reads, and the grace
// pause that ran before the acquire returned are already counted against it.
func (h *ClusterLockHandle) WindowDeadline(reserve time.Duration) time.Time {
	return h.expiry.Add(-reserve)
}

// Release deletes the sentinel pool, freeing the lock. It is idempotent: a
// second call is a no-op, and a not-found pool is treated as success (the lock
// may already have been stolen after expiry). All other delete failures are
// returned for the caller to log, and they leave the handle unreleased so a
// retry can still delete. A nil handle Release is a no-op.
//
// Release checks the owner before it deletes, and that check and the delete
// are two separate calls. The only thing that can replace our sentinel between
// them is a steal, and a steal needs our claim to have expired. So Release
// refuses to delete a claim that is expired or will expire within the release
// margin, which is the steal budget, plus the grace for a lock that takes it,
// plus one round trip. Such a sentinel is left for its TTL steal, which costs
// nothing once the claim has lapsed. A second re-read right before the delete
// would look safer but would not close the gap, because that read and the
// delete are still two calls that a steal can fall between. The margin closes
// it, because a claim that is outside the margin at the check cannot become
// stealable before our delete lands.
func (h *ClusterLockHandle) Release(ctx context.Context) error {
	if h == nil || h.released {
		return nil
	}
	margin := h.settings.releaseMargin()
	comment, found, readErr := h.pools.GetPoolComment(ctx, h.pool)
	switch {
	case readErr == nil && !found:
		// The sentinel is already gone, so there is nothing to delete.
		h.released = true
		return nil
	case readErr == nil:
		owner, ownerOK := decodeLockOwner(comment)
		exp, expOK := decodeLockExpiry(comment)
		if !ownerOK || !expOK {
			// A comment we cannot parse is never deleted. Acquirers treat it
			// as expired and steal it, so leaving it costs nothing.
			h.released = true
			return nil
		}
		if owner != h.owner {
			// Somebody else holds it now. Ours is already gone.
			h.released = true
			return nil
		}
		if h.now != nil && !h.now().Add(margin).Before(exp) {
			// Our claim is expired or within the margin. A steal may land
			// before our delete would, so leave it for the TTL steal.
			h.released = true
			return nil
		}
	default:
		// The read did not answer, so we judge by our own recorded expiry. A
		// claim within the margin is left alone, as above. Otherwise the
		// delete below carries our claim, and a guarded pool service refuses
		// it when the sentinel holds anything else.
		if h.now != nil && !h.expiry.IsZero() && !h.now().Add(margin).Before(h.expiry) {
			h.released = true
			return nil
		}
	}
	deleteCtx := ctx
	if h.claim != "" {
		deleteCtx = WithExpectedLockClaim(ctx, h.claim)
	}
	err := h.pools.DeletePool(deleteCtx, h.pool)
	if errors.Is(err, ErrLockClaimChanged) {
		// The sentinel holds someone else's claim, or none. Ours is gone.
		h.released = true
		return nil
	}
	if err != nil && !isPoolNotFound(err) {
		// released stays false: a failed delete (cancelled ctx, transient
		// API fault, a failed pre-delete read in the guard) must not latch
		// the handle closed, or a retried Release silently no-ops and the
		// sentinel pool is orphaned until a later acquirer steals it past the
		// TTL.
		return cpierrors.Wrap(err, fmt.Sprintf("ReleaseClusterLock: delete sentinel pool %q", h.pool))
	}
	h.released = true
	return nil
}

// lockOwnerSeq numbers the owner tokens ProcessLockOwner issues in this process.
var lockOwnerSeq atomic.Uint64

// lockOwnerHostLimit caps the host part of an owner token. PVE sets no length
// limit on a pool comment, and the cap only keeps claims short.
const lockOwnerHostLimit = 63

var (
	lockOwnerIdentityOnce sync.Once
	lockOwnerIdentity     string
)

// processLockIdentity names this process for owner tokens: the host it runs
// on, its pid, and a random nonce drawn once per process. The host tells
// Directors on different machines apart, and the nonce still tells them apart
// when two machines share a hostname and a pid.
func processLockIdentity() string {
	lockOwnerIdentityOnce.Do(func() {
		host, err := os.Hostname()
		if err != nil {
			host = ""
		}
		nonce := make([]byte, 4)
		if _, err := rand.Read(nonce); err != nil {
			binary.BigEndian.PutUint32(nonce, uint32(time.Now().UnixNano()))
		}
		lockOwnerIdentity = fmt.Sprintf("%s/%d-%s", lockOwnerHost(host), os.Getpid(), hex.EncodeToString(nonce))
	})
	return lockOwnerIdentity
}

// lockOwnerHost renders a hostname for an owner token. Anything outside
// letters, digits, '-', '.', and '_' becomes '-', so the token never carries
// the space a sentinel comment is split on, and the result is capped at
// lockOwnerHostLimit bytes.
func lockOwnerHost(host string) string {
	var b strings.Builder
	for _, r := range host {
		if b.Len() >= lockOwnerHostLimit {
			break
		}
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '.', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	if b.Len() == 0 {
		return "unknown-host"
	}
	return b.String()
}

// ProcessLockOwner qualifies a caller's owner token with this process's
// identity and a per-process sequence, rendering "<owner>@<host>/<pid>-<nonce>-<seq>".
// Two CPI processes that lock the same key in the same second would otherwise
// stamp byte-identical claims, and a caller that compares claims could not
// tell the holder's sentinel from its own. The sentinel comment is split on
// whitespace, so lockOwnerToken replaces any in the caller's part, and the
// owner this returns always decodes back to itself.
func ProcessLockOwner(owner string) string {
	return fmt.Sprintf("%s@%s-%d", lockOwnerToken(owner), processLockIdentity(), lockOwnerSeq.Add(1))
}

// lockOwnerToken makes a caller's owner token safe to stamp into a sentinel
// comment. decodeLockOwner splits the comment with strings.Fields, so every
// rune that unicode.IsSpace reports becomes '-', and an empty token becomes
// "unnamed". Every other byte is copied as it is, so a token that was already
// safe comes back byte for byte the same. That includes bytes that are not
// valid UTF-8, which we keep on purpose. The owner only has to decode back to
// itself through our own decoder so that a release can prove the claim ours,
// and rewriting bytes that are already safe would give us a token that is no
// longer the caller's. lockOwnerHost replaces such bytes because the host ends
// up in text we print, which is a different job.
func lockOwnerToken(owner string) string {
	if owner == "" {
		return "unnamed"
	}
	var b strings.Builder
	for i := 0; i < len(owner); {
		r, size := utf8.DecodeRuneInString(owner[i:])
		if unicode.IsSpace(r) {
			b.WriteByte('-')
		} else {
			b.WriteString(owner[i : i+size])
		}
		i += size
	}
	return b.String()
}

// ErrClusterLockStateUnknown marks an acquire that created its sentinel but
// could not read it back before its deadline, so it cannot tell whether it
// holds the lock. Like a timeout, it is not a sign that the lock mechanism is
// unavailable, and a caller must not run the guarded work unserialized beside
// a sentinel that may be ours.
var ErrClusterLockStateUnknown = errors.New("cluster lock state unknown after create")

// ErrClusterLockTimeout marks the acquire that ran out its timeout waiting for a
// holder that was live on every attempt. It is distinct from every other acquire
// failure: an expired or unreadable holder is stolen rather than waited on, so a
// timeout is positive evidence that somebody else is inside the window right
// now. Callers that would otherwise proceed unserialized use it to tell "nobody
// can lock here" from "somebody is locked here". An acquire whose request
// deadline came first, less clusterLockContextMargin, returns it too, having
// changed nothing, and so does one that starts with less than that margin left.
var ErrClusterLockTimeout = errors.New("cluster lock acquire timed out")

// ErrClusterLockNoTimeToWait marks the lock timeout of an acquire that gave up
// before it waited at all, because its request's deadline left less than
// clusterLockContextMargin. It comes joined with ErrClusterLockTimeout, so a
// caller that reports on the wait can say none happened.
var ErrClusterLockNoTimeToWait = errors.New("the request's deadline leaves no time to wait")

// ErrClusterLockInterrupted marks an acquire whose wait was cut short because
// its request context was cancelled, as a SIGTERM does. It is kept apart from a
// timeout, so callers can tell the two, and like a timeout it is not a sign
// that the lock mechanism is unavailable.
var ErrClusterLockInterrupted = errors.New("cluster lock wait interrupted")

// decodeLockOwner extracts the owner token from a sentinel pool comment.
func decodeLockOwner(comment string) (string, bool) {
	for _, field := range strings.Fields(comment) {
		if strings.HasPrefix(field, lockCommentOwnerKey) {
			return strings.TrimPrefix(field, lockCommentOwnerKey), true
		}
	}
	return "", false
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
// LIVE-VALIDATION CONFIRMED (PVE 9.2.4): duplicate poolid always
// returns HTTP 500 + text "... already exists", never 409 — the 409 sentinel
// branch above never fires against real PVE and is kept as belt-and-suspenders
// for any future/other version that does use 409; the substring branch below
// is the reliable primary path.
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
