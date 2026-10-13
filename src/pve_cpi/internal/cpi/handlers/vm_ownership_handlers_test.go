package handlers_test

// Handler-level pins for the VM ownership guard. Each test reaches a VM
// through the handler's public entry point with a CID the Director could
// really hold, on the shared digits-only mocks, and checks both the answer and
// that nothing was mutated. The predicate itself is pinned row by row in
// vm_ownership_internal_test.go.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/tasks"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

const (
	// ownRequestDirector is the Director making every request in this file.
	ownRequestDirector = "5b0c2a4e-1d3f-4a6b-9c8d-7e6f5a4b3c2d"
	// ownOtherDirector is a second Director sharing the cluster.
	ownOtherDirector = "9f8e7d6c-5b4a-4392-8170-6f5e4d3c2b1a"

	// ownForeignTags are the tags a VM the other Director created carries.
	ownForeignTags = "bosh-cpi;bosh-owner--" + ownOtherDirector
	// ownOwnTags are the tags a VM the requesting Director created carries.
	ownOwnTags = "bosh-cpi;bosh-owner--" + ownRequestDirector
	// ownDirectorNotes are the notes set_vm_metadata writes for a deployed VM.
	ownDirectorNotes = "deployment: cf\ndirector: bosh\nname: router/6f3c\n"
	// ownReusedTags are the tags of a VM that took over a stamped CID's VMID
	// on the same cluster after the CID's own VM was destroyed. The cluster
	// stamp matches stampedCID101 and stampedCID100, and the token does not.
	ownReusedTags = "bosh-cpi;bosh-cluster--0a1b2c3d;bosh-cid--deadbeef"
)

// withVMRow is withVMTags for a cluster row that also names the guest type
// and the template flag, so a test can reach a container or a template
// through a handler.
func withVMRow(deps handlers.Deps, vmid int, node, tags, guestType string, template bool) handlers.Deps {
	deps = withVMTags(deps, vmid, node, tags)
	row := map[string]any{"vmid": vmid, "node": node, "type": guestType, "tags": tags}
	if template {
		row["template"] = 1
	}
	raw, _ := json.Marshal(row)
	deps.PVE.(*mockPVEClient).clusterSvc = &mockClusterSvc{
		listResourcesFn: func(context.Context, *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
			return &cluster.ListResourcesResponse{raw}, nil
		},
	}
	return deps
}

// withVMTags serves the fixture VM in deps with tags, both on its config and,
// when the suite places it with a mockClusterSvc, on the cluster row that
// places vmid on node.
func withVMTags(deps handlers.Deps, vmid int, node, tags string) handlers.Deps {
	client := deps.PVE.(*mockPVEClient)
	client.fixtureTags = tags
	if _, ok := client.clusterSvc.(*mockClusterSvc); ok {
		raw, _ := json.Marshal(map[string]any{"vmid": vmid, "node": node, "type": "qemu", "tags": tags})
		client.clusterSvc = &mockClusterSvc{
			listResourcesFn: func(context.Context, *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
				return &cluster.ListResourcesResponse{raw}, nil
			},
		}
	}
	return deps
}

// ownRPC is the request context every call in this file carries. Handlers
// take the requesting Director's UUID from it.
var ownRPC = jsonrpc.Context{DirectorUUID: ownRequestDirector}

// asDirector sets the guard mode deps runs under ("" leaves the default).
func asDirector(deps handlers.Deps, mode string) handlers.Deps {
	deps.Config.VMOwnershipGuard = mode
	return deps
}

// guardModes are the two modes a mutation must refuse in.
var guardModes = []string{config.VMOwnershipGuardEnforce, config.VMOwnershipGuardWarn}

// requireForeignRefusal checks err is the non-retriable refusal naming reason
// and the escapes.
func requireForeignRefusal(t *testing.T, err error, reason string) {
	t.Helper()
	if err == nil {
		t.Fatal("want the ownership refusal, got nil")
	}
	if !cpierrors.IsType(err, cpierrors.TypeCloud) {
		t.Fatalf("error type: want %s, got %v", cpierrors.TypeCloud, err)
	}
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) || cpiErr.OkToRetry() {
		t.Errorf("the refusal must not be retriable: %v", err)
	}
	for _, want := range []string{"which this Director does not own (" + reason + ")", `"Delete VM reference"`, "bosh delete-deployment --force"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not contain %q", err.Error(), want)
		}
	}
}

// ownDeleteVMFixture builds a delete_vm Deps for VM 101 that records every
// destructive call.
type ownDeleteVMFixture struct {
	mu       sync.Mutex
	stopped  bool
	deleted  []string
	removed  bool
	cfgExtra map[string]any
}

func (f *ownDeleteVMFixture) deps() handlers.Deps {
	qemuSvc := &mockQEMUService{
		stopFn: func(context.Context, string, int) (string, error) {
			f.mu.Lock()
			f.stopped = true
			f.mu.Unlock()
			return "UPID:node:stop-task", nil
		},
		configFn: func(context.Context, string, int) (map[string]any, error) {
			cfg := map[string]any{"name": "vm-101"}
			for k, v := range f.cfgExtra {
				cfg[k] = v
			}
			return cfg, nil
		},
	}
	nodesSvc := &mockNodesService{
		deleteQemuFn: func(_ context.Context, _ string, vmid string, _ *nodes.DeleteQemuParams) (*nodes.DeleteQemuResponse, error) {
			f.mu.Lock()
			f.deleted = append(f.deleted, vmid)
			f.mu.Unlock()
			return &nodes.DeleteQemuResponse{}, nil
		},
		updateQemuConfigFn: func(context.Context, string, string, *nodes.UpdateQemuConfigParams) error { return nil },
	}
	tasksSvc := &mockTasksService{
		waitFn: func(context.Context, string, string, *tasks.WaitOptions) (*tasks.Status, error) {
			return &tasks.Status{ExitStatus: "OK"}, nil
		},
	}
	return testDepsFoundVM(101, qemuSvc, nodesSvc, tasksSvc, &mockAgentService{
		removeFn: func(context.Context, string, int) error {
			f.mu.Lock()
			f.removed = true
			f.mu.Unlock()
			return nil
		},
	})
}

func (f *ownDeleteVMFixture) touched() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stopped || len(f.deleted) > 0 || f.removed
}

