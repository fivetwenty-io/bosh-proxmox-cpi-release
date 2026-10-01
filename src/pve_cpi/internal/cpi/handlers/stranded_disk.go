package handlers

// A stable-ID disk is stranded when its transfer to a parker deleted the
// guest's slot, the move never finished, and the transfer record that linked
// the disk's CID to the volume was lost afterwards. PVE keeps a deleted slot as
// an unusedN entry only when the guest owns the volume by name, so this needs a
// disk whose birth name carries the guest's own VMID, which a relocated VM band
// allows. No slot carries the disk's serial, so the identity scan finds no
// holder, and removing the unused entry by hand makes PVE free the volume. The
// identity scan reports those entries in resolvedDisk.unused, and the handlers
// here either move the disk off the entry or refuse with the entry named.

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// strandedDiskRunbook points the stranded-disk refusals at the way out. The
// docs do not ship in the release, so it names the repository as well as the
// file, and a test pins the heading it quotes.
const strandedDiskRunbook = `see "delete_disk refuses a disk stranded on an unused entry" in docs/troubleshooting.md of bosh-proxmox-cpi-release`

// strandedEntry returns the one guest that holds the disk only on an unused
// entry. It reports false when nothing strands the disk, and false with
// ambiguous set when the entries can't be tied to one guest: more than one
// entry names the volume, or the only one sits on a VM in the parker band,
// where an entry is the leftover of an unpark rather than a stranded transfer.
func strandedEntry(deps Deps, rd resolvedDisk) (ref pve.VolumeReference, stranded, ambiguous bool) {
	switch len(rd.unused) {
	case 0:
		return pve.VolumeReference{}, false, false
	case 1:
		cfg := parkerReadConfigFor(deps)
		ref = rd.unused[0]
		if ref.VMID >= cfg.VMIDRangeStart && ref.VMID <= cfg.VMIDRangeEnd {
			return pve.VolumeReference{}, false, true
		}
		return ref, true, false
	default:
		return pve.VolumeReference{}, false, true
	}
}

// describeUnusedEntry names one unused entry the way the refusals quote it.
func describeUnusedEntry(ref pve.VolumeReference) string {
	return fmt.Sprintf("unused entry %s of VM %d on node %s", ref.Slot, ref.VMID, ref.Node)
}

// strandedAmbiguousRefusal refuses a disk whose unused entries can't be tied
// to one guest. Every handler refuses it the same way, because moving the
// volume off any one of them could take it from the guest that should hold it.
func strandedAmbiguousRefusal(op string, rd resolvedDisk) error {
	entries := make([]string, 0, len(rd.unused))
	for _, ref := range rd.unused {
		entries = append(entries, describeUnusedEntry(ref))
	}
	return cpierrors.Cloud(
		"%s: refusing disk %s, because no slot carries its volume %s and the unused entries that name it can't be tied to one guest outside the parker band: %s. "+
			"Removing any of those entries by hand can make PVE free the volume. Nothing was changed; %s",
		op, rd.diskCID, rd.volid, strings.Join(entries, ", "), strandedDiskRunbook,
	)
}

// refuseStrandedDisk refuses any stranded or ambiguous disk for a handler that
// has no way to act on an unused entry. It runs before a managed lifecycle
// opens, so the refusal leaves the allocation record as it was.
func refuseStrandedDisk(deps Deps, op string, rd resolvedDisk) error {
	ref, stranded, ambiguous := strandedEntry(deps, rd)
	switch {
	case ambiguous:
		return strandedAmbiguousRefusal(op, rd)
	case stranded:
		return cpierrors.Cloud(
			"%s: refusing disk %s, because its volume %s sits on %s and no slot carries it. Nothing was changed; %s",
			op, rd.diskCID, rd.volid, describeUnusedEntry(ref), strandedDiskRunbook,
		)
	}
	return nil
}

// refuseStrandedDelete is delete_disk's refusal. The Director sends delete_disk
// only for a disk it treats as orphaned, so it has no detach to send, and the
// runbook gives the two ways out: attach the disk to an instance, which moves
// it off the entry, or check the disk and remove the entry, which frees it.
func refuseStrandedDelete(deps Deps, rd resolvedDisk) error {
	ref, stranded, ambiguous := strandedEntry(deps, rd)
	switch {
	case ambiguous:
		return strandedAmbiguousRefusal("delete_disk", rd)
	case stranded:
		return cpierrors.Cloud(
			"delete_disk: refusing to delete disk %s, because VM %d on node %s still names its volume %s as %s, and no other configuration does. "+
				"Removing that entry by hand makes PVE free the volume. Nothing was deleted; %s",
			rd.diskCID, ref.VMID, ref.Node, rd.volid, ref.Slot, strandedDiskRunbook,
		)
	}
	return nil
}

