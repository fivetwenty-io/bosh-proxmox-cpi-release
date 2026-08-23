// Shared root@pam-password-only skiplock recovery for VMs stuck under a PVE
// guest-config lock (lock: clone|create|backup|migrate|snapshot|rollback).
// PVE's HTTP API has no unlock endpoint; the only in-process recovery is a
// skiplock=true retry, which PVE honors only for the root@pam superuser
// authenticated via password (PVE::API2::Qemu rejects it for every other
// identity regardless of granted privileges — including an API token owned
// by root@pam; see pve.IsRootPamIdentity's doc comment for why). Used by
// cleanupVM (create_vm.go rollback) and the delete_vm.go synchronous destroy
// path: both call the skiplock helpers so that retry's behavior, logging,
// and operator-facing recovery message stay identical across both call
// sites. The bounded lock-clear wait (retryDestroyAfterLockClear) is
// rollback-only, deliberately: a delete_vm still locked after skiplock
// surfaces a RETRIABLE error, so the Director's own re-drive converges once
// the lock holder finishes, while nothing ever re-drives a create_vm
// rollback, so it must wait out the lock in-call or leak the orphan.
package handlers

import (
	"context"
	"fmt"
	"time"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

// retryDestroyWithSkiplock retries a lock-rejected DeleteQemu with
// skiplock=true, but only when the CPI's configured PVE identity is
// root@pam authenticated via password (pve.IsRootPamIdentity). Any other
// identity — including an API token owned by root@pam — never qualifies, so
// origErr is returned unretried: PVE would reject skiplock=true for that
// identity anyway, and attempting it would only add a guaranteed-to-fail API
// call and a confusing log line. The caller's pve.IsVMConfigLocked check
// still fires against the SAME unretried error and drives its
// actionable-error / tagging path.
func retryDestroyWithSkiplock(
	ctx context.Context, deps Deps, node, vmCID string, vmid int,
	purge, destroyUnref bool, origErr error, logger *log.Logger,
) (*sdknodes.DeleteQemuResponse, error) {
	if !pve.IsRootPamIdentity(deps.Config) {
		return nil, origErr
	}
	lockType := pve.VMConfigLockType(origErr)
	logger.Warn("destroy rejected -- VM config is locked; retrying with skiplock (root@pam)",
		log.Int(metadataKeyVMID, vmid), log.String("node", node), log.String("lock_type", lockType))
	skiplock := true
	// The skiplock destroy fires into the same contended storage state that
	// just failed the primary destroy, so it must not be single-shot: one
	// cfs-lock timeout here would permanently orphan the VM the rollback is
	// reclaiming (nothing re-drives a rollback). Same budget and already-gone
	// short-circuit as the primary destroy in cleanupVM.
	var resp *sdknodes.DeleteQemuResponse
	var delErr error
	_ = pve.RetryOnTransientOrLock(ctx, logger, "vm_lock_recovery.skiplock_destroy", rollbackDestroyMaxAttempts, func() error {
		resp, delErr = deps.PVE.Nodes().DeleteQemu(ctx, node, vmCID, &sdknodes.DeleteQemuParams{
			Purge:                    &purge,
			DestroyUnreferencedDisks: &destroyUnref,
			Skiplock:                 &skiplock,
		})
		if delErr != nil && (pve.IsNotFound(delErr) || pve.IsPmxcfsConfigMissing(delErr)) {
			return nil
		}
		return delErr
	})
	return resp, delErr
}

// retryStopWithSkiplock is the Stop analogue of retryDestroyWithSkiplock. The
// higher-level qemu.Service.Stop used elsewhere has no params variant, so the
// retry goes through the raw nodes endpoint (POST
// /nodes/{node}/qemu/{vmid}/status/stop), which does expose Skiplock. Returns
// the UPID (empty string for a synchronous completion) on success, or
// origErr unretried when identity does not qualify (not root@pam via
// password — see retryDestroyWithSkiplock and pve.IsRootPamIdentity).
func retryStopWithSkiplock(
	ctx context.Context, deps Deps, node, vmCID string, vmid int, origErr error, logger *log.Logger,
) (string, error) {
	if !pve.IsRootPamIdentity(deps.Config) {
		return "", origErr
	}
	lockType := pve.VMConfigLockType(origErr)
	logger.Warn("stop rejected -- VM config is locked; retrying with skiplock (root@pam)",
		log.Int(metadataKeyVMID, vmid), log.String("node", node), log.String("lock_type", lockType))
	skiplock := true
	resp, err := deps.PVE.Nodes().CreateQemuStatusStop(ctx, node, vmCID, &sdknodes.CreateQemuStatusStopParams{
		Skiplock: &skiplock,
	})
	if err != nil {
		return "", err
	}
	if resp == nil {
		return "", nil
	}
	upid, upidErr := pve.UPIDFromRaw(*resp)
	if upidErr != nil {
		logger.Warn("cannot parse UPID from skiplock stop response -- skipping await",
			log.Int(metadataKeyVMID, vmid), log.Err(upidErr))
		return "", nil
	}
	return upid, nil
}

// logUnresolvedVMLock logs the actionable error surfaced when a stop/destroy
// call is rejected by PVE's guest-config lock and no further in-process
// recovery is possible — either the CPI is not root@pam (skiplock could not
// be attempted) or a skiplock retry was attempted and still failed. The lock
// usually belongs to an in-flight task (clone, create, migrate), so the
// recovery hint leads with waiting for that task to finish; `qm unlock` is
// right only when the task is gone and the lock is stale, and the HTTP API
// has no endpoint equivalent for it either way.
func logUnresolvedVMLock(logger *log.Logger, action string, vmid int, node string, err error) {
	logger.Error(action+": VM config is locked; manual recovery required",
		log.Int(metadataKeyVMID, vmid),
		log.String("node", node),
		log.String("lock_type", pve.VMConfigLockType(err)),
		log.String("recovery", fmt.Sprintf(
			"wait for the in-flight task holding the lock to finish, then retry the delete; run `qm unlock %d` only if no task is running and the lock is stale", vmid)),
		log.Err(err),
	)
}

// Lock-clear poll bounds for awaitVMConfigLockClear. Package vars, replaced
// by SetVMLockClearBounds in tests.
var (
	vmLockClearPollInterval = 2 * time.Second
	vmLockClearMaxWait      = 120 * time.Second
)

// SetVMLockClearBounds replaces the lock-clear poll interval and bound and
// returns a restore func. Test seam:
//
//	defer handlers.SetVMLockClearBounds(0, 50*time.Millisecond)()
func SetVMLockClearBounds(interval, maxWait time.Duration) func() {
	prevInterval, prevMax := vmLockClearPollInterval, vmLockClearMaxWait
	vmLockClearPollInterval = interval
	vmLockClearMaxWait = maxWait
	return func() {
		vmLockClearPollInterval = prevInterval
		vmLockClearMaxWait = prevMax
	}
}

// retryDestroyAfterLockClear handles a destroy refused by a guest-config lock
// when skiplock was unavailable (any API token) or refused. The lock is
// usually held by the failed create's own in-flight task (a clone dropped
// mid-task keeps `lock: clone` until the worker finishes or dies), so wait
// bounded for it to clear (awaitVMConfigLockClear), then destroy once more.
// Without this, every token-authenticated deploy that loses a clone mid-task
// leaks a locked orphan that only `qm unlock` can free. When the lock never
// clears, origErr is returned unchanged so the caller's lock classification
// still fires.
func retryDestroyAfterLockClear(
	ctx context.Context, deps Deps, node, vmCID string, vmid int,
	purge, destroyUnref bool, origErr error, logger *log.Logger,
) (*sdknodes.DeleteQemuResponse, error) {
	logger.Info("create_vm: rollback delete blocked by a config lock; waiting for the in-flight task to release it",
		log.Int(metadataKeyVMID, vmid),
		log.String("node", node),
		log.String("lock_type", pve.VMConfigLockType(origErr)),
	)
	if !awaitVMConfigLockClear(ctx, deps, node, vmid, logger) {
		return nil, origErr
	}
	// Not single-shot: the post-lock-clear destroy is the last chance to
	// reclaim this VM (nothing re-drives a rollback), and storage contention
	// that outlived the lock would otherwise orphan it on one timeout. Same
	// budget and already-gone short-circuit as the primary destroy.
	var resp *sdknodes.DeleteQemuResponse
	var delErr error
	_ = pve.RetryOnTransientOrLock(ctx, logger, "vm_lock_recovery.lockclear_destroy", rollbackDestroyMaxAttempts, func() error {
		resp, delErr = deps.PVE.Nodes().DeleteQemu(ctx, node, vmCID, &sdknodes.DeleteQemuParams{
			Purge:                    &purge,
			DestroyUnreferencedDisks: &destroyUnref,
		})
		if delErr != nil && (pve.IsNotFound(delErr) || pve.IsPmxcfsConfigMissing(delErr)) {
			return nil
		}
		return delErr
	})
	return resp, delErr
}

// awaitVMConfigLockClear polls the guest config until its lock field clears,
// reporting whether it did. It backs the rollback path for identities that
// cannot skiplock (every API token): a destroy refused with `lock: clone` is
// usually blocked by the failed clone's own in-flight task, which finishes or
// dies on its own, so a bounded wait converts a guaranteed orphan into a
// completed cleanup. Config-read errors and a VM that disappears mid-poll
// both report true: the follow-up destroy is the authority on what remains
// (a gone VM makes it an idempotent 404), and a read blip must not strand
// the orphan without even attempting the retry.
func awaitVMConfigLockClear(ctx context.Context, deps Deps, node string, vmid int, logger *log.Logger) bool {
	deadline := time.Now().Add(vmLockClearMaxWait)
	for {
		cfg, cfgErr := deps.PVE.QEMU().Config(ctx, node, vmid)
		if cfgErr != nil {
			return true
		}
		if lock, _ := cfg["lock"].(string); lock == "" {
			return true
		}
		if time.Now().After(deadline) {
			logger.Warn("VM config lock did not clear within the wait budget",
				log.Int(metadataKeyVMID, vmid),
				log.String("node", node),
			)
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(vmLockClearPollInterval):
		}
	}
}
