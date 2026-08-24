// cleanup_retry_internal_test.go: white-box tests for the cleanup-under-
// contention retry wrappers in the handler package. The causal loop under
// test: a cleanup triggered by a storage fault immediately issues another
// storage mutation into the same contended lock. Each test injects one
// cfs-lock timeout followed by success and asserts the cleanup completes
// with exactly two calls; unfixed single-shot code gives up after one call.
// The probe-visibility tests assert that a failed existence probe lands a
// Warn naming the volid instead of being indistinguishable from
// nothing-to-clean.
package handlers

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	sdkcloudinit "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cloudinit"
	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdkclusterstorage "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/clusterstorage"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	sdkqemu "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
	sdkstorage "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/storage"
	sdktasks "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/tasks"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

func crhLockErr() error {
	return errors.New("cfs-lock 'storage-local' error: got lock request timeout")
}

func crhCtx() context.Context {
	return pve.WithTestBackoff(context.Background(), func(int) time.Duration { return 0 })
}

// crhStorage is a storage.Service fake with per-call scripting for the three
// sweep entry points.
type crhStorage struct {
	sdkstorage.Service

	deleteAsyncFn    func(call int) (string, error)
	deleteIfExistsFn func(call int) (bool, error)
	existsFn         func() (bool, error)
	deleteAsyncCalls int
	deleteIfCalls    int
	existsCalls      int
}

func (s *crhStorage) DeleteVolumeAsync(_ context.Context, _, _, _ string) (string, error) {
	s.deleteAsyncCalls++
	if s.deleteAsyncFn != nil {
		return s.deleteAsyncFn(s.deleteAsyncCalls)
	}
	return "", nil
}

func (s *crhStorage) DeleteVolumeIfExists(_ context.Context, _, _, _ string) (bool, error) {
	s.deleteIfCalls++
	if s.deleteIfExistsFn != nil {
		return s.deleteIfExistsFn(s.deleteIfCalls)
	}
	return true, nil
}

func (s *crhStorage) Exists(_ context.Context, _, _, _ string) (bool, error) {
	s.existsCalls++
	if s.existsFn != nil {
		return s.existsFn()
	}
	return true, nil
}

// crhQEMU backs the unused-slot sweep: Config feeds the slot scan, DetachDisk
// is the contended mutation.
type crhQEMU struct {
	sdkqemu.Service

	cfg         map[string]any
	detachFn    func(call int) error
	detachCalls int
}

func (q *crhQEMU) Config(_ context.Context, _ string, _ int) (map[string]any, error) {
	return q.cfg, nil
}

func (q *crhQEMU) DetachDisk(_ context.Context, _ string, _ int, _ string) error {
	q.detachCalls++
	if q.detachFn != nil {
		return q.detachFn(q.detachCalls)
	}
	return nil
}

// crhPools is a pve.PoolService fake with per-call scripting for the pool
// pipeline sites.
type crhPools struct {
	moveFn       func(call int) error
	deletePoolFn func(call int) error
	comment      string
	moveCalls    int
	deleteCalls  int
}

func (p *crhPools) CreatePool(_ context.Context, _, _ string) error { return nil }
func (p *crhPools) AddVM(_ context.Context, _ string, _ int64) error {
	return nil
}

func (p *crhPools) MoveVMToPool(_ context.Context, _ string, _ int64) error {
	p.moveCalls++
	if p.moveFn != nil {
		return p.moveFn(p.moveCalls)
	}
	return nil
}

func (p *crhPools) DeletePool(_ context.Context, _ string) error {
	p.deleteCalls++
	if p.deletePoolFn != nil {
		return p.deletePoolFn(p.deleteCalls)
	}
	return nil
}

func (p *crhPools) GetPoolComment(_ context.Context, _ string) (string, bool, error) {
	return p.comment, true, nil
}

// crhClient wires the fakes above into a pve.Client; unset services return
// nil so an unexpected call fails the test with a clear nil-pointer panic.
type crhClient struct {
	storage *crhStorage
	qemuSvc *crhQEMU
	pools   *crhPools
}

