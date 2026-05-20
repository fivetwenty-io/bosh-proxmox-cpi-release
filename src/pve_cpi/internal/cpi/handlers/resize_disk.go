// Package handlers contains the 22 BOSH CPI v2 method implementations.
package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
)

// HandleResizeDisk returns a Handler for the BOSH CPI resize_disk method.
//
// Arguments (positional JSON array):
//
//	[0] disk_cid     string — disk CID of the form "<storage>:<volume>"
//	[1] new_size_mb  int    — new total disk size in MiB (must be > 0)
//
// Returns: null on success (BOSH void method).
//
// Logic:
//  1. Parse disk_cid and locate the attached VM + diskID.
//  2. Read current disk size from VM config (parsed from the disk option string).
//  3. Compute delta GiB. Shrink (delta < 0) is not supported. No-op (delta == 0) returns nil.
//  4. Call ResizeDisk with the positive delta in GiB (PVE additive "+NG" format).
//  5. Await the returned task UPID.
//
// Size conversion: BOSH provides MiB; PVE ResizeDisk accepts GiB deltas. The delta
// is computed as ceiling(new_size_mb / 1024) - current_size_gib. If current_size_gib
// cannot be determined from the config, the handler returns an error.
//
// Shrink is not supported: PVE does not support disk shrink via the resize API.
// Attempting to shrink returns a NotSupported error.
func HandleResizeDisk(deps Deps) Handler {
	return HandlerFunc(func(ctx context.Context, args []json.RawMessage, _ jsonrpc.Context) (any, error) {
		// ----------------------------------------------------------------
		// 1. Unmarshal and validate arguments.
		// ----------------------------------------------------------------
		if len(args) < 2 {
			return nil, fmt.Errorf("resize_disk: expected 2 arguments (disk_cid, new_size_mb), got %d", len(args))
		}

		var diskCID string
		if err := json.Unmarshal(args[0], &diskCID); err != nil {
			return nil, fmt.Errorf("resize_disk: args[0] disk_cid must be a string: %w", err)
		}
		if diskCID == "" {
			return nil, fmt.Errorf("resize_disk: args[0] disk_cid must not be empty")
		}

		var newSizeMB int
		if err := json.Unmarshal(args[1], &newSizeMB); err != nil {
			return nil, fmt.Errorf("resize_disk: args[1] new_size_mb must be an integer: %w", err)
		}
		if newSizeMB <= 0 {
			return nil, fmt.Errorf("resize_disk: new_size_mb must be > 0, got %d", newSizeMB)
		}

		// ----------------------------------------------------------------
		// 2. Parse disk_cid → volid.
		// ----------------------------------------------------------------
		if _, _, err := pve.ParseDiskCID(diskCID); err != nil {
			return nil, cpierrors.DiskNotFound(diskCID)
		}

		// ----------------------------------------------------------------
		// 3. Locate attached VM + its current node, then resolve diskID.
		//    FindVMByDiskVolid scans /cluster/resources so resize works in
		//    multi-node deployments without depending on Config.Node matching
		//    the VM's node. PVE VM config stores disk values in canonical
		//    "<storage>:<volname>" form — the disk_cid is that form.
		// ----------------------------------------------------------------
		vmid, node, err := pve.FindVMByDiskVolid(ctx, deps.PVE, deps.Config.Node, diskCID)
		if err != nil {
			return nil, err
		}

		diskID, err := pve.ResolveDiskID(ctx, deps.PVE, node, vmid, diskCID)
		if err != nil {
			// ResolveDiskID returns CloudError when disk not attached.
			return nil, fmt.Errorf("resize_disk: cannot resolve diskID for %s on VM %d: %w", diskCID, vmid, err)
		}

		// ----------------------------------------------------------------
		// 4. Read current size from VM config disk option string.
		//    PVE stores disk config as: "<volid>[,size=<N>G][,<other-opts>]"
		//    The size field may be absent on very old disks; if absent, we
		//    cannot determine the delta safely and return an error.
		// ----------------------------------------------------------------
		cfg, err := deps.PVE.QEMU().Config(ctx, node, vmid)
		if err != nil {
			return nil, fmt.Errorf("resize_disk: failed to read VM %d config: %w", vmid, err)
		}

		diskOptStr, ok := cfg[diskID].(string)
		if !ok || diskOptStr == "" {
			return nil, fmt.Errorf("resize_disk: disk %q not found in VM %d config", diskID, vmid)
		}

		currentGiB, parseErr := parseDiskSizeGiB(diskOptStr)
		if parseErr != nil {
			return nil, fmt.Errorf("resize_disk: cannot determine current size of disk %s: %w", diskCID, parseErr)
		}

		// ----------------------------------------------------------------
		// 5. Compute delta in GiB (ceiling of new_size_mb / 1024).
		// ----------------------------------------------------------------
		newGiB := (newSizeMB + 1023) / 1024
		deltaGiB := newGiB - currentGiB

		if deltaGiB < 0 {
			return nil, cpierrors.NotSupported(
				"resize_disk",
				fmt.Sprintf(
					"shrink not supported by PVE (current %d GiB, requested %d GiB via %d MiB)",
					currentGiB, newGiB, newSizeMB,
				),
			)
		}

		if deltaGiB == 0 {
			deps.Logger.Info("resize_disk: no-op, disk already at or above requested size",
				log.String("disk_cid", diskCID),
				log.Int("current_gib", currentGiB),
				log.Int("new_size_mb", newSizeMB),
			)
			return nil, nil
		}

		// ----------------------------------------------------------------
		// 6-7. Submit ResizeDisk and await its task. PVE's qemu-img resize
		// runs under the per-storage lockfile, so on bursty deploys the
		// task can fail with "can't lock file ... got timeout". Retry the
		// submit+await pair on that signal; non-lock errors propagate.
		// ----------------------------------------------------------------
		rerr := pve.RetryOnStorageLock(ctx, deps.Logger, "resize_disk", 0, func() error {
			upid, e := deps.PVE.QEMU().ResizeDisk(ctx, node, vmid, diskID, deltaGiB)
			if e != nil {
				return e
			}
			if upid == "" {
				return nil
			}
			return pve.AwaitTaskWithLogger(ctx, deps.PVE, node, upid, deps.Logger)
		})
		if rerr != nil {
			return nil, fmt.Errorf("resize_disk: ResizeDisk failed for VM %d disk %s (+%dG): %w",
				vmid, diskCID, deltaGiB, pve.WrapError(rerr))
		}

		deps.Logger.Info("resize_disk",
			log.String("disk_cid", diskCID),
			log.Int("vmid", vmid),
			log.String("disk_id", diskID),
			log.Int("current_gib", currentGiB),
			log.Int("delta_gib", deltaGiB),
			log.Int("new_gib", currentGiB+deltaGiB),
		)

		return nil, nil
	})
}

// parseDiskSizeGiB extracts the numeric GiB value from a PVE disk option string.
// The option string format is: "<volid>[,key=value,...]" where one key may be
// "size=<N>G". For example:
//
//	"local-lvm:vm-100-disk-1,size=10G,cache=writeback" → 10
//	"local-lvm:vm-100-disk-1"                          → error (size not present)
//
// Only the "G" suffix (GiB) is handled. PVE stores disk sizes in GiB for
// QEMU VMs. Sizes in other units (M, T, P) are rejected with an error.
func parseDiskSizeGiB(optStr string) (int, error) {
	for _, part := range strings.Split(optStr, ",") {
		if !strings.HasPrefix(part, "size=") {
			continue
		}
		sizeVal := strings.TrimPrefix(part, "size=")
		if strings.HasSuffix(sizeVal, "G") {
			n, err := strconv.Atoi(strings.TrimSuffix(sizeVal, "G"))
			if err != nil {
				return 0, fmt.Errorf("cannot parse size value %q: %w", sizeVal, err)
			}
			return n, nil
		}
		return 0, fmt.Errorf("unsupported size unit in %q (only GiB supported)", sizeVal)
	}
	return 0, fmt.Errorf("size option not found in disk option string %q", optStr)
}
