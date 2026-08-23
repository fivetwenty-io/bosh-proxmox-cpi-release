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
//	[0] disk_cid  string         — disk CID in the "pvd-"/"pvz-" envelope form
//	                               emitted by create_disk
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
	return HandlerFunc(func(ctx context.Context, args []json.RawMessage, reqCtx jsonrpc.Context) (any, error) {
		deps, err := deps.WithRequestOverrides(ctx, reqCtx)
		if err != nil {
			return nil, err
		}
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
		bareDiskCID, meta, decErr := decodeDiskCID(ctx, deps, "snapshot_disk", diskCID)
		if decErr != nil {
			return nil, decErr
		}
		// Resolve to the volume's current name (identity seam): the holder
		// scan below matches VM config entries, which carry the
		// post-reassignment name for stable-ID disks.
		rd, resolveErr := resolveDiskForOp(ctx, deps, "snapshot_disk", diskCID, bareDiskCID, meta)
		if resolveErr != nil {
			return nil, resolveErr
		}
		bareDiskCID = rd.volid

		// metadata arg is optional and may be null or absent.
		var metadata map[string]any
		if len(args) >= 2 && args[1] != nil {
			_ = json.Unmarshal(args[1], &metadata)
		}

		// ----------------------------------------------------------------
		// 2. Parse disk_cid → storage + volid components.
		// ----------------------------------------------------------------
		storageName, _, parseErr := pve.ParseDiskCID(bareDiskCID)
		if parseErr != nil {
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
		vmid, node, holderTags, err := pve.FindVMByDiskVolidTagged(ctx, deps.PVE, bareDiskCID)
		if err != nil {
			return nil, err
		}

		// ----------------------------------------------------------------
		// 3a. Parker guard.
		//
		// A PVE snapshot targets the entire VM, not a single disk. Snapshotting
		// a parker VM would entangle ALL parked disks from ALL BOSH deployments
		// into a single snapshot — semantically wrong and potentially very large.
		// Reject the operation with a non-retriable error so the director surfaces
		// the condition to the operator rather than silently succeeding.
		//
		// The holder is classified by its bosh-parker tag, not by whether its
		// VMID sits in the configured band, so a parker left outside the band
		// by a strategy or band change is still recognized. The tags come from
		// the holder scan above, so this costs no API call.
		// ----------------------------------------------------------------
		if err := guardSnapshotParked(diskCID, vmid, holderTags); err != nil {
			return nil, err
		}

		// ----------------------------------------------------------------
		// 3b. Storage-utilization gate (pve.storage.max_utilization_pct):
		// Warn-only regardless of storage.max_utilization_mode. Snapshot
		// growth is unbounded and cannot be estimated ahead of time, so this
		// only warns when the pool is ALREADY above the ceiling; it never
		// blocks the snapshot. No-op when the ceiling is unset (0, default).
		// ----------------------------------------------------------------
		warnIfStorageAboveCeiling(ctx, deps, node, storageName)

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
		serr := takeVMSnapshotConverging(ctx, deps, node, vmid, snapName, snapOpts)
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

// takeVMSnapshotConverging submits the snapshot and awaits its task under the
// storage-lock retry loop, converging replays: the name is generated once per
// handler call, so a replay of a committed-then-dropped attempt hits
// "snapshot name ... already exists" and would burn the whole lock budget
// before failing; the Director's redo then generates a fresh name and orphans
// the first snapshot, which later hard-blocks resize_disk and parker
// transfers. On a replayed attempt's failure, the loop checks whether
// snapName already exists: if it does, a prior attempt committed and the goal
// is reached. A first-attempt failure cannot mean that (the name is fresh),
// so it skips the probe entirely.
func takeVMSnapshotConverging(ctx context.Context, deps Deps, node string, vmid int, snapName string, snapOpts map[string]any) error {
	firstAttempt := true
	return pve.RetryOnTransientOrLock(ctx, deps.Log(ctx), "snapshot_disk", 0, func() error {
		replay := !firstAttempt
		firstAttempt = false
		upid, e := deps.PVE.QEMU().Snapshot(ctx, node, vmid, snapName, snapOpts)
		if e == nil {
			if upid == "" {
				return nil
			}
			e = pve.AwaitTaskWithLogger(ctx, deps.PVE, node, upid, deps.Log(ctx))
			if e == nil {
				return nil
			}
		}
		if replay && snapshotAlreadyCommitted(ctx, deps, node, vmid, snapName) {
			deps.Log(ctx).Info("snapshot_disk: replay found the snapshot already committed",
				log.Int("vmid", vmid),
				log.String("snap_name", snapName),
			)
			return nil
		}
		return e
	})
}

// snapshotAlreadyCommitted reports whether snapName exists on the VM as a
// COMPLETED snapshot. It backs the replay tolerance in HandleSnapshotDisk:
// only a prior attempt of the same handler call can have created that exact
// name, so its completed presence after a failed replay means the goal is
// reached. Presence alone is not enough: PVE writes the snapshot's config
// section as soon as the worker starts and stamps it with a snapstate field
// (prepare, delete) while in progress or after a failure it could not roll
// back, and the "already exists" rejection fires on that section too. An
// entry still carrying snapstate is a half-created snapshot, not the goal
// state, so it reports false and the replay's own error stands. A listing
// error also reports false for the same reason.
func snapshotAlreadyCommitted(ctx context.Context, deps Deps, node string, vmid int, snapName string) bool {
	entries, listErr := deps.PVE.QEMU().ListSnapshots(ctx, node, vmid)
	if listErr != nil {
		return false
	}
	for _, entry := range entries {
		name, _ := entry["name"].(string)
		if name != snapName {
			continue
		}
		state, _ := entry["snapstate"].(string)
		return state == ""
	}
	return false
}

// guardSnapshotParked rejects a snapshot operation when the disk's holder VM is
// a parker VM. A PVE snapshot targets the entire VM, not a single disk;
// snapshotting a parker VM would entangle all parked disks from all BOSH
// deployments. Returns a non-retriable Cloud error when the condition is
// detected; nil when the holder is a regular workload VM.
//
// The holder is classified by tag rather than by band membership, matching
// delete_vm's refusal: the band is configuration and the tag is a fact about
// the VM, so a parker stranded outside the band by a strategy or band change
// is still recognized. The tags come from the holder scan, which read that
// config to decide the VM was the holder, so the guard costs no API call.
func guardSnapshotParked(diskCID string, vmid int, holderTags string) error {
	if pve.TagsMarkParker(holderTags) {
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
