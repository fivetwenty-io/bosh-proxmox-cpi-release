package storageinventory

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// Collector owns request-start generation ordering and bounded read-only API
// access. Use one collector throughout an operation and its ledger refreshes.
type Collector struct {
	source      Source
	clock       func() time.Time
	concurrency int
	epoch       [16]byte
	generation  atomic.Uint64
}

// NewCollector creates an inventory reader with an injectable clock and source.
func NewCollector(source Source, opts Options) (*Collector, error) {
	if source == nil {
		return nil, fmt.Errorf("inventory source is required")
	}
	if opts.Concurrency == 0 {
		opts.Concurrency = 4
	}
	if opts.Concurrency < 1 || opts.Concurrency > 64 {
		return nil, fmt.Errorf("inventory concurrency must be 1-64")
	}
	if opts.Clock == nil {
		opts.Clock = time.Now
	}
	c := &Collector{source: source, clock: opts.Clock, concurrency: opts.Concurrency}
	if _, err := rand.Read(c.epoch[:]); err != nil {
		return nil, fmt.Errorf("inventory epoch: %w", err)
	}
	return c, nil
}

func (c *Collector) begin() ObservationStamp {
	return ObservationStamp{Epoch: c.epoch, Generation: c.generation.Add(1), StartedAt: c.clock()}
}

func (c *Collector) definitions(ctx context.Context) (map[string]pve.StorageInfo, error) {
	raw, err := c.source.Definitions(ctx)
	if err != nil {
		return nil, &ObservationError{Operation: "definitions", Err: &sourceError{err}}
	}
	if raw == nil {
		return nil, &ObservationError{Operation: "definitions", Err: fmt.Errorf("nil response")}
	}
	result := map[string]pve.StorageInfo{}
	for i, item := range raw {
		info, err := parseDefinition(item)
		if err != nil {
			return nil, &ObservationError{Operation: fmt.Sprintf("definition %d", i), Err: err}
		}
		if _, exists := result[info.Name]; exists {
			return nil, &ObservationError{Operation: "definitions", Err: fmt.Errorf("duplicate storage ID %q", info.Name)}
		}
		result[info.Name] = info
	}
	return result, nil
}

// Discover fetches definitions once and freezes membership before observing nodes.
func (c *Collector) Discover(ctx context.Context, cfg *config.CPIConfig, request Request) (*Snapshot, error) {
	if ctx == nil {
		return nil, fmt.Errorf("inventory context is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if cfg == nil {
		return nil, fmt.Errorf("storage configuration is required")
	}
	cloned := cfg.CloneStoragePlacement()
	if err := cloned.ValidateStoragePlacement(); err != nil {
		return nil, err
	}
	// Tier predicates are legacy policy but must also be stable across the API
	// observation. Copy their slices/pointers before any discovery call.
	cloned.StorageTiers = make(map[string]config.StorageTierCriteria, len(cfg.StorageTiers))
	for name, criteria := range cfg.StorageTiers {
		criteria.Types = slices.Clone(criteria.Types)
		if criteria.Shared != nil {
			value := *criteria.Shared
			criteria.Shared = &value
		}
		if criteria.Encrypted != nil {
			value := *criteria.Encrypted
			criteria.Encrypted = &value
		}
		cloned.StorageTiers[name] = criteria
	}
	defs, err := c.definitions(ctx)
	if err != nil {
		return nil, err
	}
	snapshot, err := resolve(&cloned, defs, request)
	if err != nil {
		return nil, err
	}
	return c.Refresh(ctx, snapshot)
}

// Refresh replaces observations only. A contradictory total is refreshed once;
// a persistent contradiction fails instead of selecting an arbitrary report.
func (c *Collector) Refresh(ctx context.Context, frozen *Snapshot) (*Snapshot, error) {
	if ctx == nil || frozen == nil {
		return nil, fmt.Errorf("refresh requires context and frozen snapshot")
	}
	for attempt := 0; attempt < 2; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		s := *frozen
		s.pairs = map[pairKey]Pair{}
		s.backings = map[string]Backing{}
		s.domains = map[string]Domain{}
		s.issues = nil
		pairs, issues, err := c.observeNodes(ctx, frozen)
		if err != nil {
			return nil, err
		}
		s.pairs = pairs
		s.issues = issues
		s.observedAt = c.clock()
		contradiction := s.consolidate(s.observedAt)
		if contradiction != "" {
			if attempt == 0 {
				continue
			}
			return nil, &ObservationError{Operation: "capacity consolidation", Err: fmt.Errorf("inconsistent totals for backing %q after refresh", contradiction)}
		}
		s.buildDomains()
		if len(s.backings) == 0 && len(s.issues) > 0 {
			return nil, &ObservationError{Operation: "node inventory", Err: fmt.Errorf("%s", strings.Join(s.issues, "; "))}
		}
		return &s, nil
	}
	return nil, fmt.Errorf("unreachable inventory refresh state")
}

