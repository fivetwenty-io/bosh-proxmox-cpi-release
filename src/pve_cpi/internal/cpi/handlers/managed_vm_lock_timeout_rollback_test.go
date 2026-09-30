package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/clusterstorage"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
	sdk "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/client"
)

// rollbackFlowVMs tracks the guests a managed create_vm builds on the flow
// fixture's cluster, and counts the VM disposals that start.
type rollbackFlowVMs struct {
	mu        sync.Mutex
	created   map[int]bool
	running   map[int]bool
	destroyed []int
	// disposals counts HA rule reads, which is the first thing a VM
	// disposal asks PVE after its admission.
	disposals int
}

func (v *rollbackFlowVMs) disposalStarted() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.disposals++
}

// rollbackFlowPVE is the contended flow fixture with what a managed create_vm
// needs on top of it. Storage definitions carry import content for the
// stemcell, a create imports a root volume, a created guest starts, and a
// destroy removes a created guest along with the volumes it still references,
// the way PVE's purge does.
type rollbackFlowPVE struct {
	contendedFlowPVE
	vms *rollbackFlowVMs
}

func (c rollbackFlowPVE) QEMU() qemu.Service {
	return rollbackFlowQEMU{lifecycleFlowQEMU: lifecycleFlowQEMU{managedDiskTestQEMU: managedDiskTestQEMU{state: c.state}, c: c.lifecycleFlowPVE}, vms: c.vms}
}

func (c rollbackFlowPVE) Nodes() nodes.Service {
	return rollbackFlowNodes{Service: c.contendedFlowPVE.Nodes(), c: c.lifecycleFlowPVE, vms: c.vms}
}

func (c rollbackFlowPVE) Cluster() cluster.Service {
	return rollbackFlowCluster{Service: c.lifecycleFlowPVE.Cluster(), vms: c.vms}
}

func (c rollbackFlowPVE) ClusterStorage() clusterstorage.Service { return diagnosticVMDefinitions{} }

type rollbackFlowQEMU struct {
	lifecycleFlowQEMU
	vms *rollbackFlowVMs
}

func (q rollbackFlowQEMU) Create(ctx context.Context, node string, params map[string]any) (string, error) {
	upid, err := q.lifecycleFlowQEMU.Create(ctx, node, params)
	if err != nil {
		return "", err
	}
	vmid, ok := params["vmid"].(int)
	if !ok {
		return "", fmt.Errorf("missing VMID")
	}
	drive, ok := params["virtio0"].(string)
	if !ok {
		return "", fmt.Errorf("missing import root")
	}
	pool := strings.Split(drive, ":")[0]
	volume := fmt.Sprintf("%s:%d/vm-%d-disk-0.qcow2", pool, vmid, vmid)
	q.c.state.configs[vmid]["virtio0"] = volume + ",size=1G"
	q.c.state.volumes[volume] = &nodes.GetStorageContentResponse{Size: sdk.PVEInt(1 << 30), Format: "qcow2"}
	q.vms.mu.Lock()
	q.vms.created[vmid] = true
	q.vms.mu.Unlock()
	return upid, nil
}

func (q rollbackFlowQEMU) Start(_ context.Context, _ string, vmid int) (string, error) {
	q.vms.mu.Lock()
	defer q.vms.mu.Unlock()
	q.vms.running[vmid] = true
	return "UPID:n1:start", nil
}

func (q rollbackFlowQEMU) Status(_ context.Context, _ string, vmid int) (map[string]any, error) {
	q.vms.mu.Lock()
	defer q.vms.mu.Unlock()
	if q.vms.running[vmid] {
		return map[string]any{"status": "running"}, nil
	}
	return map[string]any{"status": "stopped"}, nil
}

type rollbackFlowNodes struct {
	nodes.Service
	c   *lifecycleFlowPVE
	vms *rollbackFlowVMs
}

func (n rollbackFlowNodes) DeleteQemu(ctx context.Context, node, vmidText string, params *nodes.DeleteQemuParams) (*nodes.DeleteQemuResponse, error) {
	vmid, err := strconv.Atoi(vmidText)
	if err != nil {
		return nil, err
	}
	n.vms.mu.Lock()
	created := n.vms.created[vmid]
	n.vms.mu.Unlock()
	if !created {
		return n.Service.DeleteQemu(ctx, node, vmidText, params)
	}
	if params == nil || params.DestroyUnreferencedDisks == nil || *params.DestroyUnreferencedDisks {
		return nil, fmt.Errorf("cleanup used unbounded volume destruction")
	}
	for key, value := range n.c.state.configs[vmid] {
		if !managedVMVolumeDevice(key) {
			continue
		}
		drive, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("malformed owned volume")
		}
		delete(n.c.state.volumes, strings.Split(drive, ",")[0])
	}
	delete(n.c.state.configs, vmid)
	delete(n.c.vmNodes, vmid)
	n.vms.mu.Lock()
	n.vms.destroyed = append(n.vms.destroyed, vmid)
	delete(n.vms.running, vmid)
	n.vms.mu.Unlock()
	raw := json.RawMessage(`"UPID:n1:destroy"`)
	return &raw, nil
}

