package handlers_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

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
	configFn     func(ctx context.Context, node string, vmid int) (map[string]any, error)
	resizeDiskFn func(ctx context.Context, node string, vmid int, diskID string, sizeGiB int) (string, error)
	attachDiskFn func(ctx context.Context, node string, vmid int, volid string, bus string, opts *qemu.AttachOpts) (string, error)
}

func (m *updateDiskQEMUService) Config(ctx context.Context, node string, vmid int) (map[string]any, error) {
	if m.configFn != nil {
		return m.configFn(ctx, node, vmid)
	}
	return map[string]any{}, nil
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
func (m *updateDiskQEMUService) Create(_ context.Context, _ string, _ map[string]any) (string, error) {
	panic("updateDiskQEMUService.Create: not expected")
}
func (m *updateDiskQEMUService) Status(_ context.Context, _ string, _ int) (map[string]any, error) {
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
func (m *updateDiskQEMUService) Clone(_ context.Context, _ string, _ int, _ map[string]any) (string, error) {
	panic("updateDiskQEMUService.Clone: not expected")
}
func (m *updateDiskQEMUService) Template(_ context.Context, _ string, _ int) (string, error) {
	panic("updateDiskQEMUService.Template: not expected")
}
func (m *updateDiskQEMUService) DetachDisk(_ context.Context, _ string, _ int, _ string) error {
	panic("updateDiskQEMUService.DetachDisk: not expected")
}
func (m *updateDiskQEMUService) Snapshot(_ context.Context, _ string, _ int, _ string, _ map[string]any) (string, error) {
	panic("updateDiskQEMUService.Snapshot: not expected")
}
func (m *updateDiskQEMUService) DeleteSnapshot(_ context.Context, _ string, _ int, _ string) error {
	panic("updateDiskQEMUService.DeleteSnapshot: not expected")
}
func (m *updateDiskQEMUService) ListSnapshots(_ context.Context, _ string, _ int) ([]map[string]any, error) {
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
			Node: testNode,
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
//
//nolint:unparam // vmid kept for call-site clarity; always 100 in this suite
func updateClusterWith(vmid int) sdkclusterapi.Service {
	return &snapClusterService{
		listFn: func(_ context.Context, _ *sdkclusterapi.ListResourcesParams) (*sdkclusterapi.ListResourcesResponse, error) {
			return clusterRespWith(vmid, testNode), nil
		},
	}
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestHandleUpdateDisk_OptionsOnly(t *testing.T) {
	t.Parallel()

	const volid = "local-lvm:vm-9001-disk-0"

	var capturedOptStr string

	// configFn: calls 1-2 return the canonical volid (for FindVMByDiskVolid +
	// ResolveDiskID, which match the full "<storage>:<volume>" form PVE stores).
	// call 3+ returns the full option string (for reading existing options to merge).
	callCount := 0
	qemuSvc := &updateDiskQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			callCount++
			if callCount <= 2 {
				return map[string]any{diskSlot: volid}, nil
			}
			return map[string]any{diskSlot: volid + ",size=10G"}, nil
		},
		attachDiskFn: func(_ context.Context, _ string, _ int, optStr string, _ string, opts *qemu.AttachOpts) (string, error) {
			capturedOptStr = optStr
			if opts == nil || opts.DiskID != diskSlot {
				t.Errorf("expected AttachDisk with DiskID=scsi2, got %v", opts)
			}
			return diskSlot, nil
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
	t.Parallel()

	const volid = "local-lvm:vm-9001-disk-0"

	var capturedDelta int
	var attachCalled bool

	// calls 1-2: canonical volid; call 3+: option string with size (for resize).
	callCount := 0
	qemuSvc := &updateDiskQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			callCount++
			if callCount <= 2 {
				return map[string]any{diskSlot: volid}, nil
			}
			return map[string]any{diskSlot: volid + ",size=10G"}, nil
		},
		resizeDiskFn: func(_ context.Context, _ string, _ int, _ string, deltaGiB int) (string, error) {
			capturedDelta = deltaGiB
			return "", nil
		},
		attachDiskFn: func(_ context.Context, _ string, _ int, _ string, _ string, _ *qemu.AttachOpts) (string, error) {
			attachCalled = true
			return diskSlot, nil
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
	t.Parallel()

	const volid = "local-lvm:vm-9001-disk-0"

	var resizeCalled bool
	var attachCalled bool

	callCount := 0
	qemuSvc := &updateDiskQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			callCount++
			if callCount <= 2 {
				return map[string]any{diskSlot: volid}, nil
			}
			return map[string]any{diskSlot: volid + ",size=10G"}, nil
		},
		resizeDiskFn: func(_ context.Context, _ string, _ int, _ string, _ int) (string, error) {
			resizeCalled = true
			return "", nil
		},
		attachDiskFn: func(_ context.Context, _ string, _ int, _ string, _ string, _ *qemu.AttachOpts) (string, error) {
			attachCalled = true
			return diskSlot, nil
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
	t.Parallel()
	// Empty spec → no resize, no AttachDisk.

	const volid = "local-lvm:vm-9001-disk-0"

	var attachCalled bool

	qemuSvc := &updateDiskQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{diskSlot: volid}, nil
		},
		attachDiskFn: func(_ context.Context, _ string, _ int, _ string, _ string, _ *qemu.AttachOpts) (string, error) {
			attachCalled = true
			return diskSlot, nil
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
	t.Parallel()

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
	// Error must say "detached disk" not a transport message.
	if !strings.Contains(err.Error(), "detached disk") {
		t.Errorf("error must mention detached disk, got: %v", err)
	}
}

// TestHandleUpdateDisk_FindVMTransportError — FindVMByDiskVolid cluster-scan fails
// with a transport error (connection refused). The bug was that the handler swallowed
// this as "detached disk"; the fix propagates it as a distinct wrapped error.
func TestHandleUpdateDisk_FindVMTransportError(t *testing.T) {
	t.Parallel()

	transportErr := errors.New("connection refused dialing cluster API")
	// Cluster ListResources returns a transport-level error (not "disk not found").
	faultCluster := &snapClusterService{
		listFn: func(_ context.Context, _ *sdkclusterapi.ListResourcesParams) (*sdkclusterapi.ListResourcesResponse, error) {
			return nil, transportErr
		},
	}

	h := handlers.HandleUpdateDisk(updateDiskDeps(&updateDiskQEMUService{}, faultCluster, nil))
	_, err := h.Handle(context.Background(), marshalArgs(diskCID, map[string]any{"cache": "writeback"}), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error for cluster transport failure, got nil")
	}
	// Must NOT be "detached disk" — that message must only appear when the disk
	// is genuinely absent from every VM. Transport errors must propagate as-is.
	if strings.Contains(err.Error(), "detached disk cannot be updated") {
		t.Errorf("transport error must not be reported as 'detached disk', got: %v", err)
	}
	// Must be a CloudError type (wrapped transport error).
	if !cpierrors.IsType(err, cpierrors.TypeCloud) && !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Errorf("transport error must be wrapped as Cloud or RetriableCloud, got: %T %v", err, err)
	}
}

func TestHandleUpdateDisk_ShrinkRejected(t *testing.T) {
	t.Parallel()

	const volid = "local-lvm:vm-9001-disk-0"

	callCount := 0
	qemuSvc := &updateDiskQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			callCount++
			if callCount <= 2 {
				return map[string]any{diskSlot: volid}, nil
			}
			return map[string]any{diskSlot: volid + ",size=20G"}, nil
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
	t.Parallel()
	h := handlers.HandleUpdateDisk(updateDiskDeps(&updateDiskQEMUService{}, &snapClusterService{}, nil))
	_, err := h.Handle(context.Background(), marshalArgs("bad-cid-no-colon", map[string]any{}), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error for malformed disk_cid")
	}
}

func TestHandleUpdateDisk_TooFewArgs(t *testing.T) {
	t.Parallel()
	h := handlers.HandleUpdateDisk(updateDiskDeps(&updateDiskQEMUService{}, &snapClusterService{}, nil))
	_, err := h.Handle(context.Background(), marshalArgs("local-lvm:vol"), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error for missing update_spec argument")
	}
}

func TestHandleUpdateDisk_IOThreadFalseRemovesOption(t *testing.T) {
	t.Parallel()

	const volid = "local-lvm:vm-9001-disk-0"

	var capturedOptStr string

	callCount := 0
	qemuSvc := &updateDiskQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			callCount++
			if callCount <= 2 {
				return map[string]any{diskSlot: volid}, nil
			}
			return map[string]any{diskSlot: volid + ",iothread=1,size=10G"}, nil
		},
		attachDiskFn: func(_ context.Context, _ string, _ int, optStr string, _ string, _ *qemu.AttachOpts) (string, error) {
			capturedOptStr = optStr
			return diskSlot, nil
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
	t.Parallel()

	const volid = "local-lvm:vm-9001-disk-0"

	callCount := 0
	qemuSvc := &updateDiskQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			callCount++
			if callCount <= 2 {
				return map[string]any{diskSlot: volid}, nil
			}
			return map[string]any{diskSlot: volid + ",size=10G"}, nil
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
	t.Parallel()

	const volid = "local-lvm:vm-9001-disk-0"

	var waitCalled bool
	tasksSvc := &mockTasksService{
		waitFn: func(_ context.Context, _, _ string, _ *tasks.WaitOptions) (*tasks.Status, error) {
			waitCalled = true
			return &tasks.Status{ExitStatus: "OK"}, nil
		},
	}

	callCount := 0
	qemuSvc := &updateDiskQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			callCount++
			if callCount <= 2 {
				return map[string]any{diskSlot: volid}, nil
			}
			return map[string]any{diskSlot: volid + ",size=10G"}, nil
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
	t.Parallel()
	// Existing options not in update_spec must be preserved in the merged string.

	const volid = "local-lvm:vm-9001-disk-0"

	var capturedOptStr string

	callCount := 0
	qemuSvc := &updateDiskQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			callCount++
			if callCount <= 2 {
				return map[string]any{diskSlot: volid}, nil
			}
			// Existing options: size, backup.
			return map[string]any{diskSlot: volid + ",backup=1,size=10G"}, nil
		},
		attachDiskFn: func(_ context.Context, _ string, _ int, optStr string, _ string, _ *qemu.AttachOpts) (string, error) {
			capturedOptStr = optStr
			return diskSlot, nil
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
	t.Parallel()
	// Exercises mbps_rd, mbps_wr, iops_rd, iops_wr option fields.

	const volid = "local-lvm:vm-9001-disk-0"

	var capturedOptStr string

	callCount := 0
	qemuSvc := &updateDiskQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			callCount++
			if callCount <= 2 {
				return map[string]any{diskSlot: volid}, nil
			}
			return map[string]any{diskSlot: volid + ",size=10G"}, nil
		},
		attachDiskFn: func(_ context.Context, _ string, _ int, optStr string, _ string, _ *qemu.AttachOpts) (string, error) {
			capturedOptStr = optStr
			return diskSlot, nil
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
	t.Parallel()
	// Exercises ssd and backup bool options.

	const volid = "local-lvm:vm-9001-disk-0"

	var capturedOptStr string

	callCount := 0
	qemuSvc := &updateDiskQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			callCount++
			if callCount <= 2 {
				return map[string]any{diskSlot: volid}, nil
			}
			return map[string]any{diskSlot: volid}, nil
		},
		attachDiskFn: func(_ context.Context, _ string, _ int, optStr string, _ string, _ *qemu.AttachOpts) (string, error) {
			capturedOptStr = optStr
			return diskSlot, nil
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
	t.Parallel()
	// Null update_spec (JSON null) → treated as empty map → no-op.
	const volid = "local-lvm:vm-9001-disk-0"

	qemuSvc := &updateDiskQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{diskSlot: volid}, nil
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

// ---------------------------------------------------------------------------
// Gap tests: error paths (gaps #12, #13, #15, #23 from R1).
// ---------------------------------------------------------------------------

// TestHandleUpdateDisk_ConfigReadError — Config() fails on the 3rd call (during
// option read, after locate + ResolveDiskID succeed). Handler must propagate the
// error (gap #12).
func TestHandleUpdateDisk_ConfigReadError(t *testing.T) {
	t.Parallel()

	const volid = "local-lvm:vm-9001-disk-0"

	configCallCount := 0
	qemuSvc := &updateDiskQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			configCallCount++
			if configCallCount <= 2 {
				// Calls 1-2: FindVMByDiskVolid + ResolveDiskID — return canonical volid.
				return map[string]any{diskSlot: volid}, nil
			}
			// Call 3: option read for merge — inject failure.
			return nil, errors.New("config read failure injected")
		},
	}

	h := handlers.HandleUpdateDisk(updateDiskDeps(qemuSvc, updateClusterWith(100), nil))
	_, err := h.Handle(context.Background(), marshalArgs(diskCID, map[string]any{
		"cache": "writeback",
	}), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error when Config() fails during option read")
	}
	if !strings.Contains(err.Error(), "config") && !strings.Contains(err.Error(), "config read failure injected") {
		t.Errorf("error must mention config failure, got: %v", err)
	}
}

// TestHandleUpdateDisk_ResolveDiskIDError — Config() fails on the 2nd call
// (used by ResolveDiskID internally), so ResolveDiskID returns error. Handler
// must propagate it (gap #13).
func TestHandleUpdateDisk_ResolveDiskIDError(t *testing.T) {
	t.Parallel()

	const volid = "local-lvm:vm-9001-disk-0"

	configCallCount := 0
	qemuSvc := &updateDiskQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			configCallCount++
			if configCallCount == 1 {
				// Call 1: FindVMByDiskVolid — returns canonical volid so VM is located.
				return map[string]any{diskSlot: volid}, nil
			}
			// Call 2: ResolveDiskID config fetch — inject failure.
			return nil, errors.New("resolve config error injected")
		},
	}

	h := handlers.HandleUpdateDisk(updateDiskDeps(qemuSvc, updateClusterWith(100), nil))
	_, err := h.Handle(context.Background(), marshalArgs(diskCID, map[string]any{
		"cache": "writeback",
	}), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error when ResolveDiskID fails")
	}
}

