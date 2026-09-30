package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

// lockGuardPools is a scripted pool service for the guard's own classification.
type lockGuardPools struct {
	pve.PoolService
	createErr   error
	deleteErr   error
	comment     string
	found       bool
	readErr     error
	deleteFound bool
	successor   string
	// rawErr, when set, is what a raw read answers instead of the state.
	rawErr error
	// createGone and createDisplaced model a stealer acting between a
	// successful create and its readback.
	createGone, createDisplaced bool
	// plain hides the raw read from the guard.
	plain bool
	// normalize, when set, is what PVE stores for a created comment, for a
	// PVE that does not keep the comment byte for byte.
	normalize func(string) string
	// readErrs scripts the plain reads in order. A nil entry reads the state
	// as usual, and the reads after the script runs out do the same.
	readErrs []error
	// readComments scripts the comment each plain read answers with, in the
	// same way. An empty entry answers with the stored state.
	readComments []string
	calls        []string
}

// poolVerdictError is PVE's 500 answer carrying message, as the SDK wraps it.
func poolVerdictError(message string) error {
	body, _ := json.Marshal(map[string]any{"data": nil, "message": message + "\n"})
	return fmt.Errorf("API request failed: %w", sdkerrors.ParseAPIError(500, body))
}

func (p *lockGuardPools) ReadPoolComment(_ context.Context, id string) (string, error) {
	p.calls = append(p.calls, "raw:"+id)
	switch {
	case p.rawErr != nil:
		return "", p.rawErr
	case p.readErr != nil:
		return "", p.readErr
	case !p.found:
		return "", poolVerdictError("pool '" + id + "' does not exist")
	}
	return p.comment, nil
}

// plainPools hides the raw read, like a pool service that cannot offer one.
type plainPools struct{ pve.PoolService }

func (p *lockGuardPools) CreatePool(_ context.Context, id, comment string) error {
	p.calls = append(p.calls, "create:"+id)
	if p.createErr != nil {
		return p.createErr
	}
	p.comment, p.found = comment, true
	if p.normalize != nil {
		p.comment = p.normalize(comment)
	}
	switch {
	case p.createGone:
		p.found = false
	case p.createDisplaced:
		p.comment = "owner=stealer exp=9"
	}
	return nil
}

func (p *lockGuardPools) DeletePool(_ context.Context, id string) error {
	p.calls = append(p.calls, "delete:"+id)
	if p.deleteErr != nil {
		return p.deleteErr
	}
	p.found = p.deleteFound
	if p.successor != "" {
		p.comment, p.found = p.successor, true
	}
	return nil
}

func (p *lockGuardPools) GetPoolComment(_ context.Context, id string) (string, bool, error) {
	p.calls = append(p.calls, "read:"+id)
	if len(p.readErrs) > 0 {
		err := p.readErrs[0]
		p.readErrs = p.readErrs[1:]
		if err != nil {
			return "", false, err
		}
	}
	if len(p.readComments) > 0 {
		comment := p.readComments[0]
		p.readComments = p.readComments[1:]
		if comment != "" {
			return comment, true, nil
		}
	}
	return p.comment, p.found, p.readErr
}

type lockGuardClient struct {
	pve.Client
	pools *lockGuardPools
}

func (c lockGuardClient) Pools() pve.PoolService {
	if c.pools.plain {
		return plainPools{c.pools}
	}
	return c.pools
}

type lockGuardEvents struct {
	after   []any
	failed  int
	afterFn func(context.Context, ManagedAllocationMutation, any) error
}

func newLockGuard(t *testing.T, pools *lockGuardPools, events *lockGuardEvents) *ManagedAllocationGuard {
	t.Helper()
	return newLockGuardOver(t, lockGuardClient{pools: pools}, events)
}

