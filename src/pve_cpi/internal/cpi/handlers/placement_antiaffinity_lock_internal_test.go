// Internal tests for the cross-process cluster lock and read-after-write verify
// applied to the anti-affinity HA-rule read-modify-write.
package handlers

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
)

// aaLockPools is a fake PoolService backing the cluster lock, sharing an ordered
// event log with the cluster stub so acquire/release ordering relative to the
// RMW (list/create/delete rule) can be asserted.
type aaLockPools struct {
	pools  map[string]string // poolid -> comment
	events *[]string
	// createErr, when non-nil, lets a test force CreatePool to fail (e.g. a
	// permanently-held lock for the timeout case).
	createErr func(id string) error
}

func newAALockPools(events *[]string) *aaLockPools {
	p := &aaLockPools{pools: map[string]string{}, events: events}
	return p
}

func (p *aaLockPools) record(ev string) {
	if p.events != nil {
		*p.events = append(*p.events, ev)
	}
}

func (p *aaLockPools) AddVM(_ context.Context, _ string, _ int64) error        { return nil }
func (p *aaLockPools) MoveVMToPool(_ context.Context, _ string, _ int64) error { return nil }

func (p *aaLockPools) CreatePool(_ context.Context, poolID, comment string) error {
	p.record("lock-create:" + poolID)
	if p.createErr != nil {
		if err := p.createErr(poolID); err != nil {
			return err
		}
	}
	if _, ok := p.pools[poolID]; ok {
		return fmt.Errorf("pool '%s' already exists", poolID)
	}
	p.pools[poolID] = comment
	return nil
}

func (p *aaLockPools) DeletePool(_ context.Context, poolID string) error {
	p.record("lock-delete:" + poolID)
	if _, ok := p.pools[poolID]; !ok {
		return fmt.Errorf("pool '%s' does not exist", poolID)
	}
	delete(p.pools, poolID)
	return nil
}

func (p *aaLockPools) GetPoolComment(_ context.Context, poolID string) (string, bool, error) {
	p.record("lock-get:" + poolID)
	c, ok := p.pools[poolID]
	return c, ok, nil
}

// aaLockConfig returns a config with the requested lock mode and verify toggle.
func aaLockConfig(mode string, verify bool, timeoutSec int) *config.CPIConfig {
	c := icMinConfig()
	c.ClusterLock = mode
	c.ClusterLockTimeoutSec = timeoutSec
	if verify {
		v := true
		c.AntiAffinityVerify = &v
	}
	return c
}

// aaDepsLock builds Deps wiring both the cluster stub and the lock pool service
// onto a single fake client, with the given config.
func aaDepsLock(cfg *config.CPIConfig, stub *aaClusterStub, pools *aaLockPools) Deps {
	return Deps{
		Config: cfg,
		PVE: &icPVEClient{
			clusterSvc: stub,
			poolsSvc:   pools,
			nodesSvc: &icNodesService{listFn: func(ctx context.Context, p *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
				return stub.ListResources(ctx, p)
			}},
		},
		Agent:  &icAgentStub{},
		Logger: log.NewNopLogger(),
	}
}

// --------------------------------------------------------------------------
// mode=off + verify=off → zero new calls (golden RMW sequence unchanged).
// --------------------------------------------------------------------------

