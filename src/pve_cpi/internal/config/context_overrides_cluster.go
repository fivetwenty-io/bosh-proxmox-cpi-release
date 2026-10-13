package config

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// This file holds the per-cluster half of the context-override registry: the
// coercers for the list, optional-integer, and profile-map keys, and the
// rules that apply only when an entry moves a request to another PVE endpoint.

// coerceOverrideStringList decodes a context value into a list of strings.
// The value must be a JSON array whose every element is a string. A null or
// an empty array yields nil, which is what an unset job-level list holds. A
// single string is rejected rather than split, because a comma-separated
// value is a manifest mistake the CPI cannot repair without guessing.
func coerceOverrideStringList(v any) ([]string, error) {
	switch t := v.(type) {
	case nil:
		return nil, nil
	case []string:
		if len(t) == 0 {
			return nil, nil
		}
		return slices.Clone(t), nil
	case []any:
		if len(t) == 0 {
			return nil, nil
		}
		out := make([]string, len(t))
		for i, item := range t {
			s, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("expected a list of strings, element %d is %T", i, item)
			}
			out[i] = s
		}
		return out, nil
	default:
		return nil, fmt.Errorf("expected a list of strings, got %T", v)
	}
}

// coerceOverrideOptionalInt64 decodes a context value into an optional
// integer. A null or 0 yields nil, because the job spec renders an unset
// integer of this kind as 0 and omits it from the config. Any other value
// follows coerceOverrideInt, and the result is always a fresh pointer.
func coerceOverrideOptionalInt64(v any) (*int64, error) {
	if v == nil {
		return nil, nil
	}
	n, err := coerceOverrideInt(v)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, nil
	}
	out := int64(n)
	return &out, nil
}

// coerceOverrideTypeProfiles decodes a context value into a vm_types or
// disk_types profile map, and returns, sorted, the "<profile>.<field>" name
// of every sibling field next to cloud_properties that it ignored.
//
// It is a whole-map replace, like coerceOverridePlacement. A profile can name
// storages and nodes, and profile values sit below per-call cloud_properties,
// so merging an entry's profiles over the job's would leave a stale job key
// that the AZ's own cloud_properties could never clear. An entry that wants
// the job's profiles omits the key, and an entry that wants none sends null
// or an empty map.
//
// A profile carries only cloud_properties. The job-level parse ignores any
// other field, such as a name or description written in the cloud-config
// habit, so the entry path ignores it too rather than failing every request
// routed to the entry; the caller logs the ignored names so a profile written
// without its cloud_properties wrapper is visible. A profile or
// cloud_properties value that is not a map still fails. Each cloud_properties
// map is decoded through JSON, so the result never shares a map with the
// request context or with base.
func coerceOverrideTypeProfiles(v any) (map[string]TypeProfile, []string, error) {
	if v == nil {
		return nil, nil, nil
	}
	raw, ok := v.(map[string]any)
	if !ok {
		return nil, nil, fmt.Errorf("expected a map of profiles, got %T", v)
	}
	profiles := make(map[string]TypeProfile, len(raw))
	var ignored []string
	for name, rawProfile := range raw {
		if rawProfile == nil {
			profiles[name] = TypeProfile{}
			continue
		}
		fields, ok := rawProfile.(map[string]any)
		if !ok {
			return nil, nil, fmt.Errorf("profile %q: expected a map with cloud_properties, got %T", name, rawProfile)
		}
		var profile TypeProfile
		for field, value := range fields {
			if field != "cloud_properties" {
				ignored = append(ignored, name+"."+field)
				continue
			}
			props, err := decodeProfileCloudProperties(value)
			if err != nil {
				return nil, nil, fmt.Errorf("profile %q: cloud_properties: %w", name, err)
			}
			profile.CloudProperties = props
		}
		profiles[name] = profile
	}
	slices.Sort(ignored)
	return profiles, ignored, nil
}

