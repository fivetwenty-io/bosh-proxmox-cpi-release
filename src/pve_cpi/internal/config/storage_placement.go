package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/storageplacement"
)

// StoragePlacementStrategy selects an installed, versioned ranking algorithm.
type StoragePlacementStrategy struct {
	Name    string `json:"name"`
	Version int    `json:"version"`
}

// StorageSet describes policy over existing shared NFS storage IDs. Inventory
// resolution, backing aliases, and live feasibility are validated at allocation.
type StorageSet struct {
	Names             []string                 `json:"names,omitempty"`
	NamePattern       string                   `json:"name_pattern,omitempty"`
	Types             []string                 `json:"types,omitempty"`
	Shared            *bool                    `json:"shared,omitempty"`
	Encrypted         *bool                    `json:"encrypted,omitempty"`
	Strategy          StoragePlacementStrategy `json:"strategy"`
	MinFreeMB         int64                    `json:"min_free_mb,omitempty"`
	MaxUtilizationPct *int                     `json:"max_utilization_pct,omitempty"`
}

// StorageCapacityDomain declares exports consuming one shared capacity budget.
type StorageCapacityDomain struct {
	Members []string `json:"members"`
}

var storagePlacementKeys = map[string]bool{
	"storage_sets": true, "storage_capacity_domains": true,
	"ephemeral_storage_set": true, "persistent_storage_set": true, "root_storage_set": true,
	"storage_placement_namespace": true, "storage_allocation_journal_dir": true,
	"require_disjoint_storage_sets": true, "storage_status_max_age_seconds": true,
}

// UnmarshalJSON preserves legacy decoding while rejecting malformed new policy
// fields even for callers that decode directly instead of using Load.
func (c *CPIConfig) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	for key, raw := range fields {
		if storagePlacementKeys[key] {
			if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
				return fmt.Errorf("%s must not be null", key)
			}
			if isStoragePlacementString(key) {
				var value string
				if err := json.Unmarshal(raw, &value); err != nil {
					return fmt.Errorf("%s: %w", key, err)
				}
				if strings.TrimSpace(value) == "" {
					return fmt.Errorf("%s must not be blank", key)
				}
			}
		} else if isStoragePlacementKey(strings.ToLower(key)) {
			return fmt.Errorf("unknown storage placement key %q", key)
		}
	}
	type plain CPIConfig
	decoded := plain(*c)
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*c = CPIConfig(decoded)
	return nil
}

func isStoragePlacementString(key string) bool {
	switch key {
	case "ephemeral_storage_set", "persistent_storage_set", "root_storage_set", "storage_placement_namespace", "storage_allocation_journal_dir":
		return true
	default:
		return false
	}
}

func isStoragePlacementKey(key string) bool {
	for _, prefix := range []string{"storage_set", "storage_capacity_domain", "storage_placement_", "storage_allocation_journal", "storage_status_max_age", "require_disjoint_storage", "ephemeral_storage_set", "persistent_storage_set", "root_storage_set"} {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

// decodePlacementObject rejects null members as well as unknown fields. Ordinary
// encoding/json accepts null into scalars, which could silently disable a policy.
func decodePlacementObject(data []byte, target any) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, err
	}
	if fields == nil {
		return nil, fmt.Errorf("expected a non-null object")
	}
	allowed := make(map[string]bool)
	typ := reflect.TypeOf(target).Elem()
	for i := 0; i < typ.NumField(); i++ {
		allowed[strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]] = true
	}
	for key, raw := range fields {
		if !allowed[key] {
			return nil, fmt.Errorf("unknown field %q", key)
		}
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return nil, fmt.Errorf("%s must not be null", key)
		}
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		return nil, err
	}
	return fields, nil
}

// UnmarshalJSON rejects unknown strategy keys and requires explicit name and version.
func (s *StoragePlacementStrategy) UnmarshalJSON(data []byte) error {
	type plain StoragePlacementStrategy
	var value plain
	fields, err := decodePlacementObject(data, &value)
	if err != nil {
		return fmt.Errorf("strategy: %w", err)
	}
	if _, ok := fields["name"]; !ok {
		return fmt.Errorf("strategy.name is required")
	}
	if _, ok := fields["version"]; !ok {
		return fmt.Errorf("strategy.version is required")
	}
	*s = StoragePlacementStrategy(value)
	return nil
}