// TestVMOwnershipGuard_BareCIDOnAnotherDirectorsVM is the cross-Director case
// the guard exists for. A bare CID reaches a VM that carries the other
// Director's owner tag. Every handler that acts on the VM refuses in both
// modes and mutates nothing, and delete_snapshot leaves it untouched.
func TestVMOwnershipGuard_BareCIDOnAnotherDirectorsVM(t *testing.T) {
	t.Parallel()

	for _, mode := range guardModes {
		t.Run("delete_vm/"+mode, func(t *testing.T) {
			t.Parallel()
			f := &ownDeleteVMFixture{}
			deps := asDirector(withVMTags(f.deps(), 101, "pve-node1", ownForeignTags), mode)
			_, err := handlers.HandleDeleteVM(deps).Handle(context.Background(), marshalArgs("101"), ownRPC)
			requireForeignRefusal(t, err, "other-director")
			if f.touched() {
				t.Errorf("delete_vm stopped, destroyed, or deregistered another Director's VM: stopped=%v deleted=%v removed=%v",
					f.stopped, f.deleted, f.removed)
			}
		})

		t.Run("reboot_vm/"+mode, func(t *testing.T) {
			t.Parallel()
			rebooted := false
			qemuSvc := &mockQEMUService{
				statusFn: func(context.Context, string, int) (map[string]any, error) {
					return map[string]any{"status": "running"}, nil
				},
			}
			nodesSvc := &mockNodesService{
				createQemuStatusRebootFn: func(context.Context, string, string, *nodes.CreateQemuStatusRebootParams) (*nodes.CreateQemuStatusRebootResponse, error) {
					rebooted = true
					return rebootRawUPID("UPID:node:reboot-task"), nil
				},
			}
			deps := asDirector(withVMTags(testDepsReboot(qemuSvc, nodesSvc, &mockTasksService{}, "soft"), 101, "pve-node1", ownForeignTags), mode)
			_, err := handlers.HandleRebootVM(deps).Handle(context.Background(), marshalArgs("101"), ownRPC)
			requireForeignRefusal(t, err, "other-director")
			if rebooted {
				t.Error("reboot_vm rebooted another Director's VM")
			}
		})

		t.Run("set_vm_metadata/"+mode, func(t *testing.T) {
			t.Parallel()
			written := false
			nodesSvc := &mockNodesService{
				updateQemuConfigFn: func(context.Context, string, string, *nodes.UpdateQemuConfigParams) error {
					written = true
					return nil
				},
			}
			deps := asDirector(withVMTags(testDepsFoundVM(101, nil, nodesSvc, nil, &mockAgentService{}), 101, "pve-node1", ownForeignTags), mode)
			metadata := map[string]any{"director": "bosh", "deployment": "cf", "name": "router/6f3c"}
			_, err := handlers.HandleSetVMMetadata(deps).Handle(context.Background(), marshalArgs("101", metadata), ownRPC)
			requireForeignRefusal(t, err, "other-director")
			if written {
				t.Error("set_vm_metadata rewrote another Director's VM config")
			}
		})

		t.Run("attach_disk/"+mode, func(t *testing.T) {
			t.Parallel()
			qemuSvc := &attachQEMUService{
				attachReturnDiskID: diskSlot,
				configCfg:          map[string]any{"scsi0": testDiskCID},
			}
			deps := asDirector(withVMTags(attachDeps(qemuSvc, &captureAgent{}), 100, testNode, ownForeignTags), mode)
			_, err := handlers.HandleAttachDisk(deps).Handle(context.Background(), attachArgs(t, "100", diskCID), ownRPC)
			requireForeignRefusal(t, err, "other-director")
			if qemuSvc.attachLastVolid != "" {
				t.Errorf("attach_disk plugged %q into another Director's VM", qemuSvc.attachLastVolid)
			}
		})

		t.Run("detach_disk/"+mode, func(t *testing.T) {
			t.Parallel()
			qemuSvc := &detachQEMUService{
				configCfg: map[string]any{
					"scsi0":  testDiskCID,
					diskSlot: "local-lvm:vm-9001-disk-0",
				},
			}
			deps := asDirector(withVMTags(detachDeps(qemuSvc), 100, testNode, ownForeignTags), mode)
			_, err := handlers.HandleDetachDisk(deps).Handle(context.Background(), detachArgs(t, "100", diskCID), ownRPC)
			requireForeignRefusal(t, err, "other-director")
			if qemuSvc.detachCalled {
				t.Error("detach_disk unplugged a disk from another Director's VM")
			}
		})

		t.Run("snapshot_disk/"+mode, func(t *testing.T) {
			t.Parallel()
			snapshotted := false
			qemuSvc := &snapQEMUService{
				configFn: func(_ context.Context, _ string, vmid int) (map[string]any, error) {
					if vmid == 100 {
						return map[string]any{diskSlot: "local-lvm:vm-9001-disk-0"}, nil
					}
					return map[string]any{}, nil
				},
				snapshotFn: func(context.Context, string, int, string, map[string]any) (string, error) {
					snapshotted = true
					return "", nil
				},
			}
			clusterSvc := &snapClusterService{
				listFn: func(context.Context, *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
					return clusterRespWith(100, testNode), nil
				},
			}
			deps := asDirector(withVMTags(snapDeps(qemuSvc, clusterSvc, nil), 100, testNode, ownForeignTags), mode)
			_, err := handlers.HandleSnapshotDisk(deps).Handle(context.Background(), marshalArgs(mustEncodeDiskCID(t, diskCID, nil)), ownRPC)
			requireForeignRefusal(t, err, "other-director")
			if snapshotted {
				t.Error("snapshot_disk snapshotted another Director's VM")
			}
		})

		t.Run("delete_snapshot/"+mode, func(t *testing.T) {
			t.Parallel()
			deleted := false
			qemuSvc := &delSnapQEMUService{
				deleteSnapshotFn: func(context.Context, string, int, string) error {
					deleted = true
					return nil
				},
			}
			deps := asDirector(withVMTags(delSnapDeps(qemuSvc), 100, testNode, ownForeignTags), mode)
			result, err := handlers.HandleDeleteSnapshot(deps).Handle(context.Background(), marshalArgs("100:bosh-1234567890-abcd1234"), ownRPC)
			if err != nil || result != nil {
				t.Fatalf("delete_snapshot = (%v, %v), want success with nothing done", result, err)
			}
			if deleted {
				t.Error("delete_snapshot removed a snapshot from another Director's VM")
			}
		})
	}
}

// TestVMOwnershipGuard_ReadsOnAnotherDirectorsVM covers the read answers for
// the same cross-Director case. Under enforce has_vm and get_disks say the VM
// is not there, and under warn they answer as if the VM were ours.
func TestVMOwnershipGuard_ReadsOnAnotherDirectorsVM(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		mode string
		want bool
	}{
		{config.VMOwnershipGuardEnforce, false},
		{config.VMOwnershipGuardWarn, true},
	} {
		t.Run("has_vm/"+tc.mode, func(t *testing.T) {
			t.Parallel()
			deps := asDirector(withVMTags(testDepsFoundVM(101, nil, nil, nil, &mockAgentService{}), 101, "pve-node1", ownForeignTags), tc.mode)
			result, err := handlers.HandleHasVM(deps).Handle(context.Background(), marshalArgs("101"), ownRPC)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got, ok := result.(bool); !ok || got != tc.want {
				t.Errorf("has_vm = %v, want %v", result, tc.want)
			}
		})
	}

	t.Run("get_disks/enforce", func(t *testing.T) {
		t.Parallel()
		deps := asDirector(withVMTags(getDisksDeps(ownGetDisksQEMU()), 100, "pve-node1", ownForeignTags), config.VMOwnershipGuardEnforce)
		_, err := handlers.HandleGetDisks(deps).Handle(context.Background(), marshalArgs("100"), ownRPC)
		if !cpierrors.IsType(err, cpierrors.TypeVMNotFound) {
			t.Fatalf("get_disks error = %v, want VMNotFound", err)
		}
	})

	t.Run("get_disks/warn", func(t *testing.T) {
		t.Parallel()
		deps := asDirector(withVMTags(getDisksDeps(ownGetDisksQEMU()), 100, "pve-node1", ownForeignTags), config.VMOwnershipGuardWarn)
		result, err := handlers.HandleGetDisks(deps).Handle(context.Background(), marshalArgs("100"), ownRPC)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got, ok := result.([]string); !ok || len(got) != 1 {
			t.Errorf("get_disks = %v, want the one persistent disk listed under warn", result)
		}
	})
}

func ownGetDisksQEMU() *getDisksQEMUService {
	return ownGetDisksQEMUWithTags("")
}

// ownGetDisksQEMUWithTags is ownGetDisksQEMU with a config that carries tags,
// or no tags key at all when tags is empty.
func ownGetDisksQEMUWithTags(tags string) *getDisksQEMUService {
	return &getDisksQEMUService{
		configFn: func(context.Context, string, int) (map[string]any, error) {
			cfg := map[string]any{
				"scsi0": "local-lvm:vm-100-disk-0",
				"scsi1": "local-lvm:vm-100-disk-1",
			}
			if tags != "" {
				cfg["tags"] = tags
			}
			return cfg, nil
		},
	}
}

