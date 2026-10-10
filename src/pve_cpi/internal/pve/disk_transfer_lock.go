package pve

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// diskTransferLockName is the logical lock key for the per-disk transfer lock
// of the disk with stableID. It lives in PVE as the sentinel pool
// ClusterLockPoolName("disk-<stableID>").
func diskTransferLockName(stableID string) string {
	return "disk-" + stableID
}

// DiskTransferLockPoolName is the sentinel pool the per-disk transfer lock of
// the disk with stableID lives in, for a caller outside this package that
// reads it, such as the journal's settlement of a lock step.
func DiskTransferLockPoolName(stableID string) string {
	return ClusterLockPoolName(diskTransferLockName(stableID))
}

// diskTransferLockTimeouts returns the TTL and the acquire wait of a per-disk
// transfer lock under ctx. The TTL is the parker protection-window lock's TTL
// plus that lock's default wait (parkerProtectionLockTimeout), because the
// disk lock is taken first and must still be held when a parker window that
// waited for the parker lock runs to the end of its own claim. A test that
// shortens the parker lock through withTestParkerLockTimeouts shortens this
// one too.
//
// The wait added to the TTL is capped at the default parker wait, not the
// longer wait a managed caller asks for (WithParkerLockWait), because the TTL
// is also how long a crashed holder blocks every other transfer of the disk.
// On the shipped curves that bound is about 250s: the parker TTL of about
// 235s plus the 15s default wait. A parker window whose acquire waited longer
// than the default runs on what is left of the disk claim, which
// diskTransferWindowContext checks before the window opens.
//
// The wait is the whole TTL plus the create grace, so a second transfer of the
// disk waits out a holder that keeps the lock for as long as its claim allows,
// and a crashed holder's claim expires and is stolen inside the wait. A waiter
// is therefore bounded by the TTL plus the grace, and the acquire itself also
// stops waiting before the request's deadline (clusterLockDeadline), which is
// the bound the journal lock's wait has too (managedLockWaitContext in the
// handlers).
func diskTransferLockTimeouts(ctx context.Context) (time.Duration, time.Duration) {
	parkerTTL, parkerWait := parkerLockTimeoutsFrom(ctx)
	ttl := parkerTTL + min(parkerWait, parkerProtectionLockTimeout)
	return ttl, ttl + clusterLockGrace()
}

// diskTransferLockOptions are the options a per-disk transfer lock acquire with
// this TTL passes. Like the parker lock it takes the create grace, because two
// holders would race two transfers of one disk. A TTL that can hold the
// release margin and a minimal parker window body also asks for that much as
// its claim reserve, so a claim confirmed too late to run one window is
// refused rather than handed back.
func diskTransferLockOptions(ttl time.Duration) []ClusterLockOption {
	opts := []ClusterLockOption{WithCreateGrace()}
	if need := parkerLockReleaseTimeout + parkerMinimumWindowBody; ttl > need {
		opts = append(opts, WithClaimReserve(need))
	}
	return opts
}

// diskTransferLockDeadline is when work under a per-disk transfer lock must
// stop, which is the claim's recorded expiry less the release margin, and
// never less than a second from now. A parker window inside it runs on the
// earlier of this deadline and its own, so the parker lock's deadline still
// governs every window that fits inside the disk claim.
func diskTransferLockDeadline(handle *ClusterLockHandle, now time.Time) time.Time {
	deadline := handle.WindowDeadline(parkerLockReleaseTimeout)
	if floor := now.Add(time.Second); deadline.Before(floor) {
		return floor
	}
	return deadline
}

// diskTransferLockUnserializedKey marks the context of a transfer that runs
// without its per-disk lock, because the client has no pool service or PVE
// refused the lock's create outright.
type diskTransferLockUnserializedKey struct{}

// diskTransferLockUnserialized reports whether ctx belongs to a transfer that
// runs without its per-disk lock. The parker windows and the re-resolve put it
// in their logs, so a transfer that raced another one of the same disk can be
// told apart from one the lock serialized.
func diskTransferLockUnserialized(ctx context.Context) bool {
	unserialized, _ := ctx.Value(diskTransferLockUnserializedKey{}).(bool)
	return unserialized
}

// diskTransferLockHeldKey marks the context of work that already runs under
// the per-disk transfer lock of the disk whose stable ID it carries, whether
// the lock was taken or the work fell back to running without it.
type diskTransferLockHeldKey struct{}

