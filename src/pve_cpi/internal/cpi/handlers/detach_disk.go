package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
)

// detachDiskResolveSlot resolves the PVE node hosting diskCID and the disk's
// current bus slot (e.g. "scsi2") on the given VM.
//
// It performs three steps:
//  1. Backend resolution to locate the node that holds diskCID in PVE storage.
//  2. ResolveDiskID to find the active bus slot for diskCID in the VM config.
//  3. If ResolveDiskID returns ErrDiskNotAttached or a not-found error,
//     sweepUnusedDiskSlot is called to clean up any lingering unusedN entry.
//
// Return values:
//   - node: the PVE node name (non-empty on success).
//   - diskID: the slot string such as "scsi2" (non-empty when attached).
//   - alreadyDetached: true when the disk is not on any active bus or unusedN
//     slot — the caller should treat this as idempotent success and return nil.
//   - err: non-nil only for genuine failures (network errors, VM not found, etc.).
func detachDiskResolveSlot(
	ctx context.Context,
	deps Deps,
	vmCID string,
	vmid int,
	diskCID string,
) (node, diskID string, alreadyDetached bool, err error) {
	storage, _, err := pve.ParseDiskCID(diskCID)
	if err != nil {
		return "", "", false, cpierrors.DiskNotFound(diskCID)
	}

	// Locate the PVE node that holds the volume via the storage backend.
	backend, err := backendResolverOrDefault(deps).Resolve(ctx, storage)
	if err != nil {
		return "", "", false, cpierrors.Wrap(err, fmt.Sprintf("detach_disk: backend resolution failed for storage %q", storage))
	}
	node, err = backend.NodeForExisting(ctx, diskCID)
	if err != nil {
		if pve.IsNotFound(err) {
			// Volume gone — disk already detached from its perspective; idempotent.
			deps.Log(ctx).Warn("detach_disk: volume not found on any node, treating as already detached",
				log.String("vm_cid", vmCID),
				log.String("disk_cid", diskCID),
			)
			return "", "", true, nil
		}
		return "", "", false, cpierrors.Wrap(err, "detach_disk: node lookup failed")
	}

	// The VM-config operations that follow (ResolveDiskID's config read,
	// the detach PUT, the unusedN sweep) must target the node that RUNS
	// the VM. For shared backends NodeForExisting returns the configured
	// default node — a storage routing hint, not the VM's location; on a
	// multi-node cluster a config read there 404s and would be silently
	// misread as "disk not attached" (idempotent success). The cluster
	// scan is authoritative. A VM absent from the scan keeps the
	// backend-derived node: the config read then 404s and the existing
	// VM-gone → already-detached semantics apply.
	if vmNode, found, lookupErr := pve.FindVMNodeViaCluster(ctx, deps.PVE, vmid); lookupErr != nil {
		return "", "", false, cpierrors.Wrap(lookupErr, fmt.Sprintf("detach_disk: lookup VM %s node failed", vmCID))
	} else if backend.Kind() != pve.BackendLocal && found && vmNode != "" {
		node = vmNode
	}

	// Resolve the active bus slot for diskCID in the VM config.
	diskID, err = pve.ResolveDiskID(ctx, deps.PVE, node, vmid, diskCID)
	if err != nil {
		if errors.Is(err, pve.ErrDiskNotAttached) || pve.IsNotFound(err) {
			// Disk is not on an active bus. It may still linger as an unusedN
			// slot: a prior detach with allow_disk_ops_with_snapshots=true
			// parked it there, but PVE's unusedN sweep was blocked by a
			// snapshot. Once the snapshot is gone a follow-up detach_disk lands
			// here — completing that sweep is what makes the documented "delete
			// snapshots, then retry detach_disk" recovery actually free the
			// volume (delete_disk) and unblock delete_vm.
			//
			// The sentinel check (errors.Is) narrows the previously broad
			// TypeCloud catch: any other Cloud error from ResolveDiskID
			// (validation failures on node/vmid/volid) now propagates as a
			// real error instead of being silently swallowed as idempotent.
			swept, sweepErr := sweepUnusedDiskSlot(ctx, deps, node, vmid, vmCID, diskCID)
			if sweepErr != nil {
				return "", "", false, sweepErr
			}
			if !swept {
				deps.Log(ctx).Warn("detach_disk: disk not attached to VM; skipping",
					log.String("vm_cid", vmCID),
					log.String("disk_cid", diskCID),
					log.Err(err),
				)
			}
			return "", "", true, nil
		}
		// Config fetch error (network, 404 on VM itself, etc.).
		return "", "", false, cpierrors.Wrap(pve.WrapError(err), fmt.Sprintf("detach_disk: config fetch failed for VM %s", vmCID))
	}

	return node, diskID, false, nil
}

