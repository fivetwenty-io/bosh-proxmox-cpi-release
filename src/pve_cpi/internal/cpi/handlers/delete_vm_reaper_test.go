package handlers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/tasks"
)

// Tests for delete_vm's opt-in empty-pool reaper (pve.pool_reap_empty). The
// reaper deletes a CPI-managed pool that becomes empty after this VM's
// destroy; it never harms an operator's own pool, and every failure mode is
// non-fatal to delete_vm.

// reaperPoolService is a PoolService fake recording GetPoolComment/DeletePool
// calls so tests can assert which branch of reapEmptyPoolIfManaged ran.
type reaperPoolService struct {
	getCommentFn func(ctx context.Context, poolID string) (string, bool, error)
	deletePoolFn func(ctx context.Context, poolID string) error

	getCommentCalls []string
	deletePoolCalls []string
}

func (p *reaperPoolService) AddVM(_ context.Context, _ string, _ int64) error        { return nil }
func (p *reaperPoolService) MoveVMToPool(_ context.Context, _ string, _ int64) error { return nil }
func (p *reaperPoolService) CreatePool(_ context.Context, _, _ string) error         { return nil }

func (p *reaperPoolService) DeletePool(ctx context.Context, poolID string) error {
	p.deletePoolCalls = append(p.deletePoolCalls, poolID)
	if p.deletePoolFn != nil {
		return p.deletePoolFn(ctx, poolID)
	}
	return nil
}

func (p *reaperPoolService) GetPoolComment(ctx context.Context, poolID string) (string, bool, error) {
	p.getCommentCalls = append(p.getCommentCalls, poolID)
	if p.getCommentFn != nil {
		return p.getCommentFn(ctx, poolID)
	}
	return "", false, nil
}

// clusterVMOnNodeWithPool builds a ListResourcesResponse placing vmid on node
// with the given resource-pool membership (empty pool omits the field
// entirely, matching PVE's live behaviour for a non-member VM).
func clusterVMOnNodeWithPool(vmid int, node, pool string) *cluster.ListResourcesResponse {
	row := map[string]any{"vmid": vmid, "node": node, "type": "qemu"}
	if pool != "" {
		row["pool"] = pool
	}
	raw, _ := json.Marshal(row)
	resp := cluster.ListResourcesResponse{raw}
	return &resp
}

// reaperTestFixture bundles the mocks needed for a delete_vm reaper test: a
// clean stop->destroy happy path (no unused disk entries, empty DeleteQemu
// response so no task-await is needed) plus the pool plumbing under test.
type reaperTestFixture struct {
	deps     handlers.Deps
	pools    *reaperPoolService
	logBuf   *bytes.Buffer
	deleteQC int // DeleteQemu call count
}

func newReaperTestFixture(t *testing.T, vmid int, poolReapEmpty bool, clusterPool string) *reaperTestFixture {
	t.Helper()

	fx := &reaperTestFixture{
		pools:  &reaperPoolService{},
		logBuf: &bytes.Buffer{},
	}

	qemuSvc := &mockQEMUService{
		stopFn: func(_ context.Context, _ string, _ int) (string, error) {
			return "UPID:node:stop-task", nil
		},
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{}, nil // no unused slots -- guard falls through
		},
	}
	nodesSvc := &mockNodesService{
		deleteQemuFn: func(_ context.Context, _ string, _ string, _ *nodes.DeleteQemuParams) (*nodes.DeleteQemuResponse, error) {
			fx.deleteQC++
			raw := nodes.DeleteQemuResponse{}
			return &raw, nil
		},
	}
	tasksSvc := &mockTasksService{
		waitFn: func(_ context.Context, _, _ string, _ *tasks.WaitOptions) (*tasks.Status, error) {
			return &tasks.Status{ExitStatus: "OK"}, nil
		},
	}
	agentSvc := &mockAgentService{
		removeFn: func(_ context.Context, _ string, _ int) error { return nil },
	}

	logger, lerr := log.NewLogger("debug", fx.logBuf)
	if lerr != nil {
		t.Fatalf("log.NewLogger: %v", lerr)
	}

	deps := testDepsFoundVM(vmid, qemuSvc, nodesSvc, tasksSvc, agentSvc)
	deps.Config.PoolReapEmpty = poolReapEmpty
	deps.Logger = logger
	deps.PVE = &mockPVEClient{
		qemuSvc:  qemuSvc,
		nodesSvc: nodesSvc,
		tasksSvc: tasksSvc,
		clusterSvc: &mockClusterSvc{
			listResourcesFn: func(_ context.Context, _ *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
				return clusterVMOnNodeWithPool(vmid, vmNode, clusterPool), nil
			},
		},
		storageSvc: &mockStorageService{},
		poolsSvc:   fx.pools,
	}
	fx.deps = deps
	return fx
}

func (fx *reaperTestFixture) run(t *testing.T, vmid int) error {
	t.Helper()
	h := handlers.HandleDeleteVM(fx.deps)
	_, err := h.Handle(context.Background(), marshalArgs(strconv.Itoa(vmid)), jsonrpc.Context{})
	return err
}

