package handlers

import (
	"encoding/json"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	sdk "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/client"
)

// TestObserveVMPlan_ResolvesTheHAOptOut pins that the storage plan diagnostic
// reads the ha cloud property the way create_vm does. Under
// pve.ha.anti_affinity on, a group member that keeps HA gets HA nodes in the
// diagnostic's plan, a member whose cloud_properties set ha to false gets
// none, as in the create's plan, and a value the create rejects fails the
// diagnostic too.
func TestObserveVMPlan_ResolvesTheHAOptOut(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		ha      string
		haNodes bool
		fails   bool
	}{
		"unset":  {haNodes: true},
		"true":   {ha: `,"ha":true`, haNodes: true},
		"false":  {ha: `,"ha":false`},
		"string": {ha: `,"ha":"false"`, fails: true},
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
				json.RawMessage(`{"cpu":1,"ram":1024,"ephemeral_disk_size_mb":1024,"ha_anti_affinity":"block"` + tc.ha + `}`),
				json.RawMessage(`{}`), json.RawMessage(`[]`), json.RawMessage(`{"bosh":{"instance":{"name":"web"}}}`)}
			_, _, plan, _, err := observeVMPlan(t.Context(), deps, args)
			if tc.fails {
				if err == nil {
					t.Fatal("observeVMPlan accepted an ha value that create_vm rejects")
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
