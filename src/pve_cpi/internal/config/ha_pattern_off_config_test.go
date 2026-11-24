package config_test

import (
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
)

// TestHAConfig_PatternOffLoadsAsSet pins that the job default reads off from
// the string "off" and from false, which is what YAML makes of an unquoted
// off, and that off counts as a set pattern rather than as unset.
func TestHAConfig_PatternOffLoadsAsSet(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{`"off"`, `false`} {
		cfg := haBase(t, `{"mode":"auto","anti_affinity_pattern":`+raw+`}`)
		got := cfg.HAAntiAffinityPatternValue()
		if !got.Off || got.Canonical() != "off" {
			t.Errorf("pattern %s = %s, want off", raw, got.Canonical())
		}
		if !cfg.HAAntiAffinityPatternSet() {
			t.Errorf("pattern %s must read as set", raw)
		}
	}
}

func TestHAConfig_PatternBareBlockLoadsAsTheDefaultBlock(t *testing.T) {
	t.Parallel()
	cfg := haBase(t, `{"mode":"auto","anti_affinity_pattern":"block"}`)
	if got := cfg.HAAntiAffinityPatternValue(); got.Canonical() != "block" || !got.IsDefaultBlock() {
		t.Errorf("pattern = %s, want the default block", got.Canonical())
	}
	if !cfg.HAAntiAffinityPatternSet() {
		t.Error("a bare block must read as set")
	}
}

func TestHAConfig_PatternTrueFailsTheLoad(t *testing.T) {
	t.Parallel()
	_, err := mustLoad(t, haJSON(`{"anti_affinity_pattern":true}`))
	if err == nil || !strings.Contains(err.Error(), "anti_affinity_pattern") ||
		!strings.Contains(err.Error(), "true names no pattern") {
		t.Fatalf("err = %v, want one naming anti_affinity_pattern and refusing true", err)
	}
}

func TestApplyContextOverrides_HA_PatternOffMergesBothWays(t *testing.T) {
	t.Parallel()
	block := haBase(t, `{"mode":"auto","anti_affinity_pattern":{"block":{}}}`)
	for _, off := range []any{"off", false} {
		eff, _, _, err := config.ApplyContextOverrides(block, map[string]any{
			"pve_ha": map[string]any{"anti_affinity_pattern": off},
		})
		if err != nil {
			t.Fatalf("entry pattern %v: unexpected error: %v", off, err)
		}
		if got := eff.HAAntiAffinityPatternValue(); !got.Off {
			t.Errorf("entry pattern %v = %s, want off over the job's block", off, got.Canonical())
		}
		if !eff.HAAntiAffinityPatternSet() {
			t.Errorf("entry pattern %v must read as set", off)
		}
		if got := block.HAAntiAffinityPatternValue(); got.Off {
			t.Errorf("entry pattern %v changed the job pattern to off", off)
		}
	}

	off := haBase(t, `{"mode":"auto","anti_affinity_pattern":"off"}`)
	for _, over := range []any{map[string]any{"block": map[string]any{}}, "block"} {
		eff, _, _, err := config.ApplyContextOverrides(off, map[string]any{
			"pve_ha": map[string]any{"anti_affinity_pattern": over},
		})
		if err != nil {
			t.Fatalf("entry pattern %v: unexpected error: %v", over, err)
		}
		if got := eff.HAAntiAffinityPatternValue(); got.Off || !got.IsDefaultBlock() {
			t.Errorf("entry pattern %v = %s, want block over the job's off", over, got.Canonical())
		}
	}

	eff, _, _, err := config.ApplyContextOverrides(off, map[string]any{"pve_ha": map[string]any{"mode": "managed"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := eff.HAAntiAffinityPatternValue(); !got.Off {
		t.Errorf("entry pattern = %s, want the job's off inherited", got.Canonical())
	}

	_, _, _, err = config.ApplyContextOverrides(off, map[string]any{
		"pve_ha": map[string]any{"anti_affinity_pattern": true},
	})
	if err == nil || !strings.Contains(err.Error(), "true names no pattern") {
		t.Errorf("err = %v, want the entry's true refused", err)
	}
}
