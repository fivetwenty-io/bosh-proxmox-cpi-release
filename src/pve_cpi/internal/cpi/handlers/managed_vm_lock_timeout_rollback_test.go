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
// fixture's cluster, counts the VM disposals that start, and lets a test act
// at the moment the first one begins.
type rollbackFlowVMs struct {
	mu        sync.Mutex
	created   map[int]bool
	running   map[int]bool
	destroyed []int
	// disposals counts HA rule reads, which is the first thing a VM
	// disposal asks PVE after its admission. onDisposal runs once, on the
	// first of them.
	disposals  int
	onDisposal func()
	// auditBlind, while set, is the error the cluster's audit visibility
	// check returns, so every allocation audit comes back incomplete.
	auditBlind error
}

func (v *rollbackFlowVMs) setAuditBlind(err error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.auditBlind = err
}

func (v *rollbackFlowVMs) disposalStarted() {
	v.mu.Lock()
	v.disposals++
	hook := v.onDisposal
	v.onDisposal = nil
	v.mu.Unlock()
	if hook != nil {
		hook()
	}
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

func (c rollbackFlowPVE) StorageAuditVisibility(ctx context.Context) error {
	c.vms.mu.Lock()
	blind := c.vms.auditBlind
	c.vms.mu.Unlock()
	if blind != nil {
		return blind
	}
	return c.lifecycleFlowPVE.StorageAuditVisibility(ctx)
}

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

// generations returns every VM generation the agent has had, open or closed.
func (f *rollbackFlow) generations(t *testing.T) []aj.Record {
	t.Helper()
	records, err := f.parked.journal.List()
	if err != nil {
		t.Fatal(err)
	}
	var generations []aj.Record
	for i := range records {
		if records[i].Kind == "vm" && records[i].AgentID == rollbackFlowAgent {
			generations = append(generations, records[i])
		}
	}
	return generations
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

// TestCreateVMLockTimeoutRollsBackTheAttempt drives the real create_vm
// handler into a persistent disk whose parker lock another request holds past
// the managed wait. The first disk is already attached when the second one
// times out. The other request finishes its window as the rollback starts, so
// the rollback preserves the first disk to the parker, destroys the VM, closes
// the generation, and only then hands the Director the retriable timeout. The
// Director's retry under the same agent ID then starts a new generation and
// builds a fresh VM with both disks.
func TestCreateVMLockTimeoutRollsBackTheAttempt(t *testing.T) {
	flow := newRollbackFlow(t)
	flow.vms.onDisposal = flow.locks.reset

	_, err := flow.createVM(t)
	if !errors.Is(err, pve.ErrClusterLockTimeout) || !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("create_vm did not hand the Director the retriable lock timeout: %v", err)
	}
	created := flow.created(t)
	if len(created) != 1 {
		t.Fatalf("create_vm built %d VMs, want 1", len(created))
	}
	first := created[0]
	if _, exists := flow.parked.client.state.configs[first]; exists || len(flow.vms.destroyed) != 1 {
		t.Fatalf("the attempt's VM %d was not destroyed: destroyed=%v", first, flow.vms.destroyed)
	}
	if flow.vms.disposals == 0 {
		t.Fatal("the rollback never ran a disposal")
	}
	for name, cid := range map[string]string{"attached": flow.free.cid, "parked": flow.parked.cid} {
		holder := flow.holderOf(t, cid)
		if holder == nil || !holder.IsParker || holder.VMID != flow.parked.parker {
			t.Fatalf("the %s disk is not on the parker after the rollback: %+v", name, holder)
		}
	}
	assertReturnedRecord(t, "attached disk", flow.diskRecord(t, flow.free.id))
	assertReturnedRecord(t, "parked disk", flow.parked.record(t))

	if _, found, err := flow.parked.journal.InspectVM(rollbackFlowAgent); err != nil || found {
		t.Fatalf("the rollback left a generation for a retry to resume: found=%t err=%v", found, err)
	}
	generations := flow.generations(t)
	if len(generations) != 1 {
		t.Fatalf("the agent has %d generations, want the one rolled back", len(generations))
	}
	old := generations[0]
	if old.State != aj.Deleted {
		t.Fatalf("the rolled-back generation is %s (reason %q), want %s", old.State, old.Reason, aj.Deleted)
	}
	persistent := false
	for i := range old.Steps {
		step := &old.Steps[i]
		if step.State != aj.Observed {
			t.Fatalf("rolled-back step %s (%s) left %s", step.ID, step.Kind, step.State)
		}
		persistent = persistent || strings.HasPrefix(step.Kind, "vm.persistent.")
	}
	if !persistent {
		t.Fatal("the generation never journaled the persistent disk handoff")
	}

	result, err := flow.createVM(t)
	if err != nil {
		t.Fatalf("the Director's retry failed: %v", err)
	}
	values, ok := result.([]any)
	if !ok || len(values) == 0 {
		t.Fatalf("unexpected create_vm result %v", result)
	}
	cid, ok := values[0].(string)
	if !ok {
		t.Fatalf("create_vm returned no CID: %v", values)
	}
	vmid, err := strconv.Atoi(cid)
	if err != nil {
		t.Fatal(err)
	}
	fresh := flow.generation(t)
	if fresh.ID == old.ID || fresh.State != aj.ReadyToReturn || fresh.CID != cid {
		t.Fatalf("the retry did not build a fresh generation: old=%s new=%s state=%s cid=%s", old.ID, fresh.ID, fresh.State, fresh.CID)
	}
	if vmid == first || len(flow.created(t)) != 2 {
		t.Fatalf("the retry did not build a fresh VM: first=%d retry=%d", first, vmid)
	}
	for name, cid := range map[string]string{"first": flow.free.cid, "second": flow.parked.cid} {
		if holder := flow.holderOf(t, cid); holder == nil || holder.VMID != vmid {
			t.Fatalf("the %s disk is not attached to the fresh VM %d: %+v", name, vmid, holder)
		}
	}
}

// TestCreateVMUnknownLockStateRollsBackTheAttempt runs the same attach with no
// other holder. The parker lock's create lands, but its confirming reads fail
// up to the deadline, so the acquire ends in the unknown lock state. The read
// on its way out proves the sentinel ours and the delete answers, so the disk
// comes back returned exactly as it does after a timed-out wait. create_vm
// therefore rolls the attempt back the same way: the attached disk is
// preserved to the parker, the VM is destroyed, the generation is closed, and
// the Director gets the retriable error with the unknown state inside it.
func TestCreateVMUnknownLockStateRollsBackTheAttempt(t *testing.T) {
	flow := newRollbackFlow(t)
	flow.locks.reset()
	failConfirmingReads(flow.locks)
	flow.vms.onDisposal = flow.locks.reset

	_, err := flow.createVM(t)
	if !errors.Is(err, pve.ErrClusterLockStateUnknown) || !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("create_vm did not hand the Director the retriable unknown lock state: %v", err)
	}
	created := flow.created(t)
	if len(created) != 1 || len(flow.vms.destroyed) != 1 || flow.vms.disposals == 0 {
		t.Fatalf("the attempt's VM was not disposed of: created=%v destroyed=%v disposals=%d", created, flow.vms.destroyed, flow.vms.disposals)
	}
	if _, exists := flow.parked.client.state.configs[created[0]]; exists {
		t.Fatalf("the attempt's VM %d is still on PVE", created[0])
	}
	for name, cid := range map[string]string{"attached": flow.free.cid, "parked": flow.parked.cid} {
		holder := flow.holderOf(t, cid)
		if holder == nil || !holder.IsParker || holder.VMID != flow.parked.parker {
			t.Fatalf("the %s disk is not on the parker after the rollback: %+v", name, holder)
		}
	}
	assertReturnedRecord(t, "attached disk", flow.diskRecord(t, flow.free.id))
	assertReturnedRecord(t, "parked disk", flow.parked.record(t))
	if _, found, err := flow.parked.journal.InspectVM(rollbackFlowAgent); err != nil || found {
		t.Fatalf("the rollback left a generation for a retry to resume: found=%t err=%v", found, err)
	}
	generations := flow.generations(t)
	if len(generations) != 1 || generations[0].State != aj.Deleted {
		t.Fatalf("the rolled-back generation was not closed: %d generations", len(generations))
	}
	for i := range generations[0].Steps {
		if step := &generations[0].Steps[i]; step.State != aj.Observed {
			t.Fatalf("rolled-back step %s (%s) left %s", step.ID, step.Kind, step.State)
		}
	}
}

