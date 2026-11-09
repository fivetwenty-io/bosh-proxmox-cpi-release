package handlers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/endpointpool"
)

// sendByWireCall is one pool call as the send-by wire saw it.
type sendByWireCall struct {
	method, route string
	// guarded says a send-by marker applied to this call, and steal says
	// its context carried the steal's marker at all.
	guarded, steal, refused bool
}

// sendByWire stands in for the failover transport's send-by check below a
// guard. A call whose context carries the steal's marker (DELETE /pools) runs
// after that deadline, except a guarded delete that late says is on time. A
// read on such a context therefore runs after the deadline too, which is how
// the guard's claim read and readback meet it. Every check goes through
// endpointpool.CheckSendBy with the method and SDK-prefixed path the call
// sends, and a refused call never reaches the pool store.
type sendByWire struct {
	// late reports whether the n-th guarded delete, counted from zero,
	// arrives after its deadline. A nil late makes every one late.
	late func(n int) bool

	mu      sync.Mutex
	guarded int
	calls   []sendByWireCall
}

// sdkSendByRefusal is the error the SDK surfaces when its transport refuses a
// request for its send-by deadline: the retry loop's wrap around a *url.Error.
func sdkSendByRefusal(method, path string, cause error) error {
	op := strings.ToUpper(method[:1]) + strings.ToLower(method[1:])
	return fmt.Errorf("request failed after 1 attempt(s): %w",
		&url.Error{Op: op, URL: "https://pve1:8006" + path, Err: cause})
}

func (w *sendByWire) admit(ctx context.Context, method, route string) error {
	path := endpointpool.APIPrefix + route
	_, guarded := endpointpool.SendBy(ctx, method, path)
	stealDeadline, steal := endpointpool.SendBy(ctx, http.MethodDelete, endpointpool.APIPrefix+"/pools")
	w.mu.Lock()
	defer w.mu.Unlock()
	at := time.Now()
	if steal {
		at = stealDeadline.Add(time.Hour)
		if guarded {
			if w.late != nil && !w.late(w.guarded) {
				at = stealDeadline.Add(-time.Second)
			}
			w.guarded++
		}
	}
	refusal := endpointpool.CheckSendBy(ctx, at, method, path)
	w.calls = append(w.calls, sendByWireCall{method: method, route: route, guarded: guarded, steal: steal, refused: refusal != nil})
	if refusal != nil {
		return sdkSendByRefusal(method, path, refusal)
	}
	return nil
}

func (w *sendByWire) recorded() []sendByWireCall {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]sendByWireCall(nil), w.calls...)
}

// refusedDeletes counts the deletes the wire refused, and sentDeletes the
// guarded deletes it let through.
func (w *sendByWire) refusedDeletes() (refused, sent int) {
	for _, call := range w.recorded() {
		switch {
		case call.method == http.MethodDelete && call.refused:
			refused++
		case call.method == http.MethodDelete && call.guarded:
			sent++
		}
	}
	return refused, sent
}

// assertMarkedReadsRan fails the test unless at least one read ran on a
// context that carried the steal's marker, past its deadline, and no read was
// refused.
func (w *sendByWire) assertMarkedReadsRan(t *testing.T) {
	t.Helper()
	marked := 0
	for _, call := range w.recorded() {
		if call.method != http.MethodGet {
			continue
		}
		if call.refused {
			t.Fatalf("a read was refused for the delete's deadline: %+v", w.recorded())
		}
		if call.steal {
			marked++
		}
	}
	if marked == 0 {
		t.Fatalf("no read ran on the steal's context: %+v", w.recorded())
	}
}

// sendByWirePools puts the wire in front of a pool service and keeps its raw
// read, which a guard needs to settle a refused sentinel create.
type sendByWirePools struct {
	pve.PoolService
	wire *sendByWire
}

func (p sendByWirePools) CreatePool(ctx context.Context, id, comment string) error {
	if err := p.wire.admit(ctx, http.MethodPost, "/pools"); err != nil {
		return err
	}
	return p.PoolService.CreatePool(ctx, id, comment)
}

func (p sendByWirePools) DeletePool(ctx context.Context, id string) error {
	if err := p.wire.admit(ctx, http.MethodDelete, "/pools"); err != nil {
		return err
	}
	return p.PoolService.DeletePool(ctx, id)
}

