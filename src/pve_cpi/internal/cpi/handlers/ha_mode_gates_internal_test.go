package handlers

import (
	"context"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// haGateConfig returns a config with every old-style HA ingredient present, so
// that the pve.ha block alone decides what is written: scheduler-soft
// anti-affinity on, an az_map with two AZs, and one PCI-style node.
func haGateConfig(ha *config.HAConfig) *config.CPIConfig {
	cfg := isoHABaseConfig("local")
	on := true
	cfg.Placement = &config.PlacementConfig{
		AntiAffinity: &config.AntiAffinityConfig{Enabled: &on},
		AZMap:        map[string][]string{"z1": {"pve01", "pve02"}, "z2": {"pve03"}},
	}
	cfg.HA = ha
	return cfg
}

// TestHARegistrationFeatures_ModeOff_AffectsNoVM covers a missing pve.ha block
// and an explicit mode off with every writing key switched on: no feature is
// active for any AZ, including the DLB sentinel.
func TestHARegistrationFeatures_ModeOff_AffectsNoVM(t *testing.T) {
	t.Parallel()
	blocks := map[string]*config.HAConfig{
		"no ha block":          nil,
		"empty ha block":       {},
		"mode off, keys on":    haModeOff(),
		"mode off, dlb + pins": {Mode: haChoiceOf("off"), AntiAffinity: haChoiceOf("on"), PinAZ: haChoiceOf("on")},
	}
	for name, ha := range blocks {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			deps := Deps{Config: haGateConfig(ha)}
			for _, az := range []string{"", "z1", "dlb"} {
				got := haRegistrationFeatures(deps, createVMCloudProps{AvailabilityZone: az}, "pve01", isoHAGroupEnv("web"))
				if len(got) != 0 {
					t.Errorf("az %q: mode off must register no feature, got %v", az, got)
				}
			}
			if deps.Config.DLBConfigured() || deps.Config.HAAntiAffinityRulesEnabled() || deps.Config.HANodeAffinityPinEnabled() {
				t.Error("no HA writer may be enabled under mode off")
			}
		})
	}
}

// TestHARegistrationFeatures_ModeAutoAlone_WritesNoRuleOrPin shows that setting
// the mode alone does not start rule writing: anti-affinity rules and the AZ pin
// need an explicit "on". Only the default sentinel AZ registers, which is what
// the sentinel did before the mode existed.
func TestHARegistrationFeatures_ModeAutoAlone_WritesNoRuleOrPin(t *testing.T) {
	t.Parallel()
	deps := Deps{Config: haGateConfig(haModeAuto())}
	env := isoHAGroupEnv("web")

	if got := haRegistrationFeatures(deps, createVMCloudProps{AvailabilityZone: "z1"}, "pve01", env); len(got) != 0 {
		t.Errorf("mode auto alone, real AZ: want no feature, got %v", got)
	}
	got := haRegistrationFeatures(deps, createVMCloudProps{AvailabilityZone: "dlb"}, "pve01", env)
	if len(got) != 1 || got[0] != haFeatureDLB {
		t.Errorf("mode auto alone, sentinel AZ: want [%s], got %v", haFeatureDLB, got)
	}
	if deps.Config.HAAntiAffinityRulesEnabled() || deps.Config.HANodeAffinityPinEnabled() {
		t.Error("auto alone must not enable the anti-affinity rule or the AZ pin writers")
	}
}

// TestHARegistrationFeatures_ExplicitOn_WritesRuleAndPin shows the same config
// with the keys set to "on" does enable both writers.
func TestHARegistrationFeatures_ExplicitOn_WritesRuleAndPin(t *testing.T) {
	t.Parallel()
	ha := &config.HAConfig{Mode: haChoiceOf("auto"), AntiAffinity: haChoiceOf("on"), PinAZ: haChoiceOf("on")}
	deps := Deps{Config: haGateConfig(ha)}
	got := haRegistrationFeatures(deps, createVMCloudProps{AvailabilityZone: "z1"}, "pve01", isoHAGroupEnv("api"))
	if len(got) != 2 {
		t.Fatalf("want anti-affinity and az pin, got %v", got)
	}
}

