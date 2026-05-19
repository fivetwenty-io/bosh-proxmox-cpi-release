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
	sdkclusterapi "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/qemu"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/tasks"
)

// ---------------------------------------------------------------------------
// updateDiskQEMUService: QEMU mock for update_disk tests.
// ---------------------------------------------------------------------------

type updateDiskQEMUService struct {
	configFn     func(ctx context.Context, node string, vmid int) (map[string]interface{}, error)
	resizeDiskFn func(ctx context.Context, node string, vmid int, diskID string, sizeGiB int) (string, error)
	attachDiskFn func(ctx context.Context, node string, vmid int, volid string, bus string, opts *qemu.AttachOpts) (string, error)
}

func (m *updateDiskQEMUService) Config(ctx context.Context, node string, vmid int) (map[string]interface{}, error) {
	if m.configFn != nil {
		return m.configFn(ctx, node, vmid)
	}
	return map[string]interface{}{}, nil
}

func (m *updateDiskQEMUService) ResizeDisk(ctx context.Context, node string, vmid int, diskID string, sizeGiB int) (string, error) {
	if m.resizeDiskFn != nil {
		return m.resizeDiskFn(ctx, node, vmid, diskID, sizeGiB)
	}
	return "", nil
}

func (m *updateDiskQEMUService) AttachDisk(ctx context.Context, node string, vmid int, volid string, bus string, opts *qemu.AttachOpts) (string, error) {
	if m.attachDiskFn != nil {
		return m.attachDiskFn(ctx, node, vmid, volid, bus, opts)
	}
	return "", nil
}

// Unimplemented methods.
func (m *updateDiskQEMUService) Create(_ context.Context, _ string, _ map[string]interface{}) (string, error) {
	panic("updateDiskQEMUService.Create: not expected")
}
func (m *updateDiskQEMUService) Status(_ context.Context, _ string, _ int) (map[string]interface{}, error) {
	panic("updateDiskQEMUService.Status: not expected")
}
func (m *updateDiskQEMUService) Start(_ context.Context, _ string, _ int) (string, error) {
	panic("updateDiskQEMUService.Start: not expected")
}
func (m *updateDiskQEMUService) Stop(_ context.Context, _ string, _ int) (string, error) {
	panic("updateDiskQEMUService.Stop: not expected")
}
func (m *updateDiskQEMUService) Reset(_ context.Context, _ string, _ int) (string, error) {
	panic("updateDiskQEMUService.Reset: not expected")
}
func (m *updateDiskQEMUService) Clone(_ context.Context, _ string, _ int, _ map[string]interface{}) (string, error) {
	panic("updateDiskQEMUService.Clone: not expected")
}
func (m *updateDiskQEMUService) Template(_ context.Context, _ string, _ int) (string, error) {
	panic("updateDiskQEMUService.Template: not expected")
}
func (m *updateDiskQEMUService) DetachDisk(_ context.Context, _ string, _ int, _ string) error {
	panic("updateDiskQEMUService.DetachDisk: not expected")
}
func (m *updateDiskQEMUService) Snapshot(_ context.Context, _ string, _ int, _ string, _ map[string]interface{}) (string, error) {
	panic("updateDiskQEMUService.Snapshot: not expected")
}
func (m *updateDiskQEMUService) DeleteSnapshot(_ context.Context, _ string, _ int, _ string) error {
	panic("updateDiskQEMUService.DeleteSnapshot: not expected")
}
func (m *updateDiskQEMUService) ListSnapshots(_ context.Context, _ string, _ int) ([]map[string]interface{}, error) {
	panic("updateDiskQEMUService.ListSnapshots: not expected")
}
func (m *updateDiskQEMUService) RollbackSnapshot(_ context.Context, _ string, _ int, _ string) (string, error) {
	panic("updateDiskQEMUService.RollbackSnapshot: not expected")
}

var _ qemu.Service = (*updateDiskQEMUService)(nil)

// ---------------------------------------------------------------------------
// updateDiskDeps builds Deps for update_disk tests.
// ---------------------------------------------------------------------------

func updateDiskDeps(qemuSvc qemu.Service, clusterSvc sdkclusterapi.Service, tasksSvc tasks.Service) handlers.Deps {
	return handlers.Deps{
		Config: &config.CPIConfig{
			Node: "pve1",
		},
		PVE: &mockPVEClient{
			qemuSvc:    qemuSvc,
			clusterSvc: clusterSvc,
			tasksSvc:   tasksSvc,
		},
		Logger: log.NewNopLogger(),
	}
}