func (p sendByWirePools) GetPoolComment(ctx context.Context, id string) (string, bool, error) {
	if err := p.wire.admit(ctx, http.MethodGet, "/pools/"+id); err != nil {
		return "", false, err
	}
	return p.PoolService.GetPoolComment(ctx, id)
}

func (p sendByWirePools) ReadPoolComment(ctx context.Context, id string) (string, error) {
	reader, ok := p.PoolService.(pve.RawPoolCommentReader)
	if !ok {
		return "", errors.New("wrapped pool service cannot read a pool comment raw")
	}
	if err := p.wire.admit(ctx, http.MethodGet, "/pools/"+id); err != nil {
		return "", err
	}
	return reader.ReadPoolComment(ctx, id)
}

// stealContext is the context a steal's delete carries: the claim it judged
// expired and the send-by marker for DELETE /pools.
func stealContext(ctx context.Context, claim string) context.Context {
	return endpointpool.WithSendBy(pve.WithExpectedLockClaim(ctx, claim), time.Now().Add(2*time.Second), http.MethodDelete, "/pools")
}

// expiredClaim is a claim whose expiry passed a minute ago.
func expiredClaim(owner string) string {
	return fmt.Sprintf("owner=%s exp=%d", owner, time.Now().Add(-time.Minute).Unix())
}

// TestGuardedStealDeleteSendByRefusalSettles refuses a steal's delete for its
// send-by deadline under the managed pool guard. The guard's claim read runs
// after the deadline on the same context and is not refused, because the
// marker names only DELETE /pools. The refused delete settles through
// settleRefused, so After receives the refusal proof, the guard is not
// poisoned, the lock code gets the refusal unchanged, and the next mutation
// goes through.
func TestGuardedStealDeleteSendByRefusalSettles(t *testing.T) {
	claim := "owner=crashed@h/1-a-1 exp=1"
	pools := &lockGuardPools{found: true, comment: claim}
	wire := &sendByWire{}
	client := storeClient{pools: sendByWirePools{PoolService: pools, wire: wire}}
	events := &lockGuardEvents{}
	guard := newLockGuardOver(t, client, events)

	err := guard.Client().Pools().DeletePool(stealContext(t.Context(), claim), lockGuardPool)
	if !errors.Is(err, endpointpool.ErrSendByPassed) {
		t.Fatalf("want the refusal back unchanged, got %v", err)
	}
	if !strings.Contains(err.Error(), "request failed after 1 attempt(s)") {
		t.Fatalf("the refusal was rewrapped: %v", err)
	}
	if guard.Err() != nil || events.failed != 0 {
		t.Fatalf("a refused steal delete poisoned the guard: %v (failed=%d)", guard.Err(), events.failed)
	}
	if len(events.after) != 1 {
		t.Fatalf("the refusal was not settled through After: %v", events.after)
	}
	if got, ok := events.after[0].(managedLockPoolRejection); !ok || got.poolID != lockGuardPool || got.method != "DeletePool" {
		t.Fatalf("After received %#v, want the refusal proof for the delete of %s", events.after[0], lockGuardPool)
	}
	if deletedByPVE(pools) || !pools.found {
		t.Fatalf("the refused delete reached PVE: %v", pools.calls)
	}
	wire.assertMarkedReadsRan(t)

	// The next mutation proceeds.
	if err := guard.Client().Pools().DeletePool(pve.WithExpectedLockClaim(t.Context(), claim), lockGuardPool); err != nil {
		t.Fatalf("the guard refused the next delete: %v", err)
	}
	if pools.found || guard.Err() != nil {
		t.Fatalf("the next delete did not land cleanly: found=%v guard=%v", pools.found, guard.Err())
	}
}

// TestGuardedStealDeleteReadbackRunsAfterTheDeadline has the steal's delete
// land just before its deadline. The guard reads it back on the same context
// after the deadline, and that read is not refused, so the delete is observed
// and the guard stays usable.
func TestGuardedStealDeleteReadbackRunsAfterTheDeadline(t *testing.T) {
	claim := "owner=crashed@h/1-a-1 exp=1"
	pools := &lockGuardPools{found: true, comment: claim}
	wire := &sendByWire{late: func(int) bool { return false }}
	client := storeClient{pools: sendByWirePools{PoolService: pools, wire: wire}}
	events := &lockGuardEvents{}
	guard := newLockGuardOver(t, client, events)

	if err := guard.Client().Pools().DeletePool(stealContext(t.Context(), claim), lockGuardPool); err != nil {
		t.Fatalf("the steal delete that went out in time failed: %v", err)
	}
	if guard.Err() != nil || events.failed != 0 || pools.found {
		t.Fatalf("the delete was not observed: guard=%v failed=%d found=%v", guard.Err(), events.failed, pools.found)
	}
	if got, ok := events.after[0].(managedLockPoolDeletion); !ok || !got.known || got.comment != claim {
		t.Fatalf("After did not receive the deleted claim: %#v", events.after[0])
	}
	reads := 0
	afterDelete := false
	for _, call := range wire.recorded() {
		if call.method == http.MethodDelete {
			afterDelete = true
			continue
		}
		if afterDelete && call.method == http.MethodGet && call.steal && !call.refused {
			reads++
		}
	}
	if reads == 0 {
		t.Fatalf("no readback ran after the deadline: %+v", wire.recorded())
	}
	wire.assertMarkedReadsRan(t)
}

