package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// contextOverridePrefix marks a context key as a candidate pve.* per-request
// override (see ApplyContextOverrides). Any context key without this prefix
// is never treated as a config override candidate, known or unknown.
const contextOverridePrefix = "pve_"

// contextOverrideFieldOrder lists, in a stable/deterministic order, every
// context key ApplyContextOverrides honors. Only these pve.* properties may
// be overridden per dispatched request: the subset that drives PVE
// connection identity (host/port/auth/tls), node selection, storage-pool
// names, network bridge, VMID allocation range, agent bootstrap mode, and
// disk format — i.e. everything a single request needs to target the
// correct PVE cluster and place its VM/disk/stemcell correctly there once
// it does.
//
// pve_placement is in scope too, despite being a policy block rather than a
// connection field: its az_map names PVE NODES, and node names are meaningful
// only within one cluster. Excluding it made the three HA features
// (placement.dlb, anti_affinity.use_ha_rules, pin_az_via_ha_rules)
// unreachable from a cpi-config entry — that is, unreachable in exactly the
// multi-cluster deployments that have more than one cluster to fail over
// within — and, once cloud_properties.availability_zone was set, made every
// create_vm on a non-job-level entry hard-fail against the job-level (empty)
// az_map. See coerceOverridePlacement for the whole-block replace semantics.
//
// The SDN settings, the VM and template pools, the stemcell strategy and
// template node, the two ISO layout flags, destroy_unreferenced_disks, and
// the vm_types and disk_types profile maps are in scope because each one
// names or depends on something that lives on one cluster: an SDN zone, its
// VXLAN peers and VNI band, a pool a cluster-scoped token is granted, a node
// name, or that cluster's storage layout. destroy_unreferenced_disks is reset
// to false on an endpoint change unless the entry names it, and an entry that
// changes the endpoint under auto-managed SDN must name the job's SDN
// settings rather than inherit them (see resetEndpointScopedPolicy and
// validateSDNContext).
//
// Every other pve.* config knob (hooks, otel, disk-performance policy, retry
// curves, etc.) is process-wide operating policy rather than a property of
// which cluster a request targets, and is deliberately NOT in this list — it
// continues to come from the job-level config unconditionally for every
// request, overridden or not.
var contextOverrideFieldOrder = []string{
	"pve_host",
	"pve_port",
	"pve_user",
	"pve_password",
	"pve_api_token",
	"pve_realm",
	"pve_node",
	"pve_vm_storage",
	"pve_disk_storage",
	"pve_stemcell_storage",
	"pve_iso_storage",
	"pve_network_bridge",
	"pve_verify_ssl",
	"pve_ca_cert",
	"pve_vmid_range_start",
	"pve_vmid_range_end",
	"pve_disk_vmid_range_start",
	"pve_disk_vmid_range_end",
	"pve_stemcell_template_vmid_range_start",
	"pve_stemcell_template_vmid_range_end",
	"pve_parked_disk_vmid_range_start",
	"pve_parked_disk_vmid_range_end",
	"pve_detached_disk_strategy",
	"pve_disk_migration",
	"pve_stemcell_replicate_local",
	"pve_stemcell_replicate_storage_set",
	"pve_vm_prefix",
	"pve_agent_mode",
	"pve_vm_disk_format",
	"pve_agent_mbus",
	"pve_placement",
	"pve_storage_sets",
	"pve_storage_capacity_domains",
	"pve_ephemeral_storage_set",
	"pve_persistent_storage_set",
	"pve_root_storage_set",
	"pve_storage_placement_namespace",
	"pve_require_disjoint_storage_sets",
	"pve_storage_status_max_age_seconds",
	"pve_parker_prefix",
	"pve_parker_pool",
	"pve_network_mode",
	"pve_sdn_zone",
	"pve_sdn_zone_type",
	"pve_sdn_auto_manage_zone",
	"pve_sdn_vxlan_peers",
	"pve_sdn_vni_range_start",
	"pve_sdn_vni_range_end",
	"pve_sdn_zone_mtu",
	"pve_vm_pool",
	"pve_stemcell_template_pool",
	"pve_stemcell_strategy",
	"pve_stemcell_template_node",
	"pve_require_shared_iso_for_ha",
	"pve_iso_storage_follow_vm_storage",
	"pve_destroy_unreferenced_disks",
	"pve_vm_types",
	"pve_disk_types",
}

