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
	baseKey := requestOverrideCacheKey(base, base)

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
		"pve_api_endpoint_discovery":             "on",
		"pve_host":                               "pve-b.example",
		"pve_port":                               float64(9999), // JSON numbers decode as float64
		"pve_endpoints":                          []any{"pve-a.example", "pve-c.example"},
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
			"az_map":        map[string]any{"z3": []any{"pve-az2-1"}},
			"anti_affinity": map[string]any{"enabled": true},
		},
		// The HA block is merged per entry, and two entries that differ only
		// in their HA settings must not share a cached bundle.
		"pve_ha":                            map[string]any{"mode": "auto", "headroom": "enforce"},
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
		"pve_host": {"pve_user": "cpi", "pve_password": "pw-one"},
		"pve_port": {"pve_user": "cpi", "pve_password": "pw-one"},
		// A second endpoint the job never named gets no inherited credentials.
		"pve_endpoints": {"pve_user": "cpi", "pve_password": "pw-one"},
		"pve_user":      {"pve_password": "pw-one"},
		"pve_password":  {"pve_user": "cpi"},
		"pve_api_token": {"pve_user": "cpi"},
	}

	boundBase := *base
	boundBase.EphemeralStorageSet = "set-a"
	boundBaseKey := requestOverrideCacheKey(&boundBase, &boundBase)

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
		if key := requestOverrideCacheKey(fieldBase, eff); key == fieldBaseKey {
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
	keyA, keyB, keyC := requestOverrideCacheKey(nil, a), requestOverrideCacheKey(nil, &b), requestOverrideCacheKey(nil, &c)
	if keyA == keyB || keyB == keyC || keyA == keyC {
		t.Error("configs that differ only in pve_ca_cert must key differently")
	}
	same := *a
	if requestOverrideCacheKey(nil, &same) != keyA {
		t.Error("the cache key must be deterministic")
	}
}

// TestRequestOverrideCacheKey_EndpointListSeparatesConfigs asserts that two
// configs that differ only in their endpoint list never share a cached
// client, that the order of the list counts, that no list keys exactly as a
// config did before the list existed, and that elements cannot run together.
func TestRequestOverrideCacheKey_EndpointListSeparatesConfigs(t *testing.T) {
	t.Parallel()
	mk := func(list ...string) *config.CPIConfig {
		return &config.CPIConfig{Host: "pve-a.example", Port: 8006, User: "cpi", Password: "pw-one", Endpoints: list}
	}
	none := requestOverrideCacheKey(nil, mk())
	if empty := requestOverrideCacheKey(nil, &config.CPIConfig{
		Host: "pve-a.example", Port: 8006, User: "cpi", Password: "pw-one", Endpoints: []string{},
	}); empty != none {
		t.Error("an empty list must key like no list")
	}
	keys := map[string]string{
		"no list":        none,
		"one":            requestOverrideCacheKey(nil, mk("pve-a.example:8006")),
		"two":            requestOverrideCacheKey(nil, mk("pve-a.example:8006", "pve-b.example:8006")),
		"two, other":     requestOverrideCacheKey(nil, mk("pve-a.example:8006", "pve-c.example:8006")),
		"two, reordered": requestOverrideCacheKey(nil, mk("pve-b.example:8006", "pve-a.example:8006")),
		"joined":         requestOverrideCacheKey(nil, mk("pve-a.example:8006,pve-b.example:8006")),
		"three": requestOverrideCacheKey(nil, mk("pve-a.example:8006", "pve-b.example:8006",
			"pve-c.example:8006")),
	}
	seen := map[string]string{}
	for name, key := range keys {
		if prev, dup := seen[key]; dup {
			t.Errorf("%q and %q share a cache key", prev, name)
		}
		seen[key] = name
	}
	if requestOverrideCacheKey(nil, mk("pve-a.example:8006", "pve-b.example:8006")) != keys["two"] {
		t.Error("the cache key must be deterministic")
	}
}

