package handlers_test

import (
	"context"
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
// resizeQEMUService: QEMU mock for resize_disk tests.
// ---------------------------------------------------------------------------

type resizeQEMUService struct {
	configFn     func(ctx context.Context, node string, vmid int) (map[string]interface{}, error)
	resizeDiskFn func(ctx context.Context, node string, vmid int, diskID string, sizeGiB int) (string, error)
	// listSnapshotsFn controls ListSnapshots for snapshot guard tests.
	// nil → return only the synthetic "current" entry (no real snapshots),
	// so existing tests are unaffected by the guard.
	listSnapshotsFn func(ctx context.Context, node string, vmid int) ([]map[string]interface{}, error)
}

func (m *resizeQEMUService) Config(ctx context.Context, node string, vmid int) (map[string]interface{}, error) {
	if m.configFn != nil {
		return m.configFn(ctx, node, vmid)
	}
	return map[string]interface{}{}, nil
}

func (m *resizeQEMUService) ResizeDisk(ctx context.Context, node string, vmid int, diskID string, sizeGiB int) (string, error) {
	if m.resizeDiskFn != nil {
		return m.resizeDiskFn(ctx, node, vmid, diskID, sizeGiB)
	}
	return "", nil
}

// Unimplemented methods.
func (m *resizeQEMUService) Create(_ context.Context, _ string, _ map[string]interface{}) (string, error) {
	panic("resizeQEMUService.Create: not expected")
}
func (m *resizeQEMUService) Status(_ context.Context, _ string, _ int) (map[string]interface{}, error) {
	panic("resizeQEMUService.Status: not expected")
}
func (m *resizeQEMUService) Start(_ context.Context, _ string, _ int) (string, error) {
	panic("resizeQEMUService.Start: not expected")
}
func (m *resizeQEMUService) Stop(_ context.Context, _ string, _ int) (string, error) {
	panic("resizeQEMUService.Stop: not expected")
}
func (m *resizeQEMUService) Reset(_ context.Context, _ string, _ int) (string, error) {
	panic("resizeQEMUService.Reset: not expected")
}
func (m *resizeQEMUService) Clone(_ context.Context, _ string, _ int, _ map[string]interface{}) (string, error) {
	panic("resizeQEMUService.Clone: not expected")
}
func (m *resizeQEMUService) Template(_ context.Context, _ string, _ int) (string, error) {
	panic("resizeQEMUService.Template: not expected")
}
func (m *resizeQEMUService) AttachDisk(_ context.Context, _ string, _ int, _ string, _ string, _ *qemu.AttachOpts) (string, error) {
	panic("resizeQEMUService.AttachDisk: not expected")
}
func (m *resizeQEMUService) DetachDisk(_ context.Context, _ string, _ int, _ string) error {
	panic("resizeQEMUService.DetachDisk: not expected")
}
func (m *resizeQEMUService) Snapshot(_ context.Context, _ string, _ int, _ string, _ map[string]interface{}) (string, error) {
	panic("resizeQEMUService.Snapshot: not expected")
}
func (m *resizeQEMUService) DeleteSnapshot(_ context.Context, _ string, _ int, _ string) error {
	panic("resizeQEMUService.DeleteSnapshot: not expected")
}
func (m *resizeQEMUService) ListSnapshots(
	ctx context.Context, node string, vmid int,
) ([]map[string]interface{}, error) {
	if m.listSnapshotsFn != nil {
		return m.listSnapshotsFn(ctx, node, vmid)
	}
	// Safe default: return only the synthetic "current" entry so the guard
	// proceeds normally in tests that do not exercise snapshot behaviour.
	return []map[string]interface{}{{"name": "current"}}, nil
}
func (m *resizeQEMUService) RollbackSnapshot(_ context.Context, _ string, _ int, _ string) (string, error) {
	panic("resizeQEMUService.RollbackSnapshot: not expected")
}

var _ qemu.Service = (*resizeQEMUService)(nil)

// ---------------------------------------------------------------------------
// resizeDeps builds Deps for resize_disk tests.
// ---------------------------------------------------------------------------

func resizeDeps(qemuSvc qemu.Service, clusterSvc sdkclusterapi.Service, tasksSvc tasks.Service) handlers.Deps {
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

// resizeClusterWith returns a cluster service that reports one VM (vmid on pve1).
func resizeClusterWith(vmid int) sdkclusterapi.Service {
	return &snapClusterService{
		listFn: func(_ context.Context, _ *sdkclusterapi.ListResourcesParams) (*sdkclusterapi.ListResourcesResponse, error) {
			return clusterRespWith(vmid, "pve1"), nil
		},
	}
}

// resizeQEMUWithDisk returns a QEMU mock that serves config with the given disk slot.
// diskOptStr must be in the format "<bareVolid>[,options...]" (e.g. "local-lvm:vm-9001-disk-0,size=10G").
// The first two Config calls (from findVMByDiskVolid and ResolveDiskID) return the bare volid
// so FindDiskIDByVolID can match exactly. Subsequent calls return the full option string so
// parseDiskSizeGiB can read the size field.
func resizeQEMUWithDisk(diskSlot, diskOptStr string, resizeFn func(ctx context.Context, node string, vmid int, diskID string, sizeGiB int) (string, error)) *resizeQEMUService {
	// Extract bare volid (part before first comma).
	bareVolid := diskOptStr
	if idx := strings.IndexByte(diskOptStr, ','); idx >= 0 {
		bareVolid = diskOptStr[:idx]
	}
	callCount := 0
	return &resizeQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]interface{}, error) {
			callCount++
			// Calls 1 and 2: findVMByDiskVolid + ResolveDiskID — need exact bare volid match.
			if callCount <= 2 {
				return map[string]interface{}{diskSlot: bareVolid}, nil
			}
			// Call 3+: parseDiskSizeGiB — need full option string with size=.
			return map[string]interface{}{diskSlot: diskOptStr}, nil
		},
		resizeDiskFn: resizeFn,
	}
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestHandleResizeDisk_Grow(t *testing.T) {
	const diskCID = "local-lvm:vm-9001-disk-0"
	const volid = "local-lvm:vm-9001-disk-0"

	var capturedDelta int

	qemuSvc := resizeQEMUWithDisk("scsi2", volid+",size=10G", func(_ context.Context, _ string, _ int, _ string, deltaGiB int) (string, error) {
		capturedDelta = deltaGiB
		return "", nil
	})

	h := handlers.HandleResizeDisk(resizeDeps(qemuSvc, resizeClusterWith(100), nil))

	// 15360 MiB = 15 GiB; current = 10 GiB → delta = 5 GiB.
	result, err := h.Handle(context.Background(), marshalArgs(diskCID, 15360), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != nil {
		t.Errorf("result: want nil (void), got %v", result)
	}
	if capturedDelta != 5 {
		t.Errorf("resize delta: want 5 GiB, got %d GiB", capturedDelta)
	}
}

func TestHandleResizeDisk_NoOp(t *testing.T) {
	// new_size_mb == current_size → delta == 0 → no-op, no resize call.
	const diskCID = "local-lvm:vm-9001-disk-0"

	var resizeCalled bool

	qemuSvc := resizeQEMUWithDisk("scsi2", "local-lvm:vm-9001-disk-0,size=10G", func(_ context.Context, _ string, _ int, _ string, _ int) (string, error) {
		resizeCalled = true
		return "", nil
	})

	h := handlers.HandleResizeDisk(resizeDeps(qemuSvc, resizeClusterWith(100), nil))
	// 10240 MiB = 10 GiB exactly, same as current.
	_, err := h.Handle(context.Background(), marshalArgs(diskCID, 10240), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error for no-op resize: %v", err)
	}
	if resizeCalled {
		t.Error("ResizeDisk must not be called when delta is zero")
	}
}

func TestHandleResizeDisk_ShrinkRejected(t *testing.T) {
	// new_size_mb < current_size → NotSupported error.
	const diskCID = "local-lvm:vm-9001-disk-0"

	qemuSvc := resizeQEMUWithDisk("scsi2", "local-lvm:vm-9001-disk-0,size=20G", nil)

	h := handlers.HandleResizeDisk(resizeDeps(qemuSvc, resizeClusterWith(100), nil))
	// 5120 MiB = 5 GiB; current = 20 GiB → shrink.
	_, err := h.Handle(context.Background(), marshalArgs(diskCID, 5120), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error for shrink attempt")
	}
	if !cpierrors.IsType(err, cpierrors.TypeNotSupported) {
		t.Errorf("error type: want NotSupported, got %T %v", err, err)
	}
}

func TestHandleResizeDisk_WithUpid(t *testing.T) {
	const diskCID = "local-lvm:vm-9001-disk-0"

	var waitCalled bool
	tasksSvc := &mockTasksService{
		waitFn: func(_ context.Context, _, _ string, _ *tasks.WaitOptions) (*tasks.Status, error) {
			waitCalled = true
			return &tasks.Status{ExitStatus: "OK"}, nil
		},
	}

	qemuSvc := resizeQEMUWithDisk("scsi2", "local-lvm:vm-9001-disk-0,size=10G", func(_ context.Context, _ string, _ int, _ string, _ int) (string, error) {
		return "UPID:pve1:resize:abc", nil
	})

	h := handlers.HandleResizeDisk(resizeDeps(qemuSvc, resizeClusterWith(100), tasksSvc))
	_, err := h.Handle(context.Background(), marshalArgs(diskCID, 20480), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !waitCalled {
		t.Error("AwaitTask was not called for UPID")
	}
}

func TestHandleResizeDisk_DiskNotAttached(t *testing.T) {
	const diskCID = "local-lvm:vm-9001-disk-0"

	// Cluster has a VM but it doesn't have the disk.
	qemuSvc := &resizeQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]interface{}, error) {
			return map[string]interface{}{"scsi0": "local-lvm:other"}, nil
		},
	}

	h := handlers.HandleResizeDisk(resizeDeps(qemuSvc, resizeClusterWith(100), nil))
	_, err := h.Handle(context.Background(), marshalArgs(diskCID, 20480), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error for disk not attached to any VM")
	}
}

