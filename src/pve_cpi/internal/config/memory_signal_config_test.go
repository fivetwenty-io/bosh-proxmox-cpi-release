package config_test

import (
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
)

// ---------------------------------------------------------------------------
// pve.placement.memory_signal tests
// ---------------------------------------------------------------------------

// TestMemorySignalValue_NilReceiver verifies a nil *CPIConfig is safe and
// resolves to the "reserved" default, matching the nil-safe accessor pattern
// used elsewhere (e.g. MaxUtilizationPctValue).
func TestMemorySignalValue_NilReceiver(t *testing.T) {
	t.Parallel()
	var cfg *config.CPIConfig
	if got := cfg.MemorySignalValue(); got != "reserved" {
		t.Errorf("nil receiver: MemorySignalValue() = %q; want %q", got, "reserved")
	}
}

// TestMemorySignalValue_DefaultReserved verifies "reserved" is the default
// when Placement is nil or MemorySignal is empty.
func TestMemorySignalValue_DefaultReserved(t *testing.T) {
	t.Parallel()
	cases := []*config.CPIConfig{
		{},                                     // nil Placement
		{Placement: &config.PlacementConfig{}}, // nil/empty MemorySignal
		{Placement: &config.PlacementConfig{MemorySignal: ""}}, // explicit empty string
	}
	for i, cfg := range cases {
		if got := cfg.MemorySignalValue(); got != "reserved" {
			t.Errorf("case %d: MemorySignalValue() = %q; want %q (default)", i, got, "reserved")
		}
	}
}

// TestMemorySignalValue_ExplicitResident verifies "resident" is honored,
// case-insensitively and with surrounding whitespace trimmed.
func TestMemorySignalValue_ExplicitResident(t *testing.T) {
	t.Parallel()
	cases := []string{"resident", "Resident", "RESIDENT", " resident ", "\tresident\n"}
	for _, v := range cases {
		cfg := &config.CPIConfig{Placement: &config.PlacementConfig{MemorySignal: v}}
		if got := cfg.MemorySignalValue(); got != "resident" {
			t.Errorf("MemorySignal=%q: MemorySignalValue() = %q; want %q", v, got, "resident")
		}
	}
}

// TestMemorySignalValue_ExplicitReserved verifies "reserved" is honored
// case-insensitively (redundant with the default, but confirms an operator
// who explicitly writes "reserved" gets the same result as leaving it unset).
func TestMemorySignalValue_ExplicitReserved(t *testing.T) {
	t.Parallel()
	cases := []string{"reserved", "Reserved", "RESERVED", " reserved "}
	for _, v := range cases {
		cfg := &config.CPIConfig{Placement: &config.PlacementConfig{MemorySignal: v}}
		if got := cfg.MemorySignalValue(); got != "reserved" {
			t.Errorf("MemorySignal=%q: MemorySignalValue() = %q; want %q", v, got, "reserved")
		}
	}
}

// TestMemorySignalValue_JunkFallsBackToReserved verifies an unrecognized
// value silently falls back to the protective "reserved" default rather than
// producing a validation error — a config typo must never block a deploy,
// and it must not silently downgrade to the legacy "resident" behavior
// either.
func TestMemorySignalValue_JunkFallsBackToReserved(t *testing.T) {
	t.Parallel()
	cases := []string{"bogus", "residentt", "reserve", "RESERVE", "on", "off", "true", "1"}
	for _, v := range cases {
		cfg := &config.CPIConfig{Placement: &config.PlacementConfig{MemorySignal: v}}
		if got := cfg.MemorySignalValue(); got != "reserved" {
			t.Errorf("MemorySignal=%q (junk): MemorySignalValue() = %q; want %q (fallback)", v, got, "reserved")
		}
	}
}

// TestMemorySignalValue_JunkDoesNotFailLoad verifies Load() succeeds (no
// validation error) for an unrecognized placement.memory_signal value — the
// fallback happens silently in the accessor, not as a hard config-load
// rejection, unlike storage.max_utilization_mode's typo-catching validation.
func TestMemorySignalValue_JunkDoesNotFailLoad(t *testing.T) {
	t.Parallel()
	cfg, err := mustLoad(t, `{
		"host": "h", "user": "u", "password": "p",
		"vm_storage": "s", "disk_storage": "s", "network_bridge": "br",
		"placement": {"memory_signal": "bogus"}
	}`)
	if err != nil {
		t.Fatalf("Load returned unexpected error for junk memory_signal: %v", err)
	}
	if got := cfg.MemorySignalValue(); got != "reserved" {
		t.Errorf("after JSON round-trip: MemorySignalValue() = %q; want %q (fallback)", got, "reserved")
	}
}

// TestMemorySignalValue_JSONRoundTrip verifies an explicit "resident" value
// survives JSON marshal/unmarshal through the nested "placement" block.
func TestMemorySignalValue_JSONRoundTrip(t *testing.T) {
	t.Parallel()
	cfg, err := mustLoad(t, `{
		"host": "h", "user": "u", "password": "p",
		"vm_storage": "s", "disk_storage": "s", "network_bridge": "br",
		"placement": {"memory_signal": "resident"}
	}`)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if got := cfg.MemorySignalValue(); got != "resident" {
		t.Errorf("after JSON round-trip: MemorySignalValue() = %q; want %q", got, "resident")
	}
}

// TestMemorySignalValue_DefaultOnFreshLoad verifies a config with no
// placement block at all still resolves to "reserved" after a full Load()
// (ApplyDefaults + Validate), confirming the new default applies to
// deployments that never mention placement.memory_signal.
func TestMemorySignalValue_DefaultOnFreshLoad(t *testing.T) {
	t.Parallel()
	cfg, err := mustLoad(t, `{
		"host": "h", "user": "u", "password": "p",
		"vm_storage": "s", "disk_storage": "s", "network_bridge": "br"
	}`)
	if err != nil {
		t.Fatalf("Load returned unexpected error: %v", err)
	}
	if got := cfg.MemorySignalValue(); got != "reserved" {
		t.Errorf("fresh load with no placement block: MemorySignalValue() = %q; want %q", got, "reserved")
	}
}
