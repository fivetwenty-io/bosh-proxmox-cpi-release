// Storage classification cache for the persistent-disk backend abstraction.
// StorageInfo distills the PVE /storage record into the two fields the CPI
// needs at the backend layer: whether the storage is shared across the cluster
// and which nodes (if restricted) can host volumes on it.
package pve

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/clusterstorage"

	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
)

// StorageInfo is the CPI-facing view of a PVE storage entry.
type StorageInfo struct {
	Name   string
	Type   string
	Shared bool
	Nodes  []string
}

// IsShared classifies the storage as "shared" (cluster-visible) or "local"
// (single-node). PVE marks rbd/cephfs/nfs/cifs/glusterfs/pbs storages as shared
// by definition; lvm/lvmthin/zfspool/dir/btrfs may be explicitly flagged shared
// via storage.cfg even though they are inherently local — both signals count.
func (s StorageInfo) IsShared() bool {
	if s.Shared {
		return true
	}
	switch strings.ToLower(s.Type) {
	case StorageTypeRBD, StorageTypeCephFS, StorageTypeNFS, StorageTypeCIFS, StorageTypeGlusterFS, StorageTypePBS:
		return true
	}
	return false
}

// StorageLister is the slice of the PVE SDK we depend on to classify storages.
// Implemented in production by *clusterstorage.service from the pve-apiclient-go
// SDK; tests can substitute a fake.
type StorageLister interface {
	ListStorage(ctx context.Context, params *clusterstorage.ListStorageParams) (*clusterstorage.ListStorageResponse, error)
}

// StorageInfoCache holds short-lived StorageInfo lookups so the backend
// abstraction does not flood the PVE Director with /storage requests during
// bursts of disk activity.
//
// The inflight map coalesces concurrent misses on the same key: the first
// goroutine to observe a miss creates a channel, releases the mutex, performs
// the refresh, then closes the channel. All subsequent goroutines that arrive
// while the refresh is in flight wait on that channel and re-read the cache
// once it closes. This eliminates the TOCTOU double-refresh race (B5).
//
// A short-lived negative cache mirrors the positive cache: when a refresh
// returns an error the failure is cached for negativeCacheTTL before the
// next attempt is allowed. This stops a degraded PVE cluster from being
// hammered by every concurrent CPI request on the same lookup key while
// the underlying fault clears (thundering-herd guard).
type StorageInfoCache struct {
	lister StorageLister
	ttl    time.Duration
	// negTTL is the window during which a failed refresh is replayed
	// from the negative cache instead of being re-attempted. Mirrors the
	// package default but per-instance so tests can inject a sub-millisecond
	// value without touching a global.
	negTTL time.Duration

	mu       sync.Mutex
	entries  map[string]storageInfoEntry
	inflight map[string]chan struct{}
	negCache negativeCacheEntry
}

type storageInfoEntry struct {
	info StorageInfo
	exp  time.Time
}

// negativeCacheEntry holds the most-recent refresh failure with an expiry.
// One entry covers the entire cache because refresh fetches the full index
// in a single call: a failure to fetch the index applies to every key.
type negativeCacheEntry struct {
	err error
	exp time.Time
}

// negativeCacheTTL is the window during which a failed refresh is replayed
// from the negative cache instead of being re-attempted. 5 seconds is short
// enough that a transient PVE blip clears before any human-noticeable
// extra latency, long enough that a thundering herd of CPI requests on
// the same cold cache only triggers one upstream call per window.
//
// Declared as a var (not const) so tests can swap the value via t.Cleanup
// to exercise TTL expiry without sleeping 5+ seconds in CI.
var negativeCacheTTL = 5 * time.Second

// StorageInfoCacheOption configures a StorageInfoCache at construction time.
type StorageInfoCacheOption func(*StorageInfoCache)

// WithNegativeCacheTTL overrides the negative-cache window. Use 0 to disable
// negative caching (every failed refresh is retried immediately). Tests use
// this to drive TTL-expiry behaviour without sleeping the production default.
func WithNegativeCacheTTL(d time.Duration) StorageInfoCacheOption {
	return func(c *StorageInfoCache) {
		c.negTTL = d
	}
}