func TestHandleResizeDisk_SizeParseFail(t *testing.T) {
	// Disk has no "size=" in option string → parseDiskSizeGiB returns error.
	const diskCID = "local-lvm:vm-9001-disk-0"

	qemuSvc := resizeQEMUWithDisk("scsi2", "local-lvm:vm-9001-disk-0,cache=writeback", nil)

	h := handlers.HandleResizeDisk(resizeDeps(qemuSvc, resizeClusterWith(100), nil))
	_, err := h.Handle(context.Background(), marshalArgs(diskCID, 20480), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error when size cannot be parsed from config")
	}
}

func TestHandleResizeDisk_ResizeSDKError(t *testing.T) {
	const diskCID = "local-lvm:vm-9001-disk-0"

	qemuSvc := resizeQEMUWithDisk("scsi2", "local-lvm:vm-9001-disk-0,size=10G", func(_ context.Context, _ string, _ int, _ string, _ int) (string, error) {
		return "", errors.New("PVE refused resize")
	})

	h := handlers.HandleResizeDisk(resizeDeps(qemuSvc, resizeClusterWith(100), nil))
	_, err := h.Handle(context.Background(), marshalArgs(diskCID, 20480), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error from ResizeDisk SDK failure")
	}
}

func TestHandleResizeDisk_MalformedDiskCID(t *testing.T) {
	h := handlers.HandleResizeDisk(resizeDeps(&resizeQEMUService{}, &snapClusterService{}, nil))
	_, err := h.Handle(context.Background(), marshalArgs("no-colon-cid", 10240), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error for malformed disk_cid")
	}
}

