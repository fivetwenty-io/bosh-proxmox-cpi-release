package handlers

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// teardownFixture runs one vmTeardown against the lock-recovery fakes: a VM on
// pve01 that PVE HA does not manage, whose config, power state, and destroy
// each test scripts.
type teardownFixture struct {
	nodes         *lrNodesStub
	qemu          *lrQEMUStub
	deps          Deps
	preserveCalls int
	ownedCalls    int
	preserveErr   error
	ownedErr      error
	awaitErr      error
}

func newTeardownFixture(nodes *lrNodesStub, q *lrQEMUStub) *teardownFixture {
	return &teardownFixture{
		nodes: nodes,
		qemu:  q,
		deps:  Deps{Config: lrTokenConfig(), PVE: &lrClient{nodes: nodes, qemu: q}, Logger: log.NewNopLogger()},
	}
}

func (f *teardownFixture) teardown() *vmTeardown {
	return &vmTeardown{
		caller:         "test teardown",
		vmCID:          "100",
		vmid:           100,
		node:           "pve01",
		logger:         log.NewNopLogger(),
		gate:           teardownGateListed,
		awaitLockClear: true,
		stop:           func(context.Context) (bool, error) { return false, nil },
		preserve: func(context.Context) (bool, error) {
			f.preserveCalls++
			return false, f.preserveErr
		},
		owned: func(context.Context, pve.QemuHolding) error {
			f.ownedCalls++
			return f.ownedErr
		},
		awaitTask: func(context.Context, *sdknodes.DeleteQemuResponse) error { return f.awaitErr },
	}
}

func (f *teardownFixture) run(t *testing.T) (teardownResult, error) {
	t.Helper()
	return f.teardown().run(crhCtx(), f.deps)
}

func requireRetriableTeardown(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("want a retriable error naming %q, got nil", want)
	}
	if !okToRetryCPIError(err) {
		t.Fatalf("want a retriable error, got %T %v", err, err)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error %q does not name %q", err, want)
	}
}

func TestVMTeardown_DestroysAndProvesAbsence(t *testing.T) {
	f := newTeardownFixture(&lrNodesStub{}, &lrQEMUStub{})

	result, err := f.run(t)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.gone || result.stage != teardownStageConfirm {
		t.Fatalf("result = %+v; want a destroy confirmed by this run", result)
	}
	if len(f.nodes.deleteCalls) != 1 {
		t.Fatalf("want 1 destroy, got %d", len(f.nodes.deleteCalls))
	}
	call := f.nodes.deleteCalls[0]
	if call.Purge == nil || !*call.Purge {
		t.Error("the destroy must purge")
	}
	if call.Skiplock != nil && *call.Skiplock {
		t.Error("the first destroy never carries skiplock")
	}
	if f.preserveCalls != 1 || f.ownedCalls != 1 {
		t.Errorf("preserve and ownership checks ran %d and %d times; want once each", f.preserveCalls, f.ownedCalls)
	}
	if f.qemu.statusCalls != 1 {
		t.Errorf("want one power-state read before the destroy, got %d", f.qemu.statusCalls)
	}
}

func TestVMTeardown_RefusesWithoutPreserveOrOwnership(t *testing.T) {
	for name, mutate := range map[string]func(*vmTeardown){
		"no preserve": func(td *vmTeardown) { td.preserve = nil },
		"no owned":    func(td *vmTeardown) { td.owned = nil },
	} {
		t.Run(name, func(t *testing.T) {
			f := newTeardownFixture(&lrNodesStub{}, &lrQEMUStub{})
			td := f.teardown()
			mutate(td)
			_, err := td.run(crhCtx(), f.deps)
			if err == nil || okToRetryCPIError(err) {
				t.Fatalf("want a non-retriable refusal, got %v", err)
			}
			if len(f.nodes.deleteCalls) != 0 || f.qemu.stopCalls != 0 {
				t.Fatal("a teardown without its guards must touch nothing")
			}
		})
	}
}

