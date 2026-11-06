package config_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
)

// haJSON returns a loadable cpi.json whose ha block is the given JSON object.
// An empty block argument omits the key.
func haJSON(block string) string {
	base := `{"host":"pve.example.com","user":"root","password":"s3cr3t","vm_storage":"local-lvm",` +
		`"disk_storage":"local-lvm","stemcell_storage":"local","network_bridge":"vmbr0","node":"pve1"`
	if block == "" {
		return base + "}"
	}
	return base + `,"ha":` + block + "}"
}

// haBase is a validated job-level config carrying the given ha block, for the
// per-entry override tests.
func haBase(t *testing.T, block string) *config.CPIConfig {
	t.Helper()
	cfg, err := mustLoad(t, haJSON(block))
	if err != nil {
		t.Fatalf("load base config: %v", err)
	}
	return cfg
}

func TestHAConfig_DefaultsWhenUnset(t *testing.T) {
	t.Parallel()
	for name, block := range map[string]string{"absent block": "", "empty block": "{}"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg := haBase(t, block)
			checks := []struct {
				key  string
				got  any
				want any
			}{
				{"mode", cfg.HAModeValue(), "off"},
				{"writes enabled", cfg.HAWritesEnabled(), false},
				{"storage_topology", cfg.HAStorageTopologyValue(), "auto"},
				{"local_policy", cfg.HALocalPolicyValue(), "none"},
				{"anti_affinity", cfg.HAAntiAffinityValue(), "auto"},
				{"anti_affinity_overflow", cfg.HAAntiAffinityOverflowValue(), "skip"},
				{"pin_az", cfg.HAPinAZValue(), "auto"},
				{"pin_az_strict", cfg.HAPinAZStrictValue(), true},
				{"headroom", cfg.HAHeadroomValue(), "warn"},
				{"dlb.enabled", cfg.HADLBEnabledValue(), false},
				{"dlb.az_name", cfg.HADLBAZNameValue(), "dlb"},
				{"motion_wait_sec", cfg.HAMotionWaitSecValue(), 600},
				{"replication.schedule", cfg.HAReplicationScheduleValue(), "*/5"},
				{"replication.rate_mbps", cfg.HAReplicationRateMbpsValue(), 0},
			}
			for _, c := range checks {
				if c.got != c.want {
					t.Errorf("%s = %v, want default %v", c.key, c.got, c.want)
				}
			}
		})
	}
}

func TestHAConfig_NilConfigReadsDefaults(t *testing.T) {
	t.Parallel()
	var cfg *config.CPIConfig
	if cfg.HAModeValue() != "off" || cfg.HAWritesEnabled() || !cfg.HAPinAZStrictValue() ||
		cfg.HADLBAZNameValue() != "dlb" || cfg.HAMotionWaitSecValue() != 600 {
		t.Error("a nil config must read every ha key as its default")
	}
}

func TestHAConfig_AbsentBlockStaysNil(t *testing.T) {
	t.Parallel()
	if cfg := haBase(t, ""); cfg.HA != nil {
		t.Errorf("ApplyDefaults must not materialize the ha block, got %+v", cfg.HA)
	}
}

func TestHAConfig_EachKeyRoundTrips(t *testing.T) {
	t.Parallel()
	cfg := haBase(t, `{"mode":"managed","storage_topology":"mixed","local_policy":"none","anti_affinity":"on",`+
		`"anti_affinity_overflow":"split","pin_az":"on","pin_az_strict":false,"headroom":"enforce",`+
		`"dlb":{"enabled":true,"az_name":"balance"},"motion_wait_sec":900,`+
		`"replication":{"schedule":"mon..fri 22:00","rate_mbps":50}}`)
	checks := []struct {
		key  string
		got  any
		want any
	}{
		{"mode", cfg.HAModeValue(), "managed"},
		{"writes enabled", cfg.HAWritesEnabled(), true},
		{"storage_topology", cfg.HAStorageTopologyValue(), "mixed"},
		{"local_policy", cfg.HALocalPolicyValue(), "none"},
		{"anti_affinity", cfg.HAAntiAffinityValue(), "on"},
		{"anti_affinity_overflow", cfg.HAAntiAffinityOverflowValue(), "split"},
		{"pin_az", cfg.HAPinAZValue(), "on"},
		{"pin_az_strict", cfg.HAPinAZStrictValue(), false},
		{"headroom", cfg.HAHeadroomValue(), "enforce"},
		{"dlb.enabled", cfg.HADLBEnabledValue(), true},
		{"dlb.az_name", cfg.HADLBAZNameValue(), "balance"},
		{"motion_wait_sec", cfg.HAMotionWaitSecValue(), 900},
		{"replication.schedule", cfg.HAReplicationScheduleValue(), "mon..fri 22:00"},
		{"replication.rate_mbps", cfg.HAReplicationRateMbpsValue(), 50},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.key, c.got, c.want)
		}
	}
}