// diskTransferLockHeld reports whether ctx already runs under the per-disk
// transfer lock of the disk with stableID.
func diskTransferLockHeld(ctx context.Context, stableID string) bool {
	held, _ := ctx.Value(diskTransferLockHeldKey{}).(string)
	return held != "" && held == stableID
}

// diskTransferBudgetKey carries the time a transfer holds its per-disk lock
// for, so each parker window it opens can check that it still fits.
type diskTransferBudgetKey struct{}

// diskTransferBudget is the work deadline of a held per-disk lock and the time
// a parker window inside it must leave after its body. The reserve is zero
// for a test-sized TTL, which can't hold a window's reserve at all, and the
// windows then run on the disk deadline alone.
type diskTransferBudget struct {
	deadline time.Time
	reserve  time.Duration
}

// diskTransferWindowNeed is the least time a parker window must have before
// the point where its body has to stop under the disk lock. The window's lock
// acquire stops waiting clusterLockContextMargin before that point, and a
// window that wins its lock then still needs parkerMinimumWindowBody to work.
func diskTransferWindowNeed() time.Duration {
	return clusterLockContextMargin + parkerMinimumWindowBody
}

// diskTransferBudgetFor is the budget a per-disk lock with this TTL and work
// deadline hands its transfer. Only a TTL that can hold a parker window's
// reserve, the window's need, and the release margin gets the reserve.
func diskTransferBudgetFor(ttl time.Duration, deadline time.Time) diskTransferBudget {
	budget := diskTransferBudget{deadline: deadline}
	if reserve := parkerWindowReserveNow(); ttl > reserve+diskTransferWindowNeed()+parkerLockReleaseTimeout {
		budget.reserve = reserve
	}
	return budget
}

// diskTransferWindowContext returns the context a parker window of a transfer
// under the per-disk lock opens on, and refuses the window when too little of
// the lock is left for it to finish.
//
// A transfer can open several windows under one disk claim, one per parker it
// tries, and the claim doesn't grow with them. So the window's context ends a
// window's reserve before the disk deadline. The sweep, the protection
// restore, and the release that run after the window's body then end inside
// the disk claim. When less than diskTransferWindowNeed is left before that
// point, the window isn't opened and the transfer fails retriably, because a
// window started there would clear a parker's protection and then run out of
// time. The Director's retry takes a fresh disk claim.
//
// A context without a budget, which is a transfer that runs without the lock,
// and a test-sized budget come back unchanged.
func diskTransferWindowContext(ctx context.Context, stableID string, now time.Time) (context.Context, context.CancelFunc, error) {
	budget, ok := ctx.Value(diskTransferBudgetKey{}).(diskTransferBudget)
	if !ok || budget.reserve <= 0 {
		return ctx, func() {}, nil
	}
	limit := budget.deadline.Add(-budget.reserve)
	if left := limit.Sub(now); left < diskTransferWindowNeed() {
		return nil, nil, cpierrors.Retriable(
			"TransferDiskToParker: only %s of the per-disk lock of disk %s is left for another parker window, "+
				"which needs %s; the Director's retry takes the lock again",
			max(left, 0).Round(time.Second), stableID, diskTransferWindowNeed())
	}
	windowCtx, cancel := context.WithDeadline(ctx, limit)
	return windowCtx, cancel, nil
}

