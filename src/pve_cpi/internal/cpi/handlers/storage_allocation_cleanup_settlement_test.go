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
	// The attach row is a config write that attach_disk planned, such as
	// the detach tail's removal on the disk's old VM. Cleanup settles it on
	// the same evidence it uses for a step that delete_disk plans.
	for _, kind := range []string{"vm", "disk", "attach"} {
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
				stepKind := "lifecycle_delete_disk_Nodes_UpdateQemuConfig"
				if kind == "attach" {
					stepKind = "lifecycle_attach_disk_Nodes_UpdateQemuConfig"
				}
				step = appendCleanupPending(t, j, id, stepKind)
			}
			c := &cleanupTaskClient{Client: deps.PVE}
			deps.PVE = c
			node := "pve1"
			if kind != "vm" {
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
	// Each of these is a call that put the disk on parker 90000 or moved it
	// off, and then couldn't turn the parker's protection back on. A
	// delete_disk finds the disk where an earlier detach_disk parked it.
	t.Run("attach protection restore", cleanupRefusesProtectionRestore("lifecycle_attach_disk_QEMU_AttachDisk", "lifecycle_attach_disk_Nodes_UpdateQemuConfig"))
	t.Run("detach protection restore", cleanupRefusesProtectionRestore("lifecycle_detach_disk_QEMU_AttachDisk", "lifecycle_detach_disk_Nodes_UpdateQemuConfig"))
	t.Run("delete protection restore", cleanupRefusesProtectionRestore("lifecycle_detach_disk_QEMU_AttachDisk", "lifecycle_delete_disk_Nodes_UpdateQemuConfig"))
}

// cleanupRefusesProtectionRestore returns a row whose record names parker
// 90000 through an observed step of parkerKind and then holds a planned
// protection_on write of protectionKind, which is what a call leaves when it
// couldn't turn the parker's protection back on. While the parker reads
// unprotected, cleanup must refuse with the command that puts protection
// back, even with every attestation given.
func cleanupRefusesProtectionRestore(parkerKind, protectionKind string) func(*testing.T) {
	return func(t *testing.T) {
		checkCleanupRefusesProtectionRestore(t, parkerKind, protectionKind)
	}
}

