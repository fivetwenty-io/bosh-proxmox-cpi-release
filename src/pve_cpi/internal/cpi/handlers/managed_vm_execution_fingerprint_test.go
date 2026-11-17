package handlers

import (
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
// recorded execution.
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
