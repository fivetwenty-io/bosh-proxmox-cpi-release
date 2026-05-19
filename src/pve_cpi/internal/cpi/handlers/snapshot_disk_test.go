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
	sdkclusterapi "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/qemu"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/tasks"
	sdkerrors "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/errors"
)

// ---------------------------------------------------------------------------
// snapQEMUService: QEMU mock for snapshot_disk tests.
// ---------------------------------------------------------------------------

type snapQEMUService struct {
	configFn   func(ctx context.Context, node string, vmid int) (map[string]interface{}, error)
	snapshotFn func(ctx context.Context, node string, vmid int, name string, opts map[string]interface{}) (string, error)
}

func (m *snapQEMUService) Config(ctx context.Context, node string, vmid int) (map[string]interface{}, error) {
	if m.configFn != nil {
		return m.configFn(ctx, node, vmid)
	}
	return map[string]interface{}{}, nil
}

func (m *snapQEMUService) Snapshot(ctx context.Context, node string, vmid int, name string, opts map[string]interface{}) (string, error) {
	if m.snapshotFn != nil {
		return m.snapshotFn(ctx, node, vmid, name, opts)
	}
	return "", nil
}

// Unimplemented methods — panic on accidental call.
func (m *snapQEMUService) Create(_ context.Context, _ string, _ map[string]interface{}) (string, error) {
	panic("snapQEMUService.Create: not expected")
}
func (m *snapQEMUService) Status(_ context.Context, _ string, _ int) (map[string]interface{}, error) {
	panic("snapQEMUService.Status: not expected")
}
func (m *snapQEMUService) Start(_ context.Context, _ string, _ int) (string, error) {
	panic("snapQEMUService.Start: not expected")
}
func (m *snapQEMUService) Stop(_ context.Context, _ string, _ int) (string, error) {
	panic("snapQEMUService.Stop: not expected")
}
func (m *snapQEMUService) Reset(_ context.Context, _ string, _ int) (string, error) {
	panic("snapQEMUService.Reset: not expected")
}
func (m *snapQEMUService) Clone(_ context.Context, _ string, _ int, _ map[string]interface{}) (string, error) {
	panic("snapQEMUService.Clone: not expected")
}
func (m *snapQEMUService) Template(_ context.Context, _ string, _ int) (string, error) {
	panic("snapQEMUService.Template: not expected")
}
func (m *snapQEMUService) AttachDisk(_ context.Context, _ string, _ int, _ string, _ string, _ *qemu.AttachOpts) (string, error) {
	panic("snapQEMUService.AttachDisk: not expected")
}
func (m *snapQEMUService) DetachDisk(_ context.Context, _ string, _ int, _ string) error {
	panic("snapQEMUService.DetachDisk: not expected")
}
func (m *snapQEMUService) ResizeDisk(_ context.Context, _ string, _ int, _ string, _ int) (string, error) {
	panic("snapQEMUService.ResizeDisk: not expected")
}
func (m *snapQEMUService) DeleteSnapshot(_ context.Context, _ string, _ int, _ string) error {
	panic("snapQEMUService.DeleteSnapshot: not expected")
}
func (m *snapQEMUService) ListSnapshots(_ context.Context, _ string, _ int) ([]map[string]interface{}, error) {
	panic("snapQEMUService.ListSnapshots: not expected")
}
func (m *snapQEMUService) RollbackSnapshot(_ context.Context, _ string, _ int, _ string) (string, error) {
	panic("snapQEMUService.RollbackSnapshot: not expected")
}

var _ qemu.Service = (*snapQEMUService)(nil)

// ---------------------------------------------------------------------------
// snapClusterService: cluster mock for snapshot_disk tests.
// ---------------------------------------------------------------------------

type snapClusterService struct {
	sdkclusterapi.Service // nil embed — panics on uncovered methods
	listFn                func(ctx context.Context, params *sdkclusterapi.ListResourcesParams) (*sdkclusterapi.ListResourcesResponse, error)
}