func (c *crhClient) QEMU() sdkqemu.Service   { return c.qemuSvc }
func (c *crhClient) Nodes() sdknodes.Service { return nil }
func (c *crhClient) Tasks() sdktasks.Service { return nil }
func (c *crhClient) Storage() sdkstorage.Service {
	return c.storage
}
func (c *crhClient) CloudInit() sdkcloudinit.Service           { return nil }
func (c *crhClient) Cluster() sdkcluster.Service               { return nil }
func (c *crhClient) ClusterStorage() sdkclusterstorage.Service { return nil }
func (c *crhClient) Pools() pve.PoolService                    { return c.pools }

// ---------------------------------------------------------------------------
// L-F-04: create_vm rollback destroy
// ---------------------------------------------------------------------------

func TestCleanupVM_DestroyLockTimeout_RetriesAndSucceeds(t *testing.T) {
	nodes := &lrNodesStub{
		deleteErrs: []error{crhLockErr(), nil},
	}
	q := &lrQEMUStub{}
	deps := Deps{Config: lrTokenConfig(), PVE: &lrClient{nodes: nodes, qemu: q}, Logger: log.NewNopLogger()}

	cleanupVM(crhCtx(), deps, "pve01", 100, lrEnv(), log.NewNopLogger())

	if len(nodes.deleteCalls) != 2 {
		t.Fatalf("expected 2 DeleteQemu calls (lock timeout + retry), got %d", len(nodes.deleteCalls))
	}
	if second := nodes.deleteCalls[1]; second.Skiplock != nil && *second.Skiplock {
		t.Error("a cfs-lock timeout is not a config lock; the retry must not carry skiplock")
	}
	if nodes.updateCalls != 0 {
		t.Errorf("rollback succeeded after retry; the VM must not be tagged as failed (got %d tag writes)", nodes.updateCalls)
	}
}

// TestCleanupVM_SkiplockDestroy_LockTimeout_RetriesAndSucceeds pins the
// skiplock destroy's own retry: the primary destroy is refused by a guest
// config lock (not retried; config locks are not in the retry union), the
// root@pam skiplock retry then fires into the same contended storage state
// and hits a cfs-lock timeout, and one more attempt completes the destroy.
// Single-shot skiplock code would orphan the VM here (nothing re-drives a
// rollback).
func TestCleanupVM_SkiplockDestroy_LockTimeout_RetriesAndSucceeds(t *testing.T) {
	nodes := &lrNodesStub{
		deleteErrs: []error{errors.New(lockedCloneMsg), crhLockErr(), nil},
	}
	q := &lrQEMUStub{}
	deps := Deps{Config: lrRootPamConfig(), PVE: &lrClient{nodes: nodes, qemu: q}, Logger: log.NewNopLogger()}

	cleanupVM(crhCtx(), deps, "pve01", 100, lrEnv(), log.NewNopLogger())

	if len(nodes.deleteCalls) != 3 {
		t.Fatalf("expected 3 DeleteQemu calls (config-locked + skiplock lock-timeout + skiplock retry), got %d",
			len(nodes.deleteCalls))
	}
	for i := 1; i <= 2; i++ {
		if call := nodes.deleteCalls[i]; call.Skiplock == nil || !*call.Skiplock {
			t.Errorf("DeleteQemu call %d must carry skiplock=true", i)
		}
	}
	if nodes.updateCalls != 0 {
		t.Errorf("destroy succeeded after retry; the VM must not be tagged as failed (got %d tag writes)", nodes.updateCalls)
	}
}

// TestCleanupVM_LockClearDestroy_LockTimeout_RetriesAndSucceeds is the token
// identity analogue: no skiplock is available, so the rollback waits for the
// config lock to clear (the stub's Config reports no lock, so the wait
// returns immediately) and the post-lock-clear destroy must also retry
// through a cfs-lock timeout instead of giving up on its last-chance
// reclaim.
func TestCleanupVM_LockClearDestroy_LockTimeout_RetriesAndSucceeds(t *testing.T) {
	nodes := &lrNodesStub{
		deleteErrs: []error{errors.New(lockedCloneMsg), crhLockErr(), nil},
	}
	q := &lrQEMUStub{}
	deps := Deps{Config: lrTokenConfig(), PVE: &lrClient{nodes: nodes, qemu: q}, Logger: log.NewNopLogger()}

	cleanupVM(crhCtx(), deps, "pve01", 100, lrEnv(), log.NewNopLogger())

	if len(nodes.deleteCalls) != 3 {
		t.Fatalf("expected 3 DeleteQemu calls (config-locked + post-lock-clear lock-timeout + retry), got %d",
			len(nodes.deleteCalls))
	}
	for i, call := range nodes.deleteCalls {
		if call.Skiplock != nil && *call.Skiplock {
			t.Errorf("DeleteQemu call %d must not carry skiplock=true for a token identity", i)
		}
	}
	if nodes.updateCalls != 0 {
		t.Errorf("destroy succeeded after retry; the VM must not be tagged as failed (got %d tag writes)", nodes.updateCalls)
	}
}

