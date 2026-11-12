package handlers

// parker_lock_wait_internal_test.go holds the fakes the parker lock wait tests
// share. They live in package handlers with exported names so the external
// tests in parker_lock_wait_test.go can use them, and the internal tests that
// need unexported handler functions or the stateful lifecycle fixture can too.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	nodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

// HeldParkerPools is a sentinel pool store that refuses a duplicate create
// with PVE's live verdict, so a claim another request holds is waited on.
type HeldParkerPools struct {
	mu    sync.Mutex
	pools map[string]string
}

func (p *HeldParkerPools) AddVM(context.Context, string, int64) error        { return nil }
func (p *HeldParkerPools) MoveVMToPool(context.Context, string, int64) error { return nil }
func (p *HeldParkerPools) PoolHasVM(context.Context, string, int64) (bool, error) {
	return false, nil
}

func (p *HeldParkerPools) CreatePool(_ context.Context, id, comment string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, taken := p.pools[id]; taken {
		return fmt.Errorf("API request failed: create pool failed: pool '%s' already exists", id)
	}
	p.pools[id] = comment
	return nil
}

func (p *HeldParkerPools) DeletePool(_ context.Context, id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.pools, id)
	return nil
}

func (p *HeldParkerPools) GetPoolComment(_ context.Context, id string) (string, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	comment, found := p.pools[id]
	return comment, found, nil
}

// ParkerWaitClock is the clock the parker lock's acquire runs on. It moves only
// when the acquire sleeps, so a production-sized wait costs no wall-clock time.
// Once it passes releaseAt, release runs once, which is the other request
// leaving its protection window.
type ParkerWaitClock struct {
	mu        sync.Mutex
	start     time.Time
	cur       time.Time
	releaseAt time.Duration
	release   func()
	released  bool
}

func NewParkerWaitClock(releaseAt time.Duration, release func()) *ParkerWaitClock {
	now := time.Now().Truncate(time.Second)
	return &ParkerWaitClock{start: now, cur: now, releaseAt: releaseAt, release: release}
}

func (c *ParkerWaitClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cur
}

func (c *ParkerWaitClock) Sleep(ctx context.Context, d time.Duration) error {
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

// Start is the instant the clock began at.
func (c *ParkerWaitClock) Start() time.Time { return c.start }

func (c *ParkerWaitClock) Elapsed() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cur.Sub(c.start)
}

// HeldParkerLock gives another CPI process the lock of every parker in
// parkerVMIDs, with a claim that outlives any wait, so a request can only get
// in when that holder leaves. The holder leaves after holdFor on the returned
// clock, or never when holdFor is zero. The returned context runs the parker
// lock's acquires on that clock.
func HeldParkerLock(holdFor time.Duration, parkerVMIDs ...int) (*HeldParkerPools, *ParkerWaitClock, context.Context) {
	pools := &HeldParkerPools{pools: map[string]string{}}
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
	clk := NewParkerWaitClock(holdFor, release)
	for i, sentinel := range sentinels {
		pools.pools[sentinel] = fmt.Sprintf("owner=delete_parked/%d@1-1 exp=%d",
			parkerVMIDs[i], clk.Now().Add(time.Hour).Unix())
	}
	return pools, clk, pve.WithParkerLockClockForTest(context.Background(), clk.Now, clk.Sleep)
}

// sentinelHeldPVE routes every bosh-lock- sentinel pool call of the client it
// wraps to a HeldParkerPools store, which another request holds, and leaves
// every other pool call on the wrapped client's own pool service.
type sentinelHeldPVE struct {
	pve.Client
	held *HeldParkerPools
}

func (c sentinelHeldPVE) Pools() pve.PoolService {
	return sentinelHeldPools{PoolService: c.Client.Pools(), held: c.held}
}

type sentinelHeldPools struct {
	pve.PoolService
	held *HeldParkerPools
}

func (p sentinelHeldPools) CreatePool(ctx context.Context, id, comment string) error {
	if strings.HasPrefix(id, reservedPoolLockPrefix) {
		return p.held.CreatePool(ctx, id, comment)
	}
	return p.PoolService.CreatePool(ctx, id, comment)
}