func TestHandleResizeDisk_ZeroSizeMB(t *testing.T) {
	h := handlers.HandleResizeDisk(resizeDeps(&resizeQEMUService{}, &snapClusterService{}, nil))
	_, err := h.Handle(context.Background(), marshalArgs("local-lvm:vol", 0), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error for zero new_size_mb")
	}
}

func TestHandleResizeDisk_TooFewArgs(t *testing.T) {
	h := handlers.HandleResizeDisk(resizeDeps(&resizeQEMUService{}, &snapClusterService{}, nil))
	_, err := h.Handle(context.Background(), marshalArgs("local-lvm:vol"), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error for missing new_size_mb argument")
	}
}

// With no Config.Node and an empty cluster (no VMs holding the disk), the
// handler must fail with a "disk not attached to any VM" error rather than
// panicking. This replaces the legacy missing-node assertion: under the
// backend abstraction Config.Node is no longer mandatory — the cluster scan
// drives node selection, and a successful scan with no matches is the new
// failure surface.
func TestHandleResizeDisk_NoConfigNodeAndEmptyCluster(t *testing.T) {
	h := handlers.HandleResizeDisk(handlers.Deps{
		Config: &config.CPIConfig{Node: ""},
		PVE: &mockPVEClient{
			qemuSvc:    &resizeQEMUService{},
			clusterSvc: &snapClusterService{},
		},
		Logger: log.NewNopLogger(),
	})
	_, err := h.Handle(context.Background(), marshalArgs("local-lvm:vol", 10240), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error when no VM hosts the disk")
	}
}