type rollbackFlowCluster struct {
	cluster.Service
	vms *rollbackFlowVMs
}

func (c rollbackFlowCluster) ListHaRules(ctx context.Context, params *cluster.ListHaRulesParams) (*cluster.ListHaRulesResponse, error) {
	c.vms.disposalStarted()
	return c.Service.ListHaRules(ctx, params)
}

// ListStatus reports the fixture's two nodes with the compute capacity the VM
// placement reads. Only n1 has the memory for the VM, so the VM lands beside
// the parker and its disks attach without a cross-node migration.
func (c rollbackFlowCluster) ListStatus(context.Context) (*cluster.ListStatusResponse, error) {
	r := cluster.ListStatusResponse{
		json.RawMessage(`{"type":"cluster","quorate":1}`),
		json.RawMessage(`{"type":"node","name":"n1","online":1,"maxcpu":8,"maxmem":17179869184,"mem":1073741824,"cpu":0.1}`),
		json.RawMessage(`{"type":"node","name":"n2","online":1,"maxcpu":8,"maxmem":536870912,"mem":0,"cpu":0.1}`),
	}
	return &r, nil
}

func (c rollbackFlowCluster) ListHaResources(context.Context, *cluster.ListHaResourcesParams) (*cluster.ListHaResourcesResponse, error) {
	rows := cluster.ListHaResourcesResponse{}
	return &rows, nil
}

func (c rollbackFlowCluster) ListSdnVnets(context.Context, *cluster.ListSdnVnetsParams) (*cluster.ListSdnVnetsResponse, error) {
	rows := cluster.ListSdnVnetsResponse{}
	return &rows, nil
}

// rollbackFlow is a managed create_vm about to attach two journal-managed
// persistent disks. The first is free and attaches without a parker. The
// second sits on the parker, whose lock another request holds.
type rollbackFlow struct {
	parked *parkedFlowDisk
	free   lifecycleFlowDisk
	locks  *lockContention
	vms    *rollbackFlowVMs
	args   []json.RawMessage
}

const rollbackFlowAgent = "rollback-agent"

func newRollbackFlow(t *testing.T) *rollbackFlow {
	t.Helper()
	locks := newLockContention(t)
	parked := newParkedFlowDisk(t, locks)
	free := journalLifecycleFlowDisk(t, parked.journal, parked.client.state, true, false)
	vms := &rollbackFlowVMs{created: map[int]bool{}, running: map[int]bool{}}
	parked.deps.PVE = rollbackFlowPVE{contendedFlowPVE: parked.deps.PVE.(contendedFlowPVE), vms: vms}
	parked.deps.Logger = log.NewNopLogger()
	no := false
	cfg := parked.deps.Config
	cfg.VMStorage = "a"
	cfg.EphemeralStorageSet = "E"
	cfg.StorageSets = map[string]config.StorageSet{"E": {Names: []string{"a", "b"}, Strategy: config.StoragePlacementStrategy{Name: "spread", Version: 1}}}
	cfg.AgentMode = config.AgentModeNoAgent
	cfg.StemcellStrategy = config.StemcellStrategyImport
	cfg.Placement = &config.PlacementConfig{ExcludeMaintenanceNodes: &no}
	parked.client.state.volumes["a:import/stemcell.qcow2"] = &nodes.GetStorageContentResponse{Size: sdk.PVEInt(1 << 30), Format: "qcow2"}
	disks, err := json.Marshal([]string{free.cid, parked.cid})
	if err != nil {
		t.Fatal(err)
	}
	args := []json.RawMessage{json.RawMessage(fmt.Sprintf("%q", rollbackFlowAgent)), json.RawMessage(`":heavy:a:import/stemcell.qcow2"`), json.RawMessage(`{"cpu":1,"ram":1024,"root_disk_size":1024}`), json.RawMessage(`{}`), disks, json.RawMessage(`{}`)}
	locks.reset()
	plantHeldParkerLock(locks, parked.parker)
	shortenManagedLockWait(t, 1500*time.Millisecond)
	return &rollbackFlow{parked: parked, free: free, locks: locks, vms: vms, args: args}
}

func (f *rollbackFlow) createVM(t *testing.T) (any, error) {
	t.Helper()
	return createVM(t.Context(), f.parked.deps, f.args)
}

// generation returns the agent's active VM generation, the one a create_vm
// under that agent ID resumes.
func (f *rollbackFlow) generation(t *testing.T) aj.Record {
	t.Helper()
	record, found, err := f.parked.journal.InspectVM(rollbackFlowAgent)
	if err != nil || !found {
		t.Fatalf("no active VM generation for the agent: found=%t err=%v", found, err)
	}
	return record
}