// withDiskTransferLock runs fn while it holds the per-disk transfer lock of the
// disk with stableID, so only one detach-side transfer can move that disk at a
// time anywhere in the cluster.
//
// The parker protection-window lock serializes two transfers into one parker,
// but not two transfers of one disk into different parkers, and the identity
// scan that names the disk's volume runs before either lock. Without this lock
// a second detach_disk of the same disk, such as a dynamic-disk broker's and a
// deploy's that the Director doesn't serialize, can act on a volume name the
// first one has already renamed.
//
// The lock is always taken before the parker lock and never inside a parker
// window, so the order between the two is fixed. A journal-managed caller
// holds its journal lock around this one, so the order is journal, then disk,
// then parker. fn runs on a deadline derived from the claim's expiry
// (diskTransferLockDeadline), and its context carries the budget each parker
// window checks before it opens (diskTransferWindowContext).
//
// A call whose context already runs under this disk's lock
// (diskTransferLockHeld) runs fn on that context at once, with the outer
// holder's deadline and budget. The lock is a sentinel pool, not a reentrant
// mutex, so a nested acquire would wait on its own claim until the claim
// expired.
//
// The fallbacks match withParkerProtectionLock. A client with no pool service,
// or a lock create that PVE refused outright, runs fn unserialized, because
// the transfer's own checks inside the parker window still stop it from acting
// on a disk that has already left its source. That is logged as an error,
// because two transfers of the disk can then overlap, and fn's context is
// marked (diskTransferLockUnserialized). Every other acquire failure is
// returned, retriably, because a live holder may be moving the disk right now.
func withDiskTransferLock(ctx context.Context, c Client, logger *log.Logger, stableID, purpose string, fn func(context.Context) error) error {
	if diskTransferLockHeld(ctx, stableID) {
		return fn(ctx)
	}
	var pools PoolService
	if c != nil {
		pools = c.Pools()
	}
	ctx = context.WithValue(ctx, diskTransferLockHeldKey{}, stableID)
	unserialized := context.WithValue(ctx, diskTransferLockUnserializedKey{}, true)
	if pools == nil {
		if logger != nil {
			logger.Error("disk transfer: no pool service; running the transfer without the per-disk lock, "+
				"so another transfer of this disk can overlap it",
				log.String("stable_id", stableID),
				log.String("purpose", purpose),
			)
		}
		return fn(unserialized)
	}
	owner := ProcessLockOwner(fmt.Sprintf("%s/%s", purpose, stableID))
	ttl, timeout := diskTransferLockTimeouts(ctx)
	handle, lockErr := acquireClusterLockWithClock(ctx, pools,
		diskTransferLockName(stableID), owner, ttl, timeout, parkerLockClockFrom(ctx), diskTransferLockOptions(ttl)...)
	if lockErr != nil {
		if !errors.Is(lockErr, ErrClusterLockCreateRefused) {
			// A timeout means another transfer of this disk held the lock
			// throughout the wait, and an acquire that can't tell who holds
			// it may be overlapping one. Either way the Director's retry is
			// the safe next step, and it re-resolves the disk first.
			return lockErr
		}
		if logger != nil {
			logger.Error("disk transfer: PVE refused the per-disk lock; running the transfer without it, "+
				"so another transfer of this disk can overlap it",
				log.String("stable_id", stableID),
				log.String("purpose", purpose),
				log.Err(lockErr),
			)
		}
		return fn(unserialized)
	}
	defer func() {
		// Released on a detached context, so a request that has already ended
		// doesn't strand the sentinel until a later acquirer steals it.
		relCtx, relCancel := context.WithTimeout(context.WithoutCancel(ctx), parkerLockReleaseTimeout)
		defer relCancel()
		if relErr := handle.Release(relCtx); relErr != nil && logger != nil {
			logger.Warn("disk transfer: could not release the per-disk lock (non-fatal)",
				log.String("stable_id", stableID),
				log.Err(relErr),
			)
		}
	}()
	deadline := diskTransferLockDeadline(handle, time.Now())
	lockCtx, cancel := context.WithDeadline(context.WithValue(ctx, diskTransferBudgetKey{}, diskTransferBudgetFor(ttl, deadline)), deadline)
	defer cancel()
	return fn(lockCtx)
}

// diskTransferLockSweepReadTimeout bounds each re-read the expired-lock sweep
// makes of one sentinel before it deletes it.
const diskTransferLockSweepReadTimeout = 5 * time.Second

