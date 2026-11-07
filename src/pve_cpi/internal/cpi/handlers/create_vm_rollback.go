// create_vm_rollback.go handles create/clone/await failure classification
// and the rollback and cleanup of partially-created VMs.
// Split out of create_vm.go (mechanical move, no behavior change).
package handlers

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
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
		} else if name, _ := pve.ConfigString(cfg, metadataKeyName); name != "" && name != expectedName {
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
			keepFailedVM(rbCtx, deps, node, vmid, env, logger)
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
		keepFailedVM(ctx, deps, node, vmid, env, logger)
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
	tagFailedVMFor(ctx, deps, node, vmid, env, false, logger)
}

// keepFailedVM tags a VM that debug.keep_failed_vms preserves. It is
// tagFailedVM that also drops the VM's birth tag, so the deferred-teardown
// sweep never destroys a VM the operator asked to keep.
func keepFailedVM(ctx context.Context, deps Deps, node string, vmid int, env map[string]any, logger *log.Logger) {
	tagFailedVMFor(ctx, deps, node, vmid, env, true, logger)
}

func tagFailedVMFor(
	ctx context.Context, deps Deps, node string, vmid int, env map[string]any, kept bool, logger *log.Logger,
) {
	entries := []string{tagCreateFailed}
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
			if s, ok := pve.ConfigString(cfg, jsonKeyTags); ok {
				existing = parseTagsField(s)
			}
		}

		// hoistHeadTags keeps the ownership record ahead of the tail the
		// byte cap truncates, whatever order PVE returned the tags in.
		if kept {
			existing = withoutBirthTags(existing)
		}
		tags := mergeTagList(hoistHeadTags(existing), entries, maxTagLength)
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
		// Both views, the same read delete_vm's guards make, so a foreign slot
		// whose delete is pending still refuses the purge.
		holding, cfgErr := pve.ReadQemuHolding(ctx, deps.PVE, node, vmid)
		if cfgErr != nil {
			if pve.IsNotFound(cfgErr) || pve.IsPmxcfsConfigMissing(cfgErr) {
				// The node holds no config for the VM. Only a VM proven gone
				// from every member lets the purge below run, idempotent about
				// the absence; one that moved fails closed into the preserve
				// branch.
				return confirmVMGone(ctx, deps, "create_vm: rollback", vmid, node, "the config read before purge")
			}
			return cpierrors.Wrap(pve.WrapError(cfgErr),
				fmt.Sprintf("create_vm: rollback: read config for VM %s before purge", vmCID))
		}
		if err := refusePendingDriveReplacement("create_vm: rollback", vmCID, holding); err != nil {
			return err
		}
		cfg := holding.Config
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
//
// A VM that holds a migrate lock is never stopped with skiplock, because that
// would interrupt the migration. The skiplock retry refuses it, and so does
// one whose lock cannot be read, and that refusal is the one error returned,
// so the teardown defers the VM to the sweep. Every other stop failure is
// logged and returns nil, and the protocol's wait for the VM to halt decides.
func stopVMForRollback(ctx context.Context, deps Deps, node string, vmid int, logger *log.Logger) error {
	var stopUPID string
	stopErr := pve.RetryOnTransient(ctx, logger, "create_vm.cleanup.stop", 0, func() error {
		var innerErr error
		stopUPID, innerErr = deps.PVE.QEMU().Stop(ctx, node, vmid)
		return innerErr
	})
	if stopErr != nil && pve.IsVMConfigLocked(stopErr) {
		stopUPID, stopErr = retryStopWithSkiplock(ctx, deps, "create_vm: rollback", node, vmid, stopErr, logger)
		if errors.Is(stopErr, errSkiplockStopDuringMigration) {
			return stopErr
		}
	}
	if stopErr != nil {
		logger.Debug("create_vm: rollback stop failed (best-effort; the teardown waits for the VM to halt)",
			log.Int(metadataKeyVMID, vmid), log.Err(stopErr))
		return nil
	}
	if stopUPID != "" {
		if awaitErr := pve.AwaitTaskWithLogger(ctx, deps.PVE, node, stopUPID, logger); awaitErr != nil {
			logger.Warn("create_vm: rollback stop task failed", log.Int(metadataKeyVMID, vmid), log.Err(awaitErr))
		}
	}
	return nil
}

