package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/vmcid"
)

// HandleDeleteSnapshot returns a Handler for the BOSH CPI delete_snapshot method.
//
// Arguments (positional JSON array):
//
//	[0] snapshot_cid  string -- snapshot CID of the form "<vmid>:<snap_name>"
//
// Returns: null on success (BOSH void method).
//
// Idempotency: a 404 from PVE (snapshot not found) is treated as success.
// This matches BOSH Director expectations for delete operations, which may be
// retried after partial failures.
//
// The VM node is resolved via cluster scan (FindVMNodeViaCluster) so the call
// works after an HA failover. When the VM is not found in the cluster the
// handler returns nil (idempotent: if the VM is gone the snapshot is gone).
func HandleDeleteSnapshot(deps Deps) Handler {
	return HandlerFunc(func(ctx context.Context, args []json.RawMessage, reqCtx jsonrpc.Context) (any, error) {
		deps, err := deps.WithRequestOverrides(ctx, reqCtx)
		if err != nil {
			return nil, err
		}
		// ----------------------------------------------------------------
		// 1. Unmarshal and validate arguments.
		// ----------------------------------------------------------------
		if len(args) < 1 {
			return nil, cpierrors.Cloud("delete_snapshot: expected 1 argument (snapshot_cid), got 0")
		}

		var snapshotCID string
		if err := json.Unmarshal(args[0], &snapshotCID); err != nil {
			return nil, cpierrors.Wrap(err, "delete_snapshot: args[0] snapshot_cid must be a string")
		}
		if snapshotCID == "" {
			return nil, cpierrors.Cloud("delete_snapshot: args[0] snapshot_cid must not be empty")
		}

		// ----------------------------------------------------------------
		// 2. Parse snapshot_cid -> vmCID + snapName.
		// ----------------------------------------------------------------
		vmCID, snapName, err := pve.ParseSnapshotCID(snapshotCID)
		if err != nil {
			return nil, cpierrors.Wrap(err, "delete_snapshot: invalid snapshot_cid "+snapshotCID)
		}

		parsedCID, parseErr := vmcid.Parse(vmCID)
		if parseErr != nil {
			return nil, cpierrors.VMNotFound(vmCID)
		}
		vmid := parsedCID.VMID

		// ----------------------------------------------------------------
		// 3. Locate VM authoritatively. The idempotent-success branch below
		//    concludes "snapshot necessarily absent" from the VM's absence,
		//    so absence must be proven (per-node config probes on an index
		//    miss), never inferred from the lagging /cluster/resources index.
		// ----------------------------------------------------------------
		// The wait comes first: a VM mid-migration answers a snapshot delete
		// with a lock, and a node the cluster reports down answers nothing. A
		// VM the wait does not find is left to the lookup below, whose absence
		// proof is the only thing that turns a missing VM into success.
		if _, settleErr := settleDiskVM(ctx, deps, "delete_snapshot", vmCID, vmid, true); settleErr != nil {
			return nil, settleErr
		}
		loc, lookupErr := findVMAuthoritative(ctx, deps, vmid)
		if lookupErr != nil {
			return nil, cpierrors.Wrap(lookupErr,
				fmt.Sprintf("delete_snapshot: locate VM %s", vmCID))
		}
		if !loc.Found || loc.Node == "" {
			// VM absent -> snapshot is necessarily absent. Idempotent success.
			logDeleteSnapshotAbsent(ctx, deps, snapshotCID, parsedCID,
				"delete_snapshot: VM absent from cluster index and every node's config probe -- snapshot already absent")
			return nil, nil
		}
		node := loc.Node

		deps.Log(ctx).Debug("delete_snapshot: VM located",
			log.String("snapshot_cid", snapshotCID),
			log.Int("vmid", vmid),
			log.String("node", node),
		)

		// ----------------------------------------------------------------
		// 3a. Ownership guard. Deleting a snapshot is a mutation, so warn
		//     does not relax it: a VM this Director does not own keeps its
		//     snapshots in both modes. The answer is success, because the
		//     snapshot the CID named is not on this VM, and the Director
		//     drops a record it could never act on. A snapshot that really
		//     lives on another cluster leaks there and goes when its VM
		//     does.
		// ----------------------------------------------------------------
		verdict, ownErr := checkVMOwnership(ctx, deps, "delete_snapshot", parsedCID, loc, nil)
		if ownErr != nil {
			return nil, ownErr
		}
		if verdict.Absent {
			logDeleteSnapshotAbsent(ctx, deps, snapshotCID, parsedCID,
				"delete_snapshot: VM is gone from every cluster member -- answering success")
			return nil, nil
		}
		if !verdict.Owned {
			deps.Log(ctx).Warn("delete_snapshot: VM is not owned by this Director -- leaving it untouched and answering success",
				log.String("snapshot_cid", snapshotCID),
				log.Int("vmid", vmid),
				log.String("ownership_reason", verdict.Reason),
				log.String("ownership_detail", verdict.Detail),
			)
			return nil, nil
		}

		// ----------------------------------------------------------------
		// 4. Delete snapshot via SDK. A migration that starts after the wait
		//    refuses the delete with a lock or leaves its answer lost, and
		//    pve.MutateVMConfig settles, follows the VM, and looks for the
		//    snapshot before it sends again. A snapshot that is gone is success.
		// ----------------------------------------------------------------
		node, delErr := mutateSnapshotDelete(ctx, deps, vmCID, vmid, node, snapName)
		if delErr != nil {
			return nil, deleteSnapshotFailure(ctx, deps, vmCID, vmid, node, snapshotCID, snapName, delErr)
		}

		// PVE deletes snapshots via an async worker task, and the SDK discards
		// the task UPID, so the DELETE above returns before PVE has actually
		// removed the snapshot. Wait until it is gone; otherwise an immediately
		// following detach_disk (whose guard rejects live snapshots) fails
		// spuriously. A missing snapshot or VM returned earlier, so reaching
		// here means the snapshot existed and a deletion task is in flight.
		if waitErr := pve.WaitForSnapshotAbsent(ctx, deps.PVE, node, vmid, snapName,
			pve.WithMaxWait(120*time.Second)); waitErr != nil {
			if isConfigMissing(waitErr) {
				if err := deleteSnapshotMissing(ctx, deps, vmid, node, snapName); err != nil {
					return nil, err
				}
				return nil, nil
			}
			return nil, cpierrors.Wrap(waitErr, "delete_snapshot: waiting for snapshot "+snapName+" removal on VM "+vmCID)
		}

		deps.Log(ctx).Info("delete_snapshot",
			log.String("snapshot_cid", snapshotCID),
			log.Int("vmid", vmid),
			log.String("snap_name", snapName),
		)

		return nil, nil
	})
}

