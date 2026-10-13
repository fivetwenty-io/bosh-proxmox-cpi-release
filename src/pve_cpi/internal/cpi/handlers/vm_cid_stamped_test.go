package handlers_test

import (
	"context"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/tasks"
)

// stampedCID101 is the stamped form of VMID 101, the form create_vm returns by
// default. These tests prove each VM handler parses its CID at entry and hands
// PVE only the bare VMID. The shared mockNodesService rejects any vmid path
// argument that is not all digits, so a handler that leaks the raw CID fails.
const stampedCID101 = "101-0a1b2c3d-4e5f6a7b"

// stampedVMTags carries the cluster and token stamp that match the stamped
// CIDs below. The ownership guard refuses a stamped CID whose VM lacks its
// stamp, so the fixture VM must carry the tags a stamped create would write.
const stampedVMTags = "bosh-cpi;bosh-cluster--0a1b2c3d;bosh-cid--4e5f6a7b"

// stamped serves the fixture VM in deps with stampedVMTags, both on its
// config and on the cluster row that places vmid on node.
func stamped(deps handlers.Deps, vmid int, node string) handlers.Deps {
	return withVMTags(deps, vmid, node, stampedVMTags)
}

func TestMockNodesServiceRejectsNonDigitVMID(t *testing.T) {
	t.Parallel()

	nodesSvc := &mockNodesService{
		updateQemuConfigFn: func(context.Context, string, string, *nodes.UpdateQemuConfigParams) error { return nil },
	}
	for _, vmid := range []string{stampedCID101, "", "12a", "-1"} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("UpdateQemuConfig(%q) did not panic", vmid)
				}
			}()
			_ = nodesSvc.UpdateQemuConfig(context.Background(), "n", vmid, nil)
		}()
	}
	if err := nodesSvc.UpdateQemuConfig(context.Background(), "n", "101", nil); err != nil {
		t.Errorf("UpdateQemuConfig(101): %v", err)
	}
}

