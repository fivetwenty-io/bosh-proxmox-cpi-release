package handlers

import (
	"errors"
	"strings"
	"testing"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
)

// crashedLockStepDisk builds a disk record whose request stopped right after
// it planned a lifecycle sentinel create. The step names the VM the disk was
// going to, and no outcome was ever written, so nothing moved the record out
// of planned. The deps answer every sentinel read exactly.
func crashedLockStepDisk(t *testing.T) (Deps, *lifecycleFlowPVE, *aj.Journal, string, string) {
	t.Helper()
	var step string
	deps, client, journal, id, _ := lifecycleFlowFixtureWith(t, false, false, func(h *aj.Handle) {
		birth := h.Record().Steps[0].Target
		var err error
		step, err = storageMutationIntent(h, "lifecycle_attach_disk_Pool_CreatePool", aj.Target{Node: birth.Node, Storage: birth.Storage, Backing: birth.Backing, VMID: 777, IntendedVolume: birth.IntendedVolume}, nil)
		if err != nil {
			t.Fatal(err)
		}
	})
	// create_disk never returned this disk, so no VM holds it.
	delete(client.state.configs[777], "scsi1")
	deps.PVE = contendedFlowPVE{lifecycleFlowPVE: client, locks: newLockContention(t)}
	record, err := journal.Inspect(id)
	if err != nil {
		t.Fatal(err)
	}
	if record.State != aj.Planned || stepByID(t, record, step).State != aj.Planned {
		t.Fatalf("the crashed record is %s with its lock step %s", record.State, stepByID(t, record, step).State)
	}
	return deps, client, journal, id, step
}

// TestCrashedLockStepSettlesOutsideReconciliation settles the lock step of a
// record that a crash left in planned. Settlement records the step the way
// the guard records one and leaves the record's state alone.
func TestCrashedLockStepSettlesOutsideReconciliation(t *testing.T) {
	deps, _, journal, id, step := crashedLockStepDisk(t)
	handle, err := journal.Acquire(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := handle.Close(); err != nil {
			t.Error(err)
		}
	}()
	if _, err := settlePlannedLockSteps(t.Context(), deps, handle); err != nil {
		t.Fatalf("settlement failed on the crashed record: %v", err)
	}
	record := handle.Record()
	settled := stepByID(t, record, step)
	if settled.State != aj.Observed || len(settled.VolIDs) != 1 || settled.VolIDs[0] != settled.Target.IntendedVolume {
		t.Fatalf("the lock step was not settled the way the guard settles one: %+v", settled)
	}
	if record.State != aj.Planned {
		t.Fatalf("settlement moved the record to %s", record.State)
	}
}

// TestCrashedLockStepRecordCleansUp runs the operator's cleanup on the same
// record. Cleanup gets past settlement and removes the disk's volume.
func TestCrashedLockStepRecordCleansUp(t *testing.T) {
	deps, client, journal, id, step := crashedLockStepDisk(t)
	result, err := CleanupStorageAllocation(t.Context(), deps, journal, []string{"n1"}, StorageAllocationDecision{Action: "cleanup", AllocationID: id, DecisionID: "crashed-lock-step"})
	if err != nil {
		t.Fatalf("cleanup refused the crashed record: %v", err)
	}
	if result.State != aj.Cleaned || client.deletes != 1 {
		t.Fatalf("cleanup left %s with %d volume deletes", result.State, client.deletes)
	}
	if settled := stepByID(t, result, step); settled.State != aj.Observed {
		t.Fatalf("cleanup left the lock step %s", settled.State)
	}
}

// TestRefusedSettlementNamesItsSteps makes the journal refuse the settlement
// write, here because the handle has already closed. The error names the step
// it was settling and keeps the journal's own error in the chain.
func TestRefusedSettlementNamesItsSteps(t *testing.T) {
	deps, _, journal, id, step := crashedLockStepDisk(t)
	handle, err := journal.Acquire(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = settlePlannedLockSteps(t.Context(), deps, handle)
	if !errors.Is(err, aj.ErrClosed) || !strings.Contains(err.Error(), "step "+step+" (lifecycle_attach_disk_Pool_CreatePool)") {
		t.Fatalf("the refused settlement did not name its step: %v", err)
	}
}