func TestEnsureAntiAffinity_LockOffVerifyOff_NoPoolCalls(t *testing.T) {
	events := []string{}
	stub := newAAStub()
	stub.events = &events
	stub.listResourcesFn = aaResourcesFn(aaQEMU(100, "job--web"))
	pools := newAALockPools(&events)

	cfg := aaLockConfig("off", false, 0)
	if err := ensureAntiAffinityMembership(context.Background(), aaDepsLock(cfg, stub, pools), "web", 101, log.NewNopLogger()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, ev := range events {
		if strings.HasPrefix(ev, "lock-") {
			t.Fatalf("mode=off must issue zero pool/lock calls; saw %q in %v", ev, events)
		}
	}
	if got := stub.rules["bosh-aa-web"]; got != "vm:100,vm:101" {
		t.Errorf("rule resources = %q; want vm:100,vm:101", got)
	}
}

// --------------------------------------------------------------------------
// mode=pool → acquire (CreatePool) BEFORE the first read, release after recreate.
// --------------------------------------------------------------------------

func TestEnsureAntiAffinity_LockPool_AcquireBeforeReadReleaseAfter(t *testing.T) {
	events := []string{}
	stub := newAAStub()
	stub.events = &events
	stub.listResourcesFn = aaResourcesFn(aaQEMU(100, "job--web"))
	pools := newAALockPools(&events)

	cfg := aaLockConfig("pool", false, 30)
	if err := ensureAntiAffinityMembership(context.Background(), aaDepsLock(cfg, stub, pools), "web", 101, log.NewNopLogger()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// First event must be the lock acquire; last must be the lock release.
	if len(events) == 0 || events[0] != "lock-create:bosh-lock-aa-web" {
		t.Fatalf("first op must be lock acquire; got %v", events)
	}
	if events[len(events)-1] != "lock-delete:bosh-lock-aa-web" {
		t.Fatalf("last op must be lock release; got %v", events)
	}
	// Acquire precedes the first cluster read (list-resources / list-rules).
	acquireIdx, firstReadIdx := -1, -1
	for i, ev := range events {
		if ev == "lock-create:bosh-lock-aa-web" && acquireIdx == -1 {
			acquireIdx = i
		}
		if (ev == "list-resources" || ev == "list-rules") && firstReadIdx == -1 {
			firstReadIdx = i
		}
	}
	if acquireIdx == -1 || firstReadIdx == -1 || acquireIdx >= firstReadIdx {
		t.Errorf("acquire(%d) must precede first read(%d); events=%v", acquireIdx, firstReadIdx, events)
	}
}

// --------------------------------------------------------------------------
// acquire fails (lock unobtainable) → retriable, and the RMW never runs.
// --------------------------------------------------------------------------

func TestEnsureAntiAffinity_LockPool_AcquireFailsRetriableNoRMW(t *testing.T) {
	events := []string{}
	stub := newAAStub()
	stub.events = &events
	pools := newAALockPools(&events)
	// A non-duplicate create failure (transport/pmxcfs fault) is classified
	// retriable immediately, so the acquire never enters its poll loop — the test
	// stays deterministic with no real sleep. The held-live → wait → timeout path
	// is covered against a fake clock in the internal/pve cluster-lock tests.
	pools.createErr = func(_ string) error { return fmt.Errorf("pmxcfs unavailable") }

	cfg := aaLockConfig("pool", false, 1)
	err := ensureAntiAffinityMembership(context.Background(), aaDepsLock(cfg, stub, pools), "web", 101, log.NewNopLogger())
	if err == nil {
		t.Fatal("expected retriable error when the lock cannot be acquired")
	}
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Errorf("lock-acquire failure must be retriable; got %v", err)
	}
	// No RMW must have run (rule never touched).
	if stub.createRuleCalls != 0 || stub.deleteRuleCalls != 0 {
		t.Errorf("RMW must not run while lock unacquired; create=%d delete=%d", stub.createRuleCalls, stub.deleteRuleCalls)
	}
}

// --------------------------------------------------------------------------
// acquire when lock held + expiry in the past → steals (delete+recreate) → proceeds.
// --------------------------------------------------------------------------

func TestEnsureAntiAffinity_LockPool_HeldExpiredSteals(t *testing.T) {
	events := []string{}
	stub := newAAStub()
	stub.events = &events
	stub.listResourcesFn = aaResourcesFn(aaQEMU(100, "job--web"))
	pools := newAALockPools(&events)
	// Held by a dead owner: expiry of 1 (epoch) is always in the past.
	pools.pools["bosh-lock-aa-web"] = encodeAALockComment("dead", 1)

	cfg := aaLockConfig("pool", false, 30)
	if err := ensureAntiAffinityMembership(context.Background(), aaDepsLock(cfg, stub, pools), "web", 101, log.NewNopLogger()); err != nil {
		t.Fatalf("steal acquire failed: %v", err)
	}
	// The steal must have happened: a lock-delete then a second lock-create before
	// any RMW read.
	joined := strings.Join(events, ",")
	if !strings.Contains(joined, "lock-get:bosh-lock-aa-web,lock-delete:bosh-lock-aa-web,lock-create:bosh-lock-aa-web") {
		t.Errorf("expected steal sequence get->delete->create; events=%v", events)
	}
	if got := stub.rules["bosh-aa-web"]; got != "vm:100,vm:101" {
		t.Errorf("RMW should still produce the rule after steal; got %q", got)
	}
}

// --------------------------------------------------------------------------
// release is deferred even when the RMW errors mid-way.
// --------------------------------------------------------------------------

func TestEnsureAntiAffinity_LockPool_ReleaseOnRMWError(t *testing.T) {
	events := []string{}
	stub := newAAStub()
	stub.events = &events
	stub.failListRules = true // RMW fails at the ListHaRules step
	stub.listResourcesFn = aaResourcesFn(aaQEMU(100, "job--web"))
	pools := newAALockPools(&events)

	cfg := aaLockConfig("pool", false, 30)
	err := ensureAntiAffinityMembership(context.Background(), aaDepsLock(cfg, stub, pools), "web", 101, log.NewNopLogger())
	if err == nil {
		t.Fatal("expected the RMW error to propagate")
	}
	// The lock must have been released despite the error.
	if events[len(events)-1] != "lock-delete:bosh-lock-aa-web" {
		t.Errorf("lock must be released on RMW error; last event=%q events=%v", events[len(events)-1], events)
	}
	if _, held := pools.pools["bosh-lock-aa-web"]; held {
		t.Error("sentinel pool should be gone after deferred release")
	}
}

// --------------------------------------------------------------------------
// verify on + member present → ok. verify on + member absent → retriable.
// --------------------------------------------------------------------------

func TestEnsureAntiAffinity_VerifyOn_MemberPresentOK(t *testing.T) {
	stub := newAAStub()
	stub.listResourcesFn = aaResourcesFn(aaQEMU(100, "job--web"))
	cfg := aaLockConfig("off", true, 0)
	pools := newAALockPools(nil)
	if err := ensureAntiAffinityMembership(context.Background(), aaDepsLock(cfg, stub, pools), "web", 101, log.NewNopLogger()); err != nil {
		t.Fatalf("verify should pass when member present: %v", err)
	}
}

func TestEnsureAntiAffinity_VerifyOn_MemberAbsentRetriable(t *testing.T) {
	stub := newAAStub()
	stub.listResourcesFn = aaResourcesFn(aaQEMU(100, "job--web"))
	// Simulate a concurrent writer dropping the new member from the recreated rule.
	stub.dropMemberOnRecreate = "vm:101"
	cfg := aaLockConfig("off", true, 0)
	pools := newAALockPools(nil)
	err := ensureAntiAffinityMembership(context.Background(), aaDepsLock(cfg, stub, pools), "web", 101, log.NewNopLogger())
	if err == nil {
		t.Fatal("expected a retriable error when the verify member is absent")
	}
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Errorf("verify failure must be retriable; got %v", err)
	}
}

// encodeAALockComment mirrors the lock comment format for test seeding without
// importing the internal pve helper (different package).
func encodeAALockComment(owner string, expUnix int64) string {
	return fmt.Sprintf("owner=%s exp=%d", owner, expUnix)
}
