package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/vmcid"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

// HandleRebootVM returns a handler for the reboot_vm CPI method.
//
// Arguments:
//   - args[0]: vm_cid (string) — a VM CID (a bare VMID or a stamped CID).
//
// Logic:
//  1. Parse vm_cid → vmid int.
//  2. Locate VM via cluster scan (FindVMNodeViaCluster) to get authoritative node.
//     - Not found → VMNotFound.
//     - Transport error → CloudError.
//     2a. Wait for the VM to settle (AwaitVMSettled) and take the node that wait
//     returns, so a migration or an HA state change in flight is waited out and
//     followed to the VM's new node. A node the cluster does not report able to
//     answer, or an HA state that defers, is a retriable error.
//  3. GET /qemu/{vmid}/status/current — determine running state.
//     - 404 or config missing → the VM is proven gone (VMNotFound), or it moved
//     (retriable); other err → CloudError.
//     3a. An HA-managed VM is stopped and then started through SetHAState as a
//     caller that keeps the VM, in every HA mode, and each request is polled
//     until the VM reaches its state. A running VM whose resource is in error
//     is never sent disabled; it returns the re-registration error.
//  4. If state == "stopped": issue qemu.Start; await UPID; return nil.
//  5. If mode == "hard": call rebootVMHardReset() directly.
//  6. If mode == "soft": POST /status/reboot with configured timeout.
//     - 404 on reboot call → VMNotFound (no fallback).
//     - Other reboot-call error → log warn + fallback to rebootVMHardReset().
//     - Reboot UPID await failure → log warn + fallback to rebootVMHardReset().
//     - Empty UPID (synchronous) → success without await.
//
// Config knobs: reboot_mode ("soft"|"hard", default "soft"),
// reboot_timeout (seconds, default 60).
func HandleRebootVM(deps Deps) cpi.Handler {
	return cpi.HandlerFunc(func(ctx context.Context, args []json.RawMessage, reqCtx jsonrpc.Context) (any, error) {
		deps, err := deps.WithRequestOverrides(ctx, reqCtx)
		if err != nil {
			return nil, err
		}
		// --- argument extraction ---
		if len(args) < 1 {
			return nil, cpierrors.Cloud("reboot_vm: missing required argument vm_cid")
		}
		var vmCID string
		if err := json.Unmarshal(args[0], &vmCID); err != nil {
			return nil, cpierrors.Cloud("reboot_vm: vm_cid must be a string: %s", err.Error())
		}
		if vmCID == "" {
			return nil, cpierrors.Cloud("reboot_vm: vm_cid must not be empty")
		}

		cid, err := parseVMCIDArg("reboot_vm", vmCID)
		if err != nil {
			return nil, err
		}
		vmid := cid.VMID

		logger := deps.Log(ctx).With(
			log.String("vm_cid", vmCID),
			log.Int("vmid", vmid),
		)

		// --- locate VM authoritatively ---
		// A cluster-scan hit is correct even after an HA failover; a miss is
		// proven by per-node config probes before VMNotFound may be returned,
		// since the /cluster/resources index lags and a young VM can be
		// invisible there. Probe failure → retriable. Transport error →
		// propagate.
		logger.Debug("reboot_vm: locating VM (cluster scan, per-node probes on miss)")
		loc, lookupErr := findVMAuthoritative(ctx, deps, vmid)
		if lookupErr != nil {
			return nil, cpierrors.Wrap(lookupErr, fmt.Sprintf("reboot_vm: locate VM %s", vmCID))
		}
		if !loc.Found || loc.Node == "" {
			return nil, refuseAbsentVM(ctx, deps, "reboot_vm", vmCID, cid, logger)
		}
		node := loc.Node
		logger.Debug("reboot_vm: VM located", log.String("node", node))

		// --- ownership guard: never power-cycle another Director's VM ---
		if ownErr := refuseUnownedVM(ctx, deps, "reboot_vm", vmCID, cid, loc, nil, logger); ownErr != nil {
			return nil, ownErr
		}

		// --- wait for motion to end and follow the VM to its node ---
		// The wait re-resolves the node on every poll, and it refuses a node
		// the cluster does not report able to answer, so the calls below go
		// to the node that holds the VM now.
		settled, health, settleErr := settleVMForCall(ctx, deps, "reboot_vm", vmCID, vmid, logger)
		if settleErr != nil {
			return nil, settleErr
		}
		if settled.Node != node {
			logger.Info("reboot_vm: VM moved while the reboot waited for it to settle",
				log.String("from_node", node), log.String("node", settled.Node))
		}
		node = settled.Node

		haState, haErr := readRebootHAState(ctx, deps, vmCID, vmid, node, health)
		if haErr != nil {
			return nil, haErr
		}

		mode := deps.Config.RebootModeValue()
		timeout := deps.Config.RebootTimeoutValue()

		// --- pre-check: get current VM state ---
		// Inputs: ctx, node, vmid (all validated).
		// Failure modes:
		//   - 404 → VMNotFound (non-retriable; VM is gone).
		//   - transient → retried by RetryOnTransient; on exhaustion returns Retriable.
		//   - other → non-retriable CloudError via WrapError.
		//   - status map missing "status" key → state is "" → treated as running.
		var st map[string]any
		statusErr := pve.RetryOnTransient(ctx, logger, "reboot_vm.status", 0, func() error {
			var inner error
			st, inner = deps.PVE.QEMU().Status(ctx, node, vmid)
			return inner
		})
		if statusErr != nil {
			return nil, rebootCallFailure(ctx, deps, "status", vmCID, vmid, node, statusErr)
		}
		state, _ := pve.ConfigString(st, "status")

		// --- HA-managed VM: stop and start through PVE HA ---
		// A direct reset or reboot call never changes the HA state, but a
		// stop does, and the CRM restarts a VM it still wants started. So the
		// stop and the start both go through the HA resource.
		if haState != nil && haState.Managed() {
			return rebootHAManaged(ctx, deps, logger, vmCID, vmid, haState, state)
		}

		// --- stopped VM: start instead of reboot ---
		if state == vmPowerStateStopped {
			return rebootVMHandleStopped(ctx, deps, logger, node, vmid, vmCID)
		}

		// --- running VM: hard or soft reboot ---
		if mode == "hard" {
			return rebootVMHardReset(ctx, deps, logger, node, vmid, vmCID)
		}

		return rebootVMSoftWithFallback(ctx, deps, logger, node, vmid, vmCID, timeout)
	})
}

