package handlers

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// Raw CRM states and node states the delete reads by name. The normalized model
// in pve.HAServiceState accepts only raw CRM states, so these compare against
// HAServiceState.Raw and NodeHealthEntry.CRMState.
const (
	haRawStateError = "error"
	haRawStateFence = "fence"
	haNodeFence     = "fence"
)

// haDeleteMotionPollNs is the pause between two HA reads while the delete waits
// for HA motion to settle, stored as nanoseconds in an atomic so tests can
// shrink it race-safely. The default matches the SetHAState poll cadence.
var haDeleteMotionPollNs atomic.Int64

func init() { haDeleteMotionPollNs.Store(int64(2 * time.Second)) }

func haDeleteMotionPoll() time.Duration { return time.Duration(haDeleteMotionPollNs.Load()) }

// SetHADeleteMotionPollForTest replaces the pause between two HA reads of the
// delete's motion wait and returns a restore function. Test-only.
func SetHADeleteMotionPollForTest(d time.Duration) func() {
	prev := haDeleteMotionPollNs.Swap(int64(d))
	return func() { haDeleteMotionPollNs.Store(prev) }
}

// haMotionWait is pve.ha.motion_wait_sec as a duration. It bounds the wait for
// HA motion, the SetHAState stop, and the recovery grace window.
func haMotionWait(deps Deps) time.Duration {
	if deps.Config == nil {
		return pve.DefaultMotionWait
	}
	return time.Duration(deps.Config.HAMotionWaitSecValue()) * time.Second
}

// haHealthOptions are the node health inputs of every HA read the delete makes.
func haHealthOptions(deps Deps) pve.NodeHealthOptions {
	if deps.Config == nil {
		return pve.NodeHealthOptions{}
	}
	return pve.NodeHealthOptions{MaintenanceTags: deps.Config.MaintenanceNodeTagsValue()}
}

// haEpisodes returns the store of recovery episode first observations.
func haEpisodes(deps Deps) *ha.EpisodeStore {
	if deps.HAEpisodes != nil {
		return deps.HAEpisodes
	}
	return &ha.EpisodeStore{}
}

// readHADeleteState reads the normalized HA model for a delete. The delete may
// handle a dead node, so it reads as a recovery decision: a fresh CRM view that
// shows the service in recovery on a fenced node is the evidence, and the
// failed source's stale LRM timestamp is expected there.
func readHADeleteState(ctx context.Context, deps Deps, caller string, vmid int, node string) (*pve.HAServiceState, error) {
	st, err := pve.ReadHAServiceState(ctx, deps.PVE.Cluster(), deps.PVE.Nodes(), pve.HAServiceStateInput{
		VMID: vmid, VMNode: node, Kind: pve.HADecisionRecovery, HealthOptions: haHealthOptions(deps),
	})
	if err != nil {
		return nil, cpierrors.Wrap(err, fmt.Sprintf("%s: read the HA state of VM %d", caller, vmid))
	}
	return st, nil
}

// haDeferral marks an HA answer that leaves the VM and the allocation record as
// they were: every answer of gateHADelete, and every refusal or unfinished stop
// of stopHAManagedVMForDelete. None of them writes a journal step, so a
// journal-managed delete hands the error back unchanged, and the Director
// retries it, instead of settling the record for reconciliation (see
// managedVMCleanupFailure). Its text and CPI type are those of the error it
// carries.
type haDeferral struct{ err error }

func (e *haDeferral) Error() string { return e.err.Error() }

func (e *haDeferral) Unwrap() error { return e.err }

// deferHA marks err as an HA deferral. A nil err stays nil.
func deferHA(err error) error {
	if err == nil {
		return nil
	}
	var marked *haDeferral
	if errors.As(err, &marked) {
		return err
	}
	return &haDeferral{err: err}
}

// isWholeHADeferral reports whether err is, and is only, an HA deferral.
func isWholeHADeferral(err error) bool {
	return isWholeRefusal(err, func(err error) bool {
		_, ok := err.(*haDeferral) //nolint:errorlint // isWholeRefusal unwraps one error at a time and hands each one here.
		return ok
	})
}

