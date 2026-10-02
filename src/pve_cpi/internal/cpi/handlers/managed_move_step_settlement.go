package handlers

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// A managed disk's move goes out as one Nodes.CreateQemuMoveDisk POST, and the
// lifecycle guard records the step as planned before it sends it. When PVE's
// answer never arrives, the guard can't know whether PVE forked a task, so it
// poisons the record and leaves the step planned with no UPID. A request PVE
// refused on a check before the fork leaves the same step. Every readmission
// then refuses on that step until something settles it.
//
// The settler below settles such a step as a move that never started, and it
// is deliberately narrow. The step is a planned lifecycle
// Nodes.CreateQemuMoveDisk step of the active attempt of a disk record, with
// no UPID, no recorded parameters, no external target, and a target that
// names a node, the source VMID, and the volume. The record must name the
// disk's serial. The step settles only when every one of these holds, read in
// this order. Items 3 and 4 are proofs 1 and 2, and item 5 holds proofs 3 and
// 4, which read from one scan.
//
//  1. The quiet period has passed. At least moveSettlementQuietPeriod has gone
//     by since the record's last save, which was no earlier than the save that
//     planned the step, immediately before the POST.
//  2. No move task for the source is still active. The settler lists the
//     active qmmove tasks on the step's node and on the node the source VM
//     lives on now, and any task whose ID names the source refuses. A listing
//     without Sys.Audit on the node refuses too, because PVE hides another
//     user's tasks from it.
//  3. The source still names the volume, and the volume is still this disk.
//     Exactly one disk key of the source names it in either view, and that
//     key's applied value names it with no pending delete and no pending
//     replacement. PVE hands a freed name to the next volume it renames onto
//     the same VM, so the name alone doesn't identify the disk. A bus slot
//     must carry the disk's serial in every view that has it, and no other
//     serial. An unused entry carries no serial, so the receiving side is read
//     instead, the way a refused move's readback pairs the source with the
//     receiving slot. Exactly one parker may keep a record of the disk, so
//     the record and the landing check read the same parker. That parker
//     must keep the record of this disk's transfer from the source under the
//     volume's name, and it may hold, on no disk key in either view, a volume
//     named for it with no serial, which is what a move that landed after its
//     answer was lost leaves behind. Every key is read, because a
//     resume can land on a fallback slot, and another disk's landing refuses
//     too, because nothing on the parker tells the two apart.
//  4. The volume exists under that name on the step's node, by the content
//     listing and then by the exact content read.
//  5. No guest holds the disk's serial, and no parker names the volume, on any
//     disk key in either view. The one holder tolerated is the source's own
//     key from item 3, when that key is a bus slot, because a move whose
//     source is a parker, or a source the documented recovery reattached,
//     still holds the disk there.
//
// The task listing runs before the proofs on purpose, so the proofs read PVE
// after the listing did, and a move that lands after the listing fails them.
// With the proofs first, a move that landed and left the active list between
// the two reads would pass both, and the settler would settle a move that
// happened, so the order must not be swapped back.
//
// A task's registration in the active list follows its fork within seconds,
// so after the quiet period a forked task is either listed or finished. What
// is left is a task whose registration failed and whose rename has hung for
// longer than the quiet period. That takes two separate PVE failures, and it
// strands the volume under another VM's name rather than losing it. Every move
// POST carries both configuration digests, so a late task of either kind,
// managed or legacy, lands only when neither configuration has changed since
// its POST read them. The settler writes nothing to PVE, so it never
// changes them itself.
//
// The quiet period is anchored on the record's current UpdatedAt, which moves
// with every save. While a move step is planned, admission refuses before any
// operation writes, and only the settlements save the record. The lock
// settlement saves at most once, because it leaves no planned lock step behind
// it, and the protection settlement saves at most once for each protection
// step. So an operator who reruns every minute restarts the wait a bounded
// number of times and never pushes it back forever.
//
// A settled step is recorded observed, with no UPID and the volume appended to
// its VolIDs, which is the shape the guard records for a move PVE refused in
// the request. The settler only reads. It runs only while the caller holds the
// record's journal lock, so the request that planned the step has finished or
// died.

// moveSettlementQuietPeriod is how long after a record's last save the
// settler waits before it treats a planned move step as never started.
const moveSettlementQuietPeriod = 10 * time.Minute

// settlementClockKey carries the clock WithSettlementClockForTest installs.
type settlementClockKey struct{}

// WithSettlementClockForTest returns a derived context whose move settlement
// judges the quiet period against now. It lets a test age a record through the
// real settlement path instead of sleeping.
//
// Production code MUST NOT call this.
func WithSettlementClockForTest(ctx context.Context, now func() time.Time) context.Context {
	return context.WithValue(ctx, settlementClockKey{}, now)
}

