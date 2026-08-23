// Package handlers -- internal tests for cleanupVM's foreign-disk protection:
// a rollback purge destroys every disk the VM config references, so a
// persistent disk the failed create already attached must be detached to
// safety first, and a detach failure must abort the purge entirely (an
// orphaned VM is recoverable; a purged persistent volume is not).
package handlers

import (
	"context"
	"errors"
	"testing"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
)

// --------------------------------------------------------------------------
// rbfdNodesStub -- nodes.Service fake recording DeleteQemu calls and the
// order they arrive in relative to detaches (via the shared *[]string log).
// --------------------------------------------------------------------------

type rbfdNodesStub struct {
	sdknodes.Service // embedded nil: panics on any unconfigured method

	callLog     *[]string
	deleteCalls int
	tagWrites   int
}

func (n *rbfdNodesStub) DeleteQemu(
	_ context.Context, _, _ string, _ *sdknodes.DeleteQemuParams,
) (*sdknodes.DeleteQemuResponse, error) {
	n.deleteCalls++
	*n.callLog = append(*n.callLog, "delete")
	return &sdknodes.DeleteQemuResponse{}, nil
}

func (n *rbfdNodesStub) UpdateQemuConfig(_ context.Context, _, _ string, _ *sdknodes.UpdateQemuConfigParams) error {
	n.tagWrites++
	*n.callLog = append(*n.callLog, "tag")
	return nil
}

// --------------------------------------------------------------------------
// rbfdQEMUStub -- qemu.Service fake: Stop no-ops, Config replays a scripted
// sequence (last entry repeats), DetachDisk records and returns detachErr.
// --------------------------------------------------------------------------

type rbfdQEMUStub struct {
	qemu.Service // embedded nil: panics on any unconfigured method

	callLog     *[]string
	configs     []map[string]any
	configCalls int
	configErr   error
	detachErr   error
	detaches    []string
}

func (q *rbfdQEMUStub) Stop(_ context.Context, _ string, _ int) (string, error) { return "", nil }

func (q *rbfdQEMUStub) Config(_ context.Context, _ string, _ int) (map[string]any, error) {
	if q.configErr != nil {
		return nil, q.configErr
	}
	idx := q.configCalls
	if idx >= len(q.configs) {
		idx = len(q.configs) - 1
	}
	q.configCalls++
	if idx < 0 {
		return map[string]any{}, nil
	}
	return q.configs[idx], nil
}

func (q *rbfdQEMUStub) DetachDisk(_ context.Context, _ string, _ int, slot string) error {
	q.detaches = append(q.detaches, slot)
	*q.callLog = append(*q.callLog, "detach:"+slot)
	return q.detachErr
}

type rbfdClient struct {
	pve.Client
	nodes *rbfdNodesStub
	qemu  *rbfdQEMUStub
}

func (c *rbfdClient) Nodes() sdknodes.Service  { return c.nodes }
func (c *rbfdClient) QEMU() qemu.Service       { return c.qemu }
func (c *rbfdClient) Cluster() cluster.Service { return newNAStub() }
func (c *rbfdClient) Pools() pve.PoolService   { return nil }

func rbfdDeps(qemuStub *rbfdQEMUStub, nodesStub *rbfdNodesStub) Deps {
	return Deps{
		Config: &config.CPIConfig{Node: "pve01", DiskStorage: "zfs-1"},
		PVE:    &rbfdClient{nodes: nodesStub, qemu: qemuStub},
		Logger: log.NewNopLogger(),
	}
}

// foreignCfg is a VM config for VMID 6031 carrying a foreign legacy
// persistent disk on an active slot: the volume's embedded VMID (15689)
// differs from the VM's own, which is exactly what create_disk's synthetic
// allocation produces.
func rbfdForeignCfg() map[string]any {
	return map[string]any{
		"scsi0": "zfs-1:vm-6031-disk-0,size=32G",
		"scsi1": "zfs-1:vm-15689-disk-0,size=10G",
	}
}

func rbfdCleanCfg() map[string]any {
	return map[string]any{
		"scsi0": "zfs-1:vm-6031-disk-0,size=32G",
	}
}

// TestCleanupVM_ForeignDisk_DetachedBeforePurge: a rollback of a VM that
// already has a persistent disk attached must detach it BEFORE DeleteQemu,
// so the purge cannot take the volume.
func TestCleanupVM_ForeignDisk_DetachedBeforePurge(t *testing.T) {
	callLog := []string{}
	qemuStub := &rbfdQEMUStub{
		callLog: &callLog,
		// Scan read sees the foreign disk; confirm re-read and the unusedN
		// guard read see it gone after the detach.
		configs: []map[string]any{rbfdForeignCfg(), rbfdCleanCfg(), rbfdCleanCfg()},
	}
	nodesStub := &rbfdNodesStub{callLog: &callLog}
	deps := rbfdDeps(qemuStub, nodesStub)

	cleanupVM(context.Background(), deps, "pve01", 6031, nil, log.NewNopLogger())

	if len(qemuStub.detaches) != 1 || qemuStub.detaches[0] != "scsi1" {
		t.Fatalf("expected exactly one detach of scsi1, got %v", qemuStub.detaches)
	}
	if nodesStub.deleteCalls != 1 {
		t.Fatalf("expected 1 DeleteQemu call, got %d", nodesStub.deleteCalls)
	}
	if len(callLog) < 2 || callLog[0] != "detach:scsi1" || callLog[len(callLog)-1] != "delete" {
		t.Errorf("expected detach before delete, got order %v", callLog)
	}
}

