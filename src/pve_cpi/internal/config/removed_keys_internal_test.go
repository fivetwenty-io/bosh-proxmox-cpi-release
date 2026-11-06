package config

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// TestRemovedKeys_AreDeclaredInSpec keeps every removed key in the job spec
// property list. BOSH ignores a property the spec does not declare, so a
// dropped declaration would let a deployment set the key and see nothing.
func TestRemovedKeys_AreDeclaredInSpec(t *testing.T) {
	t.Parallel()
	declared := map[string]bool{}
	for _, n := range jobSpecPropertyNames {
		declared[n] = true
	}
	for _, k := range removedKeys {
		if !declared[k.specName()] {
			t.Errorf("removed key %s is not declared in jobs/pve_cpi/spec", k.specName())
		}
	}
	if len(removedKeys) != 9 {
		t.Errorf("removedKeys holds %d entries, want 9", len(removedKeys))
	}
}

// TestRemovedKeys_SpecHasNoDefaultAndSaysRemoved checks the spec side of the
// guard. A default would make "set" undetectable, and the description must say
// the key is gone and what replaces it.
func TestRemovedKeys_SpecHasNoDefaultAndSaysRemoved(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("../../../../jobs/pve_cpi/spec")
	if err != nil {
		t.Fatalf("read the job spec: %v", err)
	}
	lines := strings.Split(string(raw), "\n")
	for _, k := range removedKeys {
		start := -1
		for i, l := range lines {
			if strings.HasPrefix(l, "  "+k.specName()+":") {
				start = i
				break
			}
		}
		if start < 0 {
			t.Fatalf("%s not found in the spec", k.specName())
		}
		end := start + 1
		for end < len(lines) && strings.HasPrefix(lines[end], "   ") {
			end++
		}
		block := strings.Join(lines[start:end], "\n")
		if strings.Contains(block, "default:") {
			t.Errorf("%s must not carry a default", k.specName())
		}
		if !strings.Contains(block, "REMOVED") {
			t.Errorf("%s description must say REMOVED", k.specName())
		}
	}
}

// TestRemovedKeys_TemplateRepeatsReplacements keeps the ERB guard text and the
// Go table in step, so an operator sees the same advice from either path.
func TestRemovedKeys_TemplateRepeatsReplacements(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("../../../../jobs/pve_cpi/templates/cpi.json.erb")
	if err != nil {
		t.Fatalf("read the template: %v", err)
	}
	for _, k := range removedKeys {
		if !strings.Contains(string(raw), k.specName()) {
			t.Errorf("template does not guard %s", k.specName())
		}
		if !strings.Contains(string(raw), k.replacement) {
			t.Errorf("template does not carry the replacement text for %s: %q", k.specName(), k.replacement)
		}
	}
}

func TestPlacementHasKey(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		block map[string]any
		path  []string
		want  bool
	}{
		{"nil block", nil, []string{"dlb", "enabled"}, false},
		{"empty path", map[string]any{"dlb": true}, nil, false},
		{"top level", map[string]any{"exclude_maintenance_nodes": false}, []string{"exclude_maintenance_nodes"}, true},
		{"nested", map[string]any{"dlb": map[string]any{"enabled": false}}, []string{"dlb", "enabled"}, true},
		{"dotted", map[string]any{"dlb.enabled": true}, []string{"dlb", "enabled"}, true},
		{"dotted inside nested", map[string]any{"anti_affinity": map[string]any{"use_ha_rules": nil}},
			[]string{"anti_affinity", "use_ha_rules"}, true},
		{"sibling only", map[string]any{"dlb": map[string]any{"az_name": "x"}}, []string{"dlb", "enabled"}, false},
		{"parent is not a map", map[string]any{"dlb": true}, []string{"dlb", "enabled"}, false},
		{"unrelated", map[string]any{"az_map": map[string]any{}}, []string{"dlb", "enabled"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := placementHasKey(tc.block, tc.path); got != tc.want {
				t.Errorf("placementHasKey = %v, want %v", got, tc.want)
			}
		})
	}
}

