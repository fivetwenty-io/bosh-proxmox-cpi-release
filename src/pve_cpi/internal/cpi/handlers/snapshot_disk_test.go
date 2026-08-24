package handlers_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	sdkclusterapi "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/tasks"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

// ---------------------------------------------------------------------------
// snapQEMUService: QEMU mock for snapshot_disk tests.
// ---------------------------------------------------------------------------

type snapQEMUService struct {
	configFn   func(ctx context.Context, node string, vmid int) (map[string]any, error)
	snapshotFn func(ctx context.Context, node string, vmid int, name string, opts map[string]any) (string, error)
	// listSnapshotsFn, when non-nil, is called by ListSnapshots. The handler
	// lists snapshots only when a replayed attempt fails (checking whether a
	// prior attempt committed), so the nil default keeps the panic guard.
	listSnapshotsFn func(ctx context.Context, node string, vmid int) ([]map[string]any, error)
}

func (m *snapQEMUService) Config(ctx context.Context, node string, vmid int) (map[string]any, error) {
	if m.configFn != nil {
		return m.configFn(ctx, node, vmid)
	}
	return map[string]any{}, nil
}

func (m *snapQEMUService) Snapshot(ctx context.Context, node string, vmid int, name string, opts map[string]any) (string, error) {
	if m.snapshotFn != nil {
		return m.snapshotFn(ctx, node, vmid, name, opts)
	}
	return "", nil
}

