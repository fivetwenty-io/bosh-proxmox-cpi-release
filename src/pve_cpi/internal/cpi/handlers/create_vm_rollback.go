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
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

// sweepCandidateVMID destroys the partial VM a failed create attempt may
// have left at candidate, but only after checking the guest there is ours.
// The sweeping paths are commit-indeterminate (a dropped POST or lost await
// may not have registered the VMID at all), so between the failure and this
// sweep a concurrent create can win the same VMID; destroying the winner
// would take a live VM this process never owned. The name our own create
// params carried is the discriminator: a guest whose name differs is the
// peer's and is left alone. A missing guest returns without any destroy; an
// unnamed guest, an unreadable config, or an empty expectedName falls
// through to the normal cleanup, whose destroy tolerates already-gone
// verdicts (best-effort: the guard narrows the exposure, it cannot close it
// for two attempts that chose the same name; that residual window is itself
// narrowed by the allocator's used-set now including the authoritative
// per-node listings, so a peer's just-registered VMID is normally never
// handed out twice in the first place).
func sweepCandidateVMID(ctx context.Context, deps Deps, node string, candidate int, expectedName string, env map[string]any, logger *log.Logger) {
	if expectedName != "" {
		cfg, cfgErr := deps.PVE.QEMU().Config(ctx, node, candidate)
		if cfgErr != nil {
			if pve.IsNotFound(cfgErr) || pve.IsPmxcfsConfigMissing(cfgErr) {
				return
			}
			// Unreadable: proceed; cleanupVM's own destroy handling
			// re-classifies and tolerates already-gone.
		} else if name, _ := cfg[metadataKeyName].(string); name != "" && name != expectedName {
			logger.Warn("create_vm: candidate VMID now holds a different VM (a concurrent create won the ID); leaving it alone",
				log.Int("vmid_attempted", candidate),
				log.String("expected_name", expectedName),
				log.String("actual_name", name),
			)
			return
		}
	}
	cleanupVMDetached(ctx, deps, node, candidate, env, logger)
}

// handleCreateError classifies a QEMU.Create error and logs the appropriate
// message. It cleans up transient-transport failures (where the POST may have
// committed) and returns the original error so AllocateWithRetry can retry.
func handleCreateError(
	ctx context.Context,
	deps Deps,
	logger *log.Logger,
	node string,
	candidate int,
	candidateName string,
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
		sweepCandidateVMID(ctx, deps, node, candidate, candidateName, nil, logger)
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
	candidateName string,
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
		sweepCandidateVMID(ctx, deps, node, candidate, candidateName, nil, logger)
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
		sweepCandidateVMID(ctx, deps, node, candidate, candidateName, nil, logger)
		return werr
	}
	// Non-conflict failure after Create succeeded: the VM may
	// have been partially registered. Roll back this attempt
	// before propagating so the next retry (which won't run)
	// or the caller sees a clean slate.
	sweepCandidateVMID(ctx, deps, node, candidate, candidateName, nil, logger)
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
		cleanupVMDetached(ctx, deps, node, vmid, env, logger)
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