// settlementNow returns the context clock when one is installed, and the wall
// clock otherwise.
func settlementNow(ctx context.Context) time.Time {
	if ctx != nil {
		if now, ok := ctx.Value(settlementClockKey{}).(func() time.Time); ok && now != nil {
			return now().UTC()
		}
	}
	return time.Now().UTC()
}

// moveSettlementGap says why settlement left a move step planned. Its text is
// the CPI's own, and a PVE error behind it is only ever rendered through
// pve.DescribeAuditError.
type moveSettlementGap struct {
	text  string
	cause error
}

func (g *moveSettlementGap) Error() string {
	if g.cause != nil {
		return g.text + " (" + pve.DescribeAuditError(g.cause) + ")"
	}
	return g.text
}

func (g *moveSettlementGap) Unwrap() error { return g.cause }

// moveSettlementText is the clause a refusal adds for a move step the settler
// left planned, or "" when reason is not one of its gaps.
func moveSettlementText(reason error) string {
	var gap *moveSettlementGap
	if !errors.As(reason, &gap) {
		return ""
	}
	return "; its move could not be settled as never started because " + gap.Error()
}

// hasMoveSettlementGap reports whether reasons hold a gap the move settler
// left on a step.
func hasMoveSettlementGap(reasons map[string]error) bool {
	for _, reason := range reasons {
		if moveSettlementText(reason) != "" {
			return true
		}
	}
	return false
}

// isUnfiredMoveCandidate reports whether step is a move step the settler may
// judge: a planned lifecycle Nodes.CreateQemuMoveDisk step of the active
// attempt of a disk record, with no UPID, no recorded parameters, no external
// target, and a target naming its node, its source VMID, and its volume. An
// external step records work for another allocation, and the storage audit
// counts none of its volumes, so settling one would leave a later deletion
// proof without the volume as evidence.
func isUnfiredMoveCandidate(record aj.Record, step aj.Step) bool {
	if record.Kind != allocationKindDisk || step.Attempt != record.ActiveAttempt() || step.State != aj.Planned || step.UPID != "" || step.Target.External {
		return false
	}
	if !strings.HasPrefix(step.Kind, "lifecycle_") || !strings.HasSuffix(step.Kind, "_Nodes_CreateQemuMoveDisk") {
		return false
	}
	return len(step.Parameters) == 0 && step.Target.Node != "" && step.Target.VMID > 0 && step.Target.IntendedVolume != ""
}

// settlePlannedMoveSteps settles every planned move step of handle's active
// attempt that the reads above prove never started, and adds a gap to reasons
// for each one it leaves planned. It runs after settlePlannedProtectionSteps
// and takes that function's results, so a failure there passes through
// untouched.
func settlePlannedMoveSteps(ctx context.Context, client pve.Client, handle *aj.Handle, reasons map[string]error, err error) (map[string]error, error) {
	if err != nil || handle == nil {
		return reasons, err
	}
	record := handle.Record()
	if n := len(record.Attempts); n > 0 && record.Attempts[n-1].Completion != nil {
		return reasons, nil
	}
	gap := func(id string, reason error) {
		if reasons == nil {
			reasons = map[string]error{}
		}
		reasons[id] = reason
	}
	logger := log.FromContext(ctx)
	var settled []aj.Step
	for i := range record.Steps {
		step := &record.Steps[i]
		if !isUnfiredMoveCandidate(record, *step) {
			continue
		}
		if proofErr := proveMoveNeverStarted(ctx, client, record, *step); proofErr != nil {
			logger.Info("planned move step left planned",
				log.String("allocation", record.ID), log.String("step", step.ID), log.String("reason", proofErr.Error()))
			gap(step.ID, proofErr)
			continue
		}
		logger.Info("planned move step settled as never started",
			log.String("allocation", record.ID), log.String("step", step.ID), log.Int("vmid", step.Target.VMID),
			log.String("node", step.Target.Node), log.String("volume", step.Target.IntendedVolume))
		step.State = aj.Observed
		if !containsString(step.VolIDs, step.Target.IntendedVolume) {
			step.VolIDs = append(step.VolIDs, step.Target.IntendedVolume)
		}
		settled = append(settled, *step)
	}
	if len(settled) > 0 {
		if saveErr := handle.Save(record); saveErr != nil {
			return nil, refusedSettlementSave("move", settled, saveErr)
		}
	}
	return reasons, nil
}