// TestNestedGuardSettlesASendByRefusal stacks a guard on another guard's
// client. The inner guard settles the refused delete and hands it back
// unchanged, and the outer guard settles it the same way, so neither is
// poisoned.
func TestNestedGuardSettlesASendByRefusal(t *testing.T) {
	claim := "owner=crashed@h/1-a-1 exp=1"
	pools := &lockGuardPools{found: true, comment: claim}
	wire := &sendByWire{}
	innerEvents, outerEvents := &lockGuardEvents{}, &lockGuardEvents{}
	inner := newLockGuardOver(t, storeClient{pools: sendByWirePools{PoolService: pools, wire: wire}}, innerEvents)
	outer := newLockGuardOver(t, inner.Client(), outerEvents)

	err := outer.Client().Pools().DeletePool(stealContext(t.Context(), claim), lockGuardPool)
	if !errors.Is(err, endpointpool.ErrSendByPassed) {
		t.Fatalf("want the refusal back unchanged, got %v", err)
	}
	for name, g := range map[string]*ManagedAllocationGuard{"inner": inner, "outer": outer} {
		if g.Err() != nil {
			t.Fatalf("the %s guard was poisoned: %v", name, g.Err())
		}
	}
	for name, events := range map[string]*lockGuardEvents{"inner": innerEvents, "outer": outerEvents} {
		if len(events.after) != 1 || events.failed != 0 {
			t.Fatalf("the %s guard did not settle the refusal: after=%v failed=%d", name, events.after, events.failed)
		}
		if _, ok := events.after[0].(managedLockPoolRejection); !ok {
			t.Fatalf("the %s guard settled with %#v, want the refusal proof", name, events.after[0])
		}
	}
}

// TestGuardedOperatorPoolSendByRefusalStaysUncertain keeps the new arm to
// lock sentinels. A send-by refusal on any other pool is not something the
// lock code sends, so it is not settled as a sentinel refusal and fails closed.
func TestGuardedOperatorPoolSendByRefusalStaysUncertain(t *testing.T) {
	pools := &lockGuardPools{found: true}
	wire := &sendByWire{}
	events := &lockGuardEvents{}
	guard := newLockGuardOver(t, storeClient{pools: sendByWirePools{PoolService: pools, wire: wire}}, events)
	ctx := endpointpool.WithSendBy(t.Context(), time.Now().Add(time.Second), http.MethodDelete, "/pools")
	if err := guard.Client().Pools().DeletePool(ctx, "bosh-director"); err == nil || guard.Err() == nil || events.failed != 1 {
		t.Fatalf("a refused operator pool delete was settled: err=%v guard=%v failed=%d", err, guard.Err(), events.failed)
	}
}

