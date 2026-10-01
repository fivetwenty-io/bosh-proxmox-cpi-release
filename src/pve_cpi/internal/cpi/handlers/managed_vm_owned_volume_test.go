package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	sdk "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/client"
)

// ownedVolumeManagedClient fails the images-storage listing on demand, and
// otherwise answers exactly as the managed entry fixture does.
type ownedVolumeManagedClient struct {
	*managedVMEntryClient
	failImagesListing bool
}

func (c *ownedVolumeManagedClient) Nodes() nodes.Service {
	return &ownedVolumeManagedNodes{Service: c.managedVMEntryClient.Nodes(), fail: c.failImagesListing}
}

type ownedVolumeManagedNodes struct {
	nodes.Service
	fail bool
}

func (n *ownedVolumeManagedNodes) ListStorage(ctx context.Context, node string, params *nodes.ListStorageParams) (*nodes.ListStorageResponse, error) {
	if n.fail && params != nil && params.Content != nil && *params.Content == "images" {
		return nil, errors.New("storage list unavailable")
	}
	return n.Service.ListStorage(ctx, node, params)
}

// runManagedVMOwnedVolumeCase narrows the VM band to [8000, 8001], places a
// live VM on 8001, and puts a volume named for 8000 on pool "b", which is not
// vm_storage. The managed path must neither hand out 8000 nor create blind
// when it cannot read the pools.
func runManagedVMOwnedVolumeCase(t *testing.T, failListing bool) {
	t.Helper()
	no := false
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	cfg := &config.CPIConfig{Node: "n1", VMStorage: "a", EphemeralStorageSet: "E", StoragePlacementNamespace: "namespace", StorageAllocationJournalDir: directory, AgentMode: config.AgentModeNoAgent, StemcellStrategy: config.StemcellStrategyImport, Placement: &config.PlacementConfig{ExcludeMaintenanceNodes: &no}, StorageSets: map[string]config.StorageSet{"E": {Names: []string{"a", "b"}, Strategy: config.StoragePlacementStrategy{Name: "spread", Version: 1}}}}
	cfg.VMIDRangeStart, cfg.VMIDRangeEnd = 8000, 8001
	journal, err := aj.Initialize(t.Context(), directory, cfg.StoragePlacementNamespace, aj.Enrollment{ClusterID: "pve-root-ca-sha256:" + strings.Repeat("ab", 32), AuthorityID: "authority", AuditID: "audit", CompleteHistoricalAudit: true, PreviousWriterFenced: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := journal.Close(); err != nil {
			t.Error(err)
		}
	})
	state := &managedDiskTestState{volumes: map[string]*nodes.GetStorageContentResponse{
		"a:import/stemcell.qcow2": {Size: sdk.PVEInt(1 << 30), Format: "qcow2"},
	}}
	state.configs = map[int]map[string]any{cfg.VMIDRangeEnd: {"name": "occupied-end"}}
	if failListing {
		// Nothing names 8000 here, so only the failed read stands between
		// the allocator and a create it cannot vouch for.
		state.configs = map[int]map[string]any{}
	} else {
		state.volumes["b:vm-8000-disk-0"] = &nodes.GetStorageContentResponse{Size: sdk.PVEInt(1 << 30), Format: "raw"}
	}
	entry := &managedVMEntryClient{diagnosticVMClient: diagnosticVMClient{managedDiskTestPVE{state: state}}, journal: journal}
	client := &ownedVolumeManagedClient{managedVMEntryClient: entry, failImagesListing: failListing}
	deps := Deps{Config: cfg, PVE: client, Logger: log.NewNopLogger()}
	args := []json.RawMessage{json.RawMessage(`"owned-volume-agent"`), json.RawMessage(`":heavy:a:import/stemcell.qcow2"`), json.RawMessage(`{"cpu":1,"ram":1024,"root_disk_size":1024}`), json.RawMessage(`{}`), json.RawMessage(`[]`), json.RawMessage(`{}`)}

	result, err := createVM(t.Context(), deps, args)
	if err == nil {
		t.Fatalf("the managed allocator must refuse; got result %v", result)
	}
	if entry.creates != 0 {
		t.Fatalf("no VM may be created; got %d creates (err=%v)", entry.creates, err)
	}
	want := "no free VMID"
	if failListing {
		want = "storage list unavailable"
	}
	if !strings.Contains(err.Error(), want) {
		t.Errorf("want an error naming %q, got %v", want, err)
	}
}

