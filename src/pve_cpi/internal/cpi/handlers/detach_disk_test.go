package handlers_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi/handlers"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/qemu"
	sdkerrors "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/errors"
)

// ---------------------------------------------------------------------------
// detachQEMUService: QEMU mock for detach_disk tests.
// ---------------------------------------------------------------------------

type detachQEMUService struct {
	// Config control — used by ResolveDiskID.
	configCfg map[string]interface{}
	configErr error

	// DetachDisk control.
	detachErr      error
	detachCalled   bool
	detachedDiskID string
}

func (m *detachQEMUService) Config(_ context.Context, _ string, _ int) (map[string]interface{}, error) {
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
func (m *detachQEMUService) Create(_ context.Context, _ string, _ map[string]interface{}) (string, error) {
	panic("detachQEMUService.Create: not expected")
}
func (m *detachQEMUService) Status(_ context.Context, _ string, _ int) (map[string]interface{}, error) {
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
func (m *detachQEMUService) Clone(_ context.Context, _ string, _ int, _ map[string]interface{}) (string, error) {
	panic("detachQEMUService.Clone: not expected")
}
func (m *detachQEMUService) Template(_ context.Context, _ string, _ int) (string, error) {
	panic("detachQEMUService.Template: not expected")
}
func (m *detachQEMUService) ResizeDisk(_ context.Context, _ string, _ int, _ string, _ int) (string, error) {
	panic("detachQEMUService.ResizeDisk: not expected")
}
func (m *detachQEMUService) Snapshot(_ context.Context, _ string, _ int, _ string, _ map[string]interface{}) (string, error) {
	panic("detachQEMUService.Snapshot: not expected")
}
func (m *detachQEMUService) DeleteSnapshot(_ context.Context, _ string, _ int, _ string) error {
	panic("detachQEMUService.DeleteSnapshot: not expected")
}
func (m *detachQEMUService) ListSnapshots(_ context.Context, _ string, _ int) ([]map[string]interface{}, error) {
	panic("detachQEMUService.ListSnapshots: not expected")
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
			Node:         "pve1",
			VMDiskFormat: "qcow2",
		},
		PVE:    &mockPVEClient{qemuSvc: qemuSvc},
		Agent:  &mockAgentService{}, // detach_disk does not call UpdateDiskHints
		Logger: log.NewNopLogger(),
	}
}