func TestVMTeardown_PreserveFailureLeavesTheVM(t *testing.T) {
	f := newTeardownFixture(&lrNodesStub{}, &lrQEMUStub{})
	f.preserveErr = cpierrors.Cloud("persistent disk could not be detached")

	result, err := f.run(t)
	if !errors.Is(err, f.preserveErr) {
		t.Fatalf("want the preserve error, got %v", err)
	}
	if result.stage != teardownStagePreserve {
		t.Errorf("stage = %q; want %q", result.stage, teardownStagePreserve)
	}
	if len(f.nodes.deleteCalls) != 0 {
		t.Fatal("a failed preserve must never reach the destroy")
	}
}

func TestVMTeardown_MigrateLockDefersAtTheCheck(t *testing.T) {
	q := &lrQEMUStub{configFn: func(int) (map[string]any, error) {
		return map[string]any{"lock": "migrate"}, nil
	}}
	f := newTeardownFixture(&lrNodesStub{}, q)

	result, err := f.run(t)
	requireRetriableTeardown(t, err, "migrate lock")
	if result.stage != teardownStageVerify {
		t.Errorf("stage = %q; want %q", result.stage, teardownStageVerify)
	}
	if len(f.nodes.deleteCalls) != 0 {
		t.Fatal("a migrating VM must never be destroyed")
	}
	if f.ownedCalls != 0 {
		t.Error("the ownership check runs only on a VM the lock check passed")
	}
}

func TestVMTeardown_OwnershipRefusalLeavesTheVM(t *testing.T) {
	f := newTeardownFixture(&lrNodesStub{}, &lrQEMUStub{})
	f.ownedErr = cpierrors.Cloud("VM 100 carries another Director's owner tag")

	result, err := f.run(t)
	if !errors.Is(err, f.ownedErr) {
		t.Fatalf("want the ownership refusal, got %v", err)
	}
	if result.stage != teardownStageVerify {
		t.Errorf("stage = %q; want %q", result.stage, teardownStageVerify)
	}
	if len(f.nodes.deleteCalls) != 0 {
		t.Fatal("a VM the ownership check refuses must never be destroyed")
	}
}

func TestVMTeardown_PowerStateReadFailureDefers(t *testing.T) {
	q := &lrQEMUStub{statusErr: errors.New("500 status read failed")}
	f := newTeardownFixture(&lrNodesStub{}, q)

	result, err := f.run(t)
	requireRetriableTeardown(t, err, "power state")
	if result.stage != teardownStageVerify || len(f.nodes.deleteCalls) != 0 {
		t.Fatalf("an unreadable power state must stop the teardown before the destroy; stage %q, destroys %d",
			result.stage, len(f.nodes.deleteCalls))
	}
}

func TestVMTeardown_VMGoneBeforeTheDestroyIsIdempotent(t *testing.T) {
	notFound := &sdkerrors.APIError{HTTPCode: 404}
	q := &lrQEMUStub{
		statusErr: notFound,
		configFn:  func(int) (map[string]any, error) { return nil, notFound },
	}
	f := newTeardownFixture(&lrNodesStub{}, q)

	result, err := f.run(t)
	if err != nil {
		t.Fatalf("a VM absent from every member is an idempotent success, got %v", err)
	}
	if !result.gone || result.stage != teardownStageVerify {
		t.Fatalf("result = %+v; want gone at the check before the destroy", result)
	}
	if len(f.nodes.deleteCalls) != 0 {
		t.Fatal("a VM proven absent needs no destroy")
	}
}

func TestVMTeardown_StillPresentAfterTheDestroyIsRetriable(t *testing.T) {
	f := newTeardownFixture(&lrNodesStub{survives: true}, &lrQEMUStub{})

	result, err := f.run(t)
	requireRetriableTeardown(t, err, "still present")
	if result.stage != teardownStageConfirm {
		t.Errorf("stage = %q; want %q", result.stage, teardownStageConfirm)
	}
}

func TestVMTeardown_AwaitErrorWithTheVMStillPresentIsReturned(t *testing.T) {
	f := newTeardownFixture(&lrNodesStub{survives: true}, &lrQEMUStub{})
	f.awaitErr = cpierrors.Cloud("destroy task ended with ERROR")

	_, err := f.run(t)
	if !errors.Is(err, f.awaitErr) {
		t.Fatalf("want the await error, got %v", err)
	}
}

