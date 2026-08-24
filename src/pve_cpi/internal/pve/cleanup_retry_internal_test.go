// cleanup_retry_internal_test.go: white-box tests for the cleanup-under-
// contention retry wrappers. Each test injects one cfs-lock timeout followed
// by success and asserts the operation completes with exactly two calls;
// unfixed single-shot code gives up after one call and surfaces the lock
// error instead.
package pve

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	sdktasks "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/tasks"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// crLockErr models PVE's cluster-wide user_cfg lock contention verdict.
func crLockErr() error {
	return errors.New("cfs-lock 'user_cfg' error: got lock request timeout")
}

func crCtx() context.Context {
	return WithTestBackoff(context.Background(), func(int) time.Duration { return 0 })
}

// crPoolSvc is a PoolService fake with per-method call scripting.
type crPoolSvc struct {
	createFn       func(call int) error
	addVMFn        func(call int) error
	poolHasVMFn    func(poolID string, vmid int64) (bool, error)
	createCalls    int
	addVMCalls     int
	poolHasVMCalls int
}

func (s *crPoolSvc) CreatePool(_ context.Context, _, _ string) error {
	s.createCalls++
	if s.createFn != nil {
		return s.createFn(s.createCalls)
	}
	return nil
}

func (s *crPoolSvc) AddVM(_ context.Context, _ string, _ int64) error {
	s.addVMCalls++
	if s.addVMFn != nil {
		return s.addVMFn(s.addVMCalls)
	}
	return nil
}

func (s *crPoolSvc) MoveVMToPool(_ context.Context, _ string, _ int64) error { return nil }
func (s *crPoolSvc) DeletePool(_ context.Context, _ string) error            { return nil }
func (s *crPoolSvc) GetPoolComment(_ context.Context, _ string) (string, bool, error) {
	return "", false, nil
}

// crPoolClient exposes the pool service and an optional cluster service (nil
// by default, which FindVMPoolViaCluster tolerates as not-found); every other
// method panics via the embedded nil interface, so unexpected calls fail the
// test loudly.
type crPoolClient struct {
	Client
	pools   *crPoolSvc
	cluster sdkcluster.Service
}

func (c *crPoolClient) Pools() PoolService          { return c.pools }
func (c *crPoolClient) Cluster() sdkcluster.Service { return c.cluster }

// crCluster serves a canned ListResources response so membership
// verification after an already-a-pool-member verdict can be scripted.
type crCluster struct {
	sdkcluster.Service
	resp sdkcluster.ListResourcesResponse
}

func (s *crCluster) ListResources(
	_ context.Context, _ *sdkcluster.ListResourcesParams,
) (*sdkcluster.ListResourcesResponse, error) {
	return &s.resp, nil
}

func TestEnsurePoolExists_LockTimeout_RetriesAndSucceeds(t *testing.T) {
	t.Parallel()
	svc := &crPoolSvc{createFn: func(call int) error {
		if call == 1 {
			return crLockErr()
		}
		return nil
	}}
	err := EnsurePoolExists(crCtx(), &crPoolClient{pools: svc}, "dep-pool", "managed", log.NewNopLogger())
	if err != nil {
		t.Fatalf("expected success after lock-timeout retry, got: %v", err)
	}
	if svc.createCalls != 2 {
		t.Fatalf("expected 2 CreatePool calls (lock timeout + retry), got %d", svc.createCalls)
	}
}

func TestEnsurePoolExists_ConcurrentWinnerAfterLockTimeout_StopsRetrying(t *testing.T) {
	t.Parallel()
	// Attempt 1 loses the user_cfg lock; attempt 2 finds a concurrent create
	// won the race. "Already exists" is the goal state and must terminate the
	// retry loop as success, not burn the remaining budget.
	svc := &crPoolSvc{createFn: func(call int) error {
		if call == 1 {
			return crLockErr()
		}
		return errors.New("500 create pool failed: pool 'dep-pool' already exists")
	}}
	err := EnsurePoolExists(crCtx(), &crPoolClient{pools: svc}, "dep-pool", "managed", log.NewNopLogger())
	if err != nil {
		t.Fatalf("expected already-exists to resolve as success, got: %v", err)
	}
	if svc.createCalls != 2 {
		t.Fatalf("expected exactly 2 CreatePool calls, got %d", svc.createCalls)
	}
}

