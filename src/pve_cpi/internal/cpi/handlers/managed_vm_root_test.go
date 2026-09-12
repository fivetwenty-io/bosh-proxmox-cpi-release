package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/tasks"
)

type managedRootClient struct {
	pve.Client
	q qemu.Service
	n nodes.Service
}

func (c managedRootClient) QEMU() qemu.Service   { return c.q }
func (c managedRootClient) Nodes() nodes.Service { return c.n }
func (c managedRootClient) Tasks() tasks.Service { return &cloneTasksService{} }

type managedRootNodes struct {
	nodes.Service
	clone  func(string, string, *nodes.CreateQemuCloneParams) (*json.RawMessage, error)
	update func(string, string, *nodes.UpdateQemuConfigParams) error
}

func (n *managedRootNodes) CreateQemuClone(_ context.Context, node, id string, p *nodes.CreateQemuCloneParams) (*nodes.CreateQemuCloneResponse, error) {
	return n.clone(node, id, p)
}
func (n *managedRootNodes) UpdateQemuConfig(_ context.Context, node, id string, p *nodes.UpdateQemuConfigParams) error {
	return n.update(node, id, p)
}
func TestManagedVMRootFrozenMechanism(t *testing.T) {
	for _, mechanism := range []string{"import", "full_clone", "linked_clone"} {
		t.Run(mechanism, func(t *testing.T) {
			checkManagedVMRootFrozenMechanism(t, mechanism)
		})
	}
}
func TestManagedVMRootFailureNeverFallsBack(t *testing.T) {
	marker, _ := pve.FormatStorageAllocationMarker(pve.StorageAllocationMarker{Version: 1, Kind: "vm", Namespace: "ns", AllocationID: "12345678-1234-4234-8234-123456789abc", AgentSHA256: strings.Repeat("a", 64)})
	calls := 0
	n := &managedRootNodes{clone: func(_ string, _ string, p *nodes.CreateQemuCloneParams) (*json.RawMessage, error) {
		if p.Description == nil || *p.Description != marker {
			t.Fatal("response-loss clone lacks submitted provenance")
		}
		calls++
		return nil, errors.New("transport unknown")
	}}
	deps := Deps{PVE: managedRootClient{n: n}, Config: &config.CPIConfig{}, Logger: log.NewNopLogger()}
	err := createManagedVMRoot(context.Background(), deps, &createVMParsedArgs{}, &createVMShape{node: "n", vmStorage: "s"}, StoragePlanTarget{Role: storageRoleRoot, Node: "n", StorageID: "s", Mechanism: "full_clone", Source: &StorageRootSource{Node: "n", TemplateVMID: 900}}, 101, marker)
	if err == nil || calls != 1 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
	// Missing QEMU and delete services panic if an import retry or cleanup occurs.
}

func TestManagedCloneGuardRequiresSubmittedProvenance(t *testing.T) {
	marker := "full-allocation-marker"
	full := true
	storage := "dest"
	target := StoragePlanTarget{Role: storageRoleRoot, StorageID: storage, Mechanism: "full_clone", Source: &StorageRootSource{Node: "source", TemplateVMID: 900}}
	m := &managedVMAllocation{vmid: 101, marker: marker, prepared: &managedVMPlan{plan: &StorageAllocationPlan{Targets: []StoragePlanTarget{target}}}}
	for _, value := range []*string{nil, new(string), &marker} {
		params := &nodes.CreateQemuCloneParams{Newid: 101, Full: &full, Storage: &storage, Description: value}
		call := ManagedAllocationMutation{Service: "Nodes", Method: "CreateQemuClone", Args: map[string]any{"node": "source", "vmid": "900", "params": params}}
		err := m.validateStorageMutationTarget(call, storageRoleRoot)
		if (err == nil) != (value == &marker) {
			t.Fatalf("provenance guard: %v", err)
		}
	}
}

func checkManagedVMRootFrozenMechanism(t *testing.T, mechanism string) {
	t.Helper()
	marker, err := pve.FormatStorageAllocationMarker(pve.StorageAllocationMarker{Version: 1, Kind: "vm", Namespace: "ns", AllocationID: "12345678-1234-4234-8234-123456789abc", AgentSHA256: strings.Repeat("a", 64)})
	if err != nil {
		t.Fatal(err)
	}
	writes := []string{}
	q := &guardTestQEMU{createFn: func(_ context.Context, node string, p map[string]any) (string, error) {
		writes = append(writes, "import")
		if node != "target" || p["description"] != marker || !strings.Contains(p["virtio0"].(string), "import-from=source:import/frozen.qcow2") {
			t.Fatal("lost frozen import target or provenance")
		}
		return "UPID:target:1", nil
	}}
	n := &managedRootNodes{clone: func(node, id string, p *nodes.CreateQemuCloneParams) (*json.RawMessage, error) {
		writes = append(writes, "clone")
		if node != "source-node" || id != "900" || p.Newid != 101 || p.Target == nil || *p.Target != "target" || p.Full == nil || *p.Full != (mechanism == "full_clone") {
			t.Fatal("wrong frozen clone")
		}
		if p.Description == nil || *p.Description != marker {
			t.Fatal("clone submission lacks full allocation provenance")
		}
		if mechanism == "linked_clone" && (p.Storage != nil || p.Format != nil) {
			t.Fatal("linked clone got full-clone storage fields")
		}
		raw := json.RawMessage(`"UPID:source-node:1"`)
		return &raw, nil
	}, update: func(node, id string, p *nodes.UpdateQemuConfigParams) error {
		writes = append(writes, "marker")
		if node != "target" || id != "101" || p.Description == nil || *p.Description != marker {
			t.Fatal("inherited marker not replaced")
		}
		return nil
	}}
	deps := Deps{PVE: managedRootClient{q: q, n: n}, Config: &config.CPIConfig{}, Logger: log.NewNopLogger()}
	parsed := &createVMParsedArgs{agentID: "agent", rawVolid: "wrong:import/ignored.qcow2"}
	shape := &createVMShape{node: "target", vmStorage: "dest", rootDiskKey: "virtio0", rootDiskGiB: 8, vmDiskFormat: "qcow2", memMiB: 1024, cores: 2, sockets: 1}
	target := StoragePlanTarget{Role: storageRoleRoot, Node: "target", StorageID: "dest", Mechanism: mechanism, Source: &StorageRootSource{Node: "source-node", StorageID: "source", VolumeID: "source:import/frozen.qcow2", TemplateVMID: 900}}
	if err := createManagedVMRoot(context.Background(), deps, parsed, shape, target, 101, marker); err != nil {
		t.Fatal(err)
	}
	if mechanism == "import" {
		if len(writes) != 1 || writes[0] != "import" {
			t.Fatal(writes)
		}
	} else if len(writes) != 2 || writes[0] != "clone" || writes[1] != "marker" {
		t.Fatal(writes)
	}
}