func TestHandleResizeDisk_CeilingMath(t *testing.T) {
	// 10241 MiB → ceil → 11 GiB; current = 10 GiB → delta = 1 GiB.
	const diskCID = "local-lvm:vm-9001-disk-0"

	var capturedDelta int

	qemuSvc := resizeQEMUWithDisk("scsi2", "local-lvm:vm-9001-disk-0,size=10G", func(_ context.Context, _ string, _ int, _ string, deltaGiB int) (string, error) {
		capturedDelta = deltaGiB
		return "", nil
	})

	h := handlers.HandleResizeDisk(resizeDeps(qemuSvc, resizeClusterWith(100), nil))
	_, err := h.Handle(context.Background(), marshalArgs(diskCID, 10241), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedDelta != 1 {
		t.Errorf("delta: want 1 GiB for 10241 MiB -> 11 GiB - 10 GiB, got %d GiB", capturedDelta)
	}
}

// ---------------------------------------------------------------------------
// Snapshot guard tests.
// ---------------------------------------------------------------------------

// resizeDepsWithConfig builds Deps using a caller-supplied *config.CPIConfig.
func resizeDepsWithConfig(
	cfg *config.CPIConfig, qemuSvc qemu.Service, clusterSvc sdkclusterapi.Service, tasksSvc tasks.Service,
) handlers.Deps {
	return handlers.Deps{
		Config: cfg,
		PVE: &mockPVEClient{
			qemuSvc:    qemuSvc,
			clusterSvc: clusterSvc,
			tasksSvc:   tasksSvc,
		},
		Logger: log.NewNopLogger(),
	}
}

// resizeQEMUWithDiskAndSnapshots returns a mock with snapshot control.
func resizeQEMUWithDiskAndSnapshots(
	diskSlot, diskOptStr string,
	resizeFn func(ctx context.Context, node string, vmid int, diskID string, sizeGiB int) (string, error),
	snapshotFn func(ctx context.Context, node string, vmid int) ([]map[string]interface{}, error),
) *resizeQEMUService {
	svc := resizeQEMUWithDisk(diskSlot, diskOptStr, resizeFn)
	svc.listSnapshotsFn = snapshotFn
	return svc
}

func TestHandleResizeDisk_SnapshotsPresent_HardFail(t *testing.T) {
	// Snapshots exist, AllowDiskOpsWithSnapshots=false → Cloud error; ResizeDisk NOT called.
	const diskCID = "local-lvm:vm-9001-disk-0"

	var resizeCalled bool
	qemuSvc := resizeQEMUWithDiskAndSnapshots(
		"scsi2", "local-lvm:vm-9001-disk-0,size=10G",
		func(_ context.Context, _ string, _ int, _ string, _ int) (string, error) {
			resizeCalled = true
			return "", nil
		},
		func(_ context.Context, _ string, _ int) ([]map[string]interface{}, error) {
			return []map[string]interface{}{
				{"name": "snap1"},
				{"name": "snap2"},
			}, nil
		},
	)

	cfg := &config.CPIConfig{
		Node:                      "pve1",
		AllowDiskOpsWithSnapshots: false,
	}
	h := handlers.HandleResizeDisk(resizeDepsWithConfig(cfg, qemuSvc, resizeClusterWith(100), nil))
	_, err := h.Handle(context.Background(), marshalArgs(diskCID, 20480), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error when snapshots exist and allow_disk_ops_with_snapshots=false")
	}
	if !cpierrors.IsType(err, cpierrors.TypeCloud) {
		t.Errorf("error type: want Cloud, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "snap1") || !strings.Contains(err.Error(), "snap2") {
		t.Errorf("error message should contain snapshot names; got: %v", err)
	}
	if !strings.Contains(err.Error(), "allow_disk_ops_with_snapshots") {
		t.Errorf("error message should contain remediation hint; got: %v", err)
	}
	if resizeCalled {
		t.Error("ResizeDisk must not be called when guard blocks")
	}
}

func TestHandleResizeDisk_NoSnapshots_Proceeds(t *testing.T) {
	// No real snapshots → guard passes → ResizeDisk called.
	const diskCID = "local-lvm:vm-9001-disk-0"

	var resizeCalled bool
	qemuSvc := resizeQEMUWithDiskAndSnapshots(
		"scsi2", "local-lvm:vm-9001-disk-0,size=10G",
		func(_ context.Context, _ string, _ int, _ string, _ int) (string, error) {
			resizeCalled = true
			return "", nil
		},
		func(_ context.Context, _ string, _ int) ([]map[string]interface{}, error) {
			// Only the synthetic "current" entry — no real snapshots.
			return []map[string]interface{}{{"name": "current"}}, nil
		},
	)

	cfg := &config.CPIConfig{Node: "pve1"}
	h := handlers.HandleResizeDisk(resizeDepsWithConfig(cfg, qemuSvc, resizeClusterWith(100), nil))
	_, err := h.Handle(context.Background(), marshalArgs(diskCID, 20480), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error when no snapshots: %v", err)
	}
	if !resizeCalled {
		t.Error("ResizeDisk should be called when no real snapshots exist")
	}
}

func TestHandleResizeDisk_SnapshotCheckError_FailOpen(t *testing.T) {
	// ListSnapshots returns error, RequireSnapshotCheckPass=false → WARN + proceed (ResizeDisk called).
	const diskCID = "local-lvm:vm-9001-disk-0"

	var resizeCalled bool
	qemuSvc := resizeQEMUWithDiskAndSnapshots(
		"scsi2", "local-lvm:vm-9001-disk-0,size=10G",
		func(_ context.Context, _ string, _ int, _ string, _ int) (string, error) {
			resizeCalled = true
			return "", nil
		},
		func(_ context.Context, _ string, _ int) ([]map[string]interface{}, error) {
			return nil, errors.New("PVE API timeout")
		},
	)

	cfg := &config.CPIConfig{
		Node:                     "pve1",
		RequireSnapshotCheckPass: false,
	}
	h := handlers.HandleResizeDisk(resizeDepsWithConfig(cfg, qemuSvc, resizeClusterWith(100), nil))
	_, err := h.Handle(context.Background(), marshalArgs(diskCID, 20480), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("expected fail-open: no error when RequireSnapshotCheckPass=false; got: %v", err)
	}
	if !resizeCalled {
		t.Error("ResizeDisk should be called in fail-open mode")
	}
}

func TestHandleResizeDisk_SnapshotCheckError_FailClosed(t *testing.T) {
	// ListSnapshots returns error, RequireSnapshotCheckPass=true → error returned; ResizeDisk NOT called.
	const diskCID = "local-lvm:vm-9001-disk-0"

	var resizeCalled bool
	qemuSvc := resizeQEMUWithDiskAndSnapshots(
		"scsi2", "local-lvm:vm-9001-disk-0,size=10G",
		func(_ context.Context, _ string, _ int, _ string, _ int) (string, error) {
			resizeCalled = true
			return "", nil
		},
		func(_ context.Context, _ string, _ int) ([]map[string]interface{}, error) {
			return nil, errors.New("PVE API timeout")
		},
	)

	cfg := &config.CPIConfig{
		Node:                     "pve1",
		RequireSnapshotCheckPass: true,
	}
	h := handlers.HandleResizeDisk(resizeDepsWithConfig(cfg, qemuSvc, resizeClusterWith(100), nil))
	_, err := h.Handle(context.Background(), marshalArgs(diskCID, 20480), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error when RequireSnapshotCheckPass=true and ListSnapshots fails")
	}
	if !strings.Contains(err.Error(), "require_snapshot_check_pass") {
		t.Errorf("error message should mention require_snapshot_check_pass; got: %v", err)
	}
	if resizeCalled {
		t.Error("ResizeDisk must not be called when fail-closed guard blocks")
	}
}

func TestHandleResizeDisk_SnapshotsPresent_AllowOverride(t *testing.T) {
	// Snapshots exist, AllowDiskOpsWithSnapshots=true → WARN + ResizeDisk called.
	const diskCID = "local-lvm:vm-9001-disk-0"

	var resizeCalled bool
	qemuSvc := resizeQEMUWithDiskAndSnapshots(
		"scsi2", "local-lvm:vm-9001-disk-0,size=10G",
		func(_ context.Context, _ string, _ int, _ string, _ int) (string, error) {
			resizeCalled = true
			return "", nil
		},
		func(_ context.Context, _ string, _ int) ([]map[string]interface{}, error) {
			return []map[string]interface{}{{"name": "snap1"}}, nil
		},
	)

	cfg := &config.CPIConfig{
		Node:                      "pve1",
		AllowDiskOpsWithSnapshots: true,
	}
	h := handlers.HandleResizeDisk(resizeDepsWithConfig(cfg, qemuSvc, resizeClusterWith(100), nil))
	_, err := h.Handle(context.Background(), marshalArgs(diskCID, 20480), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("expected no error with allow_disk_ops_with_snapshots=true; got: %v", err)
	}
	if !resizeCalled {
		t.Error("ResizeDisk should be called when allow override is set")
	}
}

// ---------------------------------------------------------------------------
// Error-path gap tests (gaps #1, #2, #3 from R1).
// ---------------------------------------------------------------------------

func TestHandleResizeDisk_ConfigFetchError(t *testing.T) {
	// Gap #1: QEMU().Config() returns an error after the disk is located.
	// The handler must propagate the error rather than proceeding with
	// an unknown current size.
	const diskCID = "local-lvm:vm-9001-disk-0"
	const diskSlot = "scsi2"

	configErr := errors.New("PVE config fetch timeout")
	callCount := 0
	qemuSvc := &resizeQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]interface{}, error) {
			callCount++
			// Calls 1-2: FindVMByDiskVolid + ResolveDiskID need the bare volid.
			if callCount <= 2 {
				return map[string]interface{}{diskSlot: diskCID}, nil
			}
			// Call 3: the real Config() read — return error.
			return nil, configErr
		},
	}

	h := handlers.HandleResizeDisk(resizeDeps(qemuSvc, resizeClusterWith(100), nil))
	_, err := h.Handle(context.Background(), marshalArgs(diskCID, 20480), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error when Config() fails after disk is located")
	}
	if !strings.Contains(err.Error(), "config") {
		t.Errorf("error should mention config read failure; got: %v", err)
	}
}