func TestAssignVMToPool_LockTimeout_RetriesAndSucceeds(t *testing.T) {
	t.Parallel()
	svc := &crPoolSvc{addVMFn: func(call int) error {
		if call == 1 {
			return crLockErr()
		}
		return nil
	}}
	err := AssignVMToPool(crCtx(), &crPoolClient{pools: svc}, "stem-pool", 30500, log.NewNopLogger())
	if err != nil {
		t.Fatalf("expected success after lock-timeout retry, got: %v", err)
	}
	if svc.addVMCalls != 2 {
		t.Fatalf("expected 2 AddVM calls (lock timeout + retry), got %d", svc.addVMCalls)
	}
}

// TestMigrateMoverToNode_LockTimeoutOnMigrate_Retries proves the migrate POST
// rides the lock curve: with Targetstorage the migration allocates the
// destination volume under the storage lock, so a cfs-lock timeout is the
// same contention every other storage mutation retries through. The scripted
// task-body failure after the successful resubmit ends the flow
// deterministically; the assertion is the call count, which stays 1 on
// unfixed transient-only code.
func TestMigrateMoverToNode_LockTimeoutOnMigrate_Retries(t *testing.T) {
	t.Parallel()
	const moverVMID = 90000
	volid := "local:vm-90000-disk-0"

	c := newRPClient()
	c.configs[moverVMID] = map[string]any{
		"name":  parkerVMName(moverVMID),
		"scsi1": volid + ",size=1G",
	}
	c.migrateFn = func(call int) (*sdknodes.CreateQemuMigrateResponse, error) {
		if call == 1 {
			return nil, errors.New("cfs-lock 'storage-local' error: got lock request timeout")
		}
		resp := sdknodes.CreateQemuMigrateResponse(`"UPID:pve1:0000AB12:00512345:65D0AA11:qmigrate:90000:root@pam:"`)
		return &resp, nil
	}
	c.waitFn = func(_ int, _ string) (*sdktasks.Status, error) {
		return nil, errors.New("task failed: migration aborted (node maintenance)")
	}

	mover := DiskHolder{Found: true, VMID: moverVMID, Node: "pve1", IsParker: true, Slot: "scsi1"}
	spec := DiskMigrationSpec{
		Holder:      mover,
		TargetNode:  "pve2",
		Volid:       volid,
		StableID:    "bpd-feedbeef00000001",
		AwaitBudget: time.Second,
	}

	_, _, err := migrateMoverToNode(crCtx(), c, log.NewNopLogger(), mover, volid, spec, dmBand(), ParkContext{})
	if err == nil {
		t.Fatal("expected the scripted task failure to surface")
	}
	if !strings.Contains(err.Error(), "migrate task for mover") {
		t.Fatalf("expected the post-retry task failure, got the submit error: %v", err)
	}
	if c.migrateCalls != 2 {
		t.Fatalf("expected 2 CreateQemuMigrate calls (lock timeout + retry), got %d", c.migrateCalls)
	}
}

// cr500 fabricates the typed HTTP-500 shape PVE uses for pool verdicts, so
// these tests exercise the same blanket-5xx transient classification the live
// error carries; a bare errors.New would never enter the retry union and the
// tests would pass vacuously on unfixed code.
func cr500(body string) error {
	return sdkerrors.ParseAPIError(500, []byte(body))
}

