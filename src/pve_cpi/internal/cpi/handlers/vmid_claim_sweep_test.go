package handlers_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// exclusiveClaimPools is a pool service whose create refuses a pool that
// already exists, the way PVE does, so two VMID claims exclude each other.
type exclusiveClaimPools struct {
	mu    sync.Mutex
	pools map[string]string
}

func newExclusiveClaimPools() *exclusiveClaimPools {
	return &exclusiveClaimPools{pools: map[string]string{}}
}

func (p *exclusiveClaimPools) AddVM(context.Context, string, int64) error        { return nil }
func (p *exclusiveClaimPools) MoveVMToPool(context.Context, string, int64) error { return nil }
func (p *exclusiveClaimPools) PoolHasVM(context.Context, string, int64) (bool, error) {
	return false, nil
}

func (p *exclusiveClaimPools) CreatePool(_ context.Context, poolID, comment string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.pools[poolID]; ok {
		return fmt.Errorf("pool '%s' already exists", poolID)
	}
	p.pools[poolID] = comment
	return nil
}

func (p *exclusiveClaimPools) DeletePool(_ context.Context, poolID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.pools[poolID]; !ok {
		return fmt.Errorf("pool '%s' does not exist", poolID)
	}
	delete(p.pools, poolID)
	return nil
}

func (p *exclusiveClaimPools) GetPoolComment(_ context.Context, poolID string) (string, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	comment, ok := p.pools[poolID]
	return comment, ok, nil
}

// held reports whether any process claims vmid.
func (p *exclusiveClaimPools) held(vmid int) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.pools[pve.ClusterLockPoolName(fmt.Sprintf("vm-%d", vmid))]
	return ok
}

// hold records another process's claim on vmid, expiring at exp.
func (p *exclusiveClaimPools) hold(vmid int, owner string, exp time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pools[pve.ClusterLockPoolName(fmt.Sprintf("vm-%d", vmid))] = "owner=" + owner + " exp=" +
		strconv.FormatInt(exp.Unix(), 10)
}

// claimSweepFixture is a fast-path delete_vm whose sweep sees one queued VM,
// the straggler, carrying stragglerTags in both the index and its config.
type claimSweepFixture struct {
	deps          handlers.Deps
	mu            sync.Mutex
	destroys      map[string]int
	listStraggler atomic.Bool
	// block, when set, holds the first destroy of the straggler until it is
	// closed, and entered is closed once that destroy has started.
	block   chan struct{}
	entered chan struct{}
	once    sync.Once
}

const (
	claimSweepStraggler = 811
	claimSweepNode      = "pve-node1"
)

func newClaimSweepFixture(t *testing.T, stragglerTags string, pools pve.PoolService) *claimSweepFixture {
	t.Helper()
	f := &claimSweepFixture{destroys: map[string]int{}, entered: make(chan struct{})}
	f.listStraggler.Store(true)
	stragglerRaw, err := json.Marshal(map[string]any{
		"vmid": claimSweepStraggler, "node": claimSweepNode, "type": "qemu", "tags": stragglerTags,
	})
	if err != nil {
		t.Fatal(err)
	}
	clusterSvc := &mockClusterSvc{
		listResourcesFn: func(context.Context, *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
			resp := cluster.ListResourcesResponse{}
			if f.listStraggler.Load() {
				resp = append(resp, stragglerRaw)
			}
			return &resp, nil
		},
	}
	qemuSvc := &mockQEMUService{
		stopFn: func(context.Context, string, int) (string, error) { return "", nil },
		configFn: func(_ context.Context, _ string, vmid int) (map[string]any, error) {
			if vmid == claimSweepStraggler {
				return map[string]any{"tags": stragglerTags}, nil
			}
			return map[string]any{}, nil
		},
	}
	nodesSvc := &mockNodesService{
		deleteQemuFn: func(_ context.Context, _ string, vmid string, _ *nodes.DeleteQemuParams) (*nodes.DeleteQemuResponse, error) {
			f.mu.Lock()
			f.destroys[vmid]++
			first := f.destroys[vmid] == 1
			f.mu.Unlock()
			if vmid == strconv.Itoa(claimSweepStraggler) && first && f.block != nil {
				f.once.Do(func() { close(f.entered) })
				<-f.block
			}
			return &nodes.DeleteQemuResponse{}, nil
		},
		updateQemuConfigFn: func(context.Context, string, string, *nodes.UpdateQemuConfigParams) error { return nil },
	}
	f.deps = testDepsWithCluster(qemuSvc, nodesSvc, &mockTasksService{}, &mockAgentService{}, &mockStorageService{}, clusterSvc)
	enabled := true
	f.deps.Config.FastPathDelete = &enabled
	f.deps.PVE.(*mockPVEClient).poolsSvc = pools
	return f
}

