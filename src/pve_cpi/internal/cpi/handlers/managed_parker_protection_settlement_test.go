package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
)

// stepState returns the state of the step with id in record.
func stepState(t *testing.T, record aj.Record, id string) aj.State {
	t.Helper()
	for i := range record.Steps {
		if record.Steps[i].ID == id {
			return record.Steps[i].State
		}
	}
	t.Fatalf("record has no step %s", id)
	return ""
}

// TestCutOffRestoreAdoptsOnceTheParkerReadsProtected is the operator's route:
// a cut-off restore leaves its step planned, the operator puts protection
// back, and adopt then settles the step by reading the parker and adopts the
// disk.
func TestCutOffRestoreAdoptsOnceTheParkerReadsProtected(t *testing.T) {
	t.Parallel()
	c := newCutOffRestore(t, 200*time.Millisecond)
	step := c.restoreStep(t)
	if v, _ := c.client.state.configs[c.parker]["protection"].(int); v != 0 {
		t.Fatalf("parker protection after the cut-off restore = %v, want the window's clear to have left it off", c.client.state.configs[c.parker]["protection"])
	}

	c.setProtection(true)
	next, err := c.adopt(t)
	if err != nil {
		t.Fatalf("adopt refused a record whose parker reads protected: %v", err)
	}
	if next.State != aj.Adopted || next.CID != c.cid {
		t.Fatalf("adoption produced %s with CID %q", next.State, next.CID)
	}
	if got := stepState(t, c.record(t), step.ID); got != aj.Observed {
		t.Fatalf("restore step %s left %s after a protected readback", step.ID, got)
	}
}

// TestCutOffRestoreRefusesWhileProtectionIsOff checks that the settler never
// settles a restore while the parker reads unprotected on its recorded node.
// Adopt refuses, names the step, and gives the qm set command that puts
// protection back.
func TestCutOffRestoreRefusesWhileProtectionIsOff(t *testing.T) {
	t.Parallel()
	t.Run("protection off", func(t *testing.T) {
		t.Parallel()
		c := newCutOffRestore(t, 200*time.Millisecond)
		step := c.restoreStep(t)
		c.setProtection(false)
		_, err := c.adopt(t)
		want := fmt.Sprintf("step %s (lifecycle_attach_disk_Nodes_UpdateQemuConfig) is planned; its parker protection write could not be settled because protection is off on parker %d; confirm with pvesh get /pools --poolid bosh-lock-vm-%d that no CPI operation holds the parker's lock, and only when it answers that the pool does not exist, run qm set %d --protection 1 on node n1, then retry",
			step.ID, c.parker, c.parker, c.parker)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("adopt with protection off = %v, want a refusal containing %q", err, want)
		}
		t.Logf("refusal: %v", err)
		if got := stepState(t, c.record(t), step.ID); got != aj.Planned {
			t.Fatalf("restore step settled to %s while protection is off", got)
		}
	})
}

// TestPlannedConfigStepWithOtherFieldsStaysPlanned plants two planned
// configuration steps on the parker next to the cut-off restore: one whose
// parameters carry a field besides protection, and one with no parameters at
// all. With the parker protected, the settler settles the restore but neither
// of those, and adopt refuses on the first of them.
func TestPlannedConfigStepWithOtherFieldsStaysPlanned(t *testing.T) {
	t.Parallel()
	c := newCutOffRestore(t, 200*time.Millisecond)
	restore := c.restoreStep(t)
	handle, err := c.journal.Acquire(c.ctx, c.id)
	if err != nil {
		t.Fatal(err)
	}
	target := restore.Target
	extra, err := storageMutationIntent(handle, "lifecycle_attach_disk_Nodes_UpdateQemuConfig", target, nil,
		json.RawMessage(`{"comment":"x","kind":"parker_protection_on","version":1}`))
	if err != nil {
		t.Fatal(err)
	}
	bare, err := storageMutationIntent(handle, "lifecycle_attach_disk_Nodes_UpdateQemuConfig", target, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}

	c.setProtection(true)
	_, err = c.adopt(t)
	if err == nil || !strings.Contains(err.Error(), "step "+extra+" (lifecycle_attach_disk_Nodes_UpdateQemuConfig) is planned") {
		t.Fatalf("adopt = %v, want a refusal naming the step with a non-protection field", err)
	}
	record := c.record(t)
	if got := stepState(t, record, restore.ID); got != aj.Observed {
		t.Fatalf("the protection-only restore step was left %s", got)
	}
	for _, id := range []string{extra, bare} {
		if got := stepState(t, record, id); got != aj.Planned {
			t.Fatalf("step %s, which is not protection-only, was settled to %s", id, got)
		}
	}
}