// protectForeignDisksOnRollback keeps a rollback purge from destroying
// persistent volumes the failed create already attached. With config in hand
// it runs the same two guards delete_vm runs before its destroy:
// detachForeignActiveDisks (stable-ID disks transfer to a parker so the
// anchor promise survives; legacy disks plain-detach and stay free-floating)
// and guardUnusedVolumes (a persistent volume demoted to unusedN must not
// ride the purge either). Without config (the shared helper is reachable
// with deps.Config unset) the transfer guard cannot run, so a foreign disk
// on an active slot refuses the purge outright instead.
//
// Two deliberate asymmetries with delete_vm's use of the same guards. First,
// delete_vm confirms the VM stopped before detaching; here the Stop above is
// best-effort and unawaited-on-error, so a detach can race a still-running
// guest. The transfer path tolerates a running source (detach-to-unusedN,
// then reassign), and any detach failure fails closed into the preserve
// branch -- the cost of the race is an unnecessary orphan, never a lost
// volume. Second, the reused helpers log and wrap errors with their native
// "delete_vm:" prefix; during a create_vm rollback those lines appear inside
// the create_vm request's log stream. Accepted for now to keep the guards
// byte-identical with the delete path they mirror.
func protectForeignDisksOnRollback(ctx context.Context, deps Deps, node, vmCID string, vmid int, logger *log.Logger) error {
	if deps.Config == nil {
		cfg, cfgErr := deps.PVE.QEMU().Config(ctx, node, vmid)
		if cfgErr != nil {
			if pve.IsNotFound(cfgErr) || pve.IsPmxcfsConfigMissing(cfgErr) {
				return nil // VM gone -- the purge below is idempotent about that
			}
			return cpierrors.Wrap(pve.WrapError(cfgErr),
				fmt.Sprintf("create_vm: rollback: read config for VM %s before purge", vmCID))
		}
		if foreign := pve.FindForeignActiveDisks(cfg, vmid); len(foreign) > 0 {
			return cpierrors.Cloud(
				"create_vm: rollback: refusing to purge VM %s -- persistent disks still attached on active slots and no CPI config is available to park them: %v",
				vmCID, foreign)
		}
		return nil
	}
	if err := detachForeignActiveDisks(ctx, deps, node, vmCID, vmid, logger); err != nil {
		return err
	}
	return guardUnusedVolumes(ctx, deps, node, vmCID, vmid, deps.Config.DiskStorage)
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
// env carries the deploy identity for failure tagging: callers rolling back
// the final VM of a create (rollbackOnExit, the node-fallback loop) pass the
// parsed env so a VM preserved by the foreign-disk guard is tagged and
// findable; callers cleaning up an intermediate allocation candidate (which
// cannot have persistent disks attached and has no deploy identity worth
// stamping) pass nil.
func cleanupVMDetached(ctx context.Context, deps Deps, node string, vmid int, env map[string]any, logger *log.Logger) {
	rbCtx, rbCancel := detachedContext(ctx, rollbackCleanupTimeout)
	defer rbCancel()
	cleanupVM(rbCtx, deps, node, vmid, env, logger)
}

// stopVMForRollback stops the VM best-effort ahead of the rollback purge (the
// VM may not have started yet). The stop rides RetryOnTransient so a
// pvedaemon worker-recycle during rollback doesn't bubble out. A killed
// worker or node reboot mid-clone/mid-create can leave the guest config
// carrying an in-flight lock (lock: clone|create|...), which PVE rejects even
// a Stop against; retry once with skiplock=true when the CPI is authenticated
// as root@pam via password (the only identity PVE honors skiplock for — not
// even an API token owned by root@pam qualifies, see pve.IsRootPamIdentity).
// Otherwise the lock rejection stands, which is fine: Stop is best-effort
// and the purge is where the orphan actually matters.
func stopVMForRollback(ctx context.Context, deps Deps, node, vmCID string, vmid int, logger *log.Logger) {
	var stopUPID string
	stopErr := pve.RetryOnTransient(ctx, logger, "create_vm.cleanup.stop", 0, func() error {
		var innerErr error
		stopUPID, innerErr = deps.PVE.QEMU().Stop(ctx, node, vmid)
		return innerErr
	})
	if stopErr != nil && pve.IsVMConfigLocked(stopErr) {
		stopUPID, stopErr = retryStopWithSkiplock(ctx, deps, node, vmCID, vmid, stopErr, logger)
	}
	if stopErr == nil && stopUPID != "" {
		if awaitErr := pve.AwaitTaskWithLogger(ctx, deps.PVE, node, stopUPID, logger); awaitErr != nil {
			logger.Warn("create_vm: rollback stop task failed", log.Int(metadataKeyVMID, vmid), log.Err(awaitErr))
		}
	}
}

func cleanupVM(ctx context.Context, deps Deps, node string, vmid int, env map[string]any, logger *log.Logger) {
	logger.Warn("create_vm: rolling back, destroying created VM", log.Int(metadataKeyVMID, vmid))
	vmCID := strconv.Itoa(vmid)

	stopVMForRollback(ctx, deps, node, vmCID, vmid, logger)

	// Protect persistent disks before the purge. The purge below destroys
	// every disk the VM config references -- including a persistent disk
	// attachPersistentDisks already attached when a later create stage (agent
	// configure, start, post-start checks) failed and armed this rollback.
	// delete_vm runs detachForeignActiveDisks for exactly this reason; the
	// rollback path must too, or a routine start timeout turns into the loss
	// of the very volume the recreate was trying to re-attach. Fail-closed:
	// when the disks cannot be moved to safety, the VM is tagged and LEFT --
	// an orphaned VM is recoverable, a purged persistent volume is not.
	if protErr := protectForeignDisksOnRollback(ctx, deps, node, vmCID, vmid, logger); protErr != nil {
		logger.Error("create_vm: rollback: persistent disks could not be detached to safety; preserving the VM instead of purging it (detach the disks, then remove the VM with delete_vm or qm destroy)",
			log.Int(metadataKeyVMID, vmid), log.String("node", node), log.Err(protErr))
		if env != nil {
			tagFailedVM(ctx, deps, node, vmid, env, logger)
		}
		// The preserved VM is outside BOSH's view (no CID was ever returned),
		// so nothing will ever GC its cluster-level HA rule; drop the pin now.
		// The ConfigDrive ISO is deliberately kept -- the preserved VM still
		// references it, and the operator's eventual delete removes it.
		if pinErr := removeNodeAffinityPin(ctx, deps, vmid, logger); pinErr != nil {
			logger.Warn("create_vm: rollback node-affinity pin cleanup incomplete on preserved VM (non-fatal)",
				log.Int(metadataKeyVMID, vmid), log.Err(pinErr))
		}
		return
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
	// The destroy rides RetryOnTransientOrLock: this rollback usually fires
	// because a storage fault just failed the create, so the very next
	// mutation into the same contended storage lock must not be single-shot
	// (one cfs-lock timeout would otherwise orphan the VM permanently, since
	// no caller ever retries a rollback). The budget is bounded
	// (rollbackDestroyMaxAttempts) because this retry shares the detached
	// rollback context with the task await, ISO removal, and HA pin removal
	// that follow. Config-lock rejections ("lock: clone" and friends) are not
	// in the retry union and fall through unchanged to the skiplock and
	// lock-clear branches below; already-gone verdicts (404, pmxcfs
	// config-missing 500) are short-circuited inside the closure so the
	// blanket 5xx transient rule cannot spend the budget on a VM that no
	// longer exists, and the switch below still sees them via delErr.
	var delResp *sdknodes.DeleteQemuResponse
	var delErr error
	_ = pve.RetryOnTransientOrLock(ctx, logger, "create_vm.cleanup.destroy", rollbackDestroyMaxAttempts, func() error {
		delResp, delErr = deps.PVE.Nodes().DeleteQemu(ctx, node, vmCID, &sdknodes.DeleteQemuParams{
			Purge:                    &purge,
			DestroyUnreferencedDisks: &destroyUnref,
		})
		if delErr != nil && (pve.IsNotFound(delErr) || pve.IsPmxcfsConfigMissing(delErr)) {
			return nil
		}
		return delErr
	})
	if delErr != nil && pve.IsVMConfigLocked(delErr) {
		delResp, delErr = retryDestroyWithSkiplock(ctx, deps, node, vmCID, vmid, purge, destroyUnref, delErr, logger)
	}
	if delErr != nil && pve.IsVMConfigLocked(delErr) {
		delResp, delErr = retryDestroyAfterLockClear(ctx, deps, node, vmCID, vmid, purge, destroyUnref, delErr, logger)
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