// proveMoveNeverStarted runs the quiet period, the task listing, and the four
// proofs for one candidate step, in the order the comment above gives, and
// returns the first gap it finds, or nil when the move never started.
func proveMoveNeverStarted(ctx context.Context, client pve.Client, record aj.Record, step aj.Step) error {
	if ends := record.UpdatedAt.Add(moveSettlementQuietPeriod); settlementNow(ctx).Before(ends) {
		return &moveSettlementGap{text: "its quiet period ends at " + ends.UTC().Format(time.RFC3339)}
	}
	if record.DiskToken == "" {
		return &moveSettlementGap{text: "the record names no disk serial"}
	}
	if client == nil {
		return &moveSettlementGap{text: "no PVE client is available to read the move's source"}
	}
	c := unguardedPVE(client)
	source, node, volume := step.Target.VMID, step.Target.Node, step.Target.IntendedVolume

	guests, err := pve.ListGuestsAuthoritative(ctx, c, nil)
	if err != nil {
		return &moveSettlementGap{text: "the cluster's guests could not be listed", cause: err}
	}
	current := ""
	for _, guest := range guests {
		if guest.VMID == source {
			current = guest.Node
		}
	}
	if current == "" {
		return &moveSettlementGap{text: fmt.Sprintf("VM %d is not listed on any node", source)}
	}
	if err := noActiveMoveTask(ctx, c, source, node, current); err != nil {
		return err
	}

	key, err := sourceStillNamesVolume(ctx, c, source, current, volume, record.DiskToken)
	if err != nil {
		return err
	}
	if strings.HasPrefix(key, "unused") {
		if err := transferLandingEmpty(ctx, c, source, volume, record.DiskToken); err != nil {
			return err
		}
	}
	if err := volumeExistsOnNode(ctx, c, node, volume); err != nil {
		return err
	}
	holders, err := pve.FindDiskKeyHolders(ctx, c, volume, record.DiskToken)
	if err != nil {
		return &moveSettlementGap{text: "the guests that hold the disk could not be read", cause: err}
	}
	for _, holder := range holders {
		if holder.VMID == source && holder.Slot == key && !strings.HasPrefix(key, "unused") && holder.StillNames {
			continue
		}
		if holder.CarriesSerial {
			return &moveSettlementGap{text: fmt.Sprintf("VM %d holds the disk's serial on %s", holder.VMID, holder.Slot)}
		}
		if holder.Parker && holder.NamesVolume {
			return &moveSettlementGap{text: fmt.Sprintf("parker %d names volume %s on %s", holder.VMID, volume, holder.Slot)}
		}
	}
	return nil
}

// noActiveMoveTask lists the active qmmove tasks on the step's node and on
// the node the source lives on now, and returns a gap when any of them names
// the source or a listing can't be trusted.
func noActiveMoveTask(ctx context.Context, c pve.Client, source int, nodes ...string) error {
	reader, ok := c.(pve.ActiveMoveTaskReader)
	if !ok {
		return &moveSettlementGap{text: "the PVE client cannot list active move tasks"}
	}
	seen := map[string]bool{}
	for _, node := range nodes {
		if seen[node] {
			continue
		}
		seen[node] = true
		tasks, err := reader.ActiveMoveTasks(ctx, node)
		if err != nil {
			var visibility *pve.AuditVisibilityError
			if errors.As(err, &visibility) && visibility.Privilege == "Sys.Audit" {
				return &moveSettlementGap{text: fmt.Sprintf("listing move tasks on node %s needs Sys.Audit on /nodes/%s", node, node)}
			}
			return &moveSettlementGap{text: fmt.Sprintf("the active move tasks on node %s could not be listed", node), cause: err}
		}
		for _, task := range tasks {
			if pve.MoveTaskIDNamesVM(task.ID, source) {
				return &moveSettlementGap{text: fmt.Sprintf("move task %s is still active on node %s", task.UPID, node)}
			}
		}
	}
	return nil
}