// hasDisposalAdmission reports whether a record carries a VM cleanup
// admission, which a disposal records before its first delete step.
func hasDisposalAdmission(record aj.Record) bool {
	for i := range record.Verifications {
		if strings.Contains(record.Verifications[i].EvidenceJSON, managedVMCleanupAdmissionOperation) {
			return true
		}
	}
	return false
}

// resumeAfterTimeout releases the parker lock, runs the Director's next try,
// and checks that it resumed the given generation on the given VM rather
// than building another.
func (f *rollbackFlow) resumeAfterTimeout(t *testing.T, generation aj.Record, vmid int) {
	t.Helper()
	f.locks.reset()
	result, err := f.createVM(t)
	if err != nil {
		t.Fatalf("the next try did not resume the generation: %v", err)
	}
	values, ok := result.([]any)
	if !ok || len(values) == 0 {
		t.Fatalf("unexpected create_vm result %v", result)
	}
	if cid, _ := values[0].(string); cid != strconv.Itoa(vmid) {
		t.Fatalf("the next try returned VM %v, want the kept VM %d", values[0], vmid)
	}
	if created := f.created(t); len(created) != 1 || len(f.vms.destroyed) != 0 {
		t.Fatalf("the next try rebuilt the VM: created=%v destroyed=%v", created, f.vms.destroyed)
	}
	resumed := f.generation(t)
	if resumed.ID != generation.ID || resumed.State != aj.ReadyToReturn {
		t.Fatalf("the next try did not finish the same generation: was %s, now %s in %s", generation.ID, resumed.ID, resumed.State)
	}
}