// ---------------------------------------------------------------------------
// L-F-05: deleteTemplateVM
// ---------------------------------------------------------------------------

func TestDeleteTemplateVM_LockTimeout_RetriesAndSucceeds(t *testing.T) {
	t.Parallel()
	var calls int
	nodes := &wbTemplateNodes{}
	nodes.deleteQemuFn = func(_ context.Context, _, _ string, _ *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error) {
		calls++
		if calls == 1 {
			return nil, crhLockErr()
		}
		raw := sdknodes.DeleteQemuResponse(`""`)
		return &raw, nil
	}
	deps := buildEnsureTemplateDeps(&wbMockQEMU{}, nodes, &wbMockTasks{}, &wbTemplateStorage{})

	if err := deleteTemplateVM(crhCtx(), deps, "pve-node1", 30500, deps.Logger); err != nil {
		t.Fatalf("expected success after lock-timeout retry, got: %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected 2 DeleteQemu calls (lock timeout + retry), got %d", calls)
	}
}

// ---------------------------------------------------------------------------
// L-F-12: single-shot volume sweeps
// ---------------------------------------------------------------------------

func TestRollbackCreatedVolume_LockTimeout_RetriesAndDeletes(t *testing.T) {
	t.Parallel()
	s := &crhStorage{deleteAsyncFn: func(call int) (string, error) {
		if call == 1 {
			return "", crhLockErr()
		}
		return "", nil
	}}
	deps := Deps{PVE: &crhClient{storage: s}, Logger: log.NewNopLogger()}

	rollbackCreatedVolume(crhCtx(), deps, "pve1", "local", "local:vm-9001-disk-0", log.NewNopLogger())

	if s.deleteAsyncCalls != 2 {
		t.Fatalf("expected 2 DeleteVolumeAsync calls (lock timeout + retry), got %d", s.deleteAsyncCalls)
	}
}

func TestSweepEphemeralVolume_LockTimeout_RetriesAndDeletes(t *testing.T) {
	t.Parallel()
	s := &crhStorage{deleteAsyncFn: func(call int) (string, error) {
		if call == 1 {
			return "", crhLockErr()
		}
		return "", nil
	}}
	deps := Deps{PVE: &crhClient{storage: s}, Logger: log.NewNopLogger()}
	shape := &createVMShape{node: "pve1", ephemeralStorage: "local"}

	sweepEphemeralVolumeAfterCreateFailure(crhCtx(), deps, log.NewNopLogger(), shape, 596, "vm-596-disk-1")

	if s.deleteAsyncCalls != 2 {
		t.Fatalf("expected 2 DeleteVolumeAsync calls (lock timeout + retry), got %d", s.deleteAsyncCalls)
	}
}

func TestDeleteHeavyQcow2Replica_LockTimeout_RetriesAndDeletes(t *testing.T) {
	t.Parallel()
	s := &crhStorage{deleteIfExistsFn: func(call int) (bool, error) {
		if call == 1 {
			return false, crhLockErr()
		}
		return true, nil
	}}
	deps := Deps{PVE: &crhClient{storage: s}, Logger: log.NewNopLogger()}
	logger, obs := log.NewObservedLogger(log.LevelInfo)

	deleteHeavyQcow2ReplicaBestEffort(crhCtx(), deps, logger, "pve2", "local", "import/stemcell.qcow2")

	if s.deleteIfCalls != 2 {
		t.Fatalf("expected 2 DeleteVolumeIfExists calls (lock timeout + retry), got %d", s.deleteIfCalls)
	}
	for _, e := range obs.All() {
		if strings.Contains(e.Message, "delete failed") {
			t.Fatalf("success after retry must not log the best-effort failure line: %q", e.Message)
		}
	}
}

