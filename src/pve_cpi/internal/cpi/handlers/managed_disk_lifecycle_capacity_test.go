package handlers

import (
	"context"
	"encoding/json"
	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"testing"
)

func TestManagedDiskLifecycleCapacityAfterSetRemoval(t *testing.T) {
	req, _, source := planFixture(t, nil)
	iterator, err := NewStoragePlanIterator(req)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := iterator.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	intent, err := storageJournalIntent("create_disk", []json.RawMessage{json.RawMessage(`1024`)}, req.Selection, req.Inventory, plan)
	if err != nil {
		t.Fatal(err)
	}
	record := aj.Record{ID: "01234567-89ab-4def-8123-456789abcdef", Namespace: plan.Namespace, Intent: intent}
	target := plan.Targets[0]
	// No current set definitions or role bindings survive. Admission must still
	// use the actual recorded target and the frozen plan's limits.
	deps := Deps{Config: &config.CPIConfig{}}
	charges, err := managedDiskLifecycleCapacityFrom(context.Background(), deps, record, target.Node, target.StorageID, target.BackingKey, 1<<30, source)
	if err != nil {
		t.Fatal(err)
	}
	if len(charges) != 1 || charges[0].Charge.Bytes != 1<<30 || charges[0].Charge.StorageID != target.StorageID {
		t.Fatalf("incorrect target charge: %+v", charges)
	}
	if _, err := managedDiskLifecycleCapacityFrom(context.Background(), deps, record, target.Node, target.StorageID, "different-backing", 1<<30, source); err == nil {
		t.Fatal("changed backing admitted")
	}
	for i, raw := range source.statuses[target.Node] {
		var status map[string]any
		if err := json.Unmarshal(raw, &status); err != nil {
			t.Fatal(err)
		}
		if status["storage"] == target.StorageID {
			status["avail"] = 1
			source.statuses[target.Node][i] = planJSON(t, status)
		}
	}
	if _, err := managedDiskLifecycleCapacityFrom(context.Background(), deps, record, target.Node, target.StorageID, target.BackingKey, 1<<30, source); err == nil {
		t.Fatal("insufficient actual target admitted")
	}
}