func TestDeleteVM_ReaperDisabled_NoPoolCalls(t *testing.T) {
	t.Parallel()

	const vmid = 9001
	fx := newReaperTestFixture(t, vmid, false, "bosh")

	if err := fx.run(t, vmid); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fx.deleteQC != 1 {
		t.Fatalf("DeleteQemu: want 1 call, got %d", fx.deleteQC)
	}
	if len(fx.pools.getCommentCalls) != 0 {
		t.Errorf("GetPoolComment: want 0 calls with reaper disabled, got %v", fx.pools.getCommentCalls)
	}
	if len(fx.pools.deletePoolCalls) != 0 {
		t.Errorf("DeletePool: want 0 calls with reaper disabled, got %v", fx.pools.deletePoolCalls)
	}
}

func TestDeleteVM_ReaperDeletesEmptyManagedPool(t *testing.T) {
	t.Parallel()

	const vmid = 9002
	fx := newReaperTestFixture(t, vmid, true, "bosh")
	fx.pools.getCommentFn = func(_ context.Context, _ string) (string, bool, error) {
		return "managed by bosh-pve-cpi (director d)", true, nil
	}

	if err := fx.run(t, vmid); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(fx.pools.getCommentCalls) != 1 || fx.pools.getCommentCalls[0] != "bosh" {
		t.Fatalf("GetPoolComment: want 1 call for pool %q, got %v", "bosh", fx.pools.getCommentCalls)
	}
	if len(fx.pools.deletePoolCalls) != 1 || fx.pools.deletePoolCalls[0] != "bosh" {
		t.Fatalf("DeletePool: want 1 call for pool %q, got %v", "bosh", fx.pools.deletePoolCalls)
	}
	if !strings.Contains(fx.logBuf.String(), "reaped empty pool") {
		t.Errorf("expected an Info log confirming the reap; log=%s", fx.logBuf.String())
	}
}

func TestDeleteVM_ReaperSkipsOperatorPool(t *testing.T) {
	t.Parallel()

	const vmid = 9003
	fx := newReaperTestFixture(t, vmid, true, "ops-pool")
	fx.pools.getCommentFn = func(_ context.Context, _ string) (string, bool, error) {
		return "my ops pool", true, nil
	}

	if err := fx.run(t, vmid); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(fx.pools.getCommentCalls) != 1 {
		t.Fatalf("GetPoolComment: want 1 call, got %v", fx.pools.getCommentCalls)
	}
	if len(fx.pools.deletePoolCalls) != 0 {
		t.Errorf("DeletePool: want 0 calls for a non-CPI-managed pool, got %v", fx.pools.deletePoolCalls)
	}
}

func TestDeleteVM_ReaperToleratesNotEmptyRace(t *testing.T) {
	t.Parallel()

	const vmid = 9004
	fx := newReaperTestFixture(t, vmid, true, "bosh")
	fx.pools.getCommentFn = func(_ context.Context, _ string) (string, bool, error) {
		return "managed by bosh-pve-cpi", true, nil
	}
	fx.pools.deletePoolFn = func(_ context.Context, _ string) error {
		return errors.New("pool 'x' is not empty (contains VM 99098)")
	}

	if err := fx.run(t, vmid); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(fx.pools.deletePoolCalls) != 1 {
		t.Fatalf("DeletePool: want 1 call, got %v", fx.pools.deletePoolCalls)
	}
	logs := fx.logBuf.String()
	if strings.Contains(logs, "empty-pool reap failed") {
		t.Errorf("Warn path must NOT be taken for the not-empty race; log=%s", logs)
	}
	if !strings.Contains(logs, "still has members or already gone") {
		t.Errorf("expected the tolerate-race debug log; log=%s", logs)
	}
}

func TestDeleteVM_ReaperToleratesPoolGone(t *testing.T) {
	t.Parallel()

	const vmid = 9005
	fx := newReaperTestFixture(t, vmid, true, "bosh")
	fx.pools.getCommentFn = func(_ context.Context, _ string) (string, bool, error) {
		return "managed by bosh-pve-cpi", true, nil
	}
	fx.pools.deletePoolFn = func(_ context.Context, _ string) error {
		return errors.New("delete pool failed: pool 'x' does not exist")
	}

	if err := fx.run(t, vmid); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	logs := fx.logBuf.String()
	if strings.Contains(logs, "empty-pool reap failed") {
		t.Errorf("Warn path must NOT be taken for the already-gone race; log=%s", logs)
	}
	if !strings.Contains(logs, "still has members or already gone") {
		t.Errorf("expected the tolerate-race debug log; log=%s", logs)
	}
}

