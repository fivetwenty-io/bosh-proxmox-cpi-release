package handlers

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	sdk "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/client"
)

func TestManagedVMHonorsEffectiveVMIDRange(t *testing.T) {
	for _, override := range []bool{false, true} {
		for _, exhausted := range []bool{false, true} {
			t.Run(fmt.Sprintf("override=%t/exhausted=%t", override, exhausted), func(t *testing.T) { testManagedVMRange(t, override, exhausted, false) })
		}
	}
}
func TestManagedVMUnsetRangeEndPreservesConfiguredStart(t *testing.T) {
	testManagedVMRange(t, false, false, true)
}

func testManagedVMRange(t *testing.T, override, exhausted, defaultEnd bool) {
	t.Helper()
	no := false
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	cfg := &config.CPIConfig{Node: "n1", VMStorage: "a", EphemeralStorageSet: "E", StoragePlacementNamespace: "namespace", StorageAllocationJournalDir: directory, AgentMode: config.AgentModeNoAgent, StemcellStrategy: config.StemcellStrategyImport, Placement: &config.PlacementConfig{ExcludeMaintenanceNodes: &no}, StorageSets: map[string]config.StorageSet{"E": {Names: []string{"a", "b"}, Strategy: config.StoragePlacementStrategy{Name: "spread", Version: 1}}}}
	cfg.VMIDRangeStart, cfg.VMIDRangeEnd = 8000, 8001
	if defaultEnd {
		cfg.VMIDRangeStart, cfg.VMIDRangeEnd = 8998, 0
	}
	if override {
		cfg.Host, cfg.Port, cfg.User, cfg.Password = "test.invalid", 8006, "root", "test-password"
		cfg.DiskStorage, cfg.NetworkBridge = "a", "vmbr0"
		cfg.ApplyDefaults()
		effective, _, _, err := config.ApplyContextOverrides(cfg, map[string]any{"pve_vmid_range_start": 8200, "pve_vmid_range_end": 8201})
		if err != nil {
			t.Fatal(err)
		}
		if cfg.VMIDRangeStart != 8000 || cfg.VMIDRangeEnd != 8001 {
			t.Fatal("override changed base range")
		}
		cfg = effective
	}
	journal, err := aj.Initialize(t.Context(), directory, cfg.StoragePlacementNamespace, aj.Enrollment{ClusterID: "pve-root-ca-sha256:" + strings.Repeat("ab", 32), AuthorityID: "authority", AuditID: "audit", CompleteHistoricalAudit: true, PreviousWriterFenced: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := journal.Close(); err != nil {
			t.Error(err)
		}
	})
	state := &managedDiskTestState{volumes: map[string]*nodes.GetStorageContentResponse{"a:import/stemcell.qcow2": {Size: sdk.PVEInt(1 << 30), Format: "qcow2"}}}
	client := &managedVMEntryClient{diagnosticVMClient: diagnosticVMClient{managedDiskTestPVE{state: state}}, journal: journal}
	state.configs = map[int]map[string]any{cfg.VMIDRangeStart: {"name": "occupied"}}
	if exhausted {
		state.configs[cfg.VMIDRangeEnd] = map[string]any{"name": "occupied-end"}
	}
	deps := Deps{Config: cfg, PVE: client, Logger: log.NewNopLogger()}
	args := []json.RawMessage{json.RawMessage(`"entry-agent"`), json.RawMessage(`":heavy:a:import/stemcell.qcow2"`), json.RawMessage(`{"cpu":1,"ram":1024,"root_disk_size":1024}`), json.RawMessage(`{}`), json.RawMessage(`[]`), json.RawMessage(`{}`)}
	result, err := createVM(t.Context(), deps, args)
	if exhausted {
		if err == nil || client.creates != 0 {
			t.Fatalf("exhausted VMID range escaped: result=%v err=%v creates=%d", result, err, client.creates)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	expectedEnd := cfg.VMIDRangeEnd
	if defaultEnd {
		expectedEnd = 8999
	}
	values, ok := result.([]any)
	if !ok || len(values) == 0 || values[0] != strconv.Itoa(expectedEnd) {
		t.Fatalf("VM escaped configured last free ID: %v", result)
	}
}
