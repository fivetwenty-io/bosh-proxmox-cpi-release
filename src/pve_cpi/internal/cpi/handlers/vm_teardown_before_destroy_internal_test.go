package handlers

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

// runWithBeforeDestroy runs the fixture's teardown with a beforeDestroy that
// records, at each call, how many destroys and ownership checks ran before it.
func (f *teardownFixture) runWithBeforeDestroy(t *testing.T) (calls [][2]int, err error) {
	t.Helper()
	td := f.teardown()
	td.beforeDestroy = func(context.Context, *pve.HAServiceState) {
		calls = append(calls, [2]int{len(f.nodes.deleteCalls), f.ownedCalls})
	}
	_, err = td.run(crhCtx(), f.deps)
	return calls, err
}

// TestVMTeardown_BeforeDestroyRunsOnceAfterTheCheckAndBeforeTheDestroy pins
// that the hook runs once, after the ownership check and before the destroy,
// while the VM still exists.
func TestVMTeardown_BeforeDestroyRunsOnceAfterTheCheckAndBeforeTheDestroy(t *testing.T) {
	f := newTeardownFixture(&lrNodesStub{}, &lrQEMUStub{})
	calls, err := f.runWithBeforeDestroy(t)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(calls) != 1 || calls[0] != [2]int{0, 1} {
		t.Fatalf("beforeDestroy calls (destroys, checks before it) = %v; want one call after the check and "+
			"before the destroy", calls)
	}
	if len(f.nodes.deleteCalls) != 1 {
		t.Fatalf("want 1 destroy, got %d", len(f.nodes.deleteCalls))
	}
}

// TestVMTeardown_BeforeDestroyRunsOnceAcrossALockClear pins that a destroy a
// config lock refused, and that runs again once the lock clears, runs the
// hook only before the first destroy.
func TestVMTeardown_BeforeDestroyRunsOnceAcrossALockClear(t *testing.T) {
	defer SetVMLockClearBounds(time.Millisecond, time.Second)()
	nodes := &lrNodesStub{deleteErrs: []error{errors.New(lockedCloneMsg), nil}}
	f := newTeardownFixture(nodes, &lrQEMUStub{})
	calls, err := f.runWithBeforeDestroy(t)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(calls) != 1 || len(nodes.deleteCalls) != 2 {
		t.Fatalf("beforeDestroy calls %v and destroys %d; want one call and two destroys", calls,
			len(nodes.deleteCalls))
	}
}

// TestVMTeardown_BeforeDestroySkipsAVMThatWillNotBeDestroyed pins that the
// hook does not run for a VM proven gone before the destroy, or one the
// ownership check refuses.
func TestVMTeardown_BeforeDestroySkipsAVMThatWillNotBeDestroyed(t *testing.T) {
	notFound := &sdkerrors.APIError{HTTPCode: 404}
	gone := newTeardownFixture(&lrNodesStub{}, &lrQEMUStub{
		statusErr: notFound,
		configFn:  func(int) (map[string]any, error) { return nil, notFound },
	})
	if calls, err := gone.runWithBeforeDestroy(t); err != nil || len(calls) != 0 {
		t.Fatalf("gone VM: beforeDestroy calls %v, error %v; want none and no error", calls, err)
	}
	refused := newTeardownFixture(&lrNodesStub{}, &lrQEMUStub{})
	refused.ownedErr = errors.New("not ours")
	if calls, err := refused.runWithBeforeDestroy(t); err == nil || len(calls) != 0 {
		t.Fatalf("refused VM: beforeDestroy calls %v, error %v; want none and the refusal", calls, err)
	}
}