// TestGuardedAcquireStealsAfterASendByRefusal takes a lock whose holder
// expired through a guard, with the first steal's delete refused for its
// deadline. The steal is abandoned instead of failing the acquire with
// ErrClusterLockStateUnknown, the next poll steals in time, and the guard is
// never poisoned.
func TestGuardedAcquireStealsAfterASendByRefusal(t *testing.T) {
	t.Parallel()
	locks := newLockContention(t)
	sentinel := pve.ClusterLockPoolName("vm-90000")
	locks.pools[sentinel] = expiredClaim("crashed/90000@1-1")
	wire := &sendByWire{late: func(n int) bool { return n == 0 }}
	client := storeClient{pools: sendByWirePools{PoolService: contendedPools{locks: locks}, wire: wire}}
	events := &lockGuardEvents{}
	guard := newLockGuardOver(t, client, events)

	handle, err := pve.AcquireClusterLock(withShortLockPoll(t.Context()), guard.Client().Pools(), "vm-90000", "unpark/90000@9-1", time.Minute, 5*time.Second)
	if err != nil || handle == nil {
		t.Fatalf("the acquire did not steal after the refused delete: handle %v err %v (guard=%v)", handle, err, guard.Err())
	}
	if err := handle.Release(context.WithoutCancel(t.Context())); err != nil {
		t.Fatalf("release: %v", err)
	}
	if guard.Err() != nil || events.failed != 0 {
		t.Fatalf("the refused steal poisoned the guard: %v", guard.Err())
	}
	if refused, sent := wire.refusedDeletes(); refused != 1 || sent != 1 {
		t.Fatalf("want one refused and one sent steal delete, got refused=%d sent=%d: %+v", refused, sent, wire.recorded())
	}
	wire.assertMarkedReadsRan(t)
	locks.mu.Lock()
	defer locks.mu.Unlock()
	if len(locks.pools) != 0 {
		t.Fatalf("sentinel left behind: %v", locks.pools)
	}
}

// poolStepKinds counts the journaled steps whose kind ends in suffix, and
// fails the test when any step is not observed.
func poolStepKinds(t *testing.T, record aj.Record, suffix string) int {
	t.Helper()
	n := 0
	for i := range record.Steps {
		step := &record.Steps[i]
		if step.State != aj.Observed {
			t.Fatalf("step %s (%s) left %s", step.ID, step.Kind, step.State)
		}
		if strings.HasSuffix(step.Kind, suffix) {
			n++
		}
	}
	return n
}

// sendByFlowPVE puts the send-by wire in front of the lifecycle flow fixture's
// sentinel store.
type sendByFlowPVE struct {
	contendedFlowPVE
	wire *sendByWire
}

func (c sendByFlowPVE) Pools() pve.PoolService {
	return sendByWirePools{PoolService: c.contendedFlowPVE.Pools(), wire: c.wire}
}

// TestManagedLifecycleSettlesASendByRefusedSteal is an attach_disk out of a
// parker whose lock holder expired. The disk lifecycle guard admits the
// steal's delete, the delete is refused for its deadline, and the guard
// settles it through observeLockPoolMutation as an observed non-mutation. The
// next poll steals in time and the attach completes, with the allocation
// returned and every step observed.
func TestManagedLifecycleSettlesASendByRefusedSteal(t *testing.T) {
	t.Parallel()
	locks := newLockContention(t)
	disk := newParkedFlowDisk(t, locks)
	locks.reset()
	locks.mu.Lock()
	locks.pools[pve.ClusterLockPoolName(fmt.Sprintf("vm-%d", disk.parker))] = expiredClaim(fmt.Sprintf("transfer_out/%d@1-1", disk.parker))
	locks.mu.Unlock()
	wire := &sendByWire{late: func(n int) bool { return n == 0 }}
	disk.deps.PVE = sendByFlowPVE{contendedFlowPVE: disk.deps.PVE.(contendedFlowPVE), wire: wire}

	if err := disk.attach(withShortLockPoll(t.Context())); err != nil {
		t.Fatalf("attach_disk failed after a refused steal: %v", err)
	}
	record := disk.record(t)
	assertReturnedRecord(t, "attach", record)
	if deletes := poolStepKinds(t, record, "_Pool_DeletePool"); deletes < 3 {
		t.Fatalf("journaled %d sentinel deletes, want the refused steal, the steal, and the release", deletes)
	}
	if refused, sent := wire.refusedDeletes(); refused != 1 || sent != 1 {
		t.Fatalf("want one refused and one sent steal delete, got refused=%d sent=%d", refused, sent)
	}
	wire.assertMarkedReadsRan(t)
	if slot := disk.client.state.configs[777]["scsi1"]; slot == nil {
		t.Fatalf("the attach did not land the disk: %v", disk.client.state.configs[777])
	}
}

// sendByDiskPVE puts the send-by wire in front of the park fixture's sentinel
// store.
type sendByDiskPVE struct {
	contendedDiskPVE
	wire *sendByWire
}

func (c sendByDiskPVE) Pools() pve.PoolService {
	return sendByWirePools{PoolService: c.contendedDiskPVE.Pools(), wire: c.wire}
}