// The delete's context must still hold time for the work that follows each
// HA step. haDeleteStopReserve is the least time the gate's motion wait leaves
// for the HA stop, and haDeleteDestroyReserve is the least time the HA stop
// leaves for the rest of the delete: the detach, one destroy with its lock
// recovery, and the stopped-wait of a destroy that found the VM running.
const haDeleteStopReserve = 3 * time.Minute

func haDeleteDestroyReserve() time.Duration { return deleteStopWaitBudget() + time.Minute }

// haDeleteWaitLimit returns the end of an HA wait that may last budget and
// must leave reserve on ctx, and whether ctx's deadline is what set it.
func haDeleteWaitLimit(ctx context.Context, budget, reserve time.Duration) (time.Time, bool) {
	limit := time.Now().Add(budget)
	if dl, ok := ctx.Deadline(); ok && dl.Add(-reserve).Before(limit) {
		return dl.Add(-reserve), true
	}
	return limit, false
}

// gateHADelete applies the delete's HA state table to the VM on node before
// anything stops, detaches, or destroys it. It returns the state the delete
// proceeds with, which is known and names node. Every other answer is a
// retriable error with no time limit of its own, marked as an HA deferral:
//
//   - Proceed returns the state. A CRM that places the service on another node
//     than the one the delete located is a retriable error, so the retry
//     locates the VM again.
//   - Wait for motion waits through AwaitVMSettled and reads again, until the
//     table answers something else or the wait runs out, which is a retriable
//     error that names the raw state. The wait lasts pve.ha.motion_wait_sec,
//     cut short so the delete's deadline still holds haDeleteStopReserve and
//     haDeleteDestroyReserve. AwaitVMSettled applies its own caps on top: a
//     migrate or relocate gets the whole wait, while request_stop,
//     request_start, and freeze get its shorter budget for other HA states.
//     A resource the CRM has not processed yet is read again on every poll.
//   - The grace window runs for a service in recovery on a fenced node. It is
//     measured from the CPI's first observation of the service ID and uid
//     pair, and it defers both while it runs and after it expires, because
//     this CPI does not complete a delete on a fenced node. The expiry is
//     logged.
//   - Defer and unknown are retriable errors that say what the CPI waits for.
//
// caller names the operation in the log and the errors.
func gateHADelete(
	ctx context.Context, deps Deps, caller string, vmid int, node string, logger *log.Logger,
) (*pve.HAServiceState, error) {
	st, err := gateHADeleteState(ctx, deps, caller, vmid, node, logger)
	if err != nil {
		return nil, deferHA(err)
	}
	return st, nil
}

func gateHADeleteState(
	ctx context.Context, deps Deps, caller string, vmid int, node string, logger *log.Logger,
) (*pve.HAServiceState, error) {
	wait := haMotionWait(deps)
	deadline, clamped := haDeleteWaitLimit(ctx, wait, haDeleteStopReserve+haDeleteDestroyReserve())
	for {
		st, err := readHADeleteState(ctx, deps, caller, vmid, node)
		if err != nil {
			return nil, err
		}
		if ctxErr := ctx.Err(); ctxErr != nil && !st.Known {
			return nil, cpierrors.WrapAs(ctxErr, cpierrors.TypeRetriableCloud, fmt.Sprintf(
				"%s: VM %d: the delete's deadline ended the read of its HA state; retry with more time",
				caller, vmid))
		}
		disp, reason := st.Disposition()
		switch disp {
		case pve.HAProceed:
			if st.Node != node {
				return nil, cpierrors.Retriable(
					"%s: VM %d: the HA manager places %s on node %s, but the delete located the VM on node %s; "+
						"retry so the delete runs against the node that holds it", caller, vmid, st.SID, st.Node, node)
			}
			logHADeleteState(logger, caller, st, disp, reason)
			return st, nil

		case pve.HAWaitForMotion:
			remaining := time.Until(deadline)
			if remaining <= 0 {
				if clamped {
					return nil, cpierrors.Retriable(
						"%s: VM %d: %s, and the delete's deadline leaves no more time to wait for the HA motion "+
							"(raw HA state %q); retry once the HA manager has finished", caller, vmid, reason, st.Raw)
				}
				return nil, cpierrors.Retriable(
					"%s: VM %d: %s, and the HA motion did not settle within the %s motion budget (raw HA state %q); "+
						"retry once the HA manager has finished", caller, vmid, reason, wait, st.Raw)
			}
			logger.Info(caller+": waiting for HA motion to settle before the delete",
				log.String("sid", st.SID), log.String("raw_state", st.Raw), log.String("requested", st.Requested),
				log.String("reason", reason), log.String("remaining", remaining.Round(time.Second).String()))
			// The settle wait has budgets of its own, so it runs under a
			// context that ends it at the gate's deadline. When that deadline
			// ends it, the next read reports the motion that outlasted the
			// budget.
			settleCtx, cancelSettle := context.WithDeadline(ctx, deadline.Add(pve.SettleDeadlineReserve))
			settled, settleErr := pve.AwaitVMSettled(settleCtx, deps.PVE, logger, pve.VMSettleRequest{
				VMID: vmid, MotionWait: remaining, Label: caller,
			})
			budgetSpent := ctx.Err() == nil && !time.Now().Before(deadline)
			cancelSettle()
			if settleErr != nil && budgetSpent {
				continue
			}
			if settleErr != nil {
				return nil, cpierrors.WrapAs(settleErr, cpierrors.TypeRetriableCloud, fmt.Sprintf(
					"%s: VM %d is in HA motion (raw HA state %q) and did not settle", caller, vmid, st.Raw))
			}
			if !settled.Found || settled.Node != node {
				return nil, cpierrors.Retriable(
					"%s: VM %d moved while the delete waited for HA motion (located on %q, now %q); "+
						"retry so the delete locates it again", caller, vmid, node, settled.Node)
			}
			if err := sleepOrDone(ctx, min(haDeleteMotionPoll(), time.Until(deadline))); err != nil {
				return nil, cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud, fmt.Sprintf(
					"%s: VM %d: the wait for HA motion was cancelled", caller, vmid))
			}

		default:
			return nil, haDeleteRefusal(deps, caller, st, disp, reason, logger)
		}
	}
}

