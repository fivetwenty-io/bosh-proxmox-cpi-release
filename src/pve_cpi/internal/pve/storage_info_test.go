package pve

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/clusterstorage"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
)

type fakeLister struct {
	entries []map[string]any
	err     error
	calls   int
}

func (f *fakeLister) ListStorage(_ context.Context, _ *clusterstorage.ListStorageParams) (*clusterstorage.ListStorageResponse, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	out := make(clusterstorage.ListStorageResponse, 0, len(f.entries))
	for _, e := range f.entries {
		raw, err := json.Marshal(e)
		if err != nil {
			return nil, err
		}
		out = append(out, raw)
	}
	return &out, nil
}

func TestStorageInfoCache_ClassifiesByType(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		storageRow map[string]any
		wantShared bool
	}{
		{"rbd", map[string]any{"storage": "ceph", "type": "rbd"}, true},
		{"cephfs", map[string]any{"storage": "cfs", "type": "cephfs"}, true},
		{"nfs", map[string]any{"storage": "nfs1", "type": "nfs"}, true},
		{"cifs", map[string]any{"storage": "cifs1", "type": "cifs"}, true},
		{"glusterfs", map[string]any{"storage": "gfs", "type": "glusterfs"}, true},
		{"pbs", map[string]any{"storage": "pbs1", "type": "pbs"}, true},
		{"lvm-not-shared", map[string]any{"storage": "vg0", "type": "lvm"}, false},
		{"lvmthin-not-shared", map[string]any{"storage": "lvtp", "type": "lvmthin"}, false},
		{"zfspool", map[string]any{"storage": "rpool", "type": "zfspool"}, false},
		{"dir-not-shared", map[string]any{"storage": "d1", "type": "dir"}, false},
		{"lvm-with-shared-flag", map[string]any{"storage": "vg0", "type": "lvm", "shared": 1}, true},
		{"dir-with-shared-flag", map[string]any{"storage": "d1", "type": "dir", "shared": 1}, true},
		{"nfs-shared-zero-still-shared-by-type", map[string]any{"storage": "n2", "type": "nfs", "shared": 0}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			lister := &fakeLister{entries: []map[string]any{c.storageRow}}
			cache := NewStorageInfoCache(lister, time.Minute)
			info, err := cache.Get(context.Background(), c.storageRow["storage"].(string))
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if got := info.IsShared(); got != c.wantShared {
				t.Fatalf("IsShared() = %v, want %v (info=%+v)", got, c.wantShared, info)
			}
		})
	}
}

func TestStorageInfoCache_ParsesNodesRestriction(t *testing.T) {
	t.Parallel()
	lister := &fakeLister{entries: []map[string]any{
		{"storage": "local-zfs", "type": "zfspool", "nodes": "pve-01,pve-02 , pve-03"},
	}}
	cache := NewStorageInfoCache(lister, time.Minute)
	info, err := cache.Get(context.Background(), "local-zfs")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	want := []string{"pve-01", "pve-02", "pve-03"}
	if len(info.Nodes) != len(want) {
		t.Fatalf("Nodes=%v, want %v", info.Nodes, want)
	}
	for i, n := range want {
		if info.Nodes[i] != n {
			t.Fatalf("Nodes[%d]=%q, want %q", i, info.Nodes[i], n)
		}
	}
}

func TestStorageInfoCache_CachesUntilTTL(t *testing.T) {
	t.Parallel()
	lister := &fakeLister{entries: []map[string]any{
		{"storage": "s1", "type": "rbd"},
	}}
	cache := NewStorageInfoCache(lister, time.Minute)
	for i := 0; i < 5; i++ {
		if _, err := cache.Get(context.Background(), "s1"); err != nil {
			t.Fatalf("Get #%d: %v", i, err)
		}
	}
	if lister.calls != 1 {
		t.Fatalf("lister.calls=%d, want 1 (subsequent reads should hit cache)", lister.calls)
	}
	cache.Invalidate("s1")
	if _, err := cache.Get(context.Background(), "s1"); err != nil {
		t.Fatalf("Get after invalidate: %v", err)
	}
	if lister.calls != 2 {
		t.Fatalf("lister.calls=%d, want 2 (invalidate should force refetch)", lister.calls)
	}
}