// HandleDetachDisk returns a Handler for the BOSH CPI detach_disk method.
//
// Arguments (positional JSON array):
//
//	[0] vm_cid   string — VMID of the target VM (integer as string, e.g. "100")
//	[1] disk_cid string — persistent disk CID in the "pvd-" (or compressed
//	                      "pvz-") envelope form emitted by create_disk
//
// Returns: null (void). The Director expects a null result on success.
//
// Logic:
//  1. Parse vm_cid to VMID int; parse disk_cid to storage+volid components.
//  2. Call detachDiskResolveSlot to locate the node and disk bus slot.
//     Returns alreadyDetached=true → return nil (idempotent).
//  3. Snapshot pre-flight guard (fail-closed or fail-open per config).
//  4. Call qemu.DetachDisk with the resolved diskID. The SDK issues a synchronous
//     PUT /nodes/{node}/qemu/{vmid}/config with {delete: diskID}. No UPID is returned
//     and no AwaitTask is required.
//  5. Remove the recorded disk_cid entry from the VM's description sentinel
//     (best-effort; see pve.RemoveAttachedDiskCID), mirroring attach_disk's write.
//  6. Return nil (void success).
//
// Idempotency: if the disk is not attached to the VM at step 2, the handler returns
// nil without calling DetachDisk. This matches the Perl CPI's "warn + return 1"
// behaviour and satisfies the BOSH Director's expectation that repeated detach_disk
// calls on an already-detached disk succeed without error.
//
// The disk volume is NOT deleted from storage; that is handled by delete_disk.
func HandleDetachDisk(deps Deps) Handler {
	return HandlerFunc(func(ctx context.Context, args []json.RawMessage, reqCtx jsonrpc.Context) (any, error) {
		deps, err := deps.WithRequestOverrides(ctx, reqCtx)
		if err != nil {
			return nil, err
		}
		// --------------------------------------------------------------------
		// 1. Unmarshal and validate arguments.
		// --------------------------------------------------------------------
		if len(args) < 2 {
			return nil, cpierrors.Cloud("detach_disk: expected 2 arguments (vm_cid, disk_cid), got %d", len(args))
		}

		var vmCID string
		if err := json.Unmarshal(args[0], &vmCID); err != nil {
			return nil, cpierrors.Wrap(err, "detach_disk: args[0] vm_cid must be a string")
		}
		if vmCID == "" {
			return nil, cpierrors.Cloud("detach_disk: args[0] vm_cid must not be empty")
		}

		var diskCID string
		if err := json.Unmarshal(args[1], &diskCID); err != nil {
			return nil, cpierrors.Wrap(err, "detach_disk: args[1] disk_cid must be a string")
		}
		if diskCID == "" {
			return nil, cpierrors.Cloud("detach_disk: args[1] disk_cid must not be empty")
		}
		// Strip optional metadata suffix; PVE API and volid comparisons need
		// the bare "<storage>:<volid>" form.
		bareDiskCID, meta, decErr := decodeDiskCID(ctx, deps, "detach_disk", diskCID)
		if decErr != nil {
			return nil, decErr
		}

		// --------------------------------------------------------------------
		// 2. Parse vm_cid → VMID; resolve node + disk slot via helper.
		// --------------------------------------------------------------------
		vmid, err := strconv.Atoi(vmCID)
		if err != nil || vmid <= 0 {
			return nil, cpierrors.VMNotFound(vmCID)
		}

		// Stable-ID disks detach by ownership transfer: the volume is
		// reassigned onto a parker rather than config-edited off the VM. The
		// two flows share nothing past this point — a renamed volume must
		// never meet the SDK's detach-and-sweep, whose unused sweep would let
		// PVE deallocate a volume its holder owns.
		rd, resolveErr := resolveDiskForOp(ctx, deps, "detach_disk", diskCID, bareDiskCID, meta)
		if resolveErr != nil {
			return nil, resolveErr
		}
		if rd.stableID != "" {
			return nil, handleDetachStableID(ctx, deps, vmCID, vmid, rd)
		}

		node, diskID, alreadyDetached, err := detachDiskResolveSlot(ctx, deps, vmCID, vmid, bareDiskCID)
		if err != nil {
			return nil, err
		}
		if alreadyDetached {
			// Disk not on an active bus: could be a retry (disk already detached by
			// a prior attempt) or a genuinely free-floating disk. When the parked
			// strategy is active check whether the disk is already parked cluster-wide;
			// if not, resolve its node and park it now so retries converge to parked state.
			return nil, handleAlreadyDetachedParked(ctx, deps, diskCID, bareDiskCID)
		}

		// --------------------------------------------------------------------
		// 2b. Read the disk's recorded option overrides off the holder before
		//     anything mutates, so the park below can carry them into the
		//     parker's provenance entry. Fail-closed: proceeding without a
		//     read that failed transiently would silently revert an
		//     operator's update, and the Director retries a failed detach
		//     safely. Skipped under strategy "free" — nothing parks there,
		//     so the overrides have no carrier to ride and are dropped with
		//     the holder's record below.
		// --------------------------------------------------------------------
		var overlay map[string]string
		if deps.Config.DetachedDiskParkedEnabled() {
			var ovErr error
			overlay, ovErr = pve.GetVMDiskOptOverlay(ctx, deps.PVE, node, vmid, bareDiskCID)
			if ovErr != nil {
				return nil, retriableUnlessPermanent(ovErr,
					fmt.Sprintf("detach_disk: read recorded option overrides for disk %s before detach", diskCID))
			}
		}

		// --------------------------------------------------------------------
		// 3. Snapshot pre-flight guard.
		//
		// PVE rejects DetachDisk while a snapshot references the disk slot.
		// Guard before any mutating call to surface the issue with an
		// actionable message instead of an opaque PVE API error.
		//
		// Policy:
		//   HasSnapshots error + RequireSnapshotCheckPass=true  → fail-closed (abort)
		//   HasSnapshots error + RequireSnapshotCheckPass=false → WARN + proceed
		//   Snapshots present + AllowDiskOpsWithSnapshots=false → Cloud error (hard fail)
		//   Snapshots present + AllowDiskOpsWithSnapshots=true  → WARN + proceed
		//   No snapshots                                        → proceed normally
		// --------------------------------------------------------------------
		if err := detachDiskSnapshotGuard(ctx, deps, vmCID, node, vmid, diskCID, deps.Config, deps.Log(ctx)); err != nil {
			return nil, err
		}

		// --------------------------------------------------------------------
		// 4. Detach disk via SDK. Synchronous config PUT; no UPID returned.
		// --------------------------------------------------------------------
		// SDK ≥ v3.1.2 sweeps any unusedN slot PVE auto-creates on detach,
		// so the disk is fully removed from the VM config and survives a
		// subsequent delete_vm DELETE. No additional cleanup required here.
		if err := pve.RetryOnTransientOrUnplugBusy(ctx, deps.Log(ctx), "detach_disk", 0, func() error {
			return deps.PVE.QEMU().DetachDisk(ctx, node, vmid, diskID)
		}); err != nil {
			wrapped := pve.WrapError(err)
			if pve.IsNotFound(err) {
				return nil, cpierrors.VMNotFound(vmCID)
			}
			return nil, cpierrors.Wrap(wrapped, fmt.Sprintf("detach_disk: DetachDisk failed for VM %s disk %s (diskID=%s)", vmCID, diskCID, diskID))
		}

		deps.Log(ctx).Info("detach_disk",
			log.String("vm_cid", vmCID),
			log.String("disk_cid", diskCID),
			log.String("disk_id", diskID),
		)

		// --------------------------------------------------------------------
		// 4b. Remove the recorded disk_cid entry from the VM's description
		//     sentinel (mirror of attach_disk's UpdateAttachedDiskCID write).
		//     Best-effort: never fails the detach.
		// --------------------------------------------------------------------
		pve.RemoveAttachedDiskCID(ctx, deps.PVE, deps.Log(ctx), node, vmid, bareDiskCID)

		// --------------------------------------------------------------------
		// 5. Park disk when detached_disk_strategy=parked (fail-closed
		//    retriable). A park failure causes the Director to retry detach_disk;
		//    on retry the disk is free-floating so ParkDisk's idempotency check
		//    skips the IsDiskParked scan and re-parks directly.
		// --------------------------------------------------------------------
		if err := parkAfterDetach(ctx, deps, vmCID, diskCID, bareDiskCID, node, overlay); err != nil {
			return nil, err
		}

		// --------------------------------------------------------------------
		// 5b. Remove the override record from the former holder, only after
		//     the park has persisted it (or strategy "free" deliberately
		//     dropped it). Best-effort: a leftover entry names a disk this VM
		//     no longer holds and is overwritten by the disk's next attach.
		// --------------------------------------------------------------------
		pve.RemoveVMDiskOptOverlay(ctx, deps.PVE, deps.Log(ctx), node, vmid, bareDiskCID)

		return nil, nil
	})
}

