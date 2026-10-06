package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
)

// endingRequest is a request context under operation_timeout whose deadline a
// test ends at a moment it chooses, with no real waiting. It keeps the values
// of the context it wraps. Until end is called it reports a deadline an hour
// out and is live. After that it reports the moment it ended as its deadline,
// its Done channel is closed, and Err is context.DeadlineExceeded, which is
// how a request whose per-method budget ran out looks to the work inside it.
type endingRequest struct {
	context.Context
	mu       sync.Mutex
	deadline time.Time
	ended    bool
	done     chan struct{}
}

func newEndingRequest(parent context.Context) *endingRequest {
	return &endingRequest{Context: parent, deadline: time.Now().Add(time.Hour), done: make(chan struct{})}
}

func (r *endingRequest) Deadline() (time.Time, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.deadline, true
}

func (r *endingRequest) Done() <-chan struct{} { return r.done }

func (r *endingRequest) Err() error {
	select {
	case <-r.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

// end runs the request out of time. Only the first call has an effect.
func (r *endingRequest) end() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ended {
		return
	}
	r.ended = true
	r.deadline = time.Now()
	close(r.done)
}

func (r *endingRequest) hasEnded() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ended
}

// endingReadClient ends req on the first VM config read it serves, and then
// answers that read the way the real client does, with the error of the
// context the read runs on when that context is done. It stands for a request
// whose deadline arrives while completion work is reading PVE back.
type endingReadClient struct {
	pve.Client
	req *endingRequest
}

func (c endingReadClient) QEMU() qemu.Service {
	return endingReadQEMU{Service: c.Client.QEMU(), req: c.req}
}

type endingReadQEMU struct {
	qemu.Service
	req *endingRequest
}

func (q endingReadQEMU) Config(ctx context.Context, node string, vmid int) (map[string]any, error) {
	q.req.end()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return q.Service.Config(ctx, node, vmid)
}

// TestCreateVMRollbackOutlivesTheRequestDeadline is the lock-timeout rollback
// with operation_timeout on. The managed lock wait gives up, and the request's
// deadline arrives just as the rollback's VM disposal begins. The disposal
// must still run to its end, so the generation closes, the VM is destroyed,
// both disks return to the parker, and the Director gets the retriable lock
// timeout. A disposal cut off by the deadline would leave the generation
// requiring reconciliation, and the Director's retry would be refused.
func TestCreateVMRollbackOutlivesTheRequestDeadline(t *testing.T) {
	t.Parallel()
	flow := newRollbackFlow(t)
	req := newEndingRequest(flow.ctx)
	flow.vms.onDisposal = func() {
		flow.locks.reset()
		req.end()
	}

	_, err := createVM(req, flow.parked.deps, flow.args)
	if !req.hasEnded() {
		t.Fatal("the rollback never began a disposal, so the request never ended")
	}
	if !errors.Is(err, pve.ErrClusterLockTimeout) || !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("create_vm did not hand the Director the retriable lock timeout: %v", err)
	}
	generations := flow.generations(t)
	if len(generations) != 1 || generations[0].State != aj.Deleted {
		states := make([]string, 0, len(generations))
		for i := range generations {
			states = append(states, string(generations[i].State)+" ("+generations[i].Reason+")")
		}
		t.Fatalf("the rolled-back generation was not closed: %v", states)
	}
	created := flow.created(t)
	if len(created) != 1 || len(flow.vms.destroyed) != 1 {
		t.Fatalf("the attempt's VM was not destroyed: created=%v destroyed=%v", created, flow.vms.destroyed)
	}
	for name, cid := range map[string]string{"attached": flow.free.cid, "parked": flow.parked.cid} {
		holder := flow.holderOf(t, cid)
		if holder == nil || !holder.IsParker || holder.VMID != flow.parked.parker {
			t.Fatalf("the %s disk is not on the parker after the rollback: %+v", name, holder)
		}
	}
	assertReturnedRecord(t, "attached disk", flow.diskRecord(t, flow.free.id))
	assertReturnedRecord(t, "parked disk", flow.parked.record(t))

	if _, err := flow.createVM(t); err != nil {
		t.Fatalf("the Director's retry was refused: %v", err)
	}
}

// TestManagedLifecycleCompletionOutlivesTheRequestDeadline drives
// lifecycle.finish with a clean lock timeout whose request deadline arrives
// during the completion audit's first read. The completion must still read
// the disk back and return the allocation, and the retriable timeout goes
// back unchanged. A completion cut off by the deadline would send the record
// to reconciliation even though the operation changed nothing.
func TestManagedLifecycleCompletionOutlivesTheRequestDeadline(t *testing.T) {
	t.Parallel()
	timeout := cpierrors.WrapAs(errors.Join(errors.New("held"), pve.ErrClusterLockTimeout), cpierrors.TypeRetriableCloud, "AcquireClusterLock: timed out")
	deps, _, journal, id, cid := lifecycleFlowFixtureState(t, true, true)
	bare, meta, err := decodeDiskCID(t.Context(), deps, "attach_disk", cid)
	if err != nil {
		t.Fatal(err)
	}
	rd, err := resolveDiskForOp(t.Context(), deps, "attach_disk", cid, bare, meta)
	if err != nil {
		t.Fatal(err)
	}
	_, lifecycle, err := managedDiskOperation(t.Context(), deps, rd, "attach_disk")
	if err != nil || lifecycle == nil {
		t.Fatalf("lifecycle admission: %v", err)
	}
	req := newEndingRequest(t.Context())
	lifecycle.deps.PVE = endingReadClient{Client: lifecycle.deps.PVE, req: req}

	result := lifecycle.finish(req, timeout, false)
	if !req.hasEnded() {
		t.Fatal("the completion never read the disk back, so the request never ended")
	}
	if !errors.Is(result, timeout) {
		t.Fatalf("finish lost the operation error: %v", result)
	}
	record, err := journal.Inspect(id)
	if err != nil {
		t.Fatal(err)
	}
	assertReturnedRecord(t, "timed-out disk", record)
}

