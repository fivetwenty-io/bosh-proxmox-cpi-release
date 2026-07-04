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
		// 3a. Parker guard (opt-in, zero extra calls when strategy unset).
		//
		// A PVE snapshot targets the entire VM, not a single disk. Snapshotting
		// a parker VM would entangle ALL parked disks from ALL BOSH deployments
		// into a single snapshot — semantically wrong and potentially very large.
		// Reject the operation with a non-retriable error so the director surfaces
		// the condition to the operator rather than silently succeeding.
		//
		// The guard runs only when ParkedStrategyActive() is true; never-opted-in
		// installations see zero added API calls and byte-identical behavior.
		//
		// Range check first (fast, no API): if holder VMID falls in the parker
		// band, fetch the VM config once to confirm the bosh-parker tag before
		// classifying. Out-of-range holders short-circuit without a Config read.
		// ----------------------------------------------------------------
		if err := guardSnapshotParked(ctx, deps, diskCID, vmid, node); err != nil {
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
		serr := pve.RetryOnTransientOrLock(ctx, deps.Log(ctx), "snapshot_disk", 0, func() error {
			upid, e := deps.PVE.QEMU().Snapshot(ctx, node, vmid, snapName, snapOpts)
			if e != nil {
				return e
			}
			if upid == "" {
				return nil
			}
			return pve.AwaitTaskWithLogger(ctx, deps.PVE, node, upid, deps.Log(ctx))
		})
		if serr != nil {
			deps.Log(ctx).Error("snapshot_disk: Snapshot failed",
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

		deps.Log(ctx).Info("snapshot_disk",
			log.String("disk_cid", diskCID),
			log.Int("vmid", vmid),
			log.String("snap_name", snapName),
			log.String("snapshot_cid", snapshotCID),
		)

		return snapshotCID, nil
	})
}

// guardSnapshotParked rejects a snapshot operation when the disk's holder VM is
// a parker VM. A PVE snapshot targets the entire VM, not a single disk;
// snapshotting a parker VM would entangle all parked disks from all BOSH
// deployments. Returns a non-retriable Cloud error when the condition is
// detected; nil when strategy is unset or the holder is a regular workload VM.
//
// Range check first (fast, no API): only when vmid falls within the parker band
// does the helper fetch the VM config once to confirm the bosh-parker tag.
// Out-of-range holders short-circuit with no Config read.
func guardSnapshotParked(ctx context.Context, deps Deps, diskCID string, vmid int, node string) error {
	if deps.Config == nil || !deps.Config.ParkedStrategyActive() {
		return nil
	}
	parkerCfg := pve.ParkerConfig{
		VMIDRangeStart: deps.Config.ParkedDiskVMIDRangeStartValue(),
		VMIDRangeEnd:   deps.Config.ParkedDiskVMIDRangeEndValue(),
		DirectorID:     deps.Config.StemcellDirectorID(),
	}
	if vmid < parkerCfg.VMIDRangeStart || vmid > parkerCfg.VMIDRangeEnd {
		return nil
	}
	holderCfg, cfgErr := deps.PVE.QEMU().Config(ctx, node, vmid)
	if cfgErr != nil {
		return cpierrors.WrapAs(cfgErr, cpierrors.TypeRetriableCloud,
			fmt.Sprintf("snapshot_disk: parker check: config fetch for vmid %d", vmid))
	}
	tags, _ := holderCfg["tags"].(string)
	if pve.IsParkerVM(vmid, tags, parkerCfg) {
		return cpierrors.Cloud(
			"snapshot_disk: disk %s is held by a parker VM (vmid %d): "+
				"disk is not attached to a workload VM (disk is parked as detached); "+
				"snapshotting a parker VM would entangle all parked disks across deployments",
			diskCID, vmid,
		)
	}
	return nil
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