// TestStorageInfoCache_ZeroTTLDisablesPositiveCaching pins the documented
// ttl <= 0 contract: every Get must trigger a fetch. The inverted hit
// condition this regression-tests pinned the first /storage snapshot for the
// process lifetime, silently routing disk placement by stale data after a
// storage.cfg edit.
func TestStorageInfoCache_ZeroTTLDisablesPositiveCaching(t *testing.T) {
	t.Parallel()
	lister := &fakeLister{entries: []map[string]any{
		{"storage": "s1", "type": "rbd"},
	}}
	cache := NewStorageInfoCache(lister, 0)
	for i := 0; i < 3; i++ {
		if _, err := cache.Get(context.Background(), "s1"); err != nil {
			t.Fatalf("Get #%d: %v", i, err)
		}
	}
	if lister.calls != 3 {
		t.Fatalf("lister.calls=%d, want 3 (ttl<=0 must fetch on every Get)", lister.calls)
	}
}

func TestStorageInfoCache_MissingStorageReportsError(t *testing.T) {
	t.Parallel()
	lister := &fakeLister{entries: []map[string]any{
		{"storage": "ceph", "type": "rbd"},
	}}
	cache := NewStorageInfoCache(lister, time.Minute)
	_, err := cache.Get(context.Background(), "nope")
	if err == nil {
		t.Fatalf("Get for absent storage returned nil error")
	}
}

func TestStorageInfoCache_PropagatesListerError(t *testing.T) {
	t.Parallel()
	lister := &fakeLister{err: errors.New("boom")}
	cache := NewStorageInfoCache(lister, time.Minute)
	_, err := cache.Get(context.Background(), "anything")
	if err == nil {
		t.Fatalf("expected error from lister, got nil")
	}
}

// TestRefresh_NegativeCacheTTL confirms that a failed refresh is cached for
// negativeCacheTTL (5s): within the window the second Get returns the same
// error WITHOUT re-invoking the lister, preventing a thundering herd on a
// degraded PVE pool. After Invalidate the negative cache is cleared so the
// next Get retries the upstream.
func TestRefresh_NegativeCacheTTL(t *testing.T) {
	t.Parallel()
	lister := &fakeLister{err: errors.New("pve unreachable")}
	cache := NewStorageInfoCache(lister, time.Minute)

	// First Get triggers a refresh that fails; lister called once.
	_, err1 := cache.Get(context.Background(), "ceph")
	if err1 == nil {
		t.Fatal("expected error from first Get, got nil")
	}
	if lister.calls != 1 {
		t.Fatalf("lister.calls after first Get = %d, want 1", lister.calls)
	}

	// Second Get within the TTL replays the negative cache; lister NOT
	// called again. Issue several rapid Gets to confirm the herd-cap.
	for i := 0; i < 10; i++ {
		_, err := cache.Get(context.Background(), "ceph")
		if err == nil {
			t.Fatalf("Get #%d: expected cached error, got nil", i+2)
		}
	}
	if lister.calls != 1 {
		t.Errorf("lister.calls after 10 cached Gets = %d, want 1 (negative cache should suppress re-invocation)", lister.calls)
	}

	// Gets on a different name also share the negative cache because the
	// underlying /storage index failure applies to every key.
	_, err := cache.Get(context.Background(), "different-name")
	if err == nil {
		t.Fatal("expected cached error for different name, got nil")
	}
	if lister.calls != 1 {
		t.Errorf("lister.calls after different-name Get = %d, want 1 (shared neg cache)", lister.calls)
	}

	// Invalidate clears the negative cache; next Get re-invokes lister
	// (which still fails, but the call count rises).
	cache.Invalidate("ceph")
	_, err = cache.Get(context.Background(), "ceph")
	if err == nil {
		t.Fatal("expected lister-propagated error after Invalidate, got nil")
	}
	if lister.calls != 2 {
		t.Errorf("lister.calls after Invalidate+Get = %d, want 2 (Invalidate should clear neg cache)", lister.calls)
	}
}

