package config_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
)

// layeredBaseCfg returns a minimal CPIConfig that passes Validate, used as the
// base for layered-resolver field tests so each test only sets the field under
// examination.
func layeredBaseCfg() *config.CPIConfig {
	return &config.CPIConfig{
		Host:           "h",
		User:           "u",
		Password:       "p",
		VMStorage:      "s",
		DiskStorage:    "s",
		NetworkBridge:  "br",
		Port:           8006,
		AgentMode:      "cloudinit",
		VMDiskFormat:   "qcow2",
		LogLevel:       "info",
		VMIDRangeStart: 100,
		VMIDRangeEnd:   5999,
		RebootMode:     "soft",
		RebootTimeout:  60,
		NetworkMode:    "auto",
		SDNZoneType:    "simple",
	}
}

// --------------------------------------------------------------------------
// VMTypes
// --------------------------------------------------------------------------

func TestValidate_VMTypes_EmptyAbsent_OK(t *testing.T) {
	t.Parallel()
	cfg := layeredBaseCfg()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("absent VMTypes must not fail: %v", err)
	}
}

func TestValidate_VMTypes_EmptyMap_OK(t *testing.T) {
	t.Parallel()
	cfg := layeredBaseCfg()
	cfg.VMTypes = map[string]config.TypeProfile{}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("empty VMTypes map must not fail: %v", err)
	}
}

func TestValidate_VMTypes_PopulatedProfile_OK(t *testing.T) {
	t.Parallel()
	cfg := layeredBaseCfg()
	cfg.VMTypes = map[string]config.TypeProfile{
		"small": {CloudProperties: map[string]any{"cpu": 1, "ram": 1024}},
		"large": {CloudProperties: map[string]any{"cpu": 8, "ram": 16384}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("populated VMTypes must not fail: %v", err)
	}
}

func TestValidate_VMTypes_EmptyProfileCloudProperties_OK(t *testing.T) {
	t.Parallel()
	// A named profile with no cloud_properties is still valid.
	cfg := layeredBaseCfg()
	cfg.VMTypes = map[string]config.TypeProfile{
		"bare": {},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("VMTypes profile with empty CloudProperties must not fail: %v", err)
	}
}

// --------------------------------------------------------------------------
// DiskTypes
// --------------------------------------------------------------------------

func TestValidate_DiskTypes_EmptyAbsent_OK(t *testing.T) {
	t.Parallel()
	cfg := layeredBaseCfg()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("absent DiskTypes must not fail: %v", err)
	}
}

func TestValidate_DiskTypes_EmptyMap_OK(t *testing.T) {
	t.Parallel()
	cfg := layeredBaseCfg()
	cfg.DiskTypes = map[string]config.TypeProfile{}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("empty DiskTypes map must not fail: %v", err)
	}
}

func TestValidate_DiskTypes_PopulatedProfile_OK(t *testing.T) {
	t.Parallel()
	cfg := layeredBaseCfg()
	cfg.DiskTypes = map[string]config.TypeProfile{
		"ssd":  {CloudProperties: map[string]any{"type": "lvmthin", "size": 10240}},
		"bulk": {CloudProperties: map[string]any{"type": "nfs", "size": 204800}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("populated DiskTypes must not fail: %v", err)
	}
}

// --------------------------------------------------------------------------
// SecurityGroups
// --------------------------------------------------------------------------

func TestValidate_SecurityGroups_EmptyAbsent_OK(t *testing.T) {
	t.Parallel()
	cfg := layeredBaseCfg()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("absent SecurityGroups must not fail: %v", err)
	}
}

func TestValidate_SecurityGroups_EmptySlice_OK(t *testing.T) {
	t.Parallel()
	cfg := layeredBaseCfg()
	cfg.SecurityGroups = []string{}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("empty SecurityGroups slice must not fail: %v", err)
	}
}

func TestValidate_SecurityGroups_Populated_OK(t *testing.T) {
	t.Parallel()
	cfg := layeredBaseCfg()
	cfg.SecurityGroups = []string{"default", "web-tier"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("populated SecurityGroups must not fail: %v", err)
	}
}

// --------------------------------------------------------------------------
// StorageTiers — valid cases
// --------------------------------------------------------------------------

func TestValidate_StorageTiers_EmptyAbsent_OK(t *testing.T) {
	t.Parallel()
	cfg := layeredBaseCfg()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("absent StorageTiers must not fail: %v", err)
	}
}

func TestValidate_StorageTiers_EmptyMap_OK(t *testing.T) {
	t.Parallel()
	cfg := layeredBaseCfg()
	cfg.StorageTiers = map[string]config.StorageTierCriteria{}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("empty StorageTiers map must not fail: %v", err)
	}
}