// contextOverrideFields maps each supported context key to the function that
// applies its coerced value onto an effective CPIConfig. Built once at
// package init from contextOverrideFieldOrder; every entry in that slice
// must have a matching entry here (asserted by TestContextOverrideFieldOrder
// in context_overrides_test.go) or ApplyContextOverrides would silently skip it.
var contextOverrideFields = map[string]func(*CPIConfig, any) error{
	"pve_storage_sets": func(c *CPIConfig, v any) error { return applyStoragePlacementOverride(c, "storage_sets", v) },
	"pve_storage_capacity_domains": func(c *CPIConfig, v any) error {
		return applyStoragePlacementOverride(c, "storage_capacity_domains", v)
	},
	"pve_ephemeral_storage_set":  func(c *CPIConfig, v any) error { return applyStoragePlacementOverride(c, "ephemeral_storage_set", v) },
	"pve_persistent_storage_set": func(c *CPIConfig, v any) error { return applyStoragePlacementOverride(c, "persistent_storage_set", v) },
	"pve_root_storage_set":       func(c *CPIConfig, v any) error { return applyStoragePlacementOverride(c, "root_storage_set", v) },
	"pve_storage_placement_namespace": func(c *CPIConfig, v any) error {
		return applyStoragePlacementOverride(c, "storage_placement_namespace", v)
	},
	"pve_require_disjoint_storage_sets": func(c *CPIConfig, v any) error {
		return applyStoragePlacementOverride(c, "require_disjoint_storage_sets", v)
	},
	"pve_storage_status_max_age_seconds": func(c *CPIConfig, v any) error {
		return applyStoragePlacementOverride(c, "storage_status_max_age_seconds", v)
	},
	"pve_host": func(c *CPIConfig, v any) error {
		s, err := coerceOverrideString(v)
		if err != nil {
			return err
		}
		c.Host = s
		return nil
	},
	"pve_port": func(c *CPIConfig, v any) error {
		n, err := coerceOverrideInt(v)
		if err != nil {
			return err
		}
		// Value-range/constraint checking (1-65535) is intentionally NOT done
		// here — it is delegated entirely to the eff.Validate() call at the
		// end of ApplyContextOverrides, the same validation the job-level
		// config must pass at CPI startup. A field-local check here previously
		// used a looser bound (0-65535) than Validate() (1-65535), silently
		// accepting pve_port=0; see the H1 finding in the A13 review. One
		// source of truth for value constraints avoids that class of drift.
		c.Port = n
		return nil
	},
	"pve_user": func(c *CPIConfig, v any) error {
		s, err := coerceOverrideString(v)
		if err != nil {
			return err
		}
		c.User = s
		return nil
	},
	"pve_password": func(c *CPIConfig, v any) error {
		s, err := coerceOverrideString(v)
		if err != nil {
			return err
		}
		c.Password = s
		return nil
	},
	"pve_api_token": func(c *CPIConfig, v any) error {
		s, err := coerceOverrideString(v)
		if err != nil {
			return err
		}
		// An entry follows the job's token rule, which ApplyDefaults applies
		// only to the job config.
		c.APIToken = normalizeAPIToken(s)
		return nil
	},
	"pve_realm": func(c *CPIConfig, v any) error {
		s, err := coerceOverrideString(v)
		if err != nil {
			return err
		}
		c.Realm = s
		return nil
	},
	"pve_node": func(c *CPIConfig, v any) error {
		s, err := coerceOverrideString(v)
		if err != nil {
			return err
		}
		c.Node = s
		return nil
	},
	"pve_vm_storage": func(c *CPIConfig, v any) error {
		s, err := coerceOverrideString(v)
		if err != nil {
			return err
		}
		c.VMStorage = s
		return nil
	},
	"pve_disk_storage": func(c *CPIConfig, v any) error {
		s, err := coerceOverrideString(v)
		if err != nil {
			return err
		}
		c.DiskStorage = s
		return nil
	},
	"pve_stemcell_storage": func(c *CPIConfig, v any) error {
		s, err := coerceOverrideString(v)
		if err != nil {
			return err
		}
		c.StemcellStorage = s
		return nil
	},
	"pve_iso_storage": func(c *CPIConfig, v any) error {
		s, err := coerceOverrideString(v)
		if err != nil {
			return err
		}
		c.SetISOStoragePolicy(s)
		return nil
	},
	"pve_network_bridge": func(c *CPIConfig, v any) error {
		s, err := coerceOverrideString(v)
		if err != nil {
			return err
		}
		c.NetworkBridge = s
		return nil
	},
	"pve_verify_ssl": func(c *CPIConfig, v any) error {
		b, err := coerceOverrideBool(v)
		if err != nil {
			return err
		}
		// Always allocate a NEW *bool. base's VerifySSL pointer (if any) is
		// shared by the shallow copy ApplyContextOverrides makes; writing
		// through the inherited pointer instead of replacing it would mutate
		// base's own config out from under every other in-flight/future
		// request that has not opted into this override.
		c.VerifySSL = &b
		return nil
	},
	// The CA is the trust root of one endpoint, so it travels with the host
	// the same way the credential group does: an entry that names another
	// host never inherits the job's private CA (see
	// resolveOverrideCredentials), and this key is how it supplies its own.
	// An explicit empty string selects the system trust pool. PEM parsing
	// is delegated to the eff.Validate() call at the end of
	// ApplyContextOverrides, matching the job-level pve.ca_cert check.
	"pve_ca_cert": func(c *CPIConfig, v any) error {
		s, err := coerceOverrideString(v)
		if err != nil {
			return err
		}
		c.PVECACertPEM = s
		return nil
	},
	"pve_vmid_range_start": func(c *CPIConfig, v any) error {
		n, err := coerceOverrideInt(v)
		if err != nil {
			return err
		}
		// Value constraints (>=100, ordering against pve_vmid_range_end, and
		// non-overlap against the disk/template/parker VMID bands) are
		// enforced by the eff.Validate() call at the end of
		// ApplyContextOverrides, not here — see the pve_port entry's comment
		// for why a single source of truth matters, and the H1 finding in the
		// A13 review for the specific single-bound-override inversion this
		// closes (this field-local check previously only rejected n<=0, and
		// the caller only cross-checked ordering when BOTH bounds were
		// overridden together, so overriding just this field above the
		// inherited end silently produced an empty/inverted range).
		c.VMIDRangeStart = n
		return nil
	},
	"pve_vmid_range_end": func(c *CPIConfig, v any) error {
		n, err := coerceOverrideInt(v)
		if err != nil {
			return err
		}
		// See pve_vmid_range_start above: value constraints are enforced by
		// eff.Validate(), not here.
		c.VMIDRangeEnd = n
		return nil
	},
	// The disk/template/parker bands are per-request overridable for the same
	// reason vmid_range_* is: disjoint per-entry banding is the multi-cluster
	// safety mechanism when cpi-config entries share storage
	// (docs/multi-cluster.md), so each entry's own band literals must reach
	// the request that allocates in them. Constraints (ordering, mutual
	// disjointness) are enforced by eff.Validate() at the end of
	// ApplyContextOverrides, exactly as for the job-level config.
	"pve_disk_vmid_range_start": func(c *CPIConfig, v any) error {
		n, err := coerceOverrideInt(v)
		if err != nil {
			return err
		}
		c.DiskVMIDRangeStart = n
		return nil
	},
	"pve_disk_vmid_range_end": func(c *CPIConfig, v any) error {
		n, err := coerceOverrideInt(v)
		if err != nil {
			return err
		}
		c.DiskVMIDRangeEnd = n
		return nil
	},
	"pve_stemcell_template_vmid_range_start": func(c *CPIConfig, v any) error {
		n, err := coerceOverrideInt(v)
		if err != nil {
			return err
		}
		c.StemcellTemplateVMIDRangeStart = n
		return nil
	},
	"pve_stemcell_template_vmid_range_end": func(c *CPIConfig, v any) error {
		n, err := coerceOverrideInt(v)
		if err != nil {
			return err
		}
		c.StemcellTemplateVMIDRangeEnd = n
		return nil
	},
	"pve_parked_disk_vmid_range_start": func(c *CPIConfig, v any) error {
		n, err := coerceOverrideInt(v)
		if err != nil {
			return err
		}
		c.ParkedDiskVMIDRangeStart = n
		return nil
	},
	"pve_parked_disk_vmid_range_end": func(c *CPIConfig, v any) error {
		n, err := coerceOverrideInt(v)
		if err != nil {
			return err
		}
		c.ParkedDiskVMIDRangeEnd = n
		return nil
	},
	// The detached-disk strategy belongs here for the same reason the parker
	// band does: it is a statement about the TARGET cluster's VMID topology.
	// A cluster whose 90000-90999 band is already occupied by operator guests,
	// or that is administered by someone who does not want BOSH creating
	// parker VMs on it, needs to opt out for that cpi-config entry alone.
	// Without this key the band is movable per entry but the feature is not
	// switchable per entry, and an entry-level "free" would be silently
	// dropped into the unknown-keys warning while every detach kept parking.
	// Enum validation is delegated to the eff.Validate() call at the end of
	// ApplyContextOverrides, matching every other value-constrained key here.
	"pve_detached_disk_strategy": func(c *CPIConfig, v any) error {
		s, err := coerceOverrideString(v)
		if err != nil {
			return err
		}
		c.DetachedDiskStrategy = s
		return nil
	},
	// Cross-node disk migration belongs here for the same reason the
	// detached-disk strategy does: whether the CPI may move volumes between
	// a cluster's nodes (and create the transient mover VMs that move needs,
	// in that cluster's parker band) is a statement about the TARGET
	// cluster, so an entry must be able to turn it off for its cluster
	// alone. Enum validation is delegated to the eff.Validate() call at the
	// end of ApplyContextOverrides, matching every other value-constrained
	// key here.
	"pve_disk_migration": func(c *CPIConfig, v any) error {
		s, err := coerceOverrideString(v)
		if err != nil {
			return err
		}
		c.DiskMigration = s
		return nil
	},
	// Whether cache templates need per-node replicas is a property of the
	// TARGET cluster's storage topology (node-local vs shared vm_storage),
	// so a cpi-config entry must be able to set it for its own cluster.
	"pve_stemcell_replicate_local": func(c *CPIConfig, v any) error {
		b, err := coerceOverrideBool(v)
		if err != nil {
			return err
		}
		c.StemcellReplicateLocal = b
		return nil
	},
	// Whether a cluster's storage set members each carry a cache template is
	// a property of that cluster's set topology, so a cpi-config entry must
	// be able to set it for its own cluster.
	"pve_stemcell_replicate_storage_set": func(c *CPIConfig, v any) error {
		b, err := coerceOverrideBool(v)
		if err != nil {
			return err
		}
		// An entry that names the key is stating an explicit choice, so the
		// pointer is always set here and the tri-state default never applies
		// to that cluster.
		c.StemcellReplicateStorageSet = &b
		return nil
	},
	// VM-name prefix is cluster-facing identity (operators distinguish each
	// director/entry's VMs by it in the PVE UI), not process policy.
	"pve_vm_prefix": func(c *CPIConfig, v any) error {
		s, err := coerceOverrideString(v)
		if err != nil {
			return err
		}
		c.VMPrefix = s
		return nil
	},
	// Parker VM names are cluster-facing identity rather than process
	// policy, the same justification pve_vm_prefix carries above and the
	// reason the parker VMID band is already overridable too.
	"pve_parker_prefix": func(c *CPIConfig, v any) error {
		s, err := coerceOverrideString(v)
		if err != nil {
			return err
		}
		c.ParkerPrefix = s
		return nil
	},
	// The parker pool name is cluster-facing identity for the same reason
	// the parker prefix above is, since it also names something on the
	// target cluster. pve.vm_pool_template stays job-level on purpose,
	// because it is a naming pattern and pools are created on demand.
	"pve_parker_pool": func(c *CPIConfig, v any) error {
		s, err := coerceOverrideString(v)
		if err != nil {
			return err
		}
		c.ParkerPool = s
		return nil
	},
	"pve_agent_mode": func(c *CPIConfig, v any) error {
		s, err := coerceOverrideString(v)
		if err != nil {
			return err
		}
		c.AgentMode = s
		return nil
	},
	"pve_vm_disk_format": func(c *CPIConfig, v any) error {
		s, err := coerceOverrideString(v)
		if err != nil {
			return err
		}
		c.VMDiskFormat = s
		return nil
	},
	"pve_placement": func(c *CPIConfig, v any) error {
		p, err := coerceOverridePlacement(v)
		if err != nil {
			return err
		}
		// Always a freshly-allocated block (or nil), never base's pointer —
		// ApplyContextOverrides' shallow copy shares every other pointer
		// field with base, so writing through this one would leak an entry's
		// az_map into every other cpi-config entry this process serves.
		c.Placement = p
		// Give the overridden block the same weight defaults ApplyDefaults
		// gives the job-level one; ApplyContextOverrides runs Validate but
		// not ApplyDefaults, so without this an entry setting a partial
		// weights block would score on zeroed axes.
		c.applyPlacementDefaults()
		return nil
	},
	"pve_agent_mbus": func(c *CPIConfig, v any) error {
		s, err := coerceOverrideString(v)
		if err != nil {
			return err
		}
		c.AgentMBus = s
		return nil
	},
	// SDN zones, their VXLAN peers, and their VNI bands are per cluster by
	// nature, and with sdn_auto_manage_zone on, create_network writes them
	// onto whichever cluster the request reaches. Enum and range checks are
	// delegated to the eff.Validate() call at the end of
	// ApplyContextOverrides, matching every other value-constrained key here.
	"pve_network_mode": func(c *CPIConfig, v any) error {
		s, err := coerceOverrideString(v)
		if err != nil {
			return err
		}
		c.NetworkMode = s
		return nil
	},
	"pve_sdn_zone": func(c *CPIConfig, v any) error {
		s, err := coerceOverrideString(v)
		if err != nil {
			return err
		}
		c.SDNZone = s
		return nil
	},
	"pve_sdn_zone_type": func(c *CPIConfig, v any) error {
		s, err := coerceOverrideString(v)
		if err != nil {
			return err
		}
		c.SDNZoneType = s
		return nil
	},
	"pve_sdn_auto_manage_zone": func(c *CPIConfig, v any) error {
		b, err := coerceOverrideBool(v)
		if err != nil {
			return err
		}
		// A fresh pointer, never a write through base's, for the reason the
		// pve_verify_ssl entry gives.
		c.SDNAutoManageZone = &b
		return nil
	},
	"pve_sdn_vxlan_peers": func(c *CPIConfig, v any) error {
		peers, err := coerceOverrideStringList(v)
		if err != nil {
			return err
		}
		// Each peer must parse as an IP, which eff.Validate() checks. An
		// empty list or null derives the peers from the target cluster's own
		// membership, the same as an unset job-level list.
		c.SDNVxlanPeers = peers
		return nil
	},
	"pve_sdn_vni_range_start": func(c *CPIConfig, v any) error {
		n, err := coerceOverrideInt(v)
		if err != nil {
			return err
		}
		c.SDNVNIRangeStart = n
		return nil
	},
	"pve_sdn_vni_range_end": func(c *CPIConfig, v any) error {
		n, err := coerceOverrideInt(v)
		if err != nil {
			return err
		}
		c.SDNVNIRangeEnd = n
		return nil
	},
	"pve_sdn_zone_mtu": func(c *CPIConfig, v any) error {
		mtu, err := coerceOverrideOptionalInt64(v)
		if err != nil {
			return err
		}
		// A fresh pointer or nil. Null and 0 both mean unset, which lets PVE
		// derive the MTU from that cluster's underlay, because the job spec
		// renders 0 as an absent key.
		c.SDNZoneMTU = mtu
		return nil
	},
	// A token scoped to one cluster's pool needs that cluster's pool name.
	// An explicit empty string opts the entry out of pool assignment, the
	// same as it does at job level. Charset and collision rules are
	// delegated to eff.Validate().
	"pve_vm_pool": func(c *CPIConfig, v any) error {
		s, err := coerceOverrideString(v)
		if err != nil {
			return err
		}
		c.VMPool = s
		return nil
	},
	"pve_stemcell_template_pool": func(c *CPIConfig, v any) error {
		s, err := coerceOverrideString(v)
		if err != nil {
			return err
		}
		c.StemcellTemplatePool = s
		return nil
	},
	// A standalone host, or a cluster without shared template storage, may
	// need import while the job cluster clones templates. Enum validation is
	// delegated to eff.Validate().
	"pve_stemcell_strategy": func(c *CPIConfig, v any) error {
		s, err := coerceOverrideString(v)
		if err != nil {
			return err
		}
		c.StemcellStrategy = s
		return nil
	},
	// It names a node, and node names differ per cluster.
	"pve_stemcell_template_node": func(c *CPIConfig, v any) error {
		s, err := coerceOverrideString(v)
		if err != nil {
			return err
		}
		c.StemcellTemplateNode = s
		return nil
	},
	// Both ISO flags describe the target cluster's storage layout, and a
	// standalone host has no HA at all. Each gets a fresh pointer, so an
	// entry that names the key states an explicit choice for its cluster.
	"pve_require_shared_iso_for_ha": func(c *CPIConfig, v any) error {
		b, err := coerceOverrideBool(v)
		if err != nil {
			return err
		}
		c.RequireSharedISOForHA = &b
		return nil
	},
	"pve_iso_storage_follow_vm_storage": func(c *CPIConfig, v any) error {
		b, err := coerceOverrideBool(v)
		if err != nil {
			return err
		}
		c.ISOStorageFollowVMStorage = &b
		return nil
	},
	// The flag frees every unreferenced same-VMID volume on the storage a VM
	// used, so its safety depends on whether that cluster shares storage
	// with another one. An entry on another endpoint that does not name it
	// gets false rather than the job's value (see resetEndpointScopedPolicy).
	"pve_destroy_unreferenced_disks": func(c *CPIConfig, v any) error {
		b, err := coerceOverrideBool(v)
		if err != nil {
			return err
		}
		c.DestroyUnreferencedDisks = b
		return nil
	},
	// Profiles can name storages and nodes, and profile values sit below
	// per-call cloud_properties, so an AZ could never clear a stale profile
	// key through its own cloud_properties. Each map is therefore replaced
	// whole, like placement (see coerceOverrideTypeProfiles).
	"pve_vm_types": func(c *CPIConfig, v any) error {
		profiles, ignored, err := coerceOverrideTypeProfiles(v)
		if err != nil {
			return err
		}
		c.VMTypes = profiles
		c.recordIgnoredProfileFields("vm_types", ignored)
		return nil
	},
	"pve_disk_types": func(c *CPIConfig, v any) error {
		profiles, ignored, err := coerceOverrideTypeProfiles(v)
		if err != nil {
			return err
		}
		c.DiskTypes = profiles
		c.recordIgnoredProfileFields("disk_types", ignored)
		return nil
	},
}

