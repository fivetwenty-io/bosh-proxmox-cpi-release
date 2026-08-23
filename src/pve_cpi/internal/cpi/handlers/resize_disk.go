package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
)

// HandleResizeDisk returns a Handler for the BOSH CPI resize_disk method.
//
// Arguments (positional JSON array):
//
//	[0] disk_cid     string — disk CID in the "pvd-"/"pvz-" envelope form
//	                          emitted by create_disk
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
//
//nolint:gocognit // Multi-phase orchestration not yet decomposed; flagged for follow-up extraction (see .golangci.yml comment). Behaviour-preserving refactor scope-deferred.
func HandleResizeDisk(deps Deps) Handler {
	return HandlerFunc(func(ctx context.Context, args []json.RawMessage, reqCtx jsonrpc.Context) (any, error) {
		deps, err := deps.WithRequestOverrides(ctx, reqCtx)
		if err != nil {
			return nil, err
		}
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
		bareDiskCID, meta, decErr := decodeDiskCID(ctx, deps, "resize_disk", diskCID)
		if decErr != nil {
			return nil, decErr
		}
		// Resolve to the volume's current name (identity seam): the locator
		// and slot resolver below match VM config entries, which carry the
		// post-reassignment name for stable-ID disks.
		rd, resolveErr := resolveDiskForOp(ctx, deps, "resize_disk", diskCID, bareDiskCID, meta)
		if resolveErr != nil {
			return nil, resolveErr
		}
		bareDiskCID = rd.volid

		var newSizeMB int
		if err := json.Unmarshal(args[1], &newSizeMB); err != nil {
			return nil, cpierrors.Wrap(err, "resize_disk: args[1] new_size_mb must be an integer")
		}
		if newSizeMB <= 0 {
			return nil, cpierrors.Cloud("resize_disk: new_size_mb must be > 0, got %d", newSizeMB)
		}

		// ----------------------------------------------------------------
		// 2. Parse disk_cid → storage + volid.
		// ----------------------------------------------------------------
		storageName, _, parseErr2 := pve.ParseDiskCID(bareDiskCID)
		if parseErr2 != nil {
			return nil, cpierrors.DiskNotFound(diskCID)
		}

		// ----------------------------------------------------------------
		// 3. Locate attached VM + its current node, then resolve diskID.
		//    FindVMByDiskVolid scans /cluster/resources so resize works in
		//    multi-node deployments without depending on Config.Node matching
		//    the VM's node. PVE VM config stores disk values in canonical
		//    "<storage>:<volname>" form — the disk_cid is that form.
		// ----------------------------------------------------------------
		vmid, node, err := pve.FindVMByDiskVolid(ctx, deps.PVE, bareDiskCID)
		if err != nil {
			return nil, err
		}

		// Parker VMs are always stopped (onboot=0, never started). Resizing a
		// disk attached to a parker VM is safe: PVE's resize API operates on
		// the backing volume directly, with no live-disk constraint. The resize
		// proceeds through the normal path — no parker-specific gate needed.

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
			deps.Log(ctx).Warn("resize_disk: snapshot pre-flight check failed — proceeding (fail-open)",
				log.String("node", node),
				log.Int("vmid", vmid),
				log.Err(snapErr),
			)
		} else if len(snapNames) > 0 {
			if deps.Config.AllowDiskOpsWithSnapshots {
				deps.Log(ctx).Warn("resize_disk: proceeding despite snapshots (allow_disk_ops_with_snapshots=true)",
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
			deps.Log(ctx).Info("resize_disk: no-op, disk already at or above requested size",
				log.String("disk_cid", diskCID),
				log.Int("current_gib", currentGiB),
				log.Int("new_size_mb", newSizeMB),
			)
			return nil, nil
		}

		// ----------------------------------------------------------------
		// 7a. Storage-utilization gate (pve.storage.max_utilization_pct).
		// No-op when the ceiling is unset (0, the default). addBytes is only
		// the positive delta being ADDED to the pool by this resize, not the
		// disk's resulting total size.
		// ----------------------------------------------------------------
		if gateErr := checkMaxUtilizationGate(
			ctx, deps, node, storageName, int64(deltaGiB)*storageUtilBytesPerGiB, "resize_disk",
		); gateErr != nil {
			return nil, gateErr
		}

		// ----------------------------------------------------------------
		// 8-9. Submit ResizeDisk and await its task. PVE's qemu-img resize
		// runs under the per-storage lockfile, so on bursty deploys the
		// task can fail with "can't lock file ... got timeout". Retry the
		// submit+await pair on that signal; non-lock errors propagate.
		// The retry recomputes the remaining delta from the live config so
		// a committed-then-dropped attempt is not replayed on top of itself.
		// ----------------------------------------------------------------
		rerr := resizeDiskConverging(ctx, deps, deps.Log(ctx), "resize_disk", node, vmid, diskID, currentGiB, newGiB, 0)
		if rerr != nil {
			return nil, cpierrors.Wrap(pve.WrapError(rerr), fmt.Sprintf("resize_disk: ResizeDisk failed for VM %d disk %s (+%dG)", vmid, diskCID, deltaGiB))
		}

		// ----------------------------------------------------------------
		// 10. Optional post-resize size-convergence wait (§7.27, opt-in).
		// Best-effort: never errors, so a slow async backend cannot fail the
		// resize. Skipped entirely (zero extra calls) when disabled.
		// ----------------------------------------------------------------
		if deps.Config.ResizeWaitForConvergenceEnabled() {
			timeout := time.Duration(deps.Config.ResizeConvergenceTimeoutSecValue()) * time.Second
			waitForResizeConvergence(ctx, deps, node, vmid, diskID, currentGiB+deltaGiB, timeout)
		}

		deps.Log(ctx).Info("resize_disk",
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

// Settle bounds for resizeDiskConverging's dropped-POST window. Package
// vars, replaced by SetResizeSettleBounds in tests.
var (
	resizeSettlePollInterval = 2 * time.Second
	resizeSettleMaxWait      = 30 * time.Second
)

// SetResizeSettleBounds replaces the dropped-POST settle poll interval and
// bound and returns a restore func. Test seam:
//
//	defer handlers.SetResizeSettleBounds(0, 50*time.Millisecond)()
func SetResizeSettleBounds(interval, maxWait time.Duration) func() {
	prevInterval, prevMax := resizeSettlePollInterval, resizeSettleMaxWait
	resizeSettlePollInterval = interval
	resizeSettleMaxWait = maxWait
	return func() {
		resizeSettlePollInterval = prevInterval
		resizeSettleMaxWait = prevMax
	}
}

// readDiskSizeGiB reads the live size of diskID from the guest config.
func readDiskSizeGiB(ctx context.Context, deps Deps, op, node string, vmid int, diskID string) (int, error) {
	cfg, cfgErr := deps.PVE.QEMU().Config(ctx, node, vmid)
	if cfgErr != nil {
		return 0, cfgErr
	}
	diskOptStr, ok := cfg[diskID].(string)
	if !ok || diskOptStr == "" {
		return 0, cpierrors.Cloud("%s: disk %s missing from VM %d config while re-reading size for retry", op, diskID, vmid)
	}
	gib, parseErr := parseDiskSizeGiB(diskOptStr)
	if parseErr != nil {
		return 0, cpierrors.Wrap(parseErr, fmt.Sprintf("%s: re-read size of disk %s on VM %d", op, diskID, vmid))
	}
	return gib, nil
}

// settleDroppedResizeGiB gives an unnamed in-flight resize task a bounded
// window to land before the delta is recomputed. It backs the dropped-POST
// arm of resizeDiskConverging: when the resize POST's response drops, PVE may
// have already created a task we never learned the UPID of, and that task
// only writes the new size into the config when it finishes; recomputing the
// delta from a config read taken while it still runs re-submits the full
// growth. The poll returns as soon as the size reaches targetGiB or stops at
// the window's edge with the latest observed size; read errors end the poll
// with the last known size (the caller's re-read already succeeded once).
func settleDroppedResizeGiB(
	ctx context.Context, deps Deps, logger *log.Logger,
	op, node string, vmid int, diskID string, observedGiB, targetGiB int,
) int {
	deadline := time.Now().Add(resizeSettleMaxWait)
	latest := observedGiB
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return latest
		case <-time.After(resizeSettlePollInterval):
		}
		gib, err := readDiskSizeGiB(ctx, deps, op, node, vmid, diskID)
		if err != nil {
			return latest
		}
		latest = gib
		if latest >= targetGiB {
			return latest
		}
	}
	logger.Info("resize settle window closed without reaching the target; resubmitting the remaining delta",
		log.String("op", op),
		log.Int(metadataKeyVMID, vmid),
		log.String("disk_id", diskID),
		log.Int("observed_gib", latest),
		log.Int("target_gib", targetGiB),
	)
	return latest
}

// resizeDiskConverging grows the disk at diskID to targetGiB through PVE's
// relative resize API (the SDK sends "+NG"), recomputing the remaining delta
// on every retry. Computing the delta once outside the loop is a replay
// hazard: an attempt whose resize committed but whose response was dropped
// (connection blip, lock timeout on the await) would re-issue the same
// relative delta and land the disk at current plus twice the growth,
// silently, because convergence checks are >=. Re-reading the live size
// inside the closure makes the replay idempotent; a zero or negative
// remaining delta means a prior attempt already committed, which is success.
//
// The re-read is only authoritative once the prior attempt's task has
// settled: PVE writes the new size into the config when the resize task
// FINISHES, not when the POST returns. Two arms guard that ordering. When
// the prior attempt holds a task UPID that has not RESOLVED (its await
// failed without carrying the task's own exit verdict, pve.IsTaskExitVerdict),
// the retry re-awaits that same task before re-reading; a resolved task,
// success or failure verdict alike, settles the config and falls through to
// the re-read plus resubmit. When the POST's own response dropped, no UPID
// exists to await, so the re-read gets a bounded settle window
// (settleDroppedResizeGiB) for the possible unnamed task to land; a task
// slower than the window resubmits into PVE's config lock, which rejects the
// overlap with a retryable lock fault rather than double-applying.
//
// currentGiB is the size the caller already read for its first-attempt delta,
// so the steady-state path issues no extra config reads.
func resizeDiskConverging(
	ctx context.Context,
	deps Deps,
	logger *log.Logger,
	op, node string,
	vmid int,
	diskID string,
	currentGiB, targetGiB, maxAttempts int,
) error {
	knownGiB := currentGiB
	firstAttempt := true
	pendingUPID := ""      // prior attempt's task whose completion is unresolved
	submitDropped := false // prior POST's response dropped: an unnamed task may exist
	return pve.RetryOnTransientOrLock(ctx, logger, op, maxAttempts, func() error {
		if !firstAttempt {
			if pendingUPID != "" {
				awaitErr := pve.AwaitTaskWithLogger(ctx, deps.PVE, node, pendingUPID, logger)
				switch {
				case awaitErr == nil, pve.IsTaskExitVerdict(awaitErr):
					// The task RESOLVED (success, or a failure verdict):
					// the config now reflects its outcome, and the re-read
					// plus resubmit below is the recovery for a failed one.
					pendingUPID = ""
				default:
					// UNRESOLVED: a poll transport fault or a poll timeout;
					// the task may still be executing, so re-reading now
					// would recompute the delta from a moving config. A
					// retryable fault rides the loop's backoff into another
					// re-await; a poll timeout exits to the Director as
					// retriable, matching a first attempt's await semantics.
					return awaitErr
				}
			}
			gib, readErr := readDiskSizeGiB(ctx, deps, op, node, vmid, diskID)
			if readErr != nil {
				return readErr
			}
			if submitDropped && gib < targetGiB {
				gib = settleDroppedResizeGiB(ctx, deps, logger, op, node, vmid, diskID, gib, targetGiB)
			}
			submitDropped = false
			knownGiB = gib
		}
		firstAttempt = false

		remaining := targetGiB - knownGiB
		if remaining <= 0 {
			logger.Info("resize replay converged: a prior attempt committed the growth",
				log.String("op", op),
				log.Int(metadataKeyVMID, vmid),
				log.String("disk_id", diskID),
				log.Int("current_gib", knownGiB),
				log.Int("target_gib", targetGiB),
			)
			return nil
		}
		upid, e := deps.PVE.QEMU().ResizeDisk(ctx, node, vmid, diskID, remaining)
		if e != nil {
			submitDropped = pve.IsTransportConnectionDrop(e)
			return e
		}
		if upid == "" {
			return nil
		}
		pendingUPID = upid
		e = pve.AwaitTaskWithLogger(ctx, deps.PVE, node, upid, logger)
		if e == nil || pve.IsTaskExitVerdict(e) {
			// Resolved either way: a retry must not re-poll a dead task,
			// it must re-read and resubmit the remaining delta.
			pendingUPID = ""
		}
		return e
	})
}

// waitForResizeConvergence polls the VM config until the disk at diskID reports
// a size >= targetGiB, or until the timeout (or parent context) elapses. It is
// best-effort and never returns an error: a backend that has not yet propagated
// the new size logs a warning and the caller proceeds. The poll interval comes
// from the resizeConvergencePollInterval seam; the bound is an independent
// timeout so it works even when the operation_timeout envelope is disabled.
//
// Read errors during polling are tolerated (logged at debug and retried until
// the bound) — the convergence wait must not convert a transient read blip into
// a resize failure.
func waitForResizeConvergence(
	ctx context.Context,
	deps Deps,
	node string,
	vmid int,
	diskID string,
	targetGiB int,
	timeout time.Duration,
) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	interval := resizeConvergencePollInterval()
	for {
		cfg, err := deps.PVE.QEMU().Config(cctx, node, vmid)
		if err != nil {
			deps.Log(cctx).Debug("resize_disk: convergence poll config read failed — retrying",
				log.Int("vmid", vmid),
				log.String("disk_id", diskID),
				log.Err(err),
			)
		} else if optStr, ok := cfg[diskID].(string); ok && optStr != "" {
			gib, perr := parseDiskSizeGiB(optStr)
			switch {
			case perr != nil:
				// Unparseable size= keeps polling until the bound; log so a
				// permanently-malformed value is visible rather than silent.
				deps.Log(cctx).Debug("resize_disk: convergence poll could not parse disk size — retrying",
					log.Int("vmid", vmid),
					log.String("disk_id", diskID),
					log.String("disk_opt", optStr),
					log.Err(perr),
				)
			case gib >= targetGiB:
				deps.Log(cctx).Info("resize_disk: size converged",
					log.Int("vmid", vmid),
					log.String("disk_id", diskID),
					log.Int("reported_gib", gib),
					log.Int("target_gib", targetGiB),
				)
				return
			}
		}

		select {
		case <-cctx.Done():
			deps.Log(cctx).Warn("resize_disk: disk size did not converge within budget — proceeding (best-effort)",
				log.Int("vmid", vmid),
				log.String("disk_id", diskID),
				log.Int("target_gib", targetGiB),
				log.String("timeout", timeout.String()),
			)
			return
		case <-time.After(interval):
		}
	}
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
