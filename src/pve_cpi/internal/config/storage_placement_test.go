package config_test

import (
	"encoding/json"
	"math"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
)

const placementSetJSON = `{"names":["nfs-e"],"strategy":{"name":"spread","version":1}}`

func decodeStoragePlacement(t *testing.T, raw string) *config.CPIConfig {
	t.Helper()
	c := validBaseCfg()
	if err := json.Unmarshal([]byte(raw), c); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestStoragePlacementStrictDecode(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"null map":                   `{"storage_sets":null}`,
		"null set":                   `{"storage_sets":{"e":null}}`,
		"wrong map":                  `{"storage_sets":[]}`,
		"wrong set":                  `{"storage_sets":{"e":false}}`,
		"missing strategy":           `{"storage_sets":{"e":{"names":["e"]}}}`,
		"null strategy":              `{"storage_sets":{"e":{"names":["e"],"strategy":null}}}`,
		"empty names":                `{"storage_sets":{"e":{"names":[],"strategy":{"name":"spread","version":1}}}}`,
		"null names":                 `{"storage_sets":{"e":{"names":null,"strategy":{"name":"spread","version":1}}}}`,
		"both selectors":             `{"storage_sets":{"e":{"names":["e"],"name_pattern":"^e$","strategy":{"name":"spread","version":1}}}}`,
		"blank regex":                `{"storage_sets":{"e":{"name_pattern":" ","strategy":{"name":"spread","version":1}}}}`,
		"unknown set key":            `{"storage_sets":{"e":{"names":["e"],"strategy":{"name":"spread","version":1},"priority":1}}}`,
		"noncanonical set key":       `{"storage_sets":{"e":{"Names":["e"],"strategy":{"name":"spread","version":1}}}}`,
		"unknown algorithm argument": `{"storage_sets":{"e":{"names":["e"],"strategy":{"name":"spread","version":1,"weight":2}}}}`,
		"missing version":            `{"storage_sets":{"e":{"names":["e"],"strategy":{"name":"spread"}}}}`,
		"fractional version":         `{"storage_sets":{"e":{"names":["e"],"strategy":{"name":"spread","version":1.5}}}}`,
		"quoted version":             `{"storage_sets":{"e":{"names":["e"],"strategy":{"name":"spread","version":"1"}}}}`,
		"null reserve":               `{"storage_sets":{"e":{"names":["e"],"strategy":{"name":"spread","version":1},"min_free_mb":null}}}`,
		"fractional reserve":         `{"storage_sets":{"e":{"names":["e"],"strategy":{"name":"spread","version":1},"min_free_mb":0.5}}}`,
		"null ceiling":               `{"storage_sets":{"e":{"names":["e"],"strategy":{"name":"spread","version":1},"max_utilization_pct":null}}}`,
		"null anti_affinity":         `{"storage_sets":{"e":{"names":["e"],"strategy":{"name":"spread","version":1},"anti_affinity":null}}}`,
		"unknown anti_affinity key":  `{"storage_sets":{"e":{"names":["e"],"strategy":{"name":"spread","version":1},"anti_affinity":{"scope":"instance_group","priority":1}}}}`,
		"null anti_affinity band":    `{"storage_sets":{"e":{"names":["e"],"strategy":{"name":"spread","version":1},"anti_affinity":{"utilization_band_pct":null}}}}`,
		"wrong bool":                 `{"require_disjoint_storage_sets":"false"}`,
		"null bool":                  `{"require_disjoint_storage_sets":null}`,
		"null age":                   `{"storage_status_max_age_seconds":null}`,
		"fractional age":             `{"storage_status_max_age_seconds":1.5}`,
		"quoted age":                 `{"storage_status_max_age_seconds":"5"}`,
		"null role":                  `{"ephemeral_storage_set":null}`,
		"empty role":                 `{"ephemeral_storage_set":""}`,
		"blank namespace":            `{"storage_placement_namespace":" "}`,
		"null directory":             `{"storage_allocation_journal_dir":null}`,
		"unknown policy":             `{"storage_sets_typo":{}}`,
		"null case alias":            `{"Storage_Sets":null}`,
		"null domains":               `{"storage_capacity_domains":null}`,
		"null domain":                `{"storage_capacity_domains":{"nas":null}}`,
		"empty domain":               `{"storage_capacity_domains":{"nas":{"members":[]}}}`,
		"unknown domain field":       `{"storage_capacity_domains":{"nas":{"members":["e"],"quota":2}}}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			var c config.CPIConfig
			if err := json.Unmarshal([]byte(raw), &c); err == nil {
				t.Fatalf("accepted malformed policy %s", raw)
			}
		})
	}
}

func TestStoragePlacementStaticValidation(t *testing.T) {
	t.Parallel()
	cases := map[string]func(*config.CPIConfig){
		"duplicate IDs": func(c *config.CPIConfig) {
			s := c.StorageSets["e"]
			s.Names = []string{"e", "e"}
			c.StorageSets["e"] = s
		},
		"blank ID": func(c *config.CPIConfig) { s := c.StorageSets["e"]; s.Names = []string{" "}; c.StorageSets["e"] = s },
		"invalid regex": func(c *config.CPIConfig) {
			s := c.StorageSets["e"]
			s.Names = nil
			s.NamePattern = "["
			c.StorageSets["e"] = s
		},
		"unsupported type": func(c *config.CPIConfig) { s := c.StorageSets["e"]; s.Types = []string{"rbd"}; c.StorageSets["e"] = s },
		"unshared":         func(c *config.CPIConfig) { s := c.StorageSets["e"]; s.Shared = boolPtr(false); c.StorageSets["e"] = s },
		"negative reserve": func(c *config.CPIConfig) { s := c.StorageSets["e"]; s.MinFreeMB = -1; c.StorageSets["e"] = s },
		"overflow reserve": func(c *config.CPIConfig) {
			s := c.StorageSets["e"]
			s.MinFreeMB = math.MaxInt64
			c.StorageSets["e"] = s
		},
		"zero ceiling": func(c *config.CPIConfig) {
			s := c.StorageSets["e"]
			zero := 0
			s.MaxUtilizationPct = &zero
			c.StorageSets["e"] = s
		},
		"large ceiling": func(c *config.CPIConfig) {
			s := c.StorageSets["e"]
			n := 101
			s.MaxUtilizationPct = &n
			c.StorageSets["e"] = s
		},
		"unknown anti_affinity scope": func(c *config.CPIConfig) {
			s := c.StorageSets["e"]
			s.AntiAffinity = &config.StorageAntiAffinity{Scope: "rack"}
			c.StorageSets["e"] = s
		},
		"blank anti_affinity scope": func(c *config.CPIConfig) {
			s := c.StorageSets["e"]
			s.AntiAffinity = &config.StorageAntiAffinity{Scope: ""}
			c.StorageSets["e"] = s
		},
		"negative anti_affinity band": func(c *config.CPIConfig) {
			s := c.StorageSets["e"]
			n := -1
			s.AntiAffinity = &config.StorageAntiAffinity{Scope: config.StorageAntiAffinityScopeInstanceGroup, UtilizationBandPct: &n}
			c.StorageSets["e"] = s
		},
		"large anti_affinity band": func(c *config.CPIConfig) {
			s := c.StorageSets["e"]
			n := 101
			s.AntiAffinity = &config.StorageAntiAffinity{Scope: config.StorageAntiAffinityScopeInstanceGroup, UtilizationBandPct: &n}
			c.StorageSets["e"] = s
		},
		"unknown strategy": func(c *config.CPIConfig) {
			s := c.StorageSets["e"]
			s.Strategy.Name = "round_robin"
			c.StorageSets["e"] = s
		},
		"unknown version": func(c *config.CPIConfig) { s := c.StorageSets["e"]; s.Strategy.Version = 2; c.StorageSets["e"] = s },
		"missing binding": func(c *config.CPIConfig) { c.PersistentStorageSet = "missing" },
		"contradictory assertions": func(c *config.CPIConfig) {
			s := c.StorageSets["e"]
			s.Encrypted = boolPtr(true)
			c.StorageSets["e"] = s
			s.Encrypted = boolPtr(false)
			c.StorageSets["alias"] = s
		},
		"blank set label":  func(c *config.CPIConfig) { c.StorageSets[" "] = c.StorageSets["e"] },
		"zero age":         func(c *config.CPIConfig) { n := 0; c.StorageStatusMaxAgeSeconds = &n },
		"large age":        func(c *config.CPIConfig) { n := 61; c.StorageStatusMaxAgeSeconds = &n },
		"relative journal": func(c *config.CPIConfig) { c.StorageAllocationJournalDir = "relative/path" },
		"domain duplicate": func(c *config.CPIConfig) {
			c.StorageCapacityDomains = map[string]config.StorageCapacityDomain{"a": {Members: []string{"e"}}, "b": {Members: []string{"e"}}}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c := decodeStoragePlacement(t, `{"storage_sets":{"e":`+placementSetJSON+`}}`)
			mutate(c)
			if err := c.ValidateStoragePlacement(); err == nil {
				t.Fatal("accepted invalid policy")
			}
		})
	}
}

func TestStoragePlacementDefaultsAndActivationPrerequisites(t *testing.T) {
	t.Parallel()
	c := decodeStoragePlacement(t, `{"storage_sets":{"e":`+placementSetJSON+`}}`)
	if err := c.Validate(); err != nil {
		t.Fatalf("unused set affected legacy config: %v", err)
	}
	if c.StorageStatusMaxAgeSecondsValue() != 5 || c.RequireDisjointStorageSetsEnabled() || c.HasGlobalStorageSetBindings() {
		t.Fatal("wrong omitted defaults")
	}
	if err := c.ValidateStoragePlacementAllocation(); err == nil {
		t.Fatal("allocation accepted without namespace")
	}
	c.StoragePlacementNamespace = "director"
	if err := c.ValidateStoragePlacementAllocation(); err == nil {
		t.Fatal("allocation accepted without durable directory")
	}
	c.StorageAllocationJournalDir = "/var/vcap/store/pve_cpi/allocations"
	if err := c.ValidateStoragePlacementAllocation(); err != nil {
		t.Fatal(err)
	}
	c.PersistentStorageSet = "e"
	c.EphemeralStorageSet = "e"
	if !c.RequireDisjointStorageSetsEnabled() {
		t.Fatal("both bindings should default disjoint")
	}
	c.RequireDisjointStorageSets = boolPtr(false)
	if c.RequireDisjointStorageSetsEnabled() {
		t.Fatal("explicit false lost")
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("live overlap must not block static lifecycle validation: %v", err)
	}
}

func TestStoragePlacementDecodePreservesOmittedLegacyFields(t *testing.T) {
	t.Parallel()
	c := validBaseCfg()
	original := *c
	if err := json.Unmarshal([]byte(`{"storage_status_max_age_seconds":6,"future_legacy_field":true}`), c); err != nil {
		t.Fatal(err)
	}
	if c.Host != original.Host || c.Password != original.Password || c.VMIDRangeEnd != original.VMIDRangeEnd || c.VerifySSL != original.VerifySSL {
		t.Fatal("partial JSON reset omitted legacy fields")
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestStoragePlacementContextIsolationAndReplacement(t *testing.T) {
	t.Parallel()
	base := decodeStoragePlacement(t, `{"storage_sets":{"e":`+placementSetJSON+`},"storage_capacity_domains":{"old":{"members":["nfs-e"]}},"ephemeral_storage_set":"e","storage_placement_namespace":"old","require_disjoint_storage_sets":false}`)
	var wg sync.WaitGroup
	for _, name := range []string{"cluster-a", "cluster-b"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			extra := map[string]any{"pve": map[string]any{
				"host": name, "storage_sets": map[string]any{"e": map[string]any{"names": []string{name}, "strategy": map[string]any{"name": "spread", "version": 1}}},
				"storage_capacity_domains": map[string]any{}, "storage_placement_namespace": name,
			}}
			eff, _, _, err := config.ApplyContextOverrides(base, extra)
			if err != nil {
				t.Error(err)
				return
			}
			if len(eff.StorageCapacityDomains) != 0 || eff.StorageSets["e"].Names[0] != name || eff.StoragePlacementNamespace != name {
				t.Error("whole-map replacement or cluster isolation failed")
			}
			eff.StorageSets["e"].Names[0] = "mutated"
			*eff.RequireDisjointStorageSets = true
		}()
	}
	wg.Wait()
	if base.StorageSets["e"].Names[0] != "nfs-e" || *base.RequireDisjointStorageSets || len(base.StorageCapacityDomains) != 1 {
		t.Fatal("context mutation leaked into base")
	}
	// Even a legacy override must not share mutable inherited storage policy.
	eff, _, _, err := config.ApplyContextOverrides(base, map[string]any{"pve_node": "node2"})
	if err != nil {
		t.Fatal(err)
	}
	eff.StorageSets["e"].Names[0] = "changed"
	eff.StorageCapacityDomains["old"].Members[0] = "changed"
	if base.StorageSets["e"].Names[0] != "nfs-e" || base.StorageCapacityDomains["old"].Members[0] != "nfs-e" {
		t.Fatal("inherited policy was not deep-copied")
	}
}

func TestStoragePlacementContextRejectsUnsafeOverrides(t *testing.T) {
	t.Parallel()
	base := decodeStoragePlacement(t, `{"storage_sets":{"e":`+placementSetJSON+`},"ephemeral_storage_set":"e","storage_placement_namespace":"director"}`)
	for name, extra := range map[string]map[string]any{
		"flat journal":               {"pve_storage_allocation_journal_dir": "/tmp/other"},
		"nested journal null":        {"pve": map[string]any{"storage_allocation_journal_dir": nil}},
		"flat case alias":            {"pve_Storage_sets": map[string]any{}},
		"nested case alias":          {"pve": map[string]any{"Storage_sets": map[string]any{}}},
		"unknown new policy":         {"pve_storage_sets_typo": map[string]any{}},
		"switch without policy":      {"pve_host": "other"},
		"switch without namespace":   {"pve_host": "other", "pve_storage_sets": map[string]any{}},
		"quoted age":                 {"pve_storage_status_max_age_seconds": "5"},
		"clear binding":              {"pve_ephemeral_storage_set": ""},
		"null sets":                  {"pve_storage_sets": nil},
		"dangling after replacement": {"pve_storage_sets": map[string]any{}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, _, err := config.ApplyContextOverrides(base, extra); err == nil {
				t.Fatal("unsafe override accepted")
			}
		})
	}
}

func TestStoragePlacementERBRoundTrip(t *testing.T) {
	if _, err := exec.LookPath("ruby"); err != nil {
		t.Skip("Ruby unavailable for ERB integration")
	}
	root := filepath.Join("..", "..", "..", "..")
	for _, strategy := range []string{"spread", "weighted_free_space", "least_utilized"} {
		t.Run(strategy, func(t *testing.T) {
			props := map[string]any{
				"storage_sets": map[string]any{
					"e":   map[string]any{"name_pattern": "^nfs-e-[0-9]+$", "strategy": map[string]any{"name": strategy, "version": 1}, "min_free_mb": 0, "max_utilization_pct": 85, "shared": true, "types": []string{"nfs"}},
					"p":   map[string]any{"names": []string{"nfs-p-1", "nfs-p-2"}, "strategy": map[string]any{"name": strategy, "version": 1}},
					"pin": map[string]any{"names": []string{"nfs-p-1"}, "strategy": map[string]any{"name": "spread", "version": 1}},
				},
				"ephemeral_storage_set": "e", "persistent_storage_set": "p", "root_storage_set": "e",
				"require_disjoint_storage_sets": false, "storage_status_max_age_seconds": 5,
				"storage_capacity_domains":    map[string]any{"nas": map[string]any{"members": []string{"nfs-p-1", "nfs-p-2"}}},
				"storage_placement_namespace": "production-director", "storage_allocation_journal_dir": "/var/vcap/store/pve_cpi/allocations",
			}
			encoded, err := json.Marshal(props)
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.CommandContext(t.Context(), "bash", filepath.Join(root, "scripts", "_test_erb_render.sh"), "--storage-placement-fixture", string(encoded))
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("render: %v\n%s", err, out)
			}
			cfg, err := config.Load(strings.NewReader(string(out)))
			if err != nil {
				t.Fatalf("load rendered JSON: %v", err)
			}
			if cfg.RequireDisjointStorageSets == nil || *cfg.RequireDisjointStorageSets || cfg.StorageSets["e"].Strategy.Name != strategy || !reflect.DeepEqual(cfg.StorageSets["pin"].Names, []string{"nfs-p-1"}) {
				t.Fatal("policy changed through rendering")
			}
			var raw map[string]json.RawMessage
			if err := json.Unmarshal(out, &raw); err != nil {
				t.Fatal(err)
			}
			var sets map[string]map[string]json.RawMessage
			if err := json.Unmarshal(raw["storage_sets"], &sets); err != nil {
				t.Fatal(err)
			}
			if string(sets["e"]["min_free_mb"]) != "0" {
				t.Fatal("ERB dropped explicit zero")
			}
		})
	}
}

func TestStorageAntiAffinityValidScopesAndBandsDecodeAndValidateCleanly(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		raw   string
		scope string
		band  int
	}{
		{"instance_group scope", `{"scope":"instance_group"}`, config.StorageAntiAffinityScopeInstanceGroup, config.DefaultStorageAntiAffinityBandPct},
		{"deployment scope", `{"scope":"deployment"}`, config.StorageAntiAffinityScopeDeployment, config.DefaultStorageAntiAffinityBandPct},
		{"none scope", `{"scope":"none"}`, config.StorageAntiAffinityScopeNone, config.DefaultStorageAntiAffinityBandPct},
		{"band zero", `{"scope":"instance_group","utilization_band_pct":0}`, config.StorageAntiAffinityScopeInstanceGroup, 0},
		{"band one hundred", `{"scope":"instance_group","utilization_band_pct":100}`, config.StorageAntiAffinityScopeInstanceGroup, 100},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := decodeStoragePlacement(t, `{"storage_sets":{"e":{"names":["nfs-e"],"strategy":{"name":"spread","version":1},"anti_affinity":`+tc.raw+`}}}`)
			if err := c.ValidateStoragePlacement(); err != nil {
				t.Fatalf("valid anti_affinity rejected: %v", err)
			}
			s := c.StorageSets["e"]
			if got := s.EffectiveAntiAffinityScope(); got != tc.scope {
				t.Fatalf("scope = %q, want %q", got, tc.scope)
			}
			if got := s.EffectiveAntiAffinityBandPct(); got != tc.band {
				t.Fatalf("band = %d, want %d", got, tc.band)
			}
		})
	}
}

func TestStoragePlacementCloneAntiAffinityDoesNotAlias(t *testing.T) {
	t.Parallel()
	c := decodeStoragePlacement(t, `{"storage_sets":{"e":`+placementSetJSON+`}}`)
	s := c.StorageSets["e"]
	s.AntiAffinity = &config.StorageAntiAffinity{Scope: config.StorageAntiAffinityScopeDeployment, UtilizationBandPct: intPtr(15)}
	c.StorageSets["e"] = s

	cloned := c.CloneStoragePlacement()
	clonedSet := cloned.StorageSets["e"]
	if clonedSet.AntiAffinity == c.StorageSets["e"].AntiAffinity {
		t.Fatal("cloned anti_affinity aliases the original pointer")
	}
	if clonedSet.AntiAffinity.UtilizationBandPct == c.StorageSets["e"].AntiAffinity.UtilizationBandPct {
		t.Fatal("cloned band aliases the original pointer")
	}
	*clonedSet.AntiAffinity.UtilizationBandPct = 90
	clonedSet.AntiAffinity.Scope = config.StorageAntiAffinityScopeNone
	if *c.StorageSets["e"].AntiAffinity.UtilizationBandPct != 15 || c.StorageSets["e"].AntiAffinity.Scope != config.StorageAntiAffinityScopeDeployment {
		t.Fatal("mutating the clone leaked into the original set")
	}
}

func TestStorageSetEffectiveAntiAffinityDefaultsNeverMaterializePointer(t *testing.T) {
	t.Parallel()
	c := decodeStoragePlacement(t, `{"storage_sets":{"e":`+placementSetJSON+`}}`)
	s := c.StorageSets["e"]
	if s.AntiAffinity != nil {
		t.Fatal("a set that never declared anti_affinity must decode with a nil pointer")
	}
	if got := s.EffectiveAntiAffinityScope(); got != config.DefaultStorageAntiAffinityScope {
		t.Fatalf("scope = %q, want default %q", got, config.DefaultStorageAntiAffinityScope)
	}
	if got := s.EffectiveAntiAffinityBandPct(); got != config.DefaultStorageAntiAffinityBandPct {
		t.Fatalf("band = %d, want default %d", got, config.DefaultStorageAntiAffinityBandPct)
	}
	if s.AntiAffinity != nil {
		t.Fatal("reading the effective defaults must never materialize the pointer")
	}
}