// haDeleteRefusal is the retriable error for an HA state that neither lets
// the delete proceed nor is motion the delete waits out.
func haDeleteRefusal(
	deps Deps, caller string, st *pve.HAServiceState, disp pve.HADisposition, reason string, logger *log.Logger,
) error {
	switch disp {
	case pve.HAGraceWindow:
		return haRecoveryGraceError(deps, caller, st, reason, logger)
	case pve.HADefer:
		logHADeleteState(logger, caller, st, disp, reason)
		return cpierrors.Retriable(
			"%s: VM %d deferred: %s. The CPI waits until PVE HA can prove the VM's state; resolve the fence "+
				"or bring node %s back online, and the next retry proceeds", caller, st.VMID, reason, st.Node)
	case pve.HAWaitForMotion:
		logHADeleteState(logger, caller, st, disp, reason)
		return cpierrors.Retriable(
			"%s: VM %d deferred: %s, and the CPI waits for the HA manager to finish; the next retry proceeds "+
				"once it has", caller, st.VMID, reason)
	default:
		logHADeleteState(logger, caller, st, disp, reason)
		return cpierrors.Retriable(
			"%s: VM %d deferred because its HA state is unknown: %s. The CPI waits for a fresh HA view from "+
				"one quorate entry node; check 'ha-manager status' and the nodes, and the next retry proceeds",
			caller, st.VMID, reason)
	}
}

// haDeferralForUnreadableConfig reads the VM's HA state once after its config
// could not be read on node, which is what a dead or fenced node answers. When
// that state does not let the delete proceed, it returns the HA deferral, so
// the error names the fence, the recovery, or the grace window rather than an
// unreadable config. It returns nil when the state lets the delete proceed,
// or cannot be read at all, and the caller returns its own error.
func haDeferralForUnreadableConfig(
	ctx context.Context, deps Deps, caller string, vmid int, node string, logger *log.Logger,
) error {
	st, err := readHADeleteState(ctx, deps, caller, vmid, node)
	if err != nil {
		return nil
	}
	disp, reason := st.Disposition()
	if disp == pve.HAProceed {
		return nil
	}
	return deferHA(haDeleteRefusal(deps, caller, st, disp, reason, logger))
}