// coerceOverridePlacement decodes a context value into a *PlacementConfig.
//
// Whole-block REPLACE, not a merge: a cpi-config entry that defines placement
// defines it completely, so an operator reading one entry sees that entry's
// entire placement policy rather than a diff against a job-level block they
// have to hold in their head. Merging pointer-typed sub-blocks (weights,
// anti_affinity, dlb) field by field would also make "unset" and "explicitly
// false" indistinguishable per field, which is exactly the ambiguity the
// pointer types exist to avoid. An entry that wants the job-level policy
// simply omits the key; an entry that wants NO placement policy sends null.
//
// Decoding goes through JSON so the block honors the identical struct tags
// and nested shapes the job-level config parses, with DisallowUnknownFields
// so a typo inside the block (say "pin_az_via_ha_rule") is rejected rather
// than silently parsed into a block that pins nothing. Values that are not
// objects at all are rejected for the same reason: coercing them to an empty
// block would quietly disable every placement feature for that entry.
//
// nil (explicit JSON null) clears the block, letting one entry opt out of an
// inherited job-level policy.
func coerceOverridePlacement(v any) (*PlacementConfig, error) {
	if v == nil {
		return nil, nil
	}
	raw, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("expected a placement object, got %T", v)
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("cannot re-encode placement block: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(encoded))
	dec.DisallowUnknownFields()
	var p PlacementConfig
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("cannot decode placement block: %w", err)
	}
	return &p, nil
}

