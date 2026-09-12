package handlers

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
)

// StorageSelectorSource identifies the winning layer and property, including
// global defaults. Explicit distinguishes a profile/call choice from a scalar
// compatibility default when deciding whether VM disks should be bundled.
type StorageSelectorSource struct {
	Layer    string
	Property string
}

// StorageRoleSelection records the winning selector and its global storage boundary.
type StorageRoleSelection struct {
	Role         string
	Kind         string
	Value        string
	Source       StorageSelectorSource
	Explicit     bool
	Atomic       bool
	SetName      string
	Set          *config.StorageSet
	BoundaryName string
	Boundary     *config.StorageSet
	Encrypted    bool
	ReserveMB    int64
	CeilingPct   *int
	// BundleDefault means root inherited E rather than an explicit root choice.
	BundleDefault bool
}

// StoragePlacementSelection is a request-local snapshot for the planner. It
// contains no inventory claims. SetManaged covers all resources of the creation
// operation; a legacy-selected companion keeps its supported backend types.
type StoragePlacementSelection struct {
	Root                *StorageRoleSelection
	Ephemeral           *StorageRoleSelection
	Persistent          *StorageRoleSelection
	SetManaged          bool
	UsesAtomicSelectors bool
	BundleVMDisks       bool
	FuturePersistentSet string
	Policy              *config.CPIConfig
	CloudProperties     map[string]any
}

type storageSelectorLayer struct {
	name   string
	values map[string]any
}

// ResolveStoragePlacementSelectors resolves policy without PVE or journal calls.
// dedicatedEphemeral reflects the existing parsed layout, never set membership.
// Runtime callers must validate full inventory membership through the boundary
// gate and check allocation prerequisites before submitting any set-managed write.
func ResolveStoragePlacementSelectors(cfg *config.CPIConfig, operation string, cloudProperties map[string]any, dedicatedEphemeral bool) (*StoragePlacementSelection, error) {
	if cfg == nil {
		return nil, fmt.Errorf("storage placement: config is required")
	}
	if !knownStoragePlacementOperation(operation) {
		return nil, fmt.Errorf("storage placement: unknown operation %q", operation)
	}
	policy, call, err := copyStoragePlacementInputs(cfg, cloudProperties)
	if err != nil {
		return nil, err
	}
	if err := policy.ValidateStoragePlacement(); err != nil {
		return nil, err
	}
	resolver, err := newLayeredResolver(call, policy)
	if err != nil {
		return nil, err
	}
	layers := storageSelectorLayers(resolver, call)
	if err := validateStorageSelectorLayers(policy, layers); err != nil {
		return nil, err
	}
	result := &StoragePlacementSelection{Policy: policy, CloudProperties: call}
	switch operation {
	case "create_vm":
		result.FuturePersistentSet = policy.PersistentStorageSet
		root, err := resolveStorageRole(policy, resolver, layers, storageRoleRoot)
		if err != nil {
			return nil, err
		}
		result.Root = &root
		if dedicatedEphemeral {
			ephemeral, err := resolveStorageRole(policy, resolver, layers, storageRoleEphemeral)
			if err != nil {
				return nil, err
			}
			result.Ephemeral = &ephemeral
		}
		result.BundleVMDisks = root.BundleDefault && root.Kind == storageSelectorSet
		if result.Ephemeral != nil {
			result.BundleVMDisks = result.BundleVMDisks && result.Ephemeral.Kind == storageSelectorSet && result.Ephemeral.Value == root.Value && result.Ephemeral.Source == root.Source
		}
		result.SetManaged = constrainedStorageRole(result.Root) || constrainedStorageRole(result.Ephemeral)
	case "create_disk":
		persistent, err := resolveStorageRole(policy, resolver, layers, storageRolePersistent)
		if err != nil {
			return nil, err
		}
		result.Persistent = &persistent
		result.SetManaged = constrainedStorageRole(result.Persistent)
	}
	for _, role := range []*StorageRoleSelection{result.Root, result.Ephemeral, result.Persistent} {
		if role != nil && role.Atomic {
			result.UsesAtomicSelectors = true
		}
	}
	return result, nil
}

