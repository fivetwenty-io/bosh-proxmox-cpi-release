package handlers

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// lockBugRecord replays the shape releases before the lock fix left behind. A
// parked disk's attach_disk wrote its steps up to the parker lock, and the
// sentinel create then failed with an outcome the guard could not classify,
// which is how every contended create used to end. The record is left in
// reconciliation_required with its last step, the lifecycle's
// Pool.CreatePool, still planned, and no sentinel exists on PVE.
func lockBugRecord(t *testing.T) (*parkedFlowDisk, *lockContention, aj.Step) {
	t.Helper()
	locks := newLockContention(t)
	disk := newParkedFlowDisk(t, locks)
	locks.reset()
	// The incident's disk carried drive-option overrides, so its attach wrote
	// them onto the receiving VM before it took the parker lock. Recording the
	// same overrides on the parker's entry makes this attach do the same.
	withParkedOverlay(t, disk, map[string]string{"discard": "on", "iothread": "1", "ssd": "1"})
	locks.createErr = errors.New("connection reset by peer")
	if err := disk.attach(t.Context()); err == nil {
		t.Fatal("the replayed lock failure did not fail the attach")
	}
	locks.reset()
	record := disk.record(t)
	if record.State != aj.ReconciliationRequired {
		t.Fatalf("replayed record is %s, want %s", record.State, aj.ReconciliationRequired)
	}
	if overlay := overlayOn(t, disk); !strings.Contains(overlay, `"ssd":"1"`) {
		t.Fatalf("the replay did not write the overrides onto the receiving VM before the lock: %s", overlay)
	}
	var planned []aj.Step
	for i := range record.Steps {
		if record.Steps[i].State != aj.Observed {
			planned = append(planned, record.Steps[i])
		}
	}
	if len(planned) != 1 || planned[0].Kind != "lifecycle_attach_disk_Pool_CreatePool" || planned[0].State != aj.Planned {
		t.Fatalf("replayed record does not have the incident's shape: %+v", record.Steps)
	}
	return disk, locks, planned[0]
}

// withParkedOverlay records drive-option overrides on the parker's provenance
// entry for disk, the way a park records overrides it carried off a VM.
func withParkedOverlay(t *testing.T, disk *parkedFlowDisk, opts map[string]string) {
	t.Helper()
	cfg := disk.client.state.configs[disk.parker]
	nonBOSH, raw := pve.ParseSentinel(pve.DescriptionFromConfig(cfg))
	var entries map[string]map[string]any
	if err := json.Unmarshal(raw["bosh_parked_disks"], &entries); err != nil || len(entries) != 1 {
		t.Fatalf("parker provenance is not a single entry: %v %s", err, raw["bosh_parked_disks"])
	}
	for key := range entries {
		entries[key]["opts"] = opts
	}
	encoded, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	raw["bosh_parked_disks"] = encoded
	description, err := pve.RenderSentinel(nonBOSH, raw)
	if err != nil {
		t.Fatal(err)
	}
	cfg[pveConfigKeyDescription] = description
}

// overlayOn returns the drive-option overrides VM 777 carries for disk.
func overlayOn(t *testing.T, disk *parkedFlowDisk) string {
	t.Helper()
	_, raw := pve.ParseSentinel(pve.DescriptionFromConfig(disk.client.state.configs[777]))
	return string(raw["bosh_disk_opt_overlays"])
}

func stepByID(t *testing.T, record aj.Record, id string) aj.Step {
	t.Helper()
	for i := range record.Steps {
		if record.Steps[i].ID == id {
			return record.Steps[i]
		}
	}
	t.Fatalf("step %s missing", id)
	return aj.Step{}
}

func adoptDecision(disk *parkedFlowDisk) StorageAllocationDecision {
	return StorageAllocationDecision{Action: "adopt", AllocationID: disk.id, ExpectedCID: disk.cid, DecisionID: "lock-bug-adoption"}
}