// TestRequestOverrideCacheKey_NoListKeysAsBeforeTheListExisted pins the key of
// a fixed config that holds no endpoint list to the value the key function
// produced before the list was folded in. A later edit that writes anything
// for an empty list changes the key and fails here, which would otherwise
// drop every warm cached client on upgrade.
func TestRequestOverrideCacheKey_NoListKeysAsBeforeTheListExisted(t *testing.T) {
	t.Parallel()
	const golden = "e1b319f637957cfa0d54052d63706dd2951c0350482db1975f46542f0d2d84d3"
	cfg := &config.CPIConfig{
		Host: "pve-a.example", Port: 8006, User: "cpi", Realm: "pam", Node: "pve1", Password: "pw-one",
		VMStorage: "local-lvm", DiskStorage: "local-lvm", NetworkBridge: "vmbr0",
	}
	if got := requestOverrideCacheKey(nil, cfg); got != golden {
		t.Errorf("key = %s, want %s", got, golden)
	}
	cfg.Endpoints = []string{}
	if got := requestOverrideCacheKey(nil, cfg); got != golden {
		t.Errorf("key with an empty list = %s, want %s", got, golden)
	}
}

// discoveryKeyConfig returns a config that differs from another only in its
// discovery mode.
func discoveryKeyConfig(mode config.DiscoveryMode) *config.CPIConfig {
	return &config.CPIConfig{
		Host: "pve-a.example", Port: 8006, User: "cpi", Password: "pw-one", APIEndpointDiscovery: mode,
	}
}

// discoveryEffectiveJob returns a valid token-auth job config that verifies
// TLS, for the tests that drive ApplyContextOverrides.
func discoveryEffectiveJob() *config.CPIConfig {
	verify := true
	return &config.CPIConfig{
		Host: "pve-a.example", Port: 8006, User: "u", Realm: "pve", APIToken: "u@pve!cpi=fake-token",
		VMStorage: "s", DiskStorage: "s", NetworkBridge: "br", VerifySSL: &verify,
		AgentMode: "cloudinit", VMDiskFormat: "qcow2", LogLevel: "info",
		VMIDRangeStart: 100, VMIDRangeEnd: 5999, RebootMode: "soft", RebootTimeout: 60,
		NetworkMode: "auto", SDNZoneType: "simple",
	}
}

// TestRequestOverrideCacheKey_DiscoveryModeSeparatesConfigs asserts that two
// configs whose discovery modes differ from the job's in different ways never
// share a cached bundle, and that a mode equal to the job's, however it is
// written, adds nothing to the key.
func TestRequestOverrideCacheKey_DiscoveryModeSeparatesConfigs(t *testing.T) {
	t.Parallel()
	job := discoveryKeyConfig("")
	keys := map[string]string{
		"unset": requestOverrideCacheKey(job, discoveryKeyConfig("")),
		"on":    requestOverrideCacheKey(job, discoveryKeyConfig(config.DiscoveryModeOn)),
		"off":   requestOverrideCacheKey(job, discoveryKeyConfig(config.DiscoveryModeOff)),
	}
	seen := map[string]string{}
	for name, key := range keys {
		if prev, dup := seen[key]; dup {
			t.Errorf("%q and %q share a cache key", prev, name)
		}
		seen[key] = name
	}
	if requestOverrideCacheKey(job, discoveryKeyConfig(config.DiscoveryModeOn)) != keys["on"] {
		t.Error("the cache key must be deterministic")
	}
	if got := requestOverrideCacheKey(job, discoveryKeyConfig(config.DiscoveryModeAuto)); got != keys["unset"] {
		t.Error("an explicit auto under an unset job mode must share the unset bundle")
	}
	// The cache directory is process-level and never part of a bundle's identity.
	withDir := discoveryKeyConfig(config.DiscoveryModeOn)
	withDir.EndpointCacheDir = "/var/vcap/store/pve_cpi/endpoint-cache"
	if requestOverrideCacheKey(job, withDir) != keys["on"] {
		t.Error("the cache directory must not change the bundle key")
	}
}