func checkCleanupRefusesProtectionRestore(t *testing.T, parkerKind, protectionKind string) {
	deps, c, j, id, _ := lifecycleFlowFixture(t)
	const parker = 90000
	c.state.configs[parker] = map[string]any{"name": "parker", "digest": "1", "tags": pve.ParkerTag, "protection": "0"}
	h, err := j.Acquire(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	r := h.Record()
	target := r.Steps[0].Target
	target.VMID, target.Node, target.IntendedVolume = parker, "n1", ""
	r.State = aj.ReconciliationRequired
	r.Reason = "lifecycle operation did not complete"
	// The journal takes a step only as planned, so the parker step is saved
	// planned and then observed before the protection write is planned.
	r.Steps = append(r.Steps, aj.Step{ID: "on-parker", Kind: parkerKind, State: aj.Planned, Attempt: r.ActiveAttempt(), Target: target})
	if err := h.Save(r); err != nil {
		t.Fatal(err)
	}
	r = h.Record()
	r.Steps[len(r.Steps)-1].State = aj.Observed
	r.Steps = append(r.Steps, aj.Step{ID: "protection-on", Kind: protectionKind, State: aj.Planned, Attempt: r.ActiveAttempt(), Target: target,
		Parameters: parkerProtectionStepParameters(map[string]any{"protection": true})})
	if err := h.Save(r); err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	record, err := CleanupStorageAllocation(t.Context(), deps, j, []string{"n1"}, cleanupAttestedDecision(id))
	if want := "qm set 90000 --protection 1"; err == nil || !strings.Contains(StorageAllocationDecisionFailure(err), want) {
		t.Fatalf("cleanup with parker %d unprotected returned %v, want a refusal that says %q", parker, err, want)
	}
	if record.State == aj.Cleaned {
		t.Fatal("cleanup closed the record while the parker's protection restore was unsettled")
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
	ctx, settlement, err := admitStorageCleanupSettlement(t.Context(), deps, h.Record(), cleanupAttestedDecision(record.ID), nil)
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
	_, err = beginStorageLifecycleMode(h, "resize_disk", ownership, false)
	if err == nil {
		t.Fatal("persisted cleanup proof authorized ordinary lifecycle")
	}
	if want := "resize_disk has unresolved mutation evidence; step pending-config (vm.Nodes.UpdateQemuConfig) is planned"; err.Error() != want {
		t.Fatalf("refusal = %q, want %q", err, want)
	}
	if !reflect.DeepEqual(before, h.Record()) {
		t.Fatal("rejected lifecycle changed journal")
	}
}

// TestStorageLifecycleRefusalNamesItsOperation pins that an ordinary disk
// operation refused for an unsettled step names itself rather than cleanup,
// while explicit cleanup, which drives delete_disk underneath, keeps naming
// cleanup, the command the operator ran.
func TestStorageLifecycleRefusalNamesItsOperation(t *testing.T) {
	proofID, proofBody, err := aj.VerificationEvidence(map[string]string{"ownership": "fresh independent test proof"})
	if err != nil {
		t.Fatal(err)
	}
	ownership := aj.Verification{EvidenceID: proofID, EvidenceJSON: proofBody, Complete: true, OwnershipVerified: true}
	const step = "step pending-config (lifecycle_attach_disk_Nodes_UpdateQemuConfig) is planned"
	cases := []struct {
		name  string
		begin func(*aj.Handle) (*storageLifecycle, error)
		want  string
	}{
		{"attach_disk", func(h *aj.Handle) (*storageLifecycle, error) {
			return beginStorageLifecycle(h, "attach_disk", ownership)
		},
			"attach_disk has unresolved mutation evidence; " + step},
		{"detach_disk", func(h *aj.Handle) (*storageLifecycle, error) {
			return beginStorageLifecycle(h, "detach_disk", ownership)
		},
			"detach_disk has unresolved mutation evidence; " + step},
		{"delete_disk", func(h *aj.Handle) (*storageLifecycle, error) {
			return beginStorageLifecycle(h, "delete_disk", ownership)
		},
			"delete_disk has unresolved mutation evidence; " + step},
		{"cleanup", func(h *aj.Handle) (*storageLifecycle, error) {
			return beginStorageLifecycleCleanup(t.Context(), h, "delete_disk", ownership)
		}, "cleanup has unresolved mutation evidence; " + step},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, j, _, record := deleteManagedFixture(t)
			appendCleanupPending(t, j, record.ID, "lifecycle_attach_disk_Nodes_UpdateQemuConfig")
			h, err := j.Acquire(t.Context(), record.ID)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := h.Close(); err != nil {
					t.Error(err)
				}
			}()
			before := h.Record()
			_, err = tc.begin(h)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("refusal = %v, want %q", err, tc.want)
			}
			if !reflect.DeepEqual(before, h.Record()) {
				t.Fatal("refused lifecycle changed journal")
			}
		})
	}
}

// TestStorageOperationSettledRejectsEmptyCaller pins that admission refuses
// before it could print a refusal with no operation in front of it.
func TestStorageOperationSettledRejectsEmptyCaller(t *testing.T) {
	_, j, _, record := deleteManagedFixture(t)
	h, err := j.Acquire(t.Context(), record.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := h.Close(); err != nil {
			t.Error(err)
		}
	}()
	proofID, proofBody, err := aj.VerificationEvidence(map[string]string{"ownership": "fresh independent test proof"})
	if err != nil {
		t.Fatal(err)
	}
	ownership := aj.Verification{EvidenceID: proofID, EvidenceJSON: proofBody, Complete: true, OwnershipVerified: true}
	before := h.Record()
	if _, err := beginStorageLifecycleContext(t.Context(), h, "delete_disk", " ", ownership, false); err == nil {
		t.Fatal("empty caller admitted")
	}
	if !reflect.DeepEqual(before, h.Record()) {
		t.Fatal("refused lifecycle changed journal")
	}
}
