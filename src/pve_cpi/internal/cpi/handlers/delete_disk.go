package handlers

import (
	"context"
	"encoding/json"

	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
)

// unparkBeforeDelete resolves which VM holds bareDiskCID and, when that VM is a
// parker, detaches the disk before the volume is deleted. Unpark failure
// returns a retriable wrapped error; the disk stays safe on the parker VM and
// the caller can retry.
//
// The holder scan is unconditional rather than gated on the parked strategy,
// for the same reason attach_disk's is: a parker the configuration can no
// longer recognize still holds the volume, and deleting it would leave that
// parker's scsi slot referencing storage that no longer exists. Deleting a
// volume out from under a live reference is not a failure mode worth saving a
// cluster scan over. A holder that is an ordinary VM is left to the existing
// behavior, which the optional disk_delete_state_guard governs.
func unparkBeforeDelete(ctx context.Context, deps Deps, diskCID, bareDiskCID string) error {
	if deps.Config == nil {
		return nil
	}
	parkerCfg := parkerReadConfigFor(deps)
	holder, resolveErr := pve.ResolveDiskHolder(ctx, deps.PVE, deps.Log(ctx), bareDiskCID, parkerCfg)
	if resolveErr != nil {
		return wrapHolderScanError(resolveErr, "delete_disk: resolve current holder before delete")
	}
	if strandedErr := strandedParkerRefusal(deps, "delete_disk", diskCID, holder); strandedErr != nil {
		return strandedErr
	}
	// UnparkDiskAt reuses the holder just resolved and is a no-op when the disk
	// is not on a parker, so the parked path still costs one cluster scan.
	if unparkErr := pve.UnparkDiskAt(ctx, deps.PVE, deps.Log(ctx), bareDiskCID, holder, parkerCfg); unparkErr != nil {
		// cpierrors.Wrap keeps whatever class the unpark chose. Most failures
		// are retriable, but two are permanent on purpose: a denied grant, and
		// a reference the sweep could not clear. Neither improves on a retry,
		// and both carry the command that repairs them.
		deps.Log(ctx).Info("delete_disk: unpark failed",
			log.String("disk_cid", diskCID),
		)
		return cpierrors.Wrap(unparkErr, "delete_disk: unpark before delete")
	}
	return nil
}