// TestVMOwnershipGuard_OwnVMProceeds proves the refusals above come from the
// owner tag and not from the fixture. The same calls on a VM tagged with the
// requesting Director's owner, or with no owner tag at all, go through.
func TestVMOwnershipGuard_OwnVMProceeds(t *testing.T) {
	t.Parallel()
	for name, tags := range map[string]string{"owner-tagged": ownOwnTags, "legacy": "bosh-cpi"} {
		t.Run("delete_vm/"+name, func(t *testing.T) {
			t.Parallel()
			f := &ownDeleteVMFixture{}
			deps := asDirector(withVMTags(f.deps(), 101, "pve-node1", tags), "")
			if _, err := handlers.HandleDeleteVM(deps).Handle(context.Background(), marshalArgs("101"), ownRPC); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(f.deleted) != 1 || f.deleted[0] != "101" {
				t.Errorf("DeleteQemu vmids = %v, want [101]", f.deleted)
			}
		})
		t.Run("has_vm/"+name, func(t *testing.T) {
			t.Parallel()
			deps := asDirector(withVMTags(testDepsFoundVM(101, nil, nil, nil, &mockAgentService{}), 101, "pve-node1", tags), "")
			result, err := handlers.HandleHasVM(deps).Handle(context.Background(), marshalArgs("101"), ownRPC)
			if err != nil || result != true {
				t.Errorf("has_vm = (%v, %v), want true", result, err)
			}
		})
	}
}

// TestVMOwnershipGuard_LegacyVMWithoutTheCPITag covers a VM created before the
// owner tag existed whose bosh-cpi tag an operator removed. Its Director
// metadata lines still mark it as ours. Without them it reads as a hand-made
// VM and is refused.
func TestVMOwnershipGuard_LegacyVMWithoutTheCPITag(t *testing.T) {
	t.Parallel()

	t.Run("notes keep it ours", func(t *testing.T) {
		t.Parallel()
		f := &ownDeleteVMFixture{cfgExtra: map[string]any{"tags": "web", "description": ownDirectorNotes}}
		deps := asDirector(withVMTags(f.deps(), 101, "pve-node1", "web"), "")
		if _, err := handlers.HandleDeleteVM(deps).Handle(context.Background(), marshalArgs("101"), ownRPC); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(f.deleted) != 1 {
			t.Errorf("DeleteQemu vmids = %v, want the legacy VM destroyed", f.deleted)
		}
	})

	t.Run("no notes is hand-made", func(t *testing.T) {
		t.Parallel()
		f := &ownDeleteVMFixture{cfgExtra: map[string]any{"tags": "web", "description": "built by hand"}}
		deps := asDirector(withVMTags(f.deps(), 101, "pve-node1", "web"), "")
		_, err := handlers.HandleDeleteVM(deps).Handle(context.Background(), marshalArgs("101"), ownRPC)
		requireForeignRefusal(t, err, "not-bosh")
		if f.touched() {
			t.Errorf("delete_vm touched a hand-made VM: deleted=%v", f.deleted)
		}
	})
}

// TestVMOwnershipGuard_TokenMismatchAnswersVMNotFound covers a stamped CID
// whose VM is gone and whose VMID now holds a different VM on the same
// cluster. The answer is the one the VM's absence would give, and the new VM
// is left alone, even under warn.
func TestVMOwnershipGuard_TokenMismatchAnswersVMNotFound(t *testing.T) {
	t.Parallel()
	const reusedTags = ownReusedTags

	t.Run("delete_vm", func(t *testing.T) {
		t.Parallel()
		f := &ownDeleteVMFixture{}
		deps := asDirector(withVMTags(f.deps(), 101, "pve-node1", reusedTags), "")
		_, err := handlers.HandleDeleteVM(deps).Handle(context.Background(), marshalArgs(stampedCID101), ownRPC)
		if !cpierrors.IsType(err, cpierrors.TypeVMNotFound) {
			t.Fatalf("delete_vm error = %v, want VMNotFound", err)
		}
		if f.touched() {
			t.Errorf("delete_vm touched the VM that reused the VMID: deleted=%v", f.deleted)
		}
	})

	t.Run("has_vm under warn", func(t *testing.T) {
		t.Parallel()
		deps := asDirector(withVMTags(testDepsFoundVM(101, nil, nil, nil, &mockAgentService{}), 101, "pve-node1", reusedTags), config.VMOwnershipGuardWarn)
		result, err := handlers.HandleHasVM(deps).Handle(context.Background(), marshalArgs(stampedCID101), ownRPC)
		if err != nil || result != false {
			t.Errorf("has_vm = (%v, %v), want false", result, err)
		}
	})
}

// TestVMOwnershipGuard_CreateDiskIgnoresAForeignHint covers create_disk's VM
// hint. A hint naming another Director's VM is dropped, so the disk's tags
// are never written onto that VM, and a hint naming our own VM still receives
// them.
func TestVMOwnershipGuard_CreateDiskIgnoresAForeignHint(t *testing.T) {
	t.Parallel()
	const hintVMID = 200
	const hintNode = "pve-node2"

	for _, tc := range []struct {
		name    string
		tags    string
		wantTag bool
	}{
		{"foreign", ownForeignTags, false},
		{"own", ownOwnTags, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var mu sync.Mutex
			var taggedVMIDs []string
			storageSvc := &mockStorageService{
				createVolumeFn: func(_ context.Context, _, _ string, _ int, _ string, vmid int, _ string) (string, error) {
					return fmt.Sprintf("%s:vm-%d-disk-0", storageName, vmid), nil
				},
			}
			deps := baseDepsForCreate(t, storageSvc, nil)
			client := deps.PVE.(*mockPVEClient)
			client.qemuSvc = &mockQEMUService{
				configFn: func(context.Context, string, int) (map[string]any, error) {
					return map[string]any{"name": "hinted"}, nil
				},
			}
			client.nodesSvc = &mockNodesService{
				updateQemuConfigFn: func(_ context.Context, _ string, vmid string, _ *nodes.UpdateQemuConfigParams) error {
					mu.Lock()
					taggedVMIDs = append(taggedVMIDs, vmid)
					mu.Unlock()
					return nil
				},
			}
			deps = asDirector(withVMTags(deps, hintVMID, hintNode, tc.tags), "")

			_, err := handlers.HandleCreateDisk(deps).Handle(context.Background(), []json.RawMessage{
				marshal(1024),
				marshal(map[string]any{"tags": map[string]string{"team": "data"}}),
				marshal(fmt.Sprint(hintVMID)),
			}, ownRPC)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			tagged := false
			for _, v := range taggedVMIDs {
				tagged = tagged || v == fmt.Sprint(hintVMID)
			}
			if tagged != tc.wantTag {
				t.Errorf("hinted VM tagged = %v (writes %v), want %v", tagged, taggedVMIDs, tc.wantTag)
			}
		})
	}
}