func (p sentinelHeldPools) DeletePool(ctx context.Context, id string) error {
	if strings.HasPrefix(id, reservedPoolLockPrefix) {
		return p.held.DeletePool(ctx, id)
	}
	return p.PoolService.DeletePool(ctx, id)
}

func (p sentinelHeldPools) GetPoolComment(ctx context.Context, id string) (string, bool, error) {
	if strings.HasPrefix(id, reservedPoolLockPrefix) {
		return p.held.GetPoolComment(ctx, id)
	}
	return p.PoolService.GetPoolComment(ctx, id)
}

// parkerBand lists every VMID in the parker band the lifecycle fixtures use.
func parkerBand() []int {
	band := make([]int, 0, 1000)
	for vmid := 90000; vmid <= 90999; vmid++ {
		band = append(band, vmid)
	}
	return band
}

// TestDetachDisk_StableIDNoJournal_WaitsOutALongHolder is the detach of a
// stable-ID disk that has no allocation journal record. It takes the transfer
// path, which locks the disk itself and then waits for a parker's lock.
// Another request holds the lock of every parker in the band for
// parkerLockHolderFor, and only the parker lock is held, so the per-disk lock
// is free. The transfer must wait its turn, and the disk must end up on a
// parker with 777 no longer carrying it.
func TestDetachDisk_StableIDNoJournal_WaitsOutALongHolder(t *testing.T) {
	t.Parallel()
	deps, client, _, _, _ := lifecycleFlowFixture(t)
	deps.PVE = legacyDestroyPVE{lifecycleFlowPVE: client, recorder: &legacyDestroyRecorder{}}
	deps.Logger = log.NewNopLogger()
	deps.Agent = legacyDestroyAgent{}
	deps.Config.DetachedDiskStrategy = "parked"
	deps.Config.StoragePlacementNamespace = ""
	deps.Config.StorageAllocationJournalDir = ""
	// Replace the fixture's journal-managed disk with a legacy disk that
	// carries a stable ID in its serial.
	delete(client.state.volumes, strings.Split(client.state.configs[777]["scsi1"].(string), ",")[0])
	token, err := pve.GenerateDiskStableID()
	if err != nil {
		t.Fatal(err)
	}
	const volume = "a:777/vm-777-disk-0.raw"
	cid, err := pve.EncodeDiskCID(volume, &pve.DiskCIDMeta{ID: token, Format: "raw"})
	if err != nil {
		t.Fatal(err)
	}
	client.state.volumes[volume] = &nodes.GetStorageContentResponse{Size: 5 << 30, Format: "raw"}
	client.state.configs[777]["scsi1"] = volume + ",serial=" + token + ",size=5G"

	pools, clk, ctx := HeldParkerLock(parkerLockHolderForInternal, parkerBand()...)
	deps.PVE = sentinelHeldPVE{Client: deps.PVE, held: pools}
	_, err = HandleDetachDisk(deps).Handle(ctx,
		[]json.RawMessage{planJSON(t, "777"), planJSON(t, cid)}, jsonrpc.Context{})
	if err != nil {
		t.Fatalf("detach_disk gave up on the parker's lock after %v instead of waiting out the holder: %v",
			clk.Elapsed(), err)
	}
	if waited := clk.Elapsed(); waited < parkerLockHolderForInternal {
		t.Fatalf("detach_disk took the parker's lock after %v, before the holder left at %v",
			waited, parkerLockHolderForInternal)
	}
	for key, value := range client.state.configs[777] {
		if text, ok := value.(string); ok && isDiskOptionKey(key) && strings.Contains(text, "serial="+token) {
			t.Fatalf("777 still carries the disk in %s after the detach", key)
		}
	}
	onParker := false
	for vmid, cfg := range client.state.configs {
		tags, _ := cfg["tags"].(string)
		for key, value := range cfg {
			if text, ok := value.(string); ok && isDiskOptionKey(key) && strings.Contains(text, "serial="+token) &&
				strings.Contains(tags, pve.ParkerTag) && vmid >= 90000 {
				onParker = true
			}
		}
	}
	if !onParker {
		t.Fatal("detach_disk returned success without parking the disk on a parker")
	}
}

