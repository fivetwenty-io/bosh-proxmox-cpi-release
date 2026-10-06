package handlers

// These tests cover how storage-journal adopt settles a configuration write
// that attach_disk planned on the disk's new holder and never sent, which
// leaves the holder without the disk's provenance entry and the write planned
// with nothing on PVE to read it back from. Adopt settles that write only when
// the decision attests both that the previous writer is fenced and that its
// PVE tasks have settled, the write has no task, charges nothing, and isn't a
// parker protection write, and a fresh readback finds the disk's volume, with
// its serial, in a drive slot of the VM the write targets. It writes only the
// journal. The next attach_disk or detach_disk then writes the holder's entry
// under the disk's allocation lock and goes on as usual. In every other case
// adopt refuses, names the step, and says which condition failed.

import (
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"strings"
	"testing"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
)

// plantPlannedHolderWrite adds to the disk's record the configuration write
// attach_disk plans on VM 777 before it records the disk's provenance entry
// there, left planned with no task, as a stop before the write leaves it.
// change, when set, adjusts the step first. It returns the step's ID.
func plantPlannedHolderWrite(t *testing.T, disk *parkedFlowDisk, change func(*aj.Step)) string {
	t.Helper()
	record := disk.record(t)
	step := aj.Step{
		ID:      fmt.Sprintf("attempt-%d-step-99", record.ActiveAttempt()),
		Attempt: record.ActiveAttempt(),
		Kind:    "lifecycle_attach_disk_Nodes_UpdateQemuConfig",
		State:   aj.Planned,
		Target:  aj.Target{Node: "n1", VMID: 777},
	}
	if change != nil {
		change(&step)
	}
	record.Steps = append(record.Steps, step)
	rewriteJournalRecord(t, disk.deps, disk.id, record)
	return step.ID
}

// attestedAdopt is the adopt decision for the disk with both attestations.
func attestedAdopt(disk *parkedFlowDisk) StorageAllocationDecision {
	decision := adoptDecision(disk)
	decision.DecisionID = "fenced-writer-adoption"
	decision.PreviousWriterFenced = true
	decision.RemoteTasksSettled = true
	return decision
}

// adoptDisk runs storage-journal adopt for the disk with decision.
func adoptDisk(t *testing.T, disk *parkedFlowDisk, decision StorageAllocationDecision) (aj.Record, error) {
	t.Helper()
	return ApplyStorageAllocationDecision(t.Context(), disk.deps, disk.journal, []string{"n1"}, decision)
}

// diskSerial returns the stable serial in the disk's CID.
func diskSerial(t *testing.T, disk *parkedFlowDisk) string {
	t.Helper()
	_, meta, err := pve.ParseEncodedDiskCID(disk.cid)
	if err != nil || meta == nil || meta.ID == "" {
		t.Fatalf("disk CID %s carries no stable serial (%v)", disk.cid, err)
	}
	return meta.ID
}

// slotWithSerial returns the slot in cfg that carries serial and the volume
// it names, or two empty strings when no slot carries it.
func slotWithSerial(cfg map[string]any, serial string) (slot, volume string) {
	for key, value := range qemu.ParseDisks(cfg) {
		if got, ok := pve.StableIDFromDriveOptStr(value); ok && got == serial {
			return key, strings.Split(value, ",")[0]
		}
	}
	return "", ""
}

// requireVolumePresent fails unless node n1 still lists volume with a size.
func requireVolumePresent(t *testing.T, disk *parkedFlowDisk, volume string) {
	t.Helper()
	storage, name, err := pve.ParseDiskCID(volume)
	if err != nil {
		t.Fatalf("volume %s doesn't parse: %v", volume, err)
	}
	info, err := disk.deps.PVE.Nodes().GetStorageContent(t.Context(), "n1", storage, name)
	if err != nil || info == nil || info.Size <= 0 {
		t.Fatalf("volume %s is gone (%+v, %v)", volume, info, err)
	}
	if disk.client.deletes != 0 {
		t.Fatalf("PVE destroyed %d volumes, want none", disk.client.deletes)
	}
}