// TestVMOwnershipGuard_StragglerSweepLeavesForeignStragglers covers the
// fast-path sweep. It reaps the bosh-deleting VMs that carry this Director's
// owner tag, plus legacy stragglers that carry no owner tag. A full cap's
// worth of another Director's stragglers listed first must not use up the
// cap, and a parker and a hand-made VM carrying the deleting tag are left
// alone too.
func TestVMOwnershipGuard_StragglerSweepLeavesForeignStragglers(t *testing.T) {
	t.Parallel()

	const currentVMID = 730
	const ownStraggler = 740
	const parkerStraggler = 741
	const handMadeStraggler = 742
	const legacyStraggler = 743
	foreignStragglers := []int{750, 751, 752, 753, 754, 755}

	rowTags := map[int]string{
		ownStraggler:      ownOwnTags + ";bosh-deleting",
		parkerStraggler:   "bosh-cpi;bosh-parker;bosh-deleting",
		handMadeStraggler: "bosh-deleting",
		legacyStraggler:   "bosh-cpi;bosh-deleting",
	}
	for _, id := range foreignStragglers {
		rowTags[id] = ownForeignTags + ";bosh-deleting"
	}

	rows := cluster.ListResourcesResponse{}
	add := func(vmid int, tags string) {
		row := map[string]any{"vmid": vmid, "node": vmNode, "type": "qemu"}
		if tags != "" {
			row["tags"] = tags
		}
		raw, _ := json.Marshal(row)
		rows = append(rows, raw)
	}
	add(currentVMID, ownOwnTags)
	for _, id := range foreignStragglers {
		add(id, rowTags[id])
	}
	add(parkerStraggler, rowTags[parkerStraggler])
	add(handMadeStraggler, rowTags[handMadeStraggler])
	add(legacyStraggler, rowTags[legacyStraggler])
	add(ownStraggler, rowTags[ownStraggler])

	clusterSvc := &mockClusterSvc{
		listResourcesFn: func(context.Context, *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
			resp := append(cluster.ListResourcesResponse(nil), rows...)
			return &resp, nil
		},
	}
	qemuSvc := &mockQEMUService{
		stopFn: func(context.Context, string, int) (string, error) { return "", nil },
		configFn: func(_ context.Context, _ string, vmid int) (map[string]any, error) {
			if tags, ok := rowTags[vmid]; ok {
				return map[string]any{"tags": tags}, nil
			}
			return map[string]any{"tags": ownOwnTags}, nil
		},
	}
	var mu sync.Mutex
	destroyed := map[string]bool{}
	nodesSvc := &mockNodesService{
		deleteQemuFn: func(_ context.Context, _ string, vmid string, _ *nodes.DeleteQemuParams) (*nodes.DeleteQemuResponse, error) {
			mu.Lock()
			destroyed[vmid] = true
			mu.Unlock()
			return &nodes.DeleteQemuResponse{}, nil
		},
		updateQemuConfigFn: func(context.Context, string, string, *nodes.UpdateQemuConfigParams) error { return nil },
	}

	deps := testDepsWithCluster(qemuSvc, nodesSvc, &mockTasksService{}, &mockAgentService{}, &mockStorageService{}, clusterSvc)
	enabled := true
	deps.Config.FastPathDelete = &enabled
	deps = asDirector(deps, "")

	if _, err := handlers.HandleDeleteVM(deps).Handle(context.Background(), marshalArgs(fmt.Sprint(currentVMID)), ownRPC); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !destroyed[fmt.Sprint(currentVMID)] {
		t.Error("the VM being deleted was not destroyed")
	}
	if !destroyed[fmt.Sprint(ownStraggler)] {
		t.Error("our own straggler was not reaped; foreign stragglers must not count against the sweep cap")
	}
	// A legacy straggler carries no owner tag, so nothing tells it apart from
	// our own until it is stamped, and the plan keeps it in scope.
	if !destroyed[fmt.Sprint(legacyStraggler)] {
		t.Error("the legacy straggler, which carries bosh-cpi and no owner tag, was not reaped")
	}
	for _, id := range append([]int{parkerStraggler, handMadeStraggler}, foreignStragglers...) {
		if destroyed[fmt.Sprint(id)] {
			t.Errorf("the sweep destroyed VM %d (tags %q), which this Director does not own", id, rowTags[id])
		}
	}
}

// TestVMOwnershipGuard_ConfigOverridesAStaleScanRow covers a scan row that
// lags the VM it names. Our VM was destroyed, another Director's create_vm
// took the VMID, and the index row still shows our owner tag. A handler that
// read the VM's config judges the VM by that config, so the other Director's
// VM is refused, and our own VM behind a stale foreign row still proceeds.
func TestVMOwnershipGuard_ConfigOverridesAStaleScanRow(t *testing.T) {
	t.Parallel()

	t.Run("delete_vm/row ours, config foreign", func(t *testing.T) {
		t.Parallel()
		f := &ownDeleteVMFixture{cfgExtra: map[string]any{"tags": ownForeignTags}}
		deps := asDirector(withVMTags(f.deps(), 101, "pve-node1", ownOwnTags), "")
		_, err := handlers.HandleDeleteVM(deps).Handle(context.Background(), marshalArgs("101"), ownRPC)
		requireForeignRefusal(t, err, "other-director")
		if f.touched() {
			t.Errorf("delete_vm destroyed another Director's VM behind a stale row: stopped=%v deleted=%v removed=%v",
				f.stopped, f.deleted, f.removed)
		}
	})

	t.Run("delete_vm/row foreign, config ours", func(t *testing.T) {
		t.Parallel()
		f := &ownDeleteVMFixture{cfgExtra: map[string]any{"tags": ownOwnTags}}
		deps := asDirector(withVMTags(f.deps(), 101, "pve-node1", ownForeignTags), "")
		if _, err := handlers.HandleDeleteVM(deps).Handle(context.Background(), marshalArgs("101"), ownRPC); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(f.deleted) != 1 || f.deleted[0] != "101" {
			t.Errorf("DeleteQemu vmids = %v, want [101]", f.deleted)
		}
	})

	t.Run("get_disks/row ours, config foreign", func(t *testing.T) {
		t.Parallel()
		deps := asDirector(withVMTags(getDisksDeps(ownGetDisksQEMUWithTags(ownForeignTags)), 100, "pve-node1", ownOwnTags), "")
		_, err := handlers.HandleGetDisks(deps).Handle(context.Background(), marshalArgs("100"), ownRPC)
		if !cpierrors.IsType(err, cpierrors.TypeVMNotFound) {
			t.Fatalf("get_disks error = %v, want VMNotFound", err)
		}
	})

	t.Run("get_disks/row foreign, config ours", func(t *testing.T) {
		t.Parallel()
		deps := asDirector(withVMTags(getDisksDeps(ownGetDisksQEMUWithTags(ownOwnTags)), 100, "pve-node1", ownForeignTags), "")
		result, err := handlers.HandleGetDisks(deps).Handle(context.Background(), marshalArgs("100"), ownRPC)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got, ok := result.([]string); !ok || len(got) != 1 {
			t.Errorf("get_disks = %v, want the one persistent disk listed", result)
		}
	})

	t.Run("set_vm_metadata/row ours, config under the lock foreign", func(t *testing.T) {
		t.Parallel()
		var mu sync.Mutex
		written := false
		qemuSvc := &mockQEMUService{
			configFn: func(context.Context, string, int) (map[string]any, error) {
				return map[string]any{"tags": ownForeignTags}, nil
			},
		}
		nodesSvc := &mockNodesService{
			updateQemuConfigFn: func(context.Context, string, string, *nodes.UpdateQemuConfigParams) error {
				mu.Lock()
				written = true
				mu.Unlock()
				return nil
			},
		}
		deps := asDirector(withVMTags(testDepsFoundVM(101, qemuSvc, nodesSvc, nil, &mockAgentService{}), 101, "pve-node1", ownOwnTags), "")
		metadata := map[string]any{"director": "bosh", "deployment": "cf", "name": "router/6f3c"}
		_, err := handlers.HandleSetVMMetadata(deps).Handle(context.Background(), marshalArgs("101", metadata), ownRPC)
		requireForeignRefusal(t, err, "other-director")
		mu.Lock()
		defer mu.Unlock()
		if written {
			t.Error("set_vm_metadata rewrote another Director's VM behind a stale row")
		}
	})

	// set_vm_metadata runs right after create_vm, where a VMID our create_vm
	// took from another Director's destroyed VM can still show that
	// Director's tags in the index. The config decides, so our own VM gets
	// its metadata.
	t.Run("set_vm_metadata/row foreign, config ours", func(t *testing.T) {
		t.Parallel()
		var mu sync.Mutex
		written := false
		qemuSvc := &mockQEMUService{
			configFn: func(context.Context, string, int) (map[string]any, error) {
				return map[string]any{"tags": ownOwnTags}, nil
			},
		}
		nodesSvc := &mockNodesService{
			updateQemuConfigFn: func(context.Context, string, string, *nodes.UpdateQemuConfigParams) error {
				mu.Lock()
				written = true
				mu.Unlock()
				return nil
			},
		}
		deps := asDirector(withVMTags(testDepsFoundVM(101, qemuSvc, nodesSvc, nil, &mockAgentService{}), 101, "pve-node1", ownForeignTags), "")
		metadata := map[string]any{"director": "bosh", "deployment": "cf", "name": "router/6f3c"}
		if _, err := handlers.HandleSetVMMetadata(deps).Handle(context.Background(), marshalArgs("101", metadata), ownRPC); err != nil {
			t.Fatalf("set_vm_metadata refused our own VM behind a stale foreign row: %v", err)
		}
		mu.Lock()
		defer mu.Unlock()
		if !written {
			t.Error("set_vm_metadata wrote nothing to our own VM")
		}
	})

	t.Run("attach_disk/row foreign, config ours", func(t *testing.T) {
		t.Parallel()
		qemuSvc := &attachQEMUService{
			attachReturnDiskID: diskSlot,
			configCfg:          map[string]any{"tags": ownOwnTags},
		}
		deps := asDirector(withVMTags(attachDeps(qemuSvc, &captureAgent{}), 100, testNode, ownForeignTags), "")
		if _, err := handlers.HandleAttachDisk(deps).Handle(context.Background(), attachArgs(t, "100", diskCID), ownRPC); err != nil {
			t.Fatalf("attach_disk refused our own VM behind a stale foreign row: %v", err)
		}
		if qemuSvc.attachLastVolid == "" {
			t.Error("attach_disk did not attach the disk to our own VM")
		}
	})

	t.Run("attach_disk/row ours, config foreign", func(t *testing.T) {
		t.Parallel()
		qemuSvc := &attachQEMUService{
			attachReturnDiskID: diskSlot,
			configCfg:          map[string]any{"tags": ownForeignTags, "scsi0": testDiskCID},
		}
		deps := asDirector(withVMTags(attachDeps(qemuSvc, &captureAgent{}), 100, testNode, ownOwnTags), "")
		_, err := handlers.HandleAttachDisk(deps).Handle(context.Background(), attachArgs(t, "100", diskCID), ownRPC)
		requireForeignRefusal(t, err, "other-director")
		if qemuSvc.attachLastVolid != "" || len(qemuSvc.detachCalls) != 0 {
			t.Errorf("attach_disk wrote to another Director's VM behind a stale row: attached %q, detached %v",
				qemuSvc.attachLastVolid, qemuSvc.detachCalls)
		}
	})

	t.Run("detach_disk/row ours, config foreign", func(t *testing.T) {
		t.Parallel()
		qemuSvc := &detachQEMUService{
			configCfg: map[string]any{
				"tags":   ownForeignTags,
				"scsi0":  testDiskCID,
				diskSlot: "local-lvm:vm-9001-disk-0",
			},
		}
		// detachDeps places no cluster row, which would make the lookup probe
		// the config and see its tags, so the stale row is placed here.
		deps := asDirector(withVMRow(detachDeps(qemuSvc), 100, testNode, ownOwnTags, "qemu", false), "")
		_, err := handlers.HandleDetachDisk(deps).Handle(context.Background(), detachArgs(t, "100", diskCID), ownRPC)
		requireForeignRefusal(t, err, "other-director")
		if qemuSvc.detachCalled {
			t.Errorf("detach_disk unplugged %q from another Director's VM behind a stale row", qemuSvc.detachedDiskID)
		}
		if _, ok := qemuSvc.configCfg[diskSlot]; !ok {
			t.Error("the disk's slot is gone from another Director's VM")
		}
	})
}

