package handlers

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
)

// planHealthDeps returns deps for the storage plan's compute observation on
// two nodes whose cluster status reads fail where failReads says.
func planHealthDeps(t *testing.T, failReads map[int32]bool, mutate func(*config.CPIConfig)) Deps {
	t.Helper()
	deps, client, _ := bridgeDeps(t, []map[string]any{onlineNode("pve1"), onlineNode("pve2")},
		map[string][]string{"pve1": {"vmbr0"}, "pve2": {"vmbr0"}}, "off", mutate)
	client.clusterClient.failReads = failReads
	return deps
}

// TestObserveStoragePlanCompute_RefusesWhenNodeHealthStaysUnreadable pins the
// fail-closed side of the storage plan. The gather reads the node list first
// and node health after it, so failing reads 2 and 3 fails the health read
// and its retry. With maintenance tags named, the plan must stop with a
// retriable observation error instead of scoring every node as healthy.
func TestObserveStoragePlanCompute_RefusesWhenNodeHealthStaysUnreadable(t *testing.T) {
	t.Parallel()
	deps := planHealthDeps(t, map[int32]bool{2: true, 3: true}, func(c *config.CPIConfig) {
		c.Placement.MaintenanceNodeTags = []string{"maintenance"}
	})
	_, err := ObserveStoragePlanCompute(context.Background(), deps, oneNet(), nil)
	var planErr *StoragePlanError
	if !errors.As(err, &planErr) || planErr.Kind != StoragePlanObservation {
		t.Fatalf("want a storage plan observation error, got %T %v", err, err)
	}
	if !planErr.CPI().OkToRetry() {
		t.Fatalf("the observation error must be retriable: %v", planErr.CPI())
	}
	if !strings.Contains(err.Error(), "cannot tell which nodes are in maintenance") {
		t.Fatalf("error %q does not say why", err)
	}
}

// TestObserveStoragePlanCompute_FailsOpenWhenNodeHealthIsNotRequired pins
// that with HA writes off and no maintenance tags named, the plan still treats
// every node as healthy when node health stays unreadable.
func TestObserveStoragePlanCompute_FailsOpenWhenNodeHealthIsNotRequired(t *testing.T) {
	t.Parallel()
	deps := planHealthDeps(t, map[int32]bool{2: true, 3: true}, nil)
	groups, err := ObserveStoragePlanCompute(context.Background(), deps, oneNet(), nil)
	if err != nil {
		t.Fatalf("the plan failed although node health is not required: %v", err)
	}
	nodes := 0
	for _, g := range groups {
		nodes += len(g.Nodes)
	}
	if nodes != 2 {
		t.Fatalf("plan nodes = %d, want both nodes", nodes)
	}
}
