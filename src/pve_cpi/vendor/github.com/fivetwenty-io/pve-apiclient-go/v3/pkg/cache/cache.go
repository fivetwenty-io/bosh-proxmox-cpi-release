// Package cache implements an LRU cache with TTL support for HTTP responses.
//
// The cache provides:
//   - Thread-safe concurrent access
//   - LRU (Least Recently Used) eviction policy
//   - TTL (Time-To-Live) expiration
//   - Pattern-based invalidation
//   - Memory-bounded operation
//   - Hit/miss/eviction metrics
//
// Usage:
//
//	config := cache.DefaultConfig()
//	config.Enabled = true
//	c := cache.NewCache(config)
//
//	// Store a value
//	c.Set("key", value, 5*time.Minute)
//
//	// Retrieve a value
//	if val, found := c.Get("key"); found {
//	    // Use cached value
//	}
//
//	// Get statistics
//	stats := c.Stats()
//	fmt.Printf("Hit rate: %.2f%%\n", stats.HitRate*100)
package cache

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
	"unsafe"
)

const (
	// DefaultMaxCacheSizeMB is the default maximum cache size in bytes (100 MB).
	DefaultMaxCacheSizeMB = 100 * 1024 * 1024
	// DefaultCacheTTL is the default time-to-live for cache entries (5 minutes).
	DefaultCacheTTL = 5 * time.Minute
	// DefaultCleanupInterval is the default cleanup interval for expired entries (1 minute).
	DefaultCleanupInterval = 1 * time.Minute
	// EstimatedEntrySizeBytes is the estimated size per cache entry in bytes (1KB).
	EstimatedEntrySizeBytes = 1024
)

// Entry represents a cached response.
type Entry struct {
	Key        string
	Value      interface{}
	Expiration time.Time
	Size       int64
}

// IsExpired checks if entry has expired.
func (e *Entry) IsExpired() bool {
	return time.Now().After(e.Expiration)
}

// Config holds cache configuration.
type Config struct {
	MaxSize         int64         // Maximum cache size in bytes
	DefaultTTL      time.Duration // Default TTL for entries
	CleanupInterval time.Duration // How often to clean expired entries
	Enabled         bool          // Enable/disable caching
}

// DefaultConfig returns sensible defaults.
func DefaultConfig() Config {
	return Config{
		MaxSize:         DefaultMaxCacheSizeMB,
		DefaultTTL:      DefaultCacheTTL,
		CleanupInterval: DefaultCleanupInterval,
		Enabled:         false, // Opt-in
	}
}

// Cache implements an LRU cache with TTL support.
type Cache struct {
	config      Config
	entries     map[string]*list.Element
	lru         *list.List
	size        int64
	mu          sync.RWMutex
	stopCleanup chan struct{}
	stopOnce    sync.Once

	// Metrics
	hits      int64
	misses    int64
	evictions int64
	corrupted int64
}

// cacheItem wraps an entry for LRU list.
type cacheItem struct {
	key   string
	entry *Entry
}

// NewCache creates a new cache instance.
func NewCache(config Config) *Cache {
	cache := &Cache{
		config:      config,
		entries:     make(map[string]*list.Element),
		lru:         list.New(),
		stopCleanup: make(chan struct{}),
	}

	if config.Enabled {
		go cache.cleanupLoop()
	}

	return cache
}

