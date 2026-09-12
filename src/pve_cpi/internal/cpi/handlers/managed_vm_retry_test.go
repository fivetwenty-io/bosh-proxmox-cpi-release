package handlers

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

func TestManagedVMRetryPreservesGenerationAndFrozenBounds(t *testing.T) {
	for _, mode := range []string{"same-policy", "changed-policy", "budget-exhausted", "changed-execution"} {
		t.Run(mode, func(t *testing.T) {
			testManagedVMRetryMode(t, mode)
		})
	}
}

func testManagedVMRetryMode(t *testing.T, mode string) {
	t.Helper()
	req, collector, _ := planFixture(t, func(_ *planFixtureSource, cfg *config.CPIConfig) {
		limit := 1
		if mode == "budget-exhausted" {
			limit = 0
		}
		cfg.Placement = &config.PlacementConfig{FallbackMax: &limit}
	})
	iterator, err := NewStoragePlanIterator(req)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := iterator.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	shape := &createVMShape{node: plan.Node, vmStorage: plan.Targets[0].StorageID, rootDiskKey: "virtio0", rootDiskGiB: 5, vmDiskFormat: "qcow2"}
	plan.VMExecution, err = freezeManagedVMExecution(req.Selection.Policy, shape)
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
	proof := func(scope string) aj.Verification {
		id, payload, e := aj.VerificationEvidence(map[string]any{"scope": scope, "allocation": identity})
		if e != nil {
			t.Fatal(e)
		}
		return aj.Verification{EvidenceID: id, EvidenceJSON: payload, Complete: true, AbsenceVerified: true, VMAbsenceVerified: true, ArtifactDispositionVerified: true}
	}
	if err := handle.CompleteAttempt(aj.AttemptVerification{Verification: proof("no remote mutation submitted"), NoSubmissionVerified: true}); err != nil {
		t.Fatal(err)
	}
	if !managedVMAttemptClosed(handle.Record()) {
		t.Fatal("safe cleanup checkpoint lost")
	}
	nextPlan := *plan
	nextPlan.Seed[0] = 1
	if mode == "changed-policy" {
		nextPlan.PolicyFingerprint = strings.Repeat("e", 64)
	}
	if mode == "changed-execution" {
		req.Selection.Policy.NetworkBridge = "changed"
	}
	next := &managedVMPlan{selection: req.Selection, collector: collector, inventory: req.Inventory, plan: &nextPlan}
	deps := Deps{Config: req.Selection.Policy, Logger: log.NewNopLogger()}
	runtime, err := beginManagedVMRetry(context.Background(), deps, &createVMParsedArgs{agentID: "agent"}, req.Selection, handle, next, proof("fresh independent admission after cleanup"))
	if mode != "same-policy" {
		if err == nil || handle.Record().ActiveAttempt() != 0 {
			t.Fatal("changed constraint or exhausted budget admitted replacement")
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if runtime.handle.Record().ID != identity || handle.Record().ActiveAttempt() != 1 || len(handle.Record().Attempts) != 2 {
		t.Fatal("retry replaced generation or lost history")
	}
	active, err := activeStorageAllocationPlan(handle.Record())
	if err != nil {
		t.Fatal(err)
	}
	if active.VMExecution.MaxAttempts != 2 || active.Seed[0] != 1 || handle.Record().Attempts[0].Completion == nil {
		t.Fatal("new seed/frozen budget/cleanup proof lost")
	}
}