// updateClusterWith builds a cluster service reporting one VM.
func updateClusterWith(vmid int) sdkclusterapi.Service {
	return &snapClusterService{
		listFn: func(_ context.Context, _ *sdkclusterapi.ListResourcesParams) (*sdkclusterapi.ListResourcesResponse, error) {
			return clusterRespWith(vmid, "pve1"), nil
		},
	}
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestHandleUpdateDisk_OptionsOnly(t *testing.T) {
	const diskCID = "local-lvm:vm-9001-disk-0"
	const volid = "vm-9001-disk-0"

	var capturedOptStr string

	// configFn: calls 1-2 return bare volid (for findVMByDiskVolid + ResolveDiskID).
	// call 3+ returns full option string (for reading existing options to merge).
	callCount := 0
	qemuSvc := &updateDiskQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]interface{}, error) {
			callCount++
			if callCount <= 2 {
				return map[string]interface{}{"scsi2": volid}, nil
			}
			return map[string]interface{}{"scsi2": volid + ",size=10G"}, nil
		},
		attachDiskFn: func(_ context.Context, _ string, _ int, optStr string, _ string, opts *qemu.AttachOpts) (string, error) {
			capturedOptStr = optStr
			if opts == nil || opts.DiskID != "scsi2" {
				t.Errorf("expected AttachDisk with DiskID=scsi2, got %v", opts)
			}
			return "scsi2", nil
		},
	}

	h := handlers.HandleUpdateDisk(updateDiskDeps(qemuSvc, updateClusterWith(100), nil))
	_, err := h.Handle(context.Background(), marshalArgs(diskCID, map[string]any{
		"cache":    "writeback",
		"iothread": true,
	}), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Resulting option string must contain the disk volid, cache, iothread, and preserved size.
	if !strings.Contains(capturedOptStr, volid) {
		t.Errorf("option string must contain volid %q, got %q", volid, capturedOptStr)
	}
	if !strings.Contains(capturedOptStr, "cache=writeback") {
		t.Errorf("option string must contain cache=writeback, got %q", capturedOptStr)
	}
	if !strings.Contains(capturedOptStr, "iothread=1") {
		t.Errorf("option string must contain iothread=1, got %q", capturedOptStr)
	}
	if !strings.Contains(capturedOptStr, "size=10G") {
		t.Errorf("option string must preserve existing size=10G, got %q", capturedOptStr)
	}
	t.Logf("capturedOptStr = %q", capturedOptStr)
}

func TestHandleUpdateDisk_SizeOnly(t *testing.T) {
	const diskCID = "local-lvm:vm-9001-disk-0"
	const volid = "vm-9001-disk-0"

	var capturedDelta int
	var attachCalled bool

	// calls 1-2: bare volid; call 3+: option string with size (for resize).
	callCount2 := 0
	qemuSvc := &updateDiskQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]interface{}, error) {
			callCount2++
			if callCount2 <= 2 {
				return map[string]interface{}{"scsi2": volid}, nil
			}
			return map[string]interface{}{"scsi2": volid + ",size=10G"}, nil
		},
		resizeDiskFn: func(_ context.Context, _ string, _ int, _ string, deltaGiB int) (string, error) {
			capturedDelta = deltaGiB
			return "", nil
		},
		attachDiskFn: func(_ context.Context, _ string, _ int, _ string, _ string, _ *qemu.AttachOpts) (string, error) {
			attachCalled = true
			return "scsi2", nil
		},
	}

	h := handlers.HandleUpdateDisk(updateDiskDeps(qemuSvc, updateClusterWith(100), nil))
	// size=20480 MiB = 20 GiB; current = 10 GiB → delta = 10 GiB.
	_, err := h.Handle(context.Background(), marshalArgs(diskCID, map[string]any{
		"size": 20480,
	}), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedDelta != 10 {
		t.Errorf("resize delta: want 10 GiB, got %d GiB", capturedDelta)
	}
	// size-only update should not call AttachDisk (no option changes).
	if attachCalled {
		t.Error("AttachDisk must not be called for size-only update with no option changes")
	}
}

