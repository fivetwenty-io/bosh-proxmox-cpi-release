package handlers

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
)

func TestAllocationCleanupDiskUsesLifecycleAuthorityWithoutLockReentry(t *testing.T) {
	deps, client, journal, id, _ := lifecycleFlowFixture(t)
	delete(client.state.configs[777], "scsi1")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	record, err := CleanupStorageAllocation(ctx, deps, journal, []string{"n1"}, StorageAllocationDecision{Action: "cleanup", AllocationID: id, DecisionID: "incident-explicit-delete"})
	if err != nil {
		t.Fatalf("cleanup: %v; source: %v", err, errors.Unwrap(err))
	}
	if record.State != aj.Deleted && record.State != aj.Cleaned {
		t.Fatalf("not terminal: %s", record.State)
	}
	found := false
	for _, v := range record.Verifications {
		found = found || strings.Contains(v.EvidenceJSON, "incident-explicit-delete")
	}
	if !found {
		t.Fatal("operator cleanup admission missing")
	}
	if len(client.state.volumes) != 0 {
		t.Fatal("owned disk survived successful cleanup")
	}
}

func TestAllocationCleanupRefusalsPreserveJournalAndResources(t *testing.T) {
	for _, mode := range []string{"wrong namespace", "unknown task", "duplicate holder", "secret read error", "attached disk"} {
		t.Run(mode, func(t *testing.T) {
			deps, client, journal, id, _ := lifecycleFlowFixture(t)
			switch mode {
			case "wrong namespace":
				deps.Config.StoragePlacementNamespace = "foreign"
			case "unknown task":
				h, e := journal.Acquire(t.Context(), id)
				if e != nil {
					t.Fatal(e)
				}
				r := h.Record()
				r.State = aj.ReconciliationRequired
				r.Reason = "unknown task"
				if e = h.Save(r); e != nil {
					t.Fatal(e)
				}
				r = h.Record()
				r.Steps = append(r.Steps, aj.Step{ID: "unknown", Kind: "delete", State: aj.Planned, Target: r.Steps[0].Target})
				if e = h.Save(r); e != nil {
					t.Fatal(e)
				}
				if e = h.Close(); e != nil {
					t.Fatal(e)
				}
			case "duplicate holder":
				client.state.configs[778] = map[string]any{"scsi1": client.state.configs[777]["scsi1"]}
			case "secret read error":
				client.state.readErr = errors.New("backend password=cleanup-secret")
			}
			before := diagnosticFiles(t, deps.Config.StorageAllocationJournalDir)
			_, err := CleanupStorageAllocation(t.Context(), deps, journal, []string{"n1"}, StorageAllocationDecision{Action: "cleanup", AllocationID: id, DecisionID: "refused"})
			if err == nil {
				t.Fatal("unsafe cleanup accepted")
			}
			if strings.Contains(err.Error(), "cleanup-secret") {
				t.Fatal("raw source error leaked")
			}
			if !reflect.DeepEqual(before, diagnosticFiles(t, deps.Config.StorageAllocationJournalDir)) {
				t.Fatal("failed admission changed journal")
			}
			if len(client.state.volumes) != 1 || client.state.parkMutations != 0 {
				t.Fatal("failed admission mutated resources")
			}
		})
	}
}

func TestRetainedMutationHelpersNeverReopenVMGeneration(t *testing.T) {
	_, journal, client, record, _ := resumeVMFixture(t)
	h, err := journal.Acquire(t.Context(), record.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := h.Close(); err != nil {
			t.Error(err)
		}
	}()
	target := record.Steps[1].Target
	target.IntendedVolume = record.Steps[1].VolIDs[0]
	proofID, payload, err := aj.VerificationEvidence(aj.VMRetentionEvidence{VMID: 123, RetainedArtifacts: []aj.Target{target}})
	if err != nil {
		t.Fatal(err)
	}
	r := h.Record()
	r.State = aj.VMDeletedRetained
	r.Verifications = append(r.Verifications, aj.Verification{EvidenceID: proofID, EvidenceJSON: payload, Complete: true, VMAbsenceVerified: true, ArtifactDispositionVerified: true})
	if err = h.Save(r); err != nil {
		t.Fatal(err)
	}
	delete(client.configs, 123)
	step, err := storageMutationIntent(h, "retained.delete", target, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = storageMutationSubmitted(h, step, "UPID:n1:retained"); err != nil {
		t.Fatal(err)
	}
	if h.Record().State != aj.VMDeletedRetained {
		t.Fatal("submission reopened generation")
	}
	if err = storageAllocationUncertain(h, "retained cleanup"); err == nil {
		t.Fatal("uncertainty not reported")
	}
	if h.Record().State != aj.VMDeletedRetained {
		t.Fatal("uncertainty reopened generation")
	}
	if err = storageMutationObserved(h, step, nil, false); err != nil {
		t.Fatal(err)
	}
	if h.Record().State != aj.VMDeletedRetained {
		t.Fatal("observation reopened generation")
	}
}

func TestAllocationCleanupVMRecordsDecisionAndRefusesUnknownDestroyRetry(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "unknown"}[unknown], func(t *testing.T) {
			deps, journal, client, record := deleteManagedFixture(t)
			client.unknownDestroy = unknown
			result, err := CleanupStorageAllocation(t.Context(), deps, journal, []string{"pve1"}, StorageAllocationDecision{Action: "cleanup", AllocationID: record.ID, DecisionID: "operator-vm-removal"})
			after, inspectErr := journal.Inspect(record.ID)
			if inspectErr != nil {
				t.Fatal(inspectErr)
			}
			if unknown {
				if err == nil || strings.Contains(err.Error(), "lost response secret") || after.State == aj.Cleaned || after.State == aj.Deleted {
					t.Fatalf("unknown destroy result=%+v err=%v state=%s", result, err, after.State)
				}
				count := client.destroyCount
				if _, again := CleanupStorageAllocation(t.Context(), deps, journal, []string{"pve1"}, StorageAllocationDecision{Action: "cleanup", AllocationID: record.ID, DecisionID: "retry-refused"}); again == nil {
					t.Fatal("unknown destroy blindly retried")
				}
				if client.destroyCount != count {
					t.Fatal("unknown response caused second destruction")
				}
			} else if err != nil || after.State != aj.Cleaned || client.destroyCount != 1 {
				t.Fatalf("cleanup state=%s count=%d err=%v", after.State, client.destroyCount, err)
			}
			found := false
			for _, v := range after.Verifications {
				found = found || strings.Contains(v.EvidenceJSON, "operator-vm-removal")
			}
			if !found {
				t.Fatal("VM operator admission lost")
			}
		})
	}
}