// handleDetachStableID is the whole detach flow for a stable-ID disk. The
// detached state for such a disk is "parked with its serial applied", and
// every branch converges there:
//
//   - interrupted transfer → resume it (that finishes the detach);
//   - attached to this VM → transfer to a parker by reassignment;
//   - already on a parker → idempotent success;
//   - attached to another VM → stale-Director retry; warn and succeed, as the
//     legacy path does;
//   - free-floating → park it (the CID promises a parker anchor, and the
//     promise outlives a detached_disk_strategy flip — an identity disk left
//     free-floating would be one rename away from unresolvable).
//
// Fail-closed: a transfer failure is returned (retriable unless the transfer
// classified it otherwise) so the Director re-drives detach_disk, and the
// retry resumes from whatever step the failure left behind.
func handleDetachStableID(ctx context.Context, deps Deps, vmCID string, vmid int, rd resolvedDisk) error {
	logger := deps.Log(ctx)
	if rd.intent != nil {
		refreshed, err := resumeTransferIfNeeded(ctx, deps, "detach_disk", rd)
		if err != nil {
			return err
		}
		rd = refreshed
	}
	switch {
	case rd.holder == nil:
		return parkFreeFloatingStableID(ctx, deps, rd)
	case rd.holder.IsParker:
		// Already in the detached (parked) state — idempotent success.
		return nil
	case rd.holder.VMID != vmid:
		// Stale-Director retry: the disk was re-attached elsewhere after the
		// original detach. Preserve the legacy idempotent warn+nil semantics.
		logger.Warn("detach_disk: disk attached to a different VM — treating as already detached from this one",
			log.String("vm_cid", vmCID),
			log.String("disk_cid", rd.diskCID),
			log.Int("holder_vmid", rd.holder.VMID),
		)
		return nil
	}

	node := rd.holder.Node
	if err := detachDiskSnapshotGuard(ctx, deps, vmCID, node, vmid, rd.diskCID, deps.Config, logger); err != nil {
		return err
	}
	// The recorded option overrides ride the transfer inside the provenance
	// intent record, which is written strictly before the source slot is
	// deleted — so the fail-closed read here is the only added crash exposure,
	// and a failed read fails the detach retriably rather than silently
	// dropping an operator's update.
	overlay, ovErr := pve.GetVMDiskOptOverlay(ctx, deps.PVE, node, vmid, rd.stableID, rd.volid, rd.birth)
	if ovErr != nil {
		return retriableUnlessPermanent(ovErr,
			fmt.Sprintf("detach_disk: read recorded option overrides for disk %s before transfer", rd.diskCID))
	}
	pctx := pve.ParkContext{DiskCID: rd.diskCID, SourceVMCID: vmCID, StableID: rd.stableID, Opts: overlay}
	landed, transferErr := pve.TransferDiskToParker(ctx, deps.PVE, logger, node, vmid, rd.volid, parkerWriteConfigFor(deps), pctx)
	if transferErr != nil {
		return retriableUnlessPermanent(transferErr,
			fmt.Sprintf("detach_disk: transfer disk %s to parker (fail-closed: retry resumes the transfer)", rd.diskCID))
	}
	// Giving side's record last: the parker's provenance entry (the receiving
	// side) was written before the source slot was touched, so the holder
	// sentinel can now come off. Both keyings, in case an older write raced.
	pve.RemoveAttachedDiskCID(ctx, deps.PVE, logger, node, vmid, rd.stableID, rd.volid)
	pve.RemoveVMDiskOptOverlay(ctx, deps.PVE, logger, node, vmid, rd.stableID, rd.volid, rd.birth)

	logger.Info("detach_disk: disk transferred to parker",
		log.String("vm_cid", vmCID),
		log.String("disk_cid", rd.diskCID),
		log.String("volid_before", rd.volid),
		log.String("volid_after", landed),
	)
	return nil
}

