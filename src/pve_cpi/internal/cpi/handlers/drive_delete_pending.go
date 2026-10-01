package handlers

import (
	"fmt"
	"sort"
	"strings"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// classedPendingDelete is the error a caller returns once it has chosen the
// class of a pending slot delete. Its text and class are the classed error's,
// which is what the dispatcher finds first and sends to the Director, and its
// chain also holds the typed error, so a journal-managed operation's finish
// can tell why it failed.
type classedPendingDelete struct {
	classed *cpierrors.Error
	pending *pve.DriveDeletePendingError
}

func (e *classedPendingDelete) Error() string { return e.classed.Error() }

func (e *classedPendingDelete) Unwrap() []error { return []error{e.classed, e.pending} }

// classPendingDelete pairs a classed error with the typed error it classes.
func classPendingDelete(classed *cpierrors.Error, pending *pve.DriveDeletePendingError) error {
	return &classedPendingDelete{classed: classed, pending: pending}
}

// driveDeletePendingDiskError chooses the class detach_disk and attach_disk
// give a slot delete that PVE could only record as pending. pve.DeleteDriveSlot
// has already reverted it by the time it returns the typed error, and the
// typed error reaches here through pve.TransferDiskToParker as well as
// straight from the helper. It returns nil when err carries no such error, and
// the caller keeps its own handling.
//
// A busy guest is retriable, because the guest may let go and a later attempt
// can succeed. A hotplug setting that lacks disk isn't, because no retry can
// succeed while the VM runs with it. An unconfirmed revert is retriable, so the
// operator and the next attempt both know a pending delete may still be there,
// and the next attempt reverts it again. A pending delete that a resume found
// and left alone is retriable, because a stop of the VM applies it and the
// transfer then finishes.
func driveDeletePendingDiskError(op string, err error) error {
	pending, ok := pve.IsDriveDeletePending(err)
	if !ok {
		return nil
	}
	switch pending.Reason {
	case pve.DriveDeletePendingHotplug:
		return classPendingDelete(cpierrors.Cloud(
			"%s: VM %d on node %s can't hot-unplug a disk while it runs, because its hotplug setting %q doesn't include disk. "+
				"PVE could only record the delete of slot %s as pending, so we reverted it, and the disk is still attached. "+
				"Add disk to the VM's hotplug setting or stop the VM, and then retry",
			op, pending.VMID, pending.Node, pending.Hotplug, pending.Slot), pending)
	case pve.DriveDeletePendingRevertUnconfirmed:
		return classPendingDelete(cpierrors.Retriable(
			"%s: the delete of slot %s on VM %d (node %s) is pending, and we couldn't confirm that our revert of it took effect (%s). "+
				"The running guest still has the disk and drops it at its next stop unless the pending delete is reverted, "+
				"and the next attempt reverts it again",
			op, pending.Slot, pending.VMID, pending.Node, pending.Error()), pending)
	case pve.DriveDeletePendingFound:
		return classPendingDelete(cpierrors.Retriable(
			"%s: an earlier attempt or an operator left the delete of slot %s on VM %d (node %s) pending, so the running guest still "+
				"has the disk, and nothing was attached to a parker. The transfer finishes once the VM stops and PVE applies the delete",
			op, pending.Slot, pending.VMID, pending.Node), pending)
	default:
		return classPendingDelete(cpierrors.Retriable(
			"%s: the guest on VM %d (node %s) still holds the disk on slot %s, so PVE could only record its delete as pending. "+
				"We reverted that pending delete, and the disk is still attached. A retry can succeed once the guest lets go of the disk",
			op, pending.VMID, pending.Node, pending.Slot), pending)
	}
}

// deleteVMStop says how far delete_vm's stop had got when it detached the VM's
// disks, which decides what the text of a pending delete says.
type deleteVMStop int

const (
	// deleteVMStopIssued is the fast path, whose stop is fire-and-forget, so
	// the VM can still be running when the detach runs.
	deleteVMStopIssued deleteVMStop = iota
	// deleteVMStopAwaited is the synchronous path, which waits for the stop
	// before it detaches anything, so a VM that is running there was started
	// again by something else, such as HA or an operator.
	deleteVMStopAwaited
)

// deleteVMDriveDeletePendingError makes every pending slot delete that reaches
// delete_vm retriable, whatever its reason, and leaves any other error as it
// is. It covers the legacy foreign detach, the foreign-disk transfer, and the
// legacy ephemeral retention transfer. The VM is on its way to stopped on the
// fast path, so the next attempt meets it stopped, and on the synchronous path
// the next attempt stops it again first. Either way nothing was destroyed,
// because the destroy comes after the detach.
func deleteVMDriveDeletePendingError(err error, vmCID string, stop deleteVMStop) error {
	pending, ok := pve.IsDriveDeletePending(err)
	if !ok {
		return err
	}
	if pending.Reason == pve.DriveDeletePendingFound {
		if stop == deleteVMStopAwaited {
			return classPendingDelete(cpierrors.Retriable(
				"delete_vm: refusing to destroy VM %s yet, because an earlier attempt or an operator left the delete of slot %s "+
					"on VM %d pending, and the VM was running again when we got to it, so its guest still has the disk. "+
					"Nothing was destroyed, and the next delete_vm stops the VM and finishes the detach",
				vmCID, pending.Slot, pending.VMID), pending)
		}
		return classPendingDelete(cpierrors.Retriable(
			"delete_vm: refusing to destroy VM %s yet, because an earlier attempt or an operator left the delete of slot %s "+
				"on VM %d pending, and the VM's stop is still in progress, so its guest still has the disk. "+
				"Nothing was destroyed, and the next delete_vm finishes the detach",
			vmCID, pending.Slot, pending.VMID), pending)
	}
	if stop == deleteVMStopAwaited {
		return classPendingDelete(cpierrors.Retriable(
			"delete_vm: refusing to destroy VM %s yet, because the VM was running again when we detached the disk on slot %s, "+
				"so PVE could only record that delete as pending (%s). Something such as HA or an operator started it after our stop. "+
				"Nothing was destroyed, and the next delete_vm stops the VM and finishes the detach",
			vmCID, pending.Slot, pending.Error()), pending)
	}
	return classPendingDelete(cpierrors.Retriable(
		"delete_vm: refusing to destroy VM %s yet, because the VM's stop is still in progress, so PVE could only record the delete "+
			"of slot %s as pending (%s). Nothing was destroyed, and the next delete_vm finishes the detach",
		vmCID, pending.Slot, pending.Error()), pending)
}

// refusePendingDriveReplacement refuses a destroy decision while a disk key on
// the VM carries a pending value naming a different volume than its current
// value. PVE's destroy_vm frees an owned volume that either value names, and
// the guards read one value per key, so they'd judge only one of the two
// drives. A CPI attach can leave the shape when it writes a volume onto a slot
// whose delete is pending on a running VM. The refusal is retriable, because
// PVE applies the change at the VM's next clean stop, or at its next start
// when a crash or a kill left the VM stopped with the change still pending
// (QemuServer.pm:6250, :6281, and :5549). It names both volumes on each slot
// and offers no revert, because a revert keeps the current drive and drops
// the other volume's reference, and only the operator can choose which one
// to keep. It runs only on reads after delete_vm's stop, so that stop gets
// the chance to apply the change.
func refusePendingDriveReplacement(op, vmCID string, holding pve.QemuHolding) error {
	if len(holding.Replaced) == 0 {
		return nil
	}
	keys := make([]string, 0, len(holding.Replaced))
	for key := range holding.Replaced {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	slots := make([]string, 0, len(keys))
	for _, key := range keys {
		slots = append(slots, fmt.Sprintf("%s (%s)", key, strings.Join(holding.Volumes(key), " and ")))
	}
	return cpierrors.Retriable(
		"%s: refusing to destroy VM %s yet, because a pending drive change puts a second volume on %s, and the destroy "+
			"would take a volume either value names. PVE applies the change at the VM's next clean stop, or at its next start "+
			"when the VM is already stopped, and then the next attempt goes ahead. Nothing was destroyed",
		op, vmCID, strings.Join(slots, ", "))
}
