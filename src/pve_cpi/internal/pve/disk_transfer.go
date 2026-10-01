// Ownership transfer for stable-ID disks (D13): a disk moves between its
// parker and a workload VM by PVE move_disk reassignment, which renames the
// volume to match its new owner. The drive serial= token (disk_stable_id.go)
// is what survives that rename.
//
// Direction matters. A parker is never running, so the attach direction
// (parker → VM) reassigns the attached slot directly and the full option
// string — serial included — rides along (live-spike result). A running
// source VM refuses direct reassignment, so the detach direction (VM →
// parker) detaches the slot to an unusedN entry first and reassigns that;
// unused entries carry no options, so the serial is re-applied on the landed
// parker slot afterwards. The crash window that opens between those steps is
// covered by write ordering: the receiving parker's provenance record (the
// intent) is written BEFORE the source slot is deleted, so a scan always
// finds at least one identity carrier.
package pve

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// ErrMoveDiskSnapshotRefused wraps PVE's immediate refusal to reassign a
// volume a snapshot references ("Can't move disk used by a snapshot to
// another VM"). Callers detect it via errors.Is and fall back to the
// config-edit transfer path, which the snapshot machinery already governs.
var ErrMoveDiskSnapshotRefused = errors.New("move_disk refused: disk used by a snapshot")

// IsMoveDiskSnapshotRefusal reports whether err is PVE's snapshot refusal of
// a move_disk reassignment. The refusal arrives synchronously, before any
// task starts (live-spike result), so it surfaces as the POST's own error.
func IsMoveDiskSnapshotRefusal(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrMoveDiskSnapshotRefused) {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "used by a snapshot")
}

// moveDiskToVM issues one move_disk reassignment and awaits its task. disk is
// the source config key ("scsi3" or "unused0"), targetSlot the config key the
// volume lands on. Same-node only — PVE refuses cross-node reassignment.
//
//nolint:gocognit // Move retry-state machine: pendingUPID re-await, replay probe, and the POST+await path are one ordered decision tree; splitting it would scatter the ordering the double-apply protection depends on.
func moveDiskToVM(ctx context.Context, c Client, logger *log.Logger, node string, srcVMID int, disk string, targetVMID int, targetSlot string) error {
	nodesSvc := c.Nodes()
	if nodesSvc == nil {
		return cpierrors.Cloud("moveDiskToVM: nodes service not available")
	}
	tv := int64(targetVMID)
	ts := targetSlot
	// The await runs INSIDE the retry closure: the per-storage lock most
	// often lands on the move task rather than the POST, and the parker's
	// protection window stays open for the whole transfer, so a task-level
	// lock timeout must re-drive the move in-process instead of surfacing.
	//
	// pendingUPID tracks a submitted move whose await did not RESOLVE
	// (IsTaskExitVerdict false — a poll transport fault or an unresolved
	// poll timeout, IsTaskPollUnresolved). On the NEXT attempt the closure
	// re-awaits that same task instead of re-POSTing CreateQemuMoveDisk:
	// the earlier move may still be executing on PVE, and a second POST
	// would double-apply it. Mirrors resizeDiskConverging.
	attempts := 0
	errFromAwait := false
	pendingUPID := ""
	moveErr := RetryOnTransientOrLock(ctx, logger, "disk_transfer_move", parkerWindowMaxAttempts, func() error {
		attempts++
		errFromAwait = false

		if pendingUPID != "" {
			upid := pendingUPID
			inner := AwaitTaskWithLogger(ctx, c, node, upid, logger)
			errFromAwait = true
			if inner == nil {
				pendingUPID = ""
				return nil
			}
			if IsTaskExitVerdict(inner) {
				// Resolved with a failure verdict: the task settled, so a
				// fresh POST (next attempt) is the recovery, not another
				// re-await.
				pendingUPID = ""
			}
			// Unresolved (pendingUPID stays set) rides the loop's backoff
			// into another re-await; a resolved verdict falls through to
			// the replay probe below before surfacing.
			if moveDiskLanded(ctx, c, node, srcVMID, disk, targetVMID, targetSlot) {
				pendingUPID = ""
				return nil
			}
			return inner
		}

		raw, inner := nodesSvc.CreateQemuMoveDisk(ctx, node, strconv.Itoa(srcVMID), &sdknodes.CreateQemuMoveDiskParams{
			Disk:       disk,
			TargetVmid: &tv,
			TargetDisk: &ts,
		})
		if inner == nil {
			var upid string
			if raw != nil {
				var upidErr error
				upid, upidErr = UPIDFromRaw(*raw)
				if upidErr != nil {
					return cpierrors.Wrap(upidErr, "move_disk: parse task UPID")
				}
			}
			if upid == "" {
				// No task to await: treat the POST's 200 as completion, like
				// every other endpoint that answers without a UPID.
				return nil
			}
			pendingUPID = upid
			inner = AwaitTaskWithLogger(ctx, c, node, upid, logger)
			if inner == nil {
				pendingUPID = ""
				return nil
			}
			if IsTaskExitVerdict(inner) {
				pendingUPID = ""
			}
			errFromAwait = true
		}
		// Replay tolerance: a prior attempt whose response was dropped may
		// have committed the reassignment, and the re-submit then fails on
		// "no such disk" (the source slot is empty). Probe both sides: the
		// move landed exactly when the target slot holds a volume and the
		// source slot no longer does. Only a repeat attempt can be a replay;
		// a first-attempt failure surfaces as-is.
		if attempts > 1 && moveDiskLanded(ctx, c, node, srcVMID, disk, targetVMID, targetSlot) {
			if logger != nil {
				logger.Info("disk transfer: move replay found the reassignment already committed",
					log.Int("src_vmid", srcVMID),
					log.String("disk", disk),
					log.Int("target_vmid", targetVMID),
					log.String("target_slot", targetSlot),
				)
			}
			pendingUPID = ""
			return nil
		}
		return inner
	})
	if moveErr != nil {
		if IsMoveDiskSnapshotRefusal(moveErr) {
			return fmt.Errorf("move %s of vm %d to vm %d slot %s: %w: %s",
				disk, srcVMID, targetVMID, targetSlot, ErrMoveDiskSnapshotRefused, moveErr.Error())
		}
		// Preserve the pre-loop classification split: a task-body failure
		// carries a verdict about the move itself (unsupported target
		// storage, ...) and stays on WrapError's non-retriable fallback,
		// while a POST failure keeps the mutation wrapper's retriable
		// default. Folding both into WrapMutationError flipped permanent
		// task verdicts to ok_to_retry=true.
		if errFromAwait {
			return cpierrors.Wrap(WrapError(moveErr),
				fmt.Sprintf("move_disk task for %s of vm %d to vm %d slot %s", disk, srcVMID, targetVMID, targetSlot))
		}
		return cpierrors.Wrap(WrapMutationError(moveErr),
			fmt.Sprintf("move_disk %s of vm %d to vm %d slot %s on node %s", disk, srcVMID, targetVMID, targetSlot, node))
	}
	return nil
}

