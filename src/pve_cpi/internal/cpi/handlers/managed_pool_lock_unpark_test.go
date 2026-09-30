package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

// contendedFlowPVE is one request's cluster in the lifecycle flow fixture. Its
// sentinel pools live on the store two requests share, and its disk moves can
// be held at a gate so the request stays inside its parker window while the
// other one contends.
type contendedFlowPVE struct {
	*lifecycleFlowPVE
	locks *lockContention
	gate  <-chan struct{}
}

func (c contendedFlowPVE) Pools() pve.PoolService {
	return contendedPools{PoolService: c.lifecycleFlowPVE.Pools(), locks: c.locks}
}

func (c contendedFlowPVE) Nodes() nodes.Service {
	return gatedFlowNodes{lifecycleFlowNodes: lifecycleFlowNodes{managedDiskTestNodes: managedDiskTestNodes{state: c.state}, c: c.lifecycleFlowPVE}, gate: c.gate}
}

type gatedFlowNodes struct {
	lifecycleFlowNodes
	gate <-chan struct{}
}

func (n gatedFlowNodes) CreateQemuMoveDisk(ctx context.Context, node, vmid string, p *nodes.CreateQemuMoveDiskParams) (*nodes.CreateQemuMoveDiskResponse, error) {
	if n.gate != nil {
		select {
		case <-n.gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return n.lifecycleFlowNodes.CreateQemuMoveDisk(ctx, node, vmid, p)
}

// parkedFlowDisk is one journal-managed disk that a detach has parked.
type parkedFlowDisk struct {
	deps    Deps
	client  *lifecycleFlowPVE
	journal *aj.Journal
	id, cid string
	parker  int
	gate    chan struct{}
}

// newParkedFlowDisk attaches a fresh managed disk to VM 777 and detaches it
// again under the parked strategy, which leaves it on the fixture's parker.
func newParkedFlowDisk(t *testing.T, locks *lockContention) *parkedFlowDisk {
	t.Helper()
	deps, client, journal, id, cid := lifecycleFlowFixtureState(t, true)
	d := &parkedFlowDisk{deps: deps, client: client, journal: journal, id: id, cid: cid, gate: make(chan struct{})}
	close(d.gate)
	d.deps.PVE = contendedFlowPVE{lifecycleFlowPVE: client, locks: locks, gate: d.gate}
	delete(client.state.configs[777], "scsi1")
	// Every fixture gets the same parker, so two requests contend for one
	// parker's sentinel exactly as two instances of a deployment do.
	d.deps.Config.DetachedDiskStrategy = "parked"
	parker := d.deps.Config.ParkedDiskVMIDRangeStartValue()
	client.state.configs[parker] = map[string]any{"name": fmt.Sprintf("bosh-parker-%d", parker), "tags": "bosh-parker", "protection": 1, "scsihw": "virtio-scsi-pci", "digest": "1"}
	if err := d.attach(t.Context()); err != nil {
		t.Fatalf("setup attach: %v", err)
	}
	d.deps.Config.DetachedDiskStrategy = "parked"
	if _, err := HandleDetachDisk(d.deps).Handle(t.Context(), []json.RawMessage{planJSON(t, "777"), planJSON(t, cid)}, jsonrpc.Context{}); err != nil {
		t.Fatalf("setup detach into the parker: %v", err)
	}
	for vmid, cfg := range client.state.configs {
		if tags, _ := cfg["tags"].(string); strings.Contains(tags, "bosh-parker") {
			d.parker = vmid
		}
	}
	if d.parker == 0 {
		t.Fatal("the detach left no parker")
	}
	return d
}

func (d *parkedFlowDisk) gateMoves(gate chan struct{}) {
	d.gate = gate
	d.deps.PVE = contendedFlowPVE{lifecycleFlowPVE: d.client, locks: d.deps.PVE.(contendedFlowPVE).locks, gate: gate}
}

func (d *parkedFlowDisk) attach(ctx context.Context) error {
	args := []json.RawMessage{json.RawMessage(`"777"`), json.RawMessage(fmt.Sprintf("%q", d.cid))}
	_, err := HandleAttachDisk(d.deps).Handle(ctx, args, jsonrpc.Context{})
	return err
}

func (d *parkedFlowDisk) record(t *testing.T) aj.Record {
	t.Helper()
	record, err := d.journal.Inspect(d.id)
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func assertReturnedRecord(t *testing.T, name string, record aj.Record) {
	t.Helper()
	if record.State != aj.ReadyToReturn {
		t.Fatalf("%s allocation state = %s (reason %q), want %s", name, record.State, record.Reason, aj.ReadyToReturn)
	}
	for i := range record.Steps {
		if record.Steps[i].State != aj.Observed {
			t.Fatalf("%s step %s (%s) left %s", name, record.Steps[i].ID, record.Steps[i].Kind, record.Steps[i].State)
		}
	}
}

// TestManagedAttachWaitsOutAHeldParkerLock is the path the incident took: two
// attach_disk calls moving journal-managed disks out of one parker at once.
// The loser's sentinel create is refused with PVE's live duplicate verdict, and
// it must wait for the holder and then attach, leaving both allocations
// returned with every step observed.
func TestManagedAttachWaitsOutAHeldParkerLock(t *testing.T) {
	locks := newLockContention(t)
	holder := newParkedFlowDisk(t, locks)
	waiter := newParkedFlowDisk(t, locks)
	if holder.parker != waiter.parker {
		t.Fatalf("parkers differ: %d and %d", holder.parker, waiter.parker)
	}
	locks.reset()
	gate := make(chan struct{})
	holder.gateMoves(gate)

	var wg sync.WaitGroup
	var holderErr, waiterErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		holderErr = holder.attach(t.Context())
	}()
	waitFor(t, locks.held, "the holder to take the parker lock")
	locks.waitNextSecond()
	wg.Add(1)
	go func() {
		defer wg.Done()
		waiterErr = waiter.attach(t.Context())
	}()
	waitFor(t, locks.rejected, "the waiter's sentinel create to be refused")
	close(gate)
	wg.Wait()

	if holderErr != nil {
		t.Fatalf("holder attach_disk failed: %v", holderErr)
	}
	if waiterErr != nil {
		t.Fatalf("waiter attach_disk failed instead of waiting for the lock: %v", waiterErr)
	}
	assertReturnedRecord(t, "holder", holder.record(t))
	record := waiter.record(t)
	assertReturnedRecord(t, "waiter", record)
	creates := 0
	for _, step := range record.Steps {
		if step.Kind == "lifecycle_attach_disk_Pool_CreatePool" {
			creates++
		}
	}
	if creates < 2 {
		t.Fatalf("waiter journaled %d sentinel creates, want the refused one and the winning one", creates)
	}
	locks.mu.Lock()
	defer locks.mu.Unlock()
	if locks.rejections == 0 || len(locks.pools) != 0 {
		t.Fatalf("contention not exercised or sentinel left: rejections=%d pools=%v", locks.rejections, locks.pools)
	}
}

// plantHeldParkerLock gives another request a live claim on the parker.
func plantHeldParkerLock(locks *lockContention, parker int) {
	locks.mu.Lock()
	defer locks.mu.Unlock()
	locks.pools[pve.ClusterLockPoolName(fmt.Sprintf("vm-%d", parker))] = fmt.Sprintf("owner=transfer_out/%d@1-1 exp=%d", parker, time.Now().Add(time.Hour).Unix())
}

// shortenManagedLockWait lowers the managed lock wait for one test.
func shortenManagedLockWait(t *testing.T, d time.Duration) {
	t.Helper()
	previous := managedLockWait
	managedLockWait = d
	t.Cleanup(func() { managedLockWait = previous })
}

// TestManagedAttachLockTimeoutReturnsTheAllocation runs out the wait against a
// holder that never leaves. Nothing changed, so the allocation goes back to
// ready_to_return, the Director gets the retriable timeout, and the retry is
// admitted and completes once the lock frees.
func TestManagedAttachLockTimeoutReturnsTheAllocation(t *testing.T) {
	locks := newLockContention(t)
	disk := newParkedFlowDisk(t, locks)
	locks.reset()
	plantHeldParkerLock(locks, disk.parker)
	shortenManagedLockWait(t, 1500*time.Millisecond)

	err := disk.attach(t.Context())
	if err == nil || !errors.Is(err, pve.ErrClusterLockTimeout) || !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("want the retriable lock timeout, got %v", err)
	}
	assertReturnedRecord(t, "timed-out", disk.record(t))

	locks.reset()
	if err := disk.attach(t.Context()); err != nil {
		t.Fatalf("the retry after the timeout failed: %v", err)
	}
	assertReturnedRecord(t, "retried", disk.record(t))
}

// TestManagedLifecycleTimeoutRules drives lifecycle.finish directly with a lock
// timeout. A settled record comes back returned, while a timeout that follows a
// step the operation journaled but never observed, or that follows a poisoned
// guard, goes uncertain.
func TestManagedLifecycleTimeoutRules(t *testing.T) {
	timeout := cpierrors.WrapAs(errors.Join(errors.New("held"), pve.ErrClusterLockTimeout), cpierrors.TypeRetriableCloud, "AcquireClusterLock: timed out")
	for _, tc := range []struct {
		name    string
		prepare func(t *testing.T, local Deps, lifecycle *managedDiskLifecycle)
		err     error
		want    aj.State
	}{
		{name: "settled timeout", err: timeout, want: aj.ReadyToReturn},
		{name: "unsettled mutation", err: timeout, want: aj.ReconciliationRequired, prepare: func(t *testing.T, _ Deps, lifecycle *managedDiskLifecycle) {
			if _, err := lifecycle.session.Intent("unsettled_probe", aj.Target{Node: "n1"}); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "poisoned guard", err: timeout, want: aj.ReconciliationRequired, prepare: func(t *testing.T, _ Deps, lifecycle *managedDiskLifecycle) {
			if lifecycle.guard.Poison(nil) == nil {
				t.Fatal("the guard did not record the poison")
			}
		}},
		{name: "other failure", err: cpierrors.Cloud("transfer failed"), want: aj.ReconciliationRequired},
		{name: "disk mutation before the timeout", err: timeout, want: aj.ReconciliationRequired, prepare: func(t *testing.T, local Deps, _ *managedDiskLifecycle) {
			// A real guarded write to the disk's holder lands and settles
			// before the wait runs out. Every step is observed, yet the disk
			// has been touched, so the timeout must not return the allocation.
			protect := true
			if err := local.PVE.Nodes().UpdateQemuConfig(t.Context(), "n1", "777", &nodes.UpdateQemuConfigParams{Protection: &protect}); err != nil {
				t.Fatalf("guarded holder write: %v", err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deps, _, journal, id, cid := lifecycleFlowFixtureState(t, true, true)
			bare, meta, err := decodeDiskCID(t.Context(), deps, "attach_disk", cid)
			if err != nil {
				t.Fatal(err)
			}
			rd, err := resolveDiskForOp(t.Context(), deps, "attach_disk", cid, bare, meta)
			if err != nil {
				t.Fatal(err)
			}
			local, lifecycle, err := managedDiskOperation(t.Context(), deps, rd, "attach_disk")
			if err != nil || lifecycle == nil {
				t.Fatalf("lifecycle admission: %v", err)
			}
			if tc.prepare != nil {
				tc.prepare(t, local, lifecycle)
			}
			result := lifecycle.finish(t.Context(), tc.err, false)
			if !errors.Is(result, tc.err) {
				t.Fatalf("finish lost the operation error: %v", result)
			}
			record, err := journal.Inspect(id)
			if err != nil {
				t.Fatal(err)
			}
			if record.State != tc.want {
				t.Fatalf("record state = %s (reason %q), want %s", record.State, record.Reason, tc.want)
			}
		})
	}
}

// TestManagedLockTimeoutSurvivesTheAttachWrappers checks the error finish
// really receives. attachDiskViaTransfer wraps the transfer's failure with the
// CPI's own error types, and the timeout sentinel must still be visible
// through them, or a clean timeout would never be recognized.
func TestManagedLockTimeoutSurvivesTheAttachWrappers(t *testing.T) {
	locks := newLockContention(t)
	disk := newParkedFlowDisk(t, locks)
	locks.reset()
	plantHeldParkerLock(locks, disk.parker)
	shortenManagedLockWait(t, 1500*time.Millisecond)

	ctx := t.Context()
	bare, meta, err := decodeDiskCID(ctx, disk.deps, "attach_disk", disk.cid)
	if err != nil {
		t.Fatal(err)
	}
	rd, err := resolveDiskForOp(ctx, disk.deps, "attach_disk", disk.cid, bare, meta)
	if err != nil || rd.holder == nil {
		t.Fatalf("resolving the parked disk: %v", err)
	}
	local, lifecycle, err := managedDiskOperation(ctx, disk.deps, rd, "attach_disk")
	if err != nil || lifecycle == nil {
		t.Fatalf("lifecycle admission: %v", err)
	}
	ctx = managedLockWaitContext(ctx)
	_, _, opErr := attachDiskViaTransfer(ctx, local, "attach_disk", "777", "n1", 777, disk.cid, lifecycle.disk, attachPlan{viaTransfer: true, parker: *rd.holder}, "scsi1", nil)
	if !errors.Is(opErr, pve.ErrClusterLockTimeout) || !cpierrors.IsType(opErr, cpierrors.TypeRetriableCloud) {
		t.Fatalf("the attach wrappers hid the lock timeout: %v", opErr)
	}
	if result := lifecycle.finish(ctx, opErr, false); !errors.Is(result, pve.ErrClusterLockTimeout) {
		t.Fatalf("finish lost the timeout: %v", result)
	}
	assertReturnedRecord(t, "timed-out", disk.record(t))
}

// TestManagedDetachLockTimeoutKeepsTheTimeout runs a parked detach against a
// held parker lock. The Director must see the retriable timeout, and the
// Director's retry must be admitted and complete once the lock frees.
func TestManagedDetachLockTimeoutKeepsTheTimeout(t *testing.T) {
	locks := newLockContention(t)
	disk := newParkedFlowDisk(t, locks)
	locks.reset()
	if err := disk.attach(t.Context()); err != nil {
		t.Fatalf("attach before the detach: %v", err)
	}
	plantHeldParkerLock(locks, disk.parker)
	shortenManagedLockWait(t, 1500*time.Millisecond)

	detach := func() error {
		_, err := HandleDetachDisk(disk.deps).Handle(t.Context(), []json.RawMessage{json.RawMessage(`"777"`), json.RawMessage(fmt.Sprintf("%q", disk.cid))}, jsonrpc.Context{})
		return err
	}
	err := detach()
	if !errors.Is(err, pve.ErrClusterLockTimeout) || !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("detach did not hand back the retriable lock timeout: %v", err)
	}
	// The parked detach waits for the parker lock before it touches the disk,
	// so the timeout returns the allocation exactly as it was.
	assertReturnedRecord(t, "timed-out detach", disk.record(t))
	locks.reset()
	if err := detach(); err != nil {
		t.Fatalf("the detach retry failed: %v", err)
	}
	assertReturnedRecord(t, "retried detach", disk.record(t))
}

// TestManagedAttachOverlayBeforeTimeoutStaysUncertain drives the attach
// ordering the mutation gate exists for. The parked disk carries drive-option
// overrides, so the attach writes them onto the receiving VM before it waits
// for the parker lock. That write is a real, observed change to the disk's
// holders, so when the wait runs out the allocation must go uncertain rather
// than snap back.
func TestManagedAttachOverlayBeforeTimeoutStaysUncertain(t *testing.T) {
	locks := newLockContention(t)
	disk := newParkedFlowDisk(t, locks)
	locks.reset()
	withParkedOverlay(t, disk, map[string]string{"discard": "on", "ssd": "1"})
	plantHeldParkerLock(locks, disk.parker)
	shortenManagedLockWait(t, 1500*time.Millisecond)

	err := disk.attach(t.Context())
	if !errors.Is(err, pve.ErrClusterLockTimeout) {
		t.Fatalf("want the lock timeout, got %v", err)
	}
	if overlay := overlayOn(t, disk); !strings.Contains(overlay, `"ssd":"1"`) {
		t.Fatalf("the attach did not write the overrides before it waited: %s", overlay)
	}
	record := disk.record(t)
	if record.State != aj.ReconciliationRequired {
		t.Fatalf("a timeout after an observed overlay write left the allocation %s", record.State)
	}
	observedWrite := false
	for i := range record.Steps {
		if record.Steps[i].Kind == "lifecycle_attach_disk_Nodes_UpdateQemuConfig" && record.Steps[i].State == aj.Observed && record.Steps[i].Target.VMID == 777 {
			observedWrite = true
		}
	}
	if !observedWrite {
		t.Fatalf("the overlay write was not journaled as an observed step: %+v", record.Steps)
	}
}

// resolveFlowDisk resolves a flow fixture disk the way the handlers do.
func resolveFlowDisk(t *testing.T, disk *parkedFlowDisk) resolvedDisk {
	t.Helper()
	bare, meta, err := decodeDiskCID(t.Context(), disk.deps, "create_vm", disk.cid)
	if err != nil {
		t.Fatal(err)
	}
	rd, err := resolveDiskForOp(t.Context(), disk.deps, "create_vm", disk.cid, bare, meta)
	if err != nil {
		t.Fatal(err)
	}
	return rd
}

// assertCleanDiskTimeout checks what a caller holding its own allocation
// relies on after a disk operation waited out a parker lock. The error carries
// the returned-disk marker and the retriable timeout, the wait was the managed
// one rather than the 15-second default, and the disk's record is returned.
func assertCleanDiskTimeout(t *testing.T, disk *parkedFlowDisk, err error, elapsed time.Duration) {
	t.Helper()
	if !isDiskReturnedAfterLockTimeout(err) || !errors.Is(err, pve.ErrClusterLockTimeout) || !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("want the retriable lock timeout with the returned-disk marker, got %v", err)
	}
	if elapsed >= 10*time.Second {
		t.Fatalf("the wait took %s, so the managed wait was not applied", elapsed)
	}
	assertReturnedRecord(t, "timed-out", disk.record(t))
}

// TestCreateVMPreAttachUsesTheManagedWait covers create_vm's disk_cids
// pre-attach, which unparks through the same parker lock as attach_disk.
func TestCreateVMPreAttachUsesTheManagedWait(t *testing.T) {
	locks := newLockContention(t)
	disk := newParkedFlowDisk(t, locks)
	locks.reset()
	plantHeldParkerLock(locks, disk.parker)
	shortenManagedLockWait(t, 1500*time.Millisecond)

	rd := resolveFlowDisk(t, disk)
	started := time.Now()
	_, err := attachManagedPersistentDisk(t.Context(), disk.deps, "777", "n1", 777, rd)
	assertCleanDiskTimeout(t, disk, err, time.Since(started))
}

// TestDeleteVMPreservationUsesTheManagedWait covers delete_vm's
// preserve_disk, which parks the disk through the same parker lock as
// detach_disk.
func TestDeleteVMPreservationUsesTheManagedWait(t *testing.T) {
	locks := newLockContention(t)
	disk := newParkedFlowDisk(t, locks)
	locks.reset()
	if err := disk.attach(t.Context()); err != nil {
		t.Fatalf("attach before the preservation: %v", err)
	}
	plantHeldParkerLock(locks, disk.parker)
	shortenManagedLockWait(t, 1500*time.Millisecond)

	rd := resolveFlowDisk(t, disk)
	started := time.Now()
	err := detachManagedPersistentForVMDeleteOne(t.Context(), disk.deps, "n1", 777, rd, nil)
	assertCleanDiskTimeout(t, disk, err, time.Since(started))
}

// createdManagedVM is a VM allocation whose root exists, ready for the
// post-create steps.
func createdManagedVM(t *testing.T) *managedVMAllocation {
	t.Helper()
	m, _, _, _ := newManagedVMGuardCase(t, managedVMGuardCase{})
	guarded := m.deps
	guarded.PVE = m.guard.Client()
	if err := createManagedVMRoot(t.Context(), guarded, m.parsed, m.shape, m.prepared.plan.Targets[0], 101, m.marker); err != nil {
		t.Fatal(err)
	}
	return m
}

func withDiskAttachOutcome(t *testing.T, outcome error) {
	t.Helper()
	previous := attachExistingDiskForVM
	attachExistingDiskForVM = func(context.Context, Deps, *aj.Handle, resolvedDisk, string, int) (string, error) {
		return "", outcome
	}
	t.Cleanup(func() { attachExistingDiskForVM = previous })
}

func lockTimeoutError() error {
	return cpierrors.WrapAs(errors.Join(errors.New("held"), pve.ErrClusterLockTimeout), cpierrors.TypeRetriableCloud, "AcquireClusterLock: timed out")
}

// TestVMAllocationSettlesACleanDiskTimeout is create_vm's side of the same
// wait-out. The disk came back unchanged, so the VM's handoff step is settled,
// the VM guard stays clean, and the post-create failure goes back retriable
// without marking the VM allocation uncertain.
func TestVMAllocationSettlesACleanDiskTimeout(t *testing.T) {
	m := createdManagedVM(t)
	withDiskAttachOutcome(t, &diskReturnedAfterLockTimeout{err: lockTimeoutError()})

	err := m.attachPersistent(t.Context(), resolvedDisk{diskCID: "pvd-example"})
	if !isDiskReturnedAfterLockTimeout(err) || m.guard.Err() != nil {
		t.Fatalf("a clean disk timeout poisoned the VM allocation: err=%v guard=%v", err, m.guard.Err())
	}
	if final := m.postCreateFailure(err); !errors.Is(final, pve.ErrClusterLockTimeout) || !cpierrors.IsType(final, cpierrors.TypeRetriableCloud) {
		t.Fatalf("the post-create failure lost the retriable timeout: %v", final)
	}
	record := m.handle.Record()
	if record.State == aj.ReconciliationRequired {
		t.Fatalf("a clean disk timeout demanded reconciliation: %s", record.Reason)
	}
	found := false
	for i := range record.Steps {
		if record.Steps[i].State != aj.Observed {
			t.Fatalf("step %s (%s) left %s", record.Steps[i].ID, record.Steps[i].Kind, record.Steps[i].State)
		}
		found = found || strings.HasPrefix(record.Steps[i].Kind, "vm.persistent.")
	}
	if !found {
		t.Fatal("the VM's handoff step was never journaled")
	}
}

// TestVMAllocationPoisonsOnAnUncertainDiskFailure is the negative case. A disk
// failure that is not a clean wait-out still poisons the VM allocation.
func TestVMAllocationPoisonsOnAnUncertainDiskFailure(t *testing.T) {
	for name, outcome := range map[string]error{
		"bare timeout without the marker": lockTimeoutError(),
		"other disk failure":              cpierrors.Cloud("transfer failed"),
	} {
		t.Run(name, func(t *testing.T) {
			m := createdManagedVM(t)
			withDiskAttachOutcome(t, outcome)
			err := m.attachPersistent(t.Context(), resolvedDisk{diskCID: "pvd-example"})
			if err == nil || m.guard.Err() == nil {
				t.Fatalf("an uncertain disk failure left the VM guard clean: %v", err)
			}
			if record := m.handle.Record(); record.State != aj.ReconciliationRequired {
				t.Fatalf("an uncertain disk failure left the VM allocation %s", record.State)
			}
		})
	}
}

// TestVMCleanupFailureRules covers delete_vm's side. A preservation that
// waited out the parker lock leaves nothing uncertain while every step is
// observed, and any other failure marks the VM allocation uncertain.
func TestVMCleanupFailureRules(t *testing.T) {
	m := createdManagedVM(t)
	returned := &diskReturnedAfterLockTimeout{err: lockTimeoutError()}
	if err := managedVMCleanupFailure(m.handle, returned); !isDiskReturnedAfterLockTimeout(err) || m.handle.Record().State == aj.ReconciliationRequired {
		t.Fatalf("a clean preservation timeout marked the VM uncertain: %v %s", err, m.handle.Record().State)
	}
	if err := managedVMCleanupFailure(m.handle, cpierrors.Cloud("preservation failed")); err == nil || m.handle.Record().State != aj.ReconciliationRequired {
		t.Fatalf("an uncertain preservation failure left the VM %s", m.handle.Record().State)
	}
}

// TestManagedLockWaitDoesNotGrowTheRecord waits out a live holder for several
// seconds, which is several polls. Only the first refused create reaches PVE
// and the journal, so the disk record grows by one step however long the wait
// runs.
func TestManagedLockWaitDoesNotGrowTheRecord(t *testing.T) {
	locks := newLockContention(t)
	disk := newParkedFlowDisk(t, locks)
	locks.reset()
	plantHeldParkerLock(locks, disk.parker)
	shortenManagedLockWait(t, 5*time.Second)
	before, err := json.Marshal(disk.record(t))
	if err != nil {
		t.Fatal(err)
	}

	if err := disk.attach(t.Context()); !errors.Is(err, pve.ErrClusterLockTimeout) {
		t.Fatalf("want the lock timeout, got %v", err)
	}
	record := disk.record(t)
	after, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	creates := 0
	for i := range record.Steps {
		if record.Steps[i].Kind == "lifecycle_attach_disk_Pool_CreatePool" {
			creates++
		}
	}
	locks.mu.Lock()
	rejections := locks.rejections
	locks.mu.Unlock()
	if creates != 1 || rejections != 1 {
		t.Fatalf("a 5-second wait journaled %d sentinel creates and sent PVE %d, want one of each", creates, rejections)
	}
	if growth := len(after) - len(before); growth > 2048 {
		t.Fatalf("a 5-second wait grew the record by %d bytes", growth)
	}
}
