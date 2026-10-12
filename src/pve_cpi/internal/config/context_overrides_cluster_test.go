package config_test

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
)

// -----------------------------------------------------------------------
// Per-cluster override keys: SDN, pools, stemcell, ISO layout, the D9
// reset, and the vm_types and disk_types profile maps
// -----------------------------------------------------------------------

// clusterTestJob returns a loaded job config, defaults applied, that
// authenticates with a token on credTestJobHost. Tests that change the
// endpoint pair it with clusterTestEntry so the credential group is complete.
func clusterTestJob(t *testing.T) *config.CPIConfig {
	t.Helper()
	base := credTestTokenJob()
	base.SDNZoneType = ""
	base.NetworkMode = ""
	base.ApplyDefaults()
	if err := base.Validate(); err != nil {
		t.Fatalf("fixture job config does not validate: %v", err)
	}
	return base
}

// clusterTestEntry returns an entry that moves the request to
// credTestEntryHost with its own token, plus the given extra keys.
func clusterTestEntry(extra map[string]any) map[string]any {
	entry := map[string]any{"pve_host": credTestEntryHost, "pve_api_token": credTestEntryToken}
	for k, v := range extra {
		entry[k] = v
	}
	return entry
}

// clusterTestSDNJob returns a job config that runs auto-managed SDN with an
// explicit VXLAN peer list. An entry on another cluster that inherited it
// would create its zone there with these peers, so the other cluster's
// tunnels would point at the job cluster's nodes.
func clusterTestSDNJob(t *testing.T) *config.CPIConfig {
	t.Helper()
	base := clusterTestJob(t)
	base.NetworkMode = config.NetworkModeSDN
	base.SDNVxlanPeers = []string{"192.0.2.11", "192.0.2.12"}
	return base
}

func TestApplyContextOverrides_ClusterKeys_Coercion(t *testing.T) {
	t.Parallel()
	mtu := func(eff *config.CPIConfig) any {
		if eff.SDNZoneMTU == nil {
			return nil
		}
		return *eff.SDNZoneMTU
	}
	derefBool := func(p *bool) any {
		if p == nil {
			return nil
		}
		return *p
	}
	for _, tc := range []struct {
		name  string
		key   string
		value any
		read  func(*config.CPIConfig) any
		want  any
	}{
		{"network_mode string", "pve_network_mode", "sdn", func(c *config.CPIConfig) any { return c.NetworkMode }, "sdn"},
		{"sdn_zone string", "pve_sdn_zone", "zone-b", func(c *config.CPIConfig) any { return c.SDNZone }, "zone-b"},
		{"sdn_zone_type string", "pve_sdn_zone_type", "evpn", func(c *config.CPIConfig) any { return c.SDNZoneType }, "evpn"},
		{"sdn_auto_manage_zone bool", "pve_sdn_auto_manage_zone", false,
			func(c *config.CPIConfig) any { return derefBool(c.SDNAutoManageZone) }, false},
		{"sdn_auto_manage_zone string bool", "pve_sdn_auto_manage_zone", "false",
			func(c *config.CPIConfig) any { return derefBool(c.SDNAutoManageZone) }, false},
		{"sdn_vxlan_peers list", "pve_sdn_vxlan_peers", []any{"10.0.0.1", "10.0.0.2"},
			func(c *config.CPIConfig) any { return c.SDNVxlanPeers }, []string{"10.0.0.1", "10.0.0.2"}},
		{"sdn_vxlan_peers empty list derives", "pve_sdn_vxlan_peers", []any{},
			func(c *config.CPIConfig) any { return c.SDNVxlanPeers }, []string(nil)},
		{"sdn_vxlan_peers null derives", "pve_sdn_vxlan_peers", nil,
			func(c *config.CPIConfig) any { return c.SDNVxlanPeers }, []string(nil)},
		{"sdn_vni_range_start number", "pve_sdn_vni_range_start", float64(5100),
			func(c *config.CPIConfig) any { return c.SDNVNIRangeStart }, 5100},
		{"sdn_vni_range_end numeric string", "pve_sdn_vni_range_end", "5800",
			func(c *config.CPIConfig) any { return c.SDNVNIRangeEnd }, 5800},
		{"sdn_zone_mtu number", "pve_sdn_zone_mtu", float64(1400), mtu, int64(1400)},
		{"sdn_zone_mtu zero is unset", "pve_sdn_zone_mtu", float64(0), mtu, nil},
		{"sdn_zone_mtu null is unset", "pve_sdn_zone_mtu", nil, mtu, nil},
		{"vm_pool string", "pve_vm_pool", "az2-vms", func(c *config.CPIConfig) any { return c.VMPool }, "az2-vms"},
		{"vm_pool empty opts out", "pve_vm_pool", "", func(c *config.CPIConfig) any { return c.VMPool }, ""},
		{"stemcell_template_pool string", "pve_stemcell_template_pool", "az2-templates",
			func(c *config.CPIConfig) any { return c.StemcellTemplatePool }, "az2-templates"},
		{"stemcell_strategy string", "pve_stemcell_strategy", "import",
			func(c *config.CPIConfig) any { return c.StemcellStrategy }, "import"},
		{"stemcell_template_node string", "pve_stemcell_template_node", "pve-b-1",
			func(c *config.CPIConfig) any { return c.StemcellTemplateNode }, "pve-b-1"},
		{"require_shared_iso_for_ha bool", "pve_require_shared_iso_for_ha", true,
			func(c *config.CPIConfig) any { return derefBool(c.RequireSharedISOForHA) }, true},
		{"iso_storage_follow_vm_storage bool", "pve_iso_storage_follow_vm_storage", false,
			func(c *config.CPIConfig) any { return derefBool(c.ISOStorageFollowVMStorage) }, false},
		{"destroy_unreferenced_disks bool", "pve_destroy_unreferenced_disks", true,
			func(c *config.CPIConfig) any { return c.DestroyUnreferencedDisks }, true},
		{"destroy_unreferenced_disks string bool", "pve_destroy_unreferenced_disks", "1",
			func(c *config.CPIConfig) any { return c.DestroyUnreferencedDisks }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			base := clusterTestJob(t)
			eff, applied, unknown, err := config.ApplyContextOverrides(base, map[string]any{tc.key: tc.value})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !slices.Contains(applied, tc.key) || len(unknown) != 0 {
				t.Fatalf("applied=%v unknown=%v; want %s applied and nothing unknown", applied, unknown, tc.key)
			}
			if got := tc.read(eff); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("%s = %#v, want %#v", tc.key, got, tc.want)
			}
		})
	}
}

