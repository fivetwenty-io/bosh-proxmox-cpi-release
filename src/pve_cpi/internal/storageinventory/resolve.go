package storageinventory

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

func resolve(cfg *config.CPIConfig, defs map[string]pve.StorageInfo, request Request) (*Snapshot, error) {
	s := &Snapshot{definitions: defs, members: map[string][]string{}, sets: map[string]config.StorageSet{}, domainMembers: map[string][]string{}, domainForID: map[string]string{}, required: map[string]bool{}, constrained: map[string]bool{}, maxAge: time.Duration(cfg.StorageStatusMaxAgeSecondsValue()) * time.Second}
	s.tiers = map[string]string{}
	nodes := map[string]bool{}
	for _, node := range request.Nodes {
		if strings.TrimSpace(node) == "" {
			return nil, fmt.Errorf("blank candidate node")
		}
		if nodes[node] {
			return nil, fmt.Errorf("duplicate candidate node %q", node)
		}
		nodes[node] = true
		s.nodes = append(s.nodes, node)
	}
	if len(s.nodes) == 0 {
		return nil, fmt.Errorf("inventory requires candidate nodes")
	}
	sort.Strings(s.nodes)
	selected := map[string]bool{}
	for _, name := range request.SetNames {
		if strings.TrimSpace(name) == "" {
			return nil, fmt.Errorf("blank active set")
		}
		selected[name] = true
	}
	for _, name := range []string{cfg.PersistentStorageSet, cfg.EphemeralStorageSet, cfg.RootStorageSet} {
		if name != "" {
			selected[name] = true
		}
	}
	missing := map[string]bool{}
	assertions := map[string]bool{}
	if err := s.resolveSets(cfg, defs, selected, missing, assertions); err != nil {
		return nil, err
	}
	for _, id := range request.CompanionStorageIDs {
		if strings.TrimSpace(id) == "" {
			return nil, fmt.Errorf("blank companion storage ID")
		}
		if _, ok := defs[id]; !ok {
			missing[id] = true
		}
		s.required[id] = true
	}
	if err := s.resolveTiers(cfg, defs, request.TierNames); err != nil {
		return nil, err
	}
	backingDomains := map[string]string{}
	if err := s.resolveDomains(cfg, defs, missing, backingDomains); err != nil {
		return nil, err
	}
	if len(missing) > 0 {
		return nil, policyError(PolicyMissingStorage, "missing storage IDs: %s", strings.Join(sortedKeys(missing), ", "))
	}
	// A policy alias of an explicitly declared member consumes the same budget.
	for id := range defs {
		if name, ok := backingDomains[defs[id].BackingKey()]; ok {
			s.domainForID[id] = name
		}
	}
	if cfg.RequireDisjointStorageSetsEnabled() {
		persistent := map[string]string{}
		for _, id := range s.members[cfg.PersistentStorageSet] {
			persistent[defs[id].BackingKey()] = id
		}
		for _, name := range []string{cfg.EphemeralStorageSet, cfg.RootStorageSet} {
			for _, id := range s.members[name] {
				if previous, ok := persistent[defs[id].BackingKey()]; ok {
					return nil, policyError(PolicyOverlappingSets, "persistent and VM storage sets overlap at %q/%q", previous, id)
				}
			}
		}
	}
	return s, nil
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// SetLimits returns a set's configured reserve and utilization ceiling.
func (s *Snapshot) SetLimits(name string) (Limits, error) {
	set, ok := s.sets[name]
	if !ok {
		return Limits{}, fmt.Errorf("set %q is outside frozen inventory", name)
	}
	if set.MinFreeMB < 0 {
		return Limits{}, fmt.Errorf("negative set reserve")
	}
	reserve, err := multiply(uint64(set.MinFreeMB), 1024*1024)
	if err != nil {
		return Limits{}, err
	}
	result := Limits{ReserveBytes: reserve}
	if set.MaxUtilizationPct != nil {
		result.MaxUtilizationPct = *set.MaxUtilizationPct
	}
	return result, nil
}

func (s *Snapshot) resolveSets(cfg *config.CPIConfig, defs map[string]pve.StorageInfo, selected, missing, assertions map[string]bool) error {
	for _, name := range sortedKeys(selected) {
		set, ok := cfg.StorageSets[name]
		if !ok {
			return fmt.Errorf("undefined active storage set %q", name)
		}
		s.sets[name] = set
		members := slices.Clone(set.Names)
		if set.NamePattern != "" {
			pattern, err := regexp.Compile(set.NamePattern)
			if err != nil {
				return fmt.Errorf("set %q pattern: %w", name, err)
			}
			for id := range defs {
				if pattern.MatchString(id) {
					members = append(members, id)
				}
			}
		}
		sort.Strings(members)
		s.members[name] = members
		backings := map[string]string{}
		for _, id := range members {
			info, exists := defs[id]
			if !exists {
				missing[id] = true
				continue
			}
			s.required[id], s.constrained[id] = true, true
			key := info.BackingKey()
			if key == "" {
				return fmt.Errorf("set %q member %q has unknown backing", name, id)
			}
			if previous, exists := backings[key]; exists {
				return policyError(PolicyBackingAliases, "set %q has backing aliases %q and %q", name, previous, id)
			}
			backings[key] = id
			if set.Encrypted != nil {
				if previous, exists := assertions[key]; exists && previous != *set.Encrypted {
					return fmt.Errorf("backing %q has contradictory encryption assertions", key)
				}
				assertions[key] = *set.Encrypted
			}
		}
	}
	return nil
}

func (s *Snapshot) resolveTiers(cfg *config.CPIConfig, defs map[string]pve.StorageInfo, names []string) error {
	for _, name := range names {
		criteria, exists := cfg.StorageTiers[name]
		if !exists {
			return fmt.Errorf("unknown storage tier %q", name)
		}
		var matches []string
		for id := range defs {
			matchesType := len(criteria.Types) == 0
			for _, allowed := range criteria.Types {
				if strings.EqualFold(defs[id].Type, allowed) {
					matchesType = true
					break
				}
			}
			if matchesType && (criteria.Shared == nil || defs[id].IsShared() == *criteria.Shared) {
				matches = append(matches, id)
			}
		}
		if len(matches) == 0 {
			return fmt.Errorf("storage tier %q has no matching storage", name)
		}
		sort.Strings(matches)
		s.tiers[name] = matches[0]
		s.required[matches[0]] = true
	}
	return nil
}

func (s *Snapshot) resolveDomains(cfg *config.CPIConfig, defs map[string]pve.StorageInfo, missing map[string]bool, backingDomains map[string]string) error {
	for _, name := range sortedKeys(cfg.StorageCapacityDomains) {
		members := slices.Clone(cfg.StorageCapacityDomains[name].Members)
		sort.Strings(members)
		s.domainMembers[name] = members
		within := map[string]string{}
		for _, id := range members {
			s.required[id] = true
			info, ok := defs[id]
			if !ok {
				missing[id] = true
				continue
			}
			key := info.BackingKey()
			if key == "" {
				return fmt.Errorf("domain %q member %q has unknown backing", name, id)
			}
			if previous, exists := within[key]; exists {
				return fmt.Errorf("domain %q has backing aliases %q and %q", name, previous, id)
			}
			within[key] = id
			if previous, exists := backingDomains[key]; exists && previous != name {
				return fmt.Errorf("backing %q belongs to domains %q and %q", key, previous, name)
			}
			backingDomains[key] = name
			s.domainForID[id] = name
		}
	}
	return nil
}