// Unimplemented methods — panic on accidental call.
func (m *snapQEMUService) Create(_ context.Context, _ string, _ map[string]any) (string, error) {
	panic("snapQEMUService.Create: not expected")
}
func (m *snapQEMUService) Status(_ context.Context, _ string, _ int) (map[string]any, error) {
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
func (m *snapQEMUService) Clone(_ context.Context, _ string, _ int, _ map[string]any) (string, error) {
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
func (m *snapQEMUService) ListSnapshots(ctx context.Context, node string, vmid int) ([]map[string]any, error) {
	if m.listSnapshotsFn != nil {
		return m.listSnapshotsFn(ctx, node, vmid)
	}
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

// ListConfigNodes derives corosync membership from the same ListResources
// fixture (falling back to testNode), so pve.ListGuestsAuthoritative sees
// the cluster the suite scripts through the index fixture.
func (m *snapClusterService) ListConfigNodes(ctx context.Context) (*sdkclusterapi.ListConfigNodesResponse, error) {
	rows, err := m.ListResources(ctx, nil)
	if err != nil {
		return nil, err
	}
	return authConfigNodesFromResources(rows, testNode), nil
}

// ---------------------------------------------------------------------------
// helper: build a cluster response with one VM entry.
// ---------------------------------------------------------------------------

//nolint:unparam // node kept for call-site clarity; always testNode in this suite
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

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestHandleSnapshotDisk_Happy(t *testing.T) {
	t.Parallel()

	const volid = "local-lvm:vm-9001-disk-0"

	type snapCall struct{ name string }
	var snapCalls []snapCall

	qemuSvc := &snapQEMUService{
		configFn: func(_ context.Context, _ string, vmid int) (map[string]any, error) {
			if vmid == 100 {
				return map[string]any{
					diskSlot: volid,
				}, nil
			}
			return map[string]any{}, nil
		},
		snapshotFn: func(_ context.Context, _ string, vmid int, name string, _ map[string]any) (string, error) {
			snapCalls = append(snapCalls, snapCall{name})
			return "", nil // no UPID (synchronous success)
		},
	}

	clusterSvc := &snapClusterService{
		listFn: func(_ context.Context, _ *sdkclusterapi.ListResourcesParams) (*sdkclusterapi.ListResourcesResponse, error) {
			return clusterRespWith(100, testNode), nil
		},
	}

	h := handlers.HandleSnapshotDisk(snapDeps(qemuSvc, clusterSvc, nil))

	result, err := h.Handle(context.Background(), marshalArgs(mustEncodeDiskCID(t, diskCID, nil)), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(snapCalls) != 1 {
		t.Fatalf("Snapshot: want 1 call, got %d", len(snapCalls))
	}
	sid, ok := result.(string)
	if !ok || sid == "" {
		t.Fatalf("result: want non-empty string snapshot_cid, got %T %v", result, result)
	}
	// snapshot_cid must contain the snap name.
	if snapCalls[0].name == "" {
		t.Error("snap name must not be empty")
	}
	t.Logf("snapshot_cid = %s, snap_name = %s", sid, snapCalls[0].name)
}

func TestHandleSnapshotDisk_WithDescription(t *testing.T) {
	t.Parallel()

	const volid = "local-lvm:vm-9001-disk-0"

	var capturedOpts map[string]any

	qemuSvc := &snapQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{"scsi1": volid}, nil
		},
		snapshotFn: func(_ context.Context, _ string, _ int, _ string, opts map[string]any) (string, error) {
			capturedOpts = opts
			return "", nil
		},
	}

	clusterSvc := &snapClusterService{
		listFn: func(_ context.Context, _ *sdkclusterapi.ListResourcesParams) (*sdkclusterapi.ListResourcesResponse, error) {
			return clusterRespWith(100, testNode), nil
		},
	}

	h := handlers.HandleSnapshotDisk(snapDeps(qemuSvc, clusterSvc, nil))
	_, err := h.Handle(context.Background(), marshalArgs(mustEncodeDiskCID(t, diskCID, nil), map[string]any{
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
	t.Parallel()

	var taskWaitCalls []struct{}
	tasksSvc := &mockTasksService{
		waitFn: func(_ context.Context, _, _ string, _ *tasks.WaitOptions) (*tasks.Status, error) {
			taskWaitCalls = append(taskWaitCalls, struct{}{})
			return &tasks.Status{ExitStatus: "OK"}, nil
		},
	}

	qemuSvc := &snapQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{"scsi1": "local-lvm:vm-9001-disk-0"}, nil
		},
		snapshotFn: func(_ context.Context, _ string, _ int, _ string, _ map[string]any) (string, error) {
			return "UPID:pve1:abc:snap", nil // returns a UPID
		},
	}

	clusterSvc := &snapClusterService{
		listFn: func(_ context.Context, _ *sdkclusterapi.ListResourcesParams) (*sdkclusterapi.ListResourcesResponse, error) {
			return clusterRespWith(100, testNode), nil
		},
	}

	h := handlers.HandleSnapshotDisk(snapDeps(qemuSvc, clusterSvc, tasksSvc))
	result, err := h.Handle(context.Background(), marshalArgs(mustEncodeDiskCID(t, diskCID, nil)), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(taskWaitCalls) == 0 {
		t.Error("AwaitTask was not called for UPID")
	}
	if result == nil {
		t.Error("expected snapshot_cid result")
	}
}

func TestHandleSnapshotDisk_DiskNotAttached(t *testing.T) {
	t.Parallel()

	qemuSvc := &snapQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			// VM exists but does not have this disk.
			return map[string]any{"scsi0": "local-lvm:other"}, nil
		},
	}

	clusterSvc := &snapClusterService{
		listFn: func(_ context.Context, _ *sdkclusterapi.ListResourcesParams) (*sdkclusterapi.ListResourcesResponse, error) {
			return clusterRespWith(100, testNode), nil
		},
	}

	h := handlers.HandleSnapshotDisk(snapDeps(qemuSvc, clusterSvc, nil))
	_, err := h.Handle(context.Background(), marshalArgs(mustEncodeDiskCID(t, diskCID, nil)), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error for disk not attached to any VM")
	}
	// Any error type (DiskNotFound or CloudError) is acceptable; err != nil already verified above.
}

func TestHandleSnapshotDisk_EmptyClusterList(t *testing.T) {
	t.Parallel()

	clusterSvc := &snapClusterService{
		listFn: func(_ context.Context, _ *sdkclusterapi.ListResourcesParams) (*sdkclusterapi.ListResourcesResponse, error) {
			resp := sdkclusterapi.ListResourcesResponse{}
			return &resp, nil
		},
	}

	h := handlers.HandleSnapshotDisk(snapDeps(&snapQEMUService{}, clusterSvc, nil))
	_, err := h.Handle(context.Background(), marshalArgs(mustEncodeDiskCID(t, diskCID, nil)), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error when cluster has no VMs")
	}
}

func TestHandleSnapshotDisk_SnapshotFails(t *testing.T) {
	t.Parallel()

	qemuSvc := &snapQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{"scsi1": "local-lvm:vm-9001-disk-0"}, nil
		},
		snapshotFn: func(_ context.Context, _ string, _ int, _ string, _ map[string]any) (string, error) {
			return "", errors.New("PVE snapshot quota exceeded")
		},
	}

	clusterSvc := &snapClusterService{
		listFn: func(_ context.Context, _ *sdkclusterapi.ListResourcesParams) (*sdkclusterapi.ListResourcesResponse, error) {
			return clusterRespWith(100, testNode), nil
		},
	}

	h := handlers.HandleSnapshotDisk(snapDeps(qemuSvc, clusterSvc, nil))
	_, err := h.Handle(context.Background(), marshalArgs(mustEncodeDiskCID(t, diskCID, nil)), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error when Snapshot fails")
	}
}

