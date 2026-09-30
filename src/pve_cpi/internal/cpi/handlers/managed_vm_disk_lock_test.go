package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
	sdk "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/client"
)

// createVMDiskClient is one cluster that can run a journal-managed create_vm
// and a journal-managed disk lifecycle. It takes VM creation, status, and
// destruction from the create_vm entry fixture, digest-checked configuration
// writes and disk moves from the lifecycle flow fixture, and sentinel pools
// from the store the test holds.
type createVMDiskClient struct {
	*managedVMEntryClient
	flow  *lifecycleFlowPVE
	locks *lockContention
	// destroyedConfigs keeps each destroyed VM's last configuration, so a
	// test can still read what a rolled-back VM carried.
	destroyedConfigs map[int]map[string]any
}

func (c *createVMDiskClient) Pools() pve.PoolService {
	return contendedPools{PoolService: managedDiskTestPools{state: c.state}, locks: c.locks}
}

func (c *createVMDiskClient) QEMU() qemu.Service {
	return &createVMDiskQEMU{managedVMEntryQEMU: managedVMEntryQEMU{managedDiskTestQEMU{state: c.state}, c.managedVMEntryClient}}
}

func (c *createVMDiskClient) Nodes() nodes.Service {
	return &createVMDiskNodes{
		managedVMEntryNodes: &managedVMEntryNodes{managedDiskTestNodes: managedDiskTestNodes{state: c.state}, client: c.managedVMEntryClient},
		flow:                lifecycleFlowNodes{managedDiskTestNodes: managedDiskTestNodes{state: c.state}, c: c.flow},
		disk:                c,
	}
}

type createVMDiskQEMU struct{ managedVMEntryQEMU }

// Create stamps the generation digest the lifecycle guard's config writes need.
func (q *createVMDiskQEMU) Create(ctx context.Context, node string, params map[string]any) (string, error) {
	upid, err := q.managedVMEntryQEMU.Create(ctx, node, params)
	if err == nil {
		q.state.configs[params["vmid"].(int)]["digest"] = "1"
	}
	return upid, err
}

// ListSnapshots reports no snapshots, so the attach's snapshot guard passes.
func (q *createVMDiskQEMU) ListSnapshots(context.Context, string, int) ([]map[string]any, error) {
	return nil, nil
}

type createVMDiskNodes struct {
	*managedVMEntryNodes
	flow lifecycleFlowNodes
	disk *createVMDiskClient
}

// DeleteQemu keeps a copy of the VM's configuration before it is destroyed.
func (n *createVMDiskNodes) DeleteQemu(ctx context.Context, node, id string, params *nodes.DeleteQemuParams) (*nodes.DeleteQemuResponse, error) {
	vmid, err := strconv.Atoi(id)
	if err != nil {
		return nil, err
	}
	last := map[string]any{}
	for key, value := range n.state.configs[vmid] {
		last[key] = value
	}
	result, err := n.managedVMEntryNodes.DeleteQemu(ctx, node, id, params)
	if err == nil {
		if n.disk.destroyedConfigs == nil {
			n.disk.destroyedConfigs = map[int]map[string]any{}
		}
		n.disk.destroyedConfigs[vmid] = last
	}
	return result, err
}

func (n *createVMDiskNodes) UpdateQemuConfig(ctx context.Context, node, vmid string, p *nodes.UpdateQemuConfigParams) error {
	id, err := strconv.Atoi(vmid)
	if err != nil {
		return err
	}
	if n.state.configs[id]["digest"] == nil {
		n.state.configs[id]["digest"] = "1"
	}
	return n.flow.UpdateQemuConfig(ctx, node, vmid, p)
}

func (n *createVMDiskNodes) CreateQemuMoveDisk(ctx context.Context, node, vmid string, p *nodes.CreateQemuMoveDiskParams) (*nodes.CreateQemuMoveDiskResponse, error) {
	return n.flow.CreateQemuMoveDisk(ctx, node, vmid, p)
}