// parkFreeFloatingStableID parks a free-floating stable-ID disk so a detach
// retry converges to the parked state. A volume that vanished between calls
// is idempotent success, matching the legacy already-detached path.
func parkFreeFloatingStableID(ctx context.Context, deps Deps, rd resolvedDisk) error {
	node, resolveErr := resolveNodeForDetachedDisk(ctx, deps, rd.volid)
	if resolveErr != nil {
		if cpierrors.IsType(resolveErr, cpierrors.TypeDiskNotFound) {
			return nil
		}
		return resolveErr
	}
	pctx := pve.ParkContext{DiskCID: rd.diskCID, StableID: rd.stableID}
	if parkErr := pve.ParkDisk(ctx, deps.PVE, deps.Log(ctx), node, rd.volid, parkerWriteConfigFor(deps), pctx); parkErr != nil {
		return retriableUnlessPermanent(parkErr,
			fmt.Sprintf("detach_disk: park free-floating disk %s (fail-closed)", rd.diskCID))
	}
	return nil
}

// handleAlreadyDetachedParked handles the alreadyDetached=true branch of
// HandleDetachDisk under strategy "parked". It checks whether the free-floating
// disk is already parked; if not, it re-resolves the disk's node and parks it
// so retries converge to parked state. Under "free" it returns nil before any
// cluster call: nothing parks there, so an already-detached disk is already in
// its terminal state — a disk parked earlier drains on its next attach_disk or
// delete_disk, not on a detach retry, and paying the holder sweep here would
// only expose the retry to a transient scan failure for an answer that cannot
// change the outcome. Returns nil on idempotent success (already parked or
// volume gone).
func handleAlreadyDetachedParked(ctx context.Context, deps Deps, diskCID, bareDiskCID string) error {
	if !deps.Config.DetachedDiskParkedEnabled() {
		return nil
	}
	parkerCfg := pve.ParkerConfig{
		VMIDRangeStart: deps.Config.ParkedDiskVMIDRangeStartValue(),
		VMIDRangeEnd:   deps.Config.ParkedDiskVMIDRangeEndValue(),
		DirectorID:     deps.RequestDirectorUUID,
		// DiskStorage feeds WithStorageScan on parker VMID allocation, same as
		// parkAfterDetach below -- this already-detached (retry) path reaches
		// the same createParkerVM/NextVMID allocation and must close the same
		// cross-cluster parker-VMID collision gap.
		DiskStorage: deps.Config.DiskStorage,
		// See parkerReadConfigFor: a cluster-resources row without a node is
		// dropped by the holder scan unless there is a fallback to attribute it
		// to, and a dropped row reads as "nobody holds this volume".
		FallbackNode: deps.Config.Node,
		// Always true here (the gate above), recorded for the holder scan's
		// log-level choice.
		// Same strict anchor invariant the read paths apply; see
		// ParkerConfig.AnchorStrict.
		ParkedEnabled: deps.Config.DetachedDiskParkedEnabled(),
		AnchorStrict:  deps.Config.ParkedAnchorStrictValue(),
	}
	// "Is it already parked?" and "is a real VM holding it?" are two readings of
	// one fact, and the cluster-wide sweep that establishes that fact is the
	// expensive call in this whole path — it reads the config of every VM and
	// cannot short-circuit for a free-floating disk, which is what a
	// just-detached disk is. Resolve the holder once and read both from it.
	holder, holderErr := pve.ResolveDiskHolder(ctx, deps.PVE, deps.Log(ctx), bareDiskCID, parkerCfg)
	if holderErr != nil {
		return wrapHolderScanError(holderErr,
			fmt.Sprintf("detach_disk: already-detached parker check for disk %s", diskCID))
	}
	_, _, _, isParked, parkedErr := pve.ParkedFromHolder(holder, bareDiskCID)
	if parkedErr != nil {
		return cpierrors.WrapAs(parkedErr, cpierrors.TypeRetriableCloud,
			fmt.Sprintf("detach_disk: already-detached parker check for disk %s", diskCID))
	}
	if isParked {
		// Already in parked state — idempotent success.
		return nil
	}
	// Refuse to park a disk that a real (non-parker) VM holds. This path is
	// reached on a stale-Director retry where the disk was re-attached to a
	// different VM after the original detach; parking it would double-reference
	// the volume. Preserve the old idempotent warn+nil semantics.
	if holder.Found && !holder.IsParker {
		// A holder carrying the bosh-parker tag from outside the configured band
		// is not an ordinary VM: it is a parker the band no longer recognizes,
		// and the disk on it cannot be attached or deleted until the band comes
		// back. detach_disk itself is safe either way -- the downstream guards
		// refuse -- but it is the call the operator is watching, so say so here
		// rather than leaving them to discover it at the next attach.
		if pve.TagsMarkParker(holder.Tags) {
			deps.Log(ctx).Warn("detach_disk: disk is held by a bosh-parker VM outside the configured parker band; "+
				"it stays there and attach_disk and delete_disk will refuse it until "+
				"parked_disk_vmid_range_start/end cover that VMID again",
				log.String("disk_cid", diskCID),
				log.Int("holder_vmid", holder.VMID),
			)
			return nil
		}
		deps.Log(ctx).Warn("detach_disk: disk attached to a non-parker VM — skipping park (idempotent)",
			log.String("disk_cid", diskCID),
			log.Int("holder_vmid", holder.VMID),
		)
		return nil
	}
	// Free-floating disk + strategy=parked: re-resolve node and park.
	alreadyDetachedNode, resolveErr := resolveNodeForDetachedDisk(ctx, deps, bareDiskCID)
	if resolveErr != nil {
		// Volume gone between detach and re-park attempt — idempotent success.
		if cpierrors.IsType(resolveErr, cpierrors.TypeDiskNotFound) {
			return nil
		}
		return resolveErr
	}
	if parkErr := pve.ParkDisk(ctx, deps.PVE, deps.Log(ctx), alreadyDetachedNode, bareDiskCID, parkerCfg, pve.ParkContext{DiskCID: diskCID}); parkErr != nil {
		// Keep the class the park chose. A 403 on creating bosh-parker-*, an
		// exhausted VMID band, and an unswept reference are all permanent and
		// each names what to do; relabelling them retriable hides the message
		// behind a Director retry loop that cannot end.
		return retriableUnlessPermanent(parkErr,
			fmt.Sprintf("detach_disk: park free-floating disk %s (fail-closed)", diskCID))
	}
	return nil
}