// rebootVMHardReset issues POST /nodes/{node}/qemu/{vmid}/status/reset (immediate
// power cycle) and awaits the resulting task.
//
// Inputs: ctx, deps, logger, node, vmid, vmCID — all validated by the caller.
// Failure modes:
//   - Reset returns 404 or config missing → VMNotFound when the VM is proven
//     gone, a retriable error when it moved (see rebootCallFailure).
//   - Reset returns transient error → retried by RetryOnTransient.
//   - Reset returns other non-transient error → CloudError via WrapError.
//   - AwaitTaskWithLogger returns task failure → wrapped CloudError.
//   - Empty UPID → success (synchronous reset, no task to await).
func rebootVMHardReset(ctx context.Context, deps Deps, logger *log.Logger, node string, vmid int, vmCID string) (any, error) {
	logger.Debug("reboot_vm: issuing hard reset")
	var upid string
	resetErr := pve.RetryOnTransient(ctx, logger, "reboot_vm.hard_reset", 0, func() error {
		var inner error
		upid, inner = deps.PVE.QEMU().Reset(ctx, node, vmid)
		return inner
	})
	if resetErr != nil {
		return nil, rebootCallFailure(ctx, deps, "reset", vmCID, vmid, node, resetErr)
	}
	if upid != "" {
		if awaitErr := pve.AwaitTaskWithLogger(ctx, deps.PVE, node, upid, logger); awaitErr != nil {
			return nil, cpierrors.Wrap(pve.WrapError(awaitErr), fmt.Sprintf("reboot_vm: await reset task for VM %s", vmCID))
		}
	}
	logger.Info("reboot_vm: VM hard-reset completed")
	return nil, nil
}

// VM power states as reported by the qemu status endpoints.
const (
	vmPowerStateRunning = "running"
	vmPowerStateStopped = "stopped"
)