// coerceOverrideString requires v to already be a JSON string. Context
// property values arrive as whatever the BOSH director's manifest author
// wrote in cpi-config properties; a non-string value for a string-typed
// pve.* field (e.g. a number or object where a hostname is expected) is
// always an operator/manifest error, so this never attempts a lossy
// stringification — it fails loudly instead.
func coerceOverrideString(v any) (string, error) {
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("expected string, got %T", v)
	}
	return s, nil
}

// coerceOverrideInt accepts a JSON number (decoded as float64 by
// encoding/json into `any`) or a numeric string ("8006"), matching the same
// tolerance the BOSH director's own property rendering can produce depending
// on manifest YAML typing. A non-integer float (e.g. 8006.5) is rejected
// rather than truncated, since a fractional PVE port/VMID bound can only be
// a manifest mistake. int/int64 are accepted too so tests and any future
// programmatic caller need not round-trip through JSON.
func coerceOverrideInt(v any) (int, error) {
	switch t := v.(type) {
	case float64:
		if t != math.Trunc(t) {
			return 0, fmt.Errorf("expected integer, got non-integer number %v", t)
		}
		return int(t), nil
	case int:
		return t, nil
	case int64:
		return int(t), nil
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return 0, fmt.Errorf("expected integer, got empty string")
		}
		n, err := strconv.Atoi(s)
		if err != nil {
			return 0, fmt.Errorf("expected integer, got %q: %w", t, err)
		}
		return n, nil
	default:
		return 0, fmt.Errorf("expected integer, got %T", v)
	}
}

