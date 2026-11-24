package ha_test

import (
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
)

// TestCreateRefusal covers the managed-mode refusals a create makes before
// anything exists. A missing prerequisite returns PrerequisiteError itself,
// and a request for the dynamic load balancer below PVE 9.2 is refused unless
// target_node takes the VM out of it. Auto mode never refuses here.
func TestCreateRefusal(t *testing.T) {
	t.Parallel()
	on := true
	dlbCfg := func(mode string, enabled bool) *config.CPIConfig {
		m := config.HAChoice(mode)
		c := &config.CPIConfig{HA: &config.HAConfig{Mode: &m}}
		if enabled {
			c.HA.DLB = &config.HADLBConfig{Enabled: &on}
		}
		return c
	}
	missing := &ha.Profile{Mode: config.HAModeManaged, Effective: ha.EffectiveObserveOnly,
		Gaps: []string{"missing Sys.Console on /"}, Reason: "missing Sys.Console on /"}
	full := &ha.Profile{Mode: config.HAModeManaged, Effective: ha.EffectiveFull, MinVersion: "9.1.2"}
	fullDLB := &ha.Profile{Mode: config.HAModeManaged, Effective: ha.EffectiveFull, MinVersion: "9.2.0", DLBSupported: true}
	auto := &ha.Profile{Mode: config.HAModeAuto, Effective: ha.EffectiveObserveOnly, Gaps: []string{"x"}}

	cases := []struct {
		name         string
		p            *ha.Profile
		cfg          *config.CPIConfig
		inDLBAZ      bool
		nodeTargeted bool
		want         string
	}{
		{"missing prerequisite", missing, dlbCfg(config.HAModeManaged, false), false, false,
			missing.PrerequisiteError().Error()},
		{"full, no dlb", full, dlbCfg(config.HAModeManaged, false), false, false, ""},
		{"dlb below 9.2", full, dlbCfg(config.HAModeManaged, true), false, false,
			`create_vm: the VM asks for the dynamic load balancer, which needs PVE 9.2 or later on every online node (lowest version "9.1.2")`},
		{"sentinel az below 9.2", full, dlbCfg(config.HAModeManaged, false), true, false, "asks for the dynamic load balancer"},
		{"target_node wins over dlb", full, dlbCfg(config.HAModeManaged, true), false, true, ""},
		{"dlb supported", fullDLB, dlbCfg(config.HAModeManaged, true), true, false, ""},
		{"auto never refuses", auto, dlbCfg(config.HAModeAuto, true), true, false, ""},
		{"nil profile", nil, dlbCfg(config.HAModeManaged, true), true, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.p.CreateRefusal(tc.cfg, tc.inDLBAZ, tc.nodeTargeted)
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("CreateRefusal = %v, want nil", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("CreateRefusal = %v, want an error containing %q", err, tc.want)
			}
		})
	}
}