func TestValidate_StorageTiers_TypesOnly_OK(t *testing.T) {
	t.Parallel()
	cfg := layeredBaseCfg()
	cfg.StorageTiers = map[string]config.StorageTierCriteria{
		"fast": {Types: []string{"lvmthin", "zfspool"}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("types-only criteria must not fail: %v", err)
	}
}

func TestValidate_StorageTiers_SharedOnly_OK(t *testing.T) {
	t.Parallel()
	cfg := layeredBaseCfg()
	shared := true
	cfg.StorageTiers = map[string]config.StorageTierCriteria{
		"shared": {Shared: &shared},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("shared-only criteria must not fail: %v", err)
	}
}

func TestValidate_StorageTiers_SharedFalseOnly_OK(t *testing.T) {
	t.Parallel()
	// Shared=false (local-only) is a valid non-nil constraint.
	cfg := layeredBaseCfg()
	shared := false
	cfg.StorageTiers = map[string]config.StorageTierCriteria{
		"local": {Shared: &shared},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("shared=false criteria must not fail: %v", err)
	}
}

func TestValidate_StorageTiers_TypesAndShared_OK(t *testing.T) {
	t.Parallel()
	cfg := layeredBaseCfg()
	shared := true
	cfg.StorageTiers = map[string]config.StorageTierCriteria{
		"fast-shared": {Types: []string{"rbd", "cephfs"}, Shared: &shared},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("types+shared criteria must not fail: %v", err)
	}
}

func TestValidate_StorageTiers_AllKnownTypes_OK(t *testing.T) {
	t.Parallel()
	// Every PVE storage type string must be accepted individually.
	known := []string{
		"lvm", "lvmthin", "zfspool", "dir", "nfs", "cifs", "rbd", "cephfs",
		"btrfs", "glusterfs", "pbs",
	}
	for _, st := range known {
		st := st
		t.Run(st, func(t *testing.T) {
			t.Parallel()
			cfg := layeredBaseCfg()
			cfg.StorageTiers = map[string]config.StorageTierCriteria{
				"tier": {Types: []string{st}},
			}
			if err := cfg.Validate(); err != nil {
				t.Fatalf("known type %q must not fail: %v", st, err)
			}
		})
	}
}

// --------------------------------------------------------------------------
// StorageTiers — error cases
// --------------------------------------------------------------------------

func TestValidate_StorageTiers_NeitherTypesNorShared_Error(t *testing.T) {
	t.Parallel()
	cfg := layeredBaseCfg()
	cfg.StorageTiers = map[string]config.StorageTierCriteria{
		"empty-tier": {},
	}
	err := cfg.Validate()
	assertCloudError(t, err, "storage_tiers[empty-tier]")
	assertCloudError(t, err, "must set at least one of types, shared, or encrypted")
}

func TestValidate_StorageTiers_UnknownType_Error(t *testing.T) {
	t.Parallel()
	cfg := layeredBaseCfg()
	cfg.StorageTiers = map[string]config.StorageTierCriteria{
		"weird": {Types: []string{"bogusfs"}},
	}
	err := cfg.Validate()
	assertCloudError(t, err, "storage_tiers[weird]")
	assertCloudError(t, err, "bogusfs")
}

func TestValidate_StorageTiers_MixedKnownUnknown_Error(t *testing.T) {
	t.Parallel()
	// Even with one valid type, an unknown type in the same slice is rejected.
	cfg := layeredBaseCfg()
	cfg.StorageTiers = map[string]config.StorageTierCriteria{
		"mixed": {Types: []string{"lvm", "unknownfs"}},
	}
	err := cfg.Validate()
	assertCloudError(t, err, "storage_tiers[mixed]")
	assertCloudError(t, err, "unknownfs")
}

func TestValidate_StorageTiers_MultipleTiers_OneBad_Error(t *testing.T) {
	t.Parallel()
	shared := true
	cfg := layeredBaseCfg()
	cfg.StorageTiers = map[string]config.StorageTierCriteria{
		"good-tier": {Types: []string{"nfs"}, Shared: &shared},
		"bad-tier":  {}, // neither types nor shared
	}
	err := cfg.Validate()
	assertCloudError(t, err, "storage_tiers[bad-tier]")
}

func TestValidate_StorageTiers_MultipleBad_BothNamed(t *testing.T) {
	t.Parallel()
	cfg := layeredBaseCfg()
	cfg.StorageTiers = map[string]config.StorageTierCriteria{
		"alpha": {},
		"beta":  {Types: []string{"notarealtype"}},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for two invalid tiers, got nil")
	}
	msg := err.Error()
	// Both tier names appear in the aggregated error.
	if !strings.Contains(msg, "alpha") && !strings.Contains(msg, "beta") {
		t.Errorf("error should name at least one bad tier; got: %v", msg)
	}
}

// --------------------------------------------------------------------------
// JSON round-trip (ensures json tags wire correctly)
// --------------------------------------------------------------------------

func TestLoad_LayeredFields_RoundTrip(t *testing.T) {
	t.Parallel()
	sharedVal := true
	cfg := layeredBaseCfg()
	cfg.VMTypes = map[string]config.TypeProfile{
		"web": {CloudProperties: map[string]any{"cpu": float64(2)}},
	}
	cfg.DiskTypes = map[string]config.TypeProfile{
		"ssd": {CloudProperties: map[string]any{"size": float64(10240)}},
	}
	cfg.StorageTiers = map[string]config.StorageTierCriteria{
		"fast": {Types: []string{"lvmthin"}, Shared: &sharedVal},
	}
	cfg.SecurityGroups = []string{"base", "app"}

	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	loaded, err := mustLoad(t, string(raw))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if len(loaded.VMTypes) != 1 {
		t.Errorf("VMTypes: want 1 entry, got %d", len(loaded.VMTypes))
	}
	if len(loaded.DiskTypes) != 1 {
		t.Errorf("DiskTypes: want 1 entry, got %d", len(loaded.DiskTypes))
	}
	if len(loaded.StorageTiers) != 1 {
		t.Errorf("StorageTiers: want 1 entry, got %d", len(loaded.StorageTiers))
	}
	tier, ok := loaded.StorageTiers["fast"]
	if !ok {
		t.Fatal("StorageTiers[fast] missing after round-trip")
	}
	if tier.Shared == nil || !*tier.Shared {
		t.Errorf("StorageTiers[fast].Shared: want *true, got %v", tier.Shared)
	}
	if len(loaded.SecurityGroups) != 2 {
		t.Errorf("SecurityGroups: want 2, got %d", len(loaded.SecurityGroups))
	}
	// Validate that the loaded config still passes.
	if err := loaded.Validate(); err != nil {
		t.Fatalf("round-tripped config must validate: %v", err)
	}
}
