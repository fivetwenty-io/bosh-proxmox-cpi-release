package pve

import (
	"context"
	"errors"
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
	createFn func(id, comment string) error // optional override; may mutate f.pools directly to simulate a concurrent stealer
	deleteFn func(id string) error          // optional override; non-nil error short-circuits the default delete
	// getFn, when set, is consulted on every GetPoolComment call. When override is
	// true its (comment, found, err) triple is returned as-is instead of the
	// default map lookup, letting a test script per-call-count behavior (e.g. the
	// steal's initial read succeeds but the post-steal verify read fails/shows a
	// different owner).
	getFn func(id string) (comment string, found bool, err error, override bool)
}

func newFakeLockPools() *fakeLockPools {
	return &fakeLockPools{pools: map[string]string{}}
}

func (f *fakeLockPools) AddVM(_ context.Context, _ string, _ int64) error        { return nil }
func (f *fakeLockPools) MoveVMToPool(_ context.Context, _ string, _ int64) error { return nil }

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
	if f.deleteFn != nil {
		if err := f.deleteFn(poolID); err != nil {
			return err
		}
	}
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
	if f.getFn != nil {
		if comment, found, err, override := f.getFn(poolID); override {
			return comment, found, err
		}
	}
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
		"web":           "bosh-lock-web",
		"aa-web":        "bosh-lock-aa-web",
		"cf/diego_cell": "bosh-lock-cf-diego_cell",
		"a b.c":         "bosh-lock-a-b-c",
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
	if h.pool != "bosh-lock-web" {
		t.Fatalf("pool = %q; want bosh-lock-web", h.pool)
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
	// The sentinel is the contract withParkerProtectionLock branches on: a
	// timeout means a live holder was inside the window throughout, so the
	// caller must NOT proceed unserialized the way it does for every other
	// acquire failure. A timeout that stops carrying it silently reopens the
	// unlocked-window path under contention.
	if !errors.Is(err, ErrClusterLockTimeout) {
		t.Errorf("timeout error must carry ErrClusterLockTimeout; got %v", err)
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
	if h.pool != "bosh-lock-web" {
		t.Fatalf("pool = %q; want bosh-lock-web", h.pool)
	}
	// Steal = first create fails (dup) -> get holder -> delete -> recreate ->
	// post-steal get confirming our owner token won.
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

// The following tests exercise tryStealExpired directly (it is unexported, and
// this test file is in package pve) so each of the five documented steal-race
// branches can be driven precisely without relying on the wrapping acquire
// loop's retry/backoff timing.

func TestTryStealExpired_PoolVanishedBeforeRead(t *testing.T) {
	// Branch: the sentinel pool is gone by the time tryStealExpired reads it
	// (another process already released/stole it). No delete/recreate should be
	// attempted; the caller must retry the top-level create instead.
	f := newFakeLockPools()
	clk := fixedClock(time.Unix(1000, 0), time.Second)
	h, err := tryStealExpired(context.Background(), f, "bosh-lock-web", "me", 60*time.Second, clk)
	if err != nil {
		t.Fatalf("expected no error when pool vanished before read, got %v", err)
	}
	if h != nil {
		t.Fatal("expected nil handle when pool vanished before the steal read")
	}
	if f.deleteN != 0 || f.createN != 0 {
		t.Errorf("no delete/create should occur once GetPoolComment reports absent; delete=%d create=%d",
			f.deleteN, f.createN)
	}
	if f.getN != 1 {
		t.Errorf("expected exactly one GetPoolComment call; got %d", f.getN)
	}
}

func TestTryStealExpired_DeleteNonNotFoundErrorRetriable(t *testing.T) {
	// Branch: DeletePool fails during the steal with an error that is NOT a
	// not-found (e.g. a transport/pmxcfs fault). This must propagate as a
	// retriable error, not be treated as "someone else already deleted it".
	f := newFakeLockPools()
	f.pools["bosh-lock-web"] = encodeLockComment("dead-owner", time.Unix(500, 0)) // expired at now=1000
	f.deleteFn = func(_ string) error { return fmt.Errorf("500 pmxcfs temporarily unavailable") }

	clk := fixedClock(time.Unix(1000, 0), time.Second)
	h, err := tryStealExpired(context.Background(), f, "bosh-lock-web", "me", 60*time.Second, clk)
	if h != nil {
		t.Fatal("expected nil handle on steal-delete failure")
	}
	if err == nil {
		t.Fatal("expected an error when steal-delete fails with a non-not-found error")
	}
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Errorf("steal-delete failure must be retriable; got %v", err)
	}
	if !strings.Contains(err.Error(), "steal-delete") {
		t.Errorf("error should identify the steal-delete step; got %v", err)
	}
	if f.createN != 0 {
		t.Errorf("recreate must not be attempted after a delete failure; createN=%d", f.createN)
	}
}

func TestTryStealExpired_RecreateLosesToConcurrentStealer(t *testing.T) {
	// Branch: our steal-delete succeeds, but the recreate CreatePool loses to a
	// concurrent stealer B who recreated the pool first (isPoolAlreadyExists).
	// This must signal loop-back (nil, nil), never a false-positive handle.
	f := newFakeLockPools()
	f.pools["bosh-lock-web"] = encodeLockComment("dead-owner", time.Unix(500, 0))
	f.createFn = func(id, _ string) error {
		// Simulate stealer B winning the recreate race between our DeletePool and
		// our CreatePool: B's entry appears in the pool map first, so the fake's
		// normal duplicate check (which runs after this hook) will reject us.
		f.pools[id] = encodeLockComment("stealer-b", time.Unix(999999, 0))
		return nil
	}

	clk := fixedClock(time.Unix(1000, 0), time.Second)
	h, err := tryStealExpired(context.Background(), f, "bosh-lock-web", "me", 60*time.Second, clk)
	if err != nil {
		t.Fatalf("losing the recreate race must signal loop/retry, not an error: %v", err)
	}
	if h != nil {
		t.Fatal("expected nil handle when a concurrent stealer wins the recreate")
	}
	if got := f.pools["bosh-lock-web"]; !strings.Contains(got, "owner=stealer-b") {
		t.Errorf("stealer B's comment should remain after we lose the race; got %q", got)
	}
}

func TestTryStealExpired_VerifyReadErrorRetriable(t *testing.T) {
	// Branch: the post-steal verification GetPoolComment call itself errors
	// (transport fault after a successful recreate). This must propagate as a
	// retriable error rather than either a false handle or a silent loop.
	f := newFakeLockPools()
	f.pools["bosh-lock-web"] = encodeLockComment("dead-owner", time.Unix(500, 0))
	getCalls := 0
	f.getFn = func(_ string) (string, bool, error, bool) {
		getCalls++
		if getCalls == 2 {
			// Second GetPoolComment call is the post-steal verify read.
			return "", false, fmt.Errorf("500 pmxcfs read timeout"), true
		}
		return "", false, nil, false // first call: fall through to normal map lookup
	}

	clk := fixedClock(time.Unix(1000, 0), time.Second)
	h, err := tryStealExpired(context.Background(), f, "bosh-lock-web", "me", 60*time.Second, clk)
	if h != nil {
		t.Fatal("expected nil handle when the post-steal verify read errors")
	}
	if err == nil {
		t.Fatal("expected an error when the post-steal verify read fails")
	}
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Errorf("verify-read failure must be retriable; got %v", err)
	}
	if !strings.Contains(err.Error(), "verify") {
		t.Errorf("error should identify the verify step; got %v", err)
	}
}