// TestLockBugRecordHealsOnAttachRetry is the Director's retry of the attach
// that left the record behind. The planned lock step is settled by readback,
// the attach completes, and the allocation is returned.
func TestLockBugRecordHealsOnAttachRetry(t *testing.T) {
	disk, _, planned := lockBugRecord(t)
	if err := disk.attach(t.Context()); err != nil {
		t.Fatalf("the Director's retry was refused: %v", err)
	}
	record := disk.record(t)
	assertReturnedRecord(t, "retried", record)
	settled := stepByID(t, record, planned.ID)
	if settled.State != aj.Observed || len(settled.VolIDs) != 1 || settled.VolIDs[0] != planned.Target.IntendedVolume {
		t.Fatalf("the lock step was not settled the way the guard settles one: %+v", settled)
	}
	if slot := disk.client.state.configs[777]["scsi1"]; slot == nil {
		t.Fatalf("the retried attach did not land the disk: %v", disk.client.state.configs[777])
	}
	// The overrides the failed attach already wrote are rewritten in place,
	// not duplicated, and the landed drive carries them.
	overlay := overlayOn(t, disk)
	if strings.Count(overlay, `"ssd":"1"`) != 1 {
		t.Fatalf("the retry did not keep exactly one override entry: %s", overlay)
	}
	if drive, _ := pve.ConfigString(disk.client.state.configs[777], "scsi1"); !strings.Contains(drive, "ssd=1") || !strings.Contains(drive, "discard=on") {
		t.Fatalf("the landed drive lost the overrides: %s", drive)
	}
}

// TestLockBugRecordAdopts is the operator's route for the same record.
func TestLockBugRecordAdopts(t *testing.T) {
	disk, _, planned := lockBugRecord(t)
	next, err := ApplyStorageAllocationDecision(t.Context(), disk.deps, disk.journal, []string{"n1"}, adoptDecision(disk))
	if err != nil {
		t.Fatalf("adopt refused the record: %v", err)
	}
	if next.State != aj.Adopted || next.CID != disk.cid {
		t.Fatalf("adoption produced %s with CID %q", next.State, next.CID)
	}
	if step := stepByID(t, next, planned.ID); step.State != aj.Observed {
		t.Fatalf("adoption left the lock step %s", step.State)
	}
}

// TestLockBugRecordStaysUnsettledOnAnAmbiguousRead refuses both routes when
// the sentinel read does not answer exactly, and names the step it could not
// settle without repeating PVE's raw text.
func TestLockBugRecordStaysUnsettledOnAnAmbiguousRead(t *testing.T) {
	disk, locks, planned := lockBugRecord(t)
	locks.readErr = poolVerdictError("permission check failed for /pool/bosh-lock-vm-90000 (Pool.Audit)")

	_, err := ApplyStorageAllocationDecision(t.Context(), disk.deps, disk.journal, []string{"n1"}, adoptDecision(disk))
	if err == nil || !strings.Contains(err.Error(), "step "+planned.ID+" (lifecycle_attach_disk_Pool_CreatePool) is planned") {
		t.Fatalf("adopt did not name the unsettled lock step: %v", err)
	}
	if !strings.Contains(err.Error(), "PVE did not answer exactly for sentinel") {
		t.Fatalf("adopt did not say why the step stayed unsettled: %v", err)
	}
	err = disk.attach(t.Context())
	if err == nil || !strings.Contains(err.Error(), "step "+planned.ID+" (lifecycle_attach_disk_Pool_CreatePool) is planned") {
		t.Fatalf("the retry did not name the unsettled lock step: %v", err)
	}
	record := disk.record(t)
	if step := stepByID(t, record, planned.ID); step.State != aj.Planned || record.State != aj.ReconciliationRequired {
		t.Fatalf("an ambiguous read settled the record: %s %s", record.State, step.State)
	}
}

