package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// detachLockBugRecord attaches a fresh managed disk to VM 777 and then
// detaches it under the parked strategy while every sentinel create fails with
// an outcome the guard cannot classify. The first lock the detach takes is the
// disk's own per-disk transfer lock, so the record it leaves behind holds one
// planned detach lock step whose sentinel is bosh-lock-disk-<stable ID>.
func detachLockBugRecord(t *testing.T) (*parkedFlowDisk, *lockContention, aj.Step) {
	t.Helper()
	locks := newLockContention(t)
	deps, client, journal, id, cid := lifecycleFlowFixtureState(t, true)
	d := &parkedFlowDisk{deps: deps, client: client, journal: journal, id: id, cid: cid, gate: make(chan struct{})}
	close(d.gate)
	d.deps.PVE = contendedFlowPVE{lifecycleFlowPVE: client, locks: locks, gate: d.gate}
	delete(client.state.configs[777], "scsi1")
	d.deps.Config.DetachedDiskStrategy = "parked"
	parker := d.deps.Config.ParkedDiskVMIDRangeStartValue()
	client.state.configs[parker] = map[string]any{"name": fmt.Sprintf("bosh-parker-%d", parker), "tags": "bosh-parker", "protection": 1, "scsihw": "virtio-scsi-pci", "digest": "1"}
	d.parker = parker
	if err := d.attach(t.Context()); err != nil {
		t.Fatalf("setup attach: %v", err)
	}
	locks.reset()
	locks.createErr = errors.New("connection reset by peer")
	if err := d.detach(t); err == nil {
		t.Fatal("the replayed lock failure did not fail the detach")
	}
	locks.reset()
	record := d.record(t)
	if record.State != aj.ReconciliationRequired {
		t.Fatalf("replayed record is %s, want %s", record.State, aj.ReconciliationRequired)
	}
	var planned []aj.Step
	for i := range record.Steps {
		if record.Steps[i].Attempt == record.ActiveAttempt() && record.Steps[i].State != aj.Observed {
			planned = append(planned, record.Steps[i])
		}
	}
	if len(planned) != 1 || planned[0].Kind != "lifecycle_detach_disk_Pool_CreatePool" || planned[0].State != aj.Planned {
		t.Fatalf("replayed record does not hold one planned detach lock step: %+v", record.Steps)
	}
	return d, locks, planned[0]
}

func (d *parkedFlowDisk) detach(t *testing.T) error {
	t.Helper()
	_, err := HandleDetachDisk(d.deps).Handle(t.Context(), []json.RawMessage{planJSON(t, "777"), planJSON(t, d.cid)}, jsonrpc.Context{})
	return err
}

// TestLockStepSentinelsAddTheDiskLockOnlyForATransfer pins which disk records
// read the per-disk transfer lock. Only a planned lock step of an operation
// that transfers the disk into a parker could have meant it, so an attach's
// lock step, a park's, an observed one, and a record without a token all
// leave it out.
func TestLockStepSentinelsAddTheDiskLockOnlyForATransfer(t *testing.T) {
	const token = "bpd-00112233aabbccdd"
	diskLock := pve.DiskTransferLockPoolName(token)
	step := func(kind string, state aj.State, attempt int) aj.Step {
		return aj.Step{Kind: kind, State: state, Attempt: attempt, Target: aj.Target{Node: "n1", VMID: 90000}}
	}
	for name, tc := range map[string]struct {
		record aj.Record
		want   bool
	}{
		"planned detach lock step":   {aj.Record{Kind: "disk", DiskToken: token, Steps: []aj.Step{step("lifecycle_detach_disk_Pool_CreatePool", aj.Planned, 0)}}, true},
		"planned detach unlock step": {aj.Record{Kind: "disk", DiskToken: token, Steps: []aj.Step{step("lifecycle_detach_disk_Pool_DeletePool", aj.Planned, 0)}}, true},
		"planned attach lock step":   {aj.Record{Kind: "disk", DiskToken: token, Steps: []aj.Step{step("lifecycle_attach_disk_Pool_CreatePool", aj.Planned, 0)}}, false},
		"planned park lock step":     {aj.Record{Kind: "disk", DiskToken: token, Steps: []aj.Step{step("park_Pool_CreatePool", aj.Planned, 0)}}, false},
		"observed detach lock step":  {aj.Record{Kind: "disk", DiskToken: token, Steps: []aj.Step{step("lifecycle_detach_disk_Pool_CreatePool", aj.Observed, 0), step("lifecycle_attach_disk_Pool_CreatePool", aj.Planned, 0)}}, false},
		"detach configuration step":  {aj.Record{Kind: "disk", DiskToken: token, Steps: []aj.Step{step("lifecycle_detach_disk_Nodes_UpdateQemuConfig", aj.Planned, 0)}}, false},
		"record without a token":     {aj.Record{Kind: "disk", Steps: []aj.Step{step("lifecycle_detach_disk_Pool_CreatePool", aj.Planned, 0)}}, false},
	} {
		t.Run(name, func(t *testing.T) {
			got := lockStepSentinels(tc.record)
			if slices.Contains(got, diskLock) != tc.want {
				t.Fatalf("sentinel candidates = %v, want the per-disk lock: %t", got, tc.want)
			}
			if !slices.Contains(got, "bosh-lock-vm-90000") {
				t.Fatalf("sentinel candidates = %v lost the step's VM sentinel", got)
			}
		})
	}
}

