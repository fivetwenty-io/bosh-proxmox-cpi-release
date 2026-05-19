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
)

// HandleRebootVM returns a handler for the reboot_vm CPI method.
//
// Arguments:
//   - args[0]: vm_cid (string) — integer VMID as a string.
//
// Logic:
//  1. Parse vm_cid → vmid int.
//  2. Call qemu.Reset(ctx, node, vmid) — hard reset (POST /status/reset).
//  3. 404 → return VMNotFound error.
//  4. Other error → propagate.
//  5. Await task UPID.
//
// Uses hard reset (qemu.Reset) not graceful ACPI reboot, matching the Perl reference.
// Returns nil result on success.
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

		node := deps.Config.Node
		logger := deps.Logger.With(log.String("method", "reboot_vm"), log.String("vm_cid", vmCID), log.Int("vmid", vmid))

		// --- hard reset ---
		// qemu.Reset maps to POST /nodes/{node}/qemu/{vmid}/status/reset.
		// This is a hard reset (power cycle), NOT a graceful ACPI shutdown+start.
		logger.Debug("reboot_vm: issuing hard reset")
		upid, resetErr := deps.PVE.QEMU().Reset(ctx, node, vmid)
		if resetErr != nil {
			if pve.IsNotFound(resetErr) {
				return nil, cpierrors.VMNotFound(vmCID)
			}
			return nil, cpierrors.Wrap(pve.WrapError(resetErr), fmt.Sprintf("reboot_vm: reset VM %s", vmCID))
		}

		// --- await task ---
		if upid != "" {
			if awaitErr := pve.AwaitTaskWithLogger(ctx, deps.PVE, node, upid, logger); awaitErr != nil {
				return nil, cpierrors.Wrap(awaitErr, fmt.Sprintf("reboot_vm: await reset task for VM %s", vmCID))
			}
		}

		logger.Info("reboot_vm: VM hard-reset completed")
		return nil, nil
	})
}