func TestHandleUpdateDisk_CombinedSizeAndOptions(t *testing.T) {
	const diskCID = "local-lvm:vm-9001-disk-0"
	const volid = "vm-9001-disk-0"

	var resizeCalled bool
	var attachCalled bool

	callCount3 := 0
	qemuSvc := &updateDiskQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]interface{}, error) {
			callCount3++
			if callCount3 <= 2 {
				return map[string]interface{}{"scsi2": volid}, nil
			}
			return map[string]interface{}{"scsi2": volid + ",size=10G"}, nil
		},
		resizeDiskFn: func(_ context.Context, _ string, _ int, _ string, _ int) (string, error) {
			resizeCalled = true
			return "", nil
		},
		attachDiskFn: func(_ context.Context, _ string, _ int, _ string, _ string, _ *qemu.AttachOpts) (string, error) {
			attachCalled = true
			return "scsi2", nil
		},
	}

	h := handlers.HandleUpdateDisk(updateDiskDeps(qemuSvc, updateClusterWith(100), nil))
	_, err := h.Handle(context.Background(), marshalArgs(diskCID, map[string]any{
		"size":    20480,
		"discard": "on",
	}), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !resizeCalled {
		t.Error("ResizeDisk must be called when spec has size")
	}
	if !attachCalled {
		t.Error("AttachDisk must be called when spec has option changes")
	}
}

func TestHandleUpdateDisk_EmptySpec_NoOp(t *testing.T) {
	// Empty spec → no resize, no AttachDisk.
	const diskCID = "local-lvm:vm-9001-disk-0"
	const volid = "vm-9001-disk-0"

	var attachCalled bool

	qemuSvc := &updateDiskQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]interface{}, error) {
			return map[string]interface{}{"scsi2": volid}, nil
		},
		attachDiskFn: func(_ context.Context, _ string, _ int, _ string, _ string, _ *qemu.AttachOpts) (string, error) {
			attachCalled = true
			return "scsi2", nil
		},
	}

	h := handlers.HandleUpdateDisk(updateDiskDeps(qemuSvc, updateClusterWith(100), nil))
	_, err := h.Handle(context.Background(), marshalArgs(diskCID, map[string]any{}), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if attachCalled {
		t.Error("AttachDisk must not be called for empty update_spec")
	}
}

func TestHandleUpdateDisk_DetachedDisk(t *testing.T) {
	const diskCID = "local-lvm:vm-9001-disk-0"

	// Cluster has no VMs → disk is not attached.
	emptyCluster := &snapClusterService{
		listFn: func(_ context.Context, _ *sdkclusterapi.ListResourcesParams) (*sdkclusterapi.ListResourcesResponse, error) {
			resp := sdkclusterapi.ListResourcesResponse{}
			return &resp, nil
		},
	}

	h := handlers.HandleUpdateDisk(updateDiskDeps(&updateDiskQEMUService{}, emptyCluster, nil))
	_, err := h.Handle(context.Background(), marshalArgs(diskCID, map[string]any{"cache": "writeback"}), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error for detached disk")
	}
	if !cpierrors.IsType(err, cpierrors.TypeCloud) {
		t.Errorf("error type: want CloudError for detached disk, got %T %v", err, err)
	}
}

func TestHandleUpdateDisk_ShrinkRejected(t *testing.T) {
	const diskCID = "local-lvm:vm-9001-disk-0"
	const volid = "vm-9001-disk-0"

	callCount4 := 0
	qemuSvc := &updateDiskQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]interface{}, error) {
			callCount4++
			if callCount4 <= 2 {
				return map[string]interface{}{"scsi2": volid}, nil
			}
			return map[string]interface{}{"scsi2": volid + ",size=20G"}, nil
		},
	}

	h := handlers.HandleUpdateDisk(updateDiskDeps(qemuSvc, updateClusterWith(100), nil))
	// 5120 MiB = 5 GiB; current = 20 GiB → shrink.
	_, err := h.Handle(context.Background(), marshalArgs(diskCID, map[string]any{"size": 5120}), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error for shrink attempt")
	}
	if !cpierrors.IsType(err, cpierrors.TypeNotSupported) {
		t.Errorf("error type: want NotSupported, got %T %v", err, err)
	}
}

func TestHandleUpdateDisk_MalformedDiskCID(t *testing.T) {
	h := handlers.HandleUpdateDisk(updateDiskDeps(&updateDiskQEMUService{}, &snapClusterService{}, nil))
	_, err := h.Handle(context.Background(), marshalArgs("bad-cid-no-colon", map[string]any{}), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error for malformed disk_cid")
	}
}