// logHADeleteState records the HA reading a delete acted on.
func logHADeleteState(logger *log.Logger, caller string, st *pve.HAServiceState, disp pve.HADisposition, reason string) {
	fields := []log.Field{
		log.String("sid", st.SID), log.Bool("ha_managed", st.Managed()), log.String("raw_state", st.Raw),
		log.String("requested", st.Requested), log.String("ha_node", st.Node),
		log.String("disposition", string(disp)), log.String("reason", reason),
	}
	if disp == pve.HAProceed {
		logger.Debug(caller+": HA state allows the delete", fields...)
		return
	}
	logger.Warn(caller+": HA state defers the delete", fields...)
}

// haRecoveryGraceError is the deferral of a service in recovery on a fenced
// node. While the grace window runs, PVE HA keeps trying to recover the VM.
// Once it expires the expiry is logged, and the delete keeps deferring, because
// this CPI does not complete the delete of an HA-managed VM on a fenced node.
// The error says only that: PVE HA can still recover the VM onto a node its
// rules allow, and the delete proceeds once it has.
func haRecoveryGraceError(deps Deps, caller string, st *pve.HAServiceState, reason string, logger *log.Logger) error {
	window := haMotionWait(deps)
	ep, recErr := haEpisodes(deps).Observe(st.SID, st.UID)
	if recErr != nil {
		logger.Warn(caller+": the HA recovery episode record is unavailable, so the grace window starts again",
			log.String("sid", st.SID), log.String("uid", st.UID), log.Err(recErr))
	}
	elapsed := ep.Elapsed()
	fields := []log.Field{
		log.String("sid", st.SID), log.String("uid", st.UID), log.String("ha_node", st.Node),
		log.String("elapsed", elapsed.Round(time.Second).String()), log.String("window", window.String()),
	}
	if elapsed < window {
		logger.Info(caller+": VM is in HA recovery; the grace window is running", fields...)
		return cpierrors.Retriable(
			"%s: VM %d deferred: %s. PVE HA is still trying to recover it, and %s of the %s grace window remain; "+
				"the delete keeps deferring while the window runs and proceeds once PVE HA has recovered the VM "+
				"onto a live node", caller, st.VMID, reason, (window - elapsed).Round(time.Second), window)
	}
	logger.Warn(caller+": the HA recovery grace window expired", fields...)
	return cpierrors.Retriable(
		"%s: VM %d deferred: %s, and the %s grace window expired %s ago without PVE HA recovering it. This CPI "+
			"does not complete the delete of an HA-managed VM on a fenced node, so the delete keeps deferring "+
			"until PVE HA recovers the VM onto a live node that its HA rules allow; check 'ha-manager status' "+
			"for %s, and the next retry proceeds once the VM is recovered",
		caller, st.VMID, reason, window, (elapsed - window).Round(time.Second), st.SID)
}

// haDeleteSysConsole reports whether the token holds Sys.Console on /, which
// SetHAState needs to write the HA resource's requested state and to clear an
// error. The delete reads it directly because the HA profile probes nothing in
// mode off, and an HA-managed VM is stopped through PVE HA in every mode. A
// client that cannot read permissions, and a read that fails, return a
// retriable error, because the stop it chooses depends on the answer: the
// delete defers rather than guess.
func haDeleteSysConsole(ctx context.Context, deps Deps, caller string, vmid int) (bool, error) {
	reader, ok := deps.PVE.(pve.TokenPermissionReader)
	if !ok {
		return false, cpierrors.Retriable(
			"%s: VM %d: the PVE client cannot read the token's permissions on /, so the delete cannot tell "+
				"whether it may stop the HA-managed VM through its HA resource; retry", caller, vmid)
	}
	perms, err := reader.EffectivePermissions(ctx, "/")
	if err != nil {
		return false, cpierrors.WrapAs(pve.WrapError(err), cpierrors.TypeRetriableCloud, fmt.Sprintf(
			"%s: VM %d: the token's permissions on / are unreadable, so the delete cannot tell whether it may "+
				"stop the HA-managed VM through its HA resource; retry once they read", caller, vmid))
	}
	// A privilege is held when its key is present; the value only says
	// whether it propagates to children of the path.
	_, held := perms["Sys.Console"]
	return held, nil
}

// haStopOutcome is how stopHAManagedVMForDelete left the VM.
type haStopOutcome int

