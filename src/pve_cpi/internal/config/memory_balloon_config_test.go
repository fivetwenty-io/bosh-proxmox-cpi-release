package config_test

import (
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
)

// ---------------------------------------------------------------------------
// pve.balloon tests
// ---------------------------------------------------------------------------

// TestBalloonValue_DefaultOff verifies nil receiver and never-defaulted
// zero-value config both resolve to "0" (ballooning disabled) — the default
// must hold even for configs that never went through ApplyDefaults, because
// handler tests build bare CPIConfig literals.
func TestBalloonValue_DefaultOff(t *testing.T) {
	t.Parallel()
	var nilCfg *config.CPIConfig
	if got := nilCfg.BalloonValue(); got != "0" {
		t.Errorf("nil receiver: BalloonValue() = %q; want \"0\"", got)
	}
	if got := (&config.CPIConfig{}).BalloonValue(); got != "0" {
		t.Errorf("zero-value config: BalloonValue() = %q; want \"0\"", got)
	}
}

// TestBalloonValue_PVEDefaultSentinel verifies the "pve-default" sentinel
// resolves to "" so callers write no balloon key and PVE keeps its own
// default (balloon device enabled, balloon = memory).
func TestBalloonValue_PVEDefaultSentinel(t *testing.T) {
	t.Parallel()
	cfg := &config.CPIConfig{Balloon: "  " + config.BalloonPVEDefault + "  "}
	if got := cfg.BalloonValue(); got != "" {
		t.Errorf("sentinel: BalloonValue() = %q; want \"\"", got)
	}
}

// TestBalloonValue_ExplicitValue verifies a set MiB value is returned trimmed.
func TestBalloonValue_ExplicitValue(t *testing.T) {
	t.Parallel()
	cfg := &config.CPIConfig{Balloon: "  1024  "}
	if got := cfg.BalloonValue(); got != "1024" {
		t.Errorf("BalloonValue() = %q; want \"1024\"", got)
	}
}

// TestBalloon_ApplyDefaults_FillsZero verifies ApplyDefaults fills an empty
// Balloon with DefaultBalloon ("0" — ballooning disabled).
func TestBalloon_ApplyDefaults_FillsZero(t *testing.T) {
	t.Parallel()
	cfg := &config.CPIConfig{}
	cfg.ApplyDefaults()
	if cfg.Balloon != config.DefaultBalloon {
		t.Errorf("after ApplyDefaults: Balloon = %q; want %q", cfg.Balloon, config.DefaultBalloon)
	}
	if config.DefaultBalloon != "0" {
		t.Errorf("DefaultBalloon = %q; want \"0\"", config.DefaultBalloon)
	}
}

// TestBalloon_JSONRoundTrip verifies the knob survives Load (which applies
// defaults and validates).
func TestBalloon_JSONRoundTrip(t *testing.T) {
	t.Parallel()
	cfg, err := mustLoad(t, `{
		"host": "h", "user": "u", "password": "p",
		"vm_storage": "s", "disk_storage": "s", "network_bridge": "br",
		"balloon": "512"
	}`)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if got := cfg.BalloonValue(); got != "512" {
		t.Errorf("after JSON round-trip: BalloonValue() = %q; want \"512\"", got)
	}
}

// TestBalloon_AbsentFromJSON_DefaultsOff verifies a manifest that never sets
// balloon loads without error and resolves to "0" (ballooning disabled).
func TestBalloon_AbsentFromJSON_DefaultsOff(t *testing.T) {
	t.Parallel()
	cfg, err := mustLoad(t, `{
		"host": "h", "user": "u", "password": "p",
		"vm_storage": "s", "disk_storage": "s", "network_bridge": "br"
	}`)
	if err != nil {
		t.Fatalf("Load returned unexpected error: %v", err)
	}
	if got := cfg.BalloonValue(); got != "0" {
		t.Errorf("BalloonValue() = %q; want \"0\" when balloon is absent from the manifest", got)
	}
}

// TestBalloon_SentinelFromJSON verifies the "pve-default" sentinel survives
// Load/ApplyDefaults and resolves to "" (no balloon key written).
func TestBalloon_SentinelFromJSON(t *testing.T) {
	t.Parallel()
	cfg, err := mustLoad(t, `{
		"host": "h", "user": "u", "password": "p",
		"vm_storage": "s", "disk_storage": "s", "network_bridge": "br",
		"balloon": "pve-default"
	}`)
	if err != nil {
		t.Fatalf("Load returned unexpected error: %v", err)
	}
	if got := cfg.BalloonValue(); got != "" {
		t.Errorf("BalloonValue() = %q; want \"\" for the pve-default sentinel", got)
	}
}

// TestBalloon_InvalidValue_Rejected verifies a non-numeric, non-sentinel
// balloon value fails validation at Load time with an error naming the knob.
func TestBalloon_InvalidValue_Rejected(t *testing.T) {
	t.Parallel()
	for _, bad := range []string{"lots", "-5", "1.5"} {
		_, err := mustLoad(t, `{
			"host": "h", "user": "u", "password": "p",
			"vm_storage": "s", "disk_storage": "s", "network_bridge": "br",
			"balloon": "`+bad+`"
		}`)
		if err == nil {
			t.Errorf("balloon=%q: Load succeeded; want validation error", bad)
			continue
		}
		if !strings.Contains(err.Error(), "balloon") {
			t.Errorf("balloon=%q: error %q does not name the balloon knob", bad, err)
		}
	}
}