func TestHandleSnapshotDisk_MalformedDiskCID(t *testing.T) {
	t.Parallel()
	h := handlers.HandleSnapshotDisk(snapDeps(&snapQEMUService{}, &snapClusterService{}, nil))
	_, err := h.Handle(context.Background(), marshalArgs("no-colon-disk-cid"), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error for malformed disk_cid")
	}
}

func TestHandleSnapshotDisk_EmptyDiskCID(t *testing.T) {
	t.Parallel()
	h := handlers.HandleSnapshotDisk(snapDeps(&snapQEMUService{}, &snapClusterService{}, nil))
	_, err := h.Handle(context.Background(), marshalArgs(""), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error for empty disk_cid")
	}
}

func TestHandleSnapshotDisk_ClusterListError(t *testing.T) {
	t.Parallel()

	clusterSvc := &snapClusterService{
		listFn: func(_ context.Context, _ *sdkclusterapi.ListResourcesParams) (*sdkclusterapi.ListResourcesResponse, error) {
			return nil, errors.New("cluster API unavailable")
		},
	}

	h := handlers.HandleSnapshotDisk(snapDeps(&snapQEMUService{}, clusterSvc, nil))
	_, err := h.Handle(context.Background(), marshalArgs(mustEncodeDiskCID(t, diskCID, nil)), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error when cluster list fails")
	}
}

func TestHandleSnapshotDisk_DiskInOptionString(t *testing.T) {
	t.Parallel()
	// Disk stored with option string: "local-lvm:vm-9001-disk-0,size=10G,cache=writeback"

	const volid = "local-lvm:vm-9001-disk-0"

	var snapCalls []struct{}

	qemuSvc := &snapQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{
				diskSlot: volid + ",size=10G,cache=writeback",
			}, nil
		},
		snapshotFn: func(_ context.Context, _ string, _ int, _ string, _ map[string]any) (string, error) {
			snapCalls = append(snapCalls, struct{}{})
			return "", nil
		},
	}

	clusterSvc := &snapClusterService{
		listFn: func(_ context.Context, _ *sdkclusterapi.ListResourcesParams) (*sdkclusterapi.ListResourcesResponse, error) {
			return clusterRespWith(100, testNode), nil
		},
	}

	h := handlers.HandleSnapshotDisk(snapDeps(qemuSvc, clusterSvc, nil))
	_, err := h.Handle(context.Background(), marshalArgs(mustEncodeDiskCID(t, diskCID, nil)), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error for disk stored with option string: %v", err)
	}
	if len(snapCalls) == 0 {
		t.Error("Snapshot must be called when disk found via option-string prefix match")
	}
}

func TestHandleSnapshotDisk_SDKError404(t *testing.T) {
	t.Parallel()
	// A 404 API error from Snapshot should propagate as an error.

	qemuSvc := &snapQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{"scsi1": "local-lvm:vm-9001-disk-0"}, nil
		},
		snapshotFn: func(_ context.Context, _ string, _ int, _ string, _ map[string]any) (string, error) {
			return "", &sdkerrors.APIError{HTTPCode: 404, Message: "VM not found"}
		},
	}

	clusterSvc := &snapClusterService{
		listFn: func(_ context.Context, _ *sdkclusterapi.ListResourcesParams) (*sdkclusterapi.ListResourcesResponse, error) {
			return clusterRespWith(100, testNode), nil
		},
	}

	h := handlers.HandleSnapshotDisk(snapDeps(qemuSvc, clusterSvc, nil))
	_, err := h.Handle(context.Background(), marshalArgs(mustEncodeDiskCID(t, diskCID, nil)), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error for 404 from Snapshot")
	}
}

// ---------------------------------------------------------------------------
// CID-variant tests: dir, zfspool, lvmthin.
//
// snapshot_disk has no storage-type branching. These tests verify that
// ParseDiskCID accepts each CID format and that the handler locates the VM
// and calls Snapshot regardless of storage type. The CID is passed through
// unchanged — identity, not transformation, is what is tested.
// ---------------------------------------------------------------------------