// deleteSnapshotFailure maps the failure of the snapshot delete to the
// handler's answer. A missing config is success when the VM is proven gone and
// the snapshot is absent, and anything else is wrapped with the call that
// failed. It returns nil for the success answer.
func deleteSnapshotFailure(
	ctx context.Context, deps Deps, vmCID string, vmid int, node, snapshotCID, snapName string, delErr error,
) error {
	var typed *cpierrors.Error
	if isConfigMissing(delErr) && !errors.As(delErr, &typed) {
		if err := deleteSnapshotMissing(ctx, deps, vmid, node, snapName); err != nil {
			return err
		}
		deps.Log(ctx).Info("delete_snapshot: snapshot already absent, skipping",
			log.String("snapshot_cid", snapshotCID),
			log.Int("vmid", vmid),
			log.String("snap_name", snapName),
		)
		return nil
	}
	return cpierrors.Wrap(pve.WrapError(delErr), "delete_snapshot: DeleteSnapshot failed for VM "+vmCID+" snap "+snapName)
}

// mutateSnapshotDelete sends the snapshot delete through pve.MutateVMConfig and
// returns the node the delete landed on. The delete is converged by name: a
// snapshot that is no longer listed counts as deleted, so a delete whose answer
// was lost is never sent again after it landed.
func mutateSnapshotDelete(
	ctx context.Context, deps Deps, vmCID string, vmid int, node, snapName string,
) (string, error) {
	return mutateDiskVM(ctx, deps, diskVMMutation{
		op: "delete_snapshot", vmCID: vmCID, vmid: vmid, node: node,
		apply: func(ctx context.Context, at string) error {
			return retryOnTransientOrLockUntilMissing(ctx, deps.Log(ctx), "delete_snapshot", 0, func() error {
				return deps.PVE.QEMU().DeleteSnapshot(ctx, at, vmid, snapName)
			})
		},
		landed: func(ctx context.Context, at string) (bool, error) {
			present, err := snapshotListed(ctx, deps, at, vmid, snapName)
			return !present, err
		},
		reresolve: func(ctx context.Context, vm pve.VMLocation) error {
			return requireNodeAnswers(ctx, deps, "delete_snapshot", fmt.Sprintf("VM %s", vmCID), vm.Node)
		},
	})
}