func knownStoragePlacementOperation(operation string) bool {
	switch operation {
	case "create_vm", "create_disk", "create_stemcell", "delete_stemcell", "delete_vm", "has_vm", "reboot_vm", "set_vm_metadata", "calculate_vm_cloud_properties", "delete_disk", "has_disk", "attach_disk", "detach_disk", "snapshot_disk", "delete_snapshot", "get_disks", "resize_disk", "set_disk_metadata", "update_disk", "create_network", "delete_network", "info":
		return true
	}
	return false
}

// JSON round-trip copies all serialized policy, including profile maps and
// encryption/headroom pointers. No defaults are reapplied. Private config-load
// bookkeeping is not part of the planner policy and is never used as deps.Config.
func copyStoragePlacementInputs(cfg *config.CPIConfig, call map[string]any) (*config.CPIConfig, map[string]any, error) {
	encoded, err := json.Marshal(cfg) // #nosec G117 -- private in-memory deep copy; bytes are never logged, persisted, or returned
	if err != nil {
		return nil, nil, fmt.Errorf("storage placement: copy policy: %w", err)
	}
	var policy config.CPIConfig
	if err := json.Unmarshal(encoded, &policy); err != nil {
		return nil, nil, fmt.Errorf("storage placement: copy policy: %w", err)
	}
	encoded, err = json.Marshal(call)
	if err != nil {
		return nil, nil, fmt.Errorf("storage placement: copy cloud properties: %w", err)
	}
	var copied map[string]any
	if err := json.Unmarshal(encoded, &copied); err != nil {
		return nil, nil, fmt.Errorf("storage placement: copy cloud properties: %w", err)
	}
	return &policy, copied, nil
}

func storageSelectorLayers(r *layeredResolver, call map[string]any) []storageSelectorLayer {
	names := []string{"call"}
	for _, key := range []string{"disk_type", "vm_type"} {
		if value, ok := call[key].(string); ok && value != "" {
			names = append(names, key+":"+value)
		}
	}
	layers := make([]storageSelectorLayer, len(r.layers))
	for i, values := range r.layers {
		layers[i] = storageSelectorLayer{name: names[i], values: values}
	}
	return layers
}

func validateStorageSelectorLayers(cfg *config.CPIConfig, layers []storageSelectorLayer) error {
	for _, layer := range layers {
		if err := validateStorageSelectorLayer(cfg, layer); err != nil {
			return err
		}
	}
	return nil
}

func constrainedStorageRole(role *StorageRoleSelection) bool {
	return role != nil && (role.Kind == storageSelectorSet || role.BoundaryName != "")
}

func storageRoleKeys(role string) (setKey string, poolKeys []string, tierKey string) {
	switch role {
	case storageRoleRoot:
		return "root_storage_set", []string{"storage_pool"}, "storage_tier"
	case storageRoleEphemeral:
		return storageEphemeralSetProperty, []string{"ephemeral_storage_pool"}, "ephemeral_storage_tier"
	default:
		return "storage_set", []string{"storage_pool", "storage"}, "storage_tier"
	}
}