func (f *claimSweepFixture) destroyed(vmid int) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.destroys[strconv.Itoa(vmid)]
}

func (f *claimSweepFixture) deleteVM(t *testing.T, vmid int) {
	t.Helper()
	if _, err := handlers.HandleDeleteVM(f.deps).Handle(context.Background(), marshalArgs(strconv.Itoa(vmid)),
		jsonrpc.Context{}); err != nil {
		t.Fatalf("delete_vm %d: %v", vmid, err)
	}
}

// TestDeleteVM_SweepNeverRacesADeleteThatHoldsTheClaim runs a fast-path
// delete of the straggler that stamps bosh-deleting and stalls in its destroy,
// and a second delete whose sweep sees the stamped VM meanwhile. The second
// sweep must leave the VM to the delete that claims it, and the claim must be
// free once that delete returns.
func TestDeleteVM_SweepNeverRacesADeleteThatHoldsTheClaim(t *testing.T) {
	pools := newExclusiveClaimPools()
	f := newClaimSweepFixture(t, "bosh-cpi;bosh-deleting", pools)
	f.block = make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-f.block:
		default:
			close(f.block)
		}
	})
	f.listStraggler.Store(false)

	done := make(chan error, 1)
	go func() {
		_, err := handlers.HandleDeleteVM(f.deps).Handle(context.Background(),
			marshalArgs(strconv.Itoa(claimSweepStraggler)), jsonrpc.Context{})
		done <- err
	}()
	select {
	case <-f.entered:
	case <-time.After(30 * time.Second):
		t.Fatal("the first delete never reached its destroy")
	}
	f.listStraggler.Store(true)

	f.deleteVM(t, 810)
	if got := f.destroyed(claimSweepStraggler); got != 1 {
		t.Fatalf("the sweep must not destroy a VM another delete claims; %d destroys", got)
	}
	if f.destroyed(810) != 1 {
		t.Fatal("the second delete must still destroy its own VM")
	}

	close(f.block)
	if err := <-done; err != nil {
		t.Fatalf("the first delete: %v", err)
	}
	if pools.held(claimSweepStraggler) {
		t.Fatal("the first delete must release its claim when it returns")
	}
}

// TestDeleteVM_SweepLeavesABirthTaggedVMWhileItsCreateClaimsIt shows an
// expired birth tag is not enough: a create that still claims the VMID is
// still running, whether or not anything enforces its timeout, and the sweep
// leaves it. Once that claim has expired the VM is abandoned, and the sweep
// steals the claim and reaps it.
func TestDeleteVM_SweepLeavesABirthTaggedVMWhileItsCreateClaimsIt(t *testing.T) {
	pools := newExclusiveClaimPools()
	pools.hold(claimSweepStraggler, "create_vm/811", time.Now().Add(time.Hour))
	f := newClaimSweepFixture(t, "bosh-cpi;bosh-creating-1000", pools)

	f.deleteVM(t, 810)
	if got := f.destroyed(claimSweepStraggler); got != 0 {
		t.Fatalf("a create that claims the VMID is still running; %d destroys", got)
	}

	pools.hold(claimSweepStraggler, "create_vm/811", time.Now().Add(-time.Minute))
	f.deleteVM(t, 812)
	if got := f.destroyed(claimSweepStraggler); got != 1 {
		t.Fatalf("an abandoned create's VM must be reaped once its claim has expired; %d destroys", got)
	}
}