func TestHAConfig_EveryEnumValueAccepted(t *testing.T) {
	t.Parallel()
	values := map[string][]string{
		"mode":                   {"off", "auto", "managed"},
		"storage_topology":       {"auto", "shared", "local", "mixed"},
		"local_policy":           {"none"},
		"anti_affinity":          {"off", "auto"},
		"anti_affinity_overflow": {"skip", "split"},
		"pin_az":                 {"off", "auto"},
		"headroom":               {"off", "warn", "enforce"},
	}
	for key, accepted := range values {
		for _, v := range accepted {
			if _, err := mustLoad(t, haJSON(fmt.Sprintf(`{%q:%q}`, key, v))); err != nil {
				t.Errorf("ha.%s %q must be accepted: %v", key, v, err)
			}
		}
	}
}

func TestHAConfig_ExplicitFalsyValuesCountAsSet(t *testing.T) {
	t.Parallel()
	cfg := haBase(t, `{"pin_az_strict":false,"dlb":{"enabled":false,"az_name":""}}`)
	if cfg.HAPinAZStrictValue() {
		t.Error("an explicit pin_az_strict false must read false")
	}
	if cfg.HADLBEnabledValue() {
		t.Error("an explicit dlb.enabled false must read false")
	}
	if got := cfg.HADLBAZNameValue(); got != "" {
		t.Errorf("an explicit empty dlb.az_name must disable the sentinel, got %q", got)
	}
}

// YAML 1.1 reads an unquoted off or on as a boolean, so a manifest that writes
// `mode: off` reaches the CPI as JSON false.
func TestHAConfig_BooleanSpellingOfOnAndOff(t *testing.T) {
	t.Parallel()
	cfg := haBase(t, `{"mode":false,"anti_affinity":false,"pin_az":false,"headroom":false}`)
	if cfg.HAModeValue() != "off" || cfg.HAAntiAffinityValue() != "off" ||
		cfg.HAPinAZValue() != "off" || cfg.HAHeadroomValue() != "off" {
		t.Errorf("false must read as off, got mode=%s anti_affinity=%s pin_az=%s headroom=%s",
			cfg.HAModeValue(), cfg.HAAntiAffinityValue(), cfg.HAPinAZValue(), cfg.HAHeadroomValue())
	}
	cfg = haBase(t, `{"mode":"auto","anti_affinity":true,"pin_az":true}`)
	if cfg.HAAntiAffinityValue() != "on" || cfg.HAPinAZValue() != "on" {
		t.Errorf("true must read as on, got anti_affinity=%s pin_az=%s", cfg.HAAntiAffinityValue(), cfg.HAPinAZValue())
	}
	_, err := mustLoad(t, haJSON(`{"mode":true}`))
	assertCloudError(t, err, `ha.mode must be one of off, auto, managed, got "on"`)
}

func TestHAConfig_RejectsInvalidValues(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		block string
		want  string
	}{
		{"mode", `{"mode":"always"}`, `ha.mode must be one of off, auto, managed, got "always"`},
		{"mode empty", `{"mode":""}`, `ha.mode must be one of off, auto, managed, got ""`},
		{"mode case", `{"mode":"Auto"}`, `ha.mode must be one of`},
		{"storage_topology", `{"storage_topology":"nfs"}`, `ha.storage_topology must be one of auto, shared, local, mixed`},
		{"local_policy", `{"local_policy":"copy"}`, `ha.local_policy must be one of none, hold, replicate`},
		{"anti_affinity", `{"anti_affinity":"strict"}`, `ha.anti_affinity must be one of off, auto, on`},
		{"anti_affinity_overflow", `{"anti_affinity_overflow":"fail"}`,
			`ha.anti_affinity_overflow must be one of skip, split`},
		{"pin_az", `{"pin_az":"maybe"}`, `ha.pin_az must be one of off, auto, on`},
		{"headroom", `{"headroom":"strict"}`, `ha.headroom must be one of off, warn, enforce`},
		{"motion_wait_sec zero", `{"motion_wait_sec":0}`, `ha.motion_wait_sec must be between 1 and 86400, got 0`},
		{"motion_wait_sec negative", `{"motion_wait_sec":-5}`, `ha.motion_wait_sec must be between 1 and 86400, got -5`},
		{"motion_wait_sec huge", `{"motion_wait_sec":86401}`, `ha.motion_wait_sec must be between 1 and 86400`},
		{"schedule empty", `{"replication":{"schedule":""}}`, `ha.replication.schedule must not be empty`},
		{"schedule blank", `{"replication":{"schedule":"  "}}`, `ha.replication.schedule must not be empty`},
		{"schedule control", `{"replication":{"schedule":"*/5\n*/6"}}`,
			`ha.replication.schedule must not contain control characters`},
		{"schedule long", `{"replication":{"schedule":"` + strings.Repeat("a", 129) + `"}}`,
			`ha.replication.schedule must be at most 128 characters`},
		{"rate negative", `{"replication":{"rate_mbps":-1}}`, `ha.replication.rate_mbps must be 0 or greater, got -1`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := mustLoad(t, haJSON(tc.block))
			assertCloudError(t, err, tc.want)
		})
	}
}

