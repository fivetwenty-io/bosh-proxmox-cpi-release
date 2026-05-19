// Package handlers contains the 22 BOSH CPI v2 method implementations.
package handlers

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
)

// HandleDeleteDisk returns a Handler for the BOSH CPI delete_disk method.
//
// Arguments (positional JSON array):
//
//	[0] disk_cid  string — disk CID of the form "<storage>:<volume>"
//
// Returns: null on success (BOSH void method).
//
// Idempotency: a 404 from PVE (volume not found) is treated as success,
// matching BOSH Director expectations for delete operations.
//
// Node selection: deps.Config.Node is used as the target node. Shared storage
// volumes are accessible from any node; local storage volumes must be accessed
// from the node that hosts them. Operators using local storage must ensure the
// configured node matches the volume's location.
func HandleDeleteDisk(deps Deps) Handler {
	return HandlerFunc(func(ctx context.Context, args []json.RawMessage, _ jsonrpc.Context) (any, error) {
		// ----------------------------------------------------------------
		// 1. Unmarshal and validate arguments.
		// ----------------------------------------------------------------
		if len(args) < 1 {
			return nil, fmt.Errorf("delete_disk: expected 1 argument (disk_cid), got 0")
		}

		var diskCID string
		if err := json.Unmarshal(args[0], &diskCID); err != nil {
			return nil, fmt.Errorf("delete_disk: args[0] disk_cid must be a string: %w", err)
		}
		if diskCID == "" {
			return nil, fmt.Errorf("delete_disk: disk_cid must not be empty")
		}

		// ----------------------------------------------------------------
		// 2. Parse disk CID → storage + volume.
		// ----------------------------------------------------------------
		storage, volume, err := pve.ParseDiskCID(diskCID)
		if err != nil {
			return nil, fmt.Errorf("delete_disk: invalid disk_cid %q: %w", diskCID, err)
		}

		// ----------------------------------------------------------------
		// 3. Resolve node via backend (shared → defaultNode; local → cluster
		//    scan locating the volume's owning node).
		// ----------------------------------------------------------------
		backend, err := backendResolverOrDefault(deps).Resolve(ctx, storage)
		if err != nil {
			return nil, fmt.Errorf("delete_disk: backend resolution failed for storage %q: %w", storage, err)
		}
		node, err := backend.NodeForExisting(ctx, volume)
		if err != nil {
			if pve.IsNotFound(err) {
				deps.Logger.Info("delete_disk: volume not found on any node, treating as already-deleted",
					log.String("disk_cid", diskCID),
				)
				return nil, nil
			}
			return nil, fmt.Errorf("delete_disk: %w", err)
		}

		// ----------------------------------------------------------------
		// 4. Delete the volume. SDK DeleteVolume is already 404-safe;
		//    we do NOT propagate 404 as an error.
		// ----------------------------------------------------------------
		if err := deps.PVE.Storage().DeleteVolume(ctx, node, storage, volume); err != nil {
			// Check whether the error is a not-found variant. The SDK
			// DeleteVolume already swallows 404 internally, but if a
			// non-SDK not-found surfaces we still treat it as success.
			if pve.IsNotFound(err) {
				deps.Logger.Info("delete_disk: disk already absent, skipping",
					log.String("disk_cid", diskCID),
				)
				return nil, nil
			}
			return nil, fmt.Errorf("delete_disk: DeleteVolume failed for %q on node %q: %w", diskCID, node, err)
		}

		deps.Logger.Info("delete_disk", log.String("disk_cid", diskCID), log.String("node", node))
		return nil, nil
	})
}
