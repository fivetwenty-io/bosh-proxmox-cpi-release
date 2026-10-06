package handlers

// These tests start from an attested adopt that settled a stopped attach's
// configuration write, saved the record in reconciliation_required, and then
// stopped before it saved the adoption. A refused adoption save leaves the
// journal exactly as a process that stopped between the two saves does, with
// the write observed and VM 777 still holding the disk without its provenance
// entry. A rerun of the attested adopt repeats the readback and finishes the
// adoption. A rerun without both attestations, and an attested adopt of a
// record that reached reconciliation_required any other way, keep the
// ownership check that refuses a holder without the entry, and the CLI prints
// that refusal's own text.

import (
	"encoding/json"
	"errors"
	"maps"
	"reflect"
	"strings"
	"testing"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
)

// halfAdoptedAttach returns a stopped attach whose attested adopt saved the
// settlement and then could not save the adoption, its holderWrites, and the
// settled step's ID.
func halfAdoptedAttach(t *testing.T) (*parkedFlowDisk, *holderWrites, string) {
	t.Helper()
	disk, writes, planned := stoppedAttach(t)
	refused := errors.New("disk full")
	ctx := aj.WithSaveFaultForTest(t.Context(), func(record aj.Record) error {
		if record.State == aj.Adopted {
			return refused
		}
		return nil
	})
	if _, err := ApplyStorageAllocationDecision(ctx, disk.deps, disk.journal, []string{"n1"}, attestedAdopt(disk)); !errors.Is(err, refused) {
		t.Fatalf("adopt err = %v, want the refused adoption save", err)
	}
	record := disk.record(t)
	if record.State != aj.ReconciliationRequired || !strings.HasPrefix(record.Reason, "lifecycle attach_disk stopped; adopt settled") || stepByID(t, record, planned).State != aj.Observed {
		t.Fatalf("the half-saved adopt left the record %s (reason %q) with the write %s, want reconciliation_required with the write observed", record.State, record.Reason, stepByID(t, record, planned).State)
	}
	if held := holderProvenanceEntries(t, disk); len(held) != 0 {
		t.Fatalf("777 records the disk although adopt writes nothing to PVE: %v", held)
	}
	return disk, writes, planned
}

// requireOwnershipRefusal runs adopt with decision and fails unless it
// refuses at the ownership check for 777's missing entry, the CLI prints that
// refusal's text, and the record and 777 stay as they were.
func requireOwnershipRefusal(t *testing.T, disk *parkedFlowDisk, decision StorageAllocationDecision) {
	t.Helper()
	before := disk.record(t)
	holder := maps.Clone(disk.client.state.configs[777])
	_, err := adoptDisk(t, disk, decision)
	if err == nil {
		t.Fatal("adopt accepted VM 777 without the disk's provenance entry")
	}
	failure := StorageAllocationDecisionFailure(err)
	for _, text := range []string{
		"identity_or_audit_evidence: managed disk " + disk.cid + " is attached to VM 777 as ",
		" without its provenance entry, and storage-journal doesn't write that entry; the next attach_disk, detach_disk, or delete_vm on VM 777 writes it under the disk's allocation lock and heals the disk's record",
	} {
		if !strings.Contains(failure, text) {
			t.Fatalf("CLI failure = %q, want it to contain %q", failure, text)
		}
	}
	for _, text := range []string{"unclassified error", "retry the operation"} {
		if strings.Contains(failure, text) {
			t.Fatalf("CLI failure = %q, want no %q in it", failure, text)
		}
	}
	if got := disk.record(t); !reflect.DeepEqual(got, before) {
		t.Fatalf("the refused adopt changed the record to %+v, want %+v", got, before)
	}
	if got := disk.client.state.configs[777]; !reflect.DeepEqual(got, holder) {
		t.Fatalf("the refused adopt changed 777 to %v, want %v", got, holder)
	}
}

