package config_test

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
)

// -----------------------------------------------------------------------
// ApplyContextOverrides — no-op / byte-identical fast path
// -----------------------------------------------------------------------

func TestApplyContextOverrides_NilBase(t *testing.T) {
	t.Parallel()
	_, _, _, err := config.ApplyContextOverrides(nil, map[string]any{"pve_host": "x"})
	if err == nil {
		t.Fatal("expected error for nil base, got nil")
	}
}

func TestApplyContextOverrides_EmptyExtra_ReturnsBaseUnchanged(t *testing.T) {
	t.Parallel()
	base := validBaseCfg()

	eff, applied, unknown, err := config.ApplyContextOverrides(base, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if eff != base {
		t.Error("expected the SAME pointer back (byte-identical fast path) for nil extra")
	}
	if applied != nil {
		t.Errorf("applied = %#v, want nil", applied)
	}
	if unknown != nil {
		t.Errorf("unknown = %#v, want nil", unknown)
	}

	// Same contract for an explicitly-empty (non-nil) map.
	eff2, applied2, unknown2, err2 := config.ApplyContextOverrides(base, map[string]any{})
	if err2 != nil {
		t.Fatalf("unexpected error: %v", err2)
	}
	if eff2 != base {
		t.Error("expected the SAME pointer back for an empty (non-nil) extra map")
	}
	if applied2 != nil || unknown2 != nil {
		t.Errorf("applied2=%#v unknown2=%#v, want both nil", applied2, unknown2)
	}
}

func TestApplyContextOverrides_OnlyUnknownKeys_ReturnsBaseUnchanged(t *testing.T) {
	t.Parallel()
	base := validBaseCfg()

	// pve_placement_enabled is a real job property, but is deliberately not
	// in the per-request override set (it is process-wide placement policy,
	// not connection/routing identity) — see contextOverrideFieldOrder's doc.
	eff, applied, unknown, err := config.ApplyContextOverrides(base, map[string]any{
		"pve_placement_enabled": true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if eff != base {
		t.Error("expected the SAME pointer back when no SUPPORTED override key is present")
	}
	if applied != nil {
		t.Errorf("applied = %#v, want nil", applied)
	}
	if len(unknown) != 1 || unknown[0] != "pve_placement_enabled" {
		t.Errorf("unknown = %#v, want [pve_placement_enabled]", unknown)
	}
}

func TestApplyContextOverrides_NonPVEPrefixedKeys_Ignored(t *testing.T) {
	t.Parallel()
	base := validBaseCfg()
	eff, applied, unknown, err := config.ApplyContextOverrides(base, map[string]any{
		"some_future_context_key": "value",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if eff != base || applied != nil || unknown != nil {
		t.Errorf("non-pve_-prefixed extra keys must be silently ignored, got eff==base:%v applied:%#v unknown:%#v",
			eff == base, applied, unknown)
	}
}

// -----------------------------------------------------------------------
// ApplyContextOverrides — override application, base immutability, and
// absent-key inheritance
// -----------------------------------------------------------------------

func TestApplyContextOverrides_AppliesOverrides_BaseUnmutated(t *testing.T) {
	t.Parallel()
	base := validBaseCfg()
	base.Node = "pve01"
	base.NetworkBridge = "vmbr0"

	// A new host needs its own credentials (see the credential group tests
	// below), so the entry carries a token alongside the host.
	eff, applied, unknown, err := config.ApplyContextOverrides(base, map[string]any{
		"pve_host":       "10.255.0.10",
		"pve_api_token":  "fake-entry@pve!cpi=fake-entry-token-value",
		"pve_vm_storage": "az2-vms",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(unknown) != 0 {
		t.Errorf("unknown = %#v, want empty", unknown)
	}
	wantApplied := []string{"pve_api_token", "pve_host", "pve_vm_storage"} // already alpha-sorted
	if !equalStringSlices(applied, wantApplied) {
		t.Errorf("applied = %#v, want %#v", applied, wantApplied)
	}

	if eff == base {
		t.Fatal("expected a NEW *CPIConfig when overrides applied, got the same pointer")
	}
	if eff.Host != "10.255.0.10" {
		t.Errorf("eff.Host = %q, want 10.255.0.10", eff.Host)
	}
	if eff.VMStorage != "az2-vms" {
		t.Errorf("eff.VMStorage = %q, want az2-vms", eff.VMStorage)
	}

	// Absent keys inherit job config unchanged.
	if eff.Node != "pve01" {
		t.Errorf("eff.Node = %q, want inherited pve01", eff.Node)
	}
	if eff.NetworkBridge != "vmbr0" {
		t.Errorf("eff.NetworkBridge = %q, want inherited vmbr0", eff.NetworkBridge)
	}

	// base itself must never be mutated by ApplyContextOverrides.
	if base.Host != "h" {
		t.Errorf("base.Host mutated to %q, want unchanged \"h\"", base.Host)
	}
	if base.VMStorage != "s" {
		t.Errorf("base.VMStorage mutated to %q, want unchanged \"s\"", base.VMStorage)
	}
}

func TestApplyContextOverrides_VerifySSL_NewPointer_BaseUnmutated(t *testing.T) {
	t.Parallel()
	base := validBaseCfg() // VerifySSL = true
	if !base.VerifySSLValue() {
		t.Fatal("test fixture assumption broken: base.VerifySSLValue() should start true")
	}

	eff, applied, _, err := config.ApplyContextOverrides(base, map[string]any{
		"pve_verify_ssl": false,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(applied) != 1 || applied[0] != "pve_verify_ssl" {
		t.Errorf("applied = %#v, want [pve_verify_ssl]", applied)
	}
	if eff.VerifySSLValue() != false {
		t.Errorf("eff.VerifySSLValue() = true, want false")
	}
	if base.VerifySSLValue() != true {
		t.Errorf("base.VerifySSLValue() mutated to false, want unchanged true")
	}
	// eff.VerifySSL must be a DIFFERENT pointer than base.VerifySSL, proving
	// the override allocated a fresh *bool rather than writing through the
	// shared shallow-copy pointer.
	if eff.VerifySSL == base.VerifySSL {
		t.Error("eff.VerifySSL and base.VerifySSL point at the same *bool; override must allocate a new one")
	}
}

// -----------------------------------------------------------------------
// Empty-string vs unset semantics
// -----------------------------------------------------------------------

func TestApplyContextOverrides_ExplicitEmptyString_Applies(t *testing.T) {
	t.Parallel()
	base := validBaseCfg()
	base.AgentMBus = "https://mbus:pw@job-level-mbus:6868"

	// An explicit empty-string override for pve_agent_mbus must APPLY (clear
	// the inherited value), not be treated as "absent". pve_agent_mbus is
	// used here specifically because it carries no eff.Validate() constraint
	// (unlike, say, pve_password — see
	// TestApplyContextOverrides_EmptyString_ClearingBothCredentials_Rejected
	// below), so this test isolates the empty-string-applies SEMANTIC from
	// the separate question of whether the resulting effective config is
	// still valid.
	eff, applied, _, err := config.ApplyContextOverrides(base, map[string]any{
		"pve_agent_mbus": "",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(applied) != 1 || applied[0] != "pve_agent_mbus" {
		t.Errorf("applied = %#v, want [pve_agent_mbus] (explicit empty string must count as applied)", applied)
	}
	if eff.AgentMBus != "" {
		t.Errorf("eff.AgentMBus = %q, want empty string", eff.AgentMBus)
	}
	if base.AgentMBus != "https://mbus:pw@job-level-mbus:6868" {
		t.Errorf("base.AgentMBus mutated to %q, want unchanged", base.AgentMBus)
	}
}

func TestApplyContextOverrides_AbsentKey_Inherits(t *testing.T) {
	t.Parallel()
	base := validBaseCfg()
	base.Password = "job-level-password"

	// pve_password absent entirely on the job's own endpoint (only a storage
	// key present): Password must be INHERITED from base, not cleared. An
	// entry that changes the endpoint never inherits it; see
	// TestApplyContextOverrides_HostChange_DropsInheritedCredentials.
	eff, _, _, err := config.ApplyContextOverrides(base, map[string]any{
		"pve_vm_storage": "az2-vms",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if eff.Password != "job-level-password" {
		t.Errorf("eff.Password = %q, want inherited \"job-level-password\" (key was absent, not empty)", eff.Password)
	}
}

// -----------------------------------------------------------------------
// Type coercion — numbers and booleans arriving as either JSON type or string
// -----------------------------------------------------------------------

func TestApplyContextOverrides_PortCoercion(t *testing.T) {
	t.Parallel()

	t.Run("as JSON number", func(t *testing.T) {
		t.Parallel()
		eff, _, _, err := config.ApplyContextOverrides(validBaseCfg(), map[string]any{
			"pve_port": float64(8007), "pve_user": "u", "pve_password": "fake-entry-password",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if eff.Port != 8007 {
			t.Errorf("eff.Port = %d, want 8007", eff.Port)
		}
	})

	t.Run("as numeric string", func(t *testing.T) {
		t.Parallel()
		eff, _, _, err := config.ApplyContextOverrides(validBaseCfg(), map[string]any{
			"pve_port": "8007", "pve_user": "u", "pve_password": "fake-entry-password",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if eff.Port != 8007 {
			t.Errorf("eff.Port = %d, want 8007", eff.Port)
		}
	})

	t.Run("non-integer number rejected", func(t *testing.T) {
		t.Parallel()
		_, _, _, err := config.ApplyContextOverrides(validBaseCfg(), map[string]any{"pve_port": 8006.5})
		if err == nil {
			t.Fatal("expected error for non-integer pve_port, got nil")
		}
	})

	t.Run("non-numeric string rejected", func(t *testing.T) {
		t.Parallel()
		_, _, _, err := config.ApplyContextOverrides(validBaseCfg(), map[string]any{"pve_port": "not-a-number"})
		if err == nil {
			t.Fatal("expected error for non-numeric pve_port string, got nil")
		}
		if !strings.Contains(err.Error(), "pve_port") {
			t.Errorf("error %q should mention pve_port", err.Error())
		}
	})

	t.Run("out of range rejected", func(t *testing.T) {
		t.Parallel()
		_, _, _, err := config.ApplyContextOverrides(validBaseCfg(), map[string]any{"pve_port": 99999})
		if err == nil {
			t.Fatal("expected error for out-of-range pve_port, got nil")
		}
	})

	t.Run("wrong JSON type rejected", func(t *testing.T) {
		t.Parallel()
		_, _, _, err := config.ApplyContextOverrides(validBaseCfg(), map[string]any{"pve_port": true})
		if err == nil {
			t.Fatal("expected error for boolean pve_port, got nil")
		}
	})
}

func TestApplyContextOverrides_VerifySSLCoercion(t *testing.T) {
	t.Parallel()

	t.Run("as JSON bool", func(t *testing.T) {
		t.Parallel()
		eff, _, _, err := config.ApplyContextOverrides(validBaseCfg(), map[string]any{"pve_verify_ssl": false})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if eff.VerifySSLValue() != false {
			t.Error("eff.VerifySSLValue() = true, want false")
		}
	})

	t.Run("as string", func(t *testing.T) {
		t.Parallel()
		eff, _, _, err := config.ApplyContextOverrides(validBaseCfg(), map[string]any{"pve_verify_ssl": "false"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if eff.VerifySSLValue() != false {
			t.Error("eff.VerifySSLValue() = true, want false")
		}
	})

	t.Run("as \"1\"/\"0\" string", func(t *testing.T) {
		t.Parallel()
		eff, _, _, err := config.ApplyContextOverrides(validBaseCfg(), map[string]any{"pve_verify_ssl": "1"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if eff.VerifySSLValue() != true {
			t.Error("eff.VerifySSLValue() = false, want true (\"1\" parses as true)")
		}
	})

	t.Run("invalid string rejected", func(t *testing.T) {
		t.Parallel()
		_, _, _, err := config.ApplyContextOverrides(validBaseCfg(), map[string]any{"pve_verify_ssl": "not-a-bool"})
		if err == nil {
			t.Fatal("expected error for invalid pve_verify_ssl string, got nil")
		}
	})

	t.Run("wrong JSON type rejected", func(t *testing.T) {
		t.Parallel()
		_, _, _, err := config.ApplyContextOverrides(validBaseCfg(), map[string]any{"pve_verify_ssl": 1.0})
		if err == nil {
			t.Fatal("expected error for numeric pve_verify_ssl, got nil")
		}
	})
}

// -----------------------------------------------------------------------
// pve_vmid_range_start/end structural validation
// -----------------------------------------------------------------------

func TestApplyContextOverrides_VMIDRange_BothOverridden_OrderEnforced(t *testing.T) {
	t.Parallel()
	_, _, _, err := config.ApplyContextOverrides(validBaseCfg(), map[string]any{
		"pve_vmid_range_start": 500,
		"pve_vmid_range_end":   400,
	})
	if err == nil {
		t.Fatal("expected error for inverted vmid range when both bounds are overridden together")
	}
	// The error now comes from eff.Validate() (config.go's own field-name
	// convention "vmid_range_end", not the context key "pve_vmid_range_end").
	if !strings.Contains(err.Error(), "vmid_range_end") {
		t.Errorf("error %q should name vmid_range_end", err.Error())
	}
}

func TestApplyContextOverrides_VMIDRange_BothOverridden_ValidOrder(t *testing.T) {
	t.Parallel()
	eff, _, _, err := config.ApplyContextOverrides(validBaseCfg(), map[string]any{
		"pve_vmid_range_start": 400,
		"pve_vmid_range_end":   500,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if eff.VMIDRangeStart != 400 || eff.VMIDRangeEnd != 500 {
		t.Errorf("eff range = [%d,%d], want [400,500]", eff.VMIDRangeStart, eff.VMIDRangeEnd)
	}
}

func TestApplyContextOverrides_VMIDRange_OnlyStartOverridden_InheritsEnd(t *testing.T) {
	t.Parallel()
	base := validBaseCfg() // VMIDRangeEnd = 5999 per validBaseCfg
	eff, _, _, err := config.ApplyContextOverrides(base, map[string]any{
		"pve_vmid_range_start": 100,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if eff.VMIDRangeStart != 100 {
		t.Errorf("eff.VMIDRangeStart = %d, want 100", eff.VMIDRangeStart)
	}
	if eff.VMIDRangeEnd != base.VMIDRangeEnd {
		t.Errorf("eff.VMIDRangeEnd = %d, want inherited %d", eff.VMIDRangeEnd, base.VMIDRangeEnd)
	}
}

func TestApplyContextOverrides_VMIDRangeStart_NonPositive_Rejected(t *testing.T) {
	t.Parallel()
	_, _, _, err := config.ApplyContextOverrides(validBaseCfg(), map[string]any{"pve_vmid_range_start": 0})
	if err == nil {
		t.Fatal("expected error for pve_vmid_range_start=0, got nil")
	}
}

// -----------------------------------------------------------------------
// H1 (A13 review) reproducers — the effective override config must be
// re-validated against the SAME invariants config.Validate checks for the
// job-level config at CPI startup. Each test below is a confirmed
// reproducer from the review that the (now-removed) ad-hoc "only check vmid
// ordering when both bounds are overridden together" logic missed.
// -----------------------------------------------------------------------

// TestApplyContextOverrides_VMIDRange_SingleBoundInversion_Rejected is the
// primary H1 reproducer: overriding ONLY pve_vmid_range_start above the
// INHERITED pve_vmid_range_end (never itself touched by this request)
// previously passed silently, because the old ad-hoc check only fired when
// both bounds were overridden in the same request. The inherited end (5999,
// from validBaseCfg) is itself perfectly valid on its own — the violation
// only exists in combination with the override — which is exactly why
// re-validating the full EFFECTIVE config (not just the overridden fields in
// isolation) is required.
func TestApplyContextOverrides_VMIDRange_SingleBoundInversion_Rejected(t *testing.T) {
	t.Parallel()
	base := validBaseCfg() // VMIDRangeEnd = 5999
	_, _, _, err := config.ApplyContextOverrides(base, map[string]any{
		"pve_vmid_range_start": 9000, // > inherited end 5999; pve_vmid_range_end NOT overridden
	})
	if err == nil {
		t.Fatal("expected error: overriding only vmid_range_start above the inherited vmid_range_end must be rejected")
	}
	if !strings.Contains(err.Error(), "vmid_range_end") {
		t.Errorf("error %q should name vmid_range_end (the effective, inherited bound the override now conflicts with)", err.Error())
	}
}

// TestApplyContextOverrides_VMIDRange_OverlapsDiskBand_Rejected overrides
// both VM VMID bounds (order is internally valid, 9000 < 10000) into the
// DEFAULT persistent-disk VMID band (9000-29999), producing colliding VMIDs
// between VMs and persistent-disk containers — a data-integrity violation
// config.Validate's validateVMIDBands rejects for the job-level config, and
// which the override path must reject identically.
func TestApplyContextOverrides_VMIDRange_OverlapsDiskBand_Rejected(t *testing.T) {
	t.Parallel()
	_, _, _, err := config.ApplyContextOverrides(validBaseCfg(), map[string]any{
		"pve_vmid_range_start": 9000,
		"pve_vmid_range_end":   10000,
	})
	if err == nil {
		t.Fatal("expected error: VM VMID range overlapping the persistent-disk band must be rejected")
	}
	if !strings.Contains(err.Error(), "overlap") {
		t.Errorf("error %q should mention the band overlap", err.Error())
	}
}

// TestApplyContextOverrides_VMIDRangeStart_BelowPVEReservedFloor_Rejected
// covers vmid_range_start values in (0, 100) — the old field-local check
// only rejected n<=0, silently accepting anything from 1 to 99 despite PVE
// reserving VMIDs below 100.
func TestApplyContextOverrides_VMIDRangeStart_BelowPVEReservedFloor_Rejected(t *testing.T) {
	t.Parallel()
	_, _, _, err := config.ApplyContextOverrides(validBaseCfg(), map[string]any{
		"pve_vmid_range_start": 50,
		"pve_vmid_range_end":   5999,
	})
	if err == nil {
		t.Fatal("expected error for pve_vmid_range_start=50 (below PVE's reserved floor of 100)")
	}
	if !strings.Contains(err.Error(), "100") {
		t.Errorf("error %q should mention the reserved floor 100", err.Error())
	}
}

// TestApplyContextOverrides_Port_Zero_Rejected covers pve_port=0 — the old
// field-local check accepted [0,65535], silently letting an operator set
// port=0 (config.Validate requires [1,65535] for the job-level config; there
// is no "0 means use the SDK default 8006" semantic for an explicit
// per-request override, unlike ApplyDefaults' job-level JSON-unset case).
func TestApplyContextOverrides_Port_Zero_Rejected(t *testing.T) {
	t.Parallel()
	_, _, _, err := config.ApplyContextOverrides(validBaseCfg(), map[string]any{"pve_port": 0})
	if err == nil {
		t.Fatal("expected error for pve_port=0, got nil")
	}
}

// TestApplyContextOverrides_EmptyString_ClearingRequiredField_Rejected
// covers clearing a startup-required field (host) via an explicit
// empty-string override. Previously this silently produced an effective
// config with host="" that only surfaced as a confusing downstream RETRIABLE
// failure (pve.NewClient's "host is required" CloudError arrives through the
// resolve() path, wrapped as retriable — see H1) instead of an immediate,
// clearly-attributed, non-retriable rejection at the override boundary.
func TestApplyContextOverrides_EmptyString_ClearingRequiredField_Rejected(t *testing.T) {
	t.Parallel()
	_, _, _, err := config.ApplyContextOverrides(validBaseCfg(), map[string]any{"pve_host": ""})
	if err == nil {
		t.Fatal("expected error when clearing the required pve_host field, got nil")
	}
	if !strings.Contains(err.Error(), "host") {
		t.Errorf("error %q should mention host", err.Error())
	}
}

// TestApplyContextOverrides_EmptyString_ClearingBothCredentials_Rejected
// covers clearing pve_password while the effective config has no api_token
// (validBaseCfg sets Password only), leaving the effective config with NO
// auth credential at all. The credential group check rejects it before
// validation runs, rather than producing an auth-less client that fails
// downstream.
func TestApplyContextOverrides_EmptyString_ClearingBothCredentials_Rejected(t *testing.T) {
	t.Parallel()
	base := validBaseCfg()
	base.Password = "job-level-password"
	base.APIToken = ""

	_, _, _, err := config.ApplyContextOverrides(base, map[string]any{"pve_password": ""})
	if err == nil {
		t.Fatal("expected error when pve_password is cleared and no api_token is present")
	}
	if !strings.Contains(err.Error(), "set pve.api_token, or pve.user and pve.password") {
		t.Errorf("error %q should name the complete credential group", err.Error())
	}
	// base itself must never be mutated, including on the rejected path.
	if base.Password != "job-level-password" {
		t.Errorf("base.Password mutated to %q, want unchanged", base.Password)
	}
}

// TestApplyContextOverrides_EmptyString_ClearingPassword_WithAPIToken_Applies
// is the companion positive case: clearing pve_password is VALID when the
// entry also names its own api_token, which is how a cpi-config entry
// switches from password auth to token auth. Clearing the password alone no
// longer rides on the job's token, because naming any credential key drops
// the inherited group; see
// TestApplyContextOverrides_PasswordOnlyEntry_DropsInheritedToken.
func TestApplyContextOverrides_EmptyString_ClearingPassword_WithAPIToken_Applies(t *testing.T) {
	t.Parallel()
	base := validBaseCfg()
	base.Password = "job-level-password"

	eff, applied, _, err := config.ApplyContextOverrides(base, map[string]any{
		"pve_password":  "",
		"pve_api_token": "fake-entry@pve!cpi=fake-entry-token-value",
	})
	if err != nil {
		t.Fatalf("unexpected error clearing password while the entry names a token: %v", err)
	}
	if eff.Password != "" {
		t.Errorf("eff.Password = %q, want empty string", eff.Password)
	}
	if eff.APIToken != "fake-entry@pve!cpi=fake-entry-token-value" {
		t.Errorf("eff.APIToken = %q, want the entry's token", eff.APIToken)
	}
	if want := []string{"pve_api_token", "pve_password"}; !equalStringSlices(applied, want) {
		t.Errorf("applied = %#v, want %#v", applied, want)
	}
}

// -----------------------------------------------------------------------
// Unknown pve_* keys never fail the merge
// -----------------------------------------------------------------------

func TestApplyContextOverrides_UnknownKeysAlongsideKnown_NoError(t *testing.T) {
	t.Parallel()
	eff, applied, unknown, err := config.ApplyContextOverrides(validBaseCfg(), map[string]any{
		"pve_node":              "pve02",
		"pve_placement_enabled": true,
		"pve_hooks":             []any{"audit_log"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if eff.Node != "pve02" {
		t.Errorf("eff.Node = %q, want pve02", eff.Node)
	}
	if len(applied) != 1 || applied[0] != "pve_node" {
		t.Errorf("applied = %#v, want [pve_node]", applied)
	}
	wantUnknown := []string{"pve_hooks", "pve_placement_enabled"}
	if !equalStringSlices(unknown, wantUnknown) {
		t.Errorf("unknown = %#v, want %#v", unknown, wantUnknown)
	}
}

// TestApplyContextOverrides_ParkerPrefix_Applies proves pve_parker_prefix is
// a registered override. The context value lands on the effective config's
// ParkerPrefix, and the job-level base is left untouched.
func TestApplyContextOverrides_ParkerPrefix_Applies(t *testing.T) {
	t.Parallel()
	base := validBaseCfg()
	base.ParkerPrefix = "bosh"

	eff, applied, _, err := config.ApplyContextOverrides(base, map[string]any{
		"pve_parker_prefix": "az1",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(applied) != 1 || applied[0] != "pve_parker_prefix" {
		t.Errorf("applied = %#v, want [pve_parker_prefix]", applied)
	}
	if eff.ParkerPrefix != "az1" {
		t.Errorf("eff.ParkerPrefix = %q, want az1", eff.ParkerPrefix)
	}
	if base.ParkerPrefix != "bosh" {
		t.Errorf("base.ParkerPrefix mutated to %q, want unchanged \"bosh\"", base.ParkerPrefix)
	}
}

// TestApplyContextOverrides_ParkerPool_Applies proves pve_parker_pool is a
// registered override. The context value lands on the effective config's
// ParkerPool, and the job-level base is left untouched.
func TestApplyContextOverrides_ParkerPool_Applies(t *testing.T) {
	t.Parallel()
	base := validBaseCfg()
	base.ParkerPool = "bosh-parker"

	eff, applied, _, err := config.ApplyContextOverrides(base, map[string]any{
		"pve_parker_pool": "az1-parker",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(applied) != 1 || applied[0] != "pve_parker_pool" {
		t.Errorf("applied = %#v, want [pve_parker_pool]", applied)
	}
	if eff.ParkerPool != "az1-parker" {
		t.Errorf("eff.ParkerPool = %q, want az1-parker", eff.ParkerPool)
	}
	if base.ParkerPool != "bosh-parker" {
		t.Errorf("base.ParkerPool mutated to %q, want unchanged \"bosh-parker\"", base.ParkerPool)
	}
}

// TestApplyContextOverrides_ParkerPrefix_MovesParkerPoolValue is the point of
// the accessor design ParkerPoolValue documents. The pool renders per
// request from the effective config rather than being frozen at
// ApplyDefaults time, so a per-entry pve_parker_prefix override moves the
// rendered parker pool name along with it, not just the raw ParkerPrefix
// field.
func TestApplyContextOverrides_ParkerPrefix_MovesParkerPoolValue(t *testing.T) {
	t.Parallel()
	base := validBaseCfg()
	base.ParkerPool = "{prefix}-parker"
	if want := "bosh-parker"; base.ParkerPoolValue() != want {
		t.Fatalf("base.ParkerPoolValue() = %q, want %q", base.ParkerPoolValue(), want)
	}

	eff, _, _, err := config.ApplyContextOverrides(base, map[string]any{
		"pve_parker_prefix": "az1",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if eff.ParkerPoolValue() != "az1-parker" {
		t.Errorf("eff.ParkerPoolValue() = %q, want az1-parker", eff.ParkerPoolValue())
	}
}

// -----------------------------------------------------------------------
// helpers
// -----------------------------------------------------------------------

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	aSorted := append([]string(nil), a...)
	bSorted := append([]string(nil), b...)
	sort.Strings(aSorted)
	sort.Strings(bSorted)
	for i := range aSorted {
		if aSorted[i] != bSorted[i] {
			return false
		}
	}
	return true
}

// TestContextOverrideFieldOrder is the sync assertion the source comments at
// contextOverrideFieldOrder / contextOverrideFields have cited since the
// feature landed: every entry in the order slice must have a matching apply
// function, every apply function must be listed in the order slice, and the
// slice must be duplicate-free. A 19th overridable field added to one side
// but not the other would either be silently skipped by
// ApplyContextOverrides or invisible to the audit ordering — this converts
// that mistake into a failing build.
func TestContextOverrideFieldOrder(t *testing.T) {
	t.Parallel()
	order := config.ContextOverrideFieldOrderForTest()
	keys := config.ContextOverrideFieldKeysForTest()

	seen := make(map[string]bool, len(order))
	for _, f := range order {
		if seen[f] {
			t.Errorf("contextOverrideFieldOrder contains duplicate entry %q", f)
		}
		seen[f] = true
	}
	keySet := make(map[string]bool, len(keys))
	for _, k := range keys {
		keySet[k] = true
	}
	for _, f := range order {
		if !keySet[f] {
			t.Errorf("field %q is in contextOverrideFieldOrder but has no apply function in contextOverrideFields", f)
		}
	}
	for _, k := range keys {
		if !seen[k] {
			t.Errorf("field %q has an apply function but is missing from contextOverrideFieldOrder", k)
		}
	}
}

// TestApplyContextOverrides_NestedCPIConfigShape verifies the shape BOSH's
// cpi-config feature actually delivers (live-verified against a 282.x
// director): the entry's whole properties hash arrives NESTED in the request
// context as context.pve = {...} and context.agent = {mbus: ...} — not as
// flat pve_* keys. Every supported nested property must apply, agent.mbus
// must land on AgentMBus, and unsupported nested properties must surface in
// unknown under their flat name.
func TestApplyContextOverrides_NestedCPIConfigShape(t *testing.T) {
	t.Parallel()
	base := validBaseCfg()

	extra := map[string]any{
		"director_uuid": "d-1",
		"request_id":    "cpi-1",
		"agent":         map[string]any{"mbus": "nats://10.254.48.10:4222"},
		"pve": map[string]any{
			"host":                               "10.255.0.10",
			"api_token":                          "fake-entry@pve!cpi=fake-entry-token-value",
			"node":                               "az2-node",
			"vm_storage":                         "az2-lvm",
			"disk_storage":                       "az2-lvm",
			"stemcell_storage":                   "nfs-shared",
			"iso_storage":                        "local",
			"vmid_range_start":                   float64(5000),
			"vmid_range_end":                     float64(8999),
			"disk_vmid_range_start":              float64(20000),
			"disk_vmid_range_end":                float64(29999),
			"stemcell_template_vmid_range_start": float64(30500),
			"stemcell_template_vmid_range_end":   float64(30999),
			"stemcell_replicate_local":           true,
			"vm_prefix":                          "az2",
			"log_level":                          "info", // deliberately unsupported per-request
		},
	}

	eff, applied, unknown, err := config.ApplyContextOverrides(base, extra)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if eff.Host != "10.255.0.10" || eff.Node != "az2-node" {
		t.Errorf("host/node = %q/%q, want the nested entry's cluster", eff.Host, eff.Node)
	}
	if eff.StemcellStorage != "nfs-shared" || eff.VMStorage != "az2-lvm" {
		t.Errorf("storages = %q/%q, want nested values", eff.StemcellStorage, eff.VMStorage)
	}
	if eff.VMIDRangeStart != 5000 || eff.VMIDRangeEnd != 8999 {
		t.Errorf("vm band = %d-%d, want 5000-8999", eff.VMIDRangeStart, eff.VMIDRangeEnd)
	}
	if eff.DiskVMIDRangeStart != 20000 || eff.DiskVMIDRangeEnd != 29999 {
		t.Errorf("disk band = %d-%d, want 20000-29999", eff.DiskVMIDRangeStart, eff.DiskVMIDRangeEnd)
	}
	if eff.StemcellTemplateVMIDRangeStart != 30500 || eff.StemcellTemplateVMIDRangeEnd != 30999 {
		t.Errorf("template band = %d-%d, want 30500-30999",
			eff.StemcellTemplateVMIDRangeStart, eff.StemcellTemplateVMIDRangeEnd)
	}
	if !eff.StemcellReplicateLocal {
		t.Error("stemcell_replicate_local must apply from the nested entry")
	}
	if eff.VMPrefix != "az2" {
		t.Errorf("vm_prefix = %q, want az2", eff.VMPrefix)
	}
	if eff.AgentMBus != "nats://10.254.48.10:4222" {
		t.Errorf("agent mbus = %q, want the nested agent.mbus", eff.AgentMBus)
	}
	if len(applied) == 0 {
		t.Fatal("applied is empty — the nested shape did not fold into overrides")
	}
	foundUnknown := false
	for _, k := range unknown {
		if k == "pve_log_level" {
			foundUnknown = true
		}
	}
	if !foundUnknown {
		t.Errorf("unknown = %v, want it to carry pve_log_level (unsupported nested property)", unknown)
	}
	// Input map must not be mutated by the fold.
	if _, mutated := extra["pve_host"]; mutated {
		t.Error("input extra map was mutated with flattened keys")
	}
	// Base must stay untouched.
	if base.Host != "h" || base.StemcellReplicateLocal {
		t.Error("base config was mutated")
	}
}

// TestApplyContextOverrides_NestedFlatPrecedence verifies explicit flat keys
// win over the nested entry hash.
func TestApplyContextOverrides_NestedFlatPrecedence(t *testing.T) {
	t.Parallel()
	base := validBaseCfg()
	extra := map[string]any{
		"pve_host":      "flat-wins",
		"pve_api_token": "fake-entry@pve!cpi=fake-entry-token-value",
		"pve":           map[string]any{"host": "nested-loses"},
	}
	eff, _, _, err := config.ApplyContextOverrides(base, extra)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if eff.Host != "flat-wins" {
		t.Errorf("host = %q, want the explicit flat key to win", eff.Host)
	}
}

// TestApplyContextOverrides_StemcellReplicateStorageSet verifies that a
// cpi-config entry decides per-member stemcell replicas for its own cluster
// in both directions. The false case is the one that matters, because the
// base already has a set bound and so replicates by default; an entry that
// says false must be able to turn that off. It does not ride the
// nested-shape test, because that entry moves the cluster endpoint, and an
// endpoint change with inherited storage bindings is refused unless the
// entry restates pve_storage_sets.
func TestApplyContextOverrides_StemcellReplicateStorageSet(t *testing.T) {
	t.Parallel()
	base := validBaseCfg()
	base.EphemeralStorageSet = "eph"
	base.StorageSets = map[string]config.StorageSet{
		"eph": {Names: []string{"ns1", "ns2"}, Strategy: config.StoragePlacementStrategy{Name: "spread", Version: 1}},
	}
	if !base.StemcellReplicateStorageSetEnabled() {
		t.Fatal("a bound ephemeral set must replicate by default, or this test proves nothing")
	}

	for _, tc := range []struct {
		name string
		want bool
	}{{"on", true}, {"off", false}} {
		eff, applied, _, err := config.ApplyContextOverrides(base, map[string]any{
			"pve_stemcell_replicate_storage_set": tc.want,
		})
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", tc.name, err)
		}
		if eff.StemcellReplicateStorageSet == nil || *eff.StemcellReplicateStorageSet != tc.want {
			t.Errorf("%s: effective pointer = %v, want an explicit %t", tc.name, eff.StemcellReplicateStorageSet, tc.want)
		}
		if got := eff.StemcellReplicateStorageSetEnabled(); got != tc.want {
			t.Errorf("%s: resolved = %t, want %t", tc.name, got, tc.want)
		}
		if base.StemcellReplicateStorageSet != nil {
			t.Errorf("%s: base config was mutated", tc.name)
		}
		found := false
		for _, k := range applied {
			if k == "pve_stemcell_replicate_storage_set" {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: applied = %v, want it to carry pve_stemcell_replicate_storage_set", tc.name, applied)
		}
	}
}

// -----------------------------------------------------------------------
// Credential group: user, password, and api_token travel together, and
// they and the CA travel with the endpoint
// -----------------------------------------------------------------------

// Fixture credentials. Every value is an obvious placeholder, distinct from
// every other one, so a test can prove which group reached the effective
// config and that no error text carries any of them.
const (
	credTestJobUser       = "fake-job-user"
	credTestJobRealm      = "fakejobrealm"
	credTestJobPassword   = "FAKE-JOB-PASSWORD-PLACEHOLDER"
	credTestJobToken      = "fake-job-user@fakejobrealm!cpi=FAKE-JOB-TOKEN-PLACEHOLDER"
	credTestEntryUser     = "fake-entry-user"
	credTestEntryRealm    = "pve"
	credTestEntryPassword = "FAKE-ENTRY-PASSWORD-PLACEHOLDER"
	credTestEntryToken    = "fake-entry-user@pve!cpi=FAKE-ENTRY-TOKEN-PLACEHOLDER"
	credTestJobHost       = "pve-a.example"
	credTestEntryHost     = "pve-b.example"
)

// credTestSecrets lists every secret fixture value, for the no-leak checks.
var credTestSecrets = []string{
	credTestJobPassword, credTestJobToken, "FAKE-JOB-TOKEN-PLACEHOLDER",
	credTestEntryPassword, credTestEntryToken, "FAKE-ENTRY-TOKEN-PLACEHOLDER",
}

// credTestTokenJob returns a job config that authenticates with a token and
// also carries a user and realm, the shape that leaked before the group rule.
func credTestTokenJob() *config.CPIConfig {
	base := validBaseCfg()
	base.Host = credTestJobHost
	base.User = credTestJobUser
	base.Realm = credTestJobRealm
	base.Password = ""
	base.APIToken = credTestJobToken
	return base
}

// credTestPasswordJob returns a job config that authenticates with a user and
// password under a non-default realm.
func credTestPasswordJob() *config.CPIConfig {
	base := validBaseCfg()
	base.Host = credTestJobHost
	base.User = credTestJobUser
	base.Realm = credTestJobRealm
	base.Password = credTestJobPassword
	base.APIToken = ""
	return base
}

// assertNoSecretInError fails when err's text carries any fixture secret.
func assertNoSecretInError(t *testing.T, err error, extraSecrets ...string) {
	t.Helper()
	if err == nil {
		return
	}
	for _, secret := range append(append([]string(nil), credTestSecrets...), extraSecrets...) {
		if secret != "" && strings.Contains(err.Error(), secret) {
			t.Errorf("error text carries a secret fixture value; it must name property keys only: %q", err.Error())
		}
	}
}

func TestApplyContextOverrides_HostChange_DropsInheritedCredentials(t *testing.T) {
	t.Parallel()
	for name, base := range map[string]*config.CPIConfig{
		"token job":    credTestTokenJob(),
		"password job": credTestPasswordJob(),
	} {
		t.Run(name+", entry token", func(t *testing.T) {
			t.Parallel()
			eff, _, _, err := config.ApplyContextOverrides(base, map[string]any{
				"pve_host":      credTestEntryHost,
				"pve_api_token": credTestEntryToken,
			})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if eff.APIToken != credTestEntryToken {
				t.Error("eff.APIToken is not the entry's token")
			}
			if eff.Password != "" || eff.User != "" {
				t.Errorf("inherited user or password survived a host change (user set: %t, password set: %t)",
					eff.User != "", eff.Password != "")
			}
		})
		t.Run(name+", entry user and password", func(t *testing.T) {
			t.Parallel()
			eff, _, _, err := config.ApplyContextOverrides(base, map[string]any{
				"pve_host":     credTestEntryHost,
				"pve_user":     credTestEntryUser,
				"pve_password": credTestEntryPassword,
			})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if eff.APIToken != "" {
				t.Error("the job's api_token survived a host change; the PVE client would prefer it and send it to the other cluster")
			}
			if eff.User != credTestEntryUser || eff.Password != credTestEntryPassword {
				t.Error("the entry's user and password did not reach the effective config")
			}
		})
	}
}

func TestApplyContextOverrides_HostChange_NoCredentials_Fails(t *testing.T) {
	t.Parallel()
	for name, base := range map[string]*config.CPIConfig{
		"token job":    credTestTokenJob(),
		"password job": credTestPasswordJob(),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			eff, _, _, err := config.ApplyContextOverrides(base, map[string]any{"pve_host": credTestEntryHost})
			if err == nil {
				t.Fatal("expected a host change with no credentials to fail")
			}
			if eff != nil {
				t.Error("effective config must be nil on error")
			}
			want := `context entry for host "pve-b.example" changes pve.host but supplies no credentials; ` +
				"set pve.api_token, or pve.user and pve.password, and pve.ca_cert if that cluster uses a private CA"
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), want)
			}
			assertNoSecretInError(t, err)
		})
	}
}

func TestApplyContextOverrides_PortChange_IsEndpointChange(t *testing.T) {
	t.Parallel()
	base := credTestTokenJob()

	_, _, _, err := config.ApplyContextOverrides(base, map[string]any{"pve_port": float64(8443)})
	if err == nil {
		t.Fatal("expected a port change with no credentials to fail, because it reaches a different endpoint")
	}
	if !strings.Contains(err.Error(), `context entry for host "pve-a.example" changes pve.port but supplies no credentials`) {
		t.Errorf("error = %q, want it to name the entry's host and pve.port", err.Error())
	}
	assertNoSecretInError(t, err)

	eff, _, _, err := config.ApplyContextOverrides(base, map[string]any{
		"pve_port":      float64(8443),
		"pve_api_token": credTestEntryToken,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if eff.APIToken != credTestEntryToken || eff.User != "" {
		t.Error("a port change must use only the entry's credentials")
	}
}

func TestApplyContextOverrides_HostCaseAndTrailingDot_SameEndpoint(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		extra map[string]any
	}{
		{"upper case host", map[string]any{"pve_host": "PVE-A.Example"}},
		{"trailing dot", map[string]any{"pve_host": credTestJobHost + "."}},
		{"case, dot, and space", map[string]any{"pve_host": " Pve-A.EXAMPLE. "}},
		{"explicit default port", map[string]any{"pve_port": float64(8006)}},
		{"host spelling and default port", map[string]any{"pve_host": "PVE-A.EXAMPLE.", "pve_port": "8006"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			base := credTestTokenJob()
			eff, applied, _, err := config.ApplyContextOverrides(base, tc.extra)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if eff.APIToken != credTestJobToken || eff.User != credTestJobUser {
				t.Error("the same endpoint, spelled differently, must keep the job's credential group")
			}
			if got := config.OverrideCredentialGroup(base, eff, applied); got != config.CredentialGroupInherited {
				t.Errorf("OverrideCredentialGroup = %q, want %q", got, config.CredentialGroupInherited)
			}
		})
	}

	t.Run("unset job port equals 8006", func(t *testing.T) {
		t.Parallel()
		a := credTestTokenJob()
		a.Port = 0
		b := credTestTokenJob()
		b.Port = 8006
		if !config.SameEndpoint(a, b) {
			t.Error("an unset port and 8006 must be the same endpoint")
		}
		b.Port = 8007
		if config.SameEndpoint(a, b) {
			t.Error("8006 and 8007 must be different endpoints")
		}
	})
}

func TestApplyContextOverrides_PasswordOnlyEntry_DropsInheritedToken(t *testing.T) {
	t.Parallel()
	base := credTestTokenJob()

	// Same endpoint, password only: the job's token and user are both part of
	// the inherited group, so the entry is left with a password and no user.
	eff, _, _, err := config.ApplyContextOverrides(base, map[string]any{"pve_password": credTestEntryPassword})
	if err == nil {
		t.Fatalf("expected a password-only entry to fail rather than ride on the job token (token kept: %t)",
			eff != nil && eff.APIToken == credTestJobToken)
	}
	if !strings.Contains(err.Error(),
		`context entry for host "pve-a.example" sets pve.password but its credential group is incomplete`) {
		t.Errorf("error = %q, want it to name the host, pve.password, and the incomplete group", err.Error())
	}
	if strings.Contains(err.Error(), "pve.ca_cert") {
		t.Errorf("error = %q; an entry on the job endpoint keeps the job CA, so the CA hint does not apply", err.Error())
	}
	assertNoSecretInError(t, err)

	// Same endpoint, user and password: the job's token is dropped, so the
	// entry's password is what authenticates.
	eff, applied, _, err := config.ApplyContextOverrides(base, map[string]any{
		"pve_user":     credTestEntryUser,
		"pve_password": credTestEntryPassword,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if eff.APIToken != "" {
		t.Error("the job's api_token survived an entry that names its own password; the client would prefer the token")
	}
	if eff.Password != credTestEntryPassword || eff.User != credTestEntryUser {
		t.Error("the entry's user and password did not reach the effective config")
	}
	if got := config.OverrideCredentialGroup(base, eff, applied); got != config.CredentialGroupEntry {
		t.Errorf("OverrideCredentialGroup = %q, want %q", got, config.CredentialGroupEntry)
	}
}

func TestApplyContextOverrides_UserOnlyEntry_Fails(t *testing.T) {
	t.Parallel()
	for name, extra := range map[string]map[string]any{
		"same endpoint": {"pve_user": credTestEntryUser},
		"new host":      {"pve_host": credTestEntryHost, "pve_user": credTestEntryUser},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, _, _, err := config.ApplyContextOverrides(credTestPasswordJob(), extra)
			if err == nil {
				t.Fatal("expected a user-only entry to fail rather than borrow the job's password")
			}
			if !strings.Contains(err.Error(), "sets pve.user but its credential group is incomplete") {
				t.Errorf("error = %q, want it to name pve.user and the incomplete group", err.Error())
			}
			assertNoSecretInError(t, err)
		})
	}
}

func TestApplyContextOverrides_TokenOnlyEntry_Passes(t *testing.T) {
	t.Parallel()
	for name, base := range map[string]*config.CPIConfig{
		"token job":    credTestTokenJob(),
		"password job": credTestPasswordJob(),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			eff, _, _, err := config.ApplyContextOverrides(base, map[string]any{
				"pve_host":      credTestEntryHost,
				"pve_api_token": credTestEntryToken,
			})
			if err != nil {
				t.Fatalf("a token-only entry must pass, because the token already names its user: %v", err)
			}
			if eff.User != "" || eff.Password != "" || eff.APIToken != credTestEntryToken {
				t.Error("a token-only entry must authenticate with its token alone")
			}
			if err := eff.Validate(); err != nil {
				t.Errorf("the token-only effective config must validate: %v", err)
			}
		})
	}

	// An entry follows the job's token rule, so the Authorization-header form
	// a PVE example shows works on an entry as it does at the job level.
	t.Run("header-form token loses its prefix", func(t *testing.T) {
		t.Parallel()
		for _, extra := range []map[string]any{
			{"pve_host": credTestEntryHost, "pve_api_token": "PVEAPIToken=" + credTestEntryToken},
			{"pve": map[string]any{"host": credTestEntryHost, "api_token": "PVEAPIToken=" + credTestEntryToken}},
		} {
			eff, _ := credTestMustApply(t, credTestTokenJob(), extra)
			if eff.APIToken != credTestEntryToken {
				t.Error("the entry's api_token kept its PVEAPIToken= prefix, which the SDK would read as the token ID")
			}
		}
		job := credTestTokenJob()
		job.APIToken = "PVEAPIToken=" + credTestJobToken
		job.ApplyDefaults()
		if job.APIToken != credTestJobToken {
			t.Error("the job api_token kept its PVEAPIToken= prefix")
		}
	})
	t.Run("a bare prefix is an empty token", func(t *testing.T) {
		t.Parallel()
		_, _, _, err := config.ApplyContextOverrides(credTestTokenJob(), map[string]any{
			"pve_host": credTestEntryHost, "pve_api_token": "PVEAPIToken=",
		})
		if err == nil || !strings.Contains(err.Error(), "sets pve.api_token but its credential group is incomplete") {
			t.Errorf("error = %v, want the incomplete-group error for an empty token", err)
		}
	})
}

func TestApplyContextOverrides_TokenAndPassword_KeepsToken(t *testing.T) {
	t.Parallel()
	eff, _, _, err := config.ApplyContextOverrides(credTestPasswordJob(), map[string]any{
		"pve_host":      credTestEntryHost,
		"pve_user":      credTestEntryUser,
		"pve_password":  credTestEntryPassword,
		"pve_api_token": credTestEntryToken,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if eff.APIToken != credTestEntryToken {
		t.Error("the entry's token must win over its password, as it does for the job config")
	}
	if eff.Password != "" {
		t.Error("the entry's password must be cleared when its token is set, as ApplyDefaults does for the job config")
	}
}

func TestApplyContextOverrides_RealmInherits(t *testing.T) {
	t.Parallel()
	base := credTestPasswordJob()

	// A user and password on the job host keep the job's realm, which is
	// the shape the realm rule protects.
	eff, _, _, err := config.ApplyContextOverrides(base, map[string]any{
		"pve_user":     credTestEntryUser,
		"pve_password": credTestEntryPassword,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if eff.Realm != credTestJobRealm {
		t.Errorf("eff.Realm = %q, want the inherited %q", eff.Realm, credTestJobRealm)
	}

	// The realm is not a secret, so it inherits across a host change too.
	eff, _, _, err = config.ApplyContextOverrides(base, map[string]any{
		"pve_host":     credTestEntryHost,
		"pve_user":     credTestEntryUser,
		"pve_password": credTestEntryPassword,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if eff.Realm != credTestJobRealm {
		t.Errorf("eff.Realm = %q, want the inherited %q", eff.Realm, credTestJobRealm)
	}

	// An entry that names a realm gets it.
	eff, _, _, err = config.ApplyContextOverrides(base, map[string]any{
		"pve_host":     credTestEntryHost,
		"pve_user":     credTestEntryUser,
		"pve_password": credTestEntryPassword,
		"pve_realm":    credTestEntryRealm,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if eff.Realm != credTestEntryRealm {
		t.Errorf("eff.Realm = %q, want the entry's %q", eff.Realm, credTestEntryRealm)
	}

	// A realm alone on the job host is not a credential key, so the job's
	// whole group stays.
	eff, applied, _, err := config.ApplyContextOverrides(base, map[string]any{"pve_realm": credTestEntryRealm})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if eff.Password != credTestJobPassword || eff.User != credTestJobUser {
		t.Error("a realm-only entry on the job host must keep the job's credential group")
	}
	if got := config.OverrideCredentialGroup(base, eff, applied); got != config.CredentialGroupInherited {
		t.Errorf("OverrideCredentialGroup = %q, want %q", got, config.CredentialGroupInherited)
	}
}

// credTestMustApply applies extra to base and fails the test on any error.
func credTestMustApply(t *testing.T, base *config.CPIConfig, extra map[string]any) (*config.CPIConfig, []string) {
	t.Helper()
	eff, applied, _, err := config.ApplyContextOverrides(base, extra)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	return eff, applied
}

func TestApplyContextOverrides_CACert(t *testing.T) {
	t.Parallel()
	jobCA := selfSignedCAPEMForConfigTest(t)
	entryCA := selfSignedCAPEMForConfigTest(t)
	newBase := func() *config.CPIConfig {
		base := credTestTokenJob()
		base.PVECACertPEM = jobCA
		return base
	}

	t.Run("host change without ca_cert uses the system pool", func(t *testing.T) {
		t.Parallel()
		eff, _ := credTestMustApply(t, newBase(), map[string]any{
			"pve_host": credTestEntryHost, "pve_api_token": credTestEntryToken,
		})
		if eff.PVECACertPEM != "" {
			t.Error("the job's private CA must not follow a request to another host")
		}
		if !eff.VerifySSLValue() {
			t.Error("verify_ssl must stay on")
		}
	})
	t.Run("port change without ca_cert uses the system pool", func(t *testing.T) {
		t.Parallel()
		eff, _ := credTestMustApply(t, newBase(), map[string]any{
			"pve_port": float64(8443), "pve_api_token": credTestEntryToken,
		})
		if eff.PVECACertPEM != "" {
			t.Error("the job's private CA must not follow a request to another port")
		}
	})
	t.Run("host change with ca_cert uses the entry CA", func(t *testing.T) {
		t.Parallel()
		eff, applied := credTestMustApply(t, newBase(), map[string]any{
			"pve_host": credTestEntryHost, "pve_api_token": credTestEntryToken, "pve_ca_cert": entryCA,
		})
		if eff.PVECACertPEM != entryCA {
			t.Error("eff.PVECACertPEM is not the entry's CA")
		}
		if !slices.Contains(applied, "pve_ca_cert") {
			t.Errorf("applied = %v, want it to carry pve_ca_cert", applied)
		}
	})
	t.Run("nested ca_cert applies", func(t *testing.T) {
		t.Parallel()
		eff, _ := credTestMustApply(t, newBase(), map[string]any{"pve": map[string]any{
			"host": credTestEntryHost, "api_token": credTestEntryToken, "ca_cert": entryCA,
		}})
		if eff.PVECACertPEM != entryCA {
			t.Error("the nested pve.ca_cert did not reach the effective config")
		}
	})
	t.Run("same endpoint inherits the job CA", func(t *testing.T) {
		t.Parallel()
		eff, _ := credTestMustApply(t, newBase(), map[string]any{"pve_node": "pve02"})
		if eff.PVECACertPEM != jobCA {
			t.Error("a request on the job endpoint must keep the job's CA")
		}
	})
	t.Run("same endpoint ca_cert replaces the job CA", func(t *testing.T) {
		t.Parallel()
		eff, _ := credTestMustApply(t, newBase(), map[string]any{"pve_ca_cert": entryCA})
		if eff.PVECACertPEM != entryCA || eff.APIToken != credTestJobToken {
			t.Error("a CA-only entry on the job endpoint must replace the CA and keep the job's credential group")
		}
	})
	t.Run("explicit empty ca_cert selects the system pool", func(t *testing.T) {
		t.Parallel()
		eff, _ := credTestMustApply(t, newBase(), map[string]any{"pve_ca_cert": ""})
		if eff.PVECACertPEM != "" {
			t.Error("an explicit empty pve_ca_cert must clear the inherited CA")
		}
	})
	t.Run("malformed ca_cert is rejected by key name", func(t *testing.T) {
		t.Parallel()
		const malformed = "-----BEGIN CERTIFICATE-----\nNOT-A-CERT-PLACEHOLDER\n-----END CERTIFICATE-----\n"
		_, _, _, err := config.ApplyContextOverrides(newBase(), map[string]any{
			"pve_host": credTestEntryHost, "pve_api_token": credTestEntryToken, "pve_ca_cert": malformed,
		})
		if err == nil {
			t.Fatal("expected a malformed pve_ca_cert to fail validation")
		}
		if !strings.Contains(err.Error(), "pve_ca_cert") || strings.Contains(err.Error(), "NOT-A-CERT-PLACEHOLDER") {
			t.Errorf("error = %q, want it to name pve_ca_cert and not echo the value", err.Error())
		}
		assertNoSecretInError(t, err)
	})
	t.Run("non-string ca_cert is rejected", func(t *testing.T) {
		t.Parallel()
		_, _, _, err := config.ApplyContextOverrides(newBase(), map[string]any{"pve_ca_cert": 42})
		if err == nil || !strings.Contains(err.Error(), "pve_ca_cert") {
			t.Errorf("error = %v, want a coercion error naming pve_ca_cert", err)
		}
	})
}

// TestApplyContextOverrides_NoCombinationSendsInheritedCredentialElsewhere
// walks every combination of the endpoint, credential, realm, and CA keys,
// each absent, set to an entry value, or set to an empty string, against a
// token job and a password job. It proves three things for every
// combination: an effective config whose endpoint differs from the job's
// holds none of the job's credentials and not the job's CA, an entry that
// names any credential key holds none of the job's credentials either, and
// every error names property keys only.
func TestApplyContextOverrides_NoCombinationSendsInheritedCredentialElsewhere(t *testing.T) {
	t.Parallel()
	jobCA := selfSignedCAPEMForConfigTest(t)
	entryCA := selfSignedCAPEMForConfigTest(t)

	type choice struct {
		key    string
		values []any // nil entry means the key is absent
	}
	choices := []choice{
		{"pve_host", []any{nil, credTestJobHost, "PVE-A.EXAMPLE.", credTestEntryHost}},
		{"pve_port", []any{nil, float64(8006), float64(8443)}},
		{"pve_user", []any{nil, credTestEntryUser, ""}},
		{"pve_realm", []any{nil, credTestEntryRealm}},
		{"pve_password", []any{nil, credTestEntryPassword, ""}},
		{"pve_api_token", []any{nil, credTestEntryToken, ""}},
		{"pve_ca_cert", []any{nil, entryCA, ""}},
	}
	bases := map[string]func() *config.CPIConfig{
		"token job":    credTestTokenJob,
		"password job": credTestPasswordJob,
	}

	combos, accepted := 0, 0
	for baseName, newBase := range bases {
		idx := make([]int, len(choices))
		for {
			extra := map[string]any{}
			for i, c := range choices {
				if v := c.values[idx[i]]; v != nil {
					extra[c.key] = v
				}
			}
			base := newBase()
			base.PVECACertPEM = jobCA
			combos++
			if checkCredentialCombo(t, baseName, base, extra, jobCA, entryCA) {
				accepted++
			}
			if !advanceOdometer(idx, func(pos int) int { return len(choices[pos].values) }) {
				break
			}
		}
	}
	if accepted == 0 || accepted == combos {
		t.Fatalf("the walk accepted %d of %d combinations; it must exercise both outcomes", accepted, combos)
	}
}

// checkCredentialCombo applies one combination of override keys and asserts
// that no job credential or job CA reaches another endpoint, that an entry
// naming a credential key keeps no job credential, that OverrideCredentialGroup
// agrees with the resulting config, and that no error carries a secret. It
// reports whether the combination was accepted.
func checkCredentialCombo(t *testing.T, baseName string, base *config.CPIConfig, extra map[string]any,
	jobCA, entryCA string,
) bool {
	t.Helper()
	eff, applied, _, err := config.ApplyContextOverrides(base, extra)
	assertNoSecretInError(t, err, jobCA, entryCA)
	if err != nil || eff == base {
		return false
	}
	label := fmt.Sprintf("%s, extra keys %v", baseName, sortedKeys(extra))
	namedCredential := slices.ContainsFunc([]string{"pve_user", "pve_password", "pve_api_token"}, func(key string) bool {
		_, ok := extra[key]
		return ok
	})
	leaked := eff.APIToken == credTestJobToken || eff.Password == credTestJobPassword || eff.User == credTestJobUser
	sameEndpoint := config.SameEndpoint(base, eff)
	if !sameEndpoint && leaked {
		t.Errorf("%s: a job credential reached another endpoint", label)
	}
	if !sameEndpoint && eff.PVECACertPEM == jobCA {
		t.Errorf("%s: the job CA followed the request to another endpoint", label)
	}
	if namedCredential && leaked {
		t.Errorf("%s: an entry that named a credential key kept a job credential", label)
	}
	wantEntry := !sameEndpoint || namedCredential
	if gotEntry := config.OverrideCredentialGroup(base, eff, applied) == config.CredentialGroupEntry; gotEntry != wantEntry {
		t.Errorf("%s: OverrideCredentialGroup disagrees with the group the config holds", label)
	}
	return true
}

// advanceOdometer steps idx to the next combination, where position pos
// counts up to size(pos). It returns false once every combination is done.
func advanceOdometer(idx []int, size func(pos int) int) bool {
	for pos := range idx {
		idx[pos]++
		if idx[pos] < size(pos) {
			return true
		}
		idx[pos] = 0
	}
	return false
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func TestValidate_APITokenWithoutUser_Passes(t *testing.T) {
	t.Parallel()
	cfg := validBaseCfg()
	cfg.User = ""
	cfg.Password = ""
	cfg.APIToken = credTestEntryToken
	if err := cfg.Validate(); err != nil {
		t.Fatalf("a token-only job config must validate, because the token names its user: %v", err)
	}

	cfg.APIToken = ""
	cfg.Password = credTestJobPassword
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "user is required") {
		t.Fatalf("password auth without a user must still fail with %q, got %v", "user is required", err)
	}
	assertNoSecretInError(t, err)
}

// TestOverrideCredentialGroup_NilClaimsEntry asserts that a nil config gets
// the safer label, so a warning built from it never claims the job
// credentials stayed on the job host.
func TestOverrideCredentialGroup_NilClaimsEntry(t *testing.T) {
	t.Parallel()
	cfg := credTestTokenJob()
	for name, pair := range map[string][2]*config.CPIConfig{
		"nil base":      {nil, cfg},
		"nil effective": {cfg, nil},
	} {
		if got := config.OverrideCredentialGroup(pair[0], pair[1], nil); got != config.CredentialGroupEntry {
			t.Errorf("%s: OverrideCredentialGroup = %q, want %q", name, got, config.CredentialGroupEntry)
		}
	}
}
