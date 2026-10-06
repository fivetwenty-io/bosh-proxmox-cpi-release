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

// TestCreateVMRollbackStopsWhenAnUnboundedRequestIsCancelled runs the
// lock-timeout rollback with operation_timeout off, so the request has no
// deadline. A stop signal cancels the request after the disk's lock wait gives
// up and before the rollback begins. Without a deadline the rollback runs on
// the request's own context, the way it always has, so the signal stops it
// instead of the rollback running on for a whole delete_sec after the process
// was told to stop.
func TestCreateVMRollbackStopsWhenAnUnboundedRequestIsCancelled(t *testing.T) {
	t.Parallel()
	flow := newRollbackFlow(t)
	req, stop := context.WithCancel(flow.ctx)
	defer stop()
	var once sync.Once
	deps := flow.parked.deps
	deps.PVE = stoppingReadClient{Client: deps.PVE, stop: func() {
		flow.locks.mu.Lock()
		waited := flow.locks.rejections > 0
		flow.locks.mu.Unlock()
		if waited {
			once.Do(stop)
		}
	}}
	flow.vms.onDisposal = flow.locks.reset

	if _, err := createVM(req, deps, flow.args); err == nil {
		t.Fatal("create_vm succeeded although its request was cancelled")
	}
	if req.Err() == nil {
		t.Fatal("the disk never read its VM back after the lock wait, so the request was never cancelled")
	}
	if len(flow.vms.destroyed) != 0 {
		t.Fatalf("the rollback ran on after the stop signal and destroyed %v", flow.vms.destroyed)
	}
}

// stoppingReadClient calls stop on every VM config read and then serves the
// read, so a test can send a stop signal at the moment a disk reads its VM
// back.
type stoppingReadClient struct {
	pve.Client
	stop func()
}

func (c stoppingReadClient) QEMU() qemu.Service {
	return stoppingReadQEMU{Service: c.Client.QEMU(), stop: c.stop}
}

// StorageAuditVisibility answers for the wrapped client, so the allocation
// audits a create_vm runs still see the cluster.
func (c stoppingReadClient) StorageAuditVisibility(ctx context.Context) error {
	reader, ok := c.Client.(pve.StorageAuditVisibilityReader)
	if !ok {
		return errors.New("the wrapped client cannot prove audit visibility")
	}
	return reader.StorageAuditVisibility(ctx)
}

type stoppingReadQEMU struct {
	qemu.Service
	stop func()
}

func (q stoppingReadQEMU) Config(ctx context.Context, node string, vmid int) (map[string]any, error) {
	q.stop()
	return q.Service.Config(ctx, node, vmid)
}

// failingAuditClient fails every VM config read once failing reports true,
// the way PVE does when it answers a read with an error.
type failingAuditClient struct {
	pve.Client
	failing func() bool
}

func (c failingAuditClient) QEMU() qemu.Service {
	return failingAuditQEMU{Service: c.Client.QEMU(), failing: c.failing}
}

// StorageAuditVisibility answers for the wrapped client, so the allocation
// audits a create_vm runs still see the cluster.
func (c failingAuditClient) StorageAuditVisibility(ctx context.Context) error {
	reader, ok := c.Client.(pve.StorageAuditVisibilityReader)
	if !ok {
		return errors.New("the wrapped client cannot prove audit visibility")
	}
	return reader.StorageAuditVisibility(ctx)
}

type failingAuditQEMU struct {
	qemu.Service
	failing func() bool
}

func (q failingAuditQEMU) Config(ctx context.Context, node string, vmid int) (map[string]any, error) {
	if q.failing() {
		return nil, errors.New("PVE answered 500 to the VM config read")
	}
	return q.Service.Config(ctx, node, vmid)
}

// TestCreateVMLastAttemptCompletionFailureIsNotRetriable runs create_vm
// through the dispatcher on its last attempt. A persistent disk waits out
// another request's parker lock, and the completion audit that follows the
// clean timeout fails, so the disk's allocation requires reconciliation. The
// Director must read that refusal, not the retriable lock timeout, because a
// retry would only meet the refusal.
func TestCreateVMLastAttemptCompletionFailureIsNotRetriable(t *testing.T) {
	t.Parallel()
	flow := newRollbackFlow(t)
	deps := flow.parked.deps
	deps.PVE = failingAuditClient{Client: deps.PVE, failing: func() bool {
		flow.locks.mu.Lock()
		defer flow.locks.mu.Unlock()
		return flow.locks.rejections > 0
	}}
	d := cpi.NewDispatcherWithOptions(log.NewNopLogger())
	handler := cpi.HandlerFunc(func(ctx context.Context, args []json.RawMessage, _ jsonrpc.Context) (any, error) {
		return createVM(shortenManagedLockWait(ctx, testManagedLockWait), deps, args)
	})
	if err := d.Register("create_vm", handler); err != nil {
		t.Fatal(err)
	}

	resp := d.Handle(t.Context(), &jsonrpc.Request{Method: "create_vm", Arguments: flow.args, Context: jsonrpc.Context{RequestID: "completion-audit-failed"}})
	if resp.Error == nil {
		t.Fatal("create_vm succeeded although the disk's completion audit failed")
	}
	record := flow.parked.record(t)
	if record.State != aj.ReconciliationRequired || !strings.Contains(record.Reason, "completion audit after a lock timeout failed") {
		t.Fatalf("the parked disk's allocation is %s (reason %q), want the failed completion audit; create_vm answered %q", record.State, record.Reason, resp.Error.Message)
	}
	if resp.Error.OkToRetry || !strings.Contains(resp.Error.Message, "requires reconciliation") {
		t.Fatalf("the Director would retry a disk that refuses the retry: ok_to_retry=%t message=%q", resp.Error.OkToRetry, resp.Error.Message)
	}
}