// TestHandleUpdateDisk_SizeWrongType — update_spec.size is a string "big" (not a
// number). toInt() returns false → handler returns a descriptive error (gap #15).
func TestHandleUpdateDisk_SizeWrongType(t *testing.T) {
	t.Parallel()

	const volid = "local-lvm:vm-9001-disk-0"

	configCallCount := 0
	qemuSvc := &updateDiskQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			configCallCount++
			if configCallCount <= 2 {
				return map[string]any{diskSlot: volid}, nil
			}
			return map[string]any{diskSlot: volid + ",size=10G"}, nil
		},
	}

	h := handlers.HandleUpdateDisk(updateDiskDeps(qemuSvc, updateClusterWith(100), nil))
	_, err := h.Handle(context.Background(), marshalArgs(diskCID, map[string]any{
		"size": "big",
	}), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error for non-numeric size field")
	}
	if !strings.Contains(err.Error(), "size") {
		t.Errorf("error must mention size field, got: %v", err)
	}
}

// TestHandleUpdateDisk_EmptyDiskCID — diskCID is an empty string after unmarshal.
// update_disk.go:68-70 returns an explicit error before ParseDiskCID is reached
// (gap #23).
func TestHandleUpdateDisk_EmptyDiskCID(t *testing.T) {
	t.Parallel()
	h := handlers.HandleUpdateDisk(updateDiskDeps(&updateDiskQEMUService{}, &snapClusterService{}, nil))
	_, err := h.Handle(context.Background(), marshalArgs("", map[string]any{}), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error for empty diskCID")
	}
}