func TestApplyContextOverrides_ClusterKeys_RejectWrongTypes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		key   string
		value any
	}{
		{"pve_network_mode", 3.0},
		{"pve_sdn_zone", true},
		{"pve_sdn_zone_type", []any{"vxlan"}},
		{"pve_sdn_auto_manage_zone", "maybe"},
		{"pve_sdn_vxlan_peers", "10.0.0.1,10.0.0.2"},
		{"pve_sdn_vxlan_peers", []any{"10.0.0.1", 7.0}},
		{"pve_sdn_vni_range_start", 5100.5},
		{"pve_sdn_vni_range_end", "lots"},
		{"pve_sdn_zone_mtu", "jumbo"},
		{"pve_vm_pool", 1.0},
		{"pve_stemcell_template_pool", false},
		{"pve_stemcell_strategy", map[string]any{}},
		{"pve_stemcell_template_node", 2.0},
		{"pve_require_shared_iso_for_ha", 1.0},
		{"pve_iso_storage_follow_vm_storage", "sometimes"},
		{"pve_destroy_unreferenced_disks", nil},
		{"pve_vm_types", []any{"small"}},
		{"pve_disk_types", "fast"},
	} {
		_, _, _, err := config.ApplyContextOverrides(clusterTestJob(t), map[string]any{tc.key: tc.value})
		if err == nil {
			t.Errorf("%s=%#v: expected a coercion error, got nil", tc.key, tc.value)
			continue
		}
		if !strings.Contains(err.Error(), tc.key) {
			t.Errorf("%s=%#v: error %q does not name the key", tc.key, tc.value, err.Error())
		}
	}
}