// parkerLockHolderForInternal matches parkerLockHolderFor in the external
// tests, which this package cannot name.
const parkerLockHolderForInternal = 20 * time.Second

// parkedHAQEMU is the HA rig's QEMU service with a parker's detach, which the
// rig's own service refuses.
type parkedHAQEMU struct {
	*icQEMUService
	onDetach func(vmid int, slot string)
}

func (q *parkedHAQEMU) DetachDisk(_ context.Context, _ string, vmid int, slot string) error {
	q.onDetach(vmid, slot)
	return nil
}

// parkedHANodes is the HA rig's node service with the config write that
// re-asserts a parker's protection, which the rig's own service refuses.
type parkedHANodes struct{ *icNodesService }

func (parkedHANodes) UpdateQemuConfig(context.Context, string, string, *nodes.UpdateQemuConfigParams) error {
	return nil
}

// parkerRowsCluster adds the parker VM's index row to the HA rig's cluster,
// which lists only VM 100.
type parkerRowsCluster struct {
	*attachHAStub
}

func (c parkerRowsCluster) ListResources(ctx context.Context, params *sdkcluster.ListResourcesParams,
) (*sdkcluster.ListResourcesResponse, error) {
	resp, err := c.attachHAStub.ListResources(ctx, params)
	if err != nil {
		return nil, err
	}
	raw, _ := json.Marshal(map[string]any{"type": "qemu", "vmid": 90000, "node": "pve01", "id": "qemu/90000"})
	rows := append(sdkcluster.ListResourcesResponse{}, *resp...)
	rows = append(rows, raw)
	return &rows, nil
}

// TestAttachHARun_ParkedNoJournal_UnparkInTheBarrierWaitsOutALongHolder runs
// the part of attach_disk that takes the parker's lock inside the HA narrowing
// barrier. The disk is parked, has no allocation journal record, and sits on a
// storage the VM's HA pin does not record yet, so the attach narrows the pin.
// Another request holds the parker's lock for parkerLockHolderFor. The call
// is attachHA.run around guardAndUnparkBeforeAttach, which is the unpark step
// of attachDiskSteps, because a full attach_disk needs storage and task fakes
// that the HA rig does not have. The unpark must wait out the holder, and the
// HA resource must be back in the state it was in before the barrier.
func TestAttachHARun_ParkedNoJournal_UnparkInTheBarrierWaitsOutALongHolder(t *testing.T) {
	t.Parallel()
	const volid = "nfs-ab:vm-9001-disk-0"
	deps, stub, _ := attachRig(config.HAModeAuto)
	stub.recordPin("nfs")
	deps.Config.DetachedDiskStrategy = "parked"
	deps.Config.ParkedDiskVMIDRangeStart = 90000
	deps.Config.ParkedDiskVMIDRangeEnd = 90999
	pools, clk, ctx := HeldParkerLock(parkerLockHolderForInternal, 90000)
	client := deps.PVE.(*icPVEClient)
	client.poolsSvc = pools
	client.clusterSvc = parkerRowsCluster{attachHAStub: stub}
	var mu sync.Mutex
	detached := false
	client.qemuSvc = &parkedHAQEMU{
		icQEMUService: &icQEMUService{configFn: func(_ context.Context, _ string, vmid int) (map[string]any, error) {
			mu.Lock()
			defer mu.Unlock()
			if vmid == 90000 {
				cfg := map[string]any{"tags": "bosh-parker", "name": "bosh-parker-90000"}
				if !detached {
					cfg["scsi0"] = volid
				}
				return cfg, nil
			}
			// The target VM holds the disk once the unpark has released it.
			cfg := map[string]any{"scsi0": "nfs:vm-100-disk-0,size=10G"}
			if detached {
				cfg["scsi1"] = volid
			}
			return cfg, nil
		}},
		onDetach: func(vmid int, _ string) {
			mu.Lock()
			defer mu.Unlock()
			if vmid == 90000 {
				detached = true
			}
		},
	}

	h, res, _ := narrowingRunFor(t, deps, stub, resolvedDisk{volid: volid})
	var ran bool
	err := h.run(ctx, func(runCtx context.Context) (string, error) {
		if res.state != "ignored" {
			t.Errorf("the unpark runs while the resource is ignored, state %q", res.state)
		}
		rd := resolvedDisk{diskCID: "pvd-x", birth: volid, volid: volid}
		_, err := guardAndUnparkBeforeAttach(runCtx, h.deps, "attach_disk", &rd, "pve01", 100)
		ran = err == nil
		return "pve01", err
	})
	if err != nil {
		t.Fatalf("the attach inside the barrier gave up on the parker's lock after %v instead of waiting: %v",
			clk.Elapsed(), err)
	}
	if waited := clk.Elapsed(); waited < parkerLockHolderForInternal {
		t.Fatalf("the unpark took the parker's lock after %v, before the holder left at %v",
			waited, parkerLockHolderForInternal)
	}
	mu.Lock()
	defer mu.Unlock()
	if !ran || !detached {
		t.Fatalf("the unpark did not run to the end: ran=%t detached=%t", ran, detached)
	}
	if res.state != "started" || res.comment != stub.comment || res.gone {
		t.Fatalf("the HA resource is state %q comment %q gone %t, want it back to started with its comment",
			res.state, res.comment, res.gone)
	}
}