// TestCreateVMLockTimeoutKeepsTheVMUnderKeepFailedVMs runs the same clean
// parker lock timeout with keep_failed_vms on. That setting asks us to keep
// the VM, so create_vm does not roll the attempt back. The timeout goes back
// retriable with the VM and its generation in place, and the next try resumes
// that generation on the same VM.
func TestCreateVMLockTimeoutKeepsTheVMUnderKeepFailedVMs(t *testing.T) {
	flow := newRollbackFlow(t)
	keep := true
	flow.parked.deps.Config.Debug = &config.DebugConfig{KeepFailedVMs: &keep}

	_, err := flow.createVM(t)
	if !errors.Is(err, pve.ErrClusterLockTimeout) || !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("create_vm did not hand the Director the retriable lock timeout: %v", err)
	}
	created := flow.created(t)
	if len(created) != 1 || len(flow.vms.destroyed) != 0 {
		t.Fatalf("keep_failed_vms did not keep the VM: created=%v destroyed=%v", created, flow.vms.destroyed)
	}
	generation := flow.generation(t)
	if generation.State == aj.ReconciliationRequired || hasDisposalAdmission(generation) {
		t.Fatalf("keep_failed_vms left the generation %s (admission recorded: %t)", generation.State, hasDisposalAdmission(generation))
	}
	flow.resumeAfterTimeout(t, generation, created[0])
}

// TestCreateVMRefusedRollbackKeepsTheTimeoutRetriable makes the rollback's
// disposal refuse before its admission. The allocation audit loses cluster
// visibility while the disk waits out the parker lock, so the disposal's
// admission audit is incomplete and its gate refuses. Nothing was disposed of,
// so the error still reads retriable, the generation stays as it was with no
// admission recorded, and the next try resumes it on the same VM.
func TestCreateVMRefusedRollbackKeepsTheTimeoutRetriable(t *testing.T) {
	flow := newRollbackFlow(t)
	rejected := flow.locks.rejected
	go func() {
		select {
		case <-rejected:
			flow.vms.setAuditBlind(errors.New("cluster resource listing unavailable"))
		case <-t.Context().Done():
		}
	}()

	_, err := flow.createVM(t)
	if !errors.Is(err, pve.ErrClusterLockTimeout) || !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("a refused rollback did not leave the timeout retriable: %v", err)
	}
	if !strings.Contains(err.Error(), "audit visibility is unproven") {
		t.Fatalf("the rollback was not refused by its admission audit: %v", err)
	}
	created := flow.created(t)
	if len(created) != 1 || len(flow.vms.destroyed) != 0 {
		t.Fatalf("a refused rollback changed the VM: created=%v destroyed=%v", created, flow.vms.destroyed)
	}
	generation := flow.generation(t)
	if generation.State == aj.ReconciliationRequired || hasDisposalAdmission(generation) {
		t.Fatalf("a refused rollback left the generation %s (admission recorded: %t)", generation.State, hasDisposalAdmission(generation))
	}
	flow.vms.setAuditBlind(nil)
	flow.resumeAfterTimeout(t, generation, created[0])
}

