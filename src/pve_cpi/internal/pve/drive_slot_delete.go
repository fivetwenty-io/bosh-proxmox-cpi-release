package pve

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// DriveDeletePendingReason says why a slot delete stayed pending.
type DriveDeletePendingReason string

const (
	// DriveDeletePendingBusy means the guest still held the device, so the
	// unplug failed after PVE had written the pending delete.
	DriveDeletePendingBusy DriveDeletePendingReason = "busy"
	// DriveDeletePendingHotplug means the VM's hotplug setting lacks disk, so
	// the PUT succeeded with the delete left pending.
	DriveDeletePendingHotplug DriveDeletePendingReason = "hotplug_lacks_disk"
	// DriveDeletePendingRevertUnconfirmed means the revert failed, or the read
	// after it still shows the pending delete.
	DriveDeletePendingRevertUnconfirmed DriveDeletePendingReason = "revert_unconfirmed"
	// DriveDeletePendingFound means a transfer's resume found a pending delete
	// it didn't send, left by an earlier attempt, a crash, or an operator, and
	// left it alone. The resume can't tell whose delete it is or what its
	// caller wants, and a stop of the VM applies it, so it doesn't revert it.
	// The error carries no Cause.
	DriveDeletePendingFound DriveDeletePendingReason = "found_pending"
)

// DriveDeletePendingError is the one error DeleteDriveSlot returns when PVE
// could only record a slot delete as pending. It carries the reason, the VM,
// and the slot, and it doesn't carry a CPI error class, because whether a
// retry can succeed depends on the caller. A detach can't succeed while the VM
// runs with a hotplug setting that lacks disk, while a delete_vm meets the
// same VM stopped on its next attempt. TransferDiskToParker wraps it without
// hiding it, so a caller finds it with errors.As.
//
// It deliberately has no Unwrap. The cause is the PUT or the revert that
// failed, and a classifier that walked into it would answer for the cause
// rather than for the pending delete, such as a retry loop that reads a
// transport fault in the cause as a reason to delete the slot again.
type DriveDeletePendingError struct {
	Reason DriveDeletePendingReason
	Node   string
	VMID   int
	Slot   string
	// Hotplug is the VM's hotplug setting as read from its current config,
	// set for DriveDeletePendingHotplug.
	Hotplug string
	// Cause is the error the delete or the revert returned, if any. It is
	// part of the text only.
	Cause error
}

func (e *DriveDeletePendingError) Error() string {
	var text string
	switch e.Reason {
	case DriveDeletePendingHotplug:
		text = fmt.Sprintf("PVE recorded the delete of slot %s on VM %d (node %s) as pending, because the VM's hotplug setting %q "+
			"doesn't include disk, and we reverted it, so the disk is still attached", e.Slot, e.VMID, e.Node, e.Hotplug)
	case DriveDeletePendingRevertUnconfirmed:
		text = fmt.Sprintf("PVE recorded the delete of slot %s on VM %d (node %s) as pending, and we couldn't confirm that "+
			"our revert of it took effect", e.Slot, e.VMID, e.Node)
	case DriveDeletePendingFound:
		text = fmt.Sprintf("the delete of slot %s on VM %d (node %s) was already pending, left by an earlier attempt or an operator, "+
			"and we left it alone, so the running guest still has the disk", e.Slot, e.VMID, e.Node)
	default:
		text = fmt.Sprintf("the guest on VM %d (node %s) still holds the disk on slot %s, so PVE recorded its delete as pending, "+
			"and we reverted it, so the disk is still attached", e.VMID, e.Node, e.Slot)
	}
	if e.Cause != nil {
		text += " (" + e.Cause.Error() + ")"
	}
	return text
}

// IsDriveDeletePending reports whether err carries a DriveDeletePendingError
// and returns it.
func IsDriveDeletePending(err error) (*DriveDeletePendingError, bool) {
	var pending *DriveDeletePendingError
	if errors.As(err, &pending) {
		return pending, true
	}
	return nil, false
}

// DeleteDriveSlot deletes one bus slot that names bareVolid and proves the VM
// let go of it. On a running VM, qemu-server can only record the delete as
// pending in two cases. When the VM's hotplug setting lacks disk, the PUT
// succeeds with the delete left pending, and when the guest still holds the
// device, the unplug fails after the pending delete is written. Either way the
// guest keeps the disk, and it drops the disk at its next stop while the
// Director still believes the disk is attached.
//
// So after the delete it reads both views. When the slot no longer names the
// volume, it returns nil. When the delete is pending, it reverts it, reads
// again to confirm, and returns a *DriveDeletePendingError with the reason,
// and the caller chooses whether to retry. A busy unplug is retried first, on
// the transient budget maxAttempts selects.
//
// digest guards the first attempt only. A delete that left a pending entry
// changes the config digest, so a retry with the same digest would fail for
// that reason alone. A guard-wrapped managed client supplies its own digest on
// every write.
//
// Every slot delete that can reach a running VM goes through this helper. PVE
// removes an unused entry at once, outside the pending path, and parkers never
// run, so neither needs it.
func DeleteDriveSlot(
	ctx context.Context, c Client, logger *log.Logger,
	node string, vmid int, slot, bareVolid string, digest *string, maxAttempts int,
) error {
	if c == nil || c.Nodes() == nil {
		return cpierrors.Cloud("DeleteDriveSlot: nodes service not available")
	}
	vmidText := strconv.Itoa(vmid)
	first := true
	putErr := RetryOnTransientOrUnplugBusy(ctx, logger, "drive_slot_delete", maxAttempts, func() error {
		del := slot
		params := &sdknodes.UpdateQemuConfigParams{Delete: &del}
		if first {
			params.Digest = digest
			first = false
		}
		return c.Nodes().UpdateQemuConfig(ctx, node, vmidText, params)
	})
	if putErr != nil && IsNotFound(putErr) {
		// The VM is gone, so it holds nothing, and the caller's own not-found
		// handling decides what that means.
		return putErr
	}
	views, readErr := ReadQemuViews(ctx, c, node, vmid)
	if readErr != nil {
		return cpierrors.Retriable("delete of slot %s on VM %d (node %s) could not be confirmed, because its pending view could not be read: %s",
			slot, vmid, node, joinErrorText(putErr, WrapConfigReadError(readErr)))
	}
	if !slices.Contains(views.SlotsNaming(bareVolid), slot) {
		// The slot no longer names the volume in either view, so the VM let go,
		// whatever the PUT reported on its way there.
		return nil
	}
	if !views.PendingDelete(slot) {
		if putErr != nil {
			return cpierrors.Wrap(WrapMutationError(putErr),
				fmt.Sprintf("delete slot %s of %q on VM %d (node %s)", slot, bareVolid, vmid, node))
		}
		return cpierrors.Retriable("slot %s on VM %d (node %s) still names %q after its delete; retry", slot, vmid, node, bareVolid)
	}
	return revertPendingDriveDelete(ctx, c, node, vmid, slot, views, putErr)
}