// vmAlreadyRunning reports whether a start rejection says the VM is already
// running — PVE's qmstart (call and task result alike) fails with
// "VM <vmid> already running". On the reboot start-a-stopped-VM path this is
// the desired end state, not a fault: the status probe can transiently read
// "stopped" right after a graceful reboot task completes (the old QEMU
// process is gone, the new one not yet registered), and the start the CPI
// then issues loses the race to the reboot's own start.
func vmAlreadyRunning(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "already running")
}

// vmRunningNow probes the VM's live status and reports whether it is running.
// It backs the replay tolerance on start paths: a start whose first attempt
// committed but whose response was dropped replays into a rejection, and the
// goal state (a running VM) is already reached no matter who reached it. A
// probe error reports false so the caller surfaces its original start error
// rather than masking it with a probe failure.
func vmRunningNow(ctx context.Context, deps Deps, logger *log.Logger, node string, vmid int) bool {
	if deps.PVE == nil || deps.PVE.QEMU() == nil {
		return false
	}
	st, err := deps.PVE.QEMU().Status(ctx, node, vmid)
	if err != nil {
		logger.Debug("status probe after failed start could not read the VM",
			log.Int(metadataKeyVMID, vmid),
			log.Err(err),
		)
		return false
	}
	state, _ := pve.ConfigString(st, "status")
	return state == vmPowerStateRunning
}

// rebootVMHandleStopped starts a stopped VM instead of rebooting it.
//
// Inputs: ctx, deps, logger, node, vmid, vmCID — all validated by the caller.
// Failure modes:
//   - 404 or config missing → VMNotFound when the VM is proven gone, a
//     retriable error when it moved (see rebootCallFailure).
//   - start rejected or start task failed with "already running" → success
//     (the VM is in the state reboot_vm exists to produce; see vmAlreadyRunning).
//   - other → CloudError via WrapError.
//   - await fails → wrapped CloudError.
//   - empty UPID → synchronous start, no await needed.
func rebootVMHandleStopped(ctx context.Context, deps Deps, logger *log.Logger, node string, vmid int, vmCID string) (any, error) {
	logger.Info("reboot_vm: VM stopped, starting")
	var startUPID string
	startErr := pve.RetryOnTransient(ctx, logger, "reboot_vm.start", 0, func() error {
		var inner error
		startUPID, inner = deps.PVE.QEMU().Start(ctx, node, vmid)
		return inner
	})
	if startErr != nil {
		if isConfigMissing(startErr) {
			return nil, rebootCallFailure(ctx, deps, "start stopped", vmCID, vmid, node, startErr)
		}
		if vmAlreadyRunning(startErr) || vmRunningNow(ctx, deps, logger, node, vmid) {
			logger.Info("reboot_vm: start raced a concurrent boot; VM already running — success")
			return nil, nil
		}
		return nil, rebootCallFailure(ctx, deps, "start stopped", vmCID, vmid, node, startErr)
	}
	if startUPID != "" {
		if awaitErr := pve.AwaitTaskWithLogger(ctx, deps.PVE, node, startUPID, logger); awaitErr != nil {
			if vmAlreadyRunning(awaitErr) || vmRunningNow(ctx, deps, logger, node, vmid) {
				logger.Info("reboot_vm: start task raced a concurrent boot; VM already running — success")
				return nil, nil
			}
			return nil, cpierrors.Wrap(pve.WrapError(awaitErr), fmt.Sprintf("reboot_vm: await start task for VM %s", vmCID))
		}
	}
	logger.Info("reboot_vm: stopped VM started")
	return nil, nil
}

