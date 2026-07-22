package handlers

import (
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
)

// ---------------------------------------------------------------------------
// resolveVMShapeBalloon: precedence matrix
// ---------------------------------------------------------------------------

// TestResolveVMShapeBalloon_NeitherSet_DefaultsOff verifies absent everywhere
// resolves to "0" (ballooning disabled) even on a never-defaulted config —
// the default lives in BalloonValue, not only in ApplyDefaults, so handler
// tests built from bare CPIConfig literals still get balloon-off.
func TestResolveVMShapeBalloon_NeitherSet_DefaultsOff(t *testing.T) {
	t.Parallel()
	r := buildResolver(t, map[string]any{})
	got, err := resolveVMShapeBalloon(r, &config.CPIConfig{})
	if err != nil {
		t.Fatalf("resolveVMShapeBalloon() error: %v", err)
	}
	if got != "0" {
		t.Errorf("resolveVMShapeBalloon() = %q; want \"0\" when unset everywhere", got)
	}
}

// TestResolveVMShapeBalloon_SentinelSuppressesKey verifies the "pve-default"
// sentinel resolves to "" at both layers: as the global config value, and as
// a cloud_properties value overriding a real global value.
func TestResolveVMShapeBalloon_SentinelSuppressesKey(t *testing.T) {
	t.Parallel()
	// Global sentinel → "".
	r1 := buildResolver(t, map[string]any{})
	cfg := &config.CPIConfig{Balloon: config.BalloonPVEDefault}
	got, err := resolveVMShapeBalloon(r1, cfg)
	if err != nil {
		t.Fatalf("resolveVMShapeBalloon() error: %v", err)
	}
	if got != "" {
		t.Errorf("resolveVMShapeBalloon() = %q; want \"\" for global sentinel", got)
	}
	// Cloud-properties sentinel beats a real global value → "".
	r2 := buildResolver(t, map[string]any{"balloon": " " + config.BalloonPVEDefault + " "})
	cfgReal := &config.CPIConfig{Balloon: "1024"}
	got, err = resolveVMShapeBalloon(r2, cfgReal)
	if err != nil {
		t.Fatalf("resolveVMShapeBalloon() error: %v", err)
	}
	if got != "" {
		t.Errorf("resolveVMShapeBalloon() = %q; want \"\" for cloud_properties sentinel", got)
	}
}

// TestResolveVMShapeBalloon_GlobalConfigOnly verifies the global pve.balloon
// value applies when cloud_properties.balloon is absent.
func TestResolveVMShapeBalloon_GlobalConfigOnly(t *testing.T) {
	t.Parallel()
	r := buildResolver(t, map[string]any{})
	cfg := &config.CPIConfig{Balloon: "2048"}
	got, err := resolveVMShapeBalloon(r, cfg)
	if err != nil {
		t.Fatalf("resolveVMShapeBalloon() error: %v", err)
	}
	if got != "2048" {
		t.Errorf("resolveVMShapeBalloon() = %q; want \"2048\" (global value)", got)
	}
}

// TestResolveVMShapeBalloon_CloudPropertiesOverridesGlobal verifies a
// call-level cloud_properties.balloon wins over the global value, in both
// JSON-number and string forms.
func TestResolveVMShapeBalloon_CloudPropertiesOverridesGlobal(t *testing.T) {
	t.Parallel()
	cfg := &config.CPIConfig{Balloon: "2048"}
	// JSON number (float64 is encoding/json's default number type).
	r1 := buildResolver(t, map[string]any{"balloon": float64(512)})
	got, err := resolveVMShapeBalloon(r1, cfg)
	if err != nil {
		t.Fatalf("resolveVMShapeBalloon() error: %v", err)
	}
	if got != "512" {
		t.Errorf("resolveVMShapeBalloon() = %q; want \"512\" (numeric cloud_properties wins)", got)
	}
	// Numeric string.
	r2 := buildResolver(t, map[string]any{"balloon": "768"})
	got, err = resolveVMShapeBalloon(r2, cfg)
	if err != nil {
		t.Fatalf("resolveVMShapeBalloon() error: %v", err)
	}
	if got != "768" {
		t.Errorf("resolveVMShapeBalloon() = %q; want \"768\" (string cloud_properties wins)", got)
	}
	// Explicit zero re-disables against an enabling global.
	r3 := buildResolver(t, map[string]any{"balloon": float64(0)})
	got, err = resolveVMShapeBalloon(r3, cfg)
	if err != nil {
		t.Fatalf("resolveVMShapeBalloon() error: %v", err)
	}
	if got != "0" {
		t.Errorf("resolveVMShapeBalloon() = %q; want \"0\" (explicit zero wins)", got)
	}
}

// TestResolveVMShapeBalloon_VMTypeProfileAppliesBelowCallLevel verifies a
// vm_type profile's cloud_properties.balloon is honored when the per-call
// cloud_properties do not set balloon, and an explicit call-level value still
// wins over the profile.
func TestResolveVMShapeBalloon_VMTypeProfileAppliesBelowCallLevel(t *testing.T) {
	t.Parallel()
	cfg := &config.CPIConfig{
		VMTypes: map[string]config.TypeProfile{
			"balloonable": {CloudProperties: map[string]any{"balloon": float64(1024)}},
		},
	}

	// vm_type selector present, no call-level balloon → profile value wins.
	r1, err := newLayeredResolver(map[string]any{"vm_type": "balloonable"}, cfg)
	if err != nil {
		t.Fatalf("newLayeredResolver: %v", err)
	}
	got, rerr := resolveVMShapeBalloon(r1, cfg)
	if rerr != nil {
		t.Fatalf("resolveVMShapeBalloon() error: %v", rerr)
	}
	if got != "1024" {
		t.Errorf("resolveVMShapeBalloon() = %q; want \"1024\" (vm_type profile)", got)
	}

	// Same vm_type, but an explicit call-level balloon still wins.
	r2, err := newLayeredResolver(map[string]any{"vm_type": "balloonable", "balloon": float64(256)}, cfg)
	if err != nil {
		t.Fatalf("newLayeredResolver: %v", err)
	}
	got, rerr = resolveVMShapeBalloon(r2, cfg)
	if rerr != nil {
		t.Fatalf("resolveVMShapeBalloon() error: %v", rerr)
	}
	if got != "256" {
		t.Errorf("resolveVMShapeBalloon() = %q; want \"256\" (call-level beats vm_type profile)", got)
	}
}

// TestResolveVMShapeBalloon_InvalidValue_Errors verifies non-numeric,
// non-sentinel cloud_properties.balloon values produce an error naming the
// knob (fail fast, before any PVE API call).
func TestResolveVMShapeBalloon_InvalidValue_Errors(t *testing.T) {
	t.Parallel()
	for _, bad := range []any{"lots", "-5", float64(-1), true} {
		r := buildResolver(t, map[string]any{"balloon": bad})
		if _, err := resolveVMShapeBalloon(r, &config.CPIConfig{}); err == nil {
			t.Errorf("balloon=%v: resolveVMShapeBalloon() succeeded; want error", bad)
		}
	}
}
