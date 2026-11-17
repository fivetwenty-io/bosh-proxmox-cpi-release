package handlers

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
)

// TestManagedVMExecutionFingerprintAcceptsTransportSettings pins which config
// changes a frozen VM execution survives. The API transport settings that
// 0.10.0 added choose how the CPI reaches PVE, not what it builds, so a record
// stores its fingerprint without them, which is also what 0.9.x computes. A
// record matches however those settings change, and a record that 0.10.0
// through 0.10.2 froze with them included still matches while they are
// unchanged. Any other change, the ha block included, still refuses the
// recorded execution. The release_version the job template renders names the
// release, not the VM, so it stays out of the stored form too, and a record
// resumes under any later release.
func TestManagedVMExecutionFingerprintAcceptsTransportSettings(t *testing.T) {
	base := func() *config.CPIConfig {
		return &config.CPIConfig{Host: "pve.example", Node: "pve1", NetworkBridge: "vmbr0"}
	}
	withTransport := func(cfg *config.CPIConfig) *config.CPIConfig {
		cfg.Endpoints = []string{"pve.example:8006", "pve2.example:8006"}
		cfg.EndpointCacheDir = "/var/vcap/data/pve_cpi/endpoint-cache"
		cfg.APIEndpointDiscovery = config.DiscoveryModeOn
		return cfg
	}
	otherTransport := func(cfg *config.CPIConfig) *config.CPIConfig {
		cfg.Endpoints = []string{"pve.example:8006", "pve3.example:8006"}
		cfg.APIEndpointDiscovery = config.DiscoveryModeOff
		return cfg
	}
	fingerprint := func(cfg *config.CPIConfig) string {
		fp, err := managedVMExecutionFingerprint(cfg)
		if err != nil {
			t.Fatal(err)
		}
		return fp
	}
	full := func(cfg *config.CPIConfig) string {
		fp, err := managedVMExecutionFingerprintOf(cfg, false)
		if err != nil {
			t.Fatal(err)
		}
		return fp
	}
	withRelease := func(cfg *config.CPIConfig, version string) *config.CPIConfig {
		cfg.ReleaseVersion = version
		return cfg
	}
	// A record 0.9.x froze holds the fingerprint of the config without the
	// transport settings, and so does every record this release freezes.
	frozenBefore := fingerprint(base())
	frozenWith := fingerprint(withTransport(base()))
	if frozenWith != frozenBefore {
		t.Fatalf("a record frozen with transport settings stores %s, want the 0.9.x form %s", frozenWith, frozenBefore)
	}
	// A record that 0.10.0 through 0.10.2 froze holds the full fingerprint.
	frozenTenZero := full(withTransport(base()))
	if frozenTenZero == frozenBefore {
		t.Fatal("the transport settings do not move the full fingerprint, so the test proves nothing")
	}
	// A 0.9.x release hashes release_version but not the transport settings.
	frozenNineRelease := func(version string) string {
		fp, err := managedVMExecutionFingerprintForm(withRelease(base(), version), true, false)
		if err != nil {
			t.Fatal(err)
		}
		return fp
	}
	frozenNine := frozenNineRelease("0.9.7")
	if frozenNine == frozenBefore {
		t.Fatal("release_version does not move the 0.9.x form, so the test proves nothing")
	}
	frozenTenZeroRelease := func(version string) string {
		return full(withRelease(withTransport(base()), version))
	}
	strict := true
	for _, tc := range []struct {
		name    string
		current *config.CPIConfig
		stored  string
		want    bool
	}{
		{name: "unchanged config", current: base(), stored: frozenBefore, want: true},
		{name: "transport settings added after the record froze", current: withTransport(base()), stored: frozenBefore, want: true},
		{name: "record frozen with transport settings", current: withTransport(base()), stored: frozenWith, want: true},
		{name: "transport settings changed after the record froze them", current: otherTransport(base()), stored: frozenWith, want: true},
		{name: "transport settings removed after the record froze them", current: base(), stored: frozenWith, want: true},
		{name: "0.10.0 to 0.10.2 full fingerprint, settings unchanged", current: withTransport(base()), stored: frozenTenZero, want: true},
		{name: "0.10.0 to 0.10.2 full fingerprint, settings changed", current: otherTransport(base()), stored: frozenTenZero, want: false},
		{name: "endpoints alone added", current: func() *config.CPIConfig {
			cfg := base()
			cfg.Endpoints = []string{"pve.example:8006"}
			return cfg
		}(), stored: frozenBefore, want: true},
		{name: "transport settings added beside another change", current: func() *config.CPIConfig {
			cfg := withTransport(base())
			cfg.NetworkBridge = "vmbr1"
			return cfg
		}(), stored: frozenBefore, want: false},
		{name: "another change alone", current: func() *config.CPIConfig {
			cfg := base()
			cfg.NetworkBridge = "vmbr1"
			return cfg
		}(), stored: frozenBefore, want: false},
		{name: "ha block added", current: func() *config.CPIConfig {
			cfg := base()
			cfg.HA = &config.HAConfig{PinAZStrict: &strict}
			return cfg
		}(), stored: frozenBefore, want: false},
		{name: "ha block added beside transport settings", current: func() *config.CPIConfig {
			cfg := withTransport(base())
			cfg.HA = &config.HAConfig{PinAZStrict: &strict}
			return cfg
		}(), stored: frozenBefore, want: false},
		{name: "record frozen at 0.10.3 resumes under 0.10.4", current: withRelease(base(), "0.10.4"), stored: fingerprint(withRelease(base(), "0.10.3")), want: true},
		{name: "record frozen without a release version resumes under one", current: withRelease(base(), "0.10.4"), stored: frozenBefore, want: true},
		{name: "record frozen at 0.10.3 refused after ha is added", current: func() *config.CPIConfig {
			cfg := withRelease(base(), "0.10.4")
			cfg.HA = &config.HAConfig{PinAZStrict: &strict}
			return cfg
		}(), stored: fingerprint(withRelease(base(), "0.10.3")), want: false},
		{name: "0.9.x form, release unchanged", current: withRelease(base(), "0.9.7"), stored: frozenNine, want: true},
		{name: "0.9.x form, release unchanged beside new transport settings", current: withTransport(withRelease(base(), "0.9.7")), stored: frozenNine, want: true},
		{name: "0.9.x form, release changed", current: withRelease(base(), "0.10.3"), stored: frozenNine, want: false},
		{name: "0.10.0 to 0.10.2 full form, every setting unchanged", current: withRelease(withTransport(base()), "0.10.2"), stored: frozenTenZeroRelease("0.10.2"), want: true},
		{name: "0.10.0 to 0.10.2 full form, release changed", current: withRelease(withTransport(base()), "0.10.3"), stored: frozenTenZeroRelease("0.10.2"), want: false},
		{name: "unrelated fingerprint", current: withTransport(base()), stored: strings.Repeat("f", 64), want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := managedVMExecutionFingerprintMatches(tc.current, tc.stored)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("match = %t, want %t", got, tc.want)
			}
			// The resume path compares through shape, so it must agree.
			_, err = (&StorageVMExecution{Version: 1, InputFingerprint: tc.stored}).shape(tc.current)
			if tc.want && err != nil {
				t.Fatalf("resume refused a matching execution: %v", err)
			}
			if !tc.want && (err == nil || !strings.Contains(err.Error(), "VM execution defaults changed")) {
				t.Fatalf("resume accepted a changed execution: %v", err)
			}
		})
	}
	// New records store the fingerprint without the transport settings, which
	// is the value 0.9.x computes for the same config after a rollback.
	execution, err := freezeManagedVMExecution(withTransport(base()), &createVMShape{node: "pve1"})
	if err != nil {
		t.Fatal(err)
	}
	if execution.InputFingerprint != frozenBefore {
		t.Fatalf("new record froze %s, want the transportless fingerprint %s", execution.InputFingerprint, frozenBefore)
	}
}

