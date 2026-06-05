package pve

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
)

// fakeLockPools is an in-memory PoolService recording the call sequence and
// holding pool comments, so cluster-lock tests can assert ordering and the
// create-or-fail contract without a live PVE.
type fakeLockPools struct {
	mu       sync.Mutex
	pools    map[string]string // poolid -> comment
	calls    []string          // ordered op log: "create:<id>", "delete:<id>", "get:<id>"
	createN  int
	deleteN  int
	getN     int
	createFn func(id, comment string) error // optional override
}

func newFakeLockPools() *fakeLockPools {
	return &fakeLockPools{pools: map[string]string{}}
}

func (f *fakeLockPools) AddVM(_ context.Context, _ string, _ int64) error { return nil }

func (f *fakeLockPools) CreatePool(_ context.Context, poolID, comment string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createN++
	f.calls = append(f.calls, "create:"+poolID)
	if f.createFn != nil {
		if err := f.createFn(poolID, comment); err != nil {
			return err
		}
	}
	if _, ok := f.pools[poolID]; ok {
		return fmt.Errorf("pool '%s' already exists", poolID)
	}
	f.pools[poolID] = comment
	return nil
}

func (f *fakeLockPools) DeletePool(_ context.Context, poolID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteN++
	f.calls = append(f.calls, "delete:"+poolID)
	if _, ok := f.pools[poolID]; !ok {
		return fmt.Errorf("pool '%s' does not exist", poolID)
	}
	delete(f.pools, poolID)
	return nil
}

func (f *fakeLockPools) GetPoolComment(_ context.Context, poolID string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getN++
	f.calls = append(f.calls, "get:"+poolID)
	c, ok := f.pools[poolID]
	return c, ok, nil
}

// fixedClock returns a lockClock whose now advances by step on each sleep, so
// the acquire loop reaches its deadline deterministically without real waits.
func fixedClock(start time.Time, step time.Duration) lockClock {
	cur := start
	return lockClock{
		now: func() time.Time { return cur },
		sleep: func(_ context.Context, _ time.Duration) error {
			cur = cur.Add(step)
			return nil
		},
	}
}

func TestClusterLockPoolName_SanitizesAndPrefixes(t *testing.T) {
	cases := map[string]string{
		"web":          "bosh-lock-web",
		"aa-web":       "bosh-lock-aa-web",
		"cf/diego_cell": "bosh-lock-cf-diego_cell",
		"a b.c":        "bosh-lock-a-b-c",
	}
	for in, want := range cases {
		if got := ClusterLockPoolName(in); got != want {
			t.Errorf("ClusterLockPoolName(%q) = %q; want %q", in, got, want)
		}
	}
}

func TestAcquireClusterLock_FreeCreatesPool(t *testing.T) {
	f := newFakeLockPools()
	clk := fixedClock(time.Unix(1000, 0), time.Second)
	h, err := acquireClusterLockWithClock(context.Background(), f, "web", "req-1/pid-9/100",
		60*time.Second, 30*time.Second, clk)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if h.PoolName() != "bosh-lock-web" {
		t.Fatalf("pool = %q; want bosh-lock-web", h.PoolName())
	}
	if f.createN != 1 || f.getN != 0 {
		t.Fatalf("free acquire should create once and never read; create=%d get=%d", f.createN, f.getN)
	}
	if _, ok := f.pools["bosh-lock-web"]; !ok {
		t.Fatal("sentinel pool not present after acquire")
	}
}

func TestReleaseClusterLock_DeletesAndIsIdempotent(t *testing.T) {
	f := newFakeLockPools()
	clk := fixedClock(time.Unix(1000, 0), time.Second)
	h, err := acquireClusterLockWithClock(context.Background(), f, "web", "owner-1",
		60*time.Second, 30*time.Second, clk)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if relErr := h.Release(context.Background()); relErr != nil {
		t.Fatalf("release: %v", relErr)
	}
	if _, ok := f.pools["bosh-lock-web"]; ok {
		t.Fatal("sentinel pool should be gone after release")
	}
	// Second release is a no-op (no extra delete).
	delsBefore := f.deleteN
	if relErr := h.Release(context.Background()); relErr != nil {
		t.Fatalf("second release: %v", relErr)
	}
	if f.deleteN != delsBefore {
		t.Errorf("second release issued a delete; deleteN went %d -> %d", delsBefore, f.deleteN)
	}
}