func resolveStorageRole(cfg *config.CPIConfig, resolver *layeredResolver, layers []storageSelectorLayer, role string) (StorageRoleSelection, error) {
	s := StorageRoleSelection{Role: role}
	defaultSet, scalar, scalarKey := cfg.PersistentStorageSet, cfg.DiskStorage, "disk_storage"
	switch role {
	case storageRoleRoot:
		defaultSet, scalar, scalarKey = cfg.RootStorageSet, cfg.VMStorage, "vm_storage"
		if defaultSet == "" {
			defaultSet = cfg.EphemeralStorageSet
			s.BundleDefault = defaultSet != ""
		}
	case storageRoleEphemeral:
		defaultSet, scalar, scalarKey = cfg.EphemeralStorageSet, cfg.VMStorage, "vm_storage"
	}
	s.BoundaryName = defaultSet
	if defaultSet != "" {
		boundary := cfg.StorageSets[defaultSet]
		s.Boundary = &boundary
	}
	setKey, poolKeys, tierKey := storageRoleKeys(role)
	atomic := defaultSet != ""
	for _, layer := range layers {
		_, hasSet := layer.values[setKey]
		_, hasE := layer.values[storageEphemeralSetProperty]
		atomic = atomic || hasSet || role == storageRoleRoot && hasE
	}
	s.Atomic = atomic
	var chosen bool
	var err error
	if atomic {
		s, chosen, err = chooseAtomicStorageRole(s, layers, setKey, poolKeys, tierKey)
	} else {
		s, chosen = chooseLegacyStorageRole(s, layers, poolKeys, tierKey)
	}
	if err != nil {
		return s, err
	}
	if !chosen {
		if defaultSet != "" {
			key := setKey
			if role == storageRolePersistent {
				key = "persistent_storage_set"
			}
			if role == storageRoleRoot && cfg.RootStorageSet == "" {
				key = storageEphemeralSetProperty
			}
			s.Kind, s.Value, s.Source = storageSelectorSet, defaultSet, StorageSelectorSource{Layer: "global", Property: key}
		} else {
			s.Kind, s.Value, s.Source = storageSelectorPool, strings.TrimSpace(scalar), StorageSelectorSource{Layer: "global", Property: scalarKey}
		}
	}
	if s.Kind == storageSelectorSet {
		selected := cfg.StorageSets[s.Value]
		s.SetName = s.Value
		s.Set = &selected
	}
	if role != storageRoleRoot {
		if err := resolveStorageRoleEncryption(cfg, resolver, layers, poolKeys, &s, atomic); err != nil {
			return s, err
		}
	}

	for _, set := range []*config.StorageSet{s.Set, s.Boundary} {
		if set == nil {
			continue
		}
		if set.MinFreeMB > s.ReserveMB {
			s.ReserveMB = set.MinFreeMB
		}
		if set.MaxUtilizationPct != nil {
			s.CeilingPct = stricterStorageCeiling(s.CeilingPct, *set.MaxUtilizationPct)
		}
	}
	if ceiling := cfg.MaxUtilizationPctValue(); ceiling > 0 {
		s.CeilingPct = stricterStorageCeiling(s.CeilingPct, ceiling)
	}
	return s, nil
}

func chooseAtomicStorageRole(s StorageRoleSelection, layers []storageSelectorLayer, setKey string, poolKeys []string, tierKey string) (StorageRoleSelection, bool, error) {
	var winner *StorageRoleSelection
	for _, layer := range layers {
		_, hasSet := layer.values[setKey]
		pool, poolKey, poolExists, err := strictLayerSelector(layer, poolKeys)
		if err != nil {
			return s, false, err
		}
		tier, _, tierExists, err := strictLayerSelector(layer, []string{tierKey})
		if err != nil {
			return s, false, err
		}
		if hasSet && (poolExists || tierExists) {
			return s, false, fmt.Errorf("%s has competing %s set/pool/tier selectors", layer.name, s.Role)
		}
		candidate := s
		candidate.BundleDefault = false
		candidate.Source.Layer = layer.name
		candidate.Explicit = true
		switch {
		case hasSet:
			value, ok := layer.values[setKey].(string)
			if !ok {
				return s, false, fmt.Errorf("storage set selector must be a string")
			}
			candidate.Kind, candidate.Value, candidate.Source.Property = storageSelectorSet, value, setKey
		case s.Role == storageRoleEphemeral && tierExists:
			candidate.Kind, candidate.Value, candidate.Source.Property = storageSelectorTier, tier, tierKey
		case poolExists:
			candidate.Kind, candidate.Value, candidate.Source.Property = storageSelectorPool, pool, poolKey
		case tierExists:
			candidate.Kind, candidate.Value, candidate.Source.Property = storageSelectorTier, tier, tierKey
		case s.Role == storageRoleRoot:
			if value, exists := layer.values[storageEphemeralSetProperty]; exists {
				name, ok := value.(string)
				if !ok {
					return s, false, fmt.Errorf("ephemeral storage set selector must be a string")
				}
				candidate.Kind, candidate.Value, candidate.Source.Property = storageSelectorSet, name, storageEphemeralSetProperty
				candidate.BundleDefault = true
			} else {
				continue
			}
		default:
			continue
		}
		if winner == nil {
			winner = &candidate
		}
	}
	if winner == nil {
		return s, false, nil
	}
	return *winner, true, nil
}