// TestDLBEligibility_UnderMode pins the DLB readers to the mode.
func TestDLBEligibility_UnderMode(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		ha   *config.HAConfig
		az   string
		want bool
	}{
		{"off sentinel", haModeOff(), "dlb", false},
		{"unset sentinel", nil, "dlb", false},
		{"auto sentinel", haModeAuto(), "dlb", true},
		{"auto real az", haModeAuto(), "z1", false},
		{"auto master any az", haDLBEnabled(), "z1", true},
		{"auto master no az", haDLBEnabled(), "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := haGateConfig(tc.ha)
			if got := cfg.DLBEligibleForAZ(tc.az); got != tc.want {
				t.Errorf("DLBEligibleForAZ(%q) = %v, want %v", tc.az, got, tc.want)
			}
		})
	}
}

// TestApplyPCINodeAffinityPin_UnderMode shows the strict PCI pin is a no-op
// under mode off and is written under a mode that writes HA, whatever the
// pin_az key says.
func TestApplyPCINodeAffinityPin_UnderMode(t *testing.T) {
	t.Parallel()
	pts := []PCIPassthrough{{Address: "0000:01:00.0"}}
	cases := []struct {
		name      string
		ha        *config.HAConfig
		wantRules int
	}{
		{"no ha block", nil, 0},
		{"mode off", haModeOff(), 0},
		{"mode auto", haModeAuto(), 1},
		{"mode managed", &config.HAConfig{Mode: haChoiceOf("managed")}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stub := newNAStub()
			deps := naDeps(stub)
			deps.Config.HA = tc.ha
			if err := applyPCINodeAffinityPin(context.Background(), deps, 321, "pve01", pts, log.NewNopLogger()); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(stub.rules) != tc.wantRules {
				t.Errorf("rules written = %d, want %d (%v)", len(stub.rules), tc.wantRules, stub.rules)
			}
			if tc.wantRules == 0 && (len(stub.resources) != 0 || stub.createCalls != 0) {
				t.Errorf("mode off wrote HA state: resources=%v createCalls=%d", stub.resources, stub.createCalls)
			}
			if tc.wantRules == 1 && !stub.resources["vm:321"] {
				t.Error("the pinned VM must be registered as an HA resource")
			}
		})
	}
}

// TestApplyAZNodeAffinityPin_AutoAlone_WritesNothing shows the AZ pin needs an
// explicit "on".
func TestApplyAZNodeAffinityPin_AutoAlone_WritesNothing(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		ha        *config.HAConfig
		wantRules int
	}{
		"mode off":   {haModeOff(), 0},
		"auto alone": {haModeAuto(), 0},
		"pin on":     {haPinOn(nil), 1},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			stub := newNAStub()
			cfg := naPinConfig(map[string][]string{"z1": {"pve01", "pve02"}})
			cfg.HA = tc.ha
			deps := Deps{Config: cfg, PVE: &icPVEClient{clusterSvc: stub}, Agent: &icAgentStub{}, Logger: log.NewNopLogger()}
			if err := applyAZNodeAffinityPin(context.Background(), deps, 100,
				createVMCloudProps{AvailabilityZone: "z1"}, "pve01", log.NewNopLogger()); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(stub.rules) != tc.wantRules {
				t.Errorf("rules written = %d, want %d", len(stub.rules), tc.wantRules)
			}
		})
	}
}

// TestApplyAntiAffinityMembership_AutoAlone_WritesNothing shows the rule writer
// is silent under mode off and under auto alone. The stub embeds a nil service,
// so any PVE call it did not expect would panic.
func TestApplyAntiAffinityMembership_AutoAlone_WritesNothing(t *testing.T) {
	t.Parallel()
	for name, ha := range map[string]*config.HAConfig{"no ha block": nil, "mode off": haModeOff(), "auto alone": haModeAuto()} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			stub := newAAStub()
			cfg := aaMembershipConfig("", false, 0)
			cfg.HA = ha
			deps := aaDepsLock(cfg, stub, newAALockPools(nil))
			if err := applyAntiAffinityMembership(context.Background(), deps, 100, isoHAGroupEnv("web"), log.NewNopLogger()); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(stub.resources) != 0 || len(stub.rules) != 0 || stub.createRuleCalls != 0 {
				t.Errorf("wrote HA state: resources=%v rules=%v creates=%d", stub.resources, stub.rules, stub.createRuleCalls)
			}
		})
	}
}
