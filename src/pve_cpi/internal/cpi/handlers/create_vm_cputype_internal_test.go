package handlers

import (
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
)

// ---------------------------------------------------------------------------
// resolveVMShapeCPUType: precedence matrix
// ---------------------------------------------------------------------------

// TestResolveVMShapeCPUType_NeitherSet_Empty verifies absent everywhere
// resolves to "" on a never-defaulted config (production configs pass through
// ApplyDefaults, which fills CPUType with config.DefaultCPUType first).
func TestResolveVMShapeCPUType_NeitherSet_Empty(t *testing.T) {
	t.Parallel()
	r := buildResolver(t, map[string]any{})
	if got := resolveVMShapeCPUType(r, &config.CPIConfig{}); got != "" {
		t.Errorf("resolveVMShapeCPUType() = %q; want \"\" when unset everywhere", got)
	}
}

// TestResolveVMShapeCPUType_SentinelSuppressesCPUKey verifies the
// "pve-default" sentinel resolves to "" at both layers: as the global config
// value, and as a cloud_properties value overriding a real global default.
func TestResolveVMShapeCPUType_SentinelSuppressesCPUKey(t *testing.T) {
	t.Parallel()
	// Global sentinel → "".
	r1 := buildResolver(t, map[string]any{})
	cfg := &config.CPIConfig{CPUType: config.CPUTypePVEDefault}
	if got := resolveVMShapeCPUType(r1, cfg); got != "" {
		t.Errorf("resolveVMShapeCPUType() = %q; want \"\" for global sentinel", got)
	}
	// Cloud-properties sentinel beats a real global value → "".
	r2 := buildResolver(t, map[string]any{"cpu_type": " " + config.CPUTypePVEDefault + " "})
	cfgReal := &config.CPIConfig{CPUType: "x86-64-v2-AES"}
	if got := resolveVMShapeCPUType(r2, cfgReal); got != "" {
		t.Errorf("resolveVMShapeCPUType() = %q; want \"\" for cloud_properties sentinel", got)
	}
}

// TestResolveVMShapeCPUType_GlobalConfigOnly verifies the global pve.cpu_type
// default applies when cloud_properties.cpu_type is absent.
func TestResolveVMShapeCPUType_GlobalConfigOnly(t *testing.T) {
	t.Parallel()
	r := buildResolver(t, map[string]any{})
	cfg := &config.CPIConfig{CPUType: "x86-64-v2-AES"}
	if got := resolveVMShapeCPUType(r, cfg); got != "x86-64-v2-AES" {
		t.Errorf("resolveVMShapeCPUType() = %q; want x86-64-v2-AES (global default)", got)
	}
}

// TestResolveVMShapeCPUType_CloudPropertiesOverridesGlobal verifies a
// call-level cloud_properties.cpu_type wins over the global default.
func TestResolveVMShapeCPUType_CloudPropertiesOverridesGlobal(t *testing.T) {
	t.Parallel()
	r := buildResolver(t, map[string]any{"cpu_type": "host"})
	cfg := &config.CPIConfig{CPUType: "x86-64-v2-AES"}
	if got := resolveVMShapeCPUType(r, cfg); got != "host" {
		t.Errorf("resolveVMShapeCPUType() = %q; want host (cloud_properties wins)", got)
	}
}

// TestResolveVMShapeCPUType_CloudPropertiesOnly verifies a call-level value
// applies even when the global default is unset.
func TestResolveVMShapeCPUType_CloudPropertiesOnly(t *testing.T) {
	t.Parallel()
	r := buildResolver(t, map[string]any{"cpu_type": "Skylake-Server-noTSX-IBRS"})
	if got := resolveVMShapeCPUType(r, &config.CPIConfig{}); got != "Skylake-Server-noTSX-IBRS" {
		t.Errorf("resolveVMShapeCPUType() = %q; want Skylake-Server-noTSX-IBRS", got)
	}
}

// TestResolveVMShapeCPUType_VMTypeProfileAppliesBelowCallLevel verifies a
// vm_type profile's cloud_properties.cpu_type is honored (the "per-vm_type
// override" the feature is named for) when the per-call cloud_properties do
// not set cpu_type directly, and that an explicit call-level value still
// wins over the profile.
func TestResolveVMShapeCPUType_VMTypeProfileAppliesBelowCallLevel(t *testing.T) {
	t.Parallel()
	cfg := &config.CPIConfig{
		VMTypes: map[string]config.TypeProfile{
			"aes-workers": {CloudProperties: map[string]any{"cpu_type": "x86-64-v2-AES"}},
		},
	}

	// vm_type selector present, no call-level cpu_type → profile value wins.
	r1, err := newLayeredResolver(map[string]any{"vm_type": "aes-workers"}, cfg)
	if err != nil {
		t.Fatalf("newLayeredResolver: %v", err)
	}
	if got := resolveVMShapeCPUType(r1, cfg); got != "x86-64-v2-AES" {
		t.Errorf("resolveVMShapeCPUType() = %q; want x86-64-v2-AES (vm_type profile)", got)
	}

	// Same vm_type, but an explicit call-level cpu_type still wins.
	r2, err := newLayeredResolver(map[string]any{"vm_type": "aes-workers", "cpu_type": "host"}, cfg)
	if err != nil {
		t.Fatalf("newLayeredResolver: %v", err)
	}
	if got := resolveVMShapeCPUType(r2, cfg); got != "host" {
		t.Errorf("resolveVMShapeCPUType() = %q; want host (call-level beats vm_type profile)", got)
	}
}
