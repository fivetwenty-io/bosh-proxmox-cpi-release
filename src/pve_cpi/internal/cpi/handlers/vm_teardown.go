package handlers

import (
	"context"
	"errors"
	"fmt"
	"strings"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/vmcid"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

// vmTeardownGate says how a teardown reads the VM's HA state in its first step.
type vmTeardownGate int

const (
	// teardownGated is a teardown whose caller already ran gateHADelete and
	// passes the state it returned. delete_vm gates before its ownership and
	// marker refusals, so its teardown starts from that state.
	teardownGated vmTeardownGate = iota
	// teardownGateListed gates a VM that /cluster/ha/resources lists through
	// gateHADelete, with its motion wait, and treats any other VM as one PVE HA
	// does not manage. The create_vm rollback uses it, because a VM the create
	// never staged for HA has no state the CRM could act on.
	teardownGateListed
	// teardownGateNoWait reads the HA state once and proceeds only when the
	// state table lets a delete proceed on the node the teardown located.
	// Every other answer is a deferral, with no wait. The deferred-teardown
	// sweep uses it, because it runs ahead of the request that pays for it.
	teardownGateNoWait
)

// teardownStage names the step of the teardown protocol that ended a run.
type teardownStage string

const (
	teardownStageHA       teardownStage = "ha"
	teardownStageStop     teardownStage = "stop"
	teardownStagePreserve teardownStage = "preserve"
	teardownStageVerify   teardownStage = "verify"
	teardownStageDestroy  teardownStage = "destroy"
	teardownStageConfirm  teardownStage = "confirm"
)

// teardownResult reports how a teardown run ended. stage is the step that
// returned the error, or the step that found the VM already gone. gone is true
// when the VM was proven absent from every member before this run's destroy
// removed it, which callers treat as an idempotent success.
type teardownResult struct {
	stage teardownStage
	gone  bool
}

// vmTeardown is the one protocol every legacy VM disposal runs: delete_vm's
// synchronous path, the create_vm rollback and the middleware rollback that
// shares it, and the deferred-teardown sweep. It never deregisters the VM's
// HA resource as a step of its own. The destroy's purge removes the resource,
// its rule memberships, and its replication jobs, and the absence proof
// confirms that PVE HA no longer lists the VM. The six steps are:
//
//  1. Read the normalized HA state (see vmTeardownGate). A state that defers
//     or waits returns a retriable HA deferral, and nothing is touched.
//  2. Settle power and HA ownership. An HA-managed VM stops through
//     SetHAState, and the error-state resource the token cannot write while
//     its VM is stopped skips the HA write. Any other VM stops directly.
//  3. Preserve every volume that must outlive the VM, through the caller's
//     preserve hook, which runs the guards that path has always run.
//  4. Check on the owner node that the VM is stopped, holds no migrate lock,
//     and that the caller's ownership check still passes on both config views.
//  5. Destroy with purge=1 and the caller's lock recovery.
//  6. Await the destroy task and prove the VM absent from every member and
//     from PVE HA. A task whose answer is an error is re-read before the run
//     decides, and a VM that is still present is a retriable error.
type vmTeardown struct {
	caller string
	vmCID  string
	vmid   int
	node   string
	logger *log.Logger

	gate    vmTeardownGate
	haState *pve.HAServiceState

	// skiplock lets a destroy that a config lock refused retry once with
	// skiplock, for a root@pam password identity and never for a migrate lock.
	skiplock bool
	// awaitLockClear lets a destroy that a config lock still refuses wait,
	// bounded, for the lock to clear. Steps 3 and 4 run again before the second
	// destroy, because the lock's task may have changed the VM.
	awaitLockClear bool
	// destroyUnreferenced is the base value of destroy-unreferenced-disks,
	// which a retained ephemeral disk forces off.
	destroyUnreferenced bool

	// stop stops a VM that PVE HA does not manage and reports whether it
	// proved the VM gone. nil runs stopVMBeforeDelete.
	stop func(ctx context.Context) (gone bool, err error)
	// preserve runs step 3 and reports whether it retained the VM's ephemeral
	// disk. It must be set.
	preserve func(ctx context.Context) (retained bool, err error)
	// owned checks the VM's config views in step 4. It must be set.
	owned func(ctx context.Context, holding pve.QemuHolding) error
	// awaitTask awaits the destroy task. nil runs awaitDeleteTask.
	awaitTask func(ctx context.Context, resp *sdknodes.DeleteQemuResponse) error
}

// run executes the protocol. On success the VM is either destroyed by this
// run and proven absent, or was proven absent before the destroy (gone).
func (t *vmTeardown) run(ctx context.Context, deps Deps) (teardownResult, error) {
	if t.preserve == nil || t.owned == nil {
		return teardownResult{stage: teardownStagePreserve}, cpierrors.Cloud(
			"%s: VM %d teardown has no volume preservation or ownership check; refusing to destroy it", t.caller, t.vmid)
	}
	if err := t.settleHAGate(ctx, deps); err != nil {
		return teardownResult{stage: teardownStageHA}, err
	}
	gone, err := t.settlePower(ctx, deps)
	if err != nil {
		return teardownResult{stage: teardownStageStop}, err
	}
	if gone {
		return teardownResult{stage: teardownStageStop, gone: true}, nil
	}
	retained, err := t.preserve(ctx)
	if err != nil {
		return teardownResult{stage: teardownStagePreserve}, err
	}
	if gone, err = t.verifyBeforeDestroy(ctx, deps); err != nil || gone {
		return teardownResult{stage: teardownStageVerify, gone: gone}, err
	}
	resp, gone, err := t.destroy(ctx, deps, retained)
	if err != nil || gone {
		return teardownResult{stage: teardownStageDestroy, gone: gone}, err
	}
	if err := t.confirm(ctx, deps, resp); err != nil {
		return teardownResult{stage: teardownStageConfirm}, err
	}
	return teardownResult{stage: teardownStageConfirm}, nil
}

// settleHAGate is step 1.
func (t *vmTeardown) settleHAGate(ctx context.Context, deps Deps) error {
	switch t.gate {
	case teardownGated:
		return nil
	case teardownGateListed:
		listed, err := managedVMHAResourcePresent(ctx, deps, haResourceSid(t.vmid))
		if err != nil {
			return deferHA(cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud, fmt.Sprintf(
				"%s: VM %d: the HA resource list could not be read before the teardown", t.caller, t.vmid)))
		}
		if !listed {
			t.haState = nil
			return nil
		}
		st, err := gateHADelete(ctx, deps, t.caller, t.vmid, t.node, t.logger)
		if err != nil {
			return err
		}
		t.haState = st
		return nil
	default:
		st, err := readHADeleteState(ctx, deps, t.caller, t.vmid, t.node)
		if err != nil {
			return deferHA(cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud, fmt.Sprintf(
				"%s: VM %d: the HA state could not be read", t.caller, t.vmid)))
		}
		disp, reason := st.Disposition()
		if disp != pve.HAProceed {
			return deferHA(haDeleteRefusal(deps, t.caller, st, disp, reason, t.logger))
		}
		if st.Node != t.node {
			return deferHA(cpierrors.Retriable(
				"%s: VM %d: the HA manager places %s on node %s, not on node %s where the teardown located it",
				t.caller, t.vmid, st.SID, st.Node, t.node))
		}
		t.haState = st
		return nil
	}
}

