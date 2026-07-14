package config_test

import (
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
)

// ---------------------------------------------------------------------------
// pve.cpu_type tests
// ---------------------------------------------------------------------------

// TestCPUTypeValue_DefaultEmpty verifies the gate is disabled (empty string)
// by default: nil receiver and zero-value config both resolve to "".
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

// TestCPUType_AbsentFromJSON_NoErrorNoBehaviorChange verifies a manifest that
// never sets cpu_type loads without error and resolves to disabled — the
// additive, zero-behavior-change contract.
func TestCPUType_AbsentFromJSON_NoErrorNoBehaviorChange(t *testing.T) {
	t.Parallel()
	cfg, err := mustLoad(t, `{
		"host": "h", "user": "u", "password": "p",
		"vm_storage": "s", "disk_storage": "s", "network_bridge": "br"
	}`)
	if err != nil {
		t.Fatalf("Load returned unexpected error: %v", err)
	}
	if got := cfg.CPUTypeValue(); got != "" {
		t.Errorf("CPUTypeValue() = %q; want \"\" when cpu_type is absent from the manifest", got)
	}
}