// TestRequestOverrideCacheKey_DiscoveryModeIsComparedWithTheJobs asserts the
// rule from every side. A mode equal to the job's resolved mode adds nothing,
// whether it is unset, written as auto, or inherited. A mode that differs
// splits the bundle, whichever way the job's mode is written.
func TestRequestOverrideCacheKey_DiscoveryModeIsComparedWithTheJobs(t *testing.T) {
	t.Parallel()
	const (
		auto = config.DiscoveryModeAuto
		on   = config.DiscoveryModeOn
		off  = config.DiscoveryModeOff
	)
	for _, tc := range []struct {
		name     string
		job, cfg config.DiscoveryMode
		splits   bool
	}{
		{"unset beside an unset job", "", "", false},
		{"auto beside an unset job", "", auto, false},
		{"unset beside an auto job", auto, "", false},
		{"auto beside an auto job", auto, auto, false},
		{"on beside an on job", on, on, false},
		{"off beside an auto job", auto, off, true},
		{"on beside an auto job", auto, on, true},
		{"on beside an unset job", "", on, true},
		{"auto beside an on job", on, auto, true},
		{"off beside an on job", on, off, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			job := discoveryKeyConfig(tc.job)
			got := requestOverrideCacheKey(job, discoveryKeyConfig(tc.cfg))
			own := requestOverrideCacheKey(job, discoveryKeyConfig(tc.job))
			if tc.splits && got == own {
				t.Errorf("a mode %q beside a job mode %q must split from the job's own bundle", tc.cfg, tc.job)
			}
			if !tc.splits && got != own {
				t.Errorf("a mode %q beside a job mode %q must share the job's own bundle", tc.cfg, tc.job)
			}
		})
	}

	// Through ApplyContextOverrides, where the booleans and the inheritance
	// happen, an explicit auto under a job-level auto shares the unset bundle
	// and an explicit on under a job-level auto splits from it.
	job := discoveryEffectiveJob()
	job.APIEndpointDiscovery = auto
	keyFor := func(extra map[string]any) string {
		eff, _, _, err := config.ApplyContextOverrides(job, extra)
		if err != nil {
			t.Fatalf("ApplyContextOverrides(%v): %v", extra, err)
		}
		return requestOverrideCacheKey(job, eff)
	}
	unset := keyFor(map[string]any{"pve_vm_storage": "other"})
	if got := keyFor(map[string]any{"pve_vm_storage": "other", "pve_api_endpoint_discovery": "auto"}); got != unset {
		t.Error("an explicit auto under a job-level auto must share the unset bundle")
	}
	if got := keyFor(map[string]any{"pve_vm_storage": "other", "pve_api_endpoint_discovery": "on"}); got == unset {
		t.Error("an explicit on under a job-level auto must split from the unset bundle")
	}
	if got := keyFor(map[string]any{"pve_vm_storage": "other", "pve_api_endpoint_discovery": true}); got == unset {
		t.Error("a boolean true under a job-level auto reads as on and must split from the unset bundle")
	}
}

// TestRequestOverrideCacheKey_DependsOnTheJobConfig asserts that the key is a
// function of the job config as well as of the effective config. The same
// effective config keys differently under a job config whose mode it matches
// and under one whose mode it differs from, and it keys the same under two job
// configs that it differs from alike, since the bundle is then the same.
func TestRequestOverrideCacheKey_DependsOnTheJobConfig(t *testing.T) {
	t.Parallel()
	const (
		auto = config.DiscoveryModeAuto
		on   = config.DiscoveryModeOn
		off  = config.DiscoveryModeOff
	)
	cfg := discoveryKeyConfig(on)
	underAuto := requestOverrideCacheKey(discoveryKeyConfig(auto), cfg)
	underOn := requestOverrideCacheKey(discoveryKeyConfig(on), cfg)
	underOff := requestOverrideCacheKey(discoveryKeyConfig(off), cfg)
	if underAuto == underOn {
		t.Error("the same config keys alike under a job it matches and under an auto job it differs from")
	}
	if underOff == underOn {
		t.Error("the same config keys alike under a job it matches and under an off job it differs from")
	}
	if underAuto != underOff {
		t.Error("the same config keys differently under two job configs that it differs from in the same way")
	}
}