// settlePower is step 2.
func (t *vmTeardown) settlePower(ctx context.Context, deps Deps) (bool, error) {
	direct := !t.haState.Managed()
	if !direct {
		outcome, err := stopHAManagedVMForDelete(ctx, deps, t.caller, t.haState, t.vmid, t.logger)
		if err != nil {
			return false, err
		}
		direct = outcome == haStopNotManaged
	}
	if !direct {
		return false, nil
	}
	if t.stop != nil {
		return t.stop(ctx)
	}
	return stopVMBeforeDelete(ctx, deps, t.node, t.vmid, t.vmCID, t.logger)
}

// verifyBeforeDestroy is step 4. It reports gone when the owner node no
// longer holds the VM and the absence proof holds.
func (t *vmTeardown) verifyBeforeDestroy(ctx context.Context, deps Deps) (bool, error) {
	status, err := deps.PVE.QEMU().Status(ctx, t.node, t.vmid)
	if err != nil {
		if pve.IsNotFound(err) || pve.IsPmxcfsConfigMissing(err) {
			return t.confirmGone(ctx, deps, "the power state read before the destroy")
		}
		return false, cpierrors.WrapAs(pve.WrapError(err), cpierrors.TypeRetriableCloud, fmt.Sprintf(
			"%s: read the power state of VM %d on node %s before the destroy", t.caller, t.vmid, t.node))
	}
	if state, _ := pve.ConfigString(status, "status"); state != vmPowerStateStopped {
		t.logger.Info(t.caller+": VM is not stopped yet; waiting for it to halt before the destroy",
			log.Int(metadataKeyVMID, t.vmid), log.String("node", t.node), log.String("state", state))
		if err := waitForVMStopped(ctx, deps, t.node, t.vmid, t.vmCID, t.logger); err != nil {
			return false, err
		}
	}
	holding, err := pve.ReadQemuHolding(ctx, deps.PVE, t.node, t.vmid)
	if err != nil {
		if pve.IsNotFound(err) || pve.IsPmxcfsConfigMissing(err) {
			return t.confirmGone(ctx, deps, "the config read before the destroy")
		}
		return false, cpierrors.WrapAs(pve.WrapError(err), cpierrors.TypeRetriableCloud, fmt.Sprintf(
			"%s: read the config of VM %d on node %s before the destroy", t.caller, t.vmid, t.node))
	}
	if lock, _ := pve.ConfigString(holding.Config, "lock"); strings.TrimSpace(lock) == vmLockMigrate {
		t.logger.Warn(t.caller+": VM holds a migrate lock; deferring the destroy",
			log.Int(metadataKeyVMID, t.vmid), log.String("node", t.node))
		return false, migrationLockDeferral(t.caller, t.vmid, t.node)
	}
	return false, t.owned(ctx, holding)
}