func TestAssignVMToPool_AlreadyMemberVerdict_SingleCallSuccess(t *testing.T) {
	t.Parallel()
	verdict := cr500("update pools failed: VM 30500 is already a pool member")
	if !IsVMAlreadyPoolMember(verdict) {
		t.Fatalf("fixture error must match IsVMAlreadyPoolMember: %v", verdict)
	}
	if !IsTransientTransport(verdict) {
		t.Fatalf("fixture error must look transient to the blanket 5xx rule: %v", verdict)
	}
	svc := &crPoolSvc{addVMFn: func(int) error { return verdict }}
	err := AssignVMToPool(crCtx(), &crPoolClient{pools: svc}, "stem-pool", 30500, log.NewNopLogger())
	if err != nil {
		t.Fatalf("already-a-pool-member must resolve as success (replayed committed AddVM), got: %v", err)
	}
	if svc.addVMCalls != 1 {
		t.Fatalf("resolved verdict must not spend the retry budget: expected 1 AddVM call, got %d", svc.addVMCalls)
	}
}

func TestAssignVMToPool_AlreadyMemberAfterLockTimeout_Success(t *testing.T) {
	t.Parallel()
	svc := &crPoolSvc{addVMFn: func(call int) error {
		if call == 1 {
			return crLockErr()
		}
		return cr500("update pools failed: VM 30500 is already a pool member")
	}}
	err := AssignVMToPool(crCtx(), &crPoolClient{pools: svc}, "stem-pool", 30500, log.NewNopLogger())
	if err != nil {
		t.Fatalf("lock timeout then already-member must resolve as success, got: %v", err)
	}
	if svc.addVMCalls != 2 {
		t.Fatalf("expected exactly 2 AddVM calls, got %d", svc.addVMCalls)
	}
}

func TestAssignVMToPool_PoolNotFoundVerdict_SingleCallError(t *testing.T) {
	t.Parallel()
	svc := &crPoolSvc{addVMFn: func(int) error {
		return cr500("update pools failed: pool 'stem-pool' does not exist")
	}}
	err := AssignVMToPool(crCtx(), &crPoolClient{pools: svc}, "stem-pool", 30500, log.NewNopLogger())
	if err == nil {
		t.Fatal("a missing pool is a real failure and must surface")
	}
	if svc.addVMCalls != 1 {
		t.Fatalf("resolved verdict must not spend the retry budget: expected 1 AddVM call, got %d", svc.addVMCalls)
	}
}

func TestAssignVMToPool_AlreadyMemberOfRequestedPool_VerifiedSuccess(t *testing.T) {
	t.Parallel()
	svc := &crPoolSvc{addVMFn: func(int) error {
		return cr500("update pools failed: VM 30500 is already a pool member")
	}}
	cl := &crPoolClient{pools: svc, cluster: &crCluster{resp: sdkcluster.ListResourcesResponse{
		json.RawMessage(`{"vmid": 30500, "pool": "stem-pool"}`),
	}}}
	err := AssignVMToPool(crCtx(), cl, "stem-pool", 30500, log.NewNopLogger())
	if err != nil {
		t.Fatalf("membership in the requested pool must resolve as success, got: %v", err)
	}
	if svc.addVMCalls != 1 {
		t.Fatalf("resolved verdict must not spend the retry budget: expected 1 AddVM call, got %d", svc.addVMCalls)
	}
}

func TestAssignVMToPool_AlreadyMemberOfOtherPool_FailsLoudly(t *testing.T) {
	t.Parallel()
	svc := &crPoolSvc{addVMFn: func(int) error {
		return cr500("update pools failed: VM 30500 is already a pool member")
	}}
	cl := &crPoolClient{pools: svc, cluster: &crCluster{resp: sdkcluster.ListResourcesResponse{
		json.RawMessage(`{"vmid": 30500, "pool": "other-pool"}`),
	}}}
	err := AssignVMToPool(crCtx(), cl, "stem-pool", 30500, log.NewNopLogger())
	if err == nil {
		t.Fatal("membership in a different pool means this caller's pool preference was not applied; it must surface")
	}
	if !strings.Contains(err.Error(), "other-pool") || !strings.Contains(err.Error(), "stem-pool") {
		t.Fatalf("the error must name both pools so the operator can reconcile, got: %v", err)
	}
	if svc.addVMCalls != 1 {
		t.Fatalf("resolved verdict must not spend the retry budget: expected 1 AddVM call, got %d", svc.addVMCalls)
	}
}