func TestTryStealExpired_VerifyShowsDifferentOwnerDisplaced(t *testing.T) {
	// Branch: the recreate succeeds, but the post-steal verify re-read shows a
	// DIFFERENT owner's comment — the exact residual race the doc comment on
	// tryStealExpired calls the correctness backstop. We must yield the handle
	// and signal loop/retry (nil, nil), never return a handle for an owner token
	// that is not actually persisted.
	f := newFakeLockPools()
	f.pools["bosh-lock-web"] = encodeLockComment("dead-owner", time.Unix(500, 0))
	getCalls := 0
	f.getFn = func(_ string) (string, bool, error, bool) {
		getCalls++
		if getCalls == 2 {
			return encodeLockComment("stealer-b", time.Unix(999999, 0)), true, nil, true
		}
		return "", false, nil, false
	}

	clk := fixedClock(time.Unix(1000, 0), time.Second)
	h, err := tryStealExpired(context.Background(), f, "bosh-lock-web", "me", 60*time.Second, clk)
	if err != nil {
		t.Fatalf("displacement by a concurrent stealer must signal loop/retry, not an error: %v", err)
	}
	if h != nil {
		t.Fatal("expected nil handle when the verify read shows a different owner (displaced)")
	}
}

func TestDefaultLockClock_NowReflectsWallClock(t *testing.T) {
	clk := defaultLockClock()
	before := time.Now().Add(-time.Second)
	got := clk.now()
	after := time.Now().Add(time.Second)
	if got.Before(before) || got.After(after) {
		t.Errorf("defaultLockClock().now() = %v; want within [%v,%v]", got, before, after)
	}
}