// rebootVMSoftWithFallback issues a graceful ACPI reboot via
// POST /nodes/{node}/qemu/{vmCID}/status/reboot with the configured timeout.
// On any failure other than 404, falls back to rebootVMHardReset.
//
// Inputs: ctx, deps, logger, node, vmid, vmCID, timeout — all validated by the caller.
// Failure modes:
//   - 404, config missing, or a config lock on the reboot call → no fallback
//     (see rebootCallFailure): VMNotFound when the VM is proven gone, a
//     retriable error otherwise.
//   - other reboot-call error → log warn + rebootVMHardReset() fallback.
//   - UPIDFromRaw parse error → log warn + rebootVMHardReset() fallback.
//   - await task failure → log warn + rebootVMHardReset() fallback.
//   - empty UPID → success without await.
func rebootVMSoftWithFallback(ctx context.Context, deps Deps, logger *log.Logger, node string, vmid int, vmCID string, timeout int) (any, error) {
	t := int64(timeout)
	resp, rebootErr := deps.PVE.Nodes().CreateQemuStatusReboot(
		ctx, node, vmcid.PathOf(vmid),
		&sdknodes.CreateQemuStatusRebootParams{Timeout: &t},
	)
	if rebootErr != nil {
		if isConfigMissing(rebootErr) || pve.IsVMConfigLocked(rebootErr) {
			return nil, rebootCallFailure(ctx, deps, "reboot", vmCID, vmid, node, rebootErr)
		}
		logger.Warn("reboot_vm: graceful reboot call failed, falling back to hard reset", log.Err(rebootErr))
		return rebootVMHardReset(ctx, deps, logger, node, vmid, vmCID)
	}

	var rawMsg json.RawMessage
	if resp != nil {
		rawMsg = *resp
	}
	upid, upidErr := pve.UPIDFromRaw(rawMsg)
	if upidErr != nil {
		logger.Warn("reboot_vm: could not parse UPID from reboot response, falling back to hard reset", log.Err(upidErr))
		return rebootVMHardReset(ctx, deps, logger, node, vmid, vmCID)
	}

	if upid != "" {
		if awaitErr := pve.AwaitTaskWithLogger(ctx, deps.PVE, node, upid, logger); awaitErr != nil {
			logger.Warn("reboot_vm: graceful reboot task failed, falling back to hard reset", log.Err(awaitErr))
			return rebootVMHardReset(ctx, deps, logger, node, vmid, vmCID)
		}
	}

	logger.Info("reboot_vm: graceful reboot completed")
	return nil, nil
}

// isConfigMissing reports whether err says the node holds no config for the VM,
// either as a 404 or as pmxcfs's config-missing answer, which PVE sends as a 500.
func isConfigMissing(err error) bool {
	return pve.IsNotFound(err) || pve.IsPmxcfsConfigMissing(err)
}

// rebootCallFailure turns the failure of a call to the VM's node into the
// handler's answer. step names the call, for the error text.
//
// A config-missing answer speaks for the node that gave it alone, and the VM
// may have migrated since the wait, so it is VMNotFound only when the absence
// proof finds the VM nowhere. A VM found elsewhere is a retriable error, so the
// retry locates it on its new node. A config lock after the wait is a motion or
// a task that began after it, and the retry waits it out. Every other failure
// keeps the class that WrapError gives it.
func rebootCallFailure(ctx context.Context, deps Deps, step, vmCID string, vmid int, node string, err error) error {
	switch {
	case isConfigMissing(err):
		if goneErr := confirmVMGone(ctx, deps, "reboot_vm", vmid, node, step+" call"); goneErr != nil {
			return goneErr
		}
		return cpierrors.VMNotFound(vmCID)
	case pve.IsVMConfigLocked(err):
		return cpierrors.WrapAs(pve.WrapError(err), cpierrors.TypeRetriableCloud, fmt.Sprintf(
			"reboot_vm: %s VM %s: the VM is locked (%s) on node %s, so a motion or a task began after the reboot "+
				"waited for it; retry once it ends", step, vmCID, pve.VMConfigLockType(err), node))
	}
	return cpierrors.Wrap(pve.WrapError(err), fmt.Sprintf("reboot_vm: %s VM %s", step, vmCID))
}

