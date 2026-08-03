// create_vm_rollback.go handles create/clone/await failure classification
// and the rollback and cleanup of partially-created VMs.
// Split out of create_vm.go (mechanical move, no behavior change).
package handlers

import (
	"context"
	"fmt"
	"strconv"

	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
	sdknodes "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/nodes"
)

// handleCreateError classifies a QEMU.Create error and logs the appropriate
// message. It cleans up transient-transport failures (where the POST may have
// committed) and returns the original error so AllocateWithRetry can retry.
func handleCreateError(
	ctx context.Context,
	deps Deps,
	logger *log.Logger,
	node string,
	candidate int,
	cerr error,
) error {
	// Classify mutually exclusively so VMID conflicts don't
	// trigger the transient-transport cleanup path: a "VM N
	// already exists" 500 satisfies both predicates, but
	// destroying the conflicting VMID would wipe another
	// process's in-flight VM.
	switch {
	case pve.IsVMIDConflict(cerr):
		logger.Info("create_vm: vmid conflict, retrying",
			log.Int("vmid_attempted", candidate),
		)
	case pve.IsStorageLockTimeout(cerr):
		logger.Info("create_vm: storage lock timeout on create, retrying",
			log.Int("vmid_attempted", candidate),
		)
	case pve.IsTransientTransport(cerr):
		// HTTP 596 or auth-EOF: pvedaemon worker cycled
		// mid-request. POST may or may not have committed
		// the VM. Sweep this VMID before the next attempt
		// so the cluster list is clean.
		logger.Info("create_vm: transient transport fault on create, retrying",
			log.Int("vmid_attempted", candidate),
			log.ErrScrubbed(cerr),
		)
		cleanupVMDetached(ctx, deps, node, candidate, logger)
	}
	return cerr
}

// handleAwaitError classifies an AwaitTask error after QEMU.Create succeeded.
// It cleans up partially registered VMIDs for retriable errors and returns the
// original error so AllocateWithRetry can retry.
func handleAwaitError(
	ctx context.Context,
	deps Deps,
	logger *log.Logger,
	node string,
	candidate int,
	werr error,
) error {
	if pve.IsVMIDConflict(werr) {
		logger.Info("create_vm: vmid conflict on await, retrying",
			log.Int("vmid_attempted", candidate),
		)
		return werr
	}
	if pve.IsStorageLockTimeout(werr) {
		logger.Info("create_vm: storage lock timeout on import, retrying",
			log.Int("vmid_attempted", candidate),
		)
		// PVE rolled back its own qmcreate task — but the
		// VMID may still be registered with the partial
		// state. Clean up before the next attempt.
		cleanupVMDetached(ctx, deps, node, candidate, logger)
		return werr
	}
	if pve.IsTransientTransport(werr) {
		logger.Info("create_vm: transient transport fault on await, retrying",
			log.Int("vmid_attempted", candidate),
			log.ErrScrubbed(werr),
		)
		// The qmcreate task itself may still be running on
		// PVE — we only lost the await connection. Clean
		// up the VMID so a fresh attempt has a clean slate.
		cleanupVMDetached(ctx, deps, node, candidate, logger)
		return werr
	}
	// Non-conflict failure after Create succeeded: the VM may
	// have been partially registered. Roll back this attempt
	// before propagating so the next retry (which won't run)
	// or the caller sees a clean slate.
	cleanupVMDetached(ctx, deps, node, candidate, logger)
	return werr
}

// rollbackOnExit is createVM's deferred cleanup for the post-allocation stages.
// It destroys the just-created VM when createVM returns an error (*retErr set)
// or panics, so a failed create never leaks a VM. On panic it cleans up and
// re-panics so the dispatcher's recover still maps the panic to a CPI error —
// Go does not assign the named return on a panic unwind, so the *retErr guard
// alone would miss the panic path. vmCreated and retErr are read through
// pointers because both are still mutating when the defer is registered.
// deferred guard that must observe createVM's final named-return error and the
// latest vmCreated value, both still mutating when the defer is registered.
//
//nolint:gocritic // retErr and vmCreated are pointers by necessity: this is a
func rollbackOnExit(
	ctx context.Context, deps Deps, node string, vmid int, env map[string]any,
	logger *log.Logger, vmCreated *bool, retErr *error,
) {
	if r := recover(); r != nil {
		if *vmCreated {
			rbCtx, rbCancel := detachedContext(ctx, rollbackCleanupTimeout)
			disposeFailedVM(rbCtx, deps, node, vmid, env, logger)
			rbCancel()
		}
		panic(r)
	}
	if *retErr != nil && *vmCreated {
		if deps.Config.KeepFailedVMsEnabled() {
			rbCtx, rbCancel := detachedContext(ctx, rollbackCleanupTimeout)
			tagFailedVM(rbCtx, deps, node, vmid, env, logger)
			rbCancel()
			*retErr = preserveFailedVMError(*retErr, vmid, node)
			return
		}
		cleanupVMDetached(ctx, deps, node, vmid, logger)
	}
}