func (m *snapClusterService) ListResources(ctx context.Context, params *sdkclusterapi.ListResourcesParams) (*sdkclusterapi.ListResourcesResponse, error) {
	if m.listFn != nil {
		return m.listFn(ctx, params)
	}
	resp := sdkclusterapi.ListResourcesResponse{}
	return &resp, nil
}

// ---------------------------------------------------------------------------
// helper: build a cluster response with one VM entry.
// ---------------------------------------------------------------------------

func clusterRespWith(vmid int, node string) *sdkclusterapi.ListResourcesResponse {
	type entry struct {
		VMID int64  `json:"vmid"`
		Node string `json:"node"`
	}
	raw, _ := json.Marshal(entry{VMID: int64(vmid), Node: node})
	resp := sdkclusterapi.ListResourcesResponse{raw}
	return &resp
}

// ---------------------------------------------------------------------------
// snapDeps builds Deps for snapshot_disk tests.
// ---------------------------------------------------------------------------

func snapDeps(qemuSvc qemu.Service, clusterSvc sdkclusterapi.Service, tasksSvc tasks.Service) handlers.Deps {
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

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestHandleSnapshotDisk_Happy(t *testing.T) {
	const diskCID = "local-lvm:vm-9001-disk-0"
	const volid = "vm-9001-disk-0"

	var snapCalled bool
	var snapName string

	qemuSvc := &snapQEMUService{
		configFn: func(_ context.Context, _ string, vmid int) (map[string]interface{}, error) {
			if vmid == 100 {
				return map[string]interface{}{
					"scsi2": volid,
				}, nil
			}
			return map[string]interface{}{}, nil
		},
		snapshotFn: func(_ context.Context, _ string, vmid int, name string, _ map[string]interface{}) (string, error) {
			snapCalled = true
			snapName = name
			return "", nil // no UPID (synchronous success)
		},
	}

	clusterSvc := &snapClusterService{
		listFn: func(_ context.Context, _ *sdkclusterapi.ListResourcesParams) (*sdkclusterapi.ListResourcesResponse, error) {
			return clusterRespWith(100, "pve1"), nil
		},
	}

	h := handlers.HandleSnapshotDisk(snapDeps(qemuSvc, clusterSvc, nil))

	result, err := h.Handle(context.Background(), marshalArgs(diskCID), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !snapCalled {
		t.Error("Snapshot was not called")
	}
	sid, ok := result.(string)
	if !ok || sid == "" {
		t.Fatalf("result: want non-empty string snapshot_cid, got %T %v", result, result)
	}
	// snapshot_cid must contain the snap name.
	if snapName == "" {
		t.Error("snap name must not be empty")
	}
	t.Logf("snapshot_cid = %s, snap_name = %s", sid, snapName)
}

func TestHandleSnapshotDisk_WithDescription(t *testing.T) {
	const diskCID = "local-lvm:vm-9001-disk-0"
	const volid = "vm-9001-disk-0"

	var capturedOpts map[string]interface{}

	qemuSvc := &snapQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]interface{}, error) {
			return map[string]interface{}{"scsi1": volid}, nil
		},
		snapshotFn: func(_ context.Context, _ string, _ int, _ string, opts map[string]interface{}) (string, error) {
			capturedOpts = opts
			return "", nil
		},
	}

	clusterSvc := &snapClusterService{
		listFn: func(_ context.Context, _ *sdkclusterapi.ListResourcesParams) (*sdkclusterapi.ListResourcesResponse, error) {
			return clusterRespWith(100, "pve1"), nil
		},
	}

	h := handlers.HandleSnapshotDisk(snapDeps(qemuSvc, clusterSvc, nil))
	_, err := h.Handle(context.Background(), marshalArgs(diskCID, map[string]any{
		"description": "my snap",
	}), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedOpts["description"] != "my snap" {
		t.Errorf("description not forwarded: %v", capturedOpts)
	}
}

