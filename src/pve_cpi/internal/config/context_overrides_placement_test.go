package config_test

import (
	"reflect"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
)

// ---------------------------------------------------------------------------
// pve_placement per-request override.
//
// Placement was previously excluded from the override registry, which made the
// three HA features (placement.dlb, anti_affinity.use_ha_rules,
// pin_az_via_ha_rules) unreachable from a cpi-config entry — i.e. unreachable
// in exactly the multi-cluster deployments that have more than one cluster to
// fail over within. Worse, with cloud_properties.availability_zone set, a
// request fell back to the job-level (usually empty) az_map and create_vm
// hard-failed with "availability_zone %q is not defined in placement.az_map".
// ---------------------------------------------------------------------------

// placementOverrideMap is a representative cpi-config entry placement block:
// an AZ map plus the two HA-registration knobs an entry would realistically set.
func placementOverrideMap() map[string]any {
	return map[string]any{
		"az_map": map[string]any{
			"z3": []any{"pve-az2-1", "pve-az2-2"},
		},
		"pin_az_via_ha_rules": true,
		"anti_affinity": map[string]any{
			"enabled":      true,
			"use_ha_rules": true,
			"strict":       false,
		},
		"dlb": map[string]any{
			"enabled": true,
		},
	}
}

func TestApplyContextOverrides_Placement_FlatKey(t *testing.T) {
	t.Parallel()
	base := validBaseCfg()
	base.ApplyDefaults()

	eff, applied, unknown, err := config.ApplyContextOverrides(base, map[string]any{
		"pve_placement": placementOverrideMap(),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(unknown) != 0 {
		t.Errorf("unknown = %#v, want empty — pve_placement must be a supported override", unknown)
	}
	if len(applied) != 1 || applied[0] != "pve_placement" {
		t.Fatalf("applied = %#v, want [pve_placement]", applied)
	}
	if eff == base {
		t.Fatal("expected a distinct effective config")
	}
	if eff.Placement == nil {
		t.Fatal("effective Placement is nil")
	}
	if got := eff.Placement.AZMap["z3"]; !reflect.DeepEqual(got, []string{"pve-az2-1", "pve-az2-2"}) {
		t.Errorf("az_map[z3] = %#v, want the entry's node list", got)
	}
	if !eff.HANodeAffinityPinEnabled() {
		t.Error("pin_az_via_ha_rules from the entry must be visible through the accessor")
	}
	if !eff.AntiAffinityUseHaRulesEnabled() {
		t.Error("anti_affinity.use_ha_rules from the entry must be visible through the accessor")
	}
	if !eff.DLBExplicitlyEnabled() {
		t.Error("dlb.enabled from the entry must be visible through the accessor")
	}
}

// TestApplyContextOverrides_Placement_NestedKey covers the shape a director
// actually sends: cpi-config `properties: {pve: {placement: {...}}}` arrives as
// a nested context.pve map, folded to pve_placement by
// flattenNestedContextOverrides.
func TestApplyContextOverrides_Placement_NestedKey(t *testing.T) {
	t.Parallel()
	base := validBaseCfg()
	base.ApplyDefaults()

	eff, applied, _, err := config.ApplyContextOverrides(base, map[string]any{
		"pve": map[string]any{
			"placement": placementOverrideMap(),
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(applied) != 1 || applied[0] != "pve_placement" {
		t.Fatalf("applied = %#v, want [pve_placement]", applied)
	}
	if eff.Placement == nil || eff.Placement.AZMap["z3"] == nil {
		t.Fatalf("nested context.pve.placement did not reach the effective config: %+v", eff.Placement)
	}
}

// TestApplyContextOverrides_Placement_DoesNotMutateBase is the critical
// isolation property: the effective config is a SHALLOW copy of base, so the
// override must install a fresh *PlacementConfig rather than writing through
// the pointer base shares. Mutating base would leak one entry's az_map into
// every other cpi-config entry served by this process.
func TestApplyContextOverrides_Placement_DoesNotMutateBase(t *testing.T) {
	t.Parallel()
	base := validBaseCfg()
	base.Placement = &config.PlacementConfig{
		AZMap: map[string][]string{"z1": {"pve-az1-1"}},
	}
	base.ApplyDefaults()
	basePlacement := base.Placement

	eff, _, _, err := config.ApplyContextOverrides(base, map[string]any{
		"pve_placement": placementOverrideMap(),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if base.Placement != basePlacement {
		t.Error("base.Placement pointer was replaced — ApplyContextOverrides must never mutate base")
	}
	if _, leaked := base.Placement.AZMap["z3"]; leaked {
		t.Errorf("the entry's AZ leaked into the job-level az_map: %#v", base.Placement.AZMap)
	}
	if base.Placement.AZMap["z1"] == nil {
		t.Error("job-level az_map lost its own entry")
	}
	if eff.Placement == basePlacement {
		t.Error("effective Placement must be a distinct block, not base's")
	}
}

// TestApplyContextOverrides_Placement_ReplacesWholeBlock pins the semantics:
// an entry's placement block REPLACES the job-level one rather than merging
// into it, so each entry is self-contained and an operator reading one entry
// sees that entry's complete placement policy.
func TestApplyContextOverrides_Placement_ReplacesWholeBlock(t *testing.T) {
	t.Parallel()
	base := validBaseCfg()
	base.Placement = &config.PlacementConfig{
		AZMap:           map[string][]string{"z1": {"pve-az1-1"}},
		AZFallbackOrder: []string{"z1"},
	}
	base.ApplyDefaults()

	eff, _, _, err := config.ApplyContextOverrides(base, map[string]any{
		"pve_placement": map[string]any{
			"az_map": map[string]any{"z3": []any{"pve-az2-1"}},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, stillThere := eff.Placement.AZMap["z1"]; stillThere {
		t.Error("job-level az_map entries must NOT merge into an entry that defines its own placement block")
	}
	if len(eff.Placement.AZFallbackOrder) != 0 {
		t.Errorf("job-level az_fallback_order must not survive a whole-block replace, got %#v", eff.Placement.AZFallbackOrder)
	}
}

// TestApplyContextOverrides_Placement_WeightsDefaulted verifies the overridden
// block still receives the same weight defaults ApplyDefaults gives the
// job-level block, so an entry that sets a partial weights block does not end
// up scoring on zeroed axes.
func TestApplyContextOverrides_Placement_WeightsDefaulted(t *testing.T) {
	t.Parallel()
	base := validBaseCfg()
	base.ApplyDefaults()

	eff, _, _, err := config.ApplyContextOverrides(base, map[string]any{
		"pve_placement": map[string]any{
			"weights": map[string]any{"mem": 2.0},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	w := eff.Placement.Weights
	if w == nil {
		t.Fatal("weights block missing")
	}
	if w.Mem != 2.0 {
		t.Errorf("explicit mem weight = %v, want 2.0", w.Mem)
	}
	if w.Storage == 0 || w.CPU == 0 || w.GuestCount == 0 {
		t.Errorf("unset weight axes must be defaulted, got %+v", *w)
	}
}

// TestApplyContextOverrides_Placement_InvalidBlockHardFails proves the
// override runs through the same validator the job-level config must pass, so
// a malformed entry fails loudly instead of silently falling back to the
// job-level placement (the failure mode this whole mechanism exists to close).
func TestApplyContextOverrides_Placement_InvalidBlockHardFails(t *testing.T) {
	t.Parallel()
	base := validBaseCfg()
	base.ApplyDefaults()

	cases := []struct {
		name string
		val  any
	}{
		{"az with an empty node list", map[string]any{"az_map": map[string]any{"z3": []any{}}}},
		{"az with an empty node name", map[string]any{"az_map": map[string]any{"z3": []any{""}}}},
		{"negative scoring weight", map[string]any{"weights": map[string]any{"mem": -1.0}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			eff, _, _, err := config.ApplyContextOverrides(base, map[string]any{"pve_placement": tc.val})
			if err == nil {
				t.Fatalf("expected a hard error, got effective placement %+v", eff.Placement)
			}
			if eff != nil {
				t.Error("effective config must be nil on error")
			}
		})
	}
}

// TestApplyContextOverrides_Placement_WrongType rejects a value that is not a
// placement object at all, rather than coercing it into an empty block that
// would silently disable every placement feature for that entry.
func TestApplyContextOverrides_Placement_WrongType(t *testing.T) {
	t.Parallel()
	base := validBaseCfg()
	base.ApplyDefaults()

	for _, val := range []any{"placement", 42, true, []any{"a"}} {
		_, _, _, err := config.ApplyContextOverrides(base, map[string]any{"pve_placement": val})
		if err == nil {
			t.Errorf("pve_placement = %#v (%T): expected a coercion error", val, val)
		}
	}
}

// TestApplyContextOverrides_Placement_UnknownFieldRejected keeps a typo inside
// the block from being silently dropped. An operator who writes
// "pin_az_via_ha_rule" (singular) would otherwise get a block that parses
// cleanly and pins nothing.
func TestApplyContextOverrides_Placement_UnknownFieldRejected(t *testing.T) {
	t.Parallel()
	base := validBaseCfg()
	base.ApplyDefaults()

	_, _, _, err := config.ApplyContextOverrides(base, map[string]any{
		"pve_placement": map[string]any{"pin_az_via_ha_rule": true},
	})
	if err == nil {
		t.Fatal("expected a typo'd placement field to be rejected")
	}
}

// TestApplyContextOverrides_Placement_ExplicitNullClearsBlock lets an entry
// opt out of an inherited job-level placement policy explicitly.
func TestApplyContextOverrides_Placement_ExplicitNullClearsBlock(t *testing.T) {
	t.Parallel()
	base := validBaseCfg()
	base.Placement = &config.PlacementConfig{AZMap: map[string][]string{"z1": {"pve-az1-1"}}}
	base.ApplyDefaults()

	eff, applied, _, err := config.ApplyContextOverrides(base, map[string]any{
		"pve_placement": nil,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(applied) != 1 {
		t.Fatalf("applied = %#v, want [pve_placement]", applied)
	}
	if eff.Placement != nil {
		t.Errorf("explicit null must clear the block, got %+v", eff.Placement)
	}
	if base.Placement == nil {
		t.Error("base must keep its own placement block")
	}
}