func TestDeleteVM_SweepLeavesQueuedVMs(t *testing.T) {
	cases := map[string]struct {
		tags       string
		pools      pve.PoolService
		keepFailed bool
	}{
		"without a pool service no claim proves the VM abandoned": {tags: "bosh-cpi;bosh-creating-1000"},
		"keep_failed_vms keeps an expired birth tag": {
			tags: "bosh-cpi;bosh-creating-1000", pools: newExclusiveClaimPools(), keepFailed: true,
		},
		"an unfinished HA re-registration is never reaped": {
			tags: "bosh-cpi;bosh-deleting;bosh-ha-readd-1000", pools: newExclusiveClaimPools(),
		},
		"a fresh birth tag is still settling": {
			tags: "bosh-cpi;bosh-creating-" + strconv.FormatInt(time.Now().Unix(), 10), pools: newExclusiveClaimPools(),
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newClaimSweepFixture(t, tc.tags, tc.pools)
			if tc.keepFailed {
				f.deps.Config.Debug = &config.DebugConfig{KeepFailedVMs: boolPtr(true)}
			}
			f.deleteVM(t, 810)
			if got := f.destroyed(claimSweepStraggler); got != 0 {
				t.Fatalf("the sweep must leave the VM; %d destroys", got)
			}
			if f.destroyed(810) != 1 {
				t.Fatal("the delete must still destroy its own VM")
			}
		})
	}
}

func TestDeleteVM_SweepReapsAnExpiredBirthTag(t *testing.T) {
	f := newClaimSweepFixture(t, "bosh-cpi;bosh-creating-1000", newExclusiveClaimPools())
	f.deleteVM(t, 810)
	if got := f.destroyed(claimSweepStraggler); got != 1 {
		t.Fatalf("an unclaimed VM with an expired birth tag must be reaped; %d destroys", got)
	}
}

func (p *exclusiveClaimPools) empty() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.pools) == 0
}

// TestCreateVM_ClaimsTheNewVMIDUntilTheCreateReturns pins the claim a legacy
// create holds: taken before the clone, held while the VM carries its birth
// tag, and released once the create returns.
func TestCreateVM_ClaimsTheNewVMIDUntilTheCreateReturns(t *testing.T) {
	pools := newExclusiveClaimPools()
	var cloneVMID int
	heldDuringClone := false
	n := &vmMockNodes{
		createQemuCloneFn: func(_ context.Context, _, _ string, p *nodes.CreateQemuCloneParams) (*nodes.CreateQemuCloneResponse, error) {
			cloneVMID = int(p.Newid)
			heldDuringClone = pools.held(cloneVMID)
			return cloneUPIDResponse(), nil
		},
	}
	deps := buildVMDepsForTemplate(&vmMockQEMU{}, n, &vmMockCluster{}, &vmMockAgent{})
	deps.PVE.(*mockPVEClient).poolsSvc = pools

	args := mkArgs("agent-claim", testTemplateCID, map[string]any{"cores": 1, "memory": 512},
		map[string]any{"default": map[string]any{"type": "dynamic", "cloud_properties": map[string]any{}}},
		[]string{}, map[string]any{})
	if _, err := handlers.HandleCreateVM(deps).Handle(context.Background(), args, mkCtx("create-claim")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cloneVMID == 0 || !heldDuringClone {
		t.Fatalf("the create must claim VM %d before its clone", cloneVMID)
	}
	if !pools.empty() {
		t.Fatal("the create must release its claim when it returns")
	}
}

// TestCreateVM_ReleasesTheClaimOfAFailedClone pins the release on the failure
// path, so a failed create never leaves a claim that blocks the sweep.
func TestCreateVM_ReleasesTheClaimOfAFailedClone(t *testing.T) {
	pools := newExclusiveClaimPools()
	n := &vmMockNodes{
		createQemuCloneFn: func(context.Context, string, string, *nodes.CreateQemuCloneParams) (*nodes.CreateQemuCloneResponse, error) {
			return nil, fmt.Errorf("clone refused: storage full")
		},
	}
	deps := buildVMDepsForTemplate(&vmMockQEMU{}, n, &vmMockCluster{}, &vmMockAgent{})
	deps.PVE.(*mockPVEClient).poolsSvc = pools

	args := mkArgs("agent-claim-fail", testTemplateCID, map[string]any{"cores": 1, "memory": 512},
		map[string]any{"default": map[string]any{"type": "dynamic", "cloud_properties": map[string]any{}}},
		[]string{}, map[string]any{})
	if _, err := handlers.HandleCreateVM(deps).Handle(context.Background(), args, mkCtx("create-claim-fail")); err == nil {
		t.Fatal("a refused clone must fail the create")
	}
	if !pools.empty() {
		t.Fatal("a failed create must release every claim it took")
	}
}