// TestVMOwnershipGuard_TokenMismatchOnTheMutations covers the rest of the
// token-mismatch column. Each handler that acts on a VM answers VMNotFound,
// as the VM's absence would, and delete_snapshot answers success. None of
// them touches the VM that took over the VMID.
func TestVMOwnershipGuard_TokenMismatchOnTheMutations(t *testing.T) {
	t.Parallel()

	requireVMNotFound := func(t *testing.T, op string, err error) {
		t.Helper()
		if !cpierrors.IsType(err, cpierrors.TypeVMNotFound) {
			t.Fatalf("%s error = %v, want VMNotFound", op, err)
		}
	}

	t.Run("reboot_vm", func(t *testing.T) {
		t.Parallel()
		rebooted := false
		qemuSvc := &mockQEMUService{
			statusFn: func(context.Context, string, int) (map[string]any, error) {
				return map[string]any{"status": "running"}, nil
			},
		}
		nodesSvc := &mockNodesService{
			createQemuStatusRebootFn: func(context.Context, string, string, *nodes.CreateQemuStatusRebootParams) (*nodes.CreateQemuStatusRebootResponse, error) {
				rebooted = true
				return rebootRawUPID("UPID:node:reboot-task"), nil
			},
		}
		deps := asDirector(withVMTags(testDepsReboot(qemuSvc, nodesSvc, &mockTasksService{}, "soft"), 101, "pve-node1", ownReusedTags), "")
		_, err := handlers.HandleRebootVM(deps).Handle(context.Background(), marshalArgs(stampedCID101), ownRPC)
		requireVMNotFound(t, "reboot_vm", err)
		if rebooted {
			t.Error("reboot_vm rebooted the VM that reused the VMID")
		}
	})

	t.Run("set_vm_metadata", func(t *testing.T) {
		t.Parallel()
		written := false
		nodesSvc := &mockNodesService{
			updateQemuConfigFn: func(context.Context, string, string, *nodes.UpdateQemuConfigParams) error {
				written = true
				return nil
			},
		}
		deps := asDirector(withVMTags(testDepsFoundVM(101, nil, nodesSvc, nil, &mockAgentService{}), 101, "pve-node1", ownReusedTags), "")
		metadata := map[string]any{"director": "bosh", "deployment": "cf", "name": "router/6f3c"}
		_, err := handlers.HandleSetVMMetadata(deps).Handle(context.Background(), marshalArgs(stampedCID101, metadata), ownRPC)
		requireVMNotFound(t, "set_vm_metadata", err)
		if written {
			t.Error("set_vm_metadata rewrote the VM that reused the VMID")
		}
	})

	t.Run("attach_disk", func(t *testing.T) {
		t.Parallel()
		qemuSvc := &attachQEMUService{
			attachReturnDiskID: diskSlot,
			configCfg:          map[string]any{"scsi0": testDiskCID},
		}
		deps := asDirector(withVMTags(attachDeps(qemuSvc, &captureAgent{}), 100, testNode, ownReusedTags), "")
		_, err := handlers.HandleAttachDisk(deps).Handle(context.Background(), attachArgs(t, stampedCID100, diskCID), ownRPC)
		requireVMNotFound(t, "attach_disk", err)
		if qemuSvc.attachLastVolid != "" {
			t.Errorf("attach_disk plugged %q into the VM that reused the VMID", qemuSvc.attachLastVolid)
		}
	})

	t.Run("detach_disk", func(t *testing.T) {
		t.Parallel()
		qemuSvc := &detachQEMUService{
			configCfg: map[string]any{
				"scsi0":  testDiskCID,
				diskSlot: "local-lvm:vm-9001-disk-0",
			},
		}
		deps := asDirector(withVMTags(detachDeps(qemuSvc), 100, testNode, ownReusedTags), "")
		_, err := handlers.HandleDetachDisk(deps).Handle(context.Background(), detachArgs(t, stampedCID100, diskCID), ownRPC)
		requireVMNotFound(t, "detach_disk", err)
		if qemuSvc.detachCalled {
			t.Error("detach_disk unplugged a disk from the VM that reused the VMID")
		}
	})

	t.Run("delete_snapshot", func(t *testing.T) {
		t.Parallel()
		deleted := false
		qemuSvc := &delSnapQEMUService{
			deleteSnapshotFn: func(context.Context, string, int, string) error {
				deleted = true
				return nil
			},
		}
		deps := asDirector(withVMTags(delSnapDeps(qemuSvc), 100, testNode, ownReusedTags), "")
		result, err := handlers.HandleDeleteSnapshot(deps).Handle(context.Background(), marshalArgs(stampedCID100+":bosh-1234567890-abcd1234"), ownRPC)
		if err != nil || result != nil {
			t.Fatalf("delete_snapshot = (%v, %v), want success with nothing done", result, err)
		}
		if deleted {
			t.Error("delete_snapshot removed a snapshot from the VM that reused the VMID")
		}
	})
}