type nodeResult struct {
	node   string
	pairs  []Pair
	issues []string
	err    error
}

func (c *Collector) observeNodes(ctx context.Context, s *Snapshot) (map[pairKey]Pair, []string, error) {
	jobs := make(chan string)
	results := make(chan nodeResult, len(s.nodes))
	var wg sync.WaitGroup
	for i := 0; i < min(c.concurrency, len(s.nodes)); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for node := range jobs {
				if ctx.Err() != nil {
					return
				}
				results <- c.observeNode(ctx, s, node)
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, node := range s.nodes {
			select {
			case jobs <- node:
			case <-ctx.Done():
				return
			}
		}
	}()
	wg.Wait()
	close(results)
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	pairs := map[pairKey]Pair{}
	var issues []string
	for result := range results {
		if result.err != nil {
			issues = append(issues, fmt.Sprintf("node %s: %v", result.node, result.err))
			for id := range s.required {
				info := s.definitions[id]
				pairs[pairKey{result.node, id}] = Pair{Node: result.node, StorageID: id, BackingKey: info.BackingKey(), CapacityKey: capacityKey(info, result.node), Reason: "node observation failed"}
			}
			continue
		}
		issues = append(issues, result.issues...)
		for pairIndex := range result.pairs {
			pair := result.pairs[pairIndex]
			pairs[pairKey{result.node, pair.StorageID}] = pair
		}
	}
	sort.Strings(issues)
	return pairs, issues, nil
}

func (c *Collector) observeNode(ctx context.Context, s *Snapshot, node string) nodeResult {
	result := nodeResult{node: node}
	stamp := c.begin()
	raw, err := c.source.Statuses(ctx, node)
	stamp.CompletedAt = c.clock()
	if err != nil {
		result.err = &sourceError{err}
		return result
	}
	if raw == nil {
		result.err = fmt.Errorf("nil storage status response")
		return result
	}
	statuses := map[string]storageStatus{}
	parseErrors := map[string]error{}
	for _, item := range raw {
		// Decode identity first so unused storage statuses do not trigger live
		// policy checks or capacity requirements for unrelated legacy resources.
		var identity struct {
			Storage string `json:"storage"`
		}
		if err := json.Unmarshal(item, &identity); err != nil {
			result.err = err
			return result
		}
		if strings.TrimSpace(identity.Storage) == "" {
			result.err = fmt.Errorf("node status entry lacks storage identity")
			return result
		}
		if !s.required[identity.Storage] {
			continue
		}
		if _, exists := statuses[identity.Storage]; exists {
			result.err = fmt.Errorf("duplicate node storage status %q", identity.Storage)
			return result
		}
		status, err := parseStatus(item)
		statuses[identity.Storage] = status
		if err != nil {
			parseErrors[identity.Storage] = err
		}
	}
	for _, id := range sortedKeys(s.required) {
		info := s.definitions[id]
		pair := Pair{Node: node, StorageID: id, BackingKey: info.BackingKey(), CapacityKey: capacityKey(info, node), Stamp: stamp}
		status, present := statuses[id]
		switch {
		case info.Disabled:
			pair.Reason = "storage definition is disabled"
		case len(info.Nodes) > 0 && !slices.Contains(info.Nodes, node):
			pair.Reason = "node is excluded by storage definition"
		case !present:
			pair.Reason = "storage status is missing"
			result.issues = append(result.issues, fmt.Sprintf("node %s storage %s: missing status", node, id))
		case parseErrors[id] != nil:
			pair.Reason = "storage status is malformed"
			result.issues = append(result.issues, fmt.Sprintf("node %s storage %s: %v", node, id, parseErrors[id]))
		case !status.active || !status.enabled:
			pair.Reason = "storage is inactive or disabled on node"
		case !stampFresh(stamp, c.clock(), s.maxAge):
			pair.Reason = "storage status is stale or clock is inconsistent"
			result.issues = append(result.issues, fmt.Sprintf("node %s storage %s: stale observation", node, id))
		case pair.BackingKey == "":
			pair.Reason = "storage backing is unknown"
		case s.constrained[id] && (!strings.EqualFold(info.Type, "nfs") || !info.IsShared() || !hasContent(info.Content, "images")):
			pair.Reason = "set member requires shared NFS with images content"
		default:
			pair.TotalBytes, pair.AvailableBytes = status.total, status.available
		}
		result.pairs = append(result.pairs, pair)
	}
	return result
}