// refuseStrandedDetach refuses a detach the stranded disk can't satisfy: one
// whose entry sits on a VM other than the one being detached, which nothing in
// the CPI would find again, or one whose entries are ambiguous. A disk
// stranded on the VM being detached is left to handleDetachStableID, which
// moves it to a parker.
func refuseStrandedDetach(deps Deps, vmid int, rd resolvedDisk) error {
	ref, stranded, ambiguous := strandedEntry(deps, rd)
	switch {
	case ambiguous:
		return strandedAmbiguousRefusal("detach_disk", rd)
	case stranded && ref.VMID != vmid:
		return cpierrors.Cloud(
			"detach_disk: refusing to detach disk %s from VM %d, because its volume %s sits on %s, and no slot carries it. "+
				"Removing that entry by hand makes PVE free the volume. Nothing was changed; %s",
			rd.diskCID, vmid, rd.volid, describeUnusedEntry(ref), strandedDiskRunbook,
		)
	}
	return nil
}

// refuseStrandedManagedAttach refuses a journal-managed disk that is stranded
// or ambiguous before attach_disk opens its lifecycle. Its allocation journal
// has no step that moves a disk off another VM's unused entry, so a transfer
// would be refused inside the lifecycle and leave the record needing
// reconciliation.
func refuseStrandedManagedAttach(deps Deps, vmCID string, rd resolvedDisk) error {
	if rd.allocation == nil {
		return nil
	}
	ref, stranded, ambiguous := strandedEntry(deps, rd)
	switch {
	case ambiguous:
		return strandedAmbiguousRefusal("attach_disk", rd)
	case stranded:
		return cpierrors.Cloud(
			"attach_disk: refusing to attach journal-managed disk %s to VM %s, because its volume %s sits on %s and no slot carries it. "+
				"Its allocation journal can't move it off that entry yet, so nothing was changed. Leave the entry in place, because removing it by hand "+
				"makes PVE free the volume, and run storage-journal audit --summary to see the allocation; %s",
			rd.diskCID, vmCID, rd.volid, describeUnusedEntry(ref), strandedDiskRunbook,
		)
	}
	return nil
}