// cleanupVM tears down a VM whose create failed through the teardown
// protocol (see vmTeardown). Every failure is logged and suppressed, so the
// create's own error reaches the Director unchanged. A VM the protocol cannot
// destroy stays queued: its birth tag, and the bosh-deleting tag written here
// as a diagnostic, hand it to sweepDeferredTeardowns.
//
// The create may already have put the VM under PVE HA. A VM that PVE HA's
// resource configuration lists passes the same HA gate as delete_vm, and is
// stopped through PVE HA. One in fence, in recovery, in motion, on a node
// that is not online, or in a state that cannot be read is tagged and LEFT,
// with nothing written to HA, because a direct stop or destroy there races
// the CRM. Any other VM is stopped directly, best-effort, and the protocol
// waits on its owner node until it reports stopped.
//
// Persistent disks are protected before the purge (see
// protectForeignDisksOnRollback). The purge destroys every disk the VM config
// references, including a persistent disk attachPersistentDisks already
// attached when a later create stage failed. When the disks cannot be moved
// to safety, the VM is tagged and LEFT: an orphaned VM is recoverable, a
// purged persistent volume is not.
//
// DestroyUnreferencedDisks follows pve.destroy_unreferenced_disks (default
// false; see the config field doc for the cross-cluster shared-storage
// data-loss hazard enabling it introduces). This rollback fires on every
// failed create, a routine path, so it honors the same safety default every
// other destroy does. deps.Config may be nil here, because cleanupVM is a
// shared rollback helper reachable with deps.Config unset.
//
// A config lock rides the destroy loop's lock curve first, then the skiplock
// recovery for a root@pam password identity, then a bounded wait for the
// lock to clear followed by a second check and destroy. A migrate lock is
// never skipped or waited out.
//
// env carries the BOSH deploy identity used to tag a VM the rollback leaves
// "bosh-create-failed". It is nil for callers that clean up an intermediate
// placement-fallback or VMID-retry candidate, and tagging is skipped then.
//
// The rollback runs under the VM's VMID claim (see withVMIDClaim). Inside the
// create that claimed the VMID it reuses that claim. The middleware rollback
// runs after the create has released it, so it takes the claim again, waiting
// up to vmidLockTimeout. A claim it cannot take leaves the VM to the sweep,
// which its birth tag queues it for. A claim PVE refuses for lack of
// permission does not stop the rollback, which then runs unclaimed.
func cleanupVM(ctx context.Context, deps Deps, node string, vmid int, env map[string]any, logger *log.Logger) {
	claimErr := withVMIDClaim(ctx, deps, vmid, "create_vm-rollback/"+strconv.Itoa(vmid), claimForDisposal, logger,
		func(claimCtx context.Context) error {
			cleanupVMClaimed(claimCtx, deps, node, vmid, env, logger)
			return nil
		})
	if claimErr != nil {
		logger.Error("create_vm: rollback: could not claim the VM, so it is not destroyed here; it stays queued "+
			"for the deferred-teardown sweep", log.Int(metadataKeyVMID, vmid), log.String("node", node),
			log.Err(claimErr))
	}
}