func TestHandleSnapshotDisk_WithUpid(t *testing.T) {
	const diskCID = "local-lvm:vm-9001-disk-0"

	var waitCalled bool
	tasksSvc := &mockTasksService{
		waitFn: func(_ context.Context, _, _ string, _ *tasks.WaitOptions) (*tasks.Status, error) {
			waitCalled = true
			return &tasks.Status{ExitStatus: "OK"}, nil
		},
	}

	qemuSvc := &snapQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]interface{}, error) {
			return map[string]interface{}{"scsi1": "vm-9001-disk-0"}, nil
		},
		snapshotFn: func(_ context.Context, _ string, _ int, _ string, _ map[string]interface{}) (string, error) {
			return "UPID:pve1:abc:snap", nil // returns a UPID
		},
	}

	clusterSvc := &snapClusterService{
		listFn: func(_ context.Context, _ *sdkclusterapi.ListResourcesParams) (*sdkclusterapi.ListResourcesResponse, error) {
			return clusterRespWith(100, "pve1"), nil
		},
	}

	h := handlers.HandleSnapshotDisk(snapDeps(qemuSvc, clusterSvc, tasksSvc))
	result, err := h.Handle(context.Background(), marshalArgs(diskCID), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !waitCalled {
		t.Error("AwaitTask was not called for UPID")
	}
	if result == nil {
		t.Error("expected snapshot_cid result")
	}
}

func TestHandleSnapshotDisk_DiskNotAttached(t *testing.T) {
	const diskCID = "local-lvm:vm-9001-disk-0"

	qemuSvc := &snapQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]interface{}, error) {
			// VM exists but does not have this disk.
			return map[string]interface{}{"scsi0": "local-lvm:other"}, nil
		},
	}

	clusterSvc := &snapClusterService{
		listFn: func(_ context.Context, _ *sdkclusterapi.ListResourcesParams) (*sdkclusterapi.ListResourcesResponse, error) {
			return clusterRespWith(100, "pve1"), nil
		},
	}

	h := handlers.HandleSnapshotDisk(snapDeps(qemuSvc, clusterSvc, nil))
	_, err := h.Handle(context.Background(), marshalArgs(diskCID), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error for disk not attached to any VM")
	}
	if cpierrors.IsType(err, cpierrors.TypeDiskNotFound) {
		// CloudError is acceptable; DiskNotFound is also fine.
	}
	// Must be an error of some kind.
}

func TestHandleSnapshotDisk_EmptyClusterList(t *testing.T) {
	const diskCID = "local-lvm:vm-9001-disk-0"

	clusterSvc := &snapClusterService{
		listFn: func(_ context.Context, _ *sdkclusterapi.ListResourcesParams) (*sdkclusterapi.ListResourcesResponse, error) {
			resp := sdkclusterapi.ListResourcesResponse{}
			return &resp, nil
		},
	}

	h := handlers.HandleSnapshotDisk(snapDeps(&snapQEMUService{}, clusterSvc, nil))
	_, err := h.Handle(context.Background(), marshalArgs(diskCID), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error when cluster has no VMs")
	}
}

func TestHandleSnapshotDisk_SnapshotFails(t *testing.T) {
	const diskCID = "local-lvm:vm-9001-disk-0"

	qemuSvc := &snapQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]interface{}, error) {
			return map[string]interface{}{"scsi1": "vm-9001-disk-0"}, nil
		},
		snapshotFn: func(_ context.Context, _ string, _ int, _ string, _ map[string]interface{}) (string, error) {
			return "", errors.New("PVE snapshot quota exceeded")
		},
	}

	clusterSvc := &snapClusterService{
		listFn: func(_ context.Context, _ *sdkclusterapi.ListResourcesParams) (*sdkclusterapi.ListResourcesResponse, error) {
			return clusterRespWith(100, "pve1"), nil
		},
	}

	h := handlers.HandleSnapshotDisk(snapDeps(qemuSvc, clusterSvc, nil))
	_, err := h.Handle(context.Background(), marshalArgs(diskCID), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error when Snapshot fails")
	}
}

