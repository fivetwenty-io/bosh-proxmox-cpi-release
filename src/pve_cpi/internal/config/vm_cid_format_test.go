package config_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
)

// cidTestLoad loads a minimal job config with extra JSON members appended.
func cidTestLoad(t *testing.T, extra string) (*config.CPIConfig, error) {
	t.Helper()
	body := `{
		"host":"h","user":"u","password":"p",
		"vm_storage":"s","disk_storage":"s","network_bridge":"br"`
	if extra != "" {
		body += "," + extra
	}
	return mustLoad(t, body+"}")
}

// TestVMCIDFormat_DefaultStamped pins the default. An unset format, a nil
// config, and an explicit "stamped" all resolve to stamped, and only "bare"
// returns bare CIDs.
func TestVMCIDFormat_DefaultStamped(t *testing.T) {
	t.Parallel()
	var nilCfg *config.CPIConfig
	if got := nilCfg.VMCIDFormatValue(); got != config.VMCIDFormatStamped {
		t.Errorf("nil receiver: VMCIDFormatValue() = %q, want stamped", got)
	}
	cfg, err := cidTestLoad(t, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := cfg.VMCIDFormatValue(); got != config.VMCIDFormatStamped {
		t.Errorf("unset: VMCIDFormatValue() = %q, want stamped", got)
	}
	for raw, want := range map[string]string{
		"stamped": config.VMCIDFormatStamped,
		"bare":    config.VMCIDFormatBare,
		" BARE ":  config.VMCIDFormatBare,
		"b are":   config.VMCIDFormatStamped,
	} {
		c := &config.CPIConfig{VMCIDFormat: raw}
		if got := c.VMCIDFormatValue(); got != want {
			t.Errorf("VMCIDFormatValue(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestVMCIDFormat_ValidateEnum(t *testing.T) {
	t.Parallel()
	for _, v := range []string{"stamped", "bare", "Bare"} {
		if _, err := cidTestLoad(t, `"vm_cid_format":"`+v+`"`); err != nil {
			t.Errorf("value %q: unexpected error: %v", v, err)
		}
	}
	for _, v := range []string{"numeric", "legacy", "off"} {
		_, err := cidTestLoad(t, `"vm_cid_format":"`+v+`"`)
		if err == nil || !strings.Contains(err.Error(), "vm_cid_format must be one of stamped|bare") {
			t.Errorf("value %q: err = %v, want the enum error", v, err)
		}
	}
}

// TestVMCIDFormat_JobLevelOnly pins that a cpi_config entry cannot set the
// format. A release either can or cannot read stamped CIDs, so one entry
// returning bare CIDs while another stamps them would only hide which CIDs a
// downgrade can still read. The key is reported as unknown, the way every
// key without an override is.
func TestVMCIDFormat_JobLevelOnly(t *testing.T) {
	t.Parallel()
	eff, applied, unknown, err := config.ApplyContextOverrides(validBaseCfg(), map[string]any{"pve_vm_cid_format": "bare"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(applied) != 0 || len(unknown) != 1 || unknown[0] != "pve_vm_cid_format" {
		t.Errorf("applied = %#v, unknown = %#v, want pve_vm_cid_format reported unknown", applied, unknown)
	}
	if got := eff.VMCIDFormatValue(); got != config.VMCIDFormatStamped {
		t.Errorf("eff.VMCIDFormatValue() = %q, want the job's stamped", got)
	}
}

func TestClusterStampAliases_Validate(t *testing.T) {
	t.Parallel()
	if _, err := cidTestLoad(t, `"cluster_stamp_aliases":["0a1b2c3d","99aabbcc"]`); err != nil {
		t.Errorf("valid aliases: unexpected error: %v", err)
	}
	if _, err := cidTestLoad(t, `"cluster_stamp_aliases":[]`); err != nil {
		t.Errorf("empty aliases: unexpected error: %v", err)
	}
	for list, want := range map[string]string{
		`["0A1B2C3D"]`:            "must be eight lowercase hex characters",
		`["0a1b2c3"]`:             "must be eight lowercase hex characters",
		`["0a1b2c3d4"]`:           "must be eight lowercase hex characters",
		`["0a1b2c3g"]`:            "must be eight lowercase hex characters",
		`[""]`:                    "must be eight lowercase hex characters",
		`["00000000"]`:            "names no cluster",
		`["0a1b2c3d","0a1b2c3d"]`: "is listed twice",
	} {
		_, err := cidTestLoad(t, `"cluster_stamp_aliases":`+list)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("aliases %s: err = %v, want one containing %q", list, err, want)
		}
	}
}

func TestClusterStampKnown(t *testing.T) {
	t.Parallel()
	c := &config.CPIConfig{ClusterStampAliases: []string{"99aabbcc"}}
	cases := []struct {
		stamp, live string
		want        bool
	}{
		{"0a1b2c3d", "0a1b2c3d", true},
		{"99aabbcc", "0a1b2c3d", true},
		{"99aabbcc", "", true},
		{"11111111", "0a1b2c3d", false},
		{"", "", false},
		{"", "0a1b2c3d", false},
	}
	for _, tc := range cases {
		if got := c.ClusterStampKnown(tc.stamp, tc.live); got != tc.want {
			t.Errorf("ClusterStampKnown(%q, %q) = %v, want %v", tc.stamp, tc.live, got, tc.want)
		}
	}
	var nilCfg *config.CPIConfig
	if !nilCfg.ClusterStampKnown("0a1b2c3d", "0a1b2c3d") || nilCfg.ClusterStampKnown("99aabbcc", "0a1b2c3d") {
		t.Error("a nil config must match only the live stamp")
	}
}

// TestApplyContextOverrides_ClusterStampAliases pins the per-entry key. The
// entry's list replaces the job's, an empty list or null declares none, and a
// bad entry is refused by the closing Validate.
func TestApplyContextOverrides_ClusterStampAliases(t *testing.T) {
	t.Parallel()
	base := validBaseCfg()
	base.ClusterStampAliases = []string{"11111111"}
	for _, tc := range []struct {
		name string
		v    any
		want []string
	}{
		{"list", []any{"0a1b2c3d", "99aabbcc"}, []string{"0a1b2c3d", "99aabbcc"}},
		{"empty list", []any{}, nil},
		{"null", nil, nil},
	} {
		eff, applied, _, err := config.ApplyContextOverrides(base, map[string]any{"pve_cluster_stamp_aliases": tc.v})
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", tc.name, err)
		}
		if len(applied) != 1 || applied[0] != "pve_cluster_stamp_aliases" {
			t.Errorf("%s: applied = %#v", tc.name, applied)
		}
		if len(eff.ClusterStampAliases) != len(tc.want) || (len(tc.want) > 0 && !reflect.DeepEqual(eff.ClusterStampAliases, tc.want)) {
			t.Errorf("%s: eff aliases = %#v, want %#v", tc.name, eff.ClusterStampAliases, tc.want)
		}
	}
	if !reflect.DeepEqual(base.ClusterStampAliases, []string{"11111111"}) {
		t.Errorf("the override wrote through to the job config: %#v", base.ClusterStampAliases)
	}
	for _, bad := range []any{[]any{"00000000"}, []any{"XYZ"}, "0a1b2c3d", []any{float64(1)}} {
		if _, _, _, err := config.ApplyContextOverrides(base, map[string]any{"pve_cluster_stamp_aliases": bad}); err == nil {
			t.Errorf("pve_cluster_stamp_aliases=%#v: want an error, got nil", bad)
		}
	}
}

// TestApplyContextOverrides_ClusterStampAliases_ResetOnEndpointChange pins
// that the job's aliases, which record the job cluster's root CA history,
// never follow a request to another endpoint unless the entry names them.
func TestApplyContextOverrides_ClusterStampAliases_ResetOnEndpointChange(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		extra map[string]any
		want  []string
	}{
		{"host change resets", clusterTestEntry(nil), nil},
		{"host change keeps the entry's own list", clusterTestEntry(map[string]any{"pve_cluster_stamp_aliases": []any{"99aabbcc"}}), []string{"99aabbcc"}},
		{"same endpoint inherits", map[string]any{"pve_vm_storage": "other"}, []string{"11111111"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			base := clusterTestJob(t)
			base.ClusterStampAliases = []string{"11111111"}
			eff, _ := credTestMustApply(t, base, tc.extra)
			if !reflect.DeepEqual(eff.ClusterStampAliases, tc.want) {
				t.Errorf("eff aliases = %#v, want %#v", eff.ClusterStampAliases, tc.want)
			}
			if !reflect.DeepEqual(base.ClusterStampAliases, []string{"11111111"}) {
				t.Error("the reset wrote through to the job config")
			}
		})
	}
}