// settleVMForCall waits until the VM is safe to act on and returns where it is
// now. It is the wait that reboot_vm and set_vm_metadata run before they touch
// the VM: AwaitVMSettled re-resolves the node on every poll, so a migration in
// flight is waited out within pve.ha.motion_wait_sec and followed to the VM's
// new node, and a service in fence or recovery is a retriable error at once.
// A VM that vanished from the index during the wait is a retriable error,
// because the wait proves nothing about absence.
//
// The node it returns must then be one the cluster reports able to answer, so
// a call about a VM on a dead or fenced node is a retriable error rather than
// a call into a node that cannot answer. A node in maintenance can answer. The
// health report that check read is returned for the caller's later decisions,
// and it is nil when the PVE client carries no cluster or nodes service to ask.
func settleVMForCall(
	ctx context.Context, deps Deps, op, vmCID string, vmid int, logger *log.Logger,
) (pve.VMLocation, *pve.NodeHealthReport, error) {
	settled, err := pve.AwaitVMSettled(ctx, deps.PVE, logger, pve.VMSettleRequest{
		VMID: vmid, MotionWait: haMotionWait(deps), Label: op,
	})
	if err != nil {
		return pve.VMLocation{}, nil, cpierrors.Wrap(err, fmt.Sprintf("%s: wait for VM %s to settle", op, vmCID))
	}
	if !settled.Found || settled.Node == "" {
		return pve.VMLocation{}, nil, cpierrors.Retriable(
			"%s: VM %s left the cluster index while the call waited for it to settle, and the wait does not prove "+
				"it is gone; retry so the call locates it again", op, vmCID)
	}
	if deps.PVE.Cluster() == nil || deps.PVE.Nodes() == nil {
		return settled, nil, nil
	}
	report, err := pve.NodeHealth(ctx, deps.PVE.Cluster(), deps.PVE.Nodes(), haHealthOptions(deps))
	if err != nil {
		return pve.VMLocation{}, nil, cpierrors.WrapAs(pve.WrapError(err), cpierrors.TypeRetriableCloud, fmt.Sprintf(
			"%s: VM %s is on node %s, and the cluster status could not be read to confirm that node can answer",
			op, vmCID, settled.Node))
	}
	if !report.ProxyTarget(settled.Node) {
		reason := "the cluster does not list it"
		if entry, ok := report.Node(settled.Node); ok && entry.Reason != "" {
			reason = entry.Reason
		}
		return pve.VMLocation{}, nil, cpierrors.Retriable(
			"%s: VM %s is on node %s, which the cluster does not report able to answer (%s); retry once it can",
			op, vmCID, settled.Node, reason)
	}
	return settled, report, nil
}

// readRebootHAState reads the VM's normalized HA state for the reboot, which
// acts on a live node. It returns nil when the PVE client carries no cluster or
// nodes service to read it with. A state that does not let the reboot proceed
// is a retriable error, and so is a CRM that places the service on another
// node than the one the reboot located, so the retry locates the VM again.
func readRebootHAState(
	ctx context.Context, deps Deps, vmCID string, vmid int, node string, health *pve.NodeHealthReport,
) (*pve.HAServiceState, error) {
	if deps.PVE.Cluster() == nil || deps.PVE.Nodes() == nil {
		return nil, nil
	}
	st, err := pve.ReadHAServiceState(ctx, deps.PVE.Cluster(), deps.PVE.Nodes(), pve.HAServiceStateInput{
		VMID: vmid, VMNode: node, Kind: pve.HADecisionLive, Health: health, HealthOptions: haHealthOptions(deps),
	})
	if err != nil {
		return nil, cpierrors.Wrap(err, fmt.Sprintf("reboot_vm: read the HA state of VM %s", vmCID))
	}
	if disp, reason := st.Disposition(); disp != pve.HAProceed {
		return nil, cpierrors.Retriable(
			"reboot_vm: VM %s deferred (HA disposition %s): %s. The reboot waits until PVE HA can prove the VM's "+
				"state or has finished moving it; check 'ha-manager status', and the next retry proceeds",
			vmCID, disp, reason)
	}
	if st.Managed() && st.InCRM && st.Node != node {
		return nil, cpierrors.Retriable(
			"reboot_vm: the HA manager places %s on node %s, but the reboot located VM %s on node %s; retry so the "+
				"reboot runs against the node that holds it", st.SID, st.Node, vmCID, node)
	}
	return st, nil
}

// rebootSysConsole reports whether the token holds Sys.Console on /, which
// SetHAState needs to write an HA resource's requested state. The answer
// decides the path of the stop and the start, so a client that cannot read the
// token's permissions, and a read that fails, return a retriable error rather
// than a guess.
func rebootSysConsole(ctx context.Context, deps Deps, vmCID string, vmid int) (bool, error) {
	reader, ok := deps.PVE.(pve.TokenPermissionReader)
	if !ok {
		return false, cpierrors.Retriable(
			"reboot_vm: VM %s (%d): the PVE client cannot read the token's permissions on /, so the reboot cannot "+
				"tell whether it may restart the HA-managed VM through its HA resource; retry", vmCID, vmid)
	}
	perms, err := reader.EffectivePermissions(ctx, "/")
	if err != nil {
		return false, cpierrors.WrapAs(pve.WrapError(err), cpierrors.TypeRetriableCloud, fmt.Sprintf(
			"reboot_vm: VM %s (%d): the token's permissions on / are unreadable, so the reboot cannot tell whether "+
				"it may restart the HA-managed VM through its HA resource; retry once they read", vmCID, vmid))
	}
	_, held := perms["Sys.Console"]
	return held, nil
}

