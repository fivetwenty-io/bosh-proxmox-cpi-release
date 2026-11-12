package config_test

import (
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
)

// TestValidateRejectsANamespaceWithoutAJournalDirectory pins the one half of
// the allocation journal that config load rejects. A namespace with no journal
// directory can never create or delete a VM, and nothing can supply the
// directory later, so load refuses it. A journal directory with no namespace
// still loads, because a cpi-config entry can supply the namespace, and so do
// both halves together and neither.
func TestValidateRejectsANamespaceWithoutAJournalDirectory(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, namespace, journal string
		refused                  bool
	}{
		{"namespace without a journal directory", "director", "", true},
		{"journal directory without a namespace", "", "/var/vcap/store/pve_cpi/allocations", false},
		{"both halves", "director", "/var/vcap/store/pve_cpi/allocations", false},
		{"neither half", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := validBaseCfg()
			c.StoragePlacementNamespace, c.StorageAllocationJournalDir = tc.namespace, tc.journal
			err := c.Validate()
			if tc.refused {
				if err == nil || !strings.Contains(err.Error(), config.StorageJournalPairError) {
					t.Fatalf("Validate() = %v, want the pair refusal %q", err, config.StorageJournalPairError)
				}
				return
			}
			if err != nil {
				t.Fatalf("Validate() = %v, want nil", err)
			}
		})
	}
}

// TestContextNamespaceNeedsTheJobJournalDirectory checks the same rule on an
// entry's effective configuration. The journal directory is process-level and
// inherited, so an entry may supply a namespace for a job that sets the
// directory, and is refused when the job sets none.
func TestContextNamespaceNeedsTheJobJournalDirectory(t *testing.T) {
	t.Parallel()
	extra := map[string]any{"pve_storage_placement_namespace": "east"}

	withJournal := validBaseCfg()
	withJournal.StorageAllocationJournalDir = "/var/vcap/store/pve_cpi/allocations"
	eff, _, _, err := config.ApplyContextOverrides(withJournal, extra)
	if err != nil {
		t.Fatalf("an entry namespace over a job journal directory was refused: %v", err)
	}
	if eff.StoragePlacementNamespace != "east" ||
		eff.StorageAllocationJournalDir != withJournal.StorageAllocationJournalDir {
		t.Fatalf("effective namespace %q and journal directory %q, want east and the job's directory",
			eff.StoragePlacementNamespace, eff.StorageAllocationJournalDir)
	}

	if _, _, _, err := config.ApplyContextOverrides(validBaseCfg(), extra); err == nil ||
		!strings.Contains(err.Error(), config.StorageJournalPairError) {
		t.Fatalf("an entry namespace without a job journal directory gave %v, want the pair refusal", err)
	}
}