// moveDiskLanded reports whether a move_disk reassignment of disk from
// srcVMID actually committed: the target slot holds a volume and the source
// slot no longer does. Both conditions are required: an occupied target slot
// alone could predate the move, and an empty source alone proves nothing
// landed. Probe errors report false so the caller surfaces the move's own
// error rather than masking it.
func moveDiskLanded(ctx context.Context, c Client, node string, srcVMID int, disk string, targetVMID int, targetSlot string) bool {
	targetCfg, tErr := c.QEMU().Config(ctx, node, targetVMID)
	if tErr != nil {
		return false
	}
	if _, occupied := slotBareVolid(targetCfg, targetSlot); !occupied {
		return false
	}
	srcCfg, sErr := c.QEMU().Config(ctx, node, srcVMID)
	if sErr != nil {
		return false
	}
	_, stillHeld := slotBareVolid(srcCfg, disk)
	return !stillHeld
}

// slotBareVolid returns the bare volid a config key currently holds, or
// ("", false) when the key is absent or empty.
func slotBareVolid(cfg map[string]any, slot string) (string, bool) {
	v, _ := ConfigString(cfg, slot)
	if v == "" {
		return "", false
	}
	if comma := strings.IndexByte(v, ','); comma >= 0 {
		v = v[:comma]
	}
	return v, true
}

// TransferDiskFromParker reassigns a parked stable-ID disk from its parker
// slot onto targetSlot of targetVMID (same node), carrying the full drive
// option string with it. optStr is the final drive value the disk should land
// with — volid first, then serial and performance options — and is written
// onto the parker slot before the move, since the reassignment propagates
// the source slot's options verbatim (live-spike result) and a stopped
// parker is the one place a drive string can be edited without config/guest
// divergence.
//
// Returns the volid the volume landed under on the target VM (move_disk
// renames it to match its new owner).
//
// A snapshot refusal from PVE is returned wrapping ErrMoveDiskSnapshotRefused
// so the caller can fall back to the config-edit path where that is safe.
// The parker's provenance entry is NOT removed here: the caller records the
// disk on the receiving side first (holder sentinel), then removes the parker
// record, preserving the D13 write ordering.
func TransferDiskFromParker(
	ctx context.Context, c Client, logger *log.Logger,
	parker DiskHolder, targetVMID int, targetSlot, bareVolid, optStr string,
	cfg ParkerConfig,
) (string, error) {
	if c == nil {
		return "", cpierrors.Cloud("TransferDiskFromParker: client must not be nil")
	}
	if !parker.Found || !parker.IsParker {
		return "", cpierrors.Cloud("TransferDiskFromParker: holder is not a parker")
	}
	if targetVMID <= 0 || targetSlot == "" || bareVolid == "" || optStr == "" {
		return "", cpierrors.Cloud("TransferDiskFromParker: target VMID, target slot, volid, and option string are all required")
	}

	var landedVolid string
	lockErr := withParkerProtectionLock(ctx, c, logger, parker.VMID, "transfer_out", func(wctx context.Context) error {
		var innerErr error
		landedVolid, innerErr = transferFromParkerLocked(wctx, c, logger, parker, targetVMID, targetSlot, bareVolid, optStr)
		return innerErr
	})
	if lockErr != nil {
		// A restore cut off after the disk landed comes back with the landed
		// name, so the caller can record the disk where it now is before it
		// reports the cut-off. Every other failure returns no name.
		var cutOff *ProtectionRestoreCutOffError
		if landedVolid != "" && errors.As(lockErr, &cutOff) && cutOff.WorkCompleted {
			return landedVolid, lockErr
		}
		return "", lockErr
	}
	_ = cfg // band/attribution config reserved for parity with the other transfer entry points
	return landedVolid, nil
}

// transferFromParkerLocked is TransferDiskFromParker's body, run inside the
// parker's protection window.
func transferFromParkerLocked(
	ctx context.Context, c Client, logger *log.Logger,
	parker DiskHolder, targetVMID int, targetSlot, bareVolid, optStr string,
) (string, error) {
	// Re-resolve the slot under the lock: the caller's holder came from a scan
	// taken before the lock was held, and both the option bake and the move
	// address the slot by name.
	vmCfg, cfgErr := c.QEMU().Config(ctx, parker.Node, parker.VMID)
	if cfgErr != nil {
		return "", cpierrors.Wrap(WrapConfigReadError(cfgErr),
			fmt.Sprintf("transfer out: config read for parker vmid %d", parker.VMID))
	}
	slot, onBus := FindDiskIDByVolID(qemu.ParseDisks(vmCfg), bareVolid)
	if !onBus {
		return "", cpierrors.Retriable(
			"transfer out: volume %q no longer on an active slot of parker vmid %d (concurrent transfer?); re-resolve and retry",
			bareVolid, parker.VMID)
	}

	// Bake the final option string onto the parker slot so the move carries
	// it. This is a same-volid option edit on a stopped VM — the attach
	// boundary D13 permits identity writes at.
	bakeErr := RetryOnTransientOrLock(ctx, logger, "disk_transfer_bake_opts", parkerWindowMaxAttempts, func() error {
		_, err := c.QEMU().AttachDisk(ctx, parker.Node, parker.VMID, optStr, "scsi", &qemu.AttachOpts{DiskID: slot})
		return err
	})
	if bakeErr != nil {
		return "", cpierrors.Wrap(WrapMutationError(bakeErr),
			fmt.Sprintf("transfer out: bake drive options on parker vmid %d slot %s", parker.VMID, slot))
	}

	// Moving a disk OFF the parker is a remove-disk operation; protection
	// blocks it. Clear for the move, restore unconditionally after.
	if protErr := setParkerProtection(ctx, c, logger, parker.Node, parker.VMID, false); protErr != nil {
		return "", cpierrors.Wrap(WrapMutationError(protErr),
			fmt.Sprintf("transfer out: clear protection on parker vmid %d", parker.VMID))
	}
	moveErr := moveDiskToVM(ctx, c, logger, parker.Node, parker.VMID, slot, targetVMID, targetSlot)
	work := fmt.Sprintf("the disk transfer to vm %d slot %s completed", targetVMID, targetSlot)
	if moveErr != nil {
		work = windowWorkEnding(fmt.Sprintf("the disk transfer to vm %d slot %s", targetVMID, targetSlot), moveErr)
	}
	restoreErr := markWorkCompleted(restoreParkerProtection(ctx, c, logger, "transfer out", parker.Node, parker.VMID, work), moveErr == nil)
	if moveErr != nil {
		return "", joinWindowErrors(moveErr, restoreErr)
	}

	// The move renamed the volume for its new owner; read the landed name off
	// the target slot. This runs even when the restore was cut off, because
	// the disk is on the VM either way and the caller needs its new name to
	// record it there; the cut-off comes back beside the name.
	targetCfg, tErr := c.QEMU().Config(ctx, parker.Node, targetVMID)
	if tErr != nil {
		return "", joinWindowErrors(cpierrors.Wrap(WrapConfigReadError(tErr),
			fmt.Sprintf("transfer out: config read for target vm %d after move", targetVMID)), restoreErr)
	}
	landed, ok := slotBareVolid(targetCfg, targetSlot)
	if !ok {
		return "", joinWindowErrors(cpierrors.Retriable(
			"transfer out: move_disk reported success but target vm %d slot %s is empty; retry",
			targetVMID, targetSlot), restoreErr)
	}
	if logger != nil {
		logger.Info("transfer out: disk reassigned from parker to VM",
			log.Int("parker_vmid", parker.VMID),
			log.Int("target_vmid", targetVMID),
			log.String("slot", targetSlot),
			log.String("volid_before", bareVolid),
			log.String("volid_after", landed),
		)
	}
	return landed, restoreErr
}

