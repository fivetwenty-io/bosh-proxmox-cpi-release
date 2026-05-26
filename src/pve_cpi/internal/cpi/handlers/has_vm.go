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
//  2. Locate VM via cluster scan (FindVMNodeViaCluster → /cluster/resources).
//  3. Not found in cluster → return false.
//  4. Transport error → propagate as CPI error (caller may retry).
//  5. Found → return true.
//
// Using the cluster scan rather than Config(node, vmid) means the result is
// correct after an HA failover: the VM may have migrated to a different node
// since the CPI was configured.
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

		logger := deps.Logger.With(log.String("method", "has_vm"), log.String("vm_cid", vmCID), log.Int("vmid", vmid))

		// --- locate VM via cluster scan ---
		// Queries /cluster/resources so the node returned is authoritative even
		// after an HA failover. Scan-not-found → VM absent → return false.
		// Transport error → propagate; caller may retry.
		logger.Debug("has_vm: locating VM via cluster scan")
		node, found, lookupErr := pve.FindVMNodeViaCluster(ctx, deps.PVE, vmid)
		if lookupErr != nil {
			return nil, cpierrors.Wrap(pve.WrapError(lookupErr), "has_vm: locate VM")
		}
		if !found || node == "" {
			logger.Debug("has_vm: VM not found in cluster — returning false")
			return false, nil
		}

		logger.Debug("has_vm: VM found via cluster scan — returning true", log.String("node", node))
		return true, nil
	})
}
