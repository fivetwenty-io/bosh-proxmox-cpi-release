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
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
)

// contendedFlowPVE is one request's cluster in the lifecycle flow fixture. Its
// sentinel pools live on the store two requests share, and its disk moves can
// be held at a gate so the request stays inside its parker window while the
// other one contends.
type contendedFlowPVE struct {
	*lifecycleFlowPVE
	locks *lockContention
	gate  <-chan struct{}
	// enter, when set, runs as a disk move reaches PVE, after the guard has
	// admitted it, and moveErr, when set, is what that move then returns.
	enter   func()
	moveErr error
	// onSnapshots and onConfig, when set, see a VM's snapshot listing and a
	// VM's config read.
	onSnapshots func(vmid int)
	onConfig    func(ctx context.Context, vmid int)
}

func (c contendedFlowPVE) QEMU() qemu.Service {
	return watchedFlowQEMU{Service: c.lifecycleFlowPVE.QEMU(), onSnapshots: c.onSnapshots, onConfig: c.onConfig}
}

// watchedFlowQEMU reports snapshot listings and config reads to the test.
type watchedFlowQEMU struct {
	qemu.Service
	onSnapshots func(vmid int)
	onConfig    func(ctx context.Context, vmid int)
}

func (q watchedFlowQEMU) ListSnapshots(ctx context.Context, node string, vmid int) ([]map[string]interface{}, error) {
	if q.onSnapshots != nil {
		q.onSnapshots(vmid)
	}
	return q.Service.ListSnapshots(ctx, node, vmid)
}

func (q watchedFlowQEMU) Config(ctx context.Context, node string, vmid int) (map[string]interface{}, error) {
	if q.onConfig != nil {
		q.onConfig(ctx, vmid)
	}
	return q.Service.Config(ctx, node, vmid)
}

func (c contendedFlowPVE) Pools() pve.PoolService {
	return contendedPools{PoolService: c.lifecycleFlowPVE.Pools(), locks: c.locks}
}

func (c contendedFlowPVE) Nodes() nodes.Service {
	return gatedFlowNodes{lifecycleFlowNodes: lifecycleFlowNodes{managedDiskTestNodes: managedDiskTestNodes{state: c.state}, c: c.lifecycleFlowPVE}, gate: c.gate, enter: c.enter, moveErr: c.moveErr}
}

type gatedFlowNodes struct {
	lifecycleFlowNodes
	gate    <-chan struct{}
	enter   func()
	moveErr error
}

