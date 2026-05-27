package pve

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/clusterstorage"
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
// the failure. Uses WithNegativeCacheTTL to inject a 1ms window per-instance
// so the test stays parallel-safe — no package-global mutation.
func TestNegativeCacheTTL_Expires(t *testing.T) {
	t.Parallel()

	lister := &fakeLister{err: errors.New("pve unreachable")}
	cache := NewStorageInfoCache(lister, time.Minute, WithNegativeCacheTTL(1*time.Millisecond))

	_, err := cache.Get(context.Background(), "ceph")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if lister.calls != 1 {
		t.Fatalf("calls=%d want 1", lister.calls)
	}

	// Wait out the per-instance TTL (1ms) plus slack so expiry is reliable.
	time.Sleep(10 * time.Millisecond)

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
