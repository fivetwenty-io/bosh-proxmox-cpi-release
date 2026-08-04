// Storage classification cache for the persistent-disk backend abstraction.
// StorageInfo distills the PVE /storage record into the two fields the CPI
// needs at the backend layer: whether the storage is shared across the cluster
// and which nodes (if restricted) can host volumes on it.
package pve

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/clusterstorage"

	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
)

// StorageInfo is the CPI-facing view of a PVE storage entry.
//
// Path/Server/Export carry the backing-identity fields PVE reports for
// file-content-capable storages: Path for dir-style plugins, Server+Export
// for nfs, Server+Export (populated from the "share" field) for cifs. They
// are used only by BackingKey — nothing else in this struct depends on them.
type StorageInfo struct {
	Name    string
	Type    string
	Shared  bool
	Nodes   []string
	Path    string
	Server  string
	Export  string
	Content string
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

// backingKeyNetFSScheme prefixes the normalized identity of an NFS storage.
// NFS mounts a remote export by server+path, so a storage ID configured
// against the same server and export is the same physical backing.
const backingKeyNetFSScheme = "nfs://"

// backingKeyCIFSScheme prefixes the normalized identity of a CIFS/SMB
// storage. It is deliberately distinct from backingKeyNetFSScheme: a CIFS
// share name is an entry in the SMB server's namespace, not a filesystem
// path, so a share name that happens to render identically to some NFS
// export's path is a coincidence, not evidence the two protocols are
// exposing the same bytes. Keeping the schemes separate means that
// coincidence can never merge them.
const backingKeyCIFSScheme = "cifs://"

// backingKeyDirScheme prefixes the normalized identity of a dir-plugin
// storage: two storage IDs pointing at the same local filesystem path AND
// the same restricted node set are the same backing directory. See the
// Nodes handling in BackingKey's dir case.
const backingKeyDirScheme = "dir://"

// backingKeyIDScheme prefixes the fallback identity used for every storage
// type this package does not know how to normalize (block-native backends —
// lvm, lvmthin, zfspool, rbd — plus cluster-service-backed types this package
// does not parse a location out of — cephfs, glusterfs, pbs, btrfs, and any
// unrecognised/future type). The key is just the storage ID itself, so it can
// never accidentally equal another storage's key unless the name is literally
// the same: correctness over cleverness — a wrong merge here would treat two
// genuinely distinct pools as one and mis-drive clone-mode, placement, or
// (once destroy paths use it) disk deletion.
const backingKeyIDScheme = "id://"

// BackingKey returns a normalized identity for the physical storage backing
// this entry, so two differently-named PVE storage IDs that point at the same
// physical location (Kevin's "two names, one export" scenario) can be
// recognized as sharing state.
//
//   - nfs: backingKeyNetFSScheme + lowercased Server + cleaned Export — e.g.
//     "nfs://10.0.0.5/tank/proxmox". Server is lowercased because DNS names
//     and most operators' IP literals are case-insensitive identity; the
//     export path is path.Clean-ed so a trailing slash or "//" typo does not
//     defeat the match.
//   - cifs: backingKeyCIFSScheme, normalized the same way as nfs but under
//     its own scheme — a CIFS share name and an NFS export path are never
//     compared to each other, only to other CIFS shares (see the scheme
//     doc comment).
//   - dir: backingKeyDirScheme + cleaned Path + a canonicalized Nodes suffix.
//     PVE's dir plugin is commonly registered once per node against an
//     identically-mounted local disk ("ssd-n1" on node n1, "ssd-n2" on node
//     n2, both /mnt/ssd) — those are DIFFERENT physical disks that merely
//     share a mount path, so the node set is part of the key: two dir
//     entries at the same path only produce the same key when their Nodes
//     sets are identical, INCLUDING both empty (PVE's "no nodes list"
//     meaning "available on every node" is one unambiguous visibility, so
//     two such entries at the same path are the same backing). Any other
//     relationship between the two Nodes sets — disjoint, overlapping, or
//     one empty and the other restricted — is conservatively treated as
//     NOT the same backing, even though the sets are not necessarily
//     disjoint; guessing "same" on partial node-set overlap risks the exact
//     misplacement this key exists to prevent.
//   - every other type (lvm, lvmthin, zfspool, rbd, cephfs, glusterfs, pbs,
//     btrfs, unknown/future types): backingKeyIDScheme + Name — never
//     normalized, so two distinct storage IDs of these types are NEVER
//     reported as sharing a backing even if they happen to wrap the same
//     underlying device (this package has no reliable way to tell, and
//     guessing wrong is worse than not merging).
//
// Returns "" when the entry carries no usable identity for its type (nfs/cifs
// missing Server or Export, dir missing Path, or Name empty for the fallback
// case). Callers MUST treat "" as "backing unknown, never a match" — see
// SameBacking.
func (s StorageInfo) BackingKey() string {
	switch strings.ToLower(s.Type) {
	case StorageTypeNFS:
		if s.Server == "" || s.Export == "" {
			return ""
		}
		return backingKeyNetFSScheme + strings.ToLower(s.Server) + path.Clean("/"+s.Export)
	case StorageTypeCIFS:
		if s.Server == "" || s.Export == "" {
			return ""
		}
		return backingKeyCIFSScheme + strings.ToLower(s.Server) + path.Clean("/"+s.Export)
	case StorageTypeDir:
		if s.Path == "" {
			return ""
		}
		return backingKeyDirScheme + path.Clean(s.Path) + "#nodes=" + canonicalNodeSet(s.Nodes)
	default:
		if s.Name == "" {
			return ""
		}
		return backingKeyIDScheme + s.Name
	}
}

// canonicalNodeSet returns a deterministic, order-independent representation
// of a storage entry's restricted-node list, for folding into BackingKey's
// dir case. An empty/nil nodes list (PVE's "no restriction, every node")
// canonicalizes to "" so two unrestricted dir entries at the same path
// compare equal; any non-empty list is sorted and comma-joined so the same
// set of nodes in a different order still canonicalizes identically.
func canonicalNodeSet(nodes []string) string {
	if len(nodes) == 0 {
		return ""
	}
	sorted := make([]string, len(nodes))
	copy(sorted, nodes)
	sort.Strings(sorted)
	return strings.Join(sorted, ",")
}

// SameBacking reports whether a and b are backed by the same physical storage
// location: equal, non-empty BackingKey values. An empty key on either side
// (undeterminable identity) never counts as a match — treating "unknown" as
// "same" would silently merge storages that might be entirely different, the
// opposite of the safety BackingKey exists to provide.
func SameBacking(a, b StorageInfo) bool {
	ka := a.BackingKey()
	if ka == "" {
		return false
	}
	return ka == b.BackingKey()
}

// SharedViaBacking classifies target as shared when either target.IsShared()
// is true on its own terms, OR target shares a BackingKey with some other
// entry in all whose own IsShared() is true.
//
// This closes a config-drift gap specific to storage types whose IsShared()
// depends on the storage.cfg "shared" flag rather than being fixed by type
// (dir/lvm/lvmthin/zfspool/btrfs — see the IsShared doc comment): an operator
// who registers the same network mount under two storage IDs and only
// remembers to flag one of them "shared: 1" gets an inconsistent answer from
// a plain per-ID IsShared() check even though both IDs resolve to the same
// bytes. Types whose IsShared() is fixed by type (nfs/cifs/rbd/cephfs/
// glusterfs/pbs) are unaffected — they are already shared on their own terms,
// so the "other entry" branch is only ever consulted for the drift-prone
// types, and even then only when BackingKey identifies a genuine physical
// match (dir path or nfs/cifs server+export) — never for the id:// fallback,
// which by construction only matches itself.
//
// target must be present in all (or have Name equal to some entry there) for
// the backing-propagation branch to find anything; a target absent from all
// simply falls back to its own IsShared().
func SharedViaBacking(target StorageInfo, all []StorageInfo) bool {
	if target.IsShared() {
		return true
	}
	key := target.BackingKey()
	if key == "" {
		return false
	}
	for i := range all {
		if all[i].Name == target.Name {
			continue
		}
		if all[i].BackingKey() == key && all[i].IsShared() {
			return true
		}
	}
	return false
}

// WarnDuplicateBackingStorages logs one Warn for every distinct BackingKey
// shared by two or more storage IDs in infos — e.g. two storage IDs
// configured against the same NFS export ("two names, one export"). Storage
// types that never normalize (BackingKey's id:// fallback) can never appear
// here, since that key is always unique to a single Name by construction.
//
// infos with an empty BackingKey (undeterminable identity) are skipped
// entirely — never grouped together, matching SameBacking's "unknown is
// never a match" contract.
func WarnDuplicateBackingStorages(ctx context.Context, infos []StorageInfo) {
	byKey := make(map[string][]string)
	for i := range infos {
		key := infos[i].BackingKey()
		if key == "" {
			continue
		}
		byKey[key] = append(byKey[key], infos[i].Name)
	}

	dupeKeys := make([]string, 0)
	for key, names := range byKey {
		if len(names) > 1 {
			dupeKeys = append(dupeKeys, key)
		}
	}
	if len(dupeKeys) == 0 {
		return
	}
	sort.Strings(dupeKeys) // deterministic log order

	logger := log.FromContext(ctx)
	for _, key := range dupeKeys {
		names := byKey[key]
		sort.Strings(names)
		logger.Warn("storage_info: two or more storage IDs share one physical backing — "+
			"two names, one export; prefer a single storage ID to avoid silent full-clone "+
			"downgrades, cross-cluster VMID collisions, and split placement decisions",
			log.String("backing", key),
			log.String("storage_ids", strings.Join(names, ",")),
		)
	}
}

// StorageLister is the slice of the PVE SDK we depend on to classify storages.
// In production it is satisfied by clusterstorage.Service from the
// pve-apiclient-go SDK (see ClusterStorageAsLister, which adapts one to the
// other); tests can substitute a fake.
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
	// clock returns the current time. Defaults to time.Now. Tests inject a
	// fake clock to drive TTL-expiry logic without wall-clock sleeps.
	clock func() time.Time

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
const negativeCacheTTL = 5 * time.Second

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

// WithCacheClock overrides the clock function used for TTL comparisons.
// The default is time.Now. Tests inject a fake clock to advance time
// deterministically without wall-clock sleeps.
//
// Production code MUST NOT call this.
func WithCacheClock(fn func() time.Time) StorageInfoCacheOption {
	return func(c *StorageInfoCache) {
		c.clock = fn
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
		clock:    time.Now,
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

		// Cache hit: valid entry exists, positive caching is enabled
		// (ttl > 0), and the entry has not expired. ttl <= 0 must MISS —
		// the constructor documents it as "every Get triggers a fetch";
		// short-circuiting to a permanent hit here would pin the first
		// /storage snapshot for the process lifetime and silently route
		// disk placement by stale data after a storage.cfg edit.
		if entry, ok := c.entries[name]; ok && c.ttl > 0 && c.clock().Before(entry.exp) {
			c.mu.Unlock()
			return entry.info, nil
		}

		// Negative-cache hit: a recent refresh failed and the TTL has not
		// expired. Replay the cached error rather than hammering the upstream
		// while the failure mode is still in effect. Preserves the wrapped
		// retriable classification produced by refresh.
		if c.negCache.err != nil && c.clock().Before(c.negCache.exp) {
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
		c.negCache = negativeCacheEntry{err: wrapped, exp: c.clock().Add(c.negTTL)}
		c.mu.Unlock()
		return wrapped
	}
	if resp == nil {
		wrapped := cpierrors.Cloud("storage_info: nil response from cluster storage list")
		c.mu.Lock()
		c.negCache = negativeCacheEntry{err: wrapped, exp: c.clock().Add(c.negTTL)}
		c.mu.Unlock()
		return wrapped
	}

	now := c.clock()
	deadline := now.Add(c.ttl)

	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = make(map[string]storageInfoEntry, len(*resp))
	for i, raw := range *resp {
		info, perr := parseStorageEntry(raw)
		if perr != nil {
			// Skip malformed entries: one bad element must not fail the whole
			// scan (see the package doc). Logged at Debug so schema drift in a
			// genuinely malformed PVE response leaves a diagnostic trail
			// instead of silently degrading to "storage absent from index".
			log.FromContext(ctx).Debug("storage_info: skipping malformed /storage entry",
				log.Int("index", i),
				log.Err(perr),
			)
			continue
		}
		c.entries[info.Name] = storageInfoEntry{info: info, exp: deadline}
	}
	// Successful refresh clears the negative cache.
	c.negCache = negativeCacheEntry{}

	// One-time duplicate-backing warning, gated process-wide rather than per
	// cache instance so this and create_stemcell's PolicyDeps adapter — the
	// only other path that decodes a full /storage index — cannot both warn
	// about the same storage.cfg (see duplicateBackingWarnOnce). Run under the
	// same c.mu already held for this refresh: it is pure logging over the
	// just-built entries, so it cannot deadlock or re-enter the cache.
	infos := make([]StorageInfo, 0, len(c.entries))
	for k := range c.entries {
		infos = append(infos, c.entries[k].info)
	}
	WarnDuplicateBackingStoragesOnce(ctx, infos)
	return nil
}

// parseStorageEntry decodes the relevant fields from a /storage response item.
// PVE returns shared as a 0/1 integer; nodes is comma-joined when present.
//
// Backing-identity fields per PVE storage plugin (pveStorage(5)): dir-style
// plugins report "path"; nfs reports "server"+"export"; cifs reports
// "server"+"share" — decoded into the same StorageInfo.Export field as nfs
// purely because both name the mounted resource on the remote server; they
// are NOT treated as identical by BackingKey, which keys them under separate
// nfs:// and cifs:// schemes (see the BackingKey doc comment) so a share name
// that happens to render like some export's path never merges the two. Every
// other plugin type leaves Path/Server/Export empty, which is correct:
// BackingKey falls back to the storage ID for those types regardless.
func parseStorageEntry(raw json.RawMessage) (StorageInfo, error) {
	var v struct {
		Storage string `json:"storage"`
		Type    string `json:"type"`
		Shared  *int   `json:"shared,omitempty"`
		Nodes   string `json:"nodes,omitempty"`
		Path    string `json:"path,omitempty"`
		Server  string `json:"server,omitempty"`
		Export  string `json:"export,omitempty"`
		Share   string `json:"share,omitempty"`
		Content string `json:"content,omitempty"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return StorageInfo{}, err
	}
	if v.Storage == "" {
		return StorageInfo{}, fmt.Errorf("storage_info: entry missing storage name")
	}

	info := StorageInfo{
		Name:    v.Storage,
		Type:    v.Type,
		Path:    v.Path,
		Server:  v.Server,
		Content: v.Content,
	}
	if v.Shared != nil && *v.Shared != 0 {
		info.Shared = true
	}
	// cifs names its remote resource "share", not "export"; both feed the
	// same BackingKey field since a cifs share and an nfs export are the same
	// concept (the path under Server that is actually mounted).
	switch {
	case v.Export != "":
		info.Export = v.Export
	case v.Share != "":
		info.Export = v.Share
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

// ParseStorageEntry decodes a single /storage response item into a
// StorageInfo. Exported so callers that already hold a json.RawMessage from a
// direct ClusterStorage().ListStorage() call — rather than a single-name
// StorageInfoCache.Get lookup — can decode consistently with the cache's own
// parsing. placement_dlb.go's shared-storage guard is the current example: it
// needs the FULL index (to evaluate backing-identity across pools), not one
// cached name, so it cannot go through StorageInfoCache.Get as-is.
func ParseStorageEntry(raw json.RawMessage) (StorageInfo, error) {
	return parseStorageEntry(raw)
}

// ClusterStorageAsLister adapts a clusterstorage.Service to StorageLister.
// clusterstorage.Service already satisfies StorageLister directly (it exposes
// ListStorage), so this is a convenience alias for production callers.
//
// Usage: pve.NewStorageInfoCache(client.ClusterStorage(), ttl)
func ClusterStorageAsLister(svc clusterstorage.Service) StorageLister {
	return svc
}
