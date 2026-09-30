package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

// livePoolVerdict builds the error a live PVE 9 cluster returns for a pool
// verdict. Pool.pm raises the verdict inside lock_user_config, which re-raises
// it as "<errmsg>: <verdict>\n"; the JSON formatter carries that text in the
// body's message field under HTTP 500; the SDK wraps the parsed APIError.
func livePoolVerdict(t *testing.T, message string) error {
	t.Helper()
	body, err := json.Marshal(map[string]any{"data": nil, "message": message + "\n"})
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Errorf("API request failed: %w", sdkerrors.ParseAPIError(500, body))
}

// lockContention is the cluster-wide sentinel store two CPI processes share.
// It answers exactly the way PVE does: a duplicate create and a delete of a
// missing pool both fail with the live 500 verdicts.
type lockContention struct {
	t          *testing.T
	mu         sync.Mutex
	pools      map[string]string
	creates    int
	rejections int
	heldAt     time.Time
	held       chan struct{}
	rejected   chan struct{}
	heldOnce   sync.Once
	rejectOnce sync.Once
}

func newLockContention(t *testing.T) *lockContention {
	return &lockContention{t: t, pools: map[string]string{}, held: make(chan struct{}), rejected: make(chan struct{})}
}

// contendedPools routes every bosh-lock- sentinel call to the shared store and
// leaves every other pool call on the fixture's own pool service.
type contendedPools struct {
	pve.PoolService
	locks *lockContention
}

func (p contendedPools) CreatePool(ctx context.Context, id, comment string) error {
	if !strings.HasPrefix(id, reservedPoolLockPrefix) {
		return p.PoolService.CreatePool(ctx, id, comment)
	}
	l := p.locks
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, taken := l.pools[id]; taken {
		l.rejections++
		l.rejectOnce.Do(func() { close(l.rejected) })
		return livePoolVerdict(l.t, "create pool failed: pool '"+id+"' already exists")
	}
	l.pools[id] = comment
	l.creates++
	l.heldOnce.Do(func() {
		l.heldAt = time.Now()
		close(l.held)
	})
	return nil
}

func (p contendedPools) DeletePool(ctx context.Context, id string) error {
	if !strings.HasPrefix(id, reservedPoolLockPrefix) {
		return p.PoolService.DeletePool(ctx, id)
	}
	l := p.locks
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, taken := l.pools[id]; !taken {
		return livePoolVerdict(l.t, "delete pool failed: pool '"+id+"' does not exist")
	}
	delete(l.pools, id)
	return nil
}

func (p contendedPools) GetPoolComment(ctx context.Context, id string) (string, bool, error) {
	if !strings.HasPrefix(id, reservedPoolLockPrefix) {
		return p.PoolService.GetPoolComment(ctx, id)
	}
	p.locks.mu.Lock()
	defer p.locks.mu.Unlock()
	comment, found := p.locks.pools[id]
	return comment, found, nil
}

// gatedQEMU holds an attach until the test opens the gate, which keeps the
// holder inside its protection window while the other request contends.
type gatedQEMU struct {
	qemu.Service
	gate <-chan struct{}
}