func (n gatedFlowNodes) CreateQemuMoveDisk(ctx context.Context, node, vmid string, p *nodes.CreateQemuMoveDiskParams) (*nodes.CreateQemuMoveDiskResponse, error) {
	if n.enter != nil {
		n.enter()
	}
	if n.moveErr != nil {
		return nil, n.moveErr
	}
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

// testLockPoll is the cluster lock poll the lock-wait tests use. It is short
// enough that a wait of a tenth of a second still covers many polls.
const testLockPoll = 5 * time.Millisecond

// testManagedLockWait is the managed lock wait most lock-wait tests run out.
// At testLockPoll it still covers about twenty polls.
const testManagedLockWait = 150 * time.Millisecond

// withShortLockPoll returns ctx with the test's short cluster lock poll.
func withShortLockPoll(ctx context.Context) context.Context {
	return pve.WithClusterLockPollForTest(ctx, testLockPoll)
}

// shortenManagedLockWait returns ctx with the managed lock wait lowered to d
// and the short cluster lock poll. Both ride the context, so nothing another
// test reads changes.
func shortenManagedLockWait(ctx context.Context, d time.Duration) context.Context {
	return withShortLockPoll(context.WithValue(ctx, managedLockWaitKey{}, d))
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
	ctx := shortenManagedLockWait(t.Context(), testManagedLockWait)

	err := disk.attach(ctx)
	if err == nil || !errors.Is(err, pve.ErrClusterLockTimeout) || !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("want the retriable lock timeout, got %v", err)
	}
	assertReturnedRecord(t, "timed-out", disk.record(t))

	locks.reset()
	if err := disk.attach(ctx); err != nil {
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
	ctx := shortenManagedLockWait(t.Context(), testManagedLockWait)

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
	ctx := shortenManagedLockWait(t.Context(), testManagedLockWait)

	detach := func() error {
		_, err := HandleDetachDisk(disk.deps).Handle(ctx, []json.RawMessage{json.RawMessage(`"777"`), json.RawMessage(fmt.Sprintf("%q", disk.cid))}, jsonrpc.Context{})
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

// defaultDriveOverrides is the drive-option override set every managed
// persistent disk carries by default, as its parker records it.
var defaultDriveOverrides = map[string]string{"discard": "on", "iothread": "1", "ssd": "1"}

// assertOverlayNoteObserved checks that the attach wrote the overrides onto VM
// 777 before it waited, and that the write was journaled as an observed step.
func assertOverlayNoteObserved(t *testing.T, disk *parkedFlowDisk, record aj.Record) {
	t.Helper()
	if overlay := overlayOn(t, disk); !strings.Contains(overlay, `"ssd":"1"`) {
		t.Fatalf("the attach did not write the overrides before it waited: %s", overlay)
	}
	for i := range record.Steps {
		if record.Steps[i].Kind == "lifecycle_attach_disk_Nodes_UpdateQemuConfig" && record.Steps[i].State == aj.Observed && record.Steps[i].Target.VMID == 777 {
			return
		}
	}
	t.Fatalf("the overlay write was not journaled as an observed step: %+v", record.Steps)
}

// TestManagedAttachOverlayBeforeTimeoutReturnsTheDisk drives the default
// attach from a parker. The parked disk carries the default drive-option
// overrides, so the attach writes them onto the receiving VM before it waits
// for the parker lock. That note is journaled and observed, but it leaves the
// disk where it was, so when the wait runs out the allocation is returned, the
// Director gets the retriable timeout, and the retry attaches the disk.
func TestManagedAttachOverlayBeforeTimeoutReturnsTheDisk(t *testing.T) {
	locks := newLockContention(t)
	disk := newParkedFlowDisk(t, locks)
	locks.reset()
	withParkedOverlay(t, disk, defaultDriveOverrides)
	plantHeldParkerLock(locks, disk.parker)
	ctx := shortenManagedLockWait(t.Context(), testManagedLockWait)

	err := disk.attach(ctx)
	if !isDiskReturnedAfterLockTimeout(err) || !errors.Is(err, pve.ErrClusterLockTimeout) || !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("want the retriable lock timeout with the returned-disk marker, got %v", err)
	}
	record := disk.record(t)
	assertOverlayNoteObserved(t, disk, record)
	assertReturnedRecord(t, "timed-out", record)

	locks.reset()
	if err := disk.attach(ctx); err != nil {
		t.Fatalf("the retry after the timeout failed: %v", err)
	}
	assertReturnedRecord(t, "retried", disk.record(t))
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
// one rather than the 15-second default, the timeout came only after that wait
// ran out, and the disk's record is returned.
func assertCleanDiskTimeout(t *testing.T, disk *parkedFlowDisk, err error, elapsed, wait time.Duration) {
	t.Helper()
	if !isDiskReturnedAfterLockTimeout(err) || !errors.Is(err, pve.ErrClusterLockTimeout) || !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("want the retriable lock timeout with the returned-disk marker, got %v", err)
	}
	if elapsed >= 10*time.Second {
		t.Fatalf("the wait took %s, so the managed wait was not applied", elapsed)
	}
	if elapsed < wait {
		t.Fatalf("the timeout came after %s, before the %s wait ran out", elapsed, wait)
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
	ctx := shortenManagedLockWait(t.Context(), testManagedLockWait)

	rd := resolveFlowDisk(t, disk)
	started := time.Now()
	_, err := attachManagedPersistentDisk(ctx, disk.deps, "777", "n1", 777, rd)
	assertCleanDiskTimeout(t, disk, err, time.Since(started), testManagedLockWait)
}

// TestCreateVMPreAttachOverlayBeforeTimeoutReturnsTheDisk is the create_vm
// disk_cids pre-attach of a disk carrying the default drive-option overrides.
// The overlay note it writes onto the receiving VM before the wait does not
// count as a disk mutation, so the timeout still returns the disk cleanly.
func TestCreateVMPreAttachOverlayBeforeTimeoutReturnsTheDisk(t *testing.T) {
	locks := newLockContention(t)
	disk := newParkedFlowDisk(t, locks)
	locks.reset()
	withParkedOverlay(t, disk, defaultDriveOverrides)
	plantHeldParkerLock(locks, disk.parker)
	ctx := shortenManagedLockWait(t.Context(), testManagedLockWait)

	rd := resolveFlowDisk(t, disk)
	started := time.Now()
	_, err := attachManagedPersistentDisk(ctx, disk.deps, "777", "n1", 777, rd)
	assertCleanDiskTimeout(t, disk, err, time.Since(started), testManagedLockWait)
	assertOverlayNoteObserved(t, disk, disk.record(t))
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
	ctx := shortenManagedLockWait(t.Context(), testManagedLockWait)

	rd := resolveFlowDisk(t, disk)
	started := time.Now()
	err := detachManagedPersistentForVMDeleteOne(ctx, disk.deps, "n1", 777, rd, nil)
	assertCleanDiskTimeout(t, disk, err, time.Since(started), testManagedLockWait)
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

// TestManagedLockWaitDoesNotGrowTheRecord waits out a live holder for half a
// second, which at the test poll is dozens of polls. Only the first refused
// create reaches PVE and the journal, so the disk record grows by one step
// however many polls the wait makes.
func TestManagedLockWaitDoesNotGrowTheRecord(t *testing.T) {
	locks := newLockContention(t)
	disk := newParkedFlowDisk(t, locks)
	locks.reset()
	plantHeldParkerLock(locks, disk.parker)
	ctx := shortenManagedLockWait(t.Context(), 500*time.Millisecond)
	before, err := json.Marshal(disk.record(t))
	if err != nil {
		t.Fatal(err)
	}

	if err := disk.attach(ctx); !errors.Is(err, pve.ErrClusterLockTimeout) {
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
		t.Fatalf("a wait of many polls journaled %d sentinel creates and sent PVE %d, want one of each", creates, rejections)
	}
	if growth := len(after) - len(before); growth > 2048 {
		t.Fatalf("a wait of many polls grew the record by %d bytes", growth)
	}
}

// legacyPreservationFixture is a VM allocation in delete, holding a legacy
// stable-ID disk that no allocation owns, with the parker the preservation
// will use pinned so a test can hold its lock.
func legacyPreservationFixture(t *testing.T, locks *lockContention) (Deps, *lifecycleFlowPVE, *aj.Handle, int, string) {
	t.Helper()
	deps, client, journal, id, _ := lifecycleFlowFixture(t)
	deps.PVE = contendedFlowPVE{lifecycleFlowPVE: client, locks: locks}
	deps.Config.DetachedDiskStrategy = "parked"
	parker := deps.Config.ParkedDiskVMIDRangeStartValue()
	client.state.configs[parker] = map[string]any{"name": fmt.Sprintf("bosh-parker-%d", parker), "tags": "bosh-parker", "protection": 1, "scsihw": "virtio-scsi-pci", "digest": "1"}
	record, err := journal.Inspect(id)
	if err != nil {
		t.Fatal(err)
	}
	old := strings.Split(client.state.configs[777]["scsi1"].(string), ",")[0]
	volume := "a:123/vm-123-disk-0.raw"
	token, err := pve.GenerateDiskStableID()
	if err != nil {
		t.Fatal(err)
	}
	cid, err := pve.EncodeDiskCID(volume, &pve.DiskCIDMeta{ID: token, Format: "raw"})
	if err != nil {
		t.Fatal(err)
	}
	client.state.volumes[volume] = client.state.volumes[old]
	delete(client.state.volumes, old)
	client.state.configs[777]["scsi1"] = volume + ",serial=" + token + ",size=5G"
	pve.UpdateAttachedDiskCID(t.Context(), client, deps.Log(t.Context()), "n1", 777, token, cid)
	handle, err := journal.AcquireVM(t.Context(), "vm-agent", record.Intent)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := handle.Close(); err != nil {
			t.Error(err)
		}
	})
	step, err := storageMutationIntent(handle, "vm_create", aj.Target{Node: "n1", VMID: 777}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := storageMutationObserved(handle, step, nil, false); err != nil {
		t.Fatal(err)
	}
	vm := handle.Record()
	vm.State = aj.Observed
	vm.CID = "777"
	if err := handle.Save(vm); err != nil {
		t.Fatal(err)
	}
	return deps, client, handle, parker, cid
}

// TestDeleteVMLegacyPreservationUsesTheManagedWait covers delete_vm's legacy
// branch, which preserves a stable-ID disk that no allocation owns. It takes
// the same parker lock, so it gets the managed wait. A wait that runs out
// before the preservation touched the disk leaves the VM allocation settled,
// and delete_vm's cleanup rule does not mark it uncertain.
func TestDeleteVMLegacyPreservationUsesTheManagedWait(t *testing.T) {
	locks := newLockContention(t)
	deps, client, handle, parker, _ := legacyPreservationFixture(t, locks)
	plantHeldParkerLock(locks, parker)
	ctx := shortenManagedLockWait(t.Context(), testManagedLockWait)

	started := time.Now()
	err := detachManagedPersistentForVMDelete(ctx, deps, "n1", 777, nil, handle)
	elapsed := time.Since(started)
	if !isDiskReturnedAfterLockTimeout(err) || !errors.Is(err, pve.ErrClusterLockTimeout) || !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("want the retriable lock timeout with the returned-disk marker, got %v", err)
	}
	if elapsed >= 10*time.Second {
		t.Fatalf("the wait took %s, so the managed wait was not applied", elapsed)
	}
	if elapsed < testManagedLockWait {
		t.Fatalf("the timeout came after %s, before the %s wait ran out", elapsed, testManagedLockWait)
	}
	if client.moves != 0 || !strings.Contains(fmt.Sprint(client.state.configs[777]["scsi1"]), "vm-123-disk-0") {
		t.Fatalf("the preservation moved the disk before its wait ran out: moves=%d", client.moves)
	}
	if final := managedVMCleanupFailure(handle, err); !isDiskReturnedAfterLockTimeout(final) {
		t.Fatalf("delete_vm's cleanup rule lost the marker: %v", final)
	}
	record := handle.Record()
	if record.State == aj.ReconciliationRequired {
		t.Fatalf("a clean legacy preservation timeout demanded reconciliation: %s", record.Reason)
	}
	for i := range record.Steps {
		if record.Steps[i].State != aj.Observed {
			t.Fatalf("step %s (%s) left %s", record.Steps[i].ID, record.Steps[i].Kind, record.Steps[i].State)
		}
	}
}

// TestCreateVMLegacyAttachReturnsTheDiskOnATimeout covers create_vm's attach
// of a legacy stable-ID disk from its parker. A wait that runs out before the
// attach touched the disk hands back the returned-disk marker, so
// attachPersistent settles its step instead of poisoning the VM allocation,
// and the VM record is not marked uncertain.
func TestCreateVMLegacyAttachReturnsTheDiskOnATimeout(t *testing.T) {
	locks := newLockContention(t)
	deps, client, handle, parker, cid := legacyPreservationFixture(t, locks)
	if err := detachManagedPersistentForVMDelete(t.Context(), deps, "n1", 777, nil, handle); err != nil {
		t.Fatalf("parking the legacy disk: %v", err)
	}
	moves := client.moves
	locks.reset()
	plantHeldParkerLock(locks, parker)
	ctx := shortenManagedLockWait(t.Context(), testManagedLockWait)

	bare, meta, err := decodeDiskCID(ctx, deps, "create_vm", cid)
	if err != nil {
		t.Fatal(err)
	}
	disk, err := resolveDiskForOp(ctx, deps, "create_vm", cid, bare, meta)
	if err != nil {
		t.Fatal(err)
	}
	_, err = attachExistingDiskToManagedVM(ctx, deps, handle, disk, "n1", 777)
	if !isDiskReturnedAfterLockTimeout(err) || !errors.Is(err, pve.ErrClusterLockTimeout) {
		t.Fatalf("want the lock timeout with the returned-disk marker, got %v", err)
	}
	if client.moves != moves {
		t.Fatal("the attach moved the disk before its wait ran out")
	}
	if record := handle.Record(); record.State == aj.ReconciliationRequired {
		t.Fatalf("a clean legacy attach timeout demanded reconciliation: %s", record.Reason)
	}
}

// failConfirmingReads makes the parker lock's confirming reads fail until the
// acquire gives up, while the reads around them answer. The guard observes an
// accepted create without reading it back, so every read after the create
// until the way out is a confirming read. A read answers only when its
// context carries a deadline. In this flow the reads on the lock code's way out are the only
// bounded ones, because they run on its own detached context with a timeout,
// while the request context under test has no deadline and the managed wait
// rides a context value rather than a deadline. So the way out can prove the
// sentinel ours and delete it. If that ever stops being true, the confirming
// reads answer too, the acquire takes the lock, and the test fails loudly on
// the missing unknown state rather than passing for the wrong reason.
func failConfirmingReads(locks *lockContention) {
	createdReads := -1
	locks.afterCreate = func(string) { createdReads = 0 }
	locks.plainRead = func(ctx context.Context, _ string) error {
		if createdReads < 0 {
			return nil
		}
		createdReads++
		if _, bounded := ctx.Deadline(); bounded {
			return nil
		}
		return errors.New("connection reset by peer")
	}
}

// TestManagedAttachUnknownLockStateReturnsTheAllocation covers an attach whose
// parker lock create landed but whose confirming reads failed up to the
// deadline. The acquire cannot tell whether it holds the lock, so it fails
// with the unknown lock state. The read on its way out proves the sentinel
// ours and the guarded delete answers, so nothing the allocation owns changed
// and every step is observed. The allocation goes back to ready_to_return, the
// error is retriable, the sentinel is gone, and the retry completes.
func TestManagedAttachUnknownLockStateReturnsTheAllocation(t *testing.T) {
	locks := newLockContention(t)
	disk := newParkedFlowDisk(t, locks)
	locks.reset()
	failConfirmingReads(locks)
	ctx := shortenManagedLockWait(t.Context(), 120*time.Millisecond)

	err := disk.attach(ctx)
	if err == nil || !errors.Is(err, pve.ErrClusterLockStateUnknown) || !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("want the retriable unknown lock state, got %v", err)
	}
	assertReturnedRecord(t, "unknown-state", disk.record(t))
	locks.mu.Lock()
	sentinels := len(locks.pools)
	locks.mu.Unlock()
	if sentinels != 0 {
		t.Fatalf("the proven sentinel was left standing: %v", locks.pools)
	}

	locks.reset()
	if err := disk.attach(ctx); err != nil {
		t.Fatalf("the retry after the unknown lock state failed: %v", err)
	}
	assertReturnedRecord(t, "retried", disk.record(t))
}

// TestManagedAttachUnknownLockStateUnansweredDeleteIsSettledNextCall covers the
// same attach when the delete on the way out does not answer. That delete's
// step stays planned and the guard is poisoned, so the allocation needs
// reconciliation. The next attach reads the sentinel back, settles the planned
// delete step, and is admitted. Our old claim still stands, so that attach
// waits it out and times out cleanly, and once the claim has lapsed the
// following attach completes.
func TestManagedAttachUnknownLockStateUnansweredDeleteIsSettledNextCall(t *testing.T) {
	locks := newLockContention(t)
	disk := newParkedFlowDisk(t, locks)
	locks.reset()
	failConfirmingReads(locks)
	locks.deleteErr = errors.New("connection reset by peer")
	ctx := shortenManagedLockWait(t.Context(), 120*time.Millisecond)

	if err := disk.attach(ctx); !errors.Is(err, pve.ErrClusterLockStateUnknown) {
		t.Fatalf("want the unknown lock state, got %v", err)
	}
	record := disk.record(t)
	if record.State != aj.ReconciliationRequired {
		t.Fatalf("allocation state = %s, want %s", record.State, aj.ReconciliationRequired)
	}
	last := record.Steps[len(record.Steps)-1]
	if last.Kind != "lifecycle_attach_disk_Pool_DeletePool" || last.State != aj.Planned {
		t.Fatalf("last step = %s (%s), want a planned lifecycle_attach_disk_Pool_DeletePool", last.Kind, last.State)
	}
	sentinel := pve.ClusterLockPoolName(fmt.Sprintf("vm-%d", disk.parker))
	locks.mu.Lock()
	claim, standing := locks.pools[sentinel]
	locks.plainRead, locks.deleteErr, locks.afterCreate = nil, nil, nil
	locks.mu.Unlock()
	if !standing {
		t.Fatal("the sentinel is gone although its delete never answered")
	}

	err := disk.attach(ctx)
	if !errors.Is(err, pve.ErrClusterLockTimeout) || !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("the next attach should be admitted and wait out our old claim, got %v", err)
	}
	record = disk.record(t)
	assertReturnedRecord(t, "settled", record)
	for i := range record.Steps {
		if record.Steps[i].Kind == last.Kind && record.Steps[i].ID == last.ID && record.Steps[i].State != aj.Observed {
			t.Fatalf("the planned delete step was not settled: %s", record.Steps[i].State)
		}
	}

	locks.mu.Lock()
	if owner, ok := strings.CutPrefix(strings.Fields(claim)[0], "owner="); ok {
		locks.pools[sentinel] = fmt.Sprintf("owner=%s exp=%d", owner, time.Now().Add(-time.Minute).Unix())
	}
	locks.mu.Unlock()
	if err := disk.attach(ctx); err != nil {
		t.Fatalf("the attach after our old claim lapsed failed: %v", err)
	}
	assertReturnedRecord(t, "completed", disk.record(t))
}