// TestPlannedNonLockStepStaysUnsettled keeps every other planned step out of
// the rule. A planned configuration write could have changed the disk's
// holder, so neither route may settle it, and both name it.
func TestPlannedNonLockStepStaysUnsettled(t *testing.T) {
	locks := newLockContention(t)
	disk := newParkedFlowDisk(t, locks)
	locks.reset()
	handle, err := disk.journal.Acquire(t.Context(), disk.id)
	if err != nil {
		t.Fatal(err)
	}
	if err := storageAllocationUncertain(handle, "lifecycle attach_disk Nodes.UpdateQemuConfig"); err == nil {
		t.Fatal("marking the record uncertain returned no refusal")
	}
	step, err := storageMutationIntent(handle, "lifecycle_attach_disk_Nodes_UpdateQemuConfig", aj.Target{Node: "n1", VMID: 777}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}

	_, err = ApplyStorageAllocationDecision(t.Context(), disk.deps, disk.journal, []string{"n1"}, adoptDecision(disk))
	if err == nil || !strings.Contains(err.Error(), "step "+step+" (lifecycle_attach_disk_Nodes_UpdateQemuConfig) is planned") {
		t.Fatalf("adopt did not name the planned configuration step: %v", err)
	}
	err = disk.attach(t.Context())
	if err == nil || !strings.Contains(err.Error(), "step "+step+" (lifecycle_attach_disk_Nodes_UpdateQemuConfig) is planned") {
		t.Fatalf("the retry did not name the planned configuration step: %v", err)
	}
	if got := stepByID(t, disk.record(t), step); got.State != aj.Planned {
		t.Fatalf("a planned configuration step was settled: %s", got.State)
	}
}

// TestLockBugRecordSettlesPastALiveClaim covers a sentinel that still holds a
// claim, which may be the dead request's own. Settlement proves the read and
// settles, and it leaves the claim for its TTL rather than deleting it.
func TestLockBugRecordSettlesPastALiveClaim(t *testing.T) {
	disk, locks, planned := lockBugRecord(t)
	plantHeldParkerLock(locks, disk.parker)
	next, err := ApplyStorageAllocationDecision(t.Context(), disk.deps, disk.journal, []string{"n1"}, adoptDecision(disk))
	if err != nil {
		t.Fatalf("adopt refused past a live claim: %v", err)
	}
	if step := stepByID(t, next, planned.ID); step.State != aj.Observed {
		t.Fatalf("the lock step stayed %s", step.State)
	}
	locks.mu.Lock()
	defer locks.mu.Unlock()
	if len(locks.pools) != 1 {
		t.Fatalf("settlement touched the sentinel: %v", locks.pools)
	}
}

// TestLockStepKinds pins which planned steps the rule may settle. A VM
// record's pool steps are excluded, because the same kind also creates
// deployment pools and a step does not record its pool.
func TestLockStepKinds(t *testing.T) {
	for kind, want := range map[string]bool{
		"lifecycle_attach_disk_Pool_CreatePool":        true,
		"lifecycle_detach_disk_Pool_DeletePool":        true,
		"park_Pool_CreatePool":                         true,
		"park_Pool_DeletePool":                         true,
		"vm.Pool.CreatePool":                           false,
		"vm.Pool.DeletePool":                           false,
		"lifecycle_attach_disk_Nodes_UpdateQemuConfig": false,
		"park_QEMU_AttachDisk":                         false,
		"lifecycle_attach_disk_Pool_AddVM":             false,
	} {
		if got := isLockStep(aj.Step{Kind: kind}); got != want {
			t.Errorf("isLockStep(%s) = %t, want %t", kind, got, want)
		}
	}
	record := aj.Record{Steps: []aj.Step{{Target: aj.Target{VMID: 90372}}, {Target: aj.Target{VMID: 4356}}, {Target: aj.Target{VMID: 90372}}, {}}}
	if got := lockStepSentinels(record); strings.Join(got, ",") != "bosh-lock-vm-4356,bosh-lock-vm-90372" {
		t.Errorf("sentinel candidates = %v", got)
	}
}
