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
	configFn func(ctx context.Context, node string, vmid int) (map[string]any, error)
}

func (m *getDisksQEMUService) Config(ctx context.Context, node string, vmid int) (map[string]any, error) {
	if m.configFn != nil {
		return m.configFn(ctx, node, vmid)
	}
	return map[string]any{}, nil
}

// Unimplemented methods.
func (m *getDisksQEMUService) Create(_ context.Context, _ string, _ map[string]any) (string, error) {
	panic("getDisksQEMUService.Create: not expected")
}
func (m *getDisksQEMUService) Status(_ context.Context, _ string, _ int) (map[string]any, error) {
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
func (m *getDisksQEMUService) Clone(_ context.Context, _ string, _ int, _ map[string]any) (string, error) {
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
func (m *getDisksQEMUService) Snapshot(_ context.Context, _ string, _ int, _ string, _ map[string]any) (string, error) {
	panic("getDisksQEMUService.Snapshot: not expected")
}
func (m *getDisksQEMUService) DeleteSnapshot(_ context.Context, _ string, _ int, _ string) error {
	panic("getDisksQEMUService.DeleteSnapshot: not expected")
}
func (m *getDisksQEMUService) ListSnapshots(_ context.Context, _ string, _ int) ([]map[string]any, error) {
	panic("getDisksQEMUService.ListSnapshots: not expected")
}
func (m *getDisksQEMUService) RollbackSnapshot(_ context.Context, _ string, _ int, _ string) (string, error) {
	panic("getDisksQEMUService.RollbackSnapshot: not expected")
}

var _ qemu.Service = (*getDisksQEMUService)(nil)

// ---------------------------------------------------------------------------
// getDisksDeps builds Deps for get_disks tests.
// The cluster mock places VMID 100 on "pve-node1" so FindVMNodeViaCluster
// resolves and the handler proceeds to QEMU.Config. Tests exercising
// not-found behavior (VMNotFound) use the zero-vmid variant directly.
// ---------------------------------------------------------------------------

func getDisksDeps(qemuSvc qemu.Service) handlers.Deps {
	return testDepsFoundVM(100, qemuSvc, nil, nil, &mockAgentService{})
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestHandleGetDisks_MultiplePersistentDisks(t *testing.T) {
	t.Parallel()
	qemuSvc := &getDisksQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{
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
	t.Parallel()
	// VM has only a system disk → persistent disk list is empty.
	qemuSvc := &getDisksQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{
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
	t.Parallel()
	qemuSvc := &getDisksQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
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
	t.Parallel()
	qemuSvc := &getDisksQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
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
	t.Parallel()
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
	t.Parallel()
	h := handlers.HandleGetDisks(getDisksDeps(&getDisksQEMUService{}))
	_, err := h.Handle(context.Background(), marshalArgs(""), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error for empty vm_cid")
	}
}

func TestHandleGetDisks_TooFewArgs(t *testing.T) {
	t.Parallel()
	h := handlers.HandleGetDisks(getDisksDeps(&getDisksQEMUService{}))
	_, err := h.Handle(context.Background(), []json.RawMessage{}, jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error for missing vm_cid argument")
	}
}

func TestHandleGetDisks_MissingNode(t *testing.T) {
	t.Parallel()
	// With cluster-scan-based node resolution, a missing Config.Node is no longer
	// an error: the node is resolved from the cluster scan. When the cluster scan
	// returns not-found (empty list), the handler returns VMNotFound. This test
	// verifies that a missing Config.Node with a VM absent from the cluster returns
	// a VMNotFound error rather than panicking.
	h := handlers.HandleGetDisks(handlers.Deps{
		Config: &config.CPIConfig{Node: ""},
		PVE: &mockPVEClient{
			qemuSvc:    &getDisksQEMUService{},
			clusterSvc: &mockClusterSvc{}, // empty: VM 100 not found
		},
		Logger: log.NewNopLogger(),
	})
	_, err := h.Handle(context.Background(), marshalArgs("100"), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected VMNotFound when Config.Node is empty and cluster scan finds nothing")
	}
	if !cpierrors.IsType(err, cpierrors.TypeVMNotFound) {
		t.Errorf("error type: want VMNotFound, got %T %v", err, err)
	}
}

func TestHandleGetDisks_CloudinitExcluded(t *testing.T) {
	t.Parallel()
	// Verify cloudinit drive (media=cdrom) is not included even when not on ide2.
	qemuSvc := &getDisksQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{
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
	t.Parallel()
	// VM with no disks at all → empty list.
	qemuSvc := &getDisksQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{
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
	t.Parallel()
	// Disk stored as bare volid (no option string, no sentinel entry) → the
	// fallback re-encodes it as a metadata-free pvd- envelope, since the raw
	// "<storage>:<volid>" form is rejected everywhere else.
	const bareVolid = "local-lvm:vm-9001-disk-0"
	qemuSvc := &getDisksQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{
				"scsi0": "local-lvm:vm-100-disk-0", // system disk
				"scsi1": bareVolid,
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
	want := mustEncodeDiskCID(t, bareVolid, nil)
	if diskCIDs[0] != want {
		t.Errorf("disk_cid: want re-encoded envelope %q, got %q", want, diskCIDs[0])
	}
}

// ---------------------------------------------------------------------------
// CID-variant tests: dir, zfspool, lvmthin.
//
// get_disks has no storage-type branching. These tests verify that the handler
// returns the correct bare volid (CID identity) for each storage type. The
// volid extracted by bareVolidFromOptStr is identical to the input CID when
// no option string is present — ParseDiskCID is exercised implicitly by the
// caller, but the handler itself just returns the volid string unchanged.
// ---------------------------------------------------------------------------

func TestHandleGetDisks_Dir_CID(t *testing.T) {
	t.Parallel()
	// dir storage: CID has subpath form "<storage>:<vmid>/<volname>.<ext>".
	// The colon splits at first occurrence; the full volid, re-encoded through
	// EncodeDiskCID (no sentinel entry recorded), is the returned CID.
	const diskCID = "local:9001/vm-9001-disk-0.raw"
	qemuSvc := &getDisksQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{
				"scsi0": "local:100/vm-100-disk-0.raw", // system disk
				"scsi1": diskCID,
			}, nil
		},
	}

	h := handlers.HandleGetDisks(getDisksDeps(qemuSvc))
	result, err := h.Handle(context.Background(), marshalArgs("100"), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("Dir CID: unexpected error: %v", err)
	}

	diskCIDs, ok := result.([]string)
	if !ok {
		t.Fatalf("Dir CID: result: want []string, got %T", result)
	}
	if len(diskCIDs) != 1 {
		t.Fatalf("Dir CID: disk count: want 1, got %d: %v", len(diskCIDs), diskCIDs)
	}
	want := mustEncodeDiskCID(t, diskCID, nil)
	if diskCIDs[0] != want {
		t.Errorf("Dir CID: disk_cid: want re-encoded envelope %q, got %q", want, diskCIDs[0])
	}
}

func TestHandleGetDisks_ZFSPool_CID(t *testing.T) {
	t.Parallel()
	// zfspool storage: bare volname (no subpath), e.g. "local-zfs:vm-9001-disk-0".
	const diskCID = "local-zfs:vm-9001-disk-0"
	qemuSvc := &getDisksQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{
				"scsi0": "local-zfs:vm-100-disk-0", // system disk
				"scsi1": diskCID,
			}, nil
		},
	}

	h := handlers.HandleGetDisks(getDisksDeps(qemuSvc))
	result, err := h.Handle(context.Background(), marshalArgs("100"), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("ZFSPool CID: unexpected error: %v", err)
	}

	diskCIDs, ok := result.([]string)
	if !ok {
		t.Fatalf("ZFSPool CID: result: want []string, got %T", result)
	}
	if len(diskCIDs) != 1 {
		t.Fatalf("ZFSPool CID: disk count: want 1, got %d: %v", len(diskCIDs), diskCIDs)
	}
	want := mustEncodeDiskCID(t, diskCID, nil)
	if diskCIDs[0] != want {
		t.Errorf("ZFSPool CID: disk_cid: want re-encoded envelope %q, got %q", want, diskCIDs[0])
	}
}

func TestHandleGetDisks_LVMThin_CID(t *testing.T) {
	t.Parallel()
	// lvmthin storage: bare volname, e.g. "local-lvm-thin:vm-9001-disk-0".
	const diskCID = "local-lvm-thin:vm-9001-disk-0"
	qemuSvc := &getDisksQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{
				"scsi0": "local-lvm-thin:vm-100-disk-0", // system disk
				"scsi1": diskCID,
			}, nil
		},
	}

	h := handlers.HandleGetDisks(getDisksDeps(qemuSvc))
	result, err := h.Handle(context.Background(), marshalArgs("100"), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("LVMThin CID: unexpected error: %v", err)
	}

	diskCIDs, ok := result.([]string)
	if !ok {
		t.Fatalf("LVMThin CID: result: want []string, got %T", result)
	}
	if len(diskCIDs) != 1 {
		t.Fatalf("LVMThin CID: disk count: want 1, got %d: %v", len(diskCIDs), diskCIDs)
	}
	want := mustEncodeDiskCID(t, diskCID, nil)
	if diskCIDs[0] != want {
		t.Errorf("LVMThin CID: disk_cid: want re-encoded envelope %q, got %q", want, diskCIDs[0])
	}
}

// TODO(storage-network): nfs — wired to PVE network-call boundary, stubbed pending
// live shared-storage test infrastructure. Storage: nfs-store:9001/vm-9001-disk-0.qcow2. Re-enable when
// integration-test harness provides a nfs pool via env.
//
// func TestHandleGetDisks_NFS_CID(t *testing.T) { ... }

// TODO(storage-network): rbd — wired to PVE network-call boundary, stubbed pending
// live shared-storage test infrastructure. Storage: ceph-pool:vm-9001-disk-0. Re-enable when
// integration-test harness provides a rbd pool via env.
//
// func TestHandleGetDisks_RBD_CID(t *testing.T) { ... }

// TODO(storage-network): cephfs — wired to PVE network-call boundary, stubbed pending
// live shared-storage test infrastructure. Storage: cephfs-pool:vm-9001-disk-0. Re-enable when
// integration-test harness provides a cephfs pool via env.
//
// func TestHandleGetDisks_CephFS_CID(t *testing.T) { ... }

// TODO(storage-network): cifs — wired to PVE network-call boundary, stubbed pending
// live shared-storage test infrastructure. Storage: cifs-store:9001/vm-9001-disk-0.qcow2. Re-enable when
// integration-test harness provides a cifs pool via env.
//
// func TestHandleGetDisks_CIFS_CID(t *testing.T) { ... }
