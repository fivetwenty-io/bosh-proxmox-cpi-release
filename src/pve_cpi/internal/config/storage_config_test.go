package config_test

import (
	"strconv"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
)

// ---------------------------------------------------------------------------
// pve.storage.max_utilization_pct / max_utilization_mode tests
// ---------------------------------------------------------------------------

// intPtr (constructs *int field literals) is defined in config_test.go.

// TestMaxUtilizationPctValue_DefaultOff verifies the gate is disabled (0) by
// default: nil Storage, nil field, and an explicit 0 all resolve to 0.
func TestMaxUtilizationPctValue_DefaultOff(t *testing.T) {
	t.Parallel()
	cases := []*config.CPIConfig{
		{},                                 // nil Storage
		{Storage: &config.StorageConfig{}}, // nil MaxUtilizationPct
		{Storage: &config.StorageConfig{MaxUtilizationPct: intPtr(0)}}, // explicit 0
	}
	for i, cfg := range cases {
		if got := cfg.MaxUtilizationPctValue(); got != 0 {
			t.Errorf("case %d: MaxUtilizationPctValue() = %d; want 0 (disabled)", i, got)
		}
	}
}

// TestMaxUtilizationPctValue_NilReceiver verifies a nil *CPIConfig is safe and
// resolves to disabled, matching the nil-safe accessor pattern used elsewhere.
func TestMaxUtilizationPctValue_NilReceiver(t *testing.T) {
	t.Parallel()
	var cfg *config.CPIConfig
	if got := cfg.MaxUtilizationPctValue(); got != 0 {
		t.Errorf("nil receiver: MaxUtilizationPctValue() = %d; want 0", got)
	}
	if !cfg.MaxUtilizationEnforce() {
		t.Error("nil receiver: MaxUtilizationEnforce() = false; want true (enforce default)")
	}
}

// TestMaxUtilizationPctValue_ExplicitPositive verifies a positive value is
// returned as-is.
func TestMaxUtilizationPctValue_ExplicitPositive(t *testing.T) {
	t.Parallel()
	cfg := &config.CPIConfig{Storage: &config.StorageConfig{MaxUtilizationPct: intPtr(80)}}
	if got := cfg.MaxUtilizationPctValue(); got != 80 {
		t.Errorf("MaxUtilizationPctValue() = %d; want 80", got)
	}
}

// TestMaxUtilizationEnforce_DefaultTrue verifies enforce is the default when
// the mode is unset, nil Storage, or empty string.
func TestMaxUtilizationEnforce_DefaultTrue(t *testing.T) {
	t.Parallel()
	cases := []*config.CPIConfig{
		{}, // nil Storage
		{Storage: &config.StorageConfig{MaxUtilizationPct: intPtr(80)}}, // empty mode
		{Storage: &config.StorageConfig{MaxUtilizationPct: intPtr(80), MaxUtilizationMode: "enforce"}},
		{Storage: &config.StorageConfig{MaxUtilizationPct: intPtr(80), MaxUtilizationMode: "Enforce"}}, // case-insensitive
	}
	for i, cfg := range cases {
		if !cfg.MaxUtilizationEnforce() {
			t.Errorf("case %d: MaxUtilizationEnforce() = false; want true", i)
		}
	}
}

// TestMaxUtilizationEnforce_Warn verifies "warn" (case/whitespace-insensitive)
// disables enforcement.
func TestMaxUtilizationEnforce_Warn(t *testing.T) {
	t.Parallel()
	cases := []string{"warn", "WARN", " warn ", "Warn"}
	for _, mode := range cases {
		cfg := &config.CPIConfig{Storage: &config.StorageConfig{MaxUtilizationPct: intPtr(80), MaxUtilizationMode: mode}}
		if cfg.MaxUtilizationEnforce() {
			t.Errorf("mode %q: MaxUtilizationEnforce() = true; want false", mode)
		}
	}
}