// TestRerunAttachSettlesTheCutOffRestore is the deploy's route: once the
// operator puts protection back, the rerun's attach_disk settles the restore
// step through the same settlement as adopt and returns the disk.
func TestRerunAttachSettlesTheCutOffRestore(t *testing.T) {
	t.Parallel()
	c := newCutOffRestore(t, 200*time.Millisecond)
	step := c.restoreStep(t)

	c.setProtection(false)
	if _, err := HandleAttachDisk(c.deps).Handle(c.ctx, c.attachArgs, jsonrpc.Context{}); err == nil ||
		!strings.Contains(err.Error(), fmt.Sprintf("run qm set %d --protection 1", c.parker)) {
		t.Fatalf("rerun attach with protection off = %v, want a refusal naming qm set", err)
	}

	c.setProtection(true)
	if _, err := HandleAttachDisk(c.deps).Handle(c.ctx, c.attachArgs, jsonrpc.Context{}); err != nil {
		t.Fatalf("rerun attach after protection was put back: %v", err)
	}
	record := c.record(t)
	if got := stepState(t, record, step.ID); got != aj.Observed {
		t.Fatalf("restore step %s left %s after the rerun", step.ID, got)
	}
	assertReturnedRecord(t, "rerun", record)
	if disk, _ := c.client.state.configs[777]["scsi1"].(string); !strings.Contains(disk, "vm-777-disk-") {
		t.Fatalf("VM 777 slot scsi1 = %q, want the transferred disk", disk)
	}
}

// newOwnedParkerRestoreDisk is newParkedRestoreDisk with the park, attach,
// park cycle run, so the disk sits on the parker under a name the parker owns.
// One park leaves a managed disk under its disk-band name, which no parker
// owns, and delete_disk then takes the unpark path instead of the owned-parker
// deletion this fixture is for.
func newOwnedParkerRestoreDisk(t *testing.T, timeout time.Duration) *cutOffRestore {
	t.Helper()
	c := newParkedRestoreDisk(t, timeout)
	if _, err := HandleAttachDisk(c.deps).Handle(c.ctx, c.attachArgs, jsonrpc.Context{}); err != nil {
		t.Fatalf("setup attach back to 777: %v", err)
	}
	if _, err := HandleDetachDisk(c.deps).Handle(c.ctx, []json.RawMessage{planJSON(t, "777"), planJSON(t, c.cid)}, jsonrpc.Context{}); err != nil {
		t.Fatalf("setup second detach into the parker: %v", err)
	}
	token := c.record(t).DiskToken
	slots := parkerSlotsCarrying(c.client, c.parker, token)
	if len(slots) != 1 {
		t.Fatalf("parker %d slots carrying the serial: %v, want exactly one", c.parker, slots)
	}
	volume, _ := c.client.state.configs[c.parker][slots[0]].(string)
	if !strings.Contains(volume, fmt.Sprintf("/vm-%d-", c.parker)) {
		t.Fatalf("parker %d holds %q, want a volume renamed for the parker", c.parker, volume)
	}
	return c
}

