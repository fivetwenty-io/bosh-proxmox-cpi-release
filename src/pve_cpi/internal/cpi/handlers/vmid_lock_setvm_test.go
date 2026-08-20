package handlers_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi/handlers"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

// --------------------------------------------------------------------------
// recordingPoolService records pool operations so ordering can be asserted.
// --------------------------------------------------------------------------

type recordingPoolService struct {
	events    *[]string
	pools     map[string]string
	createErr error
}

func newRecordingPoolService(events *[]string) *recordingPoolService {
	return &recordingPoolService{events: events, pools: map[string]string{}}
}

func (p *recordingPoolService) record(ev string) {
	if p.events != nil {
		*p.events = append(*p.events, ev)
	}
}

func (p *recordingPoolService) AddVM(_ context.Context, _ string, _ int64) error { return nil }

func (p *recordingPoolService) MoveVMToPool(_ context.Context, poolID string, vmid int64) error {
	p.record(fmt.Sprintf("move:%s:%d", poolID, vmid))
	return nil
}

func (p *recordingPoolService) CreatePool(_ context.Context, poolID, _ string) error {
	p.record("create:" + poolID)
	if p.createErr != nil {
		return p.createErr
	}
	if _, ok := p.pools[poolID]; ok {
		return fmt.Errorf("pool '%s' already exists", poolID)
	}
	p.pools[poolID] = "held"
	return nil
}

func (p *recordingPoolService) DeletePool(_ context.Context, poolID string) error {
	p.record("delete:" + poolID)
	if _, ok := p.pools[poolID]; !ok {
		return fmt.Errorf("pool '%s' does not exist", poolID)
	}
	delete(p.pools, poolID)
	return nil
}

func (p *recordingPoolService) GetPoolComment(_ context.Context, poolID string) (string, bool, error) {
	p.record("get:" + poolID)
	c, ok := p.pools[poolID]
	return c, ok, nil
}

var _ pve.PoolService = (*recordingPoolService)(nil)

// --------------------------------------------------------------------------
// testDepsFoundVMWithPools builds Deps with a recording pool service and
// uses mockPVEClient.poolsSvc (now exposed after adding the field).
// --------------------------------------------------------------------------

//nolint:unparam // vmid is genuinely variable; all callers in this file happen to use 101
func testDepsFoundVMWithPools(
	vmid int,
	qemuSvc *mockQEMUService,
	nodesSvc *mockNodesService,
	pools pve.PoolService,
) handlers.Deps {
	deps := testDepsFoundVM(vmid, qemuSvc, nodesSvc, nil, &mockAgentService{})
	// Patch the pool service into the client. testDepsFoundVM constructs a
	// *mockPVEClient which now carries a poolsSvc field.
	if mc, ok := deps.PVE.(*mockPVEClient); ok {
		mc.poolsSvc = pools
	}
	return deps
}

// --------------------------------------------------------------------------
// TestHandleSetVMMetadata_LockAcquiredBeforeRead — the per-VMID lock is
// acquired before the QEMU.Config read and released after UpdateQemuConfig.
// --------------------------------------------------------------------------

func TestHandleSetVMMetadata_LockAcquiredBeforeRead(t *testing.T) {
	t.Parallel()

	const vmid = 101
	events := []string{}
	pools := newRecordingPoolService(&events)

	var gotUpdateCount int
	nodesSvc := &mockNodesService{
		updateQemuConfigFn: func(_ context.Context, _ string, _ string, _ *sdknodes.UpdateQemuConfigParams) error {
			events = append(events, "update-config")
			gotUpdateCount++
			return nil
		},
	}
	qemuSvc := &mockQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			events = append(events, "qemu-config")
			return map[string]any{"tags": "existing-tag"}, nil
		},
	}

	deps := testDepsFoundVMWithPools(vmid, qemuSvc, nodesSvc, pools)
	metadata := map[string]any{
		"director": "d", "deployment": "dep", "job": "api", "index": "0",
	}
	h := handlers.HandleSetVMMetadata(deps)
	_, err := h.Handle(context.Background(), marshalArgs("101", metadata), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expectedPool := "bosh-lock-vm-101"

	acquireIdx, readIdx := -1, -1
	for i, ev := range events {
		if ev == "create:"+expectedPool && acquireIdx == -1 {
			acquireIdx = i
		}
		if ev == "qemu-config" && readIdx == -1 {
			readIdx = i
		}
	}
	if acquireIdx == -1 {
		t.Fatalf("lock was never acquired; events=%v", events)
	}
	if readIdx == -1 {
		t.Fatalf("qemu-config was never read; events=%v", events)
	}
	if acquireIdx >= readIdx {
		t.Errorf("lock acquire(%d) must precede qemu-config read(%d); events=%v", acquireIdx, readIdx, events)
	}

	// Lock must be released after UpdateQemuConfig.
	updateIdx, releaseIdx := -1, -1
	for i, ev := range events {
		if ev == "update-config" && updateIdx == -1 {
			updateIdx = i
		}
		if ev == "delete:"+expectedPool {
			releaseIdx = i
		}
	}
	if releaseIdx == -1 {
		t.Fatalf("lock was never released; events=%v", events)
	}
	if updateIdx == -1 {
		t.Fatalf("update-config was never called; events=%v", events)
	}
	if releaseIdx <= updateIdx {
		t.Errorf("lock release(%d) must come after update(%d); events=%v", releaseIdx, updateIdx, events)
	}

	if gotUpdateCount != 1 {
		t.Errorf("expected exactly 1 UpdateQemuConfig call; got %d", gotUpdateCount)
	}
}

