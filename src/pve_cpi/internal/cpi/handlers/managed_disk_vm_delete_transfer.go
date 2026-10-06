package handlers

import (
	"context"
	"fmt"
	"strconv"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// managedSourceTransferRecords returns the parkers' records of a
// journal-managed disk's transfer whose source is VM vmid and that hasn't
// landed. A record whose parker already carries the disk's serial is left out
// before any disk is resolved, so a disk that has moved on since can't stop the
// destroy. A record that belongs to no allocation is left to the legacy path,
// which finishes it from the VM's unused entry (see resumeStrandedTransfers). A
// deployment without a configuration has no parker band and finds nothing. A
// failed read refuses the destroy, because a record we never read must not
// count as one that isn't there.
func managedSourceTransferRecords(ctx context.Context, deps Deps, vmid int) ([]pve.SourceTransferRecord, error) {
	if deps.Config == nil {
		return nil, nil
	}
	records, err := pve.FindSourceTransferRecords(ctx, deps.PVE, strconv.Itoa(vmid), parkerReadConfigFor(deps))
	if err != nil {
		return nil, retriableUnlessPermanent(err, fmt.Sprintf(
			"delete_vm: refusing to destroy VM %d because the parkers' transfer records could not be read, so nothing was destroyed; retry delete_vm once the parkers' configs can be read",
			vmid))
	}
	var managed []pve.SourceTransferRecord
	for i := range records {
		if records[i].Intent.AllocationID != "" || records[i].Intent.AllocationNamespace != "" {
			managed = append(managed, records[i])
		}
	}
	return managed, nil
}

// preserveManagedTransferForVMDelete finishes the transfer that record names
// from VM vmid to its parker, so delete_vm never destroys the VM while a
// journal-managed disk it was moving off hasn't landed.
//
// A record whose disk resolves without a transfer in flight has landed, or the
// disk has moved on since, and needs nothing here. Otherwise the disk must
// resolve to this same transfer, with no slot that carries its serial, and the
// transfer is finished under the disk's own lifecycle, which settles a source
// slot delete PVE already applied (see settlePlannedTransferSourceWrite) and
// then resumes the transfer the way detach_disk does. When a snapshot of the
// VM still names the volume, so the move to the parker is refused, it returns
// the same SnapshotBlocked error the disk calls give a deferred park. The disk
// must then read back on its parker with no transfer in flight. Anything else
// refuses the destroy and leaves the VM in place.
func preserveManagedTransferForVMDelete(ctx context.Context, deps Deps, vmid int, record pve.SourceTransferRecord) (operationErr error) {
	ctx = managedLockWaitContext(ctx)
	refuse := func(reason string) error {
		return cpierrors.Cloud(
			"delete_vm: refusing to destroy VM %d because parker vmid %d keeps a record of disk %s moving off it and %s, so nothing was destroyed; read the disk's allocation %s with storage-journal audit, and rerun delete_vm once the disk is on a parker",
			vmid, record.Intent.ParkerVMID, record.StableID, reason, record.Intent.AllocationID)
	}
	diskCID := record.DiskCID
	if diskCID == "" {
		diskCID = record.Intent.Volid
	}
	birth, meta := record.Intent.Volid, &pve.DiskCIDMeta{ID: record.StableID}
	if diskCID != record.Intent.Volid {
		bare, parsed, err := decodeDiskCID(ctx, deps, "delete_vm.preserve_disk", diskCID)
		if err != nil || parsed == nil || parsed.ID != record.StableID {
			return refuse("the record's disk CID doesn't carry the disk's identity")
		}
		birth, meta = bare, parsed
	}
	rd, err := resolveDiskForOp(ctx, deps, "delete_vm.preserve_disk", diskCID, birth, meta)
	if err != nil {
		return retriableUnlessPermanent(err, fmt.Sprintf(
			"delete_vm: refusing to destroy VM %d because disk %s, which parker vmid %d's transfer record names as moving off it, could not be resolved, so nothing was destroyed; retry delete_vm",
			vmid, record.StableID, record.Intent.ParkerVMID))
	}
	if rd.intent == nil {
		return nil
	}
	if !sameTransfer(*rd.intent, record.Intent) {
		return refuse(fmt.Sprintf("the disk resolves to a transfer to parker vmid %d instead", rd.intent.ParkerVMID))
	}
	if rd.allocation == nil {
		return refuse("the disk resolves to no allocation the journal manages")
	}
	deps.Log(ctx).Warn("delete_vm: a managed disk's transfer to a parker stopped after its slot delete on this VM -- finishing it before destroy",
		log.Int("vmid", vmid), log.String("stable_id", record.StableID), log.String("volid", record.Intent.Volid),
		log.Int("parker_vmid", record.Intent.ParkerVMID))
	local, lifecycle, err := managedDiskOperation(ctx, deps, rd, "delete_vm.preserve_disk")
	if err != nil {
		return protectionPendingPreservation(vmid, err)
	}
	if lifecycle == nil {
		return fmt.Errorf("persistent disk preservation did not acquire allocation ownership")
	}
	defer func() { operationErr = lifecycle.finish(ctx, operationErr, false) }()
	current := lifecycle.disk
	if current.intent == nil {
		if current.holder != nil && current.holder.IsParker {
			return nil
		}
		return refuse("the disk changed before its transfer could be finished")
	}
	if current.holder != nil || !sameTransfer(*current.intent, record.Intent) {
		return refuse("the disk changed before its transfer could be finished")
	}
	// detach_disk answers a snapshot refusal with success, because its disk is
	// already off the bus. We resume here instead, so the refusal reaches the
	// destroy it has to stop and names the snapshot to delete.
	resumed, err := resumeTransferIfNeeded(ctx, local, "detach_disk", current)
	if err != nil {
		if pve.IsMoveDiskSnapshotRefusal(err) {
			if refusal := deferredParkSnapshotRefusal(ctx, local, "delete_vm", current, err); refusal != nil {
				return refusal
			}
		}
		return err
	}
	if err := handleDetachStableID(ctx, local, strconv.Itoa(vmid), vmid, resumed); err != nil {
		return err
	}
	landed, err := resolveDiskForOp(ctx, deps, "delete_vm.preserve_disk", diskCID, birth, meta)
	if err != nil {
		return retriableUnlessPermanent(err, fmt.Sprintf(
			"delete_vm: refusing to destroy VM %d because disk %s could not be read back after its transfer to parker vmid %d, so nothing was destroyed; retry delete_vm",
			vmid, record.StableID, record.Intent.ParkerVMID))
	}
	if landed.intent != nil || landed.holder == nil || !landed.holder.IsParker {
		return refuse("the disk didn't read back on a parker after its transfer was resumed")
	}
	return nil
}