func strictLayerSelector(layer storageSelectorLayer, keys []string) (string, string, bool, error) {
	var value, property string
	for _, key := range keys {
		raw, present := layer.values[key]
		if !present {
			continue
		}
		text, ok := raw.(string)
		if !ok || strings.TrimSpace(text) == "" {
			return "", "", false, fmt.Errorf("%s.%s must be a nonblank string in set-constrained placement", layer.name, key)
		}
		if property == "" {
			value, property = strings.TrimSpace(text), key
		}
	}
	return value, property, property != "", nil
}

// Legacy key priority is intentionally separate from atomic layer precedence.
// In particular a lower-layer ephemeral tier beats a higher-layer legacy pool.
func chooseLegacyStorageRole(s StorageRoleSelection, layers []storageSelectorLayer, poolKeys []string, tierKey string) (StorageRoleSelection, bool) {
	groups := [][]string{poolKeys, {tierKey}}
	if s.Role == storageRoleEphemeral {
		groups = [][]string{{tierKey}, poolKeys}
	}
	for _, keys := range groups {
		for _, layer := range layers {
			for _, key := range keys {
				text, ok := layer.values[key].(string)
				if !ok || strings.TrimSpace(text) == "" {
					continue
				}
				s.Kind = storageSelectorPool
				if key == tierKey {
					s.Kind = storageSelectorTier
				}
				s.Value = strings.TrimSpace(text)
				s.Source = StorageSelectorSource{Layer: layer.name, Property: key}
				s.Explicit = true
				return s, true
			}
		}
	}
	return s, false
}

func validateStorageEncryptionValues(layers []storageSelectorLayer) error {
	for _, layer := range layers {
		if raw, present := layer.values["encrypted"]; present {
			if _, ok := raw.(bool); !ok {
				return fmt.Errorf("%s.encrypted must be a boolean in set-constrained placement", layer.name)
			}
		}
	}
	return nil
}

func applyStorageRoleEncryption(cfg *config.CPIConfig, s *StorageRoleSelection) error {
	if !s.Encrypted {
		return nil
	}
	if s.Boundary != nil && (s.Boundary.Encrypted == nil || !*s.Boundary.Encrypted) {
		return fmt.Errorf("%s boundary %q must assert encrypted storage", s.Role, s.BoundaryName)
	}
	if s.Set != nil {
		if s.Set.Encrypted == nil || !*s.Set.Encrypted {
			return fmt.Errorf("%s set %q must assert encrypted storage", s.Role, s.SetName)
		}
		return nil
	}
	if s.Kind == storageSelectorPool && s.Explicit {
		return fmt.Errorf("%s encrypted=true cannot verify explicit storage pool %q; select an encrypted tier or set", s.Role, s.Value)
	}
	if s.Kind == storageSelectorPool {
		tier, ok := selectEncryptedTier(cfg)
		if !ok {
			return fmt.Errorf("%s encrypted=true requires an encrypted tier or set", s.Role)
		}
		s.Kind, s.Value, s.Source = storageSelectorTier, tier, StorageSelectorSource{Layer: "global", Property: "encrypted"}
	}
	criteria, ok := cfg.StorageTiers[s.Value]
	if !ok || criteria.Encrypted == nil || !*criteria.Encrypted {
		return fmt.Errorf("%s tier %q must assert encrypted storage", s.Role, s.Value)
	}
	return nil
}

func stricterStorageCeiling(current *int, value int) *int {
	if current == nil || value < *current {
		return &value
	}
	return current
}

