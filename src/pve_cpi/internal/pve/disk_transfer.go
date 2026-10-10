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
// parker slot afterwards. PVE keeps that unused entry only for a volume the
// source owns, so a volume named for any other VMID loses its last reference
// with the slot delete, and the parker takes it by config edit instead, under
// the name it already has. The crash window that opens between those steps is
// covered by write ordering: the receiving parker's provenance record (the
// intent) is written BEFORE the source slot is deleted, so a scan always
// finds at least one identity carrier.
package pve

import (
	"context"
	"errors"
	"fmt"
	"reflect"
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

// moveSnapshotRefusalText is the message PVE's reassign check dies with when
// a snapshot or another drive key of the source VM still names the volume.
const moveSnapshotRefusalText = "Can't move disk used by a snapshot to another VM"

// IsMoveSnapshotRefusalAnswer reports whether err is PVE's own answer to a
// move_disk request that it refused because the volume is still in use. PVE
// makes that check in the request, before it forks a task or changes any
// configuration, so the answer proves that the request moved nothing. It
// needs PVE's whole sentence and an HTTP answer from PVE. A task's exit status
// with the same text, the CPI's own snapshot refusal, and a dropped
// connection don't count, so it is narrower than IsMoveDiskSnapshotRefusal.
// IsMoveSnapshotRefusalTaskExit covers the task's exit status.
func IsMoveSnapshotRefusalAnswer(err error) bool {
	if err == nil || !strings.Contains(err.Error(), moveSnapshotRefusalText) {
		return false
	}
	_, answered := pveAnswered(err)
	return answered
}

// IsMoveSnapshotRefusalTaskExit reports whether err is a finished move task
// whose exit status is PVE's snapshot refusal and nothing else. The task makes
// the same in-use check again under both configuration locks before its
// rename, and PVE keeps a worker's die message as the task's exit status, so
// the exit proves that the task moved nothing. The status has to be the whole
// sentence, so a longer status that contains it doesn't count, and neither
// does a poll fault or a task whose status is unknown.
func IsMoveSnapshotRefusalTaskExit(err error) bool {
	return err != nil && strings.Contains(err.Error(), fmt.Sprintf("failed: exit status %q", moveSnapshotRefusalText))
}

// stopOnMoveSnapshotRefusal ends the move's retry loop on PVE's answer to the
// request when it refuses for a snapshot. PVE sends that answer as a 500,
// which reads as a transient fault to the loop, and it refuses again for as
// long as anything still names the volume. The refusal leaves the loop with
// its text, which is what IsMoveDiskSnapshotRefusal and the callers read, but
// without the transport error underneath it. A task's exit status with the
// same text needs no stop, because a task verdict never reads as transient.
// Any other error comes back unchanged.
func stopOnMoveSnapshotRefusal(err error) error {
	if !IsMoveDiskSnapshotRefusal(err) {
		return err
	}
	return errors.New(err.Error())
}

// ErrMoveDiskDigestRefused wraps PVE's refusal of a move_disk reassignment
// whose source or target configuration changed after the CPI read the digest
// it sent. PVE compares both digests before it renames anything, once in the
// request and again inside the task under both configuration locks, so the
// refusal proves that its own request or task changed nothing. moveDiskToVM
// returns it only when no earlier POST of the call went unanswered, or when a
// readback shows the disk still where it was.
var ErrMoveDiskDigestRefused = errors.New("move_disk refused: configuration changed after its digest was read")

// moveDigestRefusalText is the message PVE's assert_if_modified dies with.
// The move handler prefixes it with "VM <vmid>: " for the side that changed.
const moveDigestRefusalText = "detected modified configuration - file changed by other user"

// IsMoveDigestRefusal reports whether err is PVE's digest refusal of a
// move_disk reassignment, either as the request's own answer or as the move
// task's exit status. It is meant for move results only. A configuration
// update with a stale digest fails with the same text, and that failure keeps
// its own handling.
func IsMoveDigestRefusal(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrMoveDiskDigestRefused) {
		return true
	}
	if !strings.Contains(err.Error(), moveDigestRefusalText) {
		return false
	}
	_, answered := pveAnswered(err)
	return answered || IsTaskExitVerdict(err)
}

// stopOnMoveDigestRefusal ends the move's retry loop on a digest refusal. A
// 500 reads as a transient fault to the loop, and a retry would read fresh
// digests and could move whatever volume holds the disk key by then, so the
// refusal leaves the loop as ErrMoveDiskDigestRefused with PVE's text and
// without the transport error underneath it.
func stopOnMoveDigestRefusal(err error) error {
	return fmt.Errorf("%w: %s", ErrMoveDiskDigestRefused, err.Error())
}

// moveDigests reads the source and target configurations immediately before a
// move POST and returns their digests, along with the volume the source's
// disk key names. Every write the caller makes before the move, the source
// slot delete included, lands before these reads, so the digests match the
// configurations PVE checks. A late task whose response was lost then refuses
// to move anything once either configuration has changed. A configuration
// without a digest leaves that digest empty, and PVE skips the check for an
// empty one. The volume is what a readback after an unanswered POST compares
// the source against.
func moveDigests(ctx context.Context, c Client, node string, srcVMID int, disk string, targetVMID int) (digest, targetDigest, sourceVolume string, err error) {
	source, err := c.QEMU().Config(ctx, node, srcVMID)
	if err != nil {
		return "", "", "", cpierrors.Wrap(WrapConfigReadError(err), fmt.Sprintf("move_disk: config read for source vm %d", srcVMID))
	}
	target, err := c.QEMU().Config(ctx, node, targetVMID)
	if err != nil {
		return "", "", "", cpierrors.Wrap(WrapConfigReadError(err), fmt.Sprintf("move_disk: config read for target vm %d", targetVMID))
	}
	digest, _ = ConfigString(source, "digest")
	targetDigest, _ = ConfigString(target, "digest")
	sourceVolume, _ = slotBareVolid(source, disk)
	return digest, targetDigest, sourceVolume, nil
}

// MoveSourceStillNames reports whether a move's source key still names volume
// the way it did before the move. The applied view has to name it, and the
// key can't have a pending delete or a pending replacement. A refusal proves
// only that its own request or task moved nothing, so a caller reads this
// back, together with MoveSlotEmpty, before it relies on the refusal.
func MoveSourceStillNames(source QemuViews, key, volume string) bool {
	if volume == "" || source.PendingDelete(key) {
		return false
	}
	if _, replaced := source.PendingReplacements()[key]; replaced {
		return false
	}
	held, _ := slotBareVolid(source.Applied(), key)
	return held == volume
}

// MoveSlotEmpty reports whether a move's receiving slot is empty in both
// views, so neither a drive nor a pending add holds it.
func MoveSlotEmpty(target QemuViews, slot string) bool {
	_, applied := slotBareVolid(target.Applied(), slot)
	_, current := slotBareVolid(target.Current(), slot)
	return !applied && !current
}

// moveReadback is what a readback after an unanswered move POST found.
type moveReadback int

const (
	// moveReadbackUncertain means the readback failed or found a state
	// that proves neither outcome, such as an earlier task mid-commit.
	moveReadbackUncertain moveReadback = iota
	// moveReadbackLanded means the move landed, as moveDiskLanded reports.
	moveReadbackLanded
	// moveReadbackInPlace means the source still names the original volume
	// on the same key and the receiving slot is empty.
	moveReadbackInPlace
)

// readBackUnansweredMove decides what a move came to when one of its POSTs
// went unanswered and a later attempt ended on a digest refusal or a failed
// digest read. PVE may have forked the unanswered POST's task, and that task
// can land after a later attempt read its digests. The later refusal then
// proves only that its own request or task moved nothing. volume is what the
// source's disk key named before the first POST.
//
// When the source still names volume on the same key and the receiving slot
// is empty, the configuration has changed since the first POST's digests were
// read, so any earlier task that hasn't committed yet will fail its own digest
// check. While a task holds both configuration locks, no API write lands, so
// a refusal can't come back while a task that passed its check is still
// waiting. That reasoning holds only for a refusal. A failed digest read
// proves no change, so moveDiskToVM treats the disk in place after one as an
// uncertain outcome.
func readBackUnansweredMove(ctx context.Context, c Client, node string, srcVMID int, disk string, targetVMID int, targetSlot, volume string) moveReadback {
	if moveDiskLanded(ctx, c, node, srcVMID, disk, targetVMID, targetSlot) {
		return moveReadbackLanded
	}
	source, err := ReadQemuViews(ctx, c, node, srcVMID)
	if err != nil {
		return moveReadbackUncertain
	}
	target, err := ReadQemuViews(ctx, c, node, targetVMID)
	if err != nil {
		return moveReadbackUncertain
	}
	if MoveSourceStillNames(source, disk, volume) && MoveSlotEmpty(target, targetSlot) {
		return moveReadbackInPlace
	}
	return moveReadbackUncertain
}

// uncertainUnansweredMove is what moveDiskToVM returns when one of its POSTs
// went unanswered and the readback can't settle what the move came to. The
// error is retriable, wraps nothing, and carries none of a refusal's text, so
// it doesn't wrap ErrMoveDiskDigestRefused, and windowWorkEnding reads the
// outcome as unknown. readFailed says the call ended on a failed digest read
// rather than on a refusal, and inPlace says the readback after that read
// error found the disk still in place, rather than proving neither outcome.
//
// After a failed digest read, the message ends with that read error's text,
// so the Director sees why the read failed. The error carries only the text
// and doesn't wrap the read error, because a wrapped API error or ended
// context would let pveAnswered, windowWorkEnding, and the callers' own checks
// classify the outcome as the read error rather than as unknown.
func uncertainUnansweredMove(logger *log.Logger, moveErr error, srcVMID int, disk string, targetVMID int, targetSlot, volume string, readFailed, inPlace bool) error {
	found := fmt.Sprintf("a readback showed neither the landed move nor %q still on vm %d %s with the receiving slot empty", volume, srcVMID, disk)
	if inPlace {
		found = fmt.Sprintf("a digest read failed, which can't rule out that request's task landing later, even though a readback found %q still on vm %d %s with the receiving slot empty", volume, srcVMID, disk)
	}
	cause := ""
	if readFailed {
		cause = fmt.Sprintf(". The digest read failed with %s", strings.TrimSpace(moveErr.Error()))
	}
	if logger != nil {
		logger.Warn("disk transfer: a move ended on a digest refusal or read error after an unanswered request, and its outcome is unknown",
			log.Int("src_vmid", srcVMID),
			log.String("disk", disk),
			log.Int("target_vmid", targetVMID),
			log.String("target_slot", targetSlot),
			log.String("readback", found),
			log.Err(moveErr),
		)
	}
	return cpierrors.Retriable(
		"move_disk %s of vm %d to vm %d slot %s has an unknown outcome, because an earlier request got no answer from PVE and %s; retry%s",
		disk, srcVMID, targetVMID, targetSlot, found, cause)
}

// movePin is what a caller proved before it asked for a move. It holds the
// source's and the target's configuration digests from the caller's own read
// and the volume the source's disk key named when the caller read it.
type movePin struct {
	SourceDigest string
	TargetDigest string
	Volume       string
}

// check compares the pin with the digests and the volume a move attempt read
// just before its POST, and names the first that changed.
func (p movePin) check(srcVMID, targetVMID int, disk, digest, targetDigest, sourceVolume string) error {
	switch {
	case digest != p.SourceDigest:
		return fmt.Errorf("source vm %d's digest is %q, not the %q its proof read", srcVMID, digest, p.SourceDigest)
	case targetDigest != p.TargetDigest:
		return fmt.Errorf("target vm %d's digest is %q, not the %q its proof read", targetVMID, targetDigest, p.TargetDigest)
	case sourceVolume != p.Volume:
		return fmt.Errorf("%s of source vm %d names %q, not the proven volume %q", disk, srcVMID, sourceVolume, p.Volume)
	}
	return nil
}