// TestVMOwnershipGuard_TokenMismatchOnTheReads covers the reads in the
// token-mismatch column. get_disks answers VMNotFound in both modes, because
// warn relaxes only a foreign verdict and the CID's VM is gone, and
// create_disk drops a hint that names the VM which took over the VMID.
func TestVMOwnershipGuard_TokenMismatchOnTheReads(t *testing.T) {
	t.Parallel()

	for _, mode := range guardModes {
		t.Run("get_disks/"+mode, func(t *testing.T) {
			t.Parallel()
			deps := asDirector(withVMTags(getDisksDeps(ownGetDisksQEMU()), 100, "pve-node1", ownReusedTags), mode)
			_, err := handlers.HandleGetDisks(deps).Handle(context.Background(), marshalArgs(stampedCID100), ownRPC)
			if !cpierrors.IsType(err, cpierrors.TypeVMNotFound) {
				t.Fatalf("get_disks error = %v, want VMNotFound", err)
			}
		})
	}

	t.Run("create_disk", func(t *testing.T) {
		t.Parallel()
		const hintVMID = 200
		var mu sync.Mutex
		var taggedVMIDs []string
		storageSvc := &mockStorageService{
			createVolumeFn: func(_ context.Context, _, _ string, _ int, _ string, vmid int, _ string) (string, error) {
				return fmt.Sprintf("%s:vm-%d-disk-0", storageName, vmid), nil
			},
		}
		deps := baseDepsForCreate(t, storageSvc, nil)
		client := deps.PVE.(*mockPVEClient)
		client.qemuSvc = &mockQEMUService{
			configFn: func(context.Context, string, int) (map[string]any, error) {
				return map[string]any{"name": "hinted"}, nil
			},
		}
		client.nodesSvc = &mockNodesService{
			updateQemuConfigFn: func(_ context.Context, _ string, vmid string, _ *nodes.UpdateQemuConfigParams) error {
				mu.Lock()
				taggedVMIDs = append(taggedVMIDs, vmid)
				mu.Unlock()
				return nil
			},
		}
		deps = asDirector(withVMTags(deps, hintVMID, "pve-node2", ownReusedTags), "")
		_, err := handlers.HandleCreateDisk(deps).Handle(context.Background(), []json.RawMessage{
			marshal(1024),
			marshal(map[string]any{"tags": map[string]string{"team": "data"}}),
			marshal(fmt.Sprintf("%d-0a1b2c3d-4e5f6a7b", hintVMID)),
		}, ownRPC)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		mu.Lock()
		defer mu.Unlock()
		for _, v := range taggedVMIDs {
			if v == fmt.Sprint(hintVMID) {
				t.Errorf("create_disk tagged the VM that reused the hinted VMID (writes %v)", taggedVMIDs)
			}
		}
	})
}

// TestVMOwnershipGuard_TemplatesAndContainersAtTheHandlers reaches a stemcell
// template and a container through the handlers, not only through the
// predicate. A template flag in the config refuses delete_vm, and a
// container row answers has_vm false and refuses reboot_vm.
func TestVMOwnershipGuard_TemplatesAndContainersAtTheHandlers(t *testing.T) {
	t.Parallel()

	t.Run("delete_vm/template", func(t *testing.T) {
		t.Parallel()
		f := &ownDeleteVMFixture{cfgExtra: map[string]any{"tags": "bosh-cpi", "template": 1}}
		deps := asDirector(withVMRow(f.deps(), 101, "pve-node1", "bosh-cpi", "qemu", true), "")
		_, err := handlers.HandleDeleteVM(deps).Handle(context.Background(), marshalArgs("101"), ownRPC)
		requireForeignRefusal(t, err, "template")
		if f.touched() {
			t.Errorf("delete_vm touched a stemcell template: deleted=%v", f.deleted)
		}
	})

	t.Run("has_vm/container", func(t *testing.T) {
		t.Parallel()
		deps := asDirector(withVMRow(testDepsFoundVM(101, nil, nil, nil, &mockAgentService{}), 101, "pve-node1", "bosh-cpi", "lxc", false), "")
		result, err := handlers.HandleHasVM(deps).Handle(context.Background(), marshalArgs("101"), ownRPC)
		if err != nil || result != false {
			t.Errorf("has_vm = (%v, %v), want false for a container", result, err)
		}
	})

	t.Run("reboot_vm/container", func(t *testing.T) {
		t.Parallel()
		rebooted := false
		nodesSvc := &mockNodesService{
			createQemuStatusRebootFn: func(context.Context, string, string, *nodes.CreateQemuStatusRebootParams) (*nodes.CreateQemuStatusRebootResponse, error) {
				rebooted = true
				return rebootRawUPID("UPID:node:reboot-task"), nil
			},
		}
		qemuSvc := &mockQEMUService{
			statusFn: func(context.Context, string, int) (map[string]any, error) {
				return map[string]any{"status": "running"}, nil
			},
		}
		deps := asDirector(withVMRow(testDepsReboot(qemuSvc, nodesSvc, &mockTasksService{}, "soft"), 101, "pve-node1", "bosh-cpi", "lxc", false), "")
		_, err := handlers.HandleRebootVM(deps).Handle(context.Background(), marshalArgs("101"), ownRPC)
		requireForeignRefusal(t, err, "not-bosh")
		if rebooted {
			t.Error("reboot_vm rebooted a container")
		}
	})
}

// TestVMOwnershipGuard_UnreadableConfigStopsTheWrite pins the fail-closed
// half of the config judgment at the handlers. The row says the VM is ours,
// and the config read that must confirm it fails, so each handler answers
// retriable and writes nothing. attach_disk is also checked on a parked disk,
// because its holder guard would unpark the disk right after the ownership
// guard.
func TestVMOwnershipGuard_UnreadableConfigStopsTheWrite(t *testing.T) {
	t.Parallel()
	readErr := errors.New("connection reset by peer")

	requireRetriable := func(t *testing.T, op string, err error) {
		t.Helper()
		if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
			t.Fatalf("%s error = %v, want a retriable error", op, err)
		}
	}

	t.Run("attach_disk", func(t *testing.T) {
		t.Parallel()
		qemuSvc := &attachQEMUService{
			attachReturnDiskID: diskSlot,
			configCfg:          map[string]any{"tags": ownOwnTags},
			configErrOnCall:    map[int]error{1: readErr},
		}
		deps := asDirector(withVMTags(attachDeps(qemuSvc, &captureAgent{}), 100, testNode, ownOwnTags), "")
		_, err := handlers.HandleAttachDisk(deps).Handle(context.Background(), attachArgs(t, "100", diskCID), ownRPC)
		requireRetriable(t, "attach_disk", err)
		if qemuSvc.attachLastVolid != "" || len(qemuSvc.detachCalls) != 0 {
			t.Errorf("attach_disk wrote to a VM it could not judge: attached %q, detached %v",
				qemuSvc.attachLastVolid, qemuSvc.detachCalls)
		}
	})

	t.Run("attach_disk/parked disk is not unparked", func(t *testing.T) {
		t.Parallel()
		parkerVMCfg := map[string]any{parkerSlot: parkedVolid, "tags": "bosh-parker", "name": "bosh-parker-90000"}
		qemuSvc := &attachQEMUService{
			attachReturnDiskID: "scsi1",
			// The calls after the failed guard read are the ones a parked
			// attach makes, so a guard that let the failure through would
			// unpark the disk and attach it.
			configCfgs: []map[string]any{
				{}, parkerVMCfg, parkerVMCfg, parkerVMCfg, parkerVMCfg, parkerVMCfg, {}, {"scsi1": parkedVolid},
			},
			configErrOnCall: map[int]error{1: readErr},
		}
		deps := handlers.Deps{
			Config: parkedCfg(),
			PVE:    &mockPVEClient{qemuSvc: qemuSvc, clusterSvc: parkerClusterSvc()},
			Agent:  &captureAgent{},
			Logger: log.NewNopLogger(),
		}
		_, err := handlers.HandleAttachDisk(deps).Handle(context.Background(), attachArgs(t, "100", parkedVolid), ownRPC)
		requireRetriable(t, "attach_disk", err)
		if len(qemuSvc.detachCalls) != 0 {
			t.Errorf("attach_disk unparked the disk (detach calls %v) toward a VM it could not judge", qemuSvc.detachCalls)
		}
		if qemuSvc.attachLastVolid != "" {
			t.Errorf("attach_disk attached %q to a VM it could not judge", qemuSvc.attachLastVolid)
		}
	})

	t.Run("detach_disk", func(t *testing.T) {
		t.Parallel()
		qemuSvc := &detachQEMUService{configErr: readErr}
		deps := asDirector(withVMRow(detachDeps(qemuSvc), 100, testNode, ownOwnTags, "qemu", false), "")
		_, err := handlers.HandleDetachDisk(deps).Handle(context.Background(), detachArgs(t, "100", diskCID), ownRPC)
		requireRetriable(t, "detach_disk", err)
		if qemuSvc.detachCalled {
			t.Errorf("detach_disk unplugged %q from a VM it could not judge", qemuSvc.detachedDiskID)
		}
	})
}