// sourceStillNamesVolume is proof 1 on the source. It returns the one disk key
// of the source that names volume, after checking that the key's applied value
// names it with no pending delete and no pending replacement, and that a bus
// slot carries the disk's serial and no other.
func sourceStillNamesVolume(ctx context.Context, c pve.Client, source int, node, volume, serial string) (string, error) {
	views, err := pve.ReadQemuViews(ctx, c, node, source)
	if err != nil {
		if pve.IsNotFound(err) || pve.IsPmxcfsConfigMissing(err) {
			return "", &moveSettlementGap{text: fmt.Sprintf("VM %d no longer exists on node %s", source, node), cause: err}
		}
		return "", &moveSettlementGap{text: fmt.Sprintf("the config of VM %d could not be read", source), cause: err}
	}
	keys := views.SlotsNaming(volume)
	switch {
	case len(keys) == 0:
		return "", &moveSettlementGap{text: fmt.Sprintf("no disk key of VM %d names volume %s", source, volume)}
	case len(keys) > 1:
		return "", &moveSettlementGap{text: fmt.Sprintf("disk keys %s of VM %d all name volume %s", strings.Join(keys, ", "), source, volume)}
	}
	key := keys[0]
	if views.PendingDelete(key) {
		return "", &moveSettlementGap{text: fmt.Sprintf("VM %d has a pending delete of %s", source, key)}
	}
	if _, replaced := views.PendingReplacements()[key]; replaced {
		return "", &moveSettlementGap{text: fmt.Sprintf("VM %d has a pending replacement of %s", source, key)}
	}
	if !pve.MoveSourceStillNames(views, key, volume) {
		return "", &moveSettlementGap{text: fmt.Sprintf("%s of VM %d no longer names volume %s", key, source, volume)}
	}
	if strings.HasPrefix(key, "unused") {
		return key, nil
	}
	for _, view := range []map[string]any{views.Current(), views.Applied()} {
		drive, ok := pve.ConfigString(view, key)
		if !ok {
			continue
		}
		found, carries := pve.StableIDFromDriveOptStr(drive)
		if !carries {
			return "", &moveSettlementGap{text: fmt.Sprintf("%s of VM %d names volume %s without the disk's serial", key, source, volume)}
		}
		if found != serial {
			return "", &moveSettlementGap{text: fmt.Sprintf("%s of VM %d names volume %s with another disk's serial %s", key, source, volume, found)}
		}
	}
	return key, nil
}

// transferLandingEmpty is proof 1 on the receiving side, for a source key that
// is an unused entry. Only one parker may keep a record of the disk, so the
// record that proves the transfer and the landing check read the same parker.
// That parker must keep the record of the disk's transfer from source under
// volume, and it may hold, on no disk key, a volume named for it with no
// serial.
func transferLandingEmpty(ctx context.Context, c pve.Client, source int, volume, serial string) error {
	records, err := pve.FindDiskTransferRecords(ctx, c, serial)
	if err != nil {
		return &moveSettlementGap{text: "the parkers' records of the disk's transfer could not be read", cause: err}
	}
	for _, record := range records {
		if record.ParkerVMID != records[0].ParkerVMID {
			return &moveSettlementGap{text: fmt.Sprintf("parkers %d and %d each keep a record of the disk's transfer", records[0].ParkerVMID, record.ParkerVMID)}
		}
	}
	kept := false
	for _, record := range records {
		if record.UnclaimedLanding != "" {
			return &moveSettlementGap{text: fmt.Sprintf("%s of parker %d, which keeps the disk's transfer record, holds a volume named for the parker with no serial", record.UnclaimedLanding, record.ParkerVMID)}
		}
		if record.Slot != "" && record.Volid == volume && record.SourceVMCID == strconv.Itoa(source) {
			kept = true
		}
	}
	if !kept {
		return &moveSettlementGap{text: fmt.Sprintf("no parker keeps the record of the disk's transfer from VM %d", source)}
	}
	return nil
}

// volumeExistsOnNode is proof 2. It proves the volume exists under its name on
// node by the content listing and then by the exact content read, the way
// observeManagedDiskVolume does.
func volumeExistsOnNode(ctx context.Context, c pve.Client, node, volume string) error {
	present, err := pve.ObserveStorageVolumePresence(ctx, c, node, volume)
	if err != nil {
		return &moveSettlementGap{text: fmt.Sprintf("the storage content on node %s could not be listed", node), cause: err}
	}
	if !present {
		return &moveSettlementGap{text: fmt.Sprintf("volume %s is not listed on node %s", volume, node)}
	}
	storage, bare, err := pve.ParseDiskCID(volume)
	if err != nil {
		return &moveSettlementGap{text: fmt.Sprintf("volume %s is not a disk volume", volume)}
	}
	nodes := c.Nodes()
	if nodes == nil {
		return &moveSettlementGap{text: "no node service is available to read the volume"}
	}
	info, err := nodes.GetStorageContent(ctx, node, storage, bare)
	if err != nil {
		if pve.IsNotFound(err) {
			return &moveSettlementGap{text: fmt.Sprintf("volume %s is not on node %s", volume, node), cause: err}
		}
		return &moveSettlementGap{text: fmt.Sprintf("volume %s could not be read on node %s", volume, node), cause: err}
	}
	if info == nil || info.Size <= 0 || info.Format == "" {
		return &moveSettlementGap{text: fmt.Sprintf("PVE returned malformed content for volume %s", volume)}
	}
	return nil
}
