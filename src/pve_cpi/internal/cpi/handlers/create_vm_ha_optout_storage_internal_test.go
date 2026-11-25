package handlers

import (
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	inv "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/storageinventory"
)

// optOutISOPlan returns a storage plan request over node-local storages only,
// with the config-drive ISO pinned to the node-local root storage and
// pve.require_shared_iso_for_ha on. The request takes its shared-ISO rule
// from ConfigureStorageVMPlanRequest for parsed, so the test reads the rule
// the create would plan with.
func optOutISOPlan(t *testing.T, parsed *createVMParsedArgs) (StoragePlanRequest, *inv.Collector) {
	t.Helper()
	request, collector, _ := planFixture(t, func(source *planFixtureSource, cfg *config.CPIConfig) {
		cfg.EphemeralStorageSet = ""
		cfg.VMStorage = managedCloneStorage
		cfg.ISOStorage = managedCloneStorage
		requireShared := true
		cfg.RequireSharedISOForHA = &requireShared
		source.defs = nil
		for _, id := range []string{"a", "b", "p", "source"} {
			source.defs = append(source.defs, planJSON(t, map[string]any{
				"storage": id, "type": "dir", "shared": 0, "path": "/" + id, "nodes": "n1,n2",
				"content": "images,iso,import",
			}))
		}
	})
	configured, err := ConfigureStorageVMPlanRequest(Deps{Config: request.Selection.Policy}, parsed,
		StoragePlanRequest{}, 1<<20)
	if err != nil {
		t.Fatalf("ConfigureStorageVMPlanRequest: %v", err)
	}
	request.ISOBytes = configured.ISOBytes
	request.OriginalISOStorage = configured.OriginalISOStorage
	request.ISOFollowRoot = configured.ISOFollowRoot
	request.RequireSharedISO = configured.RequireSharedISO
	return request, collector
}

// TestStoragePlan_SharedISORuleSkipsAnOptedOutVM pins that the storage plan
// asks for a shared config-drive ISO under pve.require_shared_iso_for_ha only
// for a VM that may join PVE HA. The plain create path checks the ISO only
// for a VM with an HA registration feature, and an opted-out VM has none, so
// the plan places that VM's ISO on node-local storage and the plan's
// revalidation before each mutation accepts it. A VM that keeps HA still gets
// the shared-ISO refusal from both.
func TestStoragePlan_SharedISORuleSkipsAnOptedOutVM(t *testing.T) {
	t.Parallel()
	for name, haOff := range map[string]bool{"keeps HA": false, "opted out": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			parsed := &createVMParsedArgs{cloudProps: createVMCloudProps{haOff: haOff}}
			request, collector := optOutISOPlan(t, parsed)
			if request.RequireSharedISO == haOff {
				t.Fatalf("RequireSharedISO = %v; want %v", request.RequireSharedISO, !haOff)
			}
			it, err := NewStoragePlanIterator(request)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := it.Next(t.Context())
			if !haOff {
				if err == nil {
					t.Fatalf("plan %+v; a VM that keeps HA must not get a node-local ISO", plan)
				}
				if !strings.Contains(strings.Join(it.rejections, "; "), "ISO target is not shared") {
					t.Fatalf("rejections %v; want the shared-ISO refusal", it.rejections)
				}
				return
			}
			if err != nil {
				t.Fatalf("an opted-out VM's node-local ISO was refused: %v", err)
			}
			iso, ok := managedVMRoleTarget(plan, storageRoleISO)
			if !ok || iso.StorageID != managedCloneStorage {
				t.Fatalf("ISO target %+v; want the node-local storage %s", iso, managedCloneStorage)
			}
			ledger := inv.NewLedger()
			for _, c := range plan.Charges {
				if ledger, err = ledger.WithPlanned(request.Inventory, c.Charge); err != nil {
					t.Fatal(err)
				}
			}
			cfg := request.Selection.Policy
			m := &managedVMAllocation{deps: Deps{Config: cfg}, parsed: parsed, prepared: &managedVMPlan{
				collector: collector, inventory: request.Inventory, selection: request.Selection, plan: plan,
				ledger: ledger,
			}}
			if err := m.revalidate(t.Context()); err != nil {
				t.Fatalf("revalidation refused an opted-out VM's node-local ISO: %v", err)
			}
			m.parsed = &createVMParsedArgs{}
			if err := m.revalidate(t.Context()); err == nil {
				t.Fatal("revalidation accepted a node-local ISO for a VM that keeps HA")
			}
		})
	}
}
