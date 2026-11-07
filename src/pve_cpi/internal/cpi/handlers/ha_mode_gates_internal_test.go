package handlers

import (
	"context"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
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

// TestApplyPCINodeAffinityPin_Confinement shows the PCI check passes only a
// strict, enabled pin to the VM's node alone, and that a VM with no passthrough
// device or no registration needs no pin.
func TestApplyPCINodeAffinityPin_Confinement(t *testing.T) {
	t.Parallel()
	pts := []PCIPassthrough{{Address: "0000:01:00.0"}}
	registered := &ha.VMDecision{VMID: 321, SID: "vm:321", Outcome: ha.OutcomeRegister, Hard: []string{"pve01"}}
	skipped := &ha.VMDecision{VMID: 321, SID: "vm:321", Outcome: ha.OutcomeSkip}
	pin := func(strict, disabled bool, nodes ...string) *ha.Rule {
		r := &ha.Rule{ID: "bosh-pin-vm-321", Type: "node-affinity", Strict: strict, Disabled: disabled,
			Nodes: map[string]int{}}
		for _, n := range nodes {
			r.Nodes[n] = 0
		}
		return r
	}
	cases := []struct {
		name    string
		d       *ha.VMDecision
		pin     *ha.Rule
		pts     []PCIPassthrough
		wantErr bool
	}{
		{"no decision", nil, nil, pts, false},
		{"skipped", skipped, nil, pts, false},
		{"no device", registered, nil, nil, false},
		{"strict pin to the node", registered, pin(true, false, "pve01"), pts, false},
		{"no pin", registered, nil, pts, true},
		{"non-strict pin", registered, pin(false, false, "pve01"), pts, true},
		{"disabled pin", registered, pin(true, true, "pve01"), pts, true},
		{"pin to another node", registered, pin(true, false, "pve02"), pts, true},
		{"pin to a node set", registered, pin(true, false, "pve01", "pve02"), pts, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := applyPCINodeAffinityPin(tc.d, tc.pin, "pve01", tc.pts)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), "not a strict pin to that node alone") {
				t.Errorf("error must name the missing confinement, got %v", err)
			}
		})
	}
}

// TestApplyAZNodeAffinityPin_UnderMode shows the pin writer is silent under
// mode off and pins a registered VM to its AZ under a writing mode: pin_az
// defaults to auto and pin_az_strict to true, so the AZ is hard unless
// pin_az_strict is false.
func TestApplyAZNodeAffinityPin_UnderMode(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		ha         *config.HAConfig
		wantRules  int
		wantStrict bool
	}{
		"mode off":   {haModeOff(), 0, false},
		"auto alone": {haModeAuto(), 1, true},
		"pin on":     {haPinOn(nil), 1, true},
		"not strict": {haPinOn(new(false)), 1, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			stub := newNAStub()
			cfg := naPinConfig(map[string][]string{"z1": {"pve01", "pve02"}})
			cfg.HA = tc.ha
			deps := Deps{Config: cfg, PVE: &icPVEClient{clusterSvc: stub}, Agent: &icAgentStub{}, Logger: log.NewNopLogger(),
				RequestDirectorUUID: aaDirector, HAProfile: pve9Profile}
			if err := azPinApply(context.Background(), deps, 100,
				createVMCloudProps{AvailabilityZone: "z1"}, "pve01", log.NewNopLogger()); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(stub.rules) != tc.wantRules {
				t.Fatalf("rules written = %d, want %d", len(stub.rules), tc.wantRules)
			}
			if tc.wantRules == 0 {
				return
			}
			rule := stub.rules["bosh-na-100"]
			if got := rule.Strict != nil && *rule.Strict; got != tc.wantStrict {
				t.Errorf("strict = %v, want %v", got, tc.wantStrict)
			}
			if rule.Nodes == nil || !strings.Contains(*rule.Nodes, "pve01") || !strings.Contains(*rule.Nodes, "pve02") {
				t.Errorf("nodes = %v, want both AZ nodes", rule.Nodes)
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
			if err := aaApply(context.Background(), deps, 100, isoHAGroupEnv("web"), log.NewNopLogger()); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(stub.resources) != 0 || len(stub.rules) != 0 || stub.createRuleCalls != 0 {
				t.Errorf("wrote HA state: resources=%v rules=%v creates=%d", stub.resources, stub.rules, stub.createRuleCalls)
			}
		})
	}
}
