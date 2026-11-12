// parker_lock_wait_test.go covers how long an attach_disk, detach_disk, or
// delete_disk of a disk without an allocation journal record waits for its
// parker's lock. Every disk on one parker shares that lock, so a clean-up that
// deletes many parked orphans at once queues them all behind it.
package handlers_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// heldParkerPools is a sentinel pool store that refuses a duplicate create
// with PVE's live verdict, so a claim another request holds is waited on.
type heldParkerPools struct {
	mu    sync.Mutex
	pools map[string]string
}

func (p *heldParkerPools) AddVM(context.Context, string, int64) error        { return nil }
func (p *heldParkerPools) MoveVMToPool(context.Context, string, int64) error { return nil }
func (p *heldParkerPools) PoolHasVM(context.Context, string, int64) (bool, error) {
	return false, nil
}

func (p *heldParkerPools) CreatePool(_ context.Context, id, comment string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, taken := p.pools[id]; taken {
		return fmt.Errorf("API request failed: create pool failed: pool '%s' already exists", id)
	}
	p.pools[id] = comment
	return nil
}

func (p *heldParkerPools) DeletePool(_ context.Context, id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.pools, id)
	return nil
}

func (p *heldParkerPools) GetPoolComment(_ context.Context, id string) (string, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	comment, found := p.pools[id]
	return comment, found, nil
}

// parkerWaitClock is the clock the parker lock's acquire runs on. It moves only
// when the acquire sleeps, so a production-sized wait costs no wall-clock time.
// Once it passes releaseAt, release runs once, which is the other request
// leaving its protection window.
type parkerWaitClock struct {
	mu        sync.Mutex
	start     time.Time
	cur       time.Time
	releaseAt time.Duration
	release   func()
	released  bool
}

func newParkerWaitClock(releaseAt time.Duration, release func()) *parkerWaitClock {
	now := time.Now().Truncate(time.Second)
	return &parkerWaitClock{start: now, cur: now, releaseAt: releaseAt, release: release}
}

func (c *parkerWaitClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cur
}

func (c *parkerWaitClock) sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	c.cur = c.cur.Add(d)
	due := c.release != nil && !c.released && c.cur.Sub(c.start) >= c.releaseAt
	if due {
		c.released = true
	}
	c.mu.Unlock()
	if due {
		c.release()
	}
	return nil
}

func (c *parkerWaitClock) elapsed() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cur.Sub(c.start)
}

// heldParkerLock gives another CPI process the lock of every parker in
// parkerVMIDs, with a claim that outlives any wait, so a request can only get
// in when that holder leaves. The holder leaves after holdFor on the returned
// clock, or never when holdFor is zero. The returned context runs the parker
// lock's acquires on that clock.
func heldParkerLock(holdFor time.Duration, parkerVMIDs ...int) (*heldParkerPools, *parkerWaitClock, context.Context) {
	pools := &heldParkerPools{pools: map[string]string{}}
	sentinels := make([]string, 0, len(parkerVMIDs))
	for _, vmid := range parkerVMIDs {
		sentinels = append(sentinels, pve.ClusterLockPoolName(fmt.Sprintf("vm-%d", vmid)))
	}
	var release func()
	if holdFor > 0 {
		release = func() {
			for _, sentinel := range sentinels {
				_ = pools.DeletePool(context.Background(), sentinel)
			}
		}
	}
	clk := newParkerWaitClock(holdFor, release)
	for i, sentinel := range sentinels {
		pools.pools[sentinel] = fmt.Sprintf("owner=delete_parked/%d@1-1 exp=%d",
			parkerVMIDs[i], clk.now().Add(time.Hour).Unix())
	}
	return pools, clk, pve.WithParkerLockClockForTest(context.Background(), clk.now, clk.sleep)
}

// parkerLockHolderFor is how long the other request holds the parker's lock
// in the tests that wait it out. It is longer than the lock's old 15-second
// default wait and well inside its TTL.
const parkerLockHolderFor = 20 * time.Second