func TestHandleResizeDisk_UnknownSizeUnit(t *testing.T) {
	// parseDiskSizeGiB accepts K/M/G/T/P case-insensitive; any other unit
	// suffix is rejected. Confirm "xyz" trailing characters trigger the
	// handler's error path.
	const diskCID = "local-lvm:vm-9001-disk-0"
	qemuSvc := resizeQEMUWithDisk("scsi2", "local-lvm:vm-9001-disk-0,size=100xyz", nil)

	h := handlers.HandleResizeDisk(resizeDeps(qemuSvc, resizeClusterWith(100), nil))
	_, err := h.Handle(context.Background(), marshalArgs(diskCID, 20480), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error for unsupported size unit")
	}
}

func TestHandleResizeDisk_AwaitTaskFailure(t *testing.T) {
	// Gap #3: ResizeDisk returns a UPID; AwaitTask returns a non-OK ExitStatus.
	// AwaitTaskWithLogger (pve/task.go) wraps non-OK exit status as a Cloud
	// error; RetryOnTransientOrLock propagates it; the handler wraps it again
	// with context. The caller must receive a non-nil error.
	const diskCID = "local-lvm:vm-9001-disk-0"

	qemuSvc := resizeQEMUWithDisk("scsi2", "local-lvm:vm-9001-disk-0,size=10G",
		func(_ context.Context, _ string, _ int, _ string, _ int) (string, error) {
			return "UPID:pve1:resize:deadbeef", nil
		},
	)

	tasksSvc := &mockTasksService{
		waitFn: func(_ context.Context, _, _ string, _ *tasks.WaitOptions) (*tasks.Status, error) {
			return &tasks.Status{ExitStatus: "ERROR: resize failed"}, nil
		},
	}

	h := handlers.HandleResizeDisk(resizeDeps(qemuSvc, resizeClusterWith(100), tasksSvc))
	_, err := h.Handle(context.Background(), marshalArgs(diskCID, 20480), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error when AwaitTask returns non-OK ExitStatus")
	}
}