// v097MinimalFingerprint is the execution fingerprint that v0.9.7 computes for
// the minimal configuration below, measured on a v0.9.7 build. The stored form
// must keep producing it, because a rollback to 0.9.x compares a record
// against that value. A change to this constant is a deliberate compatibility
// break with 0.9.x and needs a release note.
const v097MinimalFingerprint = "eea603abf6266d6f907a5e03e6a3faff3b0b098a127f3a8b65a3ed7a0b4c3e44"

const v097MinimalConfig = `{"host":"pve.example","port":8006,"user":"bosh","realm":"pve","node":"pve1","vm_storage":"local-lvm","disk_storage":"local-lvm","stemcell_storage":"local-lvm","iso_storage":"local","network_bridge":"vmbr0","network_mode":"bridge","stemcell_strategy":"","sdn_zone":"","sdn_zone_type":"","sdn_auto_manage_zone":false,"verify_ssl":false,"agent_mode":"cloudinit","log_level":"info","vmid_range_start":1000,"vmid_range_end":1999,"allow_disk_ops_with_snapshots":false,"require_snapshot_check_pass":false,"hotplug":"","numa":false,"reboot_mode":"","reboot_timeout":0,"vm_prefix":"","create_env_deployment":"","fetch_credential_defaults":[],"token_id":"bosh@pve!cpi","token_secret":"x"}`

func TestManagedVMExecutionFingerprintMatchesV097Value(t *testing.T) {
	var cfg config.CPIConfig
	if err := json.Unmarshal([]byte(v097MinimalConfig), &cfg); err != nil {
		t.Fatal(err)
	}
	cfg.ApplyDefaults()
	got, err := managedVMExecutionFingerprint(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got != v097MinimalFingerprint {
		t.Fatalf("stored form = %s, want the v0.9.7 value %s", got, v097MinimalFingerprint)
	}
	// A rollback target built from the same inputs, transport settings and a
	// release version included, still matches the value 0.9.7 stored.
	cfg.Endpoints = []string{"pve.example:8006"}
	cfg.APIEndpointDiscovery = config.DiscoveryModeOn
	cfg.ReleaseVersion = "0.10.4"
	matches, err := managedVMExecutionFingerprintMatches(&cfg, v097MinimalFingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if !matches {
		t.Fatal("a record with the v0.9.7 fingerprint no longer matches")
	}
}
