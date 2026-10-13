package handlers

import (
	"context"
	"encoding/json"
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
		loc, lookupErr := pve.FindVMAuthoritative(ctx, deps.PVE, vmid)
		if lookupErr != nil {
			return nil, cpierrors.Wrap(lookupErr,
				fmt.Sprintf("delete_snapshot: locate VM %s", vmCID))
		}
		if !loc.Found || loc.Node == "" {
			// VM absent -> snapshot is necessarily absent. Idempotent success.
			deps.Log(ctx).Info("delete_snapshot: VM absent from cluster index and every node's config probe -- snapshot already absent",
				log.String("snapshot_cid", snapshotCID),
				log.Int("vmid", vmid),
			)
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
			deps.Log(ctx).Info("delete_snapshot: VM is gone from every cluster member -- answering success",
				log.String("snapshot_cid", snapshotCID),
				log.Int("vmid", vmid),
			)
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
		// 4. Delete snapshot via SDK. 404 -> idempotent success.
		// ----------------------------------------------------------------
		delErr := pve.RetryOnTransientOrLock(ctx, deps.Log(ctx), "delete_snapshot", 0, func() error {
			return deps.PVE.QEMU().DeleteSnapshot(ctx, node, vmid, snapName)
		})
		if err := delErr; err != nil {
			if pve.IsNotFound(err) {
				deps.Log(ctx).Info("delete_snapshot: snapshot already absent, skipping",
					log.String("snapshot_cid", snapshotCID),
					log.Int("vmid", vmid),
					log.String("snap_name", snapName),
				)
				return nil, nil
			}
			return nil, cpierrors.Wrap(pve.WrapError(err), "delete_snapshot: DeleteSnapshot failed for VM "+vmCID+" snap "+snapName)
		}

		// PVE deletes snapshots via an async worker task, and the SDK discards
		// the task UPID, so the DELETE above returns before PVE has actually
		// removed the snapshot. Wait until it is gone; otherwise an immediately
		// following detach_disk (whose guard rejects live snapshots) fails
		// spuriously. A 404/idempotent delete returned earlier, so reaching here
		// means the snapshot existed and a deletion task is in flight.
		if waitErr := pve.WaitForSnapshotAbsent(ctx, deps.PVE, node, vmid, snapName,
			pve.WithMaxWait(120*time.Second)); waitErr != nil {
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
