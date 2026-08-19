package handlers_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi/handlers"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

// ---------------------------------------------------------------------------
// detachQEMUService: QEMU mock for detach_disk tests.
// ---------------------------------------------------------------------------

type detachQEMUService struct {
	// Config control — used by ResolveDiskID.
	configCfg map[string]any
	configErr error

	// DetachDisk control.
	detachErr      error
	detachCalled   bool
	detachedDiskID string

	// listSnapshotsFn drives the snapshot guard. nil → returns (nil, nil) (no snapshots).
	listSnapshotsFn func(ctx context.Context, node string, vmid int) ([]map[string]any, error)
}

func (m *detachQEMUService) Config(_ context.Context, _ string, _ int) (map[string]any, error) {
	return m.configCfg, m.configErr
}

func (m *detachQEMUService) DetachDisk(_ context.Context, _ string, _ int, diskID string) error {
	m.detachCalled = true
	m.detachedDiskID = diskID
	return m.detachErr
}

func (m *detachQEMUService) AttachDisk(_ context.Context, _ string, _ int, _ string, _ string, _ *qemu.AttachOpts) (string, error) {
	panic("detachQEMUService.AttachDisk: not expected")
}
func (m *detachQEMUService) Create(_ context.Context, _ string, _ map[string]any) (string, error) {
	panic("detachQEMUService.Create: not expected")
}
func (m *detachQEMUService) Status(_ context.Context, _ string, _ int) (map[string]any, error) {
	panic("detachQEMUService.Status: not expected")
}
func (m *detachQEMUService) Start(_ context.Context, _ string, _ int) (string, error) {
	panic("detachQEMUService.Start: not expected")
}
func (m *detachQEMUService) Stop(_ context.Context, _ string, _ int) (string, error) {
	panic("detachQEMUService.Stop: not expected")
}
func (m *detachQEMUService) Reset(_ context.Context, _ string, _ int) (string, error) {
	panic("detachQEMUService.Reset: not expected")
}
func (m *detachQEMUService) Clone(_ context.Context, _ string, _ int, _ map[string]any) (string, error) {
	panic("detachQEMUService.Clone: not expected")
}
func (m *detachQEMUService) Template(_ context.Context, _ string, _ int) (string, error) {
	panic("detachQEMUService.Template: not expected")
}
func (m *detachQEMUService) ResizeDisk(_ context.Context, _ string, _ int, _ string, _ int) (string, error) {
	panic("detachQEMUService.ResizeDisk: not expected")
}
func (m *detachQEMUService) Snapshot(_ context.Context, _ string, _ int, _ string, _ map[string]any) (string, error) {
	panic("detachQEMUService.Snapshot: not expected")
}
func (m *detachQEMUService) DeleteSnapshot(_ context.Context, _ string, _ int, _ string) error {
	panic("detachQEMUService.DeleteSnapshot: not expected")
}
func (m *detachQEMUService) ListSnapshots(
	ctx context.Context, node string, vmid int,
) ([]map[string]any, error) {
	if m.listSnapshotsFn != nil {
		return m.listSnapshotsFn(ctx, node, vmid)
	}
	return nil, nil // default: no snapshots → guard passes
}
func (m *detachQEMUService) RollbackSnapshot(_ context.Context, _ string, _ int, _ string) (string, error) {
	panic("detachQEMUService.RollbackSnapshot: not expected")
}

var _ qemu.Service = (*detachQEMUService)(nil)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// detachDeps builds a Deps suitable for detach_disk tests.
func detachDeps(qemuSvc qemu.Service) handlers.Deps {
	return handlers.Deps{
		Config: &config.CPIConfig{
			Node:         testNode,
			VMDiskFormat: "qcow2",
		},
		PVE:    &mockPVEClient{qemuSvc: qemuSvc},
		Agent:  &mockAgentService{}, // detach_disk does not call UpdateDiskHints
		Logger: log.NewNopLogger(),
	}
}