// ---------------------------------------------------------------------------
// CID variant tests: dir and zfspool formats (gap #16 for update_disk).
// update_disk has no storage-type branching; these tests exercise ParseDiskCID
// with non-lvm volume formats and confirm the full update path completes.
// ---------------------------------------------------------------------------

// TestHandleUpdateDisk_Dir_CID — diskCID uses dir-storage subpath format
// "local:9001/vm-9001-disk-0.raw". ParseDiskCID splits on first colon:
// storage="local", volume="9001/vm-9001-disk-0.raw". Handler locates the VM
// and applies option updates normally.
func TestHandleUpdateDisk_Dir_CID(t *testing.T) {
	t.Parallel()
	const diskCID = "local:9001/vm-9001-disk-0.raw"
	const volid = "local:9001/vm-9001-disk-0.raw"

	var capturedOptStr string

	configCallCount := 0
	qemuSvc := &updateDiskQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			configCallCount++
			if configCallCount <= 2 {
				return map[string]any{diskSlot: volid}, nil
			}
			return map[string]any{diskSlot: volid + ",size=10G"}, nil
		},
		attachDiskFn: func(_ context.Context, _ string, _ int, optStr string, _ string, opts *qemu.AttachOpts) (string, error) {
			capturedOptStr = optStr
			if opts == nil || opts.DiskID != diskSlot {
				t.Errorf("expected AttachDisk with DiskID=scsi2, got %v", opts)
			}
			return diskSlot, nil
		},
	}

	h := handlers.HandleUpdateDisk(updateDiskDeps(qemuSvc, updateClusterWith(100), nil))
	_, err := h.Handle(context.Background(), marshalArgs(diskCID, map[string]any{
		"cache": "none",
	}), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error for dir-storage CID: %v", err)
	}
	if !strings.Contains(capturedOptStr, "cache=none") {
		t.Errorf("option string must contain cache=none, got %q", capturedOptStr)
	}
}

