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
	t.Logf("record after the timed-out detach: %s (%s)", disk.record(t).State, disk.record(t).Reason)
	locks.reset()
	if err := detach(); err != nil {
		t.Fatalf("the detach retry failed: %v", err)
	}
	assertReturnedRecord(t, "retried detach", disk.record(t))
}