// moveDiskToVM issues one move_disk reassignment and awaits its task. disk is
// the source config key ("scsi3" or "unused0"), targetSlot the config key the
// volume lands on. Same-node only — PVE refuses cross-node reassignment.
func moveDiskToVM(ctx context.Context, c Client, logger *log.Logger, node string, srcVMID int, disk string, targetVMID int, targetSlot string) error {
	return moveDiskToVMPinned(ctx, c, logger, node, srcVMID, disk, targetVMID, targetSlot, nil)
}

// moveDiskToVMPinned is moveDiskToVM for a caller that proved which volume it
// moves. Each attempt's fresh digests and source volume must still match pin
// before its POST. A mismatch stops the move the way PVE's digest refusal
// does, so it's retriable unless an earlier POST went unanswered, and then the
// readback decides. A nil pin checks nothing.
//
//nolint:gocognit // Move retry-state machine: pendingUPID re-await, replay probe, and the POST+await path are one ordered decision tree; splitting it would scatter the ordering the double-apply protection depends on.
func moveDiskToVMPinned(
	ctx context.Context, c Client, logger *log.Logger,
	node string, srcVMID int, disk string, targetVMID int, targetSlot string, pin *movePin,
) error {
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
	//
	// unanswered records that one of this call's POSTs ended without an
	// answer from PVE, so its task may have forked and may still land. After
	// that, a later attempt's digest refusal or failed digest read goes
	// through readBackUnansweredMove before it surfaces. originalVolume is
	// what the source's disk key named at the first digest read.
	attempts := 0
	errFromAwait := false
	errFromDigestRead := false
	unanswered := false
	originalVolume := ""
	pendingUPID := ""
	logReplay := func() {
		if logger != nil {
			logger.Info("disk transfer: move replay found the reassignment already committed",
				log.Int("src_vmid", srcVMID),
				log.String("disk", disk),
				log.Int("target_vmid", targetVMID),
				log.String("target_slot", targetSlot),
			)
		}
	}
	moveErr := RetryOnTransientOrLock(ctx, logger, "disk_transfer_move", parkerWindowMaxAttempts, func() error {
		attempts++
		errFromAwait = false
		errFromDigestRead = false

		if pendingUPID != "" {
			upid := pendingUPID
			inner := AwaitTaskWithLogger(ctx, c, node, upid, logger)
			errFromAwait = true
			if inner == nil {
				pendingUPID = ""
				return nil
			}
			if IsMoveDigestRefusal(inner) {
				pendingUPID = ""
				return stopOnMoveDigestRefusal(inner)
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

		digest, targetDigest, sourceVolume, digestErr := moveDigests(ctx, c, node, srcVMID, disk, targetVMID)
		if digestErr != nil {
			errFromDigestRead = true
			return digestErr
		}
		if originalVolume == "" {
			originalVolume = sourceVolume
		}
		if pin != nil {
			if pinErr := pin.check(srcVMID, targetVMID, disk, digest, targetDigest, sourceVolume); pinErr != nil {
				return stopOnMoveDigestRefusal(pinErr)
			}
		}
		params := &sdknodes.CreateQemuMoveDiskParams{
			Disk:       disk,
			TargetVmid: &tv,
			TargetDisk: &ts,
		}
		if digest != "" {
			params.Digest = &digest
		}
		if targetDigest != "" {
			params.TargetDigest = &targetDigest
		}
		raw, inner := nodesSvc.CreateQemuMoveDisk(ctx, node, strconv.Itoa(srcVMID), params)
		if inner != nil && IsMoveDigestRefusal(inner) {
			return stopOnMoveDigestRefusal(inner)
		}
		if _, answered := pveAnswered(inner); inner != nil && !answered {
			unanswered = true
		}
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
			if inner != nil && IsMoveDigestRefusal(inner) {
				pendingUPID = ""
				return stopOnMoveDigestRefusal(inner)
			}
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
			logReplay()
			pendingUPID = ""
			return nil
		}
		return stopOnMoveSnapshotRefusal(inner)
	})
	if moveErr != nil && unanswered && (errFromDigestRead || IsMoveDigestRefusal(moveErr)) {
		// The refusal or the read error comes from a later attempt, and the
		// unanswered POST's task may have landed in between. Only the
		// readback says which.
		readback := readBackUnansweredMove(ctx, c, node, srcVMID, disk, targetVMID, targetSlot, originalVolume)
		if readback == moveReadbackLanded {
			logReplay()
			return nil
		}
		if readback == moveReadbackUncertain {
			return uncertainUnansweredMove(logger, moveErr, srcVMID, disk, targetVMID, targetSlot, originalVolume, errFromDigestRead, false)
		}
		if errFromDigestRead {
			// A refusal proves that a configuration changed since the
			// unanswered POST's digests were read, and a read error proves
			// nothing, so that POST's task could still land after this
			// readback.
			return uncertainUnansweredMove(logger, moveErr, srcVMID, disk, targetVMID, targetSlot, originalVolume, true, true)
		}
		// The source still names the volume and the slot is empty, so the
		// refusal stands as it is.
	}
	if moveErr != nil {
		if IsMoveDiskSnapshotRefusal(moveErr) {
			return fmt.Errorf("move %s of vm %d to vm %d slot %s: %w: %s",
				disk, srcVMID, targetVMID, targetSlot, ErrMoveDiskSnapshotRefused, moveErr.Error())
		}
		// A digest refusal that gets here means the volume is still where it
		// was, because no earlier POST went unanswered or the readback above
		// showed it in place. It is retriable, because a retry reads fresh
		// digests, and it must not read as an uncertain outcome.
		if IsMoveDigestRefusal(moveErr) {
			return cpierrors.WrapAs(moveErr, cpierrors.TypeRetriableCloud,
				fmt.Sprintf("move_disk %s of vm %d to vm %d slot %s refused because a configuration changed; retry", disk, srcVMID, targetVMID, targetSlot))
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

// transferLandedNameReadTimeout bounds the config read that finds a
// transferred disk's new name after the protection restore. The read runs on
// a detached context, so without this bound a PVE that never answered would
// hold the call open until the API client's own timeout.
const transferLandedNameReadTimeout = 10 * time.Second

// landedNameReadTimeoutKey carries a test's shorter bound for the landed-name
// read.
type landedNameReadTimeoutKey struct{}

// withTestLandedNameReadTimeout returns a context whose landed-name reads are
// bounded by d instead of transferLandedNameReadTimeout. A non-positive d
// leaves ctx as it is.
func withTestLandedNameReadTimeout(ctx context.Context, d time.Duration) context.Context {
	if d <= 0 {
		return ctx
	}
	return context.WithValue(ctx, landedNameReadTimeoutKey{}, d)
}

// landedNameReadTimeout is the bound of the landed-name read, which is
// transferLandedNameReadTimeout unless a test override rides ctx.
func landedNameReadTimeout(ctx context.Context) time.Duration {
	if d, ok := ctx.Value(landedNameReadTimeoutKey{}).(time.Duration); ok && d > 0 {
		return d
	}
	return transferLandedNameReadTimeout
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
	//
	// The read runs on a detached context with its own bound, not on the
	// window's. The restore before it runs on the time the window reserves
	// after its deadline, so a restore that uses that time up returns after the
	// window's deadline has passed, and a read on the window's context would
	// fail at once and drop the name of a disk that did land. A read changes
	// nothing on PVE, so running it past the window's deadline cannot
	// interleave with another window's writes, and the transfer runs no
	// demoted-slot sweep, so the read fits in the share of the reserve that
	// sweep would have used.
	readCtx, cancelRead := context.WithTimeout(context.WithoutCancel(ctx), landedNameReadTimeout(ctx))
	targetCfg, tErr := c.QEMU().Config(readCtx, parker.Node, targetVMID)
	cancelRead()
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

// RemoveParkerProvenanceEntry drops from a parker's sentinel the record of the
// disk with stableID that left the parker as bareVolid, or the legacy record
// keyed by bareVolid. Another disk's record survives even when PVE has
// already parked that disk under the same name (see removeParkerProvenance).
// Best-effort, exported for the handlers that finish an attach-side transfer:
// sentinel on the receiving VM first, then this — the giving side's record is
// removed last.
func RemoveParkerProvenanceEntry(ctx context.Context, c Client, logger *log.Logger, node string, parkerVMID int, bareVolid, stableID string, cfg ParkerConfig) {
	removeParkerProvenance(ctx, c, logger, node, parkerVMID, bareVolid, stableID, cfg)
}

// TransferDiskToParker moves an attached stable-ID disk from a workload VM
// onto a parker on the same node: intent record, slot delete (no sweep —
// after a reassignment the volume is named for the source VM, and PVE
// physically removes a swept unused volume its holder owns), unused-entry
// reassignment, serial re-apply, record finalize. pctx.StableID is required.
//
// The whole transfer runs under the disk's per-disk lock
// (withDiskTransferLock), taken before any parker lock, so a second transfer
// of the same disk waits for the first to finish. The caller resolved the
// disk's volume before that lock, so each parker window checks again where the
// disk is before it writes anything. A disk the parker already carries is
// returned as landed, and a disk its source no longer names comes back as
// errDiskLeftSource. On that answer the transfer re-resolves the disk by its
// serial. A disk that is now on a parker is a success with the name it landed
// under, once that parker's record of it is finished, and a disk still on its
// source under a new name is transferred again once under that name. A disk
// that another VM now holds comes back as a *DiskAttachedElsewhereError.
// Anything else is a retriable error, and the Director's retry runs the full
// resolve and resume.
//
// Every caller takes the per-disk lock, a journal-managed disk
// (pctx.AllocationID or pctx.AllocationNamespace set) included. The journal's
// lock serializes the operations that go through the journal, but delete_vm
// and create_vm's rollback transfer the same volume without it, so only the
// disk lock serializes them against a managed detach. A managed caller holds
// its journal lock around the disk lock, so the order is journal, then disk,
// then parker. When the re-resolve finds a disk under a new name, a client
// that implements DiskRelocationObserver is told before the transfer goes on,
// so the journal guard follows the volume the way it follows a move it
// observed.
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
	// A source slot whose pending value replaces its drive can't be detached
	// until PVE applies the change, so the transfer refuses it before it
	// creates a parker or writes an intent record, and a journal-managed
	// caller can see that nothing was written. A read that fails leaves the
	// decision to the source read inside the parker window, which reports it.
	if views, err := ReadQemuViews(ctx, c, node, srcVMID); err == nil {
		if slot, onBus := views.BusSlotNaming(bareVolid); onBus {
			if _, replaced := views.PendingReplacements()[slot]; replaced {
				return "", &DriveDeletePendingError{Reason: DriveDeletePendingReplaced, Node: node, VMID: srcVMID, Slot: slot}
			}
		}
	}

	var landed string
	lockErr := withDiskTransferLock(ctx, c, logger, pctx.StableID, "transfer_in", func(lctx context.Context) error {
		var innerErr error
		landed, innerErr = transferDiskToParkerOnce(lctx, c, logger, node, srcVMID, bareVolid, cfg, pctx)
		return innerErr
	})
	if lockErr != nil {
		return "", lockErr
	}
	return landed, nil
}

// diskTransferRestarts bounds how many times one transfer starts again under
// a fresh name after the re-resolve found the disk still on its source.
const diskTransferRestarts = 1

// transferDiskToParkerOnce is TransferDiskToParker's body, run under the
// per-disk lock when the disk takes one. It runs the parker loops and, when a
// window reports that the disk left its source, re-resolves the disk and acts
// on what it finds (see TransferDiskToParker).
func transferDiskToParkerOnce(
	ctx context.Context, c Client, logger *log.Logger,
	node string, srcVMID int, bareVolid string,
	cfg ParkerConfig, pctx ParkContext,
) (string, error) {
	volid := bareVolid
	for restart := 0; ; restart++ {
		landed, err := transferAcrossParkers(ctx, c, logger, node, srcVMID, volid, cfg, pctx)
		if err == nil {
			return landed, nil
		}
		if !errors.Is(err, errDiskLeftSource) && !errors.Is(err, errParkerRecordFinished) {
			return "", err
		}
		parked, fresh, resolveErr := relocateDiskThatLeftSource(ctx, c, logger, node, srcVMID, volid, cfg, pctx, err)
		if resolveErr != nil {
			return "", resolveErr
		}
		if parked != "" {
			return parked, nil
		}
		if restart >= diskTransferRestarts {
			return "", cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud, fmt.Sprintf(
				"TransferDiskToParker: disk %s left source vm %d again after the transfer restarted under %q; "+
					"the Director's retry re-resolves it", pctx.StableID, srcVMID, volid))
		}
		volid = fresh
	}
}

// transferAcrossParkers is the parker selection loop of one transfer attempt
// under volid. It mirrors parkDiskOnNode: first existing parker with a free
// slot, then fresh parkers, bounded.
func transferAcrossParkers(
	ctx context.Context, c Client, logger *log.Logger,
	node string, srcVMID int, bareVolid string,
	cfg ParkerConfig, pctx ParkContext,
) (string, error) {
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

// errDiskLeftSource is the answer of a parker window that found its disk gone
// from the source VM before it moved it: no slot and no unused entry of the
// source names the volume, or the volume left by some route other than this
// window's own slot delete, or the storage couldn't show the volume still
// there before a config-edit attach. The window attached nothing, and it
// left no intent record of its own behind unless its slot delete had run.
// TransferDiskToParker answers it by re-resolving the disk.
var errDiskLeftSource = errors.New("disk left its source VM before this transfer moved it")

// diskLeftSource wraps errDiskLeftSource with what the window saw.
func diskLeftSource(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errDiskLeftSource, fmt.Sprintf(format, args...))
}

// DiskRelocationObserver is implemented by a client decorator that tracks the
// volume name of the disk it guards, such as the journal-managed disk
// lifecycle. TransferDiskToParker calls it when its re-resolve finds the disk
// under a name other than the one the transfer started with, before it acts on
// the new name, so the decorator follows the volume as it does after a move it
// observed. An error refuses the relocation, and the transfer returns it.
type DiskRelocationObserver interface {
	ObserveDiskRelocated(ctx context.Context, stableID, from, to string) error
}

// DiskAttachedElsewhereError is the answer of a transfer whose disk left its
// source VM before the transfer moved it and is now attached to another,
// non-parker VM. Nothing was moved, and the source no longer holds the disk.
// It is retriable for a caller that needs the disk on a parker, while
// detach_disk treats it as a disk that is already detached from its VM, the
// way its own resolve does, and delete_vm treats it as a disk the doomed VM no
// longer holds.
type DiskAttachedElsewhereError struct {
	StableID string
	VMID     int
	Node     string
	Volid    string
}

func (e *DiskAttachedElsewhereError) Error() string {
	return fmt.Sprintf("disk %s left its source VM before this transfer moved it and is attached to vm %d on node %s as %s",
		e.StableID, e.VMID, e.Node, e.Volid)
}

// IsDiskAttachedElsewhere reports whether err carries a
// *DiskAttachedElsewhereError and returns it.
func IsDiskAttachedElsewhere(err error) (*DiskAttachedElsewhereError, bool) {
	var elsewhere *DiskAttachedElsewhereError
	if errors.As(err, &elsewhere) {
		return elsewhere, true
	}
	return nil, false
}

// relocateDiskThatLeftSource re-resolves a disk a parker window reported as
// gone from its source (cause), by its serial, across the cluster. It returns
// the landed name when the disk is on a parker, the fresh name when it is
// still on the source VM, a *DiskAttachedElsewhereError when another
// non-parker VM holds it, or a retriable error naming what it found.
//
// A disk on a parker counts as parked only after that parker's own window has
// read the serial on its slot and finished the parker's record of the disk
// (finishAlreadyParked), so a disk the re-resolve found there has the same
// record a transfer that moved it would have left.
//
// The resolve compares the disk's birth name, which the disk CID carries,
// with the names on every guest. A caller that has no CID, such as delete_vm
// on a VM whose notes lost it, can't name the birth volume, so the volid the
// transfer started with stands in for it. A refusal by birth name then says
// nothing about the disk, so it is turned into a plain retriable error, and
// the Director's retry resolves the disk from its CID.
func relocateDiskThatLeftSource(
	ctx context.Context, c Client, logger *log.Logger,
	node string, srcVMID int, volid string,
	cfg ParkerConfig, pctx ParkContext, cause error,
) (parked, fresh string, err error) {
	birth, birthKnown := volid, false
	if pctx.DiskCID != "" {
		if decoded, _, decodeErr := ParseEncodedDiskCID(pctx.DiskCID); decodeErr == nil && decoded != "" {
			birth, birthKnown = decoded, true
		}
	}
	identity, resolveErr := ResolveDiskIdentity(ctx, c, logger, birth, pctx.StableID, cfg)
	if resolveErr != nil {
		var held *DiskBirthNameHeldError
		if !birthKnown && errors.As(resolveErr, &held) {
			return "", "", cpierrors.Retriable(
				"TransferDiskToParker: disk %s left source vm %d before this transfer moved it, and without its CID the re-resolve "+
					"can't tell its birth volume from %q, which another guest names; the Director's retry resolves it from its CID (%v)",
				pctx.StableID, srcVMID, volid, cause)
		}
		return "", "", cpierrors.WrapAs(resolveErr, cpierrors.TypeRetriableCloud, fmt.Sprintf(
			"TransferDiskToParker: re-resolve disk %s after it left source vm %d (%v)", pctx.StableID, srcVMID, cause))
	}
	holder := identity.Holder
	switch {
	case identity.Intent == nil && holder.Found && holder.IsParker:
		landed, finishErr := finishOnParkerHolding(ctx, c, logger, node, srcVMID, volid, holder, cfg, pctx)
		if finishErr != nil {
			return "", "", finishErr
		}
		if logger != nil {
			logger.Info("transfer in: disk is already parked; another operation moved it, so this transfer moved nothing",
				log.Int("source_vmid", srcVMID),
				log.Int("parker_vmid", holder.VMID),
				log.String("slot", holder.Slot),
				log.String("stable_id", pctx.StableID),
				log.String("volid_before", volid),
				log.String("volid_after", landed),
				log.Bool("disk_lock_unserialized", diskTransferLockUnserialized(ctx)),
				log.Err(cause),
			)
		}
		return landed, "", nil
	case identity.Intent == nil && holder.Found && holder.VMID == srcVMID && holder.Node == node:
		if err := observeDiskRelocation(ctx, c, pctx.StableID, volid, identity.Volid); err != nil {
			return "", "", err
		}
		if logger != nil {
			logger.Warn("transfer in: disk is still on its source VM; transferring it again under the name it has now",
				log.Int("source_vmid", srcVMID),
				log.String("stable_id", pctx.StableID),
				log.String("volid_before", volid),
				log.String("volid_now", identity.Volid),
				log.Bool("disk_lock_unserialized", diskTransferLockUnserialized(ctx)),
				log.Err(cause),
			)
		}
		return "", identity.Volid, nil
	case identity.Intent == nil && holder.Found && holder.VMID != srcVMID:
		if logger != nil {
			logger.Warn("transfer in: disk left its source VM and is attached to another VM; this transfer moved nothing",
				log.Int("source_vmid", srcVMID),
				log.Int("holder_vmid", holder.VMID),
				log.String("holder_node", holder.Node),
				log.String("stable_id", pctx.StableID),
				log.String("volid_before", volid),
				log.String("volid_now", identity.Volid),
				log.Bool("disk_lock_unserialized", diskTransferLockUnserialized(ctx)),
				log.Err(cause),
			)
		}
		return "", "", cpierrors.WrapAs(
			&DiskAttachedElsewhereError{StableID: pctx.StableID, VMID: holder.VMID, Node: holder.Node, Volid: identity.Volid},
			cpierrors.TypeRetriableCloud,
			fmt.Sprintf("TransferDiskToParker: disk %s left source vm %d before this transfer moved it", pctx.StableID, srcVMID))
	}
	where := "no guest carries its serial"
	switch {
	case identity.Intent != nil:
		where = fmt.Sprintf("parker vmid %d keeps an unfinished transfer record for it", identity.Intent.ParkerVMID)
	case holder.Found:
		where = fmt.Sprintf("vm %d on node %s carries its serial", holder.VMID, holder.Node)
	}
	return "", "", cpierrors.WrapAs(cause, cpierrors.TypeRetriableCloud, fmt.Sprintf(
		"TransferDiskToParker: disk %s left source vm %d before this transfer moved it, and %s; "+
			"the Director's retry re-resolves and resumes it", pctx.StableID, srcVMID, where))
}

// finishOnParkerHolding finishes a transfer whose re-resolve found the disk's
// serial on parker holder, inside that parker's protection window, the same
// way a window that finds the serial on its own parker does
// (finishAlreadyParked). The window reads the parker again, because the
// resolve held no lock, and a parker that no longer carries the serial fails
// retriably. A parker on another node than the source's is finished too, but
// the parker pool sweep this request runs covers only the source's node, so
// that parker's pool placement is left to the next sweep on its own node,
// which is logged.
func finishOnParkerHolding(
	ctx context.Context, c Client, logger *log.Logger,
	srcNode string, srcVMID int, volid string, holder DiskHolder,
	cfg ParkerConfig, pctx ParkContext,
) (string, error) {
	if holder.Node != srcNode && logger != nil {
		logger.Warn("transfer in: disk is parked on another node than its source; this request's parker pool sweep doesn't cover that parker",
			log.Int("source_vmid", srcVMID),
			log.String("source_node", srcNode),
			log.Int("parker_vmid", holder.VMID),
			log.String("parker_node", holder.Node),
			log.String("stable_id", pctx.StableID),
		)
	}
	budgetCtx, cancel, budgetErr := diskTransferWindowContext(ctx, pctx.StableID, time.Now())
	if budgetErr != nil {
		return "", budgetErr
	}
	defer cancel()
	var landed string
	lockErr := withParkerProtectionLock(budgetCtx, c, logger, holder.VMID, "transfer_in", func(wctx context.Context) error {
		vmCfg, cfgErr := c.QEMU().Config(wctx, holder.Node, holder.VMID)
		if cfgErr != nil {
			return cpierrors.Wrap(WrapConfigReadError(cfgErr),
				fmt.Sprintf("transfer in: config read for parker vmid %d that the re-resolve found holding disk %s", holder.VMID, pctx.StableID))
		}
		slot, parkedVolid, carried := parkerSlotCarryingSerial(vmCfg, pctx.StableID)
		if !carried {
			return cpierrors.Retriable(
				"TransferDiskToParker: the re-resolve found disk %s on parker vmid %d, but its window no longer reads the serial there; "+
					"the Director's retry re-resolves it", pctx.StableID, holder.VMID)
		}
		var finishErr error
		landed, finishErr = finishAlreadyParked(wctx, c, logger, holder.Node, srcNode, holder.VMID, srcVMID, volid, slot, parkedVolid, vmCfg, cfg, pctx)
		return finishErr
	})
	if lockErr != nil {
		return "", lockErr
	}
	return landed, nil
}

// observeDiskRelocation tells a DiskRelocationObserver client that the disk
// with stableID now goes by to rather than from. Nothing is called when the
// name didn't change or the client doesn't observe.
func observeDiskRelocation(ctx context.Context, c Client, stableID, from, to string) error {
	if from == to {
		return nil
	}
	observer, ok := c.(DiskRelocationObserver)
	if !ok {
		return nil
	}
	if err := observer.ObserveDiskRelocated(ctx, stableID, from, to); err != nil {
		return cpierrors.Wrap(err, fmt.Sprintf("TransferDiskToParker: disk %s moved from %q to %q outside this transfer", stableID, from, to))
	}
	return nil
}

// transferIntoParker runs one detach-side transfer attempt against a known
// parker, inside its protection window. Under the per-disk lock the window
// opens only when enough of the disk claim is left for it to finish
// (diskTransferWindowContext).
func transferIntoParker(
	ctx context.Context, c Client, logger *log.Logger,
	node string, parkerVMID, srcVMID int, bareVolid string,
	cfg ParkerConfig, pctx ParkContext,
) (string, error) {
	budgetCtx, cancel, budgetErr := diskTransferWindowContext(ctx, pctx.StableID, time.Now())
	if budgetErr != nil {
		return "", budgetErr
	}
	defer cancel()
	var landed string
	lockErr := withParkerProtectionLock(budgetCtx, c, logger, parkerVMID, "transfer_in", func(wctx context.Context) error {
		var innerErr error
		landed, innerErr = transferIntoParkerLocked(wctx, c, logger, node, parkerVMID, srcVMID, bareVolid, cfg, pctx)
		return innerErr
	})
	return landed, lockErr
}

//nolint:gocognit // Sequential transfer protocol: slot choice, intent, detach, the branch on what PVE left, finalize. The step count is the protocol; splitting it would scatter the ordering the crash-window analysis depends on.
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
	// The caller resolved the disk before it held any lock, so another
	// transfer of the same disk may have finished since. A parker that
	// already carries the disk's serial is where it landed, and the transfer
	// is done without writing anything.
	if parkedSlot, parkedVolid, parked := parkerSlotCarryingSerial(parkerCfg, pctx.StableID); parked {
		return finishAlreadyParked(ctx, c, logger, node, node, parkerVMID, srcVMID, bareVolid, parkedSlot, parkedVolid, parkerCfg, cfg, pctx)
	}
	// The source is read before anything is written, so a transfer whose disk
	// has already left the source writes no intent at all. Both views count,
	// because a slot whose delete is pending and an unused entry a crashed
	// transfer left still hold the volume.
	srcViews, srcErr := ReadQemuViews(ctx, c, node, srcVMID)
	if srcErr != nil {
		return "", cpierrors.Wrap(WrapConfigReadError(srcErr),
			fmt.Sprintf("transfer in: config read for source vm %d", srcVMID))
	}
	if !srcViews.NamesVolume(bareVolid) {
		return "", diskLeftSource("source vm %d names %q nowhere, so this transfer wrote no intent record", srcVMID, bareVolid)
	}
	// A record of the disk this parker already keeps is replaced only while
	// its slot is empty or holds a drive whose serial names another disk. A
	// slot that holds any other volume is what a transfer that died after its
	// move landed and before its serial write leaves, and the resume finds
	// that volume only through the slot the record names. The intent write
	// checks the same on its own fresh read (landedParkerRecord).
	_, priorRecords, _ := parseParkerSentinel(DescriptionFromConfig(parkerCfg))
	prior, hadRecord := priorRecords[pctx.StableID]
	if held, occupied := parkerRecordSlotHolds(parkerCfg, prior, pctx.StableID); hadRecord && occupied {
		return "", diskLeftSource("parker vmid %d keeps an unfinished transfer record for disk %s whose slot %s holds %s, "+
			"so this transfer wrote no intent record", parkerVMID, pctx.StableID, prior.Slot, held)
	}
	// A slot another disk's unfinished record names is that disk's landing
	// spot, so it stays out of the choice even while it's empty.
	slot, slotErr := chooseParkSlotExcluding(qemu.ParseDisks(parkerCfg), slotSet(otherUnfinishedTransferSlots(pctx.StableID, parkerCfg)))
	if slotErr != nil {
		return "", slotErr // ErrNoSlots — caller tries the next parker
	}

	// 2. Intent record, strict: from the moment the source slot is deleted
	// until the serial lands on the parker, this record is the disk's only
	// identity carrier, so the transfer must not proceed without it. The write
	// refuses to replace a finished record of the disk, which would mean the
	// disk landed here after the read above, and a record whose slot holds a
	// volume.
	intent := buildParkerProvEntry(ctx, node, bareVolid, slot, cfg, pctx)
	if provErr := writeParkerTransferIntent(ctx, c, logger, node, parkerVMID, pctx.StableID, intent, cfg); provErr != nil {
		return "", cpierrors.Wrap(provErr,
			fmt.Sprintf("transfer in: write intent record on parker vmid %d (fail-closed: the record is the crash-window identity carrier)", parkerVMID))
	}
	// leftBeforeDelete answers a source that let the volume go after the read
	// above and before this transfer deleted anything. Nothing was moved, so
	// the intent record this transfer just wrote is taken back when it is the
	// only record of the disk here. A record it replaced held no landing of
	// this disk on its slot and is left as the intent now names it, because the transfer that wrote
	// that one may still need it. A take-back that doesn't land leaves an
	// intent that names a volume this transfer never moved, so the transfer
	// fails retriably instead of re-resolving the disk, and the Director's
	// retry finds that record and resumes from it.
	leftBeforeDelete := func(reason error) error {
		if hadRecord {
			return reason
		}
		stillHeld, removeErr := removeParkerProvenanceChecked(ctx, c, node, parkerVMID, bareVolid, pctx.StableID)
		if removeErr == nil && !stillHeld {
			return reason
		}
		if removeErr == nil {
			removeErr = fmt.Errorf("parker vmid %d still names %q, so the record stays", parkerVMID, bareVolid)
		}
		if logger != nil {
			logger.Error("transfer in: could not take back the intent record of a disk this transfer never moved",
				log.Int("source_vmid", srcVMID),
				log.Int("parker_vmid", parkerVMID),
				log.String("stable_id", pctx.StableID),
				log.String("volid", bareVolid),
				log.Bool("disk_lock_unserialized", diskTransferLockUnserialized(ctx)),
				log.Err(removeErr),
			)
		}
		return cpierrors.Retriable(
			"transfer in: source vm %d let %q go before this transfer deleted its slot, and the intent record on parker vmid %d "+
				"could not be taken back (%v); the Director's retry resumes from that record (%v)",
			srcVMID, bareVolid, parkerVMID, removeErr, reason)
	}

	// 3. Delete the source slot — a raw config delete, NOT the SDK's
	// DetachDisk: its unusedN sweep physically removes a volume its holder
	// owns, which after a reassignment this volume is.
	// The source is read again after the intent write, because it may have
	// changed since the read above. Both views: a slot whose delete is
	// pending still counts as attached, so the transfer sends the delete
	// again rather than skipping it, and the helper proves the source let go
	// before anything lands on the parker.
	srcViews, srcErr = ReadQemuViews(ctx, c, node, srcVMID)
	if srcErr != nil {
		return "", cpierrors.Wrap(WrapConfigReadError(srcErr),
			fmt.Sprintf("transfer in: config read for source vm %d", srcVMID))
	}
	// deleted records that this transfer's own slot delete took the volume
	// off the source's bus. It is half of the proof a config-edit attach
	// needs (see attachReleasedSourceVolume).
	deleted := false
	if actualSlot, onBus := srcViews.BusSlotNaming(bareVolid); onBus {
		// A delete PVE could only record as pending comes back as a
		// *DriveDeletePendingError, already reverted, and the wrap keeps it
		// visible to errors.As, so each caller chooses its class.
		if delErr := DeleteDriveSlot(ctx, c, logger, node, srcVMID, actualSlot, bareVolid, nil, parkerWindowMaxAttempts); delErr != nil {
			return "", cpierrors.Wrap(delErr,
				fmt.Sprintf("transfer in: detach %q (slot %s) from source vm %d", bareVolid, actualSlot, srcVMID))
		}
		deleted = true
		srcViews, srcErr = ReadQemuViews(ctx, c, node, srcVMID)
		if srcErr != nil {
			return "", cpierrors.Wrap(WrapConfigReadError(srcErr),
				fmt.Sprintf("transfer in: re-read source vm %d after detach", srcVMID))
		}
	}
	srcCfg := srcViews.Applied()

	// 4. Branch on what PVE left, never on a prediction of ownership. PVE
	// keeps an unused entry for a deleted slot only when the VM owns the
	// volume (vm_is_volid_owner in qemu-server's
	// vmconfig_register_unused_drive), so an unused entry that names the
	// volume means the move path. When neither view names the volume
	// anywhere, PVE let the reference go, and the parker takes the volume by
	// config edit. Any other key that still names it means something else
	// holds the volume, and the caller re-resolves.
	unusedKey := ""
	for key, volid := range FindUnusedDiskEntries(srcCfg) {
		if volid == bareVolid {
			unusedKey = key
			break
		}
	}
	var landed string
	if unusedKey == "" {
		if !deleted {
			// Nothing names the volume now, and this transfer deleted
			// nothing, so whatever let it go wasn't this transfer. A config
			// edit would attach a name that may no longer exist.
			return "", leftBeforeDelete(diskLeftSource(
				"source vm %d stopped naming %q after the intent record was written, and this transfer deleted no slot",
				srcVMID, bareVolid))
		}
		attached, attachErr := attachReleasedSourceVolume(ctx, c, logger, node, parkerVMID, srcVMID, bareVolid, pctx.StableID, srcViews)
		if attachErr != nil {
			return "", attachErr
		}
		slot, landed = attached, bareVolid
	} else {
		moved, moveErr := moveUnusedEntryToParker(ctx, c, logger, node, srcVMID, unusedKey, parkerVMID, slot, pctx.StableID)
		if moveErr != nil {
			return "", moveErr
		}
		landed = moved
	}

	// 7. Finalize the landed volume identity. Managed disks require durable
	// full provenance and receiving-side readback before source cleanup.
	// Legacy disks retain their serial-based best-effort behavior.
	// The record is rewritten in update mode, so option overrides that an
	// update_disk on this disk wrote into the intent meanwhile survive.
	final := intent
	final.Volid = landed
	final.Slot = slot
	if provErr := rewriteParkerProvenance(ctx, c, logger, node, parkerVMID, pctx.StableID, final, cfg); provErr != nil {
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
	reassertParkerProtection(ctx, c, logger, node, parkerVMID, parkerWindowLockCheck(ctx, parkerVMID))
	if logger != nil {
		logger.Info("transfer in: disk reassigned from VM to parker",
			log.Int("source_vmid", srcVMID),
			log.Int("parker_vmid", parkerVMID),
			log.String("slot", slot),
			log.String("volid_before", bareVolid),
			log.String("volid_after", landed),
			log.Bool("disk_lock_unserialized", diskTransferLockUnserialized(ctx)),
		)
	}
	return landed, nil
}

// moveUnusedEntryToParker is the move path, steps 5 and 6 of the transfer. It
// reassigns the unused entry PVE kept on the source to the parker slot, reads
// the landed volid, and re-applies the serial, which the unused-entry move
// drops along with every other drive option (live-spike result). The parker's
// protection flag doesn't block receiving a disk, because that's an add, not a
// remove.
func moveUnusedEntryToParker(
	ctx context.Context, c Client, logger *log.Logger,
	node string, srcVMID int, unusedKey string, parkerVMID int, slot, stableID string,
) (string, error) {
	if moveErr := moveDiskToVM(ctx, c, logger, node, srcVMID, unusedKey, parkerVMID, slot); moveErr != nil {
		return "", moveErr
	}
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
		_, err := c.QEMU().AttachDisk(ctx, node, parkerVMID, landed+",serial="+stableID, "scsi", &qemu.AttachOpts{DiskID: slot})
		return err
	})
	if serialErr != nil {
		return "", cpierrors.Wrap(WrapMutationError(serialErr),
			fmt.Sprintf("transfer in: re-apply serial on parker vmid %d slot %s", parkerVMID, slot))
	}
	return landed, nil
}

// attachReleasedSourceVolume is the transfer's path for a volume the source
// doesn't own. After the slot delete, PVE kept no unused entry, so no key of
// the source names the volume, and the parker takes it by the same config-edit
// attach the resume's released-source window uses, with the serial baked in.
// The volume keeps the name it was created with, so the parker doesn't own it
// either, and nothing PVE does to either VM frees it.
//
// A key that still names the volume in either view is something other than
// the slot this transfer deleted, so it attaches nothing and hands back a
// retriable error for the caller to re-resolve. A snapshot of the source that
// still names the volume defers the park the way PVE's move refusal does for
// an owned volume, because a rollback of that snapshot would put the volume
// back on the source as a second reference.
//
// The caller runs it only after its own slot delete took the volume off the
// source, which is half of the proof that the volume was released rather than
// taken. The other half is an unfiltered content listing of the volume's
// storage, read from the source's node, that still shows the volume under
// this name. A listing that doesn't show it attaches nothing and returns
// errDiskLeftSource, and so does an attach that PVE refuses because the
// volume does not exist. File storage never answers a missing volume with a
// 404, so only the listing can show it is there.
//
// A listing that can't be read proves nothing either way, and by then the
// volume is off the source and not yet on the parker, with the intent record
// as its only carrier. So the read is retried within the window's deadline
// (observeReleasedVolume), and when it still fails, the transfer returns a
// plain retriable error rather than errDiskLeftSource. The intent record
// stays as it is, and the Director's retry resumes the transfer from it.
func attachReleasedSourceVolume(
	ctx context.Context, c Client, logger *log.Logger,
	node string, parkerVMID, srcVMID int, bareVolid, stableID string, srcViews QemuViews,
) (string, error) {
	if keys := srcViews.SlotsNaming(bareVolid); len(keys) > 0 {
		return "", cpierrors.Retriable(
			"transfer in: source vm %d still names %q on %s after its slot delete; re-resolve and retry",
			srcVMID, bareVolid, strings.Join(keys, ", "))
	}
	if err := refuseSnapshotNamingVolume(ctx, c, node, srcVMID, bareVolid); err != nil {
		return "", err
	}
	present, probeErr := observeReleasedVolume(ctx, c, logger, node, bareVolid)
	if probeErr != nil {
		return "", cpierrors.WrapAs(probeErr, cpierrors.TypeRetriableCloud, fmt.Sprintf(
			"transfer in: the storage listing that has to show %q before parker vmid %d attaches it could not be read; "+
				"the volume is off source vm %d, and the intent record stays for the Director's retry to resume from",
			bareVolid, parkerVMID, srcVMID))
	}
	if !present {
		return "", diskLeftSource("the storage listing on node %s no longer shows %q after this transfer's slot delete",
			node, bareVolid)
	}
	slot, err := attachToParkerLocked(ctx, c, logger, node, parkerVMID, bareVolid, stableID)
	if err != nil && IsStorageVolumeMissing(err) {
		return "", diskLeftSource("PVE refused to attach %q because the volume does not exist: %v", bareVolid, err)
	}
	return slot, err
}

// observeReleasedVolume reads whether the storage listing on node shows
// bareVolid, for the config-edit attach of a released source volume. Any read
// that fails is tried again, up to parkerWindowMaxAttempts reads in all, on
// the transient backoff, and never past ctx's deadline. It returns the last
// read's error when none of them answered.
func observeReleasedVolume(ctx context.Context, c Client, logger *log.Logger, node, bareVolid string) (bool, error) {
	var lastErr error
	for attempt := range parkerWindowMaxAttempts {
		present, err := ObserveStorageVolumePresence(ctx, c, node, bareVolid)
		if err == nil {
			return present, nil
		}
		lastErr = err
		if attempt == parkerWindowMaxAttempts-1 {
			break
		}
		wait := TransientBackoffFor(ctx, attempt)
		if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= wait {
			break
		}
		if logger != nil {
			logger.Warn("transfer in: storage listing read failed; reading it again before the parker attaches the volume",
				log.String("node", node),
				log.String("volid", bareVolid),
				log.Int("attempt", attempt+1),
				log.Err(err),
			)
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false, errors.Join(lastErr, ctx.Err())
		case <-timer.C:
		}
	}
	return false, lastErr
}

// finishAlreadyParked ends a transfer whose parker already carries the disk's
// serial on parkedSlot as parkedVolid, because another operation landed it
// there after the caller resolved the disk. The parker is on node, and the
// source VM the transfer started from is on srcNode. The transfer writes no
// intent and touches no source. When the parker's record of the disk doesn't
// name the landed slot and volume, which is the state a transfer that died
// between its serial write and its finalize leaves, it finalizes the record
// the way the resume does, keeping the record's option overrides.
//
// A parker that keeps no record of the disk at all gets a new one, and its
// option overrides come from the caller's context, or else from the source
// VM's own record of them, so a landed disk doesn't lose an operator's
// update_disk. A source read that fails fails the transfer retriably rather
// than write a record that says the disk has no overrides.
func finishAlreadyParked(
	ctx context.Context, c Client, logger *log.Logger,
	node, srcNode string, parkerVMID, srcVMID int, bareVolid, parkedSlot, parkedVolid string,
	parkerCfg map[string]any, cfg ParkerConfig, pctx ParkContext,
) (string, error) {
	if err := observeDiskRelocation(ctx, c, pctx.StableID, bareVolid, parkedVolid); err != nil {
		return "", err
	}
	_, records, _ := parseParkerSentinel(DescriptionFromConfig(parkerCfg))
	record, recorded := records[pctx.StableID]
	if !recorded {
		if len(pctx.Opts) == 0 {
			opts, optsErr := GetVMDiskOptOverlay(ctx, c, srcNode, srcVMID, pctx.StableID, bareVolid)
			if optsErr != nil {
				return "", cpierrors.WrapAs(optsErr, cpierrors.TypeRetriableCloud, fmt.Sprintf(
					"transfer in: read the option overrides of disk %s from source vm %d before recording it on parker vmid %d",
					pctx.StableID, srcVMID, parkerVMID))
			}
			pctx.Opts = opts
		}
		record = buildParkerProvEntry(ctx, node, parkedVolid, parkedSlot, cfg, pctx)
	}
	// A transfer that carries no allocation, such as delete_vm's, can park a
	// journal-managed disk first and leave a record without the allocation.
	// The managed transfer that waited on the disk lock then stamps its own
	// allocation onto that record, because the serial on the parker's slot
	// already proves the record is this disk's. A record that names another
	// allocation is left alone, and the readback below refuses it.
	managed := pctx.AllocationID != "" || pctx.AllocationNamespace != ""
	stamp := managed && recorded && record.AllocationID == "" && record.AllocationNamespace == ""
	if stamp {
		record.AllocationID = pctx.AllocationID
		record.AllocationNamespace = pctx.AllocationNamespace
		if record.AllocationBacking == "" {
			record.AllocationBacking = pctx.AllocationBacking
		}
	}
	if !recorded || stamp || record.Volid != parkedVolid || record.Slot != parkedSlot {
		record.Volid = parkedVolid
		record.Slot = parkedSlot
		if provErr := rewriteParkerProvenance(ctx, c, logger, node, parkerVMID, pctx.StableID, record, cfg); provErr != nil {
			if managed {
				return "", cpierrors.Cloud("managed transfer provenance persistence requires reconciliation")
			}
			return "", cpierrors.Wrap(provErr,
				fmt.Sprintf("transfer in: finalize the record of disk %s already parked on parker vmid %d", pctx.StableID, parkerVMID))
		}
	}
	if managed {
		if err := VerifyAllocationParked(ctx, c, logger, parkedVolid, pctx.StableID, pctx.AllocationNamespace, pctx.AllocationID, cfg); err != nil {
			return "", cpierrors.Cloud("managed transfer provenance readback requires reconciliation")
		}
	}
	if logger != nil {
		logger.Info("transfer in: disk is already on this parker; another operation moved it, so this transfer moved nothing",
			log.Int("source_vmid", srcVMID),
			log.Int("parker_vmid", parkerVMID),
			log.String("node", node),
			log.String("slot", parkedSlot),
			log.String("stable_id", pctx.StableID),
			log.String("volid_before", bareVolid),
			log.String("volid_after", parkedVolid),
			log.Bool("disk_lock_unserialized", diskTransferLockUnserialized(ctx)),
		)
	}
	return parkedVolid, nil
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
//     been taken while the lock was down). A bus slot whose delete is pending
//     comes first. On a running source the resume leaves it alone and returns
//     DriveDeletePendingFound. On a stopped source it applies the delete when
//     pctx.ApplyFoundPendingDelete asks for it, and then goes on to whichever
//     window PVE left. A fallback slot is written into the record before the
//     move, so a landing whose serial write is lost sits on the slot the
//     record names.
//   - the recorded parker slot holds a parker-named volume with no serial,
//     which means the move landed but the serial write was lost, so claim it.
//     Claim it only when that volume is the one volume on the parker named for
//     the parker with no serial in either view, and only when no other disk's
//     unfinished record names the slot. In any other case the resume refuses.
//     A landing on the slot of another disk's unfinished transfer counts as
//     that disk's only when that disk's move is proved, which means its
//     source exists and names its recorded volume nowhere, and that volume is
//     either gone from storage or named by another guest. Every other answer
//     leaves that disk's move unknown, and a landing on an unknown disk's slot
//     refuses with audit required. A proved move with no landing on its slot
//     refuses the same way, and so does a claim that rests on setting another
//     landing aside by inference rather than by its serial. While another
//     transfer's move is unknown or unmoved with nothing on its slot, the claim
//     also refuses with audit required unless this disk's own move is proved,
//     because the landing may then be that disk's. A failed read refuses
//     retriably. While the parker holds a parker-named volume with no serial
//     that the resume can't set aside as another disk's landing, the move
//     window doesn't run, because PVE renamed a landed volume off the recorded
//     name and another disk may have taken it.
//
// Before it moves a volume or writes the serial onto one, the resume proves
// which volume is the disk, the same way the settlement of a move step does
// (disk_transfer_resume_proof.go).
// It refuses when another parker keeps a record of the disk's transfer, when
// any guest carries the serial, or when another parker names the volume. A
// source that reads as gone counts as gone only when the cluster can't find
// it either, and a released volume may have no guest naming it at all. In
// every window it refuses with audit required while another unfinished record
// on the parker names the same source VM and recorded volume as this one.
//
// With pctx.ClaimOnly set, the resume runs only the finalize and the claim,
// and it returns a ClaimOnlyRefusal before any other write. It refuses when
// the window would run without the parker's lock, and when the transfer
// record it reads again under the lock differs from the intent the caller
// read. It claims a slot only when pctx.ClaimLandings proves
// the slot's volume is this transfer's landing. Where it would move the
// volume, apply a pending delete, or attach by config edit, it refuses before
// that write, and when no window applies, its refusal wraps the permanent
// error below.
//
// Anything else is a state this code cannot safely converge; it returns a
// permanent error naming what it found so an operator can look.
//
// The resume takes the disk's per-disk transfer lock (withDiskTransferLock)
// before the parker's lock, the same order a fresh transfer takes them in, so
// a resume and a fresh transfer of one disk into different parkers can't
// overlap. The caller reads the intent before either lock, and the window
// reads the parker again under both. A caller that already holds the disk's
// lock runs inside it rather than waiting on its own claim. Under the lock
// the parker window opens only when enough of the disk claim is left for it
// to finish (diskTransferWindowContext).
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
	lockErr := withDiskTransferLock(ctx, c, logger, stableID, "transfer_resume", func(dctx context.Context) error {
		budgetCtx, cancel, budgetErr := diskTransferWindowContext(dctx, stableID, time.Now())
		if budgetErr != nil {
			return budgetErr
		}
		defer cancel()
		var windowErr error
		landed, windowErr = resumeDiskTransferWindow(budgetCtx, c, logger, intent, stableID, cfg, pctx)
		return windowErr
	})
	if lockErr != nil {
		return "", lockErr
	}
	return landed, nil
}

// resumeDiskTransferWindow runs the resume's windows inside the parker's
// protection lock. Its caller holds the disk's per-disk transfer lock.
//
//nolint:gocognit,gocyclo // Case analysis over the transfer's crash windows; each branch is one window and the ordering between them is load-bearing.
func resumeDiskTransferWindow(
	ctx context.Context, c Client, logger *log.Logger,
	intent DiskTransferIntent, stableID string,
	cfg ParkerConfig, pctx ParkContext,
) (string, error) {
	var landed string
	lockErr := withParkerProtectionLock(ctx, c, logger, intent.ParkerVMID, "transfer_resume", func(wctx context.Context) error {
		parkerCfg, cfgErr := c.QEMU().Config(wctx, intent.ParkerNode, intent.ParkerVMID)
		if cfgErr != nil {
			return cpierrors.Wrap(WrapConfigReadError(cfgErr),
				fmt.Sprintf("transfer resume: config read for parker vmid %d", intent.ParkerVMID))
		}
		if pctx.ClaimOnly {
			if claimErr := claimOnlyPreconditions(wctx, parkerCfg, intent, stableID); claimErr != nil {
				return claimErr
			}
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

		// A volume named for the parker with no serial is a move that landed
		// before its serial write, this transfer's or another's. While one is
		// there, the recorded volid on the source may already be another
		// disk's, because PVE renamed this disk's volume off that name when it
		// landed. So the move window doesn't run, and the landing window below
		// claims the landing or refuses.
		// A landing on the slot of another disk's unfinished transfer whose
		// move is proved is that disk's, and readResumeParker refuses any
		// other landing it can't attribute.
		parker, parkerErr := readResumeParker(wctx, c, intent, stableID, cfg)
		if parkerErr != nil {
			return parkerErr
		}

		// Window: the move never ran — the source VM still holds the volume
		// on an unusedN entry under its recorded (pre-move) name.
		srcVMID, convErr := strconv.Atoi(intent.SourceVMCID)
		if len(parker.landings) == 0 && convErr == nil && srcVMID > 0 && intent.Volid != "" {
			moved, handled, moveErr := resumeMoveWindow(wctx, c, logger, intent, stableID, srcVMID, parker, disks, cfg, pctx)
			if moveErr != nil || handled {
				landed = moved
				return moveErr
			}
		}

		// Window: the move landed on the recorded slot but the serial write
		// was lost. Claimable only when the landing is the parker's one volume
		// named for it with no stable-ID serial, it sits on the recorded slot,
		// no other disk's unfinished record names that slot, and nothing else
		// in the cluster holds the disk. Any other landing can't be told apart
		// from this transfer's, so the resume refuses.
		if len(parker.landings) > 0 {
			bare, proofErr := proveRecordedLanding(wctx, c, intent, stableID, parker)
			if proofErr != nil {
				return proofErr
			}
			if pctx.ClaimOnly && !landingRecorded(pctx.ClaimLandings, intent, bare) {
				return claimOnlyResumeRefusal(ClaimOnlyUnprovenLanding, intent, stableID,
					fmt.Sprintf("parker slot %s holds %s with no serial, and no move the disk's record observed landed it there",
						intent.Slot, bare))
			}
			if serialErr := applyResumedSerial(wctx, c, logger, intent, stableID, intent.Slot, bare); serialErr != nil {
				return serialErr
			}
			landed = bare
			return finalizeResumedTransfer(wctx, c, logger, intent, stableID, intent.Slot, bare, cfg, pctx)
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
		if convErr == nil && srcVMID > 0 && intent.Volid != "" {
			srcViews, srcErr := ReadQemuViews(wctx, c, intent.ParkerNode, srcVMID)
			sourceReleased := false
			switch {
			case srcErr != nil:
				// Window 2 already returned every non-gone read error, so a
				// second failing read here is either the same gone answer or a
				// fault that appeared mid-resume. Only the gone answer, with
				// the cluster agreeing, proves the source released the volume.
				gone, goneErr := resumeSourceGone(wctx, c, intent, stableID, srcVMID, srcErr)
				if goneErr != nil {
					return goneErr
				}
				sourceReleased = gone
			default:
				// Released only when neither view names the volume: a pending
				// delete leaves the volume plugged into the running guest.
				sourceReleased = !srcViews.NamesVolume(intent.Volid)
			}
			if sourceReleased {
				// A snapshot of a source that still exists can name the volume
				// and would put it back on the source if rolled back, so the
				// park waits for it, whichever release left the intent.
				if srcErr == nil {
					if snapErr := refuseSnapshotNamingVolume(wctx, c, intent.ParkerNode, srcVMID, intent.Volid); snapErr != nil {
						return snapErr
					}
				}
				if proofErr := proveReleasedVolume(wctx, c, intent, stableID); proofErr != nil {
					return proofErr
				}
				if pctx.ClaimOnly {
					return claimOnlyResumeRefusal(ClaimOnlyConfigEdit, intent, stableID,
						"the transfer needs a full resume to attach the released volume by config edit, which a claim-only resume doesn't do")
				}
				slot, attachErr := attachToParkerLocked(wctx, c, logger, intent.ParkerNode, intent.ParkerVMID, intent.Volid, stableID)
				if attachErr != nil {
					return attachErr
				}
				landed = intent.Volid
				return finalizeResumedTransfer(wctx, c, logger, intent, stableID, slot, intent.Volid, cfg, pctx)
			}
		}

		neither := cpierrors.Cloud(
			"transfer resume: disk %s has an intent record on parker vmid %d (node %s, slot %q, recorded volid %q, source %q) "+
				"but neither the parker nor the source VM holds a state this transfer can converge; inspect the parker's "+
				"slots and the source VM's unused entries by hand before retrying",
			stableID, intent.ParkerVMID, intent.ParkerNode, intent.Slot, intent.Volid, intent.SourceVMCID)
		if pctx.ClaimOnly {
			return &ClaimOnlyRefusal{Window: ClaimOnlyNoWindow, err: neither}
		}
		return neither
	})
	if lockErr != nil {
		return "", lockErr
	}
	return landed, nil
}

// resumeMoveWindow is the resume's window for a move that never ran, where the
// source VM still names the recorded volid. It proves the disk, takes the
// volume off any source bus slot that names it, moves the unused entry onto the
// parker, writes the serial, and finalizes the record. The move is pinned to
// the digests of the source and parker reads the proof used and to the recorded
// volid. A fallback slot goes into the record first, through recordResumeSlot,
// and the move is then pinned to the parker read after that write. It returns
// the landed volid and handled true once it has acted, and handled false when
// the source is gone or names the volume on no unused entry, so the later
// windows decide.
func resumeMoveWindow(
	ctx context.Context, c Client, logger *log.Logger,
	intent DiskTransferIntent, stableID string, srcVMID int, parker resumeParker, disks map[string]string,
	cfg ParkerConfig, pctx ParkContext,
) (string, bool, error) {
	srcViews, srcErr := ReadQemuViews(ctx, c, intent.ParkerNode, srcVMID)
	if srcErr != nil {
		gone, goneErr := resumeSourceGone(ctx, c, intent, stableID, srcVMID, srcErr)
		if goneErr != nil {
			return "", true, goneErr
		}
		if gone {
			return "", false, nil
		}
		return "", true, cpierrors.Wrap(WrapConfigReadError(srcErr),
			fmt.Sprintf("transfer resume: config read for source vm %d", srcVMID))
	}
	if !srcViews.NamesVolume(intent.Volid) {
		return "", false, nil
	}
	if proofErr := proveSourceNamesDisk(ctx, c, intent, stableID, srcVMID, srcViews); proofErr != nil {
		return "", true, proofErr
	}
	// The proof read srcViews, but the pending delete that resumeSourceOffBus
	// applies isn't pinned to that read. DeleteDriveSlot takes a digest, but on a
	// mismatch it reverts the pending delete it finds, and that delete isn't the
	// resume's to revert. So a change to the source between the proof and the
	// apply goes unchecked here. proveSourceKey checks the source again after the
	// apply, and the move is pinned to that later read.
	offBus, appliedSlot, offErr := resumeSourceOffBus(ctx, c, logger, intent, srcVMID, srcViews, pctx)
	if offErr != nil {
		return "", true, offErr
	}
	// Once resumeSourceOffBus applied a pending delete, every refusal after it
	// says so, because the source no longer holds that slot.
	afterDelete := func(err error, then string) error {
		if appliedSlot == "" {
			return err
		}
		return cpierrors.Wrap(&appliedPendingDeleteError{slot: appliedSlot, err: err}, fmt.Sprintf(
			"transfer resume: applied the pending delete of %s on source vm %d, and then %s did not go through",
			appliedSlot, srcVMID, then))
	}
	for key, volid := range FindUnusedDiskEntries(offBus.Applied()) {
		if volid != intent.Volid {
			continue
		}
		if proofErr := proveSourceKey(intent, stableID, srcVMID, offBus, key, appliedSlot); proofErr != nil {
			return "", true, proofErr
		}
		slot, slotErr := resumeTargetSlot(disks, intent.Slot, slotSet(parker.others))
		if slotErr != nil {
			return "", true, afterDelete(slotErr, fmt.Sprintf("the choice of a slot on parker vmid %d", intent.ParkerVMID))
		}
		if pctx.ClaimOnly {
			// The source VM's config names the volume by its old
			// name here, but the identity check runs a claim-only
			// resume only after storage said that name is gone, so
			// the entry may be dangling. The identity check
			// resolves the disk once more. If the old name is back,
			// the next call that changes the disk, such as
			// attach_disk or detach_disk, finishes the transfer, and
			// if it's still gone, the check refuses for audit,
			// permanently. We haven't proven that full resume safe
			// against a volume that reused the old name, and that
			// proof belongs to the call that runs it.
			return "", true, claimOnlyResumeRefusal(ClaimOnlyMove, intent, stableID,
				"the transfer needs a full resume to move the volume off source vm "+intent.SourceVMCID+
					", which a claim-only resume doesn't do")
		}
		pinned := parker.views
		if slot != intent.Slot {
			rewritten, rewriteErr := recordResumeSlot(ctx, c, logger, intent, stableID, slot, parker.views, cfg)
			if rewriteErr != nil {
				return "", true, afterDelete(rewriteErr, fmt.Sprintf(
					"the rewrite of disk %s's record on parker vmid %d", stableID, intent.ParkerVMID))
			}
			pinned = rewritten
		}
		sourceDigest, _ := ConfigString(offBus.Applied(), "digest")
		parkerDigest, _ := ConfigString(pinned.Applied(), "digest")
		pin := &movePin{SourceDigest: sourceDigest, TargetDigest: parkerDigest, Volume: intent.Volid}
		if moveErr := moveDiskToVMPinned(ctx, c, logger, intent.ParkerNode, srcVMID, key, intent.ParkerVMID, slot, pin); moveErr != nil {
			return "", true, afterDelete(moveErr, fmt.Sprintf("the move of %s", key))
		}
		afterCfg, afterErr := c.QEMU().Config(ctx, intent.ParkerNode, intent.ParkerVMID)
		if afterErr != nil {
			return "", true, cpierrors.Wrap(WrapConfigReadError(afterErr),
				fmt.Sprintf("transfer resume: config read for parker vmid %d after move", intent.ParkerVMID))
		}
		bare, ok := slotBareVolid(afterCfg, slot)
		if !ok {
			return "", true, cpierrors.Retriable(
				"transfer resume: move_disk reported success but parker vmid %d slot %s is empty; retry",
				intent.ParkerVMID, slot)
		}
		if serialErr := applyResumedSerial(ctx, c, logger, intent, stableID, slot, bare); serialErr != nil {
			return "", true, serialErr
		}
		return bare, true, finalizeResumedTransfer(ctx, c, logger, intent, stableID, slot, bare, cfg, pctx)
	}
	return "", false, nil
}

// recordResumeSlot points this disk's transfer record at slot, the fallback
// the move window chose because the recorded slot was taken, before the move
// runs. A move that lands and then loses its serial write leaves its landing
// on the slot the record names, so the next resume claims that landing rather
// than refusing it for an audit. The helper rewrites the record the resume
// read and changes only its slot, and it refuses retriably when that record no
// longer matches the intent. The write changes the parker's digest, so the
// helper reads the parker again and returns that read for the move to pin. It
// refuses retriably when the new read doesn't show the slot it wrote, or when
// anything besides this disk's record changed between the two reads, apart
// from the stale records the write collected, because the move would then be
// pinned to a parker the proof never saw.
func recordResumeSlot(
	ctx context.Context, c Client, logger *log.Logger,
	intent DiskTransferIntent, stableID, slot string, before QemuViews, cfg ParkerConfig,
) (QemuViews, error) {
	_, records, _ := parseParkerSentinel(DescriptionFromConfig(before.Applied()))
	entry, ok := records[stableID]
	if !ok || entry.Slot != intent.Slot || entry.Volid != intent.Volid || entry.SourceVMCID != intent.SourceVMCID {
		return QemuViews{}, cpierrors.Retriable(
			"transfer resume: parker vmid %d no longer keeps the record of disk %s that the resume read; retry",
			intent.ParkerVMID, stableID)
	}
	entry.Slot = slot
	collected, err := writeParkerProvenanceCollecting(ctx, c, logger, intent.ParkerNode, intent.ParkerVMID, stableID, entry, cfg, parkerProvUpdate)
	if err != nil {
		return QemuViews{}, cpierrors.Wrap(err, fmt.Sprintf(
			"transfer resume: point the record of disk %s on parker vmid %d at fallback slot %s",
			stableID, intent.ParkerVMID, slot))
	}
	after, err := ReadQemuViews(ctx, c, intent.ParkerNode, intent.ParkerVMID)
	if err != nil {
		return QemuViews{}, cpierrors.Wrap(WrapConfigReadError(err), fmt.Sprintf(
			"transfer resume: read parker vmid %d in both views after its record write", intent.ParkerVMID))
	}
	_, written, _ := parseParkerSentinel(DescriptionFromConfig(after.Applied()))
	if written[stableID].Slot != slot || !sameParkerApartFromRecords(before, after, stableID, collected) {
		return QemuViews{}, cpierrors.Retriable(
			"transfer resume: parker vmid %d changed while the resume pointed disk %s's record at slot %s; retry",
			intent.ParkerVMID, stableID, slot)
	}
	return after, nil
}

// sameParkerApartFromRecords reports whether two reads of a parker agree. They
// agree when both hold the same keys with the same values in both views, and
// the same transfer records in both views. Four things don't count as a
// difference. They are the description's own text, the digest that changes
// with it, the record of the disk that own names, which the write changed, and
// the records of the disks that collected names, which the write removed. Any
// other difference means someone else wrote to the parker between the reads.
func sameParkerApartFromRecords(before, after QemuViews, own string, collected []string) bool {
	for _, pair := range [][2]QemuViews{{before, after}, {after, before}} {
		for key, entry := range pair[0].entries {
			if key == "description" || key == "digest" {
				continue
			}
			if other, ok := pair[1].entries[key]; !ok || other != entry {
				return false
			}
		}
	}
	setAside := func(views map[string]any) map[string]parkerProvEntry {
		_, records, _ := parseParkerSentinel(DescriptionFromConfig(views))
		delete(records, own)
		for _, key := range collected {
			delete(records, key)
		}
		return records
	}
	return reflect.DeepEqual(setAside(before.Applied()), setAside(after.Applied())) &&
		reflect.DeepEqual(setAside(before.Current()), setAside(after.Current()))
}

// resumeSourceOffBus is the start of the resume's move window. It returns the
// source's views once no bus slot names the recorded volid, which is what the
// move window needs before it reassigns an unused entry, and the bus slot
// whose pending delete it applied to get there, if any.
//
// A bus slot whose pending value names a different volume than its current
// drive can't be detached until PVE applies the change, so the resume returns
// DriveDeletePendingReplaced for the caller to class, the same reason the slot
// delete and the transfer give. A bus slot that still names the volid without
// a pending delete means the detach never happened, so this isn't a resume at
// all. The identity scan should have found it, and a race between the scan
// and this read is the only path here. A bus slot whose delete is pending was
// left by a crash, an earlier release, or an operator, and nothing may land on
// the parker until the VM lets go of the disk. The resume can't tell whose
// delete it is. On a running source the guest still has the disk and the VM's
// next clean stop applies the delete, so the resume leaves it alone and hands
// back the typed error for the caller to class. On a stopped source nothing
// applies it until the VM starts, so a caller whose request moves the disk off
// the source has the resume apply it now, through applyFoundPendingDelete.
func resumeSourceOffBus(
	ctx context.Context, c Client, logger *log.Logger,
	intent DiskTransferIntent, srcVMID int, srcViews QemuViews, pctx ParkContext,
) (QemuViews, string, error) {
	slot, onBus := srcViews.BusSlotNaming(intent.Volid)
	if !onBus {
		return srcViews, "", nil
	}
	if _, replaced := srcViews.PendingReplacements()[slot]; replaced {
		return QemuViews{}, "", &DriveDeletePendingError{Reason: DriveDeletePendingReplaced, Node: intent.ParkerNode, VMID: srcVMID, Slot: slot}
	}
	if !srcViews.PendingDelete(slot) {
		return QemuViews{}, "", cpierrors.Retriable(
			"transfer resume: volume %q is still attached to source vm %d; re-resolve and retry",
			intent.Volid, srcVMID)
	}
	if pctx.ClaimOnly {
		return QemuViews{}, "", claimOnlyResumeRefusal(ClaimOnlyPendingDelete, intent, pctx.StableID,
			"the transfer needs a full resume to apply the pending delete of slot "+slot+" on source vm "+intent.SourceVMCID+
				", which a claim-only resume doesn't do")
	}
	applied, err := applyFoundPendingDelete(ctx, c, logger, intent.ParkerNode, srcVMID, slot, intent.Volid, pctx)
	if err != nil {
		return QemuViews{}, "", err
	}
	if !applied {
		return QemuViews{}, "", &DriveDeletePendingError{Reason: DriveDeletePendingFound, Node: intent.ParkerNode, VMID: srcVMID, Slot: slot}
	}
	after, err := ReadQemuViews(ctx, c, intent.ParkerNode, srcVMID)
	if err != nil {
		return QemuViews{}, "", cpierrors.Wrap(WrapConfigReadError(err),
			fmt.Sprintf("transfer resume: re-read source vm %d after applying its pending delete", srcVMID))
	}
	if _, stillOnBus := after.BusSlotNaming(intent.Volid); stillOnBus {
		return QemuViews{}, "", cpierrors.Retriable(
			"transfer resume: volume %q is still on a slot of source vm %d after its pending delete was applied; re-resolve and retry",
			intent.Volid, srcVMID)
	}
	return after, slot, nil
}

// ClaimOnlyWindow names the point where a claim-only resume stopped.
type ClaimOnlyWindow string

const (
	// ClaimOnlyMove is the move window, where the source VM's unused entry
	// still names the volume by its old name.
	ClaimOnlyMove ClaimOnlyWindow = "move"
	// ClaimOnlyPendingDelete is a source slot whose delete is still pending.
	ClaimOnlyPendingDelete ClaimOnlyWindow = "pending_delete"
	// ClaimOnlyConfigEdit is a source that released the volume, which only a
	// config-edit attach parks.
	ClaimOnlyConfigEdit ClaimOnlyWindow = "config_edit"
	// ClaimOnlyUnprovenLanding is a slot with no serial whose volume no
	// recorded move of this transfer landed.
	ClaimOnlyUnprovenLanding ClaimOnlyWindow = "unproven_landing"
	// ClaimOnlyIntentMoved is a transfer record that changed between the
	// caller's read and the resume's read under the parker's lock.
	ClaimOnlyIntentMoved ClaimOnlyWindow = "intent_moved"
	// ClaimOnlyUnserialized is a window that would run without the parker's
	// lock because PVE refused the lock's create or the client has no pool
	// service that could grant it.
	ClaimOnlyUnserialized ClaimOnlyWindow = "unserialized"
	// ClaimOnlyNoWindow is a state no window of the resume converges.
	ClaimOnlyNoWindow ClaimOnlyWindow = "no_window"
)

// ClaimOnlyRefusal is what a claim-only resume returns where it stops without
// a write. Window says where it stopped, and Reason says why in a phrase a
// caller can put after "because". The wrapped error carries the CPI class.
// A refusal at the move, at a changed record, or at an unserialized lock is
// retriable, because a retry resolves the disk again. Every other refusal is
// permanent. The identity check, which is the only caller, resolves the disk
// once more after a refusal at the move, and it refuses for audit, permanently,
// when the old name is still gone. It also refuses for audit, permanently, at
// the unserialized window, because no retry can grant the privilege the lock
// needs.
type ClaimOnlyRefusal struct {
	Window ClaimOnlyWindow
	Reason string
	err    error
}

func (e *ClaimOnlyRefusal) Error() string { return e.err.Error() }

// Unwrap returns the classed CPI error the refusal carries.
func (e *ClaimOnlyRefusal) Unwrap() error { return e.err }

// AsClaimOnlyRefusal returns the ClaimOnlyRefusal in err's chain, if any.
func AsClaimOnlyRefusal(err error) (*ClaimOnlyRefusal, bool) {
	var refusal *ClaimOnlyRefusal
	if errors.As(err, &refusal) {
		return refusal, true
	}
	return nil, false
}

// claimOnlyResumeRefusal builds the refusal a claim-only resume returns at
// window, with reason saying why it stopped there.
func claimOnlyResumeRefusal(window ClaimOnlyWindow, intent DiskTransferIntent, stableID, reason string) error {
	text := fmt.Sprintf("transfer resume: claim-only resume of disk %s on parker vmid %d stopped, because %s",
		stableID, intent.ParkerVMID, reason)
	switch window {
	case ClaimOnlyMove, ClaimOnlyIntentMoved, ClaimOnlyUnserialized:
		return &ClaimOnlyRefusal{Window: window, Reason: reason, err: cpierrors.Retriable("%s; retry", text)}
	default:
		return &ClaimOnlyRefusal{Window: window, Reason: reason, err: cpierrors.Cloud("%s", text)}
	}
}

// claimOnlyPreconditions refuses a claim-only resume before any window runs.
// The claim is safe to repeat only while the parker's lock keeps a second
// resume out, so a window that runs without the lock refuses. The caller read
// the transfer record before the lock, and another call may have finished or
// changed the transfer while this one waited, so the record read again here
// must still name the caller's slot, volume, and source VM.
func claimOnlyPreconditions(ctx context.Context, parkerCfg map[string]any, intent DiskTransferIntent, stableID string) error {
	if parkerLockUnserialized(ctx) {
		return claimOnlyResumeRefusal(ClaimOnlyUnserialized, intent, stableID,
			"PVE refused the parker's lock or no pool service could take it, and a claim-only resume runs only under that lock")
	}
	_, entries, _ := parseParkerSentinel(DescriptionFromConfig(parkerCfg))
	entry, found := entries[stableID]
	if found && entry.Slot == intent.Slot && entry.Volid == intent.Volid && entry.SourceVMCID == intent.SourceVMCID {
		return nil
	}
	return claimOnlyResumeRefusal(ClaimOnlyIntentMoved, intent, stableID,
		"the transfer moved underneath this call, and its record on the parker no longer matches the one the call read")
}

// landingRecorded reports whether one of landings moved the transfer's
// recorded volume off its source VM and landed it as bare.
func landingRecorded(landings []RecordedLanding, intent DiskTransferIntent, bare string) bool {
	source, err := strconv.Atoi(intent.SourceVMCID)
	if err != nil || source <= 0 || intent.Volid == "" {
		return false
	}
	for _, landing := range landings {
		if landing.SourceVMID == source && landing.From == intent.Volid && landing.To == bare {
			return true
		}
	}
	return false
}

// applyFoundPendingDelete applies a pending delete of slot that the resume
// found on its source VM and didn't send, and reports whether it did. It does
// so only when the caller asked for it and the source reads stopped. A stopped
// VM applies every pending change with the next config write, which is what a
// start would do anyway (API2/Qemu.pm:2595 and QemuServer.pm:5549 at
// qemu-server a7b4240b), and the intent record already says the disk is
// leaving this source. So the delete goes through DeleteDriveSlot, which
// applies at once on a stopped VM.
//
// The source can start between the status read and the delete. Then
// DeleteDriveSlot meets a running VM, and when the delete stays pending, it
// reverts it and returns the busy or hotplug reason. The revert leaves the disk
// on the slot where the running guest has it, and the caller fails retriably
// or permanently as it does for its own delete, so that outcome is accepted.
func applyFoundPendingDelete(ctx context.Context, c Client, logger *log.Logger, node string, vmid int, slot, volid string, pctx ParkContext) (bool, error) {
	if !pctx.ApplyFoundPendingDelete {
		return false, nil
	}
	status, err := c.QEMU().Status(ctx, node, vmid)
	if err != nil {
		return false, cpierrors.Wrap(WrapError(err), fmt.Sprintf("transfer resume: status of source vm %d", vmid))
	}
	if state, _ := ConfigString(status, "status"); state != "stopped" {
		return false, nil
	}
	if logger != nil {
		logger.Warn("transfer resume: applying a pending delete found on a stopped source",
			log.Int("source_vmid", vmid),
			log.String("slot", slot),
			log.String("volid", volid),
		)
	}
	if err := DeleteDriveSlot(ctx, c, logger, node, vmid, slot, volid, nil, parkerWindowMaxAttempts); err != nil {
		return false, cpierrors.Wrap(err,
			fmt.Sprintf("transfer resume: apply the pending delete of slot %s on stopped source vm %d", slot, vmid))
	}
	return true, nil
}

// resumeTargetSlot prefers the intent's recorded slot when it is still free
// and falls back to a fresh choice — a concurrent park may have taken the
// recorded one while the crashed transfer's lock was expired. exclude holds
// the slots other disks' unfinished records name, and neither the recorded
// slot nor the fallback may be one of them.
func resumeTargetSlot(disks map[string]string, recorded string, exclude map[string]bool) (string, error) {
	if recorded != "" && !exclude[recorded] {
		if _, occupied := disks[recorded]; !occupied {
			return recorded, nil
		}
	}
	return chooseParkSlotExcluding(disks, exclude)
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
// the slot is the authoritative carrier by the time this runs. The rewrite is
// in update mode, so it keeps the option overrides the record carries at the
// write rather than the ones the resume read when it started.
func finalizeResumedTransfer(
	ctx context.Context, c Client, logger *log.Logger,
	intent DiskTransferIntent, stableID, slot, landed string,
	cfg ParkerConfig, pctx ParkContext,
) error {
	entry := buildParkerProvEntry(ctx, intent.ParkerNode, landed, slot, cfg, pctx)
	if provErr := rewriteParkerProvenance(ctx, c, logger, intent.ParkerNode, intent.ParkerVMID, stableID, entry, cfg); provErr != nil {
		if pctx.AllocationID != "" || pctx.AllocationNamespace != "" {
			return cpierrors.Cloud("managed transfer provenance persistence requires reconciliation")
		}
		if logger != nil {
			logger.Warn("transfer resume: could not finalize legacy provenance", log.Err(provErr))
		}
	}
	reassertParkerProtection(ctx, c, logger, intent.ParkerNode, intent.ParkerVMID, parkerWindowLockCheck(ctx, intent.ParkerVMID))
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
	node string, parkerVMID int, bareVolid, stableID string, cfg ParkerConfig,
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
			removeParkerProvenance(ctx, c, logger, node, parkerVMID, bareVolid, stableID, cfg)
		}
		return lockErr
	}
	removeParkerProvenance(ctx, c, logger, node, parkerVMID, bareVolid, stableID, cfg)
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
	if _, answered := pveAnswered(err); answered || IsTaskExitVerdict(err) || IsMoveDiskSnapshotRefusal(err) || IsMoveDigestRefusal(err) {
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

// parkerRestoreTimeoutKey carries a test's shorter protection restore
// deadline on the request context.
type parkerRestoreTimeoutKey struct{}

// WithParkerProtectionRestoreTimeoutForTest returns a context whose protection
// restores use deadline d instead of parkerProtectionRestoreReserveNow. It rides
// the context rather than a package variable so tests that shorten it can run
// in parallel. A non-positive d leaves ctx as it is. Production code never
// calls it; it mirrors WithTestBackoff.
func WithParkerProtectionRestoreTimeoutForTest(ctx context.Context, d time.Duration) context.Context {
	if d <= 0 {
		return ctx
	}
	return context.WithValue(ctx, parkerRestoreTimeoutKey{}, d)
}

// parkerRestoreTimeout is the deadline of one protection restore. It is the
// reserve parkerWindowReserveNow sets aside for the restore, so a window body
// that runs to its deadline, then a restore that runs its whole deadline, the
// demoted-slot sweep, and the lock release, still ends before the lock's TTL,
// where a waiter may steal the lock and open its own window on this parker.
// The reserve follows the retry curves configured now, so a restore whose
// retries need longer on a lengthened curve gets that time. A test override
// carried on ctx replaces it.
func parkerRestoreTimeout(ctx context.Context) time.Duration {
	if d, ok := ctx.Value(parkerRestoreTimeoutKey{}).(time.Duration); ok && d > 0 {
		return d
	}
	return parkerProtectionRestoreReserveNow()
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
// 5xx status, except 502 through 504, which a proxy in front of PVE sends,
// and 596 through 599, which pveproxy relays from its own HTTP client when a
// call it forwards to pvedaemon or another node fails. A 597, for one, means
// the body broke off after the backend had answered, so the backend may
// already have forked a task. 595 stays an answer, because pveproxy sends it
// when it could not connect, before anything reached the backend. It is false
// for a transport fault, a timeout, and a cancelled or expired context, where
// the request may have been applied.
func pveAnswered(err error) (int, bool) {
	if err == nil || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return 0, false
	}
	code, ok := apiHTTPCode(err)
	if !ok {
		return 0, false
	}
	switch code {
	case 502, 503, 504, 596, 597, 598, 599:
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
//
// A deadline that ends the retry loop while it waits out the backoff after
// PVE refused the write didn't cut a write off, because no write went out
// after that refusal. The restore judges the last attempt, as it does when
// the loop runs out of attempts, so it returns the refusal itself and reports no cut-off, the same as a refusal on the
// loop's last attempt. A deadline that ends the backoff after an attempt that
// got no answer still reports a cut-off, because that attempt may have
// applied.
func putParkerProtectionBack(ctx context.Context, c Client, logger *log.Logger, node string, parkerVMID int) (bool, error) {
	restoreCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), parkerRestoreTimeout(ctx))
	defer cancel()
	protErr := setParkerProtection(restoreCtx, c, logger, node, parkerVMID, true)
	if last := lastAttemptBeforeContextEnded(protErr); ProtectionWriteRefused(last) {
		return false, last
	}
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

// parkerWindowLockCheck is the lock clause for a restore message about a
// parker whose protection window ran under ctx (ParkerLockCheck).
func parkerWindowLockCheck(ctx context.Context, parkerVMID int) string {
	return ParkerLockCheck(parkerVMID, parkerLockUnserialized(ctx))
}

// moverLockCheck is the lock clause for a restore message about a migration
// mover. The migration never takes the mover's lock, so the lock's pool
// proves nothing about it, and an attach_disk that a rerun starts clears the
// mover's protection without that lock, as does a delete_disk of the disk.
// What can have the protection off on purpose is a running attach_disk or
// delete_disk, so the clause has us look for one.
const moverLockCheck = "only when bosh tasks lists no running attach_disk or delete_disk task for the disk, "

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
func restoreParkerProtectionLogged(ctx context.Context, c Client, logger *log.Logger, op, node string, parkerVMID int, check string) {
	timedOut, protErr := putParkerProtectionBack(ctx, c, logger, node, parkerVMID)
	if protErr == nil || logger == nil {
		return
	}
	timeout := parkerRestoreTimeout(ctx)
	if ending := protectionRestoreEnding(protErr, timedOut, timeout); ending != "" {
		logger.Warn(fmt.Sprintf("%s: protection restore on parker %s; check the parker with qm config %d, and if protection is off, %srun qm set %d --protection 1",
			op, ending, parkerVMID, check, parkerVMID),
			log.Int("parker_vmid", parkerVMID),
			log.String("node", node),
			log.String("timeout", timeout.String()),
			log.Err(protErr),
		)
		return
	}
	logger.Warn(fmt.Sprintf("%s: could not restore protection on parker %d; %srun qm set %d --protection 1",
		op, parkerVMID, check, parkerVMID),
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
//     stands. That holds too when the deadline ends the retries during the
//     backoff after PVE refused, because no write went out after the refusal
//     (see putParkerProtectionBack).
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
				"check the parker with qm config %d, and if protection is off, %srun qm set %d --protection 1",
			op, parkerVMID, ending, work, parkerVMID, ParkerLockCheck(parkerVMID, parkerLockUnserialized(ctx)), parkerVMID))}
	}
	if logger != nil {
		logger.Warn(fmt.Sprintf("%s: could not restore protection on parker %d; %srun qm set %d --protection 1",
			op, parkerVMID, ParkerLockCheck(parkerVMID, parkerLockUnserialized(ctx)), parkerVMID),
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

// appliedPendingDeleteError carries the source slot whose pending delete a
// resume applied before a later step of the resume failed. Its text and class
// are the failure's own, so it changes nothing a caller reads except through
// ResumeAppliedPendingDelete.
type appliedPendingDeleteError struct {
	slot string
	err  error
}

func (e *appliedPendingDeleteError) Error() string { return e.err.Error() }

func (e *appliedPendingDeleteError) Unwrap() error { return e.err }

// ResumeAppliedPendingDelete reports whether a transfer resume applied the
// pending delete of a source slot before err stopped it, and returns that
// slot. Once the delete is applied, the source holds the volume on an unused
// entry rather than on the slot.
func ResumeAppliedPendingDelete(err error) (string, bool) {
	var applied *appliedPendingDeleteError
	if errors.As(err, &applied) {
		return applied.slot, true
	}
	return "", false
}