// TestDiskCompletionFailureAfterALockTimeoutIsNotRetriable runs a disk
// operation through the dispatcher whose parker lock wait gave up cleanly and
// whose completion audit then failed, so the disk's allocation requires
// reconciliation. The operation's own error is the retriable lock timeout, and
// the Director acts on the first typed error it reads. The failed completion
// must come first, or the Director would retry a disk that refuses every
// retry.
func TestDiskCompletionFailureAfterALockTimeoutIsNotRetriable(t *testing.T) {
	t.Parallel()
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
	lifecycle.deps.PVE = failingAuditClient{Client: lifecycle.deps.PVE, failing: func() bool { return true }}
	d := cpi.NewDispatcherWithOptions(log.NewNopLogger())
	handler := cpi.HandlerFunc(func(ctx context.Context, _ []json.RawMessage, _ jsonrpc.Context) (any, error) {
		return nil, lifecycle.finish(ctx, lockTimeoutError(), false)
	})
	if err := d.Register("attach_disk", handler); err != nil {
		t.Fatal(err)
	}

	resp := d.Handle(t.Context(), &jsonrpc.Request{Method: "attach_disk", Context: jsonrpc.Context{RequestID: "disk-completion-failed"}})
	if resp.Error == nil {
		t.Fatal("the operation succeeded although its completion audit failed")
	}
	record, err := journal.Inspect(id)
	if err != nil {
		t.Fatal(err)
	}
	if record.State != aj.ReconciliationRequired {
		t.Fatalf("the failed completion audit left the allocation %s", record.State)
	}
	if resp.Error.OkToRetry || strings.Contains(resp.Error.Message, "timed out") {
		t.Fatalf("the Director would retry a disk that refuses the retry: ok_to_retry=%t message=%q", resp.Error.OkToRetry, resp.Error.Message)
	}
}

// TestCreateVMFallbackRollbackAfterTheDeadlineLeavesARetry ends the request
// while create_vm rolls back an attempt that still has a fallback after it.
// The rollback finishes and closes the attempt, but nothing after it can run
// on the ended request. The Director must get a retriable error, and its
// retry must build a fresh VM in a new attempt.
func TestCreateVMFallbackRollbackAfterTheDeadlineLeavesARetry(t *testing.T) {
	t.Parallel()
	flow := newRollbackFlow(t)
	limit := 1
	flow.parked.deps.Config.Placement.FallbackMax = &limit
	req := newEndingRequest(flow.ctx)
	flow.vms.onDisposal = func() {
		flow.locks.reset()
		req.end()
	}

	_, err := createVM(req, flow.parked.deps, flow.args)
	if !req.hasEnded() {
		t.Fatal("the rollback never began a disposal, so the request never ended")
	}
	var typed *cpierrors.Error
	if !errors.As(err, &typed) || !typed.OkToRetry() {
		t.Fatalf("create_vm did not hand the Director a retriable error after the rollback: %v", err)
	}
	created := flow.created(t)
	if len(created) != 1 || len(flow.vms.destroyed) != 1 {
		t.Fatalf("the rolled-back attempt's VM was not destroyed: created=%v destroyed=%v", created, flow.vms.destroyed)
	}
	generation := flow.generation(t)
	if !managedVMAttemptClosed(generation) {
		t.Fatalf("the rollback left the attempt open: the generation is %s (reason %q)", generation.State, generation.Reason)
	}

	result, err := flow.createVM(t)
	if err != nil {
		t.Fatalf("the Director's retry was refused: %v", err)
	}
	values, ok := result.([]any)
	if !ok || len(values) == 0 {
		t.Fatalf("unexpected create_vm result %v", result)
	}
	if fresh := flow.created(t); len(fresh) != 2 {
		t.Fatalf("the retry did not build a fresh VM: created=%v", fresh)
	}
	if retried := flow.generation(t); retried.ID != generation.ID || retried.ActiveAttempt() <= generation.ActiveAttempt() {
		t.Fatalf("the retry did not resume the generation in a new attempt: before=%s/%d after=%s/%d", generation.ID, generation.ActiveAttempt(), retried.ID, retried.ActiveAttempt())
	}
}

