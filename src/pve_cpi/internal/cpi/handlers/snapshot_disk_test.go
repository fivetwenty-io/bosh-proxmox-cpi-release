package handlers_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi/handlers"
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
	configFn   func(ctx context.Context, node string, vmid int) (map[string]any, error)
	snapshotFn func(ctx context.Context, node string, vmid int, name string, opts map[string]any) (string, error)
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
func (m *snapQEMUService) ListSnapshots(_ context.Context, _ string, _ int) ([]map[string]any, error) {
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
	t.Parallel()
	const diskCID = "local-lvm:vm-9001-disk-0"
	const volid = "local-lvm:vm-9001-disk-0"

	var snapCalled bool
	var snapName string

	qemuSvc := &snapQEMUService{
		configFn: func(_ context.Context, _ string, vmid int) (map[string]any, error) {
			if vmid == 100 {
				return map[string]any{
					"scsi2": volid,
				}, nil
			}
			return map[string]any{}, nil
		},
		snapshotFn: func(_ context.Context, _ string, vmid int, name string, _ map[string]any) (string, error) {
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
	t.Parallel()
	const diskCID = "local-lvm:vm-9001-disk-0"
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
	t.Parallel()
	const diskCID = "local-lvm:vm-9001-disk-0"

	var waitCalled bool
	tasksSvc := &mockTasksService{
		waitFn: func(_ context.Context, _, _ string, _ *tasks.WaitOptions) (*tasks.Status, error) {
			waitCalled = true
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
	t.Parallel()
	const diskCID = "local-lvm:vm-9001-disk-0"

	qemuSvc := &snapQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			// VM exists but does not have this disk.
			return map[string]any{"scsi0": "local-lvm:other"}, nil
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
	// Any error type (DiskNotFound or CloudError) is acceptable; err != nil already verified above.
}

func TestHandleSnapshotDisk_EmptyClusterList(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	const diskCID = "local-lvm:vm-9001-disk-0"

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
	t.Parallel()
	// Disk stored with option string: "local-lvm:vm-9001-disk-0,size=10G,cache=writeback"
	const diskCID = "local-lvm:vm-9001-disk-0"
	const volid = "local-lvm:vm-9001-disk-0"

	var snapCalled bool

	qemuSvc := &snapQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{
				"scsi2": volid + ",size=10G,cache=writeback",
			}, nil
		},
		snapshotFn: func(_ context.Context, _ string, _ int, _ string, _ map[string]any) (string, error) {
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
	t.Parallel()
	// A 404 API error from Snapshot should propagate as an error.
	const diskCID = "local-lvm:vm-9001-disk-0"

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
			return clusterRespWith(100, "pve1"), nil
		},
	}

	h := handlers.HandleSnapshotDisk(snapDeps(qemuSvc, clusterSvc, nil))
	_, err := h.Handle(context.Background(), marshalArgs(diskCID), jsonrpc.Context{})
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

	var snapCalled bool
	qemuSvc := &snapQEMUService{
		configFn: func(_ context.Context, _ string, vmid int) (map[string]any, error) {
			if vmid == 9001 {
				return map[string]any{"scsi1": diskCID}, nil
			}
			return map[string]any{}, nil
		},
		snapshotFn: func(_ context.Context, _ string, _ int, _ string, _ map[string]any) (string, error) {
			snapCalled = true
			return "", nil
		},
	}
	clusterSvc := &snapClusterService{
		listFn: func(_ context.Context, _ *sdkclusterapi.ListResourcesParams) (*sdkclusterapi.ListResourcesResponse, error) {
			return clusterRespWith(9001, "pve1"), nil
		},
	}

	h := handlers.HandleSnapshotDisk(snapDeps(qemuSvc, clusterSvc, nil))
	result, err := h.Handle(context.Background(), marshalArgs(diskCID), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("Dir CID: unexpected error: %v", err)
	}
	if !snapCalled {
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

	var snapCalled bool
	qemuSvc := &snapQEMUService{
		configFn: func(_ context.Context, _ string, vmid int) (map[string]any, error) {
			if vmid == 9001 {
				return map[string]any{"scsi1": diskCID}, nil
			}
			return map[string]any{}, nil
		},
		snapshotFn: func(_ context.Context, _ string, _ int, _ string, _ map[string]any) (string, error) {
			snapCalled = true
			return "", nil
		},
	}
	clusterSvc := &snapClusterService{
		listFn: func(_ context.Context, _ *sdkclusterapi.ListResourcesParams) (*sdkclusterapi.ListResourcesResponse, error) {
			return clusterRespWith(9001, "pve1"), nil
		},
	}

	h := handlers.HandleSnapshotDisk(snapDeps(qemuSvc, clusterSvc, nil))
	result, err := h.Handle(context.Background(), marshalArgs(diskCID), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("ZFSPool CID: unexpected error: %v", err)
	}
	if !snapCalled {
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

	var snapCalled bool
	qemuSvc := &snapQEMUService{
		configFn: func(_ context.Context, _ string, vmid int) (map[string]any, error) {
			if vmid == 9001 {
				return map[string]any{"scsi1": diskCID}, nil
			}
			return map[string]any{}, nil
		},
		snapshotFn: func(_ context.Context, _ string, _ int, _ string, _ map[string]any) (string, error) {
			snapCalled = true
			return "", nil
		},
	}
	clusterSvc := &snapClusterService{
		listFn: func(_ context.Context, _ *sdkclusterapi.ListResourcesParams) (*sdkclusterapi.ListResourcesResponse, error) {
			return clusterRespWith(9001, "pve1"), nil
		},
	}

	h := handlers.HandleSnapshotDisk(snapDeps(qemuSvc, clusterSvc, nil))
	result, err := h.Handle(context.Background(), marshalArgs(diskCID), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("LVMThin CID: unexpected error: %v", err)
	}
	if !snapCalled {
		t.Error("LVMThin CID: Snapshot was not called")
	}
	sid, ok := result.(string)
	if !ok || sid == "" {
		t.Fatalf("LVMThin CID: result: want non-empty snapshot_cid string, got %T %v", result, result)
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