func TestHandleHasVM_StampedCID(t *testing.T) {
	t.Parallel()

	h := handlers.HandleHasVM(stamped(testDepsFoundVM(101, nil, nil, nil, &mockAgentService{}), 101, "pve-node1"))
	result, err := h.Handle(context.Background(), marshalArgs(stampedCID101), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got, ok := result.(bool); !ok || !got {
		t.Errorf("has_vm(%q) = %v, want true", stampedCID101, result)
	}
}

func TestHandleSetVMMetadata_StampedCID(t *testing.T) {
	t.Parallel()

	var gotVMID string
	nodesSvc := &mockNodesService{
		updateQemuConfigFn: func(_ context.Context, _ string, vmid string, _ *nodes.UpdateQemuConfigParams) error {
			gotVMID = vmid
			return nil
		},
	}
	h := handlers.HandleSetVMMetadata(stamped(testDepsFoundVM(101, nil, nodesSvc, nil, &mockAgentService{}), 101, "pve-node1"))
	metadata := map[string]any{"director": "bosh-director", "deployment": "cf", "job": "cell", "index": "0", "id": "abc"}
	if _, err := h.Handle(context.Background(), marshalArgs(stampedCID101, metadata), jsonrpc.Context{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotVMID != "101" {
		t.Errorf("UpdateQemuConfig vmid = %q, want 101", gotVMID)
	}
}

func TestHandleRebootVM_StampedCID(t *testing.T) {
	t.Parallel()

	var gotVMID string
	qemuSvc := &mockQEMUService{
		statusFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{"status": "running"}, nil
		},
	}
	nodesSvc := &mockNodesService{
		createQemuStatusRebootFn: func(_ context.Context, _, vmid string, _ *nodes.CreateQemuStatusRebootParams) (*nodes.CreateQemuStatusRebootResponse, error) {
			gotVMID = vmid
			return rebootRawUPID("UPID:node:reboot-task"), nil
		},
	}
	tasksSvc := &mockTasksService{
		waitFn: func(_ context.Context, _, _ string, _ *tasks.WaitOptions) (*tasks.Status, error) {
			return &tasks.Status{ExitStatus: "OK"}, nil
		},
	}
	h := handlers.HandleRebootVM(stamped(testDepsReboot(qemuSvc, nodesSvc, tasksSvc, "soft"), 101, "pve-node1"))
	if _, err := h.Handle(context.Background(), marshalArgs(stampedCID101), jsonrpc.Context{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotVMID != "101" {
		t.Errorf("CreateQemuStatusReboot vmid = %q, want 101", gotVMID)
	}
}

func TestHandleDeleteVM_StampedCID(t *testing.T) {
	t.Parallel()

	var deleted []string
	qemuSvc := &mockQEMUService{
		stopFn: func(_ context.Context, _ string, _ int) (string, error) { return "UPID:node:stop-task", nil },
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{}, nil
		},
	}
	nodesSvc := &mockNodesService{
		deleteQemuFn: func(_ context.Context, _ string, vmid string, _ *nodes.DeleteQemuParams) (*nodes.DeleteQemuResponse, error) {
			deleted = append(deleted, vmid)
			return &nodes.DeleteQemuResponse{}, nil
		},
		updateQemuConfigFn: func(context.Context, string, string, *nodes.UpdateQemuConfigParams) error { return nil },
	}
	tasksSvc := &mockTasksService{
		waitFn: func(_ context.Context, _, _ string, _ *tasks.WaitOptions) (*tasks.Status, error) {
			return &tasks.Status{ExitStatus: "OK"}, nil
		},
	}
	h := handlers.HandleDeleteVM(stamped(testDepsFoundVM(101, qemuSvc, nodesSvc, tasksSvc, &mockAgentService{
		removeFn: func(context.Context, string, int) error { return nil },
	}), 101, "pve-node1"))
	if _, err := h.Handle(context.Background(), marshalArgs(stampedCID101), jsonrpc.Context{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(deleted) != 1 || deleted[0] != "101" {
		t.Errorf("DeleteQemu vmids = %v, want [101]", deleted)
	}
}

// stampedCID100 is the stamped form of VMID 100, the VM the disk and snapshot
// fixtures in this package place on the test node.
const stampedCID100 = "100-0a1b2c3d-4e5f6a7b"

func TestHandleAttachDisk_StampedCID(t *testing.T) {
	t.Parallel()
	const volid = "vm-9001-disk-0"

	qemuSvc := &attachQEMUService{
		attachReturnDiskID: diskSlot,
		configCfg: map[string]any{
			"scsi0":  testDiskCID,
			"scsi1":  "local-lvm:vm-100-disk-1",
			diskSlot: volid,
		},
	}
	h := handlers.HandleAttachDisk(stamped(attachDeps(qemuSvc, &captureAgent{}), 100, testNode))
	result, err := h.Handle(context.Background(), attachArgs(t, stampedCID100, diskCID), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("result is nil; want disk_hints object")
	}
	if qemuSvc.attachLastVolid == "" {
		t.Error("AttachDisk was not called")
	}
}

func TestHandleDetachDisk_StampedCID(t *testing.T) {
	t.Parallel()
	const volid = "local-lvm:vm-9001-disk-0"

	qemuSvc := &detachQEMUService{
		configCfg: map[string]any{
			"scsi0":  testDiskCID,
			"scsi1":  "local-lvm:vm-100-disk-1",
			diskSlot: volid,
		},
	}
	h := handlers.HandleDetachDisk(stamped(detachDeps(qemuSvc), 100, testNode))
	if _, err := h.Handle(context.Background(), detachArgs(t, stampedCID100, diskCID), jsonrpc.Context{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !qemuSvc.detachCalled || qemuSvc.detachedDiskID != diskSlot {
		t.Errorf("DetachDisk called=%v slot=%q, want slot %q", qemuSvc.detachCalled, qemuSvc.detachedDiskID, diskSlot)
	}
}

func TestHandleGetDisks_StampedCID(t *testing.T) {
	t.Parallel()
	qemuSvc := &getDisksQEMUService{
		configFn: func(_ context.Context, _ string, vmid int) (map[string]any, error) {
			if vmid != 100 {
				t.Errorf("Config vmid = %d, want 100", vmid)
			}
			return map[string]any{
				"scsi0": "local-lvm:vm-100-disk-0",
				"scsi1": "local-lvm:vm-100-disk-1",
			}, nil
		},
	}
	h := handlers.HandleGetDisks(stamped(getDisksDeps(qemuSvc), 100, "pve-node1"))
	result, err := h.Handle(context.Background(), marshalArgs(stampedCID100), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got, ok := result.([]string); !ok || len(got) != 1 {
		t.Errorf("result = %v, want one persistent disk", result)
	}
}

func TestHandleDeleteSnapshot_StampedCID(t *testing.T) {
	t.Parallel()
	var gotVMID int
	var gotName string
	qemuSvc := &delSnapQEMUService{
		deleteSnapshotFn: func(_ context.Context, _ string, vmid int, name string) error {
			gotVMID, gotName = vmid, name
			return nil
		},
	}
	h := handlers.HandleDeleteSnapshot(stamped(delSnapDeps(qemuSvc), 100, testNode))
	if _, err := h.Handle(context.Background(), marshalArgs(stampedCID100+":bosh-1234567890-abcd1234"), jsonrpc.Context{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotVMID != 100 || gotName != "bosh-1234567890-abcd1234" {
		t.Errorf("DeleteSnapshot vmid=%d name=%q, want 100 and the snapshot name", gotVMID, gotName)
	}
}