// TestRequestOverrideCacheKey_JobConfigsNeverShareAKeyForTheSameEntry applies
// one entry under two job configs that differ in a single field the key reads
// and requires the two keys to differ, so a key built under one job config is
// never taken for the key of the same entry under another. The discovery mode
// row names a mode that matches one job's and not the other's.
func TestRequestOverrideCacheKey_JobConfigsNeverShareAKeyForTheSameEntry(t *testing.T) {
	t.Parallel()
	ca := selfSignedCAPEMForCacheKeyTest(t)
	verifyOff := false
	for name, tc := range map[string]struct {
		vary  func(*config.CPIConfig)
		entry map[string]any
	}{
		"host":              {func(c *config.CPIConfig) { c.Host = "pve-b.example" }, map[string]any{"pve_vm_storage": "other"}},
		"port":              {func(c *config.CPIConfig) { c.Port = 8007 }, map[string]any{"pve_vm_storage": "other"}},
		"realm":             {func(c *config.CPIConfig) { c.Realm = "pam" }, map[string]any{"pve_vm_storage": "other"}},
		"token":             {func(c *config.CPIConfig) { c.APIToken = "u@pve!cpi=another-token" }, map[string]any{"pve_vm_storage": "other"}},
		"node":              {func(c *config.CPIConfig) { c.Node = "pve2" }, map[string]any{"pve_vm_storage": "other"}},
		"certificate check": {func(c *config.CPIConfig) { c.VerifySSL = &verifyOff }, map[string]any{"pve_vm_storage": "other"}},
		"CA":                {func(c *config.CPIConfig) { c.PVECACertPEM = ca }, map[string]any{"pve_vm_storage": "other"}},
		"discovery mode named by the entry": {
			func(c *config.CPIConfig) { c.APIEndpointDiscovery = config.DiscoveryModeOn },
			map[string]any{"pve_api_endpoint_discovery": "on"},
		},
		"discovery mode restated as auto": {
			func(c *config.CPIConfig) { c.APIEndpointDiscovery = config.DiscoveryModeOff },
			map[string]any{"pve_api_endpoint_discovery": "auto"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			jobA := discoveryEffectiveJob()
			jobB := discoveryEffectiveJob()
			tc.vary(jobB)
			keyUnder := func(job *config.CPIConfig) string {
				eff, _, _, err := config.ApplyContextOverrides(job, tc.entry)
				if err != nil {
					t.Fatalf("ApplyContextOverrides(%v): %v", tc.entry, err)
				}
				return requestOverrideCacheKey(job, eff)
			}
			first, again := keyUnder(jobA), keyUnder(jobA)
			if first != again {
				t.Error("the key must be deterministic under one job config")
			}
			if first == keyUnder(jobB) {
				t.Errorf("the same entry keys alike under job configs that differ in the %s", name)
			}
		})
	}
}

