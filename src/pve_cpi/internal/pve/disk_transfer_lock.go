package pve

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// diskTransferLockName is the logical lock key for the per-disk transfer lock
// of the disk with stableID. It lives in PVE as the sentinel pool
// ClusterLockPoolName("disk-<stableID>").
func diskTransferLockName(stableID string) string {
	return "disk-" + stableID
}

// diskTransferLockTimeouts returns the TTL and the acquire wait of a per-disk
// transfer lock under ctx. The wait is the parker protection-window lock's, so
// a caller that sets a longer parker wait through WithParkerLockWait waits as
// long here, and a test that shortens the parker lock shortens this one too.
// The TTL is the parker lock's TTL plus that wait, because the disk lock is
// taken first and must still be held when a parker window that waited its
// whole wait for the parker lock runs to the end of its own claim.
func diskTransferLockTimeouts(ctx context.Context) (time.Duration, time.Duration) {
	ttl, timeout := parkerLockTimeoutsFrom(ctx)
	return ttl + timeout, timeout
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
// window, so the order between the two is fixed. fn runs on a deadline derived
// from the claim's expiry (diskTransferLockDeadline).
//
// The fallbacks match withParkerProtectionLock. A client with no pool service,
// or a lock create that PVE refused outright, runs fn unserialized after a
// warning, because the transfer's own checks inside the parker window still
// stop it from acting on a disk that has already left its source. Every other
// acquire failure is returned, retriably, because a live holder may be moving
// the disk right now.
func withDiskTransferLock(ctx context.Context, c Client, logger *log.Logger, stableID, purpose string, fn func(context.Context) error) error {
	var pools PoolService
	if c != nil {
		pools = c.Pools()
	}
	if pools == nil {
		if logger != nil {
			logger.Warn("disk transfer: no pool service; running the transfer without the per-disk lock",
				log.String("stable_id", stableID),
				log.String("purpose", purpose),
			)
		}
		return fn(ctx)
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
			logger.Warn("disk transfer: could not acquire the per-disk lock; running the transfer without it",
				log.String("stable_id", stableID),
				log.String("purpose", purpose),
				log.Err(lockErr),
			)
		}
		return fn(ctx)
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
	lockCtx, cancel := context.WithDeadline(ctx, diskTransferLockDeadline(handle, time.Now()))
	defer cancel()
	return fn(lockCtx)
}