// settleStrandedBeforeAttach handles a stranded disk inside the attach guard.
// It reports true when the disk is stranded on the target VM itself, where the
// ordinary attach lands it with one reference, because PVE drops an unused
// entry when the same volume is attached again. A disk stranded on another
// VM moves to a parker on that VM's node first on the plain attach_disk and
// create_vm paths, and rd is re-resolved so the parked plan picks it up. That
// is how bosh attach-disk recovers an orphaned disk. The create_vm paths of a
// journal-managed VM, and any ambiguous disk, refuse instead.
func settleStrandedBeforeAttach(ctx context.Context, deps Deps, op string, rd *resolvedDisk, targetVMID int) (onTarget bool, err error) {
	ref, stranded, ambiguous := strandedEntry(deps, *rd)
	switch {
	case ambiguous:
		return false, strandedAmbiguousRefusal(op, *rd)
	case !stranded:
		return false, nil
	case ref.VMID == targetVMID:
		return true, nil
	case rd.allocation != nil || (op != "attach_disk" && op != "create_vm"):
		return false, cpierrors.Cloud(
			"%s: refusing to attach disk %s to VM %d, because its volume %s sits on %s and no slot carries it. "+
				"This path doesn't move a disk off another VM, so nothing was attached. Removing that entry by hand makes PVE free the volume; %s",
			op, rd.diskCID, targetVMID, rd.volid, describeUnusedEntry(ref), strandedDiskRunbook,
		)
	}

	logger := deps.Log(ctx)
	// Moving the disk off the stranded VM detaches it from that VM, so the
	// same snapshot guard detach_disk runs applies to it first.
	if err := strandedAttachSnapshotGuard(ctx, deps, op, *rd, ref, targetVMID); err != nil {
		return false, err
	}
	logger.Warn(op+": disk is stranded on another VM's unused entry; moving it to a parker before the attach",
		log.String("disk_cid", rd.diskCID),
		log.Int("stranded_vmid", ref.VMID),
		log.String("stranded_slot", ref.Slot),
	)
	// The recorded option overrides ride the transfer in its intent record,
	// the same way detach_disk carries them, so read them off the stranded VM
	// first and fail the attach retriably when that read fails.
	overlay, ovErr := pve.GetVMDiskOptOverlay(ctx, deps.PVE, ref.Node, ref.VMID, rd.stableID, rd.volid, rd.birth)
	if ovErr != nil {
		return false, retriableUnlessPermanent(ovErr,
			fmt.Sprintf("%s: read recorded option overrides for disk %s on VM %d", op, rd.diskCID, ref.VMID))
	}
	parkerCfg := parkerWriteConfigFor(deps)
	pctx := pve.ParkContext{DiskCID: rd.diskCID, SourceVMCID: strconv.Itoa(ref.VMID), StableID: rd.stableID, Opts: overlay}
	if _, err := pve.TransferDiskToParker(ctx, deps.PVE, logger, ref.Node, ref.VMID, rd.volid, parkerCfg, pctx); err != nil {
		return false, retriableUnlessPermanent(err,
			fmt.Sprintf("%s: move disk %s off %s to a parker (fail-closed: retry resumes the transfer)", op, rd.diskCID, describeUnusedEntry(ref)))
	}
	sweepParkerPool(ctx, deps, ref.Node, parkerCfg)
	// The parker's record now carries the disk, so the stranded VM's own
	// records of it can come off, as they do after an ordinary detach.
	pve.RemoveAttachedDiskCID(ctx, deps.PVE, logger, ref.Node, ref.VMID, rd.stableID, rd.volid)
	pve.RemoveVMDiskOptOverlay(ctx, deps.PVE, logger, ref.Node, ref.VMID, rd.stableID, rd.volid, rd.birth)

	refreshed, err := resolveDiskForOp(ctx, deps, op, rd.diskCID, rd.birth, rd.meta)
	if err != nil {
		return false, err
	}
	if refreshed.holder == nil || !refreshed.holder.IsParker {
		return false, cpierrors.Retriable("%s: disk %s did not resolve to a parker after it moved off %s; retry",
			op, rd.diskCID, describeUnusedEntry(ref))
	}
	*rd = refreshed
	return false, nil
}

// strandedAttachSnapshotGuard is detach_disk's snapshot guard for the move off
// the stranded VM. It reads that VM's snapshots, and it follows
// allow_disk_ops_with_snapshots and require_snapshot_check_pass the same way.
func strandedAttachSnapshotGuard(ctx context.Context, deps Deps, op string, rd resolvedDisk, ref pve.VolumeReference, targetVMID int) error {
	logger := deps.Log(ctx)
	snapNames, snapErr := pve.HasSnapshots(ctx, deps.PVE, ref.Node, ref.VMID)
	if snapErr != nil {
		if deps.Config.RequireSnapshotCheckPass {
			return cpierrors.Wrap(snapErr, fmt.Sprintf(
				"%s: snapshot pre-flight check on VM %d, where disk %s is stranded, failed and require_snapshot_check_pass is set",
				op, ref.VMID, rd.diskCID))
		}
		logger.Warn(op+": snapshot pre-flight check on the stranded VM failed; proceeding (fail-open)",
			log.String("node", ref.Node),
			log.Int("vmid", ref.VMID),
			log.Err(snapErr),
		)
		return nil
	}
	if len(snapNames) == 0 {
		return nil
	}
	if deps.Config.AllowDiskOpsWithSnapshots {
		logger.Warn(op+": moving a stranded disk despite snapshots on its VM (allow_disk_ops_with_snapshots=true)",
			log.Int("stranded_vmid", ref.VMID),
			log.String("node", ref.Node),
			log.String("snapshots", strings.Join(snapNames, ", ")),
		)
		return nil
	}
	return cpierrors.SnapshotBlocked(
		"%s: refusing to attach disk %s to VM %d, because the attach first moves it off %s, which detaches it from VM %d, "+
			"and VM %d (node %s) has %d snapshot(s) [%s] that can reference it. Delete snapshot(s) [%s] first, then retry; "+
			"or set pve.allow_disk_ops_with_snapshots=true to bypass this guard",
		op, rd.diskCID, targetVMID, describeUnusedEntry(ref), ref.VMID, ref.VMID, ref.Node, len(snapNames),
		strings.Join(snapNames, ", "), strings.Join(snapNames, ", "),
	)
}