// RemoveParkerProvenanceEntry drops the provenance record naming bareVolid
// from a parker's sentinel (either keying scheme). Best-effort, exported for
// the handlers that finish an attach-side transfer: sentinel on the receiving
// VM first, then this — the giving side's record is removed last.
func RemoveParkerProvenanceEntry(ctx context.Context, c Client, logger *log.Logger, node string, parkerVMID int, bareVolid string, cfg ParkerConfig) {
	removeParkerProvenance(ctx, c, logger, node, parkerVMID, bareVolid, cfg)
}

// TransferDiskToParker moves an attached stable-ID disk from a workload VM
// onto a parker on the same node: intent record, slot delete (no sweep —
// after a reassignment the volume is named for the source VM, and PVE
// physically removes a swept unused volume its holder owns), unused-entry
// reassignment, serial re-apply, record finalize. pctx.StableID is required.
//
// Returns the volid the volume landed under on the parker.
func TransferDiskToParker(
	ctx context.Context, c Client, logger *log.Logger,
	node string, srcVMID int, bareVolid string,
	cfg ParkerConfig, pctx ParkContext,
) (string, error) {
	if c == nil {
		return "", cpierrors.Cloud("TransferDiskToParker: client must not be nil")
	}
	if node == "" || srcVMID <= 0 || bareVolid == "" {
		return "", cpierrors.Cloud("TransferDiskToParker: node, source VMID, and volid are all required")
	}
	if actual, _, err := ParseDiskCID(bareVolid); err == nil {
		cfg.DiskStorage = actual
	}
	if pctx.StableID == "" {
		return "", cpierrors.Cloud("TransferDiskToParker: a stable ID is required; legacy disks park by config edit")
	}
	if cfg.FallbackNode == "" {
		cfg.FallbackNode = node
	}

	// Parker selection mirrors parkDiskOnNode: first existing parker with a
	// free slot, then fresh parkers, bounded.
	parkers, listErr := ListParkersForNode(ctx, c, node, cfg)
	if listErr != nil {
		return "", cpierrors.Wrap(listErr, "TransferDiskToParker: list parkers")
	}
	if len(parkers) == 0 {
		parkerVMID, ensureErr := EnsureParker(ctx, c, logger, node, cfg)
		if ensureErr != nil {
			return "", cpierrors.Wrap(ensureErr, "TransferDiskToParker: ensure parker")
		}
		parkers = []int{parkerVMID}
	}
	for _, parkerVMID := range parkers {
		landed, err := transferIntoParker(ctx, c, logger, node, parkerVMID, srcVMID, bareVolid, cfg, pctx)
		if err == nil {
			return landed, nil
		}
		if isParkerCapacityError(err) {
			continue
		}
		return "", cpierrors.Wrap(err, "TransferDiskToParker: transfer to parker")
	}
	for attempt := 0; attempt < freshParkerAttempts; attempt++ {
		freshVMID, freshErr := EnsureFreshParker(ctx, c, logger, node, cfg)
		if freshErr != nil {
			return "", cpierrors.Wrap(freshErr, "TransferDiskToParker: ensure fresh parker after all parkers full")
		}
		landed, err := transferIntoParker(ctx, c, logger, node, freshVMID, srcVMID, bareVolid, cfg, pctx)
		if err == nil {
			return landed, nil
		}
		if !isParkerCapacityError(err) {
			return "", cpierrors.Wrap(err, "TransferDiskToParker: transfer to fresh parker")
		}
	}
	return "", cpierrors.Retriable(
		"TransferDiskToParker: could not find a parker on node %s with both a free slot and room in its "+
			"provenance store, after %d fresh-parker attempts",
		node, freshParkerAttempts)
}

// transferIntoParker runs one detach-side transfer attempt against a known
// parker, inside its protection window.
func transferIntoParker(
	ctx context.Context, c Client, logger *log.Logger,
	node string, parkerVMID, srcVMID int, bareVolid string,
	cfg ParkerConfig, pctx ParkContext,
) (string, error) {
	var landed string
	lockErr := withParkerProtectionLock(ctx, c, logger, parkerVMID, "transfer_in", func(wctx context.Context) error {
		var innerErr error
		landed, innerErr = transferIntoParkerLocked(wctx, c, logger, node, parkerVMID, srcVMID, bareVolid, cfg, pctx)
		return innerErr
	})
	return landed, lockErr
}

