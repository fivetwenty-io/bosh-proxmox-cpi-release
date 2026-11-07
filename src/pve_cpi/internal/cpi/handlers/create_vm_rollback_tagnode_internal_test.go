package handlers

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// keepFailedHADeps is rollbackHADeps with debug.keep_failed_vms on.
func keepFailedHADeps(scene *HADeleteScene, nodes *kfNodes, q *kfQEMU) Deps {
	deps := rollbackHADeps(scene, nodes, q)
	deps.Config.Debug = &config.DebugConfig{KeepFailedVMs: kfBoolPtr(true)}
	deps.RequestDirectorUUID = aaDirector
	return deps
}

// TestRollbackOnExit_KeepFailedTagsAnHAVMWhereItRunsNow creates the VM on
// pve-node1 and lets PVE HA run it on pve-node2 before the create fails. The
// keep-failed rollback tags the VM on pve-node2 and names pve-node2 in its
// error, because a VM PVE HA moved does not answer on the node it was created on.
func TestRollbackOnExit_KeepFailedTagsAnHAVMWhereItRunsNow(t *testing.T) {
	scene := NewHADeleteScene()
	scene.Manage(100, "pve-node2", "started", "started")
	scene.Comments["vm:100"] = haIdentity{Owner: aaDirector}.marker()
	nodes, q := &kfNodes{}, &kfQEMU{}
	deps := keepFailedHADeps(scene, nodes, q)
	vmCreated := true
	retErr := errors.New("stage 8: agent never came up")

	rollbackOnExit(context.Background(), deps, func() string { return "pve-node1" }, 100, kfEnv(),
		log.NewNopLogger(), &vmCreated, &retErr)

	if nodes.deleteCalls != 0 {
		t.Fatalf("keep-failed must not destroy the VM, got %d destroys", nodes.deleteCalls)
	}
	if nodes.failedTagCalls != 1 || nodes.failedTagOn != "pve-node2" {
		t.Fatalf("tag writes %d on %q, want one on pve-node2, where PVE HA runs the VM", nodes.failedTagCalls,
			nodes.failedTagOn)
	}
	if retErr == nil || !strings.Contains(retErr.Error(), `"pve-node2"`) {
		t.Fatalf("the preserve error must name pve-node2, got %v", retErr)
	}
	if !strings.Contains(retErr.Error(), "agent never came up") {
		t.Errorf("the preserve error must keep the original cause, got %v", retErr)
	}
}

// TestRollbackOnExit_KeepFailedFallsBackToThePlacedNode leaves an HA-listed VM
// out of the cluster index, so its current node cannot be proved. The tag goes
// to the node the create last placed the VM on, and the error names that node.
func TestRollbackOnExit_KeepFailedFallsBackToThePlacedNode(t *testing.T) {
	scene := NewHADeleteScene()
	scene.Manage(100, "pve-node2", "started", "started")
	scene.Do(func(s *HADeleteScene) { delete(s.IndexNode, 100) })
	scene.Comments["vm:100"] = haIdentity{Owner: aaDirector}.marker()
	nodes, q := &kfNodes{}, &kfQEMU{}
	deps := keepFailedHADeps(scene, nodes, q)
	vmCreated := true
	retErr := errors.New("stage 8: agent never came up")

	rollbackOnExit(context.Background(), deps, func() string { return "pve-node1" }, 100, kfEnv(),
		log.NewNopLogger(), &vmCreated, &retErr)

	if nodes.failedTagCalls != 1 || nodes.failedTagOn != "pve-node1" {
		t.Fatalf("tag writes %d on %q, want one on the placed node pve-node1", nodes.failedTagCalls,
			nodes.failedTagOn)
	}
	if retErr == nil || !strings.Contains(retErr.Error(), `"pve-node1"`) {
		t.Fatalf("the preserve error must name the placed node, got %v", retErr)
	}
	if events := scene.EventsSnapshot(); len(events) != 0 {
		t.Fatalf("a keep-failed rollback must write nothing to HA, got %v", events)
	}
}

// TestDisposeFailedVM_KeepFailedTagsAnHAVMWhereItRunsNow covers the panic
// path: the kept VM is tagged on the node PVE HA moved it to.
func TestDisposeFailedVM_KeepFailedTagsAnHAVMWhereItRunsNow(t *testing.T) {
	scene := NewHADeleteScene()
	scene.Manage(100, "pve-node2", "started", "started")
	scene.Comments["vm:100"] = haIdentity{Owner: aaDirector}.marker()
	nodes, q := &kfNodes{}, &kfQEMU{}
	deps := keepFailedHADeps(scene, nodes, q)

	disposeFailedVM(context.Background(), deps, func() string { return "pve-node1" }, 100, kfEnv(), log.NewNopLogger())

	if nodes.deleteCalls != 0 {
		t.Fatalf("keep-failed must not destroy the VM, got %d destroys", nodes.deleteCalls)
	}
	if nodes.failedTagCalls != 1 || nodes.failedTagOn != "pve-node2" {
		t.Fatalf("tag writes %d on %q, want one on pve-node2", nodes.failedTagCalls, nodes.failedTagOn)
	}
}