// TestRequestOverrideCacheKey_NoModeKeysAsBeforeTheModeExisted pins the key of
// a fixed config with no discovery mode to the value the key function produced
// before the mode was folded in, so no warm cached client is dropped on
// upgrade. It is the same fixed config and golden value as the endpoint list
// test, because an unset mode and an empty list must both add nothing. A mode
// that matches the job's adds nothing either.
func TestRequestOverrideCacheKey_NoModeKeysAsBeforeTheModeExisted(t *testing.T) {
	t.Parallel()
	const golden = "e1b319f637957cfa0d54052d63706dd2951c0350482db1975f46542f0d2d84d3"
	cfg := &config.CPIConfig{
		Host: "pve-a.example", Port: 8006, User: "cpi", Realm: "pam", Node: "pve1", Password: "pw-one",
		VMStorage: "local-lvm", DiskStorage: "local-lvm", NetworkBridge: "vmbr0",
	}
	if got := requestOverrideCacheKey(nil, cfg); got != golden {
		t.Errorf("key = %s, want %s", got, golden)
	}
	cfg.APIEndpointDiscovery = config.DiscoveryModeAuto
	if got := requestOverrideCacheKey(nil, cfg); got != golden {
		t.Errorf("key with an explicit auto = %s, want %s", got, golden)
	}
	cfg.APIEndpointDiscovery = config.DiscoveryModeOn
	if got := requestOverrideCacheKey(nil, cfg); got == golden {
		t.Error("an explicit on beside an unset job mode must key differently")
	}
	job := *cfg
	if got := requestOverrideCacheKey(&job, cfg); got != golden {
		t.Errorf("an on that matches the job's on keyed %s, want %s", got, golden)
	}
	cfgAuto := *cfg
	cfgAuto.APIEndpointDiscovery = config.DiscoveryModeAuto
	jobUnset := cfgAuto
	jobUnset.APIEndpointDiscovery = ""
	if requestOverrideCacheKey(&jobUnset, &cfgAuto) != requestOverrideCacheKey(&jobUnset, &jobUnset) {
		t.Error("an explicit auto must key like an unset mode beside an unset job")
	}
}

// TestRequestOverrideCacheKey_LooserTLSFlagSeparatesEntries asserts that two
// entries which name the same TLS keys, and differ only in whether they
// inherited the job's credentials, never share a bundle. Entry A inherits the
// token and so resolves its discovery to off. Entry B restates the token
// itself and keeps the mode it would otherwise have. Whichever is built
// first, the other must not reuse its client, and A must always resolve to off.
func TestRequestOverrideCacheKey_LooserTLSFlagSeparatesEntries(t *testing.T) {
	t.Parallel()
	job := discoveryEffectiveJob()
	build := func(extra map[string]any) *config.CPIConfig {
		eff, _, _, err := config.ApplyContextOverrides(job, extra)
		if err != nil {
			t.Fatalf("ApplyContextOverrides(%v): %v", extra, err)
		}
		return eff
	}
	entryA := func() map[string]any { return map[string]any{"pve_verify_ssl": false} }
	entryB := func() map[string]any {
		return map[string]any{"pve_verify_ssl": false, "pve_api_token": job.APIToken, "pve_user": job.User}
	}
	for name, aFirst := range map[string]bool{"A then B": true, "B then A": false} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var effA, effB *config.CPIConfig
			if aFirst {
				effA = build(entryA())
				effB = build(entryB())
			} else {
				effB = build(entryB())
				effA = build(entryA())
			}
			if !effA.InheritedCredsLooserTLS() || effB.InheritedCredsLooserTLS() {
				t.Fatalf("flags = %v and %v, want the inheriting entry flagged and the other not",
					effA.InheritedCredsLooserTLS(), effB.InheritedCredsLooserTLS())
			}
			if requestOverrideCacheKey(job, effA) == requestOverrideCacheKey(job, effB) {
				t.Error("entries that differ only in the looser TLS flag must not share a bundle")
			}
			if mode, _ := config.EffectiveDiscoveryMode(effA); mode != config.DiscoveryModeOff {
				t.Errorf("the inheriting entry resolves to %q, want off", mode)
			}
			if mode, _ := config.EffectiveDiscoveryMode(effB); mode != config.DiscoveryModeOn {
				t.Errorf("the entry with its own token resolves to %q, want on", mode)
			}
		})
	}
}