// disposeFailedVM either preserves (keep-failed mode) or destroys a VM that is
// being abandoned on the panic path. On the panic path retErr is not assigned,
// so the caller re-panics afterward; here we only need to tag-or-destroy.
func disposeFailedVM(ctx context.Context, deps Deps, node string, vmid int, env map[string]any, logger *log.Logger) {
	if deps.Config.KeepFailedVMsEnabled() {
		tagFailedVM(ctx, deps, node, vmid, env, logger)
		return
	}
	cleanupVM(ctx, deps, node, vmid, env, logger)
}

// tagFailedVM marks a VM that failed mid-creation with "bosh-create-failed"
// plus the deployment/job derived from env, so an operator can find it in the
// PVE UI. Existing tags (operator custom tags stamped at create) are preserved:
// PVE's Tags field is full-replace, so the current tags are read and merged
// rather than overwritten. It is best-effort: a tagging failure is logged, never
// propagated — the create error is what matters. The VM is left running, intact.
//
// The tag read-modify-write runs under a per-VMID cluster lock so a concurrent
// set_vm_metadata call cannot interleave and lose either writer's changes.
// Lock acquisition failure is also best-effort: logged, then the RMW proceeds
// unlocked rather than skipping the failure tag entirely.
func tagFailedVM(ctx context.Context, deps Deps, node string, vmid int, env map[string]any, logger *log.Logger) {
	entries := []string{"bosh-create-failed"}
	// instanceGroupName falls back to the env.bosh.instance name on the
	// create-env path (where env.bosh.group is absent), so a failed bootstrap VM
	// still gets a job tag.
	job := instanceGroupName(env)
	if deployment := sanitizeTagValue(extractDeploymentFromEnv(env, extractJobNameFromEnv(env))); deployment != "" {
		entries = append(entries, "deployment--"+deployment)
	}
	if j := sanitizeTagValue(job); j != "" {
		entries = append(entries, "job--"+j)
	}

	// tagRMW is the actual read-modify-write body. It is called under the
	// per-VMID cluster lock when the pool service is available, or directly
	// (best-effort, unlocked) when it is not.
	tagRMW := func() {
		// Preserve whatever tags the VM already carries (operator custom tags set
		// at QEMU.Create). Best-effort read: on failure we still apply the failure tag.
		var existing []string
		if cfg, cfgErr := deps.PVE.QEMU().Config(ctx, node, vmid); cfgErr == nil {
			if v, ok := cfg[jsonKeyTags]; ok {
				if s, ok := v.(string); ok {
					existing = parseTagsField(s)
				}
			}
		}

		tags := mergeTagList(existing, entries, maxTagLength)
		if err := deps.PVE.Nodes().UpdateQemuConfig(ctx, node, strconv.Itoa(vmid),
			&sdknodes.UpdateQemuConfigParams{Tags: &tags}); err != nil {
			logger.Warn("create_vm: keep_failed_vms tag write failed (non-fatal)",
				log.Int(metadataKeyVMID, vmid), log.String("node", node), log.Err(err))
			return
		}
		logger.Info("create_vm: VM preserved for diagnostics (debug.keep_failed_vms)",
			log.Int(metadataKeyVMID, vmid), log.String("node", node), log.String(jsonKeyTags, tags))
	}

	lockOwner := fmt.Sprintf("tagFailedVM/%d", vmid)
	lockErr := withVMIDLock(ctx, deps.PVE.Pools(), vmid, lockOwner, logger, func() error {
		tagRMW()
		return nil
	})
	if lockErr != nil {
		// Lock unavailable (pool service nil or cluster fault): proceed unlocked
		// so the failure tag is still written. A lost concurrent write is
		// acceptable here; the failure tag is the critical signal.
		logger.Warn("create_vm: tagFailedVM: could not acquire VMID lock; tagging without lock (best-effort)",
			log.Int(metadataKeyVMID, vmid), log.Err(lockErr))
		tagRMW()
	}
}