// PoolHasVM reports no membership unless poolHasVMFn scripts otherwise.
func (s *crPoolSvc) PoolHasVM(_ context.Context, poolID string, vmid int64) (bool, error) {
	s.poolHasVMCalls++
	if s.poolHasVMFn != nil {
		return s.poolHasVMFn(poolID, vmid)
	}
	return false, nil
}

// TestAssignVMToPool_AlreadyMemberOfRequestedPool_PoolProbeBeatsStaleIndex
// pins the disambiguation order: the target pool's own pmxcfs-backed
// membership (PoolHasVM) answers first, so a VM another path just moved into
// the requested pool resolves as success even while the lagging
// /cluster/resources index still names the pool it came FROM. Before the
// probe, the stale index row turned this replay into a false permanent error.
func TestAssignVMToPool_AlreadyMemberOfRequestedPool_PoolProbeBeatsStaleIndex(t *testing.T) {
	t.Parallel()
	svc := &crPoolSvc{
		addVMFn: func(int) error {
			return cr500("update pools failed: VM 30500 is already a pool member")
		},
		poolHasVMFn: func(poolID string, vmid int64) (bool, error) {
			if poolID != "stem-pool" || vmid != 30500 {
				t.Errorf("PoolHasVM(%q, %d); want the requested pool and VMID", poolID, vmid)
			}
			return true, nil
		},
	}
	// The stale index still shows the pool the VM was just moved out of.
	cl := &crPoolClient{pools: svc, cluster: &crCluster{resp: sdkcluster.ListResourcesResponse{
		json.RawMessage(`{"vmid": 30500, "pool": "other-pool"}`),
	}}}
	err := AssignVMToPool(crCtx(), cl, "stem-pool", 30500, log.NewNopLogger())
	if err != nil {
		t.Fatalf("membership confirmed by the pool object itself must resolve as success, got: %v", err)
	}
	if svc.poolHasVMCalls != 1 {
		t.Fatalf("expected exactly 1 PoolHasVM probe, got %d", svc.poolHasVMCalls)
	}
}

// TestAssignVMToPool_AlreadyMember_ProbeFailureSkipsStaleIndexVerdict pins
// the probe-failure posture: when PoolHasVM itself errors, the replay reading
// stands and the lagging index is never allowed to mint the fatal
// already-in-another-pool error on stale evidence alone.
func TestAssignVMToPool_AlreadyMember_ProbeFailureSkipsStaleIndexVerdict(t *testing.T) {
	t.Parallel()
	svc := &crPoolSvc{
		addVMFn: func(int) error {
			return cr500("update pools failed: VM 30500 is already a pool member")
		},
		poolHasVMFn: func(string, int64) (bool, error) {
			return false, cr500("pvedaemon worker cycling")
		},
	}
	// The stale index still shows the pool the VM was just moved out of; with
	// the probe answer missing, that row alone must not fail the call.
	cl := &crPoolClient{pools: svc, cluster: &crCluster{resp: sdkcluster.ListResourcesResponse{
		json.RawMessage(`{"vmid": 30500, "pool": "other-pool"}`),
	}}}
	err := AssignVMToPool(crCtx(), cl, "stem-pool", 30500, log.NewNopLogger())
	if err != nil {
		t.Fatalf("a failed membership probe must keep the replay reading, got: %v", err)
	}
	if svc.poolHasVMCalls != 1 {
		t.Fatalf("expected exactly 1 PoolHasVM probe, got %d", svc.poolHasVMCalls)
	}
}
