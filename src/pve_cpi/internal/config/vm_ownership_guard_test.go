package config_test

import (
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
)

// TestVMOwnershipGuard_DefaultEnforce pins the default. An unset guard, a nil
// config, and an explicit "enforce" all resolve to enforce, and only "warn"
// (in any case, with surrounding space) relaxes it.
func TestVMOwnershipGuard_DefaultEnforce(t *testing.T) {
	t.Parallel()

	var nilCfg *config.CPIConfig
	if got := nilCfg.VMOwnershipGuardValue(); got != config.VMOwnershipGuardEnforce {
		t.Errorf("nil receiver: VMOwnershipGuardValue() = %q, want enforce", got)
	}

	cfg, err := mustLoad(t, `{
		"host":"h","user":"u","password":"p",
		"vm_storage":"s","disk_storage":"s","network_bridge":"br"
	}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := cfg.VMOwnershipGuardValue(); got != config.VMOwnershipGuardEnforce {
		t.Errorf("unset: VMOwnershipGuardValue() = %q, want enforce", got)
	}

	for raw, want := range map[string]string{
		"enforce":  config.VMOwnershipGuardEnforce,
		"ENFORCE":  config.VMOwnershipGuardEnforce,
		"warn":     config.VMOwnershipGuardWarn,
		" Warn ":   config.VMOwnershipGuardWarn,
		"wa rn":    config.VMOwnershipGuardEnforce,
		"disabled": config.VMOwnershipGuardEnforce,
	} {
		c := &config.CPIConfig{VMOwnershipGuard: raw}
		if got := c.VMOwnershipGuardValue(); got != want {
			t.Errorf("VMOwnershipGuardValue(%q) = %q, want %q", raw, got, want)
		}
	}
}

// TestVMOwnershipGuard_ValidateEnum proves an unknown mode, including the
// "off" the plan deliberately leaves out, fails at load instead of quietly
// resolving to a mode the operator did not ask for.
func TestVMOwnershipGuard_ValidateEnum(t *testing.T) {
	t.Parallel()
	for _, v := range []string{"enforce", "warn", "WARN"} {
		_, err := mustLoad(t, `{
			"host":"h","user":"u","password":"p",
			"vm_storage":"s","disk_storage":"s","network_bridge":"br",
			"vm_ownership_guard":"`+v+`"
		}`)
		if err != nil {
			t.Errorf("value %q: unexpected error: %v", v, err)
		}
	}
	for _, v := range []string{"off", "none", "disabled"} {
		_, err := mustLoad(t, `{
			"host":"h","user":"u","password":"p",
			"vm_storage":"s","disk_storage":"s","network_bridge":"br",
			"vm_ownership_guard":"`+v+`"
		}`)
		if err == nil || !strings.Contains(err.Error(), "vm_ownership_guard must be one of enforce|warn") {
			t.Errorf("value %q: err = %v, want the enum error", v, err)
		}
	}
}

// TestApplyContextOverrides_VMOwnershipGuard_Applies proves the guard is a
// per-entry override: the context value lands on the effective config, the
// base stays untouched, and a bad value is refused by the closing Validate.
func TestApplyContextOverrides_VMOwnershipGuard_Applies(t *testing.T) {
	t.Parallel()
	base := validBaseCfg()

	eff, applied, _, err := config.ApplyContextOverrides(base, map[string]any{
		"pve_vm_ownership_guard": "warn",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(applied) != 1 || applied[0] != "pve_vm_ownership_guard" {
		t.Errorf("applied = %#v, want [pve_vm_ownership_guard]", applied)
	}
	if got := eff.VMOwnershipGuardValue(); got != config.VMOwnershipGuardWarn {
		t.Errorf("eff.VMOwnershipGuardValue() = %q, want warn", got)
	}
	if got := base.VMOwnershipGuardValue(); got != config.VMOwnershipGuardEnforce {
		t.Errorf("base.VMOwnershipGuardValue() = %q, want unchanged enforce", got)
	}

	if _, _, _, err := config.ApplyContextOverrides(base, map[string]any{
		"pve_vm_ownership_guard": "off",
	}); err == nil {
		t.Error("pve_vm_ownership_guard=off: want a validation error, got nil")
	}
}
