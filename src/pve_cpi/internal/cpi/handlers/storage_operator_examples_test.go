package handlers

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
)

func TestMultiStorageCloudConfigExamples(t *testing.T) {
	root := filepath.Join("..", "..", "..", "..", "..", "manifests", "examples", "multi-storage-placement")
	cfg, err := config.LoadFile(filepath.Join(root, "alternate-strategies.json"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "cloud-config.json"))
	if err != nil {
		t.Fatal(err)
	}
	type entry struct {
		Name            string         `json:"name"`
		CloudProperties map[string]any `json:"cloud_properties"`
	}
	var cloud struct {
		VMTypes   []entry `json:"vm_types"`
		DiskTypes []entry `json:"disk_types"`
	}
	if err = json.Unmarshal(raw, &cloud); err != nil {
		t.Fatal(err)
	}
	if len(cloud.VMTypes) != 3 || len(cloud.DiskTypes) != 3 {
		t.Fatal("examples must exercise each strategy on both roles")
	}
	for _, vm := range cloud.VMTypes {
		selected, err := ResolveStoragePlacementSelectors(cfg, "create_vm", vm.CloudProperties, true)
		if err != nil {
			t.Fatalf("%s: %v", vm.Name, err)
		}
		if !selected.SetManaged || !selected.BundleVMDisks || selected.Ephemeral == nil || selected.Root == nil {
			t.Fatalf("%s lost bundled root/ephemeral policy", vm.Name)
		}
	}
	for _, disk := range cloud.DiskTypes {
		selected, err := ResolveStoragePlacementSelectors(cfg, "create_disk", disk.CloudProperties, false)
		if err != nil {
			t.Fatalf("%s: %v", disk.Name, err)
		}
		if !selected.SetManaged || selected.Persistent == nil {
			t.Fatalf("%s lost persistent policy", disk.Name)
		}
	}
}
