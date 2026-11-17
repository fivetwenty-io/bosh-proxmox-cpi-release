package handlers_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/tasks"
)

// volumeScene describes how the node holding VM 101 sees its storages.
type volumeScene struct {
	// entries is the cluster storage index.
	entries []map[string]any
	// inactive names storages the node reports inactive, and absent names
	// storages its status read does not report at all.
	inactive, absent string
	// listed is what the node's content listing shows for every storage.
	listed []string
}

// runDeleteWithVolumes deletes VM 101 whose config names the given scsi0
// volume, with the node seeing its storages as scene says, and returns the
// warnings the delete logged.
func runDeleteWithVolumes(t *testing.T, volume string, scene volumeScene) []log.Entry {
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
		listStorageContentFn: func(context.Context, string, string, *nodes.ListStorageContentParams) (*nodes.ListStorageContentResponse, error) {
			return storageContentListing(scene.listed...), nil
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
	client.clusterStorageSvc = &deadNodeStorage{entries: scene.entries}
	client.storageStatusFn = func(_ context.Context, _, storage string) (*nodes.ListStorageStatusResponse, error) {
		if storage == scene.absent {
			return nil, errors.New("storage '" + storage + "' is not available on node")
		}
		kind := "lvmthin"
		for _, e := range scene.entries {
			if e["storage"] == storage {
				kind, _ = e["type"].(string)
			}
		}
		active := 1
		if storage == scene.inactive {
			active = 0
		}
		out := &nodes.ListStorageStatusResponse{}
		raw, _ := json.Marshal(map[string]any{"active": active, "enabled": 1, "type": kind})
		if err := json.Unmarshal(raw, out); err != nil {
			return nil, err
		}
		return out, nil
	}

	if _, err := handlers.HandleDeleteVM(deps).Handle(context.Background(), marshalArgs("101"), jsonrpc.Context{}); err != nil {
		t.Fatalf("delete must not fail over a volume it cannot reach: %v", err)
	}
	var warns []log.Entry
	for _, e := range obs.All() {
		if e.Level == log.LevelWarn && strings.Contains(e.Message, "does not hold it") {
			warns = append(warns, e)
		}
	}
	return warns
}

func checkUnreachableWarning(t *testing.T, w log.Entry, volid, storage string) {
	t.Helper()
	if w.Attrs["volid"] != volid || w.Attrs["storage"] != storage {
		t.Errorf("warning does not name the volume and storage: %v", w.Attrs)
	}
	if w.Attrs["vmid"] != int64(101) && w.Attrs["vmid"] != 101 {
		t.Errorf("warning does not name the VM: %v", w.Attrs)
	}
	for _, want := range []string{"stay behind", "once that node is back", "storage is not usable on this node", "no such volume"} {
		if !strings.Contains(w.Message, want) {
			t.Errorf("message %q lacks %q", w.Message, want)
		}
	}
}

// TestHandleDeleteVM_WarnsForLocalVolumeMissingFromActiveStorage covers the
// case the storage status cannot show. The storage is defined for every node,
// so it is active here, but the volume lives in another node's copy of it.
func TestHandleDeleteVM_WarnsForLocalVolumeMissingFromActiveStorage(t *testing.T) {
	t.Parallel()

	warns := runDeleteWithVolumes(t, "local-lvm-data:vm-101-ephemeral-0", volumeScene{
		entries: []map[string]any{{"storage": "local-lvm-data", "type": "lvmthin"}},
		listed:  []string{"local-lvm-data:vm-999-disk-0"},
	})
	if len(warns) != 1 {
		t.Fatalf("want 1 warning, got %d", len(warns))
	}
	checkUnreachableWarning(t, warns[0], "local-lvm-data:vm-101-ephemeral-0", "local-lvm-data")
	if _, has := warns[0].Attrs["likely_node"]; has {
		t.Errorf("an unrestricted storage has no likely node: %v", warns[0].Attrs)
	}
}

func TestHandleDeleteVM_WarnsForLocalVolumeOnStorageAbsentFromNodeStatus(t *testing.T) {
	t.Parallel()

	warns := runDeleteWithVolumes(t, "local-lvm-data:vm-101-ephemeral-0", volumeScene{
		entries: []map[string]any{{"storage": "local-lvm-data", "type": "lvmthin", "nodes": "pve-node2"}},
		absent:  "local-lvm-data",
	})
	if len(warns) != 1 {
		t.Fatalf("want 1 warning, got %d", len(warns))
	}
	checkUnreachableWarning(t, warns[0], "local-lvm-data:vm-101-ephemeral-0", "local-lvm-data")
	if warns[0].Attrs["likely_node"] != "pve-node2" {
		t.Errorf("warning does not name the likely node: %v", warns[0].Attrs)
	}
}

func TestHandleDeleteVM_WarnsForLocalVolumeOnStorageInactiveOnNode(t *testing.T) {
	t.Parallel()

	warns := runDeleteWithVolumes(t, "local-lvm:vm-101-disk-0", volumeScene{
		entries:  []map[string]any{{"storage": "local-lvm", "type": "lvmthin"}},
		inactive: "local-lvm",
	})
	if len(warns) != 1 {
		t.Fatalf("want 1 warning, got %d", len(warns))
	}
}

func TestHandleDeleteVM_NoWarningForSharedStorage(t *testing.T) {
	t.Parallel()

	warns := runDeleteWithVolumes(t, "nfs-ephemeral:101/vm-101-disk-0.qcow2", volumeScene{
		entries:  []map[string]any{{"storage": "nfs-ephemeral", "type": "nfs"}},
		inactive: "nfs-ephemeral",
	})
	if len(warns) != 0 {
		t.Fatalf("shared storage must not warn, got %d", len(warns))
	}
}

func TestHandleDeleteVM_NoWarningForLocalVolumePresentOnNode(t *testing.T) {
	t.Parallel()

	warns := runDeleteWithVolumes(t, "local-lvm:vm-101-disk-0", volumeScene{
		entries: []map[string]any{{"storage": "local-lvm", "type": "lvmthin"}},
		listed:  []string{"local-lvm:vm-101-disk-0"},
	})
	if len(warns) != 0 {
		t.Fatalf("a volume the node holds must not warn, got %d", len(warns))
	}
}