// cleanupVMClaimed is cleanupVM's body, run under the VMID claim.
func cleanupVMClaimed(ctx context.Context, deps Deps, node string, vmid int, env map[string]any, logger *log.Logger) {
	const caller = "create_vm: rollback"
	logger.Warn("create_vm: rolling back, destroying created VM", log.Int(metadataKeyVMID, vmid))
	vmCID := strconv.Itoa(vmid)
	// This create allocated the VMID itself, so its config-drive ISO is ours to
	// remove at the end of the rollback whatever band the VMID lies in.
	ctx = pve.WithOwnedConfigDriveISO(ctx, vmid)
	// The diagnostic marker. The birth tag already queues the VM, so a write
	// that fails here, as it does under fencing or a quorum loss, loses only
	// the marker.
	stampDeletingTag(ctx, deps, node, vmCID, vmid, logger)

	identity := vmIdentity{node: node, vmid: vmid}
	teardown := &vmTeardown{
		caller:              caller,
		vmCID:               vmCID,
		vmid:                vmid,
		node:                node,
		logger:              logger,
		gate:                teardownGateListed,
		skiplock:            true,
		awaitLockClear:      true,
		destroyUnreferenced: deps.Config != nil && deps.Config.DestroyUnreferencedDisks,
		stop: func(ctx context.Context) (bool, error) {
			return false, stopVMForRollback(ctx, deps, node, vmid, logger)
		},
		preserve: func(ctx context.Context) (bool, error) {
			return false, protectForeignDisksOnRollback(ctx, deps, node, vmCID, vmid, logger)
		},
		owned: func(_ context.Context, holding pve.QemuHolding) error {
			// The config read right before the destroy names the VM the
			// destroy takes, so its identity is what the cleanups compare.
			identity = captureVMIdentity(node, vmid, holding.Config)
			return refuseForeignRollbackVM(deps, vmCID, holding)
		},
		awaitTask: func(ctx context.Context, resp *sdknodes.DeleteQemuResponse) error {
			return awaitRollbackDestroy(ctx, deps, node, vmid, resp, logger)
		},
	}
	result, err := teardown.run(ctx, deps)
	if err != nil {
		cleanupVMLeftVM(ctx, deps, node, vmid, env, result.stage, err, logger)
		return
	}
	if result.gone {
		logger.Info("create_vm: rollback -- VM already gone (idempotent)", log.Int(metadataKeyVMID, vmid))
	} else {
		logger.Info("create_vm: rollback complete", log.Int(metadataKeyVMID, vmid))
	}
	cleanupRollbackArtifacts(ctx, deps, identity, logger)
}

// cleanupVMLeftVM logs and tags a VM the rollback's teardown did not destroy,
// by the step that stopped it.
func cleanupVMLeftVM(
	ctx context.Context, deps Deps, node string, vmid int, env map[string]any, stage teardownStage, err error,
	logger *log.Logger,
) {
	switch {
	case stage == teardownStageHA || stage == teardownStageStop && isWholeHADeferral(err):
		logger.Error("create_vm: rollback: the VM's HA state does not allow the destroy yet; preserving the VM "+
			"instead of purging it (the deferred-teardown sweep or delete_vm removes it once PVE HA has settled it)",
			log.Int(metadataKeyVMID, vmid), log.String("node", node), log.Err(err))
		if env != nil {
			tagFailedVM(ctx, deps, node, vmid, env, logger)
		}
	case stage == teardownStagePreserve:
		logger.Error("create_vm: rollback: persistent disks could not be detached to safety; preserving the VM "+
			"instead of purging it (detach the disks, then remove the VM with delete_vm or qm destroy)",
			log.Int(metadataKeyVMID, vmid), log.String("node", node), log.Err(err))
		if env != nil {
			tagFailedVM(ctx, deps, node, vmid, env, logger)
		}
		// The preserved VM is outside BOSH's view (no CID was ever returned),
		// so nothing GCs its cluster-level HA rule; drop the pin now. The
		// ConfigDrive ISO is deliberately kept, because the preserved VM still
		// references it.
		if pinErr := removeNodeAffinityPin(ctx, deps, vmid, logger); pinErr != nil {
			logger.Warn("create_vm: rollback node-affinity pin cleanup incomplete on preserved VM (non-fatal)",
				log.Int(metadataKeyVMID, vmid), log.Err(pinErr))
		}
	case pve.IsVMConfigLocked(err):
		// The skiplock retry either was not attempted (identity is not
		// root@pam) or still failed, and the lock did not clear: the VM is
		// locked and not destroyed. Tag it, when a deploy identity is
		// available, so operators can find it.
		logUnresolvedVMLock(logger, "create_vm: rollback delete failed", vmid, node, err)
		if env != nil {
			tagFailedVM(ctx, deps, node, vmid, env, logger)
		}
	default:
		logger.Error("create_vm: rollback delete failed; the VM stays queued for the deferred-teardown sweep",
			log.Int(metadataKeyVMID, vmid), log.String("node", node), log.String("stage", string(stage)),
			log.Err(err))
	}
}