// assertWaitedOutHolder fails the test unless the call succeeded after the
// holder left.
func assertWaitedOutHolder(t *testing.T, call string, err error, waited time.Duration) {
	t.Helper()
	if ttl := pve.ParkerProtectionLockTTLNow(); parkerLockHolderFor >= ttl {
		t.Fatalf("the holder's %v must be shorter than the lock's TTL of %v", parkerLockHolderFor, ttl)
	}
	if err != nil {
		t.Fatalf("%s gave up on the parker's lock after %v instead of waiting out the holder: %v",
			call, waited, err)
	}
	if waited < parkerLockHolderFor {
		t.Fatalf("%s took the lock after %v, before the holder left at %v", call, waited, parkerLockHolderFor)
	}
}

// parkedDeleteUnderHeldLock runs one delete_disk of a parked disk with no
// journal record while another request holds the parker's lock for holdFor,
// or for good when holdFor is zero. It returns whether the parker's detach
// ran, how long the delete spent waiting on the lock's clock, and the
// handler's error.
func parkedDeleteUnderHeldLock(t *testing.T, holdFor time.Duration) (bool, time.Duration, error) {
	t.Helper()
	pools, clk, ctx := heldParkerLock(holdFor, reparkParkerVMID)
	detached := false
	qemuSvc := &mockQEMUService{
		configFn: func(_ context.Context, _ string, vmid int) (map[string]any, error) {
			if vmid != reparkParkerVMID {
				return map[string]any{}, nil
			}
			cfg := map[string]any{"tags": "bosh-cpi;bosh-parker", "protection": true}
			if !detached {
				cfg["scsi0"] = reparkVolid + ",serial=" + reparkStableID + ",size=10G"
			}
			return cfg, nil
		},
		detachDiskFn: func(_ context.Context, _ string, _ int, _ string) error {
			detached = true
			return nil
		},
	}
	deps := reparkDeps(&mockPVEClient{
		storageSvc: &mockStorageService{}, qemuSvc: qemuSvc, clusterSvc: reparkClusterSvc(), poolsSvc: pools,
	})
	_, err := handlers.HandleDeleteDisk(deps).Handle(ctx,
		[]json.RawMessage{marshal(reparkDiskCID(t))}, jsonrpc.Context{})
	return detached, clk.elapsed(), err
}

// TestHandleDeleteDisk_ParkedNoJournal_WaitsOutALongHolder is the clean-up
// case. Another delete holds the parker's lock for parkerLockHolderFor. A
// delete of a disk without a journal record must wait its turn and then delete
// the disk, as a journal-managed delete always has.
func TestHandleDeleteDisk_ParkedNoJournal_WaitsOutALongHolder(t *testing.T) {
	t.Parallel()
	detached, waited, err := parkedDeleteUnderHeldLock(t, parkerLockHolderFor)
	assertWaitedOutHolder(t, "delete_disk", err, waited)
	if !detached {
		t.Fatal("delete_disk returned success without detaching the disk from its parker")
	}
}

// TestHandleDeleteDisk_ParkedNoJournal_WaitIsBoundedByTheTTL keeps the wait
// bounded. A holder that never leaves still times the delete out once the
// lock's TTL has passed, with the retriable lock timeout and the disk left in
// place.
func TestHandleDeleteDisk_ParkedNoJournal_WaitIsBoundedByTheTTL(t *testing.T) {
	t.Parallel()
	detached, waited, err := parkedDeleteUnderHeldLock(t, 0)
	if !errors.Is(err, pve.ErrClusterLockTimeout) {
		t.Fatalf("delete_disk against a holder that never leaves = %v, want the cluster lock timeout", err)
	}
	if detached {
		t.Fatal("delete_disk detached the disk without holding the parker's lock")
	}
	ttl := pve.ParkerProtectionLockTTLNow()
	if waited < ttl || waited > ttl+time.Minute {
		t.Fatalf("delete_disk waited %v for the parker's lock, want about its TTL of %v", waited, ttl)
	}
}