//nolint:gocognit // Sequential transfer protocol: slot choice, intent, detach, demote-find, move, serial, finalize. The step count is the protocol; splitting it would scatter the ordering the crash-window analysis depends on.
func transferIntoParkerLocked(
	ctx context.Context, c Client, logger *log.Logger,
	node string, parkerVMID, srcVMID int, bareVolid string,
	cfg ParkerConfig, pctx ParkContext,
) (string, error) {
	// 1. Choose the receiving slot on the parker.
	parkerCfg, cfgErr := c.QEMU().Config(ctx, node, parkerVMID)
	if cfgErr != nil {
		return "", cpierrors.Wrap(WrapConfigReadError(cfgErr),
			fmt.Sprintf("transfer in: config read for parker vmid %d", parkerVMID))
	}
	slot, slotErr := chooseParkSlotExcluding(qemu.ParseDisks(parkerCfg), nil)
	if slotErr != nil {
		return "", slotErr // ErrNoSlots — caller tries the next parker
	}

	// 2. Intent record, strict: from the moment the source slot is deleted
	// until the serial lands on the parker, this record is the disk's only
	// identity carrier, so the transfer must not proceed without it.
	intent := buildParkerProvEntry(ctx, node, bareVolid, slot, cfg, pctx)
	if provErr := writeParkerProvenance(ctx, c, logger, node, parkerVMID, pctx.StableID, intent, cfg); provErr != nil {
		return "", cpierrors.Wrap(provErr,
			fmt.Sprintf("transfer in: write intent record on parker vmid %d (fail-closed: the record is the crash-window identity carrier)", parkerVMID))
	}

	// 3. Delete the source slot — a raw config delete, NOT the SDK's
	// DetachDisk: its unusedN sweep physically removes a volume its holder
	// owns, which after a reassignment this volume is.
	// Both views: a slot whose delete is pending still counts as attached, so
	// the transfer sends the delete again rather than skipping it, and the
	// helper proves the source let go before anything lands on the parker.
	srcViews, srcErr := ReadQemuViews(ctx, c, node, srcVMID)
	if srcErr != nil {
		return "", cpierrors.Wrap(WrapConfigReadError(srcErr),
			fmt.Sprintf("transfer in: config read for source vm %d", srcVMID))
	}
	if actualSlot, onBus := srcViews.BusSlotNaming(bareVolid); onBus {
		// A delete PVE could only record as pending comes back as a
		// *DriveDeletePendingError, already reverted, and the wrap keeps it
		// visible to errors.As, so each caller chooses its class.
		if delErr := DeleteDriveSlot(ctx, c, logger, node, srcVMID, actualSlot, bareVolid, nil, parkerWindowMaxAttempts); delErr != nil {
			return "", cpierrors.Wrap(delErr,
				fmt.Sprintf("transfer in: detach %q (slot %s) from source vm %d", bareVolid, actualSlot, srcVMID))
		}
		srcViews, srcErr = ReadQemuViews(ctx, c, node, srcVMID)
		if srcErr != nil {
			return "", cpierrors.Wrap(WrapConfigReadError(srcErr),
				fmt.Sprintf("transfer in: re-read source vm %d after detach", srcVMID))
		}
	}
	srcCfg := srcViews.Applied()

	// 4. Find the unusedN entry PVE demoted the volume to. Absent on both the
	// active bus and the unused keys means another actor moved it mid-window;
	// hand it back retriable so the caller re-resolves.
	unusedKey := ""
	for key, volid := range FindUnusedDiskEntries(srcCfg) {
		if volid == bareVolid {
			unusedKey = key
			break
		}
	}
	if unusedKey == "" {
		return "", cpierrors.Retriable(
			"transfer in: volume %q is on neither an active nor an unused slot of source vm %d after detach; re-resolve and retry",
			bareVolid, srcVMID)
	}

	// 5. Reassign the unused entry to the parker slot. The parker's
	// protection flag does not block receiving a disk (an add, not a remove).
	if moveErr := moveDiskToVM(ctx, c, logger, node, srcVMID, unusedKey, parkerVMID, slot); moveErr != nil {
		return "", moveErr
	}

	// 6. The unused-entry path drops all drive options (live-spike result);
	// read the landed volid and re-apply the serial at this attach boundary.
	landedCfg, landedErr := c.QEMU().Config(ctx, node, parkerVMID)
	if landedErr != nil {
		return "", cpierrors.Wrap(WrapConfigReadError(landedErr),
			fmt.Sprintf("transfer in: config read for parker vmid %d after move", parkerVMID))
	}
	landed, ok := slotBareVolid(landedCfg, slot)
	if !ok {
		return "", cpierrors.Retriable(
			"transfer in: move_disk reported success but parker vmid %d slot %s is empty; retry",
			parkerVMID, slot)
	}
	serialErr := RetryOnTransientOrLock(ctx, logger, "disk_transfer_serial", parkerWindowMaxAttempts, func() error {
		_, err := c.QEMU().AttachDisk(ctx, node, parkerVMID, landed+",serial="+pctx.StableID, "scsi", &qemu.AttachOpts{DiskID: slot})
		return err
	})
	if serialErr != nil {
		return "", cpierrors.Wrap(WrapMutationError(serialErr),
			fmt.Sprintf("transfer in: re-apply serial on parker vmid %d slot %s", parkerVMID, slot))
	}

	// 7. Finalize the landed volume identity. Managed disks require durable
	// full provenance and receiving-side readback before source cleanup.
	// Legacy disks retain their serial-based best-effort behavior.
	final := intent
	final.Volid = landed
	if provErr := writeParkerProvenance(ctx, c, logger, node, parkerVMID, pctx.StableID, final, cfg); provErr != nil {
		if pctx.AllocationID != "" || pctx.AllocationNamespace != "" {
			return "", cpierrors.Cloud("managed transfer provenance persistence requires reconciliation")
		}
		if logger != nil {
			logger.Warn("transfer in: could not finalize legacy provenance", log.Err(provErr))
		}
	}
	if pctx.AllocationID != "" || pctx.AllocationNamespace != "" {
		if err := VerifyAllocationParked(ctx, c, logger, landed, pctx.StableID, pctx.AllocationNamespace, pctx.AllocationID, cfg); err != nil {
			return "", cpierrors.Cloud("managed transfer provenance readback requires reconciliation")
		}
	}
	reassertParkerProtection(ctx, c, logger, node, parkerVMID)
	if logger != nil {
		logger.Info("transfer in: disk reassigned from VM to parker",
			log.Int("source_vmid", srcVMID),
			log.Int("parker_vmid", parkerVMID),
			log.String("slot", slot),
			log.String("volid_before", bareVolid),
			log.String("volid_after", landed),
		)
	}
	return landed, nil
}

