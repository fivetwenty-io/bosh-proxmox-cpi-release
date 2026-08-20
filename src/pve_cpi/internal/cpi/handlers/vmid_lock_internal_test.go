// Internal tests for the per-VMID cluster lock helper (withVMIDLock).
package handlers

import (
	"context"
	"fmt"
	"strings"
	"testing"

	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
)

// vmidLockPools is a minimal PoolService backed by an in-memory map; it records
// operations to an events slice shared with the test so acquire/release ordering
// can be asserted against the fn body.
type vmidLockPools struct {
	pools     map[string]string // poolid -> comment
	events    *[]string
	createErr error // when non-nil, every CreatePool returns this error immediately
}

func newVMIDLockPools(events *[]string) *vmidLockPools {
	return &vmidLockPools{pools: map[string]string{}, events: events}
}

func (p *vmidLockPools) record(ev string) {
	if p.events != nil {
		*p.events = append(*p.events, ev)
	}
}

func (p *vmidLockPools) AddVM(_ context.Context, _ string, _ int64) error        { return nil }
func (p *vmidLockPools) MoveVMToPool(_ context.Context, _ string, _ int64) error { return nil }

func (p *vmidLockPools) CreatePool(_ context.Context, poolID, comment string) error {
	p.record("create:" + poolID)
	if p.createErr != nil {
		return p.createErr
	}
	if _, ok := p.pools[poolID]; ok {
		return fmt.Errorf("pool '%s' already exists", poolID)
	}
	p.pools[poolID] = comment
	return nil
}

func (p *vmidLockPools) DeletePool(_ context.Context, poolID string) error {
	p.record("delete:" + poolID)
	if _, ok := p.pools[poolID]; !ok {
		return fmt.Errorf("pool '%s' does not exist", poolID)
	}
	delete(p.pools, poolID)
	return nil
}

func (p *vmidLockPools) GetPoolComment(_ context.Context, poolID string) (string, bool, error) {
	p.record("get:" + poolID)
	c, ok := p.pools[poolID]
	return c, ok, nil
}

var _ pve.PoolService = (*vmidLockPools)(nil)

// --------------------------------------------------------------------------
// TestWithVMIDLock_Success — fn runs and lock is released on success.
// --------------------------------------------------------------------------

func TestWithVMIDLock_Success(t *testing.T) {
	t.Parallel()

	events := []string{}
	pools := newVMIDLockPools(&events)
	fnRan := false

	err := withVMIDLock(context.Background(), pools, 12345, "test-owner", log.NewNopLogger(), func() error {
		events = append(events, "fn")
		fnRan = true
		return nil
	})

	if err != nil {
		t.Fatalf("withVMIDLock: unexpected error: %v", err)
	}
	if !fnRan {
		t.Fatal("fn was not called")
	}

	// Lock key must be "vm-12345" → pool "bosh-lock-vm-12345".
	expectedPool := "bosh-lock-vm-12345"

	// acquire (create) before fn, release (delete) after fn.
	if len(events) < 3 {
		t.Fatalf("expected at least 3 events (create, fn, delete); got %v", events)
	}
	if events[0] != "create:"+expectedPool {
		t.Errorf("first event must be lock acquire; got %q (events=%v)", events[0], events)
	}
	if events[1] != "fn" {
		t.Errorf("second event must be fn; got %q (events=%v)", events[1], events)
	}
	// Last event must be the delete (release).
	last := events[len(events)-1]
	if last != "delete:"+expectedPool {
		t.Errorf("last event must be lock release; got %q (events=%v)", last, events)
	}

	// Lock pool must be gone after release.
	if _, held := pools.pools[expectedPool]; held {
		t.Error("sentinel pool must be deleted after successful fn")
	}
}

// --------------------------------------------------------------------------
// TestWithVMIDLock_FnErrorReleasesLock — fn error propagates; lock still released.
// --------------------------------------------------------------------------

func TestWithVMIDLock_FnErrorReleasesLock(t *testing.T) {
	t.Parallel()

	events := []string{}
	pools := newVMIDLockPools(&events)
	fnErr := fmt.Errorf("fn body error")

	err := withVMIDLock(context.Background(), pools, 99, "test-owner", log.NewNopLogger(), func() error {
		events = append(events, "fn")
		return fnErr
	})

	if err == nil {
		t.Fatal("expected fn error to propagate")
	}
	if !strings.Contains(err.Error(), fnErr.Error()) {
		t.Errorf("returned error %q must contain fn error %q", err, fnErr)
	}

	expectedPool := "bosh-lock-vm-99"
	last := events[len(events)-1]
	if last != "delete:"+expectedPool {
		t.Errorf("lock must be released after fn error; last event=%q events=%v", last, events)
	}
	if _, held := pools.pools[expectedPool]; held {
		t.Error("sentinel pool must be gone after fn error")
	}
}

// --------------------------------------------------------------------------
// TestWithVMIDLock_AcquireFailureRetriable — lock acquire failure → retriable error,
// fn never called.
// --------------------------------------------------------------------------

func TestWithVMIDLock_AcquireFailureRetriable(t *testing.T) {
	t.Parallel()

	pools := newVMIDLockPools(nil)
	// Non-duplicate failure → immediately retriable, no poll loop.
	pools.createErr = fmt.Errorf("pmxcfs unavailable")
	fnRan := false

	err := withVMIDLock(context.Background(), pools, 7777, "test-owner", log.NewNopLogger(), func() error {
		fnRan = true
		return nil
	})

	if err == nil {
		t.Fatal("expected a retriable error when lock cannot be acquired")
	}
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Errorf("lock-acquire failure must be retriable; got %v (type=%T)", err, err)
	}
	if fnRan {
		t.Error("fn must not run when lock acquire fails")
	}
}

// --------------------------------------------------------------------------
// TestWithVMIDLock_LockKeyScheme — verifies pool name is "bosh-lock-vm-<vmid>".
// --------------------------------------------------------------------------

func TestWithVMIDLock_LockKeyScheme(t *testing.T) {
	t.Parallel()

	for _, vmid := range []int{1, 100, 90000, 999999} {
		vmid := vmid
		t.Run(fmt.Sprintf("vmid=%d", vmid), func(t *testing.T) {
			t.Parallel()
			events := []string{}
			pools := newVMIDLockPools(&events)
			_ = withVMIDLock(context.Background(), pools, vmid, "owner", log.NewNopLogger(), func() error {
				return nil
			})
			expectedPool := fmt.Sprintf("bosh-lock-vm-%d", vmid)
			if len(events) == 0 || events[0] != "create:"+expectedPool {
				t.Errorf("vmid=%d: expected first event create:%s; got %v", vmid, expectedPool, events)
			}
		})
	}
}

// --------------------------------------------------------------------------
// TestWithVMIDLock_NilPools — nil pool service → retriable error, fn not called.
// --------------------------------------------------------------------------

func TestWithVMIDLock_NilPools(t *testing.T) {
	t.Parallel()

	fnRan := false
	err := withVMIDLock(context.Background(), nil, 42, "owner", log.NewNopLogger(), func() error {
		fnRan = true
		return nil
	})
	if err == nil {
		t.Fatal("expected error with nil pool service")
	}
	if fnRan {
		t.Error("fn must not run with nil pool service")
	}
}
