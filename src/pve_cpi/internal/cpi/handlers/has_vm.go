// Package handlers contains the 22 BOSH CPI v2 method implementations.
package handlers

import (
	"context"
	"encoding/json"
	"strconv"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
)

// HandleHasVM returns a handler for the has_vm CPI method.
//
// Arguments:
//   - args[0]: vm_cid (string) — integer VMID as a string.
//
// Logic:
//  1. Parse vm_cid → vmid int.
//  2. Call qemu.Config(ctx, node, vmid).
//  3. 404 response → return false.
//  4. Any other SDK error → propagate as CPI error.
//  5. Success → return true.
//
// Returns bool.
func HandleHasVM(deps Deps) cpi.Handler {
	return cpi.HandlerFunc(func(ctx context.Context, args []json.RawMessage, _ jsonrpc.Context) (any, error) {
		// --- argument extraction ---
		if len(args) < 1 {
			return nil, cpierrors.Cloud("has_vm: missing required argument vm_cid")
		}
		var vmCID string
		if err := json.Unmarshal(args[0], &vmCID); err != nil {
			return nil, cpierrors.Cloud("has_vm: vm_cid must be a string: %s", err.Error())
		}
		if vmCID == "" {
			return nil, cpierrors.Cloud("has_vm: vm_cid must not be empty")
		}

		vmid, err := strconv.Atoi(vmCID)
		if err != nil {
			return nil, cpierrors.Cloud("has_vm: vm_cid %q is not a valid integer VMID: %s", vmCID, err.Error())
		}
		if vmid <= 0 {
			return nil, cpierrors.Cloud("has_vm: vm_cid %q must be a positive integer", vmCID)
		}

		node := deps.Config.Node
		logger := deps.Logger.With(log.String("method", "has_vm"), log.String("vm_cid", vmCID), log.Int("vmid", vmid))

		// --- existence check via Config ---
		logger.Debug("has_vm: fetching VM config")
		_, configErr := deps.PVE.QEMU().Config(ctx, node, vmid)
		if configErr != nil {
			if pve.IsNotFound(configErr) {
				logger.Debug("has_vm: VM not found — returning false")
				return false, nil
			}
			// Non-404 error: propagate to caller.
			return nil, cpierrors.Wrap(pve.WrapError(configErr), "has_vm: config fetch failed")
		}

		logger.Debug("has_vm: VM found — returning true")
		return true, nil
	})
}
