package handlers

import (
	"encoding/json"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	sdk "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/client"
)

// TestObserveVMPlan_ResolvesTheAntiAffinityPattern pins that the storage plan
// diagnostic resolves the ha_anti_affinity pattern the way create_vm does.
// Under pve.ha.anti_affinity on, a VM whose pattern is off gets no HA nodes
// in the diagnostic's plan, as in the create's, a VM with block gets them,
// and a pattern the create rejects fails the diagnostic too.
func TestObserveVMPlan_ResolvesTheAntiAffinityPattern(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		pattern string
		haNodes bool
		fails   bool
	}{
		"off":   {pattern: `"off"`},
		"block": {pattern: `"block"`, haNodes: true},
		"true":  {pattern: `true`, fails: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			trueVal := true
			cfg := &config.CPIConfig{Node: "n1", EphemeralStorageSet: "E", StoragePlacementNamespace: "namespace",
				AgentMode: config.AgentModeNoAgent, StemcellStrategy: config.StemcellStrategyImport,
				Placement: &config.PlacementConfig{AntiAffinity: &config.AntiAffinityConfig{Enabled: &trueVal}},
				HA:        haAntiAffinityOn(),
				StorageSets: map[string]config.StorageSet{"E": {Names: []string{"a", "b"},
					Strategy: config.StoragePlacementStrategy{Name: "spread", Version: 1}}}}
			state := &managedDiskTestState{volumes: map[string]*nodes.GetStorageContentResponse{
				"a:import/stemcell.qcow2": {Size: sdk.PVEInt(1 << 30), Format: "qcow2"}}}
			deps := Deps{Config: cfg, PVE: diagnosticVMClient{managedDiskTestPVE{state: state}}, Logger: log.NewNopLogger()}
			args := []json.RawMessage{json.RawMessage(`"agent"`), json.RawMessage(`":heavy:a:import/stemcell.qcow2"`),
				json.RawMessage(`{"cpu":1,"ram":1024,"ephemeral_disk_size_mb":1024,"ha_anti_affinity":` + tc.pattern + `}`),
				json.RawMessage(`{}`), json.RawMessage(`[]`), json.RawMessage(`{"bosh":{"instance":{"name":"web"}}}`)}
			_, _, plan, _, err := observeVMPlan(t.Context(), deps, args)
			if tc.fails {
				if err == nil {
					t.Fatal("observeVMPlan accepted a pattern that create_vm rejects")
				}
				return
			}
			if err != nil || plan == nil {
				t.Fatalf("observeVMPlan: plan %v, err %v", plan, err)
			}
			if got := len(plan.HANodes) > 0; got != tc.haNodes {
				t.Fatalf("HA nodes = %v; want some %v", plan.HANodes, tc.haNodes)
			}
		})
	}
}