// detachDiskSnapshotGuard runs the snapshot pre-flight check for detach_disk.
// Returns nil when safe to proceed; returns a Cloud or wrapped error otherwise.
//
// Policy:
//
//	HasSnapshots error + cfg.RequireSnapshotCheckPass  → Wrap error (fail-closed)
//	HasSnapshots error + !cfg.RequireSnapshotCheckPass → nil (WARN + proceed)
//	snapshots present + !cfg.AllowDiskOpsWithSnapshots → SnapshotBlocked error
//	snapshots present + cfg.AllowDiskOpsWithSnapshots  → nil (WARN + proceed)
//	no snapshots                                       → nil
func detachDiskSnapshotGuard(ctx context.Context, deps Deps, vmCID, node string, vmid int, diskCID string, cfg *config.CPIConfig, logger *log.Logger) error {
	snapNames, snapErr := pve.HasSnapshots(ctx, deps.PVE, node, vmid)
	if snapErr != nil {
		if cfg.RequireSnapshotCheckPass {
			return cpierrors.Wrap(snapErr,
				"detach_disk: snapshot pre-flight check failed and require_snapshot_check_pass is set",
			)
		}
		logger.Warn("detach_disk: snapshot pre-flight check failed — proceeding (fail-open)",
			log.String("node", node),
			log.Int("vmid", vmid),
			log.Err(snapErr),
		)
		return nil
	}
	if len(snapNames) == 0 {
		return nil
	}
	if cfg.AllowDiskOpsWithSnapshots {
		logger.Warn("detach_disk: proceeding despite snapshots (allow_disk_ops_with_snapshots=true)",
			log.String("vm_cid", vmCID),
			log.String("node", node),
			log.String("snapshots", strings.Join(snapNames, ", ")),
		)
		return nil
	}
	return cpierrors.SnapshotBlocked(
		"detach_disk: VM %s (node %s) has %d snapshot(s) [%s] that reference disk %s."+
			" PVE will reject detach while snapshots reference this disk."+
			" Delete snapshot(s) [%s] first, then retry detach_disk;"+
			" or set pve.allow_disk_ops_with_snapshots=true to bypass this guard.",
		vmCID, node, len(snapNames), strings.Join(snapNames, ", "),
		diskCID, strings.Join(snapNames, ", "),
	)
}