// sentinelCount reports how many sentinels the shared store holds.
func sentinelCount(locks *lockContention) int {
	locks.mu.Lock()
	defer locks.mu.Unlock()
	return len(locks.pools)
}

// assertCleanCancellation checks what a clean exit on an ended request owes
// the Director: a retriable error and the disk's allocation returned with
// every step observed, which also shows the guard was never poisoned.
func assertCleanCancellation(t *testing.T, disk *parkedFlowDisk, err error, signal error) {
	t.Helper()
	if !errors.Is(err, signal) || !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("want a retriable error carrying %v, got %v", signal, err)
	}
	assertReturnedRecord(t, "cancelled", disk.record(t))
}

// TestManagedAttachCancelledDuringTheConfirmReturnsTheAllocation cancels the
// request while its parker lock is confirming a create whose reads fail. The
// acquire cannot tell whether it holds the lock and gives up. Its way out runs
// on a detached context, reads the sentinel, proves it ours, and deletes it,
// and the guard admits that delete although the request has ended. Nothing
// else was admitted, so the allocation is returned with a retriable error and
// the sentinel is gone.
func TestManagedAttachCancelledDuringTheConfirmReturnsTheAllocation(t *testing.T) {
	locks := newLockContention(t)
	disk := newParkedFlowDisk(t, locks)
	locks.reset()
	failConfirmingReads(locks)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	failing := locks.plainRead
	locks.plainRead = func(readCtx context.Context, pool string) error {
		err := failing(readCtx, pool)
		if err != nil {
			cancel()
		}
		return err
	}

	err := disk.attach(ctx)
	assertCleanCancellation(t, disk, err, pve.ErrClusterLockStateUnknown)
	if n := sentinelCount(locks); n != 0 {
		t.Fatalf("the proven sentinel was left standing: %v", locks.pools)
	}
}

