package handlers

import (
	"strings"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
)

// managedVMPersistentHandoffPrefix starts the kind of the step a VM allocation
// journals before it hands an independently owned persistent disk to the disk's
// own lifecycle (see attachPersistent). The rest of the kind is the hex SHA-256
// of the disk CID.
const managedVMPersistentHandoffPrefix = "vm.persistent."

// cleanupPersistentHandoffStep accepts the one open step a create_vm leaves on
// its VM record when a persistent disk attach fails in a way the VM allocation
// cannot account for, such as a parker lock wait that ran out after the disk
// lifecycle had already changed something. The step records only the handoff.
// It carries no task, no charge, no volume, and no storage target of its own,
// because the disk's allocation owns every disk mutation and settles them in
// its own record. The caller admits it only through the attested cleanup
// path, which already requires writer fencing and settled remote tasks.
func cleanupPersistentHandoffStep(step aj.Step, record aj.Record) bool {
	if record.Kind != "vm" || step.Attempt != record.ActiveAttempt() || step.State != aj.Planned {
		return false
	}
	if step.UPID != "" || len(step.Charges) != 0 || len(step.VolIDs) != 0 || len(step.Parameters) != 0 {
		return false
	}
	if step.Target.External || step.Target.Node == "" || step.Target.Storage != "" || step.Target.Backing != "" || step.Target.IntendedVolume != "" {
		return false
	}
	if !isPersistentHandoffKind(step.Kind) {
		return false
	}
	vmid, ok := managedVMRootVMID(record)
	return ok && step.Target.VMID == vmid
}

// isPersistentHandoffKind reports whether kind is the handoff prefix followed
// by exactly 64 lowercase hex characters.
func isPersistentHandoffKind(kind string) bool {
	digest, ok := strings.CutPrefix(kind, managedVMPersistentHandoffPrefix)
	if !ok || len(digest) != 64 {
		return false
	}
	for _, c := range digest {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// managedVMRootVMID returns the VMID the record's active attempt created its VM
// under, read from that attempt's single root create or clone step.
func managedVMRootVMID(record aj.Record) (int, bool) {
	vmid := 0
	for index := range record.Steps {
		step := &record.Steps[index]
		if step.Attempt != record.ActiveAttempt() || step.Target.External {
			continue
		}
		if step.Kind != "vm."+managedVMCallCreate && step.Kind != "vm."+managedVMCallClone {
			continue
		}
		if vmid != 0 || step.Target.VMID <= 0 {
			return 0, false
		}
		vmid = step.Target.VMID
	}
	return vmid, vmid > 0
}