// revertPendingDriveDelete reverts our own pending delete of slot, confirms it
// with a second pending read, and returns the typed error that says why the
// delete stayed pending.
//
// With disk in the hotplug setting, qemu-server leaves a drive delete pending
// only when the unplug fails, so the reason is the hotplug setting when it
// lacks disk and a busy guest otherwise, whatever the PUT itself returned.
func revertPendingDriveDelete(ctx context.Context, c Client, node string, vmid int, slot string, views QemuViews, putErr error) error {
	if err := RevertPendingDriveDelete(ctx, c, node, vmid, slot); err != nil {
		return err
	}
	pending := &DriveDeletePendingError{Node: node, VMID: vmid, Slot: slot}
	if setting, lacksDisk := hotplugLacksDisk(views.Current()); lacksDisk {
		pending.Reason = DriveDeletePendingHotplug
		pending.Hotplug = setting
		return pending
	}
	pending.Reason = DriveDeletePendingBusy
	pending.Cause = putErr
	return pending
}

// RevertPendingDriveDelete reverts a pending delete of slot and confirms with a
// pending read that it's gone. It returns nil once the read shows no pending
// delete, and a *DriveDeletePendingError with DriveDeletePendingRevertUnconfirmed
// when the revert fails, the read fails, or the read still shows the delete.
func RevertPendingDriveDelete(ctx context.Context, c Client, node string, vmid int, slot string) error {
	if c == nil || c.Nodes() == nil {
		return cpierrors.Cloud("RevertPendingDriveDelete: nodes service not available")
	}
	unconfirmed := &DriveDeletePendingError{Reason: DriveDeletePendingRevertUnconfirmed, Node: node, VMID: vmid, Slot: slot}
	revert := slot
	if err := c.Nodes().UpdateQemuConfig(ctx, node, strconv.Itoa(vmid), &sdknodes.UpdateQemuConfigParams{Revert: &revert}); err != nil {
		unconfirmed.Cause = err
		return unconfirmed
	}
	after, err := ReadQemuViews(ctx, c, node, vmid)
	if err != nil {
		unconfirmed.Cause = err
		return unconfirmed
	}
	if after.PendingDelete(slot) {
		return unconfirmed
	}
	return nil
}

// hotplugLacksDisk reads a VM's hotplug setting the way qemu-server's
// parse_hotplug_features does. An absent key or "1" means the default, which
// includes disk, "0" means nothing, and anything else is a comma list.
func hotplugLacksDisk(cfg map[string]any) (string, bool) {
	value, ok := ConfigString(cfg, "hotplug")
	if !ok {
		return "network,disk,usb", false
	}
	switch strings.TrimSpace(value) {
	case "1":
		return value, false
	case "0", "":
		return value, true
	}
	for _, feature := range strings.Split(value, ",") {
		if strings.TrimSpace(feature) == "disk" {
			return value, false
		}
	}
	return value, true
}

// joinErrorText joins the text of every non-nil error with a semicolon.
func joinErrorText(errs ...error) string {
	var parts []string
	for _, err := range errs {
		if err != nil {
			parts = append(parts, err.Error())
		}
	}
	return strings.Join(parts, "; ")
}

// DetachDriveSlot is DeleteDriveSlot followed by the second step the SDK's
// DetachDisk takes, which removes any unused entry of the same VM that names
// bareVolid. PVE adds such an entry only for a volume the VM owns, and it
// removes an unused key at once rather than recording it as pending.
func DetachDriveSlot(ctx context.Context, c Client, logger *log.Logger, node string, vmid int, slot, bareVolid string, maxAttempts int) error {
	if err := DeleteDriveSlot(ctx, c, logger, node, vmid, slot, bareVolid, nil, maxAttempts); err != nil {
		return err
	}
	views, err := ReadQemuViews(ctx, c, node, vmid)
	if err != nil {
		return cpierrors.Wrap(WrapConfigReadError(err), fmt.Sprintf("re-read VM %d after detaching slot %s", vmid, slot))
	}
	for _, key := range views.SlotsNaming(bareVolid) {
		if !strings.HasPrefix(key, "unused") {
			continue
		}
		del := key
		if err := c.Nodes().UpdateQemuConfig(ctx, node, strconv.Itoa(vmid), &sdknodes.UpdateQemuConfigParams{Delete: &del}); err != nil {
			return cpierrors.Wrap(WrapMutationError(err), fmt.Sprintf("remove unused entry %s of %q on VM %d", key, bareVolid, vmid))
		}
	}
	return nil
}
