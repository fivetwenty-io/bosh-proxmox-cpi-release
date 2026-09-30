package handlers

import (
	"strings"
	"testing"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
)

// TestCleanupNamesALockStepItCouldNotSettle runs explicit cleanup on a record
// whose planned lock step cannot be settled, because PVE does not answer the
// sentinel read exactly. Cleanup still refuses at the same stage with the same
// head, and the refusal now goes on to name the step and say why its read
// failed, for an attested decision and for a plain one alike.
func TestCleanupNamesALockStepItCouldNotSettle(t *testing.T) {
	for _, attested := range []bool{true, false} {
		name := map[bool]string{true: "attested decision", false: "plain decision"}[attested]
		t.Run(name, func(t *testing.T) {
			disk, locks, planned := lockBugRecord(t)
			locks.readErr = poolVerdictError("permission check failed for /pool/bosh-lock-vm-90000 (Pool.Audit)")
			decision := StorageAllocationDecision{Action: "cleanup", AllocationID: disk.id, DecisionID: "unsettled-lock-step"}
			if attested {
				decision = cleanupAttestedDecision(disk.id)
			}
			_, err := CleanupStorageAllocation(t.Context(), disk.deps, disk.journal, []string{"n1"}, decision)
			if err == nil {
				t.Fatal("cleanup accepted a record whose lock step could not be settled")
			}
			text := StorageAllocationDecisionFailure(err)
			for _, want := range []string{
				"cleanup_pending_mutation_settlement: cleanup refuses unresolved allocation or asynchronous mutation",
				"step " + planned.ID + " (lifecycle_attach_disk_Pool_CreatePool) is planned",
				"its lock sentinel could not be settled because PVE did not answer exactly for sentinel bosh-lock-vm-",
			} {
				if !strings.Contains(text, want) {
					t.Fatalf("the cleanup refusal does not say %q: %s", want, text)
				}
			}
			record := disk.record(t)
			if step := stepByID(t, record, planned.ID); step.State != aj.Planned || record.State != aj.ReconciliationRequired {
				t.Fatalf("a refused cleanup changed the record: %s %s", record.State, step.State)
			}
		})
	}
}