// --------------------------------------------------------------------------
// TestHandleSetVMMetadata_LockAcquireFailureRetriable — pool service error →
// set_vm_metadata returns retriable; no config update.
// --------------------------------------------------------------------------

func TestHandleSetVMMetadata_LockAcquireFailureRetriable(t *testing.T) {
	t.Parallel()

	const vmid = 101
	pools := newRecordingPoolService(nil)
	pools.createErr = fmt.Errorf("pmxcfs unavailable")

	var updateCalled bool
	nodesSvc := &mockNodesService{
		updateQemuConfigFn: func(_ context.Context, _ string, _ string, _ *sdknodes.UpdateQemuConfigParams) error {
			updateCalled = true
			return nil
		},
	}

	deps := testDepsFoundVMWithPools(vmid, nil, nodesSvc, pools)
	metadata := map[string]any{"director": "d"}
	h := handlers.HandleSetVMMetadata(deps)
	_, err := h.Handle(context.Background(), marshalArgs("101", metadata), jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected retriable error when lock cannot be acquired")
	}
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Errorf("lock-acquire failure must be retriable; got %v", err)
	}
	if updateCalled {
		t.Error("UpdateQemuConfig must not be called when lock acquisition fails")
	}
}

// --------------------------------------------------------------------------
// TestHandleSetVMMetadata_NilPoolsRetriable — nil pool service → retriable.
// --------------------------------------------------------------------------

func TestHandleSetVMMetadata_NilPoolsRetriable(t *testing.T) {
	t.Parallel()

	// Explicitly set poolsSvc = nil to exercise the nil-guard in withVMIDLock.
	var updateCalled bool
	nodesSvc := &mockNodesService{
		updateQemuConfigFn: func(_ context.Context, _ string, _ string, _ *sdknodes.UpdateQemuConfigParams) error {
			updateCalled = true
			return nil
		},
	}

	deps := testDepsFoundVMWithPools(101, nil, nodesSvc, nil)
	metadata := map[string]any{"director": "d"}
	h := handlers.HandleSetVMMetadata(deps)
	_, err := h.Handle(context.Background(), marshalArgs("101", metadata), jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected retriable error when pools is nil")
	}
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Errorf("nil-pools error must be retriable; got %v", err)
	}
	if updateCalled {
		t.Error("UpdateQemuConfig must not be called when pool service is nil")
	}
}

// --------------------------------------------------------------------------
// TestHandleSetVMMetadata_ExistingTagsPreservedUnderLock — operator tags
// read inside the lock survive the merge.
// --------------------------------------------------------------------------

func TestHandleSetVMMetadata_ExistingTagsPreservedUnderLock(t *testing.T) {
	t.Parallel()

	const vmid = 101
	pools := newRecordingPoolService(nil)

	var gotTags string
	nodesSvc := &mockNodesService{
		updateQemuConfigFn: func(_ context.Context, _ string, _ string, params *sdknodes.UpdateQemuConfigParams) error {
			if params.Tags != nil {
				gotTags = *params.Tags
			}
			return nil
		},
	}
	qemuSvc := &mockQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{"tags": "env--prod;owner--cpi-test"}, nil
		},
	}

	deps := testDepsFoundVMWithPools(vmid, qemuSvc, nodesSvc, pools)
	metadata := map[string]any{
		"director": "d", "deployment": "cf", "job": "api", "index": "0",
	}
	h := handlers.HandleSetVMMetadata(deps)
	_, err := h.Handle(context.Background(), marshalArgs("101", metadata), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, want := range []string{"env--prod", "owner--cpi-test", "deployment--cf"} {
		if !strings.Contains(gotTags, want) {
			t.Errorf("tag %q missing from result; got tags=%q", want, gotTags)
		}
	}
}