// rerunDeleteDiskSettlesTheCutOffRestore cuts off the protection restore of a
// delete_disk on a parked disk, then reruns delete_disk. firstWant is what the
// cut-off call's error has to contain. With protection off the rerun is refused
// with the qm set text. Once the parker reads protected, the rerun settles the
// restore step through the same settlement before it judges the record.
func rerunDeleteDiskSettlesTheCutOffRestore(t *testing.T, c *cutOffRestore, firstWant string) {
	t.Helper()
	deleteArgs := []json.RawMessage{planJSON(t, c.cid)}
	c.hung.arm(true)
	_, err := HandleDeleteDisk(c.deps).Handle(c.ctx, deleteArgs, jsonrpc.Context{})
	c.hung.arm(false)
	if err == nil || !strings.Contains(err.Error(), firstWant) {
		t.Fatalf("delete_disk with a hung restore = %v, want an error containing %q", err, firstWant)
	}
	t.Logf("delete_disk error: %v", err)
	var step aj.Step
	record := c.record(t)
	for i := range record.Steps {
		if record.Steps[i].Kind == "lifecycle_delete_disk_Nodes_UpdateQemuConfig" && record.Steps[i].State == aj.Planned {
			step = record.Steps[i]
		}
	}
	if step.ID == "" || !isParkerProtectionParameters(step.Parameters) || step.Target.VMID != c.parker {
		t.Fatalf("the cut-off delete left no planned protection step on parker %d: %+v", c.parker, record.Steps)
	}

	c.setProtection(false)
	if _, err := HandleDeleteDisk(c.deps).Handle(c.ctx, deleteArgs, jsonrpc.Context{}); err == nil ||
		!strings.Contains(err.Error(), fmt.Sprintf("run qm set %d --protection 1", c.parker)) {
		t.Fatalf("rerun delete_disk with protection off = %v, want a refusal naming qm set", err)
	}
	if got := stepState(t, c.record(t), step.ID); got != aj.Planned {
		t.Fatalf("restore step settled to %s while protection is off", got)
	}

	c.setProtection(true)
	if _, err := HandleDeleteDisk(c.deps).Handle(c.ctx, deleteArgs, jsonrpc.Context{}); err != nil {
		t.Fatalf("rerun delete_disk after protection was put back: %v", err)
	}
	record = c.record(t)
	if got := stepState(t, record, step.ID); got != aj.Observed {
		t.Fatalf("restore step %s left %s after the rerun delete_disk", step.ID, got)
	}
	if record.State != aj.Deleted {
		t.Fatalf("allocation state after the rerun delete_disk = %s (reason %q), want %s", record.State, record.Reason, aj.Deleted)
	}
}

// TestRerunDeleteDiskSettlesTheCutOffRestore covers both delete paths for a
// parked disk. A volume the parker owns is deleted in place, and the restore
// that follows reports its cut-off as an error. A volume under the disk-band
// name that one park left is unparked first, and that path logs a cut-off
// restore without returning it, so the call's completion audit is what refuses,
// with the qm set remedy. Either way the step stays planned, the rerun is held
// while protection is off, and it settles once the parker reads protected.
func TestRerunDeleteDiskSettlesTheCutOffRestore(t *testing.T) {
	t.Parallel()
	t.Run("parker-owned name", func(t *testing.T) {
		t.Parallel()
		c := newOwnedParkerRestoreDisk(t, 200*time.Millisecond)
		rerunDeleteDiskSettlesTheCutOffRestore(t, c, fmt.Sprintf("protection restore on parker vmid %d", c.parker))
	})
	t.Run("one park by config edit", func(t *testing.T) {
		t.Parallel()
		c := newParkedRestoreDisk(t, 200*time.Millisecond)
		rerunDeleteDiskSettlesTheCutOffRestore(t, c, fmt.Sprintf("run qm set %d --protection 1", c.parker))
	})
}

// TestProtectionPendingNeedsEveryUnsettledStepToBeAProtectionGap pins that a
// refusal is marked protection-pending only when every unsettled step of the
// active attempt is a protection write left planned. One other planned step
// beside the protection step keeps the refusal unmarked.
func TestProtectionPendingNeedsEveryUnsettledStepToBeAProtectionGap(t *testing.T) {
	protection := []byte(`{"version":1,"kind":"parker_protection_on"}`)
	record := aj.Record{
		Attempts: []aj.Attempt{{}},
		Steps: []aj.Step{
			{ID: "s0", Attempt: 0, Kind: "lifecycle_attach_disk_Nodes_CreateQemuMoveDisk", State: aj.Observed, Target: aj.Target{Node: "n1", VMID: 90000}},
			{ID: "s1", Attempt: 0, Kind: "lifecycle_attach_disk_Nodes_UpdateQemuConfig", State: aj.Planned, Target: aj.Target{Node: "n1", VMID: 90000}, Parameters: protection},
			{ID: "s2", Attempt: 0, Kind: "lifecycle_attach_disk_Nodes_CreateQemuMoveDisk", State: aj.Planned, Target: aj.Target{Node: "n1", VMID: 777}},
		},
	}
	gaps := map[string]error{"s1": &protectionSettlementGap{text: "protection is off on parker 90000"}}
	refusal := errors.New("lifecycle has unresolved mutation evidence")
	var pending *protectionPendingRefusal
	if errors.As(protectionPendingOr(record, gaps, refusal), &pending) {
		t.Fatal("a refusal with a planned move beside the protection step was marked protection-pending")
	}
	record.Steps = record.Steps[:2]
	if !errors.As(protectionPendingOr(record, gaps, refusal), &pending) {
		t.Fatal("a refusal whose only unsettled step is a protection gap was not marked protection-pending")
	}
}