// ResumeDiskTransferToParker completes a detach-side transfer a crash left
// mid-flight, working from the intent record the resolver found. It converges
// the disk to "parked with its serial applied" and returns the parked volid.
//
// The windows it distinguishes, inside the parker's protection lock:
//
//   - serial already on a parker slot: only the finalize was lost — rewrite
//     the record and finish.
//   - source VM still holds the recorded volid on an unusedN entry: the move
//     never ran — re-run it (re-choosing the slot; the recorded one may have
//     been taken while the lock was down).
//   - the recorded parker slot holds a parker-named volume with no serial:
//     the move landed but the serial write was lost — claim it.
//
// Anything else is a state this code cannot safely converge; it returns a
// permanent error naming what it found so an operator can look.
//
//nolint:gocognit // Case analysis over the transfer's crash windows; each branch is one window and the ordering between them is load-bearing.
func ResumeDiskTransferToParker(
	ctx context.Context, c Client, logger *log.Logger,
	intent DiskTransferIntent, stableID string,
	cfg ParkerConfig, pctx ParkContext,
) (string, error) {
	if c == nil {
		return "", cpierrors.Cloud("ResumeDiskTransferToParker: client must not be nil")
	}
	if stableID == "" || intent.ParkerVMID <= 0 || intent.ParkerNode == "" {
		return "", cpierrors.Cloud("ResumeDiskTransferToParker: stable ID and parker identity are required")
	}
	var contextErr error
	pctx, cfg, contextErr = resumeDiskTransferContext(intent, stableID, cfg, pctx)
	if contextErr != nil {
		return "", contextErr
	}

	var landed string
	lockErr := withParkerProtectionLock(ctx, c, logger, intent.ParkerVMID, "transfer_resume", func(wctx context.Context) error {
		parkerCfg, cfgErr := c.QEMU().Config(wctx, intent.ParkerNode, intent.ParkerVMID)
		if cfgErr != nil {
			return cpierrors.Wrap(WrapConfigReadError(cfgErr),
				fmt.Sprintf("transfer resume: config read for parker vmid %d", intent.ParkerVMID))
		}

		// Window: everything landed, only the finalize write was lost.
		disks := qemu.ParseDisks(parkerCfg)
		for slot, optStr := range disks {
			serial, has := StableIDFromDriveOptStr(optStr)
			if !has || serial != stableID {
				continue
			}
			bare := optStr
			if comma := strings.IndexByte(bare, ','); comma >= 0 {
				bare = bare[:comma]
			}
			landed = bare
			return finalizeResumedTransfer(wctx, c, logger, intent, stableID, slot, bare, cfg, pctx)
		}

		// Window: the move never ran — the source VM still holds the volume
		// on an unusedN entry under its recorded (pre-move) name.
		if srcVMID, convErr := strconv.Atoi(intent.SourceVMCID); convErr == nil && srcVMID > 0 && intent.Volid != "" {
			srcViews, srcErr := ReadQemuViews(wctx, c, intent.ParkerNode, srcVMID)
			switch {
			case srcErr != nil && !parkerConfigGone(srcErr):
				return cpierrors.Wrap(WrapConfigReadError(srcErr),
					fmt.Sprintf("transfer resume: config read for source vm %d", srcVMID))
			case srcErr == nil:
				srcCfg := srcViews.Applied()
				if slot, onBus := srcViews.BusSlotNaming(intent.Volid); onBus {
					if srcViews.PendingDelete(slot) {
						// A crash, an earlier release, or an operator left the
						// source's delete pending, so the running guest still
						// has the disk, and nothing may land on the parker until
						// the VM lets go of it. The resume can't tell whose
						// delete this is or what its caller wants, and a stop
						// of the VM applies it, so it leaves it alone and hands
						// back the typed error for the caller to class.
						return &DriveDeletePendingError{Reason: DriveDeletePendingFound, Node: intent.ParkerNode, VMID: srcVMID, Slot: slot}
					}
					// Still attached: the detach never happened, so this is not
					// a resume at all. The identity scan should have found it;
					// a race between the scan and this read is the only path
					// here.
					return cpierrors.Retriable(
						"transfer resume: volume %q is still attached to source vm %d; re-resolve and retry",
						intent.Volid, srcVMID)
				}
				for key, volid := range FindUnusedDiskEntries(srcCfg) {
					if volid != intent.Volid {
						continue
					}
					slot, slotErr := resumeTargetSlot(disks, intent.Slot)
					if slotErr != nil {
						return slotErr
					}
					if moveErr := moveDiskToVM(wctx, c, logger, intent.ParkerNode, srcVMID, key, intent.ParkerVMID, slot); moveErr != nil {
						return moveErr
					}
					afterCfg, afterErr := c.QEMU().Config(wctx, intent.ParkerNode, intent.ParkerVMID)
					if afterErr != nil {
						return cpierrors.Wrap(WrapConfigReadError(afterErr),
							fmt.Sprintf("transfer resume: config read for parker vmid %d after move", intent.ParkerVMID))
					}
					bare, ok := slotBareVolid(afterCfg, slot)
					if !ok {
						return cpierrors.Retriable(
							"transfer resume: move_disk reported success but parker vmid %d slot %s is empty; retry",
							intent.ParkerVMID, slot)
					}
					if serialErr := applyResumedSerial(wctx, c, logger, intent, stableID, slot, bare); serialErr != nil {
						return serialErr
					}
					landed = bare
					return finalizeResumedTransfer(wctx, c, logger, intent, stableID, slot, bare, cfg, pctx)
				}
			}
		}

		// Window: the move landed on the recorded slot but the serial write
		// was lost. Claimable only when the slot holds a parker-named volume
		// carrying no stable-ID serial — anything else is not this transfer.
		if intent.Slot != "" {
			if bare, ok := slotBareVolid(parkerCfg, intent.Slot); ok {
				_, hasSerial := StableIDFromDriveOptStr(disks[intent.Slot])
				if embedded, named := EmbeddedDiskVMID(bare); named && embedded == intent.ParkerVMID && !hasSerial {
					if serialErr := applyResumedSerial(wctx, c, logger, intent, stableID, intent.Slot, bare); serialErr != nil {
						return serialErr
					}
					landed = bare
					return finalizeResumedTransfer(wctx, c, logger, intent, stableID, intent.Slot, bare, cfg, pctx)
				}
			}
		}

		// Window: the move never ran and the source VM no longer exists (or no
		// longer references the volume anywhere). A snapshot-deferred detach
		// leaves the volume on the source VM's unusedN entry until the snapshot
		// is deleted; a delete_vm in that window destroys the source VM while
		// the volume — still birth-named, since only move_disk renames —
		// survives free-floating (PVE only deallocates volumes named for the
		// VM being destroyed). With no source config entry left there is
		// nothing to reassign, so converge by the config-edit park: attach the
		// recorded volid onto this parker with the serial baked, the same
		// attach boundary a legacy park uses. PVE validates the volid on
		// attach, so a volume that truly vanished fails the attach instead of
		// parking a dangling reference.
		if srcVMID, convErr := strconv.Atoi(intent.SourceVMCID); convErr == nil && srcVMID > 0 && intent.Volid != "" {
			srcViews, srcErr := ReadQemuViews(wctx, c, intent.ParkerNode, srcVMID)
			sourceReleased := false
			switch {
			case srcErr != nil:
				// Window 2 already returned every non-gone read error, so a
				// second failing read here is either the same gone answer or a
				// fault that appeared mid-resume; only the gone answer proves
				// the source released the volume.
				sourceReleased = parkerConfigGone(srcErr)
			default:
				// Released only when neither view names the volume: a pending
				// delete leaves the volume plugged into the running guest.
				sourceReleased = !srcViews.NamesVolume(intent.Volid)
			}
			if sourceReleased {
				slot, attachErr := attachToParkerLocked(wctx, c, logger, intent.ParkerNode, intent.ParkerVMID, intent.Volid, stableID)
				if attachErr != nil {
					return attachErr
				}
				landed = intent.Volid
				return finalizeResumedTransfer(wctx, c, logger, intent, stableID, slot, intent.Volid, cfg, pctx)
			}
		}

		return cpierrors.Cloud(
			"transfer resume: disk %s has an intent record on parker vmid %d (node %s, slot %q, recorded volid %q, source %q) "+
				"but neither the parker nor the source VM holds a state this transfer can converge; inspect the parker's "+
				"slots and the source VM's unused entries by hand before retrying",
			stableID, intent.ParkerVMID, intent.ParkerNode, intent.Slot, intent.Volid, intent.SourceVMCID)
	})
	if lockErr != nil {
		return "", lockErr
	}
	return landed, nil
}

// resumeTargetSlot prefers the intent's recorded slot when it is still free
// and falls back to a fresh choice — a concurrent park may have taken the
// recorded one while the crashed transfer's lock was expired.
func resumeTargetSlot(disks map[string]string, recorded string) (string, error) {
	if recorded != "" {
		if _, occupied := disks[recorded]; !occupied {
			return recorded, nil
		}
	}
	return chooseParkSlotExcluding(disks, nil)
}

// applyResumedSerial re-applies the stable-ID serial on a resumed transfer's
// landed slot.
func applyResumedSerial(ctx context.Context, c Client, logger *log.Logger, intent DiskTransferIntent, stableID, slot, bare string) error {
	serialErr := RetryOnTransientOrLock(ctx, logger, "disk_transfer_serial", parkerWindowMaxAttempts, func() error {
		_, err := c.QEMU().AttachDisk(ctx, intent.ParkerNode, intent.ParkerVMID, bare+",serial="+stableID, "scsi", &qemu.AttachOpts{DiskID: slot})
		return err
	})
	if serialErr != nil {
		return cpierrors.Wrap(WrapMutationError(serialErr),
			fmt.Sprintf("transfer resume: re-apply serial on parker vmid %d slot %s", intent.ParkerVMID, slot))
	}
	return nil
}

// finalizeResumedTransfer rewrites the provenance record with the landed
// volid and re-asserts protection. Best-effort on both counts: the serial on
// the slot is the authoritative carrier by the time this runs.
func finalizeResumedTransfer(
	ctx context.Context, c Client, logger *log.Logger,
	intent DiskTransferIntent, stableID, slot, landed string,
	cfg ParkerConfig, pctx ParkContext,
) error {
	entry := buildParkerProvEntry(ctx, intent.ParkerNode, landed, slot, cfg, pctx)
	if provErr := writeParkerProvenance(ctx, c, logger, intent.ParkerNode, intent.ParkerVMID, stableID, entry, cfg); provErr != nil {
		if pctx.AllocationID != "" || pctx.AllocationNamespace != "" {
			return cpierrors.Cloud("managed transfer provenance persistence requires reconciliation")
		}
		if logger != nil {
			logger.Warn("transfer resume: could not finalize legacy provenance", log.Err(provErr))
		}
	}
	reassertParkerProtection(ctx, c, logger, intent.ParkerNode, intent.ParkerVMID)
	if pctx.AllocationID != "" || pctx.AllocationNamespace != "" {
		return VerifyAllocationParked(ctx, c, logger, landed, stableID, pctx.AllocationNamespace, pctx.AllocationID, cfg)
	}
	return nil
}