// ---------------------------------------------------------------------------
// L-F-13: probe failures are visible
// ---------------------------------------------------------------------------

func TestSweepEphemeralVolume_ProbeError_WarnsWithVolid(t *testing.T) {
	t.Parallel()
	s := &crhStorage{existsFn: func() (bool, error) {
		return false, errors.New("596 storage 'local' is not online")
	}}
	deps := Deps{PVE: &crhClient{storage: s}, Logger: log.NewNopLogger()}
	logger, obs := log.NewObservedLogger(log.LevelInfo)
	shape := &createVMShape{node: "pve1", ephemeralStorage: "local"}

	sweepEphemeralVolumeAfterCreateFailure(crhCtx(), deps, logger, shape, 596, "vm-596-disk-1")

	if s.deleteAsyncCalls != 0 {
		t.Fatalf("a failed probe must skip the sweep, got %d delete calls", s.deleteAsyncCalls)
	}
	var warned bool
	for _, e := range obs.All() {
		if e.Level == log.LevelWarn && strings.Contains(e.Message, "existence probe failed") {
			warned = true
			if got, _ := e.Attrs["volid"].(string); got != "local:vm-596-disk-1" {
				t.Errorf("probe warn must name the volid, got %v", e.Attrs["volid"])
			}
		}
	}
	if !warned {
		t.Fatal("a failed existence probe must land a Warn instead of being indistinguishable from nothing-to-clean")
	}
}

// ---------------------------------------------------------------------------
// L-F-14: pool pipeline
// ---------------------------------------------------------------------------

func TestMovePoolMember_MoveLockTimeout_RetriesAndReportsSuccess(t *testing.T) {
	t.Parallel()
	pools := &crhPools{moveFn: func(call int) error {
		if call == 1 {
			return errors.New("cfs-lock 'user_cfg' error: got lock request timeout")
		}
		return nil
	}}
	deps := Deps{PVE: &crhClient{pools: pools}, Logger: log.NewNopLogger()}

	ok := movePoolMember(crhCtx(), deps, 596, "dep-pool", "old-pool", "dir-1", log.NewNopLogger())

	if !ok {
		t.Fatal("move succeeded on retry; movePoolMember must report success so the caller writes the sentinel")
	}
	if pools.moveCalls != 2 {
		t.Fatalf("expected 2 MoveVMToPool calls (lock timeout + retry), got %d", pools.moveCalls)
	}
}

func TestReapEmptyPool_LockTimeout_RetriesAndReaps(t *testing.T) {
	t.Parallel()
	pools := &crhPools{
		comment: pve.PoolProvenanceComment + " (director dir-1)",
		deletePoolFn: func(call int) error {
			if call == 1 {
				return errors.New("cfs-lock 'user_cfg' error: got lock request timeout")
			}
			return nil
		},
	}
	deps := Deps{
		Config: &config.CPIConfig{PoolReapEmpty: true, VMPool: "static-pool", StemcellTemplatePool: "stem-pool"},
		PVE:    &crhClient{pools: pools},
		Logger: log.NewNopLogger(),
	}
	logger, obs := log.NewObservedLogger(log.LevelInfo)

	reapEmptyPoolIfManaged(crhCtx(), deps, "dep-pool", logger)

	if pools.deleteCalls != 2 {
		t.Fatalf("expected 2 DeletePool calls (lock timeout + retry), got %d", pools.deleteCalls)
	}
	var reaped bool
	for _, e := range obs.All() {
		if strings.Contains(e.Message, "reaped empty pool") {
			reaped = true
		}
	}
	if !reaped {
		t.Fatal("success after retry must log the reaped line, not the non-fatal failure")
	}
}