// adoptedHolderWrite runs the attested adopt on an unrecorded holder with a
// planned holder write, and checks that adopt settled the write from the
// readback of 777's slot, adopted the record, and changed nothing on PVE.
func adoptedHolderWrite(t *testing.T) (*parkedFlowDisk, *holderWrites, string, string) {
	t.Helper()
	disk, writes := unrecordedHolderDisk(t)
	serial := diskSerial(t, disk)
	slot, volume := slotWithSerial(disk.client.state.configs[777], serial)
	if slot == "" {
		t.Fatalf("777 carries no slot with serial %s: %v", serial, disk.client.state.configs[777])
	}
	planned := plantPlannedHolderWrite(t, disk, nil)
	holder := maps.Clone(disk.client.state.configs[777])
	parker := maps.Clone(disk.client.state.configs[disk.parker])

	record, err := adoptDisk(t, disk, attestedAdopt(disk))
	if err != nil {
		t.Fatalf("attested adopt refused a planned holder write whose disk reads back in 777's slot: %v", err)
	}
	if record.State != aj.Adopted {
		t.Fatalf("adopted record state = %s, want %s", record.State, aj.Adopted)
	}
	step := stepByID(t, record, planned)
	if step.State != aj.Observed || !containsString(step.VolIDs, volume) {
		t.Fatalf("planned holder write = %+v, want it observed with volume %s", step, volume)
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
	if got := disk.client.state.configs[777]; !reflect.DeepEqual(got, holder) {
		t.Fatalf("adopt changed 777 to %v, want %v", got, holder)
	}
	if got := disk.client.state.configs[disk.parker]; !reflect.DeepEqual(got, parker) {
		t.Fatalf("adopt changed parker %d to %v, want %v", disk.parker, got, parker)
	}
	if held := holderProvenanceEntries(t, disk); len(held) != 0 {
		t.Fatalf("adopt wrote 777's provenance for the disk: %v", held)
	}
	requireVolumePresent(t, disk, volume)
	return disk, writes, serial, volume
}

// TestAttestedAdoptSettlesAPlannedHolderWriteThenAttachHealsTheHolder settles
// the planned holder write with the attested adopt, and then runs the
// Director's attach_disk again. The attach writes 777's missing entry once,
// under the disk's allocation lock, and completes, and the volume stays.
func TestAttestedAdoptSettlesAPlannedHolderWriteThenAttachHealsTheHolder(t *testing.T) {
	t.Parallel()
	disk, writes, _, volume := adoptedHolderWrite(t)

	if err := disk.attach(t.Context()); err != nil {
		t.Fatalf("attach_disk after the attested adopt failed: %v", err)
	}
	if _, _, heals, underLock, _ := writes.counts(); heals != 1 || underLock != 1 {
		t.Fatalf("the heal wrote 777's entry %d times, %d of them under the allocation lock, want once under it", heals, underLock)
	}
	if held := holderProvenanceEntries(t, disk); len(held) != 1 || held[0].Volid != volume {
		t.Fatalf("777 records the disk as %+v, want one entry for %s", held, volume)
	}
	if unsettled, ok := unsettledRecordStep(disk.record(t)); ok {
		t.Fatalf("attach_disk left step %s", unsettledStepName(unsettled))
	}
	requireVolumePresent(t, disk, volume)
}

// TestAttestedAdoptSettlesAPlannedHolderWriteThenDetachParksTheDisk settles
// the planned holder write with the attested adopt, and then runs
// detach_disk. The detach writes 777's missing entry once, under the disk's
// allocation lock, and moves the disk to its parker, which records it under
// the disk's allocation.
func TestAttestedAdoptSettlesAPlannedHolderWriteThenDetachParksTheDisk(t *testing.T) {
	t.Parallel()
	disk, writes, serial, _ := adoptedHolderWrite(t)

	args := []json.RawMessage{json.RawMessage(`"777"`), json.RawMessage(fmt.Sprintf("%q", disk.cid))}
	if _, err := HandleDetachDisk(disk.deps).Handle(t.Context(), args, jsonrpc.Context{}); err != nil {
		t.Fatalf("detach_disk after the attested adopt failed: %v", err)
	}
	if _, _, heals, underLock, _ := writes.counts(); heals != 1 || underLock != 1 {
		t.Fatalf("the heal wrote 777's entry %d times, %d of them under the allocation lock, want once under it", heals, underLock)
	}
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

// requireAdoptLeavesStep runs adopt with decision and fails unless it refuses
// with the step named and every text in want, and leaves the disk's record,
// VM 777, and the parker exactly as they were.
func requireAdoptLeavesStep(t *testing.T, disk *parkedFlowDisk, decision StorageAllocationDecision, step string, want ...string) {
	t.Helper()
	record := disk.record(t)
	holder := maps.Clone(disk.client.state.configs[777])
	parker := maps.Clone(disk.client.state.configs[disk.parker])
	_, err := adoptDisk(t, disk, decision)
	if err == nil {
		t.Fatal("adopt accepted the record although it should leave the step unsettled")
	}
	for _, text := range append([]string{"allocation has unsettled mutation evidence", "step " + step + " ("}, want...) {
		if !strings.Contains(err.Error(), text) {
			t.Fatalf("adopt err = %v, want it to contain %q", err, text)
		}
	}
	if got := disk.record(t); !reflect.DeepEqual(got, record) {
		t.Fatalf("adopt changed the disk's record to %+v, want %+v", got, record)
	}
	if got := disk.client.state.configs[777]; !reflect.DeepEqual(got, holder) {
		t.Fatalf("adopt changed 777 to %v, want %v", got, holder)
	}
	if got := disk.client.state.configs[disk.parker]; !reflect.DeepEqual(got, parker) {
		t.Fatalf("adopt changed parker %d to %v, want %v", disk.parker, got, parker)
	}
}

// TestAdoptLeavesAPlannedHolderWriteWithoutBothAttestations runs adopt
// without one or both attestations. It settles nothing, and its refusal names
// the flags the decision lacks.
func TestAdoptLeavesAPlannedHolderWriteWithoutBothAttestations(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name            string
		fenced, settled bool
		want            string
	}{
		{name: "neither", want: "rerun adopt with --previous-writer-fenced and --remote-tasks-settled once both hold"},
		{name: "fenced only", fenced: true, want: "rerun adopt with --remote-tasks-settled once both hold"},
		{name: "tasks settled only", settled: true, want: "rerun adopt with --previous-writer-fenced once both hold"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			disk, _ := unrecordedHolderDisk(t)
			planned := plantPlannedHolderWrite(t, disk, nil)
			decision := attestedAdopt(disk)
			decision.PreviousWriterFenced, decision.RemoteTasksSettled = tc.fenced, tc.settled
			requireAdoptLeavesStep(t, disk, decision, planned, "is planned", tc.want)
		})
	}
}