func (q gatedQEMU) AttachDisk(ctx context.Context, node string, vmid int, volume, bus string, opts *qemu.AttachOpts) (string, error) {
	if q.gate != nil {
		select {
		case <-q.gate:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return q.Service.AttachDisk(ctx, node, vmid, volume, bus, opts)
}

type contendedDiskPVE struct {
	managedDiskTestPVE
	locks *lockContention
	gate  <-chan struct{}
}

func (c contendedDiskPVE) Pools() pve.PoolService {
	return contendedPools{PoolService: c.managedDiskTestPVE.Pools(), locks: c.locks}
}

func (c contendedDiskPVE) QEMU() qemu.Service {
	return gatedQEMU{Service: c.managedDiskTestPVE.QEMU(), gate: c.gate}
}

// contendedParkRequest builds one managed create_disk that parks onto an
// existing parker, with its sentinel pools on the shared store.
func contendedParkRequest(t *testing.T, locks *lockContention, gate <-chan struct{}) (*managedDiskRequest, *aj.Handle) {
	t.Helper()
	m, h, state := managedDiskFixture(t, "spread", false)
	m.deps.Config.DetachedDiskStrategy = "parked"
	vmid := m.deps.Config.ParkedDiskVMIDRangeStartValue()
	state.configs = map[int]map[string]any{vmid: {"name": fmt.Sprintf("bosh-parker-%d", vmid), "tags": "bosh-parker", "protection": 1, "scsihw": "virtio-scsi-pci"}}
	m.deps.PVE = contendedDiskPVE{managedDiskTestPVE: managedDiskTestPVE{state: state}, locks: locks, gate: gate}
	return m, h
}

func waitFor(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(30 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// TestManagedParkWaitsOutAHeldParkerLock reproduces two guarded parks racing for
// one parker's protection-window lock. The loser's sentinel create is refused
// with the live duplicate verdict; that refusal proves PVE changed nothing, so
// the loser must wait for the holder instead of poisoning its allocation, and
// both allocations must finish without asking for reconciliation.
func TestManagedParkWaitsOutAHeldParkerLock(t *testing.T) {
	locks := newLockContention(t)
	gate := make(chan struct{})
	holder, holderHandle := contendedParkRequest(t, locks, gate)
	waiter, waiterHandle := contendedParkRequest(t, locks, nil)

	var wg sync.WaitGroup
	var holderErr, waiterErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, holderErr = holder.execute(t.Context(), holderHandle)
	}()
	waitFor(t, locks.held, "the holder to take the parker lock")
	// Both requests run in this one process, so they share the owner token
	// "park/<pid>/<parker>", and a claim's expiry has one-second resolution. A
	// waiter that started in the holder's second would stamp the holder's exact
	// claim, which the guard rightly reads as its own create having landed.
	// Separate CPI processes never share a pid, so start the waiter one second on.
	locks.mu.Lock()
	heldAt := locks.heldAt
	locks.mu.Unlock()
	for time.Now().Unix() <= heldAt.Unix() {
		time.Sleep(20 * time.Millisecond)
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		_, waiterErr = waiter.execute(t.Context(), waiterHandle)
	}()
	waitFor(t, locks.rejected, "the waiter's sentinel create to be refused")
	close(gate)
	wg.Wait()

	if holderErr != nil {
		t.Fatalf("holder create_disk failed: %v", holderErr)
	}
	if waiterErr != nil {
		t.Fatalf("waiter create_disk failed instead of waiting for the lock: %v", waiterErr)
	}
	for name, h := range map[string]*aj.Handle{"holder": holderHandle, "waiter": waiterHandle} {
		record := h.Record()
		if record.State != aj.ReadyToReturn {
			t.Fatalf("%s allocation state = %s (reason %q), want %s", name, record.State, record.Reason, aj.ReadyToReturn)
		}
		for i := range record.Steps {
			if record.Steps[i].State != aj.Observed {
				t.Fatalf("%s step %s (%s) left %s", name, record.Steps[i].ID, record.Steps[i].Kind, record.Steps[i].State)
			}
		}
	}
	locks.mu.Lock()
	defer locks.mu.Unlock()
	if locks.rejections == 0 || locks.creates != 2 {
		t.Fatalf("contention not exercised: rejections=%d creates=%d", locks.rejections, locks.creates)
	}
	if len(locks.pools) != 0 {
		t.Fatalf("sentinel pools left behind: %v", locks.pools)
	}
	creates := 0
	for _, step := range waiterHandle.Record().Steps {
		if step.Kind == "park_Pool_CreatePool" {
			creates++
		}
	}
	if creates < 2 {
		t.Fatalf("waiter journaled %d sentinel creates, want the refused one and the winning one", creates)
	}
}

// lockedVMClient gives the VM guard fixture a shared sentinel store.
type lockedVMClient struct {
	*managedVMGuardFixture
	locks *lockContention
}

func (c lockedVMClient) Pools() pve.PoolService { return contendedPools{locks: c.locks} }

// TestManagedVMGuardWaitsOutAHeldAntiAffinityLock takes the anti-affinity lock
// through the VM allocation guard, as create_vm does when HA rules and the pool
// lock are enabled. The first create is refused while another create_vm holds
// the group's sentinel; the guard must let the acquire wait, and it must admit
// the release that deletes the sentinel afterwards.
func TestManagedVMGuardWaitsOutAHeldAntiAffinityLock(t *testing.T) {
	m, fixture, _, _ := newManagedVMGuardCase(t, managedVMGuardCase{})
	locks := newLockContention(t)
	sentinel := pve.ClusterLockPoolName("aa-web")
	locks.pools[sentinel] = fmt.Sprintf("owner=other-director exp=%d", time.Now().Add(time.Hour).Unix())
	m.deps.PVE = lockedVMClient{managedVMGuardFixture: fixture, locks: locks}
	m.guard = nil
	if err := m.newGuard(); err != nil {
		t.Fatal(err)
	}
	go func() {
		<-locks.rejected
		locks.mu.Lock()
		delete(locks.pools, sentinel)
		locks.mu.Unlock()
	}()

	guarded := m.deps
	guarded.PVE = m.guard.Client()
	handle, err := acquireAntiAffinityLock(t.Context(), guarded, "web", m.vmid)
	if err != nil {
		t.Fatalf("anti-affinity acquire did not wait out the holder: %v (guard=%v)", err, m.guard.Err())
	}
	if err := handle.Release(t.Context()); err != nil {
		t.Fatalf("anti-affinity release: %v", err)
	}
	if err := m.guard.Err(); err != nil {
		t.Fatalf("lock contention poisoned the VM allocation: %v", err)
	}
	record := m.handle.Record()
	if record.State == aj.ReconciliationRequired {
		t.Fatalf("lock contention demanded reconciliation: %s", record.Reason)
	}
	kinds := map[string]int{}
	for i := range record.Steps {
		step := &record.Steps[i]
		if step.State != aj.Observed {
			t.Fatalf("step %s (%s) left %s", step.ID, step.Kind, step.State)
		}
		kinds[step.Kind]++
	}
	if kinds["vm.Pool.CreatePool"] < 2 || kinds["vm.Pool.DeletePool"] != 1 {
		t.Fatalf("sentinel steps = %v, want the refused create, the winning create, and one delete", kinds)
	}
	locks.mu.Lock()
	defer locks.mu.Unlock()
	if len(locks.pools) != 0 {
		t.Fatalf("sentinel left behind: %v", locks.pools)
	}
}