func TestReapEmptyPool_NotEmptyVerdict_SingleCall(t *testing.T) {
	t.Parallel()
	// "Pool is not empty" is a resolved verdict (another VM joined the pool),
	// not contention; the reaper must tolerate it after exactly one call
	// instead of spending the lock retry budget on an answer that will not
	// change.
	pools := &crhPools{
		comment: pve.PoolProvenanceComment,
		deletePoolFn: func(int) error {
			return errors.New("500 delete pool failed: pool 'dep-pool' is not empty")
		},
	}
	deps := Deps{
		Config: &config.CPIConfig{PoolReapEmpty: true},
		PVE:    &crhClient{pools: pools},
		Logger: log.NewNopLogger(),
	}

	reapEmptyPoolIfManaged(crhCtx(), deps, "dep-pool", log.NewNopLogger())

	if pools.deleteCalls != 1 {
		t.Fatalf("a not-empty verdict must not be retried, got %d DeletePool calls", pools.deleteCalls)
	}
}

// ---------------------------------------------------------------------------
// L-F-18: detach_disk unused-slot sweep
// ---------------------------------------------------------------------------

func TestSweepUnusedDiskSlot_LockTimeout_RetriesAndSweeps(t *testing.T) {
	t.Parallel()
	q := &crhQEMU{
		cfg: map[string]any{"unused0": "local:vm-596-disk-2"},
		detachFn: func(call int) error {
			if call == 1 {
				return crhLockErr()
			}
			return nil
		},
	}
	deps := Deps{PVE: &crhClient{qemuSvc: q}, Logger: log.NewNopLogger()}

	swept, err := sweepUnusedDiskSlot(crhCtx(), deps, "pve1", 596, "596", "local:vm-596-disk-2")
	if err != nil {
		t.Fatalf("expected success after lock-timeout retry, got: %v", err)
	}
	if !swept {
		t.Fatal("expected the lingering unused slot to be swept")
	}
	if q.detachCalls != 2 {
		t.Fatalf("expected 2 DetachDisk calls (lock timeout + retry), got %d", q.detachCalls)
	}
}

// crh500 fabricates the typed HTTP-500 shape PVE uses for pmxcfs and pool
// verdicts. The typed 500 matters: a bare errors.New would never enter the
// retry union, so the short-circuit these tests pin would pass vacuously on
// unfixed code.
func crh500(body string) error {
	return sdkerrors.ParseAPIError(500, []byte(body))
}

// crhConfigMissingErr models the pmxcfs verdict a replayed destroy gets for a
// VM whose config is already gone.
func crhConfigMissingErr() error {
	return crh500("Configuration file 'nodes/pve01/qemu-server/100.conf' does not exist")
}

func TestCleanupVM_PersistentLockTimeout_BoundedDestroyBudget(t *testing.T) {
	t.Parallel()
	nodes := &lrNodesStub{
		deleteErrs: []error{crhLockErr()},
	}
	deps := Deps{Config: lrTokenConfig(), PVE: &lrClient{nodes: nodes, qemu: &lrQEMUStub{}}, Logger: log.NewNopLogger()}

	cleanupVM(crhCtx(), deps, "pve01", 100, lrEnv(), log.NewNopLogger())

	if len(nodes.deleteCalls) != rollbackDestroyMaxAttempts {
		t.Fatalf("destroy retry must stop at the bounded rollback budget: expected %d DeleteQemu calls, got %d",
			rollbackDestroyMaxAttempts, len(nodes.deleteCalls))
	}
}

func TestCleanupVM_ConfigMissing500_SingleCall(t *testing.T) {
	t.Parallel()
	nodes := &lrNodesStub{
		deleteErrs: []error{crhConfigMissingErr()},
	}
	deps := Deps{Config: lrTokenConfig(), PVE: &lrClient{nodes: nodes, qemu: &lrQEMUStub{}}, Logger: log.NewNopLogger()}

	cleanupVM(crhCtx(), deps, "pve01", 100, lrEnv(), log.NewNopLogger())

	if len(nodes.deleteCalls) != 1 {
		t.Fatalf("an already-gone verdict must not spend the retry budget: expected 1 DeleteQemu call, got %d",
			len(nodes.deleteCalls))
	}
	if nodes.updateCalls != 0 {
		t.Errorf("already-gone is idempotent success; the VM must not be tagged as failed (got %d tag writes)", nodes.updateCalls)
	}
}