// logDeleteSnapshotAbsent records delete_snapshot's success answer for a VM
// absent from every member of this cluster. The answer is success in every
// case, because the snapshot is not on this cluster and the Director drops a
// record it could never act on. A stamped CID whose cluster stamp names
// another cluster is logged as a warning, because its snapshot may still live
// there and leaks until its VM is deleted. A failed read of the cluster
// identity is logged as a warning too, rather than failing a delete that has
// nothing to remove here.
func logDeleteSnapshotAbsent(ctx context.Context, deps Deps, snapshotCID string, cid vmcid.VMCID, msg string) {
	logger := deps.Log(ctx).With(log.String("snapshot_cid", snapshotCID), log.Int("vmid", cid.VMID))
	verdict, err := absentVMVerdict(ctx, deps, "delete_snapshot", cid)
	switch {
	case err != nil:
		logger.Warn("delete_snapshot: the VM is absent from this cluster, and the cluster identity could not be read "+
			"to tell whether the snapshot CID names this cluster -- answering success", log.Err(err))
	case verdict.Elsewhere:
		logger.Warn("delete_snapshot: the VM is absent from this cluster, and the snapshot CID names another cluster, "+
			"where the snapshot may remain until its VM is deleted -- answering success",
			log.String("ownership_reason", verdict.Reason), log.String("ownership_detail", verdict.Detail))
	default:
		logger.Info(msg)
	}
}

// snapshotListed reports whether snapName is among the snapshots PVE lists for
// the VM on node. A listing that fails is an error, never "absent".
func snapshotListed(ctx context.Context, deps Deps, node string, vmid int, snapName string) (bool, error) {
	names, err := pve.HasSnapshots(ctx, deps.PVE, node, vmid)
	if err != nil {
		return false, err
	}
	for _, name := range names {
		if name == snapName {
			return true, nil
		}
	}
	return false, nil
}

// deleteSnapshotMissing settles a delete whose last call to node answered not
// found or config missing. That answer speaks for node alone. When the absence
// proof finds the VM nowhere, the snapshot is gone with it and the delete is
// done. When the VM is found, whether it is still on node or moved, the
// snapshot list on the node that holds it decides: a snapshot that is not
// listed is gone, and one that is listed is a retriable error, so the next try
// runs where the VM is. Every lookup that fails is a retriable error too.
func deleteSnapshotMissing(ctx context.Context, deps Deps, vmid int, node, snapName string) error {
	loc, err := locateAfterConfigMissing(ctx, deps, "delete_snapshot", vmid, node)
	if err != nil {
		return err
	}
	if !loc.Found {
		return nil
	}
	listed, listErr := snapshotListed(ctx, deps, loc.Node, vmid, snapName)
	if listErr != nil {
		return cpierrors.WrapAs(pve.WrapError(listErr), cpierrors.TypeRetriableCloud, fmt.Sprintf(
			"delete_snapshot: the delete on node %s answered that the VM is missing, and the snapshot list on node %s "+
				"could not be read", node, loc.Node))
	}
	if listed {
		return cpierrors.Retriable(
			"delete_snapshot: the delete on node %s answered that the VM is missing, but snapshot %s is still listed on "+
				"node %s; retry so the delete runs where the VM is", node, snapName, loc.Node)
	}
	return nil
}
