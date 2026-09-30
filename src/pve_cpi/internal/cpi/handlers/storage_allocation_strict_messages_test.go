package handlers

import (
	"context"
	"strings"
	"testing"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
)

// These refusals stay strict when a VM moves, so their messages must name
// the VM and both nodes for an operator to act on them.

func TestManagedDiskPlanningNodesNamesTheMovedHint(t *testing.T) {
	deps := idTestDeps(newIDFakeClient(map[int]map[string]any{700: {}}))
	_, err := managedDiskPlanningNodes(context.Background(), deps, "700", []string{"pve1"}, []string{"pve2", "pve3"})
	want := "create_disk: hinted VM 700 migrated outside observed nodes: now on pve1, observed pve2, pve3; no allocation submitted"
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
	nodes, err := managedDiskPlanningNodes(context.Background(), deps, "700", []string{"pve1"}, []string{"pve1"})
	if err != nil || len(nodes) != 1 || nodes[0] != "pve1" {
		t.Fatalf("observed hint refused: %v %v", nodes, err)
	}
}

func TestPendingVMAllocationMoveNamesBothTargets(t *testing.T) {
	record := aj.Record{ID: findingAllocationID}
	step := aj.Step{Target: aj.Target{Node: "pve1", VMID: 123}}
	if err := pendingVMAllocationMoved(record, step, "pve1", 123); err != nil {
		t.Fatalf("exact target refused: %v", err)
	}
	err := pendingVMAllocationMoved(record, step, "pve2", 123)
	want := "pending VM allocation " + findingAllocationID + " moved from exact target: VM 123 observed on pve2, recorded VM 123 on pve1"
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
	if err := pendingVMAllocationMoved(record, step, "pve1", 124); err == nil || !strings.Contains(err.Error(), "VM 124 observed on pve1, recorded VM 123 on pve1") {
		t.Fatalf("VMID change not named: %v", err)
	}
}