func (f *rollbackFlow) diskRecord(t *testing.T, id string) aj.Record {
	t.Helper()
	record, err := f.parked.journal.Inspect(id)
	if err != nil {
		t.Fatal(err)
	}
	return record
}

// holderOf resolves a disk the way the handlers do and names what holds it.
func (f *rollbackFlow) holderOf(t *testing.T, cid string) *pve.DiskHolder {
	t.Helper()
	deps := f.parked.deps
	bare, meta, err := decodeDiskCID(t.Context(), deps, "test", cid)
	if err != nil {
		t.Fatal(err)
	}
	rd, err := resolveDiskForOp(t.Context(), deps, "test", cid, bare, meta)
	if err != nil {
		t.Fatalf("resolving %s: %v", cid, err)
	}
	return rd.holder
}

func (f *rollbackFlow) created(t *testing.T) []int {
	t.Helper()
	f.vms.mu.Lock()
	defer f.vms.mu.Unlock()
	vmids := make([]int, 0, len(f.vms.created))
	for vmid := range f.vms.created {
		vmids = append(vmids, vmid)
	}
	return vmids
}

// TestCreateVMRollbackPreservationTimeoutIsUncertain drives the real
// create_vm handler into a persistent disk whose parker lock another request
// holds past the managed wait, with a fallback attempt left. The attempt retry
// rolls the attempt back, and the holder never leaves, so preserving the disk
// that had already attached waits out the same parker lock and times out. By
// then the disposal has recorded its admission, so the generation is half
// disposed and must not be resumed as a create. The generation requires
// reconciliation, and the Director reads a failure it does not retry.
func TestCreateVMRollbackPreservationTimeoutIsUncertain(t *testing.T) {
	flow := newRollbackFlow(t)
	limit := 1
	flow.parked.deps.Config.Placement.FallbackMax = &limit

	_, err := flow.createVM(t)
	if !errors.Is(err, pve.ErrClusterLockTimeout) {
		t.Fatalf("want the lock timeout inside the failure, got %v", err)
	}
	var typed *cpierrors.Error
	if !errors.As(err, &typed) || typed.Type() == cpierrors.TypeRetriableCloud || typed.OkToRetry() {
		t.Fatalf("a half-disposed generation went back to the Director as retriable: %v", err)
	}
	if flow.vms.disposals == 0 {
		t.Fatal("the rollback never ran a disposal")
	}
	generation := flow.generation(t)
	if generation.State != aj.ReconciliationRequired {
		t.Fatalf("the half-disposed generation is %s, want %s", generation.State, aj.ReconciliationRequired)
	}
	admitted := false
	for i := range generation.Verifications {
		admitted = admitted || strings.Contains(generation.Verifications[i].EvidenceJSON, managedVMCleanupAdmissionOperation)
	}
	if !admitted {
		t.Fatal("the rollback failed before its disposal was admitted, so it proves nothing")
	}
	created := flow.created(t)
	if len(created) != 1 || len(flow.vms.destroyed) != 0 {
		t.Fatalf("the rollback destroyed a VM whose disk it could not preserve: created=%v destroyed=%v", created, flow.vms.destroyed)
	}
	// The preservation waited out the lock before it touched the disk, so the
	// disk's own allocation comes back returned and still on the VM.
	assertReturnedRecord(t, "attached disk", flow.diskRecord(t, flow.free.id))
	if holder := flow.holderOf(t, flow.free.cid); holder == nil || holder.VMID != created[0] {
		t.Fatalf("the attached disk moved although its preservation timed out: %+v", holder)
	}
}

// TestVMRollbackFailureRules covers the rule itself. A create_vm rollback
// marks the generation uncertain on every failure, including a preservation
// that waited out a parker lock cleanly, and the reconciliation error leads
// the joined error so the Director cannot read the timeout's retriable type
// first. delete_vm's rule keeps its clean exit for that same timeout.
func TestVMRollbackFailureRules(t *testing.T) {
	returned := &diskReturnedAfterLockTimeout{err: lockTimeoutError()}

	rollback := createdManagedVM(t)
	err := managedVMCreateRollback.settle(rollback.handle, returned)
	if !errors.Is(err, pve.ErrClusterLockTimeout) || cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("a rollback preservation timeout kept the retriable type in front: %v", err)
	}
	if state := rollback.handle.Record().State; state != aj.ReconciliationRequired {
		t.Fatalf("a rollback preservation timeout left the generation %s", state)
	}
	if err := managedVMCreateRollback.settle(rollback.handle, nil); err != nil {
		t.Fatalf("a rollback without a failure returned %v", err)
	}

	deletion := createdManagedVM(t)
	if err := managedVMDeletion.settle(deletion.handle, returned); !isDiskReturnedAfterLockTimeout(err) || deletion.handle.Record().State == aj.ReconciliationRequired {
		t.Fatalf("a delete_vm preservation timeout lost its clean exit: %v %s", err, deletion.handle.Record().State)
	}
}