// createVMDiskFixture configures a journal-managed create_vm and create_disk
// on one fake cluster, parks one managed disk on a pinned parker, and returns
// what a create_vm with that disk in disk_cids needs. With overrides false the
// disk carries no drive-option overrides, so its attach writes nothing to the
// receiving VM before it takes the parker lock.
func createVMDiskFixture(t *testing.T, locks *lockContention, overrides bool) (Deps, *createVMDiskClient, *aj.Journal, string, int) {
	t.Helper()
	no := false
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	cfg := &config.CPIConfig{Node: "n1", VMStorage: "a", EphemeralStorageSet: "E", PersistentStorageSet: "P", DetachedDiskStrategy: "parked", StoragePlacementNamespace: "namespace", StorageAllocationJournalDir: directory, AgentMode: config.AgentModeNoAgent, StemcellStrategy: config.StemcellStrategyImport, Placement: &config.PlacementConfig{ExcludeMaintenanceNodes: &no}, StorageSets: map[string]config.StorageSet{
		"E": {Names: []string{"a"}, Strategy: config.StoragePlacementStrategy{Name: "spread", Version: 1}},
		"P": {Names: []string{"b"}, Strategy: config.StoragePlacementStrategy{Name: "spread", Version: 1}},
	}}
	if !overrides {
		off := false
		cfg.DiskPerformance = &config.DiskPerformanceDefaults{Iothread: &off, SSD: &off, Discard: &off}
	}
	journal, err := aj.Initialize(t.Context(), directory, cfg.StoragePlacementNamespace, aj.Enrollment{ClusterID: "pve-root-ca-sha256:" + strings.Repeat("ab", 32), AuthorityID: "authority", AuditID: "audit", CompleteHistoricalAudit: true, PreviousWriterFenced: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := journal.Close(); err != nil {
			t.Error(err)
		}
	})
	state := &managedDiskTestState{
		volumes: map[string]*nodes.GetStorageContentResponse{"a:import/stemcell.qcow2": {Size: sdk.PVEInt(1 << 30), Format: "qcow2"}},
		configs: map[int]map[string]any{},
	}
	parker := cfg.ParkedDiskVMIDRangeStartValue()
	state.configs[parker] = map[string]any{"name": fmt.Sprintf("bosh-parker-%d", parker), "tags": "bosh-parker", "protection": 1, "scsihw": "virtio-scsi-pci", "digest": "1"}
	entry := &managedVMEntryClient{diagnosticVMClient: diagnosticVMClient{managedDiskTestPVE{state: state}}, journal: journal}
	client := &createVMDiskClient{managedVMEntryClient: entry, flow: &lifecycleFlowPVE{managedDiskTestPVE: managedDiskTestPVE{state: state}}, locks: locks}
	deps := Deps{Config: cfg, PVE: client, Logger: log.NewNopLogger()}
	created, err := HandleCreateDisk(deps).Handle(t.Context(), []json.RawMessage{json.RawMessage(`1024`), json.RawMessage(`{}`), json.RawMessage(`null`)}, jsonrpc.Context{})
	if err != nil {
		t.Fatalf("create_disk: %v", err)
	}
	cid, ok := created.(string)
	if !ok {
		t.Fatalf("create_disk returned %T", created)
	}
	return deps, client, journal, cid, parker
}

func createVMArgs(t *testing.T, cid string) []json.RawMessage {
	t.Helper()
	disks, err := json.Marshal([]string{cid})
	if err != nil {
		t.Fatal(err)
	}
	return []json.RawMessage{json.RawMessage(`"disk-agent"`), json.RawMessage(`":heavy:a:import/stemcell.qcow2"`), json.RawMessage(`{"cpu":1,"ram":1024,"root_disk_size":1024}`), json.RawMessage(`{}`), disks, json.RawMessage(`{}`)}
}

// createVMDiskLockTimeout runs create_vm through its real entry with a parked
// disk in disk_cids while another request holds the parker's lock. The
// pre-attach waits out its wait and returns the disk unchanged, so create_vm
// rolls the attempt back and fails retriably. The VM is destroyed, its
// generation is closed with every step observed, and the disk is returned on
// its parker.
func createVMDiskLockTimeout(t *testing.T, overrides bool) (Deps, *createVMDiskClient, *aj.Journal, *lockContention, []json.RawMessage) {
	t.Helper()
	locks := newLockContention(t)
	deps, client, journal, cid, parker := createVMDiskFixture(t, locks, overrides)
	locks.reset()
	plantHeldParkerLock(locks, parker)
	shortenManagedLockWait(t, 1500*time.Millisecond)
	args := createVMArgs(t, cid)

	_, err := createVM(t.Context(), deps, args)
	if !errors.Is(err, pve.ErrClusterLockTimeout) || !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("want the retriable lock timeout, got %v", err)
	}
	if client.creates != 1 || client.destroys != 1 {
		t.Fatalf("the timed-out attempt was not rolled back: creates=%d destroys=%d", client.creates, client.destroys)
	}
	if _, found, err := journal.InspectVM("disk-agent"); err != nil || found {
		t.Fatalf("the rollback left a VM generation for the retry to resume: found=%t err=%v", found, err)
	}
	records, err := journal.List()
	if err != nil {
		t.Fatal(err)
	}
	for i := range records {
		if records[i].Kind != "vm" {
			continue
		}
		if records[i].State != aj.Deleted {
			t.Fatalf("the rolled-back VM generation is %s (reason %q), want %s", records[i].State, records[i].Reason, aj.Deleted)
		}
		assertStepsObserved(t, "VM", records[i])
	}
	assertCreateVMDiskReturned(t, journal)
	return deps, client, journal, locks, args
}