// preserveFailedVMError wraps the original create failure with a message naming
// the preserved VMID and node, so the director's error clearly states the VM was
// retained rather than destroyed. Non-retriable: a retry would re-create and
// fail again, leaving a second preserved VM.
func preserveFailedVMError(orig error, vmid int, node string) error {
	return cpierrors.Cloud(
		"create_vm: VM %d on node %q preserved for diagnostics (debug.keep_failed_vms=true); original error: %s",
		vmid, node, orig.Error(),
	)
}

// --------------------------------------------------------------------------
// cleanupVM attempts to stop and purge a created VM on error. All errors are
// logged but suppressed so the original error propagates unmodified.
//
// env carries the BOSH deploy identity (deployment/job) used to tag the VM
// "bosh-create-failed" when the purge cannot complete because the guest
// config is locked (see the locked-delete branch below) — best-effort and
// fail-open: env is nil for callers that clean up an intermediate placement-
// fallback or VMID-retry candidate (a VM that was never the final outcome and
// has no deploy identity to tag with), in which case tagging is skipped, but
// the skiplock recovery attempt and the actionable log message still run
// regardless of env.
// --------------------------------------------------------------------------
// cleanupVMDetached runs cleanupVM on a cancellation-detached context bounded
// by rollbackCleanupTimeout. Every rollback call site funnels here so cleanup
// survives caller cancellation without inheriting the SDK's 30-minute
// transport timeout times a retry budget as its only bound (see
// detachedContext).
func cleanupVMDetached(ctx context.Context, deps Deps, node string, vmid int, logger *log.Logger) {
	rbCtx, rbCancel := detachedContext(ctx, rollbackCleanupTimeout)
	defer rbCancel()
	cleanupVM(rbCtx, deps, node, vmid, nil, logger)
}