// removedKeyContexts builds the context spellings that carry one removed key.
func removedKeyContexts(k removedKey, val any) map[string]map[string]any {
	nested := map[string]any{}
	cur := nested
	for i, seg := range k.path {
		if i == len(k.path)-1 {
			cur[seg] = val
			break
		}
		next := map[string]any{}
		cur[seg] = next
		cur = next
	}
	return map[string]map[string]any{
		"nested block":    {"pve_placement": nested},
		"dotted in block": {"pve_placement": map[string]any{strings.Join(k.path, "."): val}},
		"flat dotted":     {"pve_placement." + strings.Join(k.path, "."): val},
		"flat underscore": {"pve_placement_" + strings.Join(k.path, "_"): val},
		"nested pve":      {"pve": map[string]any{"placement": nested}},
	}
}

func TestCheckRemovedContextKeys_EveryKeyEverySpellingEveryValue(t *testing.T) {
	t.Parallel()
	values := map[string]any{"true": true, "false": false, "string": "yes", "null": nil, "number": 1.0}
	for _, k := range removedKeys {
		for vname, val := range values {
			for sname, ctx := range removedKeyContexts(k, val) {
				t.Run(fmt.Sprintf("%s/%s/%s", k.specName(), sname, vname), func(t *testing.T) {
					t.Parallel()
					// The nested pve form is folded to pve_placement before the check runs.
					err := checkRemovedContextKeys(flattenNestedContextOverrides(ctx))
					if err == nil {
						t.Fatal("a removed key must be rejected, got nil")
					}
					if !strings.Contains(err.Error(), k.specName()) || !strings.Contains(err.Error(), k.replacement) {
						t.Errorf("error must name the key and its replacement; got: %v", err)
					}
				})
			}
		}
	}
}

func TestCheckRemovedContextKeys_CleanContexts(t *testing.T) {
	t.Parallel()
	cases := map[string]map[string]any{
		"nil":                  nil,
		"empty":                {},
		"unrelated key":        {"pve_node": "pve1"},
		"az map only":          {"pve_placement": map[string]any{"az_map": map[string]any{"z1": []any{"pve1"}}}},
		"anti-affinity enable": {"pve_placement": map[string]any{"anti_affinity": map[string]any{"enabled": true}}},
		"replacement keys":     {"pve_ha": map[string]any{"mode": "auto", "dlb": map[string]any{"enabled": true}}},
		"non-object block":     {"pve_placement": "garbage"},
		"null block":           {"pve_placement": nil},
	}
	for name, ctx := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := checkRemovedContextKeys(ctx); err != nil {
				t.Errorf("clean context rejected: %v", err)
			}
		})
	}
}

func TestCheckRemovedContextKeys_NamesEveryKeyInOneError(t *testing.T) {
	t.Parallel()
	ctx := map[string]any{
		"pve_placement": map[string]any{
			"exclude_maintenance_nodes": true,
			"pin_az_strict":             false,
			"dlb":                       map[string]any{"enabled": true, "manage_cluster_crs": "x"},
		},
		"pve_placement_anti_affinity_strict": false,
	}
	err := checkRemovedContextKeys(ctx)
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, name := range []string{
		"pve.placement.exclude_maintenance_nodes", "pve.placement.pin_az_strict",
		"pve.placement.dlb.enabled", "pve.placement.dlb.manage_cluster_crs", "pve.placement.anti_affinity.strict",
	} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error must name %s; got: %v", name, err)
		}
	}
	if strings.Contains(err.Error(), "pve.placement.dlb.az_name") {
		t.Errorf("error must not name a key that is not set; got: %v", err)
	}
	if strings.Count(err.Error(), "the cpi-config entry") != 1 {
		t.Errorf("want one error, got: %v", err)
	}
}

// TestApplyContextOverrides_RemovedKeyIsRejected goes through the public entry
// point to show the guard fires before the strict decode of the block, so the
// operator reads the replacement rather than "unknown field".
func TestApplyContextOverrides_RemovedKeyIsRejected(t *testing.T) {
	t.Parallel()
	base := &CPIConfig{}
	base.ApplyDefaults()
	_, _, _, err := ApplyContextOverrides(base, map[string]any{
		"pve": map[string]any{"placement": map[string]any{"dlb": map[string]any{"enabled": false}}},
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "use pve.ha.dlb.enabled with pve.ha.mode: auto") {
		t.Errorf("error must carry the replacement; got: %v", err)
	}
	if strings.Contains(err.Error(), "unknown field") {
		t.Errorf("guard must run before the strict decode; got: %v", err)
	}
}
