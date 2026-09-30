package pve

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
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
	// normalize, when set, is what the store keeps for a created comment, for
	// a PVE that does not keep a comment byte for byte.
	normalize func(comment string) string
	// deleteHook, when set, sees every delete with its context and the
	// comment the store held at that moment.
	deleteHook func(ctx context.Context, id, stored string)
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
	if f.normalize != nil {
		comment = f.normalize(comment)
	}
	f.pools[poolID] = comment
	return nil
}

func (f *fakeLockPools) DeletePool(ctx context.Context, poolID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteN++
	f.calls = append(f.calls, "delete:"+poolID)
	if f.deleteHook != nil {
		f.deleteHook(ctx, poolID, f.pools[poolID])
	}
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
	if f.createN != 1 || f.getN != 1 {
		t.Fatalf("free acquire should create once and read its claim back once; create=%d get=%d", f.createN, f.getN)
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
	// Steal = first create fails (dup) -> get holder -> re-read it right
	// before the delete -> delete -> recreate -> post-steal get confirming our
	// owner token won. This acquire takes no grace, so it confirms once.
	want := []string{
		"create:bosh-lock-web", "get:bosh-lock-web", "get:bosh-lock-web", "delete:bosh-lock-web",
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
	h, err := tryStealExpired(context.Background(), f, "bosh-lock-web", "me", 60*time.Second, time.Unix(1000, 0), clusterLockSettings{}, clk)
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
	h, err := tryStealExpired(context.Background(), f, "bosh-lock-web", "me", 60*time.Second, time.Unix(1000, 0), clusterLockSettings{}, clk)
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
	h, err := tryStealExpired(context.Background(), f, "bosh-lock-web", "me", 60*time.Second, time.Unix(1000, 0), clusterLockSettings{}, clk)
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
		if getCalls == 3 {
			// The first two reads judge the holder and re-read it before the
			// delete. The third is the post-steal verify read.
			return "", false, fmt.Errorf("500 pmxcfs read timeout"), true
		}
		return "", false, nil, false // first call: fall through to normal map lookup
	}

	clk := fixedClock(time.Unix(1000, 0), time.Second)
	h, err := tryStealExpired(context.Background(), f, "bosh-lock-web", "me", 60*time.Second, time.Unix(1000, 0), clusterLockSettings{}, clk)
	if h != nil {
		t.Fatal("expected nil handle when the post-steal verify read errors")
	}
	if err == nil {
		t.Fatal("expected an error when the post-steal verify read fails")
	}
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Errorf("verify-read failure must be retriable; got %v", err)
	}
	if !strings.Contains(err.Error(), "could not confirm who holds lock") || !errors.Is(err, ErrClusterLockStateUnknown) {
		t.Errorf("error should say the lock state is unknown; got %v", err)
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
	h, err := tryStealExpired(context.Background(), f, "bosh-lock-web", "me", 60*time.Second, time.Unix(1000, 0), clusterLockSettings{}, clk)
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

// PoolHasVM reports no membership; tests that exercise the
// disambiguation supply their own fake.
func (f *fakeLockPools) PoolHasVM(context.Context, string, int64) (bool, error) {
	return false, nil
}

// TestProcessLockOwner_IsUniquePerAcquisition pins the invariant a guarded
// sentinel create relies on: two acquirers never write the same claim, even
// in one process and one second, and the token never contains the space the
// sentinel comment is split on.
func TestProcessLockOwner_IsUniquePerAcquisition(t *testing.T) {
	first := ProcessLockOwner("unpark/90000")
	second := ProcessLockOwner("unpark/90000")
	if first == second {
		t.Fatalf("two acquisitions share the owner token %q", first)
	}
	host, _ := os.Hostname()
	identity := "@" + processLockIdentity() + "-"
	if !strings.HasPrefix(identity, "@"+lockOwnerHost(host)+fmt.Sprintf("/%d-", os.Getpid())) {
		t.Fatalf("process identity %q does not name this host and pid", identity)
	}
	for _, owner := range []string{first, second} {
		if !strings.HasPrefix(owner, "unpark/90000"+identity) || strings.ContainsAny(owner, " \t\n") {
			t.Fatalf("owner token %q does not name the caller and this process", owner)
		}
		got, ok := decodeLockOwner(encodeLockComment(owner, time.Now()))
		if !ok || got != owner {
			t.Fatalf("owner token %q does not survive the sentinel comment: %q", owner, got)
		}
	}
}

// TestAcquireClusterLock_DisplacedCreateReturnsNoHandle covers a create that
// succeeds and is then displaced before the acquirer acts. A stealer that read
// a crashed holder's expired claim deletes our fresh sentinel and recreates its
// own. The acquire must read its claim back, see the stealer's, and wait rather
// than hand back a lock the stealer also holds.
func TestAcquireClusterLock_DisplacedCreateReturnsNoHandle(t *testing.T) {
	f := newFakeLockPools()
	stealer := encodeLockComment("stealer@7-1", time.Unix(1000, 0).Add(time.Hour))
	displaced := false
	f.getFn = func(poolID string) (string, bool, error, bool) {
		if !displaced {
			displaced = true
			f.pools[poolID] = stealer
		}
		return "", false, nil, false
	}
	clk := fixedClock(time.Unix(1000, 0), time.Second)
	h, err := acquireClusterLockWithClock(context.Background(), f, "vm-90000", "waiter@9-1",
		time.Minute, 5*time.Second, clk)
	if h != nil {
		t.Fatalf("a displaced create returned a lock handle while %q holds the sentinel", f.pools["bosh-lock-vm-90000"])
	}
	if !errors.Is(err, ErrClusterLockTimeout) {
		t.Fatalf("want a lock timeout behind the stealer, got %v", err)
	}
	if f.pools["bosh-lock-vm-90000"] != stealer {
		t.Fatalf("the stealer's claim was disturbed: %q", f.pools["bosh-lock-vm-90000"])
	}
}

// TestLockOwnerHost keeps the host part of an owner token free of spaces and
// bounded, so the token survives the sentinel comment's encoding.
func TestLockOwnerHost(t *testing.T) {
	for host, want := range map[string]string{
		"bosh-director.example": "bosh-director.example",
		"has space\tand tab":    "has-space-and-tab",
		"":                      "unknown-host",
		strings.Repeat("a", 90): strings.Repeat("a", lockOwnerHostLimit),
	} {
		if got := lockOwnerHost(host); got != want {
			t.Errorf("lockOwnerHost(%q) = %q, want %q", host, got, want)
		}
	}
}

// TestRelease_LeavesAClaimNearItsExpiry covers the release margin. Release
// reads the claim and then deletes, and a steal can fall between the two once
// our claim has lapsed. So a claim that has expired, or will expire within the
// steal budget plus a round trip, is left for the TTL steal instead of being
// deleted. A claim outside the margin is still deleted.
func TestRelease_LeavesAClaimNearItsExpiry(t *testing.T) {
	t.Parallel()
	expiry := time.Unix(5000, 0)
	for name, tc := range map[string]struct {
		now        time.Time
		wantDelete bool
	}{
		"expired":                          {now: expiry.Add(time.Second)},
		"at its expiry":                    {now: expiry},
		"inside the budget and round trip": {now: expiry.Add(-2 * time.Second)},
		"outside the margin":               {now: expiry.Add(-time.Minute), wantDelete: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFakeLockPools()
			f.pools["bosh-lock-web"] = encodeLockComment("me", expiry)
			now := tc.now
			h := &ClusterLockHandle{
				pool: "bosh-lock-web", owner: "me", pools: f,
				expiry: expiry, now: func() time.Time { return now },
			}
			if err := h.Release(context.Background()); err != nil {
				t.Fatalf("Release: %v", err)
			}
			_, standing := f.pools["bosh-lock-web"]
			if tc.wantDelete && (standing || f.deleteN != 1) {
				t.Fatalf("a claim outside the margin must be deleted; deletes=%d standing=%v", f.deleteN, standing)
			}
			if !tc.wantDelete && (!standing || f.deleteN != 0) {
				t.Fatalf("a claim within the margin of its expiry was deleted; deletes=%d", f.deleteN)
			}
			if !h.released {
				t.Error("the handle should latch released either way")
			}
		})
	}
}

// TestRelease_GraceWidensTheMargin pins the margin for a lock that takes the
// grace. A stealer's create waits out the grace before it trusts its claim, so
// the margin adds it. Four seconds before expiry is inside the margin with the
// grace and outside it without.
func TestRelease_GraceWidensTheMargin(t *testing.T) {
	defer SetClusterLockGraceForTest(clusterLockDefaultGrace)()
	expiry := time.Unix(5000, 0)
	now := expiry.Add(-4 * time.Second)
	for name, settings := range map[string]clusterLockSettings{"with the grace": {grace: true}, "without it": {}} {
		f := newFakeLockPools()
		f.pools["bosh-lock-web"] = encodeLockComment("me", expiry)
		h := &ClusterLockHandle{
			pool: "bosh-lock-web", owner: "me", pools: f, settings: settings,
			expiry: expiry, now: func() time.Time { return now },
		}
		if err := h.Release(context.Background()); err != nil {
			t.Fatalf("%s: Release: %v", name, err)
		}
		if deleted := f.deleteN == 1; deleted == settings.grace {
			t.Errorf("%s: deleted=%v four seconds before expiry", name, deleted)
		}
	}
}

// TestRelease_NeverDeletesAClaimItCannotParse covers a sentinel whose comment
// names no owner or no expiry. Release cannot tell whose it is, so it leaves
// it. Acquirers treat such a comment as expired and steal it, so leaving it
// costs nothing, while deleting it could remove a claim that is not ours.
func TestRelease_NeverDeletesAClaimItCannotParse(t *testing.T) {
	t.Parallel()
	for name, comment := range map[string]string{
		"no owner":    "exp=99999999999",
		"no expiry":   "owner=me ",
		"not a claim": "operator notes",
		"empty":       "",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFakeLockPools()
			f.pools["bosh-lock-web"] = comment
			now := time.Unix(2000, 0)
			h := &ClusterLockHandle{
				pool: "bosh-lock-web", owner: "me", pools: f,
				expiry: time.Unix(9000, 0), now: func() time.Time { return now },
			}
			if err := h.Release(context.Background()); err != nil {
				t.Fatalf("Release: %v", err)
			}
			if f.deleteN != 0 {
				t.Fatalf("Release deleted a sentinel whose comment %q it cannot parse", comment)
			}
		})
	}
}

