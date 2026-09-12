package handlers

import (
	"context"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	inv "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/storageinventory"
	"testing"
)

func TestStoragePlanRecoveryDefinitionSafety(t *testing.T) {
	for _, tc := range []struct {
		name      string
		change    func(*pve.StorageInfo)
		wantError bool
	}{
		{"content order", func(d *pve.StorageInfo) { d.Content = "iso,import,images" }, false},
		{"empty node representation", func(d *pve.StorageInfo) { d.Nodes = []string{} }, false},
		{"removed content", func(d *pve.StorageInfo) { d.Content = "images,iso" }, true},
		{"added content", func(d *pve.StorageInfo) { d.Content += ",backup" }, true},
		{"restricted nodes", func(d *pve.StorageInfo) { d.Nodes = []string{"n1"} }, true},
		{"changed backing", func(d *pve.StorageInfo) { d.Export = "/other" }, true},
		{"changed server", func(d *pve.StorageInfo) { d.Server = "other" }, true},
		{"disabled", func(d *pve.StorageInfo) { d.Disabled = true }, true},
		{"changed type", func(d *pve.StorageInfo) { d.Type = "dir" }, true},
		{"changed name", func(d *pve.StorageInfo) { d.Name = "other" }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, collector, _ := planFixture(t, nil)
			iterator, err := NewStoragePlanIterator(r)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := iterator.Next(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			ledger := inv.NewLedger()
			for _, charge := range plan.Charges {
				ledger, err = ledger.WithPlanned(r.Inventory, charge.Charge)
				if err != nil {
					t.Fatal(err)
				}
			}
			id := plan.Targets[0].StorageID
			definition := plan.Definitions[id]
			tc.change(&definition)
			plan.Definitions[id] = definition
			_, _, err = RevalidateStorageAllocationPlan(context.Background(), collector, r.Inventory, r.Inventory, r.Selection, plan, ledger, r.Clock)
			if (err != nil) != tc.wantError {
				t.Fatalf("error=%v, want rejection=%v", err, tc.wantError)
			}
		})
	}
}