func TestDefaultLockClock_SleepReturnsAfterDuration(t *testing.T) {
	clk := defaultLockClock()
	start := time.Now()
	if err := clk.sleep(context.Background(), 10*time.Millisecond); err != nil {
		t.Fatalf("sleep: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 10*time.Millisecond {
		t.Errorf("sleep returned before its duration elapsed: %v", elapsed)
	}
}

func TestDefaultLockClock_SleepReturnsContextErrOnCancellation(t *testing.T) {
	clk := defaultLockClock()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := clk.sleep(ctx, time.Second)
	if err == nil {
		t.Fatal("expected sleep to return an error for an already-cancelled context")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled; got %v", err)
	}
}

// TestRelease_ExpiredClaimUnreadableComment_LeavesSentinel covers the one
// branch where Release must NOT fall through to the delete: the comment read
// failed AND this handle's own claim has lapsed. A lapsed claim is one a later
// acquirer is entitled to steal, so the sentinel standing there is more likely
// theirs -- deleting it would let a third caller in while they are mid-window.
func TestRelease_ExpiredClaimUnreadableComment_LeavesSentinel(t *testing.T) {
	t.Parallel()
	f := newFakeLockPools()
	f.pools["bosh-lock-web"] = encodeLockComment("stealer", time.Unix(9000, 0))
	f.getFn = func(string) (string, bool, error, bool) {
		return "", false, fmt.Errorf("transport down"), true
	}
	now := time.Unix(2000, 0)
	h := &ClusterLockHandle{
		pool:   "bosh-lock-web",
		owner:  "me",
		pools:  f,
		expiry: time.Unix(1000, 0), // lapsed relative to now
		now:    func() time.Time { return now },
	}
	if err := h.Release(context.Background()); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if f.deleteN != 0 {
		t.Errorf("expired handle with unreadable comment must not delete the sentinel; deletes=%d", f.deleteN)
	}
	if !h.released {
		t.Error("handle should latch released: its own claim is gone either way")
	}
	if _, ok := f.pools["bosh-lock-web"]; !ok {
		t.Error("the (probable stealer's) sentinel must survive")
	}
}

// TestRelease_LiveClaimUnreadableComment_StillDeletes pins the other half of
// the same branch: while this handle's claim is live, an unreadable comment
// falls through to the delete, since leaving the pool behind would block every
// acquire until the TTL lapses.
func TestRelease_LiveClaimUnreadableComment_StillDeletes(t *testing.T) {
	t.Parallel()
	f := newFakeLockPools()
	f.pools["bosh-lock-web"] = encodeLockComment("me", time.Unix(9000, 0))
	f.getFn = func(string) (string, bool, error, bool) {
		return "", false, fmt.Errorf("transport down"), true
	}
	now := time.Unix(2000, 0)
	h := &ClusterLockHandle{
		pool:   "bosh-lock-web",
		owner:  "me",
		pools:  f,
		expiry: time.Unix(3000, 0), // still live
		now:    func() time.Time { return now },
	}
	if err := h.Release(context.Background()); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if f.deleteN != 1 {
		t.Errorf("live handle must fall through to the delete; deletes=%d", f.deleteN)
	}
}

// TestRelease_CommentNamesAnotherOwner_LeavesSentinel: a readable comment that
// names somebody else is proof the lock was stolen; ours is already gone.
func TestRelease_CommentNamesAnotherOwner_LeavesSentinel(t *testing.T) {
	t.Parallel()
	f := newFakeLockPools()
	f.pools["bosh-lock-web"] = encodeLockComment("stealer", time.Unix(9000, 0))
	h := &ClusterLockHandle{pool: "bosh-lock-web", owner: "me", pools: f}
	if err := h.Release(context.Background()); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if f.deleteN != 0 {
		t.Errorf("stolen lock must not be deleted by the old owner; deletes=%d", f.deleteN)
	}
	if !h.released {
		t.Error("handle should latch released")
	}
}