// TestCreateVMResumeRefusesAHalfDisposedGeneration resumes the generation a
// failed rollback leaves behind. Every step of that attempt is observed, which
// used to let the resume clear reconciliation_required and carry on as a
// create on a VM the disposal had already begun to take apart. The recorded
// disposal admission now refuses the resume, even after the parker lock frees.
func TestCreateVMResumeRefusesAHalfDisposedGeneration(t *testing.T) {
	flow := newRollbackFlow(t)
	if _, err := flow.createVM(t); err == nil || flow.generation(t).State != aj.ReconciliationRequired {
		t.Fatalf("the rollback did not leave a half-disposed generation: %v", err)
	}
	locks := flow.locks
	locks.reset()

	_, err := flow.createVM(t)
	if err == nil {
		t.Fatal("create_vm resumed a half-disposed generation as a create")
	}
	if !strings.Contains(err.Error(), "resume after VM disposal admission") {
		t.Fatalf("the resume failed for another reason: %v", err)
	}
	if created := flow.created(t); len(created) != 1 {
		t.Fatalf("the refused resume built another VM: %v", created)
	}
	if state := flow.generation(t).State; state != aj.ReconciliationRequired {
		t.Fatalf("the refused resume left the generation %s", state)
	}
}

// TestCreateVMRollbackPreservationTimeoutIsUncertain drives the real
// create_vm handler into a persistent disk whose parker lock another request
// holds past the managed wait. create_vm rolls the attempt back, on the last
// attempt and with a fallback attempt left alike, and the holder never leaves,
// so preserving the disk that had already attached waits out the same parker
// lock and times out. By then the disposal has recorded its admission, so the
// generation is half disposed and must not be resumed as a create. The
// generation requires reconciliation, and the Director reads a failure it does
// not retry.
func TestCreateVMRollbackPreservationTimeoutIsUncertain(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		t.Run(fmt.Sprintf("fallback=%t", fallback), func(t *testing.T) {
			flow := newRollbackFlow(t)
			if fallback {
				limit := 1
				flow.parked.deps.Config.Placement.FallbackMax = &limit
			}
			assertUncertainRollback(t, flow)
		})
	}
}

func assertUncertainRollback(t *testing.T, flow *rollbackFlow) {
	t.Helper()
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

// TestVMCleanupFailureLeadsWithReconciliation covers a delete_vm disposal
// that fails after it was admitted. The generation requires reconciliation, so
// a Director retry would only meet a refusal. A retriable cleanup error goes
// behind the reconciliation error, so the first CPI error the Director reads
// is not retriable. A cleanup error that is already not retriable keeps its
// place, so the Director still shows its own message.
func TestVMCleanupFailureLeadsWithReconciliation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cause   error
		leading string
	}{
		{name: "retriable cleanup failure", cause: cpierrors.Retriable("VM destroy task timed out"), leading: "requires reconciliation at VM cleanup"},
		{name: "audit refusal", cause: cpierrors.Cloud("VM cleanup refused: 1 audit issue"), leading: "VM cleanup refused: 1 audit issue"},
		{name: "untyped cleanup failure", cause: errors.New("cleanup used unbounded volume destruction"), leading: "cleanup used unbounded volume destruction"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := createdManagedVM(t)
			err := managedVMCleanupFailure(m.handle, tc.cause)
			if !errors.Is(err, tc.cause) {
				t.Fatalf("the cleanup error left the chain: %v", err)
			}
			var typed *cpierrors.Error
			if !errors.As(err, &typed) || typed.Type() != cpierrors.TypeCloud || typed.OkToRetry() {
				t.Fatalf("the Director would read the failed disposal as retriable: %v", err)
			}
			if !strings.Contains(typed.Error(), tc.leading) {
				t.Fatalf("the Director would show %q, want it to name %q", typed.Error(), tc.leading)
			}
			if state := m.handle.Record().State; state != aj.ReconciliationRequired {
				t.Fatalf("the failed disposal left the generation %s", state)
			}
		})
	}
}