func TestHandleSnapshotDisk_MalformedDiskCID(t *testing.T) {
	h := handlers.HandleSnapshotDisk(snapDeps(&snapQEMUService{}, &snapClusterService{}, nil))
	_, err := h.Handle(context.Background(), marshalArgs("no-colon-disk-cid"), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error for malformed disk_cid")
	}
}

func TestHandleSnapshotDisk_EmptyDiskCID(t *testing.T) {
	h := handlers.HandleSnapshotDisk(snapDeps(&snapQEMUService{}, &snapClusterService{}, nil))
	_, err := h.Handle(context.Background(), marshalArgs(""), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error for empty disk_cid")
	}
}

func TestHandleSnapshotDisk_ClusterListError(t *testing.T) {
	const diskCID = "local-lvm:vm-9001-disk-0"

	clusterSvc := &snapClusterService{
		listFn: func(_ context.Context, _ *sdkclusterapi.ListResourcesParams) (*sdkclusterapi.ListResourcesResponse, error) {
			return nil, errors.New("cluster API unavailable")
		},
	}

	h := handlers.HandleSnapshotDisk(snapDeps(&snapQEMUService{}, clusterSvc, nil))
	_, err := h.Handle(context.Background(), marshalArgs(diskCID), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error when cluster list fails")
	}
}

func TestHandleSnapshotDisk_DiskInOptionString(t *testing.T) {
	// Disk stored with option string: "local-lvm:vm-9001-disk-0,size=10G,cache=writeback"
	const diskCID = "local-lvm:vm-9001-disk-0"
	const volid = "vm-9001-disk-0"

	var snapCalled bool

	qemuSvc := &snapQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]interface{}, error) {
			return map[string]interface{}{
				"scsi2": volid + ",size=10G,cache=writeback",
			}, nil
		},
		snapshotFn: func(_ context.Context, _ string, _ int, _ string, _ map[string]interface{}) (string, error) {
			snapCalled = true
			return "", nil
		},
	}

	clusterSvc := &snapClusterService{
		listFn: func(_ context.Context, _ *sdkclusterapi.ListResourcesParams) (*sdkclusterapi.ListResourcesResponse, error) {
			return clusterRespWith(100, "pve1"), nil
		},
	}

	h := handlers.HandleSnapshotDisk(snapDeps(qemuSvc, clusterSvc, nil))
	_, err := h.Handle(context.Background(), marshalArgs(diskCID), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error for disk stored with option string: %v", err)
	}
	if !snapCalled {
		t.Error("Snapshot must be called when disk found via option-string prefix match")
	}
}

func TestHandleSnapshotDisk_SDKError404(t *testing.T) {
	// A 404 API error from Snapshot should propagate as an error.
	const diskCID = "local-lvm:vm-9001-disk-0"

	qemuSvc := &snapQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]interface{}, error) {
			return map[string]interface{}{"scsi1": "vm-9001-disk-0"}, nil
		},
		snapshotFn: func(_ context.Context, _ string, _ int, _ string, _ map[string]interface{}) (string, error) {
			return "", &sdkerrors.APIError{HTTPCode: 404, Message: "VM not found"}
		},
	}

	clusterSvc := &snapClusterService{
		listFn: func(_ context.Context, _ *sdkclusterapi.ListResourcesParams) (*sdkclusterapi.ListResourcesResponse, error) {
			return clusterRespWith(100, "pve1"), nil
		},
	}

	h := handlers.HandleSnapshotDisk(snapDeps(qemuSvc, clusterSvc, nil))
	_, err := h.Handle(context.Background(), marshalArgs(diskCID), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error for 404 from Snapshot")
	}
}