// newLockGuardOver builds the same guard over any client, including another
// guard's client.
func newLockGuardOver(t *testing.T, raw pve.Client, events *lockGuardEvents) *ManagedAllocationGuard {
	t.Helper()
	guard, err := NewManagedAllocationGuard(raw, ManagedAllocationHooks{
		Before: func(context.Context, ManagedAllocationMutation) (string, error) { return "step", nil },
		After: func(ctx context.Context, call ManagedAllocationMutation, _ string, result any) error {
			events.after = append(events.after, result)
			if events.afterFn != nil {
				return events.afterFn(ctx, call, result)
			}
			return observeLockPoolMutation(ctx, raw.Pools(), call, result, "test")
		},
		Failed: func(context.Context, ManagedAllocationMutation, string, error) error {
			events.failed++
			return errors.New("allocation requires reconciliation")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return guard
}

const lockGuardPool = "bosh-lock-vm-90372"

// unchangedError reports whether got is want itself, carried through without a
// wrapper, which is what the lock code needs in order to classify it.
func unchangedError(got, want error) bool {
	return errors.Is(got, want) && got.Error() == want.Error()
}

func TestManagedLockCreateRefusalIsProvenNonMutation(t *testing.T) {
	for _, message := range []string{
		"create pool failed: pool '" + lockGuardPool + "' already exists",
		"pool '" + lockGuardPool + "' already exists",
	} {
		t.Run(message, func(t *testing.T) {
			refusal := livePoolVerdict(t, message)
			pools := &lockGuardPools{createErr: refusal, comment: "owner=holder exp=1", found: true}
			events := &lockGuardEvents{}
			guard := newLockGuard(t, pools, events)
			err := guard.Client().Pools().CreatePool(t.Context(), lockGuardPool, "owner=waiter exp=2")
			if !unchangedError(err, refusal) {
				t.Fatalf("refusal was replaced: got %v, want the original %v", err, refusal)
			}
			if guard.Err() != nil || events.failed != 0 {
				t.Fatalf("a held lock poisoned the guard: poison=%v failed=%d", guard.Err(), events.failed)
			}
			if len(events.after) != 1 {
				t.Fatalf("refusal was not settled through After: %v", events.after)
			}
			if got, ok := events.after[0].(managedLockPoolRejection); !ok || got.poolID != lockGuardPool || got.method != "CreatePool" {
				t.Fatalf("After received %#v, want the refusal proof for %s", events.after[0], lockGuardPool)
			}
			if !strings.Contains(strings.ToLower(err.Error()), "already exists") {
				t.Fatalf("the lock's duplicate classifier cannot read %q", err)
			}
			if err := guard.Client().Pools().CreatePool(t.Context(), lockGuardPool, "owner=waiter exp=3"); !unchangedError(err, refusal) {
				t.Fatalf("the guard refused the next acquire attempt: %v", err)
			}
		})
	}
}

func TestManagedLockCreateFailuresStayUncertain(t *testing.T) {
	cases := []struct {
		name    string
		pool    string
		err     func(t *testing.T) error
		comment string
		found   bool
		readErr error
		rawErr  error
		plain   bool
		after   func(context.Context, ManagedAllocationMutation, any) error
	}{
		{name: "operator pool duplicate", pool: "bosh-director", err: func(t *testing.T) error {
			return livePoolVerdict(t, "create pool failed: pool 'bosh-director' already exists")
		}, comment: "owner=holder exp=1", found: true},
		{name: "duplicate names another pool", pool: lockGuardPool, err: func(t *testing.T) error {
			return livePoolVerdict(t, "create pool failed: pool 'bosh-lock-vm-1' already exists")
		}, comment: "owner=holder exp=1", found: true},
		{name: "untyped duplicate text", pool: lockGuardPool, err: func(*testing.T) error {
			return errors.New("create pool failed: pool '" + lockGuardPool + "' already exists")
		}, comment: "owner=holder exp=1", found: true},
		{name: "other lock verdict", pool: lockGuardPool, err: func(t *testing.T) error {
			return livePoolVerdict(t, "create pool failed: pool name must start with a letter")
		}},
		{name: "transport failure", pool: lockGuardPool, err: func(*testing.T) error { return errors.New("connection reset by peer") }},
		{name: "sentinel carries our own claim", pool: lockGuardPool, err: func(t *testing.T) error {
			return livePoolVerdict(t, "create pool failed: pool '"+lockGuardPool+"' already exists")
		}, comment: "owner=waiter exp=2", found: true},
		{name: "holder unreadable", pool: lockGuardPool, err: func(t *testing.T) error {
			return livePoolVerdict(t, "create pool failed: pool '"+lockGuardPool+"' already exists")
		}, readErr: errors.New("read failed")},
		{name: "holder read answers loosely", pool: lockGuardPool, err: func(t *testing.T) error {
			return livePoolVerdict(t, "create pool failed: pool '"+lockGuardPool+"' already exists")
		}, comment: "owner=holder exp=1", found: true, rawErr: errors.New("pools.GetPools: pool '" + lockGuardPool + "' does not exist")},
		{name: "holder read names another pool", pool: lockGuardPool, err: func(t *testing.T) error {
			return livePoolVerdict(t, "create pool failed: pool '"+lockGuardPool+"' already exists")
		}, rawErr: poolVerdictError("pool 'bosh-lock-vm-1' does not exist")},
		{name: "pool service offers no raw read", pool: lockGuardPool, err: func(t *testing.T) error {
			return livePoolVerdict(t, "create pool failed: pool '"+lockGuardPool+"' already exists")
		}, comment: "owner=holder exp=1", found: true, plain: true},
		{name: "journal cannot settle", pool: lockGuardPool, err: func(t *testing.T) error {
			return livePoolVerdict(t, "create pool failed: pool '"+lockGuardPool+"' already exists")
		}, comment: "owner=holder exp=1", found: true, after: func(context.Context, ManagedAllocationMutation, any) error {
			return errors.New("journal write failed")
		}},
	}
	for i := range cases {
		tc := &cases[i]
		t.Run(tc.name, func(t *testing.T) {
			pools := &lockGuardPools{createErr: tc.err(t), comment: tc.comment, found: tc.found, readErr: tc.readErr, rawErr: tc.rawErr, plain: tc.plain}
			events := &lockGuardEvents{afterFn: tc.after}
			guard := newLockGuard(t, pools, events)
			err := guard.Client().Pools().CreatePool(t.Context(), tc.pool, "owner=waiter exp=2")
			if err == nil || guard.Err() == nil || events.failed != 1 {
				t.Fatalf("failure was not held uncertain: err=%v poison=%v failed=%d", err, guard.Err(), events.failed)
			}
			if errors.Is(err, pools.createErr) {
				t.Fatal("an uncertain failure leaked the upstream error")
			}
		})
	}
}

// TestNestedGuardSettlesALockCreateRefusal builds a guard over another
// guard's client. The outer guard proves a refused sentinel create by reading
// the holder's claim through the inner guard's pool service, so a contended
// create must settle as refused on both guards instead of poisoning the outer
// one. The next poll then reads the same claim through the inner guard and is
// answered with the same refusal without another create reaching PVE.
func TestNestedGuardSettlesALockCreateRefusal(t *testing.T) {
	refusal := livePoolVerdict(t, "create pool failed: pool '"+lockGuardPool+"' already exists")
	pools := &lockGuardPools{createErr: refusal, comment: "owner=holder exp=1", found: true}
	innerEvents, outerEvents := &lockGuardEvents{}, &lockGuardEvents{}
	inner := newLockGuard(t, pools, innerEvents)
	outer := newLockGuardOver(t, inner.Client(), outerEvents)
	err := outer.Client().Pools().CreatePool(t.Context(), lockGuardPool, "owner=waiter exp=2")
	if !unchangedError(err, refusal) {
		t.Fatalf("refusal was replaced: got %v, want the original %v", err, refusal)
	}
	if outer.Err() != nil || outerEvents.failed != 0 || inner.Err() != nil || innerEvents.failed != 0 {
		t.Fatalf("a held lock poisoned a guard: outer=%v inner=%v", outer.Err(), inner.Err())
	}
	if len(outerEvents.after) != 1 || len(innerEvents.after) != 1 {
		t.Fatalf("refusal was not settled on both guards: outer=%v inner=%v", outerEvents.after, innerEvents.after)
	}
	pools.calls = nil
	if err := outer.Client().Pools().CreatePool(t.Context(), lockGuardPool, "owner=waiter exp=3"); !unchangedError(err, refusal) {
		t.Fatalf("the outer guard refused the next poll: %v", err)
	}
	if want := []string{"raw:" + lockGuardPool}; strings.Join(pools.calls, ",") != strings.Join(want, ",") {
		t.Fatalf("the next poll made calls %v, want only the claim read %v", pools.calls, want)
	}
	if len(outerEvents.after) != 1 || len(innerEvents.after) != 1 {
		t.Fatalf("a repeated refusal was journaled: outer=%v inner=%v", outerEvents.after, innerEvents.after)
	}
}

// TestManagedLockCreateRefusalAfterRelease covers a holder that released
// between the refusal and the readback. The readback answers with the exact
// missing-pool verdict, which still proves the refused create changed nothing.
func TestManagedLockCreateRefusalAfterRelease(t *testing.T) {
	refusal := livePoolVerdict(t, "create pool failed: pool '"+lockGuardPool+"' already exists")
	pools := &lockGuardPools{createErr: refusal}
	events := &lockGuardEvents{}
	guard := newLockGuard(t, pools, events)
	if err := guard.Client().Pools().CreatePool(t.Context(), lockGuardPool, "owner=waiter exp=2"); !unchangedError(err, refusal) {
		t.Fatalf("refusal was replaced: %v", err)
	}
	if guard.Err() != nil || events.failed != 0 {
		t.Fatalf("a released holder poisoned the guard: %v", guard.Err())
	}
}

// TestManagedLockCreateDisplacedAfterSuccess covers what can happen to a
// sentinel between a successful create and anything that reads it: a stealer
// deletes it, a stealer replaces it, or a read of it fails. PVE accepted the
// create, so the step is observed in every case without a read, and the lock
// code's own verification decides who holds the lock.
func TestManagedLockCreateDisplacedAfterSuccess(t *testing.T) {
	for _, tc := range []struct {
		name  string
		pools *lockGuardPools
	}{
		{name: "sentinel gone", pools: &lockGuardPools{createGone: true}},
		{name: "another claim", pools: &lockGuardPools{createDisplaced: true}},
		{name: "a read would fail", pools: &lockGuardPools{readErr: errors.New("read failed")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := &lockGuardEvents{}
			guard := newLockGuard(t, tc.pools, events)
			err := guard.Client().Pools().CreatePool(t.Context(), lockGuardPool, "owner=me exp=1")
			if err != nil || guard.Err() != nil || events.failed != 0 {
				t.Fatalf("an accepted create poisoned the guard: err=%v poison=%v", err, guard.Err())
			}
			for _, call := range tc.pools.calls {
				if strings.HasPrefix(call, "read:") {
					t.Fatalf("the guard read the sentinel back after an accepted create: %v", tc.pools.calls)
				}
			}
		})
	}
}

func TestManagedLockDeleteOutcomes(t *testing.T) {
	t.Run("sentinel already gone", func(t *testing.T) {
		missing := livePoolVerdict(t, "delete pool failed: pool '"+lockGuardPool+"' does not exist")
		pools := &lockGuardPools{deleteErr: missing}
		events := &lockGuardEvents{}
		guard := newLockGuard(t, pools, events)
		if err := guard.Client().Pools().DeletePool(t.Context(), lockGuardPool); !unchangedError(err, missing) {
			t.Fatalf("missing sentinel refusal was replaced: %v", err)
		}
		if guard.Err() != nil || events.failed != 0 {
			t.Fatalf("a released sentinel poisoned the guard: %v", guard.Err())
		}
	})
	t.Run("operator pool already gone", func(t *testing.T) {
		pools := &lockGuardPools{deleteErr: livePoolVerdict(t, "delete pool failed: pool 'bosh-director' does not exist")}
		events := &lockGuardEvents{}
		guard := newLockGuard(t, pools, events)
		if err := guard.Client().Pools().DeletePool(t.Context(), "bosh-director"); err == nil || guard.Err() == nil || events.failed != 1 {
			t.Fatalf("operator pool delete refusal was accepted: %v", err)
		}
	})
	t.Run("lock delete refused for another reason", func(t *testing.T) {
		pools := &lockGuardPools{found: true, comment: "owner=me exp=1", deleteErr: livePoolVerdict(t, "delete pool failed: pool '"+lockGuardPool+"' is not empty (contains VM 100)")}
		events := &lockGuardEvents{}
		guard := newLockGuard(t, pools, events)
		if err := guard.Client().Pools().DeletePool(t.Context(), lockGuardPool); err == nil || guard.Err() == nil || events.failed != 1 {
			t.Fatalf("unexpected lock delete refusal was accepted: %v", err)
		}
	})
	t.Run("waiter recreated the sentinel before readback", func(t *testing.T) {
		pools := &lockGuardPools{found: true, comment: "owner=me exp=1", successor: "owner=waiter exp=2"}
		events := &lockGuardEvents{}
		guard := newLockGuard(t, pools, events)
		if err := guard.Client().Pools().DeletePool(t.Context(), lockGuardPool); err != nil || guard.Err() != nil {
			t.Fatalf("a successor's sentinel hid our observed delete: %v", err)
		}
		if got, ok := events.after[0].(managedLockPoolDeletion); !ok || !got.known || got.comment != "owner=me exp=1" {
			t.Fatalf("After did not receive the deleted sentinel's claim: %#v", events.after[0])
		}
	})
	t.Run("our own sentinel survived the delete", func(t *testing.T) {
		pools := &lockGuardPools{found: true, comment: "owner=me exp=1", deleteFound: true}
		events := &lockGuardEvents{}
		guard := newLockGuard(t, pools, events)
		if err := guard.Client().Pools().DeletePool(t.Context(), lockGuardPool); err == nil || guard.Err() == nil {
			t.Fatal("an unobserved delete was accepted")
		}
	})
	t.Run("claim unreadable before delete", func(t *testing.T) {
		pools := &lockGuardPools{found: true, comment: "owner=me exp=1", readErr: errors.New("read failed")}
		events := &lockGuardEvents{}
		guard := newLockGuard(t, pools, events)
		if err := guard.Client().Pools().DeletePool(t.Context(), lockGuardPool); err == nil || guard.Err() == nil {
			t.Fatal("a delete without readable evidence was accepted")
		}
	})
}

// TestManagedLifecycleLockObservation covers the lifecycle guard's readback of
// the same three sentinel outcomes.
func TestManagedLifecycleLockObservation(t *testing.T) {
	const volume = "nfs-persistent-1:20768/owned.qcow2"
	pools := &lockGuardPools{found: true, comment: "owner=waiter exp=2"}
	guard := managedDiskLifecycleGuard{lifecycle: &managedDiskLifecycle{deps: Deps{PVE: lockGuardClient{pools: pools}}, disk: resolvedDisk{volid: volume}}}
	create := ManagedAllocationMutation{Service: managedDiskServicePool, Method: "CreatePool", Args: map[string]any{managedArgumentPoolID: lockGuardPool, "comment": "owner=me exp=1"}}
	if _, err := guard.observePool(t.Context(), create, nil); err != nil {
		t.Fatalf("a create displaced before its readback was not observed: %v", err)
	}
	// PVE's acceptance is the observation, so a read that would fail no
	// longer makes an accepted create uncertain. A delete still needs one.
	pools.readErr = errors.New("read failed")
	if _, err := guard.observePool(t.Context(), create, nil); err != nil {
		t.Fatalf("an accepted create was not observed while a read would fail: %v", err)
	}
	pools.readErr = nil
	volumes, err := guard.observePool(t.Context(), create, managedLockPoolRejection{poolID: lockGuardPool, method: "CreatePool"})
	if err != nil || len(volumes) != 1 || volumes[0] != volume {
		t.Fatalf("refused create was not settled: %v %v", volumes, err)
	}
	if _, err := guard.observePool(t.Context(), create, managedLockPoolRejection{poolID: "bosh-lock-other", method: "CreatePool"}); err == nil {
		t.Fatal("refusal evidence for another pool was accepted")
	}
	remove := ManagedAllocationMutation{Service: managedDiskServicePool, Method: "DeletePool", Args: map[string]any{managedArgumentPoolID: lockGuardPool}}
	if _, err := guard.observePool(t.Context(), remove, managedLockPoolDeletion{poolID: lockGuardPool, comment: "owner=me exp=1", known: true}); err != nil {
		t.Fatalf("successor sentinel hid our delete: %v", err)
	}
	if _, err := guard.observePool(t.Context(), remove, managedLockPoolDeletion{poolID: lockGuardPool, comment: "owner=waiter exp=2", known: true}); err == nil {
		t.Fatal("the deleted sentinel is still present, yet the delete was observed")
	}
	if _, err := guard.observePool(t.Context(), remove, nil); err == nil {
		t.Fatal("a present sentinel without delete evidence was observed as deleted")
	}
}

// storeClient serves one pool service to a guard.
type storeClient struct {
	pve.Client
	pools pve.PoolService
}

func (c storeClient) Pools() pve.PoolService { return c.pools }

// TestGuardedAcquireNeverSharesAWindowWithAStealer runs three contenders for
// one parker lock through a guard. Holder H crashed, so its claim expired, and
// stealers S1 and S3 both read that expired claim. S1 deletes H's sentinel.
// Waiter W's create then lands and succeeds, and S3's steal deletes W's fresh
// sentinel and recreates its own, which S3 verifies and holds. The guard
// settles W's displaced create as observed, so the lock code must read its
// claim back, find S3's, and wait instead of entering S3's window too.
func TestGuardedAcquireNeverSharesAWindowWithAStealer(t *testing.T) {
	t.Parallel()
	locks := newLockContention(t)
	sentinel := pve.ClusterLockPoolName("vm-90000")
	s3Claim := fmt.Sprintf("owner=steal/90000@3-1 exp=%d", time.Now().Add(time.Hour).Unix())
	locks.afterCreate = func(pool string) {
		if pool == sentinel {
			locks.pools[pool] = s3Claim
			locks.afterCreate = nil
		}
	}
	client := storeClient{pools: contendedPools{locks: locks}}
	failed := 0
	guard, err := NewManagedAllocationGuard(client, ManagedAllocationHooks{
		Before: func(context.Context, ManagedAllocationMutation) (string, error) { return "step", nil },
		After: func(ctx context.Context, call ManagedAllocationMutation, _ string, result any) error {
			return observeLockPoolMutation(ctx, client.Pools(), call, result, "test")
		},
		Failed: func(context.Context, ManagedAllocationMutation, string, error) error {
			failed++
			return errors.New("allocation requires reconciliation")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := pve.AcquireClusterLock(withShortLockPoll(t.Context()), guard.Client().Pools(), "vm-90000", "unpark/90000@9-1", time.Minute, testManagedLockWait)
	if handle != nil {
		t.Fatal("the waiter took a lock handle while the stealer holds the sentinel, so both are inside one window")
	}
	if !errors.Is(err, pve.ErrClusterLockTimeout) {
		t.Fatalf("want the waiter to time out behind the stealer, got %v", err)
	}
	if guard.Err() != nil || failed != 0 {
		t.Fatalf("waiting behind a stealer poisoned the guard: %v", guard.Err())
	}
	locks.mu.Lock()
	defer locks.mu.Unlock()
	if locks.pools[sentinel] != s3Claim {
		t.Fatalf("the stealer's claim was disturbed: %q", locks.pools[sentinel])
	}
}

// TestGuardedStealDeleteRefusesAChangedClaim covers the guard's last check
// before a steal's delete. The lock code passes the claim it judged expired,
// and the guard reads the sentinel once more right before it would call PVE.
// When the sentinel now holds another claim, or none, the delete is refused
// without reaching PVE, the refusal is settled as a non-mutation, and the lock
// code gets ErrLockClaimChanged so it abandons the steal.
func TestGuardedStealDeleteRefusesAChangedClaim(t *testing.T) {
	for name, pools := range map[string]*lockGuardPools{
		"a live claim replaced the judged one": {found: true, comment: "owner=S1@h/2-b-1 exp=99999999999"},
		"the sentinel is already gone":         {},
	} {
		t.Run(name, func(t *testing.T) {
			events := &lockGuardEvents{}
			guard := newLockGuard(t, pools, events)
			ctx := pve.WithExpectedLockClaim(t.Context(), "owner=crashed@h/1-a-1 exp=1")
			err := guard.Client().Pools().DeletePool(ctx, lockGuardPool)
			if !errors.Is(err, pve.ErrLockClaimChanged) {
				t.Fatalf("want ErrLockClaimChanged, got %v", err)
			}
			for _, call := range pools.calls {
				if strings.HasPrefix(call, "delete:") {
					t.Fatalf("the refused delete reached PVE: %v", pools.calls)
				}
			}
			if guard.Err() != nil || events.failed != 0 {
				t.Fatalf("a refused steal delete poisoned the guard: %v", guard.Err())
			}
		})
	}
	t.Run("the judged claim is still there", func(t *testing.T) {
		pools := &lockGuardPools{found: true, comment: "owner=crashed@h/1-a-1 exp=1"}
		guard := newLockGuard(t, pools, &lockGuardEvents{})
		ctx := pve.WithExpectedLockClaim(t.Context(), "owner=crashed@h/1-a-1 exp=1")
		if err := guard.Client().Pools().DeletePool(ctx, lockGuardPool); err != nil || guard.Err() != nil {
			t.Fatalf("the steal of the judged claim was refused: %v", err)
		}
		if pools.found {
			t.Fatal("the judged claim was not deleted")
		}
	})
}

// deletedByPVE reports whether a sentinel delete reached PVE.
func deletedByPVE(pools *lockGuardPools) bool {
	for _, call := range pools.calls {
		if strings.HasPrefix(call, "delete:") {
			return true
		}
	}
	return false
}

// TestGuardedLockDeleteReadErrorIsNotAChangedClaim covers a failed read in the
// guard's last check before a delete that expects a claim. A read that did not
// answer tells us nothing about the claim, so the delete is refused without
// reaching PVE and comes back as a retriable error, not as
// ErrLockClaimChanged, which the lock code reads as "ours is gone".
func TestGuardedLockDeleteReadErrorIsNotAChangedClaim(t *testing.T) {
	pools := &lockGuardPools{found: true, comment: "owner=me@h/1-a-1 exp=99999999999", readErr: errors.New("connection reset by peer")}
	events := &lockGuardEvents{}
	guard := newLockGuard(t, pools, events)
	ctx := pve.WithExpectedLockClaim(t.Context(), "owner=me@h/1-a-1 exp=99999999999")
	err := guard.Client().Pools().DeletePool(ctx, lockGuardPool)
	if err == nil || errors.Is(err, pve.ErrLockClaimChanged) {
		t.Fatalf("a failed pre-delete read must not read as a changed claim, got %v", err)
	}
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("a failed pre-delete read must be retriable, got %v", err)
	}
	if deletedByPVE(pools) || !pools.found {
		t.Fatalf("the delete reached PVE although the guard could not read the claim: %v", pools.calls)
	}
	if guard.Err() != nil || events.failed != 0 {
		t.Fatalf("a refused delete poisoned the guard: %v", guard.Err())
	}
}

// TestGuardedReleaseRetriesAfterAFailedPreDeleteRead is the Release side of
// the same case. A transient failure of the guard's read must not latch the
// handle released while our live claim stands on the sentinel, or the claim
// strands every other request for its whole TTL. The first Release fails and
// leaves the sentinel, and a second Release deletes it.
func TestGuardedReleaseRetriesAfterAFailedPreDeleteRead(t *testing.T) {
	pools := &lockGuardPools{}
	guard := newLockGuard(t, pools, &lockGuardEvents{})
	handle, err := pve.AcquireClusterLock(t.Context(), guard.Client().Pools(), "vm-90372", pve.ProcessLockOwner("unpark/90372"), time.Minute, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	// Release reads the claim first, and the guard's own read comes next.
	pools.readErrs = []error{nil, errors.New("connection reset by peer")}
	first := handle.Release(t.Context())
	if first == nil && pools.found {
		t.Fatalf("Release reported success but left our claim on the sentinel: %v", pools.calls)
	}
	if first != nil {
		if errors.Is(first, pve.ErrLockClaimChanged) || !pools.found {
			t.Fatalf("first Release: err=%v sentinel standing=%v", first, pools.found)
		}
		if err := handle.Release(t.Context()); err != nil {
			t.Fatalf("the retried Release failed: %v", err)
		}
	}
	if pools.found {
		t.Fatalf("our sentinel is still standing: %v", pools.calls)
	}
	if guard.Err() != nil {
		t.Fatalf("the release poisoned the guard: %v", guard.Err())
	}
}

// TestGuardedStealReturnsAFailedPreDeleteRead covers the steal side. When the
// guard cannot read the expired claim right before the steal's delete, the
// acquire returns a retriable error at once rather than giving up the steal
// quietly and waiting out its whole timeout behind a claim nobody holds.
func TestGuardedStealReturnsAFailedPreDeleteRead(t *testing.T) {
	pools := &lockGuardPools{
		createErr: livePoolVerdict(t, "create pool failed: pool '"+lockGuardPool+"' already exists"),
		found:     true, comment: "owner=crashed@h/1-a-1 exp=1",
		// The steal reads the holder, re-reads it, and then the guard reads.
		readErrs: []error{nil, nil, errors.New("connection reset by peer")},
	}
	guard := newLockGuard(t, pools, &lockGuardEvents{})
	handle, err := pve.AcquireClusterLock(t.Context(), guard.Client().Pools(), "vm-90372", pve.ProcessLockOwner("unpark/90372"), time.Minute, 2*time.Second)
	if handle != nil {
		t.Fatal("the steal took the lock without deleting the expired claim")
	}
	if err == nil || errors.Is(err, pve.ErrClusterLockTimeout) || !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("want the retriable read failure from the steal, got %v", err)
	}
	if deletedByPVE(pools) {
		t.Fatalf("the steal deleted without the guard's read: %v", pools.calls)
	}
	if guard.Err() != nil {
		t.Fatalf("the refused steal poisoned the guard: %v", guard.Err())
	}
}

// TestGuardedReleaseDeletesWhenPVENormalizesTheClaim covers a PVE that does
// not return a pool comment byte for byte as it was sent. The handle compares
// against the claim its confirming read returned, so the guard's check before
// the delete compares two reads and still matches, and Release removes our
// sentinel instead of stranding it for the TTL.
func TestGuardedReleaseDeletesWhenPVENormalizesTheClaim(t *testing.T) {
	pools := &lockGuardPools{normalize: func(comment string) string { return comment + "\n" }}
	guard := newLockGuard(t, pools, &lockGuardEvents{})
	handle, err := pve.AcquireClusterLock(t.Context(), guard.Client().Pools(), "vm-90372", pve.ProcessLockOwner("unpark/90372"), time.Minute, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := handle.Release(t.Context()); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if pools.found {
		t.Fatalf("Release left our sentinel standing: %q; calls %v", pools.comment, pools.calls)
	}
	if guard.Err() != nil {
		t.Fatalf("the release poisoned the guard: %v", guard.Err())
	}
}

// TestPoisonedGuardReleasesAProvenOwnSentinel covers our own release after the
// guard was poisoned. begin refuses every write on a poisoned guard, so this
// release used to be refused and our claim stood for a whole TTL. The release
// is marked as our own claim, and the guard's read right before the delete
// finds exactly that claim, so the sentinel is deleted.
func TestPoisonedGuardReleasesAProvenOwnSentinel(t *testing.T) {
	pools := &lockGuardPools{}
	guard := newLockGuard(t, pools, &lockGuardEvents{})
	handle, err := pve.AcquireClusterLock(t.Context(), guard.Client().Pools(), "vm-90372", pve.ProcessLockOwner("unpark/90372"), time.Minute, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = guard.Poison(errors.New("an earlier write was uncertain"))
	if err := handle.Release(t.Context()); err != nil {
		t.Fatalf("the poisoned guard refused our own release: %v", err)
	}
	if pools.found || !deletedByPVE(pools) {
		t.Fatalf("our sentinel is still standing: %v", pools.calls)
	}
}

// TestPoisonedGuardLeavesAClaimItCannotProve is the other side. A poisoned
// guard deletes nothing it cannot prove is our own claim: not when its read
// right before the delete fails, not when that read finds another claim, and
// never for a delete that is not marked as our own, such as a steal's.
func TestPoisonedGuardLeavesAClaimItCannotProve(t *testing.T) {
	for name, script := range map[string]func(p *lockGuardPools){
		// Release reads the claim first, and the guard's own read comes next.
		"the read fails": func(p *lockGuardPools) { p.readErrs = []error{nil, errors.New("connection reset by peer")} },
		"another claim":  func(p *lockGuardPools) { p.readComments = []string{"", "owner=S1@h/2-b-1 exp=99999999999"} },
	} {
		t.Run(name, func(t *testing.T) {
			pools := &lockGuardPools{}
			guard := newLockGuard(t, pools, &lockGuardEvents{})
			handle, err := pve.AcquireClusterLock(t.Context(), guard.Client().Pools(), "vm-90372", pve.ProcessLockOwner("unpark/90372"), time.Minute, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			_ = guard.Poison(errors.New("an earlier write was uncertain"))
			script(pools)
			_ = handle.Release(t.Context())
			if deletedByPVE(pools) || !pools.found {
				t.Fatalf("the poisoned guard deleted a claim it could not prove: %v", pools.calls)
			}
		})
	}
	t.Run("a steal's delete", func(t *testing.T) {
		const expired = "owner=crashed@h/3-c-1 exp=1"
		pools := &lockGuardPools{found: true, comment: expired}
		guard := newLockGuard(t, pools, &lockGuardEvents{})
		_ = guard.Poison(errors.New("an earlier write was uncertain"))
		if err := guard.Client().Pools().DeletePool(pve.WithExpectedLockClaim(t.Context(), expired), lockGuardPool); err == nil {
			t.Fatal("the poisoned guard accepted a steal's delete")
		}
		if deletedByPVE(pools) || !pools.found {
			t.Fatalf("the poisoned guard deleted someone else's claim: %v", pools.calls)
		}
	})
}

// TestPoisonedGuardLockCreateIsMarkedNotAttempted pins the mark on a lock
// create that a poisoned guard refuses. Like every other write on the guard,
// the create is refused before it reaches PVE. The parker lock fails its call
// retriable on the mark, where an unmarked fault would let its window run
// unserialized.
func TestPoisonedGuardLockCreateIsMarkedNotAttempted(t *testing.T) {
	pools := &lockGuardPools{}
	guard := newLockGuard(t, pools, &lockGuardEvents{})
	_ = guard.Poison(errors.New("an earlier write was uncertain"))
	err := guard.Client().Pools().CreatePool(t.Context(), lockGuardPool, "owner=x exp=9999999999")
	if !errors.Is(err, pve.ErrMutationNotAttempted) {
		t.Fatalf("the poisoned guard's lock create was not marked not attempted: %v", err)
	}
	if len(pools.calls) != 0 {
		t.Fatalf("the poisoned guard's lock create reached PVE: %v", pools.calls)
	}
}

// TestGuardedReleaseReadFailureNeverReadsAWaiterAsUnobserved pins why a
// guarded release refuses its delete when its read right before the delete
// fails. A waiter polls for exactly the moment our sentinel goes, and its new
// sentinel can land before our readback. Without a known claim from before the
// delete, that readback could not tell the waiter's sentinel from ours and
// would report "lock deletion not observed", poisoning an allocation whose
// release worked. expectedLockClaimRefusal refuses first, before PVE is
// called, with a retriable error, so the release is retried instead.
func TestGuardedReleaseReadFailureNeverReadsAWaiterAsUnobserved(t *testing.T) {
	pools := &lockGuardPools{}
	events := &lockGuardEvents{}
	guard := newLockGuard(t, pools, events)
	handle, err := pve.AcquireClusterLock(t.Context(), guard.Client().Pools(), "vm-90372", pve.ProcessLockOwner("unpark/90372"), time.Minute, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	pools.successor = "owner=waiter@h/9-z-1 exp=99999999999"
	// Release reads the claim first, and the guard's own read comes next.
	pools.readErrs = []error{nil, errors.New("connection reset by peer")}
	err = handle.Release(t.Context())
	if err == nil || strings.Contains(err.Error(), "lock deletion not observed") {
		t.Fatalf("want the retriable pre-delete refusal, got %v", err)
	}
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("the refusal is not retriable: %v", err)
	}
	if deletedByPVE(pools) {
		t.Fatalf("the delete reached PVE without a known claim: %v", pools.calls)
	}
	if guard.Err() != nil || events.failed != 0 {
		t.Fatalf("the refused release poisoned the guard: %v", guard.Err())
	}
}