// SweepExpiredDiskTransferLocks deletes every per-disk transfer lock sentinel
// whose claim has expired, and returns how many it deleted.
//
// A transfer that crashes while it holds a per-disk lock leaves the sentinel
// pool behind. The next transfer of that disk steals it, but a disk that never
// transfers again would keep its sentinel forever, and one sentinel per disk
// adds up. So the parker pool sweep runs this one too.
//
// It acts only on a claim whose comment carries an expiry that passed more
// than clusterLockStealBudget plus the create grace ago (sweepExpiredLockAge).
// A live claim, and a comment it can't parse, are never touched, because an
// unparseable comment may be a holder this release doesn't understand. A claim
// that expired only just now is left to a stealing acquirer, which is waiting
// for exactly that expiry and replaces the claim through its own
// read-and-steal (tryStealExpired), so the sweep doesn't race it for the
// sentinel.
//
// Each removal follows the discipline a stealing acquirer follows. The
// sentinel is read again right before the delete, and the delete happens only
// when that read shows the exact claim the listing showed and came back within
// clusterLockStealBudget. The delete itself is unconditional. The sweep runs
// on the client the parker pool sweep hands it, which isn't the journal's
// guarded pool service, and the SDK's DeletePool ignores the expected claim
// the context carries (WithExpectedLockClaim), so nothing on the PVE side
// refuses a delete whose sentinel changed after the re-read. The protection
// is on the acquirer's side instead. An acquirer that creates a fresh claim in
// that gap waits out its create grace and confirms its claim again, so a
// delete that slipped in between displaces it rather than overlapping it.
//
// A pool service without PoolCommentLister has nothing to list, and the sweep
// does nothing. Every failure is collected and returned, after the sweep has
// tried every sentinel it listed.
func SweepExpiredDiskTransferLocks(ctx context.Context, c Client, logger *log.Logger) (int, error) {
	if c == nil {
		return 0, nil
	}
	pools := c.Pools()
	if pools == nil {
		return 0, nil
	}
	lister, ok := pools.(PoolCommentLister)
	if !ok {
		return 0, nil
	}
	listed, err := lister.ListPoolComments(ctx)
	if err != nil {
		return 0, cpierrors.Wrap(err, "sweep expired per-disk transfer locks: list pools")
	}
	prefix := ClusterLockPoolName(diskTransferLockName(""))
	names := make([]string, 0, len(listed))
	for name := range listed {
		if strings.HasPrefix(name, prefix) && len(name) > len(prefix) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	clk := parkerLockClockFrom(ctx)
	removed := 0
	var errs []error
	for _, name := range names {
		comment := listed[name]
		expiry, parsed := decodeLockExpiry(comment)
		if !parsed || !clk.now().After(expiry.Add(sweepExpiredLockAge())) {
			continue
		}
		deleted, sweepErr := sweepExpiredLockSentinel(ctx, pools, name, comment, clk)
		if sweepErr != nil {
			errs = append(errs, sweepErr)
			continue
		}
		if !deleted {
			continue
		}
		removed++
		if logger != nil {
			logger.Info("disk transfer: deleted an expired per-disk lock that a transfer left behind",
				log.String("pool", name),
				log.String("expired_at", expiry.UTC().Format(time.RFC3339)),
			)
		}
	}
	return removed, errors.Join(errs...)
}

// sweepExpiredLockAge is how long ago a claim must have expired before the
// sweep deletes its sentinel. A stealer that saw the claim expire takes up to
// clusterLockStealBudget to re-read and replace it, and then the create grace
// to confirm its own claim, so a claim younger than both is still a stealer's
// to take.
func sweepExpiredLockAge() time.Duration {
	return clusterLockStealBudget + clusterLockGrace()
}

// sweepExpiredLockSentinel deletes the sentinel pool when it still carries
// exactly comment, which the caller judged expired. It reports whether it
// deleted it. A sentinel that is gone, carries another claim, or whose re-read
// took longer than clusterLockStealBudget is left alone.
func sweepExpiredLockSentinel(ctx context.Context, pools PoolService, name, comment string, clk lockClock) (bool, error) {
	started := clk.now()
	readCtx, cancel := context.WithTimeout(ctx, diskTransferLockSweepReadTimeout)
	current, found, err := pools.GetPoolComment(readCtx, name)
	cancel()
	if err != nil {
		return false, cpierrors.Wrap(err, fmt.Sprintf("sweep expired per-disk transfer locks: re-read %s", name))
	}
	if !found || current != comment || clk.now().Sub(started) > clusterLockStealBudget {
		return false, nil
	}
	// The expected claim is passed for a guarded pool service that checks it.
	// The SDK service the sweep normally runs on deletes unconditionally.
	delErr := pools.DeletePool(WithExpectedLockClaim(ctx, comment), name)
	switch {
	case delErr == nil:
		return true, nil
	case isPoolNotFound(delErr), errors.Is(delErr, ErrLockClaimChanged):
		return false, nil
	default:
		return false, cpierrors.Wrap(delErr, fmt.Sprintf("sweep expired per-disk transfer locks: delete %s", name))
	}
}