func TestHandleUpdateDisk_TooFewArgs(t *testing.T) {
	h := handlers.HandleUpdateDisk(updateDiskDeps(&updateDiskQEMUService{}, &snapClusterService{}, nil))
	_, err := h.Handle(context.Background(), marshalArgs("local-lvm:vol"), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error for missing update_spec argument")
	}
}

func TestHandleUpdateDisk_IOThreadFalseRemovesOption(t *testing.T) {
	const diskCID = "local-lvm:vm-9001-disk-0"
	const volid = "vm-9001-disk-0"

	var capturedOptStr string

	callCount5 := 0
	qemuSvc := &updateDiskQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]interface{}, error) {
			callCount5++
			if callCount5 <= 2 {
				return map[string]interface{}{"scsi2": volid}, nil
			}
			return map[string]interface{}{"scsi2": volid + ",iothread=1,size=10G"}, nil
		},
		attachDiskFn: func(_ context.Context, _ string, _ int, optStr string, _ string, _ *qemu.AttachOpts) (string, error) {
			capturedOptStr = optStr
			return "scsi2", nil
		},
	}

	h := handlers.HandleUpdateDisk(updateDiskDeps(qemuSvc, updateClusterWith(100), nil))
	_, err := h.Handle(context.Background(), marshalArgs(diskCID, map[string]any{
		"iothread": false, // disable iothread
	}), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// iothread=0 or no iothread key in the merged string.
	if strings.Contains(capturedOptStr, "iothread=1") {
		t.Errorf("iothread=1 must not be in result when disabled: %q", capturedOptStr)
	}
	t.Logf("capturedOptStr = %q", capturedOptStr)
}

func TestHandleUpdateDisk_AttachSDKError(t *testing.T) {
	const diskCID = "local-lvm:vm-9001-disk-0"
	const volid = "vm-9001-disk-0"

	callCount8 := 0
	qemuSvc := &updateDiskQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]interface{}, error) {
			callCount8++
			if callCount8 <= 2 {
				return map[string]interface{}{"scsi2": volid}, nil
			}
			return map[string]interface{}{"scsi2": volid + ",size=10G"}, nil
		},
		attachDiskFn: func(_ context.Context, _ string, _ int, _ string, _ string, _ *qemu.AttachOpts) (string, error) {
			return "", errors.New("PVE config update rejected")
		},
	}

	h := handlers.HandleUpdateDisk(updateDiskDeps(qemuSvc, updateClusterWith(100), nil))
	_, err := h.Handle(context.Background(), marshalArgs(diskCID, map[string]any{
		"cache": "writeback",
	}), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error from AttachDisk SDK failure")
	}
}

func TestHandleUpdateDisk_ResizeWithUpid(t *testing.T) {
	const diskCID = "local-lvm:vm-9001-disk-0"
	const volid = "vm-9001-disk-0"

	var waitCalled bool
	tasksSvc := &mockTasksService{
		waitFn: func(_ context.Context, _, _ string, _ *tasks.WaitOptions) (*tasks.Status, error) {
			waitCalled = true
			return &tasks.Status{ExitStatus: "OK"}, nil
		},
	}

	callCount6 := 0
	qemuSvc := &updateDiskQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]interface{}, error) {
			callCount6++
			if callCount6 <= 2 {
				return map[string]interface{}{"scsi2": volid}, nil
			}
			return map[string]interface{}{"scsi2": volid + ",size=10G"}, nil
		},
		resizeDiskFn: func(_ context.Context, _ string, _ int, _ string, _ int) (string, error) {
			return "UPID:pve1:resize:abc", nil
		},
	}

	h := handlers.HandleUpdateDisk(updateDiskDeps(qemuSvc, updateClusterWith(100), tasksSvc))
	_, err := h.Handle(context.Background(), marshalArgs(diskCID, map[string]any{
		"size": 20480,
	}), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !waitCalled {
		t.Error("AwaitTask must be called when resize returns a UPID")
	}
}

