package config_test

import (
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
)

func TestBridgeCheckMode_Accessor(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		cfg  *config.CPIConfig
		want string
	}{
		{"nil config defaults to warn", nil, "warn"},
		{"nil placement defaults to warn", &config.CPIConfig{}, "warn"},
		{"unset key defaults to warn", &config.CPIConfig{Placement: &config.PlacementConfig{}}, "warn"},
		{"off", &config.CPIConfig{Placement: &config.PlacementConfig{BridgeCheck: "off"}}, "off"},
		{"warn", &config.CPIConfig{Placement: &config.PlacementConfig{BridgeCheck: "warn"}}, "warn"},
		{"enforce", &config.CPIConfig{Placement: &config.PlacementConfig{BridgeCheck: "enforce"}}, "enforce"},
		{"case and space tolerant", &config.CPIConfig{Placement: &config.PlacementConfig{BridgeCheck: " Enforce "}}, "enforce"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.cfg.BridgeCheckMode(); got != tc.want {
				t.Errorf("BridgeCheckMode() = %q, want %q", got, tc.want)
			}
		})
	}
}

func bridgeCheckJSON(value string) string {
	return `{
		"host":"h","user":"u","password":"p",
		"vm_storage":"s","disk_storage":"s","network_bridge":"br",
		"placement":{"az_map":{"az1":["pve01","pve02"]}` + value + `}
	}`
}

func TestValidate_Placement_BridgeCheck(t *testing.T) {
	t.Parallel()
	for _, v := range []string{"", `,"bridge_check":"off"`, `,"bridge_check":"warn"`, `,"bridge_check":"enforce"`} {
		if _, err := mustLoad(t, bridgeCheckJSON(v)); err != nil {
			t.Errorf("value %q: unexpected error: %v", v, err)
		}
	}
	_, err := mustLoad(t, bridgeCheckJSON(`,"bridge_check":"strict"`))
	if err == nil {
		t.Fatal("expected error for an unknown bridge_check value")
	}
	if !strings.Contains(err.Error(), "placement.bridge_check") || !strings.Contains(err.Error(), "strict") {
		t.Errorf("error %q should name the key and the bad value", err.Error())
	}
}

func TestApplyContextOverrides_Placement_BridgeCheck(t *testing.T) {
	t.Parallel()
	base := validBaseCfg()
	base.ApplyDefaults()

	eff, _, unknown, err := config.ApplyContextOverrides(base, map[string]any{
		"pve_placement": map[string]any{
			"az_map":       map[string]any{"z3": []any{"pve-az2-1"}},
			"bridge_check": "enforce",
		},
	})
	if err != nil {
		t.Fatalf("entry placement block with bridge_check must decode: %v", err)
	}
	if len(unknown) != 0 {
		t.Errorf("unknown = %#v, want empty", unknown)
	}
	if eff.BridgeCheckMode() != "enforce" {
		t.Errorf("entry bridge_check = %q, want enforce", eff.BridgeCheckMode())
	}
	if base.BridgeCheckMode() != "warn" {
		t.Errorf("the entry override leaked into the base config: %q", base.BridgeCheckMode())
	}
}