// TestAcquireClusterLock_HandleKeepsTheClaimPVEReturned covers a PVE that does
// not return a comment byte for byte as it was sent. The handle keeps the
// claim its confirming read returned, because a guarded delete compares the
// expected claim with another read, and two reads agree where a read and our
// own copy might not.
func TestAcquireClusterLock_HandleKeepsTheClaimPVEReturned(t *testing.T) {
	t.Parallel()
	f := newFakeLockPools()
	var sent string
	f.createFn = func(_, comment string) error {
		sent = comment
		return nil
	}
	f.getFn = func(id string) (string, bool, error, bool) {
		if c, ok := f.pools[id]; ok {
			return c + "\n", true, nil, true
		}
		return "", false, nil, false
	}
	h, err := acquireClusterLockWithClock(context.Background(), f, "web", "me", time.Minute, time.Second,
		fixedClock(time.Unix(1000, 0), time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if h.claim != sent+"\n" {
		t.Fatalf("handle claim = %q, want the comment PVE returned %q", h.claim, sent+"\n")
	}
}

// TestAcquireClusterLock_RequestDeadlineEndsTheWait covers a request whose
// deadline lands inside the lock wait. The wait ends at that deadline less
// clusterLockContextMargin, which leaves the caller time to release and close
// out its request, and it ends with ErrClusterLockTimeout rather than being cut
// short by the deadline itself. The request's deadline is set on the test
// clock's time line.
func TestAcquireClusterLock_RequestDeadlineEndsTheWait(t *testing.T) {
	t.Parallel()
	start := time.Unix(1000, 0)
	f := newFakeLockPools()
	f.pools["bosh-lock-web"] = encodeLockComment("holder", start.Add(time.Hour))
	clk, _ := sleepLog(start)
	ctx, cancel := context.WithDeadline(context.Background(), start.Add(40*time.Second))
	defer cancel()
	h, err := acquireClusterLockWithClock(ctx, f, "web", "me", time.Minute, 3*time.Minute, clk)
	if h != nil || !errors.Is(err, ErrClusterLockTimeout) || errors.Is(err, ErrClusterLockInterrupted) {
		t.Fatalf("want the lock timeout, got handle=%v err=%v", h != nil, err)
	}
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("the timeout must be retriable: %v", err)
	}
	if got, want := clk.now(), start.Add(40*time.Second-clusterLockContextMargin); !got.Equal(want) {
		t.Fatalf("the wait ended at %v, want the request's deadline less the margin, %v", got, want)
	}
}

// TestAcquireClusterLock_TooLittleTimeLeftCreatesNothing covers a request
// that starts with less than clusterLockContextMargin before its deadline. It
// gets the lock timeout at once and creates no sentinel, because it could not
// release one and close out in the time left.
func TestAcquireClusterLock_TooLittleTimeLeftCreatesNothing(t *testing.T) {
	t.Parallel()
	start := time.Unix(1000, 0)
	f := newFakeLockPools()
	clk, _ := sleepLog(start)
	ctx, cancel := context.WithDeadline(context.Background(), start.Add(clusterLockContextMargin-time.Second))
	defer cancel()
	h, err := acquireClusterLockWithClock(ctx, f, "web", "me", time.Minute, 3*time.Minute, clk)
	if h != nil || !errors.Is(err, ErrClusterLockTimeout) {
		t.Fatalf("want the lock timeout, got handle=%v err=%v", h != nil, err)
	}
	if f.createN != 0 || len(f.pools) != 0 {
		t.Fatalf("an acquire with no time left created a sentinel: creates=%d pools=%v", f.createN, f.pools)
	}
}

// TestAcquireClusterLock_CancelledWaitIsInterrupted covers a plain
// cancellation, as a SIGTERM makes. The wait ends with
// ErrClusterLockInterrupted, which callers match with errors.Is, and not with
// the timeout, so the two stay apart.
func TestAcquireClusterLock_CancelledWaitIsInterrupted(t *testing.T) {
	t.Parallel()
	start := time.Unix(1000, 0)
	f := newFakeLockPools()
	f.pools["bosh-lock-web"] = encodeLockComment("holder", start.Add(time.Hour))
	clk := lockClock{
		now:   func() time.Time { return start },
		sleep: func(context.Context, time.Duration) error { return context.Canceled },
	}
	h, err := acquireClusterLockWithClock(context.Background(), f, "web", "me", time.Minute, 3*time.Minute, clk)
	if h != nil || !errors.Is(err, ErrClusterLockInterrupted) || errors.Is(err, ErrClusterLockTimeout) {
		t.Fatalf("want the interrupted wait, got handle=%v err=%v", h != nil, err)
	}
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) || !errors.Is(err, context.Canceled) {
		t.Fatalf("the interrupted wait must be retriable and keep the cancellation: %v", err)
	}
}