// coerceOverrideBool accepts a JSON bool or a boolean-ish string ("true",
// "false", "1", "0", "TRUE", ...) via strconv.ParseBool, matching the same
// dual-typed tolerance coerceOverrideInt applies to numbers — BOSH manifest
// YAML can render a boolean property as either JSON type depending on how
// the operator quoted it.
func coerceOverrideBool(v any) (bool, error) {
	switch t := v.(type) {
	case bool:
		return t, nil
	case string:
		b, err := strconv.ParseBool(strings.TrimSpace(t))
		if err != nil {
			return false, fmt.Errorf("expected boolean, got %q: %w", t, err)
		}
		return b, nil
	default:
		return false, fmt.Errorf("expected boolean, got %T", v)
	}
}

// flattenNestedContextOverrides folds BOSH's actual cpi-config context shape
// into the flat pve_* candidate keys this mechanism reads. The director does
// NOT deliver an entry's properties as flat pve_* keys: the entry's whole
// `properties:` hash is merged into the request context as-is, so they
// arrive NESTED — context.pve = {host: ..., vm_storage: ..., ...} and
// context.agent = {mbus: ...} (live-verified against a BOSH 282.x director's
// task debug log; without this fold every cpi-config request silently ran
// against the job-level cluster, the exact defect this mechanism exists to
// close). Every key of a nested pve map becomes "pve_<key>", so supported
// ones apply and unsupported ones surface in the unknown list under the
// same name an operator would use for the flat form. context.agent.mbus
// maps to pve_agent_mbus, the registry's existing key for the agent
// bootstrap endpoint. Explicit flat keys always win over nested ones. The
// input map is never mutated.
func flattenNestedContextOverrides(extra map[string]any) map[string]any {
	pveRaw, _ := extra["pve"].(map[string]any)
	agentRaw, _ := extra["agent"].(map[string]any)
	if pveRaw == nil && agentRaw == nil {
		return extra
	}
	merged := make(map[string]any, len(extra)+len(pveRaw)+1)
	maps.Copy(merged, extra)
	for k, v := range pveRaw {
		flat := contextOverridePrefix + k
		if _, exists := merged[flat]; !exists {
			merged[flat] = v
		}
	}
	if mbus, ok := agentRaw["mbus"]; ok {
		if _, exists := merged["pve_agent_mbus"]; !exists {
			merged["pve_agent_mbus"] = mbus
		}
	}
	return merged
}

