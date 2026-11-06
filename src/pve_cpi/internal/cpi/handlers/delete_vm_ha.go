package handlers

import (
	"context"
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

// gateHADelete applies the delete's HA state table to the VM on node before
// anything stops, detaches, or destroys it. It returns the state the delete
// proceeds with, which is known and names node. Every other answer is a
// retriable error with no time limit of its own:
//
//   - Proceed returns the state. A CRM that places the service on another node
//     than the one the delete located is a retriable error, so the retry
//     locates the VM again.
//   - Wait for motion waits through AwaitVMSettled and reads again, until the
//     table answers something else or pve.ha.motion_wait_sec runs out, which
//     is a retriable error that names the raw state.
//   - The grace window runs for a service in recovery on a fenced node. It is
//     measured from the CPI's first observation of the service ID and uid
//     pair, and it defers both while it runs and after it expires, because
//     completing a delete on a dead node is a separate path. The expiry is
//     logged and the error names the operator's procedure.
//   - Defer and unknown are retriable errors that say what the CPI waits for.
func gateHADelete(
	ctx context.Context, deps Deps, vmid int, node string, logger *log.Logger,
) (*pve.HAServiceState, error) {
	const caller = "delete_vm"
	wait := haMotionWait(deps)
	deadline := time.Now().Add(wait)
	for {
		st, err := readHADeleteState(ctx, deps, caller, vmid, node)
		if err != nil {
			return nil, err
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
				return nil, cpierrors.Retriable(
					"%s: VM %d: %s, and the HA motion did not settle within the %s motion budget (raw HA state %q); "+
						"retry once the HA manager has finished", caller, vmid, reason, wait, st.Raw)
			}
			logger.Info(caller+": waiting for HA motion to settle before the delete",
				log.String("sid", st.SID), log.String("raw_state", st.Raw), log.String("requested", st.Requested),
				log.String("reason", reason), log.String("remaining", remaining.Round(time.Second).String()))
			settled, settleErr := pve.AwaitVMSettled(ctx, deps.PVE, logger, pve.VMSettleRequest{
				VMID: vmid, MotionWait: remaining, Label: caller,
			})
			if settleErr != nil {
				return nil, cpierrors.WrapAs(settleErr, cpierrors.TypeRetriableCloud, fmt.Sprintf(
					"%s: VM %d is in HA motion (raw HA state %q) and did not settle", caller, vmid, st.Raw))
			}
			if !settled.Found || settled.Node != node {
				return nil, cpierrors.Retriable(
					"%s: VM %d moved while the delete waited for HA motion (located on %q, now %q); "+
						"retry so the delete locates it again", caller, vmid, node, settled.Node)
			}
			if err := sleepOrDone(ctx, haDeleteMotionPoll()); err != nil {
				return nil, cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud, fmt.Sprintf(
					"%s: VM %d: the wait for HA motion was cancelled", caller, vmid))
			}

		case pve.HAGraceWindow:
			return nil, haRecoveryGraceError(deps, caller, st, reason, logger)

		case pve.HADefer:
			logHADeleteState(logger, caller, st, disp, reason)
			return nil, cpierrors.Retriable(
				"%s: VM %d deferred: %s. The CPI waits until PVE HA can prove the VM's state; resolve the fence "+
					"or bring node %s back online, and the next retry proceeds", caller, vmid, reason, st.Node)

		default:
			logHADeleteState(logger, caller, st, disp, reason)
			return nil, cpierrors.Retriable(
				"%s: VM %d deferred because its HA state is unknown: %s. The CPI waits for a fresh HA view from "+
					"one quorate entry node; check 'ha-manager status' and the nodes, and the next retry proceeds",
				caller, vmid, reason)
		}
	}
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
// Once it expires the expiry is logged, and the delete keeps deferring, with
// the operator's procedure for a VM stranded on a dead node, because this
// path does not complete the delete itself.
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
			"%s: VM %d deferred: %s. PVE HA is still trying to recover it, and %s of the %s grace window remain "+
				"before the CPI hands the VM back to BOSH", caller, st.VMID, reason,
			(window - elapsed).Round(time.Second), window)
	}
	logger.Warn(caller+": the HA recovery grace window expired", fields...)
	return cpierrors.Retriable(
		"%s: VM %d deferred: %s, and the %s grace window expired %s ago without PVE HA recovering it. "+
			"Confirm that node %s is powered off, move the VM's config to a live node with "+
			"'mv /etc/pve/nodes/%s/qemu-server/%d.conf /etc/pve/nodes/<live node>/qemu-server/', and retry",
		caller, st.VMID, reason, window, (elapsed - window).Round(time.Second), st.Node, st.Node, st.VMID)
}

