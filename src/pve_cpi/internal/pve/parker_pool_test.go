// parker_pool_test.go holds the tests for the parker pool placement sweep.
// They live in package pve rather than pve_test because two of them read
// parkerPoolSweepTimeout, which is the bound the sweep is allowed to spend and
// which no exported symbol reports.
//
// The sweep is cosmetic and best-effort, so most of what these tests pin is
// what it must NOT do. It must not fail a park, it must not touch a parker that
// another pool already holds, it must not read the lagging cluster index, and
// it must not run past its bound. Each of those is a change somebody could make
// later in good faith, and each one would be invisible without a test that says
// so.
package pve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	sdkcloudinit "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cloudinit"
	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdkclusterstorage "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/clusterstorage"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	sdkqemu "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
	sdkstorage "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/storage"
	sdktasks "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/tasks"
)

const (
	sweepNode = "pve1"
	sweepPool = "acme-parker"
)

// sweepPools is the pool service the sweep drives. It holds membership in a
// map keyed by pool, so a parker another pool already owns and a parker the
// target pool already owns are both expressible, and it records every call in
// order so a test can assert on the sequence rather than on the end state
// alone.
type sweepPools struct {
	members map[string]map[int64]bool
	calls   []string

	createErr error
	addErr    error
	probeErr  error
	// block, when non-nil, is waited on by CreatePool until it closes or the
	// context ends, which is how the bound test holds the sweep still.
	block <-chan struct{}
	// deadlines records the deadline every call saw, which is how the bound
	// test reads the sweep's own context without a wall-clock wait.
	deadlines []time.Time
}

func newSweepPools() *sweepPools {
	return &sweepPools{members: map[string]map[int64]bool{}}
}

func (p *sweepPools) record(ctx context.Context, call string) {
	p.calls = append(p.calls, call)
	if deadline, ok := ctx.Deadline(); ok {
		p.deadlines = append(p.deadlines, deadline)
	}
}