// refuseForeignRollbackVM is step 4's ownership check for the rollback. This
// create allocated the VMID, so a VM with no ownership tags yet, as a create
// that failed before its first config write leaves, is still its own. A VM
// whose tags name another Director, two owners, a parker, or a stemcell
// template is not, because the VMID then holds something else.
func refuseForeignRollbackVM(deps Deps, vmCID string, holding pve.QemuHolding) error {
	tags, _ := pve.ConfigString(holding.Config, jsonKeyTags)
	if stragglerRowForeign(tags, deps.RequestDirectorUUID) {
		return cpierrors.Cloud(
			"create_vm: rollback: VM %s carries tags %q that do not belong to this create; refusing to destroy it",
			vmCID, tags)
	}
	return nil
}

// awaitRollbackDestroy awaits the rollback's destroy task. An unparseable
// response has no task to wait for, and the absence proof decides.
func awaitRollbackDestroy(
	ctx context.Context, deps Deps, node string, vmid int, resp *sdknodes.DeleteQemuResponse, logger *log.Logger,
) error {
	if resp == nil {
		return nil
	}
	upid, err := pve.UPIDFromRaw(*resp)
	if err != nil {
		logger.Warn("create_vm: cannot parse UPID from rollback delete response -- the absence proof decides",
			log.Int(metadataKeyVMID, vmid), log.Err(err))
		return nil
	}
	if upid == "" {
		return nil
	}
	if awaitErr := pve.AwaitTaskWithLogger(ctx, deps.PVE, node, upid, logger); awaitErr != nil {
		if pve.IsNotFound(awaitErr) || pve.IsPmxcfsConfigMissing(awaitErr) {
			logger.Info("create_vm: rollback destroy await -- VM config already gone; the absence proof decides",
				log.Int(metadataKeyVMID, vmid))
			return nil
		}
		return cpierrors.Wrap(pve.WrapError(awaitErr), fmt.Sprintf("create_vm: rollback: await destroy of VM %d", vmid))
	}
	return nil
}

// cleanupRollbackArtifacts removes what the destroy leaves behind once the VM
// is gone. The agent removes the ConfigDrive ISO uploaded with content=iso,
// which the purge does not touch, and runs after the destroy so the CD-ROM
// reference is gone first. The node-affinity pin (bosh-na-<vmid>) is removed
// too, because the purge does not GC the cluster-level rule, and two writers
// create it: the AZ pin and the PCI strict pin. Both steps are idempotent and
// best-effort. Both key on the VMID alone, so each first checks that the
// VMID is free or still holds the VM the rollback destroyed (see
// cleanupTargetStillOurs). A VM that was gone before the rollback's destroy
// has no captured identity, so its cleanups run only when the VMID is free.
func cleanupRollbackArtifacts(ctx context.Context, deps Deps, identity vmIdentity, logger *log.Logger) {
	node, vmid := identity.node, identity.vmid
	if deps.Agent != nil {
		runPostDestroyCleanup(ctx, deps, identity, "agent", logger, func() {
			if remErr := deps.Agent.Remove(ctx, node, vmid); remErr != nil {
				logger.Warn("create_vm: rollback agent remove failed",
					log.Int(metadataKeyVMID, vmid), log.Err(remErr))
			}
		})
	}
	runPostDestroyCleanup(ctx, deps, identity, "node_affinity_pin", logger, func() {
		if pinErr := removeNodeAffinityPin(ctx, deps, vmid, logger); pinErr != nil {
			logger.Warn("create_vm: rollback node-affinity pin cleanup incomplete (non-fatal)",
				log.Int(metadataKeyVMID, vmid), log.Err(pinErr))
		}
	})
}
