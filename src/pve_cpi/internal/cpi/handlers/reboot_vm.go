package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

// HandleRebootVM returns a handler for the reboot_vm CPI method.
//
// Arguments:
//   - args[0]: vm_cid (string) — integer VMID as a string.
//
// Logic:
//  1. Parse vm_cid → vmid int.
//  2. Locate VM via cluster scan (FindVMNodeViaCluster) to get authoritative node.
//     - Not found → VMNotFound.
//     - Transport error → CloudError.
//  3. GET /qemu/{vmid}/status/current — determine running state.
//     - 404 → VMNotFound; other err → CloudError.
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

		vmid, err := strconv.Atoi(vmCID)
		if err != nil {
			return nil, cpierrors.Cloud("reboot_vm: vm_cid %q is not a valid integer VMID: %s", vmCID, err.Error())
		}
		if vmid <= 0 {
			return nil, cpierrors.Cloud("reboot_vm: vm_cid %q must be a positive integer", vmCID)
		}

		logger := deps.Log(ctx).With(
			log.String("vm_cid", vmCID),
			log.Int("vmid", vmid),
		)

		// --- locate VM via cluster scan ---
		// Queries /cluster/resources for the authoritative node, correct even
		// after an HA failover. Not-found → VMNotFound. Transport error → propagate.
		logger.Debug("reboot_vm: locating VM via cluster scan")
		node, found, lookupErr := pve.FindVMNodeViaCluster(ctx, deps.PVE, vmid)
		if lookupErr != nil {
			return nil, cpierrors.Wrap(pve.WrapError(lookupErr), fmt.Sprintf("reboot_vm: locate VM %s", vmCID))
		}
		if !found || node == "" {
			return nil, cpierrors.VMNotFound(vmCID)
		}
		logger.Debug("reboot_vm: VM located", log.String("node", node))

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
			if pve.IsNotFound(statusErr) {
				return nil, cpierrors.VMNotFound(vmCID)
			}
			return nil, cpierrors.Wrap(pve.WrapError(statusErr), fmt.Sprintf("reboot_vm: status VM %s", vmCID))
		}
		state, _ := st["status"].(string)

		// --- stopped VM: start instead of reboot ---
		if state == "stopped" {
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
//   - Reset returns 404 → VMNotFound (non-retriable).
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
		if pve.IsNotFound(resetErr) {
			return nil, cpierrors.VMNotFound(vmCID)
		}
		return nil, cpierrors.Wrap(pve.WrapError(resetErr), fmt.Sprintf("reboot_vm: reset VM %s", vmCID))
	}
	if upid != "" {
		if awaitErr := pve.AwaitTaskWithLogger(ctx, deps.PVE, node, upid, logger); awaitErr != nil {
			return nil, cpierrors.Wrap(pve.WrapError(awaitErr), fmt.Sprintf("reboot_vm: await reset task for VM %s", vmCID))
		}
	}
	logger.Info("reboot_vm: VM hard-reset completed")
	return nil, nil
}

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

// rebootVMHandleStopped starts a stopped VM instead of rebooting it.
//
// Inputs: ctx, deps, logger, node, vmid, vmCID — all validated by the caller.
// Failure modes:
//   - 404 → VMNotFound.
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
		if pve.IsNotFound(startErr) {
			return nil, cpierrors.VMNotFound(vmCID)
		}
		if vmAlreadyRunning(startErr) {
			logger.Info("reboot_vm: start raced a concurrent boot; VM already running — success")
			return nil, nil
		}
		return nil, cpierrors.Wrap(pve.WrapError(startErr), fmt.Sprintf("reboot_vm: start stopped VM %s", vmCID))
	}
	if startUPID != "" {
		if awaitErr := pve.AwaitTaskWithLogger(ctx, deps.PVE, node, startUPID, logger); awaitErr != nil {
			if vmAlreadyRunning(awaitErr) {
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
//   - 404 on reboot call → VMNotFound (no fallback — VM is gone).
//   - other reboot-call error → log warn + rebootVMHardReset() fallback.
//   - UPIDFromRaw parse error → log warn + rebootVMHardReset() fallback.
//   - await task failure → log warn + rebootVMHardReset() fallback.
//   - empty UPID → success without await.
func rebootVMSoftWithFallback(ctx context.Context, deps Deps, logger *log.Logger, node string, vmid int, vmCID string, timeout int) (any, error) {
	t := int64(timeout)
	resp, rebootErr := deps.PVE.Nodes().CreateQemuStatusReboot(
		ctx, node, vmCID,
		&sdknodes.CreateQemuStatusRebootParams{Timeout: &t},
	)
	if rebootErr != nil {
		if pve.IsNotFound(rebootErr) {
			return nil, cpierrors.VMNotFound(vmCID)
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