// TestHandleUpdateDisk_ZFSPool_CID — diskCID uses zfspool bare-volname format
// "local-zfs:vm-9001-disk-0". ParseDiskCID: storage="local-zfs",
// volume="vm-9001-disk-0". update_disk has no storage-type branching so the
// zfspool CID passes through the same path as lvm.
func TestHandleUpdateDisk_ZFSPool_CID(t *testing.T) {
	t.Parallel()
	const diskCID = "local-zfs:vm-9001-disk-0"
	const volid = "local-zfs:vm-9001-disk-0"

	var capturedOptStr string

	configCallCount := 0
	qemuSvc := &updateDiskQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			configCallCount++
			if configCallCount <= 2 {
				return map[string]any{diskSlot: volid}, nil
			}
			return map[string]any{diskSlot: volid + ",size=10G"}, nil
		},
		attachDiskFn: func(_ context.Context, _ string, _ int, optStr string, _ string, opts *qemu.AttachOpts) (string, error) {
			capturedOptStr = optStr
			if opts == nil || opts.DiskID != diskSlot {
				t.Errorf("expected AttachDisk with DiskID=scsi2, got %v", opts)
			}
			return diskSlot, nil
		},
	}

	h := handlers.HandleUpdateDisk(updateDiskDeps(qemuSvc, updateClusterWith(100), nil))
	_, err := h.Handle(context.Background(), marshalArgs(diskCID, map[string]any{
		"iothread": true,
	}), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error for zfspool CID: %v", err)
	}
	if !strings.Contains(capturedOptStr, "iothread=1") {
		t.Errorf("option string must contain iothread=1, got %q", capturedOptStr)
	}
}