func (s *Snapshot) consolidate(now time.Time) string {
	for _, node := range s.nodes {
		for _, id := range sortedKeys(s.required) {
			key := pairKey{node, id}
			p := s.pairs[key]
			if p.Reason != "" {
				continue
			}
			if !stampFresh(p.Stamp, now, s.maxAge) {
				p.Reason = "storage status expired during inventory"
				s.pairs[key] = p
				s.issues = append(s.issues, fmt.Sprintf("node %s storage %s: expired observation", node, id))
				continue
			}
			b, exists := s.backings[p.CapacityKey]
			if exists && b.TotalBytes != p.TotalBytes {
				return p.CapacityKey
			}
			if !exists {
				b = Backing{Key: p.CapacityKey, TotalBytes: p.TotalBytes, AvailableBytes: p.AvailableBytes}
			} else {
				b.AvailableBytes = min(b.AvailableBytes, p.AvailableBytes)
			}
			b.Reports = append(b.Reports, p.Stamp)
			s.backings[p.CapacityKey] = b
		}
	}
	return ""
}

func (s *Snapshot) buildDomains() {
	for _, name := range sortedKeys(s.domainMembers) {
		d := Domain{Name: name, Members: slices.Clone(s.domainMembers[name])}
		seen := map[string]bool{}
		for _, id := range d.Members {
			found := false
			for _, node := range s.nodes {
				pair := s.pairs[pairKey{node, id}]
				if pair.Reason != "" {
					continue
				}
				b, ok := s.backings[pair.CapacityKey]
				if !ok {
					continue
				}
				found = true
				if seen[b.Key] {
					continue
				}
				seen[b.Key] = true
				if len(d.CapacityKeys) == 0 {
					d.TotalBytes, d.AvailableBytes = b.TotalBytes, b.AvailableBytes
				} else {
					d.TotalBytes = min(d.TotalBytes, b.TotalBytes)
					d.AvailableBytes = min(d.AvailableBytes, b.AvailableBytes)
				}
				d.CapacityKeys = append(d.CapacityKeys, b.Key)
				d.Reports = append(d.Reports, b.Reports...)
			}
			if !found {
				if d.Reason != "" {
					d.Reason += "; "
				}
				d.Reason += "no usable observation for " + id
			}
		}
		sort.Strings(d.CapacityKeys)
		s.domains[name] = d
	}
}

// RevalidateDefinitions rereads selected safety fields immediately before a
// mutation. It never repoints an old plan; callers retain the old identity for
// reconciliation. External edits can still race the later API mutation.
func (c *Collector) RevalidateDefinitions(ctx context.Context, s *Snapshot, ids []string) error {
	if ctx == nil || s == nil {
		return fmt.Errorf("definition revalidation requires context and snapshot")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	current, err := c.definitions(ctx)
	if err != nil {
		return err
	}
	for _, id := range ids {
		old, exists := s.definitions[id]
		if !exists {
			return fmt.Errorf("storage %q is outside frozen definitions", id)
		}
		now, exists := current[id]
		if !exists {
			return fmt.Errorf("selected storage %q was removed", id)
		}
		if !SameDefinitionSafety(old, now) {
			return fmt.Errorf("selected storage %q changed backing or safety fields", id)
		}
	}
	return nil
}

// SameDefinitionSafety compares storage identity, backing, access, and content
// semantics. Node and content ordering do not change a storage definition.
func SameDefinitionSafety(a, b pve.StorageInfo) bool {
	if a.Name != b.Name {
		return false
	}
	if a.BackingKey() != b.BackingKey() || a.Disabled != b.Disabled || !strings.EqualFold(a.Type, b.Type) || a.IsShared() != b.IsShared() {
		return false
	}
	an, bn := slices.Clone(a.Nodes), slices.Clone(b.Nodes)
	sort.Strings(an)
	sort.Strings(bn)
	if !slices.Equal(an, bn) {
		return false
	}
	ac, bc := map[string]bool{}, map[string]bool{}
	for _, item := range strings.Split(a.Content, ",") {
		ac[strings.TrimSpace(item)] = true
	}
	for _, item := range strings.Split(b.Content, ",") {
		bc[strings.TrimSpace(item)] = true
	}
	return maps.Equal(ac, bc)
}
