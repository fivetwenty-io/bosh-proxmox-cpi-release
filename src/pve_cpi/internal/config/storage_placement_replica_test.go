package config

import (
	"strings"
	"testing"
)

func replicaCfg(adjust func(*CPIConfig)) *CPIConfig {
	c := &CPIConfig{
		VMStorage:           "ns1",
		EphemeralStorageSet: "eph",
		StorageSets: map[string]StorageSet{
			"eph": {Names: []string{"ns1", "ns2"}, Strategy: StoragePlacementStrategy{Name: "spread", Version: 1}},
		},
		StemcellReplicateStorageSet: true,
	}
	if adjust != nil {
		adjust(c)
	}
	return c
}

func TestStemcellReplicateStorageSetValidation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		adjust  func(*CPIConfig)
		wantErr string
	}{
		{"passes with ephemeral set", nil, ""},
		{"passes with root set", func(c *CPIConfig) { c.RootStorageSet = "eph" }, ""},
		{"off needs nothing", func(c *CPIConfig) { c.StemcellReplicateStorageSet = false; c.EphemeralStorageSet = "" }, ""},
		{"requires a root or ephemeral set", func(c *CPIConfig) { c.EphemeralStorageSet = "" }, "stemcell_replicate_storage_set requires root_storage_set or ephemeral_storage_set"},
		{"rejects import strategy", func(c *CPIConfig) { c.StemcellStrategy = StemcellStrategyImport }, "stemcell_replicate_storage_set requires stemcell_strategy template"},
		{"rejects colliding member tags", func(c *CPIConfig) {
			c.StorageSets["eph"] = StorageSet{Names: []string{"ns_1", "ns-1"}, Strategy: StoragePlacementStrategy{Name: "spread", Version: 1}}
		}, `members "ns_1" and "ns-1" sanitize to the same replica tag`},
		{"off ignores colliding member tags", func(c *CPIConfig) {
			c.StemcellReplicateStorageSet = false
			c.StorageSets["eph"] = StorageSet{Names: []string{"ns_1", "ns-1"}, Strategy: StoragePlacementStrategy{Name: "spread", Version: 1}}
		}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := replicaCfg(tc.adjust).ValidateStoragePlacement()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestEffectiveRootStorageSet(t *testing.T) {
	t.Parallel()
	if got := replicaCfg(nil).EffectiveRootStorageSet(); got != "eph" {
		t.Fatalf("want eph, got %q", got)
	}
	if got := replicaCfg(func(c *CPIConfig) { c.RootStorageSet = "root" }).EffectiveRootStorageSet(); got != "root" {
		t.Fatalf("want root, got %q", got)
	}
	if got := replicaCfg(func(c *CPIConfig) { c.EphemeralStorageSet = "" }).EffectiveRootStorageSet(); got != "" {
		t.Fatalf("want empty, got %q", got)
	}
}

func TestReplicaTagPart(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"pvuproxcf1_ns_1": "pvuproxcf1-ns-1",
		"NFS.Fast":        "nfs-fast",
		"a--b":            "a-b",
		"-lead":           "lead",
		"trail-":          "trail",
		"UP_per":          "up-per",
		"local":           "local",
	}
	for in, want := range cases {
		if got := ReplicaTagPart(in); got != want {
			t.Errorf("%q: want %q, got %q", in, want, got)
		}
	}
}
