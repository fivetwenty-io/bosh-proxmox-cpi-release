package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
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

		vmid, err := strconv.Atoi(vmCID)
		if err != nil || vmid <= 0 {
			return nil, cpierrors.VMNotFound(vmCID)
		}

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
