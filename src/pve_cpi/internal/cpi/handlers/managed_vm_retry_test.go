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
	for _, mode := range []string{"same-policy", "snake-frozen-fingerprint", "changed-policy", "budget-exhausted", "changed-execution", "changed-frozen-fingerprint", "added-transport", "added-ha"} {
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
	switch mode {
	case "snake-frozen-fingerprint":
		// A record that 0.10.0 through 0.10.2 opened holds the fingerprint
		// over the snake_case definitions.
		frozen, err := storageJournalFrozenInputs(req.Selection, req.Inventory, plan)
		if err != nil {
			t.Fatal(err)
		}
		if intent.FrozenInputsFingerprint, err = frozen.snakeFingerprint(); err != nil {
			t.Fatal(err)
		}
	case "changed-frozen-fingerprint":
		intent.FrozenInputsFingerprint = strings.Repeat("f", 64)
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
	switch mode {
	case "changed-execution":
		req.Selection.Policy.NetworkBridge = "changed"
	case "added-transport":
		// API transport settings set after the record froze its execution
		// leave the fallback admitted.
		req.Selection.Policy.Endpoints = []string{"pve.example:8006"}
		req.Selection.Policy.EndpointCacheDir = "/var/vcap/data/pve_cpi/endpoint-cache"
		req.Selection.Policy.APIEndpointDiscovery = config.DiscoveryModeOff
	case "added-ha":
		strict := true
		req.Selection.Policy.HA = &config.HAConfig{PinAZStrict: &strict}
	}
	next := &managedVMPlan{selection: req.Selection, collector: collector, inventory: req.Inventory, plan: &nextPlan}
	deps := Deps{Config: req.Selection.Policy, Logger: log.NewNopLogger()}
	runtime, err := beginManagedVMRetry(context.Background(), deps, &createVMParsedArgs{agentID: "agent"}, req.Selection, handle, next, proof("fresh independent admission after cleanup"))
	if mode != "same-policy" && mode != "snake-frozen-fingerprint" && mode != "added-transport" {
		if err == nil || handle.Record().ActiveAttempt() != 0 {
			t.Fatal("changed constraint or exhausted budget admitted replacement")
		}
		if (mode == "changed-execution" || mode == "added-ha") && !strings.Contains(err.Error(), "frozen VM execution inputs changed") {
			t.Fatalf("%s refused for another reason: %v", mode, err)
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
	if got := handle.Record().ActivePlan().FrozenInputsFingerprint; got != intent.FrozenInputsFingerprint {
		t.Fatalf("attempt carries frozen fingerprint %s, the record was opened with %s", got, intent.FrozenInputsFingerprint)
	}
}

// TestManagedVMRetryExcludesItsOwnAllocation pins what the retry branch passes
// as self. A retry re-plans while its own record is still in flight, and
// charging that record against the re-plan would steer the attempt away from
// the artifacts it has already retained.
func TestManagedVMRetryExcludesItsOwnAllocation(t *testing.T) {
	request := siblingPlacementFixture(t, map[string]siblingMemberCapacity{
		"a": {total: 100 * siblingPlacementGiB, available: 83 * siblingPlacementGiB},
		"b": {total: 100 * siblingPlacementGiB, available: 80 * siblingPlacementGiB},
	}, nil)
	journal := siblingPlacementJournal(t, request.Namespace)
	handle := siblingPlacementPeer(t, journal, request, "agent")
	records, err := journal.List()
	if err != nil {
		t.Fatal(err)
	}
	charge := func(self string) map[string]uint64 {
		t.Helper()
		observed := &managedVMPlan{selection: request.Selection, inventory: request.Inventory,
			request: request, snapshotStart: request.Clock()}
		if err := applyManagedVMSiblings(observed, records, self); err != nil {
			t.Fatal(err)
		}
		return observed.request.SiblingMemberBytes
	}
	if own := charge(handle.Record().ID); len(own) != 0 {
		t.Fatalf("a retry charged its own allocation %v", own)
	}
	key := siblingCapacityKey(t, request, siblingPlacementRootStorage(t, handle.Record()))
	if other := charge("alloc-other"); other[key] == 0 {
		t.Fatalf("the same record charges %v under another identifier, want its claim on %q", other, key)
	}
}