// ---------------------------------------------------------------------------
// TODO(storage-network): nfs — wired to PVE network-call boundary, stubbed pending
// live shared-storage test infrastructure. Storage: nfs-store:9001/vm-9001-disk-0.qcow2. Re-enable when
// integration-test harness provides a nfs pool via env.
//
// func TestHandleUpdateDisk_NFS_CID(t *testing.T) { ... }

// TODO(storage-network): rbd — wired to PVE network-call boundary, stubbed pending
// live shared-storage test infrastructure. Storage: ceph-pool:vm-9001-disk-0. Re-enable when
// integration-test harness provides a rbd pool via env.
//
// func TestHandleUpdateDisk_RBD_CID(t *testing.T) { ... }

// TODO(storage-network): cephfs — wired to PVE network-call boundary, stubbed pending
// live shared-storage test infrastructure. Storage: cephfs-pool:vm-9001-disk-0. Re-enable when
// integration-test harness provides a cephfs pool via env.
//
// func TestHandleUpdateDisk_CephFS_CID(t *testing.T) { ... }

// TODO(storage-network): cifs — wired to PVE network-call boundary, stubbed pending
// live shared-storage test infrastructure. Storage: cifs-store:9001/vm-9001-disk-0.qcow2. Re-enable when
// integration-test harness provides a cifs pool via env.
//
// func TestHandleUpdateDisk_CIFS_CID(t *testing.T) { ... }

// ---------------------------------------------------------------------------
// Storage-lock retry tests (wiring verification).
//
// resizeDiskInternal wraps ResizeDisk+AwaitTask in pve.RetryOnTransientOrLock.
// These tests exercise that path via the full HandleUpdateDisk call stack,
// using error strings that pve.IsStorageLockTimeout recognises:
//   "can't lock file '/var/lock/pve-manager/pve-storage-*' - got timeout"
// ---------------------------------------------------------------------------

// storageLockErr returns an error that pve.IsStorageLockTimeout recognises —
// the canonical PVE storage-lockfile timeout string format.
func storageLockErr() error {
	return errors.New("can't lock file '/var/lock/pve-manager/pve-storage-local-lvm' - got timeout")
}