// TestManagedAttachCancelledWhileWaitingReturnsTheAllocation cancels the
// request while its parker lock waits behind another request's live claim.
// The wait ends interrupted, the window never runs, and the allocation is
// returned with a retriable error. The holder's claim is untouched.
func TestManagedAttachCancelledWhileWaitingReturnsTheAllocation(t *testing.T) {
	locks := newLockContention(t)
	disk := newParkedFlowDisk(t, locks)
	locks.reset()
	plantHeldParkerLock(locks, disk.parker)
	sentinel := pve.ClusterLockPoolName(fmt.Sprintf("vm-%d", disk.parker))
	holder := locks.pools[sentinel]
	// The wait stays long so the cancel always lands before the deadline could.
	ctx, cancel := context.WithCancel(shortenManagedLockWait(t.Context(), 5*time.Second))
	defer cancel()
	rejected := locks.rejected
	go func() {
		select {
		case <-rejected:
			cancel()
		case <-ctx.Done():
		}
	}()

	err := disk.attach(ctx)
	assertCleanCancellation(t, disk, err, pve.ErrClusterLockInterrupted)
	locks.mu.Lock()
	defer locks.mu.Unlock()
	if locks.pools[sentinel] != holder {
		t.Fatalf("the holder's claim was disturbed: %q", locks.pools[sentinel])
	}
}