// TestNegativeCacheTTL_Expires confirms that after the negative-cache window
// elapses, a new Get re-invokes the lister rather than indefinitely caching
// the failure. Uses WithNegativeCacheTTL and WithCacheClock to drive
// TTL expiry deterministically — no wall-clock sleep required.
func TestNegativeCacheTTL_Expires(t *testing.T) {
	t.Parallel()

	// fakeNow is a synthetic clock that the test advances manually.
	// The initial value is arbitrary; only relative advancement matters.
	fakeNow := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	var clockMu sync.Mutex
	clock := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return fakeNow
	}
	advanceClock := func(d time.Duration) {
		clockMu.Lock()
		defer clockMu.Unlock()
		fakeNow = fakeNow.Add(d)
	}

	const ttl = 5 * time.Millisecond
	lister := &fakeLister{err: errors.New("pve unreachable")}
	cache := NewStorageInfoCache(lister, time.Minute,
		WithNegativeCacheTTL(ttl),
		WithCacheClock(clock),
	)

	_, err := cache.Get(context.Background(), "ceph")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if lister.calls != 1 {
		t.Fatalf("calls=%d want 1", lister.calls)
	}

	// Advance the fake clock past the negative-cache TTL so the next Get
	// sees an expired window and re-invokes the lister.
	advanceClock(ttl + time.Nanosecond)

	_, err = cache.Get(context.Background(), "ceph")
	if err == nil {
		t.Fatal("expected error on retry after TTL expiry, got nil")
	}
	if lister.calls != 2 {
		t.Errorf("lister.calls after TTL expiry = %d, want 2", lister.calls)
	}
}

func TestBackendResolver_FallsBackToLocalOnLookupMiss(t *testing.T) {
	t.Parallel()
	lister := &fakeLister{entries: []map[string]any{}}
	cache := NewStorageInfoCache(lister, time.Minute)
	r := NewBackendResolver(nil, cache, "pve-default")
	b, err := r.Resolve(context.Background(), "unknown-storage")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if b.Kind() != BackendLocal {
		t.Fatalf("Kind()=%s, want local (safe default on miss)", b.Kind())
	}
}

func TestBackendResolver_PicksSharedForCephRBD(t *testing.T) {
	t.Parallel()
	lister := &fakeLister{entries: []map[string]any{
		{"storage": "ceph", "type": "rbd"},
	}}
	cache := NewStorageInfoCache(lister, time.Minute)
	r := NewBackendResolver(nil, cache, "pve-default")
	b, err := r.Resolve(context.Background(), "ceph")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if b.Kind() != BackendShared {
		t.Fatalf("Kind()=%s, want shared", b.Kind())
	}
}

func TestBackendResolver_PicksLocalForLVMThin(t *testing.T) {
	t.Parallel()
	lister := &fakeLister{entries: []map[string]any{
		{"storage": "local-lvm", "type": "lvmthin"},
	}}
	cache := NewStorageInfoCache(lister, time.Minute)
	r := NewBackendResolver(nil, cache, "pve-default")
	b, err := r.Resolve(context.Background(), "local-lvm")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if b.Kind() != BackendLocal {
		t.Fatalf("Kind()=%s, want local", b.Kind())
	}
}

// TestBackendResolver_TransientListerError_PropagatesRetriable confirms that
// a transient StorageInfoCache.Get failure (refresh's lister call hit a
// connection blip, e.g. pvedaemon worker recycling mid-refresh) is propagated
// as a retriable error rather than being silently folded into the "storage:
// local, unclassified" safe-default fallback. Falling through here would
// change a shared storage's placement algorithm (cloud_properties.node ->
// vmHint -> default) to local's (vmHint -> cloud_properties.node -> default)
// on a condition the Director could clear by retrying, instead of a bounded
// retry via RetriableCloudError.
func TestBackendResolver_TransientListerError_PropagatesRetriable(t *testing.T) {
	t.Parallel()
	lister := &fakeLister{err: &sdkerrors.ConnectionError{
		Host:    "pve.example",
		Port:    8006,
		Message: "transient blip",
	}}
	cache := NewStorageInfoCache(lister, time.Minute)
	r := NewBackendResolver(nil, cache, "pve-default")

	_, err := r.Resolve(context.Background(), "ceph")
	if err == nil {
		t.Fatalf("expected retriable error propagated from Resolve, got nil")
	}

	var ce *cpierrors.Error
	if !errors.As(err, &ce) {
		t.Fatalf("error does not carry cpierrors classification (type=%T): %v", err, err)
	}
	if !ce.OkToRetry() {
		t.Fatalf("expected OkToRetry()=true for transient lister failure; err=%v", err)
	}
}

