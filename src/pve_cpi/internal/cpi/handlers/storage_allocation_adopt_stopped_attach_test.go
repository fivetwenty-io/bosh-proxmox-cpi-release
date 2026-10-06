package handlers

// These tests build the state an attach_disk leaves when its process stops
// as the write of the holder's provenance entry reaches PVE, before PVE
// applies it. The journal and every VM configuration are captured at that
// moment and restored after the attach returns, so the disk's record stays
// planned under the attach's admission, with the attach's planned
// configuration write on VM 777, and 777 holds the disk without the entry.
// The attested adopt settles that write from a readback, moves the record to
// reconciliation_required in the same save, and adopts it. The next
// attach_disk, detach_disk, or delete_vm then writes 777's entry under the
// disk's allocation lock and goes on.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"strings"
	"testing"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

// stoppedAttach returns a parked disk whose attach_disk to VM 777 stopped as
// its write of 777's provenance entry reached PVE, the planned step of that
// write, and the holderWrites its cluster reports to from then on. It fails
// unless the record is planned under the attach's admission and that write
// is its only unsettled step, and 777 holds the disk without the entry.
func stoppedAttach(t *testing.T) (*parkedFlowDisk, *holderWrites, string) {
	t.Helper()
	locks := newLockContention(t)
	disk := newParkedFlowDisk(t, locks)
	locks.reset()
	flow := disk.deps.PVE.(contendedFlowPVE)
	var stopped aj.Record
	var configs map[int]map[string]any
	crash := &holderWrites{journal: disk.journal, id: disk.id, faults: 1}
	crash.change = func() {
		record, err := disk.journal.Inspect(disk.id)
		if err != nil {
			t.Fatalf("the disk's record can't be read as the provenance write arrives: %v", err)
		}
		stopped = record
		configs = map[int]map[string]any{}
		for vmid, cfg := range disk.client.state.configs {
			configs[vmid] = maps.Clone(cfg)
		}
	}
	disk.deps.PVE = holderWritesPVE{contendedFlowPVE: flow, writes: crash}
	_ = disk.attach(noBackoff(t.Context()))
	if configs == nil {
		t.Fatal("attach_disk never sent the write of 777's provenance entry")
	}
	for vmid := range disk.client.state.configs {
		if _, ok := configs[vmid]; !ok {
			delete(disk.client.state.configs, vmid)
		}
	}
	maps.Copy(disk.client.state.configs, configs)
	rewriteJournalRecord(t, disk.deps, disk.id, stopped)
	writes := &holderWrites{journal: disk.journal, id: disk.id}
	disk.deps.PVE = holderWritesPVE{contendedFlowPVE: flow, writes: writes}

	record := disk.record(t)
	if record.State != aj.Planned || record.Reason != "lifecycle attach_disk admitted; completion pending" {
		t.Fatalf("the stopped attach left the record %s (reason %q), want it planned under the attach's admission", record.State, record.Reason)
	}
	var planned []aj.Step
	for i := range record.Steps {
		if record.Steps[i].State != aj.Observed && !storageDecisionClosedAttemptStepSettled(record, record.Steps[i]) {
			planned = append(planned, record.Steps[i])
		}
	}
	if len(planned) != 1 || planned[0].State != aj.Planned || planned[0].Kind != "lifecycle_attach_disk_Nodes_UpdateQemuConfig" || planned[0].Target.VMID != 777 {
		t.Fatalf("the stopped attach left unsettled steps %+v, want one planned attach_disk configuration write on 777", planned)
	}
	if slot, _ := slotWithSerial(disk.client.state.configs[777], diskSerial(t, disk)); slot == "" {
		t.Fatalf("777 doesn't hold the disk after the stopped attach: %v", disk.client.state.configs[777])
	}
	if held := holderProvenanceEntries(t, disk); len(held) != 0 {
		t.Fatalf("777 records the disk although its provenance write never applied: %v", held)
	}
	return disk, writes, planned[0].ID
}

