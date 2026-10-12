package handlers_test

import (
	"context"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

const ignoredNodeKeyWarn = "cloud_properties.node is ignored"

func createVMWithCloudProps(t *testing.T, cloudProps map[string]any) *log.Observer {
	t.Helper()
	obsLogger, obs := log.NewObservedLogger(log.LevelWarn)
	deps := buildVMDeps(&vmMockQEMU{}, &vmMockNodes{}, &vmMockCluster{}, &vmMockAgent{})
	deps.Logger = obsLogger
	args := mkArgs("agent-node-key", testStemcellCID, cloudProps, defaultNetMap(), []string{}, map[string]any{})
	if _, err := handlers.HandleCreateVM(deps).Handle(context.Background(), args, mkCtx("node-key")); err != nil {
		t.Fatalf("create_vm: %v", err)
	}
	return obs
}

func TestCreateVM_NodeKeyInCloudProps_WarnsOnceAndIsIgnored(t *testing.T) {
	obs := createVMWithCloudProps(t, map[string]any{"cores": 1, "memory": 512, "node": "pve-other"})
	if got := countWarnsContaining(obs, ignoredNodeKeyWarn); got != 1 {
		t.Errorf("expected exactly 1 ignored-node Warn, got %d (entries: %+v)", got, obs.All())
	}
}

func TestCreateVM_NestedNodeKey_DoesNotWarn(t *testing.T) {
	obs := createVMWithCloudProps(t, map[string]any{
		"cores": 1, "memory": 512,
		"network_defaults": map[string]any{"node": "pve-other"},
	})
	if got := countWarnsContaining(obs, ignoredNodeKeyWarn); got != 0 {
		t.Errorf("nested node key must not warn, got %d (entries: %+v)", got, obs.All())
	}
}

func TestCreateVM_NoNodeKey_DoesNotWarn(t *testing.T) {
	obs := createVMWithCloudProps(t, map[string]any{"cores": 1, "memory": 512})
	if got := countWarnsContaining(obs, ignoredNodeKeyWarn); got != 0 {
		t.Errorf("no node key must not warn, got %d", got)
	}
}