const (
	// haStopReached means PVE HA shows the service stopped and the owner node
	// reports the VM stopped. The destroy follows with purge.
	haStopReached haStopOutcome = iota + 1

	// haStopNotManaged means the resource is ignored and the CRM no longer
	// holds it, so the delete stops the VM directly.
	haStopNotManaged

	// haStopNoHAWrite means the resource is in error, the token lacks
	// Sys.Console, and the owner node reports the VM stopped. The delete
	// writes no HA state at all, so it neither requests a state nor purges
	// the resource on its own, and the destroy removes the resource with
	// purge.
	haStopNoHAWrite
)

// stopHAManagedVMForDelete stops an HA-managed VM through PVE HA, so the CRM
// neither restarts nor moves it before the destroy. The HA resource stays
// configured, and the destroy that follows removes it with purge. Every error
// it returns is marked as an HA deferral, because none of them leaves a
// journal step behind and the retry reads the HA state again.
//
// A resource in error that the CRM has given up on takes one of two paths
// without Sys.Console. When its owner node reports the VM stopped, the delete
// writes no HA state at all, because the destroy checks only that the VM is
// not running. When the VM runs, SetHAState returns the retriable error that
// names Sys.Console and the command 'ha-manager set vm:<vmid> --state
// disabled'.
func stopHAManagedVMForDelete(
	ctx context.Context, deps Deps, caller string, st *pve.HAServiceState, vmid int, logger *log.Logger,
) (haStopOutcome, error) {
	outcome, err := stopHAManagedVM(ctx, deps, caller, st, vmid, logger)
	return outcome, deferHA(err)
}

func stopHAManagedVM(
	ctx context.Context, deps Deps, caller string, st *pve.HAServiceState, vmid int, logger *log.Logger,
) (haStopOutcome, error) {
	if st.InCRM && !st.Configured {
		return 0, cpierrors.Retriable(
			"%s: VM %d: the HA manager still holds %s although no HA resource is configured for it; the CRM drops "+
				"it on its next round, and the next retry proceeds", caller, vmid, st.SID)
	}
	sysConsole, err := haDeleteSysConsole(ctx, deps, caller, vmid)
	if err != nil {
		logger.Warn(caller+": deferring the stop of the HA-managed VM because Sys.Console cannot be read",
			log.String("sid", st.SID), log.Err(err))
		return 0, err
	}
	if st.Raw == haRawStateError && !sysConsole {
		stopped, err := haOwnerNodeReportsStopped(ctx, deps, caller, st.Node, vmid)
		if err != nil {
			return 0, err
		}
		if stopped {
			logger.Info(caller+": HA resource is in error and the VM is stopped; without Sys.Console the delete "+
				"writes no HA state and destroys the VM with purge",
				log.String("sid", st.SID), log.String("ha_node", st.Node))
			return haStopNoHAWrite, nil
		}
	}
	budget, err := haDeleteStopBudget(ctx, deps, caller, vmid)
	if err != nil {
		return 0, err
	}
	logger.Info(caller+": stopping the HA-managed VM through PVE HA",
		log.String("sid", st.SID), log.String("raw_state", st.Raw), log.String("requested", st.Requested),
		log.String("ha_node", st.Node), log.Bool("sys_console", sysConsole),
		log.String("budget", budget.Round(time.Second).String()))
	res, err := pve.SetHAState(ctx, pve.HAStateDepsFor(deps.PVE), pve.SetHAStateInput{
		VMID:          vmid,
		Node:          st.Node,
		Target:        pve.HATargetStopped,
		Caller:        pve.HACallerTeardown,
		SysConsole:    sysConsole,
		Budget:        budget,
		HealthOptions: haHealthOptions(deps),
	})
	if err != nil {
		return 0, cpierrors.Wrap(err, fmt.Sprintf("%s: stop HA-managed VM %d through PVE HA", caller, vmid))
	}
	switch {
	case res == nil:
		return 0, cpierrors.Retriable("%s: stop HA-managed VM %d: PVE HA returned no result", caller, vmid)
	case res.Node != "" && res.Node != st.Node &&
		(res.Outcome == pve.HAStateReached || res.Outcome == pve.HAStateNotManaged):
		return 0, cpierrors.Retriable(
			"%s: VM %d is on node %s, not on node %s where the delete located it; retry so the delete "+
				"runs against the node that holds it", caller, vmid, res.Node, st.Node)
	case res.Outcome == pve.HAStateReached:
		return haStopReached, nil
	case res.Outcome == pve.HAStateNotManaged:
		logger.Info(caller+": HA resource is ignored and the CRM no longer holds it; stopping the VM directly",
			log.String("sid", st.SID))
		return haStopNotManaged, nil
	default:
		return 0, cpierrors.Retriable("%s: stop HA-managed VM %d: PVE HA answered %q, which a delete cannot act on",
			caller, vmid, res.Outcome)
	}
}