// TestCleanupVM_ForeignDisk_DetachFails_PurgeRefused: when the persistent
// disk cannot be detached, the rollback must NOT purge the VM -- data loss is
// worse than an orphaned VM the Director or an operator can recover.
func TestCleanupVM_ForeignDisk_DetachFails_PurgeRefused(t *testing.T) {
	callLog := []string{}
	qemuStub := &rbfdQEMUStub{
		callLog:   &callLog,
		configs:   []map[string]any{rbfdForeignCfg(), rbfdForeignCfg()},
		detachErr: errors.New("some persistent PVE failure"),
	}
	nodesStub := &rbfdNodesStub{callLog: &callLog}
	deps := rbfdDeps(qemuStub, nodesStub)

	cleanupVM(context.Background(), deps, "pve01", 6031, nil, log.NewNopLogger())

	if nodesStub.deleteCalls != 0 {
		t.Fatalf("expected NO DeleteQemu call when the foreign-disk detach fails, got %d", nodesStub.deleteCalls)
	}
}

// TestCleanupVM_ForeignDisk_DetachFails_TaggedWhenEnvPresent: the refused
// purge should still tag the preserved VM (bosh-create-failed + identity)
// when a deploy identity is available, so operators can find it.
func TestCleanupVM_ForeignDisk_DetachFails_TaggedWhenEnvPresent(t *testing.T) {
	callLog := []string{}
	qemuStub := &rbfdQEMUStub{
		callLog:   &callLog,
		configs:   []map[string]any{rbfdForeignCfg(), rbfdForeignCfg()},
		detachErr: errors.New("some persistent PVE failure"),
	}
	nodesStub := &rbfdNodesStub{callLog: &callLog}
	deps := rbfdDeps(qemuStub, nodesStub)
	env := map[string]any{"bosh": map[string]any{"group": "postgres"}}

	cleanupVM(context.Background(), deps, "pve01", 6031, env, log.NewNopLogger())

	if nodesStub.deleteCalls != 0 {
		t.Fatalf("expected NO DeleteQemu call when the foreign-disk detach fails, got %d", nodesStub.deleteCalls)
	}
	if nodesStub.tagWrites != 1 {
		t.Errorf("expected the preserved VM to be tagged once, got %d tag writes", nodesStub.tagWrites)
	}
}

// TestCleanupVM_PmxcfsConfigMissing_PurgeProceeds: pmxcfs reports a vanished
// VM as a 500 with "Configuration file ... does not exist" prose rather than
// a 404. The guard must treat that exactly like NotFound -- the VM is gone,
// there is nothing to protect, and the (idempotent) purge must proceed
// instead of spuriously preserving a non-existent VM.
func TestCleanupVM_PmxcfsConfigMissing_PurgeProceeds(t *testing.T) {
	callLog := []string{}
	qemuStub := &rbfdQEMUStub{
		callLog:   &callLog,
		configErr: errors.New("Configuration file 'nodes/pve01/qemu-server/6031.conf' does not exist"),
	}
	nodesStub := &rbfdNodesStub{callLog: &callLog}
	deps := rbfdDeps(qemuStub, nodesStub)

	cleanupVM(context.Background(), deps, "pve01", 6031, nil, log.NewNopLogger())

	if len(qemuStub.detaches) != 0 {
		t.Errorf("expected no detach attempts for a vanished VM, got %v", qemuStub.detaches)
	}
	if nodesStub.deleteCalls != 1 {
		t.Fatalf("expected the idempotent purge to proceed (1 DeleteQemu call), got %d", nodesStub.deleteCalls)
	}
}

// TestCleanupVM_NilConfig_ForeignDiskPresent_PurgeRefused: the shared rollback
// helper is reachable with deps.Config unset; without config the transfer
// guard cannot run, so a foreign disk on an active slot must still refuse the
// purge rather than proceed and destroy the volume.
func TestCleanupVM_NilConfig_ForeignDiskPresent_PurgeRefused(t *testing.T) {
	callLog := []string{}
	qemuStub := &rbfdQEMUStub{
		callLog: &callLog,
		configs: []map[string]any{rbfdForeignCfg()},
	}
	nodesStub := &rbfdNodesStub{callLog: &callLog}
	deps := Deps{
		PVE:    &rbfdClient{nodes: nodesStub, qemu: qemuStub},
		Logger: log.NewNopLogger(),
	}

	cleanupVM(context.Background(), deps, "pve01", 6031, nil, log.NewNopLogger())

	if nodesStub.deleteCalls != 0 {
		t.Fatalf("expected NO DeleteQemu call with nil Config and a foreign disk attached, got %d", nodesStub.deleteCalls)
	}
	if len(qemuStub.detaches) != 0 {
		t.Errorf("expected no detach attempts with nil Config (no parker config to park with), got %v", qemuStub.detaches)
	}
}