// TestVMOwnershipGuard_ProvenAbsenceAnswers covers a row for a VM that is
// gone. The guard's config read answers not-found, the config probe of every
// member proves the VM exists nowhere, and each handler gives the answer it
// gives a VM it cannot find rather than a retriable error. The row is
// untagged or foreign, because those are the rows the guard reads behind.
func TestVMOwnershipGuard_ProvenAbsenceAnswers(t *testing.T) {
	t.Parallel()
	notFound := func(context.Context, string, int) (map[string]any, error) {
		return nil, sdkerrors.ErrNotFound
	}

	for _, mode := range guardModes {
		t.Run("has_vm/"+mode, func(t *testing.T) {
			t.Parallel()
			qemuSvc := &mockQEMUService{configFn: notFound}
			deps := asDirector(withVMTags(testDepsFoundVM(101, qemuSvc, nil, nil, &mockAgentService{}), 101, "pve-node1", ownForeignTags), mode)
			result, err := handlers.HandleHasVM(deps).Handle(context.Background(), marshalArgs("101"), ownRPC)
			if err != nil || result != false {
				t.Fatalf("has_vm = (%v, %v), want false", result, err)
			}
		})
	}

	t.Run("has_vm/a member's probe fails", func(t *testing.T) {
		t.Parallel()
		qemuSvc := &mockQEMUService{configFn: func(_ context.Context, node string, _ int) (map[string]any, error) {
			if node == "pve-node1" {
				return nil, sdkerrors.ErrNotFound
			}
			return nil, errors.New("connection reset by peer")
		}}
		deps := asDirector(withVMTags(testDepsFoundVM(101, qemuSvc, nil, nil, &mockAgentService{}), 101, "pve-node1", ownForeignTags), "")
		deps.PVE.(*mockPVEClient).clusterSvc.(*mockClusterSvc).listConfigNodesFn = func(context.Context) (*cluster.ListConfigNodesResponse, error) {
			return &cluster.ListConfigNodesResponse{json.RawMessage(`{"name":"pve-node1"}`), json.RawMessage(`{"name":"pve-node2"}`)}, nil
		}
		result, err := handlers.HandleHasVM(deps).Handle(context.Background(), marshalArgs("101"), ownRPC)
		if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
			t.Fatalf("has_vm = (%v, %v), want a retriable error while absence is unproven", result, err)
		}
	})

	t.Run("has_vm/an offline member leaves absence unproven", func(t *testing.T) {
		t.Parallel()
		// pve-node2 is where the VM may have migrated before it lost power.
		// The quorate cluster reports it offline, so no probe can answer for
		// it, and has_vm must not answer false for a VM that may be there.
		qemuSvc := &mockQEMUService{configFn: func(_ context.Context, node string, _ int) (map[string]any, error) {
			if node == "pve-node1" {
				return nil, sdkerrors.ErrNotFound
			}
			return nil, errors.New("offline member must not be probed")
		}}
		deps := asDirector(withVMTags(testDepsFoundVM(101, qemuSvc, nil, nil, &mockAgentService{}), 101, "pve-node1", ownForeignTags), "")
		clusterSvc := deps.PVE.(*mockPVEClient).clusterSvc.(*mockClusterSvc)
		clusterSvc.listConfigNodesFn = func(context.Context) (*cluster.ListConfigNodesResponse, error) {
			return &cluster.ListConfigNodesResponse{json.RawMessage(`{"name":"pve-node1"}`), json.RawMessage(`{"name":"pve-node2"}`)}, nil
		}
		clusterSvc.listStatusFn = func(context.Context) (*cluster.ListStatusResponse, error) {
			return &cluster.ListStatusResponse{
				json.RawMessage(`{"type":"cluster","name":"lab","quorate":1}`),
				json.RawMessage(`{"type":"node","name":"pve-node1","online":1}`),
				json.RawMessage(`{"type":"node","name":"pve-node2","online":0}`),
			}, nil
		}
		result, err := handlers.HandleHasVM(deps).Handle(context.Background(), marshalArgs("101"), ownRPC)
		if result == false && err == nil {
			t.Fatal("has_vm answered false while a member the cluster reports offline was never probed")
		}
		if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
			t.Fatalf("has_vm = (%v, %v), want a retriable error while absence is unproven", result, err)
		}
	})

	t.Run("delete_snapshot", func(t *testing.T) {
		t.Parallel()
		deleted := false
		qemuSvc := &delSnapQEMUService{
			configErr: sdkerrors.ErrNotFound,
			deleteSnapshotFn: func(context.Context, string, int, string) error {
				deleted = true
				return nil
			},
		}
		deps := asDirector(withVMTags(delSnapDeps(qemuSvc), 100, testNode, ownForeignTags), "")
		result, err := handlers.HandleDeleteSnapshot(deps).Handle(context.Background(), marshalArgs("100:bosh-1234567890-abcd1234"), ownRPC)
		if err != nil || result != nil {
			t.Fatalf("delete_snapshot = (%v, %v), want success", result, err)
		}
		if deleted {
			t.Error("delete_snapshot called DeleteSnapshot on a VM that is gone")
		}
	})

	t.Run("reboot_vm", func(t *testing.T) {
		t.Parallel()
		rebooted := false
		qemuSvc := &mockQEMUService{configFn: notFound}
		nodesSvc := &mockNodesService{
			createQemuStatusRebootFn: func(context.Context, string, string, *nodes.CreateQemuStatusRebootParams) (*nodes.CreateQemuStatusRebootResponse, error) {
				rebooted = true
				return rebootRawUPID("UPID:node:reboot-task"), nil
			},
		}
		deps := asDirector(withVMTags(testDepsReboot(qemuSvc, nodesSvc, &mockTasksService{}, "soft"), 101, "pve-node1", ownForeignTags), "")
		_, err := handlers.HandleRebootVM(deps).Handle(context.Background(), marshalArgs("101"), ownRPC)
		if !cpierrors.IsType(err, cpierrors.TypeVMNotFound) {
			t.Fatalf("reboot_vm error = %v, want VMNotFound", err)
		}
		if rebooted {
			t.Error("reboot_vm rebooted a VM that is gone")
		}
	})

	t.Run("detach_disk", func(t *testing.T) {
		t.Parallel()
		qemuSvc := &detachQEMUService{configErr: sdkerrors.ErrNotFound}
		deps := asDirector(withVMRow(detachDeps(qemuSvc), 100, testNode, ownOwnTags, "qemu", false), "")
		if _, err := handlers.HandleDetachDisk(deps).Handle(context.Background(), detachArgs(t, "100", diskCID), ownRPC); err != nil {
			t.Fatalf("detach_disk error = %v, want success as already detached", err)
		}
		if qemuSvc.detachCalled {
			t.Error("detach_disk deleted a slot on a VM that is gone")
		}
	})
}

// TestVMOwnershipGuard_ReusedRowOwnConfigHasVM is the has_vm half of the
// stale token-mismatch row. The row still carries the token of the VM the
// VMID held before, and the config carries the CID's own token, so the config
// decides and our live VM is reported present.
func TestVMOwnershipGuard_ReusedRowOwnConfigHasVM(t *testing.T) {
	t.Parallel()
	qemuSvc := &mockQEMUService{configFn: func(context.Context, string, int) (map[string]any, error) {
		return map[string]any{"tags": ownOwnTags + ";bosh-cluster--0a1b2c3d;bosh-cid--4e5f6a7b"}, nil
	}}
	deps := asDirector(withVMTags(testDepsFoundVM(101, qemuSvc, nil, nil, &mockAgentService{}), 101, "pve-node1", ownReusedTags), "")
	result, err := handlers.HandleHasVM(deps).Handle(context.Background(), marshalArgs(stampedCID101), ownRPC)
	if err != nil || result != true {
		t.Fatalf("has_vm = (%v, %v), want true for our own VM behind a stale reused row", result, err)
	}
}