// ValidateStoragePlacementBoundary checks complete inventory-resolved membership.
// It must run before feasibility narrows a set, or an invalid out-of-bound member
// could be hidden by its temporary inactivity. Backing/disjoint checks remain P3.
func ValidateStoragePlacementBoundary(selection StorageRoleSelection, selectedIDs, boundaryIDs []string) error {
	if selection.BoundaryName == "" {
		return nil
	}
	if selection.Kind == storageSelectorPool {
		selectedIDs = []string{selection.Value}
	}
	if len(selectedIDs) == 0 || len(boundaryIDs) == 0 {
		return fmt.Errorf("%s boundary %q requires nonempty resolved membership", selection.Role, selection.BoundaryName)
	}
	for _, id := range selectedIDs {
		if !slices.Contains(boundaryIDs, id) {
			return fmt.Errorf("%s selector %s.%s places storage %q outside global boundary %q", selection.Role, selection.Source.Layer, selection.Source.Property, id, selection.BoundaryName)
		}
	}
	return nil
}

func validateStorageSelectorLayer(cfg *config.CPIConfig, layer storageSelectorLayer) error {
	for _, key := range []string{"root_storage_set", storageEphemeralSetProperty, "storage_set"} {
		if raw, exists := layer.values[key]; exists {
			value, ok := raw.(string)
			if !ok || strings.TrimSpace(value) == "" {
				return fmt.Errorf("%s.%s must be a nonblank string", layer.name, key)
			}
			if _, ok := cfg.StorageSets[value]; !ok {
				return fmt.Errorf("%s.%s references unknown storage set %q", layer.name, key, value)
			}
		}
	}
	keys := make([]string, 0, len(layer.values))
	for key := range layer.values {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		canonical := strings.TrimPrefix(strings.ToLower(key), "pve_")
		switch canonical {
		case "storage_sets", "storage_capacity_domains", "storage_placement_namespace", "storage_allocation_journal_dir", "persistent_storage_set", "require_disjoint_storage_sets", "storage_status_max_age_seconds":
			return fmt.Errorf("%s.%s is cluster/process policy and cannot be set by resource cloud properties", layer.name, key)
		case "root_storage_set", storageEphemeralSetProperty, "storage_set":
			if key != canonical {
				return fmt.Errorf("%s.%s is not a recognized resource selector", layer.name, key)
			}
		}
		for _, prefix := range []string{"storage_set", "root_storage_set", storageEphemeralSetProperty, "storage_placement_", "storage_allocation_journal", "storage_capacity_domain", "storage_status_max_age", "require_disjoint_storage"} {
			if strings.HasPrefix(canonical, prefix) && canonical != "storage_set" && canonical != "root_storage_set" && canonical != storageEphemeralSetProperty {
				return fmt.Errorf("%s.%s is not a recognized resource selector", layer.name, key)
			}
		}
		if key == "pve" {
			if nested, ok := layer.values[key].(map[string]any); ok {
				for nestedKey := range nested {
					if strings.Contains(strings.ToLower(nestedKey), "storage") {
						return fmt.Errorf("%s.pve.%s cannot override cluster storage policy", layer.name, nestedKey)
					}
				}
			}
		}
	}
	return nil
}

func resolveStorageRoleEncryption(cfg *config.CPIConfig, resolver *layeredResolver, layers []storageSelectorLayer, poolKeys []string, s *StorageRoleSelection, atomic bool) error {
	s.Encrypted = cfg.EncryptedEnabled()
	if value, ok := resolver.Bool("encrypted"); ok {
		s.Encrypted = value
	}
	if atomic {
		if err := validateStorageEncryptionValues(layers); err != nil {
			return err
		}
	}
	if !atomic && s.Encrypted {
		if _, explicitPool := resolver.String(poolKeys...); explicitPool {
			return fmt.Errorf("%s encrypted=true cannot verify an explicit storage pool", s.Role)
		}
	}
	if err := applyStorageRoleEncryption(cfg, s); err != nil {
		return err
	}
	return nil
}