// requireStoppedAttachAdopted runs the attested adopt on a stopped attach and
// fails unless it adopts the record with the write observed on the volume
// 777's slot names, keeps the settled step in its evidence, and changes
// nothing on PVE. It returns the volume.
func requireStoppedAttachAdopted(t *testing.T, disk *parkedFlowDisk, writes *holderWrites, planned string) string {
	t.Helper()
	serial := diskSerial(t, disk)
	slot, volume := slotWithSerial(disk.client.state.configs[777], serial)
	before := map[int]map[string]any{}
	for vmid, cfg := range disk.client.state.configs {
		before[vmid] = maps.Clone(cfg)
	}
	record, err := adoptDisk(t, disk, attestedAdopt(disk))
	if err != nil {
		t.Fatalf("attested adopt refused the write a stopped attach left planned: %v", err)
	}
	if record.State != aj.Adopted || record.Reason != "" {
		t.Fatalf("adopt left the record %s (reason %q), want it adopted", record.State, record.Reason)
	}
	step := stepByID(t, record, planned)
	if step.State != aj.Observed || !containsString(step.VolIDs, volume) {
		t.Fatalf("the stopped attach's write = %+v, want it observed with volume %s", step, volume)
	}
	if unsettled, ok := unsettledRecordStep(record); ok {
		t.Fatalf("adopted record still holds unsettled step %s", unsettledStepName(unsettled))
	}
	var evidence struct {
		Settled []adoptSettledConfigStep `json:"settled_configuration_steps"`
	}
	if err := json.Unmarshal([]byte(record.Verifications[len(record.Verifications)-1].EvidenceJSON), &evidence); err != nil {
		t.Fatalf("adopt's evidence doesn't parse: %v", err)
	}
	want := []adoptSettledConfigStep{{StepID: planned, Kind: step.Kind, Node: "n1", VMID: 777, Slot: slot, Volume: volume, Serial: serial}}
	if !reflect.DeepEqual(evidence.Settled, want) {
		t.Fatalf("adopt's evidence settled %+v, want %+v", evidence.Settled, want)
	}
	if descriptions, _, _, _, _ := writes.counts(); descriptions != 0 {
		t.Fatalf("adopt wrote 777's notes %d times, want never", descriptions)
	}
	if !reflect.DeepEqual(disk.client.state.configs, before) {
		t.Fatalf("adopt changed VM configurations to %v, want %v", disk.client.state.configs, before)
	}
	requireVolumePresent(t, disk, volume)
	return volume
}