// TestMaxUtilizationPct_JSONRoundTrip verifies both knobs survive JSON
// marshal/unmarshal through the nested "storage" block.
func TestMaxUtilizationPct_JSONRoundTrip(t *testing.T) {
	t.Parallel()
	cfg, err := mustLoad(t, `{
		"host": "h", "user": "u", "password": "p",
		"vm_storage": "s", "disk_storage": "s", "network_bridge": "br",
		"storage": {"max_utilization_pct": 80, "max_utilization_mode": "warn"}
	}`)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if got := cfg.MaxUtilizationPctValue(); got != 80 {
		t.Errorf("after JSON round-trip: MaxUtilizationPctValue() = %d; want 80", got)
	}
	if cfg.MaxUtilizationEnforce() {
		t.Error("after JSON round-trip: MaxUtilizationEnforce() = true; want false (mode=warn)")
	}
}

// TestValidateStorage_NilBlockIsValid verifies a fully absent Storage block
// passes validation (zero behavior change, the default).
func TestValidateStorage_NilBlockIsValid(t *testing.T) {
	t.Parallel()
	_, err := mustLoad(t, `{
		"host": "h", "user": "u", "password": "p",
		"vm_storage": "s", "disk_storage": "s", "network_bridge": "br"
	}`)
	if err != nil {
		t.Fatalf("Load returned unexpected error: %v", err)
	}
}

// TestValidateStorage_PctOutOfRange verifies pct outside [0, 100] is rejected.
func TestValidateStorage_PctOutOfRange(t *testing.T) {
	t.Parallel()
	for _, pct := range []int{-1, 101, 1000} {
		_, err := mustLoad(t, `{
			"host": "h", "user": "u", "password": "p",
			"vm_storage": "s", "disk_storage": "s", "network_bridge": "br",
			"storage": {"max_utilization_pct": `+strconv.Itoa(pct)+`}
		}`)
		assertCloudError(t, err, "storage.max_utilization_pct must be between 0 and 100")
	}
}

// TestValidateStorage_PctInRange verifies boundary values 0 and 100 are valid.
func TestValidateStorage_PctInRange(t *testing.T) {
	t.Parallel()
	for _, pct := range []int{0, 1, 80, 100} {
		_, err := mustLoad(t, `{
			"host": "h", "user": "u", "password": "p",
			"vm_storage": "s", "disk_storage": "s", "network_bridge": "br",
			"storage": {"max_utilization_pct": `+strconv.Itoa(pct)+`}
		}`)
		if err != nil {
			t.Errorf("pct=%d: Load returned unexpected error: %v", pct, err)
		}
	}
}

// TestValidateStorage_ModeEnum verifies max_utilization_mode is restricted to
// enforce|warn, and that the check runs even when max_utilization_pct is 0
// (the gate being off does not excuse a config typo).
func TestValidateStorage_ModeEnum(t *testing.T) {
	t.Parallel()
	_, err := mustLoad(t, `{
		"host": "h", "user": "u", "password": "p",
		"vm_storage": "s", "disk_storage": "s", "network_bridge": "br",
		"storage": {"max_utilization_mode": "block"}
	}`)
	assertCloudError(t, err, "storage.max_utilization_mode must be one of enforce|warn")
}

// TestValidateStorage_ModeEnumValid verifies both accepted enum values pass,
// case-insensitively.
func TestValidateStorage_ModeEnumValid(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"enforce", "warn", "ENFORCE", "Warn"} {
		_, err := mustLoad(t, `{
			"host": "h", "user": "u", "password": "p",
			"vm_storage": "s", "disk_storage": "s", "network_bridge": "br",
			"storage": {"max_utilization_pct": 80, "max_utilization_mode": "`+mode+`"}
		}`)
		if err != nil {
			t.Errorf("mode=%q: Load returned unexpected error: %v", mode, err)
		}
	}
}
