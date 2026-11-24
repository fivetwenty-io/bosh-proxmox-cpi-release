package handlers

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
)

func TestResolveAntiAffinityPattern_ReadsEveryLayer(t *testing.T) {
	t.Parallel()
	cfg := &config.CPIConfig{
		VMTypes: map[string]config.TypeProfile{
			"spread": {CloudProperties: map[string]any{"ha_anti_affinity": map[string]any{"stride": 2}}},
		},
	}
	cases := []struct {
		name string
		cp   string
		want string
	}{
		{"unset", `{}`, "block"},
		{"null", `{"ha_anti_affinity": null}`, "block"},
		{"call", `{"ha_anti_affinity": {"window": 3}}`, "window:3"},
		{"list", `{"ha_anti_affinity": [{"stride": 2}, {"block": {}, "drain_safe": false}]}`,
			"stride:2+block!unsafe"},
		{"vm_type", `{"vm_type": "spread"}`, "stride:2"},
		{"call beats vm_type", `{"vm_type": "spread", "ha_anti_affinity": {"sets": [[0, 1]]}}`, "sets:0.1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var cp map[string]any
			if err := json.Unmarshal([]byte(tc.cp), &cp); err != nil {
				t.Fatal(err)
			}
			got, err := resolveAntiAffinityPattern(cp, cfg)
			if err != nil || got != tc.want {
				t.Fatalf("resolveAntiAffinityPattern(%s) = %q, %v; want %q", tc.cp, got, err, tc.want)
			}
		})
	}
}

// The pattern a create records is the effective one, so a group that relies
// on pve.ha.anti_affinity_pattern records that default, and a later plan
// under another default keeps it.
func TestResolveAntiAffinityPattern_RecordsTheConfigDefault(t *testing.T) {
	t.Parallel()
	var hp config.HAPattern
	if err := json.Unmarshal([]byte(`{"window": 3}`), &hp); err != nil {
		t.Fatal(err)
	}
	cfg := &config.CPIConfig{HA: &config.HAConfig{AntiAffinityPattern: &hp}}
	for cp, want := range map[string]string{`{}`: "window:3", `{"ha_anti_affinity": {"stride": 2}}`: "stride:2"} {
		var props map[string]any
		if err := json.Unmarshal([]byte(cp), &props); err != nil {
			t.Fatal(err)
		}
		got, err := resolveAntiAffinityPattern(props, cfg)
		if err != nil || got != want {
			t.Errorf("resolveAntiAffinityPattern(%s) = %q, %v; want %q", cp, got, err, want)
		}
	}
}

func TestResolveAntiAffinityPattern_RefusesABadValue(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{`{"ha_anti_affinity": "stride"}`, `{"ha_anti_affinity": true}`,
		`{"ha_anti_affinity": {"stride": 1}}`, `{"ha_anti_affinity": {"strde": 2}}`} {
		var cp map[string]any
		if err := json.Unmarshal([]byte(raw), &cp); err != nil {
			t.Fatal(err)
		}
		_, err := resolveAntiAffinityPattern(cp, &config.CPIConfig{})
		if err == nil || !strings.Contains(err.Error(), "ha_anti_affinity") {
			t.Errorf("resolveAntiAffinityPattern(%s) err = %v; want one that names the property", raw, err)
		}
	}
}

func TestLayeredResolver_RawReturnsAStructuredValue(t *testing.T) {
	t.Parallel()
	r, err := newLayeredResolver(map[string]any{"k": []any{1.0}, "n": nil}, &config.CPIConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := r.Raw("n", "k"); !ok || len(v.([]any)) != 1 {
		t.Fatalf("Raw = %v, %v; want the list, with the null skipped", v, ok)
	}
	if _, ok := r.Raw("missing"); ok {
		t.Fatal("Raw found a missing key")
	}
}