// overrideCredentialKeys is the credential group: the context keys that
// authenticate a request to one PVE endpoint. They travel as a group, so an
// effective config holds either all three exactly as the job config has
// them, or exactly what the entry itself named, never a mix. The realm is
// deliberately not in the group. It is not a secret, and an entry on the job
// host that sets a user and password commonly relies on the job's realm
// (for example "pve"), so it keeps inheriting unless the entry sets it.
var overrideCredentialKeys = []string{"pve_user", "pve_password", "pve_api_token"}

// Credential group sources reported by OverrideCredentialGroup.
const (
	// CredentialGroupInherited means the request authenticates with the
	// job-level credentials, which only happens on the job-level endpoint.
	CredentialGroupInherited = "inherited"
	// CredentialGroupEntry means the request authenticates with only the
	// credentials its cpi-config entry named.
	CredentialGroupEntry = "entry"
)

// defaultPVEAPIPort is the port pveproxy listens on, and the value
// ApplyDefaults gives an unset pve.port.
const defaultPVEAPIPort = 8006

// normalizedEndpoint returns c's PVE API endpoint in the form endpoint
// comparison uses: the host trimmed of surrounding space, lowercased, and
// without a trailing dot, and the port with 0 read as the default 8006.
func normalizedEndpoint(c *CPIConfig) (host string, port int) {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(c.Host)), ".")
	port = c.Port
	if port == 0 {
		port = defaultPVEAPIPort
	}
	return host, port
}

// SameEndpoint reports whether a and b address the same PVE API endpoint.
// Host case, a trailing dot on the host, and an unset port versus an
// explicit 8006 do not count as differences, because each of those spellings
// reaches the same pveproxy. Any other difference does, including a
// different name for the same machine, because the CPI cannot prove two
// names resolve to one host and must not send credentials on that guess.
func SameEndpoint(a, b *CPIConfig) bool {
	aHost, aPort := normalizedEndpoint(a)
	bHost, bPort := normalizedEndpoint(b)
	return aHost == bHost && aPort == bPort
}

// overrideCredentialKeysSet returns, in group order, the credential keys an
// entry named. A key counts as named whenever it is present, including with
// an empty value, because an entry that writes the key is stating its own
// credentials rather than asking for the job's.
func overrideCredentialKeysSet(present func(key string) bool) []string {
	var set []string
	for _, key := range overrideCredentialKeys {
		if present(key) {
			set = append(set, key)
		}
	}
	return set
}

// OverrideCredentialGroup reports which credential group an effective config
// built by ApplyContextOverrides authenticates with, given the job config
// and the applied key list ApplyContextOverrides returned with it. The
// answer is CredentialGroupEntry whenever the endpoint changed or the entry
// named any credential key, and CredentialGroupInherited otherwise. It reads
// key names and endpoints only, never a credential value, so callers may log
// the result.
func OverrideCredentialGroup(base, effective *CPIConfig, applied []string) string {
	if base == nil || effective == nil {
		// No caller passes nil. If one ever does, claim the safer label, so
		// a warning never says the job credentials stayed on the job host.
		return CredentialGroupEntry
	}
	named := overrideCredentialKeysSet(func(key string) bool { return slices.Contains(applied, key) })
	if !SameEndpoint(base, effective) || len(named) > 0 {
		return CredentialGroupEntry
	}
	return CredentialGroupInherited
}