// haDeleteSysConsole reports whether the token holds Sys.Console on /, which
// SetHAState needs to write the HA resource's requested state and to clear an
// error. The delete reads it directly because the HA profile probes nothing in
// mode off, and an HA-managed VM is stopped through PVE HA in every mode. A
// client that cannot read permissions, and a read that fails, answer false,
// which fails closed: the stop then uses the direct call, and an errored
// resource that still runs gets a retriable error naming the command.
func haDeleteSysConsole(ctx context.Context, deps Deps, logger *log.Logger) bool {
	reader, ok := deps.PVE.(pve.TokenPermissionReader)
	if !ok {
		logger.Debug("delete_vm: the client cannot read token permissions; treating Sys.Console as missing")
		return false
	}
	perms, err := reader.EffectivePermissions(ctx, "/")
	if err != nil {
		logger.Warn("delete_vm: token permissions on / are unreadable; treating Sys.Console as missing", log.Err(err))
		return false
	}
	// A privilege is held when its key is present; the value only says
	// whether it propagates to children of the path.
	_, held := perms["Sys.Console"]
	return held
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
// configured, and the destroy that follows removes it with purge.
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
	if st.InCRM && !st.Configured {
		return 0, cpierrors.Retriable(
			"%s: VM %d: the HA manager still holds %s although no HA resource is configured for it; the CRM drops "+
				"it on its next round, and the next retry proceeds", caller, vmid, st.SID)
	}
	sysConsole := haDeleteSysConsole(ctx, deps, logger)
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
	logger.Info(caller+": stopping the HA-managed VM through PVE HA",
		log.String("sid", st.SID), log.String("raw_state", st.Raw), log.String("requested", st.Requested),
		log.String("ha_node", st.Node), log.Bool("sys_console", sysConsole))
	res, err := pve.SetHAState(ctx, pve.HAStateDepsFor(deps.PVE), pve.SetHAStateInput{
		VMID:          vmid,
		Node:          st.Node,
		Target:        pve.HATargetStopped,
		Caller:        pve.HACallerTeardown,
		SysConsole:    sysConsole,
		Budget:        haMotionWait(deps),
		HealthOptions: haHealthOptions(deps),
	})
	if err != nil {
		return 0, cpierrors.Wrap(err, fmt.Sprintf("%s: stop HA-managed VM %d through PVE HA", caller, vmid))
	}
	switch {
	case res == nil:
		return 0, cpierrors.Retriable("%s: stop HA-managed VM %d: PVE HA returned no result", caller, vmid)
	case res.Outcome == pve.HAStateReached:
		if res.Node != "" && res.Node != st.Node {
			return 0, cpierrors.Retriable(
				"%s: VM %d stopped on node %s, not on node %s where the delete located it; retry so the delete "+
					"runs against the node that holds it", caller, vmid, res.Node, st.Node)
		}
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

// logHasVMHAState records the HA state of a VM has_vm found, so the log says
// why a VM on a dead or fenced node still answers true. It never changes the
// answer: has_vm answers false only through the absence proof. The read is
// best-effort, and a failure is logged.
func logHasVMHAState(ctx context.Context, deps Deps, loc pve.VMLocation, vmid int, logger *log.Logger) {
	if loc.HAState == "" {
		return
	}
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
