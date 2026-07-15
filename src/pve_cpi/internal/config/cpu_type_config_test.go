package config_test

import (
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
)

// ---------------------------------------------------------------------------
// pve.cpu_type tests
// ---------------------------------------------------------------------------

// TestCPUTypeValue_DefaultEmpty verifies nil receiver and never-defaulted
// zero-value config both resolve to "" (ApplyDefaults is what fills the
// x86-64-v2-AES default; these configs never went through it).
func TestCPUTypeValue_DefaultEmpty(t *testing.T) {
	t.Parallel()
	var nilCfg *config.CPIConfig
	if got := nilCfg.CPUTypeValue(); got != "" {
		t.Errorf("nil receiver: CPUTypeValue() = %q; want \"\"", got)
	}
	if got := (&config.CPIConfig{}).CPUTypeValue(); got != "" {
		t.Errorf("zero-value config: CPUTypeValue() = %q; want \"\"", got)
	}
}

// TestCPUTypeValue_PVEDefaultSentinel verifies the "pve-default" sentinel
// resolves to "" so callers write no cpu key and PVE keeps its kvm64 default.
func TestCPUTypeValue_PVEDefaultSentinel(t *testing.T) {
	t.Parallel()
	cfg := &config.CPIConfig{CPUType: "  " + config.CPUTypePVEDefault + "  "}
	if got := cfg.CPUTypeValue(); got != "" {
		t.Errorf("sentinel: CPUTypeValue() = %q; want \"\"", got)
	}
}

// TestCPUTypeValue_ExplicitValue verifies a set value is returned as-is.
func TestCPUTypeValue_ExplicitValue(t *testing.T) {
	t.Parallel()
	cfg := &config.CPIConfig{CPUType: "x86-64-v2-AES"}
	if got := cfg.CPUTypeValue(); got != "x86-64-v2-AES" {
		t.Errorf("CPUTypeValue() = %q; want x86-64-v2-AES", got)
	}
}

// TestCPUTypeValue_TrimsWhitespace verifies surrounding whitespace is trimmed.
func TestCPUTypeValue_TrimsWhitespace(t *testing.T) {
	t.Parallel()
	cfg := &config.CPIConfig{CPUType: "  host  "}
	if got := cfg.CPUTypeValue(); got != "host" {
		t.Errorf("CPUTypeValue() = %q; want trimmed \"host\"", got)
	}
}

// TestCPUType_JSONRoundTrip verifies the knob survives JSON marshal/unmarshal.
func TestCPUType_JSONRoundTrip(t *testing.T) {
	t.Parallel()
	cfg, err := mustLoad(t, `{
		"host": "h", "user": "u", "password": "p",
		"vm_storage": "s", "disk_storage": "s", "network_bridge": "br",
		"cpu_type": "x86-64-v2-AES"
	}`)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if got := cfg.CPUTypeValue(); got != "x86-64-v2-AES" {
		t.Errorf("after JSON round-trip: CPUTypeValue() = %q; want x86-64-v2-AES", got)
	}
}

// TestCPUType_AbsentFromJSON_DefaultsToV2AES verifies a manifest that never
// sets cpu_type loads without error and resolves to the built-in
// x86-64-v2-AES default (PVE's own create-wizard default; keeps AES-NI).
func TestCPUType_AbsentFromJSON_DefaultsToV2AES(t *testing.T) {
	t.Parallel()
	cfg, err := mustLoad(t, `{
		"host": "h", "user": "u", "password": "p",
		"vm_storage": "s", "disk_storage": "s", "network_bridge": "br"
	}`)
	if err != nil {
		t.Fatalf("Load returned unexpected error: %v", err)
	}
	if got := cfg.CPUTypeValue(); got != config.DefaultCPUType {
		t.Errorf("CPUTypeValue() = %q; want %q when cpu_type is absent from the manifest", got, config.DefaultCPUType)
	}
}

// TestCPUType_SentinelFromJSON_WritesNoCPUKey verifies the "pve-default"
// sentinel survives Load/ApplyDefaults and resolves to "" (legacy behavior:
// no cpu key written, PVE falls back to kvm64).
func TestCPUType_SentinelFromJSON_WritesNoCPUKey(t *testing.T) {
	t.Parallel()
	cfg, err := mustLoad(t, `{
		"host": "h", "user": "u", "password": "p",
		"vm_storage": "s", "disk_storage": "s", "network_bridge": "br",
		"cpu_type": "pve-default"
	}`)
	if err != nil {
		t.Fatalf("Load returned unexpected error: %v", err)
	}
	if got := cfg.CPUTypeValue(); got != "" {
		t.Errorf("CPUTypeValue() = %q; want \"\" for the pve-default sentinel", got)
	}
}