// TestManagedVMSkipsVMIDNamingVolumeOnOtherImagesStorage pins the root fix on
// the managed path, which allocated through NextVMID with no storage scan.
func TestManagedVMSkipsVMIDNamingVolumeOnOtherImagesStorage(t *testing.T) {
	runManagedVMOwnedVolumeCase(t, false)
}

// TestManagedVMImagesStorageListingFailureFailsAllocation pins fail-closed on
// the managed path.
func TestManagedVMImagesStorageListingFailureFailsAllocation(t *testing.T) {
	runManagedVMOwnedVolumeCase(t, true)
}

// TestManagedVMAttachRefusesOwnedLegacyDiskBeforeItsStep pins the clean
// refusal on the managed create_vm attach. A legacy disk named for the VM is
// refused before the VM records its handoff step, so the refusal leaves the
// VM allocation clean instead of marking it uncertain, and the disk attach
// never runs.
func TestManagedVMAttachRefusesOwnedLegacyDiskBeforeItsStep(t *testing.T) {
	// Stays serial: it swaps the package variable attachExistingDiskForVM.
	m := createdManagedVM(t)
	attaches := 0
	previous := attachExistingDiskForVM
	attachExistingDiskForVM = func(context.Context, Deps, *aj.Handle, resolvedDisk, string, int) (string, error) {
		attaches++
		return "", errors.New("the disk attach ran")
	}
	t.Cleanup(func() { attachExistingDiskForVM = previous })
	volume := fmt.Sprintf("a:%d/vm-%d-disk-0.raw", m.vmid, m.vmid)
	cid, err := pve.EncodeDiskCID(volume, nil)
	if err != nil {
		t.Fatal(err)
	}

	err = m.attachPersistent(t.Context(), resolvedDisk{diskCID: cid, birth: volume, volid: volume})

	// The whole create_vm text, because the op prefix is what picks it over
	// attach_disk's.
	want := fmt.Sprintf("create_vm: refusing to attach disk %s to the new VM %d. Its volume %s is named for VMID %d, so PVE would "+
		"count it as one of the VM's own disks and free it on the next detach or delete. Nothing was "+
		"attached, and create_vm rolls the new VM back, or keeps it tagged when pve.debug.keep_failed_vms is set. "+
		"The VMID allocator could not see this volume when it drew VMID %d, so its storage was not active "+
		"on node %s or is local to another node, and a retry can draw the same VMID again. Make that "+
		"storage active on node %s, or set cloud_properties.target_node to the node whose local storage holds "+
		"the disk, and then retry", cid, m.vmid, volume, m.vmid, m.vmid, m.shape.node, m.shape.node)
	if err == nil || err.Error() != want {
		t.Fatalf("want exactly the create_vm refusal:\n%s\ngot:\n%v", want, err)
	}
	if attaches != 0 {
		t.Errorf("the disk attach must not run; got %d calls", attaches)
	}
	if m.guard.Err() != nil {
		t.Errorf("the refusal poisoned the VM allocation: %v", m.guard.Err())
	}
	record := m.handle.Record()
	if record.State == aj.ReconciliationRequired {
		t.Errorf("the refusal demanded reconciliation: %s", record.Reason)
	}
	for i := range record.Steps {
		if strings.HasPrefix(record.Steps[i].Kind, "vm.persistent.") {
			t.Errorf("the refusal must come before the handoff step; found %s in %s", record.Steps[i].Kind, record.Steps[i].State)
		}
	}
}
