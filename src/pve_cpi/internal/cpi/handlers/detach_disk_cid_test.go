package handlers_test

import (
	"context"
	"errors"
	"testing"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
)

// errTestDetachTransport is a generic transport-failure sentinel used by the
// CID-removal failure test to simulate an UpdateQemuConfig error.
var errTestDetachTransport = errors.New("detach_disk CID test: simulated UpdateQemuConfig failure")

// detachDepsWithNodes is detachDeps plus an injectable nodes.Service, used by
// tests that assert the bosh_attached_disks sentinel removal (or its
// absence) triggered by pve.RemoveAttachedDiskCID.
func detachDepsWithNodes(qemuSvc qemu.Service, nodesSvc nodes.Service) handlers.Deps {
	return handlers.Deps{
		Config: &config.CPIConfig{
			Node:         testNode,
			VMDiskFormat: "qcow2",
			// Sentinel-removal tests care about the CID bookkeeping, not the
			// parker lifecycle, so opt out of the parked default.
			DetachedDiskStrategy: "free",
		},
		PVE:    &mockPVEClient{qemuSvc: qemuSvc, nodesSvc: nodesSvc},
		Agent:  &mockAgentService{},
		Logger: log.NewNopLogger(),
	}
}

// TestHandleDetachDisk_RemovesRecordedCID verifies that a successful
// detach_disk removes the bosh_attached_disks entry for the detached volid
// while preserving unrelated entries.
func TestHandleDetachDisk_RemovesRecordedCID(t *testing.T) {
	t.Parallel()
	const vmCID = "100"
	// diskCID (package const) == "local-lvm:vm-9001-disk-0"

	seededDesc := `<!--BOSH:{"bosh_attached_disks":{"local-lvm:vm-9001-disk-0":"pvd-abc","local-lvm:vm-9099-disk-0":"pvd-def"}}-->`

	qemuSvc := &detachQEMUService{
		configCfg: map[string]any{
			"scsi0":       testDiskCID,
			diskSlot:      diskCID,
			"description": seededDesc,
		},
	}
	var capturedDesc string
	nodesSvc := &mockNodesService{
		updateQemuConfigFn: func(_ context.Context, _ string, _ string, params *nodes.UpdateQemuConfigParams) error {
			if params.Description != nil {
				capturedDesc = *params.Description
			}
			return nil
		},
	}
	h := handlers.HandleDetachDisk(detachDepsWithNodes(qemuSvc, nodesSvc))

	result, err := h.Handle(context.Background(), detachArgs(t, vmCID, diskCID), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != nil {
		t.Errorf("result: want nil (void), got %v", result)
	}
	if !qemuSvc.detachCalled {
		t.Fatal("DetachDisk was not called")
	}

	if capturedDesc == "" {
		t.Fatal("expected UpdateQemuConfig to be called to remove the recorded CID")
	}
	got := pve.GetAttachedDiskCIDs(capturedDesc)
	if _, exists := got[diskCID]; exists {
		t.Errorf("detached volid %q must be removed from the sentinel; got %v", diskCID, got)
	}
	if got["local-lvm:vm-9099-disk-0"] != "pvd-def" {
		t.Errorf("unrelated entry must survive removal; got %v", got)
	}
}

// TestHandleDetachDisk_NoRecordedCID_SkipsSentinelWrite verifies that when
// no bosh_attached_disks entry exists for the detached volid,
// RemoveAttachedDiskCID makes no UpdateQemuConfig call at all (matching the
// "skip API write when key absent" contract).
func TestHandleDetachDisk_NoRecordedCID_SkipsSentinelWrite(t *testing.T) {
	t.Parallel()
	const vmCID = "100"

	qemuSvc := &detachQEMUService{
		configCfg: map[string]any{
			"scsi0":  testDiskCID,
			diskSlot: diskCID,
			// No description at all → no bosh_attached_disks entry for diskCID.
		},
	}
	updateCalled := false
	nodesSvc := &mockNodesService{
		updateQemuConfigFn: func(_ context.Context, _ string, _ string, _ *nodes.UpdateQemuConfigParams) error {
			updateCalled = true
			return nil
		},
	}
	h := handlers.HandleDetachDisk(detachDepsWithNodes(qemuSvc, nodesSvc))

	_, err := h.Handle(context.Background(), detachArgs(t, vmCID, diskCID), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if updateCalled {
		t.Error("expected no UpdateQemuConfig call when no CID was recorded for this volid")
	}
}

// TestHandleDetachDisk_CIDRemoveFailureDoesNotFailDetach verifies that an
// UpdateQemuConfig failure during sentinel removal is swallowed: detach_disk
// still returns nil (void success).
func TestHandleDetachDisk_CIDRemoveFailureDoesNotFailDetach(t *testing.T) {
	t.Parallel()
	const vmCID = "100"

	seededDesc := `<!--BOSH:{"bosh_attached_disks":{"local-lvm:vm-9001-disk-0":"pvd-abc"}}-->`
	qemuSvc := &detachQEMUService{
		configCfg: map[string]any{
			"scsi0":       testDiskCID,
			diskSlot:      diskCID,
			"description": seededDesc,
		},
	}
	nodesSvc := &mockNodesService{
		updateQemuConfigFn: func(_ context.Context, _ string, _ string, _ *nodes.UpdateQemuConfigParams) error {
			return errTestDetachTransport
		},
	}
	h := handlers.HandleDetachDisk(detachDepsWithNodes(qemuSvc, nodesSvc))

	result, err := h.Handle(context.Background(), detachArgs(t, vmCID, diskCID), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("detach_disk must succeed despite CID-remove failure: %v", err)
	}
	if result != nil {
		t.Errorf("result: want nil (void), got %v", result)
	}
	if !qemuSvc.detachCalled {
		t.Error("DetachDisk should still have been called")
	}
}