func TestVMTeardown_AwaitErrorWithTheVMAbsentSucceeds(t *testing.T) {
	f := newTeardownFixture(&lrNodesStub{}, &lrQEMUStub{})
	f.awaitErr = cpierrors.Cloud("destroy task status unreadable")

	if _, err := f.run(t); err != nil {
		t.Fatalf("the absence proof decides, not the task answer; got %v", err)
	}
}

func TestVMTeardown_LockClearRechecksBeforeTheSecondDestroy(t *testing.T) {
	defer SetVMLockClearBounds(time.Millisecond, time.Second)()
	nodes := &lrNodesStub{deleteErrs: []error{errors.New(lockedCloneMsg), nil}}
	f := newTeardownFixture(nodes, &lrQEMUStub{})

	if _, err := f.run(t); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(nodes.deleteCalls) != 2 {
		t.Fatalf("want the refused destroy and the one after the lock cleared, got %d", len(nodes.deleteCalls))
	}
	if f.preserveCalls != 2 || f.ownedCalls != 2 {
		t.Fatalf("steps 3 and 4 must run again after the lock clears; preserve %d, owned %d",
			f.preserveCalls, f.ownedCalls)
	}
}

func TestVMTeardown_LockClearRecheckRefusalStopsTheSecondDestroy(t *testing.T) {
	defer SetVMLockClearBounds(time.Millisecond, time.Second)()
	nodes := &lrNodesStub{deleteErrs: []error{errors.New(lockedCloneMsg), nil}}
	f := newTeardownFixture(nodes, &lrQEMUStub{})
	td := f.teardown()
	preserve := td.preserve
	td.preserve = func(ctx context.Context) (bool, error) {
		if f.preserveCalls == 1 {
			f.preserveCalls++
			return false, cpierrors.Cloud("the lock's task attached a persistent disk")
		}
		return preserve(ctx)
	}

	if _, err := td.run(crhCtx(), f.deps); err == nil {
		t.Fatal("a recheck refusal must fail the teardown")
	}
	if len(nodes.deleteCalls) != 1 {
		t.Fatalf("the recheck refusal must keep the second destroy from running, got %d destroys",
			len(nodes.deleteCalls))
	}
}

func TestVMTeardown_LockedDestroyWithoutRecoveryIsALockError(t *testing.T) {
	nodes := &lrNodesStub{deleteErrs: []error{errors.New(lockedCloneMsg)}}
	f := newTeardownFixture(nodes, &lrQEMUStub{})
	td := f.teardown()
	td.awaitLockClear = false

	result, err := td.run(crhCtx(), f.deps)
	if err == nil || !pve.IsVMConfigLocked(err) {
		t.Fatalf("want a config-lock error, got %v", err)
	}
	if result.stage != teardownStageDestroy || len(nodes.deleteCalls) != 1 {
		t.Fatalf("stage %q, destroys %d; want one refused destroy", result.stage, len(nodes.deleteCalls))
	}
}

func TestAwaitVMConfigLockClear_FailsClosed(t *testing.T) {
	defer SetVMLockClearBounds(time.Millisecond, 50*time.Millisecond)()
	cases := map[string]struct {
		cfg       func(int) (map[string]any, error)
		want      bool
		wantReads int
	}{
		"lock cleared": {cfg: func(int) (map[string]any, error) { return map[string]any{}, nil }, want: true, wantReads: 1},
		"not found": {cfg: func(int) (map[string]any, error) {
			return nil, &sdkerrors.APIError{HTTPCode: 404}
		}, want: true, wantReads: 1},
		"read failure": {cfg: func(int) (map[string]any, error) {
			return nil, errors.New("500 internal error")
		}, want: false, wantReads: 1},
		"migrate lock": {cfg: func(int) (map[string]any, error) {
			return map[string]any{"lock": "migrate"}, nil
		}, want: false, wantReads: 1},
		"clone lock clears": {cfg: func(call int) (map[string]any, error) {
			if call < 3 {
				return map[string]any{"lock": "clone"}, nil
			}
			return map[string]any{}, nil
		}, want: true, wantReads: 3},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			q := &lrQEMUStub{configFn: tc.cfg}
			deps := Deps{Config: lrTokenConfig(), PVE: &lrClient{nodes: &lrNodesStub{}, qemu: q}, Logger: log.NewNopLogger()}
			if got := awaitVMConfigLockClear(context.Background(), deps, "pve01", 100, log.NewNopLogger()); got != tc.want {
				t.Fatalf("awaitVMConfigLockClear = %v; want %v", got, tc.want)
			}
			if q.configCalls != tc.wantReads {
				t.Errorf("config reads = %d; want %d", q.configCalls, tc.wantReads)
			}
		})
	}
}