func TestDeleteVM_ReaperGetPoolCommentErrors_DeleteNeverCalled(t *testing.T) {
	t.Parallel()

	const vmid = 9007
	fx := newReaperTestFixture(t, vmid, true, "bosh")
	fx.pools.getCommentFn = func(_ context.Context, _ string) (string, bool, error) {
		return "", false, errors.New("simulated: GetPoolComment transport failure")
	}

	if err := fx.run(t, vmid); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(fx.pools.getCommentCalls) != 1 || fx.pools.getCommentCalls[0] != "bosh" {
		t.Fatalf("GetPoolComment: want 1 call for pool %q, got %v", "bosh", fx.pools.getCommentCalls)
	}
	if len(fx.pools.deletePoolCalls) != 0 {
		t.Errorf("DeletePool: want 0 calls when GetPoolComment errors, got %v", fx.pools.deletePoolCalls)
	}
	logs := fx.logBuf.String()
	if !strings.Contains(logs, "GetPoolComment failed") {
		t.Errorf("expected the GetPoolComment-failed debug log; log=%s", logs)
	}
}

func TestDeleteVM_ReaperPoolNotFound_DeleteNeverCalled(t *testing.T) {
	t.Parallel()

	const vmid = 9008
	fx := newReaperTestFixture(t, vmid, true, "bosh")
	fx.pools.getCommentFn = func(_ context.Context, _ string) (string, bool, error) {
		return "", false, nil
	}

	if err := fx.run(t, vmid); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(fx.pools.getCommentCalls) != 1 || fx.pools.getCommentCalls[0] != "bosh" {
		t.Fatalf("GetPoolComment: want 1 call for pool %q, got %v", "bosh", fx.pools.getCommentCalls)
	}
	if len(fx.pools.deletePoolCalls) != 0 {
		t.Errorf("DeletePool: want 0 calls when the pool is already gone, got %v", fx.pools.deletePoolCalls)
	}
	logs := fx.logBuf.String()
	if !strings.Contains(logs, "pool already gone before reap attempt") {
		t.Errorf("expected the pool-already-gone debug log; log=%s", logs)
	}
}

func TestDeleteVM_ReaperPoolLookupFails_NoOp(t *testing.T) {
	t.Parallel()

	const vmid = 9006
	fx := newReaperTestFixture(t, vmid, true, "bosh")

	// Override the cluster service: the first ListResources call (locate VM
	// via FindVMViaCluster) succeeds; the second (the reaper's pre-destroy
	// FindVMPoolViaCluster lookup) fails.
	var calls int
	fx.deps.PVE = &mockPVEClient{
		qemuSvc:  fx.deps.PVE.QEMU(),
		nodesSvc: fx.deps.PVE.Nodes(),
		tasksSvc: fx.deps.PVE.Tasks(),
		clusterSvc: &mockClusterSvc{
			listResourcesFn: func(_ context.Context, _ *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
				calls++
				if calls == 1 {
					return clusterVMOnNodeWithPool(vmid, vmNode, "bosh"), nil
				}
				return nil, errors.New("simulated: pool lookup transport failure")
			},
		},
		storageSvc: &mockStorageService{},
		poolsSvc:   fx.pools,
	}

	if err := fx.run(t, vmid); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fx.deleteQC != 1 {
		t.Fatalf("DeleteQemu: want 1 call (destroy still proceeds), got %d", fx.deleteQC)
	}
	if len(fx.pools.getCommentCalls) != 0 {
		t.Errorf("GetPoolComment: want 0 calls when the pre-destroy pool lookup failed, got %v", fx.pools.getCommentCalls)
	}
	if len(fx.pools.deletePoolCalls) != 0 {
		t.Errorf("DeletePool: want 0 calls when the pre-destroy pool lookup failed, got %v", fx.pools.deletePoolCalls)
	}
}

func TestDeleteVM_ReaperRefusesStaticVMPool(t *testing.T) {
	t.Parallel()

	// The static vm_pool is a long-lived shared pool: even CPI-managed and
	// empty, the reaper must refuse it by name before any pool API call.
	const vmid = 9010
	fx := newReaperTestFixture(t, vmid, true, "bosh")
	fx.deps.Config.VMPool = "bosh"

	if err := fx.run(t, vmid); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(fx.pools.getCommentCalls) != 0 {
		t.Errorf("GetPoolComment: want 0 calls for the static vm_pool, got %v", fx.pools.getCommentCalls)
	}
	if len(fx.pools.deletePoolCalls) != 0 {
		t.Errorf("DeletePool: want 0 calls for the static vm_pool, got %v", fx.pools.deletePoolCalls)
	}
	if !strings.Contains(fx.logBuf.String(), "never reaped") {
		t.Errorf("expected the by-name refusal debug log; log=%s", fx.logBuf.String())
	}
}

func TestDeleteVM_ReaperRefusesStemcellTemplatePool(t *testing.T) {
	t.Parallel()

	const vmid = 9011
	fx := newReaperTestFixture(t, vmid, true, "bosh-templates")
	fx.deps.Config.StemcellTemplatePool = "bosh-templates"

	if err := fx.run(t, vmid); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(fx.pools.getCommentCalls) != 0 {
		t.Errorf("GetPoolComment: want 0 calls for the stemcell template pool, got %v", fx.pools.getCommentCalls)
	}
	if len(fx.pools.deletePoolCalls) != 0 {
		t.Errorf("DeletePool: want 0 calls for the stemcell template pool, got %v", fx.pools.deletePoolCalls)
	}
}