func TestStaticResolver_AlwaysShared(t *testing.T) {
	t.Parallel()
	r := NewStaticBackendResolver(nil, "pve-x")
	b, err := r.Resolve(context.Background(), "anything")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if b.Kind() != BackendShared {
		t.Fatalf("Kind()=%s, want shared", b.Kind())
	}
	got, err := b.NodeForExisting(context.Background(), "vol")
	if err != nil {
		t.Fatalf("NodeForExisting: %v", err)
	}
	if got != "pve-x" {
		t.Fatalf("NodeForExisting=%q, want pve-x", got)
	}
}

// TestStaticBackend_NodeForCreate_DispatcherPaths covers staticBackend's
// NodeForCreate dispatch: cloudPropNode takes priority over defaultNode,
// defaultNode is used as fallback, and an error surfaces when neither is set.
// vmHint is intentionally ignored by the static backend (unlike shared/local),
// so it is passed a non-empty value throughout to confirm it has no effect.
func TestStaticBackend_NodeForCreate_DispatcherPaths(t *testing.T) {
	t.Parallel()

	t.Run("prefers cloudPropNode over defaultNode", func(t *testing.T) {
		t.Parallel()
		r := NewStaticBackendResolver(nil, "pve-default")
		b, err := r.Resolve(context.Background(), "anything")
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		got, err := b.NodeForCreate(context.Background(), "100", "pve-explicit")
		if err != nil {
			t.Fatalf("NodeForCreate: %v", err)
		}
		if got != "pve-explicit" {
			t.Fatalf("got %q, want pve-explicit", got)
		}
	})

	t.Run("falls back to defaultNode", func(t *testing.T) {
		t.Parallel()
		r := NewStaticBackendResolver(nil, "pve-default")
		b, err := r.Resolve(context.Background(), "anything")
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		got, err := b.NodeForCreate(context.Background(), "100", "")
		if err != nil {
			t.Fatalf("NodeForCreate: %v", err)
		}
		if got != "pve-default" {
			t.Fatalf("got %q, want pve-default", got)
		}
	})

	t.Run("errors when nothing resolves", func(t *testing.T) {
		t.Parallel()
		r := NewStaticBackendResolver(nil, "")
		b, err := r.Resolve(context.Background(), "anything")
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if _, err := b.NodeForCreate(context.Background(), "100", ""); err == nil {
			t.Fatalf("expected error when neither cloudPropNode nor defaultNode set")
		}
	})
}

// atomicLister is a StorageLister that counts calls with an atomic counter so
// the count is race-detector-safe when used from concurrent goroutines.
type atomicLister struct {
	calls   atomic.Int64
	entries []map[string]any
	// delay, when non-nil, is called before returning so tests can inject
	// artificial latency that forces all concurrent goroutines to observe a
	// cache miss before any one of them completes the refresh.
	delay func()
}

func (a *atomicLister) ListStorage(_ context.Context, _ *clusterstorage.ListStorageParams) (*clusterstorage.ListStorageResponse, error) {
	a.calls.Add(1)
	if a.delay != nil {
		a.delay()
	}
	out := make(clusterstorage.ListStorageResponse, 0, len(a.entries))
	for _, e := range a.entries {
		raw, err := json.Marshal(e)
		if err != nil {
			return nil, err
		}
		out = append(out, raw)
	}
	return &out, nil
}

