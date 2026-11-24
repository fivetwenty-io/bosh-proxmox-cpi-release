package handlers

import (
	"encoding/json"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/aapattern"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
)

// haPatternConfig returns a config whose job default pattern is raw and whose
// vm_type and disk_type profiles carry the given ha_anti_affinity values.
func haPatternConfig(t *testing.T, raw string, vmType, diskType any) *config.CPIConfig {
	t.Helper()
	cfg := &config.CPIConfig{
		VMTypes: map[string]config.TypeProfile{
			"db": {CloudProperties: map[string]any{"ha_anti_affinity": vmType}},
		},
		DiskTypes: map[string]config.TypeProfile{
			"fast": {CloudProperties: map[string]any{"ha_anti_affinity": diskType}},
		},
	}
	if raw != "" {
		var hp config.HAPattern
		if err := json.Unmarshal([]byte(raw), &hp); err != nil {
			t.Fatal(err)
		}
		cfg.HA = &config.HAConfig{AntiAffinityPattern: &hp}
	}
	return cfg
}

func TestResolveAntiAffinityPattern_OffAtEveryLayer(t *testing.T) {
	t.Parallel()
	for _, off := range []any{"off", false} {
		cfg := haPatternConfig(t, `{"block": {}}`, off, off)
		for name, cp := range map[string]map[string]any{
			"call":      {"ha_anti_affinity": off},
			"disk_type": {"disk_type": "fast"},
			"vm_type":   {"vm_type": "db"},
		} {
			got, err := resolveAntiAffinityPattern(cp, cfg)
			if err != nil || got != "off" {
				t.Errorf("%s set to %v: resolveAntiAffinityPattern = %q, %v; want off over the job's block",
					name, off, got, err)
			}
		}
	}
}

func TestResolveAntiAffinityPattern_BlockOptsBackInOverAnOffDefault(t *testing.T) {
	t.Parallel()
	for _, block := range []any{map[string]any{"block": map[string]any{}}, "block"} {
		cfg := haPatternConfig(t, `"off"`, block, nil)
		got, err := resolveAntiAffinityPattern(map[string]any{"vm_type": "db"}, cfg)
		if err != nil || got != "block" {
			t.Errorf("vm_type set to %v: resolveAntiAffinityPattern = %q, %v; want block over the job's off",
				block, got, err)
		}
	}
	for _, raw := range []string{`"off"`, `false`} {
		cfg := haPatternConfig(t, raw, nil, nil)
		got, err := resolveAntiAffinityPattern(map[string]any{}, cfg)
		if err != nil || got != "off" {
			t.Errorf("job default %s: resolveAntiAffinityPattern = %q, %v; want off", raw, got, err)
		}
	}
}

// A member created with off records aa=off on its HA resource, and the
// group's later plans read it back as the off pattern.
func TestResourceMarker_RecordsOff(t *testing.T) {
	t.Parallel()
	ident := haIdentity{Owner: "dir-1", Deployment: "cf", Group: "db"}
	m := ident.resourceMarker("off", 1700000000)
	want := ident.marker() + " aa=off born=1700000000"
	if m != want {
		t.Fatalf("resourceMarker = %q; want %q", m, want)
	}
	parsed, ok := parseHaMarker(m)
	if !ok || parsed.AA != "off" || parsed.Born != 1700000000 {
		t.Fatalf("parseHaMarker(%q) = %+v, %v", m, parsed, ok)
	}
	p, err := aapattern.Parse(parsed.AA)
	if err != nil || !p.Off {
		t.Fatalf("aapattern.Parse(%q) = %+v, %v; want the off pattern", parsed.AA, p, err)
	}
}