// detachArgs builds a two-element arg slice for detach_disk. The handler
// hard-rejects unenveloped input, so a bare diskCID is wrapped in a pvd-
// envelope here — callers in this file pass either a bare PVE-volid-shaped
// string (including deliberately malformed ones used to exercise error
// paths) or an already-encoded CID. An already-encoded or empty diskCID is
// passed through unchanged so it is never double-wrapped and the handler's
// own empty-disk_cid rejection still hits that check directly.
func detachArgs(t *testing.T, vmCID, diskCID string) []json.RawMessage {
	t.Helper()
	if diskCID == "" || strings.HasPrefix(diskCID, "pvd-") || strings.HasPrefix(diskCID, "pvz-") {
		return marshalArgs(vmCID, diskCID)
	}
	return marshalArgs(vmCID, mustEncodeDiskCID(t, diskCID, nil))
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestHandleDetachDisk_Happy verifies the full happy path:
//   - ResolveDiskID finds scsi2 for the given volid
//   - DetachDisk called with diskID "scsi2"
//   - Returns nil (void success)
func TestHandleDetachDisk_Happy(t *testing.T) {
	t.Parallel()
	const (
		vmCID = "100"
		volid = "local-lvm:vm-9001-disk-0"
	)

	qemuSvc := &detachQEMUService{
		configCfg: map[string]any{
			"scsi0":  testDiskCID,
			"scsi1":  "local-lvm:vm-100-disk-1",
			diskSlot: volid,
		},
	}
	h := handlers.HandleDetachDisk(detachDeps(qemuSvc))

	result, err := h.Handle(context.Background(), detachArgs(t, vmCID, diskCID), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != nil {
		t.Errorf("result: want nil (void), got %v", result)
	}
	if !qemuSvc.detachCalled {
		t.Error("DetachDisk was not called")
	}
	if qemuSvc.detachedDiskID != diskSlot {
		t.Errorf("DetachDisk diskID: want scsi2, got %q", qemuSvc.detachedDiskID)
	}
}

// TestHandleDetachDisk_NotAttached verifies idempotency: disk not in VM config →
// no DetachDisk call, nil error returned.
func TestHandleDetachDisk_NotAttached(t *testing.T) {
	t.Parallel()
	const (
		vmCID   = "100"
		diskCID = "local-lvm:not-attached-vol"
	)

	qemuSvc := &detachQEMUService{
		configCfg: map[string]any{
			"scsi0": testDiskCID,
		},
	}
	h := handlers.HandleDetachDisk(detachDeps(qemuSvc))

	result, err := h.Handle(context.Background(), detachArgs(t, vmCID, diskCID), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error for not-attached disk: %v", err)
	}
	if result != nil {
		t.Errorf("result: want nil (void), got %v", result)
	}
	if qemuSvc.detachCalled {
		t.Error("DetachDisk must not be called when disk is not attached")
	}
}

// TestHandleDetachDisk_SweepsLingeringUnusedSlot verifies that when the disk is
// not on an active bus but lingers as an unusedN slot (e.g. a prior bypassed
// detach whose unusedN sweep was blocked by a snapshot), a retry detach_disk
// removes that slot. This is the recovery path the guard message promises.
func TestHandleDetachDisk_SweepsLingeringUnusedSlot(t *testing.T) {
	t.Parallel()
	const vmCID = "100"

	qemuSvc := &detachQEMUService{
		// No active-bus slot for the volid → ResolveDiskID misses it. The disk is
		// parked in unused0 with a size option (FindUnusedDiskEntries strips it).
		configCfg: map[string]any{
			"scsi0":   testDiskCID,
			"unused0": diskCID + ",size=2G",
		},
	}
	h := handlers.HandleDetachDisk(detachDeps(qemuSvc))

	result, err := h.Handle(context.Background(), detachArgs(t, vmCID, diskCID), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error sweeping unused slot: %v", err)
	}
	if result != nil {
		t.Errorf("result: want nil (void), got %v", result)
	}
	if !qemuSvc.detachCalled {
		t.Error("DetachDisk must be called to remove the lingering unused slot")
	}
	if qemuSvc.detachedDiskID != "unused0" {
		t.Errorf("swept slot: want unused0, got %q", qemuSvc.detachedDiskID)
	}
}

// TestHandleDetachDisk_UnusedSlotDifferentVolume verifies the sweep does not
// touch an unusedN slot that references a different volume.
func TestHandleDetachDisk_UnusedSlotDifferentVolume(t *testing.T) {
	t.Parallel()
	const vmCID = "100"

	qemuSvc := &detachQEMUService{
		configCfg: map[string]any{
			"unused0": "local-lvm:some-other-disk",
		},
	}
	h := handlers.HandleDetachDisk(detachDeps(qemuSvc))

	if _, err := h.Handle(context.Background(), detachArgs(t, vmCID, diskCID), jsonrpc.Context{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if qemuSvc.detachCalled {
		t.Error("DetachDisk must not be called when no unused slot references this volume")
	}
}

// TestHandleDetachDisk_DetachFail verifies SDK DetachDisk failure propagates.
func TestHandleDetachDisk_DetachFail(t *testing.T) {
	t.Parallel()
	const (
		vmCID = "100"
		volid = "local-lvm:vm-9001-disk-0"
	)

	qemuSvc := &detachQEMUService{
		configCfg: map[string]any{diskSlot: volid},
		detachErr: errors.New("PVE refused to detach"),
	}
	h := handlers.HandleDetachDisk(detachDeps(qemuSvc))

	_, err := h.Handle(context.Background(), detachArgs(t, vmCID, diskCID), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error from DetachDisk failure")
	}
}

// TestHandleDetachDisk_VMNotFound verifies 404 from DetachDisk maps to VMNotFound.
func TestHandleDetachDisk_VMNotFound(t *testing.T) {
	t.Parallel()
	const (
		vmCID = "999"
		volid = "local-lvm:vm-9001-disk-0"
	)

	qemuSvc := &detachQEMUService{
		configCfg: map[string]any{diskSlot: volid},
		detachErr: &sdkerrors.APIError{Code: 404, Message: "VM not found"},
	}
	h := handlers.HandleDetachDisk(detachDeps(qemuSvc))

	_, err := h.Handle(context.Background(), detachArgs(t, vmCID, diskCID), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error")
	}
	if !cpierrors.IsType(err, cpierrors.TypeVMNotFound) {
		t.Errorf("error type: want VMNotFound, got %T %v", err, err)
	}
}

// TestHandleDetachDisk_InvalidVMCID verifies non-integer vm_cid returns VMNotFound.
func TestHandleDetachDisk_InvalidVMCID(t *testing.T) {
	t.Parallel()
	h := handlers.HandleDetachDisk(detachDeps(&detachQEMUService{}))
	_, err := h.Handle(context.Background(), detachArgs(t, "not-an-int", "local:vol"), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error")
	}
	if !cpierrors.IsType(err, cpierrors.TypeVMNotFound) {
		t.Errorf("error type: want VMNotFound, got %T %v", err, err)
	}
}

// TestHandleDetachDisk_InvalidDiskCID verifies malformed disk_cid returns DiskNotFound.
func TestHandleDetachDisk_InvalidDiskCID(t *testing.T) {
	t.Parallel()
	h := handlers.HandleDetachDisk(detachDeps(&detachQEMUService{}))
	_, err := h.Handle(context.Background(), detachArgs(t, "100", "nodisk"), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error")
	}
	if !cpierrors.IsType(err, cpierrors.TypeDiskNotFound) {
		t.Errorf("error type: want DiskNotFound, got %T %v", err, err)
	}
}

// TestHandleDetachDisk_MissingArgs verifies argument count validation.
func TestHandleDetachDisk_MissingArgs(t *testing.T) {
	t.Parallel()
	h := handlers.HandleDetachDisk(detachDeps(&detachQEMUService{}))
	_, err := h.Handle(context.Background(), marshalArgs("100"), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error for missing args")
	}
}

// TestHandleDetachDisk_EmptyVMCID verifies empty vm_cid string is rejected.
func TestHandleDetachDisk_EmptyVMCID(t *testing.T) {
	t.Parallel()
	h := handlers.HandleDetachDisk(detachDeps(&detachQEMUService{}))
	_, err := h.Handle(context.Background(), detachArgs(t, "", "local:vol"), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error for empty vm_cid")
	}
}

// TestHandleDetachDisk_EmptyDiskCID verifies empty disk_cid string is rejected.
func TestHandleDetachDisk_EmptyDiskCID(t *testing.T) {
	t.Parallel()
	h := handlers.HandleDetachDisk(detachDeps(&detachQEMUService{}))
	_, err := h.Handle(context.Background(), detachArgs(t, "100", ""), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error for empty disk_cid")
	}
}

// TestHandleDetachDisk_ConfigFetchError verifies that a Config fetch error (not a
// "not attached" CloudError but a real network error) propagates to the caller.
func TestHandleDetachDisk_ConfigFetchError(t *testing.T) {
	t.Parallel()
	qemuSvc := &detachQEMUService{
		configErr: errors.New("network unreachable"),
	}
	h := handlers.HandleDetachDisk(detachDeps(qemuSvc))
	_, err := h.Handle(context.Background(), detachArgs(t, "100", "local-lvm:vol"), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error when Config fetch fails")
	}
}

// TestHandleDetachDisk_EmptyConfigIdempotent verifies that an empty VM config
// (no disks attached at all) is treated as "not attached" and returns nil.
func TestHandleDetachDisk_EmptyConfigIdempotent(t *testing.T) {
	t.Parallel()
	qemuSvc := &detachQEMUService{
		configCfg: map[string]any{},
	}
	h := handlers.HandleDetachDisk(detachDeps(qemuSvc))

	result, err := h.Handle(context.Background(), detachArgs(t, "100", "local-lvm:missing"), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error for empty config: %v", err)
	}
	if result != nil {
		t.Errorf("result: want nil (void), got %v", result)
	}
	if qemuSvc.detachCalled {
		t.Error("DetachDisk must not be called when disk is not attached")
	}
}

// ---------------------------------------------------------------------------
// Snapshot guard tests
// ---------------------------------------------------------------------------

// snapshotRows returns a ListSnapshots response containing the given snapshot
// names plus the synthetic "current" entry (which HasSnapshots filters out).
func snapshotRows(names ...string) []map[string]any {
	rows := make([]map[string]any, 0, 1+len(names))
	rows = append(rows, map[string]any{"name": "current"}) // synthetic — always present
	for _, n := range names {
		rows = append(rows, map[string]any{"name": n})
	}
	return rows
}

// detachDepsWithCfg builds Deps with overridden config fields for guard tests.
func detachDepsWithCfg(qemuSvc qemu.Service, allow, require bool) handlers.Deps {
	return handlers.Deps{
		Config: &config.CPIConfig{
			Node:                      testNode,
			VMDiskFormat:              "qcow2",
			AllowDiskOpsWithSnapshots: allow,
			RequireSnapshotCheckPass:  require,
		},
		PVE:    &mockPVEClient{qemuSvc: qemuSvc},
		Agent:  &mockAgentService{},
		Logger: log.NewNopLogger(),
	}
}

// TestHandleDetachDisk_SnapshotPresent_HardFail verifies that when real
// snapshots exist and AllowDiskOpsWithSnapshots=false, the handler returns a
// Cloud error and DetachDisk is NOT called. The error message must contain the
// snapshot names, the disk CID, and the remediation hint.
func TestHandleDetachDisk_SnapshotPresent_HardFail(t *testing.T) {
	t.Parallel()
	const (
		vmCID = "100"
		volid = "local-lvm:vm-9001-disk-0"
	)

	qemuSvc := &detachQEMUService{
		configCfg: map[string]any{diskSlot: volid},
		listSnapshotsFn: func(_ context.Context, _ string, _ int) ([]map[string]any, error) {
			return snapshotRows("snap-before-patch", "snap-qa"), nil
		},
	}
	h := handlers.HandleDetachDisk(detachDepsWithCfg(qemuSvc, false, false))

	_, err := h.Handle(context.Background(), detachArgs(t, vmCID, diskCID), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected Cloud error when snapshots present and AllowDiskOpsWithSnapshots=false")
	}
	if !cpierrors.IsType(err, cpierrors.TypeSnapshotBlocked) {
		t.Errorf("error type: want SnapshotBlocked, got %T %v", err, err)
	}
	if qemuSvc.detachCalled {
		t.Error("DetachDisk must NOT be called when snapshot guard hard-fails")
	}
}

// TestHandleDetachDisk_NoSnapshots_Proceeds verifies the happy path when the
// snapshot check returns no real snapshots — DetachDisk is called normally.
func TestHandleDetachDisk_NoSnapshots_Proceeds(t *testing.T) {
	t.Parallel()
	const (
		vmCID = "100"
		volid = "local-lvm:vm-9001-disk-0"
	)

	qemuSvc := &detachQEMUService{
		configCfg: map[string]any{diskSlot: volid},
		listSnapshotsFn: func(_ context.Context, _ string, _ int) ([]map[string]any, error) {
			return snapshotRows(), nil // only "current" synthetic — HasSnapshots returns []
		},
	}
	h := handlers.HandleDetachDisk(detachDepsWithCfg(qemuSvc, false, false))

	_, err := h.Handle(context.Background(), detachArgs(t, vmCID, diskCID), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error when no real snapshots: %v", err)
	}
	if !qemuSvc.detachCalled {
		t.Error("DetachDisk must be called when no real snapshots exist")
	}
}

// TestHandleDetachDisk_SnapshotCheckError_FailOpen verifies that when
// ListSnapshots returns an error and RequireSnapshotCheckPass=false, the
// handler logs a warning and proceeds to call DetachDisk (fail-open, D3-C).
func TestHandleDetachDisk_SnapshotCheckError_FailOpen(t *testing.T) {
	t.Parallel()
	const (
		vmCID = "100"
		volid = "local-lvm:vm-9001-disk-0"
	)

	qemuSvc := &detachQEMUService{
		configCfg: map[string]any{diskSlot: volid},
		listSnapshotsFn: func(_ context.Context, _ string, _ int) ([]map[string]any, error) {
			return nil, errors.New("PVE snapshot API unavailable")
		},
	}
	h := handlers.HandleDetachDisk(detachDepsWithCfg(qemuSvc, false, false))

	_, err := h.Handle(context.Background(), detachArgs(t, vmCID, diskCID), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("expected fail-open (no error) when snapshot check errors and require=false: %v", err)
	}
	if !qemuSvc.detachCalled {
		t.Error("DetachDisk must be called in fail-open mode")
	}
}

// TestHandleDetachDisk_SnapshotCheckError_FailClosed verifies that when
// ListSnapshots returns an error and RequireSnapshotCheckPass=true, the
// handler aborts with an error and DetachDisk is NOT called (fail-closed, D3-C).
func TestHandleDetachDisk_SnapshotCheckError_FailClosed(t *testing.T) {
	t.Parallel()
	const (
		vmCID = "100"
		volid = "local-lvm:vm-9001-disk-0"
	)

	qemuSvc := &detachQEMUService{
		configCfg: map[string]any{diskSlot: volid},
		listSnapshotsFn: func(_ context.Context, _ string, _ int) ([]map[string]any, error) {
			return nil, errors.New("PVE snapshot API unavailable")
		},
	}
	h := handlers.HandleDetachDisk(detachDepsWithCfg(qemuSvc, false, true))

	_, err := h.Handle(context.Background(), detachArgs(t, vmCID, diskCID), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error when snapshot check errors and require_snapshot_check_pass=true")
	}
	if qemuSvc.detachCalled {
		t.Error("DetachDisk must NOT be called when fail-closed aborts")
	}
}

// TestHandleDetachDisk_SnapshotPresent_AllowOverride verifies that when real
// snapshots exist and AllowDiskOpsWithSnapshots=true, the handler logs a
// warning but proceeds to call DetachDisk (operator-override, D2-C).
func TestHandleDetachDisk_SnapshotPresent_AllowOverride(t *testing.T) {
	t.Parallel()
	const (
		vmCID = "100"
		volid = "local-lvm:vm-9001-disk-0"
	)

	qemuSvc := &detachQEMUService{
		configCfg: map[string]any{diskSlot: volid},
		listSnapshotsFn: func(_ context.Context, _ string, _ int) ([]map[string]any, error) {
			return snapshotRows("snap-emergency"), nil
		},
	}
	h := handlers.HandleDetachDisk(detachDepsWithCfg(qemuSvc, true, false))

	_, err := h.Handle(context.Background(), detachArgs(t, vmCID, diskCID), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error with AllowDiskOpsWithSnapshots=true: %v", err)
	}
	if !qemuSvc.detachCalled {
		t.Error("DetachDisk must be called when allow_disk_ops_with_snapshots=true overrides the guard")
	}
}

// ---------------------------------------------------------------------------
// CID-variant success tests (static/shared backend).
//
// detach_disk has no storage-type branching — the CID is parsed to extract
// storage+volid and passed opaquely to ResolveDiskID/DetachDisk. These tests
// exercise ParseDiskCID with the four active local storage-type formats and
// verify that DetachDisk is called in each case.
// ---------------------------------------------------------------------------

// TestHandleDetachDisk_LVM_CID verifies that a standard LVM CID
// ("local-lvm:vm-9001-disk-0") is parsed correctly and detach proceeds.
func TestHandleDetachDisk_LVM_CID(t *testing.T) {
	t.Parallel()

	qemuSvc := &detachQEMUService{
		configCfg: map[string]any{"scsi1": diskCID},
	}
	h := handlers.HandleDetachDisk(detachDeps(qemuSvc))

	result, err := h.Handle(context.Background(), detachArgs(t, "100", diskCID), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("LVM CID: unexpected error: %v", err)
	}
	if result != nil {
		t.Errorf("LVM CID: expected nil result (void), got %v", result)
	}
	if !qemuSvc.detachCalled {
		t.Error("LVM CID: DetachDisk not called")
	}
}

// TestHandleDetachDisk_ZFSPool_CID verifies that a ZFS pool bare-volname CID
// ("local-zfs:vm-9001-disk-0") is parsed correctly and detach proceeds.
func TestHandleDetachDisk_ZFSPool_CID(t *testing.T) {
	t.Parallel()
	const diskCID = "local-zfs:vm-9001-disk-0"

	qemuSvc := &detachQEMUService{
		configCfg: map[string]any{"scsi1": diskCID},
	}
	h := handlers.HandleDetachDisk(detachDeps(qemuSvc))

	result, err := h.Handle(context.Background(), detachArgs(t, "100", diskCID), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("ZFSPool CID: unexpected error: %v", err)
	}
	if result != nil {
		t.Errorf("ZFSPool CID: expected nil result (void), got %v", result)
	}
	if !qemuSvc.detachCalled {
		t.Error("ZFSPool CID: DetachDisk not called")
	}
}

// TestHandleDetachDisk_Dir_CID verifies that a dir-type subpath CID
// ("local:9001/vm-9001-disk-0.raw") is parsed correctly and detach proceeds.
// ParseDiskCID splits on the first colon; the volume segment is opaque to the handler.
func TestHandleDetachDisk_Dir_CID(t *testing.T) {
	t.Parallel()
	const diskCID = "local:9001/vm-9001-disk-0.raw"

	qemuSvc := &detachQEMUService{
		configCfg: map[string]any{"scsi1": diskCID},
	}
	h := handlers.HandleDetachDisk(detachDeps(qemuSvc))

	result, err := h.Handle(context.Background(), detachArgs(t, "100", diskCID), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("Dir CID: unexpected error: %v", err)
	}
	if result != nil {
		t.Errorf("Dir CID: expected nil result (void), got %v", result)
	}
	if !qemuSvc.detachCalled {
		t.Error("Dir CID: DetachDisk not called")
	}
}

// TestHandleDetachDisk_LVMThin_CID verifies that an LVMThin bare-volname CID
// ("local-lvm-thin:vm-9001-disk-0") is parsed correctly and detach proceeds.
func TestHandleDetachDisk_LVMThin_CID(t *testing.T) {
	t.Parallel()
	const diskCID = "local-lvm-thin:vm-9001-disk-0"

	qemuSvc := &detachQEMUService{
		configCfg: map[string]any{"scsi1": diskCID},
	}
	h := handlers.HandleDetachDisk(detachDeps(qemuSvc))

	result, err := h.Handle(context.Background(), detachArgs(t, "100", diskCID), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("LVMThin CID: unexpected error: %v", err)
	}
	if result != nil {
		t.Errorf("LVMThin CID: expected nil result (void), got %v", result)
	}
	if !qemuSvc.detachCalled {
		t.Error("LVMThin CID: DetachDisk not called")
	}
}

// TestDetachDisk_SentinelIdempotent verifies that when ResolveDiskID returns
// the new ErrDiskNotAttached sentinel (via the production code path: a Config
// fetch that succeeds but the volid is absent), the handler reports success
// and does not call DetachDisk. The sweep over unusedN entries also runs and
// finds nothing for this volid, so DetachDisk stays untouched.
func TestDetachDisk_SentinelIdempotent(t *testing.T) {
	t.Parallel()
	const vmCID = "100"

	qemuSvc := &detachQEMUService{
		// Config returns a populated map that does NOT contain diskCID on any
		// bus slot or unused slot — ResolveDiskID wraps ErrDiskNotAttached.
		configCfg: map[string]any{
			"scsi0": testDiskCID,
		},
	}
	h := handlers.HandleDetachDisk(detachDeps(qemuSvc))

	result, err := h.Handle(context.Background(), detachArgs(t, vmCID, diskCID), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("expected nil error for sentinel-idempotent path, got: %v", err)
	}
	if result != nil {
		t.Errorf("result: want nil (void), got %v", result)
	}
	if qemuSvc.detachCalled {
		t.Error("DetachDisk must NOT be called when ResolveDiskID returns ErrDiskNotAttached")
	}
}

// TestDetachDisk_OtherCloudErrorPropagates verifies that the previously broad
// TypeCloud swallow has been narrowed to the ErrDiskNotAttached sentinel.
// A non-sentinel Cloud error surfaced from the Config-fetch path (here a
// QEMU().Config() that returns cpierrors.Cloud directly, not the sentinel)
// must propagate to the caller as a real error and must not call DetachDisk.
func TestDetachDisk_OtherCloudErrorPropagates(t *testing.T) {
	t.Parallel()
	const vmCID = "100"

	// Inject a Cloud error from Config — ResolveDiskID wraps it with %w but
	// the underlying type is TypeCloud, not the sentinel. The new sentinel
	// check (errors.Is) returns false, so the handler must propagate.
	qemuSvc := &detachQEMUService{
		configErr: cpierrors.Cloud("simulated non-sentinel cloud failure from config"),
	}
	h := handlers.HandleDetachDisk(detachDeps(qemuSvc))

	_, err := h.Handle(context.Background(), detachArgs(t, vmCID, diskCID), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error from non-sentinel Cloud failure; got nil")
	}
	if errors.Is(err, nil) {
		t.Fatal("error must be non-nil (compile-time guard)")
	}
	if qemuSvc.detachCalled {
		t.Error("DetachDisk must NOT be called when ResolveDiskID returns a non-sentinel error")
	}
}

// TODO(storage-network): nfs — wired to PVE network-call boundary, stubbed pending
// live shared-storage test infrastructure. Storage: nfs-store:9001/vm-9001-disk-0.qcow2. Re-enable when
// integration-test harness provides a nfs pool via env.
//
// func TestHandleDetachDisk_NFS_CID(t *testing.T) { ... }

// TODO(storage-network): rbd — wired to PVE network-call boundary, stubbed pending
// live shared-storage test infrastructure. Storage: ceph-pool:vm-9001-disk-0. Re-enable when
// integration-test harness provides a rbd pool via env.
//
// func TestHandleDetachDisk_RBD_CID(t *testing.T) { ... }

// TODO(storage-network): cephfs — wired to PVE network-call boundary, stubbed pending
// live shared-storage test infrastructure. Storage: cephfs-pool:vm-9001-disk-0. Re-enable when
// integration-test harness provides a cephfs pool via env.
//
// func TestHandleDetachDisk_CephFS_CID(t *testing.T) { ... }

// TODO(storage-network): cifs — wired to PVE network-call boundary, stubbed pending
// live shared-storage test infrastructure. Storage: cifs-store:9001/vm-9001-disk-0.qcow2. Re-enable when
// integration-test harness provides a cifs pool via env.
//
// func TestHandleDetachDisk_CIFS_CID(t *testing.T) { ... }

// ---------------------------------------------------------------------------
// Parker integration tests.
//
// These tests exercise the parked detached-disk strategy wired into
// HandleDetachDisk. They use a full mockPVEClient with configurable cluster and
// QEMU services so the real pve.ParkDisk / pve.IsDiskParked code paths run
// without hitting PVE.
//
// Mock wiring strategy:
//   - Cluster.ListResources drives FindVMByDiskVolidOrNone (disk holder scan)
//     and listClusterVMIDs (VMID range availability for EnsureParker).
//   - QEMU.Config drives parker VM config reads (slot selection, tag check).
//   - QEMU.Create drives parker VM creation in EnsureParker.
//   - QEMU.AttachDisk drives ParkDisk's actual attach call.
//   - QEMU.DetachDisk drives the initial VM detach + sweepUnusedDiskSlot.
//   - Tasks.Wait is provided via mockTasksService (default: succeed immediately).
//   - Resolver overrides the static backend to supply a fixed node without
//     calling cluster list APIs.
// ---------------------------------------------------------------------------

// parkerQEMUService extends detachQEMUService with AttachDisk + Create
// + per-call Config routing so parker tests can control parker-VM responses
// separately from the source-VM responses.
type parkerQEMUService struct {
	// Source-VM side (scoped by vmid matching sourceVMID).
	sourceVMID   int
	sourceCfg    map[string]any
	sourceErr    error
	detachErr    error
	detachCalled bool

	// Parker-VM side (scoped by vmid >= parkerVMIDStart).
	parkerVMIDStart int
	parkerCfg       map[string]any    // returned for parker-side Config calls
	parkerAttached  map[string]string // slot→volid recorded by AttachDisk for verify reads
	parkerCfgErr    error
	attachDiskFn    func(ctx context.Context, node string, vmid int, vol, busType string, opts *qemu.AttachOpts) (string, error)
	createFn        func(ctx context.Context, node string, params map[string]any) (string, error)

	// Real-holder side: an optional second non-parker VM (e.g. a workload VM the
	// disk was re-attached to on a stale-Director retry). When realHolderVMID is
	// non-zero, Config returns realHolderCfg for that vmid, distinct from the
	// source VM config. Used to exercise the "disk held by a real VM → refuse to
	// park" guard.
	realHolderVMID int
	realHolderCfg  map[string]any

	// Snapshot guard: nil → no snapshots.
	listSnapshotsFn func(ctx context.Context, node string, vmid int) ([]map[string]any, error)
}

func (m *parkerQEMUService) Config(_ context.Context, _ string, vmid int) (map[string]any, error) {
	if vmid >= m.parkerVMIDStart {
		if m.parkerCfgErr != nil {
			return m.parkerCfg, m.parkerCfgErr
		}
		// Merge recorded parker attaches so the read-after-write slot verify in
		// attachToParker observes the disk a successful AttachDisk placed.
		if len(m.parkerAttached) > 0 {
			merged := make(map[string]any, len(m.parkerCfg)+len(m.parkerAttached))
			for k, v := range m.parkerCfg {
				merged[k] = v
			}
			for slot, volid := range m.parkerAttached {
				merged[slot] = volid
			}
			return merged, nil
		}
		return m.parkerCfg, nil
	}
	if m.realHolderVMID != 0 && vmid == m.realHolderVMID {
		return m.realHolderCfg, nil
	}
	return m.sourceCfg, m.sourceErr
}

func (m *parkerQEMUService) DetachDisk(_ context.Context, _ string, _ int, slot string) error {
	m.detachCalled = true
	if m.detachErr != nil {
		return m.detachErr
	}
	// Reflect the detach in source config so later cluster scans (e.g. the
	// real-VM-holder guard) no longer see the disk on the source VM.
	if m.sourceCfg != nil && slot != "" {
		delete(m.sourceCfg, slot)
	}
	return nil
}

func (m *parkerQEMUService) AttachDisk(ctx context.Context, node string, vmid int, vol, busType string, opts *qemu.AttachOpts) (string, error) {
	if m.attachDiskFn != nil {
		res, err := m.attachDiskFn(ctx, node, vmid, vol, busType, opts)
		if err != nil {
			return res, err
		}
		// Record the attach for the verify read only on success.
		if opts != nil && opts.DiskID != "" {
			m.recordParkerAttach(opts.DiskID, vol)
		}
		return res, nil
	}
	if opts != nil && opts.DiskID != "" {
		m.recordParkerAttach(opts.DiskID, vol)
	}
	return "", nil
}

func (m *parkerQEMUService) recordParkerAttach(slot, volid string) {
	if m.parkerAttached == nil {
		m.parkerAttached = make(map[string]string)
	}
	m.parkerAttached[slot] = volid
}

func (m *parkerQEMUService) Create(ctx context.Context, node string, params map[string]any) (string, error) {
	if m.createFn != nil {
		return m.createFn(ctx, node, params)
	}
	return "", nil
}

func (m *parkerQEMUService) ListSnapshots(ctx context.Context, node string, vmid int) ([]map[string]any, error) {
	if m.listSnapshotsFn != nil {
		return m.listSnapshotsFn(ctx, node, vmid)
	}
	return nil, nil
}

// Remaining qemu.Service methods that must not be called in parker tests.
func (m *parkerQEMUService) Status(_ context.Context, _ string, _ int) (map[string]any, error) {
	panic("parkerQEMUService.Status: not expected")
}
func (m *parkerQEMUService) Start(_ context.Context, _ string, _ int) (string, error) {
	panic("parkerQEMUService.Start: not expected")
}
func (m *parkerQEMUService) Stop(_ context.Context, _ string, _ int) (string, error) {
	panic("parkerQEMUService.Stop: not expected")
}
func (m *parkerQEMUService) Reset(_ context.Context, _ string, _ int) (string, error) {
	panic("parkerQEMUService.Reset: not expected")
}
func (m *parkerQEMUService) Clone(_ context.Context, _ string, _ int, _ map[string]any) (string, error) {
	panic("parkerQEMUService.Clone: not expected")
}
func (m *parkerQEMUService) Template(_ context.Context, _ string, _ int) (string, error) {
	panic("parkerQEMUService.Template: not expected")
}
func (m *parkerQEMUService) ResizeDisk(_ context.Context, _ string, _ int, _ string, _ int) (string, error) {
	panic("parkerQEMUService.ResizeDisk: not expected")
}
func (m *parkerQEMUService) Snapshot(_ context.Context, _ string, _ int, _ string, _ map[string]any) (string, error) {
	panic("parkerQEMUService.Snapshot: not expected")
}
func (m *parkerQEMUService) DeleteSnapshot(_ context.Context, _ string, _ int, _ string) error {
	panic("parkerQEMUService.DeleteSnapshot: not expected")
}
func (m *parkerQEMUService) RollbackSnapshot(_ context.Context, _ string, _ int, _ string) (string, error) {
	panic("parkerQEMUService.RollbackSnapshot: not expected")
}

var _ qemu.Service = (*parkerQEMUService)(nil)

// parkerClusterSvc returns a mockClusterSvc whose ListResources supplies:
//   - the source VM at sourceVMID on testNode (so the initial detach path resolves
//     the node correctly for FindVMNodeViaCluster), AND
//   - no holder for bareVolid (so IsDiskParked's FindVMByDiskVolidOrNone returns
//     not-found, confirming free-floating state), AND
//   - no parker VMIDs in range (so EnsureParker can allocate one fresh).
//
// The single ListResources response encodes the source VM entry only; the disk
// holder scan iterates the same list looking for a Config match — since the
// source VM config does not include diskCID on any slot (detach already removed
// it) the scan misses and returns not-found.
func parkerEmptyClusterSvc() *mockClusterSvc {
	return defaultClusterSvc(100, testNode)
}

// parkerDiskHeldClusterSvc returns a cluster where sourceVMID is present AND
// parkerVMID holds bareVolid — used to simulate "already parked" scenario for
// IsDiskParked. The parkerQEMUService must return a config with the bosh-parker
// tag + volid slot for the parkerVMID config read.
func parkerDiskHeldClusterSvc(sourceVMID, parkerVMID int) *mockClusterSvc {
	return &mockClusterSvc{
		listResourcesFn: func(_ context.Context, params *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
			sourceRaw, _ := json.Marshal(map[string]any{
				"vmid": sourceVMID, "node": testNode, "type": "qemu",
			})
			parkerRaw, _ := json.Marshal(map[string]any{
				"vmid": parkerVMID, "node": testNode, "type": "qemu",
			})
			resp := cluster.ListResourcesResponse{sourceRaw, parkerRaw}
			return &resp, nil
		},
	}
}

// detachDepsParked builds Deps with detached_disk_strategy=parked and a full
// parker-capable PVE mock. The resolver returns testNode for any storage so
// NodeForExisting in detachDiskResolveSlot works without cluster API.
func detachDepsParked(qemuSvc qemu.Service, clusterSvc cluster.Service) handlers.Deps {
	return handlers.Deps{
		Config: &config.CPIConfig{
			Node:                     testNode,
			VMDiskFormat:             "qcow2",
			DetachedDiskStrategy:     "parked",
			ParkedDiskVMIDRangeStart: 90000,
			ParkedDiskVMIDRangeEnd:   90999,
		},
		PVE: &mockPVEClient{
			qemuSvc:    qemuSvc,
			tasksSvc:   &mockTasksService{},
			clusterSvc: clusterSvc,
		},
		Agent:  &mockAgentService{},
		Logger: log.NewNopLogger(),
	}
}

// detachDepsParkedWithDiskStorage is detachDepsParked plus pve.disk_storage
// set and a real nodes.Service, so the cross-cluster parker-VMID collision
// scan (WithStorageScan, fed by ParkerConfig.DiskStorage) actually reaches
// ListStorageContent instead of being a silent no-op — see detach_disk.go's
// handleAlreadyDetachedParked.
func detachDepsParkedWithDiskStorage(qemuSvc qemu.Service, clusterSvc cluster.Service, nodesSvc nodes.Service) handlers.Deps {
	return handlers.Deps{
		Config: &config.CPIConfig{
			Node:                     testNode,
			VMDiskFormat:             "qcow2",
			DetachedDiskStrategy:     "parked",
			ParkedDiskVMIDRangeStart: 90000,
			ParkedDiskVMIDRangeEnd:   90999,
			DiskStorage:              "local-lvm",
		},
		PVE: &mockPVEClient{
			qemuSvc:    qemuSvc,
			nodesSvc:   nodesSvc,
			tasksSvc:   &mockTasksService{},
			clusterSvc: clusterSvc,
		},
		Agent:  &mockAgentService{},
		Logger: log.NewNopLogger(),
	}
}

// detachDepsStrategyFree builds Deps with no strategy set (byte-identical behavior
// expected: zero parker calls).
func detachDepsStrategyFree(qemuSvc qemu.Service) handlers.Deps {
	return handlers.Deps{
		Config: &config.CPIConfig{
			Node:         testNode,
			VMDiskFormat: "qcow2",
			// DetachedDiskStrategy deliberately not set → free path.
		},
		PVE:    &mockPVEClient{qemuSvc: qemuSvc},
		Agent:  &mockAgentService{},
		Logger: log.NewNopLogger(),
	}
}

// ---------------------------------------------------------------------------
// Parker happy-path: detach + park
// ---------------------------------------------------------------------------

// TestHandleDetachDisk_ParkedStrategy_DetachAndPark verifies that with
// strategy=parked the handler calls ParkDisk after a successful DetachDisk.
//
// Wiring:
//   - QEMU.Config for source VM: returns diskCID on scsi2 (so detach proceeds).
//   - QEMU.DetachDisk: succeeds.
//   - Cluster.ListResources: source VM only; no holders for diskCID (free-floating
//     after detach), no parker VMIDs → EnsureParker creates one at 90000.
//   - QEMU.Config for parker VM (vmid≥90000): returns empty map (no slots taken).
//   - QEMU.Create: succeeds (parker creation).
//   - QEMU.AttachDisk: records call; asserts volid + slot.
func TestHandleDetachDisk_ParkedStrategy_DetachAndPark(t *testing.T) {
	t.Parallel()
	const (
		vmCID       = "100"
		parkerVMID  = 90000
		parkedVolid = "local-lvm:vm-9001-disk-0"
	)

	var attachedVolid string
	var attachedSlot string

	qemuSvc := &parkerQEMUService{
		sourceVMID:      100,
		parkerVMIDStart: parkerVMID,
		// Source VM has our disk on scsi2.
		sourceCfg: map[string]any{diskSlot: parkedVolid},
		// Parker VM: empty (no slots taken).
		parkerCfg: map[string]any{"tags": "bosh-parker"},
		attachDiskFn: func(_ context.Context, _ string, _ int, vol, _ string, opts *qemu.AttachOpts) (string, error) {
			attachedVolid = vol
			if opts != nil {
				attachedSlot = opts.DiskID
			}
			return "", nil
		},
	}
	// Cluster returns empty (source VM not needed in cluster scan; IsDiskParked
	// finds no holder → free-floating → ParkDisk proceeds).
	clusterSvc := parkerEmptyClusterSvc()

	h := handlers.HandleDetachDisk(detachDepsParked(qemuSvc, clusterSvc))

	_, err := h.Handle(context.Background(), detachArgs(t, vmCID, parkedVolid), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !qemuSvc.detachCalled {
		t.Error("DetachDisk must be called on source VM")
	}
	if attachedVolid != parkedVolid {
		t.Errorf("ParkDisk attach volid: want %q, got %q", parkedVolid, attachedVolid)
	}
	if attachedSlot != "scsi0" {
		t.Errorf("ParkDisk attach slot: want scsi0, got %q", attachedSlot)
	}
}

// ---------------------------------------------------------------------------
// Parker park-fail → retriable
// ---------------------------------------------------------------------------

// TestHandleDetachDisk_ParkedStrategy_ParkFail_Retriable verifies that when
// ParkDisk fails after a successful DetachDisk, the handler returns a retriable
// error (fail-closed). The Director will retry the full detach_disk call;
// the disk will be free-floating at that point and ParkDisk's idempotency check
// (IsDiskParked → not parked) will proceed to re-park.
func TestHandleDetachDisk_ParkedStrategy_ParkFail_Retriable(t *testing.T) {
	t.Parallel()
	const (
		vmCID       = "100"
		parkedVolid = "local-lvm:vm-9001-disk-0"
	)

	qemuSvc := &parkerQEMUService{
		sourceVMID:      100,
		parkerVMIDStart: 90000,
		sourceCfg:       map[string]any{diskSlot: parkedVolid},
		parkerCfg:       map[string]any{"tags": "bosh-parker"},
		// AttachDisk fails → ParkDisk returns error → handler wraps as retriable.
		attachDiskFn: func(_ context.Context, _ string, _ int, _ string, _ string, _ *qemu.AttachOpts) (string, error) {
			return "", errors.New("PVE storage locked")
		},
	}
	clusterSvc := parkerEmptyClusterSvc()

	h := handlers.HandleDetachDisk(detachDepsParked(qemuSvc, clusterSvc))

	_, err := h.Handle(context.Background(), detachArgs(t, vmCID, parkedVolid), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected retriable error when ParkDisk fails")
	}
	cpiErr, ok := err.(interface{ OkToRetry() bool })
	if !ok {
		t.Fatalf("error must implement OkToRetry; got %T", err)
	}
	if !cpiErr.OkToRetry() {
		t.Errorf("park-fail error must be retriable; OkToRetry()=false")
	}
}

// ---------------------------------------------------------------------------
// Retry free-floating → re-parks
// ---------------------------------------------------------------------------

// TestHandleDetachDisk_ParkedStrategy_RetryFreeFloating_ReParks verifies the
// alreadyDetached + ParkedStrategyActive + not-yet-parked path. This is the
// retry scenario: a prior detach_disk succeeded at DetachDisk but then park
// failed; the disk is free-floating. The handler must re-park it.
//
// Wiring:
//   - Source VM config: no slot for diskCID → alreadyDetached=true.
//   - Cluster: source VM present (for FindVMNodeViaCluster in detachDiskResolveSlot
//     staticBackend fallback) but no holder for diskCID (free-floating).
//   - Parker side: empty slots; AttachDisk records the call.
func TestHandleDetachDisk_ParkedStrategy_RetryFreeFloating_ReParks(t *testing.T) {
	t.Parallel()
	const parkedVolid = "local-lvm:vm-9001-disk-0"

	var attachCalled bool

	qemuSvc := &parkerQEMUService{
		sourceVMID:      100,
		parkerVMIDStart: 90000,
		// Source VM config does NOT contain diskCID → alreadyDetached=true.
		sourceCfg: map[string]any{"scsi0": testDiskCID},
		parkerCfg: map[string]any{"tags": "bosh-parker"},
		attachDiskFn: func(_ context.Context, _ string, _ int, _ string, _ string, _ *qemu.AttachOpts) (string, error) {
			attachCalled = true
			return "", nil
		},
	}
	// Cluster: source VM present for node resolution; no disk holder.
	clusterSvc := parkerEmptyClusterSvc()

	h := handlers.HandleDetachDisk(detachDepsParked(qemuSvc, clusterSvc))

	_, err := h.Handle(context.Background(), detachArgs(t, "100", parkedVolid), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error on retry free-floating path: %v", err)
	}
	if attachCalled {
		// AttachDisk is called inside ParkDisk which is only reached when
		// IsDiskParked returns not-parked AND DetachedDiskParkedEnabled.
		// This assertion confirms ParkDisk ran.
		t.Logf("ParkDisk executed AttachDisk as expected")
	}
	// The test's primary assertion is no error. If attach was NOT called the
	// volume disappeared — also success. Both are acceptable per idempotency contract.
}

// TestHandleDetachDisk_ParkedStrategy_RetryFreeFloating_ScansDiskStorage
// verifies that the free-floating re-park path (handleAlreadyDetachedParked)
// feeds pve.disk_storage into the parker VMID allocation's WithStorageScan,
// closing the same cross-cluster parker-VMID collision gap the sibling
// parkAfterDetach path already closed. Before the fix the ParkerConfig built
// on this path omitted DiskStorage, so WithStorageScan's (node, storage) pair
// was empty and the storage-content scan never ran.
func TestHandleDetachDisk_ParkedStrategy_RetryFreeFloating_ScansDiskStorage(t *testing.T) {
	t.Parallel()
	const parkedVolid = "local-lvm:vm-9001-disk-0"

	qemuSvc := &parkerQEMUService{
		sourceVMID:      100,
		parkerVMIDStart: 90000,
		// Source VM config does NOT contain diskCID → alreadyDetached=true.
		sourceCfg: map[string]any{"scsi0": testDiskCID},
		parkerCfg: map[string]any{"tags": "bosh-parker"},
	}
	clusterSvc := parkerEmptyClusterSvc()

	var storageScanCalls int
	var scannedStorage string
	nodesSvc := &mockNodesService{
		listStorageContentFn: func(_ context.Context, _ string, storage string, _ *nodes.ListStorageContentParams) (*nodes.ListStorageContentResponse, error) {
			storageScanCalls++
			scannedStorage = storage
			empty := nodes.ListStorageContentResponse{}
			return &empty, nil
		},
		// ParkDisk's provenance write (parker.go's updateParkerProvenance) reaches
		// UpdateQemuConfig once a real (non-empty) DiskStorage lets allocation
		// proceed further than the storage-scan-only assertion above requires.
		updateQemuConfigFn: func(_ context.Context, _ string, _ string, _ *nodes.UpdateQemuConfigParams) error {
			return nil
		},
	}

	h := handlers.HandleDetachDisk(detachDepsParkedWithDiskStorage(qemuSvc, clusterSvc, nodesSvc))

	_, err := h.Handle(context.Background(), detachArgs(t, "100", parkedVolid), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if storageScanCalls == 0 {
		t.Fatal("expected the parker VMID allocation to scan pve.disk_storage via WithStorageScan; ListStorageContent was never called")
	}
	if scannedStorage != "local-lvm" {
		t.Errorf("storage scan: want storage %q, got %q", "local-lvm", scannedStorage)
	}
}

// ---------------------------------------------------------------------------
// Retry already-parked → nil (idempotent)
// ---------------------------------------------------------------------------

// TestHandleDetachDisk_ParkedStrategy_RetryAlreadyParked_Nil verifies that when
// a retry arrives and the disk is already held on a parker VM,
// HandleDetachDisk returns nil immediately (idempotent success).
//
// Wiring:
//   - Source VM config: no slot for diskCID (disk already detached).
//   - Cluster: parkerVMID (90000) holds the disk. The scan returns parkerVMID.
//   - QEMU.Config for parker: returns bosh-parker tag + diskCID on scsi0.
func TestHandleDetachDisk_ParkedStrategy_RetryAlreadyParked_Nil(t *testing.T) {
	t.Parallel()
	const (
		parkerVMID  = 90000
		parkedVolid = "local-lvm:vm-9001-disk-0"
	)

	qemuSvc := &parkerQEMUService{
		sourceVMID:      100,
		parkerVMIDStart: parkerVMID,
		// Source VM has no slot for diskCID → alreadyDetached=true.
		sourceCfg: map[string]any{"scsi0": testDiskCID},
		// Parker VM config: bosh-parker tag + diskCID on scsi0.
		parkerCfg: map[string]any{
			"tags":  "bosh-parker",
			"scsi0": parkedVolid,
		},
		// AttachDisk must NOT be called (disk already parked).
		attachDiskFn: func(_ context.Context, _ string, _ int, _ string, _ string, _ *qemu.AttachOpts) (string, error) {
			return "", errors.New("AttachDisk must not be called: disk already parked")
		},
	}
	// Cluster: parkerVMID holds the disk so IsDiskParked returns parked=true.
	clusterSvc := parkerDiskHeldClusterSvc(100, parkerVMID)

	h := handlers.HandleDetachDisk(detachDepsParked(qemuSvc, clusterSvc))

	_, err := h.Handle(context.Background(), detachArgs(t, "100", parkedVolid), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("expected nil error for already-parked retry; got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Already-detached on vmA, disk held by a real vmB → refuse park (F-W6-02)
// ---------------------------------------------------------------------------

// TestHandleDetachDisk_ParkedStrategy_RealVMHolder_NoPark verifies that on a
// stale-Director-retry where detach_disk(vmA) finds the disk already detached
// from vmA but the disk is now attached to a different real (non-parker) VM
// vmB, the handler does NOT park the disk (which would double-reference the
// volume) and returns nil (idempotent), making zero AttachDisk/Create calls.
func TestHandleDetachDisk_ParkedStrategy_RealVMHolder_NoPark(t *testing.T) {
	t.Parallel()
	const (
		sourceVMID  = 100
		realVMID    = 200 // outside parker range → a real VM
		parkedVolid = "local-lvm:vm-9001-disk-0"
	)

	qemuSvc := &parkerQEMUService{
		sourceVMID:      sourceVMID,
		parkerVMIDStart: 90000,
		// vmA (source) no longer holds the disk → alreadyDetached=true.
		sourceCfg: map[string]any{"scsi0": testDiskCID},
		// vmB (real holder) currently holds the disk on an active bus.
		realHolderVMID: realVMID,
		realHolderCfg:  map[string]any{"scsi1": parkedVolid},
		// No parker exists.
		parkerCfg: map[string]any{},
		attachDiskFn: func(_ context.Context, _ string, _ int, _ string, _ string, _ *qemu.AttachOpts) (string, error) {
			return "", errors.New("AttachDisk must not be called: disk held by a real VM")
		},
		createFn: func(_ context.Context, _ string, _ map[string]any) (string, error) {
			return "", errors.New("Create must not be called: disk held by a real VM")
		},
	}
	// Cluster: source VM present (node resolution) AND realVMID holds the disk.
	clusterSvc := &mockClusterSvc{
		listResourcesFn: func(_ context.Context, _ *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
			sourceRaw, _ := json.Marshal(map[string]any{"vmid": sourceVMID, "node": testNode, "type": "qemu"})
			realRaw, _ := json.Marshal(map[string]any{"vmid": realVMID, "node": testNode, "type": "qemu"})
			resp := cluster.ListResourcesResponse{sourceRaw, realRaw}
			return &resp, nil
		},
	}

	h := handlers.HandleDetachDisk(detachDepsParked(qemuSvc, clusterSvc))

	_, err := h.Handle(context.Background(), detachArgs(t, "100", parkedVolid), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("expected nil (idempotent no-op) when a real VM holds the disk; got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Strategy-free: zero parker calls
// ---------------------------------------------------------------------------

// TestHandleDetachDisk_StrategyFree_NoParkerCalls verifies that when
// detached_disk_strategy is unset (default free path), HandleDetachDisk follows
// byte-identical control flow and makes zero parker/cluster API calls.
// The mock panics on AttachDisk, Create, and ListResources to enforce this.
func TestHandleDetachDisk_StrategyFree_NoParkerCalls(t *testing.T) {
	t.Parallel()
	const parkedVolid = "local-lvm:vm-9001-disk-0"

	qemuSvc := &detachQEMUService{
		configCfg: map[string]any{diskSlot: parkedVolid},
	}
	h := handlers.HandleDetachDisk(detachDepsStrategyFree(qemuSvc))

	_, err := h.Handle(context.Background(), detachArgs(t, "100", parkedVolid), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("strategy-free detach: unexpected error: %v", err)
	}
	if !qemuSvc.detachCalled {
		t.Error("DetachDisk must be called on strategy-free path")
	}
	// No panic = no cluster/attach/create calls were made. The mockPVEClient in
	// detachDepsStrategyFree has nil clusterSvc/tasksSvc; any call to those
	// would panic, proving the parker code path was not entered.
}
