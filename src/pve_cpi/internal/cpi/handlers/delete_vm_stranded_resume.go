package handlers

import (
	"context"
	"fmt"
	"strings"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// guardUnusedVolumesResumingTransfers is guardUnusedVolumes for delete_vm's own
// destroy paths. Before it refuses, it finishes any transfer to a parker that
// stopped after deleting this VM's slot, and then it scans again.
//
// detachForeignActiveDisks moves a stable-ID disk to a parker by deleting its
// slot and then moving the unused entry PVE leaves behind. When the move fails,
// the volume stays on this VM's unused entry with no serial, so a retry finds
// no slot to transfer, and the guard refuses over the entry. The parker's
// transfer record still names the volume, and that record is what the retry
// finishes from here. create-env can't issue the detach_disk that finishes it
// any other way.
//
// The step only ever moves a volume the guard already protects, and only
// through the same resume detach_disk runs, so the resume's own proofs decide
// whether the volume is the disk. Nothing here deletes a volume or an unused
// entry. The destroy still waits on a fresh scan that finds nothing protected,
// so any entry the step didn't move keeps today's refusal.
func guardUnusedVolumesResumingTransfers(
	ctx context.Context, deps Deps, node, vmCID string, vmid int, diskStorage string, logger *log.Logger,
) error {
	protected, err := protectedUnusedVolumes(ctx, deps, node, vmCID, vmid, diskStorage)
	if err != nil || len(protected) == 0 {
		return err
	}
	resumed, err := resumeStrandedTransfers(ctx, deps, node, vmCID, vmid, protected, logger)
	if err != nil {
		return err
	}
	if resumed {
		if protected, err = protectedUnusedVolumes(ctx, deps, node, vmCID, vmid, diskStorage); err != nil {
			return err
		}
	}
	return unusedVolumesRefusal(vmCID, protected)
}

// resumeStrandedTransfers finishes the transfers a parker's record names for
// the protected entries, and reports whether it finished any.
//
// An entry qualifies only when exactly one record names its volume as a
// transfer from this VM, that record belongs to no managed allocation, and its
// parker is on this VM's node, where every delete_vm transfer puts it. A
// managed disk's resume needs its allocation's own context, which this path
// doesn't have, and a record on another node or two records for one volume
// prove nothing about which disk the volume is. Each of those keeps the
// refusal for an operator to settle.
func resumeStrandedTransfers(
	ctx context.Context, deps Deps, node, vmCID string, vmid int, protected []unusedVolume, logger *log.Logger,
) (bool, error) {
	if deps.Config == nil {
		return false, nil
	}
	records, err := pve.FindSourceTransferRecords(ctx, deps.PVE, vmCID, parkerReadConfigFor(deps))
	if err != nil {
		return false, retriableUnlessPermanent(err, fmt.Sprintf(
			"delete_vm: refusing to destroy VM %s -- could not read the parkers' transfer records before deciding about its unused volumes, so nothing was destroyed; retry delete_vm",
			vmCID))
	}
	byVolid := map[string][]pve.SourceTransferRecord{}
	for i := range records {
		byVolid[records[i].Intent.Volid] = append(byVolid[records[i].Intent.Volid], records[i])
	}
	resumed := false
	for _, entry := range protected {
		matches := byVolid[entry.volid]
		if len(matches) == 0 {
			continue
		}
		record := matches[0]
		skip := ""
		switch {
		case len(matches) > 1:
			skip = "more than one parker record names the volume"
		case record.Intent.AllocationID != "" || record.Intent.AllocationNamespace != "":
			skip = "the record belongs to a managed allocation"
		case record.Intent.ParkerNode != node:
			skip = "the record's parker is on another node"
		}
		if skip != "" {
			logger.Warn("delete_vm: unused volume has a parker transfer record this path can't finish -- leaving it for the refusal",
				log.String("slot", entry.slot), log.String("volid", entry.volid), log.String("reason", skip))
			continue
		}
		done, resumeErr := resumeStrandedTransfer(ctx, deps, node, vmCID, vmid, entry, record, logger)
		if resumeErr != nil {
			return resumed, resumeErr
		}
		resumed = resumed || done
	}
	return resumed, nil
}

// resumeStrandedTransfer finishes one record's transfer the way detach_disk
// does. It resolves the disk by the record's stable ID and CID, goes on only
// when the resolution reaches the same record, and then runs resumeTransfer,
// whose proofs refuse any volume they can't show is the disk. It reports
// whether the resume ran.
func resumeStrandedTransfer(
	ctx context.Context, deps Deps, node, vmCID string, vmid int, entry unusedVolume, record pve.SourceTransferRecord, logger *log.Logger,
) (bool, error) {
	// A transfer delete_vm started records the volume's own name as its CID,
	// and one a disk call started records the Director's CID. The resolution
	// takes the birth name and the metadata from the CID when it carries this
	// stable ID, and from the record otherwise.
	// A Director's CID that doesn't decode, or that carries another stable ID,
	// proves nothing about the volume, so it keeps the refusal.
	diskCID := record.DiskCID
	if diskCID == "" {
		diskCID = record.Intent.Volid
	}
	birth, meta := record.Intent.Volid, &pve.DiskCIDMeta{ID: record.StableID}
	if diskCID != record.Intent.Volid {
		bare, parsed, decodeErr := decodeDiskCID(ctx, deps, "delete_vm", diskCID)
		if decodeErr != nil || parsed == nil || parsed.ID != record.StableID {
			logger.Warn("delete_vm: a parker record's disk CID doesn't carry the record's stable ID -- leaving the unused volume for the refusal",
				log.String("slot", entry.slot), log.String("volid", entry.volid), log.String("stable_id", record.StableID))
			return false, nil
		}
		birth, meta = bare, parsed
	}
	logger.Warn("delete_vm: unused volume is a transfer to a parker that stopped after the slot delete -- finishing it before destroy",
		log.String("slot", entry.slot), log.String("volid", entry.volid), log.String("stable_id", record.StableID),
		log.Int("parker_vmid", record.Intent.ParkerVMID))
	rd, err := resolveDiskForOp(ctx, deps, "delete_vm", diskCID, birth, meta)
	if err != nil {
		return false, retriableUnlessPermanent(err, fmt.Sprintf(
			"delete_vm: refusing to destroy VM %s -- could not resolve the disk that parker vmid %d's transfer record names on unused entry %s=%s, so nothing was destroyed",
			vmCID, record.Intent.ParkerVMID, entry.slot, entry.volid))
	}
	if rd.intent == nil || rd.allocation != nil || !sameTransfer(*rd.intent, record.Intent) {
		logger.Warn("delete_vm: the disk a parker record names doesn't resolve to that record's transfer -- leaving the unused volume for the refusal",
			log.String("slot", entry.slot), log.String("volid", entry.volid), log.String("stable_id", record.StableID))
		return false, nil
	}
	refreshed, err := resumeTransfer(ctx, deps, "delete_vm", rd, false)
	if err != nil {
		if pve.IsMoveDiskSnapshotRefusal(err) {
			return false, strandedSnapshotRefusal(ctx, deps, node, vmCID, vmid, entry)
		}
		return false, err
	}
	// The disk's attached-CID note on this VM goes the way detachForeignActiveDisks
	// removes it after a transfer that finished in one go, but only once the
	// disk is on a parker. The scan the caller runs next decides the destroy.
	if refreshed.holder != nil && refreshed.holder.IsParker {
		pve.RemoveAttachedDiskCID(ctx, deps.PVE, logger, node, vmid, record.StableID, entry.volid)
	}
	return true, nil
}

// strandedSnapshotRefusal is the error for a resume that PVE's snapshot check
// stopped. The refusal comes from reassigning the volume, and only an operator
// can clear it, so it is permanent, the class detach_disk gives a snapshot
// block, and its text names the snapshots to delete. A failed snapshot listing
// leaves the names out and keeps the class.
func strandedSnapshotRefusal(ctx context.Context, deps Deps, node, vmCID string, vmid int, entry unusedVolume) error {
	names, err := pve.HasSnapshots(ctx, deps.PVE, node, vmid)
	if err != nil || len(names) == 0 {
		return cpierrors.SnapshotBlocked(
			"delete_vm: refusing to destroy VM %s -- PVE won't move unused entry %s=%s to its parker while a snapshot of the VM references it, so nothing was destroyed."+
				" Delete the VM's snapshot that references the volume, then retry delete_vm",
			vmCID, entry.slot, entry.volid)
	}
	return cpierrors.SnapshotBlocked(
		"delete_vm: refusing to destroy VM %s -- PVE won't move unused entry %s=%s to its parker while the VM's snapshot(s) [%s] reference it, so nothing was destroyed."+
			" Delete snapshot(s) [%s], then retry delete_vm",
		vmCID, entry.slot, entry.volid, strings.Join(names, ", "), strings.Join(names, ", "))
}
