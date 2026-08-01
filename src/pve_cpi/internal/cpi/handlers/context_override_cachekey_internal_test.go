package handlers

import (
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
)

// TestRequestOverrideCacheKey_CoversEveryOverridableField asserts that every
// field ApplyContextOverrides can change also changes requestOverrideCacheKey.
// The cache key is a hand-written hash over the overridable fields; a new
// overridable field that feeds the cached bundle but is absent from the key
// would let two requests with different effective configs collide on one
// cached PVE client — the second silently executing against the first's
// cluster, which is the exact defect the override feature exists to prevent.
// Driving the assertion from config.ContextOverrideFieldOrderForTest keeps
// this test in lockstep with the authoritative field list: adding a field
// there without extending requestOverrideCacheKey fails here.
func TestRequestOverrideCacheKey_CoversEveryOverridableField(t *testing.T) {
	t.Parallel()

	base := &config.CPIConfig{
		Host:            "pve-a.example",
		Port:            8006,
		User:            "cpi",
		Realm:           "pam",
		Node:            "pve1",
		Password:        "pw-one",
		VMStorage:       "vmstore",
		DiskStorage:     "diskstore",
		StemcellStorage: "stemstore",
		ISOStorage:      "isostore",
		NetworkBridge:   "vmbr0",
		VMIDRangeStart:  100,
		VMIDRangeEnd:    8999,
		AgentMode:       config.AgentModeCloudInit,
		VMDiskFormat:    "qcow2",
		AgentMBus:       "nats://mbus-a",
	}
	// ApplyContextOverrides re-validates the effective config, so the base
	// must be a fully-defaulted, valid config first.
	base.ApplyDefaults()
	baseKey := requestOverrideCacheKey(base)

	// One distinct override value per field. Values only need to differ from
	// base and coerce cleanly; enum validity is enforced later by Validate(),
	// not by ApplyContextOverrides.
	overrideValues := map[string]any{
		"pve_host":             "pve-b.example",
		"pve_port":             float64(9999), // JSON numbers decode as float64
		"pve_user":             "other",
		"pve_password":         "pw-two",
		"pve_api_token":        "tok-two",
		"pve_realm":            "pve",
		"pve_node":             "pve2",
		"pve_vm_storage":       "vmstore-b",
		"pve_disk_storage":     "diskstore-b",
		"pve_stemcell_storage": "stemstore-b",
		"pve_iso_storage":      "isostore-b",
		"pve_network_bridge":   "vmbr1",
		"pve_verify_ssl":       false,
		"pve_vmid_range_start": float64(5000),
		"pve_vmid_range_end":   float64(8000),
		"pve_agent_mode":       config.AgentModeNoAgent,
		"pve_vm_disk_format":   "vmdk",
		"pve_agent_mbus":       "nats://mbus-b",
	}

	for _, field := range config.ContextOverrideFieldOrderForTest() {
		val, ok := overrideValues[field]
		if !ok {
			t.Errorf("field %q is overridable but this test has no distinct value for it — add one so the cache key stays covered", field)
			continue
		}
		eff, applied, unknown, err := config.ApplyContextOverrides(base, map[string]any{field: val})
		if err != nil {
			t.Errorf("ApplyContextOverrides(%q): %v", field, err)
			continue
		}
		if len(unknown) != 0 || len(applied) != 1 {
			t.Errorf("ApplyContextOverrides(%q): applied=%v unknown=%v; want exactly one applied", field, applied, unknown)
			continue
		}
		if key := requestOverrideCacheKey(eff); key == baseKey {
			t.Errorf("overriding %q did not change requestOverrideCacheKey — two requests with different effective configs would share one cached bundle", field)
		}
	}
}
