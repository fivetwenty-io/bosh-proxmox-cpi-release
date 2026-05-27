// Package handlers contains the 22 BOSH CPI v2 method implementations.
package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
	sdknodes "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/nodes"
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
//  5. If mode == "hard": call hardReset() directly.
//  6. If mode == "soft": POST /status/reboot with configured timeout.
//     - 404 on reboot call → VMNotFound (no fallback).
//     - Other reboot-call error → log warn + fallback to hardReset().
//     - Reboot UPID await failure → log warn + fallback to hardReset().
//     - Empty UPID (synchronous) → success without await.
//
// Config knobs: reboot_mode ("soft"|"hard", default "soft"),
// reboot_timeout (seconds, default 60).
func HandleRebootVM(deps Deps) cpi.Handler {
	return cpi.HandlerFunc(func(ctx context.Context, args []json.RawMessage, _ jsonrpc.Context) (any, error) {
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

		logger := deps.Logger.With(
			log.String("method", "reboot_vm"),
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

		// --- hardReset closure — captures ctx/node/vmid/vmCID/logger/deps ---
		// Maps to POST /nodes/{node}/qemu/{vmid}/status/reset (immediate power cycle).
		// All inputs: ctx (from outer closure), node/vmid/vmCID (validated above),
		//             logger (structured), deps.PVE (non-nil by construction).
		// Failure modes:
		//   - Reset returns 404 → VMNotFound (non-retriable, not retried).
		//   - Reset returns transient error → retried by RetryOnTransient.
		//   - Reset returns other non-transient error → CloudError via WrapError.
		//   - AwaitTaskWithLogger returns task failure → wrapped CloudError.
		//   - Empty UPID → success (synchronous reset, no task to await).
		hardReset := func() (any, error) {
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
		// Inputs: ctx, node, vmid (validated). Start returns (upid, err).
		// Failure modes:
		//   - 404 → VMNotFound.
		//   - other → CloudError via WrapError.
		//   - await fails → wrapped CloudError.
		//   - empty UPID → synchronous start, no await needed.
		if state == "stopped" {
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
				return nil, cpierrors.Wrap(pve.WrapError(startErr), fmt.Sprintf("reboot_vm: start stopped VM %s", vmCID))
			}
			if startUPID != "" {
				if awaitErr := pve.AwaitTaskWithLogger(ctx, deps.PVE, node, startUPID, logger); awaitErr != nil {
					return nil, cpierrors.Wrap(pve.WrapError(awaitErr), fmt.Sprintf("reboot_vm: await start task for VM %s", vmCID))
				}
			}
			logger.Info("reboot_vm: stopped VM started")
			return nil, nil
		}

		// --- running VM: hard or soft reboot ---
		if mode == "hard" {
			return hardReset()
		}

		// mode == "soft": POST /status/reboot with timeout, fall back to hard on failure.
		// Inputs: ctx, node, vmCID (string form for SDK), timeout (validated by config).
		// Failure modes:
		//   - 404 → VMNotFound (not a fallback candidate — VM is gone).
		//   - other reboot-call error → log warn + hardReset() fallback.
		//   - UPIDFromRaw parse error → log warn + hardReset() fallback.
		//   - await task failure → log warn + hardReset() fallback.
		//   - empty UPID → success without await.
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
			return hardReset()
		}

		var rawMsg json.RawMessage
		if resp != nil {
			rawMsg = json.RawMessage(*resp)
		}
		upid, upidErr := pve.UPIDFromRaw(rawMsg)
		if upidErr != nil {
			logger.Warn("reboot_vm: could not parse UPID from reboot response, falling back to hard reset", log.Err(upidErr))
			return hardReset()
		}

		if upid != "" {
			if awaitErr := pve.AwaitTaskWithLogger(ctx, deps.PVE, node, upid, logger); awaitErr != nil {
				logger.Warn("reboot_vm: graceful reboot task failed, falling back to hard reset", log.Err(awaitErr))
				return hardReset()
			}
		}

		logger.Info("reboot_vm: graceful reboot completed")
		return nil, nil
	})
}