// decodeProfileCloudProperties copies one profile's cloud_properties value
// into a fresh map. A null yields nil, and anything other than a JSON object
// is an error.
func decodeProfileCloudProperties(v any) (map[string]any, error) {
	if v == nil {
		return nil, nil
	}
	if _, ok := v.(map[string]any); !ok {
		return nil, fmt.Errorf("expected a map, got %T", v)
	}
	encoded, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("cannot re-encode: %w", err)
	}
	var props map[string]any
	if err := json.Unmarshal(encoded, &props); err != nil {
		return nil, fmt.Errorf("cannot decode: %w", err)
	}
	return props, nil
}

// recordIgnoredProfileFields appends the ignored sibling fields of one
// profile-map key to c, each as "pve.<key>.<profile>.<field>". The slice is
// always freshly allocated, so an effective config never writes into base.
func (c *CPIConfig) recordIgnoredProfileFields(specKey string, ignored []string) {
	if len(ignored) == 0 {
		return
	}
	out := slices.Clone(c.ignoredProfileFields)
	for _, f := range ignored {
		out = append(out, "pve."+specKey+"."+f)
	}
	c.ignoredProfileFields = out
}

// IgnoredProfileFields lists the profile fields other than cloud_properties
// that a per-entry pve_vm_types or pve_disk_types override carried, each as
// "pve.<key>.<profile>.<field>". ApplyContextOverrides ignores them, the way
// the job-level parse does, and the request handler logs them once.
func (c *CPIConfig) IgnoredProfileFields() []string {
	if c == nil {
		return nil
	}
	return c.ignoredProfileFields
}

// resetEndpointScopedPolicy clears the job-level policy that must never
// follow a request to another PVE endpoint unless the entry names it. Two
// items qualify, destroy_unreferenced_disks and cluster_stamp_aliases, and
// each is judged on its own.
//
// destroy_unreferenced_disks makes every non-retain delete_vm ask PVE to free
// each volume that carries the VM's VMID and is not referenced by its config.
// On storage that two clusters share, the other cluster's live disks match
// that description, so a job-level true inherited by an entry on another
// cluster is cross-cluster data loss. An entry that knows its cluster's
// storage is its own sets the key itself.
//
// cluster_stamp_aliases records one cluster's root CA history, so it is
// meaningless, and unsafe, on any other cluster.
func resetEndpointScopedPolicy(base, eff *CPIConfig, extra map[string]any) {
	if SameEndpoint(base, eff) {
		return
	}
	if _, named := extra["pve_destroy_unreferenced_disks"]; !named {
		eff.DestroyUnreferencedDisks = false
	}
	// The aliases name the job cluster's earlier root CAs. Inherited by an
	// entry on another cluster, a job-level alias that happens to equal the
	// stamp a CID carries would turn a repointed CID into a proven absence on
	// the wrong cluster, and delete_vm would answer success for a VM that
	// still exists elsewhere.
	if _, named := extra["pve_cluster_stamp_aliases"]; !named {
		eff.ClusterStampAliases = nil
	}
}

// DestroyUnreferencedDisksReset reports whether ApplyContextOverrides turned
// off a job-level destroy_unreferenced_disks for the effective config,
// because the entry moved the request to another endpoint without naming the
// key. applied is the key list ApplyContextOverrides returned with effective.
func DestroyUnreferencedDisksReset(base, effective *CPIConfig, applied []string) bool {
	if base == nil || effective == nil {
		return false
	}
	return base.DestroyUnreferencedDisks && !effective.DestroyUnreferencedDisks &&
		!slices.Contains(applied, "pve_destroy_unreferenced_disks") && !SameEndpoint(base, effective)
}

// reapplySDNVNIRangeDefaults gives an entry the VNI band default a job config
// of the entry's zone type would get. ApplyDefaults never runs on an override
// config, so without this an entry that switches sdn_zone_type from vxlan to
// vlan inherits the job's defaulted 5000..5999 band and fails the 4094 VLAN
// ID cap. The job's band counts as defaulted when it equals the default for
// the job's own zone type, and an entry that names either bound keeps exactly
// what it named, with any 0 bound filled the way ApplyDefaults fills it.
func reapplySDNVNIRangeDefaults(base, eff *CPIConfig, extra map[string]any) {
	_, namedType := extra["pve_sdn_zone_type"]
	_, namedStart := extra["pve_sdn_vni_range_start"]
	_, namedEnd := extra["pve_sdn_vni_range_end"]
	if !namedType && !namedStart && !namedEnd {
		return
	}
	if namedType && !namedStart && !namedEnd && sdnVNIRangeIsDefault(base) {
		eff.SDNVNIRangeStart, eff.SDNVNIRangeEnd = 0, 0
	}
	eff.applySDNVNIRangeDefaults()
}

