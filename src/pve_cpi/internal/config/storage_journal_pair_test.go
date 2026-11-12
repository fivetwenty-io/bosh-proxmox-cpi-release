package config_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
)

// TestLoadRejectsANamespaceWithoutAJournalDirectory pins the one half of the
// allocation journal that config load rejects in the job's own configuration.
// A namespace with no journal directory can never create or delete a VM, and
// nothing can supply the directory later, so load refuses it. A journal
// directory with no namespace still loads, because a cpi-config entry can
// supply the namespace, and so do both halves together and neither. Validate
// alone accepts every case, because it also runs on each cpi-config entry's
// effective configuration, where the journal gates refuse instead.
func TestLoadRejectsANamespaceWithoutAJournalDirectory(t *testing.T) {
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
			if err := c.Validate(); err != nil {
				t.Fatalf("Validate() = %v, want nil, because only load checks the pair", err)
			}
			raw, err := json.Marshal(c)
			if err != nil {
				t.Fatal(err)
			}
			_, err = config.Load(bytes.NewReader(raw))
			if tc.refused {
				if err == nil || !strings.Contains(err.Error(), config.StorageJournalPairError) {
					t.Fatalf("Load() = %v, want the pair refusal %q", err, config.StorageJournalPairError)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load() = %v, want nil", err)
			}
		})
	}
}

// TestContextNamespaceAppliesWithoutAJobJournalDirectory checks that an
// entry's namespace applies whether or not the job sets the journal directory.
// The directory is process-level and inherited, so an entry over a job without
// one carries a namespace and no directory. The entry still applies, so its
// calls that answer from PVE alone keep working, and the journal gates refuse
// its create_vm, delete_vm, and journal-managed disk calls with a text that
// names the entry.
func TestContextNamespaceAppliesWithoutAJobJournalDirectory(t *testing.T) {
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

	eff, _, _, err = config.ApplyContextOverrides(validBaseCfg(), extra)
	if err != nil {
		t.Fatalf("an entry namespace without a job journal directory was refused: %v", err)
	}
	if eff.StoragePlacementNamespace != "east" || eff.StorageAllocationJournalDir != "" {
		t.Fatalf("effective namespace %q and journal directory %q, want east and none",
			eff.StoragePlacementNamespace, eff.StorageAllocationJournalDir)
	}
}