// resolveOverrideCredentials makes the credential group and the CA travel
// with the endpoint. It runs on eff after every field handler has applied the
// entry's keys and before the effective config is validated, so it sees the
// entry's final host and port.
//
// Without it, each credential key applied one at a time: an entry that set
// only a new host inherited the job's api_token, and the PVE client prefers a
// token whenever one is set, so the job's token went to the other cluster.
// An entry that set only a password while the job held a token kept both,
// and the job's token again won on the wire.
//
// The rules are these. When the endpoint changes, or the entry names any
// credential key, every credential key the entry did not name is cleared, so
// nothing from the job's group survives. When the endpoint changes, the job's
// pve.ca_cert is cleared too unless the entry names pve_ca_cert, so an entry
// that keeps verify_ssl on gets the system trust pool rather than a private
// CA issued for another host. The job's token-over-password rule then
// applies, and the result must hold an api_token, or a user together with a
// password. Errors name property keys only, never a value.
func resolveOverrideCredentials(base, eff *CPIConfig, extra map[string]any) error {
	present := func(key string) bool {
		_, ok := extra[key]
		return ok
	}
	endpointChanged := !SameEndpoint(base, eff)
	if endpointChanged && !present("pve_ca_cert") {
		eff.PVECACertPEM = ""
	}

	named := overrideCredentialKeysSet(present)
	if !endpointChanged && len(named) == 0 {
		// Same endpoint and no credential key named: the request keeps the
		// job's whole group, which already passed startup validation.
		return nil
	}

	if !present("pve_user") {
		eff.User = ""
	}
	if !present("pve_password") {
		eff.Password = ""
	}
	if !present("pve_api_token") {
		eff.APIToken = ""
	}
	eff.applyCredentialPrecedence()

	if eff.APIToken != "" || (eff.User != "" && eff.Password != "") {
		return nil
	}
	// The effective host is not a secret, and naming it tells an operator
	// with several entries which one failed. A cluster that changed endpoint
	// may also sit behind a private CA, which fails next on TLS.
	remedy := "set pve.api_token, or pve.user and pve.password"
	if endpointChanged {
		remedy += ", and pve.ca_cert if that cluster uses a private CA"
	}
	if len(named) == 0 {
		changed := "pve.host"
		baseHost, _ := normalizedEndpoint(base)
		if effHost, _ := normalizedEndpoint(eff); effHost == baseHost {
			changed = "pve.port"
		}
		return fmt.Errorf("config: context entry for host %q changes %s but supplies no credentials; %s",
			eff.Host, changed, remedy)
	}
	specNames := make([]string, len(named))
	for i, key := range named {
		specNames[i] = "pve." + strings.TrimPrefix(key, contextOverridePrefix)
	}
	return fmt.Errorf(
		"config: context entry for host %q sets %s but its credential group is incomplete, and an entry never "+
			"borrows job-level credentials to complete it; %s",
		eff.Host, strings.Join(specNames, " and "), remedy)
}

