package handlers

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
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

// TestRequestOverrideCacheKey_EndpointListSeparatesConfigs asserts that two
// configs that differ only in their endpoint list never share a cached
// client, that the order of the list counts, that no list keys exactly as a
// config did before the list existed, and that elements cannot run together.
func TestRequestOverrideCacheKey_EndpointListSeparatesConfigs(t *testing.T) {
	t.Parallel()
	mk := func(list ...string) *config.CPIConfig {
		return &config.CPIConfig{Host: "pve-a.example", Port: 8006, User: "cpi", Password: "pw-one", Endpoints: list}
	}
	none := requestOverrideCacheKey(mk())
	if empty := requestOverrideCacheKey(&config.CPIConfig{
		Host: "pve-a.example", Port: 8006, User: "cpi", Password: "pw-one", Endpoints: []string{},
	}); empty != none {
		t.Error("an empty list must key like no list")
	}
	keys := map[string]string{
		"no list":        none,
		"one":            requestOverrideCacheKey(mk("pve-a.example:8006")),
		"two":            requestOverrideCacheKey(mk("pve-a.example:8006", "pve-b.example:8006")),
		"two, other":     requestOverrideCacheKey(mk("pve-a.example:8006", "pve-c.example:8006")),
		"two, reordered": requestOverrideCacheKey(mk("pve-b.example:8006", "pve-a.example:8006")),
		"joined":         requestOverrideCacheKey(mk("pve-a.example:8006,pve-b.example:8006")),
		"three": requestOverrideCacheKey(mk("pve-a.example:8006", "pve-b.example:8006",
			"pve-c.example:8006")),
	}
	seen := map[string]string{}
	for name, key := range keys {
		if prev, dup := seen[key]; dup {
			t.Errorf("%q and %q share a cache key", prev, name)
		}
		seen[key] = name
	}
	if requestOverrideCacheKey(mk("pve-a.example:8006", "pve-b.example:8006")) != keys["two"] {
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
	if got := requestOverrideCacheKey(cfg); got != golden {
		t.Errorf("key = %s, want %s", got, golden)
	}
	cfg.Endpoints = []string{}
	if got := requestOverrideCacheKey(cfg); got != golden {
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

// TestRequestOverrideCacheKey_DiscoveryModeSeparatesConfigs asserts that
// configs with different resolved discovery modes never share a cached
// bundle, and that an unset mode and an explicit auto, which resolve alike,
// share one.
func TestRequestOverrideCacheKey_DiscoveryModeSeparatesConfigs(t *testing.T) {
	t.Parallel()
	keys := map[string]string{
		"unset": requestOverrideCacheKey(discoveryKeyConfig("")),
		"auto":  requestOverrideCacheKey(discoveryKeyConfig(config.DiscoveryModeAuto)),
		"on":    requestOverrideCacheKey(discoveryKeyConfig(config.DiscoveryModeOn)),
		"off":   requestOverrideCacheKey(discoveryKeyConfig(config.DiscoveryModeOff)),
	}
	if keys["unset"] != keys["auto"] {
		t.Error("an explicit auto must share the unset bundle")
	}
	seen := map[string]string{}
	for _, name := range []string{"auto", "on", "off"} {
		if prev, dup := seen[keys[name]]; dup {
			t.Errorf("%q and %q share a cache key", prev, name)
		}
		seen[keys[name]] = name
	}
	if requestOverrideCacheKey(discoveryKeyConfig(config.DiscoveryModeOn)) != keys["on"] {
		t.Error("the cache key must be deterministic")
	}
	// The cache directory is process-level and never part of a bundle's identity.
	withDir := discoveryKeyConfig(config.DiscoveryModeOn)
	withDir.EndpointCacheDir = "/var/vcap/store/pve_cpi/endpoint-cache"
	if requestOverrideCacheKey(withDir) != keys["on"] {
		t.Error("the cache directory must not change the bundle key")
	}
}

// TestRequestOverrideCacheKey_DiscoveryModeMatrix drives the mode matrix
// through ApplyContextOverrides under every job mode. An entry that names a
// mode shares the key of an entry that names none exactly when the two resolve
// to the same mode, whichever way the job's mode is written, and the booleans
// read as on and off.
func TestRequestOverrideCacheKey_DiscoveryModeMatrix(t *testing.T) {
	t.Parallel()
	const (
		auto = config.DiscoveryModeAuto
		on   = config.DiscoveryModeOn
		off  = config.DiscoveryModeOff
	)
	resolved := func(m config.DiscoveryMode) config.DiscoveryMode {
		if m == "" {
			return auto
		}
		return m
	}
	for _, jobMode := range []config.DiscoveryMode{"", auto, on, off} {
		for _, entryMode := range []any{"auto", "on", "off", true, false} {
			t.Run(string(jobMode)+" job, entry "+fmt.Sprint(entryMode), func(t *testing.T) {
				t.Parallel()
				job := discoveryEffectiveJob()
				job.APIEndpointDiscovery = jobMode
				keyFor := func(extra map[string]any) (string, *config.CPIConfig) {
					eff, _, _, err := config.ApplyContextOverrides(job, extra)
					if err != nil {
						t.Fatalf("ApplyContextOverrides(%v): %v", extra, err)
					}
					return requestOverrideCacheKey(eff), eff
				}
				unnamed, _ := keyFor(map[string]any{"pve_vm_storage": "other"})
				named, eff := keyFor(map[string]any{"pve_vm_storage": "other", "pve_api_endpoint_discovery": entryMode})
				want := map[any]config.DiscoveryMode{
					"auto": auto,
					"on":   on,
					"off":  off,
					true:   on,
					false:  off,
				}[entryMode]
				if got := eff.DiscoveryModeValue(); got != want {
					t.Fatalf("entry mode %v resolved to %q, want %q", entryMode, got, want)
				}
				if same := named == unnamed; same != (want == resolved(jobMode)) {
					t.Errorf("entry mode %v under job mode %q shares the unnamed key = %v, want %v",
						entryMode, jobMode, same, want == resolved(jobMode))
				}
			})
		}
	}
}

// TestRequestOverrideCacheKey_BooleanModesKeyAsTheirNames asserts that a YAML
// boolean for the discovery mode keys exactly as the word it reads as, so true
// shares the bundle of on and false shares the bundle of off, under every job
// mode.
func TestRequestOverrideCacheKey_BooleanModesKeyAsTheirNames(t *testing.T) {
	t.Parallel()
	jobModes := []config.DiscoveryMode{"", config.DiscoveryModeAuto, config.DiscoveryModeOn, config.DiscoveryModeOff}
	for _, jobMode := range jobModes {
		t.Run("job "+string(jobMode), func(t *testing.T) {
			t.Parallel()
			job := discoveryEffectiveJob()
			job.APIEndpointDiscovery = jobMode
			keyFor := func(mode any) string {
				eff, _, _, err := config.ApplyContextOverrides(job,
					map[string]any{"pve_vm_storage": "other", "pve_api_endpoint_discovery": mode})
				if err != nil {
					t.Fatalf("ApplyContextOverrides(%v): %v", mode, err)
				}
				return requestOverrideCacheKey(eff)
			}
			if keyFor(true) != keyFor("on") {
				t.Error("a boolean true must key like on")
			}
			if keyFor(false) != keyFor("off") {
				t.Error("a boolean false must key like off")
			}
			if keyFor("on") == keyFor("off") {
				t.Error("on and off must key differently")
			}
		})
	}
}

// TestRequestOverrideCacheKey_JobConfigsNeverShareAKeyForTheSameEntry applies
// one entry under two job configs that differ in a single field the key reads
// and requires the two keys to differ, so a key built under one job config is
// never taken for the key of the same entry under another. The job's
// discovery mode is among the fields, and the entry names no mode, so it
// inherits the job's. An entry that names a mode gets the same effective
// config under any job config, and so the same key, which the last cases
// assert.
func TestRequestOverrideCacheKey_JobConfigsNeverShareAKeyForTheSameEntry(t *testing.T) {
	t.Parallel()
	ca := selfSignedCAPEMForCacheKeyTest(t)
	verifyOff := false
	inherit := map[string]any{"pve_vm_storage": "other"}
	withMode := func(mode config.DiscoveryMode) func(*config.CPIConfig) {
		return func(c *config.CPIConfig) { c.APIEndpointDiscovery = mode }
	}
	for name, tc := range map[string]struct {
		vary func(*config.CPIConfig)
	}{
		"host":              {func(c *config.CPIConfig) { c.Host = "pve-b.example" }},
		"port":              {func(c *config.CPIConfig) { c.Port = 8007 }},
		"realm":             {func(c *config.CPIConfig) { c.Realm = "pam" }},
		"token":             {func(c *config.CPIConfig) { c.APIToken = "u@pve!cpi=another-token" }},
		"node":              {func(c *config.CPIConfig) { c.Node = "pve2" }},
		"certificate check": {func(c *config.CPIConfig) { c.VerifySSL = &verifyOff }},
		"CA":                {func(c *config.CPIConfig) { c.PVECACertPEM = ca }},
		"mode on":           {withMode(config.DiscoveryModeOn)},
		"mode off":          {withMode(config.DiscoveryModeOff)},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			jobA := discoveryEffectiveJob()
			jobB := discoveryEffectiveJob()
			tc.vary(jobB)
			keyUnder := func(job *config.CPIConfig, entry map[string]any) string {
				eff, _, _, err := config.ApplyContextOverrides(job, entry)
				if err != nil {
					t.Fatalf("ApplyContextOverrides(%v): %v", entry, err)
				}
				return requestOverrideCacheKey(eff)
			}
			first, again := keyUnder(jobA, inherit), keyUnder(jobA, inherit)
			if first != again {
				t.Error("the key must be deterministic under one job config")
			}
			if first == keyUnder(jobB, inherit) {
				t.Errorf("the same entry keys alike under job configs that differ in the %s", name)
			}
		})
	}

	// The job modes against each other, in every pairing, for an entry that
	// names no mode.
	modes := []config.DiscoveryMode{"", config.DiscoveryModeOn, config.DiscoveryModeOff}
	keyFor := func(mode config.DiscoveryMode, entry map[string]any) string {
		job := discoveryEffectiveJob()
		job.APIEndpointDiscovery = mode
		eff, _, _, err := config.ApplyContextOverrides(job, entry)
		if err != nil {
			t.Fatalf("ApplyContextOverrides(%v): %v", entry, err)
		}
		return requestOverrideCacheKey(eff)
	}
	for i, a := range modes {
		for _, b := range modes[i+1:] {
			if keyFor(a, inherit) == keyFor(b, inherit) {
				t.Errorf("job modes %q and %q share a key for the same entry", a, b)
			}
		}
	}

	// An entry that names the mode gets the same effective config under either
	// job, so it keys alike.
	named := map[string]any{"pve_vm_storage": "other", "pve_api_endpoint_discovery": "on"}
	for _, a := range modes {
		for _, b := range modes {
			if keyFor(a, named) != keyFor(b, named) {
				t.Errorf("an entry that names on keys differently under job modes %q and %q", a, b)
			}
		}
	}
}

// TestRequestOverrideCacheKey_NoModeKeysAsBeforeTheModeExisted pins the key of
// a fixed config with no discovery mode to the value the key function produced
// before the mode was folded in, so no warm cached client is dropped on
// upgrade. It is the same fixed config and golden value as the endpoint list
// test, because an unset mode, an explicit auto, and an empty list must all
// add nothing.
func TestRequestOverrideCacheKey_NoModeKeysAsBeforeTheModeExisted(t *testing.T) {
	t.Parallel()
	const golden = "e1b319f637957cfa0d54052d63706dd2951c0350482db1975f46542f0d2d84d3"
	cfg := &config.CPIConfig{
		Host: "pve-a.example", Port: 8006, User: "cpi", Realm: "pam", Node: "pve1", Password: "pw-one",
		VMStorage: "local-lvm", DiskStorage: "local-lvm", NetworkBridge: "vmbr0",
	}
	if got := requestOverrideCacheKey(cfg); got != golden {
		t.Errorf("key = %s, want %s", got, golden)
	}
	cfg.APIEndpointDiscovery = config.DiscoveryModeAuto
	if got := requestOverrideCacheKey(cfg); got != golden {
		t.Errorf("key with an explicit auto = %s, want %s", got, golden)
	}
	for _, mode := range []config.DiscoveryMode{config.DiscoveryModeOn, config.DiscoveryModeOff} {
		cfg.APIEndpointDiscovery = mode
		if got := requestOverrideCacheKey(cfg); got == golden {
			t.Errorf("an explicit %s keyed as the config with no mode", mode)
		}
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
			if requestOverrideCacheKey(effA) == requestOverrideCacheKey(effB) {
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

// TestRequestOverrideCacheKey_PlacementOwnershipSeparatesEntries asserts that
// two entries which differ only in whether they name pve_placement never share
// a bundle. Entry A restates the job's map under its own host, CA, and token,
// so its cold-start candidates are the map's nodes. Entry B names the same
// host, CA, and token and inherits the map, so it gets none. Whichever builds
// first, the other must not reuse its client.
func TestRequestOverrideCacheKey_PlacementOwnershipSeparatesEntries(t *testing.T) {
	t.Parallel()
	ca := selfSignedCAPEMForCacheKeyTest(t)
	job := discoveryEffectiveJob()
	job.PVECACertPEM = ca
	job.Placement = &config.PlacementConfig{AZMap: map[string][]string{"az1": {"pve1", "pve2"}}}
	build := func(withMap bool) *config.CPIConfig {
		extra := map[string]any{"pve_host": "pve-b.example", "pve_ca_cert": ca, "pve_api_token": "u@pve!cpi=own"}
		if withMap {
			extra["pve_placement"] = map[string]any{"az_map": map[string]any{"az1": []any{"pve1", "pve2"}}}
		}
		eff, _, _, err := config.ApplyContextOverrides(job, extra)
		if err != nil {
			t.Fatalf("ApplyContextOverrides(%v): %v", extra, err)
		}
		return eff
	}
	for name, aFirst := range map[string]bool{"A then B": true, "B then A": false} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var effA, effB *config.CPIConfig
			if aFirst {
				effA = build(true)
				effB = build(false)
			} else {
				effB = build(false)
				effA = build(true)
			}
			if !effA.PlacementNamedByEntry() || effB.PlacementNamedByEntry() {
				t.Fatalf("flags = %v and %v, want the entry naming its placement flagged and the other not",
					effA.PlacementNamedByEntry(), effB.PlacementNamedByEntry())
			}
			if got := config.ColdStartNodeNames(effA, job); !slices.Equal(got, []string{"pve1", "pve2"}) {
				t.Errorf("entry A cold-start names = %v, want [pve1 pve2]", got)
			}
			if got := config.ColdStartNodeNames(effB, job); got != nil {
				t.Errorf("entry B cold-start names = %v, want none", got)
			}
			if requestOverrideCacheKey(effA) == requestOverrideCacheKey(effB) {
				t.Error("entries that differ only in placement ownership must not share a bundle")
			}
		})
	}
}

// TestRequestOverrideCacheKey_PlacementOwnershipKeysAsBeforeWhenItDecidesNothing
// asserts that the ownership flag adds nothing to the key unless the entry
// names pve_placement and every other cold-start condition holds, so no warm
// bundle is dropped for a config the flag cannot affect.
func TestRequestOverrideCacheKey_PlacementOwnershipKeysAsBeforeWhenItDecidesNothing(t *testing.T) {
	t.Parallel()
	ca := selfSignedCAPEMForCacheKeyTest(t)
	job := discoveryEffectiveJob()
	job.PVECACertPEM = ca
	placement := map[string]any{"az_map": map[string]any{"az1": []any{"pve1"}}}
	job.Placement = &config.PlacementConfig{AZMap: map[string][]string{"az1": {"pve1"}}}
	entry := func(extra map[string]any) *config.CPIConfig {
		eff, _, _, err := config.ApplyContextOverrides(job, extra)
		if err != nil {
			t.Fatalf("ApplyContextOverrides(%v): %v", extra, err)
		}
		return eff
	}
	// The same effective config with the flag down is the key a config had
	// before the flag existed, because only an entry's handler raises it.
	for name, extra := range map[string]map[string]any{
		"discovery off": {
			"pve_host": "pve-b.example", "pve_ca_cert": ca, "pve_api_token": "u@pve!cpi=own",
			"pve_api_endpoint_discovery": "off", "pve_placement": placement,
		},
		"password auth": {
			"pve_host": "pve-b.example", "pve_ca_cert": ca, "pve_user": "root", "pve_password": "pw",
			"pve_placement": placement,
		},
		"two endpoints under auto": {
			"pve_host": "pve-b.example", "pve_ca_cert": ca, "pve_api_token": "u@pve!cpi=own",
			"pve_endpoints": []any{"pve-b.example", "pve-c.example"}, "pve_placement": placement,
		},
		"no ca": {
			"pve_host": "pve-b.example", "pve_api_token": "u@pve!cpi=own", "pve_placement": placement,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			eff := entry(extra)
			if !eff.PlacementNamedByEntry() {
				t.Fatal("the entry names pve_placement, so the flag must be up")
			}
			if got := config.ColdStartNodeNames(eff, job); got != nil {
				t.Fatalf("cold-start names = %v, want none, or the case proves nothing", got)
			}
			without := make(map[string]any, len(extra))
			for k, v := range extra {
				if k != "pve_placement" {
					without[k] = v
				}
			}
			// With the map left out the entry inherits an identical one, so
			// every field the key reads is equal and only the flag differs.
			if requestOverrideCacheKey(eff) != requestOverrideCacheKey(entry(without)) {
				t.Error("the flag changed the key although the cold start cannot apply")
			}
		})
	}
	// Neither entry has a CA, so the hash is deterministic. The pinned keys
	// are what a build before the ownership flag produced for the same
	// configs, so a key line written for either entry fails here.
	for name, tc := range map[string]struct {
		extra map[string]any
		named bool
		key   string
	}{
		"an entry that names nothing": {
			extra: map[string]any{"pve_vm_storage": "other"},
			key:   "1c85a5a21eb7d78b8335699731a4dba3d3baa3c7d822d35c90ab71da1711b5b2",
		},
		"a named entry the cold start cannot use": {
			extra: map[string]any{
				"pve_host": "pve-b.example", "pve_api_token": "u@pve!cpi=own", "pve_placement": placement,
			},
			named: true,
			key:   "3478059b2c7311e485584361be8c78039cdbacd1cf5c2113bc305c86610ca863",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			noCAJob := discoveryEffectiveJob()
			noCAJob.Placement = &config.PlacementConfig{AZMap: map[string][]string{"az1": {"pve1"}}}
			eff, _, _, err := config.ApplyContextOverrides(noCAJob, tc.extra)
			if err != nil {
				t.Fatalf("ApplyContextOverrides(%v): %v", tc.extra, err)
			}
			if eff.PlacementNamedByEntry() != tc.named {
				t.Fatalf("PlacementNamedByEntry = %v, want %v", eff.PlacementNamedByEntry(), tc.named)
			}
			if got := config.ColdStartNodeNames(eff, noCAJob); got != nil {
				t.Fatalf("cold-start names = %v, want none, or the case proves nothing", got)
			}
			if got := requestOverrideCacheKey(eff); got != tc.key {
				t.Errorf("key = %s, want %s", got, tc.key)
			}
		})
	}
}
