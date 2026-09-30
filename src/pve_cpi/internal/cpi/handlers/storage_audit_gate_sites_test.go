package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// These tests pin the three refusals that used to fall through to the
// generic "complete consistent allocation audit required" error. Each now
// names the audit's findings, in the same place in its order of checks.

// TestUnsubmittedVMGenerationInspectionNamesAuditFindings is the zero-step
// resume path: a VM generation with no recorded step whose absence proof
// needs a complete audit.
func TestUnsubmittedVMGenerationInspectionNamesAuditFindings(t *testing.T) {
	deps, j, c := auditFixture(t)
	c.nodesRead.nodeNames = []string{"pve1"}
	def, err := pve.ParseStorageEntry(c.storageRead.definitions[0])
	if err != nil {
		t.Fatal(err)
	}
	plan := StorageAllocationPlan{Version: 1, Namespace: "director", AllocationKey: "agent", PolicyFingerprint: strings.Repeat("a", 64), Node: "pve1", Definitions: map[string]pve.StorageInfo{"a": def}, Targets: []StoragePlanTarget{{Role: "root", Node: "pve1", StorageID: "a", BackingKey: def.BackingKey(), VirtualBytes: 1 << 30}}}
	payload, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	args := []json.RawMessage{json.RawMessage(`"agent"`)}
	fingerprint, err := storageCallerIntentFingerprint("create_vm", args)
	if err != nil {
		t.Fatal(err)
	}
	h, err := j.AcquireVM(t.Context(), "agent", aj.Intent{IntentFingerprint: fingerprint, PolicyFingerprint: plan.PolicyFingerprint, PlanVersion: 1, Plan: payload})
	if err != nil {
		t.Fatal(err)
	}
	record := h.Record()
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	c.nodesRead.failure = errors.New("listing failed: token=secret-value")
	_, err = resumeManagedVM(t.Context(), deps, &createVMParsedArgs{agentID: "agent"}, nil, j, record, args, nil)
	message := directorMessage(err)
	if !strings.HasPrefix(message, "unsubmitted VM generation inspection refused: 1 audit issue; ") || !strings.Contains(message, `storage "a" on node "pve1"`) {
		t.Fatalf("zero-step resume refusal names no finding: %q", message)
	}
	if strings.Contains(message, "secret-value") || strings.Contains(message, "complete consistent allocation audit required") {
		t.Fatalf("refusal leaked or fell back to the generic error: %q", message)
	}
	after, err := j.Inspect(record.ID)
	if err != nil || len(after.Verifications) != 0 {
		t.Fatalf("refused inspection retained evidence: %+v %v", after.Verifications, err)
	}
}

// TestVMCleanupCompletionNamesAuditFindings drives managedVMDispositionProof,
// which both delete_vm and explicit cleanup of a deleted VM finish with.
func TestVMCleanupCompletionNamesAuditFindings(t *testing.T) {
	report := StorageAllocationAudit{Complete: false, VMScanComplete: true, Issues: []string{`storage "a" on node "pve1" could not be inspected: connection to 10.0.0.1:8006 failed`}}
	_, err := managedVMDispositionProof(context.Background(), Deps{}, report, aj.Record{ID: "allocation"}, 123, nil)
	message := directorMessage(err)
	if !strings.HasPrefix(message, "VM cleanup completion refused: 1 audit issue; storage \"a\" on node \"pve1\" could not be inspected") {
		t.Fatalf("completion refusal names no finding: %q", message)
	}
	report.addConflict("remote allocation other (VM 9) is outside recorded mutation targets", "VM 9 outside its targets")
	report.Complete, report.Issues = false, nil
	_, err = cleanupVMDeletedOwnership(context.Background(), Deps{}, aj.Record{ID: "allocation"}, report)
	if message := directorMessage(err); !strings.HasPrefix(message, "VM cleanup completion refused: 1 audit conflict; VM 9 outside its targets") {
		t.Fatalf("deleted-VM cleanup refusal names no finding: %q", message)
	}
}

// TestRetainedVMCleanupCompletionNamesAuditFindings loses audit visibility
// after the retained volume is deleted, so only the completion audit fails.
func TestRetainedVMCleanupCompletionNamesAuditFindings(t *testing.T) {
	deps, client, journal, vmID, _ := retainDeleteFixture(t)
	if _, err := HandleDeleteVM(deps).Handle(context.Background(), []json.RawMessage{planJSON(t, "777")}, jsonrpc.Context{}); err != nil {
		t.Fatal(err)
	}
	volume := strings.Split(retainedVolume(t, client), ",")[0]
	client.visibilityErrAfterDelete = errors.New("visibility lost")
	_, err := CleanupStorageAllocation(t.Context(), deps, journal, []string{"n1", "n2"}, cleanupAttestedDecision(vmID))
	message := directorMessage(err)
	if !strings.HasPrefix(message, "retained VM cleanup completion refused: ") || !strings.Contains(message, "cluster-wide VM and storage audit visibility is unproven") {
		t.Fatalf("retained completion refusal names no finding: %q", message)
	}
	if client.state.volumes[volume] != nil {
		t.Fatal("the completion gate ran before the retained volume was deleted")
	}
}
