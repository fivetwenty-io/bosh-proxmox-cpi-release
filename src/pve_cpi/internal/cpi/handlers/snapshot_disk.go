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
			return nil, cpierrors.Cloud("snapshot_disk: expected at least 1 argument (disk_cid), got 0")
		}

		var diskCID string
		if err := json.Unmarshal(args[0], &diskCID); err != nil {
			return nil, cpierrors.Wrap(err, "snapshot_disk: args[0] disk_cid must be a string")
		}
		if diskCID == "" {
			return nil, cpierrors.Cloud("snapshot_disk: args[0] disk_cid must not be empty")
		}
		// Strip optional metadata suffix before any PVE API or storage lookup.
		bareDiskCID, _, decErr := pve.ParseEncodedDiskCID(diskCID)
		if decErr != nil {
			return nil, cpierrors.DiskNotFound(diskCID)
		}

		// metadata arg is optional and may be null or absent.
		var metadata map[string]any
		if len(args) >= 2 && args[1] != nil {
			_ = json.Unmarshal(args[1], &metadata)
		}

		// ----------------------------------------------------------------
		// 2. Parse disk_cid → storage + volid components.
		// ----------------------------------------------------------------
		if _, _, err := pve.ParseDiskCID(bareDiskCID); err != nil {
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
		vmid, node, err := pve.FindVMByDiskVolid(ctx, deps.PVE, deps.Config.Node, bareDiskCID)
		if err != nil {
			return nil, err
		}

		// ----------------------------------------------------------------
		// 4. Generate a unique snapshot name: bosh-<timestamp>-<hex4>.
		// ----------------------------------------------------------------
		snapName, err := generateSnapName()
		if err != nil {
			return nil, cpierrors.Wrap(err, "snapshot_disk: generate snapshot name")
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
			return nil, cpierrors.Wrap(pve.WrapError(serr), fmt.Sprintf("snapshot_disk: Snapshot failed for VM %d disk %s", vmid, diskCID))
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
//
// PVE snapshot name constraints (PVE 8.x API schema):
//
//	Pattern: [a-zA-Z][a-zA-Z0-9_-]{0,39}  (max 40 chars, must start with a letter)
//
// The generated format "bosh-<unix_ts>-<8hex>" is at most 25 chars, starts
// with 'b' (a letter), and uses only alphanumeric characters plus hyphens —
// all within the allowed set.
//
// Hyphen compatibility note: PVE 8.x permits hyphens in snapshot names.
// PVE 7.x and earlier have inconsistent hyphen support depending on the
// storage backend; underscore separators are universally safe across all
// PVE versions. This implementation keeps hyphens for human readability,
// accepting that PVE <8.x deployments may need to switch to underscores
// (change the fmt.Sprintf format string; no logic change required).
func generateSnapName() (string, error) {
	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return fmt.Sprintf("bosh-%d-%s", time.Now().Unix(), hex.EncodeToString(buf)), nil
}