// ---------------------------------------------------------------------------
// Storage-type CID-variant tests.
// resize_disk has no storage-type branching; these tests exercise
// ParseDiskCID with different volume-format strings and confirm the handler
// calls ResizeDisk when the disk is attached.
// ---------------------------------------------------------------------------

func TestHandleResizeDisk_Dir_CID(t *testing.T) {
	// dir-style CID: storage="local", volume="9001/vm-9001-disk-0.raw".
	// ParseDiskCID splits on the first colon only; the subpath segment is
	// opaque to the handler. FindVMByDiskVolid matches on the full CID string.
	const diskCID = "local:9001/vm-9001-disk-0.raw"
	const diskSlot = "scsi0"

	var resizeCalled bool
	qemuSvc := resizeQEMUWithDisk(diskSlot, diskCID+",size=20G",
		func(_ context.Context, _ string, _ int, _ string, _ int) (string, error) {
			resizeCalled = true
			return "", nil
		},
	)

	h := handlers.HandleResizeDisk(resizeDeps(qemuSvc, resizeClusterWith(9001), nil))
	_, err := h.Handle(context.Background(), marshalArgs(diskCID, 30720), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error for dir-style CID: %v", err)
	}
	if !resizeCalled {
		t.Error("ResizeDisk must be called for dir-style CID")
	}
}