// TestApplyContextOverrides_ClusterKeys_ValueRulesFromValidate pins that the
// new keys go through the same startup validation as the job config, as
// their neighbours do, rather than a looser field-local check.
func TestApplyContextOverrides_ClusterKeys_ValueRulesFromValidate(t *testing.T) {
	t.Parallel()
	for name, extra := range map[string]map[string]any{
		"network_mode enum":         {"pve_network_mode": "overlay"},
		"sdn_zone_type enum":        {"pve_network_mode": "sdn", "pve_sdn_zone_type": "geneve"},
		"vxlan peer not an IP":      {"pve_sdn_vxlan_peers": []any{"peer-one"}},
		"vni band inverted":         {"pve_sdn_vni_range_start": float64(5900), "pve_sdn_vni_range_end": float64(5100)},
		"zone mtu out of range":     {"pve_sdn_zone_mtu": float64(100)},
		"stemcell_strategy enum":    {"pve_stemcell_strategy": "copy"},
		"vm_pool charset":           {"pve_vm_pool": "a/b"},
		"pools equal":               {"pve_vm_pool": "same", "pve_stemcell_template_pool": "same"},
		"template pool lock prefix": {"pve_stemcell_template_pool": "bosh-lock-x"},
	} {
		if _, _, _, err := config.ApplyContextOverrides(clusterTestJob(t), extra); err == nil {
			t.Errorf("%s: expected the effective config to fail validation", name)
		}
	}
}

