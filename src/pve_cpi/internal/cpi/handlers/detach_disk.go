package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

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

		// --------------------------------------------------------------------
		// 2. Parse vm_cid → VMID; resolve node + disk slot via helper.
		// --------------------------------------------------------------------
		vmid, err := strconv.Atoi(vmCID)
		if err != nil || vmid <= 0 {
			return nil, cpierrors.VMNotFound(vmCID)
		}

		node, diskID, alreadyDetached, err := detachDiskResolveSlot(ctx, deps, vmCID, vmid, diskCID)
		if err != nil {
			return nil, err
		}
		if alreadyDetached {
			return nil, nil
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
		if snapNames, snapErr := pve.HasSnapshots(ctx, deps.PVE, node, vmid); snapErr != nil {
			if deps.Config.RequireSnapshotCheckPass {
				return nil, cpierrors.Wrap(snapErr,
					"detach_disk: snapshot pre-flight check failed and require_snapshot_check_pass is set",
				)
			}
			deps.Logger.Warn("detach_disk: snapshot pre-flight check failed — proceeding (fail-open)",
				log.String("node", node),
				log.Int("vmid", vmid),
				log.Err(snapErr),
			)
		} else if len(snapNames) > 0 {
			if deps.Config.AllowDiskOpsWithSnapshots {
				deps.Logger.Warn("detach_disk: proceeding despite snapshots (allow_disk_ops_with_snapshots=true)",
					log.String("vm_cid", vmCID),
					log.String("node", node),
					log.String("snapshots", strings.Join(snapNames, ", ")),
				)
			} else {
				return nil, cpierrors.Cloud(
					"detach_disk: VM %s (node %s) has %d snapshot(s) [%s] that reference disk %s."+
						" PVE will reject detach while snapshots reference this disk."+
						" Delete snapshot(s) [%s] first, then retry detach_disk;"+
						" or set pve.allow_disk_ops_with_snapshots=true to bypass this guard.",
					vmCID, node, len(snapNames), strings.Join(snapNames, ", "),
					diskCID, strings.Join(snapNames, ", "),
				)
			}
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
		// 5. Return nil (void success).
		// --------------------------------------------------------------------
		return nil, nil
	})
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