// DeleteParkedOwnedDisk deletes a parked stable-ID disk whose volume is named
// for the parker holding it (the state every transferred disk parks in). The
// legacy unpark-then-DeleteVolume sequence cannot run here — the unpark's
// sweep semantics are exactly the deallocation being requested, and its
// safety guard rightly refuses owner-named removals it cannot prove are
// intentional. This function makes the intent explicit: detach the slot and
// let PVE's owned-volume unused sweep deallocate the disk, under the
// protection window, verified.
func DeleteParkedOwnedDisk(
	ctx context.Context, c Client, logger *log.Logger,
	node string, parkerVMID int, bareVolid string, cfg ParkerConfig,
) error {
	if c == nil {
		return cpierrors.Cloud("DeleteParkedOwnedDisk: client must not be nil")
	}
	if node == "" || parkerVMID <= 0 || bareVolid == "" {
		return cpierrors.Cloud("DeleteParkedOwnedDisk: node, parker VMID, and volid are all required")
	}
	if embedded, ok := EmbeddedDiskVMID(bareVolid); !ok || embedded != parkerVMID {
		return cpierrors.Cloud(
			"DeleteParkedOwnedDisk: volume %q is not named for parker vmid %d; use the ordinary unpark-and-delete path",
			bareVolid, parkerVMID)
	}

	lockErr := withParkerProtectionLock(ctx, c, logger, parkerVMID, "delete_parked", func(wctx context.Context) error {
		return deleteParkedOwnedDiskLocked(wctx, c, logger, node, parkerVMID, bareVolid)
	})
	if lockErr != nil {
		// A restore cut off after the deletion completed says so. The volume
		// is gone either way, so
		// its provenance entry is removed as on success, and the cut-off is
		// reported after it; otherwise the entry would name a volume that no
		// longer exists and every later lookup of the disk would refuse it.
		var cutOff *ProtectionRestoreCutOffError
		if errors.As(lockErr, &cutOff) && cutOff.WorkCompleted {
			removeParkerProvenance(ctx, c, logger, node, parkerVMID, bareVolid, cfg)
		}
		return lockErr
	}
	removeParkerProvenance(ctx, c, logger, node, parkerVMID, bareVolid, cfg)
	return nil
}

// deleteParkedOwnedDiskLocked is DeleteParkedOwnedDisk's body, run inside the
// parker's protection window.
func deleteParkedOwnedDiskLocked(ctx context.Context, c Client, logger *log.Logger, node string, parkerVMID int, bareVolid string) error {
	vmCfg, cfgErr := c.QEMU().Config(ctx, node, parkerVMID)
	if cfgErr != nil {
		if parkerConfigGone(cfgErr) {
			return nil
		}
		return cpierrors.Wrap(WrapConfigReadError(cfgErr),
			fmt.Sprintf("delete parked: config read for parker vmid %d", parkerVMID))
	}
	slot, onBus := FindDiskIDByVolID(qemu.ParseDisks(vmCfg), bareVolid)
	if !onBus && !unusedEntriesReference(vmCfg, bareVolid) {
		// Nothing on the parker references the volume: a previous attempt
		// already deallocated it (or it was never here). Idempotent success.
		return nil
	}

	if protErr := setParkerProtection(ctx, c, logger, node, parkerVMID, false); protErr != nil {
		return cpierrors.Wrap(WrapMutationError(protErr),
			fmt.Sprintf("delete parked: clear protection on parker vmid %d", parkerVMID))
	}
	var detachErr error
	if onBus {
		// The SDK's DetachDisk demotes the slot and sweeps the resulting
		// unusedN entry; on a volume this parker owns, the sweep IS the
		// deletion.
		detachErr = RetryOnTransientOrLock(ctx, logger, "delete_parked_detach", parkerWindowMaxAttempts, func() error {
			return c.QEMU().DetachDisk(ctx, node, parkerVMID, slot)
		})
	}
	// Sweep any unusedN entry still naming the volume — a prior partial
	// attempt, or a detach whose second request failed. Own-named removal is
	// the intended deallocation here.
	if detachErr == nil {
		detachErr = sweepOwnedUnusedEntries(ctx, c, logger, node, parkerVMID, bareVolid)
	}
	work := fmt.Sprintf("the deletion of %q completed", bareVolid)
	if detachErr != nil {
		detachErr = cpierrors.Wrap(WrapMutationError(detachErr),
			fmt.Sprintf("delete parked: deallocate %q on parker vmid %d", bareVolid, parkerVMID))
		work = windowWorkEnding(fmt.Sprintf("the deletion of %q", bareVolid), detachErr)
	}
	restoreErr := markWorkCompleted(restoreParkerProtection(ctx, c, logger, "delete parked", node, parkerVMID, work), detachErr == nil)
	return joinWindowErrors(detachErr, restoreErr)
}

// markWorkCompleted records on a cut-off restore whether the window's own
// change completed. Any other error, and nil, passes through unchanged.
func markWorkCompleted(restoreErr error, completed bool) error {
	var cutOff *ProtectionRestoreCutOffError
	if errors.As(restoreErr, &cutOff) {
		cutOff.WorkCompleted = completed
	}
	return restoreErr
}

// windowWorkEnding says how a protection window's own change ended when it
// returned err, for the work string a cut-off restore carries. work names the
// change, for example "the disk transfer to vm 700 slot scsi1". It says the
// change failed only when PVE gave a verdict: a task that exited with a
// failure, the snapshot refusal, or another answer pveAnswered accepts. A
// dropped connection, a poll that gave up, or an ended context leaves the
// change's outcome unknown, because PVE may still have applied it, and the
// string says so.
func windowWorkEnding(work string, err error) string {
	if _, answered := pveAnswered(err); answered || IsTaskExitVerdict(err) || IsMoveDiskSnapshotRefusal(err) {
		return work + " failed"
	}
	return "the outcome of " + work + " is unknown"
}

// joinWindowErrors combines a protection window's own error with its restore's
// error as errors.Join(workErr, restoreErr), except that a single error comes
// back as itself rather than inside a join, so callers that compare or unwrap
// it see exactly what they always did.
//
// When both are present, the work error wins classification. errors.As walks a
// join in order, so the dispatcher, retriableUnlessPermanent, and every other
// caller that reads the CPI error type find the work error's type first, and a
// permanent move or deletion verdict stays permanent. The restore error's type
// applies only when the work error carries no CPI type of its own, such as the
// untyped snapshot refusal. The restore's text rides along in both cases, so
// the operator still sees that protection needs checking.
func joinWindowErrors(workErr, restoreErr error) error {
	switch {
	case workErr == nil:
		return restoreErr
	case restoreErr == nil:
		return workErr
	default:
		return errors.Join(workErr, restoreErr)
	}
}

// parkerProtectionRestoreTimeout bounds one protection restore. It is the
// reserve parkerWindowBudget sets aside for the restore, so a window body that
// runs its whole budget, then a restore that runs its whole deadline, the
// demoted-slot sweep, and the lock release, still ends before the lock's TTL,
// where a waiter may steal the lock and open its own window on this parker.
const parkerProtectionRestoreTimeout = parkerProtectionRestoreReserve

// parkerRestoreTimeoutKey carries a test's shorter protection restore
// deadline on the request context.
type parkerRestoreTimeoutKey struct{}

