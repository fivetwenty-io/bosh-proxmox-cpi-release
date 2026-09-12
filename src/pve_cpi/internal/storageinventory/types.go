// Package storageinventory freezes read-only PVE storage observations and
// accounts for request-local allocation charges without reserving NAS capacity.
package storageinventory

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// Source is the read-only inventory boundary. A nil response is an observation
// failure; implementations must distinguish it from a successful empty array.
type Source interface {
	Definitions(context.Context) ([]json.RawMessage, error)
	Statuses(context.Context, string) ([]json.RawMessage, error)
}

// Request selects active sets and legacy/infrastructure companion targets.
// Global role sets are included automatically for boundary/future-access checks.
// Unreferenced sets are not resolved. Nodes and IDs are copied by Discover.
type Request struct {
	Nodes               []string
	SetNames            []string
	CompanionStorageIDs []string
	TierNames           []string
}

// Options defines bounded discovery and an injectable, concurrent-safe clock.
type Options struct {
	Concurrency int
	Clock       func() time.Time
}

// ObservationStamp identifies when a request began, not just when it returned.
// Epoch prevents generations from different collectors/processes being confused.
type ObservationStamp struct {
	Epoch       [16]byte
	Generation  uint64
	StartedAt   time.Time
	CompletedAt time.Time
}

// Pair is one node/storage observation. A nonempty Reason excludes this pair.
// CapacityKey scopes nonshared backings to their node without changing the
// canonical PVE BackingKey used for definition safety comparisons.
type Pair struct {
	Node, StorageID, BackingKey, CapacityKey string
	TotalBytes, AvailableBytes               uint64
	Stamp                                    ObservationStamp
	Reason                                   string
}

// Backing is a conservative capacity observation consolidated over healthy
// reports of the same physical budget. Reports are copied by snapshot accessors.
type Backing struct {
	Key                        string
	TotalBytes, AvailableBytes uint64
	Reports                    []ObservationStamp
}

// Domain combines distinct backings using minimum total/available capacity.
// Every configured member must have a usable observation; otherwise Reason
// excludes allocations through this domain without invalidating other domains.
type Domain struct {
	Name                       string
	Members                    []string
	CapacityKeys               []string
	TotalBytes, AvailableBytes uint64
	Reports                    []ObservationStamp
	Reason                     string
}

// sourceError keeps untrusted API response text out of diagnostics while
// preserving the cause for errors.Is/errors.As classification.
type sourceError struct{ err error }

func (e *sourceError) Error() string { return "storage API request failed" }
func (e *sourceError) Unwrap() error { return e.err }

// ObservationError identifies unavailable or malformed inventory, preserving
// its underlying error for cancellation and transport classification.
type ObservationError struct {
	Operation string
	Err       error
}

func (e *ObservationError) Error() string {
	return "storage observation " + e.Operation + ": " + e.Err.Error()
}
func (e *ObservationError) Unwrap() error { return e.Err }

// Snapshot owns its maps and slices. All exported accessors return values or
// defensive copies; Refresh creates a new snapshot and never expands membership.
type Snapshot struct {
	definitions   map[string]pve.StorageInfo
	members       map[string][]string
	sets          map[string]config.StorageSet
	tiers         map[string]string
	domainMembers map[string][]string
	domainForID   map[string]string
	required      map[string]bool
	constrained   map[string]bool
	nodes         []string
	pairs         map[pairKey]Pair
	backings      map[string]Backing
	domains       map[string]Domain
	issues        []string
	maxAge        time.Duration
	observedAt    time.Time
}
type pairKey struct{ node, id string }

// Nodes returns a copy of the frozen candidate node list.
func (s *Snapshot) Nodes() []string { return slices.Clone(s.nodes) }

// Members returns a copy of a set's resolved membership.
func (s *Snapshot) Members(set string) ([]string, bool) {
	v, ok := s.members[set]
	return slices.Clone(v), ok
}

// Definition returns a copy of the named storage definition.
func (s *Snapshot) Definition(id string) (pve.StorageInfo, bool) {
	v, ok := s.definitions[id]
	v.Nodes = slices.Clone(v.Nodes)
	return v, ok
}

// Pair returns the observed node and storage pair.
func (s *Snapshot) Pair(node, id string) (Pair, bool) {
	v, ok := s.pairs[pairKey{node, id}]
	return v, ok
}

// Backing returns the capacity observation for a physical backing.
func (s *Snapshot) Backing(key string) (Backing, bool) {
	v, ok := s.backings[key]
	v.Reports = slices.Clone(v.Reports)
	return v, ok
}

// Domain returns the aggregate observation for a capacity domain.
func (s *Snapshot) Domain(name string) (Domain, bool) {
	v, ok := s.domains[name]
	v.Members = slices.Clone(v.Members)
	v.CapacityKeys = slices.Clone(v.CapacityKeys)
	v.Reports = slices.Clone(v.Reports)
	return v, ok
}

// DomainForStorage returns the domain assigned to a storage ID or backing alias.
func (s *Snapshot) DomainForStorage(id string) string { return s.domainForID[id] }

// Issues returns a copy of the inventory observation failures.
func (s *Snapshot) Issues() []string { return slices.Clone(s.issues) }

// MaxAge returns the maximum permitted observation age.
func (s *Snapshot) MaxAge() time.Duration { return s.maxAge }

// TierStorage returns the lexical legacy tier choice frozen during discovery.
func (s *Snapshot) TierStorage(name string) (string, bool) { id, ok := s.tiers[name]; return id, ok }

// Fresh reports whether all usable observations still satisfy this snapshot's
// age policy. A backward clock invalidates the observation rather than extending
// its lifetime. Failed pairs remain excluded and cannot be resurrected by age.
func (s *Snapshot) Fresh(now time.Time) bool {
	for rangeIndex155 := range s.pairs {
		if s.pairs[rangeIndex155].Reason == "" && !stampFresh(s.pairs[rangeIndex155].Stamp, now, s.maxAge) {
			return false
		}
	}
	return true
}

func stampFresh(s ObservationStamp, now time.Time, age time.Duration) bool {
	return s.Generation > 0 && !s.StartedAt.IsZero() && !s.CompletedAt.Before(s.StartedAt) && !now.Before(s.CompletedAt) && now.Sub(s.StartedAt) <= age
}

func capacityKey(info pve.StorageInfo, node string) string {
	if info.IsShared() {
		return info.BackingKey()
	}
	return fmt.Sprintf("local:%d:%s:%s", len(node), node, info.BackingKey())
}
