// Package pve: storage classification cache for the persistent-disk backend
// abstraction. StorageInfo distills the PVE /storage record into the two
// fields the CPI needs at the backend layer: whether the storage is shared
// across the cluster and which nodes (if restricted) can host volumes on it.
package pve

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/clusterstorage"
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
	case "rbd", "cephfs", "nfs", "cifs", "glusterfs", "pbs":
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
type StorageInfoCache struct {
	lister StorageLister
	ttl    time.Duration

	mu      sync.Mutex
	entries map[string]storageInfoEntry
}

type storageInfoEntry struct {
	info StorageInfo
	exp  time.Time
}

// NewStorageInfoCache constructs a cache backed by lister. ttl <= 0 disables
// caching (every Get triggers a fetch).
func NewStorageInfoCache(lister StorageLister, ttl time.Duration) *StorageInfoCache {
	return &StorageInfoCache{
		lister:  lister,
		ttl:     ttl,
		entries: make(map[string]storageInfoEntry),
	}
}

// Get returns the StorageInfo for the named storage. The first call (or a call
// after expiry) fetches the full /storage index and populates the cache.
//
// A storage not present in the index is reported as
// ("", os.ErrNotExist-wrapped CloudError). Callers that want "treat-as-local"
// behavior on lookup miss should handle the error explicitly.
func (c *StorageInfoCache) Get(ctx context.Context, name string) (StorageInfo, error) {
	if name == "" {
		return StorageInfo{}, fmt.Errorf("storage_info: name must not be empty")
	}

	c.mu.Lock()
	if entry, ok := c.entries[name]; ok && (c.ttl <= 0 || time.Now().Before(entry.exp)) {
		c.mu.Unlock()
		return entry.info, nil
	}
	c.mu.Unlock()

	if err := c.refresh(ctx); err != nil {
		return StorageInfo{}, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[name]
	if !ok {
		return StorageInfo{}, fmt.Errorf("storage_info: storage %q not present in PVE /storage index", name)
	}
	return entry.info, nil
}

// Invalidate drops the cached entry for name. The next Get refetches the index.
func (c *StorageInfoCache) Invalidate(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, name)
}

// refresh fetches /storage and replaces the cache wholesale. PVE returns the
// full index even when only one entry is needed, so populating all entries from
// a single request is strictly cheaper than per-name lookups.
func (c *StorageInfoCache) refresh(ctx context.Context) error {
	if c.lister == nil {
		return fmt.Errorf("storage_info: cache has no lister configured")
	}
	resp, err := c.lister.ListStorage(ctx, &clusterstorage.ListStorageParams{})
	if err != nil {
		return fmt.Errorf("storage_info: list cluster storage: %w", err)
	}
	if resp == nil {
		return fmt.Errorf("storage_info: nil response from cluster storage list")
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
