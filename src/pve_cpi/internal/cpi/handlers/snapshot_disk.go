// Package handlers contains the 22 BOSH CPI v2 method implementations.
package handlers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
)

// HandleSnapshotDisk returns a Handler for the BOSH CPI snapshot_disk method.
//
// Arguments (positional JSON array):
//
//	[0] disk_cid  string         — disk CID of the form "<storage>:<volume>"
//	[1] metadata  map[string]any — optional snapshot metadata; "description" key
//	                               is forwarded to PVE as the snapshot description.
//
// Returns: snapshot_cid string of the form "<vmid>:<snap_name>".
//
// PVE has no per-disk snapshot primitive. Snapshots must target the VM that
// holds the disk. This handler locates the VM by scanning cluster resources
// for a VM whose config contains the disk volid, then snapshots that VM.
//
// If the disk is not attached to any VM, the handler returns a CloudError.
func HandleSnapshotDisk(deps Deps) Handler {
	return HandlerFunc(func(ctx context.Context, args []json.RawMessage, _ jsonrpc.Context) (any, error) {
		// ----------------------------------------------------------------
		// 1. Unmarshal and validate arguments.
		// ----------------------------------------------------------------
		if len(args) < 1 {
			return nil, fmt.Errorf("snapshot_disk: expected at least 1 argument (disk_cid), got 0")
		}

		var diskCID string
		if err := json.Unmarshal(args[0], &diskCID); err != nil {
			return nil, fmt.Errorf("snapshot_disk: args[0] disk_cid must be a string: %w", err)
		}
		if diskCID == "" {
			return nil, fmt.Errorf("snapshot_disk: args[0] disk_cid must not be empty")
		}

		// metadata arg is optional and may be null or absent.
		var metadata map[string]any
		if len(args) >= 2 && args[1] != nil {
			_ = json.Unmarshal(args[1], &metadata)
		}

		// ----------------------------------------------------------------
		// 2. Parse disk_cid → storage + volid components.
		// ----------------------------------------------------------------
		if _, _, err := pve.ParseDiskCID(diskCID); err != nil {
			return nil, cpierrors.DiskNotFound(diskCID)
		}

		// ----------------------------------------------------------------
		// 3. Locate the VM hosting this disk by scanning cluster resources.
		//    Returns the VMID and its current node — snapshot must target the
		//    VM's actual node, which may differ from Config.Node in multi-node
		//    deployments. Local-storage disks require both VM and disk on the
		//    same node, which is implicit in the cluster scan result. PVE VM
		//    config stores disk values in canonical "<storage>:<volname>"
		//    form — the disk_cid is that form.
		// ----------------------------------------------------------------
		vmid, node, err := pve.FindVMByDiskVolid(ctx, deps.PVE, deps.Config.Node, diskCID)
		if err != nil {
			return nil, err
		}

		// ----------------------------------------------------------------
		// 4. Generate a unique snapshot name: bosh-<timestamp>-<hex4>.
		// ----------------------------------------------------------------
		snapName, err := generateSnapName()
		if err != nil {
			return nil, fmt.Errorf("snapshot_disk: failed to generate snapshot name: %w", err)
		}

		// ----------------------------------------------------------------
		// 5. Build snapshot opts; forward description if present in metadata.
		// ----------------------------------------------------------------
		snapOpts := map[string]any{}
		if desc, ok := metadata["description"].(string); ok && desc != "" {
			snapOpts["description"] = desc
		}

		// ----------------------------------------------------------------
		// 6-7. Take VM snapshot via SDK and await its task. PVE snapshot
		// operations on zfs/lvm/btrfs storages take the per-storage lock,
		// so retry the submit+await pair on IsStorageLockTimeout.
		// ----------------------------------------------------------------
		serr := pve.RetryOnTransientOrLock(ctx, deps.Logger, "snapshot_disk", 0, func() error {
			upid, e := deps.PVE.QEMU().Snapshot(ctx, node, vmid, snapName, snapOpts)
			if e != nil {
				return e
			}
			if upid == "" {
				return nil
			}
			return pve.AwaitTaskWithLogger(ctx, deps.PVE, node, upid, deps.Logger)
		})
		if serr != nil {
			deps.Logger.Error("snapshot_disk: Snapshot failed",
				log.String("disk_cid", diskCID),
				log.Int("vmid", vmid),
				log.String("snap_name", snapName),
				log.Err(serr),
			)
			return nil, fmt.Errorf("snapshot_disk: Snapshot failed for VM %d disk %s: %w", vmid, diskCID, pve.WrapError(serr))
		}

		// ----------------------------------------------------------------
		// 8. Return snapshot_cid = "<vmid>:<snap_name>".
		// ----------------------------------------------------------------
		snapshotCID := pve.FormatSnapshotCID(strconv.Itoa(vmid), snapName)

		deps.Logger.Info("snapshot_disk",
			log.String("disk_cid", diskCID),
			log.Int("vmid", vmid),
			log.String("snap_name", snapName),
			log.String("snapshot_cid", snapshotCID),
		)

		return snapshotCID, nil
	})
}

// generateSnapName returns a snapshot name of the form "bosh-<timestamp>-<hex4>".
// The timestamp is seconds since Unix epoch. The 4-byte random suffix reduces
// collision probability for concurrent snapshot calls on the same VM.
func generateSnapName() (string, error) {
	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return fmt.Sprintf("bosh-%d-%s", time.Now().Unix(), hex.EncodeToString(buf)), nil
}