func TestApplyContextOverrides_ClusterKeys_BaseUnmutated(t *testing.T) {
	t.Parallel()
	base := clusterTestJob(t)
	autoManage := true
	base.SDNAutoManageZone = &autoManage
	base.SDNVxlanPeers = []string{"192.0.2.1"}
	jobMTU := int64(1450)
	base.SDNZoneMTU = &jobMTU
	follow := true
	base.ISOStorageFollowVMStorage = &follow
	base.VMTypes = map[string]config.TypeProfile{"small": {CloudProperties: map[string]any{"cpu": 1}}}
	base.DiskTypes = map[string]config.TypeProfile{"fast": {CloudProperties: map[string]any{"vm_storage": "ssd-a"}}}

	eff := func() *config.CPIConfig {
		eff, _, _, err := config.ApplyContextOverrides(base, map[string]any{
			"pve_sdn_auto_manage_zone":          false,
			"pve_sdn_vxlan_peers":               []any{"198.51.100.1"},
			"pve_sdn_zone_mtu":                  float64(9000),
			"pve_iso_storage_follow_vm_storage": false,
			"pve_require_shared_iso_for_ha":     true,
			"pve_vm_types":                      map[string]any{"large": map[string]any{"cloud_properties": map[string]any{"cpu": 8.0}}},
			"pve_disk_types":                    map[string]any{},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		return eff
	}()
	eff.SDNVxlanPeers[0] = "203.0.113.9"
	eff.VMTypes["extra"] = config.TypeProfile{}

	if !*base.SDNAutoManageZone || !*base.ISOStorageFollowVMStorage || base.RequireSharedISOForHA != nil {
		t.Error("an override wrote through a pointer it shares with the job config")
	}
	if *base.SDNZoneMTU != 1450 || base.SDNVxlanPeers[0] != "192.0.2.1" {
		t.Error("an override changed the job config's MTU or peer list")
	}
	if len(base.VMTypes) != 1 || len(base.DiskTypes) != 1 {
		t.Error("an override changed the job config's profile maps")
	}
}

// TestApplyContextOverrides_VMTypes_WholeMapReplace pins the placement-style
// replace. An entry's map is the entry's whole map, so a job profile the
// entry does not repeat is gone, and a profile key the entry drops cannot
// survive through a merge.
func TestApplyContextOverrides_VMTypes_WholeMapReplace(t *testing.T) {
	t.Parallel()
	newBase := func() *config.CPIConfig {
		base := clusterTestJob(t)
		base.VMTypes = map[string]config.TypeProfile{
			"small": {CloudProperties: map[string]any{"cpu": 1.0, "vm_storage": "job-ssd", "target_node": "pve-a-1"}},
			"large": {CloudProperties: map[string]any{"cpu": 8.0}},
		}
		return base
	}

	t.Run("entry map replaces the job map", func(t *testing.T) {
		t.Parallel()
		base := newBase()
		eff, _ := credTestMustApply(t, base, map[string]any{
			"pve_vm_types": map[string]any{
				"small": map[string]any{"cloud_properties": map[string]any{"cpu": 2.0}},
			},
		})
		want := map[string]config.TypeProfile{"small": {CloudProperties: map[string]any{"cpu": 2.0}}}
		if !reflect.DeepEqual(eff.VMTypes, want) {
			t.Errorf("eff.VMTypes = %#v, want %#v (no job profile or profile key may survive)", eff.VMTypes, want)
		}
		if len(base.VMTypes) != 2 || base.VMTypes["small"].CloudProperties["vm_storage"] != "job-ssd" {
			t.Error("the job config's vm_types changed")
		}
	})
	t.Run("null clears the map", func(t *testing.T) {
		t.Parallel()
		eff, _ := credTestMustApply(t, newBase(), map[string]any{"pve_vm_types": nil})
		if eff.VMTypes != nil {
			t.Errorf("eff.VMTypes = %#v, want nil", eff.VMTypes)
		}
	})
	t.Run("absent key inherits", func(t *testing.T) {
		t.Parallel()
		base := newBase()
		eff, _ := credTestMustApply(t, base, map[string]any{"pve_vm_pool": "az2"})
		if !reflect.DeepEqual(eff.VMTypes, base.VMTypes) {
			t.Error("an entry that omits pve_vm_types must keep the job's profiles")
		}
	})
	t.Run("sibling fields are ignored and recorded", func(t *testing.T) {
		t.Parallel()
		base := newBase()
		eff, _ := credTestMustApply(t, base, map[string]any{
			"pve_vm_types": map[string]any{
				"small": map[string]any{"cpu": 2.0},
				"large": map[string]any{
					"name": "large", "description": "eight cores",
					"cloud_properties": map[string]any{"cpu": 8.0},
				},
			},
			"pve_disk_types": map[string]any{"bulk": map[string]any{"name": "bulk"}},
		})
		want := map[string]config.TypeProfile{
			"small": {},
			"large": {CloudProperties: map[string]any{"cpu": 8.0}},
		}
		if !reflect.DeepEqual(eff.VMTypes, want) {
			t.Errorf("eff.VMTypes = %#v, want %#v", eff.VMTypes, want)
		}
		wantIgnored := []string{
			"pve.vm_types.large.description", "pve.vm_types.large.name", "pve.vm_types.small.cpu",
			"pve.disk_types.bulk.name",
		}
		if got := eff.IgnoredProfileFields(); !reflect.DeepEqual(got, wantIgnored) {
			t.Errorf("IgnoredProfileFields = %v, want %v", got, wantIgnored)
		}
		if got := base.IgnoredProfileFields(); got != nil {
			t.Errorf("the job config recorded ignored fields %v", got)
		}
	})
	t.Run("a malformed profile or cloud_properties still fails", func(t *testing.T) {
		t.Parallel()
		for _, profile := range []any{
			"small",
			map[string]any{"cloud_properties": "cpu=2"},
			map[string]any{"cloud_properties": []any{"cpu"}},
		} {
			_, _, _, err := config.ApplyContextOverrides(newBase(), map[string]any{
				"pve_vm_types": map[string]any{"small": profile},
			})
			if err == nil || !strings.Contains(err.Error(), "pve_vm_types") || !strings.Contains(err.Error(), `"small"`) {
				t.Errorf("profile %#v: err = %v, want a pve_vm_types error naming the profile", profile, err)
			}
		}
	})
	t.Run("disk_types replaces the same way", func(t *testing.T) {
		t.Parallel()
		base := newBase()
		base.DiskTypes = map[string]config.TypeProfile{"fast": {CloudProperties: map[string]any{"vm_storage": "job-ssd"}}}
		eff, _ := credTestMustApply(t, base, map[string]any{
			"pve_disk_types": map[string]any{"bulk": map[string]any{"cloud_properties": map[string]any{"vm_storage": "hdd-b"}}},
		})
		if _, kept := eff.DiskTypes["fast"]; kept || len(eff.DiskTypes) != 1 {
			t.Errorf("eff.DiskTypes = %#v, want only the entry's bulk profile", eff.DiskTypes)
		}
	})
}

// TestApplyContextOverrides_DestroyUnreferencedDisks_ResetOnEndpointChange
// pins design decision D9. A job-level true frees every unreferenced
// same-VMID volume on the storage a deleted VM used, which on storage shared
// with another cluster frees that cluster's live disks, so it never follows a
// request to another endpoint unless the entry names it.
func TestApplyContextOverrides_DestroyUnreferencedDisks_ResetOnEndpointChange(t *testing.T) {
	t.Parallel()
	newBase := func() *config.CPIConfig {
		base := clusterTestJob(t)
		base.DestroyUnreferencedDisks = true
		return base
	}
	for _, tc := range []struct {
		name      string
		extra     map[string]any
		want      bool
		wantReset bool
	}{
		{"host change resets", clusterTestEntry(nil), false, true},
		{"port change resets", map[string]any{"pve_port": float64(8443), "pve_api_token": credTestEntryToken}, false, true},
		{"host change keeps an entry's own true", clusterTestEntry(map[string]any{"pve_destroy_unreferenced_disks": true}), true, false},
		{"host change keeps an entry's own false", clusterTestEntry(map[string]any{"pve_destroy_unreferenced_disks": false}), false, false},
		{"same endpoint inherits", map[string]any{"pve_vm_storage": "other"}, true, false},
		{"same endpoint in another spelling inherits", map[string]any{"pve_host": strings.ToUpper(credTestJobHost) + "."}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			base := newBase()
			eff, applied := credTestMustApply(t, base, tc.extra)
			if eff.DestroyUnreferencedDisks != tc.want {
				t.Errorf("eff.DestroyUnreferencedDisks = %t, want %t", eff.DestroyUnreferencedDisks, tc.want)
			}
			if got := config.DestroyUnreferencedDisksReset(base, eff, applied); got != tc.wantReset {
				t.Errorf("DestroyUnreferencedDisksReset = %t, want %t", got, tc.wantReset)
			}
			if !base.DestroyUnreferencedDisks {
				t.Error("the reset wrote through to the job config")
			}
		})
	}
}

// TestApplyContextOverrides_SDN_InheritedSettingsFailOnEndpointChange pins
// the rule that keeps the job cluster's SDN fabric off another cluster. With
// network_mode sdn and auto-manage on, create_network writes the zone onto
// whichever cluster the request reaches, so an entry on another endpoint must
// name every job SDN setting it would otherwise inherit.
func TestApplyContextOverrides_SDN_InheritedSettingsFailOnEndpointChange(t *testing.T) {
	t.Parallel()
	_, _, _, err := config.ApplyContextOverrides(clusterTestSDNJob(t), clusterTestEntry(nil))
	if err == nil {
		t.Fatal("expected an entry on another endpoint that inherits the job's VXLAN peers to fail")
	}
	for _, want := range []string{"pve.sdn_vxlan_peers", credTestEntryHost, "pve.sdn_auto_manage_zone: false"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err.Error(), want)
		}
	}
	for _, peer := range []string{"192.0.2.11", "192.0.2.12"} {
		if strings.Contains(err.Error(), peer) {
			t.Errorf("error %q carries a setting value; it must name property keys only", err.Error())
		}
	}
	assertNoSecretInError(t, err)
}

