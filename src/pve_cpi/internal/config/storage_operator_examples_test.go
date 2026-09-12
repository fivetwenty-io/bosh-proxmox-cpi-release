package config_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
)

func TestMultiStorageOperatorExamples(t *testing.T) {
	root := filepath.Join("..", "..", "..", "..", "manifests", "examples", "multi-storage-placement")
	for _, name := range []string{"separate-sets", "singleton-persistent", "alternate-strategies", "regex-membership", "root-split", "shared-capacity-domain"} {
		t.Run(name, func(t *testing.T) {
			cfg, err := config.LoadFile(filepath.Join(root, name+".json"))
			if err != nil {
				t.Fatal(err)
			}
			if err = cfg.ValidateStoragePlacementAllocation(); err != nil {
				t.Fatal(err)
			}
			if cfg.EphemeralStorageSet == "" || cfg.PersistentStorageSet == "" {
				t.Fatal("role policy missing")
			}
		})
	}
	base, err := config.LoadFile(filepath.Join(root, "separate-sets.json"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "cpi-config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var entries struct {
		CPIs []struct {
			Properties map[string]any `json:"properties"`
		} `json:"cpis"`
	}
	if err = json.Unmarshal(raw, &entries); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, entry := range entries.CPIs {
		effective, _, unknown, err := config.ApplyContextOverrides(base, entry.Properties)
		if err != nil || len(unknown) != 0 {
			t.Fatalf("invalid CPI context: %v, unknown %v", err, unknown)
		}
		if err = effective.Validate(); err != nil {
			t.Fatal(err)
		}
		if effective.StorageAllocationJournalDir != base.StorageAllocationJournalDir {
			t.Fatal("context changed process-owned journal path")
		}
		if seen[effective.StoragePlacementNamespace] {
			t.Fatal("clusters share namespace")
		}
		seen[effective.StoragePlacementNamespace] = true
	}
	if len(seen) != 2 {
		t.Fatal("two independent cluster contexts required")
	}
}