// NewStorageInfoCache constructs a cache backed by lister. ttl <= 0 disables
// positive caching (every Get triggers a fetch). The negative-cache window
// defaults to negativeCacheTTL and may be overridden via WithNegativeCacheTTL.
func NewStorageInfoCache(lister StorageLister, ttl time.Duration, opts ...StorageInfoCacheOption) *StorageInfoCache {
	c := &StorageInfoCache{
		lister:   lister,
		ttl:      ttl,
		negTTL:   negativeCacheTTL,
		entries:  make(map[string]storageInfoEntry),
		inflight: make(map[string]chan struct{}),
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Get returns the StorageInfo for the named storage. The first call (or a call
// after expiry) fetches the full /storage index and populates the cache.
//
// Concurrent misses on the same key are coalesced: only one goroutine performs
// the refresh; all others wait on an inflight channel and re-read the cache
// once the refresh completes. This prevents the TOCTOU double-refresh race
// where two goroutines both observe a miss and both call refresh concurrently.
//
// A storage not present in the index is reported as a non-nil error. Callers
// that want "treat-as-local" behavior on lookup miss should handle explicitly.
//
// Inputs and failure modes:
//   - name "" → error before any lock or fetch.
//   - ctx cancelled while waiting on inflight → returns ctx.Err() wrapped.
//   - lister error → refresh goroutine returns the error; waiters receive it
//     because the cache is not populated and they attempt their own refresh
//     via the same coalescing path (the inflight channel is deleted after
//     refresh returns regardless of success).
//   - storage absent from index → non-nil error after successful refresh.
func (c *StorageInfoCache) Get(ctx context.Context, name string) (StorageInfo, error) {
	if name == "" {
		return StorageInfo{}, fmt.Errorf("storage_info: name must not be empty")
	}

	for {
		c.mu.Lock()

		// Cache hit: valid entry exists and TTL not expired (or TTL disabled).
		if entry, ok := c.entries[name]; ok && (c.ttl <= 0 || time.Now().Before(entry.exp)) {
			c.mu.Unlock()
			return entry.info, nil
		}

		// Negative-cache hit: a recent refresh failed and the TTL has not
		// expired. Replay the cached error rather than hammering the upstream
		// while the failure mode is still in effect. Preserves the wrapped
		// retriable classification produced by refresh.
		if c.negCache.err != nil && time.Now().Before(c.negCache.exp) {
			cachedErr := c.negCache.err
			c.mu.Unlock()
			return StorageInfo{}, cachedErr
		}

		// Cache miss: check whether another goroutine is already refreshing.
		if ch, inflight := c.inflight[name]; inflight {
			// Another goroutine owns the refresh; wait for it to complete.
			c.mu.Unlock()
			select {
			case <-ch:
				// Refresh done; loop back to re-read the cache. The entry may
				// or may not be present (refresh could have failed), so we
				// re-enter the loop rather than reading unconditionally.
				continue
			case <-ctx.Done():
				return StorageInfo{}, fmt.Errorf("storage_info: context done while waiting for cache refresh of %q: %w", name, ctx.Err())
			}
		}

		// This goroutine is the refresh owner: publish the inflight channel
		// before releasing the lock so latecomers see it immediately.
		ch := make(chan struct{})
		c.inflight[name] = ch
		c.mu.Unlock()

		// Perform the refresh outside all locks. Other goroutines for this key
		// are parked on ch; goroutines for different keys proceed unobstructed.
		refreshErr := c.refresh(ctx)

		// Ordering invariant: refresh() writes the negative-cache entry under
		// c.mu before returning a non-nil error (see negCache writes in
		// refresh). Closing ch under the same mutex ensures that release pairs
		// with each waiter's c.mu acquire on re-entry, making the negCache
		// write visible before the re-read. Without this ordering, waiters
		// unblock on close(ch), find no valid entry and no negative-cache hit,
		// and each spawns its own refresh (thundering herd).
		if refreshErr != nil {
			c.mu.Lock()
			close(ch)
			delete(c.inflight, name)
			c.mu.Unlock()
			return StorageInfo{}, refreshErr
		}

		// Success path: close the channel and remove the sentinel.
		close(ch)
		c.mu.Lock()
		delete(c.inflight, name)
		c.mu.Unlock()

		// Read the result under lock; absent entry is a not-found error.
		c.mu.Lock()
		entry, ok := c.entries[name]
		c.mu.Unlock()
		if !ok {
			return StorageInfo{}, fmt.Errorf("storage_info: storage %q not present in PVE /storage index", name)
		}
		return entry.info, nil
	}
}

// Invalidate drops the cached entry for name and clears any negative-cache
// failure so the next Get refetches the index. Operators that have remedied
// a PVE outage can call this to force re-discovery without waiting out the
// negative-cache TTL.
func (c *StorageInfoCache) Invalidate(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, name)
	c.negCache = negativeCacheEntry{}
}

// refresh fetches /storage and replaces the cache wholesale. PVE returns the
// full index even when only one entry is needed, so populating all entries from
// a single request is strictly cheaper than per-name lookups.
//
// On error the returned value is classified through pve.WrapError so callers
// preserve the retriable/non-retriable distinction (transient transport
// faults remain retriable; 404 stays non-retriable). The same wrapped error
// is recorded in the negative cache so concurrent callers within
// negativeCacheTTL replay it instead of triggering parallel refreshes.
func (c *StorageInfoCache) refresh(ctx context.Context) error {
	if c.lister == nil {
		return fmt.Errorf("storage_info: cache has no lister configured")
	}
	resp, err := c.lister.ListStorage(ctx, &clusterstorage.ListStorageParams{})
	if err != nil {
		wrapped := cpierrors.Wrap(WrapError(err), "storage_info: list cluster storage")
		c.mu.Lock()
		c.negCache = negativeCacheEntry{err: wrapped, exp: time.Now().Add(c.negTTL)}
		c.mu.Unlock()
		return wrapped
	}
	if resp == nil {
		wrapped := cpierrors.Cloud("storage_info: nil response from cluster storage list")
		c.mu.Lock()
		c.negCache = negativeCacheEntry{err: wrapped, exp: time.Now().Add(c.negTTL)}
		c.mu.Unlock()
		return wrapped
	}

	now := time.Now()
	deadline := now.Add(c.ttl)

	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = make(map[string]storageInfoEntry, len(*resp))
	for _, raw := range *resp {
		info, perr := parseStorageEntry(raw)
		if perr != nil {
			continue
		}
		c.entries[info.Name] = storageInfoEntry{info: info, exp: deadline}
	}
	// Successful refresh clears the negative cache.
	c.negCache = negativeCacheEntry{}
	return nil
}

// parseStorageEntry decodes the relevant fields from a /storage response item.
// PVE returns shared as a 0/1 integer; nodes is comma-joined when present.
func parseStorageEntry(raw json.RawMessage) (StorageInfo, error) {
	var v struct {
		Storage string `json:"storage"`
		Type    string `json:"type"`
		Shared  *int   `json:"shared,omitempty"`
		Nodes   string `json:"nodes,omitempty"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return StorageInfo{}, err
	}
	if v.Storage == "" {
		return StorageInfo{}, fmt.Errorf("storage_info: entry missing storage name")
	}

	info := StorageInfo{
		Name: v.Storage,
		Type: v.Type,
	}
	if v.Shared != nil && *v.Shared != 0 {
		info.Shared = true
	}
	if v.Nodes != "" {
		for _, part := range strings.Split(v.Nodes, ",") {
			part = strings.TrimSpace(part)
			if part != "" {
				info.Nodes = append(info.Nodes, part)
			}
		}
	}
	return info, nil
}

// ClusterStorageAsLister adapts a clusterstorage.Service to StorageLister.
// clusterstorage.Service already satisfies StorageLister directly (it exposes
// ListStorage), so this is a convenience alias for production callers.
//
// Usage: pve.NewStorageInfoCache(client.ClusterStorage(), ttl)
func ClusterStorageAsLister(svc clusterstorage.Service) StorageLister {
	return svc
}
