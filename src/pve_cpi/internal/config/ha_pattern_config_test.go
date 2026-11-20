package config_test

import (
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
)

func TestHAConfig_PatternDefaultsToBlockAndOverflowToSplit(t *testing.T) {
	t.Parallel()
	for _, block := range []string{"", "{}"} {
		cfg := haBase(t, block)
		if got := cfg.HAAntiAffinityPatternValue(); got.Canonical() != "block" || !got.IsDefaultBlock() {
			t.Errorf("block %q: pattern = %s, want the built-in block", block, got.Canonical())
		}
		if cfg.HAAntiAffinityPatternSet() {
			t.Errorf("block %q: the pattern must read as unset", block)
		}
		if got := cfg.HAAntiAffinityOverflowValue(); got != config.HAOverflowSplit {
			t.Errorf("block %q: overflow = %s, want split", block, got)
		}
	}
	var cfg *config.CPIConfig
	if cfg.HAAntiAffinityPatternValue().Canonical() != "block" {
		t.Error("a nil config must read the built-in block")
	}
}

func TestHAConfig_PatternLoadsEveryShape(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		`{"block":{}}`:                                    "block",
		`{"block":{"size":3}}`:                            "block:3",
		`{"stride":2}`:                                    "stride:2",
		`[{"stride":2},{"sets":[[0,1]]}]`:                 "stride:2+sets:0.1",
		`{"window":3,"drain_safe":false}`:                 "window:3!unsafe",
		`{"sets":[[0,1,2]],"drain_safe":true}`:            "sets:0.1.2",
		`[{"window":2,"drain_safe":true},{"block":null}]`: "window:2+block",
	}
	for raw, want := range tests {
		cfg := haBase(t, `{"anti_affinity_pattern":`+raw+`}`)
		if got := cfg.HAAntiAffinityPatternValue().Canonical(); got != want {
			t.Errorf("pattern %s = %s, want %s", raw, got, want)
		}
		if !cfg.HAAntiAffinityPatternSet() {
			t.Errorf("pattern %s must read as set", raw)
		}
	}
}

func TestHAConfig_PatternRejectsBadShapes(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		`{"stride":1}`:                "from 2 to 64",
		`{"strde":2}`:                 "unknown anti-affinity pattern key",
		`{"block":{"size":1}}`:        "at least 2",
		`"block"`:                     "must be a map or a list",
		`[]`:                          "at least one term",
		`{"sets":[[0,0]]}`:            "twice",
		`{"window":2,"drain_safe":1}`: "boolean",
	}
	for raw, want := range tests {
		_, err := mustLoad(t, haJSON(`{"anti_affinity_pattern":`+raw+`}`))
		if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "anti_affinity_pattern") {
			t.Errorf("pattern %s: err = %v, want one naming anti_affinity_pattern and saying %q", raw, err, want)
		}
	}
}

func TestApplyContextOverrides_HA_PatternMergesOverTheJobBlock(t *testing.T) {
	t.Parallel()
	base := haBase(t, `{"mode":"auto","anti_affinity_pattern":{"stride":2}}`)
	eff, _, _, err := config.ApplyContextOverrides(base, map[string]any{
		"pve_ha": map[string]any{"anti_affinity_pattern": map[string]any{"window": 2}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := eff.HAAntiAffinityPatternValue().Canonical(); got != "window:2" {
		t.Errorf("entry pattern = %s, want window:2", got)
	}
	if got := base.HAAntiAffinityPatternValue().Canonical(); got != "stride:2" {
		t.Errorf("job pattern = %s, want stride:2 kept", got)
	}
	eff, _, _, err = config.ApplyContextOverrides(base, map[string]any{"pve_ha": map[string]any{"mode": "managed"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := eff.HAAntiAffinityPatternValue().Canonical(); got != "stride:2" {
		t.Errorf("entry pattern = %s, want the job's stride:2 inherited", got)
	}
	_, _, _, err = config.ApplyContextOverrides(base, map[string]any{
		"pve_ha": map[string]any{"anti_affinity_pattern": map[string]any{"window": 1}},
	})
	if err == nil || !strings.Contains(err.Error(), "from 2 to 64") {
		t.Errorf("err = %v, want the entry's bad pattern refused", err)
	}
}
