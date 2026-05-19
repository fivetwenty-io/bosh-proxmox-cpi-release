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
//  2. Resolve the disk's current slot (diskID, e.g. "scsi2") via pve.ResolveDiskID
//     by fetching the VM config and scanning for the matching volid.
//  3. If the disk is not attached (ResolveDiskID returns a not-found error), log a
//     warning and return nil (idempotent — Director may call detach_disk more than
//     once for the same disk).
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
			return nil, fmt.Errorf("detach_disk: expected 2 arguments (vm_cid, disk_cid), got %d", len(args))
		}

		var vmCID string
		if err := json.Unmarshal(args[0], &vmCID); err != nil {
			return nil, fmt.Errorf("detach_disk: args[0] vm_cid must be a string: %w", err)
		}
		if vmCID == "" {
			return nil, fmt.Errorf("detach_disk: args[0] vm_cid must not be empty")
		}

		var diskCID string
		if err := json.Unmarshal(args[1], &diskCID); err != nil {
			return nil, fmt.Errorf("detach_disk: args[1] disk_cid must be a string: %w", err)
		}
		if diskCID == "" {
			return nil, fmt.Errorf("detach_disk: args[1] disk_cid must not be empty")
		}

		// --------------------------------------------------------------------
		// 2. Parse vm_cid → VMID; parse disk_cid → storage + volid.
		// --------------------------------------------------------------------
		vmid, err := strconv.Atoi(vmCID)
		if err != nil || vmid <= 0 {
			return nil, cpierrors.VMNotFound(vmCID)
		}

		storage, volid, err := pve.ParseDiskCID(diskCID)
		if err != nil {
			return nil, cpierrors.DiskNotFound(diskCID)
		}

		// detach_disk modifies VM config, which lives on the VM's node — which
		// is the same node that holds the volume (the disk is attached). Resolve
		// via the storage backend to share behavior with attach_disk.
		backend, err := backendResolverOrDefault(deps).Resolve(ctx, storage)
		if err != nil {
			return nil, fmt.Errorf("detach_disk: backend resolution failed for storage %q: %w", storage, err)
		}
		node, err := backend.NodeForExisting(ctx, volid)
		if err != nil {
			if pve.IsNotFound(err) {
				// Volume gone → disk already detached from its perspective; idempotent.
				deps.Logger.Warn("detach_disk: volume not found on any node, treating as already detached",
					log.String("vm_cid", vmCID),
					log.String("disk_cid", diskCID),
				)
				return nil, nil
			}
			return nil, fmt.Errorf("detach_disk: %w", err)
		}

		// --------------------------------------------------------------------
		// 3. Resolve diskID from current VM config.
		//    If not attached: log warning, return nil (idempotent).
		// --------------------------------------------------------------------
		diskID, err := pve.ResolveDiskID(ctx, deps.PVE, node, vmid, volid)
		if err != nil {
			if cpierrors.IsType(err, cpierrors.TypeCloud) || pve.IsNotFound(err) {
				// Disk is not attached to this VM; idempotent success.
				deps.Logger.Warn("detach_disk: disk not attached to VM; skipping",
					log.String("vm_cid", vmCID),
					log.String("disk_cid", diskCID),
					log.String("volid", volid),
					log.Err(err),
				)
				return nil, nil
			}
			// Config fetch error (network, 404 on VM itself, etc.).
			return nil, pve.WrapError(err)
		}

		// --------------------------------------------------------------------
		// 4. Detach disk via SDK. Synchronous config PUT; no UPID returned.
		// --------------------------------------------------------------------
		// SDK ≥ v3.1.2 sweeps any unusedN slot PVE auto-creates on detach,
		// so the disk is fully removed from the VM config and survives a
		// subsequent delete_vm DELETE. No additional cleanup required here.
		if err := deps.PVE.QEMU().DetachDisk(ctx, node, vmid, diskID); err != nil {
			wrapped := pve.WrapError(err)
			if pve.IsNotFound(err) {
				return nil, cpierrors.VMNotFound(vmCID)
			}
			return nil, fmt.Errorf("detach_disk: DetachDisk failed for VM %s disk %s (diskID=%s): %w",
				vmCID, diskCID, diskID, wrapped)
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