func TestHAConfig_RejectsOnAndDLBWhileModeIsOff(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		block string
		want  string
	}{
		{"anti_affinity on, mode unset", `{"anti_affinity":"on"}`,
			`ha.anti_affinity "on" requires pve.ha.mode to be "auto" or "managed"`},
		{"anti_affinity on, mode off", `{"mode":"off","anti_affinity":"on"}`, `requires pve.ha.mode`},
		{"anti_affinity on, mode boolean off", `{"mode":false,"anti_affinity":"on"}`, `requires pve.ha.mode`},
		{"pin_az on, mode unset", `{"pin_az":"on"}`,
			`ha.pin_az "on" requires pve.ha.mode to be "auto" or "managed"`},
		{"pin_az on, mode off", `{"mode":"off","pin_az":"on"}`, `requires pve.ha.mode`},
		{"dlb enabled, mode unset", `{"dlb":{"enabled":true}}`,
			`ha.dlb.enabled requires pve.ha.mode to be "auto" or "managed"`},
		{"dlb enabled, mode off", `{"mode":"off","dlb":{"enabled":true}}`, `requires pve.ha.mode`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := mustLoad(t, haJSON(tc.block))
			assertCloudError(t, err, tc.want)
		})
	}
}

func TestHAConfig_ReportsEveryCombinationError(t *testing.T) {
	t.Parallel()
	_, err := mustLoad(t, haJSON(`{"anti_affinity":"on","pin_az":"on","dlb":{"enabled":true}}`))
	for _, want := range []string{"ha.anti_affinity", "ha.pin_az", "ha.dlb.enabled"} {
		assertCloudError(t, err, want)
	}
}

func TestHAConfig_AcceptsOnAndDLBWhenModeIsSet(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"auto", "managed"} {
		block := fmt.Sprintf(`{"mode":%q,"anti_affinity":"on","pin_az":"on","dlb":{"enabled":true}}`, mode)
		if _, err := mustLoad(t, haJSON(block)); err != nil {
			t.Errorf("mode %s must accept on and dlb.enabled: %v", mode, err)
		}
	}
	// A mode of off with the keys at their non-on values stays valid.
	offBlock := `{"mode":"off","anti_affinity":"auto","pin_az":"off","dlb":{"enabled":false}}`
	if _, err := mustLoad(t, haJSON(offBlock)); err != nil {
		t.Errorf("mode off with no on key must load: %v", err)
	}
}

func TestHAConfig_RejectsPoliciesNotYetAvailable(t *testing.T) {
	t.Parallel()
	for _, policy := range []string{"hold", "replicate"} {
		for _, mode := range []string{"auto", "managed"} {
			block := fmt.Sprintf(`{"mode":%q,"local_policy":%q}`, mode, policy)
			_, err := mustLoad(t, haJSON(block))
			assertCloudError(t, err, fmt.Sprintf(`ha.local_policy %q is not available in this build`, policy))
		}
	}
}

func TestHAConfig_DoesNotValidateUnsetKeys(t *testing.T) {
	t.Parallel()
	// An empty sub-block and a lone sentinel setting carry no checked key.
	blocks := []string{`{"dlb":{}}`, `{"replication":{}}`, `{"dlb":{"az_name":""}}`, `{"dlb":{"az_name":"x"}}`}
	for _, block := range blocks {
		if _, err := mustLoad(t, haJSON(block)); err != nil {
			t.Errorf("block %s must load: %v", block, err)
		}
	}
}