// parkAfterDetach parks bareDiskCID onto a parker VM after a successful
// DetachDisk call when detached_disk_strategy=parked. A no-op when the parked
// strategy is not enabled. Failure is fail-closed retriable: the Director
// retries detach_disk; on retry the disk is free-floating so ParkDisk's
// idempotency check skips the IsDiskParked scan and re-parks directly.
//
// overlay is the disk's recorded option-override map, read off the holder
// before the detach; the provenance entry carries it across the park. The
// provenance write itself stays best-effort, so a park that succeeds while
// its provenance write fails loses the overrides — narrow (one description
// write after a successful attach) and logged by the provenance writer.
func parkAfterDetach(ctx context.Context, deps Deps, vmCID, diskCID, bareDiskCID, node string, overlay map[string]string) error {
	if !deps.Config.DetachedDiskParkedEnabled() {
		return nil
	}
	parkerCfg := pve.ParkerConfig{
		VMIDRangeStart: deps.Config.ParkedDiskVMIDRangeStartValue(),
		VMIDRangeEnd:   deps.Config.ParkedDiskVMIDRangeEndValue(),
		DirectorID:     deps.RequestDirectorUUID,
		// DiskStorage feeds WithStorageScan on parker VMID allocation so a
		// VMID whose number is already claimed by orphaned volumes on the
		// disk storage is skipped (same guard create_vm applies).
		DiskStorage: deps.Config.DiskStorage,
		// Always true here (the gate above), recorded for the holder scan's
		// log-level choice.
		// Same strict anchor invariant the read paths apply; see
		// ParkerConfig.AnchorStrict.
		ParkedEnabled: deps.Config.DetachedDiskParkedEnabled(),
		AnchorStrict:  deps.Config.ParkedAnchorStrictValue(),
	}
	pctx := pve.ParkContext{DiskCID: diskCID, SourceVMCID: vmCID, Opts: overlay}
	if parkErr := pve.ParkDisk(ctx, deps.PVE, deps.Log(ctx), node, bareDiskCID, parkerCfg, pctx); parkErr != nil {
		// Same reasoning as the free-floating park above: the class the park
		// chose is the one the Director should see.
		return retriableUnlessPermanent(parkErr,
			fmt.Sprintf("detach_disk: park disk %s after detach (fail-closed: retry will re-park)", diskCID))
	}
	return nil
}