func TestApplyContextOverrides_SDN_EveryNonDefaultSettingCounts(t *testing.T) {
	t.Parallel()
	mtu := int64(1400)
	for _, tc := range []struct {
		key string
		set func(*config.CPIConfig)
	}{
		{"pve.sdn_zone", func(c *config.CPIConfig) { c.SDNZone = "job-zone" }},
		{"pve.sdn_zone_type", func(c *config.CPIConfig) { c.SDNZoneType = "evpn" }},
		{"pve.sdn_vxlan_peers", func(c *config.CPIConfig) { c.SDNVxlanPeers = []string{"192.0.2.1"} }},
		{"pve.sdn_vni_range_start", func(c *config.CPIConfig) { c.SDNVNIRangeStart = 5100 }},
		{"pve.sdn_vni_range_end", func(c *config.CPIConfig) { c.SDNVNIRangeEnd = 5800 }},
		{"pve.sdn_zone_mtu", func(c *config.CPIConfig) { c.SDNZoneMTU = &mtu }},
	} {
		base := clusterTestJob(t)
		base.NetworkMode = config.NetworkModeSDN
		tc.set(base)
		_, _, _, err := config.ApplyContextOverrides(base, clusterTestEntry(nil))
		if err == nil || !strings.Contains(err.Error(), tc.key) {
			t.Errorf("%s: err = %v, want the SDN inheritance error naming the key", tc.key, err)
		}
		flat := "pve_" + strings.TrimPrefix(tc.key, "pve.")
		named := clusterTestEntry(map[string]any{flat: sdnEntryValue(flat)})
		if _, _, _, err := config.ApplyContextOverrides(base, named); err != nil {
			t.Errorf("%s: an entry that names the key must pass, got %v", tc.key, err)
		}
	}
}