// TestDetachLockBugRecordReadsThePerDiskLock replays a detach whose per-disk
// transfer lock create failed with an outcome the guard could not classify.
// The record's candidates include the per-disk lock, and when PVE does not
// answer that read exactly the refusal names it, so settlement never passes a
// step whose own sentinel it did not read.
func TestDetachLockBugRecordReadsThePerDiskLock(t *testing.T) {
	disk, locks, planned := detachLockBugRecord(t)
	record := disk.record(t)
	diskLock := pve.DiskTransferLockPoolName(record.DiskToken)
	if got := lockStepSentinels(record); !slices.Contains(got, diskLock) {
		t.Fatalf("settlement reads %v for the detach record, want it to include %s", got, diskLock)
	}
	locks.readErr = poolVerdictError("permission check failed for /pool/" + diskLock + " (Pool.Audit)")
	_, err := ApplyStorageAllocationDecision(t.Context(), disk.deps, disk.journal, []string{"n1"}, adoptDecision(disk))
	if err == nil || !strings.Contains(err.Error(), "step "+planned.ID+" (lifecycle_detach_disk_Pool_CreatePool) is planned") {
		t.Fatalf("adopt did not name the unsettled detach lock step: %v", err)
	}
	if !strings.Contains(err.Error(), "PVE did not answer exactly for sentinel "+diskLock) {
		t.Fatalf("adopt did not name the per-disk lock it could not read: %v", err)
	}
	if step := stepByID(t, disk.record(t), planned.ID); step.State != aj.Planned {
		t.Fatalf("an ambiguous read settled the detach lock step: %s", step.State)
	}
}

// TestDetachLockBugRecordSettlesByOwnerlessReadback covers the same record
// once PVE answers. No sentinel holds a claim, so the readback proves the
// planned create left nothing behind, settles the step, and lets the adopt
// through.
func TestDetachLockBugRecordSettlesByOwnerlessReadback(t *testing.T) {
	disk, locks, planned := detachLockBugRecord(t)
	next, err := ApplyStorageAllocationDecision(t.Context(), disk.deps, disk.journal, []string{"n1"}, adoptDecision(disk))
	if err != nil {
		t.Fatalf("adopt refused the detach record: %v", err)
	}
	if step := stepByID(t, next, planned.ID); step.State != aj.Observed {
		t.Fatalf("adoption left the detach lock step %s", step.State)
	}
	locks.mu.Lock()
	defer locks.mu.Unlock()
	if len(locks.pools) != 0 {
		t.Fatalf("settlement created or left a sentinel: %v", locks.pools)
	}
}

// TestDetachLockBugRecordHealsOnDetachRetry is the Director's retry of the
// detach that left the record behind. The planned lock step is settled by
// readback, the detach parks the disk, and no sentinel is left behind.
func TestDetachLockBugRecordHealsOnDetachRetry(t *testing.T) {
	disk, locks, planned := detachLockBugRecord(t)
	if err := disk.detach(t); err != nil {
		t.Fatalf("the retried detach failed: %v", err)
	}
	record := disk.record(t)
	if step := stepByID(t, record, planned.ID); step.State != aj.Observed {
		t.Fatalf("the retry left the detach lock step %s", step.State)
	}
	if _, attached := disk.client.state.configs[777]["scsi1"]; attached {
		t.Fatal("the retried detach left the disk on VM 777")
	}
	locks.mu.Lock()
	defer locks.mu.Unlock()
	if len(locks.pools) != 0 {
		t.Fatalf("the retried detach left a sentinel behind: %v", locks.pools)
	}
}
