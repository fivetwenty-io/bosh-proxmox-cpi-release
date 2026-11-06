package handlers

import (
	"strings"
	"testing"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// TestAllocationDecisionRefusesToAdoptForeignVolume pins that adopt never
// takes ownership of a volume the client's shared-volume guard would call
// foreign. The fixture's VM 123 adopts under the default bands (see
// TestAllocationDecisionAdoptsVMMovedOnSharedStorage); with the VM band moved
// to 5000-5999, its volumes name an owner outside every band.
func TestAllocationDecisionRefusesToAdoptForeignVolume(t *testing.T) {
	deps, j, c, record, _ := resumeVMFixture(t)
	moveResumeVM(c)
	cfg := *deps.Config
	cfg.VMIDRangeStart, cfg.VMIDRangeEnd = 5000, 5999
	deps.Config = &cfg
	_, err := ApplyStorageAllocationDecision(t.Context(), deps, j, []string{"pve1", "pve2"}, StorageAllocationDecision{Action: "adopt", AllocationID: record.ID, ExpectedCID: record.CID, DecisionID: "foreign-adoption"})
	if err == nil {
		t.Fatal("adopt took ownership of a volume whose owner lies outside every band")
	}
	for _, want := range []string{"adoption refused", "a:123/vm-123-disk-0.qcow2"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q lacks %q", err, want)
		}
	}
	after, inspectErr := j.Inspect(record.ID)
	if inspectErr != nil {
		t.Fatal(inspectErr)
	}
	if after.State != record.State {
		t.Errorf("refused adoption moved the record from %s to %s", record.State, after.State)
	}
	if len(c.descWrites) != 0 || len(c.destroyed) != 0 {
		t.Fatal("refused adoption mutated PVE")
	}
}

func TestAdoptForeignVolume(t *testing.T) {
	t.Parallel()
	cfg := &config.CPIConfig{StoragePlacementNamespace: "namespace"}
	ours := "vm-50000-bosh-" + pve.AllocationNamespaceLocator("namespace") + "-alloc-0b0c8a52-6a1f-4c3e-9b1d-2f4e6a8c0d1e.raw"
	theirs := "vm-150-bosh-" + pve.AllocationNamespaceLocator("elsewhere") + "-alloc-0b0c8a52-6a1f-4c3e-9b1d-2f4e6a8c0d1e.raw"
	step := func(intended string, volids ...string) aj.Step {
		return aj.Step{Target: aj.Target{IntendedVolume: intended}, VolIDs: volids}
	}
	tests := []struct {
		name    string
		steps   []aj.Step
		foreign string
	}{
		{name: "in-band volumes", steps: []aj.Step{step("nfs:vm-150-disk-0", "nfs:150/vm-150-disk-0.qcow2")}},
		{name: "our locator outside the bands", steps: []aj.Step{step("nfs:" + ours)}},
		{name: "an observed volume outside the bands", steps: []aj.Step{step("nfs:vm-150-disk-0", "nfs:50000/vm-50000-disk-0.qcow2")}, foreign: "nfs:50000/vm-50000-disk-0.qcow2"},
		{name: "an intended volume with another namespace's locator", steps: []aj.Step{step("nfs:" + theirs)}, foreign: "nfs:" + theirs},
		{name: "preservation work for another allocation", steps: []aj.Step{{Target: aj.Target{External: true, IntendedVolume: "nfs:vm-50000-disk-0"}}}},
		{name: "a volume with no storage prefix", steps: []aj.Step{step("vm-50000-disk-0")}, foreign: "vm-50000-disk-0"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, foreign := adoptForeignVolume(cfg, aj.Record{Kind: allocationKindDisk, Steps: tc.steps})
			if foreign != (tc.foreign != "") || got != tc.foreign {
				t.Errorf("adoptForeignVolume = %q, %v; want %q", got, foreign, tc.foreign)
			}
		})
	}
}

func TestAdoptableRecordRefusesForeignVolume(t *testing.T) {
	t.Parallel()
	cfg := &config.CPIConfig{}
	decision := StorageAllocationDecision{ExpectedCID: "cid"}
	inBand := aj.Record{Kind: allocationKindDisk, State: aj.ReadyToReturn, CID: "cid", Steps: []aj.Step{{VolIDs: []string{"nfs:vm-9000-disk-0"}}}}
	foreign := inBand
	foreign.Steps = []aj.Step{{VolIDs: []string{"nfs:vm-50000-disk-0"}}}
	if !adoptableRecord(cfg, inBand, decision) {
		t.Error("an in-band record is not adoptable")
	}
	if adoptableRecord(cfg, foreign, decision) {
		t.Error("a record naming a foreign volume is adoptable")
	}
	if _, ok := adoptSettleableRecord(cfg, foreign, decision); ok {
		t.Error("a record naming a foreign volume is settleable")
	}
	stalled := foreign
	stalled.State = aj.Planned
	stalled.Reason = "lifecycle attach_disk admitted; completion pending"
	if _, ok := adoptSettleableRecord(cfg, stalled, decision); ok {
		t.Error("a stalled record naming a foreign volume is settleable")
	}
	stalled.Steps = inBand.Steps
	if operation, ok := adoptSettleableRecord(cfg, stalled, decision); !ok || operation != "attach_disk" {
		t.Errorf("a stalled in-band record = %q, %v; want attach_disk settleable", operation, ok)
	}
}