// ApplyContextOverrides returns a per-request effective CPIConfig built by
// overriding select fields of base with the "pve_"-prefixed keys present in
// extra (the jsonrpc.Context.Extra map decoded from one dispatched JSON-RPC
// request's context object).
//
// This exists to fix a defect: BOSH's cpi-config feature lets a director
// register multiple named CPI entries (e.g. two `type: pve` blocks pointing
// at distinct Proxmox clusters) that are all served by ONE running instance
// of this CPI binary. The director merges each entry's `properties:` hash
// into the JSON-RPC request context per dispatched request, but until this
// function existed nothing read those keys back out — every request ran
// against whichever pve.* config this process happened to be launched with
// (the job-level config), silently executing against the wrong PVE cluster
// for any cpi-config entry other than the one the process booted with.
//
// Only the keys enumerated in contextOverrideFieldOrder are applied — see
// that slice's doc comment for exactly which pve.* properties are in scope
// and why the rest (hooks, otel, retry curves, ...) are deliberately
// excluded from per-request override.
//
// The credential keys (pve_user, pve_password, pve_api_token) and pve_ca_cert
// do not apply one at a time. They travel with the endpoint as a group, so an
// entry that names another host, or names any credential key, authenticates
// with only what it named and never with the job's token or password. See
// resolveOverrideCredentials for the rules and OverrideCredentialGroup for
// how a caller learns which group a request uses.
//
// base must be non-nil. extra may be nil or empty; that is the ordinary case
// (single-CPI deployments, or a cpi-config entry matching the job-level
// cluster) and returns base completely UNCHANGED — the same pointer, not a
// copy — so a request without any pve_* context keys is byte-identical in
// cost and behavior to every release before this function existed.
//
// Returns:
//   - effective: base when no override key is present; otherwise a shallow
//     copy of base with only the matched fields replaced. Every field NOT
//     named in contextOverrideFieldOrder — including every pointer, slice,
//     and map field other than storage-placement policy — is shared with base by the
//     shallow copy: an overridden request inherits ALL other job-level
//     policy (hooks, otel, retry curves, ...) unconditionally. Every pointer,
//     slice, and map field this function may itself write (VerifySSL,
//     Placement, the SDN and ISO flags, the VXLAN peer list, the zone MTU,
//     and the vm_types and disk_types maps) always gets a freshly allocated
//     value rather than a write through base's (see their entries in
//     contextOverrideFields), so base is never mutated by this call.
//   - applied: the sorted list of context keys that were recognized AND
//     successfully applied. Empty (nil) exactly when effective == base.
//   - unknown: the sorted list of "pve_"-prefixed keys present in extra that
//     are not in contextOverrideFieldOrder — i.e. real pve.* properties (or
//     typos) the director forwarded that this function does not support
//     overriding per-request. Returned rather than silently dropped so the
//     caller (handlers.Deps.WithRequestOverrides) can log a Warn naming
//     them; a director's cpi-config properties block commonly carries the
//     FULL pve.* property set for that entry, most of which is intentionally
//     job-level-only, so this is expected in normal operation and must never
//     fail the request.
//   - err: non-nil when a key IN contextOverrideFieldOrder is present with a
//     value that fails type coercion (e.g. pve_port: "not-a-number"), OR when
//     the fully-merged effective config fails the SAME eff.Validate() check
//     the job-level config must pass at CPI startup (see below) — required
//     fields cleared, invalid enum values, port/VMID-range bounds, VMID band
//     overlap, auth entirely absent, etc. This is deliberately hard-fail, not
//     fail-open: silently falling back to the job config on a malformed
//     override would reproduce the exact "request quietly runs against the
//     wrong cluster" defect class this function exists to close, and
//     silently ACCEPTING an invalid effective config (e.g. an inverted VMID
//     range, or a password cleared with no api_token to fall back to) would
//     surface only as a confusing downstream runtime failure — or worse, a
//     data-integrity violation (colliding VMID bands) that never surfaces at
//     all until two VMs collide. On error, effective is nil and must not be
//     used.
//
// Effective-config re-validation: after every matched key is applied, this
// function calls eff.Validate() — the identical validator CPI startup runs
// against the job-level config (internal/config/config.go's
// ValidateWithLogger: required fields, auth presence, enum fields, VMID
// range bounds/ordering/overlap against the disk/template/parker bands, port
// range, and every other startup invariant). eff is always a full copy of
// base, and base already passed this exact check when the process loaded its
// job config, so a validation failure here can only originate from a field
// this call just overrode (individually, or in combination with an inherited
// field it did not touch — e.g. overriding only pve_vmid_range_start above
// an inherited pve_vmid_range_end that was never itself overridden). This
// closes a class of gaps a narrower ad-hoc check (validating vmid range
// order only when BOTH bounds were overridden in the same request) missed
// entirely: single-bound range inversion, VMID band overlap with the
// persistent-disk/stemcell-template/parker bands, vmid_range_start below
// PVE's reserved floor (100), port=0, and an explicit empty-string override
// that clears a required field (host/user/vm_storage/disk_storage/
// network_bridge) or both auth credentials (password AND api_token) at once.
func ApplyContextOverrides(base *CPIConfig, extra map[string]any) (effective *CPIConfig, applied []string, unknown []string, err error) {
	if base == nil {
		return nil, nil, nil, fmt.Errorf("config: ApplyContextOverrides: base must not be nil")
	}
	if len(extra) == 0 {
		return base, nil, nil, nil
	}
	extra = flattenNestedContextOverrides(extra)
	if err := validateStoragePlacementContext(base, base, extra); err != nil {
		return nil, nil, nil, err
	}

	eff := base.CloneStoragePlacement() // isolate mutable storage policy for this request.
	// Start from the pre-defaulting shape of the parker band, so an entry that
	// sets one bound does not silently inherit the other from ApplyDefaults and
	// an entry that sets none can be told apart from one that named the built-in
	// band deliberately. reevaluateParkedDefaultAfterOverrides puts it back.
	eff.clearDefaultedParkerBand()
	var appliedKeys []string

	for _, key := range contextOverrideFieldOrder {
		raw, present := extra[key]
		if !present {
			continue
		}
		apply := contextOverrideFields[key]
		if apply == nil {
			// Invariant violation: every entry in contextOverrideFieldOrder
			// must have a matching contextOverrideFields entry (asserted by
			// TestContextOverrideFieldOrder). Unreachable in a correctly
			// built binary; guarded defensively rather than panicking on a
			// live request.
			return nil, nil, nil, fmt.Errorf("config: context override %q: no handler registered (internal error)", key)
		}
		if applyErr := apply(&eff, raw); applyErr != nil {
			return nil, nil, nil, fmt.Errorf("config: context override %q: %w", key, applyErr)
		}
		appliedKeys = append(appliedKeys, key)
	}

	// Credentials and the CA travel with the endpoint. This must run before
	// any early return below that could hand back eff, and before
	// validation, so an incomplete group fails with its own message.
	if len(appliedKeys) > 0 {
		if credErr := resolveOverrideCredentials(base, &eff, extra); credErr != nil {
			return nil, nil, nil, credErr
		}
		resetEndpointScopedPolicy(base, &eff, extra)
		reapplySDNVNIRangeDefaults(base, &eff, extra)
	}

	var unknownKeys []string
	for k := range extra {
		if !strings.HasPrefix(k, contextOverridePrefix) {
			continue // not a pve.* override candidate at all (e.g. a future non-CPI context key)
		}
		if _, ok := contextOverrideFields[k]; !ok {
			unknownKeys = append(unknownKeys, k)
		}
	}

	sort.Strings(appliedKeys)
	sort.Strings(unknownKeys)

	if len(appliedKeys) == 0 {
		// No supported key matched (extra may still hold unrelated or
		// unknown pve_* keys) — return base unchanged, same byte-identical
		// contract as the empty-extra case above.
		return base, nil, unknownKeys, nil
	}

	// H1 (A13 review): re-run full startup validation against the merged
	// effective config. See the doc comment above for exactly what this
	// closes. A validation failure is returned as a plain error; the caller
	// (handlers.Deps.WithRequestOverrides) wraps it as a non-retriable
	// CloudError, matching every other coercion failure from this function —
	// a manifest/cpi-config authoring bug, never a transient condition.
	// Re-decide the parked default against this entry's bands: an entry that
	// widened another VMID band over the built-in parker band, or that opted out
	// of parking entirely, would otherwise fail validation below for every
	// request routed to it. See reevaluateParkedDefaultAfterOverrides.
	eff.reevaluateParkedDefaultAfterOverrides()

	if err := validateStoragePlacementContext(base, &eff, extra); err != nil {
		return nil, nil, nil, err
	}
	if err := validateSDNContext(base, &eff, extra); err != nil {
		return nil, nil, nil, err
	}

	if validateErr := eff.Validate(); validateErr != nil {
		return nil, nil, nil, fmt.Errorf("config: effective override config failed validation: %w", validateErr)
	}

	return &eff, appliedKeys, unknownKeys, nil
}
