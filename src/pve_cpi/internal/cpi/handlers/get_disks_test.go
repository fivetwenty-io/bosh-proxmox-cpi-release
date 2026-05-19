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
// getDisksQEMUService: QEMU mock for get_disks tests.
// ---------------------------------------------------------------------------

type getDisksQEMUService struct {
	configFn func(ctx context.Context, node string, vmid int) (map[string]interface{}, error)
}

func (m *getDisksQEMUService) Config(ctx context.Context, node string, vmid int) (map[string]interface{}, error) {
	if m.configFn != nil {
		return m.configFn(ctx, node, vmid)
	}
	return map[string]interface{}{}, nil
}

// Unimplemented methods.
func (m *getDisksQEMUService) Create(_ context.Context, _ string, _ map[string]interface{}) (string, error) {
	panic("getDisksQEMUService.Create: not expected")
}
func (m *getDisksQEMUService) Status(_ context.Context, _ string, _ int) (map[string]interface{}, error) {
	panic("getDisksQEMUService.Status: not expected")
}
func (m *getDisksQEMUService) Start(_ context.Context, _ string, _ int) (string, error) {
	panic("getDisksQEMUService.Start: not expected")
}
func (m *getDisksQEMUService) Stop(_ context.Context, _ string, _ int) (string, error) {
	panic("getDisksQEMUService.Stop: not expected")
}
func (m *getDisksQEMUService) Reset(_ context.Context, _ string, _ int) (string, error) {
	panic("getDisksQEMUService.Reset: not expected")
}
func (m *getDisksQEMUService) Clone(_ context.Context, _ string, _ int, _ map[string]interface{}) (string, error) {
	panic("getDisksQEMUService.Clone: not expected")
}
func (m *getDisksQEMUService) Template(_ context.Context, _ string, _ int) (string, error) {
	panic("getDisksQEMUService.Template: not expected")
}
func (m *getDisksQEMUService) AttachDisk(_ context.Context, _ string, _ int, _ string, _ string, _ *qemu.AttachOpts) (string, error) {
	panic("getDisksQEMUService.AttachDisk: not expected")
}
func (m *getDisksQEMUService) DetachDisk(_ context.Context, _ string, _ int, _ string) error {
	panic("getDisksQEMUService.DetachDisk: not expected")
}
func (m *getDisksQEMUService) ResizeDisk(_ context.Context, _ string, _ int, _ string, _ int) (string, error) {
	panic("getDisksQEMUService.ResizeDisk: not expected")
}
func (m *getDisksQEMUService) Snapshot(_ context.Context, _ string, _ int, _ string, _ map[string]interface{}) (string, error) {
	panic("getDisksQEMUService.Snapshot: not expected")
}
func (m *getDisksQEMUService) DeleteSnapshot(_ context.Context, _ string, _ int, _ string) error {
	panic("getDisksQEMUService.DeleteSnapshot: not expected")
}
func (m *getDisksQEMUService) ListSnapshots(_ context.Context, _ string, _ int) ([]map[string]interface{}, error) {
	panic("getDisksQEMUService.ListSnapshots: not expected")
}
func (m *getDisksQEMUService) RollbackSnapshot(_ context.Context, _ string, _ int, _ string) (string, error) {
	panic("getDisksQEMUService.RollbackSnapshot: not expected")
}

var _ qemu.Service = (*getDisksQEMUService)(nil)

// ---------------------------------------------------------------------------
// getDisksDeps builds Deps for get_disks tests.
// ---------------------------------------------------------------------------

func getDisksDeps(qemuSvc qemu.Service) handlers.Deps {
	return handlers.Deps{
		Config: &config.CPIConfig{
			Node: "pve1",
		},
		PVE:    &mockPVEClient{qemuSvc: qemuSvc},
		Logger: log.NewNopLogger(),
	}
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestHandleGetDisks_MultiplePersistentDisks(t *testing.T) {
	qemuSvc := &getDisksQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]interface{}, error) {
			return map[string]interface{}{
				"scsi0": "local-lvm:vm-100-disk-0",                // system disk → excluded
				"scsi1": "local-lvm:vm-100-disk-1",                // persistent disk 1
				"scsi2": "local-lvm:vm-9001-disk-0,size=10G",      // persistent disk 2 (option string)
				"ide2":  "local-lvm:vm-100-cloudinit,media=cdrom", // cloudinit → excluded
			}, nil
		},
	}

	h := handlers.HandleGetDisks(getDisksDeps(qemuSvc))
	result, err := h.Handle(context.Background(), marshalArgs("100"), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	diskCIDs, ok := result.([]string)
	if !ok {
		t.Fatalf("result: want []string, got %T", result)
	}
	if len(diskCIDs) != 2 {
		t.Errorf("disk count: want 2, got %d: %v", len(diskCIDs), diskCIDs)
	}

	// Verify bare volids are returned (no option fragments).
	for _, cid := range diskCIDs {
		if cid == "" {
			t.Error("empty disk_cid in result")
		}
	}
	t.Logf("disk_cids = %v", diskCIDs)
}

func TestHandleGetDisks_SystemDiskOnly(t *testing.T) {
	// VM has only a system disk → persistent disk list is empty.
	qemuSvc := &getDisksQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]interface{}, error) {
			return map[string]interface{}{
				"scsi0": "local-lvm:vm-100-disk-0",
				"ide2":  "local-lvm:vm-100-cloudinit,media=cdrom",
			}, nil
		},
	}

	h := handlers.HandleGetDisks(getDisksDeps(qemuSvc))
	result, err := h.Handle(context.Background(), marshalArgs("100"), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	diskCIDs, ok := result.([]string)
	if !ok {
		t.Fatalf("result: want []string, got %T", result)
	}
	if len(diskCIDs) != 0 {
		t.Errorf("disk count: want 0 for system-disk-only VM, got %d: %v", len(diskCIDs), diskCIDs)
	}
}