func TestHandleSnapshotDisk_Dir_CID(t *testing.T) {
	t.Parallel()
	// dir storage: CID has subpath form "<storage>:<vmid>/<volname>.<ext>".
	const diskCID = "local:9001/vm-9001-disk-0.raw"

	var snapCalls []struct{}
	qemuSvc := &snapQEMUService{
		configFn: func(_ context.Context, _ string, vmid int) (map[string]any, error) {
			if vmid == 9001 {
				return map[string]any{"scsi1": diskCID}, nil
			}
			return map[string]any{}, nil
		},
		snapshotFn: func(_ context.Context, _ string, _ int, _ string, _ map[string]any) (string, error) {
			snapCalls = append(snapCalls, struct{}{})
			return "", nil
		},
	}
	clusterSvc := &snapClusterService{
		listFn: func(_ context.Context, _ *sdkclusterapi.ListResourcesParams) (*sdkclusterapi.ListResourcesResponse, error) {
			return clusterRespWith(9001, testNode), nil
		},
	}

	h := handlers.HandleSnapshotDisk(snapDeps(qemuSvc, clusterSvc, nil))
	result, err := h.Handle(context.Background(), marshalArgs(mustEncodeDiskCID(t, diskCID, nil)), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("Dir CID: unexpected error: %v", err)
	}
	if len(snapCalls) == 0 {
		t.Error("Dir CID: Snapshot was not called")
	}
	sid, ok := result.(string)
	if !ok || sid == "" {
		t.Fatalf("Dir CID: result: want non-empty snapshot_cid string, got %T %v", result, result)
	}
}

func TestHandleSnapshotDisk_ZFSPool_CID(t *testing.T) {
	t.Parallel()
	// zfspool storage: bare volname (no subpath), e.g. "local-zfs:vm-9001-disk-0".
	const diskCID = "local-zfs:vm-9001-disk-0"

	var snapCalls []struct{}
	qemuSvc := &snapQEMUService{
		configFn: func(_ context.Context, _ string, vmid int) (map[string]any, error) {
			if vmid == 9001 {
				return map[string]any{"scsi1": diskCID}, nil
			}
			return map[string]any{}, nil
		},
		snapshotFn: func(_ context.Context, _ string, _ int, _ string, _ map[string]any) (string, error) {
			snapCalls = append(snapCalls, struct{}{})
			return "", nil
		},
	}
	clusterSvc := &snapClusterService{
		listFn: func(_ context.Context, _ *sdkclusterapi.ListResourcesParams) (*sdkclusterapi.ListResourcesResponse, error) {
			return clusterRespWith(9001, testNode), nil
		},
	}

	h := handlers.HandleSnapshotDisk(snapDeps(qemuSvc, clusterSvc, nil))
	result, err := h.Handle(context.Background(), marshalArgs(mustEncodeDiskCID(t, diskCID, nil)), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("ZFSPool CID: unexpected error: %v", err)
	}
	if len(snapCalls) == 0 {
		t.Error("ZFSPool CID: Snapshot was not called")
	}
	sid, ok := result.(string)
	if !ok || sid == "" {
		t.Fatalf("ZFSPool CID: result: want non-empty snapshot_cid string, got %T %v", result, result)
	}
}

func TestHandleSnapshotDisk_LVMThin_CID(t *testing.T) {
	t.Parallel()
	// lvmthin storage: bare volname, e.g. "local-lvm-thin:vm-9001-disk-0".
	const diskCID = "local-lvm-thin:vm-9001-disk-0"

	var snapCalls []struct{}
	qemuSvc := &snapQEMUService{
		configFn: func(_ context.Context, _ string, vmid int) (map[string]any, error) {
			if vmid == 9001 {
				return map[string]any{"scsi1": diskCID}, nil
			}
			return map[string]any{}, nil
		},
		snapshotFn: func(_ context.Context, _ string, _ int, _ string, _ map[string]any) (string, error) {
			snapCalls = append(snapCalls, struct{}{})
			return "", nil
		},
	}
	clusterSvc := &snapClusterService{
		listFn: func(_ context.Context, _ *sdkclusterapi.ListResourcesParams) (*sdkclusterapi.ListResourcesResponse, error) {
			return clusterRespWith(9001, testNode), nil
		},
	}

	h := handlers.HandleSnapshotDisk(snapDeps(qemuSvc, clusterSvc, nil))
	result, err := h.Handle(context.Background(), marshalArgs(mustEncodeDiskCID(t, diskCID, nil)), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("LVMThin CID: unexpected error: %v", err)
	}
	if len(snapCalls) == 0 {
		t.Error("LVMThin CID: Snapshot was not called")
	}
	sid, ok := result.(string)
	if !ok || sid == "" {
		t.Fatalf("LVMThin CID: result: want non-empty snapshot_cid string, got %T %v", result, result)
	}
}

