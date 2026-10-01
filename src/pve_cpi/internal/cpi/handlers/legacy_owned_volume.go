package handlers

import (
	"fmt"
	"sort"
	"strings"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// A legacy persistent disk keeps the name create_disk gave it, which carries
// a VMID from the disk band. PVE decides who owns a volume from that name, so
// while no VM has the disk's VMID, no VM owns the disk, and every legacy
// detach and delete_vm leaves it alone. Moving the VM band over VMIDs that
// still name disks breaks that: create_vm can give a VM the disk's VMID, and
// from then on PVE counts the disk as that VM's own. A detach then frees the
// volume through the SDK's unused sweep, and a destroy frees it with the VM.
//
// create_vm leaves out any VMID that names a volume its node can see (see
// pve.WithNodeImageStorageScan), so the shape no longer forms there. The
// refusals below cover what that cannot: a VM that migrated onto a node whose
// local storage names its VMID, and disks an earlier release attached. A
// legacy disk cannot be moved off a VMID without being renamed, and a rename
// strands its CID, so refusing with a clear message is the whole behavior.

// ownedLegacyRemedy is the manual way out every refusal names.
const ownedLegacyRemedy = "To keep the data, reassign the volume by hand to a placeholder VM " +
	"(qm disk move with --target-vmid) and then retry. The disk's CID will not find the renamed volume, " +
	"so recover the data from it by hand. See \"VMID ranges\" in docs/configuration.md in the " +
	"bosh-proxmox-cpi repository"

// refuseOwnedLegacyAttach refuses to attach a legacy disk to the VM its volume
// is named for, which is the earliest point the shape can be stopped. node is
// the node the VM runs on. The remedy depends on the caller. attach_disk meets
// a VM that already exists, and a recreate draws a VMID that names no volume
// its node can see. create_vm has just drawn this VMID itself, so its
// allocator could not see the disk's storage, and a plain retry can draw the
// same VMID again until that changes.
func refuseOwnedLegacyAttach(op string, rd resolvedDisk, node string, vmid int) error {
	if rd.stableID != "" || !pve.VolumeNamedForVM(rd.birth, vmid) {
		return nil
	}
	if strings.HasPrefix(op, "create_vm") {
		return cpierrors.Cloud(
			"%s: refusing to attach disk %s to the new VM %d. Its volume %s is named for VMID %d, so PVE would "+
				"count it as one of the VM's own disks and free it on the next detach or delete. Nothing was "+
				"attached, and create_vm rolls the new VM back, or keeps it tagged when pve.debug.keep_failed_vms is set. "+
				"The VMID allocator could not see this volume when it drew VMID %d, so its storage was not active "+
				"on node %s or is local to another node, and a retry can draw the same VMID again. Make that "+
				"storage active on node %s, or set cloud_properties.target_node to the node whose local storage holds "+
				"the disk, and then retry",
			op, rd.diskCID, vmid, rd.birth, vmid, vmid, node, node,
		)
	}
	return cpierrors.Cloud(
		"%s: refusing to attach disk %s to VM %d. Its volume %s is named for VMID %d, so PVE would count it as "+
			"one of the VM's own disks and free it on the next detach or delete. Let the Director recreate the "+
			"VM, for example with bosh recreate. The new VM draws a VMID that names no volume its node can see, and "+
			"the attach then goes ahead",
		op, rd.diskCID, vmid, rd.birth, vmid,
	)
}

// refuseOwnedLegacyDetach refuses to detach a legacy disk from the VM its
// volume is named for. slot is where the disk sits on that VM, either a bus
// slot or a lingering unusedN entry, and removing either frees the volume.
func refuseOwnedLegacyDetach(vmCID string, vmid int, diskCID, bareDiskCID, slot string) error {
	if !pve.VolumeNamedForVM(bareDiskCID, vmid) {
		return nil
	}
	return cpierrors.Cloud(
		"detach_disk: refusing to detach disk %s from VM %s: its volume %s is named for VMID %d, so PVE counts it "+
			"as one of the VM's own disks and would free the volume, not just detach it. The disk stays on %s. %s",
		diskCID, vmCID, bareDiskCID, vmid, slot, ownedLegacyRemedy,
	)
}

// refuseOwnedLegacyDestroy refuses to destroy a VM that holds a legacy
// persistent disk named for it, because the destroy would free that disk.
// cfg is a config read of the VM itself.
func refuseOwnedLegacyDestroy(cfg map[string]any, vmCID string, vmid int) error {
	owned := pve.FindOwnedLegacyPersistentDisks(cfg, vmid)
	if len(owned) == 0 {
		return nil
	}
	return cpierrors.Cloud(
		"delete_vm: refusing to destroy VM %s: persistent disk %s is named for VMID %d, so PVE counts it as one of "+
			"the VM's own disks and the destroy would free it. %s",
		vmCID, ownedLegacySlots(owned), vmid, ownedLegacyRemedy,
	)
}

// ownedLegacySlots renders slot=volid pairs in slot order.
func ownedLegacySlots(owned map[string]string) string {
	pairs := make([]string, 0, len(owned))
	for slot, volid := range owned {
		pairs = append(pairs, fmt.Sprintf("%s=%s", slot, volid))
	}
	sort.Strings(pairs)
	return strings.Join(pairs, ", ")
}
