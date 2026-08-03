package handlers_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/qemu"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/agent"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
)

// mustEncodeDiskCID calls pve.EncodeDiskCID and fails the test on error. Test
// call sites in this package always pass a non-empty bareCID; the error path
// (empty bareCID) is covered directly in internal/pve's disk_test.go.
func mustEncodeDiskCID(t *testing.T, bareCID string, meta *pve.DiskCIDMeta) string {
	t.Helper()
	got, err := pve.EncodeDiskCID(bareCID, meta)
	if err != nil {
		t.Fatalf("EncodeDiskCID(%q): unexpected error: %v", bareCID, err)
	}
	return got
}

// attachDepsWithNodes is attachDeps plus an injectable nodes.Service, used by
// tests that assert the bosh_attached_disks sentinel write (or its absence)
// triggered by pve.UpdateAttachedDiskCID.
func attachDepsWithNodes(qemuSvc qemu.Service, nodesSvc nodes.Service, ag agent.Agent) handlers.Deps {
	return handlers.Deps{
		Config: &config.CPIConfig{
			Node:         testNode,
			VMDiskFormat: "qcow2",
		},
		PVE:    &mockPVEClient{qemuSvc: qemuSvc, nodesSvc: nodesSvc},
		Agent:  ag,
		Logger: log.NewNopLogger(),
	}
}

// TestHandleAttachDisk_RecordsVerbatimCID verifies that a successful
// attach_disk records the Director's exact (envelope) disk_cid string against
// the bare volid on the VM's description sentinel.
func TestHandleAttachDisk_RecordsVerbatimCID(t *testing.T) {
	t.Parallel()
	const vmCID = "100"
	bareVolid := "local-lvm:vm-9001-disk-0"
	envelopeCID := mustEncodeDiskCID(t, bareVolid, &pve.DiskCIDMeta{Pool: "local-lvm", Node: "pve1", AZ: "az1"})

	qemuSvc := &attachQEMUService{
		attachReturnDiskID: "scsi1",
		configCfg: map[string]any{
			"scsi0": testDiskCID,
			"scsi1": bareVolid,
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
	h := handlers.HandleAttachDisk(attachDepsWithNodes(qemuSvc, nodesSvc, &captureAgent{}))

	_, err := h.Handle(context.Background(), attachArgs(t, vmCID, envelopeCID), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if capturedDesc == "" {
		t.Fatal("expected UpdateQemuConfig to be called with the attached-disk CID sentinel")
	}
	got := pve.GetAttachedDiskCIDs(capturedDesc)
	if got[bareVolid] != envelopeCID {
		t.Errorf("recorded CID: want %q, got %q (sentinel: %q)", envelopeCID, got[bareVolid], capturedDesc)
	}
}

// TestHandleAttachDisk_SentinelMergePreservesExistingKeys seeds the VM
// description with a bosh_parked_disks entry and an unrelated sentinel key,
// then verifies both survive the bosh_attached_disks write untouched.
func TestHandleAttachDisk_SentinelMergePreservesExistingKeys(t *testing.T) {
	t.Parallel()
	const vmCID = "100"
	bareVolid := "local-lvm:vm-9002-disk-0"
	envelopeCID := mustEncodeDiskCID(t, bareVolid, nil)

	seededDesc := `<!--BOSH:{"bosh_parked_disks":{"local-lvm:vm-9099-disk-0":{"disk_cid":"local-lvm:vm-9099-disk-0","parked_at":"2026-06-01T00:00:00Z","node":"pve1"}},"unknown_future_key":{"z":1}}-->`

	qemuSvc := &attachQEMUService{
		attachReturnDiskID: "scsi1",
		configCfg: map[string]any{
			"scsi0":       testDiskCID,
			"scsi1":       bareVolid,
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
	h := handlers.HandleAttachDisk(attachDepsWithNodes(qemuSvc, nodesSvc, &captureAgent{}))

	// Pass the already-encoded CID directly (attachArgs passes an already-
	// enveloped CID through unchanged) so the recorded sentinel entry is
	// asserted against the exact verbatim string the Director sent, per the
	// "record whatever the Director handed us" contract this sentinel exists
	// for.
	_, err := h.Handle(context.Background(), attachArgs(t, vmCID, envelopeCID), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(capturedDesc, "bosh_parked_disks") {
		t.Errorf("description %q must preserve bosh_parked_disks", capturedDesc)
	}
	if !strings.Contains(capturedDesc, "unknown_future_key") {
		t.Errorf("description %q must preserve unknown_future_key", capturedDesc)
	}
	got := pve.GetAttachedDiskCIDs(capturedDesc)
	if got[bareVolid] != envelopeCID {
		t.Errorf("recorded CID: want %q, got %q", envelopeCID, got[bareVolid])
	}
}

// TestHandleAttachDisk_CIDRecordFailureDoesNotFailAttach verifies that a
// sentinel-write failure (UpdateQemuConfig error) is swallowed: attach_disk
// still returns disk_hints and no error.
func TestHandleAttachDisk_CIDRecordFailureDoesNotFailAttach(t *testing.T) {
	t.Parallel()
	const vmCID = "100"
	bareVolid := "local-lvm:vm-9003-disk-0"

	qemuSvc := &attachQEMUService{
		attachReturnDiskID: "scsi1",
		configCfg: map[string]any{
			"scsi0": testDiskCID,
			"scsi1": bareVolid,
		},
	}
	nodesSvc := &mockNodesService{
		updateQemuConfigFn: func(_ context.Context, _ string, _ string, _ *nodes.UpdateQemuConfigParams) error {
			return errors.New("simulated PVE write failure")
		},
	}
	h := handlers.HandleAttachDisk(attachDepsWithNodes(qemuSvc, nodesSvc, &captureAgent{}))

	result, err := h.Handle(context.Background(), attachArgs(t, vmCID, bareVolid), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("attach_disk must succeed despite CID-record failure: %v", err)
	}
	if result == nil {
		t.Fatal("expected disk_hints result despite CID-record failure")
	}
	path := extractPath(t, result)
	if path == "" {
		t.Error("expected a non-empty device path despite CID-record failure")
	}
}

// TestHandleAttachDisk_NoNodesService_CIDRecordSkippedSilently verifies that
// attach_disk succeeds unchanged when no nodes.Service is wired at all (the
// pre-existing attachDeps helper, used by every other attach_disk test).
func TestHandleAttachDisk_NoNodesService_CIDRecordSkippedSilently(t *testing.T) {
	t.Parallel()
	const vmCID = "100"
	bareVolid := "local-lvm:vm-9004-disk-0"

	qemuSvc := &attachQEMUService{
		attachReturnDiskID: "scsi1",
		configCfg: map[string]any{
			"scsi0": testDiskCID,
			"scsi1": bareVolid,
		},
	}
	h := handlers.HandleAttachDisk(attachDeps(qemuSvc, &captureAgent{}))

	result, err := h.Handle(context.Background(), attachArgs(t, vmCID, bareVolid), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected disk_hints result")
	}
}