// TestStorageInfoCache_ConcurrentGetCoalesces verifies the B5 fix: 100
// goroutines that all observe a cold-cache miss for the same key must trigger
// exactly 1 refresh call, not 100. The atomicLister injects a brief pause
// inside ListStorage so that all 100 goroutines reach the miss path before
// any refresh completes, maximising the probability of observing the bug if
// the fix is absent.
func TestStorageInfoCache_ConcurrentGetCoalesces(t *testing.T) {
	t.Parallel()
	const goroutines = 100

	// allStarted is closed after all Get goroutines have been launched.
	// The delay function blocks inside ListStorage until allStarted is closed,
	// ensuring the maximum number of goroutines observe a cold-cache miss
	// before the refresh completes.
	allStarted := make(chan struct{})

	lister := &atomicLister{
		entries: []map[string]any{
			{"storage": "ceph", "type": "rbd"},
		},
		delay: func() {
			select {
			case <-allStarted:
				// All goroutines have been launched; proceed with refresh.
			case <-time.After(10 * time.Second):
				// Safety valve: do not hang indefinitely if the test is broken.
			}
		},
	}

	cache := NewStorageInfoCache(lister, time.Minute)

	errs := make(chan error, goroutines)
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := cache.Get(context.Background(), "ceph")
			if err != nil {
				errs <- err
			}
		}()
	}
	// Signal that all goroutines have been spawned; the delay in ListStorage
	// unblocks once this channel is closed.
	close(allStarted)
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("Get error: %v", err)
	}

	got := lister.calls.Load()
	if got != 1 {
		t.Errorf("expected exactly 1 ListStorage call for %d concurrent misses, got %d (TOCTOU double-refresh race)", goroutines, got)
	}
}

// fakeClusterStorageService implements clusterstorage.Service with a
// configurable ListStorage; every other method panics if called, since
// ClusterStorageAsLister only needs ListStorage.
type fakeClusterStorageService struct {
	clusterstorage.Service
	listFn func(ctx context.Context, params *clusterstorage.ListStorageParams) (*clusterstorage.ListStorageResponse, error)
}

func (f *fakeClusterStorageService) ListStorage(ctx context.Context, params *clusterstorage.ListStorageParams) (*clusterstorage.ListStorageResponse, error) {
	return f.listFn(ctx, params)
}

// TestClusterStorageAsLister_DelegatesToService verifies the adapter passes
// ctx/params through to the wrapped clusterstorage.Service and returns its
// response unmodified.
func TestClusterStorageAsLister_DelegatesToService(t *testing.T) {
	t.Parallel()

	want := clusterstorage.ListStorageResponse{json.RawMessage(`{"storage":"local-lvm","type":"lvmthin"}`)}
	var gotParams *clusterstorage.ListStorageParams
	svc := &fakeClusterStorageService{
		listFn: func(_ context.Context, params *clusterstorage.ListStorageParams) (*clusterstorage.ListStorageResponse, error) {
			gotParams = params
			return &want, nil
		},
	}

	lister := ClusterStorageAsLister(svc)
	callParams := &clusterstorage.ListStorageParams{}
	resp, err := lister.ListStorage(context.Background(), callParams)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp != &want {
		t.Errorf("ListStorage response = %v, want the exact pointer returned by the wrapped service", resp)
	}
	if gotParams != callParams {
		t.Error("params passed to the wrapped service must be the exact pointer given to lister.ListStorage")
	}
}

// TestClusterStorageAsLister_PropagatesError verifies the adapter does not
// swallow an error from the wrapped service.
func TestClusterStorageAsLister_PropagatesError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("cluster storage list failed")
	svc := &fakeClusterStorageService{
		listFn: func(context.Context, *clusterstorage.ListStorageParams) (*clusterstorage.ListStorageResponse, error) {
			return nil, wantErr
		},
	}

	lister := ClusterStorageAsLister(svc)
	resp, err := lister.ListStorage(context.Background(), &clusterstorage.ListStorageParams{})
	if !errors.Is(err, wantErr) {
		t.Errorf("ListStorage error = %v, want %v", err, wantErr)
	}
	if resp != nil {
		t.Errorf("ListStorage response = %v, want nil on error", resp)
	}
}
