package handlers

import (
	"errors"
	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"reflect"
	"strings"
	"testing"
)

func TestCleanupSubmittedDiskDeleteRequiresTaskAndIndependentAbsence(t *testing.T) {
	for _, mode := range []string{"complete", "present", "task failed", "unknown upid", "wrong target", "wrong kind", "visibility", "wrong task node", "wrong task type", "wrong task id"} {
		t.Run(mode, func(t *testing.T) { testCleanupSubmittedDiskDeleteCase(t, mode) })
	}
}
func testCleanupSubmittedDiskDeleteCase(t *testing.T, mode string) {

	deps, client, j, id, _ := lifecycleFlowFixtureState(t, true, true)
	delete(client.state.configs[777], "scsi1")
	pending := appendCleanupPending(t, j, id, "lifecycle_delete_disk_Nodes_UpdateQemuConfig")
	h, err := j.Acquire(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	r := h.Record()
	birth := r.Steps[0]
	target := birth.Target
	target.IntendedVolume = birth.VolIDs[0]
	deleted := aj.Step{ID: "delete-submitted", Kind: "lifecycle_delete_disk_Storage_DeleteVolumeAsync", State: aj.Submitted, Attempt: r.ActiveAttempt(), Target: target, UPID: "UPID:n1:00000001:00000002:6AA1786A:imgdel:123@" + target.Storage + ":root@pam!token:"}
	switch mode {
	case "unknown upid":
		deleted.UPID = ""
		deleted.State = aj.Planned
	case "wrong target":
		deleted.Target.IntendedVolume = "a:999/foreign.qcow2"
	case "wrong task node":
		deleted.UPID = strings.Replace(deleted.UPID, "UPID:n1:", "UPID:n2:", 1)
	case "wrong task type":
		deleted.UPID = strings.Replace(deleted.UPID, ":imgdel:", ":qmstart:", 1)
	case "wrong task id":
		deleted.UPID = strings.Replace(deleted.UPID, ":123@"+target.Storage+":", ":999@"+target.Storage+":", 1)
	case "wrong kind":
		deleted.Kind = "allocate"
	}
	planned := deleted
	planned.State = aj.Planned
	planned.UPID = ""
	r.Steps = append(r.Steps, planned)
	if err = h.Save(r); err != nil {
		t.Fatal(err)
	}
	r = h.Record()
	r.Steps[len(r.Steps)-1] = deleted
	if err = h.Save(r); err != nil {
		t.Fatal(err)
	}
	if err = h.Close(); err != nil {
		t.Fatal(err)
	}
	if mode != "present" {
		clear(client.state.volumes)
	}
	if mode == "visibility" {
		client.state.readErr = errors.New("audit unavailable")
	}
	taskClient := &cleanupTaskClient{Client: client}
	if mode == "visibility" {
		taskClient.visibilityErr = errors.New("audit visibility unavailable")
	}
	if mode == "task failed" {
		taskClient.taskErr = errors.New("task not stopped OK")
	}
	deps.PVE = taskClient
	before := diagnosticFiles(t, deps.Config.StorageAllocationJournalDir)
	got, err := CleanupStorageAllocation(t.Context(), deps, j, []string{"n1"}, cleanupAttestedDecision(id))
	if mode != "complete" && mode != "unknown upid" {
		if err == nil {
			t.Fatal("incomplete deletion proof admitted")
		}
		if !reflect.DeepEqual(before, diagnosticFiles(t, deps.Config.StorageAllocationJournalDir)) {
			t.Fatal("failed admission modified history")
		}
	} else {
		if err != nil {
			t.Fatal(err)
		}
		if got.State != aj.Cleaned || !reflect.DeepEqual(got.Steps[1], pending) || !reflect.DeepEqual(got.Steps[2], deleted) {
			t.Fatal("terminal proof rewrote pending history")
		}
		proof := got.Verifications[len(got.Verifications)-1]
		if !proof.Complete || !proof.AbsenceVerified || !proof.ArtifactDispositionVerified {
			t.Fatal("terminal disposition lacks full absence")
		}
	}
	if client.deletes != 0 || client.state.parkMutations != 0 {
		t.Fatal("settlement repeated a remote mutation")
	}
}

func TestCleanupSubmittedDeleteAcceptsRecordedPVEImgdelIdentity(t *testing.T) {
	volume := "nfs-persistent-1:20768/vm-20768-bosh-53c692497f8f79a9-alloc-4a3d3ff6-e386-4e98-8cad-d3fc7af4d822.qcow2"
	target := aj.Target{Node: "lab-pve-cpi-0", Storage: "nfs-persistent-1", Backing: "nfs://10.254.0.1/tank/nfs/labs/pve-cpi/multi-storage/persistent-1", IntendedVolume: volume}
	step := aj.Step{ID: "delete", Kind: "lifecycle_delete_disk_Storage_DeleteVolumeAsync", State: aj.Submitted, Target: target, UPID: "UPID:lab-pve-cpi-0:000573BD:03504636:6AA1786A:imgdel:20768@nfs-persistent-1:pmx@pve!pmx:"}
	record := aj.Record{Kind: "disk", Steps: []aj.Step{{ID: "owned", State: aj.Observed, Target: target, VolIDs: []string{volume}}, step}}
	if !cleanupPendingDiskDelete(step, record) {
		t.Fatal("real PVE deletion identity rejected")
	}
}