// Get retrieves a value from cache.
func (c *Cache) Get(key string) (interface{}, bool) {
	if !c.config.Enabled {
		return nil, false
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	elem, exists := c.entries[key]
	if !exists {
		c.misses++

		return nil, false
	}

	item, ok := elem.Value.(*cacheItem)
	if !ok {
		// Internal invariant violation: elem.Value is not a *cacheItem. This
		// should never happen; degrade by dropping the corrupt element
		// instead of panicking on the request path.
		c.removeElement(elem)
		c.misses++

		return nil, false
	}

	// Check expiration
	if item.entry.IsExpired() {
		c.removeElement(elem)
		c.misses++

		return nil, false
	}

	// Move to front (most recently used)
	c.lru.MoveToFront(elem)
	c.hits++

	return item.entry.Value, true
}

// Set stores a value in cache.
func (c *Cache) Set(key string, value interface{}, ttl time.Duration) {
	if !c.config.Enabled {
		return
	}

	if ttl == 0 {
		ttl = c.config.DefaultTTL
	}

	entry := &Entry{
		Key:        key,
		Value:      value,
		Expiration: time.Now().Add(ttl),
		Size:       estimateSize(value),
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// Check if key already exists
	if elem, exists := c.entries[key]; exists {
		item, ok := elem.Value.(*cacheItem)
		if ok {
			c.size -= item.entry.Size
			item.entry = entry
			c.size += entry.Size
			c.lru.MoveToFront(elem)

			return
		}

		// Internal invariant violation: elem.Value is not a *cacheItem. Drop
		// the corrupt element and fall through to insert entry as new,
		// rather than panicking on the request path.
		c.removeElement(elem)
	}

	// Evict if necessary
	for c.size+entry.Size > c.config.MaxSize && c.lru.Len() > 0 {
		c.evictOldest()
	}

	// Add new entry
	item := &cacheItem{key: key, entry: entry}
	elem := c.lru.PushFront(item)
	c.entries[key] = elem
	c.size += entry.Size
}

// Invalidate removes entries matching a pattern.
// Pattern supports wildcard (*) at the end, e.g., "/nodes/*" matches all node paths.
func (c *Cache) Invalidate(pattern string) int {
	c.mu.Lock()
	defer c.mu.Unlock()

	removed := 0

	for key, elem := range c.entries {
		if matchPattern(key, pattern) {
			c.removeElement(elem)

			removed++
		}
	}

	return removed
}

// Clear removes all entries.
func (c *Cache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.entries = make(map[string]*list.Element)
	c.lru = list.New()
	c.size = 0
}

// Stats returns cache statistics.
func (c *Cache) Stats() CacheStats {
	c.mu.RLock()
	defer c.mu.RUnlock()

	total := c.hits + c.misses

	hitRate := 0.0
	if total > 0 {
		hitRate = float64(c.hits) / float64(total)
	}

	return CacheStats{
		Hits:      c.hits,
		Misses:    c.misses,
		Evictions: c.evictions,
		HitRate:   hitRate,
		Size:      c.size,
		Entries:   int64(c.lru.Len()),
		Corrupted: c.corrupted,
	}
}

// CacheStats holds cache statistics.
type CacheStats struct {
	Hits      int64
	Misses    int64
	Evictions int64
	HitRate   float64
	Size      int64
	Entries   int64
	// Corrupted counts internal LRU elements found with an unexpected Value
	// type. This should always be 0; a nonzero value indicates memory
	// corruption or a bug rather than normal cache operation.
	Corrupted int64
}

// Close stops background cleanup. It is safe to call more than once.
func (c *Cache) Close() {
	c.stopOnce.Do(func() {
		close(c.stopCleanup)
	})
}

// Private methods

func (c *Cache) evictOldest() {
	elem := c.lru.Back()
	if elem != nil {
		c.removeElement(elem)
		c.evictions++
	}
}

func (c *Cache) removeElement(elem *list.Element) {
	item, ok := elem.Value.(*cacheItem)
	if !ok {
		c.removeCorruptElement(elem)

		return
	}

	delete(c.entries, item.key)
	c.size -= item.entry.Size
	c.lru.Remove(elem)
}

// removeCorruptElement removes elem from the LRU list, and from the key
// index if a key still maps to it, after elem.Value failed the *cacheItem
// type assertion. This is an internal invariant violation that should never
// occur; the O(n) index scan is acceptable because it only runs on that
// unexpected path, and it avoids leaking a dangling entries[key] -> elem
// mapping when the key cannot be read from the corrupt Value.
func (c *Cache) removeCorruptElement(elem *list.Element) {
	for key, candidate := range c.entries {
		if candidate == elem {
			delete(c.entries, key)

			break
		}
	}

	c.lru.Remove(elem)
	c.corrupted++
}

func (c *Cache) cleanupLoop() {
	ticker := time.NewTicker(c.config.CleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			c.cleanupExpired()
		case <-c.stopCleanup:
			return
		}
	}
}

func (c *Cache) cleanupExpired() {
	c.mu.Lock()
	defer c.mu.Unlock()

	var toRemove []*list.Element

	for elem := c.lru.Back(); elem != nil; elem = elem.Prev() {
		item, ok := elem.Value.(*cacheItem)
		if !ok {
			// Internal invariant violation: schedule the corrupt element for
			// removal alongside expired entries instead of panicking.
			toRemove = append(toRemove, elem)

			continue
		}

		if item.entry.IsExpired() {
			toRemove = append(toRemove, elem)
		}
	}

	for _, elem := range toRemove {
		c.removeElement(elem)
	}
}

// Helper functions

// Sizer lets a cached value report its own approximate memory footprint in
// bytes. Values implementing Sizer bypass the JSON-based estimation in
// estimateSize, which matters for large values (e.g. cached response bodies)
// where re-encoding just to measure would be wasteful and inflate the
// measurement.
type Sizer interface {
	CacheSize() int64
}

// estimateSize returns an approximate memory footprint for cached in bytes,
// used to bound Cache against Config.MaxSize. It is a heuristic, not an exact
// measurement: values implementing Sizer report their own size; for strings
// and byte slices it uses the actual length; for fixed-width scalar types it
// uses their in-memory size (see scalarSize); for everything else it falls
// back to the JSON-encoded length as a proxy for value complexity, and to
// EstimatedEntrySizeBytes if that encoding fails (e.g. channels, funcs, or
// values with cyclic references).
func estimateSize(cached interface{}) int64 {
	if sizer, ok := cached.(Sizer); ok {
		return sizer.CacheSize()
	}

	switch typed := cached.(type) {
	case nil:
		return 0
	case string:
		return int64(len(typed))
	case []byte:
		return int64(len(typed))
	default:
		if size, ok := scalarSize(cached); ok {
			return size
		}

		data, err := json.Marshal(typed)
		if err != nil {
			return EstimatedEntrySizeBytes
		}

		return int64(len(data))
	}
}

// scalarSize returns the in-memory size of cached in bytes and true when it
// holds a fixed-width integer type, or defers to floatOrComplexSize for the
// remaining fixed-width kinds (bool, float, complex). Splitting the type
// switch across scalarSize and floatOrComplexSize keeps each function's
// cyclomatic complexity within project limits.
func scalarSize(cached interface{}) (int64, bool) {
	switch typed := cached.(type) {
	case int:
		return int64(unsafe.Sizeof(typed)), true
	case uint:
		return int64(unsafe.Sizeof(typed)), true
	case int8:
		return int64(unsafe.Sizeof(typed)), true
	case uint8:
		return int64(unsafe.Sizeof(typed)), true
	case int16:
		return int64(unsafe.Sizeof(typed)), true
	case uint16:
		return int64(unsafe.Sizeof(typed)), true
	case int32:
		return int64(unsafe.Sizeof(typed)), true
	case uint32:
		return int64(unsafe.Sizeof(typed)), true
	case int64:
		return int64(unsafe.Sizeof(typed)), true
	case uint64:
		return int64(unsafe.Sizeof(typed)), true
	default:
		return floatOrComplexSize(cached)
	}
}

// floatOrComplexSize returns the in-memory size of cached in bytes and true
// when it holds a bool, float, or complex value, or (0, false) for any other
// type. See scalarSize.
func floatOrComplexSize(cached interface{}) (int64, bool) {
	switch typed := cached.(type) {
	case bool:
		return int64(unsafe.Sizeof(typed)), true
	case float32:
		return int64(unsafe.Sizeof(typed)), true
	case float64:
		return int64(unsafe.Sizeof(typed)), true
	case complex64:
		return int64(unsafe.Sizeof(typed)), true
	case complex128:
		return int64(unsafe.Sizeof(typed)), true
	default:
		return 0, false
	}
}

func matchPattern(key, pattern string) bool {
	// Simple wildcard matching (* at end)
	if len(pattern) == 0 {
		return false
	}

	if pattern[len(pattern)-1] == '*' {
		prefix := pattern[:len(pattern)-1]

		return len(key) >= len(prefix) && key[:len(prefix)] == prefix
	}

	return key == pattern
}

// GenerateKey creates a cache key from request components.
func GenerateKey(method, path string, params map[string]interface{}) string {
	hasher := sha256.New()
	hasher.Write([]byte(method))
	hasher.Write([]byte(path))

	// Sort params for consistent keys
	if len(params) > 0 {
		keys := make([]string, 0, len(params))
		for k := range params {
			keys = append(keys, k)
		}

		sort.Strings(keys)

		for _, k := range keys {
			hasher.Write([]byte(k))
			_, _ = fmt.Fprintf(hasher, "%v", params[k])
		}
	}

	return hex.EncodeToString(hasher.Sum(nil))
}

// GenerateKeyFromURL creates a cache key from a URL string.
func GenerateKeyFromURL(method, url string) string {
	hasher := sha256.New()
	hasher.Write([]byte(method))
	hasher.Write([]byte(url))

	return hex.EncodeToString(hasher.Sum(nil))
}

// ShouldCache determines if a request should be cached based on method.
func ShouldCache(method string) bool {
	// Only cache idempotent GET requests
	return strings.ToUpper(method) == "GET"
}