// WithParkerProtectionRestoreTimeoutForTest returns a context whose protection
// restores use deadline d instead of parkerProtectionRestoreTimeout. It rides
// the context rather than a package variable so tests that shorten it can run
// in parallel. A non-positive d leaves ctx as it is. Production code never
// calls it; it mirrors WithTestBackoff.
func WithParkerProtectionRestoreTimeoutForTest(ctx context.Context, d time.Duration) context.Context {
	if d <= 0 {
		return ctx
	}
	return context.WithValue(ctx, parkerRestoreTimeoutKey{}, d)
}

// parkerRestoreTimeout returns the test override carried on ctx when one is
// set, and parkerProtectionRestoreTimeout otherwise.
func parkerRestoreTimeout(ctx context.Context) time.Duration {
	if d, ok := ctx.Value(parkerRestoreTimeoutKey{}).(time.Duration); ok && d > 0 {
		return d
	}
	return parkerProtectionRestoreTimeout
}

// ErrMutationNotAttempted marks a mutation that a client wrapper refused
// before sending it to PVE, such as the allocation guard once an earlier
// failure has made its operation uncertain. The write never reached PVE, so
// its failure says nothing about PVE or the network.
var ErrMutationNotAttempted = errors.New("mutation not attempted")

// ErrMutationChecksIncomplete marks a mutation that a client wrapper did not
// send because it could not finish the checks it makes before sending one,
// such as a read the allocation guard makes to admit a parker protection
// restore that failed or ran out of time. The mutation never reached PVE, so
// whatever those checks guard is still as it was. It differs from
// ErrMutationNotAttempted, where the wrapper refused because an earlier
// failure had already made its operation uncertain.
var ErrMutationChecksIncomplete = errors.New("mutation checks incomplete")

// ProtectionWriteRefused reports whether err is PVE's own answer refusing a
// protection write, which means the write did not apply and its outcome is
// known. pveAnswered says which errors are PVE's own answer.
func ProtectionWriteRefused(err error) bool {
	_, answered := pveAnswered(err)
	return answered
}

// pveAnswered reports whether err is PVE's own answer refusing a request, and
// returns its HTTP status when it is. That is an API error carrying a 4xx or
// 5xx status, except 596, which pveproxy sends when it could not reach
// pvedaemon, and 502 through 504, which a proxy in front of PVE sends. It is
// false for a transport fault, a timeout, and a cancelled or expired context,
// where the request may have been applied.
func pveAnswered(err error) (int, bool) {
	if err == nil || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return 0, false
	}
	code, ok := apiHTTPCode(err)
	if !ok {
		return 0, false
	}
	switch code {
	case 502, 503, 504, 596:
		return 0, false
	}
	if code < 400 || code >= 600 {
		return 0, false
	}
	return code, true
}

// ProtectionRestoreCutOffError is the error a protection restore returns when
// it did not put protection back and PVE never refused it. That covers a
// restore that ended without an answer from PVE, because its deadline cut it
// off or its transport failed, so nobody knows whether protection went back
// on. It also covers a restore the client never sent, because an earlier
// failure made the operation uncertain or the checks before the write could
// not finish, and then protection is known to be still off.
// Its text and its CPI error type (retriable) come from the error it wraps, so
// callers that only read the message or the type see no difference. Callers
// that need to tell a cut-off restore from every other failure, such as an
// attach that still has receiving-side bookkeeping to finish after the disk
// landed, match it with errors.As.
type ProtectionRestoreCutOffError struct {
	// ParkerVMID is the parker whose restore ended without an answer or was
	// never sent, so its protection is unknown or still off.
	ParkerVMID int
	// WorkCompleted is true when the window's own change, the transfer or
	// the deletion, completed before the restore was cut off. The caller
	// then still records where the disk is, or that it is gone.
	WorkCompleted bool
	err           error
}

func (e *ProtectionRestoreCutOffError) Error() string { return e.err.Error() }

func (e *ProtectionRestoreCutOffError) Unwrap() error { return e.err }

// putParkerProtectionBack writes protection back on a parker at the end of a
// protection window. Every protection restore goes through it, whether its
// caller returns a failed restore or only logs it. reassertParkerProtection
// is a different write: it re-asserts the flag after a park or a completed
// migration, on that window's own live context.
//
// The write runs on context.WithoutCancel(ctx), so a request that was
// cancelled, or a window whose deadline stopped the work, still puts the flag
// back. Without a deadline of its own, though, a PVE that never answers would
// hold the restore open past the lock's TTL, so it gets
// parkerRestoreTimeout. It returns the write's error and whether that
// deadline cut the write off.
func putParkerProtectionBack(ctx context.Context, c Client, logger *log.Logger, node string, parkerVMID int) (bool, error) {
	restoreCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), parkerRestoreTimeout(ctx))
	defer cancel()
	protErr := setParkerProtection(restoreCtx, c, logger, node, parkerVMID, true)
	return errors.Is(restoreCtx.Err(), context.DeadlineExceeded), protErr
}

// protectionRestoreEnding says how a failed protection restore ended, in the
// words both the returned error and the log use, for every restore that ends
// in the retriable error. It returns "" when PVE answered with a failure,
// because then the write provably did not apply and protection is off, and
// that restore only warns.
//
// A restore the client did not send because it could not finish the checks
// before it is checked first. The window's protection-off write was observed
// and nothing was sent after it, so protection is still off, and the ending
// says whether those checks failed or ran out of time. A restore the client
// refused before sending, because an earlier failure already made the
// operation uncertain, comes next. PVE never saw either one, so blaming the
// network or PVE would mislead.
func protectionRestoreEnding(protErr error, timedOut bool, timeout time.Duration) string {
	switch {
	case errors.Is(protErr, ErrMutationChecksIncomplete):
		if timedOut {
			return fmt.Sprintf("was not sent because the checks before it did not finish within %s, so protection is still off", timeout)
		}
		return "was not sent because the checks before it failed, so protection is still off"
	case errors.Is(protErr, ErrMutationNotAttempted):
		return "was not attempted because the operation was already uncertain"
	case timedOut:
		return fmt.Sprintf("did not finish within %s and its outcome is unknown", timeout)
	case !ProtectionWriteRefused(protErr):
		return "ended without an answer from PVE and its outcome is unknown"
	default:
		return ""
	}
}

// restoreParkerProtectionLogged puts protection back on a parker, or on a
// mover, for a window whose contract is to log a failed restore rather than
// return it: the unpark, its sweep of a demoted reference, the park path's
// deferred sweep, and a mover's migration that failed. Those callers act on
// their own result, and a later park re-asserts the flag. op names the window
// in the log line, for example "UnparkDisk".
//
// The restore goes through putParkerProtectionBack, so it ends at the same
// deadline as the transfer and deletion restores. The one Warn it logs uses
// the same endings as restoreParkerProtection, so an operator reading it can
// tell a restore PVE refused from one whose outcome is unknown, and both of
// those from one that was never sent because the checks before it did not
// finish. In a journal-managed request, a cut-off write leaves its step
// planned, and so does a restore whose checks did not finish, so the
// operation's record still says the restore is unsettled.
func restoreParkerProtectionLogged(ctx context.Context, c Client, logger *log.Logger, op, node string, parkerVMID int) {
	timedOut, protErr := putParkerProtectionBack(ctx, c, logger, node, parkerVMID)
	if protErr == nil || logger == nil {
		return
	}
	timeout := parkerRestoreTimeout(ctx)
	if ending := protectionRestoreEnding(protErr, timedOut, timeout); ending != "" {
		logger.Warn(op+": protection restore on parker "+ending+"; check the parker with qm config <vmid> "+
			"and run qm set <vmid> --protection 1 if protection is off",
			log.Int("parker_vmid", parkerVMID),
			log.String("node", node),
			log.String("timeout", timeout.String()),
			log.Err(protErr),
		)
		return
	}
	logger.Warn(op+": could not restore protection on parker — re-set it by hand (qm set <vmid> --protection 1)",
		log.Int("parker_vmid", parkerVMID),
		log.String("node", node),
		log.Err(protErr),
	)
}