// UnmarshalJSON decodes a storage set without accepting unknown policy fields.
func (s *StorageSet) UnmarshalJSON(data []byte) error {
	type plain StorageSet
	var value plain
	fields, err := decodePlacementObject(data, &value)
	if err != nil {
		return fmt.Errorf("storage set: %w", err)
	}
	_, names := fields["names"]
	_, pattern := fields["name_pattern"]
	if names == pattern {
		return fmt.Errorf("storage set requires exactly one of names or name_pattern")
	}
	if names && len(value.Names) == 0 {
		return fmt.Errorf("storage set names must not be empty")
	}
	if pattern && strings.TrimSpace(value.NamePattern) == "" {
		return fmt.Errorf("storage set name_pattern must not be blank")
	}
	if _, ok := fields["types"]; ok && len(value.Types) == 0 {
		return fmt.Errorf("storage set types must not be empty")
	}
	if _, ok := fields["strategy"]; !ok {
		return fmt.Errorf("storage set strategy is required")
	}
	*s = StorageSet(value)
	return nil
}

// UnmarshalJSON decodes an explicit capacity domain and rejects unknown fields.
func (d *StorageCapacityDomain) UnmarshalJSON(data []byte) error {
	type plain StorageCapacityDomain
	var value plain
	_, err := decodePlacementObject(data, &value)
	if err != nil {
		return fmt.Errorf("storage capacity domain: %w", err)
	}
	if len(value.Members) == 0 {
		return fmt.Errorf("storage capacity domain members must not be empty")
	}
	*d = StorageCapacityDomain(value)
	return nil
}

// StorageStatusMaxAgeSecondsValue returns the admission observation age limit.
func (c *CPIConfig) StorageStatusMaxAgeSecondsValue() int {
	if c.StorageStatusMaxAgeSeconds == nil {
		return 5
	}
	return *c.StorageStatusMaxAgeSeconds
}

// RequireDisjointStorageSetsEnabled preserves explicit false and defaults to
// separation when both global persistent and ephemeral bindings are present.
func (c *CPIConfig) RequireDisjointStorageSetsEnabled() bool {
	if c.RequireDisjointStorageSets != nil {
		return *c.RequireDisjointStorageSets
	}
	return c.PersistentStorageSet != "" && c.EphemeralStorageSet != ""
}

// HasGlobalStorageSetBindings reports policy presence, not operation activation.
// A persistent-only binding does not activate set-managed VM creation.
func (c *CPIConfig) HasGlobalStorageSetBindings() bool {
	return c.EphemeralStorageSet != "" || c.PersistentStorageSet != "" || c.RootStorageSet != ""
}

// ValidateStoragePlacementAllocation checks static prerequisites only when a
// caller has classified an operation as set-managed. Filesystem readiness and
// cluster enrollment belong to the allocation journal, never CID lifecycle reads.
func (c *CPIConfig) ValidateStoragePlacementAllocation() error {
	if strings.TrimSpace(c.StoragePlacementNamespace) == "" {
		return fmt.Errorf("storage_placement_namespace is required for set-enabled allocation")
	}
	if !filepath.IsAbs(c.StorageAllocationJournalDir) || strings.TrimSpace(c.StorageAllocationJournalDir) == "" {
		return fmt.Errorf("storage_allocation_journal_dir must be an absolute durable directory for set-enabled allocation")
	}
	return nil
}

