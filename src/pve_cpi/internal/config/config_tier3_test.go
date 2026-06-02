package config_test

import (
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
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