// restoreParkerProtection puts protection back on a parker at the end of a
// transfer or deletion window and reports a restore whose outcome is unknown.
// op names the window in log lines and in the error, for example "transfer
// out", and work says how the window's own change ended, for example "the
// disk transfer to vm 700 slot scsi1 completed", so an operator reading a
// cut-off restore knows whether the disk moved.
//
// The write goes through putParkerProtectionBack, so a cancelled request still
// sends it, and parkerRestoreTimeout bounds it.
//
// Five outcomes:
//
//   - The restore succeeds: nil.
//   - PVE answers with a failure (ProtectionWriteRefused): the outcome is
//     known, protection is off, and the Warn tells the operator how to put it
//     back. It returns nil, as it always has, so the window's own result
//     stands.
//   - The client did not send the restore because it could not finish the
//     checks it makes before the write (ErrMutationChecksIncomplete), for
//     example because a read the allocation guard needs failed or ran out of
//     time. The outcome is known: protection is still off, because the
//     window's protection-off write was observed and nothing was sent after
//     it. It returns the same retriable error, worded to say the restore was
//     not sent and whether the checks failed or did not finish in time. In a
//     journal-managed request the guard has left the restore planned without
//     locking itself, the same way it leaves a cut-off write.
//   - The client refused the restore before sending it
//     (ErrMutationNotAttempted), because an earlier failure already made the
//     operation uncertain: it returns the same retriable error, worded to say
//     the restore was not attempted, since PVE never saw it.
//   - The restore ends without an answer from PVE, because the deadline cut
//     it off or the transport failed on the last attempt: nobody knows
//     whether the write landed. It returns a retriable error that names the
//     protection restore,
//     says how the window's change ended, and gives the commands that check
//     and fix the flag. Retriable because a retry does come back to this
//     parker: the Director's in-task create_vm retry reuses the same
//     disk_cids, a rerun deploy reissues attach_disk, and the restore is
//     idempotent. In a journal-managed request the guard leaves the cut-off
//     write's step planned without locking itself, so the operation can still
//     record where the disk is, and the lifecycle then leaves the allocation
//     reconciliation_required. That record, not this error's type, is what
//     holds a managed retry until the parker reads back protected.
//
// Both the Warn and the returned error carry the underlying error, which can
// hold PVE text; log.Err scrubs the log line, and the dispatcher scrubs the
// error before it reaches the Director.
func restoreParkerProtection(ctx context.Context, c Client, logger *log.Logger, op, node string, parkerVMID int, work string) error {
	timedOut, protErr := putParkerProtectionBack(ctx, c, logger, node, parkerVMID)
	if protErr == nil {
		return nil
	}
	timeout := parkerRestoreTimeout(ctx)
	if ending := protectionRestoreEnding(protErr, timedOut, timeout); ending != "" {
		if logger != nil {
			logger.Warn(op+": protection restore on parker "+ending+"; check the parker's protection flag",
				log.Int("parker_vmid", parkerVMID),
				log.String("node", node),
				log.String("timeout", timeout.String()),
				log.String("work", work),
				log.Err(protErr),
			)
		}
		return &ProtectionRestoreCutOffError{ParkerVMID: parkerVMID, err: cpierrors.WrapAs(protErr, cpierrors.TypeRetriableCloud, fmt.Sprintf(
			"%s: protection restore on parker vmid %d %s; %s; "+
				"check the parker with qm config %d and run qm set %d --protection 1 if protection is off",
			op, parkerVMID, ending, work, parkerVMID, parkerVMID))}
	}
	if logger != nil {
		logger.Warn(op+": could not restore protection on parker — re-set it by hand (qm set <vmid> --protection 1)",
			log.Int("parker_vmid", parkerVMID),
			log.String("node", node),
			log.Err(protErr),
		)
	}
	return nil
}

// sweepOwnedUnusedEntries removes every unusedN entry naming bareVolid on a
// parker, verified with a re-read. Unlike sweepParkerUnusedSlots this variant
// is for deletion: PVE deallocating the owner-named volume is the goal.
func sweepOwnedUnusedEntries(ctx context.Context, c Client, logger *log.Logger, node string, parkerVMID int, bareVolid string) error {
	cfg, err := c.QEMU().Config(ctx, node, parkerVMID)
	if err != nil {
		if parkerConfigGone(err) {
			return nil
		}
		return cpierrors.Wrap(WrapConfigReadError(err),
			fmt.Sprintf("delete parked: re-read parker vmid %d", parkerVMID))
	}
	for slot, volid := range FindUnusedDiskEntries(cfg) {
		if volid != bareVolid {
			continue
		}
		sweepErr := RetryOnTransientOrLock(ctx, logger, "delete_parked_sweep", parkerWindowMaxAttempts, func() error {
			return c.QEMU().DetachDisk(ctx, node, parkerVMID, slot)
		})
		if sweepErr != nil && !IsNotFound(sweepErr) {
			return cpierrors.Wrap(WrapMutationError(sweepErr),
				fmt.Sprintf("delete parked: remove %s referencing %q on parker vmid %d", slot, bareVolid, parkerVMID))
		}
	}
	verifyCfg, verifyErr := c.QEMU().Config(ctx, node, parkerVMID)
	if verifyErr != nil {
		if parkerConfigGone(verifyErr) {
			return nil
		}
		return cpierrors.Wrap(WrapConfigReadError(verifyErr),
			fmt.Sprintf("delete parked: verify read for parker vmid %d", parkerVMID))
	}
	if unusedEntriesReference(verifyCfg, bareVolid) {
		return cpierrors.Cloud(
			"delete parked: unused entry referencing %q survived removal on parker vmid %d", bareVolid, parkerVMID)
	}
	return nil
}

func resumeDiskTransferContext(intent DiskTransferIntent, stableID string, cfg ParkerConfig, pctx ParkContext) (ParkContext, ParkerConfig, error) {
	pctx.StableID = stableID
	if pctx.AllocationID == "" {
		pctx.AllocationID = intent.AllocationID
		pctx.AllocationNamespace = intent.AllocationNamespace
		pctx.AllocationBacking = intent.AllocationBacking
	}
	if pctx.AllocationID != intent.AllocationID || pctx.AllocationNamespace != intent.AllocationNamespace {
		return pctx, cfg, cpierrors.Cloud("transfer resume: allocation provenance conflict")
	}
	if actual, _, err := ParseDiskCID(intent.Volid); err == nil {
		cfg.DiskStorage = actual
	}
	// The finalize rewrites the whole provenance entry from pctx, so a resume
	// that omitted the recorded option overrides would silently drop them.
	if len(pctx.Opts) == 0 {
		pctx.Opts = intent.Opts
	}

	return pctx, cfg, nil
}