// TestManagedAttachCancelledInsideTheWindowReturnsTheAllocation cancels the
// request as its parker lock confirms the claim for the last time, so the
// window opens on an ended request. The guard refuses the window's first
// mutation before it reaches PVE, without poisoning, and still admits the
// release of our sentinel. Nothing was admitted, so the allocation is returned
// with a retriable error and the sentinel is gone.
func TestManagedAttachCancelledInsideTheWindowReturnsTheAllocation(t *testing.T) {
	locks := newLockContention(t)
	disk := newParkedFlowDisk(t, locks)
	locks.reset()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	reads := -1
	locks.afterCreate = func(string) { reads = 0 }
	locks.plainRead = func(context.Context, string) error {
		if reads < 0 {
			return nil
		}
		reads++
		// The two confirming reads come right after the create, and the
		// second of them is the last read before the window opens.
		if reads == 2 {
			cancel()
		}
		return nil
	}

	err := disk.attach(ctx)
	assertCleanCancellation(t, disk, err, errManagedRequestEnded)
	if n := sentinelCount(locks); n != 0 {
		t.Fatalf("our sentinel was not released: %v", locks.pools)
	}
}

// TestManagedAttachCancelledAfterAnAdmittedMoveStaysUncertain is the other
// side. The guard admitted the disk move before the request ended, and the
// move then failed on the ended request. A mutation that was admitted may
// have changed the disk, so the guard is poisoned and the allocation needs
// reconciliation, exactly as before.
func TestManagedAttachCancelledAfterAnAdmittedMoveStaysUncertain(t *testing.T) {
	locks := newLockContention(t)
	disk := newParkedFlowDisk(t, locks)
	locks.reset()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	disk.deps.PVE = contendedFlowPVE{lifecycleFlowPVE: disk.client, locks: locks, gate: make(chan struct{}), enter: cancel}

	if err := disk.attach(ctx); err == nil {
		t.Fatal("an attach whose admitted move failed reported success")
	}
	if record := disk.record(t); record.State != aj.ReconciliationRequired {
		t.Fatalf("allocation state = %s, want %s", record.State, aj.ReconciliationRequired)
	}
}