// TestProcessLockOwner_RoundTripsAnyCallerToken covers a caller token that
// carries whitespace. The sentinel comment is split on whitespace, so such a
// token used to decode as only its first part, and every release then read
// its own claim as someone else's and left it standing for a whole TTL. The
// owner now decodes back to itself, and a release proves the claim ours and
// deletes it.
func TestProcessLockOwner_RoundTripsAnyCallerToken(t *testing.T) {
	t.Parallel()
	for name, token := range map[string]string{
		"space":           "set vm metadata/4242",
		"tab":             "unpark\t90000",
		"newline":         "park/90000\n",
		"no-break space":  "aa\u00a0web",
		"line separator":  "aa\u2028web",
		"only whitespace": " \t\n",
		"empty":           "",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			owner := ProcessLockOwner(token)
			expiry := time.Unix(5000, 0)
			comment := encodeLockComment(owner, expiry)
			if got, ok := decodeLockOwner(comment); !ok || got != owner {
				t.Errorf("owner %q decoded as %q (ok=%t) from %q", owner, got, ok, comment)
			}
			f := newFakeLockPools()
			f.pools["bosh-lock-web"] = comment
			now := time.Unix(1000, 0)
			h := &ClusterLockHandle{
				pool: "bosh-lock-web", owner: owner, pools: f,
				expiry: expiry, now: func() time.Time { return now },
			}
			if err := h.Release(context.Background()); err != nil {
				t.Fatalf("Release: %v", err)
			}
			if f.deleteN != 1 {
				t.Fatalf("Release did not prove the claim %q ours and delete it", comment)
			}
		})
	}
}