// sdnVNIRangeIsDefault reports whether c's VNI band is the one ApplyDefaults
// gives c's zone type, or is still unset.
func sdnVNIRangeIsDefault(c *CPIConfig) bool {
	if c.SDNVNIRangeStart == 0 && c.SDNVNIRangeEnd == 0 {
		return true
	}
	start, end := defaultSDNVNIRange(c.SDNZoneType)
	return c.SDNVNIRangeStart == start && c.SDNVNIRangeEnd == end
}

// inheritedSDNSettings returns, in registry order, the SDN settings the job
// config carries with a value other than the built-in default and the entry
// does not name, as reported by named. A setting at its default is not a statement about the job
// cluster, because a fresh config for the entry's cluster would get the same
// value. network_mode and sdn_auto_manage_zone are not listed, because they
// decide whether the rule applies at all.
func inheritedSDNSettings(base *CPIConfig, named func(key string) bool) []string {
	defaultZoneType := base.SDNZoneType == "" || base.SDNZoneType == defaultSDNZoneType
	defaultStart, defaultEnd := defaultSDNVNIRange(base.SDNZoneType)
	carried := []struct {
		key string
		set bool
	}{
		{"pve_sdn_zone", base.SDNZone != ""},
		{"pve_sdn_zone_type", !defaultZoneType},
		{"pve_sdn_vxlan_peers", len(base.SDNVxlanPeers) > 0},
		{"pve_sdn_vni_range_start", base.SDNVNIRangeStart != 0 && base.SDNVNIRangeStart != defaultStart},
		{"pve_sdn_vni_range_end", base.SDNVNIRangeEnd != 0 && base.SDNVNIRangeEnd != defaultEnd},
		{"pve_sdn_zone_mtu", base.SDNZoneMTU != nil},
	}
	var inherited []string
	for _, setting := range carried {
		if !setting.set {
			continue
		}
		if !named(setting.key) {
			inherited = append(inherited, setting.key)
		}
	}
	return inherited
}

// sdnIsDefaultPath reports whether create_network sends a managed network
// that names neither a zone nor a bridge down the SDN path under c. That is
// always the case in sdn mode, and in auto mode whenever a job- or
// entry-level sdn_zone is set, because auto mode routes to SDN as soon as a
// zone resolves. It mirrors the routing switch in create_network, where any
// mode other than sdn and bridge behaves as auto.
func sdnIsDefaultPath(c *CPIConfig) bool {
	switch c.NetworkMode {
	case NetworkModeSDN:
		return true
	case NetworkModeBridge:
		return false
	default:
		return c.SDNZone != ""
	}
}

// validateSDNContext fails an entry that moves a request to another endpoint
// while the CPI owns SDN zones there and the entry leaves job-level SDN
// settings to inheritance. When managed networks take the SDN path by default
// (sdn mode, or auto mode with a zone) and sdn_auto_manage_zone is on,
// create_network creates the zone it needs on whichever cluster the request
// reaches, so an inherited zone name, VXLAN peer list, VNI band, zone type,
// or MTU would write the job cluster's fabric onto the other cluster. The
// rule mirrors validateStoragePlacementContext, which fails inherited
// storage-set bindings on an endpoint change for the same reason. The error
// names property keys only, never a value. A network that names its own zone
// in a mode where SDN is not the default path is checked later, when
// create_network is about to create the zone.
func validateSDNContext(base, eff *CPIConfig, extra map[string]any) error {
	if SameEndpoint(base, eff) || !sdnIsDefaultPath(eff) || !eff.SDNAutoManageZoneEnabled() {
		return nil
	}
	inherited := inheritedSDNSettings(base, func(key string) bool {
		_, named := extra[key]
		return named
	})
	if len(inherited) == 0 {
		return nil
	}
	specNames := make([]string, len(inherited))
	for i, key := range inherited {
		specNames[i] = "pve." + strings.TrimPrefix(key, contextOverridePrefix)
	}
	return fmt.Errorf(
		"config: context entry for host %q changes the PVE endpoint while network_mode %q sends managed networks "+
			"to SDN and sdn_auto_manage_zone is on, but inherits job-level SDN settings it does not name (%s); "+
			"set each of them on the entry, or set pve.sdn_auto_manage_zone: false on it",
		eff.Host, eff.NetworkMode, strings.Join(specNames, ", "))
}