// assertCreateVMDiskRecordsSettled checks both allocations a create_vm with a
// disk in disk_cids holds. The VM record is not left for reconciliation and
// every VM step is observed, and the one disk record is returned with every
// step observed.
func assertCreateVMDiskRecordsSettled(t *testing.T, journal *aj.Journal) {
	t.Helper()
	vm, found, err := journal.InspectVM("disk-agent")
	if err != nil || !found {
		t.Fatalf("VM record: found=%t err=%v", found, err)
	}
	if vm.State == aj.ReconciliationRequired {
		t.Fatalf("the VM allocation was marked for reconciliation: %s", vm.Reason)
	}
	assertStepsObserved(t, "VM", vm)
	assertCreateVMDiskReturned(t, journal)
}

func assertStepsObserved(t *testing.T, name string, record aj.Record) {
	t.Helper()
	for i := range record.Steps {
		if record.Steps[i].State != aj.Observed {
			t.Fatalf("%s step %s (%s) left %s", name, record.Steps[i].ID, record.Steps[i].Kind, record.Steps[i].State)
		}
	}
}

// assertCreateVMDiskReturned checks that the one disk record is returned with
// every step observed.
func assertCreateVMDiskReturned(t *testing.T, journal *aj.Journal) {
	t.Helper()
	records, err := journal.List()
	if err != nil {
		t.Fatal(err)
	}
	disks := 0
	for i := range records {
		if records[i].Kind != "disk" {
			continue
		}
		disks++
		if records[i].State != aj.ReadyToReturn {
			t.Fatalf("the disk allocation was left %s (reason %q)", records[i].State, records[i].Reason)
		}
		for j := range records[i].Steps {
			if records[i].Steps[j].State != aj.Observed {
				t.Fatalf("disk step %s (%s) left %s", records[i].Steps[j].ID, records[i].Steps[j].Kind, records[i].Steps[j].State)
			}
		}
	}
	if disks != 1 {
		t.Fatalf("found %d disk records, want 1", disks)
	}
}

// retryCreateVMAfterDiskLockTimeout is the Director's retry of create_vm under
// the same agent ID once the parker lock is free. The first attempt's VM is
// gone, so the retry builds exactly one fresh VM in a new generation, attaches
// the disk, and ends with both allocations settled.
func retryCreateVMAfterDiskLockTimeout(t *testing.T, deps Deps, client *createVMDiskClient, journal *aj.Journal, locks *lockContention, args []json.RawMessage) {
	t.Helper()
	creates := client.creates
	locks.reset()
	result, err := createVM(t.Context(), deps, args)
	if err != nil {
		t.Fatalf("the Director's create_vm retry failed: %v", err)
	}
	values, ok := result.([]any)
	if !ok || len(values) == 0 {
		t.Fatalf("unexpected create_vm result %v", result)
	}
	vmid, err := strconv.Atoi(fmt.Sprint(values[0]))
	if err != nil {
		t.Fatal(err)
	}
	if client.creates != creates+1 {
		t.Fatalf("the retry did not build exactly one fresh VM: creates %d then %d", creates, client.creates)
	}
	attached := false
	for key := range client.state.configs[vmid] {
		if isDiskOptionKey(key) && key != "virtio0" {
			attached = true
		}
	}
	if !attached {
		t.Fatalf("the retry did not attach the persistent disk: %v", client.state.configs[vmid])
	}
	assertCreateVMDiskRecordsSettled(t, journal)
}

// TestCreateVMDiskLockTimeoutRebuildsOnRetry covers a parked disk with no
// drive-option overrides, whose attach writes nothing to the receiving VM
// before it takes the parker lock.
func TestCreateVMDiskLockTimeoutRebuildsOnRetry(t *testing.T) {
	deps, client, journal, locks, args := createVMDiskLockTimeout(t, false)
	retryCreateVMAfterDiskLockTimeout(t, deps, client, journal, locks, args)
}

// TestCreateVMDiskLockTimeoutAfterOverlayNote is the default shape. Every
// managed disk carries drive-option overrides, so the attach writes them onto
// the receiving VM before it waits for the parker lock. That note does not
// move or change the disk, so the timeout after it is still clean. The
// rollback destroys the receiving VM, so the note is read from the
// configuration the VM had when it was destroyed.
func TestCreateVMDiskLockTimeoutAfterOverlayNote(t *testing.T) {
	_, client, _, _, _ := createVMDiskLockTimeout(t, true)
	noted := false
	for _, cfg := range client.destroyedConfigs {
		_, raw := pve.ParseSentinel(pve.DescriptionFromConfig(cfg))
		if tags, _ := cfg["tags"].(string); len(raw[pve.DiskOptOverlaysSentinelKey]) > 0 && !strings.Contains(tags, "bosh-parker") {
			noted = true
		}
	}
	if !noted {
		t.Fatal("the pre-attach did not write the overlay note onto the receiving VM before it waited")
	}
}

// TestCreateVMDiskLockTimeoutAfterOverlayNoteRebuildsOnRetry is the Director's
// in-task retry of that default shape once the parker lock frees.
func TestCreateVMDiskLockTimeoutAfterOverlayNoteRebuildsOnRetry(t *testing.T) {
	deps, client, journal, locks, args := createVMDiskLockTimeout(t, true)
	retryCreateVMAfterDiskLockTimeout(t, deps, client, journal, locks, args)
}