// TestAdoptLeavesAHolderWriteThatHasATask runs the attested adopt on a holder
// write that was submitted as a PVE task. Its outcome is the task's, so adopt
// leaves it and says so.
func TestAdoptLeavesAHolderWriteThatHasATask(t *testing.T) {
	t.Parallel()
	disk, _ := unrecordedHolderDisk(t)
	submitted := plantPlannedHolderWrite(t, disk, func(step *aj.Step) {
		step.State = aj.Submitted
		step.UPID = "UPID:n1:00001234:00005678:00000000:qmconfig:777:root@pam:"
	})
	requireAdoptLeavesStep(t, disk, attestedAdopt(disk), submitted, "is submitted", "it has a PVE task, and adopt settles only a configuration write that has none")
}

// TestAdoptLeavesAPlannedParkerProtectionWrite runs the attested adopt on a
// parker protection restore left planned while the parker reads unprotected.
// Only the parker's readback settles that write, so adopt leaves it, keeps
// the protection settler's instructions, and says it doesn't settle such a
// write from the disk.
func TestAdoptLeavesAPlannedParkerProtectionWrite(t *testing.T) {
	t.Parallel()
	disk, _ := unrecordedHolderDisk(t)
	plantPlannedRestore(t, disk)
	disk.client.state.configs[disk.parker]["protection"] = 0
	var restore string
	for _, step := range disk.record(t).Steps {
		if step.State == aj.Planned && IsParkerProtectionStep(disk.record(t), step) {
			restore = step.ID
		}
	}
	if restore == "" {
		t.Fatal("the record holds no planned parker protection write")
	}
	requireAdoptLeavesStep(t, disk, attestedAdopt(disk), restore, "is planned",
		fmt.Sprintf("protection is off on parker %d", disk.parker),
		"adopt doesn't settle a parker protection write from the disk's readback, because that write settles only when the parker reads back protected")
}

// TestAdoptLeavesAPlannedHolderWriteTheReadbackDoesNotShow runs the attested
// adopt when the readback doesn't find the disk where the write left it: the
// slot no longer holds the disk, the slot holds the volume under another
// serial, the write targets another VM, or the write names another volume.
// Adopt settles nothing and says what the readback found.
func TestAdoptLeavesAPlannedHolderWriteTheReadbackDoesNotShow(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		step   func(*aj.Step)
		holder func(cfg map[string]any, slot string)
		want   func(serial, volume string) string
	}{
		{
			name:   "slot empty",
			holder: func(cfg map[string]any, slot string) { delete(cfg, slot) },
			want: func(serial, _ string) string {
				return "adopt read VM 777 on node n1 back, and no drive slot there carries the disk's serial " + serial
			},
		},
		{
			name: "serial differs",
			holder: func(cfg map[string]any, slot string) {
				volume, _, _ := strings.Cut(cfg[slot].(string), "serial=")
				cfg[slot] = volume + "serial=bpd-0000000000000000"
			},
			want: func(serial, _ string) string {
				return "adopt read VM 777 on node n1 back, and no drive slot there carries the disk's serial " + serial
			},
		},
		{
			name: "another VM",
			step: func(step *aj.Step) { step.Target.VMID = 778 },
			want: func(string, string) string {
				return "adopt read the disk back and found it on VM 777 on node n1, where the step targets VM 778 on node n1"
			},
		},
		{
			name: "another volume",
			step: func(step *aj.Step) { step.Target.IntendedVolume = "c:777/vm-777-disk-9.raw" },
			want: func(_, volume string) string {
				return "the step names volume c:777/vm-777-disk-9.raw, and adopt read the disk back as " + volume
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			disk, _ := unrecordedHolderDisk(t)
			serial := diskSerial(t, disk)
			slot, volume := slotWithSerial(disk.client.state.configs[777], serial)
			if slot == "" {
				t.Fatalf("777 carries no slot with serial %s", serial)
			}
			planned := plantPlannedHolderWrite(t, disk, tc.step)
			if tc.holder != nil {
				tc.holder(disk.client.state.configs[777], slot)
			}
			requireAdoptLeavesStep(t, disk, attestedAdopt(disk), planned, "is planned", tc.want(serial, volume))
		})
	}
}