// ---------------------------------------------------------------------------
// Parker-guard tests.
// ---------------------------------------------------------------------------

// snapDepsWithConfig constructs Deps using a caller-supplied *config.CPIConfig.
func snapDepsWithConfig(cfg *config.CPIConfig, qemuSvc qemu.Service, clusterSvc sdkclusterapi.Service) handlers.Deps {
	return handlers.Deps{
		Config: cfg,
		PVE: &mockPVEClient{
			qemuSvc:    qemuSvc,
			clusterSvc: clusterSvc,
		},
		Logger: log.NewNopLogger(),
	}
}

// TestHandleSnapshotDisk_ParkedDisk_Rejected verifies that when the holder VM
// is a parker VM (VMID in range, bosh-parker tag), snapshot_disk returns a
// non-retriable CloudError and does not call Snapshot.
func TestHandleSnapshotDisk_ParkedDisk_Rejected(t *testing.T) {
	t.Parallel()

	const parkerVMID = 90001
	const parkerNode = testNode
	const volid = "local-lvm:vm-9001-disk-0"

	var snapshotCalled bool

	qemuSvc := &snapQEMUService{
		configFn: func(_ context.Context, _ string, vmid int) (map[string]any, error) {
			if vmid == parkerVMID {
				// Parker VM config: disk attached + bosh-parker tag.
				return map[string]any{
					"scsi5": volid,
					"tags":  "bosh-parker",
				}, nil
			}
			return map[string]any{}, nil
		},
		snapshotFn: func(_ context.Context, _ string, _ int, _ string, _ map[string]any) (string, error) {
			snapshotCalled = true
			return "", nil
		},
	}

	clusterSvc := &snapClusterService{
		listFn: func(_ context.Context, _ *sdkclusterapi.ListResourcesParams) (*sdkclusterapi.ListResourcesResponse, error) {
			return clusterRespWith(parkerVMID, parkerNode), nil
		},
	}

	cfg := &config.CPIConfig{
		Node:                     testNode,
		ParkedDiskVMIDRangeStart: 90000,
		ParkedDiskVMIDRangeEnd:   90999,
		DetachedDiskStrategy:     "parked",
	}

	h := handlers.HandleSnapshotDisk(snapDepsWithConfig(cfg, qemuSvc, clusterSvc))
	_, err := h.Handle(context.Background(), marshalArgs(mustEncodeDiskCID(t, volid, nil)), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected non-retriable CloudError when disk is parked")
	}
	if !cpierrors.IsType(err, cpierrors.TypeCloud) {
		t.Errorf("error type: want TypeCloud (non-retriable), got %T: %v", err, err)
	}
	if cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Errorf("error must NOT be retriable for parked-disk snapshot guard")
	}
	if snapshotCalled {
		t.Error("Snapshot must not be called when disk is parked")
	}
}

