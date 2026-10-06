package handlers

import (
	"testing"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// TestAttestedAdoptSettlesAPlannedHolderWriteThenDeleteVMParksTheDisk runs
// delete_vm on a managed VM 777 that holds the disk without its provenance
// entry while the configuration write attach_disk planned there stays
// planned. delete_vm refuses, because no readback settles that write. The
// attested adopt then settles the write from the readback of 777's slot,
// writing nothing to PVE, and the next delete_vm writes 777's missing entry
// once under the disk's allocation lock, moves the disk to its parker, and
// destroys 777. The parker records the disk under its allocation, and the
// volume stays.
func TestAttestedAdoptSettlesAPlannedHolderWriteThenDeleteVMParksTheDisk(t *testing.T) {
	t.Parallel()
	disk, writes := unrecordedHolderDisk(t)
	vmID := unrecordedHolderManagedVM(t, disk)
	serial := diskSerial(t, disk)
	if slot, _ := slotWithSerial(disk.client.state.configs[777], serial); slot == "" {
		t.Fatalf("777 carries no slot with serial %s: %v", serial, disk.client.state.configs[777])
	}
	planned := plantPlannedHolderWrite(t, disk, nil)

	requireUnsettledStepRefusal(t, "delete_vm before adopt", deleteVM777(t, disk), planned)
	if descriptions, _, _, _, _ := writes.counts(); descriptions != 0 {
		t.Fatalf("the refused delete_vm wrote 777's notes %d times, want never", descriptions)
	}

	record, err := adoptDisk(t, disk, attestedAdopt(disk))
	if err != nil {
		t.Fatalf("attested adopt refused a planned holder write whose disk reads back in 777's slot: %v", err)
	}
	if step := stepByID(t, record, planned); record.State != aj.Adopted || step.State != aj.Observed {
		t.Fatalf("adopt left the record %s with the write %s, want it adopted with the write observed", record.State, step.State)
	}
	if descriptions, _, _, _, _ := writes.counts(); descriptions != 0 {
		t.Fatalf("adopt wrote 777's notes %d times, want never", descriptions)
	}

	if err := deleteVM777(t, disk); err != nil {
		t.Fatalf("delete_vm after the attested adopt failed: %v", err)
	}
	if _, _, heals, underLock, _ := writes.counts(); heals != 1 || underLock != 1 {
		t.Fatalf("the heal wrote 777's entry %d times, %d of them under the allocation lock, want once under it", heals, underLock)
	}
	if cfg := disk.client.state.configs[777]; len(cfg) != 0 {
		t.Fatalf("delete_vm left 777 in place: %v", cfg)
	}
	vm, err := disk.journal.Inspect(vmID)
	if err != nil {
		t.Fatal(err)
	}
	if vm.State != aj.Deleted {
		t.Fatalf("the VM's allocation is %s (reason %q), want %s", vm.State, vm.Reason, aj.Deleted)
	}
	if unsettled, ok := unsettledRecordStep(disk.record(t)); ok {
		t.Fatalf("delete_vm left step %s in the disk's record", unsettledStepName(unsettled))
	}
	parker := disk.client.state.configs[disk.parker]
	_, parked := slotWithSerial(parker, serial)
	if parked == "" {
		t.Fatalf("parker %d doesn't hold the disk: %v", disk.parker, parker)
	}
	entry, found, err := pve.FindDiskAllocationProvenance(pve.DescriptionFromConfig(parker), serial)
	if err != nil || !found || entry.AllocationID != disk.id || entry.Volid != parked {
		t.Fatalf("parker %d records the disk as %+v (found %v, err %v), want allocation %s on %s", disk.parker, entry, found, err, disk.id, parked)
	}
	requireVolumePresent(t, disk, parked)
}