func TestAcquireClusterLock_HeldLiveOwnerTimesOutRetriable(t *testing.T) {
	f := newFakeLockPools()
	// Pre-seed the lock held by a live owner whose expiry is far in the future.
	f.pools["bosh-lock-web"] = encodeLockComment("other-owner", time.Unix(100000, 0))

	clk := fixedClock(time.Unix(1000, 0), 2*time.Second)
	_, err := acquireClusterLockWithClock(context.Background(), f, "web", "me",
		60*time.Second, 6*time.Second, clk)
	if err == nil {
		t.Fatal("expected a timeout error when lock held by a live owner")
	}
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Errorf("timeout error must be retriable; got %v", err)
	}
	if !strings.Contains(strings.ToLower(err.Error()), "timed out") {
		t.Errorf("error should mention timeout; got %v", err)
	}
}

func TestAcquireClusterLock_HeldExpiredOwnerSteals(t *testing.T) {
	f := newFakeLockPools()
	// Lock held by an owner whose expiry is in the past relative to now=1000.
	f.pools["bosh-lock-web"] = encodeLockComment("dead-owner", time.Unix(500, 0))

	clk := fixedClock(time.Unix(1000, 0), time.Second)
	h, err := acquireClusterLockWithClock(context.Background(), f, "web", "me",
		60*time.Second, 30*time.Second, clk)
	if err != nil {
		t.Fatalf("steal acquire: %v", err)
	}
	if h.PoolName() != "bosh-lock-web" {
		t.Fatalf("pool = %q; want bosh-lock-web", h.PoolName())
	}
	// Steal = first create fails (dup) -> get holder -> delete -> recreate ->
	// post-steal get confirming our owner token won (H3 mitigation).
	want := []string{
		"create:bosh-lock-web", "get:bosh-lock-web", "delete:bosh-lock-web",
		"create:bosh-lock-web", "get:bosh-lock-web",
	}
	if strings.Join(f.calls, ",") != strings.Join(want, ",") {
		t.Errorf("steal call sequence = %v; want %v", f.calls, want)
	}
	if got := f.pools["bosh-lock-web"]; !strings.Contains(got, "owner=me") {
		t.Errorf("stolen pool comment should record new owner; got %q", got)
	}
}

func TestAcquireClusterLock_MalformedCommentTreatedExpired(t *testing.T) {
	f := newFakeLockPools()
	f.pools["bosh-lock-web"] = "garbage-no-exp-field"
	clk := fixedClock(time.Unix(1000, 0), time.Second)
	h, err := acquireClusterLockWithClock(context.Background(), f, "web", "me",
		60*time.Second, 30*time.Second, clk)
	if err != nil {
		t.Fatalf("acquire over malformed comment: %v", err)
	}
	if h == nil {
		t.Fatal("expected a handle when stealing an unparseable holder")
	}
}

func TestAcquireClusterLock_NonDuplicateCreateErrorRetriable(t *testing.T) {
	f := newFakeLockPools()
	f.createFn = func(_, _ string) error { return fmt.Errorf("503 service unavailable") }
	clk := fixedClock(time.Unix(1000, 0), time.Second)
	_, err := acquireClusterLockWithClock(context.Background(), f, "web", "me",
		60*time.Second, 30*time.Second, clk)
	if err == nil {
		t.Fatal("expected error on non-duplicate create failure")
	}
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Errorf("transient create failure must be retriable; got %v", err)
	}
}

func TestAcquireClusterLock_RejectsBadInputs(t *testing.T) {
	f := newFakeLockPools()
	ctx := context.Background()
	if _, err := AcquireClusterLock(ctx, nil, "web", "o", time.Second, time.Second); err == nil {
		t.Error("nil pool service should be rejected")
	}
	if _, err := AcquireClusterLock(ctx, f, "web", "", time.Second, time.Second); err == nil {
		t.Error("empty owner should be rejected")
	}
	if _, err := AcquireClusterLock(ctx, f, "web", "o", 0, time.Second); err == nil {
		t.Error("non-positive ttl should be rejected")
	}
	if _, err := AcquireClusterLock(ctx, f, "web", "o", time.Second, 0); err == nil {
		t.Error("non-positive timeout should be rejected")
	}
}

func TestDecodeLockExpiry(t *testing.T) {
	exp := time.Unix(1717000000, 0)
	got, ok := decodeLockExpiry(encodeLockComment("me", exp))
	if !ok || !got.Equal(exp) {
		t.Fatalf("decodeLockExpiry round-trip = (%v,%v); want (%v,true)", got, ok, exp)
	}
	if _, ok := decodeLockExpiry("owner=me"); ok {
		t.Error("comment without exp= should not parse")
	}
}