// TestRetentionReadbackOutlivesTheRequestDeadline judges a clean delete_vm
// retention timeout whose request deadline arrives during the readback's
// first read. The readback must still finish and judge the timeout clean.
func TestRetentionReadbackOutlivesTheRequestDeadline(t *testing.T) {
	c, check, record, _ := cleanRetentionTimeout(t)
	req := newEndingRequest(t.Context())
	deps := c.deps
	deps.PVE = endingReadClient{Client: c.deps.PVE, req: req}
	if !check.returned(req, deps, record, lockTimeoutError(), nil) {
		t.Fatal("the request's deadline made the clean retention timeout unclean")
	}
	if !req.hasEnded() {
		t.Fatal("the readback never read the guest, so the request never ended")
	}
}

// TestDispatcherKeepsReconciliationAfterTheDeadline runs a create_vm whose
// rollback failed after the per-method deadline fired, leaving the allocation
// requiring reconciliation. The dispatcher must hand that refusal to the
// Director as it is, not as the retriable timeout, because a retry would be
// refused.
func TestDispatcherKeepsReconciliationAfterTheDeadline(t *testing.T) {
	t.Parallel()
	d := cpi.NewDispatcherWithOptions(log.NewNopLogger(), cpi.WithMethodTimeouts(func(string) time.Duration { return 20 * time.Millisecond }))
	handler := cpi.HandlerFunc(func(ctx context.Context, _ []json.RawMessage, _ jsonrpc.Context) (any, error) {
		<-ctx.Done()
		return nil, joinReconciliation(ctx.Err(), storageAllocationUncertain(nil, "VM create rollback"))
	})
	if err := d.Register("create_vm", handler); err != nil {
		t.Fatal(err)
	}
	resp := d.Handle(t.Context(), &jsonrpc.Request{Method: "create_vm", Context: jsonrpc.Context{RequestID: "reconciliation-after-deadline"}})
	if resp.Error == nil {
		t.Fatal("the dispatcher returned success for a failed rollback")
	}
	if resp.Error.OkToRetry || strings.Contains(resp.Error.Message, "exceeded its") || !strings.Contains(resp.Error.Message, "requires reconciliation") {
		t.Fatalf("the dispatcher did not hand back the reconciliation refusal: ok_to_retry=%t message=%q", resp.Error.OkToRetry, resp.Error.Message)
	}
}

// TestDispatcherKeepsGuardReconciliationAfterTheDeadline runs a create_vm
// whose guarded VM create was cut off by the per-method deadline after it was
// sent, so its outcome is unknown and the allocation requires reconciliation.
// The guard's poison carries that answer, and the dispatcher must hand it to
// the Director as it is, not as the retriable timeout.
func TestDispatcherKeepsGuardReconciliationAfterTheDeadline(t *testing.T) {
	t.Parallel()
	q := &guardTestQEMU{createFn: func(ctx context.Context, _ string, _ map[string]any) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}}
	guard, err := NewManagedAllocationGuard(guardTestClient{q: q}, ManagedAllocationHooks{
		Before: func(context.Context, ManagedAllocationMutation) (string, error) { return "step-1", nil },
		After:  func(context.Context, ManagedAllocationMutation, string, any) error { return nil },
		Failed: func(_ context.Context, call ManagedAllocationMutation, _ string, _ error) error {
			return storageAllocationUncertain(nil, "VM "+call.Service+"."+call.Method)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	d := cpi.NewDispatcherWithOptions(log.NewNopLogger(), cpi.WithMethodTimeouts(func(string) time.Duration { return 20 * time.Millisecond }))
	handler := cpi.HandlerFunc(func(ctx context.Context, _ []json.RawMessage, _ jsonrpc.Context) (any, error) {
		_, err := guard.Client().QEMU().Create(ctx, "n1", map[string]any{"vmid": 101})
		return nil, err
	})
	if err := d.Register("create_vm", handler); err != nil {
		t.Fatal(err)
	}
	resp := d.Handle(t.Context(), &jsonrpc.Request{Method: "create_vm", Context: jsonrpc.Context{RequestID: "guard-after-deadline"}})
	if resp.Error == nil {
		t.Fatal("the dispatcher returned success for an unknown create")
	}
	if resp.Error.OkToRetry || !strings.Contains(resp.Error.Message, "requires reconciliation") {
		t.Fatalf("the dispatcher rewrote the guard's reconciliation refusal: ok_to_retry=%t message=%q", resp.Error.OkToRetry, resp.Error.Message)
	}
}