func cleanupVM(ctx context.Context, deps Deps, node string, vmid int, env map[string]any, logger *log.Logger) {
	logger.Warn("create_vm: rolling back, destroying created VM", log.Int(metadataKeyVMID, vmid))
	vmCID := strconv.Itoa(vmid)

	// Stop (best-effort; VM may not have started yet). Wrap in RetryOnTransient
	// so a pvedaemon worker-recycle during rollback doesn't bubble out — this
	// path is best-effort already and absorbing the transient keeps the
	// rollback graceful.
	var stopUPID string
	stopErr := pve.RetryOnTransient(ctx, logger, "create_vm.cleanup.stop", 0, func() error {
		var innerErr error
		stopUPID, innerErr = deps.PVE.QEMU().Stop(ctx, node, vmid)
		return innerErr
	})
	// A killed worker or node reboot mid-clone/mid-create can leave the guest
	// config carrying an in-flight lock (lock: clone|create|...), which PVE
	// rejects even a Stop against. Retry once with skiplock=true when the CPI
	// is authenticated as root@pam via password (the only identity PVE
	// honors skiplock for — not even an API token owned by root@pam
	// qualifies, see pve.IsRootPamIdentity); otherwise this is a no-op and
	// stopErr is left as the lock rejection, which is fine — Stop is
	// best-effort and the purge below is where the orphan actually matters.
	if stopErr != nil && pve.IsVMConfigLocked(stopErr) {
		stopUPID, stopErr = retryStopWithSkiplock(ctx, deps, node, vmCID, vmid, stopErr, logger)
	}
	if stopErr == nil && stopUPID != "" {
		if awaitErr := pve.AwaitTaskWithLogger(ctx, deps.PVE, node, stopUPID, logger); awaitErr != nil {
			logger.Warn("create_vm: rollback stop task failed", log.Int(metadataKeyVMID, vmid), log.Err(awaitErr))
		}
	}

	// Purge the VM. DestroyUnreferencedDisks follows pve.destroy_unreferenced_disks
	// (default false; see the config field doc for the cross-cluster shared-storage
	// data-loss hazard enabling it introduces). This rollback fires on every failed
	// create (placement rejection, cloud-init error, NIC validation error, ...) —
	// a routine path, not an exceptional one — so it must honor the same safety
	// default every other DeleteQemu call site does (delete_vm.go's straggler
	// sweep, sync path, and fast path all read the same config field). deps.Config
	// nil-checked here (unlike a method call such as KeepFailedVMsEnabled, a plain
	// field read has no nil-receiver safety of its own) because cleanupVM is a
	// shared rollback helper reachable with deps.Config unset.
	purge := true
	destroyUnref := deps.Config != nil && deps.Config.DestroyUnreferencedDisks
	delResp, delErr := deps.PVE.Nodes().DeleteQemu(ctx, node, vmCID, &sdknodes.DeleteQemuParams{
		Purge:                    &purge,
		DestroyUnreferencedDisks: &destroyUnref,
	})
	if delErr != nil && pve.IsVMConfigLocked(delErr) {
		delResp, delErr = retryDestroyWithSkiplock(ctx, deps, node, vmCID, vmid, purge, destroyUnref, delErr, logger)
	}
	if delErr != nil {
		switch {
		case pve.IsNotFound(delErr) || pve.IsPmxcfsConfigMissing(delErr):
			logger.Info("create_vm: rollback delete -- VM already gone (idempotent)", log.Int(metadataKeyVMID, vmid))
		case pve.IsVMConfigLocked(delErr):
			// Skiplock retry above either was not attempted (identity is not
			// root@pam) or was attempted and still failed: the VM is orphaned,
			// locked, and not destroyed. Surface the actionable recovery
			// command and tag it (when a deploy identity is available) so
			// keep_failed_vms tooling and operators can find it — this VM was
			// never meant to be preserved, but ended up stuck, so visibility
			// matters regardless of the keep_failed_vms setting.
			logUnresolvedVMLock(logger, "create_vm: rollback delete failed", vmid, node, delErr)
			if env != nil {
				tagFailedVM(ctx, deps, node, vmid, env, logger)
			}
		default:
			logger.Error("create_vm: rollback delete failed", log.Int(metadataKeyVMID, vmid), log.Err(delErr))
		}
	} else {
		// Await the destroy task so PVE fully releases the VMID before we return.
		// An empty UPID means synchronous completion; skip await in that case.
		if delResp != nil {
			delUPID, upidErr := pve.UPIDFromRaw(*delResp)
			if upidErr != nil {
				logger.Warn("create_vm: cannot parse UPID from rollback delete response -- skipping await",
					log.Int(metadataKeyVMID, vmid), log.Err(upidErr))
			} else if delUPID != "" {
				if awaitErr := pve.AwaitTaskWithLogger(ctx, deps.PVE, node, delUPID, logger); awaitErr != nil {
					if pve.IsNotFound(awaitErr) || pve.IsPmxcfsConfigMissing(awaitErr) {
						logger.Info("create_vm: rollback destroy await -- VM already gone (idempotent)",
							log.Int(metadataKeyVMID, vmid))
					} else {
						logger.Error("create_vm: rollback destroy await failed",
							log.Int(metadataKeyVMID, vmid), log.Err(awaitErr))
					}
				}
			}
		}
		logger.Info("create_vm: rollback complete", log.Int(metadataKeyVMID, vmid))
	}

	// Remove any agent-side artifacts (e.g. the ConfigDrive ISO uploaded by
	// the configdrive agent). VM purge removes referenced disk volumes
	// but does not touch independent content uploaded with content=iso, so
	// the ISO must be deleted via the agent. Order matters: purge first, so
	// the CD-ROM reference is gone before the underlying volume is removed.
	if deps.Agent != nil {
		if remErr := deps.Agent.Remove(ctx, node, vmid); remErr != nil {
			logger.Warn("create_vm: rollback agent remove failed",
				log.Int(metadataKeyVMID, vmid), log.Err(remErr))
		}
	}

	// Remove the node-affinity HA pin (bosh-na-<vmid>) and deregister its HA
	// resource. VM purge does not GC the cluster-level HA rule, so without this
	// a rolled-back create that reached a pin step would leave an orphan rule
	// referencing a destroyed VM. Unconditional because two writers create this
	// rule: the AZ pin (gated by HANodeAffinityPinEnabled) and the PCI strict
	// pin (applied whenever pci_passthroughs is set, regardless of that flag).
	// removeNodeAffinityPin is idempotent and not-found-tolerant, so for a VM
	// that never had a pin this is two cheap no-op HA calls. Best-effort.
	if pinErr := removeNodeAffinityPin(ctx, deps, vmid, logger); pinErr != nil {
		logger.Warn("create_vm: rollback node-affinity pin cleanup incomplete (non-fatal)",
			log.Int(metadataKeyVMID, vmid), log.Err(pinErr))
	}
}
