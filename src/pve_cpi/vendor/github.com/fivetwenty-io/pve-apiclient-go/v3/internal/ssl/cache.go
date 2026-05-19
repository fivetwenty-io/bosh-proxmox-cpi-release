package ssl

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fivetwenty-io/pve-apiclient-go/v3/internal/constants"
)

// FingerprintCache manages persistent storage of certificate fingerprints.
type FingerprintCache struct {
	mu           sync.RWMutex
	filename     string
	autoSave     bool
	fingerprints map[string]FingerprintEntry
}

// FingerprintEntry represents a cached fingerprint entry.
type FingerprintEntry struct {
	Fingerprint string    `json:"fingerprint"`
	Host        string    `json:"host"`
	Port        int       `json:"port"`
	Trusted     bool      `json:"trusted"`
	FirstSeen   time.Time `json:"first_seen"`
	LastSeen    time.Time `json:"last_seen"`
	LastUsed    time.Time `json:"last_used"`
	Subject     string    `json:"subject,omitempty"`
	Issuer      string    `json:"issuer,omitempty"`
	NotBefore   time.Time `json:"not_before,omitempty"`
	NotAfter    time.Time `json:"not_after,omitempty"`
	Notes       string    `json:"notes,omitempty"`
}

// NewFingerprintCache creates a new fingerprint cache.
func NewFingerprintCache(filename string) *FingerprintCache {
	return &FingerprintCache{
		mu:           sync.RWMutex{},
		filename:     filename,
		autoSave:     true,
		fingerprints: make(map[string]FingerprintEntry),
	}
}

// Load loads fingerprints from the cache file.
func (fc *FingerprintCache) Load() error {
	fc.mu.Lock()
	defer fc.mu.Unlock()

	if fc.filename == "" {
		return nil // No file specified, use memory-only cache
	}

	file, err := os.Open(fc.filename)
	if err != nil {
		if os.IsNotExist(err) {
			// File doesn't exist yet, that's OK
			return nil
		}

		return fmt.Errorf("failed to open cache file: %w", err)
	}

	defer func() {
		_ = file.Close() // Ignore close errors for read operations
	}()

	decoder := json.NewDecoder(file)

	err = decoder.Decode(&fc.fingerprints)
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("failed to decode cache file: %w", err)
	}

	return nil
}

// Save saves fingerprints to the cache file.
func (fc *FingerprintCache) Save() error {
	fc.mu.RLock()
	defer fc.mu.RUnlock()

	if fc.filename == "" {
		return nil // No file specified, use memory-only cache
	}

	// Create directory if it doesn't exist
	dir := filepath.Dir(fc.filename)

	err := os.MkdirAll(dir, constants.DirPermissions)
	if err != nil {
		return fmt.Errorf("failed to create cache directory: %w", err)
	}

	// Create temporary file
	tempFile := filepath.Clean(fc.filename + ".tmp")

	file, err := os.OpenFile(tempFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, constants.FilePermissions)
	if err != nil {
		return fmt.Errorf("failed to create cache file: %w", err)
	}

	// Write JSON data
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")

	err = encoder.Encode(fc.fingerprints)
	if err != nil {
		_ = file.Close()
		_ = os.Remove(tempFile)

		return fmt.Errorf("failed to encode cache data: %w", err)
	}

	err = file.Close()
	if err != nil {
		_ = os.Remove(tempFile)

		return fmt.Errorf("failed to close cache file: %w", err)
	}

	// Atomically rename temp file to actual file
	err = os.Rename(tempFile, fc.filename)
	if err != nil {
		_ = os.Remove(tempFile)

		return fmt.Errorf("failed to save cache file: %w", err)
	}

	return nil
}

// Add adds a fingerprint entry to the cache.
func (fc *FingerprintCache) Add(entry FingerprintEntry) error {
	fc.mu.Lock()
	defer fc.mu.Unlock()

	// Normalize fingerprint
	entry.Fingerprint = NormalizeFingerprint(entry.Fingerprint)

	// Update timestamps
	now := time.Now()

	if existing, exists := fc.fingerprints[entry.Fingerprint]; exists {
		entry.FirstSeen = existing.FirstSeen
		entry.LastSeen = now
	} else {
		entry.FirstSeen = now
		entry.LastSeen = now
	}

	entry.LastUsed = now

	fc.fingerprints[entry.Fingerprint] = entry

	if fc.autoSave {
		return fc.saveUnlocked()
	}

	return nil
}

// Get retrieves a fingerprint entry from the cache.
func (fc *FingerprintCache) Get(fingerprint string) (FingerprintEntry, bool) {
	fc.mu.RLock()
	defer fc.mu.RUnlock()

	normalized := NormalizeFingerprint(fingerprint)

	entry, exists := fc.fingerprints[normalized]
	if exists {
		// Update last used time
		entry.LastUsed = time.Now()
		fc.fingerprints[normalized] = entry
	}

	return entry, exists
}