// confirmGone proves the VM absent after a step on its node answered that the
// VM is missing, and reports gone when the proof holds.
func (t *vmTeardown) confirmGone(ctx context.Context, deps Deps, step string) (bool, error) {
	if err := confirmVMGone(ctx, deps, t.caller, t.vmid, t.node, step); err != nil {
		return false, err
	}
	t.logger.Info(t.caller+": VM is absent from every member; nothing left to destroy",
		log.Int(metadataKeyVMID, t.vmid), log.String("node", t.node), log.String("step", step))
	return true, nil
}

// destroy is step 5. It returns the destroy's task response, or gone when the
// destroy found the VM already absent from every member. It works the two
// recoverable refusals PVE answers with:
//
//   - "VM is running - destroy failed": a stop accepted while the VM was still
//     HA-managed can end its task before the LRM halts the guest, so the
//     destroy waits until the VM reports stopped and runs once more.
//   - A config lock left by a killed worker or a node reboot mid-clone: the
//     fast path retries with skiplock when the CPI authenticates as root@pam,
//     and a teardown that awaits the lock waits for it to clear and runs
//     steps 3 and 4 again first. A migrate lock is never skipped.
//
// Purge removes the VMID from the backup, HA, and replication configs.
// Resource-pool membership is not documented as part of what purge removes, so
// nothing depends on the pool member list shrinking right after the destroy.
//
// DestroyUnreferencedDisks is pve.destroy_unreferenced_disks AND-ed with
// !retained. On the retain path the ephemeral volume already belongs to a
// parker, so the forced false is a second guard; see the field doc on
// config.CPIConfig for the shared-storage data-loss hazard the knob carries.
func (t *vmTeardown) destroy(
	ctx context.Context, deps Deps, retained bool,
) (*sdknodes.DeleteQemuResponse, bool, error) {
	purge := true
	destroyDisks := t.destroyUnreferenced && !retained
	t.logger.Debug(t.caller+": destroying VM", log.Int(metadataKeyVMID, t.vmid), log.String("node", t.node))
	resp, err := destroyQemuWithLockCurve(ctx, deps, t.caller, t.node, t.vmid, purge, destroyDisks, t.logger)
	if err != nil && pve.IsVMRunningDestroyFailure(err) {
		t.logger.Info(t.caller+": VM still running at the destroy; waiting for it to halt before the retry",
			log.Int(metadataKeyVMID, t.vmid))
		if waitErr := waitForVMStopped(ctx, deps, t.node, t.vmid, t.vmCID, t.logger); waitErr != nil {
			return nil, false, waitErr
		}
		resp, err = destroyQemuWithLockCurve(ctx, deps, t.caller, t.node, t.vmid, purge, destroyDisks, t.logger)
	}
	var migrating *skiplockMigrationRefusal
	if err != nil && pve.IsVMConfigLocked(err) && t.skiplock {
		resp, err = retryDestroyWithSkiplock(ctx, deps, t.node, t.vmid, purge, destroyDisks, err, t.logger)
	}
	if err != nil && pve.IsVMConfigLocked(err) && t.awaitLockClear && !errors.As(err, &migrating) {
		recheck := func(ctx context.Context) (bool, error) { return t.recheck(ctx, deps) }
		resp, err = retryDestroyAfterLockClear(ctx, deps, t.node, t.vmid, destroyDisks, err, recheck, t.logger)
	}
	if err == nil {
		return resp, false, nil
	}
	switch {
	case pve.IsNotFound(err) || pve.IsPmxcfsConfigMissing(err):
		gone, goneErr := t.confirmGone(ctx, deps, "the destroy")
		return nil, gone, goneErr
	case errors.As(err, &migrating):
		return nil, false, migrating.deferral(t.caller)
	case pve.IsVMConfigLocked(err):
		logUnresolvedVMLock(t.logger, t.caller+": destroy failed", t.vmid, t.node, err)
		return nil, false, pve.WrapVMConfigLocked(err, t.vmid, t.node)
	}
	var cpiErr *cpierrors.Error
	if errors.As(err, &cpiErr) {
		return nil, false, err
	}
	return nil, false, cpierrors.Wrap(pve.WrapError(err), fmt.Sprintf("%s: delete VM %s", t.caller, t.vmCID))
}

