package config_test

import (
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
)

// ---------------------------------------------------------------------------
// VNI band defaults and validation for vlan/qinq zone types
// ---------------------------------------------------------------------------

// TestSDNVNIBand_VlanZoneType_DefaultsToVlanSafeBand verifies that when the
// operator selects a 802.1Q-capped zone type (vlan, qinq) and leaves the VNI
// band unset, ApplyDefaults picks a band inside the 4094 VLAN ID cap instead
// of the VXLAN-oriented 5000..5999 default (which would make every
// auto-allocated tag fail at create_network time).
func TestSDNVNIBand_VlanZoneType_DefaultsToVlanSafeBand(t *testing.T) {
	t.Parallel()
	for _, zt := range []string{"vlan", "qinq"} {
		cfg := &config.CPIConfig{SDNZoneType: zt}
		cfg.ApplyDefaults()
		if cfg.SDNVNIRangeStart != 2000 || cfg.SDNVNIRangeEnd != 2999 {
			t.Errorf("zone type %q: band = %d..%d; want 2000..2999 (vlan-safe default)",
				zt, cfg.SDNVNIRangeStart, cfg.SDNVNIRangeEnd)
		}
	}
}

// TestSDNVNIBand_UncappedZoneTypes_KeepDefaultBand verifies vxlan/evpn/simple
// (and the unset-defaults-to-vxlan case) keep the historic 5000..5999 band.
func TestSDNVNIBand_UncappedZoneTypes_KeepDefaultBand(t *testing.T) {
	t.Parallel()
	for _, zt := range []string{"", "vxlan", "evpn", "simple"} {
		cfg := &config.CPIConfig{SDNZoneType: zt}
		cfg.ApplyDefaults()
		if cfg.SDNVNIRangeStart != 5000 || cfg.SDNVNIRangeEnd != 5999 {
			t.Errorf("zone type %q: band = %d..%d; want 5000..5999 (historic default)",
				zt, cfg.SDNVNIRangeStart, cfg.SDNVNIRangeEnd)
		}
	}
}

// TestSDNVNIBand_VlanZoneType_ExplicitBandWins verifies an operator-set band
// within the cap is honored unchanged for vlan zones.
func TestSDNVNIBand_VlanZoneType_ExplicitBandWins(t *testing.T) {
	t.Parallel()
	cfg := &config.CPIConfig{SDNZoneType: "vlan", SDNVNIRangeStart: 100, SDNVNIRangeEnd: 199}
	cfg.ApplyDefaults()
	if cfg.SDNVNIRangeStart != 100 || cfg.SDNVNIRangeEnd != 199 {
		t.Errorf("band = %d..%d; want explicit 100..199 preserved",
			cfg.SDNVNIRangeStart, cfg.SDNVNIRangeEnd)
	}
}

// TestSDNVNIBand_VlanZoneType_BandOutsideCap_Rejected verifies an explicit
// band beyond the 4094 802.1Q cap fails validation at Load time for vlan and
// qinq zone types — today this would only surface later, inside
// create_network, after the Director has already committed to the config.
func TestSDNVNIBand_VlanZoneType_BandOutsideCap_Rejected(t *testing.T) {
	t.Parallel()
	for _, zt := range []string{"vlan", "qinq"} {
		_, err := mustLoad(t, `{
			"host": "h", "user": "u", "password": "p",
			"vm_storage": "s", "disk_storage": "s", "network_bridge": "br",
			"network_mode": "sdn",
			"sdn_zone_type": "`+zt+`",
			"sdn_vni_range_start": 5000, "sdn_vni_range_end": 5999
		}`)
		if err == nil {
			t.Errorf("zone type %q with band 5000..5999: Load succeeded; want 4094-cap validation error", zt)
			continue
		}
		if !strings.Contains(err.Error(), "4094") {
			t.Errorf("zone type %q: error %q does not mention the 4094 cap", zt, err)
		}
	}
}

