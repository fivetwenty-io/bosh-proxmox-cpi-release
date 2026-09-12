package handlers

import (
	"maps"
	"reflect"
	"testing"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
)

func TestExplicitCleanupReconcilesAnchoredDiskAfterHolderReferenceRemoval(t *testing.T) {
	deps, client, journal, id, cid := lifecycleFlowFixtureState(t, true, true)
	deps.Config.DetachedDiskStrategy = config.DetachedDiskStrategyParked
	delete(client.state.configs[777], "scsi1")
	parker := map[string]any{"name": "bosh-parker-90283", "tags": "bosh-cpi;bosh-parker", "protection": 1, "digest": "1"}
	client.state.configs[90283] = parker
	wantParker := maps.Clone(parker)
	pending := appendCleanupPending(t, journal, id, "lifecycle_delete_disk_Nodes_UpdateQemuConfig")
	record, err := journal.Inspect(id)
	if err != nil || record.State != aj.ReconciliationRequired {
		t.Fatalf("fixture lacks actual failed-cleanup state: %v %s", err, record.State)
	}
	disk, err := resolveDeleteDiskCID(t.Context(), deps, cid)
	if err != nil {
		t.Fatal(err)
	}
	if disk.holder != nil {
		t.Fatal("fixture still has a disk holder")
	}
	if _, err := unparkBeforeDelete(t.Context(), deps, disk, deps.Config.Node); err == nil {
		t.Fatal("ordinary delete lost the missing-anchor refusal")
	}
	deps.PVE = &cleanupTaskClient{Client: deps.PVE}
	result, err := CleanupStorageAllocation(t.Context(), deps, journal, []string{deps.Config.Node}, cleanupAttestedDecision(id))
	if err != nil {
		t.Fatalf("audited cleanup of free anchored disk failed: %v", err)
	}
	if result.State != aj.Cleaned || client.deletes != 1 || len(client.state.volumes) != 0 {
		t.Fatal("cleanup did not prove physical disk disposition")
	}
	if !reflect.DeepEqual(client.state.configs[90283], wantParker) || len(client.state.configs) != 2 {
		t.Fatal("cleanup changed or created holder infrastructure")
	}
	if !reflect.DeepEqual(result.Steps[1], pending) || result.CID != cid {
		t.Fatal("cleanup rewrote pending history or anchored identity")
	}
	if !deps.Config.ParkedAnchorStrictValue() {
		t.Fatal("cleanup relaxed shared anchor policy")
	}
}
