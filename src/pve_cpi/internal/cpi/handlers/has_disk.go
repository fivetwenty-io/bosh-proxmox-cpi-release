package handlers

import (
	"context"
	"encoding/json"

	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
)

// HandleHasDisk returns a Handler for the BOSH CPI has_disk method.
//
// Arguments (positional JSON array):
//
//	[0] disk_cid  string — disk CID of the form "<storage>:<volume>"
//
// Returns: bool — true if the volume exists in PVE storage, false if not.
//
// Error handling:
//   - Malformed disk_cid → non-nil error (CloudError, not retriable).
//   - 404 from PVE → false, nil (volume absent is normal).
//   - Other SDK errors → non-nil error propagated to the dispatcher.
//
// Node selection: deps.Config.Node is used as the target node. Shared storage
// volumes are cluster-visible; local storage volumes require the correct node.
func HandleHasDisk(deps Deps) Handler {
	return HandlerFunc(func(ctx context.Context, args []json.RawMessage, _ jsonrpc.Context) (any, error) {
		// ----------------------------------------------------------------
		// 1. Unmarshal and validate arguments.
		// ----------------------------------------------------------------
		if len(args) < 1 {
			return nil, cpierrors.Cloud("has_disk: expected 1 argument (disk_cid), got 0")
		}

		var diskCID string
		if err := json.Unmarshal(args[0], &diskCID); err != nil {
			return nil, cpierrors.Wrap(err, "has_disk: args[0] disk_cid must be a string")
		}
		if diskCID == "" {
			return nil, cpierrors.Cloud("has_disk: disk_cid must not be empty")
		}
		// Strip optional metadata suffix before any PVE API or storage lookup.
		bareDiskCID, _, decErr := pve.ParseEncodedDiskCID(diskCID)
		if decErr != nil {
			return nil, cpierrors.DiskNotFound(diskCID)
		}

		// ----------------------------------------------------------------
		// 2. Parse disk CID → storage + volume.
		// ----------------------------------------------------------------
		storage, _, err := pve.ParseDiskCID(bareDiskCID)
		if err != nil {
			return nil, cpierrors.Wrap(err, "has_disk: invalid disk_cid "+diskCID)
		}

		// ----------------------------------------------------------------
		// 3. Resolve node via backend. For local backends, NodeForExisting
		//    scans the cluster for the owning node and returns DiskNotFound
		//    when no node holds the volume — surface that as has_disk=false
		//    rather than an error. PVE's storage content endpoint wants the
		//    canonical "<storage>:<volname>" volid, which is the disk_cid.
		// ----------------------------------------------------------------
		backend, err := backendResolverOrDefault(deps).Resolve(ctx, storage)
		if err != nil {
			return nil, cpierrors.Wrap(err, "has_disk: backend resolution failed for storage "+storage)
		}
		node, err := backend.NodeForExisting(ctx, bareDiskCID)
		if err != nil {
			if pve.IsNotFound(err) {
				deps.Log(ctx).Debug("has_disk: backend reports volume not present on any node",
					log.String("disk_cid", diskCID),
				)
				return false, nil
			}
			return nil, cpierrors.Wrap(err, "has_disk")
		}

		// ----------------------------------------------------------------
		// 4. Call storage.Exists via ExistsTolerant so block-backed
		//    storages (lvmthin/zfspool) that return 500 wrapping
		//    "Failed to find logical volume" / "dataset does not exist"
		//    for a missing volume report a clean (false, nil) — has_disk
		//    must answer false for a just-deleted disk regardless of
		//    backend, not raise a retriable cloud error.
		// ----------------------------------------------------------------
		exists, err := pve.ExistsTolerant(ctx, deps.PVE, node, storage, bareDiskCID)
		if err != nil {
			// Belt-and-braces: any not-found classification surfacing through
			// a non-Exists path still resolves to false.
			if pve.IsVolumeMissing(err) {
				deps.Log(ctx).Debug("has_disk: not found via error path, returning false",
					log.String("disk_cid", diskCID),
				)
				return false, nil
			}
			return nil, cpierrors.Wrap(err, "has_disk: Exists check failed for "+diskCID+" on node "+node)
		}

		deps.Log(ctx).Debug("has_disk", log.String("disk_cid", diskCID), log.Bool("exists", exists))
		return exists, nil
	})
}