// detachArgs builds a two-element arg slice for detach_disk.
func detachArgs(vmCID, diskCID string) []json.RawMessage {
	return marshalArgs(vmCID, diskCID)
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestHandleDetachDisk_Happy verifies the full happy path:
//   - ResolveDiskID finds scsi2 for the given volid
//   - DetachDisk called with diskID "scsi2"
//   - Returns nil (void success)
func TestHandleDetachDisk_Happy(t *testing.T) {
	const (
		vmCID   = "100"
		diskCID = "local-lvm:vm-9001-disk-0"
		volid   = "local-lvm:vm-9001-disk-0"
	)

	qemuSvc := &detachQEMUService{
		configCfg: map[string]interface{}{
			"scsi0": "local-lvm:vm-100-disk-0",
			"scsi1": "local-lvm:vm-100-disk-1",
			"scsi2": volid,
		},
	}
	h := handlers.HandleDetachDisk(detachDeps(qemuSvc))

	result, err := h.Handle(context.Background(), detachArgs(vmCID, diskCID), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != nil {
		t.Errorf("result: want nil (void), got %v", result)
	}
	if !qemuSvc.detachCalled {
		t.Error("DetachDisk was not called")
	}
	if qemuSvc.detachedDiskID != "scsi2" {
		t.Errorf("DetachDisk diskID: want scsi2, got %q", qemuSvc.detachedDiskID)
	}
}

// TestHandleDetachDisk_NotAttached verifies idempotency: disk not in VM config →
// no DetachDisk call, nil error returned.
func TestHandleDetachDisk_NotAttached(t *testing.T) {
	const (
		vmCID   = "100"
		diskCID = "local-lvm:not-attached-vol"
	)

	qemuSvc := &detachQEMUService{
		configCfg: map[string]interface{}{
			"scsi0": "local-lvm:vm-100-disk-0",
		},
	}
	h := handlers.HandleDetachDisk(detachDeps(qemuSvc))

	result, err := h.Handle(context.Background(), detachArgs(vmCID, diskCID), jsonrpc.Context{})
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

// TestHandleDetachDisk_DetachFail verifies SDK DetachDisk failure propagates.
func TestHandleDetachDisk_DetachFail(t *testing.T) {
	const (
		vmCID   = "100"
		diskCID = "local-lvm:vm-9001-disk-0"
		volid   = "local-lvm:vm-9001-disk-0"
	)

	qemuSvc := &detachQEMUService{
		configCfg: map[string]interface{}{"scsi2": volid},
		detachErr: errors.New("PVE refused to detach"),
	}
	h := handlers.HandleDetachDisk(detachDeps(qemuSvc))

	_, err := h.Handle(context.Background(), detachArgs(vmCID, diskCID), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error from DetachDisk failure")
	}
}

// TestHandleDetachDisk_VMNotFound verifies 404 from DetachDisk maps to VMNotFound.
func TestHandleDetachDisk_VMNotFound(t *testing.T) {
	const (
		vmCID   = "999"
		diskCID = "local-lvm:vm-9001-disk-0"
		volid   = "local-lvm:vm-9001-disk-0"
	)

	qemuSvc := &detachQEMUService{
		configCfg: map[string]interface{}{"scsi2": volid},
		detachErr: &sdkerrors.APIError{Code: 404, Message: "VM not found"},
	}
	h := handlers.HandleDetachDisk(detachDeps(qemuSvc))

	_, err := h.Handle(context.Background(), detachArgs(vmCID, diskCID), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error")
	}
	if !cpierrors.IsType(err, cpierrors.TypeVMNotFound) {
		t.Errorf("error type: want VMNotFound, got %T %v", err, err)
	}
}

// TestHandleDetachDisk_InvalidVMCID verifies non-integer vm_cid returns VMNotFound.
func TestHandleDetachDisk_InvalidVMCID(t *testing.T) {
	h := handlers.HandleDetachDisk(detachDeps(&detachQEMUService{}))
	_, err := h.Handle(context.Background(), detachArgs("not-an-int", "local:vol"), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error")
	}
	if !cpierrors.IsType(err, cpierrors.TypeVMNotFound) {
		t.Errorf("error type: want VMNotFound, got %T %v", err, err)
	}
}

// TestHandleDetachDisk_InvalidDiskCID verifies malformed disk_cid returns DiskNotFound.
func TestHandleDetachDisk_InvalidDiskCID(t *testing.T) {
	h := handlers.HandleDetachDisk(detachDeps(&detachQEMUService{}))
	_, err := h.Handle(context.Background(), detachArgs("100", "nodisk"), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error")
	}
	if !cpierrors.IsType(err, cpierrors.TypeDiskNotFound) {
		t.Errorf("error type: want DiskNotFound, got %T %v", err, err)
	}
}

// TestHandleDetachDisk_MissingArgs verifies argument count validation.
func TestHandleDetachDisk_MissingArgs(t *testing.T) {
	h := handlers.HandleDetachDisk(detachDeps(&detachQEMUService{}))
	_, err := h.Handle(context.Background(), marshalArgs("100"), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error for missing args")
	}
}

// TestHandleDetachDisk_EmptyVMCID verifies empty vm_cid string is rejected.
func TestHandleDetachDisk_EmptyVMCID(t *testing.T) {
	h := handlers.HandleDetachDisk(detachDeps(&detachQEMUService{}))
	_, err := h.Handle(context.Background(), detachArgs("", "local:vol"), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error for empty vm_cid")
	}
}

// TestHandleDetachDisk_EmptyDiskCID verifies empty disk_cid string is rejected.
func TestHandleDetachDisk_EmptyDiskCID(t *testing.T) {
	h := handlers.HandleDetachDisk(detachDeps(&detachQEMUService{}))
	_, err := h.Handle(context.Background(), detachArgs("100", ""), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error for empty disk_cid")
	}
}

// TestHandleDetachDisk_ConfigFetchError verifies that a Config fetch error (not a
// "not attached" CloudError but a real network error) propagates to the caller.
func TestHandleDetachDisk_ConfigFetchError(t *testing.T) {
	qemuSvc := &detachQEMUService{
		configErr: errors.New("network unreachable"),
	}
	h := handlers.HandleDetachDisk(detachDeps(qemuSvc))
	_, err := h.Handle(context.Background(), detachArgs("100", "local-lvm:vol"), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error when Config fetch fails")
	}
}

// TestHandleDetachDisk_EmptyConfigIdempotent verifies that an empty VM config
// (no disks attached at all) is treated as "not attached" and returns nil.
func TestHandleDetachDisk_EmptyConfigIdempotent(t *testing.T) {
	qemuSvc := &detachQEMUService{
		configCfg: map[string]interface{}{}, // VM exists but has no disks
	}
	h := handlers.HandleDetachDisk(detachDeps(qemuSvc))

	result, err := h.Handle(context.Background(), detachArgs("100", "local-lvm:missing"), jsonrpc.Context{})
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
