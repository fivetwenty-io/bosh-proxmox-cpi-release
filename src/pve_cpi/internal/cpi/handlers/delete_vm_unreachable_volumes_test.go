package handlers_test

import (
	"context"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/tasks"
)

// runDeleteWithVolumes deletes VM 101 whose config names the given scsi0
// volume, against a storage index of the given entries, and returns the
// warnings the delete logged. The storage status read answers active unless
// inactive names the storage.
func runDeleteWithVolumes(
	t *testing.T, volume string, entries []map[string]any, inactive string,
) []log.Entry {
	t.Helper()

	qemuSvc := &mockQEMUService{
		stopFn: func(context.Context, string, int) (string, error) { return "UPID:node:stop-task", nil },
		configFn: func(context.Context, string, int) (map[string]any, error) {
			return map[string]any{"scsi0": volume + ",size=2G"}, nil
		},
	}
	nodesSvc := &mockNodesService{
		deleteQemuFn: func(context.Context, string, string, *nodes.DeleteQemuParams) (*nodes.DeleteQemuResponse, error) {
			return &nodes.DeleteQemuResponse{}, nil
		},
	}
	tasksSvc := &mockTasksService{
		waitFn: func(context.Context, string, string, *tasks.WaitOptions) (*tasks.Status, error) {
			return &tasks.Status{ExitStatus: "OK"}, nil
		},
	}
	deps := testDepsFoundVM(101, qemuSvc, nodesSvc, tasksSvc, &mockAgentService{})
	logger, obs := log.NewObservedLogger(log.LevelDebug)
	deps.Logger = logger
	client, ok := deps.PVE.(*mockPVEClient)
	if !ok {
		t.Fatalf("unexpected PVE client type %T", deps.PVE)
	}
	client.clusterStorageSvc = &deadNodeStorage{entries: entries}
	client.storageStatusFn = func(_ context.Context, _, storage string) (*nodes.ListStorageStatusResponse, error) {
		return storageStatusResponse(storage != inactive, 0, 0), nil
	}

	if _, err := handlers.HandleDeleteVM(deps).Handle(context.Background(), marshalArgs("101"), jsonrpc.Context{}); err != nil {
		t.Fatalf("delete must not fail over a volume it cannot reach: %v", err)
	}
	var warns []log.Entry
	for _, e := range obs.All() {
		if e.Level == log.LevelWarn && strings.Contains(e.Message, "cannot reach") {
			warns = append(warns, e)
		}
	}
	return warns
}

func TestHandleDeleteVM_WarnsForLocalVolumeOnStorageNotConfiguredForNode(t *testing.T) {
	t.Parallel()

	warns := runDeleteWithVolumes(t, "local-lvm-data:vm-101-ephemeral-0", []map[string]any{
		{"storage": "local-lvm-data", "type": "lvmthin", "nodes": "pve-node2"},
	}, "local-lvm-data")
	if len(warns) != 1 {
		t.Fatalf("want 1 warning, got %d", len(warns))
	}
	w := warns[0]
	if w.Attrs["volid"] != "local-lvm-data:vm-101-ephemeral-0" || w.Attrs["storage"] != "local-lvm-data" {
		t.Errorf("warning does not name the volume and storage: %v", w.Attrs)
	}
	if w.Attrs["vmid"] != int64(101) && w.Attrs["vmid"] != 101 {
		t.Errorf("warning does not name the VM: %v", w.Attrs)
	}
	if w.Attrs["likely_node"] != "pve-node2" {
		t.Errorf("warning does not name the likely node: %v", w.Attrs)
	}
	for _, want := range []string{"stay behind", "once that node is back"} {
		if !strings.Contains(w.Message, want) {
			t.Errorf("message %q lacks %q", w.Message, want)
		}
	}
}

func TestHandleDeleteVM_WarnsForLocalVolumeOnStorageInactiveOnNode(t *testing.T) {
	t.Parallel()

	warns := runDeleteWithVolumes(t, "local-lvm:vm-101-disk-0", []map[string]any{
		{"storage": "local-lvm", "type": "lvmthin"},
	}, "local-lvm")
	if len(warns) != 1 {
		t.Fatalf("want 1 warning, got %d", len(warns))
	}
	if _, has := warns[0].Attrs["likely_node"]; has {
		t.Errorf("an unrestricted storage has no likely node: %v", warns[0].Attrs)
	}
}

func TestHandleDeleteVM_NoWarningForSharedStorage(t *testing.T) {
	t.Parallel()

	warns := runDeleteWithVolumes(t, "nfs-ephemeral:101/vm-101-disk-0.qcow2", []map[string]any{
		{"storage": "nfs-ephemeral", "type": "nfs", "nodes": "pve-node2"},
	}, "nfs-ephemeral")
	if len(warns) != 0 {
		t.Fatalf("shared storage must not warn, got %d", len(warns))
	}
}

func TestHandleDeleteVM_NoWarningForLocalStorageActiveOnNode(t *testing.T) {
	t.Parallel()

	warns := runDeleteWithVolumes(t, "local-lvm:vm-101-disk-0", []map[string]any{
		{"storage": "local-lvm", "type": "lvmthin", "nodes": "pve-node1,pve-node2"},
	}, "")
	if len(warns) != 0 {
		t.Fatalf("a local storage active on the node must not warn, got %d", len(warns))
	}
}