// InheritedSDNSettings returns, as "pve_"-prefixed override keys in registry
// order, the job-level SDN settings an effective config carries after its
// entry moved the request to another endpoint without naming them. A setting
// counts only when the job value differs from the built-in default, the same
// test validateSDNContext applies. The result is nil on the job endpoint.
// create_network reads it to refuse auto-creating a zone on the other cluster
// from settings that describe the job cluster's fabric, which matters for a
// network that names its own zone in a mode where validateSDNContext does not
// apply. applied is the key list ApplyContextOverrides returned with
// effective.
func InheritedSDNSettings(base, effective *CPIConfig, applied []string) []string {
	if base == nil || effective == nil || SameEndpoint(base, effective) {
		return nil
	}
	return inheritedSDNSettings(base, func(key string) bool { return slices.Contains(applied, key) })
}

// InheritedClusterNames lists the job-level names an effective config still
// carries after its entry moved the request to another endpoint, in a fixed
// order. It lists the node, the template node, the storage names, and the
// network bridge, each as "pve.<property>=<value>", then each node of the
// inherited placement.az_map as "pve.placement.az_map.<az>=<node>" with the
// AZs sorted, then a bare "pve.vm_types" or "pve.disk_types" marker when the
// entry inherits a non-empty profile map, because a profile can name
// storages and nodes too. These names are often the same on two clusters,
// and the lab uses vmbr0 on both, so inheriting one is legitimate, but a name
// that does not exist on the other cluster fails there with an error that
// does not say where the name came from. The caller logs the list once per
// request. iso_storage counts only when it pins a pool other than the "local"
// spec default, which exists on every node. A name is listed only when it is
// non-empty and the entry did not name its key, and the result is nil on the
// job endpoint. Names are not secrets.
func InheritedClusterNames(base, effective *CPIConfig, applied []string) []string {
	if base == nil || effective == nil || SameEndpoint(base, effective) {
		return nil
	}
	iso := base.OriginalISOStorage()
	if iso == "local" {
		iso = ""
	}
	candidates := []struct {
		key, value string
	}{
		{"pve_node", base.Node},
		{"pve_stemcell_template_node", base.StemcellTemplateNode},
		{"pve_vm_storage", base.VMStorage},
		{"pve_disk_storage", base.DiskStorage},
		{"pve_stemcell_storage", base.StemcellStorage},
		{"pve_iso_storage", iso},
		{"pve_network_bridge", base.NetworkBridge},
	}
	var names []string
	for _, c := range candidates {
		if c.value == "" || slices.Contains(applied, c.key) {
			continue
		}
		names = append(names, "pve."+strings.TrimPrefix(c.key, contextOverridePrefix)+"="+c.value)
	}
	if base.Placement != nil && !slices.Contains(applied, "pve_placement") {
		azs := slices.Sorted(maps.Keys(base.Placement.AZMap))
		for _, az := range azs {
			for _, node := range base.Placement.AZMap[az] {
				if node != "" {
					names = append(names, "pve.placement.az_map."+az+"="+node)
				}
			}
		}
	}
	if len(base.VMTypes) > 0 && !slices.Contains(applied, "pve_vm_types") {
		names = append(names, "pve.vm_types")
	}
	if len(base.DiskTypes) > 0 && !slices.Contains(applied, "pve_disk_types") {
		names = append(names, "pve.disk_types")
	}
	return names
}
