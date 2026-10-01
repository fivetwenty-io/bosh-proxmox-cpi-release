// Internal tests for the cross-process cluster lock and read-after-write verify
// applied to the anti-affinity HA-rule read-modify-write.
package handlers

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
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

// TestEnsureAntiAffinity_LockPool_TakesTheGrace covers the anti-affinity
// lock's grace. Two holders at once would place two instances of a group on
// one node, so after its create the lock reads its claim back, waits out the
// grace, and reads it again before the read-modify-write starts.
func TestEnsureAntiAffinity_LockPool_TakesTheGrace(t *testing.T) {
	defer pve.SetClusterLockGraceForTest(20 * time.Millisecond)()
	events := []string{}
	stub := newAAStub()
	stub.events = &events
	stub.listResourcesFn = aaResourcesFn(aaQEMU(100, "job--web"))
	pools := newAALockPools(&events)
	started := time.Now()
	if err := ensureAntiAffinityMembership(context.Background(), aaDepsLock(aaLockConfig("pool", false, 30), stub, pools), "web", 101, log.NewNopLogger()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"lock-create:bosh-lock-aa-web", "lock-get:bosh-lock-aa-web", "lock-get:bosh-lock-aa-web"}
	if len(events) <= len(want) || strings.Join(events[:len(want)], ",") != strings.Join(want, ",") || strings.HasPrefix(events[len(want)], "lock-") {
		t.Fatalf("the acquire must confirm its claim twice before the RMW; events %v", events)
	}
	if elapsed := time.Since(started); elapsed < 20*time.Millisecond {
		t.Fatalf("the RMW finished after %v, inside the grace", elapsed)
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
	// stays deterministic with no real sleep. It reads the sentinel once on its
	// way out, because the create may have landed, and finds nothing to delete.
	// The held-live → wait → timeout path is covered against a fake clock in the
	// internal/pve cluster-lock tests.
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

// PoolHasVM reports no membership; tests that exercise the
// disambiguation supply their own fake.
func (p *aaLockPools) PoolHasVM(context.Context, string, int64) (bool, error) {
	return false, nil
}

// aaLockClaimExpiry reads the expiry a lock comment records.
func aaLockClaimExpiry(t *testing.T, comment string) int64 {
	t.Helper()
	for _, field := range strings.Fields(comment) {
		if v, ok := strings.CutPrefix(field, "exp="); ok {
			exp, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				t.Fatalf("claim %q has an unreadable expiry: %v", comment, err)
			}
			return exp
		}
	}
	t.Fatalf("claim %q records no expiry", comment)
	return 0
}

// TestAntiAffinityLockTTLHasAFloor reads the expiry that the anti-affinity
// lock's claim records. The claim lasts twice cluster_lock_timeout_sec, and
// never less than 30 seconds, so the grace and the release margin always fit
// inside it with time left for the read-modify-write.
func TestAntiAffinityLockTTLHasAFloor(t *testing.T) {
	for _, tc := range []struct {
		sec int
		ttl time.Duration
	}{
		{1, 30 * time.Second},
		{3, 30 * time.Second},
		{14, 30 * time.Second},
		{15, 30 * time.Second},
		{60, 120 * time.Second},
	} {
		t.Run(fmt.Sprintf("cluster_lock_timeout_sec=%d", tc.sec), func(t *testing.T) {
			pools := newAALockPools(nil)
			deps := aaDepsLock(aaLockConfig("pool", false, tc.sec), newAAStub(), pools)
			before := time.Now()
			handle, err := acquireAntiAffinityLock(t.Context(), deps, "web", 101)
			after := time.Now()
			if err != nil {
				t.Fatalf("acquire: %v", err)
			}
			t.Cleanup(func() { _ = handle.Release(context.Background()) })
			exp := aaLockClaimExpiry(t, pools.pools["bosh-lock-aa-web"])
			if earliest, latest := before.Add(tc.ttl).Unix(), after.Add(tc.ttl).Unix(); exp < earliest || exp > latest {
				t.Fatalf("the claim expires %ds after the acquire started, want a TTL of %v", exp-before.Unix(), tc.ttl)
			}
		})
	}
}

// productionClusterLockGrace and productionReleaseMargin are the grace pause
// the anti-affinity lock takes outside tests and the margin a release then
// needs before the claim's expiry, which is the steal budget, that grace, and
// one round trip.
const (
	productionClusterLockGrace = 2500 * time.Millisecond
	productionReleaseMargin    = 2*time.Second + productionClusterLockGrace + 500*time.Millisecond
)

// TestAntiAffinityLockOutlastsTheGraceAtShortTimeouts covers the anti-affinity
// lock at small cluster_lock_timeout_sec values, where twice the setting used
// to be shorter than the grace plus the release margin. The test shortens the
// grace so it runs fast. What the claim has left when the acquire returns,
// less the grace this run didn't wait out, is what it would have left under
// the production grace, and that must still exceed the production release
// margin. A release right away must delete the sentinel, and at a setting of
// 1, a second request must not get the lock while the first holds it.
func TestAntiAffinityLockOutlastsTheGraceAtShortTimeouts(t *testing.T) {
	const grace = 20 * time.Millisecond
	t.Cleanup(pve.SetClusterLockGraceForTest(grace))
	sentinel := pve.ClusterLockPoolName(antiAffinityLockPrefix + "web")
	for _, sec := range []int{1, 2, 3, 4, 60} {
		t.Run(fmt.Sprintf("cluster_lock_timeout_sec=%d", sec), func(t *testing.T) {
			pools := newAALockPools(nil)
			deps := aaDepsLock(aaLockConfig("pool", false, sec), newAAStub(), pools)
			handle, err := acquireAntiAffinityLock(t.Context(), deps, "web", 101)
			if err != nil {
				t.Fatalf("first acquire: %v", err)
			}
			left := time.Until(handle.WindowDeadline(0)) - (productionClusterLockGrace - grace)
			if left <= productionReleaseMargin {
				t.Fatalf("under the production grace the claim would have %v left when the acquire returns, want more than %v",
					left.Round(time.Millisecond), productionReleaseMargin)
			}
			if sec == 1 {
				claim := pools.pools[sentinel]
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				// The second request stops at its first poll wait, once it
				// has found the sentinel taken and read the claim.
				pools.createErr = func(string) error { cancel(); return nil }
				second, err := acquireAntiAffinityLock(ctx, deps, "web", 102)
				pools.createErr = nil
				if second != nil || err == nil {
					t.Fatalf("a second request took the lock while the first held it: err=%v", err)
				}
				if pools.pools[sentinel] != claim {
					t.Fatalf("the second request replaced the first request's claim %q with %q", claim, pools.pools[sentinel])
				}
			}
			if err := handle.Release(t.Context()); err != nil {
				t.Fatalf("release: %v", err)
			}
			if _, standing := pools.pools[sentinel]; standing {
				t.Fatal("a release right after the acquire left the sentinel standing until its claim expired")
			}
		})
	}
}