// TestManagedAttachLockCreateReadFailureStaysClean covers a read of the parker
// sentinel that fails right after PVE accepted our create. That read used to
// be the guard's readback, which poisoned the allocation with "cannot verify
// lifecycle lock mutation" although the create had happened. The acquire's
// own confirming read now makes it, retries on the lock's poll cadence, and
// takes the lock once a read answers, so the attach completes and the record
// stays clean. The poll is cut to a millisecond so the retry costs no real
// wait.
func TestManagedAttachLockCreateReadFailureStaysClean(t *testing.T) {
	ctx := pve.WithClusterLockPollForTest(t.Context(), time.Millisecond)
	locks := newLockContention(t)
	disk := newParkedFlowDisk(t, locks)
	locks.reset()
	failed := -1
	locks.afterCreate = func(string) {
		if failed < 0 {
			failed = 0
		}
	}
	locks.plainRead = func(context.Context, string) error {
		if failed != 0 {
			return nil
		}
		failed = 1
		return errors.New("connection reset by peer")
	}

	if err := disk.attach(ctx); err != nil {
		t.Fatalf("a failed read after an accepted create failed the attach: %v", err)
	}
	if failed != 1 {
		t.Fatal("the read after the create never failed, so the test proves nothing")
	}
	assertReturnedRecord(t, "attached", disk.record(t))
}