// requireParked fails unless 777 no longer holds the disk and the parker
// holds it under the disk's allocation, with the volume still present.
func requireParked(t *testing.T, disk *parkedFlowDisk) {
	t.Helper()
	serial := diskSerial(t, disk)
	if slot, _ := slotWithSerial(disk.client.state.configs[777], serial); slot != "" {
		t.Fatalf("777 still holds the disk in %s", slot)
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

// requireHealedOnceUnderLock fails unless the heal wrote 777's entry exactly
// once, under the disk's allocation lock.
func requireHealedOnceUnderLock(t *testing.T, writes *holderWrites) {
	t.Helper()
	if _, _, heals, underLock, _ := writes.counts(); heals != 1 || underLock != 1 {
		t.Fatalf("the heal wrote 777's entry %d times, %d of them under the allocation lock, want once under it", heals, underLock)
	}
}

// TestAttestedAdoptSettlesAStoppedAttachThenDeleteVMParksTheDisk runs
// delete_vm on a managed VM 777 that an attach_disk stopped on. delete_vm
// refuses and keeps the disk. The attested adopt then settles the attach's
// write and adopts the record, and the next delete_vm writes 777's entry
// under the disk's allocation lock, moves the disk to its parker, and
// destroys 777. The volume stays.
func TestAttestedAdoptSettlesAStoppedAttachThenDeleteVMParksTheDisk(t *testing.T) {
	t.Parallel()
	disk, writes, planned := stoppedAttach(t)
	vmID := unrecordedHolderManagedVM(t, disk)

	if err := deleteVM777(t, disk); err == nil {
		t.Fatal("delete_vm accepted 777 while the stopped attach's write is planned")
	}
	if slot, _ := slotWithSerial(disk.client.state.configs[777], diskSerial(t, disk)); slot == "" || disk.client.deletes != 0 {
		t.Fatalf("the refused delete_vm moved or destroyed the disk (777 %v, %d deletes)", disk.client.state.configs[777], disk.client.deletes)
	}

	requireStoppedAttachAdopted(t, disk, writes, planned)

	if err := deleteVM777(t, disk); err != nil {
		t.Fatalf("delete_vm after the attested adopt failed: %v", err)
	}
	requireHealedOnceUnderLock(t, writes)
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
	requireParked(t, disk)
}

// TestAttestedAdoptSettlesAStoppedAttachThenAttachCompletes settles a stopped
// attach with the attested adopt and runs the Director's attach_disk again,
// which writes 777's entry once under the disk's allocation lock and
// completes.
func TestAttestedAdoptSettlesAStoppedAttachThenAttachCompletes(t *testing.T) {
	t.Parallel()
	disk, writes, planned := stoppedAttach(t)
	volume := requireStoppedAttachAdopted(t, disk, writes, planned)

	if err := disk.attach(t.Context()); err != nil {
		t.Fatalf("attach_disk after the attested adopt failed: %v", err)
	}
	requireHealedOnceUnderLock(t, writes)
	if held := holderProvenanceEntries(t, disk); len(held) != 1 || held[0].Volid != volume {
		t.Fatalf("777 records the disk as %+v, want one entry for %s", held, volume)
	}
	if unsettled, ok := unsettledRecordStep(disk.record(t)); ok {
		t.Fatalf("attach_disk left step %s", unsettledStepName(unsettled))
	}
	requireVolumePresent(t, disk, volume)
}

// TestAttestedAdoptSettlesAStoppedAttachThenDetachParksTheDisk settles a
// stopped attach with the attested adopt and runs detach_disk, which writes
// 777's entry once under the disk's allocation lock and parks the disk.
func TestAttestedAdoptSettlesAStoppedAttachThenDetachParksTheDisk(t *testing.T) {
	t.Parallel()
	disk, writes, planned := stoppedAttach(t)
	requireStoppedAttachAdopted(t, disk, writes, planned)

	args := []json.RawMessage{json.RawMessage(`"777"`), json.RawMessage(fmt.Sprintf("%q", disk.cid))}
	if _, err := HandleDetachDisk(disk.deps).Handle(t.Context(), args, jsonrpc.Context{}); err != nil {
		t.Fatalf("detach_disk after the attested adopt failed: %v", err)
	}
	requireHealedOnceUnderLock(t, writes)
	requireParked(t, disk)
}

// ownershipReadFailsPVE is a cluster whose storage content read fails with a
// 500 when it comes from adopt's ownership check, which runs with the
// settled holder in its context, and answers as usual otherwise.
type ownershipReadFailsPVE struct{ holderWritesPVE }

func (c ownershipReadFailsPVE) Nodes() nodes.Service {
	return ownershipReadFailsNodes{Service: c.holderWritesPVE.Nodes()}
}

type ownershipReadFailsNodes struct{ nodes.Service }

func (n ownershipReadFailsNodes) GetStorageContent(ctx context.Context, node, storage, volume string) (*nodes.GetStorageContentResponse, error) {
	if ctx.Value(settledHolderKey{}) != nil {
		return nil, &sdkerrors.APIError{HTTPCode: 500, Message: "storage busy"}
	}
	return n.Service.GetStorageContent(ctx, node, storage, volume)
}

// adoptFixture builds a disk with a planned holder write on 777 for adopt to
// settle, and returns the disk, its holderWrites, and the step's ID.
type adoptFixture struct {
	name  string
	build func(t *testing.T) (*parkedFlowDisk, *holderWrites, string)
}

// adoptFixtures are the stopped attach, and an unrecorded holder whose
// record a disk call left in reconciliation_required with a planned holder
// write.
var adoptFixtures = []adoptFixture{
	{name: "stopped attach", build: stoppedAttach},
	{name: "reconciliation_required", build: func(t *testing.T) (*parkedFlowDisk, *holderWrites, string) {
		disk, writes := unrecordedHolderDisk(t)
		return disk, writes, plantPlannedHolderWrite(t, disk, nil)
	}},
}

// TestAdoptRefusedByItsOwnershipCheckLeavesTheJournalAsItWas runs the
// attested adopt when its ownership check can't read the disk's volume, and
// so can't verify the disk where the readback found it. Adopt
// refuses before it saves anything, so the record and its planned write stay
// exactly as they were, and a rerun once the volume reads back adopts it.
func TestAdoptRefusedByItsOwnershipCheckLeavesTheJournalAsItWas(t *testing.T) {
	t.Parallel()
	for _, fixture := range adoptFixtures {
		t.Run(fixture.name, func(t *testing.T) {
			t.Parallel()
			disk, writes, planned := fixture.build(t)
			before := disk.record(t)
			cluster := disk.deps.PVE.(holderWritesPVE)
			disk.deps.PVE = ownershipReadFailsPVE{holderWritesPVE: cluster}

			_, err := adoptDisk(t, disk, attestedAdopt(disk))
			var unverified *storageDecisionObservationError
			if !errors.As(err, &unverified) {
				t.Fatalf("adopt err = %v, want the ownership check's refusal to verify the disk", err)
			}
			if got := disk.record(t); !reflect.DeepEqual(got, before) {
				t.Fatalf("the refused adopt changed the record to %+v, want %+v", got, before)
			}

			disk.deps.PVE = cluster
			record, err := adoptDisk(t, disk, attestedAdopt(disk))
			if err != nil || record.State != aj.Adopted || stepByID(t, record, planned).State != aj.Observed {
				t.Fatalf("the rerun adopt left the record %s with the write %s (err %v), want it adopted with the write observed", record.State, stepByID(t, disk.record(t), planned).State, err)
			}
			if descriptions, _, _, _, _ := writes.counts(); descriptions != 0 {
				t.Fatalf("adopt wrote 777's notes %d times, want never", descriptions)
			}
		})
	}
}

// TestAdoptRefusedByItsAuditGateLeavesTheJournalAsItWas runs the attested
// adopt while VM 778 claims the disk's serial for the same allocation under
// another volume. Adopt refuses, and the record and its planned write stay
// exactly as they were.
func TestAdoptRefusedByItsAuditGateLeavesTheJournalAsItWas(t *testing.T) {
	t.Parallel()
	for _, fixture := range adoptFixtures {
		t.Run(fixture.name, func(t *testing.T) {
			t.Parallel()
			disk, _, _ := fixture.build(t)
			claimDiskOnVM778(t, disk)
			before := disk.record(t)

			_, err := adoptDisk(t, disk, attestedAdopt(disk))
			if err == nil || !strings.Contains(err.Error(), "allocation disposition refused") {
				t.Fatalf("adopt err = %v, want the audit gate's refusal", err)
			}
			if got := disk.record(t); !reflect.DeepEqual(got, before) {
				t.Fatalf("the refused adopt changed the record to %+v, want %+v", got, before)
			}
		})
	}
}

// claimDiskOnVM778 gives VM 778 a provenance entry for the disk's serial that
// names the disk's allocation on another volume.
func claimDiskOnVM778(t *testing.T, disk *parkedFlowDisk) {
	t.Helper()
	claim := pve.DiskAllocationProvenance{Version: 1, AllocationID: disk.id, AllocationNamespace: disk.deps.Config.StoragePlacementNamespace, Volid: "a:778/vm-778-disk-0.raw", Node: "n1", Backing: "claimed-elsewhere"}
	encoded, err := json.Marshal(map[string]pve.DiskAllocationProvenance{diskStableIDFromCID(t, disk.cid): claim})
	if err != nil {
		t.Fatal(err)
	}
	notes, err := pve.RenderSentinel("", map[string]json.RawMessage{"bosh_disk_allocations": encoded})
	if err != nil {
		t.Fatal(err)
	}
	disk.client.state.configs[778] = map[string]any{"name": "other", "description": notes, "digest": "778"}
}

// TestAdoptThatSettlesAndThenCannotSaveSaysWhatItSaved runs the attested
// adopt on a stopped attach when the journal refuses the save that adopts
// the record. The settlement's own save went through, so adopt says that it
// settled the write and saved the record in reconciliation_required, and
// names the disk calls that go on from there and the rerun of the attested
// adopt that finishes the adoption. The next attach_disk goes on.
func TestAdoptThatSettlesAndThenCannotSaveSaysWhatItSaved(t *testing.T) {
	t.Parallel()
	disk, writes, planned := stoppedAttach(t)
	refused := errors.New("disk full")
	ctx := aj.WithSaveFaultForTest(t.Context(), func(record aj.Record) error {
		if record.State == aj.Adopted {
			return refused
		}
		return nil
	})

	_, err := ApplyStorageAllocationDecision(ctx, disk.deps, disk.journal, []string{"n1"}, attestedAdopt(disk))
	if !errors.Is(err, refused) {
		t.Fatalf("adopt err = %v, want the refused save as its cause", err)
	}
	for _, text := range []string{
		"adopt settled step " + planned + " (lifecycle_attach_disk_Nodes_UpdateQemuConfig) from its readback of VM 777 on node n1",
		"saved the record in reconciliation_required, and then it could not save the adoption",
		"the next attach_disk, detach_disk, or delete_vm of the disk writes the holder's provenance entry under the disk's allocation lock and goes on",
		"a rerun of this adopt with --previous-writer-fenced and --remote-tasks-settled reads the VM back again and finishes the adoption",
	} {
		if !strings.Contains(err.Error(), text) {
			t.Fatalf("adopt err = %v, want it to contain %q", err, text)
		}
	}
	record := disk.record(t)
	if record.State != aj.ReconciliationRequired || record.Reason != adoptStalledReason("attach_disk", []string{planned}) || stepByID(t, record, planned).State != aj.Observed {
		t.Fatalf("adopt left the record %s (reason %q) with the write %s, want reconciliation_required with the write observed", record.State, record.Reason, stepByID(t, record, planned).State)
	}

	if err := disk.attach(t.Context()); err != nil {
		t.Fatalf("attach_disk after the settled adopt failed: %v", err)
	}
	requireHealedOnceUnderLock(t, writes)
}

// TestOnlyAdoptSettlesAPlannedHolderWrite runs finalize-cleanup with both
// attestations and the disk's CID on a record with a planned holder write.
// Only adopt settles such a write, so the write stays planned, and the
// refusal doesn't offer adopt's conditions.
func TestOnlyAdoptSettlesAPlannedHolderWrite(t *testing.T) {
	t.Parallel()
	for _, fixture := range adoptFixtures {
		t.Run(fixture.name, func(t *testing.T) {
			t.Parallel()
			disk, _, planned := fixture.build(t)
			before := disk.record(t)
			decision := attestedAdopt(disk)
			decision.Action = "finalize-cleanup"
			_, err := ApplyStorageAllocationDecision(t.Context(), disk.deps, disk.journal, []string{"n1"}, decision)
			if err == nil || !strings.Contains(err.Error(), "step "+planned+" (") || strings.Contains(err.Error(), "adopt") {
				t.Fatalf("finalize-cleanup err = %v, want a refusal that names step %s and says nothing of adopt", err, planned)
			}
			if got := disk.record(t); !reflect.DeepEqual(got, before) {
				t.Fatalf("finalize-cleanup changed the record to %+v, want %+v", got, before)
			}
		})
	}
}

// TestAdoptNamesTheReadbackThatFailedForEveryWrite runs the attested adopt on
// two planned holder writes, the second on VM 778, where the disk isn't.
// Adopt settles neither, and the refusal for the first write says that the
// second's readback failed and why.
func TestAdoptNamesTheReadbackThatFailedForEveryWrite(t *testing.T) {
	t.Parallel()
	for _, fixture := range adoptFixtures {
		t.Run(fixture.name, func(t *testing.T) {
			t.Parallel()
			disk, _, planned := fixture.build(t)
			second := plantPlannedHolderWrite(t, disk, func(step *aj.Step) {
				step.ID += "-778"
				step.Target.VMID = 778
			})
			requireAdoptLeavesStep(t, disk, attestedAdopt(disk), planned, "is planned",
				"adopt settles this write only together with every other unsettled step, and its readback of step "+second+" failed",
				"adopt read the disk back and found it on VM 777 on node n1, where the step targets VM 778 on node n1")
		})
	}
}

// TestAdoptSettlesOnlyTheConfigurationWritesOfDiskCalls runs the attested
// adopt on a planned configuration write that no attach_disk, detach_disk,
// delete_disk, or delete_vm planned. Adopt leaves it and says which writes it
// settles, and leaves the stopped attach's write with it, naming the step it
// can't settle.
func TestAdoptSettlesOnlyTheConfigurationWritesOfDiskCalls(t *testing.T) {
	t.Parallel()
	for _, fixture := range adoptFixtures {
		t.Run(fixture.name, func(t *testing.T) {
			t.Parallel()
			requireOnlyDiskCallWritesSettle(t, fixture)
		})
	}
}

// requireOnlyDiskCallWritesSettle plants a resize write beside the fixture's
// holder write and checks that adopt leaves both, and then the resize alone.
func requireOnlyDiskCallWritesSettle(t *testing.T, fixture adoptFixture) {
	t.Helper()
	disk, _, planned := fixture.build(t)
	other := plantPlannedHolderWrite(t, disk, func(step *aj.Step) {
		step.ID += "-resize"
		step.Kind = "lifecycle_resize_disk_Nodes_UpdateQemuConfig"
	})
	requireAdoptLeavesStep(t, disk, attestedAdopt(disk), planned, "is planned",
		"adopt settles this write only together with every other unsettled step, and it can't settle step "+other+" (lifecycle_resize_disk_Nodes_UpdateQemuConfig)")

	record := disk.record(t)
	settled := stepByID(t, record, planned)
	settled.State = aj.Observed
	for i := range record.Steps {
		if record.Steps[i].ID == planned {
			record.Steps[i] = settled
		}
	}
	rewriteJournalRecord(t, disk.deps, disk.id, record)
	requireAdoptLeavesStep(t, disk, attestedAdopt(disk), other, "is planned",
		"adopt settles only a configuration write that attach_disk, detach_disk, delete_disk, or delete_vm planned")
}

// TestAdoptSettlesOnlyARecordItCanAdopt runs the attested adopt on a stopped
// attach whose decision names another CID, and on one whose record no disk
// call's admission explains. Adopt settles nothing and says which records it
// settles a write on.
func TestAdoptSettlesOnlyARecordItCanAdopt(t *testing.T) {
	t.Parallel()
	const want = "; adopt settles this write only on a disk record with the exact CID that is in reconciliation_required, or that the disk call which planned the write left planned or observed when it stopped"
	t.Run("another CID", func(t *testing.T) {
		t.Parallel()
		disk, _, planned := stoppedAttach(t)
		decision := attestedAdopt(disk)
		decision.ExpectedCID += "-other"
		requireAdoptLeavesStep(t, disk, decision, planned, "is planned", want)
	})
	t.Run("no disk call's admission", func(t *testing.T) {
		t.Parallel()
		disk, _, planned := stoppedAttach(t)
		record := disk.record(t)
		record.Reason = "allocation planned"
		rewriteJournalRecord(t, disk.deps, disk.id, record)
		requireAdoptLeavesStep(t, disk, attestedAdopt(disk), planned, "is planned", want)
	})
}

// TestAdoptReadbackRefusesWhatItCannotSettle checks the readback adopt
// settles a write from against the disk as a stopped attach left it, and
// against that disk on a parker, mid-transfer, and resolved to another
// allocation.
func TestAdoptReadbackRefusesWhatItCannotSettle(t *testing.T) {
	t.Parallel()
	disk, _, planned := stoppedAttach(t)
	record := disk.record(t)
	step := stepByID(t, record, planned)
	rd, err := resolveDeleteDiskCID(withHolderHeal(t.Context(), holderHealDefer), disk.deps, record.CID)
	if err != nil {
		t.Fatalf("the stopped attach's disk doesn't resolve: %v", err)
	}
	if evidence, reason := adoptConfigReadback(t.Context(), disk.deps, rd, record, step); reason != "" || evidence.VMID != 777 {
		t.Fatalf("readback of the stopped attach = %+v, %q, want it settled on 777", evidence, reason)
	}
	for _, tc := range []struct {
		name   string
		change func(rd *resolvedDisk)
		want   string
	}{
		{
			name: "parker holder",
			change: func(rd *resolvedDisk) {
				holder := *rd.holder
				holder.IsParker = true
				rd.holder = &holder
			},
			want: "the step targets VM 777, which is a parker, and adopt settles only a write on a workload VM",
		},
		{
			name: "transfer in flight",
			change: func(rd *resolvedDisk) {
				rd.intent = &pve.DiskTransferIntent{ParkerVMID: disk.parker, ParkerNode: "n1", Volid: rd.volid}
			},
			want: fmt.Sprintf("adopt read the disk back and found a transfer of it to parker %d still in flight", disk.parker),
		},
		{
			name: "another allocation",
			change: func(rd *resolvedDisk) {
				other := *rd.allocation
				other.record.ID = "another-allocation"
				rd.allocation = &other
			},
			want: "adopt read the disk back, and it doesn't resolve to this allocation",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := rd
			tc.change(&changed)
			if _, reason := adoptConfigReadback(t.Context(), disk.deps, changed, record, step); !strings.Contains(reason, tc.want) {
				t.Fatalf("readback reason = %q, want it to contain %q", reason, tc.want)
			}
		})
	}
}

// TestSettledHolderProofRefusesWhatItCannotProve checks the proof adopt's
// ownership check makes for a holder without the disk's entry against the
// disk as a stopped attach left it, and against another holder, a transfer
// in flight, another unsettled step, and another VM's claim on the disk.
func TestSettledHolderProofRefusesWhatItCannotProve(t *testing.T) {
	t.Parallel()
	disk, _, planned := stoppedAttach(t)
	record := disk.record(t)
	rd, err := resolveDeleteDiskCID(withHolderHeal(t.Context(), holderHealDefer), disk.deps, record.CID)
	if err != nil {
		t.Fatalf("the stopped attach's disk doesn't resolve: %v", err)
	}
	evidence, reason := adoptConfigReadback(t.Context(), disk.deps, rd, record, stepByID(t, record, planned))
	if reason != "" {
		t.Fatalf("readback of the stopped attach refused: %s", reason)
	}
	ctx := withSettledHolderProof(t.Context(), &adoptSettlement{record: record, steps: []adoptSettledConfigStep{evidence}})
	cfg := maps.Clone(disk.client.state.configs[777])
	if err := proveSettledHolder(ctx, disk.deps, rd, record, false, cfg); err != nil {
		t.Fatalf("the proof refused the holder adopt read back: %v", err)
	}
	for _, tc := range []struct {
		name   string
		change func(rd *resolvedDisk, record *aj.Record)
		want   string
	}{
		{
			name: "another holder",
			change: func(rd *resolvedDisk, _ *aj.Record) {
				holder := *rd.holder
				holder.VMID = 778
				rd.holder = &holder
			},
			want: "and it is not VM 777 on node n1, which adopt read back",
		},
		{
			name: "transfer in flight",
			change: func(rd *resolvedDisk, _ *aj.Record) {
				rd.intent = &pve.DiskTransferIntent{ParkerVMID: disk.parker, ParkerNode: "n1", Volid: rd.volid}
			},
			want: fmt.Sprintf("and a transfer of the disk to parker %d is still in flight", disk.parker),
		},
		{
			name: "another unsettled step",
			change: func(_ *resolvedDisk, record *aj.Record) {
				step := stepByID(t, *record, planned)
				step.ID = planned + "-other"
				record.Steps = append(record.Steps, step)
			},
			want: "step " + planned + "-other (lifecycle_attach_disk_Nodes_UpdateQemuConfig) is planned in the disk's record",
		},
		{
			name:   "another VM's claim",
			change: func(*resolvedDisk, *aj.Record) { claimDiskOnVM778(t, disk) },
			want:   "778",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changedRD, changedRecord := rd, record
			changedRecord.Steps = append([]aj.Step(nil), record.Steps...)
			tc.change(&changedRD, &changedRecord)
			err := proveSettledHolder(ctx, disk.deps, changedRD, changedRecord, false, cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("proof err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}
