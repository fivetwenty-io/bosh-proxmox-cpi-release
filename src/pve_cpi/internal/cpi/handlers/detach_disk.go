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
			deps.Logger.Warn("detach_disk: volume not found on any node, treating as already detached",
				log.String("vm_cid", vmCID),
				log.String("disk_cid", diskCID),
			)
			return "", "", true, nil
		}
		return "", "", false, cpierrors.Wrap(err, "detach_disk: node lookup failed")
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
				deps.Logger.Warn("detach_disk: disk not attached to VM; skipping",
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
//	[1] disk_cid string — persistent disk CID in "<storage>:<volid>" form
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
//  5. Return nil (void success).
//
// Idempotency: if the disk is not attached to the VM at step 2, the handler returns
// nil without calling DetachDisk. This matches the Perl CPI's "warn + return 1"
// behaviour and satisfies the BOSH Director's expectation that repeated detach_disk
// calls on an already-detached disk succeed without error.
//
// The disk volume is NOT deleted from storage; that is handled by delete_disk.
func HandleDetachDisk(deps Deps) Handler {
	return HandlerFunc(func(ctx context.Context, args []json.RawMessage, _ jsonrpc.Context) (any, error) {
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
		bareDiskCID, _, decErr := pve.ParseEncodedDiskCID(diskCID)
		if decErr != nil {
			return nil, cpierrors.DiskNotFound(diskCID)
		}

		// --------------------------------------------------------------------
		// 2. Parse vm_cid → VMID; resolve node + disk slot via helper.
		// --------------------------------------------------------------------
		vmid, err := strconv.Atoi(vmCID)
		if err != nil || vmid <= 0 {
			return nil, cpierrors.VMNotFound(vmCID)
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
		if err := detachDiskSnapshotGuard(ctx, deps, vmCID, node, vmid, diskCID, deps.Config, deps.Logger); err != nil {
			return nil, err
		}

		// --------------------------------------------------------------------
		// 4. Detach disk via SDK. Synchronous config PUT; no UPID returned.
		// --------------------------------------------------------------------
		// SDK ≥ v3.1.2 sweeps any unusedN slot PVE auto-creates on detach,
		// so the disk is fully removed from the VM config and survives a
		// subsequent delete_vm DELETE. No additional cleanup required here.
		if err := pve.RetryOnTransient(ctx, deps.Logger, "detach_disk", 0, func() error {
			return deps.PVE.QEMU().DetachDisk(ctx, node, vmid, diskID)
		}); err != nil {
			wrapped := pve.WrapError(err)
			if pve.IsNotFound(err) {
				return nil, cpierrors.VMNotFound(vmCID)
			}
			return nil, cpierrors.Wrap(wrapped, fmt.Sprintf("detach_disk: DetachDisk failed for VM %s disk %s (diskID=%s)", vmCID, diskCID, diskID))
		}

		deps.Logger.Info("detach_disk",
			log.String("vm_cid", vmCID),
			log.String("disk_cid", diskCID),
			log.String("disk_id", diskID),
		)

		// --------------------------------------------------------------------
		// 5. Park disk when detached_disk_strategy=parked (fail-closed
		//    retriable). A park failure causes the Director to retry detach_disk;
		//    on retry the disk is free-floating so ParkDisk's idempotency check
		//    skips the IsDiskParked scan and re-parks directly.
		// --------------------------------------------------------------------
		if err := parkAfterDetach(ctx, deps, vmCID, diskCID, bareDiskCID, node); err != nil {
			return nil, err
		}

		return nil, nil
	})
}

// handleAlreadyDetachedParked handles the alreadyDetached=true branch of
// HandleDetachDisk when the parked strategy is active. It checks whether the
// free-floating disk is already parked; if not and DetachedDiskParkedEnabled is
// set, it re-resolves the disk's node and parks it so retries converge to parked
// state. Returns nil on idempotent success (already parked, volume gone, or
// strategy unset).
func handleAlreadyDetachedParked(ctx context.Context, deps Deps, diskCID, bareDiskCID string) error {
	if !deps.Config.ParkedStrategyActive() {
		return nil
	}
	parkerCfg := pve.ParkerConfig{
		VMIDRangeStart: deps.Config.ParkedDiskVMIDRangeStartValue(),
		VMIDRangeEnd:   deps.Config.ParkedDiskVMIDRangeEndValue(),
		DirectorID:     deps.Config.StemcellDirectorID(),
	}
	_, _, _, isParked, parkedErr := pve.IsDiskParked(ctx, deps.PVE, deps.Logger, bareDiskCID, parkerCfg)
	if parkedErr != nil {
		return cpierrors.WrapAs(parkedErr, cpierrors.TypeRetriableCloud,
			fmt.Sprintf("detach_disk: already-detached parker check for disk %s", diskCID))
	}
	if isParked {
		// Already in parked state — idempotent success.
		return nil
	}
	if !deps.Config.DetachedDiskParkedEnabled() {
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
	if parkErr := pve.ParkDisk(ctx, deps.PVE, deps.Logger, alreadyDetachedNode, bareDiskCID, parkerCfg, pve.ParkContext{DiskCID: diskCID}); parkErr != nil {
		return cpierrors.WrapAs(parkErr, cpierrors.TypeRetriableCloud,
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
func parkAfterDetach(ctx context.Context, deps Deps, vmCID, diskCID, bareDiskCID, node string) error {
	if !deps.Config.DetachedDiskParkedEnabled() {
		return nil
	}
	parkerCfg := pve.ParkerConfig{
		VMIDRangeStart: deps.Config.ParkedDiskVMIDRangeStartValue(),
		VMIDRangeEnd:   deps.Config.ParkedDiskVMIDRangeEndValue(),
		DirectorID:     deps.Config.StemcellDirectorID(),
	}
	pctx := pve.ParkContext{DiskCID: diskCID, SourceVMCID: vmCID}
	if parkErr := pve.ParkDisk(ctx, deps.PVE, deps.Logger, node, bareDiskCID, parkerCfg, pctx); parkErr != nil {
		return cpierrors.WrapAs(parkErr, cpierrors.TypeRetriableCloud,
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
		if sweepErr := pve.RetryOnTransient(ctx, deps.Logger, "detach_disk.sweep", 0, func() error {
			return deps.PVE.QEMU().DetachDisk(ctx, node, vmid, slot)
		}); sweepErr != nil {
			if pve.IsNotFound(sweepErr) {
				return true, nil // slot already gone
			}
			return false, cpierrors.Wrap(pve.WrapError(sweepErr), fmt.Sprintf("detach_disk: remove lingering unused slot %s for disk %s on VM %s", slot, diskCID, vmCID))
		}
		deps.Logger.Info("detach_disk: removed lingering unused disk slot",
			log.String("vm_cid", vmCID),
			log.String("slot", slot),
			log.String("disk_cid", diskCID),
		)
		return true, nil
	}
	return false, nil
}