// resolveNodeForDetachedDisk locates the PVE node holding bareDiskCID using the
// storage backend. Used when the disk is free-floating (not attached to any VM)
// and the handler needs a node to pass to ParkDisk.
//
// Mirrors the backend-resolution step in detachDiskResolveSlot but returns only
// the node; caller already confirmed the disk exists (volume-not-found is
// treated as retriable here since the just-completed detach should have left it
// present on a node).
func resolveNodeForDetachedDisk(ctx context.Context, deps Deps, bareDiskCID string) (string, error) {
	storage, _, err := pve.ParseDiskCID(bareDiskCID)
	if err != nil {
		return "", cpierrors.DiskNotFound(bareDiskCID)
	}
	backend, err := backendResolverOrDefault(deps).Resolve(ctx, storage)
	if err != nil {
		return "", cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud,
			fmt.Sprintf("detach_disk: backend resolution for free-floating disk %s", bareDiskCID))
	}
	node, err := backend.NodeForExisting(ctx, bareDiskCID)
	if err != nil {
		if pve.IsNotFound(err) {
			// Volume disappeared between detach and park — not retriable, treat as success.
			return "", cpierrors.DiskNotFound(bareDiskCID)
		}
		return "", cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud,
			fmt.Sprintf("detach_disk: node lookup for free-floating disk %s", bareDiskCID))
	}
	return node, nil
}

