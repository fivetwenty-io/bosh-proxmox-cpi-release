package handlers

import (
	"context"
	"encoding/json"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// HandleHasVM returns a handler for the has_vm CPI method.
//
// Arguments:
//   - args[0]: vm_cid (string) — a VM CID (a bare VMID or a stamped CID).
//
// Logic:
//  1. Parse vm_cid → vmid int.
//  2. Locate VM via cluster scan (FindVMNodeViaCluster → /cluster/resources).
//  3. Not found in cluster → return false.
//  4. Transport error → propagate as CPI error (caller may retry).
//  5. Found → check ownership (see ownVM). A foreign VM returns false, or
//     true with an error log under vm_ownership_guard: warn; a token
//     mismatch returns false.
//  6. Owned → return true.
//
// Using the cluster scan rather than Config(node, vmid) means the result is
// correct after an HA failover: the VM may have migrated to a different node
// since the CPI was configured.
//
// Returns bool.
func HandleHasVM(deps Deps) cpi.Handler {
	return cpi.HandlerFunc(func(ctx context.Context, args []json.RawMessage, reqCtx jsonrpc.Context) (any, error) {
		deps, err := deps.WithRequestOverrides(ctx, reqCtx)
		if err != nil {
			return nil, err
		}
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

		cid, err := parseVMCIDArg("has_vm", vmCID)
		if err != nil {
			return nil, err
		}
		vmid := cid.VMID

		logger := deps.Log(ctx).With(log.String("vm_cid", vmCID), log.Int("vmid", vmid))

		// --- locate VM authoritatively ---
		// A cluster-scan hit is authoritative even after an HA failover, but a
		// miss is not: the /cluster/resources index lags by minutes on loaded
		// clusters, and a false "no" here makes the Director treat a live VM
		// as gone. FindVMAuthoritative proves absence on a miss with per-node
		// config probes; a probe failure surfaces retriable instead of a
		// false no. Transport error → propagate; caller may retry.
		logger.Debug("has_vm: locating VM (cluster scan, per-node probes on miss)")
		loc, lookupErr := pve.FindVMAuthoritative(ctx, deps.PVE, vmid)
		if lookupErr != nil {
			return nil, cpierrors.Wrap(lookupErr, "has_vm: locate VM")
		}
		if !loc.Found || loc.Node == "" {
			logger.Debug("has_vm: VM absent from cluster index and every node's config probe — returning false")
			return false, nil
		}

		// --- ownership guard ---
		// A VM this Director does not own is not the VM the CID names, so the
		// enforce answer is false. warn keeps today's true and logs the same
		// error, for an operator who must see the answer before trusting it.
		verdict, ownErr := checkVMOwnership(ctx, deps, "has_vm", cid, loc, nil)
		if ownErr != nil {
			return nil, ownErr
		}
		if verdict.Absent {
			logger.Debug("has_vm: VM config gone from its node and every node's config probe — returning false")
			return false, nil
		}
		if verdict.Gone {
			logger.Info("has_vm: the VMID now holds a different VM than the CID names — returning false",
				log.String("ownership_reason", verdict.Reason), log.String("ownership_detail", verdict.Detail))
			return false, nil
		}
		if verdict.foreign() {
			if vmOwnershipGuardWarns(deps) {
				logForeignVM(logger, "has_vm: VM is not owned by this Director; vm_ownership_guard is warn, so returning true", verdict)
				return true, nil
			}
			logForeignVM(logger, "has_vm: VM is not owned by this Director — returning false", verdict)
			return false, nil
		}

		logger.Debug("has_vm: VM found — returning true", log.String("node", loc.Node))
		return true, nil
	})
}