// ValidateStoragePlacement checks schema and references without contacting PVE,
// inspecting live membership, or requiring access to the journal directory.
func (c *CPIConfig) ValidateStoragePlacement() error {
	var errs []string
	keys := make([]string, 0, len(c.StorageSets))
	for name := range c.StorageSets {
		keys = append(keys, name)
	}
	sort.Strings(keys)
	assertions := make(map[string]bool)
	for _, name := range keys {
		errs = append(errs, validateStorageSet(name, c.StorageSets[name], assertions)...)
	}
	for _, binding := range []struct{ key, value string }{
		{"ephemeral_storage_set", c.EphemeralStorageSet}, {"persistent_storage_set", c.PersistentStorageSet}, {"root_storage_set", c.RootStorageSet},
	} {
		if binding.value == "" {
			continue
		}
		if _, ok := c.StorageSets[binding.value]; !ok {
			errs = append(errs, fmt.Sprintf("%s references undefined storage set %q", binding.key, binding.value))
		}
	}
	if age := c.StorageStatusMaxAgeSecondsValue(); age < 1 || age > 60 {
		errs = append(errs, "storage_status_max_age_seconds must be 1-60")
	}
	if c.StoragePlacementNamespace != "" && strings.TrimSpace(c.StoragePlacementNamespace) == "" {
		errs = append(errs, "storage_placement_namespace must not be blank")
	}
	if c.StorageAllocationJournalDir != "" && !filepath.IsAbs(c.StorageAllocationJournalDir) {
		errs = append(errs, "storage_allocation_journal_dir must be absolute")
	}
	keys = keys[:0]
	for name := range c.StorageCapacityDomains {
		keys = append(keys, name)
	}
	sort.Strings(keys)
	owners := make(map[string]string)
	for _, name := range keys {
		d := c.StorageCapacityDomains[name]
		prefix := fmt.Sprintf("storage_capacity_domains[%s]", name)
		if strings.TrimSpace(name) == "" {
			errs = append(errs, "storage_capacity_domains names must not be blank")
		}
		if len(d.Members) == 0 {
			errs = append(errs, prefix+".members must not be empty")
		}
		validatePlacementNames(prefix+".members", d.Members, &errs)
		for _, member := range d.Members {
			if owner, exists := owners[member]; exists && owner != name {
				errs = append(errs, fmt.Sprintf("storage %q belongs to capacity domains %q and %q", member, owner, name))
			}
			owners[member] = name
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

func validatePlacementNames(field string, values []string, errs *[]string) {
	seen := make(map[string]bool, len(values))
	for _, name := range values {
		if strings.TrimSpace(name) == "" {
			*errs = append(*errs, field+" must not contain blank IDs")
		}
		if seen[name] {
			*errs = append(*errs, fmt.Sprintf("%s contains duplicate ID %q", field, name))
		}
		seen[name] = true
	}
}

// CloneStoragePlacement copies all mutable storage policy values. Other CPI
// fields retain the existing shallow-cloned contract used by context overrides.
func (c *CPIConfig) CloneStoragePlacement() CPIConfig {
	cloned := *c
	if c.StorageSets != nil {
		cloned.StorageSets = make(map[string]StorageSet, len(c.StorageSets))
		for name, s := range c.StorageSets {
			s.Names = slices.Clone(s.Names)
			s.Types = slices.Clone(s.Types)
			s.Shared = clonePlacementPointer(s.Shared)
			s.Encrypted = clonePlacementPointer(s.Encrypted)
			s.MaxUtilizationPct = clonePlacementPointer(s.MaxUtilizationPct)
			cloned.StorageSets[name] = s
		}
	}
	if c.StorageCapacityDomains != nil {
		cloned.StorageCapacityDomains = make(map[string]StorageCapacityDomain, len(c.StorageCapacityDomains))
		for name, d := range c.StorageCapacityDomains {
			d.Members = slices.Clone(d.Members)
			cloned.StorageCapacityDomains[name] = d
		}
	}
	cloned.RequireDisjointStorageSets = clonePlacementPointer(c.RequireDisjointStorageSets)
	cloned.StorageStatusMaxAgeSeconds = clonePlacementPointer(c.StorageStatusMaxAgeSeconds)
	return cloned
}

func clonePlacementPointer[T any](value *T) *T {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

// applyStoragePlacementOverride shares strict decoding with the startup path;
// new fields intentionally do not use legacy string/number coercion.
func applyStoragePlacementOverride(c *CPIConfig, key string, value any) error {
	data, err := json.Marshal(map[string]any{key: value})
	if err != nil {
		return fmt.Errorf("encode storage placement override: %w", err)
	}
	var decoded CPIConfig
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	switch key {
	case "storage_sets":
		c.StorageSets = decoded.StorageSets
	case "storage_capacity_domains":
		c.StorageCapacityDomains = decoded.StorageCapacityDomains
	case "ephemeral_storage_set":
		c.EphemeralStorageSet = decoded.EphemeralStorageSet
	case "persistent_storage_set":
		c.PersistentStorageSet = decoded.PersistentStorageSet
	case "root_storage_set":
		c.RootStorageSet = decoded.RootStorageSet
	case "storage_placement_namespace":
		c.StoragePlacementNamespace = decoded.StoragePlacementNamespace
	case "require_disjoint_storage_sets":
		c.RequireDisjointStorageSets = decoded.RequireDisjointStorageSets
	case "storage_status_max_age_seconds":
		c.StorageStatusMaxAgeSeconds = decoded.StorageStatusMaxAgeSeconds
	default:
		return fmt.Errorf("unsupported storage placement override %q", key)
	}
	return nil
}

// validateStoragePlacementContext treats endpoint changes conservatively: an
// alias may keep the same explicit namespace, but never creates an authority
// from the URL. Verified cluster continuity is checked by journal enrollment.
func validateStoragePlacementContext(base, effective *CPIConfig, extra map[string]any) error {
	if _, present := extra["pve_storage_allocation_journal_dir"]; present {
		return fmt.Errorf("pve_storage_allocation_journal_dir is process-level policy and cannot be overridden")
	}
	for key := range extra {
		if !strings.HasPrefix(key, "pve_") {
			continue
		}
		field := strings.TrimPrefix(key, "pve_")
		if isStoragePlacementKey(strings.ToLower(field)) && !storagePlacementKeys[field] {
			return fmt.Errorf("unknown storage placement override %q", key)
		}
	}
	if (effective.Host != base.Host || effective.Port != base.Port) && base.HasGlobalStorageSetBindings() {
		if _, ok := extra["pve_storage_sets"]; !ok {
			return fmt.Errorf("cluster endpoint change with inherited storage bindings requires explicit pve_storage_sets")
		}
		if _, ok := extra["pve_storage_placement_namespace"]; !ok {
			return fmt.Errorf("cluster endpoint change with inherited storage bindings requires explicit pve_storage_placement_namespace")
		}
	}
	return nil
}

func validateStorageSet(name string, s StorageSet, assertions map[string]bool) []string {
	var errs []string
	prefix := fmt.Sprintf("storage_sets[%s]", name)
	if strings.TrimSpace(name) == "" {
		errs = append(errs, "storage_sets names must not be blank")
	}
	if (len(s.Names) > 0) == (s.NamePattern != "") {
		errs = append(errs, prefix+" requires exactly one of nonempty names or name_pattern")
	}
	if s.NamePattern != "" {
		if strings.TrimSpace(s.NamePattern) == "" {
			errs = append(errs, prefix+".name_pattern must not be blank")
		} else if _, err := regexp.Compile(s.NamePattern); err != nil {
			errs = append(errs, fmt.Sprintf("%s.name_pattern: %v", prefix, err))
		}
	}
	validatePlacementNames(prefix+".names", s.Names, &errs)
	if s.Encrypted != nil {
		for _, id := range s.Names {
			if previous, ok := assertions[id]; ok && previous != *s.Encrypted {
				errs = append(errs, fmt.Sprintf("storage %q has conflicting encrypted assertions across storage sets", id))
			}
			assertions[id] = *s.Encrypted
		}
	}
	if s.Types != nil && !validNFSStorageTypes(s.Types) {
		errs = append(errs, prefix+".types must contain only nfs")
	}
	if s.Shared != nil && !*s.Shared {
		errs = append(errs, prefix+".shared must be true")
	}
	if err := storageplacement.ValidateStrategy(s.Strategy.Name, s.Strategy.Version); err != nil {
		errs = append(errs, fmt.Sprintf("%s.strategy: %v", prefix, err))
	}
	if s.MinFreeMB < 0 || s.MinFreeMB > math.MaxInt64/(1024*1024) {
		errs = append(errs, prefix+".min_free_mb must be nonnegative and fit int64 bytes")
	}
	if s.MaxUtilizationPct != nil && (*s.MaxUtilizationPct < 1 || *s.MaxUtilizationPct > 100) {
		errs = append(errs, prefix+".max_utilization_pct must be 1-100")
	}
	return errs
}

func validNFSStorageTypes(types []string) bool { return len(types) == 1 && types[0] == "nfs" }
