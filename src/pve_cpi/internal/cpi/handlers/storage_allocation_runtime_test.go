package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
)

func runtimePlanFixture(t *testing.T) (StoragePlanRequest, *StorageAllocationPlan, allocationjournal.Intent) {
	t.Helper()
	req, _, _ := planFixture(t, nil)
	it, err := NewStoragePlanIterator(req)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := it.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	intent, err := storageJournalIntent("create_vm", []json.RawMessage{json.RawMessage(`{"agent":"one","password":"caller-secret"}`)}, req.Selection, req.Inventory, plan)
	if err != nil {
		t.Fatal(err)
	}
	return req, plan, intent
}
func TestStorageJournalIntentSeparatesCallerAndPolicy(t *testing.T) {
	req, plan, first := runtimePlanFixture(t)
	req.Selection.Policy.Password = "config-secret"
	second, err := storageJournalIntent("create_vm", []json.RawMessage{json.RawMessage(`{"password":"rotated-secret","agent":"one"}`)}, req.Selection, req.Inventory, plan)
	if err != nil {
		t.Fatal(err)
	}
	if first.IntentFingerprint != second.IntentFingerprint {
		t.Fatal("JSON property order or credential rotation changed caller identity")
	}
	changed := *plan
	changed.PolicyFingerprint = strings.Repeat("b", 64)
	third, err := storageJournalIntent("create_vm", []json.RawMessage{json.RawMessage(`{"agent":"one","password":"caller-secret"}`)}, req.Selection, req.Inventory, &changed)
	if err != nil {
		t.Fatal(err)
	}
	if third.IntentFingerprint != first.IntentFingerprint || third.PolicyFingerprint == first.PolicyFingerprint {
		t.Fatal("policy change changed caller identity or was lost")
	}
	encoded, _ := json.Marshal(second)
	for _, secret := range []string{"caller-secret", "config-secret"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatal("journal exposed credential")
		}
	}
	for _, arg := range []string{`{} {}`, `null null`, `{`} {
		if _, err := storageJournalIntent("create_vm", []json.RawMessage{json.RawMessage(arg)}, req.Selection, req.Inventory, plan); err == nil {
			t.Fatal("malformed argument accepted")
		}
	}
}
func TestStorageMutationEvidenceAndUncertainty(t *testing.T) {
	_, plan, intent := runtimePlanFixture(t)
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	j, err := allocationjournal.Initialize(context.Background(), directory, plan.Namespace, allocationjournal.Enrollment{ClusterID: "cluster", AuthorityID: "authority", AuditID: "audit", CompleteHistoricalAudit: true, PreviousWriterFenced: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := j.Close(); err != nil {
			t.Error(err)
		}
	}()
	h, err := j.AcquireVM(context.Background(), "agent", intent)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := h.Close(); err != nil {
			t.Error(err)
		}
	}()
	target := plan.Targets[0]
	step, err := storageMutationIntent(h, "create", allocationjournal.Target{Node: target.Node, Storage: target.StorageID, Backing: target.BackingKey, VMID: 123}, plan.Charges)
	if err != nil {
		t.Fatal(err)
	}
	if got := h.Record().Steps[0]; got.State != allocationjournal.Planned || got.UPID != "" {
		t.Fatal("pre-submission intent lost")
	}
	if err := storageMutationSubmitted(h, step, "UPID:n1:task"); err != nil {
		t.Fatal(err)
	}
	if err := storageMutationObserved(h, step, []string{"a:123/vm-123-disk-0.qcow2"}, true); err != nil {
		t.Fatal(err)
	}
	record := h.Record()
	if record.Steps[0].Charges[0].AcquiredBytes != record.Steps[0].Charges[0].PlannedBytes {
		t.Fatal("readback acquisition lost")
	}
	if _, err := activeStorageAllocationPlan(record); err != nil {
		t.Fatal(err)
	}
	uncertain := storageAllocationUncertain(h, "root expansion")
	var cloud *cpierrors.Error
	if !errors.As(uncertain, &cloud) || cloud.OkToRetry() {
		t.Fatal("uncertainty allowed Director retry")
	}
	if h.Record().State != allocationjournal.ReconciliationRequired || len(h.Record().Steps[0].VolIDs) != 1 {
		t.Fatal("uncertainty erased evidence")
	}
	if err := storageMutationSubmitted(h, "missing", "task"); err == nil {
		t.Fatal("missing step accepted")
	}
}

func TestStorageJournalIntentIgnoresUnconsumedGlobalSets(t *testing.T) {
	req, plan, _ := runtimePlanFixture(t)
	req.Selection.Policy.RootStorageSet = "unused-root-policy"
	req.Selection.Policy.EphemeralStorageSet = "unused-ephemeral-policy"
	req.Selection.Policy.PersistentStorageSet = "unused-persistent-policy"
	if _, err := storageJournalIntent("create_vm", []json.RawMessage{json.RawMessage(`"agent"`)}, req.Selection, req.Inventory, plan); err != nil {
		t.Fatalf("unconsumed global bindings changed frozen inputs: %v", err)
	}
}
