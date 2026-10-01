package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// TestManagedVMResumeRefusalNamesActiveUnsettledStep pins the create_vm resume
// refusal for an open attempt with unsettled evidence. It names the first
// unsettled step of the active attempt, never a step an earlier attempt left
// planned or a later step of the same attempt, and it stays a non-retryable
// CloudError.
func TestManagedVMResumeRefusalNamesActiveUnsettledStep(t *testing.T) {
	req, _, _ := planFixture(t, nil)
	iterator, err := NewStoragePlanIterator(req)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := iterator.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	journal, err := aj.Initialize(context.Background(), dir, plan.Namespace, aj.Enrollment{ClusterID: "cluster", AuthorityID: "authority", AuditID: "audit", CompleteHistoricalAudit: true, PreviousWriterFenced: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := journal.Close(); err != nil {
			t.Error(err)
		}
	})
	intent, err := storageJournalIntent("create_vm", []json.RawMessage{json.RawMessage(`{}`)}, req.Selection, req.Inventory, plan)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := journal.AcquireVM(context.Background(), "agent", intent)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := handle.Close(); err != nil {
			t.Error(err)
		}
	})
	identity := handle.Record().ID
	target := aj.Target{Node: plan.Node, VMID: 123}

	earlier, err := storageMutationIntent(handle, "vm.Nodes.CreateVM", target, nil)
	if err != nil {
		t.Fatal(err)
	}
	evidenceID, evidenceJSON, err := aj.VerificationEvidence(map[string]any{"scope": "no remote mutation submitted", "allocation": identity})
	if err != nil {
		t.Fatal(err)
	}
	proof := aj.AttemptVerification{Verification: aj.Verification{EvidenceID: evidenceID, EvidenceJSON: evidenceJSON, Complete: true, AbsenceVerified: true, VMAbsenceVerified: true, ArtifactDispositionVerified: true}, NoSubmissionVerified: true}
	if err := handle.BeginAttempt(handle.Record().ActivePlan(), proof); err != nil {
		t.Fatal(err)
	}
	first, err := storageMutationIntent(handle, "vm.Nodes.CreateVM", target, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := storageMutationSubmitted(handle, first, "UPID:n1:00000001:00000001:00000001:qmcreate:123:root@pam:"); err != nil {
		t.Fatal(err)
	}
	later, err := storageMutationIntent(handle, "vm.Nodes.UpdateVMConfig", target, nil)
	if err != nil {
		t.Fatal(err)
	}
	recorded := handle.Record()
	if recorded.ActiveAttempt() != 1 || managedVMAttemptClosed(recorded) {
		t.Fatalf("fixture did not leave attempt 1 open: attempts=%d", len(recorded.Attempts))
	}
	var firstStep aj.Step
	for _, step := range recorded.Steps {
		if step.ID == first {
			firstStep = step
		}
	}
	if firstStep.Attempt != 1 || firstStep.State != aj.Submitted {
		t.Fatalf("fixture step %s is not the active attempt's submitted step: %+v", first, firstStep)
	}

	deps := Deps{Config: req.Selection.Policy, Logger: log.NewNopLogger()}
	_, resumeErr := continueManagedVM(context.Background(), deps, &createVMParsedArgs{agentID: "agent"}, req.Selection, journal, handle, plan, nil)
	if resumeErr == nil {
		t.Fatal("resume with an unsettled active step was admitted")
	}
	var cpiErr *cpierrors.Error
	if !errors.As(resumeErr, &cpiErr) {
		t.Fatalf("refusal is not a CPI error: %T %v", resumeErr, resumeErr)
	}
	if cpiErr.Type() != cpierrors.TypeCloud {
		t.Errorf("refusal type = %s, want %s", cpiErr.Type(), cpierrors.TypeCloud)
	}
	if cpiErr.OkToRetry() {
		t.Error("refusal became retryable")
	}
	message := resumeErr.Error()
	want := "allocation " + identity + " requires reconciliation at unsettled recorded mutation; " + unsettledStepName(firstStep) + "; no alternate allocation was attempted"
	if message != want {
		t.Errorf("refusal = %q\nwant      %q", message, want)
	}
	if !strings.Contains(message, "step "+first+" (vm.Nodes.CreateVM) is submitted") {
		t.Errorf("refusal does not name the active attempt's first unsettled step %s: %q", first, message)
	}
	if strings.Contains(message, earlier) {
		t.Errorf("refusal names step %s from the earlier attempt: %q", earlier, message)
	}
	if strings.Contains(message, later) {
		t.Errorf("refusal names step %s instead of the first unsettled one: %q", later, message)
	}
}