// TestVMOwnershipGuard_ReadCountsBeforeTheWrite pins what the guard costs.
// detach_disk judges the read it takes the slot from, so the guard and the
// slot resolution share one config read, and the slot-delete helper's own
// read just before its write is the only other one. attach_disk on a VM the lookup found through
// its config probe judges that probe's answer, so the probe and the slot
// read are the only reads before the attach.
func TestVMOwnershipGuard_ReadCountsBeforeTheWrite(t *testing.T) {
	t.Parallel()

	t.Run("detach_disk", func(t *testing.T) {
		t.Parallel()
		qemuSvc := &detachQEMUService{configCfg: map[string]any{
			"tags":   ownOwnTags,
			diskSlot: diskCID,
		}}
		deps := asDirector(withVMRow(detachDeps(qemuSvc), 100, testNode, ownOwnTags, "qemu", false), "")
		if _, err := handlers.HandleDetachDisk(deps).Handle(context.Background(), detachArgs(t, "100", diskCID), ownRPC); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !qemuSvc.detachCalled || qemuSvc.detachedDiskID != diskSlot {
			t.Fatalf("detach_disk detached %q (called %v), want %q", qemuSvc.detachedDiskID, qemuSvc.detachCalled, diskSlot)
		}
		if qemuSvc.configCallsAtDetach != 2 {
			t.Errorf("config reads before the slot delete = %d, want 2 (the judged slot read and the delete helper's own read)",
				qemuSvc.configCallsAtDetach)
		}
	})

	t.Run("attach_disk/index miss", func(t *testing.T) {
		t.Parallel()
		qemuSvc := &attachQEMUService{
			attachReturnDiskID: "scsi1",
			configCfgs: []map[string]any{
				{"tags": ownOwnTags},                   // the lookup's config probe
				{"tags": ownOwnTags},                   // the slot read
				{"tags": ownOwnTags, "scsi1": diskCID}, // after the attach
			},
		}
		deps := handlers.Deps{
			Config: &config.CPIConfig{Node: testNode, VMDiskFormat: "qcow2"},
			PVE:    &mockPVEClient{qemuSvc: qemuSvc, clusterSvc: emptyClusterSvc()},
			Agent:  &captureAgent{},
			Logger: log.NewNopLogger(),
		}
		if _, err := handlers.HandleAttachDisk(deps).Handle(context.Background(), attachArgs(t, "100", diskCID), ownRPC); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if qemuSvc.configCallsAtAttach != 2 {
			t.Errorf("config reads before the attach = %d, want 2 (the probe and the slot read)", qemuSvc.configCallsAtAttach)
		}
	})
}

// TestVMOwnershipGuard_AttachJudgesTheSlotRead covers the second judgment in
// attach_disk. The early guard sees our VM, and by the slot read the VMID
// holds another Director's VM, so the attach is refused before any write.
func TestVMOwnershipGuard_AttachJudgesTheSlotRead(t *testing.T) {
	t.Parallel()
	qemuSvc := &attachQEMUService{
		attachReturnDiskID: diskSlot,
		configCfgs: []map[string]any{
			{"tags": ownOwnTags},                           // the early guard
			{"tags": ownForeignTags, "scsi0": testDiskCID}, // the slot read
		},
	}
	deps := asDirector(withVMTags(attachDeps(qemuSvc, &captureAgent{}), 100, testNode, ownOwnTags), "")
	_, err := handlers.HandleAttachDisk(deps).Handle(context.Background(), attachArgs(t, "100", diskCID), ownRPC)
	requireForeignRefusal(t, err, "other-director")
	if strings.Contains(err.Error(), "slot selection") {
		t.Errorf("refusal %q was reported as a slot-selection failure", err)
	}
	if qemuSvc.attachLastVolid != "" || len(qemuSvc.detachCalls) != 0 {
		t.Errorf("attach_disk wrote to another Director's VM: attached %q, detached %v",
			qemuSvc.attachLastVolid, qemuSvc.detachCalls)
	}
}

// detachNodeQEMU serves each node's own config, answers not-found on a node
// that holds no config, and records the node of every QEMU call and slot
// delete, so a detach test can tell which node it read and wrote.
type detachNodeQEMU struct {
	detachQEMUService
	mu     sync.Mutex
	byNode map[string]map[string]any
	calls  []string
}

func (m *detachNodeQEMU) record(op, node string) {
	m.calls = append(m.calls, op+"@"+node)
}

func (m *detachNodeQEMU) Config(_ context.Context, node string, _ int) (map[string]any, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.record("config", node)
	cfg, ok := m.byNode[node]
	if !ok {
		return nil, sdkerrors.ErrNotFound
	}
	out := make(map[string]any, len(cfg))
	for k, v := range cfg {
		out[k] = v
	}
	return out, nil
}

func (m *detachNodeQEMU) DetachDisk(_ context.Context, node string, _ int, diskID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.record("detach "+diskID, node)
	delete(m.byNode[node], diskID)
	return nil
}

func (m *detachNodeQEMU) deleteConfigKey(node string, _ int, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.record("delete "+key, node)
	cfg, ok := m.byNode[node]
	if !ok {
		return sdkerrors.ErrNotFound
	}
	delete(cfg, key)
	return nil
}

func (m *detachNodeQEMU) ListSnapshots(_ context.Context, node string, _ int) ([]map[string]any, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.record("snapshots", node)
	return nil, nil
}

func (m *detachNodeQEMU) recorded() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.calls...)
}

// TestVMOwnershipGuard_DetachActsOnTheNodeItJudged covers a shared-backend VM
// that moved inside the index lag. The row still names n1, whose read answers
// not-found, and the re-locate finds our VM on n2 with the disk in its slot.
// The detach must unplug the slot on n2, the node it judged. A detach sent
// back to the row's node would find no VM there and report the disk already
// detached while it stays plugged in on n2.
func TestVMOwnershipGuard_DetachActsOnTheNodeItJudged(t *testing.T) {
	t.Parallel()
	qemuSvc := &detachNodeQEMU{byNode: map[string]map[string]any{
		"n2": {"tags": ownOwnTags, diskSlot: diskCID},
	}}
	deps := withVMRow(detachDeps(qemuSvc), 100, "n1", ownOwnTags, "qemu", false)
	deps.PVE.(*mockPVEClient).clusterSvc.(*mockClusterSvc).listConfigNodesFn = func(context.Context) (*cluster.ListConfigNodesResponse, error) {
		return &cluster.ListConfigNodesResponse{json.RawMessage(`{"name":"n1"}`), json.RawMessage(`{"name":"n2"}`)}, nil
	}
	if _, err := handlers.HandleDetachDisk(asDirector(deps, "")).Handle(context.Background(), detachArgs(t, "100", diskCID), ownRPC); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	calls := qemuSvc.recorded()
	unplugged := false
	for _, c := range calls {
		if c == "delete "+diskSlot+"@n2" || c == "detach "+diskSlot+"@n2" {
			unplugged = true
		}
	}
	if !unplugged {
		t.Errorf("detach_disk never removed %s on n2, where the VM it judged lives; calls %v", diskSlot, calls)
	}
	if _, still := qemuSvc.byNode["n2"][diskSlot]; still {
		t.Errorf("%s is still in n2's config after the detach", diskSlot)
	}
	// The guard's first read on n1 and the re-locate's probes of n1 are the
	// only calls n1 may see; the slot read, the snapshot guard, and every
	// write belong to n2.
	for _, c := range calls {
		if strings.HasSuffix(c, "@n1") && c != "config@n1" {
			t.Errorf("detach_disk acted on n1, the row's stale node: %s (calls %v)", c, calls)
		}
	}
}