func TestHandleUpdateDisk_PreservesExistingOptions(t *testing.T) {
	// Existing options not in update_spec must be preserved in the merged string.
	const diskCID = "local-lvm:vm-9001-disk-0"
	const volid = "vm-9001-disk-0"

	var capturedOptStr string

	callCount7 := 0
	qemuSvc := &updateDiskQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]interface{}, error) {
			callCount7++
			if callCount7 <= 2 {
				return map[string]interface{}{"scsi2": volid}, nil
			}
			// Existing options: size, backup.
			return map[string]interface{}{"scsi2": volid + ",backup=1,size=10G"}, nil
		},
		attachDiskFn: func(_ context.Context, _ string, _ int, optStr string, _ string, _ *qemu.AttachOpts) (string, error) {
			capturedOptStr = optStr
			return "scsi2", nil
		},
	}

	h := handlers.HandleUpdateDisk(updateDiskDeps(qemuSvc, updateClusterWith(100), nil))
	_, err := h.Handle(context.Background(), marshalArgs(diskCID, map[string]any{
		"cache": "none",
		// backup not in update_spec → must be preserved
	}), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(capturedOptStr, "backup=1") {
		t.Errorf("existing backup=1 must be preserved in merged option string: %q", capturedOptStr)
	}
	if !strings.Contains(capturedOptStr, "cache=none") {
		t.Errorf("new cache=none must be in merged option string: %q", capturedOptStr)
	}
}

func TestHandleUpdateDisk_BandwidthIOPS(t *testing.T) {
	// Exercises mbps_rd, mbps_wr, iops_rd, iops_wr option fields.
	const diskCID = "local-lvm:vm-9001-disk-0"
	const volid = "vm-9001-disk-0"

	var capturedOptStr string

	callCount9 := 0
	qemuSvc := &updateDiskQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]interface{}, error) {
			callCount9++
			if callCount9 <= 2 {
				return map[string]interface{}{"scsi2": volid}, nil
			}
			return map[string]interface{}{"scsi2": volid + ",size=10G"}, nil
		},
		attachDiskFn: func(_ context.Context, _ string, _ int, optStr string, _ string, _ *qemu.AttachOpts) (string, error) {
			capturedOptStr = optStr
			return "scsi2", nil
		},
	}

	h := handlers.HandleUpdateDisk(updateDiskDeps(qemuSvc, updateClusterWith(100), nil))
	_, err := h.Handle(context.Background(), marshalArgs(diskCID, map[string]any{
		"mbps_rd": 100,
		"mbps_wr": 50,
		"iops_rd": 200,
		"iops_wr": 100,
	}), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, want := range []string{"mbps_rd=100", "mbps_wr=50", "iops_rd=200", "iops_wr=100"} {
		if !strings.Contains(capturedOptStr, want) {
			t.Errorf("option string must contain %q, got %q", want, capturedOptStr)
		}
	}
}

func TestHandleUpdateDisk_SSDAndBackup(t *testing.T) {
	// Exercises ssd and backup bool options.
	const diskCID = "local-lvm:vm-9001-disk-0"
	const volid = "vm-9001-disk-0"

	var capturedOptStr string

	callCount10 := 0
	qemuSvc := &updateDiskQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]interface{}, error) {
			callCount10++
			if callCount10 <= 2 {
				return map[string]interface{}{"scsi2": volid}, nil
			}
			return map[string]interface{}{"scsi2": volid}, nil
		},
		attachDiskFn: func(_ context.Context, _ string, _ int, optStr string, _ string, _ *qemu.AttachOpts) (string, error) {
			capturedOptStr = optStr
			return "scsi2", nil
		},
	}

	h := handlers.HandleUpdateDisk(updateDiskDeps(qemuSvc, updateClusterWith(100), nil))
	_, err := h.Handle(context.Background(), marshalArgs(diskCID, map[string]any{
		"ssd":    true,
		"backup": true,
	}), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(capturedOptStr, "ssd=1") {
		t.Errorf("option string must contain ssd=1, got %q", capturedOptStr)
	}
	if !strings.Contains(capturedOptStr, "backup=1") {
		t.Errorf("option string must contain backup=1, got %q", capturedOptStr)
	}
}

func TestHandleUpdateDisk_NullSpec_NoOp(t *testing.T) {
	// Null update_spec (JSON null) → treated as empty map → no-op.
	const diskCID = "local-lvm:vm-9001-disk-0"
	const volid = "vm-9001-disk-0"

	qemuSvc := &updateDiskQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]interface{}, error) {
			return map[string]interface{}{"scsi2": volid}, nil
		},
	}

	h := handlers.HandleUpdateDisk(updateDiskDeps(qemuSvc, updateClusterWith(100), nil))
	_, err := h.Handle(context.Background(), []json.RawMessage{
		json.RawMessage(`"local-lvm:vm-9001-disk-0"`),
		json.RawMessage(`null`),
	}, jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error for null spec: %v", err)
	}
}