func TestHandleGetDisks_VMNotFound(t *testing.T) {
	qemuSvc := &getDisksQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]interface{}, error) {
			return nil, &sdkerrors.APIError{HTTPCode: 404, Message: "VM not found"}
		},
	}

	h := handlers.HandleGetDisks(getDisksDeps(qemuSvc))
	_, err := h.Handle(context.Background(), marshalArgs("999"), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error for 404 VM")
	}
	if !cpierrors.IsType(err, cpierrors.TypeVMNotFound) {
		t.Errorf("error type: want VMNotFound, got %T %v", err, err)
	}
}

func TestHandleGetDisks_ConfigFetchError(t *testing.T) {
	qemuSvc := &getDisksQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]interface{}, error) {
			return nil, errors.New("storage backend unreachable")
		},
	}

	h := handlers.HandleGetDisks(getDisksDeps(qemuSvc))
	_, err := h.Handle(context.Background(), marshalArgs("100"), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error when Config fetch fails")
	}
}

func TestHandleGetDisks_InvalidVMCID(t *testing.T) {
	h := handlers.HandleGetDisks(getDisksDeps(&getDisksQEMUService{}))
	_, err := h.Handle(context.Background(), marshalArgs("not-an-int"), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error for non-integer vm_cid")
	}
	if !cpierrors.IsType(err, cpierrors.TypeVMNotFound) {
		t.Errorf("error type: want VMNotFound, got %T %v", err, err)
	}
}

func TestHandleGetDisks_EmptyVMCID(t *testing.T) {
	h := handlers.HandleGetDisks(getDisksDeps(&getDisksQEMUService{}))
	_, err := h.Handle(context.Background(), marshalArgs(""), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error for empty vm_cid")
	}
}

func TestHandleGetDisks_TooFewArgs(t *testing.T) {
	h := handlers.HandleGetDisks(getDisksDeps(&getDisksQEMUService{}))
	_, err := h.Handle(context.Background(), []json.RawMessage{}, jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error for missing vm_cid argument")
	}
}

func TestHandleGetDisks_MissingNode(t *testing.T) {
	h := handlers.HandleGetDisks(handlers.Deps{
		Config: &config.CPIConfig{Node: ""},
		PVE:    &mockPVEClient{qemuSvc: &getDisksQEMUService{}},
		Logger: log.NewNopLogger(),
	})
	_, err := h.Handle(context.Background(), marshalArgs("100"), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error for missing node")
	}
}

func TestHandleGetDisks_CloudinitExcluded(t *testing.T) {
	// Verify cloudinit drive (media=cdrom) is not included even when not on ide2.
	qemuSvc := &getDisksQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]interface{}, error) {
			return map[string]interface{}{
				"scsi0": "local-lvm:vm-100-disk-0",
				"scsi3": "local-lvm:vm-100-ci,media=cdrom", // cloudinit on non-standard slot
				"scsi2": "local-lvm:vm-9001-disk-0",
			}, nil
		},
	}

	h := handlers.HandleGetDisks(getDisksDeps(qemuSvc))
	result, err := h.Handle(context.Background(), marshalArgs("100"), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	diskCIDs := result.([]string)
	for _, cid := range diskCIDs {
		if cid == "local-lvm:vm-100-ci" {
			t.Error("cloudinit drive must not be included in disk list")
		}
	}
	if len(diskCIDs) != 1 {
		t.Errorf("disk count: want 1 persistent disk, got %d: %v", len(diskCIDs), diskCIDs)
	}
}

func TestHandleGetDisks_EmptyVM(t *testing.T) {
	// VM with no disks at all → empty list.
	qemuSvc := &getDisksQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]interface{}, error) {
			return map[string]interface{}{
				"name":   "test-vm",
				"memory": 2048.0,
			}, nil
		},
	}

	h := handlers.HandleGetDisks(getDisksDeps(qemuSvc))
	result, err := h.Handle(context.Background(), marshalArgs("100"), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	diskCIDs, ok := result.([]string)
	if !ok {
		t.Fatalf("result: want []string, got %T", result)
	}
	if len(diskCIDs) != 0 {
		t.Errorf("disk count: want 0, got %d", len(diskCIDs))
	}
}

func TestHandleGetDisks_BareVolidNoOptions(t *testing.T) {
	// Disk stored as bare volid (no option string) → returned as-is.
	qemuSvc := &getDisksQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]interface{}, error) {
			return map[string]interface{}{
				"scsi0": "local-lvm:vm-100-disk-0",  // system disk
				"scsi1": "local-lvm:vm-9001-disk-0", // bare volid, no options
			}, nil
		},
	}

	h := handlers.HandleGetDisks(getDisksDeps(qemuSvc))
	result, err := h.Handle(context.Background(), marshalArgs("100"), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	diskCIDs := result.([]string)
	if len(diskCIDs) != 1 {
		t.Fatalf("disk count: want 1, got %d: %v", len(diskCIDs), diskCIDs)
	}
	if diskCIDs[0] != "local-lvm:vm-9001-disk-0" {
		t.Errorf("disk_cid: want local-lvm:vm-9001-disk-0, got %q", diskCIDs[0])
	}
}