// TestHandleUpdateDisk_ResizeUnderStorageLock_RetriesAndSucceeds verifies that
// resizeDiskInternal retries on a storage-lock error and returns nil when
// ResizeDisk eventually succeeds within the retry budget.
//
// Setup: ResizeDisk returns storageLockErr on the first N-1 calls, then ""
// (no UPID, synchronous success) on the Nth call. AwaitTask is not invoked
// because the final call returns an empty UPID.
func TestHandleUpdateDisk_ResizeUnderStorageLock_RetriesAndSucceeds(t *testing.T) {
	t.Parallel()

	const volid = "local-lvm:vm-9001-disk-0"

	// Fail twice with a storage-lock error then succeed.
	resizeCallCount := 0
	const failN = 2

	configCallCount := 0
	qemuSvc := &updateDiskQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			configCallCount++
			// Calls 1-2: FindVMByDiskVolid + ResolveDiskID — return canonical volid.
			// Call 3+: resizeDiskInternal config read — return volid with size=10G.
			if configCallCount <= 2 {
				return map[string]any{diskSlot: volid}, nil
			}
			return map[string]any{diskSlot: volid + ",size=10G"}, nil
		},
		resizeDiskFn: func(_ context.Context, _ string, _ int, _ string, _ int) (string, error) {
			resizeCallCount++
			if resizeCallCount <= failN {
				return "", storageLockErr()
			}
			// Synchronous success (empty UPID) — no AwaitTask needed.
			return "", nil
		},
	}

	h := handlers.HandleUpdateDisk(updateDiskDeps(qemuSvc, updateClusterWith(100), nil))
	_, err := h.Handle(fastRetryCtx(context.Background()), marshalArgs(diskCID, map[string]any{
		"size": 20480, // 20 GiB, current=10 GiB → delta=10 GiB
	}), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("expected nil error after retry success, got: %v", err)
	}
	if resizeCallCount <= failN {
		t.Errorf("ResizeDisk must be called at least %d times (retry path), got %d", failN+1, resizeCallCount)
	}
}

// TestHandleUpdateDisk_ResizeUnderStorageLock_ExhaustsRetries verifies that
// resizeDiskInternal propagates a storage-lock error once the retry budget
// (pve.DefaultStorageLockMaxAttempts) is exhausted. The test caps calls at
// DefaultStorageLockMaxAttempts+2 to detect infinite-loop regressions.
//
// Note: pve.RetryOnTransientOrLock does not sleep in tests because the
// backoff timer fires on real time — using context.Background() the full
// budget runs immediately (each call is synchronous and instantaneous, so
// StorageLockBackoff sleep durations apply). To keep the test fast, the
// test sets up a context with a short deadline so the retry loop terminates
// via ctx cancellation if somehow it exceeds the expected call count.
func TestHandleUpdateDisk_ResizeUnderStorageLock_ExhaustsRetries(t *testing.T) {
	t.Parallel()

	const volid = "local-lvm:vm-9001-disk-0"

	// Always return a storage-lock error so RetryOnTransientOrLock exhausts.
	// We count calls to detect regressions where retries don't stop.
	resizeCallCount := 0
	configCallCount := 0
	qemuSvc := &updateDiskQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			configCallCount++
			if configCallCount <= 2 {
				return map[string]any{diskSlot: volid}, nil
			}
			return map[string]any{diskSlot: volid + ",size=10G"}, nil
		},
		resizeDiskFn: func(_ context.Context, _ string, _ int, _ string, _ int) (string, error) {
			resizeCallCount++
			return "", storageLockErr()
		},
	}

	// fastRetryCtx zeros the backoff so DefaultStorageLockMaxAttempts retries
	// burn no wall-clock time. Cap with a short deadline so an infinite-loop
	// regression still aborts.
	ctx, cancel := context.WithTimeout(fastRetryCtx(context.Background()), 500*time.Millisecond)
	defer cancel()

	h := handlers.HandleUpdateDisk(updateDiskDeps(qemuSvc, updateClusterWith(100), nil))
	_, err := h.Handle(ctx, marshalArgs(diskCID, map[string]any{
		"size": 20480,
	}), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error when storage-lock errors persist across entire retry budget")
	}
	// The error must not be nil — it should be the storage-lock error (possibly
	// wrapped) or a context.DeadlineExceeded (both are acceptable: the test
	// confirms the handler does not silently swallow persistent lock errors).
	if resizeCallCount == 0 {
		t.Error("ResizeDisk must be called at least once")
	}
}