// sdnEntryValue returns a valid entry value for one SDN key.
func sdnEntryValue(key string) any {
	switch key {
	case "pve_sdn_zone":
		return "entry-zone"
	case "pve_sdn_zone_type":
		return "vxlan"
	case "pve_sdn_vxlan_peers":
		return []any{"198.51.100.1"}
	case "pve_sdn_vni_range_start":
		return float64(5000)
	case "pve_sdn_vni_range_end":
		return float64(5999)
	case "pve_sdn_zone_mtu":
		return nil
	}
	return nil
}

func TestApplyContextOverrides_SDN_RuleScope(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		base  func(*config.CPIConfig)
		extra map[string]any
	}{
		{"entry names the peers", nil, clusterTestEntry(map[string]any{"pve_sdn_vxlan_peers": []any{"198.51.100.1"}})},
		{"entry names empty peers to derive them", nil, clusterTestEntry(map[string]any{"pve_sdn_vxlan_peers": []any{}})},
		{"entry turns auto-manage off", nil, clusterTestEntry(map[string]any{"pve_sdn_auto_manage_zone": false})},
		{"entry uses bridge mode", nil, clusterTestEntry(map[string]any{"pve_network_mode": "bridge"})},
		{"job turns auto-manage off", func(c *config.CPIConfig) {
			off := false
			c.SDNAutoManageZone = &off
		}, clusterTestEntry(nil)},
		{"job uses bridge mode", func(c *config.CPIConfig) { c.NetworkMode = config.NetworkModeBridge }, clusterTestEntry(nil)},
		{"job SDN settings all default", func(c *config.CPIConfig) { c.SDNVxlanPeers = nil }, clusterTestEntry(nil)},
		{"same endpoint", nil, map[string]any{"pve_vm_storage": "other"}},
		{"same endpoint with the entry's own credentials", nil, map[string]any{"pve_api_token": credTestEntryToken}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			base := clusterTestSDNJob(t)
			if tc.base != nil {
				tc.base(base)
			}
			if _, _, _, err := config.ApplyContextOverrides(base, tc.extra); err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

// TestApplyContextOverrides_SDN_AutoModeWithZone covers auto mode, which
// sends every managed network that names no zone or bridge down the SDN path
// as soon as a zone resolves. A job in auto mode with a zone, or an entry
// that switches to auto while it inherits the job's zone, would otherwise
// create that zone on the other cluster with the job cluster's peers.
func TestApplyContextOverrides_SDN_AutoModeWithZone(t *testing.T) {
	t.Parallel()
	t.Run("job runs auto with a zone", func(t *testing.T) {
		t.Parallel()
		base := clusterTestSDNJob(t)
		base.NetworkMode = config.NetworkModeAuto
		base.SDNZone = "bosh-a"
		_, _, _, err := config.ApplyContextOverrides(base, clusterTestEntry(nil))
		if err == nil {
			t.Fatal("expected an auto-mode entry on another endpoint that inherits the job zone and peers to fail")
		}
		for _, want := range []string{"pve.sdn_zone", "pve.sdn_vxlan_peers", `network_mode "auto"`} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not contain %q", err.Error(), want)
			}
		}
	})
	t.Run("entry switches to auto and inherits the job zone", func(t *testing.T) {
		t.Parallel()
		base := clusterTestSDNJob(t)
		base.SDNZone = "bosh-a"
		_, _, _, err := config.ApplyContextOverrides(base, clusterTestEntry(map[string]any{"pve_network_mode": "auto"}))
		if err == nil || !strings.Contains(err.Error(), "pve.sdn_zone") {
			t.Errorf("err = %v, want the SDN inheritance error naming pve.sdn_zone", err)
		}
	})
	t.Run("auto mode without a zone stays on bridges", func(t *testing.T) {
		t.Parallel()
		base := clusterTestSDNJob(t)
		base.NetworkMode = config.NetworkModeAuto
		if _, _, _, err := config.ApplyContextOverrides(base, clusterTestEntry(nil)); err != nil {
			t.Errorf("unexpected error for auto mode with no zone: %v", err)
		}
	})
	t.Run("entry names its own zone and peers", func(t *testing.T) {
		t.Parallel()
		base := clusterTestSDNJob(t)
		base.NetworkMode = config.NetworkModeAuto
		base.SDNZone = "bosh-a"
		entry := clusterTestEntry(map[string]any{"pve_sdn_zone": "bosh-b", "pve_sdn_vxlan_peers": []any{}})
		if _, _, _, err := config.ApplyContextOverrides(base, entry); err != nil {
			t.Errorf("unexpected error for an entry that names its zone and peers: %v", err)
		}
	})
}