// recheck runs steps 3 and 4 again ahead of the destroy that follows a lock
// clear, because the task that held the lock may have attached a volume or
// started the VM. It reports gone when the VM left meanwhile.
func (t *vmTeardown) recheck(ctx context.Context, deps Deps) (bool, error) {
	if _, err := t.preserve(ctx); err != nil {
		return false, err
	}
	return t.verifyBeforeDestroy(ctx, deps)
}

// confirm is step 6. The task's answer alone never decides: the VM's absence
// from every member and from PVE HA does. A VM proven absent finishes the run
// even when the await answered an error, and a VM that is still present, or
// whose absence cannot be proven, is a retriable error.
func (t *vmTeardown) confirm(ctx context.Context, deps Deps, resp *sdknodes.DeleteQemuResponse) error {
	var awaitErr error
	if t.awaitTask != nil {
		awaitErr = t.awaitTask(ctx, resp)
	} else {
		awaitErr = awaitDeleteTask(ctx, deps, t.node, t.vmCID, t.vmid, resp, t.logger)
	}
	loc, err := pollManagedVMDestroyAbsence(ctx, managedVMDestroyCRMWait, managedVMDestroyCRMInterval,
		func() (pve.VMLocation, error) {
			return locateAfterConfigMissing(ctx, deps, t.caller, t.vmid, t.node)
		})
	switch {
	case err != nil && awaitErr != nil:
		return errors.Join(awaitErr, cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud, fmt.Sprintf(
			"%s: VM %d: the absence proof after the destroy did not hold", t.caller, t.vmid)))
	case err != nil:
		return cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud, fmt.Sprintf(
			"%s: VM %d: the destroy finished, but the VM's absence from the cluster and from PVE HA could not be "+
				"proven; retry so the next attempt proves it", t.caller, t.vmid))
	case loc.Found && awaitErr != nil:
		return awaitErr
	case loc.Found:
		return cpierrors.Retriable(
			"%s: VM %d is still present on node %s after its destroy; retry so the next attempt destroys it",
			t.caller, t.vmid, loc.Node)
	}
	if awaitErr != nil {
		t.logger.Warn(t.caller+": the destroy task answered an error, but the VM is proven absent; the destroy is done",
			log.Int(metadataKeyVMID, t.vmid), log.String("node", t.node), log.Err(awaitErr))
	}
	return nil
}

// destroyQemuWithLockCurve sends one purge destroy through
// RetryOnTransientOrLock with the rollback destroy's small budget. A config
// lock the loop does not wait out goes straight back to the caller's lock
// recovery (see lockForRecovery), and an already-gone answer is short-circuited
// so the transient rule cannot spend the budget on a VM that no longer exists.
func destroyQemuWithLockCurve(
	ctx context.Context, deps Deps, caller, node string, vmid int, purge, destroyDisks bool, logger *log.Logger,
) (*sdknodes.DeleteQemuResponse, error) {
	var resp *sdknodes.DeleteQemuResponse
	var lastErr error
	err := pve.RetryOnTransientOrLock(ctx, logger, caller+".destroy", rollbackDestroyMaxAttempts, func() error {
		resp, lastErr = deps.PVE.Nodes().DeleteQemu(ctx, node, vmcid.PathOf(vmid), &sdknodes.DeleteQemuParams{
			Purge:                    &purge,
			DestroyUnreferencedDisks: &destroyDisks,
		})
		if lastErr != nil && (pve.IsNotFound(lastErr) || pve.IsPmxcfsConfigMissing(lastErr) || lockForRecovery(lastErr)) {
			return nil
		}
		return lastErr
	})
	if lastErr != nil {
		return nil, lastErr
	}
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// requireStoppedOnOwner reads the VM's power state on its owner node and
// returns a retriable error unless the node reports it stopped. The journaled
// managed delete runs it ahead of its destroy submission, as step 4 of the
// teardown protocol.
func requireStoppedOnOwner(ctx context.Context, deps Deps, caller, node string, vmid int) error {
	stopped, err := haOwnerNodeReportsStopped(ctx, deps, caller, node, vmid)
	if err != nil {
		return err
	}
	if !stopped {
		return cpierrors.Retriable(
			"%s: VM %d on node %s is not stopped ahead of its destroy; retry once it has halted", caller, vmid, node)
	}
	return nil
}
