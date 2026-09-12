package handlers

import (
	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"testing"
)

func TestManagedDiskJournalEvidenceAssociation(t *testing.T) {
	const birth = "pool:100/birth.raw"
	const moved = "pool:200/moved.raw"
	cases := []struct {
		name            string
		state           aj.State
		steps           []aj.Step
		actual, backing string
		wantErr         bool
	}{
		{"initial", aj.ReadyToReturn, []aj.Step{{Target: aj.Target{Backing: "a", IntendedVolume: birth}}}, birth, "a", false},
		{"unrelated backing cannot authorize birth", aj.ReadyToReturn, []aj.Step{{Target: aj.Target{Backing: "a", IntendedVolume: birth}}, {Target: aj.Target{Backing: "b", IntendedVolume: "other"}}}, birth, "b", true},
		{"renamed without durable lineage", aj.ReadyToReturn, []aj.Step{{Target: aj.Target{Backing: "a", IntendedVolume: birth}}}, moved, "a", true},
		{"renamed durable lineage", aj.ReadyToReturn, []aj.Step{{Target: aj.Target{Backing: "a", IntendedVolume: birth}}, {Target: aj.Target{Backing: "a"}, VolIDs: []string{birth, moved}}}, moved, "a", false},
		{"terminal deleted", aj.Deleted, []aj.Step{{Target: aj.Target{Backing: "a", IntendedVolume: birth}}}, birth, "a", true},
		{"terminal cleaned", aj.Cleaned, []aj.Step{{Target: aj.Target{Backing: "a", IntendedVolume: birth}}}, birth, "a", true},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			err := validateManagedDiskJournalIdentity(aj.Record{State: tt.state, Steps: tt.steps}, birth, tt.actual, tt.backing)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error=%v wantErr=%v", err, tt.wantErr)
			}
		})
	}
}