// TestManagedParkSettlesASendByRefusedSteal is a managed create_disk that
// parks onto a parker whose lock holder expired. The parker's guard settles
// the refused steal delete through observeLockPoolMutation, the next poll
// steals in time, and the park completes with every step observed.
func TestManagedParkSettlesASendByRefusedSteal(t *testing.T) {
	t.Parallel()
	locks := newLockContention(t)
	m, handle := contendedParkRequest(t, locks, nil)
	parker := m.deps.Config.ParkedDiskVMIDRangeStartValue()
	locks.pools[pve.ClusterLockPoolName(fmt.Sprintf("vm-%d", parker))] = expiredClaim(fmt.Sprintf("transfer_out/%d@1-1", parker))
	wire := &sendByWire{late: func(n int) bool { return n == 0 }}
	m.deps.PVE = sendByDiskPVE{contendedDiskPVE: m.deps.PVE.(contendedDiskPVE), wire: wire}

	if _, err := m.execute(withShortLockPoll(t.Context()), handle); err != nil {
		t.Fatalf("create_disk failed after a refused steal: %v", err)
	}
	record := handle.Record()
	if record.State != aj.ReadyToReturn {
		t.Fatalf("allocation state = %s (reason %q), want %s", record.State, record.Reason, aj.ReadyToReturn)
	}
	if deletes := poolStepKinds(t, record, "park_Pool_DeletePool"); deletes < 3 {
		t.Fatalf("journaled %d sentinel deletes, want the refused steal, the steal, and the release", deletes)
	}
	if refused, sent := wire.refusedDeletes(); refused != 1 || sent != 1 {
		t.Fatalf("want one refused and one sent steal delete, got refused=%d sent=%d", refused, sent)
	}
	wire.assertMarkedReadsRan(t)
	locks.mu.Lock()
	defer locks.mu.Unlock()
	if len(locks.pools) != 0 {
		t.Fatalf("sentinel left behind: %v", locks.pools)
	}
}

// sendByVMClient puts the send-by wire in front of the VM guard fixture's
// sentinel store.
type sendByVMClient struct {
	lockedVMClient
	wire *sendByWire
}

func (c sendByVMClient) Pools() pve.PoolService {
	return sendByWirePools{PoolService: c.lockedVMClient.Pools(), wire: c.wire}
}

// TestManagedVMGuardSettlesASendByRefusedSteal takes the anti-affinity lock
// through the VM allocation guard while its holder has expired. The guard
// settles the refused steal delete through observeLockPoolMutation, the next
// poll steals in time, and the acquire and release leave the allocation
// usable with every step observed.
func TestManagedVMGuardSettlesASendByRefusedSteal(t *testing.T) {
	m, fixture, _, _ := newManagedVMGuardCase(t, managedVMGuardCase{})
	locks := newLockContention(t)
	locks.pools[pve.ClusterLockPoolName("aa-web")] = expiredClaim("other-director")
	wire := &sendByWire{late: func(n int) bool { return n == 0 }}
	m.deps.PVE = sendByVMClient{lockedVMClient: lockedVMClient{managedVMGuardFixture: fixture, locks: locks}, wire: wire}
	m.guard = nil
	if err := m.newGuard(); err != nil {
		t.Fatal(err)
	}

	guarded := m.deps
	guarded.PVE = m.guard.Client()
	handle, err := acquireAntiAffinityLock(withShortLockPoll(t.Context()), guarded, "web", m.vmid)
	if err != nil {
		t.Fatalf("anti-affinity acquire failed after a refused steal: %v (guard=%v)", err, m.guard.Err())
	}
	if err := handle.Release(context.WithoutCancel(t.Context())); err != nil {
		t.Fatalf("anti-affinity release: %v", err)
	}
	if err := m.guard.Err(); err != nil {
		t.Fatalf("the refused steal poisoned the VM allocation: %v", err)
	}
	record := m.handle.Record()
	if record.State == aj.ReconciliationRequired {
		t.Fatalf("the refused steal demanded reconciliation: %s", record.Reason)
	}
	if deletes := poolStepKinds(t, record, "vm.Pool.DeletePool"); deletes != 3 {
		t.Fatalf("journaled %d sentinel deletes, want the refused steal, the steal, and the release", deletes)
	}
	if refused, sent := wire.refusedDeletes(); refused != 1 || sent != 1 {
		t.Fatalf("want one refused and one sent steal delete, got refused=%d sent=%d", refused, sent)
	}
	wire.assertMarkedReadsRan(t)
	locks.mu.Lock()
	defer locks.mu.Unlock()
	if len(locks.pools) != 0 {
		t.Fatalf("sentinel left behind: %v", locks.pools)
	}
}
