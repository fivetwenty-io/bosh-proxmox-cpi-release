package config

import (
	"strings"
	"testing"
)

func replicaPtr(b bool) *bool { return &b }

func replicaCfg(adjust func(*CPIConfig)) *CPIConfig {
	c := &CPIConfig{
		VMStorage:           "ns1",
		EphemeralStorageSet: "eph",
		StorageSets: map[string]StorageSet{
			"eph": {Names: []string{"ns1", "ns2"}, Strategy: StoragePlacementStrategy{Name: "spread", Version: 1}},
		},
		StemcellReplicateStorageSet: replicaPtr(true),
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
		{"off needs nothing", func(c *CPIConfig) { c.StemcellReplicateStorageSet = replicaPtr(false); c.EphemeralStorageSet = "" }, ""},
		{"requires a root or ephemeral set", func(c *CPIConfig) { c.EphemeralStorageSet = "" }, "stemcell_replicate_storage_set requires root_storage_set or ephemeral_storage_set"},
		{"rejects import strategy", func(c *CPIConfig) { c.StemcellStrategy = StemcellStrategyImport }, "stemcell_replicate_storage_set requires stemcell_strategy template"},
		{"rejects colliding member tags", func(c *CPIConfig) {
			c.StorageSets["eph"] = StorageSet{Names: []string{"ns_1", "ns-1"}, Strategy: StoragePlacementStrategy{Name: "spread", Version: 1}}
		}, `members "ns_1" and "ns-1" sanitize to the same replica tag`},
		{"off ignores colliding member tags", func(c *CPIConfig) {
			c.StemcellReplicateStorageSet = replicaPtr(false)
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

// TestStemcellReplicateStorageSetEnabled pins the tri-state resolution. The
// default is on because a set-managed root that lands on a member other than
// vm_storage copies the whole stemcell across the network otherwise, and it
// is silent where it cannot help rather than demanding the operator turn it
// off by hand.
func TestStemcellReplicateStorageSetEnabled(t *testing.T) {
	t.Parallel()
	on, off := true, false
	cases := []struct {
		name   string
		adjust func(*CPIConfig)
		want   bool
	}{
		{"unset with an ephemeral set replicates", func(c *CPIConfig) { c.StemcellReplicateStorageSet = nil }, true},
		{"unset with a root set replicates", func(c *CPIConfig) {
			c.StemcellReplicateStorageSet = nil
			c.EphemeralStorageSet = ""
			c.RootStorageSet = "eph"
		}, true},
		{"unset with no set bound stays quiet", func(c *CPIConfig) {
			c.StemcellReplicateStorageSet = nil
			c.EphemeralStorageSet = ""
		}, false},
		{"unset on the import strategy stays quiet", func(c *CPIConfig) {
			c.StemcellReplicateStorageSet = nil
			c.StemcellStrategy = StemcellStrategyImport
		}, false},
		{"explicit false opts out", func(c *CPIConfig) { c.StemcellReplicateStorageSet = &off }, false},
		{"explicit true agrees with the default", func(c *CPIConfig) { c.StemcellReplicateStorageSet = &on }, true},
		{"explicit true still cannot replicate without a set", func(c *CPIConfig) {
			c.StemcellReplicateStorageSet = &on
			c.EphemeralStorageSet = ""
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := replicaCfg(tc.adjust).StemcellReplicateStorageSetEnabled(); got != tc.want {
				t.Errorf("StemcellReplicateStorageSetEnabled() = %t, want %t", got, tc.want)
			}
		})
	}
	var nilCfg *CPIConfig
	if nilCfg.StemcellReplicateStorageSetEnabled() {
		t.Error("a nil config must never replicate")
	}
}