func (p *sweepPools) CreatePool(ctx context.Context, poolID, comment string) error {
	p.record(ctx, "create:"+poolID+":"+comment)
	if p.block != nil {
		select {
		case <-p.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return p.createErr
}

func (p *sweepPools) DeletePool(ctx context.Context, poolID string) error {
	p.record(ctx, "delete:"+poolID)
	return nil
}

func (p *sweepPools) GetPoolComment(ctx context.Context, poolID string) (string, bool, error) {
	p.record(ctx, "comment:"+poolID)
	return "", false, nil
}

func (p *sweepPools) AddVM(ctx context.Context, poolID string, vmid int64) error {
	p.record(ctx, fmt.Sprintf("add:%s:%d", poolID, vmid))
	if p.addErr != nil {
		return p.addErr
	}
	if p.members[poolID] == nil {
		p.members[poolID] = map[int64]bool{}
	}
	p.members[poolID][vmid] = true
	return nil
}

func (p *sweepPools) MoveVMToPool(ctx context.Context, poolID string, vmid int64) error {
	p.record(ctx, fmt.Sprintf("move:%s:%d", poolID, vmid))
	return nil
}

func (p *sweepPools) PoolHasVM(ctx context.Context, poolID string, vmid int64) (bool, error) {
	p.record(ctx, fmt.Sprintf("has:%s:%d", poolID, vmid))
	if p.probeErr != nil {
		return false, p.probeErr
	}
	return p.members[poolID][vmid], nil
}

// sweepNodesSvc answers the node's own qemu listing with one row per scripted
// guest, which is the listing ListParkersForNode reads.
type sweepNodesSvc struct {
	sdknodes.Service
	guests map[int]string
	err    error
}

func (n *sweepNodesSvc) ListQemu(
	_ context.Context, _ string, _ *sdknodes.ListQemuParams,
) (*sdknodes.ListQemuResponse, error) {
	if n.err != nil {
		return nil, n.err
	}
	resp := sdknodes.ListQemuResponse{}
	for vmid, tags := range n.guests {
		raw, err := json.Marshal(map[string]any{"vmid": vmid, "tags": tags})
		if err != nil {
			return nil, err
		}
		resp = append(resp, raw)
	}
	return &resp, nil
}

// sweepClusterSvc fails the test the moment the sweep reads the cluster index.
type sweepClusterSvc struct {
	sdkcluster.Service
	t      *testing.T
	rows   []json.RawMessage
	banned bool
}

func (c *sweepClusterSvc) ListResources(
	_ context.Context, _ *sdkcluster.ListResourcesParams,
) (*sdkcluster.ListResourcesResponse, error) {
	if c.banned {
		c.t.Error("the sweep read /cluster/resources; membership must come from " +
			"PoolHasVM, because the index lags by minutes and the parker this park " +
			"just created is the one most likely to be missing from it")
	}
	resp := sdkcluster.ListResourcesResponse(c.rows)
	return &resp, nil
}

// sweepTestClient wires only the services the sweep may touch. Every other
// service is nil, so an unplanned call panics loudly instead of passing.
type sweepTestClient struct {
	pools   PoolService
	nodes   sdknodes.Service
	cluster sdkcluster.Service
}

func (c *sweepTestClient) QEMU() sdkqemu.Service                     { return nil }
func (c *sweepTestClient) Storage() sdkstorage.Service               { return nil }
func (c *sweepTestClient) CloudInit() sdkcloudinit.Service           { return nil }
func (c *sweepTestClient) Tasks() sdktasks.Service                   { return nil }
func (c *sweepTestClient) Nodes() sdknodes.Service                   { return c.nodes }
func (c *sweepTestClient) Cluster() sdkcluster.Service               { return c.cluster }
func (c *sweepTestClient) ClusterStorage() sdkclusterstorage.Service { return nil }
func (c *sweepTestClient) Pools() PoolService                        { return c.pools }

func sweepCfg() ParkerConfig {
	return ParkerConfig{
		VMIDRangeStart: 90000,
		VMIDRangeEnd:   90999,
		Pool:           sweepPool,
		DirectorID:     "d1",
	}
}

// sweepFixture builds a client holding the given guests, with a cluster
// service that fails the test if the sweep reads the index.
func sweepFixture(t *testing.T, pools *sweepPools, guests map[int]string) *sweepTestClient {
	t.Helper()
	return &sweepTestClient{
		pools:   pools,
		nodes:   &sweepNodesSvc{guests: guests},
		cluster: &sweepClusterSvc{t: t, banned: true},
	}
}

// countCalls reports how many recorded calls start with prefix.
func countCalls(calls []string, prefix string) int {
	n := 0
	for _, call := range calls {
		if strings.HasPrefix(call, prefix) {
			n++
		}
	}
	return n
}

func TestPlaceParkersInPool_AssignsEveryUnpooledParker(t *testing.T) {
	t.Parallel()

	pools := newSweepPools()
	c := sweepFixture(t, pools, map[int]string{
		90000: "bosh-cpi;bosh-parker;director--d1",
		90001: "bosh-cpi;bosh-parker;director--d1",
		// A workload VM outside the band, which the listing filters out.
		120: "bosh-cpi",
	})

	if err := PlaceParkersInPool(context.Background(), c, nil, sweepNode, sweepCfg()); err != nil {
		t.Fatalf("PlaceParkersInPool: unexpected error: %v", err)
	}

	if !pools.members[sweepPool][90000] || !pools.members[sweepPool][90001] {
		t.Errorf("both parkers must end up in %q, got %v", sweepPool, pools.members[sweepPool])
	}
	if got := countCalls(pools.calls, "add:"); got != 2 {
		t.Errorf("AddVM calls = %d, want 2; calls = %v", got, pools.calls)
	}
	if got := countCalls(pools.calls, "move:"); got != 0 {
		t.Errorf("MoveVMToPool must never be called; calls = %v", pools.calls)
	}
	wantComment := PoolProvenance("d1")
	if len(pools.calls) == 0 || pools.calls[0] != "create:"+sweepPool+":"+wantComment {
		t.Errorf("the sweep must ensure the pool first with the provenance comment %q, got %v",
			wantComment, pools.calls)
	}
}

func TestPlaceParkersInPool_SkipsAParkerTheTargetPoolAlreadyHolds(t *testing.T) {
	t.Parallel()

	pools := newSweepPools()
	pools.members[sweepPool] = map[int64]bool{90000: true}
	c := sweepFixture(t, pools, map[int]string{90000: "bosh-parker;director--d1"})

	if err := PlaceParkersInPool(context.Background(), c, nil, sweepNode, sweepCfg()); err != nil {
		t.Fatalf("PlaceParkersInPool: unexpected error: %v", err)
	}

	if got := countCalls(pools.calls, "add:"); got != 0 {
		t.Errorf("a parker the pool already holds must not be touched; calls = %v", pools.calls)
	}
}

func TestPlaceParkersInPool_LeavesAParkerAnotherPoolHolds(t *testing.T) {
	t.Parallel()

	// PVE answers AddVM for a guest another pool holds with "already a pool
	// member", which does not name the pool. AssignVMToPool re-probes, finds
	// the target pool does not hold it, and returns a permanent error naming
	// both pools. The sweep skips that parker and carries on, because moving
	// a parker out of a pool an operator chose is not a cosmetic sweep's call
	// to make.
	pools := newSweepPools()
	pools.addErr = errors.New("500 VM 90000 is already a pool member")
	// The cluster index is readable here, and deliberately so. The sweep still
	// never reads it; AssignVMToPool does, once its own membership probe has
	// settled the verdict, and only to name the other pool in the message.
	other, err := json.Marshal(map[string]any{"vmid": 90000, "pool": "an-operators-pool"})
	if err != nil {
		t.Fatal(err)
	}
	c := &sweepTestClient{
		pools: pools,
		nodes: &sweepNodesSvc{guests: map[int]string{
			90000: "bosh-parker;director--d1",
			90001: "bosh-parker;director--d1",
		}},
		cluster: &sweepClusterSvc{t: t, rows: []json.RawMessage{other}},
	}

	if sweepErr := PlaceParkersInPool(context.Background(), c, nil, sweepNode, sweepCfg()); sweepErr != nil {
		t.Fatalf("a parker in another pool must not surface as a sweep failure: %v", sweepErr)
	}
	if got := countCalls(pools.calls, "add:"); got != 2 {
		t.Errorf("the second parker must still be attempted; calls = %v", pools.calls)
	}
	if got := countCalls(pools.calls, "move:"); got != 0 {
		t.Errorf("MoveVMToPool must never reconcile a parker another pool holds; calls = %v", pools.calls)
	}
}

func TestPlaceParkersInPool_OtherPoolSkipReadsTheSentinelNotTheMessage(t *testing.T) {
	t.Parallel()

	// The skip above turns on ErrVMInAnotherPool, which AssignVMToPool chains
	// onto that one verdict. An unrelated failure whose text happens to read
	// like the verdict is still a failure, and this case is here so that a
	// later rewording of either message cannot quietly turn real failures into
	// silent skips.
	pools := newSweepPools()
	pools.addErr = errors.New("500 vmid 90000 already belongs to something the storage layer is unhappy about")
	c := sweepFixture(t, pools, map[int]string{90000: "bosh-parker;director--d1"})

	err := PlaceParkersInPool(context.Background(), c, nil, sweepNode, sweepCfg())
	if err == nil {
		t.Fatal("an error that merely reads like the other-pool verdict must still be a failure")
	}
	if errors.Is(err, ErrVMInAnotherPool) {
		t.Error("nothing chained the sentinel, so nothing may report it")
	}
}

func TestAssignVMToPool_AnotherPoolVerdictCarriesTheSentinel(t *testing.T) {
	t.Parallel()

	// The other half of the contract the sweep depends on. PVE answers with
	// "already a pool member", the membership probe says the target pool does
	// not hold the guest, and the error that comes back names both pools and
	// chains the sentinel.
	pools := newSweepPools()
	pools.addErr = errors.New("500 VM 90000 is already a pool member")
	other, err := json.Marshal(map[string]any{"vmid": 90000, "pool": "an-operators-pool"})
	if err != nil {
		t.Fatal(err)
	}
	c := &sweepTestClient{
		pools:   pools,
		cluster: &sweepClusterSvc{t: t, rows: []json.RawMessage{other}},
	}

	assignErr := AssignVMToPool(context.Background(), c, sweepPool, 90000, nil)
	if !errors.Is(assignErr, ErrVMInAnotherPool) {
		t.Fatalf("want the other-pool sentinel chained, got %v", assignErr)
	}
	if !strings.Contains(assignErr.Error(), "an-operators-pool") {
		t.Errorf("the message must still name the pool that holds the guest, got %q", assignErr)
	}
}

func TestPlaceParkersInPool_EmptyPoolMakesNoCall(t *testing.T) {
	t.Parallel()

	pools := newSweepPools()
	c := sweepFixture(t, pools, map[int]string{90000: "bosh-parker;director--d1"})
	cfg := sweepCfg()
	cfg.Pool = ""

	if err := PlaceParkersInPool(context.Background(), c, nil, sweepNode, cfg); err != nil {
		t.Fatalf("the documented opt-out must not error: %v", err)
	}
	if len(pools.calls) != 0 {
		t.Errorf("pve.parker_pool: \"\" is the opt-out and must reach PVE not at all; calls = %v",
			pools.calls)
	}
}

func TestPlaceParkersInPool_NilPoolServiceMakesNoCallAndDoesNotPanic(t *testing.T) {
	t.Parallel()

	// AssignVMToPool dereferences c.Pools() unguarded, and several fixtures
	// build a client without a pool service, so the sweep checks for one
	// before it starts.
	c := &sweepTestClient{nodes: &sweepNodesSvc{guests: map[int]string{90000: "bosh-parker"}}}

	if err := PlaceParkersInPool(context.Background(), c, nil, sweepNode, sweepCfg()); err != nil {
		t.Fatalf("a client without a pool service must be a quiet no-op: %v", err)
	}
}

func TestPlaceParkersInPool_FailedEnsureStillProcessesEveryParker(t *testing.T) {
	t.Parallel()

	// A pool the CPI cannot create may still exist, and a pool it cannot
	// create at all leaves the assignments to fail on their own. Either way
	// the sweep keeps going, because the caller's park has already succeeded
	// and nothing here may take it back.
	pools := newSweepPools()
	pools.createErr = errors.New("403 Pool.Allocate missing")
	c := sweepFixture(t, pools, map[int]string{
		90000: "bosh-parker;director--d1",
		90001: "bosh-parker;director--d1",
	})

	err := PlaceParkersInPool(context.Background(), c, nil, sweepNode, sweepCfg())
	if err == nil {
		t.Fatal("the returned error is what a test reads the failure from; want non-nil")
	}
	if got := countCalls(pools.calls, "add:"); got != 2 {
		t.Errorf("a failed ensure must still let both parkers be attempted; calls = %v", pools.calls)
	}
	if !pools.members[sweepPool][90000] || !pools.members[sweepPool][90001] {
		t.Errorf("both parkers must still land in %q, got %v", sweepPool, pools.members[sweepPool])
	}
}

func TestPlaceParkersInPool_FailedEnsureWarnsOnceForTheWholeSweep(t *testing.T) {
	t.Parallel()

	// A missing Pool.Allocate grant fails the ensure and then fails every
	// assignment after it for that one reason. One warning says that; a
	// warning per parker turns a single grant an operator has to add into a
	// wall of lines that scales with how many parkers the node happens to
	// hold. So the assignment failures drop to debug once the ensure has
	// warned.
	sink := &bytes.Buffer{}
	logger, err := log.NewLogger("debug", sink)
	if err != nil {
		t.Fatalf("build a logger: %v", err)
	}

	pools := newSweepPools()
	pools.createErr = errors.New("403 Pool.Allocate missing")
	pools.addErr = errors.New("403 Pool.Allocate missing")
	c := sweepFixture(t, pools, map[int]string{
		90000: "bosh-parker;director--d1",
		90001: "bosh-parker;director--d1",
		90002: "bosh-parker;director--d1",
	})

	_ = PlaceParkersInPool(context.Background(), c, logger, sweepNode, sweepCfg())

	logged := sink.String()
	if got := strings.Count(logged, "could not ensure the parker pool exists"); got != 1 {
		t.Errorf("the failed ensure logged %d times, want exactly 1", got)
	}
	if got := strings.Count(logged, "it stays where it is and the park itself is unaffected"); got != 0 {
		t.Errorf("the per-parker failures warned %d times after the ensure had already warned, want 0", got)
	}
	if got := strings.Count(logged, "for the reason the pool ensure above already reported"); got != 3 {
		t.Errorf("the per-parker failures logged at debug %d times, want one per parker, which is 3", got)
	}
}

func TestPlaceParkersInPool_FailedAssignLeavesTheOtherParkersProcessed(t *testing.T) {
	t.Parallel()

	pools := newSweepPools()
	failing := errors.New("403 Pool.Allocate missing on the parker pool")
	pools.addErr = failing
	c := sweepFixture(t, pools, map[int]string{
		90000: "bosh-parker;director--d1",
		90001: "bosh-parker;director--d1",
		90002: "bosh-parker;director--d1",
	})

	err := PlaceParkersInPool(context.Background(), c, nil, sweepNode, sweepCfg())
	if err == nil {
		t.Fatal("the returned error is what a test reads the failure from; want non-nil")
	}
	if got := countCalls(pools.calls, "add:"); got != 3 {
		t.Errorf("one failed assignment must not stop the others; calls = %v", pools.calls)
	}
}

func TestPlaceParkersInPool_ProbeFailureSkipsOnlyThatParker(t *testing.T) {
	t.Parallel()

	pools := newSweepPools()
	pools.probeErr = errors.New("403 Pool.Audit missing")
	c := sweepFixture(t, pools, map[int]string{90000: "bosh-parker;director--d1"})

	if err := PlaceParkersInPool(context.Background(), c, nil, sweepNode, sweepCfg()); err == nil {
		t.Fatal("a failed membership probe must be visible in the returned error")
	}
	if got := countCalls(pools.calls, "add:"); got != 0 {
		t.Errorf("a parker whose membership we could not read must not be assigned blind; calls = %v",
			pools.calls)
	}
}

func TestPlaceParkersInPool_StaysInsideItsBound(t *testing.T) {
	t.Parallel()

	// Two things are under test. The sweep hands every pool call a deadline no
	// further out than the bound, and a pool call that blocks ends with the
	// context rather than running on. The parent deadline here is far shorter
	// than the bound, so the blocked call resolves in milliseconds while the
	// deadline assertion still reads the real constant.
	blocked := make(chan struct{})
	defer close(blocked)

	pools := newSweepPools()
	pools.block = blocked
	c := sweepFixture(t, pools, map[int]string{90000: "bosh-parker;director--d1"})

	parent, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	started := time.Now()
	_ = PlaceParkersInPool(parent, c, nil, sweepNode, sweepCfg())
	elapsed := time.Since(started)

	if elapsed >= parkerPoolSweepTimeout {
		t.Errorf("the sweep ran for %s, which reaches its %s bound; a blocked pool call must end with the context",
			elapsed, parkerPoolSweepTimeout)
	}
	if len(pools.deadlines) == 0 {
		t.Fatal("no pool call saw a deadline; the sweep must bound its own context")
	}
	ceiling := started.Add(parkerPoolSweepTimeout)
	for _, deadline := range pools.deadlines {
		if deadline.After(ceiling) {
			t.Errorf("a pool call saw a deadline %s past the %s bound",
				deadline.Sub(ceiling), parkerPoolSweepTimeout)
		}
	}
}

func TestPlaceParkersInPool_NeverReadsTheClusterIndex(t *testing.T) {
	t.Parallel()

	// The banned cluster service in sweepFixture fails the test on any read,
	// so this case is the whole assertion. A sweep that reaches its goal state
	// does it from the node listing and PoolHasVM alone.
	pools := newSweepPools()
	pools.members[sweepPool] = map[int64]bool{90001: true}
	c := sweepFixture(t, pools, map[int]string{
		90000: "bosh-parker;director--d1",
		90001: "bosh-parker;director--d1",
	})

	if err := PlaceParkersInPool(context.Background(), c, nil, sweepNode, sweepCfg()); err != nil {
		t.Fatalf("PlaceParkersInPool: unexpected error: %v", err)
	}
	if got := countCalls(pools.calls, "has:"); got != 2 {
		t.Errorf("membership must be read once per parker through PoolHasVM; calls = %v", pools.calls)
	}
}

func TestPlaceParkersInPool_SkipsAMover(t *testing.T) {
	t.Parallel()

	// A mover carries the parker tag so that every parker guard fires for it,
	// and ListParkersForNode drops it so no park ever targets a migration
	// vehicle. The sweep reads its candidates from that same listing, so a
	// mover is invisible here by construction rather than by a rule of its own.
	pools := newSweepPools()
	c := sweepFixture(t, pools, map[int]string{
		90000: "bosh-parker;bosh-disk-mover;director--d1",
	})

	if err := PlaceParkersInPool(context.Background(), c, nil, sweepNode, sweepCfg()); err != nil {
		t.Fatalf("PlaceParkersInPool: unexpected error: %v", err)
	}
	if got := countCalls(pools.calls, "add:"); got != 0 {
		t.Errorf("a mover must never join the parker pool; calls = %v", pools.calls)
	}
}
