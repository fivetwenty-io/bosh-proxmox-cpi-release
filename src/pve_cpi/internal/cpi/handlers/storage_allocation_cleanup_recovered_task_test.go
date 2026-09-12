package handlers

import (
	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"reflect"
	"testing"
)

func TestCleanupRecoveredUploadTaskPreservesUnknownHistory(t *testing.T) {
	deps, j, c, r, step := cleanupISOFixture(t, true)
	decision := cleanupAttestedDecision(r.ID)
	decision.RecoveredTaskStep = step.ID
	decision.RecoveredTaskUPID = cleanupTestUploadUPID
	decision.RecoveredTaskEvidence = recoveredTaskTestEvidence(t, r, decision)
	result, err := CleanupStorageAllocation(t.Context(), deps, j, []string{"pve1"}, decision)
	if err != nil {
		t.Fatalf("%v: %v", err, unwrapCleanupTest(err))
	}
	if result.State != aj.Cleaned || c.destroyCount != 1 || c.deleted != 1 {
		t.Fatal("cleanup did not dispose exact resources")
	}
	for _, got := range result.Steps {
		if got.ID == step.ID && !reflect.DeepEqual(got, step) {
			t.Fatal("original unknown step rewritten")
		}
	}
}
func TestCleanupRecoveredUploadTaskRefusesUnprovenIdentity(t *testing.T) {
	for _, mode := range []string{"missing recovered step", "wrong step", "wrong task", "unfenced", "unsettled", "wrong timestamp", "missing marker", "absent task"} {
		t.Run(mode, func(t *testing.T) {
			deps, j, c, r, step := cleanupISOFixture(t, true)
			decision := cleanupAttestedDecision(r.ID)
			decision.RecoveredTaskStep = step.ID
			decision.RecoveredTaskUPID = cleanupTestUploadUPID
			decision.RecoveredTaskEvidence = recoveredTaskTestEvidence(t, r, decision)
			switch mode {
			case "missing recovered step":
				decision.RecoveredTaskStep = ""
			case "wrong step":
				decision.RecoveredTaskStep = "another"
			case "wrong task":
				decision.RecoveredTaskUPID = "UPID:pve1:00088AE5:03547171:6AA18319:qmcreate:123:pmx@pve!pmx:"
			case "unfenced":
				decision.PreviousWriterFenced = false
			case "unsettled":
				decision.RemoteTasksSettled = false
			case "wrong timestamp":
				c.ctime++
			case "missing marker":
				delete(c.configs[123], "description")
			case "absent task":
				deps.PVE.(*cleanupTaskClient).incomplete = true
			}
			before := diagnosticFiles(t, deps.Config.StorageAllocationJournalDir)
			if _, err := CleanupStorageAllocation(t.Context(), deps, j, []string{"pve1"}, decision); err == nil {
				t.Fatal("unproven recovery accepted")
			}
			if c.destroyCount != 0 || c.deleted != 0 || !reflect.DeepEqual(before, diagnosticFiles(t, deps.Config.StorageAllocationJournalDir)) {
				t.Fatal("refused recovery changed state")
			}
		})
	}
}

func TestCleanupRecoveredUploadSurvivesCleanupResponseLoss(t *testing.T) {
	for _, mode := range []string{"destroy", "delete absent", "delete present"} {
		t.Run(mode, func(t *testing.T) {
			deps, j, c, r, step := cleanupISOFixture(t, true)
			c.stopped = true
			c.lostDestroy = mode == "destroy"
			c.lostDelete = mode != "destroy"
			c.keepDelete = mode == "delete present"
			decision := cleanupAttestedDecision(r.ID)
			decision.RecoveredTaskStep = step.ID
			decision.RecoveredTaskUPID = cleanupTestUploadUPID
			decision.RecoveredTaskEvidence = recoveredTaskTestEvidence(t, r, decision)
			if _, err := CleanupStorageAllocation(t.Context(), deps, j, []string{"pve1"}, decision); err == nil {
				t.Fatal("lost response hidden")
			}
			prior, err := j.Inspect(r.ID)
			if err != nil {
				t.Fatal(err)
			}
			decision.DecisionID += "-second"
			result, err := CleanupStorageAllocation(t.Context(), deps, j, []string{"pve1"}, decision)
			if mode == "delete present" {
				if err == nil {
					t.Fatal("unknown delete replay admitted")
				}
			} else if err != nil || result.State != aj.Cleaned {
				t.Fatalf("cleanup reentry failed: %v (%v)", err, unwrapCleanupTest(err))
			}
			if c.destroyCount != 1 || c.deleted != 1 {
				t.Fatal("cleanup replayed mutation or omitted ISO")
			}
			after, e := j.Inspect(r.ID)
			if e != nil {
				t.Fatal(e)
			}
			for i, old := range prior.Steps {
				if !reflect.DeepEqual(old, after.Steps[i]) {
					t.Fatal("unknown history rewritten")
				}
			}
		})
	}
}