// sweepUnusedDiskSlot removes a lingering unusedN config entry that still
// references diskCID on the given VM, returning true when a slot was removed.
//
// PVE demotes a detached disk to unusedN and the SDK's DetachDisk normally
// removes that slot in the same call. That sweep is rejected while a snapshot
// references the disk (the allow_disk_ops_with_snapshots bypass path), so the
// slot can linger. ResolveDiskID does not see unusedN slots (PVE's disk-key
// pattern covers only active buses), so a retried detach_disk reaches here once
// the snapshot is gone and completes the cleanup. PUT delete=unusedN frees the
// reference so delete_disk can remove the volume and delete_vm is unblocked.
func sweepUnusedDiskSlot(
	ctx context.Context, deps Deps, node string, vmid int, vmCID, diskCID string,
) (bool, error) {
	cfg, err := deps.PVE.QEMU().Config(ctx, node, vmid)
	if err != nil {
		if pve.IsNotFound(err) {
			return false, nil // VM gone — nothing to sweep.
		}
		return false, cpierrors.Wrap(pve.WrapError(err), fmt.Sprintf("detach_disk: read config for VM %s to sweep unused slot", vmCID))
	}

	for slot, volid := range pve.FindUnusedDiskEntries(cfg) {
		if volid != diskCID {
			continue
		}
		if sweepErr := pve.RetryOnTransient(ctx, deps.Log(ctx), "detach_disk.sweep", 0, func() error {
			return deps.PVE.QEMU().DetachDisk(ctx, node, vmid, slot)
		}); sweepErr != nil {
			if pve.IsNotFound(sweepErr) {
				return true, nil // slot already gone
			}
			return false, cpierrors.Wrap(pve.WrapError(sweepErr), fmt.Sprintf("detach_disk: remove lingering unused slot %s for disk %s on VM %s", slot, diskCID, vmCID))
		}
		deps.Log(ctx).Info("detach_disk: removed lingering unused disk slot",
			log.String("vm_cid", vmCID),
			log.String("slot", slot),
			log.String("disk_cid", diskCID),
		)
		return true, nil
	}
	return false, nil
}
