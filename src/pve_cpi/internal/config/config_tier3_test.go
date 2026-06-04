package config_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi/hooks"
)

// tier3BaseCfg returns a minimal config that passes Validate, for exercising the
// vm_firewall and hooks additions in isolation.
func tier3BaseCfg() *config.CPIConfig {
	return &config.CPIConfig{
		Host:           "h",
		User:           "u",
		Password:       "p",
		VMStorage:      "s",
		DiskStorage:    "s",
		NetworkBridge:  "br",
		Port:           8006,
		AgentMode:      "cloudinit",
		VMDiskFormat:   "qcow2",
		LogLevel:       "info",
		VMIDRangeStart: 100,
		VMIDRangeEnd:   5999,
		RebootMode:     "soft",
		RebootTimeout:  60,
		NetworkMode:    "auto",
		SDNZoneType:    "simple",
	}
}

func TestVMFirewallEnabled(t *testing.T) {
	cfg := &config.CPIConfig{}
	if cfg.VMFirewallEnabled() {
		t.Error("nil VMFirewall must be false (no behavior change default)")
	}
	cfg.VMFirewall = boolPtr(false)
	if cfg.VMFirewallEnabled() {
		t.Error("*false must be false")
	}
	cfg.VMFirewall = boolPtr(true)
	if !cfg.VMFirewallEnabled() {
		t.Error("*true must be true")
	}
}

func TestKeepFailedVMsEnabled(t *testing.T) {
	cfg := &config.CPIConfig{}
	if cfg.KeepFailedVMsEnabled() {
		t.Error("nil Debug must be false (no behavior change default)")
	}
	cfg.Debug = &config.DebugConfig{}
	if cfg.KeepFailedVMsEnabled() {
		t.Error("nil KeepFailedVMs must be false")
	}
	cfg.Debug.KeepFailedVMs = boolPtr(false)
	if cfg.KeepFailedVMsEnabled() {
		t.Error("*false must be false")
	}
	cfg.Debug.KeepFailedVMs = boolPtr(true)
	if !cfg.KeepFailedVMsEnabled() {
		t.Error("*true must be true")
	}
}

func TestKeepFailedVMs_JSONRoundTrip(t *testing.T) {
	cfg := &config.CPIConfig{}
	if err := json.Unmarshal([]byte(`{"debug":{"keep_failed_vms":true}}`), cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !cfg.KeepFailedVMsEnabled() {
		t.Error("debug.keep_failed_vms:true must decode to enabled")
	}
}

func TestHANodeAffinityPinEnabled(t *testing.T) {
	cfg := &config.CPIConfig{}
	if cfg.HANodeAffinityPinEnabled() {
		t.Error("nil Placement must be false")
	}
	cfg.Placement = &config.PlacementConfig{PinAZViaHARules: boolPtr(true)}
	if cfg.HANodeAffinityPinEnabled() {
		t.Error("pin without az_map must be false (nothing to pin against)")
	}
	cfg.Placement.AZMap = map[string][]string{"z1": {"pve01"}}
	if !cfg.HANodeAffinityPinEnabled() {
		t.Error("pin true + az_map must be enabled")
	}
	cfg.Placement.PinAZViaHARules = boolPtr(false)
	if cfg.HANodeAffinityPinEnabled() {
		t.Error("explicit false must be disabled")
	}
}

func TestPinAZStrict_DefaultsTrue(t *testing.T) {
	cfg := &config.CPIConfig{}
	if !cfg.PinAZStrict() {
		t.Error("nil Placement must default strict=true")
	}
	cfg.Placement = &config.PlacementConfig{}
	if !cfg.PinAZStrict() {
		t.Error("nil PinAZStrict must default true (durable AZ guarantee)")
	}
	cfg.Placement.PinAZStrict = boolPtr(false)
	if cfg.PinAZStrict() {
		t.Error("explicit false must be non-strict")
	}
}

func TestValidate_Pin_RequiresAZMap(t *testing.T) {
	cfg := tier3BaseCfg()
	cfg.Placement = &config.PlacementConfig{PinAZViaHARules: boolPtr(true)}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "az_map") {
		t.Fatalf("pin without az_map must fail naming az_map; got %v", err)
	}
}

func TestValidate_Pin_RejectsDLBSentinelAZ(t *testing.T) {
	cfg := tier3BaseCfg()
	cfg.Placement = &config.PlacementConfig{
		PinAZViaHARules: boolPtr(true),
		AZMap:           map[string][]string{"dlb": {"pve01"}, "z1": {"pve02"}},
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "DLB sentinel") {
		t.Fatalf("pin + DLB sentinel AZ must fail; got %v", err)
	}
}

