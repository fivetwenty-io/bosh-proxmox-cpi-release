package handlers

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// vmidLockTTL is how long a per-VMID lock claim stands before another process
// may steal it. It has to outlast the slowest body that runs under the lock,
// and set_vm_metadata's is the slowest by far, because its pool reconcile rides
// three retry loops. vmidLockBodyBudget works that worst case out from the
// retry loops' own attempt counts and backoff curves, and a test fails if this
// constant stops covering it at the shipped curves. An operator who lengthens
// a curve gets a longer TTL at acquire time instead (see vmidLockTTLNow).
//
// We deliberately do not cut the body off when the TTL runs out. A deadline
// that fires in the middle of a write leaves that write's outcome unknown: PVE
// may have applied it or not, and the retry loop around it cannot tell a write
// that landed from one that was lost. Cutting the body short would trade one
// hazard for another. The cost of a long TTL instead is that a holder that
// dies blocks metadata writes on its VM until the claim's recorded expiry.
const vmidLockTTL = 25 * time.Minute

// vmidLockTimeout is the maximum time withVMIDLock waits to acquire the lock.
// Set to 10s. On timeout AcquireClusterLock returns a retriable error so the
// BOSH director re-drives the operation rather than failing the deployment.
// It is a variable only so tests can shorten the wait.
var vmidLockTimeout = 10 * time.Second

// vmidLockHolderReadTimeout bounds the read that names the holder after the
// wait for a VMID lock runs out. It runs on a detached context, because the
// request's own context may be what ran out.
const vmidLockHolderReadTimeout = 10 * time.Second

// vmidLockClaimLimit caps how much of a sentinel comment the timeout error
// quotes. A claim the CPI wrote is far shorter; the cap only matters for a
// comment somebody else put on the pool.
const vmidLockClaimLimit = 256

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
	// Callers name the operation and the VMID. The pid and sequence make the
	// claim unique to this acquisition, which a guarded create relies on.
	handle, err := pve.AcquireClusterLock(ctx, pools, lockName, pve.ProcessLockOwner(owner), vmidLockTTLNow(), vmidLockTimeout)
	if err != nil {
		if errors.Is(err, pve.ErrClusterLockTimeout) {
			return vmidLockHeldError(ctx, pools, lockName, err)
		}
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

// vmidLockHeldError turns a VMID lock wait that ran out into an error that
// names the holder, so an operator can see which host and process hold the
// lock and when the claim lapses without reading the pool by hand. It reads the
// sentinel once more on a detached, bounded context and quotes the claim's
// owner token and expiry. When the sentinel is gone by then, the error says the
// holder released it; when the read fails, the error says the claim could not
// be read. The timeout stays the cause in every case, so the error keeps its
// retriable type and still matches pve.ErrClusterLockTimeout.
func vmidLockHeldError(ctx context.Context, pools pve.PoolService, lockName string, timeoutErr error) error {
	pool := pve.ClusterLockPoolName(lockName)
	readCtx, cancel := detachedContext(ctx, vmidLockHolderReadTimeout)
	defer cancel()
	comment, found, readErr := pools.GetPoolComment(readCtx, pool)
	var msg string
	switch {
	case readErr != nil:
		msg = fmt.Sprintf("withVMIDLock: lock %q is held by another process, and its claim could not be read", pool)
	case !found:
		msg = fmt.Sprintf("withVMIDLock: lock %q was held throughout the wait and released after it; a retry can take it", pool)
	default:
		msg = fmt.Sprintf("withVMIDLock: lock %q is held by %s", pool, describeVMIDLockClaim(comment))
	}
	return cpierrors.WrapAs(timeoutErr, cpierrors.TypeRetriableCloud, msg)
}

// describeVMIDLockClaim renders a sentinel comment of the form
// "owner=<token> exp=<unix-seconds>" as the claim itself followed by the
// expiry in UTC. A comment without a readable expiry is quoted whole instead.
// Either way the text is flattened to printable ASCII and capped at
// vmidLockClaimLimit, because the comment comes back from PVE and anyone with
// Pool.Allocate can write one.
func describeVMIDLockClaim(comment string) string {
	var owner, exp string
	for _, field := range strings.Fields(comment) {
		if v, ok := strings.CutPrefix(field, "owner="); ok {
			owner = v
		} else if v, ok := strings.CutPrefix(field, "exp="); ok {
			exp = v
		}
	}
	secs, err := strconv.ParseInt(exp, 10, 64)
	if owner == "" || err != nil {
		return fmt.Sprintf("a claim with no readable owner or expiry: %s", flattenVMIDLockClaim(comment))
	}
	claim := flattenVMIDLockClaim("owner=" + owner + " exp=" + exp)
	return fmt.Sprintf("%s, which lapses at %s", claim, time.Unix(secs, 0).UTC().Format(time.RFC3339))
}

// flattenVMIDLockClaim keeps printable ASCII, replaces everything else with
// '?', and cuts the result at vmidLockClaimLimit bytes.
func flattenVMIDLockClaim(s string) string {
	var b strings.Builder
	for _, r := range s {
		if b.Len() >= vmidLockClaimLimit {
			b.WriteString("...")
			break
		}
		if r < 0x20 || r > 0x7e {
			b.WriteByte('?')
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