// TestApplyContextOverrides_SDN_EntryTurnsOnSDN covers an entry that itself
// selects sdn mode on another cluster while the job runs bridges with a
// leftover peer list: the effective mode decides, so the entry must name it.
func TestApplyContextOverrides_SDN_EntryTurnsOnSDN(t *testing.T) {
	t.Parallel()
	base := clusterTestSDNJob(t)
	base.NetworkMode = config.NetworkModeBridge
	_, _, _, err := config.ApplyContextOverrides(base, clusterTestEntry(map[string]any{"pve_network_mode": "sdn"}))
	if err == nil || !strings.Contains(err.Error(), "pve.sdn_vxlan_peers") {
		t.Errorf("err = %v, want the SDN inheritance error", err)
	}
}

// TestApplyContextOverrides_SDN_VNIBandFollowsEntryZoneType pins that an
// entry gets the band default a job config of its own zone type gets, since
// ApplyDefaults never runs on an override config.
func TestApplyContextOverrides_SDN_VNIBandFollowsEntryZoneType(t *testing.T) {
	t.Parallel()
	t.Run("defaulted job band follows a vlan entry", func(t *testing.T) {
		t.Parallel()
		base := clusterTestJob(t)
		base.NetworkMode = config.NetworkModeSDN
		eff, _ := credTestMustApply(t, base, map[string]any{"pve_sdn_zone_type": "vlan"})
		if eff.SDNVNIRangeStart != 2000 || eff.SDNVNIRangeEnd != 2999 {
			t.Errorf("band = %d..%d, want the vlan default 2000..2999", eff.SDNVNIRangeStart, eff.SDNVNIRangeEnd)
		}
		if base.SDNVNIRangeStart != 5000 || base.SDNVNIRangeEnd != 5999 {
			t.Error("the job config's band changed")
		}
	})
	t.Run("explicit job band is kept", func(t *testing.T) {
		t.Parallel()
		base := clusterTestJob(t)
		base.SDNVNIRangeStart, base.SDNVNIRangeEnd = 100, 900
		eff, _ := credTestMustApply(t, base, map[string]any{"pve_sdn_zone_type": "vlan"})
		if eff.SDNVNIRangeStart != 100 || eff.SDNVNIRangeEnd != 900 {
			t.Errorf("band = %d..%d, want the job's explicit 100..900", eff.SDNVNIRangeStart, eff.SDNVNIRangeEnd)
		}
	})
	t.Run("named zero bounds take the default", func(t *testing.T) {
		t.Parallel()
		eff, _ := credTestMustApply(t, clusterTestJob(t), map[string]any{
			"pve_sdn_vni_range_start": float64(0), "pve_sdn_vni_range_end": float64(0),
		})
		if eff.SDNVNIRangeStart != 5000 || eff.SDNVNIRangeEnd != 5999 {
			t.Errorf("band = %d..%d, want 5000..5999", eff.SDNVNIRangeStart, eff.SDNVNIRangeEnd)
		}
	})
}

