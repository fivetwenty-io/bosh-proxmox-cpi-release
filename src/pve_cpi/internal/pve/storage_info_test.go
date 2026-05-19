package pve

import (
	"context"
	"encoding/json"
	"errors"
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
	lister := &fakeLister{err: errors.New("boom")}
	cache := NewStorageInfoCache(lister, time.Minute)
	_, err := cache.Get(context.Background(), "anything")
	if err == nil {
		t.Fatalf("expected error from lister, got nil")
	}
}

func TestBackendResolver_FallsBackToLocalOnLookupMiss(t *testing.T) {
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