// narrowingRunFor is narrowingRun for a disk of the caller's choosing.
func narrowingRunFor(t *testing.T, deps Deps, stub *attachHAStub, disk resolvedDisk,
) (*attachHA, *attachRunRes, *attachMoveTaskClient) {
	t.Helper()
	mc := &attachMoveTaskClient{icPVEClient: deps.PVE.(*icPVEClient)}
	// The holder scan lists the VMs of each node, so the node service serves
	// the same rows as the cluster index.
	mc.nodesSvc = parkedHANodes{icNodesService: &icNodesService{
		listFn: mc.clusterSvc.ListResources, fallbackNode: "pve01", qemu: mc.QEMU,
	}}
	deps.PVE = mc
	h, err := decideAttachHA(context.Background(), deps, "100", disk)
	if err != nil || h == nil || h.action != attachHAGuarded || h.newPin == nil {
		t.Fatalf("decision: h %+v err %v, want a guarded narrowing", h, err)
	}
	res := &attachRunRes{log: &stagingLog{}, state: "started", comment: stub.comment}
	h.narrow = barrierView(res)
	return h, res, mc
}

// TestSweepContext_KeepsTheShortParkerLockWait pins that the deferred-teardown
// sweep still gives up on a busy parker after pve.ParkerLockSweepWait while
// every other caller waits a whole TTL. Another request holds the parker's
// lock and never leaves. The lock taken on the sweep's context must time out
// with the retriable lock timeout after at least the sweep's wait, well before
// the TTL, and the work under the lock must never run.
func TestSweepContext_KeepsTheShortParkerLockWait(t *testing.T) {
	t.Parallel()
	const parkerVMID = 90000
	pools, clk, ctx := HeldParkerLock(0, parkerVMID)
	sweepCtx, cancel := sweepContext(ctx)
	defer cancel()
	ran := false
	err := pve.RunUnderParkerLock(sweepCtx, &icPVEClient{poolsSvc: pools}, log.NewNopLogger(), parkerVMID, "test",
		func(context.Context) error {
			ran = true
			return nil
		})
	if !errors.Is(err, pve.ErrClusterLockTimeout) {
		t.Fatalf("the sweep's lock against a holder that never leaves = %v, want the cluster lock timeout", err)
	}
	if ran {
		t.Fatal("the work under the lock ran although the sweep never took the lock")
	}
	waited, ttl := clk.Elapsed(), pve.ParkerProtectionLockTTLNow()
	if waited < pve.ParkerLockSweepWait || waited >= ttl/2 {
		t.Fatalf("the sweep waited %v for the parker's lock, want about %v and far below its TTL of %v",
			waited, pve.ParkerLockSweepWait, ttl)
	}
}