// Remove removes a fingerprint from the cache.
func (fc *FingerprintCache) Remove(fingerprint string) error {
	fc.mu.Lock()
	defer fc.mu.Unlock()

	normalized := NormalizeFingerprint(fingerprint)
	delete(fc.fingerprints, normalized)

	if fc.autoSave {
		return fc.saveUnlocked()
	}

	return nil
}

// GetAll returns all fingerprint entries.
func (fc *FingerprintCache) GetAll() []FingerprintEntry {
	fc.mu.RLock()
	defer fc.mu.RUnlock()

	entries := make([]FingerprintEntry, 0, len(fc.fingerprints))
	for _, entry := range fc.fingerprints {
		entries = append(entries, entry)
	}

	return entries
}

// GetTrusted returns all trusted fingerprint entries.
func (fc *FingerprintCache) GetTrusted() []FingerprintEntry {
	fc.mu.RLock()
	defer fc.mu.RUnlock()

	var trusted []FingerprintEntry

	for _, entry := range fc.fingerprints {
		if entry.Trusted {
			trusted = append(trusted, entry)
		}
	}

	return trusted
}

// GetByHost returns fingerprint entries for a specific host.
func (fc *FingerprintCache) GetByHost(host string) []FingerprintEntry {
	fc.mu.RLock()
	defer fc.mu.RUnlock()

	var entries []FingerprintEntry

	for _, entry := range fc.fingerprints {
		if entry.Host == host {
			entries = append(entries, entry)
		}
	}

	return entries
}

// SetTrusted sets the trusted status of a fingerprint.
func (fc *FingerprintCache) SetTrusted(fingerprint string, trusted bool) error {
	fc.mu.Lock()
	defer fc.mu.Unlock()

	normalized := NormalizeFingerprint(fingerprint)
	if entry, exists := fc.fingerprints[normalized]; exists {
		entry.Trusted = trusted
		entry.LastUsed = time.Now()
		fc.fingerprints[normalized] = entry

		if fc.autoSave {
			return fc.saveUnlocked()
		}
	}

	return nil
}

// Clear removes all entries from the cache.
func (fc *FingerprintCache) Clear() error {
	fc.mu.Lock()
	defer fc.mu.Unlock()

	fc.fingerprints = make(map[string]FingerprintEntry)

	if fc.autoSave {
		return fc.saveUnlocked()
	}

	return nil
}

// SetAutoSave enables or disables automatic saving.
func (fc *FingerprintCache) SetAutoSave(enabled bool) {
	fc.mu.Lock()
	defer fc.mu.Unlock()

	fc.autoSave = enabled
}

// CleanupExpired removes expired certificate entries.
func (fc *FingerprintCache) CleanupExpired() error {
	fc.mu.Lock()
	defer fc.mu.Unlock()

	now := time.Now()
	changed := false

	for fingerprint, entry := range fc.fingerprints {
		// Remove if certificate has expired and hasn't been used recently
		if !entry.NotAfter.IsZero() && entry.NotAfter.Before(now) {
			// Keep if used within the last 30 days
			if entry.LastUsed.Add(30 * 24 * time.Hour).Before(now) {
				delete(fc.fingerprints, fingerprint)

				changed = true
			}
		}
	}

	if changed && fc.autoSave {
		return fc.saveUnlocked()
	}

	return nil
}

func (fc *FingerprintCache) saveUnlocked() error {
	if fc.filename == "" {
		return nil
	}

	// Create directory if it doesn't exist
	dir := filepath.Dir(fc.filename)

	err := os.MkdirAll(dir, constants.DirPermissions)
	if err != nil {
		return fmt.Errorf("failed to create cache directory: %w", err)
	}

	// Create temporary file
	tempFile := filepath.Clean(fc.filename + ".tmp")

	file, err := os.OpenFile(tempFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, constants.FilePermissions)
	if err != nil {
		return fmt.Errorf("failed to create cache file: %w", err)
	}

	// Write JSON data
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")

	err = encoder.Encode(fc.fingerprints)
	if err != nil {
		_ = file.Close()
		_ = os.Remove(tempFile)

		return fmt.Errorf("failed to encode cache data: %w", err)
	}

	err = file.Close()
	if err != nil {
		_ = os.Remove(tempFile)

		return fmt.Errorf("failed to close cache file: %w", err)
	}

	// Atomically rename temp file to actual file
	err = os.Rename(tempFile, fc.filename)
	if err != nil {
		_ = os.Remove(tempFile)

		return fmt.Errorf("failed to save cache file: %w", err)
	}

	return nil
}

// GetDefaultCacheFile returns the default cache file path.
func GetDefaultCacheFile() string {
	// Get user's home directory
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}

	// Return path to cache file in user's config directory
	return filepath.Join(home, ".config", "pve-apiclient-go", "fingerprints.json")
}