func TestDeleteTemplateVM_ConfigMissing500_ToleratedAsGone(t *testing.T) {
	t.Parallel()
	var calls int
	nodes := &wbTemplateNodes{}
	nodes.deleteQemuFn = func(_ context.Context, _, _ string, _ *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error) {
		calls++
		return nil, crhConfigMissingErr()
	}
	deps := buildEnsureTemplateDeps(&wbMockQEMU{}, nodes, &wbMockTasks{}, &wbTemplateStorage{})

	if err := deleteTemplateVM(crhCtx(), deps, "pve-node1", 30500, deps.Logger); err != nil {
		t.Fatalf("a destroyed template's config-missing 500 must resolve as success, got: %v", err)
	}
	if calls != 1 {
		t.Fatalf("an already-gone verdict must not spend the retry budget: expected 1 DeleteQemu call, got %d", calls)
	}
}

func TestDestroyTemplateVM_ConfigMissing500_ToleratedAsGone(t *testing.T) {
	t.Parallel()
	nodes := &lrNodesStub{
		deleteErrs: []error{crhConfigMissingErr()},
	}
	deps := Deps{Config: lrTokenConfig(), PVE: &lrClient{nodes: nodes, qemu: &lrQEMUStub{}}, Logger: log.NewNopLogger()}

	if err := destroyTemplateVM(crhCtx(), deps, "pve01", 30500, "sc-test"); err != nil {
		t.Fatalf("a destroyed template's config-missing 500 must resolve as success, got: %v", err)
	}
	if len(nodes.deleteCalls) != 1 {
		t.Fatalf("an already-gone verdict must not spend the retry budget: expected 1 DeleteQemu call, got %d",
			len(nodes.deleteCalls))
	}
}

func TestMovePoolMember_PersistentLockTimeout_BoundedBudget(t *testing.T) {
	t.Parallel()
	pools := &crhPools{moveFn: func(int) error { return crhLockErr() }}
	deps := Deps{PVE: &crhClient{pools: pools}, Logger: log.NewNopLogger()}

	ok := movePoolMember(crhCtx(), deps, 596, "dep-pool", "old-pool", "dir-1", log.NewNopLogger())
	if ok {
		t.Fatal("a move that never succeeded must not report success")
	}
	if pools.moveCalls != cleanupSweepMaxAttempts {
		t.Fatalf("best-effort request-path move must stop at the sweep budget: expected %d MoveVMToPool calls, got %d",
			cleanupSweepMaxAttempts, pools.moveCalls)
	}
}

func TestReapEmptyPool_PersistentLockTimeout_BoundedBudget(t *testing.T) {
	t.Parallel()
	pools := &crhPools{
		comment:      pve.PoolProvenanceComment + " (director dir-1)",
		deletePoolFn: func(int) error { return crhLockErr() },
	}
	deps := Deps{
		Config: &config.CPIConfig{PoolReapEmpty: true},
		PVE:    &crhClient{pools: pools},
		Logger: log.NewNopLogger(),
	}

	reapEmptyPoolIfManaged(crhCtx(), deps, "dep-pool", log.NewNopLogger())

	if pools.deleteCalls != cleanupSweepMaxAttempts {
		t.Fatalf("best-effort request-path reap must stop at the sweep budget: expected %d DeletePool calls, got %d",
			cleanupSweepMaxAttempts, pools.deleteCalls)
	}
}

func TestSweepUnusedDiskSlot_PersistentLockTimeout_TransientBudget(t *testing.T) {
	t.Parallel()
	q := &crhQEMU{
		cfg:      map[string]any{"unused0": "local:vm-596-disk-2"},
		detachFn: func(int) error { return crhLockErr() },
	}
	deps := Deps{PVE: &crhClient{qemuSvc: q}, Logger: log.NewNopLogger()}

	swept, err := sweepUnusedDiskSlot(crhCtx(), deps, "pve1", 596, "596", "local:vm-596-disk-2")
	if err == nil {
		t.Fatal("a sweep that never succeeded must surface its error")
	}
	if swept {
		t.Fatal("a failed sweep must not report the slot as swept")
	}
	if q.detachCalls != pve.TransientMaxAttempts() {
		t.Fatalf("sweep must run on the explicit transient budget: expected %d DetachDisk calls, got %d",
			pve.TransientMaxAttempts(), q.detachCalls)
	}
}

// PoolHasVM reports no membership; tests that exercise the
// disambiguation supply their own fake.
func (p *crhPools) PoolHasVM(context.Context, string, int64) (bool, error) {
	return false, nil
}
