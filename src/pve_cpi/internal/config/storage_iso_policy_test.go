package config_test

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
)

func TestISOStoragePolicyCapturePreservesOriginalAndLegacyDefaults(t *testing.T) {
	for _, original := range []string{"", "local", "fixed-iso"} {
		for _, follow := range []bool{false, true} {
			t.Run(original+"/follow="+map[bool]string{false: "false", true: "true"}[follow], func(t *testing.T) {
				cfg := &config.CPIConfig{ISOStorage: original, ISOStorageFollowVMStorage: &follow, VMStorage: "legacy-vm"}
				cfg.ApplyDefaults()
				wantEffective := original
				if original == "" {
					wantEffective = "local"
				}
				if cfg.ISOStorage != wantEffective || cfg.OriginalISOStorage() != wantEffective {
					t.Fatalf("default effective/original = %q/%q", cfg.ISOStorage, cfg.OriginalISOStorage())
				}
				cfg.ISOStorage = "legacy-resolved"
				cfg.CaptureISOStoragePolicy()
				cfg.ApplyDefaults()
				if cfg.OriginalISOStorage() != wantEffective {
					t.Fatal("repeated capture overwrote original choice")
				}
				selected := cfg.WithResolvedISOStorage("selected-root-iso")
				if selected.ISOStorage != "selected-root-iso" || selected.OriginalISOStorage() != wantEffective || cfg.ISOStorage != "legacy-resolved" {
					t.Fatal("resolved request cloned changed source policy")
				}
				if selected.ISOStorageFollowVMStorageEnabled() != follow {
					t.Fatal("cloned changed following policy")
				}
			})
		}
	}
}

func TestISOStoragePolicyUncapturedAndJSONSnapshotContract(t *testing.T) {
	cfg := &config.CPIConfig{ISOStorage: "literal-pool"}
	if cfg.OriginalISOStorage() != "literal-pool" {
		t.Fatal("uncaptured literal lost ISO policy")
	}
	cloned := cfg.WithResolvedISOStorage("effective-pool")
	if cloned.OriginalISOStorage() != "literal-pool" {
		t.Fatal("request helper failed to capture literal")
	}
	encoded, err := json.Marshal(cloned)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "literal-pool") || strings.Contains(string(encoded), "isoStorageOriginal") || strings.Contains(string(encoded), "iso_storage_original") {
		t.Fatal("private bookkeeping serialized")
	}
	var snapshot config.CPIConfig
	if err = json.Unmarshal(encoded, &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.OriginalISOStorage() != "effective-pool" {
		t.Fatal("unexpected restoration of private fields from JSON")
	}
	cloned.SetISOStoragePolicy("new-pin")
	if cloned.OriginalISOStorage() != "new-pin" || cloned.ISOStorage != "new-pin" || cfg.OriginalISOStorage() != "literal-pool" {
		t.Fatal("explicit replacement changed shared original")
	}
}

func TestISOStoragePolicyConcurrentOverridesRemainIsolated(t *testing.T) {
	base := &config.CPIConfig{Host: "pve.example", User: "cpi", APIToken: "test-only-token", VMStorage: "vm", DiskStorage: "disk", NetworkBridge: "vmbr0", ISOStorage: "local"}
	base.ApplyDefaults()
	base.ISOStorage = "legacy-resolved"
	choices := []string{"fixed-a", "fixed-b", "", "local"}
	var wg sync.WaitGroup
	for _, choice := range choices {
		wg.Go(func() {
			for range 20 {
				effective, _, _, err := config.ApplyContextOverrides(base, map[string]any{"pve_iso_storage": choice})
				if err != nil {
					t.Error(err)
					return
				}
				if effective.ISOStorage != choice || effective.OriginalISOStorage() != choice {
					t.Errorf("override %q lost original/effective choice", choice)
				}
				copied := effective.WithResolvedISOStorage("request-selected")
				if copied.OriginalISOStorage() != choice {
					t.Errorf("override %q lost during cloned", choice)
				}
			}
		})
	}
	wg.Wait()
	inherited, _, _, err := config.ApplyContextOverrides(base, map[string]any{"pve_host": "alias.example"})
	if err != nil {
		t.Fatal(err)
	}
	if inherited.OriginalISOStorage() != "local" || inherited.ISOStorage != "legacy-resolved" {
		t.Fatal("inherited override lost original policy or changed legacy effective value")
	}
	if base.ISOStorage != "legacy-resolved" || base.OriginalISOStorage() != "local" {
		t.Fatal("concurrent requests mutated base")
	}
}

func TestISOStoragePolicyCopyIsolatesNewStorageMaps(t *testing.T) {
	cfg := &config.CPIConfig{ISOStorage: "local", StorageSets: map[string]config.StorageSet{"e": {Names: []string{"nfs-a"}}}}
	cloned := cfg.WithResolvedISOStorage("nfs-a")
	entry := cloned.StorageSets["e"]
	entry.Names[0] = "changed"
	cloned.StorageSets["e"] = entry
	if cfg.StorageSets["e"].Names[0] != "nfs-a" {
		t.Fatal("request cloned shares mutable storage placement map")
	}
}