// TestHandleSnapshotDisk_StrandedParker_Rejected verifies the guard classifies
// the holder by tag, not by band: a parker left outside a cleared band still
// refuses the snapshot, because a PVE snapshot takes the whole VM and would
// entangle every disk that parker holds.
func TestHandleSnapshotDisk_StrandedParker_Rejected(t *testing.T) {
	t.Parallel()

	const parkerVMID = 90001
	const volid = "local-lvm:vm-9001-disk-0"

	var snapshotCalled bool
	qemuSvc := &snapQEMUService{
		configFn: func(_ context.Context, _ string, vmid int) (map[string]any, error) {
			if vmid == parkerVMID {
				return map[string]any{"scsi5": volid, "tags": "bosh-cpi;bosh-parker"}, nil
			}
			return map[string]any{}, nil
		},
		snapshotFn: func(_ context.Context, _ string, _ int, _ string, _ map[string]any) (string, error) {
			snapshotCalled = true
			return "", nil
		},
	}
	clusterSvc := &snapClusterService{
		listFn: func(_ context.Context, _ *sdkclusterapi.ListResourcesParams) (*sdkclusterapi.ListResourcesResponse, error) {
			return clusterRespWith(parkerVMID, testNode), nil
		},
	}

	// Free strategy with the band unset (the effective accessors still resolve
	// the built-in one); the refusal must key on the bosh-parker tag alone.
	cfg := &config.CPIConfig{
		Node:                 testNode,
		DetachedDiskStrategy: "free",
	}

	h := handlers.HandleSnapshotDisk(snapDepsWithConfig(cfg, qemuSvc, clusterSvc))
	_, err := h.Handle(context.Background(), marshalArgs(mustEncodeDiskCID(t, volid, nil)), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected a refusal for a tagged parker holder even with the band cleared")
	}
	if !cpierrors.IsType(err, cpierrors.TypeCloud) || cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Errorf("refusal must be a non-retriable CloudError; got %T: %v", err, err)
	}
	if snapshotCalled {
		t.Error("Snapshot must not be called for a parker holder")
	}
}

// TestHandleSnapshotDisk_ParkedStrategyActive_RealVM_Proceeds verifies that
// when parked strategy is active but the holder is a real VM (VMID outside
// the parker range), snapshot proceeds normally with no extra Config calls
// beyond what FindVMByDiskVolid already makes.
func TestHandleSnapshotDisk_ParkedStrategyActive_RealVM_Proceeds(t *testing.T) {
	t.Parallel()

	const realVMID = 200 // well outside parker range 90000–90999
	const volid = "local-lvm:vm-9001-disk-0"

	var snapshotCalled bool
	// configFn call count: FindVMByDiskVolid reads Config for each cluster VM.
	// The parker guard must NOT add an extra Config call when vmid is out of range.
	var configCalls int

	qemuSvc := &snapQEMUService{
		configFn: func(_ context.Context, _ string, vmid int) (map[string]any, error) {
			configCalls++
			if vmid == realVMID {
				return map[string]any{"scsi0": volid}, nil
			}
			return map[string]any{}, nil
		},
		snapshotFn: func(_ context.Context, _ string, _ int, _ string, _ map[string]any) (string, error) {
			snapshotCalled = true
			return "", nil
		},
	}

	clusterSvc := &snapClusterService{
		listFn: func(_ context.Context, _ *sdkclusterapi.ListResourcesParams) (*sdkclusterapi.ListResourcesResponse, error) {
			return clusterRespWith(realVMID, testNode), nil
		},
	}

	cfg := &config.CPIConfig{
		Node:                     testNode,
		ParkedDiskVMIDRangeStart: 90000,
		ParkedDiskVMIDRangeEnd:   90999,
		DetachedDiskStrategy:     "parked",
	}

	h := handlers.HandleSnapshotDisk(snapDepsWithConfig(cfg, qemuSvc, clusterSvc))
	_, err := h.Handle(context.Background(), marshalArgs(mustEncodeDiskCID(t, volid, nil)), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error for real VM with parked strategy active: %v", err)
	}
	if !snapshotCalled {
		t.Error("Snapshot must be called for real VM holder")
	}
	// Config called exactly once (FindVMByDiskVolid scans cluster VMs).
	// Parker guard adds zero calls because vmid=200 is out of the parker range.
	if configCalls != 1 {
		t.Errorf("parker guard: want 1 Config call (scan only, no extra for out-of-range vmid), got %d", configCalls)
	}
}

// TestHandleSnapshotDisk_NeverOptedIn_ZeroExtraCalls verifies that when
// ParkedStrategyActive() is false (strategy unset, range unset), no parker
// API calls are made and snapshot succeeds as before.
func TestHandleSnapshotDisk_NeverOptedIn_ZeroExtraCalls(t *testing.T) {
	t.Parallel()

	const volid = "local-lvm:vm-9001-disk-0"

	var configCalls int
	var snapshotCalled bool

	qemuSvc := &snapQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			configCalls++
			return map[string]any{"scsi0": volid}, nil
		},
		snapshotFn: func(_ context.Context, _ string, _ int, _ string, _ map[string]any) (string, error) {
			snapshotCalled = true
			return "", nil
		},
	}

	clusterSvc := &snapClusterService{
		listFn: func(_ context.Context, _ *sdkclusterapi.ListResourcesParams) (*sdkclusterapi.ListResourcesResponse, error) {
			return clusterRespWith(100, testNode), nil
		},
	}

	// Never-opted-in: no strategy, no range.
	cfg := &config.CPIConfig{Node: testNode}

	h := handlers.HandleSnapshotDisk(snapDepsWithConfig(cfg, qemuSvc, clusterSvc))
	_, err := h.Handle(context.Background(), marshalArgs(mustEncodeDiskCID(t, volid, nil)), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error for never-opted-in: %v", err)
	}
	if !snapshotCalled {
		t.Error("Snapshot must be called when strategy never opted in")
	}
	// Only the Config read from FindVMByDiskVolid — no parker guard call.
	if configCalls != 1 {
		t.Errorf("never-opted-in: want exactly 1 Config call, got %d (possible extra parker call)", configCalls)
	}
}

