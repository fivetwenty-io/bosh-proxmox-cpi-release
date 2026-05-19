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
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
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

	// Metrics
	hits      int64
	misses    int64
	evictions int64
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
		panic("cache: invalid item type")
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
		if !ok {
			panic("cache: invalid item type")
		}

		c.size -= item.entry.Size
		item.entry = entry
		c.size += entry.Size
		c.lru.MoveToFront(elem)

		return
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
}

// Close stops background cleanup.
func (c *Cache) Close() {
	close(c.stopCleanup)
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
		panic("cache: invalid item type")
	}

	delete(c.entries, item.key)
	c.size -= item.entry.Size
	c.lru.Remove(elem)
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
			panic("cache: invalid item type")
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

func estimateSize(v interface{}) int64 {
	// Simple estimation - could be more sophisticated
	// For now, use a fixed size estimate of 1KB per entry
	return EstimatedEntrySizeBytes
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
