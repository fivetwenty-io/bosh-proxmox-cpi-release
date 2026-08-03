package handlers

import (
	"context"
	"fmt"
	"time"

	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
)

// vmidLockTTL is the maximum lifetime of a held per-VMID cluster lock.
// Set to 30s — matches the StorageLock convention and is well above the
// typical tag read-modify-write latency (~1s). A holder that crashes after
// acquisition but before release is reclaimed after TTL expires.
const vmidLockTTL = 30 * time.Second

// vmidLockTimeout is the maximum time withVMIDLock waits to acquire the lock.
// Set to 10s. On timeout AcquireClusterLock returns a retriable error so the
// BOSH director re-drives the operation rather than failing the deployment.
const vmidLockTimeout = 10 * time.Second

// withVMIDLock acquires a per-VMID cross-process advisory lock backed by PVE
// resource pools (the same pmxcfs sentinel mechanism used for anti-affinity)
// and then calls fn under that lock. The lock is released via a deferred call
// regardless of whether fn succeeds or fails.
//
// Lock key scheme: "vm-<vmid>" → ClusterLockPoolName("vm-<vmid>") →
// "bosh-lock-vm-<vmid>". This serializes all tag/notes read-modify-write
// operations for a given VMID across concurrent CPI process invocations.
//
// SCOPE: pools (and therefore this lock) live inside a single pmxcfs
// instance, so "vm-<vmid>" in cluster A and "vm-<vmid>" in cluster B are two
// unrelated locks — see the per-cluster scope note on the pve package's
// cluster_lock.go doc comment. A VMID that collides across two independent
// clusters sharing storage (same VMID band, no per-CPI banding) is NOT
// serialized by this lock: set_vm_metadata, set_disk_metadata, stemcell_refs,
// and delete_vm on that VMID can interleave freely between the clusters.
//
// Failure modes:
//   - pools == nil: returns a retriable error immediately; fn is not called.
//   - AcquireClusterLock failure: returns the retriable error from the lock
//     infrastructure; fn is not called.
//   - fn returns an error: the error is returned to the caller; the lock is
//     still released via defer.
//   - fn succeeds: nil is returned; lock is released.
//
// The lock release error (if any) is logged at Warn level and not returned
// so a deferred release failure does not mask the fn result.
func withVMIDLock(
	ctx context.Context,
	pools pve.PoolService,
	vmid int,
	owner string,
	logger *log.Logger,
	fn func() error,
) error {
	if pools == nil {
		return cpierrors.WrapAs(
			cpierrors.Cloud("withVMIDLock: pool service is nil for vmid=%d", vmid),
			cpierrors.TypeRetriableCloud,
			fmt.Sprintf("withVMIDLock: acquire lock for vm-%d", vmid),
		)
	}

	lockName := fmt.Sprintf("vm-%d", vmid)
	handle, err := pve.AcquireClusterLock(ctx, pools, lockName, owner, vmidLockTTL, vmidLockTimeout)
	if err != nil {
		return err
	}
	defer func() {
		// Release is cleanup: run it on a detached, bounded context so an
		// expired or cancelled request ctx cannot make DeletePool fail
		// instantly and orphan the sentinel pool until a later acquirer
		// steals it past the TTL.
		relCtx, relCancel := detachedContext(ctx, lockReleaseTimeout)
		defer relCancel()
		if relErr := handle.Release(relCtx); relErr != nil {
			if logger != nil {
				logger.Warn("withVMIDLock: release failed (non-fatal)",
					log.Int("vmid", vmid),
					log.Err(relErr),
				)
			}
		}
	}()

	return fn()
}