// TestHandleAttachDisk_ParkedNoJournal_WaitsOutALongHolder is the attach of a
// parked disk without a journal record while another request holds the
// parker's lock for parkerLockHolderFor. The unpark must wait its turn, and the
// attach then completes.
func TestHandleAttachDisk_ParkedNoJournal_WaitsOutALongHolder(t *testing.T) {
	t.Parallel()
	const bareCID = parkedVolid
	parkerVMCfg := map[string]any{parkerSlot: bareCID, "tags": "bosh-parker", "name": "bosh-parker-90000"}
	targetVMCfgEmpty := map[string]any{}
	qemuSvc := &attachQEMUService{
		attachReturnDiskID: "scsi1",
		configCfgs: []map[string]any{
			targetVMCfgEmpty, parkerVMCfg, parkerVMCfg, parkerVMCfg, parkerVMCfg, parkerVMCfg,
			targetVMCfgEmpty, {"scsi1": bareCID},
		},
	}
	pools, clk, ctx := heldParkerLock(parkerLockHolderFor, 90000)
	deps := handlers.Deps{
		Config: parkedCfg(),
		PVE:    &mockPVEClient{qemuSvc: qemuSvc, clusterSvc: parkerClusterSvc(), poolsSvc: pools},
		Agent:  &captureAgent{},
		Logger: log.NewNopLogger(),
	}
	_, err := handlers.HandleAttachDisk(deps).Handle(ctx, attachArgs(t, "100", bareCID), jsonrpc.Context{})
	assertWaitedOutHolder(t, "attach_disk", err, clk.elapsed())
	if len(qemuSvc.detachCalls) != 1 || qemuSvc.detachCalls[0] != parkerSlot {
		t.Fatalf("expected the unpark to detach %q from the parker; got %v", parkerSlot, qemuSvc.detachCalls)
	}
	if qemuSvc.attachLastVolid != bareCID {
		t.Fatalf("attach volid = %q, want %q", qemuSvc.attachLastVolid, bareCID)
	}
}

// TestHandleDetachDisk_ParkedNoJournal_WaitsOutALongHolder is the detach of a
// disk without a journal record into a parker whose lock another request holds
// for parkerLockHolderFor. The park must wait its turn and then attach the
// disk to the parker.
func TestHandleDetachDisk_ParkedNoJournal_WaitsOutALongHolder(t *testing.T) {
	t.Parallel()
	const volid = "local-lvm:vm-9001-disk-0"
	var parkedVolid string
	qemuSvc := &parkerQEMUService{
		sourceVMID:      100,
		parkerVMIDStart: 90000,
		sourceCfg:       map[string]any{diskSlot: volid},
		parkerCfg:       map[string]any{"tags": "bosh-parker"},
		attachDiskFn: func(_ context.Context, _ string, _ int, vol, _ string, _ *qemu.AttachOpts) (string, error) {
			parkedVolid = vol
			return "", nil
		},
	}
	// The detach creates a fresh parker, so another request holds the lock of
	// every parker in the band.
	band := make([]int, 0, 1000)
	for vmid := 90000; vmid <= 90999; vmid++ {
		band = append(band, vmid)
	}
	pools, clk, ctx := heldParkerLock(parkerLockHolderFor, band...)
	deps := detachDepsParked(qemuSvc, parkerEmptyClusterSvc())
	deps.PVE.(*mockPVEClient).poolsSvc = pools
	_, err := handlers.HandleDetachDisk(deps).Handle(ctx, detachArgs(t, "100", volid), jsonrpc.Context{})
	assertWaitedOutHolder(t, "detach_disk", err, clk.elapsed())
	if parkedVolid != volid {
		t.Fatalf("park attach volid = %q, want %q", parkedVolid, volid)
	}
}
