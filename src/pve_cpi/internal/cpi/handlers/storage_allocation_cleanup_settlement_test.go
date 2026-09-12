package handlers

import (
	"context"
	"errors"
	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

type cleanupTaskClient struct {
	incomplete    bool
	visibilityErr error
	pve.Client
	taskErr         error
	taskCalls       int
	taskExit        string
	uploadEndOffset int64
	upids           []string
}

func (c *cleanupTaskClient) StorageAuditVisibility(ctx context.Context) error {
	if c.visibilityErr != nil {
		return c.visibilityErr
	}
	return c.Client.(pve.StorageAuditVisibilityReader).StorageAuditVisibility(ctx)
}
func (c *cleanupTaskClient) ObserveStorageTaskSettlement(_ context.Context, nodes []string, upids []string) (pve.StorageTaskSettlementEvidence, error) {
	c.taskCalls++
	if c.incomplete {
		return pve.StorageTaskSettlementEvidence{}, nil
	}
	c.upids = append([]string{}, upids...)
	now := time.Now()
	sorted := slices.Clone(nodes)
	slices.Sort(sorted)
	proof := pve.StorageTaskSettlementEvidence{Tasks: []pve.StorageSettledTask{}, Version: 1, StartedAt: now, CompletedAt: now, Nodes: sorted, TaskVisibilityVerified: true, ActiveTasksEmpty: true}
	exitStatus := c.taskExit
	if exitStatus == "" {
		exitStatus = "OK"
	}
	for _, upid := range upids {
		task := pve.StorageSettledTask{UPID: upid, Node: strings.Split(upid, ":")[1], Status: "stopped", ExitStatus: exitStatus}
		if strings.Split(upid, ":")[5] == "imgcopy" {
			task.StartTime, _ = strconv.ParseInt(strings.Split(upid, ":")[4], 16, 64)
			task.EndTime = task.StartTime + c.uploadEndOffset
		}
		proof.Tasks = append(proof.Tasks, task)
	}
	return proof, c.taskErr
}
func appendCleanupPending(t *testing.T, j *aj.Journal, id, kind string) aj.Step {
	t.Helper()
	h, err := j.Acquire(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := h.Close(); err != nil {
			t.Error(err)
		}
	}()
	r := h.Record()
	r.State = aj.ReconciliationRequired
	r.Reason = "configuration response readback rejected"
	if err = h.Save(r); err != nil {
		t.Fatal(err)
	}
	r = h.Record()
	step := aj.Step{ID: "pending-config", Kind: kind, State: aj.Planned, Attempt: r.ActiveAttempt(), Target: r.Steps[0].Target}
	step.Target.IntendedVolume = ""
	r.Steps = append(r.Steps, step)
	if err = h.Save(r); err != nil {
		t.Fatal(err)
	}
	return step
}
func cleanupAttestedDecision(id string) StorageAllocationDecision {
	return StorageAllocationDecision{Action: "cleanup", AllocationID: id, DecisionID: "incident-proof", AuthorityID: "fenced-writer", PreviousWriterFenced: true, RemoteTasksSettled: true}
}
func TestCleanupPendingConfigRetainsHistoryAndCannotAuthorizeReplay(t *testing.T) {
	for _, kind := range []string{"vm", "disk"} {
		t.Run(kind, func(t *testing.T) {
			var deps Deps
			var j *aj.Journal
			var id string
			var step aj.Step
			if kind == "vm" {
				d, jj, _, r := deleteManagedFixture(t)
				deps, j, id = d, jj, r.ID
				step = appendCleanupPending(t, j, id, "vm.Nodes.UpdateQemuConfig")
			} else {
				d, c, jj, allocation, _ := lifecycleFlowFixture(t)
				delete(c.state.configs[777], "scsi1")
				deps, j, id = d, jj, allocation
				step = appendCleanupPending(t, j, id, "lifecycle_delete_disk_Nodes_UpdateQemuConfig")
			}
			c := &cleanupTaskClient{Client: deps.PVE}
			deps.PVE = c
			node := "pve1"
			if kind == "disk" {
				node = "n1"
			}
			record, err := CleanupStorageAllocation(t.Context(), deps, j, []string{node}, cleanupAttestedDecision(id))
			if err != nil {
				t.Fatalf("cleanup: %v / %v", err, errors.Unwrap(err))
			}
			if record.State != aj.Cleaned {
				t.Fatalf("state %s", record.State)
			}
			found := false
			for _, s := range record.Steps {
				if s.ID == step.ID {
					found = true
					if !reflect.DeepEqual(s, step) {
						t.Fatal("pending history rewritten")
					}
				}
			}
			if !found {
				t.Fatal("pending history lost")
			}
			if storageLifecycleSettled(record) == nil {
				t.Fatal("ordinary lifecycle accepted original pending step")
			}
			if c.taskCalls != 1 {
				t.Fatal("independent task observation missing")
			}
		})
	}
}
func TestCleanupSettlementRejectsMissingProofAndUnknownAllocation(t *testing.T) {
	for _, mode := range []string{"unfenced", "unsettled", "task error", "incomplete proof", "allocation", "missing marker"} {
		t.Run(mode, func(t *testing.T) {
			deps, j, base, r := deleteManagedFixture(t)
			kind := "vm.Nodes.UpdateQemuConfig"
			if mode == "allocation" {
				kind = managedVMStepClone
			}
			appendCleanupPending(t, j, r.ID, kind)
			c := &cleanupTaskClient{Client: deps.PVE}
			deps.PVE = c
			decision := cleanupAttestedDecision(r.ID)
			switch mode {
			case "unfenced":
				decision.PreviousWriterFenced = false
			case "unsettled":
				decision.RemoteTasksSettled = false
			case "incomplete proof":
				c.incomplete = true
			case "task error":
				c.taskErr = errors.New("active task")
			case "missing marker":
				delete(base.configs[123], "description")
			}
			before := diagnosticFiles(t, deps.Config.StorageAllocationJournalDir)
			if _, err := CleanupStorageAllocation(t.Context(), deps, j, []string{"pve1"}, decision); err == nil {
				t.Fatal("unsafe cleanup accepted")
			}
			if base.destroyCount != 0 || base.stopCount != 0 {
				t.Fatal("refused cleanup mutated PVE")
			}
			if !reflect.DeepEqual(before, diagnosticFiles(t, deps.Config.StorageAllocationJournalDir)) {
				t.Fatal("refused cleanup changed authority")
			}
		})
	}
}
func TestCleanupCapabilityRejectsChangedAndNewSteps(t *testing.T) {
	r := aj.Record{ID: "allocation", Kind: "vm", Steps: []aj.Step{{ID: "pending", State: aj.Planned}}}
	hash, err := aj.Fingerprint(r.Steps[0])
	if err != nil {
		t.Fatal(err)
	}
	proof := &cleanupSettlement{AllocationID: r.ID, Steps: map[string]string{"pending": hash}}
	ctx := context.WithValue(t.Context(), cleanupSettlementKey{}, proof)
	if err := storageCleanupSettled(ctx, r); err != nil {
		t.Fatal(err)
	}
	r.ID = "other"
	if storageCleanupSettled(ctx, r) == nil {
		t.Fatal("other allocation accepted")
	}
	r.ID = proof.AllocationID
	proof.Attempt = 1
	if storageCleanupSettled(ctx, r) == nil {
		t.Fatal("other attempt accepted")
	}
	proof.Attempt = 0
	r.Steps[0].UPID = "unknown"
	if storageCleanupSettled(ctx, r) == nil {
		t.Fatal("changed unknown accepted")
	}
	r.Steps[0].UPID = ""
	r.Steps = append(r.Steps, aj.Step{ID: "new", State: aj.Planned})
	if storageCleanupSettled(ctx, r) == nil {
		t.Fatal("new unknown accepted")
	}
	if storageCleanupSettled(t.Context(), r) == nil {
		t.Fatal("durable proof leaked into another request")
	}
}

func TestPersistedCleanupEvidenceDoesNotAuthorizeOrdinaryLifecycle(t *testing.T) {
	deps, j, _, record := deleteManagedFixture(t)
	appendCleanupPending(t, j, record.ID, "vm.Nodes.UpdateQemuConfig")
	c := &cleanupTaskClient{Client: deps.PVE}
	deps.PVE = c
	h, err := j.Acquire(t.Context(), record.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := h.Close(); err != nil {
			t.Error(err)
		}
	}()
	ctx, settlement, err := admitStorageCleanupSettlement(t.Context(), deps, h.Record(), cleanupAttestedDecision(record.ID))
	if err != nil {
		t.Fatal(err)
	}
	id, body, err := aj.VerificationEvidence(settlement)
	if err != nil {
		t.Fatal(err)
	}
	r := h.Record()
	r.Verifications = append(r.Verifications, aj.Verification{EvidenceID: id, EvidenceJSON: body, Complete: true})
	if err := h.Save(r); err != nil {
		t.Fatal(err)
	}
	if err := storageCleanupSettled(ctx, h.Record()); err != nil {
		t.Fatal(err)
	}
	before := h.Record()
	proofID, proofBody, err := aj.VerificationEvidence(map[string]string{"ownership": "fresh independent test proof"})
	if err != nil {
		t.Fatal(err)
	}
	ownership := aj.Verification{EvidenceID: proofID, EvidenceJSON: proofBody, Complete: true, OwnershipVerified: true}
	if _, err := beginStorageLifecycleMode(h, "resize_disk", ownership, false); err == nil {
		t.Fatal("persisted cleanup proof authorized ordinary lifecycle")
	}
	if !reflect.DeepEqual(before, h.Record()) {
		t.Fatal("rejected lifecycle changed journal")
	}
}