// TestRerunAttestedAdoptFinishesAHalfSavedAdopt reruns the attested adopt
// after it stopped between its two saves. The rerun reads 777 back again,
// accepts it without the disk's entry on the same proofs, and adopts the
// record with that readback in its evidence, writing nothing to PVE. The next
// delete_vm then writes 777's entry under the disk's allocation lock, parks
// the disk, and destroys 777, and the volume stays.
func TestRerunAttestedAdoptFinishesAHalfSavedAdopt(t *testing.T) {
	t.Parallel()
	disk, writes, planned := halfAdoptedAttach(t)
	vmID := unrecordedHolderManagedVM(t, disk)
	serial := diskSerial(t, disk)
	slot, volume := slotWithSerial(disk.client.state.configs[777], serial)
	before := map[int]map[string]any{}
	for vmid, cfg := range disk.client.state.configs {
		before[vmid] = maps.Clone(cfg)
	}

	record, err := adoptDisk(t, disk, attestedAdopt(disk))
	if err != nil {
		t.Fatalf("the rerun attested adopt refused the half-saved record: %v (CLI: %s)", err, StorageAllocationDecisionFailure(err))
	}
	if record.State != aj.Adopted || record.Reason != "" {
		t.Fatalf("the rerun left the record %s (reason %q), want it adopted", record.State, record.Reason)
	}
	if step := stepByID(t, record, planned); step.State != aj.Observed || !containsString(step.VolIDs, volume) {
		t.Fatalf("the settled write = %+v, want it observed with volume %s", step, volume)
	}
	var evidence struct {
		Settled []adoptSettledConfigStep `json:"settled_configuration_steps"`
	}
	if err := json.Unmarshal([]byte(record.Verifications[len(record.Verifications)-1].EvidenceJSON), &evidence); err != nil {
		t.Fatalf("adopt's evidence doesn't parse: %v", err)
	}
	want := []adoptSettledConfigStep{{StepID: planned, Kind: "lifecycle_attach_disk_Nodes_UpdateQemuConfig", Node: "n1", VMID: 777, Slot: slot, Volume: volume, Serial: serial}}
	if !reflect.DeepEqual(evidence.Settled, want) {
		t.Fatalf("the rerun's evidence = %+v, want its readback %+v", evidence.Settled, want)
	}
	if descriptions, _, _, _, _ := writes.counts(); descriptions != 0 {
		t.Fatalf("adopt wrote 777's notes %d times, want never", descriptions)
	}
	if !reflect.DeepEqual(disk.client.state.configs, before) {
		t.Fatalf("adopt changed VM configurations to %v, want %v", disk.client.state.configs, before)
	}

	if err := deleteVM777(t, disk); err != nil {
		t.Fatalf("delete_vm after the rerun adopt failed: %v", err)
	}
	requireHealedOnceUnderLock(t, writes)
	vm, err := disk.journal.Inspect(vmID)
	if err != nil {
		t.Fatal(err)
	}
	if vm.State != aj.Deleted || len(disk.client.state.configs[777]) != 0 {
		t.Fatalf("delete_vm left the VM's allocation %s and 777 as %v, want it deleted and 777 gone", vm.State, disk.client.state.configs[777])
	}
	requireParked(t, disk)
}

// TestRerunAdoptWithoutBothAttestationsRefusesAHalfSavedAdopt reruns adopt
// on the half-saved record with one attestation or none. It keeps the
// ownership check, which refuses 777 without the disk's entry, and the CLI
// prints that refusal instead of an unclassified error.
func TestRerunAdoptWithoutBothAttestationsRefusesAHalfSavedAdopt(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name          string
		fenced, tasks bool
	}{
		{name: "neither"},
		{name: "fenced only", fenced: true},
		{name: "tasks settled only", tasks: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			disk, _, _ := halfAdoptedAttach(t)
			decision := adoptDecision(disk)
			decision.PreviousWriterFenced = tc.fenced
			decision.RemoteTasksSettled = tc.tasks
			requireOwnershipRefusal(t, disk, decision)
		})
	}
}

// TestAttestedAdoptKeepsTheOwnershipCheckForAnyOtherReason runs the
// attested adopt on the half-saved record after its reason is replaced, so
// no settlement of adopt explains it. The ownership check refuses 777 without
// the disk's entry, as it did before, and the CLI prints that refusal.
func TestAttestedAdoptKeepsTheOwnershipCheckForAnyOtherReason(t *testing.T) {
	t.Parallel()
	for _, reason := range []string{
		"lifecycle attach_disk failed; reconcile the disk before the next call",
		"lifecycle attach_disk stopped; adopt settled its configuration write from a readback under attestation",
	} {
		t.Run(reason, func(t *testing.T) {
			t.Parallel()
			disk, _, _ := halfAdoptedAttach(t)
			record := disk.record(t)
			record.Reason = reason
			rewriteJournalRecord(t, disk.deps, disk.id, record)
			requireOwnershipRefusal(t, disk, attestedAdopt(disk))
		})
	}
}

