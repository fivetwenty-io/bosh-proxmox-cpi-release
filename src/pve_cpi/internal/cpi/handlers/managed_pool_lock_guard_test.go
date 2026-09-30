package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

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
	calls []string
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
	raw := lockGuardClient{pools: pools}
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

// TestManagedLockCreateDisplacedAfterSuccess covers a stealer that acts between
// a successful create and its readback. PVE accepted the create, so the step
// is observed whatever the readback finds, and the lock code's own
// verification decides who holds the lock. Only an unreadable sentinel stays
// uncertain.
func TestManagedLockCreateDisplacedAfterSuccess(t *testing.T) {
	for _, tc := range []struct {
		name     string
		pools    *lockGuardPools
		poisoned bool
	}{
		{name: "sentinel gone", pools: &lockGuardPools{createGone: true}},
		{name: "another claim", pools: &lockGuardPools{createDisplaced: true}},
		{name: "readback fails", pools: &lockGuardPools{readErr: errors.New("read failed")}, poisoned: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := &lockGuardEvents{}
			guard := newLockGuard(t, tc.pools, events)
			err := guard.Client().Pools().CreatePool(t.Context(), lockGuardPool, "owner=me exp=1")
			if tc.poisoned {
				if err == nil || guard.Err() == nil || events.failed != 1 {
					t.Fatalf("an unreadable sentinel was accepted: %v", err)
				}
				return
			}
			if err != nil || guard.Err() != nil || events.failed != 0 {
				t.Fatalf("a displaced create poisoned the guard: err=%v poison=%v", err, guard.Err())
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
	pools.readErr = errors.New("read failed")
	if _, err := guard.observePool(t.Context(), create, nil); err == nil {
		t.Fatal("an unreadable sentinel was observed as our create")
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