// TestProcessLockOwner_KeepsSafeTokens pins that a token with no whitespace
// passes through byte for byte, so the owners today's callers produce do not
// change, and that an empty token gets a readable fallback.
func TestProcessLockOwner_KeepsSafeTokens(t *testing.T) {
	t.Parallel()
	for _, token := range []string{"set_vm_metadata/4242", "unpark/90000", "aa-web/101", "caf\u00e9/1", "bad\xffbyte"} {
		if got := ProcessLockOwner(token); !strings.HasPrefix(got, token+"@") {
			t.Errorf("ProcessLockOwner(%q) = %q, want the token kept as it is", token, got)
		}
	}
	if got := ProcessLockOwner(""); !strings.HasPrefix(got, "unnamed@") {
		t.Errorf("ProcessLockOwner(\"\") = %q, want the unnamed fallback", got)
	}
}

// TestRelease_MarksItsOwnClaim pins which sentinel deletes carry the own-claim
// mark a poisoned allocation guard honors. A release of our handle carries it
// with the claim our confirming read returned. A steal's delete of someone
// else's expired claim does not.
func TestRelease_MarksItsOwnClaim(t *testing.T) {
	t.Parallel()
	f := newFakeLockPools()
	var marked []string
	f.deleteHook = func(ctx context.Context, _, stored string) {
		if claim, own := OwnLockClaim(ctx); own {
			if claim != stored {
				t.Errorf("own claim %q does not match the sentinel's %q", claim, stored)
			}
			marked = append(marked, "own")
			return
		}
		marked = append(marked, "not own")
	}
	start := time.Unix(1000, 0)
	f.pools["bosh-lock-web"] = encodeLockComment("crashed", start.Add(-time.Minute))
	h, err := acquireClusterLockWithClock(context.Background(), f, "web", "me", time.Minute, 5*time.Second,
		fixedClock(start, time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Join(marked, ",") != "not own,own" {
		t.Fatalf("delete marks = %v, want the steal unmarked and the release marked", marked)
	}
}