// HandleDeleteDisk returns a Handler for the BOSH CPI delete_disk method.
//
// Arguments (positional JSON array):
//
//	[0] disk_cid  string — disk CID in the "pvd-" (or compressed "pvz-")
//	                       envelope form emitted by create_disk
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
	return HandlerFunc(func(ctx context.Context, args []json.RawMessage, reqCtx jsonrpc.Context) (any, error) {
		deps, err := deps.WithRequestOverrides(ctx, reqCtx)
		if err != nil {
			return nil, err
		}
		// ----------------------------------------------------------------
		// 1. Unmarshal and validate arguments.
		// ----------------------------------------------------------------
		if len(args) < 1 {
			return nil, cpierrors.Cloud("delete_disk: expected 1 argument (disk_cid), got 0")
		}

		var diskCID string
		if err := json.Unmarshal(args[0], &diskCID); err != nil {
			return nil, cpierrors.Wrap(err, "delete_disk: args[0] disk_cid must be a string")
		}
		if diskCID == "" {
			return nil, cpierrors.Cloud("delete_disk: disk_cid must not be empty")
		}
		// Strip optional metadata suffix before any PVE API or storage lookup.
		bareDiskCID, _, decErr := decodeDiskCID(ctx, deps, "delete_disk", diskCID)
		if decErr != nil {
			return nil, decErr
		}

		// ----------------------------------------------------------------
		// 2. Parse disk CID → storage + volume.
		// ----------------------------------------------------------------
		storage, _, err := pve.ParseDiskCID(bareDiskCID)
		if err != nil {
			return nil, cpierrors.Wrap(err, "delete_disk: invalid disk_cid "+diskCID)
		}

		// ----------------------------------------------------------------
		// 3. Resolve node via backend (shared → defaultNode; local → cluster
		//    scan locating the volume's owning node). PVE's storage content
		//    endpoint wants the canonical "<storage>:<volname>" volid, which
		//    is the disk_cid as-is.
		// ----------------------------------------------------------------
		backend, err := backendResolverOrDefault(deps).Resolve(ctx, storage)
		if err != nil {
			return nil, cpierrors.Wrap(err, "delete_disk: backend resolution failed for storage "+storage)
		}
		node, err := backend.NodeForExisting(ctx, bareDiskCID)
		if err != nil {
			if pve.IsNotFound(err) {
				deps.Log(ctx).Info("delete_disk: volume not found on any node, treating as already-deleted",
					log.String("disk_cid", diskCID),
				)
				return nil, nil
			}
			return nil, cpierrors.Wrap(err, "delete_disk")
		}

		// ----------------------------------------------------------------
		// 3b. Optional pre-delete attached-VM lock guard. When enabled, defer
		//     the delete if the VM this volume is attached to is mid-backup/
		//     clone/migrate/snapshot/rollback/create, so the imgdel does not
		//     race a destructive operation. Also defers (retriable) when the
		//     holder or its lock state cannot be resolved transiently; fails
		//     open only on permanent resolution failures (see
		//     pve.GuardDiskDeleteState). A disk attached to no VM passes
		//     straight through. Off → no extra calls.
		// ----------------------------------------------------------------
		if deps.Config.DiskDeleteStateGuardEnabled() {
			if guardErr := pve.GuardDiskDeleteState(ctx, deps.PVE, node, bareDiskCID); guardErr != nil {
				deps.Log(ctx).Info("delete_disk: deferring delete, attached VM busy or holder state unresolved",
					log.String("disk_cid", diskCID),
				)
				return nil, cpierrors.Wrap(guardErr, "delete_disk")
			}
		}

		// ----------------------------------------------------------------
		// 3c. Parker unpark. When the parked strategy is active, check
		//     cluster-wide whether the disk is held on a parker VM and, if
		//     so, detach it before deleting. Not-parked → fall through.
		//     Unpark failure → retriable; the disk is still safe on the
		//     parker VM and the caller can retry.
		// ----------------------------------------------------------------
		if err := unparkBeforeDelete(ctx, deps, diskCID, bareDiskCID); err != nil {
			return nil, err
		}

		// ----------------------------------------------------------------
		// 4. Delete the volume.
		//
		// Fast-path mode (fast_path_delete=true): issue DeleteVolumeAsync and
		// return immediately without awaiting the imgdel task. Eventual
		// consistency: has_disk may briefly still see the volume. Disk volumes
		// cannot carry PVE tags, so no "bosh-deleting" marker is applied here;
		// the operator must rely on the volume eventually disappearing from
		// storage. The §7.13 orphan-GC sweep (for VM templates) does not cover
		// raw volumes; PVE's own storage GC handles stale imgdel residue.
		//
		// Slow-path mode (default): issue DeleteVolumeAsync and await the
		// returned imgdel UPID so a queued imgdel under storage-lock contention
		// cannot fire after delete_disk has already returned success.
		// ----------------------------------------------------------------
		delErr := pve.RetryOnTransientOrLock(ctx, deps.Log(ctx), "delete_disk", 0, func() error {
			upid, err := deps.PVE.Storage().DeleteVolumeAsync(ctx, node, storage, bareDiskCID)
			if err != nil {
				return err
			}
			// Fast-path: return without awaiting the task UPID.
			if deps.Config.FastPathDeleteEnabled() {
				return nil
			}
			if upid == "" {
				return nil
			}
			return pve.AwaitTaskWithLogger(ctx, deps.PVE, node, upid, deps.Log(ctx))
		})
		if err := delErr; err != nil {
			// Check whether the error is a not-found variant. The SDK
			// DeleteVolume already swallows 404 internally, but if a
			// non-SDK not-found surfaces we still treat it as success.
			if pve.IsNotFound(err) {
				deps.Log(ctx).Info("delete_disk: disk already absent, skipping",
					log.String("disk_cid", diskCID),
				)
				return nil, nil
			}
			return nil, cpierrors.Wrap(err, "delete_disk: DeleteVolume failed for "+diskCID+" on node "+node)
		}

		deps.Log(ctx).Info("delete_disk", log.String("disk_cid", diskCID), log.String("node", node))
		return nil, nil
	})
}
