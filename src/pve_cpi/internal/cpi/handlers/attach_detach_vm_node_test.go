package handlers_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
)

// On a multi-node cluster the VM-config operations of attach_disk and
// detach_disk must target the node that RUNS the VM. For shared-storage
// disks the backend's NodeForExisting answer is the configured default node —
// a storage routing hint only. Observed live: attach_disk after create_vm
// placed the VM on another node and every config call 404'd with
// "Configuration file 'nodes/<default>/qemu-server/<vmid>.conf' does not
// exist". These tests pin that both handlers follow the /cluster/resources
// answer instead.

// nodeCapturingQEMU wraps attachQEMUService and records the node argument of
// every Config / AttachDisk / DetachDisk call.
type nodeCapturingQEMU struct {
	attachQEMUService
	nodes []string
}

func (m *nodeCapturingQEMU) Config(ctx context.Context, node string, vmid int) (map[string]any, error) {
	m.nodes = append(m.nodes, node)
	return m.attachQEMUService.Config(ctx, node, vmid)
}

func (m *nodeCapturingQEMU) AttachDisk(ctx context.Context, node string, vmid int, volid string, bus string, opts *qemu.AttachOpts) (string, error) {
	m.nodes = append(m.nodes, node)
	return m.attachQEMUService.AttachDisk(ctx, node, vmid, volid, bus, opts)
}

func (m *nodeCapturingQEMU) DetachDisk(ctx context.Context, node string, vmid int, diskID string) error {
	m.nodes = append(m.nodes, node)
	return m.attachQEMUService.DetachDisk(ctx, node, vmid, diskID)
}

// clusterReportingVMOn returns a mockClusterSvc whose resource list places
// vmid on the given node.
func clusterReportingVMOn(vmid int, node string) *mockClusterSvc {
	return &mockClusterSvc{
		listResourcesFn: func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			entry, _ := json.Marshal(map[string]any{"vmid": vmid, "node": node})
			resp := sdkcluster.ListResourcesResponse{entry}
			return &resp, nil
		},
	}
}

func assertAllNodes(t *testing.T, got []string, want string) {
	t.Helper()
	if len(got) == 0 {
		t.Fatal("no QEMU calls were captured")
	}
	for i, n := range got {
		if n != want {
			t.Errorf("QEMU call %d targeted node %q; want %q (the VM's node per /cluster/resources)", i, n, want)
		}
	}
}

func TestHandleAttachDisk_SharedDisk_TargetsVMNodeNotDefault(t *testing.T) {
	t.Parallel()
	const (
		vmCID  = "4229"
		vmNode = "node2" // VM's actual node; testNode is the configured default
		volid  = "local-lvm:vm-9001-disk-0"
	)

	qemuSvc := &nodeCapturingQEMU{
		attachQEMUService: attachQEMUService{
			attachReturnDiskID: "scsi1",
			configCfg: map[string]any{
				"scsi1": volid,
			},
		},
	}
	deps := handlers.Deps{
		Config: &config.CPIConfig{
			Node:         testNode,
			VMDiskFormat: "qcow2",
		},
		PVE: &mockPVEClient{
			qemuSvc:    qemuSvc,
			clusterSvc: clusterReportingVMOn(4229, vmNode),
		},
		Agent:  &captureAgent{},
		Logger: log.NewNopLogger(),
	}

	h := handlers.HandleAttachDisk(deps)
	result, err := h.Handle(context.Background(), attachArgs(t, vmCID, volid), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected disk_hints; got nil")
	}
	assertAllNodes(t, qemuSvc.nodes, vmNode)
}

func TestHandleDetachDisk_SharedDisk_TargetsVMNodeNotDefault(t *testing.T) {
	t.Parallel()
	const (
		vmCID  = "4229"
		vmNode = "node2"
		volid  = "local-lvm:vm-9001-disk-0"
	)

	qemuSvc := &nodeCapturingQEMU{
		attachQEMUService: attachQEMUService{
			configCfg: map[string]any{
				"scsi1": volid,
			},
		},
	}
	deps := handlers.Deps{
		Config: &config.CPIConfig{
			Node:         testNode,
			VMDiskFormat: "qcow2",
		},
		PVE: &mockPVEClient{
			qemuSvc:    qemuSvc,
			clusterSvc: clusterReportingVMOn(4229, vmNode),
		},
		Agent:  &captureAgent{},
		Logger: log.NewNopLogger(),
	}

	h := handlers.HandleDetachDisk(deps)
	_, err := h.Handle(context.Background(), detachArgs(t, vmCID, volid), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertAllNodes(t, qemuSvc.nodes, vmNode)
	if len(qemuSvc.detachCalls) != 1 {
		t.Errorf("expected exactly 1 DetachDisk call; got %v", qemuSvc.detachCalls)
	}
}