// retainedVMHandle holds a VM generation that delete_vm retained, the state
// the next delete_vm resumes.
func retainedVMHandle(t *testing.T) *aj.Handle {
	t.Helper()
	_, _, h := retainedVMFixture(t)
	return h
}

// retainedVMFixture is retainedVMHandle with the deps and the journal that
// hold the generation, so a test can run the next delete_vm's disposal on it.
// Every step of the generation's active attempt is observed.
func retainedVMFixture(t *testing.T) (Deps, *aj.Journal, *aj.Handle) {
	t.Helper()
	deps, journal, _, record, _ := resumeVMFixture(t)
	h, err := journal.Acquire(t.Context(), record.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := h.Close(); err != nil {
			t.Error(err)
		}
	})
	target := record.Steps[1].Target
	target.IntendedVolume = record.Steps[1].VolIDs[0]
	proofID, payload, err := aj.VerificationEvidence(aj.VMRetentionEvidence{VMID: 123, RetainedArtifacts: []aj.Target{target}})
	if err != nil {
		t.Fatal(err)
	}
	r := h.Record()
	r.State = aj.VMDeletedRetained
	r.Verifications = append(r.Verifications, aj.Verification{EvidenceID: proofID, EvidenceJSON: payload, Complete: true, VMAbsenceVerified: true, ArtifactDispositionVerified: true})
	if err := h.Save(r); err != nil {
		t.Fatal(err)
	}
	return deps, journal, h
}