func TestRetryDestroyAfterLockClear_Recheck(t *testing.T) {
	defer SetVMLockClearBounds(time.Millisecond, time.Second)()
	origErr := errors.New(lockedCloneMsg)
	t.Run("gone", func(t *testing.T) {
		nodes := &lrNodesStub{}
		deps := Deps{Config: lrTokenConfig(), PVE: &lrClient{nodes: nodes, qemu: &lrQEMUStub{}}, Logger: log.NewNopLogger()}
		resp, err := retryDestroyAfterLockClear(crhCtx(), deps, "pve01", 100, false, origErr,
			func(context.Context) (bool, error) { return true, nil }, log.NewNopLogger())
		if resp != nil || err != nil {
			t.Fatalf("a VM that left while the lock cleared needs no destroy; got %v, %v", resp, err)
		}
		if len(nodes.deleteCalls) != 0 {
			t.Fatalf("want no destroy, got %d", len(nodes.deleteCalls))
		}
	})
	t.Run("refused", func(t *testing.T) {
		nodes := &lrNodesStub{}
		deps := Deps{Config: lrTokenConfig(), PVE: &lrClient{nodes: nodes, qemu: &lrQEMUStub{}}, Logger: log.NewNopLogger()}
		refusal := cpierrors.Cloud("recheck refused")
		_, err := retryDestroyAfterLockClear(crhCtx(), deps, "pve01", 100, false, origErr,
			func(context.Context) (bool, error) { return false, refusal }, log.NewNopLogger())
		if !errors.Is(err, refusal) {
			t.Fatalf("want the recheck refusal, got %v", err)
		}
		if len(nodes.deleteCalls) != 0 {
			t.Fatalf("want no destroy, got %d", len(nodes.deleteCalls))
		}
	})
	t.Run("lock never clears", func(t *testing.T) {
		defer SetVMLockClearBounds(time.Millisecond, 5*time.Millisecond)()
		nodes := &lrNodesStub{}
		q := &lrQEMUStub{configFn: func(int) (map[string]any, error) { return map[string]any{"lock": "clone"}, nil }}
		deps := Deps{Config: lrTokenConfig(), PVE: &lrClient{nodes: nodes, qemu: q}, Logger: log.NewNopLogger()}
		rechecked := false
		_, err := retryDestroyAfterLockClear(crhCtx(), deps, "pve01", 100, false, origErr,
			func(context.Context) (bool, error) { rechecked = true; return false, nil }, log.NewNopLogger())
		if !errors.Is(err, origErr) {
			t.Fatalf("want the original lock error, got %v", err)
		}
		if rechecked || len(nodes.deleteCalls) != 0 {
			t.Fatalf("a lock that never clears must not recheck or destroy; rechecked %v, destroys %d",
				rechecked, len(nodes.deleteCalls))
		}
	})
}