// TestManagedAttachPoisonedWindowReleasesItsSentinel covers a window whose disk
// move fails after the guard admitted it. The guard is poisoned and the
// allocation needs reconciliation, as before. The window's own sentinel is
// still released, because the release reads it back, finds exactly our claim,
// and deletes it, so the next request on this parker does not wait out a
// whole TTL behind one uncertain request.
func TestManagedAttachPoisonedWindowReleasesItsSentinel(t *testing.T) {
	locks := newLockContention(t)
	disk := newParkedFlowDisk(t, locks)
	locks.reset()
	disk.deps.PVE = contendedFlowPVE{lifecycleFlowPVE: disk.client, locks: locks, moveErr: errors.New("move task failed")}

	if err := disk.attach(t.Context()); err == nil {
		t.Fatal("an attach whose move failed reported success")
	}
	if record := disk.record(t); record.State != aj.ReconciliationRequired {
		t.Fatalf("allocation state = %s, want %s", record.State, aj.ReconciliationRequired)
	}
	if n := sentinelCount(locks); n != 0 {
		t.Fatalf("the poisoned request left its sentinel standing: %v", locks.pools)
	}
}

// TestManagedAttachEndedBeforeTheLockCreateFailsTheCall ends the request just
// before its parker lock create. The guard refuses that create as not
// attempted, and the call fails there instead of running the parker window
// unserialized, where every write would be refused anyway. Nothing was
// admitted, so the allocation is returned with a retriable error.
func TestManagedAttachEndedBeforeTheLockCreateFailsTheCall(t *testing.T) {
	locks := newLockContention(t)
	disk := newParkedFlowDisk(t, locks)
	locks.reset()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ended := false
	windowReads := 0
	disk.deps.PVE = contendedFlowPVE{
		lifecycleFlowPVE: disk.client, locks: locks,
		// The attach lists the VM's snapshots right before it moves the
		// disk, which is the last read before the parker lock create.
		onSnapshots: func(vmid int) {
			if vmid == 777 {
				ended = true
				cancel()
			}
		},
		// The window's first call reads the parker's config on the ended
		// request's context. The completion that returns the allocation
		// reads it too, but on its own detached context.
		onConfig: func(ctx context.Context, vmid int) {
			if ctx.Err() != nil && vmid == disk.parker {
				windowReads++
			}
		},
	}

	err := disk.attach(ctx)
	if !ended {
		t.Fatal("the attach never listed the VM's snapshots, so the test proves nothing")
	}
	if windowReads != 0 {
		t.Fatalf("the parker window ran on the ended request: %d parker config reads", windowReads)
	}
	assertCleanCancellation(t, disk, err, errManagedRequestEnded)
	if n := sentinelCount(locks); n != 0 {
		t.Fatalf("a sentinel was created on the ended request: %v", locks.pools)
	}
}