// TestHandleSnapshotDisk_InRangeNoParkerTag_Proceeds verifies that when the
// holder VMID falls in the parker range but lacks the bosh-parker tag (not a
// parker VM), the snapshot proceeds normally.
func TestHandleSnapshotDisk_InRangeNoParkerTag_Proceeds(t *testing.T) {
	t.Parallel()

	const vmidInRange = 90050
	const volid = "local-lvm:vm-9001-disk-0"

	var snapshotCalled bool

	qemuSvc := &snapQEMUService{
		configFn: func(_ context.Context, _ string, vmid int) (map[string]any, error) {
			if vmid == vmidInRange {
				// In-range VM but NO bosh-parker tag.
				return map[string]any{"scsi0": volid}, nil
			}
			return map[string]any{}, nil
		},
		snapshotFn: func(_ context.Context, _ string, _ int, _ string, _ map[string]any) (string, error) {
			snapshotCalled = true
			return "", nil
		},
	}

	clusterSvc := &snapClusterService{
		listFn: func(_ context.Context, _ *sdkclusterapi.ListResourcesParams) (*sdkclusterapi.ListResourcesResponse, error) {
			return clusterRespWith(vmidInRange, testNode), nil
		},
	}

	cfg := &config.CPIConfig{
		Node:                     testNode,
		ParkedDiskVMIDRangeStart: 90000,
		ParkedDiskVMIDRangeEnd:   90999,
		DetachedDiskStrategy:     "parked",
	}

	h := handlers.HandleSnapshotDisk(snapDepsWithConfig(cfg, qemuSvc, clusterSvc))
	_, err := h.Handle(context.Background(), marshalArgs(mustEncodeDiskCID(t, volid, nil)), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("in-range VM without bosh-parker tag must not be blocked: %v", err)
	}
	if !snapshotCalled {
		t.Error("Snapshot must be called when in-range VM is not a parker VM")
	}
}

// TODO(storage-network): nfs — wired to PVE network-call boundary, stubbed pending
// live shared-storage test infrastructure. Storage: nfs-store:9001/vm-9001-disk-0.qcow2. Re-enable when
// integration-test harness provides a nfs pool via env.
//
// func TestHandleSnapshotDisk_NFS_CID(t *testing.T) { ... }

// TODO(storage-network): rbd — wired to PVE network-call boundary, stubbed pending
// live shared-storage test infrastructure. Storage: ceph-pool:vm-9001-disk-0. Re-enable when
// integration-test harness provides a rbd pool via env.
//
// func TestHandleSnapshotDisk_RBD_CID(t *testing.T) { ... }

// TODO(storage-network): cephfs — wired to PVE network-call boundary, stubbed pending
// live shared-storage test infrastructure. Storage: cephfs-pool:vm-9001-disk-0. Re-enable when
// integration-test harness provides a cephfs pool via env.
//
// func TestHandleSnapshotDisk_CephFS_CID(t *testing.T) { ... }

// TODO(storage-network): cifs — wired to PVE network-call boundary, stubbed pending
// live shared-storage test infrastructure. Storage: cifs-store:9001/vm-9001-disk-0.qcow2. Re-enable when
// integration-test harness provides a cifs pool via env.
//
// func TestHandleSnapshotDisk_CIFS_CID(t *testing.T) { ... }

// ListStatus reports no offline members; the fixture cluster is fully online.
func (m *snapClusterService) ListStatus(context.Context) (*sdkclusterapi.ListStatusResponse, error) {
	empty := sdkclusterapi.ListStatusResponse{}
	return &empty, nil
}