func TestRequireStoppedOnOwner(t *testing.T) {
	cases := map[string]struct {
		q       *lrQEMUStub
		wantErr string
	}{
		"stopped":     {q: &lrQEMUStub{}},
		"running":     {q: &lrQEMUStub{status: "running"}, wantErr: "is not stopped"},
		"read failed": {q: &lrQEMUStub{statusErr: errors.New("500 status read failed")}, wantErr: "power state"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			deps := Deps{Config: lrTokenConfig(), PVE: &lrClient{nodes: &lrNodesStub{}, qemu: tc.q}, Logger: log.NewNopLogger()}
			err := requireStoppedOnOwner(context.Background(), deps, "delete_vm", "pve01", 100)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			requireRetriableTeardown(t, err, tc.wantErr)
		})
	}
}

// haReadFailingCluster is the node-affinity cluster stub whose HA resource
// list cannot be read.
type haReadFailingCluster struct{ *naClusterStub }

func (haReadFailingCluster) ListHaResources(context.Context, *cluster.ListHaResourcesParams) (*cluster.ListHaResourcesResponse, error) {
	return nil, errors.New("500 ha resource list unavailable")
}

// gateClient is the lock-recovery client with a cluster service of the test's
// choosing, so the teardown's HA gate reads a scripted HA state.
type gateClient struct {
	*lrClient
	cluster cluster.Service
}

func (c gateClient) Cluster() cluster.Service { return c.cluster }

func TestVMTeardown_GateNoWaitProceedsOnAnUnmanagedVM(t *testing.T) {
	f := newTeardownFixture(&lrNodesStub{}, &lrQEMUStub{})
	td := f.teardown()
	td.gate = teardownGateNoWait

	if _, err := td.run(crhCtx(), f.deps); err != nil {
		t.Fatalf("an unmanaged VM passes the no-wait gate, got %v", err)
	}
	if len(f.nodes.deleteCalls) != 1 {
		t.Fatalf("want one destroy, got %d", len(f.nodes.deleteCalls))
	}
}

func TestVMTeardown_GatesDeferWhenTheHAStateCannotBeRead(t *testing.T) {
	for name, gate := range map[string]vmTeardownGate{
		"listed":  teardownGateListed,
		"no wait": teardownGateNoWait,
	} {
		t.Run(name, func(t *testing.T) {
			nodes, q := &lrNodesStub{}, &lrQEMUStub{}
			f := newTeardownFixture(nodes, q)
			f.deps.PVE = gateClient{
				lrClient: &lrClient{nodes: nodes, qemu: q}, cluster: haReadFailingCluster{newNAStub()},
			}
			td := f.teardown()
			td.gate = gate
			stopped := false
			td.stop = func(context.Context) (bool, error) {
				stopped = true
				return false, nil
			}

			result, err := td.run(crhCtx(), f.deps)
			var deferral *haDeferral
			if !errors.As(err, &deferral) || !okToRetryCPIError(err) {
				t.Fatalf("want a retriable HA deferral, got %v", err)
			}
			if result.stage != teardownStageHA || stopped || len(nodes.deleteCalls) != 0 || f.preserveCalls != 0 {
				t.Fatalf("an unreadable HA state must touch nothing; stage %q, stopped %v, destroys %d, preserves %d",
					result.stage, stopped, len(nodes.deleteCalls), f.preserveCalls)
			}
		})
	}
}

func TestVMTeardown_LockClearRecheckOwnershipRefusalStopsTheSecondDestroy(t *testing.T) {
	defer SetVMLockClearBounds(time.Millisecond, time.Second)()
	nodes := &lrNodesStub{deleteErrs: []error{errors.New(lockedCloneMsg), nil}}
	f := newTeardownFixture(nodes, &lrQEMUStub{})
	td := f.teardown()
	td.owned = func(context.Context, pve.QemuHolding) error {
		f.ownedCalls++
		if f.ownedCalls > 1 {
			return cpierrors.Retriable("the VMID now holds a VM this Director does not own")
		}
		return nil
	}

	if _, err := td.run(crhCtx(), f.deps); err == nil {
		t.Fatal("an ownership refusal on the recheck must fail the teardown")
	}
	if len(nodes.deleteCalls) != 1 || f.ownedCalls != 2 {
		t.Fatalf("the recheck must re-prove ownership and keep the second destroy from running; "+
			"destroys %d, ownership checks %d", len(nodes.deleteCalls), f.ownedCalls)
	}
}
