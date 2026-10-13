package handlers

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"slices"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
)

// selfSignedCAPEMForCacheKeyTest returns a freshly generated self-signed CA
// certificate as PEM, so the pve_ca_cert row has a value that validates.
func selfSignedCAPEMForCacheKeyTest(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "cache-key-test-ca"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

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
		// Parker band set in the base so single-bound parker overrides
		// validate against a real opposite bound.
		ParkedDiskVMIDRangeStart: 90000,
		ParkedDiskVMIDRangeEnd:   90999,
		VMDiskFormat:             "qcow2",
		AgentMBus:                "nats://mbus-a",
	}
	// ApplyContextOverrides re-validates the effective config, so the base
	// must be a fully-defaulted, valid config first.
	base.StorageSets = map[string]config.StorageSet{
		"set-a": {Names: []string{"nfs-a"}, Strategy: config.StoragePlacementStrategy{Name: "spread", Version: 1}},
	}
	base.ApplyDefaults()
	baseKey := requestOverrideCacheKey(base)

	// One distinct override value per field. Values only need to differ from
	// base and coerce cleanly; enum validity is enforced later by Validate(),
	// not by ApplyContextOverrides.
	overrideValues := map[string]any{
		"pve_storage_sets": map[string]any{"set-a": map[string]any{
			"names": []string{"nfs-b"}, "strategy": map[string]any{"name": "spread", "version": 1},
		}},
		"pve_storage_capacity_domains":           map[string]any{"nas": map[string]any{"members": []string{"nfs-a"}}},
		"pve_ephemeral_storage_set":              "set-a",
		"pve_persistent_storage_set":             "set-a",
		"pve_root_storage_set":                   "set-a",
		"pve_storage_placement_namespace":        "other-authority",
		"pve_require_disjoint_storage_sets":      false,
		"pve_storage_status_max_age_seconds":     12,
		"pve_ca_cert":                            selfSignedCAPEMForCacheKeyTest(t),
		"pve_host":                               "pve-b.example",
		"pve_port":                               float64(9999), // JSON numbers decode as float64
		"pve_user":                               "other",
		"pve_password":                           "pw-two",
		"pve_api_token":                          "tok-two",
		"pve_realm":                              "pve",
		"pve_node":                               "pve2",
		"pve_vm_storage":                         "vmstore-b",
		"pve_disk_storage":                       "diskstore-b",
		"pve_stemcell_storage":                   "stemstore-b",
		"pve_iso_storage":                        "isostore-b",
		"pve_network_bridge":                     "vmbr1",
		"pve_verify_ssl":                         false,
		"pve_vmid_range_start":                   float64(5000),
		"pve_vmid_range_end":                     float64(8000),
		"pve_disk_vmid_range_start":              float64(20000),
		"pve_disk_vmid_range_end":                float64(28999),
		"pve_stemcell_template_vmid_range_start": float64(30500),
		"pve_stemcell_template_vmid_range_end":   float64(30998),
		"pve_parked_disk_vmid_range_start":       float64(90100),
		"pve_parked_disk_vmid_range_end":         float64(90900),
		"pve_detached_disk_strategy":             "free",
		"pve_disk_migration":                     "off",
		"pve_stemcell_replicate_local":           true,
		"pve_stemcell_replicate_storage_set":     true,
		"pve_vm_prefix":                          "az2",
		"pve_parker_prefix":                      "az2",
		"pve_parker_pool":                        "az2-parker",
		"pve_vm_ownership_guard":                 "warn",
		"pve_cluster_stamp_aliases":              []any{"0a1b2c3d"},
		"pve_agent_mode":                         config.AgentModeNoAgent,
		"pve_vm_disk_format":                     "vmdk",
		"pve_agent_mbus":                         "nats://mbus-b",
		// placement is a nested block, not a scalar: az_map names PVE NODES,
		// which are meaningful only within one cluster, so two entries with
		// different placement must not share a cached bundle.
		"pve_placement": map[string]any{
			"az_map":              map[string]any{"z3": []any{"pve-az2-1"}},
			"pin_az_via_ha_rules": true,
		},
		"pve_network_mode":                  config.NetworkModeSDN,
		"pve_sdn_zone":                      "zone-b",
		"pve_sdn_zone_type":                 "evpn",
		"pve_sdn_auto_manage_zone":          false,
		"pve_sdn_vxlan_peers":               []any{"10.20.0.1", "10.20.0.2"},
		"pve_sdn_vni_range_start":           float64(5500),
		"pve_sdn_vni_range_end":             float64(5900),
		"pve_sdn_zone_mtu":                  float64(1400),
		"pve_vm_pool":                       "az2-vms",
		"pve_stemcell_template_pool":        "az2-templates",
		"pve_stemcell_strategy":             config.StemcellStrategyImport,
		"pve_stemcell_template_node":        "pve2",
		"pve_require_shared_iso_for_ha":     true,
		"pve_iso_storage_follow_vm_storage": false,
		"pve_destroy_unreferenced_disks":    true,
		// The profile maps replace the job's whole map, and profiles can
		// name storages and nodes, so two entries with different profiles
		// must not share a cached bundle.
		"pve_vm_types": map[string]any{
			"small": map[string]any{"cloud_properties": map[string]any{"cpu": float64(2)}},
		},
		"pve_disk_types": map[string]any{
			"fast": map[string]any{"cloud_properties": map[string]any{"vm_storage": "ssd-b"}},
		},
	}

	// An explicit stemcell_replicate_storage_set validates against the
	// effective root set, so a true cannot be applied to a base with no
	// binding. The base itself must
	// stay unbound, because an inherited binding makes ApplyContextOverrides
	// refuse the pve_host and pve_port rows unless the entry also restates
	// pve_storage_sets. This field therefore gets a bound base of its own.
	// Credentials travel with the endpoint as a group, so a row that changes
	// the endpoint or names one credential key needs the rest of a complete
	// group beside it. Each companion restates the base's own value, so the
	// row's field stays the only difference from the base that the cache key
	// can see (the password row also clears nothing, because the base holds
	// no token).
	companions := map[string]map[string]any{
		"pve_host":      {"pve_user": "cpi", "pve_password": "pw-one"},
		"pve_port":      {"pve_user": "cpi", "pve_password": "pw-one"},
		"pve_user":      {"pve_password": "pw-one"},
		"pve_password":  {"pve_user": "cpi"},
		"pve_api_token": {"pve_user": "cpi"},
	}

	boundBase := *base
	boundBase.EphemeralStorageSet = "set-a"
	boundBaseKey := requestOverrideCacheKey(&boundBase)

	for _, field := range config.ContextOverrideFieldOrderForTest() {
		val, ok := overrideValues[field]
		if !ok {
			t.Errorf("field %q is overridable but this test has no distinct value for it — add one so the cache key stays covered", field)
			continue
		}
		fieldBase, fieldBaseKey := base, baseKey
		if field == "pve_stemcell_replicate_storage_set" {
			fieldBase, fieldBaseKey = &boundBase, boundBaseKey
		}
		extra := map[string]any{field: val}
		for k, v := range companions[field] {
			extra[k] = v
		}
		eff, applied, unknown, err := config.ApplyContextOverrides(fieldBase, extra)
		if err != nil {
			t.Errorf("ApplyContextOverrides(%q): %v", field, err)
			continue
		}
		if len(unknown) != 0 || len(applied) != len(extra) || !slices.Contains(applied, field) {
			t.Errorf("ApplyContextOverrides(%q): applied=%v unknown=%v; want the field and its companions applied",
				field, applied, unknown)
			continue
		}
		if key := requestOverrideCacheKey(eff); key == fieldBaseKey {
			t.Errorf("overriding %q did not change requestOverrideCacheKey — two requests with different effective configs would share one cached bundle", field)
		}
	}
}

// TestRequestOverrideCacheKey_CACertSeparatesSameHost asserts that two
// entries on one host with different trust roots never share a cached
// client. TestRequestOverrideCacheKeyIsUnchangedWithoutAntiAffinity pins
// that a config with no CA still keys exactly as it did before.
func TestRequestOverrideCacheKey_CACertSeparatesSameHost(t *testing.T) {
	t.Parallel()
	a := &config.CPIConfig{Host: "pve-a.example", Port: 8006, User: "cpi", Password: "pw-one"}
	b := *a
	b.PVECACertPEM = selfSignedCAPEMForCacheKeyTest(t)
	c := *a
	c.PVECACertPEM = selfSignedCAPEMForCacheKeyTest(t)
	keyA, keyB, keyC := requestOverrideCacheKey(a), requestOverrideCacheKey(&b), requestOverrideCacheKey(&c)
	if keyA == keyB || keyB == keyC || keyA == keyC {
		t.Error("configs that differ only in pve_ca_cert must key differently")
	}
	same := *a
	if requestOverrideCacheKey(&same) != keyA {
		t.Error("the cache key must be deterministic")
	}
}
