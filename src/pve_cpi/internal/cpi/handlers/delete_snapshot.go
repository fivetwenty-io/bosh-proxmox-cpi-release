// Package handlers contains the 22 BOSH CPI v2 method implementations.
package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
)

// HandleDeleteSnapshot returns a Handler for the BOSH CPI delete_snapshot method.
//
// Arguments (positional JSON array):
//
//	[0] snapshot_cid  string — snapshot CID of the form "<vmid>:<snap_name>"
//
// Returns: null on success (BOSH void method).
//
// Idempotency: a 404 from PVE (snapshot not found) is treated as success.
// This matches BOSH Director expectations for delete operations, which may be
// retried after partial failures.
func HandleDeleteSnapshot(deps Deps) Handler {
	return HandlerFunc(func(ctx context.Context, args []json.RawMessage, _ jsonrpc.Context) (any, error) {
		// ----------------------------------------------------------------
		// 1. Unmarshal and validate arguments.
		// ----------------------------------------------------------------
		if len(args) < 1 {
			return nil, fmt.Errorf("delete_snapshot: expected 1 argument (snapshot_cid), got 0")
		}

		var snapshotCID string
		if err := json.Unmarshal(args[0], &snapshotCID); err != nil {
			return nil, fmt.Errorf("delete_snapshot: args[0] snapshot_cid must be a string: %w", err)
		}
		if snapshotCID == "" {
			return nil, fmt.Errorf("delete_snapshot: args[0] snapshot_cid must not be empty")
		}

		// ----------------------------------------------------------------
		// 2. Parse snapshot_cid → vmCID + snapName.
		// ----------------------------------------------------------------
		vmCID, snapName, err := pve.ParseSnapshotCID(snapshotCID)
		if err != nil {
			return nil, fmt.Errorf("delete_snapshot: invalid snapshot_cid %q: %w", snapshotCID, err)
		}

		vmid, err := strconv.Atoi(vmCID)
		if err != nil || vmid <= 0 {
			return nil, cpierrors.VMNotFound(vmCID)
		}

		node := deps.Config.Node
		if node == "" {
			return nil, fmt.Errorf("delete_snapshot: node is not configured")
		}

		// ----------------------------------------------------------------
		// 3. Delete snapshot via SDK. 404 → idempotent success.
		// ----------------------------------------------------------------
		delErr := pve.RetryOnStorageLock(ctx, deps.Logger, "delete_snapshot", 0, func() error {
			return deps.PVE.QEMU().DeleteSnapshot(ctx, node, vmid, snapName)
		})
		if err := delErr; err != nil {
			if pve.IsNotFound(err) {
				deps.Logger.Info("delete_snapshot: snapshot already absent, skipping",
					log.String("snapshot_cid", snapshotCID),
					log.Int("vmid", vmid),
					log.String("snap_name", snapName),
				)
				return nil, nil
			}
			wrapped := pve.WrapError(err)
			return nil, fmt.Errorf("delete_snapshot: DeleteSnapshot failed for VM %s snap %s: %w",
				vmCID, snapName, wrapped)
		}

		deps.Logger.Info("delete_snapshot",
			log.String("snapshot_cid", snapshotCID),
			log.Int("vmid", vmid),
			log.String("snap_name", snapName),
		)

		return nil, nil
	})
}