// TestSDNVNIBand_VlanZoneType_SingleFieldBand_FailsLoudly verifies an
// operator setting only ONE of the band fields on a vlan zone gets a clear
// load-time error, not a silently mixed band: start-only pairs with the
// 5999 default end (over the cap); end-only pairs with the 5000 default
// start (start > end).
func TestSDNVNIBand_VlanZoneType_SingleFieldBand_FailsLoudly(t *testing.T) {
	t.Parallel()
	// start-only: end fills to 5999 → 4094-cap violation. network_mode: sdn is
	// explicit here — the 4094 cap depends on the effective zone type, which
	// only resolves on the reachable SDN path (the cap check is
	// deliberately gated on mode, unlike the other SDN field checks).
	_, err := mustLoad(t, `{
		"host": "h", "user": "u", "password": "p",
		"vm_storage": "s", "disk_storage": "s", "network_bridge": "br",
		"network_mode": "sdn",
		"sdn_zone_type": "vlan",
		"sdn_vni_range_start": 100
	}`)
	if err == nil {
		t.Error("start-only band on a vlan zone: Load succeeded; want 4094-cap error")
	} else if !strings.Contains(err.Error(), "4094") {
		t.Errorf("start-only band: error %q does not mention the 4094 cap", err)
	}
	// end-only: start fills to 5000 → start > end bounds violation. This
	// ordering check is NOT mode-gated (it fires whenever the operator sets
	// either band field, in every mode), so network_mode is left at its
	// bridge default here to also cover that cross-mode behavior.
	_, err = mustLoad(t, `{
		"host": "h", "user": "u", "password": "p",
		"vm_storage": "s", "disk_storage": "s", "network_bridge": "br",
		"sdn_zone_type": "vlan",
		"sdn_vni_range_end": 3000
	}`)
	if err == nil {
		t.Error("end-only band on a vlan zone: Load succeeded; want bounds error")
	} else if !strings.Contains(err.Error(), "sdn_vni_range") {
		t.Errorf("end-only band: error %q does not name sdn_vni_range", err)
	}
}

// TestSDNVNIBand_BridgeMode_CapNotEnforced verifies the 4094-cap check does
// not fire under network_mode bridge, where the SDN path is unreachable and
// sdn_zone_type is inert (mirroring the zone-type enum validation, which is
// also skipped in bridge mode). Guards against rejecting configs migrated
// from SDN that kept stale sdn_* fields.
func TestSDNVNIBand_BridgeMode_CapNotEnforced(t *testing.T) {
	t.Parallel()
	_, err := mustLoad(t, `{
		"host": "h", "user": "u", "password": "p",
		"vm_storage": "s", "disk_storage": "s", "network_bridge": "br",
		"network_mode": "bridge",
		"sdn_zone_type": "vlan",
		"sdn_vni_range_start": 5000, "sdn_vni_range_end": 5999
	}`)
	if err != nil {
		t.Errorf("bridge mode with stale vlan sdn fields: Load returned error: %v", err)
	}
}

// TestSDNVNIBand_VlanZoneType_BandEndAtCap_Accepted verifies the boundary:
// an explicit band ending exactly at 4094 is valid for vlan zones.
func TestSDNVNIBand_VlanZoneType_BandEndAtCap_Accepted(t *testing.T) {
	t.Parallel()
	_, err := mustLoad(t, `{
		"host": "h", "user": "u", "password": "p",
		"vm_storage": "s", "disk_storage": "s", "network_bridge": "br",
		"network_mode": "sdn",
		"sdn_zone_type": "vlan",
		"sdn_vni_range_start": 4000, "sdn_vni_range_end": 4094
	}`)
	if err != nil {
		t.Errorf("vlan band ending exactly at 4094: Load returned error: %v", err)
	}
}

// TestSDNVNIBand_VxlanZoneType_HighBandAccepted verifies the same high band
// stays valid for vxlan zones (24-bit VNI space).
func TestSDNVNIBand_VxlanZoneType_HighBandAccepted(t *testing.T) {
	t.Parallel()
	_, err := mustLoad(t, `{
		"host": "h", "user": "u", "password": "p",
		"vm_storage": "s", "disk_storage": "s", "network_bridge": "br",
		"network_mode": "sdn",
		"sdn_zone_type": "vxlan",
		"sdn_vni_range_start": 5000, "sdn_vni_range_end": 5999
	}`)
	if err != nil {
		t.Errorf("vxlan with band 5000..5999: Load returned error: %v", err)
	}
}

// TestSDNVNIBand_VlanZoneType_LoadDefaultsWithinCap verifies the whole Load
// pipeline (defaults + validation) succeeds for a vlan zone with no explicit
// band — the vlan-safe default band must itself pass the cap validation.
func TestSDNVNIBand_VlanZoneType_LoadDefaultsWithinCap(t *testing.T) {
	t.Parallel()
	cfg, err := mustLoad(t, `{
		"host": "h", "user": "u", "password": "p",
		"vm_storage": "s", "disk_storage": "s", "network_bridge": "br",
		"network_mode": "sdn",
		"sdn_zone_type": "vlan"
	}`)
	if err != nil {
		t.Fatalf("Load returned unexpected error: %v", err)
	}
	if cfg.SDNVNIRangeStart != 2000 || cfg.SDNVNIRangeEnd != 2999 {
		t.Errorf("band = %d..%d; want 2000..2999", cfg.SDNVNIRangeStart, cfg.SDNVNIRangeEnd)
	}
}