func TestInheritedClusterNames(t *testing.T) {
	t.Parallel()
	newBase := func() *config.CPIConfig {
		base := clusterTestJob(t)
		base.Node = "pve-a-1"
		base.StemcellTemplateNode = "pve-a-2"
		base.VMStorage = "vm-a"
		base.DiskStorage = "disk-a"
		base.StemcellStorage = "stem-a"
		// ApplyDefaults already captured the ISO policy, so the fixture pins
		// it the way a loaded config holds an operator pin.
		base.SetISOStoragePolicy("iso-a")
		base.NetworkBridge = "vmbr0"
		return base
	}

	t.Run("another endpoint lists every inherited name", func(t *testing.T) {
		t.Parallel()
		base := newBase()
		eff, applied := credTestMustApply(t, base, clusterTestEntry(nil))
		want := []string{
			"pve.node=pve-a-1", "pve.stemcell_template_node=pve-a-2", "pve.vm_storage=vm-a",
			"pve.disk_storage=disk-a", "pve.stemcell_storage=stem-a", "pve.iso_storage=iso-a",
			"pve.network_bridge=vmbr0",
		}
		if got := config.InheritedClusterNames(base, eff, applied); !reflect.DeepEqual(got, want) {
			t.Errorf("InheritedClusterNames = %v, want %v", got, want)
		}
	})
	t.Run("names the entry sets are not listed", func(t *testing.T) {
		t.Parallel()
		base := newBase()
		base.SetISOStoragePolicy("local")
		eff, applied := credTestMustApply(t, base, clusterTestEntry(map[string]any{
			"pve_node": "pve-b-1", "pve_vm_storage": "vm-b", "pve_disk_storage": "disk-b",
			"pve_stemcell_storage": "stem-b", "pve_stemcell_template_node": "pve-b-1",
		}))
		want := []string{"pve.network_bridge=vmbr0"}
		if got := config.InheritedClusterNames(base, eff, applied); !reflect.DeepEqual(got, want) {
			t.Errorf("InheritedClusterNames = %v, want %v (the local ISO default is not a cluster name)", got, want)
		}
	})
	t.Run("inherited az_map nodes and profile maps are listed", func(t *testing.T) {
		t.Parallel()
		base := newBase()
		base.SetISOStoragePolicy("local")
		base.Placement = &config.PlacementConfig{AZMap: map[string][]string{
			"z2": {"pve-a-3"}, "z1": {"pve-a-1", "pve-a-2"},
		}}
		base.VMTypes = map[string]config.TypeProfile{"small": {CloudProperties: map[string]any{"cpu": 1.0}}}
		base.DiskTypes = map[string]config.TypeProfile{"fast": {CloudProperties: map[string]any{"vm_storage": "ssd-a"}}}
		entry := clusterTestEntry(map[string]any{
			"pve_node": "pve-b-1", "pve_vm_storage": "vm-b", "pve_disk_storage": "disk-b",
			"pve_stemcell_storage": "stem-b", "pve_stemcell_template_node": "pve-b-1", "pve_network_bridge": "vmbr1",
		})
		eff, applied := credTestMustApply(t, base, entry)
		want := []string{
			"pve.placement.az_map.z1=pve-a-1", "pve.placement.az_map.z1=pve-a-2", "pve.placement.az_map.z2=pve-a-3",
			"pve.vm_types", "pve.disk_types",
		}
		if got := config.InheritedClusterNames(base, eff, applied); !reflect.DeepEqual(got, want) {
			t.Errorf("InheritedClusterNames = %v, want %v", got, want)
		}

		entry["pve_placement"] = map[string]any{"az_map": map[string]any{"z1": []any{"pve-b-1"}}}
		entry["pve_vm_types"] = nil
		entry["pve_disk_types"] = map[string]any{}
		eff, applied = credTestMustApply(t, base, entry)
		if got := config.InheritedClusterNames(base, eff, applied); got != nil {
			t.Errorf("InheritedClusterNames = %v, want nil once the entry names placement and both profile maps", got)
		}
	})
	t.Run("the job endpoint lists nothing", func(t *testing.T) {
		t.Parallel()
		base := newBase()
		eff, applied := credTestMustApply(t, base, map[string]any{"pve_vm_pool": "az1"})
		if got := config.InheritedClusterNames(base, eff, applied); got != nil {
			t.Errorf("InheritedClusterNames = %v, want nil on the job endpoint", got)
		}
	})
}

// TestApplyContextOverrides_ClusterKeys_SingleClusterUnchanged pins the
// no-wedge rule. A job config that turns on every new behavior's trigger,
// auto-managed SDN with peers and destroy_unreferenced_disks, sees no change
// without a cpi_config and none from an entry on its own endpoint.
func TestApplyContextOverrides_ClusterKeys_SingleClusterUnchanged(t *testing.T) {
	t.Parallel()
	base := clusterTestSDNJob(t)
	base.DestroyUnreferencedDisks = true

	eff, applied, unknown, err := config.ApplyContextOverrides(base, nil)
	if err != nil || eff != base || applied != nil || unknown != nil {
		t.Fatalf("no context must return the job config itself: eff==base %t, applied %v, unknown %v, err %v",
			eff == base, applied, unknown, err)
	}

	eff, applied = credTestMustApply(t, base, map[string]any{"pve_vm_storage": "other"})
	if !eff.DestroyUnreferencedDisks || !reflect.DeepEqual(eff.SDNVxlanPeers, base.SDNVxlanPeers) {
		t.Error("an entry on the job endpoint must inherit destroy_unreferenced_disks and the SDN settings")
	}
	if names := config.InheritedClusterNames(base, eff, applied); names != nil {
		t.Errorf("an entry on the job endpoint must not warn about inherited names, got %v", names)
	}
}