// TestRetainedVMUncertaintyStaysRetriable covers a failure while delete_vm
// works through a retained VM generation whose steps are all observed. The
// record stays retained, and the next delete_vm resumes it, so the error must
// not be held back from the deadline's retriable rewrite. That holds for the
// error itself and for a guard's poison built from it. A generation the
// failure does move to reconciliation keeps its definite answer.
func TestRetainedVMUncertaintyStaysRetriable(t *testing.T) {
	t.Parallel()
	retained := retainedVMHandle(t)
	if err := storageLifecycleSettled(retained.Record()); err != nil {
		t.Fatalf("the retained generation has an unsettled step: %v", err)
	}
	err := storageAllocationUncertain(retained, "retained VM cleanup")
	if err == nil || cpierrors.IsDefinite(err) {
		t.Fatalf("a retained generation's failure was held back from the retry: %v", err)
	}
	if state := retained.Record().State; state != aj.VMDeletedRetained {
		t.Fatalf("the failure moved the retained generation to %s", state)
	}

	q := &guardTestQEMU{createFn: func(context.Context, string, map[string]any) (string, error) {
		return "", errors.New("PVE answered 500 to the create")
	}}
	guard, err := NewManagedAllocationGuard(guardTestClient{q: q}, ManagedAllocationHooks{
		Before: func(context.Context, ManagedAllocationMutation) (string, error) { return "step-1", nil },
		After:  func(context.Context, ManagedAllocationMutation, string, any) error { return nil },
		Failed: func(context.Context, ManagedAllocationMutation, string, error) error {
			return storageAllocationUncertain(retained, "retained ephemeral cleanup")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := guard.Client().QEMU().Create(t.Context(), "n1", map[string]any{"vmid": 101}); err == nil || cpierrors.IsDefinite(err) || cpierrors.IsDefinite(guard.Err()) {
		t.Fatalf("the guard held a retained generation's failure back from the retry: %v", err)
	}

	d := cpi.NewDispatcherWithOptions(log.NewNopLogger(), cpi.WithMethodTimeouts(func(string) time.Duration { return 20 * time.Millisecond }))
	handler := cpi.HandlerFunc(func(ctx context.Context, _ []json.RawMessage, _ jsonrpc.Context) (any, error) {
		<-ctx.Done()
		return nil, storageAllocationUncertain(retained, "retained VM cleanup")
	})
	if err := d.Register("delete_vm", handler); err != nil {
		t.Fatal(err)
	}
	resp := d.Handle(t.Context(), &jsonrpc.Request{Method: "delete_vm", Context: jsonrpc.Context{RequestID: "retained-after-deadline"}})
	if resp.Error == nil || !resp.Error.OkToRetry || !strings.Contains(resp.Error.Message, "exceeded its") {
		t.Fatalf("the dispatcher did not hand back the retriable timeout for a retained generation: %+v", resp.Error)
	}

	m := createdManagedVM(t)
	if err := storageAllocationUncertain(m.handle, "VM cleanup"); !cpierrors.IsDefinite(err) || m.handle.Record().State != aj.ReconciliationRequired {
		t.Fatalf("a generation moved to reconciliation lost its definite answer: %v (%s)", err, m.handle.Record().State)
	}
}

// TestRetainedVMUncertaintyWithAPlannedLockStepStaysRetriable covers a failure
// that leaves only a planned lock step unsettled on a retained VM generation.
// The next delete_vm settles that step before its cleanup check and resumes
// the generation, so the error must stay retriable.
func TestRetainedVMUncertaintyWithAPlannedLockStepStaysRetriable(t *testing.T) {
	t.Parallel()
	retained := retainedVMHandle(t)
	base := retained.Record().Steps[1].Target
	target := aj.Target{Node: base.Node, VMID: base.VMID}
	step, err := storageMutationIntent(retained, "vm.Pool.CreatePool", target, nil)
	if err != nil {
		t.Fatal(err)
	}
	var planned aj.Step
	for _, s := range retained.Record().Steps {
		if s.ID == step {
			planned = s
		}
	}
	if planned.State != aj.Planned || !isLockStep(planned) {
		t.Fatalf("the fixture's step is not a planned lock step: %+v", planned)
	}
	if storageLifecycleSettled(retained.Record()) == nil {
		t.Fatal("the planned lock step did not make the record unsettled")
	}

	err = storageAllocationUncertain(retained, "retained VM cleanup")
	if err == nil || cpierrors.IsDefinite(err) {
		t.Fatalf("a retained generation whose only unsettled step is a lock step was held back from the retry: %v", err)
	}
	if strings.Contains(err.Error(), step) {
		t.Fatalf("the error names a step the next delete_vm settles on its own: %v", err)
	}
	if state := retained.Record().State; state != aj.VMDeletedRetained {
		t.Fatalf("the failure moved the retained generation to %s", state)
	}
}

// TestRetainedVMUncertaintyWithAnUnsettledStepIsNotRetriable covers a failure
// that leaves a step of a retained VM generation's active attempt submitted,
// which is what a mutation with an unknown outcome leaves behind during the
// retained cleanup. The record stays retained, but the next delete_vm refuses
// at its cleanup settlement check instead of resuming it, so a retry cannot
// repair it. The error must be definite, survive the deadline's retriable
// rewrite, and name the step and what the operator does about it.
func TestRetainedVMUncertaintyWithAnUnsettledStepIsNotRetriable(t *testing.T) {
	t.Parallel()
	deps, journal, retained := retainedVMFixture(t)
	target := retained.Record().Steps[1].Target
	step, err := storageMutationIntent(retained, "lifecycle_delete_disk_Nodes_DeleteStorageContent", target, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := storageMutationSubmitted(retained, step, "UPID:pve1:retained-cleanup"); err != nil {
		t.Fatal(err)
	}

	err = storageAllocationUncertain(retained, "lifecycle delete_disk retained ephemeral cleanup incomplete")
	if !cpierrors.IsDefinite(err) {
		t.Fatalf("a retained generation that the next delete_vm refuses was offered a retry: %v", err)
	}
	if state := retained.Record().State; state != aj.VMDeletedRetained {
		t.Fatalf("the failure moved the retained generation to %s", state)
	}
	for _, want := range []string{"requires reconciliation", step, "is submitted", "the next delete_vm refuses", "storage-journal audit"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the error does not say %q: %v", want, err)
		}
	}

	d := cpi.NewDispatcherWithOptions(log.NewNopLogger(), cpi.WithMethodTimeouts(func(string) time.Duration { return 20 * time.Millisecond }))
	handler := cpi.HandlerFunc(func(ctx context.Context, _ []json.RawMessage, _ jsonrpc.Context) (any, error) {
		<-ctx.Done()
		return nil, storageAllocationUncertain(retained, "retained VM cleanup")
	})
	if err := d.Register("delete_vm", handler); err != nil {
		t.Fatal(err)
	}
	resp := d.Handle(t.Context(), &jsonrpc.Request{Method: "delete_vm", Context: jsonrpc.Context{RequestID: "retained-unsettled-after-deadline"}})
	if resp.Error == nil || resp.Error.OkToRetry || !strings.Contains(resp.Error.Message, "requires reconciliation") || !strings.Contains(resp.Error.Message, step) {
		t.Fatalf("the dispatcher did not hand back the definite answer for a retained generation with an unsettled step: %+v", resp.Error)
	}

	if _, err := disposeManagedRetainedVM(t.Context(), deps, journal, retained); err == nil || !strings.Contains(err.Error(), "cleanup has unresolved mutation evidence") {
		t.Fatalf("the next delete_vm did not refuse the unsettled step: %v", err)
	}
}
