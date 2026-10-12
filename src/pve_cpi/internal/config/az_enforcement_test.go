package config_test

import (
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
)

func TestAZEnforcementStrict_Accessor(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		cfg  *config.CPIConfig
		want bool
	}{
		{"nil config", nil, false},
		{"nil placement", &config.CPIConfig{}, false},
		{"unset key defaults to warn", &config.CPIConfig{Placement: &config.PlacementConfig{}}, false},
		{"warn", &config.CPIConfig{Placement: &config.PlacementConfig{AZEnforcement: "warn"}}, false},
		{"strict", &config.CPIConfig{Placement: &config.PlacementConfig{AZEnforcement: "strict"}}, true},
		{"strict is case and space tolerant", &config.CPIConfig{Placement: &config.PlacementConfig{AZEnforcement: " Strict "}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.cfg.AZEnforcementStrict(); got != tc.want {
				t.Errorf("AZEnforcementStrict() = %v, want %v", got, tc.want)
			}
		})
	}
}

func azEnforcementJSON(value string) string {
	return `{
		"host":"h","user":"u","password":"p",
		"vm_storage":"s","disk_storage":"s","network_bridge":"br",
		"placement":{"az_map":{"az1":["pve01","pve02"]}` + value + `}
	}`
}

func TestValidate_Placement_AZEnforcement(t *testing.T) {
	t.Parallel()
	for _, v := range []string{"", `,"az_enforcement":"warn"`, `,"az_enforcement":"strict"`} {
		if _, err := mustLoad(t, azEnforcementJSON(v)); err != nil {
			t.Errorf("value %q: unexpected error: %v", v, err)
		}
	}
	_, err := mustLoad(t, azEnforcementJSON(`,"az_enforcement":"stricct"`))
	if err == nil {
		t.Fatal("expected error for an unknown az_enforcement value")
	}
	if !strings.Contains(err.Error(), "placement.az_enforcement") || !strings.Contains(err.Error(), "stricct") {
		t.Errorf("error %q should name the key and the bad value", err.Error())
	}
}

func TestApplyContextOverrides_Placement_AZEnforcement(t *testing.T) {
	t.Parallel()
	base := validBaseCfg()
	base.ApplyDefaults()

	eff, _, unknown, err := config.ApplyContextOverrides(base, map[string]any{
		"pve_placement": map[string]any{
			"az_map":         map[string]any{"z3": []any{"pve-az2-1"}},
			"az_enforcement": "strict",
		},
	})
	if err != nil {
		t.Fatalf("entry placement block with az_enforcement must decode: %v", err)
	}
	if len(unknown) != 0 {
		t.Errorf("unknown = %#v, want empty", unknown)
	}
	if !eff.AZEnforcementStrict() {
		t.Error("az_enforcement from the entry must be visible through the accessor")
	}
	if base.AZEnforcementStrict() {
		t.Error("the entry override must not leak into the base config")
	}
}