// haDeleteStopBudget is the budget of the HA stop: pve.ha.motion_wait_sec, cut
// short so the delete's deadline still holds haDeleteDestroyReserve for the
// destroy. A deadline that leaves no time is a retriable error, and nothing is
// written.
func haDeleteStopBudget(ctx context.Context, deps Deps, caller string, vmid int) (time.Duration, error) {
	limit, _ := haDeleteWaitLimit(ctx, haMotionWait(deps), haDeleteDestroyReserve())
	budget := time.Until(limit)
	if budget <= 0 {
		return 0, cpierrors.Retriable(
			"%s: VM %d: the delete's deadline leaves no time to stop the HA-managed VM through PVE HA and then "+
				"destroy it, so nothing was written; retry with more time", caller, vmid)
	}
	return budget, nil
}

// haOwnerNodeReportsStopped reads the VM's power state on its owner node.
func haOwnerNodeReportsStopped(ctx context.Context, deps Deps, caller, node string, vmid int) (bool, error) {
	status, err := deps.PVE.QEMU().Status(ctx, node, vmid)
	if err != nil {
		return false, cpierrors.WrapAs(pve.WrapError(err), cpierrors.TypeRetriableCloud, fmt.Sprintf(
			"%s: read the power state of VM %d on its owner node %s", caller, vmid, node))
	}
	state, _ := pve.ConfigString(status, "status")
	return state == vmPowerStateStopped, nil
}

// haRefusesMembershipCleanup returns the refusal of an HA membership cleanup.
// The cleanup never runs on a state it cannot read, and never while the
// service or its node is in fence: removing the resource there leaves PVE with
// a fenced node and no service to recover.
func haRefusesMembershipCleanup(st *pve.HAServiceState, vmid int) error {
	switch {
	case st == nil || !st.Known:
		return cpierrors.Retriable(
			"delete_vm: HA membership cleanup of VM %d refused because its HA state is not known", vmid)
	case st.Raw == haRawStateFence || st.NodeHealth.CRMState == haNodeFence:
		return cpierrors.Retriable(
			"delete_vm: HA membership cleanup of VM %d refused because %s is in fence on node %s; resolve the "+
				"fence, and the next retry proceeds", vmid, st.SID, st.Node)
	}
	return nil
}

// sleepOrDone waits for d or until ctx is done.
func sleepOrDone(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// hasVMHAReadTimeout bounds has_vm's best-effort HA read, because has_vm is a
// query-class call and its answer never depends on that read.
const hasVMHAReadTimeout = 15 * time.Second

// logHasVMHAState records the HA state of a VM has_vm found, so the log says
// why a VM on a dead or fenced node still answers true. It never changes the
// answer: has_vm answers false only through the absence proof. The read is
// best-effort and bounded by hasVMHAReadTimeout, and a failure is logged.
func logHasVMHAState(ctx context.Context, deps Deps, loc pve.VMLocation, vmid int, logger *log.Logger) {
	if loc.HAState == "" {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, hasVMHAReadTimeout)
	defer cancel()
	st, err := pve.ReadHAServiceState(ctx, deps.PVE.Cluster(), deps.PVE.Nodes(), pve.HAServiceStateInput{
		VMID: vmid, VMNode: loc.Node, Kind: pve.HADecisionRecovery, HealthOptions: haHealthOptions(deps),
	})
	if err != nil {
		logger.Warn("has_vm: VM is HA-managed but its HA state could not be read",
			log.String("scan_hastate", loc.HAState), log.Err(err))
		return
	}
	disp, reason := st.Disposition()
	logger.Info("has_vm: VM exists and is HA-managed",
		log.String("sid", st.SID), log.String("scan_hastate", loc.HAState), log.String("raw_state", st.Raw),
		log.String("requested", st.Requested), log.String("ha_node", st.Node), log.Bool("known", st.Known),
		log.String("disposition", string(disp)), log.String("reason", reason))
}