func TestValidate_Pin_Valid(t *testing.T) {
	cfg := tier3BaseCfg()
	cfg.Placement = &config.PlacementConfig{
		PinAZViaHARules: boolPtr(true),
		AZMap:           map[string][]string{"z1": {"pve01", "pve02"}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid pin config must pass; got %v", err)
	}
}

func TestHooksValue(t *testing.T) {
	cfg := &config.CPIConfig{}
	if cfg.HooksValue() != nil {
		t.Error("absent Hooks must be nil")
	}
	cfg.Hooks = []string{"audit_log"}
	got := cfg.HooksValue()
	if len(got) != 1 || got[0] != "audit_log" {
		t.Errorf("HooksValue = %v; want [audit_log]", got)
	}
}

func TestValidate_Hooks_KnownPasses(t *testing.T) {
	cfg := tier3BaseCfg()
	cfg.Hooks = []string{"audit_log"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("a known hook must validate: %v", err)
	}
}

func TestValidate_Hooks_UnknownFails(t *testing.T) {
	cfg := tier3BaseCfg()
	cfg.Hooks = []string{"audit_log", "bogus"}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("an unknown hook name must fail validation")
	}
	if !strings.Contains(err.Error(), "bogus") {
		t.Errorf("error should name the unknown hook; got %v", err)
	}
}

func TestValidate_Hooks_EmptyPasses(t *testing.T) {
	cfg := tier3BaseCfg()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("no hooks configured must validate: %v", err)
	}
}

func TestValidate_LBRegister_RequiresBlock(t *testing.T) {
	cfg := tier3BaseCfg()
	cfg.Hooks = []string{"lb_register"}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "lb_register") {
		t.Fatalf("active lb_register without a block must fail; got %v", err)
	}
}

func TestValidate_LBRegister_RequiresEndpointAndBackend(t *testing.T) {
	cfg := tier3BaseCfg()
	cfg.Hooks = []string{"lb_register"}
	cfg.LBRegister = &hooks.LBRegisterConfig{} // empty endpoint + backend
	err := cfg.Validate()
	if err == nil {
		t.Fatal("lb_register without endpoint/backend must fail")
	}
	for _, want := range []string{"lb_register.endpoint", "lb_register.backend"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must name %q; got %v", want, err)
		}
	}
}

func TestValidate_LBRegister_Valid(t *testing.T) {
	cfg := tier3BaseCfg()
	cfg.Hooks = []string{"lb_register"}
	cfg.LBRegister = &hooks.LBRegisterConfig{Endpoint: "https://lb:5555", Backend: "cf-routers"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid lb_register must pass; got %v", err)
	}
}

func TestValidate_ExternalCommand_RelativeAllowlistFails(t *testing.T) {
	cfg := tier3BaseCfg()
	cfg.Hooks = []string{"external_command"}
	cfg.ExternalCommand = &hooks.ExternalCommandConfig{
		Command: "notify", Allowlist: []string{"notify"},
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "absolute path") {
		t.Fatalf("relative allowlist/command must fail with absolute-path error; got %v", err)
	}
}

func TestValidate_ExternalCommand_CommandNotInAllowlistFails(t *testing.T) {
	cfg := tier3BaseCfg()
	cfg.Hooks = []string{"external_command"}
	cfg.ExternalCommand = &hooks.ExternalCommandConfig{
		Command: "/usr/bin/notify", Allowlist: []string{"/bin/true"},
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "member of external_command.allowlist") {
		t.Fatalf("command outside allowlist must fail; got %v", err)
	}
}

func TestValidate_ExternalCommand_EmptyAllowlistFails(t *testing.T) {
	cfg := tier3BaseCfg()
	cfg.Hooks = []string{"external_command"}
	cfg.ExternalCommand = &hooks.ExternalCommandConfig{Command: "/usr/bin/notify"}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "allowlist must be non-empty") {
		t.Fatalf("empty allowlist must fail; got %v", err)
	}
}

func TestValidate_ExternalCommand_Valid(t *testing.T) {
	cfg := tier3BaseCfg()
	cfg.Hooks = []string{"external_command"}
	cfg.ExternalCommand = &hooks.ExternalCommandConfig{
		Command: "/usr/bin/notify", Allowlist: []string{"/usr/bin/notify"},
		EnvPasslist: []string{"PATH"}, TimeoutMS: 5000, Methods: []string{"create_vm"},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid external_command must pass; got %v", err)
	}
}
