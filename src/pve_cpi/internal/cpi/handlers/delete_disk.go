package handlers

import (
	"context"
	"encoding/json"
	"fmt"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// unparkBeforeDelete resolves which VM holds the disk and, when that VM is a
// parker, releases the volume before the storage delete. Unpark failure
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
//
// A stable-ID disk whose volume a reassignment renamed for its parker cannot
// take the ordinary unpark: PVE deallocates an unused volume its holder owns,
// so the unpark's sweep semantics ARE the deletion, and its safety guard
// rightly refuses. DeleteParkedOwnedDisk makes that intent explicit — the
// detach deallocates the volume.
//
// The bool reports that the volume is already off storage and the caller must
// not issue a storage delete. Two paths set it. The parked-owned deallocation
// above IS the delete, and an imgdel behind it can only fail: on Ceph it does,
// with "rbd: error opening image <name>: (2) No such file or directory" from
// a task PVE accepted, which is a task failure rather than the 404 the
// idempotent path reads. The second is a promised anchor whose volume is not
// on storage at all: nothing to unpark and nothing to delete, so the delete
// the Director asked for has already happened.
func unparkBeforeDelete(ctx context.Context, deps Deps, rd resolvedDisk, node string) (bool, error) {
	if deps.Config == nil {
		return false, nil
	}
	parkerCfg := parkerReadConfigFor(deps)
	var holder pve.DiskHolder
	if rd.stableID != "" {
		if rd.holder != nil {
			holder = *rd.holder
		}
	} else {
		var resolveErr error
		holder, resolveErr = pve.ResolveDiskHolder(ctx, deps.PVE, deps.Log(ctx), rd.volid, parkerCfg)
		if resolveErr != nil {
			return false, wrapHolderScanError(resolveErr, "delete_disk: resolve current holder before delete")
		}
	}
	// A disk whose CID promises a parker anchor must have a holder while
	// detached; no holder at all means the parker vanished out-of-band, and
	// deleting the volume would destroy the one copy of the data before
	// anyone has looked at why its protected home disappeared.
	if anchorErr := anchorMissingRefusal(ctx, deps, "delete_disk", rd.diskCID, rd.meta, holder); anchorErr != nil {
		// The refusal reads "no holder" as a parker deleted out-of-band, which
		// is right only while the volume is still there. When it is not, the
		// state is a completed delete, and answering it with advice to relax
		// pve.parked_anchor_strict points the operator at a fault that is not
		// the one in front of them. Absence has to be established, not assumed:
		// a probe that fails proves nothing, so the refusal stands.
		gone, checkErr := volumeAbsentFromStorage(ctx, deps, node, rd.volid)
		if checkErr != nil || !gone {
			return false, anchorErr
		}
		deps.Log(ctx).Info("delete_disk: promised anchor has no holder and the volume is not on storage, treating as already-deleted",
			log.String("disk_cid", rd.diskCID),
		)
		return true, nil
	}
	if strandedErr := strandedParkerRefusal(deps, "delete_disk", rd.diskCID, holder); strandedErr != nil {
		return false, strandedErr
	}
	if holder.Found && holder.IsParker && rd.stableID != "" {
		if embedded, ok := pve.EmbeddedDiskVMID(rd.volid); ok && embedded == holder.VMID {
			if delErr := pve.DeleteParkedOwnedDisk(ctx, deps.PVE, deps.Log(ctx), holder.Node, holder.VMID, rd.volid, parkerCfg); delErr != nil {
				return false, retriableUnlessPermanent(delErr,
					fmt.Sprintf("delete_disk: deallocate parked disk %s on its parker", rd.diskCID))
			}
			// The detach deallocated the volume. There is nothing left for a
			// storage delete to remove.
			return true, nil
		}
	}
	// UnparkDiskAt reuses the holder just resolved and is a no-op when the disk
	// is not on a parker, so the parked path still costs one cluster scan.
	if unparkErr := pve.UnparkDiskAt(ctx, deps.PVE, deps.Log(ctx), rd.volid, holder, parkerCfg); unparkErr != nil {
		// cpierrors.Wrap keeps whatever class the unpark chose. Most failures
		// are retriable, but two are permanent on purpose: a denied grant, and
		// a reference the sweep could not clear. Neither improves on a retry,
		// and both carry the command that repairs them.
		deps.Log(ctx).Info("delete_disk: unpark failed",
			log.String("disk_cid", rd.diskCID),
		)
		return false, cpierrors.Wrap(unparkErr, "delete_disk: unpark before delete")
	}
	return false, nil
}

// volumeAbsentFromStorage reports whether the volume is provably not on
// storage. The error return distinguishes "present" from "could not tell":
// callers use this to turn a refusal into idempotent success, and only a
// probe that answered may do that.
func volumeAbsentFromStorage(ctx context.Context, deps Deps, node, bareVolid string) (bool, error) {
	if node == "" {
		return false, cpierrors.Cloud("delete_disk: no node to probe storage from")
	}
	storage, _, err := pve.ParseDiskCID(bareVolid)
	if err != nil {
		return false, err
	}
	exists, existsErr := pve.ExistsTolerant(ctx, deps.PVE, node, storage, bareVolid)
	if existsErr != nil {
		return false, existsErr
	}
	return !exists, nil
}

// deleteDiskVolume issues the storage delete for a volume nothing references
// any more.
//
// Fast-path mode (fast_path_delete=true): issue DeleteVolumeAsync and return
// immediately without awaiting the imgdel task. Eventual consistency: has_disk
// may briefly still see the volume. Disk volumes cannot carry PVE tags, so no
// "bosh-deleting" marker is applied here; the operator must rely on the volume
// eventually disappearing from storage. The §7.13 orphan-GC sweep (for VM
// templates) does not cover raw volumes; PVE's own storage GC handles stale
// imgdel residue.
//
// Slow-path mode (default): issue DeleteVolumeAsync and await the returned
// imgdel UPID so a queued imgdel under storage-lock contention cannot fire
// after delete_disk has already returned success.
func deleteDiskVolume(ctx context.Context, deps Deps, diskCID, bareDiskCID, storage, node string) error {
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
	if delErr == nil {
		return nil
	}
	// Check whether the error says the volume is already gone. The SDK
	// DeleteVolume swallows 404 internally, but block-backed storages report an
	// absent volume through a failed task rather than a 404: lvmthin and
	// zfspool with their CLI text, rbd with "error opening image ...: (2) No
	// such file or directory". IsVolumeMissing reads all of those; IsNotFound
	// alone reads only the 404, which is how a completed delete came back to
	// the Director as a failure.
	if pve.IsVolumeMissing(delErr) {
		deps.Log(ctx).Info("delete_disk: disk already absent, skipping",
			log.String("disk_cid", diskCID),
		)
		return nil
	}
	// WrapErrorKeepingClass like every sibling delete path: a transient storage
	// fault must not surface as a permanent delete_disk failure.
	return cpierrors.Wrap(pve.WrapErrorKeepingClass(delErr),
		"delete_disk: DeleteVolume failed for "+diskCID+" on node "+node)
}

// resolveDeleteDiskCID is delete_disk's identity seam: decode the CID, map it
// to the volume's current name — after a reassignment the envelope volid is
// only the birth record — and converge an interrupted transfer to its parked
// state before anything gets deleted.
func resolveDeleteDiskCID(ctx context.Context, deps Deps, diskCID string) (resolvedDisk, error) {
	// meta feeds the anchor-missing check in unparkBeforeDelete.
	bareDiskCID, meta, decErr := decodeDiskCID(ctx, deps, "delete_disk", diskCID)
	if decErr != nil {
		return resolvedDisk{}, decErr
	}
	rd, resolveErr := resolveDiskForOp(ctx, deps, "delete_disk", diskCID, bareDiskCID, meta)
	if resolveErr != nil {
		return resolvedDisk{}, resolveErr
	}
	return resumeTransferIfNeeded(ctx, deps, "delete_disk", rd)
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
		rd, decErr := resolveDeleteDiskCID(ctx, deps, diskCID)
		if decErr != nil {
			return nil, decErr
		}
		bareDiskCID := rd.volid

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
			if guardErr := pve.GuardDiskDeleteState(ctx, deps.PVE, bareDiskCID); guardErr != nil {
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
		alreadyDeleted, err := unparkBeforeDelete(ctx, deps, rd, node)
		if err != nil {
			return nil, err
		}
		if alreadyDeleted {
			deps.Log(ctx).Info("delete_disk", log.String("disk_cid", diskCID), log.String("node", node))
			return nil, nil
		}

		// ----------------------------------------------------------------
		// 4. Delete the volume.
		// ----------------------------------------------------------------
		if delErr := deleteDiskVolume(ctx, deps, diskCID, bareDiskCID, storage, node); delErr != nil {
			return nil, delErr
		}

		deps.Log(ctx).Info("delete_disk", log.String("disk_cid", diskCID), log.String("node", node))
		return nil, nil
	})
}
