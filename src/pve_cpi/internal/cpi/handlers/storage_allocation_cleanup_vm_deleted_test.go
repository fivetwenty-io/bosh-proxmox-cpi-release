package handlers

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	ns "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

func TestCleanupSubmittedVMISODeleteRequiresIndependentAbsence(t *testing.T) {
	for _, mode := range []string{"complete", "ISO present", "VM present", "task failed", "visibility", "wrong task", "unsubmitted", "foreign ISO", "unfenced"} {
		t.Run(mode, func(t *testing.T) { testCleanupVMISODeleted(t, mode) })
	}
}
func testCleanupVMISODeleted(t *testing.T, mode string) {
	t.Helper()
	deps, j, base, r, _ := resumeVMFixture(t, true)
	c := &cleanupISOClient{deleteManagedClient: &deleteManagedClient{resumeVMClient: base}, size: 10 << 20, ctime: 1788969753}
	deps.PVE = &cleanupTaskClient{Client: c}
	h, err := j.Acquire(t.Context(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	r = h.Record()
	plan, e := activeStorageAllocationPlan(r)
	if e != nil {
		t.Fatal(e)
	}
	iso, _ := managedVMRoleTarget(plan, storageRoleISO)
	old := aj.Step{ID: "observed-iso", Kind: "vm.iso.scsi30", State: aj.Planned, Target: aj.Target{Node: "pve1", Storage: "a", Backing: iso.BackingKey, VMID: 123, IntendedVolume: cleanupTestISO}}
	r.State = aj.ReconciliationRequired
	r.Reason = "delete readback failed"
	r.Steps = append(r.Steps, old)
	if err = h.Save(r); err != nil {
		t.Fatal(err)
	}
	r = h.Record()
	old.State = aj.Observed
	old.VolIDs = []string{cleanupTestISO}
	r.Steps[len(r.Steps)-1] = old
	if err = h.Save(r); err != nil {
		t.Fatal(err)
	}
	r = h.Record()
	target := old.Target
	target.VMID = 0
	step := aj.Step{ID: "delete-iso", Kind: "vm.delete.volume", State: aj.Planned, Target: target}
	if mode == "foreign ISO" {
		step.Target.IntendedVolume = "a:iso/vm-999-config.iso"
	}
	r.Steps = append(r.Steps, step)
	if err = h.Save(r); err != nil {
		t.Fatal(err)
	}
	r = h.Record()
	step.State = aj.Submitted
	step.UPID = "UPID:pve1:0006F392:0358E7DC:6AA18E83:imgdel:a:pmx@pve!pmx:"
	switch mode {
	case "wrong task":
		step.UPID = strings.Replace(step.UPID, ":imgdel:", ":qmstart:", 1)
	case "unsubmitted":
		step.State = aj.Planned
		step.UPID = ""
	case "foreign ISO":
		step.Target.IntendedVolume = "a:iso/vm-999-config.iso"
	}
	r.Steps[len(r.Steps)-1] = step
	if err = h.Save(r); err != nil {
		t.Fatal(err)
	}
	if err = h.Close(); err != nil {
		t.Fatal(err)
	}
	c.iso = mode == "ISO present"
	c.nodesRead.content = ns.ListStorageContentResponse{}
	if mode != "VM present" {
		delete(c.configs, 123)
	}
	tc := deps.PVE.(*cleanupTaskClient)
	if mode == "task failed" {
		tc.taskErr = errors.New("not successful")
	}
	if mode == "visibility" {
		tc.visibilityErr = errors.New("not complete")
	}
	decision := cleanupAttestedDecision(r.ID)
	if mode == "unfenced" {
		decision.PreviousWriterFenced = false
	}
	before := diagnosticFiles(t, deps.Config.StorageAllocationJournalDir)
	got, err := CleanupStorageAllocation(t.Context(), deps, j, []string{"pve1"}, decision)
	if mode == "complete" {
		if err != nil {
			t.Fatalf("cleanup: %v cause %v", err, unwrapCleanupTest(err))
		}
		if got.State != aj.Cleaned || !reflect.DeepEqual(got.Steps[len(got.Steps)-1], step) {
			t.Fatal("history changed or not cleaned")
		}
		if storageLifecycleSettled(got) == nil {
			t.Fatal("ordinary lifecycle accepted submitted history")
		}
	} else {
		if err == nil {
			t.Fatal("incomplete evidence admitted")
		}
		if !reflect.DeepEqual(before, diagnosticFiles(t, deps.Config.StorageAllocationJournalDir)) {
			t.Fatal("refusal changed journal")
		}
	}
	if c.destroyCount != 0 || c.stopCount != 0 || c.deleted != 0 {
		t.Fatal("settlement repeated remote mutation")
	}

}

func TestCleanupSubmittedVMISODeleteRejectsChangedOwnership(t *testing.T) {
	_, _, _, record, _ := resumeVMFixture(t, true)
	plan, err := activeStorageAllocationPlan(record)
	if err != nil {
		t.Fatal(err)
	}
	iso, _ := managedVMRoleTarget(plan, storageRoleISO)
	old := aj.Step{ID: "iso", Kind: "vm.iso.scsi30", State: aj.Observed, Target: aj.Target{Node: iso.Node, Storage: iso.StorageID, Backing: iso.BackingKey, VMID: 123, IntendedVolume: cleanupTestISO}, VolIDs: []string{cleanupTestISO}}
	step := aj.Step{ID: "delete", Kind: "vm.delete.volume", State: aj.Submitted, Target: old.Target, UPID: "UPID:pve1:0006F392:0358E7DC:6AA18E83:imgdel:a:pmx@pve!pmx:"}
	step.Target.VMID = 0
	for _, mode := range []string{"complete", "cross node shared", "wrong prior VMID", "wrong owner", "missing owned volume", "wrong backing", "wrong role", "wrong task owner", "wrong task node", "charged", "external", "missing plan"} {
		t.Run(mode, func(t *testing.T) {
			r := record
			o := old
			s := step
			switch mode {
			case "cross node shared":
				s.Target.Node = "pve2"
				s.UPID = strings.Replace(s.UPID, "pve1", "pve2", 1)
			case "wrong prior VMID":
				o.Target.VMID = 999
			case "wrong owner":
				o.Target.Node = "other"
			case "missing owned volume":
				o.VolIDs = nil
			case "wrong backing":
				s.Target.Backing = "other"
			case "wrong role":
				o.Kind = "vm.root.virtio0"
			case "wrong task owner":
				s.UPID = strings.Replace(s.UPID, ":imgdel:a:", ":imgdel:123@a:", 1)
			case "wrong task node":
				s.UPID = strings.Replace(s.UPID, "pve1", "other", 1)
			case "charged":
				s.Charges = []aj.Charge{{PlannedBytes: 1}}
			case "external":
				s.Target.External = true
			case "missing plan":
				r.Attempts = nil
				r.Intent.Plan = nil
			}
			r.Steps = append(append([]aj.Step{}, r.Steps...), o, s)
			want := mode == "complete" || mode == "cross node shared" && plan.Definitions[iso.StorageID].IsShared()
			if cleanupSubmittedVMISODelete(s, r) != want {
				t.Fatal("unexpected admission")
			}
		})
	}
}