// TestRerunAttestedAdoptRefusesWhenItsReadbackFails reruns the attested
// adopt on the half-saved record after its settled write is changed to
// target VM 778, where the disk isn't. The repeated readback fails, so adopt
// refuses with the readback's clause and leaves the record as it was.
func TestRerunAttestedAdoptRefusesWhenItsReadbackFails(t *testing.T) {
	t.Parallel()
	disk, _, planned := halfAdoptedAttach(t)
	record := disk.record(t)
	for i := range record.Steps {
		if record.Steps[i].ID == planned {
			record.Steps[i].Target.VMID = 778
		}
	}
	rewriteJournalRecord(t, disk.deps, disk.id, record)
	before := disk.record(t)
	_, err := adoptDisk(t, disk, attestedAdopt(disk))
	failure := StorageAllocationDecisionFailure(err)
	for _, text := range []string{
		"identity_or_audit_evidence: an earlier attested adopt settled step " + planned + " (lifecycle_attach_disk_Nodes_UpdateQemuConfig) of the stopped attach_disk",
		"its readback of step " + planned + " failed; adopt read the disk back and found it on VM 777 on node n1, where the step targets VM 778 on node n1",
	} {
		if !strings.Contains(failure, text) {
			t.Fatalf("CLI failure = %q, want it to contain %q", failure, text)
		}
	}
	if got := disk.record(t); !reflect.DeepEqual(got, before) {
		t.Fatalf("the refused adopt changed the record to %+v, want %+v", got, before)
	}
}

// TestAdoptSettlesOnlyTheWriteOfTheCallThatStopped runs the attested adopt on
// a stopped attach whose record names resize_disk as the call that stopped.
// The planned write is attach_disk's, so the stopped call didn't plan it, and
// adopt settles nothing and refuses with the record's condition before it
// saves anything.
func TestAdoptSettlesOnlyTheWriteOfTheCallThatStopped(t *testing.T) {
	t.Parallel()
	disk, _, planned := stoppedAttach(t)
	record := disk.record(t)
	record.Reason = "lifecycle resize_disk admitted; completion pending"
	rewriteJournalRecord(t, disk.deps, disk.id, record)
	requireAdoptLeavesStep(t, disk, attestedAdopt(disk), planned, "is planned",
		"adopt settles this write only on a disk record with the exact CID that is in reconciliation_required, or that the disk call which planned the write left planned or observed when it stopped")
}

// TestAdoptStalledReasonReadsBack checks that adopt reads the operation and
// the settled steps back from every reason it writes, and from nothing else,
// and that the write each operation plans on a disk's holder is one adopt
// settles.
func TestAdoptStalledReasonReadsBack(t *testing.T) {
	t.Parallel()
	for _, stalled := range []string{"attach_disk", "detach_disk", "delete_disk", "delete_vm.preserve_disk"} {
		if !adoptSettledKinds[adoptStalledKind(stalled)] {
			t.Fatalf("adopt doesn't settle %s, the write %s plans on a disk's holder", adoptStalledKind(stalled), stalled)
		}
		for _, steps := range [][]string{
			{"attempt-0-step-18"},
			{"attempt-0-step-18", "attempt-0-step-19"},
			{"attempt-1-step-3", "attempt-1-step-4", "attempt-1-step-5"},
		} {
			reason := adoptStalledReason(stalled, steps)
			operation, got, ok := adoptStalledSettlement(reason)
			if !ok || operation != stalled || !reflect.DeepEqual(got, steps) {
				t.Fatalf("adoptStalledSettlement(%q) = %q, %v, %v, want %s, %v", reason, operation, got, ok, stalled, steps)
			}
		}
	}
	for _, reason := range []string{
		"",
		"lifecycle attach_disk admitted; completion pending",
		"lifecycle attach_disk stopped; adopt settled its configuration write from a readback under attestation",
		"lifecycle attach_disk stopped; adopt settled its configuration writes attempt-0-step-18 from a readback under attestation",
		"lifecycle attach_disk stopped; adopt settled its configuration write attempt-0-step-18 and attempt-0-step-19 from a readback under attestation",
		"lifecycle attach_disk stopped; adopt settled its configuration write attempt-0-step-18 from a readback",
		"lifecycle attach disk stopped; adopt settled its configuration write attempt-0-step-18 from a readback under attestation",
	} {
		if operation, steps, ok := adoptStalledSettlement(reason); ok {
			t.Fatalf("adoptStalledSettlement(%q) = %q, %v, want no settlement", reason, operation, steps)
		}
	}
}