func TestHandleResizeDisk_ZFSPool_CID(t *testing.T) {
	// zfspool-style CID: bare volume name, no subpath or extension.
	// ParseDiskCID splits on colon; volume segment is "vm-9001-disk-0".
	const diskCID = "local-zfs:vm-9001-disk-0"
	const diskSlot = "scsi1"

	var resizeCalled bool
	qemuSvc := resizeQEMUWithDisk(diskSlot, diskCID+",size=10G",
		func(_ context.Context, _ string, _ int, _ string, _ int) (string, error) {
			resizeCalled = true
			return "", nil
		},
	)

	h := handlers.HandleResizeDisk(resizeDeps(qemuSvc, resizeClusterWith(9001), nil))
	_, err := h.Handle(context.Background(), marshalArgs(diskCID, 20480), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error for zfspool CID: %v", err)
	}
	if !resizeCalled {
		t.Error("ResizeDisk must be called for zfspool CID")
	}
}

func TestHandleResizeDisk_LVMThin_CID(t *testing.T) {
	// lvmthin-style CID: bare volume name, same format as lvm.
	// ParseDiskCID splits on colon; volume segment is "vm-9001-disk-0".
	const diskCID = "local-lvm-thin:vm-9001-disk-0"
	const diskSlot = "scsi3"

	var resizeCalled bool
	qemuSvc := resizeQEMUWithDisk(diskSlot, diskCID+",size=15G",
		func(_ context.Context, _ string, _ int, _ string, _ int) (string, error) {
			resizeCalled = true
			return "", nil
		},
	)

	h := handlers.HandleResizeDisk(resizeDeps(qemuSvc, resizeClusterWith(9001), nil))
	_, err := h.Handle(context.Background(), marshalArgs(diskCID, 20480), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error for lvmthin CID: %v", err)
	}
	if !resizeCalled {
		t.Error("ResizeDisk must be called for lvmthin CID")
	}
}

// TODO(storage-network): nfs — wired to PVE network-call boundary, stubbed pending
// live shared-storage test infrastructure. Storage: nfs-store:9001/vm-9001-disk-0.qcow2. Re-enable when
// integration-test harness provides a nfs pool via env.
//
// func TestHandleResizeDisk_NFS_CID(t *testing.T) { ... }

// TODO(storage-network): rbd — wired to PVE network-call boundary, stubbed pending
// live shared-storage test infrastructure. Storage: ceph-pool:vm-9001-disk-0. Re-enable when
// integration-test harness provides a rbd pool via env.
//
// func TestHandleResizeDisk_RBD_CID(t *testing.T) { ... }

// TODO(storage-network): cephfs — wired to PVE network-call boundary, stubbed pending
// live shared-storage test infrastructure. Storage: cephfs-pool:vm-9001-disk-0. Re-enable when
// integration-test harness provides a cephfs pool via env.
//
// func TestHandleResizeDisk_CephFS_CID(t *testing.T) { ... }

// TODO(storage-network): cifs — wired to PVE network-call boundary, stubbed pending
// live shared-storage test infrastructure. Storage: cifs-store:9001/vm-9001-disk-0.qcow2. Re-enable when
// integration-test harness provides a cifs pool via env.
//
// func TestHandleResizeDisk_CIFS_CID(t *testing.T) { ... }