func TestHAConfig_MalformedTypesFailToLoad(t *testing.T) {
	t.Parallel()
	for _, block := range []string{`{"mode":5}`, `{"pin_az_strict":"yes"}`, `{"motion_wait_sec":"soon"}`,
		`{"dlb":"on"}`, `{"mode":{"x":1}}`} {
		if _, err := mustLoad(t, haJSON(block)); err == nil {
			t.Errorf("block %s must fail to load", block)
		}
	}
}

// ---------------------------------------------------------------------------
// pve_ha per-entry override.
// ---------------------------------------------------------------------------

func TestApplyContextOverrides_HA_OnlyModeInheritsTheRest(t *testing.T) {
	t.Parallel()
	base := haBase(t, `{"headroom":"enforce","pin_az_strict":false,"motion_wait_sec":900,`+
		`"dlb":{"az_name":"balance"},"replication":{"schedule":"*/15","rate_mbps":20}}`)

	eff, applied, unknown, err := config.ApplyContextOverrides(base, map[string]any{
		"pve_ha": map[string]any{"mode": "auto"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(unknown) != 0 || len(applied) != 1 || applied[0] != "pve_ha" {
		t.Fatalf("applied = %v, unknown = %v, want [pve_ha] and none", applied, unknown)
	}
	if eff.HAModeValue() != "auto" {
		t.Errorf("mode = %s, want the entry's auto", eff.HAModeValue())
	}
	if eff.HAHeadroomValue() != "enforce" || eff.HAPinAZStrictValue() || eff.HAMotionWaitSecValue() != 900 ||
		eff.HADLBAZNameValue() != "balance" || eff.HAReplicationScheduleValue() != "*/15" ||
		eff.HAReplicationRateMbpsValue() != 20 {
		t.Errorf("the entry must inherit every key it does not name, got %+v", eff.HA)
	}
	if base.HAModeValue() != "off" {
		t.Errorf("the override must not change the job-level config, base mode = %s", base.HAModeValue())
	}
}

func TestApplyContextOverrides_HA_NestedSubBlocksMergeKeyByKey(t *testing.T) {
	t.Parallel()
	base := haBase(t, `{"mode":"auto","dlb":{"az_name":"balance"},"replication":{"schedule":"*/15","rate_mbps":20}}`)

	eff, _, _, err := config.ApplyContextOverrides(base, map[string]any{
		"pve_ha": map[string]any{
			"dlb":         map[string]any{"enabled": true},
			"replication": map[string]any{"rate_mbps": 0},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !eff.HADLBEnabledValue() || eff.HADLBAZNameValue() != "balance" {
		t.Errorf("dlb must merge: enabled=%v az_name=%q", eff.HADLBEnabledValue(), eff.HADLBAZNameValue())
	}
	if eff.HAReplicationScheduleValue() != "*/15" || eff.HAReplicationRateMbpsValue() != 0 {
		t.Errorf("replication must merge: schedule=%q rate=%d",
			eff.HAReplicationScheduleValue(), eff.HAReplicationRateMbpsValue())
	}
	if base.HADLBEnabledValue() || base.HAReplicationRateMbpsValue() != 20 {
		t.Error("the override must not write through to the job-level sub-blocks")
	}
}

func TestApplyContextOverrides_HA_EntryWithoutJobBlock(t *testing.T) {
	t.Parallel()
	base := haBase(t, "")
	eff, _, _, err := config.ApplyContextOverrides(base, map[string]any{
		"pve_ha": map[string]any{"mode": "managed", "dlb": map[string]any{"az_name": ""}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if eff.HAModeValue() != "managed" || eff.HADLBAZNameValue() != "" {
		t.Errorf("entry keys must apply over an absent job block, got %+v", eff.HA)
	}
	if base.HA != nil {
		t.Error("the override must not create a block on the job-level config")
	}
}

func TestApplyContextOverrides_HA_NestedSpellingFromTheDirector(t *testing.T) {
	t.Parallel()
	base := haBase(t, "")
	eff, applied, unknown, err := config.ApplyContextOverrides(base, map[string]any{
		"pve": map[string]any{"ha": map[string]any{"mode": "auto", "headroom": "off"}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(unknown) != 0 || len(applied) != 1 || applied[0] != "pve_ha" {
		t.Fatalf("applied = %v, unknown = %v, want [pve_ha] and none", applied, unknown)
	}
	if eff.HAModeValue() != "auto" || eff.HAHeadroomValue() != "off" {
		t.Errorf("nested ha block did not apply: %+v", eff.HA)
	}
}

func TestApplyContextOverrides_HA_NullAndEmptyInheritTheJobBlock(t *testing.T) {
	t.Parallel()
	base := haBase(t, `{"mode":"auto"}`)
	for name, v := range map[string]any{"null": nil, "empty": map[string]any{}} {
		eff, _, _, err := config.ApplyContextOverrides(base, map[string]any{"pve_ha": v})
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", name, err)
		}
		if eff.HAModeValue() != "auto" {
			t.Errorf("%s: the entry names no key, so the job mode must hold, got %s", name, eff.HAModeValue())
		}
	}
}

func TestApplyContextOverrides_HA_BooleanSpelling(t *testing.T) {
	t.Parallel()
	base := haBase(t, `{"mode":"auto"}`)
	eff, _, _, err := config.ApplyContextOverrides(base, map[string]any{
		"pve_ha": map[string]any{"mode": false},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if eff.HAModeValue() != "off" {
		t.Errorf("an unquoted off must read as off, got %s", eff.HAModeValue())
	}
}

func TestApplyContextOverrides_HA_RejectsMisspelledAndMalformed(t *testing.T) {
	t.Parallel()
	base := haBase(t, `{"mode":"auto"}`)
	tests := []struct {
		name string
		v    any
		want string
	}{
		{"misspelled key", map[string]any{"modee": "auto"}, `unknown field "modee"`},
		{"misspelled nested key", map[string]any{"dlb": map[string]any{"enable": true}}, `unknown field "enable"`},
		{"misspelled replication key", map[string]any{"replication": map[string]any{"rate": 1}}, `unknown field "rate"`},
		{"string block", "auto", `expected an ha object, got string`},
		{"list block", []any{"auto"}, `expected an ha object, got []interface {}`},
		{"wrong type", map[string]any{"motion_wait_sec": "soon"}, `cannot decode ha block`},
		{"fractional int", map[string]any{"motion_wait_sec": 12.5}, `cannot decode ha block`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			eff, _, _, err := config.ApplyContextOverrides(base, map[string]any{"pve_ha": tc.v})
			if err == nil {
				t.Fatalf("expected an error, got effective config %+v", eff)
			}
			if eff != nil {
				t.Error("effective config must be nil on error")
			}
			if !strings.Contains(err.Error(), `context override "pve_ha"`) || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q must name pve_ha and contain %q", err, tc.want)
			}
		})
	}
}

func TestApplyContextOverrides_HA_ValidatesTheMergedResult(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		base string
		over map[string]any
		want string
	}{
		{"bad enum", `{"mode":"auto"}`, map[string]any{"headroom": "always"}, `ha.headroom must be one of`},
		{"entry turns the mode off under a job on", `{"mode":"auto","anti_affinity":"on"}`,
			map[string]any{"mode": "off"}, `ha.anti_affinity "on" requires pve.ha.mode`},
		{"entry sets on under mode off", `{}`, map[string]any{"pin_az": "on"}, `ha.pin_az "on" requires pve.ha.mode`},
		{"entry sets dlb under mode off", `{}`, map[string]any{"dlb": map[string]any{"enabled": true}},
			`ha.dlb.enabled requires pve.ha.mode`},
		{"hold", `{"mode":"auto"}`, map[string]any{"local_policy": "hold"},
			`ha.local_policy "hold" is not available in this build`},
		{"replicate", `{"mode":"auto"}`, map[string]any{"local_policy": "replicate"},
			`ha.local_policy "replicate" is not available in this build`},
		{"motion wait", `{}`, map[string]any{"motion_wait_sec": 0}, `ha.motion_wait_sec must be between`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			eff, _, _, err := config.ApplyContextOverrides(haBase(t, tc.base), map[string]any{"pve_ha": tc.over})
			if err == nil {
				t.Fatalf("expected a validation error, got %+v", eff)
			}
			if !strings.Contains(err.Error(), "effective override config failed validation") ||
				!strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q must be a validation failure containing %q", err, tc.want)
			}
		})
	}
}

func TestInspectContextKeys_AcceptsHABlock(t *testing.T) {
	t.Parallel()
	findings, err := config.InspectContextKeys(map[string]any{
		"pve": map[string]any{"ha": map[string]any{"mode": "auto"}},
	})
	if err != nil {
		t.Fatalf("a pve.ha block is a spec name and must be accepted: %v", err)
	}
	if len(findings.MisnestedSpecKeys) != 0 {
		t.Errorf("unexpected findings: %+v", findings)
	}
}
