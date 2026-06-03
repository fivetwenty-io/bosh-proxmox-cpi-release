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
//  2. Run snapshot pre-flight guard (D2-C/D3-C): fail if snapshots exist and
//     allow_disk_ops_with_snapshots is false; fail-open or fail-closed on check
//     error per require_snapshot_check_pass.
//  3. Read current disk size from VM config (parsed from the disk option string).
//  4. Compute delta GiB. Shrink (delta < 0) is not supported. No-op (delta == 0) returns nil.
//  5. Call ResizeDisk with the positive delta in GiB (PVE additive "+NG" format).
//  6. Await the returned task UPID.
//
// Size conversion: BOSH provides MiB; PVE ResizeDisk accepts GiB deltas. The delta
// is computed as ceiling(new_size_mb / 1024) - current_size_gib. If current_size_gib
// cannot be determined from the config, the handler returns an error.
//
// Shrink is not supported: PVE does not support disk shrink via the resize API.
// Attempting to shrink returns a NotSupported error.
// nolint:gocognit // Multi-phase orchestration not yet decomposed; flagged for follow-up extraction (see .golangci.yml comment). Behaviour-preserving refactor scope-deferred.
func HandleResizeDisk(deps Deps) Handler {
	return HandlerFunc(func(ctx context.Context, args []json.RawMessage, _ jsonrpc.Context) (any, error) {
		// ----------------------------------------------------------------
		// 1. Unmarshal and validate arguments.
		// ----------------------------------------------------------------
		if len(args) < 2 {
			return nil, cpierrors.Cloud("resize_disk: expected 2 arguments (disk_cid, new_size_mb), got %d", len(args))
		}

		var diskCID string
		if err := json.Unmarshal(args[0], &diskCID); err != nil {
			return nil, cpierrors.Wrap(err, "resize_disk: args[0] disk_cid must be a string")
		}
		if diskCID == "" {
			return nil, cpierrors.Cloud("resize_disk: args[0] disk_cid must not be empty")
		}
		// Strip optional metadata suffix before any PVE API or storage lookup.
		bareDiskCID, _, decErr := pve.ParseEncodedDiskCID(diskCID)
		if decErr != nil {
			return nil, cpierrors.DiskNotFound(diskCID)
		}

		var newSizeMB int
		if err := json.Unmarshal(args[1], &newSizeMB); err != nil {
			return nil, cpierrors.Wrap(err, "resize_disk: args[1] new_size_mb must be an integer")
		}
		if newSizeMB <= 0 {
			return nil, cpierrors.Cloud("resize_disk: new_size_mb must be > 0, got %d", newSizeMB)
		}

		// ----------------------------------------------------------------
		// 2. Parse disk_cid → volid.
		// ----------------------------------------------------------------
		if _, _, err := pve.ParseDiskCID(bareDiskCID); err != nil {
			return nil, cpierrors.DiskNotFound(diskCID)
		}

		// ----------------------------------------------------------------
		// 3. Locate attached VM + its current node, then resolve diskID.
		//    FindVMByDiskVolid scans /cluster/resources so resize works in
		//    multi-node deployments without depending on Config.Node matching
		//    the VM's node. PVE VM config stores disk values in canonical
		//    "<storage>:<volname>" form — the disk_cid is that form.
		// ----------------------------------------------------------------
		vmid, node, err := pve.FindVMByDiskVolid(ctx, deps.PVE, deps.Config.Node, bareDiskCID)
		if err != nil {
			return nil, err
		}

		diskID, err := pve.ResolveDiskID(ctx, deps.PVE, node, vmid, bareDiskCID)
		if err != nil {
			// ResolveDiskID returns CloudError when disk not attached.
			return nil, cpierrors.Wrap(err, fmt.Sprintf("resize_disk: cannot resolve diskID for %s on VM %d", diskCID, vmid))
		}

		// ----------------------------------------------------------------
		// 4. Snapshot pre-flight guard.
		//
		// PVE cannot resize disks on LVM-thin or ZFS storage when snapshots
		// exist (the API returns an error). On qcow2/raw the resize succeeds
		// but the snapshot data becomes inconsistent. Guard before any
		// mutating PVE call to surface the risk early with an actionable
		// message.
		//
		// Policy (D2-C, D3-C):
		//   HasSnapshots error + RequireSnapshotCheckPass=true  → fail-closed
		//   HasSnapshots error + RequireSnapshotCheckPass=false → WARN + proceed
		//   Snapshots present + AllowDiskOpsWithSnapshots=false → Cloud error
		//   Snapshots present + AllowDiskOpsWithSnapshots=true  → WARN + proceed
		//   No snapshots                                        → proceed normally
		// ----------------------------------------------------------------
		if snapNames, snapErr := pve.HasSnapshots(ctx, deps.PVE, node, vmid); snapErr != nil {
			if deps.Config.RequireSnapshotCheckPass {
				return nil, cpierrors.SnapshotBlocked(
					"resize_disk: snapshot pre-flight check failed and require_snapshot_check_pass is set: %s",
					snapErr.Error(),
				)
			}
			deps.Logger.Warn("resize_disk: snapshot pre-flight check failed — proceeding (fail-open)",
				log.String("node", node),
				log.Int("vmid", vmid),
				log.Err(snapErr),
			)
		} else if len(snapNames) > 0 {
			if deps.Config.AllowDiskOpsWithSnapshots {
				deps.Logger.Warn("resize_disk: proceeding despite snapshots (allow_disk_ops_with_snapshots=true)",
					log.Int("vmid", vmid),
					log.String("node", node),
					log.String("snapshots", strings.Join(snapNames, ", ")),
				)
			} else {
				return nil, cpierrors.SnapshotBlocked(
					"resize_disk: VM %d (node %s) has %d snapshot(s) [%s]."+
						" PVE cannot resize disks on LVM-thin or ZFS storage when snapshots exist;"+
						" on qcow2/raw the resize succeeds but snapshot data becomes inconsistent."+
						" Delete all snapshots before resizing, or set"+
						" pve.allow_disk_ops_with_snapshots=true to bypass this guard.",
					vmid, node, len(snapNames), strings.Join(snapNames, ", "),
				)
			}
		}

		// ----------------------------------------------------------------
		// 6. Read current size from VM config disk option string.
		//    PVE stores disk config as: "<volid>[,size=<N>G][,<other-opts>]"
		//    The size field may be absent on very old disks; if absent, we
		//    cannot determine the delta safely and return an error.
		// ----------------------------------------------------------------
		cfg, err := deps.PVE.QEMU().Config(ctx, node, vmid)
		if err != nil {
			return nil, cpierrors.Wrap(err, fmt.Sprintf("resize_disk: read VM %d config", vmid))
		}

		diskOptStr, ok := cfg[diskID].(string)
		if !ok || diskOptStr == "" {
			return nil, cpierrors.Cloud("resize_disk: disk %q not found in VM %d config", diskID, vmid)
		}

		currentGiB, parseErr := parseDiskSizeGiB(diskOptStr)
		if parseErr != nil {
			return nil, cpierrors.Wrap(parseErr, "resize_disk: cannot determine current size of disk "+diskCID)
		}

		// ----------------------------------------------------------------
		// 7. Compute delta in GiB (ceiling of new_size_mb / 1024).
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
		// 8-9. Submit ResizeDisk and await its task. PVE's qemu-img resize
		// runs under the per-storage lockfile, so on bursty deploys the
		// task can fail with "can't lock file ... got timeout". Retry the
		// submit+await pair on that signal; non-lock errors propagate.
		// ----------------------------------------------------------------
		rerr := pve.RetryOnTransientOrLock(ctx, deps.Logger, "resize_disk", 0, func() error {
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
			return nil, cpierrors.Wrap(pve.WrapError(rerr), fmt.Sprintf("resize_disk: ResizeDisk failed for VM %d disk %s (+%dG)", vmid, diskCID, deltaGiB))
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

// parseDiskSizeGiB extracts the size from a PVE disk option string and
// converts it to whole GiB. The option string format is
// "<volid>[,key=value,...]" where one key may be "size=<N><unit>". The
// unit is case-insensitive and one of K, M, G, T, P (KiB, MiB, GiB, TiB,
// PiB respectively). A unit-less value is treated as bytes.
//
// Examples:
//
//	"local-lvm:vm-100-disk-1,size=10G"          → 10
//	"local-lvm:vm-100-disk-1,size=1024M"        → 1
//	"local-lvm:vm-100-disk-1,size=1T"           → 1024
//	"local-lvm:vm-100-disk-1,size=1048576K"     → 1
//	"local-lvm:vm-100-disk-1,size=10737418240"  → 10  (bytes, no unit)
//	"local-lvm:vm-100-disk-1"                   → error (size missing)
//	"local-lvm:vm-100-disk-1,size=100xyz"       → error (unknown unit)
//
// Conversion uses integer ceiling so a value that is not a whole number
// of GiB rounds UP to the next GiB. PVE never shrinks an existing disk
// at the storage layer, so rounding up matches the on-disk semantics
// reported by qemu-img (the disk is allocated to the next GiB boundary).
//
// Inputs and failure modes:
//   - optStr empty or no "size=" segment → CloudError "size option not found".
//   - size value empty → CloudError "size value empty".
//   - size value not a positive integer → wrapped strconv error.
//   - size unit unrecognised → CloudError "unsupported size unit".
//   - size in K/M/G/T/P with case-insensitive suffix → rounded-up GiB.
func parseDiskSizeGiB(optStr string) (int, error) {
	for _, part := range strings.Split(optStr, ",") {
		if !strings.HasPrefix(part, "size=") {
			continue
		}
		sizeVal := strings.TrimPrefix(part, "size=")
		if sizeVal == "" {
			return 0, cpierrors.Cloud("size value empty in %q", part)
		}

		// Detect unit suffix (last byte). PVE accepts the same set as
		// qemu-img: K, M, G, T, P. Anything else is treated as either
		// no-unit (digit) or unknown (other letter).
		last := sizeVal[len(sizeVal)-1]
		var unit byte
		var numStr string
		switch last {
		case 'K', 'k', 'M', 'm', 'G', 'g', 'T', 't', 'P', 'p':
			unit = last
			numStr = sizeVal[:len(sizeVal)-1]
		default:
			if last >= '0' && last <= '9' {
				// No unit — treat as bytes.
				unit = 'B'
				numStr = sizeVal
			} else {
				return 0, cpierrors.Cloud("unsupported size unit in %q (expected one of K/M/G/T/P or bytes)", sizeVal)
			}
		}

		if numStr == "" {
			return 0, cpierrors.Cloud("size value missing numeric part in %q", sizeVal)
		}

		n, err := strconv.ParseInt(numStr, 10, 64)
		if err != nil {
			return 0, cpierrors.Wrap(err, "cannot parse size value "+sizeVal)
		}
		if n < 0 {
			return 0, cpierrors.Cloud("size value must be non-negative in %q", sizeVal)
		}

		// Convert each unit to bytes, then bytes → GiB rounded up.
		const (
			kib int64 = 1 << 10
			mib int64 = 1 << 20
			gib int64 = 1 << 30
			tib int64 = 1 << 40
			pib int64 = 1 << 50
		)
		var bytesVal int64
		switch unit {
		case 'K', 'k':
			bytesVal = n * kib
		case 'M', 'm':
			bytesVal = n * mib
		case 'G', 'g':
			bytesVal = n * gib
		case 'T', 't':
			bytesVal = n * tib
		case 'P', 'p':
			bytesVal = n * pib
		case 'B':
			bytesVal = n
		}

		// Ceiling division to GiB.
		out := (bytesVal + gib - 1) / gib
		return int(out), nil
	}
	return 0, cpierrors.Cloud("size option not found in disk option string %q", optStr)
}
