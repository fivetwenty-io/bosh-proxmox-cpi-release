package handlers

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	inv "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/storageinventory"
)

// The two digests pinned in this file decide identity across an upgrade. The
// plan fingerprint is written into every journal record and is compared on
// every resume, and the context-override cache key decides which cached PVE
// bundle a dispatched request reuses. A storage set that declares no
// anti_affinity has to hash exactly as it hashed before the field existed, so
// the nil default has to stay a nil pointer through every JSON round trip the
// config takes. Materializing the default anywhere moves both digests, strands
// every in-flight allocation on a fingerprint its record does not carry, and
// these two literals are what makes that failure loud.
//
// Both fixtures below are owned by this file and must not be edited. They are
// deliberately not the shared plan fixture, because a change there would move
// the literals for a reason that has nothing to do with the guarantee.

// policyDigestSnapshot discovers the two members and the companion source the
// pinned request ranks over.
func policyDigestSnapshot(t *testing.T, cfg *config.CPIConfig) *inv.Snapshot {
	t.Helper()
	source := &planFixtureSource{statuses: map[string][]json.RawMessage{}}
	for _, id := range []string{"a", "b", "source"} {
		source.defs = append(source.defs, planJSON(t, map[string]any{
			"storage": id, "type": "nfs", "shared": 1,
			"server": "nas", "export": "/" + id, "content": "images,iso,import",
		}))
		for _, node := range []string{"n1", "n2"} {
			source.statuses[node] = append(source.statuses[node], planJSON(t, map[string]any{
				"storage": id, "active": 1, "enabled": 1,
				"total": uint64(100) << 30, "avail": uint64(80) << 30,
			}))
		}
	}
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	collector, err := inv.NewCollector(source, inv.Options{Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := collector.Discover(context.Background(), cfg, inv.Request{
		Nodes: []string{"n1", "n2"}, CompanionStorageIDs: []string{"source"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

// policyDigestConfig is the pinned configuration. Every field it sets reaches
// one of the two digests, and the storage set is the one the anti_affinity
// pointer hangs off.
func policyDigestConfig(anti *config.StorageAntiAffinity) *config.CPIConfig {
	return &config.CPIConfig{
		Host: "pve-a.example", Port: 8006, User: "cpi", Realm: "pam", Node: "n1",
		VMStorage: "a", RootStorageSet: "E", EphemeralStorageSet: "E",
		StoragePlacementNamespace: "director",
		StorageSets: map[string]config.StorageSet{"E": {
			Names:        []string{"a", "b"},
			Strategy:     config.StoragePlacementStrategy{Name: "spread", Version: 1},
			AntiAffinity: anti,
		}},
	}
}

// policyFingerprint ranks nothing. It builds the iterator the way every create
// builds it and reads the digest the iterator froze, which is the string that
// lands in the journal record as PolicyFingerprint.
func policyFingerprint(t *testing.T, anti *config.StorageAntiAffinity) string {
	t.Helper()
	cfg := policyDigestConfig(anti)
	snapshot := policyDigestSnapshot(t, cfg)
	selection, err := ResolveStoragePlacementSelectors(cfg, "create_vm", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	iterator, err := NewStoragePlanIterator(StoragePlanRequest{
		Selection: selection, Inventory: snapshot,
		Groups:    []StoragePlanNodeGroup{{AZ: "z", Nodes: []string{"n1", "n2"}}},
		Namespace: "director", AllocationKey: "agent", SeedSet: true,
		RootBytes: 1 << 30,
		Sources: []StorageRootSource{{Node: "n1", StorageID: "source",
			VolumeID: "source:import/stemcell.qcow2", VirtualBytes: 5 << 30}},
		SearchBudget: 100, Clock: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	return iterator.fingerprint
}

// policyFingerprintWithoutAntiAffinity is the digest the pinned configuration
// produced before StorageSet carried an anti_affinity field at all. It must
// never change. Every fingerprint already written to a journal, every resume
// that compares one, and every plan an operator has on disk depends on a set
// that declares nothing hashing exactly as it always did. It was captured by
// running this fixture against commit d3cb2457, the last commit before the
// field existed, rather than by pasting in whatever the current code returns.
const policyFingerprintWithoutAntiAffinity = "8bc61d2018f7ac553d7f77ab2fcdd20e3cb55a3fd91f1d6431af41ba1c9c9215"

func TestPolicyFingerprintIsUnchangedWithoutAntiAffinity(t *testing.T) {
	if got := policyFingerprint(t, nil); got != policyFingerprintWithoutAntiAffinity {
		t.Fatalf("a set that declares no anti_affinity now fingerprints %q, want %q",
			got, policyFingerprintWithoutAntiAffinity)
	}
}

// A declared anti_affinity block is policy, so it has to reach the digest. A
// set that spreads and a set that does not must never share one fingerprint.
func TestPolicyFingerprintMovesWhenAntiAffinityIsDeclared(t *testing.T) {
	band := 35
	cases := []struct {
		name string
		anti *config.StorageAntiAffinity
	}{
		{name: "a declared scope", anti: &config.StorageAntiAffinity{
			Scope: config.StorageAntiAffinityScopeNone,
		}},
		{name: "a declared band", anti: &config.StorageAntiAffinity{
			Scope: config.StorageAntiAffinityScopeInstanceGroup, UtilizationBandPct: &band,
		}},
	}
	seen := map[string]string{policyFingerprintWithoutAntiAffinity: "no anti_affinity"}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := policyFingerprint(t, testCase.anti)
			if owner, taken := seen[got]; taken {
				t.Fatalf("%s fingerprints as %s does: %q", testCase.name, owner, got)
			}
			seen[got] = testCase.name
		})
	}
}

// overrideCacheKeyWithoutAntiAffinity is the cache key the pinned
// configuration produced before StorageSet carried an anti_affinity field. Two
// requests whose effective configuration is the same must keep sharing one
// cached PVE bundle across the upgrade, so a set that declares nothing has to
// hash as it always did. A deliberate change to the key's field list is the
// one reason to move this literal, and adding a field to StorageSet is not.
// It was captured the same way the fingerprint above was, by running this
// fixture against commit d3cb2457.
const overrideCacheKeyWithoutAntiAffinity = "b9c9e9ebfd4ae8db857ac6b5dd452ca674d70a080453addadd81e3279677b653"

func TestRequestOverrideCacheKeyIsUnchangedWithoutAntiAffinity(t *testing.T) {
	t.Parallel()

	if got := requestOverrideCacheKey(policyDigestConfig(nil)); got != overrideCacheKeyWithoutAntiAffinity {
		t.Fatalf("a set that declares no anti_affinity now keys %q, want %q",
			got, overrideCacheKeyWithoutAntiAffinity)
	}
}

// The cache key decides which PVE bundle a request reuses, and anti_affinity
// changes placement, so two configurations that differ in it must never
// collide on one cached bundle.
func TestRequestOverrideCacheKeyMovesWhenAntiAffinityIsDeclared(t *testing.T) {
	t.Parallel()

	band := 35
	keys := map[string]string{
		requestOverrideCacheKey(policyDigestConfig(nil)): "no anti_affinity",
	}
	declared := []struct {
		name string
		anti *config.StorageAntiAffinity
	}{
		{name: "a declared scope", anti: &config.StorageAntiAffinity{
			Scope: config.StorageAntiAffinityScopeNone,
		}},
		{name: "a declared band", anti: &config.StorageAntiAffinity{
			Scope: config.StorageAntiAffinityScopeInstanceGroup, UtilizationBandPct: &band,
		}},
	}
	for _, testCase := range declared {
		got := requestOverrideCacheKey(policyDigestConfig(testCase.anti))
		if owner, taken := keys[got]; taken {
			t.Fatalf("%s keys as %s does: %q", testCase.name, owner, got)
		}
		keys[got] = testCase.name
	}
}

// The nil default has to survive the round trips the configuration takes on
// its way to both digests. Any one of them writing the default in would move
// the two literals above, so this asserts the pointer is still nil after the
// clone and after the selector snapshot the iterator marshals.
func TestAntiAffinityDefaultIsNeverMaterialized(t *testing.T) {
	cfg := policyDigestConfig(nil)
	cloned := cfg.CloneStoragePlacement()
	if set := cloned.StorageSets["E"]; set.AntiAffinity != nil {
		t.Fatalf("the clone materialized %+v", set.AntiAffinity)
	}
	selection, err := ResolveStoragePlacementSelectors(cfg, "create_vm", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(selection)
	if err != nil {
		t.Fatal(err)
	}
	var decoded StoragePlacementSelection
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Root == nil || decoded.Root.Set == nil {
		t.Fatalf("the round trip lost the root set: %+v", decoded.Root)
	}
	if decoded.Root.Set.AntiAffinity != nil {
		t.Fatalf("the selector round trip materialized %+v", decoded.Root.Set.AntiAffinity)
	}
	if set := cfg.StorageSets["E"]; set.AntiAffinity != nil {
		t.Fatalf("reading the defaults wrote %+v back onto the set", set.AntiAffinity)
	}
	if scope := cfg.StorageSets["E"].EffectiveAntiAffinityScope(); scope != config.DefaultStorageAntiAffinityScope {
		t.Fatalf("the set reports scope %q", scope)
	}
	if band := cfg.StorageSets["E"].EffectiveAntiAffinityBandPct(); band != config.DefaultStorageAntiAffinityBandPct {
		t.Fatalf("the set reports band %d", band)
	}
}