// rebootHAManaged restarts an HA-managed VM through PVE HA, in every HA mode,
// as a caller that keeps the VM. A running VM is stopped first and then
// started, and a stopped VM is only started. Each request is polled until the
// VM reaches its state, because a direct stop or start of an HA-managed VM
// returns once the CRM has queued it, and the CRM would otherwise restart or
// move the VM against the reboot.
//
// A running VM whose resource is in error never gets disabled, which would
// stop a guest BOSH keeps. SetHAState writes nothing for it and the reboot
// returns the retriable error that names the ha-manager commands to clear the
// error without stopping the guest. A resource that is ignored, such as during
// a narrowing barrier, gets no state request either, and the reboot defers.
func rebootHAManaged(
	ctx context.Context, deps Deps, logger *log.Logger, vmCID string, vmid int, st *pve.HAServiceState, power string,
) (any, error) {
	sysConsole, err := rebootSysConsole(ctx, deps, vmCID, vmid)
	if err != nil {
		return nil, err
	}
	node := st.Node
	logger.Info("reboot_vm: restarting the HA-managed VM through PVE HA",
		log.String("sid", st.SID), log.String("raw_state", st.Raw), log.String("requested", st.Requested),
		log.String("ha_node", node), log.String("power", power), log.Bool("sys_console", sysConsole))
	if power != vmPowerStateStopped {
		stopped, stopErr := rebootSetHAState(ctx, deps, vmCID, vmid, node, pve.HATargetStopped, sysConsole)
		if stopErr != nil {
			return nil, stopErr
		}
		if stopped.Node != "" {
			node = stopped.Node
		}
	}
	if _, startErr := rebootSetHAState(ctx, deps, vmCID, vmid, node, pve.HATargetStarted, sysConsole); startErr != nil {
		return nil, startErr
	}
	logger.Info("reboot_vm: HA-managed VM restarted through PVE HA")
	return nil, nil
}

// rebootSetHAState brings the HA-managed VM to target through SetHAState and
// returns the result of a request that reached it. The refusal for an ignored
// resource is matched with errors.Is on its typed sentinel, never by its text.
func rebootSetHAState(
	ctx context.Context, deps Deps, vmCID string, vmid int, node string, target pve.HATarget, sysConsole bool,
) (*pve.SetHAStateResult, error) {
	res, err := pve.SetHAState(ctx, pve.HAStateDepsFor(deps.PVE), pve.SetHAStateInput{
		VMID:          vmid,
		Node:          node,
		Target:        target,
		Caller:        pve.HACallerKeep,
		SysConsole:    sysConsole,
		Budget:        haMotionWait(deps),
		HealthOptions: haHealthOptions(deps),
	})
	if err != nil {
		if errors.Is(err, pve.ErrHAResourceIgnored) {
			return nil, cpierrors.Wrap(err, fmt.Sprintf(
				"reboot_vm: the HA resource of VM %s is ignored, which a narrowing barrier or a staging sets, so "+
					"the reboot writes no HA state and retries once the CPI has handed the resource back to the CRM",
				vmCID))
		}
		return nil, cpierrors.Wrap(err, fmt.Sprintf("reboot_vm: bring HA-managed VM %s to %s through PVE HA", vmCID, target))
	}
	switch {
	case res == nil:
		return nil, cpierrors.Retriable("reboot_vm: bring HA-managed VM %s to %s: PVE HA returned no result", vmCID, target)
	case res.Outcome == pve.HAStateReached:
		return res, nil
	case res.Outcome == pve.HAStateNeedsReregistration:
		return nil, res.ReregistrationError()
	default:
		return nil, cpierrors.Retriable("reboot_vm: bring HA-managed VM %s to %s: PVE HA answered %q, which a reboot "+
			"cannot act on", vmCID, target, res.Outcome)
	}
}
